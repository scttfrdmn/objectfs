//go:build linux || darwin

package fuse

import (
	"context"
	"os"
	"time"

	"github.com/scttfrdmn/objectfs/internal/vfs"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// Platform-specific filesystem interface
type PlatformFileSystem interface {
	Mount(ctx context.Context) error
	Unmount() error
	IsMounted() bool
	GetStats() *FilesystemStats
}

// defaultAttrTTL is how long the kernel may cache an attribute set when the mount options do not say.
//
// One minute is long enough that ls -l of a large directory does not re-stat every entry, and short
// enough that a bucket modified by another client converges without a remount. Nothing invalidates
// this from outside — S3 sends no notification ObjectFS listens for — so this duration is the whole
// bound on how stale a foreign write can appear.
const defaultAttrTTL = time.Minute

// CreatePlatformMountManager builds the filesystem and its mount manager from a mount configuration.
//
// Deriving the filesystem's [Config] from the caller's [MountConfig] is this function's entire job,
// and through v0.10.0 it did not do it. It hardcoded uid 1000, gid 1000, and mode 0644, discarding
// MountConfig.Permissions entirely — so every file on the mount was reported as owned by whoever
// happened to be user 1000 on that host, and no permission setting in any config file had any effect.
// It also hardcoded ReadOnly to false, which meant read_only: true mounted a writable filesystem: the
// one setting whose failure a user cannot detect until something has already been overwritten.
func CreatePlatformMountManager(backend types.Backend, cache types.Cache, writeBuffer *vfs.Writer,
	metrics types.MetricsCollector, config *MountConfig) PlatformFileSystem {
	fuseConfig := &Config{
		MountPoint: config.MountPoint,

		// Both ownership defaults are the mounting process, not root. A zero uid here is not "unset" by
		// the time it reaches a stat — it is root, reported as the owner of every object that carries no
		// objectfs-uid, which makes ls -l show a mount full of files belonging to someone else and makes
		// cp -p and rsync complain about ownership they cannot set.
		DefaultUID:     safeIntToUint32(os.Getuid()),
		DefaultGID:     safeIntToUint32(os.Getgid()),
		DefaultMode:    uint32(vfs.DefaultFileMode),
		DefaultDirMode: uint32(vfs.DefaultDirMode),

		CacheTTL: defaultAttrTTL,
	}

	// Permissions is nil on the adapter's path — internal/adapter builds a MountConfig carrying only
	// MountPoint and Options — so each field falls back individually rather than the block being taken
	// or discarded as a whole.
	if p := config.Permissions; p != nil {
		if p.UID != 0 {
			fuseConfig.DefaultUID = p.UID
		}
		if p.GID != 0 {
			fuseConfig.DefaultGID = p.GID
		}
		if p.FileMode != 0 {
			fuseConfig.DefaultMode = p.FileMode
		}
		if p.DirMode != 0 {
			fuseConfig.DefaultDirMode = p.DirMode
		}
	}

	// Nil is meaningful and is passed through as nil: NewReadAheadManager substitutes
	// DefaultReadAheadConfig, which is what every mount ran on before this was plumbed.
	fuseConfig.ReadAhead = config.ReadAhead

	if o := config.Options; o != nil {
		fuseConfig.ReadOnly = o.ReadOnly

		// The attribute timeout the nodes report and the one in fs.Options must be the same duration.
		// buildFUSEOptions passes Options.AttrTimeout to go-fuse as the bridge's default; if Getattr
		// reported a different one the kernel would cache for a period no configuration named.
		if o.AttrTimeout > 0 {
			fuseConfig.CacheTTL = o.AttrTimeout
		}
	}

	filesystem := NewFileSystem(backend, cache, writeBuffer, metrics, fuseConfig)

	return NewMountManager(filesystem, config)
}
