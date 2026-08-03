package vfs

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/scttfrdmn/objectfs/pkg/types"
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

	// maxMemory caps the total dirty bytes held across every node. Zero means unbounded, which is
	// what every caller got before write_buffer.max_memory was wired: see [WriterOptions].
	maxMemory int64

	// maxNodes caps how many keys may hold pending writes at once. Zero means unbounded. Distinct
	// from maxMemory because the two exhaust differently — a million one-byte writes to a million
	// paths is a trivial byte count and a million live nodes.
	maxNodes int

	mu    sync.Mutex
	nodes map[string]*Node
}

// WriterOptions configures a [Writer]'s resource bounds. A zero value means unbounded, which is the
// behavior of every release through v0.10.3.
//
// These exist because `write_buffer.max_memory` was declared in the config schema, defaulted to
// "512MB", and validated as a size string — and read by nothing. A grep for its only Go field name
// returned three hits, all of them inside internal/config: the declaration, the default, and the
// validator. So a mount reported a 512 MB write-buffer limit and enforced no limit at all, which on
// the dirty-range write path means an interval map that grows until flush with nothing to stop it.
//
// That is the same shape as the defects the v0.10.x releases were spent correcting — a configuration
// key that claims a property no code enforces — and it is on the write path, where the consequence is
// the mount process being killed by the OOM killer with every open file's dirty ranges in it.
type WriterOptions struct {
	// MaxMemory caps total buffered dirty bytes across all keys. Zero means unbounded.
	MaxMemory int64

	// MaxBuffers caps how many keys may hold pending writes at once. Zero means unbounded.
	MaxBuffers int
}

// NewWriter returns a Writer flushing through backend, with no resource bounds.
//
// ctx bounds every flush made through the [types.WriteBuffer] methods, which carry none of their own.
// Pass a context whose lifetime is the mount's, not a request's: a flush runs when the kernel decides
// to, typically long after whatever call created the file has returned. v0.10.0 hit this and papered
// over it by hardcoding context.Background() inside the flush callback (#100), which also removed any
// way to cancel a flush at unmount.
//
// Prefer [NewWriterWithOptions] on a mount path: an unbounded write path is a memory-exhaustion
// hazard, and this constructor is retained for tests and for callers with no configuration to draw a
// bound from.
func NewWriter(ctx context.Context, backend types.Backend) (*Writer, error) {
	return NewWriterWithOptions(ctx, backend, WriterOptions{})
}

// NewWriterWithOptions returns a Writer flushing through backend, bounded by opts.
//
// A negative bound is rejected rather than treated as unbounded. "MaxMemory: -1" is a mistake, and
// silently reading a mistake as "no limit" is how a memory bound comes to be absent while the
// configuration says otherwise — which is the defect this constructor exists to close.
func NewWriterWithOptions(ctx context.Context, backend types.Backend, opts WriterOptions) (*Writer, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalid)
	}
	if opts.MaxMemory < 0 {
		return nil, fmt.Errorf("%w: negative write-buffer memory limit %d", ErrInvalid, opts.MaxMemory)
	}
	if opts.MaxBuffers < 0 {
		return nil, fmt.Errorf("%w: negative write-buffer count limit %d", ErrInvalid, opts.MaxBuffers)
	}

	f, err := NewFlusher(backend)
	if err != nil {
		return nil, err
	}

	return &Writer{
		flusher:   f,
		ctx:       ctx,
		maxMemory: opts.MaxMemory,
		maxNodes:  opts.MaxBuffers,
		nodes:     make(map[string]*Node),
	}, nil
}

// MaxMemory returns the writer's dirty-byte ceiling, or 0 if it is unbounded.
func (w *Writer) MaxMemory() int64 { return w.maxMemory }

// Write buffers a write of data at offset for key. It implements [types.WriteBuffer].
//
// No legitimate write is refused for its shape. There is no contiguity requirement, no batch-size
// threshold that forces a flush per write, and no path that returns EIO for a sparse write.
//
// A write may be refused for *memory*, but only after the writer has tried to make room by flushing
// what it already holds — see [Writer.reclaim]. ENOSPC-by-way-of-ErrNoSpace is the last resort, not
// the first response, because a bound that rejects writes it could have absorbed is a bound that
// makes the filesystem worse rather than safer.
func (w *Writer) Write(key string, offset int64, data []byte) error {
	return w.WriteContext(w.ctx, key, offset, data)
}

// WriteContext is [Writer.Write] with an explicit context, which the reclaim path needs: making room
// means flushing, and a flush issues ranged GETs and a PUT.
func (w *Writer) WriteContext(ctx context.Context, key string, offset int64, data []byte) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalid)
	}
	if offset < 0 {
		return fmt.Errorf("%w: negative write offset %d for %q", ErrInvalid, offset, key)
	}
	if len(data) == 0 {
		return nil
	}

	// Admission runs before the node is created, so a write refused for memory does not leave a node
	// behind that a later Count() would report as buffered.
	if err := w.admit(ctx, key, int64(len(data))); err != nil {
		return err
	}

	n, err := w.node(ctx, key)
	if err != nil {
		return err
	}
	if _, err := n.Write(offset, data, false); err != nil {
		return err
	}
	return nil
}

// admit makes room for an incoming write of n bytes to key, flushing other keys if necessary, and
// reports whether the write may proceed.
//
// # Why the incoming write is counted, and why it is not the whole answer
//
// The check is `held + incoming > limit`, so a single write larger than the entire limit would be
// refused outright — which would be wrong. A 128 MiB write to a mount configured with a 64 MiB buffer
// is a legal write, and the kernel splits writes at MaxWrite anyway (128 KiB), so the case arises only
// through the [types.WriteBuffer] interface directly. It is admitted: see the oversize arm below. The
// bound's purpose is to stop *accumulation* across many files, not to impose a maximum write size that
// POSIX has no way to report.
func (w *Writer) admit(ctx context.Context, key string, n int64) error {
	if w.maxMemory <= 0 && w.maxNodes <= 0 {
		return nil
	}

	// The count bound is checked against keys that would be *added*. An existing node holding pending
	// writes is already counted, so writing to it again must never be refused for the node count —
	// otherwise fsync-less programs writing one file forever would fail once the map filled with other
	// files.
	if w.maxNodes > 0 {
		w.mu.Lock()
		_, existing := w.nodes[key]
		count := len(w.nodes)
		w.mu.Unlock()

		if !existing && count >= w.maxNodes {
			if err := w.reclaim(ctx, key, 0); err != nil {
				return err
			}

			w.mu.Lock()
			count = len(w.nodes)
			w.mu.Unlock()

			if count >= w.maxNodes {
				return fmt.Errorf("%w: write path holds pending writes for %d files, the configured "+
					"limit (write_buffer.max_buffers); flushing did not release any", ErrNoSpace, count)
			}
		}
	}

	if w.maxMemory <= 0 {
		return nil
	}

	if w.Size()+n <= w.maxMemory {
		return nil
	}

	// Over the limit. Flush other keys to make room; a flush is what converts dirty bytes into durable
	// ones, so it is the only thing that can.
	if err := w.reclaim(ctx, key, n); err != nil {
		return err
	}

	held := w.Size()
	if held+n <= w.maxMemory {
		return nil
	}

	// Still over. If this key alone accounts for it, the write is oversize rather than the buffer being
	// full, and refusing it would mean a legal write is permanently impossible at this configuration.
	// Admit it and let the flush that follows return the memory: exceeding the bound transiently is
	// strictly better than an unserviceable filesystem, and the bound exists to stop unbounded growth
	// across files, which this is not.
	if held == 0 || n > w.maxMemory {
		return nil
	}

	return fmt.Errorf("%w: write path holds %d dirty bytes and this %d-byte write would exceed the "+
		"%d-byte limit (write_buffer.max_memory); flushing did not release enough",
		ErrNoSpace, held, n, w.maxMemory)
}

// reclaim flushes buffered keys other than exclude until held dirty bytes fall far enough for an
// incoming write of n bytes, or until nothing is left to flush.
//
// The excluded key is the one being written. Flushing it would be legal but wasteful: its pending
// writes are about to be extended, so uploading them now guarantees a second upload of the same object
// moments later. Every other key is fair game.
//
// Largest first. The goal is to free bytes in as few uploads as possible, and a full-object PUT costs
// roughly the same request whether it carries 4 KiB or 4 MiB.
func (w *Writer) reclaim(ctx context.Context, exclude string, n int64) error {
	type candidate struct {
		key   string
		bytes int64
	}

	w.mu.Lock()
	cands := make([]candidate, 0, len(w.nodes))
	for k, node := range w.nodes {
		if k == exclude {
			continue
		}
		cands = append(cands, candidate{key: k, bytes: node.DirtyBytes()})
	}
	w.mu.Unlock()

	sort.Slice(cands, func(i, j int) bool { return cands[i].bytes > cands[j].bytes })

	for _, c := range cands {
		if w.maxMemory > 0 && w.Size()+n <= w.maxMemory {
			return nil
		}

		if err := w.FlushContext(ctx, c.key); err != nil {
			// A failed flush is reported rather than swallowed. The alternative — carrying on to the
			// next candidate — would turn "this object could not be stored" into "the write you just
			// made was slow", and the error naming AccessDenied is the only signal the caller gets.
			return fmt.Errorf("write path over its memory limit: flushing %q to make room failed: %w",
				c.key, err)
		}
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

// Discard drops everything buffered for key without storing it, and reports whether anything was
// dropped through its error only on misuse.
//
// This is deletion's half of the write path, and it is the one operation here that destroys user data
// on purpose. It exists because unlink has no other correct ordering: a file with pending writes that
// is deleted must not come back, and the write path is the only thing that could bring it back. A flush
// after the object is gone recreates it — at the size it had when written, with no error — so the
// pending state has to go first.
//
// It is deliberately not a flush. Flushing before deleting would store bytes nobody asked to store, pay
// for a PUT of an object about to be removed, and — if the flush failed — leave the caller unable to
// delete a file at all.
//
// Nothing buffered is not an error: unlink of a file that was never written is the common case.
func (w *Writer) Discard(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalid)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Dropping the node is the whole operation. Its pending extents, its truncation, and its dirty
	// attributes all live on it, so nothing survives that a later flush could find — and FlushAll
	// iterates this map, so a node that is not in it cannot be uploaded at unmount either.
	delete(w.nodes, key)

	return nil
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
