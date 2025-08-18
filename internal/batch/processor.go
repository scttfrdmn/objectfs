package batch

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Operation represents a batched operation
type Operation struct {
	ID        string
	Type      OperationType
	Key       string
	Data      []byte
	Offset    int64
	Size      int64
	Context   context.Context
	Callback  func(error)
	Timestamp time.Time
}

// OperationType defines the type of batch operation
type OperationType int

const (
	OpTypeGet OperationType = iota
	OpTypePut
	OpTypeDelete
	OpTypeHead
)

// String returns string representation of operation type
func (ot OperationType) String() string {
	switch ot {
	case OpTypeGet:
		return "GET"
	case OpTypePut:
		return "PUT"
	case OpTypeDelete:
		return "DELETE"
	case OpTypeHead:
		return "HEAD"
	default:
		return "UNKNOWN"
	}
}

// Processor handles batching of operations for improved performance
type Processor struct {
	// Configuration
	maxBatchSize   int
	maxWaitTime    time.Duration
	maxConcurrency int

	// State
	mu         sync.Mutex
	operations map[OperationType][]*Operation
	flushTimer *time.Timer
	stopCh     chan struct{}
	wg         sync.WaitGroup
	started    bool

	// Backend interface
	backend Backend

	// Metrics
	stats ProcessorStats
}

// Backend interface for batch processor
type Backend interface {
	GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error)
	PutObject(ctx context.Context, key string, data []byte) error
	DeleteObject(ctx context.Context, key string) error
	HeadObject(ctx context.Context, key string) (interface{}, error)

	// Batch operations (if supported)
	GetObjects(ctx context.Context, keys []string) (map[string][]byte, error)
	PutObjects(ctx context.Context, objects map[string][]byte) error
}

// ProcessorConfig contains configuration for the batch processor
type ProcessorConfig struct {
	MaxBatchSize   int           `yaml:"max_batch_size"`  // Maximum operations per batch
	MaxWaitTime    time.Duration `yaml:"max_wait_time"`   // Maximum time to wait before flushing
	MaxConcurrency int           `yaml:"max_concurrency"` // Maximum concurrent batch operations
}

// ProcessorStats tracks batch processor statistics
type ProcessorStats struct {
	TotalOperations   int64         `json:"total_operations"`
	BatchedOperations int64         `json:"batched_operations"`
	BatchCount        int64         `json:"batch_count"`
	AverageBatchSize  float64       `json:"average_batch_size"`
	AverageWaitTime   time.Duration `json:"average_wait_time"`
	FlushCount        int64         `json:"flush_count"`
	ErrorCount        int64         `json:"error_count"`
}

// NewProcessor creates a new batch processor
func NewProcessor(backend Backend, config *ProcessorConfig) *Processor {
	if config == nil {
		config = &ProcessorConfig{
			MaxBatchSize:   100,
			MaxWaitTime:    10 * time.Millisecond,
			MaxConcurrency: 10,
		}
	}

	return &Processor{
		maxBatchSize:   config.MaxBatchSize,
		maxWaitTime:    config.MaxWaitTime,
		maxConcurrency: config.MaxConcurrency,
		operations:     make(map[OperationType][]*Operation),
		stopCh:         make(chan struct{}),
		backend:        backend,
	}
}

// Start starts the batch processor
func (p *Processor) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.started {
		return fmt.Errorf("processor already started")
	}

	p.started = true
	p.wg.Add(1)
	go p.processLoop()

	return nil
}

// Stop stops the batch processor and flushes pending operations
func (p *Processor) Stop() error {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return fmt.Errorf("processor not started")
	}
	started := p.started
	p.started = false
	p.mu.Unlock()

	if started {
		close(p.stopCh)
		p.wg.Wait()

		// Flush any remaining operations
		p.flush()
	}

	return nil
}

// Submit submits an operation for batching
func (p *Processor) Submit(op *Operation) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.started {
		return fmt.Errorf("processor not started")
	}

	// Add operation to appropriate batch
	p.operations[op.Type] = append(p.operations[op.Type], op)
	p.stats.TotalOperations++

	// Check if we need to flush immediately
	if p.shouldFlush() {
		go p.flush()
	} else if p.flushTimer == nil {
		// Start timer for automatic flush
		p.flushTimer = time.AfterFunc(p.maxWaitTime, func() {
			p.flush()
		})
	}

	return nil
}

// shouldFlush determines if batches should be flushed immediately
func (p *Processor) shouldFlush() bool {
	for _, ops := range p.operations {
		if len(ops) >= p.maxBatchSize {
			return true
		}
	}
	return false
}

// flush executes all pending batch operations
func (p *Processor) flush() {
	p.mu.Lock()

	if p.flushTimer != nil {
		p.flushTimer.Stop()
		p.flushTimer = nil
	}

	// Take snapshot of current operations
	toProcess := make(map[OperationType][]*Operation)
	for opType, ops := range p.operations {
		if len(ops) > 0 {
			toProcess[opType] = make([]*Operation, len(ops))
			copy(toProcess[opType], ops)
			p.operations[opType] = nil
		}
	}

	p.mu.Unlock()

	if len(toProcess) == 0 {
		return
	}

	p.stats.FlushCount++

	// Process each operation type in parallel
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, p.maxConcurrency)

	for opType, ops := range toProcess {
		if len(ops) == 0 {
			continue
		}

		wg.Add(1)
		go func(opType OperationType, ops []*Operation) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			p.processBatch(opType, ops)
		}(opType, ops)
	}

	wg.Wait()
}

// processBatch processes a batch of operations of the same type
func (p *Processor) processBatch(opType OperationType, ops []*Operation) {
	if len(ops) == 0 {
		return
	}

	p.stats.BatchCount++
	p.stats.BatchedOperations += int64(len(ops))

	// Update average batch size
	if p.stats.BatchCount > 0 {
		p.stats.AverageBatchSize = float64(p.stats.BatchedOperations) / float64(p.stats.BatchCount)
	}

	switch opType {
	case OpTypeGet:
		p.processGetBatch(ops)
	case OpTypePut:
		p.processPutBatch(ops)
	case OpTypeDelete:
		p.processDeleteBatch(ops)
	case OpTypeHead:
		p.processHeadBatch(ops)
	}
}

// processGetBatch processes a batch of GET operations
func (p *Processor) processGetBatch(ops []*Operation) {
	// Try to use batch get if available
	keys := make([]string, len(ops))
	keyToOp := make(map[string]*Operation)

	for i, op := range ops {
		keys[i] = op.Key
		keyToOp[op.Key] = op
	}

	results, err := p.backend.GetObjects(context.Background(), keys)
	if err != nil {
		// Fallback to individual operations
		for _, op := range ops {
			_, getErr := p.backend.GetObject(op.Context, op.Key, op.Offset, op.Size)
			if getErr != nil {
				p.stats.ErrorCount++
				if op.Callback != nil {
					op.Callback(getErr)
				}
			} else {
				// TODO: Store result somehow (would need to extend Operation struct)
				if op.Callback != nil {
					op.Callback(nil)
				}
			}
		}
		return
	}

	// Process batch results
	for key := range results {
		if op, exists := keyToOp[key]; exists {
			// TODO: Handle result data (would need to extend Operation struct)
			if op.Callback != nil {
				op.Callback(nil)
			}
		}
	}

	// Handle operations that didn't get results
	for _, op := range ops {
		if _, exists := results[op.Key]; !exists {
			p.stats.ErrorCount++
			if op.Callback != nil {
				op.Callback(fmt.Errorf("operation not completed"))
			}
		}
	}
}

// processPutBatch processes a batch of PUT operations
func (p *Processor) processPutBatch(ops []*Operation) {
	// Try to use batch put if available
	objects := make(map[string][]byte)

	for _, op := range ops {
		objects[op.Key] = op.Data
	}

	err := p.backend.PutObjects(context.Background(), objects)
	if err != nil {
		// Fallback to individual operations
		for _, op := range ops {
			putErr := p.backend.PutObject(op.Context, op.Key, op.Data)
			if putErr != nil {
				p.stats.ErrorCount++
			}
			if op.Callback != nil {
				op.Callback(putErr)
			}
		}
		return
	}

	// All operations succeeded
	for _, op := range ops {
		if op.Callback != nil {
			op.Callback(nil)
		}
	}
}

// processDeleteBatch processes a batch of DELETE operations
func (p *Processor) processDeleteBatch(ops []*Operation) {
	// Process individual operations (DELETE doesn't usually have batch operations)
	for _, op := range ops {
		err := p.backend.DeleteObject(op.Context, op.Key)
		if err != nil {
			p.stats.ErrorCount++
		}
		if op.Callback != nil {
			op.Callback(err)
		}
	}
}

// processHeadBatch processes a batch of HEAD operations
func (p *Processor) processHeadBatch(ops []*Operation) {
	// Process individual operations (HEAD doesn't usually have batch operations)
	for _, op := range ops {
		_, err := p.backend.HeadObject(op.Context, op.Key)
		if err != nil {
			p.stats.ErrorCount++
		}
		if op.Callback != nil {
			op.Callback(err)
		}
	}
}

// processLoop is the main processing loop
func (p *Processor) processLoop() {
	defer p.wg.Done()

	ticker := time.NewTicker(p.maxWaitTime)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.flush()
		}
	}
}

// GetStats returns current processor statistics
func (p *Processor) GetStats() ProcessorStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}
