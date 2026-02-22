package distributed

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// makeGossipMsg marshals payload into a GossipMessage with the given type.
func makeGossipMsg(t *testing.T, msgType MessageType, payload interface{}) *GossipMessage {
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
	cm, err := NewClusterManager(testConfig("gp-node"))
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
	cm, _ := NewClusterManager(testConfig("self-node"))
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
	cm, _ := NewClusterManager(testConfig("stats-gp"))
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
	cm, _ := NewClusterManager(testConfig("mbt-node"))
	stats := cm.gossip.GetStats()

	if stats.MessagesByType == nil {
		t.Error("MessagesByType should be initialised (non-nil map)")
	}
}

// TestGossipProtocol_GetMemberlist_DeepCopy verifies that mutating the map
// returned by GetMemberlist does not affect internal state.
func TestGossipProtocol_GetMemberlist_DeepCopy(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig("copy-node"))
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
	cm, _ := NewClusterManager(testConfig("host-node"))
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
	cm, _ := NewClusterManager(testConfig("stat-host"))
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
	cm, _ := NewClusterManager(testConfig("alive-host"))
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
	cm, _ := NewClusterManager(testConfig("update-host"))
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
	cm, _ := NewClusterManager(testConfig("suspect-host"))
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
	cm, _ := NewClusterManager(testConfig("wrong-inc"))
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
	cm, _ := NewClusterManager(testConfig("multi-reporter"))
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
	cm, _ := NewClusterManager(testConfig("dead-host"))
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
	cm, _ := NewClusterManager(testConfig("stale-dead"))
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
	cm, _ := NewClusterManager(testConfig("sync-host"))
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

// TestGossipProtocol_HandleSyncMessage_SkipsSelf verifies that a sync message
// does not overwrite the local node's own entry.
func TestGossipProtocol_HandleSyncMessage_SkipsSelf(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig("sync-self"))
	gp := cm.gossip

	// Record the original self incarnation.
	selfBefore := gp.GetMemberlist()["sync-self"].Incarnation

	// Remote claims our node is at incarnation 999.
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

	// Self entry should be unchanged.
	selfAfter := gp.GetMemberlist()["sync-self"].Incarnation
	if selfAfter != selfBefore {
		t.Errorf("self incarnation changed from %d to %d; handleSyncMessage must skip self",
			selfBefore, selfAfter)
	}
}

// TestGossipProtocol_HandleHeartbeatMessage_UpdatesLastSeen verifies that a
// heartbeat message updates the LastSeen time of the target node.
func TestGossipProtocol_HandleHeartbeatMessage_UpdatesLastSeen(t *testing.T) {
	t.Parallel()
	cm, _ := NewClusterManager(testConfig("hb-host"))
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
	cm, _ := NewClusterManager(testConfig("hb-suspect"))
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

// TestGossipProtocol_StartStop verifies that Start and Stop complete without
// error or panic.
func TestGossipProtocol_StartStop(t *testing.T) {
	t.Parallel()
	cfg := testConfig("lifecycle-gp")
	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cm.gossip.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cm.gossip.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
