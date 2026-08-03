package cache

import (
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/config"
)

// TestMultiLevelConfigFromMapsEveryConfiguredValue is the other half of internal/config's cache block.
//
// The mapping it checks had two defects at once (#178). cache.NewFromConfig, the only reader of
// cluster.redis.*, had no caller, so seven cluster keys plus a seven-key redis: sub-block were decoded,
// defaulted and validated while no mount consulted any of them. And its non-Redis arm passed nil to
// NewMultiLevelCache, discarding the L1/L2 sizing, TTL, persistent-cache directory and eviction policy
// its argument carried — so even called, most of cache: would still have been ignored.
//
// Every value below is spelled out rather than computed from the input, following
// TestBuildS3ConfigMapsEveryConfiguredValue in internal/adapter. A test written as
//
//	want := utils.ParseBytes(cfg.Performance.CacheSize)
//
// agrees with any mapping formula, including one that reads the persistent-cache size into the L1 size.
// Writing "3GB" as 3221225472 means it fails when the mapping is wrong rather than when it differs.
//
// Nothing here is a default: each value differs from what the field would hold if the mapping were
// absent, which is the only way a mapping test can distinguish a mapped field from a field arriving at
// a plausible value from somewhere else.
func TestMultiLevelConfigFromMapsEveryConfiguredValue(t *testing.T) {
	t.Parallel()

	cfg := config.NewDefault()
	cfg.Performance.CacheSize = "3GB"
	cfg.Cache.MaxEntries = 77777
	cfg.Cache.TTL = 11 * time.Minute
	cfg.Cache.EvictionPolicy = "lru"
	cfg.Cache.PersistentCache.Enabled = true
	cfg.Cache.PersistentCache.Directory = "/tmp/objectfs-mapping-test"
	cfg.Cache.PersistentCache.MaxSize = "42GB"

	got := multiLevelConfigFrom(cfg)

	if got.L1Config == nil || got.L2Config == nil {
		t.Fatalf("a nil level config: L1=%v L2=%v", got.L1Config, got.L2Config)
	}

	wantL1 := L1Config{
		Enabled:    true,
		Size:       3221225472, // 3GB
		MaxEntries: 77777,
		TTL:        11 * time.Minute,
		Prefetch:   true,
	}
	if *got.L1Config != wantL1 {
		t.Errorf("L1:\n got: %+v\nwant: %+v", *got.L1Config, wantL1)
	}

	wantL2 := L2Config{
		Enabled:     true,
		Size:        45097156608, // 42GB
		Directory:   "/tmp/objectfs-mapping-test",
		TTL:         11 * time.Minute,
		Compression: true,
	}
	if *got.L2Config != wantL2 {
		t.Errorf("L2:\n got: %+v\nwant: %+v", *got.L2Config, wantL2)
	}

	if got.Policy != "lru" {
		t.Errorf("Policy = %q, want %q: the eviction policy is a documented key and reached nothing",
			got.Policy, "lru")
	}
}

// TestMultiLevelConfigFromFallsBackOnAnUnparseableSize pins the warn-and-continue behavior.
//
// Zero is the failure that matters: a zero-capacity cache evicts on every write and presents as a 0%
// hit rate rather than as a misconfiguration, so an operator sees a performance problem where they have
// a typo. Falling back to the documented default and logging the setting by name is the trade — the
// mount still comes up, and the reason it is not using the configured size is in the log.
func TestMultiLevelConfigFromFallsBackOnAnUnparseableSize(t *testing.T) {
	t.Parallel()

	cfg := config.NewDefault()
	cfg.Performance.CacheSize = "2 gigabytes"
	cfg.Cache.PersistentCache.MaxSize = "tpyo"

	got := multiLevelConfigFrom(cfg)

	if got.L1Config.Size != defaultCacheSize {
		t.Errorf("L1 size = %d, want the %d fallback: an unparseable size must not reach the cache as a "+
			"capacity, and zero is the value that would", got.L1Config.Size, int64(defaultCacheSize))
	}

	if got.L2Config.Size != defaultPersistentCacheSize {
		t.Errorf("L2 size = %d, want the %d fallback", got.L2Config.Size, int64(defaultPersistentCacheSize))
	}
}

// TestMultiLevelConfigFromLeavesTheL2OffWhenTheConfigDoes asserts the one L2 field that is a decision
// rather than a value.
//
// Enabled is not derived from the directory being set, and must not be: a config that names a cache
// directory without enabling persistence is asking for the directory to be remembered, not for a
// 10 GiB on-disk cache to appear.
func TestMultiLevelConfigFromLeavesTheL2OffWhenTheConfigDoes(t *testing.T) {
	t.Parallel()

	cfg := config.NewDefault()
	cfg.Cache.PersistentCache.Enabled = false
	cfg.Cache.PersistentCache.Directory = "/var/cache/objectfs"

	if got := multiLevelConfigFrom(cfg); got.L2Config.Enabled {
		t.Error("the L2 level is enabled while cache.persistent_cache.enabled is false, so a mount " +
			"creates an on-disk cache the configuration declines")
	}
}
