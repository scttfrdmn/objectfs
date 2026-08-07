// Package filesystem declares a protocol-agnostic filesystem interface that nothing implements.
//
// # This is a design sketch, not a working contract
//
// The intent was for FUSE, SMB, and NFS handlers to share one backend abstraction. That has not
// happened and this package is not on the way there: it has **no importers anywhere in the tree**,
// and [FilesystemInterface]'s only implementation is `mockFilesystem` in this package's own test.
// The live path is `internal/fuse` → `internal/vfs` → `pkg/types.Backend`, which owes nothing to
// anything here.
//
// The distinction matters because this file reads like a capability list. It declares Rename,
// Truncate, Chmod, Chown, Link, Symlink, Readlink, four xattr methods, and Statfs — and a reader
// who takes a method here as evidence of support will be wrong about several of them. Symlinks,
// hard links, and xattrs are not implemented at all, and hard links never will be: S3 has no
// concept of two names for one object. See [internal/vfs.FileType], whose comment records that
// this interface advertising Symlink and Link with no implementation behind them is precisely what
// went wrong in v0.10.0.
//
// The supported-operations table in README.md is the authority, and it is derived from the methods
// that exist in `internal/fuse` and `internal/vfs`. Do not infer support from this file.
//
// Kept rather than deleted because the multi-protocol work it sketches is tracked (#181) and this
// records the original shape of it. If that work starts, this interface is a starting point to
// argue with — not a contract to fill in.
package filesystem

import (
	"context"
	"io"
	"os"
	"time"
)

// FilesystemInterface is the operation set a protocol handler would implement.
//
// Nothing implements it except this package's test mock, and the presence of a method here says
// nothing about whether the operation works on a mount. See the package comment.
type FilesystemInterface interface {
	// File operations
	Open(ctx context.Context, path string, flags int) (FileHandle, error)
	Create(ctx context.Context, path string, mode os.FileMode) (FileHandle, error)
	Close(ctx context.Context, fh FileHandle) error

	// I/O operations
	Read(ctx context.Context, fh FileHandle, buf []byte, offset int64) (int, error)
	Write(ctx context.Context, fh FileHandle, data []byte, offset int64) (int, error)
	Flush(ctx context.Context, fh FileHandle) error
	Sync(ctx context.Context, fh FileHandle) error

	// Directory operations
	ReadDir(ctx context.Context, path string) ([]DirEntry, error)
	Mkdir(ctx context.Context, path string, mode os.FileMode) error
	Rmdir(ctx context.Context, path string) error

	// File/directory manipulation
	Remove(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string) error

	// Metadata operations
	Stat(ctx context.Context, path string) (FileInfo, error)
	Chmod(ctx context.Context, path string, mode os.FileMode) error
	Chown(ctx context.Context, path string, uid, gid int) error
	Utimes(ctx context.Context, path string, atime, mtime time.Time) error
	Truncate(ctx context.Context, path string, size int64) error

	// Link operations
	Link(ctx context.Context, oldPath, newPath string) error
	Symlink(ctx context.Context, target, linkPath string) error
	Readlink(ctx context.Context, path string) (string, error)

	// Extended attributes (useful for storing S3 metadata)
	GetXattr(ctx context.Context, path string, name string) ([]byte, error)
	SetXattr(ctx context.Context, path string, name string, data []byte) error
	ListXattr(ctx context.Context, path string) ([]string, error)
	RemoveXattr(ctx context.Context, path string, name string) error

	// Filesystem-level operations
	Statfs(ctx context.Context, path string) (StatfsInfo, error)

	// ObjectFS-specific operations for enterprise features
	GetCostOptimization(ctx context.Context, path string) (*CostAnalysis, error)
	GetStorageTier(ctx context.Context, path string) (string, error)
	SetStorageTier(ctx context.Context, path string, tier string) error
	GetAccessPattern(ctx context.Context, path string) (*AccessPattern, error)
}

// FileHandle represents an open file handle that can be used for I/O operations
type FileHandle interface {
	io.Reader
	io.Writer
	io.Seeker
	io.Closer

	// Handle-specific operations
	ID() uint64
	Path() string
	Flags() int

	// S3-specific information
	S3Key() string
	StorageTier() string
	Size() int64
	LastModified() time.Time
}

// DirEntry represents a directory entry returned by ReadDir
type DirEntry struct {
	Name    string
	Type    FileType
	Size    int64
	Mode    os.FileMode
	ModTime time.Time
	IsDir   bool

	// S3-specific metadata
	S3Key       string
	StorageTier string
	ETag        string

	// Cost information (when available)
	StorageCost   float64 // Monthly storage cost in USD
	RetrievalCost float64 // Per-GB retrieval cost
	LastAccessed  *time.Time
}

// FileInfo represents file metadata, similar to os.FileInfo but with S3-specific fields
type FileInfo struct {
	Name_    string
	Size_    int64
	Mode_    os.FileMode
	ModTime_ time.Time
	IsDir_   bool

	// Extended S3 metadata
	S3Key       string
	StorageTier string
	ETag        string
	ContentType string
	Metadata    map[string]string

	// ObjectFS enterprise features
	CostAnalysis  *CostAnalysis
	AccessPattern *AccessPattern

	// POSIX compatibility
	Uid int
	Gid int
}

func (fi FileInfo) Name() string       { return fi.Name_ }
func (fi FileInfo) Size() int64        { return fi.Size_ }
func (fi FileInfo) Mode() os.FileMode  { return fi.Mode_ }
func (fi FileInfo) ModTime() time.Time { return fi.ModTime_ }
func (fi FileInfo) IsDir() bool        { return fi.IsDir_ }
func (fi FileInfo) Sys() any           { return nil }

// StatfsInfo represents filesystem statistics
type StatfsInfo struct {
	TotalBytes    uint64
	FreeBytes     uint64
	AvailBytes    uint64
	TotalInodes   uint64
	FreeInodes    uint64
	BlockSize     uint32
	MaxNameLength uint32

	// S3-specific information
	StorageCostPerMonth float64           // Total monthly cost
	ObjectCount         uint64            // Total objects in bucket
	TotalStorageClass   map[string]uint64 // Bytes per storage class
}

// FileType represents the type of a file system entry
type FileType uint8

const (
	FileTypeRegular FileType = iota
	FileTypeDirectory
	FileTypeSymlink
	FileTypeDevice
	FileTypeCharDevice
	FileTypeFIFO
	FileTypeSocket
	FileTypeUnknown
)

// CostAnalysis provides detailed cost information for a file or directory
type CostAnalysis struct {
	CurrentTier        string
	MonthlyStorageCost float64
	RetrievalCost      float64

	// Optimization recommendations
	RecommendedTier    string
	PotentialSavings   float64
	OptimizationReason string

	// Volume discount information
	VolumeDiscount float64
	EffectiveRate  float64

	// Access pattern insights
	AccessFrequency string // "frequent", "infrequent", "archive", "cold"
	LastAccessed    time.Time
	AccessCount     uint64
	ConfidenceScore float64 // 0-1, confidence in recommendations
}

// AccessPattern tracks how files are accessed to inform cost optimization
type AccessPattern struct {
	ReadCount       uint64
	WriteCount      uint64
	LastRead        time.Time
	LastWrite       time.Time
	AccessFrequency string // "frequent", "infrequent", "archive", "cold"
	ReadBytes       uint64
	WriteBytes      uint64

	// Predictive analytics
	PredictedNextAccess time.Time
	SeasonalPattern     bool
	AccessTrend         string // "increasing", "decreasing", "stable"
}

// ContextKey is the type of the keys this package uses with [context.WithValue], carrying
// per-request information a protocol handler knows and the filesystem operations below do not.
//
// A named string type rather than a bare string, for the reason the context documentation gives: a
// bare string key collides silently with any other package that happens to use the same word, and
// "protocol" is not an unlikely word. The constants below group by protocol, and only the ContextKey*
// values in the Common block are meaningful across all of them.
type ContextKey string

const (
	// SMB-specific context
	ContextKeySMBUser    ContextKey = "smb_user"
	ContextKeySMBShare   ContextKey = "smb_share"
	ContextKeySMBSession ContextKey = "smb_session"

	// FUSE-specific context
	ContextKeyFUSEPid ContextKey = "fuse_pid"
	ContextKeyFUSEUid ContextKey = "fuse_uid"
	ContextKeyFUSEGid ContextKey = "fuse_gid"

	// NFS-specific context
	ContextKeyNFSClient ContextKey = "nfs_client"
	ContextKeyNFSExport ContextKey = "nfs_export"

	// Common context
	ContextKeyProtocol  ContextKey = "protocol" // "fuse", "smb", "nfs"
	ContextKeyClientIP  ContextKey = "client_ip"
	ContextKeyRequestID ContextKey = "request_id"
)

// GetProtocol returns the protocol name a request arrived over — "fuse", "smb" or "nfs" — or
// "unknown" if the context carries no [ContextKeyProtocol] value or one that is not a string.
//
// It returns a sentinel rather than a (value, ok) pair, which suits a logging or metrics label and
// makes "the key was absent" indistinguishable from "the protocol is literally unknown". That is
// worth knowing before the first real caller exists, and as of now there is none outside this
// package's own test — nor does anything set [ContextKeyProtocol] outside it. The same holds for
// [GetClientIP] and [GetRequestID], which return "" for want of a non-empty sentinel. This is a
// declared surface rather than a used one, like the rest of the file; the package comment is the
// authority on that and README.md's supported-operations table on what actually works.
func GetProtocol(ctx context.Context) string {
	if protocol, ok := ctx.Value(ContextKeyProtocol).(string); ok {
		return protocol
	}
	return "unknown"
}

func GetClientIP(ctx context.Context) string {
	if ip, ok := ctx.Value(ContextKeyClientIP).(string); ok {
		return ip
	}
	return ""
}

func GetRequestID(ctx context.Context) string {
	if id, ok := ctx.Value(ContextKeyRequestID).(string); ok {
		return id
	}
	return ""
}

// FilesystemError is an operation, a path, and the underlying error, in the shape [os.PathError] uses.
//
// It implements Unwrap, so the sentinels below compare with [errors.Is] against the [os] error values
// they wrap — ErrNotExist matches os.ErrNotExist. Those sentinels carry an empty Op and Path, so they
// are values to compare against and not errors to return: returning one loses the operation and path
// that are this type's reason for existing, and Error() would render as " : file does not exist".
//
// Note that ErrTierNotSupported and ErrInvalid both wrap [os.ErrInvalid], so errors.Is against
// os.ErrInvalid cannot tell them apart — though it does distinguish the two from each other, since
// each is its own pointer and neither wraps the other. Measured, not inferred: Is(ErrTierNotSupported,
// os.ErrInvalid) is true and Is(ErrTierNotSupported, ErrInvalid) is false.
type FilesystemError struct {
	Op   string
	Path string
	Err  error
}

func (e *FilesystemError) Error() string {
	return e.Op + " " + e.Path + ": " + e.Err.Error()
}

func (e *FilesystemError) Unwrap() error {
	return e.Err
}

// Common error variables
var (
	ErrNotExist         = &FilesystemError{Err: os.ErrNotExist}
	ErrPermission       = &FilesystemError{Err: os.ErrPermission}
	ErrExist            = &FilesystemError{Err: os.ErrExist}
	ErrInvalid          = &FilesystemError{Err: os.ErrInvalid}
	ErrTierNotSupported = &FilesystemError{Err: os.ErrInvalid}
)
