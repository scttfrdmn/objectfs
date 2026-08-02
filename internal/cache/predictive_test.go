package cache

import (
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// lruMinimalCache wraps NewLRUCache to provide a concrete types.Cache for tests.
type lruMinimalCache struct {
	c *LRUCache
}

func newLRUMinimal() *lruMinimalCache {
	return &lruMinimalCache{
		c: NewLRUCache(&CacheConfig{
			MaxSize:    64 * 1024 * 1024, // 64 MiB
			MaxEntries: 1000,
		}),
	}
}

func (l *lruMinimalCache) Get(key string, offset, size int64) []byte {
	return l.c.Get(key, offset, size)
}

func (l *lruMinimalCache) Put(key string, offset int64, data []byte) {
	l.c.Put(key, offset, data)
}

func (l *lruMinimalCache) Delete(key string)       { l.c.Delete(key) }
func (l *lruMinimalCache) Evict(size int64) bool   { return l.c.Evict(size) }
func (l *lruMinimalCache) Size() int64             { return l.c.Size() }
func (l *lruMinimalCache) Stats() types.CacheStats { return l.c.Stats() }

func predictiveCacheConfig() *PredictiveCacheConfig {
	return &PredictiveCacheConfig{
		BaseCache:                 newLRUMinimal(),
		EnablePrediction:          true,
		PredictionWindow:          100,
		ConfidenceThreshold:       0.7,
		LearningRate:              0.01,
		EnablePrefetch:            false, // no background workers in unit tests
		MaxConcurrentFetch:        1,
		PrefetchAhead:             3,
		PrefetchBandwidth:         10 * 1024 * 1024,
		EnableIntelligentEviction: true,
		EvictionAlgorithm:         "ml",
		StatisticsInterval:        30 * time.Second,
		ModelUpdateInterval:       5 * time.Minute,
		PatternAnalysisDepth:      1000,
	}
}

// TestGenerateEvictionCandidates_PopulatedPatterns verifies that after
// recording accesses, generateEvictionCandidates returns real candidates.
func TestGenerateEvictionCandidates_PopulatedPatterns(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(predictiveCacheConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}

	// Record 5 sequential accesses for "key-a".
	now := time.Now()
	for i := range 5 {
		pc.predictor.RecordAccess(AccessEvent{
			Key:       "key-a",
			Offset:    int64(i) * 128 * 1024,
			Size:      128 * 1024,
			Timestamp: now.Add(time.Duration(i) * time.Second),
			Hit:       true,
		})
	}

	candidates := pc.evictionMgr.generateEvictionCandidates()
	if len(candidates) == 0 {
		t.Fatal("expected at least one eviction candidate after recording accesses, got none")
	}

	found := false
	for _, c := range candidates {
		if c.Key == "key-a" {
			found = true
			if c.AccessCount == 0 {
				t.Error("AccessCount should be > 0")
			}
			if c.EvictionScore < 0 || c.EvictionScore > 1.0 {
				t.Errorf("EvictionScore %f out of [0,1]", c.EvictionScore)
			}
		}
	}
	if !found {
		t.Error("key-a not found in eviction candidates")
	}
}

// TestGenerateEvictionCandidates_Empty verifies empty result when no patterns exist.
func TestGenerateEvictionCandidates_Empty(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(predictiveCacheConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}

	if candidates := pc.evictionMgr.generateEvictionCandidates(); len(candidates) != 0 {
		t.Errorf("expected empty candidates with no patterns, got %d", len(candidates))
	}
}

// TestPredictiveCache_SequentialPrefetch verifies that sequential byte-offset
// accesses cause PredictNextAccess to return the next-block prefetch candidate.
func TestPredictiveCache_SequentialPrefetch(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(predictiveCacheConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}

	const blockSize = int64(128 * 1024) // 128 KiB
	now := time.Now()

	// Simulate 10 sequential block reads of "data/file.dat".
	for i := range 10 {
		pc.predictor.RecordAccess(AccessEvent{
			Key:       "data/file.dat",
			Offset:    int64(i) * blockSize,
			Size:      blockSize,
			Timestamp: now.Add(time.Duration(i) * 100 * time.Millisecond),
		})
	}

	candidates := pc.predictor.PredictNextAccess("data/file.dat")
	if len(candidates) == 0 {
		t.Fatal("expected prefetch candidates after 10 sequential accesses, got none")
	}

	// The first candidate should be the next sequential block at offset 10*blockSize.
	expectedOffset := int64(10) * blockSize
	found := false
	for _, c := range candidates {
		if c.Path == "data/file.dat" && c.Offset == expectedOffset {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected candidate for data/file.dat at offset %d; got %+v",
			expectedOffset, candidates)
	}
}

// TestGenerateEvictionCandidates_EvictionScoreOrder verifies that least-recently
// used items have a higher eviction score than recently-used items.
func TestGenerateEvictionCandidates_EvictionScoreOrder(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(predictiveCacheConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}

	now := time.Now()

	// "recent-key": accessed 1 second ago
	pc.predictor.RecordAccess(AccessEvent{
		Key: "recent-key", Size: 1024,
		Timestamp: now.Add(-1 * time.Second),
	})

	// "stale-key": last accessed 25 hours ago, so recency decays to near zero
	for i := range 3 {
		pc.predictor.RecordAccess(AccessEvent{
			Key:       "stale-key",
			Size:      1024,
			Timestamp: now.Add(-25*time.Hour - time.Duration(i)*time.Hour),
		})
	}

	candidates := pc.evictionMgr.generateEvictionCandidates()
	scores := make(map[string]float64)
	for _, c := range candidates {
		scores[c.Key] = c.EvictionScore
	}

	if _, ok := scores["stale-key"]; !ok {
		t.Fatal("stale-key not in candidates")
	}
	if _, ok := scores["recent-key"]; !ok {
		t.Fatal("recent-key not in candidates")
	}
	if scores["stale-key"] <= scores["recent-key"] {
		t.Errorf("stale-key eviction score (%f) should be > recent-key (%f)",
			scores["stale-key"], scores["recent-key"])
	}
}
