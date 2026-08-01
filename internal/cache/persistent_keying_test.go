package cache

// PersistentCache carried a character-identical copy of the in-memory cache's keying, and therefore an
// identical copy of its three defects. The tests here are the same properties asserted against the disk
// tier, plus the two failures that only a disk cache can have: size accounting that disagrees with the
// bytes actually written (H9), and an index that outlives the code that wrote it.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newPersistentTestCache(t *testing.T, compression bool) *PersistentCache {
	t.Helper()

	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory:       t.TempDir(),
		MaxSize:         64 << 20,
		TTL:             time.Hour,
		Compression:     compression,
		CleanupInterval: time.Hour,
		SyncInterval:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache: %v", err)
	}

	t.Cleanup(func() { _ = cache.Close() })

	return cache
}

// TestPersistentRecordedSizeMatchesDisk is H9.
//
// writeToFile returned file.Stat().Size() while the gzip.Writer that had to flush first was closed by a
// deferred call — so the size recorded was of a file still mostly buffered in memory. Measured before
// the fix: 10 bytes recorded for a 330-byte file.
//
// currentSize drives evictIfNeeded, so an undercount means eviction never fires and the cache fills the
// disk it was given a budget for. Delete then subtracted the same wrong figure, so the counter drifted
// negative over a mount's lifetime — at which point the budget stops meaning anything at all.
func TestPersistentRecordedSizeMatchesDisk(t *testing.T) {
	t.Parallel()

	for _, compression := range []bool{false, true} {
		name := "uncompressed"
		if compression {
			name = "compressed"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cache := newPersistentTestCache(t, compression)

			sizes := []int{1, 100, 4096, 330, 65536}
			for _, size := range sizes {
				key := "object-" + name + "-" + itoa(int64(size))

				// Highly compressible, which is what makes the gzip buffering visible: an incompressible
				// payload would have been flushed by sheer volume and the bug would not have shown.
				cache.Put(key, 0, bytes.Repeat([]byte("a"), size))
			}

			// What the index claims, against what the files on disk actually weigh.
			var claimed, actual int64

			cache.mu.RLock()
			for _, item := range cache.index {
				claimed += item.Size

				stat, err := os.Stat(item.FilePath)
				if err != nil {
					t.Errorf("indexed entry %q has no file: %v", item.Key, err)

					continue
				}

				actual += stat.Size()

				if item.Size != stat.Size() {
					t.Errorf("entry %q records %d bytes, file is %d bytes on disk. Eviction is driven by "+
						"the recorded figure: an undercount means the cache never evicts and fills the "+
						"disk.", item.Key, item.Size, stat.Size())
				}
			}
			cache.mu.RUnlock()

			if got := cache.Size(); got != claimed {
				t.Errorf("Size() = %d but the index totals %d", got, claimed)
			}

			if claimed != actual {
				t.Errorf("the cache believes it holds %d bytes; the files weigh %d", claimed, actual)
			}
		})
	}
}

// TestPersistentEvictionHonorsItsBudget is H9's consequence, stated as the behavior that failed.
//
// The plan's gate for this defect: "Put 4096 compressible bytes → recorded size equals on-disk size;
// Evict honors its target."
func TestPersistentEvictionHonorsItsBudget(t *testing.T) {
	t.Parallel()

	cache := newPersistentTestCache(t, true)

	// One entry per chunk, so eviction has distinct entries to choose between.
	for i := range 10 {
		cache.Put("object", int64(i)*ChunkSize, bytes.Repeat([]byte("a"), 4096))
		time.Sleep(2 * time.Millisecond) // distinct access times, so LRU order is well defined
	}

	before := cache.Size()
	if before <= 0 {
		t.Fatalf("cache reports %d bytes after ten 4096-byte puts", before)
	}

	target := before / 2

	if !cache.Evict(target) {
		t.Errorf("Evict(%d) reported failure with %d bytes held", target, before)
	}

	freed := before - cache.Size()
	if freed < target {
		t.Errorf("Evict(%d) freed %d bytes. A cache that cannot honor an eviction request cannot "+
			"honor its capacity either.", target, freed)
	}

	// And the accounting must still agree with the disk afterwards.
	var actual int64

	cache.mu.RLock()
	for _, item := range cache.index {
		if stat, err := os.Stat(item.FilePath); err == nil {
			actual += stat.Size()
		}
	}
	cache.mu.RUnlock()

	if cache.Size() != actual {
		t.Errorf("after eviction the cache claims %d bytes and the files weigh %d; the counter drifted",
			cache.Size(), actual)
	}
}

// TestPersistentCapacityIsEnforced is the other half: eviction has to fire on its own, not only when
// asked.
func TestPersistentCapacityIsEnforced(t *testing.T) {
	t.Parallel()

	const capacity = 200 << 10

	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory:       t.TempDir(),
		MaxSize:         capacity,
		TTL:             time.Hour,
		Compression:     true,
		CleanupInterval: time.Hour,
		SyncInterval:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache: %v", err)
	}
	defer func() { _ = cache.Close() }()

	// Incompressible payloads, and enough of them to pass the budget several times over.
	//
	// Compressible bytes make this test vacuous, which is not hypothetical: the first version used
	// bytes.Repeat, and 200 puts of 7000 repeating bytes weighed 11,800 bytes on disk against this
	// 204,800-byte capacity. Eviction was never reached, so the test passed and would have kept passing
	// with eviction deleted outright.
	const (
		entries = 200
		body    = 8192
	)

	payload := make([]byte, body)

	state := uint64(0x9E3779B97F4A7C15) // an xorshift walk: deterministic, and gzip cannot shrink it
	for i := range payload {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		payload[i] = byte(state)
	}

	// Write well past the budget, in distinct chunks so nothing coalesces.
	for i := range entries {
		// Vary the leading bytes per put, so no two entries are byte-identical.
		put := append([]byte{byte(i), byte(i >> 8)}, payload...)
		cache.Put("object", int64(i)*ChunkSize, put)
	}

	// Guard against this test going vacuous. If nothing was evicted, the assertions below hold trivially
	// and say nothing at all about capacity. Two different causes land here, so report enough to tell
	// them apart: either the payload compressed small enough to fit the budget (the test's own fault), or
	// the cache undercounted what it wrote and never noticed it was full (H9, the defect under test).
	cache.mu.RLock()
	held := len(cache.index)
	cache.mu.RUnlock()

	if held == entries {
		var onDisk int64

		cache.mu.RLock()
		for _, item := range cache.index {
			if stat, err := os.Stat(item.FilePath); err == nil {
				onDisk += stat.Size()
			}
		}
		cache.mu.RUnlock()

		t.Fatalf("all %d entries are still held against a %d-byte capacity, so nothing below tests "+
			"eviction. The cache believes it holds %d bytes and the files weigh %d: if those disagree, "+
			"the size accounting is undercounting and the budget is never reached; if they agree and are "+
			"both under the capacity, this test's payload is compressing further than intended.",
			entries, capacity, cache.Size(), onDisk)
	}

	if got := cache.Size(); got > capacity {
		t.Errorf("cache holds %d bytes against a %d-byte capacity. This is what the size-accounting "+
			"undercount cost: the budget is never reached, so eviction never fires and the cache grows "+
			"until the disk is full.", got, capacity)
	}

	// The files on disk must obey the same bound — the index agreeing with itself is not enough.
	var onDisk int64

	files, err := os.ReadDir(cache.directory)
	if err != nil {
		t.Fatalf("reading cache directory: %v", err)
	}

	for _, file := range files {
		if filepath.Ext(file.Name()) != ".cache" {
			continue
		}

		if info, err := file.Info(); err == nil {
			onDisk += info.Size()
		}
	}

	if onDisk > capacity {
		t.Errorf("%d bytes of cache files on disk against a %d-byte capacity", onDisk, capacity)
	}
}

// TestPersistentReadsHitRegardlessOfRequestedLength is H6 against the disk tier.
func TestPersistentReadsHitRegardlessOfRequestedLength(t *testing.T) {
	t.Parallel()

	const fileSize = 10240

	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i % 251)
	}

	cache := newPersistentTestCache(t, true)
	cache.Put("f", 0, content)

	for _, probe := range []struct {
		name     string
		off, len int64
	}{
		{"whole file", 0, fileSize},
		{"short prefix", 0, 4096},
		{"from the middle", 4096, 4096},
		{"one byte", 5000, 1},
		{"the last byte", fileSize - 1, 1},
	} {
		got := cache.Get("f", probe.off, probe.len)
		if got == nil {
			t.Errorf("%s: Get(f, %d, %d) missed after Put(f, 0, <%d bytes>)",
				probe.name, probe.off, probe.len, fileSize)

			continue
		}

		if want := content[probe.off : probe.off+probe.len]; !bytes.Equal(got, want) {
			t.Errorf("%s: Get(f, %d, %d) returned the wrong bytes", probe.name, probe.off, probe.len)
		}
	}

	// Over-requesting past what is held must still miss, for the same reason as in the memory tier: the
	// cache is never told how long the object is.
	if got := cache.Get("f", 0, 131072); got != nil {
		t.Errorf("Get(f, 0, 131072) returned %d bytes when only %d are held", len(got), fileSize)
	}
}

// TestPersistentSequentialReadsCoalesce is why Put merges rather than replaces.
//
// Consecutive reads of one file land in the same chunk — a 128 KiB kernel buffer against a 1 MiB chunk
// means eight runs per chunk. A replacing Put keeps only the last of them, so seven eighths of every
// sequentially-read file would be dropped and re-fetched on the next pass.
func TestPersistentSequentialReadsCoalesce(t *testing.T) {
	t.Parallel()

	const bufSize = 131072

	content := make([]byte, 8*bufSize) // exactly one chunk's worth
	for i := range content {
		content[i] = byte(i % 251)
	}

	cache := newPersistentTestCache(t, true)

	for off := int64(0); off < int64(len(content)); off += bufSize {
		cache.Put("f", off, content[off:off+bufSize])
	}

	// Every buffer must be re-readable, not just the last one.
	for off := int64(0); off < int64(len(content)); off += bufSize {
		if got := cache.Get("f", off, bufSize); got == nil {
			t.Errorf("re-reading [%d,%d) missed. A replacing Put keeps only the final run of each "+
				"chunk, so a sequential reader re-fetches most of what it just read.", off, off+bufSize)
		} else if !bytes.Equal(got, content[off:off+bufSize]) {
			t.Errorf("re-read of [%d,%d) returned the wrong bytes", off, off+bufSize)
		}
	}

	// And the whole chunk at once.
	whole := cache.Get("f", 0, int64(len(content)))
	if whole == nil {
		t.Fatal("a whole-chunk read missed after the chunk was filled by eight sequential reads")
	}

	if !bytes.Equal(whole, content) {
		t.Errorf("whole-chunk read returned %d of %d bytes and they differ", len(whole), len(content))
	}
}

// TestPersistentDeleteRemovesOnlyItsOwnObject is M14 against the disk tier, including the files.
func TestPersistentDeleteRemovesOnlyItsOwnObject(t *testing.T) {
	t.Parallel()

	objects := []string{"logs/app", "logs/app2", "logs/appendix", "logs/app:0:4096", "a", "ab"}

	cache := newPersistentTestCache(t, false)

	for _, object := range objects {
		cache.Put(object, 0, []byte("zero-"+object))
		cache.Put(object, 3*ChunkSize, []byte("three-"+object))
	}

	cache.Delete("logs/app")

	for _, object := range objects {
		zero := cache.Get(object, 0, int64(len("zero-"+object)))
		three := cache.Get(object, 3*ChunkSize, int64(len("three-"+object)))

		if object == "logs/app" {
			if zero != nil || three != nil {
				t.Errorf("Delete left data for its own object: chunk 0 %q, chunk 3 %q", zero, three)
			}

			continue
		}

		if zero == nil || three == nil {
			t.Errorf("Delete(\"logs/app\") also removed %q (chunk 0 present: %v, chunk 3 present: %v). "+
				"Wired into the write path, that flushes unrelated objects' cache on every write.",
				object, zero != nil, three != nil)
		}
	}

	// The deleted object's files must be gone from disk too, or the cache leaks its budget to files no
	// index entry accounts for.
	entries, err := os.ReadDir(cache.directory)
	if err != nil {
		t.Fatalf("reading cache directory: %v", err)
	}

	cacheFiles := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".cache" {
			cacheFiles++
		}
	}

	if want := 2 * (len(objects) - 1); cacheFiles != want {
		t.Errorf("%d cache files on disk after deleting one of %d objects, want %d. Orphaned files are "+
			"budget the cache can never reclaim.", cacheFiles, len(objects), want)
	}
}

// TestPersistentIndexFromAnotherVersionIsDiscarded covers the failure only a persistent cache can have.
//
// The index survives restarts and upgrades, so it is the one place that sees records written by
// different code. An entry in the pre-chunking format carries no object name and no run length. Keeping
// it would put an entry in the index that Delete cannot find — Delete works from the object name — and
// whose file the coverage check would slice at an offset it does not know. Both are silent; the recovery
// is to drop the entry and re-fetch from S3, which costs a request and nothing else.
func TestPersistentIndexFromAnotherVersionIsDiscarded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// A file, and an index entry in the old "%s:%d:%d" format pointing at it.
	stalePath := filepath.Join(dir, "deadbeefdeadbeef.cache")
	if err := os.WriteFile(stalePath, []byte("bytes from an older objectfs"), 0o600); err != nil {
		t.Fatalf("seeding a stale cache file: %v", err)
	}

	stale := map[string]*persistentItem{
		// The old format: object, offset, requested length — no Object, no ChunkIndex, no Length.
		"logs/app:0:4096": {
			Key:        "logs/app:0:4096",
			FilePath:   stalePath,
			Size:       28,
			Timestamp:  time.Now(),
			AccessTime: time.Now(),
			Checksum:   "unverifiable",
		},
		// A record in the current shape whose key does not match its own fields — corrupt either way,
		// and indexing it would let a read of one object serve another's bytes.
		entryKey("other", 7): {
			Key:        entryKey("elsewhere", 7),
			Object:     "elsewhere",
			ChunkIndex: 7,
			Start:      0,
			Length:     10,
			FilePath:   stalePath,
			Size:       28,
			Timestamp:  time.Now(),
			AccessTime: time.Now(),
		},
	}

	indexFile := filepath.Join(dir, "cache-index.json")
	blob, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshaling a stale index: %v", err)
	}

	if err := os.WriteFile(indexFile, blob, 0o600); err != nil {
		t.Fatalf("writing a stale index: %v", err)
	}

	cache, err := NewPersistentCache(&PersistentCacheConfig{
		Directory:       dir,
		MaxSize:         64 << 20,
		TTL:             time.Hour,
		CleanupInterval: time.Hour,
		SyncInterval:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPersistentCache over a stale index: %v", err)
	}
	defer func() { _ = cache.Close() }()

	cache.mu.RLock()
	entries := len(cache.index)
	indexed := len(cache.byObject)
	size := cache.currentSize
	cache.mu.RUnlock()

	if entries != 0 {
		t.Errorf("%d entries loaded from an index written in another format; each is an entry Delete "+
			"cannot invalidate and whose bytes would be sliced at an unknown offset", entries)
	}

	if indexed != 0 {
		t.Errorf("%d objects indexed from unusable entries", indexed)
	}

	if size != 0 {
		t.Errorf("currentSize = %d after discarding every entry; the budget would start out already "+
			"spent on data the cache cannot serve", size)
	}

	// And the orphaned file must be removed rather than left to occupy the budget forever.
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("the discarded entry's file is still on disk; nothing will ever account for or remove " +
			"it, so it is permanently lost capacity")
	}

	// The cache must be usable afterwards, which is the point of recovering rather than erroring.
	cache.Put("logs/app", 0, []byte("fresh"))

	if got := cache.Get("logs/app", 0, 5); string(got) != "fresh" {
		t.Errorf("after discarding a stale index the cache returned %q for a fresh put", got)
	}
}

// TestPersistentIndexSurvivesRestart is the counterweight: discarding another version's entries must not
// become discarding this version's.
func TestPersistentIndexSurvivesRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	config := &PersistentCacheConfig{
		Directory:       dir,
		MaxSize:         64 << 20,
		TTL:             time.Hour,
		Compression:     true,
		CleanupInterval: time.Hour,
		SyncInterval:    time.Hour,
	}

	first, err := NewPersistentCache(config)
	if err != nil {
		t.Fatalf("NewPersistentCache: %v", err)
	}

	content := make([]byte, 8192)
	for i := range content {
		content[i] = byte(i % 251)
	}

	first.Put("data/results.parquet", 0, content)
	first.Put("data/results.parquet", 4*ChunkSize, content)

	if err := first.Close(); err != nil {
		t.Fatalf("Close (which persists the index): %v", err)
	}

	second, err := NewPersistentCache(config)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = second.Close() }()

	for _, off := range []int64{0, 4 * ChunkSize} {
		got := second.Get("data/results.parquet", off, int64(len(content)))
		if got == nil {
			t.Errorf("chunk at offset %d did not survive a restart; a disk cache that forgets on restart "+
				"is a disk cache for no reason", off)

			continue
		}

		if !bytes.Equal(got, content) {
			t.Errorf("chunk at offset %d came back with different bytes after a restart", off)
		}
	}

	// Delete must work against the reloaded index, which means byObject was rebuilt on load.
	second.Delete("data/results.parquet")

	for _, off := range []int64{0, 4 * ChunkSize} {
		if got := second.Get("data/results.parquet", off, int64(len(content))); got != nil {
			t.Errorf("Delete did not remove the reloaded chunk at offset %d. If byObject is not rebuilt "+
				"on load, every entry from before a restart is permanently uninvalidatable — a write "+
				"then serves pre-write bytes until the TTL expires.", off)
		}
	}
}

// TestPersistentCorruptFileIsNotServed pins what happens when the disk lies.
//
// A cache file whose contents no longer match its checksum must produce a miss, not an error and not
// the bad bytes. The object is still in S3, so the correct response is to forget the entry.
func TestPersistentCorruptFileIsNotServed(t *testing.T) {
	t.Parallel()

	cache := newPersistentTestCache(t, false)

	content := []byte("the original content")
	cache.Put("f", 0, content)

	cache.mu.RLock()
	item := cache.index[entryKey("f", 0)]
	cache.mu.RUnlock()

	if item == nil {
		t.Fatal("nothing was indexed")
	}

	// Same length, different bytes: only the checksum can tell.
	if err := os.WriteFile(item.FilePath, []byte("the corrupted conten"), 0o600); err != nil {
		t.Fatalf("corrupting the cache file: %v", err)
	}

	if got := cache.Get("f", 0, int64(len(content))); got != nil {
		t.Errorf("a corrupt cache file was served as %q. The checksum exists precisely so this cannot "+
			"reach the caller.", got)
	}

	// And the bad entry must be gone rather than re-read on every subsequent request.
	cache.mu.RLock()
	_, stillIndexed := cache.index[entryKey("f", 0)]
	cache.mu.RUnlock()

	if stillIndexed {
		t.Error("the corrupt entry is still indexed, so every read of this range pays for a failed " +
			"decode before falling through to S3")
	}
}
