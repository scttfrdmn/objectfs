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
func NewCache(cfg config.RedisConfig) (*Cache, error) {
	client := goredis.NewClient(&goredis.Options{
		Addr:       cfg.Address,
		Password:   cfg.Password,
		DB:         cfg.DB,
		MaxRetries: cfg.MaxRetries,
	})
	ctx := context.Background()
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

// Get retrieves up to size bytes of key starting at offset.
// If offset == 0 and size == 0 the full value is returned.
// Returns nil on cache miss.
func (c *Cache) Get(key string, offset, size int64) []byte {
	ctx := context.Background()
	rk := c.rkey(key)

	var (
		data []byte
		err  error
	)
	if offset == 0 && size == 0 {
		data, err = c.client.Get(ctx, rk).Bytes()
	} else {
		end := int64(-1)
		if size > 0 {
			end = offset + size - 1
		}
		data, err = c.client.GetRange(ctx, rk, offset, end).Bytes()
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
func (c *Cache) Delete(key string) {
	ctx := context.Background()
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
