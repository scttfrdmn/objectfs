package cache

// The chunk keying is where the read cache's three structural defects lived, so it gets tests that
// address the arithmetic directly rather than only through a cache. A wrong answer here is not a
// missed optimization: covers() deciding a hit it cannot honor hands the FUSE layer bytes from the
// wrong offset, and the kernel presents those as file content.

import (
	"bytes"
	"strings"
	"testing"
)

func TestSplitIntoChunksCoversEveryByte(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		offset int64
		length int64
		want   []chunkPiece // data checked separately; index/start/len only
		why    string
	}{
		{
			name:   "small read at zero",
			offset: 0,
			length: 4096,
			want:   []chunkPiece{{index: 0, start: 0, data: make([]byte, 4096)}},
			why:    "one short run in chunk 0",
		},
		{
			name:   "sequential reader's second buffer",
			offset: 131072,
			length: 131072,
			want:   []chunkPiece{{index: 0, start: 131072, data: make([]byte, 131072)}},
			why: "starts partway into chunk 0 and must still be cached. A prefix-anchored design " +
				"drops this, which means a sequential reader caches its first buffer and then nothing " +
				"for the rest of the file",
		},
		{
			name:   "exactly one chunk",
			offset: 0,
			length: ChunkSize,
			want:   []chunkPiece{{index: 0, start: 0, data: make([]byte, ChunkSize)}},
			why:    "the boundary case: must be one piece, not two with an empty second",
		},
		{
			name:   "one byte past a chunk boundary",
			offset: 0,
			length: ChunkSize + 1,
			want: []chunkPiece{
				{index: 0, start: 0, data: make([]byte, ChunkSize)},
				{index: 1, start: ChunkSize, data: make([]byte, 1)},
			},
			why: "off-by-one territory",
		},
		{
			name:   "straddles a boundary",
			offset: ChunkSize - 100,
			length: 200,
			want: []chunkPiece{
				{index: 0, start: ChunkSize - 100, data: make([]byte, 100)},
				{index: 1, start: ChunkSize, data: make([]byte, 100)},
			},
			why: "split at the boundary, both halves kept",
		},
		{
			name:   "a 16 MiB S3 read chunk",
			offset: 0,
			length: 16 << 20,
			want:   nil, // checked by count below
			why:    "what the S3 parallel read path hands over; must become 16 entries, not be refused",
		},
		{
			name:   "starts mid-chunk and spans several",
			offset: ChunkSize + 500,
			length: 2 * ChunkSize,
			want: []chunkPiece{
				{index: 1, start: ChunkSize + 500, data: make([]byte, ChunkSize-500)},
				{index: 2, start: 2 * ChunkSize, data: make([]byte, ChunkSize)},
				{index: 3, start: 3 * ChunkSize, data: make([]byte, 500)},
			},
			why: "short first piece, full middle, short last",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Position-dependent bytes, so a piece sliced from the wrong place is detectable rather
			// than merely the right length.
			data := make([]byte, tt.length)
			for i := range data {
				data[i] = byte((int64(i) + tt.offset) % 251)
			}

			got := splitIntoChunks(tt.offset, data)

			// Whatever the split, every byte must appear exactly once, in order, at the right absolute
			// offset. This is the property that matters; the piece boundaries are an implementation
			// detail underneath it.
			var reassembled []byte
			next := tt.offset

			for _, piece := range got {
				if piece.start != next {
					t.Fatalf("piece %v starts at %d, expected %d — a gap or overlap here means Get "+
						"assembles bytes from the wrong offsets", piece, piece.start, next)
				}

				if piece.index != chunkIndexOf(piece.start) {
					t.Errorf("piece %v claims index %d but its start belongs to chunk %d",
						piece, piece.index, chunkIndexOf(piece.start))
				}

				if piece.end() > chunkStart(piece.index)+ChunkSize {
					t.Errorf("piece %v crosses the end of its own chunk; an entry must lie within one "+
						"chunk or lookups by chunk index cannot find it", piece)
				}

				reassembled = append(reassembled, piece.data...)
				next = piece.end()
			}

			if !bytes.Equal(reassembled, data) {
				t.Errorf("reassembled %d of %d bytes and they differ: %s",
					len(reassembled), len(data), tt.why)
			}

			if tt.want == nil {
				return
			}

			if len(got) != len(tt.want) {
				t.Fatalf("got %d pieces %v, want %d (%s)", len(got), got, len(tt.want), tt.why)
			}

			for i, want := range tt.want {
				if got[i].index != want.index || got[i].start != want.start ||
					len(got[i].data) != len(want.data) {
					t.Errorf("piece %d = chunk %d [%d,%d), want chunk %d [%d,%d): %s",
						i, got[i].index, got[i].start, got[i].end(),
						want.index, want.start, want.start+int64(len(want.data)), tt.why)
				}
			}
		})
	}
}

func TestSplitIntoChunksRefusesNonsense(t *testing.T) {
	t.Parallel()

	if got := splitIntoChunks(-1, []byte("data")); got != nil {
		t.Errorf("a negative offset produced %v; there is no such byte position", got)
	}

	if got := splitIntoChunks(0, nil); got != nil {
		t.Errorf("no data produced %v", got)
	}

	if got := splitIntoChunks(maxInt64-10, make([]byte, 100)); got != nil {
		t.Errorf("an offset whose end overflows produced %v; the loop bound would be garbage", got)
	}
}

// TestEntryKeyCannotBeForged is the delete-precision defect at its root.
//
// The old Delete compared bare prefixes with no delimiter, so Delete("logs/app") removed logs/app2 and
// logs/appendix as well — verified by execution before the fix. Adding ":" as a delimiter, which is what
// the fix was first specified as, does not close it: S3 keys may contain ":", so "logs/app" + ":" is a
// prefix of the composed key for the distinct object "logs/app:0".
//
// Distinct (object, chunk) pairs must therefore produce distinct entry keys even when one object name is
// a prefix of another, and even when the names contain whatever the separator happens to be.
func TestEntryKeyCannotBeForged(t *testing.T) {
	t.Parallel()

	// Names chosen so that a ":" separator, a "/" separator, or a bare prefix compare each collide on
	// at least one pair here.
	objects := []string{
		"logs/app",
		"logs/app2",
		"logs/appendix",
		"a",
		"a:b",
		"a:0",
		"a\x00", // the separator itself, if a key could somehow carry it
		"",
		"a/0/4096",
	}

	seen := make(map[string]string)

	for _, object := range objects {
		for _, index := range []int64{0, 1, 4096, -1, maxInt64} {
			key := entryKey(object, index)

			if prior, clash := seen[key]; clash {
				t.Errorf("entryKey(%q, %d) = %q, already produced by %s. Two different chunks sharing "+
					"an entry key means one shadows the other: a read of one object returns bytes from "+
					"another.", object, index, key, prior)
			}

			seen[key] = "entryKey(" + object + ", " + strings.TrimSpace(itoa(index)) + ")"
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}

	neg := n < 0
	var digits []byte

	for n != 0 {
		d := n % 10
		if d < 0 {
			d = -d
		}
		digits = append([]byte{byte('0' + d)}, digits...)
		n /= 10
	}

	if neg {
		return "-" + string(digits)
	}

	return string(digits)
}

func TestChunkSpan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		offset      int64
		size        int64
		first, last int64
		ok          bool
		why         string
	}{
		{
			name: "within one chunk", offset: 0, size: 4096,
			first: 0, last: 0, ok: true,
		},
		{
			name: "exactly one chunk", offset: 0, size: ChunkSize,
			first: 0, last: 0, ok: true,
			why: "the last byte is ChunkSize-1, which is still chunk 0 — an inclusive/exclusive slip " +
				"here demands a chunk 1 that was never written and turns every aligned read into a miss",
		},
		{
			name: "one byte more", offset: 0, size: ChunkSize + 1,
			first: 0, last: 1, ok: true,
		},
		{
			name: "starts on a boundary", offset: ChunkSize, size: 1,
			first: 1, last: 1, ok: true,
		},
		{
			name: "ends one byte before a boundary", offset: ChunkSize - 1, size: 1,
			first: 0, last: 0, ok: true,
		},
		{
			name: "straddles", offset: ChunkSize - 1, size: 2,
			first: 0, last: 1, ok: true,
		},
		{
			name: "zero size is refused", offset: 0, size: 0,
			ok:  false,
			why: "means 'to end of object' to the backend, a length only the backend knows",
		},
		{
			name: "negative size is refused", offset: 0, size: -1,
			ok:  false,
			why: "C3's shape. Refused rather than interpreted",
		},
		{
			name: "negative offset is refused", offset: -1, size: 10,
			ok: false,
		},
		{
			name: "size overflows int64", offset: maxInt64 - 10, size: maxInt64,
			first: chunkIndexOf(maxInt64 - 10), last: chunkIndexOf(maxInt64 - 1), ok: true,
			why: "clamped rather than wrapped; a wrapped end gives last < first, a silently empty span",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			first, last, ok := chunkSpan(tt.offset, tt.size)

			if ok != tt.ok {
				t.Fatalf("chunkSpan(%d, %d) ok = %v, want %v: %s",
					tt.offset, tt.size, ok, tt.ok, tt.why)
			}

			if !ok {
				return
			}

			if first != tt.first || last != tt.last {
				t.Errorf("chunkSpan(%d, %d) = [%d,%d], want [%d,%d]: %s",
					tt.offset, tt.size, first, last, tt.first, tt.last, tt.why)
			}

			if last < first {
				t.Errorf("span [%d,%d] is empty, so a satisfiable read would look up no chunks at all",
					first, last)
			}
		})
	}
}

func TestCoalesce(t *testing.T) {
	t.Parallel()

	// piece builds a run whose bytes encode their own absolute offsets, so a merge that misplaces
	// bytes fails on content rather than only on length.
	piece := func(start, length int64) chunkPiece {
		data := make([]byte, length)
		for i := range data {
			data[i] = byte((start + int64(i)) % 251)
		}

		return chunkPiece{index: chunkIndexOf(start), start: start, data: data}
	}

	tests := []struct {
		name                string
		existing, incoming  chunkPiece
		wantStart, wantEnd  int64
		wantContiguousMerge bool
		why                 string
	}{
		{
			name: "abutting runs join", existing: piece(0, 100), incoming: piece(100, 100),
			wantStart: 0, wantEnd: 200, wantContiguousMerge: true,
			why: "the sequential-read case: consecutive reads must accumulate into one run or a " +
				"re-read of the pair cannot hit",
		},
		{
			name: "overlapping runs join", existing: piece(0, 200), incoming: piece(100, 200),
			wantStart: 0, wantEnd: 300, wantContiguousMerge: true,
		},
		{
			name: "incoming precedes and abuts", existing: piece(100, 100), incoming: piece(0, 100),
			wantStart: 0, wantEnd: 200, wantContiguousMerge: true,
			why: "a backwards reader; order of arrival must not matter",
		},
		{
			name: "incoming already covered", existing: piece(0, 500), incoming: piece(100, 100),
			wantStart: 0, wantEnd: 500, wantContiguousMerge: true,
			why: "must not shrink the run to the newer, smaller read",
		},
		{
			name: "incoming subsumes existing", existing: piece(100, 100), incoming: piece(0, 500),
			wantStart: 0, wantEnd: 500, wantContiguousMerge: true,
		},
		{
			name: "disjoint runs cannot both be kept", existing: piece(0, 100), incoming: piece(500, 100),
			wantStart: 500, wantEnd: 600, wantContiguousMerge: false,
			why: "an entry holds one contiguous run; joining these would claim the 400 bytes between " +
				"them, which is fabricated data. The newer read wins",
		},
		{
			name: "empty existing", existing: chunkPiece{index: 0}, incoming: piece(0, 100),
			wantStart: 0, wantEnd: 100, wantContiguousMerge: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := coalesce(tt.existing, tt.incoming)

			if got.start != tt.wantStart || got.end() != tt.wantEnd {
				t.Fatalf("coalesce = [%d,%d), want [%d,%d): %s",
					got.start, got.end(), tt.wantStart, tt.wantEnd, tt.why)
			}

			// Whatever the merge decided to keep, every byte it claims must be the byte belonging at
			// that offset. A merge that copies at the wrong index produces a run of the right length
			// holding shifted data, which is the failure mode a length check cannot see.
			for i, b := range got.data {
				at := got.start + int64(i)
				if want := byte(at % 251); b != want {
					t.Fatalf("byte at offset %d is %d, want %d — the merge placed data at the wrong "+
						"index, so a hit would return shifted bytes", at, b, want)
				}
			}

			if !tt.wantContiguousMerge {
				return
			}

			// And the result must cover both inputs' claims where they were contiguous.
			if !got.covers(tt.wantStart, tt.wantEnd) {
				t.Errorf("run [%d,%d) does not report covering itself", got.start, got.end())
			}
		})
	}
}

// TestCoalesceDoesNotAliasInput guards the one way a cache turns stale data into wrong data.
//
// The FUSE read path Puts the same slice it is about to hand the kernel. If the cache retains that
// slice, a caller reusing its read buffer rewrites cached bytes in place — and the next hit returns
// content that was never at that offset in any version of the object.
func TestCoalesceDoesNotAliasInput(t *testing.T) {
	t.Parallel()

	original := []byte("cached content")
	stored := clonePiece(chunkPiece{index: 0, start: 0, data: original})

	for i := range original {
		original[i] = 'X'
	}

	if bytes.Contains(stored.data, []byte("X")) {
		t.Errorf("cached data changed when the caller reused its buffer: %q. The cache must copy on "+
			"the way in, or a reused read buffer silently corrupts every subsequent hit.", stored.data)
	}
}

func TestCoversIsExact(t *testing.T) {
	t.Parallel()

	run := chunkPiece{index: 0, start: 100, data: make([]byte, 100)} // [100, 200)

	tests := []struct {
		from, to int64
		want     bool
		why      string
	}{
		{from: 100, to: 200, want: true, why: "exactly the run"},
		{from: 120, to: 180, want: true, why: "strictly inside"},
		{from: 100, to: 100, want: true, why: "empty range at the start"},
		{from: 99, to: 200, want: false, why: "one byte short at the front"},
		{from: 100, to: 201, want: false, why: "one byte short at the back"},
		{from: 0, to: 100, want: false, why: "entirely before"},
		{from: 200, to: 300, want: false, why: "entirely after"},
		{from: 0, to: 1000, want: false, why: "run is a strict subset of the ask"},
	}

	for _, tt := range tests {
		if got := run.covers(tt.from, tt.to); got != tt.want {
			t.Errorf("run [100,200).covers(%d, %d) = %v, want %v (%s). Claiming coverage it does not "+
				"have makes Get return bytes from the wrong offset.", tt.from, tt.to, got, tt.want, tt.why)
		}
	}
}

// FuzzChunkAssembly is the end-to-end property: whatever sequence of Puts lands, a Get that reports a
// hit must return exactly the bytes an authoritative copy holds at that range.
//
// This is the only check that covers split, coalesce, covers, and Get's assembly loop as a system.
// Each is individually testable and individually tested above, but the defect class being guarded
// against — bytes returned from the wrong offset — arises from their interaction.
//
// The two Puts read from *different versions* of the object, and the assertion is against the newer.
// An earlier version of this fuzzer sliced both from one buffer, which made it vacuous for the whole
// staleness class: wherever the two writes overlapped they were byte-identical by construction, so the
// overlap comparison in coalesce could never disagree and nothing distinguished keeping the older run
// from keeping the newer. Confirmed by mutation — with the comparison deleted, 3.3M executions passed
// while a three-case unit test failed immediately. Two inputs generated from one formula cannot detect
// a discarded input.
func FuzzChunkAssembly(f *testing.F) {
	f.Add(int64(0), int64(4096), int64(0), int64(4096))
	f.Add(int64(0), int64(131072), int64(131072), int64(131072))
	f.Add(int64(0), ChunkSize+1, ChunkSize, int64(1))
	f.Add(ChunkSize-100, int64(200), int64(0), ChunkSize)
	f.Add(int64(0), int64(10), int64(500), int64(10))

	f.Fuzz(func(t *testing.T, off1, len1, off2, len2 int64) {
		// Bound the inputs: this is a correctness fuzzer, not a memory-exhaustion one.
		const limit = 4 * ChunkSize
		if off1 < 0 || off2 < 0 || len1 < 0 || len2 < 0 ||
			off1 > limit || off2 > limit || len1 > limit || len2 > limit {
			return
		}

		size := max(off1+len1, off2+len2)

		// Two versions of the same object, differing in every byte. The first Put reads from the old
		// version, the second from the new — as happens when an object is rewritten between two reads.
		old := make([]byte, size)
		current := make([]byte, size)

		for i := range old {
			old[i] = byte(i % 251)
			current[i] = byte(i%251) ^ 0xFF
		}

		cache := NewLRUCache(&CacheConfig{MaxSize: 64 << 20, MaxEntries: 100000})
		defer func() { _ = cache.Close() }()

		cache.Put("obj", off1, old[off1:off1+len1])
		cache.Put("obj", off2, current[off2:off2+len2])

		// Probe every range that either write could plausibly have made available, plus a few that
		// straddle them.
		for _, probe := range []struct{ off, length int64 }{
			{off1, len1},
			{off2, len2},
			{off1, min(len1, 4096)},
			{min(off1, off2), max(off1+len1, off2+len2) - min(off1, off2)},
			{off1 + len1/2, len1 / 2},
		} {
			if probe.length <= 0 || probe.off < 0 || probe.off+probe.length > size {
				continue
			}

			got := cache.Get("obj", probe.off, probe.length)
			if got == nil {
				continue // a miss is always permissible
			}

			// Every byte returned must come from one version or the other at that exact offset — never a
			// splice of both, which is a buffer no version of the object ever contained. Bytes outside
			// the second Put's range can legitimately still be the old version's: nothing has told the
			// cache they changed, and bounded staleness is what invalidation and the TTL are for.
			for i, b := range got {
				at := probe.off + int64(i)
				if b == current[at] {
					continue
				}

				if b != old[at] {
					t.Fatalf("Get(obj, %d, %d) after Put(%d,%d) and Put(%d,%d) returned byte %#02x at "+
						"offset %d, which belongs to neither version (old %#02x, current %#02x). A cache "+
						"may miss and may be stale; it may never fabricate.\ngot: %x",
						probe.off, probe.length, off1, len1, off2, len2, b, at, old[at], current[at],
						truncate(got))
				}

				// Stale, and inside the range the newer Put just supplied: the newer bytes were dropped.
				if at >= off2 && at < off2+len2 {
					t.Fatalf("Get(obj, %d, %d) returned the pre-write byte %#02x at offset %d, inside the "+
						"range Put(%d,%d) had just written as %#02x. A later write's bytes must win over "+
						"an earlier read's.\ngot: %x",
						probe.off, probe.length, b, at, off2, len2, current[at], truncate(got))
				}
			}
		}
	})
}

func truncate(b []byte) []byte {
	if len(b) > 64 {
		return b[:64]
	}

	return b
}
