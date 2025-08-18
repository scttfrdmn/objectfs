/*
Package types provides the core interfaces, data structures, and type definitions for ObjectFS.

This package serves as the foundation for the entire ObjectFS system, defining the contracts
between different components and establishing the data structures used throughout the codebase.

# Architecture Overview

ObjectFS follows a layered architecture with well-defined interfaces between components:

	┌─────────────────────────────────────────────┐
	│              FUSE Interface                 │
	│         (cmd/objectfs, internal/fuse)      │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│            Core Adapter Layer              │
	│           (internal/adapter)               │
	└─────────────────────────────────────────────┘
	          │        │        │        │
	┌─────────┴───┐ ┌──┴──┐ ┌───┴───┐ ┌──┴──────┐
	│   Backend   │ │Cache│ │Buffer │ │Metrics  │
	│ (Storage)   │ │     │ │       │ │         │
	└─────────────┘ └─────┘ └───────┘ └─────────┘

# Core Interfaces

The package defines several critical interfaces that enable loose coupling and testability:

Backend Interface:
Abstracts AWS S3 storage operations with support for both
individual and batch operations. Implementations handle storage-specific details
while providing a uniform API.

Cache Interface:
Defines multi-level caching capabilities with LRU eviction, statistics tracking,
and range-based operations for optimal performance.

WriteBuffer Interface:
Provides intelligent write buffering with configurable flush policies, compression,
and batch optimization for improved write performance.

MetricsCollector Interface:
Enables comprehensive monitoring and observability with operation tracking,
cache metrics, and error reporting for Prometheus integration.

# Data Structures

Key data structures include:

ObjectInfo:
Complete metadata representation for stored objects including size, timestamps,
checksums, and custom metadata attributes.

AccessPattern:
Machine learning-ready structure for tracking file access patterns, enabling
predictive prefetching and performance optimization.

Configuration Types:
Comprehensive configuration hierarchy supporting YAML, environment variables,
and runtime reconfiguration across all ObjectFS components.

PerformanceMetrics:
Real-time performance tracking including throughput, latency, cache hit rates,
and system resource utilization.

# Usage Examples

Implementing a new backend:

	type MyBackend struct {
		client *myservice.Client
	}

	func (b *MyBackend) GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error) {
		// Implementation specific to your storage service
		return b.client.GetRange(key, offset, size)
	}

	func (b *MyBackend) HeadObject(ctx context.Context, key string) (*types.ObjectInfo, error) {
		meta, err := b.client.GetMetadata(key)
		if err != nil {
			return nil, err
		}
		return &types.ObjectInfo{
			Key:          key,
			Size:         meta.Size,
			LastModified: meta.Modified,
			ETag:         meta.ETag,
		}, nil
	}

Using configuration types:

	config := &types.Configuration{
		Performance: types.PerformanceConfig{
			CacheSize:       "2GB",
			MaxConcurrency:  100,
			CompressionEnabled: true,
		},
		Cache: types.CacheConfig{
			TTL:            5 * time.Minute,
			EvictionPolicy: "weighted_lru",
		},
	}

# Interface Contracts

All interfaces in this package follow these principles:

1. Context Awareness: Operations accept context.Context for cancellation and timeouts
2. Error Handling: All operations return explicit errors following Go conventions
3. Range Operations: Support efficient partial reads with offset/size parameters
4. Batch Operations: Provide batch variants for improved performance when applicable
5. Statistics: Include statistics and monitoring capabilities where appropriate

# Performance Considerations

The interfaces are designed with performance in mind:

- Range-based operations minimize data transfer
- Batch operations reduce API call overhead
- Asynchronous patterns supported through contexts
- Statistics collection enables performance monitoring and tuning
- Configuration supports performance profiling (low/medium/high latency)

# Thread Safety

All interfaces defined in this package are designed to be thread-safe when properly
implemented. Implementers must ensure:

- Concurrent access safety for all methods
- Proper synchronization for shared resources
- Atomic operations for statistics counters
- Context-aware cancellation handling

# Configuration Hierarchy

The configuration system supports multiple sources with precedence:

 1. Default values (lowest priority)
 2. Configuration files (YAML)
 3. Environment variables
 4. Runtime overrides (highest priority)

# Monitoring Integration

Types support comprehensive monitoring through:

- Prometheus metrics via MetricsCollector interface
- Health status reporting via HealthChecker interface
- Performance metrics collection via PerformanceMetrics
- Access pattern analysis via AccessPattern and AccessPredictor

This package serves as the contract definition for all ObjectFS components,
ensuring consistency, testability, and maintainability across the system.
*/
package types
