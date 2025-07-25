package types

import (
	"time"
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
	Path           string          `json:"path"`
	Frequency      int64           `json:"frequency"`
	LastAccess     time.Time       `json:"last_access"`
	AccessTimes    []time.Time     `json:"access_times"`
	ReadRanges     []Range         `json:"read_ranges"`
	Sequential     bool            `json:"sequential"`
	Stride         int64           `json:"stride"`
	Confidence     float64         `json:"confidence"`
	FileSize       int64           `json:"file_size"`
	ContentType    string          `json:"content_type"`
}

// Range represents a byte range
type Range struct {
	Offset int64 `json:"offset"`
	Size   int64 `json:"size"`
}

// PrefetchCandidate represents a file/range to prefetch
type PrefetchCandidate struct {
	Path     string `json:"path"`
	Offset   int64  `json:"offset"`
	Size     int64  `json:"size"`
	Priority int    `json:"priority"`
	Deadline time.Time `json:"deadline"`
}

// HealthStatus represents the health status of a component
type HealthStatus struct {
	Status      string            `json:"status"`
	LastCheck   time.Time         `json:"last_check"`
	Response    time.Duration     `json:"response_time"`
	ErrorCount  int64             `json:"error_count"`
	Message     string            `json:"message"`
	Details     map[string]string `json:"details"`
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
	Path         string            `json:"path"`
	Size         int64             `json:"size"`
	Mode         uint32            `json:"mode"`
	UID          uint32            `json:"uid"`
	GID          uint32            `json:"gid"`
	AccessTime   time.Time         `json:"atime"`
	ModifyTime   time.Time         `json:"mtime"`
	ChangeTime   time.Time         `json:"ctime"`
	IsDir        bool              `json:"is_dir"`
	Attributes   map[string]string `json:"attributes"`
	Checksum     string            `json:"checksum"`
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

// Configuration represents the main configuration structure
type Configuration struct {
	Global      GlobalConfig      `yaml:"global"`
	Performance PerformanceConfig `yaml:"performance"`
	Cache       CacheConfig       `yaml:"cache"`
	WriteBuffer WriteBufferConfig `yaml:"write_buffer"`
	Network     NetworkConfig     `yaml:"network"`
	Security    SecurityConfig    `yaml:"security"`
	Monitoring  MonitoringConfig  `yaml:"monitoring"`
	Features    FeatureConfig     `yaml:"features"`
}

// GlobalConfig represents global settings
type GlobalConfig struct {
	LogLevel    string `yaml:"log_level"`
	LogFile     string `yaml:"log_file"`
	MetricsPort int    `yaml:"metrics_port"`
	HealthPort  int    `yaml:"health_port"`
	ProfilePort int    `yaml:"profile_port"`
}

// PerformanceConfig represents performance settings
type PerformanceConfig struct {
	CacheSize         string `yaml:"cache_size"`
	WriteBufferSize   string `yaml:"write_buffer_size"`
	MaxConcurrency    int    `yaml:"max_concurrency"`
	ReadAheadSize     string `yaml:"read_ahead_size"`
	CompressionEnabled bool   `yaml:"compression_enabled"`
	ConnectionPoolSize int    `yaml:"connection_pool_size"`
}

// CacheConfig represents cache configuration
type CacheConfig struct {
	TTL           time.Duration     `yaml:"ttl"`
	MaxEntries    int               `yaml:"max_entries"`
	EvictionPolicy string           `yaml:"eviction_policy"`
	PersistentCache PersistentCacheConfig `yaml:"persistent_cache"`
}

// PersistentCacheConfig represents persistent cache settings
type PersistentCacheConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Directory string `yaml:"directory"`
	MaxSize   string `yaml:"max_size"`
}

// WriteBufferConfig represents write buffer configuration
type WriteBufferConfig struct {
	FlushInterval  time.Duration     `yaml:"flush_interval"`
	MaxBuffers     int               `yaml:"max_buffers"`
	MaxMemory      string            `yaml:"max_memory"`
	Compression    CompressionConfig `yaml:"compression"`
}

// CompressionConfig represents compression settings
type CompressionConfig struct {
	Enabled   bool   `yaml:"enabled"`
	MinSize   string `yaml:"min_size"`
	Algorithm string `yaml:"algorithm"`
	Level     int    `yaml:"level"`
}

// NetworkConfig represents network configuration
type NetworkConfig struct {
	Timeouts      TimeoutConfig      `yaml:"timeouts"`
	Retry         RetryConfig        `yaml:"retry"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
}

// TimeoutConfig represents timeout settings
type TimeoutConfig struct {
	Connect time.Duration `yaml:"connect"`
	Read    time.Duration `yaml:"read"`
	Write   time.Duration `yaml:"write"`
}

// RetryConfig represents retry settings
type RetryConfig struct {
	MaxAttempts int           `yaml:"max_attempts"`
	BaseDelay   time.Duration `yaml:"base_delay"`
	MaxDelay    time.Duration `yaml:"max_delay"`
}

// CircuitBreakerConfig represents circuit breaker settings
type CircuitBreakerConfig struct {
	Enabled          bool          `yaml:"enabled"`
	FailureThreshold int           `yaml:"failure_threshold"`
	Timeout          time.Duration `yaml:"timeout"`
}

// SecurityConfig represents security settings
type SecurityConfig struct {
	TLS        TLSConfig        `yaml:"tls"`
	Encryption EncryptionConfig `yaml:"encryption"`
}

// TLSConfig represents TLS settings
type TLSConfig struct {
	VerifyCertificates bool   `yaml:"verify_certificates"`
	MinVersion         string `yaml:"min_version"`
}

// EncryptionConfig represents encryption settings
type EncryptionConfig struct {
	InTransit bool `yaml:"in_transit"`
	AtRest    bool `yaml:"at_rest"`
}

// MonitoringConfig represents monitoring settings
type MonitoringConfig struct {
	Metrics      MetricsConfig      `yaml:"metrics"`
	HealthChecks HealthChecksConfig `yaml:"health_checks"`
	Logging      LoggingConfig      `yaml:"logging"`
}

// MetricsConfig represents metrics settings
type MetricsConfig struct {
	Enabled      bool              `yaml:"enabled"`
	Prometheus   bool              `yaml:"prometheus"`
	CustomLabels map[string]string `yaml:"custom_labels"`
}

// HealthChecksConfig represents health check settings
type HealthChecksConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

// LoggingConfig represents logging settings
type LoggingConfig struct {
	Structured bool          `yaml:"structured"`
	Format     string        `yaml:"format"`
	Sampling   SamplingConfig `yaml:"sampling"`
}

// SamplingConfig represents log sampling settings
type SamplingConfig struct {
	Enabled bool `yaml:"enabled"`
	Rate    int  `yaml:"rate"`
}

// FeatureConfig represents feature flags
type FeatureConfig struct {
	Prefetching            bool `yaml:"prefetching"`
	BatchOperations        bool `yaml:"batch_operations"`
	SmallFileOptimization  bool `yaml:"small_file_optimization"`
	MetadataCaching        bool `yaml:"metadata_caching"`
	OfflineMode           bool `yaml:"offline_mode"`
}