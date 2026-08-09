//go:build linux || darwin

package fuse

// #141's acceptance criteria ask for no benchmark regression on a mount with no coordinator. There was
// nothing anywhere in the repository to compare against — its benchmarks cover the cache
// (internal/cache, test/benchmarks), the S3 backend and URI validation, none of which run the FUSE read
// path — so the comparison is established here rather than asserted against a number nobody measured.
//
// # What each pair measures, and what it cannot
//
// A cache hit never reaches the announce call: [FileSystem.announceCached] is called from
// [FileSystem.fetchUncached], so the coordinator is not consulted on a hit at all. The cached pair
// therefore measures the one thing the criterion is actually about — whether carrying a coordinator costs
// a read anything on the path that serves most reads — and its answer should be "nothing measurable".
// The uncached pair is where the announcement happens, and it is the pair that could show a cost.
//
// Neither pair can measure the cost of a *real* coordinator: [distributed.Coordinator.AnnounceKey]
// marshals and broadcasts over UDP to every peer, and nothing about that is in this package. What is
// measured is the call site's own cost — the nil check, the metadata-cache read the announcement's ETag
// comes from, and the struct it builds — against a coordinator that records and returns. That is the
// right scope: the criterion is about the mount that has no coordinator, and the non-nil arm exists to
// bound what the other kind pays before it reaches the network.
//
// # Why the backend here is in-memory, in a package whose tests refuse mocks
//
// read_path_test.go and coordinate_test.go both drive a real S3 endpoint through internal/testaws, and
// that is the right default: a mock on the far side of a seam agrees with its caller by construction.
// Two things make a benchmark different.
//
// The first is mechanical. [testaws.Start] takes a *testing.T concretely, and so does
// emulator.StartTestServer under it — and testing.TB has an unexported method, so no cast bridges a
// *testing.B to it. A benchmark cannot reach that harness at all. That is a gap in the harness rather
// than a fact about benchmarks, and it is filed upstream as substrate#605; when a testing.TB entry point
// lands, the uncached pair below should move onto it, because HTTP is the honest cost of a miss.
//
// The second is that it does not weaken the measurement, and in one direction it strengthens it. The
// cached pair asserts the backend is not called at all inside the timed loop, so which backend it holds
// cannot affect the figure — that is checked below rather than argued. The uncached pair does call the
// backend, and an in-memory GET is orders of magnitude cheaper than a real one, which makes any
// coordinator overhead a *larger* fraction of the total than it would be against S3. A result of "no
// meaningful difference" measured this way therefore holds a fortiori against a real endpoint.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/vfs"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// benchBackend is an in-memory [types.Backend] that counts the calls the read path makes.
//
// The counters are the point as much as the storage is: they are what lets the cached benchmark assert
// that nothing in its timed loop touches the backend, which is what makes an in-memory one admissible
// there. See the file comment.
type benchBackend struct {
	mu      sync.RWMutex
	objects map[string][]byte
	meta    map[string]map[string]string

	gets  atomic.Int64
	heads atomic.Int64
}

func newBenchBackend() *benchBackend {
	return &benchBackend{
		objects: make(map[string][]byte),
		meta:    make(map[string]map[string]string),
	}
}

// calls returns the number of GET and HEAD requests made so far.
func (b *benchBackend) calls() int64 { return b.gets.Load() + b.heads.Load() }

func (b *benchBackend) GetObject(_ context.Context, key string, offset, size int64) ([]byte, error) {
	b.gets.Add(1)

	b.mu.RLock()
	defer b.mu.RUnlock()

	data, ok := b.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}

	if offset < 0 || offset > int64(len(data)) {
		return nil, fmt.Errorf("benchBackend: offset %d outside %q", offset, key)
	}

	// A size of zero or less means "to the end of the object", per [types.Backend].
	end := int64(len(data))
	if size > 0 && offset+size < end {
		end = offset + size
	}

	return data[offset:end], nil
}

func (b *benchBackend) PutObject(_ context.Context, key string, data []byte, meta map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.objects[key] = append([]byte(nil), data...)
	b.meta[key] = maps.Clone(meta)

	return nil
}

// PutObjectIf refuses rather than answering, the way tests.MockBackend does: an in-memory store cannot
// evaluate a precondition the way S3 does, and [types.Backend] is explicit that falling back to an
// unconditional write is the failure the mechanism exists to prevent.
func (b *benchBackend) PutObjectIf(context.Context, string, []byte, map[string]string,
	types.Precondition,
) (string, error) {
	return "", types.ErrNotSupported
}

func (b *benchBackend) SetObjectMetadata(_ context.Context, key string, meta map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.objects[key]; !ok {
		return os.ErrNotExist
	}

	b.meta[key] = maps.Clone(meta)

	return nil
}

func (b *benchBackend) CopyObject(_ context.Context, src, dst string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	data, ok := b.objects[src]
	if !ok {
		return os.ErrNotExist
	}

	b.objects[dst] = append([]byte(nil), data...)
	b.meta[dst] = maps.Clone(b.meta[src])

	return nil
}

func (b *benchBackend) DeleteObject(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.objects, key)
	delete(b.meta, key)

	return nil
}

func (b *benchBackend) HeadObject(_ context.Context, key string) (*types.ObjectInfo, error) {
	b.heads.Add(1)

	b.mu.RLock()
	defer b.mu.RUnlock()

	data, ok := b.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}

	// A real ETag, derived from the bytes. It has to be non-empty and content-derived, because
	// [FileSystem.announceCached] refuses to announce without one — a stub returning "" here would make
	// the uncached-with-coordinator benchmark measure the early return instead of the announce path, and
	// the assertion at the end of it is what catches that.
	sum := sha256.Sum256(data)

	return &types.ObjectInfo{
		Key:          key,
		Size:         int64(len(data)),
		LastModified: time.Unix(0, 0),
		ETag:         hex.EncodeToString(sum[:]),
		Metadata:     maps.Clone(b.meta[key]),
	}, nil
}

func (b *benchBackend) GetObjects(ctx context.Context, keys []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(keys))

	for _, key := range keys {
		data, err := b.GetObject(ctx, key, 0, 0)
		if err != nil {
			return nil, err
		}

		out[key] = data
	}

	return out, nil
}

func (b *benchBackend) PutObjects(ctx context.Context, objects map[string][]byte) error {
	for key, data := range objects {
		if err := b.PutObject(ctx, key, data, nil); err != nil {
			return err
		}
	}

	return nil
}

func (b *benchBackend) ListObjects(_ context.Context, prefix string, limit int) ([]types.ObjectInfo, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]types.ObjectInfo, 0, len(b.objects))

	for key, data := range b.objects {
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}

		out = append(out, types.ObjectInfo{Key: key, Size: int64(len(data))})

		if limit > 0 && len(out) >= limit {
			break
		}
	}

	return out, nil
}

func (b *benchBackend) HealthCheck(context.Context) error { return nil }

var _ types.Backend = (*benchBackend)(nil)

// countingCoordinator is a [types.DistributedCoordinator] that counts instead of recording.
//
// Not [recordingCoordinator], deliberately: that one appends every announcement to a slice, and b.N
// announcements would make the benchmark measure a growing slice's reallocation alongside the call it
// means to measure. Counting keeps the coordinator's own cost to a mutex-free atomic increment, which is
// the floor — anything a real coordinator does is on top of what is measured here, which is what the
// file comment says the non-nil arm bounds.
type countingCoordinator struct {
	announces   atomic.Int64
	invalidates atomic.Int64
}

func (c *countingCoordinator) ExecuteOperation(context.Context, any) (any, error) {
	return nil, types.ErrNotSupported
}

func (c *countingCoordinator) GetStats() map[string]any { return map[string]any{} }

func (c *countingCoordinator) AnnounceKey(context.Context, types.KeyAnnouncement) error {
	c.announces.Add(1)

	return nil
}

func (c *countingCoordinator) QueryKeyOwnership(context.Context, string) ([]types.KeyAnnouncement, error) {
	return nil, nil
}

func (c *countingCoordinator) InvalidateKey(context.Context, string, string) error {
	c.invalidates.Add(1)

	return nil
}

var _ types.DistributedCoordinator = (*countingCoordinator)(nil)

// benchReadFixture is the shared setup: one object seeded, its version in the metadata cache, and one
// read already performed so the range is cached.
//
// Read-ahead is configured off rather than left at its default. A prefetch is an asynchronous GET on a
// goroutine of its own, and one landing mid-loop would put a backend call inside a timed region that
// asserts there are none. Turning it off is also what makes the two arms of each pair comparable: with
// it on, the difference between them could be a difference in how many prefetches happened to land.
func benchReadFixture(b *testing.B, key string, size int, coord types.DistributedCoordinator) (*FileSystem, *benchBackend, *FileHandle) {
	b.Helper()

	backend := newBenchBackend()

	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	if err := backend.PutObject(b.Context(), key, payload, nil); err != nil {
		b.Fatalf("seeding %q: %v", key, err)
	}

	writer, err := vfs.NewWriter(b.Context(), backend)
	if err != nil {
		b.Fatalf("vfs.NewWriter: %v", err)
	}

	byteCache := cache.NewLRUCache(&cache.CacheConfig{
		MaxSize:    64 << 20,
		MaxEntries: 10000,
		TTL:        time.Hour,
	})
	b.Cleanup(func() { _ = byteCache.Close() })

	fs := NewFileSystem(b.Context(), backend, byteCache, writer, nil, &Config{
		DefaultMode:    0o644,
		DefaultDirMode: 0o755,
		DefaultUID:     1000,
		DefaultGID:     1000,
		ReadAhead:      &ReadAheadConfig{Enabled: false},
	})
	fs.coordinator = coord

	// The version the announcement will carry, put in the metadata cache the way a stat puts it there.
	// Through statObject — the production path Getattr and Lookup both reach — rather than by writing to
	// the cache directly, so the uncached arm below is measuring the same lookup a mounted filesystem
	// performs and not a shortcut.
	if _, err := fs.statObject(b.Context(), key); err != nil {
		b.Fatalf("statObject(%q): %v", key, err)
	}

	fh := &FileHandle{
		fs:     fs,
		handle: 1,
		file: &OpenFile{
			path:        key,
			lastAccess:  time.Now(),
			accessCount: 1,
		},
	}

	// One read, so the range is cached and the write path's node exists. Both matter: without the node,
	// the first timed iteration would pay a HEAD that none of the others do.
	dest := make([]byte, benchReadSize)
	if _, errno := fh.Read(b.Context(), dest, 0); errno != 0 {
		b.Fatalf("warming read: errno %v", errno)
	}

	return fs, backend, fh
}

const (
	// benchObjectSize is comfortably larger than a read, so no read is clamped at EOF.
	benchObjectSize = 1 << 20

	// benchReadSize is the kernel's default MaxRead, which is the size a mounted filesystem is actually
	// asked for.
	benchReadSize = 128 * 1024
)

// BenchmarkCachedReadWithoutCoordinator is the single-node mount, which is very nearly every mount.
//
// This is the number the criterion is about, and the one to compare against when the read path changes
// again.
func BenchmarkCachedReadWithoutCoordinator(b *testing.B) {
	_, backend, fh := benchReadFixture(b, "bench-cached-solo.dat", benchObjectSize, nil)
	ctx := b.Context()
	dest := make([]byte, benchReadSize)

	before := backend.calls()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, errno := fh.Read(ctx, dest, 0); errno != 0 {
			b.Fatalf("Read: errno %v", errno)
		}
	}

	b.StopTimer()

	// The claim the file comment rests on, checked rather than asserted in prose: an in-memory backend is
	// admissible here because the timed loop never reaches it. If a change to the read path puts a request
	// back in this loop — a metadata refresh, a size check that misses — this figure stops being a
	// cache-hit measurement and the failure says so.
	if got := backend.calls() - before; got != 0 {
		b.Fatalf("the cached read benchmark made %d backend requests inside its timed loop, so it is "+
			"measuring an uncached read and the in-memory backend under it is now load-bearing", got)
	}
}

// BenchmarkCachedReadWithCoordinator is the clustered mount, for the difference rather than the absolute.
//
// A cache hit returns before reaching the announce call, so the two should be indistinguishable. If they
// are not, the coordinator is being consulted on a path that has no need of it.
func BenchmarkCachedReadWithCoordinator(b *testing.B) {
	coord := &countingCoordinator{}
	_, _, fh := benchReadFixture(b, "bench-cached-cluster.dat", benchObjectSize, coord)
	ctx := b.Context()
	dest := make([]byte, benchReadSize)

	// From a baseline, because the fixture's warming read is a miss and announces — one announcement, and
	// asserting zero in total reported it as the defect below rather than as setup.
	before := coord.announces.Load()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, errno := fh.Read(ctx, dest, 0); errno != 0 {
			b.Fatalf("Read: errno %v", errno)
		}
	}

	b.StopTimer()

	// A hit must not announce. This is the same claim TestReadAnnouncesWhatItCached's fixture makes from
	// the other direction, and it is worth re-checking here because it is the reason the two cached
	// figures are expected to match: if a hit did announce, "no difference" would be the finding to
	// distrust rather than the one to report.
	if got := coord.announces.Load() - before; got != 0 {
		b.Fatalf("%d announcements from reads served entirely from cache; announcing bytes this node "+
			"already held tells peers nothing new and puts a coordinator call on the hot path", got)
	}
}

// BenchmarkUncachedReadWithoutCoordinator is the miss path with nothing to tell.
//
// Both uncached benchmarks evict the range inside the timed loop, because a miss requires one and doing
// it outside would measure b.N-1 hits. That makes the absolute figure a read-plus-evict rather than a
// read, and it is the same on both sides, so the difference between the two remains attributable to the
// coordinator. Compare the pair; do not read either number as read throughput.
func BenchmarkUncachedReadWithoutCoordinator(b *testing.B) {
	const key = "bench-uncached-solo.dat"

	fs, _, fh := benchReadFixture(b, key, benchObjectSize, nil)
	ctx := b.Context()
	dest := make([]byte, benchReadSize)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		fs.cache.Delete(key)

		if _, errno := fh.Read(ctx, dest, 0); errno != 0 {
			b.Fatalf("Read: errno %v", errno)
		}
	}
}

// BenchmarkUncachedReadWithCoordinator is the miss path that announces, which is the only place #141 adds
// work to a read.
func BenchmarkUncachedReadWithCoordinator(b *testing.B) {
	const key = "bench-uncached-cluster.dat"

	coord := &countingCoordinator{}
	fs, _, fh := benchReadFixture(b, key, benchObjectSize, coord)
	ctx := b.Context()
	dest := make([]byte, benchReadSize)

	before := coord.announces.Load()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		fs.cache.Delete(key)

		if _, errno := fh.Read(ctx, dest, 0); errno != 0 {
			b.Fatalf("Read: errno %v", errno)
		}
	}

	b.StopTimer()

	// Every iteration must have announced. Without this the benchmark would happily report a figure for
	// the path where announceCached returns early — no coordinator, no cached version, no bytes — and that
	// figure is indistinguishable from the nil-coordinator one by construction, which is exactly the
	// agreement it is supposed to be testing for.
	if got := coord.announces.Load() - before; got < int64(b.N) {
		b.Fatalf("%d announcements over %d uncached reads: this benchmark is not exercising the announce "+
			"path it exists to measure", got, b.N)
	}
}
