package s3

import (
	"fmt"
	"testing"
)

// FuzzSliceRange drives [sliceRange] over the whole offset/size domain, including the negatives that
// made the inline version of this arithmetic audit finding C3.
//
// C3 was a panic, not a wrong answer: with size < 0 the old expression computed end < offset, neither
// clamp arm fired, and `data[offset:end]` aborted the process — "slice bounds out of range [100:99]" —
// which for a FUSE daemon means the mount disappears and every open file descriptor on it breaks. The
// documented call form at doc.go:165 reaches it, as do three cache and coordinator call sites. A fuzzer
// is the right shape of test for it because the defect is in the *arithmetic*, and arithmetic bugs live
// at boundaries a table test only visits if its author already suspected them.
//
// The assertions go beyond "did not panic". A function that returned an empty slice for everything would
// never panic and would break every read in the filesystem, so what is checked is that the result is the
// correct sub-slice of the input under an independently computed expectation.
func FuzzSliceRange(f *testing.F) {
	// C3 itself: a negative size with a non-zero offset.
	f.Add(100, int64(50), int64(-1))
	f.Add(100, int64(0), int64(-1))
	f.Add(100, int64(99), int64(-100))

	// The boundaries of the clamp arms.
	f.Add(100, int64(0), int64(0))
	f.Add(100, int64(0), int64(100))
	f.Add(100, int64(0), int64(101))
	f.Add(100, int64(100), int64(1))
	f.Add(100, int64(101), int64(1))
	f.Add(100, int64(-1), int64(10))
	f.Add(0, int64(0), int64(0))

	// Overflow: offset+size must not wrap. A wrap would make end negative and put the panic back.
	f.Add(100, int64(1), int64(9223372036854775807))
	f.Add(100, int64(9223372036854775807), int64(1))
	f.Add(100, int64(-9223372036854775808), int64(1))

	f.Fuzz(func(t *testing.T, length int, offset, size int64) {
		// Bound the allocation, not the arguments. The offset and size stay entirely unconstrained —
		// they are the domain under test, and clamping them here would silence the very inputs that
		// caused C3 — but a fuzzer-chosen length would otherwise try to allocate 2 GiB.
		if length < 0 || length > 1<<16 {
			return
		}

		data := make([]byte, length)
		for i := range data {
			// Position-dependent, so a result with the right length at the wrong offset is still caught.
			data[i] = byte(i*7 + 1)
		}

		got := sliceRange(data, offset, size)

		// The expectation, computed independently of the implementation: clamp the offset into
		// [0, length], read "to the end" for a non-positive size, and clamp the end into [offset, length]
		// without ever letting offset+size overflow.
		wantStart := max(offset, 0)
		if wantStart > int64(length) {
			wantStart = int64(length)
		}

		wantEnd := int64(length)
		if size > 0 {
			// Compare before adding rather than after: wantStart+size can overflow to a negative
			// number, and an expectation that overflows cannot check an implementation that must not.
			if size < int64(length)-wantStart {
				wantEnd = wantStart + size
			}
		}

		want := data[wantStart:wantEnd]

		if len(got) != len(want) {
			t.Fatalf("sliceRange(len %d, offset %d, size %d) returned %d bytes, want %d",
				length, offset, size, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sliceRange(len %d, offset %d, size %d) byte %d is %#02x, want %#02x — "+
					"the length is right and the offset is not",
					length, offset, size, i, got[i], want[i])
			}
		}

		// Never a nil slice. A nil result is indistinguishable from an empty one to len() but not to a
		// caller that appends to it or hands it to a writer, and the read path does both.
		if got == nil {
			t.Errorf("sliceRange(len %d, offset %d, size %d) returned nil rather than an empty slice",
				length, offset, size)
		}
	})
}

// TestSliceRangeExpectationIsNotTheImplementation guards the one way [FuzzSliceRange] could be vacuous.
//
// Its expectation is hand-written arithmetic that resembles the implementation's. If the two were ever
// made identical — by copying one into the other during a refactor — the fuzzer would compare a function
// against itself and pass on any behavior at all, including the panic it was written to prevent. These
// cases pin the contract in literals, so the expectation is anchored to something outside both.
func TestSliceRangeExpectationIsNotTheImplementation(t *testing.T) {
	t.Parallel()

	data := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}

	cases := []struct {
		offset, size int64
		want         []byte
		why          string
	}{
		{0, -1, data, "a negative size means to the end — C3's input, which used to panic"},
		{5, -1, data[5:], "and it still honors the offset"},
		{0, 0, data, "zero means to the end, matching the range header the fetch would send"},
		{0, 4, data[:4], "an ordinary range"},
		{6, 4, data[6:], "a range that ends exactly at the end"},
		{6, 40, data[6:], "a range that runs past the end is clamped, not an error"},
		{10, 1, []byte{}, "an offset at the end is an empty read, as read(2) is"},
		{99, 1, []byte{}, "and so is one past it"},
		{-5, 4, data[:4], "a negative offset clamps to zero rather than counting from the end"},
		{1, 9223372036854775807, data[1:], "a size that would overflow offset+size still clamps"},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("offset=%d size=%d", tc.offset, tc.size), func(t *testing.T) {
			t.Parallel()

			got := sliceRange(data, tc.offset, tc.size)

			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v — %s", got, tc.want, tc.why)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v — %s", got, tc.want, tc.why)
				}
			}
		})
	}
}
