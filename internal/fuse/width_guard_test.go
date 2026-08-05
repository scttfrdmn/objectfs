//go:build linux || darwin

package fuse

import (
	"math"
	"strconv"
	"testing"
)

// These tests cover the two width-clamping helpers in filesystem.go.
//
// A caveat worth stating plainly, because it decides where the real gate lives: #198's defect was a
// *compile* error, and no test running on a 64-bit host can catch it. `i > 0xFFFFFFFF` and
// `uint64(i) > math.MaxUint32` behave identically on amd64 and arm64; the difference only appears
// when `int` is 32 bits, at which point the first form does not build at all. So the test that
// guards the regression is the linux/arm cross-build cell in .github/workflows/ci.yml — the same
// shape of gate as #240's build-tags matrix, and for the same reason: an unbuilt target is not
// merely untested.
//
// What these tests do cover is the behavior, which had never been asserted at all: the clamps, the
// boundaries, and the ordering of the two branches. The negative check must precede the upper-bound
// check, because uint64(-1) is 2^64-1 and would clamp to MaxUint32 rather than to zero — a silent
// off-by-4-billion in a function whose entire job is to not do that.

func TestSafeIntToUint32(t *testing.T) {
	t.Parallel()

	// Built through a uint64 variable rather than written as `int(math.MaxUint32)`. That constant
	// conversion is itself a compile error on a 32-bit platform, for exactly the reason this file
	// exists — a test for a 32-bit compile bug that cannot be compiled on 32-bit would be no test.
	// On a 32-bit build these two truncate to -1 and 0, which is why their cases are skipped there.
	wide := uint64(math.MaxUint32)
	maxUint32 := int(wide)
	overMaxUint32 := int(wide + 1)

	tests := []struct {
		name    string
		in      int
		want    uint32
		only64  bool
		comment string
	}{
		{name: "zero", in: 0, want: 0},
		{name: "one", in: 1, want: 1},
		{name: "typical uid", in: 501, want: 501},
		{
			name: "negative one clamps to zero, not to MaxUint32",
			in:   -1, want: 0,
			comment: "the branch order case: uint64(-1) is 2^64-1",
		},
		{name: "most negative int clamps to zero", in: math.MinInt, want: 0},
		{
			name: "MaxUint32 passes through unclamped",
			in:   maxUint32, want: math.MaxUint32, only64: true,
			comment: "the boundary itself is representable and must not be clamped",
		},
		{
			name: "MaxUint32+1 clamps",
			in:   overMaxUint32, want: math.MaxUint32, only64: true,
		},
		{
			name: "MaxInt clamps on a 64-bit platform",
			in:   math.MaxInt, want: math.MaxUint32, only64: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.only64 && strconv.IntSize < 64 {
				t.Skipf("int is %d bits here, so %s is not representable", strconv.IntSize, tt.name)
			}

			if got := safeIntToUint32(tt.in); got != tt.want {
				t.Errorf("safeIntToUint32(%d) = %d, want %d%s", tt.in, got, tt.want, note(tt.comment))
			}
		})
	}
}

// TestSafeIntToUint32OnA32BitInt checks the one boundary a 32-bit platform actually has.
//
// There, math.MaxInt is 2^31-1 — below MaxUint32 — so the largest possible input passes through
// rather than clamping, and the upper branch is unreachable. Asserting it separately keeps the table
// above from needing a second platform-dependent `want`.
func TestSafeIntToUint32OnA32BitInt(t *testing.T) {
	t.Parallel()

	if strconv.IntSize != 32 {
		t.Skipf("int is %d bits here; this asserts the 32-bit boundary", strconv.IntSize)
	}

	if got := safeIntToUint32(math.MaxInt); got != math.MaxInt32 {
		t.Errorf("safeIntToUint32(math.MaxInt) = %d, want %d — on a 32-bit platform every "+
			"non-negative int fits in uint32 and none should clamp", got, math.MaxInt32)
	}
}

func TestSafeInt64ToUint64(t *testing.T) {
	t.Parallel()

	// This one has no upper bound to test, and that is the finding rather than an omission: int64 and
	// uint64 are both 64 bits on every platform Go targets, so the only int64 values uint64 cannot
	// represent are the negative ones. The #198 audit asked whether this function had the same defect
	// as its neighbor; it does not, because it has no constant to overflow.
	tests := []struct {
		name string
		in   int64
		want uint64
	}{
		{name: "zero", in: 0, want: 0},
		{name: "one", in: 1, want: 1},
		{name: "a plausible file size", in: 4 << 30, want: 4 << 30},
		{name: "negative one clamps to zero", in: -1, want: 0},
		{name: "most negative int64 clamps to zero", in: math.MinInt64, want: 0},
		{
			name: "MaxInt64 passes through unclamped",
			in:   math.MaxInt64, want: math.MaxInt64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := safeInt64ToUint64(tt.in); got != tt.want {
				t.Errorf("safeInt64ToUint64(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// note renders a case's rationale into a failure message, or nothing when there is none.
func note(comment string) string {
	if comment == "" {
		return ""
	}

	return " (" + comment + ")"
}
