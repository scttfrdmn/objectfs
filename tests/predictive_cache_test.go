package tests

import (
	"context"
	cryptoRand "crypto/rand"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/objectfs/objectfs/internal/cache"
	"github.com/objectfs/objectfs/pkg/types"
)

// MockPredictiveBackend implements types.Backend for predictive cache testing
type MockPredictiveBackend struct {
	mu      sync.RWMutex
	objects map[string][]byte
	stats   struct {
		gets int64
		puts int64
	}
}

func NewMockPredictiveBackend() *MockPredictiveBackend {
	return &MockPredictiveBackend{
		objects: make(map[string][]byte),
	}
}

func (m *MockPredictiveBackend) GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.stats.gets++

	data, exists := m.objects[key]
	if !exists {
		return nil, fmt.Errorf("object not found: %s", key)
	}

	if offset >= int64(len(data)) {
		return []byte{}, nil
	}

	end := offset + size
	if end > int64(len(data)) {
		end = int64(len(data))
	}

	return data[offset:end], nil
}

func (m *MockPredictiveBackend) PutObject(ctx context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.puts++

	m.objects[key] = make([]byte, len(data))
	copy(m.objects[key], data)
	return nil
}

func (m *MockPredictiveBackend) DeleteObject(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *MockPredictiveBackend) ListObjects(ctx context.Context, prefix string, limit int) ([]types.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var objects []types.ObjectInfo
	for key, data := range m.objects {
		if len(prefix) == 0 || key[:len(prefix)] == prefix {
			objects = append(objects, types.ObjectInfo{
				Key:          key,
				Size:         int64(len(data)),
				LastModified: time.Now(),
			})
			if len(objects) >= limit {
				break
			}
		}
	}
	return objects, nil
}

func (m *MockPredictiveBackend) HeadObject(ctx context.Context, key string) (*types.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	data, exists := m.objects[key]
	if !exists {
		return nil, fmt.Errorf("object not found: %s", key)
	}

	return &types.ObjectInfo{
		Key:          key,
		Size:         int64(len(data)),
		LastModified: time.Now(),
	}, nil
}

func (m *MockPredictiveBackend) HealthCheck(ctx context.Context) error {
	return nil
}

func (m *MockPredictiveBackend) GetObjects(ctx context.Context, keys []string) (map[string][]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string][]byte)
	for _, key := range keys {
		if data, exists := m.objects[key]; exists {
			result[key] = data
		}
	}
	return result, nil
}

func (m *MockPredictiveBackend) PutObjects(ctx context.Context, objects map[string][]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, data := range objects {
		m.stats.puts++
		m.objects[key] = make([]byte, len(data))
		copy(m.objects[key], data)
	}
	return nil
}

func (m *MockPredictiveBackend) GetStats() (int64, int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stats.gets, m.stats.puts
}

// MockBaseCache implements a simple in-memory cache for testing
type MockBaseCache struct {
	mu    sync.RWMutex
	data  map[string][]byte
	stats types.CacheStats
}

func NewMockBaseCache() *MockBaseCache {
	return &MockBaseCache{
		data: make(map[string][]byte),
	}
}

func (c *MockBaseCache) Get(key string, offset, size int64) []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cacheKey := fmt.Sprintf("%s:%d:%d", key, offset, size)
	if data, exists := c.data[cacheKey]; exists {
		c.stats.Hits++
		return data
	}

	c.stats.Misses++
	return nil
}

func (c *MockBaseCache) Put(key string, offset int64, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cacheKey := fmt.Sprintf("%s:%d:%d", key, offset, int64(len(data)))
	c.data[cacheKey] = make([]byte, len(data))
	copy(c.data[cacheKey], data)
	c.stats.Size += int64(len(data))
}

func (c *MockBaseCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Find and delete all entries for this key
	for cacheKey := range c.data {
		if len(cacheKey) > len(key) && cacheKey[:len(key)] == key && cacheKey[len(key)] == ':' {
			delete(c.data, cacheKey)
		}
	}
}

func (c *MockBaseCache) Evict(size int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	evicted := int64(0)
	for key, data := range c.data {
		if evicted >= size {
			break
		}
		delete(c.data, key)
		evicted += int64(len(data))
		c.stats.Evictions++
	}

	c.stats.Size -= evicted
	return evicted >= size
}

func (c *MockBaseCache) Size() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats.Size
}

func (c *MockBaseCache) Stats() types.CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	totalOps := c.stats.Hits + c.stats.Misses
	hitRate := 0.0
	if totalOps > 0 {
		hitRate = float64(c.stats.Hits) / float64(totalOps)
	}

	return types.CacheStats{
		Hits:        c.stats.Hits,
		Misses:      c.stats.Misses,
		Evictions:   c.stats.Evictions,
		Size:        c.stats.Size,
		Capacity:    1024 * 1024 * 1024, // 1GB
		HitRate:     hitRate,
		Utilization: float64(c.stats.Size) / float64(1024*1024*1024),
	}
}

func TestPredictiveCache_BasicOperations(t *testing.T) {
	baseCache := NewMockBaseCache()
	config := &cache.PredictiveCacheConfig{
		BaseCache:           baseCache,
		EnablePrediction:    true,
		PredictionWindow:    10,
		ConfidenceThreshold: 0.5,
		EnablePrefetch:      false, // Disable for basic test
	}

	pc, err := cache.NewPredictiveCache(config)
	if err != nil {
		t.Fatalf("Failed to create predictive cache: %v", err)
	}

	// Test basic Put/Get
	key := "test-key"
	data := []byte("test data")

	pc.Put(key, 0, data)

	retrieved := pc.Get(key, 0, int64(len(data)))
	if retrieved == nil {
		t.Fatal("Expected to retrieve cached data")
	}

	if string(retrieved) != string(data) {
		t.Fatalf("Retrieved data mismatch: got %s, want %s", string(retrieved), string(data))
	}
}

func TestPredictiveCache_SequentialPrediction(t *testing.T) {
	baseCache := NewMockBaseCache()
	backend := NewMockPredictiveBackend()
	config := &cache.PredictiveCacheConfig{
		BaseCache:           baseCache,
		Backend:             backend,
		EnablePrediction:    true,
		PredictionWindow:    10,
		ConfidenceThreshold: 0.5,
		EnablePrefetch:      true,
		MaxConcurrentFetch:  2,
		PrefetchAhead:       3,
	}

	pc, err := cache.NewPredictiveCache(config)
	if err != nil {
		t.Fatalf("Failed to create predictive cache: %v", err)
	}
	defer func() {
		if err := pc.Close(); err != nil {
			t.Errorf("Failed to close predictive cache: %v", err)
		}
	}()

	key := "sequential-test"
	blockSize := int64(1024)

	// Create sequential access pattern
	for i := int64(0); i < 10; i++ {
		data := make([]byte, blockSize)
		for j := range data {
			data[j] = byte(i)
		}

		pc.Put(key, i*blockSize, data)

		// Simulate sequential reads
		retrieved := pc.Get(key, i*blockSize, blockSize)
		if retrieved == nil {
			t.Fatalf("Failed to get sequential block %d", i)
		}

		// Small delay to allow prediction processing
		time.Sleep(10 * time.Millisecond)
	}

	// Get predictive statistics
	stats := pc.GetPredictiveStats()
	t.Logf("Prediction accuracy: %.2f%%", stats.PredictionAccuracy*100)
	t.Logf("Total predictions: %d", stats.PredictionsTotal)
	t.Logf("Correct predictions: %d", stats.PredictionsCorrect)
}

func TestPredictiveCache_ConcurrentAccess(t *testing.T) {
	baseCache := NewMockBaseCache()
	backend := NewMockPredictiveBackend()
	config := &cache.PredictiveCacheConfig{
		BaseCache:           baseCache,
		Backend:             backend,
		EnablePrediction:    true,
		PredictionWindow:    50,
		ConfidenceThreshold: 0.7,
		EnablePrefetch:      true,
		MaxConcurrentFetch:  4,
		PrefetchAhead:       2,
	}

	pc, err := cache.NewPredictiveCache(config)
	if err != nil {
		t.Fatalf("Failed to create predictive cache: %v", err)
	}

	// Concurrent operations test
	numGoroutines := 10
	operationsPerGoroutine := 100
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			for j := 0; j < operationsPerGoroutine; j++ {
				key := fmt.Sprintf("concurrent-key-%d-%d", goroutineID, j)
				data := make([]byte, 512)
				_, _ = cryptoRand.Read(data)

				// Put data
				pc.Put(key, 0, data)

				// Get data back
				retrieved := pc.Get(key, 0, int64(len(data)))
				if retrieved == nil {
					t.Errorf("Failed to retrieve data for key %s", key)
					continue
				}

				// Verify data integrity
				if len(retrieved) != len(data) {
					t.Errorf("Data length mismatch for key %s: got %d, want %d",
						key, len(retrieved), len(data))
				}
			}
		}(i)
	}

	wg.Wait()

	// Verify cache statistics
	stats := pc.GetPredictiveStats()
	t.Logf("Final cache statistics:")
	t.Logf("  Total predictions: %d", stats.PredictionsTotal)
	t.Logf("  Prediction accuracy: %.2f%%", stats.PredictionAccuracy*100)
	t.Logf("  Prefetch requests: %d", stats.PrefetchRequests)
	t.Logf("  Prefetch hits: %d", stats.PrefetchHits)
	t.Logf("  Prefetch efficiency: %.2f%%", stats.PrefetchEfficiency*100)
}

func TestPredictiveCache_EvictionIntelligence(t *testing.T) {
	baseCache := NewMockBaseCache()
	config := &cache.PredictiveCacheConfig{
		BaseCache:                 baseCache,
		EnablePrediction:          true,
		EnableIntelligentEviction: true,
		EvictionAlgorithm:         "ml",
		PredictionWindow:          20,
		ConfidenceThreshold:       0.6,
	}

	pc, err := cache.NewPredictiveCache(config)
	if err != nil {
		t.Fatalf("Failed to create predictive cache: %v", err)
	}

	// Fill cache with test data
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("evict-test-%d", i)
		data := make([]byte, 1024)
		for j := range data {
			data[j] = byte(i)
		}

		pc.Put(key, 0, data)

		// Simulate different access patterns
		if i%3 == 0 {
			// Frequently accessed items
			for k := 0; k < 5; k++ {
				pc.Get(key, 0, 1024)
			}
		}
	}

	// Trigger eviction
	initialSize := pc.Size()
	evicted := pc.Evict(initialSize / 2)

	if !evicted {
		t.Error("Expected successful eviction")
	}

	finalSize := pc.Size()
	if finalSize >= initialSize {
		t.Errorf("Expected size reduction after eviction: initial=%d, final=%d",
			initialSize, finalSize)
	}

	t.Logf("Eviction results: initial size=%d, final size=%d, evicted=%d",
		initialSize, finalSize, initialSize-finalSize)
}

func BenchmarkPredictiveCache_SequentialRead(b *testing.B) {
	baseCache := NewMockBaseCache()
	backend := NewMockPredictiveBackend()
	config := &cache.PredictiveCacheConfig{
		BaseCache:           baseCache,
		Backend:             backend,
		EnablePrediction:    true,
		PredictionWindow:    100,
		ConfidenceThreshold: 0.7,
		EnablePrefetch:      true,
		MaxConcurrentFetch:  4,
		PrefetchAhead:       3,
	}

	pc, err := cache.NewPredictiveCache(config)
	if err != nil {
		b.Fatalf("Failed to create predictive cache: %v", err)
	}

	// Prepare test data
	key := "benchmark-sequential"
	blockSize := int64(4096)
	numBlocks := int64(1000)

	// Pre-populate cache
	for i := int64(0); i < numBlocks; i++ {
		data := make([]byte, blockSize)
		_, _ = cryptoRand.Read(data)
		pc.Put(key, i*blockSize, data)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		blockIndex := int64(i % int(numBlocks))
		data := pc.Get(key, blockIndex*blockSize, blockSize)
		if data == nil {
			b.Fatalf("Failed to get block %d", blockIndex)
		}
	}
}

func BenchmarkPredictiveCache_RandomRead(b *testing.B) {
	baseCache := NewMockBaseCache()
	config := &cache.PredictiveCacheConfig{
		BaseCache:           baseCache,
		EnablePrediction:    true,
		PredictionWindow:    100,
		ConfidenceThreshold: 0.7,
		EnablePrefetch:      false, // Disable for random access
	}

	pc, err := cache.NewPredictiveCache(config)
	if err != nil {
		b.Fatalf("Failed to create predictive cache: %v", err)
	}

	// Prepare test data
	numKeys := 1000
	keys := make([]string, numKeys)
	blockSize := int64(4096)

	for i := 0; i < numKeys; i++ {
		keys[i] = fmt.Sprintf("benchmark-random-%d", i)
		data := make([]byte, blockSize)
		_, _ = cryptoRand.Read(data)
		pc.Put(keys[i], 0, data)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		keyIndex := rand.Intn(numKeys)
		data := pc.Get(keys[keyIndex], 0, blockSize)
		if data == nil {
			b.Fatalf("Failed to get key %s", keys[keyIndex])
		}
	}
}

func BenchmarkPredictiveCache_ConcurrentAccess(b *testing.B) {
	baseCache := NewMockBaseCache()
	backend := NewMockPredictiveBackend()
	config := &cache.PredictiveCacheConfig{
		BaseCache:           baseCache,
		Backend:             backend,
		EnablePrediction:    true,
		PredictionWindow:    50,
		ConfidenceThreshold: 0.7,
		EnablePrefetch:      true,
		MaxConcurrentFetch:  8,
	}

	pc, err := cache.NewPredictiveCache(config)
	if err != nil {
		b.Fatalf("Failed to create predictive cache: %v", err)
	}

	// Prepare test data
	numKeys := 100
	keys := make([]string, numKeys)
	blockSize := int64(1024)

	for i := 0; i < numKeys; i++ {
		keys[i] = fmt.Sprintf("benchmark-concurrent-%d", i)
		data := make([]byte, blockSize)
		_, _ = cryptoRand.Read(data)
		pc.Put(keys[i], 0, data)
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			keyIndex := rand.Intn(numKeys)
			data := pc.Get(keys[keyIndex], 0, blockSize)
			if data == nil {
				b.Fatalf("Failed to get key %s", keys[keyIndex])
			}
		}
	})
}
