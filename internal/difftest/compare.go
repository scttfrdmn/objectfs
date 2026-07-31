package difftest

import (
	"bytes"
	"context"
	"fmt"
	"strings"
)

// Divergence is a disagreement between the reference and the system under test.
type Divergence struct {
	// Index is the position in the program of the operation that diverged. -1 for a final-state
	// comparison, which belongs to no single operation.
	Index int

	// Op is the operation that diverged, zero-valued when Index is -1.
	Op Op

	// What names the property that disagreed: "read", "size", "durable", or "error".
	What string

	// Want and Got describe the reference's answer and the subject's, rendered for a human.
	Want string
	Got  string

	// Detail carries the byte-level specifics when the disagreement is about content.
	Detail string
}

// Error implements error, so a Divergence can be returned where an error is expected.
func (d *Divergence) Error() string {
	var b strings.Builder
	if d.Index >= 0 {
		fmt.Fprintf(&b, "op %d (%s): ", d.Index, d.Op)
	} else {
		b.WriteString("final state: ")
	}
	fmt.Fprintf(&b, "%s mismatch\n  reference: %s\n  objectfs:  %s", d.What, d.Want, d.Got)
	if d.Detail != "" {
		fmt.Fprintf(&b, "\n  %s", d.Detail)
	}
	return b.String()
}

// Compare runs prog against both implementations and returns the first divergence, or nil if they
// agreed throughout.
//
// Each operation compares only what that operation is about: a read compares the bytes and the count,
// a stat compares the size, and the durable contents are compared once at the end. The oracle
// deliberately does not check size after every operation, even though doing so would attribute a
// divergence to the operation that caused it. When it did, every legacy write-path test diverged at
// operation 0 on a single unrelated defect — stat ignoring pending writes — and none of them ever
// reached the write it was written to examine. Six passing tests, one real finding. An eager check
// that shadows the property under test buys precision in the failure it finds and loses every failure
// behind it.
//
// Reads are compared including the byte count: a read returning the right prefix and the wrong length
// is a short read to the kernel.
//
// Errors are compared only for their presence, never their identity. An implementation may legitimately
// refuse what the local filesystem allows, but the refusal has to be visible: an error on one side and
// success on the other is a divergence, because those are different outcomes for the caller. Matching
// errno values would be asserting that ObjectFS reproduces Linux's error taxonomy, which is neither
// true nor desirable.
func Compare(ctx context.Context, prog Program, ref, subject FS) *Divergence {
	for i, op := range prog.Ops {
		if d := applyBoth(ctx, i, op, ref, subject); d != nil {
			return d
		}
	}

	return compareDurable(ctx, ref, subject)
}

func applyBoth(ctx context.Context, i int, op Op, ref, subject FS) *Divergence {
	switch op.Kind {
	case OpWrite:
		refErr := ref.WriteAt(ctx, op.Offset, op.Data)
		subErr := subject.WriteAt(ctx, op.Offset, op.Data)
		return compareErr(i, op, refErr, subErr)

	case OpRead:
		refBuf := make([]byte, op.Length)
		subBuf := make([]byte, op.Length)

		refN, refErr := ref.ReadAt(ctx, refBuf, op.Offset)
		subN, subErr := subject.ReadAt(ctx, subBuf, op.Offset)

		if d := compareErr(i, op, refErr, subErr); d != nil {
			return d
		}
		if refN != subN {
			return &Divergence{
				Index: i, Op: op, What: "read",
				Want: fmt.Sprintf("%d bytes", refN),
				Got:  fmt.Sprintf("%d bytes", subN),
			}
		}
		if !bytes.Equal(refBuf[:refN], subBuf[:subN]) {
			return &Divergence{
				Index: i, Op: op, What: "read",
				Want:   fmt.Sprintf("%d bytes", refN),
				Got:    fmt.Sprintf("%d bytes, differing", subN),
				Detail: firstDiff(refBuf[:refN], subBuf[:subN], op.Offset),
			}
		}
		return nil

	case OpTruncate:
		refErr := ref.Truncate(ctx, op.Offset)
		subErr := subject.Truncate(ctx, op.Offset)
		return compareErr(i, op, refErr, subErr)

	case OpFlush:
		refErr := ref.Flush(ctx)
		subErr := subject.Flush(ctx)
		return compareErr(i, op, refErr, subErr)

	case OpReopen:
		refErr := ref.Reopen(ctx)
		subErr := subject.Reopen(ctx)
		return compareErr(i, op, refErr, subErr)

	case OpStat:
		return compareSize(ctx, i, op, ref, subject)

	default:
		return &Divergence{
			Index: i, Op: op, What: "error",
			Want: "a known operation",
			Got:  fmt.Sprintf("unknown kind %d", uint8(op.Kind)),
		}
	}
}

// compareErr reports a divergence when one side errored and the other did not.
//
// Two errors are treated as agreement even when they say different things, and that is deliberate:
// what matters is that both refused. Two successes are agreement on the outcome, and the content
// comparison that follows decides whether they agreed on the result.
func compareErr(i int, op Op, refErr, subErr error) *Divergence {
	switch {
	case refErr == nil && subErr == nil:
		return nil
	case refErr != nil && subErr != nil:
		return nil
	case refErr == nil:
		return &Divergence{
			Index: i, Op: op, What: "error",
			Want: "success",
			Got:  subErr.Error(),
		}
	default:
		// The subject accepted something the local filesystem refused. Usually the program generator
		// produced something invalid, so the message says so rather than implying a subject bug.
		return &Divergence{
			Index: i, Op: op, What: "error",
			Want: refErr.Error(),
			Got:  "success",
		}
	}
}

func compareSize(ctx context.Context, i int, op Op, ref, subject FS) *Divergence {
	refSize, refErr := ref.Size(ctx)
	subSize, subErr := subject.Size(ctx)

	if d := compareErr(i, op, refErr, subErr); d != nil {
		return d
	}
	if refErr != nil {
		return nil
	}
	if refSize != subSize {
		return &Divergence{
			Index: i, Op: op, What: "size",
			Want: fmt.Sprintf("%d", refSize),
			Got:  fmt.Sprintf("%d", subSize),
		}
	}
	return nil
}

// compareDurable flushes both sides and compares what is actually stored.
//
// The flush is issued here rather than left to the program because durability is the property the
// audit found most often misreported, and a program that happens not to end in a flush would let that
// go unchecked. A flush that fails is itself compared: if the reference can sync and the subject
// cannot, the subject is losing data and must say so.
func compareDurable(ctx context.Context, ref, subject FS) *Divergence {
	refErr := ref.Flush(ctx)
	subErr := subject.Flush(ctx)

	if d := compareErr(-1, Op{}, refErr, subErr); d != nil {
		d.What = "flush"
		return d
	}
	if refErr != nil {
		return nil
	}

	refData, err := ref.Durable(ctx)
	if err != nil {
		return &Divergence{Index: -1, What: "durable", Want: "readable reference", Got: err.Error()}
	}
	subData, err := subject.Durable(ctx)
	if err != nil {
		return &Divergence{
			Index: -1, What: "durable",
			Want: fmt.Sprintf("%d bytes", len(refData)),
			Got:  err.Error(),
		}
	}

	if len(refData) != len(subData) {
		return &Divergence{
			Index: -1, What: "durable",
			Want:   fmt.Sprintf("%d bytes", len(refData)),
			Got:    fmt.Sprintf("%d bytes", len(subData)),
			Detail: firstDiff(refData, subData, 0),
		}
	}
	if !bytes.Equal(refData, subData) {
		return &Divergence{
			Index: -1, What: "durable",
			Want:   fmt.Sprintf("%d bytes", len(refData)),
			Got:    fmt.Sprintf("%d bytes, differing", len(subData)),
			Detail: firstDiff(refData, subData, 0),
		}
	}
	return nil
}

// firstDiff describes where two buffers first differ, in absolute file offsets.
//
// Absolute, not relative to the comparison buffer, because the offset is the diagnosis: a difference
// beginning exactly at the offset of some earlier write is a dropped write, and one beginning at the
// reference's length is a truncation.
func firstDiff(want, got []byte, base int64) string {
	n := min(len(want), len(got))
	for i := range n {
		if want[i] != got[i] {
			return fmt.Sprintf("first difference at file offset %d: reference %#02x, objectfs %#02x%s",
				base+int64(i), want[i], got[i], lengthNote(want, got))
		}
	}
	if len(want) == len(got) {
		return ""
	}
	return fmt.Sprintf("identical for %d bytes, then lengths differ%s", n, lengthNote(want, got))
}

func lengthNote(want, got []byte) string {
	if len(want) == len(got) {
		return ""
	}
	return fmt.Sprintf(" (reference %d bytes, objectfs %d)", len(want), len(got))
}
