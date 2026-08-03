package distributed

import (
	"encoding/json"
	"testing"
	"time"
)

// makeGossipMsg marshals payload into a GossipMessage with the given type.
func makeGossipMsg(t *testing.T, msgType MessageType, payload any) *GossipMessage {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return &GossipMessage{
		Type:      msgType,
		From:      "remote-node",
		Timestamp: time.Now(),
		MessageID: "test-msg-id",
		Data:      data,
	}
}

// TestNewGossipProtocol verifies the constructor succeeds and returns a
// non-nil GossipProtocol with the expected initial state.
func TestNewGossipProtocol(t *testing.T) {
	t.Parallel()
	cm, err := NewClusterManager(testConfig(t, "gp-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	gp := cm.gossip
	if gp == nil {
		t.Fatal("gossip is nil")
	}
	if gp.localNode == nil {
		t.Fatal("localNode is nil after construction")
	}
	if gp.localNode.ID != "gp-node" {
		t.Errorf("localNode.ID = %q, want %q", gp.localNode.ID, "gp-node")
	}
	if gp.stats == nil {
		t.Fatal("stats is nil")
	}
	if gp.memberlist == nil {
		t.Fatal("memberlist is nil")
	}
}

// TestGossipProtocol_InitialMemberlist_HasSelf verifies that self is
// pre-seeded in the memberlist upon construction.
func TestGossipProtocol_InitialMemberlist_HasSelf(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "self-node"))
	gp := cm.gossip

	ml := gp.GetMemberlist()
	if _, ok := ml["self-node"]; !ok {
		t.Error("memberlist should contain self after construction")
	}
	if ml["self-node"].State != StateAlive {
		t.Errorf("self state = %v, want StateAlive", ml["self-node"].State)
	}
}

// TestGossipProtocol_InitialStats_Zeroed verifies that all counters in
// GetStats are zero on a fresh GossipProtocol.
func TestGossipProtocol_InitialStats_Zeroed(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "stats-gp"))
	stats := cm.gossip.GetStats()

	if stats == nil {
		t.Fatal("GetStats returned nil")
	}
	if stats.MessagesSent != 0 {
		t.Errorf("MessagesSent = %d, want 0", stats.MessagesSent)
	}
	if stats.MessagesReceived != 0 {
		t.Errorf("MessagesReceived = %d, want 0", stats.MessagesReceived)
	}
	if stats.NodesDiscovered != 0 {
		t.Errorf("NodesDiscovered = %d, want 0", stats.NodesDiscovered)
	}
	if stats.SuspicionEvents != 0 {
		t.Errorf("SuspicionEvents = %d, want 0", stats.SuspicionEvents)
	}
	if stats.DeathEvents != 0 {
		t.Errorf("DeathEvents = %d, want 0", stats.DeathEvents)
	}
}

// TestGossipProtocol_GetStats_MessagesByType_Initialized verifies the
// MessagesByType map is non-nil (not a nil map panic waiting to happen).
func TestGossipProtocol_GetStats_MessagesByType_Initialized(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "mbt-node"))
	stats := cm.gossip.GetStats()

	if stats.MessagesByType == nil {
		t.Error("MessagesByType should be initialized (non-nil map)")
	}
}

// TestGossipProtocol_GetMemberlist_DeepCopy verifies that mutating the map
// returned by GetMemberlist does not affect internal state.
func TestGossipProtocol_GetMemberlist_DeepCopy(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "copy-node"))
	gp := cm.gossip

	copy1 := gp.GetMemberlist()
	// Mutate the copy
	copy1["copy-node"].State = StateDead
	delete(copy1, "copy-node")

	copy2 := gp.GetMemberlist()
	if _, ok := copy2["copy-node"]; !ok {
		t.Error("internal memberlist was affected by deleting copy")
	}
	if copy2["copy-node"].State == StateDead {
		t.Error("internal state was mutated via GetMemberlist copy")
	}
}

// TestGossipProtocol_HandleJoinMessage_AddsNode verifies that a join message
// adds the new node to the memberlist and updates the cluster manager.
func TestGossipProtocol_HandleJoinMessage_AddsNode(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "host-node"))
	gp := cm.gossip

	joiner := &NodeInfo{
		ID:       "joiner-node",
		Address:  "127.0.0.1:9999",
		Status:   NodeStatusAlive,
		Metadata: map[string]string{},
	}
	msg := makeGossipMsg(t, MessageTypeJoin, &JoinMessage{
		Node:        joiner,
		Incarnation: 1,
	})

	gp.handleJoinMessage(msg)

	ml := gp.GetMemberlist()
	if _, ok := ml["joiner-node"]; !ok {
		t.Error("joiner-node not found in memberlist after join")
	}
	if ml["joiner-node"].State != StateAlive {
		t.Errorf("joiner-node state = %v, want StateAlive", ml["joiner-node"].State)
	}

	// Cluster manager should also know about the node.
	nodes := cm.GetNodes()
	if _, ok := nodes["joiner-node"]; !ok {
		t.Error("joiner-node not found in ClusterManager nodes after join")
	}
}

// TestGossipProtocol_HandleJoinMessage_IncrementsStat verifies that
// NodesDiscovered increments when a join message arrives.
func TestGossipProtocol_HandleJoinMessage_IncrementsStat(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "stat-host"))
	gp := cm.gossip

	before := gp.GetStats().NodesDiscovered

	joiner := &NodeInfo{
		ID:       "new-joiner",
		Address:  "127.0.0.1:9998",
		Status:   NodeStatusAlive,
		Metadata: map[string]string{},
	}
	msg := makeGossipMsg(t, MessageTypeJoin, &JoinMessage{Node: joiner, Incarnation: 1})
	gp.handleJoinMessage(msg)

	after := gp.GetStats().NodesDiscovered
	if after != before+1 {
		t.Errorf("NodesDiscovered: got %d, want %d", after, before+1)
	}
}

// TestGossipProtocol_HandleAliveMessage_NewNode verifies that an alive
// message for an unknown node adds it to the memberlist.
func TestGossipProtocol_HandleAliveMessage_NewNode(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "alive-host"))
	gp := cm.gossip

	before := gp.GetStats().NodesDiscovered

	newNode := &NodeInfo{
		ID:       "alive-peer",
		Address:  "127.0.0.1:9997",
		Status:   NodeStatusAlive,
		Metadata: map[string]string{},
	}
	msg := makeGossipMsg(t, MessageTypeAlive, &AliveMessage{Node: newNode, Incarnation: 2})
	gp.handleAliveMessage(msg)

	ml := gp.GetMemberlist()
	if _, ok := ml["alive-peer"]; !ok {
		t.Error("alive-peer not in memberlist after AliveMessage")
	}
	if gp.GetStats().NodesDiscovered != before+1 {
		t.Error("NodesDiscovered should have incremented")
	}
}

// TestGossipProtocol_HandleAliveMessage_UpdatesExisting verifies that an
// alive message with a higher incarnation updates an existing node.
func TestGossipProtocol_HandleAliveMessage_UpdatesExisting(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "update-host"))
	gp := cm.gossip

	// Manually insert an existing node at incarnation 1.
	gp.mu.Lock()
	gp.memberlist["update-peer"] = &GossipNode{
		Info: &NodeInfo{
			ID:       "update-peer",
			Address:  "127.0.0.1:9996",
			Metadata: map[string]string{},
		},
		Incarnation: 1,
		State:       StateSuspect,
	}
	gp.mu.Unlock()

	// Send alive with higher incarnation — should clear suspect.
	updatedNode := &NodeInfo{
		ID:       "update-peer",
		Address:  "127.0.0.1:9996",
		Status:   NodeStatusAlive,
		Metadata: map[string]string{},
	}
	msg := makeGossipMsg(t, MessageTypeAlive, &AliveMessage{Node: updatedNode, Incarnation: 5})
	gp.handleAliveMessage(msg)

	ml := gp.GetMemberlist()
	if ml["update-peer"].State != StateAlive {
		t.Errorf("state = %v after higher-incarnation alive, want StateAlive", ml["update-peer"].State)
	}
	if ml["update-peer"].Incarnation != 5 {
		t.Errorf("incarnation = %d, want 5", ml["update-peer"].Incarnation)
	}
}

// TestGossipProtocol_HandleSuspectMessage_MarksSuspect verifies that a
// suspect message transitions an alive node to StateSuspect.
func TestGossipProtocol_HandleSuspectMessage_MarksSuspect(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "suspect-host"))
	gp := cm.gossip

	// Insert a live node at incarnation 1.
	gp.mu.Lock()
	gp.memberlist["suspect-peer"] = &GossipNode{
		Info:        &NodeInfo{ID: "suspect-peer", Status: NodeStatusAlive, Metadata: map[string]string{}},
		Incarnation: 1,
		State:       StateAlive,
	}
	gp.mu.Unlock()

	msg := makeGossipMsg(t, MessageTypeSuspect, &SuspectMessage{
		Node:        "suspect-peer",
		Incarnation: 1,
		From:        "some-reporter",
	})
	gp.handleSuspectMessage(msg)

	ml := gp.GetMemberlist()
	if ml["suspect-peer"].State != StateSuspect {
		t.Errorf("state = %v, want StateSuspect", ml["suspect-peer"].State)
	}
	if ml["suspect-peer"].Suspicion == nil {
		t.Error("Suspicion should be set after suspect message")
	}
	if gp.GetStats().SuspicionEvents != 1 {
		t.Errorf("SuspicionEvents = %d, want 1", gp.GetStats().SuspicionEvents)
	}
}

// TestGossipProtocol_HandleSuspectMessage_WrongIncarnation verifies that a
// suspect message with a non-matching incarnation is ignored.
func TestGossipProtocol_HandleSuspectMessage_WrongIncarnation(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "wrong-inc"))
	gp := cm.gossip

	gp.mu.Lock()
	gp.memberlist["inc-peer"] = &GossipNode{
		Info:        &NodeInfo{ID: "inc-peer", Metadata: map[string]string{}},
		Incarnation: 3,
		State:       StateAlive,
	}
	gp.mu.Unlock()

	msg := makeGossipMsg(t, MessageTypeSuspect, &SuspectMessage{
		Node:        "inc-peer",
		Incarnation: 1, // stale incarnation
		From:        "reporter",
	})
	gp.handleSuspectMessage(msg)

	ml := gp.GetMemberlist()
	if ml["inc-peer"].State != StateAlive {
		t.Errorf("state should remain StateAlive on stale incarnation, got %v", ml["inc-peer"].State)
	}
}

// TestGossipProtocol_HandleSuspectMessage_MultipleReporters verifies that a
// second suspect message from a different node is appended to Suspicion.From.
func TestGossipProtocol_HandleSuspectMessage_MultipleReporters(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "multi-reporter"))
	gp := cm.gossip

	gp.mu.Lock()
	gp.memberlist["multi-peer"] = &GossipNode{
		Info:        &NodeInfo{ID: "multi-peer", Metadata: map[string]string{}},
		Incarnation: 1,
		State:       StateAlive,
	}
	gp.mu.Unlock()

	// First suspicion
	msg1 := makeGossipMsg(t, MessageTypeSuspect, &SuspectMessage{Node: "multi-peer", Incarnation: 1, From: "reporter-a"})
	gp.handleSuspectMessage(msg1)

	// Second suspicion from a different reporter
	msg2 := makeGossipMsg(t, MessageTypeSuspect, &SuspectMessage{Node: "multi-peer", Incarnation: 1, From: "reporter-b"})
	gp.handleSuspectMessage(msg2)

	ml := gp.GetMemberlist()
	if ml["multi-peer"].Suspicion == nil {
		t.Fatal("Suspicion should be set")
	}
	if len(ml["multi-peer"].Suspicion.From) != 2 {
		t.Errorf("Suspicion.From has %d entries, want 2", len(ml["multi-peer"].Suspicion.From))
	}
}

// TestGossipProtocol_HandleDeadMessage_MarksDead verifies that a dead message
// transitions a node to StateDead.
func TestGossipProtocol_HandleDeadMessage_MarksDead(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "dead-host"))
	gp := cm.gossip

	gp.mu.Lock()
	gp.memberlist["dead-peer"] = &GossipNode{
		Info:        &NodeInfo{ID: "dead-peer", Status: NodeStatusAlive, Metadata: map[string]string{}},
		Incarnation: 2,
		State:       StateSuspect,
		Suspicion: &Suspicion{
			Incarnation: 2,
			From:        []string{"some-node"},
		},
	}
	gp.mu.Unlock()

	msg := makeGossipMsg(t, MessageTypeDead, &DeadMessage{
		Node:        "dead-peer",
		Incarnation: 2,
		From:        "witness",
	})
	gp.handleDeadMessage(msg)

	ml := gp.GetMemberlist()
	if ml["dead-peer"].State != StateDead {
		t.Errorf("state = %v, want StateDead", ml["dead-peer"].State)
	}
	if ml["dead-peer"].Suspicion != nil {
		t.Error("Suspicion should be cleared after dead message")
	}
	if gp.GetStats().DeathEvents != 1 {
		t.Errorf("DeathEvents = %d, want 1", gp.GetStats().DeathEvents)
	}
}

// TestGossipProtocol_HandleDeadMessage_StaleIncarnation verifies that a dead
// message with a lower incarnation is ignored.
func TestGossipProtocol_HandleDeadMessage_StaleIncarnation(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "stale-dead"))
	gp := cm.gossip

	gp.mu.Lock()
	gp.memberlist["stale-peer"] = &GossipNode{
		Info:        &NodeInfo{ID: "stale-peer", Metadata: map[string]string{}},
		Incarnation: 5,
		State:       StateAlive,
	}
	gp.mu.Unlock()

	msg := makeGossipMsg(t, MessageTypeDead, &DeadMessage{
		Node:        "stale-peer",
		Incarnation: 2, // older than current
		From:        "old-witness",
	})
	gp.handleDeadMessage(msg)

	ml := gp.GetMemberlist()
	if ml["stale-peer"].State != StateAlive {
		t.Errorf("state should remain StateAlive on stale dead message, got %v", ml["stale-peer"].State)
	}
}

// TestGossipProtocol_HandleSyncMessage_MergesNodes verifies that a sync
// message adds unknown nodes to the memberlist.
func TestGossipProtocol_HandleSyncMessage_MergesNodes(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "sync-host"))
	gp := cm.gossip

	remoteNodes := map[string]*GossipNode{
		"remote-a": {
			Info:        &NodeInfo{ID: "remote-a", Address: "10.0.0.1:9000", Status: NodeStatusAlive, Metadata: map[string]string{}},
			Incarnation: 1,
			State:       StateAlive,
			StateChange: time.Now(),
		},
		"remote-b": {
			Info:        &NodeInfo{ID: "remote-b", Address: "10.0.0.2:9000", Status: NodeStatusAlive, Metadata: map[string]string{}},
			Incarnation: 1,
			State:       StateAlive,
			StateChange: time.Now(),
		},
	}
	msg := makeGossipMsg(t, MessageTypeSync, &SyncMessage{Nodes: remoteNodes})
	gp.handleSyncMessage(msg)

	ml := gp.GetMemberlist()
	for _, id := range []string{"remote-a", "remote-b"} {
		if _, ok := ml[id]; !ok {
			t.Errorf("sync: node %q not found in memberlist", id)
		}
	}
}

// TestGossipProtocol_HandleSyncMessage_SkipsSelf verifies that a sync message does not overwrite
// the local node's own state with a peer's opinion of it.
//
// A peer's entry for us is hearsay: it is what that peer last heard, routed through however many
// other peers, and it can be arbitrarily stale. Adopting it would let one node with an old view
// mark a healthy node dead in its own memberlist — after which it stops gossiping, since
// performGossip and broadcastMessage both skip non-alive nodes, and the stale claim becomes true.
func TestGossipProtocol_HandleSyncMessage_SkipsSelf(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "sync-self"))
	gp := cm.gossip

	// Remote claims our node is dead at incarnation 999.
	remoteNodes := map[string]*GossipNode{
		"sync-self": {
			Info:        &NodeInfo{ID: "sync-self", Metadata: map[string]string{}},
			Incarnation: 999,
			State:       StateDead,
			StateChange: time.Now(),
		},
	}
	msg := makeGossipMsg(t, MessageTypeSync, &SyncMessage{Nodes: remoteNodes})
	gp.handleSyncMessage(msg)

	self := gp.GetMemberlist()["sync-self"]
	if self.State != StateAlive {
		t.Errorf("self state = %v after a sync claiming we are dead, want StateAlive", self.State)
	}

	// The incarnation is asserted to be above the accusation, not merely unchanged. Refuting is the
	// only way to stop that entry propagating back out, and a refutation the accuser's own
	// strictly-greater guard would reject is not a refutation (#272).
	if self.Incarnation <= 999 {
		t.Errorf("self incarnation = %d after refuting an accusation at 999; want > 999, "+
			"or the accuser will keep discarding our alive messages", self.Incarnation)
	}
}

// TestGossipProtocol_HandleHeartbeatMessage_UpdatesLastSeen verifies that a
// heartbeat message updates the LastSeen time of the target node.
func TestGossipProtocol_HandleHeartbeatMessage_UpdatesLastSeen(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "hb-host"))
	gp := cm.gossip

	oldTime := time.Now().Add(-1 * time.Hour)
	gp.mu.Lock()
	gp.memberlist["hb-peer"] = &GossipNode{
		Info:        &NodeInfo{ID: "hb-peer", LastSeen: oldTime, Metadata: map[string]string{}},
		Incarnation: 1,
		State:       StateAlive,
	}
	gp.mu.Unlock()

	newTimestamp := time.Now()
	msg := makeGossipMsg(t, MessageTypeGossipHeartbeat, &HeartbeatMessage{
		Node:        "hb-peer",
		Timestamp:   newTimestamp,
		Incarnation: 1,
	})
	gp.handleHeartbeatMessage(msg)

	ml := gp.GetMemberlist()
	if !ml["hb-peer"].Info.LastSeen.After(oldTime) {
		t.Error("LastSeen was not updated by heartbeat message")
	}
}

// TestGossipProtocol_HandleHeartbeatMessage_ClearsSuspicion verifies that a
// heartbeat from a suspected node clears suspicion and restores StateAlive.
func TestGossipProtocol_HandleHeartbeatMessage_ClearsSuspicion(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "hb-suspect"))
	gp := cm.gossip

	gp.mu.Lock()
	gp.memberlist["susp-hb-peer"] = &GossipNode{
		Info:        &NodeInfo{ID: "susp-hb-peer", Metadata: map[string]string{}},
		Incarnation: 2,
		State:       StateSuspect,
		Suspicion: &Suspicion{
			Incarnation: 2,
			From:        []string{"reporter"},
		},
	}
	gp.mu.Unlock()

	msg := makeGossipMsg(t, MessageTypeGossipHeartbeat, &HeartbeatMessage{
		Node:        "susp-hb-peer",
		Timestamp:   time.Now(),
		Incarnation: 2,
	})
	gp.handleHeartbeatMessage(msg)

	ml := gp.GetMemberlist()
	if ml["susp-hb-peer"].State != StateAlive {
		t.Errorf("state = %v after heartbeat, want StateAlive", ml["susp-hb-peer"].State)
	}
	if ml["susp-hb-peer"].Suspicion != nil {
		t.Error("Suspicion should be cleared after heartbeat")
	}
}

// The tests below cover incarnation liveness (#272): the mechanism that lets a node contradict a
// false report of its own failure, and the consequences of it never having advanced.
//
// Each of these fails on the pre-fix code, and the two most important ones — frozen stats and
// resurrection — are exactly what a single-transition assertion cannot see. "Send an alive message,
// assert the value arrived" passes on the bug, because the *first* message is applied; only a second
// message carrying a *different* value distinguishes a live feed from a permanently stale one.

// TestGossipProtocol_RefutesSuspicionAboutItself verifies that a suspect message naming this node
// raises its incarnation rather than marking it suspect in its own memberlist.
func TestGossipProtocol_RefutesSuspicionAboutItself(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "refuter"))
	gp := cm.gossip

	before := gp.GetMemberlist()["refuter"].Incarnation

	msg := makeGossipMsg(t, MessageTypeSuspect, &SuspectMessage{
		Node:        "refuter",
		Incarnation: before,
		From:        "confused-peer",
	})
	gp.handleSuspectMessage(msg)

	self := gp.GetMemberlist()["refuter"]
	if self.State != StateAlive {
		t.Errorf("self state = %v after a suspect message about ourselves, want StateAlive", self.State)
	}
	if self.Incarnation <= before {
		t.Errorf("self incarnation = %d, want > %d: a refutation that does not raise the "+
			"incarnation is indistinguishable from silence", self.Incarnation, before)
	}
	if got := gp.GetStats().SuspicionRefutations; got != 1 {
		t.Errorf("SuspicionRefutations = %d, want 1", got)
	}
}

// TestGossipProtocol_RefutesDeathReportAboutItself verifies the same for a death report, which is
// the more consequential of the two: nothing else in the protocol demotes a dead node.
func TestGossipProtocol_RefutesDeathReportAboutItself(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "not-dead"))
	gp := cm.gossip

	before := gp.GetMemberlist()["not-dead"].Incarnation

	msg := makeGossipMsg(t, MessageTypeDead, &DeadMessage{
		Node:        "not-dead",
		Incarnation: before,
		From:        "mistaken-witness",
	})
	gp.handleDeadMessage(msg)

	self := gp.GetMemberlist()["not-dead"]
	if self.State != StateAlive {
		t.Errorf("self state = %v after a death report about ourselves, want StateAlive: a node "+
			"that agrees it is dead stops gossiping and the report becomes true", self.State)
	}
	if self.Incarnation <= before {
		t.Errorf("self incarnation = %d, want > %d", self.Incarnation, before)
	}
	if got := gp.GetStats().DeathEvents; got != 0 {
		t.Errorf("DeathEvents = %d, want 0: our own refuted death is not a death event", got)
	}
}

// TestGossipProtocol_RefutationOutranksTheAccusation verifies that the new incarnation exceeds the
// accused one even when the accusation names a number higher than ours.
//
// A peer should not hold a higher incarnation for us than we published, but a peer restored from an
// old snapshot can. Refuting with self+1 would then produce a number that peer's own
// strictly-greater guard rejects, and the disagreement would never resolve.
func TestGossipProtocol_RefutationOutranksTheAccusation(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "outranked"))
	gp := cm.gossip

	const accused = 400

	msg := makeGossipMsg(t, MessageTypeSuspect, &SuspectMessage{
		Node:        "outranked",
		Incarnation: accused,
		From:        "peer-from-the-future",
	})
	gp.handleSuspectMessage(msg)

	if got := gp.GetMemberlist()["outranked"].Incarnation; got <= accused {
		t.Errorf("self incarnation = %d after an accusation at %d, want > %d", got, accused, accused)
	}
}

// TestGossipProtocol_StaleAccusationAboutItselfIsIgnored verifies that an accusation naming an
// incarnation we have already superseded does not raise ours again.
//
// This is what makes refutation converge. Without the guard, two nodes exchanging stale views would
// each refute the other's refutation and the incarnation would climb without bound.
func TestGossipProtocol_StaleAccusationAboutItselfIsIgnored(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "already-answered"))
	gp := cm.gossip

	gp.mu.Lock()
	gp.memberlist["already-answered"].Incarnation = 9
	gp.mu.Unlock()

	msg := makeGossipMsg(t, MessageTypeSuspect, &SuspectMessage{
		Node:        "already-answered",
		Incarnation: 8, // predates the refutation that produced 9
		From:        "lagging-peer",
	})
	gp.handleSuspectMessage(msg)

	if got := gp.GetMemberlist()["already-answered"].Incarnation; got != 9 {
		t.Errorf("self incarnation = %d, want 9: an accusation we have already answered must not "+
			"be answered again, or refutation never converges", got)
	}
	if got := gp.GetStats().SuspicionRefutations; got != 0 {
		t.Errorf("SuspicionRefutations = %d, want 0", got)
	}
}

// TestGossipProtocol_AliveMessageResurrectsADeadNode verifies that a node written off as dead
// returns to the memberlist and to the cluster manager on an alive message at a higher incarnation.
//
// Before #272 this was unreachable: no incarnation ever advanced, so the only alive message that
// could clear a death was one that never arrived, and a node removed by a transient network problem
// stayed removed for the life of the process — permanently absent from routing.
func TestGossipProtocol_AliveMessageResurrectsADeadNode(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "resurrect-host"))
	gp := cm.gossip

	gp.mu.Lock()
	gp.memberlist["revenant"] = &GossipNode{
		Info:        &NodeInfo{ID: "revenant", Address: "10.0.0.9:9000", Status: NodeStatusDead, Metadata: map[string]string{}},
		Incarnation: 3,
		State:       StateDead,
		StateChange: time.Now(),
	}
	gp.mu.Unlock()
	cm.UpdateNodeInfo("revenant", &NodeInfo{
		ID: "revenant", Address: "10.0.0.9:9000", Status: NodeStatusDead, Metadata: map[string]string{},
	})

	// The revenant refuted the death report and re-announced at a higher incarnation.
	msg := makeGossipMsg(t, MessageTypeAlive, &AliveMessage{
		Node:        &NodeInfo{ID: "revenant", Address: "10.0.0.9:9000", Metadata: map[string]string{}},
		Incarnation: 4,
	})
	gp.handleAliveMessage(msg)

	if got := gp.GetMemberlist()["revenant"].State; got != StateAlive {
		t.Errorf("state = %v after an alive message at a higher incarnation, want StateAlive", got)
	}

	// Asserted through the cluster manager as well, because that — not the memberlist — is what
	// SelectNodes reads. A node restored in gossip but still dead in the cluster manager is still
	// absent from routing, which is the consequence that matters.
	if got := cm.GetNodes()["revenant"].Status; got != NodeStatusAlive {
		t.Errorf("cluster manager status = %v, want %v: a node restored only in the memberlist is "+
			"still excluded from node selection", got, NodeStatusAlive)
	}
}

// TestGossipProtocol_AliveMessageDoesNotResurrectAtTheSameIncarnation verifies that the
// same-incarnation payload refresh does not quietly undo a death.
//
// The refresh exists so live stats are not frozen; it must not become a second, unguarded path back
// to alive, or the incarnation would stop being the thing that decides liveness.
func TestGossipProtocol_AliveMessageDoesNotResurrectAtTheSameIncarnation(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "no-quiet-revival"))
	gp := cm.gossip

	gp.mu.Lock()
	gp.memberlist["corpse"] = &GossipNode{
		Info:        &NodeInfo{ID: "corpse", Status: NodeStatusDead, MemoryUsage: 0.1, Metadata: map[string]string{}},
		Incarnation: 7,
		State:       StateDead,
		StateChange: time.Now(),
	}
	gp.mu.Unlock()

	msg := makeGossipMsg(t, MessageTypeAlive, &AliveMessage{
		Node:        &NodeInfo{ID: "corpse", MemoryUsage: 0.9, Metadata: map[string]string{}},
		Incarnation: 7,
	})
	gp.handleAliveMessage(msg)

	ml := gp.GetMemberlist()
	if got := ml["corpse"].State; got != StateDead {
		t.Errorf("state = %v after a same-incarnation alive message, want StateDead: overturning a "+
			"death requires a refutation, not a repeat", got)
	}
	if got := ml["corpse"].Info.MemoryUsage; got != 0.1 {
		t.Errorf("MemoryUsage = %v, want 0.1: a dead node's payload must not be refreshed either", got)
	}
}

// TestGossipProtocol_GossipedStatsAreNotFrozenAtTheFirstValue verifies that a *changed* stats value
// on a subsequent alive message reaches the memberlist and the cluster manager.
//
// This is the test #132's stated acceptance criterion could not be. "Wait two heartbeats, assert
// MemoryUsage > 0" passes on the frozen-stats bug, because the first message is applied and the
// value is non-zero forever after. A green test over a permanently stale metric is worse than no
// test: it certifies the thing it cannot see.
func TestGossipProtocol_GossipedStatsAreNotFrozenAtTheFirstValue(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "stats-host"))
	gp := cm.gossip

	// First alive message: discovery. Applied even on the buggy code.
	first := makeGossipMsg(t, MessageTypeAlive, &AliveMessage{
		Node: &NodeInfo{
			ID: "busy-peer", Address: "10.0.0.5:9000",
			MemoryUsage: 0.1, CacheSize: 1024, CacheHitRate: 0.5, Operations: 10,
			Metadata: map[string]string{},
		},
		Incarnation: 1,
	})
	gp.handleAliveMessage(first)

	// Second alive message, same incarnation — which is what a healthy node sends, since it has
	// nothing to refute and so never raises its incarnation — carrying new figures.
	second := makeGossipMsg(t, MessageTypeAlive, &AliveMessage{
		Node: &NodeInfo{
			ID: "busy-peer", Address: "10.0.0.5:9000",
			MemoryUsage: 0.9, CacheSize: 4096, CacheHitRate: 0.8, Operations: 99,
			Metadata: map[string]string{},
		},
		Incarnation: 1,
	})
	gp.handleAliveMessage(second)

	got := gp.GetMemberlist()["busy-peer"].Info
	if got.MemoryUsage != 0.9 {
		t.Errorf("MemoryUsage = %v, want 0.9: stats froze at the first value ever received", got.MemoryUsage)
	}
	if got.CacheSize != 4096 {
		t.Errorf("CacheSize = %d, want 4096", got.CacheSize)
	}
	if got.CacheHitRate != 0.8 {
		t.Errorf("CacheHitRate = %v, want 0.8", got.CacheHitRate)
	}
	if got.Operations != 99 {
		t.Errorf("Operations = %d, want 99", got.Operations)
	}

	// And through the cluster manager, since that is what a load-aware strategy would read.
	if cmGot := cm.GetNodes()["busy-peer"].MemoryUsage; cmGot != 0.9 {
		t.Errorf("cluster manager MemoryUsage = %v, want 0.9", cmGot)
	}
}

// TestGossipProtocol_PerformGossipStampsItsOwnLastSeen verifies that the LastSeen this node
// broadcasts is current.
//
// UpdateNodeInfo copies LastSeen onto the receiver's record and performHealthChecks compares it
// against HeartbeatInterval*3, so a node that announced the timestamp it was constructed with would
// be evidence of its own absence. That was inert only while the incarnation guard discarded the
// payload; once stats flow, it would make a healthy node flap.
func TestGossipProtocol_PerformGossipStampsItsOwnLastSeen(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig(t, "stamp-host"))
	gp := cm.gossip

	stale := time.Now().Add(-time.Hour)
	gp.mu.Lock()
	gp.localNode.LastSeen = stale
	// A peer is required, or performGossip returns before doing anything.
	gp.memberlist["stamp-peer"] = &GossipNode{
		Info:        &NodeInfo{ID: "stamp-peer", Address: "127.0.0.1:1", Metadata: map[string]string{}},
		Incarnation: 1,
		State:       StateAlive,
	}
	gp.mu.Unlock()

	gp.performGossip()

	gp.mu.RLock()
	got := gp.localNode.LastSeen
	gp.mu.RUnlock()

	if !got.After(stale) {
		t.Errorf("localNode.LastSeen = %v, unchanged from the stale value: every alive message this "+
			"node sends would be evidence it has not been heard from", got)
	}
}

// TestGossipProtocol_StartStop verifies that Start and Stop complete without
// error or panic.
func TestGossipProtocol_StartStop(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "lifecycle-gp")
	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	ctx := t.Context()

	if err := cm.gossip.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cm.gossip.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
