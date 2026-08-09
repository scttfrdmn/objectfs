package s3_test

// #204's end-to-end half: an acceleration error withdraws the accelerate endpoint, the read still
// returns correct bytes over the standard endpoint, and the endpoint comes back on its own.
//
// The gate's own tests (acceleration_gate_test.go) drive it directly and can hold a probe open to
// check the concurrency cap. What only this file can show is that the *backend* consults it — that
// GetObject and PutObject route through executeWithAccelerationFallback, that a withdrawn endpoint
// still serves the caller's request rather than failing it, and that the state reaches
// Backend.AccelerationStats, which is the accessor the metrics surface reads. Before #204 that
// accessor did not exist and GetMetrics had no caller outside the package, so the whole condition was
// unobservable from anywhere a user could look.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/circuit"
	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// accelerationFault is the failure S3 produces for a bucket that has not enabled Transfer
// Acceleration. The message is the load-bearing half: S3 gives every acceleration failure the
// InvalidRequest code, which it shares with a bad Range and an oversized copy source, so a fault
// carrying only the code would exercise the classifier's *rejecting* branch and prove nothing about
// the fallback.
//
// 400 rather than 500 so the SDK does not retry it: a retried fault would spend the Times budget on
// one logical request and the standard-endpoint attempt would fail too.
func accelerationFault(method string, times int) testaws.Fault {
	return testaws.Fault{
		Method:  method,
		Status:  400,
		Code:    "InvalidRequest",
		Message: "S3 Transfer Acceleration is not configured on this bucket",
		Times:   times,
	}
}

// TestAnAccelerationErrorFallsBackAndStillServesTheRead is the fallback, seen from the caller.
//
// The bytes are the assertion. A read that reached the accelerate endpoint, got an unusable answer, and
// returned an error to the caller would be a worse outcome than not accelerating at all — and the
// project's degradation rule for a performance capability is to fall back silently and keep serving.
func TestAnAccelerationErrorFallsBackAndStillServesTheRead(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.AccelerationRetry = time.Hour
	})
	s3.RouteAccelerationThroughTheTestEndpoint(backend)

	ctx := context.Background()

	want := []byte("the read must still return these bytes")
	ts.PutObject("accelerated.bin", want)

	ts.InjectFault(accelerationFault("GET", 1))

	got, err := backend.GetObject(ctx, "accelerated.bin", 0, -1)
	if err != nil {
		t.Fatalf("a read that met an acceleration error failed instead of falling back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the fallback returned %q, want %q", got, want)
	}

	if ts.FaultsFired() != 1 {
		t.Fatalf("the injected fault fired %d times, want 1; without it firing this test proves nothing "+
			"— a read that never met an acceleration error succeeds trivially", ts.FaultsFired())
	}

	stats := backend.AccelerationStats()
	if stats.Active {
		t.Error("AccelerationStats reports acceleration active after an acceleration error; this is the " +
			"field an operator reads to find out their mount stopped accelerating")
	}
	if !stats.Configured {
		t.Error("Configured is false; it reports what the operator asked for and must survive a fallback, " +
			"since Configured-and-not-Active is precisely the state #204 made reportable")
	}
	if stats.GateState != circuit.StateOpen {
		t.Errorf("the gate is %v after an acceleration error, want OPEN", stats.GateState)
	}
	if stats.Fallbacks != 1 {
		t.Errorf("Fallbacks = %d, want 1", stats.Fallbacks)
	}
	if stats.RetryPeriod != time.Hour {
		t.Errorf("RetryPeriod = %v, want the configured 1h; an operator looking at a mount in fallback "+
			"is asking when it will try again", stats.RetryPeriod)
	}
}

// TestSubsequentReadsSkipTheWithdrawnEndpoint asserts the withdrawal costs nothing per request.
//
// The point of withdrawing rather than retrying is that later requests do not pay the failed round
// trip. Counted at the wire: after the fallback, a read must produce exactly one request — the
// standard-endpoint one — and not two.
func TestSubsequentReadsSkipTheWithdrawnEndpoint(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.AccelerationRetry = time.Hour
	})
	s3.RouteAccelerationThroughTheTestEndpoint(backend)

	ctx := context.Background()

	ts.PutObject("hot.bin", []byte("payload"))
	ts.InjectFault(accelerationFault("GET", 1))

	if _, err := backend.GetObject(ctx, "hot.bin", 0, -1); err != nil {
		t.Fatalf("the read that triggers the fallback failed: %v", err)
	}

	ts.ResetRequests()

	for i := range 3 {
		if _, err := backend.GetObject(ctx, "hot.bin", 0, -1); err != nil {
			t.Fatalf("read %d after the fallback failed: %v", i, err)
		}
	}

	if got := len(ts.GETs("hot.bin")); got != 3 {
		t.Errorf("3 reads after the fallback issued %d GETs, want 3; a request per read is being sent to "+
			"an endpoint the gate has withdrawn, which is the per-request cost the backoff exists to avoid",
			got)
	}
}

// TestTheAccelerateEndpointComesBackWithinTheBackoff is #204 as a user would state it.
//
// The recovery is asserted through AccelerationStats rather than through a log line or the gate's
// internals, because the issue is half about the state being *reachable*: a recovery visible only
// inside the s3 package is the same invisibility the fallback had.
//
// Verified by mutation: freezing circuit.Config.Timeout at an hour instead of the configured retry
// leaves this test failing at the poll and the rest of the file green.
func TestTheAccelerateEndpointComesBackWithinTheBackoff(t *testing.T) {
	t.Parallel()

	const retry = 25 * time.Millisecond

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.AccelerationRetry = retry
	})
	s3.RouteAccelerationThroughTheTestEndpoint(backend)

	ctx := context.Background()

	ts.PutObject("recovered.bin", []byte("payload"))

	// Exactly one fire: the condition is transient, which is the case the permanent latch got wrong.
	ts.InjectFault(accelerationFault("GET", 1))

	if _, err := backend.GetObject(ctx, "recovered.bin", 0, -1); err != nil {
		t.Fatalf("the read that triggers the fallback failed: %v", err)
	}
	if backend.AccelerationStats().Active {
		t.Fatal("acceleration is still active, so nothing was withdrawn and this test cannot observe a " +
			"recovery")
	}

	// Reads until acceleration is in effect again. Each read is a request that would have gone to the
	// accelerate endpoint had the gate permitted it, which is how a mount actually recovers.
	deadline := time.Now().Add(10 * time.Second)
	for !backend.AccelerationStats().Active {
		if time.Now().After(deadline) {
			t.Fatalf("acceleration is still withdrawn 10s after a %v backoff, over repeated reads; the "+
				"fallback is one-way for the life of the mount, which is #204", retry)
		}

		if _, err := backend.GetObject(ctx, "recovered.bin", 0, -1); err != nil {
			t.Fatalf("a read during the backoff failed: %v", err)
		}

		time.Sleep(time.Millisecond)
	}

	stats := backend.AccelerationStats()
	if stats.GateState != circuit.StateClosed {
		t.Errorf("the gate is %v after a successful probe, want CLOSED: a gate left half-open rations "+
			"acceleration to one request per backoff period", stats.GateState)
	}
	if stats.Requests == 0 {
		t.Error("Requests is zero after recovery, so no request has been recorded against the accelerate " +
			"endpoint and the recovery is not visible as traffic")
	}
}

// TestAccelerationOnACustomEndpointFallsBackRatherThanFailing is a second defect, found by probing
// while #204 was being fixed and not named in it.
//
// The AWS SDK's endpoint ruleset refuses UseAccelerate together with a BaseEndpoint:
//
//	operation error S3: GetObject, resolve auth scheme: resolve endpoint: endpoint rule error,
//	A custom endpoint cannot be combined with S3 Accelerate
//
// It never reaches the network, so it is not a smithy.APIError, so isAccelerationError did not match it
// and the fallback never fired. `use_acceleration: true` with any `endpoint:` set therefore failed
// *every* GET and PUT with STORAGE_READ / STORAGE_WRITE, permanently — on every MinIO, Ceph, RustFS or
// Wasabi deployment that copied the acceleration example from the docs.
//
// This test is the one place the real combination is exercised, so it deliberately does not use
// RouteAccelerationThroughTheTestEndpoint: the substituted client is what makes every other test in this
// file possible and is also exactly the thing whose absence caused this defect.
//
// Refusing the combination at config load is the other candidate fix and is deliberately not taken. Per
// the project thesis, acceleration is a performance capability, so it degrades silently; failing the
// mount would apply the correctness rule to the wrong kind of capability, and would take down mounts
// that are working today.
func TestAccelerationOnACustomEndpointFallsBackRatherThanFailing(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	// The real configuration: acceleration on, endpoint set. No test hook.
	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.UseAccelerate = true
		cfg.AccelerationRetry = time.Hour
	})

	ctx := context.Background()

	want := []byte("this object was unreadable on every S3-compatible deployment")
	ts.PutObject("compat.bin", want)

	got, err := backend.GetObject(ctx, "compat.bin", 0, -1)
	if err != nil {
		t.Fatalf("a read with use_accelerate and a custom endpoint failed: %v\n\nThe SDK refuses the "+
			"combination before sending anything, and the error carries no API code, so it was not "+
			"classified as an acceleration error and no fallback ran. Every GET and PUT on such a mount "+
			"failed for its whole life.", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the fallback returned %q, want %q", got, want)
	}

	// The write side independently: PutObject goes through the same helper, and a mount that can read
	// but not write is not a working mount.
	if err := backend.PutObject(ctx, "compat-write.bin", want, nil); err != nil {
		t.Fatalf("a write with use_accelerate and a custom endpoint failed: %v", err)
	}
	if roundTripped := ts.GetObject("compat-write.bin"); !bytes.Equal(roundTripped, want) {
		t.Errorf("the written object reads back as %q, want %q", roundTripped, want)
	}

	if stats := backend.AccelerationStats(); stats.Active {
		t.Error("acceleration still reports active, so the ruleset refusal was not classified as an " +
			"acceleration error and every request is still being built against a combination the SDK " +
			"rejects")
	}
}
