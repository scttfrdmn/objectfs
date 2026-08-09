/*
Package metrics provides comprehensive metrics collection and monitoring for ObjectFS.

# Overview

The metrics package implements Prometheus-based metrics collection for ObjectFS operations,
cache performance, errors, and system resources. It provides both real-time Prometheus
metrics and historical tracking for debugging and analysis.

Architecture

	┌─────────────┐
	│  Collector  │  ← Main metrics aggregator
	└──────┬──────┘
	       │
	   ┌───┴────────────────────────────┐
	   │                                │
	┌──▼───────────┐         ┌─────────▼──────┐
	│  Prometheus  │         │  HTTP Endpoints │
	│   Registry   │         │  /metrics       │
	│              │         │  /health        │
	│ - Counters   │         │  /debug/metrics │
	│ - Histograms │         └─────────────────┘
	│ - Gauges     │
	└──────────────┘

# Core Components

Collector: The main metrics collector that aggregates and exports metrics.
It maintains both Prometheus metrics (for monitoring systems) and internal
operation tracking (for debugging).

	collector, err := metrics.NewCollector(&metrics.Config{
		Enabled:   true,
		Addr:      "127.0.0.1:8080",
		Path:      "/metrics",
		Namespace: "objectfs",
	})
	if err != nil {
		log.Fatal(err)
	}

	if err := collector.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer collector.Stop(ctx)

# Recording Operations

The collector tracks operations with timing, size, and success/failure status:

	startTime := time.Now()
	data, err := performOperation()
	duration := time.Since(startTime)

	collector.RecordOperation("read", duration, int64(len(data)), err == nil)

# Cache Metrics

Track cache hit rates and sizes across different cache levels:

	// Cache hit
	collector.RecordCacheHit("data/file.txt", 4096)

	// Cache miss
	collector.RecordCacheMiss("data/file.txt", 4096)

	// Update cache size (periodically)
	collector.UpdateCacheSize("L1", currentL1Size)
	collector.UpdateCacheSize("L2", currentL2Size)

# Error Tracking

Record and classify errors for monitoring and alerting:

	if err != nil {
		collector.RecordError("s3_upload", err)
		return err
	}

# Prometheus Metrics

The collector exports standard Prometheus metrics:

Counters:
  - objectfs_operations_total{operation,status}: Total operations by type and status
  - objectfs_cache_requests_total{type}: Cache requests, by hit or miss
  - objectfs_errors_total{operation,type}: Errors by operation and classification

Histograms:
  - objectfs_operation_duration_seconds{operation}: Operation latency distribution
  - objectfs_operation_size_bytes{operation}: Operation size distribution

Gauges:
  - objectfs_cache_size_bytes{level}: Current cache size per level
  - objectfs_active_connections: Current active S3 connections
  - objectfs_predictive_cache{statistic}: Read-ahead statistics, by statistic name
  - objectfs_s3_acceleration{statistic}: Transfer Acceleration state and counters, by statistic name
  - objectfs_s3_cost{statistic,region,tier}: Billable requests, bytes, dollars, and the rates each
    figure was computed at

The last three are one family each with a statistic label rather than a metric per statistic, because
all three sets grow: a new label value is something a dashboard and both SDKs pick up for free, where a
new name is a change every consumer has to follow. Their values are a snapshot the adapter re-reads
on each update tick, so they are gauges even where the underlying number is a running total —
objectfs_s3_acceleration{statistic="fallbacks"} is set to the backend's count, not incremented.

objectfs_s3_acceleration carries statistic="configured" and statistic="active" separately, and the
difference between them is the point (#204): configured 1 with active 0 is a mount that was asked to
accelerate and is not, which before this family existed was reported nowhere. Alert on the pair, not
on either alone.

objectfs_s3_cost carries two labels beyond statistic, and both are required to read it (#226). region
is the region the rates were resolved for, which is not always the configured one — an unpublished
region falls back to us-east-1 — and tier is the storage class every rate below depends on, which
differs tenfold between STANDARD and DEEP_ARCHIVE for a write. Summing the family across mounts without
grouping by both adds figures computed at different prices. The rate_per_* statistics are published
alongside the dollar figures so a dashboard can show the arithmetic rather than only its result: #209
was a per-1,000 price stored as if it were per-request, and a scrape that carries the rate makes that
class of error visible without waiting for a bill. Every figure is monotonic and never reset, because
rate-of-change is the form a useful alert on cost takes and a counter that can decrease cannot support
one. See internal/storage/s3.Backend.CostStats for what the numbers are and are not — they are this
process's spend since it started, not a reconciliation of an invoice.

Every series additionally carries the operator's monitoring.metrics.custom_labels as constant
labels — service="objectfs" by default. Consumers must parse the label block: a name identifies a
family, not a series, so objectfs_cache_requests_total is two samples and not one.

These names are the contract with the Python and TypeScript SDKs, captured as a real scrape in
sdks/testdata/metrics-scrape.txt. TestSDKFixtureMatchesTheLiveScrape regenerates and compares it,
so renaming a metric or dropping a label here fails this package's tests and then the SDK suites.
Regenerate with:

	go test ./internal/metrics/ -run TestSDKFixtureMatchesTheLiveScrape -update-fixture

# HTTP Endpoints

The metrics server exposes several endpoints. None of them is authenticated, which is why Addr
defaults to loopback and why Start binds exactly what it is given: through v0.10.x this was a Port
and the bind was fmt.Sprintf(":%d"), so every configured value published these to every interface
(#211). Start also binds synchronously and returns the bind error, rather than logging it on a
goroutine and leaving the mount running with no endpoint.

Curl the address the collector reports from Addr, not a hardcoded one — Config.Addr may name port 0,
in which case the kernel chose the port.

/metrics - Prometheus-formatted metrics (for scraping)

	curl http://127.0.0.1:8080/metrics

/health - Health check endpoint

	curl http://127.0.0.1:8080/health
	{"status":"healthy","service":"objectfs-metrics"}

/debug/metrics - Human-readable metrics summary

	curl http://127.0.0.1:8080/debug/metrics
	{
	  "uptime": "2h15m30s",
	  "operations": {
	    "read": {
	      "count": 15234,
	      "errors": 12,
	      "avg_duration": "45ms",
	      "avg_size": 524288.00
	    }
	  }
	}

/debug/operations - Tabular operations summary

	curl http://127.0.0.1:8080/debug/operations
	Operation            Count     Errors   Avg Duration      Avg Size
	----------           -----     ------   ------------      --------
	read                 15234         12         45ms        524288
	write                 8901          3         89ms       1048576

# Configuration

The Config struct controls metrics behavior:

	config := &metrics.Config{
		Enabled:        true,              // Enable/disable metrics collection
		Addr:           "127.0.0.1:8080",  // listener address; Start returns an error if it cannot bind
		Path:           "/metrics",        // Prometheus metrics endpoint path
		Namespace:      "objectfs",        // Prometheus namespace
		Subsystem:      "",                // Optional subsystem prefix
		UpdateInterval: 30 * time.Second,  // Periodic update interval
		Labels:         map[string]string{ // Custom labels for all metrics
			"env":     "production",
			"region":  "us-east-1",
			"version": "v0.2.0",
		},
	}

# Best Practices

1. Operation Recording
Record all significant operations (reads, writes, deletes, etc.) with accurate
timing and size information. Use consistent operation names across the codebase.

2. Cache Metrics
Update cache metrics regularly to provide accurate size and hit rate data.
Consider recording cache metrics after each cache operation or on a timer.

3. Error Classification
Record all errors with meaningful operation context. The collector automatically
classifies errors (timeout, connection, not_found, permission, throttling) for
better monitoring and alerting.

4. Resource Limits
Be mindful of metric cardinality. Avoid high-cardinality labels (like user IDs
or file paths) that can explode the metric count and impact Prometheus performance.

5. Debugging
Use the /debug/* endpoints for troubleshooting without requiring Prometheus.
These endpoints provide human-readable summaries of current system state.

# Performance Considerations

The metrics collector is designed for high-throughput environments:

- Lock-free reads for hot path operations
- Buffered updates to Prometheus
- Minimal allocation in recording path
- Configurable update intervals
- Optional metrics disabling for maximum performance

# Thread Safety

All Collector methods are thread-safe and can be called concurrently from
multiple goroutines. The collector uses RWMutex for efficient concurrent access.

# Integration with Monitoring Systems

Prometheus Setup:

	scrape_configs:
	  - job_name: 'objectfs'
	    static_configs:
	      - targets: ['127.0.0.1:8080']
	    metrics_path: '/metrics'
	    scrape_interval: 15s

Grafana Dashboards:

The exported metrics are compatible with standard Grafana dashboards for:
- RED metrics (Rate, Errors, Duration)
- Cache performance analysis
- Resource utilization trending
- Error rate alerting

# Example Usage

Complete example of metrics integration:

	package main

	import (
		"context"
		"log"
		"time"

		"github.com/scttfrdmn/objectfs/internal/metrics"
	)

	func main() {
		// Create metrics collector
		collector, err := metrics.NewCollector(&metrics.Config{
			Enabled:   true,
			Addr:      "127.0.0.1:8080",
			Namespace: "objectfs",
			Labels: map[string]string{
				"instance": "primary",
			},
		})
		if err != nil {
			log.Fatal(err)
		}

		// Start metrics server
		ctx := context.Background()
		if err := collector.Start(ctx); err != nil {
			log.Fatal(err)
		}
		defer collector.Stop(ctx)

		// Record operations
		for {
			start := time.Now()
			err := performWork()
			duration := time.Since(start)

			collector.RecordOperation("work", duration, 1024, err == nil)
			if err != nil {
				collector.RecordError("work", err)
			}

			time.Sleep(time.Second)
		}
	}

	func performWork() error {
		// Your operation here
		return nil
	}

# DetailedPerformanceMetrics is not wired up

detailed.go holds a second, richer collector — per-operation latency, per-file access counts, a
cache-source breakdown, and network utilization. NewDetailedPerformanceMetrics has no caller outside
this package's own tests, so nothing a mount does reaches any of it. It is here because it may be
wired up, not because it runs.

It also held per-operation cost, and that half was **deleted** rather than wired, for #226. A caller
would have had to supply the rates as float64 dollars, so every price would have been decided by the
call site instead of by internal/awsrates — and two of its calculations were wrong in ways no amount of
wiring would have fixed: cost per GB divided by 1 << 30 where AWS bills decimal GB, and a monthly
estimate extrapolated from process uptime, which reports a mount's first minute as the whole month's
rate. objectfs_s3_cost is the replacement, and it counts in the SDK's response path where the count
cannot disagree with what S3 received. The one thing it does not carry is cost per operation type; that
belongs as a label on the tally, not as a second collector.

Its latency percentiles are estimates, which is worth knowing before reading them as measurements.
P50Latency, P95Latency and P99Latency are computed from LatencyHistogram by interpolating within the
bucket that covers the requested rank, as Prometheus's histogram_quantile does, so each is accurate
only to the width of that bucket — LatencyBucketBounds says what the widths are. A rank landing in the
overflow bucket saturates at the top bound, since there is nothing above it to interpolate toward.
The per-file copies in FileOperationMetrics.Operations allocate no histogram and leave all three at
zero; the aggregate OperationMetrics is the only place percentiles are populated.

Both of those were defects until issue 222. The fields were declared and never assigned, so anything
serializing the struct published zeros as percentiles — which reads as a filesystem with no tail
latency rather than as an unimplemented field — and LatencyHistogram was indexed by
int(latency.Milliseconds()) % 100, a modulo rather than a bucketing, so 1 ms and 101 ms and 201 ms
shared a bucket and no percentile could have been computed from it. latency.go holds the replacement
and latency_test.go asserts the two collisions the modulo produced.

A 618-line docs/performance-metrics.md described this subsystem, with a zero-argument constructor,
a NewDetailedPerformanceMetricsWithOptions that never existed, three getters that do not exist,
OpMkdir/OpRmdir/OpStatfs against the real OpMkDir/OpRmDir/OpStatFS, a CacheSourceNone that is not
declared, and a four-argument RecordNetworkOperation. It was deleted rather than corrected: nine
defects in one document about one file is what a description maintained separately from the thing it
describes converges to. This section is short so that it can stay true.

# See Also

- internal/health: Health monitoring and alerting
- internal/circuit: Circuit breaker for reliability
- pkg/errors: Structured error handling

For more information on Prometheus metrics and best practices, see:
https://prometheus.io/docs/practices/naming/
*/
package metrics
