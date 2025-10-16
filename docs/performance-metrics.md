# Detailed Performance Metrics

ObjectFS provides a comprehensive performance metrics system for tracking filesystem operations, cache performance, network utilization, and operational costs at granular levels.

## Overview

The detailed performance metrics system (`internal/metrics/detailed.go`) provides:

- **Per-operation metrics**: Latency percentiles, throughput, cache hit rates, error rates
- **Per-file tracking**: Most accessed files with operation breakdowns
- **Cache breakdown**: Performance metrics by cache source (L1, L2, Backend, ReadAhead)
- **Network utilization**: Upload/download rates, bandwidth tracking, connection metrics
- **Cost tracking**: Per-operation cost breakdowns with monthly projections

## Quick Start

### Creating a Metrics Collector

```go
import "github.com/objectfs/objectfs/internal/metrics"

// Create with default settings (top 100 files tracked)
dpm := metrics.NewDetailedPerformanceMetrics()

// Create with custom configuration
dpm := metrics.NewDetailedPerformanceMetricsWithOptions(
    true,  // TopFilesEnabled
    1000,  // MaxTrackedFiles
)

// Create with file tracking disabled
dpm := metrics.NewDetailedPerformanceMetricsWithOptions(
    false, // TopFilesEnabled
    0,     // MaxTrackedFiles (ignored when disabled)
)
```

### Recording Operations

```go
import (
    "time"
    "github.com/objectfs/objectfs/internal/metrics"
)

// Record a successful read operation
latency := 15 * time.Millisecond
bytesRead := int64(4096)
dpm.RecordOperation(
    metrics.OpRead,
    "/path/to/file.txt",
    latency,
    bytesRead,
    metrics.CacheSourceL1, // Cache hit from L1
    nil, // No error
)

// Record a failed write operation
err := errors.New("disk full")
dpm.RecordOperation(
    metrics.OpWrite,
    "/path/to/output.dat",
    50*time.Millisecond,
    0, // No bytes written
    metrics.CacheSourceNone,
    err,
)

// Record a cache miss that went to backend
dpm.RecordOperation(
    metrics.OpRead,
    "/remote/large-file.bin",
    250*time.Millisecond,
    1048576, // 1 MB
    metrics.CacheSourceBackend, // Read from S3
    nil,
)
```

### Recording Network Operations

```go
// Record an upload operation
uploadRate := float64(50 * 1024 * 1024) // 50 MB/s
dpm.RecordNetworkOperation(true, uploadRate, 1)

// Record a download operation
downloadRate := float64(100 * 1024 * 1024) // 100 MB/s
dpm.RecordNetworkOperation(false, downloadRate, 1)
```

### Recording Costs

```go
// Record cost for a PUT operation
dpm.RecordCost(
    metrics.OpWrite,
    0.000005, // $0.000005 per request
    1024*1024, // 1 MB transferred
)

// Record cost for a GET operation
dpm.RecordCost(
    metrics.OpRead,
    0.0000004, // $0.0004 per 1000 requests
    10*1024*1024, // 10 MB transferred
)
```

### Retrieving Metrics

```go
// Get metrics for a specific operation type
readMetrics := dpm.GetOperationMetrics(metrics.OpRead)
fmt.Printf("Read operations: %d\n", readMetrics.Count)
fmt.Printf("Average latency: %v\n", readMetrics.AverageLatency)
fmt.Printf("P95 latency: %v\n", readMetrics.P95Latency)
fmt.Printf("Cache hit rate: %.2f%%\n", readMetrics.CacheHitRate*100)
fmt.Printf("Throughput: %.2f MB/s\n", readMetrics.ThroughputMBps)

// Get top accessed files
topFiles := dpm.GetTopFiles(10)
for i, file := range topFiles {
    fmt.Printf("%d. %s - %d accesses, %.2f%% cache hit rate\n",
        i+1, file.Path, file.TotalAccesses, file.CacheHitRate*100)
}

// Get cache breakdown for an operation
cacheMetrics := dpm.GetCacheBreakdown(metrics.OpRead)
fmt.Printf("L1 hits: %d (%.1fms avg)\n",
    cacheMetrics.L1Hits, cacheMetrics.L1AvgLatency.Milliseconds())
fmt.Printf("L2 hits: %d (%.1fms avg)\n",
    cacheMetrics.L2Hits, cacheMetrics.L2AvgLatency.Milliseconds())
fmt.Printf("Backend hits: %d (%.1fms avg)\n",
    cacheMetrics.BackendHits, cacheMetrics.BackendAvgLatency.Milliseconds())

// Get network utilization
netMetrics := dpm.GetNetworkUtilization()
fmt.Printf("Current upload rate: %.2f MB/s\n", netMetrics.CurrentUploadRate/(1024*1024))
fmt.Printf("Peak download rate: %.2f MB/s\n", netMetrics.PeakDownloadRate/(1024*1024))
fmt.Printf("Network error rate: %.2f%%\n", netMetrics.ErrorRate*100)

// Get cost metrics for an operation
costs := dpm.GetCostMetrics(metrics.OpWrite)
fmt.Printf("Total cost: $%.6f\n", costs.TotalCost)
fmt.Printf("Monthly projection: $%.2f\n", costs.MonthlyProjection)
fmt.Printf("Avg cost per request: $%.8f\n", costs.AvgCostPerRequest)

// Get overall summary
summary := dpm.GetSummary()
fmt.Printf("Total operations: %d\n", summary.TotalOperations)
fmt.Printf("Overall cache hit rate: %.2f%%\n", summary.OverallCacheHitRate*100)
fmt.Printf("Overall error rate: %.2f%%\n", summary.OverallErrorRate*100)
fmt.Printf("Uptime: %v\n", summary.Uptime)
```

## Operation Types

The following operation types are tracked:

| Operation Type | Description |
|---------------|-------------|
| `OpRead` | File read operations |
| `OpWrite` | File write operations |
| `OpDelete` | File deletion operations |
| `OpList` | Directory listing operations |
| `OpGetAttr` | Get file attributes (stat) |
| `OpSetAttr` | Set file attributes (chmod, chown, etc.) |
| `OpCreate` | File creation operations |
| `OpRename` | File rename/move operations |
| `OpReadDir` | Read directory contents |
| `OpMkdir` | Create directory |
| `OpRmdir` | Remove directory |
| `OpOpen` | Open file handle |
| `OpRelease` | Close file handle |
| `OpTruncate` | Truncate file |
| `OpChmod` | Change file mode |
| `OpChown` | Change file ownership |
| `OpLink` | Create hard link |
| `OpSymlink` | Create symbolic link |
| `OpStatfs` | Filesystem statistics |
| `OpFlush` | Flush file buffers |
| `OpFsync` | Synchronize file to storage |

## Cache Source Types

Cache sources indicate where data was served from:

| Cache Source | Description |
|-------------|-------------|
| `CacheSourceL1` | Level 1 cache (memory) |
| `CacheSourceL2` | Level 2 cache (persistent) |
| `CacheSourceBackend` | Backend storage (S3) |
| `CacheSourceReadAhead` | Prefetched by read-ahead system |
| `CacheSourceNone` | No cache used (e.g., write operations) |

## Metric Structures

### DetailedOperationMetrics

Per-operation type metrics:

```go
type DetailedOperationMetrics struct {
    Count              int64         // Total operation count
    TotalLatency       time.Duration // Sum of all latencies
    MinLatency         time.Duration // Minimum latency observed
    MaxLatency         time.Duration // Maximum latency observed
    AverageLatency     time.Duration // Average latency
    P50Latency         time.Duration // 50th percentile latency
    P95Latency         time.Duration // 95th percentile latency
    P99Latency         time.Duration // 99th percentile latency
    ErrorCount         int64         // Number of errors
    BytesProcessed     int64         // Total bytes processed
    CacheHits          int64         // Cache hit count
    CacheMisses        int64         // Cache miss count
    CacheHitRate       float64       // Cache hit rate (0.0-1.0)
    AvgBytesPerOp      float64       // Average bytes per operation
    ThroughputMBps     float64       // Throughput in MB/s
    LastOperationTime  time.Time     // Timestamp of last operation
}
```

### FileOperationMetrics

Per-file tracking metrics:

```go
type FileOperationMetrics struct {
    Path               string
    TotalAccesses      int64
    BytesRead          int64
    BytesWritten       int64
    LastAccessTime     time.Time
    FirstAccessTime    time.Time
    Operations         map[OperationType]*DetailedOperationMetrics
    CacheHitRate       float64
    AvgLatency         time.Duration
}
```

### CacheBreakdownMetrics

Cache performance breakdown by source:

```go
type CacheBreakdownMetrics struct {
    L1Hits          int64
    L1Misses        int64
    L1HitRate       float64
    L1AvgLatency    time.Duration
    L2Hits          int64
    L2Misses        int64
    L2HitRate       float64
    L2AvgLatency    time.Duration
    BackendHits     int64
    BackendMisses   int64
    BackendHitRate  float64
    BackendAvgLatency time.Duration
    ReadAheadHits   int64
    ReadAheadMisses int64
    ReadAheadHitRate float64
    ReadAheadAvgLatency time.Duration
}
```

### NetworkUtilizationMetrics

Network performance tracking:

```go
type NetworkUtilizationMetrics struct {
    CurrentUploadRate    float64
    CurrentDownloadRate  float64
    PeakUploadRate       float64
    PeakDownloadRate     float64
    TotalBytesUploaded   int64
    TotalBytesDownloaded int64
    ActiveConnections    int64
    TotalConnections     int64
    FailedConnections    int64
    AvgConnectionTime    time.Duration
    ErrorCount           int64
    ErrorRate            float64
    LastUpdateTime       time.Time
}
```

### CostMetrics

Cost tracking per operation type:

```go
type CostMetrics struct {
    TotalCost          float64
    RequestCount       int64
    BytesTransferred   int64
    StorageCost        float64
    RequestCost        float64
    TransferCost       float64
    AvgCostPerRequest  float64
    AvgCostPerGB       float64
    MonthlyProjection  float64
    LastUpdateTime     time.Time
}
```

## Latency Percentiles

Latency percentiles (P50, P95, P99) are calculated using histogram buckets:

- **P50 (median)**: 50% of operations complete faster than this latency
- **P95**: 95% of operations complete faster than this latency
- **P99**: 99% of operations complete faster than this latency

Histogram buckets (in milliseconds):
```
1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000+
```

## Thread Safety

All metrics operations are thread-safe:

- Uses `sync.RWMutex` for concurrent access
- Read operations use read locks (multiple concurrent readers allowed)
- Write operations use write locks (exclusive access)
- Copy-on-read pattern prevents lock holding during processing

## Performance Considerations

### Memory Usage

**File Tracking**: With `MaxTrackedFiles=100` (default), expect ~50-100 KB memory usage. Each tracked file uses approximately 500-1000 bytes depending on the number of operation types used.

**Operation Metrics**: ~2 KB per operation type (21 types = ~42 KB)

**Total**: ~50-150 KB typical memory footprint

### CPU Impact

**Recording Operations**: ~500ns per RecordOperation() call (includes lock acquisition, histogram update, and metric calculations)

**Retrieving Metrics**: Read operations are fast (~100-200ns) due to read locks allowing concurrent access

### Optimization Tips

1. **Disable file tracking** if not needed: `TopFilesEnabled=false` reduces memory by ~80%
2. **Reduce MaxTrackedFiles** for lower memory: Set to 10-50 for minimal tracking
3. **Batch metric reads**: Get summary once per minute instead of per operation
4. **Use dedicated metrics goroutine**: Periodically export metrics instead of inline

## Integration Examples

### HTTP Metrics Endpoint

```go
import (
    "encoding/json"
    "net/http"
)

func metricsHandler(dpm *metrics.DetailedPerformanceMetrics) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        summary := dpm.GetSummary()
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(summary)
    }
}

// In your server setup
http.HandleFunc("/metrics/detailed", metricsHandler(dpm))
```

### Prometheus Integration

```go
import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    opLatency = promauto.NewHistogramVec(
        prometheus.HistogramOpts{
            Name: "objectfs_operation_latency_seconds",
            Help: "Operation latency in seconds",
            Buckets: prometheus.ExponentialBuckets(0.001, 2, 15),
        },
        []string{"operation", "cache_source"},
    )
)

// Export metrics periodically
func exportMetrics(dpm *metrics.DetailedPerformanceMetrics) {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()

    for range ticker.C {
        for _, opType := range []metrics.OperationType{
            metrics.OpRead, metrics.OpWrite, metrics.OpDelete,
        } {
            m := dpm.GetOperationMetrics(opType)
            opLatency.WithLabelValues(
                string(opType),
                "all",
            ).Observe(m.AverageLatency.Seconds())
        }
    }
}
```

### Logging Integration

```go
import "log"

// Log metrics summary every minute
func logMetrics(dpm *metrics.DetailedPerformanceMetrics) {
    ticker := time.NewTicker(1 * time.Minute)
    defer ticker.Stop()

    for range ticker.C {
        summary := dpm.GetSummary()
        log.Printf("Metrics Summary: ops=%d cache_hit=%.2f%% errors=%.2f%% uptime=%v",
            summary.TotalOperations,
            summary.OverallCacheHitRate*100,
            summary.OverallErrorRate*100,
            summary.Uptime,
        )

        // Log top files
        topFiles := dpm.GetTopFiles(5)
        for i, file := range topFiles {
            log.Printf("  Top %d: %s (%d accesses, %.2f%% cache hit)",
                i+1, file.Path, file.TotalAccesses, file.CacheHitRate*100)
        }
    }
}
```

### Storage Backend Integration

```go
import "github.com/objectfs/objectfs/internal/storage/s3"

// Example: Instrument S3 backend operations
func (b *S3Backend) ReadObject(ctx context.Context, key string) ([]byte, error) {
    start := time.Now()

    // Attempt cache lookup
    data, cacheSource, err := b.cache.Get(key)
    if err == nil {
        // Cache hit
        b.metrics.RecordOperation(
            metrics.OpRead,
            key,
            time.Since(start),
            int64(len(data)),
            cacheSource, // CacheSourceL1 or CacheSourceL2
            nil,
        )
        return data, nil
    }

    // Cache miss - read from S3
    data, err = b.client.GetObject(ctx, key)
    latency := time.Since(start)

    if err != nil {
        b.metrics.RecordOperation(
            metrics.OpRead,
            key,
            latency,
            0,
            metrics.CacheSourceNone,
            err,
        )
        return nil, err
    }

    // Record successful backend read
    b.metrics.RecordOperation(
        metrics.OpRead,
        key,
        latency,
        int64(len(data)),
        metrics.CacheSourceBackend,
        nil,
    )

    // Record network operation
    downloadRate := float64(len(data)) / latency.Seconds()
    b.metrics.RecordNetworkOperation(false, downloadRate, 1)

    // Record cost (example: $0.0004 per 1000 requests, $0.09 per GB)
    b.metrics.RecordCost(
        metrics.OpRead,
        0.0000004, // Request cost
        int64(len(data)),
    )

    return data, nil
}
```

## Resetting Metrics

Reset all metrics to start fresh:

```go
dpm.Reset()
```

This is useful for:
- Testing scenarios where you need clean metrics
- Periodic resets to prevent counter overflow
- Starting new measurement windows

**Note**: Reset is thread-safe but will briefly block all metric operations.

## Best Practices

1. **Create once, use everywhere**: Create a single `DetailedPerformanceMetrics` instance and pass it to components that need it

2. **Record all operations**: Even errors should be recorded to track error rates accurately

3. **Use appropriate cache sources**: Correctly identify cache sources to get accurate cache breakdown metrics

4. **Monitor memory usage**: If tracking thousands of files, consider reducing `MaxTrackedFiles` or disabling file tracking

5. **Export metrics regularly**: Don't let metrics accumulate indefinitely; export and reset periodically

6. **Combine with existing metrics**: Use detailed metrics alongside Prometheus collector for comprehensive monitoring

7. **Profile before optimizing**: Use metrics to identify bottlenecks before applying optimizations

8. **Cost tracking**: Record actual AWS costs to validate projections and optimize spending

## Troubleshooting

### High Memory Usage

**Problem**: Metrics using too much memory

**Solutions**:
- Reduce `MaxTrackedFiles` (e.g., from 100 to 10)
- Disable file tracking: `TopFilesEnabled=false`
- Reset metrics more frequently
- Check for file path leaks (are files being deleted from tracking?)

### Inaccurate Percentiles

**Problem**: P95/P99 latencies seem wrong

**Causes**:
- Not enough samples (need at least 100 operations for accurate percentiles)
- Outliers skewing distribution (check max latency)
- Histogram bucket boundaries not suitable for your latency range

**Solutions**:
- Wait for more samples to accumulate
- Check for performance issues causing outliers
- Consider adjusting histogram buckets if needed

### Cache Hit Rate Always 0%

**Problem**: Cache hit rate shows 0% but caching is working

**Cause**: Not recording cache source correctly

**Solution**: Ensure you're passing the correct `CacheSourceType` when recording operations:
```go
// Wrong
dpm.RecordOperation(metrics.OpRead, path, latency, bytes, metrics.CacheSourceNone, nil)

// Right
dpm.RecordOperation(metrics.OpRead, path, latency, bytes, metrics.CacheSourceL1, nil)
```

### Cost Projections Too High/Low

**Problem**: Monthly cost projections don't match actual bills

**Causes**:
- Recording costs incorrectly
- Not accounting for all operation types
- Usage patterns changed since last projection

**Solutions**:
- Verify cost recording matches AWS pricing
- Ensure all S3 operations record costs
- Use shorter projection windows (weekly instead of monthly)
- Compare projections against actual AWS Cost Explorer data

## Related Documentation

- [Metrics Collector](../internal/metrics/collector.go) - Prometheus-based metrics
- [S3 Backend Metrics](../internal/storage/s3/metrics.go) - S3-specific metrics
- [Cache Metrics](../internal/cache/multilevel.go) - Cache layer metrics
- [Performance Tuning Guide](./performance-tuning.md) - Using metrics to optimize performance
- [Cost Optimization Guide](./cost-optimization.md) - Using cost metrics to reduce AWS spend

## API Reference

See [internal/metrics/detailed.go](../internal/metrics/detailed.go) for complete API documentation.

## Testing

See [internal/metrics/detailed_test.go](../internal/metrics/detailed_test.go) for comprehensive test examples covering all functionality.
