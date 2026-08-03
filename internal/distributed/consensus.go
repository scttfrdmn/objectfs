package distributed

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

// ConsensusEngine implements a Raft-like consensus algorithm for leader election
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

	// Proposal state
	proposals map[string]*ConsensusProposal

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

// EntryType represents the type of log entry
type EntryType string

const (
	EntryTypeNoop           EntryType = "noop"
	EntryTypeLeaderElection EntryType = "leader_election"
	EntryTypeConfigChange   EntryType = "config_change"
	EntryTypeOperation      EntryType = "operation"
	EntryTypeSnapshot       EntryType = "snapshot"
)

// ConsensusProposal represents a proposal for distributed consensus
type ConsensusProposal struct {
	ID        string          `json:"id"`
	Type      ProposalType    `json:"type"`
	Data      []byte          `json:"data"`
	Proposer  string          `json:"proposer"`
	Timestamp time.Time       `json:"timestamp"`
	Status    ProposalStatus  `json:"status"`
	Votes     map[string]bool `json:"votes"`
	Result    []byte          `json:"result,omitempty"`
}

// ProposalType represents the type of consensus proposal
type ProposalType string

const (
	ProposalTypeLeadershipChange ProposalType = "leadership_change"
	ProposalTypeConfigChange     ProposalType = "config_change"
	ProposalTypeOperation        ProposalType = "operation"
)

// ProposalStatus represents the status of a consensus proposal
type ProposalStatus string

const (
	ProposalStatusPending  ProposalStatus = "pending"
	ProposalStatusAccepted ProposalStatus = "accepted"
	ProposalStatusRejected ProposalStatus = "rejected"
	ProposalStatusExpired  ProposalStatus = "expired"
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
	mu                sync.RWMutex
	CurrentState      string        `json:"current_state"`
	CurrentTerm       uint64        `json:"current_term"`
	CurrentLeader     string        `json:"current_leader"`
	LogLength         int           `json:"log_length"`
	CommitIndex       uint64        `json:"commit_index"`
	LastApplied       uint64        `json:"last_applied"`
	ElectionsStarted  int64         `json:"elections_started"`
	ElectionsWon      int64         `json:"elections_won"`
	VotesCast         int64         `json:"votes_cast"`
	ProposalsReceived int64         `json:"proposals_received"`
	ProposalsAccepted int64         `json:"proposals_accepted"`
	LogEntriesAdded   int64         `json:"log_entries_added"`
	HeartbeatsSent    int64         `json:"heartbeats_sent"`
	LastElection      time.Time     `json:"last_election"`
	Uptime            time.Duration `json:"uptime"`
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
		proposals:   make(map[string]*ConsensusProposal),
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
	go ce.proposalCleanup(ctx)
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

// ProposeChange proposes a change to the cluster
func (ce *ConsensusEngine) ProposeChange(ctx context.Context, proposal *ConsensusProposal) error {
	ce.mu.Lock()
	defer ce.mu.Unlock()

	if ce.state != StateLeader {
		return fmt.Errorf("only leader can propose changes")
	}

	// Generate proposal ID if not provided
	if proposal.ID == "" {
		proposalBytes := make([]byte, 8)
		_, _ = cryptorand.Read(proposalBytes)
		proposal.ID = "prop-" + hex.EncodeToString(proposalBytes)
	}

	proposal.Status = ProposalStatusPending
	proposal.Votes = make(map[string]bool)
	proposal.Timestamp = time.Now()

	ce.proposals[proposal.ID] = proposal

	// Broadcast proposal to all nodes
	ce.broadcastProposal(proposal)

	ce.stats.mu.Lock()
	ce.stats.ProposalsReceived++
	ce.stats.mu.Unlock()

	slog.Info("proposed change", "proposal_id", proposal.ID, "type", proposal.Type)
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

func (ce *ConsensusEngine) proposalCleanup(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ce.stopCh:
			return
		case <-ticker.C:
			ce.cleanupExpiredProposals()
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

	// Check if we have majority
	nodes := ce.cluster.GetNodes()
	aliveNodes := 0
	for _, node := range nodes {
		if node.Status == NodeStatusAlive {
			aliveNodes++
		}
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
	if req.Term > ce.currentTerm {
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

func (ce *ConsensusEngine) applyLogEntry(entry *LogEntry) {
	slog.Info("applying log entry", "index", entry.Index, "type", entry.Type)

	switch entry.Type {
	case EntryTypeLeaderElection:
		// Leader election entry applied
	case EntryTypeConfigChange:
		// Apply configuration change
	case EntryTypeOperation:
		// Apply operation
	}
}

// Proposal handling

func (ce *ConsensusEngine) broadcastProposal(proposal *ConsensusProposal) {
	// In a real implementation, this would broadcast to all nodes
	slog.Info("broadcasting proposal to cluster", "proposal_id", proposal.ID)

	// For simulation, automatically accept our own proposals after a delay
	go func() {
		time.Sleep(100 * time.Millisecond)
		ce.voteOnProposal(proposal.ID, true)
	}()
}

func (ce *ConsensusEngine) voteOnProposal(proposalID string, accept bool) {
	ce.mu.Lock()
	defer ce.mu.Unlock()

	proposal, exists := ce.proposals[proposalID]
	if !exists {
		return
	}

	nodeID := ce.cluster.GetNodeID()
	proposal.Votes[nodeID] = accept

	// Count votes
	acceptVotes := 0
	totalVotes := len(proposal.Votes)

	for _, vote := range proposal.Votes {
		if vote {
			acceptVotes++
		}
	}

	nodes := ce.cluster.GetNodes()
	aliveNodes := 0
	for _, node := range nodes {
		if node.Status == NodeStatusAlive {
			aliveNodes++
		}
	}

	majority := aliveNodes/2 + 1

	// Check if proposal should be accepted or rejected
	if acceptVotes >= majority {
		proposal.Status = ProposalStatusAccepted
		ce.executeProposal(proposal)

		ce.stats.mu.Lock()
		ce.stats.ProposalsAccepted++
		ce.stats.mu.Unlock()

		slog.Info("proposal accepted", "proposal_id", proposalID, "accept_votes", acceptVotes, "total_votes", totalVotes)
	} else if totalVotes-acceptVotes > aliveNodes-majority {
		proposal.Status = ProposalStatusRejected
		slog.Info("proposal rejected", "proposal_id", proposalID, "accept_votes", acceptVotes, "total_votes", totalVotes)
	}
}

func (ce *ConsensusEngine) executeProposal(proposal *ConsensusProposal) {
	switch proposal.Type {
	case ProposalTypeLeadershipChange:
		newLeader := string(proposal.Data)
		slog.Info("executing leadership change", "new_leader", newLeader)
		ce.cluster.SetLeader(newLeader)

	case ProposalTypeConfigChange:
		slog.Info("executing configuration change")
		// Apply configuration change

	case ProposalTypeOperation:
		slog.Info("executing operation proposal")
		// Execute operation
	}
}

// Utility methods

// resetElectionTimer replaces the election timer with a fresh randomized one.
//
// ce.mu must be held for writing. It writes ce.electionTimer, which electionLoop reads under the read
// lock and Stop reads under the write lock; every caller — Start, startElection, and
// handleNetworkAppendEntries — holds it.
func (ce *ConsensusEngine) resetElectionTimer() {
	// Random timeout between 150ms and 300ms (scaled by ElectionTimeout)
	timeoutMs := 150 + (rand.Intn(150))
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if ce.config.ElectionTimeout > 0 {
		timeout = ce.config.ElectionTimeout + time.Duration(rand.Intn(int(ce.config.ElectionTimeout.Milliseconds())))
	}

	// Use time.NewTimer (not time.AfterFunc) so that the .C channel is
	// populated.  time.AfterFunc leaves .C == nil, which caused electionLoop
	// to block on a nil channel and never fire (#101).
	if ce.electionTimer != nil {
		ce.electionTimer.Stop()
	}
	ce.electionTimer = time.NewTimer(timeout)
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

func (ce *ConsensusEngine) cleanupExpiredProposals() {
	ce.mu.Lock()
	defer ce.mu.Unlock()

	now := time.Now()
	for proposalID, proposal := range ce.proposals {
		if proposal.Status == ProposalStatusPending && now.Sub(proposal.Timestamp) > 30*time.Second {
			proposal.Status = ProposalStatusExpired
			delete(ce.proposals, proposalID)
			slog.Info("proposal expired", "proposal_id", proposalID)
		}
	}
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
		CurrentState:      state,
		CurrentTerm:       term,
		CurrentLeader:     ce.stats.CurrentLeader,
		LogLength:         logLength,
		CommitIndex:       commitIndex,
		LastApplied:       lastApplied,
		ElectionsStarted:  ce.stats.ElectionsStarted,
		ElectionsWon:      ce.stats.ElectionsWon,
		VotesCast:         ce.stats.VotesCast,
		ProposalsReceived: ce.stats.ProposalsReceived,
		ProposalsAccepted: ce.stats.ProposalsAccepted,
		LogEntriesAdded:   ce.stats.LogEntriesAdded,
		HeartbeatsSent:    ce.stats.HeartbeatsSent,
		LastElection:      ce.stats.LastElection,
		Uptime:            ce.stats.Uptime,
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
