package distributed

import (
	"sort"
	"time"
)

// ClusterStatus is the cluster state a node can report about itself and its peers, as served at
// /health/cluster and rendered by `objectfs cluster status`.
//
// Every field here is one that is actually assigned at runtime on the path a mount takes. That
// constraint is the whole design of this type, and it is why it is a distinct struct rather than
// [ClusterStats] with JSON tags: ClusterStats declares eighteen fields, and on a mount, eight of them
// are never written by anything. MessagesSent, MessagesReceived, NetworkErrors, ReplicationEvents and
// ConsistencyViolations are assigned nowhere in the repository; LeaderElections, LastElectionTime and
// CurrentLeader are assigned only by [ClusterManager.SetLeader], which only the consensus engine
// reaches, and a mount does not start it. Publishing those as zeros is the defect this project has
// already shipped twice — percentile fields declared and never assigned (#222), and six resource fields
// broadcast as zeros for four releases (#132) — because a reader cannot tell a measured zero from an
// unmeasured one.
//
// Where a zero would be ambiguous the field is a pointer, so a JSON consumer sees null rather than 0
// and a human reader is told "not measured" rather than "0%".
type ClusterStatus struct {
	// Enabled is false when this instance is not running cluster coordination at all, which is the
	// ordinary case: `cluster.enabled` defaults to false. Reason then says so.
	//
	// It exists so that a client can tell three situations apart that otherwise look alike. Nothing
	// listening is a connection error and never reaches this struct. An instance that predates this
	// endpoint answers 404. An instance that is running fine with clustering off answers 200 with
	// Enabled false — which is not a fault and must not be reported as one.
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`

	NodeID string `json:"node_id,omitempty"`

	// GossipAddr is the address the gossip socket is bound to, from [ClusterManager.GossipAddr] — the
	// bound address and not the configured one, so `listen_addr: 127.0.0.1:0` reports the port the
	// kernel actually assigned. Empty means nothing is bound, which for a started manager means the
	// bind failed.
	GossipAddr string `json:"gossip_addr,omitempty"`

	// Leadership is non-nil only when [ClusterConfig.EnableConsensus] is set, and a mount never sets it:
	// there is no YAML key for it, and [ClusterManager.startLocked] leaves the engine unstarted.
	//
	// Nil rather than a zero-valued struct, because the two are not the same statement. Coordination in
	// ObjectFS is compare-and-swap against S3 — evaluated by the store on one request, needing no quorum
	// — so no leader is elected and IsLeader is false on every node of a perfectly healthy cluster.
	// Rendering that as "Role: Follower", which is what the issue asking for this command specified,
	// tells an operator their node lost an election that was never held.
	Leadership *LeadershipStatus `json:"leadership,omitempty"`

	Membership MembershipStatus `json:"membership"`

	// Self is this node's own entry in the membership map. Nil only for a manager whose Start never ran,
	// since Start is what registers it.
	Self  *NodeReport  `json:"self,omitempty"`
	Peers []NodeReport `json:"peers"`

	Cache  ClusterCacheStatus `json:"cache"`
	Gossip GossipCounters     `json:"gossip"`

	// AnnouncedKeys is how many distinct keys this node retains peer cache claims for (#140), expired
	// ones included — see [Coordinator.announcedKeys].
	//
	// It is emphatically not "top keys by access", which the issue's mockup asked for and which nothing
	// in this repository measures: there is no per-key read counter reachable from the cluster layer and
	// no windowed one anywhere. The closest thing that exists, [Coordinator.recentHoldings], orders keys
	// by when this node last *announced* them, which is a different quantity — offering it under the
	// other one's label is how a proxy from an unrelated measurement comes to be read as the real thing.
	AnnouncedKeys int `json:"announced_keys"`
}

// LeadershipStatus is who holds leadership, reported only when consensus is running.
type LeadershipStatus struct {
	Leader   string `json:"leader"`
	IsSelf   bool   `json:"is_self"`
	Election int64  `json:"elections"`
}

// MembershipStatus is the node tally, taken from [ClusterManager.GetStats].
//
// Total counts this node as well as its peers, which is worth stating because the issue's mockup
// printed "Peers (3 alive, ...)" above a list of two — the counts are cluster-wide and the peer list is
// not. Alive+Suspect+Dead equals Total: a node whose status matches none of the three is counted
// suspect rather than dropped, which is a property [ClusterManager.calculateClusterStats] was fixed to
// have.
type MembershipStatus struct {
	Total   int `json:"total"`
	Alive   int `json:"alive"`
	Suspect int `json:"suspect"`
	Dead    int `json:"dead"`
}

// NodeReport is one node's entry, from its [NodeInfo].
//
// The fields NodeInfo carries and this does not are the ones NodeInfo's own documentation says are not
// populated: CPUUsage, DiskUsage and NetworkBandwidth each need a platform-specific source that is not
// in this repository. Two more are dropped for reasons that are not written down there:
//
//   - Version is the string "1.0.0", hardcoded at two construction sites and never derived from the
//     build. It is not the ObjectFS version and reporting it would publish a wrong number.
//   - Operations is copied from [ClusterStats.TotalOperations], which only
//     [ClusterManager.DistributeOperation] increments — and nothing on a mount path calls that, so it is
//     zero on every node of every mounted cluster.
type NodeReport struct {
	ID      string     `json:"id"`
	Address string     `json:"address"`
	Status  NodeStatus `json:"status"`

	// LastSeen is when this node last heard from that node, on the local clock. For the local node it is
	// stamped every health-check tick, so it is always recent.
	LastSeen time.Time `json:"last_seen"`

	// Cache is nil when the node reports no cache figures at all, which is what a node with no cache
	// injected looks like: [ClusterManager.refreshLocalStats] deliberately leaves those fields untouched
	// rather than zeroing them, because zero is a meaningful cache size and "there is no cache" is not
	// an empty one.
	Cache *NodeCacheReport `json:"cache,omitempty"`

	// MemoryUsage is the Go heap in use over the heap obtained from the OS — this process's own
	// footprint, not the host's. Nil when the node has not reported one, which is a peer that has been
	// discovered but has not yet completed a gossip round.
	MemoryUsage *float64 `json:"memory_usage,omitempty"`
}

// NodeCacheReport is one node's cache figures.
type NodeCacheReport struct {
	Size int64 `json:"size"`

	// Capacity is nil when the cache does not report one. The Redis-backed cache is the real case: it
	// has no capacity of its own to report, so a zero there means unknown rather than "holds nothing".
	Capacity *int64 `json:"capacity,omitempty"`

	// HitRate is nil unless Requests is greater than zero, which is the point of carrying Requests at
	// all. A cache that has served nothing has a hit rate of 0.0 by arithmetic, and an operator reading
	// "cache_hit=0%" cannot tell that from a cache that misses every time — the first is a mount that
	// has just started and the second is a serious problem.
	HitRate  *float64 `json:"hit_rate,omitempty"`
	Requests int64    `json:"requests"`
}

// ClusterCacheStatus aggregates the cache figures across nodes.
type ClusterCacheStatus struct {
	// TotalSize is the sum over alive nodes, from [ClusterStats.TotalCacheSize] rather than recomputed
	// here, so that the definition of that sum lives in one place.
	TotalSize int64 `json:"total_size"`

	// TotalCapacity sums the capacities of the alive nodes that report one, and NodesReportingCapacity
	// says how many did. Without that count the sum is unreadable: a cluster where one node of three
	// reports capacity would otherwise look like a cluster three times fuller than it is.
	TotalCapacity          int64 `json:"total_capacity"`
	NodesReportingCapacity int   `json:"nodes_reporting_capacity"`

	// HitRate is the mean over the alive nodes that have served at least one request, and
	// NodesMeasured is how many that was.
	//
	// Restricted to measuring nodes deliberately, and this is where it differs from
	// [ClusterStats.CacheHitRate], which averages over every alive node: a node that has served nothing
	// contributes a 0.0 to that mean and drags the cluster figure toward zero for no reason other than
	// having just started. Nil when no node has measured anything, rather than 0.
	HitRate       *float64 `json:"hit_rate,omitempty"`
	NodesMeasured int      `json:"nodes_measured"`

	// AliveNodes is the denominator the two counts above are out of.
	AliveNodes int `json:"alive_nodes"`
}

// GossipCounters is the subset of [GossipStats] that is actually incremented.
//
// AvgMessageLatency is the notable omission: [GossipProtocol.calculateStats] leaves it at zero on
// purpose, because what it used to hold was idle time rather than latency.
//
// The four rejection counters are here rather than summarized because each answers a different operator
// question, and they are the counters that diagnose a cluster which will not form: Unauthenticated
// means a peer has the wrong secret or something that is not a member is talking to the port, Replayed
// means captured datagrams are being resent or a clock is off by more than the freshness window, and
// WrongVersion means version skew during a rolling upgrade.
type GossipCounters struct {
	MessagesSent     int64 `json:"messages_sent"`
	MessagesReceived int64 `json:"messages_received"`
	BytesSent        int64 `json:"bytes_sent"`
	BytesReceived    int64 `json:"bytes_received"`

	MessagesRejected        int64 `json:"messages_rejected"`
	MessagesUnauthenticated int64 `json:"messages_unauthenticated"`
	MessagesReplayed        int64 `json:"messages_replayed"`
	MessagesWrongVersion    int64 `json:"messages_wrong_version"`

	// MessagesTruncated and MessagesOversize are the two halves of a datagram that did not fit:
	// truncated is one that arrived clipped by the receive buffer, oversize is one this node refused to
	// send. #277 was a truncation that reported itself as a wrong cluster secret, because a clipped
	// datagram fails the authentication envelope parse.
	MessagesTruncated int64 `json:"messages_truncated"`
	MessagesOversize  int64 `json:"messages_oversize"`

	// SuspicionRefutations counts the times this node was accused of being suspect or dead and raised
	// its incarnation to say otherwise (#272). Repeated refutations mean the node is alive and being
	// falsely accused, which points at packet loss or a heartbeat interval below the network's latency.
	SuspicionRefutations int64 `json:"suspicion_refutations"`

	NodesDiscovered int64 `json:"nodes_discovered"`
	SuspicionEvents int64 `json:"suspicion_events"`
	DeathEvents     int64 `json:"death_events"`

	// NetworkErrors is a send or receive that failed at the socket, from [GossipStats] — not from
	// [ClusterStats], whose identically-named field is assigned nowhere in the repository and is one of
	// the eight this type exists to leave out. Two fields with the same name where one is measured and
	// one is not is exactly the trap that makes this worth stating.
	NetworkErrors int64 `json:"network_errors"`
}

// ClusterStatusDisabled is the payload for an instance that is not running cluster coordination.
//
// It is a real 200 answer rather than a 404 or an error, because a mount with `cluster.enabled: false`
// is not broken — it is the default configuration, and the overwhelming majority of deployments. A
// client that got an error here could not distinguish it from an instance that is failing.
func ClusterStatusDisabled(reason string) *ClusterStatus {
	return &ClusterStatus{Enabled: false, Reason: reason}
}

// StatusSnapshot builds the cluster status this node can honestly report.
//
// It reads through [ClusterManager.GetStats] and [ClusterManager.GetNodes] rather than walking cm.nodes
// itself, so the tallies and the total cache size keep the single definition those accessors already
// have — the alternative is a third opinion about how many nodes there are, which is the shape of #275.
// The cost is that the two reads are not one atomic view, so a membership change landing between them
// can leave a count one node behind the list. That is acceptable for a status report and is not
// acceptable for anything that decides something, which is why nothing does.
func (cm *ClusterManager) StatusSnapshot() *ClusterStatus {
	// Recomputed rather than read from the last tick. [ClusterManager.updateStats] refreshes on a
	// five-second ticker, so without this an operator running the command twice a second gets the same
	// figures repeatedly — and, worse, the first five seconds of a mount report the membership as it was
	// at Start. It also samples this node's own cache figures, which is what makes the local node's line
	// in the report say anything at all.
	//
	// The cost is one ReadMemStats and one cache Stats() per request against an endpoint an operator polls
	// by hand. That is the same work the gossip round already does at 2 Hz.
	cm.calculateClusterStats()

	stats := cm.GetStats()
	nodes := cm.GetNodes()

	cm.mu.RLock()
	consensusEnabled := cm.config.EnableConsensus
	selfID := cm.nodeID
	leader := cm.leader
	isLeader := cm.isLeader
	cm.mu.RUnlock()

	status := &ClusterStatus{
		Enabled:    true,
		NodeID:     selfID,
		GossipAddr: cm.GossipAddr(),
		Membership: MembershipStatus{
			Total:   stats.TotalNodes,
			Alive:   stats.AliveNodes,
			Suspect: stats.SuspectNodes,
			Dead:    stats.DeadNodes,
		},
		Peers: make([]NodeReport, 0, len(nodes)),
		Cache: ClusterCacheStatus{
			TotalSize:  stats.TotalCacheSize,
			AliveNodes: stats.AliveNodes,
		},
	}

	if consensusEnabled {
		status.Leadership = &LeadershipStatus{
			Leader:   leader,
			IsSelf:   isLeader,
			Election: stats.LeaderElections,
		}
	}

	var (
		hitRateSum float64
		measured   int
	)

	for id, info := range nodes {
		report := nodeReport(info)

		if id == selfID {
			status.Self = &report
		} else {
			status.Peers = append(status.Peers, report)
		}

		// Aggregated over alive nodes only, matching what TotalCacheSize sums: these figures describe
		// capacity the cluster can actually use, and a dead node's cache is not reachable.
		if info.Status != NodeStatusAlive || report.Cache == nil {
			continue
		}

		if report.Cache.Capacity != nil {
			status.Cache.TotalCapacity += *report.Cache.Capacity
			status.Cache.NodesReportingCapacity++
		}

		if report.Cache.HitRate != nil {
			hitRateSum += *report.Cache.HitRate
			measured++
		}
	}

	status.Cache.NodesMeasured = measured
	if measured > 0 {
		mean := hitRateSum / float64(measured)
		status.Cache.HitRate = &mean
	}

	// Sorted by ID so that repeated invocations produce the same output. Map iteration order is
	// randomized, and a status command whose peer list reshuffles between runs cannot be diffed.
	sort.Slice(status.Peers, func(i, j int) bool { return status.Peers[i].ID < status.Peers[j].ID })

	if gossip := cm.gossipProtocol(); gossip != nil {
		status.Gossip = gossipCounters(gossip.GetStats())
	}

	if coordinator := cm.getCoordinator(); coordinator != nil {
		status.AnnouncedKeys = coordinator.announcedKeys()
	}

	return status
}

// gossipProtocol returns the gossip protocol, which may be nil for a manager that did not come from
// [NewClusterManager].
func (cm *ClusterManager) gossipProtocol() *GossipProtocol {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	return cm.gossip
}

// nodeReport converts a NodeInfo into the subset of it that is worth reporting.
func nodeReport(info *NodeInfo) NodeReport {
	report := NodeReport{
		ID:       info.ID,
		Address:  info.Address,
		Status:   info.Status,
		LastSeen: info.LastSeen,
	}

	if info.MemoryUsage > 0 {
		usage := info.MemoryUsage
		report.MemoryUsage = &usage
	}

	// A node that reports none of the three is one with no cache injected — refreshLocalStats leaves
	// all of them untouched in that case — or a peer discovered before its first gossip round. Either
	// way there is nothing to say about its cache, and saying "0 bytes, 0%" instead would describe a
	// cache that exists and is empty.
	if info.CacheSize == 0 && info.CacheCapacity == 0 && info.CacheRequests == 0 {
		return report
	}

	cache := &NodeCacheReport{
		Size:     info.CacheSize,
		Requests: info.CacheRequests,
	}

	if info.CacheCapacity > 0 {
		capacity := info.CacheCapacity
		cache.Capacity = &capacity
	}

	if info.CacheRequests > 0 {
		rate := info.CacheHitRate
		cache.HitRate = &rate
	}

	report.Cache = cache

	return report
}

// gossipCounters copies the counters that are incremented out of the statistics struct.
func gossipCounters(stats *GossipStats) GossipCounters {
	return GossipCounters{
		MessagesSent:            stats.MessagesSent,
		MessagesReceived:        stats.MessagesReceived,
		BytesSent:               stats.BytesSent,
		BytesReceived:           stats.BytesReceived,
		MessagesRejected:        stats.MessagesRejected,
		MessagesUnauthenticated: stats.MessagesUnauthenticated,
		MessagesReplayed:        stats.MessagesReplayed,
		MessagesWrongVersion:    stats.MessagesWrongVersion,
		MessagesTruncated:       stats.MessagesTruncated,
		MessagesOversize:        stats.MessagesOversize,
		SuspicionRefutations:    stats.SuspicionRefutations,
		NodesDiscovered:         stats.NodesDiscovered,
		SuspicionEvents:         stats.SuspicionEvents,
		DeathEvents:             stats.DeathEvents,
		NetworkErrors:           stats.NetworkErrors,
	}
}
