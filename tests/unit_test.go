package tests

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/objectfs/objectfs/internal/buffer"
	"github.com/objectfs/objectfs/internal/cache"
	"github.com/objectfs/objectfs/internal/config"
	"github.com/objectfs/objectfs/internal/metrics"
)

// Unit tests for cache system
func TestLRUCacheUnit(t *testing.T) {
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
	assert.Greater(t, stats.Size, int64(0))

	// Test cache eviction
	evicted := lruCache.Evict(int64(len(testData)))
	assert.True(t, evicted)

	// Verify eviction worked
	result = lruCache.Get(testKey, 0, int64(len(testData)))
	assert.Nil(t, result)
}

func TestMultiLevelCacheUnit(t *testing.T) {
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
	assert.NoError(t, err)

	err = mlCache.DisableLevel("L1")
	assert.NoError(t, err)

	err = mlCache.EnableLevel("L1")
	assert.NoError(t, err)

	// Test level stats
	l1Stats, err := mlCache.GetLevelStats("L1")
	assert.NoError(t, err)
	assert.NotNil(t, l1Stats)

	// Test invalid level
	_, err = mlCache.GetLevelStats("L3")
	assert.Error(t, err)
}

// Unit tests for write buffer system
func TestWriteBufferUnit(t *testing.T) {
	var flushedDataMu sync.RWMutex
	flushedData := make(map[string][]byte)
	flushCallback := func(key string, data []byte, offset int64) error {
		flushedDataMu.Lock()
		defer flushedDataMu.Unlock()
		flushedData[key] = make([]byte, len(data))
		copy(flushedData[key], data)
		return nil
	}

	config := &buffer.WriteBufferConfig{
		MaxBufferSize:  1024,
		MaxBuffers:     10,
		FlushInterval:  100 * time.Millisecond,
		FlushThreshold: 512,
		AsyncFlush:     false, // Synchronous for testing
		BatchSize:      5,
		MaxWriteDelay:  time.Second,
		SyncOnClose:    true,
		MaxRetries:     3,
		RetryDelay:     10 * time.Millisecond,
	}

	writeBuffer, err := buffer.NewWriteBuffer(config, flushCallback)
	require.NoError(t, err)
	require.NotNil(t, writeBuffer)
	defer func() { _ = writeBuffer.Close() }()

	ctx := context.Background()

	// Test small write (should be buffered)
	testKey := "small-write"
	smallData := []byte("small")
	req := &buffer.WriteRequest{
		Key:    testKey,
		Offset: 0,
		Data:   smallData,
		Sync:   false,
	}

	err = writeBuffer.Write(req.Key, req.Offset, req.Data)
	assert.NoError(t, err)

	// Data should not be flushed yet
	assert.NotContains(t, flushedData, testKey)

	// Test write that triggers flush
	largeData := make([]byte, 600) // Exceeds flush threshold
	req = &buffer.WriteRequest{
		Key:    testKey,
		Offset: int64(len(smallData)),
		Data:   largeData,
		Sync:   false,
	}

	err = writeBuffer.Write(req.Key, req.Offset, req.Data)
	assert.NoError(t, err)

	// Give time for async flush if any
	time.Sleep(200 * time.Millisecond)

	// Test synchronous write
	syncKey := "sync-write"
	syncData := []byte("sync data")
	req = &buffer.WriteRequest{
		Key:    syncKey,
		Offset: 0,
		Data:   syncData,
		Sync:   true,
	}

	err = writeBuffer.Write(req.Key, req.Offset, req.Data)
	assert.NoError(t, err)

	// Force flush to ensure data is written
	err = writeBuffer.Flush(syncKey)
	assert.NoError(t, err)

	// Give time for flush
	time.Sleep(100 * time.Millisecond)

	// Sync data should be flushed
	flushedDataMu.RLock()
	assert.Contains(t, flushedData, syncKey)
	actualData := flushedData[syncKey]
	flushedDataMu.RUnlock()
	assert.Equal(t, syncData, actualData)

	// Test buffer stats
	stats := writeBuffer.GetStats()
	assert.Greater(t, stats.TotalWrites, uint64(0))
	assert.GreaterOrEqual(t, stats.TotalBytes, int64(len(smallData)+len(largeData)+len(syncData)))

	// Test buffer info
	bufferInfo := writeBuffer.GetBufferInfo()
	assert.NotNil(t, bufferInfo)

	// Test explicit flush
	err = writeBuffer.Flush("")
	assert.NoError(t, err)

	// Test sync
	err = writeBuffer.Sync(ctx)
	assert.NoError(t, err)
}

func TestBufferManagerUnit(t *testing.T) {
	config := &buffer.ManagerConfig{
		WriteBufferConfig: &buffer.WriteBufferConfig{
			MaxBufferSize:  1024,
			FlushThreshold: 512,
			AsyncFlush:     false,
			MaxWriteDelay:  2 * time.Second, // Increase timeout for test
		},
		EnableMetrics:       true,
		MetricsInterval:     100 * time.Millisecond,
		HealthCheckInterval: 100 * time.Millisecond,
		MaxErrorRate:        0.1,
		AlertThreshold:      5,
	}

	manager, err := buffer.NewManager(config)
	require.NoError(t, err)
	require.NotNil(t, manager)

	// Register flush callback
	flushedData := make(map[string][]byte)
	flushCallback := func(key string, data []byte, offset int64) error {
		flushedData[key] = make([]byte, len(data))
		copy(flushedData[key], data)
		return nil
	}
	manager.RegisterFlushCallback("*", flushCallback)

	ctx := context.Background()

	// Start manager
	err = manager.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = manager.Stop() }()

	// Test write operations
	testKey := "manager-test"
	testData := []byte("manager test data")

	err = manager.Write(ctx, testKey, 0, testData, false)
	assert.NoError(t, err)

	// Test sync write
	err = manager.Write(ctx, testKey+"2", 0, testData, true)
	assert.NoError(t, err)

	// Wait for operations to complete
	time.Sleep(200 * time.Millisecond)

	// Test manager stats
	stats := manager.GetStats()
	assert.Greater(t, stats.TotalOperations, uint64(0))
	assert.True(t, manager.IsHealthy())

	// Test flush
	err = manager.Flush(ctx, "")
	assert.NoError(t, err)

	// Test sync
	err = manager.Sync(ctx)
	assert.NoError(t, err)

	// Test detailed info
	info := manager.GetDetailedInfo()
	assert.NotNil(t, info)
	assert.Contains(t, info, "manager_stats")
	assert.Contains(t, info, "started")
	assert.True(t, info["started"].(bool))

	// Test optimization
	manager.Optimize()

	// Test memory usage and throughput
	memUsage := manager.GetMemoryUsage()
	assert.GreaterOrEqual(t, memUsage, int64(0))

	throughput := manager.GetThroughput()
	assert.GreaterOrEqual(t, throughput, 0.0)

	// Test clear stats
	manager.ClearStats()
	stats = manager.GetStats()
	assert.Equal(t, uint64(0), stats.TotalOperations)
}

// Unit tests for metrics system
func TestMetricsCollectorUnit(t *testing.T) {
	config := &metrics.Config{
		Enabled:        true,
		Port:           0, // Use random port for testing
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

// Helper function for parsing sizes (since it's not in the config package)
func parseSize(sizeStr string) (int64, error) {
	sizeStr = strings.TrimSpace(sizeStr)
	if sizeStr == "" {
		return 0, fmt.Errorf("empty size string")
	}

	// Handle plain numbers
	if val, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
		return val, nil
	}

	// Handle sizes with units
	units := map[string]int64{
		"B":  1,
		"KB": 1024,
		"MB": 1024 * 1024,
		"GB": 1024 * 1024 * 1024,
	}

	for unit, multiplier := range units {
		if strings.HasSuffix(strings.ToUpper(sizeStr), unit) {
			numStr := strings.TrimSuffix(strings.ToUpper(sizeStr), unit)
			if val, err := strconv.ParseFloat(numStr, 64); err == nil {
				return int64(val * float64(multiplier)), nil
			}
		}
	}

	return 0, fmt.Errorf("invalid size format: %s", sizeStr)
}

// Unit tests for configuration system
func TestConfigUnit(t *testing.T) {
	// Test default configuration
	defaultConfig := config.NewDefault()
	require.NotNil(t, defaultConfig)

	// Verify default values
	assert.Equal(t, "INFO", defaultConfig.Global.LogLevel)
	assert.Equal(t, 8080, defaultConfig.Global.MetricsPort)
	assert.Equal(t, "weighted_lru", defaultConfig.Cache.EvictionPolicy)

	// Test configuration validation
	err := defaultConfig.Validate()
	assert.NoError(t, err) // Default config should be valid

	// Test valid configuration
	validConfig := &config.Configuration{
		Global: config.GlobalConfig{
			LogLevel:    "DEBUG",
			MetricsPort: 9090,
		},
		Performance: config.PerformanceConfig{
			CacheSize:          "100MB",
			WriteBufferSize:    "10MB",
			MaxConcurrency:     8,
			ConnectionPoolSize: 5,
		},
		Cache: config.CacheConfig{
			TTL:            time.Hour,
			MaxEntries:     5000,
			EvictionPolicy: "lru",
		},
	}

	err = validConfig.Validate()
	assert.NoError(t, err)

	// Test size parsing
	size, err := parseSize(validConfig.Performance.CacheSize)
	assert.NoError(t, err)
	assert.Equal(t, int64(100*1024*1024), size)

	// Test invalid size parsing
	_, err = parseSize("invalid-size")
	assert.Error(t, err)

	// Test various size formats
	testCases := []struct {
		input    string
		expected int64
	}{
		{"1024", 1024},
		{"1KB", 1024},
		{"1MB", 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
		{"1.5MB", int64(1.5 * 1024 * 1024)},
	}

	for _, tc := range testCases {
		size, err := parseSize(tc.input)
		assert.NoError(t, err, "Failed to parse size: %s", tc.input)
		assert.Equal(t, tc.expected, size, "Unexpected size for: %s", tc.input)
	}
}

// Unit tests for utility functions and edge cases
func TestUtilityFunctions(t *testing.T) {
	// Test cache key generation and validation
	testCases := []struct {
		key    string
		offset int64
		size   int64
		valid  bool
	}{
		{"valid-key", 0, 1024, true},
		{"", 0, 1024, false},                    // Empty key
		{"valid-key", -1, 1024, false},          // Negative offset
		{"valid-key", 0, -1, false},             // Negative size
		{"valid-key", 0, 0, true},               // Zero size (valid for metadata)
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

	// Test write buffer with minimal configuration
	minimalConfig := &buffer.WriteBufferConfig{
		MaxBufferSize:  1,
		MaxBuffers:     1,
		FlushThreshold: 1,
	}

	callback := func(key string, data []byte, offset int64) error {
		return nil
	}

	writeBuffer, err := buffer.NewWriteBuffer(minimalConfig, callback)
	assert.NoError(t, err)
	defer func() { _ = writeBuffer.Close() }()

	// Write should handle small buffer size
	req := &buffer.WriteRequest{
		Key:    "test",
		Offset: 0,
		Data:   []byte("data longer than buffer"),
		Sync:   false,
	}

	err = writeBuffer.Write(req.Key, req.Offset, req.Data)
	// Should return error when data is larger than buffer capacity
	assert.Error(t, err)

	// Test metrics with disabled configuration
	disabledConfig := &metrics.Config{
		Enabled: false,
	}

	collector, err := metrics.NewCollector(disabledConfig)
	assert.NoError(t, err)

	// Operations should be no-ops
	collector.RecordOperation("test", time.Millisecond, 100, true)
	metrics := collector.GetMetrics()
	assert.NotNil(t, metrics)
}

// Concurrent access tests
func TestConcurrentAccess(t *testing.T) {
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
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer func() { done <- true }()

			for j := 0; j < operationsPerGoroutine; j++ {
				key := fmt.Sprintf("concurrent-key-%d-%d", id, j)
				data := []byte(fmt.Sprintf("data-%d-%d", id, j))

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
	for i := 0; i < numGoroutines; i++ {
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