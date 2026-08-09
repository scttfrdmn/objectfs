//go:build linux || darwin

package fuse

// These tests cover what the read and write paths tell the cluster (#141), against a real S3 endpoint, a
// real byte-range cache, and a real write path — mocked only at the seam under test, which is the
// coordinator itself.
//
// That one mock is unavoidable and worth justifying, given that read_path_test.go in this package refuses
// mocks on principle. The alternative is two gossip sockets over loopback, and it would move the
// assertion to the wrong place: what is under test here is *which announcements and invalidations this
// package makes, with which fields*, and internal/distributed's own tests already cover what a peer does
// with one. A recording coordinator is the only way to see a call whose errors are logged and never
// returned.
//
// What is deliberately *not* mocked is the metadata cache, and that matters more than it looks: the ETag
// an announcement carries comes from it, so a fake there would let the announce path pass while reading a
// version from a source production does not have. The cache is real and the object's version is put in it
// the way the kernel does — by a stat before the read.

import (
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	gofuse "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/internal/vfs"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// coordFixture is a FileSystem with a recording coordinator on it.
type coordFixture struct {
	fs    *FileSystem
	srv   *testaws.TestServer
	coord *recordingCoordinator
}

func newCoordFixture(t *testing.T) *coordFixture {
	t.Helper()

	srv := testaws.Start(t)

	// Every read below is ranged. Against an endpoint that ignores Range the whole object comes back, and
	// an announcement's Length would describe the object rather than the range that was cached.
	srv.RequireRangeGET()

	backend := srv.Backend()

	writer, err := vfs.NewWriter(t.Context(), backend)
	if err != nil {
		t.Fatalf("vfs.NewWriter: %v", err)
	}

	byteCache := cache.NewLRUCache(&cache.CacheConfig{
		MaxSize:    16 << 20,
		MaxEntries: 10000,
		TTL:        time.Hour,
	})
	t.Cleanup(func() { _ = byteCache.Close() })

	coord := &recordingCoordinator{}

	fs := NewFileSystem(t.Context(), backend, byteCache, writer, nil, &Config{
		DefaultMode:    0o644,
		DefaultDirMode: 0o755,
		DefaultUID:     1000,
		DefaultGID:     1000,
	})
	fs.coordinator = coord

	return &coordFixture{fs: fs, srv: srv, coord: coord}
}

// root returns the mount's root node, attached to a bridge.
//
// Attached, because Mkdir and Create build a child inode through Inode.NewInode, which dereferences the
// bridge — a bare &DirectoryNode{} panics there. [readPathFixture.root] in attributes_test.go does the
// same thing for the same reason; this is the same helper on a fixture that carries a coordinator.
func (f *coordFixture) root(t *testing.T) *DirectoryNode {
	t.Helper()

	root, ok := f.fs.Root().(*DirectoryNode)
	if !ok {
		t.Fatalf("FileSystem.Root returned %T, want *DirectoryNode", f.fs.Root())
	}

	timeout := f.fs.attrTimeout()
	_ = gofuse.NewNodeFS(root, &gofuse.Options{
		AttrTimeout:     &timeout,
		EntryTimeout:    &timeout,
		NullPermissions: true,
	})

	return root
}

// stat populates the metadata cache the way the kernel does before a read.
//
// Through Lookup rather than by writing to the cache directly, because the announce path's whole
// dependency is that the version it needs is *already there* by the time a read misses — put there by the
// stat the kernel must issue to learn a size. A test that seeded the cache itself would pass whether or
// not that were true.
func (f *coordFixture) stat(t *testing.T, key string) {
	t.Helper()

	var out fuse.EntryOut
	if _, errno := f.root(t).Lookup(t.Context(), key, &out); errno != 0 {
		t.Fatalf("Lookup(%q) returned %v", key, errno)
	}
}

// open returns a handle for an existing object, as [readPathFixture.open] does.
func (f *coordFixture) open(key string) *FileHandle {
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

// read performs one FUSE read of n bytes at off.
func (f *coordFixture) read(t *testing.T, fh *FileHandle, off int64, n int) []byte {
	t.Helper()

	dest := make([]byte, n)

	result, errno := fh.Read(t.Context(), dest, off)
	if errno != 0 {
		t.Fatalf("Read(off=%d, n=%d): errno %v", off, n, errno)
	}

	got, status := result.Bytes(dest)
	if !status.Ok() {
		t.Fatalf("Read(off=%d, n=%d): result status %v", off, n, status)
	}

	return got
}

// TestReadAnnouncesWhatItCached is #141's acceptance criterion for the announce half, and every field is
// asserted because every one of them is a field a peer's fetch decision depends on.
//
// The Size/Length distinction is the one worth stating: Size is the whole object's and Length is the
// range's. Announcing len(data) as both would tell a peer that a 4 KiB range of a 64 KiB object is the
// entire file, and the peer would fetch 4 KiB believing it had the object.
func TestReadAnnouncesWhatItCached(t *testing.T) {
	t.Parallel()

	f := newCoordFixture(t)

	const (
		key      = "announced.dat"
		size     = 65536
		readAt   = 4096
		readSize = 8192
	)

	f.srv.SeedRandom(key, size)
	f.stat(t, key)

	info, err := f.fs.backend.HeadObject(t.Context(), key)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	fh := f.open(key)
	got := f.read(t, fh, readAt, readSize)
	if len(got) != readSize {
		t.Fatalf("read %d bytes, want %d", len(got), readSize)
	}

	anns := f.coord.announcements()
	if len(anns) == 0 {
		t.Fatal("a read that populated the cache announced nothing, so a peer wanting these bytes reads " +
			"S3 while this node holds them")
	}

	// The read-ahead manager issues its own fetches, each of which announces, so this asserts on the
	// announcement for the range actually read rather than on there being exactly one.
	var ann *types.KeyAnnouncement
	for i := range anns {
		if anns[i].Offset == readAt {
			ann = &anns[i]

			break
		}
	}
	if ann == nil {
		t.Fatalf("no announcement for the range that was read (offset %d); got %+v", readAt, anns)
	}

	if ann.Key != key {
		t.Errorf("announced key %q, want %q", ann.Key, key)
	}

	if ann.ETag != info.ETag {
		t.Errorf("announced ETag %q, want the object's %q; a peer checks what it fetches against this, so "+
			"a wrong version costs it a rejected fetch and an S3 read", ann.ETag, info.ETag)
	}

	if ann.Size != size {
		t.Errorf("announced Size %d, want the object's %d — Size is the object's length, not the range's",
			ann.Size, size)
	}

	if ann.Length != readSize {
		t.Errorf("announced Length %d, want the range's %d — a peer that asks for more than the holder "+
			"cached makes the holder fetch from S3, which is slower than the peer reading S3 itself",
			ann.Length, readSize)
	}

	if ann.CachedAt.IsZero() {
		t.Error("announced a zero CachedAt; a recipient prefers the freshest of several claims for a key " +
			"and cannot order them without it")
	}

	// NodeID is deliberately empty here: [distributed.Coordinator.AnnounceKey] overwrites it with the
	// cluster's own ID, because a node announcing under another's would send peers to a host that never
	// cached the bytes. This package has no access to the node ID and must not invent one.
	if ann.NodeID != "" {
		t.Errorf("announced NodeID %q; this package does not know the cluster's node ID and the "+
			"coordinator overwrites the field, so setting it here can only be wrong", ann.NodeID)
	}
}

// TestReadWithNoKnownVersionDoesNotAnnounce is the fail-closed direction, applied to the one field #141's
// specification does not carry.
//
// The bytes come from [types.Backend.GetObject], which returns no version, so the ETag comes from the
// metadata cache — and when there is nothing there, there is no version to name. An announcement with an
// empty ETag would be refused by [distributed.Coordinator.AnnounceKey] anyway; what must not happen is
// this package inventing one to fill the field, because a peer that fetches bytes it cannot place against
// a version of the object hands them to a reading process as file content.
//
// A read with no preceding stat is not contrived — [FileHandle.Read] does not stat, and a handle can
// outlive its metadata cache entry, which expires on the metadata TTL while the content entry has its own.
func TestReadWithNoKnownVersionDoesNotAnnounce(t *testing.T) {
	t.Parallel()

	f := newCoordFixture(t)

	const key = "unstatted.dat"
	f.srv.SeedRandom(key, 32768)

	// No f.stat: nothing populates the metadata cache, so no version is known for the object.
	fh := f.open(key)
	if got := f.read(t, fh, 0, 4096); len(got) != 4096 {
		t.Fatalf("read %d bytes, want 4096", len(got))
	}

	if anns := f.coord.announcements(); len(anns) != 0 {
		t.Errorf("announced %+v with no version known for the object; a peer fetching bytes it cannot "+
			"place against an object version hands them to a reading process as file content", anns)
	}
}

// TestReadStillSucceedsWhenAnnouncingFails is the fire-and-forget property, asserted against a call that
// actually fails.
//
// An announcement is an optimization: a peer that never hears it reads from S3, which is correct and
// merely slower. Failing the read(2) over it would turn a slower read into no read at all.
func TestReadStillSucceedsWhenAnnouncingFails(t *testing.T) {
	t.Parallel()

	f := newCoordFixture(t)
	f.coord.announceErr = errors.New("gossip is not listening")

	const key = "announce-fails.dat"
	content := f.srv.SeedRandom(key, 8192)
	f.stat(t, key)

	fh := f.open(key)
	got := f.read(t, fh, 0, 4096)

	if len(got) != 4096 || string(got) != string(content[:4096]) {
		t.Errorf("a read whose announcement failed returned %d wrong bytes; the announcement is an "+
			"optimization and its failure must not reach the reader", len(got))
	}

	if len(f.coord.announcements()) == 0 {
		t.Error("the announcement was not attempted, so this test asserts nothing about its failure")
	}
}

// TestFlushInvalidatesOnPeersWithTheWriteOwnETag is the invalidate half's headline, and the ETag is the
// assertion that matters.
//
// [types.DistributedCoordinator.InvalidateKey] requires the version *the write itself* reported, not one
// from a later HeadObject, which could name a third node's subsequent write. A receiver's replay ledger is
// keyed on (key, etag), so a version naming somebody else's write suppresses an invalidation that has not
// been applied — and the peer keeps serving bytes that were replaced.
func TestFlushInvalidatesOnPeersWithTheWriteOwnETag(t *testing.T) {
	t.Parallel()

	f := newCoordFixture(t)

	const key = "written.dat"

	if err := f.fs.buffer.Write(key, 0, []byte("hello cluster")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	fh := f.open(key)
	if errno := fh.Flush(t.Context()); errno != 0 {
		t.Fatalf("Flush returned %v", errno)
	}

	info, err := f.fs.backend.HeadObject(t.Context(), key)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	invs := f.coord.invalidations()
	if len(invs) == 0 {
		t.Fatal("a flush told no peer to evict the key it just wrote, so every peer holding it serves the " +
			"previous contents until its cache expires")
	}

	var content *invalidation
	for i := range invs {
		if invs[i].key == key {
			content = &invs[i]

			break
		}
	}
	if content == nil {
		t.Fatalf("no invalidation for %q; got %+v", key, invs)
	}

	if content.etag == "" {
		t.Error("invalidated with no version; every receiver then applies it on every retransmission, " +
			"which is safe but is not what a write that knows its own ETag should send")
	}

	if content.etag != info.ETag {
		t.Errorf("invalidated at version %q but the stored object is %q. A receiver's replay ledger is "+
			"keyed on this, so a version naming a different write suppresses an invalidation that was "+
			"never applied", content.etag, info.ETag)
	}
}

// TestFlushInvalidatesTheMetadataKeyToo covers the half that is invisible on one node.
//
// [FileSystem.invalidate] drops both the content key and metaCacheKey(path), because a write changes the
// bytes *and* the size and mtime a stat reports; dropping only one leaves a file whose stat size does not
// match what read returns. A peer receiving an invalidation evicts one key —
// [distributed.GossipProtocol.handleCacheInvalidate] calls cache.Delete(m.Key) — so both have to be sent,
// and the absence of the metadata one produces exactly that disagreement across a node boundary.
//
// The metadata key travels unversioned even though the content key has an ETag. The version names the
// object's *bytes*, and the ledger is keyed on (key, etag): reusing it for the metadata key would claim to
// describe bytes that key does not hold.
func TestFlushInvalidatesTheMetadataKeyToo(t *testing.T) {
	t.Parallel()

	f := newCoordFixture(t)

	const key = "both-keys.dat"

	if err := f.fs.buffer.Write(key, 0, []byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	fh := f.open(key)
	if errno := fh.Flush(t.Context()); errno != 0 {
		t.Fatalf("Flush returned %v", errno)
	}

	var meta *invalidation
	for _, inv := range f.coord.invalidations() {
		if inv.key == metaCacheKey(key) {
			meta = &inv

			break
		}
	}

	if meta == nil {
		t.Fatalf("the write did not invalidate %q on peers, so a peer that has stat'ed the object reports "+
			"its pre-write size while serving post-write content: got %v", metaCacheKey(key),
			f.coord.invalidatedKeys())
	}

	if meta.etag != "" {
		t.Errorf("the metadata key was invalidated at version %q; that ETag names the object's bytes, "+
			"which this key does not hold, and it writes a ledger entry claiming otherwise", meta.etag)
	}
}

// TestFlushStillSucceedsWhenInvalidatingFails asserts the write path does not fail over an invalidation.
//
// Unlike an announcement, an unsent invalidation is not merely slower — peers keep serving replaced bytes
// until their cache TTL expires. Failing close(2) over it would still be wrong: the object *is* durable
// and the caller's data *is* stored, so reporting failure would tell a program its write was lost when it
// was not. The gap is bounded by the TTL and reported by a Warn log; that is the trade, made explicitly.
func TestFlushStillSucceedsWhenInvalidatingFails(t *testing.T) {
	t.Parallel()

	f := newCoordFixture(t)
	f.coord.invalidateErr = errors.New("gossip is not listening")

	const key = "invalidate-fails.dat"
	const content = "durable regardless"

	if err := f.fs.buffer.Write(key, 0, []byte(content)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	fh := f.open(key)
	if errno := fh.Flush(t.Context()); errno != 0 {
		t.Fatalf("Flush returned %v after a failed invalidation; the object is durable and reporting "+
			"failure would tell the program its write was lost", errno)
	}

	// The object, not the call log: what makes the errno above correct is that the bytes really are stored.
	stored, err := f.fs.backend.GetObject(t.Context(), key, 0, 0)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if string(stored) != content {
		t.Errorf("stored %q, want %q", stored, content)
	}
}

// TestDeletesInvalidateOnPeersWithNoVersion covers every mutation that has no ETag to name, in one table.
//
// An empty ETag is legal and documented: a delete has no version, and neither does an unconditional
// PutObject, which reports nothing. Every receiver then applies the invalidation every time — a redundant
// eviction, which can never serve stale bytes. What must not happen is a version being invented to fill
// the field.
func TestDeletesInvalidateOnPeersWithNoVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// seed prepares the bucket and returns the key the operation should invalidate.
		seed func(t *testing.T, f *coordFixture) string
		run  func(t *testing.T, f *coordFixture, key string)
	}{
		{
			name: "unlink",
			seed: func(t *testing.T, f *coordFixture) string {
				t.Helper()

				const key = "doomed.dat"
				if err := f.fs.backend.PutObject(t.Context(), key, []byte("bytes"), nil); err != nil {
					t.Fatalf("seed: %v", err)
				}

				return key
			},
			run: func(t *testing.T, f *coordFixture, key string) {
				t.Helper()

				root := f.root(t)
				if errno := root.Unlink(t.Context(), key); errno != 0 {
					t.Fatalf("Unlink returned %v", errno)
				}
			},
		},
		{
			name: "mkdir",
			seed: func(t *testing.T, f *coordFixture) string {
				t.Helper()

				return "newdir/"
			},
			run: func(t *testing.T, f *coordFixture, key string) {
				t.Helper()

				root := f.root(t)

				var out fuse.EntryOut
				if _, errno := root.Mkdir(t.Context(), strings.TrimSuffix(key, "/"), 0o755,
					&out); errno != 0 {
					t.Fatalf("Mkdir returned %v", errno)
				}
			},
		},
		{
			name: "rmdir",
			seed: func(t *testing.T, f *coordFixture) string {
				t.Helper()

				const key = "gone/"
				if err := f.fs.backend.PutObject(t.Context(), key, []byte{}, nil); err != nil {
					t.Fatalf("seed: %v", err)
				}

				return key
			},
			run: func(t *testing.T, f *coordFixture, key string) {
				t.Helper()

				root := f.root(t)
				if errno := root.Rmdir(t.Context(), strings.TrimSuffix(key, "/")); errno != 0 {
					t.Fatalf("Rmdir returned %v", errno)
				}
			},
		},
		{
			name: "create",
			seed: func(t *testing.T, f *coordFixture) string {
				t.Helper()

				return "fresh.dat"
			},
			run: func(t *testing.T, f *coordFixture, key string) {
				t.Helper()

				root := f.root(t)

				var out fuse.EntryOut
				_, _, _, errno := root.Create(t.Context(), key, uint32(syscall.O_WRONLY), 0o644, &out)
				if errno != 0 {
					t.Fatalf("Create returned %v", errno)
				}

				// The handle is deliberately left unreleased. Release flushes, and the flush invalidates
				// the same key — so a Release here would satisfy the assertion below whether or not Create
				// invalidated anything, which is exactly what a mutation showed: removing Create's own
				// call left this subtest green until the Release came out.
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newCoordFixture(t)
			key := tc.seed(t, f)
			tc.run(t, f, key)

			var found *invalidation
			for _, inv := range f.coord.invalidations() {
				if inv.key == key {
					found = &inv

					break
				}
			}

			if found == nil {
				t.Fatalf("%s did not invalidate %q on peers; got %v", tc.name, key,
					f.coord.invalidatedKeys())
			}

			if found.etag != "" {
				t.Errorf("%s invalidated %q at version %q; this operation reports no version, so the "+
					"only honest answer is an empty ETag and inventing one is what the interface "+
					"forbids", tc.name, key, found.etag)
			}
		})
	}
}

// TestRenameInvalidatesBothNamesOnPeers is the one operation whose invalidations a single-node test cannot
// distinguish from half of them.
//
// A rename changes two keys, and a peer that holds either serves a file that has moved. Forgetting the
// source leaves peers serving a file that no longer exists at that name; forgetting the destination leaves
// them serving whatever the rename overwrote.
func TestRenameInvalidatesBothNamesOnPeers(t *testing.T) {
	t.Parallel()

	f := newCoordFixture(t)

	const (
		src = "before.dat"
		dst = "after.dat"
	)

	if err := f.fs.backend.PutObject(t.Context(), src, []byte("moved bytes"), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	root := f.root(t)
	if errno := root.Rename(t.Context(), src, root, dst, 0); errno != 0 {
		t.Fatalf("Rename returned %v", errno)
	}

	keys := f.coord.invalidatedKeys()

	for _, want := range []string{src, dst} {
		if !slices.Contains(keys, want) {
			t.Errorf("rename did not invalidate %q on peers; a peer holding it serves a file that has "+
				"moved. Invalidated: %v", want, keys)
		}
	}
}

// TestNoCoordinatorMakesNoCalls is the single-node guarantee, asserted where it is cheapest to break.
//
// Nearly every mount takes this path. The assertion is not that the calls are skipped — there is nothing to
// call — but that the read and write paths behave identically: a nil coordinator must not change a byte of
// what a read returns or what a flush stores. A panic here would be the visible failure; a changed answer
// would be the invisible one, which is why both are checked.
func TestNoCoordinatorMakesNoCalls(t *testing.T) {
	t.Parallel()

	f := newCoordFixture(t)
	f.fs.coordinator = nil

	const readKey = "solo-read.dat"
	content := f.srv.SeedRandom(readKey, 8192)
	f.stat(t, readKey)

	if got := f.read(t, f.open(readKey), 0, 4096); string(got) != string(content[:4096]) {
		t.Error("a read on a mount with no coordinator returned different bytes")
	}

	// A key of its own for the write half. Writing over the read key would exercise read-modify-write —
	// correct behavior, since a partial write to an existing object must preserve the tail — and the
	// assertion below would then be comparing against the wrong thing for reasons that have nothing to do
	// with a coordinator.
	const (
		writeKey = "solo-write.dat"
		written  = "written without a cluster"
	)

	if err := f.fs.buffer.Write(writeKey, 0, []byte(written)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if errno := f.open(writeKey).Flush(t.Context()); errno != 0 {
		t.Fatalf("Flush returned %v on a mount with no coordinator", errno)
	}

	stored, err := f.fs.backend.GetObject(t.Context(), writeKey, 0, 0)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if string(stored) != written {
		t.Errorf("stored %q, want %q", stored, written)
	}

	if anns := f.coord.announcements(); len(anns) != 0 {
		t.Errorf("the coordinator was called with %d announcements after being set to nil", len(anns))
	}
	if invs := f.coord.invalidations(); len(invs) != 0 {
		t.Errorf("the coordinator was called with %d invalidations after being set to nil", len(invs))
	}
}

// TestAnnounceCachedRefusesAnEmptyRead pins the guard on len(data), which exists for a reason worth
// recording rather than as input hygiene.
//
// A read at EOF returns zero bytes and caches nothing. Announcing that would tell a peer this node holds a
// zero-length range of the key — actionable-looking and useless: the peer dials, gets nothing, and reads
// S3 anyway, having paid a round trip to learn what no announcement would have told it for free.
func TestAnnounceCachedRefusesAnEmptyRead(t *testing.T) {
	t.Parallel()

	f := newCoordFixture(t)

	const key = "empty-range.dat"
	f.srv.SeedRandom(key, 4096)
	f.stat(t, key)

	f.fs.announceCached(t.Context(), key, 4096, nil)
	f.fs.announceCached(t.Context(), key, 4096, []byte{})

	if anns := f.coord.announcements(); len(anns) != 0 {
		t.Errorf("announced a zero-length range: %+v. A peer acting on it dials this node, receives "+
			"nothing, and reads S3 having paid a round trip for the privilege", anns)
	}
}
