package vfs

import (
	"fmt"
	"sort"
)

// Extent is a contiguous run of bytes at a known offset in a file.
//
// Data is owned by the Extent once added to an ExtentList; callers must not retain or mutate the
// slice they passed in. FUSE reuses its write buffers after the call returns, so [ExtentList.Add]
// copies.
type Extent struct {
	Offset int64
	Data   []byte
}

// End returns the offset one past the last byte of e.
func (e Extent) End() int64 { return e.Offset + int64(len(e.Data)) }

// Len returns the byte length of e.
func (e Extent) Len() int64 { return int64(len(e.Data)) }

// ExtentList is the set of mutations pending against one file: an ordered collection of dirty byte
// ranges, plus the effect of any truncation, none of which has reached the object store yet.
//
// This is the concept v0.10.0's write path lacked. It buffered one contiguous []byte plus a single
// offset, so it could represent only "a run of bytes starting somewhere" — and therefore rejected
// any write that did not continue the run, returning EIO for the access pattern SQLite, mmap
// writeback, tar, and HDF5 all use. An ExtentList represents an arbitrary set of ranges, including
// a sparse one, so no legitimate write pattern needs to be refused.
//
// Invariants over the extents, held by every method and checked by ExtentList.check in tests:
//
//   - sorted by Offset
//   - no two overlap
//   - no two are adjacent (a[i].End() != a[i+1].Offset) — adjacent runs are coalesced, so a
//     sequential write of N buffers yields one extent, not N
//   - none is empty
//
// The zero value is an empty list against whatever the object store currently holds. ExtentList is
// not safe for concurrent use; callers hold the per-file lock.
type ExtentList struct {
	extents []Extent

	// truncated records whether Truncate has been called since the last Reset, and if so what it
	// implies about the file and about the stored object.
	//
	// Two numbers are needed, not one, and conflating them destroys data. lastSize is the file's
	// length as of the most recent truncation, which sets the size a subsequent flush must produce.
	// minSize is the *smallest* size ever truncated to, which bounds how much of the stored object
	// still means anything: truncating to 4 and then writing at offset 83 leaves bytes 4..83 a hole,
	// not the bytes the object used to hold there. A single boolean "was this truncated" cannot
	// express that, and reading the stored object past minSize resurrects deleted content.
	truncated bool
	lastSize  int64
	minSize   int64
}

// Len returns the number of extents. It is not the number of dirty bytes; see [ExtentList.Bytes].
func (l *ExtentList) Len() int { return len(l.extents) }

// Bytes returns the total number of dirty bytes across all extents.
func (l *ExtentList) Bytes() int64 {
	var n int64
	for _, e := range l.extents {
		n += e.Len()
	}
	return n
}

// Extents returns the extents in offset order.
//
// The returned slice aliases internal storage and is invalidated by the next mutating call. Callers
// that retain it must copy.
func (l *ExtentList) Extents() []Extent { return l.extents }

// Empty reports whether there are no dirty bytes. It does not report whether a flush is unnecessary
// — a truncation with nothing dirty is still pending work. See [FlushPlan.Noop].
func (l *ExtentList) Empty() bool { return len(l.extents) == 0 }

// Reset drops every extent and forgets any pending truncation, returning the list to "the object
// store is authoritative".
//
// Call this only once the data is durable. v0.10.0's flush path deleted its buffer on success
// without checking whether another flush was in flight, so writes arriving during an upload were
// annihilated — counted as flushed while never having been sent.
func (l *ExtentList) Reset() {
	l.extents = nil
	l.truncated = false
	l.lastSize = 0
	l.minSize = 0
}

// MaxEnd returns the offset one past the highest dirty byte, or 0 when nothing is dirty. It is a
// lower bound on the file size, not the size; see [ExtentList.Size].
func (l *ExtentList) MaxEnd() int64 {
	if len(l.extents) == 0 {
		return 0
	}
	return l.extents[len(l.extents)-1].End()
}

// Size returns the file's logical length given that the object store holds storedSize bytes.
//
// This is what stat must report and what a read must treat as EOF. It is not max(storedSize,
// MaxEnd): a pending truncation can make the file shorter than the stored object, and can make it
// longer than the highest dirty byte.
func (l *ExtentList) Size(storedSize int64) int64 {
	if l.truncated {
		return max(l.lastSize, l.MaxEnd())
	}
	return max(storedSize, l.MaxEnd())
}

// StoredValid returns how many leading bytes of the stored object still contribute to the file's
// contents. Bytes at or past it were destroyed by a truncation and must never be read back, even
// when the file has since grown past them again — they are a hole, and a hole reads as zeros.
func (l *ExtentList) StoredValid(storedSize int64) int64 {
	if l.truncated {
		return min(storedSize, l.minSize)
	}
	return storedSize
}

// Add records a write of data at offset, copying data.
//
// Later writes win. Where the new range overlaps existing extents the new bytes replace the old
// ones, and any extents left touching or overlapping are coalesced into one.
//
// Last-writer-wins is the whole point and is easy to get backwards. v0.10.0's mergeWrites guarded
// its overlay with "is the new end past the current end," so writing shorter new content over
// longer old content kept the old content — `echo NEW > f` over a file holding OLD left the file
// reading OLD, with no error anywhere.
//
// A zero-length write is a no-op. A negative offset is an error: a caller computing one has a bug,
// and clamping it to zero would corrupt the file at offset 0, where corruption is maximally
// destructive.
func (l *ExtentList) Add(offset int64, data []byte) error {
	if offset < 0 {
		return fmt.Errorf("%w: negative write offset %d", ErrInvalid, offset)
	}
	if len(data) == 0 {
		return nil
	}

	newExt := Extent{Offset: offset, Data: append([]byte(nil), data...)}
	end := newExt.End()

	// Find the extents that touch [offset, end]. Adjacency counts: an extent ending exactly at
	// offset is merged, so a sequential write collapses into one extent rather than accumulating
	// one per write() call.
	lo := sort.Search(len(l.extents), func(i int) bool { return l.extents[i].End() >= offset })
	hi := lo
	for hi < len(l.extents) && l.extents[hi].Offset <= end {
		hi++
	}

	if lo == hi {
		// Touches nothing: plain insert at lo.
		l.extents = append(l.extents, Extent{})
		copy(l.extents[lo+1:], l.extents[lo:])
		l.extents[lo] = newExt
		return nil
	}

	// Merge [lo, hi) with newExt. The merged extent spans the lowest start to the highest end;
	// newExt's bytes are copied last so they win on overlap.
	start := min(l.extents[lo].Offset, newExt.Offset)
	stop := newExt.End()
	for _, e := range l.extents[lo:hi] {
		if e.End() > stop {
			stop = e.End()
		}
	}

	merged := Extent{Offset: start, Data: make([]byte, stop-start)}
	for _, e := range l.extents[lo:hi] {
		copy(merged.Data[e.Offset-start:], e.Data)
	}
	copy(merged.Data[newExt.Offset-start:], newExt.Data)

	tail := append([]Extent(nil), l.extents[hi:]...)
	l.extents = append(append(l.extents[:lo], merged), tail...)
	return nil
}

// ReadAt returns what a read of len(buf) bytes at offset sees, given an object store holding
// storedSize bytes.
//
// The caller must have already filled the leading bytes of buf with the stored object's contents
// for the overlap of the request with [0, [ExtentList.StoredValid](storedSize)) — that is the only
// part of the request the backend can answer. ReadAt zeroes the rest, overlays the pending writes,
// and returns how many leading bytes of buf are valid: the request length, or fewer at EOF.
//
// Two things this gets right that v0.10.0 did not. A read is served from the pending writes as well
// as from storage, so read-after-write on one descriptor sees the new bytes — v0.10.0 consulted the
// cache and the backend but never the write buffer, and returned pre-write content for up to the
// five-minute cache TTL. And a hole inside the file reads as zeros rather than as a short read: the
// distinction between "past the stored object" and "past the end of the file" only exists because
// this type knows the file's logical size.
func (l *ExtentList) ReadAt(buf []byte, offset, storedSize int64) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("%w: negative read offset %d", ErrInvalid, offset)
	}
	if storedSize < 0 {
		return 0, fmt.Errorf("%w: negative stored size %d", ErrInvalid, storedSize)
	}
	if len(buf) == 0 {
		return 0, nil
	}

	size := l.Size(storedSize)
	if offset >= size {
		return 0, nil
	}
	valid := int(min(int64(len(buf)), size-offset))

	// Everything the caller could not have filled from storage is a hole until an extent says
	// otherwise. buf is caller-allocated, so its contents there cannot be assumed to be anything.
	filled := int(max(0, min(l.StoredValid(storedSize)-offset, int64(valid))))
	for i := filled; i < valid; i++ {
		buf[i] = 0
	}

	reqEnd := offset + int64(valid)
	for _, e := range l.extents {
		if e.End() <= offset {
			continue
		}
		if e.Offset >= reqEnd {
			break
		}
		from := int(max(e.Offset, offset) - offset)
		to := int(min(e.End(), reqEnd) - offset)
		copy(buf[from:to], e.Data[int64(from)+offset-e.Offset:])
	}

	return valid, nil
}

// FlushPlan describes how to make an ExtentList durable against an object of a known size.
type FlushPlan struct {
	// Noop is true when there is nothing to write: no dirty bytes, no size change, and no stored
	// bytes invalidated.
	//
	// This is distinguished from WholeObject explicitly because conflating them destroys data. A
	// caller that treats "not a whole-object write" as "fetch ReadRanges and splice" would, for an
	// empty plan, splice nothing and PUT Size zero bytes over an intact object.
	Noop bool

	// WholeObject is true when the pending state determines every byte of the result, so the object
	// can be replaced with a single PutObject and no read is required.
	WholeObject bool

	// Size is the byte length the object will have once written.
	Size int64

	// ReadRanges lists the byte ranges of the *current* object that must be fetched and spliced with
	// the pending writes before the result can be uploaded, in ascending order and disjoint. Empty
	// when WholeObject or Noop is true. Every range lies within [ExtentList.StoredValid], so a plan
	// can never ask for bytes a truncation destroyed.
	ReadRanges []Range
}

// Range is a half-open byte interval [Offset, Offset+Length).
type Range struct {
	Offset int64
	Length int64
}

// End returns the offset one past the last byte of r.
func (r Range) End() int64 { return r.Offset + r.Length }

// Plan computes what must happen to flush l against an object currently storedSize bytes long. Pass
// storedSize 0 for an object that does not yet exist.
//
// This is the decision v0.10.0 never made. Its flush callback received the offset and discarded it:
//
//	flushCallback := func(key string, data []byte, offset int64) error {
//	    return a.backend.PutObject(context.Background(), key, data)
//	}
//
// PutObject replaces the whole object, so every write at a non-zero offset truncated the file to
// just the bytes written — appending one byte to a 1 MiB file left a 1-byte object, reported as
// success. Plan makes the read-modify-write requirement explicit and hard to drop silently: a
// caller that ignores ReadRanges is visibly wrong rather than quietly destructive.
//
// Plan takes no truncate flag. It once did, and that was a defect of the same family it exists to
// prevent: a boolean cannot say *what* the file was truncated to, so a truncate followed by a write
// past the old end re-fetched — and so restored — bytes the truncate had destroyed. The truncation
// state lives on the list, where [ExtentList.Truncate] records it and no caller can forget to pass
// it.
func (l *ExtentList) Plan(storedSize int64) (FlushPlan, error) {
	if storedSize < 0 {
		return FlushPlan{}, fmt.Errorf("%w: negative stored size %d", ErrInvalid, storedSize)
	}

	size := l.Size(storedSize)
	bound := l.StoredValid(storedSize)

	if l.Empty() && size == storedSize && bound == storedSize {
		return FlushPlan{Noop: true, Size: size}, nil
	}

	// Walk the gaps between extents within [0, size). A gap must be fetched only where the stored
	// object still holds meaningful bytes; past that it is a hole, which Splice fills with zeros for
	// free and which a range GET could not have returned anyway.
	var reads []Range
	var cursor int64
	for _, e := range l.extents {
		if e.Offset > cursor {
			if end := min(e.Offset, bound); end > cursor {
				reads = append(reads, Range{Offset: cursor, Length: end - cursor})
			}
		}
		cursor = e.End()
	}
	if cursor < size {
		if end := min(size, bound); end > cursor {
			reads = append(reads, Range{Offset: cursor, Length: end - cursor})
		}
	}

	return FlushPlan{WholeObject: len(reads) == 0, Size: size, ReadRanges: reads}, nil
}

// Splice assembles the final object body: exactly size bytes, built from base — the fetched bytes of
// the current object, each fragment at its absolute offset, as returned for the ranges in
// [FlushPlan.ReadRanges] — with the pending writes laid over the top.
//
// Bytes covered by neither base nor an extent are zeros, matching POSIX sparse-file semantics.
// Fragments and extents extending past size are truncated rather than rejected, so a truncate
// racing a flush shortens the result instead of failing it.
func (l *ExtentList) Splice(size int64, base []Extent) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("%w: negative size %d", ErrInvalid, size)
	}
	for i, b := range base {
		if b.Offset < 0 {
			return nil, fmt.Errorf("%w: base fragment %d has negative offset %d", ErrInvalid, i, b.Offset)
		}
	}

	out := make([]byte, size)

	for _, b := range base {
		if b.Offset >= size || b.Len() == 0 {
			continue
		}
		to := min(b.End(), size)
		copy(out[b.Offset:to], b.Data[:to-b.Offset])
	}

	for _, e := range l.extents {
		if e.Offset >= size {
			break
		}
		to := min(e.End(), size)
		copy(out[e.Offset:to], e.Data[:to-e.Offset])
	}

	return out, nil
}

// Truncate records that the file has been resized to size, discarding dirty bytes at or beyond it.
//
// Growing a file adds no extents: the new bytes are a hole, which reads as zeros and which
// [ExtentList.Splice] materializes at flush time. That keeps `truncate -s 1T` an O(1) metadata
// operation rather than a terabyte allocation.
//
// Shrinking also invalidates the stored object past size, permanently — growing back afterwards
// does not restore those bytes, it creates a hole. See the truncated field for why that needs its
// own piece of state.
func (l *ExtentList) Truncate(size int64) error {
	if size < 0 {
		return fmt.Errorf("%w: negative truncate size %d", ErrInvalid, size)
	}

	kept := l.extents[:0]
	for _, e := range l.extents {
		switch {
		case e.Offset >= size:
			// Wholly past the new end; drop it.
		case e.End() > size:
			e.Data = e.Data[:size-e.Offset]
			kept = append(kept, e)
		default:
			kept = append(kept, e)
		}
	}
	l.extents = kept

	if l.truncated {
		l.minSize = min(l.minSize, size)
	} else {
		l.truncated = true
		l.minSize = size
	}
	l.lastSize = size

	return nil
}

// check verifies the type invariants. A violation is a bug in this package, not a caller error;
// tests assert it after every mutation.
func (l *ExtentList) check() error {
	if l.truncated {
		if l.minSize < 0 || l.lastSize < 0 {
			return fmt.Errorf("negative truncation state: minSize %d lastSize %d", l.minSize, l.lastSize)
		}
		if l.minSize > l.lastSize {
			return fmt.Errorf("minSize %d exceeds lastSize %d", l.minSize, l.lastSize)
		}
	}

	for i, e := range l.extents {
		if len(e.Data) == 0 {
			return fmt.Errorf("extent %d is empty", i)
		}
		if e.Offset < 0 {
			return fmt.Errorf("extent %d has negative offset %d", i, e.Offset)
		}
		if i == 0 {
			continue
		}
		prev := l.extents[i-1]
		if e.Offset < prev.End() {
			return fmt.Errorf("extent %d [%d,%d) overlaps extent %d [%d,%d)",
				i, e.Offset, e.End(), i-1, prev.Offset, prev.End())
		}
		if e.Offset == prev.End() {
			return fmt.Errorf("extent %d [%d,%d) is adjacent to extent %d [%d,%d); should have coalesced",
				i, e.Offset, e.End(), i-1, prev.Offset, prev.End())
		}
	}
	return nil
}
