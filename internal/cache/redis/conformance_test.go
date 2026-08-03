package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/scttfrdmn/objectfs/internal/cache/cachetest"
	"github.com/scttfrdmn/objectfs/internal/cache/redis"
	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// TestCacheHonorsTheCacheContract runs the same suite as the four in-process implementations.
//
// It is in this package rather than beside them because internal/cache cannot import
// internal/cache/redis — redis imports internal/config, which the parent's own tests use — so the
// contract lives in internal/cache/cachetest and both sides call it.
//
// This is the test that found the truncation. Before it existed the Redis cache had ten tests of its
// own, all passing, none of which asked for a range longer than what was stored: Get(k,0,0) and
// Get(k,2,4) over a six- or ten-byte value are both satisfiable, so GETRANGE's clamp-to-length
// behavior never showed. A shared suite asks every implementation the same questions rather than the
// ones its author thought of.
func TestCacheHonorsTheCacheContract(t *testing.T) {
	t.Parallel()

	cachetest.RunContract(t, func(t *testing.T) types.Cache {
		// miniredis, not a real server: the contract is about what Get returns for what Put stored, and
		// nothing here depends on Redis internals miniredis abstains from. A KeyPrefix is set because
		// the mount path defaults to one, and prefixing is where the Delete-removes-nothing-else case
		// could plausibly go wrong.
		mr := miniredis.RunT(t)

		c, err := redis.NewCache(context.Background(), config.RedisConfig{
			Enabled:    true,
			Address:    mr.Addr(),
			KeyPrefix:  "conformance",
			TTL:        time.Hour,
			MaxRetries: 1,
		})
		if err != nil {
			t.Fatalf("NewCache: %v", err)
		}

		t.Cleanup(func() { _ = c.Close() })

		return c
	})
}
