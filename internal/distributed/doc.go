/*
Package distributed provides cluster coordination and distributed operations for ObjectFS.

# Overview

The distributed package implements a distributed system layer that enables ObjectFS to run
in a clustered configuration with data replication, load balancing, and consensus. It provides
the coordination mechanisms needed for multi-node deployments.

⚠️ WARNING: experimental, and narrower than this document's structure implies. Not for production.

Read [#Conditional Writes] before relying on anything here. The short version is that the consensus
engine elects leaders and replicates nothing — applyLogEntry has no state machine to apply anything
to — and that the coordinator's data path calls the backend directly without consulting the log, the
commit index, the term, or leadership.

That is not going to be built out. As of 2026-08-03 the direction is per-key S3 compare-and-swap:
docs/design/conditional-writes-vs-raft.md (#169) was adopted, on the grounds that Raft replicates a
log so N nodes can agree on state they each hold a copy of, and these nodes hold no such state — the
bucket does. The Raft issues are closed (#128, #130, #133, #151) and the replacement is filed as #282
(Backend.PutObjectIf), #283 (a lease over it), #284 (removing the fan-out and the consistency
taxonomy) and #285 (verifying non-AWS backends).

So what is below describes code that exists and has shrunk, not a design being completed.
Gossip-based membership and leader election stay.

Architecture

	┌──────────────────────────────────────────────────┐
	│           Coordinator (Operation Manager)         │  - Executes distributed operations                │  - Evaluates preconditions at the store           │  - Manages operation lifecycle                    │
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
2. Coordinator: Executes one operation on one node, conditionally when asked
3. GossipProtocol: Handles node-to-node communication and state synchronization
4. ConsensusEngine: Elects a leader; see the warning above for what it does not do

# Conditional Writes

An operation is executed once, by one node. When it is a put and carries a
[DistributedOperation.Precondition], the store evaluates that precondition and refuses the write if
it does not hold — so the exclusion is done by S3, on the one request, and not by counting nodes.

	// Take a key nobody holds. Two callers racing this: exactly one succeeds.
	created, err := coordinator.ExecuteOperation(ctx, &distributed.DistributedOperation{
		Type:         distributed.OpTypePut,
		Key:          "cluster/owner",
		Data:         []byte(nodeID),
		Precondition: types.Precondition{Absent: true},
	})
	if errors.Is(err, types.ErrPreconditionFailed) {
		// Somebody else holds it. Re-read and decide; do not retry blindly.
	}

	// Replace it only if it still holds what we read. created.ETag is the version to assert next.
	_, err = coordinator.ExecuteOperation(ctx, &distributed.DistributedOperation{
		Type:         distributed.OpTypePut,
		Key:          "cluster/owner",
		Data:         []byte(successor),
		Precondition: types.Precondition{ETag: created.ETag},
	})

A refused write reports [ConditionalLost] in [OperationResult.Conditional] and wraps
[types.ErrPreconditionFailed] in the error, and nothing was written. It is not retried internally and
must not be: the answer is definitive, and the recovery is to re-read, recompute and CAS again, which
only the caller can do because only it knows what the new bytes should be. A precondition on anything
but a put is refused with [types.ErrInvalidPrecondition] rather than dropped.

An endpoint that does not evaluate preconditions reports [ConditionalUnsupported] and writes nothing.
A caller that needs mutual exclusion must stop there — falling back to an unconditional write turns
the one guarantee this package offers into a last-writer-wins overwrite. #285 tracks which non-AWS
backends evaluate them.

Whether the operation was conditional or not, a failure is reported both ways:
[Coordinator.ExecuteOperation] returns a non-nil error and an [OperationResult] with Success false,
and the error carries the same text as OperationResult.Error. Checking either is sufficient. Until
v0.11.0 it was not — the executors reported failure only in the result and returned a nil error, so a
caller checking err alone saw a success, which is what [ClusterManager.DistributeOperation] did when
incrementing its own counters (#269).

There was a "Consistency Levels" section here, documenting `ConsistencyEventual`, `ConsistencySession`
and `ConsistencyStrong` with an example each. #284 deleted the type and this section with it. All three
issued the same unconditional PutObject and differed only in how many nodes issued it and whether the
caller waited, so what an operator picked changed the request count and not the guarantee — Strong in
particular was N identical PUTs of the same bytes to one key, which this document called
"linearizable" until v0.11.0 and which is a reachability signal. The reason no level could provide
what it named is structural rather than a missing feature: every node writes the same key in the same
bucket, and S3 holds the single copy, so "replicate to the other nodes" means issuing the same PUT
again from somewhere else. What replaced the taxonomy is per-operation instead of per-mount, and is
evaluated by the store rather than voted on.

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

	// Write operation. No Precondition, so this is an unconditional put: last writer wins,
	// which is what an uncoordinated write to S3 has always been. Set Precondition when the
	// write is only correct if something about the key still holds -- see [#Conditional Writes].
	writeOp := &distributed.DistributedOperation{
		ID:      "write-123",
		Type:    distributed.OpTypePut,
		Key:     "objects/file.dat",
		Data:    fileData,
		Timeout: 30 * time.Second,
		Retries: 3,
	}
	result, err := coordinator.ExecuteOperation(ctx, writeOp)
	if err != nil {
		log.Printf("Write failed: %v", err)
	}

	// Read operation
	readOp := &distributed.DistributedOperation{
		Type: distributed.OpTypeGet,
		Key:  "objects/file.dat",
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

There is none, and this section documented one. #284 deleted the CacheReplicator, so
[Coordinator.GetStats] no longer has a "replication" key and the type assertion shown here would
panic; ReplicationStats and its BytesReplicated counter are gone with it.

What that subsystem did was send N-1 peers a put of an object's own bytes back to the same key in the
same bucket, which is a billed request that changes nothing. Worse, when gossip was not running it
sent nothing at all and added the byte count to BytesReplicated anyway, so the throughput it reported
was of work that had not happened. Warming a peer's *local* cache is a different thing and is real
work; it is filed as #141.

# Cache Coordination

What nodes do share is *where bytes are cached*, not the bytes themselves. Three methods on
[github.com/scttfrdmn/objectfs/pkg/types.DistributedCoordinator] cover it, and only one of them is
implemented:

	coord := cluster.GetCoordinator()

	// Implemented. Tells peers to evict a key, naming the version that replaced what they hold.
	if err := coord.InvalidateKey(ctx, key, res.ETag); err != nil {
		// Peers may still be serving the version this write replaced. Not fatal to the write, which
		// already succeeded at the store — but worth logging, because it is a staleness window.
		slog.Warn("could not invalidate peers", "key", key, "error", err)
	}

	// Not implemented: both return types.ErrNotSupported until #140.
	_ = coord.AnnounceKey(ctx, ann)
	_, _ = coord.QueryKeyOwnership(ctx, key)

The two unimplemented ones return an error rather than a nil or an empty slice, and that is deliberate
rather than lazy. An empty slice with a nil error is the *correct* answer for a key no peer has cached,
so returning it from a method that cannot query would be indistinguishable from a working query against
a cold cluster — a caller measuring peer-fetch hit rates would read a flat zero as "warming does not
help" instead of "warming is not built". This is the same shape as the CacheReplicator above, whose only
test asserted that its field was non-nil.

The ETag on an invalidation is load-bearing, not informational. Gossip retransmits and reorders, so a
receiver handed a bare key cannot distinguish an invalidation it has already acted on from a new one;
[GossipProtocol.markInvalidationApplied] keeps a bounded set of applied (key, ETag) pairs to make each
exactly-once. An empty ETag stays legal and means the sender could not name a version — a delete, or an
unconditional put — and is applied every time, which costs a redundant eviction and can never serve
stale bytes.

What this cannot yet do is suppress an invalidation *older* than what a peer holds. [types.Cache] stores
bytes at offsets with no version beside them, so a receiver has nothing to compare an incoming ETag
against; that needs a per-key version in the cache, which is #141's work.

# Cluster Health Monitoring

Check cluster health and node status:

	// Get cluster state
	nodes := cluster.GetNodes()
	for nodeID, node := range nodes {
		log.Printf("Node %s: Status=%s, Memory=%.2f, Cache=%d bytes",
			nodeID, node.Status, node.MemoryUsage, node.CacheSize)
	}

	// Get leader
	leader := cluster.GetLeader()
	log.Printf("Current leader: %s", leader)

	// Check if this node is leader
	if cluster.IsLeader() {
		// Perform leader-only operations
	}

Which of [NodeInfo]'s figures mean anything is worth knowing before building on them. MemoryUsage,
CacheSize, CacheHitRate, and Operations are measured once per gossip round and travel with each alive
message. CPUUsage, DiskUsage, and NetworkBandwidth are always zero: each needs a platform-specific
source that is not in this repository, and a plausible stand-in would be worse than a zero, because a
zero prompts someone to implement the field while a number that looks measured gets used as one.

None of the four carried a live value before v0.11.0. They were set at construction and never written
again, so every node advertised itself as idle with an empty cache — and the receiving side would have
discarded an update anyway, for the incarnation reason described below (#132, #272).

Failure Detection & Recovery

The gossip protocol implements:

1. Heartbeat-based failure detection
2. Automatic leader re-election
3. Node state propagation
4. Split-brain prevention (via quorum)

Failure Detection:

A node that has not been heard from for three heartbeat intervals is marked suspect, and one that
stays suspect is marked dead. Detection is a guess — a lost datagram and a lost node look identical
from the outside — so the guess has to be reversible.

That is what the incarnation number is for. Every accusation names the incarnation it was made
about, and the accused answers by publishing a higher one, which supersedes the accusation
everywhere it has spread. A node never accepts a suspect or dead report about itself; it refutes it.
Recovery therefore needs no operator action and no restart: a node that was unreachable for a while
rejoins on its next gossip round, and [GossipStats.SuspicionRefutations] counts how often that has
happened — a number that climbs steadily means a healthy cluster whose heartbeat interval is set
below the network's real latency.

Until v0.11.0 none of that worked, because nothing ever incremented an incarnation. Every
strictly-greater comparison in the protocol was permanently false after the message that discovered
a node, with two consequences: a node marked dead by one lost heartbeat was gone from routing until
its process restarted, and the node statistics riding along with each alive message were frozen at
the first value ever received, so anything reading them was reading a snapshot of startup (#272).

	// Nodes automatically detect failures via gossip protocol
	// Failed nodes are marked as NodeStatusSuspect or NodeStatusDead

	// Subscribe to node status changes
	// (Implementation-specific, depends on cluster manager)

Leader Election:

	// Automatic leader election over gossip, with terms and votes.
	// A leader is elected and is used for:
	// - Cluster-wide operations
	//
	// Configuration changes and quorum decisions were listed here and are not implemented:
	// broadcastProposal never sent a proposal (it slept and accepted its own), and #133 was
	// closed rather than fixed because proposals are a consensus concept and consensus is not
	// the direction (#169).
	//
	// Leadership is a hint, not a guard. A leader that loses its network still believes it
	// leads, so an action that matters must re-assert its own precondition at the backend
	// rather than trusting IsLeader() — that is what #283's lease is for.

A cluster of one elects itself, within one election timeout of [ClusterManager.Start] and without any
message being sent. That is worth stating because until v0.11.0 it did not: the majority comparison
lived only in the handler for an inbound vote reply, so a node with no peers — the first thing anyone
runs, and the shape a deployment has while its second node is still being provisioned — cycled
candidate → timeout → candidate for the life of the process, incrementing its term each round, and
[ClusterManager.IsLeader] never returned true (#275).

"A cluster of one" means SeedNodes names no address other than this node's own. That distinction is
load-bearing rather than pedantic: a node that is configured to join a cluster holds a membership view
of just itself for the first few hundred milliseconds, because gossip learns of a peer only from an
inbound message and the election timer does not wait for one. Reading that view as a majority of one
would have every node of a starting three-node cluster elect itself at term 1, each on its own single
vote, before any had heard of the others. So a node with seeds waits to hear from one; a node without
seeds is declaring itself the whole of a new cluster and proceeds.

Leadership is also given up, not only taken. A node that sees a higher term — in a heartbeat or in a
vote request — steps down in the consensus engine *and* through [ClusterManager.SetLeader]. Before
v0.11.0 only the promotion path told the cluster manager, so [ClusterManager.IsLeader] was effectively
write-once: a deposed leader kept reporting itself as leader for the life of the process, which is what
turned a momentary election race into a permanent split brain rather than something the next heartbeat
resolved (#275).

Statistics are also current the moment Start returns. [ClusterManager.GetStats] and
[ClusterManager.GetNodes] describe the same membership and agree from the first call; previously the
statistics were computed only by a five-second ticker, so for the first five seconds GetStats reported
zero nodes while GetNodes reported one (#275).

# Membership Maps and Locking

Both membership maps in this package — ClusterManager.nodes and GossipProtocol.memberlist — hold
pointers, and every struct behind those pointers is mutated in place by the gossip receive goroutine.
The invariant that follows is worth stating because it was violated at four separate sites (#278):

Never carry a pointer out of a membership map across the unlock. Copying the map does not help; a
map copy copies the pointers, so the copy aliases exactly the structs another goroutine is writing.
Instead, do the read inside the critical section and carry out only values — a tally, a marshaled
byte slice, an address string, or a by-value struct copy as [ClusterManager.GetNodes] and
[GossipProtocol.GetMemberlist] do.

This is not a stale-read concern. calculateClusterStats classifies a node by Status and sums CacheSize
and CacheHitRate from it in the same iteration, so a torn read mixes moments inside a single figure a
load balancer routes on; and handleSyncMessage decides whether to accept an update by comparing
incarnations, so an inconsistently-marshaled memberlist affects whether membership converges.

Regression tests for this class are race tests: they pass under `go test` and fail only under -race, so
a green run without the flag is not evidence.

# Configuration

ClusterConfig controls all distributed system behavior:

	type ClusterConfig struct {
		Enabled           bool              // Enable clustering
		NodeID            string            // Unique node identifier
		ListenAddr        string            // Bind address
		AdvertiseAddr     string            // Advertised address
		SeedNodes         []string          // Bootstrap nodes
		ReplicationFactor int               // How many nodes selectTargetNodes returns; the first executes
		GossipInterval    time.Duration     // Gossip frequency
		FailureTimeout    time.Duration     // Failure detection timeout
		ElectionTimeout   time.Duration     // Leader election timeout
		OperationTimeout  time.Duration     // Default op timeout
		RetryAttempts     int               // Default retry count
	}

# Best Practices

1. Attach a Precondition to a Write That Is Only Correct If Something Still Holds
Read-modify-write, claiming a key, and advancing a version all need one; a plain overwrite does not.
The cost is nothing -- it is the same single request -- and what it buys is that a concurrent writer
is refused rather than silently overwriting. Never retry a refused precondition without re-reading:
the answer is definitive, so retrying the same bytes either fails again or clobbers whoever won.

This item replaced "Consistency Trade-offs", which advised not using strong consistency where it was
not needed. The three levels differed in request count and not in guarantee, so the advice was sound
in shape and about a knob that selected nothing.

2. Replication Factor
This is a selection width, not a number of copies. [Coordinator.selectTargetNodes] returns this many
nodes and [Coordinator.executeOnce] uses the first; the rest are the preference order a strategy that
genuinely needed peers would consume. Raising it does not make more copies of the bytes -- S3 holds
the one copy -- so set it for routing preference, not durability.

3. Network Partitions
A partition does not divide the data, because the data is not here. A node cut off from its peers
loses membership and leader election; its reads and writes still go to S3, and a precondition is
still evaluated by S3, so mutual exclusion survives a partition that consensus would not. What to
plan for is timeouts and a stale membership view, not a split copy of the bytes.

4. Monitoring
Monitor cluster health metrics:
  - Node availability
  - Operation latency
  - Load imbalance
  - Refused conditional writes -- a rising [ConditionalLost] rate is contention, and a single
    [ConditionalUnsupported] means the endpoint does not exclude anybody and nothing was written

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
- Cross-datacenter replication
- Dynamic sharding

"Multi-Raft for better scalability" and "consistent snapshots" were listed here and have been
removed rather than reworded: both presuppose the replicated log that #169 concluded there is nothing
to put in. Coordination is per-key compare-and-swap against the bucket (#282, #283).

Example: Complete Cluster Setup

	package main

	import (
		"context"
		"errors"
		"log"
		"time"

		"github.com/scttfrdmn/objectfs/internal/distributed"
		"github.com/scttfrdmn/objectfs/pkg/types"
	)

	func main() {
		config := &distributed.ClusterConfig{
			NodeID:            "node-primary",
			ListenAddr:        "0.0.0.0:8080",
			AdvertiseAddr:     "192.168.1.10:8080",
			SeedNodes:         []string{"192.168.1.11:8080"},
			ReplicationFactor: 3,
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

		// Execute a distributed operation. Conditional on the key being absent, so if another
		// node in this cluster runs the same code, exactly one of them creates it and the rest
		// get types.ErrPreconditionFailed. Drop Precondition for a plain last-writer-wins put.
		op := &distributed.DistributedOperation{
			Type:         distributed.OpTypePut,
			Key:          "test-key",
			Data:         []byte("test-value"),
			Precondition: types.Precondition{Absent: true},
		}

		result, err := coordinator.ExecuteOperation(ctx, op)
		switch {
		case errors.Is(err, types.ErrPreconditionFailed):
			log.Printf("another node created test-key first")
		case err != nil:
			log.Fatal(err)
		default:
			// The ETag of what was just stored, which is the version a follow-on
			// compare-and-swap asserts.
			log.Printf("created test-key, etag %s", result.ETag)
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
