package cache

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/objectfs/objectfs/pkg/types"
)

// LRUCache implements a thread-safe LRU cache with weighted eviction
type LRUCache struct {
	mu          sync.RWMutex
	capacity    int64
	currentSize int64
	items       map[string]*cacheItem
	evictList   *list.List

	// Configuration
	config *CacheConfig

	// Statistics
	stats types.CacheStats
}

// CacheConfig represents cache configuration
type CacheConfig struct {
	MaxSize         int64         `yaml:"max_size"`
	MaxEntries      int           `yaml:"max_entries"`
	TTL             time.Duration `yaml:"ttl"`
	EvictionPolicy  string        `yaml:"eviction_policy"`
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
}

// cacheItem represents an item in the cache
type cacheItem struct {
	key         string
	data        []byte
	offset      int64
	size        int64
	timestamp   time.Time
	accessTime  time.Time
	accessCount int64
	weight      float64
	element     *list.Element
}

// cacheEntry represents the value stored in the list element
type cacheEntry struct {
	key string
}

// NewLRUCache creates a new LRU cache
func NewLRUCache(config *CacheConfig) *LRUCache {
	if config == nil {
		config = &CacheConfig{
			MaxSize:         2 * 1024 * 1024 * 1024, // 2GB
			MaxEntries:      100000,
			TTL:             5 * time.Minute,
			EvictionPolicy:  "weighted_lru",
			CleanupInterval: time.Minute,
		}
	}

	cache := &LRUCache{
		capacity:  config.MaxSize,
		items:     make(map[string]*cacheItem),
		evictList: list.New(),
		config:    config,
		stats: types.CacheStats{
			Capacity: config.MaxSize,
		},
	}

	// Start cleanup goroutine
	go cache.cleanupExpired()

	return cache
}

// Get retrieves data from the cache
func (c *LRUCache) Get(key string, offset, size int64) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	cacheKey := c.makeCacheKey(key, offset, size)
	item, exists := c.items[cacheKey]

	if !exists {
		c.stats.Misses++
		return nil
	}

	// Check if item has expired
	if c.isExpired(item) {
		c.removeItem(cacheKey)
		c.stats.Misses++
		return nil
	}

	// Update access information
	item.accessTime = time.Now()
	item.accessCount++
	item.weight = c.calculateWeight(item)

	// Move to front of eviction list
	c.evictList.MoveToFront(item.element)

	c.stats.Hits++
	c.updateHitRate()

	// Return a copy of the data
	result := make([]byte, len(item.data))
	copy(result, item.data)
	return result
}

// Put stores data in the cache
func (c *LRUCache) Put(key string, offset int64, data []byte) {
	if len(data) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	size := int64(len(data))
	cacheKey := c.makeCacheKey(key, offset, size)

	// Check if item already exists
	if item, exists := c.items[cacheKey]; exists {
		// Update existing item
		c.currentSize -= item.size
		item.data = make([]byte, len(data))
		copy(item.data, data)
		item.size = size
		item.timestamp = time.Now()
		item.accessTime = time.Now()
		item.accessCount++
		item.weight = c.calculateWeight(item)
		c.currentSize += size

		// Move to front
		c.evictList.MoveToFront(item.element)
		return
	}

	// Create new item
	newItem := &cacheItem{
		key:         cacheKey,
		data:        make([]byte, len(data)),
		offset:      offset,
		size:        size,
		timestamp:   time.Now(),
		accessTime:  time.Now(),
		accessCount: 1,
	}
	copy(newItem.data, data)
	newItem.weight = c.calculateWeight(newItem)

	// Add to eviction list
	element := c.evictList.PushFront(&cacheEntry{key: cacheKey})
	newItem.element = element

	// Add to items map
	c.items[cacheKey] = newItem
	c.currentSize += size

	// Evict if necessary
	c.evictIfNeeded()
}

// Delete removes an item from the cache
func (c *LRUCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Remove all items with this key prefix
	var keysToDelete []string
	for cacheKey := range c.items {
		if c.keyMatches(cacheKey, key) {
			keysToDelete = append(keysToDelete, cacheKey)
		}
	}

	for _, cacheKey := range keysToDelete {
		c.removeItem(cacheKey)
	}
}

// Evict evicts items to free up the specified amount of space
func (c *LRUCache) Evict(targetSize int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	freedSize := int64(0)

	// Evict from the back of the list (least recently used)
	for freedSize < targetSize && c.evictList.Len() > 0 {
		element := c.evictList.Back()
		if element == nil {
			break
		}

		entry := element.Value.(*cacheEntry)
		item := c.items[entry.key]
		if item != nil {
			freedSize += item.size
			c.removeItem(entry.key)
		} else {
			c.evictList.Remove(element)
		}
	}

	return freedSize >= targetSize
}

// Size returns the current cache size
func (c *LRUCache) Size() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentSize
}

// Stats returns cache statistics
func (c *LRUCache) Stats() types.CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stats := c.stats
	stats.Size = c.currentSize
	stats.Utilization = float64(c.currentSize) / float64(c.capacity)
	return stats
}

// Clear clears all items from the cache
func (c *LRUCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*cacheItem)
	c.evictList.Init()
	c.currentSize = 0
	c.stats.Evictions += uint64(len(c.items))
}

// GetKeys returns all cache keys (for debugging)
func (c *LRUCache) GetKeys() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	keys := make([]string, 0, len(c.items))
	for key := range c.items {
		keys = append(keys, key)
	}
	return keys
}

// Resize changes the cache capacity
func (c *LRUCache) Resize(newCapacity int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.capacity = newCapacity
	c.stats.Capacity = newCapacity
	c.evictIfNeeded()
}

// Helper methods

func (c *LRUCache) makeCacheKey(key string, offset, size int64) string {
	return fmt.Sprintf("%s:%d:%d", key, offset, size)
}

func (c *LRUCache) keyMatches(cacheKey, key string) bool {
	// Simple prefix match - in a real implementation, you might want more sophisticated matching
	return len(cacheKey) >= len(key) && cacheKey[:len(key)] == key
}

func (c *LRUCache) isExpired(item *cacheItem) bool {
	if c.config.TTL == 0 {
		return false
	}
	return time.Since(item.timestamp) > c.config.TTL
}

func (c *LRUCache) calculateWeight(item *cacheItem) float64 {
	// Weight calculation based on access frequency and recency
	recencyFactor := 1.0 / (1.0 + time.Since(item.accessTime).Seconds()/3600.0)
	frequencyFactor := float64(item.accessCount)
	sizeFactor := 1.0 / (1.0 + float64(item.size)/1024.0/1024.0) // Smaller items have higher weight

	return recencyFactor * frequencyFactor * sizeFactor
}

func (c *LRUCache) removeItem(key string) {
	item, exists := c.items[key]
	if !exists {
		return
	}

	// Remove from eviction list
	if item.element != nil {
		c.evictList.Remove(item.element)
	}

	// Remove from items map
	delete(c.items, key)

	// Update size
	c.currentSize -= item.size
	c.stats.Evictions++
}

func (c *LRUCache) evictIfNeeded() {
	// Evict by size
	for c.currentSize > c.capacity && c.evictList.Len() > 0 {
		c.evictOldest()
	}

	// Evict by count
	maxEntries := c.config.MaxEntries
	if maxEntries > 0 {
		for len(c.items) > maxEntries && c.evictList.Len() > 0 {
			c.evictOldest()
		}
	}
}

func (c *LRUCache) evictOldest() {
	element := c.evictList.Back()
	if element == nil {
		return
	}

	entry := element.Value.(*cacheEntry)
	c.removeItem(entry.key)
}

func (c *LRUCache) updateHitRate() {
	total := c.stats.Hits + c.stats.Misses
	if total > 0 {
		c.stats.HitRate = float64(c.stats.Hits) / float64(total)
	}
}

func (c *LRUCache) cleanupExpired() {
	cleanupInterval := c.config.CleanupInterval
	if cleanupInterval <= 0 {
		cleanupInterval = 5 * time.Minute // Default cleanup interval
	}

	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		c.mu.Lock()
		var expiredKeys []string

		for key, item := range c.items {
			if c.isExpired(item) {
				expiredKeys = append(expiredKeys, key)
			}
		}

		for _, key := range expiredKeys {
			c.removeItem(key)
		}
		c.mu.Unlock()
	}
}

// WeightedLRUCache extends LRUCache with weighted eviction
type WeightedLRUCache struct {
	*LRUCache
}

// NewWeightedLRUCache creates a new weighted LRU cache
func NewWeightedLRUCache(config *CacheConfig) *WeightedLRUCache {
	if config == nil {
		config = &CacheConfig{}
	}
	config.EvictionPolicy = "weighted_lru"

	return &WeightedLRUCache{
		LRUCache: NewLRUCache(config),
	}
}

// EvictByWeight evicts items based on their weight (frequency + recency + size)
func (c *WeightedLRUCache) EvictByWeight(targetSize int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.items) == 0 {
		return false
	}

	// Build list of items sorted by weight (ascending - lowest weight first)
	type weightedItem struct {
		key    string
		weight float64
		size   int64
	}

	items := make([]weightedItem, 0, len(c.items))
	for key, item := range c.items {
		items = append(items, weightedItem{
			key:    key,
			weight: item.weight,
			size:   item.size,
		})
	}

	// Sort by weight (lowest first)
	for i := 0; i < len(items)-1; i++ {
		for j := i + 1; j < len(items); j++ {
			if items[i].weight > items[j].weight {
				items[i], items[j] = items[j], items[i]
			}
		}
	}

	// Evict lowest weight items first
	freedSize := int64(0)
	for _, item := range items {
		if freedSize >= targetSize {
			break
		}
		c.removeItem(item.key)
		freedSize += item.size
	}

	return freedSize >= targetSize
}
