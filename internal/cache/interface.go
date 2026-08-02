package cache

import (
	"github.com/scttfrdmn/objectfs/internal/cache/redis"
	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// NewFromConfig constructs the appropriate cache backend from cfg.
//
// When cfg.Cluster.Enabled && cfg.Cluster.Redis.Enabled, a Redis-backed distributed
// cache is returned; connectivity is verified during construction.
//
// Otherwise the standard in-process MultiLevelCache is returned with default settings.
func NewFromConfig(cfg *config.Configuration) (types.Cache, error) {
	if cfg.Cluster.Enabled && cfg.Cluster.Redis.Enabled {
		return redis.NewCache(cfg.Cluster.Redis)
	}
	return NewMultiLevelCache(nil)
}
