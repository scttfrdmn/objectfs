# S3 Transfer Acceleration

ObjectFS supports AWS S3 Transfer Acceleration for faster uploads and downloads
across long distances. This feature automatically detects and handles
acceleration failures with transparent fallback to standard S3 endpoints.

## Overview

S3 Transfer Acceleration uses Amazon CloudFront's globally distributed edge
locations to accelerate uploads and downloads. Data arrives at an edge location
and is routed to Amazon S3 over an optimized network path.

**Key Benefits:**
- Up to 50-500% faster transfers for long-distance operations
- Automatic fallback on acceleration errors
- Transparent integration - no code changes required
- Built-in metrics tracking for monitoring

## Prerequisites

Before enabling S3 Transfer Acceleration in ObjectFS:

1. **Enable Transfer Acceleration on your S3 bucket:**
   ```bash
   aws s3api put-bucket-accelerate-configuration \
       --bucket your-bucket-name \
       --accelerate-configuration Status=Enabled
   ```

2. **Verify acceleration is enabled:**
   ```bash
   aws s3api get-bucket-accelerate-configuration \
       --bucket your-bucket-name
   ```

## Configuration

### YAML Configuration

```yaml
s3:
  region: us-west-2
  use_accelerate: true  # Enable S3 Transfer Acceleration
  pool_size: 20         # Recommended for acceleration
```

### Go API Configuration

```go
import "github.com/objectfs/objectfs/internal/storage/s3"

// Create S3 config with acceleration enabled
cfg := s3.NewDefaultConfig()
cfg.Region = "us-west-2"
cfg.UseAccelerate = true
cfg.PoolSize = 20  // Increase pool for better acceleration performance

// Create backend
backend, err := s3.NewBackend(ctx, "your-bucket-name", cfg)
if err != nil {
    log.Fatal(err)
}
defer backend.Close()
```

## Features

### Automatic Fallback

ObjectFS automatically detects acceleration-specific errors and falls back to
standard S3 endpoints:

**Detected Errors:**
- `InvalidRequest` - Acceleration not enabled on bucket
- `AccelerateNotSupported` - Bucket doesn't support acceleration
- Acceleration endpoint connection failures
- S3-accelerate endpoint errors

**Fallback Behavior:**
1. Attempt operation with accelerated endpoint
2. Detect acceleration error
3. Log fallback event
4. Retry operation with standard endpoint
5. Temporarily disable acceleration to avoid repeated failures

**Re-enabling:**
- Automatic re-enable after successful standard operations
- Manual re-enable via `backend.GetClientManager().EnableAcceleration()`

### Metrics Tracking

Monitor acceleration performance with built-in metrics:

```go
metrics := backend.GetMetrics()

fmt.Printf("Acceleration Status: %v\n", metrics.AccelerationEnabled)
fmt.Printf("Accelerated Requests: %d\n", metrics.AcceleratedRequests)
fmt.Printf("Accelerated Bytes: %d\n", metrics.AcceleratedBytes)
fmt.Printf("Fallback Events: %d\n", metrics.FallbackEvents)
fmt.Printf("Acceleration Rate: %.2f%%\n",
    backend.GetMetricsCollector().GetAccelerationRate())
```

### Integration with CargoShip

ObjectFS uses CargoShip for optimized uploads when available. The acceleration
feature integrates seamlessly:

**Upload Priority:**
1. CargoShip optimization (primary) - 4.6x performance improvement
2. S3 Transfer Acceleration (fallback) - Uses acceleration if CargoShip fails
3. Standard S3 endpoint (final fallback)

**Example Flow:**
```
PutObject Request
    ↓
CargoShip Upload (if available)
    ↓ (if fails)
Accelerated Endpoint
    ↓ (if acceleration error)
Standard S3 Endpoint
```

## Performance

### When to Use

S3 Transfer Acceleration works best for:

**✅ Recommended:**
- Long-distance transfers (cross-continent, cross-region)
- Large files (>10MB)
- Remote clients connecting to distant S3 regions
- High-latency network connections
- Geographic distribution of clients

**❌ Not Recommended:**
- Same-region transfers (minimal benefit)
- Very small files (<1KB) - overhead exceeds benefit
- Applications already close to S3 region

### Expected Performance

Performance improvement varies by distance and network conditions:

| Source → S3 Region | Typical Improvement |
|-------------------|---------------------|
| Same region       | 0-10%               |
| Cross-region (US) | 50-100%             |
| Cross-continent   | 100-500%            |
| International     | 200-500%            |

### Benchmarking

ObjectFS includes comprehensive benchmarks to measure acceleration performance:

```bash
# Set up environment
export OBJECTFS_BENCH_BUCKET=your-bucket-name
export OBJECTFS_BENCH_REGION=us-west-2

# Run benchmarks
go test -bench=. -benchmem ./internal/storage/s3/

# Compare standard vs accelerated
go test -bench='BenchmarkGetObject_(Standard|Accelerated)' \
    ./internal/storage/s3/

# Test specific object sizes
go test -bench='BenchmarkGetObject_Large' \
    ./internal/storage/s3/
```

**Example Output:**
```
BenchmarkGetObject_Standard-8        100    12.5 MB/s    1048576 B/op
BenchmarkGetObject_Accelerated-8     250    31.2 MB/s    1048576 B/op
BenchmarkPutObject_Standard-8        150    10.8 MB/s    1048576 B/op
BenchmarkPutObject_Accelerated-8     400    27.5 MB/s    1048576 B/op
```

## Monitoring

### Health Checks

ObjectFS tracks acceleration health:

```go
// Check if acceleration is active
if backend.GetClientManager().IsAccelerationActive() {
    log.Info("S3 Transfer Acceleration is active")
}

// Get acceleration rate
rate := backend.GetMetricsCollector().GetAccelerationRate()
if rate < 50.0 {
    log.Warn("Low acceleration rate", "rate", rate)
}
```

### Logging

Acceleration events are logged automatically:

```
INFO  S3 Transfer Acceleration enabled    component=s3-backend bucket=my-bucket
WARN  S3 Transfer Acceleration error detected, falling back to standard endpoint
      operation=GetObject error=InvalidRequest
INFO  Re-enabling S3 Transfer Acceleration    component=s3-client
```

### Metrics Integration

Export metrics to monitoring systems:

```go
// Prometheus example
accelerationGauge.Set(float64(metrics.AcceleratedRequests))
fallbackCounter.Add(float64(metrics.FallbackEvents))
```

## Troubleshooting

### Issue: Acceleration Not Working

**Symptoms:**
- No accelerated requests in metrics
- FallbackEvents increasing
- Performance not improving

**Solutions:**

1. **Verify bucket acceleration is enabled:**
   ```bash
   aws s3api get-bucket-accelerate-configuration --bucket your-bucket
   ```

2. **Check bucket location:**
   - Transfer Acceleration not supported in China regions
   - Some GovCloud regions have limited support

3. **Verify network connectivity:**
   - Test accelerated endpoint: `bucketname.s3-accelerate.amazonaws.com`
   - Check firewall rules allow acceleration endpoints

4. **Review logs for specific errors:**
   ```go
   metrics := backend.GetMetrics()
   if metrics.FallbackEvents > 0 {
       log.Warn("Acceleration fallbacks detected",
           "count", metrics.FallbackEvents)
   }
   ```

### Issue: High Fallback Rate

**Symptoms:**
- FallbackRate > 50%
- Frequent acceleration disablement

**Solutions:**

1. **Check bucket configuration:**
   - Ensure acceleration is properly enabled
   - Verify no conflicting bucket policies

2. **Review error patterns:**
   - Look for repeated acceleration errors in logs
   - Check if errors are intermittent or persistent

3. **Consider disabling if not beneficial:**
   ```go
   cfg.UseAccelerate = false  // Disable if causing issues
   ```

### Issue: No Performance Improvement

**Symptoms:**
- Acceleration active but speed unchanged
- High acceleration rate but same throughput

**Possible Causes:**

1. **Same-region transfers** - Minimal benefit expected
2. **Small files** - Overhead negates benefits for files < 1KB
3. **Network bottleneck** - Local network slower than S3
4. **CPU bound** - Processing is the bottleneck, not transfer

**Verification:**
```bash
# Run benchmarks to compare
go test -bench='BenchmarkGetObject' ./internal/storage/s3/

# Compare accelerated vs standard throughput
# If similar, acceleration may not help your use case
```

## Best Practices

1. **Enable acceleration for production workloads:**
   - Test in staging first
   - Monitor metrics for 24-48 hours
   - Verify cost-benefit ratio

2. **Use appropriate pool sizes:**
   ```go
   cfg.PoolSize = 20  // Higher for better acceleration utilization
   ```

3. **Monitor fallback events:**
   - Set up alerts for high fallback rates
   - Investigate persistent fallback issues

4. **Benchmark your specific use case:**
   - Use provided benchmarks as baseline
   - Test with your actual data sizes and patterns
   - Measure from your deployment regions

5. **Consider cost implications:**
   - Acceleration adds $0.04-0.08 per GB transfer
   - Evaluate cost vs time savings for your use case
   - Review AWS Transfer Acceleration pricing

6. **Combine with CargoShip optimization:**
   - Leave CargoShip enabled alongside acceleration
   - Let ObjectFS choose optimal path automatically
   - Benefits stack for maximum performance

## API Reference

### Configuration Options

```go
type Config struct {
    // Enable S3 Transfer Acceleration
    UseAccelerate bool `yaml:"use_accelerate"`

    // Other S3 settings
    Region         string        `yaml:"region"`
    PoolSize       int           `yaml:"pool_size"`
    RequestTimeout time.Duration `yaml:"request_timeout"`
}
```

### Client Manager Methods

```go
// Check acceleration status
IsAccelerationActive() bool

// Manually control acceleration
EnableAcceleration()
DisableAcceleration(reason string)

// Get acceleration-aware clients
GetAcceleratedClient() *s3.Client
GetStandardClient() *s3.Client
```

### Metrics Methods

```go
type BackendMetrics struct {
    AccelerationEnabled  bool
    AcceleratedRequests  int64
    AcceleratedBytes     int64
    FallbackEvents       int64
}

// Get metrics
metrics := backend.GetMetrics()

// Get rates
rate := metricsCollector.GetAccelerationRate()     // % of requests accelerated
fallback := metricsCollector.GetFallbackRate()     // % of accelerated that failed
```

## Related Documentation

- [S3 Backend Configuration](./storage-backends.md#s3)
- [Performance Tuning](./performance-tuning.md)
- [Monitoring and Metrics](./monitoring.md)
- [CargoShip Optimization](./cargoship.md)

## Additional Resources

- [AWS S3 Transfer Acceleration](https://aws.amazon.com/s3/transfer-acceleration/)
- [S3 Transfer Acceleration Speed Comparison](https://s3-accelerate-speedtest.s3-accelerate.amazonaws.com/en/accelerate-speed-comparsion.html)
- [AWS Pricing - Transfer Acceleration](https://aws.amazon.com/s3/pricing/)
