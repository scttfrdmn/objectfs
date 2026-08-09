package metrics

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/scttfrdmn/objectfs/internal/testhttp"
)

// #223 gave the predictive cache's statistics a consumer, and this is that consumer's half: a gauge
// family whose values live in another package and a periodic hook that goes and asks for them.
//
// updatePeriodicMetrics was an empty function with the comment "this would update metrics that need
// periodic updates". That is the shape these tests are about — a gauge nothing refreshes exports its
// initial value forever, which is indistinguishable from a subsystem that never changes.

// TestPredictiveGaugeCarriesOneSeriesPerStatistic pins the family's shape.
//
// One family labeled by statistic, not a metric per number: the set of statistics will change as the
// predictive layer does, and a metric-name change is one both SDKs and every dashboard have to follow,
// while a new label value is one they pick up for free.
func TestPredictiveGaugeCarriesOneSeriesPerStatistic(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.UpdatePredictiveCache(map[string]float64{
		"predictions_total":   186,
		"predictions_correct": 61,
		"prediction_accuracy": 0.32795698924731184,
	})

	got := predictiveSeries(t, c)

	want := map[string]float64{
		"predictions_total":   186,
		"predictions_correct": 61,
		"prediction_accuracy": 0.32795698924731184,
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf(`objectfs_predictive_cache{statistic=%q} = %v, want %v`, name, got[name], value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the family carries %d series, want %d: %v", len(got), len(want), got)
	}
}

// TestPredictiveGaugeIsSetNotAccumulated is why these are gauges.
//
// The values are a snapshot of the cache's own running totals, read on a schedule rather than
// incremented here. A counter would add each snapshot to the last, so a monotonic total read twice would
// double — and a ratio read twice would exceed 1, which is the one thing a ratio must not do.
func TestPredictiveGaugeIsSetNotAccumulated(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	for range 3 {
		c.UpdatePredictiveCache(map[string]float64{
			"predictions_total":   100,
			"prediction_accuracy": 0.5,
		})
	}

	got := predictiveSeries(t, c)
	if got["predictions_total"] != 100 {
		t.Errorf("predictions_total = %v after three publications of 100; the snapshot is being "+
			"accumulated rather than set", got["predictions_total"])
	}
	if got["prediction_accuracy"] != 0.5 {
		t.Errorf("prediction_accuracy = %v after three publications of 0.5; a ratio that accumulates "+
			"exceeds 1 and stops being a ratio", got["prediction_accuracy"])
	}
}

// TestPredictiveGaugeReflectsTheLatestSnapshot asserts a later publication replaces an earlier one.
//
// The pairing for the test above: a Set that was somehow a no-op after the first call would satisfy
// that one while freezing the family at the mount's opening values — which is exactly the failure an
// unrefreshed gauge produces, and the one updatePeriodicMetrics existed as an empty function to avoid.
func TestPredictiveGaugeReflectsTheLatestSnapshot(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.UpdatePredictiveCache(map[string]float64{"prefetch_hits": 1})
	c.UpdatePredictiveCache(map[string]float64{"prefetch_hits": 42})

	if got := predictiveSeries(t, c)["prefetch_hits"]; got != 42 {
		t.Errorf("prefetch_hits = %v after publishing 1 then 42; the gauge is not tracking the latest "+
			"snapshot", got)
	}
}

// TestPredictiveGaugeCarriesTheOperatorLabels asserts the new family is not an exception.
//
// monitoring.metrics.custom_labels is documented as attached to every exported metric, and the failure
// mode is precisely a family added later without ConstLabels — which is why TestCustomLabelsReachEveryMetric
// asserts over every family rather than one. This is the same check aimed at the family that family-wide
// test would not have observed without an observation in it.
func TestPredictiveGaugeCarriesTheOperatorLabels(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(&Config{
		Enabled: true,
		Addr:    anyLoopbackAddr,
		Labels:  map[string]string{"service": "objectfs", "cluster": "research-west"},
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.UpdatePredictiveCache(map[string]float64{"predictions_total": 1})

	for _, mf := range gather(t, c) {
		if mf.GetName() != "objectfs_predictive_cache" {
			continue
		}

		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}

			for name, value := range map[string]string{"service": "objectfs", "cluster": "research-west"} {
				if labels[name] != value {
					t.Errorf("objectfs_predictive_cache is missing the operator label %s=%s; it carried %v",
						name, value, labels)
				}
			}
		}
	}
}

// TestUpdatePredictiveCacheHonorsDisabled asserts a disabled collector builds no series.
//
// A disabled collector has no registry at all, so publishing into it must not dereference one. Every
// other Update* method returns early on the same check, and this one is called from a callback the
// adapter registers before it knows whether metrics are on.
func TestUpdatePredictiveCacheHonorsDisabled(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(&Config{Enabled: false})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	// The assertion is that this does not panic on a nil GaugeVec and nil registry.
	c.UpdatePredictiveCache(map[string]float64{"predictions_total": 1})
}

// TestPeriodicCallbacksRunOnEveryTick is the regression test for the empty function.
//
// updatePeriodicMetrics was a no-op with a comment describing what it would do, and the ticker driving
// it existed and did nothing. A gauge whose value lives in another subsystem cannot be pushed at the
// moment of the operation — the cache holds the totals — so something has to ask, and if nothing asks
// the family reports the mount's opening zeros for the life of the process.
//
// A short interval and a counter rather than a sleep-and-hope: the assertion is that the callback runs
// repeatedly, so it waits for the second invocation.
func TestPeriodicCallbacksRunOnEveryTick(t *testing.T) {
	t.Parallel()

	cfg := exactAdapterConfig(anyLoopbackAddr)
	cfg.UpdateInterval = 5 * time.Millisecond

	c, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	var calls atomic.Int64

	c.OnPeriodicUpdate(func() { calls.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the periodic callback ran %d time(s) in 5s at a 5ms interval; a gauge whose value "+
				"lives elsewhere is never refreshed, so it reports the mount's opening values forever",
				calls.Load())
		}

		time.Sleep(200 * time.Microsecond)
	}
}

// TestPeriodicCallbacksRegisteredAfterStartStillRun is the ordering the adapter depends on.
//
// The collector is started first, because a bind failure should fail the mount before anything else is
// built, and the cache whose statistics it publishes does not exist until several steps later. So
// registration after Start is the live path, not an edge case — a callback list read once at Start would
// leave the predictive family permanently absent while every test that registered first still passed.
func TestPeriodicCallbacksRegisteredAfterStartStillRun(t *testing.T) {
	t.Parallel()

	cfg := exactAdapterConfig(anyLoopbackAddr)
	cfg.UpdateInterval = 5 * time.Millisecond

	c, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	var calls atomic.Int64

	c.OnPeriodicUpdate(func() { calls.Add(1) })

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a callback registered after Start never ran; the adapter registers the predictive " +
				"cache's publisher after Start by necessity, so its metrics would never appear")
		}

		time.Sleep(200 * time.Microsecond)
	}
}

// TestOnPeriodicUpdateIgnoresNil keeps a nil registration from killing the update goroutine.
//
// A nil callback invoked on the update loop panics on a goroutine no caller can recover from — the same
// shape as the zero UpdateInterval that used to panic in time.NewTicker and take the mount with it.
func TestOnPeriodicUpdateIgnoresNil(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.OnPeriodicUpdate(nil)

	// Invoked directly rather than through the ticker: the panic would be on the update goroutine, where
	// this test's failure would be a dead process rather than a failed assertion.
	c.updatePeriodicMetrics()
}

// TestPeriodicCallbacksRunWithoutTheCollectorLockHeld is the deadlock guard.
//
// A callback reads another subsystem's state and takes that subsystem's lock. Holding the collector's
// lock across that call means two packages that each look correct can deadlock — the cache's read path
// takes the stats lock and publishes, while the update goroutine holds the collector lock and waits for
// the stats lock. So the callbacks are copied out under the lock and run without it, and a callback that
// calls back into the collector is how that is asserted: it would block forever otherwise.
func TestPeriodicCallbacksRunWithoutTheCollectorLockHeld(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	// RecordOperation takes c.mu for writing, which is the strongest form of the collision.
	c.OnPeriodicUpdate(func() {
		c.RecordOperation("read", time.Millisecond, 4096, true)
		c.UpdatePredictiveCache(map[string]float64{"predictions_total": 1})
	})

	done := make(chan struct{})

	go func() {
		defer close(done)

		c.updatePeriodicMetrics()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("updatePeriodicMetrics did not return within 5s with a callback that touches the " +
			"collector; the callbacks are running with c.mu held")
	}
}

// TestPeriodicCallbacksAreSafeToRegisterConcurrently is the -race check on the registration list.
//
// Registration happens from the mount path while the update goroutine is already ticking, so the slice
// is appended to and read concurrently by construction.
func TestPeriodicCallbacksAreSafeToRegisterConcurrently(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 32 {
				c.OnPeriodicUpdate(func() {})
			}
		}()
	}

	wg.Add(1)

	go func() {
		defer wg.Done()

		for range 64 {
			c.updatePeriodicMetrics()
		}
	}()

	wg.Wait()
}

// TestPredictiveFamilyIsAbsentUntilPublished pins what a scrape shows when there is no predictive layer.
//
// The adapter publishes nothing when the cache has no predictive layer — the Redis-backed cache has
// none, and the multi-level cache only wraps L1 when prefetch is on. Both are ordinary configurations,
// so neither is an error, and an absent family is how a scrape says so. That is a distinguishable answer
// only because a GaugeVec exports no series until one is set; a family reporting zeros would say "the
// predictor is doing nothing", which is a different and false claim.
func TestPredictiveFamilyIsAbsentUntilPublished(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.RecordOperation("read", time.Millisecond, 4096, true)

	if _, ok := gatherNames(t, c)["objectfs_predictive_cache"]; ok {
		t.Error("objectfs_predictive_cache appears in a gather with nothing published; an absent " +
			"predictive layer would be reported as one that has predicted nothing")
	}
}

// TestPredictiveStatisticsReachAScrape closes the loop over HTTP.
//
// Every other test here reads the registry, which cannot tell whether a series makes it through the
// exposition format. The label values are what both SDKs key on, so they are checked as they appear on
// the wire.
func TestPredictiveStatisticsReachAScrape(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.UpdatePredictiveCache(map[string]float64{
		"predictions_total":   186,
		"prefetch_efficiency": 0.25,
	})

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	body := testhttp.Get(t, c.Addr(), c.config.Path, "Start bound no listener")

	for _, want := range []string{
		"objectfs_predictive_cache",
		`statistic="predictions_total"`,
		`statistic="prefetch_efficiency"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("a scrape of /metrics does not contain %q; body:\n%s", want, body)
		}
	}
}

// predictiveSeries returns the predictive family as statistic name → value.
func predictiveSeries(t *testing.T, c *Collector) map[string]float64 {
	t.Helper()

	out := map[string]float64{}

	for _, mf := range gather(t, c) {
		if mf.GetName() != "objectfs_predictive_cache" {
			continue
		}

		if kind := mf.GetType(); kind != dto.MetricType_GAUGE {
			t.Errorf("objectfs_predictive_cache is a %v, want a gauge: the values are snapshots of "+
				"another subsystem's totals, and a counter has no Set", kind)
		}

		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "statistic" {
					out[l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}

	return out
}
