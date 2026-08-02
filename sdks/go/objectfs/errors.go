package objectfs

import pkgerrors "github.com/scttfrdmn/objectfs/pkg/errors"

// Sentinel errors for the ObjectFS SDK.
//
// Use errors.Is to check for these conditions:
//
//	if errors.Is(err, objectfs.ErrNotFound) { ... }
//
// The sentinels are matched by error code, so wrapped errors from the backend
// are also correctly identified.
var (
	// ErrNotFound is returned when the requested object does not exist.
	ErrNotFound = pkgerrors.NewError(pkgerrors.ErrCodeObjectNotFound, "object does not exist").
			WithComponent("objectfs-sdk")

	// ErrAccessDenied is returned when credentials or IAM permissions are insufficient.
	ErrAccessDenied = pkgerrors.NewError(pkgerrors.ErrCodeAccessDenied, "access denied").
			WithComponent("objectfs-sdk")

	// ErrNotMounted is returned when a FUSE operation is attempted before Mount.
	ErrNotMounted = pkgerrors.NewError(pkgerrors.ErrCodeNotInitialized, "filesystem not mounted").
			WithComponent("objectfs-sdk")

	// ErrAlreadyMounted is returned when Mount is called on an already-mounted client.
	ErrAlreadyMounted = pkgerrors.NewError(pkgerrors.ErrCodeAlreadyStarted, "filesystem already mounted").
				WithComponent("objectfs-sdk")

	// ErrInvalidConfig is returned when a configuration option has an invalid value.
	ErrInvalidConfig = pkgerrors.NewError(pkgerrors.ErrCodeInvalidConfig, "invalid configuration").
				WithComponent("objectfs-sdk")
)
