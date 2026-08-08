package distributed

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

// ConsensusEngine elects a leader using Raft's election mechanics — terms, votes, and a randomized
// election timeout — over gossip. It is not a consensus engine in the sense the name implies: the log
// below holds one bootstrap noop plus one entry per election win, applyLogEntry has no state machine
// to apply anything to, and nothing appends an operation entry. Leader election works and is tested
// (#279); replication does not exist.
//
// It is not going to. #169 concluded that Raft has nothing to replicate here — nodes hold no
// authoritative state, the bucket does — and the CAS direction was adopted on 2026-08-03, closing
// #128 (the log interface), #130 (persistent state), #133 (proposal broadcast) and #151 (compaction).
// The log, commitIndex, lastApplied, nextIndex and matchIndex fields serve the election only. #284
// took out the proposal machinery that sat beside them: a ConsensusProposal, four statuses, three
// types, a proposals map, a 30-second expiry sweep, and a `broadcastProposal` that slept 100ms and
// then voted for its own proposal so that `voteOnProposal` would find a majority of one and execute
// it. Nothing in the repository proposed anything but a leadership change, and that path reached
// `SetLeader` — which the election already does, having actually contested it.
type ConsensusEngine struct {
	mu          sync.RWMutex
	cluster     *ClusterManager
	config      *ClusterConfig
	state       ConsensusState
	currentTerm uint64
	votedFor    string
	log         []*LogEntry
	commitIndex uint64
	lastApplied uint64

	// Leader state
	nextIndex  map[string]uint64
	matchIndex map[string]uint64

	// Election state
	electionTimer *time.Timer
	voteCount     int

	stats  *ConsensusStats
	stopCh chan struct{}
}

// ConsensusState represents the state of a node in the consensus protocol
type ConsensusState int

const (
	StateFollower ConsensusState = iota
	StateCandidate
	StateLeader
)

func (s ConsensusState) String() string {
	switch s {
	case StateFollower:
		return "follower"
	case StateCandidate:
		return "candidate"
	case StateLeader:
		return "leader"
	default:
		return "unknown"
	}
}

// LogEntry represents an entry in the distributed log
type LogEntry struct {
	Term      uint64    `json:"term"`
	Index     uint64    `json:"index"`
	Type      EntryType `json:"type"`
	Data      []byte    `json:"data"`
	Timestamp time.Time `json:"timestamp"`
	ClientID  string    `json:"client_id"`
	RequestID string    `json:"request_id"`
}

// EntryType represents the type of log entry.
//
// Two of them, and both are appended by code in this file: a noop at index 0 to anchor the log, and
// one entry per election win. #284 removed EntryTypeConfigChange, EntryTypeOperation and
// EntryTypeSnapshot, which named things nothing produced — there is no configuration-change path, no
// operation is ever logged (coordinator.go goes to the backend directly), and #151, which would have
// added snapshotting, was closed when the CAS direction was adopted.
//
// The strings are unchanged, so an entry of a removed type arriving from an older peer still decodes
// and still applies as the no-op it always was; see [ConsensusEngine.applyLogEntry].
type EntryType string

const (
	EntryTypeNoop           EntryType = "noop"
	EntryTypeLeaderElection EntryType = "leader_election"
)

// RequestVoteMessage represents a vote request
type RequestVoteMessage struct {
	Term         uint64 `json:"term"`
	CandidateID  string `json:"candidate_id"`
	LastLogIndex uint64 `json:"last_log_index"`
	LastLogTerm  uint64 `json:"last_log_term"`
}

// RequestVoteResponse represents a vote response (legacy; kept for compatibility)
type RequestVoteResponse struct {
	VoteGranted bool `json:"vote_granted"`
}

// RequestVoteRespMessage is the network-level vote response carrying term and
// the voter's identity so the receiver can call handleVoteResponse.
type RequestVoteRespMessage struct {
	Term        uint64 `json:"term"`
	VoteGranted bool   `json:"vote_granted"`
	From        string `json:"from"`
}

// AppendEntriesMessage represents a log replication / heartbeat message
type AppendEntriesMessage struct {
	Term         uint64      `json:"term"`
	LeaderID     string      `json:"leader_id"`
	PrevLogIndex uint64      `json:"prev_log_index"`
	PrevLogTerm  uint64      `json:"prev_log_term"`
	Entries      []*LogEntry `json:"entries"`
	LeaderCommit uint64      `json:"leader_commit"`
}

// AppendEntriesResponse represents a log replication response
type AppendEntriesResponse struct {
	Success    bool   `json:"success"`
	MatchIndex uint64 `json:"match_index"`
}

// ConsensusStats tracks consensus protocol statistics
type ConsensusStats struct {
	mu               sync.RWMutex
	CurrentState     string        `json:"current_state"`
	CurrentTerm      uint64        `json:"current_term"`
	CurrentLeader    string        `json:"current_leader"`
	LogLength        int           `json:"log_length"`
	CommitIndex      uint64        `json:"commit_index"`
	LastApplied      uint64        `json:"last_applied"`
	ElectionsStarted int64         `json:"elections_started"`
	ElectionsWon     int64         `json:"elections_won"`
	VotesCast        int64         `json:"votes_cast"`
	LogEntriesAdded  int64         `json:"log_entries_added"`
	HeartbeatsSent   int64         `json:"heartbeats_sent"`
	LastElection     time.Time     `json:"last_election"`
	Uptime           time.Duration `json:"uptime"`
}

// NewConsensusEngine creates a new consensus engine
func NewConsensusEngine(cluster *ClusterManager, config *ClusterConfig) (*ConsensusEngine, error) {
	ce := &ConsensusEngine{
		cluster:     cluster,
		config:      config,
		state:       StateFollower,
		currentTerm: 0,
		votedFor:    "",
		log:         make([]*LogEntry, 0),
		nextIndex:   make(map[string]uint64),
		matchIndex:  make(map[string]uint64),
		stats: &ConsensusStats{
			CurrentState: StateFollower.String(),
		},
		stopCh: make(chan struct{}),
	}

	// Initialize with no-op entry
	ce.log = append(ce.log, &LogEntry{
		Term:      0,
		Index:     0,
		Type:      EntryTypeNoop,
		Data:      []byte("initial"),
		Timestamp: time.Now(),
	})

	return ce, nil
}

// Start starts the consensus engine
func (ce *ConsensusEngine) Start(ctx context.Context) error {
	slog.Info("starting consensus engine", "node_id", ce.cluster.GetNodeID())

	// Under the write lock because resetElectionTimer writes ce.electionTimer and does not lock for
	// itself — see its doc comment. This call is before any goroutine below exists, but the gossip
	// receiver started by ClusterManager.Start already does, and an inbound AppendEntries reaches
	// resetElectionTimer from there.
	ce.mu.Lock()
	ce.resetElectionTimer()
	ce.mu.Unlock()

	// Start background goroutines
	go ce.electionLoop(ctx)
	go ce.heartbeatLoop(ctx)
	go ce.updateStats(ctx)

	return nil
}

// Stop stops the consensus engine.
//
// The timer is read under the write lock, not unlocked. A heartbeat arriving from the gossip receiver
// reaches resetElectionTimer, which replaces ce.electionTimer, and Stop racing that read the field
// while it was being written — a data race CI caught in TriggerElection's two peer tests and that
// reproduces locally only under -count on a loaded machine.
//
// Neither shutdown signal orders the two. Closing stopCh does not, because the receiver goroutine
// belongs to GossipProtocol and does not watch it; and although ClusterManager.Stop stops gossip
// before consensus, GossipProtocol.Stop closes the socket without waiting for the receiver, so a
// message already in a handler keeps running past it.
func (ce *ConsensusEngine) Stop() error {
	close(ce.stopCh)

	ce.mu.Lock()
	if ce.electionTimer != nil {
		ce.electionTimer.Stop()
	}
	ce.mu.Unlock()

	slog.Info("consensus engine stopped")
	return nil
}

// TriggerElection triggers a new leader election
func (ce *ConsensusEngine) TriggerElection(ctx context.Context) error {
	ce.mu.Lock()
	defer ce.mu.Unlock()

	// Only trigger election if we're not already a leader
	if ce.state == StateLeader {
		return nil
	}

	slog.Info("triggering leader election")
	ce.startElection()

	return nil
}

// Background loops

func (ce *ConsensusEngine) electionLoop(ctx context.Context) {
	for {
		// Snapshot the timer channel under the read lock to avoid a data race
		// with resetElectionTimer, which writes ce.electionTimer under the
		// write lock.
		ce.mu.RLock()
		timerCh := ce.electionTimer.C
		ce.mu.RUnlock()

		select {
		case <-ctx.Done():
			return
		case <-ce.stopCh:
			return
		case <-timerCh:
			ce.mu.Lock()
			if ce.state != StateLeader {
				slog.Info("election timeout, starting new election")
				ce.startElection()
			}
			ce.mu.Unlock()
		}
	}
}

func (ce *ConsensusEngine) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(ce.config.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ce.stopCh:
			return
		case <-ticker.C:
			ce.mu.RLock()
			isLeader := ce.state == StateLeader
			ce.mu.RUnlock()

			if isLeader {
				ce.sendHeartbeats()
			}
		}
	}
}

// Election methods

func (ce *ConsensusEngine) startElection() {
	ce.state = StateCandidate
	ce.currentTerm++
	ce.votedFor = ce.cluster.GetNodeID()
	ce.voteCount = 1 // Vote for ourselves

	ce.resetElectionTimer()

	slog.Info("starting election", "term", ce.currentTerm)

	ce.stats.mu.Lock()
	ce.stats.ElectionsStarted++
	ce.stats.LastElection = time.Now()
	ce.stats.CurrentTerm = ce.currentTerm
	ce.stats.CurrentState = ce.state.String()
	ce.stats.mu.Unlock()

	// Send vote requests to all other nodes
	ce.sendVoteRequests()

	// And evaluate the majority we may already hold. Until v0.11.0 this check lived only in
	// handleVoteResponse, which is reached only when a peer replies — so a single-node cluster, which
	// satisfies its own majority with the vote cast on the line above, never evaluated it. The election
	// timer fired again, the term incremented, and the loop repeated for the life of the process. A
	// cluster of one is not a corner case: it is the first thing anyone runs, and the shape a
	// deployment has while the second node is still being provisioned (#275).
	ce.checkVoteMajority()
}

func (ce *ConsensusEngine) sendVoteRequests() {
	nodes := ce.cluster.GetNodes()

	requestVote := &RequestVoteMessage{
		Term:         ce.currentTerm,
		CandidateID:  ce.cluster.GetNodeID(),
		LastLogIndex: ce.getLastLogIndex(),
		LastLogTerm:  ce.getLastLogTerm(),
	}

	for nodeID, node := range nodes {
		if nodeID != ce.cluster.GetNodeID() && node.Status == NodeStatusAlive {
			go ce.sendVoteRequest(nodeID, requestVote)
		}
	}
}

func (ce *ConsensusEngine) sendVoteRequest(nodeID string, req *RequestVoteMessage) {
	slog.Info("sending vote request", "node_id", nodeID, "term", req.Term)

	nodes := ce.cluster.GetNodes()
	node, exists := nodes[nodeID]
	if !exists || node.Status != NodeStatusAlive {
		return
	}

	if err := ce.cluster.gossip.sendConsensusMsg(node.Address, MessageTypeRequestVote, req); err != nil {
		slog.Warn("failed to send vote request", "node_id", nodeID, "error", err)
	}
}

func (ce *ConsensusEngine) handleVoteResponse(nodeID string, voteGranted bool) {
	ce.mu.Lock()
	defer ce.mu.Unlock()

	if ce.state != StateCandidate {
		return
	}

	if voteGranted {
		ce.voteCount++
		slog.Info("received vote", "from", nodeID, "total_votes", ce.voteCount)
	}

	ce.checkVoteMajority()
}

// checkVoteMajority promotes this node to leader if the votes it has collected are a majority of the
// alive membership.
//
// Called from every point where the vote count changes — startElection, which casts this node's vote
// for itself, and handleVoteResponse, which counts a peer's. That is the whole of the fix for #275:
// the comparison used to live inline in handleVoteResponse alone, so it was evaluated only when a
// peer replied, and a cluster of one — which satisfies its own majority the moment startElection
// increments the count to 1 — never evaluated it at all.
//
// The caller must hold ce.mu, as becomeLeader requires and both callers already do.
func (ce *ConsensusEngine) checkVoteMajority() {
	if ce.state != StateCandidate {
		return
	}

	nodes := ce.cluster.GetNodes()
	aliveNodes := 0
	for _, node := range nodes {
		if node.Status == NodeStatusAlive {
			aliveNodes++
		}
	}

	// A membership of just this node is a majority of one — but only if this node is the whole cluster.
	// A node configured with seed nodes has peers it has not discovered yet: gossip learns of a peer
	// from an inbound message, and startElection runs on a timer that does not wait for it. Promoting
	// on a self-only view would elect every node of a starting cluster its own leader at term 1, each
	// on one vote, before any of them had heard of the others — which is what a probe of three seeded
	// nodes showed once the majority check reached startElection at all.
	//
	// So the rule is the bootstrap-versus-join distinction: a node that names no seeds is declaring
	// itself the whole of a new cluster and may elect itself, which is #275's case and the shape of
	// every single-node deployment. A node that names seeds must hear from one before it can hold a
	// majority. Discovery makes aliveNodes exceed one and the ordinary vote path takes over.
	if aliveNodes <= 1 && ce.cluster.expectsPeers() {
		slog.Debug("declining to self-elect before discovering the configured seed nodes",
			"term", ce.currentTerm, "alive_nodes", aliveNodes)
		return
	}

	majority := aliveNodes/2 + 1
	if ce.voteCount >= majority {
		ce.becomeLeader()
	}
}

func (ce *ConsensusEngine) becomeLeader() {
	slog.Info("became leader", "term", ce.currentTerm)

	ce.state = StateLeader
	ce.cluster.SetLeader(ce.cluster.GetNodeID())

	// Initialize leader state
	nodes := ce.cluster.GetNodes()
	lastLogIndex := ce.getLastLogIndex()

	for nodeID := range nodes {
		if nodeID != ce.cluster.GetNodeID() {
			ce.nextIndex[nodeID] = lastLogIndex + 1
			ce.matchIndex[nodeID] = 0
		}
	}

	ce.stats.mu.Lock()
	ce.stats.ElectionsWon++
	ce.stats.CurrentState = ce.state.String()
	ce.stats.CurrentLeader = ce.cluster.GetNodeID()
	ce.stats.mu.Unlock()

	// Send initial heartbeat (in a goroutine to avoid holding the lock while
	// sendHeartbeats attempts to reacquire it for reading).
	go ce.sendHeartbeats()

	// Add leader election log entry
	entry := &LogEntry{
		Term:      ce.currentTerm,
		Index:     ce.getLastLogIndex() + 1,
		Type:      EntryTypeLeaderElection,
		Data:      []byte(ce.cluster.GetNodeID()),
		Timestamp: time.Now(),
	}

	ce.log = append(ce.log, entry)

	ce.stats.mu.Lock()
	ce.stats.LogEntriesAdded++
	ce.stats.mu.Unlock()
}

// Heartbeat and log replication

func (ce *ConsensusEngine) sendHeartbeats() {
	ce.mu.RLock()
	if ce.state != StateLeader {
		ce.mu.RUnlock()
		return
	}

	nodes := ce.cluster.GetNodes()
	ce.mu.RUnlock()

	for nodeID, node := range nodes {
		if nodeID != ce.cluster.GetNodeID() && node.Status == NodeStatusAlive {
			go ce.sendAppendEntries(nodeID, true) // true = heartbeat
		}
	}

	ce.stats.mu.Lock()
	ce.stats.HeartbeatsSent++
	ce.stats.mu.Unlock()
}

func (ce *ConsensusEngine) sendAppendEntries(nodeID string, isHeartbeat bool) {
	ce.mu.RLock()

	nodes := ce.cluster.GetNodes()
	node, exists := nodes[nodeID]
	if !exists || node.Status != NodeStatusAlive {
		ce.mu.RUnlock()
		return
	}
	addr := node.Address

	nextIndex := ce.nextIndex[nodeID]
	if nextIndex == 0 {
		nextIndex = 1
	}
	prevLogIndex := nextIndex - 1
	prevLogTerm := uint64(0)

	if prevLogIndex > 0 && prevLogIndex <= uint64(len(ce.log)) {
		prevLogTerm = ce.log[prevLogIndex-1].Term
	}

	var entries []*LogEntry
	if !isHeartbeat && nextIndex <= uint64(len(ce.log)) {
		entries = ce.log[nextIndex-1:]
	}

	msg := &AppendEntriesMessage{
		Term:         ce.currentTerm,
		LeaderID:     ce.cluster.GetNodeID(),
		PrevLogIndex: prevLogIndex,
		PrevLogTerm:  prevLogTerm,
		Entries:      entries,
		LeaderCommit: ce.commitIndex,
	}

	ce.mu.RUnlock()

	slog.Info("sending append entries", "node_id", nodeID, "heartbeat", isHeartbeat)

	if err := ce.cluster.gossip.sendConsensusMsg(addr, MessageTypeAppendEntries, msg); err != nil {
		slog.Warn("failed to send append entries", "node_id", nodeID, "error", err)
	}
}

func (ce *ConsensusEngine) handleAppendEntriesResponse(nodeID string, resp *AppendEntriesResponse) {
	ce.mu.Lock()
	defer ce.mu.Unlock()

	if ce.state != StateLeader {
		return
	}

	if resp.Success {
		ce.matchIndex[nodeID] = resp.MatchIndex
		ce.nextIndex[nodeID] = resp.MatchIndex + 1

		// Update commit index if majority has replicated
		ce.updateCommitIndex()
	} else {
		// Decrease nextIndex and retry
		if ce.nextIndex[nodeID] > 1 {
			ce.nextIndex[nodeID]--
		}
		go ce.sendAppendEntries(nodeID, false)
	}
}

// handleNetworkRequestVote processes an incoming RequestVote RPC from a peer.
func (ce *ConsensusEngine) handleNetworkRequestVote(msg *GossipMessage) {
	var req RequestVoteMessage
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Warn("failed to unmarshal RequestVoteMessage", "error", err)
		return
	}

	ce.mu.Lock()

	// Step down if we see a higher term.
	steppedDown := false
	if req.Term > ce.currentTerm {
		steppedDown = ce.state == StateLeader
		ce.currentTerm = req.Term
		ce.state = StateFollower
		ce.votedFor = ""
	}

	voteGranted := false
	if req.Term == ce.currentTerm &&
		(ce.votedFor == "" || ce.votedFor == req.CandidateID) {
		lastLogTerm := ce.getLastLogTerm()
		lastLogIndex := ce.getLastLogIndex()
		logUpToDate := req.LastLogTerm > lastLogTerm ||
			(req.LastLogTerm == lastLogTerm && req.LastLogIndex >= lastLogIndex)
		if logUpToDate {
			voteGranted = true
			ce.votedFor = req.CandidateID
		}
	}

	currentTerm := ce.currentTerm
	ce.mu.Unlock()

	// A leader that steps down on a higher term has to stop claiming leadership, for the same reason
	// handleNetworkAppendEntries does — see the comment there. There is no new leader to name: the
	// candidate that sent this request has not won anything yet. Clearing it is what an empty leader
	// means, and monitorCluster already uses that value when a leader is declared dead.
	if steppedDown {
		slog.Info("stepping down: a vote request arrived at a higher term",
			"term", currentTerm, "candidate", req.CandidateID)
		ce.cluster.SetLeader("")
	}

	resp := &RequestVoteRespMessage{
		Term:        currentTerm,
		VoteGranted: voteGranted,
		From:        ce.cluster.GetNodeID(),
	}

	nodes := ce.cluster.GetNodes()
	node, exists := nodes[msg.From]
	if !exists {
		slog.Warn("cannot send vote response: node not found", "node_id", msg.From)
		return
	}

	slog.Info("sending vote response", "node_id", msg.From, "granted", voteGranted, "term", currentTerm)

	if err := ce.cluster.gossip.sendConsensusMsg(node.Address, MessageTypeRequestVoteResp, resp); err != nil {
		slog.Warn("failed to send vote response", "node_id", msg.From, "error", err)
	}
}

// handleNetworkRequestVoteResp processes an incoming RequestVote response.
func (ce *ConsensusEngine) handleNetworkRequestVoteResp(msg *GossipMessage) {
	var resp RequestVoteRespMessage
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		slog.Warn("failed to unmarshal RequestVoteRespMessage", "error", err)
		return
	}

	ce.mu.Lock()
	if resp.Term > ce.currentTerm {
		ce.currentTerm = resp.Term
		ce.state = StateFollower
		ce.votedFor = ""
		ce.mu.Unlock()
		return
	}
	ce.mu.Unlock()

	ce.handleVoteResponse(resp.From, resp.VoteGranted)
}

// handleNetworkAppendEntries processes an incoming AppendEntries / heartbeat RPC.
func (ce *ConsensusEngine) handleNetworkAppendEntries(msg *GossipMessage) {
	var req AppendEntriesMessage
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Warn("failed to unmarshal AppendEntriesMessage", "error", err)
		return
	}

	ce.mu.Lock()

	// Set while ce.mu is held, applied after it is released — see the comment at the assignment.
	newLeader := ""

	success := false
	if req.Term >= ce.currentTerm {
		// Recognized leader — step down if needed and reset election timer.
		if req.Term > ce.currentTerm {
			ce.currentTerm = req.Term
			ce.votedFor = ""
		}
		ce.state = StateFollower
		ce.resetElectionTimer()
		success = true

		// And tell the cluster manager, which is what callers actually ask. Until v0.11.0 this line was
		// missing: becomeLeader calls SetLeader but no step-down path did, so cm.isLeader was
		// effectively write-once. A node that had been leader and then received a heartbeat from a
		// higher term became a follower in ce.state while ClusterManager.IsLeader kept returning true
		// for the life of the process — so two nodes each claiming leadership stayed that way
		// permanently instead of resolving, which is how a transient election race became a durable
		// split brain (#275).
		//
		// Deferred past ce.mu.Unlock below rather than called here: SetLeader takes cm.mu, and while
		// ce.mu → cm.mu is the established order (becomeLeader does exactly that), there is no reason
		// to hold the consensus lock across it.
		newLeader = req.LeaderID

		// Append any new entries (simplified; full consistency is future work).
		for _, entry := range req.Entries {
			if entry.Index > uint64(len(ce.log)) {
				ce.log = append(ce.log, entry)
			}
		}

		// Advance commit index.
		if req.LeaderCommit > ce.commitIndex {
			newCommit := req.LeaderCommit
			if lastIdx := ce.getLastLogIndex(); lastIdx < newCommit {
				newCommit = lastIdx
			}
			ce.commitIndex = newCommit
		}
	}

	matchIndex := req.PrevLogIndex + uint64(len(req.Entries))
	ce.mu.Unlock()

	if newLeader != "" {
		ce.cluster.SetLeader(newLeader)
	}

	resp := &AppendEntriesResponse{
		Success:    success,
		MatchIndex: matchIndex,
	}

	nodes := ce.cluster.GetNodes()
	node, exists := nodes[msg.From]
	if !exists {
		return
	}

	if err := ce.cluster.gossip.sendConsensusMsg(node.Address, MessageTypeAppendEntriesResp, resp); err != nil {
		slog.Warn("failed to send AppendEntries response", "node_id", msg.From, "error", err)
	}
}

// handleNetworkAppendEntriesResp processes an incoming AppendEntries response.
func (ce *ConsensusEngine) handleNetworkAppendEntriesResp(msg *GossipMessage) {
	var resp AppendEntriesResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		slog.Warn("failed to unmarshal AppendEntriesResponse", "error", err)
		return
	}

	ce.handleAppendEntriesResponse(msg.From, &resp)
}

func (ce *ConsensusEngine) updateCommitIndex() {
	nodes := ce.cluster.GetNodes()
	aliveNodes := 0
	for _, node := range nodes {
		if node.Status == NodeStatusAlive {
			aliveNodes++
		}
	}

	majority := aliveNodes/2 + 1

	// Find the highest log index that has been replicated to majority
	for n := ce.commitIndex + 1; n <= uint64(len(ce.log)); n++ {
		replicationCount := 1 // Count ourselves

		for nodeID := range nodes {
			if nodeID != ce.cluster.GetNodeID() && ce.matchIndex[nodeID] >= n {
				replicationCount++
			}
		}

		if replicationCount >= majority {
			ce.commitIndex = n

			// Apply committed entries
			for ce.lastApplied < ce.commitIndex {
				ce.lastApplied++
				ce.applyLogEntry(ce.log[ce.lastApplied-1])
			}
		} else {
			break
		}
	}
}

// applyLogEntry advances lastApplied past entry. There is no state machine for it to advance, and that
// is the honest state of this engine rather than a gap: both entry types [EntryType] declares are
// records of something that already happened — the log anchor and an election this node won, whose
// effect ClusterManager.SetLeader had before the entry was appended.
//
// So this logs and returns. It used to be a five-arm switch with every arm empty, three of whose arms
// named entry types nothing produced; #284 removed those types and the arms with them, and what
// remained was a switch whose branches were indistinguishable. #169 decided coordination moves to S3
// conditional writes rather than to Raft log replication, so this is not a to-do list.
//
// It stays a method, and is still called, because lastApplied must advance for commitIndex to mean
// anything and because a per-entry line is what makes an election traceable in a log.
func (ce *ConsensusEngine) applyLogEntry(entry *LogEntry) {
	slog.Info("applying log entry", "index", entry.Index, "type", entry.Type)
}

// Utility methods

// resetElectionTimer replaces the election timer with a fresh randomized one.
//
// ce.mu must be held for writing. It writes ce.electionTimer, which electionLoop reads under the read
// lock and Stop reads under the write lock; every caller — Start, startElection, and
// handleNetworkAppendEntries — holds it.
func (ce *ConsensusEngine) resetElectionTimer() {
	timeout := electionTimeout(ce.config.ElectionTimeout)

	// Use time.NewTimer (not time.AfterFunc) so that the .C channel is
	// populated.  time.AfterFunc leaves .C == nil, which caused electionLoop
	// to block on a nil channel and never fire (#101).
	if ce.electionTimer != nil {
		ce.electionTimer.Stop()
	}
	ce.electionTimer = time.NewTimer(timeout)
}

// electionTimeout returns base plus up to 100% of base of random jitter, or a value in
// [150ms, 300ms) when base is not positive.
//
// Split out from resetElectionTimer so the arithmetic is testable: the timer that method builds does
// not expose the duration it was built with, so a spread this function got wrong was unobservable
// from outside. And it was wrong. It read
//
//	base + time.Duration(rand.Intn(int(base.Milliseconds())))
//
// where the two units disagree: Milliseconds() yields a count, which then became a Duration in
// *nanoseconds*. The default 5s timeout therefore got between 0 and 4999ns of spread — measured at
// 4.69µs, 4.16µs and 690ns against a 5s base, a jitter of about one part in a million. Randomized
// election timeouts exist so that followers whose timers expire together do not all become candidates
// in the same instant and split the vote; a spread that small is not randomization, and a split vote
// is precisely what Raft §5.2 introduces this jitter to avoid.
//
// It also panicked outright below one millisecond. Milliseconds() truncates, so a sub-ms base made
// the argument 0, and rand.Intn(0) is "invalid argument to Intn" — verified. NewClusterManager
// defaults the field to 5s so no shipped config reaches it, but `election_timeout: 500us` from a test
// or an operator would take the panic on the first timer reset, on the consensus goroutine.
//
// math/rand is right here and the gosec G404 finding is noise: this picks a timer spread, not a
// secret. Reading crypto/rand on every AppendEntries would put a syscall on the consensus hot path to
// defend against an adversary that does not exist — a node able to influence its own election timeout
// can simply decline to vote.
func electionTimeout(base time.Duration) time.Duration {
	if base <= 0 {
		base = 150 * time.Millisecond
	}

	return base + time.Duration(rand.Int63n(int64(base))) // #nosec G404 -- timer spread, not a secret
}

func (ce *ConsensusEngine) getLastLogIndex() uint64 {
	if len(ce.log) == 0 {
		return 0
	}
	return ce.log[len(ce.log)-1].Index
}

func (ce *ConsensusEngine) getLastLogTerm() uint64 {
	if len(ce.log) == 0 {
		return 0
	}
	return ce.log[len(ce.log)-1].Term
}

func (ce *ConsensusEngine) updateStats(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ce.stopCh:
			return
		case <-ticker.C:
			ce.stats.mu.Lock()
			ce.stats.LogLength = len(ce.log)
			ce.stats.CommitIndex = ce.commitIndex
			ce.stats.LastApplied = ce.lastApplied
			ce.stats.Uptime = time.Since(startTime)
			ce.stats.mu.Unlock()
		}
	}
}

// GetStats returns consensus engine statistics
func (ce *ConsensusEngine) GetStats() *ConsensusStats {
	ce.mu.RLock()
	state := ce.state.String()
	term := ce.currentTerm
	commitIndex := ce.commitIndex
	lastApplied := ce.lastApplied
	logLength := len(ce.log)
	ce.mu.RUnlock()

	ce.stats.mu.RLock()
	stats := &ConsensusStats{
		CurrentState:     state,
		CurrentTerm:      term,
		CurrentLeader:    ce.stats.CurrentLeader,
		LogLength:        logLength,
		CommitIndex:      commitIndex,
		LastApplied:      lastApplied,
		ElectionsStarted: ce.stats.ElectionsStarted,
		ElectionsWon:     ce.stats.ElectionsWon,
		VotesCast:        ce.stats.VotesCast,
		LogEntriesAdded:  ce.stats.LogEntriesAdded,
		HeartbeatsSent:   ce.stats.HeartbeatsSent,
		LastElection:     ce.stats.LastElection,
		Uptime:           ce.stats.Uptime,
	}
	ce.stats.mu.RUnlock()

	return stats
}

// GetCurrentState returns the current consensus state
func (ce *ConsensusEngine) GetCurrentState() ConsensusState {
	ce.mu.RLock()
	defer ce.mu.RUnlock()
	return ce.state
}

// GetCurrentTerm returns the current term
func (ce *ConsensusEngine) GetCurrentTerm() uint64 {
	ce.mu.RLock()
	defer ce.mu.RUnlock()
	return ce.currentTerm
}

// IsLeader returns true if this node is the current leader
func (ce *ConsensusEngine) IsLeader() bool {
	ce.mu.RLock()
	defer ce.mu.RUnlock()
	return ce.state == StateLeader
}
