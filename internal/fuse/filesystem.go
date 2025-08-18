package fuse

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/objectfs/objectfs/pkg/types"
)

// safeInt64ToUint64 safely converts int64 to uint64, preventing negative values
func safeInt64ToUint64(i int64) uint64 {
	if i < 0 {
		return 0
	}
	return uint64(i)
}

// safeIntToUint32 safely converts int to uint32, preventing overflow
func safeIntToUint32(i int) uint32 {
	if i < 0 {
		return 0
	}
	if i > 0xFFFFFFFF {
		return 0xFFFFFFFF
	}
	return uint32(i)
}

// FileSystem implements the FUSE filesystem interface
type FileSystem struct {
	fs.Inode

	// Backend storage
	backend types.Backend
	cache   types.Cache
	buffer  types.WriteBuffer
	metrics types.MetricsCollector

	// Configuration
	config *Config

	// Internal state
	mu           sync.RWMutex
	openFiles    map[uint64]*OpenFile
	nextHandle   uint64

	// Performance tracking
	stats        *Stats

	// Performance optimizations
	readAhead    *ReadAheadManager
	writeCoalescer *WriteCoalescer
}

// Config represents FUSE filesystem configuration
type Config struct {
	// Mount options
	MountPoint   string `yaml:"mount_point"`
	ReadOnly     bool   `yaml:"read_only"`
	AllowOther   bool   `yaml:"allow_other"`

	// FUSE options
	DirectIO     bool   `yaml:"direct_io"`
	KeepCache    bool   `yaml:"keep_cache"`
	BigWrites    bool   `yaml:"big_writes"`
	MaxRead      uint32 `yaml:"max_read"`
	MaxWrite     uint32 `yaml:"max_write"`

	// Filesystem behavior
	DefaultUID   uint32        `yaml:"default_uid"`
	DefaultGID   uint32        `yaml:"default_gid"`
	DefaultMode  uint32        `yaml:"default_mode"`
	CacheTTL     time.Duration `yaml:"cache_ttl"`

	// Performance settings
	ReadAhead    uint32 `yaml:"read_ahead"`
	WriteBuffer  uint32 `yaml:"write_buffer"`
	Concurrency  int    `yaml:"concurrency"`
}

// OpenFile represents an open file handle
type OpenFile struct {
	path     string
	flags    uint32
	mode     uint32
	size     int64
	modified bool
	dirty    bool

	// Access tracking
	lastAccess time.Time
	accessCount int64
}

// Stats tracks filesystem operation statistics
type Stats struct {
	mu             sync.RWMutex

	// Operation counts
	Lookups        int64 `json:"lookups"`
	Opens          int64 `json:"opens"`
	Reads          int64 `json:"reads"`
	Writes         int64 `json:"writes"`
	Creates        int64 `json:"creates"`
	Deletes        int64 `json:"deletes"`

	// Data transfer
	BytesRead      int64 `json:"bytes_read"`
	BytesWritten   int64 `json:"bytes_written"`

	// Cache statistics
	CacheHits      int64 `json:"cache_hits"`
	CacheMisses    int64 `json:"cache_misses"`

	// Error counts
	Errors         int64 `json:"errors"`

	// Performance metrics
	AvgReadTime    time.Duration `json:"avg_read_time"`
	AvgWriteTime   time.Duration `json:"avg_write_time"`
	AvgLookupTime  time.Duration `json:"avg_lookup_time"`
}

// NewFileSystem creates a new FUSE filesystem instance
func NewFileSystem(backend types.Backend, cache types.Cache, buffer types.WriteBuffer, metrics types.MetricsCollector, config *Config) *FileSystem {
	if config == nil {
		config = &Config{
			DefaultUID:  1000,
			DefaultGID:  1000,
			DefaultMode: 0644,
			CacheTTL:    5 * time.Minute,
			ReadAhead:   128 * 1024,
			WriteBuffer: 64 * 1024,
			Concurrency: 16,
		}
	}

	filesystem := &FileSystem{
		backend:    backend,
		cache:      cache,
		buffer:     buffer,
		metrics:    metrics,
		config:     config,
		openFiles:  make(map[uint64]*OpenFile),
		nextHandle: 1,
		stats:      &Stats{},
	}

	// Initialize performance optimizations
	filesystem.readAhead = NewReadAheadManager(filesystem, nil)
	filesystem.writeCoalescer = NewWriteCoalescer(filesystem, nil)

	return filesystem
}

// Root returns the root inode
func (fs *FileSystem) Root() fs.InodeEmbedder {
	return &DirectoryNode{
		fs:   fs,
		path: "",
	}
}

// GetStats returns current filesystem statistics
func (fs *FileSystem) GetStats() *Stats {
	fs.stats.mu.RLock()
	defer fs.stats.mu.RUnlock()

	return &Stats{
		Lookups:      fs.stats.Lookups,
		Opens:        fs.stats.Opens,
		Reads:        fs.stats.Reads,
		Writes:       fs.stats.Writes,
		BytesRead:    fs.stats.BytesRead,
		BytesWritten: fs.stats.BytesWritten,
		CacheHits:    fs.stats.CacheHits,
		CacheMisses:  fs.stats.CacheMisses,
		Errors:       fs.stats.Errors,
	}
}

// DirectoryNode represents a directory in the filesystem
type DirectoryNode struct {
	fs.Inode
	fs   *FileSystem
	path string
}

// Lookup looks up a child node by name
func (n *DirectoryNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	start := time.Now()
	defer func() {
		n.fs.recordLookupTime(time.Since(start))
	}()

	n.fs.stats.mu.Lock()
	n.fs.stats.Lookups++
	n.fs.stats.mu.Unlock()

	childPath := n.joinPath(name)

	// Check cache first
	if cachedInfo := n.fs.getCachedInfo(childPath); cachedInfo != nil {
		n.fs.stats.mu.Lock()
		n.fs.stats.CacheHits++
		n.fs.stats.mu.Unlock()

		return n.createChildNode(name, cachedInfo), 0
	}

	// Query backend
	info, err := n.fs.backend.HeadObject(ctx, childPath)
	if err != nil {
		n.fs.stats.mu.Lock()
		n.fs.stats.Errors++
		n.fs.stats.CacheMisses++
		n.fs.stats.mu.Unlock()

		// Try as directory by listing
		objects, listErr := n.fs.backend.ListObjects(ctx, childPath+"/", 1)
		if listErr != nil || len(objects) == 0 {
			return nil, syscall.ENOENT
		}

		// It's a directory
		return n.createDirectoryNode(name, childPath), 0
	}

	n.fs.stats.mu.Lock()
	n.fs.stats.CacheMisses++
	n.fs.stats.mu.Unlock()

	// Cache the result
	n.fs.cacheInfo(childPath, info)

	return n.createChildNode(name, info), 0
}

// Readdir reads directory contents
func (n *DirectoryNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	prefix := n.path
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	objects, err := n.fs.backend.ListObjects(ctx, prefix, 1000) // List up to 1000 objects
	if err != nil {
		n.fs.stats.mu.Lock()
		n.fs.stats.Errors++
		n.fs.stats.mu.Unlock()

		log.Printf("Readdir failed for %s: %v", n.path, err)
		return nil, syscall.EIO
	}

	entries := make([]fuse.DirEntry, 0, len(objects))
	seen := make(map[string]bool)

	for _, obj := range objects {
		// Remove prefix to get relative name
		name := strings.TrimPrefix(obj.Key, prefix)

		// Handle nested directories
		if slashIdx := strings.Index(name, "/"); slashIdx != -1 {
			// This is a subdirectory
			dirName := name[:slashIdx]
			if !seen[dirName] {
				entries = append(entries, fuse.DirEntry{
					Name: dirName,
					Mode: fuse.S_IFDIR,
				})
				seen[dirName] = true
			}
		} else if name != "" {
			// This is a file
			entries = append(entries, fuse.DirEntry{
				Name: name,
				Mode: fuse.S_IFREG,
			})
		}
	}

	return fs.NewListDirStream(entries), 0
}

// Mkdir creates a new directory
func (n *DirectoryNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.fs.config.ReadOnly {
		return nil, syscall.EROFS
	}

	childPath := n.joinPath(name) + "/"

	// Create an empty object to represent the directory
	err := n.fs.backend.PutObject(ctx, childPath, []byte{})
	if err != nil {
		n.fs.stats.mu.Lock()
		n.fs.stats.Errors++
		n.fs.stats.mu.Unlock()

		log.Printf("Mkdir failed for %s: %v", childPath, err)
		return nil, syscall.EIO
	}

	return n.createDirectoryNode(name, childPath), 0
}

// Create creates a new file
func (n *DirectoryNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (node *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	if n.fs.config.ReadOnly {
		return nil, nil, 0, syscall.EROFS
	}

	childPath := n.joinPath(name)

	// Create empty file in backend
	err := n.fs.backend.PutObject(ctx, childPath, []byte{})
	if err != nil {
		n.fs.stats.mu.Lock()
		n.fs.stats.Errors++
		n.fs.stats.mu.Unlock()

		log.Printf("Create failed for %s: %v", childPath, err)
		return nil, nil, 0, syscall.EIO
	}

	n.fs.stats.mu.Lock()
	n.fs.stats.Creates++
	n.fs.stats.mu.Unlock()

	// Create object info for new file
	info := &types.ObjectInfo{
		Key:          childPath,
		Size:         0,
		LastModified: time.Now(),
	}

	// Create file node
	fileNode := &FileNode{
		fs:   n.fs,
		path: childPath,
		info: info,
	}

	node = n.NewInode(ctx, fileNode, fs.StableAttr{
		Mode: fuse.S_IFREG,
	})

	// Open the file immediately
	fh, fuseFlags, errno = fileNode.Open(ctx, flags)

	return node, fh, fuseFlags, errno
}

// FileNode represents a file in the filesystem
type FileNode struct {
	fs.Inode
	fs   *FileSystem
	path string
	info *types.ObjectInfo
}

// Open opens a file
func (f *FileNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	f.fs.stats.mu.Lock()
	f.fs.stats.Opens++
	f.fs.stats.mu.Unlock()

	// Check if write access on read-only filesystem
	if f.fs.config.ReadOnly && (flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_CREAT|syscall.O_TRUNC) != 0) {
		return nil, 0, syscall.EROFS
	}

	f.fs.mu.Lock()
	handle := f.fs.nextHandle
	f.fs.nextHandle++

	openFile := &OpenFile{
		path:        f.path,
		flags:       flags,
		mode:        0644,
		size:        f.info.Size,
		lastAccess:  time.Now(),
		accessCount: 1,
	}

	f.fs.openFiles[handle] = openFile
	f.fs.mu.Unlock()

	return &FileHandle{
		fs:     f.fs,
		handle: handle,
		file:   openFile,
	}, 0, 0
}

// Getattr gets file attributes
func (f *FileNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = f.fs.config.DefaultMode
	// Safely convert int64 to uint64 to prevent integer overflow
	out.Size = safeInt64ToUint64(f.info.Size)
	out.Uid = f.fs.config.DefaultUID
	out.Gid = f.fs.config.DefaultGID

	// Safely convert Unix timestamp to prevent integer overflow
	unixTime := f.info.LastModified.Unix()
	out.Mtime = safeInt64ToUint64(unixTime)
	out.Atime = safeInt64ToUint64(unixTime)
	out.Ctime = safeInt64ToUint64(unixTime)

	return 0
}

// FileHandle represents an open file handle
type FileHandle struct {
	fs     *FileSystem
	handle uint64
	file   *OpenFile
}

// Read reads data from the file
func (fh *FileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	start := time.Now()
	defer func() {
		fh.fs.recordReadTime(time.Since(start))
	}()

	fh.fs.stats.mu.Lock()
	fh.fs.stats.Reads++
	fh.fs.stats.mu.Unlock()

	// Update access tracking
	fh.file.lastAccess = time.Now()
	fh.file.accessCount++

	// Try cache first
	if cachedData := fh.fs.cache.Get(fh.file.path, off, int64(len(dest))); cachedData != nil {
		fh.fs.stats.mu.Lock()
		fh.fs.stats.CacheHits++
		fh.fs.stats.BytesRead += int64(len(cachedData))
		fh.fs.stats.mu.Unlock()

		return fuse.ReadResultData(cachedData), 0
	}

	// Read from backend
	data, err := fh.fs.backend.GetObject(ctx, fh.file.path, off, int64(len(dest)))
	if err != nil {
		fh.fs.stats.mu.Lock()
		fh.fs.stats.Errors++
		fh.fs.stats.CacheMisses++
		fh.fs.stats.mu.Unlock()

		log.Printf("Read failed for %s at offset %d: %v", fh.file.path, off, err)
		return nil, syscall.EIO
	}

	fh.fs.stats.mu.Lock()
	fh.fs.stats.CacheMisses++
	fh.fs.stats.BytesRead += int64(len(data))
	fh.fs.stats.mu.Unlock()

	// Cache the data
	fh.fs.cache.Put(fh.file.path, off, data)

	// Record metrics
	if fh.fs.metrics != nil {
		fh.fs.metrics.RecordCacheMiss(fh.file.path, int64(len(data)))
	}

	// Trigger read-ahead analysis
	if fh.fs.readAhead != nil {
		fh.fs.readAhead.OnRead(fh.file.path, off, int64(len(data)))
	}

	return fuse.ReadResultData(data), 0
}

// Write writes data to the file
func (fh *FileHandle) Write(ctx context.Context, data []byte, off int64) (written uint32, errno syscall.Errno) {
	if fh.fs.config.ReadOnly {
		return 0, syscall.EROFS
	}

	start := time.Now()
	defer func() {
		fh.fs.recordWriteTime(time.Since(start))
	}()

	fh.fs.stats.mu.Lock()
	fh.fs.stats.Writes++
	fh.fs.stats.BytesWritten += int64(len(data))
	fh.fs.stats.mu.Unlock()

	// Update file info
	fh.file.modified = true
	fh.file.dirty = true
	fh.file.lastAccess = time.Now()

	// Try write coalescing first
	coalesced := false
	if fh.fs.writeCoalescer != nil {
		coalesced = fh.fs.writeCoalescer.CoalesceWrite(fh.file.path, off, data)
	}

	if !coalesced {
		// Use write buffer for efficiency
		err := fh.fs.buffer.Write(fh.file.path, off, data)
		if err != nil {
			fh.fs.stats.mu.Lock()
			fh.fs.stats.Errors++
			fh.fs.stats.mu.Unlock()

			log.Printf("Write failed for %s at offset %d: %v", fh.file.path, off, err)
			return 0, syscall.EIO
		}
	}

	// Update file size if we wrote past the end
	newSize := off + int64(len(data))
	if newSize > fh.file.size {
		fh.file.size = newSize
	}

	return safeIntToUint32(len(data)), 0
}

// Flush flushes any pending writes
func (fh *FileHandle) Flush(ctx context.Context) syscall.Errno {
	if !fh.file.dirty {
		return 0
	}

	err := fh.fs.buffer.Flush(fh.file.path)
	if err != nil {
		fh.fs.stats.mu.Lock()
		fh.fs.stats.Errors++
		fh.fs.stats.mu.Unlock()

		log.Printf("Flush failed for %s: %v", fh.file.path, err)
		return syscall.EIO
	}

	fh.file.dirty = false
	return 0
}

// Release releases the file handle
func (fh *FileHandle) Release(ctx context.Context) syscall.Errno {
	// Flush any coalesced writes first
	if fh.fs.writeCoalescer != nil {
		fh.fs.writeCoalescer.FlushAll()
	}

	// Flush any pending writes
	if fh.file.dirty {
		_ = fh.Flush(ctx)
	}

	// Remove from open files map
	fh.fs.mu.Lock()
	delete(fh.fs.openFiles, fh.handle)
	fh.fs.mu.Unlock()

	return 0
}

// Helper methods for DirectoryNode

func (n *DirectoryNode) joinPath(name string) string {
	if n.path == "" {
		return name
	}
	return filepath.Join(n.path, name)
}

func (n *DirectoryNode) createChildNode(name string, info *types.ObjectInfo) *fs.Inode {
	childPath := n.joinPath(name)

	fileNode := &FileNode{
		fs:   n.fs,
		path: childPath,
		info: info,
	}

	return n.NewInode(context.Background(), fileNode, fs.StableAttr{
		Mode: fuse.S_IFREG,
	})
}

func (n *DirectoryNode) createDirectoryNode(name, path string) *fs.Inode {
	dirNode := &DirectoryNode{
		fs:   n.fs,
		path: path,
	}

	return n.NewInode(context.Background(), dirNode, fs.StableAttr{
		Mode: fuse.S_IFDIR,
	})
}

// Helper methods for FileSystem

func (fs *FileSystem) getCachedInfo(path string) *types.ObjectInfo {
	// Try to get metadata from cache
	if fs.cache != nil {
		// Use a special metadata key prefix
		metaKey := "__meta__" + path
		if cachedData := fs.cache.Get(metaKey, 0, 1024); cachedData != nil {
			// In a real implementation, deserialize ObjectInfo from cached data
			// For now, return nil to force backend lookup
		}
	}
	return nil
}

func (fs *FileSystem) cacheInfo(path string, info *types.ObjectInfo) {
	if fs.cache != nil && info != nil {
		// Use a special metadata key prefix
		metaKey := "__meta__" + path
		// In a real implementation, serialize ObjectInfo to bytes
		// For now, just cache a placeholder
		metaData := []byte(fmt.Sprintf("%d:%d", info.Size, info.LastModified.Unix()))
		fs.cache.Put(metaKey, 0, metaData)
	}
}

func (fs *FileSystem) recordLookupTime(duration time.Duration) {
	fs.stats.mu.Lock()
	defer fs.stats.mu.Unlock()

	if fs.stats.Lookups == 1 {
		fs.stats.AvgLookupTime = duration
	} else {
		fs.stats.AvgLookupTime = time.Duration(
			(int64(fs.stats.AvgLookupTime)*9 + int64(duration)) / 10,
		)
	}
}

func (fs *FileSystem) recordReadTime(duration time.Duration) {
	fs.stats.mu.Lock()
	defer fs.stats.mu.Unlock()

	if fs.stats.Reads == 1 {
		fs.stats.AvgReadTime = duration
	} else {
		fs.stats.AvgReadTime = time.Duration(
			(int64(fs.stats.AvgReadTime)*9 + int64(duration)) / 10,
		)
	}
}

func (fs *FileSystem) recordWriteTime(duration time.Duration) {
	fs.stats.mu.Lock()
	defer fs.stats.mu.Unlock()

	if fs.stats.Writes == 1 {
		fs.stats.AvgWriteTime = duration
	} else {
		fs.stats.AvgWriteTime = time.Duration(
			(int64(fs.stats.AvgWriteTime)*9 + int64(duration)) / 10,
		)
	}
}
