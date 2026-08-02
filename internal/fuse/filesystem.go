//go:build linux || darwin

package fuse

import (
	"context"
	"encoding/json"
	iofs "io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/objectfs/objectfs/internal/vfs"
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

	// buffer is the write path, held as the concrete *vfs.Writer rather than as types.WriteBuffer.
	//
	// The node contract needs Truncate, SetAttr, Attr, FlushContext, and Dirty, none of which are on
	// types.WriteBuffer. Widening that interface instead would be the wrong move: it is the contract
	// pkg/types publishes for any write buffer, and every implementation would have to grow five
	// methods to satisfy a single consumer. The seam that mattered — "the FUSE layer must not reach
	// past the write path to the backend for a file's size" — is preserved by the write path owning
	// those answers, not by the layer above it holding an interface.
	buffer *vfs.Writer

	metrics types.MetricsCollector

	// Configuration
	config *Config

	// Internal state
	mu         sync.RWMutex
	openFiles  map[uint64]*OpenFile
	nextHandle uint64

	// Performance tracking
	stats *Stats

	// Performance optimizations
	readAhead *ReadAheadManager
}

// Config represents FUSE filesystem configuration
type Config struct {
	// Mount options
	MountPoint string `yaml:"mount_point"`
	ReadOnly   bool   `yaml:"read_only"`
	AllowOther bool   `yaml:"allow_other"`

	// FUSE options
	DirectIO  bool   `yaml:"direct_io"`
	KeepCache bool   `yaml:"keep_cache"`
	BigWrites bool   `yaml:"big_writes"`
	MaxRead   uint32 `yaml:"max_read"`
	MaxWrite  uint32 `yaml:"max_write"`

	// Filesystem behavior.
	//
	// These are the attributes reported for anything the object store does not record: an object
	// written by another tool carries no objectfs-uid, and a directory is a key prefix with no metadata
	// at all. They are defaults, not overrides — an object that records its own mode reports that mode.
	// See [FileSystem.fileDefaults] and [FileSystem.dirDefaults].
	DefaultUID uint32 `yaml:"default_uid"`
	DefaultGID uint32 `yaml:"default_gid"`

	// DefaultMode is the permission bits for a file, DefaultDirMode for a directory.
	//
	// They are separate because one value cannot serve both: a directory needs its execute bits to be
	// traversable, and a file that reported 0755 would be executable. v0.10.0 had only DefaultMode,
	// applied it to files, and reported nothing at all for directories — which is how every directory
	// came to report mode 0000.
	DefaultMode    uint32 `yaml:"default_mode"`
	DefaultDirMode uint32 `yaml:"default_dir_mode"`

	// CacheTTL is how long the kernel may cache an attribute set, as returned by Getattr and Setattr.
	CacheTTL time.Duration `yaml:"cache_ttl"`

	// Performance settings
	ReadAhead   uint32 `yaml:"read_ahead"`
	WriteBuffer uint32 `yaml:"write_buffer"`
	Concurrency int    `yaml:"concurrency"`
}

// OpenFile is the per-descriptor state one open(2) produced.
//
// It holds no size and no mode. Both were here through v0.10.0 and both were a second source of
// truth: the write path knows a file's current length including pending writes, and the object's
// metadata knows its mode, so a copy on the handle could only ever be the value at open time. A
// handle's stale size is not a cosmetic problem — it is what a read gets clamped against when another
// descriptor extends or truncates the same path.
//
// The open flags are not here either. FUSE supplies an explicit offset on every read and write, and
// the kernel resolves O_APPEND to an offset before the request arrives, so nothing in this package
// has a use for them.
type OpenFile struct {
	// path is immutable and readable without the lock.
	path string

	// Everything below is guarded by accessMu, because FUSE delivers concurrent Read and Write calls
	// for the same open file descriptor (#107).
	accessMu sync.Mutex

	// dirty says this descriptor has written something not yet flushed. It is an optimization for the
	// read path — a dirty file is served from the write path rather than the cache — not a durability
	// record; [vfs.Writer.Dirty] owns that.
	dirty       bool
	lastAccess  time.Time
	accessCount int64
}

// markDirty records that this descriptor has unflushed writes.
func (f *OpenFile) markDirty() {
	f.accessMu.Lock()
	defer f.accessMu.Unlock()
	f.dirty = true
	f.lastAccess = time.Now()
}

// markClean records that everything this descriptor wrote has reached storage.
//
// Only a flush that reported success may call this. Clearing the flag after a failed upload would
// send the next read to the cache and the object store, neither of which holds the bytes that failed
// to upload — so a file whose write was rejected would start reading as its pre-write self.
func (f *OpenFile) markClean() {
	f.accessMu.Lock()
	defer f.accessMu.Unlock()
	f.dirty = false
}

// touch records an access and reports whether this descriptor has unflushed writes.
//
// One call rather than two because the read path needs both under the same acquisition: reading the
// dirty flag outside the lock that Write takes to set it was P-2, a live data race ten lines from its
// own fix.
func (f *OpenFile) touch() bool {
	f.accessMu.Lock()
	defer f.accessMu.Unlock()
	f.lastAccess = time.Now()
	f.accessCount++
	return f.dirty
}

// Stats tracks filesystem operation statistics
type Stats struct {
	mu sync.RWMutex

	// Operation counts
	Lookups int64 `json:"lookups"`
	Opens   int64 `json:"opens"`
	Reads   int64 `json:"reads"`
	Writes  int64 `json:"writes"`
	Creates int64 `json:"creates"`
	Deletes int64 `json:"deletes"`

	// Data transfer
	BytesRead    int64 `json:"bytes_read"`
	BytesWritten int64 `json:"bytes_written"`

	// Cache statistics
	CacheHits   int64 `json:"cache_hits"`
	CacheMisses int64 `json:"cache_misses"`

	// Error counts
	Errors int64 `json:"errors"`

	// Performance metrics
	AvgReadTime   time.Duration `json:"avg_read_time"`
	AvgWriteTime  time.Duration `json:"avg_write_time"`
	AvgLookupTime time.Duration `json:"avg_lookup_time"`
}

// NewFileSystem creates a new FUSE filesystem instance.
//
// A nil config defaults to the calling process as the owner of anything the object store does not
// record, which is right for the common single-user mount and is also the only value available
// without a MountConfig. Note that a *non-nil* config is used as given: a caller that sets one field
// and leaves DefaultUID zero gets root as the fallback owner, which is why
// [CreatePlatformMountManager] fills every field it passes.
func NewFileSystem(backend types.Backend, cache types.Cache, buffer *vfs.Writer, metrics types.MetricsCollector, config *Config) *FileSystem {
	if config == nil {
		config = &Config{
			DefaultUID:     safeIntToUint32(os.Getuid()),
			DefaultGID:     safeIntToUint32(os.Getgid()),
			DefaultMode:    0644,
			DefaultDirMode: 0755,
			CacheTTL:       5 * time.Minute,
			ReadAhead:      128 * 1024,
			WriteBuffer:    64 * 1024,
			Concurrency:    16,
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

	// Initialize performance optimizations.
	//
	// There is no write coalescer. One existed and was removed: it merged writes before handing them
	// to the buffer, and its merge guarded the overlay with "is the new end past the current end", so
	// shorter new content over longer old content kept the old content — `echo NEW > f` over a file
	// holding OLD left the file reading OLD. It also discarded the buffer's write errors. The write
	// path now coalesces adjacent and overlapping ranges itself, last-writer-wins, in the one place
	// that owns the dirty state; see internal/vfs.ExtentList.Add.
	filesystem.readAhead = NewReadAheadManager(filesystem, nil)

	return filesystem
}

// Root returns the root inode
func (fs *FileSystem) Root() fs.InodeEmbedder {
	return &DirectoryNode{
		fs:   fs,
		path: "",
	}
}

// Counter helpers. Each takes stats.mu itself so call sites read as one statement rather than as a
// four-line lock/increment/unlock block; the repetition of that block is how Lookup came to increment
// Errors for an ordinary absent path.

func (fs *FileSystem) countError() {
	fs.stats.mu.Lock()
	defer fs.stats.mu.Unlock()
	fs.stats.Errors++
}

func (fs *FileSystem) countCacheHit() {
	fs.stats.mu.Lock()
	defer fs.stats.mu.Unlock()
	fs.stats.CacheHits++
}

func (fs *FileSystem) countCacheMiss() {
	fs.stats.mu.Lock()
	defer fs.stats.mu.Unlock()
	fs.stats.CacheMisses++
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

// Lookup resolves one path component.
//
// # Absence is not the same as failure
//
// v0.10.0 mapped every HeadObject error to ENOENT. A throttle, a network fault, an expired
// credential, and an AccessDenied all reported "the file is not there" — and the kernel then invited
// [DirectoryNode.Create] to make it, which PUT an empty object over a file that was merely
// temporarily unreachable. That is the single worst data-loss path in the audit, and it is one
// misclassification.
//
// So absence is distinguished by [vfs.IsNotFound] and everything else is reported as itself. A read
// that fails with EIO is a failure the caller sees; a read that fails with ENOENT is an invitation to
// overwrite.
//
// # Filling out.Attr
//
// This method must populate out.Attr itself. go-fuse's bridge has a fallback that stats the child
// through NodeGetattrer, but it is only reached when the parent does *not* implement NodeLookuper —
// and this type does. v0.10.0 returned an inode and left out.Attr zeroed, so the entry the kernel
// cached for the lifetime of EntryTimeout described a file of no type, size 0, mode 0000.
func (n *DirectoryNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	start := time.Now()
	defer func() {
		n.fs.recordLookupTime(time.Since(start))
	}()

	n.fs.stats.mu.Lock()
	n.fs.stats.Lookups++
	n.fs.stats.mu.Unlock()

	childPath := n.joinPath(name)

	if cachedInfo := n.fs.getCachedInfo(childPath); cachedInfo != nil {
		n.fs.countCacheHit()

		return n.newFileEntry(ctx, name, cachedInfo, out), 0
	}

	n.fs.countCacheMiss()

	info, err := n.fs.backend.HeadObject(ctx, childPath)
	switch {
	case err == nil:
		n.fs.cacheInfo(childPath, info)

		return n.newFileEntry(ctx, name, info, out), 0

	case !vfs.IsNotFound(err):
		// Something went wrong that is not "no such object". Report it as itself.
		n.fs.countError()
		slog.Error("lookup failed", "path", childPath, "error", err)

		return nil, toErrno(err)
	}

	// No object at that key. It may still be a directory: a prefix with objects under it is a
	// directory whether or not anything wrote a marker object for it.
	objects, listErr := n.fs.backend.ListObjects(ctx, childPath+"/", 1)
	if listErr != nil {
		// The existence question is unanswered, not answered "no". Same reasoning as above: ENOENT here
		// would invite a create over a directory that may be full of files.
		n.fs.countError()
		slog.Error("lookup: directory probe failed", "path", childPath, "error", listErr)

		return nil, toErrno(listErr)
	}
	if len(objects) == 0 {
		return nil, syscall.ENOENT
	}

	return n.newDirEntry(ctx, name, childPath, out), 0
}

// Readdir lists a directory.
//
// # No limit
//
// v0.10.0 passed 1000, with the comment "List up to 1000 objects". A truncated listing is not a
// display problem: the entries past the cap do not exist as far as any caller is concerned, so
// `rm -rf` reports success having deleted a fraction, `cp -r` copies a fraction, and `du` understates
// a dataset. The backend paginates for whatever limit it is given and a limit of zero means every
// object, so the cap is simply removed. A directory with a million objects therefore costs a thousand
// LIST requests, which is the honest cost of enumerating a million objects.
//
// # Dedup on both branches
//
// The seen set covers files as well as subdirectories. It guarded only the directory branch, on the
// reasoning that object keys are unique — but two distinct keys produce the same *entry name* here
// routinely: a marker object at "dir/" and any object under "dir/" both yield "dir", and the
// filesystem's own Mkdir writes exactly such a marker. A duplicate name in a DirStream makes readdir
// return the same entry twice, which `ls` prints twice and `rsync` treats as a protocol error.
//
// Dot entries are not emitted. go-fuse synthesizes "." and ".." in readDirMaybeLookup, and a stream
// that supplies its own gets them twice.
func (n *DirectoryNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	prefix := n.path
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	objects, err := n.fs.backend.ListObjects(ctx, prefix, 0)
	if err != nil {
		n.fs.countError()
		slog.Error("readdir failed", "path", n.path, "error", err)

		return nil, toErrno(err)
	}

	entries := make([]fuse.DirEntry, 0, len(objects))
	seen := make(map[string]bool, len(objects))

	for _, obj := range objects {
		name := strings.TrimPrefix(obj.Key, prefix)

		mode := uint32(fuse.S_IFREG)
		if before, _, isNested := strings.Cut(name, "/"); isNested {
			// Everything below the first slash belongs to a subdirectory, which is one entry here
			// however many objects it contains.
			name, mode = before, fuse.S_IFDIR
		}

		// An empty name is the directory's own marker object, whose key is the prefix itself. It is not
		// an entry in the listing — emitting it would put a nameless entry in the stream, and emitting it
		// as "." would duplicate what go-fuse already supplies.
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		entries = append(entries, fuse.DirEntry{Name: name, Mode: mode})
	}

	return fs.NewListDirStream(entries), 0
}

// Mkdir creates a directory.
//
// It writes a zero-byte marker object at "<prefix>/" so the directory exists while it is still empty.
// A prefix with no objects under it is indistinguishable from a prefix that was never created, and a
// mkdir followed by an ls that does not show the directory is not a filesystem.
//
// The requested mode is not stored. A directory's attributes are synthetic — see
// [FileSystem.dirDefaults] — so the mode reported on the next stat is the configured default whatever
// is passed here, and [DirectoryNode.Setattr] refuses a chmod for the same reason. Recording the mode
// on the marker object without reading it back would be worse than ignoring it: it would look like the
// mode was honored.
func (n *DirectoryNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.fs.config.ReadOnly {
		return nil, syscall.EROFS
	}

	childPath := n.joinPath(name) + "/"

	if err := n.fs.backend.PutObject(ctx, childPath, []byte{}, nil); err != nil {
		n.fs.countError()
		slog.Error("mkdir failed", "path", childPath, "error", err)

		return nil, toErrno(err)
	}

	// A cached negative or stale entry for this path would outlive the directory's creation.
	n.fs.invalidate(childPath)

	return n.newDirEntry(ctx, name, childPath, out), 0
}

// Create makes a new file and opens it.
//
// # It no longer PUTs
//
// v0.10.0 opened with an unconditional `PutObject(ctx, childPath, []byte{}, nil)`. Composed with a
// Lookup that reported every failure as ENOENT, that is the audit's worst data-loss path: a throttled
// or AccessDenied stat of an existing file made the kernel believe the file was absent, and the
// create that followed replaced it with nothing. The empty PUT is also unnecessary — the write path
// treats an absent object as a new empty file, so a create that writes nothing and a create that
// writes bytes both produce the right object at flush.
//
// What replaces it is an attribute record in the write path. That is what makes the file exist to the
// rest of this package before anything is written to it: [vfs.Writer.Attr] answers for it, so a stat
// between creat(2) and the first write reports the file rather than ENOENT, and the mode and
// ownership the caller asked for are persisted by the flush that Release performs.
//
// # Ownership
//
// The file is owned by the calling process, not by the mount's configured default. Ownership is
// available here — the kernel sends the caller's uid and gid with the request — and a multi-user
// mount on which every file belongs to whoever started the daemon is not a multi-user mount.
func (n *DirectoryNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (node *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	if n.fs.config.ReadOnly {
		return nil, nil, 0, syscall.EROFS
	}
	if n.fs.buffer == nil {
		return nil, nil, 0, syscall.ENOTSUP
	}

	childPath := n.joinPath(name)

	// Whatever is cached for this path describes an object this create supersedes, including a cached
	// negative entry from the lookup that preceded it.
	n.fs.invalidate(childPath)

	uid, gid := n.fs.callerOwner(ctx)
	attr := vfs.Attr{
		Type:  vfs.FileTypeRegular,
		Mode:  iofs.FileMode(mode).Perm(),
		UID:   uid,
		GID:   gid,
		Mtime: time.Now(),
	}
	if attr.Mode == 0 {
		attr.Mode = n.fs.fileDefaults().Mode
	}

	// The mask is all-true: a create sets mode, ownership, and time, unlike a chmod which sets one.
	if err := n.fs.buffer.SetAttr(ctx, childPath, true, true, true, attr); err != nil {
		n.fs.countError()
		slog.Error("create failed", "path", childPath, "error", err)

		return nil, nil, 0, toErrno(err)
	}

	n.fs.stats.mu.Lock()
	n.fs.stats.Creates++
	n.fs.stats.mu.Unlock()

	fileNode := &FileNode{fs: n.fs, path: childPath}

	node = n.NewInode(ctx, fileNode, fs.StableAttr{Mode: fuse.S_IFREG})

	fh, fuseFlags, errno = fileNode.Open(ctx, flags)
	if errno != 0 {
		return nil, nil, 0, errno
	}

	// The entry the kernel caches for the new file. NodeId and Ino are deliberately absent: the bridge
	// fills both from the inode's StableAttr in setEntryOut, and the type bits likewise, so only the
	// attributes go here.
	fillAttr(&out.Attr, attr)

	return node, fh, fuseFlags, 0
}

// Unlink reports that file removal is not implemented.
//
// This is a deliberate interim stub, not the final implementation (tracked in
// issue #163). go-fuse defaults an unimplemented NodeUnlinker to *success*, so
// without this method `rm` exits 0 and the kernel drops the inode while the S3
// object survives — the user believes the file is deleted when it is still
// present and still billing. Returning EROFS fails loudly instead, which is
// strictly safer than a silent false success.
func (n *DirectoryNode) Unlink(ctx context.Context, name string) syscall.Errno {
	slog.Warn("unlink is not implemented; refusing to report a delete that did not happen",
		"path", n.joinPath(name), "issue", 163)
	return syscall.EROFS
}

// Rmdir reports that directory removal is not implemented.
// See Unlink: go-fuse also defaults NodeRmdirer to success.
func (n *DirectoryNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	slog.Warn("rmdir is not implemented; refusing to report a delete that did not happen",
		"path", n.joinPath(name)+"/", "issue", 163)
	return syscall.EROFS
}

// FileNode is one regular file: an object in the bucket.
//
// It holds no cached ObjectInfo. One was here through v0.10.0, captured by the Lookup that created the
// inode, and Getattr answered from it — so a file reported the size and mtime it had when it was first
// looked up, for as long as the inode lived. An inode outlives any number of writes. See
// [FileNode.attr] for where the answer comes from instead.
type FileNode struct {
	fs.Inode
	fs   *FileSystem
	path string
}

// Open opens a file.
//
// O_TRUNC is not handled here. The kernel does not negotiate CAP_ATOMIC_O_TRUNC, so it issues a
// separate SETATTR carrying a size of zero before this call — see [FileNode.Setattr]. Checking the
// flag here as well would truncate twice.
func (f *FileNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	f.fs.stats.mu.Lock()
	f.fs.stats.Opens++
	f.fs.stats.mu.Unlock()

	if f.fs.config.ReadOnly && flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_CREAT|syscall.O_TRUNC) != 0 {
		return nil, 0, syscall.EROFS
	}

	openFile := &OpenFile{
		path:        f.path,
		lastAccess:  time.Now(),
		accessCount: 1,
	}

	f.fs.mu.Lock()
	handle := f.fs.nextHandle
	f.fs.nextHandle++
	f.fs.openFiles[handle] = openFile
	f.fs.mu.Unlock()

	return &FileHandle{
		fs:     f.fs,
		handle: handle,
		file:   openFile,
	}, 0, 0
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

	// Update access tracking under the per-file mutex (#105), and read the dirty flag while holding it:
	// it is written under the same lock by Write, and an unsynchronized read here was P-2, a live race
	// ten lines from its own fix.
	dirty := fh.file.touch()

	// A file with pending writes is served by the write path, which is the only component that holds
	// them. Neither the cache nor the object store has seen them yet, so asking either returns pre-write
	// bytes — the read-after-write defect. Nothing is cached from this path either: the bytes are not
	// durable, and caching them would leave the cache authoritative for a version of the object that
	// does not exist anywhere else and may yet fail to upload.
	if dirty {
		n, err := fh.fs.buffer.ReadAt(ctx, fh.file.path, dest, off)
		if err != nil {
			fh.fs.countError()
			slog.Error("read of pending writes failed", "path", fh.file.path, "offset", off, "error", err)

			return nil, toErrno(err)
		}

		fh.fs.stats.mu.Lock()
		fh.fs.stats.BytesRead += int64(n)
		fh.fs.stats.mu.Unlock()

		return fuse.ReadResultData(dest[:n]), 0
	}

	// Clamp the request to the end of the file before asking anyone for bytes.
	//
	// The kernel hands down a full buffer — 128 KiB by default — regardless of how much file is left, so
	// off+len(dest) routinely runs past EOF. That over-ask has to be trimmed here because the cache
	// cannot trim it: it is never told how long an object is, so it cannot distinguish "the object ends
	// at 10240" from "only 10240 bytes are cached", and answering a 131072-byte request with 10240 bytes
	// would be indistinguishable from a truncated file. It therefore misses, correctly, on every
	// unclamped short read — which is why every file smaller than the read buffer was uncacheable in
	// v0.10.0 no matter how many times it was read.
	//
	// The length comes from the write path rather than the handle's own size field, so that a file
	// extended or truncated through another descriptor is not clamped against this handle's stale idea
	// of where it ends.
	size, err := fh.fs.buffer.FileSize(ctx, fh.file.path)
	if err != nil {
		fh.fs.countError()
		slog.Error("read failed: cannot determine file size", "path", fh.file.path, "error", err)

		return nil, toErrno(err)
	}

	want := int64(len(dest))
	if off+want > size {
		want = size - off
	}

	if want <= 0 {
		// At or past EOF. Nothing to fetch and nothing to cache; a zero-length read is the POSIX answer.
		return fuse.ReadResultData(nil), 0
	}

	// Try cache first
	if cachedData := fh.fs.cache.Get(fh.file.path, off, want); cachedData != nil {
		fh.fs.stats.mu.Lock()
		fh.fs.stats.CacheHits++
		fh.fs.stats.BytesRead += int64(len(cachedData))
		fh.fs.stats.mu.Unlock()

		// Record the hit for Prometheus as well as internally. Only the miss below was recorded, so
		// objectfs_cache_requests_total carried misses and nothing else: a hit rate computed from it was
		// zero on a perfectly-served workload, and the SDKs derive hit_rate from exactly these two
		// counters — with hits absent they could not derive it at all.
		if fh.fs.metrics != nil {
			fh.fs.metrics.RecordCacheHit(fh.file.path, int64(len(cachedData)))
		}

		// A hit is still a read, and the read-ahead detector has to see it. Recording only misses made
		// the prefetcher defeat itself: a successful prefetch hid the next read from the detector, whose
		// contiguity check then compared the read after it against the offset of the read before, found
		// a gap, and reset the pattern to zero. Sequential-hit counts cycled 0→6→prefetch→0 forever, so
		// exactly one prefetch landed per seven reads and a long sequential traversal never reached
		// steady state. Measured on a 3 MiB file read at 128 KiB: 3 of 24 reads served from cache.
		fh.fs.readAhead.OnRead(fh.file.path, off, int64(len(cachedData)))

		return fuse.ReadResultData(cachedData), 0
	}

	// Read from backend
	data, err := fh.fs.backend.GetObject(ctx, fh.file.path, off, want)
	if err != nil {
		fh.fs.stats.mu.Lock()
		fh.fs.stats.Errors++
		fh.fs.stats.CacheMisses++
		fh.fs.stats.mu.Unlock()

		slog.Error("read failed", "path", fh.file.path, "offset", off, "error", err)

		return nil, toErrno(err)
	}

	fh.fs.stats.mu.Lock()
	fh.fs.stats.CacheMisses++
	fh.fs.stats.BytesRead += int64(len(data))
	fh.fs.stats.mu.Unlock()

	// Hand the whole read to the cache and let it choose its own entry granularity.
	//
	// This used to split reads larger than 16 MB into per-chunk Puts itself. That loop never ran — the
	// kernel's largest read is MaxRead, two orders of magnitude below the threshold — and splitting here
	// would be the wrong layer anyway: the cache already stores at a fixed chunk size and coalesces
	// adjacent runs, so a caller that pre-splits only guesses at a boundary the cache is free to change.
	fh.fs.cache.Put(fh.file.path, off, data)

	// Record metrics
	if fh.fs.metrics != nil {
		fh.fs.metrics.RecordCacheMiss(fh.file.path, int64(len(data)))
	}

	// Trigger read-ahead analysis. OnRead tolerates a nil manager.
	fh.fs.readAhead.OnRead(fh.file.path, off, int64(len(data)))

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

	// Buffer the write as a dirty byte range. Nothing is uploaded here: an object store has no way to
	// modify part of an object, so the write is recorded and the read-modify-write happens at flush.
	if err := fh.fs.buffer.Write(fh.file.path, off, data); err != nil {
		fh.fs.countError()
		slog.Error("write failed", "path", fh.file.path, "offset", off, "error", err)

		return 0, toErrno(err)
	}

	// Mark the descriptor dirty so subsequent reads on it come from the write path. Only after the write
	// was accepted: a descriptor marked dirty by a write that failed would send reads to a write path
	// that does not hold the bytes.
	fh.file.markDirty()

	return safeIntToUint32(len(data)), 0
}

// Flush flushes any pending writes for the file.
//
// It asks the write path unconditionally rather than checking fh.file.dirty first. The write path
// owns the dirty state and answers cheaply for a clean file — an unbuffered key is a no-op and a
// buffered-but-clean one takes the flush plan's Noop arm without uploading. Gating on the handle's
// own bool would mean two sources of truth for "does this need writing", and the handle's is the one
// that cannot see a truncation, a chmod, or a write made through a different descriptor on the same
// path. A missed flush is data loss; a redundant one costs a map lookup.
// It takes the request's context rather than the write path's own, so a flush is canceled when the
// kernel interrupts the syscall that triggered it instead of running to completion against a caller
// that has gone away.
func (fh *FileHandle) Flush(ctx context.Context) syscall.Errno {
	if err := fh.fs.buffer.FlushContext(ctx, fh.file.path); err != nil {
		fh.fs.countError()
		slog.Error("flush failed", "path", fh.file.path, "error", err)

		return toErrno(err)
	}

	// Drop what the cache holds for this path now that the object has changed underneath it. Ordered
	// after the flush, not before: invalidating first would leave a window in which a concurrent read
	// re-populates the cache from the old object and the flush then makes that entry stale again.
	fh.fs.invalidate(fh.file.path)

	fh.file.markClean()

	return 0
}

// Release releases the file handle, flushing anything still pending.
//
// The flush error is returned rather than discarded. Release is the last chance to report that a
// file's contents never reached storage, and the kernel surfaces the errno to close(2) — which is
// where POSIX says a program should look for exactly this failure. v0.10.0 wrote `_ = fh.Flush(ctx)`
// here, so an AccessDenied on the final upload was invisible to the process that wrote the data.
//
// The handle is removed from the open-files map either way: a failed flush must not also leak the
// handle, and the buffered data is still held by the write path, which reports it again at unmount.
func (fh *FileHandle) Release(ctx context.Context) syscall.Errno {
	errno := fh.Flush(ctx)

	fh.fs.mu.Lock()
	delete(fh.fs.openFiles, fh.handle)
	fh.fs.mu.Unlock()

	return errno
}

// Helper methods for DirectoryNode

func (n *DirectoryNode) joinPath(name string) string {
	if n.path == "" {
		return name
	}
	return filepath.Join(n.path, name)
}

// newFileEntry builds the inode and the kernel entry for a regular file found under this directory.
//
// The attributes come from the object's own metadata with the mount's defaults filling in what it does
// not record, so a file written by another tool reports the mounting user rather than root. out.NodeId
// and out.Ino are left alone: the bridge fills both from the inode's StableAttr, and so are the type
// bits, which is why fs.StableAttr carries only S_IFREG — a StableAttr.Mode with permission bits in it
// makes go-fuse panic in addNewChild.
func (n *DirectoryNode) newFileEntry(
	ctx context.Context, name string, info *types.ObjectInfo, out *fuse.EntryOut,
) *fs.Inode {
	childPath := n.joinPath(name)

	attr := vfs.AttrFromMetadataWithDefaults(
		info.Metadata, info.Size, info.LastModified, info.ETag, n.fs.fileDefaults())

	// Pending writes win over the object's metadata, for the size above all: an entry reporting the
	// pre-write length clamps every read of a file being appended to.
	if n.fs.buffer != nil {
		if pending, ok := n.fs.buffer.Attr(childPath); ok {
			attr = pending
		}
	}

	if out != nil {
		fillAttr(&out.Attr, attr)
	}

	return n.NewInode(ctx, &FileNode{fs: n.fs, path: childPath}, fs.StableAttr{Mode: fuse.S_IFREG})
}

// newDirEntry builds the inode and the kernel entry for a subdirectory.
//
// out.Attr.Mode keeps its permission bits and the S_IFDIR the bridge requires. Mkdir asserts on
// exactly this — go-fuse panics with "mode must be S_IFDIR" if the mode carries any other type bit —
// so [fillAttr] supplying it from [vfs.Attr.IsDir] is what makes the same helper serve both node
// kinds.
func (n *DirectoryNode) newDirEntry(ctx context.Context, name, path string, out *fuse.EntryOut) *fs.Inode {
	if out != nil {
		fillAttr(&out.Attr, n.fs.dirDefaults())
	}

	return n.NewInode(ctx, &DirectoryNode{fs: n.fs, path: path}, fs.StableAttr{Mode: fuse.S_IFDIR})
}

// Helper methods for FileSystem

// metaCacheKey is the cache key under which a path's marshaled ObjectInfo is held.
//
// The "__meta__" prefix shares a keyspace with object content, which is safe only because no S3 object
// key can begin with it and also name a real object this filesystem serves — the mount's own paths never
// carry it. Invalidation depends on that: invalidateMetadata deletes this key, and Delete is exact, so
// flushing a path's attributes cannot flush its content or vice versa.
func metaCacheKey(path string) string {
	return "__meta__" + path
}

// statObject returns the stored metadata for a path, from the metadata cache when it is there.
//
// It goes through the cache because Getattr is the most frequent operation a filesystem serves — `ls
// -l` of a directory is one per entry, and the kernel re-stats on a schedule of its own — and an
// uncached stat is one S3 HEAD.
func (fs *FileSystem) statObject(ctx context.Context, path string) (*types.ObjectInfo, error) {
	if info := fs.getCachedInfo(path); info != nil {
		fs.countCacheHit()

		return info, nil
	}

	fs.countCacheMiss()

	info, err := fs.backend.HeadObject(ctx, path)
	if err != nil {
		return nil, err
	}

	fs.cacheInfo(path, info)

	return info, nil
}

func (fs *FileSystem) getCachedInfo(path string) *types.ObjectInfo {
	if fs.cache == nil {
		return nil
	}

	// A size of zero asks for whatever contiguous bytes are held from offset 0, which is the one shape
	// that fits a caller storing a whole value of a length only the writer knows. Asking for a fixed
	// 8192 instead — as v0.10.0 did — could never hit: a ~138-byte ObjectInfo is all that was ever
	// stored, and the cache correctly refuses to answer a 8192-byte request with 138 bytes, since it
	// cannot tell a short value from a partially-cached one. The result was one S3 HEAD per path
	// component on every stat, forever, with the metadata cache reporting a 0% hit rate.
	cachedData := fs.cache.Get(metaCacheKey(path), 0, 0)
	if cachedData == nil {
		return nil
	}

	var info types.ObjectInfo
	if err := json.Unmarshal(cachedData, &info); err != nil {
		return nil
	}

	return &info
}

func (fs *FileSystem) cacheInfo(path string, info *types.ObjectInfo) {
	if fs.cache == nil || info == nil {
		return
	}

	data, err := json.Marshal(info)
	if err != nil {
		return
	}

	fs.cache.Put(metaCacheKey(path), 0, data)
}

// invalidate drops everything cached for a path: its content bytes and its attributes.
//
// Every mutation must call this. Nothing in the cache observes writes, so a modified path keeps serving
// its pre-write bytes and its pre-write size until the TTL expires — up to five minutes on the default
// config. v0.10.0 had no call to cache.Delete anywhere in this package, which is why writing to a file
// and reading it back on the same descriptor returned the old contents.
//
// Content and metadata are separate keys, and both go: a write changes the bytes, and it also changes
// the size and mtime that Lookup reports. Dropping only one leaves the two disagreeing, which surfaces
// as a file whose stat size does not match what read returns.
func (fs *FileSystem) invalidate(path string) {
	if fs.cache == nil {
		return
	}

	fs.cache.Delete(path)
	fs.cache.Delete(metaCacheKey(path))
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
