package distributed

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Coordinator manages distributed operations across cluster nodes
type Coordinator struct {
	mu           sync.RWMutex
	cluster      *ClusterManager
	config       *ClusterConfig
	operations   map[string]*ActiveOperation
	replicator   *CacheReplicator
	loadBalancer *LoadBalancer
	stopCh       chan struct{}
}

// DistributedOperation represents an operation to be executed across the cluster
type DistributedOperation struct {
	ID          string            `json:"id"`
	Type        OperationType     `json:"type"`
	Key         string            `json:"key"`
	Data        []byte            `json:"data,omitempty"`
	Offset      int64             `json:"offset,omitempty"`
	Size        int64             `json:"size,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Consistency ConsistencyLevel  `json:"consistency"`
	Timeout     time.Duration     `json:"timeout"`
	Retries     int               `json:"retries"`
	TargetNodes []string          `json:"target_nodes,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

// OperationType represents the type of distributed operation
type OperationType string

const (
	OpTypeGet    OperationType = "get"
	OpTypePut    OperationType = "put"
	OpTypeDelete OperationType = "delete"
	OpTypeList   OperationType = "list"
	OpTypeBatch  OperationType = "batch"
)

// ConsistencyLevel represents the consistency requirement for an operation
type ConsistencyLevel string

const (
	ConsistencyEventual ConsistencyLevel = "eventual"
	ConsistencyStrong   ConsistencyLevel = "strong"
	ConsistencySession  ConsistencyLevel = "session"
)

// OperationResult represents the result of a distributed operation
type OperationResult struct {
	Success     bool                   `json:"success"`
	Data        []byte                 `json:"data,omitempty"`
	Error       string                 `json:"error,omitempty"`
	NodeResults map[string]*NodeResult `json:"node_results"`
	Latency     time.Duration          `json:"latency"`
	RetriesUsed int                    `json:"retries_used"`
	CompletedAt time.Time              `json:"completed_at"`
}

// NodeResult represents the result from a specific node
type NodeResult struct {
	NodeID  string        `json:"node_id"`
	Success bool          `json:"success"`
	Data    []byte        `json:"data,omitempty"`
	Error   string        `json:"error,omitempty"`
	Latency time.Duration `json:"latency"`
}

// ActiveOperation tracks an ongoing distributed operation
type ActiveOperation struct {
	Operation *DistributedOperation
	Results   map[string]*NodeResult
	StartTime time.Time
	Deadline  time.Time
	_         sync.RWMutex
}

// CacheReplicator handles cache replication across nodes
type CacheReplicator struct {
	mu           sync.RWMutex
	cluster      *ClusterManager
	config       *ClusterConfig
	replications map[string]*ReplicationTask
	stats        *ReplicationStats
}

// ReplicationTask represents a cache replication task
type ReplicationTask struct {
	Key         string    `json:"key"`
	Data        []byte    `json:"data"`
	TargetNodes []string  `json:"target_nodes"`
	CreatedAt   time.Time `json:"created_at"`
	Attempts    int       `json:"attempts"`
}

// ReplicationStats tracks replication statistics
type ReplicationStats struct {
	mu                 sync.RWMutex
	TasksCreated       int64         `json:"tasks_created"`
	TasksCompleted     int64         `json:"tasks_completed"`
	TasksFailed        int64         `json:"tasks_failed"`
	BytesReplicated    int64         `json:"bytes_replicated"`
	AvgReplicationTime time.Duration `json:"avg_replication_time"`
	ActiveTasks        int           `json:"active_tasks"`
}

// LoadBalancer manages load distribution across cluster nodes
type LoadBalancer struct {
	_        sync.RWMutex
	cluster  *ClusterManager
	strategy LoadBalancingStrategy
	stats    *LoadBalancerStats
}

// LoadBalancingStrategy represents different load balancing strategies
type LoadBalancingStrategy string

const (
	StrategyRoundRobin     LoadBalancingStrategy = "round_robin"
	StrategyLeastLoad      LoadBalancingStrategy = "least_load"
	StrategyConsistentHash LoadBalancingStrategy = "consistent_hash"
	StrategyLatencyBased   LoadBalancingStrategy = "latency_based"
)

// LoadBalancerStats tracks load balancing statistics
type LoadBalancerStats struct {
	mu              sync.RWMutex
	RequestsRouted  int64            `json:"requests_routed"`
	NodeLoad        map[string]int64 `json:"node_load"`
	AvgResponseTime time.Duration    `json:"avg_response_time"`
	Imbalance       float64          `json:"imbalance"`
}

// NewCoordinator creates a new distributed operations coordinator
func NewCoordinator(cluster *ClusterManager, config *ClusterConfig) (*Coordinator, error) {
	c := &Coordinator{
		cluster:    cluster,
		config:     config,
		operations: make(map[string]*ActiveOperation),
		stopCh:     make(chan struct{}),
	}

	// Initialize cache replicator
	c.replicator = &CacheReplicator{
		cluster:      cluster,
		config:       config,
		replications: make(map[string]*ReplicationTask),
		stats:        &ReplicationStats{},
	}

	// Initialize load balancer
	c.loadBalancer = &LoadBalancer{
		cluster:  cluster,
		strategy: StrategyLeastLoad,
		stats: &LoadBalancerStats{
			NodeLoad: make(map[string]int64),
		},
	}

	return c, nil
}

// Start starts the coordinator
func (c *Coordinator) Start(ctx context.Context) error {
	log.Printf("Starting distributed operations coordinator")

	// Start background tasks
	go c.cleanupOperations(ctx)
	go c.replicationWorker(ctx)
	go c.updateLoadBalancerStats(ctx)

	return nil
}

// Stop stops the coordinator
func (c *Coordinator) Stop() error {
	close(c.stopCh)
	log.Printf("Distributed operations coordinator stopped")
	return nil
}

// ExecuteOperation executes a distributed operation
func (c *Coordinator) ExecuteOperation(ctx context.Context, op *DistributedOperation) (*OperationResult, error) {
	start := time.Now()

	// Generate operation ID if not provided
	if op.ID == "" {
		op.ID = fmt.Sprintf("op-%d-%s", time.Now().UnixNano(), c.cluster.GetNodeID()[:8])
	}

	// Set defaults
	if op.Timeout == 0 {
		op.Timeout = c.config.OperationTimeout
	}
	if op.Retries == 0 {
		op.Retries = c.config.RetryAttempts
	}
	if op.Consistency == "" {
		op.Consistency = ConsistencyLevel(c.config.ConsistencyLevel)
	}
	op.CreatedAt = start

	// Track active operation
	activeOp := &ActiveOperation{
		Operation: op,
		Results:   make(map[string]*NodeResult),
		StartTime: start,
		Deadline:  start.Add(op.Timeout),
	}

	c.mu.Lock()
	c.operations[op.ID] = activeOp
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.operations, op.ID)
		c.mu.Unlock()
	}()

	// Select target nodes
	targetNodes, err := c.selectTargetNodes(op)
	if err != nil {
		return &OperationResult{
			Success: false,
			Error:   fmt.Sprintf("failed to select target nodes: %v", err),
			Latency: time.Since(start),
		}, err
	}

	// Execute operation based on type and consistency level
	var result *OperationResult

	switch op.Consistency {
	case ConsistencyStrong:
		result, err = c.executeStrongConsistency(ctx, activeOp, targetNodes)
	case ConsistencySession:
		result, err = c.executeSessionConsistency(ctx, activeOp, targetNodes)
	case ConsistencyEventual:
		result, err = c.executeEventualConsistency(ctx, activeOp, targetNodes)
	default:
		return &OperationResult{
			Success: false,
			Error:   fmt.Sprintf("unsupported consistency level: %s", op.Consistency),
			Latency: time.Since(start),
		}, fmt.Errorf("unsupported consistency level: %s", op.Consistency)
	}

	result.CompletedAt = time.Now()
	result.Latency = time.Since(start)

	// Update load balancer stats
	c.loadBalancer.stats.mu.Lock()
	c.loadBalancer.stats.RequestsRouted++
	for _, nodeID := range targetNodes {
		c.loadBalancer.stats.NodeLoad[nodeID]++
	}
	c.loadBalancer.stats.mu.Unlock()

	return result, err
}

// selectTargetNodes selects the appropriate nodes for an operation
func (c *Coordinator) selectTargetNodes(op *DistributedOperation) ([]string, error) {
	if len(op.TargetNodes) > 0 {
		// Use explicitly specified target nodes
		return op.TargetNodes, nil
	}

	nodes := c.cluster.GetNodes()
	aliveNodes := make([]string, 0)

	for nodeID, node := range nodes {
		if node.Status == NodeStatusAlive {
			aliveNodes = append(aliveNodes, nodeID)
		}
	}

	if len(aliveNodes) == 0 {
		return nil, fmt.Errorf("no alive nodes available")
	}

	// Select nodes based on operation type and consistency requirements
	switch op.Type {
	case OpTypeGet:
		// For reads, select based on load balancing strategy
		return c.loadBalancer.SelectNodes(aliveNodes, 1)

	case OpTypePut, OpTypeDelete:
		// For writes, select based on replication factor
		replicationFactor := c.config.ReplicationFactor
		if replicationFactor > len(aliveNodes) {
			replicationFactor = len(aliveNodes)
		}
		return c.loadBalancer.SelectNodes(aliveNodes, replicationFactor)

	case OpTypeList:
		// For list operations, use the leader or a random node
		if leader := c.cluster.GetLeader(); leader != "" {
			return []string{leader}, nil
		}
		return c.loadBalancer.SelectNodes(aliveNodes, 1)

	case OpTypeBatch:
		// For batch operations, distribute across multiple nodes
		nodeCount := len(aliveNodes)
		if nodeCount > 3 {
			nodeCount = 3
		}
		return c.loadBalancer.SelectNodes(aliveNodes, nodeCount)

	default:
		return c.loadBalancer.SelectNodes(aliveNodes, 1)
	}
}

// executeStrongConsistency executes an operation with strong consistency
func (c *Coordinator) executeStrongConsistency(ctx context.Context, activeOp *ActiveOperation, targetNodes []string) (*OperationResult, error) {
	op := activeOp.Operation

	// For strong consistency, we need consensus from majority of nodes
	requiredNodes := len(targetNodes)/2 + 1

	// Execute operation on all target nodes synchronously
	results := make(map[string]*NodeResult)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, nodeID := range targetNodes {
		wg.Add(1)
		go func(nodeID string) {
			defer wg.Done()

			nodeResult := c.executeOnNode(ctx, nodeID, op)

			mu.Lock()
			results[nodeID] = nodeResult
			mu.Unlock()
		}(nodeID)
	}

	wg.Wait()

	// Count successful results
	successCount := 0
	var firstSuccess *NodeResult
	var firstError string

	for _, result := range results {
		if result.Success {
			successCount++
			if firstSuccess == nil {
				firstSuccess = result
			}
		} else if firstError == "" {
			firstError = result.Error
		}
	}

	// Determine overall success based on majority
	success := successCount >= requiredNodes

	result := &OperationResult{
		Success:     success,
		NodeResults: results,
	}

	if success && firstSuccess != nil {
		result.Data = firstSuccess.Data
	} else {
		result.Error = fmt.Sprintf("insufficient successful responses (%d/%d required), first error: %s",
			successCount, requiredNodes, firstError)
	}

	return result, nil
}

// executeSessionConsistency executes an operation with session consistency
func (c *Coordinator) executeSessionConsistency(ctx context.Context, activeOp *ActiveOperation, targetNodes []string) (*OperationResult, error) {
	op := activeOp.Operation

	// For session consistency, try to use the same node for related operations
	// Fall back to any available node if the preferred node is unavailable

	var primaryNode string
	if len(targetNodes) > 0 {
		primaryNode = targetNodes[0]
	}

	// Execute on primary node first
	result := c.executeOnNode(ctx, primaryNode, op)

	operationResult := &OperationResult{
		Success:     result.Success,
		Data:        result.Data,
		Error:       result.Error,
		NodeResults: map[string]*NodeResult{primaryNode: result},
	}

	// If write operation succeeded, asynchronously replicate to other nodes
	if op.Type == OpTypePut && result.Success && len(targetNodes) > 1 {
		go c.replicateAsync(ctx, op, targetNodes[1:])
	}

	return operationResult, nil
}

// executeEventualConsistency executes an operation with eventual consistency
func (c *Coordinator) executeEventualConsistency(ctx context.Context, activeOp *ActiveOperation, targetNodes []string) (*OperationResult, error) {
	op := activeOp.Operation

	// For eventual consistency, execute on any available node and replicate asynchronously
	primaryNode := targetNodes[0]
	result := c.executeOnNode(ctx, primaryNode, op)

	operationResult := &OperationResult{
		Success:     result.Success,
		Data:        result.Data,
		Error:       result.Error,
		NodeResults: map[string]*NodeResult{primaryNode: result},
	}

	// Asynchronously replicate to other nodes
	if result.Success && len(targetNodes) > 1 {
		go c.replicateAsync(ctx, op, targetNodes[1:])
	}

	return operationResult, nil
}

// executeOnNode executes an operation on a specific node
func (c *Coordinator) executeOnNode(ctx context.Context, nodeID string, op *DistributedOperation) *NodeResult {
	start := time.Now()

	// In a real implementation, this would send the operation to the target node
	// For now, we'll simulate the execution

	result := &NodeResult{
		NodeID:  nodeID,
		Latency: time.Since(start),
	}

	// Simulate operation execution
	switch op.Type {
	case OpTypeGet:
		// Simulate get operation
		result.Success = true
		result.Data = []byte(fmt.Sprintf("data-from-%s", nodeID))

	case OpTypePut:
		// Simulate put operation
		result.Success = true

	case OpTypeDelete:
		// Simulate delete operation
		result.Success = true

	case OpTypeList:
		// Simulate list operation
		result.Success = true
		result.Data = []byte(`[{"key":"key1","size":1024},{"key":"key2","size":2048}]`)

	default:
		result.Success = false
		result.Error = fmt.Sprintf("unsupported operation type: %s", op.Type)
	}

	return result
}

// replicateAsync asynchronously replicates data to target nodes
func (c *Coordinator) replicateAsync(ctx context.Context, op *DistributedOperation, targetNodes []string) {
	if c.replicator == nil {
		return
	}

	task := &ReplicationTask{
		Key:         op.Key,
		Data:        op.Data,
		TargetNodes: targetNodes,
		CreatedAt:   time.Now(),
		Attempts:    0,
	}

	c.replicator.mu.Lock()
	c.replicator.replications[op.Key] = task
	c.replicator.stats.TasksCreated++
	c.replicator.stats.ActiveTasks++
	c.replicator.mu.Unlock()
}

// Background worker methods

func (c *Coordinator) cleanupOperations(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.performOperationCleanup()
		}
	}
}

func (c *Coordinator) performOperationCleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for opID, activeOp := range c.operations {
		if now.After(activeOp.Deadline) {
			log.Printf("Cleaning up expired operation: %s", opID)
			delete(c.operations, opID)
		}
	}
}

func (c *Coordinator) replicationWorker(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.processReplicationTasks(ctx)
		}
	}
}

func (c *Coordinator) processReplicationTasks(ctx context.Context) {
	c.replicator.mu.Lock()
	tasks := make([]*ReplicationTask, 0, len(c.replicator.replications))
	for _, task := range c.replicator.replications {
		tasks = append(tasks, task)
	}
	c.replicator.mu.Unlock()

	for _, task := range tasks {
		c.processReplicationTask(ctx, task)
	}
}

func (c *Coordinator) processReplicationTask(ctx context.Context, task *ReplicationTask) {
	start := time.Now()
	task.Attempts++

	successCount := 0
	for _, nodeID := range task.TargetNodes {
		// Simulate replication to node
		if c.simulateReplication(nodeID, task.Key, task.Data) {
			successCount++
		}
	}

	c.replicator.mu.Lock()
	if successCount > 0 {
		c.replicator.stats.TasksCompleted++
		c.replicator.stats.BytesReplicated += int64(len(task.Data))
		delete(c.replicator.replications, task.Key)
	} else if task.Attempts >= 3 {
		c.replicator.stats.TasksFailed++
		delete(c.replicator.replications, task.Key)
	}
	c.replicator.stats.ActiveTasks = len(c.replicator.replications)

	// Update average replication time
	replicationTime := time.Since(start)
	if c.replicator.stats.AvgReplicationTime == 0 {
		c.replicator.stats.AvgReplicationTime = replicationTime
	} else {
		alpha := 0.1
		c.replicator.stats.AvgReplicationTime = time.Duration(
			alpha*float64(replicationTime) + (1-alpha)*float64(c.replicator.stats.AvgReplicationTime),
		)
	}
	c.replicator.mu.Unlock()
}

func (c *Coordinator) simulateReplication(nodeID, key string, data []byte) bool {
	// In a real implementation, this would send the data to the target node
	// For simulation, we'll just return success most of the time
	nodes := c.cluster.GetNodes()
	if node, exists := nodes[nodeID]; exists && node.Status == NodeStatusAlive {
		return true
	}
	return false
}

func (c *Coordinator) updateLoadBalancerStats(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.calculateLoadBalancerStats()
		}
	}
}

func (c *Coordinator) calculateLoadBalancerStats() {
	c.loadBalancer.stats.mu.Lock()
	defer c.loadBalancer.stats.mu.Unlock()

	// Calculate load imbalance
	if len(c.loadBalancer.stats.NodeLoad) > 1 {
		var totalLoad, maxLoad, minLoad int64
		first := true

		for _, load := range c.loadBalancer.stats.NodeLoad {
			totalLoad += load
			if first {
				maxLoad = load
				minLoad = load
				first = false
			} else {
				if load > maxLoad {
					maxLoad = load
				}
				if load < minLoad {
					minLoad = load
				}
			}
		}

		if minLoad > 0 {
			c.loadBalancer.stats.Imbalance = float64(maxLoad-minLoad) / float64(minLoad)
		}
	}
}

// LoadBalancer methods

// SelectNodes selects nodes based on the load balancing strategy
func (lb *LoadBalancer) SelectNodes(availableNodes []string, count int) ([]string, error) {
	if count > len(availableNodes) {
		count = len(availableNodes)
	}

	if count == 0 {
		return []string{}, nil
	}

	switch lb.strategy {
	case StrategyRoundRobin:
		return lb.selectRoundRobin(availableNodes, count)
	case StrategyLeastLoad:
		return lb.selectLeastLoad(availableNodes, count)
	case StrategyConsistentHash:
		return lb.selectConsistentHash(availableNodes, count)
	default:
		return availableNodes[:count], nil
	}
}

func (lb *LoadBalancer) selectRoundRobin(nodes []string, count int) ([]string, error) {
	// Simple round-robin selection
	selected := make([]string, count)
	for i := 0; i < count; i++ {
		selected[i] = nodes[i%len(nodes)]
	}
	return selected, nil
}

func (lb *LoadBalancer) selectLeastLoad(nodes []string, count int) ([]string, error) {
	// Select nodes with the least load
	type nodeLoad struct {
		nodeID string
		load   int64
	}

	nodeLoads := make([]nodeLoad, 0, len(nodes))
	lb.stats.mu.RLock()
	for _, nodeID := range nodes {
		load := lb.stats.NodeLoad[nodeID]
		nodeLoads = append(nodeLoads, nodeLoad{nodeID: nodeID, load: load})
	}
	lb.stats.mu.RUnlock()

	// Sort by load (ascending)
	for i := 0; i < len(nodeLoads)-1; i++ {
		for j := i + 1; j < len(nodeLoads); j++ {
			if nodeLoads[i].load > nodeLoads[j].load {
				nodeLoads[i], nodeLoads[j] = nodeLoads[j], nodeLoads[i]
			}
		}
	}

	selected := make([]string, count)
	for i := 0; i < count; i++ {
		selected[i] = nodeLoads[i].nodeID
	}

	return selected, nil
}

func (lb *LoadBalancer) selectConsistentHash(nodes []string, count int) ([]string, error) {
	// Simple consistent hash implementation
	// In practice, you'd use a proper consistent hash ring
	if len(nodes) <= count {
		return nodes, nil
	}

	return nodes[:count], nil
}

// GetStats returns coordinator statistics
func (c *Coordinator) GetStats() map[string]interface{} {
	c.mu.RLock()
	activeOps := len(c.operations)
	c.mu.RUnlock()

	c.replicator.stats.mu.RLock()
	replicationStats := ReplicationStats{
		TasksCreated:       c.replicator.stats.TasksCreated,
		TasksCompleted:     c.replicator.stats.TasksCompleted,
		TasksFailed:        c.replicator.stats.TasksFailed,
		BytesReplicated:    c.replicator.stats.BytesReplicated,
		AvgReplicationTime: c.replicator.stats.AvgReplicationTime,
		ActiveTasks:        c.replicator.stats.ActiveTasks,
	}
	c.replicator.stats.mu.RUnlock()

	c.loadBalancer.stats.mu.RLock()
	loadBalancerStats := LoadBalancerStats{
		RequestsRouted:  c.loadBalancer.stats.RequestsRouted,
		AvgResponseTime: c.loadBalancer.stats.AvgResponseTime,
		Imbalance:       c.loadBalancer.stats.Imbalance,
		NodeLoad:        make(map[string]int64),
	}
	for k, v := range c.loadBalancer.stats.NodeLoad {
		loadBalancerStats.NodeLoad[k] = v
	}
	c.loadBalancer.stats.mu.RUnlock()

	return map[string]interface{}{
		"active_operations": activeOps,
		"replication":       &replicationStats,
		"load_balancer":     &loadBalancerStats,
	}
}
