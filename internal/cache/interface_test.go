package cache

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/scttfrdmn/objectfs/internal/config"
)

func TestNewFromConfig_DefaultsToMultiLevel(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefault()
	c, err := NewFromConfig(cfg)
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

	c, err := NewFromConfig(cfg)
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

	c, err := NewFromConfig(cfg)
	require.NoError(t, err)
	assert.NotNil(t, c)
}
