package redis_test

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

func newTestCache(t *testing.T) (*redis.Cache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	cfg := config.RedisConfig{
		Enabled:    true,
		Address:    mr.Addr(),
		Password:   "",
		DB:         0,
		KeyPrefix:  "test",
		TTL:        5 * time.Minute,
		MaxRetries: 1,
	}
	c, err := redis.NewCache(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

func TestNewCache_ConnectOK(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	assert.NotNil(t, c)
}

func TestNewCache_BadAddress(t *testing.T) {
	t.Parallel()
	cfg := config.RedisConfig{
		Enabled:    true,
		Address:    "localhost:1", // nothing listening here
		MaxRetries: 0,
	}
	_, err := redis.NewCache(context.Background(), cfg)
	assert.Error(t, err)
}

func TestPutAndGet_FullObject(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	data := []byte("hello, objectfs")
	c.Put("key1", 0, data)
	got := c.Get("key1", 0, 0)
	assert.Equal(t, data, got)
}

func TestGet_Miss(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	got := c.Get("nonexistent", 0, 0)
	assert.Nil(t, got)
}

func TestGet_PartialRead(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	data := []byte("0123456789")
	c.Put("k", 0, data)
	got := c.Get("k", 2, 4)
	assert.Equal(t, []byte("2345"), got)
}

func TestGet_PartialReadToEnd(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	data := []byte("abcdef")
	c.Put("k2", 0, data)
	// size=0 with offset>0 means "from offset to end"
	got := c.Get("k2", 3, 0)
	assert.Equal(t, []byte("def"), got)
}

func TestPut_PartialWriteIgnored(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	c.Put("k3", 5, []byte("ignored"))
	got := c.Get("k3", 0, 0)
	assert.Nil(t, got, "partial write should not be stored")
}

func TestDelete(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	c.Put("k4", 0, []byte("value"))
	c.Delete("k4")
	got := c.Get("k4", 0, 0)
	assert.Nil(t, got)
}

func TestEvict_ReturnsFalse(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	ok := c.Evict(1024)
	assert.False(t, ok)
}

func TestStats_HitMissTracking(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	c.Put("s1", 0, []byte("data"))
	c.Get("s1", 0, 0)      // hit
	c.Get("s1", 0, 0)      // hit
	c.Get("missing", 0, 0) // miss

	stats := c.Stats()
	assert.Equal(t, uint64(2), stats.Hits)
	assert.Equal(t, uint64(1), stats.Misses)
	assert.InDelta(t, 2.0/3.0, stats.HitRate, 0.001)
}

func TestStats_ZeroHitRate_WhenNoAccesses(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	stats := c.Stats()
	assert.Zero(t, stats.HitRate)
}

func TestTTLExpiry(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	cfg := config.RedisConfig{
		Enabled:    true,
		Address:    mr.Addr(),
		TTL:        100 * time.Millisecond,
		MaxRetries: 1,
	}
	c, err := redis.NewCache(context.Background(), cfg)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	c.Put("expiring", 0, []byte("value"))
	got := c.Get("expiring", 0, 0)
	require.NotNil(t, got, "should exist before expiry")

	mr.FastForward(200 * time.Millisecond)
	got = c.Get("expiring", 0, 0)
	assert.Nil(t, got, "should have expired")
}

func TestKeyPrefix(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	cfg1 := config.RedisConfig{Enabled: true, Address: mr.Addr(), KeyPrefix: "ns1", MaxRetries: 1}
	cfg2 := config.RedisConfig{Enabled: true, Address: mr.Addr(), KeyPrefix: "ns2", MaxRetries: 1}

	c1, err := redis.NewCache(context.Background(), cfg1)
	require.NoError(t, err)
	defer func() { _ = c1.Close() }()

	c2, err := redis.NewCache(context.Background(), cfg2)
	require.NoError(t, err)
	defer func() { _ = c2.Close() }()

	c1.Put("key", 0, []byte("ns1-value"))
	c2.Put("key", 0, []byte("ns2-value"))

	assert.Equal(t, []byte("ns1-value"), c1.Get("key", 0, 0))
	assert.Equal(t, []byte("ns2-value"), c2.Get("key", 0, 0))
}
