package adapter

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/metrics"
	"github.com/scttfrdmn/objectfs/internal/testhttp"
)

// #204's exporter half, at the only layer that can show it. internal/storage/s3 can prove the gate falls
// back and comes back and that AccelerationStats reports it; internal/metrics can prove the gauge family
// and the periodic hook work. Neither can prove a *mount* joins the two, and that join is the defect:
// s3.Backend tracked the fallback accurately in a struct whose accessor had no caller outside its own
// package, so a mount serving every byte over the standard endpoint reported acceleration enabled and the
// throughput loss appeared in no scrape.
//
// These go through Start rather than calling exportAccelerationStats on a hand-built Adapter, for the same
// reason predictive_metrics_test.go does: an accessor with a test-only caller is half a fix.

// TestAMountPublishesItsAccelerationState is the end-to-end assertion, over HTTP.
//
// Acceleration is off in this mount's config, which is the case that matters most and the one an absent
// family would misreport. `configured 0` says the operator asked for the standard endpoint; an absent
// objectfs_s3_acceleration says this build does not report acceleration at all — and "which of those am I
// looking at" is the first question an operator investigating slow reads has to answer.
func TestAMountPublishesItsAccelerationState(t *testing.T) {
	t.Parallel()

	a := startTolerantly(t, metricsOnPortZero(t, nil))

	addr := a.metrics.Addr()
	if addr == "" {
		t.Fatal("the mount bound no metrics listener, so this test cannot reach the scrape")
	}

	// Present from the moment the mount is up, before any read and before the first tick — thirty seconds
	// by default, which is a long time to be unable to tell "not reporting" from "not accelerating".
	body := testhttp.Get(t, addr, "/metrics", "the mount bound no metrics listener")
	if !strings.Contains(body, "objectfs_s3_acceleration") {
		t.Fatalf("a started mount exports no objectfs_s3_acceleration at all, so Start registered no "+
			"publisher: the acceleration state is tracked by the backend and readable by nobody, which is "+
			"#204. Scrape:\n%s", body)
	}

	series := accelerationSeriesFromScrape(t, body)

	// Every statistic the exporter names, over the wire. Enumerated rather than spot-checked because these
	// label values are the contract sdks/testdata/metrics-scrape.txt captures, so a dropped key here is a
	// silently missing series for both SDKs.
	for _, statistic := range []string{
		"configured",
		"active",
		"requests",
		"bytes",
		"fallbacks",
		"avg_latency_seconds",
		"retry_period_seconds",
	} {
		if _, ok := series[statistic]; !ok {
			t.Errorf("the scrape carries no statistic=%q series; scrape:\n%s", statistic, body)
		}
	}

	// Present and zero, not absent. This is the assertion the family exists for.
	if series["configured"] != 0 {
		t.Errorf(`statistic="configured" = %v on a mount with use_acceleration unset; the publisher is `+
			`reporting something other than this mount's config`, series["configured"])
	}
	if series["active"] != 0 {
		t.Errorf(`statistic="active" = %v with acceleration unconfigured; nothing is going to the `+
			`accelerate endpoint, so this would tell an operator the opposite of the truth`, series["active"])
	}

	// The retry period is a real configured value rather than a zero, which is what distinguishes a
	// publisher reading the backend from one publishing a zeroed struct: storage.s3.acceleration_retry
	// defaults to five minutes, and no field of AccelerationStats{} does.
	if series["retry_period_seconds"] <= 0 {
		t.Errorf(`statistic="retry_period_seconds" = %v, want the configured default; a zero means the `+
			`publisher is not reading the backend's gate`, series["retry_period_seconds"])
	}
}

// TestAMountWithAccelerationConfiguredSaysSo pins the other half of the distinction.
//
// The mount runs against substrate, so `configured 1` with `active` reporting whatever the backend
// currently believes is exactly the shape of the state #204 is about — and asserting `configured` follows
// the config is what keeps the exporter from hardcoding the answer. Without this test a publisher that
// wrote 0 for both fields unconditionally would satisfy every assertion above.
//
// `active` is deliberately not asserted: the substrate endpoint is a custom BaseEndpoint, which the AWS
// SDK refuses to combine with UseAccelerate, so the first request falls back — and *when* that happens
// relative to this scrape is the gate's business and is covered by
// TestAnAccelerationErrorFallsBackAndStillServesTheRead in internal/storage/s3. What matters here is that
// the pair is published at all.
func TestAMountWithAccelerationConfiguredSaysSo(t *testing.T) {
	t.Parallel()

	a := startTolerantly(t, metricsOnPortZero(t, func(cfg *config.Configuration) {
		cfg.Storage.S3.UseAcceleration = true
		cfg.Storage.S3.AccelerationRetry = 90 * time.Second
	}))

	series := accelerationSeriesFromScrape(t,
		testhttp.Get(t, a.metrics.Addr(), "/metrics", "the mount bound no metrics listener"))

	if series["configured"] != 1 {
		t.Errorf(`statistic="configured" = %v with use_acceleration: true; the publisher is not reading `+
			`this mount's config, so the scrape cannot distinguish "asked for and not happening" from `+
			`"not asked for"`, series["configured"])
	}

	// The configured retry, not the default. This is the value an operator would use to decide how long to
	// wait before concluding a fallback is permanent, so a publisher reporting the package default while
	// the gate used something else would mislead them by more than a rounding.
	if series["retry_period_seconds"] != 90 {
		t.Errorf(`statistic="retry_period_seconds" = %v, want 90 from storage.s3.acceleration_retry; the `+
			`publisher reports a period the gate is not using`, series["retry_period_seconds"])
	}
}

// TestEachTickRereadsTheBackend asserts the callback asks the backend again rather than republishing a
// snapshot it captured at registration.
//
// This is the whole point of the family. The state at registration is always the healthy one — a mount
// begins believing it accelerates — so a publisher that closed over a snapshot would report `active 1`
// forever and the fallback would be exactly as invisible as it was before #204. Every other test in this
// file is blind to it: calling exportAccelerationStats a second time takes a fresh snapshot too.
//
// So this one builds a collector at a 5 ms interval, lets a *tick* do the work, and moves the backend's
// numbers in between by making it fall back for real: substrate is a custom BaseEndpoint, which the AWS SDK
// refuses to combine with UseAccelerate, so every attempt on the accelerate client is an acceleration
// error. A 25 ms retry period means the gate keeps going half-open and probing, so a fallback event is
// available on demand however many happened during Start.
//
// Verified by mutation: hoisting the AccelerationStats call out of the closure leaves fallbacks at its
// opening value and only this test notices.
func TestEachTickRereadsTheBackend(t *testing.T) {
	t.Parallel()

	a := newAdapterForSubstrate(t, func(cfg *config.Configuration) {
		cfg.Storage.S3.UseAcceleration = true
		cfg.Storage.S3.AccelerationRetry = 25 * time.Millisecond
	})

	if err := a.Start(context.Background()); err != nil &&
		!strings.Contains(err.Error(), "failed to mount filesystem") {
		t.Fatalf("Start failed before reaching the mount: %v", err)
	}
	t.Cleanup(func() { closePartialStart(t, a) })

	if a.backend == nil {
		t.Fatal("Start left no backend on the adapter, so there is nothing for a publisher to read")
	}

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

	// Swap in the fast-ticking collector and register the publisher, exactly as Start does.
	a.metrics = collector
	a.exportAccelerationStats()

	before := accelerationSeriesFromScrape(t, scrapeOf(t, collector))["fallbacks"]

	// Writes *after* registration, each of which attempts the accelerate client, fails, and falls back. A
	// snapshot taken at registration cannot know about them. The loop keeps writing rather than writing once
	// and waiting, because the gate may be open at any given moment — in which case nothing is attempted and
	// no fallback is recorded — and it goes half-open again 25 ms later.
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; ; i++ {
		if err := a.backend.PutObject(context.Background(),
			"tick/"+strconv.Itoa(i), []byte("x"), nil); err != nil {
			t.Fatalf("PutObject %d: %v — the fallback is supposed to keep the write serving", i, err)
		}

		body := scrapeOf(t, collector)
		if accelerationSeriesFromScrape(t, body)["fallbacks"] > before {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("fallbacks is still %v 10s and %d writes after registration, at a 5ms update interval "+
				"and a 25ms gate retry; the callback republishes a snapshot it took at registration, so the "+
				"family reports the mount's opening state — acceleration healthy — for the life of the "+
				"process, which is the exact invisibility #204 is about. Scrape:\n%s", before, i, body)
		}

		time.Sleep(time.Millisecond)
	}
}

// TestExportAccelerationStatsToleratesAMissingCollectorOrBackend guards the ordering assumption.
//
// exportAccelerationStats runs at Start's step 3, after startMetrics at step 1 and after the backend is
// built, so both fields are non-nil by then on the live path. "On the live path" is what this checks: a
// reordering of Start's steps should produce a missing metric rather than a nil dereference during mount.
func TestExportAccelerationStatsToleratesAMissingCollectorOrBackend(t *testing.T) {
	t.Parallel()

	// Neither collector nor backend — the assertion is that this returns rather than panics.
	a := &Adapter{config: config.NewDefault()}
	a.exportAccelerationStats()

	// A collector but no backend: the other order of the two guards.
	cfg := config.NewDefault()
	cfg.Monitoring.Metrics.Enabled = true
	cfg.Monitoring.Metrics.Addr = "127.0.0.1:0"

	a = &Adapter{config: cfg}
	if err := a.startMetrics(context.Background()); err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	t.Cleanup(func() { _ = a.metrics.Stop(context.Background()) })

	a.exportAccelerationStats()

	body := testhttp.Get(t, a.metrics.Addr(), "/metrics", "startMetrics bound no listener")
	if strings.Contains(body, "objectfs_s3_acceleration") {
		t.Errorf("an adapter with no backend published acceleration statistics; the numbers would be a "+
			"struct's zeros presented as a mount's state, and `active 0` in particular would read as a "+
			"fallback in effect. Scrape:\n%s", body)
	}
}

// accelerationSeriesFromScrape parses objectfs_s3_acceleration out of a scrape as statistic name → value.
//
// The exposition text rather than the registry, for the reason predictiveSeries gives: the registry is
// what internal/metrics already reads, and what is unproven here is that a mount's values survive the trip
// to a scrape.
func accelerationSeriesFromScrape(t *testing.T, scrape string) map[string]float64 {
	t.Helper()

	out := map[string]float64{}

	for line := range strings.SplitSeq(scrape, "\n") {
		if !strings.HasPrefix(line, "objectfs_s3_acceleration{") {
			continue
		}

		labels, value, ok := strings.Cut(line, "} ")
		if !ok {
			t.Fatalf("an acceleration series is not in the exposition format this parses: %q", line)
		}

		_, statistic, ok := strings.Cut(labels, `statistic="`)
		if !ok {
			t.Fatalf("an acceleration series carries no statistic label: %q", line)
		}

		statistic, _, _ = strings.Cut(statistic, `"`)

		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("the value of statistic=%q does not parse as a float: %q", statistic, value)
		}

		out[statistic] = parsed
	}

	if len(out) == 0 {
		t.Fatalf("no objectfs_s3_acceleration series in the scrape:\n%s", scrape)
	}

	return out
}
