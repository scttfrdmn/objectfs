package tests

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/objectfs/objectfs/internal/distributed"
)

func TestClusterManager_BasicOperations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create cluster configuration
	config := &distributed.ClusterConfig{
		NodeID:            "test-node-1",
		ListenAddr:        "127.0.0.1:18080",
		AdvertiseAddr:     "127.0.0.1:18080",
		ElectionTimeout:   2 * time.Second,
		HeartbeatInterval: 500 * time.Millisecond,
		GossipInterval:    100 * time.Millisecond,
		GossipFanout:      2,
		MaxGossipPacket:   1024,
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
		Key:         "test-key",
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

	// Verify stats were updated
	stats := cm.GetStats()
	if stats.TotalOperations == 0 {
		t.Error("Expected operation count to be incremented")
	}

	if stats.SuccessfulOps == 0 {
		t.Error("Expected successful operation count to be incremented")
	}
}

func TestConsensusEngine_LeaderElection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	config := &distributed.ClusterConfig{
		NodeID:            "consensus-test-1",
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
		NodeID:          "gossip-test-1",
		ListenAddr:      "127.0.0.1:18082",
		AdvertiseAddr:   "127.0.0.1:18082",
		GossipInterval:  100 * time.Millisecond,
		GossipFanout:    2,
		MaxGossipPacket: 1024,
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

func TestLoadBalancer_NodeSelection(t *testing.T) {
	config := &distributed.ClusterConfig{
		NodeID:            "lb-test-1",
		MaxConcurrentOps:  5,
		ConsistencyLevel:  "eventual",
		ReplicationFactor: 2,
	}

	cm, err := distributed.NewClusterManager(config)
	if err != nil {
		t.Fatalf("Failed to create cluster manager: %v", err)
	}

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

	// Test GET operation (should select one node)
	getOp := &distributed.DistributedOperation{
		Type:        distributed.OpTypeGet,
		Key:         "test-get-key",
		Consistency: distributed.ConsistencyEventual,
	}

	result, err := coordinator.ExecuteOperation(ctx, getOp)
	if err != nil {
		t.Errorf("Failed to execute GET operation: %v", err)
	}
	if result == nil {
		t.Error("Expected non-nil result for GET operation")
	}

	// Test PUT operation (should select multiple nodes based on replication factor)
	putOp := &distributed.DistributedOperation{
		Type:        distributed.OpTypePut,
		Key:         "test-put-key",
		Data:        []byte("test data"),
		Consistency: distributed.ConsistencyStrong,
	}

	result, err = coordinator.ExecuteOperation(ctx, putOp)
	if err != nil {
		t.Errorf("Failed to execute PUT operation: %v", err)
	}
	if result == nil {
		t.Error("Expected non-nil result for PUT operation")
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

	// Create and start multiple cluster nodes
	for i := 0; i < nodeCount; i++ {
		config := &distributed.ClusterConfig{
			NodeID:            fmt.Sprintf("multi-node-%d", i),
			ListenAddr:        fmt.Sprintf("127.0.0.1:1808%d", i),
			AdvertiseAddr:     fmt.Sprintf("127.0.0.1:1808%d", i),
			ElectionTimeout:   time.Second + time.Duration(i)*100*time.Millisecond,
			HeartbeatInterval: 200 * time.Millisecond,
			GossipInterval:    100 * time.Millisecond,
			GossipFanout:      2,
			ConsistencyLevel:  "strong",
			ReplicationFactor: 2,
		}

		cm, err := distributed.NewClusterManager(config)
		if err != nil {
			t.Fatalf("Failed to create cluster manager %d: %v", i, err)
		}

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
				cm.Stop()
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

	err = cm.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start cluster manager: %v", err)
	}
	defer func() { _ = cm.Stop() }()

	// Wait for leadership
	time.Sleep(2 * time.Second)

	// Execute concurrent operations
	numOps := 10
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

	t.Logf("Concurrent test completed - Total ops: %d, Successful: %d, Failed: %d",
		stats.TotalOperations, stats.SuccessfulOps, stats.FailedOps)
}

func BenchmarkDistributedOperations(b *testing.B) {
	ctx := context.Background()

	config := &distributed.ClusterConfig{
		NodeID:           "bench-node",
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

// Helper function to create test node info
func createTestNodeInfo(nodeID, address string) *distributed.NodeInfo {
	return &distributed.NodeInfo{
		ID:               nodeID,
		Address:          address,
		Status:           distributed.NodeStatusAlive,
		LastSeen:         time.Now(),
		Version:          "1.0.0",
		Metadata:         make(map[string]string),
		CPUUsage:         25.5,
		MemoryUsage:      60.0,
		DiskUsage:        45.0,
		NetworkBandwidth: 1000000,
		CacheSize:        1024 * 1024,
		CacheHitRate:     0.85,
		Operations:       1000,
	}
}
