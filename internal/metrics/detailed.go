package metrics

import (
	"sync"
	"time"
)

// OperationType represents different file system operations
type OperationType string

const (
	OpRead     OperationType = "read"
	OpWrite    OperationType = "write"
	OpDelete   OperationType = "delete"
	OpList     OperationType = "list"
	OpGetAttr  OperationType = "getattr"
	OpSetAttr  OperationType = "setattr"
	OpCreate   OperationType = "create"
	OpRename   OperationType = "rename"
	OpReadDir  OperationType = "readdir"
	OpMkDir    OperationType = "mkdir"
	OpRmDir    OperationType = "rmdir"
	OpOpen     OperationType = "open"
	OpRelease  OperationType = "release"
	OpTruncate OperationType = "truncate"
	OpChmod    OperationType = "chmod"
	OpChown    OperationType = "chown"
	OpLink     OperationType = "link"
	OpSymlink  OperationType = "symlink"
	OpStatFS   OperationType = "statfs"
	OpFlush    OperationType = "flush"
	OpFsync    OperationType = "fsync"
)

// CacheSourceType indicates where data was served from
type CacheSourceType string

const (
	CacheSourceL1        CacheSourceType = "l1"        // Memory cache
	CacheSourceL2        CacheSourceType = "l2"        // Persistent cache
	CacheSourceBackend   CacheSourceType = "backend"   // S3 backend
	CacheSourceReadAhead CacheSourceType = "readahead" // Prefetched
)

// DetailedOperationMetrics tracks metrics for a specific operation
type DetailedOperationMetrics struct {
	Count             int64         `json:"count"`
	TotalLatency      time.Duration `json:"total_latency"`
	MinLatency        time.Duration `json:"min_latency"`
	MaxLatency        time.Duration `json:"max_latency"`
	AverageLatency    time.Duration `json:"average_latency"`
	P50Latency        time.Duration `json:"p50_latency"`
	P95Latency        time.Duration `json:"p95_latency"`
	P99Latency        time.Duration `json:"p99_latency"`
	ErrorCount        int64         `json:"error_count"`
	BytesProcessed    int64         `json:"bytes_processed"`
	CacheHits         int64         `json:"cache_hits"`
	CacheMisses       int64         `json:"cache_misses"`
	CacheHitRate      float64       `json:"cache_hit_rate"`
	AvgBytesPerOp     float64       `json:"avg_bytes_per_op"`
	ThroughputMBps    float64       `json:"throughput_mbps"`
	LastOperationTime time.Time     `json:"last_operation_time"`
	LatencyHistogram  []int64       `json:"-"` // Histogram buckets for percentile calculation
}

// FileOperationMetrics tracks metrics for a specific file
type FileOperationMetrics struct {
	Path          string                                      `json:"path"`
	Operations    map[OperationType]*DetailedOperationMetrics `json:"operations"`
	TotalAccesses int64                                       `json:"total_accesses"`
	FirstAccess   time.Time                                   `json:"first_access"`
	LastAccess    time.Time                                   `json:"last_access"`
	BytesRead     int64                                       `json:"bytes_read"`
	BytesWritten  int64                                       `json:"bytes_written"`
	CacheHitRate  float64                                     `json:"cache_hit_rate"`
	AvgLatency    time.Duration                               `json:"avg_latency"`
	mu            sync.RWMutex                                `json:"-"`
}

// CacheBreakdownMetrics tracks cache performance by operation type
type CacheBreakdownMetrics struct {
	OperationType OperationType                     `json:"operation_type"`
	L1Hits        int64                             `json:"l1_hits"`
	L2Hits        int64                             `json:"l2_hits"`
	BackendFetch  int64                             `json:"backend_fetch"`
	ReadAheadHits int64                             `json:"readahead_hits"`
	TotalRequests int64                             `json:"total_requests"`
	L1HitRate     float64                           `json:"l1_hit_rate"`
	L2HitRate     float64                           `json:"l2_hit_rate"`
	TotalHitRate  float64                           `json:"total_hit_rate"`
	AvgLatency    map[CacheSourceType]time.Duration `json:"avg_latency"`
}

// NetworkUtilizationMetrics tracks network usage
type NetworkUtilizationMetrics struct {
	BytesUploaded      int64     `json:"bytes_uploaded"`
	BytesDownloaded    int64     `json:"bytes_downloaded"`
	UploadRate         float64   `json:"upload_rate_mbps"`
	DownloadRate       float64   `json:"download_rate_mbps"`
	TotalBandwidthUsed int64     `json:"total_bandwidth_used"`
	PeakUploadRate     float64   `json:"peak_upload_rate_mbps"`
	PeakDownloadRate   float64   `json:"peak_download_rate_mbps"`
	ActiveConnections  int64     `json:"active_connections"`
	RequestCount       int64     `json:"request_count"`
	AvgRequestSize     float64   `json:"avg_request_size"`
	AvgResponseSize    float64   `json:"avg_response_size"`
	NetworkErrors      int64     `json:"network_errors"`
	Retries            int64     `json:"retries"`
	TimeoutErrors      int64     `json:"timeout_errors"`
	LastUpdateTime     time.Time `json:"last_update_time"`
}

// CostMetrics tracks cost per operation
type CostMetrics struct {
	OperationType        OperationType `json:"operation_type"`
	RequestCount         int64         `json:"request_count"`
	StorageCost          float64       `json:"storage_cost_usd"`       // Storage cost ($/GB-month)
	RequestCost          float64       `json:"request_cost_usd"`       // API request cost
	DataTransferCost     float64       `json:"data_transfer_cost_usd"` // Data transfer cost
	TotalCost            float64       `json:"total_cost_usd"`         // Total cost for this operation
	BytesTransferred     int64         `json:"bytes_transferred"`
	CostPerRequest       float64       `json:"cost_per_request_usd"`
	CostPerGB            float64       `json:"cost_per_gb_usd"`
	EstimatedMonthlyCost float64       `json:"estimated_monthly_cost_usd"`
}

// DetailedPerformanceMetrics aggregates all detailed metrics
type DetailedPerformanceMetrics struct {
	mu                  sync.RWMutex
	OperationMetrics    map[OperationType]*DetailedOperationMetrics `json:"operation_metrics"`
	FileMetrics         map[string]*FileOperationMetrics            `json:"-"` // Not serialized by default (large)
	CacheBreakdown      map[OperationType]*CacheBreakdownMetrics    `json:"cache_breakdown"`
	NetworkUtilization  *NetworkUtilizationMetrics                  `json:"network_utilization"`
	CostMetrics         map[OperationType]*CostMetrics              `json:"cost_metrics"`
	StartTime           time.Time                                   `json:"start_time"`
	LastUpdateTime      time.Time                                   `json:"last_update_time"`
	TotalOperations     int64                                       `json:"total_operations"`
	TotalErrors         int64                                       `json:"total_errors"`
	TotalBytesProcessed int64                                       `json:"total_bytes_processed"`
	OverallCacheHitRate float64                                     `json:"overall_cache_hit_rate"`
	OverallErrorRate    float64                                     `json:"overall_error_rate"`
	TopFilesEnabled     bool                                        `json:"top_files_enabled"`
	MaxTrackedFiles     int                                         `json:"max_tracked_files"`
}

// NewDetailedPerformanceMetrics creates a new detailed performance metrics collector
func NewDetailedPerformanceMetrics(maxTrackedFiles int, trackFiles bool) *DetailedPerformanceMetrics {
	return &DetailedPerformanceMetrics{
		OperationMetrics:   make(map[OperationType]*DetailedOperationMetrics),
		FileMetrics:        make(map[string]*FileOperationMetrics),
		CacheBreakdown:     make(map[OperationType]*CacheBreakdownMetrics),
		CostMetrics:        make(map[OperationType]*CostMetrics),
		NetworkUtilization: &NetworkUtilizationMetrics{},
		StartTime:          time.Now(),
		LastUpdateTime:     time.Now(),
		TopFilesEnabled:    trackFiles,
		MaxTrackedFiles:    maxTrackedFiles,
	}
}

// RecordOperation records metrics for a file operation
func (dpm *DetailedPerformanceMetrics) RecordOperation(
	opType OperationType,
	path string,
	latency time.Duration,
	bytes int64,
	cacheSource CacheSourceType,
	err error,
) {
	dpm.mu.Lock()
	defer dpm.mu.Unlock()

	now := time.Now()
	dpm.LastUpdateTime = now
	dpm.TotalOperations++
	dpm.TotalBytesProcessed += bytes

	// Update operation metrics
	if dpm.OperationMetrics[opType] == nil {
		dpm.OperationMetrics[opType] = &DetailedOperationMetrics{
			MinLatency:       latency,
			LatencyHistogram: make([]int64, 100), // 100 buckets for percentile calculation
		}
	}

	om := dpm.OperationMetrics[opType]
	om.Count++
	om.TotalLatency += latency
	om.LastOperationTime = now
	om.BytesProcessed += bytes

	// Update min/max latency
	if latency < om.MinLatency || om.MinLatency == 0 {
		om.MinLatency = latency
	}
	if latency > om.MaxLatency {
		om.MaxLatency = latency
	}

	// Update average latency
	om.AverageLatency = time.Duration(int64(om.TotalLatency) / om.Count)

	// Update histogram for percentile calculation
	bucketIndex := int(latency.Milliseconds()) % len(om.LatencyHistogram)
	om.LatencyHistogram[bucketIndex]++

	// Update cache metrics
	if cacheSource == CacheSourceL1 || cacheSource == CacheSourceL2 || cacheSource == CacheSourceReadAhead {
		om.CacheHits++
	} else {
		om.CacheMisses++
	}
	total := om.CacheHits + om.CacheMisses
	if total > 0 {
		om.CacheHitRate = float64(om.CacheHits) / float64(total)
	}

	// Update error count
	if err != nil {
		om.ErrorCount++
		dpm.TotalErrors++
	}

	// Update bytes per operation
	if om.Count > 0 {
		om.AvgBytesPerOp = float64(om.BytesProcessed) / float64(om.Count)
	}

	// Update throughput (MB/s)
	if om.TotalLatency > 0 {
		seconds := om.TotalLatency.Seconds()
		om.ThroughputMBps = (float64(om.BytesProcessed) / (1024 * 1024)) / seconds
	}

	// Update cache breakdown
	dpm.updateCacheBreakdown(opType, cacheSource, latency)

	// Update file metrics if enabled
	if dpm.TopFilesEnabled && path != "" {
		dpm.updateFileMetrics(path, opType, latency, bytes, cacheSource, err)
	}

	// Update overall metrics
	dpm.updateOverallMetrics()
}

// RecordNetworkOperation records network-specific metrics
func (dpm *DetailedPerformanceMetrics) RecordNetworkOperation(
	bytesUploaded, bytesDownloaded int64,
	duration time.Duration,
	err error,
) {
	dpm.mu.Lock()
	defer dpm.mu.Unlock()

	nu := dpm.NetworkUtilization
	nu.BytesUploaded += bytesUploaded
	nu.BytesDownloaded += bytesDownloaded
	nu.TotalBandwidthUsed = nu.BytesUploaded + nu.BytesDownloaded
	nu.RequestCount++
	nu.LastUpdateTime = time.Now()

	// Calculate current rates (MB/s)
	if duration > 0 {
		seconds := duration.Seconds()
		uploadRate := (float64(bytesUploaded) / (1024 * 1024)) / seconds
		downloadRate := (float64(bytesDownloaded) / (1024 * 1024)) / seconds

		// Update peak rates
		if uploadRate > nu.PeakUploadRate {
			nu.PeakUploadRate = uploadRate
		}
		if downloadRate > nu.PeakDownloadRate {
			nu.PeakDownloadRate = downloadRate
		}

		// Update current rates (rolling average)
		if nu.RequestCount == 1 {
			nu.UploadRate = uploadRate
			nu.DownloadRate = downloadRate
		} else {
			// 90/10 weighted average for smooth rate calculation
			nu.UploadRate = (nu.UploadRate * 0.9) + (uploadRate * 0.1)
			nu.DownloadRate = (nu.DownloadRate * 0.9) + (downloadRate * 0.1)
		}
	}

	// Update averages
	if nu.RequestCount > 0 {
		nu.AvgRequestSize = float64(nu.BytesUploaded) / float64(nu.RequestCount)
		nu.AvgResponseSize = float64(nu.BytesDownloaded) / float64(nu.RequestCount)
	}

	// Update error counts
	if err != nil {
		nu.NetworkErrors++
	}
}

// RecordCost records cost metrics for an operation
func (dpm *DetailedPerformanceMetrics) RecordCost(
	opType OperationType,
	requestCost, storageCost, transferCost float64,
	bytesTransferred int64,
) {
	dpm.mu.Lock()
	defer dpm.mu.Unlock()

	if dpm.CostMetrics[opType] == nil {
		dpm.CostMetrics[opType] = &CostMetrics{
			OperationType: opType,
		}
	}

	cm := dpm.CostMetrics[opType]
	cm.RequestCount++
	cm.RequestCost += requestCost
	cm.StorageCost += storageCost
	cm.DataTransferCost += transferCost
	cm.TotalCost = cm.RequestCost + cm.StorageCost + cm.DataTransferCost
	cm.BytesTransferred += bytesTransferred

	// Calculate per-request and per-GB costs
	if cm.RequestCount > 0 {
		cm.CostPerRequest = cm.TotalCost / float64(cm.RequestCount)
	}
	if cm.BytesTransferred > 0 {
		gb := float64(cm.BytesTransferred) / (1024 * 1024 * 1024)
		cm.CostPerGB = cm.TotalCost / gb
	}

	// Estimate monthly cost based on current rate
	uptime := time.Since(dpm.StartTime)
	if uptime > 0 {
		hoursRunning := uptime.Hours()
		hoursInMonth := float64(24 * 30)
		cm.EstimatedMonthlyCost = (cm.TotalCost / hoursRunning) * hoursInMonth
	}
}

// GetOperationMetrics returns metrics for a specific operation type
func (dpm *DetailedPerformanceMetrics) GetOperationMetrics(opType OperationType) *DetailedOperationMetrics {
	dpm.mu.RLock()
	defer dpm.mu.RUnlock()

	if om, exists := dpm.OperationMetrics[opType]; exists {
		// Return a copy to avoid race conditions
		omCopy := *om
		return &omCopy
	}
	return nil
}

// GetTopFiles returns the N most accessed files
func (dpm *DetailedPerformanceMetrics) GetTopFiles(n int) []*FileOperationMetrics {
	dpm.mu.RLock()
	defer dpm.mu.RUnlock()

	if !dpm.TopFilesEnabled {
		return nil
	}

	// Convert map to slice for sorting
	files := make([]*FileOperationMetrics, 0, len(dpm.FileMetrics))
	for _, fm := range dpm.FileMetrics {
		// Create a copy
		fmCopy := &FileOperationMetrics{
			Path:          fm.Path,
			TotalAccesses: fm.TotalAccesses,
			FirstAccess:   fm.FirstAccess,
			LastAccess:    fm.LastAccess,
			BytesRead:     fm.BytesRead,
			BytesWritten:  fm.BytesWritten,
			CacheHitRate:  fm.CacheHitRate,
			AvgLatency:    fm.AvgLatency,
		}
		files = append(files, fmCopy)
	}

	// Sort by total accesses (descending)
	for i := 0; i < len(files)-1; i++ {
		for j := i + 1; j < len(files); j++ {
			if files[j].TotalAccesses > files[i].TotalAccesses {
				files[i], files[j] = files[j], files[i]
			}
		}
	}

	// Return top N
	if n > len(files) {
		n = len(files)
	}
	return files[:n]
}

// GetSummary returns a summary of all metrics
func (dpm *DetailedPerformanceMetrics) GetSummary() map[string]interface{} {
	dpm.mu.RLock()
	defer dpm.mu.RUnlock()

	uptime := time.Since(dpm.StartTime)

	summary := map[string]interface{}{
		"uptime_seconds":         uptime.Seconds(),
		"total_operations":       dpm.TotalOperations,
		"total_errors":           dpm.TotalErrors,
		"total_bytes_processed":  dpm.TotalBytesProcessed,
		"overall_cache_hit_rate": dpm.OverallCacheHitRate,
		"overall_error_rate":     dpm.OverallErrorRate,
		"operations_per_second":  float64(dpm.TotalOperations) / uptime.Seconds(),
		"throughput_mbps":        (float64(dpm.TotalBytesProcessed) / (1024 * 1024)) / uptime.Seconds(),
		"tracked_files_count":    len(dpm.FileMetrics),
		"last_update":            dpm.LastUpdateTime.Format(time.RFC3339),
	}

	return summary
}

// Reset resets all metrics
func (dpm *DetailedPerformanceMetrics) Reset() {
	dpm.mu.Lock()
	defer dpm.mu.Unlock()

	dpm.OperationMetrics = make(map[OperationType]*DetailedOperationMetrics)
	dpm.FileMetrics = make(map[string]*FileOperationMetrics)
	dpm.CacheBreakdown = make(map[OperationType]*CacheBreakdownMetrics)
	dpm.CostMetrics = make(map[OperationType]*CostMetrics)
	dpm.NetworkUtilization = &NetworkUtilizationMetrics{}
	dpm.StartTime = time.Now()
	dpm.LastUpdateTime = time.Now()
	dpm.TotalOperations = 0
	dpm.TotalErrors = 0
	dpm.TotalBytesProcessed = 0
	dpm.OverallCacheHitRate = 0
	dpm.OverallErrorRate = 0
}

// Helper methods

func (dpm *DetailedPerformanceMetrics) updateCacheBreakdown(
	opType OperationType,
	source CacheSourceType,
	latency time.Duration,
) {
	if dpm.CacheBreakdown[opType] == nil {
		dpm.CacheBreakdown[opType] = &CacheBreakdownMetrics{
			OperationType: opType,
			AvgLatency:    make(map[CacheSourceType]time.Duration),
		}
	}

	cb := dpm.CacheBreakdown[opType]
	cb.TotalRequests++

	switch source {
	case CacheSourceL1:
		cb.L1Hits++
	case CacheSourceL2:
		cb.L2Hits++
	case CacheSourceBackend:
		cb.BackendFetch++
	case CacheSourceReadAhead:
		cb.ReadAheadHits++
	}

	// Update hit rates
	if cb.TotalRequests > 0 {
		cb.L1HitRate = float64(cb.L1Hits) / float64(cb.TotalRequests)
		cb.L2HitRate = float64(cb.L2Hits) / float64(cb.TotalRequests)
		totalCacheHits := cb.L1Hits + cb.L2Hits + cb.ReadAheadHits
		cb.TotalHitRate = float64(totalCacheHits) / float64(cb.TotalRequests)
	}

	// Update average latency by source (rolling average)
	if cb.AvgLatency[source] == 0 {
		cb.AvgLatency[source] = latency
	} else {
		cb.AvgLatency[source] = time.Duration(
			(int64(cb.AvgLatency[source])*9 + int64(latency)) / 10,
		)
	}
}

func (dpm *DetailedPerformanceMetrics) updateFileMetrics(
	path string,
	opType OperationType,
	latency time.Duration,
	bytes int64,
	cacheSource CacheSourceType,
	err error,
) {
	// Limit number of tracked files
	if len(dpm.FileMetrics) >= dpm.MaxTrackedFiles && dpm.FileMetrics[path] == nil {
		// Don't track new files if we're at the limit
		return
	}

	if dpm.FileMetrics[path] == nil {
		dpm.FileMetrics[path] = &FileOperationMetrics{
			Path:        path,
			Operations:  make(map[OperationType]*DetailedOperationMetrics),
			FirstAccess: time.Now(),
		}
	}

	fm := dpm.FileMetrics[path]
	fm.mu.Lock()
	defer fm.mu.Unlock()

	fm.TotalAccesses++
	fm.LastAccess = time.Now()

	// Track bytes by operation type
	if opType == OpRead {
		fm.BytesRead += bytes
	} else if opType == OpWrite {
		fm.BytesWritten += bytes
	}

	// Update operation-specific metrics for this file
	if fm.Operations[opType] == nil {
		fm.Operations[opType] = &DetailedOperationMetrics{
			MinLatency: latency,
		}
	}

	opMetrics := fm.Operations[opType]
	opMetrics.Count++
	opMetrics.TotalLatency += latency
	opMetrics.BytesProcessed += bytes

	if latency < opMetrics.MinLatency || opMetrics.MinLatency == 0 {
		opMetrics.MinLatency = latency
	}
	if latency > opMetrics.MaxLatency {
		opMetrics.MaxLatency = latency
	}

	opMetrics.AverageLatency = time.Duration(int64(opMetrics.TotalLatency) / opMetrics.Count)

	if cacheSource != CacheSourceBackend {
		opMetrics.CacheHits++
	} else {
		opMetrics.CacheMisses++
	}

	if err != nil {
		opMetrics.ErrorCount++
	}

	// Update file-level cache hit rate
	totalOps := int64(0)
	totalHits := int64(0)
	totalLatency := time.Duration(0)
	for _, om := range fm.Operations {
		totalOps += om.Count
		totalHits += om.CacheHits
		totalLatency += om.TotalLatency
	}

	if totalOps > 0 {
		fm.CacheHitRate = float64(totalHits) / float64(totalOps)
		fm.AvgLatency = time.Duration(int64(totalLatency) / totalOps)
	}
}

func (dpm *DetailedPerformanceMetrics) updateOverallMetrics() {
	totalCacheHits := int64(0)
	totalCacheMisses := int64(0)

	for _, om := range dpm.OperationMetrics {
		totalCacheHits += om.CacheHits
		totalCacheMisses += om.CacheMisses
	}

	total := totalCacheHits + totalCacheMisses
	if total > 0 {
		dpm.OverallCacheHitRate = float64(totalCacheHits) / float64(total)
	}

	if dpm.TotalOperations > 0 {
		dpm.OverallErrorRate = float64(dpm.TotalErrors) / float64(dpm.TotalOperations)
	}
}
