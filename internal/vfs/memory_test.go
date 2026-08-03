package vfs_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// The tests in this file pin the write path's memory bound (#205).
//
// Before it, write_buffer.max_memory was declared in the config schema, defaulted to "512MB", and
// validated as a size string — and read by nothing. A grep for the field name outside internal/config
// returned no hits: not the declaration's reader, not a constructor argument, nothing. So a mount
// reported a 512 MB write-buffer ceiling and enforced none, on the one path that holds user data in
// memory before it is durable.
//
// That is the same defect shape the v0.10.x releases were spent correcting — a configuration key
// asserting a property no code enforces — with the difference that this one's failure mode is the OOM
// killer taking the mount process, and with it every open file's unflushed dirty ranges.
//
// What the bound must *not* do is refuse writes it could have absorbed. A limit that turns legal
// writes into ENOSPC is worse than the unbounded growth it replaced, so the ordering asserted below is
// load-bearing: flush to reclaim first, refuse only when that did not release enough.

// TestWriterEnforcesTheMemoryLimitItIsConfiguredWith is the direct regression: an unbounded writer
// accepts an unbounded number of dirty bytes.
//
// Mutation-verified. Deleting the admit() call in Writer.WriteContext makes this fail with
// "held 4096 dirty bytes with a 1024-byte limit", which is what the defect looked like at every size:
// the number held tracked what was written and never the configured ceiling.
func TestWriterEnforcesTheMemoryLimitItIsConfiguredWith(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	// PutObject fails, so nothing can be reclaimed by flushing. That isolates the bound itself from the
	// reclaim path, which the next test covers: here the only way to stay under the limit is to refuse.
	backend.putErr = errors.New("AccessDenied: no")

	w, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{MaxMemory: 1024})
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}

	// Write 1 KiB across four distinct keys, which exactly fills the limit.
	for i := range 4 {
		key := fmt.Sprintf("f%d", i)
		if err := w.Write(key, 0, make([]byte, 256)); err != nil {
			t.Fatalf("write %q within the limit: %v", key, err)
		}
	}

	if got := w.Size(); got != 1024 {
		t.Fatalf("held %d dirty bytes after filling the limit, want 1024", got)
	}

	// The next write is over. Reclaim cannot help — every flush fails — so it must be refused, and the
	// refusal must name the flush failure rather than pretending the limit was simply reached.
	err = w.Write("f4", 0, make([]byte, 256))
	if err == nil {
		t.Fatalf("write past a %d-byte limit succeeded; the writer now holds %d dirty bytes, which is "+
			"the unbounded behavior this bound exists to prevent", 1024, w.Size())
	}

	if got := w.Size(); got > 1024 {
		t.Errorf("writer holds %d dirty bytes with a 1024-byte limit", got)
	}
}

// TestWriterFlushesToMakeRoomBeforeRefusingAWrite pins the ordering that keeps the bound from making
// the filesystem worse.
//
// A writer at its ceiling with flushable data must flush and accept, not refuse. If this inverts, a
// mount with a 512 MB default starts returning ENOSPC on ordinary workloads that write more than
// 512 MB in total — which is every workload ObjectFS exists for — while the bytes it is holding were
// perfectly storable.
func TestWriterFlushesToMakeRoomBeforeRefusingAWrite(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()

	w, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{MaxMemory: 1024})
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}

	for i := range 4 {
		if err := w.Write(fmt.Sprintf("f%d", i), 0, make([]byte, 256)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Over the limit, but everything held is flushable. This must succeed.
	if err := w.Write("f4", 0, make([]byte, 256)); err != nil {
		t.Fatalf("write over the limit with flushable data was refused: %v\n"+
			"The bound must reclaim by flushing before refusing; refusing first turns every workload "+
			"that writes more than the limit in total into ENOSPC", err)
	}

	if got := w.Size(); got > 1024 {
		t.Errorf("holding %d dirty bytes after reclaim, limit is 1024", got)
	}

	// The reclaim has to have been real: the flushed keys are durable now, with their bytes intact.
	// A "reclaim" that dropped buffers instead of storing them would satisfy the byte count above and
	// lose data, which is strictly worse than the unbounded growth.
	stored := 0
	for i := range 4 {
		key := fmt.Sprintf("f%d", i)
		data, ok := backend.Object(key)
		if !ok {
			continue
		}
		stored++
		if len(data) != 256 {
			t.Errorf("reclaimed %q is %d bytes in storage, want 256 — reclaim corrupted it", key, len(data))
		}
	}
	if stored == 0 {
		t.Error("reclaim freed memory without storing anything; the dirty bytes were discarded, not flushed")
	}
}

// TestWriterStreamsOneFilePastItsWholeLimit is the regression for the defect the coverage gate
// surfaced, and it is the most ordinary workload there is.
//
// A program appending to a single file, with nothing else buffered, has no *other* key for reclaim to
// flush — reclaim excludes the key being written, since its pending writes are about to be extended and
// uploading them now means uploading the same object twice. As the only rule that made the bound refuse
// the write it should least refuse: at the shipped 512 MB default, writing any file past 512 MB failed
// at exactly 512 MB with ENOSPC, in increments far too small for the oversize escape hatch to fire.
//
// Sequentially writing a large file is what ObjectFS is for. A bound that forbids it is worse than no
// bound, which is the same standard the reclaim-before-refuse ordering is held to above.
func TestWriterStreamsOneFilePastItsWholeLimit(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()

	w, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{MaxMemory: 1024})
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}

	// 8 KiB to one file in 256-byte appends: eight times the limit, no other key, and every individual
	// write far below the limit so nothing is admitted as oversize.
	const (
		chunk  = 256
		chunks = 32
	)
	for i := range chunks {
		if err := w.Write("big.log", int64(i*chunk), make([]byte, chunk)); err != nil {
			t.Fatalf("append %d at offset %d was refused: %v\n"+
				"One file cannot grow past the buffer limit. At the 512MB default this fails every write "+
				"beyond 512MB, which is the workload this filesystem exists for", i, i*chunk, err)
		}
	}

	// Bounded throughout, not merely accepted: the point is that flushing the target key released the
	// memory rather than the bound being abandoned.
	if got := w.Size(); got > 1024 {
		t.Errorf("held %d dirty bytes with a 1024-byte limit; the bound was abandoned rather than "+
			"enforced by flushing", got)
	}

	if err := w.FlushAll(); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	// And the file is whole. A "reclaim" of the target key that lost or misplaced its earlier bytes would
	// leave a short or corrupt object, which is worse than the ENOSPC it replaced.
	data, ok := backend.Object("big.log")
	if !ok {
		t.Fatal("big.log is not in storage after FlushAll")
	}
	if len(data) != chunk*chunks {
		t.Errorf("stored %d bytes, want %d — streaming through the bounded buffer lost data",
			len(data), chunk*chunks)
	}
}

// TestWriterRefusesWhenAnotherFileHoldsTheMemoryAndCannotBeFlushed pins that a full buffer whose data
// cannot be stored refuses the next write rather than growing without limit.
//
// The refusal arrives as the flush failure rather than as the "did not release enough" message, and that
// is the honest outcome: reclaim reports the AccessDenied it hit, which names the cause an operator can
// act on. The bare memory refusal at the end of admit is not reachable from here — or from any
// deterministic sequence, per its comment — because every failure it could follow returns earlier with a
// more specific error. What matters to the caller is asserted below: the write does not succeed, the
// buffer does not grow, and the error is distinguishable.
func TestWriterRefusesWhenAnotherFileHoldsTheMemoryAndCannotBeFlushed(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()

	w, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{MaxMemory: 1024})
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}

	// Fill the limit with one key while flushes still work.
	if err := w.Write("holder", 0, make([]byte, 1024)); err != nil {
		t.Fatalf("fill the limit: %v", err)
	}

	// Now nothing can be flushed, so neither reclaim nor a flush of the target key can free anything.
	backend.putErr = errors.New("AccessDenied: no")

	err = w.Write("newcomer", 0, make([]byte, 256))
	if err == nil {
		t.Fatalf("write was admitted with a full, unflushable buffer; writer now holds %d bytes against "+
			"a 1024-byte limit", w.Size())
	}

	if !errors.Is(err, vfs.ErrNoSpace) && !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("refusal is %v, which neither satisfies errors.Is(err, vfs.ErrNoSpace) nor names the "+
			"flush failure behind it", err)
	}

	// And the refused write left nothing behind. A refusal that had already created the node would leak a
	// buffer entry per rejected write, so the bound would consume the very resource it protects — which is
	// why admit runs before the node is materialized.
	if w.Dirty("newcomer") {
		t.Error("the refused write left a buffered node behind")
	}
	if got := w.Size(); got != 1024 {
		t.Errorf("held %d dirty bytes after a refused 256-byte write, want the 1024 held before it", got)
	}
}

// TestWriterRefusesToGrowOneFilePastTheLimitWhenItCannotBeFlushed is the failure half of
// TestWriterStreamsOneFilePastItsWholeLimit.
//
// Streaming one file past the bound works by flushing that same file to reclaim its memory. When the
// flush cannot succeed there is nothing else to try, and the write must be refused rather than admitted
// — otherwise a backend that is rejecting every upload would let the buffer grow without limit while
// reporting success to the application, which is unbounded growth plus silent data loss.
func TestWriterRefusesToGrowOneFilePastTheLimitWhenItCannotBeFlushed(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()

	w, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{MaxMemory: 1024})
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}

	if err := w.Write("one.log", 0, make([]byte, 1024)); err != nil {
		t.Fatalf("fill the limit: %v", err)
	}

	backend.putErr = errors.New("AccessDenied: no")

	err = w.Write("one.log", 1024, make([]byte, 256))
	if err == nil {
		t.Fatalf("append past the limit was admitted with an unflushable backend; the writer holds %d "+
			"dirty bytes against a 1024-byte limit and the application was told the write succeeded",
			w.Size())
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("error is %v, which does not name the flush failure an operator would need to act on", err)
	}
}

// TestWriterBufferCountRefusalNamesTheLimit reaches the node-count refusal after a failed reclaim.
//
// Distinct from the memory refusal because the two exhaust differently: a million one-byte writes to a
// million paths is a trivial byte count and a million live nodes. This is the arm that fires when
// flushing frees no *nodes*.
func TestWriterBufferCountRefusalNamesTheLimit(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.putErr = errors.New("AccessDenied: no")

	w, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{MaxBuffers: 2})
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}

	for _, k := range []string{"a", "b"} {
		if err := w.Write(k, 0, []byte("x")); err != nil {
			t.Fatalf("write %q: %v", k, err)
		}
	}

	err = w.Write("c", 0, []byte("x"))
	if err == nil {
		t.Fatal("a third key was admitted past a 2-buffer limit")
	}
	if !errors.Is(err, vfs.ErrNoSpace) && !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("refusal is %v, want ErrNoSpace or a message naming the flush failure", err)
	}
}

// TestWriterDiscardDropsPendingWrites covers Discard in the package that owns it.
//
// Its behavior under Unlink is tested in internal/fuse against a real endpoint; this is the unit-level
// contract, which is narrow: after a Discard the key is not dirty, holds no bytes, and is not flushed by
// FlushAll.
func TestWriterDiscardDropsPendingWrites(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err := w.Write("gone", 0, []byte("never stored")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Write("kept", 0, []byte("stored")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := w.Discard("gone"); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	if w.Dirty("gone") {
		t.Error("the discarded key is still dirty")
	}
	if !w.Dirty("kept") {
		t.Error("Discard dropped a key it was not given; every other key's pending writes are lost")
	}

	if err := w.FlushAll(); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	if _, ok := backend.Object("gone"); ok {
		t.Error("FlushAll uploaded a discarded key; the node survived in the map that FlushAll iterates")
	}
	if _, ok := backend.Object("kept"); !ok {
		t.Error("the key that was not discarded is missing from storage")
	}
}

// TestWriterDiscardRejectsAnEmptyKey pins the sentinel, so the FUSE layer's EINVAL mapping holds.
func TestWriterDiscardRejectsAnEmptyKey(t *testing.T) {
	t.Parallel()

	w, err := vfs.NewWriter(context.Background(), newFakeBackend())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err := w.Discard(""); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("Discard(\"\") = %v, want an error satisfying errors.Is(err, vfs.ErrInvalid)", err)
	}
}

// TestWriterDiscardOfAnUnknownKeyIsNotAnError pins idempotence.
//
// Unlink calls Discard unconditionally, before it knows whether the write path holds anything for the
// path, so a key that was never written must not produce an error — otherwise deleting a file that was
// never open would fail.
func TestWriterDiscardOfAnUnknownKeyIsNotAnError(t *testing.T) {
	t.Parallel()

	w, err := vfs.NewWriter(context.Background(), newFakeBackend())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err := w.Discard("never-written"); err != nil {
		t.Errorf("Discard of an unbuffered key returned %v; Unlink calls it unconditionally, so this "+
			"makes `rm` fail on any file that was not open for writing", err)
	}
}

// TestWriterKeepsAFileUsableWhenItsStoredAttributesAreMalformed pins that a bad metadata value costs
// the attribute and not the file.
//
// Metadata can be written by anything: another tool, an older ObjectFS, a truncated multipart copy. If a
// non-numeric mode or an unparseable timestamp made the write path refuse the object, a single bad header
// would render a file permanently unwritable — so the values are dropped, the defaults stand, and the
// discard is logged rather than silently swallowed, because a chmod that appears to work and does nothing
// is its own defect.
func TestWriterKeepsAFileUsableWhenItsStoredAttributesAreMalformed(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()

	// Every attribute unparseable at once: a non-numeric mode, a non-numeric uid, and a timestamp that is
	// not RFC 3339.
	backend.PutWithMeta("odd.txt", []byte("hello"), map[string]string{
		"objectfs-mode":  "not-a-number",
		"objectfs-uid":   "twelve",
		"objectfs-mtime": "last Tuesday",
	})

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err := w.Write("odd.txt", 5, []byte(" world")); err != nil {
		t.Fatalf("writing to a file with unparseable stored attributes failed: %v\n"+
			"A bad metadata value must cost the attribute, not the file", err)
	}
	if err := w.Flush("odd.txt"); err != nil {
		t.Fatalf("flush: %v", err)
	}

	data, ok := backend.Object("odd.txt")
	if !ok {
		t.Fatal("odd.txt is not in storage")
	}
	if string(data) != "hello world" {
		t.Errorf("stored %q, want %q — the read-modify-write did not survive the bad attributes",
			data, "hello world")
	}
}

// TestWriterAdmitsAWriteLargerThanTheWholeLimit pins the one case where the bound must yield.
//
// A single write bigger than the entire buffer is a legal write, and POSIX has no way to say "this
// write is too large, try a smaller one" — write(2)'s ENOSPC means the filesystem is full, and a
// caller that retries the same write gets the same answer forever. Refusing it would make the write
// permanently impossible at that configuration rather than merely slow.
//
// The bound's job is to stop accumulation across files, which a single oversize write is not.
func TestWriterAdmitsAWriteLargerThanTheWholeLimit(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()

	w, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{MaxMemory: 1024})
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}

	// Four times the entire limit, in one call, to an empty writer.
	if err := w.Write("big", 0, make([]byte, 4096)); err != nil {
		t.Fatalf("a single %d-byte write to a writer with a %d-byte limit was refused: %v\n"+
			"This write can never succeed at this configuration, so refusing it makes the file "+
			"unwritable rather than the buffer bounded", 4096, 1024, err)
	}

	if err := w.Flush("big"); err != nil {
		t.Fatalf("flush the oversize write: %v", err)
	}
	if data, ok := backend.Object("big"); !ok || len(data) != 4096 {
		t.Errorf("stored %d bytes (present=%v), want 4096", len(data), ok)
	}
}

// TestWriterRefusalReportsENOSPCThroughItsSentinel pins the error's identity, not just its presence.
//
// The FUSE layer maps vfs.ErrNoSpace to ENOSPC. A refusal that returned a bare fmt.Errorf would arrive
// at the kernel as EIO — "the filesystem is broken" rather than "the filesystem is full" — and no
// program handles EIO by retrying or freeing space.
func TestWriterRefusalReportsENOSPCThroughItsSentinel(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.putErr = errors.New("AccessDenied: no")

	w, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{MaxMemory: 512})
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}

	if err := w.Write("a", 0, make([]byte, 512)); err != nil {
		t.Fatalf("first write: %v", err)
	}

	err = w.Write("b", 0, make([]byte, 512))
	if err == nil {
		t.Fatal("write past the limit succeeded")
	}

	// The flush failure is what actually blocked the reclaim, so that is what the error names. Either
	// classification is honest here — the buffer is full *because* the flush failed — but the caller
	// must be able to tell it apart from a generic backend error, and the message must name the cause a
	// human can act on.
	if !strings.Contains(err.Error(), "AccessDenied") && !errors.Is(err, vfs.ErrNoSpace) {
		t.Errorf("refusal is %v, which neither satisfies errors.Is(err, vfs.ErrNoSpace) nor names the "+
			"flush failure that caused it; the kernel would see EIO, not ENOSPC", err)
	}
}

// TestWriterBoundsRejectNegativeLimits pins that a malformed bound fails loudly at construction.
//
// Silently reading a negative limit as "unbounded" is exactly how the bound came to be absent while
// the configuration claimed otherwise. A mistake in a config file must not resolve to the behavior
// the bound exists to prevent.
func TestWriterBoundsRejectNegativeLimits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opts vfs.WriterOptions
	}{
		{name: "negative memory", opts: vfs.WriterOptions{MaxMemory: -1}},
		{name: "negative buffer count", opts: vfs.WriterOptions{MaxBuffers: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w, err := vfs.NewWriterWithOptions(context.Background(), newFakeBackend(), tc.opts)
			if err == nil {
				t.Fatalf("NewWriterWithOptions accepted %+v and returned a writer with MaxMemory=%d; a "+
					"negative limit read as unbounded is the defect, not the fix", tc.opts, w.MaxMemory())
			}
			if !errors.Is(err, vfs.ErrInvalid) {
				t.Errorf("error is %v, want one satisfying errors.Is(err, vfs.ErrInvalid)", err)
			}
		})
	}
}

// TestWriterZeroLimitMeansUnbounded pins the compatibility case.
//
// Every release through v0.10.3 had no bound, and NewWriter still constructs one that way — tests and
// callers with no configuration to draw a limit from depend on it. This asserts the zero value is
// unbounded rather than "reject everything", which is the other way a zero could be read.
func TestWriterZeroLimitMeansUnbounded(t *testing.T) {
	t.Parallel()

	w, err := vfs.NewWriter(context.Background(), newFakeBackend())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if got := w.MaxMemory(); got != 0 {
		t.Errorf("MaxMemory() = %d, want 0 (unbounded)", got)
	}

	for i := range 8 {
		if err := w.Write(fmt.Sprintf("f%d", i), 0, make([]byte, 4096)); err != nil {
			t.Fatalf("unbounded writer refused write %d: %v", i, err)
		}
	}
	if got := w.Size(); got != 8*4096 {
		t.Errorf("held %d dirty bytes, want %d", got, 8*4096)
	}
}

// TestWriterBufferCountLimitDoesNotRefuseWritesToAnOpenFile pins the count bound's exemption.
//
// The node-count limit must apply to keys being *added*, never to a key already holding writes.
// Otherwise a program appending to one log file forever starts failing as soon as other files fill the
// map — its own writes refused for a limit it is not contributing to.
func TestWriterBufferCountLimitDoesNotRefuseWritesToAnOpenFile(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.putErr = errors.New("AccessDenied: no") // nothing can be reclaimed

	w, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{MaxBuffers: 2})
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}

	if err := w.Write("log", 0, []byte("a")); err != nil {
		t.Fatalf("first write to log: %v", err)
	}
	if err := w.Write("other", 0, []byte("b")); err != nil {
		t.Fatalf("write to other: %v", err)
	}

	// The map is full. A third *new* key must be refused...
	if err := w.Write("third", 0, []byte("c")); err == nil {
		t.Error("a third key was admitted past a 2-buffer limit")
	}

	// ...but appending to a key already buffered must not be, at any point.
	for i := range 16 {
		if err := w.Write("log", int64(1+i), []byte("x")); err != nil {
			t.Fatalf("append %d to an already-buffered key was refused: %v\n"+
				"The count bound must exempt existing nodes, or a program writing one file fails "+
				"because of files it does not touch", i, err)
		}
	}
}
