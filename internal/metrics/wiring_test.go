package metrics

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/scttfrdmn/objectfs/internal/testhttp"
)

// exactAdapterConfig is the Config internal/adapter builds, field-for-field.
//
// Nothing here is illustrative. adapter.go sets Enabled, Addr and Labels and stops, which leaves
// Path, Namespace, Subsystem and UpdateInterval at their zero values — and two of those zeroes used
// to panic inside Start. Reproducing the shape rather than a plausible one is the point: every test
// in this package before it passed a Namespace and an address, which is why nothing noticed.
func exactAdapterConfig(addr string) *Config {
	return &Config{
		Enabled: true,
		Addr:    addr,
		Labels:  map[string]string{"service": "objectfs"},
	}
}

// anyLoopbackAddr is the address to configure when the test does not care which port it gets.
//
// Port 0 rather than a fixed number, and rather than testhttp.FreeAddr: the kernel picks a port that
// is free at the moment of the bind, where FreeAddr picks one that was free a moment earlier and
// leaves a window. Collector.Addr reports what was chosen, so a scrape still knows where to go. Use
// FreeAddr only where the address must be known before the server starts.
const anyLoopbackAddr = "127.0.0.1:0"

// TestStartSurvivesTheConfigTheAdapterBuilds is the regression test for two panics on the live path.
//
// An empty Path reaches http.ServeMux.Handle, which panics with "invalid pattern". A zero
// UpdateInterval reaches time.NewTicker, which panics with "non-positive interval" — and that one
// fires on a goroutine Start launches, where no recover in any caller can catch it, so it takes the
// process down and the mount with it. Neither was reachable from a test that filled the fields in.
func TestStartSurvivesTheConfigTheAdapterBuilds(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector rejected the config the adapter builds: %v", err)
	}

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	// The ticker panic happens in the update goroutine, so it needs a moment to have been reached.
	// Without the fix the whole test binary dies here rather than failing.
	time.Sleep(50 * time.Millisecond)
}

// TestNewCollectorFillsEachUnsetFieldIndividually pins per-field defaulting.
//
// The constructor used to apply defaults only when the entire config was nil, so a caller who set
// one field silently lost all of them. Each case sets exactly one field and asserts the rest are
// backfilled, which is the property that all-or-nothing defaulting cannot satisfy.
func TestNewCollectorFillsEachUnsetFieldIndividually(t *testing.T) {
	t.Parallel()

	want := DefaultConfig()

	cases := []struct {
		name string
		in   *Config
	}{
		{name: "nil", in: nil},
		{name: "only enabled", in: &Config{Enabled: true}},
		{name: "only an addr", in: &Config{Enabled: true, Addr: anyLoopbackAddr}},
		{name: "only labels", in: &Config{Enabled: true, Labels: map[string]string{"a": "b"}}},
		{name: "the adapter's shape", in: exactAdapterConfig(anyLoopbackAddr)},
		{
			name: "disabled",
			in:   &Config{Enabled: false},
			// Defaulting must apply to a disabled collector too. It returns early before building any
			// metric, but its config is still readable, and reporting Path "" and Namespace "" would
			// misdescribe what enabling it would do.
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, err := NewCollector(tc.in)
			if err != nil {
				t.Fatalf("NewCollector: %v", err)
			}

			if c.config.Path != want.Path {
				t.Errorf("Path = %q, want %q — an empty pattern panics in http.ServeMux.Handle",
					c.config.Path, want.Path)
			}
			if c.config.Namespace != want.Namespace {
				t.Errorf("Namespace = %q, want %q — every metric would export unprefixed, and both "+
					"SDKs plus every documented dashboard look for the objectfs_ prefix",
					c.config.Namespace, want.Namespace)
			}
			if c.config.UpdateInterval != want.UpdateInterval {
				t.Errorf("UpdateInterval = %v, want %v — a non-positive interval panics in time.NewTicker",
					c.config.UpdateInterval, want.UpdateInterval)
			}
			// The addr is the one case where a caller-set value is not the default, so it is compared
			// against what the case asked for rather than against want.Addr.
			wantAddr := want.Addr
			if tc.in != nil && tc.in.Addr != "" {
				wantAddr = tc.in.Addr
			}
			if c.config.Addr != wantAddr {
				t.Errorf("Addr = %q, want %q — an empty address binds every interface, which is what "+
					"the port form could only ever do", c.config.Addr, wantAddr)
			}
		})
	}
}

// TestNewCollectorKeepsWhatTheCallerSet asserts defaulting fills gaps and does not overwrite.
func TestNewCollectorKeepsWhatTheCallerSet(t *testing.T) {
	t.Parallel()

	in := &Config{
		Enabled:        true,
		Addr:           "127.0.0.1:19003",
		Path:           "/internal/metrics",
		Namespace:      "research",
		Subsystem:      "objectfs",
		UpdateInterval: 5 * time.Second,
	}

	c, err := NewCollector(in)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	if c.config.Path != "/internal/metrics" || c.config.Namespace != "research" ||
		c.config.Subsystem != "objectfs" || c.config.UpdateInterval != 5*time.Second ||
		c.config.Addr != "127.0.0.1:19003" {
		t.Errorf("defaulting overwrote a value the caller set: %+v", c.config)
	}
}

// TestExportedNamesAreTheOnesDocumentedAndScraped pins the metric names against their consumers.
//
// These strings are an external contract, not an internal detail: doc.go lists them, both SDK
// monitoring modules match on them, and any dashboard built against a mount hardcodes them. Renaming
// one silently breaks every scrape, and nothing else in the package would fail. Derived from
// initMetrics by enumeration.
func TestExportedNamesAreTheOnesDocumentedAndScraped(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	// Every family has to be observed at least once before it appears in a gather.
	c.RecordOperation("read", time.Millisecond, 4096, true)
	c.RecordCacheHit("data/x", 4096)
	c.RecordCacheMiss("data/x", 4096)
	c.UpdateCacheSize("L1", 1<<20)
	c.UpdateActiveConnections(3)
	c.RecordError("read", errors.New("timeout while reading"))

	got := gatherNames(t, c)

	for _, want := range []string{
		"objectfs_operations_total",
		"objectfs_operation_duration_seconds",
		"objectfs_operation_size_bytes",
		"objectfs_cache_requests_total",
		"objectfs_cache_size_bytes",
		"objectfs_active_connections",
		"objectfs_errors_total",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s is not exported; doc.go documents it and both SDKs scrape for it. Exported: %v",
				want, keys(got))
		}
	}
}

// TestCustomLabelsReachEveryMetric is the regression test for a config key that did nothing.
//
// monitoring.metrics.custom_labels was declared, defaulted to {service: objectfs}, documented in
// examples/config.yaml as "attached to every exported metric", and mapped through the adapter into
// Config.Labels — which initMetrics never read. Asserting on every family rather than one is
// deliberate: the failure mode is a family added later without ConstLabels, which a single-metric
// check would not see.
func TestCustomLabelsReachEveryMetric(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(&Config{
		Enabled: true,
		Addr:    anyLoopbackAddr,
		Labels:  map[string]string{"service": "objectfs", "cluster": "research-west"},
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.RecordOperation("read", time.Millisecond, 4096, true)
	c.RecordCacheHit("data/x", 4096)
	c.UpdateCacheSize("L1", 1<<20)
	c.UpdateActiveConnections(1)
	c.RecordError("read", errors.New("boom"))

	mfs, err := c.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(mfs) == 0 {
		t.Fatal("nothing was gathered")
	}

	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}

			for name, value := range map[string]string{"service": "objectfs", "cluster": "research-west"} {
				if labels[name] != value {
					t.Errorf("%s is missing the operator label %s=%s; it carried %v",
						mf.GetName(), name, value, labels)
				}
			}
		}
	}
}

// TestUnusableCustomLabelFailsTheMount asserts a bad label is reported, not swallowed.
//
// An operator label colliding with one of a metric's own variable labels makes the series
// ill-defined. Prometheus catches it at Register, and NewCollector propagates that — so the mount
// refuses to start and names the problem, rather than exporting metrics whose labels are not the
// ones configured.
func TestUnusableCustomLabelFailsTheMount(t *testing.T) {
	t.Parallel()

	_, err := NewCollector(&Config{
		Enabled: true,
		Addr:    anyLoopbackAddr,
		// "operation" is a variable label on operations_total, errors_total and both histograms.
		Labels: map[string]string{"operation": "read"},
	})
	if err == nil {
		t.Fatal("NewCollector accepted a custom label that collides with a variable label; the " +
			"resulting series would be undefined and the mount would come up looking healthy")
	}
	if !strings.Contains(err.Error(), "operation") {
		t.Errorf("the error does not name the offending label, so an operator cannot find it: %v", err)
	}
}

// TestCacheCounterCarriesBothHitAndMiss pins the two label values a hit rate needs.
//
// hit_rate is derived downstream as hits/(hits+misses), so a counter that only ever records misses
// cannot produce it — which was the state of the live path: RecordCacheMiss had a caller in
// internal/fuse and RecordCacheHit had none anywhere in the repo.
func TestCacheCounterCarriesBothHitAndMiss(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.RecordCacheHit("data/x", 4096)
	c.RecordCacheHit("data/y", 4096)
	c.RecordCacheMiss("data/z", 4096)

	seen := map[string]float64{}
	for _, mf := range gather(t, c) {
		if mf.GetName() != "objectfs_cache_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "type" {
					seen[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
	}

	if seen["hit"] != 2 {
		t.Errorf(`type="hit" = %v, want 2`, seen["hit"])
	}
	if seen["miss"] != 1 {
		t.Errorf(`type="miss" = %v, want 1`, seen["miss"])
	}
}

// TestScrapeServesTheDocumentedPath closes the loop over real HTTP.
//
// Every other test in this file reads the registry directly, which cannot see whether anything is
// bound to an address. This one scrapes, because the defect it guards is precisely that: the registry
// was correct and unreachable, since internal/adapter never called Start. A test that gathers
// in-process agrees with a collector that serves nothing.
func TestScrapeServesTheDocumentedPath(t *testing.T) {
	t.Parallel()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.RecordCacheHit("data/x", 4096)
	c.RecordCacheMiss("data/x", 4096)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	body := testhttp.Get(t, c.Addr(), "/metrics", "Start bound no listener")

	for _, want := range []string{
		"objectfs_cache_requests_total",
		`type="hit"`,
		`type="miss"`,
		`service="objectfs"`, // the custom label, over the wire
	} {
		if !strings.Contains(body, want) {
			t.Errorf("a scrape of /metrics does not contain %q; body:\n%s", want, body)
		}
	}
}

// TestStartBindsWhereConfiguredAndNowhereElse is the regression test for #211.
//
// The distinction it draws is the whole issue. Config.Port could only produce fmt.Sprintf(":%d"),
// which binds every interface, and no test noticed — because a wildcard bind answers on loopback,
// so scraping 127.0.0.1 passes identically either way. Asserting that something is listening is not
// the same as asserting where.
//
// Two checks, because either alone is weak. The bound address is what the kernel reports for the
// listener, so a wildcard bind reads back as 0.0.0.0 or [::] rather than the configured host. And a
// request to a routable address on this machine must be refused, which is the property an operator
// actually cares about: that the unauthenticated /metrics and /debug/operations endpoints are not
// reachable from the network.
func TestStartBindsWhereConfiguredAndNowhereElse(t *testing.T) {
	t.Parallel()

	addr := testhttp.FreeAddr(t) // fixed port: the assertion is about the host half
	c, err := NewCollector(exactAdapterConfig(addr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	if got := c.Addr(); got != addr {
		t.Errorf("bound %s, configured %s — a wildcard bind reports 0.0.0.0 or [::] here, and it "+
			"publishes an unauthenticated endpoint on every interface", got, addr)
	}

	// Confirm it over a socket too, from an address that is this host but is not loopback.
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("splitting %s: %v", addr, err)
	}

	routable := routableIPv4(t)
	if routable == "" {
		t.Log("no non-loopback IPv4 address on this host; the socket half of this test is skipped " +
			"and only the bound-address check ran")

		return
	}

	if !testhttp.Unreachable(t, net.JoinHostPort(routable, port), "/metrics") {
		t.Errorf("/metrics answered on %s, which is not the configured %s — the endpoint is exposed "+
			"to anything that can route to this host", routable, addr)
	}
}

// routableIPv4 returns a non-loopback IPv4 address of this host, or "" if it has none.
func routableIPv4(t *testing.T) string {
	t.Helper()

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("enumerating interface addresses: %v", err)
	}

	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}

	return ""
}

func gather(t *testing.T, c *Collector) []*dto.MetricFamily {
	t.Helper()

	mfs, err := c.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	return mfs
}

func gatherNames(t *testing.T, c *Collector) map[string]struct{} {
	t.Helper()

	names := map[string]struct{}{}
	for _, mf := range gather(t, c) {
		names[mf.GetName()] = struct{}{}
	}

	return names
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
