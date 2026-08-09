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

// #226's exporter half, at the only layer that can show it. internal/storage/s3 can prove the tally counts
// what S3 received and that CostStats prices it; internal/metrics can prove the gauge family and the
// periodic hook work. Neither can prove a *mount* joins the two, and the join is the defect: the rates were
// verified against AWS's published price list and every path to a user was severed — the access-pattern
// report was gated behind a config key no mount could act on, internal/cost had no importer, and
// metrics.RecordCost had no caller.
//
// Through Start rather than by calling exportCostStats on a hand-built Adapter, on the same reasoning as
// acceleration_metrics_test.go and predictive_metrics_test.go: an accessor whose only caller is a test is
// half a fix.

// TestAMountPublishesWhatItIsSpending is the end-to-end assertion, over HTTP.
func TestAMountPublishesWhatItIsSpending(t *testing.T) {
	t.Parallel()

	a := startTolerantly(t, metricsOnPortZero(t, nil))

	addr := a.metrics.Addr()
	if addr == "" {
		t.Fatal("the mount bound no metrics listener, so this test cannot reach the scrape")
	}

	// Present from the moment the mount is up, before any filesystem work and before the first tick —
	// thirty seconds by default, which is a long time for a cost family to be missing.
	body := testhttp.Get(t, addr, "/metrics", "the mount bound no metrics listener")
	if !strings.Contains(body, "objectfs_s3_cost") {
		t.Fatalf("a started mount exports no objectfs_s3_cost at all, so Start registered no publisher: "+
			"cost is computed accurately and readable by nobody, which is #226. Scrape:\n%s", body)
	}

	series := costSeriesFromScrape(t, body)

	// Every statistic the exporter names, over the wire. Enumerated rather than spot-checked because these
	// label values are the contract sdks/testdata/metrics-scrape.txt captures, so a dropped key here is a
	// silently missing series for both SDKs.
	for _, statistic := range []string{
		"write_requests",
		"list_requests",
		"read_requests",
		"free_requests",
		"bytes_retrieved",
		"bytes_stored",
		"request_cost_dollars",
		"retrieval_cost_dollars",
		"storage_cost_dollars_per_month",
		"rate_per_write_request",
		"rate_per_list_request",
		"rate_per_read_request",
		"rate_per_gb_retrieved",
		"rate_per_gb_month",
	} {
		if _, ok := series[statistic]; !ok {
			t.Errorf("the scrape carries no statistic=%q series; scrape:\n%s", statistic, body)
		}
	}

	// The rates are the check that the publisher is reading the backend rather than emitting a zeroed
	// struct: they come from the tier's entry in the rate table, so they are non-zero before any request.
	// STANDARD has no retrieval fee, so rate_per_gb_retrieved is legitimately zero and is not asserted.
	if series["rate_per_write_request"] <= 0 {
		t.Errorf(`statistic="rate_per_write_request" = %v, want the tier's published PUT rate; a zero `+
			`means the publisher is not reading the pricing manager`, series["rate_per_write_request"])
	}
	if series["rate_per_gb_month"] <= 0 {
		t.Errorf(`statistic="rate_per_gb_month" = %v, want the tier's published storage rate`,
			series["rate_per_gb_month"])
	}

	// A mount that has done no filesystem work has still made one billable request: NewBackend's health
	// check is a HeadBucket, which AWS bills like any other HEAD. Asserting it rather than asserting zero
	// is deliberate — the cost of merely being mounted is a real number and is the sort of thing this
	// family exists to make visible.
	if series["read_requests"] < 1 {
		t.Errorf(`statistic="read_requests" = %v on a fresh mount, want at least 1: the backend's `+
			`construction-time health check is a billable HEAD`, series["read_requests"])
	}
	if series["write_requests"] != 0 {
		t.Errorf(`statistic="write_requests" = %v before any write`, series["write_requests"])
	}
}

// TestTheCostSeriesNamesItsRegionAndTier pins the two labels.
//
// Without them the numbers are uninterpretable: per-request prices differ tenfold between STANDARD and
// DEEP_ARCHIVE, so a fleet-wide sum over an unlabeled family adds figures computed at different rates. The
// region matters for the same reason and has a second edge — an unpublished configured region falls back to
// us-east-1, so the label has to carry the region rates were *read* for, not the one that was asked for.
func TestTheCostSeriesNamesItsRegionAndTier(t *testing.T) {
	t.Parallel()

	const tier = "STANDARD_IA"

	a := startTolerantly(t, metricsOnPortZero(t, func(cfg *config.Configuration) {
		cfg.Storage.S3.StorageTier = tier
	}))

	body := testhttp.Get(t, a.metrics.Addr(), "/metrics", "the mount bound no metrics listener")
	labels := costLabelsFromScrape(t, body)

	if labels["tier"] != tier {
		t.Errorf("tier label = %q, want %q: the rates in this family are the tier's, so a series that "+
			"does not name it cannot be compared with one from another mount. Scrape:\n%s",
			labels["tier"], tier, body)
	}
	if labels["region"] == "" {
		t.Errorf("region label is empty: a dollar figure that does not name the region it was priced in "+
			"cannot be checked against a bill. Scrape:\n%s", body)
	}

	// The tier's own rate, not Standard's. STANDARD_IA storage is $0.0125 per GB-month against Standard's
	// $0.023, so a publisher ignoring the configured tier would be caught here by a factor of 1.84.
	series := costSeriesFromScrape(t, body)
	if got := series["rate_per_gb_month"]; got != 0.0125 {
		t.Errorf(`statistic="rate_per_gb_month" = %v with storage_tier: %s, want 0.0125 as AWS publishes `+
			`it; %v is Standard's rate, which means the publisher is not reading the configured tier`,
			got, tier, got)
	}
}

// TestEachTickRereadsTheCost asserts the callback asks the backend again rather than republishing a
// snapshot it captured at registration.
//
// This is the whole point of the family. The state at registration is a mount that has spent almost
// nothing, so a publisher closing over a snapshot would report a near-zero bill for the life of the process
// — which is a more comfortable failure than #226's and just as useless. Every other test in this file is
// blind to it: calling exportCostStats a second time takes a fresh snapshot too.
//
// Verified by mutation: hoisting the CostStats call out of the closure leaves write_requests at its opening
// value and only this test notices.
func TestEachTickRereadsTheCost(t *testing.T) {
	t.Parallel()

	a := startTolerantly(t, newAdapterForSubstrate(t, nil))

	if a.backend == nil {
		t.Fatal("Start left no backend on the adapter, so there is nothing for a publisher to read")
	}

	// The collector Start built, which the swap below displaces. Stopped by hand because Stop stops
	// whatever a.metrics holds at teardown, which by then is the fast one.
	if started := a.metrics; started != nil {
		t.Cleanup(func() { _ = started.Stop(context.Background()) })
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
	a.exportCostStats()

	before := costSeriesFromScrape(t, scrapeOf(t, collector))["write_requests"]

	// A write *after* registration. A snapshot taken at registration cannot know about it.
	if err := a.backend.PutObject(context.Background(), "cost/tick.bin", []byte("billable"), nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		body := scrapeOf(t, collector)
		if costSeriesFromScrape(t, body)["write_requests"] > before {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("write_requests is still %v 10s after a write, at a 5ms update interval; the callback "+
				"republishes a snapshot it took at registration, so the family reports the mount's opening "+
				"spend for the life of the process. Scrape:\n%s", before, body)
		}

		time.Sleep(time.Millisecond)
	}
}

// TestExportCostStatsToleratesAMissingCollectorOrBackend guards the ordering assumption.
//
// exportCostStats runs at Start's step 3, after startMetrics at step 1 and after the backend is built, so
// both fields are non-nil by then on the live path. "On the live path" is what this checks: a reordering of
// Start's steps should produce a missing metric rather than a nil dereference during mount.
func TestExportCostStatsToleratesAMissingCollectorOrBackend(t *testing.T) {
	t.Parallel()

	// Neither collector nor backend — the assertion is that this returns rather than panics.
	a := &Adapter{config: config.NewDefault()}
	a.exportCostStats()

	// A collector but no backend: the other order of the two guards.
	cfg := config.NewDefault()
	cfg.Monitoring.Metrics.Enabled = true
	cfg.Monitoring.Metrics.Addr = "127.0.0.1:0"

	a = &Adapter{config: cfg}
	if err := a.startMetrics(context.Background()); err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	t.Cleanup(func() { _ = a.metrics.Stop(context.Background()) })

	a.exportCostStats()

	body := testhttp.Get(t, a.metrics.Addr(), "/metrics", "startMetrics bound no listener")
	if strings.Contains(body, "objectfs_s3_cost") {
		t.Errorf("an adapter with no backend published cost statistics; the numbers would be a struct's "+
			"zeros presented as a mount's spend, and an empty tier label would make every series "+
			"unaggregatable. Scrape:\n%s", body)
	}
}

// costSeriesFromScrape parses objectfs_s3_cost out of a scrape as statistic name → value.
//
// The exposition text rather than the registry, for the reason predictiveSeries gives: the registry is what
// internal/metrics already reads, and what is unproven here is that a mount's values survive the trip to a
// scrape.
func costSeriesFromScrape(t *testing.T, scrape string) map[string]float64 {
	t.Helper()

	out := map[string]float64{}

	for line := range strings.SplitSeq(scrape, "\n") {
		if !strings.HasPrefix(line, "objectfs_s3_cost{") {
			continue
		}

		labels, value, ok := strings.Cut(line, "} ")
		if !ok {
			t.Fatalf("a cost series is not in the exposition format this parses: %q", line)
		}

		statistic := labelValue(t, labels, "statistic", line)

		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("the value of statistic=%q does not parse as a float: %q", statistic, value)
		}

		out[statistic] = parsed
	}

	if len(out) == 0 {
		t.Fatalf("no objectfs_s3_cost series in the scrape:\n%s", scrape)
	}

	return out
}

// costLabelsFromScrape returns the region and tier labels the cost family carries.
//
// One reading for the whole family, and it asserts the family agrees with itself: region and tier describe
// the mount rather than an individual statistic, so two different values across the series would mean the
// publisher is mixing readings from different states.
func costLabelsFromScrape(t *testing.T, scrape string) map[string]string {
	t.Helper()

	out := map[string]string{}

	for line := range strings.SplitSeq(scrape, "\n") {
		if !strings.HasPrefix(line, "objectfs_s3_cost{") {
			continue
		}

		labels, _, ok := strings.Cut(line, "} ")
		if !ok {
			t.Fatalf("a cost series is not in the exposition format this parses: %q", line)
		}

		for _, name := range []string{"region", "tier"} {
			got := labelValue(t, labels, name, line)

			if seen, ok := out[name]; ok && seen != got {
				t.Fatalf("the cost family carries %s=%q on one series and %q on another; both label the "+
					"mount rather than the statistic, so they cannot differ", name, seen, got)
			}

			out[name] = got
		}
	}

	if len(out) == 0 {
		t.Fatalf("no objectfs_s3_cost series in the scrape:\n%s", scrape)
	}

	return out
}

// labelValue pulls one label's value out of a series' label set.
func labelValue(t *testing.T, labels, name, line string) string {
	t.Helper()

	_, rest, ok := strings.Cut(labels, name+`="`)
	if !ok {
		t.Fatalf("a cost series carries no %s label: %q", name, line)
	}

	value, _, _ := strings.Cut(rest, `"`)

	return value
}
