package distributed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// testConfig returns a minimal ClusterConfig suitable for unit tests.
// ListenAddr ":0" lets the OS assign an ephemeral UDP port.
// ElectionTimeout is set long to prevent spurious elections during tests.
//
// It needs a *testing.T because gossip authentication (#206) fails closed: a cluster cannot be
// constructed without a cluster secret, so every test that builds one needs a secret file, and that
// file's lifetime is the test's.
func testConfig(t *testing.T, nodeID string) *ClusterConfig {
	t.Helper()

	return &ClusterConfig{
		NodeID:            nodeID,
		ListenAddr:        "127.0.0.1:0",
		AdvertiseAddr:     "127.0.0.1:0",
		ElectionTimeout:   30 * time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
		SecretFile:        writeTestSecret(t),

		// On here and off for a mount, which is the asymmetry this package's tests exist to hold: the
		// consensus code is exercised deliberately by the election suite, and is not started by
		// [ClusterManager.Start] when a filesystem enables clustering. See
		// [ClusterConfig.EnableConsensus].
		//
		// Set in the shared helper rather than in each election test so that turning it off here is what
		// a reviewer does to check the gate is real — see
		// TestClusterManager_Start_DoesNotStartConsensusUnlessAskedTo, which asserts the mount-shaped
		// configuration directly.
		EnableConsensus: true,
	}
}

// writeTestSecret writes a cluster secret to a file in the test's own temporary directory and
// returns its path.
//
// A file rather than OBJECTFS_CLUSTER_SECRET: the environment is process-wide, and these tests run
// with t.Parallel(), so a test that set and unset the variable would race every other test's cluster
// construction. t.TempDir is already mode 0700 and is removed when the test finishes.
func writeTestSecret(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "cluster.secret")

	// Long enough to pass minSecretLen. Fixed rather than random because a test that fails should
	// fail the same way twice.
	if err := os.WriteFile(path, []byte(strings.Repeat("a", minSecretLen)), 0o600); err != nil {
		t.Fatalf("writing the test cluster secret: %v", err)
	}

	return path
}

// TestNewClusterManager_NilConfig verifies that a nil config is accepted and
// defaults are applied.
func TestNewClusterManager_NilConfig(t *testing.T) {
	// A nil config still needs a secret, and the only source that does not require a config field
	// is the environment. t.Setenv forbids t.Parallel, which is why this test does not call it.
	t.Setenv(ClusterSecretEnv, strings.Repeat("b", minSecretLen))

	cm, err := NewClusterManager(nil)
	if err != nil {
		t.Fatalf("NewClusterManager(nil): %v", err)
	}
	if cm == nil {
		t.Fatal("expected non-nil ClusterManager")
	}
}

// TestNewClusterManager_GeneratesNodeID verifies that an empty NodeID results
// in an auto-generated ID with the "node-" prefix.
func TestNewClusterManager_GeneratesNodeID(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(&ClusterConfig{
		ListenAddr:    "127.0.0.1:0",
		AdvertiseAddr: "127.0.0.1:0",
		SecretFile:    writeTestSecret(t),
	})
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	id := cm.GetNodeID()
	if id == "" {
		t.Fatal("expected non-empty generated node ID")
	}
	if !strings.HasPrefix(id, "node-") {
		t.Errorf("generated node ID %q does not start with 'node-'", id)
	}
}

// TestNewClusterManager_PreservesExplicitNodeID verifies that a provided NodeID
// is not overwritten.
func TestNewClusterManager_PreservesExplicitNodeID(t *testing.T) {
	t.Parallel()
	const want = "explicit-node-id"
	cm, err := NewClusterManager(testConfig(t, want))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	if got := cm.GetNodeID(); got != want {
		t.Errorf("GetNodeID() = %q, want %q", got, want)
	}
}

// TestNewClusterManager_ConfigDefaults verifies that zero-valued config fields
// are filled with sensible defaults.
func TestNewClusterManager_ConfigDefaults(t *testing.T) {
	t.Parallel()
	// All fields zero except the two that have no default: the node ID under test, and the cluster
	// secret, which fails closed rather than defaulting (#206).
	cfg := &ClusterConfig{NodeID: "n1", SecretFile: writeTestSecret(t)}
	_, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	// applyConfigDefaults runs in-place on the passed config.
	if cfg.ListenAddr == "" {
		t.Error("ListenAddr should have a default")
	}
	if cfg.ElectionTimeout == 0 {
		t.Error("ElectionTimeout should have a default")
	}
	if cfg.ReplicationFactor == 0 {
		t.Error("ReplicationFactor should have a default")
	}
	if cfg.OperationTimeout == 0 {
		t.Error("OperationTimeout should have a default")
	}
}

// TestNewClusterManager_DefaultMaxGossipPacketHoldsAThreeNodeSync asserts the default against the
// thing it has to be big enough for, rather than against its own value.
//
// A test comparing MaxGossipPacket to 8192 would pass on the 1024 that could not form a three-node
// cluster, if someone had written it when 1024 was the constant — it would only ever say that the
// constant equals itself. So this builds the sync message a three-node cluster actually sends and
// asserts it fits, which is the property #277 was about. It is also the property that would break
// silently if NodeInfo grew a field.
func TestNewClusterManager_DefaultMaxGossipPacketHoldsAThreeNodeSync(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(&ClusterConfig{NodeID: "fit-host", SecretFile: writeTestSecret(t)})
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	gp := cm.gossip

	gp.mu.Lock()
	for i := range 2 {
		// Realistic identifiers, not "a" and "b": a hostname is the length that decides whether this
		// fits, and a test using two-character IDs would pass at almost any limit.
		id := fmt.Sprintf("objectfs-node-%02d.cluster.example.edu", i)
		gp.memberlist[id] = &GossipNode{
			Info: &NodeInfo{
				ID: id, Address: fmt.Sprintf("10.20.30.%d:8080", i+1),
				Status: NodeStatusAlive, LastSeen: time.Now(), Version: "0.11.0",
				Metadata: map[string]string{},
			},
			Incarnation: 1, State: StateAlive, StateChange: time.Now(),
		}
	}
	members := len(gp.memberlist)
	chunks, err := gp.marshalSyncChunksLocked()
	gp.mu.Unlock()

	if err != nil {
		t.Fatalf("marshalSyncChunksLocked: %v", err)
	}
	if members != 3 {
		t.Fatalf("memberlist has %d members, want 3 (self plus two peers)", members)
	}
	if len(chunks) != 1 {
		t.Errorf("a three-member sync took %d datagrams at the %d-byte default, want 1: the default "+
			"cannot carry the smallest cluster that needs a quorum", len(chunks), defaultMaxGossipPacket)
	}
}

// TestClusterManager_Start_DoesNotStartConsensusUnlessAskedTo is the assertion behind #139's decision
// to wire the gossip and cache halves of clustering into a mount and leave Raft unstarted.
//
// Coordination here is compare-and-swap against S3, decided by the store on one request, and what a
// mount enables clustering for — membership, invalidation, and the key announcements that tell a cold
// node which objects are worth warming — consults no leader. Starting elections anyway would put
// quorum on the path a filesystem read takes in order to decide nothing that path asks about.
//
// The configuration here is deliberately the *mount's* shape rather than testConfig's: testConfig sets
// EnableConsensus so the election suite keeps working, so a test built on it could not observe this
// gate at all. What is asserted is that no election happens — a leaderless cluster after several
// election timeouts' worth of wall clock — rather than that a flag is false, because the flag being
// false is the code under test and not evidence about it.
func TestClusterManager_Start_DoesNotStartConsensusUnlessAskedTo(t *testing.T) {
	t.Parallel()

	// Short timeouts so a running election loop would have fired repeatedly by the time this checks.
	cfg := testConfig(t, "mount-shaped")
	cfg.EnableConsensus = false
	cfg.ElectionTimeout = 20 * time.Millisecond
	cfg.HeartbeatInterval = 10 * time.Millisecond

	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	if err := cm.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = cm.Stop() })

	// A one-node cluster elects itself within one timeout when the loop is running —
	// TestConsensusEngine_SingleNodeLeadershipIsStable relies on exactly that — so 500ms is ~25
	// timeouts of margin.
	time.Sleep(500 * time.Millisecond)

	if cm.consensus.IsLeader() {
		t.Error("consensus elected a leader on a mount-shaped configuration: enabling clustering for " +
			"cache coordination must not start Raft")
	}
	if state := cm.consensus.GetCurrentState(); state != StateFollower {
		t.Errorf("consensus state is %v, want %v: the election loop is running", state, StateFollower)
	}
	if term := cm.consensus.GetCurrentTerm(); term != 0 {
		t.Errorf("consensus term advanced to %d, want 0: a term only advances by standing for election",
			term)
	}

	// And the half that a mount *does* want is running, so this is not passing because Start failed
	// early or because clustering is off altogether.
	if cm.gossip.LocalAddr() == "" {
		t.Error("gossip is not listening, so this asserts nothing about consensus specifically")
	}
	if leader := cm.GetLeader(); leader != "" {
		t.Errorf("cluster reports leader %q with consensus off", leader)
	}
}

// TestClusterManager_InitialState verifies the state of a newly created manager
// before Start is called.
func TestClusterManager_InitialState(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "init-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	if cm.IsLeader() {
		t.Error("IsLeader() should be false before any election")
	}
	if got := cm.GetLeader(); got != "" {
		t.Errorf("GetLeader() = %q, want empty string", got)
	}
	if got := cm.GetNodes(); len(got) != 0 {
		t.Errorf("GetNodes() returned %d nodes, want 0 before Start()", len(got))
	}
}

// TestClusterManager_UpdateNodeInfo_Add verifies that a new node is added to
// the nodes map.
func TestClusterManager_UpdateNodeInfo_Add(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "node-a"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	info := &NodeInfo{
		ID:       "node-b",
		Address:  "127.0.0.1:9001",
		Status:   NodeStatusAlive,
		LastSeen: time.Now(),
		Metadata: map[string]string{"role": "worker"},
	}
	cm.UpdateNodeInfo("node-b", info)

	nodes := cm.GetNodes()
	if _, ok := nodes["node-b"]; !ok {
		t.Fatal("UpdateNodeInfo: node-b not found in nodes map")
	}
	if nodes["node-b"].Address != "127.0.0.1:9001" {
		t.Errorf("Address = %q, want 127.0.0.1:9001", nodes["node-b"].Address)
	}
	if nodes["node-b"].Metadata["role"] != "worker" {
		t.Errorf("Metadata[role] = %q, want 'worker'", nodes["node-b"].Metadata["role"])
	}
}

// TestClusterManager_UpdateNodeInfo_Update verifies that an existing node's
// fields are updated without creating a duplicate entry.
func TestClusterManager_UpdateNodeInfo_Update(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "node-a"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	first := &NodeInfo{ID: "node-b", Address: "1.2.3.4:8080", Status: NodeStatusAlive, LastSeen: time.Now(), Metadata: map[string]string{}}
	cm.UpdateNodeInfo("node-b", first)

	second := &NodeInfo{ID: "node-b", Address: "1.2.3.4:8080", Status: NodeStatusSuspect, LastSeen: time.Now(), CPUUsage: 42.0, Metadata: map[string]string{}}
	cm.UpdateNodeInfo("node-b", second)

	nodes := cm.GetNodes()
	if len(nodes) != 1 {
		t.Errorf("expected 1 node, got %d", len(nodes))
	}
	if nodes["node-b"].Status != NodeStatusSuspect {
		t.Errorf("Status = %q, want %q", nodes["node-b"].Status, NodeStatusSuspect)
	}
	if nodes["node-b"].CPUUsage != 42.0 {
		t.Errorf("CPUUsage = %.1f, want 42.0", nodes["node-b"].CPUUsage)
	}
}

// TestClusterManager_GetNodes_DeepCopy verifies that mutating the returned map
// does not affect the internal state.
func TestClusterManager_GetNodes_DeepCopy(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "node-a"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.UpdateNodeInfo("node-b", &NodeInfo{ID: "node-b", Status: NodeStatusAlive, LastSeen: time.Now(), Metadata: map[string]string{}})

	copy1 := cm.GetNodes()
	// Mutate the copy
	copy1["node-b"].Status = NodeStatusDead
	copy1["node-b"].Metadata["injected"] = "true"
	delete(copy1, "node-b")

	copy2 := cm.GetNodes()
	if _, ok := copy2["node-b"]; !ok {
		t.Error("internal node was deleted via mutated GetNodes() copy")
	}
	if copy2["node-b"].Status == NodeStatusDead {
		t.Error("internal Status was mutated via GetNodes() copy")
	}
	if _, ok := copy2["node-b"].Metadata["injected"]; ok {
		t.Error("internal Metadata was mutated via GetNodes() copy")
	}
}

// TestClusterManager_SetLeader_OtherNode verifies that SetLeader with another
// node's ID marks that node as leader but does not set isLeader.
func TestClusterManager_SetLeader_OtherNode(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "self"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	cm.SetLeader("other")

	if got := cm.GetLeader(); got != "other" {
		t.Errorf("GetLeader() = %q, want %q", got, "other")
	}
	if cm.IsLeader() {
		t.Error("IsLeader() should be false when another node is leader")
	}
}

// TestClusterManager_SetLeader_SelfNode verifies that SetLeader with the local
// node ID sets isLeader = true.
func TestClusterManager_SetLeader_SelfNode(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "self"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	cm.SetLeader("self")

	if !cm.IsLeader() {
		t.Error("IsLeader() should be true when self is leader")
	}
	if got := cm.GetLeader(); got != "self" {
		t.Errorf("GetLeader() = %q, want %q", got, "self")
	}
}

// TestClusterManager_SetLeader_UpdatesStats verifies that SetLeader increments
// the election counter and records the leader in stats.
func TestClusterManager_SetLeader_UpdatesStats(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "self"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	before := cm.GetStats()
	cm.SetLeader("leader-x")
	after := cm.GetStats()

	if after.LeaderElections != before.LeaderElections+1 {
		t.Errorf("LeaderElections: got %d, want %d", after.LeaderElections, before.LeaderElections+1)
	}
	if after.CurrentLeader != "leader-x" {
		t.Errorf("CurrentLeader = %q, want %q", after.CurrentLeader, "leader-x")
	}
	if after.LastElectionTime.IsZero() {
		t.Error("LastElectionTime should not be zero after SetLeader")
	}
}

// TestClusterManager_RemoveNode verifies that a node is removed from the map.
func TestClusterManager_RemoveNode(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "self"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.UpdateNodeInfo("remove-me", &NodeInfo{ID: "remove-me", Status: NodeStatusAlive, LastSeen: time.Now(), Metadata: map[string]string{}})

	cm.RemoveNode("remove-me")

	if _, ok := cm.GetNodes()["remove-me"]; ok {
		t.Error("RemoveNode: node still present after removal")
	}
}

// TestClusterManager_RemoveNode_WasLeader verifies that removing the current
// leader clears the leadership state.
func TestClusterManager_RemoveNode_WasLeader(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "self"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.UpdateNodeInfo("leader-x", &NodeInfo{ID: "leader-x", Status: NodeStatusAlive, LastSeen: time.Now(), Metadata: map[string]string{}})
	cm.SetLeader("leader-x")

	cm.RemoveNode("leader-x")

	if got := cm.GetLeader(); got != "" {
		t.Errorf("GetLeader() = %q after leader removal, want empty", got)
	}
	if cm.IsLeader() {
		t.Error("IsLeader() should be false after leader node removed")
	}
}

// TestClusterManager_GetStats_Initial verifies that stats are zero-valued on
// a freshly created manager.
func TestClusterManager_GetStats_Initial(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "stats-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	stats := cm.GetStats()
	if stats == nil {
		t.Fatal("GetStats() returned nil")
	}
	if stats.TotalOperations != 0 {
		t.Errorf("TotalOperations = %d, want 0", stats.TotalOperations)
	}
	if stats.LeaderElections != 0 {
		t.Errorf("LeaderElections = %d, want 0", stats.LeaderElections)
	}
}

// TestClusterManager_GetCoordinator_NotNil verifies that GetCoordinator returns
// a non-nil coordinator satisfying the DistributedCoordinator interface.
func TestClusterManager_GetCoordinator_NotNil(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "coord-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	if cm.GetCoordinator() == nil {
		t.Error("GetCoordinator() returned nil")
	}
}

// TestClusterManager_DistributeOperation_NoNodes verifies that DistributeOperation
// returns an error when no alive nodes are present.
func TestClusterManager_DistributeOperation_NoNodes(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "dist-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	op := &DistributedOperation{
		Type: OpTypeGet,
		Key:  "test-key",
	}
	_, err = cm.DistributeOperation(context.Background(), op)
	if err == nil {
		t.Error("DistributeOperation with no alive nodes should return error")
	}
}

// TestClusterManager_StartStop verifies the full lifecycle without panics.
func TestClusterManager_StartStop(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "lifecycle-node")
	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	ctx := t.Context()

	if err := cm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// After Start, self must be registered.
	nodes := cm.GetNodes()
	if _, ok := nodes["lifecycle-node"]; !ok {
		t.Error("self node should be in nodes map after Start")
	}

	if err := cm.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// ── Statistics are current when Start returns (#275) ──────────────────────────

// TestClusterManager_StartComputesStatsImmediately verifies that GetStats describes the cluster as soon
// as Start returns, with no sleep.
//
// The absence of a sleep is the test. Until v0.11.0 the statistics were computed only by updateStats'
// five-second ticker, so for the first five seconds of every process GetStats reported zero nodes while
// GetNodes over the same membership reported one. Both are public accessors and the one that
// disagreed with the truth is the one an operator, a health check, and the three tagged tests in
// tests/distributed_test.go read (#275). A test that slept before asserting could not tell the fix from
// the defect.
func TestClusterManager_StartComputesStatsImmediately(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "stats-at-start"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	if err := cm.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = cm.Stop() }()

	stats := cm.GetStats()

	if stats.TotalNodes != 1 {
		t.Errorf("TotalNodes = %d immediately after Start, want 1", stats.TotalNodes)
	}
	if stats.AliveNodes != 1 {
		t.Errorf("AliveNodes = %d immediately after Start, want 1", stats.AliveNodes)
	}

	// The two accessors must agree, which is the invariant the defect broke rather than either value
	// being wrong in isolation.
	if got := len(cm.GetNodes()); stats.TotalNodes != got {
		t.Errorf("GetStats().TotalNodes = %d but GetNodes() has %d entries; the two describe the same "+
			"membership and must agree", stats.TotalNodes, got)
	}
}

// ── Local node statistics (#132) ──────────────────────────────────────────────

// TestRefreshLocalStats_PopulatesMemoryAndOperations verifies that this node measures its own memory
// and operation count rather than advertising the zeros it was constructed with.
//
// Until v0.11.0 the six resource fields on the local NodeInfo were set at construction and never
// written again, so every node advertised itself as idle with an empty cache for the life of the
// process — and a load-aware strategy comparing those numbers was comparing identical zeros.
func TestRefreshLocalStats_PopulatesMemoryAndOperations(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "stats-refresh")

	cm.stats.mu.Lock()
	cm.stats.TotalOperations = 42
	cm.stats.mu.Unlock()

	var got NodeInfo
	cm.refreshLocalStats(&got)

	// A ratio of live heap to obtained heap, so strictly between 0 and 1 in a running process. Bounds
	// rather than a value: the exact figure depends on what the test binary has allocated, and an
	// assertion on it would be a test of the Go allocator.
	if got.MemoryUsage <= 0 || got.MemoryUsage > 1 {
		t.Errorf("MemoryUsage = %v, want a fraction in (0, 1]", got.MemoryUsage)
	}
	if got.Operations != 42 {
		t.Errorf("Operations = %d, want 42", got.Operations)
	}
}

// TestRefreshLocalStats_ReadsTheInjectedCache verifies that the cache figures come from the cache
// rather than being left at zero.
func TestRefreshLocalStats_ReadsTheInjectedCache(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "stats-cache")
	cm.SetCache(&mockCache{stats: types.CacheStats{Size: 8192, HitRate: 0.75}})

	var got NodeInfo
	cm.refreshLocalStats(&got)

	if got.CacheSize != 8192 {
		t.Errorf("CacheSize = %d, want 8192", got.CacheSize)
	}
	if got.CacheHitRate != 0.75 {
		t.Errorf("CacheHitRate = %v, want 0.75", got.CacheHitRate)
	}
}

// TestRefreshLocalStats_SaturatesCacheRequestsRatherThanWrapping pins the clamp on the uint64 → int64
// conversion behind CacheRequests.
//
// Unreachable in a real process — 2^63 cache operations is 292 years at a billion per second — and
// tested anyway, because the alternative to the clamp was a lint suppression and a suppression cannot
// be tested at all. gosec flags the conversion (G115); the reason to fix it rather than silence it is
// what happens if it ever does wrap. A negative CacheRequests reads as "nothing has asked yet" to the
// `requests > 0` guard that decides whether a hit rate is reported, so the busiest cache in the
// cluster would report `hit=not measured` — a wrong answer dressed as an honest absence, which is the
// failure mode this whole status surface is built to avoid.
//
// Two cases, because the sum is uint64 arithmetic and wraps on its own: a total above MaxInt64 that
// does not wrap, and Hits+Misses overflowing uint64 entirely. A check placed after the cast would miss
// the second, having already lost the overflow.
func TestRefreshLocalStats_SaturatesCacheRequestsRatherThanWrapping(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		hits, misses uint64
	}{
		{"above MaxInt64", math.MaxInt64, 10},
		{"sum wraps uint64", math.MaxUint64, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cm := makeClusterWithNode(t, "stats-saturate")
			cm.SetCache(&mockCache{stats: types.CacheStats{Hits: tc.hits, Misses: tc.misses}})

			var got NodeInfo
			cm.refreshLocalStats(&got)

			if got.CacheRequests != math.MaxInt64 {
				t.Errorf("CacheRequests = %d, want MaxInt64 (%d).\nHits=%d Misses=%d. A value that is "+
					"not the clamp means the conversion wrapped; if it is negative, the hit-rate guard "+
					"will report this cache as having served nothing.",
					got.CacheRequests, int64(math.MaxInt64), tc.hits, tc.misses)
			}
		})
	}
}

// TestRefreshLocalStats_LeavesCacheFieldsAloneWithNoCache verifies that an absent cache is
// distinguishable from an empty one.
//
// Zero is a meaningful cache size, so writing it for "there is no cache" would make a node with no
// cache look like the emptiest and therefore most attractive one to a size-aware strategy.
func TestRefreshLocalStats_LeavesCacheFieldsAloneWithNoCache(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "stats-nocache")

	got := NodeInfo{CacheSize: -1, CacheHitRate: -1}
	cm.refreshLocalStats(&got)

	if got.CacheSize != -1 || got.CacheHitRate != -1 {
		t.Errorf("cache fields = (%d, %v), want them untouched at (-1, -1) when no cache is injected",
			got.CacheSize, got.CacheHitRate)
	}
}

// TestRefreshLocalStats_LeavesUnmeasurableFieldsAtZero verifies that CPU, disk, and bandwidth are not
// filled with a proxy from an unrelated quantity.
//
// Each needs a platform-specific source this repository does not have. An obviously-zero CPUUsage
// prompts someone to implement it; one carrying heap fragmentation looks like a measurement and gets
// used as one. #132 proposed HeapInuse/HeapSys for CPUUsage, which is the same expression already
// assigned to MemoryUsage — so this test also pins the two fields apart.
func TestRefreshLocalStats_LeavesUnmeasurableFieldsAtZero(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "stats-honest")

	var got NodeInfo
	cm.refreshLocalStats(&got)

	if got.CPUUsage != 0 {
		t.Errorf("CPUUsage = %v, want 0: no CPU source exists in this repository, and a stand-in "+
			"reads as a measurement", got.CPUUsage)
	}
	if got.DiskUsage != 0 {
		t.Errorf("DiskUsage = %v, want 0", got.DiskUsage)
	}
	if got.NetworkBandwidth != 0 {
		t.Errorf("NetworkBandwidth = %d, want 0", got.NetworkBandwidth)
	}
}

// TestPerformGossipRefreshesTheAdvertisedStats verifies that the figures a peer receives are the
// current ones, end to end through the gossip round.
//
// refreshLocalStats being correct is not enough: it has to be called, and its output has to reach the
// struct that performGossip marshals. Asserting through gp.localNode covers the assignment the unit
// tests above cannot see.
func TestPerformGossipRefreshesTheAdvertisedStats(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "advertise-host")
	cm.SetCache(&mockCache{stats: types.CacheStats{Size: 4096, HitRate: 0.5}})

	cm.stats.mu.Lock()
	cm.stats.TotalOperations = 7
	cm.stats.mu.Unlock()

	gp := cm.gossip

	// A peer is required, or performGossip returns before sending anything.
	gp.mu.Lock()
	gp.memberlist["advertise-peer"] = &GossipNode{
		Info:        &NodeInfo{ID: "advertise-peer", Address: "127.0.0.1:1", Metadata: map[string]string{}},
		Incarnation: 1,
		State:       StateAlive,
	}
	gp.mu.Unlock()

	gp.performGossip()

	gp.mu.RLock()
	got := *gp.localNode
	gp.mu.RUnlock()

	if got.CacheSize != 4096 {
		t.Errorf("advertised CacheSize = %d, want 4096", got.CacheSize)
	}
	if got.CacheHitRate != 0.5 {
		t.Errorf("advertised CacheHitRate = %v, want 0.5", got.CacheHitRate)
	}
	if got.Operations != 7 {
		t.Errorf("advertised Operations = %d, want 7", got.Operations)
	}
	if got.MemoryUsage <= 0 {
		t.Errorf("advertised MemoryUsage = %v, want > 0", got.MemoryUsage)
	}
}

// TestCalculateClusterStats_SumsCacheSizeAcrossAliveNodes verifies that the accumulator discarded
// with `_ =` until v0.11.0 now reaches a field.
//
// Summed, not averaged: the question a total cache size answers is "how much is cached", and an
// average answers nothing that the per-node figures do not already. The hit rate beside it is averaged
// for the mirror-image reason, which is why the two are combined differently.
func TestCalculateClusterStats_SumsCacheSizeAcrossAliveNodes(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "sum-host")

	cm.mu.Lock()
	cm.nodes["sum-a"] = &NodeInfo{ID: "sum-a", Status: NodeStatusAlive, CacheSize: 1000, CacheHitRate: 0.4, Metadata: map[string]string{}}
	cm.nodes["sum-b"] = &NodeInfo{ID: "sum-b", Status: NodeStatusAlive, CacheSize: 3000, CacheHitRate: 0.6, Metadata: map[string]string{}}
	// Dead nodes contribute nothing: their cache is not reachable, so counting it would overstate what
	// the cluster can serve without going to S3.
	cm.nodes["sum-dead"] = &NodeInfo{ID: "sum-dead", Status: NodeStatusDead, CacheSize: 9_000_000, Metadata: map[string]string{}}
	cm.mu.Unlock()

	cm.calculateClusterStats()

	stats := cm.GetStats()

	// 1000 + 3000, plus whatever the self node reports. That used to be zero because nothing refreshed
	// the self entry at all — a defect, since a node then omitted its own cache from the cluster total.
	// [ClusterManager.refreshSelfEntry] now refreshes it, and it is still zero here for the honest
	// reason: no cache is injected into this manager, so there is nothing to report.
	// TestStatusSnapshot_IncludesThisNodesOwnCacheFigures covers the case where there is.
	if stats.TotalCacheSize != 4000 {
		t.Errorf("TotalCacheSize = %d, want 4000 (dead node's 9000000 excluded)", stats.TotalCacheSize)
	}
	if stats.CacheHitRate <= 0 {
		t.Errorf("CacheHitRate = %v, want > 0", stats.CacheHitRate)
	}
}

// TestCalculateClusterStats_TalliesUnderTheLock is a race test, not an assertion test: under -race it
// fails on the defect and passes after it, and without -race it passes either way.
//
// calculateClusterStats used to maps.Copy cm.nodes and walk the copy after unlocking. maps.Copy on a
// map[string]*NodeInfo copies the pointers, so every field read below aliased a struct UpdateNodeInfo
// writes in place from the gossip receive goroutine (#278). What makes it a real defect rather than a
// stale read is that Status is read to classify the node while CacheSize and CacheHitRate are summed
// from it — a node counted alive on a torn read contributes figures from a different moment, and
// TotalCacheSize is what a size-aware balancer routes on.
//
// The loop runs long enough for the two goroutines to interleave many times; -race needs the accesses
// to actually overlap, not merely to be possible.
func TestCalculateClusterStats_TalliesUnderTheLock(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "race-host")

	cm.mu.Lock()
	cm.nodes["race-peer"] = &NodeInfo{ID: "race-peer", Status: NodeStatusAlive, Metadata: map[string]string{}}
	cm.mu.Unlock()

	const rounds = 500
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range rounds {
			// The same path handleAliveMessage takes: a fresh NodeInfo for a node already known, whose
			// fields are copied onto the existing record in place.
			cm.UpdateNodeInfo("race-peer", &NodeInfo{
				ID:           "race-peer",
				Status:       NodeStatusAlive,
				LastSeen:     time.Now(),
				CacheSize:    int64(i),
				CacheHitRate: float64(i%100) / 100,
				Metadata:     map[string]string{},
			})
		}
	}()

	for range rounds {
		cm.calculateClusterStats()
	}
	<-done

	// Sanity, so a test that raced but computed nothing does not look like a pass: the peer and the
	// self node are both alive.
	if stats := cm.GetStats(); stats.AliveNodes != 2 {
		t.Errorf("AliveNodes = %d, want 2", stats.AliveNodes)
	}
}

// TestUnrecognizedNodeStatus_IsCountedAndReaped covers a status that no switch arm in this file names.
//
// NodeStatus is a string, and UpdateNodeInfo assigns info.Status directly from a json.Unmarshal of a
// gossip message — so the value here does not have to be one of the five constants. A peer running a
// different version, or anything at all that can reach the gossip port, can put an arbitrary string in
// this field. NodeStatusJoining and NodeStatusLeaving arrive at the same place: both are declared in
// cluster.go and assigned nowhere in the repository, so they are unrecognized in practice too.
//
// Two things went wrong before the default arms, and this asserts both because they are independent:
// calculateClusterStats counted such a node in TotalNodes and in none of alive/suspect/dead, so the
// breakdown did not add up to the total; and performHealthChecks never reaped it, at any staleness,
// because the switch fell through. Measured on the defect: total=2 alive=1 suspect=0 dead=0, and a node
// last seen an hour ago still reporting its original status afterwards.
//
// Reaping matters beyond the tally. Quorum in consensus.go tests `== NodeStatusAlive` in three places,
// so a node stuck at an unrecognized status is permanently invisible to elections while remaining in
// the membership map — it can neither vote nor be counted, and nothing ever removes it.
func TestUnrecognizedNodeStatus_IsCountedAndReaped(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "unknown-status-host")

	// Deliberately not one of the five constants: this is what arrives over the wire from a peer this
	// build does not know about.
	const fromTheWire = NodeStatus("from-the-wire")

	cm.mu.Lock()
	cm.nodes["martian"] = &NodeInfo{
		ID:        "martian",
		Status:    fromTheWire,
		LastSeen:  time.Now().Add(-time.Hour),
		CacheSize: 222,
		Metadata:  map[string]string{},
	}
	cm.mu.Unlock()

	cm.calculateClusterStats()

	stats := cm.GetStats()
	if got, want := stats.AliveNodes+stats.SuspectNodes+stats.DeadNodes, stats.TotalNodes; got != want {
		t.Errorf("alive+suspect+dead = %d, want TotalNodes = %d: a node was counted in the total and in "+
			"none of the breakdowns (alive=%d suspect=%d dead=%d)",
			got, want, stats.AliveNodes, stats.SuspectNodes, stats.DeadNodes)
	}

	// Its cache is deliberately excluded, for the reason a dead node's is: an unknown state is not
	// capacity this cluster can serve from. The self node reports zero, so the total is zero.
	if stats.TotalCacheSize != 0 {
		t.Errorf("TotalCacheSize = %d, want 0 (an unrecognized node's 222 must not be summed)",
			stats.TotalCacheSize)
	}

	cm.performHealthChecks(t.Context())

	cm.mu.Lock()
	got := cm.nodes["martian"].Status
	cm.mu.Unlock()

	if got != NodeStatusSuspect {
		t.Errorf("status after a health check on a node last seen an hour ago = %q, want %q: an "+
			"unrecognized status must be reapable, or the node stays in the membership map forever",
			got, NodeStatusSuspect)
	}
}

// ── Operation success accounting (#269) ───────────────────────────────────────
//
// These assert on the pair (err, result.Success) rather than on either alone. The defect they pin
// was not that either value was wrong — each executor set result.Success correctly and every
// executor returned a nil error — it was that the two disagreed, and DistributeOperation classified
// on the one that carried no information. A test checking only result.Success passes on the defect,
// and so does a test checking only that FailedOps is reachable.

// TestDistributeOperation_FailedOperationCountsAsFailed is the counter the issue was filed about: an
// operation that failed on every node must not be recorded as a success.
func TestDistributeOperation_FailedOperationCountsAsFailed(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "acct-fail-node")
	cm.SetBackend(&errBackend{err: errors.New("s3: AccessDenied")})

	result, err := cm.DistributeOperation(context.Background(), &DistributedOperation{
		Type: OpTypeGet,
		Key:  "k",
	})

	if err == nil {
		t.Error("DistributeOperation returned a nil error for an operation that failed on every node")
	}
	if result == nil {
		t.Fatal("result is nil; the failure detail is the only thing that says which node failed and why")
	}
	if result.Success {
		t.Error("result.Success = true for a failed operation")
	}

	// The error must carry the cause. "operation failed" with the AccessDenied dropped sends an
	// operator to the wrong place, which is the same defect one layer along.
	if err != nil && !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("error %q should name the backend failure", err)
	}

	stats := cm.GetStats()
	if stats.TotalOperations != 1 {
		t.Errorf("TotalOperations = %d, want 1", stats.TotalOperations)
	}
	if stats.FailedOps != 1 {
		t.Errorf("FailedOps = %d, want 1", stats.FailedOps)
	}
	if stats.SuccessfulOps != 0 {
		t.Errorf("SuccessfulOps = %d, want 0: the operation failed on every node", stats.SuccessfulOps)
	}
}

// TestDistributeOperation_SucceededOperationCountsAsSuccessful is the other half. Without it, a fix
// that returned an error unconditionally would pass the test above.
func TestDistributeOperation_SucceededOperationCountsAsSuccessful(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithBackend(t, "acct-ok-node")

	result, err := cm.DistributeOperation(context.Background(), &DistributedOperation{
		Type: OpTypeGet,
		Key:  "k",
	})
	if err != nil {
		t.Fatalf("DistributeOperation: %v", err)
	}
	if !result.Success {
		t.Fatalf("result.Success = false: %s", result.Error)
	}

	stats := cm.GetStats()
	if stats.SuccessfulOps != 1 {
		t.Errorf("SuccessfulOps = %d, want 1", stats.SuccessfulOps)
	}
	if stats.FailedOps != 0 {
		t.Errorf("FailedOps = %d, want 0", stats.FailedOps)
	}
}

// TestDistributeOperation_ErrorAndSuccessNeverDisagree checks the invariant itself across every shape
// of operation and both outcomes, rather than the two counters that happen to read it today.
//
// This is the assertion that would have caught the defect at any of the executors, and the fix is at
// one choke point precisely so a new path cannot reintroduce the disagreement. The dimension used to
// be the three consistency levels; #284 deleted those, and what varies now is what actually reaches a
// different backend method — an unconditional put, a conditional one, and a read.
func TestDistributeOperation_ErrorAndSuccessNeverDisagree(t *testing.T) {
	t.Parallel()

	shapes := []struct {
		name string
		op   DistributedOperation
	}{
		{"get", DistributedOperation{Type: OpTypeGet, Key: "k"}},
		{"put", DistributedOperation{Type: OpTypePut, Key: "k", Data: []byte("v")}},
		{"put-if", DistributedOperation{
			Type:         OpTypePut,
			Key:          "k",
			Data:         []byte("v"),
			Precondition: types.Precondition{Absent: true},
		}},
	}

	for _, shape := range shapes {
		for _, failing := range []bool{false, true} {
			name := fmt.Sprintf("%s/failing=%v", shape.name, failing)

			t.Run(name, func(t *testing.T) {
				t.Parallel()

				cm := makeClusterWithNode(t, fmt.Sprintf("agree-%s-%v", shape.name, failing))
				if failing {
					cm.SetBackend(&errBackend{err: errors.New("s3: ServiceUnavailable")})
				} else {
					// A real backend for the succeeding half, not mockBackend, because one of the three
					// shapes is a conditional write and mockBackend answers PutObjectIf with
					// ErrNotSupported — deliberately, so that no coordination test can conclude
					// exactly-one-writer against a stub that never excluded anybody. A stub cannot both
					// refuse to fake a CAS and serve as the success case for one.
					srv := testaws.Start(t)
					if shape.op.Type == OpTypeGet {
						srv.PutObject(shape.op.Key, []byte("v"))
					}
					cm.SetBackend(srv.Backend())
				}

				op := shape.op
				result, err := cm.coordinator.ExecuteOperation(context.Background(), &op)
				if result == nil {
					t.Fatal("result is nil")
				}
				if result.Success == (err != nil) {
					t.Errorf("result.Success = %v with err = %v: the two must agree", result.Success, err)
				}
				if result.Success == failing {
					t.Errorf("result.Success = %v, want %v", result.Success, !failing)
				}
			})
		}
	}
}
