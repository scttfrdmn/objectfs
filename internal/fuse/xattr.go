//go:build linux || darwin

package fuse

// The extended-attribute half of the node contract: getxattr, setxattr, listxattr, and removexattr.
//
// Storage is S3 user metadata, encoded by internal/vfs/xattr.go — see that file for why metadata rather
// than object annotations, and for the name and value encodings. This file is the translation layer, and
// the translation is where three things are decided that internal/vfs cannot see:
//
//   - **The size protocol.** getxattr and listxattr are called twice by every caller: once with a
//     zero-length buffer to learn the size, then again with a buffer that big. A zero-size call must
//     report the size and succeed; a too-small buffer must return ERANGE *with* the size. go-fuse's
//     doGetXAttr converts an ERANGE on a zero-size request into OK, so the two cases can share one
//     implementation, but only if the size is returned alongside the error rather than instead of it.
//   - **The flags.** setxattr carries XATTR_CREATE and XATTR_REPLACE, which turn a set into a
//     create-only or replace-only operation. They are numerically **different on the two platforms**
//     (1 and 2 on Linux, 2 and 4 on darwin) because the value that arrives here is the one the local
//     kernel put in the request. golang.org/x/sys/unix's constants are per-platform for the same reason,
//     which is why they are used instead of literals — a literal would be right on one platform and
//     would silently turn XATTR_CREATE into XATTR_REPLACE on the other.
//   - **Which attributes ObjectFS will store at all.** See [refuseXattrNamespace].
//
// # Why every write here flushes synchronously
//
// setxattr(2) and removexattr(2) take a path, not a descriptor. There is no handle whose release would
// later persist the change, so nothing would ever flush it and it would survive only to the FlushAll at
// unmount — a mount killed before then loses it silently. [FileNode.Setattr] flushes for exactly this
// reason and this follows it: one metadata rewrite per call, and the failure reported to the caller that
// caused it. The cost is real, and it is the honest cost of the operation. An asynchronous version would
// make `setfattr` return success for a change S3 later refused, with no one left to tell.

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// Compile-time proof that both node types satisfy the four optional interfaces. Each is probed by a type
// assertion in go-fuse's bridge and silently defaulted when absent — Getxattr, Setxattr, and Removexattr
// to ENOATTR, Listxattr to success with an empty list. A signature that drifted on a go-fuse bump would
// take every extended attribute on the mount back to "no such attribute" with no compile error, which is
// the failure mode rename.go's assertion exists to prevent for `mv`.
var (
	_ fs.NodeGetxattrer    = (*FileNode)(nil)
	_ fs.NodeSetxattrer    = (*FileNode)(nil)
	_ fs.NodeListxattrer   = (*FileNode)(nil)
	_ fs.NodeRemovexattrer = (*FileNode)(nil)

	_ fs.NodeGetxattrer    = (*DirectoryNode)(nil)
	_ fs.NodeSetxattrer    = (*DirectoryNode)(nil)
	_ fs.NodeListxattrer   = (*DirectoryNode)(nil)
	_ fs.NodeRemovexattrer = (*DirectoryNode)(nil)
)

// errNoXattr is the errno for an attribute that is not there.
//
// Taken from fuse.ENOATTR because the two platforms spell it differently — ENOATTR on darwin, ENODATA on
// linux — so naming either syscall constant directly would need a build-tagged pair of files to be right
// on both. unimplemented_test.go aliases the same value for the same reason.
const errNoXattr = syscall.Errno(fuse.ENOATTR)

// Namespaces ObjectFS refuses to store, and reports nothing from.
//
// # security.
//
// On Linux the kernel reads `security.capability` from the filesystem on every exec and grants the named
// file capabilities from it. Setting that attribute on a local filesystem requires CAP_SETFCAP; on this
// one, the store behind it is object metadata that **anyone holding s3:PutObject on the bucket can write
// directly with the AWS CLI**, without going through the mount at all. Storing and reporting the
// attribute would therefore convert bucket write access into a route to file capabilities on every host
// that mounts it — a privilege escalation ObjectFS would be providing, not inheriting. `security.selinux`
// and the other LSM labels are the same shape: a label the filesystem reports is a label the kernel acts
// on, and this one cannot vouch for who wrote it.
//
// go-fuse offers Options.IgnoreSecurityLabels for part of this, which answers ENOATTR for three specific
// names before the request reaches a node. It is not enough on its own — it covers get and not set, and
// only those three names — so the namespace is refused here as well, where every operation passes.
//
// # system.
//
// `system.posix_acl_access` and `system.posix_acl_default` are how setfacl stores an ACL. go-fuse only
// negotiates ACL support with the kernel when Options.EnableAcl is set, which mount.go does not set, so
// nothing on this filesystem would enforce a stored ACL. An ACL that `getfacl` reports and no access
// check consults is worse than a refused setfacl: it tells an operator a file is restricted when it is
// readable by anyone whose credentials reach the bucket. Refusing makes setfacl fail, which is the true
// answer.
//
// Everything else is stored, `user.` and `trusted.` included. `trusted.` is root-only by the kernel's own
// check before the request arrives, and `com.apple.*` on darwin — quarantine flags, Finder info — is what
// makes an ObjectFS mount usable from the GUI.
var refusedXattrPrefixes = []string{"security.", "system."}

// refuseXattrNamespace reports whether name is in a namespace ObjectFS will not store.
//
// Matched case-sensitively: the kernel's namespaces are lower-case, and `Security.capability` is not one
// of them — it is an ordinary attribute name that happens to look like one, and storing it grants nothing
// because no kernel reads it.
func refuseXattrNamespace(name string) bool {
	for _, p := range refusedXattrPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}

	return false
}

// xattrSize converts a byte count to the uint32 the go-fuse contract returns, saturating rather than
// wrapping.
//
// Unreachable by construction on the value path: an attribute is bounded by [vfs.XattrBudget], under 2 KB,
// and a full listing by the same budget. It is here because "unreachable" is an argument about today's
// callers and the cast is a property of the function. gosec flags it (G115) and the fix is a clamp rather
// than a suppression, for the reason internal/distributed/cluster.go records at the same conversion: a
// clamp is testable and a `//nolint` is not, and this repository runs gosec a second time standalone,
// where the suppression has no effect anyway.
//
// The failure mode if it ever did wrap is the reason to saturate rather than truncate. Both numbers this
// returns are a *buffer size* the caller allocates and then reads into: a wrapped length smaller than the
// value is a caller that sizes a short buffer, gets no ERANGE because the filesystem also compared the
// wrapped number, and reads a truncated attribute as if it were whole — silent data loss on a read path.
// MaxUint32 cannot be mistaken for a real attribute size and fails the caller's own allocation instead.
func xattrSize(n int) uint32 {
	if n < 0 || n > math.MaxUint32 {
		return math.MaxUint32
	}

	return uint32(n)
}

// xattrRead answers a getxattr against a value that is already known, applying the size protocol.
//
// dest is the caller's buffer, which may be empty. The returned size is the attribute's full length in
// every case, including the two failures, because that is what the caller sizes its next buffer from.
func xattrRead(value, dest []byte) (uint32, syscall.Errno) {
	if len(dest) == 0 {
		// A size query. Reporting the length and succeeding is the contract; go-fuse's doGetXAttr also
		// rewrites an ERANGE to OK here, so answering either way works — but returning the size with an
		// error to a *direct* caller (a test, a future in-process consumer) would be a failure with a
		// number attached, which is harder to use correctly.
		return xattrSize(len(value)), 0
	}
	if len(dest) < len(value) {
		return xattrSize(len(value)), syscall.ERANGE
	}

	return xattrSize(copy(dest, value)), 0
}

// xattrNameList renders attribute names as listxattr's reply: each name NUL-terminated, concatenated.
//
// The terminator is on every name including the last, which is what listxattr(2) specifies and what
// every parser of the result assumes. Dropping the final one truncates the last attribute's name by a
// byte in `getfattr -d` output, which is the kind of defect that looks like a corrupt attribute.
func xattrNameList(names []string) []byte {
	n := 0
	for _, name := range names {
		n += len(name) + 1
	}

	out := make([]byte, 0, n)
	for _, name := range names {
		out = append(out, name...)
		out = append(out, 0)
	}

	return out
}

// Getxattr reports the value of one extended attribute.
//
// Absence is [errNoXattr], never ENOENT. Both would reach getfattr, but ENOENT means the *file* is gone,
// and a caller probing for an attribute before creating one would conclude the path had disappeared
// underneath it.
func (f *FileNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	if refuseXattrNamespace(attr) {
		// Answered before the HEAD, deliberately. The kernel asks for security.capability on every exec,
		// so a round trip here would put a network call in the path of running a program — and the answer
		// would be the same one. See [refusedXattrPrefixes] for why it is this answer.
		return 0, errNoXattr
	}

	a, err := f.attr(ctx)
	if err != nil {
		slog.Error("getxattr: cannot read attributes", "path", f.key(), "attr", attr, "error", err)

		return 0, toErrno(err)
	}

	value, ok := a.Xattr(attr)
	if !ok {
		return 0, errNoXattr
	}

	return xattrRead(value, dest)
}

// Listxattr reports the names of every extended attribute the file carries.
//
// An empty list succeeds. That is not the same as go-fuse's default for an absent implementation, which
// also succeeds with an empty list: the difference is that this one has looked, so a file with attributes
// reports them. A caller cannot tell the two apart on a file with none, which is exactly why the
// implementation being reached at all is asserted by a test rather than assumed.
func (f *FileNode) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	a, err := f.attr(ctx)
	if err != nil {
		slog.Error("listxattr: cannot read attributes", "path", f.key(), "error", err)

		return 0, toErrno(err)
	}

	names := make([]string, 0, len(a.Xattrs))
	for _, name := range a.XattrNames() {
		if refuseXattrNamespace(name) {
			// Reachable only for metadata written outside the mount, since the set path refuses these. It
			// is filtered rather than reported for the reason [refusedXattrPrefixes] gives: a name this
			// filesystem lists is a name a caller may then act on.
			continue
		}
		names = append(names, name)
	}

	return xattrRead(xattrNameList(names), dest)
}

// Setxattr stores one extended attribute, honoring XATTR_CREATE and XATTR_REPLACE.
//
// # The flags, and the race in checking them
//
// XATTR_CREATE means "fail if it exists" and XATTR_REPLACE means "fail if it does not". Both are checked
// by reading the current attributes and then writing, which is not atomic: two mounts issuing
// XATTR_CREATE for the same new attribute can both see it absent and both succeed, where a local
// filesystem would fail one of them. That is the same non-atomicity every other write on this filesystem
// has — S3 has no compare-and-swap on metadata, and #282's conditional writes are on object content, not
// on a metadata replace — and it is documented rather than papered over. What the check does buy is the
// single-writer case, which is every use of `setfattr --create` a person actually types.
//
// A flag combination naming both is EINVAL, as setxattr(2) specifies.
func (f *FileNode) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	if f.fs.config != nil && f.fs.config.ReadOnly {
		return syscall.EROFS
	}
	if f.fs.buffer == nil {
		// No write path, so nothing could persist this. ENOTSUP rather than EROFS: the mount is not
		// declared read-only, it simply has no buffer, and claiming a read-only filesystem would be a
		// different fact.
		return syscall.ENOTSUP
	}
	if refuseXattrNamespace(attr) {
		slog.Warn("refusing to store an extended attribute in a namespace the kernel acts on, because "+
			"object metadata is writable by anyone with bucket write access",
			"path", f.key(), "attr", attr)

		return syscall.ENOTSUP
	}

	create := flags&unix.XATTR_CREATE != 0
	replace := flags&unix.XATTR_REPLACE != 0

	// Naming both flags is the one case where the two kernels genuinely disagree, so it is answered
	// per-platform by [bothXattrFlagsErrno] rather than by a single errno here. Measured on both, not read
	// off a man page: darwin returns EINVAL whether or not the attribute exists, while linux has no
	// combined-flag check at all — its fs/xattr.c tests each flag independently against existence, making
	// `CREATE|REPLACE` ENODATA on a missing attribute and EEXIST on a present one. Both are
	// self-consistent, and any single hardcoded answer is wrong on one of them.
	//
	// This function returned EINVAL unconditionally at first, which is darwin's answer and the Linux man
	// page's description of it. xattr_oracle_test.go caught it on CI and could not have caught it locally:
	// the oracle compares against the *local* OS filesystem, so on darwin ObjectFS and the reference agreed
	// and the test was green. A unit test asserting EINVAL was green there too. That is what the oracle is
	// for — a hand-written expectation encodes the platform of whoever wrote it.
	if create && replace {
		if errno := bothXattrFlagsErrno(); errno != 0 {
			return errno
		}
		// Otherwise fall through: the switch below reproduces this kernel's answer from the flags alone.
	}

	if create || replace {
		a, err := f.attr(ctx)
		if err != nil {
			slog.Error("setxattr: cannot read attributes to honor the flags",
				"path", f.key(), "attr", attr, "error", err)

			return toErrno(err)
		}

		_, exists := a.Xattr(attr)
		switch {
		case create && exists:
			return syscall.EEXIST
		case replace && !exists:
			return errNoXattr
		}
	}

	// A nil data with a zero length is what the bridge hands over for `setfattr -n user.x` with no value,
	// and vfs.Writer.SetXattr rejects nil because nil is the tombstone in the stored form. An empty
	// attribute is legal and must round-trip as an empty attribute rather than as a removal, so the
	// emptiness is made explicit here.
	if data == nil {
		data = []byte{}
	}

	if err := f.fs.buffer.SetXattr(ctx, f.key(), attr, data); err != nil {
		slog.Error("setxattr failed", "path", f.key(), "attr", attr, "bytes", len(data), "error", err)

		return toErrno(err)
	}

	return f.flushXattr(ctx, "setxattr", attr)
}

// Removexattr deletes one extended attribute, reporting [errNoXattr] when there is none to delete.
//
// The removal is stored as a tombstone rather than by dropping the metadata key, because a metadata
// replace merges rather than replaces and so cannot delete one — see the note in internal/vfs/xattr.go.
// The consequence a user can see is that `head-object` still shows the key, holding a marker; `getfattr`
// does not show the attribute, which is the answer that matters.
func (f *FileNode) Removexattr(ctx context.Context, attr string) syscall.Errno {
	if f.fs.config != nil && f.fs.config.ReadOnly {
		return syscall.EROFS
	}
	if f.fs.buffer == nil {
		return syscall.ENOTSUP
	}
	if refuseXattrNamespace(attr) {
		// Nothing in this namespace is ever stored, so there is nothing to remove and the accurate answer
		// is absence rather than a refusal. ENOTSUP here would make `setfattr -x` on a name the filesystem
		// never stored look like a filesystem limitation instead of a nonexistent attribute.
		return errNoXattr
	}

	if err := f.fs.buffer.RemoveXattr(ctx, f.key(), attr); err != nil {
		if errors.Is(err, vfs.ErrNoXattr) {
			// Not logged at error level: removing an attribute that is not there is an ordinary answer to
			// an ordinary question, and `setfattr -x` on a fresh file is how a script checks.
			return errNoXattr
		}
		slog.Error("removexattr failed", "path", f.key(), "attr", attr, "error", err)

		return toErrno(err)
	}

	return f.flushXattr(ctx, "removexattr", attr)
}

// flushXattr makes a pending extended-attribute change durable and drops every cached view of the file.
//
// Shared by the two write paths because the sequence is identical and because forgetting the
// invalidation is the half that is invisible on a single-node mount: a stale metadata cache entry would
// make the next getxattr report the value from before the write, on this host or on a peer.
func (f *FileNode) flushXattr(ctx context.Context, op, attr string) syscall.Errno {
	etag, err := f.fs.flushReportingETag(ctx, f.key())
	if err != nil {
		slog.Error("could not persist an extended attribute change",
			"op", op, "path", f.key(), "attr", attr, "error", err)

		return toErrno(err)
	}

	f.fs.invalidateBoth(ctx, f.key(), etag)

	return 0
}

// Getxattr on a directory reports that it has no attributes, because it cannot have any.
//
// A directory here is a key prefix, not an object, so there is no metadata to read one from — the same
// reason [DirectoryNode.Setattr] refuses chmod. Absence is the accurate answer and it is free: no
// request is issued.
func (n *DirectoryNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	return 0, errNoXattr
}

// Listxattr on a directory succeeds with an empty list.
//
// Failing instead would make `cp -a`, `rsync -X`, and `ls -@` report an error for every directory they
// walk, on a mount where the answer is simply that there are none — the same reasoning that has
// [DirectoryNode.Setattr] accept a time change as a no-op rather than refusing it.
func (n *DirectoryNode) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	return 0, 0
}

// Setxattr on a directory refuses, rather than accepting a change it could not report back.
//
// Mkdir does write a zero-byte marker object, and an object can carry metadata, so this is not
// impossible — it is unimplemented, and the reason it stays unimplemented is the one
// [DirectoryNode.Setattr] gives for chmod: a directory that exists only because objects share a prefix
// has no marker to hold anything, so an attribute would persist for some directories and vanish for
// others depending on how the directory came to exist. `setfattr` reporting success while the next
// `getfattr` shows nothing is the same defect as an `rm` that reports success while the object survives.
//
// ENOTSUP rather than go-fuse's ENOATTR default, which is what this returned before it was implemented:
// "no such attribute" is a strange answer to a request to create one, and a caller distinguishing "this
// filesystem cannot" from "that attribute is missing" gets the truth from ENOTSUP.
func (n *DirectoryNode) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	if n.fs.config != nil && n.fs.config.ReadOnly {
		return syscall.EROFS
	}

	slog.Warn("extended attributes on a directory are not implemented; refusing rather than reporting a "+
		"change that would not be visible on the next getfattr",
		"path", n.key(), "attr", attr, "issue", 167)

	return syscall.ENOTSUP
}

// Removexattr on a directory reports absence, since a directory never has an attribute to remove.
//
// Not ENOTSUP, unlike the set above: the set is refusing something it could conceivably do, while this
// is answering accurately about a file that has no attributes. `setfattr -x` on a directory should look
// like a missing attribute, which it is.
func (n *DirectoryNode) Removexattr(ctx context.Context, attr string) syscall.Errno {
	if n.fs.config != nil && n.fs.config.ReadOnly {
		return syscall.EROFS
	}

	return errNoXattr
}
