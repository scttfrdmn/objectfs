//go:build linux || darwin

package fuse

// This file covers the seam #139 opened: a coordinator set by internal/adapter has to arrive at the
// FileSystem that serves reads, through MountConfig and CreatePlatformMountManager.
//
// It is a plumbing assertion, and the reason for saying so out loud is that kernel_options_test.go in
// this same package rejects copy checks on principle — nine fields reached a release because
// `opts.X == cfg.X` passes whether anything downstream reads X. The distinction here is that at this
// commit *nothing downstream reads the coordinator yet*: the calls that will are #141's, on the cache
// and write paths. So there is no observable behavior to assert on, and the honest thing is a test that
// pins the chain and a fake that #141's tests can assert against once the calls exist.
//
// What is asserted rather than copied, and does matter today: that a mount configured with no
// coordinator has a nil one. Nil is the whole safety argument for #139 — every path added from here is
// guarded by it — and a non-nil-interface-wrapping-nothing would defeat every one of those guards while
// looking correct from the adapter's side.

import (
	"context"
	"sync"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/internal/vfs"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// recordingCoordinator is a types.DistributedCoordinator that records what it was asked to do.
//
// It exists for #141 and #142 as much as for this file: the announce and invalidate calls are
// fire-and-forget with errors logged at Debug, so the only way to assert that a write announced
// anything is to record the calls. Everything is guarded by a mutex because those calls happen on
// whichever goroutine the kernel dispatched the write on.
//
// The accessors below came back with #141, which is the test that reads them. They were written here
// during #139, removed because the linter correctly reported them unused, and a //nolint to keep a helper
// alive for a caller that does not exist is how a test fixture starts drifting from what the code does.
type recordingCoordinator struct {
	mu          sync.Mutex
	announced   []types.KeyAnnouncement
	invalidated []invalidation

	// announceErr and invalidateErr make the calls fail. Both call sites are fire-and-forget with the
	// error logged and nothing surfaced, so a test asserting the syscall still succeeds needs a way to
	// make them fail — otherwise "the error is not surfaced" is asserted against a path where no error
	// ever occurs.
	announceErr   error
	invalidateErr error

	queried      []string
	queryResults []types.KeyAnnouncement
	queryErr     error
}

// invalidation is one InvalidateKey call, key and etag together.
//
// Both, because the etag is the half that is easy to get wrong invisibly: a receiver's replay ledger is
// keyed on it, so an invalidation carrying the wrong version suppresses one that was never applied. A
// recorder that kept only the key would agree with an implementation that passed "" everywhere.
type invalidation struct {
	key  string
	etag string
}

// announcements returns what was announced.
func (r *recordingCoordinator) announcements() []types.KeyAnnouncement {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]types.KeyAnnouncement(nil), r.announced...)
}

// invalidations returns what was invalidated, in call order.
func (r *recordingCoordinator) invalidations() []invalidation {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]invalidation(nil), r.invalidated...)
}

// invalidatedKeys returns just the keys, for the assertions that do not care about versions.
func (r *recordingCoordinator) invalidatedKeys() []string {
	keys := make([]string, 0, len(r.invalidated))
	for _, inv := range r.invalidations() {
		keys = append(keys, inv.key)
	}

	return keys
}

func (r *recordingCoordinator) ExecuteOperation(ctx context.Context, op any) (any, error) {
	return nil, types.ErrNotSupported
}

func (r *recordingCoordinator) GetStats() map[string]any { return map[string]any{} }

func (r *recordingCoordinator) AnnounceKey(ctx context.Context, ann types.KeyAnnouncement) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Recorded even when it fails. The production AnnounceKey validates before sending — it refuses an
	// announcement with no ETag — so a test asserting what was *attempted* needs the call regardless of
	// its outcome.
	r.announced = append(r.announced, ann)

	return r.announceErr
}

func (r *recordingCoordinator) QueryKeyOwnership(ctx context.Context, key string) ([]types.KeyAnnouncement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.queried = append(r.queried, key)

	return r.queryResults, r.queryErr
}

func (r *recordingCoordinator) InvalidateKey(ctx context.Context, key, etag string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.invalidated = append(r.invalidated, invalidation{key: key, etag: etag})

	return r.invalidateErr
}

var _ types.DistributedCoordinator = (*recordingCoordinator)(nil)

// TestCoordinatorReachesTheFilesystem walks the chain internal/adapter uses.
//
// Through CreatePlatformMountManager rather than NewFileSystem directly, because the derivation from
// MountConfig to Config is the step that has historically dropped fields — it hardcoded uid, gid and
// mode for a whole release, and discarded MountConfig.Permissions entirely (#180). Calling
// NewFileSystem with a Config would skip exactly the code most likely to be wrong.
func TestCoordinatorReachesTheFilesystem(t *testing.T) {
	t.Parallel()

	coord := &recordingCoordinator{}
	fs := filesystemFromMountConfig(t, coord)

	if fs.coordinator == nil {
		t.Fatal("the coordinator set on MountConfig did not reach the FileSystem, so a clustered mount " +
			"would coordinate nothing while its configuration said it was clustered")
	}

	if fs.coordinator != types.DistributedCoordinator(coord) {
		t.Error("the FileSystem holds a coordinator other than the one configured")
	}
}

// TestNoCoordinatorLeavesItNil is the assertion the nil-guard safety argument rests on.
//
// Every mount that does not set cluster.enabled takes this path, which is very nearly all of them, and
// each coordinator call added from #141 onward is guarded by a nil check. A non-nil coordinator here —
// an empty struct, or an interface wrapping a nil pointer — would send every one of those guards down
// the coordinated branch on a single-node mount.
func TestNoCoordinatorLeavesItNil(t *testing.T) {
	t.Parallel()

	fs := filesystemFromMountConfig(t, nil)

	if fs.coordinator != nil {
		t.Errorf("coordinator is %T on a mount that configured none; every nil guard in this package "+
			"would take the coordinated branch", fs.coordinator)
	}
}

// filesystemFromMountConfig builds a FileSystem the way internal/adapter does and returns it.
//
// In-package, so it can reach mm.filesystem — the same access kernel_options_test.go uses for the
// open-flag assertions.
func filesystemFromMountConfig(t *testing.T, coord types.DistributedCoordinator) *FileSystem {
	t.Helper()

	srv := testaws.Start(t)

	writer, err := vfs.NewWriter(t.Context(), srv.Backend())
	if err != nil {
		t.Fatalf("vfs.NewWriter: %v", err)
	}

	pfs := CreatePlatformMountManager(t.Context(), srv.Backend(), nil, writer, nil, &MountConfig{
		MountPoint:  t.TempDir(),
		Options:     &MountOptions{FSName: "objectfs", MaxWrite: 128 * 1024},
		Coordinator: coord,
	})

	mm, ok := pfs.(*MountManager)
	if !ok {
		t.Fatalf("CreatePlatformMountManager returned %T, want *MountManager", pfs)
	}

	return mm.filesystem
}
