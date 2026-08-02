package cache

import (
	"bytes"
	"fmt"
	"strconv"
)

// The byte-range caches are addressed in fixed-size chunks, and this file is the one place that
// decides what a chunk is. LRUCache and PersistentCache each carried their own copy of the keying
// logic, character for character, which is how they came to share the same three defects.
//
// # What was wrong with the old key
//
// It was fmt.Sprintf("%s:%d:%d", key, offset, size) — the *requested length* was part of the identity
// of the cached bytes. That one term makes the cache structurally unable to hit in the cases that
// matter, which was verified by execution rather than by reading:
//
//	Put("f", 0, <10240 bytes>)   stores  f:0:10240
//	Get("f", 0, 131072)          asks    f:0:131072   → MISS
//	Get("f", 0, 4096)            asks    f:0:4096     → MISS
//
// The first is a whole-file read: a caller reads into a 128 KiB kernel buffer, so the length it asks
// for is its buffer size and never the file's length. The second is any short re-read of bytes already
// held. Both miss, forever, no matter how often they repeat. Only a reader that happens to request
// exactly the length previously stored ever hits — and the metadata cache in the FUSE layer could not
// even do that, since it stored ~138 bytes under a Get that always asked for 8192.
//
// The delimiter was wrong too. Delete matched cacheKey[:len(key)] == key, so Delete("logs/app")
// removed logs/app2 and logs/appendix along with its target. Verified by execution: all three gone.
//
// # What replaces it
//
// An entry holds one contiguous run of bytes within one chunk of one object, identified by
// (object key, chunk index). A read is served by locating the chunks its range touches and checking
// that each one's run covers the part needed. Length is a property of the request, not of the stored
// bytes, which is the distinction the old key collapsed.
//
// Runs, rather than whole chunks or chunk prefixes, because of how reads actually arrive. A
// sequential reader is handed 128 KiB at a time, so its second read starts at 131072 — partway into
// chunk 0. An entry that could only be anchored at its chunk's start would cache that reader's first
// buffer and then nothing at all, for the whole file. Runs coalesce instead: eight 128 KiB reads
// become one 1 MiB entry.

// ChunkSize is the granularity of a cache entry.
//
// 1 MiB is chosen against the two access patterns that matter rather than as a round number. A
// sequential reader arrives in kernel-sized reads (128 KiB on Linux, up to 1 MiB with FUSE big
// writes), so this coalesces several of them into one entry instead of one entry per read. A random
// reader's 4 KiB read costs one 4 KiB entry, since a run is only as long as what was read.
//
// It is deliberately not the S3 read chunk size (16 MiB). That figure sizes a *network* request, where
// the tradeoff is round-trip latency against wasted transfer; this one bounds a *memory* entry, where
// it decides eviction granularity. v0.10.0 conflated the two by chunking its cache Put at 16 MiB,
// which — because of the length-in-key defect above — was unreachable code regardless.
const ChunkSize int64 = 1 << 20

// chunkIndexOf returns the index of the chunk containing offset.
func chunkIndexOf(offset int64) int64 {
	return offset / ChunkSize
}

// chunkStart returns the first byte offset belonging to a chunk.
func chunkStart(index int64) int64 {
	return index * ChunkSize
}

// entryKey is the map key for one chunk of one object.
//
// Nothing parses this back apart. Deletion consults an explicit object-to-chunks index instead, because
// recovering the object name from the composed key is what produced the over-deletion bug: the original
// Delete matched a bare prefix with no delimiter at all, so Delete("logs/app") also flushed "logs/app2"
// and "logs/appendix".
//
// The separator is NUL rather than ":" because a prefix-with-delimiter scheme is still wrong with ":".
// S3 object keys may contain ":" themselves, so "logs/app" + ":" is a prefix of the composed key for
// "logs/app:0" — and Delete("logs/app") would flush it. Verified by mutation: with ":" as the separator
// and Delete matching key+":" as a prefix, TestDeleteRemovesOnlyItsOwnObject fails on exactly that pair.
// NUL is the one byte an S3 key cannot hold, so with it no object name can forge a boundary.
//
// Both defenses are deliberate. The index is the mechanism, and it would be correct even with ":". The
// separator makes the composed key unambiguous on its own, so a future reader who does reach for prefix
// matching gets a sound answer rather than a subtle one.
func entryKey(key string, index int64) string {
	return key + "\x00" + strconv.FormatInt(index, 10)
}

// chunkSpan returns the inclusive range of chunk indices that [offset, offset+size) touches, and
// whether the request describes a bounded range of bytes at all.
//
// ok is false for a non-positive size rather than defaulting to something plausible. A size of zero or
// less means "everything from offset" to the S3 backend, and no chunk range can bound that; leaving
// it implicit is the same undefined-size hole that produced the C3 panic in the read path, so it is
// answered explicitly here and refused.
func chunkSpan(offset, size int64) (first, last int64, ok bool) {
	if offset < 0 || size <= 0 {
		return 0, 0, false
	}

	// The end is derived by addition, so it can overflow for a size near MaxInt64 — and a wrapped
	// negative end would yield last < first, i.e. a silently empty span that reads as "nothing to
	// look up" rather than as the absurd request it is. Clamping keeps it bounded, if large.
	end := offset + size
	if end < offset {
		end = maxInt64
	}

	return chunkIndexOf(offset), chunkIndexOf(end - 1), true
}

const maxInt64 = int64(^uint64(0) >> 1)

// chunkPiece is one contiguous run of bytes lying entirely within a single chunk.
//
// start is an absolute object offset, not an offset within the chunk, because every comparison made
// against it is against a caller's absolute offset. Storing it relative would mean converting at each
// use, and each conversion is a place to get the arithmetic wrong.
type chunkPiece struct {
	index int64
	start int64
	data  []byte
}

// end returns the absolute offset one past the last byte of the run.
func (p chunkPiece) end() int64 {
	return p.start + int64(len(p.data))
}

// String renders a piece for diagnostics.
func (p chunkPiece) String() string {
	return fmt.Sprintf("chunk %d [%d,%d)", p.index, p.start, p.end())
}

// splitIntoChunks divides data written at offset into per-chunk runs.
//
// Every byte of data lands in exactly one piece: the split is at chunk boundaries only, so a write
// beginning or ending partway into a chunk keeps its partial run rather than being dropped. The first
// and last pieces are the ones commonly short, which is the ordinary case rather than an edge — a
// 4 KiB read at offset 131072 is a single 4 KiB run in chunk 0.
func splitIntoChunks(offset int64, data []byte) []chunkPiece {
	if offset < 0 || len(data) == 0 {
		return nil
	}

	end := offset + int64(len(data))
	if end < offset {
		// Overflow: len(data) cannot approach MaxInt64 in practice, but a wrapped end would make the
		// loop below produce garbage rather than nothing.
		return nil
	}

	pieces := make([]chunkPiece, 0, (int64(len(data))/ChunkSize)+2)

	for index := chunkIndexOf(offset); chunkStart(index) < end; index++ {
		lo := max(chunkStart(index), offset)
		hi := min(chunkStart(index)+ChunkSize, end)

		pieces = append(pieces, chunkPiece{
			index: index,
			start: lo,
			data:  data[lo-offset : hi-offset],
		})
	}

	return pieces
}

// coalesce merges a newly read run into the run an entry already holds, returning the run to store.
//
// The two runs are joined only when they overlap or abut. Disjoint runs cannot both be kept — an entry
// holds one contiguous range, and inventing the bytes in the gap is not an option — so the new run
// wins, on the reasoning that the most recent read is the better predictor of the next one.
//
// Where they overlap, the two runs must agree, and when they do not the newer one replaces the older
// outright.
//
// The alternative — overlaying the newer bytes onto the older run and keeping the union — synthesizes
// a buffer that was never observed. Overwriting "OLD CONTENT HERE" with "NEW" would leave the cache
// holding "NEW CONTENT HERE", which is not what the object held before the write or after it. Serving
// stale bytes is a bounded failure that invalidation and the TTL exist to contain; serving fabricated
// bytes is not a failure any other mechanism catches, because no version of the object ever contained
// them.
//
// Comparing the overlap is what makes that distinction available, and it is close to free where it
// matters: sequential reads abut rather than overlap, so there is nothing to compare. Note this is not
// an integrity check — two reads of the same version always agree, so a disagreement means the object
// changed between them, and the response is to keep the newer reading rather than to raise an error.
//
// An earlier draft returned the existing run unchanged whenever it already spanned the incoming range,
// which meant a write-then-read returned the pre-write content. The test written to catch that could
// not, because it generated both runs' bytes from a single offset-derived formula — so the two runs
// always agreed, and a discarded run was indistinguishable from a kept one.
//
// The returned piece never aliases incoming.data. Ownership is settled here rather than at the call
// site because each path through this function has a different answer, and a caller that has to work
// out which one it took will eventually get it wrong.
func coalesce(existing, incoming chunkPiece) chunkPiece {
	// Nothing held, or disjoint from what is held: the incoming run stands alone. Disjoint runs cannot
	// be joined at all, since the bytes between them were never read.
	if len(existing.data) == 0 || incoming.end() < existing.start || incoming.start > existing.end() {
		return clonePiece(incoming)
	}

	// Where the two runs overlap, they describe the same bytes of the same object and must agree. A
	// disagreement means the object changed between the reads, and only the newer reading can be
	// trusted.
	from := max(existing.start, incoming.start)
	to := min(existing.end(), incoming.end())

	if from < to && !bytes.Equal(
		existing.data[from-existing.start:to-existing.start],
		incoming.data[from-incoming.start:to-incoming.start],
	) {
		return clonePiece(incoming)
	}

	start := min(existing.start, incoming.start)
	end := max(existing.end(), incoming.end())

	// Already covered by what is held, and in agreement with it: nothing to do.
	if start == existing.start && end == existing.end() {
		return existing
	}

	merged := make([]byte, end-start)
	copy(merged[existing.start-start:], existing.data)
	copy(merged[incoming.start-start:], incoming.data)

	return chunkPiece{index: existing.index, start: start, data: merged}
}

// covers reports whether the run holds every byte of [from, to).
func (p chunkPiece) covers(from, to int64) bool {
	return p.start <= from && p.end() >= to
}

// clonePiece copies a run's bytes so the cache never retains a caller's buffer.
//
// splitIntoChunks and coalesce both return pieces that may alias their input — the FUSE read path
// passes the slice it is about to hand the kernel, and a caller that reuses its read buffer would
// otherwise mutate cached bytes in place, turning the cache into a source of wrong data rather than
// stale data. Copying at the one boundary where bytes enter the cache is the cheap way to make that
// impossible.
func clonePiece(p chunkPiece) chunkPiece {
	data := make([]byte, len(p.data))
	copy(data, p.data)

	return chunkPiece{index: p.index, start: p.start, data: data}
}
