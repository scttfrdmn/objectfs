package fuse

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

)

// FilesystemStats represents filesystem operation statistics
type FilesystemStats struct {
	Lookups      int64 `json:"lookups"`
	Opens        int64 `json:"opens"`
	Reads        int64 `json:"reads"`
	Writes       int64 `json:"writes"`
	BytesRead    int64 `json:"bytes_read"`
	BytesWritten int64 `json:"bytes_written"`
	CacheHits    int64 `json:"cache_hits"`
	CacheMisses  int64 `json:"cache_misses"`
	Errors       int64 `json:"errors"`
}

// MountManager manages FUSE mount operations
type MountManager struct {
	filesystem *FileSystem
	server     *fuse.Server
	config     *MountConfig
	mounted    bool
}

// MountConfig contains mount-specific configuration
type MountConfig struct {
	MountPoint  string        `yaml:"mount_point"`
	Options     *MountOptions `yaml:"options"`
	Permissions *Permissions  `yaml:"permissions"`
}

// MountOptions contains FUSE mount options
type MountOptions struct {
	// Basic options
	ReadOnly     bool   `yaml:"read_only"`
	AllowOther   bool   `yaml:"allow_other"`
	AllowRoot    bool   `yaml:"allow_root"`
	DefaultPerms bool   `yaml:"default_permissions"`
	
	// Performance options
	DirectIO     bool   `yaml:"direct_io"`
	KeepCache    bool   `yaml:"keep_cache"`
	BigWrites    bool   `yaml:"big_writes"`
	MaxRead      uint32 `yaml:"max_read"`
	MaxWrite     uint32 `yaml:"max_write"`
	
	// Advanced options
	Debug        bool          `yaml:"debug"`
	FSName       string        `yaml:"fsname"`
	Subtype      string        `yaml:"subtype"`
	AttrTimeout  time.Duration `yaml:"attr_timeout"`
	EntryTimeout time.Duration `yaml:"entry_timeout"`
	
	// Kernel options
	AsyncRead    bool `yaml:"async_read"`
	WritebackCache bool `yaml:"writeback_cache"`
	SpliceRead   bool `yaml:"splice_read"`
	SpliceWrite  bool `yaml:"splice_write"`
	SpliceMove   bool `yaml:"splice_move"`
}

// Permissions contains permission settings
type Permissions struct {
	UID      uint32 `yaml:"uid"`
	GID      uint32 `yaml:"gid"`
	FileMode uint32 `yaml:"file_mode"`
	DirMode  uint32 `yaml:"dir_mode"`
}

// NewMountManager creates a new mount manager
func NewMountManager(filesystem *FileSystem, config *MountConfig) *MountManager {
	if config == nil {
		config = &MountConfig{
			Options: &MountOptions{
				MaxRead:      128 * 1024,
				MaxWrite:     128 * 1024,
				AttrTimeout:  time.Second,
				EntryTimeout: time.Second,
				FSName:       "objectfs",
				Subtype:      "s3",
			},
			Permissions: &Permissions{
				UID:      safeIntToUint32(os.Getuid()),
				GID:      safeIntToUint32(os.Getgid()),
				FileMode: 0644,
				DirMode:  0755,
			},
		}
	}

	return &MountManager{
		filesystem: filesystem,
		config:     config,
	}
}

// Mount mounts the filesystem at the specified mount point
func (m *MountManager) Mount(ctx context.Context) error {
	if m.mounted {
		return fmt.Errorf("filesystem is already mounted")
	}

	// Validate mount point
	if err := m.validateMountPoint(); err != nil {
		return fmt.Errorf("invalid mount point: %w", err)
	}

	// Build FUSE options
	opts := m.buildFUSEOptions()

	// Create the FUSE server
	server, err := fs.Mount(m.config.MountPoint, m.filesystem.Root(), opts)
	if err != nil {
		return fmt.Errorf("failed to mount filesystem: %w", err)
	}

	m.server = server
	m.mounted = true

	log.Printf("ObjectFS mounted at %s", m.config.MountPoint)

	// Start serving in background
	go func() {
		log.Printf("Starting FUSE server...")
		m.server.Wait()
		log.Printf("FUSE server stopped")
		m.mounted = false
	}()

	return nil
}

// Unmount unmounts the filesystem
func (m *MountManager) Unmount() error {
	if !m.mounted {
		return fmt.Errorf("filesystem is not mounted")
	}

	if m.server == nil {
		return fmt.Errorf("no active server to unmount")
	}

	log.Printf("Unmounting filesystem at %s", m.config.MountPoint)

	// Unmount the filesystem
	err := m.server.Unmount()
	if err != nil {
		// Try force unmount
		log.Printf("Normal unmount failed, trying force unmount: %v", err)
		if forceErr := m.forceUnmount(); forceErr != nil {
			return fmt.Errorf("unmount failed: %w (force unmount also failed: %v)", err, forceErr)
		}
	}

	m.mounted = false
	m.server = nil

	log.Printf("Filesystem unmounted successfully")
	return nil
}

// IsMount() checks if the filesystem is currently mounted
func (m *MountManager) IsMounted() bool {
	return m.mounted
}

// GetMountPoint returns the current mount point
func (m *MountManager) GetMountPoint() string {
	return m.config.MountPoint
}

// Wait waits for the mount to complete
func (m *MountManager) Wait() {
	if m.server != nil {
		m.server.Wait()
	}
}

// GetStats returns filesystem statistics
func (m *MountManager) GetStats() *FilesystemStats {
	if m.filesystem != nil {
		stats := m.filesystem.GetStats()
		return &FilesystemStats{
			Lookups:      stats.Lookups,
			Opens:        stats.Opens,
			Reads:        stats.Reads,
			Writes:       stats.Writes,
			BytesRead:    stats.BytesRead,
			BytesWritten: stats.BytesWritten,
			CacheHits:    stats.CacheHits,
			CacheMisses:  stats.CacheMisses,
			Errors:       stats.Errors,
		}
	}
	return &FilesystemStats{}
}

// Remount remounts the filesystem with new options
func (m *MountManager) Remount(newConfig *MountConfig) error {
	wasUnmounted := !m.mounted
	
	if m.mounted {
		if err := m.Unmount(); err != nil {
			return fmt.Errorf("failed to unmount for remount: %w", err)
		}
	}

	// Update configuration
	if newConfig != nil {
		m.config = newConfig
	}

	// Only remount if it was previously mounted
	if !wasUnmounted {
		return m.Mount(context.Background())
	}

	return nil
}

// Helper methods

func (m *MountManager) validateMountPoint() error {
	if m.config.MountPoint == "" {
		return fmt.Errorf("mount point cannot be empty")
	}

	// Check if mount point exists
	info, err := os.Stat(m.config.MountPoint)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("mount point does not exist: %s", m.config.MountPoint)
		}
		return fmt.Errorf("cannot access mount point: %w", err)
	}

	// Check if it's a directory
	if !info.IsDir() {
		return fmt.Errorf("mount point is not a directory: %s", m.config.MountPoint)
	}

	// Check if directory is empty (optional check)
	entries, err := os.ReadDir(m.config.MountPoint)
	if err != nil {
		return fmt.Errorf("cannot read mount point directory: %w", err)
	}

	if len(entries) > 0 {
		log.Printf("Warning: mount point %s is not empty", m.config.MountPoint)
	}

	// Check if already mounted
	if m.isAlreadyMounted() {
		return fmt.Errorf("mount point %s is already mounted", m.config.MountPoint)
	}

	return nil
}

func (m *MountManager) buildFUSEOptions() *fs.Options {
	opts := &fs.Options{
		// Server options
		MountOptions: fuse.MountOptions{
			Name:         m.config.Options.FSName,
			FsName:       m.config.Options.FSName,
			DirectMount:  true,
			Debug:        m.config.Options.Debug,
			AllowOther:   m.config.Options.AllowOther,
			MaxWrite:     int(m.config.Options.MaxWrite),
		},
		
		// Attribute caching
		AttrTimeout:  &m.config.Options.AttrTimeout,
		EntryTimeout: &m.config.Options.EntryTimeout,
		
		// I/O options
		NullPermissions: !m.config.Options.DefaultPerms,
	}

	// Add read-only flag if specified
	if m.config.Options.ReadOnly {
		opts.Options = append(opts.Options, "ro")
	}

	// Add allow_root if specified
	if m.config.Options.AllowRoot {
		opts.Options = append(opts.Options, "allow_root")
	}

	// Add custom options
	if m.config.Options.FSName != "" {
		opts.Options = append(opts.Options, 
			fmt.Sprintf("fsname=%s", m.config.Options.FSName))
	}

	if m.config.Options.Subtype != "" {
		opts.Options = append(opts.Options,
			fmt.Sprintf("subtype=%s", m.config.Options.Subtype))
	}

	return opts
}

func (m *MountManager) isAlreadyMounted() bool {
	// Check /proc/mounts to see if the mount point is already mounted
	mountsFile := "/proc/mounts"
	
	data, err := os.ReadFile(mountsFile)
	if err != nil {
		// If we can't read /proc/mounts, assume not mounted
		return false
	}

	// Simple check - look for our mount point in the mounts file
	mountPoint := filepath.Clean(m.config.MountPoint)
	return containsString(string(data), mountPoint)
}

func (m *MountManager) forceUnmount() error {
	// Try lazy unmount first
	err := syscall.Unmount(m.config.MountPoint, 2)
	if err == nil {
		return nil
	}

	// Try force unmount
	return syscall.Unmount(m.config.MountPoint, 1)
}

// Utility functions

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || 
		(len(s) > len(substr) && indexOf(s, substr) >= 0))
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// MountWatcher watches for mount/unmount events
type MountWatcher struct {
	manager  *MountManager
	interval time.Duration
	stopCh   chan struct{}
	stopped  chan struct{}
}

// NewMountWatcher creates a new mount watcher
func NewMountWatcher(manager *MountManager, interval time.Duration) *MountWatcher {
	if interval == 0 {
		interval = 30 * time.Second
	}

	return &MountWatcher{
		manager:  manager,
		interval: interval,
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// Start starts the mount watcher
func (w *MountWatcher) Start() {
	go w.run()
}

// Stop stops the mount watcher
func (w *MountWatcher) Stop() {
	close(w.stopCh)
	<-w.stopped
}

func (w *MountWatcher) run() {
	defer close(w.stopped)
	
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.checkMount()
		}
	}
}

func (w *MountWatcher) checkMount() {
	expectedMounted := w.manager.IsMounted()
	actuallyMounted := !w.manager.isAlreadyMounted()

	if expectedMounted != actuallyMounted {
		if expectedMounted {
			log.Printf("Warning: filesystem should be mounted but appears unmounted")
			// Could trigger remount here
		} else {
			log.Printf("Warning: filesystem should be unmounted but appears mounted")
		}
	}
}