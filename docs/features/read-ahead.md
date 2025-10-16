# Advanced Read-ahead and Predictive Caching

ObjectFS provides sophisticated read-ahead capabilities with ML-based predictive caching to
optimize read performance for sequential and predictable access patterns.

## Overview

The read-ahead system uses multiple strategies to anticipate data access:

- **Simple read-ahead**: Fixed-size buffer pre-loading
- **Predictive read-ahead**: Pattern detection for sequential access
- **ML-based prediction**: Machine learning for complex access patterns

The system automatically detects access patterns, prefetches data intelligently, and tracks
effectiveness through comprehensive metrics.

## Features

### 1. Multiple Read-ahead Strategies

ObjectFS supports three read-ahead strategies, each optimized for different workloads:

#### Simple Strategy

Basic read-ahead with fixed buffer size. Best for:

- Development and testing
- Workloads with unpredictable access patterns
- Resource-constrained environments

#### Predictive Strategy (Default)

Pattern detection and intelligent prefetching. Best for:

- Sequential file access (log processing, video streaming)
- Batch workloads with predictable patterns
- Production deployments

#### ML Strategy (Advanced)

Machine learning-based access prediction. Best for:

- Complex, multi-pattern workloads
- Large-scale data processing
- Environments with training data available

### 2. Pattern Detection

The predictive cache analyzes access patterns to identify:

- **Sequential access**: Reading consecutive blocks of data
- **Temporal patterns**: Regular time-based access patterns
- **Size patterns**: Consistent request sizes
- **Frequency patterns**: Hot and cold data identification

**Configuration:**

```yaml
performance:
  read_ahead:
    enable_pattern_detection: true
    sequential_threshold: 0.7        # Confidence level (0-1)
    prediction_window: 100           # Number of accesses to analyze
    pattern_depth: 1000              # Historical analysis depth
```

### 3. Intelligent Prefetching

When patterns are detected with sufficient confidence, the system prefetches data:

- **Concurrent prefetching**: Multiple parallel prefetch operations
- **Bandwidth control**: Rate limiting to avoid overwhelming the network
- **Priority-based**: Higher confidence predictions prefetched first
- **Waste avoidance**: Tracks prefetch effectiveness to minimize wasted bandwidth

**Configuration:**

```yaml
performance:
  read_ahead:
    enable_prefetch: true
    max_concurrent_fetch: 4          # Parallel prefetch operations
    prefetch_ahead: 3                # Number of blocks to prefetch
    prefetch_bandwidth_mbs: 10       # Max bandwidth (MB/s)
    confidence_threshold: 0.7        # Min confidence to trigger (0-1)
```

### 4. ML-Based Prediction

For advanced workloads, ObjectFS can use machine learning to predict future access:

- **Online learning**: Continuously improves predictions during operation
- **Feature extraction**: Analyzes multiple access characteristics
- **Gradient descent optimization**: Adaptive learning rate
- **Model persistence**: Save and load trained models

**Configuration:**

```yaml
performance:
  read_ahead:
    enable_ml_prediction: true
    ml_model_path: "/path/to/model.bin"  # Optional: pre-trained model
    learning_rate: 0.01                   # Learning rate (0-1)
```

### 5. Metrics and Monitoring

Track read-ahead effectiveness with comprehensive metrics:

```go
// Prediction metrics
PredictionsTotal   uint64
PredictionsCorrect uint64
PredictionAccuracy float64
AvgConfidence      float64

// Prefetch metrics
PrefetchRequests   uint64
PrefetchHits       uint64
PrefetchWaste      uint64
PrefetchEfficiency float64

// Performance impact
CacheHitImprovement float64
LatencyReduction    float64
BandwidthSavings    float64
```

## Configuration Reference

### Basic Settings

```yaml
performance:
  read_ahead:
    # Enable/disable read-ahead entirely
    enabled: true

    # Read-ahead buffer size
    size: "64MB"

    # Strategy: "simple", "predictive", or "ml"
    strategy: "predictive"

    # Minimum size for sequential detection
    sequential_min_size: "1MB"
```

### Pattern Detection Settings

```yaml
performance:
  read_ahead:
    # Enable pattern detection
    enable_pattern_detection: true

    # Sequential confidence threshold (0-1)
    # Higher = more conservative, requires clearer patterns
    sequential_threshold: 0.7

    # Number of recent accesses to analyze
    prediction_window: 100
```

### Prefetch Settings

```yaml
performance:
  read_ahead:
    # Enable intelligent prefetching
    enable_prefetch: true

    # Maximum parallel prefetch operations
    max_concurrent_fetch: 4

    # Number of blocks to prefetch ahead
    prefetch_ahead: 3

    # Maximum prefetch bandwidth (MB/s)
    # 0 = unlimited
    prefetch_bandwidth_mbs: 10

    # Minimum confidence to trigger prefetch (0-1)
    confidence_threshold: 0.7
```

### ML Prediction Settings

```yaml
performance:
  read_ahead:
    # Enable ML-based prediction
    enable_ml_prediction: false

    # Path to trained ML model (required if ML enabled)
    ml_model_path: ""

    # Model learning rate (0-1)
    learning_rate: 0.01

    # Pattern analysis depth
    pattern_depth: 1000
```

### Monitoring Settings

```yaml
performance:
  read_ahead:
    # Enable metrics collection
    metrics_enabled: true

    # Statistics collection interval
    statistics_interval: "30s"

    # ML model update frequency
    model_update_interval: "5m"
```

## Configuration Examples

### High-Performance Sequential Access

Optimized for video streaming, log processing, and sequential data access:

```yaml
performance:
  read_ahead:
    enabled: true
    size: "128MB"                    # Larger buffer
    strategy: "predictive"
    sequential_min_size: "512KB"
    enable_pattern_detection: true
    sequential_threshold: 0.6        # More aggressive
    prediction_window: 50            # Shorter window for faster detection
    enable_prefetch: true
    max_concurrent_fetch: 8          # More parallelism
    prefetch_ahead: 5                # Fetch further ahead
    prefetch_bandwidth_mbs: 50       # Higher bandwidth
    confidence_threshold: 0.6
```

### Conservative/Low-Bandwidth Configuration

For limited bandwidth or unpredictable workloads:

```yaml
performance:
  read_ahead:
    enabled: true
    size: "32MB"                     # Smaller buffer
    strategy: "simple"               # No pattern detection
    enable_pattern_detection: false
    enable_prefetch: false           # Disable prefetching
    metrics_enabled: true            # Still collect metrics
```

### ML-Optimized Configuration

For complex workloads with training data available:

```yaml
performance:
  read_ahead:
    enabled: true
    size: "64MB"
    strategy: "ml"
    enable_pattern_detection: true
    sequential_threshold: 0.7
    prediction_window: 200           # Larger window for ML
    enable_prefetch: true
    max_concurrent_fetch: 4
    prefetch_ahead: 3
    prefetch_bandwidth_mbs: 20
    confidence_threshold: 0.75       # Higher confidence for ML
    enable_ml_prediction: true
    ml_model_path: "/var/lib/objectfs/model.bin"
    learning_rate: 0.01
    pattern_depth: 2000              # Deep analysis
    metrics_enabled: true
    statistics_interval: "1m"
    model_update_interval: "10m"
```

### Development/Testing Configuration

Minimal overhead for development:

```yaml
performance:
  read_ahead:
    enabled: true
    size: "16MB"                     # Minimal buffer
    strategy: "simple"
    enable_pattern_detection: false
    enable_prefetch: false
    metrics_enabled: false
```

## Environment Variables

All read-ahead settings can be configured via environment variables:

```bash
# Basic settings
export OBJECTFS_READAHEAD_ENABLED=true
export OBJECTFS_READAHEAD_SIZE="64MB"
export OBJECTFS_READAHEAD_STRATEGY="predictive"

# Pattern detection
export OBJECTFS_READAHEAD_PATTERN_DETECTION=true

# Prefetching
export OBJECTFS_READAHEAD_PREFETCH=true

# ML prediction
export OBJECTFS_READAHEAD_ML_PREDICTION=false
```

## Performance Tuning Guide

### Detecting Sequential Access

Monitor your workload to identify sequential access patterns:

1. **Enable metrics**: Set `metrics_enabled: true`
2. **Check sequential score**: Look for patterns with `SequentialScore > 0.7`
3. **Analyze prefetch efficiency**: Aim for `PrefetchEfficiency > 70%`

### Optimizing Buffer Size

The `size` parameter affects memory usage and performance:

- **Small files (< 10MB)**: Use `16-32MB` buffer
- **Medium files (10-100MB)**: Use `64-128MB` buffer
- **Large files (> 100MB)**: Use `128-256MB` buffer
- **Video/streaming**: Use `256MB+` buffer

### Tuning Prefetch Settings

Adjust prefetch parameters based on metrics:

- **High waste (> 30%)**: Increase `confidence_threshold` or decrease `prefetch_ahead`
- **Low hit rate (< 50%)**: Decrease `confidence_threshold` or increase `prediction_window`
- **Bandwidth concerns**: Lower `prefetch_bandwidth_mbs` or `max_concurrent_fetch`

### ML Model Training

For ML strategy effectiveness:

1. **Collect training data**: Run with `strategy: "predictive"` and `metrics_enabled: true`
2. **Analyze patterns**: Review access history and pattern detection results
3. **Train model**: Use collected data to train ML model offline
4. **Load model**: Set `ml_model_path` to trained model location
5. **Enable ML**: Set `enable_ml_prediction: true` and `strategy: "ml"`
6. **Monitor performance**: Track `PredictionAccuracy` and adjust `learning_rate`

## API Usage

### Accessing Predictive Stats

```go
import "github.com/objectfs/objectfs/internal/cache"

// Get predictive cache stats
predictiveCache := cache.GetPredictiveCache()
stats := predictiveCache.GetPredictiveStats()

fmt.Printf("Prediction Accuracy: %.2f%%\n", stats.PredictionAccuracy)
fmt.Printf("Prefetch Efficiency: %.2f%%\n", stats.PrefetchEfficiency)
fmt.Printf("Cache Hit Improvement: %.2f%%\n", stats.CacheHitImprovement)
```

### Custom Pattern Detection

```go
// Configure custom pattern detection
config := &cache.PredictiveCacheConfig{
    EnablePrediction:    true,
    PredictionWindow:    150,
    ConfidenceThreshold: 0.8,
    EnablePrefetch:      true,
    MaxConcurrentFetch:  6,
    PrefetchAhead:       4,
}

predictiveCache, err := cache.NewPredictiveCache(config)
if err != nil {
    log.Fatal(err)
}
```

## Troubleshooting

### Low Prediction Accuracy

**Symptoms**: `PredictionAccuracy < 50%`

**Solutions**:

- Increase `prediction_window` for more context
- Adjust `sequential_threshold` to match your access patterns
- Consider switching from `ml` to `predictive` strategy
- Check if workload has predictable patterns

### High Prefetch Waste

**Symptoms**: `PrefetchWaste > 30%` of prefetched data

**Solutions**:

- Increase `confidence_threshold` to be more conservative
- Decrease `prefetch_ahead` to fetch less aggressively
- Reduce `prefetch_bandwidth_mbs` to limit wasted bandwidth
- Disable prefetching for highly random workloads

### Insufficient Memory

**Symptoms**: Out-of-memory errors with read-ahead enabled

**Solutions**:

- Reduce `size` parameter
- Decrease `max_concurrent_fetch`
- Lower `prefetch_ahead`
- Disable prefetching: `enable_prefetch: false`

### Network Congestion

**Symptoms**: High bandwidth usage from prefetching

**Solutions**:

- Set `prefetch_bandwidth_mbs` to limit bandwidth
- Reduce `max_concurrent_fetch`
- Decrease `prefetch_ahead`
- Increase `confidence_threshold` to prefetch only high-confidence predictions

## Best Practices

1. **Start conservative**: Begin with default settings and tune based on metrics
2. **Monitor metrics**: Always enable metrics in production to track effectiveness
3. **Match workload**: Choose strategy based on your access patterns
4. **Gradual tuning**: Make small adjustments and measure impact
5. **Test thoroughly**: Validate configuration changes in staging environment

## Related Documentation

- [Cache Configuration](../configuration/cache.md)
- [Performance Tuning Guide](./performance.md)
- [Multipart Upload Optimization](./multipart-uploads.md)
- [ML Model Training Guide](./ml-training.md)

## Implementation Details

The read-ahead system is implemented in:

- `internal/cache/predictive.go` - Core predictive cache implementation (915 lines)
- `internal/config/config.go` - Configuration structure and validation

### Key Components

- **AccessPredictor**: Analyzes access patterns and predicts future accesses
- **IntelligentPrefetcher**: Manages prefetch operations with bandwidth control
- **PredictionModel**: ML model for access prediction using gradient descent
- **IntelligentEvictionManager**: ML-driven cache eviction decisions

### Performance Characteristics

- **Memory overhead**: ~1-2% of cache size for pattern tracking
- **CPU overhead**: < 1% for pattern analysis (non-ML)
- **ML overhead**: 2-5% additional CPU for model training
- **Prefetch bandwidth**: Configurable, default 10 MB/s
- **Prediction latency**: < 1ms for pattern lookup
