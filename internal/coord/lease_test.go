package coord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// leaseFor returns a Lease over the shared endpoint, with a short period so takeover tests do not
// wait out a real one.
func leaseFor(t *testing.T, ts *testaws.TestServer, holder string, mutate ...func(*Config)) *Lease {
	t.Helper()

	cfg := Config{
		Key:           "leases/resource",
		Holder:        holder,
		Period:        200 * time.Millisecond,
		RenewInterval: 20 * time.Millisecond,
		MaxAttempts:   1,
		Backoff:       time.Millisecond,
	}
	for _, m := range mutate {
		m(&cfg)
	}

	l, err := New(ts.Backend(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return l
}

// TestGuardedWriteFailsAfterTheLeaseIsStolen is the load-bearing test of this package.
//
// It is the partitioned-holder case: a node that believes it holds a lease it has actually lost must
// not be able to write. The steal here stands in for the partition — from the first holder's point of
// view the two are identical, since neither involves it being told anything.
//
// Removing the Renew from Guard.Put makes this test fail, which is the property most likely to be
// quietly regressed later: the write would succeed and nothing local would object.
func TestGuardedWriteFailsAfterTheLeaseIsStolen(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	first := leaseFor(t, ts, "first")
	if err := first.Acquire(ctx); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	// The steal: a second contender writes the lease key with the ETag the first holder is asserting,
	// which is what a takeover after expiry does. The first holder is not informed — that is the point.
	stealLease(t, ts, first.cfg.Key)

	const guarded = "guarded/output"

	err := first.Do(ctx, func(_ context.Context, g Guard) error {
		return g.Put(guarded, []byte("written by a holder that no longer holds"), nil)
	})

	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Do after the lease was stolen = %v, want an error matching ErrNotHeld", err)
	}
	if !errors.Is(err, ErrLost) {
		t.Errorf("Do after the lease was stolen = %v, want it to match ErrLost specifically", err)
	}

	// The assertion that makes this about data rather than about return values: the guarded write must
	// not have reached the store.
	if ts.ObjectExists(guarded) {
		t.Errorf("a holder that had lost the lease still wrote %q", guarded)
	}
}

// TestGuardedWriteFailsWhenTheLeaseIsStolenMidAction covers the harder timing: the lease is valid
// when Do starts and is stolen while the action is running. The re-assert inside Guard.Put is the only
// thing standing between that and a write, since Do's initial check has already passed.
func TestGuardedWriteFailsWhenTheLeaseIsStolenMidAction(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	first := leaseFor(t, ts, "first")
	if err := first.Acquire(ctx); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	const (
		before = "guarded/before"
		after  = "guarded/after"
	)

	err := first.Do(ctx, func(_ context.Context, g Guard) error {
		// A write while genuinely holding the lease succeeds.
		if err := g.Put(before, []byte("legitimate"), nil); err != nil {
			return fmt.Errorf("first write: %w", err)
		}

		// Now the lease moves. Inside a Do, so the steal is serialized against this holder's renewal
		// ticker — see stealLeaseFrom.
		stealLeaseFrom(t, ts, first, first.cfg.Key)

		// And the same call that worked a moment ago must now fail.
		if err := g.Put(after, []byte("must not land"), nil); err != nil {
			return err
		}

		return errors.New("the guarded write succeeded after the lease was stolen")
	})

	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("action error = %v, want an error matching ErrNotHeld", err)
	}

	if !ts.ObjectExists(before) {
		t.Errorf("the write performed while genuinely holding the lease did not land")
	}
	if ts.ObjectExists(after) {
		t.Errorf("the write performed after the lease was stolen landed anyway")
	}
}

// TestGuardedDeleteAndPutIfAlsoReassert checks the other two Guard methods, so a re-assert removed
// from one of them is not covered only by Put's test.
//
// The steal happens *inside* the action rather than before Do, and that detail is the whole value of
// the test. Stealing first means Do's own opening Renew rejects the action before it starts, so the
// method under test is never reached and deleting its re-assert changes nothing — verified by
// mutation, which is how the weaker version of this test was caught.
func TestGuardedDeleteAndPutIfAlsoReassert(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		act  func(Guard) error
	}{
		{"Delete", func(g Guard) error { return g.Delete("victim") }},
		{"PutIf", func(g Guard) error {
			_, err := g.PutIf("target", []byte("x"), nil, types.Precondition{Absent: true})

			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			ctx := context.Background()
			ts.PutObject("victim", []byte("should survive a stale holder's delete"))

			l := leaseFor(t, ts, "holder")
			if err := l.Acquire(ctx); err != nil {
				t.Fatalf("Acquire: %v", err)
			}

			err := l.Do(ctx, func(_ context.Context, g Guard) error {
				stealLeaseFrom(t, ts, l, l.cfg.Key)

				if err := tc.act(g); err != nil {
					return err
				}

				return fmt.Errorf("%s succeeded after the lease was stolen", tc.name)
			})
			if !errors.Is(err, ErrNotHeld) {
				t.Fatalf("%s after the lease was stolen = %v, want ErrNotHeld", tc.name, err)
			}

			if !ts.ObjectExists("victim") {
				t.Error("a stale holder's Delete removed the object")
			}
			if ts.ObjectExists("target") {
				t.Error("a stale holder's PutIf wrote the object")
			}
		})
	}
}

// TestGuardedWritesReachTheStoreWhileTheLeaseIsHeld is the other half of the two tests above, and it
// is not redundant with them: they assert that a stale holder's writes do *not* land, which a Guard
// method whose body were deleted entirely would also satisfy. Without this, Delete and PutIf are
// covered only on the path where they refuse.
func TestGuardedWritesReachTheStoreWhileTheLeaseIsHeld(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()
	ts.PutObject("doomed", []byte("to be deleted under the lease"))

	l := leaseFor(t, ts, "holder")
	if err := l.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	var putIfETag string

	if err := l.Do(ctx, func(_ context.Context, g Guard) error {
		if err := g.Put("plain", []byte("via Put"), map[string]string{"who": "holder"}); err != nil {
			return fmt.Errorf("Put: %w", err)
		}
		if err := g.Delete("doomed"); err != nil {
			return fmt.Errorf("Delete: %w", err)
		}

		etag, err := g.PutIf("conditional", []byte("via PutIf"), nil, types.Precondition{Absent: true})
		if err != nil {
			return fmt.Errorf("PutIf: %w", err)
		}
		putIfETag = etag

		// The precondition must genuinely be evaluated on the guarded write, not merely accepted: a
		// second absent-assertion against the key just written has to lose.
		if _, err := g.PutIf("conditional", []byte("second"), nil,
			types.Precondition{Absent: true}); !errors.Is(err, types.ErrPreconditionFailed) {
			return fmt.Errorf("second PutIf asserting absence = %v, want ErrPreconditionFailed", err)
		}

		return nil
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}

	// Assert the bytes, not just that the calls returned nil — a Guard method that renewed and then
	// wrote to the wrong key would satisfy a nil-error check.
	for _, tc := range []struct{ key, want string }{
		{"plain", "via Put"},
		{"conditional", "via PutIf"},
	} {
		got, err := ts.Backend().GetObject(ctx, tc.key, 0, -1)
		if err != nil {
			t.Errorf("GetObject(%q): %v", tc.key, err)

			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}

	if ts.ObjectExists("doomed") {
		t.Error("a guarded Delete by the current holder did not remove the object")
	}
	if putIfETag == "" {
		t.Error("a guarded PutIf returned no ETag, so a caller has nothing to chain a later " +
			"precondition on")
	}
}

// TestOneHoldersConcurrentRenewalsDoNotDefeatEachOther is a regression test for a defect this package
// had, and the defect was in the production path rather than in a test.
//
// Renew reads the held ETag, asserts it, and stores the new one. With those three steps not atomic,
// two renewals by the *same* holder both read the same ETag and both assert it: one wins, and the
// other is handed ErrPreconditionFailed by its own holder's write. Renew cannot tell that from a
// takeover, so it returned ErrLost and dropped the claim — after which every guarded action fails
// closed on a lease the node still genuinely holds.
//
// This is not a test of a caller doing something unusual. Lease.Do's renewal ticker and every Guard
// method's re-assert both call Renew, so a holder following this package's only supported pattern
// defeated itself under no contention at all. Reproduced 100% of the time with two goroutines.
//
// Losing the claim while still holding it is the fail-*open* direction's mirror image: nothing is
// corrupted, but a node that holds a lease and cannot act on it stalls, and the operator-visible
// message says it was taken by another holder, which is false.
func TestOneHoldersConcurrentRenewalsDoNotDefeatEachOther(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	l := leaseFor(t, ts, "holder")
	if err := l.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	const renewals = 8

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []error
	)

	for range renewals {
		wg.Go(func() {
			if err := l.Renew(ctx); err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	for _, err := range failures {
		t.Errorf("a renewal by the sole holder failed: %v", err)
	}

	// And the holder still holds it, which is the consequence that matters: a dropped claim is not
	// merely a returned error, it disables every subsequent guarded action.
	if !l.Held() {
		t.Fatal("the sole holder dropped its claim after renewing concurrently with itself")
	}
	if err := l.Do(ctx, func(_ context.Context, g Guard) error {
		return g.Put("after", []byte("still held"), nil)
	}); err != nil {
		t.Errorf("Do after concurrent self-renewal = %v, want the lease to still be usable", err)
	}
	if !ts.ObjectExists("after") {
		t.Error("the holder could not write after renewing concurrently with itself")
	}
}

// TestExactlyOneHolderUnderContention asserts mutual exclusion with a counter a second holder would
// trip, rather than by inspecting return values — a bug that let two contenders both believe they won
// would satisfy any check made only of what Acquire returned.
func TestExactlyOneHolderUnderContention(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	const contenders = 8

	var (
		inside     atomic.Int32 // how many are inside the critical section right now
		maxInside  atomic.Int32
		acquired   atomic.Int32
		lost       atomic.Int32
		start      sync.WaitGroup
		done       sync.WaitGroup
		mu         sync.Mutex
		unexpected []error
	)

	start.Add(1)
	for i := range contenders {
		done.Go(func() {
			l := leaseFor(t, ts, fmt.Sprintf("node-%d", i))

			start.Wait()

			if err := l.Acquire(ctx); err != nil {
				if errors.Is(err, ErrHeldByAnother) {
					lost.Add(1)
				} else {
					mu.Lock()
					unexpected = append(unexpected, err)
					mu.Unlock()
				}

				return
			}

			acquired.Add(1)

			err := l.Do(ctx, func(_ context.Context, _ Guard) error {
				n := inside.Add(1)
				defer inside.Add(-1)

				for {
					m := maxInside.Load()
					if n <= m || maxInside.CompareAndSwap(m, n) {
						break
					}
				}

				// Hold it long enough that a second holder would overlap rather than merely follow.
				time.Sleep(30 * time.Millisecond)

				return nil
			})
			if err != nil {
				mu.Lock()
				unexpected = append(unexpected, fmt.Errorf("Do: %w", err))
				mu.Unlock()
			}

			if err := l.Release(ctx); err != nil {
				mu.Lock()
				unexpected = append(unexpected, fmt.Errorf("Release: %w", err))
				mu.Unlock()
			}
		})
	}

	start.Done()
	done.Wait()

	if len(unexpected) > 0 {
		t.Fatalf("contenders failed for reasons other than losing the race: %v", unexpected)
	}
	if got := maxInside.Load(); got != 1 {
		t.Errorf("%d contenders were inside the critical section simultaneously, want 1", got)
	}
	if got := acquired.Load(); got != 1 {
		t.Errorf("acquired = %d, want exactly 1 (lost = %d)", got, lost.Load())
	}
}

// TestAbandonedLeaseIsTakenOverOnlyAfterAFullPeriod covers both halves of the takeover rule: a
// contender must not take a lease whose ETag is still moving, and must take one whose ETag has been
// stable for a period.
func TestAbandonedLeaseIsTakenOverOnlyAfterAFullPeriod(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	first := leaseFor(t, ts, "first")
	if err := first.Acquire(ctx); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	// A contender that observes the lease for less than a period must not take it, however many
	// attempts it makes.
	impatient := leaseFor(t, ts, "impatient", func(c *Config) {
		c.MaxAttempts = 4
		c.Backoff = time.Millisecond
		c.Period = time.Hour // never satisfied within the test
	})
	if err := impatient.Acquire(ctx); !errors.Is(err, ErrHeldByAnother) {
		t.Fatalf("impatient Acquire = %v, want ErrHeldByAnother", err)
	}
	if impatient.Held() {
		t.Fatal("a contender took over a lease before a full period of observation")
	}

	// The first holder stops renewing. A contender that observes a stable ETag across its period takes
	// over. Period here is short and MaxAttempts high enough to cover two observation windows.
	patient := leaseFor(t, ts, "patient", func(c *Config) {
		c.Period = 40 * time.Millisecond
		c.RenewInterval = 5 * time.Millisecond
		c.MaxAttempts = 12
		c.Backoff = 15 * time.Millisecond
	})
	if err := patient.Acquire(ctx); err != nil {
		t.Fatalf("patient Acquire after the holder stopped renewing: %v", err)
	}

	// And the abandoned holder is now the one that cannot act.
	if err := first.Renew(ctx); !errors.Is(err, ErrLost) {
		t.Errorf("the abandoned holder's Renew = %v, want ErrLost", err)
	}
}

// TestARenewingHolderKeepsTheLease is the complement: the takeover rule must not fire against a
// holder that is doing its job. A renewal that failed to move the ETag would break this — which is the
// content-addressed-ETag trap the nonce exists for.
func TestARenewingHolderKeepsTheLease(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	holder := leaseFor(t, ts, "holder")
	if err := holder.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Do renews on its own ticker while the action runs. A contender with the same period as the
	// holder's renewal margin must never see a stable ETag.
	err := holder.Do(ctx, func(_ context.Context, _ Guard) error {
		contender := leaseFor(t, ts, "contender", func(c *Config) {
			c.Period = 60 * time.Millisecond
			c.MaxAttempts = 6
			c.Backoff = 20 * time.Millisecond
		})

		if err := contender.Acquire(ctx); !errors.Is(err, ErrHeldByAnother) {
			return fmt.Errorf("contender Acquire against a renewing holder = %w, want ErrHeldByAnother", err)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if !holder.Held() {
		t.Error("the holder lost its lease while renewing")
	}
}

// TestEveryWriteMovesTheETag is the direct test of the nonce, and of the fact it is a response to:
// S3's ETag is content-addressed, so a lease record written twice with the same fields would keep the
// same ETag and a live holder would look abandoned.
//
// The clock is pinned, which is what makes this a test of the nonce rather than of the timestamps. The
// record also carries Acquired and Renewed at nanosecond resolution, so with a live clock the body
// varies whatever the nonce does — the first version of this test passed with the nonce replaced by a
// constant, verified by mutation. Those timestamps are documented as being for operators reading the
// object, so ETag movement must not depend on them: rounding them or dropping them should not be able
// to break the takeover rule.
func TestEveryWriteMovesTheETag(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	l := leaseFor(t, ts, "holder")

	frozen := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return frozen }

	if err := l.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	seen := map[string]bool{l.currentETag(): true}

	for i := range 4 {
		if err := l.Renew(ctx); err != nil {
			t.Fatalf("Renew %d: %v", i, err)
		}

		etag := l.currentETag()
		if seen[etag] {
			t.Fatalf("renewal %d reused ETag %s; a renewal that does not move the ETag makes a live "+
				"holder indistinguishable from an abandoned one", i, etag)
		}
		seen[etag] = true
	}
}

// TestReleaseByAStaleHolderDoesNotDisturbTheCurrentOne. The delete-based release this package
// deliberately does not have is what would break here: an unconditional delete by the stale holder
// would remove the current holder's record.
func TestReleaseByAStaleHolderDoesNotDisturbTheCurrentOne(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	stale := leaseFor(t, ts, "stale")
	if err := stale.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Someone takes over.
	stolen := currentLeaseETag(t, ts, stale.cfg.Key)
	currentBody := []byte(`{"holder":"current"}`)
	if _, err := ts.Backend().PutObjectIf(ctx, stale.cfg.Key, currentBody, nil,
		types.Precondition{ETag: stolen}); err != nil {
		t.Fatalf("stealing: %v", err)
	}
	afterSteal := currentLeaseETag(t, ts, stale.cfg.Key)

	// The stale holder releases. It must neither delete nor overwrite the current record.
	if err := stale.Release(ctx); err != nil {
		t.Errorf("Release by a stale holder returned %v, want nil: losing a race on release is normal", err)
	}

	if !ts.ObjectExists(stale.cfg.Key) {
		t.Fatal("a stale holder's Release deleted the current holder's lease")
	}
	if got := currentLeaseETag(t, ts, stale.cfg.Key); got != afterSteal {
		t.Errorf("a stale holder's Release modified the current holder's record (etag %s -> %s)", afterSteal, got)
	}
	if got := string(ts.GetObject(stale.cfg.Key)); got != string(currentBody) {
		t.Errorf("lease record = %q, want the current holder's %q", got, currentBody)
	}
}

// TestReleaseAllowsImmediateReacquisition. A released lease should not cost the next contender a full
// takeover period — released records are marked, and the release moves the ETag, so the next acquirer
// still has to observe stability. This documents the actual behavior rather than asserting a
// convenience the design does not provide.
func TestReleaseAllowsReacquisitionByTheSameHolder(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	l := leaseFor(t, ts, "holder", func(c *Config) {
		c.Period = 40 * time.Millisecond
		c.MaxAttempts = 12
		c.Backoff = 15 * time.Millisecond
	})

	if err := l.Acquire(ctx); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if l.Held() {
		t.Fatal("Held after Release")
	}

	// Release is idempotent.
	if err := l.Release(ctx); err != nil {
		t.Errorf("second Release: %v", err)
	}

	// Re-acquisition goes through the takeover path, because the record is still there.
	if err := l.Acquire(ctx); err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
	if !l.Held() {
		t.Error("re-Acquire reported success but Held is false")
	}
}

// TestDoRefusesALeaseThatWasNeverAcquired. Do does not acquire, so this must fail closed rather than
// silently taking the lease.
func TestDoRefusesALeaseThatWasNeverAcquired(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	l := leaseFor(t, ts, "holder")

	err := l.Do(context.Background(), func(_ context.Context, g Guard) error {
		return g.Put("should-not-exist", []byte("x"), nil)
	})
	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Do without Acquire = %v, want ErrNotHeld", err)
	}
	if ts.ObjectExists("should-not-exist") {
		t.Error("an action ran under a lease that was never acquired")
	}
}

// TestDoCancelsTheActionWhenTheLeaseIsLost. The renewal ticker must cancel the action's context, so a
// long-running action cooperating with its context stops rather than running to completion under a
// lease it no longer holds.
func TestDoCancelsTheActionWhenTheLeaseIsLost(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	l := leaseFor(t, ts, "holder", func(c *Config) {
		c.Period = 200 * time.Millisecond
		c.RenewInterval = 10 * time.Millisecond
	})
	if err := l.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	var sawCancel bool

	err := l.Do(ctx, func(actionCtx context.Context, _ Guard) error {
		// Steal the lease, then wait for the renewal ticker to notice. The steal races that ticker for
		// the lease object's ETag, which is why it goes through stealLeaseFrom rather than a single
		// conditional write — see the note there. This is the test whose steal failed on CI.
		stealLeaseFrom(t, ts, l, l.cfg.Key)

		select {
		case <-actionCtx.Done():
			sawCancel = true

			return actionCtx.Err()
		case <-time.After(2 * time.Second):
			return errors.New("the action's context was never canceled after the lease was lost")
		}
	})

	if !sawCancel {
		t.Errorf("the action did not observe a canceled context: %v", err)
	}
	if !errors.Is(err, ErrLost) {
		t.Errorf("Do = %v, want it to report ErrLost", err)
	}
	// The action's own error survives alongside the lease error, because how far it got is information.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do = %v, want the action's context.Canceled to survive too", err)
	}
}

// TestNewRefusesABackendThatCannotArbitrate covers the fail-closed construction rule for both shapes:
// a backend that says no, and one that cannot say anything.
func TestNewRefusesABackendThatCannotArbitrate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		backend types.Backend
		want    string
	}{
		{
			name:    "reports no conditional write",
			backend: refusingBackend{detail: "this endpoint accepted an If-Match that could not match"},
			want:    "accepted an If-Match",
		},
		{
			name:    "cannot report capabilities at all",
			backend: silentBackend{},
			want:    "does not report its capabilities",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l, err := New(tc.backend, Config{Key: "k", Holder: "h"})
			if err == nil {
				t.Fatal("New succeeded against a backend that cannot arbitrate; it must refuse to start")
			}
			if l != nil {
				t.Error("New returned a usable Lease alongside an error")
			}
			if !errors.Is(err, types.ErrNotSupported) {
				t.Errorf("error = %v, want it to match types.ErrNotSupported", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name the limitation (%q)", err, tc.want)
			}
		})
	}
}

// TestNewRejectsUnusableConfigs. RenewInterval >= Period is the interesting one: it produces two
// holders under no contention at all, because the lease becomes takeable before its holder renews.
func TestNewRejectsUnusableConfigs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no key", Config{Holder: "h"}, "Key is required"},
		{"no holder", Config{Key: "k"}, "Holder is required"},
		{
			"renew interval at the period",
			Config{Key: "k", Holder: "h", Period: time.Second, RenewInterval: time.Second},
			"must be shorter than Period",
		},
		{
			"renew interval beyond the period",
			Config{Key: "k", Holder: "h", Period: time.Second, RenewInterval: 2 * time.Second},
			"must be shorter than Period",
		},
		{
			// A period too small to divide into a renewal margin. RenewInterval defaults to Period/4 by
			// integer division, so this produced 0 — which passes the margin check above, since 0 really
			// is shorter than 2ns, and then panicked the process: time.NewTicker(0) is "non-positive
			// interval for NewTicker", raised inside Do's renewal goroutine where no caller can recover
			// it. Found by FuzzNewConfig, and tabled here so the rejection is not only fuzz-covered.
			"period too small to divide into a renewal margin",
			Config{Key: "k", Holder: "h", Period: 2 * time.Nanosecond},
			"below the 10ms minimum",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			_, err := New(ts.Backend(), tc.cfg)
			if err == nil {
				t.Fatalf("New(%+v) succeeded, want an error", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestNewRejectsANilBackend, separate because it cannot reach the capability probe.
func TestNewRejectsANilBackend(t *testing.T) {
	t.Parallel()

	if _, err := New(nil, Config{Key: "k", Holder: "h"}); err == nil {
		t.Fatal("New(nil, ...) succeeded, want an error")
	}
}

// TestDefaultsAreApplied checks the zero-value paths, including that RenewInterval defaults to a
// fraction of Period rather than to a constant that could exceed a short configured period.
func TestDefaultsAreApplied(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	l, err := New(ts.Backend(), Config{Key: "k", Holder: "h"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if l.cfg.Period != DefaultPeriod {
		t.Errorf("Period = %s, want %s", l.cfg.Period, DefaultPeriod)
	}
	if l.cfg.RenewInterval != DefaultPeriod/4 {
		t.Errorf("RenewInterval = %s, want Period/4 = %s", l.cfg.RenewInterval, DefaultPeriod/4)
	}
	if l.cfg.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", l.cfg.MaxAttempts, DefaultMaxAttempts)
	}
	if l.cfg.Backoff != DefaultBackoff {
		t.Errorf("Backoff = %s, want %s", l.cfg.Backoff, DefaultBackoff)
	}

	// A short period must still get a shorter renewal interval.
	short, err := New(ts.Backend(), Config{Key: "k", Holder: "h", Period: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("New with a short period: %v", err)
	}
	if short.cfg.RenewInterval >= short.cfg.Period {
		t.Errorf("RenewInterval %s is not shorter than Period %s", short.cfg.RenewInterval, short.cfg.Period)
	}
}

// TestAcquisitionIsBounded. Every lost race is a billed request, so contention must not spin: the
// request count is asserted against the attempt budget rather than merely observing that Acquire
// returned.
func TestAcquisitionIsBounded(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	holder := leaseFor(t, ts, "holder")
	if err := holder.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	const attempts = 3

	contender := leaseFor(t, ts, "contender", func(c *Config) {
		c.MaxAttempts = attempts
		c.Backoff = time.Millisecond
		c.Period = time.Hour // takeover never becomes available
	})

	ts.ResetRequests()

	if err := contender.Acquire(ctx); !errors.Is(err, ErrHeldByAnother) {
		t.Fatalf("Acquire = %v, want ErrHeldByAnother", err)
	}

	// Each attempt is one conditional PUT plus one HEAD for the takeover check. Anything materially
	// beyond that is a spin.
	writes := len(ts.Writes(holder.cfg.Key))
	if writes > attempts {
		t.Errorf("%d conditional writes for %d attempts: acquisition is not bounded", writes, attempts)
	}
	if writes == 0 {
		t.Error("no conditional writes recorded; the test is not measuring what it claims to")
	}
}

// TestDoRequiresAnAction, so a nil action is a caller error rather than a silently held lease.
func TestDoRequiresAnAction(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	l := leaseFor(t, ts, "holder")

	if err := l.Do(context.Background(), nil); err == nil {
		t.Fatal("Do(ctx, nil) succeeded, want an error")
	}
}

// TestRenewOnAnUnheldLeaseFailsClosed rather than acquiring one.
func TestRenewOnAnUnheldLeaseFailsClosed(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	l := leaseFor(t, ts, "holder")

	if err := l.Renew(context.Background()); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Renew without Acquire = %v, want ErrNotHeld", err)
	}
	if ts.ObjectExists(l.cfg.Key) {
		t.Error("Renew on an unheld lease created the lease object")
	}
}

// TestRenewAfterTheLeaseObjectIsDeletedReportsLost. A holder whose record was deleted has no claim to
// re-assert, and treating a 404 as a transient failure would leave it acting.
func TestRenewAfterTheLeaseObjectIsDeletedReportsLost(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	l := leaseFor(t, ts, "holder")
	if err := l.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if err := ts.Backend().DeleteObject(ctx, l.cfg.Key); err != nil {
		t.Fatalf("deleting the lease object: %v", err)
	}

	err := l.Renew(ctx)
	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Renew after the lease object was deleted = %v, want ErrNotHeld", err)
	}
	if l.Held() {
		t.Error("the holder still believes it holds a lease whose object is gone")
	}
}

// TestRenewKeepsTheClaimWhenTheStoreIsMerelyUnreachable is the fail-*open* direction of Renew, and it
// is the only branch of the three that must not drop the claim.
//
// A precondition failure is evidence the lease moved; a 404 is evidence the record is gone. A 500 is
// evidence of nothing about the lease — it says S3 could not be reached, and the holder's claim is
// exactly as valid as it was a moment earlier. Dropping it on that would make every transient blip
// hand the resource to the next contender, and worse, would do so while the original holder may still
// be mid-action: two nodes acting on one resource is the failure this package exists to prevent, and
// arriving at it through a retryable error would be a bitter way to get there.
//
// It is worth stating why this needed a fault injector rather than a backend double. A double returning
// a canned error would exercise the errors.Is arms and prove nothing about which errors S3 actually
// produces — the classification is only meaningful against the real translation path, where a 500
// becomes whatever internal/storage/s3 makes of it. This goes through the SDK, the retry policy, the
// circuit breaker, and the error translator, so the arm is tested against the shape of error that will
// really arrive.
func TestRenewKeepsTheClaimWhenTheStoreIsMerelyUnreachable(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	l := leaseFor(t, ts, "holder")
	if err := l.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Times is above the SDK's attempt budget so the renewal genuinely fails rather than succeeding on
	// a retry: the point is what Renew does with a failure it cannot attribute to a lost lease.
	ts.InjectFault(testaws.Fault{
		Method:    "PUT",
		KeySuffix: l.cfg.Key,
		Status:    http.StatusInternalServerError,
		Code:      "InternalError",
		Times:     10,
	})

	err := l.Renew(ctx)
	if err == nil {
		t.Fatal("Renew against an unreachable store succeeded, so the fault did not reach the write")
	}
	if ts.FaultsFired() == 0 {
		t.Fatal("no fault fired, so this test proved nothing about the error path")
	}

	// The error must not be ErrNotHeld. That sentinel is what callers key on to decide the lease is
	// gone, and reporting it here is how a transient failure would turn into a released resource.
	if errors.Is(err, ErrNotHeld) {
		t.Errorf("Renew after a transient store failure = %v, want an error that is not ErrNotHeld", err)
	}
	if !l.Held() {
		t.Fatal("a transient store failure dropped the claim; the holder still holds this lease")
	}

	// And the claim is not merely believed — it is still assertable. If the local ETag had been cleared
	// or replaced, the next renewal's precondition would fail even with the store healthy again, which
	// is the way this could be wrong while Held() still reported true.
	ts.ClearFaults()

	if err := l.Renew(ctx); err != nil {
		t.Fatalf("Renew after the store recovered = %v, want the claim to still be assertable", err)
	}
	if err := l.Do(ctx, func(_ context.Context, g Guard) error {
		return g.Put("after-recovery", []byte("still held"), nil)
	}); err != nil {
		t.Errorf("Do after the store recovered = %v, want the lease to still be usable", err)
	}
	if !ts.ObjectExists("after-recovery") {
		t.Error("a guarded write after recovery did not reach the store")
	}
}

// stealLease takes the lease at key away from its current holder, the way a real contender's takeover
// would: a conditional write against whatever ETag is there now.
//
// The retry is not defensive padding. Every steal performed *inside* a Lease.Do action races that
// action's own renewal ticker — read the ETag, the ticker fires and moves it, and the steal's
// precondition fails through no fault of the code under test. That made TestDoCancelsTheActionWhenThe
// LeaseIsLost fail roughly once in twenty runs at RenewInterval=10ms, reporting a precondition failure
// where the test meant to assert something about cancellation. Retrying against a fresh ETag is also
// what the contender being simulated would do, so this is the more faithful model as well as the
// stable one.
//
// The retry alone was not enough, and the reason is worth stating because "add a retry" is the
// intuitive fix and it left a test that still failed on CI. A HEAD+PUT round trip against the
// in-process endpoint measures ~4ms mean and ~6.4ms worst on an idle laptop. Once that round trip
// exceeds RenewInterval, a tick lands between the HEAD and the PUT on *every* attempt — so the 50
// attempts are not 50 independent trials with a compounding chance of success, they are 50 instances
// of the same certain loss. On a loaded runner where the round trip crosses 10ms that is a
// deterministic failure wearing a flake's clothes, and it is what failed on ca27fbe.
//
// So the steal takes the holder's own claimMu for its read-then-write, rather than trying to outrun
// the ticker. claimMu is the lock the holder already holds across its whole read-assert-store round
// trip — it exists so a holder's two concurrent renewals cannot defeat each other — and taking it here
// makes the steal's HEAD and PUT atomic with respect to renewals for the same reason. No production
// code changes: the mechanism was already there, and this test was reaching past it.
//
// That is also the faithful model rather than a convenience. A real contender takes over a lease
// precisely when the holder is *not* mid-renewal, because a renewal in flight is what makes the lease
// un-takeable; racing one is the single case where a takeover is supposed to lose. The retry stays for
// the interleaving where the ticker won the lock first and moved the ETag before this attempt read it.
//
// Verified by mutation rather than by argument, and the reproduction is worth keeping because CI's
// timing is not reproducible on demand: setting RenewInterval to 1ms puts the ticker inside the
// measured round trip, which is CI's condition expressed locally. With the claimMu hold removed, all
// three steal-inside-an-action tests then fail with the identical "could not steal … in 50 attempts"
// message CI produced — deterministically, not once in twenty. With it restored and the interval left
// at 1ms, 20 consecutive runs pass. A local run at the committed intervals proves nothing either way,
// which is why this note records the hostile setting instead.
//
// It deliberately does not use Lease.Acquire: that would wait out a full observation period, and the
// point of these tests is the case where the lease moves while the first holder still believes it has
// it.
func stealLease(t *testing.T, ts *testaws.TestServer, key string) {
	t.Helper()

	stealLeaseFrom(t, ts, nil, key)
}

// stealLeaseFrom is stealLease against a known holder, whose claimMu it takes so the read-then-write
// cannot be split by that holder's renewal ticker. holder may be nil, for the callers that steal a
// lease no live Lease value is renewing.
func stealLeaseFrom(t *testing.T, ts *testaws.TestServer, holder *Lease, key string) {
	t.Helper()

	var lastErr error

	for attempt := range 50 {
		err := func() error {
			if holder != nil {
				holder.claimMu.Lock()
				defer holder.claimMu.Unlock()
			}

			etag := currentLeaseETag(t, ts, key)

			_, err := ts.Backend().PutObjectIf(context.Background(), key, []byte(`{"holder":"second"}`), nil,
				types.Precondition{ETag: etag})

			return err
		}()
		if err == nil {
			return
		}
		if !errors.Is(err, types.ErrPreconditionFailed) {
			t.Fatalf("stealing the lease at %q on attempt %d: %v", key, attempt+1, err)
		}

		lastErr = err
	}

	t.Fatalf("could not steal the lease at %q in 50 attempts; last: %v", key, lastErr)
}

// currentLeaseETag reads the lease object's ETag, for tests that need to steal the lease.
func currentLeaseETag(t *testing.T, ts *testaws.TestServer, key string) string {
	t.Helper()

	info, err := ts.Backend().HeadObject(context.Background(), key)
	if err != nil {
		t.Fatalf("HeadObject(%q): %v", key, err)
	}

	return info.ETag
}

// refusingBackend reports that its endpoint does not evaluate preconditions.
type refusingBackend struct {
	noopBackend

	detail string
}

func (b refusingBackend) Capabilities() types.BackendCapabilities {
	return types.BackendCapabilities{ConditionalWrite: false, ConditionalWriteDetail: b.detail}
}

// silentBackend does not implement types.CapabilityReporter at all.
type silentBackend struct{ noopBackend }

// noopBackend is the unused remainder of types.Backend for the two doubles above. Every method fails,
// so a test that reached one by accident fails rather than passing against a stub — these doubles
// exist to be refused by New, and nothing should get past it to call them.
type noopBackend struct{}

var errNoop = errors.New("coord test: this backend should never be called")

func (noopBackend) GetObject(context.Context, string, int64, int64) ([]byte, error) {
	return nil, errNoop
}

func (noopBackend) PutObject(context.Context, string, []byte, map[string]string) error {
	return errNoop
}

func (noopBackend) PutObjectIf(context.Context, string, []byte, map[string]string,
	types.Precondition,
) (string, error) {
	return "", errNoop
}

func (noopBackend) SetObjectMetadata(context.Context, string, map[string]string) error {
	return errNoop
}
func (noopBackend) CopyObject(context.Context, string, string) error { return errNoop }
func (noopBackend) DeleteObject(context.Context, string) error       { return errNoop }

func (noopBackend) HeadObject(context.Context, string) (*types.ObjectInfo, error) {
	return nil, errNoop
}

func (noopBackend) ListObjects(context.Context, string, int) ([]types.ObjectInfo, error) {
	return nil, errNoop
}

func (noopBackend) GetObjects(context.Context, []string) (map[string][]byte, error) {
	return nil, errNoop
}

func (noopBackend) PutObjects(context.Context, map[string][]byte) error { return errNoop }
func (noopBackend) HealthCheck(context.Context) error                   { return errNoop }
func (noopBackend) Close() error                                        { return nil }
