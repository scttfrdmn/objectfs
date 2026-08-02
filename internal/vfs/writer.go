package vfs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/objectfs/objectfs/pkg/types"
)

// Writer is the [types.WriteBuffer] implementation backed by [Node] and [Flusher]: dirty byte
// ranges per path, and a flush that does genuine read-modify-write.
//
// It replaces internal/buffer.WriteBuffer, whose representation was one contiguous []byte plus a
// single offset per key. That representation could express "a run of bytes starting somewhere" and
// nothing else, which produced, as separate audit findings:
//
//   - an offset write truncating the object to just the bytes written, because the flush callback
//     received the offset and discarded it;
//   - EIO for any write that did not continue the run, which is the pattern SQLite, mmap writeback,
//     tar, and HDF5 all use;
//   - a flush with a nil callback counted as success, dropping every buffered byte;
//   - a buffer deleted after a successful upload without rechecking for writes that arrived during
//     it, annihilating them while accounting them flushed;
//   - Flush scheduling asynchronous work and returning nil, so close(2) reported success before the
//     object existed and an AccessDenied only ever incremented a counter.
//
// Every one of those is a consequence of the missing concept rather than an independent bug, which is
// why this is a replacement and not a patch.
//
// # The context problem
//
// [types.WriteBuffer.Flush] takes no context, but read-modify-write must issue ranged GETs, which
// need one. The interface is not changed here: it is implemented by the FUSE layer that #22 rewrites,
// and widening it now would mean editing that surface twice. Instead a Writer holds the context its
// owner's lifetime is bounded by — the adapter passes one that outlives Start, since a flush runs
// long after Start returns — and [Writer.FlushContext] is available to callers that have a
// per-operation context, which the go-fuse shim does. When the FUSE layer is rewritten the
// context-taking methods become the only ones.
//
// Safe for concurrent use.
type Writer struct {
	flusher *Flusher

	// ctx bounds flushes made through the context-free interface methods. Not a stored request
	// context: it is the owner's lifetime, and canceling it means "we are shutting down".
	ctx context.Context //nolint:containedctx // see the context problem, above

	mu    sync.Mutex
	nodes map[string]*Node
}

// NewWriter returns a Writer flushing through backend.
//
// ctx bounds every flush made through the [types.WriteBuffer] methods, which carry none of their own.
// Pass a context whose lifetime is the mount's, not a request's: a flush runs when the kernel decides
// to, typically long after whatever call created the file has returned. v0.10.0 hit this and papered
// over it by hardcoding context.Background() inside the flush callback (#100), which also removed any
// way to cancel a flush at unmount.
func NewWriter(ctx context.Context, backend types.Backend) (*Writer, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalid)
	}

	f, err := NewFlusher(backend)
	if err != nil {
		return nil, err
	}

	return &Writer{
		flusher: f,
		ctx:     ctx,
		nodes:   make(map[string]*Node),
	}, nil
}

// Write buffers a write of data at offset for key. It implements [types.WriteBuffer].
//
// No legitimate write is refused. There is no contiguity requirement, no batch-size threshold that
// forces a flush per write, and no path that returns EIO for a sparse write.
func (w *Writer) Write(key string, offset int64, data []byte) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalid)
	}
	if offset < 0 {
		return fmt.Errorf("%w: negative write offset %d for %q", ErrInvalid, offset, key)
	}
	if len(data) == 0 {
		return nil
	}

	n, err := w.node(w.ctx, key)
	if err != nil {
		return err
	}
	if _, err := n.Write(offset, data, false); err != nil {
		return err
	}
	return nil
}

// Truncate records that key's file has been resized to size.
//
// Not part of [types.WriteBuffer]: v0.10.0 had no truncate anywhere — no Setattr, no Truncate, no
// O_TRUNC handling — so `> file` could not shorten an object. It is here because the write path is
// where a pending size change belongs, and #22 wires it to Setattr.
func (w *Writer) Truncate(ctx context.Context, key string, size int64) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalid)
	}

	n, err := w.node(ctx, key)
	if err != nil {
		return err
	}
	return n.Truncate(size)
}

// SetAttr records new attributes for key, to be persisted at flush. The three booleans say which of
// mode, uid, and gid the caller is setting; a non-zero from.Mtime sets the modification time.
//
// The mask is the whole point. A Setattr call carries a FATTR bitmask saying which fields the kernel
// actually set, and applying all of them unconditionally would have `touch` reset a file's mode to
// whatever the caller's zero value happened to be. Passing the mask down to [Node.SetAttr] rather
// than resolving it here keeps one owner for the merge.
func (w *Writer) SetAttr(ctx context.Context, key string, mode, uid, gid bool, from Attr) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalid)
	}

	n, err := w.node(ctx, key)
	if err != nil {
		return err
	}
	return n.SetAttr(mode, uid, gid, from)
}

// Attr returns key's current attributes, including changes not yet persisted, and whether the write
// path holds any state for the key at all.
//
// A false second return is not an error: it means nothing is buffered, so the caller's own stored
// metadata is the current answer. Reporting an empty Attr as authoritative instead would make a stat
// of an unopened file report mode 0000 — which is the defect that made every directory untraversable
// in v0.10.0.
func (w *Writer) Attr(key string) (Attr, bool) {
	w.mu.Lock()
	n, ok := w.nodes[key]
	w.mu.Unlock()

	if !ok {
		return Attr{}, false
	}
	return n.Attr(), true
}

// ReadAt fills buf with key's contents at offset, overlaying pending writes on the stored object,
// and returns how many leading bytes are valid.
//
// A read must consult the pending writes, not just the object store. v0.10.0's read path asked the
// cache and the backend and never the write buffer, so a read after a write on the same descriptor
// returned pre-write bytes for up to the cache's five-minute TTL. Ranges covered entirely by pending
// writes are served without touching the network.
func (w *Writer) ReadAt(ctx context.Context, key string, buf []byte, offset int64) (int, error) {
	if key == "" {
		return 0, fmt.Errorf("%w: empty key", ErrInvalid)
	}
	if len(buf) == 0 {
		return 0, nil
	}

	n, err := w.node(ctx, key)
	if err != nil {
		return 0, err
	}

	r, need, err := n.ReadRange(offset, len(buf))
	if err != nil {
		return 0, err
	}

	var stored []byte
	if need {
		stored, err = w.flusher.backend.GetObject(ctx, key, r.Offset, r.Length)
		if err != nil {
			if !IsNotFound(err) {
				return 0, fmt.Errorf("%w: read %q [%d,%d): %w", ErrBackend, key, r.Offset, r.End(), err)
			}
			stored = nil
		}
	}

	return n.ReadInto(buf, offset, r.Offset, stored)
}

// FileSize returns key's logical length including pending writes, which is what stat must report.
//
// Named apart from [Writer.Size] because [types.WriteBuffer] already claims that name for the total
// buffered byte count. Two different questions — "how big is this file" and "how much memory am I
// holding" — and conflating them is how v0.10.0's Getattr came to report a file's pre-write size:
// it read the object's metadata and never consulted the write buffer, so the kernel truncated reads
// of a file being written at its old length.
func (w *Writer) FileSize(ctx context.Context, key string) (int64, error) {
	n, err := w.node(ctx, key)
	if err != nil {
		return 0, err
	}
	return n.Size(), nil
}

// Flush makes key durable. It implements [types.WriteBuffer].
//
// Synchronous, and it returns an error when the object is not stored. That is the difference between
// this and what it replaces: v0.10.0's Flush scheduled a background flush and returned nil, so
// close(2) reported success before anything had been uploaded and a rejected PUT was visible only as
// a counter.
func (w *Writer) Flush(key string) error {
	return w.FlushContext(w.ctx, key)
}

// FlushContext is [Writer.Flush] with an explicit context.
func (w *Writer) FlushContext(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalid)
	}

	w.mu.Lock()
	n, ok := w.nodes[key]
	w.mu.Unlock()

	if !ok {
		// Nothing buffered for this key. Not an error: fsync on a file with no pending writes is a
		// no-op, and so is a second close.
		return nil
	}

	if err := w.flusher.Flush(ctx, n); err != nil {
		return err
	}

	// Drop the node only once it is clean. A node still dirty after a flush that reported success
	// would mean the flusher is broken; dropping it anyway would convert that into data loss.
	w.mu.Lock()
	defer w.mu.Unlock()
	if !n.Dirty() {
		delete(w.nodes, key)
	}
	return nil
}

// FlushAll makes every buffered key durable. It implements [types.WriteBuffer].
//
// It attempts every key even after one fails, and reports the first failure with a count of the
// others. Stopping at the first error would leave later keys unflushed with no indication which, and
// this is the path unmount uses — the point at which unflushed data is lost for good.
func (w *Writer) FlushAll() error {
	return w.FlushAllContext(w.ctx)
}

// FlushAllContext is [Writer.FlushAll] with an explicit context.
func (w *Writer) FlushAllContext(ctx context.Context) error {
	w.mu.Lock()
	keys := make([]string, 0, len(w.nodes))
	for k := range w.nodes {
		keys = append(keys, k)
	}
	w.mu.Unlock()

	var firstErr error
	failed := 0
	for _, k := range keys {
		if err := w.FlushContext(ctx, k); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	if firstErr == nil {
		return nil
	}
	if failed == 1 {
		return firstErr
	}
	return fmt.Errorf("flush all: %d of %d keys failed, first: %w", failed, len(keys), firstErr)
}

// Size returns the total number of dirty bytes buffered across every key. It implements
// [types.WriteBuffer].
func (w *Writer) Size() int64 {
	w.mu.Lock()
	nodes := make([]*Node, 0, len(w.nodes))
	for _, n := range w.nodes {
		nodes = append(nodes, n)
	}
	w.mu.Unlock()

	// Summed outside the writer lock: each node takes its own, and holding both would invert the
	// order [Node] documents.
	var total int64
	for _, n := range nodes {
		total += n.DirtyBytes()
	}
	return total
}

// Count returns the number of keys with buffered writes. It implements [types.WriteBuffer].
func (w *Writer) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.nodes)
}

// Dirty reports whether key has anything not yet durable: content, a truncation, or an attribute
// change.
//
// A flush path must ask this rather than "are there dirty bytes". An fsync that returns success
// because no byte is dirty, while a truncation is still only in memory, reports durability it has not
// achieved.
func (w *Writer) Dirty(key string) bool {
	w.mu.Lock()
	n, ok := w.nodes[key]
	w.mu.Unlock()

	return ok && n.Dirty()
}

// Close flushes everything and releases the writer.
//
// It reports what it could not flush rather than discarding it. An unmount that exits silently while
// a node is dirty loses data with no record of which file.
func (w *Writer) Close() error {
	if err := w.FlushAll(); err != nil {
		return fmt.Errorf("close write path: %w", err)
	}
	return nil
}

// node returns the [Node] for key, creating it from the object's stored attributes on first use.
//
// The stored state is fetched once per key, not per write: the size is what read-modify-write splices
// against, and [Node] tracks it from there. Refetching would also reintroduce a race, since the size
// could change under a pending write.
//
// The full attributes are read, not just the size. A node created with a default mode and a zero
// uid/gid would persist those on the next flush, so writing to a file owned by someone else would
// chown it to root — the write path became an attribute writer the moment attribute write-back landed,
// and reading only the size is the shape of that defect.
func (w *Writer) node(ctx context.Context, key string) (*Node, error) {
	w.mu.Lock()
	if n, ok := w.nodes[key]; ok {
		w.mu.Unlock()
		return n, nil
	}
	w.mu.Unlock()

	// The HEAD runs outside the lock: it is a network call, and holding a mutex that every write on
	// every path contends on across it would serialize the whole write path behind one stat.
	attr, storedSize, warns, err := w.flusher.StoredAttr(ctx, key)
	if err != nil {
		return nil, err
	}
	for _, warn := range warns {
		// Logged rather than returned: a bad metadata value must not make the file inaccessible, but
		// silently discarding an attribute the user set is its own defect.
		slog.Warn("ignoring unusable stored attribute", "key", key, "detail", warn)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Recheck: another goroutine may have created the node while the HEAD was in flight. Keep theirs,
	// since it may already hold writes.
	if n, ok := w.nodes[key]; ok {
		return n, nil
	}

	n := NewNode(key, attr, storedSize)
	w.nodes[key] = n
	return n, nil
}

// Writer implements the write-buffer contract the FUSE layer consumes.
var _ types.WriteBuffer = (*Writer)(nil)
