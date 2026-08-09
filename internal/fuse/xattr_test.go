//go:build linux || darwin

package fuse

// Tests for the four extended-attribute operations, against a real S3 endpoint and the real write path.
//
// Every durability assertion reads through a fresh HeadObject or through a write path built from
// nothing, never through the same in-memory record the operation just wrote. A setxattr that updates the
// node's attributes and never reaches S3 satisfies any assertion that asks the write path what it
// believes — and "extended attributes do not survive a remount" is exactly that failure, in the same
// shape as the chmod defect attributes_test.go covers.
//
// The bridge is driven directly where the property under test belongs to the kernel-facing contract: the
// size protocol, the ERANGE path, and the flags all live in the request rather than in a node method's
// arguments. fs.NewNodeFS returns the fuse.RawFileSystem the kernel talks to, so calling its methods
// runs the same dispatch the kernel would, with no mount, no privileges, and no macFUSE.

import (
	"bytes"
	"context"
	"strings"
	"syscall"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	awstypes "github.com/aws/aws-sdk-go-v2/service/s3/types"
	gofuse "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// xattrFixture is a readPathFixture with a seeded object and a FileNode over it.
type xattrFixture struct {
	*readPathFixture

	key  string
	node *FileNode
}

// newXattrFixture seeds one object and returns a node addressing it.
//
// RequireMetadataReplace is asserted here rather than per test: every write below flushes with no content
// pending, which is the SetObjectMetadata path, and against an endpoint that ignores the directive the
// flush's own read-back fails loudly. Requiring the capability makes the skip explicit instead of leaving
// a suite of tests that fail for a reason unrelated to what they check.
func newXattrFixture(t *testing.T, key string) *xattrFixture {
	t.Helper()

	f := newReadPathFixture(t)
	f.srv.RequireMetadataReplace()
	f.srv.SeedRandom(key, 4096)

	return &xattrFixture{readPathFixture: f, key: key, node: &FileNode{fs: f.fs, path: key}}
}

// remount discards every byte of in-memory state, so the next read answers only from what S3 holds.
//
// This is the assertion that matters for a persistence feature and the one a mock cannot make: it
// replaces the write path with one that has never seen a single operation from this test.
func (x *xattrFixture) remount(t *testing.T) {
	t.Helper()

	x.fs.buffer = newWriterOver(t, x.readPathFixture)
	x.fs.invalidate(x.key)
}

// get reads one attribute the way a caller does: a size query, then a read into a buffer that size.
func (x *xattrFixture) get(t *testing.T, name string) ([]byte, syscall.Errno) {
	t.Helper()

	size, errno := x.node.Getxattr(context.Background(), name, nil)
	if errno != 0 {
		return nil, errno
	}

	buf := make([]byte, size)
	n, errno := x.node.Getxattr(context.Background(), name, buf)
	if errno != 0 {
		return nil, errno
	}

	return buf[:n], 0
}

// list reads the attribute names, following the same two-call protocol.
func (x *xattrFixture) list(t *testing.T) []string {
	t.Helper()

	size, errno := x.node.Listxattr(context.Background(), nil)
	if errno != 0 {
		t.Fatalf("listxattr size query: errno %v", errno)
	}
	if size == 0 {
		return nil
	}

	buf := make([]byte, size)
	n, errno := x.node.Listxattr(context.Background(), buf)
	if errno != 0 {
		t.Fatalf("listxattr read: errno %v", errno)
	}

	var names []string
	for raw := range bytes.SplitSeq(buf[:n], []byte{0}) {
		if len(raw) > 0 {
			names = append(names, string(raw))
		}
	}

	return names
}

// TestSetxattrThenGetxattrRoundTripsThroughStorage is the acceptance criterion from #167, as close to
// `setfattr -n user.test -v hello f && getfattr -n user.test f` as this can be driven without a mount.
//
// The read after the remount is the whole test. Everything before it would also pass if the value never
// left this process.
func TestSetxattrThenGetxattrRoundTripsThroughStorage(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "roundtrip.dat")

	if errno := x.node.Setxattr(context.Background(), "user.test", []byte("hello"), 0); errno != 0 {
		t.Fatalf("setxattr user.test=hello: errno %v", errno)
	}

	got, errno := x.get(t, "user.test")
	if errno != 0 {
		t.Fatalf("getxattr user.test: errno %v", errno)
	}
	if string(got) != "hello" {
		t.Errorf("getxattr returned %q, want %q", got, "hello")
	}

	// The stored form is on the object, under a key that is not the attribute name.
	meta := x.srv.ObjectMetadata(x.key)
	var found bool
	for k := range meta {
		if strings.HasPrefix(strings.ToLower(k), "objectfs-xattr-") {
			found = true
		}
		if strings.EqualFold(k, "objectfs-xattr-user.test") || strings.EqualFold(k, "user.test") {
			t.Errorf("the attribute is stored under %q, which is the raw name; S3 lower-cases metadata "+
				"keys, so user.Test and user.test would collide", k)
		}
	}
	if !found {
		t.Fatalf("no objectfs-xattr- key on the object after setxattr. Metadata: %v", meta)
	}

	// And the remount: nothing in memory, only what S3 holds.
	x.remount(t)

	got, errno = x.get(t, "user.test")
	if errno != 0 {
		t.Fatalf("getxattr after discarding all in-memory state: errno %v. The attribute did not survive "+
			"to storage, which is the whole of what setxattr promises.", errno)
	}
	if string(got) != "hello" {
		t.Errorf("after a remount the attribute reads %q, want %q", got, "hello")
	}
}

// TestSetxattrIsDurableWithoutAClose covers why the write path flushes synchronously.
//
// setxattr(2) takes a path, so there is no descriptor whose release would persist the change later.
// Nothing would ever flush it: it would survive only to the FlushAll at unmount, and a mount killed
// before then would lose it with no error anywhere. The assertion is that S3 already holds the attribute
// the instant Setxattr returns — no Fsync, no Release, no FlushAll.
func TestSetxattrIsDurableWithoutAClose(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "durable.dat")

	if errno := x.node.Setxattr(context.Background(), "user.sync", []byte("v"), 0); errno != 0 {
		t.Fatalf("setxattr: errno %v", errno)
	}

	// Read the object's metadata directly, with no ObjectFS layer involved at all.
	meta := x.srv.ObjectMetadata(x.key)
	if len(meta) == 0 {
		t.Fatalf("the object carries no user metadata at all after setxattr")
	}

	attrs := vfs.AttrFromMetadata(meta, 0, processStart, "")
	if v, ok := attrs.Xattr("user.sync"); !ok || string(v) != "v" {
		t.Errorf("S3 does not hold user.sync immediately after setxattr returned (got %q, present=%v). "+
			"Nothing else will ever flush it: setxattr has no descriptor to release, so the attribute "+
			"would be lost by any mount that is killed before unmount.", v, ok)
	}
}

// TestGetxattrReportsAbsenceAsNoAttrNotNoEnt is the misclassification that matters most here.
//
// ENOENT and ENOATTR are both errnos a caller sees from getxattr, and they mean different things: ENOENT
// says the *file* is gone. A program probing for an attribute before creating one would conclude the path
// had disappeared underneath it, and `getfattr` renders the two differently to a person.
func TestGetxattrReportsAbsenceAsNoAttrNotNoEnt(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "absent.dat")

	_, errno := x.node.Getxattr(context.Background(), "user.nothing", make([]byte, 64))

	if errno == syscall.ENOENT {
		t.Fatal("getxattr for a missing attribute returned ENOENT, which says the file does not exist. " +
			"A caller checking for an attribute before creating it would conclude the path is gone.")
	}
	if errno != errNoXattr {
		t.Errorf("getxattr for a missing attribute returned %s, want %s",
			errnoName(errno), errnoName(errNoXattr))
	}

	// The same distinction for removexattr, which has the same two candidate errnos.
	if errno := x.node.Removexattr(context.Background(), "user.nothing"); errno != errNoXattr {
		t.Errorf("removexattr for a missing attribute returned %s, want %s",
			errnoName(errno), errnoName(errNoXattr))
	}
}

// TestRemovexattrStopsTheAttributeBeingReadable is the removal, checked past the layer that could fake
// it.
//
// The mechanism is a tombstone, because [types.Backend.SetObjectMetadata] merges the object's existing
// metadata underneath the caller's and therefore cannot delete a key — a removexattr that simply stopped
// rendering the key would report success while the attribute stayed readable forever. So the assertion
// is made after a remount: in-memory state would hide exactly that bug.
func TestRemovexattrStopsTheAttributeBeingReadable(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "removal.dat")
	ctx := context.Background()

	if errno := x.node.Setxattr(ctx, "user.doomed", []byte("here"), 0); errno != 0 {
		t.Fatalf("setxattr: errno %v", errno)
	}
	if errno := x.node.Setxattr(ctx, "user.kept", []byte("stays"), 0); errno != 0 {
		t.Fatalf("setxattr of the second attribute: errno %v", errno)
	}
	if errno := x.node.Removexattr(ctx, "user.doomed"); errno != 0 {
		t.Fatalf("removexattr: errno %v", errno)
	}

	x.remount(t)

	if _, errno := x.get(t, "user.doomed"); errno != errNoXattr {
		t.Errorf("after a remount the removed attribute reads with errno %s, want %s. The removal did "+
			"not reach storage: omitting a key from a metadata replace does not delete it, because the "+
			"backend merges the object's existing metadata underneath.",
			errnoName(errno), errnoName(errNoXattr))
	}

	// A removal must not take the neighbors with it. A metadata replace writes the whole set, so a path
	// that rendered only the changed attribute would delete every other one.
	got, errno := x.get(t, "user.kept")
	if errno != 0 {
		t.Fatalf("the other attribute is gone after removing an unrelated one: errno %v", errno)
	}
	if string(got) != "stays" {
		t.Errorf("the other attribute reads %q, want %q", got, "stays")
	}

	if names := x.list(t); len(names) != 1 || names[0] != "user.kept" {
		t.Errorf("listxattr reports %v after one of two attributes was removed, want [user.kept]", names)
	}
}

// TestListxattrNamesEveryAttributeAndNulTerminatesThem covers the reply format and the size protocol
// together, since a caller cannot use either without the other.
func TestListxattrNamesEveryAttributeAndNulTerminatesThem(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "list.dat")
	ctx := context.Background()

	want := []string{"user.a", "user.b", "user.c"}
	for _, name := range want {
		if errno := x.node.Setxattr(ctx, name, []byte("v"), 0); errno != 0 {
			t.Fatalf("setxattr %s: errno %v", name, errno)
		}
	}

	x.remount(t)

	// The size query first, which is what every caller issues before allocating.
	size, errno := x.node.Listxattr(ctx, nil)
	if errno != 0 {
		t.Fatalf("listxattr size query: errno %v", errno)
	}

	var wantSize int
	for _, name := range want {
		wantSize += len(name) + 1
	}
	if int(size) != wantSize {
		t.Errorf("listxattr reported a size of %d, want %d (each name plus its NUL). A caller allocates "+
			"exactly this and gets ERANGE forever if it is short.", size, wantSize)
	}

	buf := make([]byte, size)
	n, errno := x.node.Listxattr(ctx, buf)
	if errno != 0 {
		t.Fatalf("listxattr read: errno %v", errno)
	}
	if int(n) != wantSize {
		t.Errorf("listxattr wrote %d bytes, want %d", n, wantSize)
	}

	// The final name must be terminated too. Dropping the last NUL truncates it by a byte in getfattr's
	// output, which looks like a corrupted attribute name rather than a formatting slip.
	if n == 0 || buf[n-1] != 0 {
		t.Errorf("the name list does not end with a NUL: %q", buf[:n])
	}

	got := x.list(t)
	if len(got) != len(want) {
		t.Fatalf("listxattr named %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("listxattr named %v, want %v (sorted, so that two identical calls agree)", got, want)
		}
	}

	// A buffer one byte short must report ERANGE *with* the size, not instead of it: the size is what the
	// caller retries with.
	shortSize, errno := x.node.Listxattr(ctx, make([]byte, wantSize-1))
	if errno != syscall.ERANGE {
		t.Errorf("listxattr into a buffer one byte too small returned %s, want ERANGE", errnoName(errno))
	}
	if int(shortSize) != wantSize {
		t.Errorf("listxattr reported %d alongside ERANGE, want %d — a caller sizes its next buffer from "+
			"this number and loops forever if it is wrong", shortSize, wantSize)
	}
}

// TestGetxattrHonorsTheSizeProtocol is the same two-call contract for a single attribute.
func TestGetxattrHonorsTheSizeProtocol(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "sizes.dat")
	ctx := context.Background()

	value := []byte("a value of some length")
	if errno := x.node.Setxattr(ctx, "user.v", value, 0); errno != 0 {
		t.Fatalf("setxattr: errno %v", errno)
	}

	size, errno := x.node.Getxattr(ctx, "user.v", nil)
	if errno != 0 {
		t.Fatalf("getxattr size query returned errno %v; a zero-length buffer is a size query and must "+
			"succeed", errno)
	}
	if int(size) != len(value) {
		t.Errorf("getxattr reported a size of %d, want %d", size, len(value))
	}

	short, errno := x.node.Getxattr(ctx, "user.v", make([]byte, len(value)-1))
	if errno != syscall.ERANGE {
		t.Errorf("getxattr into a short buffer returned %s, want ERANGE", errnoName(errno))
	}
	if int(short) != len(value) {
		t.Errorf("getxattr reported %d alongside ERANGE, want %d", short, len(value))
	}

	// An exactly-sized buffer must be filled completely, and a larger one must report the value's length
	// rather than the buffer's.
	for _, extra := range []int{0, 64} {
		buf := make([]byte, len(value)+extra)
		n, errno := x.node.Getxattr(ctx, "user.v", buf)
		if errno != 0 {
			t.Fatalf("getxattr into a buffer of %d bytes: errno %v", len(buf), errno)
		}
		if int(n) != len(value) {
			t.Errorf("getxattr into a buffer of %d bytes reported %d, want %d", len(buf), n, len(value))
		}
		if !bytes.Equal(buf[:n], value) {
			t.Errorf("getxattr returned %q, want %q", buf[:n], value)
		}
	}
}

// TestXattrNamesDifferingOnlyInCaseAreDistinct is the collision S3's wire format creates, checked where
// it actually happens: at the endpoint.
//
// S3 lower-cases user-metadata keys in transit. Storing an attribute under its own name would make
// user.Foo and user.foo the same key, so setting the second would destroy the first with no error
// anywhere — the encoding test in internal/vfs asserts the encoder's property, and this asserts the
// consequence survives a real HTTP round trip through a real endpoint.
func TestXattrNamesDifferingOnlyInCaseAreDistinct(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "case.dat")
	ctx := context.Background()

	if errno := x.node.Setxattr(ctx, "user.foo", []byte("lower"), 0); errno != 0 {
		t.Fatalf("setxattr user.foo: errno %v", errno)
	}
	if errno := x.node.Setxattr(ctx, "user.Foo", []byte("upper"), 0); errno != 0 {
		t.Fatalf("setxattr user.Foo: errno %v", errno)
	}

	x.remount(t)

	for _, tc := range []struct{ name, want string }{
		{"user.foo", "lower"},
		{"user.Foo", "upper"},
	} {
		got, errno := x.get(t, tc.name)
		if errno != 0 {
			t.Fatalf("getxattr %s after a remount: errno %v", tc.name, errno)
		}
		if string(got) != tc.want {
			t.Errorf("%s reads %q, want %q. The two names collided in storage, so setting one destroyed "+
				"the other — and neither setfattr reported anything.", tc.name, got, tc.want)
		}
	}

	if names := x.list(t); len(names) != 2 {
		t.Errorf("listxattr reports %v, want both user.Foo and user.foo", names)
	}
}

// TestSetxattrCannotRewriteThePOSIXAttributes is the security-relevant case.
//
// The stored key for an attribute is objectfs-xattr-<base32>, so no name a caller can choose produces
// objectfs-mode. Without that, `setfattr -n objectfs-mode -v 4777 f` would rewrite the file's permission
// bits — an unprivileged process editing the field the filesystem reports as the mode, on a filesystem
// where the mode is the only POSIX-visible access control there is.
//
// Checked against storage rather than against the encoder, because the property has to hold through the
// merge SetObjectMetadata performs: the POSIX keys and the xattr keys land in the same metadata map.
func TestSetxattrCannotRewriteThePOSIXAttributes(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "collide.dat")
	ctx := context.Background()

	// Establish a known mode and owner first, so a change is visible as a change.
	in := setAttrIn(fuse.FATTR_MODE|fuse.FATTR_UID, func(in *fuse.SetAttrIn) {
		in.Mode, in.Uid = 0o600, 4242
	})
	if errno := x.node.Setattr(ctx, nil, in, &fuse.AttrOut{}); errno != 0 {
		t.Fatalf("chmod+chown: errno %v", errno)
	}

	attacks := []struct{ name, value string }{
		{"objectfs-mode", "4777"},
		{"objectfs-uid", "0"},
		{"objectfs-gid", "0"},
		{"objectfs-sha256", strings.Repeat("f", 64)},
		{"objectfs-original-size", "0"},
		{"user.objectfs-mode", "4777"},
		{"OBJECTFS-MODE", "4777"},
	}

	for _, a := range attacks {
		if errno := x.node.Setxattr(ctx, a.name, []byte(a.value), 0); errno != 0 {
			t.Fatalf("setxattr %s: errno %v (the attribute should be storable — it just must not reach "+
				"the POSIX keys)", a.name, errno)
		}
	}

	x.remount(t)

	var out fuse.AttrOut
	if errno := x.node.Getattr(ctx, nil, &out); errno != 0 {
		t.Fatalf("Getattr: errno %v", errno)
	}

	if got := out.Mode & 0o7777; got != 0o600 {
		t.Errorf("the file's mode is %#o after setting an attribute named objectfs-mode, want 0600. An "+
			"unprivileged setfattr just changed a file's permissions.", got)
	}
	if out.Uid != 4242 {
		t.Errorf("the file's owner is %d after setting an attribute named objectfs-uid, want 4242", out.Uid)
	}

	// The attributes themselves are still ordinary attributes, readable under the names they were set
	// with. Refusing them would also be defensible, but silently dropping them would not.
	for _, a := range attacks {
		got, errno := x.get(t, a.name)
		if errno != 0 {
			t.Errorf("the attribute named %q did not survive: errno %v", a.name, errno)
			continue
		}
		if string(got) != a.value {
			t.Errorf("the attribute named %q reads %q, want %q", a.name, got, a.value)
		}
	}

	// And the integrity keys the backend owns are still the backend's. An attribute named
	// objectfs-sha256 must not have become the object's recorded checksum.
	meta := x.srv.ObjectMetadata(x.key)
	if got := meta["objectfs-sha256"]; got == strings.Repeat("f", 64) {
		t.Error("the object's objectfs-sha256 is the value passed to setxattr, so a caller can declare " +
			"any checksum it likes for content it did not write")
	}
}

// TestSetxattrStoresArbitraryBytes covers the binary-value case, through a real HTTP request.
//
// An xattr value is bytes: `setfattr -e hex` exists because people store them, and a value carrying a
// NUL or a newline cannot go into an HTTP header as itself. The encoding is tested in internal/vfs; what
// this adds is that the encoded form survives the SDK, the signer, and the endpoint — the layers a unit
// test over the encoder cannot reach.
func TestSetxattrStoresArbitraryBytes(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "binary.dat")
	ctx := context.Background()

	values := map[string][]byte{
		"user.nul":     {0, 1, 2, 0},
		"user.newline": []byte("line\r\nline"),
		"user.highbit": {0xff, 0xfe, 0x80},
		"user.empty":   {},
		"user.spaces":  []byte("  leading and trailing  "),
	}

	for name, value := range values {
		if errno := x.node.Setxattr(ctx, name, value, 0); errno != 0 {
			t.Fatalf("setxattr %s = %v: errno %v", name, value, errno)
		}
	}

	x.remount(t)

	for name, want := range values {
		got, errno := x.get(t, name)
		if errno != 0 {
			t.Errorf("getxattr %s after a remount: errno %v", name, errno)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s reads %v, want %v", name, got, want)
		}
	}

	// An empty value is an attribute that exists, distinct from one that was removed. Both are the empty
	// string in an untagged storage form, and conflating them would make every `setfattr -n user.x` with
	// no value read as a removal.
	if names := x.list(t); len(names) != len(values) {
		t.Errorf("listxattr reports %d attributes, want %d: an empty attribute exists and must be listed",
			len(names), len(values))
	}
}

// TestSetxattrOfANilValueStoresAnEmptyAttribute closes the gap between two representations of "nothing"
// that mean opposite things.
//
// A nil value is the tombstone in the stored form, and [vfs.Writer.SetXattr] rejects nil for exactly that
// reason — passing it through would make a set indistinguishable from a removal. But a *caller* handing
// over nil means the empty value, which is what `setfattr -n user.x f` with no `-v` sets, so it has to be
// normalised at this boundary rather than refused at the one below.
//
// This test exists because a verifying mutation found nothing covering it: removing the normalisation
// left every other test in this package passing, while `setfattr -n user.x f` returned EINVAL and stored
// nothing. The `[]byte{}` cases elsewhere do not reach it — an empty slice is not a nil one, and that
// distinction is the entire defect.
func TestSetxattrOfANilValueStoresAnEmptyAttribute(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "nilvalue.dat")
	ctx := context.Background()

	if errno := x.node.Setxattr(ctx, "user.novalue", nil, 0); errno != 0 {
		t.Fatalf("setxattr with a nil value returned %s, want success. That is `setfattr -n user.x f` "+
			"with no -v, which sets an attribute whose value is zero bytes.", errnoName(errno))
	}

	x.remount(t)

	got, errno := x.get(t, "user.novalue")
	if errno != 0 {
		t.Fatalf("the attribute is absent after a nil-valued set: errno %s. A nil value is the tombstone "+
			"in the stored form, so passing it through unnormalised turns a set into a removal.",
			errnoName(errno))
	}
	if len(got) != 0 {
		t.Errorf("the attribute reads %q, want an empty value", got)
	}

	// It exists, which is the half a tombstone would get wrong.
	var found bool
	for _, name := range x.list(t) {
		if name == "user.novalue" {
			found = true
		}
	}
	if !found {
		t.Error("listxattr does not name the attribute, so it was stored as a removal rather than as an " +
			"empty value")
	}
}

// TestSetxattrHonorsCreateAndReplaceFlags covers the two flags, and the platform difference in their
// values.
//
// XATTR_CREATE and XATTR_REPLACE are numerically different on Linux and darwin (1 and 2 versus 2 and 4),
// and the value that arrives is the local kernel's. A literal in the implementation would be right on one
// platform and would silently turn CREATE into REPLACE on the other, which is why unix.XATTR_* is used —
// and why this test names them the same way rather than hardcoding what it expects to receive.
func TestSetxattrHonorsCreateAndReplaceFlags(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "flags.dat")
	ctx := context.Background()

	// REPLACE on an attribute that does not exist: ENOATTR.
	if errno := x.node.Setxattr(ctx, "user.f", []byte("v"), unix.XATTR_REPLACE); errno != errNoXattr {
		t.Errorf("setxattr with XATTR_REPLACE on a missing attribute returned %s, want %s",
			errnoName(errno), errnoName(errNoXattr))
	}
	if _, errno := x.get(t, "user.f"); errno != errNoXattr {
		t.Error("a refused XATTR_REPLACE created the attribute anyway")
	}

	// CREATE on an attribute that does not exist: success.
	if errno := x.node.Setxattr(ctx, "user.f", []byte("first"), unix.XATTR_CREATE); errno != 0 {
		t.Fatalf("setxattr with XATTR_CREATE on a missing attribute: errno %v", errno)
	}

	// CREATE on one that now does: EEXIST, and the value must not change.
	if errno := x.node.Setxattr(ctx, "user.f", []byte("second"), unix.XATTR_CREATE); errno != syscall.EEXIST {
		t.Errorf("setxattr with XATTR_CREATE on an existing attribute returned %s, want EEXIST",
			errnoName(errno))
	}
	if got, _ := x.get(t, "user.f"); string(got) != "first" {
		t.Errorf("a refused XATTR_CREATE changed the value to %q, want %q", got, "first")
	}

	// REPLACE on one that exists: success.
	if errno := x.node.Setxattr(ctx, "user.f", []byte("second"), unix.XATTR_REPLACE); errno != 0 {
		t.Fatalf("setxattr with XATTR_REPLACE on an existing attribute: errno %v", errno)
	}
	if got, _ := x.get(t, "user.f"); string(got) != "second" {
		t.Errorf("XATTR_REPLACE left the value as %q, want %q", got, "second")
	}

	// Both flags at once is EINVAL, per setxattr(2).
	both := uint32(unix.XATTR_CREATE | unix.XATTR_REPLACE)
	if errno := x.node.Setxattr(ctx, "user.g", []byte("v"), both); errno != syscall.EINVAL {
		t.Errorf("setxattr with both XATTR_CREATE and XATTR_REPLACE returned %s, want EINVAL",
			errnoName(errno))
	}

	// The two constants must not be the same number, or every test above is checking one code path
	// twice. They differ per platform, which is the reason this is asserted rather than assumed.
	if unix.XATTR_CREATE == unix.XATTR_REPLACE {
		t.Fatal("unix.XATTR_CREATE and unix.XATTR_REPLACE are the same value on this platform, so the " +
			"create and replace cases above are indistinguishable")
	}
}

// TestSetxattrRefusesValuesLargerThanTheMetadataBudget covers the two size failures and, more
// importantly, that they are told apart.
//
// S3 caps an object's total user metadata at 2 KB and fails the request rather than truncating. E2BIG
// says no object could hold this value; ENOSPC says this one has no room left. A caller that retries
// after freeing space is right in the second case and loops forever in the first, which is why they are
// separate errnos in setxattr(2) and why collapsing them here would discard information the syscall
// interface can carry.
func TestSetxattrRefusesValuesLargerThanTheMetadataBudget(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "budget.dat")
	ctx := context.Background()

	// One value larger than the entire budget.
	huge := bytes.Repeat([]byte("x"), vfs.XattrBudget+1)
	if errno := x.node.Setxattr(ctx, "user.huge", huge, 0); errno != syscall.E2BIG {
		t.Errorf("setxattr of a %d-byte value returned %s, want E2BIG. S3 caps an object's user "+
			"metadata at 2 KB and fails the whole request, so accepting this would move the failure to a "+
			"later flush that no caller is left to see.", len(huge), errnoName(errno))
	}

	// Enough attributes that each fits and the set does not. The count is derived from the budget rather
	// than written as a literal, so a change to what ObjectFS reserves cannot make this vacuous.
	chunk := bytes.Repeat([]byte("y"), 128)
	var filled bool
	for i := range 100 {
		name := "user.fill" + string(rune('a'+i%26)) + string(rune('a'+i/26))

		errno := x.node.Setxattr(ctx, name, chunk, 0)
		if errno == 0 {
			continue
		}
		if errno == syscall.ENOSPC {
			filled = true

			break
		}
		t.Fatalf("setxattr %s returned %s, want either success or ENOSPC", name, errnoName(errno))
	}
	if !filled {
		t.Errorf("100 attributes of %d bytes each fit inside a %d-byte budget without ENOSPC; the limit "+
			"is not being enforced", len(chunk), vfs.XattrBudget)
	}

	// The object is still intact and still readable. A refused setxattr must not have left the file
	// dirty with a change it cannot persist, or the next flush of any kind fails.
	if got := x.srv.ObjectSize(x.key); got != 4096 {
		t.Errorf("the object is %d bytes after a refused setxattr, want 4096", got)
	}
}

// TestXattrsInKernelActedNamespacesAreRefused is a privilege-escalation boundary, not a compatibility
// choice.
//
// On Linux the kernel reads security.capability from the filesystem on every exec and grants the named
// capabilities. The store behind this filesystem is object metadata, which anyone holding s3:PutObject on
// the bucket can write with the AWS CLI without touching the mount — so honoring the attribute would turn
// bucket write access into a route to file capabilities on every host that mounts it. system.posix_acl_*
// is the milder version: go-fuse is not configured for ACLs, so a stored ACL would be reported by getfacl
// and enforced by nothing.
func TestXattrsInKernelActedNamespacesAreRefused(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "namespaces.dat")
	ctx := context.Background()

	refused := []string{
		"security.capability",
		"security.selinux",
		"system.posix_acl_access",
		"system.posix_acl_default",
	}

	for _, name := range refused {
		if errno := x.node.Setxattr(ctx, name, []byte("\x01\x00\x00\x02"), 0); errno != syscall.ENOTSUP {
			t.Errorf("setxattr %s returned %s, want ENOTSUP. Object metadata is writable by anyone with "+
				"bucket write access, so an attribute the kernel acts on must not be storable here.",
				name, errnoName(errno))
		}
		if _, errno := x.node.Getxattr(ctx, name, nil); errno != errNoXattr {
			t.Errorf("getxattr %s returned %s, want %s", name, errnoName(errno), errnoName(errNoXattr))
		}
	}

	// Nothing reached the object.
	for k := range x.srv.ObjectMetadata(x.key) {
		if strings.HasPrefix(strings.ToLower(k), "objectfs-xattr-") {
			t.Errorf("the object carries an extended attribute key %q after only refused sets", k)
		}
	}

	// A name that merely looks like one of those namespaces is an ordinary attribute: the kernel's
	// namespaces are lower-case, and no kernel reads Security.capability, so refusing it would deny a
	// legitimate name for no benefit.
	if errno := x.node.Setxattr(ctx, "Security.capability", []byte("ordinary"), 0); errno != 0 {
		t.Errorf("setxattr of the ordinary name Security.capability returned %s, want success",
			errnoName(errno))
	}
	if got, errno := x.get(t, "Security.capability"); errno != 0 || string(got) != "ordinary" {
		t.Errorf("Security.capability reads %q with errno %v, want %q", got, errno, "ordinary")
	}
}

// The stored form of security.capability = {1,0,0,2}, computed independently of ObjectFS's encoder.
//
// Written out as literals because that is what an out-of-band writer has: base32 of the name under the
// objectfs-xattr- prefix, lower-cased as S3 would deliver it, and "v" plus base64url of the value. If the
// encoding changes these stop matching, and the test fails at the "did the write land" assertion rather
// than quietly checking a filter against a key ObjectFS no longer looks for.
const (
	outOfBandXattrKey   = "objectfs-xattr-onswg5lsnf2hsltdmfygcytjnruxi6i"
	outOfBandXattrValue = "vAQAAAg"
)

// TestARefusedNamespaceWrittenOutsideTheMountIsNotReported is the other half of the namespace refusal,
// and the half that carries the actual privilege argument.
//
// Refusing `setfattr security.capability` only closes the door a mount controls. The threat the refusal
// exists for is the door it does not: anyone holding s3:PutObject on the bucket can write the encoded
// metadata key with the AWS CLI, never touching a mount. If getxattr and listxattr then reported that
// attribute, the kernel would read it on the next exec and grant the capabilities it names — so the read
// side has to filter too, and the write-side refusal alone would be security theater.
//
// This test exists because a verifying mutation found nothing covering it: deleting the filter from
// Listxattr left every other test in this package passing, because every attribute in them was written
// *through* the mount, where the set path had already refused it. The out-of-band write is the only way
// to reach the branch.
func TestARefusedNamespaceWrittenOutsideTheMountIsNotReported(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "outofband.dat")
	ctx := context.Background()

	// One ordinary attribute through the mount, so the object has a legitimate one alongside.
	if errno := x.node.Setxattr(ctx, "user.ok", []byte("fine"), 0); errno != 0 {
		t.Fatalf("setxattr: errno %v", errno)
	}

	// Now the out-of-band write. The key and value are computed here rather than by calling ObjectFS's
	// encoder, deliberately: an attacker with bucket credentials does not call ObjectFS's functions
	// either, and a test that shared the encoder would agree with it by construction — the same reason
	// this package prefers a real endpoint to a mock. If the encoding ever changes, the assertion below
	// that the object really carries the attribute fails loudly rather than passing vacuously.
	meta := x.srv.ObjectMetadata(x.key)
	meta[outOfBandXattrKey] = outOfBandXattrValue

	if _, err := x.srv.Client().CopyObject(ctx, &awss3.CopyObjectInput{
		Bucket:            aws.String(x.srv.Bucket),
		Key:               aws.String(x.key),
		CopySource:        aws.String(x.srv.Bucket + "/" + x.key),
		Metadata:          meta,
		MetadataDirective: awstypes.MetadataDirectiveReplace,
	}); err != nil {
		t.Fatalf("writing the attribute out of band: %v", err)
	}

	x.remount(t)

	// The object really does carry it, or this test is asserting nothing.
	var carried bool
	for k := range x.srv.ObjectMetadata(x.key) {
		if strings.EqualFold(k, outOfBandXattrKey) {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("the out-of-band write did not land, so this test cannot check the filter. Metadata: %v",
			x.srv.ObjectMetadata(x.key))
	}

	// And ObjectFS reports nothing about it.
	for _, name := range x.list(t) {
		if name == "security.capability" {
			t.Error("listxattr names security.capability for an attribute written directly to the " +
				"bucket. The kernel reads that attribute on every exec and grants the capabilities it " +
				"names, so anyone with s3:PutObject would gain file capabilities on every mounting host — " +
				"refusing it only on the write path leaves the route that matters wide open.")
		}
	}

	if _, errno := x.node.Getxattr(ctx, "security.capability", nil); errno != errNoXattr {
		t.Errorf("getxattr for the out-of-band security.capability returned %s, want %s",
			errnoName(errno), errnoName(errNoXattr))
	}

	// The legitimate attribute is untouched, so the filter is a filter and not a blanket refusal.
	if got, errno := x.get(t, "user.ok"); errno != 0 || string(got) != "fine" {
		t.Errorf("the ordinary attribute reads %q with errno %v, want %q", got, errno, "fine")
	}
}

// TestXattrOperationsAreRefusedOnAReadOnlyMount pins the read-only guard on the two write paths.
func TestXattrOperationsAreRefusedOnAReadOnlyMount(t *testing.T) {
	t.Parallel()

	filesystem := NewFileSystem(t.Context(), nil, nil, nil, nil, &Config{ReadOnly: true})
	file := &FileNode{fs: filesystem, path: "ro.dat"}
	dir := &DirectoryNode{fs: filesystem, path: "ro"}
	ctx := context.Background()

	if errno := file.Setxattr(ctx, "user.x", []byte("v"), 0); errno != syscall.EROFS {
		t.Errorf("setxattr on a read-only mount returned %s, want EROFS", errnoName(errno))
	}
	if errno := file.Removexattr(ctx, "user.x"); errno != syscall.EROFS {
		t.Errorf("removexattr on a read-only mount returned %s, want EROFS", errnoName(errno))
	}
	if errno := dir.Setxattr(ctx, "user.x", []byte("v"), 0); errno != syscall.EROFS {
		t.Errorf("setxattr on a directory of a read-only mount returned %s, want EROFS", errnoName(errno))
	}
	if errno := dir.Removexattr(ctx, "user.x"); errno != syscall.EROFS {
		t.Errorf("removexattr on a directory of a read-only mount returned %s, want EROFS",
			errnoName(errno))
	}
}

// TestDirectoryXattrsRefuseRatherThanDiscard records the decision for directories.
//
// A directory here is a key prefix, not an object, so there is nothing to hold an attribute — the same
// reason [DirectoryNode.Setattr] refuses chmod. Mkdir does write a marker object, which is why this is
// unimplemented rather than impossible; what makes refusing right is that a directory existing only
// because objects share a prefix has no marker, so an attribute would persist for some directories and
// vanish for others. Reporting success and storing nothing is the defect shape this project treats as
// worse than an error.
//
// The two errnos differ deliberately, and the split is the interesting part: a set refuses (ENOTSUP,
// "this filesystem cannot") while a get, a list, and a remove answer accurately about a file that has no
// attributes (ENOATTR, and an empty list).
func TestDirectoryXattrsRefuseRatherThanDiscard(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)
	dir := &DirectoryNode{fs: f.fs, path: "some/dir"}
	ctx := context.Background()

	if errno := dir.Setxattr(ctx, "user.x", []byte("v"), 0); errno != syscall.ENOTSUP {
		t.Errorf("setxattr on a directory returned %s, want ENOTSUP. Reporting success for an attribute "+
			"the next getfattr would not show is the same defect as an rm that reports success while the "+
			"object survives.", errnoName(errno))
	}

	if _, errno := dir.Getxattr(ctx, "user.x", nil); errno != errNoXattr {
		t.Errorf("getxattr on a directory returned %s, want %s", errnoName(errno), errnoName(errNoXattr))
	}

	if errno := dir.Removexattr(ctx, "user.x"); errno != errNoXattr {
		t.Errorf("removexattr on a directory returned %s, want %s",
			errnoName(errno), errnoName(errNoXattr))
	}

	// Listing succeeds with nothing in it. Failing would make `cp -a`, `rsync -X`, and `ls -@` report an
	// error for every directory they walk, over an answer that is simply "there are none".
	size, errno := dir.Listxattr(ctx, nil)
	if errno != 0 {
		t.Errorf("listxattr on a directory returned %s, want success with an empty list", errnoName(errno))
	}
	if size != 0 {
		t.Errorf("listxattr on a directory reported %d bytes of names", size)
	}
}

// TestXattrOperationsAreDispatchedRatherThanDefaulted is the counterpart to unimplemented_test.go's
// table, for the four rows that left it.
//
// Every one of these operations is an optional interface go-fuse probes with a type assertion, and the
// defaults are exactly the answers ObjectFS used to give: ENOATTR for get, set, and remove, and OK with
// an empty list for list. So a signature that drifted on a go-fuse bump — or a build tag that excluded
// xattr.go on some platform — would take the whole feature back to "no such attribute" with no compile
// error and no test failure anywhere else. xattr.go has compile-time assertions for the interfaces; this
// asserts the consequence, that the bridge dispatches to them.
//
// EROFS is the expected answer because the fixture is a read-only mount with no backend: it pins that the
// call arrived at ObjectFS's own code, which is the one thing the interface assertion cannot check.
func TestXattrOperationsAreDispatchedRatherThanDefaulted(t *testing.T) {
	t.Parallel()

	filesystem := NewFileSystem(t.Context(), nil, nil, nil, nil, &Config{ReadOnly: true})
	raw := gofuse.NewNodeFS(filesystem.Root(), &gofuse.Options{})

	in := &fuse.SetXAttrIn{InHeader: rootHeader()}
	if got := syscall.Errno(raw.SetXAttr(nil, in, "user.test", []byte("value"))); got != syscall.EROFS {
		t.Errorf("SetXAttr through the bridge returned %s, want EROFS. %s is go-fuse's default for an "+
			"absent NodeSetxattrer, so reaching it means the interface is no longer satisfied and every "+
			"setfattr on the mount is silently refused again.", errnoName(got), errnoName(errNoXattr))
	}

	h := rootHeader()
	if got := syscall.Errno(raw.RemoveXAttr(nil, &h, "user.test")); got != syscall.EROFS {
		t.Errorf("RemoveXAttr through the bridge returned %s, want EROFS", errnoName(got))
	}
}

// TestXattrsSurviveAContentWrite is the interaction between the two flush paths.
//
// A PutObject writes user metadata wholesale, so a content write that rendered only the POSIX attributes
// would delete every extended attribute the file had — silently, on the next ordinary `echo >> file`.
// This is the same defect shape as the write path chowning a file to root, one field over.
func TestXattrsSurviveAContentWrite(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "content.dat")
	ctx := context.Background()

	if errno := x.node.Setxattr(ctx, "user.keep", []byte("through a write"), 0); errno != 0 {
		t.Fatalf("setxattr: errno %v", errno)
	}

	// A content write, flushed the way Setattr flushes: this takes the PutObject path, not the metadata
	// replace, because the plan is no longer a no-op.
	if err := x.fs.buffer.Write(x.key, 0, []byte("REPLACED")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := x.fs.buffer.FlushReportingETag(ctx, x.key); err != nil {
		t.Fatalf("flush: %v", err)
	}

	x.remount(t)

	got, errno := x.get(t, "user.keep")
	if errno != 0 {
		t.Fatalf("the attribute is gone after a content write: errno %v. A PUT replaces user metadata "+
			"wholesale, so a write path that did not carry the attributes forward would delete them on "+
			"every ordinary write.", errno)
	}
	if string(got) != "through a write" {
		t.Errorf("the attribute reads %q after a content write, want %q", got, "through a write")
	}

	// The content went up too, so this is not passing because the write was dropped.
	stored, err := x.fs.backend.GetObject(ctx, x.key, 0, 8)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if string(stored) != "REPLACED" {
		t.Errorf("the object begins %q, want %q", stored, "REPLACED")
	}
}

// TestXattrReadsGoThroughTheWritePathBeforeStorage covers read-after-write on the same node.
//
// [FileNode.attr] asks the write path first and storage second, and it must: a getxattr that consulted
// only S3 would report the previous value for as long as the metadata cache entry lived, which is the
// read-after-write defect the read path already had for content in v0.10.0.
func TestXattrReadsGoThroughTheWritePathBeforeStorage(t *testing.T) {
	t.Parallel()

	x := newXattrFixture(t, "raw.dat")
	ctx := context.Background()

	if errno := x.node.Setxattr(ctx, "user.v", []byte("first"), 0); errno != 0 {
		t.Fatalf("first setxattr: errno %v", errno)
	}
	if errno := x.node.Setxattr(ctx, "user.v", []byte("second"), 0); errno != 0 {
		t.Fatalf("second setxattr: errno %v", errno)
	}

	got, errno := x.get(t, "user.v")
	if errno != 0 {
		t.Fatalf("getxattr: errno %v", errno)
	}
	if string(got) != "second" {
		t.Errorf("getxattr immediately after an overwrite reads %q, want %q — a read that consulted only "+
			"storage, or a stale cache entry, would report the previous value", got, "second")
	}
}
