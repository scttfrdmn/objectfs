package distributed

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
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
}

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
)

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
}

// NewGossipProtocol creates a new gossip protocol instance
func NewGossipProtocol(cluster *ClusterManager, config *ClusterConfig) (*GossipProtocol, error) {
	gp := &GossipProtocol{
		cluster:    cluster,
		config:     config,
		memberlist: make(map[string]*GossipNode),
		stats: &GossipStats{
			MessagesByType: make(map[string]int64),
		},
		stopCh: make(chan struct{}),
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

	// Add self to member list
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

	log.Printf("Gossip protocol listening on %s", gp.config.ListenAddr)

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

	log.Printf("Gossip protocol stopped")
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
	buffer := make([]byte, gp.config.MaxGossipPacket)

	for {
		select {
		case <-ctx.Done():
			return
		case <-gp.stopCh:
			return
		default:
			if gp.conn == nil {
				continue
			}

			n, addr, err := gp.conn.ReadFromUDP(buffer)
			if err != nil {
				gp.stats.mu.Lock()
				gp.stats.NetworkErrors++
				gp.stats.mu.Unlock()
				continue
			}

			gp.handleIncomingMessage(buffer[:n], addr)
		}
	}
}

func (gp *GossipProtocol) handleIncomingMessage(data []byte, addr *net.UDPAddr) {
	var msg GossipMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to unmarshal gossip message: %v", err)
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
		gp.handleJoinMessage(&msg)
	case MessageTypeLeave:
		gp.handleLeaveMessage(&msg)
	case MessageTypeAlive:
		gp.handleAliveMessage(&msg)
	case MessageTypeSuspect:
		gp.handleSuspectMessage(&msg)
	case MessageTypeDead:
		gp.handleDeadMessage(&msg)
	case MessageTypeSync:
		gp.handleSyncMessage(&msg)
	case MessageTypeGossipHeartbeat:
		gp.handleHeartbeatMessage(&msg)
	}
}

func (gp *GossipProtocol) handleJoinMessage(msg *GossipMessage) {
	var joinMsg JoinMessage
	if err := json.Unmarshal(msg.Data, &joinMsg); err != nil {
		log.Printf("Failed to unmarshal join message: %v", err)
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

	log.Printf("Node %s joined the cluster", nodeID)

	gp.stats.mu.Lock()
	gp.stats.NodesDiscovered++
	gp.stats.mu.Unlock()

	// Send sync message back to the joining node
	_ = gp.sendSyncMessage(joinMsg.Node.Address)
}

func (gp *GossipProtocol) handleLeaveMessage(msg *GossipMessage) {
	var leaveData map[string]string
	if err := json.Unmarshal(msg.Data, &leaveData); err != nil {
		log.Printf("Failed to unmarshal leave message: %v", err)
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

		log.Printf("Node %s left the cluster", nodeID)
	}
}

func (gp *GossipProtocol) handleAliveMessage(msg *GossipMessage) {
	var aliveMsg AliveMessage
	if err := json.Unmarshal(msg.Data, &aliveMsg); err != nil {
		log.Printf("Failed to unmarshal alive message: %v", err)
		return
	}

	gp.mu.Lock()
	defer gp.mu.Unlock()

	nodeID := aliveMsg.Node.ID

	if gossipNode, exists := gp.memberlist[nodeID]; exists {
		// Update incarnation and state if newer
		if aliveMsg.Incarnation > gossipNode.Incarnation {
			gossipNode.Incarnation = aliveMsg.Incarnation
			gossipNode.State = StateAlive
			gossipNode.StateChange = time.Now()
			gossipNode.Info = aliveMsg.Node
			gossipNode.Suspicion = nil // Clear any suspicion

			// Update cluster manager
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

func (gp *GossipProtocol) handleSuspectMessage(msg *GossipMessage) {
	var suspectMsg SuspectMessage
	if err := json.Unmarshal(msg.Data, &suspectMsg); err != nil {
		log.Printf("Failed to unmarshal suspect message: %v", err)
		return
	}

	gp.mu.Lock()
	defer gp.mu.Unlock()

	nodeID := suspectMsg.Node

	if gossipNode, exists := gp.memberlist[nodeID]; exists {
		// Only process if incarnation matches
		if suspectMsg.Incarnation == gossipNode.Incarnation && gossipNode.State == StateAlive {
			if gossipNode.Suspicion == nil {
				gossipNode.Suspicion = &Suspicion{
					Incarnation: suspectMsg.Incarnation,
					From:        []string{suspectMsg.From},
					Timeout:     time.Now().Add(5 * time.Second),
				}
				gossipNode.State = StateSuspect
				gossipNode.StateChange = time.Now()

				log.Printf("Node %s marked as suspect by %s", nodeID, suspectMsg.From)

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
				found := false
				for _, from := range gossipNode.Suspicion.From {
					if from == suspectMsg.From {
						found = true
						break
					}
				}
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
		log.Printf("Failed to unmarshal dead message: %v", err)
		return
	}

	gp.mu.Lock()
	defer gp.mu.Unlock()

	nodeID := deadMsg.Node

	if gossipNode, exists := gp.memberlist[nodeID]; exists {
		// Only process if incarnation matches or is newer
		if deadMsg.Incarnation >= gossipNode.Incarnation {
			gossipNode.State = StateDead
			gossipNode.StateChange = time.Now()
			gossipNode.Suspicion = nil

			log.Printf("Node %s marked as dead by %s", nodeID, deadMsg.From)

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
		log.Printf("Failed to unmarshal sync message: %v", err)
		return
	}

	gp.mu.Lock()
	defer gp.mu.Unlock()

	// Merge membership information
	for nodeID, remoteNode := range syncMsg.Nodes {
		if nodeID == gp.localNode.ID {
			continue // Skip ourselves
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
		log.Printf("Failed to unmarshal heartbeat message: %v", err)
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
	gp.mu.RLock()
	nodes := make([]*GossipNode, 0, len(gp.memberlist))
	for _, node := range gp.memberlist {
		if node.Info.ID != gp.localNode.ID && node.State != StateDead && node.State != StateLeft {
			nodes = append(nodes, node)
		}
	}
	gp.mu.RUnlock()

	if len(nodes) == 0 {
		return
	}

	// Select random nodes to gossip with
	fanout := gp.config.GossipFanout
	if fanout > len(nodes) {
		fanout = len(nodes)
	}

	// Send alive message about ourselves
	aliveMsg := &AliveMessage{
		Node:        gp.localNode,
		Incarnation: gp.getCurrentIncarnation(),
	}

	msg := &GossipMessage{
		Type:      MessageTypeAlive,
		From:      gp.localNode.ID,
		Timestamp: time.Now(),
		MessageID: gp.generateMessageID(),
	}

	data, _ := json.Marshal(aliveMsg)
	msg.Data = data

	// Gossip to random subset of nodes
	for i := 0; i < fanout; i++ {
		targetNode := nodes[i%len(nodes)]
		if targetNode.Info != nil {
			_ = gp.sendMessage(targetNode.Info.Address, msg)
		}
	}

	// Send heartbeat
	heartbeatMsg := &HeartbeatMessage{
		Node:        gp.localNode.ID,
		Timestamp:   time.Now(),
		Incarnation: gp.getCurrentIncarnation(),
	}

	heartbeatGossipMsg := &GossipMessage{
		Type:      MessageTypeGossipHeartbeat,
		From:      gp.localNode.ID,
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

				log.Printf("Node %s suspicion timeout, marking as dead", nodeID)

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
	gp.stats.mu.Lock()
	defer gp.stats.mu.Unlock()

	// Calculate average message latency (simplified)
	if gp.stats.MessagesReceived > 0 {
		timeSinceLastMessage := time.Since(gp.stats.LastMessageReceived)
		if timeSinceLastMessage < time.Minute {
			gp.stats.AvgMessageLatency = timeSinceLastMessage
		}
	}
}

// Helper methods

func (gp *GossipProtocol) sendMessage(addr string, msg *GossipMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
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

func (gp *GossipProtocol) broadcastMessage(msg *GossipMessage) error {
	gp.mu.RLock()
	nodes := make([]*NodeInfo, 0, len(gp.memberlist))
	for _, gossipNode := range gp.memberlist {
		if gossipNode.Info != nil && gossipNode.Info.ID != gp.localNode.ID &&
			gossipNode.State != StateDead && gossipNode.State != StateLeft {
			nodes = append(nodes, gossipNode.Info)
		}
	}
	gp.mu.RUnlock()

	for _, node := range nodes {
		go func(addr string) {
			_ = gp.sendMessage(addr, msg)
		}(node.Address)
	}

	return nil
}

func (gp *GossipProtocol) sendSyncMessage(addr string) error {
	gp.mu.RLock()
	nodes := make(map[string]*GossipNode)
	for id, node := range gp.memberlist {
		nodes[id] = node
	}
	gp.mu.RUnlock()

	syncMsg := &SyncMessage{
		Nodes: nodes,
	}

	msg := &GossipMessage{
		Type:      MessageTypeSync,
		From:      gp.localNode.ID,
		Timestamp: time.Now(),
		MessageID: gp.generateMessageID(),
	}

	data, _ := json.Marshal(syncMsg)
	msg.Data = data

	return gp.sendMessage(addr, msg)
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
	}
	for k, v := range gp.stats.MessagesByType {
		stats.MessagesByType[k] = v
	}
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
			for k, v := range node.Info.Metadata {
				infoCopy.Metadata[k] = v
			}
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
