package vfs

import (
	"fmt"
	"sync"
)

// OpenFlags is how a handle was opened. It is this package's own bitmask rather than syscall's, so
// that a second kernel binding translates into it instead of the core depending on Linux constants.
type OpenFlags uint32

const (
	// OpenRead permits reads through the handle.
	OpenRead OpenFlags = 1 << iota

	// OpenWrite permits writes through the handle.
	OpenWrite

	// OpenAppend forces every write to the end of the file, ignoring the offset the caller supplies.
	OpenAppend

	// OpenTruncate discards the file's existing contents at open time.
	OpenTruncate
)

// CanRead reports whether f permits reading.
func (f OpenFlags) CanRead() bool { return f&OpenRead != 0 }

// CanWrite reports whether f permits writing.
func (f OpenFlags) CanWrite() bool { return f&OpenWrite != 0 }

// String implements fmt.Stringer, for error messages and logs.
func (f OpenFlags) String() string {
	if f == 0 {
		return "0"
	}
	var parts []byte
	appendPart := func(name string) {
		if len(parts) > 0 {
			parts = append(parts, '|')
		}
		parts = append(parts, name...)
	}
	if f&OpenRead != 0 {
		appendPart("read")
	}
	if f&OpenWrite != 0 {
		appendPart("write")
	}
	if f&OpenAppend != 0 {
		appendPart("append")
	}
	if f&OpenTruncate != 0 {
		appendPart("trunc")
	}
	if rest := f &^ (OpenRead | OpenWrite | OpenAppend | OpenTruncate); rest != 0 {
		appendPart(fmt.Sprintf("unknown(%#x)", uint32(rest)))
	}
	return string(parts)
}

// Node is the state ObjectFS holds for one path, shared by every handle open on it.
//
// Shared, not per-handle, and that is the whole reason this type is separate from [Handle]. One path
// is one S3 object; two descriptors writing it are writing the same object. If each handle buffered
// its own dirty ranges, the second flush would overwrite the first's work with a full-object PUT
// assembled from stale bytes — losing the first writer's data with no error anywhere. POSIX also
// requires a read through one descriptor to see a write through another, which is only expressible
// if both consult the same extent list.
//
// Locking: take Node.mu around any access to the fields below it. A caller holding a Node lock must
// not call back into [HandleTable]; the table's lock is always acquired first. Nodes are never
// locked two at a time, so there is no ordering to get wrong between them.
type Node struct {
	// Path is the file's path within the filesystem, without a leading slash. Immutable, so it is
	// readable without the lock.
	Path string

	mu sync.Mutex

	// attr is the file's current attributes, including any not yet persisted.
	attr Attr

	// pending holds the writes not yet in the object store.
	pending ExtentList

	// storedSize is the length of the object as it exists in storage right now — not the file's
	// length, which is attr.Size and which pending writes and truncations can change.
	storedSize int64

	// dirtyAttr records that attributes have changed and must be written back even if no byte of
	// content has. A chmod with no write is still work.
	dirtyAttr bool

	// generation counts mutations. A flush captures it before uploading and MarkFlushed refuses to
	// clear the pending state if it has moved, which is how a write racing an upload is detected
	// rather than silently dropped.
	generation uint64

	// handles counts open handles. The node is evictable at zero, and only at zero — dropping it
	// while a handle is open would lose that handle's dirty ranges.
	handles int
}

// NewNode returns a node for path whose stored state is described by attr and storedSize.
//
// Most callers get nodes from [HandleTable.Open], which is the path a mount takes: a node with no
// handle open on it cannot be found by the invalidation or shutdown paths. This constructor is for
// owners that track nodes themselves, [Writer] being the one — its keys are not open descriptors.
func NewNode(path string, attr Attr, storedSize int64) *Node {
	return &Node{Path: path, attr: attr, storedSize: storedSize}
}

// DirtyBytes returns the number of buffered bytes not yet in the object store.
//
// It is a memory-accounting figure, not a durability one: a node with zero dirty bytes can still need
// a flush, because a truncation changes the file without dirtying a byte. Ask [Node.Dirty] for that.
func (n *Node) DirtyBytes() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pending.Bytes()
}

// Attr returns a snapshot of the node's attributes, with Size reflecting pending writes.
func (n *Node) Attr() Attr {
	n.mu.Lock()
	defer n.mu.Unlock()

	a := n.attr
	a.Size = n.pending.Size(n.storedSize)
	return a
}

// SetAttr replaces the mutable attributes — mode, ownership, and times — and marks them for
// write-back. Type, Size, ETag, and Checksum are not settable: they are facts about the stored
// object, not preferences.
func (n *Node) SetAttr(mode, uid, gid bool, from Attr) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if mode {
		if from.Mode&^0o7777 != 0 {
			return fmt.Errorf("%w: mode %#o carries non-permission bits", ErrInvalid, from.Mode)
		}
		n.attr.Mode = from.Mode.Perm()
	}
	if uid {
		n.attr.UID = from.UID
	}
	if gid {
		n.attr.GID = from.GID
	}
	if !from.Mtime.IsZero() {
		n.attr.Mtime = from.Mtime
		n.attr.Ctime = from.Mtime
	}
	n.dirtyAttr = true
	n.generation++
	return nil
}

// Dirty reports whether the node has anything that must reach the object store: content, a
// truncation, or an attribute change.
//
// A flush path must consult this rather than "are there dirty bytes". An fsync that returns success
// because no byte is dirty, while a truncation or a chmod is still only in memory, reports durability
// it has not achieved.
func (n *Node) Dirty() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dirtyContentLocked() || n.dirtyAttr
}

func (n *Node) dirtyContentLocked() bool {
	p, err := n.pending.Plan(n.storedSize)
	if err != nil {
		// Plan only fails on a negative stored size, which this type never assigns. Treat the
		// impossible as dirty: a spurious flush is recoverable, a skipped one is data loss.
		return true
	}
	return !p.Noop
}

// Size returns the file's current logical length, including pending writes.
func (n *Node) Size() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pending.Size(n.storedSize)
}

// StoredSize returns the length of the object as it currently exists in storage.
func (n *Node) StoredSize() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.storedSize
}

// Write buffers a write of data at offset. When atEnd is set the offset is ignored and the data goes
// to the current end of the file, as O_APPEND requires.
//
// It returns the number of bytes accepted, which is always all of them. No legitimate write is
// refused: v0.10.0 rejected any write that did not continue its single contiguous buffer, returning
// EIO to SQLite, mmap writeback, tar, and HDF5 alike.
func (n *Node) Write(offset int64, data []byte, atEnd bool) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if atEnd {
		offset = n.pending.Size(n.storedSize)
	}
	if err := n.pending.Add(offset, data); err != nil {
		return 0, fmt.Errorf("write %q: %w", n.Path, err)
	}
	if len(data) > 0 {
		n.generation++
	}
	return len(data), nil
}

// Truncate resizes the file to size, recording it as pending.
func (n *Node) Truncate(size int64) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if err := n.pending.Truncate(size); err != nil {
		return fmt.Errorf("truncate %q: %w", n.Path, err)
	}
	n.generation++
	return nil
}

// ReadRange returns the byte range of the stored object a read of length bytes at offset needs
// fetched, and whether a fetch is needed at all.
//
// A read that pending writes fully cover needs no fetch, which is what makes read-after-write both
// correct and free. Callers pass the fetched bytes to [Node.ReadInto] along with the returned range's
// Offset, which is not necessarily the read's own offset: pending writes at the head of the range are
// trimmed too, so the fetch can begin later than the read does.
func (n *Node) ReadRange(offset int64, length int) (Range, bool, error) {
	if offset < 0 {
		return Range{}, false, fmt.Errorf("%w: negative read offset %d", ErrInvalid, offset)
	}
	if length < 0 {
		return Range{}, false, fmt.Errorf("%w: negative read length %d", ErrInvalid, length)
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	end := min(offset+int64(length), n.pending.StoredValid(n.storedSize))
	if end <= offset {
		return Range{}, false, nil
	}

	// Drop the parts of both ends the pending writes already answer. A write that covers the whole
	// request makes this the entire range, and the read touches no network at all.
	start := n.pending.UncoveredStart(offset, end)
	end = n.pending.UncoveredEnd(start, end)

	if end <= start {
		return Range{}, false, nil
	}

	return Range{Offset: start, Length: end - start}, true, nil
}

// ReadInto fills buf with the file's contents at offset and returns how many leading bytes are
// valid — short at EOF, zero past it.
//
// stored must hold the bytes the object store returned for the range [Node.ReadRange] asked for, and
// storedOffset must be that range's Offset. They are separate arguments because the range can begin
// after the read does: ReadRange trims pending writes off the head as well as the tail, so the fetched
// bytes belong at buf[storedOffset-offset] and not at buf[0]. Passing the read's own offset for a range
// that was narrowed splices the object's bytes too early and silently corrupts the result — which is
// why the head narrowing and this parameter had to land together.
//
// When ReadRange reported no fetch was needed, pass a nil stored; storedOffset is then ignored.
// Supplying fewer bytes than requested is allowed and treated as the object being shorter than
// believed.
func (n *Node) ReadInto(buf []byte, offset, storedOffset int64, stored []byte) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("%w: negative read offset %d", ErrInvalid, offset)
	}
	if len(stored) > 0 && storedOffset < offset {
		return 0, fmt.Errorf("%w: stored bytes start at %d, before the read's own offset %d",
			ErrInvalid, storedOffset, offset)
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if at := storedOffset - offset; len(stored) > 0 && at < int64(len(buf)) {
		copy(buf[at:], stored)
	}

	// The object may be shorter than storedSize claims — it can have been replaced behind our back —
	// so cap the stored extent at what actually arrived. A range narrowed at the tail also ends early,
	// which caps this below the true stored size; that is harmless, because the tail is only narrowed
	// when a pending write covers it, and [ExtentList.Size] takes the later of the two.
	effective := n.storedSize
	if len(stored) > 0 {
		if got := storedOffset + int64(len(stored)); got < effective {
			effective = got
		}
	}

	valid, err := n.pending.ReadAt(buf, offset, effective)
	if err != nil {
		return 0, fmt.Errorf("read %q: %w", n.Path, err)
	}
	return valid, nil
}

// FlushPlan returns what must be done to make the node durable, and the attributes to write with it.
//
// The returned plan is a snapshot: writes arriving after this call are not in it, and the node stays
// dirty until [Node.MarkFlushed] is told which state was actually persisted.
func (n *Node) FlushPlan() (FlushPlan, Attr, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	p, err := n.pending.Plan(n.storedSize)
	if err != nil {
		return FlushPlan{}, Attr{}, fmt.Errorf("flush %q: %w", n.Path, err)
	}

	a := n.attr
	a.Size = p.Size
	return p, a, nil
}

// Splice assembles the object body to upload from the stored fragments the plan demanded.
func (n *Node) Splice(size int64, base []Extent) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	body, err := n.pending.Splice(size, base)
	if err != nil {
		return nil, fmt.Errorf("flush %q: %w", n.Path, err)
	}
	return body, nil
}

// MarkFlushed records that the object store now holds size bytes with the given etag, and clears the
// pending state that was flushed.
//
// generation is the value [Node.Generation] returned when the flush plan was made. If the node has
// changed since — a write or truncate landed during the upload — the pending state is kept and false
// is returned: the upload was of stale content and the node is still dirty.
//
// This check is the fix for a specific v0.10.0 data-loss path. Its flush deleted the write buffer on
// success without testing whether anything had arrived meanwhile, so a write concurrent with an
// upload was discarded and accounted as flushed.
func (n *Node) MarkFlushed(generation uint64, size int64, etag string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.generation != generation {
		return false
	}

	n.pending.Reset()
	n.storedSize = size
	n.attr.Size = size
	n.attr.ETag = etag
	n.dirtyAttr = false
	return true
}

// Generation is a counter of mutations to the node. Take it before building a flush plan and pass it
// to [Node.MarkFlushed], which uses it to detect a write that raced the upload.
func (n *Node) Generation() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.generation
}

// Handle is one open file description: what a single open(2) returned.
//
// It holds only what is per-descriptor — the flags it was opened with. Position is not here because
// FUSE supplies an explicit offset on every read and write; a handle carrying its own cursor would
// be a second, disagreeing source of truth. Content state lives on the shared [Node].
type Handle struct {
	// ID identifies the handle to the kernel. Immutable.
	ID uint64

	// Flags are the access mode the handle was opened with. Immutable.
	Flags OpenFlags

	// Node is the shared per-path state. Immutable; never nil for a live handle.
	Node *Node
}

// HandleTable owns every open handle and every node with a handle open on it.
//
// It is the type that makes "is this file open?" answerable, which the flush and invalidation paths
// both need and which v0.10.0 could not answer: its write buffer was a map keyed by path with no
// notion of who held it open, so a flush could delete a buffer another writer was still filling.
//
// Safe for concurrent use.
type HandleTable struct {
	mu      sync.Mutex
	nextID  uint64
	handles map[uint64]*Handle
	nodes   map[string]*Node
}

// NewHandleTable returns an empty table.
func NewHandleTable() *HandleTable {
	return &HandleTable{
		handles: make(map[uint64]*Handle),
		nodes:   make(map[string]*Node),
	}
}

// Open registers a new handle on path, creating the node if this is the first open.
//
// attr and storedSize describe the object as it exists in storage and are used only when the node is
// created — a second open of an already-open file must not reset state the first open has dirtied. A
// caller with fresher metadata for an open file should call [Node.SetAttr] or refresh explicitly,
// where the intent is visible.
//
// [OpenTruncate] is applied here, so an O_TRUNC open is recorded as a pending truncation to zero
// rather than as an immediate object write. That keeps `> file` a single flush instead of a PUT
// followed by another PUT.
func (t *HandleTable) Open(path string, flags OpenFlags, attr Attr, storedSize int64) (*Handle, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: empty path", ErrInvalid)
	}
	if storedSize < 0 {
		return nil, fmt.Errorf("%w: negative stored size %d for %q", ErrInvalid, storedSize, path)
	}
	if !flags.CanRead() && !flags.CanWrite() {
		return nil, fmt.Errorf("%w: open of %q permits neither read nor write", ErrInvalid, path)
	}
	if flags&(OpenWrite|OpenAppend|OpenTruncate) != 0 && !flags.CanWrite() {
		return nil, fmt.Errorf("%w: open of %q requests %v without write access", ErrInvalid, path, flags)
	}
	if attr.Type == FileTypeDir {
		return nil, fmt.Errorf("%w: %q", ErrIsDir, path)
	}
	if err := attr.Validate(); err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	n, ok := t.nodes[path]
	if !ok {
		n = &Node{Path: path, attr: attr, storedSize: storedSize}
		t.nodes[path] = n
	}

	// Handle IDs are never reused. Wrapping would let a stale handle from a closed file address a
	// live one — a use-after-free reachable from userspace, since the kernel supplies the ID.
	t.nextID++
	h := &Handle{ID: t.nextID, Flags: flags, Node: n}
	t.handles[h.ID] = h

	n.mu.Lock()
	n.handles++
	if flags&OpenTruncate != 0 {
		if err := n.pending.Truncate(0); err != nil {
			n.mu.Unlock()
			return nil, fmt.Errorf("open %q with truncate: %w", path, err)
		}
		n.generation++
	}
	n.mu.Unlock()

	return h, nil
}

// Lookup returns the handle with the given ID.
func (t *HandleTable) Lookup(id uint64) (*Handle, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	h, ok := t.handles[id]
	if !ok {
		return nil, fmt.Errorf("%w: no such handle %d", ErrInvalid, id)
	}
	return h, nil
}

// Node returns the node for path if any handle is open on it.
//
// This is what a write-invalidation path asks: a cached read must be dropped when the file is open
// for writing, and a stat must consult pending writes rather than the object's metadata.
func (t *HandleTable) Node(path string) (*Node, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	n, ok := t.nodes[path]
	return n, ok
}

// Release drops a handle. It reports whether that was the last handle on the node, and returns the
// node so the caller can flush it.
//
// The node is removed from the table only when the last handle goes and nothing is dirty. A dirty
// node is kept — releasing it would discard the writes, which is what close(2) must never do. The
// caller flushes and then calls [HandleTable.Forget].
//
// v0.10.0 got this backwards: its flush deleted the write buffer on success without checking for a
// concurrent writer, so writes arriving mid-upload were dropped and counted as flushed.
func (t *HandleTable) Release(id uint64) (*Node, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	h, ok := t.handles[id]
	if !ok {
		return nil, false, fmt.Errorf("%w: no such handle %d", ErrInvalid, id)
	}
	delete(t.handles, id)

	n := h.Node
	n.mu.Lock()
	n.handles--
	remaining := n.handles
	dirty := n.dirtyContentLocked() || n.dirtyAttr
	n.mu.Unlock()

	if remaining < 0 {
		// Only reachable if the table's accounting is broken, which would mean a handle was released
		// twice. Report it rather than letting the node be evicted with writes still pending.
		return n, false, fmt.Errorf("%w: handle count for %q went negative", ErrInvalid, n.Path)
	}

	last := remaining == 0
	if last && !dirty {
		delete(t.nodes, n.Path)
	}
	return n, last, nil
}

// Forget drops a node that has no open handles and nothing dirty. It is how a caller finishes the
// release [HandleTable.Release] deferred pending a flush.
//
// It refuses to drop a node that is still dirty or still open, returning false. That refusal is the
// point: a caller whose flush failed must not be able to make the failure disappear by forgetting
// the node.
func (t *HandleTable) Forget(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	n, ok := t.nodes[path]
	if !ok {
		return false
	}

	n.mu.Lock()
	open := n.handles
	dirty := n.dirtyContentLocked() || n.dirtyAttr
	n.mu.Unlock()

	if open != 0 || dirty {
		return false
	}
	delete(t.nodes, path)
	return true
}

// Len returns the number of open handles.
func (t *HandleTable) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.handles)
}

// Nodes returns the paths of every node the table holds, in unspecified order.
//
// A shutdown path uses this to find what still needs flushing. It must not silently skip anything:
// an unmount that exits while a node is dirty loses data, so the caller is expected to iterate this
// to completion and report what it could not flush.
func (t *HandleTable) Nodes() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	paths := make([]string, 0, len(t.nodes))
	for p := range t.nodes {
		paths = append(paths, p)
	}
	return paths
}

// DirtyNodes returns the nodes with unflushed state, in unspecified order.
func (t *HandleTable) DirtyNodes() []*Node {
	t.mu.Lock()
	nodes := make([]*Node, 0, len(t.nodes))
	for _, n := range t.nodes {
		nodes = append(nodes, n)
	}
	t.mu.Unlock()

	// Checked outside the table lock: Node.Dirty takes the node lock, and holding both would invert
	// the documented order.
	dirty := nodes[:0]
	for _, n := range nodes {
		if n.Dirty() {
			dirty = append(dirty, n)
		}
	}
	return dirty
}
