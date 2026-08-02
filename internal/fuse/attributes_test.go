//go:build linux || darwin

package fuse

// Tests for the attribute half of the node contract, against a real S3 endpoint and the real write
// path. Nothing here is mocked, for the reason the package doc gives: every defect covered below is a
// value correctly produced at one layer and dropped at the boundary to the next, and a mock on the far
// side of that boundary agrees with its caller by construction.
//
// Assertions go through a fresh HeadObject, or through testaws.ObjectMetadata, rather than through the
// same in-memory state the operation just wrote. A chmod that updates the write path's attribute
// record and never reaches S3 passes every assertion that asks the write path what it thinks — and
// that is exactly the failure "chmod does not survive a remount" describes.

import (
	"context"
	iofs "io/fs"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// setAttrIn builds a SETATTR request carrying exactly the fields named in valid.
//
// The mask is the whole point of the request. Every field not named in Valid holds whatever was in the
// struct, so a Setattr that applied all of them unconditionally would have `touch` reset a file's mode
// to zero — which is why each test below sets one mask bit and asserts the other attributes survived.
func setAttrIn(valid uint32, mutate func(*fuse.SetAttrIn)) *fuse.SetAttrIn {
	in := &fuse.SetAttrIn{}
	in.Valid = valid
	if mutate != nil {
		mutate(in)
	}

	return in
}

// root returns the fixture's root directory node, attached to a go-fuse bridge.
//
// The bridge is required, not decorative: Lookup, Mkdir, and Create all call Inode.NewInode, which
// reaches through the embedded Inode to the bridge that owns the node tree. A bare &DirectoryNode{}
// has no bridge and panics on the first child it tries to create, so any test that resolves a name has
// to go through NewNodeFS.
//
// NullPermissions matches what mount.go sets in production, so nothing here is silently rescued by
// go-fuse's mode backstop — which is the backstop whose absence made every directory report 0000.
func (f *readPathFixture) root(t *testing.T) *DirectoryNode {
	t.Helper()

	root, ok := f.fs.Root().(*DirectoryNode)
	if !ok {
		t.Fatalf("FileSystem.Root returned %T, want *DirectoryNode", f.fs.Root())
	}

	timeout := f.fs.attrTimeout()
	_ = fs.NewNodeFS(root, &fs.Options{
		AttrTimeout:     &timeout,
		EntryTimeout:    &timeout,
		NullPermissions: true,
	})

	return root
}

// storedMode returns the mode S3 holds for a key, read back through a fresh HEAD.
func storedMode(t *testing.T, f *readPathFixture, key string) iofs.FileMode {
	t.Helper()

	meta := f.srv.ObjectMetadata(key)

	raw, ok := meta["objectfs-mode"]
	if !ok {
		t.Fatalf("object %q carries no objectfs-mode; metadata is %v", key, meta)
	}

	n, err := strconv.ParseUint(raw, 8, 32)
	if err != nil {
		t.Fatalf("objectfs-mode for %q is %q, which is not an octal mode: %v", key, raw, err)
	}

	return iofs.FileMode(n)
}

// TestDirectoryGetattrReportsATraversableMode is C1's user-visible symptom, at the layer that caused
// it.
//
// v0.10.0 had no DirectoryNode.Getattr at all. mount.go sets Options.NullPermissions, which disables
// go-fuse's mode backstop in rawBridge.setAttr, so the bridge answered with a zero-valued fuse.Attr:
// mode 0000, no type bits, no execute bit. A directory that cannot be traversed makes every path below
// it unreachable, so this one omission made the whole mount unusable for any user but root.
func TestDirectoryGetattrReportsATraversableMode(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	node := &DirectoryNode{fs: f.fs, path: "some/dir"}

	var out fuse.AttrOut
	if errno := node.Getattr(context.Background(), nil, &out); errno != 0 {
		t.Fatalf("Getattr on a directory: errno %v", errno)
	}

	if out.Mode&07777 == 0 {
		t.Fatalf("a directory reports mode %#o. Every path below it is unreachable for any non-root "+
			"user: this is the defect that made v0.10.0 unmountable in practice.", out.Mode&07777)
	}

	// Execute is the load-bearing bit. Read permission on a directory lists it; execute permission is
	// what allows resolving a name inside it, so a mode of 0444 is nearly as unusable as 0000.
	const anyExec = 0o111
	if out.Mode&anyExec == 0 {
		t.Errorf("a directory reports mode %#o, which has no execute bit for anyone, so it cannot be "+
			"traversed", out.Mode&07777)
	}

	if out.Mode&syscall.S_IFDIR == 0 {
		t.Errorf("a directory reports mode %#o, which lacks S_IFDIR: the kernel sees a file of no type",
			out.Mode)
	}

	if out.Nlink == 0 {
		t.Error("a directory reports nlink 0, which some tools read as an unlinked inode")
	}

	// A zero timeout tells the kernel not to cache the result, which costs a round trip per stat for
	// the life of the mount. rawBridge.getattr would supply one, but Setattr's path does not — so both
	// set it, and both are checked.
	if out.Timeout() == 0 {
		t.Error("Getattr returned a zero attribute timeout, so the kernel will not cache the result")
	}
}

// TestDirectoryGetattrHonorsConfiguredDirMode checks that the configured directory mode is what gets
// reported, since a directory has no object and therefore nothing else to read a mode from.
func TestDirectoryGetattrHonorsConfiguredDirMode(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.fs.config.DefaultDirMode = 0o750

	node := &DirectoryNode{fs: f.fs, path: "d"}

	var out fuse.AttrOut
	if errno := node.Getattr(context.Background(), nil, &out); errno != 0 {
		t.Fatalf("Getattr: errno %v", errno)
	}

	if got := out.Mode & 07777; got != 0o750 {
		t.Errorf("directory mode is %#o with DefaultDirMode=0750, want 0750", got)
	}

	// DefaultMode (files) must not be what a directory reports: 0644 on a directory has no execute
	// bit, which is how one config value serving both roles produces an untraversable mount.
	if got := out.Mode & 07777; got == f.fs.config.DefaultMode {
		t.Errorf("directory mode %#o equals the *file* default; the two must be separate settings", got)
	}
}

// TestDirectoryMtimeIsStable pins the synthetic directory time.
//
// A directory whose mtime advances on every stat defeats every make(1)-style timestamp comparison and
// makes rsync --times copy unchanged trees, so the value has to be fixed for the life of the process
// even though it is invented.
func TestDirectoryMtimeIsStable(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	node := &DirectoryNode{fs: f.fs, path: "d"}

	var first, second fuse.AttrOut
	if errno := node.Getattr(context.Background(), nil, &first); errno != 0 {
		t.Fatalf("first Getattr: errno %v", errno)
	}
	time.Sleep(2 * time.Millisecond)
	if errno := node.Getattr(context.Background(), nil, &second); errno != 0 {
		t.Fatalf("second Getattr: errno %v", errno)
	}

	if first.Mtime != second.Mtime || first.Mtimensec != second.Mtimensec {
		t.Errorf("a directory's mtime changed between two stats: %d.%09d then %d.%09d",
			first.Mtime, first.Mtimensec, second.Mtime, second.Mtimensec)
	}
}

// TestChmodPersistsToObjectMetadata is the whole point of Setattr.
//
// The read-back is a fresh HeadObject against the endpoint, not the write path's own record. A chmod
// that updates memory and never uploads passes any assertion that asks the write path what it
// believes, and "chmod does not survive a remount" is precisely that failure.
func TestChmodPersistsToObjectMetadata(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.srv.RequireMetadataReplace()
	f.srv.SeedRandom("chmod.dat", 4096)

	node := &FileNode{fs: f.fs, path: "chmod.dat"}

	in := setAttrIn(fuse.FATTR_MODE, func(in *fuse.SetAttrIn) { in.Mode = 0o600 })

	var out fuse.AttrOut
	if errno := node.Setattr(context.Background(), nil, in, &out); errno != 0 {
		t.Fatalf("chmod 0600: errno %v", errno)
	}

	if got := out.Mode & 07777; got != 0o600 {
		t.Errorf("Setattr reported mode %#o, want 0600", got)
	}

	if got := storedMode(t, f, "chmod.dat"); got != 0o600 {
		t.Errorf("S3 holds objectfs-mode %#o after chmod 0600, want 0600. A chmod that returns success "+
			"without reaching storage is indistinguishable from one that was never issued.", got)
	}

	// And a reader that has never seen this filesystem's memory must see the new mode. This is the
	// remount case: a fresh FileNode over a fresh write path, reading only what S3 holds.
	fresh := newWriterOver(t, f)
	f.fs.buffer = fresh
	f.fs.invalidate("chmod.dat")

	var reread fuse.AttrOut
	if errno := node.Getattr(context.Background(), nil, &reread); errno != 0 {
		t.Fatalf("Getattr after replacing the write path: errno %v", errno)
	}
	if got := reread.Mode & 07777; got != 0o600 {
		t.Errorf("after discarding all in-memory state, the file reports mode %#o, want 0600", got)
	}
}

// TestAttributesPersistAlongsideAWrite is the same durability property as
// [TestChmodPersistsToObjectMetadata], reached by the other of the two write paths.
//
// A node with content pending sends its attributes as the PUT's user metadata — one request, and no
// window in which the object exists with the old mode. A node with *only* attributes pending takes
// SetObjectMetadata instead, because re-uploading a multi-gigabyte object to change nine permission
// bits is not a chmod anyone can use.
//
// This one is not gated on RequireMetadataReplace: it goes through PutObject, so it runs against any
// endpoint. That independence is worth keeping now that substrate honors the directive — attribute
// durability is the property this whole task exists for, and it should not become unobservable again
// because of a dependency pin.
func TestAttributesPersistAlongsideAWrite(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.srv.SeedRandom("both.dat", 512)

	node := &FileNode{fs: f.fs, path: "both.dat"}
	ctx := context.Background()

	// A write and a chmod on the same node, flushed together. Setattr's own flush is what sends them,
	// and because there is content pending the plan is not a Noop, so this is the PUT path.
	if err := f.fs.buffer.Write("both.dat", 0, []byte("REPLACED")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	in := setAttrIn(fuse.FATTR_MODE|fuse.FATTR_UID|fuse.FATTR_GID, func(in *fuse.SetAttrIn) {
		in.Mode, in.Uid, in.Gid = 0o640, 4242, 4343
	})
	if errno := node.Setattr(ctx, nil, in, &fuse.AttrOut{}); errno != 0 {
		t.Fatalf("chmod+chown with a write pending: errno %v", errno)
	}

	// Every one of the three has to be in S3, not just the mode: the PUT replaces user metadata
	// wholesale, so a path that carried the mode forward and dropped the ownership would chown the file
	// to root on the next ordinary write.
	meta := f.srv.ObjectMetadata("both.dat")
	for _, want := range []struct{ key, value string }{
		{"objectfs-mode", "640"},
		{"objectfs-uid", "4242"},
		{"objectfs-gid", "4343"},
	} {
		if got := meta[want.key]; got != want.value {
			t.Errorf("S3 holds %s=%q, want %q. Metadata: %v", want.key, got, want.value, meta)
		}
	}

	// And the bytes went up too. An attribute write that dropped the content would be the more obvious
	// half of the same defect.
	stored, err := f.fs.backend.GetObject(ctx, "both.dat", 0, 8)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if string(stored) != "REPLACED" {
		t.Errorf("object begins %q, want %q", stored, "REPLACED")
	}

	// The remount case: nothing in memory, only what storage holds.
	f.fs.buffer = newWriterOver(t, f)
	f.fs.invalidate("both.dat")

	var reread fuse.AttrOut
	if errno := node.Getattr(ctx, nil, &reread); errno != 0 {
		t.Fatalf("Getattr after replacing the write path: errno %v", errno)
	}
	if got := reread.Mode & 07777; got != 0o640 {
		t.Errorf("after discarding all in-memory state the file reports mode %#o, want 0640", got)
	}
	if reread.Uid != 4242 || reread.Gid != 4343 {
		t.Errorf("after discarding all in-memory state the file reports owner %d:%d, want 4242:4343",
			reread.Uid, reread.Gid)
	}
}

// TestAttributeFlushThatStoresNothingIsReported is the integrity check on the metadata-replace path,
// checked from the FUSE layer.
//
// S3 has no metadata-update operation: changing an object's user metadata is a self-copy with
// MetadataDirective=REPLACE, and an endpoint that does not implement the directive answers 200 while
// carrying the old metadata forward. This test is the inverse of the three that skip above — it runs
// *only* on such an endpoint, and asserts the failure is loud. A chmod that reports success while
// storing nothing is a mode that silently does not survive a remount, which is the defect class this
// task exists to remove rather than relocate.
func TestAttributeFlushThatStoresNothingIsReported(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	if f.srv.Capabilities().MetadataReplace {
		t.Skip("this endpoint honors MetadataDirective=REPLACE, so there is no silent discard to detect; " +
			"TestChmodPersistsToObjectMetadata covers the success path")
	}

	f.srv.SeedRandom("silent.dat", 512)
	node := &FileNode{fs: f.fs, path: "silent.dat"}

	in := setAttrIn(fuse.FATTR_MODE, func(in *fuse.SetAttrIn) { in.Mode = 0o600 })

	errno := node.Setattr(context.Background(), nil, in, &fuse.AttrOut{})
	if errno == 0 {
		t.Fatal("chmod reported success against an endpoint that stored nothing. The mode will not " +
			"survive a remount and nothing said so.")
	}
	if errno != syscall.EIO {
		t.Errorf("chmod returned errno %v, want EIO: the change did not happen and the cause is not "+
			"something a caller can act on more specifically", errno)
	}
}

// TestChmodDoesNotClobberOwnershipOrSize is the mask, checked in the direction that loses data.
//
// A SETATTR carrying only FATTR_MODE leaves Uid, Gid, and Size holding whatever was in the struct —
// zero, in practice. Applying them anyway turns every chmod into a chown to root and a truncation to
// nothing.
func TestChmodDoesNotClobberOwnershipOrSize(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.srv.RequireMetadataReplace()
	original := f.srv.SeedRandom("mask.dat", 8192)

	node := &FileNode{fs: f.fs, path: "mask.dat"}
	ctx := context.Background()

	// Give the object a non-default owner first, so a clobber to zero is visible as a change rather
	// than as the value it already had.
	own := setAttrIn(fuse.FATTR_UID|fuse.FATTR_GID, func(in *fuse.SetAttrIn) {
		in.Uid, in.Gid = 4242, 4343
	})
	if errno := node.Setattr(ctx, nil, own, &fuse.AttrOut{}); errno != 0 {
		t.Fatalf("chown: errno %v", errno)
	}

	mode := setAttrIn(fuse.FATTR_MODE, func(in *fuse.SetAttrIn) { in.Mode = 0o640 })

	var out fuse.AttrOut
	if errno := node.Setattr(ctx, nil, mode, &out); errno != 0 {
		t.Fatalf("chmod: errno %v", errno)
	}

	if out.Uid != 4242 || out.Gid != 4343 {
		t.Errorf("a mode-only SETATTR changed ownership to %d:%d, want 4242:4343. The unset fields of "+
			"the request were applied as though the caller had set them.", out.Uid, out.Gid)
	}

	if out.Size != uint64(len(original)) {
		t.Errorf("a mode-only SETATTR changed the size to %d, want %d — the request's zero Size field "+
			"was applied as a truncation", out.Size, len(original))
	}

	if got := f.srv.ObjectSize("mask.dat"); got != int64(len(original)) {
		t.Errorf("the stored object is %d bytes after a chmod, want %d", got, len(original))
	}
}

// TestSetattrSizeTruncates is O_TRUNC as the kernel actually delivers it.
//
// CAP_ATOMIC_O_TRUNC is never negotiated, so `> file` arrives as a SETATTR carrying FATTR_SIZE=0
// before the open completes rather than as an O_TRUNC flag on the open. v0.10.0 implemented neither,
// so shell redirection could not shorten an object.
func TestSetattrSizeTruncates(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.srv.SeedRandom("trunc.dat", 16384)

	node := &FileNode{fs: f.fs, path: "trunc.dat"}

	in := setAttrIn(fuse.FATTR_SIZE, func(in *fuse.SetAttrIn) { in.Size = 0 })

	var out fuse.AttrOut
	if errno := node.Setattr(context.Background(), nil, in, &out); errno != 0 {
		t.Fatalf("truncate to 0: errno %v", errno)
	}

	if out.Size != 0 {
		t.Errorf("Setattr reported size %d after truncating to 0", out.Size)
	}

	if got := f.srv.ObjectSize("trunc.dat"); got != 0 {
		t.Errorf("the stored object is %d bytes after truncate(0), want 0. A truncate that returns "+
			"success without shortening the object is the same defect as an rm that does not delete.",
			got)
	}
}

// TestSetattrSizeExtends checks the other direction: a truncate that grows a file must zero-fill,
// because that is what POSIX promises and what every caller that preallocates depends on.
func TestSetattrSizeExtends(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	original := f.srv.SeedRandom("extend.dat", 1024)

	node := &FileNode{fs: f.fs, path: "extend.dat"}

	const want = 4096
	in := setAttrIn(fuse.FATTR_SIZE, func(in *fuse.SetAttrIn) { in.Size = want })

	var out fuse.AttrOut
	if errno := node.Setattr(context.Background(), nil, in, &out); errno != 0 {
		t.Fatalf("truncate to %d: errno %v", want, errno)
	}

	if out.Size != want {
		t.Errorf("Setattr reported size %d, want %d", out.Size, want)
	}

	// Read through the ObjectFS backend, not f.srv.GetObject. The fixture's backend has compression on
	// above 4 KiB, and f.srv is a raw SDK client: it would return the zstd frame, so a correct
	// zero-extension would fail this assertion as a 1058-byte object. What the test is asserting is the
	// file's contents, which is what a caller reads, and that is the decoded payload.
	stored, err := f.fs.backend.GetObject(context.Background(), "extend.dat", 0, want)
	if err != nil {
		t.Fatalf("GetObject after extending: %v", err)
	}
	if len(stored) != want {
		t.Fatalf("the stored object reads back as %d bytes, want %d", len(stored), want)
	}

	// And the *stored* length is asserted separately, since it is the number HeadObject reports and the
	// one the kernel clamps reads to. A compressed object records objectfs-original-size, so this is the
	// backend's own view rather than the raw ContentLength.
	info, err := f.fs.backend.HeadObject(context.Background(), "extend.dat")
	if err != nil {
		t.Fatalf("HeadObject after extending: %v", err)
	}
	if info.Size != want {
		t.Errorf("HeadObject reports size %d after extending to %d; the kernel would clamp reads there",
			info.Size, want)
	}
	if string(stored[:len(original)]) != string(original) {
		t.Error("extending the file changed the bytes that were already there")
	}
	for i := len(original); i < want; i++ {
		if stored[i] != 0 {
			t.Fatalf("byte %d of the extended region is %#x, want 0", i, stored[i])
		}
	}
}

// TestSetattrRefusesSpecialModeBits pins the one thing Setattr will not do.
//
// vfs.Attr.Mode holds permission bits only, so setuid, setgid, and sticky have nowhere to be stored.
// Accepting them would report a change the next stat contradicts, and in the setuid case would appear
// to promise an escalation this filesystem cannot perform — access here is decided by the S3
// credentials the process holds, not by a mode bit.
func TestSetattrRefusesSpecialModeBits(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.srv.SeedRandom("suid.dat", 512)

	node := &FileNode{fs: f.fs, path: "suid.dat"}

	for name, mode := range map[string]uint32{
		"setuid": syscall.S_ISUID | 0o755,
		"setgid": syscall.S_ISGID | 0o755,
		"sticky": syscall.S_ISVTX | 0o755,
	} {
		in := setAttrIn(fuse.FATTR_MODE, func(in *fuse.SetAttrIn) { in.Mode = mode })

		errno := node.Setattr(context.Background(), nil, in, &fuse.AttrOut{})
		if errno != syscall.ENOTSUP {
			t.Errorf("chmod %s (%#o): errno %v, want ENOTSUP. Reporting success would make the bit look "+
				"stored when nothing can store it.", name, mode, errno)
		}
	}
}

// TestUtimesPersistsMtime covers `touch -d`, which cp -p, rsync --times, tar -x, and make all depend
// on.
func TestUtimesPersistsMtime(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.srv.RequireMetadataReplace()
	f.srv.SeedRandom("times.dat", 2048)

	node := &FileNode{fs: f.fs, path: "times.dat"}

	want := time.Date(2019, 3, 14, 15, 9, 26, 535000000, time.UTC)
	in := setAttrIn(fuse.FATTR_MTIME, func(in *fuse.SetAttrIn) {
		in.Mtime = uint64(want.Unix())
		in.Mtimensec = uint32(want.Nanosecond())
	})

	var out fuse.AttrOut
	if errno := node.Setattr(context.Background(), nil, in, &out); errno != 0 {
		t.Fatalf("utimes: errno %v", errno)
	}

	if got := time.Unix(int64(out.Mtime), int64(out.Mtimensec)); !got.Equal(want) {
		t.Errorf("Setattr reported mtime %v, want %v", got.UTC(), want)
	}

	// The nanosecond half is checked deliberately. fuse.Attr keeps whole seconds and nanoseconds in
	// separate fields, so assigning out.Mtime alone — which v0.10.0 did — leaves Mtimensec holding a
	// value from an unrelated stat.
	if out.Mtimensec != uint32(want.Nanosecond()) {
		t.Errorf("mtime nanoseconds are %d, want %d: the second and nanosecond halves were set "+
			"independently", out.Mtimensec, want.Nanosecond())
	}

	meta := f.srv.ObjectMetadata("times.dat")
	stored, ok := meta["objectfs-mtime"]
	if !ok {
		t.Fatalf("object carries no objectfs-mtime after utimes; metadata is %v", meta)
	}
	got, err := time.Parse(time.RFC3339Nano, stored)
	if err != nil {
		t.Fatalf("objectfs-mtime is %q, which is not RFC 3339: %v", stored, err)
	}
	if !got.Equal(want) {
		t.Errorf("S3 holds mtime %v, want %v", got.UTC(), want)
	}
}

// TestSetattrAtimeOnlyIsAcceptedAndStoresNothing.
//
// `touch -a` and every read on a relatime mount produce this request. Persisting it would mean a
// metadata rewrite per read; failing it would break utimensat for a value POSIX already lets a
// filesystem keep only approximately. So it succeeds and writes nothing — and the check that matters
// is that it did not rewrite the object.
func TestSetattrAtimeOnlyIsAcceptedAndStoresNothing(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.srv.SeedRandom("atime.dat", 1024)

	node := &FileNode{fs: f.fs, path: "atime.dat"}

	before := f.srv.Operations("PutObject")

	in := setAttrIn(fuse.FATTR_ATIME, func(in *fuse.SetAttrIn) {
		in.Atime = uint64(time.Now().Unix())
	})

	if errno := node.Setattr(context.Background(), nil, in, &fuse.AttrOut{}); errno != 0 {
		t.Fatalf("touch -a: errno %v, want success", errno)
	}

	if after := f.srv.Operations("PutObject"); after != before {
		t.Errorf("an atime-only SETATTR issued %d PutObject calls; on a relatime mount that is one "+
			"object rewrite per read", after-before)
	}
}

// TestSetattrOnAReadOnlyMountFails. A read-only mount that accepts a chmod is a read-only mount in
// name only.
func TestSetattrOnAReadOnlyMountFails(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.srv.SeedRandom("ro.dat", 512)
	f.fs.config.ReadOnly = true

	file := &FileNode{fs: f.fs, path: "ro.dat"}
	in := setAttrIn(fuse.FATTR_MODE, func(in *fuse.SetAttrIn) { in.Mode = 0o600 })

	if errno := file.Setattr(context.Background(), nil, in, &fuse.AttrOut{}); errno != syscall.EROFS {
		t.Errorf("chmod on a read-only mount: errno %v, want EROFS", errno)
	}

	dir := &DirectoryNode{fs: f.fs, path: "d"}
	if errno := dir.Setattr(context.Background(), nil, in, &fuse.AttrOut{}); errno != syscall.EROFS {
		t.Errorf("chmod of a directory on a read-only mount: errno %v, want EROFS", errno)
	}
}

// TestDirectorySetattrRefusesModeAndAcceptsTimes pins the deliberate asymmetry.
//
// A chmod is refused because a directory reports the configured default whatever is stored, so
// success would be contradicted by the next stat. utimes is accepted as a no-op because a directory
// that exists only as a shared prefix has no object to hold a time — and failing it would make every
// `tar -x`, `cp -a`, and `rsync -a` report errors on every directory.
func TestDirectorySetattrRefusesModeAndAcceptsTimes(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	node := &DirectoryNode{fs: f.fs, path: "d"}
	ctx := context.Background()

	refused := map[string]uint32{
		"chmod":       fuse.FATTR_MODE,
		"chown uid":   fuse.FATTR_UID,
		"chown gid":   fuse.FATTR_GID,
		"chmod+chown": fuse.FATTR_MODE | fuse.FATTR_UID | fuse.FATTR_GID,
	}
	for name, valid := range refused {
		in := setAttrIn(valid, func(in *fuse.SetAttrIn) { in.Mode = 0o700 })
		if errno := node.Setattr(ctx, nil, in, &fuse.AttrOut{}); errno != syscall.ENOTSUP {
			t.Errorf("%s of a directory: errno %v, want ENOTSUP", name, errno)
		}
	}

	in := setAttrIn(fuse.FATTR_MTIME, func(in *fuse.SetAttrIn) {
		in.Mtime = uint64(time.Now().Unix())
	})

	var out fuse.AttrOut
	if errno := node.Setattr(ctx, nil, in, &out); errno != 0 {
		t.Fatalf("utimes on a directory: errno %v, want success — failing it makes tar -x and cp -a "+
			"report an error for every directory", errno)
	}
	if out.Mode&07777 == 0 {
		t.Error("a directory's Setattr returned mode 0000; it must report the attributes it would " +
			"report from Getattr, because rawBridge.SetAttr applies no backstop")
	}
	if out.Timeout() == 0 {
		t.Error("a directory's Setattr returned a zero attribute timeout. rawBridge.SetAttr runs " +
			"neither setAttr nor setAttrTimeout, so a zero here means one round trip per stat forever")
	}
}

// TestFsyncFlushesPendingWrites is the durability contract fsync(2) states.
//
// v0.10.0 implemented no Fsync at all, so rawBridge returned ENOTSUP while
// docs/architecture/overview.md claimed "fsync() guarantees data is in S3 before returning". The
// assertion is on the object in the bucket, because that is what the claim is about.
func TestFsyncFlushesPendingWrites(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	original := f.srv.SeedRandom("fsync.dat", 1024)

	node := &FileNode{fs: f.fs, path: "fsync.dat"}
	fh := f.open(t, "fsync.dat")
	ctx := context.Background()

	appended := []byte("DURABLE")
	if _, errno := fh.Write(ctx, appended, int64(len(original))); errno != 0 {
		t.Fatalf("Write: errno %v", errno)
	}

	// Before the fsync the object must still be its old self: a write path that uploaded eagerly would
	// make this test pass without an Fsync existing at all.
	if got := f.srv.ObjectSize("fsync.dat"); got != int64(len(original)) {
		t.Fatalf("the object grew to %d bytes before fsync, so this test cannot tell whether Fsync "+
			"did anything", got)
	}

	if errno := node.Fsync(ctx, fh, 0); errno != 0 {
		t.Fatalf("Fsync: errno %v", errno)
	}

	want := append(append([]byte{}, original...), appended...)
	if got := f.srv.GetObject("fsync.dat"); string(got) != string(want) {
		t.Errorf("after fsync the object is %d bytes, want %d — fsync returned success before the "+
			"bytes were durable", len(got), len(want))
	}

	if f.fs.buffer.Dirty("fsync.dat") {
		t.Error("the write path still reports pending writes after a successful fsync")
	}
}

// TestFsyncOnADirectorySucceeds. A directory is a key prefix holding no state of its own, so there is
// nothing to make durable. Databases and version control systems fsync a directory to force a rename
// or create to disk; both already complete synchronously here, so success is accurate rather than
// convenient.
func TestFsyncOnADirectorySucceeds(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	node := &DirectoryNode{fs: f.fs, path: "d"}

	if errno := node.Fsync(context.Background(), nil, 0); errno != 0 {
		t.Errorf("Fsync on a directory: errno %v, want success", errno)
	}
}

// TestStatfsReportsUsableNumbers.
//
// Without a NodeStatfser, rawBridge.StatFs leaves the StatfsOut entirely zeroed and returns OK — and
// go-fuse's own documentation for that interface says an OSX filesystem must implement Statfs or the
// mount will not work. A zero block size is not a number df or the macOS VFS can do anything with.
func TestStatfsReportsUsableNumbers(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	ctx := context.Background()

	for name, statfs := range map[string]func(*fuse.StatfsOut) syscall.Errno{
		"file": func(out *fuse.StatfsOut) syscall.Errno {
			return (&FileNode{fs: f.fs, path: "x"}).Statfs(ctx, out)
		},
		"directory": func(out *fuse.StatfsOut) syscall.Errno {
			return (&DirectoryNode{fs: f.fs, path: "d"}).Statfs(ctx, out)
		},
	} {
		var out fuse.StatfsOut
		if errno := statfs(&out); errno != 0 {
			t.Fatalf("Statfs on a %s: errno %v", name, errno)
		}

		if out.Bsize == 0 {
			t.Errorf("Statfs on a %s reports block size 0; df divides by this", name)
		}
		if out.Blocks == 0 {
			t.Errorf("Statfs on a %s reports 0 total blocks, which reads as a zero-capacity "+
				"filesystem", name)
		}
		if out.Bavail == 0 {
			t.Errorf("Statfs on a %s reports 0 available blocks; install(1), dd, and most package "+
				"managers refuse to write to a filesystem that says it is full", name)
		}
		if out.NameLen == 0 {
			t.Errorf("Statfs on a %s reports a maximum name length of 0", name)
		}
	}
}

// TestFileGetattrDoesNotSetIno.
//
// rawBridge.getattr overwrites out.Ino with the inode's own stable Ino and logs a warning first if the
// two disagree, so writing one here produces a warning per stat and changes nothing.
func TestFileGetattrDoesNotSetIno(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.srv.SeedRandom("ino.dat", 128)

	node := &FileNode{fs: f.fs, path: "ino.dat"}

	var out fuse.AttrOut
	if errno := node.Getattr(context.Background(), nil, &out); errno != 0 {
		t.Fatalf("Getattr: errno %v", errno)
	}

	if out.Ino != 0 {
		t.Errorf("Getattr set Ino to %d. The bridge overwrites it from the inode's StableAttr and "+
			"logs a warning when the two disagree, so this is a warning per stat and nothing else",
			out.Ino)
	}
}

// TestGetattrOfAMissingFileIsENOENT, and — the half that matters — a backend failure that is not
// absence is not ENOENT. See toErrno: EIO fails, ENOENT invites an overwrite.
func TestGetattrOfAMissingFileIsENOENT(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	node := &FileNode{fs: f.fs, path: "nothing-here.dat"}

	var out fuse.AttrOut
	if errno := node.Getattr(context.Background(), nil, &out); errno != syscall.ENOENT {
		t.Errorf("Getattr of an absent object: errno %v, want ENOENT", errno)
	}
}

// TestBlocksAndBlksizeAreReported.
//
// go-fuse's setBlocks is a no-op on darwin and on linux derives Blocks from Size at 4096 — a second,
// independent computation that would disagree with vfs.Attr.Blocks the moment either changed. Blocks
// is in 512-byte units because POSIX fixes st_blocks at 512 and du and tar depend on it.
func TestBlocksAndBlksizeAreReported(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	const size = 5000
	f.srv.SeedRandom("blocks.dat", size)

	node := &FileNode{fs: f.fs, path: "blocks.dat"}

	var out fuse.AttrOut
	if errno := node.Getattr(context.Background(), nil, &out); errno != 0 {
		t.Fatalf("Getattr: errno %v", errno)
	}

	if want := uint64((size + 511) / 512); out.Blocks != want {
		t.Errorf("a %d-byte file reports %d blocks, want %d (512-byte units, rounded up)",
			size, out.Blocks, want)
	}
	if out.Blksize == 0 {
		t.Error("Blksize is 0; callers use it to size their I/O buffers")
	}
}

// TestFileGetattrPrefersTheWritePath is the other half of read-after-write: a chmod still only in
// memory must be what stat reports, or a caller sees its own change vanish and reappear.
func TestFileGetattrPrefersTheWritePath(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.srv.SeedRandom("pending.dat", 256)

	ctx := context.Background()
	node := &FileNode{fs: f.fs, path: "pending.dat"}

	// A mode change recorded in the write path and deliberately not flushed.
	if err := f.fs.buffer.SetAttr(ctx, "pending.dat", true, false, false,
		vfs.Attr{Mode: 0o604}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	var out fuse.AttrOut
	if errno := node.Getattr(ctx, nil, &out); errno != 0 {
		t.Fatalf("Getattr: errno %v", errno)
	}

	if got := out.Mode & 07777; got != 0o604 {
		t.Errorf("stat reports mode %#o while the write path holds an unflushed 0604; a caller sees "+
			"its own chmod disappear until the flush", got)
	}
}

// TestCreateOwnsTheFileAndDoesNotPut is the audit's worst data-loss path, from the other side.
//
// v0.10.0's Create opened with an unconditional PutObject of zero bytes. Composed with a Lookup that
// reported every HeadObject failure as ENOENT, a throttled stat of an existing file made the kernel
// believe the file was absent, and the create that followed replaced it with nothing.
func TestCreateOwnsTheFileAndDoesNotPut(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	root := f.root(t)
	ctx := context.Background()

	// An object that already exists at the path being created. A Create that PUTs would zero it.
	original := f.srv.SeedRandom("victim.dat", 4096)

	var out fuse.EntryOut
	node, fh, _, errno := root.Create(ctx, "victim.dat", uint32(syscall.O_RDWR), 0o640, &out)
	if errno != 0 {
		t.Fatalf("Create: errno %v", errno)
	}
	if node == nil || fh == nil {
		t.Fatal("Create returned a nil inode or handle with errno 0")
	}

	if got := f.srv.ObjectSize("victim.dat"); got != int64(len(original)) {
		t.Errorf("the object is %d bytes immediately after Create, want %d. Create PUT over an "+
			"existing object, which composed with a misclassified Lookup is the worst data-loss path "+
			"in the audit.", got, len(original))
	}

	if got := out.Mode & 07777; got != 0o640 {
		t.Errorf("Create filled the entry with mode %#o, want the requested 0640", got)
	}
	if out.Mode&syscall.S_IFREG == 0 {
		t.Errorf("Create filled the entry with mode %#o, which lacks S_IFREG", out.Mode)
	}

	// NodeId and Ino must be left alone: Inode.setEntryOut fills both from the inode's StableAttr.
	if out.NodeId != 0 {
		t.Errorf("Create set NodeId to %d; the bridge assigns it", out.NodeId)
	}
}

// TestCreateWithNoModeFallsBackToTheDefault. CreateIn.Mode arrives with the umask already applied, so
// a zero here means the caller asked for nothing — a file with mode 0000 is not a usable answer.
func TestCreateWithNoModeFallsBackToTheDefault(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	root := f.root(t)

	var out fuse.EntryOut
	if _, _, _, errno := root.Create(context.Background(), "nomode.dat", 0, 0, &out); errno != 0 {
		t.Fatalf("Create: errno %v", errno)
	}

	if out.Mode&07777 == 0 {
		t.Error("Create with mode 0 produced a file with mode 0000, which nothing can open")
	}
}

// TestReaddirEnumeratesEverything is the pagination cap.
//
// v0.10.0 passed a limit of 1000 with the comment "List up to 1000 objects". A truncated listing is
// not a display problem: the entries past the cap do not exist as far as any caller is concerned, so
// `rm -rf` reports success having deleted a fraction and `du` understates a dataset.
func TestReaddirEnumeratesEverything(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	// 1100 is past S3's per-response cap of 1000, which is the boundary the old limit sat on.
	const want = 1100
	for i := range want {
		f.srv.PutObject("many/f"+strconv.Itoa(i)+".dat", []byte("x"))
	}

	node := &DirectoryNode{fs: f.fs, path: "many"}

	stream, errno := node.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("Readdir: errno %v", errno)
	}
	defer stream.Close()

	names := drain(t, stream)
	if len(names) != want {
		t.Errorf("Readdir returned %d of %d entries. The entries it omitted do not exist as far as "+
			"any caller can tell, so rm -r reports success having deleted a fraction of the directory.",
			len(names), want)
	}
}

// TestReaddirDeduplicatesAndOmitsDotEntries.
//
// Two distinct object keys routinely produce the same *entry name*: a marker object at "dir/" and any
// object under "dir/" both reduce to "dir", and Mkdir writes exactly such a marker. A duplicate name
// in a DirStream makes readdir return the same entry twice, which ls prints twice and rsync treats as
// a protocol error. Dot entries are go-fuse's to synthesize in readDirMaybeLookup; a stream that
// supplies its own gets them twice.
func TestReaddirDeduplicatesAndOmitsDotEntries(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	f.srv.PutObject("top/sub/", nil)              // the marker Mkdir writes
	f.srv.PutObject("top/sub/a.dat", []byte("a")) // and an object under the same prefix
	f.srv.PutObject("top/sub/b.dat", []byte("b")) // and another
	f.srv.PutObject("top/", nil)                  // the directory's own marker
	f.srv.PutObject("top/file.dat", []byte("f"))

	node := &DirectoryNode{fs: f.fs, path: "top"}

	stream, errno := node.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("Readdir: errno %v", errno)
	}
	defer stream.Close()

	names := drain(t, stream)

	seen := map[string]int{}
	for _, n := range names {
		seen[n]++
	}

	for name, count := range seen {
		if count > 1 {
			t.Errorf("Readdir returned %q %d times; ls prints it twice and rsync reports a protocol "+
				"error", name, count)
		}
	}
	for _, dot := range []string{".", ".."} {
		if seen[dot] > 0 {
			t.Errorf("Readdir emitted %q, which go-fuse also synthesizes, so the kernel sees it twice",
				dot)
		}
	}
	if seen[""] > 0 {
		t.Error("Readdir emitted a nameless entry: the directory's own marker object, whose key is " +
			"the prefix itself")
	}

	for _, want := range []string{"sub", "file.dat"} {
		if seen[want] != 1 {
			t.Errorf("Readdir returned %q %d times, want exactly 1; got %v", want, seen[want], names)
		}
	}
}

// drain collects every name a DirStream yields.
func drain(t *testing.T, stream fs.DirStream) []string {
	t.Helper()

	var names []string
	for stream.HasNext() {
		e, errno := stream.Next()
		if errno != 0 {
			t.Fatalf("DirStream.Next: errno %v", errno)
		}
		names = append(names, e.Name)
	}

	return names
}

// TestLookupOfAMissingNameIsENOENT, and the entry it fills for one that exists describes the file.
//
// out.Attr must be populated here rather than left to the bridge. go-fuse has a fallback that stats
// the child through NodeGetattrer, but it is reached only when the parent does *not* implement
// NodeLookuper — and DirectoryNode does. v0.10.0 returned an inode with out.Attr zeroed, so the entry
// the kernel cached for the whole EntryTimeout described a file of no type, size 0, mode 0000.
func TestLookupFillsTheEntryAttributes(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	const size = 3333
	f.srv.SeedRandom("look.dat", size)

	root := f.root(t)
	ctx := context.Background()

	var out fuse.EntryOut
	child, errno := root.Lookup(ctx, "look.dat", &out)
	if errno != 0 {
		t.Fatalf("Lookup: errno %v", errno)
	}
	if child == nil {
		t.Fatal("Lookup returned a nil inode with errno 0")
	}

	if out.Size != size {
		t.Errorf("Lookup filled the entry with size %d, want %d", out.Size, size)
	}
	if out.Mode&07777 == 0 {
		t.Errorf("Lookup filled the entry with mode %#o; the kernel caches this for the whole entry "+
			"timeout", out.Mode)
	}
	if out.Mode&syscall.S_IFREG == 0 {
		t.Errorf("Lookup filled the entry with mode %#o, which lacks S_IFREG", out.Mode)
	}

	if _, errno := root.Lookup(ctx, "absent.dat", &fuse.EntryOut{}); errno != syscall.ENOENT {
		t.Errorf("Lookup of an absent name: errno %v, want ENOENT", errno)
	}
}

// TestLookupOfADirectoryReportsADirectory. A prefix with objects under it is a directory whether or
// not anything wrote a marker for it.
func TestLookupOfADirectoryReportsADirectory(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	f.srv.PutObject("implied/child.dat", []byte("x"))

	root := f.root(t)

	var out fuse.EntryOut
	if _, errno := root.Lookup(context.Background(), "implied", &out); errno != 0 {
		t.Fatalf("Lookup of an implied directory: errno %v", errno)
	}

	if out.Mode&syscall.S_IFDIR == 0 {
		t.Errorf("an implied directory reports mode %#o, which lacks S_IFDIR", out.Mode)
	}
	if out.Mode&0o111 == 0 {
		t.Errorf("an implied directory reports mode %#o, which cannot be traversed", out.Mode)
	}
}

// TestAttrTimeoutFollowsCacheTTL. The nodes and fs.Options must agree on how long the kernel may
// cache an attribute set, since rawBridge.SetAttr applies no default of its own.
func TestAttrTimeoutFollowsCacheTTL(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	if got := f.fs.attrTimeout(); got != defaultAttrTimeout {
		t.Errorf("with CacheTTL unset the timeout is %v, want the go-fuse default %v",
			got, defaultAttrTimeout)
	}

	f.fs.config.CacheTTL = 90 * time.Second
	if got := f.fs.attrTimeout(); got != 90*time.Second {
		t.Errorf("with CacheTTL=90s the timeout is %v", got)
	}
}

// TestFileDefaultsFallBackToTheMountingUser.
//
// "The object has no objectfs-uid" and "the object records objectfs-uid=0" are different facts.
// Reporting root for the first would make every object written by aws s3 cp or boto3 appear to belong
// to someone else in ls -l, and would make cp -p and rsync complain about ownership they cannot set.
func TestFileDefaultsFallBackToTheMountingUser(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	// An object with no ObjectFS metadata at all — what any other S3 tool produces.
	f.srv.PutObject("foreign.dat", []byte("written by another tool"))

	node := &FileNode{fs: f.fs, path: "foreign.dat"}

	var out fuse.AttrOut
	if errno := node.Getattr(context.Background(), nil, &out); errno != 0 {
		t.Fatalf("Getattr: errno %v", errno)
	}

	if out.Uid != f.fs.config.DefaultUID || out.Gid != f.fs.config.DefaultGID {
		t.Errorf("an object carrying no objectfs-uid reports owner %d:%d, want the configured "+
			"%d:%d", out.Uid, out.Gid, f.fs.config.DefaultUID, f.fs.config.DefaultGID)
	}
	if got := out.Mode & 07777; got != f.fs.config.DefaultMode {
		t.Errorf("an object carrying no objectfs-mode reports mode %#o, want the configured %#o",
			got, f.fs.config.DefaultMode)
	}
}

// newWriterOver returns a fresh vfs.Writer over the fixture's backend, holding no in-memory state.
// It is how a test asks "what would a remount see", without unmounting anything.
func newWriterOver(t *testing.T, f *readPathFixture) *vfs.Writer {
	t.Helper()

	w, err := vfs.NewWriter(context.Background(), f.fs.backend)
	if err != nil {
		t.Fatalf("vfs.NewWriter: %v", err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Logf("closing the replacement writer: %v", err)
		}
	})

	return w
}
