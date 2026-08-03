package cache

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/scttfrdmn/objectfs/internal/cache/redis"
	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/pkg/types"
	"github.com/scttfrdmn/objectfs/pkg/utils"
)

// Fallback sizes for the two cache capacities, used only when a configured value cannot be parsed.
//
// Same values and same reasoning as the adapter's, which is where this construction used to live: a
// size that fails to parse must not silently become zero, because a zero-capacity cache evicts
// everything on the next write and looks like a cache with a 0% hit rate rather than like a
// misconfiguration.
const (
	defaultCacheSize           = 2 << 30 // 2 GiB, matching NewDefault's "2GB"
	defaultPersistentCacheSize = 10 << 30
)

// NewFromConfig constructs the cache a mount should use from cfg.
//
// Two shapes, chosen by `cluster.enabled && cluster.redis.enabled`: a Redis-backed distributed cache,
// whose connectivity is verified during construction, or the in-process MultiLevelCache built from the
// `cache:` block.
//
// This function existed for a release with **no caller** (#178), which made the whole `cluster:` block
// — seven keys plus a seven-key `redis:` sub-block — unreachable on every path a mount takes. The
// adapter built a MultiLevelConfig by hand and called NewMultiLevelCache directly, so a deployment
// that set `cluster.redis.enabled: true` got an in-process cache with no error and no warning. It
// looked correct until two nodes disagreed about a file.
//
// Its second arm was lossy in its own right: it passed nil to NewMultiLevelCache, discarding the L1/L2
// sizing, TTL, persistent-cache directory and eviction policy the caller's config carries. So even had
// it been called, the non-Redis path would have ignored most of `cache:` too. Both halves are fixed
// here, and the mapping lives in one place rather than being duplicated between this function and the
// adapter — which is the condition that let the two drift apart unnoticed.
//
// ctx bounds the Redis connectivity check and nothing else: neither cache retains it. A cache lives for
// as long as the mount does, so a startup context stored inside one would cancel every later operation
// the moment startup returned.
func NewFromConfig(ctx context.Context, cfg *config.Configuration) (types.Cache, error) {
	if cfg.Cluster.Enabled && cfg.Cluster.Redis.Enabled {
		c, err := redis.NewCache(ctx, cfg.Cluster.Redis)
		if err != nil {
			// Returned rather than logged and swallowed. A mount that asked for a shared cache and
			// silently got a private one is the defect this function's disuse produced; falling back on a
			// connection failure would reintroduce it at a different layer.
			return nil, fmt.Errorf("cluster.redis: %w", err)
		}

		slog.Info("using the Redis-backed distributed cache",
			"address", cfg.Cluster.Redis.Address, "key_prefix", cfg.Cluster.Redis.KeyPrefix,
			"ttl", cfg.Cluster.Redis.TTL)

		return c, nil
	}

	// nolint:contextcheck // ctx deliberately does not reach the in-process cache. What it would reach
	// is the predictive layer's prefetch workers, which outlive this call by design — they run for as
	// long as the mount does, and are stopped by MultiLevelCache.Close on the unmount path rather than
	// by a context. Passing a startup context down to them would cancel every prefetch the moment
	// startup returned, so the goroutines would still be running and would simply never fetch anything.
	// Each prefetch already bounds itself with its own 5-second timeout, which is the right scope for a
	// speculative background read.
	return NewMultiLevelCache(multiLevelConfigFrom(cfg))
}

// multiLevelConfigFrom maps the cache: block onto the in-process cache's own configuration.
//
// Separate from NewFromConfig so the mapping is assertable without constructing a cache — the same
// reasoning as the adapter's buildS3Config and buildWriterOptions, and for the same reason: a mapping
// only reachable through a constructor is a mapping tests do not check field by field, which is how
// six of thirty S3 fields went unmapped for a release.
func multiLevelConfigFrom(cfg *config.Configuration) *MultiLevelConfig {
	return &MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       sizeOrDefault("performance.cache_size", cfg.Performance.CacheSize, defaultCacheSize),
			MaxEntries: cfg.Cache.MaxEntries,
			TTL:        cfg.Cache.TTL,

			// Prefetch is on for every mount and is not a configuration key. `features.prefetching` and
			// `performance.predictive_caching` both exist and neither reaches here; wiring one of them is
			// its own change, since turning the predictive layer off changes what Close has to stop.
			Prefetch: true,
		},
		L2Config: &L2Config{
			Enabled: cfg.Cache.PersistentCache.Enabled,
			Size: sizeOrDefault("cache.persistent_cache.max_size",
				cfg.Cache.PersistentCache.MaxSize, defaultPersistentCacheSize),
			Directory: cfg.Cache.PersistentCache.Directory,
			TTL:       cfg.Cache.TTL,

			// Compression is likewise unconditional and has no key. Note it is not the same setting as
			// storage.s3.compression, which governs the stored object; this one only affects bytes on the
			// local cache disk and is invisible to anything reading the bucket.
			Compression: true,
		},
		Policy: cfg.Cache.EvictionPolicy,
	}
}

// sizeOrDefault parses a configured size, falling back with a warning rather than to zero.
//
// The warning is the point. `cache_size: 2G` and `cache_size: tpyo` both configured a 1 GiB cache with
// no message at all before this logged, so an operator who mistyped a capacity had no way to learn it
// from the process they were running.
func sizeOrDefault(path, value string, fallback int64) int64 {
	if value == "" {
		return fallback
	}

	n, err := utils.ParseBytes(value)
	if err != nil {
		slog.Warn("configured size is not parseable; using the built-in default",
			"setting", path, "value", value, "default", fallback, "error", err)

		return fallback
	}

	return n
}
