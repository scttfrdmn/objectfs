//go:build cgofuse
// +build cgofuse

package fuse

import (
	"context"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"github.com/objectfs/objectfs/pkg/types"
)

// CgoFuseFS implements ObjectFS using cgofuse for cross-platform support
type CgoFuseFS struct {
	fuse.FileSystemBase
	
	// ObjectFS components
	backend     types.Backend
	cache       types.Cache
	writeBuffer types.WriteBuffer
	metrics     types.MetricsCollector
	config      *Config
	
	// Internal state
	mu          sync.RWMutex
	openFiles   map[uint64]*OpenFile
	nextHandle  uint64
	host        *fuse.FileSystemHost
	mounted     bool
}

// OpenFile represents an open file handle
type OpenFile struct {
	Path     string
	Data     []byte
	Offset   int64
	Modified bool
	Size     int64
}

// NewCgoFuseFS creates a new cgofuse-based filesystem
func NewCgoFuseFS(backend types.Backend, cache types.Cache, writeBuffer types.WriteBuffer, 
	metrics types.MetricsCollector, config *Config) *CgoFuseFS {
	
	return &CgoFuseFS{
		backend:     backend,
		cache:       cache,
		writeBuffer: writeBuffer,
		metrics:     metrics,
		config:      config,
		openFiles:   make(map[uint64]*OpenFile),
		nextHandle:  1,
	}
}

// Mount mounts the filesystem
func (fs *CgoFuseFS) Mount(ctx context.Context) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	
	if fs.mounted {
		return fmt.Errorf("filesystem already mounted")
	}
	
	fs.host = fuse.NewFileSystemHost(fs)
	
	// Mount options for cross-platform compatibility
	options := []string{
		"-o", "fsname=objectfs",
		"-o", "subtype=s3",
		"-o", "allow_other",
	}
	
	// Platform-specific options
	switch {
	case strings.Contains(os.Getenv("GOOS"), "darwin"):
		// macOS specific options
		options = append(options, "-o", "volname=ObjectFS")
	case strings.Contains(os.Getenv("GOOS"), "windows"):
		// Windows specific options
		options = append(options, "-o", "FileSystemName=ObjectFS")
	}
	
	go func() {
		ret := fs.host.Mount(fs.config.MountPoint, options)
		if ret != 0 {
			log.Printf("Mount failed with code: %d", ret)
		}
	}()
	
	// Wait a bit for mount to establish
	time.Sleep(100 * time.Millisecond)
	
	fs.mounted = true
	log.Printf("ObjectFS mounted at: %s", fs.config.MountPoint)
	return nil
}

// Unmount unmounts the filesystem
func (fs *CgoFuseFS) Unmount() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	
	if !fs.mounted {
		return fmt.Errorf("filesystem not mounted")
	}
	
	if fs.host != nil {
		ret := fs.host.Unmount()
		if ret != 0 {
			return fmt.Errorf("unmount failed with code: %d", ret)
		}
	}
	
	fs.mounted = false
	log.Printf("ObjectFS unmounted from: %s", fs.config.MountPoint)
	return nil
}

// IsMounted returns whether the filesystem is mounted
func (fs *CgoFuseFS) IsMounted() bool {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.mounted
}

// FUSE Operations Implementation

// Getattr gets file attributes
func (fs *CgoFuseFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	defer fs.recordOperation("getattr", time.Now())
	
	// Handle root directory
	if path == "/" {
		stat.Mode = fuse.S_IFDIR | 0755
		stat.Nlink = 2
		return 0
	}
	
	// Clean path for S3
	key := strings.TrimPrefix(path, "/")
	
	// Try cache first
	if info := fs.getCachedInfo(key); info != nil {
		fs.fillStat(stat, info)
		return 0
	}
	
	// Check S3
	ctx := context.Background()
	info, err := fs.backend.HeadObject(ctx, key)
	if err != nil {
		// Check if it might be a directory by trying to list with prefix
		objects, listErr := fs.backend.ListObjects(ctx, key+"/", 1)
		if listErr == nil && len(objects) > 0 {
			// It's a directory
			stat.Mode = fuse.S_IFDIR | 0755
			stat.Nlink = 2
			return 0
		}
		return -fuse.ENOENT
	}
	
	// Cache the info
	fs.cacheInfo(key, info)
	fs.fillStat(stat, info)
	return 0
}

// Open opens a file
func (fs *CgoFuseFS) Open(path string, flags int) (int, uint64) {
	defer fs.recordOperation("open", time.Now())
	
	key := strings.TrimPrefix(path, "/")
	
	fs.mu.Lock()
	handle := fs.nextHandle
	fs.nextHandle++
	
	fs.openFiles[handle] = &OpenFile{
		Path:   key,
		Offset: 0,
	}
	fs.mu.Unlock()
	
	return 0, handle
}

// Read reads from a file
func (fs *CgoFuseFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	start := time.Now()
	defer fs.recordOperation("read", start)
	
	key := strings.TrimPrefix(path, "/")
	
	// Try cache first
	if cached := fs.cache.Get(key, ofst, int64(len(buff))); cached != nil {
		fs.metrics.RecordCacheHit(key, int64(len(cached)))
		copy(buff, cached)
		return len(cached)
	}
	
	// Read from S3
	ctx := context.Background()
	data, err := fs.backend.GetObject(ctx, key, ofst, int64(len(buff)))
	if err != nil {
		return -fuse.EIO
	}
	
	// Cache the data
	fs.cache.Put(key, ofst, data)
	fs.metrics.RecordCacheMiss(key, int64(len(data)))
	
	copy(buff, data)
	return len(data)
}

// Write writes to a file
func (fs *CgoFuseFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	defer fs.recordOperation("write", time.Now())
	
	key := strings.TrimPrefix(path, "/")
	
	// Write to buffer
	err := fs.writeBuffer.Write(key, ofst, buff)
	if err != nil {
		return -fuse.EIO
	}
	
	return len(buff)
}

// Release closes a file
func (fs *CgoFuseFS) Release(path string, fh uint64) int {
	defer fs.recordOperation("release", time.Now())
	
	fs.mu.Lock()
	delete(fs.openFiles, fh)
	fs.mu.Unlock()
	
	return 0
}

// Readdir reads directory contents
func (fs *CgoFuseFS) Readdir(path string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	defer fs.recordOperation("readdir", time.Now())
	
	// Add standard entries
	fill(".", nil, 0)
	fill("..", nil, 0)
	
	// List objects from S3
	prefix := strings.TrimPrefix(path, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	
	ctx := context.Background()
	objects, err := fs.backend.ListObjects(ctx, prefix, 1000)
	if err != nil {
		return -fuse.EIO
	}
	
	// Convert S3 objects to directory entries
	seen := make(map[string]bool)
	for _, obj := range objects {
		relativePath := strings.TrimPrefix(obj.Key, prefix)
		if relativePath == "" {
			continue
		}
		
		// Handle directory-like structure
		parts := strings.Split(relativePath, "/")
		name := parts[0]
		
		if seen[name] {
			continue
		}
		seen[name] = true
		
		stat := &fuse.Stat_t{}
		if len(parts) > 1 {
			// It's a directory
			stat.Mode = fuse.S_IFDIR | 0755
			stat.Nlink = 2
		} else {
			// It's a file
			stat.Mode = fuse.S_IFREG | 0644
			stat.Size = obj.Size
			stat.Nlink = 1
		}
		
		if !fill(name, stat, 0) {
			break
		}
	}
	
	return 0
}

// Helper methods

func (fs *CgoFuseFS) getCachedInfo(key string) *types.ObjectInfo {
	// Simple implementation - in a real system you'd have a metadata cache
	return nil
}

func (fs *CgoFuseFS) cacheInfo(key string, info *types.ObjectInfo) {
	// Simple implementation - in a real system you'd cache metadata
}

func (fs *CgoFuseFS) fillStat(stat *fuse.Stat_t, info *types.ObjectInfo) {
	stat.Mode = fuse.S_IFREG | 0644
	stat.Size = info.Size
	stat.Nlink = 1
	stat.Mtim.Sec = info.LastModified.Unix()
	stat.Mtim.Nsec = info.LastModified.UnixNano() % 1e9
}

func (fs *CgoFuseFS) recordOperation(op string, start time.Time) {
	duration := time.Since(start)
	if fs.metrics != nil {
		fs.metrics.RecordOperation(op, duration, 0, true)
	}
}

// GetStats returns filesystem statistics
func (fs *CgoFuseFS) GetStats() *FilesystemStats {
	return &FilesystemStats{
		Lookups:     0, // TODO: implement proper stats
		Opens:       0,
		Reads:       0,
		Writes:      0,
		BytesRead:   0,
		BytesWritten: 0,
		CacheHits:   0,
		CacheMisses: 0,
		Errors:      0,
	}
}