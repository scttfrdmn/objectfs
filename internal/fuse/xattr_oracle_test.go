//go:build linux || darwin

package fuse

// A differential oracle for extended attributes: one sequence of operations run against ObjectFS and
// against the local operating-system filesystem, asserting the two agree.
//
// internal/difftest is the same idea for content, and this is deliberately not built on it — that
// package's FS interface is content-shaped (WriteAt, ReadAt, Truncate, Durable) and its doc.go states
// that it "does not compare errno values, permissions, timestamps, or anything about directories".
// Extended attributes are almost entirely errno and naming semantics, so widening that interface would
// change what the package is for. What is worth borrowing is the *reason* it exists: the reference is
// not written by the person who wrote the implementation and has no opinion about ObjectFS's internals,
// so it cannot agree by construction the way a table of expected errnos can.
//
// That risk is concrete here. Every errno below was chosen by reading setxattr(2), and a table of them
// would pass whether or not the man page was read correctly — the table would simply restate the same
// belief the implementation encodes. macOS and Linux also disagree about the *name* of the answer for a
// missing attribute, so a hardcoded constant is wrong on one platform by construction. Asking the
// kernel is the only reference that is independent of both.
//
// # What is compared, and what is not
//
// Compared: the errno for every operation, the value read back, the presence of a name in a listing,
// and the size protocol's answers. Not compared: the *set* of names a listing returns, because the OS
// adds its own (macOS puts com.apple.provenance on every file it creates), and the size limits, because
// ObjectFS's are far smaller and deliberately so. Where ObjectFS refuses something the OS accepts —
// security.*, a 100 KB value, a directory — the divergence is asserted as intended rather than
// smoothed over, with the reason recorded in the case.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// osXattrs is the reference: the local filesystem, through the real syscalls.
type osXattrs struct {
	path string
}

// newOSXattrs creates a file on the local filesystem to hold attributes.
//
// t.TempDir is on whatever filesystem /tmp is, which is APFS on macOS and usually ext4 or tmpfs on
// Linux. All three support user extended attributes; [requireOSXattrs] establishes that rather than
// assuming it, because a filesystem mounted with user_xattr off would make every comparison below
// vacuously agree on ENOTSUP.
func newOSXattrs(t *testing.T) *osXattrs {
	t.Helper()

	path := filepath.Join(t.TempDir(), "oracle.dat")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("creating the reference file: %v", err)
	}

	return &osXattrs{path: path}
}

// requireOSXattrs skips when the local filesystem cannot store an extended attribute at all.
//
// Without this, a filesystem with user_xattr disabled would answer ENOTSUP to everything and the
// comparisons would agree with an ObjectFS that also refused everything — the exact false pass this
// file exists to avoid.
func (o *osXattrs) requireOSXattrs(t *testing.T) {
	t.Helper()

	if err := unix.Setxattr(o.path, "user.probe", []byte("v"), 0); err != nil {
		t.Skipf("the local filesystem cannot store extended attributes (%v), so it cannot serve as a "+
			"reference. This is a skip rather than a failure: the oracle is unavailable, and ObjectFS's "+
			"own behavior is covered by xattr_test.go.", err)
	}
	if err := unix.Removexattr(o.path, "user.probe"); err != nil {
		t.Skipf("the local filesystem stored an attribute but cannot remove it (%v)", err)
	}
}

func (o *osXattrs) set(name string, value []byte, flags uint32) syscall.Errno {
	return asErrno(unix.Setxattr(o.path, name, value, int(flags)))
}

func (o *osXattrs) remove(name string) syscall.Errno {
	return asErrno(unix.Removexattr(o.path, name))
}

// get reads an attribute, following the same two-call protocol a caller uses.
//
// The syscall reports -1 for the size on failure, where a FUSE reply carries the real size alongside
// ERANGE; that difference is libc's, not the filesystem's, so only the errno and the bytes are
// compared. [TestGetxattrHonorsTheSizeProtocol] covers ObjectFS's size on the FUSE side.
func (o *osXattrs) get(name string) ([]byte, syscall.Errno) {
	size, err := unix.Getxattr(o.path, name, nil)
	if err != nil {
		return nil, asErrno(err)
	}

	buf := make([]byte, size)
	n, err := unix.Getxattr(o.path, name, buf)
	if err != nil {
		return nil, asErrno(err)
	}

	return buf[:n], 0
}

// has reports whether a name appears in the local filesystem's listing.
func (o *osXattrs) has(t *testing.T, name string) bool {
	t.Helper()

	size, err := unix.Listxattr(o.path, nil)
	if err != nil {
		t.Fatalf("listxattr on the reference file: %v", err)
	}

	buf := make([]byte, size)
	n, err := unix.Listxattr(o.path, buf)
	if err != nil {
		t.Fatalf("listxattr on the reference file: %v", err)
	}

	return slices.Contains(splitNulList(buf[:n]), name)
}

// splitNulList parses the NUL-terminated name list both implementations produce.
func splitNulList(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}

	return out
}

// asErrno extracts the errno from a syscall error, so a comparison is between two errnos rather than
// between an errno and a string.
func asErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}

	return syscall.EIO
}

// TestXattrErrnosAgreeWithTheLocalFilesystem runs one sequence against both and compares every answer.
//
// The sequence is ordered deliberately: each step's answer depends on the state the previous steps
// left, so a divergence in one operation shows up in the next as well. It is the flag interactions that
// make this worth doing — "REPLACE on a missing attribute is ENOATTR, CREATE on an existing one is
// EEXIST, both together is EINVAL" is three chances to have read the man page backwards, and the kernel
// settles each one without being asked what the answer should be.
func TestXattrErrnosAgreeWithTheLocalFilesystem(t *testing.T) {
	t.Parallel()

	oracle := newOSXattrs(t)
	oracle.requireOSXattrs(t)

	x := newXattrFixture(t, "oracle.dat")
	ctx := context.Background()

	steps := []struct {
		what string

		// objectfs and reference perform the same operation on each implementation.
		objectfs  func() syscall.Errno
		reference func() syscall.Errno
	}{
		{
			what:      "getxattr for an attribute that was never set",
			objectfs:  func() syscall.Errno { _, e := x.get(t, "user.a"); return e },
			reference: func() syscall.Errno { _, e := oracle.get("user.a"); return e },
		},
		{
			what:      "removexattr for an attribute that was never set",
			objectfs:  func() syscall.Errno { return x.node.Removexattr(ctx, "user.a") },
			reference: func() syscall.Errno { return oracle.remove("user.a") },
		},
		{
			what: "setxattr with XATTR_REPLACE on a missing attribute",
			objectfs: func() syscall.Errno {
				return x.node.Setxattr(ctx, "user.a", []byte("v"), unix.XATTR_REPLACE)
			},
			reference: func() syscall.Errno {
				return oracle.set("user.a", []byte("v"), unix.XATTR_REPLACE)
			},
		},
		{
			what: "setxattr with XATTR_CREATE on a missing attribute",
			objectfs: func() syscall.Errno {
				return x.node.Setxattr(ctx, "user.a", []byte("first"), unix.XATTR_CREATE)
			},
			reference: func() syscall.Errno {
				return oracle.set("user.a", []byte("first"), unix.XATTR_CREATE)
			},
		},
		{
			what: "setxattr with XATTR_CREATE on an attribute that now exists",
			objectfs: func() syscall.Errno {
				return x.node.Setxattr(ctx, "user.a", []byte("second"), unix.XATTR_CREATE)
			},
			reference: func() syscall.Errno {
				return oracle.set("user.a", []byte("second"), unix.XATTR_CREATE)
			},
		},
		{
			what: "setxattr with XATTR_REPLACE on an attribute that exists",
			objectfs: func() syscall.Errno {
				return x.node.Setxattr(ctx, "user.a", []byte("second"), unix.XATTR_REPLACE)
			},
			reference: func() syscall.Errno {
				return oracle.set("user.a", []byte("second"), unix.XATTR_REPLACE)
			},
		},
		{
			// The case the two kernels answer differently, which is why it is here and not only in a unit
			// test: darwin returns EINVAL, linux runs the REPLACE arm and returns ENODATA. An assertion
			// written by hand would have picked one. user.b does not exist at this point.
			what: "setxattr naming both XATTR_CREATE and XATTR_REPLACE, attribute missing",
			objectfs: func() syscall.Errno {
				return x.node.Setxattr(ctx, "user.b", []byte("v"),
					uint32(unix.XATTR_CREATE|unix.XATTR_REPLACE))
			},
			reference: func() syscall.Errno {
				return oracle.set("user.b", []byte("v"), uint32(unix.XATTR_CREATE|unix.XATTR_REPLACE))
			},
		},
		{
			// The same flags against an attribute that exists, where the platforms differ in the other
			// direction: EINVAL on darwin again, but EEXIST on linux because there the CREATE arm fires
			// first. Both arms of the divergence are covered, so a fix that satisfied one by hardcoding an
			// errno would fail here. user.a exists by now.
			what: "setxattr naming both flags, attribute present",
			objectfs: func() syscall.Errno {
				return x.node.Setxattr(ctx, "user.a", []byte("v"),
					uint32(unix.XATTR_CREATE|unix.XATTR_REPLACE))
			},
			reference: func() syscall.Errno {
				return oracle.set("user.a", []byte("v"), uint32(unix.XATTR_CREATE|unix.XATTR_REPLACE))
			},
		},
		{
			what:      "setxattr with no flags, overwriting",
			objectfs:  func() syscall.Errno { return x.node.Setxattr(ctx, "user.a", []byte("third"), 0) },
			reference: func() syscall.Errno { return oracle.set("user.a", []byte("third"), 0) },
		},
		{
			what:      "setxattr of an empty value",
			objectfs:  func() syscall.Errno { return x.node.Setxattr(ctx, "user.empty", []byte{}, 0) },
			reference: func() syscall.Errno { return oracle.set("user.empty", []byte{}, 0) },
		},
		{
			what:      "getxattr of that empty value",
			objectfs:  func() syscall.Errno { _, e := x.get(t, "user.empty"); return e },
			reference: func() syscall.Errno { _, e := oracle.get("user.empty"); return e },
		},
		{
			what:      "setxattr of a value holding NUL and newline bytes",
			objectfs:  func() syscall.Errno { return x.node.Setxattr(ctx, "user.bin", binaryValue, 0) },
			reference: func() syscall.Errno { return oracle.set("user.bin", binaryValue, 0) },
		},
		{
			what:      "removexattr of an attribute that exists",
			objectfs:  func() syscall.Errno { return x.node.Removexattr(ctx, "user.a") },
			reference: func() syscall.Errno { return oracle.remove("user.a") },
		},
		{
			what:      "getxattr after that removal",
			objectfs:  func() syscall.Errno { _, e := x.get(t, "user.a"); return e },
			reference: func() syscall.Errno { _, e := oracle.get("user.a"); return e },
		},
		{
			what:      "removexattr again, of the attribute just removed",
			objectfs:  func() syscall.Errno { return x.node.Removexattr(ctx, "user.a") },
			reference: func() syscall.Errno { return oracle.remove("user.a") },
		},
	}

	for _, step := range steps {
		// Not subtests: the sequence is stateful, so these have to run in order and cannot be parallel.
		got, want := step.objectfs(), step.reference()

		if got != want {
			t.Errorf("%s:\n\tObjectFS returned %s\n\tthe local filesystem returned %s\n"+
				"These are the same operation on the same state. A caller that branches on the errno — "+
				"and every caller of setxattr does, which is why the flags exist — behaves differently "+
				"on this filesystem than on a local one.",
				step.what, errnoName(got), errnoName(want))
		}
	}

	// The values agree too, not just the errnos. An implementation that returned the right errno and the
	// wrong bytes would pass everything above.
	for _, name := range []string{"user.empty", "user.bin"} {
		gotValue, gotErrno := x.get(t, name)
		wantValue, wantErrno := oracle.get(name)

		if gotErrno != wantErrno {
			t.Errorf("reading %s back: ObjectFS %s, local filesystem %s",
				name, errnoName(gotErrno), errnoName(wantErrno))

			continue
		}
		if string(gotValue) != string(wantValue) {
			t.Errorf("reading %s back: ObjectFS returned %q, the local filesystem returned %q",
				name, gotValue, wantValue)
		}
	}

	// And the listing agrees about the names this test set, without asserting the whole set: macOS puts
	// com.apple.provenance on every file it creates, which is the OS's business and not a divergence.
	objectfsNames := make(map[string]bool)
	for _, n := range x.list(t) {
		objectfsNames[n] = true
	}

	for _, name := range []string{"user.empty", "user.bin"} {
		if want := oracle.has(t, name); objectfsNames[name] != want {
			t.Errorf("listxattr: ObjectFS lists %s = %v, the local filesystem = %v",
				name, objectfsNames[name], want)
		}
	}

	if objectfsNames["user.a"] {
		t.Error("listxattr still names user.a after it was removed, while the local filesystem does not")
	}
}

// binaryValue holds the bytes an HTTP header cannot carry as themselves, which is why values are
// encoded. The local filesystem stores them without complaint, so it is a real reference for what a
// caller may expect to get back.
var binaryValue = []byte{0x00, 0x0a, 0x0d, 0x20, 0x7f, 0xff, 0x00}

// TestXattrDivergencesFromTheLocalFilesystemAreIntentional records where the two deliberately disagree.
//
// A differential oracle is only usable if the intended divergences are written down, because otherwise
// every one of them is a failure and the test gets deleted. Each case below is something the local
// filesystem accepts and ObjectFS refuses, with the reason — and asserting them makes the refusals
// deliberate rather than incidental, so removing one is a test failure rather than a quiet change of
// behavior.
func TestXattrDivergencesFromTheLocalFilesystemAreIntentional(t *testing.T) {
	t.Parallel()

	oracle := newOSXattrs(t)
	oracle.requireOSXattrs(t)

	x := newXattrFixture(t, "divergence.dat")
	ctx := context.Background()

	t.Run("a value larger than the metadata budget", func(t *testing.T) {
		// Parallel with its siblings: each case below asserts a *refusal*, so none of them writes an
		// attribute and there is no shared state for them to race. If a case is ever changed to one that
		// stores something, it has to stop being parallel.
		t.Parallel()

		big := make([]byte, 64*1024)

		// The local filesystem takes it — ext4 allows a value up to one block, APFS far more.
		if errno := oracle.set("user.big", big, 0); errno != 0 {
			t.Skipf("the local filesystem also refuses a %d-byte value (%s), so there is no divergence "+
				"to record here", len(big), errnoName(errno))
		}

		if errno := x.node.Setxattr(ctx, "user.big", big, 0); errno != syscall.E2BIG {
			t.Errorf("ObjectFS returned %s for a %d-byte value, want E2BIG. S3 caps an object's total "+
				"user metadata at 2 KB and rejects the request rather than truncating, so this cannot be "+
				"stored — and E2BIG is what setxattr(2) documents for a value too large for the "+
				"filesystem.", errnoName(errno), len(big))
		}
	})

	t.Run("an attribute in a namespace the kernel acts on", func(t *testing.T) {
		t.Parallel()

		// Not compared against the local filesystem's answer: setting security.capability there needs
		// CAP_SETFCAP and would fail as EPERM for an unrelated reason. The divergence being recorded is
		// that ObjectFS refuses it for *every* caller, privileged or not.
		if errno := x.node.Setxattr(ctx, "security.capability", []byte("\x01\x00\x00\x02"), 0); errno != syscall.ENOTSUP {
			t.Errorf("ObjectFS returned %s for security.capability, want ENOTSUP. A local filesystem "+
				"stores this for a privileged caller and the kernel grants the capabilities it names on "+
				"every exec; the store here is object metadata, writable by anyone with bucket write "+
				"access, so honoring it would convert that into file capabilities on every mounting host.",
				errnoName(errno))
		}
	})

	t.Run("an attribute on a directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		if err := unix.Setxattr(dir, "user.d", []byte("v"), 0); err != nil {
			t.Skipf("the local filesystem also refuses an attribute on a directory (%v)", err)
		}

		node := &DirectoryNode{fs: x.fs, path: "some/dir"}
		if errno := node.Setxattr(ctx, "user.d", []byte("v"), 0); errno != syscall.ENOTSUP {
			t.Errorf("ObjectFS returned %s for an attribute on a directory, want ENOTSUP. A local "+
				"filesystem stores it; a directory here is a key prefix, and one that exists only because "+
				"objects share that prefix has no object to hold it — so it would persist for some "+
				"directories and vanish for others.", errnoName(errno))
		}
	})
}
