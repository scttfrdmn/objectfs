/*
Package distributed provides cluster coordination and distributed operations for ObjectFS.

# Overview

The distributed package implements a distributed system layer that enables ObjectFS to run
in a clustered configuration with data replication, load balancing, and consensus. It provides
the coordination mechanisms needed for multi-node deployments.

⚠️ WARNING: experimental, and narrower than this document's structure implies. Not for production.

Read [#Consistency Levels] before relying on anything here. The short version is that the consensus
engine elects leaders and replicates nothing — applyLogEntry has three empty arms and no caller
appends an operation entry — and that the coordinator's data path calls the backend directly without
consulting the log, the commit index, the term, or leadership. Whether that gets built out as Raft or
replaced by per-key conditional writes is the open question in
docs/design/conditional-writes-vs-raft.md (#169).

Architecture

	┌──────────────────────────────────────────────────┐
	│           Coordinator (Operation Manager)         │  - Executes distributed operations                │  - Enforces consistency levels                    │  - Manages operation lifecycle                    │
	└────────┬──────────────────┬──────────────────────┘
	         │
	    ┌────▼─────┐      ┌─────▼──────┐
	    │  Cluster │    Load    │  Manager │  Balancer  │ - Nodes  │ - Routing  │ - Health │ - Strategy │
	    └────┬─────┘      └─────┬──────┘
	         │
	    ┌────▼──────────────────▼────┐
	    │    Gossip Protocol          │  - Node discovery           │  - State propagation        │  - Failure detection        │
	    └────────────────────────────┘

# Core Components

1. ClusterManager: Manages cluster membership, node health, and leader election
2. Coordinator: Executes distributed operations with configurable consistency
3. GossipProtocol: Handles node-to-node communication and state synchronization
4. ConsensusEngine: Implements consensus for critical cluster decisions

# Consistency Levels

Three levels are accepted, and this section says what each one does rather than what it is named
after, because the names promise guarantees the code does not provide.

The reason they cannot provide them is structural, not a missing feature: every node in a cluster
writes the same key in the same bucket. S3 is the single copy. So "replicate to the other nodes"
means issuing the same PUT again from another node, and a majority of nodes reporting success means
a majority could reach S3 — it says nothing about ordering, isolation, or what a subsequent read
returns. The levels therefore differ in how many redundant identical requests are issued and whether
the caller waits for them.

Eventual (default) — one node executes; the others re-issue the same operation in the background.

	op := &distributed.DistributedOperation{
		Type:        distributed.OpTypePut,
		Key:         "cache/data.bin",
		Data:        data,
		Consistency: distributed.ConsistencyEventual,
	}
	result, err := coordinator.ExecuteOperation(ctx, op)

Session — the first target node executes, then the rest re-issue in the background. Identical to
Eventual except for preferring targetNodes[0], and it does not provide read-your-writes: the
guarantee that name refers to comes from reading through the write buffer, which internal/vfs does
per descriptor and this package is not involved in.

	op := &distributed.DistributedOperation{
		Type:        distributed.OpTypeGet,
		Key:         "session/user123",
		Consistency: distributed.ConsistencySession,
	}

Strong — every target node executes concurrently and the call succeeds once a majority reports
success. This is N identical PUTs of the same bytes to one key, so it is a reachability signal and
not linearizability, which this document claimed until v0.11.0. What it buys over Eventual is that
the caller learns of a failure synchronously; what it costs is N times the requests. A genuinely
linearizable single-key write is available from S3 itself, via a conditional write — see
docs/design/conditional-writes-vs-raft.md (#169).

	op := &distributed.DistributedOperation{
		Type:        distributed.OpTypePut,
		Key:         "config/settings.yaml",
		Data:        configData,
		Consistency: distributed.ConsistencyStrong,
	}

# Gossip Authentication

Every node in a cluster must hold the same cluster secret, and a cluster will not start without one.
This is deliberate: the gossip protocol runs over UDP, and before authentication existed any host
that could reach the port could add itself to the cluster and announce that it held the current copy
of a cached object. Running unauthenticated is the failure nobody notices, so it is refused at
construction rather than warned about.

Generate a secret once per cluster and distribute it the way the rest of the node configuration is
distributed:

	openssl rand -hex 32 > /etc/objectfs/cluster.secret
	chmod 600 /etc/objectfs/cluster.secret

Then name it in the cluster configuration, or set OBJECTFS_CLUSTER_SECRET, which takes precedence
and is what a container orchestrator injects. The secret is never a field in the YAML configuration:
packaging installs that file world-readable, so a secret in it would be published to every user on
the node. A secret file readable by anyone but its owner is refused for the same reason.

Each message carries an HMAC-SHA256 over its exact bytes, plus a timestamp and message ID checked
against a 30-second freshness window, so a captured datagram cannot be replayed later to reassert
state that has since been replaced. Rejections are counted in [GossipStats] — separately for a bad
MAC, a replay, and an envelope version this build does not understand, because those three send an
operator to three different places. A cluster of one with a rising MessagesUnauthenticated count is
a wrong secret; the same cluster with a rising MessagesWrongVersion is a half-finished upgrade.

What this does not do: gossip payloads are not encrypted (they are membership metadata and cache
keys), and because every member holds the same key, a compromised node can impersonate any other.
Defending against that needs per-node keys and is a different threat model.

# Setting Up a Cluster

Basic cluster configuration:

	config := &distributed.ClusterConfig{
		NodeID:        "node-1",
		ListenAddr:    "0.0.0.0:8080",
		AdvertiseAddr: "192.168.1.10:8080",
		SeedNodes: []string{
			"192.168.1.11:8080",
			"192.168.1.12:8080",
		},
		ReplicationFactor: 3,
		ConsistencyLevel:  "eventual",

		// Required. The same file, with the same contents, on every node.
		SecretFile: "/etc/objectfs/cluster.secret",
	}

	// Create cluster manager. This returns an error rather than a cluster if no secret
	// is configured, so a missing deployment step is caught at startup.
	cluster, err := distributed.NewClusterManager(config)
	if err != nil {
		log.Fatal(err)
	}

	// Start cluster
	if err := cluster.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer cluster.Stop()

	// Create coordinator. The third argument is the types.Backend it executes
	// operations against; nil leaves it unset, and operations then fail rather
	// than silently succeeding.
	coordinator, err := distributed.NewCoordinator(cluster, config, backend)
	if err != nil {
		log.Fatal(err)
	}

	if err := coordinator.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer coordinator.Stop()

# Distributed Operations

Execute operations across the cluster:

	// Write operation
	writeOp := &distributed.DistributedOperation{
		ID:          "write-123",
		Type:        distributed.OpTypePut,
		Key:         "objects/file.dat",
		Data:        fileData,
		Consistency: distributed.ConsistencyStrong,
		Timeout:     30 * time.Second,
		Retries:     3,
	}
	result, err := coordinator.ExecuteOperation(ctx, writeOp)
	if err != nil {
		log.Printf("Write failed: %v", err)
	}

	// Read operation
	readOp := &distributed.DistributedOperation{
		Type:        distributed.OpTypeGet,
		Key:         "objects/file.dat",
		Consistency: distributed.ConsistencyEventual,
	}
	result, err = coordinator.ExecuteOperation(ctx, readOp)
	if result.Success {
		data := result.Data
		// Use data
	}

# Load Balancing Strategies

The LoadBalancer supports multiple strategies for distributing operations:

Round Robin (StrategyRoundRobin):
- Simple, fair distribution
- No state required
- Good for uniform workloads

Least Load (StrategyLeastLoad):
- Selects nodes with lowest current load
- Balances uneven workloads
- Default strategy

Consistent Hash (StrategyConsistentHash):
- Maps a key to a node by rendezvous (highest-random-weight) hashing
- The same key reaches the same node for as long as that node is alive
- Removing a node moves only the keys it owned, about 1/n, not the whole mapping
- Selection is independent of the order the node set is iterated in. Until v0.11.0 it was not:
the implementation was a slice prefix over a slice built by ranging a map, so the same key
reached a different node on each call, and a per-node cache could not hit (#131)
- Requires an operation key. A keyless operation — a list with no elected leader, or a batch —
falls back to round-robin, since hashing the empty key would send all of them to one node
- See internal/distributed/hashring for the scheme, its bounds, and the lookup benchmark

Latency Based (StrategyLatencyBased):
- Routes to fastest responding nodes
- Adapts to network conditions
- Requires latency tracking

# Cache Replication

The CacheReplicator handles asynchronous cache synchronization:

	// Replication happens automatically for write operations
	// with replication factor > 1

	// Monitor replication status
	stats := coordinator.GetStats()
	replicationStats := stats["replication"].(distributed.ReplicationStats)
	log.Printf("Replicated: %d bytes, Active: %d tasks",
		replicationStats.BytesReplicated,
		replicationStats.ActiveTasks)

# Cluster Health Monitoring

Check cluster health and node status:

	// Get cluster state
	nodes := cluster.GetNodes()
	for nodeID, node := range nodes {
		log.Printf("Node %s: Status=%s, Load=%.2f",
			nodeID, node.Status, node.Load)
	}

	// Get leader
	:= cluster.GetLeader()
	log.Printf("Current leader: %s", leader)

	// Check if this node is leader
	if cluster.IsLeader() {
		// Perform leader-only operations
	}

Failure Detection & Recovery

The gossip protocol implements:

1. Heartbeat-based failure detection
2. Automatic leader re-election
3. Node state propagation
4. Split-brain prevention (via quorum)

Failure Detection:

	// Nodes automatically detect failures via gossip protocol
	// Failed nodes are marked as NodeStatusSuspect or NodeStatusDead

	// Subscribe to node status changes
	// (Implementation-specific, depends on cluster manager)

Leader Election:

	// Automatic leader election using Raft-inspired consensus
	// Leader handles:
	// - Cluster-wide operations
	// - Configuration changes
	// - Quorum decisions

# Configuration

ClusterConfig controls all distributed system behavior:

	type ClusterConfig struct {
		Enabled           bool              // Enable clustering
		NodeID            string            // Unique node identifier
		ListenAddr        string            // Bind address
		AdvertiseAddr     string            // Advertised address
		SeedNodes         []string          // Bootstrap nodes
		ReplicationFactor int               // Data replication count
		ConsistencyLevel  string            // Default consistency
		GossipInterval    time.Duration     // Gossip frequency
		FailureTimeout    time.Duration     // Failure detection timeout
		ElectionTimeout   time.Duration     // Leader election timeout
		OperationTimeout  time.Duration     // Default op timeout
		RetryAttempts     int               // Default retry count
	}

# Best Practices

1. Consistency Trade-offs
Choose the appropriate consistency level for each operation. Don't use strong
consistency for operations that don't require it.

2. Replication Factor
Set replication factor based on:
- Data criticality (higher for important data)
- Cluster size (typically 3 for small clusters)
- Read/write ratio (higher for read-heavy workloads)

3. Network Partitions
Plan for network partitions:
- Use quorum-based operations
- Implement proper timeout handling
- Design for eventual consistency where possible

4. Monitoring
Monitor cluster health metrics:
- Node availability
- Replication lag
- Operation latency
- Load imbalance

5. Testing
Test failure scenarios:
- Single node failure
- Network partition
- Leader failure
- Cascading failures

Known Issues & Limitations

⚠️ Race Conditions: The distributed package currently has race conditions that cause
test timeouts. These are being addressed in Sprint 4 with comprehensive integration
tests using LocalStack.

⚠️ Incomplete Features:
- Consensus engine is partially implemented
- Gossip protocol needs additional testing
- Split-brain protection needs validation

⚠️ Performance: Not yet optimized for high-throughput environments. Benchmarking
and optimization planned for post-v0.2.0.

# Future Enhancements

Planned for future releases:
- SWIM-based gossip protocol
- Multi-Raft for better scalability
- Cross-datacenter replication
- Dynamic sharding
- Consistent snapshots

Example: Complete Cluster Setup

	package main

	import (
		"context"
		"log"
		"time"

		"github.com/scttfrdmn/objectfs/internal/distributed"
	)

	func main() {
		config := &distributed.ClusterConfig{
			NodeID:            "node-primary",
			ListenAddr:        "0.0.0.0:8080",
			AdvertiseAddr:     "192.168.1.10:8080",
			SeedNodes:         []string{"192.168.1.11:8080"},
			ReplicationFactor: 3,
			ConsistencyLevel:  "eventual",
			GossipInterval:    time.Second,
			HeartbeatInterval: time.Second,
			SecretFile:        "/etc/objectfs/cluster.secret",
		}

		// Initialize cluster
		cluster, err := distributed.NewClusterManager(config)
		if err != nil {
			log.Fatal(err)
		}

		ctx := context.Background()
		if err := cluster.Start(ctx); err != nil {
			log.Fatal(err)
		}
		defer cluster.Stop()

		// Initialize coordinator
		coordinator, err := distributed.NewCoordinator(cluster, config, backend)
		if err != nil {
			log.Fatal(err)
		}

		if err := coordinator.Start(ctx); err != nil {
			log.Fatal(err)
		}
		defer coordinator.Stop()

		// Execute distributed operation
		op := &distributed.DistributedOperation{
			Type:        distributed.OpTypePut,
			Key:         "test-key",
			Data:        []byte("test-value"),
			Consistency: distributed.ConsistencyStrong,
		}

		result, err := coordinator.ExecuteOperation(ctx, op)
		if err != nil {
			log.Fatal(err)
		}

		if result.Success {
			log.Printf("Operation succeeded on %d nodes",
				len(result.NodeResults))
		}

# See Also

- internal/health: Health monitoring for cluster nodes
- internal/metrics: Metrics collection for distributed operations
- internal/circuit: Circuit breakers for fault tolerance

For distributed systems theory and best practices, see:
- https://en.wikipedia.org/wiki/CAP_theorem
- https://martin.kleppmann.com/2015/05/11/please-stop-calling-databases-cp-or-ap.html
*/
package distributed
