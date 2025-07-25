package types

import (
	"context"
	"time"
)

// Backend defines the interface for object storage backends
type Backend interface {
	// Object operations
	GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error)
	PutObject(ctx context.Context, key string, data []byte) error
	DeleteObject(ctx context.Context, key string) error
	HeadObject(ctx context.Context, key string) (*ObjectInfo, error)
	
	// Batch operations
	GetObjects(ctx context.Context, keys []string) (map[string][]byte, error)
	PutObjects(ctx context.Context, objects map[string][]byte) error
	
	// List operations
	ListObjects(ctx context.Context, prefix string, limit int) ([]ObjectInfo, error)
	
	// Health check
	HealthCheck(ctx context.Context) error
}

// Cache defines the caching interface
type Cache interface {
	Get(key string, offset, size int64) []byte
	Put(key string, offset int64, data []byte)
	Delete(key string)
	Evict(size int64) bool
	Size() int64
	Stats() CacheStats
}

// WriteBuffer defines the write buffering interface
type WriteBuffer interface {
	Write(key string, offset int64, data []byte) error
	Flush(key string) error
	FlushAll() error
	Size() int64
	Count() int
}

// MetricsCollector defines the metrics collection interface
type MetricsCollector interface {
	RecordOperation(operation string, duration time.Duration, size int64, success bool)
	RecordCacheHit(key string, size int64)
	RecordCacheMiss(key string, size int64)
	RecordError(operation string, err error)
	GetMetrics() map[string]interface{}
}

// ConfigManager defines configuration management interface
type ConfigManager interface {
	Get(key string) interface{}
	GetString(key string) string
	GetInt(key string) int
	GetDuration(key string) time.Duration
	GetBool(key string) bool
	Watch(key string, callback func(interface{}))
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
	GetConnection() interface{}
	ReturnConnection(conn interface{})
	HealthCheck() error
	ScalePool(targetSize int) error
	GetStats() ConnectionStats
}