//go:build linux || darwin

package fuse

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scttfrdmn/objectfs/pkg/status"
	"github.com/scttfrdmn/objectfs/pkg/types"
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
	mu            sync.Mutex
	filesystem    *FileSystem
	server        *fuse.Server
	config        *MountConfig
	mounted       bool
	statusTracker *status.Tracker
	currentOpID   string
}

// MountConfig contains mount-specific configuration
type MountConfig struct {
	MountPoint  string        `yaml:"mount_point"`
	Options     *MountOptions `yaml:"options"`
	Permissions *Permissions  `yaml:"permissions"`

	// ReadAhead configures the sequential-read prefetcher. Nil takes
	// [DefaultReadAheadConfig], which is what every caller got unconditionally before
	// performance.read_ahead was wired (#176).
	ReadAhead *ReadAheadConfig

	// Coordinator is the cluster's cache coordination, or nil for a single-node mount. See
	// [FileSystem.coordinator], which it is copied to.
	//
	// A field rather than a `WithCoordinator(...)` functional option, which is what #139 specified.
	// This package has no functional-option pattern — there is not one `func With…` constructor
	// argument in it — so introducing one for a single nilable field would mean two ways to configure a
	// mount, and the next field would have to choose between them. It has no yaml tag because a
	// coordinator is not decodable from a config file; `cluster.enabled` is the operator-facing switch,
	// and internal/adapter is what turns it into this.
	Coordinator types.DistributedCoordinator
}

// MountOptions contains FUSE mount options.
//
// Every field here reaches [MountManager.buildFUSEOptions] and takes effect. Nine did not, and were
// removed rather than plumbed: DirectIO, KeepCache, BigWrites, MaxRead, AsyncRead, WritebackCache,
// SpliceRead, SpliceWrite, and SpliceMove. Each looked like a knob — a yaml tag, a name matching a
// real FUSE capability — and each was a field a caller could set to have nothing happen.
//
// Two of the nine were also not implementable as written. `max_read` is not a field on go-fuse's
// [fuse.MountOptions] at all: go-fuse passes it as a string mount option and sets it equal to
// MaxWrite, so the read size is not separately settable. BigWrites named a FUSE capability that has
// been unconditional since kernel 4.20.
//
// The yaml tags on this type are still bound to nothing, and that is now deliberate rather than an
// oversight: the operator-facing names live on [config.FUSEConfig], which is the type a config file
// decodes into, and this type is reached from it through internal/adapter. Two names for one setting
// is the drift #180 and #176 were both about, so the tags below are kept only because they predate
// that discovery and nothing decodes them.
//
// Three of the four capabilities #180 nominated for plumbing are the DirectIO, KeepCache, and
// AsyncRead fields below. The fourth, splice, is not plumbable — see [Config.DirectIO] for why, and
// [config.FUSEConfig] for the same note in operator-facing terms.
type MountOptions struct {
	// Basic options
	ReadOnly     bool `yaml:"read_only"`
	AllowOther   bool `yaml:"allow_other"`
	AllowRoot    bool `yaml:"allow_root"`
	DefaultPerms bool `yaml:"default_permissions"`

	// MaxWrite is the largest WRITE the kernel may send, and go-fuse derives max_read and MaxPages
	// from it. It is the one size that is settable.
	MaxWrite uint32 `yaml:"max_write"`

	// DirectIO and KeepCache are the two open-time flags, carried through to [Config] by
	// [CreatePlatformMountManager] because [FileNode.Open] is what returns them to the kernel — they
	// are not mount-time options and are not on go-fuse's [fuse.MountOptions] at all. See
	// [Config.DirectIO] for the precedence between them.
	DirectIO  bool `yaml:"direct_io"`
	KeepCache bool `yaml:"keep_cache"`

	// SyncRead makes the kernel keep at most one READ outstanding against one file.
	//
	// Named for go-fuse's field rather than for the AsyncRead the removed struct had, because
	// AsyncRead is the negation and SyncRead is the thing that exists: go-fuse turns SyncRead into a
	// disabled CAP_ASYNC_READ at INIT (fuse/server.go:187) and there is no field going the other way.
	// Keeping the negated name would also have made false the interesting value, so a zero-valued
	// MountOptions would have serialized every read on the mount. Off is asynchronous reads, which is
	// both the kernel's default and this one's.
	//
	// Turning it on costs read throughput on any sequential reader: kernel readahead is precisely the
	// mechanism CAP_ASYNC_READ enables. It is here for a backend that cannot serve concurrent reads
	// of one file. S3 can.
	SyncRead bool `yaml:"sync_read"`

	// Advanced options
	Debug        bool          `yaml:"debug"`
	FSName       string        `yaml:"fsname"`
	Subtype      string        `yaml:"subtype"`
	AttrTimeout  time.Duration `yaml:"attr_timeout"`
	EntryTimeout time.Duration `yaml:"entry_timeout"`
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
		filesystem:    filesystem,
		config:        config,
		statusTracker: status.NewTracker(status.DefaultTrackerConfig()),
	}
}

// NewMountManagerWithTracker creates a mount manager with a custom status tracker
func NewMountManagerWithTracker(filesystem *FileSystem, config *MountConfig, tracker *status.Tracker) *MountManager {
	mm := NewMountManager(filesystem, config)
	if tracker != nil {
		mm.statusTracker = tracker
	}
	return mm
}

// Mount mounts the filesystem at the specified mount point
func (m *MountManager) Mount(ctx context.Context) error {
	m.mu.Lock()
	if m.mounted {
		m.mu.Unlock()
		return fmt.Errorf("filesystem is already mounted")
	}
	m.mu.Unlock()

	// Start tracking the mount operation
	metadata := map[string]any{
		"mount_point": m.config.MountPoint,
		"fs_name":     m.config.Options.FSName,
		"read_only":   m.config.Options.ReadOnly,
	}
	op, opCtx := m.statusTracker.StartOperation(ctx, "mount", metadata)
	m.mu.Lock()
	m.currentOpID = op.ID
	m.mu.Unlock()

	// Phase 1: Validate mount point
	if err := m.statusTracker.SetPhase(op.ID, "validating"); err != nil {
		slog.Warn("failed to set phase", "error", err)
	}
	if err := m.statusTracker.SetMessage(op.ID, "Validating mount point..."); err != nil {
		slog.Warn("failed to set message", "error", err)
	}

	if err := m.validateMountPoint(); err != nil {
		if trackErr := m.statusTracker.FailOperation(op.ID, fmt.Errorf("invalid mount point: %w", err)); trackErr != nil {
			slog.Warn("failed to track operation failure", "error", trackErr)
		}
		return fmt.Errorf("invalid mount point: %w", err)
	}

	// Phase 2: Build FUSE options
	if err := m.statusTracker.SetPhase(op.ID, "configuring"); err != nil {
		slog.Warn("failed to set phase", "error", err)
	}
	if err := m.statusTracker.SetMessage(op.ID, "Building FUSE options..."); err != nil {
		slog.Warn("failed to set message", "error", err)
	}

	opts := m.buildFUSEOptions()

	// Phase 3: Create the FUSE server
	if err := m.statusTracker.SetPhase(op.ID, "mounting"); err != nil {
		slog.Warn("failed to set phase", "error", err)
	}
	if err := m.statusTracker.SetMessage(op.ID, fmt.Sprintf("Mounting filesystem at %s...", m.config.MountPoint)); err != nil {
		slog.Warn("failed to set message", "error", err)
	}

	server, err := fs.Mount(m.config.MountPoint, m.filesystem.Root(), opts)
	if err != nil {
		if trackErr := m.statusTracker.FailOperation(op.ID, fmt.Errorf("failed to mount filesystem: %w", err)); trackErr != nil {
			slog.Warn("failed to track operation failure", "error", trackErr)
		}
		return fmt.Errorf("failed to mount filesystem: %w", err)
	}

	m.mu.Lock()
	m.server = server
	m.mounted = true
	m.mu.Unlock()

	// Phase 4: Complete
	if err := m.statusTracker.SetPhase(op.ID, "complete"); err != nil {
		slog.Warn("failed to set phase", "error", err)
	}
	if err := m.statusTracker.SetMessage(op.ID, "Filesystem mounted successfully"); err != nil {
		slog.Warn("failed to set message", "error", err)
	}

	slog.Info("ObjectFS mounted", "mount_point", m.config.MountPoint)

	// Complete the operation
	if err := m.statusTracker.CompleteOperation(op.ID); err != nil {
		slog.Warn("failed to complete operation tracking", "error", err)
	}
	m.mu.Lock()
	m.currentOpID = ""
	m.mu.Unlock()

	// Start serving in background.
	//
	// Wait on the local server, not on m.server: Unmount sets m.server = nil under
	// m.mu, so reading the field here both races that write and can dereference nil
	// if Unmount wins — which would panic on the goroutine and take the process down
	// with the mount. The value is the same server either way, and this goroutine's
	// whole job is to outlive the field.
	go func() {
		slog.Info("starting FUSE server")
		server.Wait()
		slog.Info("FUSE server stopped")
		m.mu.Lock()
		m.mounted = false
		m.mu.Unlock()
	}()

	// Use operation context to ensure proper cancellation
	_ = opCtx

	return nil
}

// Unmount unmounts the filesystem
func (m *MountManager) Unmount() error {
	m.mu.Lock()
	mounted := m.mounted
	server := m.server
	m.mu.Unlock()

	if !mounted {
		return fmt.Errorf("filesystem is not mounted")
	}

	if server == nil {
		return fmt.Errorf("no active server to unmount")
	}

	// Start tracking the unmount operation
	metadata := map[string]any{
		"mount_point": m.config.MountPoint,
	}
	op, _ := m.statusTracker.StartOperation(context.Background(), "unmount", metadata)

	// Phase 1: Prepare for unmount
	if err := m.statusTracker.SetPhase(op.ID, "preparing"); err != nil {
		slog.Warn("failed to set phase", "error", err)
	}
	if err := m.statusTracker.SetMessage(op.ID, "Preparing to unmount filesystem..."); err != nil {
		slog.Warn("failed to set message", "error", err)
	}

	slog.Info("unmounting filesystem", "mount_point", m.config.MountPoint)

	// Phase 2: Unmount the filesystem
	if err := m.statusTracker.SetPhase(op.ID, "unmounting"); err != nil {
		slog.Warn("failed to set phase", "error", err)
	}
	if err := m.statusTracker.SetMessage(op.ID, fmt.Sprintf("Unmounting filesystem at %s...", m.config.MountPoint)); err != nil {
		slog.Warn("failed to set message", "error", err)
	}

	err := server.Unmount()
	if err != nil {
		// Try force unmount
		if err := m.statusTracker.SetPhase(op.ID, "force-unmounting"); err != nil {
			slog.Warn("failed to set phase", "error", err)
		}
		if err := m.statusTracker.SetMessage(op.ID, "Normal unmount failed, trying force unmount..."); err != nil {
			slog.Warn("failed to set message", "error", err)
		}

		slog.Warn("normal unmount failed, trying force unmount", "error", err)
		if forceErr := m.forceUnmount(); forceErr != nil {
			if trackErr := m.statusTracker.FailOperation(op.ID, fmt.Errorf("unmount failed: %w (force unmount also failed: %v)", err, forceErr)); trackErr != nil {
				slog.Warn("failed to track operation failure", "error", trackErr)
			}
			return fmt.Errorf("unmount failed: %w (force unmount also failed: %v)", err, forceErr)
		}
	}

	m.mu.Lock()
	m.mounted = false
	m.server = nil
	m.mu.Unlock()

	// Complete the operation
	if err := m.statusTracker.SetMessage(op.ID, "Filesystem unmounted successfully"); err != nil {
		slog.Warn("failed to set message", "error", err)
	}

	slog.Info("filesystem unmounted successfully")

	if err := m.statusTracker.CompleteOperation(op.ID); err != nil {
		slog.Warn("failed to complete operation tracking", "error", err)
	}

	return nil
}

// IsMounted checks if the filesystem is currently mounted.
func (m *MountManager) IsMounted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounted
}

// GetMountPoint returns the current mount point
func (m *MountManager) GetMountPoint() string {
	return m.config.MountPoint
}

// Wait blocks until the FUSE server stops serving, and returns immediately if the
// filesystem is not mounted.
//
// The server is read under m.mu and then waited on outside it. Holding the lock
// across Wait would deadlock Unmount, which needs the same lock to clear the field;
// checking m.server != nil and then dereferencing it unlocked, which this used to
// do, is both a race with that write and a nil dereference if it lands between the
// two.
func (m *MountManager) Wait() {
	m.mu.Lock()
	server := m.server
	m.mu.Unlock()

	if server != nil {
		server.Wait()
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

// GetStatusTracker returns the status tracker for monitoring operations
func (m *MountManager) GetStatusTracker() *status.Tracker {
	return m.statusTracker
}

// GetCurrentOperation returns the current operation being tracked (if any)
func (m *MountManager) GetCurrentOperation() (*status.Operation, error) {
	m.mu.Lock()
	opID := m.currentOpID
	m.mu.Unlock()
	if opID == "" {
		return nil, nil
	}
	return m.statusTracker.GetOperation(opID)
}

// GetOperationHistory returns the operation history
func (m *MountManager) GetOperationHistory(limit int) []*status.Operation {
	return m.statusTracker.GetHistory(limit)
}

// SubscribeToOperation subscribes to updates for a specific operation
func (m *MountManager) SubscribeToOperation(opID string) (<-chan status.OperationUpdate, error) {
	return m.statusTracker.Subscribe(opID)
}

// Remount remounts the filesystem with new options
func (m *MountManager) Remount(newConfig *MountConfig) error {
	m.mu.Lock()
	wasMounted := m.mounted
	m.mu.Unlock()

	if wasMounted {
		if err := m.Unmount(); err != nil {
			return fmt.Errorf("failed to unmount for remount: %w", err)
		}
	}

	// Update configuration
	if newConfig != nil {
		m.config = newConfig
	}

	// Only remount if it was previously mounted
	if wasMounted {
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
		slog.Warn("mount point is not empty", "mount_point", m.config.MountPoint)
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
			Name:        m.config.Options.FSName,
			FsName:      m.config.Options.FSName,
			DirectMount: true,
			Debug:       m.config.Options.Debug,
			AllowOther:  m.config.Options.AllowOther,
			MaxWrite:    int(m.config.Options.MaxWrite),

			// go-fuse's own field, and the reason MountOptions.SyncRead is not named AsyncRead. This
			// is the whole of that setting's plumbing: go-fuse ORs CAP_ASYNC_READ into
			// DisabledCapabilities at INIT when it is set (fuse/server.go:187), so the capability is
			// never negotiated with the kernel.
			SyncRead: m.config.Options.SyncRead,
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
	actuallyMounted := w.manager.isAlreadyMounted()

	if expectedMounted != actuallyMounted {
		if expectedMounted {
			slog.Warn("filesystem should be mounted but appears unmounted")
			// Could trigger remount here
		} else {
			slog.Warn("filesystem should be unmounted but appears mounted")
		}
	}
}
