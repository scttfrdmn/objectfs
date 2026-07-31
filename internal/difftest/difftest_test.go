package difftest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/objectfs/objectfs/internal/difftest"
	"github.com/objectfs/objectfs/internal/testaws"
)

// localPair returns two independent local files. Comparing the reference against itself must never
// diverge — this is the harness's calibration, and every defect assertion below is worthless without
// it. A subtly wrong oracle that reports divergences on correct code is indistinguishable from one
// that reports them on incorrect code.
func localPair(t *testing.T) difftest.Factory {
	t.Helper()

	return func() (difftest.FS, difftest.FS, func(), error) {
		ref, err := difftest.NewLocal(t.TempDir())
		if err != nil {
			return nil, nil, func() {}, err
		}
		sub, err := difftest.NewLocal(t.TempDir())
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

func TestOracleAgreesWithItself(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		prog difftest.Program
	}{
		{"empty", difftest.Program{}},
		{"single write", difftest.Program{Ops: []difftest.Op{
			{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x11, 64)},
		}}},
		{"sequential writes", difftest.Program{Ops: []difftest.Op{
			{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x11, 4)},
			{Kind: difftest.OpWrite, Offset: 4, Data: difftest.FillBytes(0x22, 4)},
			{Kind: difftest.OpRead, Offset: 0, Length: 8},
			{Kind: difftest.OpStat},
		}}},
		{"sparse write leaves a hole", difftest.Program{Ops: []difftest.Op{
			{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x11, 16)},
			{Kind: difftest.OpWrite, Offset: 65536, Data: difftest.FillBytes(0x22, 16)},
			{Kind: difftest.OpRead, Offset: 16, Length: 32},
			{Kind: difftest.OpStat},
		}}},
		{"overwrite shorter over longer", difftest.Program{Ops: []difftest.Op{
			{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x11, 1024)},
			{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x22, 4)},
			{Kind: difftest.OpRead, Offset: 0, Length: 1024},
			{Kind: difftest.OpStat},
		}}},
		{"truncate down then grow", difftest.Program{Ops: []difftest.Op{
			{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x11, 256)},
			{Kind: difftest.OpTruncate, Offset: 4},
			{Kind: difftest.OpWrite, Offset: 83, Data: difftest.FillBytes(0x22, 8)},
			{Kind: difftest.OpRead, Offset: 0, Length: 256},
			{Kind: difftest.OpStat},
		}}},
		{"read past eof", difftest.Program{Ops: []difftest.Op{
			{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x11, 8)},
			{Kind: difftest.OpRead, Offset: 4096, Length: 128},
			{Kind: difftest.OpStat},
		}}},
		{"flush and reopen", difftest.Program{Ops: []difftest.Op{
			{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x11, 32)},
			{Kind: difftest.OpFlush},
			{Kind: difftest.OpReopen},
			{Kind: difftest.OpRead, Offset: 0, Length: 32},
			{Kind: difftest.OpStat},
		}}},
	}

	factory := localPair(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ref, sub, cleanup, err := factory()
			if err != nil {
				t.Fatalf("factory: %v", err)
			}
			defer cleanup()

			if d := difftest.Compare(context.Background(), tc.prog, ref, sub); d != nil {
				t.Fatalf("the reference disagreed with itself, so the oracle is broken:\n%v\nprogram:\n%s",
					d, tc.prog)
			}
		})
	}
}

// TestOracleDetectsAPlantedDefect checks the oracle in the other direction. An oracle that never
// reports a divergence passes every calibration test above and is useless; this one plants a known
// wrong answer and requires it to be found.
func TestOracleDetectsAPlantedDefect(t *testing.T) {
	t.Parallel()

	prog := difftest.Program{Ops: []difftest.Op{
		{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x11, 64)},
		{Kind: difftest.OpRead, Offset: 0, Length: 64},
		{Kind: difftest.OpStat},
	}}

	cases := []struct {
		name     string
		defect   func(difftest.FS) difftest.FS
		wantWhat string
	}{
		{"drops a write entirely", func(f difftest.FS) difftest.FS {
			return &brokenFS{FS: f, dropWrites: true}
		}, "read"},
		{"corrupts one byte", func(f difftest.FS) difftest.FS {
			return &brokenFS{FS: f, flipByte: true}
		}, "read"},
		{"reports the wrong size", func(f difftest.FS) difftest.FS {
			return &brokenFS{FS: f, sizeDelta: 1}
		}, "size"},
		{"flushes without persisting", func(f difftest.FS) difftest.FS {
			return &brokenFS{FS: f, emptyDurable: true}
		}, "durable"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ref, err := difftest.NewLocal(t.TempDir())
			if err != nil {
				t.Fatalf("NewLocal: %v", err)
			}
			defer func() { _ = ref.Close() }()

			inner, err := difftest.NewLocal(t.TempDir())
			if err != nil {
				t.Fatalf("NewLocal: %v", err)
			}
			defer func() { _ = inner.Close() }()

			d := difftest.Compare(context.Background(), prog, ref, tc.defect(inner))
			if d == nil {
				t.Fatalf("the oracle accepted a filesystem that %s; it cannot be trusted to catch a real one",
					tc.name)
			}
			if d.What != tc.wantWhat {
				t.Errorf("divergence reported as %q, want %q — the oracle found something, but not the "+
					"property that was broken:\n%v", d.What, tc.wantWhat, d)
			}
		})
	}
}

// brokenFS wraps an FS and introduces one specific wrong behavior, for testing the oracle itself.
type brokenFS struct {
	difftest.FS

	dropWrites   bool
	flipByte     bool
	sizeDelta    int64
	emptyDurable bool
}

func (b *brokenFS) WriteAt(ctx context.Context, offset int64, data []byte) error {
	if b.dropWrites {
		return nil
	}
	return b.FS.WriteAt(ctx, offset, data)
}

func (b *brokenFS) ReadAt(ctx context.Context, buf []byte, offset int64) (int, error) {
	n, err := b.FS.ReadAt(ctx, buf, offset)
	if b.flipByte && n > 0 {
		buf[n/2] ^= 0xff
	}
	return n, err
}

func (b *brokenFS) Size(ctx context.Context) (int64, error) {
	n, err := b.FS.Size(ctx)
	return n + b.sizeDelta, err
}

func (b *brokenFS) Durable(ctx context.Context) ([]byte, error) {
	if b.emptyDurable {
		return nil, nil
	}
	return b.FS.Durable(ctx)
}

// TestOracleCatchesLegacyDefects is the reason this package exists: the v0.10.0 write path, run
// against the operation sequences the audit proved lost data, must diverge from the local filesystem
// on every one of them.
//
// These are not hypothetical regressions. Each was verified by execution against real S3 during the
// audit, and each was invisible to the 32,680 lines of tests the release shipped with, because each
// lived in a seam between two layers that were tested against mocks of each other.
//
// This test guards the harness, not the write path. When the write path is rebuilt, [difftest.Legacy]
// and this test are deleted together — but until then, a change that stops the oracle from catching
// these fails here rather than quietly passing everything forever.
func TestOracleCatchesLegacyDefects(t *testing.T) {
	cases := []struct {
		name string
		prog difftest.Program
		// why names the audit finding, so a failure here is traceable to what it was meant to catch.
		why string
		// wantWhat is the property that must be the one to diverge. Asserting it is what stops a case
		// from passing on an unrelated defect that happens to fire first — which is exactly what
		// happened when this test was first written and the oracle checked size after every operation.
		wantWhat string
		// wantIndex is the operation the divergence must be attributed to, or -1 for the final durable
		// comparison. A case about the second write must not be satisfied by a divergence on the first.
		wantIndex int
	}{
		{
			name:      "append after write truncates the object",
			why:       "H7: the flush callback discards the offset and PutObject replaces the whole object",
			wantWhat:  "durable",
			wantIndex: -1,
			prog: difftest.Program{Ops: []difftest.Op{
				{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x41, 4)},
				{Kind: difftest.OpFlush},
				{Kind: difftest.OpWrite, Offset: 4, Data: difftest.FillBytes(0x42, 4)},
				{Kind: difftest.OpFlush},
			}},
		},
		{
			name:      "one byte at a high offset destroys the file",
			why:       "H7: appending one byte to a 1 MiB file leaves a 1-byte object",
			wantWhat:  "durable",
			wantIndex: -1,
			prog: difftest.Program{Ops: []difftest.Op{
				{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x41, 65536)},
				{Kind: difftest.OpFlush},
				{Kind: difftest.OpWrite, Offset: 65535, Data: []byte{0x58}},
				{Kind: difftest.OpFlush},
			}},
		},
		{
			name:      "non-contiguous write is refused",
			why:       "H8: canBufferWrite rejects any write that does not continue the buffer, so tar and SQLite get EIO",
			wantWhat:  "error",
			wantIndex: 1,
			prog: difftest.Program{Ops: []difftest.Op{
				{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x41, 512)},
				{Kind: difftest.OpWrite, Offset: 65536, Data: difftest.FillBytes(0x42, 512)},
			}},
		},
		{
			name:      "shorter content over longer keeps the old bytes",
			why:       "mergeWrites guards its overlay with newEnd > currentEnd, so `echo NEW > f` over OLD reads OLD",
			wantWhat:  "durable",
			wantIndex: -1,
			prog: difftest.Program{Ops: []difftest.Op{
				{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x41, 1024)},
				{Kind: difftest.OpFlush},
				{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x42, 4)},
				{Kind: difftest.OpFlush},
			}},
		},
		{
			name:      "read after write returns pre-write bytes",
			why:       "H5: the read path consults the cache and the backend but never the write buffer",
			wantWhat:  "read",
			wantIndex: 1,
			prog: difftest.Program{Ops: []difftest.Op{
				{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x41, 64)},
				{Kind: difftest.OpRead, Offset: 0, Length: 64},
			}},
		},
		{
			name:      "stat ignores pending writes",
			why:       "Getattr reads the object's metadata and never consults the write buffer",
			wantWhat:  "size",
			wantIndex: 1,
			prog: difftest.Program{Ops: []difftest.Op{
				{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x41, 256)},
				{Kind: difftest.OpStat},
			}},
		},
		{
			name:      "truncate is not implemented",
			why:       "v0.10.0 has no Setattr, Fsync, or Truncate, though the docs claim fsync durability",
			wantWhat:  "error",
			wantIndex: 1,
			prog: difftest.Program{Ops: []difftest.Op{
				{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x41, 256)},
				{Kind: difftest.OpTruncate, Offset: 4},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory := legacyPair(t)

			ref, sub, cleanup, err := factory()
			if err != nil {
				t.Fatalf("factory: %v", err)
			}
			defer cleanup()

			d := difftest.Compare(context.Background(), tc.prog, ref, sub)
			if d == nil {
				t.Fatalf("the oracle found no divergence, but this sequence is known to lose data.\n"+
					"%s\nprogram:\n%s", tc.why, tc.prog)
			}

			if d.What != tc.wantWhat || d.Index != tc.wantIndex {
				t.Fatalf("the oracle diverged on %q at op %d, but this case is about %q at op %d.\n"+
					"Passing on the wrong finding is how a suite of green tests confirms nothing.\n%s\n%v",
					d.What, d.Index, tc.wantWhat, tc.wantIndex, tc.why, d)
			}

			t.Logf("caught (%s)\n%v", tc.why, d)
		})
	}
}

// legacyPair returns a local reference against the v0.10.0 write path on a substrate-backed S3
// endpoint. Not parallel-safe with t.Parallel because each pair starts its own emulator.
func legacyPair(t *testing.T) difftest.Factory {
	t.Helper()

	return func() (difftest.FS, difftest.FS, func(), error) {
		ref, err := difftest.NewLocal(t.TempDir())
		if err != nil {
			return nil, nil, func() {}, err
		}

		ts := testaws.Start(t)

		sub, err := difftest.NewLegacy(ts.Backend(), "difftest/subject.bin")
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

// TestShrinkReducesToSomethingReadable checks that a long failing program collapses to a short one
// that still fails for the planted reason. An unshrinkable counterexample is one nobody acts on; a
// counterexample that shrank to a *different* failure is worse, because it is a bug report about
// something the reporter never observed.
//
// The second assertion is the one with teeth, and it was added after this test passed while
// reporting nonsense. It had reduced 44 operations to ten identical `read(0, 1)` calls diverging on
// an availability error — the S3 read path refusing every request after enough reads of a key that
// did not exist, which had nothing to do with the write defect planted here. Checking only that the
// program got smaller accepted that happily. It is the same vacuous-assertion mistake as the one
// [difftest.OpStat] exists to prevent, made a second time in a different place.
//
// (That nonsense reduction was not a harness artifact: it was two real defects — a missing object
// counted as a health failure, and StateUnavailable having no recovery path — now fixed and pinned
// by pkg/health's admission tests and internal/storage/s3's health-gate tests. Shrinking earned its
// keep before it ever shrank a real bug report.)
func TestShrinkReducesToSomethingReadable(t *testing.T) {
	t.Parallel()

	// Bury the two-op defect — a write, then a write at a non-zero offset, which H7 turns into a
	// whole-object replace — in reads that diverge on nothing.
	ops := []difftest.Op{}
	for i := range 20 {
		ops = append(ops, difftest.Op{
			Kind:   difftest.OpRead,
			Offset: int64(i) * 128,
			Length: 64,
		})
	}
	ops = append(ops,
		difftest.Op{Kind: difftest.OpWrite, Offset: 0, Data: difftest.FillBytes(0x41, 8192)},
		difftest.Op{Kind: difftest.OpFlush},
		difftest.Op{Kind: difftest.OpWrite, Offset: 8192, Data: difftest.FillBytes(0x42, 4096)},
		difftest.Op{Kind: difftest.OpFlush},
	)
	for i := range 20 {
		ops = append(ops, difftest.Op{
			Kind:   difftest.OpRead,
			Offset: int64(i) * 64,
			Length: 32,
		})
	}

	prog := difftest.Program{Ops: ops}

	shrunk, d := difftest.Shrink(context.Background(), prog, legacyPair(t))
	if d == nil {
		t.Fatal("the planted program did not diverge, so there was nothing to shrink")
	}

	if d.What != "durable" {
		t.Fatalf("shrinking arrived at a %q divergence, but this program plants a durability defect "+
			"(H7: the flush callback drops the offset, so the second write replaces the whole object).\n"+
			"A counterexample that reduced to a different failure describes a bug nobody saw.\n%v\n%s",
			d.What, d, shrunk)
	}

	if shrunk.Len() >= prog.Len() {
		t.Errorf("shrinking %d operations produced %d; a counterexample this size is not actionable:\n%s",
			prog.Len(), shrunk.Len(), shrunk)
	}

	// H7's mechanism is a write whose offset is discarded, so the surviving program must still
	// contain a write at a non-zero offset. This asserts the mechanism rather than the shape: the
	// first version of this check demanded the two writes the program was built from, and failed
	// because the shrinker had found something better — a single one-byte write at offset 1, where
	// the reference is two bytes and the object is one. That is H7 with nothing else in the frame,
	// and rejecting it would have been the test insisting on the author's guess at the minimal
	// counterexample over the shrinker's.
	offsetWrites := 0
	for _, op := range shrunk.Ops {
		if op.Kind == difftest.OpWrite && op.Offset > 0 {
			offsetWrites++
		}
	}
	if offsetWrites == 0 {
		t.Errorf("the shrunk program contains no write at a non-zero offset, so the surviving "+
			"divergence cannot be the dropped-offset defect this program plants:\n%s\n%v", shrunk, d)
	}

	t.Logf("reduced %d operations to %d:\n%s\n%v\n\n%s",
		prog.Len(), shrunk.Len(), shrunk, d, shrunk.GoSource())
}

func TestGoSourceIsPasteable(t *testing.T) {
	t.Parallel()

	prog := difftest.Program{Ops: []difftest.Op{
		{Kind: difftest.OpWrite, Offset: 4096, Data: difftest.FillBytes(0x41, 16)},
		{Kind: difftest.OpRead, Offset: 0, Length: 32},
		{Kind: difftest.OpTruncate, Offset: 8},
		{Kind: difftest.OpFlush},
		{Kind: difftest.OpReopen},
	}}

	src := prog.GoSource()

	for _, want := range []string{
		"difftest.Program{Ops: []difftest.Op{",
		"difftest.OpWrite, Offset: 4096, Data: difftest.FillBytes(0x41, 16)",
		"difftest.OpRead, Offset: 0, Length: 32",
		"difftest.OpTruncate, Offset: 8",
		"difftest.OpFlush",
		"difftest.OpReopen",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("GoSource is missing %q, so a counterexample cannot be pasted into a test:\n%s", want, src)
		}
	}
}

// TestFillBytesIsPositionDependent pins the property the oracle's diagnosis depends on. A payload of
// one repeated byte cannot tell "wrote the right bytes at the right offset" from "wrote them at the
// wrong offset" — and offset handling is exactly what v0.10.0 got wrong.
func TestFillBytesIsPositionDependent(t *testing.T) {
	t.Parallel()

	data := difftest.FillBytes(0x41, 256)

	distinct := map[byte]bool{}
	for _, b := range data {
		distinct[b] = true
	}
	if len(distinct) < 64 {
		t.Errorf("FillBytes produced only %d distinct values across 256 bytes; a misplaced run would "+
			"not be detectable as a content mismatch", len(distinct))
	}

	// A shifted window must not match the unshifted one, or a write landing at the wrong offset reads
	// as correct.
	for _, shift := range []int{1, 2, 4, 8, 64} {
		if string(data[:len(data)-shift]) == string(data[shift:]) {
			t.Errorf("FillBytes output is invariant under a shift of %d, so a write at the wrong offset "+
				"would compare equal", shift)
		}
	}

	// Different seeds must differ, or an overwrite is undetectable.
	other := difftest.FillBytes(0x42, 256)
	if string(data) == string(other) {
		t.Error("FillBytes ignores its seed, so an overwrite with different content would compare equal")
	}
}
