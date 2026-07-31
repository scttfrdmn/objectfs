package vfs

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func testAttr(size int64) Attr {
	return Attr{
		Type:  FileTypeRegular,
		Size:  size,
		Mode:  DefaultFileMode,
		Mtime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestOpenFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		flags      OpenFlags
		canRead    bool
		canWrite   bool
		wantString string
	}{
		{flags: 0, wantString: "0"},
		{flags: OpenRead, canRead: true, wantString: "read"},
		{flags: OpenWrite, canWrite: true, wantString: "write"},
		{flags: OpenRead | OpenWrite, canRead: true, canWrite: true, wantString: "read|write"},
		{flags: OpenWrite | OpenAppend, canWrite: true, wantString: "write|append"},
		{flags: OpenWrite | OpenTruncate, canWrite: true, wantString: "write|trunc"},
		{
			flags:      OpenRead | OpenWrite | OpenAppend | OpenTruncate,
			canRead:    true,
			canWrite:   true,
			wantString: "read|write|append|trunc",
		},
		{flags: OpenRead | 1<<20, canRead: true, wantString: "read|unknown(0x100000)"},
	}

	for _, tc := range tests {
		if got := tc.flags.CanRead(); got != tc.canRead {
			t.Errorf("%v.CanRead() = %v, want %v", tc.flags, got, tc.canRead)
		}
		if got := tc.flags.CanWrite(); got != tc.canWrite {
			t.Errorf("%v.CanWrite() = %v, want %v", tc.flags, got, tc.canWrite)
		}
		if got := tc.flags.String(); got != tc.wantString {
			t.Errorf("OpenFlags(%#x).String() = %q, want %q", uint32(tc.flags), got, tc.wantString)
		}
	}
}

func TestHandleTableOpenValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		flags      OpenFlags
		attr       Attr
		storedSize int64
	}{
		{
			name:  "empty path",
			flags: OpenRead,
			attr:  testAttr(0),
		},
		{
			name:       "negative stored size",
			path:       "f",
			flags:      OpenRead,
			attr:       testAttr(0),
			storedSize: -1,
		},
		{
			name:  "neither read nor write",
			path:  "f",
			flags: 0,
			attr:  testAttr(0),
		},
		{
			// O_TRUNC without write access must not silently discard the file's contents.
			name:  "truncate without write access",
			path:  "f",
			flags: OpenRead | OpenTruncate,
			attr:  testAttr(100),
		},
		{
			name:  "append without write access",
			path:  "f",
			flags: OpenRead | OpenAppend,
			attr:  testAttr(0),
		},
		{
			name:  "directory",
			path:  "d",
			flags: OpenRead,
			attr:  Attr{Type: FileTypeDir, Mode: DefaultDirMode},
		},
		{
			name:  "invalid attr",
			path:  "f",
			flags: OpenRead,
			attr:  Attr{Type: FileTypeRegular, Size: -1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tbl := NewHandleTable()
			h, err := tbl.Open(tc.path, tc.flags, tc.attr, tc.storedSize)
			if err == nil {
				t.Fatalf("Open succeeded, want an error")
			}
			if h != nil {
				t.Errorf("Open returned a handle alongside its error")
			}
			if tbl.Len() != 0 {
				t.Errorf("failed Open left %d handles in the table", tbl.Len())
			}
			if len(tbl.Nodes()) != 0 {
				t.Errorf("failed Open left nodes in the table: %v", tbl.Nodes())
			}
		})
	}
}

func TestHandleTableOpenAndRelease(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()

	h, err := tbl.Open("a/b.txt", OpenRead|OpenWrite, testAttr(100), 100)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if h.ID == 0 {
		t.Error("handle ID is 0; IDs must be distinguishable from an unset value")
	}
	if h.Node.Path != "a/b.txt" {
		t.Errorf("node path = %q, want %q", h.Node.Path, "a/b.txt")
	}
	if tbl.Len() != 1 {
		t.Errorf("Len() = %d, want 1", tbl.Len())
	}

	got, err := tbl.Lookup(h.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != h {
		t.Error("Lookup returned a different handle")
	}

	n, last, err := tbl.Release(h.ID)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !last {
		t.Error("Release did not report the last handle")
	}
	if n != h.Node {
		t.Error("Release returned a different node")
	}
	if tbl.Len() != 0 {
		t.Errorf("Len() = %d after release, want 0", tbl.Len())
	}
	if _, ok := tbl.Node("a/b.txt"); ok {
		t.Error("clean node survived the last release")
	}

	if _, err := tbl.Lookup(h.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("Lookup of a released handle = %v, want ErrInvalid", err)
	}
	if _, _, err := tbl.Release(h.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("double Release = %v, want ErrInvalid", err)
	}
}

// Two descriptors on one path are two descriptors on one S3 object. If each buffered its own dirty
// ranges, the second flush would assemble a full-object PUT from stale bytes and silently destroy the
// first writer's data.
func TestHandleTableSharesNodeAcrossHandles(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()

	h1, err := tbl.Open("shared", OpenRead|OpenWrite, testAttr(0), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	h2, err := tbl.Open("shared", OpenRead|OpenWrite, testAttr(0), 0)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}

	if h1.Node != h2.Node {
		t.Fatal("two handles on one path got separate nodes; writes through one would be invisible to the other")
	}
	if h1.ID == h2.ID {
		t.Fatalf("both handles got ID %d", h1.ID)
	}

	if _, err := h1.Node.Write(0, []byte("written via h1"), false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// POSIX requires a read through one descriptor to see a write through another.
	buf := make([]byte, 14)
	valid, err := h2.Node.ReadInto(buf, 0, nil)
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if got := string(buf[:valid]); got != "written via h1" {
		t.Fatalf("read via h2 = %q, want the bytes written via h1", got)
	}
}

// A second open of an already-open file must not reset state the first open has dirtied.
func TestHandleTableSecondOpenPreservesDirtyState(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()

	h1, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(0), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := h1.Node.Write(0, []byte("pending"), false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Stale metadata, as a fresh HeadObject of the not-yet-flushed object would give.
	h2, err := tbl.Open("f", OpenRead, testAttr(0), 0)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}

	if !h2.Node.Dirty() {
		t.Fatal("second Open cleared the node's dirty state")
	}
	if got := h2.Node.Size(); got != 7 {
		t.Fatalf("Size = %d after a second open, want 7", got)
	}
}

// Releasing the last handle on a dirty node must keep the node: the writes still have to go
// somewhere, and close(2) losing them is exactly the failure this table exists to prevent.
func TestHandleTableReleaseKeepsDirtyNode(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()

	h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(0), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := h.Node.Write(0, []byte("unflushed"), false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	n, last, err := tbl.Release(h.ID)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !last {
		t.Error("Release did not report the last handle")
	}
	if _, ok := tbl.Node("f"); !ok {
		t.Fatal("dirty node was dropped on release; its writes are now unreachable")
	}
	if !n.Dirty() {
		t.Fatal("node is not dirty after an unflushed write")
	}

	// Forget must refuse while the node is dirty — a failed flush must not become invisible.
	if tbl.Forget("f") {
		t.Fatal("Forget dropped a dirty node")
	}

	gen := n.Generation()
	if !n.MarkFlushed(gen, 9, "etag") {
		t.Fatal("MarkFlushed rejected an unraced flush")
	}
	if !tbl.Forget("f") {
		t.Fatal("Forget refused a clean, closed node")
	}
	if _, ok := tbl.Node("f"); ok {
		t.Fatal("node survived Forget")
	}
	if tbl.Forget("f") {
		t.Fatal("Forget of an absent node returned true")
	}
}

func TestHandleTableForgetRefusesOpenNode(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	if _, err := tbl.Open("f", OpenRead, testAttr(0), 0); err != nil {
		t.Fatalf("Open: %v", err)
	}

	if tbl.Forget("f") {
		t.Fatal("Forget dropped a node with an open handle")
	}
}

// Handle IDs must never be reused: the kernel supplies the ID, so a stale one addressing a live
// handle is a use-after-free reachable from userspace.
func TestHandleTableIDsAreNotReused(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	seen := make(map[uint64]bool)

	for i := range 100 {
		h, err := tbl.Open("f", OpenRead, testAttr(0), 0)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if seen[h.ID] {
			t.Fatalf("handle ID %d reused at iteration %d", h.ID, i)
		}
		seen[h.ID] = true

		if _, _, err := tbl.Release(h.ID); err != nil {
			t.Fatalf("Release %d: %v", i, err)
		}
	}
}

func TestNodeOpenTruncate(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()

	h, err := tbl.Open("f", OpenWrite|OpenTruncate, testAttr(100), 100)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if got := h.Node.Size(); got != 0 {
		t.Fatalf("Size = %d after an O_TRUNC open, want 0", got)
	}
	if !h.Node.Dirty() {
		t.Fatal("O_TRUNC open of a 100-byte object left the node clean; the truncation would never be written")
	}

	// The plan must replace the object wholesale, not read from it.
	p, _, err := h.Node.FlushPlan()
	if err != nil {
		t.Fatalf("FlushPlan: %v", err)
	}
	if !p.WholeObject || p.Size != 0 {
		t.Fatalf("plan = %+v, want a whole-object write of 0 bytes", p)
	}
}

func TestNodeAppend(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenWrite|OpenAppend, testAttr(10), 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// O_APPEND ignores the supplied offset; a deliberately wrong one must not land there.
	if _, err := h.Node.Write(0, []byte("AAA"), true); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := h.Node.Size(); got != 13 {
		t.Fatalf("Size = %d after appending 3 bytes to a 10-byte file, want 13", got)
	}

	if _, err := h.Node.Write(0, []byte("BB"), true); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if got := h.Node.Size(); got != 15 {
		t.Fatalf("Size = %d after a second append, want 15", got)
	}

	p, _, err := h.Node.FlushPlan()
	if err != nil {
		t.Fatalf("FlushPlan: %v", err)
	}
	if p.WholeObject {
		t.Fatal("append to an existing object planned a whole-object write; the first 10 bytes would be lost")
	}
	if len(p.ReadRanges) != 1 || p.ReadRanges[0] != (Range{Offset: 0, Length: 10}) {
		t.Fatalf("ReadRanges = %v, want the whole 10-byte prefix", p.ReadRanges)
	}
}

func TestNodeAttrAndSetAttr(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(50), 50)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	n := h.Node

	if got := n.Attr(); got.Mode != DefaultFileMode || got.Size != 50 {
		t.Fatalf("Attr = %+v, want mode %#o and size 50", got, DefaultFileMode)
	}
	if n.Dirty() {
		t.Fatal("a freshly opened, unwritten node is dirty")
	}

	newMtime := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := n.SetAttr(true, true, true, Attr{Mode: 0o600, UID: 1001, GID: 2002, Mtime: newMtime}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	got := n.Attr()
	if got.Mode != 0o600 || got.UID != 1001 || got.GID != 2002 || !got.Mtime.Equal(newMtime) {
		t.Fatalf("Attr = %+v after SetAttr", got)
	}
	if !got.Ctime.Equal(newMtime) {
		t.Errorf("Ctime = %v, want it to track Mtime", got.Ctime)
	}

	// A chmod with no write is still work. An fsync that reports success here has lied.
	if !n.Dirty() {
		t.Fatal("node is clean after SetAttr; the attribute change would never be written")
	}
}

func TestNodeSetAttrSelective(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(0), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	n := h.Node

	if err := n.SetAttr(true, false, false, Attr{Mode: 0o700, UID: 9999, GID: 9999}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	got := n.Attr()
	if got.Mode != 0o700 {
		t.Errorf("Mode = %#o, want 0700", got.Mode)
	}
	if got.UID != 0 || got.GID != 0 {
		t.Errorf("UID/GID = %d/%d; unselected fields were applied anyway", got.UID, got.GID)
	}
}

func TestNodeSetAttrRejectsTypeBits(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(0), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	err = h.Node.SetAttr(true, false, false, Attr{Mode: 1 << 20})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("SetAttr with non-permission bits = %v, want ErrInvalid", err)
	}
	if h.Node.Dirty() {
		t.Fatal("rejected SetAttr still marked the node dirty")
	}
}

// Size must reflect pending writes, not the stored object. A stat that reports the stored size makes
// the kernel truncate reads of the bytes just written.
func TestNodeSizeReflectsPendingState(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(100), 100)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	n := h.Node

	if got := n.Size(); got != 100 {
		t.Fatalf("Size = %d, want 100", got)
	}
	if got := n.StoredSize(); got != 100 {
		t.Fatalf("StoredSize = %d, want 100", got)
	}

	if _, err := n.Write(200, []byte("XX"), false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := n.Size(); got != 202 {
		t.Fatalf("Size = %d after writing past the end, want 202", got)
	}
	if got := n.StoredSize(); got != 100 {
		t.Fatalf("StoredSize = %d; it must not move until the flush lands", got)
	}
	if got := n.Attr().Size; got != 202 {
		t.Fatalf("Attr().Size = %d, want 202", got)
	}

	if err := n.Truncate(50); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if got := n.Size(); got != 50 {
		t.Fatalf("Size = %d after truncating to 50, want 50", got)
	}
}

func TestNodeReadRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		storedSize int64
		ops        func(*Node) error
		offset     int64
		length     int
		wantFetch  bool
		wantRange  Range
	}{
		{
			name:       "clean read fetches the requested range",
			storedSize: 100,
			offset:     10,
			length:     20,
			wantFetch:  true,
			wantRange:  Range{Offset: 10, Length: 20},
		},
		{
			name:       "read is clipped to the stored object",
			storedSize: 15,
			offset:     10,
			length:     20,
			wantFetch:  true,
			wantRange:  Range{Offset: 10, Length: 5},
		},
		{
			name:       "read entirely past the stored object needs no fetch",
			storedSize: 10,
			offset:     50,
			length:     20,
			wantFetch:  false,
		},
		{
			name:       "new object needs no fetch",
			storedSize: 0,
			offset:     0,
			length:     20,
			wantFetch:  false,
		},
		{
			// Read-after-write on a brand-new file must not issue a GET for an object that does not
			// exist, which is where v0.10.0's Lookup-to-ENOENT collapse did its damage.
			name:       "write to a new object then read needs no fetch",
			storedSize: 0,
			ops:        func(n *Node) error { _, err := n.Write(0, []byte("data"), false); return err },
			offset:     0,
			length:     4,
			wantFetch:  false,
		},
		{
			name:       "truncation shrinks what may be fetched",
			storedSize: 100,
			ops:        func(n *Node) error { return n.Truncate(20) },
			offset:     0,
			length:     100,
			wantFetch:  true,
			wantRange:  Range{Offset: 0, Length: 20},
		},
		{
			name:       "read past a truncation needs no fetch",
			storedSize: 100,
			ops:        func(n *Node) error { return n.Truncate(20) },
			offset:     30,
			length:     10,
			wantFetch:  false,
		},
		{
			name:       "zero-length read needs no fetch",
			storedSize: 100,
			offset:     0,
			length:     0,
			wantFetch:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tbl := NewHandleTable()
			h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(tc.storedSize), tc.storedSize)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if tc.ops != nil {
				if err := tc.ops(h.Node); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}

			r, fetch, err := h.Node.ReadRange(tc.offset, tc.length)
			if err != nil {
				t.Fatalf("ReadRange: %v", err)
			}
			if fetch != tc.wantFetch {
				t.Fatalf("fetch = %v, want %v (range %+v)", fetch, tc.wantFetch, r)
			}
			if fetch && r != tc.wantRange {
				t.Fatalf("range = %+v, want %+v", r, tc.wantRange)
			}
		})
	}
}

func TestNodeReadRangeRejectsBadArgs(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenRead, testAttr(100), 100)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, _, err := h.Node.ReadRange(-1, 10); !errors.Is(err, ErrInvalid) {
		t.Errorf("ReadRange(-1, 10) = %v, want ErrInvalid", err)
	}
	// The negative-length case is C3: v0.10.0 panicked on data[offset:offset+size] with size < 0,
	// killing the mount and every open descriptor with it.
	if _, _, err := h.Node.ReadRange(0, -1); !errors.Is(err, ErrInvalid) {
		t.Errorf("ReadRange(0, -1) = %v, want ErrInvalid", err)
	}
}

// Every mutating entry point must reject a nonsensical argument with ErrInvalid rather than
// corrupting its state. These arrive from the kernel as int64 and are not all trustworthy.
func TestNodeRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(*Node) error
	}{
		{
			name: "write at a negative offset",
			call: func(n *Node) error { _, err := n.Write(-1, []byte("x"), false); return err },
		},
		{
			name: "truncate to a negative size",
			call: func(n *Node) error { return n.Truncate(-1) },
		},
		{
			name: "splice to a negative size",
			call: func(n *Node) error { _, err := n.Splice(-1, nil); return err },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tbl := NewHandleTable()
			h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(10), 10)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			if err := tc.call(h.Node); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want ErrInvalid", err)
			}
			// The path must be named: an ErrInvalid with no context is useless in a mount log.
			if err := tc.call(h.Node); err != nil && !bytes.Contains([]byte(err.Error()), []byte(`"f"`)) {
				t.Errorf("error %q does not name the file", err)
			}
			if h.Node.Dirty() {
				t.Error("a rejected operation left the node dirty")
			}
		})
	}
}

func TestNodeReadIntoRejectsNegativeOffset(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenRead, testAttr(100), 100)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := h.Node.ReadInto(make([]byte, 10), -1, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("ReadInto(offset=-1) = %v, want ErrInvalid", err)
	}
}

// An object can be replaced behind our back and come back shorter than its recorded size. Trusting
// storedSize over what actually arrived would hand the kernel uninitialised buffer as file content.
func TestNodeReadIntoToleratesShortStoredRead(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenRead, testAttr(100), 100)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	buf := bytes.Repeat([]byte{0xFE}, 100)
	valid, err := h.Node.ReadInto(buf, 0, []byte("short"))
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if valid != 5 {
		t.Fatalf("valid = %d, want 5 — the object supplied only 5 bytes", valid)
	}
	if got := string(buf[:valid]); got != "short" {
		t.Fatalf("content = %q, want %q", got, "short")
	}
}

// The full read path, as the FUSE shim will drive it: ask what to fetch, fetch it, read through the
// pending writes. Each case asserts the bytes a real filesystem would return.
func TestNodeReadPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stored string
		ops    func(*Node) error
		offset int64
		length int
		want   string
	}{
		{
			name:   "clean read",
			stored: "0123456789",
			length: 10,
			want:   "0123456789",
		},
		{
			name:   "read at an offset",
			stored: "0123456789",
			offset: 4,
			length: 3,
			want:   "456",
		},
		{
			name:   "read is short at EOF",
			stored: "0123456789",
			offset: 8,
			length: 10,
			want:   "89",
		},
		{
			name:   "read past EOF is empty",
			stored: "0123456789",
			offset: 100,
			length: 10,
			want:   "",
		},
		{
			// H5: read-after-write must see the new bytes, not the pre-write content the cache holds.
			name:   "read-after-write sees the written bytes",
			stored: "0123456789",
			ops:    func(n *Node) error { _, err := n.Write(2, []byte("XX"), false); return err },
			length: 10,
			want:   "01XX456789",
		},
		{
			name:   "read spans a write and the stored object",
			stored: "0123456789",
			ops:    func(n *Node) error { _, err := n.Write(8, []byte("ABCD"), false); return err },
			length: 12,
			want:   "01234567ABCD",
		},
		{
			name:   "sparse write leaves a hole of zeros",
			stored: "AB",
			ops:    func(n *Node) error { _, err := n.Write(6, []byte("YZ"), false); return err },
			length: 8,
			want:   "AB\x00\x00\x00\x00YZ",
		},
		{
			name:   "truncation shortens the read",
			stored: "0123456789",
			ops:    func(n *Node) error { return n.Truncate(4) },
			length: 10,
			want:   "0123",
		},
		{
			name:   "growing back after a truncation reads zeros, not the old bytes",
			stored: "0123456789",
			ops: func(n *Node) error {
				if err := n.Truncate(4); err != nil {
					return err
				}
				_, err := n.Write(8, []byte("ZZ"), false)
				return err
			},
			length: 10,
			want:   "0123\x00\x00\x00\x00ZZ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stored := []byte(tc.stored)
			tbl := NewHandleTable()
			h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(int64(len(stored))), int64(len(stored)))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if tc.ops != nil {
				if err := tc.ops(h.Node); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}

			got := readViaNode(t, h.Node, stored, tc.offset, tc.length)
			if string(got) != tc.want {
				t.Fatalf("read(off=%d len=%d) = %q, want %q", tc.offset, tc.length, got, tc.want)
			}
		})
	}
}

// readViaNode drives the read path end to end against an in-memory object.
func readViaNode(t *testing.T, n *Node, stored []byte, offset int64, length int) []byte {
	t.Helper()

	r, fetch, err := n.ReadRange(offset, length)
	if err != nil {
		t.Fatalf("ReadRange(%d, %d): %v", offset, length, err)
	}

	var fetched []byte
	if fetch {
		if r.Offset < 0 || r.End() > int64(len(stored)) {
			t.Fatalf("ReadRange asked for %+v, outside the %d-byte object", r, len(stored))
		}
		fetched = stored[r.Offset:r.End()]
	}

	// Poisoned so any byte reported valid without being written shows up.
	buf := bytes.Repeat([]byte{0xFE}, length)
	valid, err := n.ReadInto(buf, offset, fetched)
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if valid < 0 || valid > length {
		t.Fatalf("ReadInto returned valid=%d, outside [0,%d]", valid, length)
	}
	return buf[:valid]
}

// The full write path: plan, fetch what it demands, splice, mark flushed. Each case asserts the object
// body that would be uploaded — the thing v0.10.0 got wrong six different ways.
func TestNodeFlushPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stored string
		ops    func(*Node) error
		want   string
	}{
		{
			name: "new file",
			ops:  func(n *Node) error { _, err := n.Write(0, []byte("hello"), false); return err },
			want: "hello",
		},
		{
			// H7, the headline defect: v0.10.0 PUT only the appended byte, so the object became 1 byte.
			name:   "append preserves the existing content",
			stored: "0123456789",
			ops:    func(n *Node) error { _, err := n.Write(10, []byte("A"), false); return err },
			want:   "0123456789A",
		},
		{
			name:   "in-place write preserves head and tail",
			stored: "0123456789",
			ops:    func(n *Node) error { _, err := n.Write(4, []byte("XX"), false); return err },
			want:   "0123XX6789",
		},
		{
			// The mergeWrites defect: shorter new content over longer old content was discarded.
			name:   "shorter overwrite of a longer file",
			stored: "OLDCONTENT",
			ops: func(n *Node) error {
				if err := n.Truncate(0); err != nil {
					return err
				}
				_, err := n.Write(0, []byte("NEW"), false)
				return err
			},
			want: "NEW",
		},
		{
			// H8: v0.10.0 returned EIO for this, the pattern SQLite and mmap writeback use.
			name:   "non-contiguous writes",
			stored: "0123456789",
			ops: func(n *Node) error {
				if _, err := n.Write(0, []byte("AA"), false); err != nil {
					return err
				}
				_, err := n.Write(8, []byte("BB"), false)
				return err
			},
			want: "AA234567BB",
		},
		{
			name:   "truncation shortens the object",
			stored: "0123456789",
			ops:    func(n *Node) error { return n.Truncate(4) },
			want:   "0123",
		},
		{
			name:   "truncation then a write past the old end",
			stored: "0123456789",
			ops: func(n *Node) error {
				if err := n.Truncate(4); err != nil {
					return err
				}
				_, err := n.Write(6, []byte("ZZ"), false)
				return err
			},
			want: "0123\x00\x00ZZ",
		},
		{
			name:   "growing by truncation appends zeros",
			stored: "AB",
			ops:    func(n *Node) error { return n.Truncate(6) },
			want:   "AB\x00\x00\x00\x00",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stored := []byte(tc.stored)
			tbl := NewHandleTable()
			h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(int64(len(stored))), int64(len(stored)))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			n := h.Node

			if tc.ops != nil {
				if err := tc.ops(n); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}

			body, ok := flushViaNode(t, n, stored)
			if !ok {
				t.Fatal("FlushPlan reported no work, but the file should have changed")
			}
			if string(body) != tc.want {
				t.Fatalf("object body = %q, want %q", body, tc.want)
			}

			if n.Dirty() {
				t.Error("node is still dirty after a successful flush")
			}
			if got := n.StoredSize(); got != int64(len(body)) {
				t.Errorf("StoredSize = %d after flush, want %d", got, len(body))
			}

			// The flushed object must read back as the file did before the flush.
			after := readViaNode(t, n, body, 0, len(body)+8)
			if !bytes.Equal(after, body) {
				t.Errorf("post-flush read = %q, want %q", after, body)
			}
		})
	}
}

// flushViaNode drives the flush path and returns the object body that would be uploaded.
func flushViaNode(t *testing.T, n *Node, stored []byte) ([]byte, bool) {
	t.Helper()

	gen := n.Generation()
	p, attr, err := n.FlushPlan()
	if err != nil {
		t.Fatalf("FlushPlan: %v", err)
	}
	if p.Noop {
		return nil, false
	}
	if attr.Size != p.Size {
		t.Fatalf("plan Size %d disagrees with attr Size %d", p.Size, attr.Size)
	}

	var base []Extent
	for _, r := range p.ReadRanges {
		if r.Offset < 0 || r.End() > int64(len(stored)) {
			t.Fatalf("plan asked for %+v, outside the %d-byte object", r, len(stored))
		}
		base = append(base, Extent{Offset: r.Offset, Data: stored[r.Offset:r.End()]})
	}
	if p.WholeObject && len(base) != 0 {
		t.Fatal("plan claims a whole-object write but demanded reads")
	}

	body, err := n.Splice(p.Size, base)
	if err != nil {
		t.Fatalf("Splice: %v", err)
	}
	if int64(len(body)) != p.Size {
		t.Fatalf("Splice produced %d bytes, plan said %d", len(body), p.Size)
	}

	if !n.MarkFlushed(gen, int64(len(body)), "etag-flushed") {
		t.Fatal("MarkFlushed rejected an unraced flush")
	}
	return body, true
}

func TestNodeFlushPlanNoopWhenClean(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(100), 100)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	p, _, err := h.Node.FlushPlan()
	if err != nil {
		t.Fatalf("FlushPlan: %v", err)
	}
	if !p.Noop {
		t.Fatalf("plan = %+v for a clean node, want Noop", p)
	}
	// Conflating Noop with a whole-object write would PUT zero bytes over an intact 100-byte object.
	if p.WholeObject {
		t.Fatal("clean node planned a whole-object write")
	}
	if p.Size != 100 {
		t.Errorf("plan Size = %d, want 100", p.Size)
	}
}

// v0.10.0's flush deleted its write buffer on success without checking whether anything had arrived
// meanwhile, so a write concurrent with an upload was discarded and counted as flushed. The
// generation check is what makes that detectable.
func TestNodeMarkFlushedRejectsRacedWrite(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(0), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	n := h.Node

	if _, err := n.Write(0, []byte("first"), false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	gen := n.Generation()
	if _, _, err := n.FlushPlan(); err != nil {
		t.Fatalf("FlushPlan: %v", err)
	}

	// A write lands while the upload is in flight.
	if _, err := n.Write(5, []byte("second"), false); err != nil {
		t.Fatalf("racing Write: %v", err)
	}

	if n.MarkFlushed(gen, 5, "etag") {
		t.Fatal("MarkFlushed accepted a flush that a write raced; the racing write would be lost")
	}
	if !n.Dirty() {
		t.Fatal("node is clean after a rejected flush")
	}
	if got := n.Size(); got != 11 {
		t.Fatalf("Size = %d, want 11 — the racing write must survive", got)
	}

	// Retrying with the current generation succeeds and flushes everything.
	body, ok := flushViaNode(t, n, nil)
	if !ok {
		t.Fatal("retry planned no work")
	}
	if string(body) != "firstsecond" {
		t.Fatalf("retried flush body = %q, want %q", body, "firstsecond")
	}
}

func TestNodeMarkFlushedRejectsRacedTruncate(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(0), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	n := h.Node

	if _, err := n.Write(0, []byte("0123456789"), false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	gen := n.Generation()
	if err := n.Truncate(4); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	if n.MarkFlushed(gen, 10, "etag") {
		t.Fatal("MarkFlushed accepted a flush that a truncate raced")
	}
}

func TestNodeMarkFlushedClearsDirtyAttr(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(10), 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	n := h.Node

	if err := n.SetAttr(true, false, false, Attr{Mode: 0o600}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}
	if !n.Dirty() {
		t.Fatal("node is clean after SetAttr")
	}

	if !n.MarkFlushed(n.Generation(), 10, "etag-2") {
		t.Fatal("MarkFlushed rejected an unraced attribute flush")
	}
	if n.Dirty() {
		t.Fatal("node is still dirty after flushing the attribute change")
	}
	if got := n.Attr(); got.Mode != 0o600 || got.ETag != "etag-2" {
		t.Fatalf("Attr = %+v after flush, want mode 0600 and the new etag", got)
	}
}

func TestHandleTableDirtyNodes(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()

	clean, err := tbl.Open("clean", OpenRead|OpenWrite, testAttr(10), 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	written, err := tbl.Open("written", OpenRead|OpenWrite, testAttr(10), 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	chmodded, err := tbl.Open("chmodded", OpenRead|OpenWrite, testAttr(10), 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := written.Node.Write(0, []byte("X"), false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := chmodded.Node.SetAttr(true, false, false, Attr{Mode: 0o600}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	if got := len(tbl.Nodes()); got != 3 {
		t.Fatalf("Nodes() returned %d paths, want 3", got)
	}

	dirty := tbl.DirtyNodes()
	if len(dirty) != 2 {
		t.Fatalf("DirtyNodes() returned %d nodes, want 2: %v", len(dirty), nodePaths(dirty))
	}
	got := map[string]bool{}
	for _, n := range dirty {
		got[n.Path] = true
	}
	if !got["written"] || !got["chmodded"] {
		t.Errorf("DirtyNodes() = %v, want written and chmodded", nodePaths(dirty))
	}
	if got["clean"] {
		t.Error("DirtyNodes() included a clean node")
	}
	_ = clean
}

func nodePaths(nodes []*Node) []string {
	paths := make([]string, len(nodes))
	for i, n := range nodes {
		paths[i] = n.Path
	}
	return paths
}

// FuzzNodeLifecycle drives a Node the way the FUSE shim will — open, write, truncate, read, flush,
// repeat — against a simulated object store and a byte-map model of the file.
//
// This is the seam FuzzExtentList cannot see. The extent list can be perfect while the node drops
// the offset on the way to the store, or clears its pending state after a partial flush, or reports
// a size the object does not have. Every one of those is a real v0.10.0 defect, and every one lives
// in the composition rather than in either part. Flushing mid-sequence is what makes it a seam test:
// the model must agree across the boundary, not just at the end.
func FuzzNodeLifecycle(f *testing.F) {
	f.Add([]byte{0, 0, 4, 'a', 'b', 'c', 'd'})
	f.Add([]byte{0, 10, 1, 'X'})               // append past the end (H7)
	f.Add([]byte{0, 0, 2, 'A', 'B', 4, 0, 0})  // write then flush
	f.Add([]byte{3, 4, 0, 0, 20, 2, 'Z', 'Z'}) // truncate down, then grow back (H8)
	f.Add([]byte{0, 0, 3, 'a', 'b', 'c', 4, 0, 0, 0, 6, 2, 'd', 'e', 4, 0, 0})
	f.Add([]byte("200")) // zero-length write past the end must extend nothing

	f.Fuzz(func(t *testing.T, in []byte) {
		// The object as it exists in the store, and the file as a real filesystem would hold it.
		// They start equal and must end equal after the last flush.
		object := []byte("0123456789abcdefghij")
		file := append([]byte(nil), object...)

		tbl := NewHandleTable()
		h, err := tbl.Open("f", OpenRead|OpenWrite, testAttr(int64(len(object))), int64(len(object)))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		n := h.Node

		// Each op is [kind, offset, length, data...]; a malformed tail simply ends the sequence.
		for len(in) >= 3 {
			kind, offset, length := in[0], int64(in[1]), int(in[2])
			in = in[3:]

			switch kind % 5 {
			case 3: // truncate
				if err := n.Truncate(offset); err != nil {
					t.Fatalf("Truncate(%d): %v", offset, err)
				}
				file = resizeModel(file, offset)

			case 4: // flush
				body, flushed := fuzzFlush(t, n, object)
				if flushed {
					object = body
				}
				// Whether or not there was work to do, the store must now agree with the model.
				if !bytes.Equal(object, file) {
					t.Fatalf("after flush the object is %q, the file should be %q", object, file)
				}

			default: // write
				if length > len(in) {
					length = len(in)
				}
				data := in[:length]
				in = in[length:]

				got, err := n.Write(offset, data, false)
				if err != nil {
					t.Fatalf("Write(%d, %d bytes): %v", offset, len(data), err)
				}
				if got != len(data) {
					t.Fatalf("Write accepted %d of %d bytes; no legitimate write may be short", got, len(data))
				}
				// A zero-length write has no effect, including no extension — POSIX write(2) with
				// nbyte == 0 on a regular file "shall return zero and have no other results". Growing
				// the file here would make the model wrong, not the implementation.
				if len(data) > 0 {
					if end := offset + int64(len(data)); end > int64(len(file)) {
						file = resizeModel(file, end)
					}
					copy(file[offset:], data)
				}
			}

			// The size the kernel is told must be the size the file has, at every step. Reporting the
			// stored object's length instead is what made reads of just-written bytes come back short.
			if got := n.Size(); got != int64(len(file)) {
				t.Fatalf("Size = %d, model file is %d bytes", got, len(file))
			}
			if got := n.Attr().Size; got != int64(len(file)) {
				t.Fatalf("Attr().Size = %d, model file is %d bytes", got, len(file))
			}

			// Read back through the pending state at a few offsets, including past the end.
			for _, off := range []int64{0, 1, int64(len(file)) / 2, int64(len(file)), int64(len(file)) + 3} {
				if off < 0 {
					continue
				}
				got := readViaNode(t, n, object, off, len(file)+4)
				if want := modelRead(file, off, len(file)+4); !bytes.Equal(got, want) {
					t.Fatalf("read(off=%d) = %q, model says %q\nobject %q\nfile %q",
						off, got, want, object, file)
				}
			}
		}

		// A final flush must make the store equal the model. If it does not, the sequence lost data —
		// which is the whole point of the exercise.
		if body, flushed := fuzzFlush(t, n, object); flushed {
			object = body
		}
		if !bytes.Equal(object, file) {
			t.Fatalf("final object is %q, want %q", object, file)
		}
		if n.Dirty() {
			t.Fatal("node is still dirty after a successful final flush")
		}

		// And with everything durable, the node must be releasable and forgettable — a node the table
		// can never drop is a leak that grows with every file touched.
		if _, _, err := tbl.Release(h.ID); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if _, ok := tbl.Node("f"); ok {
			t.Fatal("a clean node survived its last release")
		}
	})
}

// fuzzFlush runs one flush cycle against an in-memory object, returning the new object body.
func fuzzFlush(t *testing.T, n *Node, object []byte) ([]byte, bool) {
	t.Helper()

	gen := n.Generation()
	p, attr, err := n.FlushPlan()
	if err != nil {
		t.Fatalf("FlushPlan: %v", err)
	}
	if p.Noop {
		return nil, false
	}
	if attr.Size != p.Size {
		t.Fatalf("plan Size %d disagrees with attr Size %d", p.Size, attr.Size)
	}

	var base []Extent
	for _, r := range p.ReadRanges {
		if r.Offset < 0 || r.End() > int64(len(object)) {
			t.Fatalf("plan demanded %+v, outside the %d-byte object", r, len(object))
		}
		base = append(base, Extent{Offset: r.Offset, Data: object[r.Offset:r.End()]})
	}

	body, err := n.Splice(p.Size, base)
	if err != nil {
		t.Fatalf("Splice: %v", err)
	}
	if int64(len(body)) != p.Size {
		t.Fatalf("Splice produced %d bytes, plan said %d", len(body), p.Size)
	}
	if !n.MarkFlushed(gen, int64(len(body)), "etag") {
		t.Fatal("MarkFlushed rejected an unraced flush")
	}
	return body, true
}

// resizeModel grows a model file with zeros or shortens it, as truncate(2) does.
func resizeModel(file []byte, size int64) []byte {
	if size <= int64(len(file)) {
		return file[:size]
	}
	return append(file, make([]byte, size-int64(len(file)))...)
}

// Concurrent opens, writes, reads, flushes, and releases on the same path. Run under -race, this is
// what would have caught the sixteen concurrency bugs filed after RACE_CONDITION_AUDIT.md declared
// the codebase race-free.
func TestHandleTableConcurrentAccess(t *testing.T) {
	t.Parallel()

	tbl := NewHandleTable()
	const workers = 16
	const iterations = 40

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()

			for i := range iterations {
				path := fmt.Sprintf("f%d", i%4)

				h, err := tbl.Open(path, OpenRead|OpenWrite, testAttr(0), 0)
				if err != nil {
					t.Errorf("worker %d: Open: %v", w, err)
					return
				}
				n := h.Node

				if _, err := n.Write(int64(w*8), []byte("abcdefgh"), false); err != nil {
					t.Errorf("worker %d: Write: %v", w, err)
					return
				}

				buf := make([]byte, 32)
				if _, _, err := n.ReadRange(0, len(buf)); err != nil {
					t.Errorf("worker %d: ReadRange: %v", w, err)
					return
				}
				if _, err := n.ReadInto(buf, 0, nil); err != nil {
					t.Errorf("worker %d: ReadInto: %v", w, err)
					return
				}

				gen := n.Generation()
				p, _, err := n.FlushPlan()
				if err != nil {
					t.Errorf("worker %d: FlushPlan: %v", w, err)
					return
				}
				if !p.Noop {
					body, err := n.Splice(p.Size, nil)
					if err != nil {
						t.Errorf("worker %d: Splice: %v", w, err)
						return
					}
					// May legitimately fail: another worker's write raced this flush. That is the
					// contract, not an error.
					n.MarkFlushed(gen, int64(len(body)), "etag")
				}

				_ = n.Attr()
				_ = n.Dirty()
				_ = tbl.DirtyNodes()

				if _, _, err := tbl.Release(h.ID); err != nil {
					t.Errorf("worker %d: Release: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if got := tbl.Len(); got != 0 {
		t.Errorf("Len() = %d after every handle was released, want 0", got)
	}
}
