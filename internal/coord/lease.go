package coord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"sync"
	"time"

	"github.com/scttfrdmn/objectfs/internal/vfs"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// Sentinel errors. Match with errors.Is.
var (
	// ErrNotHeld means this Lease does not currently hold the lease: it was never acquired, it was
	// released, or a renewal established that another holder has taken it.
	//
	// A guarded action that gets this must stop. It is the fail-closed answer, and the point of the
	// package is that it arrives before the action's writes rather than after them.
	ErrNotHeld = errors.New("coord: lease not held")

	// ErrHeldByAnother means the lease is currently held by someone else and was not acquired.
	//
	// This is a normal outcome of contention rather than a failure — it is how a contender learns it
	// lost — and callers should not log it as an error.
	ErrHeldByAnother = errors.New("coord: lease held by another holder")

	// ErrLost means the holder was still acting when the lease moved to another holder. It wraps
	// ErrNotHeld, so a caller that only distinguishes held from not-held does not have to know about
	// it.
	//
	// It is separate because it means something a caller may want to report differently: not "I could
	// not get the lease" but "I believed I had it and I was wrong", which is the partitioned-holder
	// case and usually worth an operator-visible message.
	ErrLost = fmt.Errorf("%w: it was taken by another holder while this one was acting", ErrNotHeld)
)

// unsupportedError explains a backend that cannot arbitrate, naming the endpoint's limitation.
type unsupportedError struct{ detail string }

func (e *unsupportedError) Error() string {
	if e.detail == "" {
		return "coord: this endpoint does not evaluate write preconditions, so it cannot arbitrate " +
			"between nodes; leases require conditional writes"
	}

	return "coord: this endpoint does not evaluate write preconditions, so it cannot arbitrate " +
		"between nodes: " + e.detail
}

func (e *unsupportedError) Unwrap() error { return types.ErrNotSupported }

// Config parameterizes a Lease.
type Config struct {
	// Key is the object the lease is held on. It must be per-resource: see the cost note in the
	// package doc for why one global lock key is the wrong shape.
	Key string

	// Holder identifies this contender in the stored record, for operator diagnosis of who holds
	// what. It has no role in arbitration — the store decides that — so a duplicate Holder across
	// nodes is a legibility problem, not a correctness one.
	Holder string

	// Period is how long an unrenewed lease must sit with an unchanged ETag before another contender
	// may take it over. It is a duration measured on one observer's monotonic clock, never a
	// comparison between two nodes' wall clocks.
	//
	// Renewing is the holder's job and RenewInterval must be comfortably shorter than this. Zero
	// means DefaultPeriod.
	Period time.Duration

	// RenewInterval is how often a Do-held lease re-asserts while an action runs. Zero means
	// Period/4, which leaves three missed renewals of margin before a takeover becomes possible.
	RenewInterval time.Duration

	// MaxAttempts bounds acquisition retries. One means try once and report contention rather than
	// waiting. Zero means DefaultMaxAttempts.
	//
	// It is bounded rather than optional because every lost race is a billed request.
	MaxAttempts int

	// Backoff is the base delay between acquisition attempts, jittered ±20%. Zero means
	// DefaultBackoff.
	Backoff time.Duration
}

// Defaults for the zero values in Config.
const (
	DefaultPeriod      = 30 * time.Second
	DefaultMaxAttempts = 3
	DefaultBackoff     = 250 * time.Millisecond
)

func (c *Config) withDefaults() {
	if c.Period <= 0 {
		c.Period = DefaultPeriod
	}
	if c.RenewInterval <= 0 {
		c.RenewInterval = c.Period / 4
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	if c.Backoff <= 0 {
		c.Backoff = DefaultBackoff
	}
}

func (c Config) validate() error {
	switch {
	case c.Key == "":
		return errors.New("coord: Config.Key is required; a lease is held on a specific object")
	case c.Holder == "":
		return errors.New("coord: Config.Holder is required, so an operator can tell who holds what")
	case c.RenewInterval >= c.Period:
		// A renewal interval at or beyond the period means the lease is takeable before its holder
		// would ever renew, which produces two holders under no contention at all.
		return fmt.Errorf("coord: RenewInterval %s must be shorter than Period %s, or the lease "+
			"expires before its holder renews it", c.RenewInterval, c.Period)
	default:
		return nil
	}
}

// record is what is stored at the lease key.
//
// Nonce is load-bearing rather than decorative: S3's ETag is the MD5 of the content, so a renewal
// writing an identical body would leave the ETag unchanged, and the takeover rule — an ETag that has
// not moved across a full period means nobody is renewing — would then read a live holder as an
// abandoned one. Every write of this record carries a fresh nonce so every write moves the ETag.
type record struct {
	Holder string `json:"holder"`
	Nonce  string `json:"nonce"`

	// Acquired and Renewed are the holder's own wall clock, recorded for operators reading the object
	// with `aws s3 cp`. Nothing in this package compares them against anything: see the clock section
	// of the package doc.
	Acquired time.Time `json:"acquired"`
	Renewed  time.Time `json:"renewed"`
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("coord: generating a lease nonce: %w", err)
	}

	return hex.EncodeToString(b[:]), nil
}

// Lease is mutual exclusion over one key, arbitrated by the object store.
//
// It has no exported method that performs a write on the caller's behalf. Acting under the lease goes
// through [Lease.Do], which re-asserts the CAS before running the action and hands it a [Guard] that
// re-asserts before each write. That shape is deliberate: an unguarded write under a lease is the
// defect this package exists to prevent, so it is not expressible through this type.
type Lease struct {
	backend types.Backend
	cfg     Config

	// now is the monotonic clock source, and sleep is the delay. Both are fields so tests can drive
	// takeover without waiting out a real period — the alternative is a test that sleeps for Period,
	// which is slow enough that nobody writes the interesting cases.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	// claimMu serializes the read-modify-write of the claim — read the held ETag, assert it, store the
	// new one — as one atomic step. mu alone is not enough: it makes each half safe and leaves the pair
	// racy, which is a bug this package cannot afford in the specific form it takes.
	//
	// Two renewals by the *same* holder both read the same ETag and both assert it. One wins; the other
	// gets ErrPreconditionFailed from its own holder's write and reads it as a takeover, so it returns
	// ErrLost and drops the claim — after which every guarded action fails closed on a lease the node
	// genuinely still holds. That is not a hypothetical concurrent caller: Do's renewal ticker and every
	// Guard method's re-assert both call Renew, so a holder doing exactly what this package tells it to
	// defeats itself under no contention at all. Reproduced 100% of the time with two concurrent
	// renewals before this existed.
	//
	// It is held across the round trip, which is the point — a lock released before the write leaves the
	// same window. The cost is that guarded writes inside one action serialize on their re-asserts,
	// which they must: a re-assert that could be skipped or shared is the guarantee this package sells.
	claimMu sync.Mutex

	mu   sync.Mutex
	etag string // the ETag this holder must assert; empty means not held
}

// New returns a Lease over cfg.Key, or an error if the backend's endpoint cannot arbitrate.
//
// The capability question is asked here rather than at first use so that a deployment against a store
// that ignores conditional headers fails while someone is watching, instead of at the first
// contention. A backend that cannot answer the question at all is treated as unable — see the
// fail-closed reasoning on [types.CapabilityReporter].
func New(backend types.Backend, cfg Config) (*Lease, error) {
	if backend == nil {
		return nil, errors.New("coord: backend is required")
	}

	// Defaults first, then validate. The other order rejects the zero value against its own defaults:
	// RenewInterval and Period are both 0, and 0 >= 0 trips the margin check before either has been
	// filled in.
	cfg.withDefaults()

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	reporter, ok := backend.(types.CapabilityReporter)
	if !ok {
		return nil, &unsupportedError{
			detail: "the backend does not report its capabilities, and an unverified conditional " +
				"write is treated as an absent one",
		}
	}

	if caps := reporter.Capabilities(); !caps.ConditionalWrite {
		return nil, &unsupportedError{detail: caps.ConditionalWriteDetail}
	}

	return &Lease{
		backend: backend,
		cfg:     cfg,
		now:     time.Now,
		sleep:   sleepCtx,
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Held reports whether this Lease believes it holds the lease.
//
// It is a local belief and therefore not a guard: a partitioned holder returns true here. It exists
// for diagnostics and for tests. Guarding an action with this instead of [Lease.Do] is the bug the
// package is shaped to prevent.
func (l *Lease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.etag != ""
}

// Acquire takes the lease, retrying a bounded number of times with jittered backoff while it is held
// by someone else.
//
// [ErrHeldByAnother] after the last attempt is a normal outcome of contention. An abandoned lease —
// one whose ETag has not moved across a full period — is taken over, which takes at least one period
// of observation by construction.
func (l *Lease) Acquire(ctx context.Context) error {
	// Acquire also establishes the claim, so it takes claimMu too: two Acquire calls on one Lease would
	// otherwise both write and the local ETag would be whichever finished last, which may not be the one
	// the store kept.
	l.claimMu.Lock()
	defer l.claimMu.Unlock()

	var lastETag string
	var stableSince time.Time

	for attempt := range l.cfg.MaxAttempts {
		if attempt > 0 {
			if err := l.sleep(ctx, l.jittered(l.cfg.Backoff)); err != nil {
				return fmt.Errorf("coord: waiting to retry acquisition of %q: %w", l.cfg.Key, err)
			}
		}

		etag, err := l.tryAcquire(ctx, types.Precondition{Absent: true})
		switch {
		case err == nil:
			l.setETag(etag)

			return nil
		case errors.Is(err, types.ErrPreconditionFailed):
			// Held by someone. Fall through to the takeover check below.
		default:
			return err
		}

		// Takeover, decided by an ETag that has not moved across a full period as measured on this
		// observer's own clock. Never by comparing a stored timestamp against a local one.
		info, err := l.backend.HeadObject(ctx, l.cfg.Key)
		if err != nil {
			// The lease vanished between the failed acquire and this read, or the read failed. Either
			// way the next attempt re-contends, which is the safe response.
			continue
		}

		if info.ETag != lastETag {
			lastETag, stableSince = info.ETag, l.now()

			continue
		}

		if l.now().Sub(stableSince) < l.cfg.Period {
			continue
		}

		etag, err = l.tryAcquire(ctx, types.Precondition{ETag: info.ETag})
		switch {
		case err == nil:
			l.setETag(etag)

			return nil
		case errors.Is(err, types.ErrPreconditionFailed):
			// The holder renewed, or another contender took over first. Both mean this observation
			// window is void, so start a fresh one rather than trying again against a dead ETag.
			lastETag, stableSince = "", time.Time{}
		default:
			return err
		}
	}

	return fmt.Errorf("coord: acquiring lease %q after %d attempt(s): %w",
		l.cfg.Key, l.cfg.MaxAttempts, ErrHeldByAnother)
}

// tryAcquire performs one conditional write of a fresh record, returning the new ETag.
func (l *Lease) tryAcquire(ctx context.Context, cond types.Precondition) (string, error) {
	body, err := l.marshal(record{Acquired: l.now(), Renewed: l.now()})
	if err != nil {
		return "", err
	}

	etag, err := l.backend.PutObjectIf(ctx, l.cfg.Key, body, nil, cond)
	if err != nil {
		// ErrPreconditionFailed is returned unwrapped in meaning but wrapped in context by the
		// backend; the caller matches with errors.Is. It is deliberately not retried here: the answer
		// is definitive and retrying spends a billed request to be told the same thing.
		return "", err
	}

	return etag, nil
}

// Renew re-asserts the lease, moving its ETag.
//
// A failed renewal means the lease is gone and the holder must stop acting: this returns [ErrLost]
// and drops the local claim, so every subsequent guarded action fails closed rather than proceeding
// on a stale belief.
func (l *Lease) Renew(ctx context.Context) error {
	// One holder's renewals must not race each other: see claimMu.
	l.claimMu.Lock()
	defer l.claimMu.Unlock()

	held := l.currentETag()
	if held == "" {
		return fmt.Errorf("coord: renewing lease %q: %w", l.cfg.Key, ErrNotHeld)
	}

	body, err := l.marshal(record{Acquired: l.now(), Renewed: l.now()})
	if err != nil {
		return err
	}

	etag, err := l.backend.PutObjectIf(ctx, l.cfg.Key, body, nil, types.Precondition{ETag: held})
	if err != nil {
		// A precondition failure means someone else's record is there now. A 404 means the object is
		// gone, which is equally a lost lease — a holder whose lease object was deleted has no claim
		// to re-assert. Both drop the claim; anything else leaves it alone, because a transient
		// network error is not evidence the lease moved.
		//
		// vfs.IsNotFound rather than a second classifier here: it already carries the reasoning about
		// which direction is dangerous, and a duplicate would be one more place to get it wrong.
		if errors.Is(err, types.ErrPreconditionFailed) || vfs.IsNotFound(err) {
			l.setETag("")

			return fmt.Errorf("coord: renewing lease %q: %w", l.cfg.Key, ErrLost)
		}

		return fmt.Errorf("coord: renewing lease %q: %w", l.cfg.Key, err)
	}

	l.setETag(etag)

	return nil
}

// Release gives up the lease.
//
// The lease object is left in place rather than deleted, because there is no conditional delete: the
// Backend interface has no DeleteObjectIf, and an unconditional delete by a holder that has since
// lost the lease would remove the *current* holder's record and hand the lease to whoever raced next.
// Leaving a released record costs one period of takeover delay for the next contender, which is the
// cheaper failure.
//
// Release is idempotent and safe to call on a lease that was never held.
func (l *Lease) Release(ctx context.Context) error {
	// Same claim, same serialization: a release racing a renewal would otherwise leave the local ETag
	// set by whichever finished last.
	l.claimMu.Lock()
	defer l.claimMu.Unlock()

	held := l.currentETag()
	if held == "" {
		return nil
	}

	// Mark the record released so an operator reading the object can tell a released lease from a
	// crashed holder, and so the write moves the ETag one last time. A failure here is not fatal: the
	// local claim is dropped either way, and the record ages out by the takeover rule.
	body, err := l.marshal(record{Holder: l.cfg.Holder + " (released)", Acquired: l.now(), Renewed: l.now()})
	if err != nil {
		l.setETag("")

		return err
	}

	_, err = l.backend.PutObjectIf(ctx, l.cfg.Key, body, nil, types.Precondition{ETag: held})
	l.setETag("")

	if err != nil && !errors.Is(err, types.ErrPreconditionFailed) && !vfs.IsNotFound(err) {
		return fmt.Errorf("coord: releasing lease %q: %w", l.cfg.Key, err)
	}

	return nil
}

// Guard is the only way to write under a lease. Each of its methods re-asserts the lease immediately
// before the write it performs, so a holder that has lost the lease fails at the store rather than at
// a local timer check.
//
// It is valid only for the duration of the [Lease.Do] call that produced it. Retaining one past that
// is pointless rather than dangerous: it re-asserts against the same Lease, so it fails closed once
// the lease is released.
type Guard struct {
	lease *Lease
	ctx   context.Context //nolint:containedctx // Do's ctx, so a guarded write cannot outlive the action
}

// Put writes an object, re-asserting the lease first.
//
// The re-assert is the point. Removing it leaves a write that a partitioned holder performs
// successfully, which is the defect the package exists to prevent, and
// TestGuardedWriteFailsAfterTheLeaseIsStolen is what catches its removal.
func (g Guard) Put(key string, data []byte, meta map[string]string) error {
	if err := g.lease.Renew(g.ctx); err != nil {
		return err
	}

	return g.lease.backend.PutObject(g.ctx, key, data, meta)
}

// PutIf writes an object conditionally, re-asserting the lease first.
//
// This is the form to prefer when correctness cannot tolerate the residual window described in the
// package doc: a precondition on state only the current holder could have set is rejected by the
// store itself, which the lease re-assert cannot be for a write to a different key.
func (g Guard) PutIf(key string, data []byte, meta map[string]string, cond types.Precondition) (string, error) {
	if err := g.lease.Renew(g.ctx); err != nil {
		return "", err
	}

	return g.lease.backend.PutObjectIf(g.ctx, key, data, meta, cond)
}

// Delete removes an object, re-asserting the lease first.
func (g Guard) Delete(key string) error {
	if err := g.lease.Renew(g.ctx); err != nil {
		return err
	}

	return g.lease.backend.DeleteObject(g.ctx, key)
}

// Do runs action while holding the lease, re-asserting before it starts and periodically while it
// runs, and canceling action's context the moment the lease is lost.
//
// The lease must already be held: Do does not acquire, so a caller cannot accidentally treat "I got
// the lease just now" and "I have held it for a while" as the same thing.
//
// Losing the lease mid-action cancels action's context and returns [ErrLost] — joined with whatever
// action itself returned, since an action that notices its canceled context has something to say
// about how far it got.
func (l *Lease) Do(ctx context.Context, action func(context.Context, Guard) error) error {
	if action == nil {
		return errors.New("coord: Do requires an action")
	}

	// Re-assert before starting. A holder that lost the lease while idle must not get as far as the
	// action's first line.
	if err := l.Renew(ctx); err != nil {
		return err
	}

	actionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		renewMu  sync.Mutex
		renewErr error
	)

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(l.cfg.RenewInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-actionCtx.Done():
				return
			case <-ticker.C:
				// Renew on ctx rather than actionCtx: canceling the action is this goroutine's own
				// response to a failure, and renewing on a context it cancels would make the first
				// cancellation look like a second renewal failure.
				if err := l.Renew(ctx); err != nil {
					renewMu.Lock()
					renewErr = err
					renewMu.Unlock()
					cancel()

					return
				}
			}
		}
	}()

	actionErr := action(actionCtx, Guard{lease: l, ctx: actionCtx})

	close(stop)
	<-done

	renewMu.Lock()
	defer renewMu.Unlock()

	if renewErr != nil {
		return errors.Join(renewErr, actionErr)
	}

	return actionErr
}

func (l *Lease) marshal(r record) ([]byte, error) {
	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}

	if r.Holder == "" {
		r.Holder = l.cfg.Holder
	}
	r.Nonce = nonce

	body, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("coord: encoding the lease record for %q: %w", l.cfg.Key, err)
	}

	return body, nil
}

func (l *Lease) currentETag() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.etag
}

func (l *Lease) setETag(etag string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.etag = etag
}

// jittered spreads d by ±20% so contenders that lost the same race do not retry in lockstep.
func (l *Lease) jittered(d time.Duration) time.Duration {
	// #nosec G404 -- backoff spread, not a secret
	spread := 1 + 0.4*(mathrand.Float64()-0.5)

	return time.Duration(float64(d) * spread)
}
