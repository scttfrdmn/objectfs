// Package cachetest holds the shared conformance suite for types.Cache implementations.
//
// There are five of them — LRUCache, PersistentCache, MultiLevelCache, PredictiveCache and the
// Redis-backed cache — and until this package existed each was tested only against itself. That is
// how the Redis cache came to violate the contract's central promise while passing its own suite: its
// tests exercised Get(key, 0, 0) and Get(key, 2, 4) against entries long enough to satisfy them, and
// never asked for a range the cache did not hold in full.
//
// It matters because of what the caller does with the answer. internal/fuse hands a cache hit
// straight to the kernel as file content:
//
//	if cachedData := fh.fs.cache.Get(fh.file.path, off, want); cachedData != nil {
//	    return fuse.ReadResultData(cachedData), 0
//	}
//
// A cache that returns 2 bytes when asked for 10 is therefore not a slow cache or a lossy one — it is
// a truncated read reported as a successful one, which is audit finding H7's shape arriving through a
// different door. So "a partial hit is a miss" is not a style rule about return values; it is the only
// thing that makes that call site safe, and it has to hold for every implementation the mount can be
// configured to use rather than for the ones whose tests happened to ask.
//
// The suite lives in a non-test package so both internal/cache and internal/cache/redis can call it
// without an import cycle, and so a sixth implementation added anywhere in the tree can be enrolled in
// one line.
package cachetest

import (
	"bytes"
	"testing"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// RunContract exercises cache against every promise types.Cache makes about Get, Put and Delete.
//
// newCache is called once per subtest rather than a single instance being shared, because several of
// the cases below depend on a key being absent and a suite that shares state cannot distinguish "this
// cache missed" from "an earlier subtest already stored it". Implementations that need cleanup should
// register it with t.Cleanup inside newCache.
//
// Deliberately not table-driven over a list of implementations here: each caller enrolls its own, so a
// package that cannot import another (redis, in particular) still runs the same assertions.
func RunContract(t *testing.T, newCache func(t *testing.T) types.Cache) {
	t.Helper()

	t.Run("a hit returns exactly the bytes asked for", func(t *testing.T) {
		c := newCache(t)
		c.Put("obj", 0, []byte("0123456789"))

		got := c.Get("obj", 2, 4)
		if !bytes.Equal(got, []byte("2345")) {
			t.Errorf("Get(obj,2,4) over a 10-byte entry = %q, want %q", got, "2345")
		}
	})

	t.Run("a partial hit is a miss", func(t *testing.T) {
		c := newCache(t)

		// Ten bytes cached; a read straddling the end of them. The object itself may well be longer —
		// the cache does not know the object's length and must not guess — so the only safe answer is
		// nil, leaving the caller to fetch the range it actually needs.
		c.Put("obj", 0, []byte("0123456789"))

		got := c.Get("obj", 8, 10)
		if got != nil {
			t.Errorf("Get(obj,8,10) over a 10-byte entry returned %d bytes (%q) instead of a miss.\n"+
				"internal/fuse returns a non-nil hit to the kernel verbatim as file content, so a short "+
				"answer here is a silently truncated read: the caller asked for 10 bytes at offset 8 and "+
				"has no way to tell a short cache entry from a short file", len(got), got)
		}
	})

	t.Run("a read past the end of what is held is a miss", func(t *testing.T) {
		c := newCache(t)
		c.Put("obj", 0, []byte("0123456789"))

		if got := c.Get("obj", 100, 10); got != nil {
			t.Errorf("Get(obj,100,10) returned %d bytes (%q), want a miss", len(got), got)
		}
	})

	t.Run("a read longer than what is held is a miss", func(t *testing.T) {
		c := newCache(t)
		c.Put("obj", 0, []byte("0123456789"))

		if got := c.Get("obj", 0, 20); got != nil {
			t.Errorf("Get(obj,0,20) over a 10-byte entry returned %d bytes (%q), want a miss.\n"+
				"This is the same defect as the straddling case and reads more innocently: offset 0 with "+
				"a length the entry cannot cover looks like a whole-object read, and answering it with "+
				"the whole entry hands back a file that is shorter than the one requested", len(got), got)
		}
	})

	t.Run("a miss on an absent key is nil", func(t *testing.T) {
		c := newCache(t)

		if got := c.Get("never-stored", 0, 16); got != nil {
			t.Errorf("Get on an unstored key returned %d bytes (%q), want nil", len(got), got)
		}
	})

	t.Run("an open-ended read returns what is held", func(t *testing.T) {
		c := newCache(t)
		c.Put("obj", 0, []byte("0123456789"))

		// size <= 0 means "whatever contiguous bytes are held from offset" — the one form where a short
		// answer is correct, because the caller has stated it does not know the length. The FUSE
		// metadata cache is the reason it exists: it stores a marshaled ObjectInfo and cannot state
		// that value's length at lookup time.
		if got := c.Get("obj", 0, 0); !bytes.Equal(got, []byte("0123456789")) {
			t.Errorf("Get(obj,0,0) = %q, want the whole 10-byte entry", got)
		}

		if got := c.Get("obj", 6, 0); !bytes.Equal(got, []byte("6789")) {
			t.Errorf("Get(obj,6,0) = %q, want %q", got, "6789")
		}
	})

	t.Run("the returned slice is the caller's own", func(t *testing.T) {
		c := newCache(t)
		c.Put("obj", 0, []byte("0123456789"))

		got := c.Get("obj", 0, 10)
		if got == nil {
			t.Fatal("Get(obj,0,10) missed on bytes just written")
		}

		// Scribble on it. A cache that handed out a view of its own storage would now hold "XXXXXXXXXX",
		// and every later reader of this key would see it — the FUSE read path passes the returned slice
		// to the kernel, which is free to keep it in the page cache.
		for i := range got {
			got[i] = 'X'
		}

		again := c.Get("obj", 0, 10)
		if !bytes.Equal(again, []byte("0123456789")) {
			t.Errorf("after a caller wrote to the slice Get returned, the cache holds %q: the returned "+
				"slice aliases the cache's own storage, so one reader can corrupt every later read of "+
				"the same key", again)
		}
	})

	t.Run("a newer Put wins where it overlaps", func(t *testing.T) {
		c := newCache(t)
		c.Put("obj", 0, []byte("OLDOLDOLD!"))
		c.Put("obj", 0, []byte("NEWNEWNEW!"))

		// This is how an overwrite reaches the cache. Keeping the older copy serves pre-write content,
		// which is audit finding H5 — read-after-write returning stale bytes for up to the TTL.
		if got := c.Get("obj", 0, 10); !bytes.Equal(got, []byte("NEWNEWNEW!")) {
			t.Errorf("Get after an overlapping Put = %q, want %q: the older bytes won, so a read after "+
				"a write returns pre-write content", got, "NEWNEWNEW!")
		}
	})

	t.Run("Delete removes the key", func(t *testing.T) {
		c := newCache(t)
		c.Put("obj", 0, []byte("0123456789"))

		if got := c.Get("obj", 0, 10); got == nil {
			t.Fatal("Get missed on bytes just written, so the Delete below would prove nothing")
		}

		c.Delete("obj")

		if got := c.Get("obj", 0, 10); got != nil {
			t.Errorf("Get after Delete returned %d bytes (%q): write invalidation relies on this, so a "+
				"surviving entry serves pre-write bytes", len(got), got)
		}
	})

	t.Run("Delete removes nothing else", func(t *testing.T) {
		c := newCache(t)

		// The three names that matter: a prefix relationship, and a shared path component. A Delete
		// implemented by prefix match rather than on the delimiter flushes all three, which is the
		// over-removal half of the same contract line (audit finding M14).
		c.Put("logs/app", 0, []byte("aaaaaaaaaa"))
		c.Put("logs/app2", 0, []byte("bbbbbbbbbb"))
		c.Put("logs/appendix", 0, []byte("cccccccccc"))

		c.Delete("logs/app")

		for _, tc := range []struct{ key, want string }{
			{"logs/app2", "bbbbbbbbbb"},
			{"logs/appendix", "cccccccccc"},
		} {
			if got := c.Get(tc.key, 0, 10); !bytes.Equal(got, []byte(tc.want)) {
				t.Errorf("after Delete(logs/app), Get(%s,0,10) = %q, want %q: the delete matched on a "+
					"prefix rather than a whole key, so it discarded an unrelated object's bytes",
					tc.key, got, tc.want)
			}
		}
	})

	t.Run("Size and Stats do not panic on an empty cache", func(t *testing.T) {
		c := newCache(t)

		// Not asserting values: Size means bytes held for the in-process caches and the server's
		// used_memory for Redis, which is not a number this suite can predict. That these are callable
		// on a cache nothing has been written to is still worth pinning — Stats divides by a request
		// total that is zero here.
		_ = c.Size()
		_ = c.Stats()
	})
}
