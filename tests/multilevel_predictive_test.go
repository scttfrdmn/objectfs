package tests

import (
	"testing"
	"time"

	"github.com/objectfs/objectfs/internal/cache"
)

func TestMultiLevelCacheWithPredictive(t *testing.T) {
	config := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       1024 * 1024, // 1MB
			MaxEntries: 1000,
			TTL:        5 * time.Minute,
			Prefetch:   true, // Enable predictive features
		},
		L2Config: &cache.L2Config{
			Enabled: false, // Disable L2 for this test
		},
		Policy: "inclusive",
	}

	mlCache, err := cache.NewMultiLevelCache(config)
	if err != nil {
		t.Fatalf("Failed to create multi-level cache: %v", err)
	}

	// Test basic functionality
	key := "test-key"
	data := []byte("test data for multilevel cache with predictive features")

	// Put data
	mlCache.Put(key, 0, data)

	// Get data back
	retrieved := mlCache.Get(key, 0, int64(len(data)))
	if retrieved == nil {
		t.Fatal("Expected to retrieve cached data")
	}

	if string(retrieved) != string(data) {
		t.Fatalf("Retrieved data mismatch: got %s, want %s", string(retrieved), string(data))
	}

	// Test sequential access pattern to trigger prediction
	blockSize := int64(256)
	numBlocks := 10

	for i := 0; i < numBlocks; i++ {
		blockKey := "sequential-block"
		offset := int64(i) * blockSize
		blockData := make([]byte, blockSize)

		// Fill with pattern data
		for j := range blockData {
			blockData[j] = byte(i)
		}

		mlCache.Put(blockKey, offset, blockData)

		// Read it back immediately
		retrieved := mlCache.Get(blockKey, offset, blockSize)
		if retrieved == nil {
			t.Fatalf("Failed to get sequential block %d", i)
		}

		if len(retrieved) != int(blockSize) {
			t.Fatalf("Block %d size mismatch: got %d, want %d", i, len(retrieved), blockSize)
		}
	}

	// Verify cache statistics
	stats := mlCache.Stats()
	if stats.Hits == 0 {
		t.Error("Expected some cache hits")
	}

	t.Logf("MultiLevel Cache Statistics:")
	t.Logf("  Total Hits: %d", stats.Hits)
	t.Logf("  Total Misses: %d", stats.Misses)
	t.Logf("  Hit Rate: %.2f%%", stats.HitRate*100)
	t.Logf("  Cache Size: %d bytes", stats.Size)
	t.Logf("  Utilization: %.2f%%", stats.Utilization*100)
}

func TestMultiLevelCache_LevelStats(t *testing.T) {
	config := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       512 * 1024, // 512KB
			MaxEntries: 100,
			TTL:        1 * time.Minute,
			Prefetch:   true,
		},
		Policy: "inclusive",
	}

	mlCache, err := cache.NewMultiLevelCache(config)
	if err != nil {
		t.Fatalf("Failed to create multi-level cache: %v", err)
	}

	// Get L1 statistics
	l1Stats, err := mlCache.GetLevelStats("L1")
	if err != nil {
		t.Fatalf("Failed to get L1 stats: %v", err)
	}

	t.Logf("L1 Cache initial stats:")
	t.Logf("  Capacity: %d bytes", l1Stats.Capacity)
	t.Logf("  Size: %d bytes", l1Stats.Size)

	// Add some data
	for i := 0; i < 50; i++ {
		key := "level-test-" + string(rune(i))
		data := make([]byte, 1024)
		for j := range data {
			data[j] = byte(i)
		}
		mlCache.Put(key, 0, data)
	}

	// Get updated stats
	l1Stats, err = mlCache.GetLevelStats("L1")
	if err != nil {
		t.Fatalf("Failed to get updated L1 stats: %v", err)
	}

	t.Logf("L1 Cache after data insertion:")
	t.Logf("  Size: %d bytes", l1Stats.Size)
	t.Logf("  Utilization: %.2f%%", l1Stats.Utilization*100)
}

func BenchmarkMultiLevelCache_PredictiveEnabled(b *testing.B) {
	config := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       2 * 1024 * 1024, // 2MB
			MaxEntries: 10000,
			TTL:        10 * time.Minute,
			Prefetch:   true, // Enable predictive features
		},
		Policy: "inclusive",
	}

	mlCache, err := cache.NewMultiLevelCache(config)
	if err != nil {
		b.Fatalf("Failed to create multi-level cache: %v", err)
	}

	// Prepare test data
	numKeys := 1000
	keys := make([]string, numKeys)
	blockSize := int64(1024)

	for i := 0; i < numKeys; i++ {
		keys[i] = "bench-key-" + string(rune(i))
		data := make([]byte, blockSize)
		for j := range data {
			data[j] = byte(i % 256)
		}
		mlCache.Put(keys[i], 0, data)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		keyIndex := i % numKeys
		data := mlCache.Get(keys[keyIndex], 0, blockSize)
		if data == nil {
			b.Fatalf("Failed to get key %s", keys[keyIndex])
		}
	}
}

func BenchmarkMultiLevelCache_PredictiveDisabled(b *testing.B) {
	config := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       2 * 1024 * 1024, // 2MB
			MaxEntries: 10000,
			TTL:        10 * time.Minute,
			Prefetch:   false, // Disable predictive features
		},
		Policy: "inclusive",
	}

	mlCache, err := cache.NewMultiLevelCache(config)
	if err != nil {
		b.Fatalf("Failed to create multi-level cache: %v", err)
	}

	// Prepare test data
	numKeys := 1000
	keys := make([]string, numKeys)
	blockSize := int64(1024)

	for i := 0; i < numKeys; i++ {
		keys[i] = "bench-key-" + string(rune(i))
		data := make([]byte, blockSize)
		for j := range data {
			data[j] = byte(i % 256)
		}
		mlCache.Put(keys[i], 0, data)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		keyIndex := i % numKeys
		data := mlCache.Get(keys[keyIndex], 0, blockSize)
		if data == nil {
			b.Fatalf("Failed to get key %s", keys[keyIndex])
		}
	}
}