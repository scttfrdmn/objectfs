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

S3 has no acceleration-specific error code — it reports every one of these as `InvalidRequest` and
distinguishes them only by the message — so the classifier is a conjunction of the two. An
`InvalidRequest` for an unrelated reason, such as `Invalid Range header` or an oversized single-part
copy, is *not* an acceleration error and must not withdraw the endpoint.

- `InvalidRequest` whose message names transfer acceleration or transfer accelerate: not configured
  on the bucket, disabled, unsupported on the bucket, or a bucket name that is not DNS-compliant
- Any error whose text names the `s3-accelerate` endpoint, such as a DNS failure resolving it. These
  carry no API error at all, so the code half of the conjunction cannot see them
- The AWS SDK's refusal to combine acceleration with a custom endpoint (see below)

**Fallback Behavior:**

1. Attempt operation with accelerated endpoint
2. Detect acceleration error
3. Log fallback event and increment `objectfs_s3_acceleration{statistic="fallbacks"}`
4. Retry operation with standard endpoint
5. Withdraw the accelerate endpoint until the retry period elapses

**Re-enabling:**

One request is allowed to try the accelerate endpoint again after `storage.s3.acceleration_retry`
(default 5 minutes). If it succeeds, acceleration resumes for everything; if it fails, the endpoint
is withdrawn for another period. Exactly one probe is in flight at a time, so a permanently broken
endpoint costs one failed request per period rather than one per read.

The mechanism is `internal/circuit`'s breaker, not a bespoke timer: withdrawn is its open state,
probing is half-open with `MaxRequests: 1`. Reusing it is why the single-probe bound holds under
concurrent load, which a mutex around two fields cannot express.

Through v0.12.x there was no way back. The fallback was one-way for the life of the mount, so a
thirty-second DNS failure reaching the accelerate endpoint cost a long-lived mount its acceleration
until restart, and nothing reported that it had happened (#204). The trade that justified it — that a
bucket without the acceleration configuration does not fix itself, so retrying forever costs a failed
round-trip per request — is real, and the retry period is the answer to it: the cost of a permanent
failure is bounded by the period rather than by the request rate.

**Acceleration and a custom endpoint are mutually exclusive.**

The AWS SDK refuses the pair outright — `A custom endpoint cannot be combined with S3 Accelerate` —
before any request leaves the process. So `use_acceleration: true` together with `endpoint:` set for
MinIO, Ceph, RustFS or Wasabi means every request attempts the accelerate client, is refused locally,
and is served by the standard endpoint instead. Reads and writes work; the acceleration does nothing.

That is a silent degradation on purpose, because acceleration is a performance capability and slower
is a correct outcome (see the thesis in `CLAUDE.md`). It is also why the classifier has to match this
particular error: unmatched, the refusal is not recognized as an acceleration problem and every GET
and PUT fails permanently. Note that the SDK's refusal is not a `smithy.APIError` and the backend
wraps it in an `*errors.ObjectFSError`, whose `Error()` deliberately omits its cause — so the
classifier walks the whole `Unwrap` chain rather than matching the top-level string.

### Metrics Tracking

A mount publishes acceleration state to its Prometheus endpoint as one gauge family,
`objectfs_s3_acceleration`, with a `statistic` label:

```text
objectfs_s3_acceleration{statistic="configured"} 1
objectfs_s3_acceleration{statistic="active"} 0
objectfs_s3_acceleration{statistic="requests"} 412
objectfs_s3_acceleration{statistic="bytes"} 1687552
objectfs_s3_acceleration{statistic="fallbacks"} 1
objectfs_s3_acceleration{statistic="avg_latency_seconds"} 0.0184
objectfs_s3_acceleration{statistic="retry_period_seconds"} 300
```

`configured` against `active` is the pair to alert on, and the scrape above is the state worth
alerting on: this mount was asked to accelerate and is not. Neither series alone can say that, which
is why both are exported. `configured 0` is present rather than absent for the same reason — an absent
family means "this build does not report acceleration", which is a different fact from "this mount was
not asked to accelerate".

The values are a snapshot the adapter re-reads from the backend on each collector tick, so they are
gauges even where the underlying number is a running total. `fallbacks` is *set* to the backend's
count, not incremented, and a fallback followed by a recovery shows as `active` returning to 1 with
`fallbacks` staying where it is.

In-process, the same numbers are available from the backend:

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

### There is no second upload path to fall back from

Until v0.15.0 this section described a three-level upload priority, with CargoShip's transporter
first and acceleration as its fallback. That path was removed in #362 — it could not carry a
`Content-Encoding`, the configured encryption headers, a per-object storage class, or a
`Content-Type`, and it was unreachable above `multipart.threshold` in the first place.

**Upload path:**

```
PutObject Request
    ↓
Accelerated Endpoint (if use_accelerate)
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

```text
INFO  S3 Transfer Acceleration enabled    bucket=my-bucket endpoint=""
WARN  S3 Transfer Acceleration error detected, falling back to standard endpoint
      operation=GetObject retry_after=5m0s error="..."
WARN  Disabling S3 Transfer Acceleration    reason="..."
INFO  Retrying S3 Transfer Acceleration after backoff    breaker=s3-acceleration
INFO  Re-enabling S3 Transfer Acceleration
INFO  S3 Transfer Acceleration restored    breaker=s3-acceleration
```

The last three are the recovery, and they are the sequence to look for when deciding whether a
fallback was transient. "Retrying ... after backoff" without a following "restored" means the probe
failed and the endpoint was withdrawn for another period.

### Metrics Integration

Nothing to write. A mount with `monitoring.metrics.enabled: true` exports
`objectfs_s3_acceleration` on its own endpoint — see "Metrics Tracking" above for the series — and
re-reads the backend on every collector tick. This section used to show two hand-rolled Prometheus
collectors, which is what an operator had to do when the state was reachable only from inside the
`s3` package.

An alert on the state that matters:

```yaml
- alert: ObjectFSAccelerationFallenBack
  expr: |
    objectfs_s3_acceleration{statistic="configured"} == 1
      unless on(instance)
    objectfs_s3_acceleration{statistic="active"} == 1
  for: 15m
  annotations:
    summary: "{{ $labels.instance }} was configured to accelerate and is not"
```

`for: 15m` rather than a shorter window because a single fallback is expected to resolve on its own
after `storage.s3.acceleration_retry` — the default is 5 minutes, so alert on longer than a couple of
retry periods, not on the first one.

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

## API Reference

### Configuration Options

```go
type Config struct {
    // Enable S3 Transfer Acceleration
    UseAccelerate bool `yaml:"use_accelerate"`

    // How long a fallback lasts before one request may try the accelerate endpoint
    // again. Zero takes the 5-minute default.
    AccelerationRetry time.Duration `yaml:"acceleration_retry"`

    // Other S3 settings
    Region         string        `yaml:"region"`
    PoolSize       int           `yaml:"pool_size"`
    RequestTimeout time.Duration `yaml:"request_timeout"`
}
```

The operator-facing keys are `storage.s3.use_acceleration` and `storage.s3.acceleration_retry`; the
struct above is what the adapter maps them onto.

### Client Manager Methods

```go
// Check acceleration status
IsAccelerationActive() bool

// Withdraw and restore the endpoint. Both are the gate's to call, not a caller's:
// the gate decides when, and the retry has to be bounded to one in-flight probe.
EnableAcceleration()
DisableAcceleration(reason string)

// Get acceleration-aware clients
GetAcceleratedClient() *s3.Client
GetStandardClient() *s3.Client
```

### Metrics Methods

`AccelerationStats` is what an exporter should read. It answers "is acceleration in effect" —
`BackendMetrics.AccelerationEnabled` answers only "was it configured", which is why a mount that had
fallen back reported acceleration enabled for the life of the process.

```go
type AccelerationStats struct {
    Configured  bool          // what the operator asked for
    Active      bool          // whether requests are taking the accelerate endpoint now
    GateState   circuit.State // closed, open (withdrawn), or half-open (probing)
    Requests    int64
    Bytes       int64
    Fallbacks   int64
    AvgLatency  time.Duration
    RetryPeriod time.Duration
}

stats := backend.AccelerationStats()

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

- [Configuration reference](./index.md) — every key the loader accepts, including the S3 block
- [Multipart uploads](./features/multipart-uploads.md) — the other half of the large-object path
- [Benchmarks](https://github.com/scttfrdmn/objectfs/tree/main/benchmarks) — runnable, which is
  what a tuning page would have to cite

`storage-backends.md`, `performance-tuning.md`, `monitoring.md`, and `cargoship.md` were linked
here and none exists. `performance-tuning.md` in particular was linked from four different pages
without ever being written — see [issue 208](https://github.com/scttfrdmn/objectfs/issues/208),
where the decision recorded is that if it is written it must cite measurements, since a tuning page
inventing numbers is the defect #182 was about.

## Additional Resources

- [AWS S3 Transfer Acceleration](https://aws.amazon.com/s3/transfer-acceleration/)
- [S3 Transfer Acceleration Speed Comparison](https://s3-accelerate-speedtest.s3-accelerate.amazonaws.com/en/accelerate-speed-comparsion.html)
- [AWS Pricing - Transfer Acceleration](https://aws.amazon.com/s3/pricing/)
