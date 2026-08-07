package cache

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestNewPersistentCache tests cache creation with various configurations
func TestNewPersistentCache(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

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
	t.Parallel()

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
	cacheKey := entryKey(key, chunkIndexOf(offset))
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
	t.Parallel()

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
	t.Parallel()

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
			t.Parallel()

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
			cacheKey := entryKey(key, 0)
			item := cache.index[cacheKey]
			if item.Compressed != tt.compression {
				t.Errorf("expected compressed=%v, got %v", tt.compression, item.Compressed)
			}
		})
	}
}

// TestPersistentCache_TTLExpiration tests TTL-based expiration.
//
// The TTL is 2s rather than the 100ms it used to be, because 100ms made this test flaky in a way
// that reported as a cache defect. `Put` stamps `Timestamp: now` at entry (persistent.go:305) and
// then gzip-encodes and writes to disk, so the item's clock starts before the write completes. Any
// delay between `Put` returning and the "immediately after Put" `Get` — GC, scheduler contention on
// a shared runner, a slow temp filesystem — eats into the same 100ms the assertion depends on. It
// failed exactly that way on a config-only PR. Reproduced deliberately by sleeping 120ms after the
// Put: the item is gone before it is ever read.
//
// 2s is not a cure for a race; it is a budget wide enough that scheduling noise cannot reach it,
// while `expiryWait` keeps the test's runtime governed by the TTL rather than a hardcoded sleep.
// Stamping the timestamp after the write would also fix the flake, but TTL-measured-from-write-start
// is the defensible semantic — the entry is as old as its data — so the test is what changes.
func TestPersistentCache_TTLExpiration(t *testing.T) {
	t.Parallel()

	const ttl = 2 * time.Second

	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       ttl,
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

	expiryWait(ttl)

	// Should be expired
	retrieved := cache.Get(key, 0, int64(len(data)))
	if retrieved != nil {
		t.Error("item should have expired")
	}
}

// expiryWait sleeps until a TTL of the given length has certainly elapsed, with enough margin that
// a slow runner cannot land inside the window. Callers assert on expiry after this returns.
func expiryWait(ttl time.Duration) {
	time.Sleep(ttl + ttl/2)
}

// TestPersistentCache_Delete tests Delete operation
func TestPersistentCache_Delete(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	// Two objects, one holding bytes in two different chunks. Entry counts are not asserted: how many
	// entries a set of puts produces is a property of the chunking, not of Delete.
	cache.Put("user:123", 0, []byte("data1"))
	cache.Put("user:123", 2*ChunkSize, []byte("data2"))
	cache.Put("user:456", 0, []byte("data3"))

	cache.Delete("user:123")

	if cache.Get("user:123", 0, 5) != nil {
		t.Error("user:123 chunk 0 should be deleted")
	}
	if cache.Get("user:123", 2*ChunkSize, 5) != nil {
		t.Error("user:123 chunk 2 should be deleted; Delete must remove every chunk of its object")
	}
	if cache.Get("user:456", 0, 5) == nil {
		t.Error("user:456 should still exist")
	}
}

// TestPersistentCache_Eviction tests eviction when cache is full
func TestPersistentCache_Eviction(t *testing.T) {
	t.Parallel()

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
	for i := range 5 {
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
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	// Five separate entries, one per chunk. Putting them at 100-byte spacing instead would coalesce
	// into a single entry within chunk 0, leaving nothing for Evict to choose between.
	for i := range 5 {
		cache.Put("key", int64(i)*ChunkSize, make([]byte, 100))
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
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	// Ten puts spread across ten chunks, so each is its own entry.
	for i := range 10 {
		cache.Put("key", int64(i)*ChunkSize, []byte("data"))
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

// TestPersistentCache_Optimize tests Optimize operation.
//
// 2s TTL for the reason given on TestPersistentCache_TTLExpiration, and this one was the more
// fragile of the two at 100ms: it asserts `finalCount == 1`, which requires key4's own Put plus
// Optimize to finish inside the TTL. Three writes, a sleep, a fourth write and a full index sweep
// against a 100ms budget is a coin flip on a loaded runner, and losing it looks like Optimize
// evicting a fresh entry.
func TestPersistentCache_Optimize(t *testing.T) {
	t.Parallel()

	const ttl = 2 * time.Second

	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       ttl,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}

	// Add items
	cache.Put("key1", 0, []byte("data1"))
	cache.Put("key2", 0, []byte("data2"))
	cache.Put("key3", 0, []byte("data3"))

	// Wait for items to expire
	expiryWait(ttl)

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
	t.Parallel()

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
	t.Parallel()

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
	cacheKey := entryKey(key, 0)
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

// TestPersistentCache_FilenameUsesFullHash pins the on-disk filename width (audit finding L25).
//
// The cache used to name files after the first 8 bytes of the key's SHA-256. Sixty-four bits is
// reachable by a long-lived cache on a large bucket, and this test asserts the full digest is used so
// a later "shorter names are tidier" change has to argue with a failing test.
func TestPersistentCache_FilenameUsesFullHash(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory: tmpDir,
		MaxSize:   10 * 1024 * 1024,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}
	defer func() { _ = cache.Close() }()

	key := entryKey("some/object", 0)
	got := filepath.Base(cache.generateFilePath(key))

	want := fmt.Sprintf("%x.cache", sha256.Sum256([]byte(key)))
	if got != want {
		t.Errorf("generateFilePath basename = %q, want %q", got, want)
	}

	// 64 hex characters plus ".cache". Stated as a length too, because that is the property that
	// matters and it fails loudly if the digest is ever truncated some other way.
	if len(got) != sha256.Size*2+len(".cache") {
		t.Errorf("filename %q is %d chars; a full SHA-256 digest plus .cache is %d",
			got, len(got), sha256.Size*2+len(".cache"))
	}
}

// TestPersistentCache_CollidingFilenameMisses records what a filename collision actually costs, which
// is not corruption: the index is keyed on the full entry key and readFromFile verifies a full SHA-256
// of the content, so a collision is a miss.
//
// The collision is simulated rather than found — with the full digest in use it is not reachable —
// by pointing one entry's FilePath at another entry's file. That makes this a test of the checksum
// guard, which is the thing standing between a collision and wrong bytes. Deleting the check in
// readFromFile makes this test report served bytes.
func TestPersistentCache_CollidingFilenameMisses(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory:   tmpDir,
		MaxSize:     10 * 1024 * 1024,
		TTL:         time.Hour,
		Compression: false,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}
	defer func() { _ = cache.Close() }()

	alpha, beta := []byte("AAAAAAAA"), []byte("BBBBBBBB")
	cache.Put("alpha", 0, alpha)
	cache.Put("beta", 0, beta)

	alphaItem := cache.index[entryKey("alpha", 0)]
	betaItem := cache.index[entryKey("beta", 0)]
	if alphaItem == nil || betaItem == nil {
		t.Fatal("both entries should be indexed after Put")
	}

	// Collide: alpha's own file goes away and its index entry points at beta's.
	if err := os.Remove(alphaItem.FilePath); err != nil {
		t.Fatalf("removing alpha's file: %v", err)
	}
	alphaItem.FilePath = betaItem.FilePath

	if got := cache.Get("alpha", 0, int64(len(alpha))); got != nil {
		t.Errorf("collision served %q under key alpha; the content checksum must turn this into a miss", got)
	}
}

// TestPersistentCache_ChecksumIsWhatCatchesWrongBytes states the same guard directly, without the
// collision framing: an entry whose stored checksum does not describe its file must not be served.
func TestPersistentCache_ChecksumIsWhatCatchesWrongBytes(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory:   tmpDir,
		MaxSize:     10 * 1024 * 1024,
		TTL:         time.Hour,
		Compression: false,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache failed: %v", err)
	}
	defer func() { _ = cache.Close() }()

	data := []byte("the bytes that were stored")
	cache.Put("obj", 0, data)

	item := cache.index[entryKey("obj", 0)]
	if item == nil {
		t.Fatal("entry should be indexed after Put")
	}

	// Same length, different content — so a length check alone would pass it.
	wrong := []byte("the bytes that were WRONGED")[:len(data)]
	if err := os.WriteFile(item.FilePath, wrong, 0600); err != nil {
		t.Fatalf("rewriting the cache file: %v", err)
	}

	if got, err := cache.readFromFile(item); err == nil {
		t.Errorf("readFromFile returned %q with no error; the checksum must reject it", got)
	}
	if got := cache.Get("obj", 0, int64(len(data))); got != nil {
		t.Errorf("Get served %q from a file whose checksum does not match", got)
	}
}

// TestPersistentCache_ConcurrentAccess tests thread-safety
func TestPersistentCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()

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
	for i := range numGoroutines {
		go func(id int) {
			defer wg.Done()
			for j := range numOpsPerGoroutine {
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
	for i := range numGoroutines {
		go func(id int) {
			defer wg.Done()
			for j := range numOpsPerGoroutine {
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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
