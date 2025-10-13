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
		Port:      8080,
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
  - objectfs_cache_requests_total{type,source}: Cache hits/misses by level
  - objectfs_errors_total{operation,type}: Errors by operation and classification

Histograms:
  - objectfs_operation_duration_seconds{operation}: Operation latency distribution
  - objectfs_operation_size_bytes{operation}: Operation size distribution

Gauges:
  - objectfs_cache_size_bytes{level}: Current cache size per level
  - objectfs_active_connections: Current active S3 connections

# HTTP Endpoints

The metrics server exposes several endpoints:

/metrics - Prometheus-formatted metrics (for scraping)

	curl http://localhost:8080/metrics

/health - Health check endpoint

	curl http://localhost:8080/health
	{"status":"healthy","service":"objectfs-metrics"}

/debug/metrics - Human-readable metrics summary

	curl http://localhost:8080/debug/metrics
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

	curl http://localhost:8080/debug/operations
	Operation            Count     Errors   Avg Duration      Avg Size
	----------           -----     ------   ------------      --------
	read                 15234         12         45ms        524288
	write                 8901          3         89ms       1048576

# Configuration

The Config struct controls metrics behavior:

	config := &metrics.Config{
		Enabled:        true,              // Enable/disable metrics collection
		Port:           8080,              // HTTP server port
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
	      - targets: ['localhost:8080']
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

		"github.com/objectfs/objectfs/internal/metrics"
	)

	func main() {
		// Create metrics collector
		collector, err := metrics.NewCollector(&metrics.Config{
			Enabled:   true,
			Port:      8080,
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

# See Also

- internal/health: Health monitoring and alerting
- internal/circuit: Circuit breaker for reliability
- pkg/errors: Structured error handling

For more information on Prometheus metrics and best practices, see:
https://prometheus.io/docs/practices/naming/
*/
package metrics
