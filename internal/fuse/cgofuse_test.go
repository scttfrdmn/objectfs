//go:build cgofuse
// +build cgofuse

package fuse

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gofuse "github.com/winfsp/cgofuse/fuse"

	"github.com/objectfs/objectfs/pkg/types"
)

// ─── Mock implementations ────────────────────────────────────────────────────

// mockBackend satisfies types.Backend with an in-memory object store.
type mockBackend struct {
	mu      sync.RWMutex
	objects map[string][]byte
	// headErr forces HeadObject to return an error for the given key.
	headErr map[string]error
}

func newMockBackend() *mockBackend {
	return &mockBackend{
		objects: make(map[string][]byte),
		headErr: make(map[string]error),
	}
}

func (m *mockBackend) GetObject(_ context.Context, key string, offset, size int64) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	if offset >= int64(len(data)) {
		return []byte{}, nil
	}
	end := offset + size
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return append([]byte(nil), data[offset:end]...), nil
}

func (m *mockBackend) PutObject(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), data...)
	return nil
}

func (m *mockBackend) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *mockBackend) HeadObject(_ context.Context, key string) (*types.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err, forced := m.headErr[key]; forced {
		return nil, err
	}
	data, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	return &types.ObjectInfo{
		Key:          key,
		Size:         int64(len(data)),
		LastModified: time.Now(),
	}, nil
}

func (m *mockBackend) GetObjects(_ context.Context, keys []string) (map[string][]byte, error) {
	result := make(map[string][]byte, len(keys))
	for _, k := range keys {
		data, err := m.GetObject(context.Background(), k, 0, -1)
		if err == nil {
			result[k] = data
		}
	}
	return result, nil
}

func (m *mockBackend) PutObjects(_ context.Context, objects map[string][]byte) error {
	for k, v := range objects {
		if err := m.PutObject(context.Background(), k, v); err != nil {
			return err
		}
	}
	return nil
}

func (m *mockBackend) ListObjects(_ context.Context, prefix string, limit int) ([]types.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []types.ObjectInfo
	for k, data := range m.objects {
		if strings.HasPrefix(k, prefix) {
			result = append(result, types.ObjectInfo{
				Key:          k,
				Size:         int64(len(data)),
				LastModified: time.Now(),
			})
			if len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

func (m *mockBackend) HealthCheck(_ context.Context) error { return nil }

// mockCache satisfies types.Cache with a simple in-memory store keyed by
// object key (offset and size are used for a ranged lookup).
type mockCache struct {
	mu     sync.RWMutex
	data   map[string][]byte // key → full object bytes
	hits   int64
	misses int64
}

func newMockCache() *mockCache {
	return &mockCache{data: make(map[string][]byte)}
}

func (m *mockCache) Get(key string, offset, size int64) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.data[key]
	if !ok {
		m.misses++
		return nil
	}
	m.hits++
	if offset >= int64(len(raw)) {
		return []byte{}
	}
	end := offset + size
	if end > int64(len(raw)) {
		end = int64(len(raw))
	}
	return append([]byte(nil), raw[offset:end]...)
}

func (m *mockCache) Put(key string, _ int64, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = append([]byte(nil), data...)
}

func (m *mockCache) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
}

func (m *mockCache) Evict(_ int64) bool      { return true }
func (m *mockCache) Size() int64             { return 0 }
func (m *mockCache) Stats() types.CacheStats { return types.CacheStats{} }

// mockWriteBuffer satisfies types.WriteBuffer and records Write calls.
type mockWriteBuffer struct {
	mu    sync.Mutex
	calls []writeBufferCall
}

type writeBufferCall struct {
	key    string
	offset int64
	data   []byte
}

func newMockWriteBuffer() *mockWriteBuffer { return &mockWriteBuffer{} }

func (m *mockWriteBuffer) Write(key string, offset int64, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, writeBufferCall{
		key:    key,
		offset: offset,
		data:   append([]byte(nil), data...),
	})
	return nil
}

func (m *mockWriteBuffer) Flush(_ string) error { return nil }
func (m *mockWriteBuffer) FlushAll() error      { return nil }
func (m *mockWriteBuffer) Size() int64          { return 0 }
func (m *mockWriteBuffer) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// mockMetrics satisfies types.MetricsCollector as a no-op with counters.
type mockMetrics struct {
	mu          sync.Mutex
	operations  []string
	cacheHits   int64
	cacheMisses int64
	errors      int64
}

func (m *mockMetrics) RecordOperation(op string, _ time.Duration, _ int64, _ bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.operations = append(m.operations, op)
}

func (m *mockMetrics) RecordCacheHit(_ string, _ int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheHits++
}

func (m *mockMetrics) RecordCacheMiss(_ string, _ int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheMisses++
}

func (m *mockMetrics) RecordError(_ string, _ error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors++
}

func (m *mockMetrics) GetMetrics() map[string]interface{} { return nil }

// ─── Test helpers ─────────────────────────────────────────────────────────────

func newTestCgoFuseFS(t *testing.T) (*CgoFuseFS, *mockBackend, *mockCache, *mockWriteBuffer, *mockMetrics) {
	t.Helper()
	backend := newMockBackend()
	cache := newMockCache()
	buf := newMockWriteBuffer()
	metrics := &mockMetrics{}
	config := &Config{
		MountPoint:  "/tmp/test-objectfs",
		DefaultUID:  1000,
		DefaultGID:  1000,
		DefaultMode: 0644,
		CacheTTL:    5 * time.Minute,
	}
	fs := NewCgoFuseFS(backend, cache, buf, metrics, config)
	return fs, backend, cache, buf, metrics
}

// ─── Getattr ─────────────────────────────────────────────────────────────────

func TestCgoFuseFS_Getattr_Root(t *testing.T) {
	t.Parallel()
	fs, _, _, _, _ := newTestCgoFuseFS(t)

	var stat gofuse.Stat_t
	ret := fs.Getattr("/", &stat, 0)
	if ret != 0 {
		t.Fatalf("Getattr(\"/\"): expected 0, got %d", ret)
	}
	if stat.Mode&gofuse.S_IFMT != gofuse.S_IFDIR {
		t.Errorf("root mode: expected S_IFDIR, got mode=%o", stat.Mode)
	}
	if stat.Nlink < 2 {
		t.Errorf("root nlink: expected >= 2, got %d", stat.Nlink)
	}
}

func TestCgoFuseFS_Getattr_KnownFile(t *testing.T) {
	t.Parallel()
	fs, backend, _, _, _ := newTestCgoFuseFS(t)

	content := []byte("hello world")
	backend.objects["test.txt"] = content

	var stat gofuse.Stat_t
	ret := fs.Getattr("/test.txt", &stat, 0)
	if ret != 0 {
		t.Fatalf("Getattr for known file: expected 0, got %d", ret)
	}
	if stat.Size != int64(len(content)) {
		t.Errorf("expected Size=%d, got %d", len(content), stat.Size)
	}
	if stat.Mode&gofuse.S_IFREG == 0 {
		t.Errorf("expected S_IFREG, got mode=%o", stat.Mode)
	}
}

func TestCgoFuseFS_Getattr_UnknownFile(t *testing.T) {
	t.Parallel()
	fs, _, _, _, _ := newTestCgoFuseFS(t)

	var stat gofuse.Stat_t
	ret := fs.Getattr("/nonexistent.txt", &stat, 0)
	if ret != -gofuse.ENOENT {
		t.Errorf("expected -ENOENT (%d), got %d", -gofuse.ENOENT, ret)
	}
}

// ─── Open / Read ─────────────────────────────────────────────────────────────

func TestCgoFuseFS_Open_ReturnsHandle(t *testing.T) {
	t.Parallel()
	fs, _, _, _, _ := newTestCgoFuseFS(t)

	ret, fh := fs.Open("/test.txt", 0)
	if ret != 0 {
		t.Fatalf("Open: expected 0, got %d", ret)
	}
	if fh == 0 {
		t.Error("expected non-zero file handle")
	}
}

func TestCgoFuseFS_Read_FromBackend(t *testing.T) {
	t.Parallel()
	fs, backend, _, _, _ := newTestCgoFuseFS(t)

	content := []byte("hello world")
	backend.objects["test.txt"] = content

	_, fh := fs.Open("/test.txt", 0)
	buf := make([]byte, len(content))
	n := fs.Read("/test.txt", buf, 0, fh)

	if n != len(content) {
		t.Fatalf("expected to read %d bytes, got %d", len(content), n)
	}
	if string(buf[:n]) != string(content) {
		t.Errorf("expected %q, got %q", content, buf[:n])
	}
}

func TestCgoFuseFS_Read_CacheHit(t *testing.T) {
	t.Parallel()
	fs, _, cache, _, metrics := newTestCgoFuseFS(t)

	// Pre-populate cache; mockCache.Get returns data when key exists.
	data := []byte("cached content")
	cache.Put("cached.txt", 0, data)

	_, fh := fs.Open("/cached.txt", 0)
	buf := make([]byte, len(data))
	n := fs.Read("/cached.txt", buf, 0, fh)

	if n != len(data) {
		t.Fatalf("expected to read %d bytes from cache, got %d", len(data), n)
	}
	if string(buf[:n]) != string(data) {
		t.Errorf("expected %q, got %q", data, buf[:n])
	}

	metrics.mu.Lock()
	hits := metrics.cacheHits
	metrics.mu.Unlock()
	if hits != 1 {
		t.Errorf("expected 1 cache-hit metric, got %d", hits)
	}
}

func TestCgoFuseFS_Read_NonexistentFile_ReturnsEIO(t *testing.T) {
	t.Parallel()
	fs, _, _, _, _ := newTestCgoFuseFS(t)

	_, fh := fs.Open("/missing.txt", 0)
	buf := make([]byte, 64)
	ret := fs.Read("/missing.txt", buf, 0, fh)
	if ret != -gofuse.EIO {
		t.Errorf("expected -EIO (%d) for missing object, got %d", -gofuse.EIO, ret)
	}
}

// ─── Write ────────────────────────────────────────────────────────────────────

func TestCgoFuseFS_Write_SendsToBuffer(t *testing.T) {
	t.Parallel()
	fs, _, _, buf, _ := newTestCgoFuseFS(t)

	data := []byte("hello world")
	_, fh := fs.Open("/test.txt", 0)
	n := fs.Write("/test.txt", data, 0, fh)

	if n != len(data) {
		t.Fatalf("expected to write %d bytes, got %d", len(data), n)
	}

	buf.mu.Lock()
	defer buf.mu.Unlock()
	if len(buf.calls) != 1 {
		t.Fatalf("expected 1 write-buffer call, got %d", len(buf.calls))
	}
	if string(buf.calls[0].data) != string(data) {
		t.Errorf("write buffer data: expected %q, got %q", data, buf.calls[0].data)
	}
	if buf.calls[0].key != "test.txt" {
		t.Errorf("write buffer key: expected %q, got %q", "test.txt", buf.calls[0].key)
	}
}

func TestCgoFuseFS_Write_MultipleChunks(t *testing.T) {
	t.Parallel()
	fs, _, _, buf, _ := newTestCgoFuseFS(t)

	_, fh := fs.Open("/multi.txt", 0)
	fs.Write("/multi.txt", []byte("first"), 0, fh)
	fs.Write("/multi.txt", []byte("second"), 5, fh)

	buf.mu.Lock()
	defer buf.mu.Unlock()
	if len(buf.calls) != 2 {
		t.Errorf("expected 2 write calls, got %d", len(buf.calls))
	}
}

// ─── Release ─────────────────────────────────────────────────────────────────

func TestCgoFuseFS_Release_RemovesHandle(t *testing.T) {
	t.Parallel()
	fs, _, _, _, _ := newTestCgoFuseFS(t)

	_, fh := fs.Open("/file.txt", 0)

	fs.mu.RLock()
	_, exists := fs.openFiles[fh]
	fs.mu.RUnlock()
	if !exists {
		t.Fatal("expected open-file entry after Open")
	}

	ret := fs.Release("/file.txt", fh)
	if ret != 0 {
		t.Errorf("Release: expected 0, got %d", ret)
	}

	fs.mu.RLock()
	_, exists = fs.openFiles[fh]
	fs.mu.RUnlock()
	if exists {
		t.Error("expected open-file entry to be removed after Release")
	}
}

func TestCgoFuseFS_Release_MultipleHandles(t *testing.T) {
	t.Parallel()
	fs, _, _, _, _ := newTestCgoFuseFS(t)

	_, fh1 := fs.Open("/a.txt", 0)
	_, fh2 := fs.Open("/b.txt", 0)

	fs.Release("/a.txt", fh1)

	fs.mu.RLock()
	_, aExists := fs.openFiles[fh1]
	_, bExists := fs.openFiles[fh2]
	fs.mu.RUnlock()

	if aExists {
		t.Error("fh1 should have been removed after release")
	}
	if !bExists {
		t.Error("fh2 should still exist (not released)")
	}
}

// ─── Readdir ──────────────────────────────────────────────────────────────────

func TestCgoFuseFS_Readdir_IncludesDotEntries(t *testing.T) {
	t.Parallel()
	fs, _, _, _, _ := newTestCgoFuseFS(t)

	var names []string
	fill := func(name string, _ *gofuse.Stat_t, _ int64) bool {
		names = append(names, name)
		return true
	}

	ret := fs.Readdir("/", fill, 0, 0)
	if ret != 0 {
		t.Fatalf("Readdir: expected 0, got %d", ret)
	}

	hasEntry := func(n string) bool {
		for _, e := range names {
			if e == n {
				return true
			}
		}
		return false
	}
	for _, want := range []string{".", ".."} {
		if !hasEntry(want) {
			t.Errorf("Readdir: expected %q in results, got %v", want, names)
		}
	}
}

func TestCgoFuseFS_Readdir_ListsFiles(t *testing.T) {
	t.Parallel()
	fs, backend, _, _, _ := newTestCgoFuseFS(t)

	backend.objects["alpha.txt"] = []byte("a")
	backend.objects["beta.txt"] = []byte("b")

	var names []string
	fill := func(name string, _ *gofuse.Stat_t, _ int64) bool {
		names = append(names, name)
		return true
	}

	fs.Readdir("/", fill, 0, 0)

	hasEntry := func(n string) bool {
		for _, e := range names {
			if e == n {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"alpha.txt", "beta.txt"} {
		if !hasEntry(want) {
			t.Errorf("Readdir: expected %q in results, got %v", want, names)
		}
	}
}

// ─── Concurrency ─────────────────────────────────────────────────────────────

func TestCgoFuseFS_ConcurrentOpenRelease(t *testing.T) {
	t.Parallel()
	fs, _, _, _, _ := newTestCgoFuseFS(t)

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ret, fh := fs.Open("/concurrent.txt", 0)
			if ret != 0 {
				t.Errorf("Open: expected 0, got %d", ret)
				return
			}
			fs.Release("/concurrent.txt", fh)
		}()
	}
	wg.Wait()

	fs.mu.RLock()
	remaining := len(fs.openFiles)
	fs.mu.RUnlock()
	if remaining != 0 {
		t.Errorf("expected 0 open files after all releases, got %d", remaining)
	}
}

func TestCgoFuseFS_ConcurrentWrites(t *testing.T) {
	t.Parallel()
	fs, _, _, buf, _ := newTestCgoFuseFS(t)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, fh := fs.Open("/shared.txt", 0)
			fs.Write("/shared.txt", []byte("data"), int64(idx)*4, fh)
			fs.Release("/shared.txt", fh)
		}(i)
	}
	wg.Wait()

	buf.mu.Lock()
	callCount := len(buf.calls)
	buf.mu.Unlock()
	if callCount != n {
		t.Errorf("expected %d write-buffer calls, got %d", n, callCount)
	}
}

// ─── Handle monotonicity ──────────────────────────────────────────────────────

func TestCgoFuseFS_HandlesAreUnique(t *testing.T) {
	t.Parallel()
	fs, _, _, _, _ := newTestCgoFuseFS(t)

	seen := make(map[uint64]bool)
	for i := 0; i < 10; i++ {
		_, fh := fs.Open("/f.txt", 0)
		if seen[fh] {
			t.Errorf("duplicate file handle: %d", fh)
		}
		seen[fh] = true
	}
}

// ─── UID/GID configurability (#78) ───────────────────────────────────────────

func TestNewCgoFuseMountManager_UsesPermissions(t *testing.T) {
	t.Parallel()

	cfg := &MountConfig{
		MountPoint: "/tmp/test-objectfs",
		Options:    &MountOptions{MaxRead: 128 * 1024},
		Permissions: &Permissions{
			UID: 42,
			GID: 43,
		},
	}
	m := NewCgoFuseMountManager(newMockBackend(), newMockCache(), newMockWriteBuffer(), &mockMetrics{}, cfg)

	if m.filesystem.config.DefaultUID != 42 {
		t.Errorf("DefaultUID: got %d, want 42", m.filesystem.config.DefaultUID)
	}
	if m.filesystem.config.DefaultGID != 43 {
		t.Errorf("DefaultGID: got %d, want 43", m.filesystem.config.DefaultGID)
	}
}

func TestNewCgoFuseMountManager_NilPermissions_DefaultsToProcess(t *testing.T) {
	t.Parallel()

	cfg := &MountConfig{
		MountPoint:  "/tmp/test-objectfs",
		Options:     &MountOptions{MaxRead: 128 * 1024},
		Permissions: nil,
	}
	m := NewCgoFuseMountManager(newMockBackend(), newMockCache(), newMockWriteBuffer(), &mockMetrics{}, cfg)

	wantUID := safeIntToUint32(os.Getuid())
	if m.filesystem.config.DefaultUID != wantUID {
		t.Errorf("DefaultUID: got %d, want %d (process uid)", m.filesystem.config.DefaultUID, wantUID)
	}

	wantGID := safeIntToUint32(os.Getgid())
	if m.filesystem.config.DefaultGID != wantGID {
		t.Errorf("DefaultGID: got %d, want %d (process gid)", m.filesystem.config.DefaultGID, wantGID)
	}
}

func TestNewCgoFuseMountManager_ZeroPermissionsUID_DefaultsToProcess(t *testing.T) {
	t.Parallel()

	// UID=0 and GID=0 in Permissions are treated as "not set" and fall back
	// to the process identity (root is not a useful sentinel in this context).
	cfg := &MountConfig{
		MountPoint:  "/tmp/test-objectfs",
		Options:     &MountOptions{MaxRead: 128 * 1024},
		Permissions: &Permissions{UID: 0, GID: 0},
	}
	m := NewCgoFuseMountManager(newMockBackend(), newMockCache(), newMockWriteBuffer(), &mockMetrics{}, cfg)

	wantUID := safeIntToUint32(os.Getuid())
	if m.filesystem.config.DefaultUID != wantUID {
		t.Errorf("DefaultUID: got %d, want %d (process uid) for zero-value Permissions", m.filesystem.config.DefaultUID, wantUID)
	}
}
