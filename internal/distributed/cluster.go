package distributed

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/objectfs/objectfs/pkg/types"
)

// ClusterManager manages distributed ObjectFS cluster operations
type ClusterManager struct {
	mu          sync.RWMutex
	config      *ClusterConfig
	nodeID      string
	nodes       map[string]*NodeInfo
	leader      string
	isLeader    bool
	coordinator *Coordinator
	gossip      *GossipProtocol
	consensus   *ConsensusEngine
	stats       *ClusterStats
	stopCh      chan struct{}
	stopped     chan struct{}
}

// ClusterConfig represents cluster configuration
type ClusterConfig struct {
	// Node identification
	NodeID        string `yaml:"node_id"`
	ListenAddr    string `yaml:"listen_addr"`
	AdvertiseAddr string `yaml:"advertise_addr"`

	// Cluster membership
	SeedNodes   []string      `yaml:"seed_nodes"`
	JoinTimeout time.Duration `yaml:"join_timeout"`

	// Leadership and consensus
	ElectionTimeout   time.Duration `yaml:"election_timeout"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	LeadershipTTL     time.Duration `yaml:"leadership_ttl"`

	// Gossip protocol
	GossipInterval  time.Duration `yaml:"gossip_interval"`
	GossipFanout    int           `yaml:"gossip_fanout"`
	MaxGossipPacket int           `yaml:"max_gossip_packet"`

	// Cache coordination
	CacheReplication  bool   `yaml:"cache_replication"`
	ReplicationFactor int    `yaml:"replication_factor"`
	ConsistencyLevel  string `yaml:"consistency_level"` // "eventual", "strong", "session"

	// Performance settings
	MaxConcurrentOps int           `yaml:"max_concurrent_ops"`
	OperationTimeout time.Duration `yaml:"operation_timeout"`
	RetryAttempts    int           `yaml:"retry_attempts"`
	RetryBackoff     time.Duration `yaml:"retry_backoff"`
}

// NodeInfo represents information about a cluster node
type NodeInfo struct {
	ID       string            `json:"id"`
	Address  string            `json:"address"`
	Status   NodeStatus        `json:"status"`
	LastSeen time.Time         `json:"last_seen"`
	Version  string            `json:"version"`
	Metadata map[string]string `json:"metadata"`

	// Resource information
	CPUUsage         float64 `json:"cpu_usage"`
	MemoryUsage      float64 `json:"memory_usage"`
	DiskUsage        float64 `json:"disk_usage"`
	NetworkBandwidth int64   `json:"network_bandwidth"`

	// Cache statistics
	CacheSize    int64   `json:"cache_size"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	Operations   int64   `json:"operations"`
}

// NodeStatus represents the status of a cluster node
type NodeStatus string

const (
	NodeStatusAlive   NodeStatus = "alive"
	NodeStatusSuspect NodeStatus = "suspect"
	NodeStatusDead    NodeStatus = "dead"
	NodeStatusJoining NodeStatus = "joining"
	NodeStatusLeaving NodeStatus = "leaving"
)

// ClusterStats tracks cluster-wide statistics
type ClusterStats struct {
	mu sync.RWMutex

	// Cluster health
	TotalNodes   int `json:"total_nodes"`
	AliveNodes   int `json:"alive_nodes"`
	SuspectNodes int `json:"suspect_nodes"`
	DeadNodes    int `json:"dead_nodes"`

	// Leadership
	CurrentLeader    string    `json:"current_leader"`
	LeaderElections  int64     `json:"leader_elections"`
	LastElectionTime time.Time `json:"last_election_time"`

	// Operations
	TotalOperations int64         `json:"total_operations"`
	SuccessfulOps   int64         `json:"successful_ops"`
	FailedOps       int64         `json:"failed_ops"`
	AvgOpLatency    time.Duration `json:"avg_op_latency"`

	// Cache coordination
	CacheHitRate          float64 `json:"cache_hit_rate"`
	ReplicationEvents     int64   `json:"replication_events"`
	ConsistencyViolations int64   `json:"consistency_violations"`

	// Network
	MessagesSent     int64 `json:"messages_sent"`
	MessagesReceived int64 `json:"messages_received"`
	NetworkErrors    int64 `json:"network_errors"`
}

// applyConfigDefaults applies default values for zero-valued configuration fields
func applyConfigDefaults(config *ClusterConfig) {
	if config.ListenAddr == "" {
		config.ListenAddr = "0.0.0.0:8080"
	}
	if config.AdvertiseAddr == "" {
		config.AdvertiseAddr = "127.0.0.1:8080"
	}
	if config.JoinTimeout == 0 {
		config.JoinTimeout = 30 * time.Second
	}
	if config.ElectionTimeout == 0 {
		config.ElectionTimeout = 5 * time.Second
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = 1 * time.Second
	}
	if config.LeadershipTTL == 0 {
		config.LeadershipTTL = 10 * time.Second
	}
	if config.GossipInterval == 0 {
		config.GossipInterval = 500 * time.Millisecond
	}
	if config.GossipFanout == 0 {
		config.GossipFanout = 3
	}
	if config.MaxGossipPacket == 0 {
		config.MaxGossipPacket = 1024
	}
	if config.ReplicationFactor == 0 {
		config.ReplicationFactor = 3
	}
	if config.ConsistencyLevel == "" {
		config.ConsistencyLevel = "eventual"
	}
	if config.MaxConcurrentOps == 0 {
		config.MaxConcurrentOps = 100
	}
	if config.OperationTimeout == 0 {
		config.OperationTimeout = 30 * time.Second
	}
	if config.RetryAttempts == 0 {
		config.RetryAttempts = 3
	}
	if config.RetryBackoff == 0 {
		config.RetryBackoff = time.Second
	}
}

// NewClusterManager creates a new cluster manager
func NewClusterManager(config *ClusterConfig) (*ClusterManager, error) {
	if config == nil {
		config = &ClusterConfig{
			ListenAddr:        "0.0.0.0:8080",
			AdvertiseAddr:     "127.0.0.1:8080",
			JoinTimeout:       30 * time.Second,
			ElectionTimeout:   5 * time.Second,
			HeartbeatInterval: 1 * time.Second,
			LeadershipTTL:     10 * time.Second,
			GossipInterval:    500 * time.Millisecond,
			GossipFanout:      3,
			MaxGossipPacket:   1024,
			CacheReplication:  true,
			ReplicationFactor: 3,
			ConsistencyLevel:  "eventual",
			MaxConcurrentOps:  100,
			OperationTimeout:  30 * time.Second,
			RetryAttempts:     3,
			RetryBackoff:      time.Second,
		}
	}

	// Apply defaults for zero-valued fields
	applyConfigDefaults(config)

	// Generate node ID if not provided
	if config.NodeID == "" {
		nodeIDBytes := make([]byte, 8)
		if _, err := cryptorand.Read(nodeIDBytes); err != nil {
			return nil, fmt.Errorf("failed to generate node ID: %w", err)
		}
		config.NodeID = "node-" + hex.EncodeToString(nodeIDBytes)
	}

	cm := &ClusterManager{
		config:  config,
		nodeID:  config.NodeID,
		nodes:   make(map[string]*NodeInfo),
		stats:   &ClusterStats{},
		stopCh:  make(chan struct{}),
		stopped: make(chan struct{}),
	}

	// Initialize components
	var err error
	cm.coordinator, err = NewCoordinator(cm, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create coordinator: %w", err)
	}

	cm.gossip, err = NewGossipProtocol(cm, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create gossip protocol: %w", err)
	}

	cm.consensus, err = NewConsensusEngine(cm, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create consensus engine: %w", err)
	}

	return cm, nil
}

// Start starts the cluster manager
func (cm *ClusterManager) Start(ctx context.Context) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	log.Printf("Starting cluster manager for node %s", cm.nodeID)

	// Add self to nodes
	cm.nodes[cm.nodeID] = &NodeInfo{
		ID:       cm.nodeID,
		Address:  cm.config.AdvertiseAddr,
		Status:   NodeStatusAlive,
		LastSeen: time.Now(),
		Version:  "1.0.0",
		Metadata: make(map[string]string),
	}

	// Start components
	if err := cm.gossip.Start(ctx); err != nil {
		return fmt.Errorf("failed to start gossip protocol: %w", err)
	}

	if err := cm.consensus.Start(ctx); err != nil {
		return fmt.Errorf("failed to start consensus engine: %w", err)
	}

	if err := cm.coordinator.Start(ctx); err != nil {
		return fmt.Errorf("failed to start coordinator: %w", err)
	}

	// Join cluster if seed nodes provided
	if len(cm.config.SeedNodes) > 0 {
		go cm.joinCluster(ctx)
	}

	// Start background tasks
	go cm.monitorCluster(ctx)
	go cm.updateStats(ctx)

	log.Printf("Cluster manager started successfully")
	return nil
}

// Stop stops the cluster manager
func (cm *ClusterManager) Stop() error {
	close(cm.stopCh)

	// Stop components
	if cm.coordinator != nil {
		_ = cm.coordinator.Stop()
	}
	if cm.gossip != nil {
		_ = cm.gossip.Stop()
	}
	if cm.consensus != nil {
		_ = cm.consensus.Stop()
	}

	close(cm.stopped)
	log.Printf("Cluster manager stopped")
	return nil
}

// GetNodeID returns the current node ID
func (cm *ClusterManager) GetNodeID() string {
	return cm.nodeID
}

// IsLeader returns true if this node is the current leader
func (cm *ClusterManager) IsLeader() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.isLeader
}

// GetLeader returns the current leader node ID
func (cm *ClusterManager) GetLeader() string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.leader
}

// GetNodes returns information about all known nodes
func (cm *ClusterManager) GetNodes() map[string]*NodeInfo {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	nodes := make(map[string]*NodeInfo)
	for id, info := range cm.nodes {
		// Create a copy to prevent external modification
		nodeCopy := *info
		nodeCopy.Metadata = make(map[string]string)
		for k, v := range info.Metadata {
			nodeCopy.Metadata[k] = v
		}
		nodes[id] = &nodeCopy
	}
	return nodes
}

// GetStats returns cluster statistics
func (cm *ClusterManager) GetStats() *ClusterStats {
	cm.stats.mu.RLock()
	stats := &ClusterStats{
		TotalNodes:            cm.stats.TotalNodes,
		AliveNodes:            cm.stats.AliveNodes,
		SuspectNodes:          cm.stats.SuspectNodes,
		DeadNodes:             cm.stats.DeadNodes,
		CurrentLeader:         cm.stats.CurrentLeader,
		LeaderElections:       cm.stats.LeaderElections,
		LastElectionTime:      cm.stats.LastElectionTime,
		TotalOperations:       cm.stats.TotalOperations,
		SuccessfulOps:         cm.stats.SuccessfulOps,
		FailedOps:             cm.stats.FailedOps,
		AvgOpLatency:          cm.stats.AvgOpLatency,
		CacheHitRate:          cm.stats.CacheHitRate,
		ReplicationEvents:     cm.stats.ReplicationEvents,
		ConsistencyViolations: cm.stats.ConsistencyViolations,
		MessagesSent:          cm.stats.MessagesSent,
		MessagesReceived:      cm.stats.MessagesReceived,
		NetworkErrors:         cm.stats.NetworkErrors,
	}
	cm.stats.mu.RUnlock()
	return stats
}

// DistributeOperation coordinates a distributed operation across the cluster
func (cm *ClusterManager) DistributeOperation(ctx context.Context, op *DistributedOperation) (*OperationResult, error) {
	if cm.coordinator == nil {
		return nil, fmt.Errorf("coordinator not initialized")
	}

	start := time.Now()
	result, err := cm.coordinator.ExecuteOperation(ctx, op)

	// Update statistics
	cm.stats.mu.Lock()
	cm.stats.TotalOperations++
	if err != nil {
		cm.stats.FailedOps++
	} else {
		cm.stats.SuccessfulOps++
	}

	// Update average latency (exponential moving average)
	latency := time.Since(start)
	if cm.stats.AvgOpLatency == 0 {
		cm.stats.AvgOpLatency = latency
	} else {
		alpha := 0.1
		cm.stats.AvgOpLatency = time.Duration(
			alpha*float64(latency) + (1-alpha)*float64(cm.stats.AvgOpLatency),
		)
	}
	cm.stats.mu.Unlock()

	return result, err
}

// ProposeLeadershipChange proposes a leadership change
func (cm *ClusterManager) ProposeLeadershipChange(ctx context.Context, newLeader string) error {
	if cm.consensus == nil {
		return fmt.Errorf("consensus engine not initialized")
	}

	proposal := &ConsensusProposal{
		Type:      ProposalTypeLeadershipChange,
		Data:      []byte(newLeader),
		Proposer:  cm.nodeID,
		Timestamp: time.Now(),
	}

	return cm.consensus.ProposeChange(ctx, proposal)
}

// Internal methods

func (cm *ClusterManager) joinCluster(ctx context.Context) {
	for _, seedAddr := range cm.config.SeedNodes {
		if seedAddr == cm.config.AdvertiseAddr {
			continue // Don't try to join ourselves
		}

		log.Printf("Attempting to join cluster via seed node: %s", seedAddr)

		if err := cm.gossip.JoinNode(ctx, seedAddr); err != nil {
			log.Printf("Failed to join via %s: %v", seedAddr, err)
			continue
		}

		log.Printf("Successfully joined cluster via %s", seedAddr)
		return
	}

	log.Printf("Failed to join cluster via any seed node")
}

func (cm *ClusterManager) monitorCluster(ctx context.Context) {
	ticker := time.NewTicker(cm.config.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-cm.stopCh:
			return
		case <-ticker.C:
			cm.performHealthChecks()
		}
	}
}

func (cm *ClusterManager) performHealthChecks() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	now := time.Now()
	deadlineTimeout := cm.config.HeartbeatInterval * 3

	for nodeID, node := range cm.nodes {
		if nodeID == cm.nodeID {
			// Update our own last seen time
			node.LastSeen = now
			continue
		}

		timeSinceLastSeen := now.Sub(node.LastSeen)

		switch node.Status {
		case NodeStatusAlive:
			if timeSinceLastSeen > deadlineTimeout {
				node.Status = NodeStatusSuspect
				log.Printf("Node %s marked as suspect (last seen: %v ago)", nodeID, timeSinceLastSeen)
			}
		case NodeStatusSuspect:
			if timeSinceLastSeen > deadlineTimeout*2 {
				node.Status = NodeStatusDead
				log.Printf("Node %s marked as dead (last seen: %v ago)", nodeID, timeSinceLastSeen)

				// If the dead node was the leader, trigger election
				if nodeID == cm.leader {
					cm.leader = ""
					cm.isLeader = false
					go func() {
						_ = cm.consensus.TriggerElection(context.Background())
					}()
				}
			}
		}
	}
}

func (cm *ClusterManager) updateStats(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-cm.stopCh:
			return
		case <-ticker.C:
			cm.calculateClusterStats()
		}
	}
}

func (cm *ClusterManager) calculateClusterStats() {
	cm.mu.RLock()
	nodes := make(map[string]*NodeInfo)
	for id, info := range cm.nodes {
		nodes[id] = info
	}
	leader := cm.leader
	cm.mu.RUnlock()

	cm.stats.mu.Lock()
	defer cm.stats.mu.Unlock()

	// Reset counters
	cm.stats.TotalNodes = len(nodes)
	cm.stats.AliveNodes = 0
	cm.stats.SuspectNodes = 0
	cm.stats.DeadNodes = 0
	cm.stats.CurrentLeader = leader

	totalCacheSize := int64(0)
	totalCacheHitRate := 0.0
	aliveNodesCount := 0

	for _, node := range nodes {
		switch node.Status {
		case NodeStatusAlive:
			cm.stats.AliveNodes++
			totalCacheSize += node.CacheSize
			totalCacheHitRate += node.CacheHitRate
			aliveNodesCount++
		case NodeStatusSuspect:
			cm.stats.SuspectNodes++
		case NodeStatusDead:
			cm.stats.DeadNodes++
		}
	}

	// Calculate average cache hit rate
	if aliveNodesCount > 0 {
		cm.stats.CacheHitRate = totalCacheHitRate / float64(aliveNodesCount)
	}
}

// Helper method to update node information
func (cm *ClusterManager) UpdateNodeInfo(nodeID string, info *NodeInfo) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if existing, exists := cm.nodes[nodeID]; exists {
		// Update existing node
		existing.LastSeen = info.LastSeen
		existing.Status = info.Status
		existing.CPUUsage = info.CPUUsage
		existing.MemoryUsage = info.MemoryUsage
		existing.DiskUsage = info.DiskUsage
		existing.NetworkBandwidth = info.NetworkBandwidth
		existing.CacheSize = info.CacheSize
		existing.CacheHitRate = info.CacheHitRate
		existing.Operations = info.Operations

		// Update metadata
		for k, v := range info.Metadata {
			existing.Metadata[k] = v
		}
	} else {
		// Add new node
		newNode := *info
		newNode.Metadata = make(map[string]string)
		for k, v := range info.Metadata {
			newNode.Metadata[k] = v
		}
		cm.nodes[nodeID] = &newNode
	}
}

// SetLeader updates the cluster leader
func (cm *ClusterManager) SetLeader(nodeID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.leader != nodeID {
		log.Printf("Leadership changed from %s to %s", cm.leader, nodeID)
		cm.leader = nodeID
		cm.isLeader = (nodeID == cm.nodeID)

		cm.stats.mu.Lock()
		cm.stats.LeaderElections++
		cm.stats.LastElectionTime = time.Now()
		cm.stats.CurrentLeader = nodeID
		cm.stats.mu.Unlock()
	}
}

// RemoveNode removes a node from the cluster
func (cm *ClusterManager) RemoveNode(nodeID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if _, exists := cm.nodes[nodeID]; exists {
		delete(cm.nodes, nodeID)
		log.Printf("Node %s removed from cluster", nodeID)

		// If the removed node was the leader, clear leadership
		if nodeID == cm.leader {
			cm.leader = ""
			cm.isLeader = false
		}
	}
}

// GetCoordinator returns the operation coordinator
func (cm *ClusterManager) GetCoordinator() types.DistributedCoordinator {
	return &coordinatorWrapper{cm.coordinator}
}

// coordinatorWrapper adapts Coordinator to DistributedCoordinator interface
type coordinatorWrapper struct {
	*Coordinator
}

func (cw *coordinatorWrapper) ExecuteOperation(ctx context.Context, op interface{}) (interface{}, error) {
	if distOp, ok := op.(*DistributedOperation); ok {
		return cw.Coordinator.ExecuteOperation(ctx, distOp)
	}
	return nil, fmt.Errorf("invalid operation type: %T", op)
}
