//go:build cgofuse
// +build cgofuse

package fuse

import (
	"context"
	"os"

	"github.com/objectfs/objectfs/pkg/types"
)

// CgoFuseMountManager manages cgofuse-based mounts
type CgoFuseMountManager struct {
	filesystem *CgoFuseFS
	config     *MountConfig
}

// NewCgoFuseMountManager creates a new cgofuse mount manager
func NewCgoFuseMountManager(backend types.Backend, cache types.Cache, writeBuffer types.WriteBuffer,
	metrics types.MetricsCollector, config *MountConfig) *CgoFuseMountManager {

	uid := safeIntToUint32(os.Getuid())
	gid := safeIntToUint32(os.Getgid())
	if config.Permissions != nil {
		if config.Permissions.UID != 0 {
			uid = config.Permissions.UID
		}
		if config.Permissions.GID != 0 {
			gid = config.Permissions.GID
		}
	}

	fuseConfig := &Config{
		MountPoint:  config.MountPoint,
		ReadOnly:    false,
		DefaultUID:  uid,
		DefaultGID:  gid,
		DefaultMode: 0644,
		CacheTTL:    config.Options.MaxRead, // Reuse for TTL
	}

	filesystem := NewCgoFuseFS(backend, cache, writeBuffer, metrics, fuseConfig)

	return &CgoFuseMountManager{
		filesystem: filesystem,
		config:     config,
	}
}

// Mount mounts the filesystem
func (m *CgoFuseMountManager) Mount(ctx context.Context) error {
	return m.filesystem.Mount(ctx)
}

// Unmount unmounts the filesystem
func (m *CgoFuseMountManager) Unmount() error {
	return m.filesystem.Unmount()
}

// IsMounted returns whether the filesystem is mounted
func (m *CgoFuseMountManager) IsMounted() bool {
	return m.filesystem.IsMounted()
}

// GetStats returns filesystem statistics
func (m *CgoFuseMountManager) GetStats() *FilesystemStats {
	return m.filesystem.GetStats()
}
