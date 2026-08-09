package distributed

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// TestStatusSnapshot_OmitsFieldsNothingAssigns is the assertion this whole type exists for.
//
// [ClusterStats] declares eighteen fields and a mount assigns ten of them. The other eight —
// MessagesSent, MessagesReceived, NetworkErrors, ReplicationEvents, ConsistencyViolations,
// CurrentLeader, LeaderElections and LastElectionTime — are either assigned nowhere in the repository or
// assigned only by the consensus engine, which a mount does not start. Publishing them would put zeros in
// front of an operator that read as measurements, which is #222 exactly.
//
// Asserted against the JSON tag set rather than against rendered output, because the failure mode is a
// field being *added* later by someone extending the struct from ClusterStats: a test on the human report
// would keep passing while --json grew a lying key.
func TestStatusSnapshot_OmitsFieldsNothingAssigns(t *testing.T) {
	t.Parallel()

	// These names are checked against the marshaled JSON of a live snapshot, so a field added under any
	// of them fails here whatever its Go name is.
	forbidden := map[string]string{
		"replication_events": "assigned nowhere in the repository",
		"consistency_violations": "assigned nowhere in the repository; a zero here would read as " +
			"'no violations detected' rather than 'nothing detects violations'",
		"total_operations": "only DistributeOperation increments it, and no mount path calls that",
		"successful_ops":   "same as total_operations",
		"failed_ops":       "same as total_operations",
		"avg_op_latency":   "same as total_operations",
		"last_election_time": "only SetLeader assigns it, and only consensus reaches SetLeader; a " +
			"mount never starts consensus",
	}

	cm := makeClusterWithNode(t, "omit-host")

	body := marshalStatus(t, cm.StatusSnapshot())

	for name, why := range forbidden {
		if containsJSONKey(t, body, name) {
			t.Errorf("the cluster status publishes %q, which is %s: an operator cannot tell an "+
				"unmeasured zero from a measured one, which is the defect #222 shipped", name, why)
		}
	}
}

// TestStatusSnapshot_LeadershipIsAbsentWithoutConsensus is the mockup field this deliberately drops.
//
// The issue asked for "Role: Leader" and per-peer leader=true/false. Nothing elects a leader on a mount
// path — [ClusterConfig.EnableConsensus] has no config key and startLocked leaves the engine unstarted —
// so IsLeader is false on every node of a healthy cluster. Reporting "Follower" for all of them tells an
// operator they lost an election that was never held.
//
// Nil rather than a zero struct, because a JSON consumer has to be able to tell "no leader" from "no
// election".
func TestStatusSnapshot_LeadershipIsAbsentWithoutConsensus(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, "no-consensus")

	// The mount-shaped configuration: the shared helper turns consensus on so the election suite can
	// drive it, and a mount never does. Turning it off here is the whole point of this test.
	cfg.EnableConsensus = false

	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.UpdateNodeInfo("no-consensus", nodeAlive("no-consensus"))

	if got := cm.StatusSnapshot(); got.Leadership != nil {
		t.Errorf("Leadership = %+v with consensus off, want nil: no leader is elected on a mount path, "+
			"so reporting a role would report an election that was never held", got.Leadership)
	}

	// And present when consensus *is* configured, so the field is not merely dead. Without this the test
	// above would pass against a Leadership that is never populated at all.
	cfg2 := testConfig(t, "with-consensus")
	cm2, err := NewClusterManager(cfg2)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm2.UpdateNodeInfo("with-consensus", nodeAlive("with-consensus"))

	if got := cm2.StatusSnapshot(); got.Leadership == nil {
		t.Error("Leadership = nil with EnableConsensus set, want a report: the field would be dead code")
	}
}

// TestStatusSnapshot_HitRateIsAbsentUntilACacheServesSomething is the "0% versus not measured"
// distinction, which is the constraint that shaped this whole payload.
//
// A cache that has served nothing has a hit rate of 0.0 by arithmetic, and an operator reading
// "cache_hit=0%" cannot tell that from a cache that misses every single read. The first is a mount that
// started a second ago; the second is an emergency. So the rate is nil until Requests is positive, and
// Requests is on NodeInfo for exactly this reason.
func TestStatusSnapshot_HitRateIsAbsentUntilACacheServesSomething(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "rate-host")

	// A cache that exists, holds bytes, and has answered nothing. Hits+Misses is zero, so HitRate's 0.0 is
	// arithmetic rather than measurement.
	cm.SetCache(&mockCache{stats: types.CacheStats{Size: 4096, Capacity: 8192}})

	self := requireSelf(t, cm.StatusSnapshot(), "rate-host")

	if self.Cache == nil {
		t.Fatalf("Cache = nil for a node with a cache holding 4096 bytes")
	}
	if self.Cache.Size != 4096 {
		t.Errorf("Cache.Size = %d, want 4096", self.Cache.Size)
	}
	if self.Cache.HitRate != nil {
		t.Errorf("Cache.HitRate = %v for a cache that has served no requests, want nil: a printed 0%% "+
			"is indistinguishable from a cache that misses every read", *self.Cache.HitRate)
	}

	// And it appears once something has actually been served, so the nil above is a distinction and not
	// a field that is always empty.
	cm.SetCache(&mockCache{stats: types.CacheStats{
		Size: 4096, Capacity: 8192, Hits: 3, Misses: 1, HitRate: 0.75,
	}})

	self = requireSelf(t, cm.StatusSnapshot(), "rate-host")

	if self.Cache.HitRate == nil {
		t.Fatal("Cache.HitRate = nil after 4 requests, want 0.75: the field is never populated")
	}
	if *self.Cache.HitRate != 0.75 {
		t.Errorf("Cache.HitRate = %v, want 0.75", *self.Cache.HitRate)
	}
	if self.Cache.Requests != 4 {
		t.Errorf("Cache.Requests = %d, want 4 (3 hits + 1 miss)", self.Cache.Requests)
	}
}

// TestStatusSnapshot_CacheIsAbsentWhenNoCacheIsInjected keeps "there is no cache" distinct from "the
// cache is empty".
//
// [ClusterManager.refreshLocalStats] signals the first by leaving the fields untouched rather than
// zeroing them, and this asserts the status report preserves that distinction instead of flattening it
// into a zero on the way out.
func TestStatusSnapshot_CacheIsAbsentWhenNoCacheIsInjected(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "nocache-host")

	self := requireSelf(t, cm.StatusSnapshot(), "nocache-host")

	if self.Cache != nil {
		t.Errorf("Cache = %+v with no cache injected, want nil: a reported size of 0 describes a cache "+
			"that exists and holds nothing, which is a different state", self.Cache)
	}
}

// TestStatusSnapshot_IncludesThisNodesOwnCacheFigures covers a defect found by running the command
// against a live two-node cluster.
//
// refreshLocalStats reached only gp.localNode — the struct the *alive message* is built from — so a node
// published its cache figures to every peer and never recorded them in its own membership map. The
// visible symptom was a status report that showed a peer's cache in full and the node being asked as
// "not reported", and TotalCacheSize summing every cache except the local one. The existing test for
// that sum documents the old behavior in a comment: "plus whatever the self node reports — zero here,
// since nothing refreshed it".
//
// Break [ClusterManager.refreshSelfEntry] by deleting its call in calculateClusterStats and this fails
// on both assertions.
func TestStatusSnapshot_IncludesThisNodesOwnCacheFigures(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "self-cache")
	cm.SetCache(&mockCache{stats: types.CacheStats{
		Size: 2048, Capacity: 4096, Hits: 9, Misses: 1, HitRate: 0.9,
	}})

	status := cm.StatusSnapshot()
	self := requireSelf(t, status, "self-cache")

	if self.Cache == nil {
		t.Fatal("this node reports no cache while a cache is injected: refreshLocalStats reaches the " +
			"alive message but not the membership map, so a node describes every peer's cache but not " +
			"its own")
	}
	if self.Cache.Size != 2048 {
		t.Errorf("own Cache.Size = %d, want 2048", self.Cache.Size)
	}

	// And it reaches the aggregate, which is the number an operator reads to size a cluster.
	if status.Cache.TotalSize != 2048 {
		t.Errorf("Cache.TotalSize = %d, want 2048: the local node's cache is excluded from the sum",
			status.Cache.TotalSize)
	}
	if status.Cache.NodesMeasured != 1 {
		t.Errorf("Cache.NodesMeasured = %d, want 1", status.Cache.NodesMeasured)
	}
}

// TestStatusSnapshot_ClusterHitRateSkipsNodesThatMeasuredNothing is why this average is not
// [ClusterStats.CacheHitRate].
//
// That field averages over every alive node, so a node that has served nothing contributes a 0.0 and
// drags the cluster figure down for no reason other than having just started. A two-node cluster where
// one node is at 80% and the other has answered nothing is an 80% cluster, not a 40% one.
func TestStatusSnapshot_ClusterHitRateSkipsNodesThatMeasuredNothing(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "mean-host")

	cm.mu.Lock()
	cm.nodes["mean-busy"] = &NodeInfo{
		ID: "mean-busy", Status: NodeStatusAlive, CacheSize: 100,
		CacheHitRate: 0.8, CacheRequests: 500, Metadata: map[string]string{},
	}
	// Served nothing: a rate of 0.0 that is arithmetic, not measurement.
	cm.nodes["mean-idle"] = &NodeInfo{
		ID: "mean-idle", Status: NodeStatusAlive, CacheSize: 100,
		CacheHitRate: 0, CacheRequests: 0, Metadata: map[string]string{},
	}
	cm.mu.Unlock()

	status := cm.StatusSnapshot()

	if status.Cache.HitRate == nil {
		t.Fatal("Cache.HitRate = nil while one node has measured 500 requests")
	}
	if got := *status.Cache.HitRate; got != 0.8 {
		t.Errorf("Cache.HitRate = %v, want 0.8: the idle node's arithmetic zero must not be averaged "+
			"in, which is what makes this different from ClusterStats.CacheHitRate", got)
	}
	if status.Cache.NodesMeasured != 1 {
		t.Errorf("Cache.NodesMeasured = %d, want 1: the count is what makes the mean readable",
			status.Cache.NodesMeasured)
	}
}

// TestStatusSnapshot_CapacityCountIsReportedWithTheSum keeps a partial capacity sum readable.
//
// The Redis-backed cache reports no capacity of its own, so a mixed cluster genuinely has nodes that
// cannot answer. Without the count beside the sum, a cluster where one node of three reports capacity
// looks three times fuller than it is.
func TestStatusSnapshot_CapacityCountIsReportedWithTheSum(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "cap-host")

	cm.mu.Lock()
	cm.nodes["cap-known"] = &NodeInfo{
		ID: "cap-known", Status: NodeStatusAlive, CacheSize: 10, CacheCapacity: 1000,
		CacheRequests: 1, Metadata: map[string]string{},
	}
	// Capacity zero: a Redis cache, which has none to report.
	cm.nodes["cap-unknown"] = &NodeInfo{
		ID: "cap-unknown", Status: NodeStatusAlive, CacheSize: 20, CacheCapacity: 0,
		CacheRequests: 1, Metadata: map[string]string{},
	}
	cm.mu.Unlock()

	status := cm.StatusSnapshot()

	if status.Cache.TotalCapacity != 1000 {
		t.Errorf("Cache.TotalCapacity = %d, want 1000", status.Cache.TotalCapacity)
	}
	if status.Cache.NodesReportingCapacity != 1 {
		t.Errorf("Cache.NodesReportingCapacity = %d, want 1: without this the sum is unreadable",
			status.Cache.NodesReportingCapacity)
	}
}

// TestStatusSnapshot_SeparatesSelfFromPeers checks the split the report's layout depends on, and that
// peers come back in a stable order.
//
// Map iteration in Go is randomized, so an unsorted peer list makes the command's output reshuffle
// between runs on the same cluster — which cannot be diffed, and is the first thing an operator does
// with two status outputs.
func TestStatusSnapshot_SeparatesSelfFromPeers(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "split-host")

	cm.mu.Lock()
	for _, id := range []string{"split-c", "split-a", "split-b"} {
		cm.nodes[id] = &NodeInfo{ID: id, Status: NodeStatusAlive, Metadata: map[string]string{}}
	}
	cm.mu.Unlock()

	status := cm.StatusSnapshot()

	if status.Self == nil || status.Self.ID != "split-host" {
		t.Fatalf("Self = %+v, want the local node", status.Self)
	}

	got := make([]string, 0, len(status.Peers))
	for _, p := range status.Peers {
		if p.ID == "split-host" {
			t.Error("the local node appears in Peers as well as in Self, so it is counted twice")
		}

		got = append(got, p.ID)
	}

	want := []string{"split-a", "split-b", "split-c"}
	if len(got) != len(want) {
		t.Fatalf("Peers = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Peers = %v, want %v sorted by ID: map order is randomized, so an unsorted list "+
				"makes two runs against one cluster undiffable", got, want)
		}
	}
}

// TestStatusSnapshot_MembershipMatchesTheAccessors holds the invariant #275 broke: two views of one
// membership must agree.
//
// The tallies come from GetStats and the node list from GetNodes, which are separate reads, so this is
// the assertion that keeps a third opinion about how many nodes there are from creeping in.
func TestStatusSnapshot_MembershipMatchesTheAccessors(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "tally-host")

	cm.mu.Lock()
	cm.nodes["tally-alive"] = &NodeInfo{ID: "tally-alive", Status: NodeStatusAlive, Metadata: map[string]string{}}
	cm.nodes["tally-dead"] = &NodeInfo{ID: "tally-dead", Status: NodeStatusDead, Metadata: map[string]string{}}
	cm.nodes["tally-suspect"] = &NodeInfo{ID: "tally-suspect", Status: NodeStatusSuspect, Metadata: map[string]string{}}
	cm.mu.Unlock()

	status := cm.StatusSnapshot()
	m := status.Membership

	if m.Total != 4 {
		t.Errorf("Membership.Total = %d, want 4 (self plus three)", m.Total)
	}
	if m.Alive != 2 || m.Suspect != 1 || m.Dead != 1 {
		t.Errorf("Membership = (alive %d, suspect %d, dead %d), want (2, 1, 1)", m.Alive, m.Suspect, m.Dead)
	}

	// The three must account for the total: a node whose status matches no arm is counted suspect rather
	// than silently dropped, which is a property calculateClusterStats was fixed to have.
	if m.Alive+m.Suspect+m.Dead != m.Total {
		t.Errorf("alive+suspect+dead = %d but Total = %d; a node is being counted in the total and in "+
			"none of the breakdowns", m.Alive+m.Suspect+m.Dead, m.Total)
	}

	// And the peer list plus self is the same population.
	if got := len(status.Peers) + 1; got != m.Total {
		t.Errorf("Peers+self = %d but Membership.Total = %d; the two describe one membership", got, m.Total)
	}
}

// TestStatusSnapshot_ReportsTheBoundGossipAddress asserts the address is the bound one.
//
// The configured value is "127.0.0.1:0" — what a test asks for so the kernel picks a port — and that is
// not an address any peer can be told about. A status report naming it would be telling an operator to
// check connectivity to port 0.
func TestStatusSnapshot_ReportsTheBoundGossipAddress(t *testing.T) {
	t.Parallel()

	cm, cm2 := startGossipPair(t, "addr-a", "addr-b")
	_ = cm2

	status := cm.StatusSnapshot()

	if status.GossipAddr == "" {
		t.Fatal("GossipAddr is empty for a started cluster")
	}
	if status.GossipAddr == "127.0.0.1:0" {
		t.Errorf("GossipAddr = %q, the configured value rather than the bound one: port 0 is not an "+
			"address a peer can be told about", status.GossipAddr)
	}
}

// TestStatusSnapshot_TwoNodesSeeEachOther runs the status report over the real gossip transport, which
// is the only way to check that a peer's figures survive the wire.
//
// Everything above builds a membership map directly. This one goes through startGossipPair — two clusters
// on loopback UDP — so the NodeInfo fields the report reads are ones that were marshaled, authenticated,
// sent, received and merged by UpdateNodeInfo. That path is where a field added to NodeInfo and not
// propagated in performGossip would be lost, and it is where CacheRequests and CacheCapacity had to be
// added rather than merely declared.
func TestStatusSnapshot_TwoNodesSeeEachOther(t *testing.T) {
	t.Parallel()

	cm1, cm2 := startGossipPair(t, "wire-a", "wire-b")

	// Distinct figures per node, so a report that mixed them up would fail rather than coincide.
	cm2.SetCache(&mockCache{stats: types.CacheStats{
		Size: 7777, Capacity: 9999, Hits: 30, Misses: 10, HitRate: 0.75,
	}})

	// cm2 must gossip for cm1 to learn its figures; the pair is registered one-directionally, so this
	// drives the round explicitly rather than waiting on a ticker.
	deadline := time.Now().Add(3 * time.Second)

	var peer *NodeReport

	for time.Now().Before(deadline) {
		cm2.gossip.mu.Lock()
		cm2.gossip.memberlist["wire-a"] = &GossipNode{
			Info:  &NodeInfo{ID: "wire-a", Address: cm1.gossip.LocalAddr()},
			State: StateAlive,
		}
		cm2.gossip.mu.Unlock()
		cm2.gossip.performGossip()

		for i := range cm1.StatusSnapshot().Peers {
			p := cm1.StatusSnapshot().Peers[i]
			if p.ID == "wire-b" && p.Cache != nil {
				peer = &p

				break
			}
		}

		if peer != nil {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	if peer == nil {
		t.Fatal("wire-a never learned wire-b's cache figures over gossip; a NodeInfo field that is not " +
			"propagated in performGossip is invisible to every peer")
	}

	if peer.Cache.Size != 7777 {
		t.Errorf("peer Cache.Size = %d, want 7777", peer.Cache.Size)
	}
	if peer.Cache.Capacity == nil || *peer.Cache.Capacity != 9999 {
		t.Errorf("peer Cache.Capacity = %v, want 9999: the field crossed the wire unpopulated", peer.Cache.Capacity)
	}
	if peer.Cache.Requests != 40 {
		t.Errorf("peer Cache.Requests = %d, want 40 (30 hits + 10 misses): without the denominator a "+
			"peer's 0%% hit rate cannot be told from a peer that has served nothing", peer.Cache.Requests)
	}
	if peer.Cache.HitRate == nil || *peer.Cache.HitRate != 0.75 {
		t.Errorf("peer Cache.HitRate = %v, want 0.75", peer.Cache.HitRate)
	}
}

// TestClusterStatusDisabled_IsNotAnError pins the commonest answer of all.
//
// `cluster.enabled` defaults to false, so this is what almost every ObjectFS instance reports, and it
// must carry a reason rather than looking like an empty or failed status.
func TestClusterStatusDisabled_IsNotAnError(t *testing.T) {
	t.Parallel()

	status := ClusterStatusDisabled("clustering is off")

	if status.Enabled {
		t.Error("Enabled = true for the disabled status")
	}
	if status.Reason == "" {
		t.Error("Reason is empty: an operator reading a disabled status needs to be told it is a " +
			"configuration state and not a fault")
	}
	if status.Membership.Total != 0 {
		t.Errorf("Membership.Total = %d, want 0", status.Membership.Total)
	}
}

// marshalStatus encodes a status the way the endpoint and --json both do.
func marshalStatus(t *testing.T, status *ClusterStatus) []byte {
	t.Helper()

	body, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("json.Marshal(ClusterStatus): %v", err)
	}

	return body
}

// containsJSONKey reports whether name appears as an object key anywhere in body, at any depth.
//
// Decoded rather than substring-matched, because a substring search would also match a *value* that
// happened to contain the name — and the string it is looking for is exactly the sort of thing a
// diagnostic message in the Reason field would contain.
func containsJSONKey(t *testing.T, body []byte, name string) bool {
	t.Helper()

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the status does not round-trip through JSON: %v", err)
	}

	return hasKey(decoded, name)
}

func hasKey(v any, name string) bool {
	switch typed := v.(type) {
	case map[string]any:
		for k, child := range typed {
			if k == name || hasKey(child, name) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if hasKey(child, name) {
				return true
			}
		}
	}

	return false
}

// requireSelf returns the local node's report or fails.
func requireSelf(t *testing.T, status *ClusterStatus, id string) *NodeReport {
	t.Helper()

	if status.Self == nil {
		t.Fatalf("Self = nil, want the report for %s", id)
	}
	if status.Self.ID != id {
		t.Fatalf("Self.ID = %q, want %q", status.Self.ID, id)
	}

	return status.Self
}
