package cache_test

import (
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/cache/cachetest"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// The four in-process implementations, each against the same contract. See internal/cache/cachetest
// for why the suite is shared rather than per-implementation.
//
// Sizes are generous relative to the suite's ten-byte payloads: an eviction mid-subtest would look
// like a contract violation and would be one of the few failures here that is the test's fault rather
// than the cache's.

func TestLRUCacheHonorsTheCacheContract(t *testing.T) {
	t.Parallel()

	cachetest.RunContract(t, func(t *testing.T) types.Cache {
		return cache.NewLRUCache(&cache.CacheConfig{
			MaxSize:    1 << 20,
			MaxEntries: 1000,
			TTL:        time.Hour,
		})
	})
}

func TestPersistentCacheHonorsTheCacheContract(t *testing.T) {
	t.Parallel()

	cachetest.RunContract(t, func(t *testing.T) types.Cache {
		// Compression on, which is what the mount path sets unconditionally: adapter.Start builds its
		// L2Config with Compression: true and no way to turn it off. A conformance run against the
		// uncompressed path would not be testing the code any mount uses.
		c, err := cache.NewPersistentCache(&cache.PersistentCacheConfig{
			Directory:   t.TempDir(),
			MaxSize:     1 << 20,
			TTL:         time.Hour,
			Compression: true,
		})
		if err != nil {
			t.Fatalf("NewPersistentCache: %v", err)
		}

		t.Cleanup(func() { _ = c.Close() })

		return c
	})
}

func TestMultiLevelCacheHonorsTheCacheContract(t *testing.T) {
	t.Parallel()

	cachetest.RunContract(t, func(t *testing.T) types.Cache {
		// Both levels on, as on a mount with persistent_cache.enabled — a promotion between them is the
		// one path where a partial answer could be assembled from two sources, so a single-level
		// configuration would miss the interesting case.
		c, err := cache.NewMultiLevelCache(&cache.MultiLevelConfig{
			L1Config: &cache.L1Config{
				Enabled:    true,
				Size:       1 << 20,
				MaxEntries: 1000,
				TTL:        time.Hour,
			},
			L2Config: &cache.L2Config{
				Enabled:     true,
				Size:        1 << 20,
				Directory:   t.TempDir(),
				TTL:         time.Hour,
				Compression: true,
			},
			Policy: "weighted_lru",
		})
		if err != nil {
			t.Fatalf("NewMultiLevelCache: %v", err)
		}

		t.Cleanup(func() { _ = c.Close() })

		return c
	})
}

func TestPredictiveCacheHonorsTheCacheContract(t *testing.T) {
	t.Parallel()

	cachetest.RunContract(t, func(t *testing.T) types.Cache {
		// No Backend, so nothing can prefetch: a prefetch would issue a GET against a nil backend, and
		// the contract this suite checks is about what Get returns for bytes already held. Prediction
		// stays on because it runs on the Get path and must not change the answer.
		c, err := cache.NewPredictiveCache(&cache.PredictiveCacheConfig{
			BaseCache: cache.NewLRUCache(&cache.CacheConfig{
				MaxSize:    1 << 20,
				MaxEntries: 1000,
				TTL:        time.Hour,
			}),
			EnablePrediction:    true,
			PredictionWindow:    100,
			ConfidenceThreshold: 0.7,
		})
		if err != nil {
			t.Fatalf("NewPredictiveCache: %v", err)
		}

		t.Cleanup(func() { _ = c.Close() })

		return c
	})
}
