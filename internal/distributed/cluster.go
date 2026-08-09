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

	// EnableConsensus starts the Raft engine — elections, heartbeats and the replicated log. It is
	// **off by default**, and a mount does not turn it on.
	//
	// Coordination in ObjectFS is compare-and-swap against S3, not Raft: a conditional write is
	// evaluated by the store, needs no quorum, and keeps working with one node reachable. See
	// docs/design/conditional-writes-vs-raft.md. What a mount enables clustering *for* is the gossip
	// layer — membership, cache invalidation, and the key announcements that let a cold node learn
	// which objects are worth warming (#140, #142) — and none of that consults a leader.
	//
	// So this is not a performance switch. Starting consensus on a mount would put leader election on
	// the path a filesystem read takes, to decide nothing that path asks about, and would make a
	// cluster that cannot hold a quorum degrade a mount that has no need of one. Left in the tree and
	// reachable because the `-tags=distributed` suite drives elections deliberately, and because
	// nothing outside this package calls [ClusterManager.IsLeader] or [ClusterManager.GetLeader] — the
	// seam is clean, which is what makes leaving it unstarted a one-line decision rather than a
	// refactor.
	EnableConsensus bool `yaml:"enable_consensus"`

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

	// ReplicationFactor is how many nodes [Coordinator.selectTargetNodes] returns for an operation.
	//
	// Only the first is used — see [Coordinator.executeOnce] — because there is one copy of the bytes
	// and S3 holds it. The rest are the preference order a follow-on strategy that genuinely needs
	// peers would consume, so this is a selection width rather than a count of copies made.
	ReplicationFactor int `yaml:"replication_factor"`

	// #284 removed two keys from this block. `consistency_level` took "eventual", "strong" or
	// "session", and all three issued the same unconditional PutObject — see
	// [DistributedOperation.Precondition] for what replaced it. `cache_replication` was a bool read by
	// no code in the repository, and the subsystem whose name it carried put an object's own bytes back
	// to itself on N-1 peers; both it and the CacheReplicator are gone. Actual cache warming is #141.

	// AnnouncementTTL is how long a peer's claim to hold a key in its cache is believed (#140).
	//
	// Tuning, not correctness: what expiry decides is whether this node bothers asking a peer's cache
	// about a key, and [types.DistributedCoordinator] already requires every caller to check what comes
	// back against the ETag it asked for — because a holder can evict at any moment, TTL or no TTL. Too
	// long wastes a round trip on a peer that has evicted; too short sends a read to S3 that a peer could
	// have served. Both are slower and neither is wrong.
	//
	// Left at zero it becomes defaultAnnouncementTTL. Sized against the traversals warming exists for —
	// see that constant for the reasoning.
	AnnouncementTTL time.Duration `yaml:"announcement_ttl"`

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

	// CacheCapacity is what the node's cache reports it can hold, and zero means it does not report one
	// rather than that it can hold nothing. The Redis-backed cache is the real case — it has no capacity
	// of its own to publish, so [types.CacheStats.Capacity] comes back zero — while the in-process
	// multi-level cache sums the configured maximum of each enabled level.
	//
	// CacheRequests is hits plus misses, and it is here so that CacheHitRate can be told apart from a
	// cache that has served nothing. Those are different states and the rate alone cannot express them:
	// zero requests gives a rate of 0.0 by arithmetic, which is identical to a cache that misses every
	// single read. The first is a mount that started a second ago and the second is an emergency. This is
	// the same class of defect as #222 — fields declared, never assigned, published as zeros — except
	// that here the zero is genuinely computed and still means nothing, which is harder to spot.
	//
	// Both are advertised in the alive message like the two above, so a peer's figures are as readable
	// as the local node's. That is what costs them their place on the wire: at ~40 bytes per member they
	// take the sealed sync from ~415 to ~455 bytes per member, so the 8 KiB default datagram carries
	// about 17 members instead of 19 — see defaultMaxGossipPacket, and
	// TestNewClusterManager_DefaultMaxGossipPacketHoldsAThreeNodeSync, which is the test that notices
	// when NodeInfo grows.
	CacheCapacity int64 `json:"cache_capacity"`
	CacheRequests int64 `json:"cache_requests"`
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
		// Empty, and then applyConfigDefaults below fills every field.
		//
		// This used to restate all sixteen fields, every one of which applyConfigDefaults sets to the
		// same value two lines later — so the literal was dead, and the duplication was live:
		// MaxGossipPacket appeared here as 1024 and had to be changed in two places, which is how a
		// stale copy of a default survives a fix to the default (#277). One field, CacheReplication,
		// then had to stay because it was a bool defaulting to true and a bool's zero value cannot be
		// told from "not set"; #284 deleted that field with the replicator it named, so there is nothing
		// left here that applyConfigDefaults cannot express.
		config = &ClusterConfig{}
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

	// Consensus is opt-in; see [ClusterConfig.EnableConsensus] for why a mount leaves it off. The
	// engine is constructed either way, so the gossip receive loop's vote and append-entries arms stay
	// harmless rather than needing their own guard: with no election loop running, this node never
	// becomes a candidate and never has a term to defend.
	if cm.config.EnableConsensus {
		if err := cm.consensus.Start(ctx); err != nil {
			return fmt.Errorf("failed to start consensus engine: %w", err)
		}
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

// GossipAddr returns the address the gossip socket is actually bound to, or "" if nothing is bound.
//
// The bound address rather than the configured one, which is the only version worth reporting: a
// cluster manager whose Start failed is non-nil and reports its configuration back just as happily as
// a running one, and `listen_addr: 127.0.0.1:0` — what a test asks for so the kernel picks the port —
// is not an address any peer can be told about until the bind has happened.
func (cm *ClusterManager) GossipAddr() string {
	cm.mu.RLock()
	gossip := cm.gossip
	cm.mu.RUnlock()

	if gossip == nil {
		return ""
	}

	return gossip.LocalAddr()
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

// There was a ProposeLeadershipChange here, and #284 deleted it with the proposal machinery it was
// the only caller of. It built a ConsensusProposal naming a node and handed it to
// ConsensusEngine.ProposeChange, which broadcast it — meaning it slept 100ms and then voted for its
// own proposal, so a majority of one was found and executeProposal called ClusterManager.SetLeader.
//
// A node cannot be made leader by being named. [ConsensusEngine] contests leadership with terms,
// votes and a randomized election timeout, and that path reaches SetLeader having actually won; a
// second path that reaches the same setter after a self-vote does not transfer leadership, it just
// overwrites one node's opinion of who holds it. Nothing outside a test called this.

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

				// If the dead node was the leader, trigger election.
				//
				// Unreachable with consensus off, since cm.leader is only ever set by an election, but
				// gated explicitly rather than left to that: relying on it would mean this arm's
				// correctness depends on no other writer of cm.leader ever appearing.
				if nodeID == cm.leader {
					cm.leader = ""
					cm.isLeader = false
					if cm.config.EnableConsensus {
						go func() {
							// Use the cluster lifecycle context so the election
							// goroutine exits cleanly when the manager stops (#110).
							_ = cm.consensus.TriggerElection(ctx)
						}()
					}
				}
			}

		// Anything else is unreapable without this arm, and NodeStatus is a string that arrives over
		// the wire: UpdateNodeInfo assigns info.Status straight from a json.Unmarshal of a gossip
		// message, so a peer running a different version — or anything at all speaking to this port —
		// can put a value here that no arm above names. Probed on a three-node manager: a peer at
		// Status "from-the-wire", last seen an hour ago, kept that status across performHealthChecks
		// forever, because the switch simply fell through. It also counted in TotalNodes while
		// appearing in none of alive/suspect/dead — the tally read total=3 alive=1 suspect=0 dead=0.
		//
		// Suspect rather than dead: this node has been heard from, so what is unknown is its state and
		// not its existence, and the suspect arm above will reap it on the next pass if it stays quiet.
		// NodeStatusJoining and NodeStatusLeaving reach here too. They are declared in this file and
		// assigned nowhere in the repository, so today this arm is what would handle them if anything
		// ever did.
		default:
			if timeSinceLastSeen > deadlineTimeout {
				slog.Warn("node has an unrecognized status and is being treated as suspect",
					"node_id", nodeID, "status", node.Status, "last_seen_ago", timeSinceLastSeen)
				node.Status = NodeStatusSuspect
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
	// This node's own entry first, or the tally below excludes it.
	//
	// Until this call existed, refreshLocalStats reached only gp.localNode — the struct the *alive
	// message* is built from — so a node published its cache figures to every peer and never recorded
	// them in its own membership map. Two consequences, both visible from a running two-node cluster:
	// TotalCacheSize summed every node's cache except the local one, and [ClusterManager.StatusSnapshot]
	// reported "cache not reported" for the node being asked while reporting its peer's figures in full.
	// The existing test for the sum encodes the old behavior in a comment — "plus whatever the self node
	// reports — zero here, since nothing refreshed it" — which is the gap observed and not closed.
	cm.refreshSelfEntry()

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

		// Counted somewhere rather than nowhere. Without this arm a node whose status no arm names is
		// included in TotalNodes and in none of the three breakdowns, so the numbers a reader would
		// naturally expect to add up do not: a probe of three nodes, one of them at a status arriving
		// from a gossip message, reported total=3 alive=1 suspect=0 dead=0 and lost the third silently.
		//
		// Suspect is where it belongs for the same reason performHealthChecks now puts it there, and
		// consistency between the two matters more than the individual choice — the health check reaps
		// on the suspect timer, so a node counted suspect here is a node that is actually on the path
		// this count implies. Its CacheSize and CacheHitRate are deliberately not summed: those
		// aggregates describe capacity this cluster can use, and a node in an unknown state is not it.
		default:
			suspectNodes++
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

// refreshSelfEntry samples this node's own figures into its entry in the membership map.
//
// Sampled outside cm.mu and assigned under it, because refreshLocalStats takes cm.mu for reading to get
// at the cache and a Go RWMutex is not reentrant — the same reason [ClusterManager.Start] calls
// calculateClusterStats after startLocked has released the lock, and the same reason performGossip
// samples before taking gp.mu.
//
// A no-op for a manager whose Start never ran, since Start is what inserts the entry. Nothing is created
// here: a self entry appearing in the membership map without Start having run would be a node counted as
// alive by a manager that is not.
func (cm *ClusterManager) refreshSelfEntry() {
	// Seeded with a sentinel the four cache fields cannot legitimately take, because refreshLocalStats
	// signals "there is no cache" by leaving them *untouched* rather than by zeroing them — see its
	// comment, and TestRefreshLocalStats_LeavesCacheFieldsAloneWithNoCache, which pins that contract.
	// Reading the sentinel back is how this distinguishes an absent cache from an empty one, and the
	// distinction is the whole reason the two are not the same value: copying an unmeasured zero onto the
	// self entry would report a cache that exists and holds nothing.
	const unset = -1

	fresh := NodeInfo{
		CacheSize: unset, CacheHitRate: unset, CacheCapacity: unset, CacheRequests: unset,
	}
	cm.refreshLocalStats(&fresh)

	cm.mu.Lock()
	defer cm.mu.Unlock()

	self, exists := cm.nodes[cm.nodeID]
	if !exists {
		return
	}

	// The same fields UpdateNodeInfo takes from a peer's alive message, so this node describes itself the
	// way its peers describe it. ID, Address, Status and Version are deliberately not touched, for the
	// reason UpdateNodeInfo does not touch them either.
	self.MemoryUsage = fresh.MemoryUsage
	self.Operations = fresh.Operations

	if fresh.CacheSize != unset {
		self.CacheSize = fresh.CacheSize
		self.CacheHitRate = fresh.CacheHitRate
		self.CacheCapacity = fresh.CacheCapacity
		self.CacheRequests = fresh.CacheRequests
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
		dst.CacheCapacity = cacheStats.Capacity

		// The denominator behind HitRate, carried so a reader can tell 0.0 from "nothing asked yet". Both
		// counters are cumulative over the cache's life, so this only ever grows.
		dst.CacheRequests = int64(cacheStats.Hits + cacheStats.Misses) //nolint:gosec // see below
		// The conversion cannot overflow in any real process: it would take 2^63 cache operations, and at
		// a billion per second that is 292 years. Signed because everything else on this struct is, and a
		// mixed-signedness pair of related fields is worse than the theoretical wrap.
	}

	cm.stats.mu.RLock()
	dst.Operations = cm.stats.TotalOperations
	cm.stats.mu.RUnlock()
}

// UpdateNodeInfo merges a node's reported state into the membership map, inserting it if unknown.
//
// The merge is selective, and which fields it leaves alone is the part worth knowing: an existing
// entry keeps its ID, Address and Version, taking only LastSeen, Status, the four resource fields,
// the four cache fields and Operations from info. So this cannot rename or re-address a node that is
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
		existing.CacheCapacity = info.CacheCapacity
		existing.CacheRequests = info.CacheRequests
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
	coordinator := cm.coordinator
	cm.mu.Unlock()

	// Set through the coordinator's own accessor rather than by assigning the field here. This used to
	// write cm.coordinator.backend under cm.mu, which is not the lock the reader holds:
	// [Coordinator.executeLocally] reads it from the gossip receive goroutine, so an injection
	// concurrent with a peer's operation was a data race on the interface value — caught by -race once
	// a test made a remote operation and an injection overlap.
	if coordinator != nil {
		coordinator.setBackend(b)
	}
}

// SetCache injects the cache instance for distributed invalidation.
// When set, cache-invalidate gossip messages received from peers will call
// cache.Delete on the local cache.
func (cm *ClusterManager) SetCache(c types.Cache) {
	cm.mu.Lock()
	cm.cache = c
	cm.mu.Unlock()
}

// InvalidateCacheKey broadcasts a cache-invalidate message to all alive peers, causing each peer to
// evict key from its local cache.
//
// etag names the version the caller just wrote, and should be [OperationResult.ETag] from the write
// that prompted this. Passing it is what lets a receiver discard an invalidation it has already
// applied: gossip is unordered and retransmits, so an unversioned invalidation can be replayed after a
// later one and evict bytes the peer has since legitimately re-cached (requirement R4 of
// docs/design/conditional-writes-vs-raft.md §1).
//
// Empty is allowed and means the caller cannot name a version — an unconditional put whose ETag was
// not read back. Such a message is applied by every receiver every time, which is safe but wasteful,
// so prefer a conditional write's reported ETag where there is one.
//
// An error means the invalidation was not broadcast, so peers may still serve the version it was meant
// to evict. It is not an acknowledgement that any of them acted on it: gossip has none to give.
func (cm *ClusterManager) InvalidateCacheKey(key, etag string) error {
	return cm.invalidateCacheKey(CacheInvalidateMessage{Key: key, ETag: etag})
}

// InvalidateDeletedCacheKey is InvalidateCacheKey for a key that was removed rather than replaced.
//
// It exists because a receiver cannot tell the two apart from the ETag: a delete has no version to
// name, and neither does an unconditional put, so an empty ETag alone is ambiguous.
func (cm *ClusterManager) InvalidateDeletedCacheKey(key string) error {
	return cm.invalidateCacheKey(CacheInvalidateMessage{Key: key, Deleted: true})
}

// invalidateCacheKey broadcasts m, filling in the sending node.
//
// It reports an error when it could not send rather than returning silently, which is what it used to
// do when gossip was not running. That silence is the shape of defect #284 deleted: a caller invalidating
// before Start got the same answer as one whose peers were all reached, and the difference is whether
// other nodes are still serving bytes this key just replaced.
//
// A running cluster with no peers is a nil error, since there is nobody to tell and nothing went wrong.
func (cm *ClusterManager) invalidateCacheKey(m CacheInvalidateMessage) error {
	cm.mu.RLock()
	gossip := cm.gossip
	nodeID := cm.nodeID
	cm.mu.RUnlock()

	if gossip == nil {
		return fmt.Errorf("cannot invalidate %q on peers: this cluster has no gossip protocol, so it has "+
			"no way to reach them: %w", m.Key, types.ErrNotSupported)
	}

	m.From = nodeID
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshaling the invalidation for %q: %w", m.Key, err)
	}

	return gossip.broadcastMessage(&GossipMessage{
		Type:      MessageTypeCacheInvalidate,
		From:      nodeID,
		Timestamp: time.Now(),
		MessageID: gossip.generateMessageID(),
		Data:      payload,
	})
}

// GetCoordinator returns the operation coordinator
func (cm *ClusterManager) GetCoordinator() types.DistributedCoordinator {
	return &coordinatorWrapper{cm.getCoordinator()}
}

// getCoordinator returns the coordinator, which may be nil.
//
// Deliberately without cm.mu, unlike almost everything else that touches a ClusterManager field. This one
// is assigned exactly once, in [NewClusterManager], before any goroutine that could read it exists — the
// `go` statements that start the receive loop and the background tasks are all downstream of that
// assignment, so publication is already ordered and a lock would only add contention. It would add it in
// the worst place, too: [GossipProtocol.handleCacheAnnounce] calls this from the receive goroutine, and
// startLocked holds cm.mu for *writing* across gossip.Start, so a lock here would stall inbound messages
// behind the rest of startup for no benefit.
//
// Nil is therefore not a startup window but a ClusterManager that did not come from the constructor.
// Guarded anyway, and for the same reason [coordinatorWrapper.AnnounceKey] guards: the alternative is a
// panic on the gossip receive goroutine, which takes the whole cluster's membership down with it.
func (cm *ClusterManager) getCoordinator() *Coordinator {
	return cm.coordinator
}

// coordinatorWrapper adapts Coordinator to DistributedCoordinator interface
type coordinatorWrapper struct {
	*Coordinator
}

// coordinatorWrapper must satisfy the whole interface, checked here rather than only at the one
// GetCoordinator return that happens to exercise it today.
var _ types.DistributedCoordinator = (*coordinatorWrapper)(nil)

func (cw *coordinatorWrapper) ExecuteOperation(ctx context.Context, op any) (any, error) {
	if distOp, ok := op.(*DistributedOperation); ok {
		return cw.Coordinator.ExecuteOperation(ctx, distOp)
	}
	return nil, fmt.Errorf("invalid operation type: %T", op)
}

// AnnounceKey tells peers this node holds ann's bytes. Implemented in #140; see
// [Coordinator.AnnounceKey].
//
// The nil check is what the stub this replaced was for. Both methods returned [types.ErrNotSupported]
// unconditionally, and the reasoning recorded there survives the implementation and is why this guard
// exists rather than a bare delegation: a nil answer here would mean "announced" to every caller, and
// #284 deleted a CacheReplicator that did exactly that — it counted bytes as replicated when gossip was
// not running and it had sent nothing, and it survived four releases because the only test covering it
// asserted that its field was non-nil. A caller told ErrNotSupported falls back to the object store,
// which is correct and merely slower.
func (cw *coordinatorWrapper) AnnounceKey(ctx context.Context, ann types.KeyAnnouncement) error {
	if cw.Coordinator == nil {
		return fmt.Errorf("cannot announce %q: this coordinator has no cluster: %w", ann.Key,
			types.ErrNotSupported)
	}

	return cw.Coordinator.AnnounceKey(ctx, ann)
}

// QueryKeyOwnership reports the peers claiming to hold key. Implemented in #140; see
// [Coordinator.QueryKeyOwnership].
//
// Note what the guard does *not* do: return an empty slice and a nil error. That is the documented answer
// for a key no peer has cached, so giving it for "there is no coordinator" would be indistinguishable
// from a working query against a cold cluster — and a caller measuring warming hit rates would read a
// flat zero as "warming does not help" rather than "warming is not running".
func (cw *coordinatorWrapper) QueryKeyOwnership(ctx context.Context, key string) ([]types.KeyAnnouncement, error) {
	if cw.Coordinator == nil {
		return nil, fmt.Errorf("cannot query which peers hold %q: this coordinator has no cluster: %w",
			key, types.ErrNotSupported)
	}

	return cw.Coordinator.QueryKeyOwnership(ctx, key)
}

// InvalidateKey broadcasts a cache invalidation for key at etag. This one is real: the message type,
// the sender, and the receiver's replay-suppressing ledger all landed in #284.
func (cw *coordinatorWrapper) InvalidateKey(_ context.Context, key, etag string) error {
	// The embedded pointer is checked by name because the selector below reads through it; || short-circuits,
	// so cw.cluster is only evaluated once there is a Coordinator to read it from.
	if cw.Coordinator == nil || cw.cluster == nil {
		return fmt.Errorf("cannot invalidate %q: this coordinator has no cluster: %w", key,
			types.ErrNotSupported)
	}

	return cw.cluster.InvalidateCacheKey(key, etag)
}
