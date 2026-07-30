package fuse

import (
	"context"
	"syscall"
	"testing"

	gofuse "github.com/hanwen/go-fuse/v2/fs"
)

// TestUnlinkRmdir_AreImplemented is the regression test for issue #163.
//
// go-fuse defaults an unimplemented NodeUnlinker/NodeRmdirer to *success*, so if
// these methods are ever removed, `rm` and `rmdir` silently exit 0 while the S3
// object survives — a false success that hides undeleted (and still billing)
// data. These assertions fail at compile time if the interfaces stop being
// satisfied, and at run time if the methods start reporting success before real
// deletion is wired up.
func TestUnlinkRmdir_AreImplemented(t *testing.T) {
	t.Parallel()

	// Compile-time guard: DirectoryNode must satisfy both interfaces, otherwise
	// go-fuse never calls our code and falls back to returning success.
	var _ gofuse.NodeUnlinker = (*DirectoryNode)(nil)
	var _ gofuse.NodeRmdirer = (*DirectoryNode)(nil)

	fs := NewFileSystem(nil, nil, nil, nil, nil)
	node := &DirectoryNode{fs: fs, path: "some/dir"}
	ctx := context.Background()

	tests := []struct {
		name string
		call func() syscall.Errno
	}{
		{name: "Unlink", call: func() syscall.Errno { return node.Unlink(ctx, "file.txt") }},
		{name: "Rmdir", call: func() syscall.Errno { return node.Rmdir(ctx, "child") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			errno := tt.call()

			// The critical assertion: must NOT report success. A zero errno tells
			// the kernel the delete happened and drops the inode from the tree.
			if errno == 0 {
				t.Fatalf("%s returned success (0) but nothing was deleted from S3; "+
					"the caller would believe the object is gone", tt.name)
			}
			if errno != syscall.EROFS {
				t.Errorf("%s: got errno %v, want EROFS", tt.name, errno)
			}
		})
	}
}
