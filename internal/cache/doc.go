/*
Package cache provides multi-level intelligent caching for high-performance object storage access.

This package implements a sophisticated caching system with multiple cache levels, intelligent
eviction policies, and performance-aware optimization. It significantly improves ObjectFS
performance by reducing object storage API calls and providing fast local access to frequently
accessed data.

# Cache Architecture

Multi-level cache hierarchy with intelligent coordination:

	┌─────────────────────────────────────────────┐
	│              Application                    │
	│         (File Operations)                   │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│            Cache Interface                  │  ← This Package
	│         (types.Cache impl)                  │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│           Multi-Level Cache                 │
	│  ┌─────────────────────────────────────────┐  │
	│  │              L1 Cache                   │  │
	│  │          (Memory - Fast)                │  │
	│  │   • LRU/Weighted LRU                   │  │
	│  │   • 256MB - 8GB capacity               │  │
	│  │   • Nanosecond access time             │  │
	│  │   • Volatile storage                    │  │
	│  └─────────────────────────────────────────┘  │
	│                     │                       │
	│  ┌─────────────────────────────────────────┐  │
	│  │              L2 Cache                   │  │
	│  │        (Persistent SSD - Durable)       │  │
	│  │   • Compression enabled                 │  │
	│  │   • 1GB - 100GB capacity               │  │
	│  │   • Microsecond access time             │  │
	│  │   • Persistent across restarts         │  │
	│  └─────────────────────────────────────────┘  │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│           Object Storage                    │
	│      (S3, GCS, Azure - Slow)               │
	│   • Millisecond+ access time               │
	│   • API call costs                         │
	│   • Network dependency                     │
	└─────────────────────────────────────────────┘

# Cache Levels

L1 Cache (Memory):
- Ultra-fast in-memory cache
- LRU and Weighted LRU eviction policies
- Configurable size (256MB to 8GB typical)
- Automatic memory pressure handling
- Hot data optimization

L2 Cache (Persistent):
- SSD-based persistent storage
- Survives application restarts
- Compression for space efficiency
- Background cleanup and maintenance
- Cold data retention

# Eviction Policies

Multiple intelligent eviction strategies:

LRU (Least Recently Used):
- Traditional time-based eviction
- Simple and predictable behavior
- Good for uniform access patterns
- Low computational overhead

Weighted LRU:
- Combines recency with access frequency
- Adapts to varying access patterns
- Protects frequently accessed data
- Optimal for mixed workloads

Access Pattern Aware:
- Machine learning-based predictions
- File type and size considerations
- Seasonal and temporal patterns
- Predictive prefetching integration

# Usage Examples

Basic cache configuration:

	config := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       2 * 1024 * 1024 * 1024, // 2GB
			MaxEntries: 100000,
			TTL:        5 * time.Minute,
			Policy:     "weighted_lru",
		},
		L2Config: &cache.L2Config{
			Enabled:     true,
			Size:        10 * 1024 * 1024 * 1024, // 10GB
			Directory:   "/var/cache/objectfs",
			TTL:         1 * time.Hour,
			Compression: true,
		},
	}

	cache, err := cache.NewMultiLevelCache(config)
	if err != nil {
		log.Fatal(err)
	}
	defer cache.Close()

Cache operations:

	// Store data in cache
	key := "data/important-file.txt"
	data := []byte("file content")
	cache.Put(key, 0, data)

	// Retrieve from cache (tries L1, then L2, then miss)
	cached := cache.Get(key, 0, int64(len(data)))
	if cached != nil {
		fmt.Printf("Cache hit! Data: %s\n", cached)
	} else {
		fmt.Println("Cache miss - fetch from storage")
		// ... fetch from object storage ...
		// Store in cache for next time
		cache.Put(key, 0, fetchedData)
	}

	// Range-based caching (for large files)
	offset := int64(1024)
	size := int64(4096)
	chunk := cache.Get(key, offset, size)

	// Cache statistics
	stats := cache.Stats()
	fmt.Printf("Hit rate: %.2f%%\n", stats.HitRate*100)
	fmt.Printf("Utilization: %.2f%%\n", stats.Utilization*100)

# Performance Optimization

Multiple optimization strategies:

Intelligent Prefetching:
- Sequential access pattern detection
- Predictive data loading
- Background prefetch workers
- Adaptive prefetch size calculation

Memory Management:
- Memory pressure monitoring
- Automatic cache size adjustment
- Memory pool reuse
- Garbage collection optimization

Compression:
- Content-aware compression
- LZ4 for speed, GZIP for space efficiency
- Compression ratio monitoring
- Automatic compression threshold tuning

# Cache Statistics

Comprehensive performance monitoring:

Hit Rate Metrics:
- L1 and L2 cache hit rates
- Overall cache effectiveness
- Time-series hit rate tracking
- Access pattern analysis

Performance Metrics:
- Average access latency
- Throughput measurements
- Memory utilization
- Disk space usage

Operational Metrics:
- Eviction rates and causes
- Cache warming statistics
- Error rates and recovery
- Background operation performance

Example statistics usage:

	stats := cache.Stats()
	fmt.Printf("Cache Statistics:\n")
	fmt.Printf("  Hits: %d\n", stats.Hits)
	fmt.Printf("  Misses: %d\n", stats.Misses)
	fmt.Printf("  Hit Rate: %.2f%%\n", stats.HitRate*100)
	fmt.Printf("  Size: %s\n", formatBytes(stats.Size))
	fmt.Printf("  Utilization: %.2f%%\n", stats.Utilization*100)
	fmt.Printf("  Evictions: %d\n", stats.Evictions)

# Content-Aware Optimization

Intelligent optimization based on data characteristics:

File Type Optimization:
- Text files: High compression, longer retention
- Images: Minimal compression, size-based eviction
- Executables: Medium retention, integrity checking
- Logs: Streaming optimization, time-based eviction

Size-Based Strategies:
- Small files (<4KB): Full file caching, high retention
- Medium files (4KB-64MB): Range caching with prefetch
- Large files (>64MB): Selective range caching
- Huge files (>1GB): Minimal caching, streaming focus

Access Pattern Adaptation:
- Sequential reads: Aggressive prefetching
- Random access: Conservative caching
- Write-heavy: Write-through optimization
- Read-only: Aggressive caching and compression

# Cache Persistence

L2 cache persistence across restarts:

Index Management:
- JSON-based cache index
- Atomic index updates
- Corruption recovery
- Metadata validation

Data Storage:
- Individual file storage
- Compression metadata
- Checksum verification
- Background cleanup

Recovery:
- Automatic index rebuild
- Partial cache recovery
- Integrity verification
- Performance impact minimization

# Thread Safety

Designed for high-concurrency access:

Locking Strategy:
- Read-write locks for maximum concurrency
- Lock-free statistics collection
- Atomic operations for counters
- Minimal lock contention

Concurrent Operations:
- Multiple simultaneous reads
- Safe concurrent writes
- Background eviction processes
- Statistics collection threads

# Memory Management

Advanced memory management features:

Memory Pools:
- Object pooling for cache entries
- Buffer reuse for data transfers
- Reduced garbage collection pressure
- Memory fragmentation prevention

Pressure Handling:
- System memory pressure detection
- Automatic cache size reduction
- Emergency eviction policies
- Graceful degradation modes

Monitoring:
- Real-time memory usage tracking
- Memory leak detection
- Allocation pattern analysis
- Performance impact assessment

# Configuration Examples

Production configuration:

	# High-performance production setup
	cache:
	  l1:
	    size: "8GB"
	    policy: "weighted_lru"
	    max_entries: 500000
	    ttl: "10m"
	  l2:
	    enabled: true
	    size: "100GB"
	    directory: "/fast-ssd/objectfs-cache"
	    compression: true
	    ttl: "4h"

Development configuration:

	# Development/testing setup
	cache:
	  l1:
	    size: "512MB"
	    policy: "lru"
	    max_entries: 10000
	    ttl: "5m"
	  l2:
	    enabled: false  # Disable persistence for testing

High-latency configuration:

	# Optimized for satellite/high-latency connections
	cache:
	  l1:
	    size: "2GB"
	    policy: "weighted_lru"
	    ttl: "30m"      # Longer retention
	  l2:
	    enabled: true
	    size: "50GB"
	    compression: true
	    ttl: "24h"      # Very long retention

This package provides the critical performance layer that makes ObjectFS
practical for real-world workloads by dramatically reducing storage API
calls and providing fast local access to frequently used data.
*/
package cache
