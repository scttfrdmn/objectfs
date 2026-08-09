package distributed

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// GossipProtocol implements a gossip-based cluster membership protocol
type GossipProtocol struct {
	mu         sync.RWMutex
	cluster    *ClusterManager
	config     *ClusterConfig
	localNode  *NodeInfo
	memberlist map[string]*GossipNode
	conn       *net.UDPConn
	stats      *GossipStats
	stopCh     chan struct{}

	// auth signs outgoing datagrams and verifies incoming ones (#206). Never nil: a gossip
	// protocol cannot be constructed without a cluster secret, because an unauthenticated gossip
	// port lets any host on the network join the cluster and announce ownership of cached
	// objects. See auth.go.
	auth *messageAuthenticator

	// appliedInvalidations is the set of (key, ETag) cache invalidations already applied, guarded by mu
	// and bounded by maxInvalidationLedger. See [GossipProtocol.markInvalidationApplied]. Lazily
	// created, because a cluster with no cache injected never touches it.
	//
	// A set rather than a map of key to latest ETag, which is what this was first written as and which
	// was wrong in a way only a test caught: holding just the latest version suppresses a
	// retransmission of it but *applies* a replay of the version before it, since an older ETag differs
	// from the stored one and ETags carry no order to compare. Remembering every version applied is
	// what makes both cases idempotent.
	appliedInvalidations map[string]struct{}
}

// maxInvalidationLedger bounds [GossipProtocol.appliedInvalidations].
//
// 4096 (key, ETag) entries at roughly a key string plus a 34-byte quoted ETag is a few hundred
// kilobytes, which is nothing beside the cache the ledger protects, and it is large enough that the
// duplicates gossip actually produces — retransmissions of the same write, seconds apart — are still
// remembered. The failure mode when it overflows is a redundant eviction, not a stale read.
//
// Note that a key written repeatedly consumes one entry per version, since each is a distinct
// invalidation; the bound is on invalidations remembered, not on keys.
const maxInvalidationLedger = 4096

// GossipNode represents a node in the gossip protocol
type GossipNode struct {
	Info        *NodeInfo   `json:"info"`
	Incarnation uint32      `json:"incarnation"`
	State       GossipState `json:"state"`
	StateChange time.Time   `json:"state_change"`
	Suspicion   *Suspicion  `json:"suspicion,omitempty"`
}

// GossipState represents the state of a node in gossip protocol
type GossipState int

const (
	StateAlive GossipState = iota
	StateSuspect
	StateDead
	StateLeft
)

// String returns the state's name, so that a log line reports "dead" rather than the integer 2.
func (s GossipState) String() string {
	switch s {
	case StateAlive:
		return "alive"
	case StateSuspect:
		return "suspect"
	case StateDead:
		return "dead"
	case StateLeft:
		return "left"
	default:
		return fmt.Sprintf("GossipState(%d)", int(s))
	}
}

// Suspicion tracks suspicion about a node's liveness
type Suspicion struct {
	Incarnation uint32    `json:"incarnation"`
	From        []string  `json:"from"`
	Timeout     time.Time `json:"timeout"`
}

// GossipMessage represents a gossip protocol message
type GossipMessage struct {
	Type      MessageType     `json:"type"`
	From      string          `json:"from"`
	Data      json.RawMessage `json:"data"`
	Timestamp time.Time       `json:"timestamp"`
	MessageID string          `json:"message_id"`
}

// MessageType represents the type of gossip message
type MessageType string

const (
	MessageTypeJoin            MessageType = "join"
	MessageTypeLeave           MessageType = "leave"
	MessageTypeAlive           MessageType = "alive"
	MessageTypeSuspect         MessageType = "suspect"
	MessageTypeDead            MessageType = "dead"
	MessageTypeSync            MessageType = "sync"
	MessageTypeGossipHeartbeat MessageType = "gossip_heartbeat"

	// Consensus and coordinator messages piggyback on the gossip UDP socket.
	MessageTypeRequestVote       MessageType = "request_vote"
	MessageTypeRequestVoteResp   MessageType = "request_vote_resp"
	MessageTypeAppendEntries     MessageType = "append_entries"
	MessageTypeAppendEntriesResp MessageType = "append_entries_resp"
	MessageTypeNodeOperation     MessageType = "node_operation"
	MessageTypeNodeOperationResp MessageType = "node_operation_resp"

	// MessageTypeCacheInvalidate requests that peers evict a key from their
	// local cache.  The payload is a CacheInvalidateMessage.
	MessageTypeCacheInvalidate MessageType = "cache_invalidate"
)

// ErrMessageOversize means a sealed datagram exceeded MaxGossipPacket and was refused before it
// reached the socket. See [GossipProtocol.sendMessage] for why that is refused rather than truncated.
//
// It is a sentinel because one caller can act on it. Gossip is a UDP transport, so there is a hard
// ceiling on what a message can carry — with the default 8192-byte limit, a payload of object bytes
// tops out at 5802 after the base64, envelope and MAC — and a sender that hits it should say so
// rather than treat it as a lost packet. Everything else on this socket is small by construction
// (membership, votes, an invalidation naming one key), so for those it is a misconfiguration and
// failing the send is the whole answer.
var ErrMessageOversize = errors.New("gossip message exceeds max_gossip_packet")

// CacheInvalidateMessage is the payload for MessageTypeCacheInvalidate.
type CacheInvalidateMessage struct {
	Key  string `json:"key"`
	From string `json:"from"`

	// ETag names the version the sender wrote, which is the version this invalidation is *about*.
	//
	// Requirement R4 of docs/design/conditional-writes-vs-raft.md §1: gossip is unordered, so without
	// a version an invalidation can be applied after a later write's invalidation has already been
	// applied, and the receiver has no way to tell that it has moved past this one. Empty means the
	// sender could not name a version — an unconditional put, or a delete — and such a message is
	// applied unconditionally, since "evict whatever you hold" is always safe.
	//
	// It comes from [OperationResult.ETag], which is why the coordinator propagates it.
	ETag string `json:"etag,omitempty"`

	// Deleted marks an invalidation caused by the key being removed rather than replaced. A receiver
	// cannot infer this from an empty ETag, because an unconditional put has one too.
	Deleted bool `json:"deleted,omitempty"`
}

// JoinMessage represents a join request
type JoinMessage struct {
	Node        *NodeInfo `json:"node"`
	Incarnation uint32    `json:"incarnation"`
}

// AliveMessage represents an alive announcement
type AliveMessage struct {
	Node        *NodeInfo `json:"node"`
	Incarnation uint32    `json:"incarnation"`
}

// SuspectMessage represents a suspicion about a node
type SuspectMessage struct {
	Node        string `json:"node"`
	Incarnation uint32 `json:"incarnation"`
	From        string `json:"from"`
}

// DeadMessage represents a death announcement
type DeadMessage struct {
	Node        string `json:"node"`
	Incarnation uint32 `json:"incarnation"`
	From        string `json:"from"`
}

// SyncMessage represents a full membership sync
type SyncMessage struct {
	Nodes map[string]*GossipNode `json:"nodes"`
}

// HeartbeatMessage represents a heartbeat
type HeartbeatMessage struct {
	Node        string    `json:"node"`
	Timestamp   time.Time `json:"timestamp"`
	Incarnation uint32    `json:"incarnation"`
}

// GossipStats tracks gossip protocol statistics
type GossipStats struct {
	mu                  sync.RWMutex
	MessagesSent        int64            `json:"messages_sent"`
	MessagesReceived    int64            `json:"messages_received"`
	MessagesByType      map[string]int64 `json:"messages_by_type"`
	BytesSent           int64            `json:"bytes_sent"`
	BytesReceived       int64            `json:"bytes_received"`
	NodesDiscovered     int64            `json:"nodes_discovered"`
	SuspicionEvents     int64            `json:"suspicion_events"`
	DeathEvents         int64            `json:"death_events"`
	NetworkErrors       int64            `json:"network_errors"`
	AvgMessageLatency   time.Duration    `json:"avg_message_latency"`
	LastMessageReceived time.Time        `json:"last_message_received"`

	// Authentication rejections (#206). These are counted separately because they answer
	// different operator questions, and a single "rejected" number cannot: a nonzero
	// MessagesUnauthenticated means a peer has the wrong secret or a host that is not a member is
	// talking to the port, MessagesReplayed means someone is re-sending captured datagrams or a
	// node's clock is off by more than the freshness window, and MessagesWrongVersion means
	// version skew during a rolling upgrade. MessagesRejected is their sum, so a monitor can alert
	// on one series.
	MessagesRejected        int64 `json:"messages_rejected"`
	MessagesUnauthenticated int64 `json:"messages_unauthenticated"`
	MessagesReplayed        int64 `json:"messages_replayed"`
	MessagesWrongVersion    int64 `json:"messages_wrong_version"`

	// SuspicionRefutations counts the times this node was reported suspect or dead and raised its
	// own incarnation to say otherwise (#272). It is an operator signal, not a health signal: a
	// node that refutes repeatedly is alive and being falsely accused, which points at packet loss
	// or a heartbeat interval set below the network's actual latency — not at the node.
	SuspicionRefutations int64 `json:"suspicion_refutations"`

	// Datagrams that did not fit MaxGossipPacket, counted separately in each direction because the
	// operator action differs: an oversize send is this node's memberlist outgrowing the limit and is
	// refused before it reaches the socket, while a truncated receive is a *peer* whose limit is
	// larger than ours, which is a configuration mismatch across the cluster.
	//
	// These exist because a truncated datagram used to present as an authentication failure. The JSON
	// was cut mid-object, the envelope parse failed, and MessagesUnauthenticated went up — whose
	// documented reading, two fields above, is "a peer with a different cluster secret." An operator
	// chasing a size problem was sent to check a secret that was correct (#277).
	MessagesTruncated int64 `json:"messages_truncated"`
	MessagesOversize  int64 `json:"messages_oversize"`
}

// NewGossipProtocol creates a new gossip protocol instance.
//
// It fails if no cluster secret is available. That is deliberate and is the whole point of #206:
// starting without authentication would mean any host that can reach the gossip port can join the
// cluster and announce cache ownership for arbitrary keys, which — once cache-warming reads fetch
// from peers — makes a peer's response into file content a reading process sees. Refusing to start
// with an error naming the missing secret is the failure an operator can fix; running unauthenticated
// is the one nobody notices.
func NewGossipProtocol(cluster *ClusterManager, config *ClusterConfig) (*GossipProtocol, error) {
	secret, err := LoadClusterSecret(config.SecretFile)
	if err != nil {
		return nil, fmt.Errorf("gossip authentication: %w", err)
	}

	gp := &GossipProtocol{
		cluster:    cluster,
		config:     config,
		memberlist: make(map[string]*GossipNode),
		stats: &GossipStats{
			MessagesByType: make(map[string]int64),
		},
		stopCh: make(chan struct{}),
		auth:   newMessageAuthenticator(secret),
	}

	// Initialize local node
	gp.localNode = &NodeInfo{
		ID:       cluster.GetNodeID(),
		Address:  config.AdvertiseAddr,
		Status:   NodeStatusAlive,
		LastSeen: time.Now(),
		Version:  "1.0.0",
		Metadata: make(map[string]string),
	}

	// Add self to member list.
	//
	// Incarnation 1 is the starting point, not a constant: refuteSuspicion raises it whenever a peer
	// accuses this node of being suspect or dead. Until v0.11.0 nothing ever raised it, which made
	// the strictly-greater guards in handleAliveMessage and handleSyncMessage permanently false for
	// every message after the one that discovered a node — so gossiped node stats froze at their
	// first observed value and a node marked dead by a transient network blip could never rejoin
	// routing without a process restart (#272).
	gp.memberlist[gp.localNode.ID] = &GossipNode{
		Info:        gp.localNode,
		Incarnation: 1,
		State:       StateAlive,
		StateChange: time.Now(),
	}

	return gp, nil
}

// Start starts the gossip protocol
func (gp *GossipProtocol) Start(ctx context.Context) error {
	// Start UDP listener
	addr, err := net.ResolveUDPAddr("udp", gp.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve listen address: %w", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to start UDP listener: %w", err)
	}

	gp.conn = conn

	slog.Info("gossip protocol listening", "addr", gp.config.ListenAddr)

	// Start background goroutines
	go gp.receiveMessages(ctx)
	go gp.gossipLoop(ctx)
	go gp.suspicionTimer(ctx)
	go gp.updateStats(ctx)

	return nil
}

// Stop stops the gossip protocol
func (gp *GossipProtocol) Stop() error {
	close(gp.stopCh)

	if gp.conn != nil {
		_ = gp.conn.Close()
	}

	slog.Info("gossip protocol stopped")
	return nil
}

// JoinNode attempts to join a node
func (gp *GossipProtocol) JoinNode(ctx context.Context, nodeAddr string) error {
	joinMsg := &JoinMessage{
		Node:        gp.localNode,
		Incarnation: gp.getCurrentIncarnation(),
	}

	msg := &GossipMessage{
		Type:      MessageTypeJoin,
		From:      gp.localNode.ID,
		Timestamp: time.Now(),
		MessageID: gp.generateMessageID(),
	}

	data, err := json.Marshal(joinMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal join message: %w", err)
	}

	msg.Data = data

	return gp.sendMessage(nodeAddr, msg)
}

// LeaveCluster announces that this node is leaving
func (gp *GossipProtocol) LeaveCluster(ctx context.Context) error {
	gp.mu.Lock()
	defer gp.mu.Unlock()

	// Update our state to leaving
	if localGossipNode, exists := gp.memberlist[gp.localNode.ID]; exists {
		localGossipNode.State = StateLeft
		localGossipNode.StateChange = time.Now()
	}

	// Broadcast leave message
	msg := &GossipMessage{
		Type:      MessageTypeLeave,
		From:      gp.localNode.ID,
		Timestamp: time.Now(),
		MessageID: gp.generateMessageID(),
	}

	data, _ := json.Marshal(map[string]string{"node": gp.localNode.ID})
	msg.Data = data

	return gp.broadcastMessage(msg)
}

// Background goroutines

func (gp *GossipProtocol) receiveMessages(ctx context.Context) {
	// One byte larger than the limit, so a datagram of exactly MaxGossipPacket is distinguishable from
	// one that exceeded it. ReadFromUDP discards the remainder of an oversize datagram and reports only
	// what it copied, so n == len(buffer) is the only evidence available that anything was lost — and
	// with a buffer sized exactly to the limit, a legitimate maximum-size message is indistinguishable
	// from a truncated larger one. The extra byte makes n > MaxGossipPacket mean truncated, full stop.
	buffer := make([]byte, gp.config.MaxGossipPacket+1)

	for {
		// Check stop conditions without blocking before each receive.
		// This ensures the goroutine exits promptly when Stop() is called,
		// even if ReadFromUDP is about to block (#102).
		select {
		case <-ctx.Done():
			return
		case <-gp.stopCh:
			return
		default:
		}

		if gp.conn == nil {
			// Connection not yet established; wait briefly before retrying
			// rather than spinning in a tight loop.
			time.Sleep(10 * time.Millisecond)
			continue
		}

		// Set a short read deadline so ReadFromUDP does not block
		// indefinitely.  On expiry we loop back to check the stop channels.
		_ = gp.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, addr, err := gp.conn.ReadFromUDP(buffer)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // deadline expired; re-check stop channels
			}
			gp.stats.mu.Lock()
			gp.stats.NetworkErrors++
			gp.stats.mu.Unlock()
			continue
		}

		// Truncation is reported as truncation, not handed to the authenticator. A datagram larger than
		// the buffer arrives with its tail discarded by the kernel, so the JSON is cut mid-object and
		// the envelope parse fails — which used to be counted and logged as an authentication failure,
		// sending the operator to check a cluster secret that was correct. The size is what is wrong and
		// the size is what the message says (#277).
		if n > gp.config.MaxGossipPacket {
			gp.stats.mu.Lock()
			gp.stats.MessagesTruncated++
			gp.stats.mu.Unlock()

			slog.Warn("dropped a truncated gossip datagram",
				"peer", addr,
				"max_gossip_packet", gp.config.MaxGossipPacket,
				"hint", "the sending node's max_gossip_packet is larger than this node's; raise it here "+
					"or lower it there, and set the same value on every node")

			continue
		}

		gp.handleIncomingMessage(ctx, buffer[:n], addr)
	}
}

// handleIncomingMessage authenticates, decodes, and dispatches one datagram.
//
// ctx is this loop's, so it is the cluster's lifetime — it descends from the context given to
// [GossipProtocol.Start]. Only one dispatch arm uses it: a NodeOperation asks this node to perform an S3
// operation on a peer's behalf, and that is the one handler here that does I/O rather than updating
// in-memory membership state. See [Coordinator.executeLocally].
func (gp *GossipProtocol) handleIncomingMessage(ctx context.Context, data []byte, addr *net.UDPAddr) {
	// Authenticate before parsing (#206). A datagram that does not verify never reaches the JSON
	// decoding of any message type, let alone a handler, so an unauthenticated host cannot join the
	// cluster or announce cache ownership.
	msg, err := gp.auth.open(data)
	if err != nil {
		// Rejections are counted and logged at warn, not dropped silently: a misconfigured secret
		// and a network problem produce the same symptom — a cluster of one — and without this the
		// operator has no way to tell them apart. The peer address is included because that is what
		// identifies which node has the wrong secret, or which host should not be talking to this
		// port at all.
		// The hint differs by reason because the three send an operator to different places, and a
		// single catch-all string would send two of them to the wrong one. A version mismatch during
		// a rolling upgrade is not a secret problem, and telling someone to check a secret that is
		// correct costs them the time it takes to rule it out.
		var hint string

		gp.stats.mu.Lock()
		gp.stats.MessagesRejected++
		switch {
		case errors.Is(err, ErrReplayed):
			gp.stats.MessagesReplayed++
			hint = "a duplicate datagram, or clock skew beyond the freshness window — check NTP on both hosts"
		case errors.Is(err, ErrUnknownAuthVersion):
			gp.stats.MessagesWrongVersion++
			hint = "a peer running a build that predates gossip authentication, or a newer envelope format"
		default:
			gp.stats.MessagesUnauthenticated++
			hint = "a peer with a different cluster secret, or a host that is not a cluster member"
		}
		gp.stats.mu.Unlock()

		slog.Warn("rejected gossip message", "error", err, "peer", addr, "hint", hint)

		return
	}

	// Update stats
	gp.stats.mu.Lock()
	gp.stats.MessagesReceived++
	gp.stats.BytesReceived += int64(len(data))
	gp.stats.MessagesByType[string(msg.Type)]++
	gp.stats.LastMessageReceived = time.Now()
	gp.stats.mu.Unlock()

	// Process message based on type
	switch msg.Type {
	case MessageTypeJoin:
		gp.handleJoinMessage(msg)
	case MessageTypeLeave:
		gp.handleLeaveMessage(msg)
	case MessageTypeAlive:
		gp.handleAliveMessage(msg)
	case MessageTypeSuspect:
		gp.handleSuspectMessage(msg)
	case MessageTypeDead:
		gp.handleDeadMessage(msg)
	case MessageTypeSync:
		gp.handleSyncMessage(msg)
	case MessageTypeGossipHeartbeat:
		gp.handleHeartbeatMessage(msg)

	// Consensus messages
	case MessageTypeRequestVote:
		if gp.cluster.consensus != nil {
			gp.cluster.consensus.handleNetworkRequestVote(msg)
		}
	case MessageTypeRequestVoteResp:
		if gp.cluster.consensus != nil {
			gp.cluster.consensus.handleNetworkRequestVoteResp(msg)
		}
	case MessageTypeAppendEntries:
		if gp.cluster.consensus != nil {
			gp.cluster.consensus.handleNetworkAppendEntries(msg)
		}
	case MessageTypeAppendEntriesResp:
		if gp.cluster.consensus != nil {
			gp.cluster.consensus.handleNetworkAppendEntriesResp(msg)
		}

	// Coordinator messages
	case MessageTypeNodeOperation:
		if gp.cluster.coordinator != nil {
			gp.cluster.coordinator.handleNetworkOperation(ctx, msg)
		}
	case MessageTypeNodeOperationResp:
		if gp.cluster.coordinator != nil {
			gp.cluster.coordinator.handleNetworkOperationResp(msg)
		}

	// Cache invalidation
	case MessageTypeCacheInvalidate:
		gp.handleCacheInvalidate(msg)
	}
}

// handleCacheInvalidate evicts a key on behalf of a peer that wrote it, discarding an invalidation
// naming a version this node has already applied.
//
// The duplicate check is the point, and it is a check against *what has been applied*, not against
// message IDs. Gossip retransmits and fans out, so the same invalidation arrives more than once by
// design; a re-delete of a key already evicted is harmless in itself, but it is indistinguishable
// from an invalidation for an *older* version arriving after a newer one — and applying that would
// throw away bytes this node has since legitimately re-cached. That is requirement R4 of
// docs/design/conditional-writes-vs-raft.md §1.
//
// What this cannot do, and does not pretend to: decide that the cached bytes are *newer* than the
// invalidation. [types.Cache] stores bytes at offsets and has no version alongside them, so this node
// cannot compare what it holds against m.ETag — only against the last ETag it was told about. So the
// rule is "apply each version once", which fixes the retransmission and the out-of-order replay of a
// version already superseded. Suppressing an invalidation for a version older than what is actually
// cached needs the cache to carry an ETag per key, which is #129's `KeyAnnouncement` plumbing and
// #141's warming work; it is not smuggled in here as a guess.
func (gp *GossipProtocol) handleCacheInvalidate(msg *GossipMessage) {
	gp.cluster.mu.RLock()
	cache := gp.cluster.cache
	gp.cluster.mu.RUnlock()

	if cache == nil {
		return
	}

	var m CacheInvalidateMessage
	if err := json.Unmarshal(msg.Data, &m); err != nil || m.Key == "" {
		return
	}

	// An invalidation that names no version is applied every time. A delete has no ETag to name, and an
	// unconditional put's sender may not have one; "evict whatever you hold" is always safe, and the
	// alternative — treating "no version" as one version and applying it once — would drop every
	// invalidation for a key after the first.
	if m.ETag != "" && !gp.markInvalidationApplied(m.Key, m.ETag) {
		return
	}

	cache.Delete(m.Key)
}

// markInvalidationApplied records that key was invalidated at etag, reporting whether this is the
// first time. It bounds its own memory: the ledger drops the oldest entries once it exceeds
// maxInvalidationLedger.
//
// Forgetting an entry means the next duplicate for it is applied — a redundant eviction, which costs a
// re-fetch and not correctness. Growing without bound would be a leak proportional to every version of
// every key the cluster has ever written, so this is the right direction to fail in.
func (gp *GossipProtocol) markInvalidationApplied(key, etag string) bool {
	// NUL rather than a printable separator: an S3 key may contain any byte except NUL, so "a\x00b" and
	// "a" + "\x00b" cannot be confused, where a colon or a slash could be.
	entry := key + "\x00" + etag

	gp.mu.Lock()
	defer gp.mu.Unlock()

	if gp.appliedInvalidations == nil {
		gp.appliedInvalidations = make(map[string]struct{})
	}
	if _, applied := gp.appliedInvalidations[entry]; applied {
		return false
	}

	if len(gp.appliedInvalidations) >= maxInvalidationLedger {
		// Drop an arbitrary tenth. Go's map iteration order is unspecified, which is what makes this a
		// fair sample rather than a preference for whatever hashes low, and there is no access time to
		// evict by: the ledger records versions, not reads.
		target := maxInvalidationLedger - maxInvalidationLedger/10
		for k := range gp.appliedInvalidations {
			delete(gp.appliedInvalidations, k)
			if len(gp.appliedInvalidations) <= target {
				break
			}
		}
	}

	gp.appliedInvalidations[entry] = struct{}{}

	return true
}

func (gp *GossipProtocol) handleJoinMessage(msg *GossipMessage) {
	var joinMsg JoinMessage
	if err := json.Unmarshal(msg.Data, &joinMsg); err != nil {
		slog.Warn("failed to unmarshal join message", "error", err)
		return
	}

	gp.mu.Lock()
	defer gp.mu.Unlock()

	nodeID := joinMsg.Node.ID

	// Add or update node in memberlist
	gp.memberlist[nodeID] = &GossipNode{
		Info:        joinMsg.Node,
		Incarnation: joinMsg.Incarnation,
		State:       StateAlive,
		StateChange: time.Now(),
	}

	// Update cluster manager
	gp.cluster.UpdateNodeInfo(nodeID, joinMsg.Node)

	slog.Info("node joined the cluster", "node_id", nodeID)

	gp.stats.mu.Lock()
	gp.stats.NodesDiscovered++
	gp.stats.mu.Unlock()

	// Send sync message back to the joining node (in a goroutine to avoid
	// holding the lock while sendSyncMessage attempts to reacquire it for reading).
	go func() { _ = gp.sendSyncMessage(joinMsg.Node.Address) }()
}

func (gp *GossipProtocol) handleLeaveMessage(msg *GossipMessage) {
	var leaveData map[string]string
	if err := json.Unmarshal(msg.Data, &leaveData); err != nil {
		slog.Warn("failed to unmarshal leave message", "error", err)
		return
	}

	nodeID := leaveData["node"]

	gp.mu.Lock()
	defer gp.mu.Unlock()

	if gossipNode, exists := gp.memberlist[nodeID]; exists {
		gossipNode.State = StateLeft
		gossipNode.StateChange = time.Now()

		// Remove from cluster manager after a delay
		go func() {
			time.Sleep(30 * time.Second)
			gp.cluster.RemoveNode(nodeID)

			gp.mu.Lock()
			delete(gp.memberlist, nodeID)
			gp.mu.Unlock()
		}()

		slog.Info("node left the cluster", "node_id", nodeID)
	}
}

func (gp *GossipProtocol) handleAliveMessage(msg *GossipMessage) {
	var aliveMsg AliveMessage
	if err := json.Unmarshal(msg.Data, &aliveMsg); err != nil {
		slog.Warn("failed to unmarshal alive message", "error", err)
		return
	}

	gp.mu.Lock()
	defer gp.mu.Unlock()

	nodeID := aliveMsg.Node.ID

	if gossipNode, exists := gp.memberlist[nodeID]; exists {
		switch {
		case aliveMsg.Incarnation > gossipNode.Incarnation:
			// A higher incarnation supersedes whatever we believed, including a death. This is the
			// only path by which a node we wrote off can rejoin routing without restarting, and it
			// is reachable because refuteSuspicion is what produces the higher number: a node we
			// accused answers with an incarnation that postdates the accusation. Before v0.11.0 no
			// incarnation ever advanced, so this arm was dead code after discovery and a node
			// removed by one lost heartbeat stayed removed (#272).
			if gossipNode.State == StateDead || gossipNode.State == StateSuspect {
				slog.Info("node refuted its own suspicion; restoring to alive",
					"node_id", nodeID, "incarnation", aliveMsg.Incarnation)
			}

			gossipNode.Incarnation = aliveMsg.Incarnation
			gossipNode.State = StateAlive
			gossipNode.StateChange = time.Now()
			gossipNode.Info = aliveMsg.Node
			gossipNode.Suspicion = nil // Clear any suspicion

			aliveMsg.Node.Status = NodeStatusAlive
			gp.cluster.UpdateNodeInfo(nodeID, aliveMsg.Node)

		case aliveMsg.Incarnation == gossipNode.Incarnation && gossipNode.State == StateAlive:
			// Refresh the payload without touching the state machine. An incarnation orders claims
			// about whether a node is alive; it says nothing about how fresh the load and cache
			// figures riding along with the claim are. A healthy node never raises its incarnation —
			// it has nothing to refute — so gating the payload on a strict increase froze every
			// node's stats at the first value ever received from it, which is how a load balancer
			// reading them came to route on numbers that were hours stale (#272, #132).
			//
			// Only for a node we already believe is alive: accepting a same-incarnation payload for
			// a suspect or dead node would silently undo the accusation without the refutation that
			// is supposed to be required to overturn it.
			gossipNode.Info = aliveMsg.Node

			aliveMsg.Node.Status = NodeStatusAlive
			gp.cluster.UpdateNodeInfo(nodeID, aliveMsg.Node)
		}
	} else {
		// New node
		gp.memberlist[nodeID] = &GossipNode{
			Info:        aliveMsg.Node,
			Incarnation: aliveMsg.Incarnation,
			State:       StateAlive,
			StateChange: time.Now(),
		}

		// Update cluster manager
		aliveMsg.Node.Status = NodeStatusAlive
		gp.cluster.UpdateNodeInfo(nodeID, aliveMsg.Node)

		gp.stats.mu.Lock()
		gp.stats.NodesDiscovered++
		gp.stats.mu.Unlock()
	}
}

// refuteSuspicion raises this node's own incarnation and re-announces it as alive, superseding a
// peer's claim that it is suspect or dead.
//
// This is what the incarnation number is for, and it is what makes the strictly-greater comparisons
// elsewhere in this file mean anything. An accusation names the incarnation it was made about, so a
// higher one is not a competing opinion — it is proof that the accusation is stale, published by the
// only node in a position to know. Until v0.11.0 nothing incremented an incarnation anywhere, so
// those comparisons were permanently false after the message that discovered a node, and a node
// accused once stayed accused for the life of the process (#272).
//
// accusedIncarnation is the incarnation the accusation was made about. The new incarnation is one
// past whichever is larger, ours or theirs — not simply ours plus one. A peer should never hold an
// incarnation for us higher than the one we published, but if it does (a peer resurrected from an old
// snapshot, or a node restarted with a lower number), refuting with self+1 would produce a number the
// accuser's guards still reject, and the two would disagree forever with each message reinforcing
// the other's position.
//
// Callers must hold gp.mu.
func (gp *GossipProtocol) refuteSuspicion(accusation string, reporter string, accusedIncarnation uint32) {
	self, exists := gp.memberlist[gp.localNode.ID]
	if !exists {
		return
	}

	self.Incarnation = max(self.Incarnation, accusedIncarnation) + 1
	self.State = StateAlive
	self.StateChange = time.Now()
	self.Suspicion = nil

	slog.Info("refuting accusation about this node",
		"accusation", accusation, "reported_by", reporter, "new_incarnation", self.Incarnation)

	gp.stats.mu.Lock()
	gp.stats.SuspicionRefutations++
	gp.stats.mu.Unlock()

	// The refutation is only useful if it reaches the cluster, and the incarnation is read here
	// under the lock we already hold rather than via getCurrentIncarnation, which would deadlock on
	// re-entry. Sending happens in a goroutine for the same reason: broadcastMessage takes gp.mu for
	// reading.
	aliveMsg := &AliveMessage{Node: gp.localNode, Incarnation: self.Incarnation}
	data, err := json.Marshal(aliveMsg)
	if err != nil {
		slog.Warn("failed to marshal refutation", "error", err)
		return
	}

	msg := &GossipMessage{
		Type:      MessageTypeAlive,
		From:      gp.localNode.ID,
		Timestamp: time.Now(),
		MessageID: gp.generateMessageID(),
		Data:      data,
	}

	go func() { _ = gp.broadcastMessage(msg) }()
}

func (gp *GossipProtocol) handleSuspectMessage(msg *GossipMessage) {
	var suspectMsg SuspectMessage
	if err := json.Unmarshal(msg.Data, &suspectMsg); err != nil {
		slog.Warn("failed to unmarshal suspect message", "error", err)
		return
	}

	gp.mu.Lock()
	defer gp.mu.Unlock()

	nodeID := suspectMsg.Node

	// An accusation about ourselves is answered, not recorded. Recording it would mark this node
	// suspect in its own memberlist, and since performGossip and broadcastMessage both skip nodes
	// that are not alive, a node could talk itself out of the cluster on one lost heartbeat (#272).
	if nodeID == gp.localNode.ID {
		if self, exists := gp.memberlist[nodeID]; exists && suspectMsg.Incarnation >= self.Incarnation {
			gp.refuteSuspicion("suspect", suspectMsg.From, suspectMsg.Incarnation)
		}
		return
	}

	if gossipNode, exists := gp.memberlist[nodeID]; exists {
		// Only process if incarnation matches and node has not already been
		// declared dead. Accept both Alive and Suspect so that additional
		// reporters can be appended to an existing Suspicion.
		if suspectMsg.Incarnation == gossipNode.Incarnation &&
			(gossipNode.State == StateAlive || gossipNode.State == StateSuspect) {
			if gossipNode.Suspicion == nil {
				gossipNode.Suspicion = &Suspicion{
					Incarnation: suspectMsg.Incarnation,
					From:        []string{suspectMsg.From},
					Timeout:     time.Now().Add(5 * time.Second),
				}
				gossipNode.State = StateSuspect
				gossipNode.StateChange = time.Now()

				slog.Info("node marked as suspect", "node_id", nodeID, "reported_by", suspectMsg.From)

				gp.stats.mu.Lock()
				gp.stats.SuspicionEvents++
				gp.stats.mu.Unlock()

				// Update cluster manager
				if gossipNode.Info != nil {
					gossipNode.Info.Status = NodeStatusSuspect
					gp.cluster.UpdateNodeInfo(nodeID, gossipNode.Info)
				}
			} else {
				// Add to suspicion list if not already there
				found := slices.Contains(gossipNode.Suspicion.From, suspectMsg.From)
				if !found {
					gossipNode.Suspicion.From = append(gossipNode.Suspicion.From, suspectMsg.From)
				}
			}
		}
	}
}

func (gp *GossipProtocol) handleDeadMessage(msg *GossipMessage) {
	var deadMsg DeadMessage
	if err := json.Unmarshal(msg.Data, &deadMsg); err != nil {
		slog.Warn("failed to unmarshal dead message", "error", err)
		return
	}

	gp.mu.Lock()
	defer gp.mu.Unlock()

	nodeID := deadMsg.Node

	// Same reasoning as handleSuspectMessage: a node that accepted a death notice about itself would
	// stop gossiping and would then genuinely be gone, having agreed with a report that was wrong
	// when it was sent (#272).
	if nodeID == gp.localNode.ID {
		if self, exists := gp.memberlist[nodeID]; exists && deadMsg.Incarnation >= self.Incarnation {
			gp.refuteSuspicion("dead", deadMsg.From, deadMsg.Incarnation)
		}
		return
	}

	if gossipNode, exists := gp.memberlist[nodeID]; exists {
		// Only process if incarnation matches or is newer
		if deadMsg.Incarnation >= gossipNode.Incarnation {
			gossipNode.State = StateDead
			gossipNode.StateChange = time.Now()
			gossipNode.Suspicion = nil

			slog.Info("node marked as dead", "node_id", nodeID, "reported_by", deadMsg.From)

			gp.stats.mu.Lock()
			gp.stats.DeathEvents++
			gp.stats.mu.Unlock()

			// Update cluster manager
			if gossipNode.Info != nil {
				gossipNode.Info.Status = NodeStatusDead
				gp.cluster.UpdateNodeInfo(nodeID, gossipNode.Info)
			}
		}
	}
}

func (gp *GossipProtocol) handleSyncMessage(msg *GossipMessage) {
	var syncMsg SyncMessage
	if err := json.Unmarshal(msg.Data, &syncMsg); err != nil {
		slog.Warn("failed to unmarshal sync message", "error", err)
		return
	}

	gp.mu.Lock()
	defer gp.mu.Unlock()

	// Merge membership information
	for nodeID, remoteNode := range syncMsg.Nodes {
		if nodeID == gp.localNode.ID {
			// Never take a peer's word for our own state — but do answer it. A full sync is often
			// the first thing a node receives after a partition heals, so it is often where a node
			// learns it was written off while it was unreachable. Refuting here is what stops that
			// entry from propagating back out through the next round of syncs (#272). The guard in
			// refuteSuspicion's callers means a stale entry that predates an earlier refutation is
			// ignored rather than refuted again, so this converges instead of ringing.
			if remoteNode.State == StateSuspect || remoteNode.State == StateDead {
				if self, ok := gp.memberlist[nodeID]; ok && remoteNode.Incarnation >= self.Incarnation {
					gp.refuteSuspicion("stale "+remoteNode.State.String()+" entry in a membership sync",
						msg.From, remoteNode.Incarnation)
				}
			}

			continue
		}

		localNode, exists := gp.memberlist[nodeID]
		if !exists {
			// New node
			gp.memberlist[nodeID] = &GossipNode{
				Info:        remoteNode.Info,
				Incarnation: remoteNode.Incarnation,
				State:       remoteNode.State,
				StateChange: remoteNode.StateChange,
				Suspicion:   remoteNode.Suspicion,
			}

			if remoteNode.Info != nil {
				gp.cluster.UpdateNodeInfo(nodeID, remoteNode.Info)
			}

			gp.stats.mu.Lock()
			gp.stats.NodesDiscovered++
			gp.stats.mu.Unlock()
		} else if remoteNode.Incarnation > localNode.Incarnation {
			// Update with newer information
			localNode.Info = remoteNode.Info
			localNode.Incarnation = remoteNode.Incarnation
			localNode.State = remoteNode.State
			localNode.StateChange = remoteNode.StateChange
			localNode.Suspicion = remoteNode.Suspicion

			if remoteNode.Info != nil {
				gp.cluster.UpdateNodeInfo(nodeID, remoteNode.Info)
			}
		}
	}
}

func (gp *GossipProtocol) handleHeartbeatMessage(msg *GossipMessage) {
	var heartbeatMsg HeartbeatMessage
	if err := json.Unmarshal(msg.Data, &heartbeatMsg); err != nil {
		slog.Warn("failed to unmarshal heartbeat message", "error", err)
		return
	}

	gp.mu.Lock()
	defer gp.mu.Unlock()

	nodeID := heartbeatMsg.Node

	if gossipNode, exists := gp.memberlist[nodeID]; exists {
		// Update last seen time
		if gossipNode.Info != nil {
			gossipNode.Info.LastSeen = heartbeatMsg.Timestamp
			gp.cluster.UpdateNodeInfo(nodeID, gossipNode.Info)
		}

		// Clear suspicion if we receive a heartbeat
		if gossipNode.State == StateSuspect && heartbeatMsg.Incarnation >= gossipNode.Incarnation {
			gossipNode.State = StateAlive
			gossipNode.Suspicion = nil
			gossipNode.StateChange = time.Now()

			if gossipNode.Info != nil {
				gossipNode.Info.Status = NodeStatusAlive
				gp.cluster.UpdateNodeInfo(nodeID, gossipNode.Info)
			}
		}
	}
}

func (gp *GossipProtocol) gossipLoop(ctx context.Context) {
	ticker := time.NewTicker(gp.config.GossipInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gp.stopCh:
			return
		case <-ticker.C:
			gp.performGossip()
		}
	}
}

func (gp *GossipProtocol) performGossip() {
	// Stamp our own LastSeen before announcing ourselves. UpdateNodeInfo copies this field onto the
	// receiver's record, and performHealthChecks compares it against HeartbeatInterval*3 to decide
	// suspicion — so broadcasting the value set at construction, as this did until v0.11.0, means
	// every alive message we send is evidence that we have not been heard from. It was inert only
	// because the incarnation guard discarded our payload entirely; with that guard fixed it would
	// make a healthy node suspect itself into a flap (#272).
	// One critical section, because the incarnation must be the one that goes out with this payload:
	// taking the write lock to stamp, dropping it, and then calling getCurrentIncarnation would let a
	// refutation land in between and publish the old number with the new stats.
	// Sampled before the lock, because ReadMemStats stops the world and cm.cache is read under
	// cm.mu — holding gp.mu across either invites a stall or a lock-order inversion. The values land in
	// gp.localNode below, under gp.mu, alongside the timestamp they belong with.
	var fresh NodeInfo
	gp.cluster.refreshLocalStats(&fresh)

	gp.mu.Lock()
	gp.localNode.LastSeen = time.Now()

	// The figures a peer will route on. Until v0.11.0 these six fields were set to zero at
	// construction and never written again, so every node advertised itself as idle with an empty cache
	// forever — and the incarnation guard meant a peer would have discarded the update anyway (#132).
	gp.localNode.MemoryUsage = fresh.MemoryUsage
	gp.localNode.CacheSize = fresh.CacheSize
	gp.localNode.CacheHitRate = fresh.CacheHitRate
	gp.localNode.Operations = fresh.Operations

	incarnation := uint32(1)
	if self, exists := gp.memberlist[gp.localNode.ID]; exists {
		incarnation = self.Incarnation
	}

	// Addresses, not *GossipNode.
	//
	// This used to collect the pointers and read targetNode.Info after the unlock below, which is a
	// read of a field handleAliveMessage assigns from the receive goroutine — the whole point of a
	// memberlist is that another goroutine is updating it. Resolving the address here, inside the
	// critical section, is all the send loop actually needs, and it removes the aliasing rather than
	// synchronizing around it (#278).
	localID := gp.localNode.ID
	targets := make([]string, 0, len(gp.memberlist))
	for _, node := range gp.memberlist {
		// Guard against nodes whose Info was never populated (e.g. added via a
		// sync message with a nil Info field) to prevent a nil dereference (#113).
		if node.Info == nil {
			continue
		}
		if node.Info.ID != localID && node.State != StateDead && node.State != StateLeft {
			targets = append(targets, node.Info.Address)
		}
	}

	// Marshal under the lock: gp.localNode is read here and written above, and refuteSuspicion
	// marshals the same struct from a receive goroutine.
	data, marshalErr := json.Marshal(&AliveMessage{Node: gp.localNode, Incarnation: incarnation})
	gp.mu.Unlock()

	if marshalErr != nil {
		slog.Warn("failed to marshal alive message", "error", marshalErr)
		return
	}

	if len(targets) == 0 {
		return
	}

	// Select random nodes to gossip with
	fanout := min(gp.config.GossipFanout, len(targets))

	msg := &GossipMessage{
		Type:      MessageTypeAlive,
		From:      localID,
		Timestamp: time.Now(),
		MessageID: gp.generateMessageID(),
		Data:      data,
	}

	// Gossip to random subset of nodes
	for i := range fanout {
		_ = gp.sendMessage(targets[i%len(targets)], msg)
	}

	// Send heartbeat. Same incarnation as the alive message above, deliberately: the two describe one
	// moment, and handleHeartbeatMessage clears suspicion on `>=`, so a heartbeat carrying a stale
	// number would fail to clear a suspicion the accompanying alive message just refuted.
	heartbeatMsg := &HeartbeatMessage{
		Node:        localID,
		Timestamp:   time.Now(),
		Incarnation: incarnation,
	}

	heartbeatGossipMsg := &GossipMessage{
		Type:      MessageTypeGossipHeartbeat,
		From:      localID,
		Timestamp: time.Now(),
		MessageID: gp.generateMessageID(),
	}

	data, _ = json.Marshal(heartbeatMsg)
	heartbeatGossipMsg.Data = data

	_ = gp.broadcastMessage(heartbeatGossipMsg)
}

func (gp *GossipProtocol) suspicionTimer(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gp.stopCh:
			return
		case <-ticker.C:
			gp.checkSuspicions()
		}
	}
}

func (gp *GossipProtocol) checkSuspicions() {
	gp.mu.Lock()
	defer gp.mu.Unlock()

	now := time.Now()

	for nodeID, gossipNode := range gp.memberlist {
		if gossipNode.State == StateSuspect && gossipNode.Suspicion != nil {
			if now.After(gossipNode.Suspicion.Timeout) {
				// Suspicion timeout, mark as dead
				gossipNode.State = StateDead
				gossipNode.StateChange = now
				gossipNode.Suspicion = nil

				slog.Info("node suspicion timeout, marking as dead", "node_id", nodeID)

				// Broadcast dead message
				deadMsg := &DeadMessage{
					Node:        nodeID,
					Incarnation: gossipNode.Incarnation,
					From:        gp.localNode.ID,
				}

				msg := &GossipMessage{
					Type:      MessageTypeDead,
					From:      gp.localNode.ID,
					Timestamp: now,
					MessageID: gp.generateMessageID(),
				}

				data, _ := json.Marshal(deadMsg)
				msg.Data = data

				go func() {
					_ = gp.broadcastMessage(msg)
				}()

				// Update cluster manager
				if gossipNode.Info != nil {
					gossipNode.Info.Status = NodeStatusDead
					gp.cluster.UpdateNodeInfo(nodeID, gossipNode.Info)
				}
			}
		}
	}
}

func (gp *GossipProtocol) updateStats(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gp.stopCh:
			return
		case <-ticker.C:
			gp.calculateStats()
		}
	}
}

func (gp *GossipProtocol) calculateStats() {
	// AvgMessageLatency is not instrumented with per-round-trip timing.
	// The previous implementation stored time.Since(LastMessageReceived) which
	// is idle time (grows when the cluster is quiet), not latency. Leave the
	// field at its zero value until real per-message timing is added (#108).
}

// Helper methods

func (gp *GossipProtocol) sendMessage(addr string, msg *GossipMessage) error {
	// Every outgoing datagram is authenticated here rather than at each call site, because this is
	// the only place that writes to the socket — so a new message type cannot be introduced
	// unauthenticated by forgetting a step.
	data, err := gp.auth.seal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Refused here rather than sent and silently truncated at the far end. The sealed length is what
	// goes on the wire, so this is the last point at which the size is knowable and the first at which
	// it is final — checking before sealing would miss the envelope and the MAC.
	//
	// An error rather than a best-effort send, because the receiver cannot do anything useful with a
	// prefix: the JSON is cut mid-object and the datagram is dropped. Returning the error gives the
	// caller the chance to send something smaller, and gives an operator a message naming the limit
	// instead of a peer that has mysteriously stopped converging (#277).
	if len(data) > gp.config.MaxGossipPacket {
		gp.stats.mu.Lock()
		gp.stats.MessagesOversize++
		gp.stats.mu.Unlock()

		// Wraps ErrMessageOversize so a caller that can do something better than fail — send less, or
		// report the size to the peer that is waiting — can tell this apart from a socket error without
		// matching on the text. See [Coordinator.handleNetworkOperation] (#399), which was dropping this
		// error entirely and letting the requester time out 30 seconds later instead.
		return fmt.Errorf("%w: a %s message is %d bytes sealed, over the %d-byte max_gossip_packet: "+
			"raise it on every node in the cluster", ErrMessageOversize, msg.Type, len(data),
			gp.config.MaxGossipPacket)
	}

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to resolve address: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	_, err = conn.Write(data)
	if err != nil {
		gp.stats.mu.Lock()
		gp.stats.NetworkErrors++
		gp.stats.mu.Unlock()
		return fmt.Errorf("failed to send message: %w", err)
	}

	gp.stats.mu.Lock()
	gp.stats.MessagesSent++
	gp.stats.BytesSent += int64(len(data))
	gp.stats.MessagesByType[string(msg.Type)]++
	gp.stats.mu.Unlock()

	return nil
}

// broadcastMessage sends msg to every alive peer, reporting whether it was able to start sending.
//
// What the error does and does not mean. It is refused up front when this protocol is not listening —
// there is no socket, so nothing can be sent and the caller's message is lost, which used to be
// indistinguishable from a fanout to every peer. It says nothing about delivery: sends run concurrently
// and gossip is an unreliable datagram protocol with no acknowledgement, so a nil means "handed to the
// network for each peer", never "received". Callers needing convergence rely on retransmission, not on
// this return value.
//
// A cluster with no peers is a nil error and no sends. That is not a failure — a single-node cluster
// broadcasting has nobody to tell, and the operation it is reporting on succeeded locally.
func (gp *GossipProtocol) broadcastMessage(msg *GossipMessage) error {
	// Addresses rather than *NodeInfo, for the reason performGossip resolves them under the lock too:
	// a pointer taken out of the memberlist aliases a struct the receive goroutine owns. This one
	// happens to be safe today, because the only NodeInfo anything mutates in place is gp.localNode
	// and the ID filter below excludes it — but that is a property of a filter two lines away, not of
	// the code doing the read, and it is the same reasoning that made three other sites wrong (#278).
	gp.mu.RLock()
	listening := gp.conn != nil
	localID := gp.localNode.ID
	targets := make([]string, 0, len(gp.memberlist))
	for _, gossipNode := range gp.memberlist {
		if gossipNode.Info != nil && gossipNode.Info.ID != localID &&
			gossipNode.State != StateDead && gossipNode.State != StateLeft {
			targets = append(targets, gossipNode.Info.Address)
		}
	}
	gp.mu.RUnlock()

	// Checked against the socket rather than against gp being non-nil, because a GossipProtocol exists
	// from NewGossipProtocol onward and only acquires a socket in Start. A cluster constructed and not
	// started has a whole gossip object whose every send would dial out from an unbound port and go
	// nowhere in particular — reporting success for that is how a caller comes to believe peers were
	// told something they were not.
	if !listening {
		return fmt.Errorf("cannot broadcast %s: gossip is not listening, so this node has no way to "+
			"reach its peers: %w", msg.Type, types.ErrNotSupported)
	}

	for _, addr := range targets {
		go func(addr string) {
			// Errors are counted rather than returned: this runs after the caller has its answer, and one
			// unreachable peer is the normal state of a gossip cluster rather than a failure of the
			// broadcast. sendMessage already increments NetworkErrors and MessagesOversize, which is where
			// an operator sees it.
			_ = gp.sendMessage(addr, msg)
		}(addr)
	}

	return nil
}

// sendSyncMessage sends this node's full membership view to addr, split across as many datagrams as
// it takes to stay under MaxGossipPacket.
//
// Chunking is sound because [GossipProtocol.handleSyncMessage] merges per node rather than replacing
// the memberlist: each chunk is a complete SyncMessage that means "these members, as I last saw
// them," and a receiver applying two of them reaches the same state as one applying their union. So a
// lost or reordered chunk costs the freshness of the members it carried until the next sync, and
// nothing more. That is already true of gossip generally — this is why the protocol is eventually
// consistent rather than atomic — and it is what makes chunking preferable to failing the whole sync,
// which is what a single oversize datagram used to do silently (#277).
func (gp *GossipProtocol) sendSyncMessage(addr string) error {
	// Marshaled under the lock, as performGossip does for its own message and for the same reason.
	//
	// This used to take the read lock only to maps.Copy the memberlist and then marshal outside it —
	// which looks synchronized and is not, because maps.Copy copies the *GossipNode pointers. The copy
	// therefore aliases the originals, and this node's entry aliases gp.localNode itself, since
	// NewGossipProtocol stores that very pointer as its Info. So the marshal walked structs that
	// performGossip was concurrently stamping with a fresh LastSeen and that handleSyncMessage was
	// concurrently overwriting. -race reported it on any cluster where a node joins while the gossip
	// loop runs, which is every cluster (#278).
	//
	// Marshaling under the lock rather than deep-copying, because the alternative is a copy that has
	// to stay deep as GossipNode grows — it holds two pointers today — and a shallow one is exactly the
	// bug being fixed.
	gp.mu.RLock()
	from := gp.localNode.ID
	chunks, marshalErr := gp.marshalSyncChunksLocked()
	gp.mu.RUnlock()

	if marshalErr != nil {
		return fmt.Errorf("marshaling the sync message: %w", marshalErr)
	}

	// Every chunk attempted even if one fails, and the first error returned: a partial sync is worth
	// more than none, and stopping at the first failure would make the members in later chunks depend
	// on the ones before them.
	var firstErr error
	for _, data := range chunks {
		err := gp.sendMessage(addr, &GossipMessage{
			Type:      MessageTypeSync,
			From:      from,
			Timestamp: time.Now(),
			MessageID: gp.generateMessageID(),
			Data:      data,
		})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// marshalSyncChunksLocked marshals gp.memberlist into one or more SyncMessage payloads, each small
// enough that the datagram carrying it fits MaxGossipPacket. gp.mu must be held.
//
// The budget is derived by measuring rather than by estimating the envelope: seal wraps the payload in
// JSON with a hex MAC, and base64-encodes it as a []byte field, so the ratio between payload and
// datagram is not a constant anyone should hardcode. Each candidate chunk is therefore grown one
// member at a time and the whole message sealed to check, which costs an extra marshal per member on a
// path that runs once per join — not per gossip round.
func (gp *GossipProtocol) marshalSyncChunksLocked() ([][]byte, error) {
	ids := slices.Sorted(maps.Keys(gp.memberlist))

	// Sorted so that a chunk boundary is a function of the membership rather than of Go's map
	// iteration order. Two syncs of an unchanged memberlist then produce identical chunks, which is
	// what makes a truncation bug reproducible instead of intermittent.

	var (
		chunks  [][]byte
		current = map[string]*GossipNode{}
	)

	flush := func() error {
		if len(current) == 0 {
			return nil
		}
		data, err := json.Marshal(&SyncMessage{Nodes: current})
		if err != nil {
			return err
		}
		chunks = append(chunks, data)
		current = map[string]*GossipNode{}
		return nil
	}

	for _, id := range ids {
		current[id] = gp.memberlist[id]

		fits, err := gp.syncChunkFits(current)
		if err != nil {
			return nil, err
		}
		if fits {
			continue
		}

		// Over budget with this member added. Send what fit without it, then start a chunk with it.
		if len(current) == 1 {
			// A single member does not fit, so no chunking can help. Emit it anyway and let
			// sendMessage refuse it with a message naming the limit — silently dropping a member from
			// the sync would make it disappear from the cluster's view with nothing logged, which is
			// strictly worse than a loud failure.
			slog.Warn("a single member does not fit max_gossip_packet; the sync for it will be refused",
				"node_id", id, "max_gossip_packet", gp.config.MaxGossipPacket)
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}

		delete(current, id)
		if err := flush(); err != nil {
			return nil, err
		}
		current[id] = gp.memberlist[id]
	}

	if err := flush(); err != nil {
		return nil, err
	}

	return chunks, nil
}

// syncChunkFits reports whether a sync message carrying nodes would fit MaxGossipPacket once sealed.
func (gp *GossipProtocol) syncChunkFits(nodes map[string]*GossipNode) (bool, error) {
	payload, err := json.Marshal(&SyncMessage{Nodes: nodes})
	if err != nil {
		return false, err
	}

	// The same message sendMessage would build, so the measurement includes the envelope, the MAC, and
	// the message ID rather than a guess at their combined size. MessageID is generated here and
	// discarded: it varies in content but not in length.
	sealed, err := gp.auth.seal(&GossipMessage{
		Type:      MessageTypeSync,
		From:      gp.localNode.ID,
		Timestamp: time.Now(),
		MessageID: gp.generateMessageID(),
		Data:      payload,
	})
	if err != nil {
		return false, err
	}

	return len(sealed) <= gp.config.MaxGossipPacket, nil
}

func (gp *GossipProtocol) getCurrentIncarnation() uint32 {
	gp.mu.RLock()
	defer gp.mu.RUnlock()

	if localNode, exists := gp.memberlist[gp.localNode.ID]; exists {
		return localNode.Incarnation
	}
	return 1
}

func (gp *GossipProtocol) generateMessageID() string {
	bytes := make([]byte, 4)
	_, _ = cryptorand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// sendConsensusMsg is a convenience helper that marshals payload into a
// GossipMessage and sends it to addr over the gossip UDP socket.
func (gp *GossipProtocol) sendConsensusMsg(addr string, msgType MessageType, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal consensus message: %w", err)
	}
	return gp.sendMessage(addr, &GossipMessage{
		Type:      msgType,
		From:      gp.localNode.ID,
		Timestamp: time.Now(),
		MessageID: gp.generateMessageID(),
		Data:      data,
	})
}

// LocalAddr returns the local UDP address the gossip socket is bound to.
// Returns "" if the socket has not been started yet.
func (gp *GossipProtocol) LocalAddr() string {
	if gp.conn == nil {
		return ""
	}
	return gp.conn.LocalAddr().String()
}

// GetStats returns gossip protocol statistics
func (gp *GossipProtocol) GetStats() *GossipStats {
	gp.stats.mu.RLock()
	stats := &GossipStats{
		MessagesSent:        gp.stats.MessagesSent,
		MessagesReceived:    gp.stats.MessagesReceived,
		BytesSent:           gp.stats.BytesSent,
		BytesReceived:       gp.stats.BytesReceived,
		NodesDiscovered:     gp.stats.NodesDiscovered,
		SuspicionEvents:     gp.stats.SuspicionEvents,
		DeathEvents:         gp.stats.DeathEvents,
		NetworkErrors:       gp.stats.NetworkErrors,
		AvgMessageLatency:   gp.stats.AvgMessageLatency,
		LastMessageReceived: gp.stats.LastMessageReceived,
		MessagesByType:      make(map[string]int64),

		MessagesRejected:        gp.stats.MessagesRejected,
		MessagesUnauthenticated: gp.stats.MessagesUnauthenticated,
		MessagesReplayed:        gp.stats.MessagesReplayed,
		MessagesWrongVersion:    gp.stats.MessagesWrongVersion,
		SuspicionRefutations:    gp.stats.SuspicionRefutations,
		MessagesTruncated:       gp.stats.MessagesTruncated,
		MessagesOversize:        gp.stats.MessagesOversize,
	}
	maps.Copy(stats.MessagesByType, gp.stats.MessagesByType)
	gp.stats.mu.RUnlock()

	return stats
}

// GetMemberlist returns the current memberlist
func (gp *GossipProtocol) GetMemberlist() map[string]*GossipNode {
	gp.mu.RLock()
	defer gp.mu.RUnlock()

	memberlist := make(map[string]*GossipNode)
	for id, node := range gp.memberlist {
		// Create a copy
		nodeCopy := *node
		if node.Info != nil {
			infoCopy := *node.Info
			infoCopy.Metadata = make(map[string]string)
			maps.Copy(infoCopy.Metadata, node.Info.Metadata)
			nodeCopy.Info = &infoCopy
		}
		if node.Suspicion != nil {
			suspicionCopy := *node.Suspicion
			suspicionCopy.From = append([]string(nil), node.Suspicion.From...)
			nodeCopy.Suspicion = &suspicionCopy
		}
		memberlist[id] = &nodeCopy
	}

	return memberlist
}
