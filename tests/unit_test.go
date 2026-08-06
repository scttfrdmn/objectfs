package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/metrics"
	"github.com/scttfrdmn/objectfs/internal/vfs"
	"github.com/scttfrdmn/objectfs/pkg/utils"
)

// Unit tests for cache system
func TestLRUCacheUnit(t *testing.T) {
	t.Parallel()

	cacheConfig := &cache.CacheConfig{
		MaxSize:    1024 * 1024, // 1MB
		MaxEntries: 100,
		TTL:        time.Minute,
	}

	lruCache := cache.NewLRUCache(cacheConfig)
	require.NotNil(t, lruCache)

	// Test basic operations
	testKey := "test-key"
	testData := []byte("test data for LRU cache")

	// Test cache miss
	result := lruCache.Get(testKey, 0, int64(len(testData)))
	assert.Nil(t, result)

	// Test cache put
	lruCache.Put(testKey, 0, testData)

	// Test cache hit
	result = lruCache.Get(testKey, 0, int64(len(testData)))
	assert.Equal(t, testData, result)

	// Test cache statistics
	stats := lruCache.Stats()
	assert.Equal(t, uint64(1), stats.Hits)
	assert.Equal(t, uint64(1), stats.Misses)
	assert.Positive(t, stats.Size)

	// Test cache eviction
	evicted := lruCache.Evict(int64(len(testData)))
	assert.True(t, evicted)

	// Verify eviction worked
	result = lruCache.Get(testKey, 0, int64(len(testData)))
	assert.Nil(t, result)
}

func TestMultiLevelCacheUnit(t *testing.T) {
	t.Parallel()

	config := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       1024 * 1024, // 1MB
			MaxEntries: 100,
			TTL:        time.Minute,
		},
		Policy: "inclusive",
	}

	mlCache, err := cache.NewMultiLevelCache(config)
	require.NoError(t, err)
	require.NotNil(t, mlCache)

	// Test multi-level operations
	testKey := "ml-test-key"
	testData := []byte("multi-level cache test data")

	// Test initial miss
	result := mlCache.Get(testKey, 0, int64(len(testData)))
	assert.Nil(t, result)

	// Test put
	mlCache.Put(testKey, 0, testData)

	// Test hit
	result = mlCache.Get(testKey, 0, int64(len(testData)))
	assert.Equal(t, testData, result)

	// Test cache management
	err = mlCache.EnableLevel("L1")
	require.NoError(t, err)

	err = mlCache.DisableLevel("L1")
	require.NoError(t, err)

	err = mlCache.EnableLevel("L1")
	require.NoError(t, err)

	// Test level stats
	l1Stats, err := mlCache.GetLevelStats("L1")
	require.NoError(t, err)
	assert.NotNil(t, l1Stats)

	// Test invalid level
	_, err = mlCache.GetLevelStats("L3")
	assert.Error(t, err)
}

// Unit tests for the write path.
//
// The assertions are on the resulting object, not on buffer bookkeeping. v0.10.0's equivalent test
// asserted TotalWrites was positive and the flushed bytes equalled the bytes handed to the callback —
// both true while the flush was replacing whole objects with fragments, because the callback never saw
// the offset the test would have had to check.
func TestWriteBufferUnit(t *testing.T) {
	t.Parallel()

	backend := NewMockBackend()
	ctx := context.Background()

	writeBuffer, err := vfs.NewWriter(ctx, backend)
	require.NoError(t, err)
	defer func() { _ = writeBuffer.Close() }()

	// Two adjacent writes to one key must produce one object holding both, not the second alone.
	const key = "small-write"
	require.NoError(t, writeBuffer.Write(key, 0, []byte("small")))
	require.NoError(t, writeBuffer.Write(key, 5, make([]byte, 600)))

	// Nothing is uploaded until a flush is asked for.
	_, err = backend.HeadObject(ctx, key)
	require.Error(t, err, "a buffered write must not be uploaded before it is flushed")

	require.NoError(t, writeBuffer.Flush(key))

	stored, err := backend.GetObject(ctx, key, 0, 0)
	require.NoError(t, err)
	assert.Len(t, stored, 605, "both writes must survive the flush")
	assert.Equal(t, []byte("small"), stored[:5])

	// A write at an offset past the end of an existing object extends it rather than replacing it.
	// This is the H7 shape: v0.10.0 left a 9-byte object here.
	syncData := []byte("sync data")
	require.NoError(t, writeBuffer.Write(key, 605, syncData))
	require.NoError(t, writeBuffer.Flush(key))

	stored, err = backend.GetObject(ctx, key, 0, 0)
	require.NoError(t, err)
	require.Len(t, stored, 614)
	assert.Equal(t, syncData, stored[605:])

	// Flushing a key with nothing pending is a no-op, not an error: fsync on a clean file and a second
	// close(2) both take this path.
	assert.NoError(t, writeBuffer.Flush(key))
	assert.NoError(t, writeBuffer.FlushAll())
	assert.Zero(t, writeBuffer.Size(), "no dirty bytes should remain after a successful flush")
}

// Unit tests for metrics system
func TestMetricsCollectorUnit(t *testing.T) {
	t.Parallel()

	config := &metrics.Config{
		Enabled:        true,
		Addr:           "127.0.0.1:0", // the kernel picks a free port; Collector.Addr reports which
		Path:           "/metrics",
		Namespace:      "objectfs_test",
		UpdateInterval: 100 * time.Millisecond,
	}

	collector, err := metrics.NewCollector(config)
	require.NoError(t, err)
	require.NotNil(t, collector)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start collector
	err = collector.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = collector.Stop(ctx) }()

	// Record operations
	collector.RecordOperation("read", 100*time.Millisecond, 1024, true)
	collector.RecordOperation("write", 200*time.Millisecond, 2048, true)
	collector.RecordOperation("read", 50*time.Millisecond, 512, false)

	// Record cache metrics
	collector.RecordCacheHit("test-key", 1024)
	collector.RecordCacheMiss("miss-key", 2048)

	// Update gauges
	collector.UpdateCacheSize("L1", 10*1024*1024)
	collector.UpdateActiveConnections(5)

	// Record errors
	collector.RecordError("read", assert.AnError)

	// Get metrics
	metrics := collector.GetMetrics()
	assert.NotNil(t, metrics)

	// Verify operations were recorded
	assert.Contains(t, metrics, "operations")

	// Test reset
	collector.ResetMetrics()
	metricsAfterReset := collector.GetMetrics()
	assert.NotNil(t, metricsAfterReset)
}

// The parseSize helper that used to be here is gone. Its comment said "since it's not in the config
// package", and it never was: [utils.ParseBytes] has been the parser this whole time, and a copy in a
// test file is the worst of the four places one can live (#159). A test that reimplements the function
// it exercises asserts that two implementations agree, which is exactly what does not need asserting.
//
// Verified rather than assumed, by running the deleted copy: it read "InfMB" as math.MaxInt64 and
// "1e3MB" as 1000 MB, because it handed the number to strconv.ParseFloat, which accepts Go float
// syntax. It also rejected "1TB", since its unit map stopped at GB. utils.ParseBytes refuses all
// three — so this file's local parser disagreed with the one every mount uses, in both directions, for
// as long as it existed.

// Unit tests for configuration system
func TestConfigUnit(t *testing.T) {
	t.Parallel()

	// Test default configuration
	defaultConfig := config.NewDefault()
	require.NotNil(t, defaultConfig)

	// Verify default values
	assert.Equal(t, "INFO", defaultConfig.Global.LogLevel)
	// Loopback, not the wildcard: both endpoints are unauthenticated, so the default is the narrowest
	// thing that still works and publishing them further is a choice an operator writes down (#211).
	assert.Equal(t, config.DefaultMetricsAddr, defaultConfig.Monitoring.Metrics.Addr)
	assert.Equal(t, "weighted_lru", defaultConfig.Cache.EvictionPolicy)

	// Test configuration validation
	err := defaultConfig.Validate()
	require.NoError(t, err) // Default config should be valid

	// Test valid configuration.
	//
	// The region is set explicitly, and it is the whole reason this literal is not just the fields
	// under test: Validate refuses an empty region unless the environment supplies one, so leaving it
	// out made this config valid on a developer's machine and invalid in a container — the same
	// environment-dependence FuzzConfigConstructsBackend found in the loader.
	validConfig := &config.Configuration{
		Global: config.GlobalConfig{
			LogLevel: "DEBUG",
		},
		Monitoring: config.MonitoringConfig{
			Metrics:      config.MetricsConfig{Enabled: true, Addr: "127.0.0.1:19090"},
			HealthChecks: config.HealthChecksConfig{Enabled: true, Addr: "127.0.0.1:19091"},
		},
		Storage: config.StorageConfig{
			S3: config.S3Config{Region: "us-west-2"},
		},
		Performance: config.PerformanceConfig{
			CacheSize:          "100MB",
			WriteBufferSize:    "10MB",
			MaxConcurrency:     8,
			ConnectionPoolSize: 5,
			// Every field is required when Enabled is true: the manager cannot run with a zero
			// worker count, a zero sequential threshold, a zero TTL, or an empty window.
			ReadAhead: config.ReadAheadConfig{
				Enabled:         true,
				WindowSize:      "64KB",
				MinSequential:   3,
				ConcurrentReads: 4,
				TTL:             5 * time.Minute,
			},
		},
		Cache: config.CacheConfig{
			TTL:            time.Hour,
			MaxEntries:     5000,
			EvictionPolicy: "lru",
		},
	}

	err = validConfig.Validate()
	require.NoError(t, err)

	// A size written in a configuration this test declares valid parses under the parser the mount
	// itself uses. That is the property worth asserting here, and it is the only one: the units, the
	// malformed inputs, and the boundary cases belong to TestParseBytes and FuzzParseBytes in
	// pkg/utils, which is where the function is. Re-testing them through a local copy is what let this
	// file's copy disagree with the real parser without failing anything.
	size, err := utils.ParseBytes(validConfig.Performance.CacheSize)
	require.NoError(t, err)
	assert.Equal(t, int64(100*1024*1024), size)
}

// Unit tests for utility functions and edge cases
func TestUtilityFunctions(t *testing.T) {
	t.Parallel()

	// Test cache key generation and validation
	testCases := []struct {
		key    string
		offset int64
		size   int64
		valid  bool
	}{
		{"valid-key", 0, 1024, true},
		{"", 0, 1024, false},                           // Empty key
		{"valid-key", -1, 1024, false},                 // Negative offset
		{"valid-key", 0, -1, false},                    // Negative size
		{"valid-key", 0, 0, true},                      // Zero size (valid for metadata)
		{"very/long/key/path/file.txt", 0, 1024, true}, // Path-like key
	}

	for _, tc := range testCases {
		// This would test utility functions for key validation
		// In a real implementation, these would be actual utility functions
		isValid := tc.key != "" && tc.offset >= 0 && tc.size >= 0
		assert.Equal(t, tc.valid, isValid, "Key validation failed for: %+v", tc)
	}
}

// Test error conditions and edge cases
func TestErrorConditions(t *testing.T) {
	t.Parallel()

	// Test cache with zero size
	config := &cache.CacheConfig{
		MaxSize:    0,
		MaxEntries: 0,
		TTL:        0,
	}

	cache := cache.NewLRUCache(config)
	assert.NotNil(t, cache)

	// Operations should handle zero-size cache gracefully
	cache.Put("test", 0, []byte("data"))
	result := cache.Get("test", 0, 4)
	// With zero size, nothing should be cached
	assert.Nil(t, result)

	// The write path refuses malformed arguments and nothing else. There is deliberately no size
	// threshold that turns a large write into an error: v0.10.0 had one, and a write it could not fit
	// became EIO rather than a flush — which is how tar and SQLite got EIO from a working filesystem.
	writeBuffer, err := vfs.NewWriter(context.Background(), NewMockBackend())
	require.NoError(t, err)
	defer func() { _ = writeBuffer.Close() }()

	require.Error(t, writeBuffer.Write("", 0, []byte("data")), "an empty key is a caller bug")
	require.Error(t, writeBuffer.Write("test", -1, []byte("data")),
		"a negative offset must not be clamped to zero, where corruption is maximally destructive")
	require.NoError(t, writeBuffer.Write("test", 0, []byte("data longer than any buffer v0.10.0 would accept")))

	// Test metrics with disabled configuration
	disabledConfig := &metrics.Config{
		Enabled: false,
	}

	collector, err := metrics.NewCollector(disabledConfig)
	require.NoError(t, err)

	// Operations should be no-ops
	collector.RecordOperation("test", time.Millisecond, 100, true)
	metrics := collector.GetMetrics()
	assert.NotNil(t, metrics)
}

// Concurrent access tests
func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Skipping concurrent test in short mode")
	}

	// Test concurrent cache access
	cacheConfig := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       1024 * 1024,
			MaxEntries: 1000,
		},
	}

	mlCache, err := cache.NewMultiLevelCache(cacheConfig)
	require.NoError(t, err)

	const numGoroutines = 10
	const operationsPerGoroutine = 100

	// Channel to collect errors
	errors := make(chan error, numGoroutines*operationsPerGoroutine)
	done := make(chan bool, numGoroutines)

	// Start concurrent operations
	for i := range numGoroutines {
		go func(id int) {
			defer func() { done <- true }()

			for j := range operationsPerGoroutine {
				key := fmt.Sprintf("concurrent-key-%d-%d", id, j)
				data := fmt.Appendf(nil, "data-%d-%d", id, j)

				// Mix of operations
				switch j % 3 {
				case 0:
					mlCache.Put(key, 0, data)
				case 1:
					mlCache.Get(key, 0, int64(len(data)))
				case 2:
					mlCache.Delete(key)
				}
			}
		}(i)
	}

	// Wait for all goroutines
	for range numGoroutines {
		select {
		case <-done:
		case err := <-errors:
			t.Errorf("Concurrent operation failed: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("Concurrent test timed out")
		}
	}

	// Verify cache is still functional
	mlCache.Put("final-test", 0, []byte("final"))
	result := mlCache.Get("final-test", 0, 5)
	assert.Equal(t, []byte("final"), result)
}
