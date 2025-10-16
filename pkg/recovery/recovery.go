// Package recovery provides enhanced error handling and automatic recovery mechanisms for ObjectFS
package recovery

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/objectfs/objectfs/internal/circuit"
	"github.com/objectfs/objectfs/pkg/errors"
	"github.com/objectfs/objectfs/pkg/retry"
	"github.com/objectfs/objectfs/pkg/status"
	"github.com/objectfs/objectfs/pkg/utils"
)

// RecoveryStrategy defines how to handle and recover from errors
type RecoveryStrategy int

const (
	// StrategyRetry attempts to retry the operation with backoff
	StrategyRetry RecoveryStrategy = iota

	// StrategyCircuitBreaker uses circuit breaker to prevent cascading failures
	StrategyCircuitBreaker

	// StrategyGracefulDegradation continues with reduced functionality
	StrategyGracefulDegradation

	// StrategyFallback uses an alternative implementation
	StrategyFallback

	// StrategyFailFast immediately fails without retry
	StrategyFailFast
)

// String returns the string representation of a recovery strategy
func (s RecoveryStrategy) String() string {
	switch s {
	case StrategyRetry:
		return "retry"
	case StrategyCircuitBreaker:
		return "circuit_breaker"
	case StrategyGracefulDegradation:
		return "graceful_degradation"
	case StrategyFallback:
		return "fallback"
	case StrategyFailFast:
		return "fail_fast"
	default:
		return "unknown"
	}
}

// RecoveryConfig configures recovery behavior
type RecoveryConfig struct {
	// DefaultStrategy is the default recovery strategy to use
	DefaultStrategy RecoveryStrategy

	// RetryConfig configures retry behavior
	RetryConfig retry.Config

	// CircuitBreakerConfig configures circuit breaker behavior
	CircuitBreakerConfig circuit.Config

	// EnableAutoRecovery enables automatic recovery attempts
	EnableAutoRecovery bool

	// MaxRecoveryAttempts limits automatic recovery attempts
	MaxRecoveryAttempts int

	// RecoveryBackoff is the delay between recovery attempts
	RecoveryBackoff time.Duration

	// Logger for recovery events
	Logger *utils.StructuredLogger

	// StatusTracker for operation status reporting
	StatusTracker *status.Tracker
}

// DefaultRecoveryConfig returns sensible defaults
func DefaultRecoveryConfig() RecoveryConfig {
	return RecoveryConfig{
		DefaultStrategy:     StrategyRetry,
		RetryConfig:         retry.DefaultConfig(),
		EnableAutoRecovery:  true,
		MaxRecoveryAttempts: 3,
		RecoveryBackoff:     5 * time.Second,
		CircuitBreakerConfig: circuit.Config{
			MaxRequests: 5,
			Interval:    30 * time.Second,
			Timeout:     60 * time.Second,
		},
	}
}

// RecoveryManager manages error recovery and graceful degradation
type RecoveryManager struct {
	config   RecoveryConfig
	retryer  *retry.Retryer
	breakers *circuit.Manager
	logger   *utils.StructuredLogger

	mu                 sync.RWMutex
	recoveryAttempts   map[string]int
	degradedComponents map[string]*DegradedState
	fallbackFunctions  map[string]FallbackFunc
}

// DegradedState tracks degraded component state
type DegradedState struct {
	Component     string
	Reason        string
	Since         time.Time
	AttemptCount  int
	LastAttempt   time.Time
	NextAttempt   time.Time
	OriginalError *errors.ObjectFSError
}

// FallbackFunc is a fallback function for an operation
type FallbackFunc func(ctx context.Context) (interface{}, error)

// RecoveryResult represents the result of a recovery attempt
type RecoveryResult struct {
	Success     bool
	Strategy    RecoveryStrategy
	Attempts    int
	Duration    time.Duration
	Error       error
	Recoverable bool
}

// NewRecoveryManager creates a new recovery manager
func NewRecoveryManager(config RecoveryConfig) *RecoveryManager {
	if config.Logger == nil {
		loggerConfig := utils.DefaultStructuredLoggerConfig()
		logger, _ := utils.NewStructuredLogger(loggerConfig)
		config.Logger = logger
	}

	return &RecoveryManager{
		config:             config,
		retryer:            retry.New(config.RetryConfig),
		breakers:           circuit.NewManager(config.CircuitBreakerConfig),
		logger:             config.Logger,
		recoveryAttempts:   make(map[string]int),
		degradedComponents: make(map[string]*DegradedState),
		fallbackFunctions:  make(map[string]FallbackFunc),
	}
}

// Execute executes an operation with automatic error recovery
func (rm *RecoveryManager) Execute(ctx context.Context, component string, operation string, fn func() error) error {
	_, err := rm.ExecuteWithResult(ctx, component, operation, func() (interface{}, error) {
		return nil, fn()
	})
	return err
}

// ExecuteWithResult executes an operation and returns its result with recovery
func (rm *RecoveryManager) ExecuteWithResult(ctx context.Context, component string, operation string, fn func() (interface{}, error)) (interface{}, error) {
	opKey := fmt.Sprintf("%s:%s", component, operation)

	// Check if component is degraded
	if rm.isComponentDegraded(component) {
		if fallback := rm.getFallback(opKey); fallback != nil {
			rm.logger.Info("Using fallback for degraded component",
				map[string]interface{}{
					"component": component,
					"operation": operation,
				})
			return fallback(ctx)
		}
		return nil, errors.NewError(errors.ErrCodeServiceDegraded,
			fmt.Sprintf("component %s is in degraded state", component)).
			WithComponent(component).
			WithOperation(operation)
	}

	// Determine recovery strategy
	strategy := rm.determineStrategy(component, operation)

	// Execute with appropriate strategy
	switch strategy {
	case StrategyRetry:
		return rm.executeWithRetry(ctx, component, operation, fn)
	case StrategyCircuitBreaker:
		return rm.executeWithCircuitBreaker(ctx, component, operation, fn)
	case StrategyGracefulDegradation:
		return rm.executeWithDegradation(ctx, component, operation, fn)
	case StrategyFallback:
		return rm.executeWithFallback(ctx, component, operation, fn)
	case StrategyFailFast:
		return fn()
	default:
		return fn()
	}
}

// executeWithRetry executes with retry logic
func (rm *RecoveryManager) executeWithRetry(ctx context.Context, component string, operation string, fn func() (interface{}, error)) (interface{}, error) {
	var result interface{}

	err := rm.retryer.DoWithContext(ctx, func(ctx context.Context) error {
		var err error
		result, err = fn()
		return err
	})

	if err != nil {
		rm.handleFailure(component, operation, err)
		return nil, rm.enhanceError(err, component, operation, "retry exhausted")
	}

	rm.handleSuccess(component, operation)
	return result, nil
}

// executeWithCircuitBreaker executes with circuit breaker protection
func (rm *RecoveryManager) executeWithCircuitBreaker(ctx context.Context, component string, operation string, fn func() (interface{}, error)) (interface{}, error) {
	breaker := rm.breakers.GetBreaker(component)

	var result interface{}
	var fnErr error

	err := breaker.ExecuteWithContext(ctx, func(ctx context.Context) error {
		var err error
		result, err = fn()
		fnErr = err
		return err
	})

	if err != nil {
		// Check if circuit breaker is open
		if err == circuit.ErrOpenState {
			rm.markDegraded(component, operation, fmt.Errorf("circuit breaker open"))
			rm.logger.Warn("Circuit breaker open", map[string]interface{}{
				"component": component,
				"operation": operation,
			})
			return nil, errors.NewError(errors.ErrCodeServiceDegraded,
				"service temporarily unavailable due to repeated failures").
				WithComponent(component).
				WithOperation(operation).
				WithCause(err)
		}
		rm.handleFailure(component, operation, err)
		return nil, rm.enhanceError(fnErr, component, operation, "circuit breaker triggered")
	}

	rm.handleSuccess(component, operation)
	return result, nil
}

// executeWithDegradation executes with graceful degradation
func (rm *RecoveryManager) executeWithDegradation(ctx context.Context, component string, operation string, fn func() (interface{}, error)) (interface{}, error) {
	result, err := fn()
	if err != nil {
		rm.markDegraded(component, operation, err)

		// Try fallback if available
		opKey := fmt.Sprintf("%s:%s", component, operation)
		if fallback := rm.getFallback(opKey); fallback != nil {
			rm.logger.Info("Using fallback due to error", map[string]interface{}{
				"component": component,
				"operation": operation,
				"error":     err.Error(),
			})
			return fallback(ctx)
		}

		return nil, rm.enhanceError(err, component, operation, "operating in degraded mode")
	}

	rm.handleSuccess(component, operation)
	return result, nil
}

// executeWithFallback executes with fallback function
func (rm *RecoveryManager) executeWithFallback(ctx context.Context, component string, operation string, fn func() (interface{}, error)) (interface{}, error) {
	result, err := fn()
	if err != nil {
		opKey := fmt.Sprintf("%s:%s", component, operation)
		if fallback := rm.getFallback(opKey); fallback != nil {
			rm.logger.Info("Primary operation failed, using fallback", map[string]interface{}{
				"component": component,
				"operation": operation,
			})
			return fallback(ctx)
		}
		return nil, rm.enhanceError(err, component, operation, "no fallback available")
	}
	return result, nil
}

// RegisterFallback registers a fallback function for an operation
func (rm *RecoveryManager) RegisterFallback(component string, operation string, fallback FallbackFunc) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	opKey := fmt.Sprintf("%s:%s", component, operation)
	rm.fallbackFunctions[opKey] = fallback
}

// getFallback retrieves a fallback function
func (rm *RecoveryManager) getFallback(opKey string) FallbackFunc {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.fallbackFunctions[opKey]
}

// markDegraded marks a component as degraded
func (rm *RecoveryManager) markDegraded(component string, operation string, err error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	state := rm.degradedComponents[component]
	if state == nil {
		state = &DegradedState{
			Component: component,
			Since:     time.Now(),
		}
		rm.degradedComponents[component] = state
	}

	state.Reason = fmt.Sprintf("%s: %v", operation, err)
	state.AttemptCount++
	state.LastAttempt = time.Now()
	state.NextAttempt = time.Now().Add(rm.config.RecoveryBackoff)

	if objErr, ok := err.(*errors.ObjectFSError); ok {
		state.OriginalError = objErr
	}

	rm.logger.Warn("Component marked as degraded", map[string]interface{}{
		"component": component,
		"reason":    state.Reason,
		"attempts":  state.AttemptCount,
	})

	// Start auto-recovery if enabled
	if rm.config.EnableAutoRecovery && state.AttemptCount <= rm.config.MaxRecoveryAttempts {
		go rm.attemptAutoRecovery(component)
	}
}

// isComponentDegraded checks if a component is in degraded state
func (rm *RecoveryManager) isComponentDegraded(component string) bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.degradedComponents[component] != nil
}

// attemptAutoRecovery attempts to automatically recover a degraded component
func (rm *RecoveryManager) attemptAutoRecovery(component string) {
	rm.mu.RLock()
	state := rm.degradedComponents[component]
	if state == nil {
		rm.mu.RUnlock()
		return
	}
	nextAttempt := state.NextAttempt
	rm.mu.RUnlock()

	// Wait until next attempt time
	time.Sleep(time.Until(nextAttempt))

	rm.logger.Info("Attempting automatic recovery", map[string]interface{}{
		"component": component,
		"attempt":   state.AttemptCount + 1,
	})

	// For now, just reset the circuit breaker for this component
	breaker := rm.breakers.GetBreaker(component)
	breaker.Reset()

	// Mark component as recovered
	rm.mu.Lock()
	delete(rm.degradedComponents, component)
	rm.mu.Unlock()

	rm.logger.Info("Component recovered", map[string]interface{}{
		"component": component,
	})
}

// RecoverComponent manually recovers a degraded component
func (rm *RecoveryManager) RecoverComponent(component string) error {
	rm.mu.Lock()
	state := rm.degradedComponents[component]
	if state == nil {
		rm.mu.Unlock()
		return errors.NewError(errors.ErrCodeInvalidState, "component not in degraded state").
			WithComponent(component)
	}
	delete(rm.degradedComponents, component)
	rm.mu.Unlock()

	// Reset circuit breaker
	breaker := rm.breakers.GetBreaker(component)
	breaker.Reset()

	rm.logger.Info("Component manually recovered", map[string]interface{}{
		"component": component,
	})

	return nil
}

// GetDegradedComponents returns all degraded components
func (rm *RecoveryManager) GetDegradedComponents() map[string]*DegradedState {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	result := make(map[string]*DegradedState, len(rm.degradedComponents))
	for k, v := range rm.degradedComponents {
		stateCopy := *v
		result[k] = &stateCopy
	}
	return result
}

// GetCircuitBreakerStats returns circuit breaker statistics
func (rm *RecoveryManager) GetCircuitBreakerStats() map[string]circuit.CircuitBreakerStats {
	return rm.breakers.GetStats()
}

// determineStrategy determines the recovery strategy for an operation
func (rm *RecoveryManager) determineStrategy(component string, operation string) RecoveryStrategy {
	// Check if component has had recent failures
	rm.mu.RLock()
	attemptCount := rm.recoveryAttempts[component]
	rm.mu.RUnlock()

	// Use circuit breaker after repeated failures
	if attemptCount >= 3 {
		return StrategyCircuitBreaker
	}

	// Default to retry for network and storage errors
	if component == "s3" || component == "storage" || component == "network" {
		return StrategyRetry
	}

	return rm.config.DefaultStrategy
}

// handleSuccess records a successful operation
func (rm *RecoveryManager) handleSuccess(component string, operation string) {
	rm.mu.Lock()
	delete(rm.recoveryAttempts, component)
	rm.mu.Unlock()
}

// handleFailure records a failed operation
func (rm *RecoveryManager) handleFailure(component string, operation string, err error) {
	rm.mu.Lock()
	rm.recoveryAttempts[component]++
	attempts := rm.recoveryAttempts[component]
	rm.mu.Unlock()

	rm.logger.Error("Operation failed", map[string]interface{}{
		"component": component,
		"operation": operation,
		"attempts":  attempts,
		"error":     err.Error(),
	})
}

// enhanceError adds recovery context to an error
func (rm *RecoveryManager) enhanceError(err error, component string, operation string, context string) error {
	if objErr, ok := err.(*errors.ObjectFSError); ok {
		return objErr.
			WithComponent(component).
			WithOperation(operation).
			WithContext("recovery_context", context)
	}

	return errors.NewError(errors.ErrCodeOperationFailed, err.Error()).
		WithComponent(component).
		WithOperation(operation).
		WithCause(err).
		WithContext("recovery_context", context)
}

// GetRecoveryStats returns recovery statistics
func (rm *RecoveryManager) GetRecoveryStats() RecoveryStats {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	return RecoveryStats{
		DegradedComponents: len(rm.degradedComponents),
		ActiveRecoveries:   rm.countActiveRecoveries(),
		CircuitBreakers:    rm.breakers.GetStats(),
		TotalAttempts:      rm.sumRecoveryAttempts(),
	}
}

// RecoveryStats provides recovery statistics
type RecoveryStats struct {
	DegradedComponents int                                    `json:"degraded_components"`
	ActiveRecoveries   int                                    `json:"active_recoveries"`
	CircuitBreakers    map[string]circuit.CircuitBreakerStats `json:"circuit_breakers"`
	TotalAttempts      int                                    `json:"total_attempts"`
}

// countActiveRecoveries counts components with active recovery attempts
func (rm *RecoveryManager) countActiveRecoveries() int {
	count := 0
	for _, state := range rm.degradedComponents {
		if state.NextAttempt.After(time.Now()) {
			count++
		}
	}
	return count
}

// sumRecoveryAttempts sums all recovery attempts
func (rm *RecoveryManager) sumRecoveryAttempts() int {
	total := 0
	for _, count := range rm.recoveryAttempts {
		total += count
	}
	return total
}

// Shutdown gracefully shuts down the recovery manager
func (rm *RecoveryManager) Shutdown(ctx context.Context) error {
	rm.logger.Info("Recovery manager shutting down", nil)

	// Close logger if it has a Close method
	if closer, ok := interface{}(rm.logger).(interface{ Close() error }); ok {
		return closer.Close()
	}

	return nil
}
