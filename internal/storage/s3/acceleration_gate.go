package s3

import (
	"log/slog"
	"time"

	"github.com/scttfrdmn/objectfs/internal/circuit"
)

// defaultAccelerationRetry is how long the accelerate endpoint stays out of service after a
// fallback, when Config.AccelerationRetry says nothing.
//
// Five minutes is chosen against the unrecoverable case, because that is the one that pays for the
// probe rather than benefiting from it: a bucket with no Transfer Acceleration configuration will
// fail one request per period forever. At 5m that is 288 failed requests a day, each of which
// immediately succeeds on the standard endpoint — invisible against any mount whose traffic
// justified turning acceleration on, and a bounded cost rather than the unbounded one a per-request
// retry would be.
//
// The recoverable case wants it shorter and tolerates this: a mount that lost acceleration to a
// transient DNS failure gets it back within five minutes instead of at the next restart, which was
// the previous answer.
const defaultAccelerationRetry = 5 * time.Minute

// accelerationGate decides whether a request may use the accelerate endpoint.
//
// # Why a circuit breaker and not a timestamp
//
// The shape wanted here — a capability that is withdrawn on failure, restored after a delay, and
// withdrawn again if the one probe that tests it fails — is a circuit breaker's half-open cycle
// exactly. [circuit.CircuitBreaker] already implements it, is already imported by this package for
// the operation breakers, and has its own tests for the parts that are easy to get subtly wrong:
// MaxRequests caps how many probes run at once, so a burst of concurrent requests at the moment the
// window opens sends *one* to the accelerate endpoint rather than all of them, and the trial ends on
// the first result rather than on a second timer.
//
// A bespoke `lastFailure time.Time` plus a comparison would have been about six lines, and every one
// of those properties would have been absent from it. #204 said as much when it was filed, and it is
// the reason this reuses rather than reimplements.
//
// # The mapping onto the breaker's vocabulary
//
// One "request" through this breaker is one *acceleration attempt*, and the only failure it counts is
// an acceleration error — [Backend.isAccelerationError]. Everything else is a success as far as this
// breaker is concerned, including a NoSuchKey and a 500: those say nothing about whether the
// accelerate endpoint is usable, and counting them would withdraw acceleration for reasons that have
// nothing to do with it. That is the same distinction circuit.defaultIsSuccessful draws for the
// operation breakers, drawn against a different question.
//
// ReadyToTrip is `ConsecutiveFailures >= 1` rather than a proportional default, and deliberately:
// unlike a flaky service, an unusable accelerate endpoint is unusable for every request, so there is
// no threshold to reach and nothing to be gained by sampling further. One acceleration error is the
// whole evidence there is going to be.
type accelerationGate struct {
	breaker *circuit.CircuitBreaker

	// retry is the resolved backoff, kept because circuit.Config is not readable back off the breaker
	// and AccelerationStats reports it: an operator looking at a mount in fallback wants to know when
	// it will try again, and the configured value may be the default rather than anything they set.
	retry time.Duration
}

// newAccelerationGate builds the gate and wires its state transitions to the client manager.
//
// The wiring direction matters. The breaker is the authority on whether acceleration is available,
// and cm.accelerationActive is a projection of it — kept in step through OnStateChange rather than
// written by whoever noticed the error. That is what makes the recovery observable: the same hook
// that flips the client also logs the transition and is where a metric hangs off, so an operator
// sees "acceleration withdrawn" and "acceleration restored" as events rather than having to infer
// them from a throughput graph. Before this, nothing anywhere reported that acceleration was off.
//
// retry of zero takes defaultAccelerationRetry; a negative value takes it too, since a negative
// timeout would put the breaker permanently half-open, which is the one state that must not be
// reachable from configuration.
func newAccelerationGate(
	cm *ClientManager,
	mc *MetricsCollector,
	retry time.Duration,
	logger *slog.Logger,
) *accelerationGate {
	if retry <= 0 {
		retry = defaultAccelerationRetry
	}

	return &accelerationGate{
		retry: retry,
		breaker: circuit.NewCircuitBreaker("s3-acceleration", circuit.Config{
			// One probe when the window opens, not one per in-flight request. A mount under load has
			// dozens of requests arriving in the millisecond the breaker half-opens, and every one of
			// them would otherwise pay the failed round trip that only the first needed to pay.
			MaxRequests: 1,

			// Interval clears the closed-state counts periodically, which for this breaker only means
			// forgetting successes it has no use for. It is set well above Timeout so it cannot
			// interact with the trip decision, which is about consecutive failures.
			Interval: time.Hour,
			Timeout:  retry,

			ReadyToTrip: func(counts circuit.Counts) bool {
				return counts.ConsecutiveFailures >= 1
			},

			// Only an acceleration error is a failure here. See the type comment: a missing object or
			// an S3 500 arriving over the accelerate endpoint is not evidence about the endpoint.
			IsSuccessful: func(err error) bool {
				return !isAccelerationError(err)
			},

			// Every state change is applied here and reported here, which is the "surface the state"
			// half of #204. It must not call back into the breaker: this runs under the breaker's own
			// lock, and both ClientManager methods take only cm.accelMu.
			OnStateChange: func(name string, from, to circuit.State) {
				switch to {
				case circuit.StateOpen:
					cm.DisableAcceleration("acceleration error; retrying in " + retry.String())
					mc.SetAccelerationEnabled(false)

				case circuit.StateHalfOpen:
					cm.EnableAcceleration()
					mc.SetAccelerationEnabled(true)
					logger.Info("Retrying S3 Transfer Acceleration after backoff",
						"breaker", name,
						"backoff", retry)

				case circuit.StateClosed:
					// Reached from half-open on a probe that did not fail — acceleration works again.
					// EnableAcceleration already ran on the half-open transition, so this is the
					// report rather than the change.
					if from == circuit.StateHalfOpen {
						logger.Info("S3 Transfer Acceleration restored", "breaker", name)
					}
				}
			},
		}),
	}
}

// attempt runs fn against the accelerate endpoint if the gate permits it, and reports whether it ran.
//
// attempted false means the gate is open or its single half-open probe slot is taken, so the caller
// must use the standard endpoint; err is nil in that case and nothing has been sent. attempted true
// means fn ran and err is its result, already counted — an acceleration error has tripped the gate by
// the time this returns, so the caller's job is only to retry on the standard endpoint.
//
// Going through [circuit.CircuitBreaker.Execute] rather than a permission/report pair is what keeps
// the accounting correct without a rule for the caller to follow. Execute counts the request in and
// the result out under the breaker's own lock; a caller that took permission and forgot to report
// would hold the half-open slot forever, and MaxRequests is 1, so that is every later probe blocked
// for the life of the mount.
//
// The distinction is drawn by observing whether fn ran, not by classifying the returned error, and that
// is not fussiness. Execute reports its own refusal as [circuit.ErrOpenState] or
// [circuit.ErrTooManyRequests], and testing for those would be indistinguishable from fn returning one
// — which an S3 call wrapped in the operation breaker can do. Misreading that as "the gate said no"
// sends a second request for an answer the caller already has.
func (g *accelerationGate) attempt(fn func() error) (err error, attempted bool) {
	var fnErr error

	// The gate's own verdict is discarded: when fn ran, its error is fn's, and when it did not, the
	// verdict is exactly what `attempted` reports.
	_ = g.breaker.Execute(func() error {
		attempted = true
		fnErr = fn()

		return fnErr
	})

	if !attempted {
		return nil, false
	}

	return fnErr, true
}

// state reports the gate's state, for metrics and for tests.
//
// It calls through to the breaker rather than caching, because reading the state is also what
// advances it: circuit.CircuitBreaker.GetState performs the open→half-open transition when the
// timeout has passed. A gate nothing asks about therefore stays open until the next request, which is
// correct — there is nothing to recover for — and a mount that scrapes metrics recovers on the scrape
// rather than on the next request, which is harmless and slightly earlier.
func (g *accelerationGate) state() circuit.State {
	return g.breaker.GetState()
}

// retryPeriod is the resolved backoff — the configured value, or defaultAccelerationRetry when the
// configuration named none.
func (g *accelerationGate) retryPeriod() time.Duration {
	return g.retry
}
