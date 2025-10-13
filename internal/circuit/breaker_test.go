package circuit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestState_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state State
		want  string
	}{
		{"Closed state", StateClosed, "CLOSED"},
		{"Open state", StateOpen, "OPEN"},
		{"Half-open state", StateHalfOpen, "HALF_OPEN"},
		{"Unknown state", State(999), "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.state.String()
			if result != tt.want {
				t.Errorf("State.String() = %q, want %q", result, tt.want)
			}
		})
	}
}

func TestNewCircuitBreaker_Defaults(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker("test", Config{})

	if cb.name != "test" {
		t.Errorf("name = %q, want %q", cb.name, "test")
	}
	if cb.state != StateClosed {
		t.Errorf("initial state = %v, want %v", cb.state, StateClosed)
	}
	if cb.config.MaxRequests != 1 {
		t.Errorf("default MaxRequests = %d, want 1", cb.config.MaxRequests)
	}
	if cb.config.Interval != 60*time.Second {
		t.Errorf("default Interval = %v, want %v", cb.config.Interval, 60*time.Second)
	}
	if cb.config.Timeout != 60*time.Second {
		t.Errorf("default Timeout = %v, want %v", cb.config.Timeout, 60*time.Second)
	}
	if cb.config.ReadyToTrip == nil {
		t.Error("default ReadyToTrip should not be nil")
	}
	if cb.config.IsSuccessful == nil {
		t.Error("default IsSuccessful should not be nil")
	}
}

func TestNewCircuitBreaker_CustomConfig(t *testing.T) {
	t.Parallel()

	config := Config{
		MaxRequests: 5,
		Interval:    10 * time.Second,
		Timeout:     30 * time.Second,
	}

	cb := NewCircuitBreaker("custom", config)

	if cb.config.MaxRequests != 5 {
		t.Errorf("MaxRequests = %d, want 5", cb.config.MaxRequests)
	}
	if cb.config.Interval != 10*time.Second {
		t.Errorf("Interval = %v, want %v", cb.config.Interval, 10*time.Second)
	}
	if cb.config.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want %v", cb.config.Timeout, 30*time.Second)
	}
}

func TestDefaultReadyToTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		counts   Counts
		wantTrip bool
	}{
		{
			name:     "not enough requests",
			counts:   Counts{Requests: 10, TotalFailures: 5},
			wantTrip: false,
		},
		{
			name:     "enough requests but low failure rate",
			counts:   Counts{Requests: 20, TotalFailures: 8},
			wantTrip: false,
		},
		{
			name:     "should trip - 50% failure threshold",
			counts:   Counts{Requests: 20, TotalFailures: 10},
			wantTrip: true,
		},
		{
			name:     "should trip - above threshold",
			counts:   Counts{Requests: 100, TotalFailures: 60},
			wantTrip: true,
		},
		{
			name:     "zero requests",
			counts:   Counts{Requests: 0, TotalFailures: 0},
			wantTrip: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := defaultReadyToTrip(tt.counts)
			if result != tt.wantTrip {
				t.Errorf("defaultReadyToTrip() = %v, want %v", result, tt.wantTrip)
			}
		})
	}
}

func TestDefaultIsSuccessful(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error is successful", nil, true},
		{"non-nil error is not successful", errors.New("test error"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := defaultIsSuccessful(tt.err)
			if result != tt.want {
				t.Errorf("defaultIsSuccessful() = %v, want %v", result, tt.want)
			}
		})
	}
}

func TestCircuitBreaker_Execute_Success(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker("test", Config{
		MaxRequests: 1,
		Interval:    time.Minute,
		Timeout:     time.Minute,
	})

	callCount := 0
	err := cb.Execute(func() error {
		callCount++
		return nil
	})

	if err != nil {
		t.Errorf("Execute() error = %v, want nil", err)
	}
	if callCount != 1 {
		t.Errorf("function called %d times, want 1", callCount)
	}

	counts := cb.GetCounts()
	if counts.Requests != 1 {
		t.Errorf("Requests = %d, want 1", counts.Requests)
	}
	if counts.TotalSuccesses != 1 {
		t.Errorf("TotalSuccesses = %d, want 1", counts.TotalSuccesses)
	}
}

func TestCircuitBreaker_Execute_Failure(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker("test", Config{
		MaxRequests: 1,
		Interval:    time.Minute,
		Timeout:     time.Minute,
	})

	testErr := errors.New("test failure")
	err := cb.Execute(func() error {
		return testErr
	})

	if err != testErr {
		t.Errorf("Execute() error = %v, want %v", err, testErr)
	}

	counts := cb.GetCounts()
	if counts.TotalFailures != 1 {
		t.Errorf("TotalFailures = %d, want 1", counts.TotalFailures)
	}
}

func TestCircuitBreaker_StateTransitions(t *testing.T) {
	t.Parallel()

	stateChanges := []string{}
	var mu sync.Mutex

	cb := NewCircuitBreaker("test", Config{
		MaxRequests: 2,
		Interval:    100 * time.Millisecond,
		Timeout:     100 * time.Millisecond,
		ReadyToTrip: func(counts Counts) bool {
			// Trip after 3 consecutive failures
			return counts.ConsecutiveFailures >= 3
		},
		OnStateChange: func(name string, from State, to State) {
			mu.Lock()
			defer mu.Unlock()
			stateChanges = append(stateChanges, from.String()+"->"+to.String())
		},
	})

	// Initial state should be closed
	if cb.GetState() != StateClosed {
		t.Errorf("initial state = %v, want %v", cb.GetState(), StateClosed)
	}

	// Cause 3 failures to trip the breaker
	for i := 0; i < 3; i++ {
		_ = cb.Execute(func() error {
			return errors.New("failure")
		})
	}

	// Should now be open
	if cb.GetState() != StateOpen {
		t.Errorf("state after failures = %v, want %v", cb.GetState(), StateOpen)
	}

	// Wait for timeout to transition to half-open
	time.Sleep(150 * time.Millisecond)

	// Check state - should be half-open now
	if cb.GetState() != StateHalfOpen {
		t.Errorf("state after timeout = %v, want %v", cb.GetState(), StateHalfOpen)
	}

	// Successful request in half-open should close the breaker
	err := cb.Execute(func() error {
		return nil
	})
	if err != nil {
		t.Errorf("Execute in half-open failed: %v", err)
	}

	if cb.GetState() != StateClosed {
		t.Errorf("state after success in half-open = %v, want %v", cb.GetState(), StateClosed)
	}

	// Verify state transitions were recorded
	mu.Lock()
	defer mu.Unlock()
	if len(stateChanges) < 2 {
		t.Errorf("expected at least 2 state changes, got %d: %v", len(stateChanges), stateChanges)
	}
}

func TestCircuitBreaker_OpenState_RejectsRequests(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker("test", Config{
		MaxRequests: 1,
		Interval:    time.Minute,
		Timeout:     time.Minute,
		ReadyToTrip: func(counts Counts) bool {
			return counts.ConsecutiveFailures >= 2
		},
	})

	// Cause 2 failures to open the breaker
	for i := 0; i < 2; i++ {
		_ = cb.Execute(func() error {
			return errors.New("failure")
		})
	}

	// Next request should be rejected
	callCount := 0
	err := cb.Execute(func() error {
		callCount++
		return nil
	})

	if err != ErrOpenState {
		t.Errorf("Execute() error = %v, want %v", err, ErrOpenState)
	}
	if callCount != 0 {
		t.Error("function should not have been called when circuit is open")
	}
}

func TestCircuitBreaker_HalfOpen_TooManyRequests(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker("test", Config{
		MaxRequests: 1,
		Interval:    50 * time.Millisecond,
		Timeout:     50 * time.Millisecond,
		ReadyToTrip: func(counts Counts) bool {
			return counts.ConsecutiveFailures >= 1
		},
	})

	// Open the breaker
	_ = cb.Execute(func() error {
		return errors.New("failure")
	})

	// Wait for half-open
	time.Sleep(100 * time.Millisecond)

	// Use channel to ensure both requests are attempted concurrently
	started := make(chan struct{})
	done := make(chan struct{})

	// Start first request
	go func() {
		_ = cb.Execute(func() error {
			close(started)
			<-done // Block until test releases it
			return nil
		})
	}()

	// Wait for first request to be accepted
	<-started

	// Second request should be rejected while first is in flight
	err2 := cb.Execute(func() error {
		return nil
	})

	// Let first request complete
	close(done)

	if err2 != ErrTooManyRequests {
		t.Errorf("second request error = %v, want %v", err2, ErrTooManyRequests)
	}
}

func TestCircuitBreaker_ExecuteWithFallback(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker("test", Config{
		MaxRequests: 1,
		Interval:    time.Minute,
		Timeout:     time.Minute,
		ReadyToTrip: func(counts Counts) bool {
			return counts.ConsecutiveFailures >= 1
		},
	})

	// Open the breaker
	_ = cb.Execute(func() error {
		return errors.New("failure")
	})

	// Execute with fallback
	fallbackCalled := false
	err, usedFallback := cb.ExecuteWithFallback(
		func() error {
			return nil
		},
		func() error {
			fallbackCalled = true
			return nil
		},
	)

	if err != nil {
		t.Errorf("ExecuteWithFallback() error = %v, want nil", err)
	}
	if !usedFallback {
		t.Error("usedFallback = false, want true")
	}
	if !fallbackCalled {
		t.Error("fallback function was not called")
	}
}

func TestCircuitBreaker_ExecuteWithContext(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker("test", Config{
		MaxRequests: 1,
		Interval:    time.Minute,
		Timeout:     time.Minute,
	})

	ctx := context.Background()
	ctxReceived := false

	err := cb.ExecuteWithContext(ctx, func(receivedCtx context.Context) error {
		if receivedCtx == ctx {
			ctxReceived = true
		}
		return nil
	})

	if err != nil {
		t.Errorf("ExecuteWithContext() error = %v, want nil", err)
	}
	if !ctxReceived {
		t.Error("context was not passed to function")
	}
}

func TestCircuitBreaker_Reset(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker("test", Config{
		MaxRequests: 1,
		Interval:    time.Minute,
		Timeout:     time.Minute,
		ReadyToTrip: func(counts Counts) bool {
			return counts.ConsecutiveFailures >= 1
		},
	})

	// Open the breaker
	_ = cb.Execute(func() error {
		return errors.New("failure")
	})

	if cb.GetState() != StateOpen {
		t.Errorf("state = %v, want %v", cb.GetState(), StateOpen)
	}

	// Reset
	cb.Reset()

	if cb.GetState() != StateClosed {
		t.Errorf("state after reset = %v, want %v", cb.GetState(), StateClosed)
	}

	counts := cb.GetCounts()
	if counts.Requests != 0 {
		t.Errorf("Requests after reset = %d, want 0", counts.Requests)
	}
	if counts.TotalFailures != 0 {
		t.Errorf("TotalFailures after reset = %d, want 0", counts.TotalFailures)
	}
}

func TestCircuitBreaker_Name(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker("my-breaker", Config{})
	if cb.Name() != "my-breaker" {
		t.Errorf("Name() = %q, want %q", cb.Name(), "my-breaker")
	}
}

func TestCounts_Operations(t *testing.T) {
	t.Parallel()

	counts := Counts{}

	// Test onRequest
	counts.onRequest()
	if counts.Requests != 1 {
		t.Errorf("Requests = %d, want 1", counts.Requests)
	}
	if counts.LastActivity.IsZero() {
		t.Error("LastActivity not set after onRequest")
	}

	// Test onSuccess
	counts.onSuccess()
	if counts.TotalSuccesses != 1 {
		t.Errorf("TotalSuccesses = %d, want 1", counts.TotalSuccesses)
	}
	if counts.ConsecutiveSuccesses != 1 {
		t.Errorf("ConsecutiveSuccesses = %d, want 1", counts.ConsecutiveSuccesses)
	}
	if counts.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", counts.ConsecutiveFailures)
	}

	// Test onFailure
	counts.onFailure()
	if counts.TotalFailures != 1 {
		t.Errorf("TotalFailures = %d, want 1", counts.TotalFailures)
	}
	if counts.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", counts.ConsecutiveFailures)
	}
	if counts.ConsecutiveSuccesses != 0 {
		t.Errorf("ConsecutiveSuccesses = %d, want 0 after failure", counts.ConsecutiveSuccesses)
	}

	// Test clear
	counts.clear()
	if counts.Requests != 0 || counts.TotalSuccesses != 0 || counts.TotalFailures != 0 {
		t.Error("counts not properly cleared")
	}
	if !counts.LastActivity.IsZero() {
		t.Error("LastActivity not cleared")
	}
}

func TestNewManager(t *testing.T) {
	t.Parallel()

	config := Config{
		MaxRequests: 5,
		Interval:    10 * time.Second,
		Timeout:     30 * time.Second,
	}

	manager := NewManager(config)

	if manager == nil {
		t.Fatal("NewManager returned nil")
	}
	if manager.breakers == nil {
		t.Error("breakers map is nil")
	}
	if manager.config.MaxRequests != 5 {
		t.Errorf("config.MaxRequests = %d, want 5", manager.config.MaxRequests)
	}
}

func TestManager_GetBreaker(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{})

	// Get new breaker
	cb1 := manager.GetBreaker("test1")
	if cb1 == nil {
		t.Fatal("GetBreaker returned nil")
	}
	if cb1.Name() != "test1" {
		t.Errorf("breaker name = %q, want %q", cb1.Name(), "test1")
	}

	// Get same breaker again
	cb2 := manager.GetBreaker("test1")
	if cb1 != cb2 {
		t.Error("GetBreaker returned different instance for same name")
	}

	// Get different breaker
	cb3 := manager.GetBreaker("test2")
	if cb3 == cb1 {
		t.Error("GetBreaker returned same instance for different name")
	}
}

func TestManager_GetAllBreakers(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{})

	manager.GetBreaker("breaker1")
	manager.GetBreaker("breaker2")
	manager.GetBreaker("breaker3")

	all := manager.GetAllBreakers()
	if len(all) != 3 {
		t.Errorf("GetAllBreakers() returned %d breakers, want 3", len(all))
	}

	if _, exists := all["breaker1"]; !exists {
		t.Error("breaker1 not found in GetAllBreakers")
	}
	if _, exists := all["breaker2"]; !exists {
		t.Error("breaker2 not found in GetAllBreakers")
	}
	if _, exists := all["breaker3"]; !exists {
		t.Error("breaker3 not found in GetAllBreakers")
	}
}

func TestManager_RemoveBreaker(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{})

	manager.GetBreaker("test")
	all := manager.GetAllBreakers()
	if len(all) != 1 {
		t.Fatalf("setup failed: expected 1 breaker, got %d", len(all))
	}

	manager.RemoveBreaker("test")
	all = manager.GetAllBreakers()
	if len(all) != 0 {
		t.Errorf("after RemoveBreaker, expected 0 breakers, got %d", len(all))
	}
}

func TestManager_ResetAll(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{
		ReadyToTrip: func(counts Counts) bool {
			return counts.ConsecutiveFailures >= 1
		},
	})

	// Create breakers and open them
	cb1 := manager.GetBreaker("test1")
	cb2 := manager.GetBreaker("test2")

	_ = cb1.Execute(func() error { return errors.New("fail") })
	_ = cb2.Execute(func() error { return errors.New("fail") })

	if cb1.GetState() != StateOpen {
		t.Error("cb1 should be open")
	}
	if cb2.GetState() != StateOpen {
		t.Error("cb2 should be open")
	}

	// Reset all
	manager.ResetAll()

	if cb1.GetState() != StateClosed {
		t.Errorf("cb1 state after ResetAll = %v, want %v", cb1.GetState(), StateClosed)
	}
	if cb2.GetState() != StateClosed {
		t.Errorf("cb2 state after ResetAll = %v, want %v", cb2.GetState(), StateClosed)
	}
}

func TestManager_GetStats(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{})

	cb1 := manager.GetBreaker("breaker1")
	cb2 := manager.GetBreaker("breaker2")

	_ = cb1.Execute(func() error { return nil })
	_ = cb2.Execute(func() error { return errors.New("fail") })

	stats := manager.GetStats()

	if len(stats) != 2 {
		t.Errorf("GetStats() returned %d entries, want 2", len(stats))
	}

	stat1, exists := stats["breaker1"]
	if !exists {
		t.Fatal("breaker1 stats not found")
	}
	if stat1.Name != "breaker1" {
		t.Errorf("stat1.Name = %q, want %q", stat1.Name, "breaker1")
	}
	if stat1.Counts.TotalSuccesses != 1 {
		t.Errorf("stat1 successes = %d, want 1", stat1.Counts.TotalSuccesses)
	}

	stat2, exists := stats["breaker2"]
	if !exists {
		t.Fatal("breaker2 stats not found")
	}
	if stat2.Counts.TotalFailures != 1 {
		t.Errorf("stat2 failures = %d, want 1", stat2.Counts.TotalFailures)
	}
}

func TestManager_HealthCheck(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{
		ReadyToTrip: func(counts Counts) bool {
			return counts.ConsecutiveFailures >= 1
		},
	})

	// All closed - should pass
	cb1 := manager.GetBreaker("test1")
	_ = cb1.Execute(func() error { return nil })

	err := manager.HealthCheck()
	if err != nil {
		t.Errorf("HealthCheck() with closed breakers error = %v, want nil", err)
	}

	// Open one breaker - should fail
	_ = cb1.Execute(func() error { return errors.New("fail") })

	err = manager.HealthCheck()
	if err == nil {
		t.Error("HealthCheck() with open breaker should return error")
	}
}

func TestManager_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			name := "breaker-concurrent"
			cb := manager.GetBreaker(name)
			_ = cb.Execute(func() error {
				time.Sleep(time.Millisecond)
				return nil
			})
		}(i)
	}

	wg.Wait()

	// Verify only one breaker was created
	all := manager.GetAllBreakers()
	if len(all) != 1 {
		t.Errorf("concurrent access created %d breakers, want 1", len(all))
	}
}
