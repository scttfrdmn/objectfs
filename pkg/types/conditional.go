package types

import "errors"

// Precondition is an assertion about a key's current state, evaluated by the backend at write time.
//
// The zero value asserts nothing and is therefore invalid for [Backend.PutObjectIf] — an empty
// precondition there is a caller that meant to write unconditionally and reached for the wrong
// method. Implementations report it as [ErrInvalidPrecondition] rather than performing the write,
// because the alternative is a caller believing it holds a lease it never contended for.
//
// Absent and ETag are mutually exclusive: they map to different HTTP headers making contradictory
// claims about the same key, and S3 evaluates both rather than choosing, so a request carrying each
// can never succeed. Rejecting the combination locally makes that a caller error at the call site
// instead of a remote 412 indistinguishable from a genuinely lost race.
type Precondition struct {
	// Absent asserts the key does not currently exist. Sent as If-None-Match: *.
	//
	// This is the primitive a lease acquisition is built from: concurrent writers all asserting
	// absence resolve to exactly one success, decided by the store rather than by any of them.
	Absent bool

	// ETag asserts the key currently has this ETag. Sent as If-Match.
	//
	// An absent key is not-found-shaped rather than [ErrPreconditionFailed], because those want
	// different recovery: a lost race means recompute and retry, a vanished object means the state
	// being updated no longer exists. Implementations must preserve that distinction — S3 answers
	// 404 rather than 412 for If-Match against a missing key, which is verified behavior and not an
	// inference from the specification.
	ETag string
}

// IsZero reports whether p asserts nothing, which is not a valid precondition for a conditional
// write.
func (p Precondition) IsZero() bool {
	return !p.Absent && p.ETag == ""
}

// Validate reports whether p is a usable precondition, naming the problem when it is not.
func (p Precondition) Validate() error {
	switch {
	case p.IsZero():
		return ErrInvalidPrecondition
	case p.Absent && p.ETag != "":
		return ErrInvalidPrecondition
	default:
		return nil
	}
}

// Sentinel errors for conditional writes. Match with errors.Is; implementations wrap these with %w
// and add context, so do not compare with ==.
var (
	// ErrPreconditionFailed means the assertion did not hold and nothing was written.
	//
	// For a caller racing for a lease this is the expected outcome, not a failure: it is how a
	// contender learns another one won. It must never be retried — the answer is definitive, and
	// retrying it burns requests to be told the same thing. It must also never be logged as an
	// error by the backend for the same reason.
	ErrPreconditionFailed = errors.New("types: precondition failed")

	// ErrConditionalConflict means the write raced a delete or another conditional write.
	//
	// Unlike ErrPreconditionFailed the caller's view of the state is not necessarily stale, so
	// retrying the same write may simply succeed — which is why the two are distinct sentinels
	// rather than one. On a multipart completion it additionally means the upload ID can no longer
	// be completed.
	ErrConditionalConflict = errors.New("types: conditional request conflict")

	// ErrInvalidPrecondition means the [Precondition] itself is unusable: it asserts nothing, or it
	// asserts both absence and a specific ETag. This is a caller error, distinct from a precondition
	// that was evaluated and did not hold.
	ErrInvalidPrecondition = errors.New("types: invalid precondition")

	// ErrNotSupported means this backend cannot perform the operation at all.
	//
	// For conditional writes this is a statement about the endpoint in front of the process, not
	// about the build: an S3-compatible store that ignores conditional headers gets this, because a
	// precondition it silently drops is worse than one it refuses.
	ErrNotSupported = errors.New("types: operation not supported")
)

// BackendCapabilities reports what the endpoint actually in front of this process implements,
// established by attempting the operation rather than by configuration.
//
// A configuration flag or an endpoint-URL heuristic can be wrong about the store it is pointed at;
// an attempt cannot. This matters most for the dangerous direction: a store that *accepts* a
// conditional header and ignores it looks identical to one that honors it, from every angle except
// the outcome of a race — which is exactly what a coordination feature must not discover in
// production.
type BackendCapabilities struct {
	// ConditionalWrite reports whether preconditions are evaluated, not merely accepted. False
	// means [Backend.PutObjectIf] returns [ErrNotSupported], and a caller needing mutual exclusion
	// must refuse to start rather than proceed unguarded.
	ConditionalWrite bool

	// ConditionalWriteDetail explains a false ConditionalWrite, for an operator-facing message.
	// It is empty when ConditionalWrite is true.
	ConditionalWriteDetail string
}

// CapabilityReporter is implemented by backends that can describe what their endpoint supports.
//
// It is separate from [Backend] so that a test double or an alternative implementation is not
// obliged to answer a question it has no way to establish. A caller that needs the answer and does
// not get it must treat the capability as absent, on the same fail-closed reasoning as
// ConditionalWrite itself.
type CapabilityReporter interface {
	Capabilities() BackendCapabilities
}
