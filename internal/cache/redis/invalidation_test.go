package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/scttfrdmn/objectfs/internal/cache/redis"
	"github.com/scttfrdmn/objectfs/internal/config"
)

func newTestCacheAndInvalidator(t *testing.T, mr *miniredis.Miniredis, nodeID string) (*redis.Cache, *redis.Invalidator) {
	t.Helper()
	cfg := config.RedisConfig{
		Enabled:    true,
		Address:    mr.Addr(),
		KeyPrefix:  "test",
		TTL:        5 * time.Minute,
		MaxRetries: 1,
	}
	c, err := redis.NewCache(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	inv := redis.NewInvalidator(c.Client(), nodeID, c)
	return c, inv
}

func TestInvalidator_Publish(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	_, inv := newTestCacheAndInvalidator(t, mr, "node1")

	ctx := context.Background()
	err := inv.Publish(ctx, "some/key")
	assert.NoError(t, err)
}

func TestInvalidator_Subscribe_RemoteInvalidation(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)

	// node1 holds the cached key
	c1, inv1 := newTestCacheAndInvalidator(t, mr, "node1")
	c1.Put("shared/key", 0, []byte("value"))
	require.NotNil(t, c1.Get("shared/key", 0, 0))

	ctx := t.Context()

	// node1 subscribes to invalidation messages
	inv1.Subscribe(ctx)

	// node2 publishes an invalidation for "shared/key"
	node2Client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = node2Client.Close() }()
	inv2 := redis.NewInvalidator(node2Client, "node2", nil)

	// Give subscriber goroutine time to connect
	time.Sleep(50 * time.Millisecond)

	err := inv2.Publish(ctx, "shared/key")
	require.NoError(t, err)

	// Allow message to propagate
	assert.Eventually(t, func() bool {
		return c1.Get("shared/key", 0, 0) == nil
	}, 500*time.Millisecond, 20*time.Millisecond, "node1 cache should be invalidated by node2")
}

func TestInvalidator_Subscribe_IgnoresSelf(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)

	c1, inv1 := newTestCacheAndInvalidator(t, mr, "node1")
	c1.Put("mykey", 0, []byte("data"))

	ctx := t.Context()
	inv1.Subscribe(ctx)

	time.Sleep(50 * time.Millisecond)

	// Publish from node1 itself — should NOT invalidate node1's own cache
	err := inv1.Publish(ctx, "mykey")
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	assert.NotNil(t, c1.Get("mykey", 0, 0), "node1 should NOT evict its own published invalidation")
}
