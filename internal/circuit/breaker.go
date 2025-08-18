package circuit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// State represents the circuit breaker state
type State int

const (
	// StateClosed - circuit breaker is closed, requests pass through
	StateClosed State = iota
	// StateOpen - circuit breaker is open, requests are rejected
	StateOpen
	// StateHalfOpen - circuit breaker allows limited requests to test if service recovered
	StateHalfOpen
)

// String returns string representation of state
func (s State) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}

// Config contains circuit breaker configuration
type Config struct {
	// Maximum number of requests allowed to pass through when state is half-open
	MaxRequests uint32 `yaml:"max_requests"`

	// Period of the closed state after which the breaker considers a trip
	Interval time.Duration `yaml:"interval"`

	// Period of the open state after which the breaker enters half-open state
	Timeout time.Duration `yaml:"timeout"`

	// Function to determine if a request should be considered a failure
	ReadyToTrip func(counts Counts) bool `yaml:"-"`

	// Function called when state changes
	OnStateChange func(name string, from State, to State) `yaml:"-"`

	// Function to determine if an error should be counted as a failure
	IsSuccessful func(err error) bool `yaml:"-"`
}

// Counts holds the numbers of requests and their successes/failures
type Counts struct {
	Requests             uint32    `json:"requests"`
	TotalSuccesses       uint32    `json:"total_successes"`
	TotalFailures        uint32    `json:"total_failures"`
	ConsecutiveSuccesses uint32    `json:"consecutive_successes"`
	ConsecutiveFailures  uint32    `json:"consecutive_failures"`
	LastActivity         time.Time `json:"last_activity"`
}

// CircuitBreaker implements the circuit breaker pattern
type CircuitBreaker struct {
	name   string
	config Config

	mu     sync.Mutex
	state  State
	counts Counts
	expiry time.Time
}

// NewCircuitBreaker creates a new circuit breaker instance
func NewCircuitBreaker(name string, config Config) *CircuitBreaker {
	if config.MaxRequests == 0 {
		config.MaxRequests = 1
	}
	if config.Interval <= 0 {
		config.Interval = 60 * time.Second
	}
	if config.Timeout <= 0 {
		config.Timeout = 60 * time.Second
	}
	if config.ReadyToTrip == nil {
		config.ReadyToTrip = defaultReadyToTrip
	}
	if config.IsSuccessful == nil {
		config.IsSuccessful = defaultIsSuccessful
	}

	return &CircuitBreaker{
		name:   name,
		config: config,
		state:  StateClosed,
		counts: Counts{},
		expiry: time.Now().Add(config.Interval),
	}
}

// defaultReadyToTrip is the default function to determine when to trip the breaker
func defaultReadyToTrip(counts Counts) bool {
	return counts.Requests >= 20 &&
		float64(counts.TotalFailures)/float64(counts.Requests) >= 0.5
}

// defaultIsSuccessful is the default function to determine if a result is successful
func defaultIsSuccessful(err error) bool {
	return err == nil
}

// Execute runs the given function if the circuit breaker allows it
func (cb *CircuitBreaker) Execute(fn func() error) error {
	err, _ := cb.ExecuteWithFallback(fn, nil)
	return err
}

// ExecuteWithFallback runs the given function if the circuit breaker allows it,
// otherwise runs the fallback function
func (cb *CircuitBreaker) ExecuteWithFallback(fn func() error, fallback func() error) (error, bool) {
	if err := cb.beforeRequest(); err != nil {
		if fallback != nil {
			fallbackErr := fallback()
			return fallbackErr, true
		}
		return err, false
	}

	err := fn()
	cb.afterRequest(err)
	return err, false
}

// ExecuteWithContext runs the given function with context if the circuit breaker allows it
func (cb *CircuitBreaker) ExecuteWithContext(ctx context.Context, fn func(context.Context) error) error {
	if err := cb.beforeRequest(); err != nil {
		return err
	}

	err := fn(ctx)
	cb.afterRequest(err)
	return err
}

// beforeRequest is called before executing the request
func (cb *CircuitBreaker) beforeRequest() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	state, _ := cb.currentState(now)

	if state == StateOpen {
		return ErrOpenState
	}

	if state == StateHalfOpen && cb.counts.Requests >= cb.config.MaxRequests {
		return ErrTooManyRequests
	}

	cb.counts.onRequest()
	return nil
}

// afterRequest is called after executing the request
func (cb *CircuitBreaker) afterRequest(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	state, _ := cb.currentState(now)

	if cb.config.IsSuccessful(err) {
		cb.onSuccess(state, now)
	} else {
		cb.onFailure(state, now)
	}
}

// onSuccess handles successful requests
func (cb *CircuitBreaker) onSuccess(state State, now time.Time) {
	cb.counts.onSuccess()

	if state == StateHalfOpen {
		cb.setState(StateClosed, now)
	}
}

// onFailure handles failed requests
func (cb *CircuitBreaker) onFailure(state State, now time.Time) {
	cb.counts.onFailure()

	switch state {
	case StateClosed:
		if cb.config.ReadyToTrip(cb.counts) {
			cb.setState(StateOpen, now)
		}
	case StateHalfOpen:
		cb.setState(StateOpen, now)
	}
}

// currentState returns the current state of the circuit breaker
func (cb *CircuitBreaker) currentState(now time.Time) (State, time.Time) {
	switch cb.state {
	case StateClosed:
		if !cb.expiry.IsZero() && cb.expiry.Before(now) {
			cb.counts.clear()
			cb.expiry = now.Add(cb.config.Interval)
		}
	case StateOpen:
		if cb.expiry.Before(now) {
			cb.setState(StateHalfOpen, now)
		}
	}
	return cb.state, cb.expiry
}

// setState changes the state of the circuit breaker
func (cb *CircuitBreaker) setState(state State, now time.Time) {
	prev := cb.state

	if cb.state == state {
		return
	}

	cb.state = state
	cb.counts.clear()

	switch state {
	case StateClosed:
		cb.expiry = now.Add(cb.config.Interval)
	case StateOpen:
		cb.expiry = now.Add(cb.config.Timeout)
	case StateHalfOpen:
		cb.expiry = time.Time{}
	}

	if cb.config.OnStateChange != nil {
		cb.config.OnStateChange(cb.name, prev, state)
	}
}

// GetState returns the current state of the circuit breaker
func (cb *CircuitBreaker) GetState() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	state, _ := cb.currentState(time.Now())
	return state
}

// GetCounts returns a copy of the current counts
func (cb *CircuitBreaker) GetCounts() Counts {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	return cb.counts
}

// Reset resets the circuit breaker to its initial state
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.counts.clear()
	cb.setState(StateClosed, time.Now())
}

// Name returns the name of the circuit breaker
func (cb *CircuitBreaker) Name() string {
	return cb.name
}

// Methods for Counts struct

func (c *Counts) onRequest() {
	c.Requests++
	c.LastActivity = time.Now()
}

func (c *Counts) onSuccess() {
	c.TotalSuccesses++
	c.ConsecutiveSuccesses++
	c.ConsecutiveFailures = 0
}

func (c *Counts) onFailure() {
	c.TotalFailures++
	c.ConsecutiveFailures++
	c.ConsecutiveSuccesses = 0
}

func (c *Counts) clear() {
	c.Requests = 0
	c.TotalSuccesses = 0
	c.TotalFailures = 0
	c.ConsecutiveSuccesses = 0
	c.ConsecutiveFailures = 0
	c.LastActivity = time.Time{}
}

// Errors

var (
	// ErrOpenState is returned when the circuit breaker is open
	ErrOpenState = errors.New("circuit breaker is open")

	// ErrTooManyRequests is returned when too many requests are made in half-open state
	ErrTooManyRequests = errors.New("too many requests in half-open state")
)

// Manager manages multiple circuit breakers
type Manager struct {
	mu       sync.RWMutex
	breakers map[string]*CircuitBreaker
	config   Config
}

// NewManager creates a new circuit breaker manager
func NewManager(config Config) *Manager {
	return &Manager{
		breakers: make(map[string]*CircuitBreaker),
		config:   config,
	}
}

// GetBreaker gets or creates a circuit breaker with the given name
func (m *Manager) GetBreaker(name string) *CircuitBreaker {
	m.mu.RLock()
	if breaker, exists := m.breakers[name]; exists {
		m.mu.RUnlock()
		return breaker
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check in case another goroutine created it
	if breaker, exists := m.breakers[name]; exists {
		return breaker
	}

	breaker := NewCircuitBreaker(name, m.config)
	m.breakers[name] = breaker
	return breaker
}

// GetAllBreakers returns a copy of all circuit breakers
func (m *Manager) GetAllBreakers() map[string]*CircuitBreaker {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]*CircuitBreaker, len(m.breakers))
	for name, breaker := range m.breakers {
		result[name] = breaker
	}
	return result
}

// RemoveBreaker removes a circuit breaker
func (m *Manager) RemoveBreaker(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.breakers, name)
}

// ResetAll resets all circuit breakers
func (m *Manager) ResetAll() {
	m.mu.RLock()
	breakers := make([]*CircuitBreaker, 0, len(m.breakers))
	for _, breaker := range m.breakers {
		breakers = append(breakers, breaker)
	}
	m.mu.RUnlock()

	for _, breaker := range breakers {
		breaker.Reset()
	}
}

// GetStats returns statistics for all circuit breakers
func (m *Manager) GetStats() map[string]CircuitBreakerStats {
	m.mu.RLock()
	breakers := make(map[string]*CircuitBreaker, len(m.breakers))
	for name, breaker := range m.breakers {
		breakers[name] = breaker
	}
	m.mu.RUnlock()

	stats := make(map[string]CircuitBreakerStats)
	for name, breaker := range breakers {
		stats[name] = CircuitBreakerStats{
			Name:   name,
			State:  breaker.GetState(),
			Counts: breaker.GetCounts(),
		}
	}
	return stats
}

// CircuitBreakerStats represents statistics for a single circuit breaker
type CircuitBreakerStats struct {
	Name   string `json:"name"`
	State  State  `json:"state"`
	Counts Counts `json:"counts"`
}

// HealthCheck performs a health check on all circuit breakers
func (m *Manager) HealthCheck() error {
	stats := m.GetStats()

	var openBreakers []string
	for name, stat := range stats {
		if stat.State == StateOpen {
			openBreakers = append(openBreakers, name)
		}
	}

	if len(openBreakers) > 0 {
		return fmt.Errorf("circuit breakers open: %v", openBreakers)
	}

	return nil
}
