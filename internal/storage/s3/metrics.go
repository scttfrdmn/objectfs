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

	// Transfer Acceleration metrics
	AcceleratedRequests int64         `json:"accelerated_requests"`
	AcceleratedBytes    int64         `json:"accelerated_bytes"`
	FallbackEvents      int64         `json:"fallback_events"`
	AccelerationEnabled bool          `json:"acceleration_enabled"`
	AccelerationLatency time.Duration `json:"acceleration_latency"`

	// Multipart upload metrics
	MultipartUploads          int64         `json:"multipart_uploads"`           // Total multipart uploads initiated
	MultipartUploadsParts     int64         `json:"multipart_uploads_parts"`     // Total parts uploaded
	MultipartUploadsCompleted int64         `json:"multipart_uploads_completed"` // Completed multipart uploads
	MultipartUploadsFailed    int64         `json:"multipart_uploads_failed"`    // Failed multipart uploads
	MultipartBytes            int64         `json:"multipart_bytes"`             // Total bytes uploaded via multipart
	AveragePartSize           int64         `json:"average_part_size"`           // Average part size in bytes
	MultipartLatency          time.Duration `json:"multipart_latency"`           // Average multipart upload latency
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

// RecordAcceleratedRequest records a request that used Transfer Acceleration
func (mc *MetricsCollector) RecordAcceleratedRequest(bytes int64, duration time.Duration) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.AcceleratedRequests++
	mc.metrics.AcceleratedBytes += bytes

	// Calculate rolling average acceleration latency
	if mc.metrics.AcceleratedRequests == 1 {
		mc.metrics.AccelerationLatency = duration
	} else {
		mc.metrics.AccelerationLatency = time.Duration(
			(int64(mc.metrics.AccelerationLatency)*9 + int64(duration)) / 10,
		)
	}
}

// RecordFallbackEvent records when acceleration fallback occurs
func (mc *MetricsCollector) RecordFallbackEvent() {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.FallbackEvents++
}

// SetAccelerationEnabled sets whether acceleration is enabled
func (mc *MetricsCollector) SetAccelerationEnabled(enabled bool) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.AccelerationEnabled = enabled
}

// GetAccelerationRate calculates the percentage of requests using acceleration
func (mc *MetricsCollector) GetAccelerationRate() float64 {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	if mc.metrics.Requests == 0 {
		return 0
	}

	return float64(mc.metrics.AcceleratedRequests) / float64(mc.metrics.Requests) * 100
}

// GetFallbackRate calculates the fallback rate
func (mc *MetricsCollector) GetFallbackRate() float64 {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	if mc.metrics.AcceleratedRequests == 0 {
		return 0
	}

	return float64(mc.metrics.FallbackEvents) / float64(mc.metrics.AcceleratedRequests) * 100
}

// RecordMultipartUploadStart records when a multipart upload is initiated
func (mc *MetricsCollector) RecordMultipartUploadStart() {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.MultipartUploads++
}

// RecordMultipartUploadPart records when a part is uploaded
func (mc *MetricsCollector) RecordMultipartUploadPart(partSize int64) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.MultipartUploadsParts++
	mc.metrics.MultipartBytes += partSize

	// Calculate rolling average part size
	if mc.metrics.MultipartUploadsParts == 1 {
		mc.metrics.AveragePartSize = partSize
	} else {
		mc.metrics.AveragePartSize = (mc.metrics.AveragePartSize*9 + partSize) / 10
	}
}

// RecordMultipartUploadComplete records successful completion of a multipart upload
func (mc *MetricsCollector) RecordMultipartUploadComplete(totalBytes int64, duration time.Duration) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.MultipartUploadsCompleted++

	// Calculate rolling average multipart latency
	if mc.metrics.MultipartUploadsCompleted == 1 {
		mc.metrics.MultipartLatency = duration
	} else {
		mc.metrics.MultipartLatency = time.Duration(
			(int64(mc.metrics.MultipartLatency)*9 + int64(duration)) / 10,
		)
	}
}

// RecordMultipartUploadFailed records when a multipart upload fails
func (mc *MetricsCollector) RecordMultipartUploadFailed() {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.MultipartUploadsFailed++
}

// GetMultipartSuccessRate calculates the success rate of multipart uploads
func (mc *MetricsCollector) GetMultipartSuccessRate() float64 {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	totalAttempts := mc.metrics.MultipartUploadsCompleted + mc.metrics.MultipartUploadsFailed
	if totalAttempts == 0 {
		return 100.0 // No failures yet, assume 100%
	}

	return float64(mc.metrics.MultipartUploadsCompleted) / float64(totalAttempts) * 100
}

// GetMultipartUsageRate calculates the percentage of uploads using multipart
func (mc *MetricsCollector) GetMultipartUsageRate() float64 {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	if mc.metrics.Requests == 0 {
		return 0
	}

	return float64(mc.metrics.MultipartUploads) / float64(mc.metrics.Requests) * 100
}

// GetAveragePartsPerUpload calculates the average number of parts per multipart upload
func (mc *MetricsCollector) GetAveragePartsPerUpload() float64 {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	if mc.metrics.MultipartUploads == 0 {
		return 0
	}

	return float64(mc.metrics.MultipartUploadsParts) / float64(mc.metrics.MultipartUploads)
}
