//go:build linux || darwin

package fuse

// These tests are #532's end-to-end proof, and the thing they assert is not an errno — it is that a
// read-only mount sends no byte to the object store.
//
// The errno matters too, and is checked, but an errno is a claim ObjectFS makes about itself. The
// endpoint's request log is not: testaws is a real HTTP server, so a PUT that happened is a PUT that
// appears there regardless of what any layer reported. That distinction is the whole reason this file
// exists rather than a table of expected syscall.Errno values, because through v0.16.0 every errno gate
// below was present and correct and the mount was writable anyway — nothing set Config.ReadOnly, so none
// of them ran. A test that only asked "does Mkdir return EROFS when ReadOnly is set" would have passed on
// that build too, and would have said nothing about any mount a user could create.

import (
	"context"
	"net/http"
	"syscall"
	"testing"
	"time"

	gofuse "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// readOnlyFixture is a FileSystem over a real S3 endpoint, with read-only enforcement either on or off in
// both layers at once.
//
// Both from one bool on purpose, because that is how internal/adapter wires it: one `mount.read_only` key
// reaches Config.ReadOnly and vfs.WriterOptions.ReadOnly. A fixture that let them disagree would be
// testing a configuration the program cannot produce — except deliberately, which
// [TestTheWriterBackstopHoldsWithoutTheFUSEGate] does.
type readOnlyFixture struct {
	fs  *FileSystem
	srv *testaws.TestServer

	// rootDir is the mount's root attached to a go-fuse bridge. See [readOnlyFixture.root].
	rootDir *DirectoryNode
}

func newReadOnlyFixture(t *testing.T, readOnly bool) *readOnlyFixture {
	t.Helper()

	return newSplitReadOnlyFixture(t, readOnly, readOnly)
}

// newSplitReadOnlyFixture builds the fixture with the two enforcement layers set independently.
func newSplitReadOnlyFixture(t *testing.T, fuseGate, writerGate bool) *readOnlyFixture {
	t.Helper()

	srv := testaws.Start(t)
	backend := srv.Backend()

	writer, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{
		ReadOnly: writerGate,
	})
	if err != nil {
		t.Fatalf("vfs.NewWriterWithOptions: %v", err)
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
		ReadOnly:       fuseGate,
	})

	root, ok := fs.Root().(*DirectoryNode)
	if !ok {
		t.Fatalf("FileSystem.Root returned %T, want *DirectoryNode", fs.Root())
	}

	// The bridge is load-bearing for the *writable* half of these tests, not the read-only half, and that
	// asymmetry is itself informative. Mkdir, Create and Rename call Inode.NewInode to publish the entry
	// they made, which reaches through the embedded Inode to the bridge that owns the tree; a bare
	// &DirectoryNode{} has none and go-fuse dereferences nil. On a read-only mount none of them gets that
	// far, so the refusal path never needed it — which is exactly why
	// [TestWritableMountRefusesNoneOfThem] is the test that found this missing.
	timeout := fs.attrTimeout()
	_ = gofuse.NewNodeFS(root, &gofuse.Options{
		AttrTimeout:     &timeout,
		EntryTimeout:    &timeout,
		NullPermissions: true,
	})

	return &readOnlyFixture{fs: fs, srv: srv, rootDir: root}
}

// seed puts an object in the bucket and clears the request log, so that what the log holds afterwards is
// exactly what the operations under test sent.
//
// Clearing matters more than it looks: testaws.Start creates the bucket with a PUT, and the seed itself is
// another. An assertion on "no PUT at all" without this would fail on a read-only mount that behaved
// perfectly.
func (f *readOnlyFixture) seed(t *testing.T, key string, data []byte) {
	t.Helper()

	if err := f.fs.backend.PutObject(context.Background(), key, data, nil); err != nil {
		t.Fatalf("seed %q: %v", key, err)
	}
	f.srv.ResetRequests()
}

// writingRequests returns every request the endpoint saw that could have stored bytes or attributes,
// across all keys.
//
// All keys rather than [testaws.TestServer.Writes]'s single key, because the failure this is looking for
// includes a write to a key the test never named — a directory marker for the Mkdir that was supposed to
// be refused, or the destination half of a Rename that got as far as its server-side copy.
func (f *readOnlyFixture) writingRequests() []testaws.Request {
	var out []testaws.Request

	for _, r := range f.srv.Requests() {
		if r.Method == http.MethodPut || r.Method == http.MethodPost || r.Method == http.MethodDelete {
			out = append(out, r)
		}
	}

	return out
}

// modifyingOperation is one FUSE entry point that must be refused on a read-only mount.
type modifyingOperation struct {
	name string
	call func(*readOnlyFixture, string) syscall.Errno
}

// modifyingOperations enumerates every entry point in this package that carries a ReadOnly gate.
//
// Thirteen of them, which is the number the grep finds:
//
//	filesystem.go — DirectoryNode.Mkdir, Create, Unlink, Rmdir; FileNode.Open; FileHandle.Write
//	rename.go     — DirectoryNode.Rename
//	attributes.go — FileNode.Setattr, DirectoryNode.Setattr
//	xattr.go      — FileNode.Setxattr, Removexattr; DirectoryNode.Setxattr, Removexattr
//
// An enumeration, with the drift problem every enumeration has — see
// [TestEveryReadOnlyGateIsExercised], which counts the gates in the source and fails when this list
// stops matching. That guard is the point: a fourteenth mutating entry point added without a gate is
// exactly the defect internal/vfs's backstop exists to survive, and it should also be *noticed*.
var modifyingOperations = []modifyingOperation{
	{"DirectoryNode.Mkdir", func(f *readOnlyFixture, _ string) syscall.Errno {
		_, errno := f.root().Mkdir(context.Background(), "newdir", 0o755, &fuse.EntryOut{})

		return errno
	}},
	{"DirectoryNode.Create", func(f *readOnlyFixture, _ string) syscall.Errno {
		_, _, _, errno := f.root().Create(context.Background(), "created.txt",
			syscall.O_CREAT|syscall.O_WRONLY, 0o644, &fuse.EntryOut{})

		return errno
	}},
	{"DirectoryNode.Unlink", func(f *readOnlyFixture, key string) syscall.Errno {
		return f.root().Unlink(context.Background(), key)
	}},
	{"DirectoryNode.Rmdir", func(f *readOnlyFixture, _ string) syscall.Errno {
		return f.root().Rmdir(context.Background(), "subdir")
	}},
	{"FileNode.Open(O_WRONLY)", func(f *readOnlyFixture, key string) syscall.Errno {
		_, _, errno := f.file(key).Open(context.Background(), syscall.O_WRONLY)

		return errno
	}},
	{"FileNode.Open(O_TRUNC)", func(f *readOnlyFixture, key string) syscall.Errno {
		_, _, errno := f.file(key).Open(context.Background(), syscall.O_TRUNC)

		return errno
	}},
	{"FileHandle.Write", func(f *readOnlyFixture, key string) syscall.Errno {
		_, errno := f.handle(key).Write(context.Background(), []byte("overwritten"), 0)

		return errno
	}},
	{"DirectoryNode.Rename", func(f *readOnlyFixture, key string) syscall.Errno {
		root := f.root()

		return root.Rename(context.Background(), key, root, "renamed.txt", 0)
	}},
	{"FileNode.Setattr", func(f *readOnlyFixture, key string) syscall.Errno {
		in := &fuse.SetAttrIn{}
		in.Valid = fuse.FATTR_MODE
		in.Mode = 0o600

		return f.file(key).Setattr(context.Background(), nil, in, &fuse.AttrOut{})
	}},
	{"DirectoryNode.Setattr", func(f *readOnlyFixture, _ string) syscall.Errno {
		in := &fuse.SetAttrIn{}
		in.Valid = fuse.FATTR_MODE
		in.Mode = 0o700

		return f.root().Setattr(context.Background(), nil, in, &fuse.AttrOut{})
	}},
	{"FileNode.Setxattr", func(f *readOnlyFixture, key string) syscall.Errno {
		return f.file(key).Setxattr(context.Background(), "user.objectfs.test", []byte("v"), 0)
	}},
	{"FileNode.Removexattr", func(f *readOnlyFixture, key string) syscall.Errno {
		return f.file(key).Removexattr(context.Background(), "user.objectfs.test")
	}},
	{"DirectoryNode.Setxattr", func(f *readOnlyFixture, _ string) syscall.Errno {
		return f.root().Setxattr(context.Background(), "user.objectfs.test", []byte("v"), 0)
	}},
	{"DirectoryNode.Removexattr", func(f *readOnlyFixture, _ string) syscall.Errno {
		return f.root().Removexattr(context.Background(), "user.objectfs.test")
	}},
}

// root returns the mount's root directory, attached to a go-fuse bridge by the constructor.
func (f *readOnlyFixture) root() *DirectoryNode {
	return f.rootDir
}

func (f *readOnlyFixture) file(key string) *FileNode {
	return &FileNode{fs: f.fs, path: key}
}

func (f *readOnlyFixture) handle(key string) *FileHandle {
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

// TestReadOnlyMountRefusesEveryModifyingOperation is the errno half.
//
// EROFS specifically, not "some error". EROFS is what tells `cp` and `tar` to stop and what `mount(8)`
// semantics mean by read-only; EACCES reads as a permissions problem an operator would go looking for in
// a bucket policy, and EIO reads as a fault in ObjectFS. The distinction is the difference between an
// operator understanding their mount and filing a bug.
func TestReadOnlyMountRefusesEveryModifyingOperation(t *testing.T) {
	t.Parallel()

	const key = "protected.txt"

	for _, op := range modifyingOperations {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()

			f := newReadOnlyFixture(t, true)
			f.seed(t, key, []byte("original contents"))

			if errno := op.call(f, key); errno != syscall.EROFS {
				t.Errorf("%s returned %v on a read-only mount, want EROFS", op.name, errno)
			}

			// The endpoint, per operation, so a failure names which one wrote.
			if got := f.writingRequests(); len(got) != 0 {
				t.Errorf("%s sent %d modifying request(s) to the object store while being refused: %+v",
					op.name, len(got), got)
			}
		})
	}
}

// TestEveryFUSEGateFiresOnItsOwn isolates the thirteen gates from the backstop underneath them.
//
// The test above cannot do this, and the reason is the redundancy it is testing. On a real read-only mount
// both layers are armed, so FileHandle.Write returns EROFS whether its own gate is present or the vfs
// backstop caught it — delete the gate at filesystem.go:1330 and that test stays green. Two layers that
// each cover the other's absence are exactly what was wanted at runtime and exactly what makes a mutation
// undetectable.
//
// So this runs with the FUSE gate armed and the write path writable, which is a configuration
// internal/adapter cannot produce: any EROFS here came from the gate itself and from nowhere else. Between
// this and [TestTheWriterBackstopHoldsWithoutTheFUSEGate], each layer is pinned without the other standing
// in for it.
func TestEveryFUSEGateFiresOnItsOwn(t *testing.T) {
	t.Parallel()

	const key = "protected.txt"

	for _, op := range modifyingOperations {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()

			f := newSplitReadOnlyFixture(t, true, false)
			f.seed(t, key, []byte("original contents"))

			if f.fs.buffer.ReadOnly() {
				t.Fatal("the fixture was supposed to leave the write path writable; with the backstop " +
					"armed, this test cannot tell a present gate from a deleted one")
			}

			if errno := op.call(f, key); errno != syscall.EROFS {
				t.Errorf("%s returned %v with only its own gate armed, want EROFS. The gate is missing or "+
					"no longer reached; on a real mount the vfs backstop would hide that for the write "+
					"path and hide nothing for Mkdir, Unlink, Rmdir or Rename", op.name, errno)
			}

			if got := f.writingRequests(); len(got) != 0 {
				t.Errorf("%s sent %d modifying request(s) to the object store: %+v", op.name, len(got), got)
			}
		})
	}
}

// TestWritableMountRefusesNoneOfThem is the non-vacuity guard, and it is not optional.
//
// Every wrong reason for a refusal is still a refusal. Mkdir on a mount with no write path returns an
// error; so does Rmdir of a directory that does not exist, and Removexattr of an attribute that was never
// set. A table asserting "these all fail" passes on a build where the ReadOnly gate was deleted and
// something else happened to fail — so what has to be shown is that *EROFS specifically* is contributed
// by the flag and by nothing else in this fixture.
//
// So this asserts the negative: on a writable mount, not one of the thirteen returns EROFS. Whatever else
// they return is out of scope here — several legitimately fail, for reasons that have their own tests.
func TestWritableMountRefusesNoneOfThem(t *testing.T) {
	t.Parallel()

	const key = "writable.txt"

	for _, op := range modifyingOperations {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()

			f := newReadOnlyFixture(t, false)
			f.seed(t, key, []byte("original contents"))

			if errno := op.call(f, key); errno == syscall.EROFS {
				t.Errorf("%s returned EROFS on a *writable* mount. EROFS therefore does not distinguish "+
					"a read-only mount here, and TestReadOnlyMountRefusesEveryModifyingOperation's row "+
					"for this operation proves nothing", op.name)
			}
		})
	}
}

// TestEveryReadOnlyGateIsExercised is the drift guard on [modifyingOperations].
//
// The list above is hand-written, and a hand-written list covers what somebody thought of. This counts
// what is actually in the source — `config.ReadOnly` gates across this package — and fails when the two
// disagree, so a fourteenth gate added without a row here is a red test rather than an untested gate.
//
// Counted rather than discovered by reflection because the gates are methods on three different receivers
// with four different signatures, and "returns EROFS when read-only" is not a property a signature
// carries. The count is maintained here; the grep that produces it is in the failure message, so the next
// person does not have to reconstruct it.
func TestEveryReadOnlyGateIsExercised(t *testing.T) {
	t.Parallel()

	// One row per gate, plus one extra row for FileNode.Open: its gate covers four open flags and two of
	// them are exercised separately, because O_TRUNC destroys data without ever calling Write.
	const wantGates = 13

	seen := make(map[string]bool, len(modifyingOperations))
	for _, op := range modifyingOperations {
		if seen[op.name] {
			t.Errorf("two rows named %q, so one of them is shadowing a gate that is not being tested",
				op.name)
		}
		seen[op.name] = true
	}

	// FileNode.Open appears twice by design; every other name is one gate.
	gates := len(modifyingOperations) - 1
	if gates != wantGates {
		t.Errorf("modifyingOperations covers %d gates, want %d. If a ReadOnly gate was added or removed, "+
			"update both — the source of truth is:\n"+
			"    grep -rn 'config\\.ReadOnly' internal/fuse/*.go | grep -v _test\n"+
			"which must list %d gates in filesystem.go, rename.go, attributes.go and xattr.go, plus "+
			"mount.go's kernel option and platform.go's assignment, which are not gates",
			gates, wantGates, wantGates)
	}
}

// TestReadOnlyMountStillServesReads is the other half of the feature, and the one a wrongly placed gate
// breaks.
//
// A mount that refuses to be read is not a read-only mount. This is easy to get wrong in the layer below:
// vfs.Writer.node is called by ReadAt and FileSize as well as by the mutators, so gating it — the obvious
// implementation — returns EROFS for every read. Nothing in the tests above notices that, because they
// only ever ask for refusals.
func TestReadOnlyMountStillServesReads(t *testing.T) {
	t.Parallel()

	const key = "protected.txt"
	body := []byte("original contents")

	f := newReadOnlyFixture(t, true)
	f.seed(t, key, body)

	// O_RDONLY is zero, so it clears the gate's mask by construction; asserted anyway, because the mask is
	// the kind of expression a later edit widens.
	fh, _, errno := f.file(key).Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open(O_RDONLY) on a read-only mount returned %v, want success", errno)
	}

	reader, ok := fh.(gofuse.FileReader)
	if !ok {
		t.Fatalf("Open returned a %T, which cannot read", fh)
	}

	dest := make([]byte, len(body))
	result, errno := reader.Read(context.Background(), dest, 0)
	if errno != 0 {
		t.Fatalf("Read on a read-only mount returned %v", errno)
	}

	got, status := result.Bytes(dest)
	if !status.Ok() {
		t.Fatalf("Read result status %v", status)
	}
	if string(got) != string(body) {
		t.Errorf("Read returned %q, want %q", got, body)
	}

	// Reads create vfs nodes, to overlay pending writes and to report sizes that include them. Those nodes
	// are what a flush at unmount walks, so a read-only mount that has served a read must still flush
	// nothing — the guarantee comes from Flusher.attemptAttrOnly returning early on a clean node, which is
	// code #532 did not touch and therefore has to be checked rather than assumed.
	if errno := f.handle(key).Flush(context.Background()); errno != 0 {
		t.Errorf("Flush after a read on a read-only mount returned %v", errno)
	}

	if got := f.writingRequests(); len(got) != 0 {
		t.Errorf("reading and flushing a read-only mount sent %d modifying request(s): %+v", len(got), got)
	}
}

// TestTheWriterBackstopHoldsWithoutTheFUSEGate is the argument for enforcing this twice, made as a test
// rather than as a comment.
//
// The configuration is deliberately one internal/adapter cannot produce: Config.ReadOnly false, writer
// read-only. It stands for the fourteenth entry point — the one added later by someone who did not know
// about the thirteen, which is the defect mode every "check it at each entry point" design has. With the
// FUSE gate absent, the write must still be refused, and it must still be refused with EROFS, because
// internal/fuse/errno.go maps vfs.ErrReadOnly to exactly that.
//
// If this test ever fails while the two above pass, the redundancy has silently become a single point of
// enforcement and the next unguarded entry point writes to the bucket.
func TestTheWriterBackstopHoldsWithoutTheFUSEGate(t *testing.T) {
	t.Parallel()

	const key = "protected.txt"

	f := newSplitReadOnlyFixture(t, false, true)
	f.seed(t, key, []byte("original contents"))

	if f.fs.config.ReadOnly {
		t.Fatal("the fixture was supposed to leave the FUSE gate off; this test proves nothing with it on")
	}

	_, errno := f.handle(key).Write(context.Background(), []byte("overwritten"), 0)
	if errno != syscall.EROFS {
		t.Errorf("a write through a FileHandle with no ReadOnly gate returned %v, want EROFS from the vfs "+
			"backstop by way of errno.go's ErrReadOnly mapping", errno)
	}

	if got := f.writingRequests(); len(got) != 0 {
		t.Errorf("the backstop let %d modifying request(s) reach the object store: %+v", len(got), got)
	}

	// And the bytes in the bucket are the seeded ones.
	stored, err := f.fs.backend.GetObject(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatalf("read back %q: %v", key, err)
	}
	if string(stored) != "original contents" {
		t.Errorf("the stored object is now %q; the write was reported as refused and happened anyway",
			stored)
	}
}
