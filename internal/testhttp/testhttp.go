// Package testhttp supports tests that assert on something a server actually served.
//
// It exists because several of the defects in v0.10.0 were invisible to any test that inspected a
// struct. The metrics collector is the clearest case: one whose HTTP listener was never bound
// gathers into its registry exactly like one whose listener was bound, the field is non-nil either
// way, and only a request over a socket tells them apart. So the missing bind survived a release,
// and deleting the call left every test in the package green.
//
// A test that wants that guarantee needs two unglamorous things — an address nothing else is using,
// and a fetch that tolerates a listener still coming up — and both were copied into
// internal/metrics/wiring_test.go and internal/adapter/metrics_wiring_test.go, where they had
// already drifted apart in their failure messages. One copy, in one place, is what keeps the
// message that explains the failure attached to the check that finds it.
//
// Addresses, not ports. The settings these helpers exercise are host:port now — a port could not
// name an interface, so every value of monitoring.metrics_port bound all of them (#211) — and a
// helper that takes a port cannot express the assertion those issues turn on: not that something is
// listening, but that it is listening *where the configuration said*. Get dials the host it is
// given, so scraping a loopback address proves a wildcard bind was not what happened; the old
// port-only form dialed 127.0.0.1 unconditionally and passed against either.
package testhttp

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// pollInterval and maxPolls bound how long [Get] waits for a listener to answer.
//
// One second in total. Generous for a bind on loopback, and short enough that a test asserting
// nothing is listening — which must wait out the whole budget — does not dominate a run.
const (
	pollInterval = 20 * time.Millisecond
	maxPolls     = 50
)

// FreeAddr returns a loopback host:port that is currently unused.
//
// Tests run in parallel with each other and with whatever else is on the machine, and the ports
// these servers default to are the popular ones: 8080 for ObjectFS's metrics endpoint, 9090 for the
// Prometheus a developer may well have running. Asking the kernel is the only way to be sure.
//
// **Only for an address nothing will bind.** The port is returned to the ephemeral pool by the
// Close below, so between here and a bind by the caller the kernel is free to hand it to anything
// else asking for an ephemeral port — including another test in the same binary, which is not a
// hypothetical: this window failed TestStartMetricsBindsTheEndpoint in CI with "bind: address
// already in use" on a port internal/adapter's own miniredis had been given in the interval. The
// window cannot be closed without handing over the listener itself.
//
// So the caller that survives this is the one asserting nothing is listening, which needs an
// address in advance and never binds it — a competing ephemeral bind cannot fail that assertion,
// because the thing it would have to do to fail it is answer HTTP 200 on /metrics. A caller that
// does bind should configure port 0 and read back where the kernel put it:
// [metrics.Collector.Addr] reports it, and [SameHost] is how the where-it-bound assertion survives
// not knowing the port up front.
func FreeAddr(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving an address: %v", err)
	}

	addr := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved address %s: %v", addr, err)
	}

	return addr
}

// SameHost fails the test unless a server bound the host its configuration named.
//
// This is the assertion #211 turns on, in the form that works when the port is 0. A server handed
// "127.0.0.1:0" binds a port only the kernel knows, so `bound == configured` cannot be the check —
// but the port half was never what the issue was about. `fmt.Sprintf(":%d", Port)` was the defect,
// and what it produced was a *wildcard host*: an unauthenticated /metrics and /debug/operations
// published on every interface the machine can route. A wildcard bind reads back here as 0.0.0.0 or
// [::], never as the loopback address that was asked for, so comparing hosts catches it — while a
// scrape of 127.0.0.1 does not, since a wildcard answers on loopback too.
//
// whatBound names the thing whose address this is, so the failure says which listener is exposed
// rather than only that some host comparison did not hold.
func SameHost(t *testing.T, bound, configured, whatBound string) {
	t.Helper()

	boundHost, _, err := net.SplitHostPort(bound)
	if err != nil {
		t.Fatalf("%s reported %q, which is not a host:port: %v", whatBound, bound, err)
	}

	configuredHost, _, err := net.SplitHostPort(configured)
	if err != nil {
		t.Fatalf("the configured address for %s is %q, which is not a host:port: %v", whatBound, configured, err)
	}

	if boundHost != configuredHost {
		t.Errorf("%s bound host %s; the configuration said %s (%q vs %q). A wildcard bind reports "+
			"0.0.0.0 or [::] here and publishes an unauthenticated endpoint on every interface",
			whatBound, boundHost, configuredHost, bound, configured)
	}
}

// Get fetches a path from a server at addr, retrying until it answers, and fails the test if nothing
// ever does.
//
// addr is a host:port, and the host is dialed as given — that is the point of taking an address
// rather than a port. whatBound describes what the caller expected to bind the listener, and is
// quoted in the failure: "nothing ever answered … — the adapter bound no metrics listener" is a
// sentence someone reading a CI log can act on, where a bare dial error is a sentence they have to
// go and interpret.
//
// A non-200 is fatal immediately rather than retried. A server that answers with a status is up, so
// retrying only delays the report by a second and buries the status behind a timeout.
func Get(t *testing.T, addr, path, whatBound string) string {
	t.Helper()

	url := "http://" + addr + path
	client := &http.Client{Timeout: time.Second}

	var lastErr error
	for range maxPolls {
		body, err := get(t, client, url)
		if err != nil {
			lastErr = err
			time.Sleep(pollInterval)

			continue
		}

		return body
	}

	t.Fatalf("nothing ever answered %s: %v — %s", url, lastErr, whatBound)

	return ""
}

// Unreachable reports whether nothing is listening at addr, having waited out the same budget [Get]
// allows.
//
// This is the assertion for a disabled configuration, and it is worth having as its own function
// because the naive version of it is wrong: a single immediate dial fails against a listener that is
// merely slow to bind, so a test written that way passes whether the feature is off or just late.
// Waiting the full budget is the cost of the distinction.
func Unreachable(t *testing.T, addr, path string) bool {
	t.Helper()

	url := "http://" + addr + path
	client := &http.Client{Timeout: time.Second}

	for range maxPolls {
		if _, err := get(t, client, url); err == nil {
			return false
		}

		time.Sleep(pollInterval)
	}

	return true
}

// get performs one request and returns the body, or an error if the server could not be reached.
func get(t *testing.T, client *http.Client, url string) (string, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building a request for %s: %v", url, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}

	return string(b), nil
}
