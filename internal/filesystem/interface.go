// Package filesystem defines the common interface that all protocol handlers use
// to interact with the ObjectFS backend. This abstraction allows FUSE, SMB, NFS,
// and other protocols to share the same S3 backend, cost optimization, and
// enterprise pricing features without code duplication.
package filesystem

import (
	"context"
	"io"
	"os"
	"time"
)

// FilesystemInterface defines the common operations that all protocol handlers
// need to perform. This interface abstracts away the specific S3 backend
// implementation and allows multiple protocols to operate on the same data.
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
func (fi FileInfo) Sys() interface{}   { return nil }

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

// Protocol-specific context keys for passing additional information
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

// Helper functions for protocol handlers
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

// Error types specific to filesystem operations
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
