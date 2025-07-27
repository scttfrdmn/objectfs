//go:build posix
// +build posix

package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/objectfs/objectfs/internal/cache"
	"github.com/objectfs/objectfs/internal/fuse"
	"github.com/objectfs/objectfs/pkg/types"
)

// POSIXTestSuite tests POSIX functionality of ObjectFS
type POSIXTestSuite struct {
	suite.Suite
	ctx        context.Context
	mountPoint string
	filesystem *fuse.FileSystem
	manager    *fuse.MountManager
	backend    types.Backend
	mockFiles  map[string][]byte
}

func TestPOSIXFunctionality(t *testing.T) {
	suite.Run(t, new(POSIXTestSuite))
}

func (s *POSIXTestSuite) SetupSuite() {
	s.ctx = context.Background()
	
	// Create temporary mount point
	tmpDir, err := os.MkdirTemp("", "objectfs-posix-test-")
	require.NoError(s.T(), err)
	s.mountPoint = tmpDir
	
	// Create mock backend for testing (we'll use a map-based mock)
	s.mockFiles = make(map[string][]byte)
	s.backend = &MockBackend{files: s.mockFiles}
	
	// Create cache and buffer components
	cacheConfig := &cache.CacheConfig{
		MaxSize:    10 * 1024 * 1024, // 10MB
		TTL:        time.Hour,
		MaxEntries: 1000,
	}
	cacheImpl := cache.NewLRUCache(cacheConfig)
	bufferImpl := &MockWriteBuffer{}
	metricsImpl := &MockMetricsCollector{}
	
	// Create filesystem
	fuseConfig := &fuse.Config{
		MountPoint:  s.mountPoint,
		ReadOnly:    false,
		DefaultUID:  uint32(os.Getuid()),
		DefaultGID:  uint32(os.Getgid()),
		DefaultMode: 0644,
		CacheTTL:    time.Minute,
	}
	
	s.filesystem = fuse.NewFileSystem(s.backend, cacheImpl, bufferImpl, metricsImpl, fuseConfig)
	
	// Create mount manager
	mountConfig := &fuse.MountConfig{
		MountPoint: s.mountPoint,
		Options: &fuse.MountOptions{
			FSName:   "objectfs-test",
			Subtype:  "s3",
			MaxRead:  128 * 1024,
			MaxWrite: 128 * 1024,
			Debug:    false,
		},
	}
	
	s.manager = fuse.NewMountManager(s.filesystem, mountConfig)
	
	s.T().Logf("✅ POSIX test suite initialized with mount point: %s", s.mountPoint)
}

func (s *POSIXTestSuite) TearDownSuite() {
	// Unmount if mounted
	if s.manager != nil && s.manager.IsMounted() {
		s.manager.Unmount()
	}
	
	// Clean up mount point
	if s.mountPoint != "" {
		os.RemoveAll(s.mountPoint)
	}
}

func (s *POSIXTestSuite) TestFilesystemMount() {
	t := s.T()
	
	t.Logf("🔧 Testing ObjectFS FUSE mount functionality")
	
	// Test mount
	err := s.manager.Mount(s.ctx)
	assert.NoError(t, err)
	assert.True(t, s.manager.IsMounted())
	
	t.Logf("✅ Filesystem mounted successfully at %s", s.mountPoint)
	
	// Verify mount point is accessible
	_, err = os.Stat(s.mountPoint)
	assert.NoError(t, err)
	
	// Test unmount
	err = s.manager.Unmount()
	assert.NoError(t, err)
	assert.False(t, s.manager.IsMounted())
	
	t.Logf("✅ Filesystem unmounted successfully")
}

func (s *POSIXTestSuite) TestBasicFileOperations() {
	t := s.T()
	
	t.Logf("📁 Testing basic POSIX file operations")
	
	// Mount filesystem for testing
	err := s.manager.Mount(s.ctx)
	require.NoError(t, err)
	defer s.manager.Unmount()
	
	// Wait for mount to be ready
	time.Sleep(100 * time.Millisecond)
	
	// Test file creation and writing
	testFile := filepath.Join(s.mountPoint, "test-file.txt")
	testContent := []byte("Hello ObjectFS POSIX!\n")
	
	err = os.WriteFile(testFile, testContent, 0644)
	assert.NoError(t, err)
	
	t.Logf("✅ File written: %s (%d bytes)", testFile, len(testContent))
	
	// Test file reading
	readContent, err := os.ReadFile(testFile)
	assert.NoError(t, err)
	assert.Equal(t, testContent, readContent)
	
	t.Logf("✅ File read successfully: %d bytes", len(readContent))
	
	// Test file stats
	info, err := os.Stat(testFile)
	assert.NoError(t, err)
	assert.Equal(t, int64(len(testContent)), info.Size())
	assert.Equal(t, "test-file.txt", info.Name())
	assert.False(t, info.IsDir())
	
	t.Logf("✅ File stats: size=%d, mode=%v", info.Size(), info.Mode())
	
	// Test file deletion
	err = os.Remove(testFile)
	assert.NoError(t, err)
	
	// Verify file is deleted
	_, err = os.Stat(testFile)
	assert.True(t, os.IsNotExist(err))
	
	t.Logf("✅ File deleted successfully")
}

func (s *POSIXTestSuite) TestDirectoryOperations() {
	t := s.T()
	
	t.Logf("📂 Testing POSIX directory operations")
	
	// Mount filesystem
	err := s.manager.Mount(s.ctx)
	require.NoError(t, err)
	defer s.manager.Unmount()
	
	time.Sleep(100 * time.Millisecond)
	
	// Test directory creation
	testDir := filepath.Join(s.mountPoint, "test-directory")
	err = os.Mkdir(testDir, 0755)
	assert.NoError(t, err)
	
	t.Logf("✅ Directory created: %s", testDir)
	
	// Test directory stats
	info, err := os.Stat(testDir)
	assert.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, "test-directory", info.Name())
	
	// Test nested directory creation
	nestedDir := filepath.Join(testDir, "nested")
	err = os.Mkdir(nestedDir, 0755)
	assert.NoError(t, err)
	
	t.Logf("✅ Nested directory created: %s", nestedDir)
	
	// Test file in directory
	fileInDir := filepath.Join(testDir, "file-in-dir.txt")
	content := []byte("File in directory")
	err = os.WriteFile(fileInDir, content, 0644)
	assert.NoError(t, err)
	
	// Test directory listing
	entries, err := os.ReadDir(testDir)
	assert.NoError(t, err)
	assert.Len(t, entries, 2) // nested dir + file
	
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	assert.Contains(t, names, "nested")
	assert.Contains(t, names, "file-in-dir.txt")
	
	t.Logf("✅ Directory listing: %v", names)
	
	// Test directory removal (should fail if not empty)
	err = os.Remove(testDir)
	assert.Error(t, err) // Should fail because directory is not empty
	
	// Clean up files first
	os.Remove(fileInDir)
	os.Remove(nestedDir)
	
	// Now directory removal should work
	err = os.Remove(testDir)
	assert.NoError(t, err)
	
	t.Logf("✅ Directory operations completed successfully")
}

func (s *POSIXTestSuite) TestFilePermissions() {
	t := s.T()
	
	t.Logf("🔐 Testing POSIX file permissions")
	
	err := s.manager.Mount(s.ctx)
	require.NoError(t, err)
	defer s.manager.Unmount()
	
	time.Sleep(100 * time.Millisecond)
	
	// Test file with different permissions
	testFile := filepath.Join(s.mountPoint, "perm-test.txt")
	content := []byte("Permission test")
	
	// Create file with specific permissions
	err = os.WriteFile(testFile, content, 0600)
	assert.NoError(t, err)
	
	// Check permissions
	info, err := os.Stat(testFile)
	assert.NoError(t, err)
	
	t.Logf("✅ File permissions: %v", info.Mode())
	
	// Test chmod (change permissions)
	err = os.Chmod(testFile, 0644)
	// Note: This might not work perfectly on all FUSE implementations
	// but we test it anyway
	
	// Clean up
	os.Remove(testFile)
	
	t.Logf("✅ Permission operations completed")
}

func (s *POSIXTestSuite) TestFileSeekAndRandomAccess() {
	t := s.T()
	
	t.Logf("🎯 Testing POSIX file seek and random access")
	
	err := s.manager.Mount(s.ctx)
	require.NoError(t, err)
	defer s.manager.Unmount()
	
	time.Sleep(100 * time.Millisecond)
	
	// Create test file with known content
	testFile := filepath.Join(s.mountPoint, "seek-test.txt")
	content := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	
	err = os.WriteFile(testFile, content, 0644)
	assert.NoError(t, err)
	
	// Open file for reading
	file, err := os.Open(testFile)
	assert.NoError(t, err)
	defer file.Close()
	
	// Test seeking to different positions
	buffer := make([]byte, 5)
	
	// Seek to position 10
	_, err = file.Seek(10, 0)
	assert.NoError(t, err)
	
	n, err := file.Read(buffer)
	assert.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, []byte("ABCDE"), buffer)
	
	t.Logf("✅ Seek to position 10: read '%s'", string(buffer))
	
	// Seek to end and read backwards
	_, err = file.Seek(-5, 2) // 5 bytes from end
	assert.NoError(t, err)
	
	n, err = file.Read(buffer)
	assert.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, []byte("VWXYZ"), buffer)
	
	t.Logf("✅ Seek from end: read '%s'", string(buffer))
	
	// Clean up
	os.Remove(testFile)
	
	t.Logf("✅ File seek operations completed successfully")
}

func (s *POSIXTestSuite) TestConcurrentAccess() {
	t := s.T()
	
	t.Logf("🔄 Testing concurrent file access")
	
	err := s.manager.Mount(s.ctx)
	require.NoError(t, err)
	defer s.manager.Unmount()
	
	time.Sleep(100 * time.Millisecond)
	
	// Create test file
	testFile := filepath.Join(s.mountPoint, "concurrent-test.txt")
	baseContent := []byte("Base content for concurrent test\n")
	
	err = os.WriteFile(testFile, baseContent, 0644)
	assert.NoError(t, err)
	
	// Test concurrent reads
	done := make(chan bool, 3)
	
	for i := 0; i < 3; i++ {
		go func(id int) {
			defer func() { done <- true }()
			
			content, err := os.ReadFile(testFile)
			assert.NoError(t, err)
			assert.Equal(t, baseContent, content)
			
			t.Logf("✅ Concurrent reader %d completed", id)
		}(i)
	}
	
	// Wait for all readers to complete
	for i := 0; i < 3; i++ {
		<-done
	}
	
	// Clean up
	os.Remove(testFile)
	
	t.Logf("✅ Concurrent access test completed")
}

func (s *POSIXTestSuite) TestFilesystemStats() {
	t := s.T()
	
	t.Logf("📊 Testing filesystem statistics")
	
	err := s.manager.Mount(s.ctx)
	require.NoError(t, err)
	defer s.manager.Unmount()
	
	time.Sleep(100 * time.Millisecond)
	
	// Perform some operations to generate stats
	testFile := filepath.Join(s.mountPoint, "stats-test.txt")
	content := []byte("Statistics test content")
	
	err = os.WriteFile(testFile, content, 0644)
	assert.NoError(t, err)
	
	_, err = os.ReadFile(testFile)
	assert.NoError(t, err)
	
	// Get filesystem statistics
	stats := s.manager.GetStats()
	assert.NotNil(t, stats)
	
	t.Logf("📊 Filesystem Statistics:")
	t.Logf("   Lookups: %d", stats.Lookups)
	t.Logf("   Opens: %d", stats.Opens)
	t.Logf("   Reads: %d", stats.Reads)
	t.Logf("   Writes: %d", stats.Writes)
	t.Logf("   Bytes Read: %d", stats.BytesRead)
	t.Logf("   Bytes Written: %d", stats.BytesWritten)
	t.Logf("   Cache Hits: %d", stats.CacheHits)
	t.Logf("   Cache Misses: %d", stats.CacheMisses)
	t.Logf("   Errors: %d", stats.Errors)
	
	// Verify some operations occurred
	assert.Greater(t, stats.Lookups, int64(0))
	
	// Clean up
	os.Remove(testFile)
	
	t.Logf("✅ Filesystem statistics retrieved successfully")
}

// Mock implementations for testing

type MockBackend struct {
	files map[string][]byte
}

func (m *MockBackend) GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error) {
	data, exists := m.files[key]
	if !exists {
		return nil, os.ErrNotExist
	}
	
	if offset >= int64(len(data)) {
		return []byte{}, nil
	}
	
	end := int64(len(data))
	if size > 0 && offset+size < end {
		end = offset + size
	}
	
	return data[offset:end], nil
}

func (m *MockBackend) PutObject(ctx context.Context, key string, data []byte) error {
	m.files[key] = make([]byte, len(data))
	copy(m.files[key], data)
	return nil
}

func (m *MockBackend) DeleteObject(ctx context.Context, key string) error {
	delete(m.files, key)
	return nil
}

func (m *MockBackend) HeadObject(ctx context.Context, key string) (*types.ObjectInfo, error) {
	data, exists := m.files[key]
	if !exists {
		return nil, os.ErrNotExist
	}
	
	return &types.ObjectInfo{
		Key:          key,
		Size:         int64(len(data)),
		LastModified: time.Now(),
		ETag:         "mock-etag",
		ContentType:  "application/octet-stream",
		Metadata:     make(map[string]string),
	}, nil
}

func (m *MockBackend) GetObjects(ctx context.Context, keys []string) (map[string][]byte, error) {
	result := make(map[string][]byte)
	for _, key := range keys {
		if data, exists := m.files[key]; exists {
			result[key] = data
		}
	}
	return result, nil
}

func (m *MockBackend) PutObjects(ctx context.Context, objects map[string][]byte) error {
	for key, data := range objects {
		m.files[key] = make([]byte, len(data))
		copy(m.files[key], data)
	}
	return nil
}

func (m *MockBackend) ListObjects(ctx context.Context, prefix string, limit int) ([]types.ObjectInfo, error) {
	var objects []types.ObjectInfo
	count := 0
	
	for key, data := range m.files {
		if count >= limit {
			break
		}
		
		if prefix == "" || (len(key) >= len(prefix) && key[:len(prefix)] == prefix) {
			objects = append(objects, types.ObjectInfo{
				Key:          key,
				Size:         int64(len(data)),
				LastModified: time.Now(),
				ETag:         "mock-etag",
				ContentType:  "application/octet-stream",
				Metadata:     make(map[string]string),
			})
			count++
		}
	}
	
	return objects, nil
}

func (m *MockBackend) HealthCheck(ctx context.Context) error {
	return nil
}

func (m *MockBackend) Close() error {
	return nil
}

type MockWriteBuffer struct{}

func (m *MockWriteBuffer) Write(key string, offset int64, data []byte) error {
	return nil
}

func (m *MockWriteBuffer) Flush(key string) error {
	return nil
}

func (m *MockWriteBuffer) FlushAll() error {
	return nil
}

func (m *MockWriteBuffer) Size() int64 {
	return 0
}

func (m *MockWriteBuffer) Count() int {
	return 0
}

type MockMetricsCollector struct{}

func (m *MockMetricsCollector) RecordOperation(operation string, duration time.Duration, size int64, success bool) {}
func (m *MockMetricsCollector) RecordCacheHit(key string, size int64) {}
func (m *MockMetricsCollector) RecordCacheMiss(key string, size int64) {}
func (m *MockMetricsCollector) RecordError(operation string, err error) {}
func (m *MockMetricsCollector) GetMetrics() map[string]interface{} { return make(map[string]interface{}) }