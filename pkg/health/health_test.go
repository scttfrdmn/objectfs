package health

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/objectfs/objectfs/pkg/errors"
)

func TestTracker_RegisterComponent(t *testing.T) {
	tracker := NewTracker(DefaultConfig())

	tracker.RegisterComponent("test-service")

	state := tracker.GetState("test-service")
	if state != StateHealthy {
		t.Errorf("Expected initial state to be StateHealthy, got %s", state)
	}
}

func TestTracker_RecordSuccess(t *testing.T) {
	tracker := NewTracker(DefaultConfig())
	tracker.RegisterComponent("test-service")

	// Record a few errors first
	tracker.RecordError("test-service", fmt.Errorf("test error"))
	tracker.RecordError("test-service", fmt.Errorf("test error"))

	// Record successes to recover
	tracker.RecordSuccess("test-service")
	tracker.RecordSuccess("test-service")

	health, err := tracker.GetComponentHealth("test-service")
	if err != nil {
		t.Fatalf("Failed to get component health: %v", err)
	}

	if health.ConsecutiveErrors != 0 {
		t.Errorf("Expected ConsecutiveErrors=0 after successes, got %d", health.ConsecutiveErrors)
	}
}

func TestTracker_RecordError_Degradation(t *testing.T) {
	config := DefaultConfig()
	config.ErrorThreshold = 3
	tracker := NewTracker(config)
	tracker.RegisterComponent("test-service")

	// Record errors below threshold
	for i := 0; i < 2; i++ {
		tracker.RecordError("test-service", fmt.Errorf("error %d", i))
	}

	state := tracker.GetState("test-service")
	if state != StateHealthy {
		t.Errorf("Expected StateHealthy before threshold, got %s", state)
	}

	// Record error that crosses threshold
	tracker.RecordError("test-service", fmt.Errorf("error 3"))

	state = tracker.GetState("test-service")
	if state != StateDegraded {
		t.Errorf("Expected StateDegraded after threshold, got %s", state)
	}
}

func TestTracker_RecordError_Unavailable(t *testing.T) {
	config := DefaultConfig()
	config.ErrorThreshold = 3
	config.UnavailableThreshold = 10
	tracker := NewTracker(config)
	tracker.RegisterComponent("test-service")

	// Record errors up to unavailable threshold
	for i := 0; i < 10; i++ {
		tracker.RecordError("test-service", fmt.Errorf("error %d", i))
	}

	state := tracker.GetState("test-service")
	if state != StateUnavailable {
		t.Errorf("Expected StateUnavailable after unavailable threshold, got %s", state)
	}
}

func TestTracker_RecordError_ReadOnly(t *testing.T) {
	config := DefaultConfig()
	config.ErrorThreshold = 3
	tracker := NewTracker(config)
	tracker.RegisterComponent("test-service")

	// Record write errors (should transition to read-only)
	writeErr := errors.NewError(errors.ErrCodeStorageWrite, "write failed")
	for i := 0; i < 3; i++ {
		tracker.RecordError("test-service", writeErr)
	}

	state := tracker.GetState("test-service")
	if state != StateReadOnly {
		t.Errorf("Expected StateReadOnly for write errors, got %s", state)
	}
}

func TestTracker_GetOverallHealth(t *testing.T) {
	tracker := NewTracker(DefaultConfig())
	tracker.RegisterComponent("service-1")
	tracker.RegisterComponent("service-2")
	tracker.RegisterComponent("service-3")

	// All healthy
	overall := tracker.GetOverallHealth()
	if overall != StateHealthy {
		t.Errorf("Expected StateHealthy with all healthy components, got %s", overall)
	}

	// One degraded
	for i := 0; i < 3; i++ {
		tracker.RecordError("service-2", fmt.Errorf("error %d", i))
	}

	overall = tracker.GetOverallHealth()
	if overall != StateDegraded {
		t.Errorf("Expected StateDegraded with one degraded component, got %s", overall)
	}

	// One unavailable
	for i := 0; i < 10; i++ {
		tracker.RecordError("service-3", fmt.Errorf("error %d", i))
	}

	overall = tracker.GetOverallHealth()
	if overall != StateUnavailable {
		t.Errorf("Expected StateUnavailable with one unavailable component, got %s", overall)
	}
}

func TestTracker_CanRead(t *testing.T) {
	tracker := NewTracker(DefaultConfig())
	tracker.RegisterComponent("test-service")

	tests := []struct {
		state    HealthState
		canRead  bool
		canWrite bool
	}{
		{StateHealthy, true, true},
		{StateDegraded, true, true},
		{StateReadOnly, true, false},
		{StateUnavailable, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.state.String(), func(t *testing.T) {
			// Set state directly for testing
			tracker.mu.Lock()
			tracker.components["test-service"].State = tt.state
			tracker.mu.Unlock()

			canRead := tracker.CanRead("test-service")
			if canRead != tt.canRead {
				t.Errorf("CanRead() = %v, want %v for state %s", canRead, tt.canRead, tt.state)
			}

			canWrite := tracker.CanWrite("test-service")
			if canWrite != tt.canWrite {
				t.Errorf("CanWrite() = %v, want %v for state %s", canWrite, tt.canWrite, tt.state)
			}
		})
	}
}

func TestTracker_StateChangeCallback(t *testing.T) {
	config := DefaultConfig()
	config.ErrorThreshold = 3
	tracker := NewTracker(config)
	tracker.RegisterComponent("test-service")

	callbackCalled := false
	var capturedOldState, capturedNewState HealthState
	var capturedComponent string

	tracker.AddStateChangeCallback(StateDegraded, func(component string, oldState, newState HealthState, err error) {
		callbackCalled = true
		capturedComponent = component
		capturedOldState = oldState
		capturedNewState = newState
	})

	// Trigger state change to degraded
	for i := 0; i < 3; i++ {
		tracker.RecordError("test-service", fmt.Errorf("error %d", i))
	}

	// Give callback time to execute (it runs in goroutine)
	time.Sleep(50 * time.Millisecond)

	if !callbackCalled {
		t.Error("State change callback was not called")
	}

	if capturedComponent != "test-service" {
		t.Errorf("Expected component='test-service', got '%s'", capturedComponent)
	}

	if capturedOldState != StateHealthy {
		t.Errorf("Expected oldState=StateHealthy, got %s", capturedOldState)
	}

	if capturedNewState != StateDegraded {
		t.Errorf("Expected newState=StateDegraded, got %s", capturedNewState)
	}
}

type testHealthListener struct {
	stateChanges []stateChange
	healthChecks []healthCheck
}

type stateChange struct {
	component string
	oldState  HealthState
	newState  HealthState
	err       error
}

type healthCheck struct {
	component string
	healthy   bool
	err       error
}

func (l *testHealthListener) OnStateChange(component string, oldState, newState HealthState, err error) {
	l.stateChanges = append(l.stateChanges, stateChange{component, oldState, newState, err})
}

func (l *testHealthListener) OnHealthCheck(component string, healthy bool, err error) {
	l.healthChecks = append(l.healthChecks, healthCheck{component, healthy, err})
}

func TestTracker_HealthListener(t *testing.T) {
	config := DefaultConfig()
	config.ErrorThreshold = 3
	tracker := NewTracker(config)
	tracker.RegisterComponent("test-service")

	listener := &testHealthListener{}
	tracker.AddHealthListener(listener)

	// Record error
	testErr := fmt.Errorf("test error")
	tracker.RecordError("test-service", testErr)

	// Give listener time to be notified
	time.Sleep(50 * time.Millisecond)

	if len(listener.healthChecks) != 1 {
		t.Errorf("Expected 1 health check notification, got %d", len(listener.healthChecks))
	}

	if listener.healthChecks[0].healthy {
		t.Error("Expected healthy=false for error")
	}

	// Record success
	tracker.RecordSuccess("test-service")
	time.Sleep(50 * time.Millisecond)

	if len(listener.healthChecks) != 2 {
		t.Errorf("Expected 2 health check notifications, got %d", len(listener.healthChecks))
	}

	if !listener.healthChecks[1].healthy {
		t.Error("Expected healthy=true for success")
	}
}

func TestTracker_GetAllComponents(t *testing.T) {
	tracker := NewTracker(DefaultConfig())
	tracker.RegisterComponent("service-1")
	tracker.RegisterComponent("service-2")
	tracker.RegisterComponent("service-3")

	components := tracker.GetAllComponents()

	if len(components) != 3 {
		t.Errorf("Expected 3 components, got %d", len(components))
	}

	for _, name := range []string{"service-1", "service-2", "service-3"} {
		if _, exists := components[name]; !exists {
			t.Errorf("Expected component '%s' to be present", name)
		}
	}
}

func TestTracker_SetComponentMetadata(t *testing.T) {
	tracker := NewTracker(DefaultConfig())
	tracker.RegisterComponent("test-service")

	tracker.SetComponentMetadata("test-service", "version", "1.0.0")
	tracker.SetComponentMetadata("test-service", "region", "us-west-2")

	health, err := tracker.GetComponentHealth("test-service")
	if err != nil {
		t.Fatalf("Failed to get component health: %v", err)
	}

	if health.Metadata["version"] != "1.0.0" {
		t.Errorf("Expected version='1.0.0', got '%v'", health.Metadata["version"])
	}

	if health.Metadata["region"] != "us-west-2" {
		t.Errorf("Expected region='us-west-2', got '%v'", health.Metadata["region"])
	}
}

func TestTracker_IsHealthy(t *testing.T) {
	tracker := NewTracker(DefaultConfig())
	tracker.RegisterComponent("test-service")

	if !tracker.IsHealthy("test-service") {
		t.Error("Expected IsHealthy=true initially")
	}

	// Record errors to degrade
	for i := 0; i < 3; i++ {
		tracker.RecordError("test-service", fmt.Errorf("error %d", i))
	}

	if tracker.IsHealthy("test-service") {
		t.Error("Expected IsHealthy=false after degradation")
	}
}

func TestTracker_StartHealthChecks(t *testing.T) {
	config := DefaultConfig()
	config.HealthCheckInterval = 50 * time.Millisecond
	tracker := NewTracker(config)
	tracker.RegisterComponent("test-service")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	checkCount := 0
	checkFn := func(component string) error {
		checkCount++
		return nil
	}

	go tracker.StartHealthChecks(ctx, checkFn)

	// Wait for a few health checks
	<-ctx.Done()

	if checkCount < 2 {
		t.Errorf("Expected at least 2 health checks, got %d", checkCount)
	}
}

func TestTracker_StartHealthChecks_WithErrors(t *testing.T) {
	config := DefaultConfig()
	config.HealthCheckInterval = 50 * time.Millisecond
	config.ErrorThreshold = 2
	tracker := NewTracker(config)
	tracker.RegisterComponent("test-service")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	checkCount := 0
	checkFn := func(component string) error {
		checkCount++
		return fmt.Errorf("health check failed")
	}

	go tracker.StartHealthChecks(ctx, checkFn)

	// Wait for health checks to run
	<-ctx.Done()

	// Component should be degraded after threshold
	state := tracker.GetState("test-service")
	if state == StateHealthy {
		t.Errorf("Expected non-healthy state after failed health checks, got %s", state)
	}
}

func TestHealthState_String(t *testing.T) {
	tests := []struct {
		state    HealthState
		expected string
	}{
		{StateHealthy, "healthy"},
		{StateDegraded, "degraded"},
		{StateReadOnly, "read-only"},
		{StateUnavailable, "unavailable"},
		{HealthState(999), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := tt.state.String()
			if result != tt.expected {
				t.Errorf("String() = %s, want %s", result, tt.expected)
			}
		})
	}
}

func TestTracker_GetComponentHealth_NotRegistered(t *testing.T) {
	tracker := NewTracker(DefaultConfig())

	_, err := tracker.GetComponentHealth("non-existent")
	if err == nil {
		t.Error("Expected error for non-existent component")
	}
}

func TestTracker_RecoveryFromDegradation(t *testing.T) {
	config := DefaultConfig()
	config.ErrorThreshold = 3
	config.RecoveryThreshold = 5
	tracker := NewTracker(config)
	tracker.RegisterComponent("test-service")

	// Degrade the service
	for i := 0; i < 3; i++ {
		tracker.RecordError("test-service", fmt.Errorf("error %d", i))
	}

	state := tracker.GetState("test-service")
	if state != StateDegraded {
		t.Errorf("Expected StateDegraded, got %s", state)
	}

	// Record successes to recover (need to clear ConsecutiveErrors)
	for i := 0; i < 3; i++ {
		tracker.RecordSuccess("test-service")
	}

	state = tracker.GetState("test-service")
	if state != StateHealthy {
		t.Errorf("Expected StateHealthy after recovery, got %s", state)
	}

	health, _ := tracker.GetComponentHealth("test-service")
	if health.ConsecutiveErrors != 0 {
		t.Errorf("Expected ConsecutiveErrors=0 after recovery, got %d", health.ConsecutiveErrors)
	}
}

// Benchmark tests
func BenchmarkTracker_RecordSuccess(b *testing.B) {
	tracker := NewTracker(DefaultConfig())
	tracker.RegisterComponent("test-service")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tracker.RecordSuccess("test-service")
	}
}

func BenchmarkTracker_RecordError(b *testing.B) {
	tracker := NewTracker(DefaultConfig())
	tracker.RegisterComponent("test-service")
	testErr := fmt.Errorf("test error")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tracker.RecordError("test-service", testErr)
	}
}

func BenchmarkTracker_GetState(b *testing.B) {
	tracker := NewTracker(DefaultConfig())
	tracker.RegisterComponent("test-service")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tracker.GetState("test-service")
	}
}
