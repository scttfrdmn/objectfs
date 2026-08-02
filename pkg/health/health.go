// Package health provides service health tracking and graceful degradation for ObjectFS
package health

import (
	"context"
	stderr "errors"
	"fmt"
	"sync"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/errors"
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
	Name              string         `json:"name"`
	State             HealthState    `json:"state"`
	LastStateChange   time.Time      `json:"last_state_change"`
	LastHealthCheck   time.Time      `json:"last_health_check"`
	ConsecutiveErrors int            `json:"consecutive_errors"`
	LastError         error          `json:"-"`
	LastErrorMessage  string         `json:"last_error_message,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`

	// probing marks a component that the availability gate has admitted one operation into after
	// ProbeAfter elapsed, and whose outcome has not come back yet. It is the half-open state of a
	// circuit breaker.
	//
	// It exists because "one success proves the service works" cannot be expressed with the error
	// counter alone: RecordSuccess decrements, so recovering from ten consecutive errors would take
	// ten successes, and the gate only ever admits one operation at a time. The probe's result has
	// to be decisive in both directions — recover fully or latch again.
	probing bool

	// nextProbe is the earliest time another probe may be admitted. It gates admission on its own,
	// without consulting probing, so a probe whose result never arrives — a panicking caller, a
	// path that forgets to record its outcome — costs one probe interval rather than latching the
	// component forever. Deciding admission on the flag would rebuild the defect this whole
	// mechanism exists to fix.
	nextProbe time.Time
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

	// ProbeAfter is how long a component stays unavailable before one operation is admitted to
	// find out whether the service came back.
	//
	// Without it, StateUnavailable is a one-way door. Reaching it makes the availability gate
	// refuse every subsequent operation, and the only thing that clears it is RecordSuccess —
	// which is recorded by the operation the gate just refused. Nothing in ObjectFS calls
	// StartHealthChecks, so no other path can supply that success either. A component that became
	// unavailable stayed unavailable for the life of the process: ten reads of a missing key took
	// the whole mount permanently offline, verified by execution.
	//
	// This is the circuit breaker's half-open state, which internal/circuit already implements
	// correctly on a timer. Two mechanisms guard the same operation; this is the one that runs
	// first, so it is the one that has to recover.
	ProbeAfter time.Duration `yaml:"probe_after" json:"probe_after"`
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
		ProbeAfter:           30 * time.Second,
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
			Metadata:        make(map[string]any),
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

	switch {
	case health.probing:
		// The probe the gate admitted came back clean, so the service works. Recover outright
		// rather than decrementing: see ComponentHealth.probing.
		health.probing = false
		health.ConsecutiveErrors = 0
		t.transitionState(health, StateHealthy, nil)

	case health.ConsecutiveErrors > 0:
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

// RecordError records an error for a component.
//
// An error that is not evidence of a service failure — a missing object, a rejected request, a
// canceled operation — is recorded as a success instead, because that is what it is: the service
// was asked a question and answered it. See [errors.IsServiceFailure] for why the distinction
// cannot be skipped.
func (t *Tracker) RecordError(component string, err error) {
	if !isServiceFailure(err) {
		t.RecordSuccess(component)
		return
	}

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

	// A failed probe is decisive: the service is still down. Clearing the flag leaves the state
	// where it was — the component was already refusing operations, and a confirmed failure is no
	// reason to escalate past that. nextProbe was set when the probe was admitted, so the next
	// attempt is already one interval out.
	health.probing = false

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

// CanRead returns true if the component can perform read operations.
//
// Like [CanWrite], this admits a probe once ProbeAfter has elapsed in a state that refuses the
// operation, so it can change the component's state as a side effect. That mirrors a circuit
// breaker's half-open transition and is the reason a degraded component can ever recover: the
// success that clears the error count is only ever produced by an operation the gate allowed
// through.
func (t *Tracker) CanRead(component string) bool {
	state := t.admissionState(component)
	return state == StateHealthy || state == StateDegraded || state == StateReadOnly
}

// CanWrite returns true if the component can perform write operations.
func (t *Tracker) CanWrite(component string) bool {
	state := t.admissionState(component)
	return state == StateHealthy || state == StateDegraded
}

// admissionState returns the state to admit against, letting one operation through to probe a
// component that has been refusing them once ProbeAfter has elapsed.
//
// Every read and every write passes through here, so the common case takes a read lock and returns:
// a healthy or degraded component already admits its operations and is never a probe candidate.
func (t *Tracker) admissionState(component string) HealthState {
	t.mu.RLock()
	health, exists := t.components[component]
	if !exists {
		t.mu.RUnlock()
		return StateUnavailable
	}
	state, due := health.State, health.nextProbe
	t.mu.RUnlock()

	if t.config.ProbeAfter <= 0 || state == StateHealthy || state == StateDegraded {
		return state
	}
	if time.Now().Before(due) {
		return state
	}

	return t.probe(component)
}

// probe admits one operation against a component that has been refusing them, and reports the state
// to admit against.
//
// The component is left in the state it was in. Only the probing flag changes, so a caller reading
// GetState during a probe still sees the truth: the component is unavailable and has not yet been
// shown otherwise. The returned state is degraded purely to get this one operation past the gate,
// and RecordSuccess or RecordError then settles it.
func (t *Tracker) probe(component string) HealthState {
	t.mu.Lock()
	defer t.mu.Unlock()

	health, exists := t.components[component]
	if !exists {
		return StateUnavailable
	}

	// Re-check under the write lock: another caller may have probed or recovered the component
	// between the read lock above and this one. Without this, a burst of concurrent operations
	// would all be admitted as probes instead of one, and the gate would stop being a gate exactly
	// when the service is least able to absorb the load.
	now := time.Now()
	if health.State == StateHealthy || health.State == StateDegraded || now.Before(health.nextProbe) {
		return health.State
	}

	health.probing = true
	health.nextProbe = now.Add(t.config.ProbeAfter)
	return StateDegraded
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
func (t *Tracker) SetComponentMetadata(component, key string, value any) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if health, exists := t.components[component]; exists {
		health.Metadata[key] = value
	}
}

// transitionState transitions a component to a new state (must be called with lock held)
func (t *Tracker) transitionState(health *ComponentHealth, newState HealthState, err error) {
	now := time.Now()
	health.State = newState
	health.LastStateChange = now

	switch newState {
	case StateHealthy:
		// Reset error counter on full recovery
		health.ConsecutiveErrors = 0
		health.LastError = nil
		health.LastErrorMessage = ""
		health.probing = false
		health.nextProbe = time.Time{}

	case StateReadOnly, StateUnavailable:
		// Arm the probe clock on entry to a state that refuses operations. Without this the first
		// probe would be admitted immediately, turning a component that just failed into one that
		// retries on every call.
		health.probing = false
		health.nextProbe = now.Add(t.config.ProbeAfter)

	case StateDegraded:
		// Nothing to do, and the emptiness is the point rather than an omission. Degraded admits
		// operations, so there is no probe clock to arm — GetState returns it without consulting
		// nextProbe — and ConsecutiveErrors must keep accumulating, because that count is what
		// escalates degraded to unavailable. Resetting it here would make a component that fails
		// steadily below the unavailable threshold never reach it.
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

// isServiceFailure reports whether err is evidence the service is unwell.
//
// An error carrying no ObjectFS code counts as a failure. That is the safe direction for an
// unclassified error: the tracker degrades, and the probe timer restores it if the service is
// actually fine.
func isServiceFailure(err error) bool {
	if err == nil {
		return false
	}

	var objErr *errors.ObjectFSError
	if stderr.As(err, &objErr) {
		return errors.IsServiceFailure(objErr.Code)
	}
	return true
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
