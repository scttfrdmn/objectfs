// Package health provides service health tracking and graceful degradation for ObjectFS
package health

import (
	"context"
	stderr "errors"
	"fmt"
	"sync"
	"time"

	"github.com/objectfs/objectfs/pkg/errors"
)

// HealthState represents the overall health state of a service
type HealthState int

const (
	// StateHealthy indicates the service is fully operational
	StateHealthy HealthState = iota

	// StateDegraded indicates the service is operational but with reduced functionality
	StateDegraded

	// StateReadOnly indicates the service can only perform read operations
	StateReadOnly

	// StateUnavailable indicates the service is not operational
	StateUnavailable
)

// String returns the string representation of a health state
func (s HealthState) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateDegraded:
		return "degraded"
	case StateReadOnly:
		return "read-only"
	case StateUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// ComponentHealth tracks the health of a specific component
type ComponentHealth struct {
	Name              string                 `json:"name"`
	State             HealthState            `json:"state"`
	LastStateChange   time.Time              `json:"last_state_change"`
	LastHealthCheck   time.Time              `json:"last_health_check"`
	ConsecutiveErrors int                    `json:"consecutive_errors"`
	LastError         error                  `json:"-"`
	LastErrorMessage  string                 `json:"last_error_message,omitempty"`
	Metadata          map[string]interface{} `json:"metadata,omitempty"`
}

// Tracker tracks the health of multiple components and determines overall system health
type Tracker struct {
	mu              sync.RWMutex
	components      map[string]*ComponentHealth
	config          TrackerConfig
	stateCallbacks  map[HealthState][]StateChangeCallback
	healthListeners []HealthListener
}

// TrackerConfig configures health tracking behavior
type TrackerConfig struct {
	// ErrorThreshold is the number of consecutive errors before marking a component degraded
	ErrorThreshold int `yaml:"error_threshold" json:"error_threshold"`

	// UnavailableThreshold is the number of consecutive errors before marking unavailable
	UnavailableThreshold int `yaml:"unavailable_threshold" json:"unavailable_threshold"`

	// RecoveryThreshold is the number of consecutive successes to recover from degraded state
	RecoveryThreshold int `yaml:"recovery_threshold" json:"recovery_threshold"`

	// HealthCheckInterval is the interval for automatic health checks
	HealthCheckInterval time.Duration `yaml:"health_check_interval" json:"health_check_interval"`

	// StateHistorySize is the number of state changes to keep in history
	StateHistorySize int `yaml:"state_history_size" json:"state_history_size"`

	// EnableAutoRecovery enables automatic recovery from degraded states
	EnableAutoRecovery bool `yaml:"enable_auto_recovery" json:"enable_auto_recovery"`
}

// StateChangeCallback is called when a component's health state changes
type StateChangeCallback func(component string, oldState, newState HealthState, err error)

// HealthListener is notified of all health events
type HealthListener interface {
	OnStateChange(component string, oldState, newState HealthState, err error)
	OnHealthCheck(component string, healthy bool, err error)
}

// DefaultConfig returns a default tracker configuration
func DefaultConfig() TrackerConfig {
	return TrackerConfig{
		ErrorThreshold:       3,
		UnavailableThreshold: 10,
		RecoveryThreshold:    5,
		HealthCheckInterval:  30 * time.Second,
		StateHistorySize:     100,
		EnableAutoRecovery:   true,
	}
}

// NewTracker creates a new health tracker
func NewTracker(config TrackerConfig) *Tracker {
	return &Tracker{
		components:      make(map[string]*ComponentHealth),
		config:          config,
		stateCallbacks:  make(map[HealthState][]StateChangeCallback),
		healthListeners: make([]HealthListener, 0),
	}
}

// RegisterComponent registers a new component for health tracking
func (t *Tracker) RegisterComponent(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, exists := t.components[name]; !exists {
		t.components[name] = &ComponentHealth{
			Name:            name,
			State:           StateHealthy,
			LastStateChange: time.Now(),
			LastHealthCheck: time.Now(),
			Metadata:        make(map[string]interface{}),
		}
	}
}

// RecordSuccess records a successful operation for a component
func (t *Tracker) RecordSuccess(component string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	health, exists := t.components[component]
	if !exists {
		return
	}

	oldState := health.State
	health.LastHealthCheck = time.Now()

	// Reset error counter on success
	if health.ConsecutiveErrors > 0 {
		health.ConsecutiveErrors--

		// Check for recovery
		if health.ConsecutiveErrors == 0 && health.State != StateHealthy {
			t.transitionState(health, StateHealthy, nil)
		}
	}

	// Notify listeners
	for _, listener := range t.healthListeners {
		listener.OnHealthCheck(component, true, nil)
	}

	// Trigger callbacks if state changed
	if oldState != health.State {
		t.notifyStateChange(component, oldState, health.State, nil)
	}
}

// RecordError records an error for a component
func (t *Tracker) RecordError(component string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	health, exists := t.components[component]
	if !exists {
		return
	}

	oldState := health.State
	health.LastHealthCheck = time.Now()
	health.ConsecutiveErrors++
	health.LastError = err
	if err != nil {
		health.LastErrorMessage = err.Error()
	}

	// Determine new state based on error count
	var newState HealthState
	if health.ConsecutiveErrors >= t.config.UnavailableThreshold {
		newState = StateUnavailable
	} else if health.ConsecutiveErrors >= t.config.ErrorThreshold {
		// Check if error allows read-only mode
		if t.isWriteError(err) {
			newState = StateReadOnly
		} else {
			newState = StateDegraded
		}
	} else {
		newState = health.State
	}

	// Transition to new state if changed
	if newState != oldState {
		t.transitionState(health, newState, err)
	}

	// Notify listeners
	for _, listener := range t.healthListeners {
		listener.OnHealthCheck(component, false, err)
	}

	// Trigger callbacks if state changed
	if oldState != health.State {
		t.notifyStateChange(component, oldState, health.State, err)
	}
}

// GetState returns the current health state of a component
func (t *Tracker) GetState(component string) HealthState {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if health, exists := t.components[component]; exists {
		return health.State
	}
	return StateUnavailable
}

// GetComponentHealth returns the health information for a component
func (t *Tracker) GetComponentHealth(component string) (*ComponentHealth, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	health, exists := t.components[component]
	if !exists {
		return nil, fmt.Errorf("component %s not registered", component)
	}

	// Return a copy to prevent external modification
	return &ComponentHealth{
		Name:              health.Name,
		State:             health.State,
		LastStateChange:   health.LastStateChange,
		LastHealthCheck:   health.LastHealthCheck,
		ConsecutiveErrors: health.ConsecutiveErrors,
		LastError:         health.LastError,
		LastErrorMessage:  health.LastErrorMessage,
		Metadata:          health.Metadata,
	}, nil
}

// GetAllComponents returns health information for all registered components
func (t *Tracker) GetAllComponents() map[string]*ComponentHealth {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[string]*ComponentHealth)
	for name, health := range t.components {
		result[name] = &ComponentHealth{
			Name:              health.Name,
			State:             health.State,
			LastStateChange:   health.LastStateChange,
			LastHealthCheck:   health.LastHealthCheck,
			ConsecutiveErrors: health.ConsecutiveErrors,
			LastError:         health.LastError,
			LastErrorMessage:  health.LastErrorMessage,
			Metadata:          health.Metadata,
		}
	}
	return result
}

// GetOverallHealth returns the overall system health based on all components
func (t *Tracker) GetOverallHealth() HealthState {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if len(t.components) == 0 {
		return StateHealthy
	}

	// Overall health is determined by the worst component state
	overallState := StateHealthy
	for _, health := range t.components {
		if health.State > overallState {
			overallState = health.State
		}
	}

	return overallState
}

// IsHealthy returns true if the component is in a healthy state
func (t *Tracker) IsHealthy(component string) bool {
	return t.GetState(component) == StateHealthy
}

// CanRead returns true if the component can perform read operations
func (t *Tracker) CanRead(component string) bool {
	state := t.GetState(component)
	return state == StateHealthy || state == StateDegraded || state == StateReadOnly
}

// CanWrite returns true if the component can perform write operations
func (t *Tracker) CanWrite(component string) bool {
	state := t.GetState(component)
	return state == StateHealthy || state == StateDegraded
}

// AddStateChangeCallback registers a callback for state changes to a specific state
func (t *Tracker) AddStateChangeCallback(state HealthState, callback StateChangeCallback) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.stateCallbacks[state] = append(t.stateCallbacks[state], callback)
}

// AddHealthListener registers a health listener
func (t *Tracker) AddHealthListener(listener HealthListener) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.healthListeners = append(t.healthListeners, listener)
}

// SetComponentMetadata sets metadata for a component
func (t *Tracker) SetComponentMetadata(component, key string, value interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if health, exists := t.components[component]; exists {
		health.Metadata[key] = value
	}
}

// transitionState transitions a component to a new state (must be called with lock held)
func (t *Tracker) transitionState(health *ComponentHealth, newState HealthState, err error) {
	health.State = newState
	health.LastStateChange = time.Now()

	// Reset error counter on full recovery
	if newState == StateHealthy {
		health.ConsecutiveErrors = 0
		health.LastError = nil
		health.LastErrorMessage = ""
	}
}

// notifyStateChange notifies all callbacks and listeners of a state change
func (t *Tracker) notifyStateChange(component string, oldState, newState HealthState, err error) {
	// Call state-specific callbacks
	if callbacks, exists := t.stateCallbacks[newState]; exists {
		for _, callback := range callbacks {
			go callback(component, oldState, newState, err)
		}
	}

	// Notify all listeners
	for _, listener := range t.healthListeners {
		go listener.OnStateChange(component, oldState, newState, err)
	}
}

// isWriteError checks if an error indicates a write failure but reads may still work
func (t *Tracker) isWriteError(err error) bool {
	if err == nil {
		return false
	}

	// Check for ObjectFS error codes that indicate write failures
	var objErr *errors.ObjectFSError
	if stderr.As(err, &objErr) {
		switch objErr.Code {
		case errors.ErrCodeAccessDenied,
			errors.ErrCodeStorageWrite,
			errors.ErrCodePermissionDenied:
			return true
		}
	}

	return false
}

// StartHealthChecks starts periodic health checks for all components
func (t *Tracker) StartHealthChecks(ctx context.Context, checkFn func(component string) error) {
	ticker := time.NewTicker(t.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.performHealthChecks(checkFn)
		}
	}
}

// performHealthChecks performs health checks on all registered components
func (t *Tracker) performHealthChecks(checkFn func(component string) error) {
	t.mu.RLock()
	components := make([]string, 0, len(t.components))
	for name := range t.components {
		components = append(components, name)
	}
	t.mu.RUnlock()

	for _, component := range components {
		err := checkFn(component)
		if err != nil {
			t.RecordError(component, err)
		} else {
			t.RecordSuccess(component)
		}
	}
}
