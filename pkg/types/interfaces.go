package types

import (
	"context"
	"time"
)

// Backend defines the interface for object storage backends
type Backend interface {
	// GetObject returns bytes [offset, offset+size) of key. A size of zero or less means "to the end
	// of the object" — assigning no meaning to it is what made a negative size a reachable panic.
	GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error)

	// PutObject stores data as key, with meta as the object's user metadata. A nil meta is valid and
	// means "no attributes to record".
	//
	// The metadata parameter is not a convenience. POSIX mode, ownership, and mtime have no native
	// field in object storage, so user metadata is the only place they can live; a Put that cannot
	// carry them makes chmod and chown unimplementable, which is precisely the state v0.10.0 was in.
	//
	// Implementations own the integrity keys — objectfs-sha256 and objectfs-original-size — and must
	// ignore caller-supplied values for them. Those describe the bytes being stored, which only the
	// implementation has seen after compression, and a second writer for them would be a second
	// source of truth for the values integrity checking depends on.
	PutObject(ctx context.Context, key string, data []byte, meta map[string]string) error

	// SetObjectMetadata replaces key's user metadata without rewriting its contents, preserving every
	// other stored property — content encoding, content type, and storage class.
	//
	// It exists because a chmod is not a write. Persisting one through PutObject would mean reading
	// the whole object back and uploading it again: on a 10 GiB file, 20 GiB of transfer to change
	// nine bits. Implementations that have no in-place metadata operation should still preserve the
	// object's bytes and properties exactly.
	//
	// Preserving Content-Encoding is load-bearing rather than tidy. The read path dispatches decoding
	// on the stored encoding and fails closed when it cannot handle what it finds, so dropping the
	// header here would turn a chmod on a compressed object into an unreadable file.
	SetObjectMetadata(ctx context.Context, key string, meta map[string]string) error

	// CopyObject copies src to dst without transferring the object's bytes through this process,
	// preserving user metadata, content encoding, content type, and storage class. An existing dst is
	// replaced. A src that does not exist is an error a caller can classify as absence.
	//
	// It exists for rename. S3 has no rename for general-purpose buckets, so `mv` is a copy followed by
	// a delete, and the copy must be server-side: reading and rewriting through the client would turn
	// renaming a 10 GiB file into 20 GiB of transfer, and renaming a directory into that times the
	// number of objects under it.
	//
	// Every property named above must survive, for the reason spelled out on SetObjectMetadata: the read
	// path dispatches decoding on the stored Content-Encoding and fails closed on one it cannot handle,
	// so a copy that dropped the header would make a compressed object permanently unreadable — and one
	// that dropped the storage class would silently promote the object to STANDARD, billing the user for
	// a tier they did not choose. That is not hypothetical here; it is audit finding L26, observed on the
	// tier-transition path.
	//
	// Implementations must not treat this as atomic with respect to anything. It is one copy of one
	// object, and callers renaming a prefix are looping.
	CopyObject(ctx context.Context, src, dst string) error

	DeleteObject(ctx context.Context, key string) error
	HeadObject(ctx context.Context, key string) (*ObjectInfo, error)

	// GetObjects fetches several objects, returning the ones that were fetched and an error naming
	// every one that was not.
	//
	// A non-nil error with a non-empty map is normal and is how a partial batch is reported: a caller
	// that can use partial results reads the map, and a caller that needs all of them checks the
	// error. What implementations must not do is what the S3 backend did before this contract was
	// written — return a nil error unless *every* key failed (audit finding H11). The map is the only
	// other channel, and a missing entry is a nil slice, so a caller could not distinguish an object
	// that is absent from one whose GET was throttled. One key failing out of a thousand is both the
	// likely case and the one that was silent.
	//
	// Wrap the per-key failures (errors.Join is enough) rather than formatting them into a message, so
	// a caller can still ask with errors.Is whether the batch failed only on absent objects.
	GetObjects(ctx context.Context, keys []string) (map[string][]byte, error)

	// PutObjects stores several objects, returning an error naming every one that failed.
	//
	// It has no partial-success channel, so unlike GetObjects a non-nil error says only that at least
	// one object is not durable. Implementations must attempt every object rather than stopping at the
	// first failure: the caller cannot retry what was never tried, and cannot tell which those were.
	PutObjects(ctx context.Context, objects map[string][]byte) error

	// ListObjects returns the objects under prefix, at most limit of them. A limit of zero or less
	// means every object under the prefix.
	//
	// Implementations must paginate to satisfy the limit. S3's ListObjectsV2 caps a single response at
	// 1000 keys whatever MaxKeys says, so an implementation that issues one request silently truncates
	// — and a truncated listing is not a display problem: it is a directory whose later entries do not
	// exist as far as every caller is concerned, so `rm -r` reports success having deleted a prefix.
	ListObjects(ctx context.Context, prefix string, limit int) ([]ObjectInfo, error)

	// Health check
	HealthCheck(ctx context.Context) error
}

// DistributedCoordinator manages distributed operations across cluster nodes
type DistributedCoordinator interface {
	// Execute a distributed operation
	ExecuteOperation(ctx context.Context, op any) (any, error)

	// Get coordinator statistics
	GetStats() map[string]any
}

// Cache defines the caching interface for byte ranges of objects.
//
// Implementations cache at their own granularity and are free to hold more or less than any single
// Put supplied. What they may not do is answer a Get with bytes that were not read from the object at
// the requested offset: a caller hands what it gets straight to the kernel, so a short or shifted
// buffer becomes a short or corrupt file. A miss is always a safe answer; a wrong hit is not.
type Cache interface {
	// Get returns the cached bytes for [offset, offset+size), or nil if the cache does not hold all of
	// them. A partial hit is a miss — returning the prefix that happens to be held would present a
	// truncated file to the caller.
	//
	// A size of zero or less means "whatever contiguous bytes are held from offset", and may return
	// fewer bytes than exist in the object. It exists for callers caching a whole value whose length
	// only they know — the FUSE metadata cache stores a marshaled ObjectInfo and cannot state its
	// length at lookup time. Callers reading file content must pass the exact length they need, since
	// they cannot distinguish "this is all there is" from "this is all that is cached".
	//
	// The returned slice is the caller's own; implementations must not retain or reuse it.
	Get(key string, offset, size int64) []byte

	// Put offers bytes read from offset for caching. Implementations must copy what they keep: callers
	// pass buffers they may reuse.
	//
	// Where a Put overlaps bytes already held and disagrees with them, the newer bytes win — an
	// overwrite reaches the cache this way, and keeping the older copy would serve pre-write content.
	Put(key string, offset int64, data []byte)

	// Delete removes every byte cached for key, and nothing belonging to any other key. Callers rely on
	// this for write invalidation, so partial removal serves stale data and over-removal silently
	// discards unrelated objects' cached bytes.
	Delete(key string)

	// Evict frees at least size bytes if it can, reporting whether it succeeded.
	Evict(size int64) bool

	// Size reports the bytes currently held.
	Size() int64

	// Stats reports hit/miss counters and utilization.
	Stats() CacheStats
}

// WriteBuffer buffers writes to an object store, which cannot modify part of an object in place.
//
// It therefore holds pending writes and applies them as whole-object replacements at flush. That makes
// it the only component that knows a file's current contents between a write and its flush, so it must
// also answer reads: a read path that consults the object store and a cache but not the buffer returns
// pre-write bytes, which is what v0.10.0 did for up to the cache's five-minute TTL.
type WriteBuffer interface {
	// Write records data at offset for key. Nothing is uploaded; the write becomes durable at Flush.
	Write(key string, offset int64, data []byte) error

	// ReadAt fills buf with key's contents at offset — pending writes overlaid on the stored object —
	// and returns how many leading bytes of buf are valid, which may be short at end of file.
	//
	// Callers must prefer this to reading the backend directly. It is the only read that reflects writes
	// not yet flushed, and reading around it breaks read-your-own-writes on a single descriptor.
	ReadAt(ctx context.Context, key string, buf []byte, offset int64) (int, error)

	// FileSize reports key's logical length including pending writes, which is what stat must report and
	// what a read must clamp against. Distinct from Size, which reports buffered bytes held in memory.
	FileSize(ctx context.Context, key string) (int64, error)

	// Flush makes key durable, synchronously. It must return an error if the object was not stored:
	// close(2) and fsync(2) surface this errno, and it is the only place a program can learn that its
	// data never reached storage.
	Flush(key string) error

	// FlushAll makes every buffered key durable.
	FlushAll() error

	// Size reports the total buffered bytes held in memory across all keys.
	Size() int64

	// Count reports the number of keys with buffered writes.
	Count() int
}

// MetricsCollector defines the metrics collection interface
type MetricsCollector interface {
	RecordOperation(operation string, duration time.Duration, size int64, success bool)
	RecordCacheHit(key string, size int64)
	RecordCacheMiss(key string, size int64)
	RecordError(operation string, err error)
	GetMetrics() map[string]any
}

// ConfigManager defines configuration management interface
type ConfigManager interface {
	Get(key string) any
	GetString(key string) string
	GetInt(key string) int
	GetDuration(key string) time.Duration
	GetBool(key string) bool
	Watch(key string, callback func(any))
	Reload() error
}

// HealthChecker defines health monitoring interface
type HealthChecker interface {
	Check(ctx context.Context) HealthStatus
	RegisterCheck(name string, check func(context.Context) error)
	GetStatus() map[string]HealthStatus
}

// AccessPredictor defines predictive prefetching interface
type AccessPredictor interface {
	RecordAccess(path string, offset, size int64, timestamp time.Time)
	PredictNextAccess(path string) []PrefetchCandidate
	UpdateModel(patterns []AccessPattern)
	GetConfidence(path string) float64
}

// ConnectionManager defines connection pool management
type ConnectionManager interface {
	GetConnection() any
	ReturnConnection(conn any)
	HealthCheck() error
	ScalePool(targetSize int) error
	GetStats() ConnectionStats
}
