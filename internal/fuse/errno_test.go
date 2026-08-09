//go:build linux || darwin

package fuse

// toErrno is where syscall meets vfs, and it is the one function in this package whose defects are
// invisible to every other test.
//
// A wrong errno does not fail an operation — it fails it *differently*, and callers branch on the
// difference. v0.10.0's Lookup collapsed every HeadObject error to ENOENT, and the consequence was not
// a bad error message: Create read "the file is not there" and wrote an empty object over a file that
// was merely throttled. Every case below is a decision some caller acts on, which is why this is
// tested by table rather than incidentally through the operations that call it.

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scttfrdmn/objectfs/internal/vfs"
	objerrors "github.com/scttfrdmn/objectfs/pkg/errors"
)

// coded builds an ObjectFSError with a code, which is the shape the S3 backend produces.
func coded(code objerrors.ErrorCode) error {
	return objerrors.NewError(code, "synthetic error for the errno table")
}

func TestToErrnoClassifiesEveryVocabularyItReceives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want syscall.Errno

		// why records the caller decision that depends on this mapping, for a failure message that
		// says what breaks rather than which constant differs.
		why string
	}{
		{
			name: "nil is success",
			err:  nil,
			want: 0,
			why:  "a nil error must not become an errno at all",
		},

		// The vfs vocabulary. These are the sentinels internal/vfs returns, and the reason it imports
		// no syscall: the binding decides what its own callers see.
		{
			name: "ErrNotFound",
			err:  vfs.ErrNotFound,
			want: syscall.ENOENT,
			why:  "absence is the one classification that licenses a caller to create the object",
		},
		{
			name: "ErrExist",
			err:  vfs.ErrExist,
			want: syscall.EEXIST,
			why:  "O_EXCL depends on this to refuse rather than truncate",
		},
		{
			name: "ErrNotDir",
			err:  vfs.ErrNotDir,
			want: syscall.ENOTDIR,
			why:  "path resolution walks components and needs to stop at a non-directory",
		},
		{
			name: "ErrIsDir",
			err:  vfs.ErrIsDir,
			want: syscall.EISDIR,
			why:  "open(2) for write on a directory must fail as a directory, not as a missing file",
		},
		{
			name: "ErrNotEmpty",
			err:  vfs.ErrNotEmpty,
			want: syscall.ENOTEMPTY,
			why:  "rmdir(2) reports this and `rm -r` recurses on it",
		},
		{
			name: "ErrReadOnly",
			err:  vfs.ErrReadOnly,
			want: syscall.EROFS,
			why:  "tools retry on EACCES and give up on EROFS, which is the accurate one for a read-only mount",
		},
		{
			name: "ErrNotSupported",
			err:  vfs.ErrNotSupported,
			want: syscall.ENOTSUP,
			why: "ENOSYS would be remembered by the kernel for the life of the mount, making " +
				"'not implemented yet' permanent for the process even after a fix",
		},
		{
			name: "ErrInvalid",
			err:  vfs.ErrInvalid,
			want: syscall.EINVAL,
			why:  "a rejected argument is the caller's to fix, not a filesystem failure",
		},
		{
			name: "ErrNoSpace",
			err:  vfs.ErrNoSpace,
			want: syscall.ENOSPC,
			why: "every program that writes files already handles ENOSPC; EDQUOT would suggest a per-user " +
				"quota that does not exist and ENOMEM is not a documented write(2) error, so callers do " +
				"not check for it",
		},
		{
			name: "ErrTooLarge",
			err:  vfs.ErrTooLarge,
			want: syscall.E2BIG,
			why: "setxattr(2)'s errno for a value no object could hold. It has to differ from the ENOSPC " +
				"above: a caller that frees an attribute and retries is right for ENOSPC and loops " +
				"forever for E2BIG",
		},
		{
			name: "ErrNoXattr",
			err:  vfs.ErrNoXattr,
			want: syscall.Errno(fuse.ENOATTR),
			why: "ENOATTR on darwin, ENODATA on linux — and never ENOENT, which would tell a caller " +
				"probing for an attribute that the file itself had disappeared underneath it",
		},
		{
			name: "ErrIntegrity",
			err:  vfs.ErrIntegrity,
			want: syscall.EIO,
			why: "a checksum mismatch is unreadable media as far as a caller is concerned; anything " +
				"suggesting a retry would invite reading the corrupt bytes again",
		},
		{
			name: "ErrBackend",
			err:  vfs.ErrBackend,
			want: syscall.EIO,
			why: "the residual: EIO fails the operation, and the alternative of ENOENT invites an " +
				"overwrite of an object that is merely unreachable",
		},

		// A wrapped sentinel must classify as the sentinel. Callers in vfs wrap with %w and context,
		// and a check that only compared identity would silently fall through to EIO.
		{
			name: "wrapped ErrNotFound",
			err:  fmt.Errorf("open %q: %w", "some/path", vfs.ErrNotFound),
			want: syscall.ENOENT,
			why:  "vfs wraps its sentinels with context and errors.Is has to see through it",
		},

		// Ordering: ErrBackend wraps the underlying cause, so a coded AccessDenied arrives satisfying
		// errors.Is(err, ErrBackend) *and* carrying a PERMISSION code. Classifying ErrBackend first
		// would flatten every such failure to EIO and lose the one distinction a user can act on.
		{
			name: "AccessDenied wrapped in ErrBackend keeps EACCES",
			err:  fmt.Errorf("%w: %w", vfs.ErrBackend, coded(objerrors.ErrCodeAccessDenied)),
			want: syscall.EACCES,
			why: "an IAM refusal reported as EIO is undiagnosable; the check order is what preserves " +
				"the actionable classification through the wrapper",
		},

		// Coded backend failures.
		{
			name: "ErrCodeAccessDenied",
			err:  coded(objerrors.ErrCodeAccessDenied),
			want: syscall.EACCES,
			why:  "tools render EACCES as 'Permission denied', which is what an IAM refusal is",
		},
		{
			name: "ErrCodePermissionDenied",
			err:  coded(objerrors.ErrCodePermissionDenied),
			want: syscall.EACCES,
			why:  "same class as AccessDenied from the caller's side",
		},
		{
			name: "ErrCodeAuthenticationFailed",
			err:  coded(objerrors.ErrCodeAuthenticationFailed),
			want: syscall.EACCES,
			why:  "bad credentials are a denial, not a missing file",
		},
		{
			name: "ErrCodeAuthorizationFailed",
			err:  coded(objerrors.ErrCodeAuthorizationFailed),
			want: syscall.EACCES,
			why:  "an authorization refusal is a denial",
		},
		{
			name: "ErrCodeCredentialsMissing",
			err:  coded(objerrors.ErrCodeCredentialsMissing),
			want: syscall.EACCES,
			why:  "no credentials is a denial the operator can act on",
		},
		{
			name: "ErrCodeObjectNotFound",
			err:  coded(objerrors.ErrCodeObjectNotFound),
			want: syscall.ENOENT,
			why:  "the backend's own absence code, which the read path relies on",
		},
		{
			name: "ErrCodeFileNotFound",
			err:  coded(objerrors.ErrCodeFileNotFound),
			want: syscall.ENOENT,
			why:  "absence via the filesystem-flavored code",
		},
		{
			name: "ErrCodeBucketNotFound",
			err:  coded(objerrors.ErrCodeBucketNotFound),
			want: syscall.ENOENT,
			why:  "a missing bucket makes every path under it absent",
		},
		{
			name: "ErrCodeQuotaExceeded",
			err:  coded(objerrors.ErrCodeQuotaExceeded),
			want: syscall.EDQUOT,
			why:  "EDQUOT tells a caller the write will not succeed by retrying, which ENOSPC also would not",
		},
		{
			name: "ErrCodeNotEmpty",
			err:  coded(objerrors.ErrCodeNotEmpty),
			want: syscall.ENOTEMPTY,
			why:  "same as the vfs sentinel, reached from a backend that codes instead",
		},
		{
			name: "ErrCodeNotDirectory",
			err:  coded(objerrors.ErrCodeNotDirectory),
			want: syscall.ENOTDIR,
			why:  "path resolution needs the same answer whichever layer noticed",
		},
		{
			name: "ErrCodeDirectoryExists",
			err:  coded(objerrors.ErrCodeDirectoryExists),
			want: syscall.EEXIST,
			why:  "mkdir(2) reports EEXIST",
		},
		{
			name: "ErrCodeBucketExists",
			err:  coded(objerrors.ErrCodeBucketExists),
			want: syscall.EEXIST,
			why:  "a bucket that already exists is the same refusal as a directory that does",
		},
		{
			name: "ErrCodePathInvalid",
			err:  coded(objerrors.ErrCodePathInvalid),
			want: syscall.EINVAL,
			why:  "a key S3 cannot represent is the caller's to fix",
		},
		{
			name: "ErrCodeValidationFailed",
			err:  coded(objerrors.ErrCodeValidationFailed),
			want: syscall.EINVAL,
			why:  "a rejected argument, from the backend's validator rather than vfs's",
		},
		{
			name: "ErrCodeOperationTimeout",
			err:  coded(objerrors.ErrCodeOperationTimeout),
			want: syscall.ETIMEDOUT,
			why:  "a timeout is retryable and ETIMEDOUT says so; EIO does not",
		},
		{
			name: "ErrCodeConnectionTimeout",
			err:  coded(objerrors.ErrCodeConnectionTimeout),
			want: syscall.ETIMEDOUT,
			why:  "same class, at connection setup",
		},
		{
			name: "ErrCodeOperationCanceled",
			err:  coded(objerrors.ErrCodeOperationCanceled),
			want: syscall.EINTR,
			why:  "a canceled operation is what the caller's syscall should report as interrupted",
		},

		// The default arm: every remaining code ends at EIO. Asserted rather than assumed, because the
		// arm is explicit precisely so this is a statement about the remaining codes and not an
		// oversight — and an assertion is the only thing that keeps it a statement.
		{
			name: "ErrCodeDataCorruption falls through to EIO",
			err:  coded(objerrors.ErrCodeDataCorruption),
			want: syscall.EIO,
			why: "a filesystem has no errno for 'the stored bytes are wrong', and inventing one that " +
				"suggested a retry would be worse than EIO",
		},
		{
			name: "ErrCodeServiceUnavailable falls through to EIO",
			err:  coded(objerrors.ErrCodeServiceUnavailable),
			want: syscall.EIO,
			why:  "there is no errno for 'the object store is unwell'; the backend already retried",
		},
		{
			name: "ErrCodeNetworkError falls through to EIO",
			err:  coded(objerrors.ErrCodeNetworkError),
			want: syscall.EIO,
			why:  "the retryer has already exhausted its budget by the time this surfaces",
		},
		{
			name: "ErrCodeRetryExhausted falls through to EIO",
			err:  coded(objerrors.ErrCodeRetryExhausted),
			want: syscall.EIO,
			why:  "retry exhausted is definitionally not worth reporting as retryable",
		},

		// Absence from a backend that does not use the coded type. IsNotFound is the only classifier
		// allowed to make this call.
		{
			name: "S3 NoSuchKey by error code",
			err:  codedError{code: "NoSuchKey"},
			want: syscall.ENOENT,
			why:  "an SDK error that only carries a code string still has to classify as absence",
		},
		{
			name: "S3 NotFound by error code",
			err:  codedError{code: "NotFound"},
			want: syscall.ENOENT,
			why: "HeadObject reports absence as NotFound rather than NoSuchKey, which is the exact " +
				"distinction DeleteObject got wrong",
		},

		// Cancellation and deadlines. go-fuse cancels the operation context when the kernel sends
		// INTERRUPT.
		{
			name: "context.Canceled",
			err:  context.Canceled,
			want: syscall.EINTR,
			why:  "the kernel sent INTERRUPT and the caller's syscall should report EINTR",
		},
		{
			name: "context.DeadlineExceeded",
			err:  context.DeadlineExceeded,
			want: syscall.ETIMEDOUT,
			why:  "a deadline is a timeout, and distinguishing it from a cancel is what tells an operator which",
		},
		{
			name: "wrapped context.Canceled",
			err:  fmt.Errorf("flush %q: %w", "some/path", context.Canceled),
			want: syscall.EINTR,
			why:  "cancellation arrives wrapped from every layer it passes through",
		},

		// io/fs sentinels, for a types.Backend over a local filesystem — which is what internal/difftest
		// uses as its oracle, so these are not hypothetical.
		{
			name: "fs.ErrNotExist",
			err:  iofs.ErrNotExist,
			want: syscall.ENOENT,
			why:  "the difftest oracle is a local filesystem and returns the standard library's sentinels",
		},
		{
			name: "fs.ErrExist",
			err:  iofs.ErrExist,
			want: syscall.EEXIST,
			why:  "same, for a create that collides",
		},
		{
			name: "fs.ErrPermission",
			err:  iofs.ErrPermission,
			want: syscall.EACCES,
			why:  "same, for a mode refusal",
		},
		{
			name: "fs.ErrInvalid",
			err:  iofs.ErrInvalid,
			want: syscall.EINVAL,
			why:  "same, for a rejected argument",
		},

		// A concrete errno in the chain is already the answer.
		{
			name: "a bare errno passes through",
			err:  syscall.ENOSPC,
			want: syscall.ENOSPC,
			why:  "a lower layer that already produced an errno knows more than a re-derivation would",
		},
		{
			name: "a wrapped errno passes through",
			err:  fmt.Errorf("write: %w", syscall.EDQUOT),
			want: syscall.EDQUOT,
			why:  "an errno wrapped with context is still the answer",
		},

		// The residual. This is the case that must never be ENOENT.
		{
			name: "an unrecognized error is EIO, never ENOENT",
			err:  errors.New("something nobody has classified"),
			want: syscall.EIO,
			why: "EIO fails the operation; ENOENT invites an overwrite. This is the defect that let " +
				"Create zero an intact object",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := toErrno(tt.err); got != tt.want {
				t.Errorf("toErrno(%v) = %v (%s), want %v (%s)\n%s",
					tt.err, got, errnoName(got), tt.want, errnoName(tt.want), tt.why)
			}
		})
	}
}

// TestToErrnoPrefersSpecificClassificationOverErrBackend states the ordering property directly rather
// than leaving it implicit in one table row.
//
// ErrBackend is a wrapper, so a coded cause is *always* also an ErrBackend. Whether the specific
// classification survives depends entirely on which check runs first, and nothing but a test makes that
// ordering load-bearing rather than incidental.
func TestToErrnoPrefersSpecificClassificationOverErrBackend(t *testing.T) {
	t.Parallel()

	specific := map[objerrors.ErrorCode]syscall.Errno{
		objerrors.ErrCodeAccessDenied:      syscall.EACCES,
		objerrors.ErrCodeObjectNotFound:    syscall.ENOENT,
		objerrors.ErrCodeQuotaExceeded:     syscall.EDQUOT,
		objerrors.ErrCodeOperationTimeout:  syscall.ETIMEDOUT,
		objerrors.ErrCodeOperationCanceled: syscall.EINTR,
	}

	for code, want := range specific {
		t.Run(string(code), func(t *testing.T) {
			t.Parallel()

			wrapped := fmt.Errorf("%w: %w", vfs.ErrBackend, coded(code))

			if !errors.Is(wrapped, vfs.ErrBackend) {
				t.Fatal("the fixture does not satisfy errors.Is(err, ErrBackend), so it is not testing " +
					"the ordering it claims to")
			}

			if got := toErrno(wrapped); got != want {
				t.Errorf("a %s wrapped in ErrBackend classified as %v (%s), want %v (%s). Checking "+
					"ErrBackend before the coded cause flattens every backend failure to EIO and "+
					"discards the one distinction a user can act on.",
					code, got, errnoName(got), want, errnoName(want))
			}
		})
	}
}

// codedError is an error carrying only an SDK-style code string, which is the shape vfs.IsNotFound
// classifies by. The AWS SDK's own error types satisfy this interface.
type codedError struct {
	code string
}

func (e codedError) Error() string     { return "api error " + e.code }
func (e codedError) ErrorCode() string { return e.code }

// errnoName renders an errno as its symbolic name, so a failure says ENOENT rather than 2.
func errnoName(e syscall.Errno) string {
	if e == 0 {
		return "success"
	}

	return e.Error()
}
