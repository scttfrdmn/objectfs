package difftest

import "context"

// Factory builds a fresh pair of implementations for one run.
//
// Fresh, because shrinking replays candidate programs dozens of times and any state surviving between
// runs makes the result depend on run order. A shrinker that reports a two-operation counterexample
// which does not reproduce on its own is worse than no shrinker.
type Factory func() (ref, subject FS, cleanup func(), err error)

// Shrink reduces a failing program to a smaller one that still fails, and returns it with the
// divergence it produces. If prog does not fail, it is returned unchanged with a nil divergence.
//
// This is what makes a fuzzer's output usable. A 200-operation counterexample is a wall of noise; the
// three operations inside it that actually matter are a bug report. Shrinking is also a check on the
// oracle itself: if a failure will not reduce, the divergence often depends on accumulated state
// rather than on the operations, which usually means the harness is at fault rather than the subject.
//
// Two reductions are applied to a fixpoint, cheapest first:
//
//  1. Delete a contiguous run of operations, halving the run length on each pass. Removing whole
//     spans first gets a 200-op program down to a handful in a few passes, where one-at-a-time
//     deletion would take hundreds.
//  2. Shrink one operation's magnitude — a write's payload, a read's length, an offset — toward zero.
//     A failure at offset 1048575 usually also happens at offset 4097, and the smaller number is the
//     one a human can reason about.
//
// Shrink never widens the program, so it terminates: every accepted candidate is strictly smaller by
// operation count or by total magnitude.
func Shrink(ctx context.Context, prog Program, factory Factory) (Program, *Divergence) {
	base := run(ctx, prog, factory)
	if base == nil {
		return prog, nil
	}

	best, bestDiv := prog, base

	for improved := true; improved; {
		improved = false

		// Pass 1: delete runs, largest first.
		for span := len(best.Ops) / 2; span >= 1; span /= 2 {
			for start := 0; start+span <= len(best.Ops); start++ {
				candidate := deleteRun(best, start, span)
				if candidate.Len() == 0 {
					continue
				}
				if d := run(ctx, candidate, factory); d != nil {
					best, bestDiv, improved = candidate, d, true
					// Restart the span loop: indices have moved and the program is smaller.
					start = -1
					if span > len(best.Ops) {
						break
					}
				}
			}
		}

		// Pass 2: shrink magnitudes.
		for i := range best.Ops {
			for _, candidate := range smallerOps(best, i) {
				if d := run(ctx, candidate, factory); d != nil {
					best, bestDiv, improved = candidate, d, true
					break
				}
			}
		}
	}

	return best, bestDiv
}

func run(ctx context.Context, prog Program, factory Factory) *Divergence {
	ref, subject, cleanup, err := factory()
	if err != nil {
		return &Divergence{Index: -1, What: "error", Want: "a usable pair", Got: err.Error()}
	}
	defer cleanup()

	return Compare(ctx, prog, ref, subject)
}

func deleteRun(p Program, start, span int) Program {
	ops := make([]Op, 0, len(p.Ops)-span)
	ops = append(ops, p.Ops[:start]...)
	ops = append(ops, p.Ops[start+span:]...)
	return Program{Ops: ops}
}

// smallerOps returns candidate programs with operation i reduced, most aggressive first, so the
// biggest win is tried before the incremental ones.
func smallerOps(p Program, i int) []Program {
	op := p.Ops[i]

	var variants []Op
	switch op.Kind {
	case OpWrite:
		for _, off := range smallerInts(op.Offset) {
			variants = append(variants, Op{Kind: OpWrite, Offset: off, Data: op.Data})
		}
		for _, n := range smallerInts(int64(len(op.Data))) {
			if n > 0 {
				variants = append(variants, Op{Kind: OpWrite, Offset: op.Offset, Data: op.Data[:n]})
			}
		}
	case OpRead:
		for _, off := range smallerInts(op.Offset) {
			variants = append(variants, Op{Kind: OpRead, Offset: off, Length: op.Length})
		}
		for _, n := range smallerInts(int64(op.Length)) {
			if n > 0 {
				variants = append(variants, Op{Kind: OpRead, Offset: op.Offset, Length: int(n)})
			}
		}
	case OpTruncate:
		for _, off := range smallerInts(op.Offset) {
			variants = append(variants, Op{Kind: OpTruncate, Offset: off})
		}
	case OpFlush, OpReopen:
		// No magnitude to shrink; deletion is the only reduction, and pass 1 tries it.
		return nil
	}

	out := make([]Program, 0, len(variants))
	for _, v := range variants {
		ops := append([]Op(nil), p.Ops...)
		ops[i] = v
		out = append(out, Program{Ops: ops})
	}
	return out
}

// smallerInts returns candidate reductions of n toward zero, most aggressive first.
//
// Zero, then a small round number, then halves. The round number matters more than it looks: a
// failure that survives at offset 4096 but not at 0 is a page-boundary bug, and naming the boundary
// in the counterexample is most of the diagnosis.
func smallerInts(n int64) []int64 {
	if n <= 0 {
		return nil
	}

	seen := map[int64]bool{n: true}
	var out []int64
	add := func(v int64) {
		if v >= 0 && v < n && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}

	add(0)
	add(1)
	for _, round := range []int64{4096, 1024, 512, 8, 4} {
		add(round)
	}
	for v := n / 2; v > 0; v /= 2 {
		add(v)
	}
	add(n - 1)

	return out
}
