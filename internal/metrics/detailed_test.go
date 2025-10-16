package metrics

import (
	"errors"
	"testing"
	"time"
)

func TestNewDetailedPerformanceMetrics(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(1000, true)

	if dpm == nil {
		t.Fatal("Expected non-nil DetailedPerformanceMetrics")
	}

	if dpm.MaxTrackedFiles != 1000 {
		t.Errorf("Expected MaxTrackedFiles=1000, got %d", dpm.MaxTrackedFiles)
	}

	if !dpm.TopFilesEnabled {
		t.Error("Expected TopFilesEnabled=true")
	}

	if dpm.OperationMetrics == nil {
		t.Error("Expected initialized OperationMetrics map")
	}
}

func TestRecordOperation_BasicMetrics(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, false)

	// Record a read operation
	dpm.RecordOperation(
		OpRead,
		"/test/file.txt",
		100*time.Millisecond,
		1024*1024, // 1MB
		CacheSourceL1,
		nil,
	)

	metrics := dpm.GetOperationMetrics(OpRead)
	if metrics == nil {
		t.Fatal("Expected operation metrics for read")
	}

	if metrics.Count != 1 {
		t.Errorf("Expected count=1, got %d", metrics.Count)
	}

	if metrics.BytesProcessed != 1024*1024 {
		t.Errorf("Expected bytes=1048576, got %d", metrics.BytesProcessed)
	}

	if metrics.CacheHits != 1 {
		t.Errorf("Expected 1 cache hit, got %d", metrics.CacheHits)
	}

	if metrics.CacheMisses != 0 {
		t.Errorf("Expected 0 cache misses, got %d", metrics.CacheMisses)
	}

	if metrics.ErrorCount != 0 {
		t.Errorf("Expected 0 errors, got %d", metrics.ErrorCount)
	}
}

func TestRecordOperation_MultipleOperations(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, false)

	// Record multiple read operations
	for i := 0; i < 10; i++ {
		dpm.RecordOperation(
			OpRead,
			"/test/file.txt",
			time.Duration(100+i*10)*time.Millisecond,
			1024*1024,
			CacheSourceL1,
			nil,
		)
	}

	metrics := dpm.GetOperationMetrics(OpRead)
	if metrics.Count != 10 {
		t.Errorf("Expected count=10, got %d", metrics.Count)
	}

	if metrics.BytesProcessed != 10*1024*1024 {
		t.Errorf("Expected bytes=10485760, got %d", metrics.BytesProcessed)
	}

	// Check average latency is in expected range (100-190ms)
	if metrics.AverageLatency < 100*time.Millisecond || metrics.AverageLatency > 200*time.Millisecond {
		t.Errorf("Expected average latency in range [100ms, 200ms], got %v", metrics.AverageLatency)
	}
}

func TestRecordOperation_ErrorHandling(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, false)

	// Record operations with errors
	dpm.RecordOperation(OpRead, "/test/file.txt", 100*time.Millisecond, 1024, CacheSourceBackend, nil)
	dpm.RecordOperation(OpRead, "/test/file.txt", 150*time.Millisecond, 1024, CacheSourceBackend, errors.New("test error"))
	dpm.RecordOperation(OpRead, "/test/file.txt", 120*time.Millisecond, 1024, CacheSourceBackend, errors.New("another error"))

	metrics := dpm.GetOperationMetrics(OpRead)
	if metrics.Count != 3 {
		t.Errorf("Expected count=3, got %d", metrics.Count)
	}

	if metrics.ErrorCount != 2 {
		t.Errorf("Expected 2 errors, got %d", metrics.ErrorCount)
	}

	if dpm.TotalErrors != 2 {
		t.Errorf("Expected total_errors=2, got %d", dpm.TotalErrors)
	}
}

func TestRecordOperation_CacheSourceTracking(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, false)

	// Record operations from different cache sources
	dpm.RecordOperation(OpRead, "/test/1.txt", 10*time.Millisecond, 1024, CacheSourceL1, nil)
	dpm.RecordOperation(OpRead, "/test/2.txt", 20*time.Millisecond, 1024, CacheSourceL2, nil)
	dpm.RecordOperation(OpRead, "/test/3.txt", 100*time.Millisecond, 1024, CacheSourceBackend, nil)
	dpm.RecordOperation(OpRead, "/test/4.txt", 15*time.Millisecond, 1024, CacheSourceReadAhead, nil)

	metrics := dpm.GetOperationMetrics(OpRead)

	if metrics.CacheHits != 3 {
		t.Errorf("Expected 3 cache hits (L1, L2, ReadAhead), got %d", metrics.CacheHits)
	}

	if metrics.CacheMisses != 1 {
		t.Errorf("Expected 1 cache miss (Backend), got %d", metrics.CacheMisses)
	}

	expectedHitRate := 0.75 // 3/4 = 0.75
	if metrics.CacheHitRate < expectedHitRate-0.01 || metrics.CacheHitRate > expectedHitRate+0.01 {
		t.Errorf("Expected cache hit rate=0.75, got %f", metrics.CacheHitRate)
	}
}

func TestRecordOperation_LatencyTracking(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, false)

	latencies := []time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		75 * time.Millisecond,
		200 * time.Millisecond,
		125 * time.Millisecond,
	}

	for _, lat := range latencies {
		dpm.RecordOperation(OpRead, "/test/file.txt", lat, 1024, CacheSourceL1, nil)
	}

	metrics := dpm.GetOperationMetrics(OpRead)

	if metrics.MinLatency != 50*time.Millisecond {
		t.Errorf("Expected min latency=50ms, got %v", metrics.MinLatency)
	}

	if metrics.MaxLatency != 200*time.Millisecond {
		t.Errorf("Expected max latency=200ms, got %v", metrics.MaxLatency)
	}

	// Average should be 110ms (550/5)
	expectedAvg := 110 * time.Millisecond
	if metrics.AverageLatency != expectedAvg {
		t.Errorf("Expected average latency=110ms, got %v", metrics.AverageLatency)
	}
}

func TestRecordOperation_FileMetrics(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, true) // Enable file tracking

	// Record operations on multiple files
	dpm.RecordOperation(OpRead, "/test/file1.txt", 100*time.Millisecond, 1024, CacheSourceL1, nil)
	dpm.RecordOperation(OpRead, "/test/file1.txt", 110*time.Millisecond, 2048, CacheSourceL2, nil)
	dpm.RecordOperation(OpWrite, "/test/file1.txt", 150*time.Millisecond, 4096, CacheSourceBackend, nil)

	dpm.RecordOperation(OpRead, "/test/file2.txt", 50*time.Millisecond, 512, CacheSourceL1, nil)

	// Check file metrics for file1
	topFiles := dpm.GetTopFiles(10)
	if len(topFiles) != 2 {
		t.Fatalf("Expected 2 tracked files, got %d", len(topFiles))
	}

	// file1 should be first (3 accesses vs 1)
	file1 := topFiles[0]
	if file1.Path != "/test/file1.txt" {
		t.Errorf("Expected file1 to be most accessed, got %s", file1.Path)
	}

	if file1.TotalAccesses != 3 {
		t.Errorf("Expected file1 to have 3 accesses, got %d", file1.TotalAccesses)
	}

	if file1.BytesRead != 1024+2048 {
		t.Errorf("Expected file1 bytes_read=3072, got %d", file1.BytesRead)
	}

	if file1.BytesWritten != 4096 {
		t.Errorf("Expected file1 bytes_written=4096, got %d", file1.BytesWritten)
	}
}

func TestRecordOperation_MaxTrackedFiles(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(2, true) // Only track 2 files

	// Try to record operations on 3 different files
	dpm.RecordOperation(OpRead, "/test/file1.txt", 100*time.Millisecond, 1024, CacheSourceL1, nil)
	dpm.RecordOperation(OpRead, "/test/file2.txt", 100*time.Millisecond, 1024, CacheSourceL1, nil)
	dpm.RecordOperation(OpRead, "/test/file3.txt", 100*time.Millisecond, 1024, CacheSourceL1, nil)

	topFiles := dpm.GetTopFiles(10)
	if len(topFiles) != 2 {
		t.Errorf("Expected only 2 tracked files due to limit, got %d", len(topFiles))
	}
}

func TestRecordNetworkOperation(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, false)

	// Record network operation
	bytesUp := int64(1024 * 1024)       // 1MB upload
	bytesDown := int64(5 * 1024 * 1024) // 5MB download
	duration := 1 * time.Second

	dpm.RecordNetworkOperation(bytesUp, bytesDown, duration, nil)

	nu := dpm.NetworkUtilization
	if nu.BytesUploaded != bytesUp {
		t.Errorf("Expected bytes_uploaded=%d, got %d", bytesUp, nu.BytesUploaded)
	}

	if nu.BytesDownloaded != bytesDown {
		t.Errorf("Expected bytes_downloaded=%d, got %d", bytesDown, nu.BytesDownloaded)
	}

	if nu.TotalBandwidthUsed != bytesUp+bytesDown {
		t.Errorf("Expected total_bandwidth=%d, got %d", bytesUp+bytesDown, nu.TotalBandwidthUsed)
	}

	if nu.RequestCount != 1 {
		t.Errorf("Expected request_count=1, got %d", nu.RequestCount)
	}

	// Check rates (should be ~1 MB/s upload, ~5 MB/s download)
	if nu.UploadRate < 0.9 || nu.UploadRate > 1.1 {
		t.Errorf("Expected upload rate ~1 MB/s, got %f", nu.UploadRate)
	}

	if nu.DownloadRate < 4.9 || nu.DownloadRate > 5.1 {
		t.Errorf("Expected download rate ~5 MB/s, got %f", nu.DownloadRate)
	}
}

func TestRecordNetworkOperation_PeakRates(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, false)

	// Record multiple operations with different rates
	dpm.RecordNetworkOperation(1024*1024, 5*1024*1024, 1*time.Second, nil)    // 1 MB/s, 5 MB/s
	dpm.RecordNetworkOperation(10*1024*1024, 2*1024*1024, 1*time.Second, nil) // 10 MB/s, 2 MB/s
	dpm.RecordNetworkOperation(2*1024*1024, 20*1024*1024, 1*time.Second, nil) // 2 MB/s, 20 MB/s

	nu := dpm.NetworkUtilization

	// Peak upload should be ~10 MB/s
	if nu.PeakUploadRate < 9.9 || nu.PeakUploadRate > 10.1 {
		t.Errorf("Expected peak upload rate ~10 MB/s, got %f", nu.PeakUploadRate)
	}

	// Peak download should be ~20 MB/s
	if nu.PeakDownloadRate < 19.9 || nu.PeakDownloadRate > 20.1 {
		t.Errorf("Expected peak download rate ~20 MB/s, got %f", nu.PeakDownloadRate)
	}
}

func TestRecordCost(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, false)

	// Record cost for read operations
	requestCost := 0.0004                         // $0.0004 per request
	storageCost := 0.023                          // $0.023 per GB-month
	transferCost := 0.09                          // $0.09 per GB
	bytesTransferred := int64(1024 * 1024 * 1024) // 1GB

	dpm.RecordCost(OpRead, requestCost, storageCost, transferCost, bytesTransferred)

	costMetrics := dpm.CostMetrics[OpRead]
	if costMetrics == nil {
		t.Fatal("Expected cost metrics for read operations")
	}

	if costMetrics.RequestCount != 1 {
		t.Errorf("Expected request_count=1, got %d", costMetrics.RequestCount)
	}

	expectedTotal := requestCost + storageCost + transferCost
	if costMetrics.TotalCost != expectedTotal {
		t.Errorf("Expected total_cost=%f, got %f", expectedTotal, costMetrics.TotalCost)
	}

	if costMetrics.BytesTransferred != bytesTransferred {
		t.Errorf("Expected bytes_transferred=%d, got %d", bytesTransferred, costMetrics.BytesTransferred)
	}
}

func TestCacheBreakdown(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, false)

	// Record read operations from different cache sources
	dpm.RecordOperation(OpRead, "/test/1.txt", 10*time.Millisecond, 1024, CacheSourceL1, nil)
	dpm.RecordOperation(OpRead, "/test/2.txt", 10*time.Millisecond, 1024, CacheSourceL1, nil)
	dpm.RecordOperation(OpRead, "/test/3.txt", 30*time.Millisecond, 1024, CacheSourceL2, nil)
	dpm.RecordOperation(OpRead, "/test/4.txt", 100*time.Millisecond, 1024, CacheSourceBackend, nil)

	cb := dpm.CacheBreakdown[OpRead]
	if cb == nil {
		t.Fatal("Expected cache breakdown for read operations")
	}

	if cb.L1Hits != 2 {
		t.Errorf("Expected 2 L1 hits, got %d", cb.L1Hits)
	}

	if cb.L2Hits != 1 {
		t.Errorf("Expected 1 L2 hit, got %d", cb.L2Hits)
	}

	if cb.BackendFetch != 1 {
		t.Errorf("Expected 1 backend fetch, got %d", cb.BackendFetch)
	}

	if cb.TotalRequests != 4 {
		t.Errorf("Expected 4 total requests, got %d", cb.TotalRequests)
	}

	// Check hit rates
	expectedL1Rate := 0.5 // 2/4
	if cb.L1HitRate < expectedL1Rate-0.01 || cb.L1HitRate > expectedL1Rate+0.01 {
		t.Errorf("Expected L1 hit rate=0.5, got %f", cb.L1HitRate)
	}

	expectedTotalHitRate := 0.75 // (2+1)/4
	if cb.TotalHitRate < expectedTotalHitRate-0.01 || cb.TotalHitRate > expectedTotalHitRate+0.01 {
		t.Errorf("Expected total hit rate=0.75, got %f", cb.TotalHitRate)
	}
}

func TestGetSummary(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, true)

	// Record some operations
	for i := 0; i < 100; i++ {
		dpm.RecordOperation(OpRead, "/test/file.txt", 100*time.Millisecond, 1024*1024, CacheSourceL1, nil)
	}

	// Record some errors
	for i := 0; i < 5; i++ {
		dpm.RecordOperation(OpWrite, "/test/file.txt", 200*time.Millisecond, 2048, CacheSourceBackend, errors.New("test error"))
	}

	summary := dpm.GetSummary()

	if summary["total_operations"] != int64(105) {
		t.Errorf("Expected total_operations=105, got %v", summary["total_operations"])
	}

	if summary["total_errors"] != int64(5) {
		t.Errorf("Expected total_errors=5, got %v", summary["total_errors"])
	}

	// Error rate should be ~4.76% (5/105)
	errorRate := summary["overall_error_rate"].(float64)
	expectedErrorRate := 5.0 / 105.0
	if errorRate < expectedErrorRate-0.01 || errorRate > expectedErrorRate+0.01 {
		t.Errorf("Expected error rate ~4.76%%, got %f%%", errorRate*100)
	}
}

func TestReset(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, true)

	// Record some operations
	dpm.RecordOperation(OpRead, "/test/file.txt", 100*time.Millisecond, 1024, CacheSourceL1, nil)
	dpm.RecordNetworkOperation(1024, 2048, 1*time.Second, nil)
	dpm.RecordCost(OpRead, 0.001, 0.023, 0.09, 1024*1024)

	// Verify metrics exist
	if dpm.TotalOperations == 0 {
		t.Error("Expected operations to be recorded before reset")
	}

	// Reset
	dpm.Reset()

	// Verify everything is reset
	if dpm.TotalOperations != 0 {
		t.Errorf("Expected total_operations=0 after reset, got %d", dpm.TotalOperations)
	}

	if dpm.TotalErrors != 0 {
		t.Errorf("Expected total_errors=0 after reset, got %d", dpm.TotalErrors)
	}

	if dpm.TotalBytesProcessed != 0 {
		t.Errorf("Expected total_bytes_processed=0 after reset, got %d", dpm.TotalBytesProcessed)
	}

	if len(dpm.OperationMetrics) != 0 {
		t.Errorf("Expected empty operation metrics after reset, got %d entries", len(dpm.OperationMetrics))
	}

	if len(dpm.FileMetrics) != 0 {
		t.Errorf("Expected empty file metrics after reset, got %d entries", len(dpm.FileMetrics))
	}
}

func TestMultipleOperationTypes(t *testing.T) {
	dpm := NewDetailedPerformanceMetrics(100, false)

	// Record different operation types
	operations := []OperationType{OpRead, OpWrite, OpDelete, OpList, OpGetAttr}

	for _, opType := range operations {
		dpm.RecordOperation(opType, "/test/file.txt", 100*time.Millisecond, 1024, CacheSourceL1, nil)
	}

	// Verify each operation type has metrics
	for _, opType := range operations {
		metrics := dpm.GetOperationMetrics(opType)
		if metrics == nil {
			t.Errorf("Expected metrics for operation type %s", opType)
			continue
		}

		if metrics.Count != 1 {
			t.Errorf("Expected count=1 for %s, got %d", opType, metrics.Count)
		}
	}
}
