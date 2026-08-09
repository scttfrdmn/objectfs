package health

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
)

// ClusterStatusPath is where cluster state is served, relative to the health listener's address.
//
// The issue asking for `objectfs cluster status` (#147) specified that the command reads
// http://localhost:8081/health/cluster, and that endpoint did not exist: [Checker.serveHealth]
// registered exactly one handler, at Config.HTTPPath, and nothing in this package mentioned clustering.
// So the issue specified a client for a server that had never been written. This is that server.
//
// A sibling of /health rather than a query parameter on it, because the two answer different questions
// with different failure modes: /health is a liveness probe a load balancer polls and reports 503 when
// the process is degraded, while this is a state report that must answer 200 even when the cluster is in
// trouble — an operator diagnosing a cluster needs the diagnosis, not a status code standing in for it.
const ClusterStatusPath = "/health/cluster"

// ClusterStatusProvider is whatever can describe the local node's cluster state.
//
// It returns `any` rather than a concrete type on purpose: this package would otherwise have to import
// internal/distributed to name it, and the dependency belongs the other way round — health is the
// transport here and has no opinion about what a cluster is. The type that owns the wire format is
// distributed.ClusterStatus, which is also what cmd/objectfs decodes into, so producer and consumer
// share one definition and cannot drift. Routing it through a stringly-typed interface in the middle
// would be the drift.
//
// Implementations must be safe to call from an HTTP handler goroutine at any time.
type ClusterStatusProvider interface {
	// ClusterStatus returns a JSON-serializable description of the cluster. It must not return nil; a
	// provider with nothing to report returns the disabled form.
	ClusterStatus() any
}

// ClusterStatusFunc adapts a function to [ClusterStatusProvider].
type ClusterStatusFunc func() any

// ClusterStatus calls f.
func (f ClusterStatusFunc) ClusterStatus() any { return f() }

// clusterProviderHolder holds the registered provider under its own mutex.
//
// Its own mutex, and not Checker.mu, because of what Checker.mu already protects: Start holds it for
// writing across the bind, and the checkLoop takes it on every round. A status request arriving during
// startup would queue behind the whole of Start for no reason, and a provider whose own snapshot takes a
// lock would have to establish an order against a mutex that serializes health checking. Nothing here
// needs to be consistent with anything Checker.mu guards.
type clusterProviderHolder struct {
	mu       sync.RWMutex
	provider ClusterStatusProvider
}

// SetClusterStatusProvider registers what /health/cluster reports, replacing any previous provider.
//
// Safe to call before or after Start: the handler reads the provider per request rather than closing
// over it, so registering after the listener is up simply means earlier requests got the unregistered
// answer. That ordering is what the adapter needs — the health monitor and the cluster manager are
// separate startup steps and either can come first.
//
// Passing nil clears it, which is a deliberate capability rather than an oversight: it is how a shutting
// down cluster stops claiming to have one.
func (c *Checker) SetClusterStatusProvider(provider ClusterStatusProvider) {
	c.cluster.mu.Lock()
	defer c.cluster.mu.Unlock()

	c.cluster.provider = provider
}

// clusterStatusProvider returns the registered provider, or nil.
func (c *Checker) clusterStatusProvider() ClusterStatusProvider {
	c.cluster.mu.RLock()
	defer c.cluster.mu.RUnlock()

	return c.cluster.provider
}

// ClusterPath is where this checker serves cluster status.
//
// It follows Config.HTTPPath when that has been moved off the default, so an operator who put the health
// endpoint at /objectfs/health finds cluster status at /objectfs/health/cluster rather than at a fixed
// path that no longer relates to it. At the default it is exactly [ClusterStatusPath], which is the
// path #147 named.
func (c *Checker) ClusterPath() string {
	if base := c.config.HTTPPath; base != "" && base != "/health" {
		return base + "/cluster"
	}

	return ClusterStatusPath
}

// unregisteredClusterStatus is the answer when no provider has been registered.
//
// Field names matching distributed.ClusterStatus's first two, so a client decodes one type either way.
// This is deliberately not an error status: a mount with `cluster.enabled: false` is the default
// configuration and by far the commonest deployment, and an endpoint that failed for it would teach
// every operator to ignore the failure. The distinction a client actually needs is between "not
// clustered", which is this, and "nothing is listening", which never reaches an HTTP handler at all.
type unregisteredClusterStatus struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
}

// serveClusterStatus writes the cluster status as JSON.
//
// Always 200 when it can answer at all, including when the cluster is unhealthy. The status code
// reports whether this endpoint worked, and the payload reports whether the cluster is well — folding
// the second into the first is what makes a 503 unreadable, since it would then mean any of "the
// process is dying", "the cluster lost members", or "clustering is off". `objectfs cluster status`
// decides its own exit code from the payload.
func (c *Checker) serveClusterStatus(w http.ResponseWriter, _ *http.Request) {
	var payload any = unregisteredClusterStatus{
		Enabled: false,
		Reason: "this instance is not running cluster coordination: set cluster.enabled in the " +
			"configuration file to join a cluster",
	}

	if provider := c.clusterStatusProvider(); provider != nil {
		if status := provider.ClusterStatus(); status != nil {
			payload = status
		}
	}

	// Encoded before the header is written, rather than streamed into the response. A marshaling
	// failure after WriteHeader(200) cannot be corrected — the client has already been told the request
	// succeeded and then gets a truncated body, which decodes as a syntax error and looks to the operator
	// like a corrupt endpoint rather than a server-side fault.
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("health: cannot serialize cluster status", "error", err)
		http.Error(w, `{"error":"cannot serialize cluster status"}`, http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(body); err != nil {
		slog.Error("health: error writing cluster status response", "error", err)
	}
}
