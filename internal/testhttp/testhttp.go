// Package testhttp supports tests that assert on something a server actually served.
//
// It exists because several of the defects in v0.10.0 were invisible to any test that inspected a
// struct. The metrics collector is the clearest case: one whose HTTP listener was never bound
// gathers into its registry exactly like one whose listener was bound, the field is non-nil either
// way, and only a request over a socket tells them apart. So the missing bind survived a release,
// and deleting the call left every test in the package green.
//
// A test that wants that guarantee needs two unglamorous things — a port nothing else is using, and
// a fetch that tolerates a listener still coming up — and both were copied into
// internal/metrics/wiring_test.go and internal/adapter/metrics_wiring_test.go, where they had
// already drifted apart in their failure messages. One copy, in one place, is what keeps the
// message that explains the failure attached to the check that finds it.
package testhttp

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
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

// FreePort returns a TCP port on the loopback interface that is currently unused.
//
// Tests run in parallel with each other and with whatever else is on the machine, and the ports
// these servers default to are the popular ones: 8080 for ObjectFS's metrics endpoint, 9090 for the
// Prometheus a developer may well have running. Asking the kernel is the only way to be sure.
//
// There is a window between the close here and the bind by the caller, which nothing can eliminate
// without handing over the listener itself. Where that matters — a server that can accept a
// net.Listener — pass one instead; the servers here take a port.
func FreePort(t *testing.T) int {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("a TCP listener reported a %T address", ln.Addr())
	}
	port := addr.Port

	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}

	return port
}

// Get fetches a path from a server on the loopback interface, retrying until it answers, and fails
// the test if nothing ever does.
//
// whatBound describes what the caller expected to bind the listener, and is quoted in the failure:
// "nothing ever answered … — the adapter bound no metrics listener" is a sentence someone reading a
// CI log can act on, where a bare dial error is a sentence they have to go and interpret.
//
// A non-200 is fatal immediately rather than retried. A server that answers with a status is up, so
// retrying only delays the report by a second and buries the status behind a timeout.
func Get(t *testing.T, port int, path, whatBound string) string {
	t.Helper()

	url := "http://127.0.0.1:" + strconv.Itoa(port) + path
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

// Unreachable reports whether nothing is listening, having waited out the same budget [Get] allows.
//
// This is the assertion for a disabled configuration, and it is worth having as its own function
// because the naive version of it is wrong: a single immediate dial fails against a listener that is
// merely slow to bind, so a test written that way passes whether the feature is off or just late.
// Waiting the full budget is the cost of the distinction.
func Unreachable(t *testing.T, port, path string) bool {
	t.Helper()

	url := "http://127.0.0.1:" + port + path
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
