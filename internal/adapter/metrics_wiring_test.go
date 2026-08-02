package adapter

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/testhttp"
)

// TestStartMetricsBindsTheEndpoint asserts the adapter actually serves /metrics.
//
// This is the regression test for the defect that everything else about the metrics chain was
// downstream of: internal/metrics.Collector was constructed here and Start was never called on it, so
// the registry was correct and unreachable. `monitoring.metrics.enabled: true` and
// `global.metrics_port: 8080` were both honored as far as building the counters, the mount logged
// nothing amiss, and a scrape of the port got connection refused. Both SDKs' get_metrics(), the
// Prometheus examples in docs/monitoring, and every dashboard anybody built against a mount were
// describing an endpoint that did not exist.
//
// It scrapes over a socket rather than inspecting a.metrics, because that distinction *is* the defect.
// A collector with no listener gathers exactly like one with a listener; the field is non-nil either
// way. Only a request can tell them apart, which is why nothing in this package noticed for a whole
// release — mutation-testing the fix by deleting the Start call left `go test ./internal/adapter/`
// entirely green before this test existed.
func TestStartMetricsBindsTheEndpoint(t *testing.T) {
	t.Parallel()

	cfg := config.NewDefault()
	cfg.Monitoring.Metrics.Enabled = true
	cfg.Global.MetricsPort = testhttp.FreePort(t)

	a := &Adapter{config: cfg}

	if err := a.startMetrics(context.Background()); err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	t.Cleanup(func() { _ = a.metrics.Stop(context.Background()) })

	body := testhttp.Get(t, cfg.Global.MetricsPort, "/metrics",
		"the adapter bound no metrics listener")

	// The prefix and the operator label, both over the wire. Between them they cover the two other
	// live-path defects in the same chain: an empty Namespace exported every series unprefixed, and
	// Config.Labels was mapped through this adapter and then read by nothing, so
	// monitoring.metrics.custom_labels attached to no metric. NewDefault sets service=objectfs.
	for _, want := range []string{"objectfs_", `service="objectfs"`} {
		if !strings.Contains(body, want) {
			t.Errorf("a scrape of /metrics does not contain %q; body:\n%s", want, body)
		}
	}
}

// TestStartMetricsHonorsDisabled asserts a disabled collector binds nothing.
//
// The pairing matters: without it, a Start that unconditionally bound would satisfy the test above
// while ignoring the setting. metrics.Collector.Start returns nil early when disabled, so the port
// must stay free — an operator who turns metrics off should not find a listener on 8080.
func TestStartMetricsHonorsDisabled(t *testing.T) {
	t.Parallel()

	port := testhttp.FreePort(t)

	cfg := config.NewDefault()
	cfg.Monitoring.Metrics.Enabled = false
	cfg.Global.MetricsPort = port

	a := &Adapter{config: cfg}

	if err := a.startMetrics(context.Background()); err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	t.Cleanup(func() { _ = a.metrics.Stop(context.Background()) })

	// Waits out the same budget a successful scrape is allowed, rather than dialing once: a single
	// immediate failure would also be what a listener that is merely slow to bind looks like, so a test
	// written that way passes whether the setting is honored or not.
	if !testhttp.Unreachable(t, strconv.Itoa(port), "/metrics") {
		t.Errorf("something is serving /metrics on port %d with monitoring.metrics.enabled false", port)
	}
}

// TestStartMetricsRejectsAnUnusableLabel asserts a bad custom label fails the mount.
//
// A label colliding with one of a metric's own variable labels makes the series ill-defined.
// Prometheus catches it at registration and NewCollector propagates it, so Start must too: the mount
// refuses with the offending name in the message rather than coming up exporting metrics whose labels
// are not the configured ones.
func TestStartMetricsRejectsAnUnusableLabel(t *testing.T) {
	t.Parallel()

	cfg := config.NewDefault()
	cfg.Monitoring.Metrics.Enabled = true
	cfg.Global.MetricsPort = testhttp.FreePort(t)
	// "operation" is a variable label on operations_total, errors_total and both histograms.
	cfg.Monitoring.Metrics.CustomLabels = map[string]string{"operation": "read"}

	a := &Adapter{config: cfg}

	err := a.startMetrics(context.Background())
	if err == nil {
		t.Fatal("startMetrics accepted a custom label that collides with a variable label; the mount " +
			"would come up looking healthy while exporting undefined series")
	}
	if !strings.Contains(err.Error(), "operation") {
		t.Errorf("the error does not name the offending label, so an operator cannot find it: %v", err)
	}
}
