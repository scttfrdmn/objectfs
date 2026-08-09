package adapter

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/metrics"
	"github.com/scttfrdmn/objectfs/internal/testhttp"
)

// #223's second half, and the half only this package can show. internal/cache can prove the predictive
// statistics are computed and that GetPredictiveCache reaches them; internal/metrics can prove the gauge
// family and the periodic hook work. Neither can prove a *mount* joins the two, and that gap is the
// defect: the numbers were computed on every read of every mount and discarded at unmount, because
// nothing above the cache could reach them.
//
// An accessor with no caller would have been half a fix — a second path nothing exercises — so the
// accessor and its consumer landed together, and these tests go through Start rather than calling
// exportPredictiveStats on a hand-built Adapter.

// TestAMountPublishesItsPredictiveStatistics is the end-to-end assertion, over HTTP.
//
// It reads through the mount's own cache and scrapes the mount's own endpoint. Every layer skipped is a
// layer where the value could be dropped, and each of #223's neighbours in this milestone was exactly
// that kind of drop: a collector that was never started (#211), a Config.Labels the collector never read,
// an accessor nothing called.
//
// The republish is deliberate and is not the wiring under test. monitoring.metrics has no
// update_interval, so a mount's collector ticks at the metrics package default of thirty seconds — too
// long to wait for and not worth a test-only config knob. That the periodic tick calls a callback
// registered after Start is asserted where it belongs, in TestPeriodicCallbacksRegisteredAfterStartStillRun;
// what is asserted here is the part only a mount has: that the callback Start registered reads *this
// mount's* cache and lands on *this mount's* endpoint.
func TestAMountPublishesItsPredictiveStatistics(t *testing.T) {
	t.Parallel()

	a := startTolerantly(t, metricsOnPortZero(t, nil))

	addr := a.metrics.Addr()
	if addr == "" {
		t.Fatal("the mount bound no metrics listener, so this test cannot reach the scrape")
	}

	// Present from the moment the mount is up, before any read. Without the publish at registration the
	// family is absent from every scrape until the first tick — thirty seconds by default — and an absent
	// family means "this mount has no predictive layer", which for this configuration is false.
	body := testhttp.Get(t, addr, "/metrics", "the mount bound no metrics listener")
	if !strings.Contains(body, "objectfs_predictive_cache") {
		t.Fatalf("a started mount exports no objectfs_predictive_cache at all, so Start registered no "+
			"publisher: the statistics are computed on every read and discarded, which is #223. Scrape:\n%s",
			body)
	}

	// Sequential reads through the mount's cache. The predictor is keyed per object and emits nothing
	// until it has three accesses of one key scoring above the sequential threshold, so a rotation of
	// distinct keys at offset 0 would produce no statistics and the assertion below would be vacuous. Fill
	// first, then stream: Put records an access too, so an interleaved loop gives the predictor a history
	// that alternates and scores as non-sequential.
	for i := range 32 {
		a.cache.Put("stream", int64(i)*4096, make([]byte, 4096))
	}
	for i := range 32 {
		a.cache.Get("stream", int64(i)*4096, 4096)
	}

	// What a tick does, without waiting for one. Registering a second callback is harmless — publishing
	// the same snapshot twice sets the same gauges — and the alternative is a thirty-second test.
	a.exportPredictiveStats()

	body = testhttp.Get(t, addr, "/metrics", "the mount bound no metrics listener")

	// Every statistic the exporter names, over the wire. Enumerated rather than spot-checked because these
	// label values are the contract sdks/testdata/metrics-scrape.txt captures, so a dropped key here is a
	// silently missing series for both SDKs.
	for _, statistic := range []string{
		"predictions_total",
		"predictions_correct",
		"prediction_accuracy",
		"avg_confidence",
		"prefetch_requests",
		"prefetch_hits",
		"prefetch_bytes",
		"prefetch_waste",
		"prefetch_efficiency",
		"evictions_total",
		"evictions_intelligent",
	} {
		if !strings.Contains(body, `statistic="`+statistic+`"`) {
			t.Errorf("the scrape carries no statistic=%q series; scrape:\n%s", statistic, body)
		}
	}

	// And the values are this mount's, not a fresh struct's zeros. A publisher that read some other cache
	// — or a snapshot captured at registration rather than re-read per tick — would satisfy every
	// assertion above with the mount's opening zeros intact.
	if series := predictiveSeries(t, body); series["predictions_total"] == 0 {
		t.Errorf("predictions_total is still zero after 32 sequential reads through the mount's cache; "+
			"the publisher is not reading the cache the mount is serving from. Scrape:\n%s", body)
	}
}

// TestAMountWithoutAPredictiveLayerPublishesNothing pins the absence case.
//
// The Redis-backed distributed cache has no predictive layer and no way to grow one, which is an ordinary
// configuration rather than an error — so the family is simply absent, and absent is distinguishable from
// present-and-zero to whoever is reading the scrape. Without this test an exporter that ignored the cache
// type and published zeros would satisfy the test above while telling an operator running a shared cache
// that their predictor had never predicted anything.
//
// It is also the safety check on the type assertion: a *redis.Cache is not a *cache.MultiLevelCache, so
// an exporter that assumed the concrete type would panic during Start on this configuration.
func TestAMountWithoutAPredictiveLayerPublishesNothing(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)

	a := startTolerantly(t, metricsOnPortZero(t, func(cfg *config.Configuration) {
		cfg.Cluster.Enabled = true
		cfg.Cluster.ListenAddr = "127.0.0.1:0"
		cfg.Cluster.SecretFile = writeClusterSecret(t)
		cfg.Cluster.Redis = config.RedisConfig{
			Enabled:    true,
			Address:    mr.Addr(),
			KeyPrefix:  "predictive",
			TTL:        time.Minute,
			MaxRetries: 1,
		}
	}))

	// Reads that would produce statistics if there were a predictive layer to produce them, then the same
	// publish a tick would perform. No sleep: publication is synchronous, so if one were going to happen
	// it has happened by the time exportPredictiveStats returns.
	for i := range 16 {
		a.cache.Put("stream", int64(i)*4096, make([]byte, 4096))
		a.cache.Get("stream", int64(i)*4096, 4096)
	}

	a.exportPredictiveStats()

	body := testhttp.Get(t, a.metrics.Addr(), "/metrics", "the mount bound no metrics listener")
	if strings.Contains(body, "objectfs_predictive_cache") {
		t.Errorf("a Redis-backed mount exports objectfs_predictive_cache; it has no predictive layer, so "+
			"every series would be a permanent zero presented as a measurement. Scrape:\n%s", body)
	}
}

// TestEachTickRereadsTheCache asserts the callback asks the cache again rather than republishing a
// snapshot it captured at registration.
//
// This is the difference between a working gauge and a gauge frozen at the mount's opening zeros, and it
// is invisible to any test that publishes by hand: calling exportPredictiveStats a second time would take
// a fresh snapshot too, so a closure over a captured PredictiveStats satisfies every other test in this
// file. The only thing that distinguishes them is a *tick* — the callback running with no help — which is
// why this test builds its own collector at a 5 ms interval instead of going through Start, whose
// collector ticks at the metrics package's thirty-second default (monitoring.metrics has no
// update_interval, deliberately: nobody has asked for one).
//
// Verified by mutation: hoisting the PredictiveStats call out of the closure leaves predictions_total at
// zero forever and only this test notices.
func TestEachTickRereadsTheCache(t *testing.T) {
	t.Parallel()

	mlc := predictiveMultiLevelCache(t, true)

	collector, err := metrics.NewCollector(&metrics.Config{
		Enabled:        true,
		Addr:           "127.0.0.1:0",
		Labels:         map[string]string{"service": "objectfs"},
		UpdateInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := collector.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = collector.Stop(context.Background()) })

	a := &Adapter{config: config.NewDefault(), cache: mlc, metrics: collector}

	// Registered before any read, exactly as Start does it: the cache exists but has served nothing, so
	// the values published at registration are zeros.
	a.exportPredictiveStats()

	if got := predictiveSeries(t, scrapeOf(t, collector))["predictions_total"]; got != 0 {
		t.Fatalf("predictions_total = %v before any read; this test cannot distinguish a re-read from a "+
			"snapshot unless it starts from zero", got)
	}

	// Reads *after* registration. A snapshot taken at registration cannot know about these.
	for i := range 32 {
		mlc.Put("stream", int64(i)*4096, make([]byte, 4096))
	}
	for i := range 32 {
		mlc.Get("stream", int64(i)*4096, 4096)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		body := scrapeOf(t, collector)
		if predictiveSeries(t, body)["predictions_total"] > 0 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("predictions_total is still zero 10s after 32 sequential reads, at a 5ms update "+
				"interval; the callback republishes a snapshot it took at registration, so the family "+
				"reports the mount's opening zeros for the life of the process. Scrape:\n%s", body)
		}

		time.Sleep(time.Millisecond)
	}
}

// TestExportPredictiveStatsHonorsTheAbsenceReport covers the guard no mount can currently reach.
//
// PredictiveStats returns a boolean as well as a struct, and the exporter has to respect it — publishing
// a zeroed struct would report a predictor that has predicted nothing, which is a different claim from
// "there is no predictor". The test above cannot show this: a Redis cache fails the type assertion first,
// so the boolean is never consulted, and multiLevelConfigFrom sets Prefetch true unconditionally, so no
// configuration produces a MultiLevelCache without a predictive layer.
//
// So the cache is built here rather than by Start. That makes this a test of the exporter and not of the
// mount path, which is the point: the guard is what keeps a future config knob for prefetch — or an L1
// that declines to wrap itself — from turning into a family of false zeros. Verified by mutation:
// dropping the boolean check leaves every other test in this file green.
func TestExportPredictiveStatsHonorsTheAbsenceReport(t *testing.T) {
	t.Parallel()

	mlc := predictiveMultiLevelCache(t, false)

	cfg := config.NewDefault()
	cfg.Monitoring.Metrics.Enabled = true

	a := &Adapter{config: cfg, cache: mlc}
	a.config.Monitoring.Metrics.Addr = "127.0.0.1:0"

	if err := a.startMetrics(context.Background()); err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	t.Cleanup(func() { _ = a.metrics.Stop(context.Background()) })

	a.exportPredictiveStats()

	body := testhttp.Get(t, a.metrics.Addr(), "/metrics", "startMetrics bound no listener")
	if strings.Contains(body, "objectfs_predictive_cache") {
		t.Errorf("a multi-level cache with no predictive layer published predictive statistics; an "+
			"operator would read the zeros as a predictor that never fires. Scrape:\n%s", body)
	}
}

// TestExportPredictiveStatsToleratesAMissingCollector guards the ordering assumption.
//
// exportPredictiveStats runs at Start's step 3, after startMetrics at step 1, so a.metrics is non-nil by
// then on the live path. "On the live path" is what this checks: a reordering of Start's steps, or a
// future caller that builds the cache first, should get a missing metric rather than a nil dereference
// during mount.
func TestExportPredictiveStatsToleratesAMissingCollector(t *testing.T) {
	t.Parallel()

	// Neither collector nor cache — the assertion is that this returns rather than panics.
	a := &Adapter{config: config.NewDefault()}
	a.exportPredictiveStats()

	// A collector but no cache: the other order of the two guards. A nil types.Cache fails the type
	// assertion rather than satisfying it, which is what keeps this from being a nil-method call.
	cfg := config.NewDefault()
	cfg.Monitoring.Metrics.Enabled = true
	cfg.Monitoring.Metrics.Addr = "127.0.0.1:0"

	a = &Adapter{config: cfg}
	if err := a.startMetrics(context.Background()); err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	t.Cleanup(func() { _ = a.metrics.Stop(context.Background()) })

	a.exportPredictiveStats()

	body := testhttp.Get(t, a.metrics.Addr(), "/metrics", "startMetrics bound no listener")
	if strings.Contains(body, "objectfs_predictive_cache") {
		t.Errorf("an adapter with no cache published predictive statistics; the numbers would be a "+
			"struct's zeros rather than any cache's. Scrape:\n%s", body)
	}
}

// metricsOnPortZero builds a substrate-backed adapter whose metrics endpoint binds an ephemeral port.
//
// The address is set after New, because New validates and monitoring.metrics.addr rejects port 0 by
// design: zero used to read as "off" and default back to 8080 (#212), so the field has no value that
// quietly means something else. A test still wants the kernel to pick at the moment of the bind rather
// than a port from testhttp.FreeAddr, which is already back in the ephemeral pool by the time it is
// returned — see TestStartMetricsBindsTheEndpoint, which failed in CI on exactly that race.
func metricsOnPortZero(t *testing.T, mutate func(*config.Configuration)) *Adapter {
	t.Helper()

	a := newAdapterForSubstrate(t, func(cfg *config.Configuration) {
		cfg.Monitoring.Metrics.Enabled = true

		if mutate != nil {
			mutate(cfg)
		}
	})

	a.config.Monitoring.Metrics.Addr = "127.0.0.1:0"

	return a
}

// predictiveMultiLevelCache builds the cache a mount builds, with prefetch as the one variable.
//
// prefetch is what decides whether L1 gets wrapped in a PredictiveCache at all, and multiLevelConfigFrom
// sets it true unconditionally — so false is a shape no configuration produces today and true is the shape
// every mount has.
func predictiveMultiLevelCache(t *testing.T, prefetch bool) *cache.MultiLevelCache {
	t.Helper()

	mlc, err := cache.NewMultiLevelCache(&cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       64 << 20,
			MaxEntries: 1000,
			TTL:        time.Minute,
			Prefetch:   prefetch,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache: %v", err)
	}
	t.Cleanup(func() { _ = mlc.Close() })

	return mlc
}

// scrapeOf fetches /metrics from a started collector.
func scrapeOf(t *testing.T, c *metrics.Collector) string {
	t.Helper()

	return testhttp.Get(t, c.Addr(), "/metrics", "the collector bound no listener")
}

// predictiveSeries parses objectfs_predictive_cache out of a scrape as statistic name → value.
//
// The exposition text rather than the registry, because the registry is what every test in
// internal/metrics already reads: what is unproven here is that the mount's values survive the trip to a
// scrape, and a gather in this package would not show that.
func predictiveSeries(t *testing.T, scrape string) map[string]float64 {
	t.Helper()

	out := map[string]float64{}

	for _, line := range strings.Split(scrape, "\n") {
		if !strings.HasPrefix(line, "objectfs_predictive_cache{") {
			continue
		}

		labels, value, ok := strings.Cut(line, "} ")
		if !ok {
			t.Fatalf("a predictive series is not in the exposition format this parses: %q", line)
		}

		_, statistic, ok := strings.Cut(labels, `statistic="`)
		if !ok {
			t.Fatalf("a predictive series carries no statistic label: %q", line)
		}

		statistic, _, _ = strings.Cut(statistic, `"`)

		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("the value of statistic=%q does not parse as a float: %q", statistic, value)
		}

		out[statistic] = parsed
	}

	if len(out) == 0 {
		t.Fatalf("no objectfs_predictive_cache series in the scrape:\n%s", scrape)
	}

	return out
}
