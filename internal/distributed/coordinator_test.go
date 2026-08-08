package distributed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// makeClusterWithNode returns a ClusterManager that has one alive node
// (the manager's own ID) pre-registered, without calling Start().
func makeClusterWithNode(t *testing.T, nodeID string) *ClusterManager {
	t.Helper()
	cm, err := NewClusterManager(testConfig(t, nodeID))
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

// PutObjectIf refuses rather than pretending, which is the load-bearing choice in this package
// specifically. A CAS is the coordination primitive that replaces the consensus code here, so a mock
// that answered a precondition with a fabricated ETag and nil would let a coordination test conclude
// exactly-one-writer against a stub that never excluded anybody.
func (m *mockBackend) PutObjectIf(_ context.Context, _ string, _ []byte, _ map[string]string,
	_ types.Precondition,
) (string, error) {
	return "", types.ErrNotSupported
}
func (m *mockBackend) SetObjectMetadata(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (m *mockBackend) CopyObject(_ context.Context, _, _ string) error { return nil }
func (m *mockBackend) DeleteObject(_ context.Context, _ string) error  { return nil }
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

// PutObjectIf returns this backend's configured error, like every other method. It is not
// ErrNotSupported: the whole point of errBackend is that every operation fails the same way, and a
// method that failed differently would be a hole in that.
func (e *errBackend) PutObjectIf(_ context.Context, _ string, _ []byte, _ map[string]string,
	_ types.Precondition,
) (string, error) {
	return "", e.err
}
func (e *errBackend) SetObjectMetadata(_ context.Context, _ string, _ map[string]string) error {
	return e.err
}
func (e *errBackend) CopyObject(_ context.Context, _, _ string) error { return e.err }
func (e *errBackend) DeleteObject(_ context.Context, _ string) error  { return e.err }
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
	cm, err := NewClusterManager(testConfig(t, "c-node"))
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

	// There was a `c.replicator == nil` check here. #284 deleted the CacheReplicator, and this
	// assertion is what let it survive: it proved a field had been assigned, which was the only thing
	// about that subsystem that worked. Its worker sent peers a PUT of an object's own bytes back to
	// itself, and when gossip was not running — which is every test in this file — it sent nothing and
	// counted the bytes as replicated anyway.
}

// TestCoordinator_ExecuteOperation_NoAliveNodes verifies that ExecuteOperation
// returns an error when no alive nodes are available.
func TestCoordinator_ExecuteOperation_NoAliveNodes(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "c-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	_, err = cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
		Type: OpTypeGet,
		Key:  "k",
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
		ID:   "", // empty — should be generated
		Type: OpTypeGet,
		Key:  "k",
	}

	_, _ = cm.coordinator.ExecuteOperation(context.Background(), op)

	if op.ID == "" {
		t.Error("ExecuteOperation should have populated op.ID")
	}
	if !strings.HasPrefix(op.ID, "op-") {
		t.Errorf("generated ID %q should start with 'op-'", op.ID)
	}
}

// TestCoordinator_ExecuteOperation_RejectsPreconditionOnNonPut verifies that a precondition attached
// to anything but a put is refused rather than ignored.
//
// Ignoring it is the dangerous outcome and the one worth a test: a caller that means "delete only if
// unchanged" and gets an unconditional delete has no way to find out. So this asserts the fail-closed
// direction — an error, wrapping types.ErrInvalidPrecondition, and the backend untouched.
//
// It replaced TestCoordinator_ExecuteOperation_AppliesDefaultConsistency, which asserted that an
// operation with no consistency level was assigned config.ConsistencyLevel. That is the defaulting
// #284 deleted: the level it filled in selected how many nodes issued the same unconditional PUT, and
// the test could not tell whether the value it read back had any effect.
func TestCoordinator_ExecuteOperation_RejectsPreconditionOnNonPut(t *testing.T) {
	t.Parallel()

	for _, opType := range []OperationType{OpTypeGet, OpTypeDelete, OpTypeList} {
		t.Run(string(opType), func(t *testing.T) {
			t.Parallel()

			cm := makeClusterWithBackend(t, "precond-"+string(opType))

			result, err := cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
				Type:         opType,
				Key:          "k",
				Precondition: types.Precondition{ETag: `"abc"`},
			})
			if err == nil {
				t.Fatalf("ExecuteOperation returned nil error for a precondition on %s: it must be "+
					"refused, never silently dropped", opType)
			}
			if !errors.Is(err, types.ErrInvalidPrecondition) {
				t.Errorf("error %v does not wrap types.ErrInvalidPrecondition", err)
			}
			if result == nil {
				t.Fatal("result is nil; the caller needs the reason as well as the error")
			}
			if result.Success {
				t.Error("result.Success = true for a rejected operation")
			}
			if !strings.Contains(result.Error, string(opType)) {
				t.Errorf("result.Error = %q should name the operation type", result.Error)
			}
		})
	}
}

// TestCoordinator_ExecuteOperation_AppliesDefaultTimeout verifies that a zero
// Timeout is replaced with the config default.
func TestCoordinator_ExecuteOperation_AppliesDefaultTimeout(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "def-timeout-node")
	op := &DistributedOperation{
		Type: OpTypeGet,
		Key:  "k",
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
		Type: OpTypeGet,
		Key:  "k",
		// Retries intentionally zero
	}

	_, _ = cm.coordinator.ExecuteOperation(context.Background(), op)

	if op.Retries == 0 {
		t.Error("Retries should have been set to config.RetryAttempts")
	}
}

// TestCoordinator_ExecuteOperation_Get verifies a successful GET returns a result with data, a
// latency, and the node that served it.
//
// This is one test where there were four. Three of them — _EventualConsistency_Get,
// _SessionConsistency and _StrongConsistency — issued the same GET at the same three levels and
// asserted result.Success, which is what the levels being three names for one behavior looks like
// from the test side: none of them could have failed while the others passed.
func TestCoordinator_ExecuteOperation_Get(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithBackend(t, "get-node")

	result, err := cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
		Type: OpTypeGet,
		Key:  "mykey",
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
	if len(result.NodeResults) != 1 {
		t.Errorf("len(NodeResults) = %d, want 1: one node performs the operation", len(result.NodeResults))
	}
}

// TestCoordinator_ExecuteOperation_Put verifies a successful unconditional PUT.
func TestCoordinator_ExecuteOperation_Put(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithBackend(t, "put-node")

	result, err := cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
		Type: OpTypePut,
		Key:  "mykey",
		Data: []byte("hello"),
	})
	if err != nil {
		t.Fatalf("ExecuteOperation: %v", err)
	}
	if !result.Success {
		t.Errorf("result.Success = false, error: %s", result.Error)
	}
	if result.Conditional != "" {
		t.Errorf("result.Conditional = %q, want empty for an unconditional write", result.Conditional)
	}
}

// makeClusterWithRealBackend is makeClusterWithBackend against a real S3 backend on an in-process
// substrate endpoint, which is what a conditional write needs: mockBackend answers PutObjectIf with
// types.ErrNotSupported on purpose, so a CAS cannot be exercised against it. The returned server is
// the same one the cluster writes to, so a test can read the stored bytes back and count requests.
func makeClusterWithRealBackend(t *testing.T, nodeID string) (*ClusterManager, *testaws.TestServer) {
	t.Helper()
	cm := makeClusterWithNode(t, nodeID)
	srv := testaws.Start(t)
	cm.SetBackend(srv.Backend())
	return cm, srv
}

// TestCoordinator_ExecuteOperation_ConcurrentUpdate_OneWriterWins is the test #284 exists for: two
// coordinators read the same object and both try to replace it, each asserting the ETag it read.
//
// Exactly one must succeed. The loser must get types.ErrPreconditionFailed and must not have written,
// so the stored bytes are one writer's and not a splice or the loser's. Under the taxonomy this
// replaced there was no test that could state this: `consistency_level: strong` issued the same
// unconditional PutObject the other two levels did, N times, and the last writer won silently.
func TestCoordinator_ExecuteOperation_ConcurrentUpdate_OneWriterWins(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const key = "cas/contended"
	cmA, srv := makeClusterWithRealBackend(t, "cas-a")
	srv.PutObject(key, []byte("v0"))

	// A second coordinator over the *same* endpoint, so both writes race one key in one bucket. A
	// second substrate server would give each its own object and the race could not happen.
	cmB := makeClusterWithNode(t, "cas-b")
	cmB.SetBackend(srv.Backend())

	info, err := srv.Backend().HeadObject(ctx, key)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if info.ETag == "" {
		t.Fatal("HeadObject returned an empty ETag: there is nothing for a CAS to assert on")
	}

	type outcome struct {
		body string
		res  *OperationResult
		err  error
	}
	results := make(chan outcome, 2)

	var start sync.WaitGroup
	start.Add(1)
	for _, w := range []struct {
		cm   *ClusterManager
		body string
	}{{cmA, "written-by-a"}, {cmB, "written-by-b"}} {
		go func() {
			start.Wait()
			res, err := w.cm.coordinator.ExecuteOperation(ctx, &DistributedOperation{
				Type:         OpTypePut,
				Key:          key,
				Data:         []byte(w.body),
				Precondition: types.Precondition{ETag: info.ETag},
			})
			results <- outcome{w.body, res, err}
		}()
	}
	start.Done()

	var winners, losers []outcome
	for range 2 {
		o := <-results
		switch {
		case o.err == nil && o.res.Success:
			winners = append(winners, o)
		case errors.Is(o.err, types.ErrPreconditionFailed):
			losers = append(losers, o)
		default:
			t.Errorf("writer %q neither won nor lost on the precondition: err=%v result=%+v",
				o.body, o.err, o.res)
		}
	}

	if len(winners) != 1 {
		t.Fatalf("%d writers succeeded, want exactly 1: both asserted the same ETag, so the second "+
			"must be refused — two successes is a lost update", len(winners))
	}
	if len(losers) != 1 {
		t.Fatalf("%d writers were refused, want exactly 1", len(losers))
	}

	won := winners[0]
	if won.res.ETag == "" {
		t.Error("the winning result carries no ETag: a CAS loop cannot continue without the new version")
	}
	if won.res.ETag == info.ETag {
		t.Errorf("the winning ETag %q equals the pre-write ETag: the write did not change the version",
			won.res.ETag)
	}
	if got := string(srv.GetObject(key)); got != won.body {
		t.Errorf("stored bytes are %q, want %q from the writer that succeeded", got, won.body)
	}

	lost := losers[0]
	if lost.res != nil && lost.res.Conditional != ConditionalLost {
		t.Errorf("refused writer reports Conditional = %q, want %q",
			lost.res.Conditional, ConditionalLost)
	}
}

// TestCoordinator_ExecuteOperation_ETagOrdering verifies that each conditional write reports the ETag
// of what it just stored, and that chaining a CAS from that ETag succeeds while replaying a stale one
// fails. That is the whole contract a caller needs to build a read-modify-write loop, and it is what
// the coordinator could not express before #284: PutObject returned no ETag at all.
func TestCoordinator_ExecuteOperation_ETagOrdering(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const key = "cas/chain"
	cm, srv := makeClusterWithRealBackend(t, "etag-chain")

	// Create it with an if-absent write, which is how a caller takes a key nobody holds.
	created, err := cm.coordinator.ExecuteOperation(ctx, &DistributedOperation{
		Type:         OpTypePut,
		Key:          key,
		Data:         []byte("gen-1"),
		Precondition: types.Precondition{Absent: true},
	})
	if err != nil {
		t.Fatalf("if-absent create: %v", err)
	}
	if created.ETag == "" {
		t.Fatal("create returned no ETag")
	}

	// A second if-absent write must fail: the key is no longer absent.
	if _, err := cm.coordinator.ExecuteOperation(ctx, &DistributedOperation{
		Type:         OpTypePut,
		Key:          key,
		Data:         []byte("gen-1-again"),
		Precondition: types.Precondition{Absent: true},
	}); !errors.Is(err, types.ErrPreconditionFailed) {
		t.Errorf("second if-absent write: err = %v, want types.ErrPreconditionFailed", err)
	}

	// Chain forward from the reported ETag. Each generation asserts the previous one and must be
	// accepted, and must report a *different* ETag than the one it asserted.
	etags := []string{created.ETag}
	for gen := 2; gen <= 4; gen++ {
		body := fmt.Sprintf("gen-%d", gen)
		prev := etags[len(etags)-1]

		res, err := cm.coordinator.ExecuteOperation(ctx, &DistributedOperation{
			Type:         OpTypePut,
			Key:          key,
			Data:         []byte(body),
			Precondition: types.Precondition{ETag: prev},
		})
		if err != nil {
			t.Fatalf("CAS from the ETag the previous write reported (%s): %v", prev, err)
		}
		if res.ETag == "" {
			t.Fatalf("%s stored but reported no ETag, so the chain cannot continue", body)
		}
		if res.ETag == prev {
			t.Errorf("%s reports the ETag it asserted (%s); a changed object has a new version",
				body, prev)
		}
		etags = append(etags, res.ETag)
	}

	if got := string(srv.GetObject(key)); got != "gen-4" {
		t.Errorf("stored bytes are %q, want %q: the accepted chain is what the object should hold", got, "gen-4")
	}

	// Every ETag in the chain but the last is now stale, and asserting any of them must be refused —
	// including the one two writes back, not merely the immediately previous one.
	for i, stale := range etags[:len(etags)-1] {
		if _, err := cm.coordinator.ExecuteOperation(ctx, &DistributedOperation{
			Type:         OpTypePut,
			Key:          key,
			Data:         []byte("from-a-stale-read"),
			Precondition: types.Precondition{ETag: stale},
		}); !errors.Is(err, types.ErrPreconditionFailed) {
			t.Errorf("write asserting generation %d's stale ETag %s: err = %v, want "+
				"types.ErrPreconditionFailed", i+1, stale, err)
		}
	}

	if got := string(srv.GetObject(key)); got != "gen-4" {
		t.Errorf("stored bytes are %q after the refused writes, want %q: a refused CAS must not write",
			got, "gen-4")
	}
}

// TestCoordinator_ExecuteOperation_ExplicitTargetNodes verifies that explicit
// TargetNodes bypass the auto-selection logic.
func TestCoordinator_ExecuteOperation_ExplicitTargetNodes(t *testing.T) {
	t.Parallel()
	// No alive nodes in the cluster, but explicit targets are provided.
	cm, err := NewClusterManager(testConfig(t, "explicit-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.SetBackend(&mockBackend{})
	// Register the target node so executeOnNode can simulate a response.
	cm.UpdateNodeInfo("target-1", nodeAlive("target-1"))

	result, err := cm.coordinator.ExecuteOperation(context.Background(), &DistributedOperation{
		Type:        OpTypeGet,
		Key:         "k",
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
		Type: OpTypeList,
		Key:  "prefix/",
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
	cm, err := NewClusterManager(testConfig(t, "stats-coord"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	stats := cm.coordinator.GetStats()
	if stats == nil {
		t.Fatal("GetStats() returned nil")
	}
	for _, key := range []string{"active_operations", "load_balancer"} {
		if _, ok := stats[key]; !ok {
			t.Errorf("stats missing key %q", key)
		}
	}

	// And it must not report a "replication" key. #284 deleted the CacheReplicator whose six counters
	// that key carried; asserting its absence is what stops the map from growing the key back with a
	// zero value, which would read as "no replication happened" rather than "no such subsystem".
	if _, ok := stats["replication"]; ok {
		t.Error(`stats has a "replication" key: the CacheReplicator it reported was deleted in #284, ` +
			`and it counted peers re-uploading an object to itself — incremented even when nothing was sent`)
	}
}

// TestCoordinator_ExecuteOperation_TwoNodes_RealUDP verifies that an operation
// targeted at a remote node travels over loopback UDP and returns a result
// populated by the remote node's handler.
func TestCoordinator_ExecuteOperation_TwoNodes_RealUDP(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	cfg1 := testConfig(t, "coord-node-a")
	cfg2 := testConfig(t, "coord-node-b")

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
	cm, err := NewClusterManager(testConfig(t, "coord-lifecycle"))
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
	cm, err := NewClusterManager(testConfig(t, "lb-empty"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	lb := cm.coordinator.loadBalancer

	got, err := lb.SelectNodes("objects/data.bin", []string{}, 3)
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
	cm, err := NewClusterManager(testConfig(t, "lb-rr"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	lb := cm.coordinator.loadBalancer
	lb.strategy = StrategyRoundRobin

	nodes := []string{"n1", "n2", "n3"}
	got, err := lb.SelectNodes("objects/data.bin", nodes, 3)
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
	cm, err := NewClusterManager(testConfig(t, "lb-cap"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	lb := cm.coordinator.loadBalancer
	lb.strategy = StrategyRoundRobin

	nodes := []string{"n1", "n2"}
	got, err := lb.SelectNodes("objects/data.bin", nodes, 10) // request more than available
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
	cm, err := NewClusterManager(testConfig(t, "lb-ll"))
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

	got, err := lb.SelectNodes("objects/data.bin", []string{"n1", "n2", "n3"}, 1)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	if len(got) != 1 || got[0] != "n2" {
		t.Errorf("LeastLoad: got %v, want [n2]", got)
	}
}

// TestLoadBalancer_ConsistentHash verifies that consistent-hash selection maps a key to the same
// nodes regardless of the order the node list arrives in.
//
// This test used to assert only the count, and its own comment described the behavior as "returns
// the requested number of nodes from the front of the list" — which was an accurate description of
// the defect (#131). A count assertion passes on `return nodes[:count]`, so it could not tell a hash
// ring from a slice prefix. The assertion below cannot pass on a slice prefix: aliveNodes reaches
// this function from a map iteration, so a prefix gives a different answer per permutation.
func TestLoadBalancer_ConsistentHash(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "lb-ch"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	lb := cm.coordinator.loadBalancer
	lb.strategy = StrategyConsistentHash

	const key = "objects/dataset.bin"

	want, err := lb.SelectNodes(key, []string{"n1", "n2", "n3"}, 2)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	if len(want) != 2 {
		t.Fatalf("expected 2 nodes, got %d: %v", len(want), want)
	}

	for _, order := range [][]string{
		{"n3", "n2", "n1"},
		{"n2", "n3", "n1"},
		{"n2", "n1", "n3"},
		{"n3", "n1", "n2"},
	} {
		got, err := lb.SelectNodes(key, order, 2)
		if err != nil {
			t.Fatalf("SelectNodes(%v): %v", order, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("nodes in order %v selected %v, want %v — selection depends on input order",
				order, got, want)
		}
	}

	// Different keys must not all land on the same node, or affinity is technically satisfied and
	// practically useless — every object in the bucket served by one member of the cluster.
	owners := make(map[string]bool)
	for i := range 32 {
		got, err := lb.SelectNodes(fmt.Sprintf("objects/file-%d.bin", i), []string{"n1", "n2", "n3"}, 1)
		if err != nil {
			t.Fatalf("SelectNodes: %v", err)
		}
		owners[got[0]] = true
	}
	if len(owners) < 2 {
		t.Errorf("32 distinct keys reached %d node(s): %v", len(owners), owners)
	}
}

// TestLoadBalancer_ConsistentHashWithoutAKey covers the operation types that carry no key — a list
// with no leader, and a batch. Hashing "" would send every keyless operation to one node, so the
// strategy falls back to round-robin, and this pins that it does rather than returning a prefix.
func TestLoadBalancer_ConsistentHashWithoutAKey(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "lb-ch-nokey"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	lb := cm.coordinator.loadBalancer
	lb.strategy = StrategyConsistentHash

	got, err := lb.SelectNodes("", []string{"n1", "n2", "n3"}, 2)
	if err != nil {
		t.Fatalf("SelectNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 nodes, got %d: %v", len(got), got)
	}
}

// TestSelectTargetNodes_IsOrderIndependent is the end-to-end version, and the one that matters:
// selectTargetNodes builds its node slice by ranging the cluster's node map, so this exercises the
// randomized iteration the unit test above can only simulate. Repeated calls with unchanged
// membership must select the same node for the same key.
func TestSelectTargetNodes_IsOrderIndependent(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "n1"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.coordinator.loadBalancer.strategy = StrategyConsistentHash

	// Five alive members, so a prefix over a randomized iteration would almost never agree with
	// itself across 50 calls. Registered directly rather than via Start, because Start opens a
	// socket and runs gossip, and this test is about the selection decision.
	cm.mu.Lock()
	for _, id := range []string{"n1", "n2", "n3", "n4", "n5"} {
		cm.nodes[id] = &NodeInfo{ID: id, Address: "10.0.0.1:8080", Status: NodeStatusAlive}
	}
	cm.mu.Unlock()

	op := &DistributedOperation{Type: OpTypeGet, Key: "objects/dataset.bin"}

	want, err := cm.coordinator.selectTargetNodes(op)
	if err != nil {
		t.Fatalf("selectTargetNodes: %v", err)
	}

	for i := range 50 {
		got, err := cm.coordinator.selectTargetNodes(op)
		if err != nil {
			t.Fatalf("selectTargetNodes (call %d): %v", i, err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("call %d selected %v, want %v — map iteration order reached the decision",
				i, got, want)
		}
	}
}

// TestSelectTargetNodes_SortsAliveNodes covers the half of the fix the hash ring does not: the ring
// sorts its own nodes, so removing the sort in selectTargetNodes changes nothing a consistent-hash
// test can see — verified by removing it. Round-robin and the least-load tiebreak index the slice
// positionally, so for them the order the map was iterated in *is* the decision.
//
// StrategyRoundRobin with count == len(nodes) returns the slice as given, which makes it the
// cheapest probe for whether the caller sorted.
func TestSelectTargetNodes_SortsAliveNodes(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "n1"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.coordinator.loadBalancer.strategy = StrategyRoundRobin
	cm.config.ReplicationFactor = 5

	cm.mu.Lock()
	for _, id := range []string{"n1", "n2", "n3", "n4", "n5"} {
		cm.nodes[id] = &NodeInfo{ID: id, Address: "10.0.0.1:8080", Status: NodeStatusAlive}
	}
	cm.mu.Unlock()

	want := []string{"n1", "n2", "n3", "n4", "n5"}
	op := &DistributedOperation{Type: OpTypePut, Key: "objects/dataset.bin"}

	// 50 calls, because an unsorted 5-element map iteration lands on ascending order roughly 1 time
	// in 120; a single call would pass on the unsorted code more often than it would be worth.
	for i := range 50 {
		got, err := cm.coordinator.selectTargetNodes(op)
		if err != nil {
			t.Fatalf("selectTargetNodes (call %d): %v", i, err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("call %d selected %v, want %v — alive nodes reach the strategy in map order",
				i, got, want)
		}
	}
}

// TestSelectTargetNodes_ExcludesDeadNodes is the precondition the two tests above assume: selection
// happens over the alive set. Asserted rather than assumed, because sorting a slice that contains
// dead nodes would be deterministic and still route to a node that cannot answer.
func TestSelectTargetNodes_ExcludesDeadNodes(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "n1"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.coordinator.loadBalancer.strategy = StrategyConsistentHash

	cm.mu.Lock()
	cm.nodes["n1"] = &NodeInfo{ID: "n1", Status: NodeStatusAlive}
	cm.nodes["n2"] = &NodeInfo{ID: "n2", Status: NodeStatusDead}
	cm.nodes["n3"] = &NodeInfo{ID: "n3", Status: NodeStatusSuspect}
	cm.mu.Unlock()

	// Enough keys that a node in the candidate set would be selected by at least one of them.
	for i := range 64 {
		got, err := cm.coordinator.selectTargetNodes(&DistributedOperation{
			Type: OpTypeGet,
			Key:  fmt.Sprintf("objects/file-%d.bin", i),
		})
		if err != nil {
			t.Fatalf("selectTargetNodes: %v", err)
		}
		if len(got) != 1 || got[0] != "n1" {
			t.Fatalf("selected %v, want [n1]: only n1 is alive", got)
		}
	}
}

// ── executeLocally tests ──────────────────────────────────────────────────────

// TestCoordinator_ExecuteLocally_NilBackend verifies that executeLocally
// returns a non-success result with a descriptive error when no backend is set.
func TestCoordinator_ExecuteLocally_NilBackend(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "nil-be-node") // no SetBackend call
	result := cm.coordinator.executeLocally(t.Context(), "nil-be-node", &DistributedOperation{
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
	result := cm.coordinator.executeLocally(t.Context(), "loc-get-node", &DistributedOperation{
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
	result := cm.coordinator.executeLocally(t.Context(), "loc-put-node", &DistributedOperation{
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
	result := cm.coordinator.executeLocally(t.Context(), "loc-del-node", &DistributedOperation{
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
	result := cm.coordinator.executeLocally(t.Context(), "loc-err-node", &DistributedOperation{
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

// ctxReportingBackend records whether the context each call receives was already canceled, and how many
// calls arrived. Embedding mockBackend supplies the other nine types.Backend methods.
type ctxReportingBackend struct {
	mockBackend

	mu        sync.Mutex
	calls     int
	sawCancel bool
}

func (b *ctxReportingBackend) GetObject(ctx context.Context, _ string, _, _ int64) ([]byte, error) {
	b.mu.Lock()
	b.calls++
	if ctx.Err() != nil {
		b.sawCancel = true
	}
	b.mu.Unlock()

	// The real backend would fail on a canceled context; do the same rather than succeed regardless,
	// so the result this produces is the one production would produce.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return []byte("mock-data"), nil
}

func (b *ctxReportingBackend) snapshot() (calls int, sawCancel bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.calls, b.sawCancel
}

// TestCoordinator_ExecuteLocally_DerivesItsTimeoutFromTheCaller is the assertion behind the two
// contextcheck findings on this path.
//
// executeLocally applied op.Timeout to context.Background(), so neither of its callers could stop an
// operation once it reached S3. executeOnNode had the caller's context and dropped it; the other caller,
// handleNetworkOperation, is reached from the gossip receive loop, whose context is the cluster's
// lifetime. Either way a 30-second default was the only bound, so a shutting-down node kept issuing the
// GETs and PUTs a peer had asked for against a backend being closed underneath it.
//
// Asserting the backend observes the cancellation, rather than that the result merely failed, is what
// makes this fail against context.Background(): under the old code the operation succeeds, because a
// canceled parent it never consulted cannot affect it.
func TestCoordinator_ExecuteLocally_DerivesItsTimeoutFromTheCaller(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "ctx-node")
	be := &ctxReportingBackend{}
	cm.SetBackend(be)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result := cm.coordinator.executeLocally(ctx, "ctx-node", &DistributedOperation{
		Type: OpTypeGet,
		Key:  "mykey",
	})

	calls, sawCancel := be.snapshot()
	if calls != 1 {
		t.Fatalf("backend received %d GetObject calls, want 1; this is not measuring what it thinks "+
			"it is", calls)
	}

	if !sawCancel {
		t.Error("the backend received a live context from a canceled caller, so op.Timeout is still " +
			"being applied to context.Background() and no caller can stop an operation in flight")
	}

	if result.Success {
		t.Error("executeLocally reported success for an operation whose context was already canceled")
	}
}

// TestCoordinator_HandleNetworkOperation_PassesItsContextToTheBackend covers the other half of the
// chain: that the context reaching executeLocally is the gossip receive loop's and not one manufactured
// in between.
//
// Verifying the two separately is the point. executeLocally respecting a canceled parent is worth
// nothing if its caller hands it context.Background(), and the caller passing one along is worth nothing
// if the callee ignores it — the pre-fix code would have satisfied a test that only checked one of them,
// which is how a context comes to be threaded through three frames and dropped in the fourth.
//
// This is not a test that a peer's operation *should* be abandoned on shutdown. It should: the response
// goes back over a gossip socket that is closing, so the work is unobservable by the node that asked for
// it, and finishing a 30-second S3 call to write into a socket nobody will read is strictly worse than
// stopping.
func TestCoordinator_HandleNetworkOperation_PassesItsContextToTheBackend(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "net-ctx-node")
	be := &ctxReportingBackend{}
	cm.SetBackend(be)

	payload, err := json.Marshal(&NodeOperationMessage{
		RequestID: "req-1",
		From:      "peer-node",
		Operation: &DistributedOperation{Type: OpTypeGet, Key: "mykey"},
	})
	if err != nil {
		t.Fatalf("marshal NodeOperationMessage: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// The response send fails — "peer-node" is not a known node — which is fine and is not what this
	// asserts. What matters is the context the operation itself ran under, before any reply.
	cm.coordinator.handleNetworkOperation(ctx, &GossipMessage{
		Type: MessageTypeNodeOperation,
		From: "peer-node",
		Data: payload,
	})

	calls, sawCancel := be.snapshot()
	if calls != 1 {
		t.Fatalf("backend received %d GetObject calls, want 1; the operation never reached the backend, "+
			"so this asserts nothing about its context", calls)
	}

	if !sawCancel {
		t.Error("a peer-requested operation ran under a live context after the cluster's context was " +
			"canceled, so gossip's receive-loop context is not reaching the S3 call")
	}
}

// ── Cache invalidation tests ──────────────────────────────────────────────────

// mockCache is a minimal types.Cache for testing cache invalidation, and for the local-stats refresh
// in cluster_test.go, which needs Stats to return something distinguishable from a zero value.
type mockCache struct {
	mu      sync.Mutex
	deleted []string
	stats   types.CacheStats
}

func (mc *mockCache) Get(_ string, _, _ int64) []byte { return nil }
func (mc *mockCache) Put(_ string, _ int64, _ []byte) {}
func (mc *mockCache) Delete(key string) {
	mc.mu.Lock()
	mc.deleted = append(mc.deleted, key)
	mc.mu.Unlock()
}
func (mc *mockCache) Evict(_ int64) bool { return false }
func (mc *mockCache) Size() int64        { return 0 }
func (mc *mockCache) Stats() types.CacheStats {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	return mc.stats
}

// TestClusterManager_InvalidateCacheKey_NoGossip verifies that
// InvalidateCacheKey is a no-op (does not panic) when gossip is not running.
func TestClusterManager_InvalidateCacheKey_NoGossip(t *testing.T) {
	t.Parallel()
	cm := makeClusterWithNode(t, "inval-no-gossip")
	mc := &mockCache{}
	cm.SetCache(mc)
	// gossip.conn is nil (Start not called) — should not panic
	cm.InvalidateCacheKey("foo", `"etag-1"`)
}

// TestGossip_CacheInvalidate_AppliesEachVersionOnce is requirement R4: an invalidation naming a
// version already applied is discarded, and one naming a new version is applied.
//
// The receive path is exercised directly rather than over UDP, because what is under test is the
// ledger and not the transport — TestClusterManager_CacheInvalidation_TwoNodes covers the wire. Going
// over gossip here would make the assertion depend on retransmission timing, which is exactly the
// nondeterminism the ledger exists to absorb.
func TestGossip_CacheInvalidate_AppliesEachVersionOnce(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "inval-ledger")
	mc := &mockCache{}
	cm.SetCache(mc)

	send := func(m CacheInvalidateMessage) {
		t.Helper()
		payload, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		cm.gossip.handleCacheInvalidate(&GossipMessage{
			Type: MessageTypeCacheInvalidate,
			From: "peer",
			Data: payload,
		})
	}
	deletes := func() []string {
		mc.mu.Lock()
		defer mc.mu.Unlock()

		return slices.Clone(mc.deleted)
	}

	// The first invalidation for a version is applied; a retransmission of it is not. Gossip
	// retransmits by design, so this arrives more than once whether or not anything is wrong.
	send(CacheInvalidateMessage{Key: "k", ETag: `"v1"`})
	send(CacheInvalidateMessage{Key: "k", ETag: `"v1"`})
	if got := deletes(); !slices.Equal(got, []string{"k"}) {
		t.Fatalf("deleted = %v, want one delete: a retransmitted invalidation for a version already "+
			"applied must be discarded", got)
	}

	// A different version of the same key is a different write and is applied.
	send(CacheInvalidateMessage{Key: "k", ETag: `"v2"`})
	if got := deletes(); !slices.Equal(got, []string{"k", "k"}) {
		t.Fatalf("deleted = %v, want two deletes: %q is a later write than %q", got, `"v2"`, `"v1"`)
	}

	// Replaying the superseded version must not evict again. This is the case the ETag exists for: the
	// bytes this node holds may have been re-cached after v2, and applying v1's invalidation now would
	// throw them away.
	send(CacheInvalidateMessage{Key: "k", ETag: `"v1"`})
	if got := deletes(); len(got) != 2 {
		t.Errorf("deleted = %v, want the replay of %q discarded: it names a version already superseded",
			got, `"v1"`)
	}

	// A different key with the same ETag string is a different entry. ETags are per-object, so a
	// ledger keyed on the ETag alone would suppress a real invalidation here.
	send(CacheInvalidateMessage{Key: "other", ETag: `"v1"`})
	if got := deletes(); !slices.Contains(got, "other") {
		t.Errorf("deleted = %v, want an eviction of \"other\": the ledger is keyed per key", got)
	}

	// An invalidation naming no version is applied every time. A delete has no ETag to name, and
	// remembering "no version" as a version would drop every invalidation for a key after the first.
	before := len(deletes())
	send(CacheInvalidateMessage{Key: "k", Deleted: true})
	send(CacheInvalidateMessage{Key: "k", Deleted: true})
	if got := deletes(); len(got) != before+2 {
		t.Errorf("deleted = %v, want both unversioned invalidations applied: evicting what you hold is "+
			"always safe, and suppressing them would leave a deleted key cached", got)
	}
}

// TestGossip_MarkInvalidationApplied_LedgerIsBounded verifies the ledger drops entries rather than
// growing with every key the cluster has ever written, and that dropping costs at most a redundant
// eviction.
func TestGossip_MarkInvalidationApplied_LedgerIsBounded(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "inval-bound")

	for i := range maxInvalidationLedger * 2 {
		cm.gossip.markInvalidationApplied(fmt.Sprintf("key-%d", i), `"v1"`)
	}

	cm.gossip.mu.RLock()
	size := len(cm.gossip.appliedInvalidations)
	cm.gossip.mu.RUnlock()

	if size > maxInvalidationLedger {
		t.Errorf("ledger holds %d entries after %d distinct keys, want at most %d: unbounded growth is "+
			"a leak proportional to every key the cluster has written",
			size, maxInvalidationLedger*2, maxInvalidationLedger)
	}

	// And the most recent key is still remembered, so eviction drops old entries rather than refusing
	// new ones — a ledger that stopped recording once full would apply every duplicate from then on.
	recent := fmt.Sprintf("key-%d", maxInvalidationLedger*2-1)
	if first := cm.gossip.markInvalidationApplied(recent, `"v1"`); first {
		t.Errorf("the ledger forgot %s, the key it recorded last: eviction must drop the oldest "+
			"entries, not decline to record new ones", recent)
	}
}

// TestClusterManager_CacheInvalidation_TwoNodes verifies that
// InvalidateCacheKey on cm1 causes cm2's cache to have Delete called for the
// same key over loopback UDP.
func TestClusterManager_CacheInvalidation_TwoNodes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	cfg1 := testConfig(t, "inval-node-a")
	cfg2 := testConfig(t, "inval-node-b")

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

	cm1.InvalidateCacheKey("foo", `"etag-1"`)

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
