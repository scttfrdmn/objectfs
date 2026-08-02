package cache

// Four defects in the byte-range cache, each verified by execution against v0.10.0 before the rekeying
// landed. Each test below reproduces one of them and names what it cost, because all four were
// invisible: the cache reported plausible statistics, served correct-looking data, and simply did no
// work — or the wrong work — while every existing test passed.
//
// The probe that established them printed, against the old code:
//
//	MISS: Get(f,0,131072) after Put(f,0,<10240 bytes>) -- the cat(1) shape
//	MISS: Get(f,0,4096) -- a short re-read of cached bytes
//	HIT for exact 10240 (10240 bytes)
//	MISS: getCachedInfo asks 8192, cacheInfo stored 138 -- metadata cache never hits
//	logs/app       -> GONE
//	logs/app2      -> GONE
//	logs/appendix  -> GONE
//	after Put(OLD): Get -> "OLD"

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func newTestCache(t *testing.T) *LRUCache {
	t.Helper()

	cache := NewLRUCache(&CacheConfig{
		MaxSize:         64 << 20,
		MaxEntries:      100000,
		TTL:             time.Hour,
		CleanupInterval: time.Hour,
	})

	t.Cleanup(func() { _ = cache.Close() })

	return cache
}

// TestReadsHitRegardlessOfRequestedLength is the length-in-key defect (H6).
//
// The old key was fmt.Sprintf("%s:%d:%d", key, offset, size) — the requested length was part of the
// identity of the stored bytes. A caller could only hit by asking for exactly the length a previous
// caller had stored, which is not how reads arrive: the kernel hands FUSE a fixed-size buffer, so the
// length requested is the buffer's, never the file's.
//
// Cost: small files were never served from cache. Every re-read of a 10 KiB file went to S3, forever,
// while the cache held those exact bytes and counted a miss.
func TestReadsHitRegardlessOfRequestedLength(t *testing.T) {
	t.Parallel()

	const fileSize = 10240

	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i % 251)
	}

	tests := []struct {
		name   string
		offset int64
		size   int64
		want   []byte
		why    string
	}{
		{
			name: "the cat(1) shape, clamped by the caller", offset: 0, size: fileSize, want: content,
			why: "a read of the whole file arrives at the FUSE layer as offset 0 with the kernel's " +
				"128 KiB buffer size, and that layer clamps to the file length before asking here. " +
				"Under the old keying the stored length had to match the *requested* length, so even " +
				"the clamped form missed unless the earlier Put had been the same shape",
		},
		{
			name: "a short re-read", offset: 0, size: 4096, want: content[:4096],
			why: "asking for less than is held must hit; the bytes are right there",
		},
		{
			name: "a short re-read from the middle", offset: 4096, size: 4096,
			want: content[4096:8192],
			why:  "and from any offset within what is held",
		},
		{
			name: "one byte", offset: 5000, size: 1, want: content[5000:5001],
		},
		{
			name: "exactly what was stored", offset: 0, size: fileSize, want: content,
			why: "the one case the old key did handle, which is why this looked like it worked",
		},
		{
			name: "the tail", offset: fileSize - 1, size: 1, want: content[fileSize-1:],
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cache := newTestCache(t)
			cache.Put("f", 0, content)

			got := cache.Get("f", tt.offset, tt.size)
			if got == nil {
				t.Fatalf("Get(f, %d, %d) missed after Put(f, 0, <%d bytes>). %s",
					tt.offset, tt.size, fileSize, tt.why)
			}

			// A hit must return only the bytes asked for. Returning the whole held run would overrun
			// the kernel's buffer, and returning a longer slice than requested is how a short file
			// comes back looking longer than it is.
			if int64(len(got)) != tt.size && int64(len(got)) != int64(len(tt.want)) {
				t.Errorf("Get(f, %d, %d) returned %d bytes; a hit must return exactly the range asked "+
					"for", tt.offset, tt.size, len(got))
			}

			if !bytes.Equal(got, tt.want) {
				t.Errorf("Get(f, %d, %d) returned the wrong bytes:\n got %d bytes starting %x\nwant %d "+
					"bytes starting %x", tt.offset, tt.size, len(got), first16(got),
					len(tt.want), first16(tt.want))
			}
		})
	}
}

// TestGetOverRequestingBeyondHeldBytesMisses is the counterweight to the test above, and the reason
// the FUSE read path has to clamp its request to the file size rather than pass the kernel's buffer
// length through.
//
// Tolerating any requested length must not become tolerating requests the cache cannot satisfy. The
// cache cannot distinguish "the object ends here" from "only this much is cached" — it is never told
// the object's length — so a request extending past what is held must miss. Answering with the short
// prefix would hand the FUSE layer a buffer the kernel reads as a truncated file, which is the
// silent-corruption outcome the whole rekeying exists to avoid.
func TestGetOverRequestingBeyondHeldBytesMisses(t *testing.T) {
	t.Parallel()

	cache := newTestCache(t)

	// Chunk 0 holds [0, 4096) only.
	cache.Put("f", 0, make([]byte, 4096))

	if got := cache.Get("f", 0, 131072); got != nil {
		t.Errorf("Get(f, 0, 131072) returned %d bytes when only 4096 are held. The cache is never told "+
			"how long the object is, so it cannot know whether the shortfall is the end of the file or "+
			"the end of what it cached — and the FUSE layer hands this buffer to the kernel as file "+
			"content. Callers must clamp to the file size; the cache must not guess.", len(got))
	}

	if got := cache.Get("f", 4096, 4096); got != nil {
		t.Errorf("Get past the end of what is held returned %d bytes; nothing was ever stored there",
			len(got))
	}

	if got := cache.Get("f", 2048, 4096); got != nil {
		t.Errorf("Get straddling the end of what is held returned %d bytes. A partial hit must be a "+
			"miss: the FUSE layer hands this buffer to the kernel, which reads a short buffer as a "+
			"short file", len(got))
	}

	// And across a chunk boundary where only the first chunk is present.
	cache2 := newTestCache(t)
	cache2.Put("g", 0, make([]byte, ChunkSize))

	if got := cache2.Get("g", 0, ChunkSize+1); got != nil {
		t.Errorf("Get spanning into an absent chunk returned %d bytes", len(got))
	}
}

// TestSequentialReadsCoalesceAndHit is the defect the first version of this rewrite would have shipped.
//
// A sequential reader's second buffer starts at 131072 — partway into chunk 0. An entry design that
// could only be anchored at a chunk's start would cache the first buffer and then nothing at all for
// the rest of the file. The runs must accumulate.
func TestSequentialReadsCoalesceAndHit(t *testing.T) {
	t.Parallel()

	const bufSize = 131072
	const buffers = 12 // spans past one 1 MiB chunk boundary

	content := make([]byte, bufSize*buffers)
	for i := range content {
		content[i] = byte(i % 251)
	}

	cache := newTestCache(t)

	// Read it the way the kernel does.
	for off := int64(0); off < int64(len(content)); off += bufSize {
		end := min(off+bufSize, int64(len(content)))
		cache.Put("f", off, content[off:end])
	}

	// Now every one of those reads must hit, and so must the whole range at once.
	for off := int64(0); off < int64(len(content)); off += bufSize {
		end := min(off+bufSize, int64(len(content)))

		got := cache.Get("f", off, end-off)
		if got == nil {
			t.Fatalf("re-reading [%d,%d) missed after a full sequential pass. A sequential reader that "+
				"cannot re-read its own data has no cache at all.", off, end)
		}

		if !bytes.Equal(got, content[off:end]) {
			t.Fatalf("re-read of [%d,%d) returned the wrong bytes", off, end)
		}
	}

	whole := cache.Get("f", 0, int64(len(content)))
	if whole == nil {
		t.Fatalf("a whole-file read missed after the file was read sequentially in full. The runs did "+
			"not coalesce across %d chunks.", chunkIndexOf(int64(len(content)))+1)
	}

	if !bytes.Equal(whole, content) {
		t.Errorf("whole-file read returned %d of %d bytes and they differ", len(whole), len(content))
	}

	// The entry count is the point of coalescing: 12 reads spanning 2 chunks must not be 12 entries.
	cache.mu.RLock()
	entries := len(cache.items)
	cache.mu.RUnlock()

	if wantMax := int(chunkIndexOf(int64(len(content)))) + 1; entries > wantMax {
		t.Errorf("%d reads produced %d entries, want at most %d (one per chunk). Runs are not "+
			"coalescing, so per-entry overhead grows with read count rather than with data size",
			buffers, entries, wantMax)
	}
}

// TestMetadataCacheHits is H5's other half, and the reason `stat` cost an S3 HEAD per path component.
//
// internal/fuse's getCachedInfo cannot state the length of what it is looking up: the payload is a
// marshaled ObjectInfo whose size varies with the path and the object's metadata. It asked for a fixed
// 8192 as a generous bound, while cacheInfo stored the ~200 bytes the payload actually was — different
// entries under the old key, so the metadata cache never hit once, for any path, for the life of the
// mount, while reporting hits and misses as though it were working.
//
// This is what the types.Cache contract's "size <= 0" shape is for: return the contiguous run held
// from offset, whatever its length. It is available only to callers caching a whole value, because a
// caller reading file content cannot distinguish a short answer from a short file.
func TestMetadataCacheHits(t *testing.T) {
	t.Parallel()

	// Payload lengths spanning what a real ObjectInfo produces, since the point is that the caller does
	// not know which of these it is about to get.
	payloads := map[string][]byte{
		"short path": []byte(`{"key":"a","size":1,"etag":"d41d8cd98f00b204e9800998ecf8427e"}`),
		"typical": []byte(`{"key":"data/experiment/run-01/results.parquet","size":1048576,` +
			`"last_modified":"2026-07-31T00:00:00Z","etag":"d41d8cd98f00b204e9800998ecf8427e",` +
			`"storage_class":"STANDARD","content_type":"application/octet-stream"}`),
		"long": append([]byte(`{"key":"`), append(bytes.Repeat([]byte("nested/"), 100),
			[]byte(`x","size":2}`)...)...),
	}

	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cache := newTestCache(t)
			const metaKey = "__meta__data/experiment/run-01/results.parquet"

			cache.Put(metaKey, 0, payload)

			// The getCachedInfo shape: "give me what you have for this key".
			got := cache.Get(metaKey, 0, 0)
			if got == nil {
				t.Fatalf("the metadata cache missed for a %d-byte payload. Every stat(2) on every path "+
					"component then issues an S3 HeadObject, for the life of the mount.", len(payload))
			}

			if !bytes.Equal(got, payload) {
				t.Errorf("metadata round trip returned %d bytes, stored %d, and they differ",
					len(got), len(payload))
			}

			stats := cache.Stats()
			if stats.HitRate == 0 {
				t.Errorf("hit rate is 0 after a successful lookup; the plan's gate for this defect is a "+
					"non-zero metadata hit rate. Stats: %+v", stats)
			}
		})
	}
}

// TestOpenEndedGetStaysWithinOneChunk pins the bound on the size<=0 shape.
//
// A caller that cannot say how much it wants also cannot be told where the object ends, so the answer
// must not depend on how many adjacent chunks happen to be resident. Spanning chunks would make the
// returned length a function of eviction state.
func TestOpenEndedGetStaysWithinOneChunk(t *testing.T) {
	t.Parallel()

	cache := newTestCache(t)

	// Two full chunks, contiguous.
	cache.Put("f", 0, make([]byte, 2*ChunkSize))

	got := cache.Get("f", 0, 0)
	if int64(len(got)) != ChunkSize {
		t.Errorf("an open-ended Get returned %d bytes across a chunk boundary; want at most %d. The "+
			"length must not depend on which neighboring chunks are resident.", len(got), ChunkSize)
	}

	// Nothing held at the offset is still a miss, not an empty slice — callers test for nil.
	if got := cache.Get("f", 5*ChunkSize, 0); got != nil {
		t.Errorf("an open-ended Get where nothing is held returned %d bytes", len(got))
	}

	// A run that starts *after* the requested offset says nothing about the bytes at that offset.
	cache.Put("g", 100, make([]byte, 50))

	if got := cache.Get("g", 0, 0); got != nil {
		t.Errorf("an open-ended Get at offset 0 returned %d bytes when the held run starts at 100; "+
			"those are not the bytes at offset 0", len(got))
	}
}

// TestDeleteRemovesOnlyItsOwnObject is M14, the over-deletion defect.
//
// Delete compared cacheKey[:len(key)] == key, so any object whose name began with the deleted name
// went too. Verified by execution: Delete("logs/app") left logs/app2 and logs/appendix both GONE.
//
// This test had to exist before invalidation was wired into the write path, because that is what makes
// the defect live: with no Delete calls in internal/fuse the bug was latent, and adding invalidation
// to a prefix-matching Delete would have made every write flush its siblings' cached data.
func TestDeleteRemovesOnlyItsOwnObject(t *testing.T) {
	t.Parallel()

	// Siblings that a prefix compare cannot distinguish, plus keys containing the old ":" delimiter and
	// the digits a naive parse would mistake for an offset.
	objects := []string{
		"logs/app",
		"logs/app2",
		"logs/appendix",
		"logs/app/nested",
		"logs/app:0",
		"logs/app:0:4096",
		"logs/ap",
		"logs",
		"a",
		"ab",
	}

	for _, target := range objects {
		t.Run("delete "+target, func(t *testing.T) {
			t.Parallel()

			cache := newTestCache(t)

			// Give each object bytes at two chunk indices, so Delete has to find both and the survivors
			// have to keep both.
			for _, object := range objects {
				cache.Put(object, 0, []byte("chunk-zero-of-"+object))
				cache.Put(object, 2*ChunkSize, []byte("chunk-two-of-"+object))
			}

			cache.Delete(target)

			for _, object := range objects {
				zero := cache.Get(object, 0, int64(len("chunk-zero-of-"+object)))
				two := cache.Get(object, 2*ChunkSize, int64(len("chunk-two-of-"+object)))

				if object == target {
					if zero != nil || two != nil {
						t.Errorf("Delete(%q) left data behind (chunk 0: %q, chunk 2: %q). Invalidation "+
							"that misses an entry serves pre-write bytes for up to the TTL.",
							target, zero, two)
					}

					continue
				}

				if zero == nil {
					t.Errorf("Delete(%q) also removed chunk 0 of %q. Over-deletion costs re-fetches of "+
						"unrelated objects on every write.", target, object)
				}

				if two == nil {
					t.Errorf("Delete(%q) also removed chunk 2 of %q", target, object)
				}
			}
		})
	}
}

// TestPutReplacesStaleBytes is the write-invalidation half of H5.
//
// The probe's last line — `after Put(OLD): Get -> "OLD"` — is the shape of the read-after-write bug: a
// cache holding an object's previous content answers reads with it. Nothing in the Cache interface can
// say "this is a different version of f", so a later Put of the same range must win outright, and the
// write path must call Delete.
func TestPutReplacesStaleBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		first, second string
		offset        int64
		why           string
	}{
		{
			name: "same length", first: "OLD", second: "NEW", offset: 0,
			why: "the case a size comparison cannot catch",
		},
		{
			name: "shorter replacement", first: "OLD CONTENT HERE", second: "NEW", offset: 0,
			why: "a truncating overwrite. The tail of the old content must not survive to be " +
				"appended to a later read",
		},
		{
			name: "longer replacement", first: "OLD", second: "NEW CONTENT HERE", offset: 0,
		},
		{
			name: "mid-chunk overwrite", first: "OLD", second: "NEW", offset: 4096,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cache := newTestCache(t)

			cache.Put("f", tt.offset, []byte(tt.first))
			cache.Put("f", tt.offset, []byte(tt.second))

			got := cache.Get("f", tt.offset, int64(len(tt.second)))
			if string(got) != tt.second {
				t.Errorf("after overwriting %q with %q, Get returned %q. %s",
					tt.first, tt.second, got, tt.why)
			}

			// A shorter replacement must not leave the old tail reachable.
			if len(tt.second) < len(tt.first) {
				if tail := cache.Get("f", tt.offset, int64(len(tt.first))); tail != nil {
					t.Errorf("reading the old length after a shorter overwrite returned %q; the stale "+
						"tail is still being served", tail)
				}
			}
		})
	}
}

// TestSizeAccountingTracksHeldBytes checks the bookkeeping the rekeying had to carry with it.
//
// currentSize gates eviction, so an undercount means the cache grows past its capacity and an
// overcount means it evicts data it is holding. Coalescing makes this non-trivial: merging two runs
// must adjust the total by the difference, not add the whole incoming run.
func TestSizeAccountingTracksHeldBytes(t *testing.T) {
	t.Parallel()

	cache := newTestCache(t)

	assertSize := func(want int64, after string) {
		t.Helper()

		if got := cache.Size(); got != want {
			t.Errorf("Size() = %d after %s, want %d. Eviction is driven by this number: an undercount "+
				"lets the cache exceed its capacity, an overcount evicts live data.", got, after, want)
		}
	}

	cache.Put("f", 0, make([]byte, 1000))
	assertSize(1000, "one 1000-byte put")

	// Abutting: the run grows to 1500, so the total is 1500 rather than 2500.
	cache.Put("f", 1000, make([]byte, 500))
	assertSize(1500, "an abutting 500-byte put")

	// Fully covered by what is held: no growth at all.
	cache.Put("f", 200, make([]byte, 100))
	assertSize(1500, "a put entirely inside the held run")

	// Overlapping and extending: [0,1500) merged with [1400,1900) is [0,1900).
	cache.Put("f", 1400, make([]byte, 500))
	assertSize(1900, "an overlapping put extending the run")

	// A disjoint put replaces the run, since an entry holds one contiguous range. The total must
	// *shrink* to the new run's length — the case an add-only accounting gets wrong.
	cache.Put("f", 100000, make([]byte, 10))
	assertSize(10, "a disjoint put that replaced the run")

	// A second object accumulates independently.
	cache.Put("g", 0, make([]byte, 700))
	assertSize(710, "a put to a second object")

	cache.Delete("f")
	assertSize(700, "deleting the first object")

	cache.Delete("g")
	assertSize(0, "deleting the second object")

	// And the object index must be empty, or every deleted object leaks a map entry for the life of
	// the mount.
	cache.mu.RLock()
	indexed := len(cache.byObject)
	items := len(cache.items)
	cache.mu.RUnlock()

	if indexed != 0 || items != 0 {
		t.Errorf("after deleting everything: %d items and %d indexed objects remain", items, indexed)
	}
}

// TestEvictionKeepsTheIndexConsistent guards the invariant Delete depends on.
//
// byObject must hold exactly the chunks items holds. If eviction removes an item without updating the
// index, Delete later tries to remove a chunk that is gone — harmless — but if the reverse happens, a
// chunk exists that Delete cannot find, and that chunk serves stale bytes past a write forever.
func TestEvictionKeepsTheIndexConsistent(t *testing.T) {
	t.Parallel()

	// Small enough that filling it forces eviction.
	cache := NewLRUCache(&CacheConfig{
		MaxSize:         10000,
		MaxEntries:      5,
		TTL:             time.Hour,
		CleanupInterval: time.Hour,
	})
	defer func() { _ = cache.Close() }()

	for i := range 50 {
		key := fmt.Sprintf("object-%d", i)
		cache.Put(key, 0, make([]byte, 1000))
		cache.Put(key, 5*ChunkSize, make([]byte, 1000))
	}

	cache.Evict(3000)

	cache.mu.RLock()
	defer cache.mu.RUnlock()

	// Every item is indexed.
	for entry, item := range cache.items {
		indices, ok := cache.byObject[item.object]
		if !ok {
			t.Fatalf("item %q belongs to object %q, which is absent from the index. Delete(%q) would "+
				"not find it, so it serves stale bytes past a write for the life of the mount.",
				entry, item.object, item.object)
		}

		if _, ok := indices[item.index]; !ok {
			t.Fatalf("item %q (object %q chunk %d) is not in its object's index",
				entry, item.object, item.index)
		}
	}

	// And every index entry has an item.
	for object, indices := range cache.byObject {
		if len(indices) == 0 {
			t.Errorf("object %q has an empty index map; deleted objects must not leak entries", object)
		}

		for index := range indices {
			if _, ok := cache.items[entryKey(object, index)]; !ok {
				t.Errorf("index claims object %q holds chunk %d, but no such item exists",
					object, index)
			}
		}
	}

	// currentSize must equal what is actually held.
	var held int64
	for _, item := range cache.items {
		held += item.size()
	}

	if cache.currentSize != held {
		t.Errorf("currentSize = %d but items hold %d bytes; the accounting drifted during eviction",
			cache.currentSize, held)
	}
}

func first16(b []byte) []byte {
	if len(b) > 16 {
		return b[:16]
	}

	return b
}
