package s3_test

// A conditional write is the only operation in this package whose *failure* is the outcome its caller
// wants, and that inverts what most of these tests have to check. The interesting assertions are not
// "the write succeeded" but "the write was declined, the stored object is byte-for-byte what it was,
// exactly one of N contenders won, and none of the machinery that normally reacts to a failed request
// reacted to this one".
//
// Everything here runs against internal/testaws — a real S3 endpoint over real HTTP — rather than a
// hand-written mock. That is not a style preference for this feature; it is the only way the tests can
// mean anything. A mock evaluating a precondition against its own in-process map agrees with its caller
// by construction, so a CAS built on one would pass every test while excluding nobody. Substrate's
// conditional-write behavior was measured over seven cases before this code was written, including the
// one that decided Precondition.Validate rejects Absent+ETag locally.

import (
	"bytes"
	"context"
	stderr "errors"
	"fmt"
	"sync"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	objerrors "github.com/scttfrdmn/objectfs/pkg/errors"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// TestPutObjectIfAbsentOnMissingKeySucceeds is the primitive a lease acquisition is built from.
func TestPutObjectIfAbsentOnMissingKeySucceeds(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	etag, err := backend.PutObjectIf(ctx, "lease", []byte("node-a"), nil, types.Precondition{Absent: true})
	if err != nil {
		t.Fatalf("PutObjectIf on an absent key: %v", err)
	}

	// The ETag is asserted non-empty because it is the whole reason this method returns one: a CAS loop
	// continues from it without a HeadObject. A method that wrote correctly and returned "" would look
	// fine here and force an extra round trip on every caller.
	if etag == "" {
		t.Error("PutObjectIf succeeded but reported no ETag; a CAS loop has nothing to continue from")
	}

	if got := string(ts.GetObject("lease")); got != "node-a" {
		t.Errorf("stored object = %q, want %q", got, "node-a")
	}
}

// TestPutObjectIfAbsentOnExistingKeyIsDeclinedAndStoresNothing asserts the bytes, not just the error.
//
// Asserting only the error would pass against an implementation that wrote the object and then reported
// a precondition failure — which is the failure mode that matters most here, since a lease-holder would
// have had its value overwritten by the contender that "lost".
func TestPutObjectIfAbsentOnExistingKeyIsDeclinedAndStoresNothing(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	ts.PutObject("lease", []byte("node-a"))

	etag, err := backend.PutObjectIf(ctx, "lease", []byte("node-b"), nil, types.Precondition{Absent: true})
	if !stderr.Is(err, types.ErrPreconditionFailed) {
		t.Fatalf("PutObjectIf asserting absence of an existing key: err = %v, want ErrPreconditionFailed", err)
	}
	if etag != "" {
		t.Errorf("declined write reported ETag %q, want empty", etag)
	}

	if got := string(ts.GetObject("lease")); got != "node-a" {
		t.Errorf("stored object = %q after a declined write, want %q unchanged", got, "node-a")
	}
}

// TestPutObjectIfMatchingETagSucceeds is the update half of a compare-and-swap.
func TestPutObjectIfMatchingETagSucceeds(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	first, err := backend.PutObjectIf(ctx, "state", []byte("v1"), nil, types.Precondition{Absent: true})
	if err != nil {
		t.Fatalf("initial conditional write: %v", err)
	}

	// The ETag from the first write is used directly, with no HeadObject in between. That is the
	// property under test as much as the success is: a CAS loop that had to re-HEAD between iterations
	// would double the request count and reintroduce the window the precondition closes.
	second, err := backend.PutObjectIf(ctx, "state", []byte("v2"), nil, types.Precondition{ETag: first})
	if err != nil {
		t.Fatalf("PutObjectIf with the current ETag: %v", err)
	}
	if second == "" {
		t.Error("successful conditional update reported no ETag")
	}
	if second == first {
		t.Errorf("ETag %q unchanged after writing different bytes; a CAS loop using it would never "+
			"detect its own write", second)
	}

	if got := string(ts.GetObject("state")); got != "v2" {
		t.Errorf("stored object = %q, want %q", got, "v2")
	}
}

// TestPutObjectIfStaleETagIsDeclinedAndStoresNothing is a lost race in its ordinary form: the caller
// read the state, someone else changed it, and the caller's write must not land.
func TestPutObjectIfStaleETagIsDeclinedAndStoresNothing(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	stale, err := backend.PutObjectIf(ctx, "state", []byte("v1"), nil, types.Precondition{Absent: true})
	if err != nil {
		t.Fatalf("initial conditional write: %v", err)
	}

	// Another writer moves the object on, invalidating the ETag the first caller holds.
	ts.PutObject("state", []byte("v2-by-someone-else"))

	_, err = backend.PutObjectIf(ctx, "state", []byte("v2-by-us"), nil, types.Precondition{ETag: stale})
	if !stderr.Is(err, types.ErrPreconditionFailed) {
		t.Fatalf("PutObjectIf with a stale ETag: err = %v, want ErrPreconditionFailed", err)
	}

	if got := string(ts.GetObject("state")); got != "v2-by-someone-else" {
		t.Errorf("stored object = %q after a declined write, want the other writer's bytes intact", got)
	}
}

// TestPutObjectIfETagOnAbsentKeyIsNotFoundNotPreconditionFailed pins the distinction the whole
// coordination series depends on.
//
// S3 answers 404 rather than 412 for an If-Match against a missing key. The two want different
// recovery: a lost race means re-read and retry, while a vanished object means the state being updated
// no longer exists — and a CAS loop that treats the second as the first spins forever against a key
// nobody is going to recreate. This is verified behavior, not an inference from the specification, and
// it is the arm of translateConditionalError most likely to be "simplified" into the 412 case.
func TestPutObjectIfETagOnAbsentKeyIsNotFoundNotPreconditionFailed(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	_, err := backend.PutObjectIf(ctx, "never-written", []byte("v1"), nil,
		types.Precondition{ETag: `"0123456789abcdef0123456789abcdef"`})
	if err == nil {
		t.Fatal("PutObjectIf asserting an ETag on an absent key succeeded; nothing was excluded")
	}

	if stderr.Is(err, types.ErrPreconditionFailed) {
		t.Fatalf("If-Match on an absent key reported ErrPreconditionFailed: %v\n"+
			"A caller cannot distinguish a lost race from a deleted object, so a CAS loop retries "+
			"forever against a key that is gone", err)
	}

	var objErr *objerrors.ObjectFSError
	if !stderr.As(err, &objErr) {
		t.Fatalf("err = %v (%T), want an *errors.ObjectFSError", err, err)
	}
	if objErr.Code != objerrors.ErrCodeObjectNotFound {
		t.Errorf("err code = %q, want %q", objErr.Code, objerrors.ErrCodeObjectNotFound)
	}
}

// TestPutObjectIfRejectsUnusablePreconditions asserts the two caller errors are refused locally, before
// anything reaches the wire.
//
// The Absent+ETag case is measured rather than assumed: substrate v0.93.0 answers 412 to a request
// carrying both headers, because S3 evaluates both rather than choosing between them, so the
// combination is genuinely unsatisfiable. Rejecting it here makes it a caller error at the call site
// instead of a remote 412 indistinguishable from a genuinely lost race.
func TestPutObjectIfRejectsUnusablePreconditions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cond types.Precondition
		why  string
	}{
		{
			name: "zero precondition asserts nothing",
			cond: types.Precondition{},
			why:  "a caller that meant to write unconditionally reached for the wrong method",
		},
		{
			name: "absent and etag together",
			cond: types.Precondition{Absent: true, ETag: `"0123456789abcdef0123456789abcdef"`},
			why:  "the two headers make contradictory claims and S3 evaluates both, so it can never succeed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			backend := ts.Backend()

			_, err := backend.PutObjectIf(context.Background(), "k", []byte("v"), nil, tt.cond)
			if !stderr.Is(err, types.ErrInvalidPrecondition) {
				t.Fatalf("PutObjectIf with %s: err = %v, want ErrInvalidPrecondition (%s)", tt.name, err, tt.why)
			}

			// Refused locally means nothing was written, and the key not existing is the only way to
			// check that from outside.
			if ts.ObjectExists("k") {
				t.Error("a rejected precondition still wrote the object")
			}
		})
	}
}

// TestConcurrentAbsentWritersElectExactlyOne is the property everything downstream rests on.
//
// A lease, a tier transition performed by one node, and the executeStrongConsistency replacement all
// reduce to this: N processes assert absence of the same key, and the store — not any of them — decides
// which one proceeds. If two can win, every guarantee built above it is void.
func TestConcurrentAbsentWritersElectExactlyOne(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	const contenders = 8

	var (
		start   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		winners []string
		losers  int
		other   []error
	)

	start.Add(1)
	for i := range contenders {
		done.Add(1)
		go func() {
			defer done.Done()

			// Every goroutine blocks here so the requests go out together. Staggered writes would let
			// each contender observe the previous winner's object and pass for a reason that has nothing
			// to do with the store arbitrating a race.
			start.Wait()

			value := fmt.Sprintf("node-%d", i)
			_, err := backend.PutObjectIf(ctx, "lease", []byte(value), nil, types.Precondition{Absent: true})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, value)
			case stderr.Is(err, types.ErrPreconditionFailed):
				losers++
			default:
				other = append(other, err)
			}
		}()
	}

	start.Done()
	done.Wait()

	if len(other) > 0 {
		t.Fatalf("contenders failed for reasons other than losing the race: %v", other)
	}
	if len(winners) != 1 {
		t.Fatalf("%d contenders produced %d winners (%v), want exactly 1", contenders, len(winners), winners)
	}
	if losers != contenders-1 {
		t.Errorf("losers = %d, want %d", losers, contenders-1)
	}

	// The winner's bytes are the ones stored. A run where one write reported success while a different
	// contender's value landed would satisfy every count above.
	if got := string(ts.GetObject("lease")); got != winners[0] {
		t.Errorf("stored object = %q, but %q reported winning", got, winners[0])
	}
}

// TestPreconditionFailureDoesNotDegradeWritesOrTripTheBreaker is the test that makes the
// errors.IsServiceFailure entry load-bearing rather than decorative.
//
// The health tracker's ErrorThreshold is 3 and the breaker counts failures too, so if a lost race
// counted as a failure then four contenders for one lease would take writes offline for all of them —
// under exactly the contention the precondition exists to arbitrate. The final unconditional write is
// how the test detects that: a degraded s3-writes component refuses it at the health gate.
func TestPreconditionFailureDoesNotDegradeWritesOrTripTheBreaker(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	ts.PutObject("lease", []byte("held"))

	// Well past ErrorThreshold (3) and past the breaker's ReadyToTrip count.
	const attempts = 10
	for i := range attempts {
		_, err := backend.PutObjectIf(ctx, "lease", []byte("contender"), nil, types.Precondition{Absent: true})
		if !stderr.Is(err, types.ErrPreconditionFailed) {
			t.Fatalf("attempt %d: err = %v, want ErrPreconditionFailed", i, err)
		}
	}

	if err := backend.PutObject(ctx, "unrelated", []byte("x"), nil); err != nil {
		t.Fatalf("an ordinary write failed after %d lost races: %v\n"+
			"Losing a race is the mechanism, not a malfunction: counting it as a service failure takes "+
			"writes offline under the contention the precondition arbitrates", attempts, err)
	}
}

// TestPreconditionFailureIsNotRetried asserts the retry policy by counting requests.
//
// A precondition failure is a definitive answer: the object's state is not what the caller asserted,
// and asking again cannot change that. Retrying it spends requests to be told the same thing — five
// times over, at the default MaxAttempts — and on a contended lease that is a burst of load produced by
// the losers.
//
// Counting PutObject operations at the endpoint is the assertion because it is the only place a
// silently-retried request is visible. Asserting on the returned error would pass either way.
func TestPreconditionFailureIsNotRetried(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	ts.PutObject("lease", []byte("held"))

	// The capability probe issues a PutObject of its own — a conditional one it expects to be declined
	// — and it runs on the first conditional write. Warming it before counting is not tidying: without
	// this line the count is 2 and the test reads as "the write was retried once", which is what the
	// first run of it reported.
	_ = backend.Capabilities()

	before := ts.Operations("PutObject")

	_, err := backend.PutObjectIf(ctx, "lease", []byte("contender"), nil, types.Precondition{Absent: true})
	if !stderr.Is(err, types.ErrPreconditionFailed) {
		t.Fatalf("err = %v, want ErrPreconditionFailed", err)
	}

	if got := ts.Operations("PutObject") - before; got != 1 {
		t.Errorf("a declined conditional write issued %d PutObject requests, want 1; a definitive "+
			"answer was retried", got)
	}
}

// TestCapabilitiesReportsConditionalWriteAgainstAnEvaluatingEndpoint asserts the probe answers
// correctly against an endpoint that does evaluate preconditions.
//
// The negative direction — an endpoint that accepts the header and ignores it — is the dangerous one and
// is covered by TestPutObjectIfRefusesWhenTheEndpointIgnoresPreconditions, which needs an endpoint that
// misbehaves and so cannot use testaws.
func TestCapabilitiesReportsConditionalWriteAgainstAnEvaluatingEndpoint(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()

	caps := backend.Capabilities()
	if !caps.ConditionalWrite {
		t.Fatalf("probe reported conditional writes unsupported against an endpoint that evaluates "+
			"them: %s", caps.ConditionalWriteDetail)
	}
	if caps.ConditionalWriteDetail != "" {
		t.Errorf("ConditionalWriteDetail = %q, want empty when the capability is present",
			caps.ConditionalWriteDetail)
	}

	// The probe writes nothing. An endpoint that evaluated the If-Match answered not-found, so the key
	// must not exist — and a probe that left an object behind would be one every mount deposits in the
	// user's bucket.
	if ts.ObjectExists(s3.CapabilityProbeKeyForTest) {
		t.Errorf("the capability probe left an object at %q", s3.CapabilityProbeKeyForTest)
	}
}

// TestCapabilitiesIsProbedOnce asserts the answer is cached.
//
// A conditional write sits on a coordination path where an extra round trip per attempt is a real cost,
// and the answer cannot change under a running process because the endpoint is fixed at construction.
func TestCapabilitiesIsProbedOnce(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()

	first := ts.Operations("PutObject")
	_ = backend.Capabilities()
	afterFirst := ts.Operations("PutObject")

	for range 5 {
		_ = backend.Capabilities()
	}

	if got := ts.Operations("PutObject") - afterFirst; got != 0 {
		t.Errorf("five further Capabilities calls issued %d requests, want 0", got)
	}
	if afterFirst-first == 0 {
		t.Error("the first Capabilities call issued no request; the capability is not being established " +
			"by attempt")
	}
}

// TestPutObjectIfAbsentAboveTheMultipartThresholdIsStillConditional is the assertion that a large
// conditional write is conditional.
//
// A precondition on a multipart upload can only be evaluated at CompleteMultipartUpload — parts are
// not an object until they are assembled — so PutObjectIf's two size branches carry the assertion by
// two different mechanisms, and only one of them is a header on the request the caller's data rides
// on. Without this test a conditional write above MultipartThreshold would silently become an
// unguarded one: every contender would upload its parts, every Complete would succeed, and the lease
// would be held by whichever finished last.
func TestPutObjectIfAbsentAboveTheMultipartThresholdIsStillConditional(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(multipartConfig)
	ctx := context.Background()

	held := testaws.DeterministicBytes("held", multipartObject)
	ts.PutObject("big-lease", held)

	contender := testaws.DeterministicBytes("contender", multipartObject)

	etag, err := backend.PutObjectIf(ctx, "big-lease", contender, nil, types.Precondition{Absent: true})
	if !stderr.Is(err, types.ErrPreconditionFailed) {
		t.Fatalf("a %d-byte conditional write over an existing key: err = %v, want ErrPreconditionFailed.\n"+
			"Above MultipartThreshold the assertion has to be carried on CompleteMultipartUpload; if it is "+
			"not carried at all the write simply succeeds", len(contender), err)
	}
	if etag != "" {
		t.Errorf("declined multipart write reported ETag %q, want empty", etag)
	}

	if got := ts.GetObject("big-lease"); !bytes.Equal(got, held) {
		t.Errorf("the stored object changed after a declined multipart write: %d bytes, want the "+
			"original %d", len(got), len(held))
	}

	// The parts do not leak. A conditional multipart write that loses its race has already transferred
	// every part, so the abort matters more here than anywhere: those parts are billed and invisible to
	// ListObjects.
	if ups := ts.MultipartUploads(); len(ups) != 0 {
		t.Errorf("after a declined conditional multipart write %d upload(s) are still open: %v", len(ups), ups)
	}
}

// TestPutObjectIfAbsentAboveTheMultipartThresholdSucceedsOnAMissingKey is the other half: the
// assertion has to permit the write it was supposed to permit.
//
// Without it, a mutation that made the multipart path reject unconditionally — or one that sent
// If-None-Match on the wrong request and got a 412 for the wrong reason — would leave the test above
// passing while large conditional writes never landed at all.
func TestPutObjectIfAbsentAboveTheMultipartThresholdSucceedsOnAMissingKey(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(multipartConfig)
	ctx := context.Background()

	want := testaws.DeterministicBytes("winner", multipartObject)

	etag, err := backend.PutObjectIf(ctx, "big-lease", want, nil, types.Precondition{Absent: true})
	if err != nil {
		t.Fatalf("conditional multipart write to an absent key: %v", err)
	}
	if etag == "" {
		t.Error("successful conditional multipart write reported no ETag; a CAS loop has nothing to " +
			"continue from")
	}

	if got := ts.GetObject("big-lease"); !bytes.Equal(got, want) {
		t.Errorf("stored object is %d bytes, want %d", len(got), len(want))
	}

	if ups := ts.MultipartUploads(); len(ups) != 0 {
		t.Errorf("after a successful conditional multipart write %d upload(s) are still open: %v",
			len(ups), ups)
	}
}
