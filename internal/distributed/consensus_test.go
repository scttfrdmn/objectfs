package distributed

import (
	"context"
	"testing"
	"time"
)

// TestNewConsensusEngine verifies construction succeeds and fields are
// initialised correctly.
func TestNewConsensusEngine(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("ce-node"))
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
	cm, _ := NewClusterManager(testConfig("follower-node"))
	ce := cm.consensus

	if got := ce.GetCurrentState(); got != StateFollower {
		t.Errorf("initial state = %v, want StateFollower", got)
	}
}

// TestConsensusEngine_InitialTerm_IsZero verifies that the initial term is 0.
func TestConsensusEngine_InitialTerm_IsZero(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig("term-node"))
	ce := cm.consensus

	if got := ce.GetCurrentTerm(); got != 0 {
		t.Errorf("initial term = %d, want 0", got)
	}
}

// TestConsensusEngine_InitialLog_HasNoopEntry verifies that the log is
// pre-seeded with exactly one no-op entry.
func TestConsensusEngine_InitialLog_HasNoopEntry(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig("log-node"))
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
	cm, _ := NewClusterManager(testConfig("not-leader"))
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
	cm, _ := NewClusterManager(testConfig("stats-ce"))
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
	cm, _ := NewClusterManager(testConfig("cand-node"))
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

// TestConsensusEngine_TriggerElection_WithPeer_BecomesLeader verifies that when
// a peer node is present the simulated vote response makes the engine become
// the leader (within 200 ms).
func TestConsensusEngine_TriggerElection_WithPeer_BecomesLeader(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("leader-elect"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	// Register self and one peer so majority = 2 can be reached.
	cm.UpdateNodeInfo("leader-elect", nodeAlive("leader-elect"))
	cm.UpdateNodeInfo("peer-node", nodeAlive("peer-node"))
	ce := cm.consensus

	if err := ce.TriggerElection(context.Background()); err != nil {
		t.Fatalf("TriggerElection: %v", err)
	}

	// The simulated vote response fires after 50 ms; give it 300 ms.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ce.GetCurrentState() == StateLeader {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if ce.GetCurrentState() != StateLeader {
		t.Errorf("state = %v after election with peer, want StateLeader", ce.GetCurrentState())
	}
	if !ce.IsLeader() {
		t.Error("IsLeader() should be true after winning election")
	}
}

// TestConsensusEngine_TriggerElection_WhenLeader_IsNoOp verifies that calling
// TriggerElection when already the leader does nothing.
func TestConsensusEngine_TriggerElection_WhenLeader_IsNoOp(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig("already-leader"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.UpdateNodeInfo("already-leader", nodeAlive("already-leader"))
	cm.UpdateNodeInfo("peer", nodeAlive("peer"))
	ce := cm.consensus

	// Win one election first.
	_ = ce.TriggerElection(context.Background())
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ce.GetCurrentState() == StateLeader {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ce.GetCurrentState() != StateLeader {
		t.Skip("could not become leader; skipping no-op test")
	}

	termBefore := ce.GetCurrentTerm()
	_ = ce.TriggerElection(context.Background()) // should be no-op
	time.Sleep(100 * time.Millisecond)

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
	cm, _ := NewClusterManager(testConfig("not-leader-prop"))
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
	cfg := testConfig("lifecycle-ce")
	cfg.ElectionTimeout = 30 * time.Second // prevent elections during the test
	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cm.consensus.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cm.consensus.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
