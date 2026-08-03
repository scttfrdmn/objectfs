//go:build linux || darwin

package fuse

// The README's "not implemented" table names an errno for every operation ObjectFS does not implement.
// Those errnos are not ObjectFS's — for an operation with no method on DirectoryNode, the answer comes
// from go-fuse's bridge, which decides what an absent optional interface means. So the table documents a
// *dependency's* behavior, and until this file nothing in this repo made it true.
//
// That is the shape of defect this file exists to prevent. `mv` was documented as ENOSYS on the strength
// of "rename is not implemented" plus a reasonable guess; the bridge returns ENOTSUP (fs/bridge.go, the
// final return of rawBridge.Rename). The guess was wrong in a way no build, lint, or test could notice,
// because the claim lived only in prose.
//
// Two things make this worth a test rather than a comment:
//
//   - The defaults differ per operation and are not guessable. Rename, Symlink, Link, Mknod, and
//     Fallocate default to ENOTSUP; Getxattr and Removexattr default to ENOATTR (which *is* ENODATA on
//     Linux); Listxattr defaults to OK with an empty list; and Unlink and Rmdir default to **success**,
//     which is why this package implements them only to refuse — see delete_stub_test.go.
//   - They are a dependency's choices, so a go-fuse upgrade can change them. A bump that turned
//     Listxattr's empty-OK into ENOTSUP would change what every `ls -@` and `rsync -X` sees, and would
//     be invisible without this.
//
// The bridge is driven directly rather than through a kernel mount: fs.NewNodeFS returns the
// fuse.RawFileSystem the kernel talks to, so calling its methods runs the same dispatch — including the
// interface assertions that produce these defaults — with no macFUSE, no privileges, and no mount point.

import (
	"syscall"
	"testing"

	gofuse "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// errXattrMissing is go-fuse's answer for an attribute that cannot exist.
//
// It is aliased from fuse.ENOATTR rather than written as syscall.ENOATTR, because the two platforms
// spell it differently: ENOATTR on darwin, ENODATA on linux (fuse/types_darwin.go, types_linux.go).
// Naming either one directly would need a build-tagged pair of files to be right on both.
const errXattrMissing = syscall.Errno(fuse.ENOATTR)

// rootBridge builds the RawFileSystem over a real DirectoryNode root, as a mount would.
func rootBridge(t *testing.T) fuse.RawFileSystem {
	t.Helper()

	filesystem := NewFileSystem(nil, nil, nil, nil, nil)

	return gofuse.NewNodeFS(filesystem.Root(), &gofuse.Options{})
}

// rootHeader addresses the root inode, which is nodeid 1 by the FUSE protocol.
func rootHeader() fuse.InHeader {
	return fuse.InHeader{NodeId: 1}
}

// TestUnimplementedOperationsReturnTheDocumentedErrno pins the README's not-implemented table.
//
// Each case is an operation with no method on DirectoryNode, so the errno is go-fuse's default for the
// absent interface. The want values are what the README states; a failure here means the documentation
// and the binding disagree, and the documentation is the thing users act on.
func TestUnimplementedOperationsReturnTheDocumentedErrno(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(fuse.RawFileSystem) fuse.Status
		want syscall.Errno

		// why records what a caller does with this answer, so a failure explains the consequence
		// rather than only the mismatch.
		why string
	}{
		{
			name: "rename",
			call: func(raw fuse.RawFileSystem) fuse.Status {
				in := &fuse.RenameIn{InHeader: rootHeader(), Newdir: 1}
				return raw.Rename(nil, in, "old.txt", "new.txt")
			},
			want: syscall.ENOTSUP,
			why: "mv reports this verbatim, and the README told users it was ENOSYS. The two are not " +
				"interchangeable: ENOSYS means 'this filesystem will never do this' and callers may " +
				"stop asking, while ENOTSUP is answered per call",
		},
		{
			name: "symlink",
			call: func(raw fuse.RawFileSystem) fuse.Status {
				h := rootHeader()
				return raw.Symlink(nil, &h, "target", "link", &fuse.EntryOut{})
			},
			want: syscall.ENOTSUP,
			why:  "ln -s reports this, and tar and rsync branch on it to skip rather than abort",
		},
		{
			name: "link",
			call: func(raw fuse.RawFileSystem) fuse.Status {
				in := &fuse.LinkIn{InHeader: rootHeader(), Oldnodeid: 1}
				return raw.Link(nil, in, "hardlink", &fuse.EntryOut{})
			},
			want: syscall.ENOTSUP,
			why: "hard links will never be supported — S3 has no second name for an object — so this " +
				"is the permanent answer, not a placeholder",
		},
		{
			name: "mknod",
			call: func(raw fuse.RawFileSystem) fuse.Status {
				in := &fuse.MknodIn{InHeader: rootHeader(), Mode: syscall.S_IFIFO | 0o644}
				return raw.Mknod(nil, in, "fifo", &fuse.EntryOut{})
			},
			want: syscall.ENOTSUP,
			why: "a device or FIFO has no object representation, so cp -a of one must fail rather than " +
				"quietly create a regular file",
		},
		{
			name: "setxattr",
			call: func(raw fuse.RawFileSystem) fuse.Status {
				in := &fuse.SetXAttrIn{InHeader: rootHeader()}
				return raw.SetXAttr(nil, in, "user.test", []byte("value"))
			},
			want: errXattrMissing,
			why: "note this is 'no such attribute' rather than 'unsupported', which is a strange answer " +
				"for a *set*. It is go-fuse's default, and the README states it rather than implying " +
				"ObjectFS chose it",
		},
		{
			name: "removexattr",
			call: func(raw fuse.RawFileSystem) fuse.Status {
				h := rootHeader()
				return raw.RemoveXAttr(nil, &h, "user.test")
			},
			want: errXattrMissing,
			why:  "removing an attribute that cannot exist reports absence, which is at least accurate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := syscall.Errno(tt.call(rootBridge(t)))

			if got != tt.want {
				t.Errorf("%s returned %s, want %s.\n%s\n"+
					"The README's not-implemented table states the want value; either go-fuse's "+
					"default changed or the table is wrong.",
					tt.name, errnoName(got), errnoName(tt.want), tt.why)
			}
		})
	}
}

// TestGetxattrReportsAbsenceAndListxattrReportsEmpty covers the two xattr reads, whose signatures return
// a size alongside the status and so do not fit the table above.
//
// These two disagree with each other in go-fuse, and the disagreement is the point: a query for one
// attribute reports the attribute missing, while a request to list them all succeeds with a count of
// zero. Both are defensible, and a caller sees very different things — `getfattr -n` errors while
// `ls -@` prints nothing and exits 0.
func TestGetxattrReportsAbsenceAndListxattrReportsEmpty(t *testing.T) {
	t.Parallel()

	t.Run("getxattr reports the attribute missing", func(t *testing.T) {
		t.Parallel()

		raw := rootBridge(t)
		h := rootHeader()

		size, status := raw.GetXAttr(nil, &h, "user.test", make([]byte, 128))

		if syscall.Errno(status) != errXattrMissing {
			t.Errorf("getxattr returned %s, want %s. getfattr renders this to the user, and 'no such "+
				"attribute' is the accurate answer for a filesystem that stores none.",
				errnoName(syscall.Errno(status)), errnoName(errXattrMissing))
		}
		if size != 0 {
			t.Errorf("getxattr reported %d bytes alongside a failure; a caller trusting the size over "+
				"the status would read uninitialized buffer", size)
		}
	})

	t.Run("listxattr succeeds with an empty list", func(t *testing.T) {
		t.Parallel()

		raw := rootBridge(t)
		h := rootHeader()

		size, status := raw.ListXAttr(nil, &h, make([]byte, 128))

		if status != fuse.OK {
			t.Errorf("listxattr returned %s, want OK. An empty list is the truthful answer — there are "+
				"no extended attributes — and failing instead would make `ls -@`, `cp -a`, and "+
				"`rsync -X` report an error per file for something with nowhere to go.",
				errnoName(syscall.Errno(status)))
		}
		if size != 0 {
			t.Errorf("listxattr reported %d bytes of names for a filesystem that stores no attributes", size)
		}
	})
}

// TestFallocateHasNoImplementationToDispatchTo covers the one row in the table that cannot be driven
// through the bridge the way the others are.
//
// Fallocate needs a live file inode *and* an open handle (rawBridge.Fallocate looks up both), which means
// a Lookup against a real backend and an Open — an integration fixture, for an operation with no code to
// exercise. The claim the README makes is not about a code path anyway; it is that neither of the two
// optional interfaces the bridge probes is satisfied, in which case its final return is ENOTSUP.
// Asserting the two negative assertions states exactly that and nothing it has not checked.
//
// The counterpart matters more than the errno: if either interface is ever implemented, this fails and
// says so, because at that point the README's row is wrong in the direction that misleads — claiming an
// operation is unavailable when it works.
func TestFallocateHasNoImplementationToDispatchTo(t *testing.T) {
	t.Parallel()

	if _, ok := any((*FileNode)(nil)).(gofuse.NodeAllocater); ok {
		t.Error("FileNode implements Allocate, so fallocate no longer returns ENOTSUP. Update the " +
			"README's not-implemented table — a row claiming an operation fails when it succeeds is " +
			"the direction that misleads, because a caller avoids something that would have worked.")
	}

	if _, ok := any((*FileHandle)(nil)).(gofuse.FileAllocater); ok {
		t.Error("FileHandle implements Allocate, so fallocate no longer returns ENOTSUP. The bridge " +
			"probes the handle after the node, so this reaches the same outcome by the second path. " +
			"Update the README's not-implemented table.")
	}
}

// TestLocksAreNotForwardedToTheFilesystem pins the mount option rather than the bridge.
//
// The README says locks are host-local rather than refused, and that claim rests entirely on EnableLocks
// being unset: go-fuse only negotiates CAP_POSIX_LOCKS/CAP_FLOCK_LOCKS with the kernel when it is set
// (fuse/opcode.go, doInit). With it unset the kernel never sends a LK opcode, the bridge's ENOTSUP
// default for Getlk/Setlk is unreachable, and the kernel arbitrates locks itself — locally, per host.
//
// This is the difference between "flock fails" and "flock succeeds and means nothing to the host next
// door", which is a data-integrity claim rather than a compatibility note. Setting EnableLocks without
// implementing the three lock methods would flip every locking caller to ENOTSUP, SQLite included, so
// both halves are asserted together.
func TestLocksAreNotForwardedToTheFilesystem(t *testing.T) {
	t.Parallel()

	// The real mount path, with the default config NewMountManager builds when given nil.
	opts := NewMountManager(NewFileSystem(nil, nil, nil, nil, nil), nil).buildFUSEOptions()

	if opts.EnableLocks {
		t.Error("EnableLocks is set, so the kernel now forwards lock requests to ObjectFS — but no " +
			"Getlk/Setlk/Setlkw is implemented, so go-fuse answers ENOTSUP and every locking caller " +
			"fails outright where it previously took a host-local lock. Implement the three methods " +
			"before setting this, and update the README's locking row either way.")
	}

	// Referencing the interface types keeps a go-fuse rename of any of them a compile error here,
	// rather than a silently-skipped assertion below.
	var (
		_ *gofuse.NodeGetlker  = nil
		_ *gofuse.NodeSetlker  = nil
		_ *gofuse.NodeSetlkwer = nil
	)

	if _, ok := any((*DirectoryNode)(nil)).(gofuse.NodeGetlker); ok {
		t.Error("DirectoryNode now implements Getlk, so locks are being arbitrated somewhere. Set " +
			"EnableLocks — otherwise the kernel never asks and the implementation is dead code — and " +
			"update the README's locking row, which says locks never reach the filesystem.")
	}
}
