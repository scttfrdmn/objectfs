package cache

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// PersistentCache implements a disk-based cache with optional compression.
//
// It is keyed the same way LRUCache is — see chunking.go, which is the single definition both share
// after each having carried its own character-identical copy of the same three defects.
type PersistentCache struct {
	mu          sync.RWMutex
	directory   string
	maxSize     int64
	currentSize int64
	index       map[string]*persistentItem

	// byObject maps an object key to the chunk indices held for it, so Delete finds an object's entries
	// exactly rather than by comparing key prefixes. See LRUCache.byObject for why a prefix compare
	// cannot be made correct here.
	byObject map[string]map[int64]struct{}

	config *PersistentCacheConfig
	stats  types.CacheStats
	// Lifecycle management
	stopCh chan struct{}
	closed bool
}

// PersistentCacheConfig represents persistent cache configuration
type PersistentCacheConfig struct {
	Directory       string        `yaml:"directory"`
	MaxSize         int64         `yaml:"max_size"`
	TTL             time.Duration `yaml:"ttl"`
	Compression     bool          `yaml:"compression"`
	IndexFile       string        `yaml:"index_file"`
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
	SyncInterval    time.Duration `yaml:"sync_interval"`
}

// persistentItem represents an item in the persistent cache.
//
// Object and ChunkIndex describe which chunk of which object this is; Start and Length describe the
// contiguous run within that chunk that the file actually holds. Size is the *on-disk* byte count,
// which differs from Length whenever compression is on and is what the capacity accounting uses.
//
// Object and ChunkIndex are stored rather than derived from Key because recovering them by parsing the
// composed key would reintroduce the delimiter ambiguity entryKey exists to avoid.
type persistentItem struct {
	Key        string `json:"key"`
	FilePath   string `json:"file_path"`
	Object     string `json:"object"`
	ChunkIndex int64  `json:"chunk_index"`

	// Start is the absolute object offset of the first byte held; Length is how many bytes follow.
	Start  int64 `json:"start"`
	Length int64 `json:"length"`

	Size       int64     `json:"size"` // bytes on disk, after any compression
	Timestamp  time.Time `json:"timestamp"`
	AccessTime time.Time `json:"access_time"`
	Compressed bool      `json:"compressed"`
	Checksum   string    `json:"checksum"`
}

// run reconstructs the item's coverage for the coverage checks in chunking.go. The data slice is left
// nil: the bytes live on disk, and only start/end are needed to decide whether a read is satisfiable
// before paying for the read.
func (i *persistentItem) span() (start, end int64) {
	return i.Start, i.Start + i.Length
}

// NewPersistentCache creates a new persistent cache
func NewPersistentCache(config *PersistentCacheConfig) (*PersistentCache, error) {
	if config == nil {
		config = &PersistentCacheConfig{
			Directory:       "/tmp/objectfs-cache",
			MaxSize:         10 * 1024 * 1024 * 1024, // 10GB
			TTL:             1 * time.Hour,
			Compression:     true,
			IndexFile:       "cache-index.json",
			CleanupInterval: 10 * time.Minute,
			SyncInterval:    time.Minute,
		}
	}

	// Apply defaults for zero/empty values
	if config.IndexFile == "" {
		config.IndexFile = "cache-index.json"
	}
	if config.CleanupInterval <= 0 {
		config.CleanupInterval = 10 * time.Minute
	}
	if config.SyncInterval <= 0 {
		config.SyncInterval = time.Minute
	}

	// Create cache directory
	if err := os.MkdirAll(config.Directory, 0750); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	cache := &PersistentCache{
		directory: config.Directory,
		maxSize:   config.MaxSize,
		index:     make(map[string]*persistentItem),
		byObject:  make(map[string]map[int64]struct{}),
		config:    config,
		stats: types.CacheStats{
			Capacity: config.MaxSize,
		},
		stopCh: make(chan struct{}),
		closed: false,
	}

	// Load existing index
	if err := cache.loadIndex(); err != nil {
		return nil, fmt.Errorf("failed to load cache index: %w", err)
	}

	// Start background goroutines
	go cache.cleanupExpired()
	go cache.syncIndex()

	return cache, nil
}

// Get returns the cached bytes for [offset, offset+size), or nil if the cache does not hold all of
// them. See the types.Cache contract: a partial hit is a miss, and a size of zero or less means
// "whatever contiguous bytes are held from offset".
func (c *PersistentCache) Get(key string, offset, size int64) []byte {
	first, last, ok := chunkSpan(offset, size)
	if !ok {
		if offset >= 0 && size <= 0 {
			return c.openEndedGet(key, offset)
		}

		c.recordMiss()

		return nil
	}

	end := offset + size

	// Decide satisfiability under the lock, then read the files outside it. Disk reads must not hold
	// the mutex — with compression on, a read is a gzip decode, and blocking every other cache
	// operation behind it would serialize the filesystem on the slowest device in the path.
	type plan struct {
		item     *persistentItem
		from, to int64
	}

	c.mu.RLock()
	plans := make([]plan, 0, last-first+1)

	for index := first; index <= last; index++ {
		item, exists := c.index[entryKey(key, index)]
		if !exists || c.isExpired(item) {
			c.mu.RUnlock()
			c.recordMiss()

			return nil
		}

		from := max(offset, chunkStart(index))
		to := min(end, chunkStart(index)+ChunkSize)

		if start, held := item.span(); start > from || held < to {
			c.mu.RUnlock()
			c.recordMiss()

			return nil
		}

		plans = append(plans, plan{item: item, from: from, to: to})
	}
	c.mu.RUnlock()

	result := make([]byte, 0, size)

	for _, p := range plans {
		data, err := c.readFromFile(p.item)
		if err != nil {
			// The file is missing or its checksum failed. Drop the entry and miss: a cache whose backing
			// file has gone bad must not answer from it, and the object is still readable from S3.
			c.dropEntry(p.item)
			c.recordMiss()

			return nil
		}

		// The file holds the whole run; slice out the requested part of it.
		lo := p.from - p.item.Start
		hi := p.to - p.item.Start

		if lo < 0 || hi > int64(len(data)) {
			// The file's length disagrees with the index's record of it. Trusting either over the other
			// would mean returning bytes from an offset that is no longer known.
			c.dropEntry(p.item)
			c.recordMiss()

			return nil
		}

		result = append(result, data[lo:hi]...)
	}

	// Only now that every chunk was readable is this a hit.
	c.mu.Lock()
	now := time.Now()
	for _, p := range plans {
		p.item.AccessTime = now
	}
	c.stats.Hits++
	c.updateHitRate()
	c.mu.Unlock()

	return result
}

// openEndedGet serves a Get whose size is zero or less: the contiguous run held from offset, bounded
// to one chunk. See LRUCache.openEndedGet for why the bound exists.
func (c *PersistentCache) openEndedGet(key string, offset int64) []byte {
	c.mu.RLock()
	item, exists := c.index[entryKey(key, chunkIndexOf(offset))]
	if !exists || c.isExpired(item) {
		c.mu.RUnlock()
		c.recordMiss()

		return nil
	}

	start, end := item.span()
	if start > offset || end <= offset {
		c.mu.RUnlock()
		c.recordMiss()

		return nil
	}
	c.mu.RUnlock()

	data, err := c.readFromFile(item)
	if err != nil {
		c.dropEntry(item)
		c.recordMiss()

		return nil
	}

	skip := offset - start
	if skip > int64(len(data)) {
		c.dropEntry(item)
		c.recordMiss()

		return nil
	}

	c.mu.Lock()
	item.AccessTime = time.Now()
	c.stats.Hits++
	c.updateHitRate()
	c.mu.Unlock()

	result := make([]byte, int64(len(data))-skip)
	copy(result, data[skip:])

	return result
}

// Put stores data read from offset, splitting it across chunk entries and merging each into whatever
// that chunk already holds.
//
// Merging costs a read-back of the existing entry, which for a disk cache is not free. Replacing
// instead would be cheaper and is wrong: consecutive reads of one file arrive as several runs within
// the same chunk — a sequential reader gets 128 KiB at a time against a 1 MiB chunk — so a replacing
// Put would leave only the last eighth of each chunk cached, and every re-read of the rest would go
// back to S3. Paying one bounded read to keep the other seven eighths is the better side of that trade.
//
// This also keeps both cache tiers behaving identically, which is worth something on its own: the
// coalescing rules live once, in chunking.go, rather than in two implementations that can drift.
//
// The read-back happens under c.mu, unlike the reads in Get, which release it first. That is a
// deliberate difference: Get may need to read an unbounded number of chunks, while this reads at most
// one entry of at most ChunkSize, and doing it under the lock is what makes read-merge-write atomic.
// Dropping the lock mid-sequence would let a concurrent Put to the same chunk interleave, and the loser
// of that race would write a merge computed against bytes that had already been replaced.
func (c *PersistentCache) Put(key string, offset int64, data []byte) {
	if len(data) == 0 || offset < 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	for _, piece := range splitIntoChunks(offset, data) {
		cacheKey := entryKey(key, piece.index)

		if existing, exists := c.index[cacheKey]; exists {
			// Merge with what is already on disk, unless it cannot be read — in which case the entry was
			// no good anyway and the incoming run simply replaces it.
			if held, err := c.readFromFile(existing); err == nil {
				piece = coalesce(chunkPiece{
					index: existing.ChunkIndex,
					start: existing.Start,
					data:  held,
				}, piece)
			}

			_ = os.Remove(existing.FilePath) // Ignore error on cleanup
			c.currentSize -= existing.Size
			delete(c.index, cacheKey)
			c.unindex(existing)
		}

		item := &persistentItem{
			Key:        cacheKey,
			Object:     key,
			ChunkIndex: piece.index,
			Start:      piece.start,
			Length:     int64(len(piece.data)),
			Timestamp:  now,
			AccessTime: now,
			Compressed: c.config.Compression,
			Checksum:   c.calculateChecksum(piece.data),
		}
		item.FilePath = c.generateFilePath(cacheKey)

		actualSize, err := c.writeToFile(item, piece.data)
		if err != nil {
			continue // Failed to write; leave this chunk uncached rather than indexing a bad file
		}

		item.Size = actualSize

		c.index[cacheKey] = item
		c.currentSize += actualSize
		c.reindex(item)
	}

	// Once, after the whole Put, so eviction cannot discard a chunk this same call just stored.
	c.evictIfNeeded()
}

// Delete removes every entry belonging to key, and only those. See LRUCache.Delete.
func (c *PersistentCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	indices, exists := c.byObject[key]
	if !exists {
		return
	}

	// Snapshot before mutating, since unindex writes to this same map.
	cacheKeys := make([]string, 0, len(indices))
	for index := range indices {
		cacheKeys = append(cacheKeys, entryKey(key, index))
	}

	for _, cacheKey := range cacheKeys {
		item, ok := c.index[cacheKey]
		if !ok {
			continue
		}

		_ = os.Remove(item.FilePath) // Ignore error on cleanup

		delete(c.index, cacheKey)
		c.unindex(item)
		c.currentSize -= item.Size
		c.stats.Evictions++
	}
}

// recordMiss counts a miss and refreshes the hit rate.
func (c *PersistentCache) recordMiss() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stats.Misses++
	c.updateHitRate()
}

// dropEntry removes an entry whose backing file could not be read.
func (c *PersistentCache) dropEntry(item *persistentItem) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// It may already be gone: the read happened outside the lock.
	if _, exists := c.index[item.Key]; !exists {
		return
	}

	_ = os.Remove(item.FilePath) // Ignore error on cleanup

	delete(c.index, item.Key)
	c.unindex(item)
	c.currentSize -= item.Size
}

// reindex records an item in the object index. Callers must hold c.mu.
func (c *PersistentCache) reindex(item *persistentItem) {
	indices, exists := c.byObject[item.Object]
	if !exists {
		indices = make(map[int64]struct{})
		c.byObject[item.Object] = indices
	}

	indices[item.ChunkIndex] = struct{}{}
}

// unindex removes an item from the object index, dropping the object once its last chunk is gone.
// Callers must hold c.mu.
func (c *PersistentCache) unindex(item *persistentItem) {
	indices, exists := c.byObject[item.Object]
	if !exists {
		return
	}

	delete(indices, item.ChunkIndex)

	if len(indices) == 0 {
		delete(c.byObject, item.Object)
	}
}

// Evict evicts items to free up space
func (c *PersistentCache) Evict(targetSize int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	freedSize := int64(0)

	// Build list of items sorted by access time (LRU)
	type itemWithTime struct {
		item       *persistentItem
		accessTime time.Time
	}

	items := make([]itemWithTime, 0, len(c.index))
	for _, item := range c.index {
		items = append(items, itemWithTime{
			item:       item,
			accessTime: item.AccessTime,
		})
	}

	// Sort by access time (oldest first)
	for i := range len(items) - 1 {
		for j := i + 1; j < len(items); j++ {
			if items[i].accessTime.After(items[j].accessTime) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}

	// Evict oldest items first
	for _, itemWithTime := range items {
		if freedSize >= targetSize {
			break
		}

		item := itemWithTime.item

		// Remove file
		_ = os.Remove(item.FilePath) // Ignore error on cleanup

		// Remove from index
		delete(c.index, item.Key)
		c.unindex(item)
		freedSize += item.Size
		c.currentSize -= item.Size
		c.stats.Evictions++
	}

	return freedSize >= targetSize
}

// Size returns the current cache size
func (c *PersistentCache) Size() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentSize
}

// Stats returns cache statistics
func (c *PersistentCache) Stats() types.CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stats := c.stats
	stats.Size = c.currentSize
	stats.Utilization = float64(c.currentSize) / float64(c.maxSize)
	return stats
}

// Clear clears all cached data
func (c *PersistentCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Remove all files
	for _, item := range c.index {
		_ = os.Remove(item.FilePath) // Ignore error on cleanup
	}

	// Capture count before resetting the map; len(c.index) would be 0 after
	// the reset and always record zero evictions (#103).
	count := uint64(len(c.index))
	c.index = make(map[string]*persistentItem)
	c.byObject = make(map[string]map[int64]struct{})
	c.currentSize = 0
	c.stats.Evictions += count
}

// Close stops background goroutines and syncs the index
func (c *PersistentCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true
	close(c.stopCh)

	// Final sync of index before closing
	return c.saveIndex()
}

// Optimize optimizes the cache by defragmenting and cleaning up
func (c *PersistentCache) Optimize() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Remove expired items
	var expiredKeys []string
	for key, item := range c.index {
		if c.isExpired(item) {
			expiredKeys = append(expiredKeys, key)
		}
	}

	for _, key := range expiredKeys {
		item := c.index[key]
		_ = os.Remove(item.FilePath) // Ignore error on cleanup
		delete(c.index, key)
		c.unindex(item)
		c.currentSize -= item.Size
	}

	// Force sync index
	_ = c.saveIndex() // Index save errors are logged internally
}

// Helper methods

func (c *PersistentCache) isExpired(item *persistentItem) bool {
	if c.config.TTL == 0 {
		return false
	}
	return time.Since(item.Timestamp) > c.config.TTL
}

// generateFilePath names the on-disk file for a cache key.
//
// The full SHA-256 rather than its first 8 bytes (audit finding L25). Sixty-four bits gives a 50%
// chance of some collision at ~5 billion entries and a one-in-a-million chance at ~6 million, which a
// long-lived cache on a large bucket can reach; 256 bits cannot be reached.
//
// What a collision actually cost was worth measuring before changing this, and it is not corruption:
// the index is keyed on the full entryKey string and readFromFile verifies a full SHA-256 of the
// content, so two keys landing on one filename produce a *miss* — the checksum fails, dropEntry runs,
// and the shared file goes with it, costing the other key its entry too. Removing that checksum check
// and re-running the same probe does serve the wrong bytes, so the checksum is the guard and this
// width is hygiene. Both were verified by mutation.
//
// Widening is backward compatible with a cache directory written by an older build: FilePath is stored
// in the index and every read uses the stored value, so this function is only consulted for new
// entries. Old files keep their short names until they expire or are evicted.
func (c *PersistentCache) generateFilePath(key string) string {
	hash := sha256.Sum256([]byte(key))
	return filepath.Join(c.directory, fmt.Sprintf("%x.cache", hash))
}

func (c *PersistentCache) calculateChecksum(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash)
}

// writeToFile writes the entry's bytes and returns how many bytes it occupies on disk.
//
// The returned size drives currentSize, which drives eviction, so an undercount means the cache never
// evicts and fills the disk. That is what this function used to do: gzip.Writer buffers, and the
// Close that flushes it was deferred — so file.Stat() ran on a file that was still mostly in memory.
// Measured before the fix: 10 bytes recorded for a 330-byte file, a ~33x undercount, and since Delete
// subtracted the same bogus figure the counter also drifted negative over time.
//
// Both closes are therefore explicit and ordered — compressor first, then stat, then the file — and
// their errors are returned rather than discarded. A failed flush means the file on disk is truncated,
// which the checksum in readFromFile would later report as corruption; better to fail the write and
// leave the entry uncached.
func (c *PersistentCache) writeToFile(item *persistentItem, data []byte) (int64, error) {
	file, err := os.Create(item.FilePath)
	if err != nil {
		return 0, err
	}

	// Tracks whether the ordinary path has already closed the file, so the cleanup path does not close
	// it twice.
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()

	fail := func(err error) (int64, error) {
		_ = os.Remove(item.FilePath) // Clean up on error, ignore result

		return 0, err
	}

	var writer io.Writer = file
	var gzipWriter *gzip.Writer

	if item.Compressed {
		gzipWriter = gzip.NewWriter(file)
		writer = gzipWriter
	}

	if _, err := writer.Write(data); err != nil {
		return fail(err)
	}

	// Flush the compressor before measuring, or the measurement is of whatever happened to have been
	// written so far.
	if gzipWriter != nil {
		if err := gzipWriter.Close(); err != nil {
			return fail(err)
		}
	}

	// Sync before Stat: on some filesystems the size is not visible to a stat of the open descriptor
	// until the data is flushed out of the kernel's page cache for this file.
	if err := file.Sync(); err != nil {
		return fail(err)
	}

	stat, err := file.Stat()
	if err != nil {
		return fail(err)
	}

	closed = true
	if err := file.Close(); err != nil {
		return fail(err)
	}

	size := stat.Size()
	if size <= 0 {
		// A zero-length file for a non-empty entry means the write did not reach the disk. Indexing it
		// would both undercount the cache and hand back an empty buffer on the next read.
		return fail(fmt.Errorf("cache file %s is %d bytes after writing %d", item.FilePath, size, len(data)))
	}

	return size, nil
}

func (c *PersistentCache) readFromFile(item *persistentItem) ([]byte, error) {
	file, err := os.Open(item.FilePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	var reader io.Reader = file

	// Handle decompression if compressed
	if item.Compressed {
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			return nil, err
		}
		defer func() { _ = gzipReader.Close() }()
		reader = gzipReader
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	// Verify checksum
	if c.calculateChecksum(data) != item.Checksum {
		return nil, fmt.Errorf("checksum mismatch for cached file")
	}

	return data, nil
}

func (c *PersistentCache) loadIndex() error {
	indexPath := filepath.Join(c.directory, c.config.IndexFile)

	// Validate path is within the cache directory
	if !strings.HasPrefix(filepath.Clean(indexPath), filepath.Clean(c.directory)) {
		return fmt.Errorf("invalid index file path: %s", indexPath)
	}

	file, err := os.Open(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No existing index, start fresh
		}
		return err
	}
	defer func() { _ = file.Close() }()

	var items map[string]*persistentItem
	if err := json.NewDecoder(file).Decode(&items); err != nil {
		return err
	}

	// Validate items and calculate current size
	c.currentSize = 0
	for key, item := range items {
		// Check if file still exists
		if _, err := os.Stat(item.FilePath); os.IsNotExist(err) {
			continue // Skip missing files
		}

		// Discard entries written by a version with different keying.
		//
		// An index persists across restarts and across upgrades, so this loop is the one place that sees
		// another version's records. An entry from the pre-chunking format has no Object and no Length:
		// keeping it would put a record in the index that Delete cannot find — since Delete works from
		// the object name — and whose file the coverage check would read at the wrong offset. Both
		// failure modes are silent, and the recovery is free: drop the entry and re-fetch from S3.
		//
		// key is checked against the item's own fields rather than parsed, because parsing it is what
		// the NUL separator exists to make unnecessary.
		if item.Object == "" || item.Length <= 0 || key != entryKey(item.Object, item.ChunkIndex) {
			_ = os.Remove(item.FilePath) // Ignore error on cleanup

			continue
		}

		c.index[key] = item
		c.reindex(item)
		c.currentSize += item.Size
	}

	return nil
}

func (c *PersistentCache) saveIndex() error {
	indexPath := filepath.Join(c.directory, c.config.IndexFile)

	// Validate path is within the cache directory
	if !strings.HasPrefix(filepath.Clean(indexPath), filepath.Clean(c.directory)) {
		return fmt.Errorf("invalid index file path: %s", indexPath)
	}

	tmpPath := indexPath + ".tmp"
	// Validate tmp path is still within cache directory
	if !strings.HasPrefix(filepath.Clean(tmpPath), filepath.Clean(c.directory)) {
		return fmt.Errorf("invalid tmp index file path: %s", tmpPath)
	}
	file, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	if err := json.NewEncoder(file).Encode(c.index); err != nil {
		_ = os.Remove(tmpPath) // Ignore cleanup error
		return err
	}

	// Atomic replace
	return os.Rename(tmpPath, indexPath)
}

func (c *PersistentCache) evictIfNeeded() {
	for c.currentSize > c.maxSize {
		if !c.evictOldest() {
			break // No more items to evict
		}
	}
}

func (c *PersistentCache) evictOldest() bool {
	if len(c.index) == 0 {
		return false
	}

	var oldestKey string
	var oldestTime time.Time

	// Find oldest item
	first := true
	for key, item := range c.index {
		if first || item.AccessTime.Before(oldestTime) {
			oldestKey = key
			oldestTime = item.AccessTime
			first = false
		}
	}

	if oldestKey != "" {
		item := c.index[oldestKey]
		_ = os.Remove(item.FilePath) // Ignore error on cleanup
		delete(c.index, oldestKey)
		c.currentSize -= item.Size
		c.stats.Evictions++
		return true
	}

	return false
}

func (c *PersistentCache) updateHitRate() {
	total := c.stats.Hits + c.stats.Misses
	if total > 0 {
		c.stats.HitRate = float64(c.stats.Hits) / float64(total)
	}
}

func (c *PersistentCache) cleanupExpired() {
	ticker := time.NewTicker(c.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.mu.Lock()
			var expiredKeys []string

			for key, item := range c.index {
				if c.isExpired(item) {
					expiredKeys = append(expiredKeys, key)
				}
			}

			for _, key := range expiredKeys {
				item := c.index[key]
				_ = os.Remove(item.FilePath) // Ignore error on cleanup
				delete(c.index, key)
				c.currentSize -= item.Size
			}
			c.mu.Unlock()
		}
	}
}

func (c *PersistentCache) syncIndex() {
	ticker := time.NewTicker(c.config.SyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.mu.RLock()
			_ = c.saveIndex() // Index save errors are logged internally
			c.mu.RUnlock()
		}
	}
}
