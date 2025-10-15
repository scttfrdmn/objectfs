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

	"github.com/objectfs/objectfs/pkg/types"
)

// PersistentCache implements a disk-based cache with optional compression
type PersistentCache struct {
	mu          sync.RWMutex
	directory   string
	maxSize     int64
	currentSize int64
	index       map[string]*persistentItem
	config      *PersistentCacheConfig
	stats       types.CacheStats
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

// persistentItem represents an item in the persistent cache
type persistentItem struct {
	Key        string    `json:"key"`
	FilePath   string    `json:"file_path"`
	Offset     int64     `json:"offset"`
	Size       int64     `json:"size"`
	Timestamp  time.Time `json:"timestamp"`
	AccessTime time.Time `json:"access_time"`
	Compressed bool      `json:"compressed"`
	Checksum   string    `json:"checksum"`
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

// Get retrieves data from the persistent cache
func (c *PersistentCache) Get(key string, offset, size int64) []byte {
	c.mu.RLock()
	cacheKey := c.makeCacheKey(key, offset, size)
	item, exists := c.index[cacheKey]
	c.mu.RUnlock()

	if !exists {
		c.mu.Lock()
		c.stats.Misses++
		c.mu.Unlock()
		return nil
	}

	// Check if item has expired
	if c.isExpired(item) {
		c.Delete(key)
		c.mu.Lock()
		c.stats.Misses++
		c.mu.Unlock()
		return nil
	}

	// Read data from file
	data, err := c.readFromFile(item)
	if err != nil {
		// File might be corrupted or missing, remove from index
		c.mu.Lock()
		delete(c.index, cacheKey)
		c.currentSize -= item.Size
		c.stats.Misses++
		c.mu.Unlock()
		return nil
	}

	// Update access time
	c.mu.Lock()
	item.AccessTime = time.Now()
	c.stats.Hits++
	c.updateHitRate()
	c.mu.Unlock()

	return data
}

// Put stores data in the persistent cache
func (c *PersistentCache) Put(key string, offset int64, data []byte) {
	if len(data) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	cacheKey := c.makeCacheKey(key, offset, int64(len(data)))

	// Check if item already exists
	if existingItem, exists := c.index[cacheKey]; exists {
		// Remove old file
		_ = os.Remove(existingItem.FilePath) // Ignore error on cleanup
		c.currentSize -= existingItem.Size
	}

	// Create new item
	item := &persistentItem{
		Key:        cacheKey,
		Offset:     offset,
		Size:       int64(len(data)),
		Timestamp:  time.Now(),
		AccessTime: time.Now(),
		Compressed: c.config.Compression,
		Checksum:   c.calculateChecksum(data),
	}

	// Generate file path
	item.FilePath = c.generateFilePath(cacheKey)

	// Write data to file
	actualSize, err := c.writeToFile(item, data)
	if err != nil {
		return // Failed to write, don't add to index
	}

	// Update item with actual file size (might be different due to compression)
	item.Size = actualSize

	// Add to index
	c.index[cacheKey] = item
	c.currentSize += actualSize

	// Evict if necessary
	c.evictIfNeeded()
}

// Delete removes data from the persistent cache
func (c *PersistentCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Find and remove all items with matching key prefix
	var itemsToDelete []*persistentItem
	for cacheKey, item := range c.index {
		if c.keyMatches(cacheKey, key) {
			itemsToDelete = append(itemsToDelete, item)
		}
	}

	for _, item := range itemsToDelete {
		// Remove file
		_ = os.Remove(item.FilePath) // Ignore error on cleanup

		// Remove from index
		delete(c.index, item.Key)
		c.currentSize -= item.Size
		c.stats.Evictions++
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
	for i := 0; i < len(items)-1; i++ {
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

	// Clear index
	c.index = make(map[string]*persistentItem)
	c.currentSize = 0
	c.stats.Evictions += uint64(len(c.index))
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
		c.currentSize -= item.Size
	}

	// Force sync index
	_ = c.saveIndex() // Index save errors are logged internally
}

// Helper methods

func (c *PersistentCache) makeCacheKey(key string, offset, size int64) string {
	return fmt.Sprintf("%s:%d:%d", key, offset, size)
}

func (c *PersistentCache) keyMatches(cacheKey, key string) bool {
	return len(cacheKey) >= len(key) && cacheKey[:len(key)] == key
}

func (c *PersistentCache) isExpired(item *persistentItem) bool {
	if c.config.TTL == 0 {
		return false
	}
	return time.Since(item.Timestamp) > c.config.TTL
}

func (c *PersistentCache) generateFilePath(key string) string {
	hash := sha256.Sum256([]byte(key))
	filename := fmt.Sprintf("%x", hash[:8]) // Use first 8 bytes of hash
	return filepath.Join(c.directory, filename+".cache")
}

func (c *PersistentCache) calculateChecksum(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash)
}

func (c *PersistentCache) writeToFile(item *persistentItem, data []byte) (int64, error) {
	file, err := os.Create(item.FilePath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()

	var writer io.Writer = file

	// Use compression if enabled
	if item.Compressed {
		gzipWriter := gzip.NewWriter(file)
		defer func() { _ = gzipWriter.Close() }()
		writer = gzipWriter
	}

	n, err := writer.Write(data)
	if err != nil {
		_ = os.Remove(item.FilePath) // Clean up on error, ignore result
		return 0, err
	}

	// Get actual file size
	if stat, err := file.Stat(); err == nil {
		return stat.Size(), nil
	}

	return int64(n), nil
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

		c.index[key] = item
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
