package coord

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// FuzzNewConfig asserts the one property that makes a Config safe to hold a lease with: New either
// refuses it, or produces a lease whose renewal interval leaves genuine margin before its period.
//
// That invariant is not decoration. A RenewInterval at or past Period means the lease becomes takeable
// before its holder would ever renew, which produces two simultaneous holders under no contention at
// all — the failure mode this package exists to prevent, arrived at through configuration rather than
// through a race.
//
// It is fuzzed rather than tabled because the defect this would have caught was an ordering bug
// between two functions that were each correct: validate() ran before withDefaults(), so the zero
// value was checked against unfilled fields and 0 >= 0 rejected it. Found by a test asserting the
// defaults, but the shape — a validation whose answer depends on what has been filled in yet — is
// exactly what a generator over the whole input domain is better at than a list of cases someone
// thought of.
func FuzzNewConfig(f *testing.F) {
	f.Add("leases/k", "holder", int64(0), int64(0), 0, int64(0))
	f.Add("leases/k", "holder", int64(time.Second), int64(time.Millisecond), 3, int64(time.Millisecond))
	f.Add("", "", int64(0), int64(0), 0, int64(0))
	// Period == RenewInterval: the boundary the margin rule turns on.
	f.Add("k", "h", int64(time.Second), int64(time.Second), 1, int64(1))
	// Negative durations, which withDefaults treats as unset.
	f.Add("k", "h", int64(-1), int64(-1), -1, int64(-1))
	// A renewal interval past the period, which must be refused rather than defaulted away.
	f.Add("k", "h", int64(time.Second), int64(time.Hour), 2, int64(time.Second))

	// One backend, built once. New performs no I/O — it only asks the backend for its capabilities —
	// so every iteration can share it, and rebuilding per iteration would spend the run on client
	// construction and exhaust the ephemeral port range.
	backend, err := testaws.Shared(f).Backend(context.Background())
	if err != nil {
		f.Fatalf("building a backend: %v", err)
	}

	f.Fuzz(func(t *testing.T, key, holder string, period, renew int64, attempts int, backoff int64) {
		cfg := Config{
			Key:           key,
			Holder:        holder,
			Period:        time.Duration(period),
			RenewInterval: time.Duration(renew),
			MaxAttempts:   attempts,
			Backoff:       time.Duration(backoff),
		}

		l, err := New(backend, cfg)
		if err != nil {
			// A refusal must name what is wrong with the config. An error an operator cannot act on is
			// only marginally better than a silent default.
			if !strings.Contains(err.Error(), "coord:") {
				t.Errorf("New(%+v) refused with an error that does not name this package: %v", cfg, err)
			}

			return
		}

		// Accepted. Every field must now be usable, whatever was passed in.
		switch {
		case l.cfg.Key == "":
			t.Errorf("New(%+v) accepted an empty Key", cfg)
		case l.cfg.Holder == "":
			t.Errorf("New(%+v) accepted an empty Holder", cfg)
		case l.cfg.Period <= 0:
			t.Errorf("New(%+v) produced Period %s", cfg, l.cfg.Period)
		case l.cfg.RenewInterval <= 0:
			t.Errorf("New(%+v) produced RenewInterval %s", cfg, l.cfg.RenewInterval)
		case l.cfg.MaxAttempts <= 0:
			t.Errorf("New(%+v) produced MaxAttempts %d; acquisition must be bounded", cfg, l.cfg.MaxAttempts)
		case l.cfg.Backoff <= 0:
			t.Errorf("New(%+v) produced Backoff %s", cfg, l.cfg.Backoff)
		case l.cfg.RenewInterval >= l.cfg.Period:
			// The load-bearing one: this config would let the lease expire before its holder renews.
			t.Errorf("New(%+v) accepted RenewInterval %s >= Period %s, which makes the lease takeable "+
				"before its holder would renew it", cfg, l.cfg.RenewInterval, l.cfg.Period)
		}

		// And a freshly constructed lease holds nothing. New must not be able to acquire.
		if l.Held() {
			t.Errorf("New(%+v) returned a lease that already believes it is held", cfg)
		}
	})
}
