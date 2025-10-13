//go:build cgofuse
// +build cgofuse

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

// CreatePlatformMountManager creates the cgofuse mount manager
func CreatePlatformMountManager(backend types.Backend, cache types.Cache, writeBuffer types.WriteBuffer,
	metrics types.MetricsCollector, config *MountConfig) PlatformFileSystem {

	return NewCgoFuseMountManager(backend, cache, writeBuffer, metrics, config)
}
