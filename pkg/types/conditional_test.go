package types

// Precondition.Validate is the only logic in this package — everything else here is interface and
// struct declarations — and it is the one place where an invalid precondition is caught before it can
// reach a store.
//
// That makes the Absent+ETag case worth testing here rather than only through a backend. It is a
// *measured* rejection, not a defensive one: substrate v0.93.0 and real S3 both answer 412 to a request
// carrying both headers, because S3 evaluates each rather than choosing between them, so the
// combination is genuinely unsatisfiable. Rejecting it locally turns what would arrive as a remote 412
// — indistinguishable from a genuinely lost race — into a caller error at the call site.

import (
	stderr "errors"
	"testing"
)

func TestPreconditionValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cond Precondition
		want error
		why  string
	}{
		{
			name: "absence is a lease acquisition",
			cond: Precondition{Absent: true},
			want: nil,
		},
		{
			name: "an ETag is a compare-and-swap",
			cond: Precondition{ETag: `"0123456789abcdef0123456789abcdef"`},
			want: nil,
		},
		{
			name: "the zero value asserts nothing",
			cond: Precondition{},
			want: ErrInvalidPrecondition,
			why: "a caller that meant to write unconditionally reached for PutObjectIf; letting it through " +
				"would have that caller believing it holds a lease it never contended for",
		},
		{
			name: "absence and an ETag together",
			cond: Precondition{Absent: true, ETag: `"0123456789abcdef0123456789abcdef"`},
			want: ErrInvalidPrecondition,
			why: "the two headers make contradictory claims and S3 evaluates both, so the request can never " +
				"succeed — measured, not assumed",
		},
		{
			name: "an empty ETag is not an assertion",
			cond: Precondition{ETag: ""},
			want: ErrInvalidPrecondition,
			why: "identical to the zero value; worth pinning separately because an If-Match built from an " +
				"unpopulated ObjectInfo.ETag is exactly how a caller reaches it by accident",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := tt.cond.Validate(); !stderr.Is(err, tt.want) {
				t.Errorf("Validate() = %v, want %v: %s", err, tt.want, tt.why)
			}
		})
	}
}

// TestPreconditionIsZero pins IsZero separately from Validate because they are not the same predicate
// and a reader could reasonably assume they were.
//
// IsZero asks "does this assert nothing" and is what the multipart path consults to decide whether a
// request carries a precondition at all; Validate additionally rejects the over-specified case. An
// IsZero that returned true for Absent+ETag would make a conditional multipart write silently
// unconditional, which is a failure no error would report.
func TestPreconditionIsZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cond Precondition
		want bool
	}{
		{"the zero value", Precondition{}, true},
		{"absence asserted", Precondition{Absent: true}, false},
		{"an ETag asserted", Precondition{ETag: `"abc"`}, false},
		{"both asserted — invalid, but not empty", Precondition{Absent: true, ETag: `"abc"`}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.cond.IsZero(); got != tt.want {
				t.Errorf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestConditionalSentinelsAreDistinct asserts the four sentinels are four values.
//
// They are declared in one var block with errors.New, so this cannot fail today — which is the point:
// it fails if one is ever re-expressed in terms of another. The temptation is real, since
// ErrConditionalConflict and ErrPreconditionFailed both mean "the write did not land" and wrapping one
// in the other reads like tidying. It would silently make every conflict retry-exempt, or every
// precondition failure retryable, depending on the direction — and those are opposite policies.
func TestConditionalSentinelsAreDistinct(t *testing.T) {
	t.Parallel()

	sentinels := map[string]error{
		"ErrPreconditionFailed":  ErrPreconditionFailed,
		"ErrConditionalConflict": ErrConditionalConflict,
		"ErrInvalidPrecondition": ErrInvalidPrecondition,
		"ErrNotSupported":        ErrNotSupported,
	}

	for aName, a := range sentinels {
		for bName, b := range sentinels {
			if aName == bName {
				continue
			}
			if stderr.Is(a, b) {
				t.Errorf("errors.Is(%s, %s) is true; a caller cannot tell the two apart, and they carry "+
					"opposite retry policies", aName, bName)
			}
		}
	}
}
