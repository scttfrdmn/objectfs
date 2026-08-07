package distributed

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

// TestNewConsensusEngine verifies construction succeeds and fields are
// initialized correctly.
func TestNewConsensusEngine(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "ce-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	ce := cm.consensus
	if ce == nil {
		t.Fatal("consensus engine is nil")
	}
}

// TestConsensusEngine_InitialState_IsFollower verifies that a freshly created
// engine starts as a follower.
func TestConsensusEngine_InitialState_IsFollower(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "follower-node"))
	ce := cm.consensus

	if got := ce.GetCurrentState(); got != StateFollower {
		t.Errorf("initial state = %v, want StateFollower", got)
	}
}

// TestConsensusEngine_InitialTerm_IsZero verifies that the initial term is 0.
func TestConsensusEngine_InitialTerm_IsZero(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "term-node"))
	ce := cm.consensus

	if got := ce.GetCurrentTerm(); got != 0 {
		t.Errorf("initial term = %d, want 0", got)
	}
}

// TestConsensusEngine_InitialLog_HasNoopEntry verifies that the log is
// pre-seeded with exactly one no-op entry.
func TestConsensusEngine_InitialLog_HasNoopEntry(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "log-node"))
	ce := cm.consensus

	ce.mu.RLock()
	logLen := len(ce.log)
	firstType := ce.log[0].Type
	ce.mu.RUnlock()

	if logLen != 1 {
		t.Errorf("initial log length = %d, want 1", logLen)
	}
	if firstType != EntryTypeNoop {
		t.Errorf("initial log[0].Type = %q, want %q", firstType, EntryTypeNoop)
	}
}

// TestConsensusEngine_IsLeader_InitiallyFalse verifies IsLeader is false before
// any election.
func TestConsensusEngine_IsLeader_InitiallyFalse(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "not-leader"))
	if cm.consensus.IsLeader() {
		t.Error("IsLeader() should be false before any election")
	}
}

// TestConsensusState_String verifies the String() method for all states.
func TestConsensusState_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state ConsensusState
		want  string
	}{
		{StateFollower, "follower"},
		{StateCandidate, "candidate"},
		{StateLeader, "leader"},
		{ConsensusState(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("ConsensusState(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// TestConsensusEngine_GetStats_InitialValues verifies the shape and initial
// values returned by GetStats.
func TestConsensusEngine_GetStats_InitialValues(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "stats-ce"))
	stats := cm.consensus.GetStats()

	if stats == nil {
		t.Fatal("GetStats() returned nil")
	}
	if stats.CurrentState != "follower" {
		t.Errorf("CurrentState = %q, want %q", stats.CurrentState, "follower")
	}
	if stats.CurrentTerm != 0 {
		t.Errorf("CurrentTerm = %d, want 0", stats.CurrentTerm)
	}
	// Log has the initial noop entry.
	if stats.LogLength != 1 {
		t.Errorf("LogLength = %d, want 1", stats.LogLength)
	}
	if stats.ElectionsStarted != 0 {
		t.Errorf("ElectionsStarted = %d, want 0", stats.ElectionsStarted)
	}
}

// TestConsensusEngine_TriggerElection_BecomesCandidate verifies that calling
// TriggerElection transitions the state to Candidate (at a minimum) when no
// peer votes are available to complete the election.
func TestConsensusEngine_TriggerElection_BecomesCandidate(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "cand-node"))
	ce := cm.consensus

	if err := ce.TriggerElection(context.Background()); err != nil {
		t.Fatalf("TriggerElection: %v", err)
	}

	// With no other nodes, the engine transitions at least to Candidate.
	state := ce.GetCurrentState()
	if state != StateCandidate && state != StateLeader {
		t.Errorf("state = %v after TriggerElection, want Candidate or Leader", state)
	}
	if ce.GetCurrentTerm() != 1 {
		t.Errorf("term = %d, want 1 after first election", ce.GetCurrentTerm())
	}

	stats := ce.GetStats()
	if stats.ElectionsStarted != 1 {
		t.Errorf("ElectionsStarted = %d, want 1", stats.ElectionsStarted)
	}
}

// ── Single-node leadership (#275) ─────────────────────────────────────────────

// TestConsensusEngine_SingleNodeElectsItself verifies that a cluster of one becomes its own leader.
//
// It asserts on the state after TriggerElection returns, with no sleep and no polling, because there
// is nothing to wait for: the majority of a one-node cluster is one vote, and the candidate casts it
// itself before TriggerElection returns. Anything that needed a peer round-trip would not be this
// test.
//
// Until v0.11.0 the majority comparison lived only in handleVoteResponse, which is reached only when a
// peer replies. With no peer it was never evaluated, so a single-node cluster cycled candidate →
// election timeout → candidate forever, incrementing its term, and IsLeader stayed false for the life
// of the process (#275). TestConsensusEngine_TriggerElection_BecomesCandidate above accepted either
// Candidate or Leader, which is why it passed throughout.
func TestConsensusEngine_SingleNodeElectsItself(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "solo")
	ce := cm.consensus

	if err := ce.TriggerElection(t.Context()); err != nil {
		t.Fatalf("TriggerElection: %v", err)
	}

	if got := ce.GetCurrentState(); got != StateLeader {
		t.Errorf("state = %v after an election in a one-node cluster, want StateLeader", got)
	}
	if !ce.IsLeader() {
		t.Error("ConsensusEngine.IsLeader() = false, want true")
	}
	// The cluster manager has to agree: it is what a caller asks, and what the coordinator routes on.
	if !cm.IsLeader() {
		t.Error("ClusterManager.IsLeader() = false, want true")
	}
	if got := cm.GetLeader(); got != "solo" {
		t.Errorf("GetLeader() = %q, want %q", got, "solo")
	}
	if won := ce.GetStats().ElectionsWon; won != 1 {
		t.Errorf("ElectionsWon = %d, want 1", won)
	}
}

// TestConsensusEngine_SingleNodeLeadershipIsStable verifies that the elected leader stays elected and
// its term stops climbing.
//
// The visible symptom of #275 was not only that IsLeader was false: electionLoop re-entered
// startElection on every timeout, so currentTerm climbed without bound. A test that checked the state
// once, immediately, would pass on a fix that elected the node and then let the timer unseat it. The
// election timeout here is deliberately short so several would have fired.
func TestConsensusEngine_SingleNodeLeadershipIsStable(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, "solo-stable")
	cfg.ElectionTimeout = 50 * time.Millisecond
	cfg.HeartbeatInterval = 20 * time.Millisecond

	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	if err := cm.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = cm.Stop() }()

	// No TriggerElection: the election has to happen on its own, driven by electionLoop, which is how it
	// happens in a real deployment. Several timeouts' worth of margin, since resetElectionTimer
	// randomizes up to 2× ElectionTimeout.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !cm.consensus.IsLeader() {
		time.Sleep(10 * time.Millisecond)
	}
	if !cm.consensus.IsLeader() {
		t.Fatalf("no leader after 2s in a one-node cluster: state = %v, term = %d",
			cm.consensus.GetCurrentState(), cm.consensus.GetCurrentTerm())
	}

	termAtElection := cm.consensus.GetCurrentTerm()

	// Long enough for a handful of election timeouts to fire against a leader that should ignore them.
	time.Sleep(300 * time.Millisecond)

	if !cm.consensus.IsLeader() {
		t.Errorf("leadership was lost: state = %v", cm.consensus.GetCurrentState())
	}
	if got := cm.consensus.GetCurrentTerm(); got != termAtElection {
		t.Errorf("term climbed from %d to %d while leader; a leader must not start elections",
			termAtElection, got)
	}
}

// TestCheckVoteMajority_IgnoresANonCandidate verifies that the extracted check does nothing unless this
// node is a candidate.
//
// startElection is not the only caller, and neither is handleVoteResponse the only path to a state
// change: a heartbeat from a higher term makes this node a follower. Without the guard, a vote arriving
// after that would promote a follower to leader on a stale count — the reason the state check was
// inside handleVoteResponse before the extraction, and the thing an extraction is most likely to drop.
func TestCheckVoteMajority_IgnoresANonCandidate(t *testing.T) {
	t.Parallel()

	cm := makeClusterWithNode(t, "not-a-candidate")
	ce := cm.consensus

	ce.mu.Lock()
	ce.state = StateFollower
	ce.voteCount = 99 // far past any majority
	ce.checkVoteMajority()
	state := ce.state
	ce.mu.Unlock()

	if state != StateFollower {
		t.Errorf("state = %v, want StateFollower: a follower must not be promoted by a vote count", state)
	}
	if ce.IsLeader() {
		t.Error("IsLeader() = true, want false")
	}
}

// TestCheckVoteMajority_WaitsForSeedsBeforeSelfElecting is the regression test for the split brain that
// the single-node fix would otherwise introduce.
//
// Evaluating the majority in startElection is correct for a cluster of one and wrong for a cluster of
// three that has not finished discovering itself: gossip learns of a peer from an inbound message, and
// the election timer does not wait for it, so all three nodes hold a membership view of just themselves
// for the first few hundred milliseconds. A probe of three seeded nodes confirmed the consequence
// directly — each elected itself at term 1 on one vote, before any of them had heard of the others.
//
// The discriminator is configuration, not timing: a node that names seeds is joining a cluster and must
// hear from a peer first; a node that names none is declaring itself the whole of one. This test sets
// the seed and asserts the candidate stays a candidate.
func TestCheckVoteMajority_WaitsForSeedsBeforeSelfElecting(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, "joiner")
	cfg.AdvertiseAddr = "127.0.0.1:19301"
	cfg.SeedNodes = []string{"127.0.0.1:19300"} // a peer that this test never starts

	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.UpdateNodeInfo("joiner", nodeAlive("joiner"))
	ce := cm.consensus

	if err := ce.TriggerElection(t.Context()); err != nil {
		t.Fatalf("TriggerElection: %v", err)
	}

	if got := ce.GetCurrentState(); got != StateCandidate {
		t.Errorf("state = %v, want StateCandidate: a node that has not yet discovered its seed nodes "+
			"must not decide it is a majority of one", got)
	}
	if ce.IsLeader() {
		t.Error("ConsensusEngine.IsLeader() = true, want false")
	}
	if cm.IsLeader() {
		t.Error("ClusterManager.IsLeader() = true, want false")
	}
}

// TestExpectsPeers verifies the bootstrap-versus-join discriminator that checkVoteMajority turns on.
//
// The third case is the one worth having: a seed list naming only this node's own address is what a
// uniform configuration file deployed to every node produces, and joinCluster already skips it for
// exactly that reason. Reading it as "has peers" would stop a single-node deployment from ever electing
// a leader — reintroducing #275 through the fix for its own regression.
func TestExpectsPeers(t *testing.T) {
	t.Parallel()

	const self = "127.0.0.1:7946"
	cases := []struct {
		name  string
		seeds []string
		want  bool
	}{
		{"no seeds is a cluster of one", nil, false},
		{"an empty seed list is a cluster of one", []string{}, false},
		{"only ourselves is a cluster of one", []string{self}, false},
		{"a peer is a cluster to join", []string{"127.0.0.1:7947"}, true},
		{"ourselves and a peer is a cluster to join", []string{self, "127.0.0.1:7947"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := testConfig(t, "peers")
			cfg.AdvertiseAddr = self
			cfg.SeedNodes = tc.seeds

			cm, err := NewClusterManager(cfg)
			if err != nil {
				t.Fatalf("NewClusterManager: %v", err)
			}
			if got := cm.expectsPeers(); got != tc.want {
				t.Errorf("expectsPeers() = %v, want %v for seeds %v", got, tc.want, tc.seeds)
			}
		})
	}
}

// TestStepDown_ClearsClusterLeadership is the regression test for the half of the split brain that made
// it permanent.
//
// becomeLeader calls ClusterManager.SetLeader; until v0.11.0 no step-down path did. So cm.isLeader was
// effectively write-once: a node that had been leader and then saw a higher term became a follower in
// ce.state while ClusterManager.IsLeader — which is what callers ask and what the coordinator routes on
// — kept returning true for the life of the process. A transient election race therefore became a
// durable two-leader state instead of resolving on the next heartbeat.
//
// Both step-down paths are covered, because they are separate code and only one of them has a new leader
// to name. Driving the handlers directly rather than over UDP is what makes this deterministic; the
// sender is not a known node, so the response send is skipped and no socket is needed.
func TestStepDown_ClearsClusterLeadership(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		msg        func(t *testing.T) (*GossipMessage, func(*ConsensusEngine, *GossipMessage))
		wantLeader string
	}{
		{
			name: "a heartbeat from a higher term names the new leader",
			msg: func(t *testing.T) (*GossipMessage, func(*ConsensusEngine, *GossipMessage)) {
				t.Helper()
				body, err := json.Marshal(&AppendEntriesMessage{Term: 99, LeaderID: "real-leader"})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				return &GossipMessage{Type: MessageTypeAppendEntries, From: "real-leader", Data: body},
					(*ConsensusEngine).handleNetworkAppendEntries
			},
			wantLeader: "real-leader",
		},
		{
			name: "a vote request at a higher term clears leadership without naming one",
			msg: func(t *testing.T) (*GossipMessage, func(*ConsensusEngine, *GossipMessage)) {
				t.Helper()
				body, err := json.Marshal(&RequestVoteMessage{Term: 99, CandidateID: "challenger"})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				return &GossipMessage{Type: MessageTypeRequestVote, From: "challenger", Data: body},
					(*ConsensusEngine).handleNetworkRequestVote
			},
			// A candidate has not won anything, so there is no leader to name — and claiming the
			// challenger were one would be worse than admitting there is none.
			wantLeader: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := testConfig(t, "incumbent")
			cfg.ElectionTimeout = time.Hour // no election fires on its own during this test
			cm, err := NewClusterManager(cfg)
			if err != nil {
				t.Fatalf("NewClusterManager: %v", err)
			}
			cm.UpdateNodeInfo("incumbent", nodeAlive("incumbent"))
			ce := cm.consensus

			if err := ce.TriggerElection(t.Context()); err != nil {
				t.Fatalf("TriggerElection: %v", err)
			}
			if !cm.IsLeader() {
				t.Fatalf("precondition: not leader before the step-down, state = %v", ce.GetCurrentState())
			}

			msg, handle := tc.msg(t)
			handle(ce, msg)

			if got := ce.GetCurrentState(); got != StateFollower {
				t.Errorf("ce.state = %v after a message at a higher term, want StateFollower", got)
			}
			if cm.IsLeader() {
				t.Error("ClusterManager.IsLeader() = true after stepping down; the consensus engine and " +
					"the cluster manager must agree on who is leader")
			}
			if got := cm.GetLeader(); got != tc.wantLeader {
				t.Errorf("GetLeader() = %q, want %q", got, tc.wantLeader)
			}
		})
	}
}

// TestConsensusEngine_TriggerElection_WithPeer_BecomesLeader verifies that when
// two ClusterManagers are connected over loopback UDP, triggering an election
// causes the candidate to receive a real vote and become the leader.
func TestConsensusEngine_TriggerElection_WithPeer_BecomesLeader(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	cfg1 := testConfig(t, "node-a")
	cfg2 := testConfig(t, "node-b")

	cm1, err := NewClusterManager(cfg1)
	if err != nil {
		t.Fatalf("NewClusterManager cm1: %v", err)
	}
	cm2, err := NewClusterManager(cfg2)
	if err != nil {
		t.Fatalf("NewClusterManager cm2: %v", err)
	}

	if err := cm1.Start(ctx); err != nil {
		t.Fatalf("cm1.Start: %v", err)
	}
	defer func() { _ = cm1.Stop() }()

	if err := cm2.Start(ctx); err != nil {
		t.Fatalf("cm2.Start: %v", err)
	}
	defer func() { _ = cm2.Stop() }()

	// Cross-register using the actual bound UDP addresses.
	addr1 := cm1.gossip.LocalAddr()
	addr2 := cm2.gossip.LocalAddr()
	if addr1 == "" || addr2 == "" {
		t.Fatalf("could not get local addresses: %q %q", addr1, addr2)
	}

	cm1.UpdateNodeInfo("node-b", &NodeInfo{
		ID: "node-b", Address: addr2, Status: NodeStatusAlive, Metadata: map[string]string{},
	})
	cm2.UpdateNodeInfo("node-a", &NodeInfo{
		ID: "node-a", Address: addr1, Status: NodeStatusAlive, Metadata: map[string]string{},
	})

	ce := cm1.consensus
	if err := ce.TriggerElection(ctx); err != nil {
		t.Fatalf("TriggerElection: %v", err)
	}

	// Allow up to 2 s for real loopback UDP round-trip.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ce.GetCurrentState() == StateLeader {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if ce.GetCurrentState() != StateLeader {
		t.Errorf("state = %v after election with peer, want StateLeader", ce.GetCurrentState())
	}
	if !ce.IsLeader() {
		t.Error("IsLeader() should be true after winning election")
	}
}

// TestConsensusEngine_TriggerElection_WhenLeader_IsNoOp verifies that calling
// TriggerElection when already the leader does nothing (term and state unchanged).
// Uses two real ClusterManagers over loopback UDP to first win an election.
func TestConsensusEngine_TriggerElection_WhenLeader_IsNoOp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	cfg1 := testConfig(t, "leader-noop-a")
	cfg2 := testConfig(t, "leader-noop-b")

	cm1, err := NewClusterManager(cfg1)
	if err != nil {
		t.Fatalf("NewClusterManager cm1: %v", err)
	}
	cm2, err := NewClusterManager(cfg2)
	if err != nil {
		t.Fatalf("NewClusterManager cm2: %v", err)
	}

	if err := cm1.Start(ctx); err != nil {
		t.Fatalf("cm1.Start: %v", err)
	}
	defer func() { _ = cm1.Stop() }()

	if err := cm2.Start(ctx); err != nil {
		t.Fatalf("cm2.Start: %v", err)
	}
	defer func() { _ = cm2.Stop() }()

	addr1 := cm1.gossip.LocalAddr()
	addr2 := cm2.gossip.LocalAddr()
	cm1.UpdateNodeInfo("leader-noop-b", &NodeInfo{
		ID: "leader-noop-b", Address: addr2, Status: NodeStatusAlive, Metadata: map[string]string{},
	})
	cm2.UpdateNodeInfo("leader-noop-a", &NodeInfo{
		ID: "leader-noop-a", Address: addr1, Status: NodeStatusAlive, Metadata: map[string]string{},
	})

	ce := cm1.consensus
	_ = ce.TriggerElection(ctx)

	// Wait for cm1 to win the election.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ce.GetCurrentState() == StateLeader {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ce.GetCurrentState() != StateLeader {
		t.Skip("could not become leader; skipping no-op test")
	}

	termBefore := ce.GetCurrentTerm()
	_ = ce.TriggerElection(ctx) // should be a no-op
	time.Sleep(150 * time.Millisecond)

	if ce.GetCurrentState() != StateLeader {
		t.Error("state should remain Leader after no-op TriggerElection")
	}
	if ce.GetCurrentTerm() != termBefore {
		t.Errorf("term changed from %d to %d; should not have changed", termBefore, ce.GetCurrentTerm())
	}
}

// TestConsensusEngine_ProposeChange_NotLeader_ReturnsError verifies that only
// the leader can propose changes.
func TestConsensusEngine_ProposeChange_NotLeader_ReturnsError(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "not-leader-prop"))
	ce := cm.consensus

	err := ce.ProposeChange(context.Background(), &ConsensusProposal{
		Type: ProposalTypeOperation,
		Data: []byte("noop"),
	})
	if err == nil {
		t.Fatal("ProposeChange as follower should return error")
	}
}

// TestConsensusEngine_StartStop verifies the lifecycle without panics.
func TestConsensusEngine_StartStop(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "lifecycle-ce")
	cfg.ElectionTimeout = 30 * time.Second // prevent elections during the test
	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	ctx := t.Context()

	if err := cm.consensus.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cm.consensus.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestConsensusEngine_StopRacesAnInboundHeartbeat is the regression test for the data race CI caught
// in TriggerElection's two peer tests.
//
// Stop read ce.electionTimer without holding ce.mu while resetElectionTimer replaces it — reached from
// handleNetworkAppendEntries, which runs on the gossip receiver goroutine. Closing stopCh does not
// order the two: the receiver belongs to GossipProtocol, which ClusterManager.Stop shuts down *after*
// the consensus engine, so a heartbeat in flight is still being handled while Stop runs.
//
// Driving handleNetworkAppendEntries directly rather than over UDP is what makes this deterministic.
// The peer tests hit it only when a heartbeat happened to land inside Stop's window, which is why it
// showed up under CI's load and not locally; here the write is guaranteed to be concurrent with the
// read, so -race reports it on every run without the fix.
func TestConsensusEngine_StopRacesAnInboundHeartbeat(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, "stop-race")
	cfg.ElectionTimeout = time.Hour // no election fires on its own during this test
	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	ce := cm.consensus

	if err := ce.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A heartbeat from a leader at a term above ours, which is the arm that resets the timer. The
	// sender is not a known node, so the response send is skipped and no socket is needed.
	body, err := json.Marshal(&AppendEntriesMessage{Term: 99, LeaderID: "leader"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg := &GossipMessage{Type: MessageTypeAppendEntries, From: "leader", Data: body}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			ce.handleNetworkAppendEntries(msg)
		}
	}()

	if err := ce.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	<-done
}

// TestElectionTimeout_JitterIsAFractionOfTheBase is the regression test for a jitter that was six
// orders of magnitude too small to do its job, and for a panic below one millisecond.
//
// The old expression mixed units — `base + rand.Intn(int(base.Milliseconds()))`, a millisecond count
// used as a nanosecond Duration. Against the 5s default that produced spreads of 4.69µs, 4.16µs and
// 690ns: every follower in a cluster whose timers expired together would still become a candidate
// within microseconds of every other one, which is the split vote randomized timeouts exist to
// prevent. Below 1ms, Milliseconds() truncated to 0 and rand.Intn(0) panicked.
//
// The assertions are on the *distribution*, not on any single draw, because a correct implementation
// is random: a fixed expected value could only be written by reproducing the formula, which would
// pass for the broken one too. So this asserts the range every draw must satisfy, and that the draws
// actually spread across it — the property the old code violated while satisfying the range.
func TestElectionTimeout_JitterIsAFractionOfTheBase(t *testing.T) {
	t.Parallel()

	for _, base := range []time.Duration{
		500 * time.Microsecond, // panicked before: Milliseconds() == 0
		time.Millisecond,
		5 * time.Second, // the NewClusterManager default
		0,               // the unconfigured arm, which uses 150ms
	} {
		want := base
		if want <= 0 {
			want = 150 * time.Millisecond
		}

		var lo, hi time.Duration = math.MaxInt64, 0
		for range 200 {
			got := electionTimeout(base)
			if got < want || got >= 2*want {
				t.Fatalf("electionTimeout(%v) = %v, want [%v, %v)", base, got, want, 2*want)
			}
			lo = min(lo, got)
			hi = max(hi, got)
		}

		// Half the base, spread over 200 draws from a range of exactly the base: the probability of a
		// correct implementation failing this is negligible, while the old one's ~1e-6 relative spread
		// misses it by six orders of magnitude.
		if spread := hi - lo; spread < want/2 {
			t.Errorf("electionTimeout(%v) spread over 200 draws = %v, want at least %v; "+
				"draws ranged [%v, %v] — a jitter this narrow does not separate candidates",
				base, spread, want/2, lo, hi)
		}
	}
}
