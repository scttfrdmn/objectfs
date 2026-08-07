//go:build linux || darwin

package fuse

// These tests cover unlink and rmdir (#163) against a real S3 endpoint, a real write path, and a real
// cache — no mock on any seam.
//
// The reason is specific to this operation rather than a general preference. go-fuse defaults an
// unimplemented NodeUnlinker or NodeRmdirer to *success*, so the failure mode here is not an error, it
// is `rm` exiting 0 while the object stays in the bucket. A mock backend asked "was Delete called"
// answers the question the implementation was written to answer; only the object's actual absence
// afterwards distinguishes a delete from a report of one. Every assertion below is on storage state.

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	gofuse "github.com/hanwen/go-fuse/v2/fs"

	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// deleteFixture is a FileSystem wired to a real S3 endpoint, write path, and cache.
type deleteFixture struct {
	fs  *FileSystem
	srv *testaws.TestServer
}

func newDeleteFixture(t *testing.T) *deleteFixture {
	t.Helper()

	srv := testaws.Start(t)
	backend := srv.Backend()

	writer, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("vfs.NewWriter: %v", err)
	}

	byteCache := cache.NewLRUCache(&cache.CacheConfig{
		MaxSize:    16 << 20,
		MaxEntries: 10000,
		TTL:        time.Hour,
	})
	t.Cleanup(func() { _ = byteCache.Close() })

	fs := NewFileSystem(t.Context(), backend, byteCache, writer, nil, &Config{
		DefaultMode:    0o644,
		DefaultDirMode: 0o755,
		DefaultUID:     1000,
		DefaultGID:     1000,
	})

	return &deleteFixture{fs: fs, srv: srv}
}

// exists reports whether an object is in the bucket, asked of the backend directly rather than through
// any ObjectFS layer. The point of these tests is what storage holds, so the check must not run through
// a cache that could answer from before the delete.
func (f *deleteFixture) exists(t *testing.T, key string) bool {
	t.Helper()

	_, err := f.fs.backend.HeadObject(context.Background(), key)
	if err == nil {
		return true
	}
	if vfs.IsNotFound(err) {
		return false
	}
	t.Fatalf("HeadObject(%q): %v", key, err)

	return false
}

// TestUnlinkRemovesTheObject is #163's headline assertion.
//
// Through v0.10.3 this could not pass: Unlink returned EROFS, which was the deliberate interim fix —
// failing loudly beats go-fuse's default of reporting success for a delete that never happened. This
// asserts the object is actually gone, which is the only thing that distinguishes the two.
func TestUnlinkRemovesTheObject(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)
	ctx := context.Background()

	const key = "doomed.txt"
	if err := f.fs.backend.PutObject(ctx, key, []byte("delete me"), nil); err != nil {
		t.Fatalf("seed the object: %v", err)
	}
	if !f.exists(t, key) {
		t.Fatal("seeded object is not present")
	}

	root := &DirectoryNode{fs: f.fs, path: ""}
	if errno := root.Unlink(ctx, key); errno != 0 {
		t.Fatalf("Unlink returned %v, want success", errno)
	}

	if f.exists(t, key) {
		t.Error("Unlink reported success and the object is still in the bucket. This is the failure " +
			"go-fuse's default produces for an unimplemented NodeUnlinker: rm exits 0, the kernel drops " +
			"the inode, and the object keeps billing with no path that reaches it")
	}
}

// TestUnlinkDiscardsPendingWritesSoTheFileCannotComeBack pins that a deleted file's pending writes are
// destroyed.
//
// `echo x > f; rm f` writes through the buffer and deletes before any flush. If unlink deletes the
// object and leaves the dirty ranges, the next flush — or the unmount — PUTs them back: the file
// returns, at the size it was written, with no error anywhere. A delete that can be undone by a
// background flush is not a delete.
//
// What this does *not* pin is the discard/delete order. Both orders discard, so both pass here; see
// Unlink's doc comment, which records that the order closes a concurrent-flush window rather than
// changing this sequential outcome.
func TestUnlinkDiscardsPendingWritesSoTheFileCannotComeBack(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)
	ctx := context.Background()

	const key = "written-then-removed.txt"

	// Write through the write path and do not flush, which is the state rm ordinarily finds a file in.
	if err := f.fs.buffer.Write(key, 0, []byte("this should never reach storage")); err != nil {
		t.Fatalf("buffered write: %v", err)
	}
	if !f.fs.buffer.Dirty(key) {
		t.Fatal("write path reports nothing dirty after a write; the fixture is not exercising the buffer")
	}

	root := &DirectoryNode{fs: f.fs, path: ""}
	if errno := root.Unlink(ctx, key); errno != 0 {
		t.Fatalf("Unlink returned %v, want success", errno)
	}

	if f.fs.buffer.Dirty(key) {
		t.Error("the write path still holds dirty state for a deleted file; the next flush will " +
			"recreate the object")
	}

	// The decisive step: flush everything, exactly as unmount does, and confirm nothing was resurrected.
	if err := f.fs.buffer.FlushAll(); err != nil {
		t.Fatalf("FlushAll after the unlink: %v", err)
	}

	if f.exists(t, key) {
		t.Error("flushing after the unlink recreated the object. The deleted file came back from the " +
			"write path's pending ranges, which is why Unlink must discard before it deletes")
	}
}

// TestUnlinkOfAMissingFileReportsENOENT pins that absence is an error rather than a success.
//
// rm distinguishes the two, POSIX requires it, and treating absence as success is a short step from the
// original defect — a filesystem that reports every delete as having worked.
func TestUnlinkOfAMissingFileReportsENOENT(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)

	root := &DirectoryNode{fs: f.fs, path: ""}
	errno := root.Unlink(context.Background(), "never-existed.txt")

	if errno == 0 {
		t.Fatal("Unlink of a missing file reported success")
	}
	if errno != syscall.ENOENT {
		t.Errorf("Unlink of a missing file returned %v, want ENOENT", errno)
	}
}

// TestUnlinkRemovesAFileThatHasNoObjectYet is the other half of the existence check, and the case
// that makes consulting only the backend wrong.
//
// Create records attributes through the write path and PUTs nothing, so between `touch f` and the first
// flush the file is real and visible to stat while no object exists. A HeadObject-only existence check
// would answer ENOENT and make `touch f && rm f` fail on a file the user can see.
func TestUnlinkRemovesAFileThatHasNoObjectYet(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)
	ctx := context.Background()

	const key = "created-never-flushed.txt"
	if err := f.fs.buffer.Write(key, 0, []byte("only in memory")); err != nil {
		t.Fatalf("buffered write: %v", err)
	}
	if f.exists(t, key) {
		t.Fatal("the fixture flushed; this test needs a file with no object behind it")
	}

	root := &DirectoryNode{fs: f.fs, path: ""}
	if errno := root.Unlink(ctx, key); errno != 0 {
		t.Fatalf("Unlink of a file that exists only in the write path returned %v, want success. "+
			"An existence check that asks only the backend reports ENOENT here, which makes "+
			"`touch f && rm f` fail on a file stat can see", errno)
	}

	if f.fs.buffer.Dirty(key) {
		t.Error("the write path still holds the deleted file")
	}
}

// TestUnlinkRefusesOnAReadOnlyMount pins that the read-only flag is honored before anything is
// destroyed. The check has to precede both the discard and the delete: a read-only mount that drops
// buffered writes before refusing would lose data it was configured never to modify.
func TestUnlinkRefusesOnAReadOnlyMount(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)
	f.fs.config.ReadOnly = true
	ctx := context.Background()

	const key = "protected.txt"
	if err := f.fs.backend.PutObject(ctx, key, []byte("keep"), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	root := &DirectoryNode{fs: f.fs, path: ""}
	if errno := root.Unlink(ctx, key); errno != syscall.EROFS {
		t.Errorf("Unlink on a read-only mount returned %v, want EROFS", errno)
	}
	if !f.exists(t, key) {
		t.Error("the object was deleted on a read-only mount")
	}
}

// TestRmdirRemovesAnEmptyDirectory pins the ordinary case: the marker object goes.
func TestRmdirRemovesAnEmptyDirectory(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)
	ctx := context.Background()

	const marker = "emptydir/"
	if err := f.fs.backend.PutObject(ctx, marker, []byte{}, nil); err != nil {
		t.Fatalf("seed the directory marker: %v", err)
	}

	root := &DirectoryNode{fs: f.fs, path: ""}
	if errno := root.Rmdir(ctx, "emptydir"); errno != 0 {
		t.Fatalf("Rmdir of an empty directory returned %v, want success", errno)
	}

	if f.exists(t, marker) {
		t.Error("Rmdir reported success and the marker object is still present")
	}
}

// TestRmdirRefusesANonEmptyDirectory is the assertion that keeps rmdir from orphaning data.
//
// S3 has no directories, so removing the marker object of a prefix that still has objects under it
// would succeed at the storage layer and leave every one of those objects unreachable through the
// filesystem — present, billing, and invisible to ls. ENOTEMPTY is both what POSIX requires and the
// only answer that does not lose data.
func TestRmdirRefusesANonEmptyDirectory(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)
	ctx := context.Background()

	const (
		marker = "fulldir/"
		child  = "fulldir/keeper.txt"
	)
	if err := f.fs.backend.PutObject(ctx, marker, []byte{}, nil); err != nil {
		t.Fatalf("seed the marker: %v", err)
	}
	if err := f.fs.backend.PutObject(ctx, child, []byte("important"), nil); err != nil {
		t.Fatalf("seed the child: %v", err)
	}

	root := &DirectoryNode{fs: f.fs, path: ""}
	errno := root.Rmdir(ctx, "fulldir")

	if errno == 0 {
		t.Fatal("Rmdir of a non-empty directory reported success. Every object under the prefix is now " +
			"unreachable through the filesystem while still present in the bucket")
	}
	if errno != syscall.ENOTEMPTY {
		t.Errorf("Rmdir of a non-empty directory returned %v, want ENOTEMPTY", errno)
	}

	if !f.exists(t, child) {
		t.Error("the child object was deleted by a failed rmdir")
	}
	if !f.exists(t, marker) {
		t.Error("the marker was deleted despite the directory being non-empty")
	}
}

// TestRmdirDetectsAChildWhenTheMarkerIsListedFirst guards the emptiness check's own arithmetic.
//
// The marker object's key *is* the prefix, so it appears in its own listing. A check that asked for one
// entry and treated a non-empty result as "not empty" would refuse to remove any directory, and one
// that asked for one entry and ignored the marker would remove a directory whose only other object
// happened to sort second. This asks for two and compares keys, and this test is what fails if that
// changes: a directory with exactly one child, which is the boundary between the two mistakes.
func TestRmdirDetectsAChildWhenTheMarkerIsListedFirst(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)
	ctx := context.Background()

	// "aaa.txt" sorts after the marker key "onechild/", so the listing returns the marker first and the
	// child second — the case a limit of one would truncate away.
	const (
		marker = "onechild/"
		child  = "onechild/aaa.txt"
	)
	for _, k := range []string{marker, child} {
		if err := f.fs.backend.PutObject(ctx, k, []byte("x"), nil); err != nil {
			t.Fatalf("seed %q: %v", k, err)
		}
	}

	root := &DirectoryNode{fs: f.fs, path: ""}
	if errno := root.Rmdir(ctx, "onechild"); errno != syscall.ENOTEMPTY {
		t.Errorf("Rmdir returned %v for a directory with exactly one child, want ENOTEMPTY. A listing "+
			"limit that cannot see past the marker object reports this directory as empty", errno)
	}
	if !f.exists(t, child) {
		t.Error("the only child was deleted")
	}
}

// TestRmdirOfAMissingDirectoryReportsENOENT covers the prefix that never had a marker.
func TestRmdirOfAMissingDirectoryReportsENOENT(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)

	root := &DirectoryNode{fs: f.fs, path: ""}
	if errno := root.Rmdir(context.Background(), "nosuchdir"); errno != syscall.ENOENT {
		t.Errorf("Rmdir of a missing directory returned %v, want ENOENT", errno)
	}
}

// TestUnlinkInvalidatesTheCache pins that a deleted file stops being readable from cache.
//
// The cache observes nothing on its own, and its default TTL is five minutes. A delete that skipped
// invalidation would leave the file's bytes served from L1 for that long after the object was gone —
// which is the read-after-write class of defect the v0.10.x work was spent on, in the one direction
// where the stale answer is a file that does not exist.
func TestUnlinkInvalidatesTheCache(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)
	ctx := context.Background()

	const key = "cached.txt"
	body := []byte("cached contents")
	if err := f.fs.backend.PutObject(ctx, key, body, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Populate the cache the way a read would.
	f.fs.cache.Put(key, 0, body)

	root := &DirectoryNode{fs: f.fs, path: ""}
	if errno := root.Unlink(ctx, key); errno != 0 {
		t.Fatalf("Unlink: %v", errno)
	}

	if data := f.fs.cache.Get(key, 0, int64(len(body))); data != nil {
		t.Errorf("the cache still serves %d bytes for a deleted file; a read would return contents for "+
			"an object that no longer exists", len(data))
	}
}

// TestDeleteStatsAreRecorded pins that the Deletes counter moves.
//
// It existed as a field on FileSystemStats and was never incremented anywhere, which is its own small
// instance of the pattern this project keeps finding: a number reported to an operator that no code
// produces.
func TestDeleteStatsAreRecorded(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)
	ctx := context.Background()

	if err := f.fs.backend.PutObject(ctx, "a.txt", []byte("a"), nil); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := f.fs.backend.PutObject(ctx, "d/", []byte{}, nil); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	root := &DirectoryNode{fs: f.fs, path: ""}
	if errno := root.Unlink(ctx, "a.txt"); errno != 0 {
		t.Fatalf("Unlink: %v", errno)
	}
	if errno := root.Rmdir(ctx, "d"); errno != 0 {
		t.Fatalf("Rmdir: %v", errno)
	}

	f.fs.stats.mu.Lock()
	deletes := f.fs.stats.Deletes
	f.fs.stats.mu.Unlock()

	if deletes != 2 {
		t.Errorf("stats.Deletes = %d after one unlink and one rmdir, want 2", deletes)
	}
}

// TestUnlinkRmdirInterfacesStaySatisfied is the compile-time half, kept from the stub this file
// replaces.
//
// If DirectoryNode ever stops satisfying these interfaces, go-fuse does not call this package's code at
// all — it defaults both operations to success. That is a silent regression to the exact defect #163
// was filed for, and no runtime test in this file would catch it, because the methods would still exist
// and still work when called directly.
func TestUnlinkRmdirInterfacesStaySatisfied(t *testing.T) {
	t.Parallel()

	var _ gofuse.NodeUnlinker = (*DirectoryNode)(nil)
	var _ gofuse.NodeRmdirer = (*DirectoryNode)(nil)
}

// TestDiscardIsNotAFlush pins that Discard destroys pending writes rather than storing them.
//
// Unlink relies on this. If Discard flushed instead, deleting a file with pending writes would upload
// it first — paying for a PUT of an object about to be removed, and, if the upload failed, making the
// file undeletable.
func TestDiscardIsNotAFlush(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)

	const key = "discarded.txt"
	if err := f.fs.buffer.Write(key, 0, []byte("never stored")); err != nil {
		t.Fatalf("buffered write: %v", err)
	}

	if err := f.fs.buffer.Discard(key); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	if f.exists(t, key) {
		t.Error("Discard uploaded the object instead of dropping it")
	}
	if f.fs.buffer.Dirty(key) {
		t.Error("Discard left the key dirty")
	}
}

// TestDiscardRejectsAnEmptyKey keeps the misuse path from being silently accepted, and asserts the
// sentinel rather than just an error so the FUSE layer's EINVAL mapping holds.
func TestDiscardRejectsAnEmptyKey(t *testing.T) {
	t.Parallel()

	f := newDeleteFixture(t)

	err := f.fs.buffer.Discard("")
	if err == nil {
		t.Fatal("Discard(\"\") returned nil")
	}
	if !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("Discard(\"\") = %v, want an error satisfying errors.Is(err, vfs.ErrInvalid)", err)
	}
	if !strings.Contains(err.Error(), "empty key") {
		t.Errorf("Discard(\"\") = %v, which does not say what was wrong", err)
	}
}
