package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/scttfrdmn/objectfs/internal/cache/redis"
	"github.com/scttfrdmn/objectfs/internal/config"
)

func TestNewFromConfig_DefaultsToMultiLevel(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefault()
	c, err := NewFromConfig(context.Background(), cfg)
	require.NoError(t, err)
	assert.NotNil(t, c)
}

func TestNewFromConfig_RedisWhenEnabled(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)

	cfg := config.NewDefault()
	cfg.Cluster.Enabled = true
	cfg.Cluster.Redis = config.RedisConfig{
		Enabled:    true,
		Address:    mr.Addr(),
		KeyPrefix:  "iftest",
		TTL:        1 * time.Minute,
		MaxRetries: 1,
	}

	c, err := NewFromConfig(context.Background(), cfg)
	require.NoError(t, err)
	require.NotNil(t, c)

	c.Put("k", 0, []byte("v"))
	got := c.Get("k", 0, 0)
	assert.Equal(t, []byte("v"), got)
}

func TestNewFromConfig_ClusterEnabledButRedisDisabled(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefault()
	cfg.Cluster.Enabled = true
	cfg.Cluster.Redis.Enabled = false

	c, err := NewFromConfig(context.Background(), cfg)
	require.NoError(t, err)
	assert.NotNil(t, c)
}

// TestNewFromConfigReturnsTheImplementationTheConfigNames asserts the *type*, not just non-nil.
//
// The three tests above all passed while this function had no caller, and two of them would still pass
// if the Redis arm were deleted: `assert.NotNil` holds for either implementation, and the round-trip in
// RedisWhenEnabled holds for any working cache. So they establish that NewFromConfig returns something
// usable and say nothing about *which* cache a configuration selects, which is the entire question
// cluster.redis.enabled asks.
//
// The Redis case additionally proves the bytes went to Redis rather than to memory, by reading the key
// out of miniredis directly. A cache that ignored the config and returned a MultiLevelCache would
// satisfy every assertion above and leave the Redis server empty.
func TestNewFromConfigReturnsTheImplementationTheConfigNames(t *testing.T) {
	t.Parallel()

	t.Run("redis when the cluster and redis are both enabled", func(t *testing.T) {
		t.Parallel()

		mr := miniredis.RunT(t)

		cfg := config.NewDefault()
		cfg.Cluster.Enabled = true
		cfg.Cluster.Redis = config.RedisConfig{
			Enabled:    true,
			Address:    mr.Addr(),
			KeyPrefix:  "selection",
			TTL:        time.Minute,
			MaxRetries: 1,
		}

		c, err := NewFromConfig(context.Background(), cfg)
		require.NoError(t, err)

		if _, ok := c.(*redis.Cache); !ok {
			t.Fatalf("cluster.redis.enabled selected %T, so a deployment that configured a shared cache "+
				"got a private in-process one — with no error, and no way to notice until two nodes "+
				"disagreed about a file", c)
		}

		// The bytes must be in Redis, not merely in something that answers Get.
		c.Put("obj", 0, []byte("stored-in-redis"))

		got, err := mr.Get("selection:obj")
		require.NoError(t, err, "the key is absent from Redis, so the cache is not the one it claims")
		assert.Equal(t, "stored-in-redis", got)
	})

	for _, tc := range []struct {
		name    string
		cluster bool
		redis   bool
	}{
		{"neither enabled", false, false},
		{"cluster enabled, redis not", true, false},
		// cluster: false with redis: true is the interesting one. Enabling a distributed cache without
		// enabling the cluster is contradictory, and the resolution is to stay in-process: a Redis cache
		// on a node that is not in a cluster is a remote cache with one participant, which is slower than
		// memory and buys nothing.
		{"redis enabled, cluster not", false, true},
	} {
		t.Run(tc.name+" gives the in-process cache", func(t *testing.T) {
			t.Parallel()

			cfg := config.NewDefault()
			cfg.Cluster.Enabled = tc.cluster
			cfg.Cluster.Redis.Enabled = tc.redis
			// A live address, so a config that wrongly took the Redis arm would fail on connect rather
			// than accidentally passing this assertion.
			cfg.Cluster.Redis.Address = miniredis.RunT(t).Addr()

			c, err := NewFromConfig(context.Background(), cfg)
			require.NoError(t, err)

			if _, ok := c.(*MultiLevelCache); !ok {
				t.Fatalf("got %T, want the in-process MultiLevelCache", c)
			}
		})
	}
}

// TestNewFromConfigFailsWhenRedisIsUnreachable pins the refusal.
//
// A mount that asked for a shared cache and silently got a private one is the defect this function's
// disuse produced, so falling back on a connection failure would reintroduce it one layer up: two nodes
// would both come up, both believe they are sharing a cache, and disagree about file contents with
// nothing in either log to say why.
func TestNewFromConfigFailsWhenRedisIsUnreachable(t *testing.T) {
	t.Parallel()

	cfg := config.NewDefault()
	cfg.Cluster.Enabled = true
	cfg.Cluster.Redis = config.RedisConfig{
		Enabled:    true,
		Address:    "127.0.0.1:1", // nothing listens here
		MaxRetries: 0,
	}

	c, err := NewFromConfig(context.Background(), cfg)
	if err == nil {
		t.Fatalf("an unreachable Redis produced a working cache (%T) instead of an error, so the mount "+
			"comes up with a private cache while the configuration says it is shared", c)
	}

	assert.Nil(t, c, "a cache was returned alongside the error, and a caller checking only one of them "+
		"gets the wrong implementation")
	assert.Contains(t, err.Error(), "cluster.redis", "the error does not name the config block to fix")
}
