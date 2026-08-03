//go:build distributed
// +build distributed

package tests

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/distributed"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// withBackend gives a cluster a real S3 backend, against an in-process substrate endpoint.
//
// Every test here that executed an operation used to run with no backend at all, so executeLocally
// took its `c.backend == nil` arm and every operation failed with "no backend configured". The
// assertions then said either nothing or the opposite of the truth — TestConcurrentOperations
// reported ten failures and ten successes in the same run (#269). A test that asserts an operation
// succeeded has to be able to perform one.
//
// A real backend rather than a mock, per the harness note in CLAUDE.md: a mock on the far side of
// this seam would agree with the coordinator by construction, which is how the seam defects this
// suite exists to catch were missed in the first place.
// It returns the endpoint so a caller can seed the keys its operations read, and check afterwards
// what actually reached storage.
func withBackend(t *testing.T, cm *distributed.ClusterManager) *testaws.TestServer {
	t.Helper()

	srv := testaws.Start(t)
	cm.SetBackend(srv.Backend())

	return srv
}

// writeClusterSecret writes a shared cluster secret to a file and returns its path.
//
// Gossip authentication (#206) fails closed: a cluster cannot be constructed without a secret,
// because an unauthenticated gossip port lets any host on the network join and announce ownership of
// cached objects. Every cluster built in this file therefore needs one.
//
// A file rather than OBJECTFS_CLUSTER_SECRET because the environment is process-wide, and mode 0600
// because LoadClusterSecret refuses anything more permissive.
func writeClusterSecret(tb testing.TB) string {
	tb.Helper()

	path := filepath.Join(tb.TempDir(), "cluster.secret")
	if err := os.WriteFile(path, []byte(strings.Repeat("s", 32)), 0o600); err != nil {
		tb.Fatalf("writing the cluster secret: %v", err)
	}

	return path
}

func TestClusterManager_BasicOperations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create cluster configuration
	config := &distributed.ClusterConfig{
		NodeID:            "test-node-1",
		SecretFile:        writeClusterSecret(t),
		ListenAddr:        "127.0.0.1:18080",
		AdvertiseAddr:     "127.0.0.1:18080",
		ElectionTimeout:   2 * time.Second,
		HeartbeatInterval: 500 * time.Millisecond,
		GossipInterval:    100 * time.Millisecond,
		GossipFanout:      2,
		CacheReplication:  true,
		ReplicationFactor: 1,
		ConsistencyLevel:  "eventual",
		MaxConcurrentOps:  10,
		OperationTimeout:  5 * time.Second,
		RetryAttempts:     2,
		RetryBackoff:      100 * time.Millisecond,
	}

	// Create cluster manager
	cm, err := distributed.NewClusterManager(config)
	if err != nil {
		t.Fatalf("Failed to create cluster manager: %v", err)
	}

	// Start cluster manager
	err = cm.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start cluster manager: %v", err)
	}
	defer func() { _ = cm.Stop() }()

	// Verify basic properties
	if cm.GetNodeID() != config.NodeID {
		t.Errorf("Expected node ID %s, got %s", config.NodeID, cm.GetNodeID())
	}

	// Initially should not be leader (no other nodes)
	if cm.IsLeader() {
		t.Error("Node should not be leader initially in single-node cluster")
	}

	// Wait a bit for election timeout
	time.Sleep(3 * time.Second)

	// Now should be leader
	if !cm.IsLeader() {
		t.Error("Node should become leader after election timeout")
	}

	// Verify node is in the member list
	nodes := cm.GetNodes()
	if len(nodes) != 1 {
		t.Errorf("Expected 1 node, got %d", len(nodes))
	}

	if _, exists := nodes[config.NodeID]; !exists {
		t.Error("Node should exist in member list")
	}

	// Test basic stats
	stats := cm.GetStats()
	if stats.TotalNodes != 1 {
		t.Errorf("Expected 1 total node, got %d", stats.TotalNodes)
	}

	if stats.AliveNodes != 1 {
		t.Errorf("Expected 1 alive node, got %d", stats.AliveNodes)
	}
}

func TestClusterManager_DistributedOperation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	config := &distributed.ClusterConfig{
		NodeID:           "test-node-2",
		SecretFile:       writeClusterSecret(t),
		ListenAddr:       "127.0.0.1:18081",
		AdvertiseAddr:    "127.0.0.1:18081",
		ElectionTimeout:  time.Second,
		ConsistencyLevel: "eventual",
		OperationTimeout: 5 * time.Second,
		RetryAttempts:    2,
	}

	cm, err := distributed.NewClusterManager(config)
	if err != nil {
		t.Fatalf("Failed to create cluster manager: %v", err)
	}

	srv := withBackend(t, cm)

	// The operation below is a GET, so the key has to exist. Seeded through the raw SDK rather than
	// through the coordinator: a test that both writes and reads through the layer under test cannot
	// tell a symmetric encoding bug from correct behaviour.
	const (
		key  = "test-key"
		body = "distributed-operation-payload"
	)

	srv.PutObject(key, []byte(body))

	err = cm.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start cluster manager: %v", err)
	}
	defer func() { _ = cm.Stop() }()

	// Wait for node to become leader
	time.Sleep(2 * time.Second)

	// Create a distributed operation
	op := &distributed.DistributedOperation{
		Type:        distributed.OpTypeGet,
		Key:         key,
		Consistency: distributed.ConsistencyEventual,
		Timeout:     3 * time.Second,
	}

	// Execute operation
	result, err := cm.DistributeOperation(ctx, op)
	if err != nil {
		t.Fatalf("Failed to execute distributed operation: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	if !result.Success {
		t.Errorf("Expected successful operation, got error: %s", result.Error)
	}

	// The bytes, not just the success flag. A GET reported successful that returned something else is
	// the failure this assertion exists for.
	if string(result.Data) != body {
		t.Errorf("result.Data = %q, want %q", result.Data, body)
	}

	// Verify stats were updated
	stats := cm.GetStats()
	if stats.TotalOperations == 0 {
		t.Error("Expected operation count to be incremented")
	}

	if stats.SuccessfulOps == 0 {
		t.Error("Expected successful operation count to be incremented")
	}

	// And the counters must not contradict the result they describe (#269).
	if stats.FailedOps != 0 {
		t.Errorf("FailedOps = %d, want 0 for an operation that succeeded", stats.FailedOps)
	}

	// The same operation against a key that does not exist. Both halves are needed: with only the
	// success case above, a fix that returned an error unconditionally would pass, and with only the
	// failure case one that always errored would too. This is also the half that fails if the
	// reconciliation in ExecuteOperation is removed — the assertion the counters could not make while
	// every operation in this file failed for the same reason (#269).
	failed, err := cm.DistributeOperation(ctx, &distributed.DistributedOperation{
		Type:        distributed.OpTypeGet,
		Key:         "a-key-that-was-never-written",
		Consistency: distributed.ConsistencyEventual,
		Timeout:     3 * time.Second,
	})
	if err == nil {
		t.Error("DistributeOperation returned a nil error for a GET of a key that does not exist")
	}
	if failed == nil {
		t.Fatal("Expected a non-nil result even on failure: it carries which node failed and why")
	}
	if failed.Success {
		t.Error("result.Success = true for a GET of a key that does not exist")
	}

	stats = cm.GetStats()
	if stats.FailedOps != 1 {
		t.Errorf("FailedOps = %d, want 1 after one failed operation", stats.FailedOps)
	}
	if stats.SuccessfulOps != 1 {
		t.Errorf("SuccessfulOps = %d, want 1: the failed operation must not be counted here",
			stats.SuccessfulOps)
	}
}

func TestConsensusEngine_LeaderElection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	config := &distributed.ClusterConfig{
		NodeID:            "consensus-test-1",
		SecretFile:        writeClusterSecret(t),
		ElectionTimeout:   time.Second,
		HeartbeatInterval: 200 * time.Millisecond,
		LeadershipTTL:     5 * time.Second,
	}

	cm, err := distributed.NewClusterManager(config)
	if err != nil {
		t.Fatalf("Failed to create cluster manager: %v", err)
	}

	err = cm.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start cluster manager: %v", err)
	}
	defer func() { _ = cm.Stop() }()

	// Get coordinator for testing
	coordinator := cm.GetCoordinator()
	if coordinator == nil {
		t.Fatal("Failed to get coordinator")
	}

	// Wait for election
	time.Sleep(3 * time.Second)

	// Should be leader now
	if !cm.IsLeader() {
		t.Error("Node should become leader after election")
	}

	// Test leadership change proposal
	err = cm.ProposeLeadershipChange(ctx, "new-leader-id")
	if err != nil {
		t.Errorf("Failed to propose leadership change: %v", err)
	}

	// Verify leader changed (in this simple case it won't actually change since we don't have other nodes)
	currentLeader := cm.GetLeader()
	t.Logf("Current leader after proposal: %s", currentLeader)
}

func TestGossipProtocol_BasicFunctionality(t *testing.T) {
	// Test basic gossip protocol functionality
	config := &distributed.ClusterConfig{
		NodeID:         "gossip-test-1",
		SecretFile:     writeClusterSecret(t),
		ListenAddr:     "127.0.0.1:18082",
		AdvertiseAddr:  "127.0.0.1:18082",
		GossipInterval: 100 * time.Millisecond,
		GossipFanout:   2,
	}

	cm, err := distributed.NewClusterManager(config)
	if err != nil {
		t.Fatalf("Failed to create cluster manager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = cm.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start cluster manager: %v", err)
	}
	defer func() { _ = cm.Stop() }()

	// Let gossip protocol run for a bit
	time.Sleep(time.Second)

	// Verify node appears in its own member list
	nodes := cm.GetNodes()
	if len(nodes) == 0 {
		t.Error("Expected at least one node in member list")
	}

	if _, exists := nodes[config.NodeID]; !exists {
		t.Error("Node should exist in its own member list")
	}
}

// TestLoadBalancer_NodeSelection verifies that an operation routed through the coordinator selects a
// target from the gossip membership and executes there.
//
// What this test can and cannot cover is worth stating, because it used to assert something it had not
// arranged. The four peers registered below are addresses nothing listens on — UpdateNodeInfo is a
// membership write, not a running node — so any operation routed to one of them can only time out.
// The test nonetheless asserted that a strong-consistency PUT succeeded, which needs a majority of the
// two selected replicas, i.e. the dead peer. It failed for 30 seconds and then reported the timeout as
// a defect in the code under test. Same class as the missing backend in #269: a test that asserts an
// operation succeeded has to be able to perform one.
//
// So the peers here exist to give selection a membership set of five to choose from, and the two halves
// are asserted separately: the GET runs unpinned, so selection really does choose from all five, and the
// PUT is pinned to the local node with TargetNodes because by then it has to be. Least-load routes the
// second operation away from the node that served the first — which is the load balancer working
// correctly, and the reason a test with one reachable node out of five can assert selection or repeated
// execution but not both at once. Multi-replica execution against nodes that really exist is
// TestMultiNodeCluster, which starts three real managers and does assert it.
func TestLoadBalancer_NodeSelection(t *testing.T) {
	config := &distributed.ClusterConfig{
		NodeID:           "lb-test-1",
		SecretFile:       writeClusterSecret(t),
		MaxConcurrentOps: 5,
		ConsistencyLevel: "eventual",

		// One replica, so the selected set is the primary alone. At 2 the second replica is one of the
		// unreachable peers below, and strong consistency would require it.
		ReplicationFactor: 1,
	}

	cm, err := distributed.NewClusterManager(config)
	if err != nil {
		t.Fatalf("Failed to create cluster manager: %v", err)
	}

	// Without this every operation takes executeLocally's `backend == nil` arm and fails with "no
	// backend configured" (#269).
	srv := withBackend(t, cm)

	ctx := context.Background()
	err = cm.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start cluster manager: %v", err)
	}
	defer func() { _ = cm.Stop() }()

	// Simulate adding nodes to the cluster
	testNodes := []string{"node-1", "node-2", "node-3", "node-4"}
	for _, nodeID := range testNodes {
		nodeInfo := &distributed.NodeInfo{
			ID:               nodeID,
			Address:          fmt.Sprintf("127.0.0.1:808%s", nodeID[len(nodeID)-1:]),
			Status:           distributed.NodeStatusAlive,
			LastSeen:         time.Now(),
			CPUUsage:         float64(len(nodeID)) * 10, // Simulate different loads
			MemoryUsage:      float64(len(nodeID)) * 15,
			NetworkBandwidth: int64(len(nodeID)) * 1000,
		}
		cm.UpdateNodeInfo(nodeID, nodeInfo)
	}

	// Test node selection for different operation types
	coordinator := cm.GetCoordinator()

	// A GET needs its key to exist. Seeded through the raw SDK rather than through the coordinator, so
	// that a symmetric encoding bug cannot pass as correct behaviour.
	const (
		getKey  = "test-get-key"
		getBody = "load-balanced-read-payload"
	)
	srv.PutObject(getKey, []byte(getBody))

	getOp := &distributed.DistributedOperation{
		Type:        distributed.OpTypeGet,
		Key:         getKey,
		Consistency: distributed.ConsistencyEventual,
		Timeout:     5 * time.Second,
	}

	raw, err := coordinator.ExecuteOperation(ctx, getOp)
	if err != nil {
		t.Fatalf("Failed to execute GET operation: %v", err)
	}

	// types.DistributedCoordinator.ExecuteOperation is declared as `(any, error)`, so the concrete
	// result type is only reachable by assertion — and asserting it is itself worth doing, since a
	// caller that cannot get at Success or Data has no way to tell an operation apart from a failure.
	result, ok := raw.(*distributed.OperationResult)
	if !ok {
		t.Fatalf("GET returned %T, want *distributed.OperationResult", raw)
	}
	if !result.Success {
		t.Errorf("GET operation reported failure: %s", result.Error)
	}

	// The bytes, not just the success flag: a GET reported successful that returned something else is
	// the failure mode this suite exists to catch.
	if got := string(result.Data); got != getBody {
		t.Errorf("GET returned %q, want %q", got, getBody)
	}

	// The selected node has to be the one that could serve it. NodeResults is keyed by node, so it says
	// where the operation ran — the assertion the test's name promises and never made.
	if _, ok := result.NodeResults[config.NodeID]; !ok {
		t.Errorf("GET executed on %v, want the local node %q to be among them",
			slices.Sorted(maps.Keys(result.NodeResults)), config.NodeID)
	}

	putOp := &distributed.DistributedOperation{
		Type:        distributed.OpTypePut,
		Key:         "test-put-key",
		Data:        []byte("test data"),
		Consistency: distributed.ConsistencyStrong,
		Timeout:     5 * time.Second,

		// Pinned, because the GET above has just raised the local node's load and StrategyLeastLoad will
		// therefore route this elsewhere — to one of the four peers that do not exist. That is the
		// balancer being right, so the assertion to keep is the one about execution rather than about
		// selection, and TargetNodes is how selectTargetNodes lets a caller say where.
		TargetNodes: []string{config.NodeID},
	}

	raw, err = coordinator.ExecuteOperation(ctx, putOp)
	if err != nil {
		t.Fatalf("Failed to execute PUT operation: %v", err)
	}
	result, ok = raw.(*distributed.OperationResult)
	if !ok {
		t.Fatalf("PUT returned %T, want *distributed.OperationResult", raw)
	}
	if !result.Success {
		t.Errorf("PUT operation reported failure: %s", result.Error)
	}

	// And the object is in storage, read back outside the coordinator. A PUT that reported success
	// without the bytes reaching S3 is silent data loss, which is the whole reason this check is here
	// rather than a look at result.Success.
	if got := string(srv.GetObject("test-put-key")); got != "test data" {
		t.Errorf("object in storage after PUT = %q, want %q", got, "test data")
	}

	// Verify coordinator stats
	stats := coordinator.GetStats()
	if stats == nil {
		t.Error("Expected non-nil coordinator stats")
	}

	t.Logf("Coordinator stats: %+v", stats)
}

func TestMultiNodeCluster(t *testing.T) {
	// Test with multiple nodes
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	nodeCount := 3
	clusters := make([]*distributed.ClusterManager, nodeCount)

	// One secret for the whole cluster, not one per node. Gossip authentication (#206) uses a
	// shared cluster secret, so three different secrets would produce three clusters of one —
	// which is the failure this test exists to distinguish from a working cluster.
	secretFile := writeClusterSecret(t)

	// Every node seeds from node 0, which is the only discovery mechanism there is: gossip learns of a
	// peer from an inbound message, and the only thing that sends an unsolicited one is joinCluster,
	// which runs only when SeedNodes is non-empty. This test set no seeds at all, so the three managers
	// were three clusters of one that had never heard of each other — which is why it reported "Node i
	// sees 1 nodes in cluster" three times and, once single-node elections started working (#275),
	// three leaders instead of none.
	seeds := []string{"127.0.0.1:18080"}

	// Create and start multiple cluster nodes
	for i := 0; i < nodeCount; i++ {
		config := &distributed.ClusterConfig{
			NodeID:            fmt.Sprintf("multi-node-%d", i),
			SecretFile:        secretFile,
			ListenAddr:        fmt.Sprintf("127.0.0.1:1808%d", i),
			AdvertiseAddr:     fmt.Sprintf("127.0.0.1:1808%d", i),
			SeedNodes:         seeds,
			ElectionTimeout:   time.Second + time.Duration(i)*100*time.Millisecond,
			HeartbeatInterval: 200 * time.Millisecond,
			GossipInterval:    100 * time.Millisecond,
			GossipFanout:      2,
			ConsistencyLevel:  "strong",
			ReplicationFactor: 2,

			// MaxGossipPacket is deliberately left at its default. It used to be set to 65507 here, to
			// work around a 1024-byte default that could not carry a three-member sync: the datagram was
			// truncated by the receive buffer and the truncated JSON then failed the authentication
			// envelope parse, so membership stalled at two nodes and reported it as a wrong cluster
			// secret. Both halves are fixed (#277) — the default holds ~19 members and larger memberlists
			// are chunked — and leaving it unset is what makes this test cover that, since a test that
			// overrides the default cannot notice the default regressing.
		}

		cm, err := distributed.NewClusterManager(config)
		if err != nil {
			t.Fatalf("Failed to create cluster manager %d: %v", i, err)
		}

		// Every node, not only the one that turns out to be leader: ConsistencyStrong fans the write out
		// to peers, so a node without a backend fails the operation for the whole cluster (#269).
		withBackend(t, cm)

		clusters[i] = cm

		err = cm.Start(ctx)
		if err != nil {
			t.Fatalf("Failed to start cluster manager %d: %v", i, err)
		}
	}

	// Cleanup
	defer func() {
		for _, cm := range clusters {
			if cm != nil {
				_ = cm.Stop()
			}
		}
	}()

	// Wait for cluster formation and leader election
	time.Sleep(5 * time.Second)

	// Verify that exactly one node is leader
	leaderCount := 0
	var leader *distributed.ClusterManager

	for i, cm := range clusters {
		if cm.IsLeader() {
			leaderCount++
			leader = cm
			t.Logf("Node %d is leader", i)
		}

		// Check node count
		nodes := cm.GetNodes()
		t.Logf("Node %d sees %d nodes in cluster", i, len(nodes))
	}

	if leaderCount != 1 {
		t.Errorf("Expected exactly 1 leader, got %d", leaderCount)
	}

	if leader == nil {
		t.Fatal("No leader found in cluster")
	}

	// Test distributed operation on leader
	op := &distributed.DistributedOperation{
		Type:        distributed.OpTypePut,
		Key:         "multi-node-test-key",
		Data:        []byte("distributed test data"),
		Consistency: distributed.ConsistencyStrong,
		Timeout:     5 * time.Second,
	}

	result, err := leader.DistributeOperation(ctx, op)
	if err != nil {
		t.Errorf("Failed to execute distributed operation: %v", err)
	}

	if result == nil || !result.Success {
		t.Errorf("Expected successful operation, got: %v", result)
	}

	// Verify stats across cluster
	for i, cm := range clusters {
		stats := cm.GetStats()
		t.Logf("Node %d stats - Total: %d, Alive: %d, Operations: %d",
			i, stats.TotalNodes, stats.AliveNodes, stats.TotalOperations)
	}
}

func TestConcurrentOperations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	config := &distributed.ClusterConfig{
		NodeID:           "concurrent-test",
		SecretFile:       writeClusterSecret(t),
		ListenAddr:       "127.0.0.1:18090",
		AdvertiseAddr:    "127.0.0.1:18090",
		ElectionTimeout:  time.Second,
		MaxConcurrentOps: 20,
		ConsistencyLevel: "eventual",
		OperationTimeout: 2 * time.Second,
	}

	cm, err := distributed.NewClusterManager(config)
	if err != nil {
		t.Fatalf("Failed to create cluster manager: %v", err)
	}

	// Execute concurrent operations
	numOps := 10

	srv := withBackend(t, cm)
	for i := range numOps {
		srv.PutObject(fmt.Sprintf("concurrent-key-%d", i), []byte(fmt.Sprintf("payload-%d", i)))
	}

	err = cm.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start cluster manager: %v", err)
	}
	defer func() { _ = cm.Stop() }()

	// Wait for leadership
	time.Sleep(2 * time.Second)

	var wg sync.WaitGroup
	errors := make(chan error, numOps)

	for i := 0; i < numOps; i++ {
		wg.Add(1)
		go func(opID int) {
			defer wg.Done()

			op := &distributed.DistributedOperation{
				Type:        distributed.OpTypeGet,
				Key:         fmt.Sprintf("concurrent-key-%d", opID),
				Consistency: distributed.ConsistencyEventual,
				Timeout:     time.Second,
			}

			result, err := cm.DistributeOperation(ctx, op)
			if err != nil {
				errors <- err
				return
			}

			if result == nil || !result.Success {
				errors <- fmt.Errorf("operation %d failed: %v", opID, result)
				return
			}

			if want := fmt.Sprintf("payload-%d", opID); string(result.Data) != want {
				errors <- fmt.Errorf("operation %d returned %q, want %q", opID, result.Data, want)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check for errors
	errorCount := 0
	for err := range errors {
		t.Errorf("Concurrent operation error: %v", err)
		errorCount++
	}

	if errorCount > 0 {
		t.Errorf("Failed %d out of %d concurrent operations", errorCount, numOps)
	}

	// Verify final stats
	stats := cm.GetStats()
	if int(stats.TotalOperations) < numOps {
		t.Errorf("Expected at least %d operations, got %d", numOps, stats.TotalOperations)
	}

	// The counters have to agree with what the operations above actually did. This test used to print
	// "Failed 10 out of 10" and "Successful: 10, Failed: 0" in the same run: every operation failed
	// with "no backend configured", and DistributeOperation classified on an error the executors never
	// returned, so the statistics an operator reads argued against investigating (#269).
	if int(stats.SuccessfulOps) < numOps {
		t.Errorf("SuccessfulOps = %d, want at least %d", stats.SuccessfulOps, numOps)
	}
	if stats.FailedOps != 0 {
		t.Errorf("FailedOps = %d, want 0: every operation above succeeded", stats.FailedOps)
	}

	t.Logf("Concurrent test completed - Total ops: %d, Successful: %d, Failed: %d",
		stats.TotalOperations, stats.SuccessfulOps, stats.FailedOps)
}

func BenchmarkDistributedOperations(b *testing.B) {
	ctx := context.Background()

	config := &distributed.ClusterConfig{
		NodeID:           "bench-node",
		SecretFile:       writeClusterSecret(b),
		ElectionTimeout:  500 * time.Millisecond,
		MaxConcurrentOps: 100,
		ConsistencyLevel: "eventual",
		OperationTimeout: time.Second,
	}

	cm, err := distributed.NewClusterManager(config)
	if err != nil {
		b.Fatalf("Failed to create cluster manager: %v", err)
	}

	err = cm.Start(ctx)
	if err != nil {
		b.Fatalf("Failed to start cluster manager: %v", err)
	}
	defer func() { _ = cm.Stop() }()

	// Wait for leadership
	time.Sleep(time.Second)

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		opID := 0
		for pb.Next() {
			op := &distributed.DistributedOperation{
				Type:        distributed.OpTypeGet,
				Key:         fmt.Sprintf("bench-key-%d", opID%1000),
				Consistency: distributed.ConsistencyEventual,
				Timeout:     500 * time.Millisecond,
			}

			result, err := cm.DistributeOperation(ctx, op)
			if err != nil {
				b.Fatalf("Operation failed: %v", err)
			}
			if result == nil {
				b.Fatal("Got nil result")
			}

			opID++
		}
	})
}
