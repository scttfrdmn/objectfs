package distributed

import (
	"context"
	"strings"
	"testing"
	"time"
)

// testConfig returns a minimal ClusterConfig suitable for unit tests.
// ListenAddr ":0" lets the OS assign an ephemeral UDP port.
// ElectionTimeout is set long to prevent spurious elections during tests.
func testConfig(nodeID string) *ClusterConfig {
	return &ClusterConfig{
		NodeID:            nodeID,
		ListenAddr:        "127.0.0.1:0",
		AdvertiseAddr:     "127.0.0.1:0",
		ElectionTimeout:   30 * time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
	}
}

// TestNewClusterManager_NilConfig verifies that a nil config is accepted and
// defaults are applied.
func TestNewClusterManager_NilConfig(t *testing.T) {
	t.Parallel()
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
	cm, err := NewClusterManager(testConfig(want))
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
	cfg := &ClusterConfig{NodeID: "n1"} // all other fields zero
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
	if cfg.ConsistencyLevel == "" {
		t.Error("ConsistencyLevel should have a default")
	}
	if cfg.OperationTimeout == 0 {
		t.Error("OperationTimeout should have a default")
	}
}

// TestClusterManager_InitialState verifies the state of a newly created manager
// before Start is called.
func TestClusterManager_InitialState(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("init-node"))
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
	cm, err := NewClusterManager(testConfig("node-a"))
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
	cm, err := NewClusterManager(testConfig("node-a"))
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
	cm, err := NewClusterManager(testConfig("node-a"))
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
	cm, err := NewClusterManager(testConfig("self"))
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
	cm, err := NewClusterManager(testConfig("self"))
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
	cm, err := NewClusterManager(testConfig("self"))
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
	cm, err := NewClusterManager(testConfig("self"))
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
	cm, err := NewClusterManager(testConfig("self"))
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
	cm, err := NewClusterManager(testConfig("stats-node"))
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
	cm, err := NewClusterManager(testConfig("coord-node"))
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
	cm, err := NewClusterManager(testConfig("dist-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	op := &DistributedOperation{
		Type:        OpTypeGet,
		Key:         "test-key",
		Consistency: ConsistencyEventual,
	}
	_, err = cm.DistributeOperation(context.Background(), op)
	if err == nil {
		t.Error("DistributeOperation with no alive nodes should return error")
	}
}

// TestClusterManager_StartStop verifies the full lifecycle without panics.
func TestClusterManager_StartStop(t *testing.T) {
	t.Parallel()
	cfg := testConfig("lifecycle-node")
	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
