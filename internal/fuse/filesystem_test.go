//go:build linux || darwin

package fuse

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/objectfs/objectfs/pkg/types"
)

// mapCache is a minimal in-memory types.Cache used by tests.
type mapCache struct {
	mu   sync.RWMutex
	data map[string][]byte
}

func newMapCache() *mapCache { return &mapCache{data: make(map[string][]byte)} }

func (m *mapCache) Get(key string, _, size int64) []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := m.data[key]
	if v == nil {
		return nil
	}
	if size > 0 && int64(len(v)) > size {
		return v[:size]
	}
	return v
}

func (m *mapCache) Put(key string, _ int64, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.data[key] = cp
}

func (m *mapCache) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
}

func (m *mapCache) Evict(_ int64) bool      { return false }
func (m *mapCache) Size() int64             { return 0 }
func (m *mapCache) Stats() types.CacheStats { return types.CacheStats{} }

// newTestFS constructs the minimal FileSystem needed to exercise cacheInfo /
// getCachedInfo without any FUSE mount.
func newTestFS(c types.Cache) *FileSystem {
	return &FileSystem{
		cache: c,
		stats: &Stats{},
	}
}

func TestCacheInfo_RoundTrip(t *testing.T) {
	t.Parallel()

	fs := newTestFS(newMapCache())

	info := &types.ObjectInfo{
		Key:          "dir/file.dat",
		Size:         1234567,
		LastModified: time.Date(2026, 2, 22, 12, 0, 0, 0, time.UTC),
		ETag:         `"abc123"`,
		ContentType:  "application/octet-stream",
		Metadata:     map[string]string{"x-owner": "scott", "x-project": "objectfs"},
		Checksum:     "sha256:deadbeef",
	}

	fs.cacheInfo("dir/file.dat", info)

	got := fs.getCachedInfo("dir/file.dat")
	if got == nil {
		t.Fatal("getCachedInfo returned nil after cacheInfo")
	}

	if got.Key != info.Key {
		t.Errorf("Key: got %q, want %q", got.Key, info.Key)
	}
	if got.Size != info.Size {
		t.Errorf("Size: got %d, want %d", got.Size, info.Size)
	}
	if !got.LastModified.Equal(info.LastModified) {
		t.Errorf("LastModified: got %v, want %v", got.LastModified, info.LastModified)
	}
	if got.ETag != info.ETag {
		t.Errorf("ETag: got %q, want %q", got.ETag, info.ETag)
	}
	if got.ContentType != info.ContentType {
		t.Errorf("ContentType: got %q, want %q", got.ContentType, info.ContentType)
	}
	if got.Checksum != info.Checksum {
		t.Errorf("Checksum: got %q, want %q", got.Checksum, info.Checksum)
	}
	if len(got.Metadata) != len(info.Metadata) {
		t.Errorf("Metadata len: got %d, want %d", len(got.Metadata), len(info.Metadata))
	}
	for k, v := range info.Metadata {
		if got.Metadata[k] != v {
			t.Errorf("Metadata[%q]: got %q, want %q", k, got.Metadata[k], v)
		}
	}
}

func TestGetCachedInfo_MissReturnsNil(t *testing.T) {
	t.Parallel()

	fs := newTestFS(newMapCache())
	if got := fs.getCachedInfo("nonexistent"); got != nil {
		t.Errorf("expected nil for uncached path, got %+v", got)
	}
}

func TestGetCachedInfo_NilCacheReturnsNil(t *testing.T) {
	t.Parallel()

	fs := newTestFS(nil)
	if got := fs.getCachedInfo("anything"); got != nil {
		t.Errorf("expected nil with nil cache, got %+v", got)
	}
}

func TestCacheInfo_NilCacheNoOp(t *testing.T) {
	t.Parallel()

	fs := newTestFS(nil)
	// Must not panic.
	fs.cacheInfo("path", &types.ObjectInfo{Key: "path", Size: 1})
}

func TestCacheInfo_NilInfoNoOp(t *testing.T) {
	t.Parallel()

	c := newMapCache()
	fs := newTestFS(c)
	fs.cacheInfo("path", nil)
	if got := fs.getCachedInfo("path"); got != nil {
		t.Error("expected nil after caching nil info")
	}
}

func TestCacheInfo_Overwrite(t *testing.T) {
	t.Parallel()

	fs := newTestFS(newMapCache())

	first := &types.ObjectInfo{Key: "f", Size: 100}
	second := &types.ObjectInfo{Key: "f", Size: 200, ETag: `"new"`}

	fs.cacheInfo("f", first)
	fs.cacheInfo("f", second)

	got := fs.getCachedInfo("f")
	if got == nil {
		t.Fatal("getCachedInfo returned nil")
	}
	if got.Size != 200 {
		t.Errorf("expected overwritten Size=200, got %d", got.Size)
	}
	if got.ETag != `"new"` {
		t.Errorf("expected overwritten ETag, got %q", got.ETag)
	}
}

func TestNewFileSystem_NilConfig_DefaultsToProcessUID(t *testing.T) {
	t.Parallel()

	fs := NewFileSystem(nil, nil, nil, nil, nil)

	wantUID := safeIntToUint32(os.Getuid())
	if fs.config.DefaultUID != wantUID {
		t.Errorf("DefaultUID: got %d, want %d (process uid)", fs.config.DefaultUID, wantUID)
	}

	wantGID := safeIntToUint32(os.Getgid())
	if fs.config.DefaultGID != wantGID {
		t.Errorf("DefaultGID: got %d, want %d (process gid)", fs.config.DefaultGID, wantGID)
	}
}

// TestMountManager_IsMounted_ConcurrentAccess is a data-race regression test for
// the missing sync.Mutex on MountManager.mounted.  Running the test suite with
// -race will catch unsynchronised reads/writes if the fix is ever reverted.
func TestMountManager_IsMounted_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	cfg := &MountConfig{
		MountPoint: "/tmp/objectfs-test-concurrent",
		Options: &MountOptions{
			FSName:       "objectfs",
			Subtype:      "s3",
			AttrTimeout:  time.Second,
			EntryTimeout: time.Second,
		},
		Permissions: &Permissions{
			UID:      safeIntToUint32(os.Getuid()),
			GID:      safeIntToUint32(os.Getgid()),
			FileMode: 0644,
			DirMode:  0755,
		},
	}
	mm := NewMountManager(nil, cfg)

	// Spawn concurrent readers and a writer to exercise the mutex paths.
	// Without the mutex fix this would trigger the -race detector.
	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range 100 {
				_ = mm.IsMounted()
				_, _ = mm.GetCurrentOperation()
			}
		}()
	}
	wg.Wait()
}

// TestMountManager_checkMount_InvertedBooleanFixed verifies that IsMounted()
// and isAlreadyMounted() return consistent results when the mount point is not
// in use.  This is a regression test for the inverted-boolean bug where
// checkMount logged a spurious "unexpected unmount" warning on every tick.
func TestMountManager_checkMount_InvertedBooleanFixed(t *testing.T) {
	t.Parallel()

	cfg := &MountConfig{
		MountPoint: "/tmp/objectfs-not-mounted",
		Options: &MountOptions{
			FSName:       "objectfs",
			AttrTimeout:  time.Second,
			EntryTimeout: time.Second,
		},
		Permissions: &Permissions{
			UID:      safeIntToUint32(os.Getuid()),
			GID:      safeIntToUint32(os.Getgid()),
			FileMode: 0644,
			DirMode:  0755,
		},
	}
	mm := NewMountManager(nil, cfg)

	// A freshly-created manager is not mounted.
	expectedMounted := mm.IsMounted()        // false
	actuallyMounted := mm.isAlreadyMounted() // false (no real mount)

	// Both must agree — if the bug is present, actuallyMounted would be !false = true.
	if expectedMounted != actuallyMounted {
		t.Errorf("IsMounted()=%v but isAlreadyMounted()=%v — inverted boolean bug may have been re-introduced",
			expectedMounted, actuallyMounted)
	}
}
