package cache

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestNewPersistentCache tests cache creation with various configurations
func TestNewPersistentCache(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name    string
		config  *PersistentCacheConfig
		wantErr bool
		verify  func(t *testing.T, cache *PersistentCache)
	}{
		{
			name:    "nil config uses defaults",
			config:  nil,
			wantErr: false,
			verify: func(t *testing.T, cache *PersistentCache) {
				if cache.maxSize != 10*1024*1024*1024 {
					t.Errorf("expected default max size 10GB, got %d", cache.maxSize)
				}
				if cache.config.TTL != 1*time.Hour {
					t.Errorf("expected default TTL 1h, got %v", cache.config.TTL)
				}
				if !cache.config.Compression {
					t.Error("expected compression enabled by default")
				}
			},
		},
		{
			name: "custom config applied",
			config: &PersistentCacheConfig{
				Directory:       tmpDir,
				MaxSize:         1024 * 1024, // 1MB
				TTL:             10 * time.Minute,
				Compression:     false,
				IndexFile:       "test-index.json",
				CleanupInterval: 5 * time.Minute,
				SyncInterval:    30 * time.Second,
			},
			wantErr: false,
			verify: func(t *testing.T, cache *PersistentCache) {
				if cache.maxSize != 1024*1024 {
					t.Errorf("expected max size 1MB, got %d", cache.maxSize)
				}
				if cache.config.TTL != 10*time.Minute {
					t.Errorf("expected TTL 10min, got %v", cache.config.TTL)
				}
				if cache.config.Compression {
					t.Error("expected compression disabled")
				}
				if cache.config.IndexFile != "test-index.json" {
					t.Errorf("expected index file test-index.json, got %s", cache.config.IndexFile)
				}
			},
		},
		{
			name: "zero values get defaults",
			config: &PersistentCacheConfig{
				Directory: tmpDir,
				MaxSize:   1024 * 1024,
				TTL:       time.Hour,
				// IndexFile, CleanupInterval, SyncInterval are zero - should get defaults
			},
			wantErr: false,
			verify: func(t *testing.T, cache *PersistentCache) {
				if cache.config.IndexFile != "cache-index.json" {
					t.Errorf("expected default index file, got %s", cache.config.IndexFile)
				}
				if cache.config.CleanupInterval != 10*time.Minute {
					t.Errorf("expected default cleanup interval, got %v", cache.config.CleanupInterval)
				}
				if cache.config.SyncInterval != time.Minute {
					t.Errorf("expected default sync interval, got %v", cache.config.SyncInterval)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, err := NewPersistentCache(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewPersistentCache() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil {
				if cache == nil {
					t.Fatal("NewPersistentCache returned nil without error")
				}
				if cache.index == nil {
					t.Error("cache index not initialized")
				}
				tt.verify(t, cache)
			}
		})
	}
}

// TestPersistentCache_PutGet tests basic Put and Get operations
func TestPersistentCache_PutGet(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory:   tmpDir,
		MaxSize:     10 * 1024 * 1024,
		TTL:         time.Hour,
		Compression: true,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	key := "test-object"
	offset := int64(0)
	data := []byte("hello persistent world")

	// Put data
	cache.Put(key, offset, data)

	// Get data
	retrieved := cache.Get(key, offset, int64(len(data)))
	if retrieved == nil {
		t.Fatal("Get returned nil for existing key")
	}
	if string(retrieved) != string(data) {
		t.Errorf("expected %q, got %q", string(data), string(retrieved))
	}

	// Verify file was created
	cacheKey := cache.makeCacheKey(key, offset, int64(len(data)))
	item := cache.index[cacheKey]
	if item == nil {
		t.Fatal("item not in index")
	}
	if _, err := os.Stat(item.FilePath); os.IsNotExist(err) {
		t.Error("cache file was not created")
	}

	// Verify stats
	stats := cache.Stats()
	if stats.Hits != 1 {
		t.Errorf("expected 1 hit, got %d", stats.Hits)
	}
}

// TestPersistentCache_GetMiss tests cache miss behavior
func TestPersistentCache_GetMiss(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

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

// TestPersistentCache_Compression tests compression functionality
func TestPersistentCache_Compression(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name        string
		compression bool
		data        []byte
	}{
		{
			name:        "with compression",
			compression: true,
			data:        []byte("this is a long repeating string that should compress well: " + string(make([]byte, 1000))),
		},
		{
			name:        "without compression",
			compression: false,
			data:        []byte("this is test data"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, err := NewPersistentCache(&PersistentCacheConfig{
				Directory:   filepath.Join(tmpDir, tt.name),
				MaxSize:     10 * 1024 * 1024,
				TTL:         time.Hour,
				Compression: tt.compression,
			})
			if err != nil {
				t.Fatalf("NewPersistentCache failed: %v", err)
			}

			key := "test"
			cache.Put(key, 0, tt.data)

			// Verify data can be retrieved correctly
			retrieved := cache.Get(key, 0, int64(len(tt.data)))
			if retrieved == nil {
				t.Fatal("Get returned nil")
			}
			if string(retrieved) != string(tt.data) {
				t.Error("retrieved data doesn't match original")
			}

			// Verify compression flag
			cacheKey := cache.makeCacheKey(key, 0, int64(len(tt.data)))
			item := cache.index[cacheKey]
			if item.Compressed != tt.compression {
				t.Errorf("expected compressed=%v, got %v", tt.compression, item.Compressed)
			}
		})
	}
}

// TestPersistentCache_TTLExpiration tests TTL-based expiration
func TestPersistentCache_TTLExpiration(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       100 * time.Millisecond, // Very short TTL
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

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
}

// TestPersistentCache_Delete tests Delete operation
func TestPersistentCache_Delete(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	// Add multiple items with same key prefix
	cache.Put("user:123", 0, []byte("data1"))
	cache.Put("user:123", 100, []byte("data2"))
	cache.Put("user:456", 0, []byte("data3"))

	if len(cache.index) != 3 {
		t.Errorf("expected 3 items, got %d", len(cache.index))
	}

	// Delete by key prefix
	cache.Delete("user:123")

	// Should have only user:456 left
	if len(cache.index) != 1 {
		t.Errorf("expected 1 item after delete, got %d", len(cache.index))
	}

	if cache.Get("user:123", 0, 5) != nil {
		t.Error("user:123:0 should be deleted")
	}
	if cache.Get("user:456", 0, 5) == nil {
		t.Error("user:456 should still exist")
	}
}

// TestPersistentCache_Eviction tests eviction when cache is full
func TestPersistentCache_Eviction(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   100, // Small cache
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	// Add items that exceed capacity
	for i := 0; i < 5; i++ {
		cache.Put("key", int64(i*100), make([]byte, 30))
		time.Sleep(10 * time.Millisecond) // Ensure different access times
	}

	// Cache should have evicted oldest items
	if cache.Size() > cache.maxSize {
		t.Errorf("cache size %d exceeds max size %d", cache.Size(), cache.maxSize)
	}

	// Oldest items should be evicted
	if cache.Get("key", 0, 30) != nil {
		t.Error("oldest item should have been evicted")
	}
}

// TestPersistentCache_EvictManual tests manual Evict operation
func TestPersistentCache_EvictManual(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	// Add some items
	for i := 0; i < 5; i++ {
		cache.Put("key", int64(i*100), make([]byte, 100))
		time.Sleep(10 * time.Millisecond)
	}

	initialSize := cache.Size()

	// Evict 200 bytes
	success := cache.Evict(200)
	if !success {
		t.Error("eviction should succeed")
	}

	finalSize := cache.Size()
	if finalSize > initialSize-200 {
		t.Errorf("expected to evict at least 200 bytes, freed %d", initialSize-finalSize)
	}
}

// TestPersistentCache_Clear tests Clear operation
func TestPersistentCache_Clear(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	// Add multiple items
	for i := 0; i < 10; i++ {
		cache.Put("key", int64(i*100), []byte("data"))
	}

	if len(cache.index) != 10 {
		t.Errorf("expected 10 items, got %d", len(cache.index))
	}

	cache.Clear()

	if len(cache.index) != 0 {
		t.Errorf("expected 0 items after clear, got %d", len(cache.index))
	}
	if cache.Size() != 0 {
		t.Errorf("expected size 0 after clear, got %d", cache.Size())
	}
}

// TestPersistentCache_Optimize tests Optimize operation
func TestPersistentCache_Optimize(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       100 * time.Millisecond, // Short TTL
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	// Add items
	cache.Put("key1", 0, []byte("data1"))
	cache.Put("key2", 0, []byte("data2"))
	cache.Put("key3", 0, []byte("data3"))

	// Wait for items to expire
	time.Sleep(150 * time.Millisecond)

	// Add fresh item
	cache.Put("key4", 0, []byte("data4"))

	initialCount := len(cache.index)
	if initialCount != 4 {
		t.Errorf("expected 4 items before optimize, got %d", initialCount)
	}

	// Optimize should remove expired items
	cache.Optimize()

	finalCount := len(cache.index)
	if finalCount != 1 {
		t.Errorf("expected 1 item after optimize (only key4), got %d", finalCount)
	}
}

// TestPersistentCache_IndexPersistence tests that index is saved and loaded
func TestPersistentCache_IndexPersistence(t *testing.T) {
	tmpDir := t.TempDir()

	// Create cache and add data
	cache1, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	cache1.Put("key1", 0, []byte("data1"))
	cache1.Put("key2", 100, []byte("data2"))

	// Force index save
	cache1.Optimize()

	// Create new cache with same directory (should load index)
	cache2, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed on reload: %v", err)
	}

	// Should be able to retrieve data from cache2
	retrieved := cache2.Get("key1", 0, 5)
	if retrieved == nil {
		t.Error("should be able to retrieve data after reload")
	}
	if string(retrieved) != "data1" {
		t.Errorf("expected 'data1', got %q", string(retrieved))
	}

	retrieved = cache2.Get("key2", 100, 5)
	if retrieved == nil {
		t.Error("should be able to retrieve key2 after reload")
	}
}

// TestPersistentCache_ChecksumValidation tests checksum verification
func TestPersistentCache_ChecksumValidation(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory:   tmpDir,
		MaxSize:     10 * 1024 * 1024,
		TTL:         time.Hour,
		Compression: false, // Easier to corrupt without compression
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	key := "test"
	data := []byte("test data")
	cache.Put(key, 0, data)

	// Get the cache file path
	cacheKey := cache.makeCacheKey(key, 0, int64(len(data)))
	item := cache.index[cacheKey]

	// Corrupt the file
	corruptData := []byte("corrupted")
	if err := os.WriteFile(item.FilePath, corruptData, 0600); err != nil {
		t.Fatalf("failed to corrupt file: %v", err)
	}

	// Get should return nil due to checksum mismatch
	retrieved := cache.Get(key, 0, int64(len(data)))
	if retrieved != nil {
		t.Error("should return nil for corrupted data")
	}
}

// TestPersistentCache_ConcurrentAccess tests thread-safety
func TestPersistentCache_ConcurrentAccess(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   50 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	var wg sync.WaitGroup
	numGoroutines := 20
	numOpsPerGoroutine := 50

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

// TestPersistentCache_Stats tests statistics tracking
func TestPersistentCache_Stats(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	// Initial stats
	stats := cache.Stats()
	if stats.Hits != 0 || stats.Misses != 0 {
		t.Error("expected zero initial stats")
	}

	// Test miss
	cache.Get("nonexistent", 0, 4)

	// Add data and hit
	cache.Put("key1", 0, []byte("data"))
	cache.Get("key1", 0, 4)

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
	if stats.Capacity != 1024 {
		t.Errorf("expected capacity 1024, got %d", stats.Capacity)
	}
}

// TestPersistentCache_EmptyData tests that empty data is ignored
func TestPersistentCache_EmptyData(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	cache.Put("test", 0, []byte{})
	cache.Put("test", 0, nil)

	if len(cache.index) != 0 {
		t.Error("expected empty cache after putting empty data")
	}
}

// TestPersistentCache_PathValidation tests security of path validation
func TestPersistentCache_PathValidation(t *testing.T) {
	tmpDir := t.TempDir()

	// Test that config with suspicious index file is rejected during load
	// First create a cache with malicious index file path
	_, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
		IndexFile: "../../../etc/passwd", // Path traversal attempt
	})

	// Should fail with path validation error
	if err == nil {
		t.Error("should reject path traversal in index file")
	}
	if err != nil && !filepath.IsAbs(err.Error()) {
		// Check error message contains path validation
		if err.Error() == "" || len(err.Error()) < 10 {
			t.Errorf("unexpected error: %v", err)
		}
	}

	// Verify no file was created outside cache directory
	parentDir := filepath.Dir(tmpDir)
	etcPath := filepath.Join(parentDir, "etc", "passwd")
	if _, err := os.Stat(etcPath); err == nil {
		t.Error("should not create file outside cache directory")
	}
}
