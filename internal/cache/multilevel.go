package cache

import (
	"fmt"
	"sync"
	"time"

	"github.com/objectfs/objectfs/pkg/types"
)

// MultiLevelCache implements a multi-level cache hierarchy
type MultiLevelCache struct {
	mu      sync.RWMutex
	statsMu sync.Mutex
	levels  []CacheLevel
	config  *MultiLevelConfig
	stats   MultiLevelStats
}

// CacheLevel represents a single level in the cache hierarchy
type CacheLevel struct {
	Name     string
	Cache    types.Cache
	Priority int
	Enabled  bool
}

// MultiLevelConfig represents multi-level cache configuration
type MultiLevelConfig struct {
	L1Config *L1Config `yaml:"l1"`
	L2Config *L2Config `yaml:"l2"`
	Policy   string    `yaml:"policy"`
}

// L1Config represents L1 (memory) cache configuration
type L1Config struct {
	Enabled    bool          `yaml:"enabled"`
	Size       int64         `yaml:"size"`
	MaxEntries int           `yaml:"max_entries"`
	TTL        time.Duration `yaml:"ttl"`
	Prefetch   bool          `yaml:"prefetch"`
}

// L2Config represents L2 (persistent) cache configuration
type L2Config struct {
	Enabled     bool          `yaml:"enabled"`
	Size        int64         `yaml:"size"`
	Directory   string        `yaml:"directory"`
	TTL         time.Duration `yaml:"ttl"`
	Compression bool          `yaml:"compression"`
}

// MultiLevelStats tracks multi-level cache statistics
type MultiLevelStats struct {
	TotalHits   uint64                      `json:"total_hits"`
	TotalMisses uint64                      `json:"total_misses"`
	LevelStats  map[string]types.CacheStats `json:"level_stats"`
	HitRatio    float64                     `json:"hit_ratio"`
	Efficiency  float64                     `json:"efficiency"`
}

// NewMultiLevelCache creates a new multi-level cache
func NewMultiLevelCache(config *MultiLevelConfig) (*MultiLevelCache, error) {
	if config == nil {
		config = &MultiLevelConfig{
			L1Config: &L1Config{
				Enabled:    true,
				Size:       1 * 1024 * 1024 * 1024, // 1GB
				MaxEntries: 100000,
				TTL:        5 * time.Minute,
				Prefetch:   true,
			},
			L2Config: &L2Config{
				Enabled:     false,
				Size:        10 * 1024 * 1024 * 1024, // 10GB
				Directory:   "/tmp/objectfs-cache",
				TTL:         1 * time.Hour,
				Compression: true,
			},
			Policy: "inclusive",
		}
	}

	cache := &MultiLevelCache{
		config: config,
		stats: MultiLevelStats{
			LevelStats: make(map[string]types.CacheStats),
		},
	}

	// Initialize cache levels
	if err := cache.initializeLevels(); err != nil {
		return nil, fmt.Errorf("failed to initialize cache levels: %w", err)
	}

	return cache, nil
}

// Get retrieves data from the cache hierarchy
func (c *MultiLevelCache) Get(key string, offset, size int64) []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Try each level in order
	for i, level := range c.levels {
		if !level.Enabled {
			continue
		}

		data := level.Cache.Get(key, offset, size)
		if data != nil {
			// Cache hit at this level
			c.recordHit(level.Name)

			// Promote to higher levels (if not already at L1)
			if i > 0 {
				c.promoteToHigherLevels(key, offset, data, i-1)
			}

			return data
		}
	}

	// Cache miss at all levels
	c.recordMiss()
	return nil
}

// Put stores data in the cache hierarchy
func (c *MultiLevelCache) Put(key string, offset int64, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Store in all enabled levels based on policy
	switch c.config.Policy {
	case "inclusive":
		// Store in all levels
		for _, level := range c.levels {
			if level.Enabled {
				level.Cache.Put(key, offset, data)
			}
		}
	case "exclusive":
		// Store only in L1, evicted items go to L2
		if len(c.levels) > 0 && c.levels[0].Enabled {
			c.levels[0].Cache.Put(key, offset, data)
		}
	case "hybrid":
		// Store in L1, selectively promote to L2 based on access patterns
		c.hybridPut(key, offset, data)
	default:
		// Default to inclusive
		for _, level := range c.levels {
			if level.Enabled {
				level.Cache.Put(key, offset, data)
			}
		}
	}
}

// Delete removes data from all cache levels
func (c *MultiLevelCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, level := range c.levels {
		if level.Enabled {
			level.Cache.Delete(key)
		}
	}
}

// Evict evicts data from cache levels to free space
func (c *MultiLevelCache) Evict(size int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	totalEvicted := int64(0)

	// Try to evict from each level
	for _, level := range c.levels {
		if !level.Enabled {
			continue
		}

		if level.Cache.Evict(size - totalEvicted) {
			totalEvicted = size
			break
		}

		// If partial eviction, continue with remaining size
		levelStats := level.Cache.Stats()
		totalEvicted += levelStats.Size
	}

	return totalEvicted >= size
}

// Size returns total size across all cache levels
func (c *MultiLevelCache) Size() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	totalSize := int64(0)
	for _, level := range c.levels {
		if level.Enabled {
			totalSize += level.Cache.Size()
		}
	}

	return totalSize
}

// Stats returns combined statistics from all cache levels
func (c *MultiLevelCache) Stats() types.CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	combined := types.CacheStats{}

	for _, level := range c.levels {
		if !level.Enabled {
			continue
		}

		levelStats := level.Cache.Stats()
		func() {
			c.statsMu.Lock()
			defer c.statsMu.Unlock()
			c.stats.LevelStats[level.Name] = levelStats
		}()

		combined.Hits += levelStats.Hits
		combined.Misses += levelStats.Misses
		combined.Evictions += levelStats.Evictions
		combined.Size += levelStats.Size
		combined.Capacity += levelStats.Capacity
	}

	// Calculate overall hit rate
	total := combined.Hits + combined.Misses
	if total > 0 {
		combined.HitRate = float64(combined.Hits) / float64(total)
	}

	// Calculate utilization
	if combined.Capacity > 0 {
		combined.Utilization = float64(combined.Size) / float64(combined.Capacity)
	}

	return combined
}

// GetLevelStats returns statistics for a specific cache level
func (c *MultiLevelCache) GetLevelStats(levelName string) (types.CacheStats, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, level := range c.levels {
		if level.Name == levelName && level.Enabled {
			return level.Cache.Stats(), nil
		}
	}

	return types.CacheStats{}, fmt.Errorf("cache level %s not found or not enabled", levelName)
}

// Warmup preloads frequently accessed data
func (c *MultiLevelCache) Warmup(keys []string) error {
	// This would typically be implemented with knowledge of the backend
	// For now, this is a placeholder
	return nil
}

// Optimize runs cache optimization routines
func (c *MultiLevelCache) Optimize() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Run optimization for each level
	for _, level := range c.levels {
		if !level.Enabled {
			continue
		}

		// If the cache supports optimization, call it
		if optimizer, ok := level.Cache.(CacheOptimizer); ok {
			optimizer.Optimize()
		}
	}

	// Update efficiency metrics
	c.updateEfficiencyMetrics()
}

// Helper methods

func (c *MultiLevelCache) initializeLevels() error {
	c.levels = make([]CacheLevel, 0, 2)

	// Initialize L1 (memory) cache
	if c.config.L1Config != nil && c.config.L1Config.Enabled {
		l1Cache := NewLRUCache(&CacheConfig{
			MaxSize:    c.config.L1Config.Size,
			MaxEntries: c.config.L1Config.MaxEntries,
			TTL:        c.config.L1Config.TTL,
		})

		// Wrap with predictive cache if prefetch is enabled
		var finalCache types.Cache = l1Cache
		if c.config.L1Config.Prefetch {
			predictiveConfig := &PredictiveCacheConfig{
				BaseCache:                 l1Cache,
				EnablePrediction:          true,
				PredictionWindow:          100,
				ConfidenceThreshold:       0.7,
				LearningRate:              0.01,
				EnablePrefetch:            true,
				MaxConcurrentFetch:        4,
				PrefetchAhead:             3,
				PrefetchBandwidth:         10 * 1024 * 1024, // 10 MB/s
				EnableIntelligentEviction: true,
				EvictionAlgorithm:         "ml",
				StatisticsInterval:        30 * time.Second,
				ModelUpdateInterval:       5 * time.Minute,
				PatternAnalysisDepth:      1000,
			}

			predictiveCache, err := NewPredictiveCache(predictiveConfig)
			if err != nil {
				return fmt.Errorf("failed to create predictive cache: %w", err)
			}
			finalCache = predictiveCache
		}

		c.levels = append(c.levels, CacheLevel{
			Name:     "L1",
			Cache:    finalCache,
			Priority: 1,
			Enabled:  true,
		})
	}

	// Initialize L2 (persistent) cache if enabled
	if c.config.L2Config != nil && c.config.L2Config.Enabled {
		l2Cache, err := NewPersistentCache(&PersistentCacheConfig{
			Directory:   c.config.L2Config.Directory,
			MaxSize:     c.config.L2Config.Size,
			TTL:         c.config.L2Config.TTL,
			Compression: c.config.L2Config.Compression,
		})
		if err != nil {
			return fmt.Errorf("failed to create L2 cache: %w", err)
		}

		c.levels = append(c.levels, CacheLevel{
			Name:     "L2",
			Cache:    l2Cache,
			Priority: 2,
			Enabled:  true,
		})
	}

	return nil
}

func (c *MultiLevelCache) recordHit(levelName string) {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	c.stats.TotalHits++
	c.updateHitRatioUnsafe()
}

func (c *MultiLevelCache) recordMiss() {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	c.stats.TotalMisses++
	c.updateHitRatioUnsafe()
}

func (c *MultiLevelCache) updateHitRatioUnsafe() {
	total := c.stats.TotalHits + c.stats.TotalMisses
	if total > 0 {
		c.stats.HitRatio = float64(c.stats.TotalHits) / float64(total)
	}
}

func (c *MultiLevelCache) promoteToHigherLevels(key string, offset int64, data []byte, toLevel int) {
	// Promote data to higher levels
	for i := 0; i <= toLevel; i++ {
		if i < len(c.levels) && c.levels[i].Enabled {
			c.levels[i].Cache.Put(key, offset, data)
		}
	}
}

func (c *MultiLevelCache) hybridPut(key string, offset int64, data []byte) {
	// Store in L1 first
	if len(c.levels) > 0 && c.levels[0].Enabled {
		c.levels[0].Cache.Put(key, offset, data)
	}

	// Decide whether to store in L2 based on access patterns
	// This is a simplified heuristic - in practice, you'd use more sophisticated ML models
	if c.shouldPromoteToL2(key, data) && len(c.levels) > 1 && c.levels[1].Enabled {
		c.levels[1].Cache.Put(key, offset, data)
	}
}

func (c *MultiLevelCache) shouldPromoteToL2(key string, data []byte) bool {
	// Simple heuristic: promote larger files or frequently accessed files
	// In practice, this would use access patterns, ML models, etc.

	// Promote files larger than 1MB
	if len(data) > 1024*1024 {
		return true
	}

	// Promote based on access frequency (placeholder logic)
	// This would typically track access patterns over time
	return false
}

func (c *MultiLevelCache) updateEfficiencyMetrics() {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	// Calculate cache efficiency based on hit ratios and performance
	if c.stats.HitRatio > 0.8 {
		c.stats.Efficiency = 1.0
	} else if c.stats.HitRatio > 0.6 {
		c.stats.Efficiency = 0.8
	} else if c.stats.HitRatio > 0.4 {
		c.stats.Efficiency = 0.6
	} else {
		c.stats.Efficiency = 0.4
	}
}

// CacheOptimizer interface for caches that support optimization
type CacheOptimizer interface {
	Optimize()
}

// Cache management functions

// EnableLevel enables a specific cache level
func (c *MultiLevelCache) EnableLevel(levelName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.levels {
		if c.levels[i].Name == levelName {
			c.levels[i].Enabled = true
			return nil
		}
	}

	return fmt.Errorf("cache level %s not found", levelName)
}

// DisableLevel disables a specific cache level
func (c *MultiLevelCache) DisableLevel(levelName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.levels {
		if c.levels[i].Name == levelName {
			c.levels[i].Enabled = false
			return nil
		}
	}

	return fmt.Errorf("cache level %s not found", levelName)
}

// ClearLevel clears a specific cache level
func (c *MultiLevelCache) ClearLevel(levelName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, level := range c.levels {
		if level.Name == levelName && level.Enabled {
			if clearer, ok := level.Cache.(CacheClearer); ok {
				clearer.Clear()
				return nil
			}
			return fmt.Errorf("cache level %s does not support clearing", levelName)
		}
	}

	return fmt.Errorf("cache level %s not found or not enabled", levelName)
}

// CacheClearer interface for caches that support clearing
type CacheClearer interface {
	Clear()
}
