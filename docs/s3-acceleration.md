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
storage:
  s3:
    region: us-west-2
    use_acceleration: true   # Enable S3 Transfer Acceleration

performance:
  connection_pool_size: 20   # a larger pool helps acceleration keep its endpoints busy
```

The YAML keys and the Go field names are not the same, and this section previously mixed them: it
showed `use_accelerate` and `pool_size` at a top-level `s3:`, which are the *internal*
`s3.Config`'s names — spelled `use_acceleration` under `storage.s3` and `connection_pool_size`
under `performance` in a config file. The loader now decodes strictly, so the old block does not
quietly do nothing; it refuses to start and names the key. The Go snippet below sets the internal
struct directly and is correct as written for that use.

### Go API Configuration

```go
import "github.com/scttfrdmn/objectfs/internal/storage/s3"

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

There is none. The fallback is one-way for the life of the mount: once an acceleration error is
seen, every later request uses the standard endpoint until ObjectFS restarts. This section used to
promise an "automatic re-enable after successful standard operations" and a manual
`backend.GetClientManager().EnableAcceleration()`; nothing calls the re-enable path, and `Backend`
has no `GetClientManager` accessor to reach it with.

That is a deliberate trade rather than an oversight worth working around. The error that triggers
fallback — a bucket without the Transfer Acceleration configuration — does not resolve on its own,
so retrying it would mean paying a failed round-trip per request forever. Restart after fixing the
bucket configuration.

### Metrics Tracking

Monitor acceleration performance with built-in metrics:

```go
metrics := backend.GetMetrics()

fmt.Printf("Acceleration Status: %v\n", metrics.AccelerationEnabled)
fmt.Printf("Accelerated Requests: %d\n", metrics.AcceleratedRequests)
fmt.Printf("Accelerated Bytes: %d\n", metrics.AcceleratedBytes)
fmt.Printf("Fallback Events: %d\n", metrics.FallbackEvents)

// AccelerationEnabled reports configuration, not effect. A mount can have it true, have fallen
// back on its first request, and be serving everything over the standard endpoint since — so
// derive the rate from the counters rather than reading the flag.
if metrics.Requests > 0 {
    fmt.Printf("Acceleration Rate: %.2f%%\n",
        float64(metrics.AcceleratedRequests)/float64(metrics.Requests)*100)
}
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
go test -bench='BenchmarkGetObject_Large_(Standard|Accelerated)' \
    ./internal/storage/s3/
```

These benchmarks need a real bucket: without `OBJECTFS_BENCH_BUCKET` they skip, and a skipped
benchmark reports nothing rather than reporting zero.

No sample output is shown here on purpose. This section used to print four lines that looked like
`go test -bench` output and were not — they had no `ns/op` column, which every real benchmark line
has, because they were written by hand to illustrate a hoped-for ratio. Acceleration's benefit
depends on your distance from the bucket region and on object size; a number measured from someone
else's network is not evidence about yours. Run the two benchmarks above against your own bucket
and compare with `benchstat`. That takes a few minutes and answers the question for your
deployment, which an invented figure cannot do at any length.

## Monitoring

### Health Checks

ObjectFS tracks acceleration health:

```go
m := backend.GetMetrics()

// Whether the accelerate endpoint is configured at all.
if m.AccelerationEnabled {
    log.Info("S3 Transfer Acceleration is enabled")
}

// What share of requests actually took it. FallbackEvents counts the ones that did not:
// acceleration can be enabled and still be delivering nothing, if the bucket lacks the
// Transfer Acceleration configuration or the endpoint is unreachable from here.
if m.Requests > 0 {
    rate := float64(m.AcceleratedRequests) / float64(m.Requests) * 100
    if rate < 50.0 {
        log.Warn("low acceleration rate", "percent", rate, "fallbacks", m.FallbackEvents)
    }
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
