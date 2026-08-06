// Package redis provides a Redis-backed distributed cache implementing types.Cache.
package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	goredis "github.com/redis/go-redis/v9"

	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// Cache implements types.Cache backed by a Redis server.
// Full objects are stored under their key; partial reads use GETRANGE.
// Writes with offset != 0 are silently dropped — only full-object puts (offset == 0) are cached.
type Cache struct {
	client    *goredis.Client
	cfg       config.RedisConfig
	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

// NewCache creates a new Redis-backed Cache and verifies connectivity.
//
// ctx bounds the connectivity check only, and is deliberately not retained. The cache outlives the
// call that builds it — it is held for the life of the mount — so storing a startup context here would
// mean every later Get and Put ran under a context canceled the moment startup finished. The
// per-operation contexts are a separate question and are noted at [Cache.Get].
//
// The check is not optional. It is what makes `cluster.redis.enabled` a setting a mount can refuse: an
// address nothing listens on has to fail startup rather than produce a cache whose every operation
// quietly misses, because the alternative is two nodes both coming up believing they share a cache.
func NewCache(ctx context.Context, cfg config.RedisConfig) (*Cache, error) {
	client := goredis.NewClient(&goredis.Options{
		Addr:       cfg.Address,
		Password:   cfg.Password,
		DB:         cfg.DB,
		MaxRetries: cfg.MaxRetries,
	})

	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()

		return nil, fmt.Errorf("redis: ping failed: %w", err)
	}

	return &Cache{client: client, cfg: cfg}, nil
}

// rkey returns the Redis key for an object key, applying the configured prefix.
func (c *Cache) rkey(key string) string {
	if c.cfg.KeyPrefix != "" {
		return c.cfg.KeyPrefix + ":" + key
	}
	return key
}

// Get returns the cached bytes for [offset, offset+size), or nil if this cache does not hold all of
// them.
//
// All of them: a partial hit is a miss, per the types.Cache contract. That is not a preference about
// return values — internal/fuse hands a non-nil hit to the kernel verbatim as file content, so a short
// answer is a truncated read reported as a successful one, and the caller cannot tell a short cache
// entry from a short file.
//
// GETRANGE does not have those semantics. It clamps to the value's length and returns what it can, so
// `GETRANGE k 8 17` over a ten-byte value answers with two bytes and no indication that eight are
// missing — which is what this cache used to return. The length check below is therefore not
// defensive tidiness; it is the whole difference between a cache miss and a silently short file. Found
// by the shared conformance suite in internal/cache/cachetest, which the four in-process
// implementations already passed.
//
// A size of zero or less means "whatever contiguous bytes are held from offset", which is the one form
// where a short answer is correct: the caller has said it does not know the length. GETRANGE's clamp is
// exactly right there, so that path is unchanged.
func (c *Cache) Get(key string, offset, size int64) []byte {
	ctx := context.Background()
	rk := c.rkey(key)

	var (
		data []byte
		err  error
	)

	switch {
	case size <= 0:
		// Open-ended: from offset to the end of whatever is stored. -1 is GETRANGE's "last byte".
		if offset == 0 {
			data, err = c.client.Get(ctx, rk).Bytes()
		} else {
			data, err = c.client.GetRange(ctx, rk, offset, -1).Bytes()
		}
	default:
		data, err = c.client.GetRange(ctx, rk, offset, offset+size-1).Bytes()

		// The clamp. A shorter answer than asked for means the stored value does not cover the request,
		// whether because the object was cached before it grew, because only a prefix was ever stored, or
		// because the request starts past the end. In all three the caller must fetch the range itself.
		if err == nil && int64(len(data)) != size {
			c.misses.Add(1)

			return nil
		}
	}

	if err != nil || len(data) == 0 {
		c.misses.Add(1)

		return nil
	}

	c.hits.Add(1)

	return data
}

// Put stores data for key. Only full-object writes (offset == 0) are accepted;
// partial writes are silently ignored to keep the cache consistent.
func (c *Cache) Put(key string, offset int64, data []byte) {
	if offset != 0 {
		return
	}
	ctx := context.Background()
	_ = c.client.Set(ctx, c.rkey(key), data, c.cfg.TTL).Err()
}

// Delete removes key from the cache.
//
// context.Background() because types.Cache assigns no context to Delete, and the alternatives are worse
// than this one: a context stored on the Cache at construction would be a startup context outliving its
// own scope (see [NewCache]), and there is no per-call one to descend from. go-redis applies its own
// DialTimeout and ReadTimeout defaults, 5s and 3s, so this is bounded rather than unbounded — it is
// simply not cancelable by the caller. A caller that does have a context should use
// [Cache.DeleteContext].
func (c *Cache) Delete(key string) {
	c.DeleteContext(context.Background(), key)
}

// DeleteContext removes key from the cache under ctx.
//
// This exists for the one caller inside this package that holds a context worth respecting: the pub/sub
// invalidation subscriber, whose deletes are round trips on the same connection pool its subscription
// uses. Not part of types.Cache — adding a context to that interface is a change across four
// implementations and every call site in internal/fuse, which is its own decision and not one to make
// by way of a lint finding.
func (c *Cache) DeleteContext(ctx context.Context, key string) {
	_ = c.client.Del(ctx, c.rkey(key)).Err()
}

// Evict asks Redis to free at least size bytes. Redis manages its own eviction
// via the configured maxmemory-policy; we cannot directly target size bytes.
// Returns false to indicate that callers should not rely on this for capacity management.
func (c *Cache) Evict(_ int64) bool {
	c.evictions.Add(1)
	return false
}

// Size returns the Redis server's used_memory in bytes, or 0 on error.
func (c *Cache) Size() int64 {
	ctx := context.Background()
	info, err := c.client.Info(ctx, "memory").Result()
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(info, "\n") {
		if after, ok := strings.CutPrefix(line, "used_memory:"); ok {
			valStr := strings.TrimSpace(after)
			if v, err := strconv.ParseInt(valStr, 10, 64); err == nil {
				return v
			}
		}
	}
	return 0
}

// Stats returns a snapshot of cache statistics.
func (c *Cache) Stats() types.CacheStats {
	hits := c.hits.Load()
	misses := c.misses.Load()
	total := hits + misses
	var hitRate float64
	if total > 0 {
		hitRate = float64(hits) / float64(total)
	}
	size := c.Size()
	return types.CacheStats{
		Hits:      hits,
		Misses:    misses,
		Evictions: c.evictions.Load(),
		Size:      size,
		HitRate:   hitRate,
	}
}

// Close releases the Redis connection.
func (c *Cache) Close() error {
	return c.client.Close()
}

// Client returns the underlying go-redis client, e.g. for use with an Invalidator.
func (c *Cache) Client() *goredis.Client {
	return c.client
}
