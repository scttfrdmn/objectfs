//go:build !cgofuse
// +build !cgofuse

package fuse

import (
	"context"
	"github.com/objectfs/objectfs/pkg/types"
)

// Platform-specific filesystem interface
type PlatformFileSystem interface {
	Mount(ctx context.Context) error
	Unmount() error
	IsMounted() bool
	GetStats() *FilesystemStats
}

// CreatePlatformMountManager creates the appropriate mount manager for the platform
func CreatePlatformMountManager(backend types.Backend, cache types.Cache, writeBuffer types.WriteBuffer,
	metrics types.MetricsCollector, config *MountConfig) PlatformFileSystem {
	// Use original hanwen/go-fuse implementation
	fuseConfig := &Config{
		MountPoint:  config.MountPoint,
		ReadOnly:    false,
		DefaultUID:  1000,
		DefaultGID:  1000,
		DefaultMode: 0644,
		CacheTTL:    60 * 1000000000, // 60 seconds in nanoseconds
	}

	filesystem := NewFileSystem(backend, cache, writeBuffer, metrics, fuseConfig)
	return NewMountManager(filesystem, config)
}
