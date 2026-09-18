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

	// Seekable-framing read metrics (#185).
	//
	// SeekableWholeFallbacks is the field that matters operationally. Every fallback is *correct* — the
	// object still reads, byte for byte — so the feature's failure mode is not an error anyone sees but
	// a silent return to whole-object transfer. Without a count, "reads got slow" has no evidence
	// attached to it. SeekableLastFallbackReason names the most recent cause, in LastError's shape and
	// for the same reason: an operator needs to know *which* of the several benign-looking causes is
	// happening before they can act on it.
	SeekableReads              int64  `json:"seekable_reads"`                // Ranged reads served from frames
	SeekableReadBytes          int64  `json:"seekable_read_bytes"`           // Stored bytes those reads transferred
	SeekableWholeFallbacks     int64  `json:"seekable_whole_fallbacks"`      // Framed objects read whole anyway
	SeekableLastFallbackReason string `json:"seekable_last_fallback_reason"` // Why the last fallback happened

	// Fan-out probe metrics (#514).
	//
	// FanOutProbeDeclines is the count that matters, and it is the only evidence there is that the probe
	// works. A decline is invisible from every other angle: the read returns the same bytes at the same
	// speed either way, and the bytes it saves are bytes an abandoned response body would have carried —
	// which the server may well have written into a socket buffer before noticing the client was gone, so
	// they are not reliably countable even on the far side of the wire.
	//
	// FanOutProbes is the denominator, and the ratio is the cost side of the trade #514 made: every
	// fan-out now waits one round trip for chunk 0's headers, and only the declines got anything for it.
	// A deployment where declines are near zero is paying that round trip for nothing.
	//
	// Probes, not reads: the retryer re-issues a GET, and each attempt consults the probe again.
	FanOutProbes        int64 `json:"fanout_probes"`         // Chunk-0 GETs whose headers were put to a probe
	FanOutProbeDeclines int64 `json:"fanout_probe_declines"` // ... of those, the ones that declined the body

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

// SetAccelerationEnabled records whether acceleration is currently in effect.
//
// "Currently in effect", not "configured". Until #204 this was called once, from NewBackend, with
// cfg.UseAccelerate — so the field reported the operator's request and never the outcome: a mount could
// say acceleration was enabled, have fallen back on its first request, and have used the standard
// endpoint ever since. The gate's OnStateChange now calls this on every transition, which is what makes
// AccelerationEnabled a fact about the mount.
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

// RecordSeekableRead records a ranged read served from an object's frames, and the stored bytes it
// transferred — index frame plus data frames, i.e. what was actually pulled over the wire.
//
// Bytes, not latency. Latency on a mount is dominated by the kernel, the page cache and whatever else
// the machine is doing, so a latency figure moves for reasons that have nothing to do with framing;
// the bytes a read transfers is the quantity this feature changes and the one it should be judged on.
func (mc *MetricsCollector) RecordSeekableRead(storedBytes int64) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.SeekableReads++
	mc.metrics.SeekableReadBytes += storedBytes
}

// RecordSeekableWholeFallback records a framed object read whole anyway, with the reason.
//
// reason is a short fixed string, not a formatted message: it is a category to aggregate on, and one
// that interpolated a key or an offset would be useless in a counter and would leak object names into
// whatever scrapes this.
func (mc *MetricsCollector) RecordSeekableWholeFallback(reason string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.SeekableWholeFallbacks++
	mc.metrics.SeekableLastFallbackReason = reason
}

// RecordFanOutProbe records a chunk-0 GET whose response headers were put to a probe, and whether the
// probe declined the body on the strength of them.
//
// Both outcomes, from one call, because the interesting quantity is the ratio and a counter that only
// moved on declines would make "the probe never fires" indistinguishable from "no read ever fanned out".
func (mc *MetricsCollector) RecordFanOutProbe(declined bool) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.metrics.FanOutProbes++
	if declined {
		mc.metrics.FanOutProbeDeclines++
	}
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
