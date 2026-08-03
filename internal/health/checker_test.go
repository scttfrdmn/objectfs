package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// This file covers the HTTP health endpoint and the Checker accessors around it.
//
// The endpoint is what an operator, a Kubernetes liveness probe, or a load balancer actually reads,
// and before this file nothing had ever sent it a request: startHTTPServer's only coverage came from
// tests *failing* to bind, because eight tests in this package started a Monitor whose default
// Config enables the endpoint on the fixed port 8081. The first to reach it won and the others took
// the error arm. How many did was a function of goroutine scheduling, so the package measured 45.0%
// on an idle machine and 44.7% under CI's load — and its coverage floor had been pinned to the
// luckier of the two, which is why CI failed a gate that passed locally.
//
// Every test here binds port 0 and asks the listener what it got, so nothing competes for a port and
// the result does not depend on what else is running.

// newTestChecker returns a Checker with the endpoint disabled and a short timeout.
func newTestChecker(t *testing.T) *Checker {
	t.Helper()

	checker, err := NewChecker(&Config{
		Enabled:       true,
		CheckInterval: time.Hour,
		Timeout:       5 * time.Second,
		MaxFailures:   3,
		HTTPEnabled:   false,
	})
	if err != nil {
		t.Fatalf("NewChecker() error = %v", err)
	}

	return checker
}

// serveOnEphemeralPort starts the health endpoint on a port the kernel picks and returns its URL.
func serveOnEphemeralPort(t *testing.T, checker *Checker) string {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback in this environment: %v", err)
	}

	checker.config.HTTPPath = "/health"

	served := make(chan struct{})

	go func() {
		defer close(served)
		checker.serveHealth(context.WithoutCancel(t.Context()), ln)
	}()

	t.Cleanup(func() {
		_ = checker.Stop() // closes stopCh, which shuts the server down
		<-served           // and the goroutine must actually return, or -race sees it outlive the test
	})

	return "http://" + ln.Addr().String() + "/health"
}

// get issues a GET and returns the status code and body.
func get(t *testing.T, url string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	return resp.StatusCode, body
}

// TestHealthEndpointReportsStatusCodeMatchingHealth pins the contract a probe depends on.
//
// The status code is the entire interface for a Kubernetes probe or an ELB target group: neither
// reads the body. A checker with a failing critical check that answers 200 is worse than no endpoint
// at all, because it actively asserts that a broken mount is serving.
func TestHealthEndpointReportsStatusCodeMatchingHealth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		checkErr   error
		wantCode   int
		wantStatus Status
	}{
		{
			name:       "a passing check answers 200",
			checkErr:   nil,
			wantCode:   http.StatusOK,
			wantStatus: StatusHealthy,
		},
		{
			name:       "a failing check answers 503",
			checkErr:   errors.New("s3: connection refused"),
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: StatusUnhealthy,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			checker := newTestChecker(t)
			if err := checker.Start(t.Context()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}

			if err := checker.RegisterCheck("s3", "the backend", CategoryStorage, PriorityCritical,
				func(context.Context) error { return tc.checkErr }); err != nil {
				t.Fatalf("RegisterCheck() error = %v", err)
			}

			// Run the check so the endpoint has a result to report. Without this the status is
			// StatusUnknown, which is neither of the cases above.
			if _, err := checker.RunAllChecks(t.Context()); err != nil {
				t.Fatalf("RunAllChecks() error = %v", err)
			}

			url := serveOnEphemeralPort(t, checker)
			code, body := get(t, url)

			if code != tc.wantCode {
				t.Errorf("GET %s returned %d, want %d. A probe reads only the code, so this is the "+
					"whole contract: body %s", url, code, tc.wantCode, body)
			}

			var payload struct {
				Status string             `json:"status"`
				Checks map[string]*Result `json:"checks"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("the endpoint served something that is not JSON (%q): %v", body, err)
			}

			if payload.Status != string(tc.wantStatus) {
				t.Errorf("status field = %q, want %q", payload.Status, tc.wantStatus)
			}

			if _, ok := payload.Checks["s3"]; !ok {
				t.Errorf("the response names no result for the registered check, so a reader cannot "+
					"tell which component is at fault: %s", body)
			}
		})
	}
}

// TestHealthEndpointStopsServingAfterStop asserts Stop actually releases the port.
//
// The shutdown runs in a goroutine that waits on stopCh, so "Stop returned" and "the listener is
// closed" are different moments. A Checker that keeps its port after Stop makes a restart fail with
// "address already in use" — the failure mode that is hardest to attribute, because the process that
// holds the port is the one trying to bind it.
func TestHealthEndpointStopsServingAfterStop(t *testing.T) {
	t.Parallel()

	checker := newTestChecker(t)
	if err := checker.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback in this environment: %v", err)
	}

	addr := ln.Addr().String()
	checker.config.HTTPPath = "/health"

	served := make(chan struct{})

	go func() {
		defer close(served)
		checker.serveHealth(context.WithoutCancel(t.Context()), ln)
	}()

	if code, _ := get(t, "http://"+addr+"/health"); code != http.StatusOK &&
		code != http.StatusServiceUnavailable {
		t.Fatalf("the endpoint answered %d before Stop; it should be serving", code)
	}

	if err := checker.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("serveHealth did not return within 10s of Stop. The shutdown goroutine waits on " +
			"stopCh, so this means Stop closed the channel and nothing acted on it — the port stays " +
			"held for the life of the process and a restart cannot bind it")
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/health", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Error("the endpoint still answered after Stop, so the listener was never closed")
	}
}

// TestStartFailsWhenTheHealthAddressCannotBeBound pins the bind-failure contract.
//
// The contract is the opposite of what it was. The bind used to happen on a goroutine that logged the
// error and returned, so the mount came up with no health endpoint and one line in the log to say
// why — and #192, reporting the unvalidated port, called that non-fatal behavior "the right call for
// observability". It is the inversion: an operator who asked for the endpoint and did not get it finds
// out when a liveness probe starts failing, and `enabled: false` is already how you ask for no
// endpoint. So Start binds before returning and returns the error.
//
// Two cases, and they fail at different depths on purpose. An occupied address is a real conflict that
// reaches the bind. A malformed one is rejected by net.Listen's address parse — the same class as
// health_port: 99999, which used to reach here from YAML unchecked and now cannot, because
// config.Validate range-checks the port at load. Both must reach the caller as an error naming the
// address.
//
// Note the occupied case has to be occupied on the *same host*, not merely the same port. The previous
// implementation bound fmt.Sprintf(":%d") — the wildcard — so a listener holding 127.0.0.1:N did not
// stop it coming up on [::]:N, and the first draft of the old test asserted a collision and watched
// exactly that happen. That is #211 seen from the test's side.
func TestStartFailsWhenTheHealthAddressCannotBeBound(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig

	held, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback in this environment: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })

	cases := []struct {
		name string
		addr string
	}{
		{name: "the address is already in use", addr: held.Addr().String()},
		{name: "the address is not a host:port", addr: "127.0.0.1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			checker, err := NewChecker(&Config{
				Enabled:       true,
				CheckInterval: time.Hour,
				Timeout:       5 * time.Second,
				HTTPEnabled:   true,
				HTTPAddr:      tc.addr,
				HTTPPath:      "/health",
			})
			if err != nil {
				t.Fatalf("NewChecker() error = %v", err)
			}

			err = checker.Start(t.Context())
			if err == nil {
				t.Fatalf("Start succeeded with health_checks.addr %q. The mount would run with no "+
					"health endpoint, so every probe against it fails and nothing says why", tc.addr)
			}
			if !strings.Contains(err.Error(), tc.addr) {
				t.Errorf("the error does not name the address that could not be bound, which is the "+
					"one thing the operator has to change: %v", err)
			}

			// Start must not leave the checker looking started when it failed, or Stop is unreachable
			// and a caller retrying gets "already started" for a checker that never ran.
			if checker.started {
				t.Error("Start reported an error but left started true; Stop would then be the only " +
					"way to clear it and Stop is not reachable from a failed Start")
			}
		})
	}
}

// TestStartServesTheEndpointWhereConfigured is the regression test for #211 on the health side.
//
// Nothing asserted where this listener was, only that requests to it succeeded — and a wildcard bind
// answers on loopback, so a test that scrapes 127.0.0.1 passes either way. /health reports component
// names, error strings and check timings with no authentication and is on by default, so a stock
// mount published that to anything that could route to it.
func TestStartServesTheEndpointWhereConfigured(t *testing.T) {
	t.Parallel()

	// A reserved-then-released address, because the address has to be known before Start binds it.
	var lc net.ListenConfig

	probe, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback in this environment: %v", err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("releasing the reserved address: %v", err)
	}

	checker, err := NewChecker(&Config{
		Enabled:       true,
		CheckInterval: time.Hour,
		Timeout:       5 * time.Second,
		HTTPEnabled:   true,
		HTTPAddr:      addr,
		HTTPPath:      "/health",
	})
	if err != nil {
		t.Fatalf("NewChecker() error = %v", err)
	}

	if err := checker.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = checker.Stop() })

	if code, _ := get(t, "http://"+addr+"/health"); code != http.StatusOK &&
		code != http.StatusServiceUnavailable {
		t.Fatalf("the endpoint answered %d on the configured address; it should be serving", code)
	}

	// And not on an address that is this host but is not the configured one.
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("splitting %s: %v", addr, err)
	}

	routable := routableIPv4(t)
	if routable == "" {
		t.Log("no non-loopback IPv4 address on this host; only the configured-address half of this " +
			"test ran")

		return
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://"+net.JoinHostPort(routable, port)+"/health", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Errorf("/health answered on %s, which is not the configured %s — an unauthenticated "+
			"endpoint is reachable from anything that can route to this host", routable, addr)
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

// TestCheckerAccessorsReflectRegisteredChecks covers GetStats, EnableCheck, and DisableCheck.
//
// All three were at 0%. Enable/Disable are the interesting pair: RunAllChecks filters on the enabled
// flag, so a DisableCheck that silently did nothing would keep running a check an operator had turned
// off — plausibly one that is failing because the component is deliberately down.
func TestCheckerAccessorsReflectRegisteredChecks(t *testing.T) {
	t.Parallel()

	checker := newTestChecker(t)

	for _, name := range []string{"a", "b"} {
		if err := checker.RegisterCheck(name, "check "+name, CategoryCore, PriorityLow,
			func(context.Context) error { return nil }); err != nil {
			t.Fatalf("RegisterCheck(%q) error = %v", name, err)
		}
	}

	if _, err := checker.RunAllChecks(t.Context()); err != nil {
		t.Fatalf("RunAllChecks() error = %v", err)
	}

	if stats := checker.GetStats(); stats.TotalChecks == 0 {
		t.Error("GetStats reports zero total checks after two ran")
	}

	if err := checker.DisableCheck("a"); err != nil {
		t.Fatalf("DisableCheck(\"a\") error = %v", err)
	}

	results, err := checker.RunAllChecks(t.Context())
	if err != nil {
		t.Fatalf("RunAllChecks() error = %v", err)
	}

	if _, ran := results["a"]; ran {
		t.Error("a disabled check still ran. RunAllChecks filters on the enabled flag, so this " +
			"means DisableCheck did not set it — and an operator who disabled a check for a " +
			"component they took down would keep getting alerts for it")
	}

	if _, ran := results["b"]; !ran {
		t.Error("disabling one check stopped an unrelated one from running")
	}

	if err := checker.EnableCheck("a"); err != nil {
		t.Fatalf("EnableCheck(\"a\") error = %v", err)
	}

	results, err = checker.RunAllChecks(t.Context())
	if err != nil {
		t.Fatalf("RunAllChecks() error = %v", err)
	}

	if _, ran := results["a"]; !ran {
		t.Error("a re-enabled check did not run")
	}

	// Both must name the missing check rather than reporting success for something that does not
	// exist, which is the difference between a typo in a config and a silently ignored directive.
	for _, fn := range []struct {
		name string
		call func(string) error
	}{
		{"EnableCheck", checker.EnableCheck},
		{"DisableCheck", checker.DisableCheck},
	} {
		err := fn.call("no-such-check")
		if err == nil {
			t.Errorf("%s accepted a name that was never registered", fn.name)
			continue
		}

		if !strings.Contains(err.Error(), "no-such-check") {
			t.Errorf("%s error %q does not name the check it could not find", fn.name, err)
		}
	}
}
