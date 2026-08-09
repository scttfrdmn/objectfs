package health

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// This file covers the /health/cluster endpoint, which exists because #147 specified a client for it and
// the endpoint had never been written: serveHealth registered exactly one handler, at Config.HTTPPath.
//
// The tests here send real requests over a real loopback listener rather than calling serveClusterStatus
// with an httptest.ResponseRecorder, for the same reason the rest of this package does: the mux routing is
// half of what is being asserted, and a handler called directly answers whatever path it is asked about.

// serveClusterOnEphemeralPort starts a checker's endpoints on a kernel-chosen port and returns the base
// URL, without the path — the caller decides which endpoint it is asking about.
//
// Deliberately not reusing serveOnEphemeralPort: that helper overwrites config.HTTPPath with "/health",
// which is exactly the value one test below needs to be something else.
func serveClusterOnEphemeralPort(t *testing.T, checker *Checker) string {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback in this environment: %v", err)
	}

	served := make(chan struct{})

	go func() {
		defer close(served)
		checker.serveHealth(context.WithoutCancel(t.Context()), ln)
	}()

	t.Cleanup(func() {
		_ = checker.Stop()
		<-served
	})

	return "http://" + ln.Addr().String()
}

// newClusterTestChecker returns a started checker with the endpoint disabled and httpPath as its health
// path.
//
// Started here, even though these tests supply their own listener, because Stop is a no-op returning an
// error on a checker that was never started — so an unstarted checker never closes stopCh, serveHealth
// never returns, and the cleanup in serveClusterOnEphemeralPort blocks until the test binary's timeout.
// HTTPEnabled is false, so Start does not bind anything of its own.
func newClusterTestChecker(t *testing.T, httpPath string) *Checker {
	t.Helper()

	checker, err := NewChecker(&Config{
		Enabled:       true,
		CheckInterval: time.Hour,
		Timeout:       5 * time.Second,
		MaxFailures:   3,
		HTTPEnabled:   false,
		HTTPPath:      httpPath,
	})
	if err != nil {
		t.Fatalf("NewChecker() error = %v", err)
	}

	if err := checker.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	return checker
}

// TestClusterEndpointAnswersWithoutAProviderRegistered is the commonest request this endpoint will ever
// serve.
//
// `cluster.enabled` defaults to false, so on almost every ObjectFS instance no provider is registered.
// That must be a 200 with a payload saying so, and not a 404 or a 500: a client cannot distinguish an
// error here from an instance that is failing, and an endpoint that errors on the default configuration
// teaches every operator to ignore it.
//
// The reason string is asserted to be non-empty because it is the entire difference between this and an
// empty answer. An operator who sees `"enabled": false` and nothing else has been told a fact without
// being told whether it is a fault.
func TestClusterEndpointAnswersWithoutAProviderRegistered(t *testing.T) {
	t.Parallel()

	checker := newClusterTestChecker(t, "/health")
	base := serveClusterOnEphemeralPort(t, checker)

	code, body := get(t, base+ClusterStatusPath)

	if code != http.StatusOK {
		t.Errorf("GET %s returned %d, want 200: an instance with clustering off is not broken, and an "+
			"error here cannot be told from an instance that is failing. Body: %s",
			ClusterStatusPath, code, body)
	}

	var payload struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("the endpoint served something that is not JSON (%q): %v", body, err)
	}

	if payload.Enabled {
		t.Errorf("enabled = true with no provider registered: %s", body)
	}

	if payload.Reason == "" {
		t.Error("reason is empty, so the payload states that clustering is off without saying whether " +
			"that is a configuration choice or a failure")
	}
}

// TestClusterEndpointServesTheRegisteredProvider is the path a clustered mount takes.
//
// The provider is registered *after* the server is already serving, which is not incidental: the adapter
// builds the health monitor and the cluster manager as separate startup steps, and the handler reads the
// provider per request rather than closing over it precisely so that either order works. Registering
// before serving would leave that untested and a closure-capturing implementation would pass.
func TestClusterEndpointServesTheRegisteredProvider(t *testing.T) {
	t.Parallel()

	checker := newClusterTestChecker(t, "/health")
	base := serveClusterOnEphemeralPort(t, checker)

	checker.SetClusterStatusProvider(ClusterStatusFunc(func() any {
		return map[string]any{"enabled": true, "node_id": "provider-node"}
	}))

	code, body := get(t, base+ClusterStatusPath)

	if code != http.StatusOK {
		t.Fatalf("GET %s returned %d, want 200. Body: %s", ClusterStatusPath, code, body)
	}

	var payload struct {
		Enabled bool   `json:"enabled"`
		NodeID  string `json:"node_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("not JSON (%q): %v", body, err)
	}

	if !payload.Enabled || payload.NodeID != "provider-node" {
		t.Errorf("payload = %s, want the registered provider's answer: the handler is not reading the "+
			"provider per request, so registration after Start is lost", body)
	}
}

// TestClusterEndpointAnswers200ForAnUnhealthyCluster is the deliberate difference from /health.
//
// /health returns 503 when the process is degraded, because a probe reads only the status code. This
// endpoint always returns 200 when it can answer at all, because an operator diagnosing a cluster needs
// the diagnosis rather than a status code standing in for it — and because a 503 here would mean any of
// "the process is dying", "members are missing", or "clustering is off". `objectfs cluster status`
// decides its exit code from the payload for this reason.
func TestClusterEndpointAnswers200ForAnUnhealthyCluster(t *testing.T) {
	t.Parallel()

	checker := newClusterTestChecker(t, "/health")

	// A critical check that fails, so /health is 503 and the two endpoints can be compared in one test.
	if err := checker.RegisterCheck("s3", "the backend", CategoryStorage, PriorityCritical,
		func(context.Context) error { return errors.New("s3: connection refused") }); err != nil {
		t.Fatalf("RegisterCheck() error = %v", err)
	}

	if _, err := checker.RunAllChecks(t.Context()); err != nil {
		t.Fatalf("RunAllChecks() error = %v", err)
	}

	checker.SetClusterStatusProvider(ClusterStatusFunc(func() any {
		// A cluster in trouble: two of three nodes gone.
		return map[string]any{"enabled": true, "membership": map[string]int{"total": 3, "dead": 2}}
	}))

	base := serveClusterOnEphemeralPort(t, checker)

	if code, _ := get(t, base+"/health"); code != http.StatusServiceUnavailable {
		t.Errorf("GET /health returned %d, want 503; the comparison below is only meaningful if the "+
			"process really is degraded", code)
	}

	code, body := get(t, base+ClusterStatusPath)
	if code != http.StatusOK {
		t.Errorf("GET %s returned %d for an unhealthy cluster, want 200: the status code reports "+
			"whether this endpoint worked and the payload reports whether the cluster is well. Folding "+
			"them together makes a 503 mean three different things. Body: %s",
			ClusterStatusPath, code, body)
	}

	if !json.Valid(body) {
		t.Errorf("the body is not JSON: %s", body)
	}
}

// TestClusterPathFollowsACustomizedHealthPath keeps the two endpoints related.
//
// An operator who moved health checking to /objectfs/health has moved it because something else owns
// /health — a reverse proxy route, another service on the same port. Serving cluster status at a fixed
// /health/cluster would put it back in the namespace they moved out of, and it would be the one path
// they did not know to reconfigure.
func TestClusterPathFollowsACustomizedHealthPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		httpPath string
		want     string
	}{
		{
			name:     "the default path yields the path #147 named",
			httpPath: "/health",
			want:     "/health/cluster",
		},
		{
			name:     "a moved health path takes cluster status with it",
			httpPath: "/objectfs/health",
			want:     "/objectfs/health/cluster",
		},
		{
			// An empty HTTPPath means the config was built without one. ClusterPath must still name a
			// path: returning "/cluster" would put it at the root of whatever else is on that port.
			name:     "an unset health path still yields the default",
			httpPath: "",
			want:     "/health/cluster",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			checker := newClusterTestChecker(t, tc.httpPath)

			if got := checker.ClusterPath(); got != tc.want {
				t.Errorf("ClusterPath() = %q with HTTPPath %q, want %q", got, tc.httpPath, tc.want)
			}
		})
	}
}

// TestClusterEndpointIsServedAtTheCustomizedPath closes the gap between ClusterPath and the mux.
//
// ClusterPath returning the right string and serveHealth registering that string are two facts, and the
// test above establishes only the first. Without this one, a serveHealth that registered the constant
// instead of calling ClusterPath would pass everything else in this file.
func TestClusterEndpointIsServedAtTheCustomizedPath(t *testing.T) {
	t.Parallel()

	checker := newClusterTestChecker(t, "/objectfs/health")
	checker.SetClusterStatusProvider(ClusterStatusFunc(func() any {
		return map[string]any{"enabled": true, "node_id": "moved-node"}
	}))

	base := serveClusterOnEphemeralPort(t, checker)

	if code, body := get(t, base+"/objectfs/health/cluster"); code != http.StatusOK {
		t.Errorf("GET /objectfs/health/cluster returned %d, want 200: serveHealth registered a path "+
			"other than the one ClusterPath names. Body: %s", code, body)
	}

	// And the fixed path is *not* served, so a moved endpoint is genuinely moved rather than served from
	// both places. Two live paths would mean an operator's reverse-proxy route still collides.
	if code, _ := get(t, base+ClusterStatusPath); code != http.StatusNotFound {
		t.Errorf("GET %s returned %d after the health path was moved, want 404: the endpoint is served "+
			"from two places at once", ClusterStatusPath, code)
	}
}

// TestSetClusterStatusProviderNilFallsBackToTheUnregisteredAnswer covers deregistration.
//
// A cluster manager shutting down clears the provider, and the endpoint must then report that this
// instance is not clustered rather than panicking on a nil interface or serving a stale snapshot.
func TestSetClusterStatusProviderNilFallsBackToTheUnregisteredAnswer(t *testing.T) {
	t.Parallel()

	checker := newClusterTestChecker(t, "/health")
	checker.SetClusterStatusProvider(ClusterStatusFunc(func() any {
		return map[string]any{"enabled": true}
	}))
	checker.SetClusterStatusProvider(nil)

	base := serveClusterOnEphemeralPort(t, checker)

	code, body := get(t, base+ClusterStatusPath)
	if code != http.StatusOK {
		t.Fatalf("GET %s returned %d after the provider was cleared, want 200. Body: %s",
			ClusterStatusPath, code, body)
	}

	var payload struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("not JSON (%q): %v", body, err)
	}

	if payload.Enabled {
		t.Errorf("enabled = true after the provider was cleared, so a shutting-down cluster still "+
			"claims to have one: %s", body)
	}
}

// TestClusterEndpointAnswersWhenAProviderReturnsNil guards the interface's one documented requirement.
//
// [ClusterStatusProvider] says an implementation must not return nil, and "must not" is worth enforcing
// rather than trusting: a provider that returns a nil `any` would otherwise serve the JSON literal
// `null`, which decodes into a zero-valued ClusterStatus — Enabled false, Reason empty — and the client
// would report "clustering is disabled" for a node that is in fact clustered and merely buggy.
func TestClusterEndpointAnswersWhenAProviderReturnsNil(t *testing.T) {
	t.Parallel()

	checker := newClusterTestChecker(t, "/health")
	checker.SetClusterStatusProvider(ClusterStatusFunc(func() any { return nil }))

	base := serveClusterOnEphemeralPort(t, checker)

	code, body := get(t, base+ClusterStatusPath)
	if code != http.StatusOK {
		t.Fatalf("GET %s returned %d, want 200. Body: %s", ClusterStatusPath, code, body)
	}

	var payload struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("not JSON (%q): %v", body, err)
	}

	if payload.Reason == "" {
		t.Errorf("a provider returning nil served %s, which carries no reason: the fallback answer is "+
			"what keeps this from decoding as a silently disabled cluster", body)
	}
}

// TestConcurrentProviderSwapsAndRequestsDoNotRace is a race test rather than an assertion test: it fails
// under -race on the defect and passes either way without it.
//
// The provider is registered by the adapter's startup and read by the HTTP handler goroutine, which are
// unrelated goroutines by construction. clusterProviderHolder has its own mutex — not Checker.mu, which
// Start holds across the bind — and this is what says the mutex is actually taken on both sides.
func TestConcurrentProviderSwapsAndRequestsDoNotRace(t *testing.T) {
	t.Parallel()

	checker := newClusterTestChecker(t, "/health")
	base := serveClusterOnEphemeralPort(t, checker)

	done := make(chan struct{})

	go func() {
		defer close(done)

		for i := range 200 {
			if i%2 == 0 {
				checker.SetClusterStatusProvider(ClusterStatusFunc(func() any {
					return map[string]any{"enabled": true}
				}))
			} else {
				checker.SetClusterStatusProvider(nil)
			}
		}
	}()

	for range 20 {
		if code, _ := get(t, base+ClusterStatusPath); code != http.StatusOK {
			t.Errorf("GET %s returned %d during a provider swap, want 200", ClusterStatusPath, code)
		}
	}

	<-done
}
