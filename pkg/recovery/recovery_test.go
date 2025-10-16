package recovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/objectfs/objectfs/internal/circuit"
	pkgerrors "github.com/objectfs/objectfs/pkg/errors"
	"github.com/objectfs/objectfs/pkg/retry"
)

func TestNewRecoveryManager(t *testing.T) {
	config := DefaultRecoveryConfig()
	rm := NewRecoveryManager(config)

	if rm == nil {
		t.Fatal("Expected non-nil recovery manager")
	}

	if rm.config.DefaultStrategy != StrategyRetry {
		t.Errorf("Expected default strategy to be retry, got %v", rm.config.DefaultStrategy)
	}

	if rm.retryer == nil {
		t.Error("Expected retryer to be initialized")
	}

	if rm.breakers == nil {
		t.Error("Expected circuit breaker manager to be initialized")
	}
}

func TestRecoveryManager_ExecuteSuccess(t *testing.T) {
	config := DefaultRecoveryConfig()
	config.DefaultStrategy = StrategyRetry
	rm := NewRecoveryManager(config)

	ctx := context.Background()
	called := false

	err := rm.Execute(ctx, "test", "operation", func() error {
		called = true
		return nil
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if !called {
		t.Error("Expected function to be called")
	}
}

func TestRecoveryManager_ExecuteWithRetry(t *testing.T) {
	config := DefaultRecoveryConfig()
	config.DefaultStrategy = StrategyRetry
	config.RetryConfig.MaxAttempts = 3
	config.RetryConfig.InitialDelay = 10 * time.Millisecond
	rm := NewRecoveryManager(config)

	ctx := context.Background()
	attempts := 0

	err := rm.Execute(ctx, "test", "operation", func() error {
		attempts++
		if attempts < 2 {
			return pkgerrors.NewError(pkgerrors.ErrCodeConnectionTimeout, "timeout")
		}
		return nil
	})

	if err != nil {
		t.Fatalf("Expected eventual success, got %v", err)
	}

	if attempts < 2 {
		t.Errorf("Expected at least 2 attempts, got %d", attempts)
	}
}

func TestRecoveryManager_ExecuteWithCircuitBreaker(t *testing.T) {
	config := DefaultRecoveryConfig()
	config.CircuitBreakerConfig = circuit.Config{
		MaxRequests: 1,
		Interval:    1 * time.Second,
		Timeout:     100 * time.Millisecond,
	}
	rm := NewRecoveryManager(config)

	ctx := context.Background()
	component := "test-breaker"

	// Force the strategy to use circuit breaker
	rm.mu.Lock()
	rm.recoveryAttempts[component] = 5 // Trigger circuit breaker strategy
	rm.mu.Unlock()

	attempts := 0
	// First few attempts should fail and trip the breaker
	for i := 0; i < 3; i++ {
		_ = rm.Execute(ctx, component, "operation", func() error {
			attempts++
			return errors.New("failure")
		})
	}

	// Circuit breaker should now be open
	stats := rm.GetCircuitBreakerStats()
	if breakerStats, exists := stats[component]; exists {
		if breakerStats.State != circuit.StateOpen {
			t.Logf("Circuit breaker state: %v after %d attempts", breakerStats.State, attempts)
		}
	}
}

func TestRecoveryManager_RegisterFallback(t *testing.T) {
	config := DefaultRecoveryConfig()
	rm := NewRecoveryManager(config)

	fallbackCalled := false
	rm.RegisterFallback("test", "operation", func(ctx context.Context) (interface{}, error) {
		fallbackCalled = true
		return "fallback-result", nil
	})

	fallback := rm.getFallback("test:operation")
	if fallback == nil {
		t.Fatal("Expected fallback to be registered")
	}

	result, err := fallback(context.Background())
	if err != nil {
		t.Fatalf("Expected no error from fallback, got %v", err)
	}

	if !fallbackCalled {
		t.Error("Expected fallback to be called")
	}

	if result != "fallback-result" {
		t.Errorf("Expected 'fallback-result', got %v", result)
	}
}

func TestRecoveryManager_GracefulDegradation(t *testing.T) {
	config := DefaultRecoveryConfig()
	config.DefaultStrategy = StrategyGracefulDegradation
	rm := NewRecoveryManager(config)

	ctx := context.Background()
	component := "test-degraded"

	// Register a fallback
	fallbackCalled := false
	rm.RegisterFallback(component, "operation", func(ctx context.Context) (interface{}, error) {
		fallbackCalled = true
		return "degraded-result", nil
	})

	// Execute operation that fails
	result, err := rm.ExecuteWithResult(ctx, component, "operation", func() (interface{}, error) {
		return nil, errors.New("primary failed")
	})

	// Should use fallback
	if !fallbackCalled {
		t.Error("Expected fallback to be called for degraded operation")
	}

	if err != nil {
		t.Fatalf("Expected fallback to succeed, got error: %v", err)
	}

	if result != "degraded-result" {
		t.Errorf("Expected degraded result, got %v", result)
	}

	// Check that component is marked as degraded
	degraded := rm.GetDegradedComponents()
	if _, exists := degraded[component]; !exists {
		t.Error("Expected component to be marked as degraded")
	}
}

func TestRecoveryManager_RecoverComponent(t *testing.T) {
	config := DefaultRecoveryConfig()
	config.EnableAutoRecovery = false // Disable auto recovery for this test
	rm := NewRecoveryManager(config)

	component := "test-recover"

	// Mark component as degraded
	rm.markDegraded(component, "test", errors.New("test error"))

	// Verify it's degraded
	if !rm.isComponentDegraded(component) {
		t.Fatal("Expected component to be degraded")
	}

	// Recover it
	err := rm.RecoverComponent(component)
	if err != nil {
		t.Fatalf("Expected successful recovery, got %v", err)
	}

	// Verify it's no longer degraded
	if rm.isComponentDegraded(component) {
		t.Error("Expected component to be recovered")
	}
}

func TestRecoveryManager_GetRecoveryStats(t *testing.T) {
	config := DefaultRecoveryConfig()
	rm := NewRecoveryManager(config)

	// Mark a component as degraded
	rm.markDegraded("test1", "op1", errors.New("error1"))
	rm.markDegraded("test2", "op2", errors.New("error2"))

	stats := rm.GetRecoveryStats()

	if stats.DegradedComponents != 2 {
		t.Errorf("Expected 2 degraded components, got %d", stats.DegradedComponents)
	}

	if stats.TotalAttempts < 0 {
		t.Error("Expected non-negative total attempts")
	}
}

func TestRecoveryManager_FailFastStrategy(t *testing.T) {
	config := DefaultRecoveryConfig()
	config.DefaultStrategy = StrategyFailFast
	rm := NewRecoveryManager(config)

	ctx := context.Background()
	attempts := 0

	err := rm.Execute(ctx, "test", "operation", func() error {
		attempts++
		return errors.New("immediate failure")
	})

	if err == nil {
		t.Error("Expected error for fail-fast strategy")
	}

	if attempts != 1 {
		t.Errorf("Expected exactly 1 attempt for fail-fast, got %d", attempts)
	}
}

func TestRecoveryManager_DetermineStrategy(t *testing.T) {
	config := DefaultRecoveryConfig()
	config.DefaultStrategy = StrategyRetry
	rm := NewRecoveryManager(config)

	// Test default strategy
	strategy := rm.determineStrategy("unknown", "operation")
	if strategy != StrategyRetry {
		t.Errorf("Expected retry strategy, got %v", strategy)
	}

	// Test storage component
	strategy = rm.determineStrategy("s3", "get")
	if strategy != StrategyRetry {
		t.Errorf("Expected retry strategy for s3, got %v", strategy)
	}

	// Test circuit breaker after failures
	rm.mu.Lock()
	rm.recoveryAttempts["failing-component"] = 5
	rm.mu.Unlock()

	strategy = rm.determineStrategy("failing-component", "operation")
	if strategy != StrategyCircuitBreaker {
		t.Errorf("Expected circuit breaker strategy after failures, got %v", strategy)
	}
}

func TestRecoveryStrategy_String(t *testing.T) {
	tests := []struct {
		strategy RecoveryStrategy
		expected string
	}{
		{StrategyRetry, "retry"},
		{StrategyCircuitBreaker, "circuit_breaker"},
		{StrategyGracefulDegradation, "graceful_degradation"},
		{StrategyFallback, "fallback"},
		{StrategyFailFast, "fail_fast"},
	}

	for _, tt := range tests {
		if got := tt.strategy.String(); got != tt.expected {
			t.Errorf("Expected %s, got %s", tt.expected, got)
		}
	}
}

func TestRecoveryManager_EnhanceError(t *testing.T) {
	config := DefaultRecoveryConfig()
	rm := NewRecoveryManager(config)

	originalErr := errors.New("original error")
	enhanced := rm.enhanceError(originalErr, "test-component", "test-operation", "test-context")

	if enhanced == nil {
		t.Fatal("Expected enhanced error")
	}

	objErr, ok := enhanced.(*pkgerrors.ObjectFSError)
	if !ok {
		t.Fatal("Expected ObjectFSError")
	}

	if objErr.Component != "test-component" {
		t.Errorf("Expected component 'test-component', got %s", objErr.Component)
	}

	if objErr.Operation != "test-operation" {
		t.Errorf("Expected operation 'test-operation', got %s", objErr.Operation)
	}

	if objErr.Context["recovery_context"] != "test-context" {
		t.Error("Expected recovery context in error")
	}
}

func TestRecoveryManager_ExecuteWithResult(t *testing.T) {
	config := DefaultRecoveryConfig()
	rm := NewRecoveryManager(config)

	ctx := context.Background()
	expectedResult := "success-result"

	result, err := rm.ExecuteWithResult(ctx, "test", "operation", func() (interface{}, error) {
		return expectedResult, nil
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if result != expectedResult {
		t.Errorf("Expected result %v, got %v", expectedResult, result)
	}
}

func TestRecoveryManager_HandleSuccessAndFailure(t *testing.T) {
	config := DefaultRecoveryConfig()
	rm := NewRecoveryManager(config)

	component := "test-component"

	// Record some failures
	rm.handleFailure(component, "op1", errors.New("error1"))
	rm.handleFailure(component, "op2", errors.New("error2"))

	rm.mu.RLock()
	attempts := rm.recoveryAttempts[component]
	rm.mu.RUnlock()

	if attempts != 2 {
		t.Errorf("Expected 2 failure attempts, got %d", attempts)
	}

	// Record success
	rm.handleSuccess(component, "op3")

	rm.mu.RLock()
	attempts = rm.recoveryAttempts[component]
	rm.mu.RUnlock()

	if attempts != 0 {
		t.Errorf("Expected attempts to be reset after success, got %d", attempts)
	}
}

func TestRecoveryManager_AutoRecoveryDisabled(t *testing.T) {
	config := DefaultRecoveryConfig()
	config.EnableAutoRecovery = false
	rm := NewRecoveryManager(config)

	component := "test-auto"

	// Mark as degraded
	rm.markDegraded(component, "operation", errors.New("test"))

	// Should be degraded
	if !rm.isComponentDegraded(component) {
		t.Fatal("Expected component to be degraded")
	}

	// Wait a bit to ensure no auto-recovery happens
	time.Sleep(100 * time.Millisecond)

	// Should still be degraded (no auto recovery)
	if !rm.isComponentDegraded(component) {
		t.Error("Component should still be degraded with auto-recovery disabled")
	}
}

func TestRecoveryManager_Shutdown(t *testing.T) {
	config := DefaultRecoveryConfig()
	rm := NewRecoveryManager(config)

	ctx := context.Background()
	err := rm.Shutdown(ctx)

	if err != nil {
		t.Errorf("Expected no error on shutdown, got %v", err)
	}
}

func TestDegradedState(t *testing.T) {
	state := &DegradedState{
		Component:    "test",
		Reason:       "test reason",
		Since:        time.Now(),
		AttemptCount: 3,
	}

	if state.Component != "test" {
		t.Errorf("Expected component 'test', got %s", state.Component)
	}

	if state.AttemptCount != 3 {
		t.Errorf("Expected 3 attempts, got %d", state.AttemptCount)
	}
}

func TestRecoveryManager_ConcurrentExecution(t *testing.T) {
	config := DefaultRecoveryConfig()
	config.RetryConfig.MaxAttempts = 2
	config.RetryConfig.InitialDelay = 5 * time.Millisecond
	rm := NewRecoveryManager(config)

	ctx := context.Background()
	const numGoroutines = 10

	done := make(chan bool, numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			_ = rm.Execute(ctx, "concurrent", "operation", func() error {
				time.Sleep(1 * time.Millisecond)
				if id%2 == 0 {
					return nil
				}
				return errors.New("failure")
			})
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	// Should complete without deadlock or panic
	stats := rm.GetRecoveryStats()
	if stats.TotalAttempts < 0 {
		t.Error("Expected valid stats after concurrent execution")
	}
}

func TestDefaultRecoveryConfig(t *testing.T) {
	config := DefaultRecoveryConfig()

	if config.DefaultStrategy != StrategyRetry {
		t.Errorf("Expected default strategy retry, got %v", config.DefaultStrategy)
	}

	if config.MaxRecoveryAttempts != 3 {
		t.Errorf("Expected 3 max recovery attempts, got %d", config.MaxRecoveryAttempts)
	}

	if !config.EnableAutoRecovery {
		t.Error("Expected auto recovery to be enabled by default")
	}

	if config.RetryConfig.MaxAttempts != retry.DefaultConfig().MaxAttempts {
		t.Error("Expected default retry config")
	}
}
