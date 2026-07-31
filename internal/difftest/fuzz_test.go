package difftest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/objectfs/objectfs/internal/difftest"
	"github.com/objectfs/objectfs/internal/testaws"
)

// The wire format decoded by [decodeProgram]: one operation per fixed-width record.
//
// Fixed width, not variable, and that choice is load-bearing. A format where a write's payload length
// determines how many bytes the next record starts at means every mutation the fuzzer makes to a length
// byte reinterprets the entire remainder of the input as different operations. Coverage-guided fuzzing
// depends on small input changes making small behaviour changes; a self-desynchronising format destroys
// that, and it also makes every hand-written seed unreadable and unmaintainable.
//
// Payload bytes are therefore not taken from the input at all. Only a seed is, and [difftest.FillBytes]
// expands it to a position-dependent pattern — which is what the oracle wants anyway, since a payload of
// one repeated byte cannot distinguish "right bytes, right place" from "right bytes, wrong place".
const (
	// opStride is the record width: kind, three offset bytes, an argument, and a payload seed.
	opStride = 6

	// maxOffset bounds where an operation may land. 1 MiB, and a power of two so the reduction is a
	// mask with no modulo bias — a biased offset distribution would cluster the fuzzer's writes and
	// leave whole regions unexercised.
	//
	// It is enough to reach every boundary that matters: the 4 KiB page, 64 KiB, the 128 KiB FUSE
	// MaxRead, and 1 MiB. Beyond that a larger offset tests the emulator's allocator, not ObjectFS.
	maxOffset = 1 << 20

	// maxOps bounds program length so one iteration stays affordable. Each flush is a real
	// read-modify-write over a real HTTP endpoint, and a fuzzer that spends a minute per input
	// explores nothing.
	maxOps = 64
)

// decodeProgram builds a [difftest.Program] from fuzzer bytes.
//
// It is total: every byte string decodes to some program, and a short or malformed tail simply ends the
// sequence. A decoder that rejects inputs spends the fuzzer's budget on rejection, and one that panics
// on a short tail reports a bug in itself while burying the one in the subject.
func decodeProgram(in []byte) difftest.Program {
	var ops []difftest.Op

	for len(in) >= opStride && len(ops) < maxOps {
		var (
			kind   = in[0]
			offset = (int64(in[1])<<16 | int64(in[2])<<8 | int64(in[3])) % maxOffset
			arg    = int(in[4])
			seed   = in[5]
		)
		in = in[opStride:]

		switch kind % 8 {
		case 0, 1:
			// Small writes, 1..256 bytes. Writes get four of the eight arms because every proven
			// data-loss defect lived in one, and a program with no writes cannot diverge on durability
			// at all. Two sizes rather than one: the 1-byte write at a high offset is H7's exact shape,
			// and a 16-byte granularity would make it unreachable.
			ops = append(ops, difftest.Op{
				Kind:   difftest.OpWrite,
				Offset: offset,
				Data:   difftest.FillBytes(seed, arg+1),
			})

		case 2:
			// Large writes, 16..4096 bytes, to reach across chunk boundaries in one operation.
			ops = append(ops, difftest.Op{
				Kind:   difftest.OpWrite,
				Offset: offset,
				Data:   difftest.FillBytes(seed, (arg+1)*16),
			})

		case 3, 4:
			// Reads of 64..16384 bytes. Scaled up from the argument byte because the interesting read
			// lengths are the kernel's — 4096, 8192, 131072 — and a 1..256 range never reaches them.
			ops = append(ops, difftest.Op{
				Kind:   difftest.OpRead,
				Offset: offset,
				Length: (arg + 1) * 64,
			})

		case 5:
			ops = append(ops, difftest.Op{Kind: difftest.OpTruncate, Offset: offset})

		case 6:
			ops = append(ops, difftest.Op{Kind: difftest.OpFlush})

		case 7:
			if arg%2 != 0 {
				ops = append(ops, difftest.Op{Kind: difftest.OpStat})
				continue
			}

			// A reopen is always preceded by a flush, and that is not a stylistic choice — it is what
			// keeps this fuzzer from reporting a defect that does not exist.
			//
			// The two sides do not mean the same thing by "discard state held only in memory". For the
			// local reference nothing is: write(2) lands in the page cache, where a second process
			// already sees it, so a reopen loses nothing. For ObjectFS an unflushed write is in the
			// node's extent list and a reopen drops it. A bare write-then-reopen would therefore
			// diverge on every input, reported as data loss, when the only disagreement is about what
			// the harness means.
			//
			// Pairing them keeps the detection power that made Reopen worth having. A flush that
			// reports success without uploading is still caught, because the flush happens first and
			// the reopen then reads back what the object store actually holds — which is the one thing
			// no amount of in-memory state can fake.
			if len(ops)+2 > maxOps {
				return difftest.Program{Ops: ops}
			}
			ops = append(ops,
				difftest.Op{Kind: difftest.OpFlush},
				difftest.Op{Kind: difftest.OpReopen},
			)
		}
	}

	return difftest.Program{Ops: ops}
}

// seedCorpus is the set of operation sequences worth starting from: every defect the v0.10.0 audit
// proved by execution, plus the boundary shapes a fuzzer takes a long time to reach on its own.
//
// These are seeds, not assertions. Each is a shape the fuzzer mutates outward from, and the reason to
// commit them is that coverage-guided fuzzing finds "write, flush, write at the end, flush" eventually
// and finds it in the first iteration when handed it. The named regressions are also pinned as ordinary
// table tests in [TestVFSPassesTheLegacyDefectSuite]; a corpus entry is not a substitute for one,
// because a corpus can be silently emptied without anything failing.
//
// The `want` signature on each entry is what [TestSeedCorpusDecodesToTheShapesItClaims] checks. Without
// it these are twelve unreadable byte strings whose comments are unverifiable prose: change the encoding
// and every one still decodes to *something*, just not to the sequence it claims, and nothing fails.
var seedCorpus = []struct {
	name string
	why  string
	in   []byte
	want string
}{
	{
		name: "append after flush",
		why:  "H7, the dropped offset: appending to a flushed file replaced the whole object",
		in: []byte{
			0, 0, 0, 0, 3, 'A', // write(0, 4)
			6, 0, 0, 0, 0, 0, // flush
			0, 0, 0, 4, 3, 'B', // write(4, 4)
			6, 0, 0, 0, 0, 0, // flush
		},
		want: "w0+4 F w4+4 F",
	},
	{
		name: "non-contiguous writes",
		why:  "H8: canBufferWrite refused any write that did not continue the buffer, so tar and SQLite got EIO",
		in: []byte{
			0, 0, 0, 0, 255, 'A', // write(0, 256)
			0, 1, 0, 0, 255, 'B', // write(65536, 256)
		},
		want: "w0+256 w65536+256",
	},
	{
		name: "shorter content over longer",
		why:  "mergeWrites guarded its overlay with newEnd > currentEnd, so `echo NEW > f` over OLD read OLD",
		in: []byte{
			2, 0, 0, 0, 255, 'A', // write(0, 4096)
			6, 0, 0, 0, 0, 0, // flush
			5, 0, 0, 0, 0, 0, // truncate(0)
			0, 0, 0, 0, 3, 'B', // write(0, 4)
			6, 0, 0, 0, 0, 0, // flush
		},
		want: "w0+4096 F t0 w0+4 F",
	},
	{
		name: "read after write",
		why:  "H5: the read path consulted the cache and the backend but never the write buffer",
		in: []byte{
			0, 0, 0, 0, 63, 'A', // write(0, 64)
			3, 0, 0, 0, 0, 0, // read(0, 64)
		},
		want: "w0+64 r0+64",
	},
	{
		name: "stat with writes pending",
		why:  "Getattr read the object's metadata and never consulted the write buffer",
		in: []byte{
			0, 0, 0, 0, 63, 'A', // write(0, 64)
			7, 0, 0, 0, 1, 0, // stat
		},
		want: "w0+64 S",
	},
	{
		name: "write, flush, reopen, read",
		why:  "the only shape that distinguishes a flush that uploaded from one that reported success",
		in: []byte{
			0, 0, 0, 0, 31, 'A', // write(0, 32)
			7, 0, 0, 0, 0, 0, // flush + reopen
			3, 0, 0, 0, 0, 0, // read(0, 64)
		},
		want: "w0+32 F O r0+64",
	},
	{
		name: "truncate down, then write past the old end",
		why:  "the truncated bytes must not be resurrected by the read-modify-write fetch",
		in: []byte{
			0, 0, 0, 0, 255, 'A', // write(0, 256)
			6, 0, 0, 0, 0, 0, // flush
			5, 0, 0, 4, 0, 0, // truncate(4)
			0, 0, 0, 83, 7, 'B', // write(83, 8)
			3, 0, 0, 0, 3, 0, // read(0, 256)
			7, 0, 0, 0, 0, 0, // flush + reopen
			3, 0, 0, 0, 3, 0, // read(0, 256)
		},
		want: "w0+256 F t4 w83+8 r0+256 F O r0+256",
	},
	{
		name: "truncate to zero, then write",
		why:  "`> file` followed by content — the shape every shell redirect produces",
		in: []byte{
			5, 0, 0, 0, 0, 0, // truncate(0)
			0, 0, 0, 0, 15, 'C', // write(0, 16)
			6, 0, 0, 0, 0, 0, // flush
		},
		want: "t0 w0+16 F",
	},
	{
		name: "truncate up, then read the hole",
		why:  "a grown file's new bytes read as zeros, and only the logical size says how many",
		in: []byte{
			5, 0, 3, 232, 0, 0, // truncate(1000)
			3, 0, 0, 0, 3, 0, // read(0, 256)
			7, 0, 0, 0, 1, 0, // stat
		},
		want: "t1000 r0+256 S",
	},
	{
		name: "read past EOF",
		why:  "must be a short read of zero bytes with no error, as read(2) is",
		in: []byte{
			0, 0, 0, 0, 7, 'A', // write(0, 8)
			3, 1, 0, 0, 0, 0, // read(65536, 64)
		},
		want: "w0+8 r65536+64",
	},
	{
		name: "overlapping writes in descending order",
		why:  "extent coalescing: last-writer-wins must hold regardless of arrival order",
		in: []byte{
			0, 0, 0, 200, 63, 'A', // write(200, 64)
			0, 0, 0, 100, 63, 'B', // write(100, 64)
			0, 0, 0, 0, 63, 'C', // write(0, 64)
			3, 0, 0, 0, 5, 0, // read(0, 384)
		},
		want: "w200+64 w100+64 w0+64 r0+384",
	},
	{
		name: "flush with nothing pending",
		why:  "a no-op plan treated as a whole-object write would PUT zero bytes over an intact object",
		in: []byte{
			6, 0, 0, 0, 0, 0, // flush
			6, 0, 0, 0, 0, 0, // flush
			7, 0, 0, 0, 1, 0, // stat
		},
		want: "F F S",
	},
}

// FuzzOperationSequence drives the differential oracle from fuzzer bytes: arbitrary sequences of writes,
// reads, truncations, flushes and reopens run against both the local filesystem and ObjectFS, asserting
// they agree on content, on size, and on what ends up durable.
//
// This is the operation-sequence fuzzer the audit called for, and it is the only target whose subject is
// the whole composed stack — [difftest.VFS] over the real S3 backend over a real HTTP endpoint. Every
// proven v0.10.0 data-loss defect is reachable from here, and none of them was reachable from a unit
// test, because each lived in a seam between two layers that were tested against mocks of each other.
//
// A failure prints the shrunk counterexample as pasteable Go, so the first thing to do with one is put it
// in [TestVFSPassesTheLegacyDefectSuite]'s table and watch it fail deterministically.
func FuzzOperationSequence(f *testing.F) {
	for _, seed := range seedCorpus {
		f.Add(seed.in)
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		prog := decodeProgram(in)
		if prog.Len() == 0 {
			return
		}

		// Shrink runs the program first and returns a nil divergence if it passes, so this is both the
		// check and the reduction. Running Compare separately beforehand would double the cost of the
		// overwhelmingly common case, which is that the program passes.
		shrunk, d := difftest.Shrink(context.Background(), prog, vfsPair(t))
		if d == nil {
			return
		}

		t.Fatalf("the oracle found a divergence:\n%v\n\nreduced from %d operations to %d:\n%s\n%s",
			d, prog.Len(), shrunk.Len(), shrunk, shrunk.GoSource())
	})
}

// vfsPair returns a [difftest.Factory] pairing a local reference against the vfs write path on a
// substrate-backed S3 endpoint.
//
// One backend serves every call from this pair; freshness comes from a distinct object key each time.
// [difftest.Shrink] calls the factory once per candidate program — hundreds of times for a large
// counterexample — and a fresh backend per call would spend the reduction on TLS-less HTTP client setup
// and connection pools. A new key is equally fresh, because the subject's entire state is one object plus
// one handle table over it.
//
// The endpoint is [testaws.Shared] rather than [testaws.Start], which matters only inside f.Fuzz: see
// Shared's own documentation for the port exhaustion that a per-iteration server produced.
func vfsPair(t *testing.T) difftest.Factory {
	t.Helper()

	backend, err := testaws.Shared(t).Backend(context.Background())
	if err != nil {
		t.Fatalf("testaws: backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	dir := t.TempDir()

	var n int

	return func() (difftest.FS, difftest.FS, func(), error) {
		n++

		// A distinct reference directory as well as a distinct key. NewLocal truncates the file it
		// opens, so reuse would in fact be safe — but relying on that couples this factory to an
		// implementation detail of the reference, and a shrinker whose runs are not independent reports
		// counterexamples that do not reproduce.
		runDir := filepath.Join(dir, fmt.Sprintf("run-%d", n))
		if err := os.Mkdir(runDir, 0o700); err != nil {
			return nil, nil, func() {}, err
		}

		ref, err := difftest.NewLocal(runDir)
		if err != nil {
			return nil, nil, func() {}, err
		}

		sub, err := difftest.NewVFS(context.Background(), backend, fmt.Sprintf("fuzz/subject-%d.bin", n))
		if err != nil {
			_ = ref.Close()
			return nil, nil, func() {}, err
		}

		return ref, sub, func() {
			_ = ref.Close()
			_ = sub.Close()
		}, nil
	}
}

// TestVFSPassesTheLegacyDefectSuite is the other half of TestOracleCatchesLegacyDefects. That test
// asserts the oracle *finds* the v0.10.0 defects; this one asserts the replacement write path does not
// have them.
//
// Both are needed and neither alone says anything. An oracle that cried wolf on correct code would pass
// the first and fail this one; a write path still carrying the defects would pass this one only if the
// oracle had been blinded.
//
// It runs the same programs as [seedCorpus] through the same decoder, rather than restating them as Op
// literals. Restating them would let the corpus and the suite drift apart, and the corpus is the half
// nobody reads.
func TestVFSPassesTheLegacyDefectSuite(t *testing.T) {
	for _, tc := range seedCorpus {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prog := decodeProgram(tc.in)

			ref, sub, cleanup, err := vfsPair(t)()
			if err != nil {
				t.Fatalf("factory: %v", err)
			}
			defer cleanup()

			if d := difftest.Compare(context.Background(), prog, ref, sub); d != nil {
				t.Fatalf("the vfs write path diverged from the local filesystem:\n%v\n\n%s\nprogram:\n%s",
					d, tc.why, prog)
			}
		})
	}
}

// TestSeedCorpusDecodesToTheShapesItClaims guards the corpus against silent rot.
//
// This has already earned its place: it caught the first draft of every seed in the corpus being one byte
// off, because the length argument is a count minus one and the seeds were written as though it were the
// count. Twelve entries that all decoded to plausible programs, none of them the program named in the
// comment above it, and no other test in the repo could have noticed.
func TestSeedCorpusDecodesToTheShapesItClaims(t *testing.T) {
	t.Parallel()

	if len(seedCorpus) == 0 {
		t.Fatal("the seed corpus is empty, so the fuzzer starts from nothing")
	}

	for _, tc := range seedCorpus {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prog := decodeProgram(tc.in)

			if got := signature(prog); got != tc.want {
				t.Errorf("decodes to a different sequence than it claims\n  want: %s\n  got:  %s\n%s",
					tc.want, got, prog)
			}

			// A record that the decoder did not consume is a seed with a typo in its length: the tail
			// silently vanishes and the entry exercises less than it says.
			if rem := len(tc.in) % opStride; rem != 0 {
				t.Errorf("is %d bytes, not a multiple of the %d-byte record width — %d trailing bytes "+
					"are ignored", len(tc.in), opStride, rem)
			}
		})
	}
}

// signature renders a program as a compact one-line shape, so a corpus entry can state what it decodes
// to in a form a human can check against the byte string beside it.
func signature(p difftest.Program) string {
	parts := make([]string, 0, p.Len())

	for _, op := range p.Ops {
		switch op.Kind {
		case difftest.OpWrite:
			parts = append(parts, fmt.Sprintf("w%d+%d", op.Offset, len(op.Data)))
		case difftest.OpRead:
			parts = append(parts, fmt.Sprintf("r%d+%d", op.Offset, op.Length))
		case difftest.OpTruncate:
			parts = append(parts, fmt.Sprintf("t%d", op.Offset))
		case difftest.OpFlush:
			parts = append(parts, "F")
		case difftest.OpReopen:
			parts = append(parts, "O")
		case difftest.OpStat:
			parts = append(parts, "S")
		default:
			parts = append(parts, fmt.Sprintf("?%d", uint8(op.Kind)))
		}
	}

	return strings.Join(parts, " ")
}

// TestDecodeProgramIsTotal pins the properties the fuzzer depends on: any byte string decodes without
// panicking, and every operation it produces is well formed and bounded.
//
// The well-formedness matters as much as the absence of a panic. A decoder that emitted a negative
// offset or a zero-length read would make the resulting divergence a bug in this file, and the reader of
// the failure would spend the afternoon in the write path.
func TestDecodeProgramIsTotal(t *testing.T) {
	t.Parallel()

	inputs := [][]byte{
		nil,
		{},
		{0},
		{0, 0, 0, 0, 0},                // one byte short of a record
		{0, 0, 0, 0, 0, 0},             // exactly one record
		{255, 255, 255, 255, 255, 255}, // every field at maximum
		{0, 255, 255, 255, 255, 'A'},   // the highest reachable write offset
		{7, 0, 0, 0, 0, 0},             // the flush+reopen pair, alone
		// Every kind arm, back to back.
		{
			0, 1, 2, 3, 4, 5,
			1, 6, 7, 8, 9, 10,
			2, 11, 12, 13, 14, 15,
			3, 16, 17, 18, 19, 20,
			4, 21, 22, 23, 24, 25,
			5, 26, 27, 28, 29, 30,
			6, 31, 32, 33, 34, 35,
			7, 36, 37, 38, 39, 40,
		},
	}

	// An input long enough to hit the operation cap, to prove the cap holds and that the flush+reopen
	// pair cannot step over it.
	var long []byte
	for range maxOps * 2 {
		long = append(long, 7, 0, 0, 0, 0, 0)
	}
	inputs = append(inputs, long)

	for _, in := range inputs {
		prog := decodeProgram(in)

		if prog.Len() > maxOps {
			t.Errorf("decode(%d bytes) produced %d operations, over the %d cap", len(in), prog.Len(), maxOps)
		}

		for i, op := range prog.Ops {
			if op.Offset < 0 || op.Offset >= maxOffset {
				t.Errorf("decode(%d bytes) op %d has offset %d, outside [0,%d)",
					len(in), i, op.Offset, maxOffset)
			}
			switch op.Kind {
			case difftest.OpRead:
				if op.Length <= 0 {
					t.Errorf("decode(%d bytes) op %d is a read of %d bytes", len(in), i, op.Length)
				}
			case difftest.OpWrite:
				if len(op.Data) == 0 {
					t.Errorf("decode(%d bytes) op %d is an empty write, a no-op that wastes an operation",
						len(in), i)
				}
			}
		}
	}
}
