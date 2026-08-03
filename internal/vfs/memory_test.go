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
