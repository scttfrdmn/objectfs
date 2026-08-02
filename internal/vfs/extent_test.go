package vfs

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

// model is a deliberately stupid reference implementation of ExtentList: one map entry per dirty
// byte. It is far too slow to ship and trivially obviously correct, which is the point — every
// property test below asserts ExtentList against it rather than against hand-computed expectations,
// so a test cannot ratify the same off-by-one the implementation has.
type model struct {
	dirty map[int64]byte
}

func newModel() *model { return &model{dirty: map[int64]byte{}} }

func (m *model) add(offset int64, data []byte) {
	for i, b := range data {
		m.dirty[offset+int64(i)] = b
	}
}

func (m *model) truncate(size int64) {
	for off := range m.dirty {
		if off >= size {
			delete(m.dirty, off)
		}
	}
}

// extents derives the coalesced extent list the model implies.
func (m *model) extents() []Extent {
	if len(m.dirty) == 0 {
		return nil
	}
	offs := make([]int64, 0, len(m.dirty))
	for off := range m.dirty {
		offs = append(offs, off)
	}
	slices.Sort(offs)

	var out []Extent
	cur := Extent{Offset: offs[0], Data: []byte{m.dirty[offs[0]]}}
	for _, off := range offs[1:] {
		if off == cur.End() {
			cur.Data = append(cur.Data, m.dirty[off])
			continue
		}
		out = append(out, cur)
		cur = Extent{Offset: off, Data: []byte{m.dirty[off]}}
	}
	return append(out, cur)
}

func (m *model) maxEnd() int64 {
	var mx int64
	for off := range m.dirty {
		if off+1 > mx {
			mx = off + 1
		}
	}
	return mx
}

// assertMatches checks the list against the model and against its own invariants.
func assertMatches(t *testing.T, l *ExtentList, m *model) {
	t.Helper()

	if err := l.check(); err != nil {
		t.Fatalf("invariant violated: %v\ngot %s", err, formatExtents(l.Extents()))
	}

	want := m.extents()
	got := l.Extents()
	if len(got) != len(want) {
		t.Fatalf("extent count = %d, model says %d\ngot  %s\nwant %s",
			len(got), len(want), formatExtents(got), formatExtents(want))
	}
	for i := range want {
		if got[i].Offset != want[i].Offset || !bytes.Equal(got[i].Data, want[i].Data) {
			t.Fatalf("extent %d = {%d, %q}, model says {%d, %q}",
				i, got[i].Offset, got[i].Data, want[i].Offset, want[i].Data)
		}
	}
	if l.MaxEnd() != m.maxEnd() {
		t.Fatalf("MaxEnd = %d, model says %d", l.MaxEnd(), m.maxEnd())
	}
	if l.Bytes() != int64(len(m.dirty)) {
		t.Fatalf("Bytes = %d, model says %d", l.Bytes(), len(m.dirty))
	}
}

// modelRead is what the model says a read of length bytes at offset returns: the file's bytes from
// offset, short at EOF, empty past it. Written to tolerate an offset past the end, which is a legal
// read and must not panic here any more than it does in ReadAt.
func modelRead(file []byte, offset int64, length int) []byte {
	if offset >= int64(len(file)) {
		return nil
	}
	return file[offset:min(offset+int64(length), int64(len(file)))]
}

func formatExtents(es []Extent) string {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, e := range es {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "{%d..%d %q}", e.Offset, e.End(), e.Data)
	}
	b.WriteByte(']')
	return b.String()
}

// writeOp is one Add call, used by the table and sequence tests.
type writeOp struct {
	offset int64
	data   string
}

func TestExtentListAdd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ops  []writeOp
		want []Extent
	}{
		{
			name: "single write",
			ops:  []writeOp{{0, "AAAA"}},
			want: []Extent{{0, []byte("AAAA")}},
		},
		{
			name: "sequential writes coalesce into one extent",
			ops:  []writeOp{{0, "AAAA"}, {4, "BBBB"}, {8, "CCCC"}},
			want: []Extent{{0, []byte("AAAABBBBCCCC")}},
		},
		{
			name: "sparse writes stay separate",
			ops:  []writeOp{{0, "AAAA"}, {100, "BBBB"}},
			want: []Extent{{0, []byte("AAAA")}, {100, []byte("BBBB")}},
		},
		{
			name: "out-of-order writes are ordered by offset",
			ops:  []writeOp{{100, "BBBB"}, {0, "AAAA"}, {50, "MMMM"}},
			want: []Extent{{0, []byte("AAAA")}, {50, []byte("MMMM")}, {100, []byte("BBBB")}},
		},
		{
			name: "backwards-adjacent write coalesces",
			ops:  []writeOp{{4, "BBBB"}, {0, "AAAA"}},
			want: []Extent{{0, []byte("AAAABBBB")}},
		},
		{
			name: "later write wins on full overlap",
			ops:  []writeOp{{0, "AAAA"}, {0, "BBBB"}},
			want: []Extent{{0, []byte("BBBB")}},
		},
		{
			name: "later write wins on partial overlap",
			ops:  []writeOp{{0, "AAAAAAAA"}, {2, "BB"}},
			want: []Extent{{0, []byte("AABBAAAA")}},
		},
		{
			// The mergeWrites defect: v0.10.0 guarded its overlay with "is the new end past the
			// current end", so a shorter write over longer content was discarded entirely.
			name: "shorter write over longer content still wins",
			ops:  []writeOp{{0, "OLDCONTENT"}, {0, "NEW"}},
			want: []Extent{{0, []byte("NEWCONTENT")}},
		},
		{
			name: "write spanning a gap merges both neighbors",
			ops:  []writeOp{{0, "AAAA"}, {10, "BBBB"}, {2, "xxxxxxxxxx"}},
			want: []Extent{{0, []byte("AAxxxxxxxxxxBB")}},
		},
		{
			name: "write swallowing an extent entirely",
			ops:  []writeOp{{4, "BB"}, {0, "xxxxxxxxxx"}},
			want: []Extent{{0, []byte("xxxxxxxxxx")}},
		},
		{
			name: "write swallowing several extents",
			ops:  []writeOp{{0, "A"}, {2, "B"}, {4, "C"}, {6, "D"}, {0, "zzzzzzzz"}},
			want: []Extent{{0, []byte("zzzzzzzz")}},
		},
		{
			name: "write filling the gap exactly coalesces three into one",
			ops:  []writeOp{{0, "AA"}, {4, "BB"}, {2, "gg"}},
			want: []Extent{{0, []byte("AAggBB")}},
		},
		{
			name: "write inside an existing extent does not split it",
			ops:  []writeOp{{0, "AAAAAAAAAA"}, {5, "b"}},
			want: []Extent{{0, []byte("AAAAAbAAAA")}},
		},
		{
			name: "write extending an extent's tail",
			ops:  []writeOp{{0, "AAAA"}, {2, "BBBB"}},
			want: []Extent{{0, []byte("AABBBB")}},
		},
		{
			name: "write extending an extent's head",
			ops:  []writeOp{{4, "AAAA"}, {2, "BBBB"}},
			want: []Extent{{2, []byte("BBBBAA")}},
		},
		{
			name: "empty write is a no-op",
			ops:  []writeOp{{0, "AAAA"}, {100, ""}},
			want: []Extent{{0, []byte("AAAA")}},
		},
		{
			name: "empty write into an empty list leaves it empty",
			ops:  []writeOp{{0, ""}},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var l ExtentList
			m := newModel()
			for _, op := range tc.ops {
				if err := l.Add(op.offset, []byte(op.data)); err != nil {
					t.Fatalf("Add(%d, %q) = %v", op.offset, op.data, err)
				}
				m.add(op.offset, []byte(op.data))
			}

			assertMatches(t, &l, m)

			got := l.Extents()
			if len(got) != len(tc.want) {
				t.Fatalf("got %s, want %s", formatExtents(got), formatExtents(tc.want))
			}
			for i := range tc.want {
				if got[i].Offset != tc.want[i].Offset || !bytes.Equal(got[i].Data, tc.want[i].Data) {
					t.Fatalf("got %s, want %s", formatExtents(got), formatExtents(tc.want))
				}
			}
		})
	}
}

func TestExtentListAddRejectsNegativeOffset(t *testing.T) {
	t.Parallel()

	var l ExtentList
	err := l.Add(-1, []byte("data"))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Add(-1, ...) = %v, want ErrInvalid", err)
	}
	if !l.Empty() {
		t.Fatalf("rejected write still mutated the list: %s", formatExtents(l.Extents()))
	}
}

// The caller's buffer is FUSE's, and FUSE reuses it after the call returns. If Add retained it
// instead of copying, every buffered write would decay into whatever the kernel wrote next.
func TestExtentListAddCopiesCallerBuffer(t *testing.T) {
	t.Parallel()

	buf := []byte("ORIGINAL")
	var l ExtentList
	if err := l.Add(0, buf); err != nil {
		t.Fatalf("Add: %v", err)
	}

	copy(buf, "OVERWRIT")

	if got := string(l.Extents()[0].Data); got != "ORIGINAL" {
		t.Fatalf("extent data = %q after caller reused its buffer, want %q", got, "ORIGINAL")
	}
}

// serveRead does what the read path will do: fill the caller's buffer with whatever the object
// store can answer for this request, then hand it to ReadAt. Only the part of the request that
// overlaps [0, StoredValid) is fillable — the rest is a hole or past EOF, and the buffer is poisoned
// there so that any byte ReadAt reports as valid without having written it shows up as 0xFE.
func serveRead(t *testing.T, l *ExtentList, stored []byte, offset int64, length int) []byte {
	t.Helper()

	buf := bytes.Repeat([]byte{0xFE}, length)
	if bound := l.StoredValid(int64(len(stored))); offset < bound {
		copy(buf, stored[offset:min(bound, offset+int64(length))])
	}

	valid, err := l.ReadAt(buf, offset, int64(len(stored)))
	if err != nil {
		t.Fatalf("ReadAt(off=%d len=%d): %v", offset, length, err)
	}
	if valid < 0 || valid > length {
		t.Fatalf("ReadAt returned valid=%d, outside [0,%d]", valid, length)
	}
	return buf[:valid]
}

func TestExtentListReadAt(t *testing.T) {
	t.Parallel()

	// Truncations are applied before the writes, so a case can express "truncate, then write past the
	// old end" — the sequence that must not resurrect the truncated bytes.
	tests := []struct {
		name     string
		truncate []int64
		ops      []writeOp
		stored   string // the object as it exists in storage
		offset   int64
		length   int
		want     string
	}{
		{
			name:   "no pending writes returns the stored bytes untouched",
			stored: "STOREDXX",
			length: 8,
			want:   "STOREDXX",
		},
		{
			// H5: v0.10.0 consulted the cache and the backend but never the pending writes.
			name:   "pending write shadows the stored bytes",
			ops:    []writeOp{{0, "NEW"}},
			stored: "STOREDXX",
			length: 8,
			want:   "NEWREDXX",
		},
		{
			name:   "pending write past the stored end extends the file",
			ops:    []writeOp{{4, "TAIL"}},
			stored: "HEAD",
			length: 8,
			want:   "HEADTAIL",
		},
		{
			name:   "hole between the stored end and a pending write reads as zeros",
			ops:    []writeOp{{6, "ZZ"}},
			stored: "HEAD",
			length: 8,
			want:   "HEAD\x00\x00ZZ",
		},
		{
			name:   "read is short at EOF, not padded",
			stored: "HEAD",
			length: 8,
			want:   "HEAD",
		},
		{
			name:   "read at a non-zero offset",
			ops:    []writeOp{{100, "PENDING"}},
			stored: "STOREDX",
			offset: 100,
			length: 7,
			want:   "PENDING",
		},
		{
			name:   "extent partially overlapping the front of the request",
			ops:    []writeOp{{0, "AAAAAA"}},
			stored: "STORED12",
			offset: 4,
			length: 4,
			want:   "AA12",
		},
		{
			name:   "extent partially overlapping the back of the request",
			ops:    []writeOp{{6, "BBBBBB"}},
			stored: "STOREDXX",
			length: 8,
			want:   "STOREDBB",
		},
		{
			name:   "extents entirely outside the request are ignored",
			ops:    []writeOp{{0, "AA"}, {1000, "BB"}},
			stored: "0123456789",
			offset: 4,
			length: 4,
			want:   "4567",
		},
		{
			name:   "several extents overlaid in one read",
			ops:    []writeOp{{0, "aa"}, {4, "bb"}, {8, "cc"}},
			stored: "0123456789",
			length: 10,
			want:   "aa23bb67cc",
		},
		{
			name:   "read entirely past the stored end is all pending",
			ops:    []writeOp{{10, "XXXX"}},
			offset: 10,
			length: 4,
			want:   "XXXX",
		},
		{
			name:   "read past EOF returns nothing",
			ops:    []writeOp{{0, "AAAA"}},
			offset: 1000,
			length: 4,
			want:   "",
		},
		{
			name:   "zero-length buffer",
			ops:    []writeOp{{0, "AAAA"}},
			length: 0,
			want:   "",
		},
		{
			name:     "read past a truncation point returns nothing",
			stored:   "0123456789",
			truncate: []int64{4},
			offset:   4,
			length:   4,
			want:     "",
		},
		{
			name:     "truncation shortens the read",
			stored:   "0123456789",
			truncate: []int64{4},
			length:   8,
			want:     "0123",
		},
		{
			// The regression the property test found: bytes past a truncation are gone. Growing the
			// file again leaves a hole there, and a hole is zeros — never the stored object's old
			// contents, which a naive "fetch the stored range" would resurrect.
			name:     "growing back after a truncation reads zeros, not the old bytes",
			stored:   "0123456789",
			truncate: []int64{4},
			ops:      []writeOp{{8, "XX"}},
			length:   10,
			want:     "0123\x00\x00\x00\x00XX",
		},
		{
			name:     "explicit grow by truncation is a hole",
			stored:   "ABCD",
			truncate: []int64{8},
			length:   8,
			want:     "ABCD\x00\x00\x00\x00",
		},
		{
			name:     "truncation to zero hides the whole object",
			stored:   "0123456789",
			truncate: []int64{0},
			length:   10,
			want:     "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var l ExtentList
			for _, size := range tc.truncate {
				if err := l.Truncate(size); err != nil {
					t.Fatalf("Truncate(%d): %v", size, err)
				}
			}
			for _, op := range tc.ops {
				if err := l.Add(op.offset, []byte(op.data)); err != nil {
					t.Fatalf("Add: %v", err)
				}
			}

			got := serveRead(t, &l, []byte(tc.stored), tc.offset, tc.length)
			if string(got) != tc.want {
				t.Fatalf("read(off=%d len=%d) = %q, want %q", tc.offset, tc.length, got, tc.want)
			}
		})
	}
}

func TestExtentListReadAtRejectsBadArgs(t *testing.T) {
	t.Parallel()

	var l ExtentList
	buf := make([]byte, 8)

	if _, err := l.ReadAt(buf, -1, 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("ReadAt(offset=-1) = %v, want ErrInvalid", err)
	}
	if _, err := l.ReadAt(buf, 0, -1); !errors.Is(err, ErrInvalid) {
		t.Errorf("ReadAt(storedSize=-1) = %v, want ErrInvalid", err)
	}
}

// Size and StoredValid are the two questions the layers above ask, and the pair only makes sense
// together: stat reports Size, while the backend can only answer for StoredValid. A truncation moves
// them independently.
func TestExtentListSizeAndStoredValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		ops             []writeOp
		truncate        []int64
		storedSize      int64
		wantSize        int64
		wantStoredValid int64
	}{
		{
			name:            "clean list is the stored object",
			storedSize:      100,
			wantSize:        100,
			wantStoredValid: 100,
		},
		{
			name:            "write inside the object does not change its size",
			ops:             []writeOp{{10, "XXXX"}},
			storedSize:      100,
			wantSize:        100,
			wantStoredValid: 100,
		},
		{
			name:            "write past the end grows the file",
			ops:             []writeOp{{100, "XXXX"}},
			storedSize:      100,
			wantSize:        104,
			wantStoredValid: 100,
		},
		{
			name:            "shrinking truncation shortens both",
			truncate:        []int64{40},
			storedSize:      100,
			wantSize:        40,
			wantStoredValid: 40,
		},
		{
			name:            "growing truncation grows the file but not the readable object",
			truncate:        []int64{200},
			storedSize:      100,
			wantSize:        200,
			wantStoredValid: 100,
		},
		{
			// The reason two numbers are needed. The file is 200 bytes and the last truncation set
			// that; but only the first 40 bytes of the stored object survive, so bytes 40..200 are a
			// hole regardless of what storage still holds there.
			name:            "shrink then grow keeps the low-water mark",
			truncate:        []int64{40, 200},
			storedSize:      100,
			wantSize:        200,
			wantStoredValid: 40,
		},
		{
			name:            "write past a truncation extends the file",
			truncate:        []int64{40},
			ops:             []writeOp{{300, "XX"}},
			storedSize:      100,
			wantSize:        302,
			wantStoredValid: 40,
		},
		{
			name:            "truncation larger than the stored object cannot invent readable bytes",
			truncate:        []int64{500},
			storedSize:      10,
			wantSize:        500,
			wantStoredValid: 10,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var l ExtentList
			for _, size := range tc.truncate {
				if err := l.Truncate(size); err != nil {
					t.Fatalf("Truncate(%d): %v", size, err)
				}
			}
			for _, op := range tc.ops {
				if err := l.Add(op.offset, []byte(op.data)); err != nil {
					t.Fatalf("Add: %v", err)
				}
			}

			if got := l.Size(tc.storedSize); got != tc.wantSize {
				t.Errorf("Size(%d) = %d, want %d", tc.storedSize, got, tc.wantSize)
			}
			if got := l.StoredValid(tc.storedSize); got != tc.wantStoredValid {
				t.Errorf("StoredValid(%d) = %d, want %d", tc.storedSize, got, tc.wantStoredValid)
			}
		})
	}
}

func TestExtentListPlan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		ops        []writeOp
		truncate   []int64
		storedSize int64
		want       FlushPlan
	}{
		{
			name: "nothing dirty, no size change, is a no-op",
			// The distinction that matters: a caller must not read this as "whole-object write of
			// zero bytes" and PUT an empty object over intact data.
			storedSize: 100,
			want:       FlushPlan{Noop: true, Size: 100},
		},
		{
			name:       "nothing dirty against a new object is a no-op",
			storedSize: 0,
			want:       FlushPlan{Noop: true, Size: 0},
		},
		{
			name:       "truncate to zero with nothing dirty is a whole-object write",
			storedSize: 100,
			truncate:   []int64{0},
			want:       FlushPlan{WholeObject: true, Size: 0},
		},
		{
			name:       "shrinking truncation with nothing dirty reads only the surviving prefix",
			storedSize: 100,
			truncate:   []int64{40},
			want: FlushPlan{
				Size:       40,
				ReadRanges: []Range{{Offset: 0, Length: 40}},
			},
		},
		{
			name:       "growing truncation with nothing dirty appends a hole",
			storedSize: 100,
			truncate:   []int64{150},
			want: FlushPlan{
				Size:       150,
				ReadRanges: []Range{{Offset: 0, Length: 100}},
			},
		},
		{
			// The regression the property test caught. Truncating to 4 destroys bytes 4..62; writing
			// at 83 must not cause them to be fetched back. Only [0,4) may be read.
			name:       "write past a truncation must not refetch the destroyed bytes",
			storedSize: 62,
			truncate:   []int64{4},
			ops:        []writeOp{{83, "XXXX"}},
			want: FlushPlan{
				Size:       87,
				ReadRanges: []Range{{Offset: 0, Length: 4}},
			},
		},
		{
			name:       "truncation to zero then a write needs no read at all",
			storedSize: 100,
			truncate:   []int64{0},
			ops:        []writeOp{{50, "XX"}},
			want:       FlushPlan{WholeObject: true, Size: 52},
		},
		{
			name:       "new object written from zero needs no read",
			ops:        []writeOp{{0, "0123456789"}},
			storedSize: 0,
			want:       FlushPlan{WholeObject: true, Size: 10},
		},
		{
			name:       "full overwrite of a same-size object needs no read",
			ops:        []writeOp{{0, "0123456789"}},
			storedSize: 10,
			want:       FlushPlan{WholeObject: true, Size: 10},
		},
		{
			name:       "full overwrite past the stored end needs no read",
			ops:        []writeOp{{0, "0123456789"}},
			storedSize: 4,
			want:       FlushPlan{WholeObject: true, Size: 10},
		},
		{
			// H7, the headline data-loss defect: v0.10.0 PUT just these bytes and the 1 MiB object
			// became 1 byte. The plan must demand the preceding megabyte.
			name:       "one byte appended to a 1 MiB object must read the rest",
			ops:        []writeOp{{1048575, "X"}},
			storedSize: 1048576,
			want: FlushPlan{
				Size:       1048576,
				ReadRanges: []Range{{Offset: 0, Length: 1048575}},
			},
		},
		{
			name:       "write at an offset into a longer object reads head and tail",
			ops:        []writeOp{{10, "XXXX"}},
			storedSize: 100,
			want: FlushPlan{
				Size:       100,
				ReadRanges: []Range{{Offset: 0, Length: 10}, {Offset: 14, Length: 86}},
			},
		},
		{
			name:       "write at offset zero into a longer object reads only the tail",
			ops:        []writeOp{{0, "XXXX"}},
			storedSize: 100,
			want: FlushPlan{
				Size:       100,
				ReadRanges: []Range{{Offset: 4, Length: 96}},
			},
		},
		{
			name:       "sparse writes read every gap",
			ops:        []writeOp{{0, "AA"}, {10, "BB"}, {20, "CC"}},
			storedSize: 100,
			want: FlushPlan{
				Size: 100,
				ReadRanges: []Range{
					{Offset: 2, Length: 8},
					{Offset: 12, Length: 8},
					{Offset: 22, Length: 78},
				},
			},
		},
		{
			// Beyond the stored end there is nothing to read: the gap is a hole, and a hole is
			// zeros that Splice supplies for free. Fetching it would be a range GET past EOF.
			name:       "gaps past the stored end are holes, not reads",
			ops:        []writeOp{{0, "AA"}, {100, "BB"}},
			storedSize: 2,
			want:       FlushPlan{WholeObject: true, Size: 102},
		},
		{
			name:       "write starting past the stored end reads only what exists",
			ops:        []writeOp{{50, "XX"}},
			storedSize: 10,
			want: FlushPlan{
				Size:       52,
				ReadRanges: []Range{{Offset: 0, Length: 10}},
			},
		},
		{
			// O_TRUNC then a full rewrite: the common `>` redirect. Nothing to read.
			name:       "truncate to zero then rewrite is a whole-object write",
			truncate:   []int64{0},
			ops:        []writeOp{{0, "SHORT"}},
			storedSize: 100,
			want:       FlushPlan{WholeObject: true, Size: 5},
		},
		{
			name:       "truncate to the write's end still reads the head",
			ops:        []writeOp{{10, "XXXX"}},
			truncate:   []int64{14},
			storedSize: 100,
			want: FlushPlan{
				Size:       14,
				ReadRanges: []Range{{Offset: 0, Length: 10}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var l ExtentList
			for _, size := range tc.truncate {
				if err := l.Truncate(size); err != nil {
					t.Fatalf("Truncate(%d): %v", size, err)
				}
			}
			for _, op := range tc.ops {
				if err := l.Add(op.offset, []byte(op.data)); err != nil {
					t.Fatalf("Add: %v", err)
				}
			}

			got, err := l.Plan(tc.storedSize)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			if got.Noop != tc.want.Noop || got.WholeObject != tc.want.WholeObject || got.Size != tc.want.Size {
				t.Fatalf("Plan = {Noop:%v WholeObject:%v Size:%d}, want {Noop:%v WholeObject:%v Size:%d}",
					got.Noop, got.WholeObject, got.Size, tc.want.Noop, tc.want.WholeObject, tc.want.Size)
			}
			if len(got.ReadRanges) != len(tc.want.ReadRanges) {
				t.Fatalf("ReadRanges = %v, want %v", got.ReadRanges, tc.want.ReadRanges)
			}
			for i := range tc.want.ReadRanges {
				if got.ReadRanges[i] != tc.want.ReadRanges[i] {
					t.Fatalf("ReadRanges = %v, want %v", got.ReadRanges, tc.want.ReadRanges)
				}
			}
		})
	}
}

func TestExtentListPlanRejectsNegativeStoredSize(t *testing.T) {
	t.Parallel()

	var l ExtentList
	if _, err := l.Plan(-1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Plan(-1) = %v, want ErrInvalid", err)
	}
}

// A plan is only useful if its parts are mutually consistent: at most one of Noop and WholeObject,
// and ReadRanges ordered, disjoint, and confined to bytes the stored object still meaningfully
// holds. A caller that fetches these ranges and splices must not be handed something
// self-contradictory — or something that would resurrect truncated data.
func TestExtentListPlanIsSelfConsistent(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(0x5eed, 0x1234))

	for range 3000 {
		var l ExtentList
		truncated := false
		// The operation count is drawn once, not re-drawn each iteration. `i < rng.IntN(7)` re-rolls
		// the bound on every pass, so the loop ends as soon as one roll lands at or below i — which
		// turns the intended uniform 0-6 into a peak at 1-2 and drops 6-operation programs from 14.3%
		// of runs to 0.62%, a 23x under-sampling of exactly the longest sequences this test exists to
		// explore. Measured over a million iterations, not reasoned about.
		for i := range rng.IntN(7) {
			if rng.IntN(6) == 0 {
				if err := l.Truncate(int64(rng.IntN(250))); err != nil {
					t.Fatalf("Truncate: %v", err)
				}
				truncated = true
				continue
			}
			offset := int64(rng.IntN(200))
			data := bytes.Repeat([]byte{byte('a' + i)}, 1+rng.IntN(30))
			if err := l.Add(offset, data); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		storedSize := int64(rng.IntN(250))

		p, err := l.Plan(storedSize)
		if err != nil {
			t.Fatalf("Plan(%d): %v", storedSize, err)
		}

		if p.Noop && p.WholeObject {
			t.Fatalf("plan is both Noop and WholeObject: %+v", p)
		}
		if (p.Noop || p.WholeObject) && len(p.ReadRanges) != 0 {
			t.Fatalf("plan needs no read but has ReadRanges: %+v", p)
		}
		if p.Size != l.Size(storedSize) {
			t.Fatalf("plan Size %d disagrees with Size() %d: %+v", p.Size, l.Size(storedSize), p)
		}
		if p.Size < l.MaxEnd() {
			t.Fatalf("plan Size %d discards dirty bytes up to %d: %+v", p.Size, l.MaxEnd(), p)
		}
		if !truncated && p.Size < storedSize {
			t.Fatalf("plan without a truncation shrinks a %d-byte object to %d: %+v", storedSize, p.Size, p)
		}
		if p.Noop && (!l.Empty() || p.Size != storedSize) {
			t.Fatalf("plan claims no work but state differs from storage: %+v", p)
		}

		bound := l.StoredValid(storedSize)
		var prevEnd int64
		for i, r := range p.ReadRanges {
			if r.Length <= 0 {
				t.Fatalf("ReadRanges[%d] is empty: %+v", i, p)
			}
			if r.Offset < prevEnd {
				t.Fatalf("ReadRanges[%d] overlaps or precedes its predecessor: %+v", i, p)
			}
			if r.End() > bound {
				t.Fatalf("ReadRanges[%d] reads past the valid stored prefix %d (stored %d): %+v",
					i, bound, storedSize, p)
			}
			if r.End() > p.Size {
				t.Fatalf("ReadRanges[%d] reads past the result size %d: %+v", i, p.Size, p)
			}
			prevEnd = r.End()
		}
	}
}

func TestExtentListSplice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ops  []writeOp
		size int64
		base []Extent
		want string
	}{
		{
			name: "base only",
			size: 8,
			base: []Extent{{0, []byte("STOREDXX")}},
			want: "STOREDXX",
		},
		{
			// H7 again, from the other side: the append must produce head+tail, not tail alone.
			name: "append splices base head with the pending tail",
			ops:  []writeOp{{4, "BBBB"}},
			size: 8,
			base: []Extent{{0, []byte("AAAA")}},
			want: "AAAABBBB",
		},
		{
			name: "in-place write splices two base fragments around it",
			ops:  []writeOp{{4, "XX"}},
			size: 10,
			base: []Extent{{0, []byte("AAAA")}, {6, []byte("BBBB")}},
			want: "AAAAXXBBBB",
		},
		{
			name: "pending writes override the base where they overlap",
			ops:  []writeOp{{2, "XX"}},
			size: 8,
			base: []Extent{{0, []byte("AAAAAAAA")}},
			want: "AAXXAAAA",
		},
		{
			name: "uncovered bytes are zeros",
			ops:  []writeOp{{6, "ZZ"}},
			size: 8,
			base: []Extent{{0, []byte("AA")}},
			want: "AA\x00\x00\x00\x00ZZ",
		},
		{
			name: "size zero yields an empty body",
			ops:  []writeOp{{0, "AAAA"}},
			size: 0,
			base: []Extent{{0, []byte("BBBB")}},
			want: "",
		},
		{
			name: "extents past size are truncated, not rejected",
			ops:  []writeOp{{0, "AAAAAAAA"}},
			size: 4,
			want: "AAAA",
		},
		{
			name: "base fragments past size are truncated",
			size: 4,
			base: []Extent{{0, []byte("AA")}, {100, []byte("ZZ")}},
			want: "AA\x00\x00",
		},
		{
			name: "sparse file with no base at all",
			ops:  []writeOp{{0, "A"}, {4, "B"}},
			size: 5,
			want: "A\x00\x00\x00B",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var l ExtentList
			for _, op := range tc.ops {
				if err := l.Add(op.offset, []byte(op.data)); err != nil {
					t.Fatalf("Add: %v", err)
				}
			}

			got, err := l.Splice(tc.size, tc.base)
			if err != nil {
				t.Fatalf("Splice: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("Splice = %q, want %q", got, tc.want)
			}
			if int64(len(got)) != tc.size {
				t.Fatalf("Splice returned %d bytes, want exactly %d", len(got), tc.size)
			}
		})
	}
}

func TestExtentListSpliceRejectsBadArgs(t *testing.T) {
	t.Parallel()

	var l ExtentList
	if _, err := l.Splice(-1, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("Splice(-1) = %v, want ErrInvalid", err)
	}
	if _, err := l.Splice(8, []Extent{{-1, []byte("x")}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Splice with negative base offset = %v, want ErrInvalid", err)
	}
}

func TestExtentListTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ops  []writeOp
		size int64
		want []Extent
	}{
		{
			name: "truncate to zero drops everything",
			ops:  []writeOp{{0, "AAAA"}, {100, "BBBB"}},
			size: 0,
			want: nil,
		},
		{
			name: "truncate inside an extent clips it",
			ops:  []writeOp{{0, "0123456789"}},
			size: 4,
			want: []Extent{{0, []byte("0123")}},
		},
		{
			name: "truncate at an extent boundary keeps it whole",
			ops:  []writeOp{{0, "0123456789"}},
			size: 10,
			want: []Extent{{0, []byte("0123456789")}},
		},
		{
			name: "truncate drops extents wholly past the new end",
			ops:  []writeOp{{0, "AAAA"}, {100, "BBBB"}},
			size: 50,
			want: []Extent{{0, []byte("AAAA")}},
		},
		{
			name: "truncate at an extent's start drops it entirely",
			ops:  []writeOp{{0, "AAAA"}, {100, "BBBB"}},
			size: 100,
			want: []Extent{{0, []byte("AAAA")}},
		},
		{
			// A terabyte truncate must stay a metadata operation. If growth allocated, this test
			// would exhaust memory rather than fail.
			name: "growing adds no extents",
			ops:  []writeOp{{0, "AAAA"}},
			size: 1 << 40,
			want: []Extent{{0, []byte("AAAA")}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var l ExtentList
			m := newModel()
			for _, op := range tc.ops {
				if err := l.Add(op.offset, []byte(op.data)); err != nil {
					t.Fatalf("Add: %v", err)
				}
				m.add(op.offset, []byte(op.data))
			}

			if err := l.Truncate(tc.size); err != nil {
				t.Fatalf("Truncate(%d): %v", tc.size, err)
			}
			m.truncate(tc.size)
			assertMatches(t, &l, m)

			got := l.Extents()
			if len(got) != len(tc.want) {
				t.Fatalf("got %s, want %s", formatExtents(got), formatExtents(tc.want))
			}
			for i := range tc.want {
				if got[i].Offset != tc.want[i].Offset || !bytes.Equal(got[i].Data, tc.want[i].Data) {
					t.Fatalf("got %s, want %s", formatExtents(got), formatExtents(tc.want))
				}
			}
		})
	}
}

func TestExtentListTruncateRejectsNegativeSize(t *testing.T) {
	t.Parallel()

	var l ExtentList
	if err := l.Add(0, []byte("AAAA")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := l.Truncate(-1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Truncate(-1) = %v, want ErrInvalid", err)
	}
	if l.Bytes() != 4 {
		t.Fatalf("rejected truncate still mutated the list: %s", formatExtents(l.Extents()))
	}
}

// Truncate reslices in place over the backing array. Growing afterwards must not resurrect the
// bytes it dropped.
func TestExtentListTruncateThenGrow(t *testing.T) {
	t.Parallel()

	var l ExtentList
	m := newModel()
	for _, op := range []writeOp{{0, "AAAA"}, {10, "BBBB"}, {20, "CCCC"}} {
		if err := l.Add(op.offset, []byte(op.data)); err != nil {
			t.Fatalf("Add: %v", err)
		}
		m.add(op.offset, []byte(op.data))
	}

	if err := l.Truncate(12); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	m.truncate(12)

	if err := l.Add(30, []byte("DDDD")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.add(30, []byte("DDDD"))

	assertMatches(t, &l, m)
}

func TestExtentListReset(t *testing.T) {
	t.Parallel()

	var l ExtentList
	if err := l.Add(0, []byte("AAAA")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	l.Reset()

	if !l.Empty() || l.Len() != 0 || l.Bytes() != 0 || l.MaxEnd() != 0 {
		t.Fatalf("after Reset: Empty=%v Len=%d Bytes=%d MaxEnd=%d",
			l.Empty(), l.Len(), l.Bytes(), l.MaxEnd())
	}

	if err := l.Add(5, []byte("BB")); err != nil {
		t.Fatalf("Add after Reset: %v", err)
	}
	m := newModel()
	m.add(5, []byte("BB"))
	assertMatches(t, &l, m)
}

// check is what every property test in this file relies on to notice a corrupted list. An invariant
// checker that cannot fail is worse than none: it converts a silent bug into a passing test. So
// assert it rejects each state it exists to reject, built by hand since the public API cannot
// produce them.
func TestExtentListCheckDetectsCorruption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		list ExtentList
	}{
		{
			name: "empty extent",
			list: ExtentList{extents: []Extent{{Offset: 0, Data: []byte{}}}},
		},
		{
			name: "negative offset",
			list: ExtentList{extents: []Extent{{Offset: -1, Data: []byte("A")}}},
		},
		{
			name: "overlapping extents",
			list: ExtentList{extents: []Extent{
				{Offset: 0, Data: []byte("AAAA")},
				{Offset: 2, Data: []byte("BB")},
			}},
		},
		{
			name: "out-of-order extents",
			list: ExtentList{extents: []Extent{
				{Offset: 10, Data: []byte("BB")},
				{Offset: 0, Data: []byte("AA")},
			}},
		},
		{
			// Adjacent-but-separate is a real defect, not cosmetics: Plan counts extents to decide
			// between a whole-object PUT and a ranged read-modify-write, so an uncoalesced pair
			// makes it fetch a range it already holds.
			name: "adjacent extents that should have coalesced",
			list: ExtentList{extents: []Extent{
				{Offset: 0, Data: []byte("AA")},
				{Offset: 2, Data: []byte("BB")},
			}},
		},
		{
			name: "negative minSize",
			list: ExtentList{truncated: true, minSize: -1, lastSize: 10},
		},
		{
			name: "negative lastSize",
			list: ExtentList{truncated: true, minSize: 0, lastSize: -1},
		},
		{
			// minSize bounds how much of the stored object still means anything, so it can never
			// exceed the file's current length — that would resurrect deleted bytes.
			name: "minSize exceeding lastSize",
			list: ExtentList{truncated: true, minSize: 20, lastSize: 10},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l := tc.list
			if err := l.check(); err == nil {
				t.Fatalf("check() accepted a corrupt list: %+v", l)
			}
		})
	}

	t.Run("a well-formed list passes", func(t *testing.T) {
		t.Parallel()

		l := ExtentList{
			extents:   []Extent{{Offset: 0, Data: []byte("AA")}, {Offset: 10, Data: []byte("BB")}},
			truncated: true,
			minSize:   4,
			lastSize:  12,
		}
		if err := l.check(); err != nil {
			t.Fatalf("check() rejected a valid list: %v", err)
		}
	})
}

// The end-to-end property that matters: whatever sequence of writes and truncates a program
// performs, flushing must produce exactly the bytes a real filesystem would hold. This composes
// Plan → fetch the demanded ranges → Splice, against a byte-map model of the whole file, which is
// the same shape as the differential oracle at the mount level — just without the mount.
func TestWriteFlushRoundTripMatchesModel(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(0xC0FFEE, 0x0DDBA11))

	for iter := range 3000 {
		// The object as it exists in storage, and the file as it should end up.
		storedSize := int64(rng.IntN(120))
		stored := make([]byte, storedSize)
		for i := range stored {
			stored[i] = byte('A' + rng.IntN(26))
		}
		want := append([]byte(nil), stored...)

		var l ExtentList
		m := newModel()

		// Drawn once. See TestExtentListPlanIsSelfConsistent for what re-rolling the bound each pass
		// costs: the program-length distribution collapses toward its short end.
		for range 1 + rng.IntN(8) {
			switch {
			case rng.IntN(8) == 0:
				size := int64(rng.IntN(150))
				if err := l.Truncate(size); err != nil {
					t.Fatalf("Truncate(%d): %v", size, err)
				}
				m.truncate(size)
				if size < int64(len(want)) {
					want = want[:size]
				} else {
					want = append(want, make([]byte, size-int64(len(want)))...)
				}

			default:
				offset := int64(rng.IntN(140))
				data := make([]byte, 1+rng.IntN(20))
				for i := range data {
					data[i] = byte('a' + rng.IntN(26))
				}
				if err := l.Add(offset, data); err != nil {
					t.Fatalf("Add(%d, %d bytes): %v", offset, len(data), err)
				}
				m.add(offset, data)
				if end := offset + int64(len(data)); end > int64(len(want)) {
					want = append(want, make([]byte, end-int64(len(want)))...)
				}
				copy(want[offset:], data)
			}
		}

		assertMatches(t, &l, m)

		p, err := l.Plan(storedSize)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if p.Size != int64(len(want)) {
			t.Fatalf("iter %d: plan Size %d, model file is %d bytes", iter, p.Size, len(want))
		}
		if p.Noop {
			// Nothing to write: the stored object must already be the answer.
			if !bytes.Equal(stored, want) {
				t.Fatalf("iter %d: plan says no-op but stored %q != want %q", iter, stored, want)
			}
			continue
		}

		// Serve the ranges the plan demanded, and only those — a caller cannot supply bytes the
		// plan never asked for, so this catches an under-specified ReadRanges.
		var base []Extent
		for _, r := range p.ReadRanges {
			base = append(base, Extent{Offset: r.Offset, Data: stored[r.Offset:r.End()]})
		}

		got, err := l.Splice(p.Size, base)
		if err != nil {
			t.Fatalf("Splice: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("iter %d: flush produced %q, want %q\nstored %q\nplan %+v\nextents %s",
				iter, got, want, stored, p, formatExtents(l.Extents()))
		}

		// The flush is now durable, so the extents are dropped and the object *is* the file. A read
		// after that must agree with the read before it — otherwise a flush silently changes what
		// the file says, which is the class of bug that made v0.10.0's cache return pre-write bytes.
		preRead := serveRead(t, &l, stored, 0, len(want)+8)
		l.Reset()
		postRead := serveRead(t, &l, got, 0, len(want)+8)
		if !bytes.Equal(preRead, postRead) {
			t.Fatalf("iter %d: read changed across flush: %q before, %q after", iter, preRead, postRead)
		}
		if !bytes.Equal(postRead, want) {
			t.Fatalf("iter %d: post-flush read = %q, want %q", iter, postRead, want)
		}
	}
}

// Reading back through the pending writes must agree with the same model, at every offset and
// length — including reads straddling the stored end, which is where v0.10.0 handed the kernel
// stale bytes.
func TestReadAtMatchesModel(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(0xBEEF, 0xCAFE))

	for iter := range 2000 {
		storedSize := int64(rng.IntN(80))
		stored := make([]byte, storedSize)
		for i := range stored {
			stored[i] = byte('A' + rng.IntN(26))
		}
		want := append([]byte(nil), stored...)

		var l ExtentList
		// Drawn once; see TestExtentListPlanIsSelfConsistent.
		for range 1 + rng.IntN(6) {
			if rng.IntN(6) == 0 {
				size := int64(rng.IntN(110))
				if err := l.Truncate(size); err != nil {
					t.Fatalf("Truncate(%d): %v", size, err)
				}
				if size < int64(len(want)) {
					want = want[:size]
				} else {
					want = append(want, make([]byte, size-int64(len(want)))...)
				}
				continue
			}

			offset := int64(rng.IntN(100))
			data := make([]byte, 1+rng.IntN(15))
			for i := range data {
				data[i] = byte('a' + rng.IntN(26))
			}
			if err := l.Add(offset, data); err != nil {
				t.Fatalf("Add: %v", err)
			}
			if end := offset + int64(len(data)); end > int64(len(want)) {
				want = append(want, make([]byte, end-int64(len(want)))...)
			}
			copy(want[offset:], data)
		}

		if got := l.Size(storedSize); got != int64(len(want)) {
			t.Fatalf("iter %d: Size = %d, model file is %d bytes", iter, got, len(want))
		}

		offset := int64(rng.IntN(120))
		length := 1 + rng.IntN(40)

		got := serveRead(t, &l, stored, offset, length)
		if wantBytes := modelRead(want, offset, length); !bytes.Equal(got, wantBytes) {
			t.Fatalf("iter %d: read(off=%d len=%d) = %q, model says %q",
				iter, offset, length, got, wantBytes)
		}
	}
}

// FuzzExtentList drives Add/Truncate from fuzzer bytes and asserts the invariants plus the
// flush round-trip. The operation-sequence fuzzer at the mount level is a larger job; this one
// covers the data structure underneath it, where a violated invariant is silent corruption.
func FuzzExtentList(f *testing.F) {
	f.Add([]byte{0, 0, 4, 'a', 'b', 'c', 'd'})
	f.Add([]byte{0, 8, 4, 'a', 'b', 'c', 'd', 1, 2})
	f.Add([]byte{0, 0, 2, 'x', 'y', 0, 1, 2, 'z', 'w'})

	f.Fuzz(func(t *testing.T, in []byte) {
		var l ExtentList
		m := newModel()

		stored := []byte("0123456789abcdefghijABCDEFGHIJ")
		want := append([]byte(nil), stored...)

		// Each op is: [kind, offset, length, data...]. Malformed tails simply end the sequence.
		for len(in) >= 3 {
			kind, offset, length := in[0], int64(in[1]), int(in[2])
			in = in[3:]

			if kind%4 == 3 {
				if err := l.Truncate(offset); err != nil {
					t.Fatalf("Truncate(%d): %v", offset, err)
				}
				m.truncate(offset)
				if offset < int64(len(want)) {
					want = want[:offset]
				} else {
					want = append(want, make([]byte, offset-int64(len(want)))...)
				}
				continue
			}

			if length > len(in) {
				length = len(in)
			}
			data := in[:length]
			in = in[length:]
			if len(data) == 0 {
				continue
			}

			if err := l.Add(offset, data); err != nil {
				t.Fatalf("Add(%d, %d bytes): %v", offset, len(data), err)
			}
			m.add(offset, data)
			if end := offset + int64(len(data)); end > int64(len(want)) {
				want = append(want, make([]byte, end-int64(len(want)))...)
			}
			copy(want[offset:], data)
		}

		assertMatches(t, &l, m)

		storedSize := int64(len(stored))
		if got := l.Size(storedSize); got != int64(len(want)) {
			t.Fatalf("Size = %d, model file is %d bytes", got, len(want))
		}

		// Reads must agree with the model at every offset, not only at the flush boundary.
		for off := int64(0); off <= int64(len(want))+4; off++ {
			got := serveRead(t, &l, stored, off, 7)
			if wantBytes := modelRead(want, off, 7); !bytes.Equal(got, wantBytes) {
				t.Fatalf("read(off=%d len=7) = %q, model says %q", off, got, wantBytes)
			}
		}

		p, err := l.Plan(storedSize)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if p.Noop {
			if !bytes.Equal(stored, want) {
				t.Fatalf("plan says no-op but stored %q != want %q", stored, want)
			}
			return
		}

		var base []Extent
		for _, r := range p.ReadRanges {
			if r.Offset < 0 || r.End() > l.StoredValid(storedSize) {
				t.Fatalf("ReadRange %+v outside the valid stored prefix of %d bytes",
					r, l.StoredValid(storedSize))
			}
			base = append(base, Extent{Offset: r.Offset, Data: stored[r.Offset:r.End()]})
		}

		got, err := l.Splice(p.Size, base)
		if err != nil {
			t.Fatalf("Splice: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("flush produced %q, want %q\nplan %+v\nextents %s",
				got, want, p, formatExtents(l.Extents()))
		}
	})
}

func BenchmarkExtentListAddSequential(b *testing.B) {
	data := make([]byte, 4096)
	for b.Loop() {
		var l ExtentList
		var off int64
		for range 1000 {
			if err := l.Add(off, data); err != nil {
				b.Fatal(err)
			}
			off += int64(len(data))
		}
		if l.Len() != 1 {
			b.Fatalf("sequential writes did not coalesce: %d extents", l.Len())
		}
	}
}

func BenchmarkExtentListAddSparse(b *testing.B) {
	data := make([]byte, 4096)
	for b.Loop() {
		var l ExtentList
		for i := range 1000 {
			// Descending offsets: every insert lands at the front, the worst case for the shift.
			if err := l.Add(int64(1000-i)*8192, data); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// TestUncoveredEndOfAnEmptyRange pins the degenerate case.
//
// A read whose computed range is empty — hi at or before lo — has nothing to fetch, and the answer has
// to be lo rather than hi. Returning hi would hand [Node.ReadRange] a range running backwards, which
// becomes a negative length at the backend, and a negative length is the C3 panic: `data[offset:end]`
// with end < offset kills the mount process and unmounts under every open descriptor.
func TestUncoveredEndOfAnEmptyRange(t *testing.T) {
	t.Parallel()

	var l ExtentList
	if err := l.Add(0, []byte("covered")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	tests := []struct {
		name   string
		lo, hi int64
	}{
		{"hi equals lo", 4, 4},
		{"hi before lo", 8, 4},
		{"both zero", 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := l.UncoveredEnd(tc.lo, tc.hi); got != tc.lo {
				t.Errorf("UncoveredEnd(%d, %d) = %d, want %d", tc.lo, tc.hi, got, tc.lo)
			}
		})
	}
}

// TestUncoveredStartOfAnEmptyRange is the mirror of TestUncoveredEndOfAnEmptyRange, and its answer is
// the mirror too: hi, not lo. Between them [Node.ReadRange] gets start >= end and reports no fetch,
// rather than a range running backwards — which reaches the backend as a negative length, and a
// negative length is the C3 panic that takes the mount down with every open descriptor.
func TestUncoveredStartOfAnEmptyRange(t *testing.T) {
	t.Parallel()

	var l ExtentList
	if err := l.Add(0, []byte("covered")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	tests := []struct {
		name   string
		lo, hi int64
	}{
		{"hi equals lo", 4, 4},
		{"hi before lo", 8, 4},
		{"both zero", 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := l.UncoveredStart(tc.lo, tc.hi); got != tc.hi {
				t.Errorf("UncoveredStart(%d, %d) = %d, want %d", tc.lo, tc.hi, got, tc.hi)
			}
		})
	}
}

// TestUncoveredStart walks the head-trimming cases, including the ones where it must not trim.
//
// Trimming too much is silent corruption: bytes the object has and the pending writes do not would be
// skipped by the fetch and then read back as whatever the caller's buffer held, or as zeros. So the
// clamp cases matter as much as the narrowing ones.
func TestUncoveredStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		writes []struct {
			off  int64
			data string
		}
		lo, hi int64
		want   int64
	}{
		{
			name: "no writes: nothing to trim",
			lo:   0, hi: 100, want: 0,
		},
		{
			name: "a write at the head moves the start past it",
			writes: []struct {
				off  int64
				data string
			}{{0, "0123456789"}},
			lo: 0, hi: 100, want: 10,
		},
		{
			name: "a write starting after lo does not move the start",
			writes: []struct {
				off  int64
				data string
			}{{20, "0123456789"}},
			lo: 0, hi: 100, want: 0,
		},
		{
			name: "a write straddling lo moves the start to its end",
			writes: []struct {
				off  int64
				data string
			}{{5, "0123456789"}},
			lo: 10, hi: 100, want: 15,
		},
		{
			name: "a write covering the whole range collapses it to hi",
			writes: []struct {
				off  int64
				data string
			}{{0, "0123456789"}},
			lo: 0, hi: 10, want: 10,
		},
		{
			name: "the start never runs past hi",
			writes: []struct {
				off  int64
				data string
			}{{0, "0123456789"}},
			lo: 0, hi: 4, want: 4,
		},
		{
			// Extents are coalesced when they touch, so a contiguous pair is one extent and one step
			// clears both. The point is that the answer is the far end, not the near one.
			name: "adjacent writes are cleared in one step",
			writes: []struct {
				off  int64
				data string
			}{
				{0, "aaaa"}, {4, "bbbb"},
			},
			lo: 0, hi: 100, want: 8,
		},
		{
			// A gap stops the trimming: those bytes are only in the object.
			name: "a gap after the first write stops the trim",
			writes: []struct {
				off  int64
				data string
			}{
				{0, "aaaa"}, {8, "bbbb"},
			},
			lo: 0, hi: 100, want: 4,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var l ExtentList
			for _, w := range tc.writes {
				if err := l.Add(w.off, []byte(w.data)); err != nil {
					t.Fatalf("Add(%d): %v", w.off, err)
				}
			}

			if got := l.UncoveredStart(tc.lo, tc.hi); got != tc.want {
				t.Errorf("UncoveredStart(%d, %d) = %d, want %d", tc.lo, tc.hi, got, tc.want)
			}
		})
	}
}
