package health

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/objectfs/objectfs/pkg/errors"
)

// TestMissingObjectDoesNotDegradeReads pins the classification defect.
//
// Ten reads of a key that does not exist drove the S3 read component to unavailable, after which the
// availability gate refused every read — including reads of objects that existed — for the life of
// the process. A 404 for an object nobody wrote means the service is up, reachable, authenticating,
// and answering correctly. Counting it as a health failure lets a handful of stat(2) calls on absent
// paths take a whole mount offline.
func TestMissingObjectDoesNotDegradeReads(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	tr := NewTracker(cfg)
	tr.RegisterComponent("s3-reads")

	notFound := errors.NewError(errors.ErrCodeObjectNotFound, "object not found")
	for range cfg.UnavailableThreshold * 3 {
		tr.RecordError("s3-reads", notFound)
	}

	if got := tr.GetState("s3-reads"); got != StateHealthy {
		t.Errorf("state is %s after %d reads of a missing object, want healthy",
			got, cfg.UnavailableThreshold*3)
	}
	if !tr.CanRead("s3-reads") {
		t.Error("reads refused after reads of a missing object; an existing object would be unreadable")
	}

	h, err := tr.GetComponentHealth("s3-reads")
	if err != nil {
		t.Fatalf("GetComponentHealth: %v", err)
	}
	if h.ConsecutiveErrors != 0 {
		t.Errorf("consecutive_errors is %d, want 0: a missing object is an answer, not a failure",
			h.ConsecutiveErrors)
	}
}

// TestOrdinaryConditionsDoNotDegrade covers the rest of the non-failure codes on the same principle.
func TestOrdinaryConditionsDoNotDegrade(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code errors.ErrorCode
		why  string
	}{
		{errors.ErrCodeObjectNotFound, "the service answered: it is not there"},
		{errors.ErrCodeFileNotFound, "same, by the POSIX-facing name"},
		{errors.ErrCodeNotEmpty, "rmdir on a populated directory is an ordinary refusal"},
		{errors.ErrCodeDirectoryExists, "mkdir of an existing directory is an ordinary refusal"},
		{errors.ErrCodeNotDirectory, "a path component that is a file is the caller's mistake"},
		{errors.ErrCodeInvalidState, "an unrestored Glacier object is the object's state, not the service's"},
		{errors.ErrCodeValidationFailed, "rejected before the service was asked"},
		{errors.ErrCodeTierValidation, "rejected before the service was asked"},
		{errors.ErrCodePathInvalid, "the caller sent something invalid"},
		{errors.ErrCodeOperationCanceled, "the caller withdrew the request"},
	}

	cfg := DefaultConfig()

	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			t.Parallel()

			tr := NewTracker(cfg)
			tr.RegisterComponent("c")

			for range cfg.UnavailableThreshold * 2 {
				tr.RecordError("c", errors.NewError(tc.code, "test"))
			}

			if got := tr.GetState("c"); got != StateHealthy {
				t.Errorf("%s degraded the component to %s, but %s", tc.code, got, tc.why)
			}
		})
	}
}

// TestRealFailuresStillDegrade is the other direction. The classifier is worthless if it also
// swallows evidence of an actual outage — a tracker that never degrades is the same as no tracker.
func TestRealFailuresStillDegrade(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"network error", errors.NewError(errors.ErrCodeNetworkError, "dial tcp: connection refused")},
		{"timeout", errors.NewError(errors.ErrCodeOperationTimeout, "context deadline exceeded")},
		{"throttled", errors.NewError(errors.ErrCodeResourceExhausted, "SlowDown")},
		{"internal", errors.NewError(errors.ErrCodeInternalError, "500 InternalError")},
		{"bucket gone", errors.NewError(errors.ErrCodeBucketNotFound, "NoSuchBucket")},
		// An error with no ObjectFS code counts as a failure. Defaulting an unclassified error to
		// "healthy" would let a whole class of real outage go unnoticed.
		{"unclassified", fmt.Errorf("something went wrong")},
	}

	cfg := DefaultConfig()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tr := NewTracker(cfg)
			tr.RegisterComponent("c")

			for range cfg.UnavailableThreshold {
				tr.RecordError("c", tc.err)
			}

			if got := tr.GetState("c"); got != StateUnavailable {
				t.Errorf("state is %s after %d %s errors, want unavailable",
					got, cfg.UnavailableThreshold, tc.name)
			}
			if tr.CanRead("c") {
				t.Error("reads admitted immediately after the component became unavailable; the probe " +
					"clock was not armed, so a failed service is retried on every call")
			}
		})
	}
}

// TestUnavailableComponentRecoversOnProbe pins the latch defect.
//
// StateUnavailable was a one-way door: the gate refused every operation, and the only thing that
// could clear the state was a success recorded by an operation the gate had just refused. Nothing in
// ObjectFS calls StartHealthChecks, so no other path could supply one either. A transient outage
// therefore took the mount down permanently.
func TestUnavailableComponentRecoversOnProbe(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.ProbeAfter = 30 * time.Millisecond
	tr := NewTracker(cfg)
	tr.RegisterComponent("s3-reads")

	outage := errors.NewError(errors.ErrCodeNetworkError, "dial tcp: connection refused")
	for range cfg.UnavailableThreshold {
		tr.RecordError("s3-reads", outage)
	}

	if tr.GetState("s3-reads") != StateUnavailable {
		t.Fatalf("a real outage did not latch: state=%s", tr.GetState("s3-reads"))
	}
	if tr.CanRead("s3-reads") {
		t.Fatal("a read was admitted before ProbeAfter elapsed")
	}

	time.Sleep(2 * cfg.ProbeAfter)

	if !tr.CanRead("s3-reads") {
		t.Fatal("no probe admitted after ProbeAfter elapsed: the component is stuck for the life of " +
			"the process, because the success that would clear it can only come from an operation the " +
			"gate refuses")
	}

	// The state itself must still tell the truth during a probe: the service has been allowed to try,
	// not shown to work.
	if got := tr.GetState("s3-reads"); got != StateUnavailable {
		t.Errorf("state during a probe is %s, want unavailable — a probe is permission to try, not a "+
			"recovery", got)
	}

	tr.RecordSuccess("s3-reads")

	if got := tr.GetState("s3-reads"); got != StateHealthy {
		t.Errorf("state is %s after a clean probe, want healthy. Recovery cannot require N successes: "+
			"the gate admits one operation per probe interval, so N successes would take N intervals",
			got)
	}
}

// TestFailedProbeDoesNotReopenTheGate checks the failure side of a probe. A probe that fails must not
// leave the component admitting traffic, or an outage would be met with full load.
func TestFailedProbeDoesNotReopenTheGate(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.ProbeAfter = 30 * time.Millisecond
	tr := NewTracker(cfg)
	tr.RegisterComponent("c")

	outage := errors.NewError(errors.ErrCodeNetworkError, "connection refused")
	for range cfg.UnavailableThreshold {
		tr.RecordError("c", outage)
	}

	time.Sleep(2 * cfg.ProbeAfter)
	if !tr.CanRead("c") {
		t.Fatal("no probe admitted")
	}

	tr.RecordError("c", outage)

	if tr.CanRead("c") {
		t.Error("the gate is still open after the probe failed")
	}
	if got := tr.GetState("c"); got != StateUnavailable {
		t.Errorf("state is %s after a failed probe, want unavailable", got)
	}

	// And the next probe is one interval out, not immediate.
	if tr.CanRead("c") {
		t.Error("a second probe was admitted immediately after the first one failed")
	}
	time.Sleep(2 * cfg.ProbeAfter)
	if !tr.CanRead("c") {
		t.Error("no further probe admitted after another interval; a failed probe must not latch the " +
			"component permanently")
	}
}

// TestProbeIsNotLatchedByALostOutcome is the reason admission is decided on a timestamp rather than
// on the probing flag. A caller that panics, or a path that forgets to record its outcome, leaves the
// flag set forever — and gating on it would rebuild the very defect this mechanism exists to fix.
func TestProbeIsNotLatchedByALostOutcome(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.ProbeAfter = 30 * time.Millisecond
	tr := NewTracker(cfg)
	tr.RegisterComponent("c")

	for range cfg.UnavailableThreshold {
		tr.RecordError("c", errors.NewError(errors.ErrCodeNetworkError, "refused"))
	}

	time.Sleep(2 * cfg.ProbeAfter)
	if !tr.CanRead("c") {
		t.Fatal("no first probe admitted")
	}

	// Deliberately record nothing: the operation vanished.
	time.Sleep(2 * cfg.ProbeAfter)

	if !tr.CanRead("c") {
		t.Error("a probe whose outcome was never recorded latched the component permanently")
	}
}

// TestConcurrentProbesAdmitOne checks that a burst of traffic against an unavailable component
// yields one probe, not thousands. The gate must not stop being a gate exactly when the service is
// least able to absorb load.
func TestConcurrentProbesAdmitOne(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.ProbeAfter = 20 * time.Millisecond
	tr := NewTracker(cfg)
	tr.RegisterComponent("c")

	for range cfg.UnavailableThreshold {
		tr.RecordError("c", errors.NewError(errors.ErrCodeNetworkError, "refused"))
	}

	time.Sleep(2 * cfg.ProbeAfter)

	const callers = 64
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted int
	)
	for range callers {
		wg.Go(func() {
			if tr.CanRead("c") {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if admitted != 1 {
		t.Errorf("%d of %d concurrent callers were admitted as probes, want exactly 1", admitted, callers)
	}
}

// TestProbeDisabledKeepsOldBehaviour documents that ProbeAfter <= 0 opts out. A deployment that wants
// a component to stay down until something else intervenes can have that, explicitly.
func TestProbeDisabledKeepsOldBehaviour(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.ProbeAfter = 0
	tr := NewTracker(cfg)
	tr.RegisterComponent("c")

	for range cfg.UnavailableThreshold {
		tr.RecordError("c", errors.NewError(errors.ErrCodeNetworkError, "refused"))
	}

	time.Sleep(20 * time.Millisecond)

	if tr.CanRead("c") {
		t.Error("a probe was admitted with ProbeAfter disabled")
	}
}

// TestSuccessStillRecoversDegradedWithoutAProbe guards the pre-existing recovery path. A degraded
// component admits its own operations, so it never needs a probe, and the incremental
// success-decrements-errors behavior must survive the addition of the probe path.
func TestSuccessStillRecoversDegradedWithoutAProbe(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	tr := NewTracker(cfg)
	tr.RegisterComponent("c")

	for range cfg.ErrorThreshold {
		tr.RecordError("c", errors.NewError(errors.ErrCodeNetworkError, "refused"))
	}
	if got := tr.GetState("c"); got != StateDegraded {
		t.Fatalf("state is %s, want degraded", got)
	}
	if !tr.CanRead("c") {
		t.Fatal("a degraded component refused a read")
	}

	for range cfg.ErrorThreshold {
		tr.RecordSuccess("c")
	}
	if got := tr.GetState("c"); got != StateHealthy {
		t.Errorf("state is %s after %d successes, want healthy", got, cfg.ErrorThreshold)
	}
}
