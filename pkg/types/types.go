package types

import (
	"time"

	"github.com/objectfs/objectfs/internal/config"
)

// ObjectInfo represents metadata about an object
type ObjectInfo struct {
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	LastModified time.Time         `json:"last_modified"`
	ETag         string            `json:"etag"`
	ContentType  string            `json:"content_type"`
	Metadata     map[string]string `json:"metadata"`
	Checksum     string            `json:"checksum"`
}

// CacheStats represents cache performance statistics
type CacheStats struct {
	Hits        uint64  `json:"hits"`
	Misses      uint64  `json:"misses"`
	Evictions   uint64  `json:"evictions"`
	Size        int64   `json:"size"`
	Capacity    int64   `json:"capacity"`
	HitRate     float64 `json:"hit_rate"`
	Utilization float64 `json:"utilization"`
}

// AccessPattern represents file access patterns for ML prediction
type AccessPattern struct {
	Path        string      `json:"path"`
	Frequency   int64       `json:"frequency"`
	LastAccess  time.Time   `json:"last_access"`
	AccessTimes []time.Time `json:"access_times"`
	ReadRanges  []Range     `json:"read_ranges"`
	Sequential  bool        `json:"sequential"`
	Stride      int64       `json:"stride"`
	Confidence  float64     `json:"confidence"`
	FileSize    int64       `json:"file_size"`
	ContentType string      `json:"content_type"`
}

// Range represents a byte range
type Range struct {
	Offset int64 `json:"offset"`
	Size   int64 `json:"size"`
}

// PrefetchCandidate represents a file/range to prefetch
type PrefetchCandidate struct {
	Path     string    `json:"path"`
	Offset   int64     `json:"offset"`
	Size     int64     `json:"size"`
	Priority int       `json:"priority"`
	Deadline time.Time `json:"deadline"`
}

// HealthStatus represents the health status of a component
type HealthStatus struct {
	Status     string            `json:"status"`
	LastCheck  time.Time         `json:"last_check"`
	Response   time.Duration     `json:"response_time"`
	ErrorCount int64             `json:"error_count"`
	Message    string            `json:"message"`
	Details    map[string]string `json:"details"`
}

// ConnectionStats represents connection pool statistics
type ConnectionStats struct {
	Active      int           `json:"active"`
	Idle        int           `json:"idle"`
	Total       int           `json:"total"`
	MaxOpen     int           `json:"max_open"`
	Lifetime    time.Duration `json:"lifetime"`
	IdleTimeout time.Duration `json:"idle_timeout"`
}

// FileMetadata represents POSIX file metadata
type FileMetadata struct {
	Path       string            `json:"path"`
	Size       int64             `json:"size"`
	Mode       uint32            `json:"mode"`
	UID        uint32            `json:"uid"`
	GID        uint32            `json:"gid"`
	AccessTime time.Time         `json:"atime"`
	ModifyTime time.Time         `json:"mtime"`
	ChangeTime time.Time         `json:"ctime"`
	IsDir      bool              `json:"is_dir"`
	Attributes map[string]string `json:"attributes"`
	Checksum   string            `json:"checksum"`
}

// WriteRequest represents a write operation
type WriteRequest struct {
	Path      string    `json:"path"`
	Offset    int64     `json:"offset"`
	Data      []byte    `json:"data"`
	Timestamp time.Time `json:"timestamp"`
	Sync      bool      `json:"sync"`
}

// ReadRequest represents a read operation
type ReadRequest struct {
	Path      string    `json:"path"`
	Offset    int64     `json:"offset"`
	Size      int64     `json:"size"`
	Timestamp time.Time `json:"timestamp"`
}

// PerformanceMetrics represents system performance metrics
type PerformanceMetrics struct {
	Timestamp        time.Time     `json:"timestamp"`
	ReadThroughput   float64       `json:"read_throughput"`
	WriteThroughput  float64       `json:"write_throughput"`
	ReadLatency      time.Duration `json:"read_latency"`
	WriteLatency     time.Duration `json:"write_latency"`
	CacheHitRate     float64       `json:"cache_hit_rate"`
	ActiveUsers      int64         `json:"active_users"`
	PendingRequests  int64         `json:"pending_requests"`
	ErrorRate        float64       `json:"error_rate"`
	NetworkBandwidth int64         `json:"network_bandwidth"`
}

// Configuration type aliases for backward compatibility.
// These types are now defined in internal/config and re-exported here to maintain
// compatibility with existing code. New code should import internal/config directly.
type (
	Configuration         = config.Configuration
	GlobalConfig          = config.GlobalConfig
	PerformanceConfig     = config.PerformanceConfig
	CacheConfig           = config.CacheConfig
	PersistentCacheConfig = config.PersistentCacheConfig
	WriteBufferConfig     = config.WriteBufferConfig
	CompressionConfig     = config.CompressionConfig
	NetworkConfig         = config.NetworkConfig
	TimeoutConfig         = config.TimeoutConfig
	RetryConfig           = config.RetryConfig
	CircuitBreakerConfig  = config.CircuitBreakerConfig
	SecurityConfig        = config.SecurityConfig
	TLSConfig             = config.TLSConfig
	EncryptionConfig      = config.EncryptionConfig
	MonitoringConfig      = config.MonitoringConfig
	MetricsConfig         = config.MetricsConfig
	HealthChecksConfig    = config.HealthChecksConfig
	LoggingConfig         = config.LoggingConfig
	SamplingConfig        = config.SamplingConfig
	FeatureConfig         = config.FeatureConfig
	StorageConfig         = config.StorageConfig
	S3Config              = config.S3Config
	S3CostOptimization    = config.S3CostOptimization
	ClusterConfig         = config.ClusterConfig
	OpenTelemetryConfig   = config.OpenTelemetryConfig
)
