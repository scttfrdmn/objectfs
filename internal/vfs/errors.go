package vfs

import "errors"

// Sentinel errors returned by this package. Callers translate these into whatever their binding
// requires — an errno for FUSE, an NTSTATUS for WinFsp, an NFS status code — which is why nothing
// here imports syscall.
//
// Match with errors.Is. Implementations wrap these with %w and add context; do not compare with ==.
var (
	// ErrNotFound reports that no file or directory exists at the given path.
	//
	// This must be returned only when absence is established. A backend error whose cause is
	// unknown — a throttle, a permission failure, a network timeout — is not absence, and reporting
	// it as such is how v0.10.0 came to overwrite intact objects: Lookup collapsed every
	// HeadObject error to ENOENT, and Create then wrote an empty object over a file that was
	// merely temporarily unreachable.
	ErrNotFound = errors.New("vfs: not found")

	// ErrExist reports that a file or directory already exists at the given path.
	ErrExist = errors.New("vfs: already exists")

	// ErrNotDir reports that a path component used as a directory is not one.
	ErrNotDir = errors.New("vfs: not a directory")

	// ErrIsDir reports that a directory was used where a file was required.
	ErrIsDir = errors.New("vfs: is a directory")

	// ErrNotEmpty reports that a directory being removed still has entries.
	ErrNotEmpty = errors.New("vfs: directory not empty")

	// ErrReadOnly reports that the filesystem, or this handle, is not writable.
	ErrReadOnly = errors.New("vfs: read-only")

	// ErrNotSupported reports an operation this implementation does not provide.
	//
	// Returning this is always preferable to returning success. Several go-fuse node interfaces
	// default an unimplemented operation to *success*, which is how rm and rmdir came to report
	// deletions that never happened through v0.10.0 — the kernel dropped the inode while the S3
	// object survived and kept billing.
	ErrNotSupported = errors.New("vfs: operation not supported")

	// ErrInvalid reports a malformed argument: a negative offset, an out-of-range length, a path
	// that escapes the root.
	ErrInvalid = errors.New("vfs: invalid argument")

	// ErrNoSpace reports that a resource limit is exhausted and flushing did not relieve it.
	//
	// This is about ObjectFS's own buffers, not the bucket's capacity — S3 has none. It exists
	// because the write path holds dirty byte ranges in memory until they are flushed, and
	// write_buffer.max_memory was declared, defaulted, and validated for three releases while being
	// read by nothing: the configuration reported a 512 MB ceiling and the process had none.
	//
	// Refusing a write is the last resort. A writer at its limit flushes to make room first, and this
	// is returned only when that failed to release enough — because a bound that rejects writes it
	// could have absorbed is worse than the unbounded growth it replaced.
	ErrNoSpace = errors.New("vfs: no space in write buffer")

	// ErrTooLarge reports that a single value is larger than this filesystem can store, whatever else
	// is or is not already stored alongside it.
	//
	// Distinct from ErrNoSpace, and the distinction is the caller's to act on. "This attribute cannot
	// fit on any object" tells a program to store less; "there is no room left on this file" tells it
	// to remove something first. setxattr(2) has separate errnos for the two — E2BIG and ENOSPC — so
	// collapsing them here would discard information the syscall interface is able to carry.
	ErrTooLarge = errors.New("vfs: value too large")

	// ErrNoXattr reports that a file has no extended attribute by that name.
	//
	// It must never be reported as ErrNotFound. Both would be an errno a caller sees from getxattr, but
	// ENOENT means the *file* does not exist, and tools branch on that: `getfattr` reports a missing
	// file rather than a missing attribute, and a caller probing for an attribute before creating one
	// could conclude the path is gone. This is the same class of misclassification as ErrNotFound's own
	// warning, at one level down.
	ErrNoXattr = errors.New("vfs: no such extended attribute")

	// ErrIntegrity reports that stored data could not be verified against its recorded checksum,
	// or that its storage encoding is not one this build can decode.
	//
	// This is deliberately distinct from a generic I/O error. Integrity failures must fail closed:
	// v0.10.0 returned raw compressed bytes with exit status 0 when an object's Content-Encoding
	// did not match the configured codec, which is worse than any error, because the caller cannot
	// tell that it happened. That particular mismatch is no longer reachable — the read path decodes
	// every algorithm ObjectFS can write, whatever a mount is configured to write — but a coding
	// ObjectFS does not implement, and a header lost after the write, still are.
	ErrIntegrity = errors.New("vfs: integrity check failed")

	// ErrBackend reports a storage-backend failure whose cause is not otherwise classified.
	// It explicitly does not mean the object is absent — see ErrNotFound.
	ErrBackend = errors.New("vfs: backend failure")
)
