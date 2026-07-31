package difftest

import (
	"fmt"
	"strings"
)

// OpKind identifies a filesystem operation.
type OpKind uint8

const (
	// OpWrite writes Data at Offset. It is pwrite(2): it does not move a file position, because
	// FUSE supplies an explicit offset on every write and a position would be a second source of
	// truth for something the kernel already knows.
	OpWrite OpKind = iota

	// OpRead reads Length bytes at Offset and compares what both sides return.
	OpRead

	// OpTruncate resizes the file to Offset bytes, growing with zeros or discarding the tail.
	OpTruncate

	// OpFlush makes pending writes durable — fsync(2). Comparing after a flush is what catches a
	// write path that reports success without uploading, and comparing *before* one is what catches
	// a read path that consults storage instead of the pending writes.
	OpFlush

	// OpReopen closes the file and opens it again. Any state held only in memory is gone, so a
	// write that was accounted as flushed but never uploaded is exposed here and nowhere else.
	OpReopen

	// OpStat compares the size each side reports — fstat(2).
	//
	// This is an explicit operation rather than a check the oracle runs after every step, and that
	// distinction was learned the hard way. Comparing size automatically made every legacy test case
	// diverge at operation 0 on the same defect (stat ignores pending writes), so a case named for a
	// write-path bug passed without ever reaching the write it was about: six tests, one finding,
	// five false confirmations. A program now says which property it is interrogating, so a case
	// about content is not silently answered by a size bug.
	OpStat
)

// String implements fmt.Stringer.
func (k OpKind) String() string {
	switch k {
	case OpWrite:
		return "write"
	case OpRead:
		return "read"
	case OpTruncate:
		return "truncate"
	case OpFlush:
		return "flush"
	case OpReopen:
		return "reopen"
	case OpStat:
		return "stat"
	default:
		return fmt.Sprintf("OpKind(%d)", uint8(k))
	}
}

// Op is one operation in a [Program].
type Op struct {
	Kind OpKind

	// Offset is the byte offset for a write or read, and the new size for a truncate.
	Offset int64

	// Length is the byte count for a read. Unused otherwise.
	Length int

	// Data is the bytes for a write. Unused otherwise.
	Data []byte
}

// String renders the op as the Go that would construct it, so a failing program can be pasted into a
// test. Data is summarised rather than dumped: a 64 KiB literal is not something a human reads, and
// [Program.GoSource] emits a reproducible generator call for it instead.
func (o Op) String() string {
	switch o.Kind {
	case OpWrite:
		return fmt.Sprintf("write(%d, %s)", o.Offset, describeBytes(o.Data))
	case OpRead:
		return fmt.Sprintf("read(%d, %d)", o.Offset, o.Length)
	case OpTruncate:
		return fmt.Sprintf("truncate(%d)", o.Offset)
	case OpFlush:
		return "flush()"
	case OpReopen:
		return "reopen()"
	case OpStat:
		return "stat()"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(o.Kind))
	}
}

func describeBytes(b []byte) string {
	if len(b) == 0 {
		return `""`
	}

	// A short run of one repeated byte is the common shape in a shrunk counterexample and is worth
	// showing literally; anything else is summarised by length and first byte.
	same := true
	for _, c := range b {
		if c != b[0] {
			same = false
			break
		}
	}
	if same {
		return fmt.Sprintf("%d×%q", len(b), string(b[0]))
	}
	return fmt.Sprintf("%d bytes starting %#x", len(b), b[0])
}

// Program is a sequence of operations against a single file.
//
// One file, not a tree: every defect this package exists to catch is a defect in how one object's
// bytes are assembled. Directory semantics are a separate problem with separate tests, and mixing
// them in would make a shrunk counterexample harder to read for no gain.
type Program struct {
	Ops []Op
}

// Len returns the number of operations.
func (p Program) Len() int { return len(p.Ops) }

// String renders the program one operation per line.
func (p Program) String() string {
	var b strings.Builder
	for i, op := range p.Ops {
		fmt.Fprintf(&b, "%2d: %s\n", i, op)
	}
	return b.String()
}

// GoSource renders the program as a Go test body, so a counterexample can be committed as a
// regression test by pasting it. Write payloads become calls to [FillBytes], which is deterministic,
// rather than byte literals.
func (p Program) GoSource() string {
	var b strings.Builder
	b.WriteString("prog := difftest.Program{Ops: []difftest.Op{\n")
	for _, op := range p.Ops {
		switch op.Kind {
		case OpWrite:
			fmt.Fprintf(&b, "\t{Kind: difftest.OpWrite, Offset: %d, Data: difftest.FillBytes(%#x, %d)},\n",
				op.Offset, byteSeed(op.Data), len(op.Data))
		case OpRead:
			fmt.Fprintf(&b, "\t{Kind: difftest.OpRead, Offset: %d, Length: %d},\n", op.Offset, op.Length)
		case OpTruncate:
			fmt.Fprintf(&b, "\t{Kind: difftest.OpTruncate, Offset: %d},\n", op.Offset)
		case OpFlush:
			b.WriteString("\t{Kind: difftest.OpFlush},\n")
		case OpReopen:
			b.WriteString("\t{Kind: difftest.OpReopen},\n")
		case OpStat:
			b.WriteString("\t{Kind: difftest.OpStat},\n")
		}
	}
	b.WriteString("}}\n")
	return b.String()
}

// byteSeed recovers the seed [FillBytes] would have been called with to produce data, or 0 if data is
// empty. It is exact for anything FillBytes generated, which is everything the generator produces.
func byteSeed(data []byte) byte {
	if len(data) == 0 {
		return 0
	}
	return data[0]
}

// FillBytes returns n bytes derived from seed: a distinguishable, position-dependent pattern.
//
// Position-dependent matters. A payload of one repeated byte cannot distinguish "wrote the right
// bytes at the right offset" from "wrote the right bytes at the wrong offset," and offset handling is
// precisely what the write path got wrong. Each byte here is a function of both the seed and its
// index, so a misplaced or reordered run shows up as a content mismatch rather than passing.
func FillBytes(seed byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	out := make([]byte, n)
	out[0] = seed
	for i := 1; i < n; i++ {
		out[i] = seed ^ byte(i*31+i>>8)
	}
	return out
}
