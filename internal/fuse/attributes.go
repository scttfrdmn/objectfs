//go:build linux || darwin

package fuse

// The attribute half of the FUSE node contract: Getattr, Setattr, Fsync, and Statfs for both node
// types.
//
// v0.10.0 implemented none of these except a partial FileNode.Getattr, and the omissions were not
// independent:
//
//   - DirectoryNode had no Getattr at all. go-fuse's mode backstop (fs/bridge.go setAttr) is disabled
//     whenever Options.NullPermissions is set, which mount.go set unconditionally, so every directory
//     reported mode 0000 and the mount was unusable for any non-root user.
//   - Without a Setattr, rawBridge.SetAttr returns ENOTSUP, so chmod, chown, touch, and truncate all
//     failed — and because CAP_ATOMIC_O_TRUNC is not negotiated, O_TRUNC arrives as a Setattr
//     carrying FATTR_SIZE, which means `> file` could not shorten an object either.
//   - Without a Statfs, rawBridge.StatFs leaves the StatfsOut zeroed and returns OK. NodeStatfser's
//     own documentation says an OSX filesystem must implement Statfs or the mount will not work.
//   - Without an Fsync, fsync(2) returned ENOTSUP while docs/architecture/overview.md claimed
//     "fsync() guarantees data is in S3 before returning".
//
// # Where the numbers come from
//
// Everything reported here is built by internal/vfs from the object's stored metadata, or is derived
// and documented as derived. This file translates; it does not decide. The one exception is
// directories, which are key prefixes rather than objects and therefore have nothing to read — see
// [FileSystem.dirDefaults].

import (
	"context"
	"log/slog"
	"math"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	iofs "io/fs"

	"github.com/objectfs/objectfs/internal/vfs"
)

// Compile-time proof that both node types satisfy the contract. Every one of these is an interface
// go-fuse probes with a type assertion and silently substitutes a default for when it is absent, and
// three of those defaults are actively harmful: mode 0000 for a missing Getattr under
// NullPermissions, a zeroed statfs for a missing Statfs, and — in Rmdir and Unlink — success.
var (
	_ fs.NodeGetattrer = (*FileNode)(nil)
	_ fs.NodeSetattrer = (*FileNode)(nil)
	_ fs.NodeFsyncer   = (*FileNode)(nil)
	_ fs.NodeStatfser  = (*FileNode)(nil)
	_ fs.NodeOpener    = (*FileNode)(nil)

	_ fs.NodeGetattrer = (*DirectoryNode)(nil)
	_ fs.NodeSetattrer = (*DirectoryNode)(nil)
	_ fs.NodeFsyncer   = (*DirectoryNode)(nil)
	_ fs.NodeStatfser  = (*DirectoryNode)(nil)
	_ fs.NodeLookuper  = (*DirectoryNode)(nil)
	_ fs.NodeReaddirer = (*DirectoryNode)(nil)
	_ fs.NodeMkdirer   = (*DirectoryNode)(nil)
	_ fs.NodeCreater   = (*DirectoryNode)(nil)
	_ fs.NodeUnlinker  = (*DirectoryNode)(nil)
	_ fs.NodeRmdirer   = (*DirectoryNode)(nil)
)

// attrBlkSize is the st_blksize a stat reports: the I/O size a caller should prefer.
//
// It is set explicitly rather than left to go-fuse's setBlocks, which is a no-op on darwin and on
// linux derives Blocks from Size at 4096 — a second, independent size computation that would
// disagree with [vfs.Attr.Blocks] the moment either changed. Blocks is always in 512-byte units
// regardless of this value, because POSIX fixes st_blocks at 512 and du and tar depend on it.
const attrBlkSize = 4096

// defaultAttrTimeout matches go-fuse's own default for Options.AttrTimeout. It applies when
// Config.CacheTTL is unset, which happens only for a FileSystem built without a config.
const defaultAttrTimeout = time.Second

// attrTimeout is how long the kernel may cache an attribute set.
//
// Setattr has to supply this itself: rawBridge.SetAttr, unlike rawBridge.getattr, runs neither
// setAttr nor setAttrTimeout, so an AttrOut returned from Setattr with a zero AttrValid tells the
// kernel not to cache the result at all — one round trip per stat for the rest of the mount.
func (fs *FileSystem) attrTimeout() time.Duration {
	if fs.config != nil && fs.config.CacheTTL > 0 {
		return fs.config.CacheTTL
	}

	return defaultAttrTimeout
}

// fileDefaults returns the attributes to report for parts of a regular file's identity that the
// object does not record.
//
// An object written by another tool — aws s3 cp, boto3, a bucket that predates ObjectFS — carries no
// objectfs-uid, and "absent" is not the same fact as "recorded as zero". Reporting root would make
// every such file appear to belong to someone else in ls -l and would make cp -p and rsync complain
// about ownership they cannot set, so absence reports the mounting user instead. An object that
// genuinely records uid 0 still reports 0.
func (fs *FileSystem) fileDefaults() vfs.Attr {
	a := vfs.Attr{
		Type: vfs.FileTypeRegular,
		Mode: vfs.DefaultFileMode,
	}
	if fs.config == nil {
		return a
	}

	if fs.config.DefaultMode != 0 {
		a.Mode = iofs.FileMode(fs.config.DefaultMode).Perm()
	}
	a.UID = fs.config.DefaultUID
	a.GID = fs.config.DefaultGID

	return a
}

// dirDefaults returns the attributes to report for a directory, which are entirely synthetic.
//
// A directory in ObjectFS is a key prefix. It has no object, so it has no metadata, no ETag, and no
// modification time of its own — there is nothing to read a mode from, which is exactly why the
// default has to be right. Mkdir does write a zero-byte marker object, but a directory that exists
// only because objects share a prefix has no marker, and reporting two different modes depending on
// how a directory came to exist would be worse than reporting one.
//
// The execute bits are the load-bearing part: a directory mode without them cannot be traversed,
// which is how mode 0000 made the whole mount inaccessible in v0.10.0.
func (fs *FileSystem) dirDefaults() vfs.Attr {
	mode := vfs.DefaultDirMode
	var uid, gid uint32

	if fs.config != nil {
		if fs.config.DefaultDirMode != 0 {
			mode = iofs.FileMode(fs.config.DefaultDirMode).Perm()
		}
		uid, gid = fs.config.DefaultUID, fs.config.DefaultGID
	}

	// The mtime is the process start time rather than time.Now(): a directory whose mtime advances on
	// every stat defeats every make(1)-style timestamp comparison and every rsync --times.
	return vfs.DirAttr(mode, uid, gid, processStart)
}

// processStart is the synthetic mtime every directory reports. Captured once so that repeated stats
// of the same directory agree with each other.
var processStart = time.Now()

// callerOwner returns the uid and gid of the process that made the request, falling back to the
// configured defaults when the kernel did not supply them.
//
// A file must be owned by whoever created it. Recording the configured default instead would make
// every file on a multi-user mount belong to the mounting user, and platform.go hardcoded uid 1000
// through v0.10.0, so it belonged to whoever happened to be user 1000 on that host.
func (fs *FileSystem) callerOwner(ctx context.Context) (uid, gid uint32) {
	d := fs.fileDefaults()
	uid, gid = d.UID, d.GID

	if caller, ok := fuse.FromContext(ctx); ok {
		uid, gid = caller.Uid, caller.Gid
	}

	return uid, gid
}

// fillAttr renders a [vfs.Attr] into the kernel's attribute struct.
//
// Ino is deliberately not set. rawBridge.getattr overwrites it with the inode's own stable Ino and
// logs a warning first if the two disagree, so writing one here produces a warning per stat and
// changes nothing.
func fillAttr(out *fuse.Attr, a vfs.Attr) {
	out.Mode = modeBits(a)
	out.Size = safeInt64ToUint64(a.Size)
	out.Blocks = safeInt64ToUint64(a.Blocks())
	out.Blksize = attrBlkSize
	out.Nlink = a.Nlink()
	out.Uid = a.UID
	out.Gid = a.GID

	// SetTimes writes the whole-second and nanosecond halves of each field together. Assigning
	// out.Mtime alone — which is what v0.10.0 did — leaves Mtimensec holding whatever was there
	// before, so a file's mtime carried a nanosecond component from an unrelated stat.
	atime, mtime, ctime := a.Atime, a.Mtime, a.Ctime
	out.SetTimes(&atime, &mtime, &ctime)
}

// modeBits returns the mode with the type bits the kernel requires.
//
// The type bits matter beyond tidiness: rawBridge.getattr recombines the permission bits with the
// inode's stable type, but nothing does that for a direct call, and a mode without S_IFREG or
// S_IFDIR describes a file of no type at all.
func modeBits(a vfs.Attr) uint32 {
	m := uint32(a.Mode.Perm())
	if a.IsDir() {
		return m | fuse.S_IFDIR
	}

	return m | fuse.S_IFREG
}

// attr returns the file's current attributes, preferring the write path's view.
//
// The order is not interchangeable. The write path holds pending writes, a pending truncation, and a
// pending chmod, so when it knows about the path its answer is the only complete one — and the size
// it reports is the one the kernel clamps reads to, which is why a file being appended to must be
// asked there first. When it holds nothing the object's own metadata is authoritative, read through
// the metadata cache so that a directory walk does not issue a HEAD per file.
//
// FileNode.info is not consulted. It is the HeadObject result from the Lookup that created the
// inode, and an inode outlives any number of writes: reporting from it made Getattr answer with the
// size and mtime the file had when it was first looked up.
func (f *FileNode) attr(ctx context.Context) (vfs.Attr, error) {
	if f.fs.buffer != nil {
		if a, ok := f.fs.buffer.Attr(f.path); ok {
			return a, nil
		}
	}

	info, err := f.fs.statObject(ctx, f.path)
	if err != nil {
		return vfs.Attr{}, err
	}

	return vfs.AttrFromMetadataWithDefaults(
		info.Metadata, info.Size, info.LastModified, info.ETag, f.fs.fileDefaults()), nil
}

// Getattr reports a file's attributes.
func (f *FileNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	a, err := f.attr(ctx)
	if err != nil {
		slog.Error("getattr failed", "path", f.path, "error", err)

		return toErrno(err)
	}

	fillAttr(&out.Attr, a)
	out.SetTimeout(f.fs.attrTimeout())

	return 0
}

// Setattr applies chmod, chown, utimes, and truncate.
//
// # The mask
//
// A SETATTR request carries a bitmask saying which fields the caller actually set, and the fields it
// did not set hold whatever was in the struct. Applying all of them unconditionally would have
// `touch` reset a file's mode to zero. Each arm below is guarded by its own accessor, and the three
// booleans passed to [vfs.Writer.SetAttr] carry the mask down to the one place that owns the merge.
//
// # Why this flushes
//
// chmod(2) and truncate(2) take a path, not a descriptor, so there is no handle whose Release would
// later make the change durable — nothing would ever flush it, and the change would survive only to
// the FlushAll at unmount. A mount killed before then would lose it silently. So this flushes
// synchronously and reports the failure, which costs one PUT or metadata rewrite per call and is the
// only way `chmod` can mean anything.
//
// # O_TRUNC
//
// CAP_ATOMIC_O_TRUNC is not negotiated, so the kernel implements O_TRUNC as a SETATTR carrying
// FATTR_SIZE=0 before the open completes. The size arm below is therefore also the O_TRUNC path;
// v0.10.0 had neither, which is why `> file` could not shorten an object.
func (f *FileNode) Setattr(
	ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut,
) syscall.Errno {
	if f.fs.config != nil && f.fs.config.ReadOnly {
		return syscall.EROFS
	}
	if f.fs.buffer == nil {
		return syscall.ENOTSUP
	}

	changed := false

	if size, ok := in.GetSize(); ok {
		if size > math.MaxInt64 {
			return syscall.EFBIG
		}
		if err := f.fs.buffer.Truncate(ctx, f.path, int64(size)); err != nil {
			slog.Error("setattr: truncate failed", "path", f.path, "size", size, "error", err)

			return toErrno(err)
		}
		changed = true
	}

	mode, modeOK := in.GetMode()
	if modeOK && mode&^0o777 != 0 {
		// setuid, setgid, and sticky have no representation in vfs.Attr, whose Mode is documented as
		// permission bits only. Refusing is the honest answer: access on this filesystem is decided by
		// the S3 credentials the process holds, not by a mode bit, so a setuid bit that appeared to be
		// stored would promise an escalation that cannot happen and could not be relied on either way.
		slog.Warn("setattr: refusing to set mode bits outside the permission mask",
			"path", f.path, "mode", mode)

		return syscall.ENOTSUP
	}

	uid, uidOK := in.GetUID()
	gid, gidOK := in.GetGID()
	mtime, mtimeOK := in.GetMTime()

	if modeOK || uidOK || gidOK || mtimeOK {
		from := vfs.Attr{Mode: iofs.FileMode(mode), UID: uid, GID: gid}
		if mtimeOK {
			from.Mtime = mtime
		}

		if err := f.fs.buffer.SetAttr(ctx, f.path, modeOK, uidOK, gidOK, from); err != nil {
			slog.Error("setattr failed", "path", f.path, "error", err)

			return toErrno(err)
		}
		changed = true
	}

	// An atime-only request — `touch -a`, and every read on a relatime mount — is accepted and stores
	// nothing. Persisting it would mean an object metadata rewrite per read, and vfs.Attr documents
	// Atime as tracking Mtime for exactly that reason. Refusing instead would fail utimensat for a
	// value POSIX already permits a filesystem to keep only approximately.

	if changed {
		if err := f.fs.buffer.FlushContext(ctx, f.path); err != nil {
			slog.Error("setattr: flush failed", "path", f.path, "error", err)

			return toErrno(err)
		}

		// The object's size, mtime, and mode all just changed. Anything cached for the path describes
		// what it was before.
		f.fs.invalidate(f.path)

		if h, ok := fh.(*FileHandle); ok {
			h.file.markClean()
		}
	}

	a, err := f.attr(ctx)
	if err != nil {
		slog.Error("setattr: cannot report resulting attributes", "path", f.path, "error", err)

		return toErrno(err)
	}

	fillAttr(&out.Attr, a)
	out.SetTimeout(f.fs.attrTimeout())

	return 0
}

// Fsync makes the file durable.
//
// It is implemented on the node rather than the handle so that it covers fsync(2) on a descriptor and
// syncfs-style callers alike; rawBridge.Fsync prefers NodeFsyncer and falls back to FileFsyncer, so
// one implementation answers both.
//
// The flags argument distinguishes fsync from fdatasync. It is ignored, and the distinction is not
// available to us: a PUT writes an object's data and its metadata in one request, so there is no
// cheaper data-only path to take.
func (f *FileNode) Fsync(ctx context.Context, fh fs.FileHandle, flags uint32) syscall.Errno {
	if f.fs.buffer == nil {
		return 0
	}

	if err := f.fs.buffer.FlushContext(ctx, f.path); err != nil {
		f.fs.countError()
		slog.Error("fsync failed", "path", f.path, "error", err)

		return toErrno(err)
	}

	f.fs.invalidate(f.path)

	if h, ok := fh.(*FileHandle); ok {
		h.file.markClean()
	}

	return 0
}

// Statfs reports filesystem-wide capacity. See [FileSystem.statfs].
func (f *FileNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	f.fs.statfs(out)

	return 0
}

// Getattr reports a directory's attributes.
//
// This method's absence was the single defect that made v0.10.0 unusable. mount.go sets
// Options.NullPermissions, which disables go-fuse's mode backstop, and with no Getattr on the
// directory node the bridge fell through to a mode of 0000 — no read, no write, and critically no
// execute, so no user but root could traverse a single directory of the mount.
func (n *DirectoryNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	fillAttr(&out.Attr, n.fs.dirDefaults())
	out.SetTimeout(n.fs.attrTimeout())

	return 0
}

// Setattr refuses to change a directory's ownership or mode, and accepts a change of its times as a
// no-op.
//
// The asymmetry is deliberate, and each half has its own reason.
//
// Mode, uid, and gid could be persisted: Mkdir writes a zero-byte marker object, and an object can
// carry metadata. They are not persisted yet, and a chmod that reported success while
// [DirectoryNode.Getattr] went on reporting the configured default would be the same defect as an
// `rm` that reports success while the object survives. Refusing is loud, and it is tracked.
//
// Times cannot be persisted meaningfully. A directory that exists only because objects share a
// prefix has no marker object to hold an mtime, and the marker's own LastModified records when the
// marker was written rather than when the directory changed. Since [vfs.DirAttr] documents directory
// times as synthetic, a request to set them is accepted rather than failed — refusing would make
// every `tar -x`, `cp -a`, and `rsync -a` report errors on every directory for an attribute that has
// nowhere to go.
func (n *DirectoryNode) Setattr(
	ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut,
) syscall.Errno {
	if n.fs.config != nil && n.fs.config.ReadOnly {
		return syscall.EROFS
	}

	_, modeOK := in.GetMode()
	_, uidOK := in.GetUID()
	_, gidOK := in.GetGID()

	if modeOK || uidOK || gidOK {
		slog.Warn("chmod and chown of a directory are not implemented; refusing rather than reporting "+
			"a change that would not be visible on the next stat",
			"path", n.path, "issue", 165)

		return syscall.ENOTSUP
	}

	fillAttr(&out.Attr, n.fs.dirDefaults())
	out.SetTimeout(n.fs.attrTimeout())

	return 0
}

// Fsync on a directory succeeds without doing anything, because there is nothing to do.
//
// A directory here is a key prefix. It holds no state of its own, so no state of its own can be made
// durable; the objects under it are made durable by their own fsync or close. Databases and version
// control systems fsync a directory to force a rename or a create to disk, and both of those already
// complete synchronously on this filesystem — so success is the accurate answer, not a convenient
// one.
func (n *DirectoryNode) Fsync(ctx context.Context, fh fs.FileHandle, flags uint32) syscall.Errno {
	return 0
}

// Statfs reports filesystem-wide capacity. See [FileSystem.statfs].
func (n *DirectoryNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	n.fs.statfs(out)

	return 0
}

// Synthetic capacity figures reported by statfs.
//
// Object storage has no size. A bucket has no quota to read, no free-space figure to report, and no
// inode table, so every number below is invented — the question is only which invented numbers do
// the least harm.
//
// A capacity of one pebibyte is large enough that no caller mistakes the mount for full and small
// enough to stay well inside the 32-bit block counts some tools still use internally. Free equals
// total: reporting usage would require a full bucket LIST on every statfs, and reporting zero free
// space would make install(1), dd, and most package managers refuse to write.
const (
	statfsBsize    = 4096
	statfsBlocks   = 1 << 38 // 1 PiB at statfsBsize
	statfsFiles    = 1 << 32
	statfsNameLen  = 255
	statfsFragSize = statfsBsize
)

// statfs fills a statfs response with documented synthetic values.
//
// It must exist. rawBridge.StatFs leaves the StatfsOut entirely zeroed and returns success when the
// node does not implement NodeStatfser, and go-fuse's own documentation for that interface says an
// OSX filesystem must implement Statfs or the mount will not work — a zero block size is not a
// number df or the macOS VFS can do anything with.
func (fs *FileSystem) statfs(out *fuse.StatfsOut) {
	out.Bsize = statfsBsize
	out.Frsize = statfsFragSize
	out.Blocks = statfsBlocks
	out.Bfree = statfsBlocks
	out.Bavail = statfsBlocks
	out.Files = statfsFiles
	out.Ffree = statfsFiles
	out.NameLen = statfsNameLen
}
