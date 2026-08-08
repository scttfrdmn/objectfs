package distributed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/scttfrdmn/objectfs/internal/distributed/hashring"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// Coordinator manages distributed operations across cluster nodes
type Coordinator struct {
	mu           sync.RWMutex
	cluster      *ClusterManager
	config       *ClusterConfig
	operations   map[string]*ActiveOperation
	loadBalancer *LoadBalancer
	stopCh       chan struct{}
	backend      types.Backend

	// pendingOps maps requestID → response channel for in-flight remote ops.
	pendingOpsMu sync.Mutex
	pendingOps   map[string]chan *NodeResult
}

// NodeOperationMessage is the on-wire request for a remote node execution.
type NodeOperationMessage struct {
	RequestID string                `json:"request_id"`
	From      string                `json:"from"`
	Operation *DistributedOperation `json:"operation"`
}

// NodeOperationRespMessage is the on-wire response for a remote node execution.
type NodeOperationRespMessage struct {
	RequestID string      `json:"request_id"`
	Result    *NodeResult `json:"result"`
}

// DistributedOperation represents an operation to be executed across the cluster.
//
// There was a Consistency field here, of type ConsistencyLevel, and #284 removed it along with the
// type and its three constants. What replaced it is [DistributedOperation.Precondition], and the
// reason it is a replacement rather than a removal plus an unrelated addition is that the two answer
// the same question — "may this write overwrite what is there?" — and only one of them could answer
// it. See the package documentation for the whole argument; the short form is that all three levels
// issued the same unconditional PutObject and differed only in how many nodes issued it.
type DistributedOperation struct {
	ID       string            `json:"id"`
	Type     OperationType     `json:"type"`
	Key      string            `json:"key"`
	Data     []byte            `json:"data,omitempty"`
	Offset   int64             `json:"offset,omitempty"`
	Size     int64             `json:"size,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`

	// Precondition, when set, makes an OpTypePut a compare-and-swap: the backend evaluates it and
	// refuses the write if it does not hold, reporting [types.ErrPreconditionFailed].
	//
	// This is the only guarantee this package offers about a write, and it is a real one, decided by
	// S3 rather than by a count of nodes that agreed. Its zero value means an unconditional write —
	// last-writer-wins — which is the honest description of what every consistency level here did.
	//
	// It is only consulted for OpTypePut. A precondition on a GET, a DELETE, or a LIST is a caller
	// error rather than something to ignore silently, so [Coordinator.ExecuteOperation] rejects it.
	Precondition types.Precondition `json:"precondition,omitzero"`

	Timeout     time.Duration `json:"timeout"`
	Retries     int           `json:"retries"`
	TargetNodes []string      `json:"target_nodes,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
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

// OperationResult represents the result of a distributed operation
type OperationResult struct {
	Success bool   `json:"success"`
	Data    []byte `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`

	// Conditional classifies a conditional-write failure and is empty for every other outcome. The
	// error [Coordinator.ExecuteOperation] returns wraps the matching sentinel, so a caller can use
	// either errors.Is on the error or this field on the result. See [ConditionalOutcome].
	Conditional ConditionalOutcome `json:"conditional,omitempty"`

	// ETag is the stored object's ETag after a successful OpTypePut, and is empty otherwise.
	//
	// It is here so that a caller performing a compare-and-swap loop can continue from it without a
	// HeadObject between iterations, and — the load-bearing use — so that a cache invalidation can
	// name the version it was computed from. See [ClusterManager.InvalidateCacheKey]: an invalidation
	// without an ETag can be applied out of order relative to the write that caused it, which is
	// requirement R4 of docs/design/conditional-writes-vs-raft.md §1.
	ETag string `json:"etag,omitempty"`

	NodeResults map[string]*NodeResult `json:"node_results"`
	Latency     time.Duration          `json:"latency"`
	RetriesUsed int                    `json:"retries_used"`
	CompletedAt time.Time              `json:"completed_at"`
}

// NodeResult represents the result from a specific node
type NodeResult struct {
	NodeID  string `json:"node_id"`
	Success bool   `json:"success"`
	Data    []byte `json:"data,omitempty"`

	// ETag is the stored object's ETag after a successful conditional put on this node. See
	// [OperationResult.ETag], which it is copied to.
	ETag string `json:"etag,omitempty"`

	// Conditional classifies a conditional-write failure, and is empty for every other outcome. See
	// [ConditionalOutcome] for why this travels as a string rather than as the error itself.
	Conditional ConditionalOutcome `json:"conditional,omitempty"`

	Error   string        `json:"error,omitempty"`
	Latency time.Duration `json:"latency"`
}

// ConditionalOutcome names why a conditional write did not land, so that a caller can select a
// recovery without parsing an error string.
//
// It exists because a NodeResult crosses the wire. An operation executed on a peer is marshaled to
// JSON, sent over gossip, and unmarshaled by the requesting node, so a Go error on the far side does
// not survive the trip — only exported fields do. Carrying the sentinel as an unexported field would
// make the classification silently correct for a local execution and silently absent for a remote
// one, which is a seam defect of exactly the kind this package's audit was full of.
//
// A bool would not do either. Per docs/design/conditional-writes-vs-raft.md §2 the taxonomy is
// three-way and the three select *different* recovery: a lost race means re-read and CAS again, a
// conflict means retry the same write as-is, and an absent object means the state being updated is
// gone and re-doing the CAS will never succeed.
type ConditionalOutcome string

const (
	// ConditionalLost means the precondition was evaluated and did not hold: another writer won.
	// Recovery is to re-read, recompute from what is now stored, and retry the compare-and-swap. This
	// is the expected outcome of a contended write, not an error to log as one.
	ConditionalLost ConditionalOutcome = "lost"

	// ConditionalConflict means the write raced a delete. The caller's view is not necessarily stale,
	// so retrying the same write may simply succeed.
	ConditionalConflict ConditionalOutcome = "conflict"

	// ConditionalUnsupported means the endpoint does not evaluate preconditions and nothing was
	// written. A caller needing mutual exclusion must refuse to proceed rather than fall back to an
	// unconditional write — see [types.ErrNotSupported].
	ConditionalUnsupported ConditionalOutcome = "unsupported"

	// ConditionalInvalid means the precondition itself was unusable — it asserted nothing, or it
	// asserted both absence and a specific ETag. A caller error, distinct from one that was evaluated.
	ConditionalInvalid ConditionalOutcome = "invalid"
)

// sentinel returns the [types] error o corresponds to, or nil for the empty outcome. It is the inverse
// of [classifyConditional] and exists so that errors.Is works identically on a local and a remote
// execution — see [Coordinator.ExecuteOperation].
func (o ConditionalOutcome) sentinel() error {
	switch o {
	case ConditionalLost:
		return types.ErrPreconditionFailed
	case ConditionalConflict:
		return types.ErrConditionalConflict
	case ConditionalUnsupported:
		return types.ErrNotSupported
	case ConditionalInvalid:
		return types.ErrInvalidPrecondition
	default:
		return nil
	}
}

// classifyConditional maps a [Backend.PutObjectIf] error onto a [ConditionalOutcome], returning the
// empty outcome for an error that is not about the precondition — a timeout, a network failure, an
// AccessDenied. Those are ordinary failures and a caller must not read them as a lost race.
func classifyConditional(err error) ConditionalOutcome {
	switch {
	case errors.Is(err, types.ErrPreconditionFailed):
		return ConditionalLost
	case errors.Is(err, types.ErrConditionalConflict):
		return ConditionalConflict
	case errors.Is(err, types.ErrNotSupported):
		return ConditionalUnsupported
	case errors.Is(err, types.ErrInvalidPrecondition):
		return ConditionalInvalid
	default:
		return ""
	}
}

// ActiveOperation tracks an ongoing distributed operation
type ActiveOperation struct {
	Operation *DistributedOperation
	Results   map[string]*NodeResult
	StartTime time.Time
	Deadline  time.Time
	_         sync.RWMutex
}

// There was a CacheReplicator here, with a ReplicationTask, a ReplicationStats of six counters, a
// once-per-second worker, and a `simulateReplication` that returned true. #284 deleted all of it, and
// the deciding fact is that nobody could say what it was for.
//
// What it did: after a write succeeded on one node, it registered the same key and bytes as a task,
// and a worker then sent every other target node an operation asking it to PUT those bytes to that
// key. Each peer can already reach S3, and the bytes were already there, so the work was to have N-1
// peers overwrite an object with its own contents. When gossip was not running — which is every unit
// test — `simulateReplication` returned true without sending anything and the statistics counted the
// bytes as replicated, so the counters reported throughput for work that had not happened.
//
// It could not have been cache warming either, which is the one purpose that would have justified it:
// warming means putting bytes into a peer's *cache*, and this sent a backend PUT. Cache warming is
// #141 and #142, it is a v0.13.0 item, and it needs a message type of its own rather than a consistency
// level's side effect. Reviving a path that re-uploads an object to itself is not a head start on it.

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

// NewCoordinator creates a new distributed operations coordinator.
// backend may be nil; inject a real backend via ClusterManager.SetBackend before
// executing operations that require S3 access.
func NewCoordinator(cluster *ClusterManager, config *ClusterConfig, backend types.Backend) (*Coordinator, error) {
	c := &Coordinator{
		cluster:    cluster,
		config:     config,
		operations: make(map[string]*ActiveOperation),
		pendingOps: make(map[string]chan *NodeResult),
		stopCh:     make(chan struct{}),
		backend:    backend,
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
	slog.Info("starting distributed operations coordinator")

	// Start background tasks
	go c.cleanupOperations(ctx)
	go c.updateLoadBalancerStats(ctx)

	return nil
}

// Stop stops the coordinator
func (c *Coordinator) Stop() error {
	close(c.stopCh)
	slog.Info("distributed operations coordinator stopped")
	return nil
}

// ExecuteOperation executes a distributed operation
func (c *Coordinator) ExecuteOperation(ctx context.Context, op *DistributedOperation) (*OperationResult, error) {
	start := time.Now()

	// Generate operation ID if not provided
	if op.ID == "" {
		nodeID := c.cluster.GetNodeID()
		if len(nodeID) > 8 {
			nodeID = nodeID[:8]
		}
		op.ID = fmt.Sprintf("op-%d-%s", time.Now().UnixNano(), nodeID)
	}

	// Set defaults
	if op.Timeout == 0 {
		op.Timeout = c.config.OperationTimeout
	}
	if op.Retries == 0 {
		op.Retries = c.config.RetryAttempts
	}
	op.CreatedAt = start

	// A precondition is only meaningful on a put, and a caller that attached one to a read or a delete
	// has a mistaken belief about what will be enforced. Rejecting is the fail-closed answer: silently
	// ignoring it would mean a caller that meant "delete only if unchanged" gets an unconditional
	// delete and no indication, which is the shape of every defect this package's audit found.
	if op.Type != OpTypePut && !op.Precondition.IsZero() {
		return &OperationResult{
			Success: false,
			Error:   fmt.Sprintf("a precondition is only supported on %s, not %s", OpTypePut, op.Type),
			Latency: time.Since(start),
		}, fmt.Errorf("distributed: precondition on %s operation: %w", op.Type, types.ErrInvalidPrecondition)
	}

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

	result := c.executeOnce(ctx, activeOp, targetNodes)

	result.CompletedAt = time.Now()
	result.Latency = time.Since(start)

	// A result that failed is returned with an error, always. The three consistency executors this
	// replaced each reported failure only in result.Success and returned a nil error, so a caller that
	// checked err — the ordinary thing to do in Go, and what ClusterManager.DistributeOperation did —
	// recorded an operation that failed on every node as a success (#269).
	//
	// Reconciling here rather than inside the executor is deliberate: this is the single point every
	// operation passes through, so a second execution strategy added later cannot reintroduce the
	// disagreement. The error text is result.Error so the two cannot drift either.
	if !result.Success {
		err = fmt.Errorf("operation %s on %q failed: %s", op.Type, op.Key, result.Error)

		// And it wraps the conditional-write sentinel when there was one, so a caller racing for a
		// claim can ask errors.Is(err, types.ErrPreconditionFailed) rather than inspect the result.
		//
		// Reconstructing it from result.Conditional rather than threading the original error through:
		// the original does not exist on the remote path. A NodeResult arrives from a peer as JSON with
		// its error already flattened to a string, so the classification the far side made is the only
		// thing that survived, and building the sentinel from it here is what makes errors.Is behave
		// the same whether the operation ran locally or on a peer.
		if sentinel := result.Conditional.sentinel(); sentinel != nil {
			err = fmt.Errorf("%w: %w", sentinel, err)
		}
	}

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

	// GetNodes returns a map, and Go randomizes map iteration order, so aliveNodes arrives in a
	// different order on every call. Sorting it makes selection a function of the membership set
	// rather than of the iteration that observed it — which the hash ring guarantees for itself, but
	// round-robin and the least-load tiebreak do not (#131).
	sort.Strings(aliveNodes)

	// Select nodes based on operation type and consistency requirements
	switch op.Type {
	case OpTypeGet:
		// For reads, select based on load balancing strategy
		return c.loadBalancer.SelectNodes(op.Key, aliveNodes, 1)

	case OpTypePut, OpTypeDelete:
		// For writes, select based on replication factor
		replicationFactor := min(c.config.ReplicationFactor, len(aliveNodes))
		return c.loadBalancer.SelectNodes(op.Key, aliveNodes, replicationFactor)

	case OpTypeList:
		// For list operations, use the leader or a random node
		if leader := c.cluster.GetLeader(); leader != "" {
			return []string{leader}, nil
		}
		return c.loadBalancer.SelectNodes(op.Key, aliveNodes, 1)

	case OpTypeBatch:
		// For batch operations, distribute across multiple nodes
		nodeCount := min(len(aliveNodes), 3)
		return c.loadBalancer.SelectNodes(op.Key, aliveNodes, nodeCount)

	default:
		return c.loadBalancer.SelectNodes(op.Key, aliveNodes, 1)
	}
}

// executeOnce runs the operation on one node — the first of targetNodes — and reports what that node
// did. It is the whole of this package's execution strategy.
//
// It replaced three functions, `executeStrongConsistency`, `executeSessionConsistency` and
// `executeEventualConsistency`, and the case for one function is not that the three were similar. It
// is that all three issued the *same unconditional PutObject* and differed only in how many nodes
// issued it and whether the caller waited:
//
//   - Strong fanned the operation to all N target nodes with a WaitGroup and declared success at
//     `successCount >= len(targetNodes)/2+1`. Every node wrote the same key in the same bucket with
//     the same bytes, so those were N redundant identical PUTs and the majority count answered "could
//     most nodes reach S3" — a reachability signal, billed N times.
//   - Session and Eventual were the same function. Diffing their bodies with comments stripped, the
//     only differences were a `len(targetNodes) > 0` guard before `targetNodes[0]` and an extra
//     `op.Type == OpTypePut` condition on the background replication.
//
// One node is enough because there is one copy of the bytes. S3 holds it, every node can already
// reach it, and a second node writing it again adds a request and no guarantee. What a caller who
// wanted "strong" actually needs is that its write not silently clobber a concurrent one, and that is
// [DistributedOperation.Precondition] — one conditional request to one key, evaluated by the store.
//
// targetNodes past the first are unused, and deliberately still selected: [Coordinator.selectTargetNodes]
// returns them in preference order, and the load-balancer statistics below count the whole set as
// routed work. A follow-on execution strategy that genuinely needs peers — cache warming, #141 — will
// want the list rather than have to reconstruct it.
func (c *Coordinator) executeOnce(ctx context.Context, activeOp *ActiveOperation, targetNodes []string) *OperationResult {
	op := activeOp.Operation

	if len(targetNodes) == 0 {
		return &OperationResult{
			Success:     false,
			Error:       "no target nodes selected",
			NodeResults: map[string]*NodeResult{},
		}
	}

	node := targetNodes[0]
	result := c.executeOnNode(ctx, node, op)

	return &OperationResult{
		Success:     result.Success,
		Data:        result.Data,
		ETag:        result.ETag,
		Conditional: result.Conditional,
		Error:       result.Error,
		NodeResults: map[string]*NodeResult{node: result},
	}
}

// executeOnNode executes an operation on a specific node. If nodeID is the
// local node, or if the gossip socket has not been started, it falls back to
// executeLocally so that unit tests without a running cluster still pass.
func (c *Coordinator) executeOnNode(ctx context.Context, nodeID string, op *DistributedOperation) *NodeResult {
	start := time.Now()

	// Local execution path.
	if nodeID == c.cluster.GetNodeID() ||
		c.cluster.gossip == nil || c.cluster.gossip.conn == nil {
		return c.executeLocally(ctx, nodeID, op)
	}

	// Remote execution path — send the operation over UDP and wait for a reply.
	nodes := c.cluster.GetNodes()
	node, exists := nodes[nodeID]
	if !exists || node.Status != NodeStatusAlive {
		return &NodeResult{
			NodeID:  nodeID,
			Success: false,
			Error:   fmt.Sprintf("node %s not found or not alive", nodeID),
			Latency: time.Since(start),
		}
	}

	requestID := op.ID + "-" + nodeID
	ch := make(chan *NodeResult, 1)

	c.pendingOpsMu.Lock()
	c.pendingOps[requestID] = ch
	c.pendingOpsMu.Unlock()

	defer func() {
		c.pendingOpsMu.Lock()
		delete(c.pendingOps, requestID)
		c.pendingOpsMu.Unlock()
	}()

	msg := &NodeOperationMessage{
		RequestID: requestID,
		From:      c.cluster.GetNodeID(),
		Operation: op,
	}

	if err := c.cluster.gossip.sendConsensusMsg(node.Address, MessageTypeNodeOperation, msg); err != nil {
		return &NodeResult{
			NodeID:  nodeID,
			Success: false,
			Error:   fmt.Sprintf("failed to send operation to %s: %v", nodeID, err),
			Latency: time.Since(start),
		}
	}

	timeout := op.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	select {
	case result := <-ch:
		result.Latency = time.Since(start)
		return result
	case <-ctx.Done():
		return &NodeResult{
			NodeID:  nodeID,
			Success: false,
			Error:   "context canceled",
			Latency: time.Since(start),
		}
	case <-time.After(timeout):
		return &NodeResult{
			NodeID:  nodeID,
			Success: false,
			Error:   "operation timed out waiting for remote response",
			Latency: time.Since(start),
		}
	}
}

// executeLocally runs the operation in-process using the configured S3 backend.
// When no backend is configured it returns an error result so callers can detect
// the misconfiguration rather than silently returning placeholder data.
//
// parent is what op.Timeout is applied to. It used to be context.Background(), which meant neither
// caller could stop an operation once it reached S3: [Coordinator.executeOnNode] had the caller's
// context and dropped it, and [Coordinator.handleNetworkOperation] is reached from the gossip receive
// loop, whose context is the cluster's lifetime. Either way a 30-second default timeout was the only
// bound, so a shutting-down node kept issuing GETs and PUTs a peer had asked for against a backend
// being closed underneath it.
func (c *Coordinator) executeLocally(parent context.Context, nodeID string, op *DistributedOperation) *NodeResult {
	start := time.Now()
	result := &NodeResult{NodeID: nodeID}

	if c.backend == nil {
		result.Error = "no backend configured"
		result.Latency = time.Since(start)
		return result
	}

	timeout := op.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	switch op.Type {
	case OpTypeGet:
		data, err := c.backend.GetObject(ctx, op.Key, op.Offset, op.Size)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Success = true
			result.Data = data
		}
	case OpTypePut:
		// One request, conditional when the caller asserted something about the key's current state.
		//
		// A precondition that does not hold comes back as types.ErrPreconditionFailed and nothing was
		// written; that is reported as a failed result like any other, and the caller distinguishes it
		// with errors.Is on the error ExecuteOperation returns. It is not retried here and must not be:
		// the answer is definitive, and the recovery is to re-read, recompute, and CAS again — which
		// only the caller can do, since only it knows what the new bytes should be.
		if op.Precondition.IsZero() {
			if err := c.backend.PutObject(ctx, op.Key, op.Data, op.Metadata); err != nil {
				result.Error = err.Error()
			} else {
				result.Success = true
			}

			break
		}

		etag, err := c.backend.PutObjectIf(ctx, op.Key, op.Data, op.Metadata, op.Precondition)
		if err != nil {
			result.Error = err.Error()
			result.Conditional = classifyConditional(err)
		} else {
			result.Success = true
			result.ETag = etag
		}
	case OpTypeDelete:
		if err := c.backend.DeleteObject(ctx, op.Key); err != nil {
			result.Error = err.Error()
		} else {
			result.Success = true
		}
	case OpTypeList:
		objs, err := c.backend.ListObjects(ctx, op.Key, 0)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Success = true
			result.Data, _ = json.Marshal(objs)
		}
	default:
		result.Error = fmt.Sprintf("unsupported operation type: %s", op.Type)
	}

	result.Latency = time.Since(start)
	return result
}

// handleNetworkOperation processes an incoming NodeOperationMessage from a peer.
//
// ctx is the gossip receive loop's, which is the cluster's lifetime: it descends from the context passed
// to [GossipProtocol.Start] and is canceled when the cluster shuts down. It reaches the S3 call
// [Coordinator.executeLocally] makes, so an operation a peer asked for is abandoned when this node is
// going away instead of running its full 30-second timeout against a backend being closed underneath it.
func (c *Coordinator) handleNetworkOperation(ctx context.Context, msg *GossipMessage) {
	var nm NodeOperationMessage
	if err := json.Unmarshal(msg.Data, &nm); err != nil {
		slog.Warn("failed to unmarshal NodeOperationMessage", "error", err)
		return
	}

	result := c.executeLocally(ctx, c.cluster.GetNodeID(), nm.Operation)

	resp := &NodeOperationRespMessage{
		RequestID: nm.RequestID,
		Result:    result,
	}

	// Send response back to the requesting node.
	nodes := c.cluster.GetNodes()
	senderID := msg.From
	if senderID == "" {
		senderID = nm.From
	}
	node, exists := nodes[senderID]
	if !exists {
		// Fall back to nm.From if msg.From was empty.
		node, exists = nodes[nm.From]
		if !exists {
			slog.Warn("cannot send operation response: sender not found", "sender_id", senderID)
			return
		}
	}

	if err := c.cluster.gossip.sendConsensusMsg(node.Address, MessageTypeNodeOperationResp, resp); err != nil {
		slog.Warn("failed to send operation response", "sender_id", senderID, "error", err)
	}
}

// handleNetworkOperationResp delivers a NodeOperationRespMessage to the
// waiting executeOnNode call via its pending channel.
func (c *Coordinator) handleNetworkOperationResp(msg *GossipMessage) {
	var resp NodeOperationRespMessage
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		slog.Warn("failed to unmarshal NodeOperationRespMessage", "error", err)
		return
	}

	c.pendingOpsMu.Lock()
	ch, exists := c.pendingOps[resp.RequestID]
	c.pendingOpsMu.Unlock()

	if exists {
		select {
		case ch <- resp.Result:
		default:
			slog.Info("dropped duplicate response for request", "request_id", resp.RequestID)
		}
	}
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
			slog.Info("cleaning up expired operation", "op_id", opID)
			delete(c.operations, opID)
		}
	}
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

// SelectNodes selects count nodes to execute an operation on the given key.
//
// key is the object key the operation addresses. It is a parameter rather than something the
// strategies look up because StrategyConsistentHash cannot function without it: the whole point of
// consistent hashing is that the node is a function of the key, and this method previously took only
// the node set and the count (#131). Every strategy takes it so that adding a key-dependent strategy
// is not another signature change, and the strategies that ignore it say so.
//
// The returned slice is in preference order: element 0 is the primary. Callers rely on that —
// Session consistency executes on targetNodes[0] first.
func (lb *LoadBalancer) SelectNodes(key string, availableNodes []string, count int) ([]string, error) {
	if count > len(availableNodes) {
		count = len(availableNodes)
	}

	if count <= 0 {
		return []string{}, nil
	}

	switch lb.strategy {
	case StrategyRoundRobin:
		return lb.selectRoundRobin(availableNodes, count)
	case StrategyLeastLoad:
		return lb.selectLeastLoad(availableNodes, count)
	case StrategyConsistentHash:
		return lb.selectConsistentHash(key, availableNodes, count)
	default:
		return availableNodes[:count], nil
	}
}

func (lb *LoadBalancer) selectRoundRobin(nodes []string, count int) ([]string, error) {
	// Simple round-robin selection
	selected := make([]string, count)
	for i := range count {
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
	for i := range len(nodeLoads) - 1 {
		for j := i + 1; j < len(nodeLoads); j++ {
			if nodeLoads[i].load > nodeLoads[j].load {
				nodeLoads[i], nodeLoads[j] = nodeLoads[j], nodeLoads[i]
			}
		}
	}

	selected := make([]string, count)
	for i := range count {
		selected[i] = nodeLoads[i].nodeID
	}

	return selected, nil
}

// selectConsistentHash maps key onto nodes with a rendezvous hash ring, so that the same key reaches
// the same node for as long as that node is alive.
//
// This used to be `return nodes[:count]` under a comment saying a real ring belonged here (#131).
// Since nodes arrives from a map iteration, that returned a different answer on each call for the
// same key — which is the one property consistent hashing exists to provide, and the property a
// cache needs in order to hit. See internal/distributed/hashring for the scheme and its bounds.
//
// The ring is built per call rather than kept as state on the LoadBalancer. Building it is a sorted
// insert per node, and the alternative is a ring that has to be kept in step with gossip membership
// — a second copy of the node set that can disagree with the first. At the node counts here the
// build is cheaper than being wrong; hashring's benchmarks are what that claim rests on.
func (lb *LoadBalancer) selectConsistentHash(key string, nodes []string, count int) ([]string, error) {
	if key == "" {
		// A keyless operation has nothing to hash. Round-robin is the honest fallback: it does not
		// pretend to affinity it cannot provide, and it does not silently return nodes[:count],
		// which would concentrate every keyless operation on whichever node sorted first.
		return lb.selectRoundRobin(nodes, count)
	}

	return hashring.New(nodes...).LookupN(key, count), nil
}

// GetStats returns coordinator statistics
func (c *Coordinator) GetStats() map[string]any {
	c.mu.RLock()
	activeOps := len(c.operations)
	c.mu.RUnlock()

	c.loadBalancer.stats.mu.RLock()
	loadBalancerStats := LoadBalancerStats{
		RequestsRouted:  c.loadBalancer.stats.RequestsRouted,
		AvgResponseTime: c.loadBalancer.stats.AvgResponseTime,
		Imbalance:       c.loadBalancer.stats.Imbalance,
		NodeLoad:        make(map[string]int64),
	}
	maps.Copy(loadBalancerStats.NodeLoad, c.loadBalancer.stats.NodeLoad)
	c.loadBalancer.stats.mu.RUnlock()

	// No "replication" key. It reported six counters from the CacheReplicator that #284 deleted, and
	// what they counted was peers re-uploading an object to itself — with the count incremented even
	// when nothing was sent, because simulateReplication returned true whenever gossip was not running.
	// A stats map that omits a subsystem is honest; one that reports throughput for work that did not
	// happen is the reason the audit distrusted this package's numbers.
	return map[string]any{
		"active_operations": activeOps,
		"load_balancer":     &loadBalancerStats,
	}
}
