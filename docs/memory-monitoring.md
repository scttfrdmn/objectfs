# Memory Monitoring and Leak Detection

ObjectFS includes comprehensive memory monitoring and leak detection capabilities to ensure reliable long-running operation.

## Overview

The `pkg/memmon` package provides:
- **Real-time memory monitoring** with configurable sampling
- **Automatic leak detection** for memory growth, goroutine leaks, GC pressure, and heap fragmentation
- **Object tracking** for tracking allocations of specific types
- **Memory profiling** for detailed analysis
- **Alert system** for proactive issue detection

## Quick Start

```go
import "github.com/objectfs/objectfs/pkg/memmon"

// Create monitor with default configuration
config := memmon.DefaultMonitorConfig()
monitor := memmon.NewMemoryMonitor(config)

// Start monitoring
ctx := context.Background()
if err := monitor.Start(ctx); err != nil {
    log.Fatal(err)
}
defer monitor.Stop()

// Get current memory stats
stats := monitor.GetStats()
fmt.Printf("Current memory: %d bytes\n", stats.CurrentSample.Alloc)
fmt.Printf("Growth since baseline: %.2f%%\n", stats.GrowthSinceBaseline)

// Check for alerts
alerts := monitor.GetAlerts()
for _, alert := range alerts {
    fmt.Printf("Alert: %s - %s\n", alert.AlertType.String(), alert.Message)
}
```

## Configuration

```go
config := memmon.MonitorConfig{
    SampleInterval:   30 * time.Second,  // How often to sample
    AlertThreshold:   20.0,                // Alert on 20% growth
    MaxSamples:       100,                 // Keep last 100 samples
    EnableGCStats:    true,                // Track GC statistics
    EnableStackTrace: false,               // Disable stack traces
    GCPercentage:     100,                 // GOGC setting
}
```

## Object Tracking

Track specific object types for leak detection:

```go
// Register object type with threshold
monitor.TrackObject("cache-entries", 10000)

// Increment when objects are created
monitor.IncrementObject("cache-entries", sizeInBytes)

// Decrement when objects are freed
monitor.DecrementObject("cache-entries", sizeInBytes)

// Get tracked objects
objects := monitor.GetTrackedObjects()
for name, obj := range objects {
    fmt.Printf("%s: %d objects, %d bytes\n", name, obj.Count, obj.Size)
}
```

## Memory Profiling

Use the profiler for detailed analysis:

```go
profiler := memmon.NewProfiler("/tmp/profiles")

// Write heap profile
profiler.WriteHeapProfile("heap.prof")

// Write all profiles (heap, goroutine, block, mutex)
profiler.WriteAllProfiles("myapp")

// Profile memory over time
samples, err := profiler.ProfileMemoryUsage(5*time.Minute, 10*time.Second)

// Detect leaks from samples
detections := profiler.DetectLeaks(samples, 20.0)
for _, detection := range detections {
    fmt.Printf("Leak detected: %s\n", detection.Description)
}
```

## Alert Types

The monitor detects four types of issues:

1. **Memory Growth**: Sustained increase in allocated memory
2. **Goroutine Leaks**: Growing number of goroutines
3. **GC Pressure**: GC consuming excessive CPU time (>5%)
4. **Heap Fragmentation**: Excessive idle heap space (>50%)

## Integration with ObjectFS Components

### Cache Monitoring

```go
// LRU Cache automatically cleans up properly now
cache := cache.NewLRUCache(config)
defer cache.Close()  // Ensures cleanup goroutine stops

// Monitor cache memory usage
monitor.TrackObject("cache-items", 100000)
```

### Write Buffer Monitoring

```go
// Write buffer tracks pending writes
wb, err := buffer.NewWriteBuffer(config, flushCallback)
defer wb.Close()  // Flushes and stops goroutines

// Track buffer usage
monitor.TrackObject("write-buffers", 1000)
```

## Best Practices

1. **Start monitoring early** in application lifecycle
2. **Set appropriate thresholds** based on workload
3. **Track key object types** that dominate memory usage
4. **Reset baseline** after warmup period
5. **Review alerts regularly** and investigate patterns
6. **Profile periodically** in production (with caution)
7. **Force GC** before taking critical measurements

## Memory Leak Fixes Applied

### LRU Cache (internal/cache/lru.go)

**Issue**: The `Clear()` and `removeItem()` methods didn't explicitly nil out data slices and list elements.

**Fix**:
- Explicitly set `item.data = nil` and `item.element = nil`
- Properly track evictions count before clearing
- Iterate and delete items individually to help GC

### Write Buffer (internal/buffer/writebuffer.go)

**Status**: Already properly implemented with:
- Goroutine cleanup on Close()
- Proper flush on shutdown
- Channel closure for signal propagation

### Byte Pool (internal/buffer/pool.go)

**Status**: Already optimal with:
- Buffer zeroing before return to pool
- Proper capacity-based pooling
- sync.Pool for automatic memory management

## Analyzing Profiles

Use Go's pprof tool to analyze collected profiles:

```bash
# Analyze heap profile
go tool pprof heap.prof

# Compare two profiles
go tool pprof -base=before.prof after.prof

# Generate visual graph
go tool pprof -png heap.prof > heap.png
```

## Performance Impact

Memory monitoring has minimal overhead:
- **Sampling**: < 1ms per sample
- **Object tracking**: ~100ns per increment/decrement
- **Profiling**: Only when explicitly requested

## Example: Production Deployment

```go
// Production configuration
config := memmon.MonitorConfig{
    SampleInterval:   60 * time.Second,   // Less frequent in prod
    AlertThreshold:   30.0,                // Higher threshold
    MaxSamples:       1000,                // More history
    EnableGCStats:    true,
    GCPercentage:     100,
}

monitor := memmon.NewMemoryMonitor(config)
monitor.Start(context.Background())

// Track critical components
monitor.TrackObject("cache-entries", 1000000)
monitor.TrackObject("active-connections", 10000)
monitor.TrackObject("pending-writes", 50000)

// Periodic profiling (e.g., every hour)
go func() {
    ticker := time.NewTicker(1 * time.Hour)
    defer ticker.Stop()

    profiler := memmon.NewProfiler("/var/log/objectfs/profiles")

    for range ticker.C {
        timestamp := time.Now().Format("20060102-150405")
        profiler.WriteAllProfiles(fmt.Sprintf("objectfs-%s", timestamp))
    }
}()
```

## Troubleshooting

### High Memory Usage

1. Check `GetStats()` for growth patterns
2. Review alerts for specific issues
3. Write heap profile and analyze with pprof
4. Check tracked objects for unexpected counts
5. Force GC and measure if memory is released

### Goroutine Leaks

1. Monitor goroutine count over time
2. Write goroutine profile
3. Look for goroutines stuck on channel operations
4. Verify all goroutines have proper shutdown signals
5. Check for missing `defer` cleanup calls

### GC Pressure

1. Check `GCCPUFraction` in stats
2. Review `NumGC` growth rate
3. Consider increasing `GOGC` percentage
4. Reduce allocation rate if possible
5. Profile to find allocation hotspots

## API Reference

See [pkg/memmon](../pkg/memmon/) for complete API documentation.

## Related Documentation

- [Error Handling and Recovery](./error-handling-recovery.md)
- [Performance Tuning](./performance-tuning.md)
- [Operations Guide](./operations.md)
