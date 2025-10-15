// Package status provides user-facing status indicators and progress tracking for ObjectFS operations
package status

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/objectfs/objectfs/pkg/errors"
	"github.com/objectfs/objectfs/pkg/health"
)

var opIDCounter uint64

// OperationStatus represents the status of a long-running operation
type OperationStatus int

const (
	// StatusPending indicates the operation has been queued but not started
	StatusPending OperationStatus = iota

	// StatusInProgress indicates the operation is currently executing
	StatusInProgress

	// StatusCompleted indicates the operation completed successfully
	StatusCompleted

	// StatusFailed indicates the operation failed
	StatusFailed

	// StatusCanceled indicates the operation was canceled
	StatusCanceled
)

// String returns the string representation of an operation status
func (s OperationStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusInProgress:
		return "in_progress"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// Operation represents a tracked operation with progress reporting
type Operation struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	Status    OperationStatus        `json:"status"`
	Progress  *Progress              `json:"progress,omitempty"`
	StartTime time.Time              `json:"start_time"`
	EndTime   *time.Time             `json:"end_time,omitempty"`
	Error     *errors.ObjectFSError  `json:"error,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`

	mu          sync.RWMutex
	cancelFunc  context.CancelFunc
	subscribers []chan OperationUpdate
}

// Progress tracks the progress of an operation
type Progress struct {
	Current    int64          `json:"current"`
	Total      int64          `json:"total"`
	Unit       string         `json:"unit"`
	Percentage float64        `json:"percentage"`
	Rate       float64        `json:"rate,omitempty"`    // items per second
	ETA        *time.Duration `json:"eta,omitempty"`     // estimated time to completion
	Phase      string         `json:"phase,omitempty"`   // current phase of operation
	Message    string         `json:"message,omitempty"` // current status message

	mu          sync.RWMutex
	lastUpdate  time.Time
	lastCurrent int64
}

// OperationUpdate represents an update to an operation's status
type OperationUpdate struct {
	Operation *Operation `json:"operation"`
	Timestamp time.Time  `json:"timestamp"`
	Message   string     `json:"message,omitempty"`
}

// Tracker tracks all operations and provides status information
type Tracker struct {
	mu            sync.RWMutex
	operations    map[string]*Operation
	history       []*Operation
	maxHistory    int
	healthTracker *health.Tracker
}

// TrackerConfig configures operation tracking behavior
type TrackerConfig struct {
	MaxHistorySize int             `json:"max_history_size"`
	HealthTracker  *health.Tracker `json:"-"`
}

// DefaultTrackerConfig returns default configuration
func DefaultTrackerConfig() TrackerConfig {
	return TrackerConfig{
		MaxHistorySize: 1000,
	}
}

// NewTracker creates a new operation tracker
func NewTracker(config TrackerConfig) *Tracker {
	if config.MaxHistorySize <= 0 {
		config.MaxHistorySize = 1000
	}

	return &Tracker{
		operations:    make(map[string]*Operation),
		history:       make([]*Operation, 0, config.MaxHistorySize),
		maxHistory:    config.MaxHistorySize,
		healthTracker: config.HealthTracker,
	}
}

// StartOperation creates and starts tracking a new operation
func (t *Tracker) StartOperation(ctx context.Context, opType string, metadata map[string]interface{}) (*Operation, context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()

	opCtx, cancel := context.WithCancel(ctx)

	op := &Operation{
		ID:          generateOperationID(),
		Type:        opType,
		Status:      StatusInProgress,
		StartTime:   time.Now(),
		Metadata:    metadata,
		cancelFunc:  cancel,
		subscribers: make([]chan OperationUpdate, 0),
	}

	t.operations[op.ID] = op
	t.notifySubscribers(op, "Operation started")

	return op, opCtx
}

// UpdateProgress updates the progress of an operation
func (t *Tracker) UpdateProgress(opID string, current, total int64, unit string) error {
	t.mu.RLock()
	op, exists := t.operations[opID]
	t.mu.RUnlock()

	if !exists {
		return errors.NewError(errors.ErrCodeObjectNotFound, "operation not found").
			WithContext("operation_id", opID)
	}

	op.mu.Lock()

	if op.Progress == nil {
		op.Progress = &Progress{
			Unit:       unit,
			lastUpdate: time.Now(),
		}
	}

	op.Progress.Update(current, total)

	// Unlock before notifying subscribers to avoid deadlock
	op.mu.Unlock()
	t.notifySubscribers(op, "Progress updated")

	return nil
}

// SetPhase sets the current phase of an operation
func (t *Tracker) SetPhase(opID string, phase string) error {
	t.mu.RLock()
	op, exists := t.operations[opID]
	t.mu.RUnlock()

	if !exists {
		return errors.NewError(errors.ErrCodeObjectNotFound, "operation not found").
			WithContext("operation_id", opID)
	}

	op.mu.Lock()

	if op.Progress == nil {
		op.Progress = &Progress{}
	}

	op.Progress.Phase = phase

	// Unlock before notifying subscribers to avoid deadlock
	op.mu.Unlock()
	t.notifySubscribers(op, "Phase changed: "+phase)

	return nil
}

// SetMessage sets the current status message of an operation
func (t *Tracker) SetMessage(opID string, message string) error {
	t.mu.RLock()
	op, exists := t.operations[opID]
	t.mu.RUnlock()

	if !exists {
		return errors.NewError(errors.ErrCodeObjectNotFound, "operation not found").
			WithContext("operation_id", opID)
	}

	op.mu.Lock()

	if op.Progress == nil {
		op.Progress = &Progress{}
	}

	op.Progress.Message = message

	// Unlock before notifying subscribers to avoid deadlock
	op.mu.Unlock()
	t.notifySubscribers(op, message)

	return nil
}

// CompleteOperation marks an operation as completed
func (t *Tracker) CompleteOperation(opID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	op, exists := t.operations[opID]
	if !exists {
		return errors.NewError(errors.ErrCodeObjectNotFound, "operation not found").
			WithContext("operation_id", opID)
	}

	op.mu.Lock()

	op.Status = StatusCompleted
	now := time.Now()
	op.EndTime = &now

	if op.cancelFunc != nil {
		op.cancelFunc()
	}

	// Save subscribers before removing from map
	subscribers := make([]chan OperationUpdate, len(op.subscribers))
	copy(subscribers, op.subscribers)
	op.mu.Unlock()

	t.moveToHistory(op)
	delete(t.operations, opID)

	// Notify subscribers after releasing all locks
	if len(subscribers) > 0 {
		t.notifySubscribersList(op, subscribers, "Operation completed")
	}

	return nil
}

// FailOperation marks an operation as failed
func (t *Tracker) FailOperation(opID string, err error) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	op, exists := t.operations[opID]
	if !exists {
		return errors.NewError(errors.ErrCodeObjectNotFound, "operation not found").
			WithContext("operation_id", opID)
	}

	op.mu.Lock()

	op.Status = StatusFailed
	now := time.Now()
	op.EndTime = &now

	if objErr, ok := err.(*errors.ObjectFSError); ok {
		op.Error = objErr
	} else {
		op.Error = errors.NewError(errors.ErrCodeOperationFailed, err.Error())
	}

	if op.cancelFunc != nil {
		op.cancelFunc()
	}

	// Save subscribers before removing from map
	subscribers := make([]chan OperationUpdate, len(op.subscribers))
	copy(subscribers, op.subscribers)
	op.mu.Unlock()

	t.moveToHistory(op)
	delete(t.operations, opID)

	// Notify subscribers after releasing all locks
	if len(subscribers) > 0 {
		t.notifySubscribersList(op, subscribers, "Operation failed: "+err.Error())
	}

	return nil
}

// CancelOperation cancels an operation
func (t *Tracker) CancelOperation(opID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	op, exists := t.operations[opID]
	if !exists {
		return errors.NewError(errors.ErrCodeObjectNotFound, "operation not found").
			WithContext("operation_id", opID)
	}

	op.mu.Lock()

	op.Status = StatusCanceled
	now := time.Now()
	op.EndTime = &now

	if op.cancelFunc != nil {
		op.cancelFunc()
	}

	// Save subscribers before removing from map
	subscribers := make([]chan OperationUpdate, len(op.subscribers))
	copy(subscribers, op.subscribers)
	op.mu.Unlock()

	t.moveToHistory(op)
	delete(t.operations, opID)

	// Notify subscribers after releasing all locks
	if len(subscribers) > 0 {
		t.notifySubscribersList(op, subscribers, "Operation canceled")
	}

	return nil
}

// GetOperation returns an operation by ID
func (t *Tracker) GetOperation(opID string) (*Operation, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	op, exists := t.operations[opID]
	if !exists {
		return nil, errors.NewError(errors.ErrCodeObjectNotFound, "operation not found").
			WithContext("operation_id", opID)
	}

	return op.Copy(), nil
}

// GetAllOperations returns all active operations
func (t *Tracker) GetAllOperations() []*Operation {
	t.mu.RLock()
	defer t.mu.RUnlock()

	ops := make([]*Operation, 0, len(t.operations))
	for _, op := range t.operations {
		ops = append(ops, op.Copy())
	}

	return ops
}

// GetHistory returns operation history
func (t *Tracker) GetHistory(limit int) []*Operation {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if limit <= 0 || limit > len(t.history) {
		limit = len(t.history)
	}

	result := make([]*Operation, limit)
	copy(result, t.history[:limit])

	return result
}

// Subscribe subscribes to operation updates
func (t *Tracker) Subscribe(opID string) (<-chan OperationUpdate, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	op, exists := t.operations[opID]
	if !exists {
		return nil, errors.NewError(errors.ErrCodeObjectNotFound, "operation not found").
			WithContext("operation_id", opID)
	}

	ch := make(chan OperationUpdate, 10)

	op.mu.Lock()
	op.subscribers = append(op.subscribers, ch)
	op.mu.Unlock()

	return ch, nil
}

// GetSystemStatus returns overall system status including health
func (t *Tracker) GetSystemStatus() *SystemStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()

	status := &SystemStatus{
		Timestamp:        time.Now(),
		ActiveOps:        len(t.operations),
		OperationsByType: make(map[string]int),
	}

	for _, op := range t.operations {
		status.OperationsByType[op.Type]++
	}

	if t.healthTracker != nil {
		status.HealthState = t.healthTracker.GetOverallHealth()
		status.ComponentHealth = t.healthTracker.GetAllComponents()
	}

	return status
}

// SystemStatus represents the overall system status
type SystemStatus struct {
	Timestamp        time.Time                          `json:"timestamp"`
	ActiveOps        int                                `json:"active_operations"`
	OperationsByType map[string]int                     `json:"operations_by_type"`
	HealthState      health.HealthState                 `json:"health_state"`
	ComponentHealth  map[string]*health.ComponentHealth `json:"component_health,omitempty"`
}

// moveToHistory moves an operation to history (must be called with lock held)
func (t *Tracker) moveToHistory(op *Operation) {
	t.history = append([]*Operation{op.Copy()}, t.history...)
	if len(t.history) > t.maxHistory {
		t.history = t.history[:t.maxHistory]
	}
}

// notifySubscribers notifies all subscribers of an operation update
func (t *Tracker) notifySubscribers(op *Operation, message string) {
	// Create copy without holding lock
	opCopy := op.Copy()

	update := OperationUpdate{
		Operation: opCopy,
		Timestamp: time.Now(),
		Message:   message,
	}

	// Acquire lock only for accessing subscribers list
	op.mu.RLock()
	subscribers := make([]chan OperationUpdate, len(op.subscribers))
	copy(subscribers, op.subscribers)
	op.mu.RUnlock()

	// Notify subscribers without holding locks
	for _, ch := range subscribers {
		select {
		case ch <- update:
		default:
			// Channel full, skip
		}
	}
}

// notifySubscribersList notifies a pre-extracted list of subscribers
func (t *Tracker) notifySubscribersList(op *Operation, subscribers []chan OperationUpdate, message string) {
	// Create copy without holding lock
	opCopy := op.Copy()

	update := OperationUpdate{
		Operation: opCopy,
		Timestamp: time.Now(),
		Message:   message,
	}

	// Notify subscribers without holding locks
	for _, ch := range subscribers {
		select {
		case ch <- update:
		default:
			// Channel full, skip
		}
	}
}

// Copy creates a deep copy of an operation
func (o *Operation) Copy() *Operation {
	o.mu.RLock()
	defer o.mu.RUnlock()

	copy := &Operation{
		ID:        o.ID,
		Type:      o.Type,
		Status:    o.Status,
		StartTime: o.StartTime,
		EndTime:   o.EndTime,
		Error:     o.Error,
		Metadata:  make(map[string]interface{}),
	}

	for k, v := range o.Metadata {
		copy.Metadata[k] = v
	}

	if o.Progress != nil {
		copy.Progress = o.Progress.Copy()
	}

	return copy
}

// Update updates progress metrics
func (p *Progress) Update(current, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	p.Current = current
	p.Total = total

	if total > 0 {
		p.Percentage = float64(current) / float64(total) * 100
	}

	// Calculate rate
	if !p.lastUpdate.IsZero() && current > p.lastCurrent {
		elapsed := now.Sub(p.lastUpdate).Seconds()
		if elapsed > 0 {
			p.Rate = float64(current-p.lastCurrent) / elapsed
		}

		// Calculate ETA
		if p.Rate > 0 && total > current {
			remaining := total - current
			etaSeconds := float64(remaining) / p.Rate
			eta := time.Duration(etaSeconds) * time.Second
			p.ETA = &eta
		}
	}

	p.lastUpdate = now
	p.lastCurrent = current
}

// Copy creates a deep copy of progress
func (p *Progress) Copy() *Progress {
	p.mu.RLock()
	defer p.mu.RUnlock()

	copy := &Progress{
		Current:     p.Current,
		Total:       p.Total,
		Unit:        p.Unit,
		Percentage:  p.Percentage,
		Rate:        p.Rate,
		Phase:       p.Phase,
		Message:     p.Message,
		lastUpdate:  p.lastUpdate,
		lastCurrent: p.lastCurrent,
	}

	if p.ETA != nil {
		eta := *p.ETA
		copy.ETA = &eta
	}

	return copy
}

// generateOperationID generates a unique operation ID
func generateOperationID() string {
	// Use atomic counter combined with timestamp for guaranteed uniqueness
	counter := atomic.AddUint64(&opIDCounter, 1)
	return fmt.Sprintf("%d-%d", time.Now().Unix(), counter)
}
