package cache

import (
	"container/list"
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

	// byObject indexes the chunk indices held for each object key, so Delete can find an object's
	// entries exactly.
	//
	// The alternative — scanning c.items and comparing key prefixes — is what Delete used to do, and it
	// removed logs/app2 and logs/appendix when asked to remove logs/app. No prefix comparison can be
	// made correct here: S3 keys may contain any byte that a delimiter could use, so "logs/app" is a
	// genuine prefix of a genuine sibling. An index sidesteps the question by never inferring the
	// object name from the entry key at all.
	//
	// Invariant: byObject[k] holds exactly the indices i for which items[entryKey(k, i)] exists. Every
	// mutation of items goes through addItem/removeItem to keep that true.
	byObject map[string]map[int64]struct{}

	// Configuration
	config *CacheConfig

	// Statistics
	stats types.CacheStats

	// Lifecycle management
	stopCh chan struct{}
	closed bool
}

// CacheConfig represents cache configuration
type CacheConfig struct {
	MaxSize         int64         `yaml:"max_size"`
	MaxEntries      int           `yaml:"max_entries"`
	TTL             time.Duration `yaml:"ttl"`
	EvictionPolicy  string        `yaml:"eviction_policy"`
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
}

// cacheItem is one contiguous run of bytes within one chunk of one object.
//
// object and index are retained alongside the map key they compose, because removeItem needs both to
// maintain the byObject index and recovering them by parsing the composed key would reintroduce the
// delimiter ambiguity that entryKey exists to avoid.
type cacheItem struct {
	key    string // the composed entry key, as stored in items
	object string // the object key, for the byObject index
	index  int64  // chunk index within the object
	run    chunkPiece

	timestamp   time.Time
	accessTime  time.Time
	accessCount int64
	weight      float64
	element     *list.Element
}

// size returns the bytes this item accounts for against the cache capacity.
func (i *cacheItem) size() int64 {
	return int64(len(i.run.data))
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
		byObject:  make(map[string]map[int64]struct{}),
		evictList: list.New(),
		config:    config,
		stats: types.CacheStats{
			Capacity: config.MaxSize,
		},
		stopCh: make(chan struct{}),
		closed: false,
	}

	// Start cleanup goroutine
	go cache.cleanupExpired()

	return cache
}

// Get returns the cached bytes for [offset, offset+size), or nil if the cache does not hold all of
// them.
//
// All of them: a partial hit is a miss. Returning the prefix a caller happens to hold would hand the
// FUSE layer a short buffer, which the kernel reads as a short file — so the guarantee here is
// all-or-nothing, and the assembly below either completes or abandons.
//
// A size of zero or less means "whatever contiguous bytes are held from offset", per the
// types.Cache contract, and is served by openEndedGet below.
func (c *LRUCache) Get(key string, offset, size int64) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	first, last, ok := chunkSpan(offset, size)
	if !ok {
		if offset >= 0 && size <= 0 {
			return c.openEndedGet(key, offset)
		}

		c.stats.Misses++
		c.updateHitRate()

		return nil
	}

	end := offset + size
	result := make([]byte, 0, size)

	// Walk the chunks the request spans in order, appending each one's contribution. Any chunk that is
	// absent, expired, or whose run does not cover the part needed ends the attempt.
	for index := first; index <= last; index++ {
		item, exists := c.items[entryKey(key, index)]
		if !exists {
			c.stats.Misses++
			c.updateHitRate()

			return nil
		}

		if c.isExpired(item) {
			c.removeItem(item.key)
			c.stats.Misses++
			c.updateHitRate()

			return nil
		}

		from := max(offset, chunkStart(index))
		to := min(end, chunkStart(index)+ChunkSize)

		if !item.run.covers(from, to) {
			c.stats.Misses++
			c.updateHitRate()

			return nil
		}

		result = append(result, item.run.data[from-item.run.start:to-item.run.start]...)
	}

	// Only now that the read is known to be satisfiable in full is it an access. Touching entries
	// during the walk would promote chunks of a request that ultimately missed, so a long strided read
	// could keep its own useless entries alive at the expense of a working set that hits.
	now := time.Now()
	for index := first; index <= last; index++ {
		item := c.items[entryKey(key, index)]
		item.accessTime = now
		item.accessCount++
		item.weight = c.calculateWeight(item)
		c.evictList.MoveToFront(item.element)
	}

	c.stats.Hits++
	c.updateHitRate()

	return result
}

// openEndedGet serves a Get whose size is zero or less: return the contiguous run held from offset,
// however long it happens to be, or nil if nothing starts there.
//
// This is the FUSE metadata cache's shape. It stores a marshaled ObjectInfo — a few hundred bytes,
// varying by path length and metadata — and at lookup time cannot state that length; under the old
// keying it asked for a fixed 8192 against whatever was stored and so never hit once, for any path, for
// the life of the mount, while reporting hits and misses as though it were working.
//
// It deliberately does not span chunks. A caller who cannot say how much it wants also cannot be told
// where the object ends, so "keep going while chunks happen to be present" would return a length that
// depends on eviction state rather than on the object. One chunk is a bound that does not vary, and a
// cached whole value larger than 1 MiB is not what this shape is for.
//
// Callers must hold c.mu.
func (c *LRUCache) openEndedGet(key string, offset int64) []byte {
	index := chunkIndexOf(offset)

	item, exists := c.items[entryKey(key, index)]
	if !exists || c.isExpired(item) {
		if exists {
			c.removeItem(item.key)
		}

		c.stats.Misses++
		c.updateHitRate()

		return nil
	}

	// The run must actually begin at or before the requested offset; a run starting later says nothing
	// about the bytes at offset.
	if item.run.start > offset || item.run.end() <= offset {
		c.stats.Misses++
		c.updateHitRate()

		return nil
	}

	item.accessTime = time.Now()
	item.accessCount++
	item.weight = c.calculateWeight(item)
	c.evictList.MoveToFront(item.element)

	c.stats.Hits++
	c.updateHitRate()

	result := make([]byte, item.run.end()-offset)
	copy(result, item.run.data[offset-item.run.start:])

	return result
}

// Put stores data read from offset, splitting it across chunk entries and merging each into whatever
// that chunk already holds.
func (c *LRUCache) Put(key string, offset int64, data []byte) {
	if len(data) == 0 || offset < 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	for _, piece := range splitIntoChunks(offset, data) {
		cacheKey := entryKey(key, piece.index)

		if item, exists := c.items[cacheKey]; exists {
			merged := coalesce(item.run, piece)

			// The run may have grown, shrunk (a disjoint newer read replacing a longer older one), or
			// stayed put. Re-derive the accounting from the result rather than assuming a direction.
			c.currentSize -= item.size()
			item.run = clonePiece(merged)
			c.currentSize += item.size()

			item.timestamp = now
			item.accessTime = now
			item.accessCount++
			item.weight = c.calculateWeight(item)

			c.evictList.MoveToFront(item.element)

			continue
		}

		item := &cacheItem{
			key:         cacheKey,
			object:      key,
			index:       piece.index,
			run:         clonePiece(piece),
			timestamp:   now,
			accessTime:  now,
			accessCount: 1,
		}
		item.weight = c.calculateWeight(item)

		c.addItem(item)
	}

	// Once, after the whole Put. Evicting per piece can drop a chunk this same call just stored, which
	// leaves the request that populated it unable to hit even immediately afterwards.
	c.evictIfNeeded()
}

// Delete removes every entry belonging to key, and only those.
//
// Exactness is the point. This is the invalidation the write path calls after modifying an object, so
// over-deleting silently costs re-fetches of unrelated files, and under-deleting serves stale bytes
// for up to the TTL. The old implementation compared key prefixes and did the former: Delete("logs/app")
// removed logs/app2 and logs/appendix, verified by execution.
func (c *LRUCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	indices, exists := c.byObject[key]
	if !exists {
		return
	}

	// Snapshot before removing: removeItem mutates this same map.
	keys := make([]string, 0, len(indices))
	for index := range indices {
		keys = append(keys, entryKey(key, index))
	}

	for _, cacheKey := range keys {
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

		entry, valid := element.Value.(*cacheEntry)
		if !valid {
			// Nothing else pushes onto this list, so this is unreachable — but an unchecked assertion
			// here would panic inside a cache eviction and take the mount with it, which is a
			// disproportionate response to a corrupt bookkeeping entry that can simply be dropped.
			c.evictList.Remove(element)

			continue
		}

		item := c.items[entry.key]
		if item != nil {
			freedSize += item.size()
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

	// Track evictions before clearing
	evictCount := uint64(len(c.items))

	// Clear all items properly to help GC
	for key, item := range c.items {
		item.run.data = nil
		item.element = nil
		delete(c.items, key)
	}

	c.byObject = make(map[string]map[int64]struct{})
	c.evictList.Init()
	c.currentSize = 0
	c.stats.Evictions += evictCount
}

// Close stops the cleanup goroutine and releases resources
func (c *LRUCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true
	close(c.stopCh)

	// Clear all items
	c.items = make(map[string]*cacheItem)
	c.byObject = make(map[string]map[int64]struct{})
	c.evictList.Init()
	c.currentSize = 0

	return nil
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

// addItem inserts a new item and registers it in both the eviction list and the object index.
//
// Every insertion goes through here, so that no code path can add to items without the byObject
// index learning about it — an entry missing from the index is an entry Delete will not invalidate,
// which is stale data served past a write.
func (c *LRUCache) addItem(item *cacheItem) {
	item.element = c.evictList.PushFront(&cacheEntry{key: item.key})
	c.items[item.key] = item
	c.currentSize += item.size()

	indices, exists := c.byObject[item.object]
	if !exists {
		indices = make(map[int64]struct{})
		c.byObject[item.object] = indices
	}
	indices[item.index] = struct{}{}
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
	sizeFactor := 1.0 / (1.0 + float64(item.size())/1024.0/1024.0) // Smaller items have higher weight

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
		item.element = nil
	}

	// Remove from items map
	delete(c.items, key)

	// And from the object index, dropping the object's entry once its last chunk is gone so that an
	// object read once and then evicted does not leave a permanent empty map behind.
	if indices, ok := c.byObject[item.object]; ok {
		delete(indices, item.index)
		if len(indices) == 0 {
			delete(c.byObject, item.object)
		}
	}

	// Update size before releasing the data, since size() reads it.
	c.currentSize -= item.size()
	item.run.data = nil
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

	entry, ok := element.Value.(*cacheEntry)
	if !ok {
		// As in Evict: unreachable, but a panic here would be inside eviction, under the mutex, and
		// would take down the mount rather than lose one cache entry.
		c.evictList.Remove(element)

		return
	}

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

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
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
			size:   item.size(),
		})
	}

	// Sort by weight (lowest first)
	for i := range len(items) - 1 {
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
