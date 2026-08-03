//go:build linux || darwin

package fuse

// Rename (#164) against a real S3 endpoint, a real write path, and a real cache — no mock on any seam,
// on the same reasoning delete_test.go gives.
//
// The reasoning is sharper here. A rename is a copy plus a delete, so a mock asked "was CopyObject
// called" confirms the implementation did what it was written to do while saying nothing about whether
// the file moved. Worse, the two failure modes this operation actually has are both invisible to such a
// mock: the destination can end up holding the *source's* bytes without its metadata, and the write
// path can flush pending ranges back to the source key after the delete, resurrecting the file at the
// name it was moved away from. Only storage state afterwards distinguishes those.

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	gofuse "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// renameFixture is a FileSystem wired to a real S3 endpoint, write path, and cache.
type renameFixture struct {
	fs  *FileSystem
	srv *testaws.TestServer
}

func newRenameFixture(t *testing.T) *renameFixture {
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

	filesystem := NewFileSystem(backend, byteCache, writer, nil, &Config{
		DefaultMode:    0o644,
		DefaultDirMode: 0o755,
		DefaultUID:     1000,
		DefaultGID:     1000,
	})

	return &renameFixture{fs: filesystem, srv: srv}
}

// root returns the fixture's root directory attached to a go-fuse bridge.
//
// The bridge is required rather than decorative: Rename calls Inode.GetChild and Inode.Children to
// repoint moved nodes, and both reach through the embedded Inode to the bridge that owns the tree. A
// bare &DirectoryNode{} has none.
func (f *renameFixture) root(t *testing.T) *DirectoryNode {
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

// bridge returns the RawFileSystem the kernel talks to, over this fixture's root.
//
// The repoint tests need this rather than the node methods, and the reason is the defect they cover.
// Registering a child in its parent is the *bridge's* job: rawBridge.Lookup calls addNewChild after
// DirectoryNode.Lookup returns (fs/bridge.go), so a direct call to DirectoryNode.Lookup hands back an
// inode that belongs to no parent — GetChild finds nothing and there is no node to go stale. Going
// through the bridge also runs Inode.MvChild on success, which is the mechanism that makes a stored path
// wrong in the first place.
func (f *renameFixture) bridge(t *testing.T) (fuse.RawFileSystem, *DirectoryNode) {
	t.Helper()

	root, ok := f.fs.Root().(*DirectoryNode)
	if !ok {
		t.Fatalf("FileSystem.Root returned %T, want *DirectoryNode", f.fs.Root())
	}

	timeout := f.fs.attrTimeout()
	raw := gofuse.NewNodeFS(root, &gofuse.Options{
		AttrTimeout:     &timeout,
		EntryTimeout:    &timeout,
		NullPermissions: true,
	})

	return raw, root
}

// rootNodeID is the root inode's nodeid, fixed at 1 by the FUSE protocol.
const rootNodeID = 1

// lookupThroughBridge resolves name under the directory at nodeID, the way the kernel does.
func lookupThroughBridge(t *testing.T, raw fuse.RawFileSystem, nodeID uint64, name string) {
	t.Helper()

	var out fuse.EntryOut
	header := fuse.InHeader{NodeId: nodeID}

	if status := raw.Lookup(nil, &header, name, &out); status != fuse.OK {
		t.Fatalf("Lookup(%q) under nodeid %d returned %v", name, nodeID, status)
	}
}

// renameThroughBridge issues a RENAME the way the kernel does, so that go-fuse's own MvChild runs.
func renameThroughBridge(t *testing.T, raw fuse.RawFileSystem, oldName, newName string) fuse.Status {
	t.Helper()

	in := &fuse.RenameIn{
		InHeader: fuse.InHeader{NodeId: rootNodeID},
		Newdir:   rootNodeID,
	}

	return raw.Rename(nil, in, oldName, newName)
}

// childOf returns the node the parent currently holds under name, or fails.
func childOf(t *testing.T, parent *gofuse.Inode, name string) any {
	t.Helper()

	child := parent.GetChild(name)
	if child == nil {
		t.Fatalf("the parent holds no child named %q; the lookup did not register it", name)
	}

	return child.Operations()
}

// exists asks the backend directly. The point of these tests is what storage holds, so the check must
// not run through a cache that could answer from before the rename.
func (f *renameFixture) exists(t *testing.T, key string) bool {
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

// TestRenameMovesTheObject is #164's headline assertion, and both halves are load-bearing: the
// destination holds the bytes *and* the source is gone. A copy that forgets to delete passes the first
// half alone, and `mv` that leaves the source behind is a `cp`.
func TestRenameMovesTheObject(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	ctx := context.Background()

	const (
		src  = "before.txt"
		dst  = "after.txt"
		body = "move me"
	)

	if err := f.fs.backend.PutObject(ctx, src, []byte(body), nil); err != nil {
		t.Fatalf("seed the object: %v", err)
	}

	root := f.root(t)
	if errno := root.Rename(ctx, src, root, dst, 0); errno != 0 {
		t.Fatalf("Rename returned %v, want success", errno)
	}

	if got := string(f.srv.GetObject(dst)); got != body {
		t.Errorf("the destination holds %q, want %q", got, body)
	}

	if f.exists(t, src) {
		t.Error("Rename reported success and the source object is still in the bucket, so this was a " +
			"copy rather than a move: the file now exists at both names and the old one keeps billing")
	}
}

// TestRenamePreservesMetadataAndEncoding is the assertion that makes a server-side copy mandatory
// rather than merely cheap.
//
// Three of these are not cosmetic. objectfs-mode and objectfs-uid are where POSIX mode and ownership
// live and there is nowhere else they are recorded, so losing them silently resets a file's permissions
// and owner. Content-Encoding is worse: the read path dispatches decoding on the stored value and fails
// closed on one it cannot handle, so a rename that dropped it leaves a compressed file permanently
// unreadable with every byte intact. That is audit finding L26, observed on the tier-transition path
// which uses the same CopyObject.
func TestRenamePreservesMetadataAndEncoding(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	ctx := context.Background()

	const (
		src = "encoded-before.dat"
		dst = "encoded-after.dat"
	)

	meta := map[string]string{
		"objectfs-mode":            "384",
		"objectfs-uid":             "1234",
		"objectfs-content-encoded": "zstd",
	}
	if err := f.fs.backend.PutObject(ctx, src, []byte("payload"), meta); err != nil {
		t.Fatalf("seed the object: %v", err)
	}

	before := f.srv.ObjectMetadata(src)

	root := f.root(t)
	if errno := root.Rename(ctx, src, root, dst, 0); errno != 0 {
		t.Fatalf("Rename returned %v, want success", errno)
	}

	after := f.srv.ObjectMetadata(dst)

	// Compared against what the source actually carried rather than against the map above, so the test
	// pins preservation across the copy rather than re-asserting what PutObject stores. Keys are matched
	// case-insensitively: real S3 lower-cases metadata keys and a Go http.Header round trip title-cases
	// them, so a case-sensitive comparison passes against one endpoint and fails against another.
	for k, want := range before {
		var got string
		var found bool
		for ak, av := range after {
			if strings.EqualFold(ak, k) {
				got, found = av, true

				break
			}
		}

		if !found || got != want {
			t.Errorf("the renamed object's %s = %q (present: %v), want %q. Mode and ownership live in "+
				"user metadata and nowhere else, and the read path fails closed on an encoding it cannot "+
				"identify — so a rename that drops these silently resets permissions or makes the file "+
				"unreadable with its bytes intact", k, got, found, want)
		}
	}
}

// TestRenameOverAnExistingFileReplacesIt pins POSIX's silent-replace rule.
//
// `mv a b` where b exists must replace b without asking, and must leave b holding a's bytes rather than
// its own. An implementation that refused would break every editor's save-to-temp-then-rename, and one
// that reported success while leaving b's old content would lose the write.
func TestRenameOverAnExistingFileReplacesIt(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	ctx := context.Background()

	const (
		src = "new-content.txt"
		dst = "existing.txt"
	)

	if err := f.fs.backend.PutObject(ctx, src, []byte("the replacement"), nil); err != nil {
		t.Fatalf("seed the source: %v", err)
	}
	if err := f.fs.backend.PutObject(ctx, dst, []byte("the original"), nil); err != nil {
		t.Fatalf("seed the destination: %v", err)
	}

	root := f.root(t)
	if errno := root.Rename(ctx, src, root, dst, 0); errno != 0 {
		t.Fatalf("Rename over an existing file returned %v, want success", errno)
	}

	if got := string(f.srv.GetObject(dst)); got != "the replacement" {
		t.Errorf("the destination holds %q after being renamed over, want %q; the replaced file's "+
			"content survived the write that was supposed to replace it", got, "the replacement")
	}

	if f.exists(t, src) {
		t.Error("the source survived a rename over an existing destination")
	}
}

// TestRenameFlushesPendingWritesSoTheCopySeesThem is the seam a mock cannot reach, and the one this
// operation is most likely to get wrong.
//
// `echo hi > a; mv a b` is an ordinary sequence and the kernel does not guarantee a flush between the
// two. A server-side copy acts on *objects*, so if the write path still holds a's bytes the copy either
// finds no object or copies a stale one — and then the delete removes the source and the pending ranges
// flush back to a. The file ends up at the name the user renamed away from, with b absent or stale, and
// every individual S3 call succeeded.
func TestRenameFlushesPendingWritesSoTheCopySeesThem(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	ctx := context.Background()

	const (
		src  = "dirty-before.txt"
		dst  = "dirty-after.txt"
		body = "written but never flushed"
	)

	if err := f.fs.buffer.Write(src, 0, []byte(body)); err != nil {
		t.Fatalf("buffered write: %v", err)
	}
	if f.exists(t, src) {
		t.Fatal("the fixture flushed; this test needs a file whose bytes are only in the write path")
	}

	root := f.root(t)
	if errno := root.Rename(ctx, src, root, dst, 0); errno != 0 {
		t.Fatalf("Rename of a file with only pending writes returned %v, want success", errno)
	}

	if got := string(f.srv.GetObject(dst)); got != body {
		t.Errorf("the destination holds %q, want %q. The rename copied an object that did not yet hold "+
			"the file's bytes, which is what happens when the write path is not flushed first", got, body)
	}

	// The decisive step. Flush everything, exactly as unmount does, and confirm the source did not come
	// back: pending ranges surviving the rename are PUT back to the old key, resurrecting the file at the
	// name it was moved away from with no error anywhere.
	if err := f.fs.buffer.FlushAll(); err != nil {
		t.Fatalf("FlushAll after the rename: %v", err)
	}

	if f.exists(t, src) {
		t.Error("flushing after the rename recreated the source object. The write path still held " +
			"pending state for the old key, so the moved-away-from name came back from the dead")
	}
}

// TestRenameMovesEveryObjectUnderADirectory covers the prefix case, including a nested level.
//
// The nesting is the part worth having: a single-level implementation that rebuilt the destination key
// from the entry name rather than from the prefix flattens the tree, moving dir/sub/deep.txt to
// newdir/deep.txt. Every object still exists, so a count-only assertion passes.
func TestRenameMovesEveryObjectUnderADirectory(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	ctx := context.Background()

	// No "project/" marker object here, so this directory is implicit — the shared prefix of its
	// contents, which is how a directory written by any other S3 tool looks. The marker case is
	// TestRenameMovesTheDirectoryMarkerToo, kept separate because the test endpoint cannot always
	// delete a marker; see there.
	seeded := map[string]string{
		"project/a.txt":         "alpha",
		"project/b.txt":         "beta",
		"project/sub/deep.txt":  "depth",
		"project/sub/other.txt": "other",
	}
	for key, body := range seeded {
		if err := f.fs.backend.PutObject(ctx, key, []byte(body), nil); err != nil {
			t.Fatalf("seed %q: %v", key, err)
		}
	}

	// A sibling whose name shares the source's prefix as a *string* but not as a path. If the prefix
	// match is a bare strings.HasPrefix, this file is swept into the rename — the same defect the cache's
	// keyMatches had.
	if err := f.fs.backend.PutObject(ctx, "project2/untouched.txt", []byte("leave me"), nil); err != nil {
		t.Fatalf("seed the sibling: %v", err)
	}

	root := f.root(t)
	if errno := root.Rename(ctx, "project", root, "archive", 0); errno != 0 {
		t.Fatalf("Rename of a directory returned %v, want success", errno)
	}

	want := map[string]string{
		"archive/a.txt":         "alpha",
		"archive/b.txt":         "beta",
		"archive/sub/deep.txt":  "depth",
		"archive/sub/other.txt": "other",
	}
	for key, body := range want {
		if !f.exists(t, key) {
			t.Errorf("%q does not exist after the directory rename; the tree was not moved intact "+
				"(a flattening bug moves sub/deep.txt to the top level, where every object still exists "+
				"and only its path is wrong)", key)

			continue
		}
		if got := string(f.srv.GetObject(key)); got != body {
			t.Errorf("%q holds %q, want %q", key, got, body)
		}
	}

	for key := range seeded {
		if f.exists(t, key) {
			t.Errorf("the source object %q survived the directory rename, so the tree now exists twice "+
				"and both copies bill", key)
		}
	}

	if got := string(f.srv.GetObject("project2/untouched.txt")); got != "leave me" {
		t.Errorf("project2/untouched.txt holds %q, want %q. Renaming \"project\" moved a sibling whose "+
			"name merely starts with the same characters, which is what a bare string-prefix match does",
			got, "leave me")
	}
}

// TestRenameMovesTheDirectoryMarkerToo covers the explicit-directory case: one created by Mkdir, which
// writes a zero-byte marker object at "prefix/".
//
// It is separate from the implicit case above because the marker is what makes an `ls` show an empty
// directory at all, and because the two are moved by the same loop for a non-obvious reason — the
// marker's key *is* the source prefix, so the listing includes it and no special case is needed. A
// change that started listing with a delimiter, or skipped zero-byte keys, would silently stop moving
// it: the files would arrive and the directory would vanish the moment it was emptied.
func TestRenameMovesTheDirectoryMarkerToo(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)

	// Deleting a marker is the half of this the endpoint may not support; see the skip's own comment.
	f.srv.RequireDirectoryMarkerDelete()

	ctx := context.Background()

	for _, key := range []string{"marked/", "marked/file.txt"} {
		if err := f.fs.backend.PutObject(ctx, key, []byte("x"), nil); err != nil {
			t.Fatalf("seed %q: %v", key, err)
		}
	}

	root := f.root(t)
	if errno := root.Rename(ctx, "marked", root, "renamed", 0); errno != 0 {
		t.Fatalf("Rename of a directory with a marker returned %v, want success", errno)
	}

	if !f.exists(t, "renamed/") {
		t.Error("the destination has no marker object. The source's marker is at the source prefix, so " +
			"the listing that drives the rename includes it and it moves with everything else — unless " +
			"the listing changed to use a delimiter or to skip zero-byte keys, in which case the files " +
			"arrive and the directory disappears as soon as it is emptied")
	}
	if !f.exists(t, "renamed/file.txt") {
		t.Error("the file under the renamed directory did not move")
	}

	if f.exists(t, "marked/") {
		t.Error("the source directory's marker survived, so an `ls` still shows the directory the " +
			"rename moved away from")
	}
	if f.exists(t, "marked/file.txt") {
		t.Error("the source file survived the directory rename")
	}
}

// TestRenameOfAMissingSourceReportsENOENT pins that absence is an error.
//
// `mv` distinguishes the two and POSIX requires it. Reporting success would also be actively unsafe
// here: the caller would believe the destination now holds a file that was never written.
func TestRenameOfAMissingSourceReportsENOENT(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	root := f.root(t)

	errno := root.Rename(context.Background(), "never-existed.txt", root, "wherever.txt", 0)

	if errno == 0 {
		t.Fatal("Rename of a missing source reported success")
	}
	if errno != syscall.ENOENT {
		t.Errorf("Rename of a missing source returned %v, want ENOENT", errno)
	}

	// The errno alone does not pin this, which is worth recording because it is not obvious. Removing
	// the existence check entirely still produces ENOENT: the copy is attempted, its own HEAD of the
	// source fails NotFound, and that maps to the same errno. So the assertion above passes against an
	// implementation that never checks.
	//
	// What separates them is whether the failure was *diagnosed* or merely propagated. A propagated one
	// counts as an error, while ENOENT for an absent source is an ordinary result — `mv nonexistent x`
	// is a user typo, not a filesystem fault — and a mount whose error counter climbs on every typo is
	// a mount whose error counter means nothing.
	if got := f.fs.GetStats().Renames; got != 0 {
		t.Errorf("a failed rename counted %d completed rename(s)", got)
	}
	if got := f.fs.GetStats().Errors; got != 0 {
		t.Errorf("Rename of a missing source recorded %d error(s). An absent source is a normal answer "+
			"to a normal question, so it must not be counted as a filesystem fault — and reaching this "+
			"means absence was discovered by a failing storage call rather than established up front",
			got)
	}
}

// TestRenameOnAReadOnlyMountReportsEROFS.
//
// The refusal must come before any storage call. A read-only mount that copied and then declined to
// delete would have modified the filesystem in the course of refusing to modify it.
func TestRenameOnAReadOnlyMountReportsEROFS(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	ctx := context.Background()

	const src = "ro-before.txt"
	if err := f.fs.backend.PutObject(ctx, src, []byte("immutable"), nil); err != nil {
		t.Fatalf("seed the object: %v", err)
	}

	f.fs.config.ReadOnly = true

	root := f.root(t)
	if errno := root.Rename(ctx, src, root, "ro-after.txt", 0); errno != syscall.EROFS {
		t.Errorf("Rename on a read-only mount returned %v, want EROFS", errno)
	}

	if f.exists(t, "ro-after.txt") {
		t.Error("a read-only mount wrote the destination while refusing the rename")
	}
	if !f.exists(t, src) {
		t.Error("a read-only mount deleted the source while refusing the rename")
	}
}

// TestRenameRefusesRenameat2Flags pins that RENAME_EXCHANGE and RENAME_NOREPLACE are declined rather
// than silently downgraded.
//
// Both are promises about atomicity that copy-then-delete cannot keep. Accepting the flag and doing the
// non-atomic thing is the failure this guards: RENAME_NOREPLACE means "fail if the destination exists",
// and a caller relying on it to claim a lock would silently overwrite the holder. EINVAL is what the
// kernel returns for an unsupported flag, and `mv` and Git both fall back on it.
func TestRenameRefusesRenameat2Flags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		flags uint32
		why   string
	}{
		{
			name:  "exchange",
			flags: renameExchange,
			why: "an atomic swap would be four operations here, with two windows in which a name holds " +
				"the wrong file or no file at all",
		},
		{
			name:  "noreplace",
			flags: renameNoReplace,
			why: "a caller using this to claim a name would silently overwrite the existing holder, " +
				"because the check and the copy cannot be one operation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newRenameFixture(t)
			ctx := context.Background()

			const src = "flagged.txt"
			if err := f.fs.backend.PutObject(ctx, src, []byte("content"), nil); err != nil {
				t.Fatalf("seed the object: %v", err)
			}

			root := f.root(t)
			if errno := root.Rename(ctx, src, root, "flagged-moved.txt", tt.flags); errno != syscall.EINVAL {
				t.Errorf("Rename with %s returned %v, want EINVAL: %s", tt.name, errno, tt.why)
			}

			if f.exists(t, "flagged-moved.txt") {
				t.Errorf("Rename with %s refused the request and moved the file anyway", tt.name)
			}
		})
	}
}

// TestRenameToTheSameNameIsANoOp.
//
// rename(2) of a name to itself returns success and changes nothing. The dangerous implementation is the
// obvious one: copy src to dst and delete src, where src == dst, deletes the file it just wrote.
func TestRenameToTheSameNameIsANoOp(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	ctx := context.Background()

	const key = "self.txt"
	if err := f.fs.backend.PutObject(ctx, key, []byte("unchanged"), nil); err != nil {
		t.Fatalf("seed the object: %v", err)
	}

	root := f.root(t)
	if errno := root.Rename(ctx, key, root, key, 0); errno != 0 {
		t.Fatalf("Rename of a name to itself returned %v, want success", errno)
	}

	if !f.exists(t, key) {
		t.Fatal("renaming a file to its own name deleted it. The copy wrote the key and the delete then " +
			"removed what it had just written, which is what happens when src == dst is not special-cased")
	}
	if got := string(f.srv.GetObject(key)); got != "unchanged" {
		t.Errorf("the file holds %q after being renamed to itself, want %q", got, "unchanged")
	}
}

// TestRenameRepointsTheMovedNodeSoWritesGoToTheNewKey is the go-fuse integration this operation needs
// and nothing else in the package does.
//
// After a Rename that returns 0, the bridge calls Inode.MvChild, which re-parents *the same* Inode under
// the new name — it does not rebuild the node and does not call back into this package. So the FileNode
// still holds the key it was constructed with, and every later operation on the moved dentry addresses
// the old key: a write to the new name flushes to the old one, recreating the source the rename deleted
// and leaving the destination as the user found it.
//
// Nothing fails visibly when this is wrong. Every S3 call succeeds; the wrong object is modified. That is
// why the assertion is on which key the bytes landed in.
func TestRenameRepointsTheMovedNodeSoWritesGoToTheNewKey(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	ctx := context.Background()

	const (
		src = "repoint-before.txt"
		dst = "repoint-after.txt"
	)

	if err := f.fs.backend.PutObject(ctx, src, []byte("original"), nil); err != nil {
		t.Fatalf("seed the object: %v", err)
	}

	raw, root := f.bridge(t)

	// Resolve the source the way the kernel does, so the inode is registered in its parent and survives
	// the rename. A direct DirectoryNode.Lookup would not register it — see [renameFixture.bridge] — and
	// then there is no node to go stale and this test cannot fail.
	lookupThroughBridge(t, raw, rootNodeID, src)

	if status := renameThroughBridge(t, raw, src, dst); status != fuse.OK {
		t.Fatalf("Rename returned %v, want OK", status)
	}

	// Fetched from the parent *after* the rename, under the new name: this is the node the kernel now
	// reaches by the destination path, which is the thing whose key has to be right. MvChild has already
	// re-parented it, so if it is a different object than the one looked up above, the premise of this
	// test is wrong and the assertion below is worth nothing.
	moved, ok := childOf(t, root.EmbeddedInode(), dst).(*FileNode)
	if !ok {
		t.Fatalf("the node under %q is a %T, want *FileNode", dst, childOf(t, root.EmbeddedInode(), dst))
	}

	if got := moved.key(); got != dst {
		t.Errorf("the moved node still addresses %q, want %q. go-fuse re-parents the existing inode "+
			"rather than rebuilding it, so a rename that does not repoint the node leaves every later "+
			"operation on the new name reading and writing the old key", got, dst)
	}

	// And the consequence, rather than only the field: write through the node the kernel now reaches by
	// the new name, and check which key the bytes reached.
	if err := f.fs.buffer.Write(moved.key(), 0, []byte("written after the rename")); err != nil {
		t.Fatalf("write after the rename: %v", err)
	}
	if err := f.fs.buffer.FlushAll(); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	if f.exists(t, src) {
		t.Error("a write through the moved node recreated the source object, so it flushed to the key " +
			"the file was renamed away from")
	}
	if got := string(f.srv.GetObject(dst)); !strings.HasPrefix(got, "written after the rename") {
		t.Errorf("the destination holds %q after a write through the moved node; the bytes went to some "+
			"other key", got)
	}
}

// TestRenameRepointsDescendantsOfAMovedDirectory is the recursive half of the same defect.
//
// A directory carries its whole resolved subtree with it, and the kernel keeps addressing those children
// through the inodes it already holds. Repointing only the directory leaves every child node naming a
// key under the old prefix — so a write to newdir/f flushes to olddir/f, which the rename has already
// deleted, recreating the source tree one file at a time.
func TestRenameRepointsDescendantsOfAMovedDirectory(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	ctx := context.Background()

	// An implicit directory, so this test does not depend on the endpoint being able to delete a marker
	// object; the marker case has its own test. What is under examination here is the node tree, not
	// the storage layout.
	if err := f.fs.backend.PutObject(ctx, "tree/leaf.txt", []byte("x"), nil); err != nil {
		t.Fatalf("seed the object: %v", err)
	}

	raw, root := f.bridge(t)

	// Resolve the directory and then the file beneath it, through the bridge, so both inodes are
	// registered in the tree — which is what makes them able to go stale.
	var dirEntry fuse.EntryOut
	dirHeader := fuse.InHeader{NodeId: rootNodeID}
	if status := raw.Lookup(nil, &dirHeader, "tree", &dirEntry); status != fuse.OK {
		t.Fatalf("Lookup(%q) returned %v", "tree", status)
	}

	lookupThroughBridge(t, raw, dirEntry.NodeId, "leaf.txt")

	if status := renameThroughBridge(t, raw, "tree", "moved"); status != fuse.OK {
		t.Fatalf("Rename of a directory returned %v, want OK", status)
	}

	movedDir, ok := childOf(t, root.EmbeddedInode(), "moved").(*DirectoryNode)
	if !ok {
		t.Fatalf("the node under %q is a %T, want *DirectoryNode",
			"moved", childOf(t, root.EmbeddedInode(), "moved"))
	}

	if got := movedDir.key(); got != "moved" {
		t.Errorf("the moved directory still addresses %q, want %q", got, "moved")
	}

	leafNode, ok := childOf(t, movedDir.EmbeddedInode(), "leaf.txt").(*FileNode)
	if !ok {
		t.Fatalf("the node under %q is a %T, want *FileNode",
			"moved/leaf.txt", childOf(t, movedDir.EmbeddedInode(), "leaf.txt"))
	}

	if got := leafNode.key(); got != "moved/leaf.txt" {
		t.Errorf("a child of the moved directory still addresses %q, want %q. The kernel reaches this "+
			"file through the inode it already holds, so a write to moved/leaf.txt would flush to the "+
			"deleted tree/leaf.txt and rebuild the source tree one file at a time",
			got, "moved/leaf.txt")
	}
}

// TestRenameReportsAFailedCopyAndLeavesTheSourceAlone is the ordering assertion, on the failure that
// matters most.
//
// Every source object is deleted only after its own copy has succeeded. That order is what makes an
// interrupted rename leave the data at the old name, the new name, or both — never at neither. The
// opposite order is not a hypothetical mistake: "delete the old entry, then write the new one" is how
// a rename reads if you think of it as moving a name rather than copying bytes, and it loses the file
// whenever the second step fails.
func TestRenameReportsAFailedCopyAndLeavesTheSourceAlone(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	ctx := context.Background()

	const (
		src  = "copyfail.txt"
		body = "must survive"
	)

	if err := f.fs.backend.PutObject(ctx, src, []byte(body), nil); err != nil {
		t.Fatalf("seed the object: %v", err)
	}

	// A destination key the endpoint will reject. An empty key is invalid for every S3 implementation, and
	// it fails at the copy rather than before it, which is the point in the sequence this test needs.
	root := f.root(t)
	errno := root.Rename(ctx, src, root, "", 0)

	if errno == 0 {
		t.Fatal("Rename with an unwritable destination reported success. `mv` would report that the file " +
			"had moved while it had not, and the user would believe the source is gone")
	}

	if !f.exists(t, src) {
		t.Error("a rename whose copy failed deleted the source anyway. The data is now at neither name, " +
			"which is the one outcome the copy-then-delete order exists to prevent")
	}
	if got := string(f.srv.GetObject(src)); got != body {
		t.Errorf("the source holds %q after a failed rename, want %q", got, body)
	}
}

// TestRenameIsRefusedWithoutAWritePath.
//
// A FileSystem built without a write path cannot flush the source before copying it, so it cannot know
// the copy will see the file's bytes. Refusing is the honest answer; proceeding would silently copy a
// stale or absent object.
func TestRenameIsRefusedWithoutAWritePath(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	f.fs.buffer = nil

	root := f.root(t)
	ctx := context.Background()

	if err := f.fs.backend.PutObject(ctx, "nobuffer.txt", []byte("x"), nil); err != nil {
		t.Fatalf("seed the object: %v", err)
	}

	if errno := root.Rename(ctx, "nobuffer.txt", root, "moved.txt", 0); errno != syscall.ENOTSUP {
		t.Errorf("Rename without a write path returned %v, want ENOTSUP", errno)
	}
}

// TestRenameToAForeignParentReportsEXDEV.
//
// newParent is an interface, so a node this package cannot compute a path for has to be refused rather
// than guessed at. EXDEV is "not the same filesystem", which is what a foreign node amounts to, and `mv`
// responds to it by falling back to copying the file's contents — which works.
func TestRenameToAForeignParentReportsEXDEV(t *testing.T) {
	t.Parallel()

	f := newRenameFixture(t)
	ctx := context.Background()

	if err := f.fs.backend.PutObject(ctx, "xdev.txt", []byte("x"), nil); err != nil {
		t.Fatalf("seed the object: %v", err)
	}

	root := f.root(t)

	// A go-fuse node that is not one of ours.
	foreign := &gofuse.Inode{}

	if errno := root.Rename(ctx, "xdev.txt", foreign, "elsewhere.txt", 0); errno != syscall.EXDEV {
		t.Errorf("Rename into a foreign parent returned %v, want EXDEV", errno)
	}

	if !f.exists(t, "xdev.txt") {
		t.Error("a refused cross-filesystem rename deleted the source")
	}
}

// The path-boundary rule that FlushPrefix and DiscardPrefix enforce is tested in internal/vfs, not
// here. It moved there when a coverage gate made the split visible: the functions live in that package,
// so a test in this one leaves them reading 0% covered no matter how thoroughly it exercises them —
// coverage is per-package and the profile follows the code, not the consequence.
//
// The move also bought two cases this fixture could not reach. Failing a flush needs a backend that
// rejects a PUT, and asserting that a rename must *not* proceed past that is the whole point of
// stopping at the first failure. And renaming a single file passes the file's own key where those
// methods take a prefix, so the key-equals-prefix arm is the common path rather than an edge case —
// invisible from a directory test, because a key beneath a directory satisfies the boundary rule too.
// See TestPrefixOperationsOnAnExactKeyTakeThatKey.
