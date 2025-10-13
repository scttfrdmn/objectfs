package cache

import (
	"testing"
	"time"
)

// TestNewMultiLevelCache tests cache creation with various configurations
func TestNewMultiLevelCache(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  *MultiLevelConfig
		wantErr bool
		verify  func(t *testing.T, cache *MultiLevelCache)
	}{
		{
			name:    "nil config uses defaults",
			config:  nil,
			wantErr: false,
			verify: func(t *testing.T, cache *MultiLevelCache) {
				t.Helper()
				if cache == nil {
					t.Fatal("cache is nil")
				}
				if cache.config.Policy != "inclusive" {
					t.Errorf("expected default policy inclusive, got %s", cache.config.Policy)
				}
				if cache.config.L1Config == nil {
					t.Error("L1 config should not be nil")
				}
				if !cache.config.L1Config.Enabled {
					t.Error("L1 should be enabled by default")
				}
			},
		},
		{
			name: "custom L1 only config",
			config: &MultiLevelConfig{
				L1Config: &L1Config{
					Enabled:    true,
					Size:       512 * 1024 * 1024, // 512MB
					MaxEntries: 50000,
					TTL:        10 * time.Minute,
					Prefetch:   false,
				},
				L2Config: &L2Config{
					Enabled: false,
				},
				Policy: "exclusive",
			},
			wantErr: false,
			verify: func(t *testing.T, cache *MultiLevelCache) {
				t.Helper()
				if len(cache.levels) != 1 {
					t.Errorf("expected 1 level (L1 only), got %d", len(cache.levels))
				}
				if cache.config.Policy != "exclusive" {
					t.Errorf("expected policy exclusive, got %s", cache.config.Policy)
				}
			},
		},
		{
			name: "L1 and L2 enabled",
			config: &MultiLevelConfig{
				L1Config: &L1Config{
					Enabled:    true,
					Size:       256 * 1024 * 1024,
					MaxEntries: 10000,
					TTL:        5 * time.Minute,
					Prefetch:   false,
				},
				L2Config: &L2Config{
					Enabled:     true,
					Size:        2 * 1024 * 1024 * 1024,
					Directory:   t.TempDir(),
					TTL:         30 * time.Minute,
					Compression: true,
				},
				Policy: "inclusive",
			},
			wantErr: false,
			verify: func(t *testing.T, cache *MultiLevelCache) {
				t.Helper()
				if len(cache.levels) != 2 {
					t.Errorf("expected 2 levels (L1 + L2), got %d", len(cache.levels))
				}
				if cache.levels[0].Name != "L1" {
					t.Errorf("first level should be L1, got %s", cache.levels[0].Name)
				}
				if cache.levels[1].Name != "L2" {
					t.Errorf("second level should be L2, got %s", cache.levels[1].Name)
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt // capture range variable
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cache, err := NewMultiLevelCache(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewMultiLevelCache() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil && cache != nil {
				tt.verify(t, cache)
			}
		})
	}
}

// TestMultiLevelCache_PutGet_L1Only tests basic operations with L1 cache only
func TestMultiLevelCache_PutGet_L1Only(t *testing.T) {
	t.Parallel()

	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	key := "test-key"
	offset := int64(0)
	data := []byte("test data for L1")

	// Put data
	cache.Put(key, offset, data)

	// Get data - should be in L1
	retrieved := cache.Get(key, offset, int64(len(data)))
	if retrieved == nil {
		t.Fatal("Get returned nil for existing key")
	}
	if string(retrieved) != string(data) {
		t.Errorf("expected %q, got %q", string(data), string(retrieved))
	}

	// Verify stats show hit
	stats := cache.Stats()
	if stats.Hits != 1 {
		t.Errorf("expected 1 hit, got %d", stats.Hits)
	}
}

// TestMultiLevelCache_PutGet_L1L2 tests operations with both cache levels
func TestMultiLevelCache_PutGet_L1L2(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       1 * 1024 * 1024,
			MaxEntries: 100,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        10 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: true,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	key := "test-key"
	offset := int64(0)
	data := []byte("test data for L1 and L2")

	// Put data - should be in both L1 and L2 (inclusive policy)
	cache.Put(key, offset, data)

	// Get data
	retrieved := cache.Get(key, offset, int64(len(data)))
	if retrieved == nil {
		t.Fatal("Get returned nil for existing key")
	}
	if string(retrieved) != string(data) {
		t.Errorf("expected %q, got %q", string(data), string(retrieved))
	}
}

// TestMultiLevelCache_GetMiss tests cache miss at all levels
func TestMultiLevelCache_GetMiss(t *testing.T) {
	t.Parallel()

	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       1 * 1024 * 1024,
			MaxEntries: 100,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Get non-existent key
	retrieved := cache.Get("nonexistent", 0, 100)
	if retrieved != nil {
		t.Error("expected nil for non-existent key")
	}

	// Verify miss was recorded
	stats := cache.Stats()
	if stats.Misses != 1 {
		t.Errorf("expected 1 miss, got %d", stats.Misses)
	}
}

// TestMultiLevelCache_Promotion tests cache promotion from L2 to L1
func TestMultiLevelCache_Promotion(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       100, // Very small L1
			MaxEntries: 2,   // Max 2 entries
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        10 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Put data in both levels
	key1 := "key1"
	data1 := []byte("d1")
	cache.Put(key1, 0, data1)

	// Clear L1 by adding more items (will evict key1 from L1)
	cache.Put("key2", 0, []byte("d2"))
	cache.Put("key3", 0, []byte("d3"))

	// Get key1 - should hit L2 and promote back to L1
	retrieved := cache.Get(key1, 0, int64(len(data1)))
	if retrieved == nil {
		t.Fatal("Get returned nil, expected data from L2")
	}
	if string(retrieved) != string(data1) {
		t.Errorf("expected %q, got %q", string(data1), string(retrieved))
	}
}

// TestMultiLevelCache_PolicyInclusive tests inclusive cache policy
func TestMultiLevelCache_PolicyInclusive(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	key := "test"
	data := []byte("data")
	cache.Put(key, 0, data)

	// With inclusive policy, data should be in both levels
	// Check by getting level-specific stats
	l1Stats, err := cache.GetLevelStats("L1")
	if err != nil {
		t.Fatalf("failed to get L1 stats: %v", err)
	}
	l2Stats, err := cache.GetLevelStats("L2")
	if err != nil {
		t.Fatalf("failed to get L2 stats: %v", err)
	}

	// Both should have data (size > 0)
	if l1Stats.Size == 0 {
		t.Error("L1 should have data with inclusive policy")
	}
	if l2Stats.Size == 0 {
		t.Error("L2 should have data with inclusive policy")
	}
}

// TestMultiLevelCache_PolicyExclusive tests exclusive cache policy
func TestMultiLevelCache_PolicyExclusive(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "exclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	key := "test"
	data := []byte("data")
	cache.Put(key, 0, data)

	// With exclusive policy, data should only be in L1
	l1Stats, err := cache.GetLevelStats("L1")
	if err != nil {
		t.Fatalf("failed to get L1 stats: %v", err)
	}
	l2Stats, err := cache.GetLevelStats("L2")
	if err != nil {
		t.Fatalf("failed to get L2 stats: %v", err)
	}

	// L1 should have data, L2 should be empty
	if l1Stats.Size == 0 {
		t.Error("L1 should have data with exclusive policy")
	}
	if l2Stats.Size != 0 {
		t.Error("L2 should be empty with exclusive policy on Put")
	}
}

// TestMultiLevelCache_PolicyHybrid tests hybrid cache policy
func TestMultiLevelCache_PolicyHybrid(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "hybrid",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Small data - should only go to L1
	smallKey := "small"
	smallData := []byte("small data")
	cache.Put(smallKey, 0, smallData)

	// Large data (>1MB) - should go to both L1 and L2
	largeKey := "large"
	largeData := make([]byte, 2*1024*1024) // 2MB
	cache.Put(largeKey, 0, largeData)

	// Verify data can be retrieved
	if cache.Get(smallKey, 0, int64(len(smallData))) == nil {
		t.Error("should be able to retrieve small data")
	}
	if cache.Get(largeKey, 0, int64(len(largeData))) == nil {
		t.Error("should be able to retrieve large data")
	}
}

// TestMultiLevelCache_Delete tests deletion across all levels
func TestMultiLevelCache_Delete(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	key := "test"
	data := []byte("data")
	cache.Put(key, 0, data)

	// Verify it exists
	if cache.Get(key, 0, int64(len(data))) == nil {
		t.Fatal("data should exist before delete")
	}

	// Delete
	cache.Delete(key)

	// Verify it's gone
	if cache.Get(key, 0, int64(len(data))) != nil {
		t.Error("data should not exist after delete")
	}
}

// TestMultiLevelCache_Evict tests eviction across levels
func TestMultiLevelCache_Evict(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       1024,
			MaxEntries: 100,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        10 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Add some data
	for i := 0; i < 5; i++ {
		cache.Put("key", int64(i*100), make([]byte, 100))
	}

	initialSize := cache.Size()
	if initialSize == 0 {
		t.Fatal("cache should have data")
	}

	// Evict some data
	success := cache.Evict(200)
	if !success {
		t.Error("eviction should succeed")
	}

	finalSize := cache.Size()
	if finalSize >= initialSize {
		t.Error("cache size should decrease after eviction")
	}
}

// TestMultiLevelCache_Size tests size calculation across levels
func TestMultiLevelCache_Size(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	initialSize := cache.Size()
	if initialSize != 0 {
		t.Errorf("expected initial size 0, got %d", initialSize)
	}

	// Add data
	data := make([]byte, 1000)
	cache.Put("test", 0, data)

	newSize := cache.Size()
	if newSize == 0 {
		t.Error("size should be > 0 after adding data")
	}
}

// TestMultiLevelCache_Stats tests statistics aggregation
func TestMultiLevelCache_Stats(t *testing.T) {
	t.Parallel()

	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Initial stats
	stats := cache.Stats()
	if stats.Hits != 0 || stats.Misses != 0 {
		t.Error("expected zero initial stats")
	}

	// Add data and access
	cache.Put("key", 0, []byte("data"))
	cache.Get("key", 0, 4)         // Hit
	cache.Get("nonexistent", 0, 4) // Miss

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
}

// TestMultiLevelCache_GetLevelStats tests level-specific statistics
func TestMultiLevelCache_GetLevelStats(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Get L1 stats
	l1Stats, err := cache.GetLevelStats("L1")
	if err != nil {
		t.Errorf("failed to get L1 stats: %v", err)
	}
	if l1Stats.Capacity == 0 {
		t.Error("L1 capacity should be > 0")
	}

	// Get L2 stats
	l2Stats, err := cache.GetLevelStats("L2")
	if err != nil {
		t.Errorf("failed to get L2 stats: %v", err)
	}
	if l2Stats.Capacity == 0 {
		t.Error("L2 capacity should be > 0")
	}

	// Get stats for non-existent level
	_, err = cache.GetLevelStats("L3")
	if err == nil {
		t.Error("expected error for non-existent level")
	}
}

// TestMultiLevelCache_EnableDisableLevel tests enabling/disabling cache levels
func TestMultiLevelCache_EnableDisableLevel(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Disable L2
	err = cache.DisableLevel("L2")
	if err != nil {
		t.Errorf("failed to disable L2: %v", err)
	}

	// Verify L2 is disabled
	cache.mu.RLock()
	l2Enabled := cache.levels[1].Enabled
	cache.mu.RUnlock()
	if l2Enabled {
		t.Error("L2 should be disabled")
	}

	// Re-enable L2
	err = cache.EnableLevel("L2")
	if err != nil {
		t.Errorf("failed to enable L2: %v", err)
	}

	// Verify L2 is enabled
	cache.mu.RLock()
	l2Enabled = cache.levels[1].Enabled
	cache.mu.RUnlock()
	if !l2Enabled {
		t.Error("L2 should be enabled")
	}

	// Try to enable non-existent level
	err = cache.EnableLevel("L99")
	if err == nil {
		t.Error("expected error when enabling non-existent level")
	}
}

// TestMultiLevelCache_ClearLevel tests clearing a specific cache level
func TestMultiLevelCache_ClearLevel(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Add data
	cache.Put("test", 0, []byte("data"))

	// Clear L1
	err = cache.ClearLevel("L1")
	if err != nil {
		t.Errorf("failed to clear L1: %v", err)
	}

	// Verify L1 is empty
	l1Stats, _ := cache.GetLevelStats("L1")
	if l1Stats.Size != 0 {
		t.Error("L1 should be empty after clear")
	}

	// Try to clear non-existent level
	err = cache.ClearLevel("L99")
	if err == nil {
		t.Error("expected error when clearing non-existent level")
	}
}

// TestMultiLevelCache_Optimize tests cache optimization
func TestMultiLevelCache_Optimize(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Add data
	cache.Put("test", 0, []byte("data"))

	// Access to create hit ratio
	cache.Get("test", 0, 4)

	// Run optimization - should not panic
	cache.Optimize()

	// Verify efficiency metrics are set
	cache.statsMu.Lock()
	efficiency := cache.stats.Efficiency
	cache.statsMu.Unlock()

	if efficiency == 0 {
		t.Error("efficiency should be set after optimization")
	}
}

// TestMultiLevelCache_Warmup tests cache warmup functionality
func TestMultiLevelCache_Warmup(t *testing.T) {
	t.Parallel()

	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Warmup is currently a placeholder, should not error
	keys := []string{"key1", "key2", "key3"}
	err = cache.Warmup(keys)
	if err != nil {
		t.Errorf("Warmup returned error: %v", err)
	}
}

// TestMultiLevelCache_L2CacheError tests handling of L2 cache creation errors
func TestMultiLevelCache_L2CacheError(t *testing.T) {
	t.Parallel()

	// Try to create cache with invalid L2 directory
	_, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024,
			Directory:   "/invalid/path/that/does/not/exist",
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "inclusive",
	})

	// Should fail to create L2 cache
	if err == nil {
		t.Error("expected error when creating cache with invalid L2 directory")
	}
}

// TestMultiLevelCache_EmptyData tests handling of empty data
func TestMultiLevelCache_EmptyData(t *testing.T) {
	t.Parallel()

	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled: false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Put empty data - should be handled gracefully
	cache.Put("empty", 0, []byte{})
	cache.Put("nil", 0, nil)

	// Size should remain 0 or minimal
	size := cache.Size()
	if size != 0 {
		t.Logf("Cache size is %d after empty data (may be non-zero due to metadata)", size)
	}
}

// TestMultiLevelCache_DefaultPolicy tests default policy handling
func TestMultiLevelCache_DefaultPolicy(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       10 * 1024 * 1024,
			MaxEntries: 1000,
			TTL:        time.Hour,
			Prefetch:   false,
		},
		L2Config: &L2Config{
			Enabled:     true,
			Size:        100 * 1024 * 1024,
			Directory:   tmpDir,
			TTL:         time.Hour,
			Compression: false,
		},
		Policy: "invalid-policy", // Invalid policy should fall back to default (inclusive)
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache failed: %v", err)
	}

	// Put data - should use default inclusive behavior
	cache.Put("test", 0, []byte("data"))

	// Should be able to retrieve data
	retrieved := cache.Get("test", 0, 4)
	if retrieved == nil {
		t.Error("should be able to retrieve data with default policy")
	}
}
