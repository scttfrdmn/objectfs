//go:build linux || darwin

package fuse

// Rename (#164), for a single file and for a directory prefix.
//
// # This operation cannot have POSIX semantics, and says so rather than pretending
//
// POSIX rename(2) is atomic: after it returns, the name is either the old one or the new one, never
// both and never neither, and a concurrent observer sees one of those two states. S3 offers no such
// primitive for a general-purpose bucket. There is a RenameObject API as of 2026, but it is restricted
// to directory buckets (S3 Express One Zone), and object annotations — which the attribute work depends
// on — are unsupported on directory buckets, so the two are mutually exclusive. What is left is a copy
// followed by a delete, which is two operations, so:
//
//   - A reader can observe the file at both names. The window is one round trip for a file, and the
//     whole traversal for a directory.
//   - A crash between the two leaves the file at both names.
//
// The response is to fail in the safe direction rather than to hide it. Every source object is deleted
// only after its own copy has succeeded, so an interrupted rename leaves the data readable at the old
// name, the new name, or both — never at neither. Duplicated data is an operator's cleanup problem;
// missing data is not recoverable. That ordering is the single most important line in this file.
//
// # Why the write path is flushed first
//
// A copy is server-side: it acts on objects. A file whose bytes are still dirty ranges in memory has no
// object, or has a stale one — so `echo hi > a; mv a b` would copy an object that does not exist or
// holds the pre-write bytes, delete the source, and then flush the pending ranges back to *a*. The file
// would end up at the name the user renamed away from, with b either absent or holding stale content.
//
// So [vfs.Writer.FlushPrefix] runs first and the rename proceeds only if it succeeds. See its own
// comment for why flushing beats migrating the pending ranges to the new key.
//
// # Why the source is copied rather than re-uploaded
//
// [types.Backend.CopyObject] transfers no bytes through this process, and it preserves user metadata,
// Content-Encoding, Content-Type, and storage class. All four matter: the POSIX mode and ownership live
// in user metadata and nowhere else, and the read path dispatches decoding on the stored
// Content-Encoding and fails closed on one it cannot handle — so a rename that dropped that header would
// leave a compressed file permanently unreadable with its bytes intact. Renaming a 10 GiB file would
// also be 20 GiB of transfer if it went through the client, and renaming a directory that times the
// number of objects under it.

import (
	"context"
	"log/slog"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// Compile-time proof that a directory implements the renamer. Without it go-fuse's bridge answers
// ENOTSUP for the absent interface (fs/bridge.go, the final return of rawBridge.Rename), which is what
// every release through v0.10.3 did — and unlike Unlink's default of *success*, that is at least honest.
var _ fs.NodeRenamer = (*DirectoryNode)(nil)

// renameExchange is RENAME_EXCHANGE from renameat2(2): atomically swap two names.
//
// Declared here rather than taken from fs.RENAME_EXCHANGE because the flag has to be recognized in
// order to be *refused*, and refusing it is the whole interaction — see [DirectoryNode.Rename].
const renameExchange = 0x2

// renameNoReplace is RENAME_NOREPLACE from renameat2(2): fail with EEXIST if the destination exists.
const renameNoReplace = 0x1

// Rename moves name under this directory to newName under newParent.
//
// # The flags are refused, not ignored
//
// renameat2(2) can carry RENAME_EXCHANGE ("swap these two names atomically") or RENAME_NOREPLACE ("fail
// if the destination exists"). Both are promises about atomicity that copy-then-delete cannot keep:
// an exchange would be four operations with two windows in which a name holds the wrong file or no file,
// and a no-replace would be a check followed by a copy with a window between them in which another writer
// creates the destination that the check said was absent.
//
// Returning EINVAL for both is what the kernel and the C library expect for a filesystem that does not
// implement them, and callers handle it: coreutils `mv` falls back to plain rename, and Git's
// no-replace path likewise. Accepting the flag and doing the non-atomic thing would silently break the
// invariant the caller asked for, which is worse than the fallback.
//
// # Read-only mounts
//
// EROFS before anything else. A read-only mount that copied and then failed to delete would leave the
// filesystem modified by an operation it had already decided to refuse.
func (n *DirectoryNode) Rename(
	ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32,
) syscall.Errno {
	if n.fs.config != nil && n.fs.config.ReadOnly {
		return syscall.EROFS
	}

	if flags&(renameExchange|renameNoReplace) != 0 {
		slog.Warn("refusing a renameat2 flag this filesystem cannot honor atomically; a rename here is "+
			"a server-side copy followed by a delete",
			"name", name, "new_name", newName, "flags", flags)

		return syscall.EINVAL
	}

	// The destination directory has to be one of ours. It always is on a real mount — the bridge resolves
	// both inodes from the same tree — but the parameter is an interface, and a rename into a node whose
	// path this package cannot compute has to be refused rather than guessed at. EXDEV is the errno for
	// "these two names are not on the same filesystem", which is what a foreign node amounts to, and `mv`
	// responds to it by falling back to copy-then-delete through the file contents.
	destDir, ok := newParent.(*DirectoryNode)
	if !ok {
		slog.Error("rename destination is not an ObjectFS directory", "type", newParent)

		return syscall.EXDEV
	}

	srcPath := n.joinPath(name)
	dstPath := destDir.joinPath(newName)

	if srcPath == dstPath {
		// rename(2) of a name to itself is a documented no-op that returns success. Doing the copy anyway
		// would be a self-copy followed by a delete of what it just wrote — which deletes the file.
		return 0
	}

	if n.fs.buffer == nil {
		return syscall.ENOTSUP
	}

	// Everything under the source must be in the object store before a server-side copy can see it. See
	// the file comment.
	if _, err := n.fs.buffer.FlushPrefix(ctx, srcPath); err != nil {
		n.fs.countError()
		slog.Error("rename could not make the source durable before copying it",
			"src", srcPath, "dst", dstPath, "error", err)

		return toErrno(err)
	}

	// What kind of thing is being renamed decides the whole shape of the operation, and getting it wrong
	// is not a cosmetic error: treating a directory as a file renames its marker object and orphans every
	// object beneath it, and treating a file as a directory renames nothing and reports success.
	keys, isDir, errno := n.renameSources(ctx, srcPath)
	if errno != 0 {
		return errno
	}

	if isDir {
		errno = n.renameTree(ctx, srcPath, dstPath, keys)
	} else {
		errno = n.renameOne(ctx, srcPath, dstPath)
	}
	if errno != 0 {
		return errno
	}

	// Only on success, and only after the storage side is done. go-fuse re-parents the existing inode
	// rather than rebuilding it, so the node still holds the old key until this runs — see [repoint].
	repoint(n.GetChild(name), dstPath)

	return 0
}

// renameSources decides whether srcPath names a file or a directory, and for a directory returns every
// object key at or under it.
//
// The listing is what makes the directory case correct, and it is also the existence check: a prefix
// with objects under it is a directory whether or not anything wrote a marker for it, so
// [DirectoryNode.Mkdir]'s marker is sufficient but not necessary. Absence of both an object and a
// listing is ENOENT.
//
// Order matters. The object is looked for first, because a marker object at "dir/" is also an object at
// a *different* key than "dir", so asking for the object at "dir" cannot be confused by it — while
// asking for the listing first would find the marker and call a regular file named "dir" a directory if
// a "dir/" prefix also happened to exist.
func (n *DirectoryNode) renameSources(ctx context.Context, srcPath string) ([]string, bool, syscall.Errno) {
	_, headErr := n.fs.backend.HeadObject(ctx, srcPath)
	switch {
	case headErr == nil:
		return nil, false, 0

	case !vfs.IsNotFound(headErr):
		// Not absence — a throttle, an expired credential, a permission failure. Reporting ENOENT here
		// would make `mv` say "no such file" about a file that is merely unreachable, and reporting
		// success would delete a source whose copy never happened.
		n.fs.countError()
		slog.Error("rename could not determine whether the source exists",
			"src", srcPath, "error", headErr)

		return nil, false, toErrno(headErr)
	}

	objects, listErr := n.fs.backend.ListObjects(ctx, srcPath+"/", 0)
	if listErr != nil {
		n.fs.countError()
		slog.Error("rename could not list the source directory", "src", srcPath, "error", listErr)

		return nil, false, toErrno(listErr)
	}

	if len(objects) == 0 {
		return nil, false, syscall.ENOENT
	}

	keys := make([]string, 0, len(objects))
	for _, obj := range objects {
		keys = append(keys, obj.Key)
	}

	return keys, true, 0
}

// renameOne renames a single file: copy to the destination, then delete the source.
//
// Replacing an existing destination is silent, as POSIX requires — an S3 copy overwrites the key
// unconditionally, so this comes for free rather than needing a delete first. It also means the
// destination's own pending writes have to go: they describe the file that was just replaced, and a
// later flush would put its bytes back over the file that replaced it.
func (n *DirectoryNode) renameOne(ctx context.Context, srcPath, dstPath string) syscall.Errno {
	if err := n.fs.backend.CopyObject(ctx, srcPath, dstPath); err != nil {
		n.fs.countError()
		slog.Error("rename failed to copy the source", "src", srcPath, "dst", dstPath, "error", err)

		return toErrno(err)
	}

	// After the copy, before the delete: from here the data exists at the destination, so a failure below
	// leaves it at both names rather than at neither.
	//
	// The destination's buffered state is dropped rather than flushed. It belongs to the file this rename
	// just overwrote, and flushing it would write those bytes over the file that replaced them.
	// Peers as well as this node, with no ETag: CopyObject reports no version, and a delete has none to
	// report. Empty is what [types.DistributedCoordinator.InvalidateKey] documents for both.
	n.fs.buffer.DiscardPrefix(dstPath)
	n.fs.invalidateBoth(ctx, dstPath, "")

	if err := n.fs.backend.DeleteObject(ctx, srcPath); err != nil {
		// The copy succeeded, so the data is safe at the destination and the rename has substantially
		// happened; what failed is the cleanup. Reporting the error is still right — the caller asked for a
		// move and got a copy — and it must not be swallowed, because `mv` reporting success would leave
		// the user believing the source is gone.
		n.fs.countError()
		slog.Error("rename copied the file but could not delete the source, so it now exists at both "+
			"names; the destination holds the data and the source needs removing by hand",
			"src", srcPath, "dst", dstPath, "error", err)

		return toErrno(err)
	}

	n.fs.buffer.DiscardPrefix(srcPath)
	n.fs.invalidateBoth(ctx, srcPath, "")

	n.fs.stats.mu.Lock()
	n.fs.stats.Renames++
	n.fs.stats.mu.Unlock()

	return 0
}

// renameTree renames a directory by copying every object under its prefix and then deleting each source.
//
// # This is not atomic and does not try to look atomic
//
// A directory with a million objects is a million copies and a million deletes, and it can fail at any
// point. There is no transaction to roll back to, and simulating one — copying everything, then deleting
// everything, and undoing the copies on failure — would only move the non-atomic window and add a failure
// mode where the undo itself fails.
//
// So the loop is per-object and each object's source is deleted immediately after its own copy succeeds,
// and on the first failure it stops and reports. What the caller is left with is a partially moved tree
// in which every object is readable at exactly one of the two names. Nothing is lost, and re-running the
// same rename completes it, because an object already moved is simply absent from the source listing on
// the second pass.
//
// Stopping on the first error rather than pressing on is deliberate: the usual cause is a credential or
// permission problem that will fail identically for every remaining object, and continuing would turn one
// error into a million while making the reported one arbitrary.
func (n *DirectoryNode) renameTree(ctx context.Context, srcPath, dstPath string, keys []string) syscall.Errno {
	srcPrefix := strings.TrimSuffix(srcPath, "/") + "/"
	dstPrefix := strings.TrimSuffix(dstPath, "/") + "/"

	moved := 0
	for _, key := range keys {
		// Defensive rather than expected: the listing was made with this prefix, so every key carries it.
		// A key that does not would produce a destination outside the target directory, which is the one
		// mistake here that writes an object somewhere the user never named.
		if !strings.HasPrefix(key, srcPrefix) {
			n.fs.countError()
			slog.Error("rename skipping a listed key that does not carry the source prefix",
				"key", key, "prefix", srcPrefix)

			return syscall.EIO
		}

		newKey := dstPrefix + strings.TrimPrefix(key, srcPrefix)

		if err := n.fs.backend.CopyObject(ctx, key, newKey); err != nil {
			n.fs.countError()
			slog.Error("rename of a directory failed partway; every object already moved is at its new "+
				"name and the rest are still at their old ones, so re-running the same rename completes it",
				"src", key, "dst", newKey, "moved", moved, "of", len(keys), "error", err)

			return toErrno(err)
		}

		n.fs.buffer.DiscardPrefix(newKey)
		n.fs.invalidateBoth(ctx, newKey, "")

		if err := n.fs.backend.DeleteObject(ctx, key); err != nil {
			n.fs.countError()
			slog.Error("rename of a directory copied an object but could not delete the source, so it "+
				"now exists at both names",
				"src", key, "dst", newKey, "moved", moved, "of", len(keys), "error", err)

			return toErrno(err)
		}

		n.fs.buffer.DiscardPrefix(key)
		n.fs.invalidateBoth(ctx, key, "")

		moved++
	}

	// The marker object for the new directory, and only if the old one had none. A directory that existed
	// implicitly — as the shared prefix of its contents, with no marker — stays implicit, since writing a
	// marker the source never had would make the destination a different kind of directory than the
	// source. One that had a marker already had it copied by the loop, because the marker's key is
	// srcPrefix and the listing includes it.
	//
	// Nothing is written here for either case; the check exists to record why. See [DirectoryNode.Mkdir]
	// for what the marker is for.

	n.fs.invalidateBoth(ctx, srcPath, "")
	n.fs.invalidateBoth(ctx, srcPrefix, "")
	n.fs.invalidateBoth(ctx, dstPath, "")
	n.fs.invalidateBoth(ctx, dstPrefix, "")

	n.fs.stats.mu.Lock()
	n.fs.stats.Renames++
	n.fs.stats.mu.Unlock()

	return 0
}

// repoint updates the moved node's stored path, and its descendants' if it is a directory.
//
// # Why this is necessary, and why it is easy to miss
//
// go-fuse's bridge, on a Rename that returns success, calls Inode.MvChild (fs/inode.go), which
// re-parents *the same* *Inode* under the new name. It does not build a new node and it does not consult
// this package again. So the [FileNode] or [DirectoryNode] the inode carries survives the rename holding
// the path it was constructed with, and every subsequent operation on the moved dentry addresses the key
// the file was moved away from: after `mv a b`, a write to b flushes to a — recreating the source the
// rename just deleted, and leaving b as the user found it.
//
// Nothing catches this at the FUSE layer's own boundary, because the stale path is a perfectly valid key
// and every call succeeds. It surfaces only as the wrong object being modified, which is why the path is
// an accessor rather than a field — see [DirectoryNode.key].
//
// A directory carries its whole resolved subtree with it, so the walk is recursive. Only inodes the
// kernel currently holds are here; anything not yet looked up resolves against the new path when it is.
func repoint(inode *fs.Inode, newPath string) {
	if inode == nil {
		return
	}

	switch node := inode.Operations().(type) {
	case *FileNode:
		node.setKey(newPath)

	case *DirectoryNode:
		node.setKey(newPath)

		for name, child := range inode.Children() {
			childPath := newPath + "/" + name
			if newPath == "" {
				childPath = name
			}

			repoint(child, childPath)
		}
	}
}
