package s3

import (
	"sync"
	"time"
)

// BackendMetrics tracks S3 backend performance metrics
type BackendMetrics struct {
	Requests        int64         `json:"requests"`
	Errors          int64         `json:"errors"`
	BytesUploaded   int64         `json:"bytes_uploaded"`
	BytesDownloaded int64         `json:"bytes_downloaded"`
	AverageLatency  time.Duration `json:"average_latency"`
	LastError       string        `json:"last_error"`
	LastErrorTime   time.Time     `json:"last_error_time"`
}

// MetricsCollector handles metrics collection and aggregation for S3 backend
type MetricsCollector struct {
	mu      sync.RWMutex
	metrics BackendMetrics
}

// NewMetricsCollector creates a new metrics collector
func NewMetricsCollector() *MetricsCollector {
	return &MetricsCollector{
		metrics: BackendMetrics{},
	}
}

// RecordMetrics records operation metrics with duration and error status
func (mc *MetricsCollector) RecordMetrics(duration time.Duration, isError bool) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.Requests++
	if isError {
		mc.metrics.Errors++
	}

	// Calculate rolling average latency
	if mc.metrics.Requests == 1 {
		mc.metrics.AverageLatency = duration
	} else {
		mc.metrics.AverageLatency = time.Duration(
			(int64(mc.metrics.AverageLatency)*9 + int64(duration)) / 10,
		)
	}
}

// RecordError records an error occurrence
func (mc *MetricsCollector) RecordError(err error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.LastError = err.Error()
	mc.metrics.LastErrorTime = time.Now()
}

// RecordBytesUploaded records uploaded bytes
func (mc *MetricsCollector) RecordBytesUploaded(bytes int64) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.BytesUploaded += bytes
}

// RecordBytesDownloaded records downloaded bytes
func (mc *MetricsCollector) RecordBytesDownloaded(bytes int64) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.BytesDownloaded += bytes
}

// GetMetrics returns current backend metrics
func (mc *MetricsCollector) GetMetrics() BackendMetrics {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return mc.metrics
}

// Reset resets all metrics to zero
func (mc *MetricsCollector) Reset() {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics = BackendMetrics{}
}

// GetThroughput calculates upload and download throughput
func (mc *MetricsCollector) GetThroughput() (uploadMBps, downloadMBps float64) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	if mc.metrics.Requests == 0 {
		return 0, 0
	}

	// Estimate total time based on average latency and request count
	totalTime := time.Duration(mc.metrics.Requests) * mc.metrics.AverageLatency
	if totalTime == 0 {
		return 0, 0
	}

	totalTimeSeconds := totalTime.Seconds()
	uploadMBps = float64(mc.metrics.BytesUploaded) / (1024 * 1024) / totalTimeSeconds
	downloadMBps = float64(mc.metrics.BytesDownloaded) / (1024 * 1024) / totalTimeSeconds

	return uploadMBps, downloadMBps
}

// GetErrorRate calculates the current error rate
func (mc *MetricsCollector) GetErrorRate() float64 {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	if mc.metrics.Requests == 0 {
		return 0
	}

	return float64(mc.metrics.Errors) / float64(mc.metrics.Requests)
}
