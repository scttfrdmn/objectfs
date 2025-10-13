package cache

import (
	"sync"
	"testing"
	"time"
)

// TestNewLRUCache tests cache creation with various configurations
func TestNewLRUCache(t *testing.T) {
	tests := []struct {
		name   string
		config *CacheConfig
		verify func(t *testing.T, cache *LRUCache)
	}{
		{
			name:   "nil config uses defaults",
			config: nil,
			verify: func(t *testing.T, cache *LRUCache) {
				if cache.capacity != 2*1024*1024*1024 {
					t.Errorf("expected default capacity 2GB, got %d", cache.capacity)
				}
				if cache.config.TTL != 5*time.Minute {
					t.Errorf("expected default TTL 5min, got %v", cache.config.TTL)
				}
				if cache.config.EvictionPolicy != "weighted_lru" {
					t.Errorf("expected default policy weighted_lru, got %s", cache.config.EvictionPolicy)
				}
			},
		},
		{
			name: "custom config applied",
			config: &CacheConfig{
				MaxSize:        1024 * 1024, // 1MB
				MaxEntries:     100,
				TTL:            time.Minute,
				EvictionPolicy: "lru",
			},
			verify: func(t *testing.T, cache *LRUCache) {
				if cache.capacity != 1024*1024 {
					t.Errorf("expected capacity 1MB, got %d", cache.capacity)
				}
				if cache.config.MaxEntries != 100 {
					t.Errorf("expected max entries 100, got %d", cache.config.MaxEntries)
				}
				if cache.config.TTL != time.Minute {
					t.Errorf("expected TTL 1min, got %v", cache.config.TTL)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewLRUCache(tt.config)
			if cache == nil {
				t.Fatal("NewLRUCache returned nil")
			}
			if cache.items == nil {
				t.Error("cache items map not initialized")
			}
			if cache.evictList == nil {
				t.Error("cache evict list not initialized")
			}
			tt.verify(t, cache)
		})
	}
}

// TestLRUCache_PutGet tests basic Put and Get operations
func TestLRUCache_PutGet(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize:    1024 * 1024,
		MaxEntries: 100,
		TTL:        time.Hour,
	})

	// Test Put and Get
	key := "test-object"
	offset := int64(0)
	data := []byte("hello world")

	cache.Put(key, offset, data)

	retrieved := cache.Get(key, offset, int64(len(data)))
	if retrieved == nil {
		t.Fatal("Get returned nil for existing key")
	}
	if string(retrieved) != string(data) {
		t.Errorf("expected %q, got %q", string(data), string(retrieved))
	}

	// Verify stats
	stats := cache.Stats()
	if stats.Hits != 1 {
		t.Errorf("expected 1 hit, got %d", stats.Hits)
	}
	if stats.Misses != 0 {
		t.Errorf("expected 0 misses, got %d", stats.Misses)
	}
}

// TestLRUCache_GetMiss tests cache miss behavior
func TestLRUCache_GetMiss(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize: 1024 * 1024,
		TTL:     time.Hour,
	})

	// Get non-existent key
	retrieved := cache.Get("nonexistent", 0, 100)
	if retrieved != nil {
		t.Error("expected nil for non-existent key")
	}

	// Verify stats
	stats := cache.Stats()
	if stats.Misses != 1 {
		t.Errorf("expected 1 miss, got %d", stats.Misses)
	}
}

// TestLRUCache_PutEmpty tests that empty data is ignored
func TestLRUCache_PutEmpty(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize: 1024 * 1024,
	})

	cache.Put("test", 0, []byte{})
	cache.Put("test", 0, nil)

	if len(cache.items) != 0 {
		t.Error("expected empty cache after putting empty data")
	}
}

// TestLRUCache_UpdateExisting tests updating an existing cache entry
func TestLRUCache_UpdateExisting(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize: 1024 * 1024,
		TTL:     time.Hour,
	})

	key := "test"
	offset := int64(0)
	data1 := []byte("first")
	data2 := []byte("again") // Same length to keep same cache key

	// Put first value
	cache.Put(key, offset, data1)
	retrieved := cache.Get(key, offset, int64(len(data1)))
	if string(retrieved) != string(data1) {
		t.Errorf("expected %q, got %q", string(data1), string(retrieved))
	}

	// Update with second value (same size)
	cache.Put(key, offset, data2)
	retrieved = cache.Get(key, offset, int64(len(data2)))
	if string(retrieved) != string(data2) {
		t.Errorf("expected %q, got %q", string(data2), string(retrieved))
	}

	// Should still have only one item in cache (same key:offset:size)
	if len(cache.items) != 1 {
		t.Errorf("expected 1 item in cache, got %d", len(cache.items))
	}
}

// TestLRUCache_Eviction tests LRU eviction when cache is full
func TestLRUCache_Eviction(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize:    100, // Small cache
		MaxEntries: 3,   // Max 3 entries
		TTL:        time.Hour,
	})

	// Add 3 items (fill cache)
	cache.Put("key1", 0, []byte("data1"))
	cache.Put("key2", 0, []byte("data2"))
	cache.Put("key3", 0, []byte("data3"))

	if len(cache.items) != 3 {
		t.Errorf("expected 3 items, got %d", len(cache.items))
	}

	// Add 4th item (should evict oldest)
	cache.Put("key4", 0, []byte("data4"))

	if len(cache.items) != 3 {
		t.Errorf("expected 3 items after eviction, got %d", len(cache.items))
	}

	// key1 should be evicted (oldest/least recently used)
	if cache.Get("key1", 0, 5) != nil {
		t.Error("key1 should have been evicted")
	}

	// Other keys should still exist
	if cache.Get("key2", 0, 5) == nil {
		t.Error("key2 should still exist")
	}
	if cache.Get("key3", 0, 5) == nil {
		t.Error("key3 should still exist")
	}
	if cache.Get("key4", 0, 5) == nil {
		t.Error("key4 should still exist")
	}
}

// TestLRUCache_EvictionBySize tests eviction based on size limit
func TestLRUCache_EvictionBySize(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize:    50, // 50 bytes
		MaxEntries: 100,
		TTL:        time.Hour,
	})

	// Add items that fill the cache
	cache.Put("key1", 0, make([]byte, 20)) // 20 bytes
	cache.Put("key2", 0, make([]byte, 20)) // 20 bytes

	currentSize := cache.Size()
	if currentSize != 40 {
		t.Errorf("expected size 40, got %d", currentSize)
	}

	// Add item that exceeds capacity
	cache.Put("key3", 0, make([]byte, 20)) // Should evict key1

	if cache.Size() > 50 {
		t.Errorf("cache size %d exceeds capacity 50", cache.Size())
	}

	// key1 should be evicted
	if cache.Get("key1", 0, 20) != nil {
		t.Error("key1 should have been evicted")
	}
}

// TestLRUCache_TTLExpiration tests TTL-based expiration
func TestLRUCache_TTLExpiration(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize: 1024 * 1024,
		TTL:     100 * time.Millisecond, // Very short TTL
	})

	key := "test"
	data := []byte("data")

	cache.Put(key, 0, data)

	// Should exist immediately
	if cache.Get(key, 0, int64(len(data))) == nil {
		t.Error("item should exist immediately after Put")
	}

	// Wait for TTL to expire
	time.Sleep(150 * time.Millisecond)

	// Should be expired
	retrieved := cache.Get(key, 0, int64(len(data)))
	if retrieved != nil {
		t.Error("item should have expired")
	}

	// Should count as miss
	stats := cache.Stats()
	if stats.Misses != 1 {
		t.Errorf("expected 1 miss from expired item, got %d", stats.Misses)
	}
}

// TestLRUCache_Delete tests Delete operation
func TestLRUCache_Delete(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize: 1024 * 1024,
		TTL:     time.Hour,
	})

	// Add multiple items with same key prefix
	cache.Put("user:123", 0, []byte("data1"))
	cache.Put("user:123", 100, []byte("data2"))
	cache.Put("user:456", 0, []byte("data3"))

	if len(cache.items) != 3 {
		t.Errorf("expected 3 items, got %d", len(cache.items))
	}

	// Delete by key prefix
	cache.Delete("user:123")

	// Should have only user:456 left
	if len(cache.items) != 1 {
		t.Errorf("expected 1 item after delete, got %d", len(cache.items))
	}

	if cache.Get("user:123", 0, 5) != nil {
		t.Error("user:123:0 should be deleted")
	}
	if cache.Get("user:456", 0, 5) == nil {
		t.Error("user:456 should still exist")
	}
}

// TestLRUCache_Clear tests Clear operation
func TestLRUCache_Clear(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize: 1024 * 1024,
		TTL:     time.Hour,
	})

	// Add multiple items
	for i := 0; i < 10; i++ {
		cache.Put("key", int64(i*100), []byte("data"))
	}

	if len(cache.items) != 10 {
		t.Errorf("expected 10 items, got %d", len(cache.items))
	}

	cache.Clear()

	if len(cache.items) != 0 {
		t.Errorf("expected 0 items after clear, got %d", len(cache.items))
	}
	if cache.Size() != 0 {
		t.Errorf("expected size 0 after clear, got %d", cache.Size())
	}
}

// TestLRUCache_ConcurrentAccess tests thread-safety
func TestLRUCache_ConcurrentAccess(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize:    10 * 1024 * 1024,
		MaxEntries: 1000,
		TTL:        time.Hour,
	})

	var wg sync.WaitGroup
	numGoroutines := 50
	numOpsPerGoroutine := 100

	// Concurrent writes
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOpsPerGoroutine; j++ {
				key := "key"
				offset := int64(id*numOpsPerGoroutine + j)
				data := []byte("data")
				cache.Put(key, offset, data)
			}
		}(i)
	}
	wg.Wait()

	// Concurrent reads
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOpsPerGoroutine; j++ {
				key := "key"
				offset := int64(id*numOpsPerGoroutine + j)
				cache.Get(key, offset, 4)
			}
		}(i)
	}
	wg.Wait()

	// No panics = success
	t.Log("Concurrent access test completed without panics")
}

// TestLRUCache_Stats tests statistics tracking
func TestLRUCache_Stats(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize:    1024,
		MaxEntries: 10,
		TTL:        time.Hour,
	})

	// Initial stats
	stats := cache.Stats()
	if stats.Hits != 0 || stats.Misses != 0 {
		t.Error("expected zero initial stats")
	}

	// Test miss first
	cache.Get("nonexistent", 0, 4) // Miss

	// Add some data
	cache.Put("key1", 0, []byte("data"))
	cache.Get("key1", 0, 4) // Hit

	stats = cache.Stats()
	if stats.Hits != 1 {
		t.Errorf("expected 1 hit, got %d", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("expected 1 miss, got %d", stats.Misses)
	}
	if stats.HitRate != 0.5 {
		t.Errorf("expected hit rate 0.5, got %f", stats.HitRate)
	}
	if stats.Size != 4 {
		t.Errorf("expected size 4, got %d", stats.Size)
	}
	if stats.Capacity != 1024 {
		t.Errorf("expected capacity 1024, got %d", stats.Capacity)
	}

	expectedUtilization := float64(4) / float64(1024)
	if stats.Utilization != expectedUtilization {
		t.Errorf("expected utilization %f, got %f", expectedUtilization, stats.Utilization)
	}
}

// TestLRUCache_Resize tests cache resize operation
func TestLRUCache_Resize(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize:    1000,
		MaxEntries: 100,
		TTL:        time.Hour,
	})

	// Fill cache with 500 bytes
	for i := 0; i < 5; i++ {
		cache.Put("key", int64(i*100), make([]byte, 100))
	}

	if cache.Size() != 500 {
		t.Errorf("expected size 500, got %d", cache.Size())
	}

	// Resize to 300 bytes (should trigger eviction)
	cache.Resize(300)

	if cache.capacity != 300 {
		t.Errorf("expected capacity 300, got %d", cache.capacity)
	}

	if cache.Size() > 300 {
		t.Errorf("size %d exceeds new capacity 300", cache.Size())
	}
}

// TestLRUCache_Evict tests manual eviction
func TestLRUCache_Evict(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize:    1024,
		MaxEntries: 100,
		TTL:        time.Hour,
	})

	// Add 500 bytes
	for i := 0; i < 5; i++ {
		cache.Put("key", int64(i*100), make([]byte, 100))
	}

	initialSize := cache.Size()
	if initialSize != 500 {
		t.Errorf("expected initial size 500, got %d", initialSize)
	}

	// Evict 200 bytes
	success := cache.Evict(200)
	if !success {
		t.Error("eviction should succeed")
	}

	if cache.Size() > 300 {
		t.Errorf("expected size <= 300 after evicting 200 bytes, got %d", cache.Size())
	}
}

// TestLRUCache_GetKeys tests GetKeys helper
func TestLRUCache_GetKeys(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize: 1024,
		TTL:     time.Hour,
	})

	cache.Put("key1", 0, []byte("data"))
	cache.Put("key2", 100, []byte("data"))
	cache.Put("key3", 200, []byte("data"))

	keys := cache.GetKeys()
	if len(keys) != 3 {
		t.Errorf("expected 3 keys, got %d", len(keys))
	}

	// Verify keys are present (order doesn't matter)
	keyMap := make(map[string]bool)
	for _, key := range keys {
		keyMap[key] = true
	}

	expectedKeys := []string{"key1:0:4", "key2:100:4", "key3:200:4"}
	for _, expected := range expectedKeys {
		if !keyMap[expected] {
			t.Errorf("expected key %q not found in result", expected)
		}
	}
}

// TestWeightedLRUCache_Creation tests weighted LRU cache creation
func TestWeightedLRUCache_Creation(t *testing.T) {
	cache := NewWeightedLRUCache(&CacheConfig{
		MaxSize: 1024 * 1024,
	})

	if cache == nil {
		t.Fatal("NewWeightedLRUCache returned nil")
	}
	if cache.config.EvictionPolicy != "weighted_lru" {
		t.Errorf("expected eviction policy weighted_lru, got %s", cache.config.EvictionPolicy)
	}
}

// TestWeightedLRUCache_EvictByWeight tests weight-based eviction
func TestWeightedLRUCache_EvictByWeight(t *testing.T) {
	cache := NewWeightedLRUCache(&CacheConfig{
		MaxSize:    1024,
		MaxEntries: 100,
		TTL:        time.Hour,
	})

	// Add items with different access patterns
	cache.Put("hot", 0, make([]byte, 100))  // Will be accessed frequently
	cache.Put("cold", 0, make([]byte, 100)) // Won't be accessed

	// Access "hot" item multiple times to increase its weight
	for i := 0; i < 10; i++ {
		cache.Get("hot", 0, 100)
		time.Sleep(10 * time.Millisecond)
	}

	// Now evict by weight
	success := cache.EvictByWeight(100)
	if !success {
		t.Error("weight-based eviction should succeed")
	}

	// "cold" item should be evicted (lower weight)
	if cache.Get("cold", 0, 100) != nil {
		t.Error("cold item should have been evicted")
	}

	// "hot" item should remain (higher weight)
	if cache.Get("hot", 0, 100) == nil {
		t.Error("hot item should still exist")
	}
}

// TestLRUCache_AccessTimeUpdate tests that access time is updated on Get
func TestLRUCache_AccessTimeUpdate(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize: 1024,
		TTL:     time.Hour,
	})

	cache.Put("key", 0, []byte("data"))

	// Get initial access time
	cacheKey := cache.makeCacheKey("key", 0, 4)
	cache.mu.RLock()
	item1 := cache.items[cacheKey]
	accessTime1 := item1.accessTime
	cache.mu.RUnlock()

	time.Sleep(50 * time.Millisecond)

	// Access the item
	cache.Get("key", 0, 4)

	// Check access time updated
	cache.mu.RLock()
	item2 := cache.items[cacheKey]
	accessTime2 := item2.accessTime
	cache.mu.RUnlock()

	if !accessTime2.After(accessTime1) {
		t.Error("access time should be updated on Get")
	}
}

// TestLRUCache_DataIsolation tests that returned data is a copy
func TestLRUCache_DataIsolation(t *testing.T) {
	cache := NewLRUCache(&CacheConfig{
		MaxSize: 1024,
		TTL:     time.Hour,
	})

	original := []byte("original data")
	cache.Put("key", 0, original)

	// Get data and modify it
	retrieved := cache.Get("key", 0, int64(len(original)))
	if retrieved == nil {
		t.Fatal("Get returned nil")
	}

	// Modify retrieved data
	retrieved[0] = 'X'

	// Get again and verify original is unchanged
	retrieved2 := cache.Get("key", 0, int64(len(original)))
	if retrieved2[0] != 'o' {
		t.Error("cached data was modified - should be isolated")
	}
}
