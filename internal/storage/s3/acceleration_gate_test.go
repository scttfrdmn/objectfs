package s3

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// #204: the acceleration fallback was a one-way latch. One acceleration error — including a DNS
// failure lasting seconds — sent every later request in the mount's life to the standard endpoint,
// and nothing reported it had happened.
//
// These tests drive the gate directly, with a ClientManager built by hand as
// TestAccelerationFallbackIsRaceFree does. What that buys over the end-to-end tests in
// acceleration_recovery_test.go is control of the clock-shaped parts: the backoff can be milliseconds,
// and the half-open probe can be held open while a second attempt is made, which is the property that
// decides whether a mount under load sends one probe or hundreds.
//
// What it does not buy is any evidence that the backend calls the gate, or that a fallback returns
// correct bytes. That is what the external tests are for, and neither set is sufficient alone.

// gateFixture builds a gate over a ClientManager with acceleration active, and returns both plus the
// metrics collector the gate reports through.
func gateFixture(t *testing.T, retry time.Duration) (*accelerationGate, *ClientManager, *MetricsCollector) {
	t.Helper()

	cm := &ClientManager{
		client:             &s3.Client{},
		acceleratedClient:  &s3.Client{},
		standardClient:     &s3.Client{},
		accelerationActive: true,
		config:             &Config{UseAccelerate: true},
		logger:             slog.New(slog.DiscardHandler),
	}

	mc := NewMetricsCollector()
	mc.SetAccelerationEnabled(true)

	return newAccelerationGate(cm, mc, retry, slog.New(slog.DiscardHandler)), cm, mc
}

// accelErr is the error S3 returns for the common acceleration failure: an InvalidRequest whose
// message is the only thing distinguishing it from a bad Range or an oversized copy source.
func accelErr() error {
	return apiErr("InvalidRequest", "S3 Transfer Acceleration is not configured on this bucket")
}

// TestOneAccelerationErrorWithdrawsTheEndpoint is the first half of the latch — the half that was
// never in doubt. It is here because the second half is only meaningful if this one holds.
func TestOneAccelerationErrorWithdrawsTheEndpoint(t *testing.T) {
	t.Parallel()

	gate, cm, mc := gateFixture(t, time.Hour)

	err, attempted := gate.attempt(accelErr)
	if !attempted {
		t.Fatal("the first attempt through a fresh gate was refused; the gate starts closed")
	}
	if !isAccelerationError(err) {
		t.Fatalf("attempt returned %v, want the acceleration error fn produced", err)
	}

	// One error, not a threshold. An unusable accelerate endpoint is unusable for every request, so
	// sampling further only spends failed round trips to learn what the first one established.
	if _, attempted := gate.attempt(func() error { return nil }); attempted {
		t.Error("a second request was sent to the accelerate endpoint after an acceleration error; the " +
			"gate did not withdraw it")
	}

	if cm.IsAccelerationActive() {
		t.Error("the client manager still reports acceleration active, so requests keep being built " +
			"against an endpoint the gate has withdrawn")
	}
	if mc.GetMetrics().AccelerationEnabled {
		t.Error("BackendMetrics reports acceleration enabled while the gate holds it withdrawn; this is " +
			"the field that used to be a copy of the config flag, and reporting the flag is #204")
	}
}

// TestANonAccelerationErrorLeavesTheEndpointInService is the classifier's half of the decision, seen
// through the gate.
//
// A NoSuchKey or a 500 arriving over the accelerate endpoint is evidence the endpoint *works* — it
// reached S3, which answered. Counting those would withdraw acceleration for reasons that have
// nothing to do with acceleration, which is the false-positive direction that costs a mount its
// throughput for the backoff period on every unrelated error.
func TestANonAccelerationErrorLeavesTheEndpointInService(t *testing.T) {
	t.Parallel()

	gate, cm, _ := gateFixture(t, time.Hour)

	for _, err := range []error{
		apiErr("NoSuchKey", "The specified key does not exist"),
		apiErr("InternalError", "We encountered an internal error. Please try again."),
		apiErr("InvalidRequest", "Invalid Range header"),
		errors.New("connection timeout"),
	} {
		if _, attempted := gate.attempt(func() error { return err }); !attempted {
			t.Fatalf("the gate refused an attempt after %v; only an acceleration error should withdraw "+
				"the endpoint", err)
		}
	}

	if !cm.IsAccelerationActive() {
		t.Error("acceleration was withdrawn by errors that say nothing about the accelerate endpoint")
	}
}

// TestTheEndpointComesBackAfterTheBackoff is #204 itself.
//
// Verified by mutation: reverting executeWithAccelerationFallback to call DisableAcceleration
// directly — the shape this replaced — leaves this test failing at the recovery poll and every other
// acceleration test green.
func TestTheEndpointComesBackAfterTheBackoff(t *testing.T) {
	t.Parallel()

	const retry = 25 * time.Millisecond

	gate, cm, mc := gateFixture(t, retry)

	if _, attempted := gate.attempt(accelErr); !attempted {
		t.Fatal("the first attempt through a fresh gate was refused")
	}

	// Polled rather than slept-and-asserted: the assertion is that a probe becomes available, and a
	// single sleep of exactly the backoff races the breaker's own comparison.
	deadline := time.Now().Add(10 * time.Second)

	var probed bool
	for !probed {
		if time.Now().After(deadline) {
			t.Fatalf("no request reached the accelerate endpoint in 10s at a %v backoff; the fallback is "+
				"still one-way, which is #204", retry)
		}

		_, probed = gate.attempt(func() error { return nil })
		if !probed {
			time.Sleep(time.Millisecond)
		}
	}

	// The probe succeeded, so the gate closes and acceleration is in service again — not merely
	// permitted for one request.
	if _, attempted := gate.attempt(func() error { return nil }); !attempted {
		t.Error("the request after a successful probe was refused; the gate did not close, so acceleration " +
			"would be rationed to one request per backoff period forever")
	}

	if !cm.IsAccelerationActive() {
		t.Error("the client manager still reports acceleration inactive after recovery, so requests would " +
			"keep going to the standard endpoint while the gate believes the accelerate endpoint is in use")
	}
	if !mc.GetMetrics().AccelerationEnabled {
		t.Error("BackendMetrics reports acceleration disabled after recovery; an operator watching the " +
			"metric would not see the recovery, which is the observability half of #204")
	}
}

// TestAFailedProbeWithdrawsTheEndpointAgain pins the unrecoverable case.
//
// A bucket with no Transfer Acceleration configuration fails every probe forever, and the cost of the
// mechanism is bounded by exactly this: the failed probe must reopen the gate for another full backoff
// rather than leaving it half-open, where every subsequent request would pay a failed round trip.
func TestAFailedProbeWithdrawsTheEndpointAgain(t *testing.T) {
	t.Parallel()

	const retry = 25 * time.Millisecond

	gate, cm, _ := gateFixture(t, retry)

	if _, attempted := gate.attempt(accelErr); !attempted {
		t.Fatal("the first attempt through a fresh gate was refused")
	}

	deadline := time.Now().Add(10 * time.Second)

	var probed bool
	for !probed {
		if time.Now().After(deadline) {
			t.Fatalf("no probe reached the accelerate endpoint in 10s at a %v backoff", retry)
		}

		// The probe fails the same way the original request did, which is the real behavior of a
		// bucket that cannot be accelerated at all.
		_, probed = gate.attempt(accelErr)
		if !probed {
			time.Sleep(time.Millisecond)
		}
	}

	if _, attempted := gate.attempt(func() error { return nil }); attempted {
		t.Error("a request immediately after a failed probe was sent to the accelerate endpoint; the gate " +
			"stayed half-open, so every request on an unacceleratable bucket pays a failed round trip")
	}

	if cm.IsAccelerationActive() {
		t.Error("acceleration reports active after the probe failed")
	}
}

// TestOnlyOneProbeIsInFlightAtATime is the property that makes the retry affordable under load.
//
// A mount serving concurrent reads has dozens of requests arriving in the millisecond the backoff
// expires. Without MaxRequests: 1 every one of them is sent to an endpoint the gate has no reason yet
// to believe works, and on an unacceleratable bucket every one pays the failed round trip that only
// the first needed to pay — turning a bounded 288-failures-a-day cost into a per-request one.
//
// The first probe is held inside fn until the second attempt has been made, so the interleaving is a
// fixture rather than a race: a version of this test that launched two goroutines and hoped would pass
// against a gate with no cap at all.
func TestOnlyOneProbeIsInFlightAtATime(t *testing.T) {
	t.Parallel()

	const retry = 25 * time.Millisecond

	gate, _, _ := gateFixture(t, retry)

	if _, attempted := gate.attempt(accelErr); !attempted {
		t.Fatal("the first attempt through a fresh gate was refused")
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	probeAttempted := make(chan bool, 1)

	// Retried until the backoff has expired: before then the gate refuses without running fn, so
	// `entered` would never be closed and the test would deadlock on a condition that is simply not
	// yet true.
	go func() {
		deadline := time.Now().Add(10 * time.Second)

		for {
			_, attempted := gate.attempt(func() error {
				close(entered)
				<-release

				return nil
			})
			if attempted {
				probeAttempted <- true

				return
			}

			if time.Now().After(deadline) {
				probeAttempted <- false

				return
			}

			time.Sleep(time.Millisecond)
		}
	}()

	select {
	case <-entered:
	case ok := <-probeAttempted:
		t.Fatalf("no probe ran within 10s at a %v backoff (attempted=%v)", retry, ok)
	}

	// The probe is inside fn and has not reported. A second request must not be sent.
	if _, attempted := gate.attempt(func() error { return nil }); attempted {
		close(release)
		t.Fatal("a second request was sent to the accelerate endpoint while the half-open probe was still " +
			"in flight; under load that is one failed round trip per concurrent request rather than one")
	}

	close(release)

	if ok := <-probeAttempted; !ok {
		t.Fatal("the probe goroutine gave up")
	}
}

// TestAZeroRetryTakesTheDefault covers the value every existing configuration has.
//
// storage.s3.acceleration_retry is new, so every config file written before it — and every embedder
// building an s3.Config by hand — leaves it zero. Zero must mean the default and not "retry
// immediately", which would send one request per acceleration error to an endpoint that just failed;
// and a negative value must not reach circuit.Config.Timeout, where it would leave the breaker
// permanently half-open, the one state configuration must not be able to produce.
func TestAZeroRetryTakesTheDefault(t *testing.T) {
	t.Parallel()

	for _, retry := range []time.Duration{0, -time.Second} {
		gate, _, _ := gateFixture(t, retry)

		if got := gate.retryPeriod(); got != defaultAccelerationRetry {
			t.Errorf("a gate built with retry=%v reports a period of %v, want the default %v",
				retry, got, defaultAccelerationRetry)
		}

		// And the resulting breaker is genuinely open rather than half-open: one error, then no
		// further request.
		if _, attempted := gate.attempt(accelErr); !attempted {
			t.Fatalf("retry=%v: the first attempt through a fresh gate was refused", retry)
		}
		if _, attempted := gate.attempt(func() error { return nil }); attempted {
			t.Errorf("retry=%v: a request was sent immediately after an acceleration error, so the "+
				"backoff is zero", retry)
		}
	}
}
