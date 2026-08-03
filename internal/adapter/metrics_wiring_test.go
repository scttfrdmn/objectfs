package adapter

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/testhttp"
)

// TestStartMetricsBindsTheEndpoint asserts the adapter actually serves /metrics.
//
// This is the regression test for the defect that everything else about the metrics chain was
// downstream of: internal/metrics.Collector was constructed here and Start was never called on it, so
// the registry was correct and unreachable. `monitoring.metrics.enabled: true` and a metrics port
// were both honored as far as building the counters, the mount logged nothing amiss, and a scrape of
// the port got connection refused. Both SDKs' get_metrics(), the Prometheus examples in
// docs/monitoring, and every dashboard anybody built against a mount were describing an endpoint that
// did not exist.
//
// It scrapes over a socket rather than inspecting a.metrics, because that distinction *is* the defect.
// A collector with no listener gathers exactly like one with a listener; the field is non-nil either
// way. Only a request can tell them apart, which is why nothing in this package noticed for a whole
// release — mutation-testing the fix by deleting the Start call left `go test ./internal/adapter/`
// entirely green before this test existed.
//
// The address is a non-default one, and that is deliberate. A fixture equal to the default passes
// whether the mapping happened or not, since the field would hold that value anyway.
func TestStartMetricsBindsTheEndpoint(t *testing.T) {
	t.Parallel()

	addr := testhttp.FreeAddr(t)

	cfg := config.NewDefault()
	cfg.Monitoring.Metrics.Enabled = true
	cfg.Monitoring.Metrics.Addr = addr

	a := &Adapter{config: cfg}

	if err := a.startMetrics(context.Background()); err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	t.Cleanup(func() { _ = a.metrics.Stop(context.Background()) })

	// The address the listener reports, which is the value the consumer received — not a recomputation
	// of the mapping. #211's whole point is that "something is listening" and "the listener is where
	// the configuration said" are different assertions, and only the second one fails for a wildcard
	// bind: a wildcard answers on loopback too.
	if got := a.metrics.Addr(); got != addr {
		t.Errorf("the collector bound %s; monitoring.metrics.addr was %s. A wildcard bind reports "+
			"0.0.0.0 or [::] here and publishes an unauthenticated endpoint on every interface", got, addr)
	}

	body := testhttp.Get(t, addr, "/metrics", "the adapter bound no metrics listener")

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
// while ignoring the setting. metrics.Collector.Start returns nil early when disabled, so the address
// must stay free — an operator who turns metrics off should not find a listener on it.
//
// This is also the only way to turn the endpoint off now. `metrics_port: 0` used to look like a way,
// and it was the worst of both: zero read as "unset" to the collector's defaulting, came back as 8080,
// and got bound (#212). An address has no value that quietly means something else.
func TestStartMetricsHonorsDisabled(t *testing.T) {
	t.Parallel()

	addr := testhttp.FreeAddr(t)

	cfg := config.NewDefault()
	cfg.Monitoring.Metrics.Enabled = false
	cfg.Monitoring.Metrics.Addr = addr

	a := &Adapter{config: cfg}

	if err := a.startMetrics(context.Background()); err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	t.Cleanup(func() { _ = a.metrics.Stop(context.Background()) })

	// Waits out the same budget a successful scrape is allowed, rather than dialing once: a single
	// immediate failure would also be what a listener that is merely slow to bind looks like, so a test
	// written that way passes whether the setting is honored or not.
	if !testhttp.Unreachable(t, addr, "/metrics") {
		t.Errorf("something is serving /metrics on %s with monitoring.metrics.enabled false", addr)
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
	cfg.Monitoring.Metrics.Addr = testhttp.FreeAddr(t)
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

// TestStartMetricsFailsWhenTheAddressIsTaken asserts a bind failure fails the mount.
//
// It used to be logged with fmt.Printf from inside the goroutine that served, and nothing propagated:
// an address already in use left the mount running with no endpoint and one line of output on stdout
// that an operator had no reason to be watching. #192 called the non-fatal behavior "the right call
// for observability", and it is the opposite — `enabled: false` is already how you ask for no
// endpoint, so an operator who asked for one and silently did not get it learns from a scrape failing
// days later.
//
// A live listener rather than a malformed address, because malformed is rejected earlier now, by
// Configuration.Validate. What reaches the bind is a real conflict.
func TestStartMetricsFailsWhenTheAddressIsTaken(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving an address: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	cfg := config.NewDefault()
	cfg.Monitoring.Metrics.Enabled = true
	cfg.Monitoring.Metrics.Addr = ln.Addr().String()

	a := &Adapter{config: cfg}

	err = a.startMetrics(context.Background())
	if err == nil {
		t.Fatal("startMetrics succeeded against an address already in use; the mount would come up " +
			"with no metrics endpoint and nothing but a log line to say so")
	}
	if !strings.Contains(err.Error(), ln.Addr().String()) {
		t.Errorf("the error does not name the address that could not be bound, which is the one piece "+
			"of information the operator needs: %v", err)
	}
}
