//go:build linux || darwin

package fuse

import (
	"context"
	"errors"
	iofs "io/fs"
	"syscall"

	"github.com/scttfrdmn/objectfs/internal/vfs"
	objerrors "github.com/scttfrdmn/objectfs/pkg/errors"
)

// toErrno translates an error from internal/vfs, pkg/errors, or a backend into the errno the kernel
// receives.
//
// This is the only place in ObjectFS where syscall meets vfs, and it exists because the translation
// is a policy decision rather than a lookup. internal/vfs deliberately imports no syscall — its
// sentinels are the vocabulary, and each binding decides what its callers should see. A WinFsp shim
// would write its own NTSTATUS version of this function against the same sentinels.
//
// # The classification that matters
//
// Absence must be distinguished from every other failure, in both directions. v0.10.0's Lookup
// collapsed every HeadObject error to ENOENT, so a throttle or an AccessDenied read as "the file is
// not there" — and Create then wrote an empty object over a file that was merely temporarily
// unreachable. That is why an unrecognized error maps to EIO and never to ENOENT: EIO fails, ENOENT
// invites an overwrite.
//
// # Ordering
//
// The order of the checks below is load-bearing, not stylistic. [vfs.ErrBackend] wraps the
// underlying cause, so an AccessDenied arrives as an error satisfying errors.Is(err,
// vfs.ErrBackend) *and* carrying a PERMISSION code. Classifying ErrBackend first would flatten
// every such failure to EIO and lose the one distinction a user can act on. So the specific
// sentinels come first, then the coded classification, then ErrBackend as the residual.
func toErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}

	// The vfs vocabulary, most specific first. ErrBackend and ErrIntegrity are held back: both end at
	// EIO, and ErrBackend in particular is a wrapper that a finer classification below can improve on.
	switch {
	case errors.Is(err, vfs.ErrNotFound):
		return syscall.ENOENT
	case errors.Is(err, vfs.ErrExist):
		return syscall.EEXIST
	case errors.Is(err, vfs.ErrNotDir):
		return syscall.ENOTDIR
	case errors.Is(err, vfs.ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, vfs.ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, vfs.ErrReadOnly):
		return syscall.EROFS
	case errors.Is(err, vfs.ErrNotSupported):
		// ENOTSUP, not ENOSYS. The kernel remembers an ENOSYS from several FUSE operations and stops
		// issuing them for the lifetime of the mount, which would make "not supported yet" permanent
		// for the process even after a fix. ENOTSUP is also what go-fuse itself returns for an
		// unimplemented Setattr, so the two agree.
		return syscall.ENOTSUP
	case errors.Is(err, vfs.ErrInvalid):
		return syscall.EINVAL
	case errors.Is(err, vfs.ErrNoSpace):
		// ENOSPC, which is about the write buffer rather than the bucket — S3 has no capacity to
		// exhaust. It is the errno every program that writes files already handles, and the honest one:
		// the filesystem cannot accept more data right now. EDQUOT would suggest a per-user quota that
		// does not exist, and ENOMEM is not a documented write(2) error, so callers do not check it.
		return syscall.ENOSPC
	}

	// Coded backend failures. This runs before ErrBackend because ErrBackend wraps these.
	var coded *objerrors.ObjectFSError
	if errors.As(err, &coded) {
		switch coded.Code {
		case objerrors.ErrCodeAccessDenied,
			objerrors.ErrCodePermissionDenied,
			objerrors.ErrCodeAuthenticationFailed,
			objerrors.ErrCodeAuthorizationFailed,
			objerrors.ErrCodeCredentialsMissing:
			// EACCES rather than EPERM: the process lacks access to the object, which is not the same
			// as lacking the privilege to perform the operation. Tools report EACCES as
			// "Permission denied", which is the accurate description of an IAM refusal.
			return syscall.EACCES
		case objerrors.ErrCodeObjectNotFound, objerrors.ErrCodeFileNotFound, objerrors.ErrCodeBucketNotFound:
			return syscall.ENOENT
		case objerrors.ErrCodeQuotaExceeded:
			return syscall.EDQUOT
		case objerrors.ErrCodeNotEmpty:
			return syscall.ENOTEMPTY
		case objerrors.ErrCodeNotDirectory:
			return syscall.ENOTDIR
		case objerrors.ErrCodeDirectoryExists, objerrors.ErrCodeBucketExists:
			return syscall.EEXIST
		case objerrors.ErrCodePathInvalid, objerrors.ErrCodeValidationFailed:
			return syscall.EINVAL
		case objerrors.ErrCodeOperationTimeout, objerrors.ErrCodeConnectionTimeout:
			return syscall.ETIMEDOUT
		case objerrors.ErrCodeOperationCanceled:
			return syscall.EINTR
		default:
			// Every other code — DATA_CORRUPTION, SERVICE_UNAVAILABLE, NETWORK_ERROR, RETRY_EXHAUSTED —
			// falls through to EIO below. A filesystem has no errno for "the object store is unwell", and
			// inventing one that suggests retrying would be worse than EIO: the backend already retried.
			//
			// The arm is explicit rather than an omitted default so that this is a statement about the
			// remaining codes rather than an oversight — which is what the exhaustive linter is
			// configured to distinguish, and it cannot tell the two apart without it.
		}
	}

	// Absence reported by a backend that does not use the coded type. IsNotFound is the one classifier
	// allowed to make this call; see its doc comment for why it is deliberately conservative.
	if vfs.IsNotFound(err) {
		return syscall.ENOENT
	}

	// An interrupted or timed-out operation. go-fuse cancels the operation context when the kernel
	// sends INTERRUPT, and EINTR is what the caller's syscall should report in that case.
	switch {
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	case errors.Is(err, context.DeadlineExceeded):
		return syscall.ETIMEDOUT
	}

	// io/fs sentinels, for a types.Backend implemented over a local filesystem — which is what
	// internal/difftest uses as its oracle.
	switch {
	case errors.Is(err, iofs.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, iofs.ErrExist):
		return syscall.EEXIST
	case errors.Is(err, iofs.ErrPermission):
		return syscall.EACCES
	case errors.Is(err, iofs.ErrInvalid):
		return syscall.EINVAL
	}

	// A concrete errno somewhere in the chain is already the answer. This is last among the typed
	// checks so that vfs classification always wins over an incidental errno from a lower layer.
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}

	// ErrBackend and ErrIntegrity, plus everything unrecognized. EIO is the honest report for "the
	// filesystem could not do this and cannot say more"; it is also what a local filesystem returns
	// for unreadable media, so tools already handle it.
	return syscall.EIO
}
