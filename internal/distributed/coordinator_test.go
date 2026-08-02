package distributed

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
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

// makeClusterWithBackend is like makeClusterWithNode but also injects a mock
// backend so executeLocally performs real (mocked) S3 operations.
func makeClusterWithBackend(t *testing.T, nodeID string) *ClusterManager {
	t.Helper()
	cm := makeClusterWithNode(t, nodeID)
	cm.SetBackend(&mockBackend{})
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

// mockBackend is a minimal types.Backend for testing.
type mockBackend struct{}

func (m *mockBackend) GetObject(_ context.Context, _ string, _, _ int64) ([]byte, error) {
	return []byte("mock-data"), nil
}
func (m *mockBackend) PutObject(_ context.Context, _ string, _ []byte, _ map[string]string) error {
	return nil
}
func (m *mockBackend) SetObjectMetadata(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (m *mockBackend) DeleteObject(_ context.Context, _ string) error { return nil }
func (m *mockBackend) HeadObject(_ context.Context, key string) (*types.ObjectInfo, error) {
	return &types.ObjectInfo{Key: key}, nil
}
func (m *mockBackend) GetObjects(_ context.Context, keys []string) (map[string][]byte, error) {
	result := make(map[string][]byte, len(keys))
	for _, k := range keys {
		result[k] = []byte("mock-data")
	}
	return result, nil
}
func (m *mockBackend) PutObjects(_ context.Context, _ map[string][]byte) error { return nil }
func (m *mockBackend) ListObjects(_ context.Context, _ string, _ int) ([]types.ObjectInfo, error) {
	return []types.ObjectInfo{{Key: "key1", Size: 1024}, {Key: "key2", Size: 2048}}, nil
}
func (m *mockBackend) HealthCheck(_ context.Context) error { return nil }

// errBackend is a mockBackend that always returns errors, used for nil-backend tests.
type errBackend struct{ err error }

func (e *errBackend) GetObject(_ context.Context, _ string, _, _ int64) ([]byte, error) {
	return nil, e.err
}
func (e *errBackend) PutObject(_ context.Context, _ string, _ []byte, _ map[string]string) error {
	return e.err
}
func (e *errBackend) SetObjectMetadata(_ context.Context, _ string, _ map[string]string) error {
	return e.err
}
func (e *errBackend) DeleteObject(_ context.Context, _ string) error { return e.err }
func (e *errBackend) HeadObject(_ context.Context, _ string) (*types.ObjectInfo, error) {
	return nil, e.err
}
func (e *errBackend) GetObjects(_ context.Context, _ []string) (map[string][]byte, error) {
	return nil, e.err
}
func (e *errBackend) PutObjects(_ context.Context, _ map[string][]byte) error { return e.err }
func (e *errBackend) ListObjects(_ context.Context, _ string, _ int) ([]types.ObjectInfo, error) {
	return nil, e.err
}
func (e *errBackend) HealthCheck(_ context.Context) error { return e.err }

// TestNewCoordinator verifies that NewCoordinator succeeds and returns a
// non-nil coordinator with its load balancer and replicator initialized.
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
	cm := makeClusterWithBackend(t, "ev-get-node")

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
		t.Error("result.Data should not be empty for a GET with mock backend")
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
	cm := makeClusterWithBackend(t, "ev-put-node")

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
	cm := makeClusterWithBackend(t, "sess-node")

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
	cm := makeClusterWithBackend(t, "strong-node")

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
	cm.SetBackend(&mockBackend{})
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
	cm := makeClusterWithBackend(t, "list-node")
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
	ctx := t.Context()

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

	// Inject a mock backend so executeLocally performs real (mocked) S3 ops.
	cm1.SetBackend(&mockBackend{})
	cm2.SetBackend(&mockBackend{})

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
	// The remote handler calls executeLocally with the mock backend which
	// returns "mock-data" for any GET operation.
	want := "mock-data"
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

	ctx := t.Context()

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

// ── executeLocally tests ──────────────────────────────────────────────────────

// TestCoordinator_ExecuteLocally_NilBackend verifies that executeLocally
// returns a non-success result with a descriptive error when no backend is set.
func TestCoordinator_ExecuteLocally_NilBackend(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "nil-be-node") // no SetBackend call
	result := cm.coordinator.executeLocally("nil-be-node", &DistributedOperation{
		Type: OpTypeGet,
		Key:  "k",
	})
	if result.Success {
		t.Error("expected failure with nil backend")
	}
	if !strings.Contains(result.Error, "no backend configured") {
		t.Errorf("error %q should mention 'no backend configured'", result.Error)
	}
}

// TestCoordinator_ExecuteLocally_Get verifies that a GET with a mock backend
// returns the mock data.
func TestCoordinator_ExecuteLocally_Get(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithBackend(t, "loc-get-node")
	result := cm.coordinator.executeLocally("loc-get-node", &DistributedOperation{
		Type: OpTypeGet,
		Key:  "mykey",
	})
	if !result.Success {
		t.Errorf("expected success, got error: %s", result.Error)
	}
	if string(result.Data) != "mock-data" {
		t.Errorf("data = %q, want %q", string(result.Data), "mock-data")
	}
}

// TestCoordinator_ExecuteLocally_Put verifies that a PUT with a mock backend
// returns success.
func TestCoordinator_ExecuteLocally_Put(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithBackend(t, "loc-put-node")
	result := cm.coordinator.executeLocally("loc-put-node", &DistributedOperation{
		Type: OpTypePut,
		Key:  "mykey",
		Data: []byte("value"),
	})
	if !result.Success {
		t.Errorf("expected success, got error: %s", result.Error)
	}
}

// TestCoordinator_ExecuteLocally_Delete verifies that a DELETE with a mock
// backend returns success.
func TestCoordinator_ExecuteLocally_Delete(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithBackend(t, "loc-del-node")
	result := cm.coordinator.executeLocally("loc-del-node", &DistributedOperation{
		Type: OpTypeDelete,
		Key:  "mykey",
	})
	if !result.Success {
		t.Errorf("expected success, got error: %s", result.Error)
	}
}

// TestCoordinator_ExecuteLocally_BackendError verifies that a backend error is
// propagated into the result.
func TestCoordinator_ExecuteLocally_BackendError(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "loc-err-node")
	cm.SetBackend(&errBackend{err: errors.New("s3: connection refused")})
	result := cm.coordinator.executeLocally("loc-err-node", &DistributedOperation{
		Type: OpTypeGet,
		Key:  "mykey",
	})
	if result.Success {
		t.Error("expected failure from errBackend")
	}
	if !strings.Contains(result.Error, "s3: connection refused") {
		t.Errorf("error %q should contain backend error", result.Error)
	}
}

// ── Cache invalidation tests ──────────────────────────────────────────────────

// mockCache is a minimal types.Cache for testing cache invalidation.
type mockCache struct {
	mu      sync.Mutex
	deleted []string
}

func (mc *mockCache) Get(_ string, _, _ int64) []byte { return nil }
func (mc *mockCache) Put(_ string, _ int64, _ []byte) {}
func (mc *mockCache) Delete(key string) {
	mc.mu.Lock()
	mc.deleted = append(mc.deleted, key)
	mc.mu.Unlock()
}
func (mc *mockCache) Evict(_ int64) bool      { return false }
func (mc *mockCache) Size() int64             { return 0 }
func (mc *mockCache) Stats() types.CacheStats { return types.CacheStats{} }

// TestClusterManager_InvalidateCacheKey_NoGossip verifies that
// InvalidateCacheKey is a no-op (does not panic) when gossip is not running.
func TestClusterManager_InvalidateCacheKey_NoGossip(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "inval-no-gossip")
	mc := &mockCache{}
	cm.SetCache(mc)
	// gossip.conn is nil (Start not called) — should not panic
	cm.InvalidateCacheKey("foo")
}

// TestClusterManager_CacheInvalidation_TwoNodes verifies that
// InvalidateCacheKey on cm1 causes cm2's cache to have Delete called for the
// same key over loopback UDP.
func TestClusterManager_CacheInvalidation_TwoNodes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	cfg1 := testConfig("inval-node-a")
	cfg2 := testConfig("inval-node-b")

	cm1, err := NewClusterManager(cfg1)
	if err != nil {
		t.Fatalf("NewClusterManager cm1: %v", err)
	}
	cm2, err := NewClusterManager(cfg2)
	if err != nil {
		t.Fatalf("NewClusterManager cm2: %v", err)
	}

	mc2 := &mockCache{}
	cm2.SetCache(mc2)

	if err := cm1.Start(ctx); err != nil {
		t.Fatalf("cm1.Start: %v", err)
	}
	defer func() { _ = cm1.Stop() }()

	if err := cm2.Start(ctx); err != nil {
		t.Fatalf("cm2.Start: %v", err)
	}
	defer func() { _ = cm2.Stop() }()

	addr1 := cm1.gossip.LocalAddr()
	addr2 := cm2.gossip.LocalAddr()
	if addr1 == "" || addr2 == "" {
		t.Fatalf("could not get local addresses: %q %q", addr1, addr2)
	}

	// Cross-register so cm1 knows about cm2's address.
	cm1.UpdateNodeInfo("inval-node-b", &NodeInfo{
		ID: "inval-node-b", Address: addr2, Status: NodeStatusAlive, Metadata: map[string]string{},
	})
	// Also register cm2's gossip memberlist entry so broadcastMessage sees it.
	cm1.gossip.mu.Lock()
	cm1.gossip.memberlist["inval-node-b"] = &GossipNode{
		Info:  &NodeInfo{ID: "inval-node-b", Address: addr2},
		State: StateAlive,
	}
	cm1.gossip.mu.Unlock()

	cm1.InvalidateCacheKey("foo")

	// Wait up to 200ms for the message to arrive and be processed.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		mc2.mu.Lock()
		found := len(mc2.deleted) > 0 && mc2.deleted[len(mc2.deleted)-1] == "foo"
		mc2.mu.Unlock()
		if found {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	mc2.mu.Lock()
	deleted := mc2.deleted
	mc2.mu.Unlock()
	t.Errorf("cache.Delete(%q) was not called on cm2 within 200ms; deleted keys: %v", "foo", deleted)
}
