package tests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/metrics"
	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/vfs"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// IntegrationTestSuite contains all integration tests
type IntegrationTestSuite struct {
	suite.Suite
	tempDir    string
	mountPoint string
	cacheDir   string
	configFile string
	testBucket string
	ctx        context.Context
	cancel     context.CancelFunc
}

// SetupSuite runs once before all tests
func (suite *IntegrationTestSuite) SetupSuite() {
	var err error

	// Create temporary directories
	suite.tempDir, err = os.MkdirTemp("", "objectfs-integration-test")
	suite.Require().NoError(err)

	suite.mountPoint = filepath.Join(suite.tempDir, "mount")
	suite.cacheDir = filepath.Join(suite.tempDir, "cache")
	suite.configFile = filepath.Join(suite.tempDir, "config.yaml")

	err = os.MkdirAll(suite.mountPoint, 0750)
	suite.Require().NoError(err)

	err = os.MkdirAll(suite.cacheDir, 0750)
	suite.Require().NoError(err)

	// Set up test context
	suite.ctx, suite.cancel = context.WithTimeout(context.Background(), 5*time.Minute)

	// Use test bucket (would be configured in actual tests)
	suite.testBucket = "objectfs-realdata-test-1753649951"
}

// TearDownSuite runs once after all tests
func (suite *IntegrationTestSuite) TearDownSuite() {
	if suite.cancel != nil {
		suite.cancel()
	}

	if suite.tempDir != "" {
		_ = os.RemoveAll(suite.tempDir)
	}
}

// SetupTest runs before each test
func (suite *IntegrationTestSuite) SetupTest() {
	// Clean up any existing test data
	suite.cleanupTestData()
}

// TearDownTest runs after each test
func (suite *IntegrationTestSuite) TearDownTest() {
	suite.cleanupTestData()
}

// Test S3 Backend Integration
func (suite *IntegrationTestSuite) TestS3BackendIntegration() {
	t := suite.T()

	// Skip in short mode
	if testing.Short() {
		t.Skip("Skipping S3 integration test in short mode")
	}

	// Skip if no S3 credentials available
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Skip("Skipping S3 integration test - no AWS credentials")
	}

	// Create S3 backend configuration
	s3Config := &s3.Config{
		Region:     "us-west-2",
		MaxRetries: 3,
		PoolSize:   4,
	}

	// Create S3 backend
	backend, err := s3.NewBackend(suite.ctx, suite.testBucket, s3Config)
	require.NoError(t, err)
	defer func() { _ = backend.Close() }()

	// Test basic operations
	testKey := "integration-test/test-file.txt"
	testData := []byte("Hello, ObjectFS Integration Test!")

	// Test PutObject
	err = backend.PutObject(suite.ctx, testKey, testData, nil)
	require.NoError(t, err)

	// Test GetObject
	retrievedData, err := backend.GetObject(suite.ctx, testKey, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, testData, retrievedData)

	// Test HeadObject
	objInfo, err := backend.HeadObject(suite.ctx, testKey)
	require.NoError(t, err)
	assert.Equal(t, testKey, objInfo.Key)
	assert.Equal(t, int64(len(testData)), objInfo.Size)

	// Test partial read
	partialData, err := backend.GetObject(suite.ctx, testKey, 0, 5)
	require.NoError(t, err)
	assert.Equal(t, testData[:5], partialData)

	// Test DeleteObject
	err = backend.DeleteObject(suite.ctx, testKey)
	require.NoError(t, err)

	// Verify deletion
	_, err = backend.GetObject(suite.ctx, testKey, 0, 0)
	assert.Error(t, err)
}

// Test Cache System Integration
func (suite *IntegrationTestSuite) TestCacheIntegration() {
	t := suite.T()

	// Create multi-level cache configuration
	cacheConfig := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024, // 10MB
			MaxEntries: 1000,
			TTL:        5 * time.Minute,
			Prefetch:   true,
		},
		L2Config: &cache.L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024, // 100MB
			Directory:   suite.cacheDir,
			TTL:         30 * time.Minute,
			Compression: true,
		},
		Policy: "inclusive",
	}

	// Create multi-level cache
	mlCache, err := cache.NewMultiLevelCache(cacheConfig)
	require.NoError(t, err)

	// Test cache operations
	testKey := "cache-test-key"
	testData := []byte("Cache test data for integration testing")

	// Test cache miss
	cachedData := mlCache.Get(testKey, 0, int64(len(testData)))
	assert.Nil(t, cachedData)

	// Test cache put
	mlCache.Put(testKey, 0, testData)

	// Test cache hit
	cachedData = mlCache.Get(testKey, 0, int64(len(testData)))
	assert.Equal(t, testData, cachedData)

	// Test cache statistics
	stats := mlCache.Stats()
	assert.Positive(t, stats.Hits)
	assert.Positive(t, stats.Misses)

	// Test cache eviction
	evicted := mlCache.Evict(int64(len(testData)))
	assert.True(t, evicted)

	// Test level-specific operations
	l1Stats, err := mlCache.GetLevelStats("L1")
	require.NoError(t, err)
	assert.NotNil(t, l1Stats)

	l2Stats, err := mlCache.GetLevelStats("L2")
	require.NoError(t, err)
	assert.NotNil(t, l2Stats)
}

// TestWriteBufferIntegration exercises the write path against a backend, asserting on what ends up
// stored rather than on what a flush callback was handed.
//
// The distinction is the whole point. v0.10.0's version of this test captured the callback's data into
// a map and compared it against the bytes written, which passed while the flush was replacing whole
// objects with fragments — the callback never saw the offset, so the test could not check it.
func (suite *IntegrationTestSuite) TestWriteBufferIntegration() {
	t := suite.T()

	backend := NewMockBackend()
	writeBuffer, err := vfs.NewWriter(suite.ctx, backend)
	require.NoError(t, err)
	defer func() { _ = writeBuffer.Close() }()

	// A file written in two disjoint pieces. v0.10.0 refused the second write outright (H8: any write
	// that did not continue the single contiguous run returned EIO), and had it accepted it, the flush
	// would have stored only one piece.
	const key = "buffer-test-key"
	head := []byte("Write buffer test data")
	tail := []byte("data past a hole")
	require.NoError(t, writeBuffer.Write(key, 0, head))
	require.NoError(t, writeBuffer.Write(key, 4096, tail))

	assert.True(t, writeBuffer.Dirty(key), "pending writes must be visible as dirty before a flush")
	assert.Positive(t, writeBuffer.Size())

	// Nothing reaches storage until a flush is asked for.
	_, err = backend.HeadObject(suite.ctx, key)
	require.Error(t, err, "a buffered write must not be uploaded before it is flushed")

	require.NoError(t, writeBuffer.FlushAll())
	assert.False(t, writeBuffer.Dirty(key))

	stored, err := backend.GetObject(suite.ctx, key, 0, 0)
	require.NoError(t, err)
	require.Len(t, stored, 4096+len(tail), "the object must span the hole, not just the last write")
	assert.Equal(t, head, stored[:len(head)])
	assert.Equal(t, tail, stored[4096:])
	// The hole reads as zeros, which is what a sparse file does on a local filesystem.
	assert.Equal(t, make([]byte, 4096-len(head)), stored[len(head):4096])

	// A second key flushed independently, and a write that modifies the middle of an existing object
	// rather than replacing it — the read-modify-write case that gives an offset write its meaning.
	const key2 = "buffer-test-key-2"
	require.NoError(t, writeBuffer.Write(key2, 0, []byte("AAAABBBBCCCC")))
	require.NoError(t, writeBuffer.Flush(key2))
	require.NoError(t, writeBuffer.Write(key2, 4, []byte("xxxx")))
	require.NoError(t, writeBuffer.Flush(key2))

	stored2, err := backend.GetObject(suite.ctx, key2, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("AAAAxxxxCCCC"), stored2,
		"an offset write must modify those bytes and leave the rest; v0.10.0 left a 4-byte object here")

	// Reading through the write path sees pending writes, not just stored bytes (H5: v0.10.0's read
	// path went straight to the backend, so a read after a write on the same descriptor returned
	// pre-write content).
	require.NoError(t, writeBuffer.Write(key2, 0, []byte("ZZZZ")))
	buf := make([]byte, 12)
	n, err := writeBuffer.ReadAt(suite.ctx, key2, buf, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("ZZZZxxxxCCCC"), buf[:n], "a read must observe writes that have not flushed")

	size, err := writeBuffer.FileSize(suite.ctx, key2)
	require.NoError(t, err)
	assert.Equal(t, int64(12), size)

	require.NoError(t, writeBuffer.FlushAll())
	assert.Zero(t, writeBuffer.Size(), "no dirty bytes may remain after a successful flush")
	assert.Zero(t, writeBuffer.Count())
}

// Test Metrics Collection Integration
func (suite *IntegrationTestSuite) TestMetricsIntegration() {
	t := suite.T()

	// Create metrics configuration
	metricsConfig := &metrics.Config{
		Enabled: true,
		// Port 0 on loopback: the kernel picks a port that is free at the moment of the bind, and
		// Collector.Addr reports which. The setting is an address rather than a port because a port
		// could not name an interface, so every value of it bound all of them (#211).
		Addr:           "127.0.0.1:0",
		Path:           "/metrics",
		Namespace:      "objectfs_test",
		UpdateInterval: time.Second,
	}

	// Create metrics collector
	collector, err := metrics.NewCollector(metricsConfig)
	require.NoError(t, err)

	// Start metrics collection
	err = collector.Start(suite.ctx)
	require.NoError(t, err)
	defer func() { _ = collector.Stop(suite.ctx) }()

	// Record some test operations
	collector.RecordOperation("read", 100*time.Millisecond, 1024, true)
	collector.RecordOperation("write", 200*time.Millisecond, 2048, true)
	collector.RecordOperation("read", 50*time.Millisecond, 512, false)

	// Record cache operations
	collector.RecordCacheHit("test-key", 1024)
	collector.RecordCacheMiss("another-key", 2048)

	// Update cache sizes
	collector.UpdateCacheSize("L1", 10*1024*1024)
	collector.UpdateCacheSize("L2", 100*1024*1024)

	// Update active connections
	collector.UpdateActiveConnections(5)

	// Get metrics
	collectedMetrics := collector.GetMetrics()
	assert.NotEmpty(t, collectedMetrics)

	// Verify operations were recorded
	operations, ok := collectedMetrics["operations"].(map[string]*metrics.OperationMetrics)
	assert.True(t, ok)
	assert.Contains(t, operations, "read")
	assert.Contains(t, operations, "write")

	readMetrics := operations["read"]
	assert.Equal(t, int64(2), readMetrics.Count)  // 1 success + 1 failure
	assert.Equal(t, int64(1), readMetrics.Errors) // 1 failure
}

// Test End-to-End File Operations
func (suite *IntegrationTestSuite) TestEndToEndFileOperations() {
	t := suite.T()

	// Skip in short mode
	if testing.Short() {
		t.Skip("Skipping end-to-end test in short mode")
	}

	// Skip if no S3 credentials available
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Skip("Skipping end-to-end test - no AWS credentials")
	}

	// This test would set up a complete ObjectFS instance
	// and test file operations through the FUSE interface

	// Create full ObjectFS configuration
	objectfsConfig := &config.Configuration{
		Global: config.GlobalConfig{
			LogLevel: "info",
		},
		Monitoring: config.MonitoringConfig{
			Metrics:      config.MetricsConfig{Enabled: true, Addr: "127.0.0.1:18080"},
			HealthChecks: config.HealthChecksConfig{Enabled: true, Addr: "127.0.0.1:18081"},
		},
		Performance: config.PerformanceConfig{
			CacheSize:          "50MB",
			WriteBufferSize:    "10MB",
			MaxConcurrency:     10,
			ConnectionPoolSize: 8,
		},
		Cache: config.CacheConfig{
			TTL:            30 * time.Minute,
			MaxEntries:     10000,
			EvictionPolicy: "lru",
		},
	}

	// In a real test, you would:
	// 1. Initialize the full ObjectFS system with this config
	// 2. Mount the filesystem
	// 3. Perform file operations through the filesystem
	// 4. Verify the operations work correctly
	// 5. Check metrics and performance
	// 6. Unmount and cleanup

	// For now, just verify the configuration is valid
	assert.NotNil(t, objectfsConfig)
	assert.Equal(t, "info", objectfsConfig.Global.LogLevel)
	assert.Equal(t, "127.0.0.1:18080", objectfsConfig.Monitoring.Metrics.Addr)
	assert.Equal(t, "50MB", objectfsConfig.Performance.CacheSize)
}

// Test Performance and Stress
func (suite *IntegrationTestSuite) TestPerformanceAndStress() {
	t := suite.T()

	// Skip long-running stress tests in short mode
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	// Create components for stress testing
	cacheConfig := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       50 * 1024 * 1024, // 50MB
			MaxEntries: 10000,
			TTL:        10 * time.Minute,
		},
		Policy: "inclusive",
	}

	mlCache, err := cache.NewMultiLevelCache(cacheConfig)
	require.NoError(t, err)

	// Stress test the cache with many concurrent operations
	const numGoroutines = 10
	const operationsPerGoroutine = 1000

	// Channel to signal completion
	done := make(chan bool, numGoroutines)

	// Start concurrent cache operations
	for i := range numGoroutines {
		go func(goroutineID int) {
			defer func() { done <- true }()

			for j := range operationsPerGoroutine {
				key := fmt.Sprintf("stress-test-%d-%d", goroutineID, j)
				data := fmt.Appendf(nil, "test data for %s", key)

				// Put data
				mlCache.Put(key, 0, data)

				// Get data
				retrieved := mlCache.Get(key, 0, int64(len(data)))
				assert.Equal(t, data, retrieved)

				// Sometimes delete data
				if j%10 == 0 {
					mlCache.Delete(key)
				}
			}
		}(i)
	}

	// Wait for all goroutines to complete
	for range numGoroutines {
		select {
		case <-done:
			// Goroutine completed
		case <-time.After(30 * time.Second):
			t.Fatal("Stress test timed out")
		}
	}

	// Verify cache statistics
	stats := mlCache.Stats()
	expectedOperations := uint64(numGoroutines * operationsPerGoroutine)

	// Should have processed all operations
	totalOps := stats.Hits + stats.Misses
	assert.GreaterOrEqual(t, totalOps, expectedOperations)

	// Should have a reasonable hit rate after warmup
	if totalOps > 0 {
		hitRate := float64(stats.Hits) / float64(totalOps)
		assert.Greater(t, hitRate, 0.1) // At least 10% hit rate
	}
}

// Test Error Handling and Recovery
func (suite *IntegrationTestSuite) TestErrorHandlingAndRecovery() {
	t := suite.T()

	// Test cache with invalid directory
	invalidCacheConfig := &cache.MultiLevelConfig{
		L2Config: &cache.L2Config{
			Enabled:   true,
			Directory: "/invalid/path/that/does/not/exist",
			Size:      1024 * 1024,
		},
	}

	_, err := cache.NewMultiLevelCache(invalidCacheConfig)
	require.Error(t, err) // Should fail to create cache with invalid directory

	// A backend that rejects uploads must produce a flush error, and the data must stay pending.
	//
	// This is defect M22. v0.10.0 recorded a failed flush by incrementing stats.Errors and returning
	// nil, and its version of this test asserted `stats.Errors >= 0` — a comparison that is true of
	// every uint64 ever produced, including the zero it got. So the one behavior that makes close(2)
	// mean something went untested by a test named for it.
	backend := &failingPutBackend{MockBackend: NewMockBackend(), err: fmt.Errorf("simulated flush error")}

	writeBuffer, err := vfs.NewWriter(suite.ctx, backend)
	require.NoError(t, err)
	defer func() { _ = writeBuffer.Close() }()

	require.NoError(t, writeBuffer.Write("error-test", 0, make([]byte, 600)),
		"a write is buffered locally, so it succeeds even when the backend will not accept uploads")

	err = writeBuffer.Flush("error-test")
	require.Error(t, err, "a rejected upload must be reported, not counted and discarded")
	require.ErrorContains(t, err, "simulated flush error",
		"the error must name the backend failure, not be flattened to a generic sync timeout")

	assert.True(t, writeBuffer.Dirty("error-test"),
		"a failed flush must leave the data pending so unmount can try again; dropping it is the "+
			"silent loss this assertion exists to prevent")

	// FlushAll reports the same failure rather than succeeding because it ran out of keys to try.
	require.Error(t, writeBuffer.FlushAllContext(suite.ctx))
}

// failingPutBackend is a [types.Backend] that stores nothing and rejects every upload.
//
// Reads are inherited from MockBackend, so the read-modify-write half of a flush behaves normally and
// the failure is isolated to the PUT — otherwise a test could pass because the GET failed first, for a
// reason that says nothing about error propagation.
type failingPutBackend struct {
	*MockBackend
	err error
}

func (b *failingPutBackend) PutObject(ctx context.Context, key string, data []byte, meta map[string]string) error {
	return b.err
}

// PutObjectIf fails with the same error, for the reason SetObjectMetadata does: this double exists to
// break every route by which a flush could reach storage, and one that still worked would let a flush
// report success through the half the test did not break.
func (b *failingPutBackend) PutObjectIf(ctx context.Context, key string, data []byte, meta map[string]string,
	cond types.Precondition,
) (string, error) {
	return "", b.err
}

// SetObjectMetadata fails too. The attribute-only write path is the other way a flush reaches storage,
// and leaving it inherited would let a flush report success through the half the test did not break.
func (b *failingPutBackend) SetObjectMetadata(ctx context.Context, key string, meta map[string]string) error {
	return b.err
}

func (b *failingPutBackend) PutObjects(ctx context.Context, objects map[string][]byte) error {
	return b.err
}

// verify at compile time that the override still satisfies the backend contract.
var _ types.Backend = (*failingPutBackend)(nil)

// Helper methods

func (suite *IntegrationTestSuite) cleanupTestData() {
	// Clean up any test files in mount point
	if suite.mountPoint != "" {
		entries, err := os.ReadDir(suite.mountPoint)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					_ = os.Remove(filepath.Join(suite.mountPoint, entry.Name()))
				}
			}
		}
	}

	// Clean up cache directory
	if suite.cacheDir != "" {
		_ = os.RemoveAll(suite.cacheDir)
		_ = os.MkdirAll(suite.cacheDir, 0750)
	}
}

// Run the integration test suite
func TestIntegrationSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, new(IntegrationTestSuite))
}

// Benchmark tests for performance validation
func BenchmarkCacheOperations(b *testing.B) {
	cacheConfig := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       100 * 1024 * 1024, // 100MB
			MaxEntries: 100000,
		},
	}

	mlCache, err := cache.NewMultiLevelCache(cacheConfig)
	if err != nil {
		b.Fatal(err)
	}

	testData := make([]byte, 1024) // 1KB test data

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench-key-%d", i%1000)

			// Mix of puts and gets
			if i%3 == 0 {
				mlCache.Put(key, 0, testData)
			} else {
				mlCache.Get(key, 0, int64(len(testData)))
			}
			i++
		}
	})
}

// BenchmarkWriteBuffer measures buffering a write, not flushing one.
//
// No flush happens here and that is deliberate: a flush is dominated by the backend round trip, so
// including it would measure the mock and hide the cost this benchmark exists to track — recording a
// dirty range, which is on the path of every write(2) the filesystem serves.
func BenchmarkWriteBuffer(b *testing.B) {
	writeBuffer, err := vfs.NewWriter(context.Background(), NewMockBackend())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = writeBuffer.Close() }()

	testData := make([]byte, 1024) // 1KB per write

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench-key-%d", i%100)
			if err := writeBuffer.Write(key, int64(i)*1024, testData); err != nil {
				b.Error(err)
				return
			}
			i++
		}
	})
}
