//go:build linux || darwin

package fuse

import (
	"context"
	"encoding/json"
	iofs "io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scttfrdmn/objectfs/internal/vfs"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// safeInt64ToUint64 clamps a negative int64 to zero so the conversion cannot wrap.
//
// Only the sign needs checking: int64 and uint64 are 64 bits wide on every platform Go targets, so
// the only values int64 can hold that uint64 cannot are the negative ones. There is no upper bound
// to test — unlike safeIntToUint32 below, where the widths genuinely differ.
func safeInt64ToUint64(i int64) uint64 {
	if i < 0 {
		return 0
	}

	return uint64(i)
}

// safeIntToUint32 clamps an int into uint32's range.
//
// The upper bound is compared as uint64 rather than as int, because `int` is 32 bits on 32-bit
// platforms and the guard's own limit does not fit in it. Written as `i > 0xFFFFFFFF` this function
// did not compile for any 32-bit target — the overflow guard overflowed:
//
//	$ CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build ./internal/fuse/
//	internal/fuse/filesystem.go:37:9: 0xFFFFFFFF (untyped int constant 4294967295) overflows int
//
// That single error is why linux/armv7 was dropped from the release matrix in v0.10.1 rather than
// fixed (#198). The negative check has to come first: uint64(-1) is 2^64-1, which would clamp to
// MaxUint32 instead of to zero.
//
// On a 32-bit platform the upper branch is unreachable — math.MaxInt is 2^31-1 there, below
// MaxUint32 — but it must still compile, which is the whole point.
func safeIntToUint32(i int) uint32 {
	if i < 0 {
		return 0
	}

	if uint64(i) > math.MaxUint32 {
		return math.MaxUint32
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

	// coordinator is the cluster's cache coordination, and is **nil on a single-node mount** — which
	// is every mount that does not set `cluster.enabled`. Nil is the whole of the safety argument: no
	// path here branches on it except to skip, so a mount without clustering does exactly what it did
	// before this field existed, at no cost.
	//
	// It is deliberately [types.DistributedCoordinator] and not *distributed.ClusterManager. This
	// package must not import internal/distributed: the dependency would run FUSE → distributed →
	// gossip → consensus, and the interface is four methods about keys and ETags that a warming
	// implementation could satisfy without a gossip socket at all.
	//
	// What reaches it is the gossip and cache half of clustering only. Consensus is not started on a
	// mount — see [distributed.ClusterConfig.EnableConsensus] — so nothing on the read or write path
	// waits on an election.
	coordinator types.DistributedCoordinator

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

	// fetches collapses a GET into one already in flight that covers it.
	//
	// The prefetcher and the reader race for the same bytes. A prefetch is issued for the offset the
	// reader is predicted to want next, so whether the reader finds those bytes in the cache is a race
	// between an S3 round trip and the application's next read(2) — and losing it means both fetch the
	// same bytes: the prefetch stops being an optimization and becomes a second copy of every read.
	//
	// This was measured, not theorized. A sequential traversal of a 3 MiB file at 128 KiB reads issues
	// 24 reads and 17 prefetches; on an unloaded machine nearly every prefetch lands first and the
	// traversal transfers 3,145,728 bytes, but under CI's load the reads win and it transfers
	// 5,373,952 — exactly 41 × 131072, every prefetch paid for twice.
	//
	// # Why containment and not equality
	//
	// This was a singleflight.Group keyed on (path, offset, length), on the stated grounds that the
	// two requests are byte-identical "by construction". They are not. prefetchLength floors the
	// prefetch at ReadAheadConfig.WindowSize — 64 KiB by default — so a reader whose reads are
	// *smaller* than the window gets a prefetch that is a strict superset of its next read, and two
	// overlapping-but-unequal ranges hash to different keys and collapse into nothing.
	//
	// Every earlier measurement used 128 KiB reads against the 64 KiB window, where the floor never
	// engages and the ranges really are equal, so the case went unexercised until a 16 KiB file read in
	// 1 KiB steps transferred 27,648 bytes — 1.7×, entirely duplicate:
	//
	//	GET bytes=6144-7167    reader, 1 KiB
	//	GET bytes=6144-16383   prefetch, the whole tail — same start, superset, no collapse
	//	GET bytes=7168-16383   prefetch again, nested inside the one above
	//	GET bytes=7168-8191    reader, inside both
	//
	// Containment answers all four from one transfer, and it subsumes equality, since a range contains
	// itself. Partial overlap that is not containment still issues its own GET: deduplicating it would
	// mean splicing two in-flight results, and an unaligned reader is not the pattern this exists for.
	//
	// A follower whose leader fails issues its own request rather than inheriting the error. Prefetches
	// carry a 5-second timeout and reads do not, so an inherited failure would let a prefetch's
	// deadline fail a read that had no deadline of its own.
	fetches inflightFetches
}

// inflightFetches tracks GETs in flight so an overlapping one can wait instead of duplicating it.
type inflightFetches struct {
	mu sync.Mutex

	// byPath is keyed by object key, because containment is only meaningful within one object and a
	// flat list would be scanned across every concurrently-read file.
	byPath map[string][]*inflightFetch
}

// inflightFetch is one GET in flight, and its result once done is closed.
type inflightFetch struct {
	off    int64
	length int64

	done chan struct{}
	data []byte
	err  error
}

// covers reports whether this fetch's range contains [off, off+length) entirely.
func (f *inflightFetch) covers(off, length int64) bool {
	return off >= f.off && off+length <= f.off+f.length
}

// slice returns the sub-range of a completed fetch's data, and whether it is actually present.
//
// The presence check is not defensive padding: a GET at EOF returns fewer bytes than its range asked
// for, so a leader whose read was short does not in fact hold every byte its range claimed. A follower
// in that position has to issue its own request — which will also come up short, and correctly so —
// rather than be handed a truncated slice as though it were the whole answer.
func (f *inflightFetch) slice(off, length int64) ([]byte, bool) {
	start := off - f.off
	if start < 0 || start+length > int64(len(f.data)) {
		return nil, false
	}

	return f.data[start : start+length], true
}

// Config represents FUSE filesystem configuration.
//
// Every field here is read on the mount path. That is a property worth stating, because it was not
// true: this struct carried AllowOther, DirectIO, KeepCache, BigWrites, MaxRead, MaxWrite, ReadAhead,
// WriteBuffer, and Concurrency, and not one of them was read by anything. Each carried a yaml tag,
// which is what made them look plumbed — but no decoder targets this type. Configuration is decoded
// into [config.Configuration], which as of v0.11.0 does have a `fuse` section — and it reaches this
// struct through internal/adapter and [CreatePlatformMountManager], not by being decoded into it.
//
// DirectIO and KeepCache below are two of the four #180 nominated, restored with the plumbing and the
// tests they lacked the first time. They are on this type rather than only on [MountOptions] because
// [FileNode.Open] is what returns them to the kernel, and Open reads this config.
//
// See [MountOptions] for the mount-time options and for why splice is not among them.
type Config struct {
	// Mount options
	MountPoint string `yaml:"mount_point"`
	ReadOnly   bool   `yaml:"read_only"`

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

	// DirectIO returns FOPEN_DIRECT_IO from every [FileNode.Open], so the kernel does not cache this
	// mount's file data and every read(2) becomes a READ reaching [FileHandle.Read].
	//
	// KeepCache returns FOPEN_KEEP_CACHE, asking the kernel to keep cached pages across open(2).
	//
	// DirectIO wins when both are set. The two ask for opposite things, and sending both leaves the
	// decision to a kernel version rather than to this configuration; go-fuse's bridge makes the same
	// choice in the passthrough case, clearing FOPEN_KEEP_CACHE when it sets FOPEN_PASSTHROUGH
	// (fs/bridge.go:805).
	//
	// No yaml tags for the reason given at ReadAhead below. The operator-facing names are
	// config.FUSEConfig's `direct_io` and `keep_cache`.
	DirectIO  bool
	KeepCache bool

	// ReadAhead configures the sequential-read prefetcher; nil takes [DefaultReadAheadConfig].
	//
	// No yaml tag, unlike its neighbors, because it would be the fourth name for one setting: the
	// operator-facing names are config.ReadAheadConfig's, and nothing decodes YAML into this type
	// anyway. The tags above are kept only because they predate that discovery.
	ReadAhead *ReadAheadConfig

	// Coordinator is the cluster's cache coordination, or nil for a single-node mount. See
	// [FileSystem.coordinator], which it is copied to, and [MountConfig.Coordinator], which it comes
	// from. No yaml tag: it is not a value a config file can express.
	Coordinator types.DistributedCoordinator
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
	Renames int64 `json:"renames"`

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
//
// ctx is the mount's lifetime, not this call's. It is the parent of the read-ahead manager's prefetch
// GETs and the signal that stops its goroutines; see [NewReadAheadManager] for why the manager needed
// a second stop signal alongside its own Stop.
func NewFileSystem(ctx context.Context, backend types.Backend, cache types.Cache, buffer *vfs.Writer, metrics types.MetricsCollector, config *Config) *FileSystem {
	if config == nil {
		config = &Config{
			DefaultUID:     safeIntToUint32(os.Getuid()),
			DefaultGID:     safeIntToUint32(os.Getgid()),
			DefaultMode:    0644,
			DefaultDirMode: 0755,
			CacheTTL:       5 * time.Minute,
		}
	}

	filesystem := &FileSystem{
		backend:     backend,
		cache:       cache,
		buffer:      buffer,
		metrics:     metrics,
		config:      config,
		coordinator: config.Coordinator,
		openFiles:   make(map[uint64]*OpenFile),
		nextHandle:  1,
		stats:       &Stats{},
	}

	// Initialize performance optimizations.
	//
	// There is no write coalescer. One existed and was removed: it merged writes before handing them
	// to the buffer, and its merge guarded the overlay with "is the new end past the current end", so
	// shorter new content over longer old content kept the old content — `echo NEW > f` over a file
	// holding OLD left the file reading OLD. It also discarded the buffer's write errors. The write
	// path now coalesces adjacent and overlapping ranges itself, last-writer-wins, in the one place
	// that owns the dirty state; see internal/vfs.ExtentList.Add.
	//
	// config.ReadAhead, not nil. This argument was nil, which meant performance.read_ahead was decoded,
	// validated, documented in five shipped preset files, and read by nothing (#176).
	filesystem.readAhead = NewReadAheadManager(ctx, filesystem, config.ReadAhead)

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
// GetStats returns a snapshot of the counters.
//
// Every field of [Stats] is copied, and that is the whole reason this comment exists: the field list
// named nine of fifteen. Creates, Deletes, and Renames were incremented by their operations and
// reported as zero, and the three latency averages were computed by [FileSystem.recordReadTime] and its
// siblings — an exponential moving average per operation — and then dropped on the way out. The
// counters existed, the operations maintained them, and `objectfs stats` said nothing had happened.
//
// A field added to Stats and not added here fails in exactly that way: not a compile error, not a test
// failure, just a number that is always zero. TestGetStatsReportsEveryCounter is what catches it, by
// reflection rather than by a second list that could go stale the same way.
func (fs *FileSystem) GetStats() *Stats {
	fs.stats.mu.RLock()
	defer fs.stats.mu.RUnlock()

	return &Stats{
		Lookups:      fs.stats.Lookups,
		Opens:        fs.stats.Opens,
		Reads:        fs.stats.Reads,
		Writes:       fs.stats.Writes,
		Creates:      fs.stats.Creates,
		Deletes:      fs.stats.Deletes,
		Renames:      fs.stats.Renames,
		BytesRead:    fs.stats.BytesRead,
		BytesWritten: fs.stats.BytesWritten,
		CacheHits:    fs.stats.CacheHits,
		CacheMisses:  fs.stats.CacheMisses,
		Errors:       fs.stats.Errors,

		AvgReadTime:   fs.stats.AvgReadTime,
		AvgWriteTime:  fs.stats.AvgWriteTime,
		AvgLookupTime: fs.stats.AvgLookupTime,
	}
}

// DirectoryNode represents a directory in the filesystem
type DirectoryNode struct {
	fs.Inode
	fs *FileSystem

	// path is the key prefix this directory stands for, and it is mutable — see [DirectoryNode.key].
	pathMu sync.RWMutex
	path   string
}

// key returns the object key prefix this node currently stands for.
//
// # Why this is not a plain field read
//
// A rename moves the node rather than replacing it. go-fuse's bridge, on a Rename that returns success,
// calls Inode.MvChild (fs/inode.go), which re-parents *the same* *Inode — so the DirectoryNode or
// FileNode it carries survives the rename with whatever path it was constructed with. Every operation
// afterwards on the moved dentry would address the key the file was moved *away* from: `mv a b` followed
// by a write to b would flush to a, recreating the source and leaving b as the user found it.
//
// So the path has to be updated in place, which makes it mutable, which makes it shared state — the
// kernel issues concurrent operations against the same node, so a rename storing a new path while a read
// takes the old one is a data race in the literal -race sense. Hence the lock rather than a bare
// assignment.
func (n *DirectoryNode) key() string {
	n.pathMu.RLock()
	defer n.pathMu.RUnlock()

	return n.path
}

// setKey repoints this node at a new prefix. See [DirectoryNode.key] for why it exists.
func (n *DirectoryNode) setKey(path string) {
	n.pathMu.Lock()
	defer n.pathMu.Unlock()
	n.path = path
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
	// Read once. Taking it twice would let a concurrent rename list one prefix and report another.
	dirPath := n.key()

	prefix := dirPath
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	objects, err := n.fs.backend.ListObjects(ctx, prefix, 0)
	if err != nil {
		n.fs.countError()
		slog.Error("readdir failed", "path", dirPath, "error", err)

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

// Unlink removes a file: it deletes the object and discards anything the write path still holds for
// that path.
//
// # Why the write path is dropped first
//
// A file being deleted may have dirty ranges buffered — `echo x > f; rm f` is an ordinary sequence, and
// the kernel does not guarantee a flush before the unlink. Deleting the object while those ranges
// survive means the next flush, or the unmount, PUTs them back: the file returns from the dead, at the
// size it had when it was written, with no error anywhere.
//
// Discarding is also why this cannot simply call the backend. A delete that bypassed the write path
// would be correct exactly until someone wrote to a file before removing it.
//
// The *ordering* — discard before delete rather than after — is a narrower claim than it looks, and worth
// stating precisely because a test cannot currently hold it. Both orders discard, so on this sequential
// path both are correct; what the order buys is the concurrent case, where a background flush landing
// between the two steps would PUT the object back after the delete had removed it. Moving the discard
// after the delete does not fail any test in delete_test.go — verified by mutation, not assumed. The
// order is kept because it is free and closes that window; it is not load-bearing for the assertions
// below.
//
// # Why existence is established here and not left to the backend
//
// A missing file must be ENOENT: POSIX requires it, `rm` distinguishes the two, and treating absence
// as success is the shape of the defect this method replaces — v0.10.0 had no Unlink at all, and
// go-fuse defaults an unimplemented NodeUnlinker to *success*, so `rm` exited 0, the kernel dropped
// the inode, and the object stayed in the bucket billing.
//
// The backend cannot supply that answer. [types.Backend.DeleteObject] returns nil for a key that is not
// there, deliberately — that is S3's contract and the Go SDK's documented behavior — so relying on its
// error to detect absence would report success for every `rm` of a file that never existed.
//
// So the check is explicit, and it asks two places. A file can exist in the bucket, or it can exist
// only as dirty ranges in the write path: [DirectoryNode.Create] records attributes through the write
// path and PUTs nothing, so a just-created file is real, is visible to stat, and has no object behind
// it yet. Consulting only the backend would make `touch f && rm f` fail with ENOENT on a file the user
// can see.
func (n *DirectoryNode) Unlink(ctx context.Context, name string) syscall.Errno {
	if n.fs.config.ReadOnly {
		return syscall.EROFS
	}

	childPath := n.joinPath(name)

	// The write path first, because it is authoritative for a file that has not been flushed and needs
	// no round trip. Only if it holds nothing is the bucket asked.
	if !n.fs.buffer.Dirty(childPath) {
		if _, err := n.fs.backend.HeadObject(ctx, childPath); err != nil {
			if vfs.IsNotFound(err) {
				return syscall.ENOENT
			}

			// Not absence — a throttle, a permission failure, a network fault. Reporting ENOENT here is
			// what let v0.10.0's Lookup invite an overwrite of an intact object; the honest answer is that
			// we do not know, so the delete does not proceed.
			n.fs.countError()
			slog.Error("unlink could not determine whether the file exists", "path", childPath, "error", err)

			return toErrno(err)
		}
	}

	// Before the delete, not after. See the doc comment: a surviving dirty range outlives the object and
	// resurrects it on the next flush.
	if err := n.fs.buffer.Discard(childPath); err != nil {
		n.fs.countError()
		slog.Error("unlink could not discard buffered writes", "path", childPath, "error", err)

		return toErrno(err)
	}

	if err := n.fs.backend.DeleteObject(ctx, childPath); err != nil {
		n.fs.countError()
		slog.Error("unlink failed", "path", childPath, "error", err)

		return toErrno(err)
	}

	n.fs.invalidate(childPath)

	n.fs.stats.mu.Lock()
	n.fs.stats.Deletes++
	n.fs.stats.mu.Unlock()

	return 0
}

// Rmdir removes an empty directory.
//
// # Emptiness is checked, and the check is the whole operation
//
// S3 has no directories. A directory here is a zero-byte marker object at "<path>/" plus whatever
// shares that prefix, so "remove the directory" is "remove the marker" — and doing only that would
// leave every object under the prefix present but unreachable through a `ls` that no longer lists the
// parent. Worse, it would report success: rmdir on a non-empty directory must fail with ENOTEMPTY, and
// a filesystem that silently orphans data instead is the failure mode this whole audit was about.
//
// So the listing runs first, and it asks for two entries rather than one. The marker object's own key
// is the prefix, so it appears in its own listing; one entry means "just the marker", and two means
// there is something else under it. Asking for a single entry could not tell those apart.
func (n *DirectoryNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	if n.fs.config.ReadOnly {
		return syscall.EROFS
	}

	childPath := n.joinPath(name) + "/"

	// Two, for the reason above: the marker is in its own listing, so the count that distinguishes empty
	// from non-empty is 1 vs more.
	objects, err := n.fs.backend.ListObjects(ctx, childPath, 2)
	if err != nil {
		n.fs.countError()
		slog.Error("rmdir could not check whether the directory is empty", "path", childPath, "error", err)

		return toErrno(err)
	}

	for _, obj := range objects {
		if obj.Key != childPath {
			// Something other than the marker lives under this prefix. Refusing is required by POSIX and
			// is also the only safe answer: the alternative leaves those objects in the bucket with no
			// path that reaches them.
			return syscall.ENOTEMPTY
		}
	}

	// A directory with no marker object at all is one that only ever existed implicitly, as the shared
	// prefix of objects that have since gone. There is nothing to delete, and reporting ENOENT is
	// accurate — rmdir of a directory that is not there is an error.
	if len(objects) == 0 {
		return syscall.ENOENT
	}

	if err := n.fs.backend.DeleteObject(ctx, childPath); err != nil {
		n.fs.countError()
		slog.Error("rmdir failed", "path", childPath, "error", err)

		return toErrno(err)
	}

	n.fs.invalidate(childPath)

	n.fs.stats.mu.Lock()
	n.fs.stats.Deletes++
	n.fs.stats.mu.Unlock()

	return 0
}

// FileNode is one regular file: an object in the bucket.
//
// It holds no cached ObjectInfo. One was here through v0.10.0, captured by the Lookup that created the
// inode, and Getattr answered from it — so a file reported the size and mtime it had when it was first
// looked up, for as long as the inode lived. An inode outlives any number of writes. See
// [FileNode.attr] for where the answer comes from instead.
type FileNode struct {
	fs.Inode
	fs *FileSystem

	// path is the object key this file stands for, mutable for the reason [DirectoryNode.key] gives.
	pathMu sync.RWMutex
	path   string
}

// key returns the object key this node currently stands for. See [DirectoryNode.key].
func (f *FileNode) key() string {
	f.pathMu.RLock()
	defer f.pathMu.RUnlock()

	return f.path
}

// setKey repoints this node at a new key. See [DirectoryNode.key].
func (f *FileNode) setKey(path string) {
	f.pathMu.Lock()
	defer f.pathMu.Unlock()
	f.path = path
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
		path:        f.key(),
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
	}, f.fs.openFlags(), 0
}

// openFlags is the FOPEN_* set every open on this mount returns to the kernel.
//
// This return value was the literal 0 through v0.10.0 while [Config] carried DirectIO and KeepCache
// fields — the two flags that belong here — so both were settable and neither reached the kernel.
// #180 removed them for that reason; this is them coming back with the one line that makes them mean
// something.
//
// DirectIO takes precedence over KeepCache rather than the two being OR'd. FOPEN_DIRECT_IO tells the
// kernel not to use the page cache for this file and FOPEN_KEEP_CACHE tells it to hold what the page
// cache already has; sending both asks for opposite things and leaves which one applies to a kernel
// version. See [Config.DirectIO].
func (fs *FileSystem) openFlags() uint32 {
	if fs.config.DirectIO {
		return fuse.FOPEN_DIRECT_IO
	}

	if fs.config.KeepCache {
		return fuse.FOPEN_KEEP_CACHE
	}

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

	// Read from the backend, sharing the request with any identical one already in flight — which is
	// routinely the prefetcher's, since it predicts exactly this range at exactly this length.
	data, err := fh.fs.fetch(ctx, fh.file.path, off, want)
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

	// Record metrics
	if fh.fs.metrics != nil {
		fh.fs.metrics.RecordCacheMiss(fh.file.path, int64(len(data)))
	}

	// Trigger read-ahead analysis. OnRead tolerates a nil manager.
	fh.fs.readAhead.OnRead(fh.file.path, off, int64(len(data)))

	return fuse.ReadResultData(data), 0
}

// fetch returns [off, off+length) of an object, from the backend, and caches what it read.
//
// A caller whose range is already covered by a GET in flight waits for that GET and takes its slice,
// rather than issuing a duplicate. There are two callers — the reader and the prefetcher — and they
// contend for the same bytes by design: a prefetch is issued for the offset the reader is predicted to
// want next, so which of them reaches S3 first is a race between a network round trip and the
// application's next read(2). Win it and the prefetch was useful; lose it and, without this, the same
// bytes are fetched and billed twice. Waiting makes total bytes transferred a function of the read
// pattern rather than of how loaded the machine is, which is what makes it assertable in a test.
//
// Containment rather than equality, because the two callers do not ask for equal ranges: see the
// inflightFetches field on FileSystem for the measurement that established that, and for why the
// stronger claim held right up until a reader read in steps smaller than the prefetch window.
//
// The returned slice aliases the leader's buffer and must not be modified. Read hands it to
// fuse.ReadResultData, which copies into the kernel's buffer, and performPrefetch only measures its
// length.
func (fs *FileSystem) fetch(ctx context.Context, path string, off, length int64) ([]byte, error) {
	// Serve from an in-flight GET that covers this range, if there is one.
	if leader := fs.fetches.join(path, off, length); leader != nil {
		<-leader.done

		if leader.err == nil {
			if data, ok := leader.slice(off, length); ok {
				return data, nil
			}
		}
		// Fall through: the leader failed, or came up short of the range it asked for. Either way this
		// caller issues its own request rather than inheriting a result that is not its answer.
	}

	self, leader := fs.fetches.start(path, off, length)
	if leader != nil {
		// A covering GET started between the join above and here.
		<-leader.done

		if leader.err == nil {
			if data, ok := leader.slice(off, length); ok {
				fs.fetches.finish(path, self, nil, nil)

				return data, nil
			}
		}
	}

	data, err := fs.fetchUncached(ctx, path, off, length)

	// Publish before returning, so a waiter is released whether this succeeded or not.
	fs.fetches.finish(path, self, data, err)

	if err != nil {
		return nil, err
	}

	return data, nil
}

// fetchUncached does the actual GET, checking the cache once more first.
func (fs *FileSystem) fetchUncached(ctx context.Context, path string, off, length int64) ([]byte, error) {
	// A caller that missed the cache, then blocked on an overlapping flight, may find the bytes already
	// stored by the time it runs — and a GET issued for bytes now in cache is the exact waste this
	// function exists to remove.
	if cached := fs.cache.Get(path, off, length); cached != nil {
		return cached, nil
	}

	data, err := fs.backend.GetObject(ctx, path, off, length)
	if err != nil {
		return nil, err
	}

	// Hand the whole read to the cache and let it choose its own entry granularity.
	//
	// The read path used to split reads larger than 16 MB into per-chunk Puts itself. That loop never
	// ran — the kernel's largest read is MaxRead, two orders of magnitude below the threshold — and
	// splitting here would be the wrong layer anyway: the cache already stores at a fixed chunk size and
	// coalesces adjacent runs, so a caller that pre-splits only guesses at a boundary the cache is free
	// to change.
	fs.cache.Put(path, off, data)

	return data, nil
}

// join returns an in-flight fetch covering [off, off+length), or nil if there is none.
func (i *inflightFetches) join(path string, off, length int64) *inflightFetch {
	i.mu.Lock()
	defer i.mu.Unlock()

	return i.coveringLocked(path, off, length)
}

// unclaimedStart returns the first offset at or after off that no in-flight fetch is already reading.
//
// This answers a different question from join's, for a caller that may adjust its range rather than
// waiting: not "can another request answer mine" but "where do the bytes nobody is fetching begin".
// Only [ReadAheadManager.performPrefetch] asks, because only a prefetch is free to move.
//
// It advances repeatedly rather than once, since the fetch it skips past may itself land inside a
// third — a reader one step ahead of the prefetcher produces exactly that chain.
//
// Overlap that begins *after* off is deliberately not considered. Truncating there would cut the
// read-ahead short at a single small read the reader happens to have outstanding in the middle of the
// window, giving up the whole tail to avoid duplicating a kilobyte; splitting the range into the gaps
// around it would mean issuing several GETs where the point of read-ahead is to issue one. The front
// is where the duplication actually occurs, because a prefetch is predicted from the read that is
// still in flight.
func (i *inflightFetches) unclaimedStart(path string, off int64) int64 {
	i.mu.Lock()
	defer i.mu.Unlock()

	flights := i.byPath[path]

	// Bounded by the number of in-flight fetches: each pass either advances past one of them or stops,
	// and a fetch cannot be passed twice because off only ever increases.
	for range flights {
		advanced := false

		for _, f := range flights {
			if f.off <= off && off < f.off+f.length {
				off = f.off + f.length
				advanced = true

				break
			}
		}

		if !advanced {
			break
		}
	}

	return off
}

// start registers a fetch for this range and returns it, unless a covering one appeared first — in
// which case the caller waits on that one instead and self is still returned so it can be retired.
func (i *inflightFetches) start(path string, off, length int64) (self, leader *inflightFetch) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Look before registering. A range contains itself, so a self that were already in the list would
	// be its own covering match and would then wait forever on its own done channel.
	leader = i.coveringLocked(path, off, length)

	self = &inflightFetch{off: off, length: length, done: make(chan struct{})}

	if i.byPath == nil {
		i.byPath = make(map[string][]*inflightFetch)
	}
	i.byPath[path] = append(i.byPath[path], self)

	return self, leader
}

// coveringLocked finds an in-flight fetch containing this range. i.mu must be held.
func (i *inflightFetches) coveringLocked(path string, off, length int64) *inflightFetch {
	for _, f := range i.byPath[path] {
		if f.covers(off, length) {
			return f
		}
	}

	return nil
}

// finish publishes a fetch's result and stops advertising it.
//
// Removing it before closing done, so that a caller which has already selected this fetch as its leader
// is not joined by a new one after the result is known: a fetch still in the map after it completes
// would be matched by join, whose waiter would then read a result it cannot distinguish from one still
// pending.
func (i *inflightFetches) finish(path string, f *inflightFetch, data []byte, err error) {
	i.mu.Lock()

	flights := i.byPath[path]
	for n, candidate := range flights {
		if candidate == f {
			i.byPath[path] = append(flights[:n], flights[n+1:]...)
			break
		}
	}

	if len(i.byPath[path]) == 0 {
		delete(i.byPath, path)
	}

	f.data, f.err = data, err

	i.mu.Unlock()

	close(f.done)
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
	dirPath := n.key()
	if dirPath == "" {
		return name
	}
	return filepath.Join(dirPath, name)
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
