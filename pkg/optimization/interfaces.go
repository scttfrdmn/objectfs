// Package optimization defines the interfaces for S3 performance optimization
// These interfaces specify the contract for CargoShip integration
package optimization

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Optimizer provides the main interface for S3 performance optimization
// This is the primary interface that ObjectFS will use for CargoShip integration
type S3Optimizer interface {
	// Core S3 operations with optimization
	GetObjectOptimized(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error)
	PutObjectOptimized(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error)
	DeleteObjectOptimized(ctx context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error)
	HeadObjectOptimized(ctx context.Context, input *s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	ListObjectsV2Optimized(ctx context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)

	// Batch operations for improved efficiency
	GetObjectsBatch(ctx context.Context, requests []*s3.GetObjectInput) ([]*BatchResult, error)
	PutObjectsBatch(ctx context.Context, requests []*s3.PutObjectInput) ([]*BatchResult, error)

	// Configuration and monitoring
	UpdateNetworkConditions(conditions *NetworkConditions) error
	GetPerformanceMetrics() *PerformanceMetrics
	GetOptimizationStats() *OptimizationStats
	
	// Lifecycle management
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	HealthCheck(ctx context.Context) error
}

// NetworkOptimizer provides network-level optimization capabilities
type NetworkOptimizer interface {
	// BBR congestion control
	GetBandwidthEstimate() float64
	GetRTTEstimate() time.Duration
	GetCongestionWindow() int64
	
	// Network adaptation
	AdaptToConditions(conditions *NetworkConditions) error
	GetOptimalBufferSize(dataSize int64) int64
	GetRecommendedConcurrency() int
}

// AdaptiveEngine provides intelligent adaptation capabilities
type AdaptiveEngine interface {
	// Real-time optimization
	OptimizeParameters(ctx context.Context, operation string, dataSize int64) (*OptimizationParams, error)
	PredictOptimalStrategy(ctx context.Context, workload *WorkloadProfile) (*TransferStrategy, error)
	
	// Learning and adaptation
	RecordPerformance(operation string, params *OptimizationParams, metrics *OperationMetrics)
	UpdatePredictionModel(workload *WorkloadProfile, actualPerformance *PerformanceMetrics)
}

// ConnectionManager handles connection pooling and health
type ConnectionManager interface {
	// Connection lifecycle
	GetHealthyConnection(ctx context.Context) (*s3.Client, error)
	ReturnConnection(client *s3.Client, healthy bool)
	
	// Pool management
	GetPoolStats() *PoolStats
	ScalePool(targetSize int) error
	HealthCheckAll(ctx context.Context) error
}

// PerformanceMonitor provides metrics and monitoring
type PerformanceMonitor interface {
	// Metrics collection
	RecordOperation(operation string, duration time.Duration, bytes int64, success bool)
	RecordNetworkMetrics(latency time.Duration, bandwidth float64, lossRate float64)
	RecordCacheMetrics(hits, misses int64, hitRatio float64)
	
	// Metrics retrieval
	GetMetrics() *PerformanceMetrics
	GetHistoricalMetrics(window time.Duration) []*PerformanceSnapshot
	ExportMetrics(format string) ([]byte, error)
}

// Data structures for optimization

// NetworkConditions represents current network state
type NetworkConditions struct {
	Bandwidth       float64       `json:"bandwidth"`        // Mbps
	Latency         time.Duration `json:"latency"`          // Round-trip time
	PacketLoss      float64       `json:"packet_loss"`      // Loss rate (0.0-1.0)
	Jitter          time.Duration `json:"jitter"`           // Latency variation
	ConnectionType  string        `json:"connection_type"`  // "wifi", "ethernet", "cellular", etc.
	QualityScore    float64       `json:"quality_score"`    // Overall quality (0.0-1.0)
	Timestamp       time.Time     `json:"timestamp"`
}

// PerformanceMetrics contains performance statistics
type PerformanceMetrics struct {
	// Operation statistics
	TotalOperations   int64         `json:"total_operations"`
	SuccessfulOps     int64         `json:"successful_ops"`
	FailedOps         int64         `json:"failed_ops"`
	AverageLatency    time.Duration `json:"average_latency"`
	P95Latency        time.Duration `json:"p95_latency"`
	P99Latency        time.Duration `json:"p99_latency"`
	
	// Throughput statistics
	TotalBytesRead    int64   `json:"total_bytes_read"`
	TotalBytesWritten int64   `json:"total_bytes_written"`
	ReadThroughput    float64 `json:"read_throughput"`    // MB/s
	WriteThroughput   float64 `json:"write_throughput"`   // MB/s
	
	// Network statistics
	NetworkLatency    time.Duration `json:"network_latency"`
	NetworkBandwidth  float64       `json:"network_bandwidth"`
	PacketLossRate    float64       `json:"packet_loss_rate"`
	
	// Cache statistics
	CacheHits         int64   `json:"cache_hits"`
	CacheMisses       int64   `json:"cache_misses"`
	CacheHitRatio     float64 `json:"cache_hit_ratio"`
	
	// Resource utilization
	CPUUsage          float64 `json:"cpu_usage"`
	MemoryUsage       int64   `json:"memory_usage"`
	DiskIOPS          float64 `json:"disk_iops"`
	
	Timestamp         time.Time `json:"timestamp"`
}

// OptimizationStats provides optimization-specific statistics
type OptimizationStats struct {
	// BBR statistics
	BBRBandwidth      float64 `json:"bbr_bandwidth"`
	BBRMinRTT         time.Duration `json:"bbr_min_rtt"`
	BBRCongestionWindow int64 `json:"bbr_congestion_window"`
	
	// CUBIC statistics
	CubicWindow       int64   `json:"cubic_window"`
	CubicSlowStart    bool    `json:"cubic_slow_start"`
	CubicBeta         float64 `json:"cubic_beta"`
	
	// Adaptation statistics
	ParameterUpdates  int64   `json:"parameter_updates"`
	PredictionAccuracy float64 `json:"prediction_accuracy"`
	
	// Connection pool statistics
	ActiveConnections int     `json:"active_connections"`
	IdleConnections   int     `json:"idle_connections"`
	ConnectionResets  int64   `json:"connection_resets"`
	
	Timestamp         time.Time `json:"timestamp"`
}

// OptimizationParams contains parameters for a specific operation
type OptimizationParams struct {
	// Transfer parameters
	ChunkSize         int64   `json:"chunk_size"`
	Concurrency       int     `json:"concurrency"`
	BufferSize        int64   `json:"buffer_size"`
	
	// Retry parameters
	MaxRetries        int           `json:"max_retries"`
	InitialBackoff    time.Duration `json:"initial_backoff"`
	MaxBackoff        time.Duration `json:"max_backoff"`
	BackoffMultiplier float64       `json:"backoff_multiplier"`
	
	// Compression parameters
	CompressionEnabled bool   `json:"compression_enabled"`
	CompressionAlgorithm string `json:"compression_algorithm"`
	CompressionLevel   int    `json:"compression_level"`
	
	// Caching parameters
	CacheEnabled      bool          `json:"cache_enabled"`
	CacheTTL          time.Duration `json:"cache_ttl"`
	
	Strategy          string        `json:"strategy"` // "aggressive", "conservative", "adaptive"
}

// TransferStrategy defines an optimized transfer approach
type TransferStrategy struct {
	Name              string                 `json:"name"`
	Description       string                 `json:"description"`
	Parameters        *OptimizationParams    `json:"parameters"`
	ExpectedThroughput float64               `json:"expected_throughput"`
	ExpectedLatency   time.Duration          `json:"expected_latency"`
	ConfidenceScore   float64                `json:"confidence_score"`
	Conditions        *NetworkConditions     `json:"conditions"`
}

// WorkloadProfile describes the characteristics of a workload
type WorkloadProfile struct {
	// File characteristics
	AverageFileSize   int64   `json:"average_file_size"`
	FileSizeVariance  float64 `json:"file_size_variance"`
	FileCount         int64   `json:"file_count"`
	
	// Access patterns
	ReadWriteRatio    float64 `json:"read_write_ratio"`
	RandomSequential  float64 `json:"random_sequential"`
	AccessFrequency   float64 `json:"access_frequency"`
	
	// Timing characteristics
	Burstiness        float64       `json:"burstiness"`
	Duration          time.Duration `json:"duration"`
	PeakThroughput    float64       `json:"peak_throughput"`
	
	// Geographic distribution
	ClientLocations   []string      `json:"client_locations"`
	S3Region          string        `json:"s3_region"`
	
	Timestamp         time.Time     `json:"timestamp"`
}

// OperationMetrics contains metrics for a single operation
type OperationMetrics struct {
	Operation         string        `json:"operation"`
	StartTime         time.Time     `json:"start_time"`
	EndTime           time.Time     `json:"end_time"`
	Duration          time.Duration `json:"duration"`
	BytesTransferred  int64         `json:"bytes_transferred"`
	Success           bool          `json:"success"`
	ErrorType         string        `json:"error_type,omitempty"`
	Throughput        float64       `json:"throughput"`
	Retries           int           `json:"retries"`
}

// BatchResult contains the result of a batch operation
type BatchResult struct {
	Index             int           `json:"index"`
	Success           bool          `json:"success"`
	Result            interface{}   `json:"result,omitempty"`
	Error             error         `json:"error,omitempty"`
	Duration          time.Duration `json:"duration"`
	BytesTransferred  int64         `json:"bytes_transferred"`
}

// PoolStats provides connection pool statistics
type PoolStats struct {
	TotalConnections  int           `json:"total_connections"`
	ActiveConnections int           `json:"active_connections"`
	IdleConnections   int           `json:"idle_connections"`
	FailedConnections int64         `json:"failed_connections"`
	AverageWaitTime   time.Duration `json:"average_wait_time"`
	MaxWaitTime       time.Duration `json:"max_wait_time"`
	CreatedConnections int64        `json:"created_connections"`
	DestroyedConnections int64      `json:"destroyed_connections"`
}

// PerformanceSnapshot represents metrics at a specific point in time
type PerformanceSnapshot struct {
	Timestamp         time.Time          `json:"timestamp"`
	Metrics           *PerformanceMetrics `json:"metrics"`
	NetworkConditions *NetworkConditions  `json:"network_conditions"`
	OptimizationStats *OptimizationStats  `json:"optimization_stats"`
}

// ObjectFSConfig defines ObjectFS-specific optimization configuration
type ObjectFSConfig struct {
	// Performance targets
	TargetReadThroughput  float64 `json:"target_read_throughput"`  // 400-800 MB/s
	TargetWriteThroughput float64 `json:"target_write_throughput"` // 300-600 MB/s
	TargetLatency         time.Duration `json:"target_latency"`     // <10ms for cached
	
	// Cache configuration
	CacheSize             int64 `json:"cache_size"`              // L1 + L2 total
	L1CacheSize           int64 `json:"l1_cache_size"`           // Memory cache
	L2CacheSize           int64 `json:"l2_cache_size"`           // Disk cache
	
	// Concurrency limits
	MaxConcurrentReads    int   `json:"max_concurrent_reads"`
	MaxConcurrentWrites   int   `json:"max_concurrent_writes"`
	
	// FUSE-specific optimizations
	ReadAheadSize         int64 `json:"read_ahead_size"`
	WriteBufferSize       int64 `json:"write_buffer_size"`
	DirectIO              bool  `json:"direct_io"`
	
	// Integration settings
	CargoShipOptimization bool  `json:"cargoship_optimization"`
	SharedMetrics         bool  `json:"shared_metrics"`
}