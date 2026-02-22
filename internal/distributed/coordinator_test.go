package distributed

import (
	"context"
	"strings"
	"testing"
	"time"
)

// makeClusterWithNode returns a ClusterManager that has one alive node
// (the manager's own ID) pre-registered, without calling Start().
func makeClusterWithNode(t *testing.T, nodeID string) *ClusterManager {
	t.Helper()
	cm, err := NewClusterManager(testConfig(nodeID))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.UpdateNodeInfo(nodeID, nodeAlive(nodeID))
	return cm
}

// nodeAlive returns a NodeInfo stub with Status=Alive.
func nodeAlive(id string) *NodeInfo {
	return &NodeInfo{
		ID:       id,
		Address:  "127.0.0.1:9000",
		Status:   NodeStatusAlive,
		Metadata: map[string]string{},
	}
}

// TestNewCoordinator verifies that NewCoordinator succeeds and returns a
// non-nil coordinator with its load balancer and replicator initialised.
func TestNewCoordinator(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("c-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	c := cm.coordinator
	if c == nil {
		t.Fatal("coordinator is nil")
	}
	if c.loadBalancer == nil {
		t.Fatal("loadBalancer is nil")
	}
	if c.replicator == nil {
		t.Fatal("replicator is nil")
	}
}

// TestCoordinator_ExecuteOperation_NoAliveNodes verifies that ExecuteOperation
// returns an error when no alive nodes are available.
func TestCoordinator_ExecuteOperation_NoAliveNodes(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("c-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	_, err = cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
		Type:        OpTypeGet,
		Key:         "k",
		Consistency: ConsistencyEventual,
	})
	if err == nil {
		t.Fatal("expected error with no alive nodes, got nil")
	}
	if !strings.Contains(err.Error(), "no alive nodes") {
		t.Errorf("error %q should mention 'no alive nodes'", err)
	}
}

// TestCoordinator_ExecuteOperation_GeneratesID verifies that an empty operation
// ID is populated by ExecuteOperation.
func TestCoordinator_ExecuteOperation_GeneratesID(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "gen-id-node")
	op := &DistributedOperation{
		ID:          "", // empty — should be generated
		Type:        OpTypeGet,
		Key:         "k",
		Consistency: ConsistencyEventual,
	}

	_, _ = cm.coordinator.ExecuteOperation(context.Background(), op)

	if op.ID == "" {
		t.Error("ExecuteOperation should have populated op.ID")
	}
	if !strings.HasPrefix(op.ID, "op-") {
		t.Errorf("generated ID %q should start with 'op-'", op.ID)
	}
}

// TestCoordinator_ExecuteOperation_AppliesDefaultConsistency verifies that an
// operation with no consistency level gets the config default.
func TestCoordinator_ExecuteOperation_AppliesDefaultConsistency(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "def-cons-node")
	op := &DistributedOperation{
		Type: OpTypeGet,
		Key:  "k",
		// Consistency intentionally empty
	}

	_, _ = cm.coordinator.ExecuteOperation(context.Background(), op)

	want := ConsistencyLevel(cm.coordinator.config.ConsistencyLevel)
	if op.Consistency != want {
		t.Errorf("Consistency = %q, want %q (config default)", op.Consistency, want)
	}
}

// TestCoordinator_ExecuteOperation_AppliesDefaultTimeout verifies that a zero
// Timeout is replaced with the config default.
func TestCoordinator_ExecuteOperation_AppliesDefaultTimeout(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "def-timeout-node")
	op := &DistributedOperation{
		Type:        OpTypeGet,
		Key:         "k",
		Consistency: ConsistencyEventual,
		// Timeout intentionally zero
	}

	_, _ = cm.coordinator.ExecuteOperation(context.Background(), op)

	if op.Timeout == 0 {
		t.Error("Timeout should have been set to config.OperationTimeout")
	}
}

// TestCoordinator_ExecuteOperation_AppliesDefaultRetries verifies that zero
// Retries are replaced with the config default.
func TestCoordinator_ExecuteOperation_AppliesDefaultRetries(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "def-retry-node")
	op := &DistributedOperation{
		Type:        OpTypeGet,
		Key:         "k",
		Consistency: ConsistencyEventual,
		// Retries intentionally zero
	}

	_, _ = cm.coordinator.ExecuteOperation(context.Background(), op)

	if op.Retries == 0 {
		t.Error("Retries should have been set to config.RetryAttempts")
	}
}

// TestCoordinator_ExecuteOperation_EventualConsistency_Get verifies a
// successful GET with eventual consistency returns a result with data.
func TestCoordinator_ExecuteOperation_EventualConsistency_Get(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "ev-get-node")

	result, err := cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
		Type:        OpTypeGet,
		Key:         "mykey",
		Consistency: ConsistencyEventual,
	})
	if err != nil {
		t.Fatalf("ExecuteOperation: %v", err)
	}
	if !result.Success {
		t.Errorf("result.Success = false, error: %s", result.Error)
	}
	if len(result.Data) == 0 {
		t.Error("result.Data should not be empty for a simulated GET")
	}
	if result.Latency == 0 {
		t.Error("result.Latency should be > 0")
	}
	if result.CompletedAt.IsZero() {
		t.Error("result.CompletedAt should not be zero")
	}
}

// TestCoordinator_ExecuteOperation_EventualConsistency_Put verifies a
// successful PUT with eventual consistency.
func TestCoordinator_ExecuteOperation_EventualConsistency_Put(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "ev-put-node")

	result, err := cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
		Type:        OpTypePut,
		Key:         "mykey",
		Data:        []byte("hello"),
		Consistency: ConsistencyEventual,
	})
	if err != nil {
		t.Fatalf("ExecuteOperation: %v", err)
	}
	if !result.Success {
		t.Errorf("result.Success = false, error: %s", result.Error)
	}
}

// TestCoordinator_ExecuteOperation_SessionConsistency verifies session
// consistency returns a result from the primary node.
func TestCoordinator_ExecuteOperation_SessionConsistency(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "sess-node")

	result, err := cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
		Type:        OpTypeGet,
		Key:         "k",
		Consistency: ConsistencySession,
	})
	if err != nil {
		t.Fatalf("ExecuteOperation: %v", err)
	}
	if !result.Success {
		t.Errorf("result.Success = false, error: %s", result.Error)
	}
	if len(result.NodeResults) == 0 {
		t.Error("NodeResults should contain at least one entry")
	}
}

// TestCoordinator_ExecuteOperation_StrongConsistency verifies that strong
// consistency succeeds with a single alive node (majority = 1).
func TestCoordinator_ExecuteOperation_StrongConsistency(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "strong-node")

	result, err := cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
		Type:        OpTypeGet,
		Key:         "k",
		Consistency: ConsistencyStrong,
	})
	if err != nil {
		t.Fatalf("ExecuteOperation: %v", err)
	}
	if !result.Success {
		t.Errorf("result.Success = false, error: %s", result.Error)
	}
}

// TestCoordinator_ExecuteOperation_ExplicitTargetNodes verifies that explicit
// TargetNodes bypass the auto-selection logic.
func TestCoordinator_ExecuteOperation_ExplicitTargetNodes(t *testing.T) {
	t.Parallel()
	// No alive nodes in the cluster, but explicit targets are provided.
	cm, err := NewClusterManager(testConfig("explicit-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	// Register the target node so executeOnNode can simulate a response.
	cm.UpdateNodeInfo("target-1", nodeAlive("target-1"))

	result, err := cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
		Type:        OpTypeGet,
		Key:         "k",
		Consistency: ConsistencyEventual,
		TargetNodes: []string{"target-1"},
	})
	if err != nil {
		t.Fatalf("ExecuteOperation: %v", err)
	}
	if !result.Success {
		t.Errorf("result.Success = false, error: %s", result.Error)
	}
	if _, ok := result.NodeResults["target-1"]; !ok {
		t.Error("NodeResults should contain 'target-1'")
	}
}

// TestCoordinator_ExecuteOperation_ListUsesLeader verifies that a LIST
// operation targets the current leader when one is set.
func TestCoordinator_ExecuteOperation_ListUsesLeader(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "list-node")
	cm.SetLeader("list-node")

	result, err := cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
		Type:        OpTypeList,
		Key:         "prefix/",
		Consistency: ConsistencyEventual,
	})
	if err != nil {
		t.Fatalf("ExecuteOperation: %v", err)
	}
	if !result.Success {
		t.Errorf("result.Success = false, error: %s", result.Error)
	}
}

// TestCoordinator_GetStats_Structure verifies that GetStats returns a map with
// the expected top-level keys.
func TestCoordinator_GetStats_Structure(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("stats-coord"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	stats := cm.coordinator.GetStats()
	if stats == nil {
		t.Fatal("GetStats() returned nil")
	}
	for _, key := range []string{"active_operations", "replication", "load_balancer"} {
		if _, ok := stats[key]; !ok {
			t.Errorf("stats missing key %q", key)
		}
	}
}

// TestCoordinator_ExecuteOperation_TwoNodes_RealUDP verifies that an operation
// targeted at a remote node travels over loopback UDP and returns a result
// populated by the remote node's handler.
func TestCoordinator_ExecuteOperation_TwoNodes_RealUDP(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg1 := testConfig("coord-node-a")
	cfg2 := testConfig("coord-node-b")

	cm1, err := NewClusterManager(cfg1)
	if err != nil {
		t.Fatalf("NewClusterManager cm1: %v", err)
	}
	cm2, err := NewClusterManager(cfg2)
	if err != nil {
		t.Fatalf("NewClusterManager cm2: %v", err)
	}

	if err := cm1.Start(ctx); err != nil {
		t.Fatalf("cm1.Start: %v", err)
	}
	defer func() { _ = cm1.Stop() }()

	if err := cm2.Start(ctx); err != nil {
		t.Fatalf("cm2.Start: %v", err)
	}
	defer func() { _ = cm2.Stop() }()

	// Cross-register with real UDP addresses.
	addr1 := cm1.gossip.LocalAddr()
	addr2 := cm2.gossip.LocalAddr()
	if addr1 == "" || addr2 == "" {
		t.Fatalf("could not get local addresses: %q %q", addr1, addr2)
	}

	cm1.UpdateNodeInfo("coord-node-b", &NodeInfo{
		ID: "coord-node-b", Address: addr2, Status: NodeStatusAlive, Metadata: map[string]string{},
	})
	cm2.UpdateNodeInfo("coord-node-a", &NodeInfo{
		ID: "coord-node-a", Address: addr1, Status: NodeStatusAlive, Metadata: map[string]string{},
	})

	// Execute a GET targeted explicitly at the remote node (coord-node-b).
	result, err := cm1.coordinator.ExecuteOperation(ctx, &DistributedOperation{
		Type:        OpTypeGet,
		Key:         "remote-key",
		Consistency: ConsistencyEventual,
		Timeout:     5 * time.Second,
		TargetNodes: []string{"coord-node-b"},
	})
	if err != nil {
		t.Fatalf("ExecuteOperation: %v", err)
	}
	if !result.Success {
		t.Errorf("result.Success = false, error: %s", result.Error)
	}

	nr, ok := result.NodeResults["coord-node-b"]
	if !ok {
		t.Fatal("NodeResults should contain 'coord-node-b'")
	}
	if !nr.Success {
		t.Errorf("NodeResult for coord-node-b not successful: %s", nr.Error)
	}
	// The remote handler calls executeLocally("coord-node-b", …) which returns
	// "data-from-coord-node-b" for a GET operation.
	want := "data-from-coord-node-b"
	if string(nr.Data) != want {
		t.Errorf("NodeResult data = %q, want %q", string(nr.Data), want)
	}
}

// TestCoordinator_StartStop verifies the coordinator lifecycle.
func TestCoordinator_StartStop(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("coord-lifecycle"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cm.coordinator.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cm.coordinator.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestLoadBalancer_SelectNodes_Empty verifies that selecting from an empty
// node list returns an empty result without error.
func TestLoadBalancer_SelectNodes_Empty(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("lb-empty"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	lb := cm.coordinator.loadBalancer

	got, err := lb.SelectNodes([]string{}, 3)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(got))
	}
}

// TestLoadBalancer_RoundRobin verifies that round-robin selection cycles
// through all provided nodes.
func TestLoadBalancer_RoundRobin(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("lb-rr"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	lb := cm.coordinator.loadBalancer
	lb.strategy = StrategyRoundRobin

	nodes := []string{"n1", "n2", "n3"}
	got, err := lb.SelectNodes(nodes, 3)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 nodes, got %d", len(got))
	}
}

// TestLoadBalancer_CountCappedToAvailable verifies that requesting more nodes
// than available returns only as many as are available.
func TestLoadBalancer_CountCappedToAvailable(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("lb-cap"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	lb := cm.coordinator.loadBalancer
	lb.strategy = StrategyRoundRobin

	nodes := []string{"n1", "n2"}
	got, err := lb.SelectNodes(nodes, 10) // request more than available
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 nodes (capped), got %d", len(got))
	}
}

// TestLoadBalancer_LeastLoad selects nodes by ascending load order.
func TestLoadBalancer_LeastLoad(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("lb-ll"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	lb := cm.coordinator.loadBalancer
	lb.strategy = StrategyLeastLoad

	// Pre-populate load counters: n2 has lowest load.
	lb.stats.mu.Lock()
	lb.stats.NodeLoad["n1"] = 10
	lb.stats.NodeLoad["n2"] = 1
	lb.stats.NodeLoad["n3"] = 5
	lb.stats.mu.Unlock()

	got, err := lb.SelectNodes([]string{"n1", "n2", "n3"}, 1)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	if len(got) != 1 || got[0] != "n2" {
		t.Errorf("LeastLoad: got %v, want [n2]", got)
	}
}

// TestLoadBalancer_ConsistentHash verifies that consistent-hash selection
// returns the requested number of nodes from the front of the list.
func TestLoadBalancer_ConsistentHash(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("lb-ch"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	lb := cm.coordinator.loadBalancer
	lb.strategy = StrategyConsistentHash

	nodes := []string{"n1", "n2", "n3"}
	got, err := lb.SelectNodes(nodes, 2)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(got))
	}
}
