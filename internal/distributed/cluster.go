package distributed

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"sync"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
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
	backend     types.Backend
	cache       types.Cache
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

	// SecretFile is the path to the shared cluster secret used to authenticate gossip messages
	// (#206). The secret itself is deliberately not a field here: this configuration is serialized
	// into a file that packaging installs world-readable, so a secret in it would be published to
	// every user on the node. The file must be mode 0600 or startup refuses it.
	//
	// OBJECTFS_CLUSTER_SECRET takes precedence, so this may be empty when the secret is injected
	// through the environment. If neither is set, clustering does not start — see
	// [NewGossipProtocol].
	SecretFile string `yaml:"secret_file"`

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

	// Resource information.
	//
	// MemoryUsage is the Go heap in use as a fraction of the heap obtained from the OS, from
	// runtime.ReadMemStats. It is the process's own memory, not the host's — a node under memory
	// pressure from something else on the box reports a low number here, which is the honest answer to
	// the question this field can actually answer.
	//
	// CPUUsage, DiskUsage, and NetworkBandwidth are not populated and are left at zero. Every one of
	// them needs a platform-specific source that is not in this repository: /proc/stat or host_statistics
	// for CPU, statfs against a cache directory this package does not know about for disk, and interface
	// counters sampled over an interval for bandwidth. Filling a field with a proxy from an unrelated
	// quantity would be worse than leaving it zero — an obviously-zero CPUUsage prompts someone to
	// implement it, while one carrying heap fragmentation looks like a measurement and gets used as one
	// (#132).
	CPUUsage         float64 `json:"cpu_usage"`
	MemoryUsage      float64 `json:"memory_usage"`
	DiskUsage        float64 `json:"disk_usage"`
	NetworkBandwidth int64   `json:"network_bandwidth"`

	// Cache statistics. Populated from the injected cache's own Stats, so they are absent rather than
	// zero when no cache is set — see refreshLocalStats.
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

	// Cache coordination.
	//
	// CacheHitRate is the mean across alive nodes and TotalCacheSize their sum, which is why the two are
	// combined differently: a rate averaged over nodes answers "how well is the cluster caching",
	// while a size summed answers "how much is cached", and summing rates or averaging sizes answers
	// nothing. TotalCacheSize was accumulated and then discarded with `_ =` until v0.11.0 — the value
	// was correct and simply never assigned anywhere (#132).
	CacheHitRate          float64 `json:"cache_hit_rate"`
	TotalCacheSize        int64   `json:"total_cache_size"`
	ReplicationEvents     int64   `json:"replication_events"`
	ConsistencyViolations int64   `json:"consistency_violations"`

	// Network
	MessagesSent     int64 `json:"messages_sent"`
	MessagesReceived int64 `json:"messages_received"`
	NetworkErrors    int64 `json:"network_errors"`
}

// defaultMaxGossipPacket is the largest gossip datagram this node will send or accept, in bytes.
//
// 8 KiB, chosen against measurement rather than round numbers. A sealed sync message costs ~200 bytes
// of envelope plus ~415 bytes per member, so this carries about 19 members in a single datagram and
// larger memberlists in chunks — see [GossipProtocol.marshalSyncChunksLocked].
//
// The old default was 1024, which holds *two* members. A three-node cluster's sync did not fit, the
// datagram was truncated by the receive buffer, and the truncated JSON then failed the authentication
// envelope parse — so cluster formation stalled and reported it as a wrong cluster secret. No CI job
// builds -tags=distributed, so the one test that would have caught it had been failing unobserved
// (#277, #240).
//
// Why not 65507, the maximum UDP payload: a datagram over the path MTU is fragmented by IP, and a
// single lost fragment loses the whole datagram, so the effective loss rate rises with size. 8 KiB
// exceeds a 1500-byte MTU and will fragment, which is a deliberate trade — six fragments for a sync
// that runs once per join is worth more than chunking every small cluster's membership across several
// round trips. Per-round alive messages are ~483 bytes sealed and never fragment.
const defaultMaxGossipPacket = 8192

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
		config.MaxGossipPacket = defaultMaxGossipPacket
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
		// CacheReplication only, and then applyConfigDefaults below fills the rest.
		//
		// This used to restate all sixteen fields, every one of which applyConfigDefaults sets to the
		// same value two lines later — so the literal was dead except for this one field, and the
		// duplication was live: MaxGossipPacket appeared here as 1024 and had to be changed in two
		// places, which is how a stale copy of a default survives a fix to the default (#277).
		//
		// CacheReplication is the exception because it is a bool whose default is true, and a bool's
		// zero value is indistinguishable from "not set" — so applyConfigDefaults cannot express it and
		// a caller passing nil is the only case where the intent is known.
		config = &ClusterConfig{CacheReplication: true}
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
	cm.coordinator, err = NewCoordinator(cm, config, nil)
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

// Start starts the cluster manager.
//
// When it returns, [ClusterManager.GetStats] already describes the cluster: the statistics are
// computed once here rather than left to the first tick of the background refresher. Until v0.11.0
// they were not, so for the first five seconds of every process GetStats reported zero nodes while
// GetNodes correctly reported one — two accessors over the same membership disagreeing, and the one an
// operator or a health check reads being the wrong one (#275).
func (cm *ClusterManager) Start(ctx context.Context) error {
	if err := cm.startLocked(ctx); err != nil {
		return err
	}

	// After startLocked releases cm.mu, because calculateClusterStats takes it for reading and a Go
	// RWMutex is not reentrant.
	cm.calculateClusterStats()

	slog.Info("cluster manager started successfully")
	return nil
}

// startLocked registers this node and starts the components and background tasks, holding cm.mu for
// the whole of it.
func (cm *ClusterManager) startLocked(ctx context.Context) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	slog.Info("starting cluster manager", "node_id", cm.nodeID)

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
	slog.Info("cluster manager stopped")
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
		maps.Copy(nodeCopy.Metadata, info.Metadata)
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
		TotalCacheSize:        cm.stats.TotalCacheSize,
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

		slog.Info("attempting to join cluster via seed node", "seed_addr", seedAddr)

		if err := cm.gossip.JoinNode(ctx, seedAddr); err != nil {
			slog.Warn("failed to join via seed node", "seed_addr", seedAddr, "error", err)
			continue
		}

		slog.Info("successfully joined cluster", "seed_addr", seedAddr)
		return
	}

	slog.Warn("failed to join cluster via any seed node")
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
			cm.performHealthChecks(ctx)
		}
	}
}

func (cm *ClusterManager) performHealthChecks(ctx context.Context) {
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
				slog.Info("node marked as suspect", "node_id", nodeID, "last_seen_ago", timeSinceLastSeen)
			}
		case NodeStatusSuspect:
			if timeSinceLastSeen > deadlineTimeout*2 {
				node.Status = NodeStatusDead
				slog.Info("node marked as dead", "node_id", nodeID, "last_seen_ago", timeSinceLastSeen)

				// If the dead node was the leader, trigger election
				if nodeID == cm.leader {
					cm.leader = ""
					cm.isLeader = false
					go func() {
						// Use the cluster lifecycle context so the election
						// goroutine exits cleanly when the manager stops (#110).
						_ = cm.consensus.TriggerElection(ctx)
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
	var (
		totalNodes     int
		aliveNodes     int
		suspectNodes   int
		deadNodes      int
		leader         string
		totalCacheSize int64
		totalHitRate   float64
	)

	// Tallied under cm.mu, not from a copy taken under it.
	//
	// This used to maps.Copy cm.nodes and then walk the copy after unlocking — which looks
	// synchronized and is not, because the copy holds the same *NodeInfo pointers. Every field read
	// below therefore hit a struct UpdateNodeInfo was concurrently writing from the gossip receive
	// goroutine, and -race reported it on any cluster where a node is still announcing itself while
	// the stats ticker fires, which is every cluster (#278). GetNodes does this correctly, copying
	// each NodeInfo by value inside the critical section; this function is where the pattern was
	// missed.
	//
	// Only cm.mu is held here: the results land in cm.stats afterwards, so the two locks are never
	// nested and no order between them has to be established.
	cm.mu.RLock()
	totalNodes = len(cm.nodes)
	leader = cm.leader
	for _, node := range cm.nodes {
		switch node.Status {
		case NodeStatusAlive:
			aliveNodes++
			totalCacheSize += node.CacheSize
			totalHitRate += node.CacheHitRate
		case NodeStatusSuspect:
			suspectNodes++
		case NodeStatusDead:
			deadNodes++
		}
	}
	cm.mu.RUnlock()

	cm.stats.mu.Lock()
	defer cm.stats.mu.Unlock()

	cm.stats.TotalNodes = totalNodes
	cm.stats.AliveNodes = aliveNodes
	cm.stats.SuspectNodes = suspectNodes
	cm.stats.DeadNodes = deadNodes
	cm.stats.CurrentLeader = leader
	cm.stats.TotalCacheSize = totalCacheSize

	// Left at its previous value when no node is alive, rather than zeroed: a hit rate of zero is a
	// cache that never hits, which is not what "there was nothing to average" means.
	if aliveNodes > 0 {
		cm.stats.CacheHitRate = totalHitRate / float64(aliveNodes)
	}
}

// refreshLocalStats reads this node's current resource and cache figures into dst.
//
// It is called once per gossip round rather than on a ticker of its own, because the only consumer is
// the alive message that immediately follows: sampling on an independent schedule would publish
// figures from an arbitrary point before the send with no benefit. ReadMemStats stops the world
// briefly, which is why this is bounded by the gossip interval and not called per request (#132).
//
// The fields it does not touch are as deliberate as the ones it sets; see the NodeInfo comments.
func (cm *ClusterManager) refreshLocalStats(dst *NodeInfo) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	// HeapSys is what the process obtained from the OS and HeapInuse what it is actually using, so the
	// ratio is how much of its own footprint is live rather than fragmentation. HeapSys is zero only
	// before the first allocation, which cannot happen here, but the guard costs nothing and a division
	// by zero would put a NaN into a field a load balancer compares.
	if mem.HeapSys > 0 {
		dst.MemoryUsage = float64(mem.HeapInuse) / float64(mem.HeapSys)
	}

	cm.mu.RLock()
	cache := cm.cache
	cm.mu.RUnlock()

	// Left untouched when no cache is injected, rather than zeroed. Zero is a meaningful cache size —
	// an empty cache — and reporting it for "there is no cache" would make a node with no cache look
	// like the emptiest and therefore most attractive one to a size-aware strategy.
	if cache != nil {
		cacheStats := cache.Stats()
		dst.CacheSize = cacheStats.Size
		dst.CacheHitRate = cacheStats.HitRate
	}

	cm.stats.mu.RLock()
	dst.Operations = cm.stats.TotalOperations
	cm.stats.mu.RUnlock()
}

// UpdateNodeInfo merges a node's reported state into the membership map, inserting it if unknown.
//
// The merge is selective, and which fields it leaves alone is the part worth knowing: an existing
// entry keeps its ID, Address and Version, taking only LastSeen, Status, the four resource fields,
// the two cache fields and Operations from info. So this cannot rename or re-address a node that is
// already a member — a gossip message claiming a new address for a known ID updates its liveness and
// nothing else. Metadata is merged key-by-key rather than replaced, so a key absent from info
// survives.
//
// On insert, info is copied by value and its Metadata map is cloned rather than aliased, which
// matters because of what the callers do with the same pointer: every non-test call site is in
// gossip.go, and handleJoinMessage stores that identical *NodeInfo as gp.memberlist[nodeID].Info
// before passing it here. Aliasing the map would leave the gossip memberlist and the cluster
// membership map sharing one map under two different locks.
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
		maps.Copy(existing.Metadata, info.Metadata)
	} else {
		// Add new node
		newNode := *info
		newNode.Metadata = make(map[string]string)
		maps.Copy(newNode.Metadata, info.Metadata)
		cm.nodes[nodeID] = &newNode
	}
}

// expectsPeers reports whether this node is configured to join an existing cluster rather than to be
// the whole of a new one.
//
// It is the seed list, minus this node's own address, exactly as [ClusterManager.joinCluster] treats
// it: a seed pointing at ourselves is not a peer to wait for. [ConsensusEngine.checkVoteMajority] uses
// this to decide whether a membership view of one is a cluster of one or a cluster whose other members
// have not been discovered yet — the two are indistinguishable from the membership map alone, and
// electing a leader on the wrong answer is a split brain (#275).
//
// No lock: cm.config is written once by NewClusterManager and never mutated, which is why joinCluster
// reads SeedNodes unlocked too.
func (cm *ClusterManager) expectsPeers() bool {
	for _, seed := range cm.config.SeedNodes {
		if seed != cm.config.AdvertiseAddr {
			return true
		}
	}
	return false
}

// SetLeader updates the cluster leader
func (cm *ClusterManager) SetLeader(nodeID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.leader != nodeID {
		slog.Info("leadership changed", "from", cm.leader, "to", nodeID)
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
		slog.Info("node removed from cluster", "node_id", nodeID)

		// If the removed node was the leader, clear leadership
		if nodeID == cm.leader {
			cm.leader = ""
			cm.isLeader = false
		}
	}
}

// SetBackend injects the S3 backend used by executeLocally for real object
// operations.  Must be called before any distributed operations are executed.
func (cm *ClusterManager) SetBackend(b types.Backend) {
	cm.mu.Lock()
	cm.backend = b
	if cm.coordinator != nil {
		cm.coordinator.backend = b
	}
	cm.mu.Unlock()
}

// SetCache injects the cache instance for distributed invalidation.
// When set, cache-invalidate gossip messages received from peers will call
// cache.Delete on the local cache.
func (cm *ClusterManager) SetCache(c types.Cache) {
	cm.mu.Lock()
	cm.cache = c
	cm.mu.Unlock()
}

// InvalidateCacheKey broadcasts a cache-invalidate message to all alive peers,
// causing each peer to evict the given key from its local cache.
func (cm *ClusterManager) InvalidateCacheKey(key string) {
	cm.mu.RLock()
	gossip := cm.gossip
	nodeID := cm.nodeID
	cm.mu.RUnlock()

	if gossip == nil {
		return
	}
	payload, err := json.Marshal(CacheInvalidateMessage{Key: key, From: nodeID})
	if err != nil {
		return
	}
	_ = gossip.broadcastMessage(&GossipMessage{
		Type:      MessageTypeCacheInvalidate,
		From:      nodeID,
		Timestamp: time.Now(),
		MessageID: gossip.generateMessageID(),
		Data:      payload,
	})
}

// GetCoordinator returns the operation coordinator
func (cm *ClusterManager) GetCoordinator() types.DistributedCoordinator {
	return &coordinatorWrapper{cm.coordinator}
}

// coordinatorWrapper adapts Coordinator to DistributedCoordinator interface
type coordinatorWrapper struct {
	*Coordinator
}

func (cw *coordinatorWrapper) ExecuteOperation(ctx context.Context, op any) (any, error) {
	if distOp, ok := op.(*DistributedOperation); ok {
		return cw.Coordinator.ExecuteOperation(ctx, distOp)
	}
	return nil, fmt.Errorf("invalid operation type: %T", op)
}
