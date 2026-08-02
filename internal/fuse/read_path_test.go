//go:build linux || darwin

package fuse

// These tests exercise the FUSE read path against the real byte-range cache, the real write path, and a
// real S3 endpoint — no mocks on any seam. That is deliberate. Every defect covered here is a value
// correctly produced at one layer and dropped at the boundary to the next, and a mock on the far side of
// that boundary agrees with the caller by construction. The mapCache in filesystem_test.go returns
// whatever it was handed regardless of the requested length, so it would have reported a hit for exactly
// the calls the real cache correctly misses — the defect this file exists to pin.
//
// Assertions are on observed HTTP GETs and bytes transferred rather than on the cache's own hit counter.
// The counter is what the cache believes; the request log is what the user pays for, and H6 was a case of
// those two disagreeing.

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/objectfs/objectfs/internal/cache"
	"github.com/objectfs/objectfs/internal/testaws"
	"github.com/objectfs/objectfs/internal/vfs"
	"github.com/objectfs/objectfs/pkg/types"
)

// readPathFixture is a FileSystem wired to a real cache, a real write path, and a real S3 endpoint.
type readPathFixture struct {
	fs    *FileSystem
	srv   *testaws.TestServer
	cache *cache.LRUCache
}

func newReadPathFixture(t *testing.T) *readPathFixture {
	t.Helper()

	srv := testaws.Start(t)

	// Every read below is a ranged GET. Against an endpoint that ignores Range, the whole object comes
	// back and a prefix comparison succeeds, so these tests would certify the read path rather than
	// check it.
	srv.RequireRangeGET()

	backend := srv.Backend()

	writer, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("vfs.NewWriter: %v", err)
	}

	// A TTL long enough that nothing here expires on its own: the property under test is that a write
	// invalidates, not that staleness eventually times out.
	byteCache := cache.NewLRUCache(&cache.CacheConfig{
		MaxSize:    64 << 20,
		MaxEntries: 100000,
		TTL:        time.Hour,
	})
	t.Cleanup(func() { _ = byteCache.Close() })

	fs := NewFileSystem(backend, byteCache, writer, nil, &Config{
		DefaultMode: 0o644,
		DefaultUID:  1000,
		DefaultGID:  1000,
	})

	return &readPathFixture{fs: fs, srv: srv, cache: byteCache}
}

// open returns a handle for an object that already exists in the bucket, as Open would build one.
//
// The handle carries the path and nothing else. It used to carry the size and mode read from
// HeadObject at open time, which is what made a handle a second source of truth for two values that
// change under it — see the OpenFile doc comment.
func (f *readPathFixture) open(t *testing.T, key string) *FileHandle {
	t.Helper()

	if _, err := f.fs.backend.HeadObject(context.Background(), key); err != nil {
		t.Fatalf("HeadObject(%q): %v", key, err)
	}

	return &FileHandle{
		fs:     f.fs,
		handle: 1,
		file: &OpenFile{
			path:        key,
			lastAccess:  time.Now(),
			accessCount: 1,
		},
	}
}

// read performs one FUSE read of n bytes at off, the way the kernel does: a full buffer, regardless of
// how much file is left.
func (f *readPathFixture) read(t *testing.T, fh *FileHandle, off int64, n int) []byte {
	t.Helper()

	dest := make([]byte, n)

	result, errno := fh.Read(context.Background(), dest, off)
	if errno != 0 {
		t.Fatalf("Read(off=%d, n=%d): errno %v", off, n, errno)
	}

	got, status := result.Bytes(dest)
	if !status.Ok() {
		t.Fatalf("Read(off=%d, n=%d): result status %v", off, n, status)
	}

	return got
}

// TestShortFileIsServedFromCache is H6 at the layer where it mattered.
//
// The kernel reads a full buffer regardless of how much file remains, so every read of a file smaller
// than MaxRead over-asks past EOF. v0.10.0 passed that over-ask straight to the cache, which cannot
// answer it: never being told the object's length, it cannot distinguish "the file ends at 10240" from
// "only 10240 bytes are cached", and answering the short buffer would be indistinguishable from a
// truncated file. So it missed, correctly, every time — and no file below the read-buffer size was ever
// served from cache, no matter how many times it was read.
func TestShortFileIsServedFromCache(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	const (
		size         = 10240
		kernelBuffer = 131072 // FUSE's default MaxRead, 12.8x the file
	)

	content := f.srv.SeedRandom("small.dat", size)
	fh := f.open(t, "small.dat")

	first := f.read(t, fh, 0, kernelBuffer)
	if !bytes.Equal(first, content) {
		t.Fatalf("first read returned %d bytes, want the whole %d-byte file", len(first), size)
	}

	coldGETs := len(f.srv.GETs("small.dat"))
	if coldGETs == 0 {
		t.Fatal("the first read issued no GET, so nothing below distinguishes a cache hit from a miss")
	}

	coldBytes := f.srv.BytesRead("small.dat")

	// Nine more identical reads. Every one must be served from cache.
	for i := range 9 {
		got := f.read(t, fh, 0, kernelBuffer)
		if !bytes.Equal(got, content) {
			t.Fatalf("read %d returned %d bytes, want the whole %d-byte file", i+2, len(got), size)
		}
	}

	if extra := len(f.srv.GETs("small.dat")) - coldGETs; extra != 0 {
		t.Errorf("nine repeat reads of a fully-cached %d-byte file issued %d further GETs (%d extra "+
			"bytes transferred), want 0. An unclamped read asks the cache for %d bytes of a %d-byte "+
			"file, which it must refuse, so every read of a small file is an S3 request.",
			size, extra, f.srv.BytesRead("small.dat")-coldBytes, kernelBuffer, size)
	}

	if stats := f.cache.Stats(); stats.Hits == 0 {
		t.Errorf("the cache reports %d hits and %d misses after ten reads of one small file",
			stats.Hits, stats.Misses)
	}
}

// TestReadClampBoundaries pins the clamp at its edges, not only its interior.
func TestReadClampBoundaries(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	const size = 4096

	content := f.srv.SeedRandom("edge.dat", size)
	fh := f.open(t, "edge.dat")

	for _, probe := range []struct {
		name string
		off  int64
		n    int
		want int
	}{
		{"exactly to EOF", 0, size, size},
		{"straddling EOF", size - 100, 4096, 100},
		{"last byte", size - 1, 4096, 1},
		{"starting at EOF", size, 4096, 0},
		{"starting past EOF", size + 8192, 4096, 0},
	} {
		got := f.read(t, fh, probe.off, probe.n)

		if len(got) != probe.want {
			t.Errorf("%s: read(off=%d, n=%d) returned %d bytes, want %d",
				probe.name, probe.off, probe.n, len(got), probe.want)

			continue
		}

		if probe.want > 0 && !bytes.Equal(got, content[probe.off:probe.off+int64(probe.want)]) {
			t.Errorf("%s: read(off=%d, n=%d) returned the wrong bytes", probe.name, probe.off, probe.n)
		}
	}
}

// TestSequentialReadIsCachedWholesale is the same property across a file larger than one cache chunk.
func TestSequentialReadIsCachedWholesale(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	// Read-ahead off, so this measures the cache and nothing else. A prefetch worker issues its own
	// asynchronous GETs, and a GET that lands after the warm pass has been counted makes the assertion
	// depend on goroutine timing — which is how the first version of this test failed intermittently
	// while the cache was working correctly. Prefetch has its own tests below.
	f.fs.readAhead.Stop()
	f.fs.readAhead = nil

	const (
		size         = 3 * 1024 * 1024 // three 1 MiB cache chunks
		kernelBuffer = 131072
	)

	content := f.srv.SeedRandom("big.dat", size)
	fh := f.open(t, "big.dat")

	for off := int64(0); off < size; off += kernelBuffer {
		got := f.read(t, fh, off, kernelBuffer)
		if want := content[off:min(off+kernelBuffer, size)]; !bytes.Equal(got, want) {
			t.Fatalf("cold read at offset %d returned the wrong bytes", off)
		}
	}

	coldGETs := len(f.srv.GETs("big.dat"))

	// Second pass: every byte is held now, so nothing may reach S3.
	for off := int64(0); off < size; off += kernelBuffer {
		got := f.read(t, fh, off, kernelBuffer)
		if want := content[off:min(off+kernelBuffer, size)]; !bytes.Equal(got, want) {
			t.Fatalf("warm read at offset %d returned the wrong bytes", off)
		}
	}

	if extra := len(f.srv.GETs("big.dat")) - coldGETs; extra != 0 {
		t.Errorf("a second sequential pass over a fully-read %d-byte file issued %d GETs, want 0. "+
			"Consecutive reads land inside one chunk, so a cache that replaces a chunk's contents "+
			"instead of merging into it keeps only the last buffer of each and re-fetches the rest.",
			size, extra)
	}
}

// TestPrefetchDoesNotAmplifyReads pins total bytes transferred for a sequential traversal.
//
// Two separate defects made this number wrong, and both had to be fixed for it to hold:
//
// The prefetch window was shorter than the read it anticipated. A prefetch that fetches less than the
// reader asks for cannot satisfy that read at all — the cache answers only for a range it holds in
// full — so the entry was walked straight past and its bytes paid for twice, in egress and in cache
// capacity. The shipped default was a 64 KiB window against the kernel's 128 KiB MaxRead, so every
// prefetch of every sequential read of every file was waste: 4,325,644 bytes across 43 GETs for a
// 3,145,728-byte file.
//
// Matching the window to the read size then made prefetch and read issue byte-identical requests, and
// nothing deduplicated them. Which one reached S3 first was a race between a network round trip and
// the next read(2), so on an idle machine the prefetch won and the traversal transferred exactly the
// file, while on a loaded one the reader won and both fetched the same range: 5,373,952 bytes across
// 41 GETs — precisely 24 reads plus 17 prefetches, every prefetch duplicated. This test passed
// locally and failed in CI for that reason, which is the correct outcome for a defect that only
// appears when the machine is busy. FileSystem.fetch now shares one flight between the two.
//
// The assertion is bytes transferred, because that is what both defects cost. Latency barely moved,
// and a hit-rate assertion would have passed the first one — the prefetched entries were never hit,
// so they never counted as misses either.
func TestPrefetchDoesNotAmplifyReads(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	const (
		size         = 3 * 1024 * 1024
		kernelBuffer = 131072
	)

	f.srv.SeedRandom("seq.dat", size)
	fh := f.open(t, "seq.dat")

	for off := int64(0); off < size; off += kernelBuffer {
		f.read(t, fh, off, kernelBuffer)

		// Sleep so prefetches land while the traversal is still running, as they would under a real
		// reader. Without it they arrive after the loop and the amplification is invisible.
		time.Sleep(20 * time.Millisecond)
	}

	// Let anything still queued finish, so a late GET is counted rather than missed.
	time.Sleep(300 * time.Millisecond)

	// A full sequential traversal reads each byte once. Prefetch may reorder that work but must not add
	// to it: every prefetched byte is one the reader was going to ask for anyway.
	if got := f.srv.BytesRead("seq.dat"); got != size {
		t.Errorf("a sequential traversal of a %d-byte file transferred %d bytes (%.2fx) in %d GETs, "+
			"want exactly the file in %d GETs. Prefetch must not add work: it fetches the range the "+
			"reader is about to ask for, so an un-deduplicated prefetch is that read issued twice. "+
			"A count near %d means every prefetch lost its race with the read it was anticipating.",
			size, got, float64(got)/float64(size), len(f.srv.GETs("seq.dat")),
			size/kernelBuffer, size/kernelBuffer+17)
	}
}

// TestConcurrentIdenticalReadsShareOneGET is the deduplication on its own, without the prefetcher.
//
// The property the test above depends on is that two callers wanting the same bytes at the same time
// cost one request. Asserting it there means racing a prefetch worker, so the byte count moves with
// machine load — which is exactly how the defect came to pass locally and fail in CI. Here the race
// is arranged deliberately and the answer is not timing-dependent: N readers, one range, one GET.
//
// Reads are issued through separate handles, because that is the shape that occurs — a prefetch worker
// and a reader are different callers on the same object, as are two processes reading one file.
func TestConcurrentIdenticalReadsShareOneGET(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	// Read-ahead off: the point is what concurrent *readers* cost, and a prefetch worker issuing its
	// own GETs is the variable this test exists to remove.
	f.fs.readAhead.Stop()
	f.fs.readAhead = nil

	const (
		size    = 1024 * 1024
		want    = 131072
		readers = 8
	)

	content := f.srv.SeedRandom("shared.dat", size)

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([][]byte, readers)
	)

	for i := range readers {
		wg.Go(func() {
			fh := &FileHandle{
				fs:     f.fs,
				handle: uint64(i + 1), //nolint:gosec // a loop index cannot be negative
				file: &OpenFile{
					path:        "shared.dat",
					lastAccess:  time.Now(),
					accessCount: 1,
				},
			}

			// Release all readers at once, so they are genuinely in flight together rather than
			// serialized by goroutine startup.
			<-start

			dest := make([]byte, want)

			result, errno := fh.Read(context.Background(), dest, 0)
			if errno != 0 {
				t.Errorf("reader %d: errno %v", i, errno)

				return
			}

			got, status := result.Bytes(dest)
			if !status.Ok() {
				t.Errorf("reader %d: status %v", i, status)

				return
			}

			// Copy: the readers share one slice by design, and holding it past the read would let a
			// later assertion observe whatever the last writer left.
			results[i] = bytes.Clone(got)
		})
	}

	close(start)
	wg.Wait()

	for i, got := range results {
		if !bytes.Equal(got, content[:want]) {
			t.Errorf("reader %d got the wrong bytes. A shared flight must hand every waiter the same "+
				"complete result, not a partially-filled buffer", i)
		}
	}

	if gets := len(f.srv.GETs("shared.dat")); gets != 1 {
		t.Errorf("%d readers of the same range issued %d GETs, want 1. Every byte past the first "+
			"request is paid for twice and cached twice; this is the same range by construction, so "+
			"there is nothing to distinguish the requests and nothing to gain by sending them",
			readers, gets)
	}
}

// TestPrefetchStopsAtEndOfFile is the 416 InvalidRange at the end of every traversal.
//
// A sequential reader's predicted next offset runs past EOF by construction — the last read of a
// complete traversal is followed by a prediction one read beyond the end. v0.10.0 sent it, so reading
// any file to its end billed one guaranteed-failing request and logged an error for a range that cannot
// exist.
func TestPrefetchStopsAtEndOfFile(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	// The file has to be long enough for the detector to reach its prefetch threshold and still have
	// reads left to run off the end. A prefetch needs sequentialHits >= MinSequential (3) *and*
	// confidence > 0.5, and confidence is sequentialHits/10 — so seven consecutive reads before the
	// tail prefetch can even be scheduled. A 4 KiB file read in 1 KiB steps never gets there, which is
	// how the first version of this test came to pass against the unfixed code.
	const (
		step = 1024
		size = 16 * step
	)

	f.srv.SeedRandom("eof.dat", size)
	fh := f.open(t, "eof.dat")

	for off := int64(0); off < size; off += step {
		f.read(t, fh, off, step)
		time.Sleep(20 * time.Millisecond)
	}

	time.Sleep(300 * time.Millisecond)

	for _, req := range f.srv.GETs("eof.dat") {
		if req.Status == 416 {
			t.Errorf("reading a %d-byte file to its end issued a GET for %s, answered 416 InvalidRange. "+
				"A prefetch past EOF is a billed request and a logged error for a range that cannot "+
				"exist; the predicted offset has to be checked against the file's length.",
				size, req.Range)
		}
	}

	if got := f.srv.BytesRead("eof.dat"); got != size {
		t.Errorf("reading a %d-byte file to its end transferred %d bytes across %d GETs, want exactly "+
			"the file", size, got, len(f.srv.GETs("eof.dat")))
	}
}

// TestCacheHitsAreReportedToTheDetector pins the prefetcher against defeating itself.
//
// v0.10.0 called OnRead only on the cache-miss path, so a successful prefetch hid the next read from the
// pattern detector. The read *after* it was then compared against the offset of the read *before* it,
// appeared non-contiguous, and reset sequentialHits to zero — so the counter cycled 0→6→prefetch→0 and a
// long sequential traversal never reached steady state. One prefetch landed per seven reads, forever.
//
// The assertion is on the detector's own contiguity state rather than on GET counts, because the defect
// changes neither: every byte is fetched exactly once either way. What it changes is how many reads block
// on S3 — measured on a 3 MiB file read at 128 KiB, 3 of 24 reads were served from cache before the fix
// and 18 of 24 after. That ratio depends on prefetch workers winning a race against the reader, which is
// not a property a test should assert on. The state machine is.
func TestCacheHitsAreReportedToTheDetector(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	const (
		step = 4096
		size = 8 * step
	)

	f.srv.SeedRandom("detect.dat", size)

	// Serve every read from cache, deterministically: fill it before reading a byte, and take the
	// prefetch workers out of the picture entirely so nothing else can populate or race it. What remains
	// is exactly the question under test — does a hit reach the detector.
	f.fs.readAhead.Stop()
	f.fs.readAhead = NewReadAheadManager(f.fs, &ReadAheadConfig{
		Enabled:         true,
		WindowSize:      64 * 1024,
		MaxDistance:     1024 * 1024,
		MinSequential:   3,
		ConcurrentReads: 0, // no workers: schedulePrefetch fills the queue and nothing drains it
		TTL:             time.Minute,
	})
	t.Cleanup(f.fs.readAhead.Stop)

	f.cache.Put("detect.dat", 0, f.srv.GetObject("detect.dat"))

	fh := f.open(t, "detect.dat")

	reads := 0

	for off := int64(0); off < size; off += step {
		f.read(t, fh, off, step)
		reads++
	}

	if got := f.fs.GetStats().CacheHits; got != int64(reads) {
		t.Fatalf("%d of %d reads were served from cache; this test needs all of them to be, or it is "+
			"not measuring the hit path", got, reads)
	}

	f.fs.readAhead.mu.RLock()
	pattern := f.fs.readAhead.activeReads["detect.dat"]
	f.fs.readAhead.mu.RUnlock()

	if pattern == nil {
		t.Fatal("after 8 sequential cache hits the read-ahead detector has no pattern for the file at " +
			"all: cache hits are not reaching OnRead, so a prefetch that succeeds makes the reader " +
			"invisible to the very detector that scheduled it")
	}

	// One hit per read. The count includes the first, which the detector scores as sequential because a
	// freshly created pattern is zero-valued and a read at offset 0 satisfies offset == lastOffset +
	// lastSize — an eagerness that costs nothing and is not what this test is about.
	//
	// What matters is that the chain is unbroken. A read the detector never sees breaks contiguity across
	// itself: the next read is compared against the offset of the read before the gap, does not match,
	// and resets the counter to zero. So any invisible read shows up here as a shortfall.
	if pattern.sequentialHits != reads {
		t.Errorf("after %d consecutive sequential reads, all served from cache, sequentialHits is %d, "+
			"want %d. A read the detector never sees breaks the contiguity chain across it, which is "+
			"how a successful prefetch used to reset the very pattern that scheduled it.",
			reads, pattern.sequentialHits, reads)
	}
}

// TestReadAfterWriteReturnsNewBytes is H5.
//
// v0.10.0's read path asked the cache and the backend and never the write path. A buffered write is in
// neither, so reading back what was just written on the same descriptor returned pre-write bytes — for
// up to the cache's five-minute TTL once a prior read had populated it.
func TestReadAfterWriteReturnsNewBytes(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	original := f.srv.SeedRandom("rw.dat", 8192)
	fh := f.open(t, "rw.dat")

	// Read first, so the cache holds the pre-write bytes. Without this the test would also pass against
	// a cache that never populated at all.
	if got := f.read(t, fh, 0, 8192); !bytes.Equal(got, original) {
		t.Fatal("the initial read did not return the object's contents")
	}

	replacement := []byte("NEW BYTES AT OFFSET ZERO")

	written, errno := fh.Write(context.Background(), replacement, 0)
	if errno != 0 {
		t.Fatalf("Write: errno %v", errno)
	}

	if int(written) != len(replacement) {
		t.Fatalf("Write reported %d bytes for a %d-byte write", written, len(replacement))
	}

	// Before any flush, the read must see the new bytes.
	if got := f.read(t, fh, 0, len(replacement)); !bytes.Equal(got, replacement) {
		t.Errorf("after writing %q and before flushing, reading the same range returned %q. A read that "+
			"consults the cache and the object store but not the write path cannot see a pending write.",
			replacement, got)
	}

	// And the range past the write is still the stored object's.
	tail := f.read(t, fh, int64(len(replacement)), 512)
	if want := original[len(replacement) : len(replacement)+512]; !bytes.Equal(tail, want) {
		t.Error("the range past the pending write returned bytes that are neither the object's nor the " +
			"write's; overlaying a write must leave the rest of the file alone")
	}

	// After the flush the same read must still return the new bytes — now from the object itself.
	if errno := fh.Flush(context.Background()); errno != 0 {
		t.Fatalf("Flush: errno %v", errno)
	}

	stored := f.srv.GetObject("rw.dat")
	if len(stored) != len(original) {
		t.Errorf("after flushing a %d-byte write at offset 0, the stored object is %d bytes, want %d: "+
			"an offset write must splice into the object, not replace it",
			len(replacement), len(stored), len(original))
	}

	if !bytes.HasPrefix(stored, replacement) {
		t.Errorf("after flush the stored object begins %q, want it to begin with %q",
			stored[:min(len(stored), len(replacement))], replacement)
	}

	if got := f.read(t, fh, 0, len(replacement)); !bytes.Equal(got, replacement) {
		t.Errorf("after flush, reading the written range returned %q, want %q — the cache is still "+
			"serving the bytes it held before the object changed underneath it", got, replacement)
	}
}

// TestFlushInvalidatesCachedBytes states the invalidation half on its own.
//
// v0.10.0 had no call to cache.Delete anywhere in this package.
func TestFlushInvalidatesCachedBytes(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	original := f.srv.SeedRandom("inv.dat", 4096)
	fh := f.open(t, "inv.dat")

	// Populate the cache with the pre-write bytes.
	_ = f.read(t, fh, 0, 4096)

	if f.cache.Get("inv.dat", 0, 4096) == nil {
		t.Fatal("the read did not populate the cache, so this test cannot observe invalidation")
	}

	if _, errno := fh.Write(context.Background(), []byte("CHANGED"), 0); errno != 0 {
		t.Fatalf("Write: errno %v", errno)
	}

	if errno := fh.Flush(context.Background()); errno != 0 {
		t.Fatalf("Flush: errno %v", errno)
	}

	if held := f.cache.Get("inv.dat", 0, 4096); held != nil && bytes.Equal(held, original) {
		t.Error("the cache still holds the pre-write bytes after the flush. Every later read serves " +
			"them until the TTL expires — five minutes on the default config — so a program that " +
			"writes a file, closes it, and reads it back watches its own write disappear.")
	}
}

// TestMetadataCacheHits is the Lookup half of the H5/H6 gate.
//
// getCachedInfo asked the cache for a fixed 8192 bytes while cacheInfo stored a marshaled ObjectInfo of
// a few hundred. The cache correctly refused to answer, so the metadata cache never hit once for any
// path for the life of a mount: one S3 HEAD per path component on every stat, while reporting hits and
// misses as though it were working.
func TestMetadataCacheHits(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	// Payloads of deliberately different lengths, since the defect was a fixed request length.
	for i, meta := range []map[string]string{
		nil,
		{"x-owner": "scott"},
		{"x-owner": "scott", "x-project": "objectfs", "x-notes": string(bytes.Repeat([]byte("n"), 400))},
	} {
		path := fmt.Sprintf("meta/%d.dat", i)

		info := &types.ObjectInfo{
			Key:          path,
			Size:         int64(1 << i),
			LastModified: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
			ETag:         `"d41d8cd98f00b204e9800998ecf8427e"`,
			Metadata:     meta,
		}

		f.fs.cacheInfo(path, info)

		got := f.fs.getCachedInfo(path)
		if got == nil {
			t.Errorf("%s: getCachedInfo returned nil immediately after cacheInfo stored %d bytes of "+
				"metadata. A metadata cache that cannot hit costs one S3 HEAD per path component on "+
				"every stat.", path, len(meta))

			continue
		}

		if got.Key != info.Key || got.Size != info.Size || got.ETag != info.ETag {
			t.Errorf("%s: round-tripped ObjectInfo differs: got %+v, want %+v", path, got, info)
		}

		if len(got.Metadata) != len(meta) {
			t.Errorf("%s: round-tripped %d metadata entries, want %d", path, len(got.Metadata), len(meta))
		}
	}

	if stats := f.cache.Stats(); stats.HitRate == 0 {
		t.Errorf("the metadata cache reports a hit rate of 0 (%d hits, %d misses) after three "+
			"store-then-load round trips", stats.Hits, stats.Misses)
	}
}

// TestMetadataInvalidationIsExact checks that dropping a path's attributes does not drop its content, or
// another path's anything. metaCacheKey shares a keyspace with object content, and invalidate deletes
// both keys for one path.
func TestMetadataInvalidationIsExact(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	f.srv.SeedRandom("a.dat", 2048)
	f.srv.SeedRandom("ab.dat", 2048)

	for _, key := range []string{"a.dat", "ab.dat"} {
		fh := f.open(t, key)
		_ = f.read(t, fh, 0, 2048)
		f.fs.cacheInfo(key, &types.ObjectInfo{Key: key, Size: 2048})
	}

	f.fs.invalidate("a.dat")

	if f.cache.Get("a.dat", 0, 2048) != nil {
		t.Error("invalidate left a.dat's content bytes cached")
	}

	if f.fs.getCachedInfo("a.dat") != nil {
		t.Error("invalidate left a.dat's attributes cached; a write changes the size and mtime too, so " +
			"keeping them leaves stat and read disagreeing about the same file")
	}

	if f.cache.Get("ab.dat", 0, 2048) == nil {
		t.Error("invalidating a.dat also dropped ab.dat's content: Delete is matching a bare prefix " +
			"rather than the whole object name")
	}

	if f.fs.getCachedInfo("ab.dat") == nil {
		t.Error("invalidating a.dat also dropped ab.dat's attributes")
	}
}

// TestGetattrReportsPendingSize is the size half of read-after-write.
//
// The kernel clamps a read to whatever stat reported, so a file reported at its pre-write length cannot
// be read past that length however correct the read path is.
func TestGetattrReportsPendingSize(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	original := f.srv.SeedRandom("grow.dat", 1024)

	node := &FileNode{fs: f.fs, path: "grow.dat"}
	fh := f.open(t, "grow.dat")

	appended := []byte("APPENDED")
	if _, errno := fh.Write(context.Background(), appended, int64(len(original))); errno != 0 {
		t.Fatalf("Write: errno %v", errno)
	}

	var out fuse.AttrOut
	if errno := node.Getattr(context.Background(), fh, &out); errno != 0 {
		t.Fatalf("Getattr: errno %v", errno)
	}

	want := uint64(len(original) + len(appended))
	if out.Size != want {
		t.Errorf("Getattr reports size %d after appending %d bytes to a %d-byte file, want %d. The "+
			"kernel clamps reads to the size stat reported, so an understated size truncates reads of "+
			"a file with writes still pending.", out.Size, len(appended), len(original), want)
	}

	// And the appended bytes must be readable at their offset, which is the point of reporting the size.
	got := f.read(t, fh, int64(len(original)), 4096)
	if !bytes.Equal(got, appended) {
		t.Errorf("reading at offset %d returned %q, want %q", len(original), got, appended)
	}
}
