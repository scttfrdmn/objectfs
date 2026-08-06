package health

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// newStartedMonitor returns a started Monitor that binds no port, and stops it on cleanup.
//
// The HTTP endpoint is disabled explicitly, and that is the point of this helper. Six tests in this
// file used to build an identical MonitorConfig with HealthCheckConfig left nil, which NewChecker
// fills in from its defaults — including HTTPEnabled: true on the fixed port 8081. Every Start
// therefore raced five siblings for one port: the first won, the rest logged "failed to bind" and
// took an error arm they were not written to test. Whether that arm ran at all depended on goroutine
// scheduling, so this package reported 45.0% coverage on an idle machine and 44.7% under CI's load,
// and its floor had been pinned to the luckier number. Tests that are not about the endpoint should
// not open one.
//
// A long MonitorInterval keeps monitorLoop from firing a cycle mid-test; these tests drive checks
// through TriggerCheck instead.
func newStartedMonitor(t *testing.T) *Monitor {
	t.Helper()

	monitor, err := NewMonitor(&MonitorConfig{
		Enabled:          true,
		MonitorInterval:  time.Hour,
		ReportingEnabled: false,
		HealthCheckConfig: &Config{
			Enabled:       true,
			CheckInterval: time.Hour,
			Timeout:       10 * time.Second,
			MaxFailures:   3,
			HTTPEnabled:   false,
		},
	})
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	if err := monitor.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	t.Cleanup(func() { _ = monitor.Stop() })

	return monitor
}

// MockComponent implements HealthyComponent for testing
type MockComponent struct {
	name       string
	compType   string
	healthErr  error
	checkCount int
}

func (m *MockComponent) HealthCheck(ctx context.Context) error {
	m.checkCount++
	return m.healthErr
}

func (m *MockComponent) GetComponentName() string {
	return m.name
}

func (m *MockComponent) GetComponentType() string {
	return m.compType
}

func TestNewMonitor_WithNilConfig(t *testing.T) {
	t.Parallel()

	monitor, err := NewMonitor(nil)
	if err != nil {
		t.Fatalf("NewMonitor(nil) error = %v, want nil", err)
	}
	if monitor == nil {
		t.Fatal("NewMonitor returned nil monitor")
	}
	if monitor.config == nil {
		t.Error("monitor.config is nil")
	}
	if !monitor.config.Enabled {
		t.Error("default config should be enabled")
	}
	if monitor.checker == nil {
		t.Error("monitor.checker is nil")
	}
	if monitor.alerts == nil {
		t.Error("monitor.alerts is nil")
	}
	if monitor.components == nil {
		t.Error("monitor.components map is nil")
	}
}

func TestNewMonitor_WithCustomConfig(t *testing.T) {
	t.Parallel()

	config := &MonitorConfig{
		Enabled:            true,
		MonitorInterval:    30 * time.Second,
		AlertingEnabled:    false,
		AutoRecovery:       true,
		RecoveryAttempts:   5,
		RecoveryDelay:      time.Minute,
		ReportingEnabled:   false,
		ReportInterval:     10 * time.Minute,
		ReportFormat:       "text",
		MetricsIntegration: false,
		LoggingIntegration: false,
	}

	monitor, err := NewMonitor(config)
	if err != nil {
		t.Fatalf("NewMonitor() error = %v, want nil", err)
	}
	if monitor.config.MonitorInterval != 30*time.Second {
		t.Errorf("MonitorInterval = %v, want %v", monitor.config.MonitorInterval, 30*time.Second)
	}
	if monitor.config.AlertingEnabled {
		t.Error("AlertingEnabled should be false")
	}
	if !monitor.config.AutoRecovery {
		t.Error("AutoRecovery should be true")
	}
}

func TestMonitor_StartStop(t *testing.T) {
	t.Parallel()

	monitor, err := NewMonitor(&MonitorConfig{
		Enabled:          true,
		MonitorInterval:  time.Hour, // Long interval to avoid background execution
		ReportingEnabled: false,
		// No HTTP endpoint: this test is about the lifecycle, not the port. See newStartedMonitor.
		HealthCheckConfig: &Config{Enabled: true, CheckInterval: time.Hour, HTTPEnabled: false},
	})
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	// Should not be started initially
	if monitor.started {
		t.Error("monitor should not be started initially")
	}

	// Start the monitor
	ctx := t.Context()
	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}

	if !monitor.started {
		t.Error("monitor should be started after Start()")
	}

	// Starting again should fail
	if err := monitor.Start(ctx); err == nil {
		t.Error("Start() on already started monitor should return error")
	}

	// Stop the monitor
	if err := monitor.Stop(); err != nil {
		t.Fatalf("Stop() error = %v, want nil", err)
	}

	if monitor.started {
		t.Error("monitor should not be started after Stop()")
	}

	// Stopping again should fail
	if err := monitor.Stop(); err == nil {
		t.Error("Stop() on non-started monitor should return error")
	}
}

func TestMonitor_StartDisabled(t *testing.T) {
	t.Parallel()

	monitor, err := NewMonitor(&MonitorConfig{
		Enabled: false,
	})
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	// Start should succeed but not actually start monitoring
	ctx := context.Background()
	if err := monitor.Start(ctx); err != nil {
		t.Errorf("Start() on disabled monitor error = %v, want nil", err)
	}

	if monitor.started {
		t.Error("disabled monitor should not be marked as started")
	}
}

func TestMonitor_RegisterComponent(t *testing.T) {
	t.Parallel()

	monitor := newStartedMonitor(t)

	// Register a healthy component
	comp := &MockComponent{
		name:      "test-storage",
		compType:  "storage",
		healthErr: nil,
	}

	if err := monitor.RegisterComponent(comp); err != nil {
		t.Errorf("RegisterComponent() error = %v, want nil", err)
	}

	// Check component was registered
	monitor.mu.RLock()
	if _, exists := monitor.components["test-storage"]; !exists {
		t.Error("component not found in components map")
	}
	monitor.mu.RUnlock()

	// Registering same component again should fail
	if err := monitor.RegisterComponent(comp); err == nil {
		t.Error("RegisterComponent() with duplicate name should return error")
	}
}

func TestMonitor_MapComponentTypeToCategory(t *testing.T) {
	t.Parallel()

	monitor, _ := NewMonitor(nil)

	tests := []struct {
		componentType string
		wantCategory  Category
	}{
		{"storage", CategoryStorage},
		{"s3", CategoryStorage},
		{"cache", CategoryCache},
		{"lru", CategoryCache},
		{"multilevel", CategoryCache},
		{"network", CategoryNetwork},
		{"http", CategoryNetwork},
		{"tcp", CategoryNetwork},
		{"security", CategorySecurity},
		{"auth", CategorySecurity},
		{"metrics", CategoryPerformance},
		{"monitoring", CategoryPerformance},
		{"unknown", CategoryCore},
		{"", CategoryCore},
	}

	for _, tt := range tests {
		t.Run(tt.componentType, func(t *testing.T) {
			t.Parallel()

			result := monitor.mapComponentTypeToCategory(tt.componentType)
			if result != tt.wantCategory {
				t.Errorf("mapComponentTypeToCategory(%q) = %v, want %v",
					tt.componentType, result, tt.wantCategory)
			}
		})
	}
}

func TestMonitor_MapComponentTypeToPriority(t *testing.T) {
	t.Parallel()

	monitor, _ := NewMonitor(nil)

	tests := []struct {
		componentType string
		wantPriority  Priority
	}{
		{"storage", PriorityCritical},
		{"core", PriorityCritical},
		{"cache", PriorityHigh},
		{"network", PriorityHigh},
		{"metrics", PriorityMedium},
		{"monitoring", PriorityMedium},
		{"unknown", PriorityLow},
		{"", PriorityLow},
	}

	for _, tt := range tests {
		t.Run(tt.componentType, func(t *testing.T) {
			t.Parallel()

			result := monitor.mapComponentTypeToPriority(tt.componentType)
			if result != tt.wantPriority {
				t.Errorf("mapComponentTypeToPriority(%q) = %v, want %v",
					tt.componentType, result, tt.wantPriority)
			}
		})
	}
}

func TestMonitor_GetStatus(t *testing.T) {
	t.Parallel()

	monitor, err := NewMonitor(nil)
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	status := monitor.GetStatus()
	if status == nil {
		t.Fatal("GetStatus() returned nil")
	}
}

func TestMonitor_GetDetailedStatus(t *testing.T) {
	t.Parallel()

	monitor := newStartedMonitor(t)

	// Register a component
	comp := &MockComponent{
		name:     "test-comp",
		compType: "storage",
	}
	_ = monitor.RegisterComponent(comp)

	status := monitor.GetDetailedStatus()
	if status == nil {
		t.Fatal("GetDetailedStatus() returned nil")
	}

	// Check for expected keys
	if _, exists := status["status"]; !exists {
		t.Error("detailed status missing 'status' key")
	}
	if _, exists := status["components"]; !exists {
		t.Error("detailed status missing 'components' key")
	}
	if _, exists := status["alerts"]; !exists {
		t.Error("detailed status missing 'alerts' key")
	}
	if _, exists := status["config"]; !exists {
		t.Error("detailed status missing 'config' key")
	}
}

func TestMonitor_IsHealthy(t *testing.T) {
	t.Parallel()

	monitor, err := NewMonitor(nil)
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	// Should return a boolean (implementation may vary)
	_ = monitor.IsHealthy()
}

func TestMonitor_TriggerCheck(t *testing.T) {
	t.Parallel()

	monitor := newStartedMonitor(t)

	// Register a component
	comp := &MockComponent{
		name:      "trigger-test",
		compType:  "storage",
		healthErr: nil,
	}
	_ = monitor.RegisterComponent(comp)

	// Trigger the check
	result, err := monitor.TriggerCheck(t.Context(), "trigger-test")
	if err != nil {
		t.Errorf("TriggerCheck() error = %v", err)
	}
	if result != nil && comp.checkCount == 0 {
		t.Error("health check was not called")
	}
}

func TestMonitor_TriggerAllChecks(t *testing.T) {
	t.Parallel()

	monitor := newStartedMonitor(t)

	// Register multiple components
	comp1 := &MockComponent{name: "comp1", compType: "storage"}
	comp2 := &MockComponent{name: "comp2", compType: "cache"}
	_ = monitor.RegisterComponent(comp1)
	_ = monitor.RegisterComponent(comp2)

	// Trigger all checks
	results, err := monitor.TriggerAllChecks(t.Context())
	if err != nil {
		t.Errorf("TriggerAllChecks() error = %v", err)
	}
	if results == nil {
		t.Error("TriggerAllChecks() returned nil results")
	}
}

// AlertManager tests

func TestNewAlertManager_WithNilConfig(t *testing.T) {
	t.Parallel()

	am, err := NewAlertManager(nil)
	if err != nil {
		t.Fatalf("NewAlertManager(nil) error = %v, want nil", err)
	}
	if am == nil {
		t.Fatal("NewAlertManager returned nil")
	}
	if am.config == nil {
		t.Error("alert manager config is nil")
	}
	if !am.config.Enabled {
		t.Error("default config should be enabled")
	}
	if am.alerts == nil {
		t.Error("alerts map is nil")
	}
	if am.channels == nil {
		t.Error("channels map is nil")
	}

	// Check default console channel
	if _, exists := am.channels["console"]; !exists {
		t.Error("default console channel not registered")
	}
}

func TestNewAlertManager_WithCustomConfig(t *testing.T) {
	t.Parallel()

	config := &AlertConfig{
		Enabled:       false,
		Channels:      []string{"custom"},
		Severity:      "critical",
		Cooldown:      10 * time.Minute,
		RetryAttempts: 5,
		RetryInterval: 2 * time.Minute,
	}

	am, err := NewAlertManager(config)
	if err != nil {
		t.Fatalf("NewAlertManager() error = %v, want nil", err)
	}
	if am.config.Enabled {
		t.Error("Enabled should be false")
	}
	if am.config.Severity != "critical" {
		t.Errorf("Severity = %q, want %q", am.config.Severity, "critical")
	}
}

func TestAlertManager_ProcessAlert(t *testing.T) {
	t.Parallel()

	am, err := NewAlertManager(nil)
	if err != nil {
		t.Fatalf("NewAlertManager() error = %v", err)
	}

	alert := &Alert{
		ID:        "test-alert-1",
		Component: "objectfs",
		Check:     "test-check",
		Severity:  "warning",
		Message:   "Test alert message",
		Timestamp: time.Now(),
		Resolved:  false,
	}

	am.ProcessAlert(alert)

	// Check alert was stored
	am.mu.RLock()
	stored, exists := am.alerts[alert.ID]
	am.mu.RUnlock()

	if !exists {
		t.Error("alert was not stored")
	}
	if stored.ID != alert.ID {
		t.Errorf("stored alert ID = %q, want %q", stored.ID, alert.ID)
	}
}

func TestAlertManager_ProcessAlertDisabled(t *testing.T) {
	t.Parallel()

	am, err := NewAlertManager(&AlertConfig{
		Enabled: false,
	})
	if err != nil {
		t.Fatalf("NewAlertManager() error = %v", err)
	}

	alert := &Alert{
		ID:        "test-alert-2",
		Component: "objectfs",
		Check:     "test-check",
		Severity:  "warning",
		Message:   "Test alert",
		Timestamp: time.Now(),
	}

	am.ProcessAlert(alert)

	// Alert should not be stored when disabled
	am.mu.RLock()
	_, exists := am.alerts[alert.ID]
	am.mu.RUnlock()

	if exists {
		t.Error("alert should not be stored when manager is disabled")
	}
}

func TestAlertManager_GetRecentAlerts(t *testing.T) {
	t.Parallel()

	am, err := NewAlertManager(nil)
	if err != nil {
		t.Fatalf("NewAlertManager() error = %v", err)
	}

	// Add multiple alerts with different timestamps
	baseTime := time.Now()
	for i := range 5 {
		alert := &Alert{
			ID:        fmt.Sprintf("alert-%d", i),
			Component: "objectfs",
			Check:     "test",
			Severity:  "warning",
			Message:   fmt.Sprintf("Alert %d", i),
			Timestamp: baseTime.Add(time.Duration(i) * time.Second),
		}
		am.ProcessAlert(alert)
	}

	// Get recent alerts with limit
	recent := am.GetRecentAlerts(3)
	if len(recent) != 3 {
		t.Errorf("GetRecentAlerts(3) returned %d alerts, want 3", len(recent))
	}

	// Verify they're sorted by timestamp (most recent first)
	for i := range len(recent) - 1 {
		if recent[i].Timestamp.Before(recent[i+1].Timestamp) {
			t.Error("alerts not sorted by timestamp (most recent first)")
			break
		}
	}

	// Get all alerts
	all := am.GetRecentAlerts(100)
	if len(all) != 5 {
		t.Errorf("GetRecentAlerts(100) returned %d alerts, want 5", len(all))
	}
}

// ConsoleAlertChannel tests

func TestConsoleAlertChannel_SendAlert(t *testing.T) {
	t.Parallel()

	channel := &ConsoleAlertChannel{}

	alert := &Alert{
		ID:        "test-console-alert",
		Component: "objectfs",
		Check:     "test-check",
		Severity:  "critical",
		Message:   "Test console alert",
		Timestamp: time.Now(),
	}

	// Should not error
	if err := channel.SendAlert(alert); err != nil {
		t.Errorf("SendAlert() error = %v, want nil", err)
	}
}

func TestConsoleAlertChannel_GetChannelName(t *testing.T) {
	t.Parallel()

	channel := &ConsoleAlertChannel{}
	if name := channel.GetChannelName(); name != "console" {
		t.Errorf("GetChannelName() = %q, want %q", name, "console")
	}
}

// HealthEndpoints tests

func TestNewHealthEndpoints(t *testing.T) {
	t.Parallel()

	monitor, err := NewMonitor(nil)
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	endpoints := NewHealthEndpoints(monitor)
	if endpoints == nil {
		t.Fatal("NewHealthEndpoints returned nil")
	}
	if endpoints.monitor != monitor {
		t.Error("endpoints.monitor not set correctly")
	}
}

func TestHealthEndpoints_GetHealthStatus(t *testing.T) {
	t.Parallel()

	monitor, err := NewMonitor(nil)
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	endpoints := NewHealthEndpoints(monitor)
	status := endpoints.GetHealthStatus()

	if status == nil {
		t.Fatal("GetHealthStatus() returned nil")
	}

	// Check for required fields
	if _, exists := status["status"]; !exists {
		t.Error("status missing 'status' field")
	}
	if _, exists := status["timestamp"]; !exists {
		t.Error("status missing 'timestamp' field")
	}

	// Status should be either "healthy" or "unhealthy"
	statusVal, ok := status["status"].(string)
	if !ok {
		t.Error("status field is not a string")
	}
	if statusVal != "healthy" && statusVal != "unhealthy" {
		t.Errorf("status = %q, want 'healthy' or 'unhealthy'", statusVal)
	}
}

func TestHealthEndpoints_GetDetailedHealth(t *testing.T) {
	t.Parallel()

	monitor, err := NewMonitor(nil)
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	endpoints := NewHealthEndpoints(monitor)
	detailed := endpoints.GetDetailedHealth()

	if detailed == nil {
		t.Fatal("GetDetailedHealth() returned nil")
	}

	// Should have same structure as monitor.GetDetailedStatus()
	if _, exists := detailed["status"]; !exists {
		t.Error("detailed health missing 'status' key")
	}
}

// MockComponent with failure scenarios

func TestMonitor_ComponentHealthFailure(t *testing.T) {
	t.Parallel()

	monitor := newStartedMonitor(t)

	// Register a failing component
	comp := &MockComponent{
		name:      "failing-comp",
		compType:  "storage",
		healthErr: errors.New("component is unhealthy"),
	}
	_ = monitor.RegisterComponent(comp)

	// Trigger check. The check must run and must report the component's own failure — an error from
	// TriggerCheck itself would mean the check never executed, which is a different outcome and not
	// the one this test is about.
	result, err := monitor.TriggerCheck(t.Context(), "failing-comp")
	if err != nil {
		t.Fatalf("TriggerCheck() error = %v: the check is registered, so it must run", err)
	}

	if result.Status != StatusUnhealthy {
		t.Errorf("Status = %q, want %q for a component whose HealthCheck returned an error",
			result.Status, StatusUnhealthy)
	}

	if result.Error == "" {
		t.Error("Result.Error is empty for a failed check, so nothing carries the reason the " +
			"component is unhealthy to whoever reads the endpoint")
	}

	if comp.checkCount == 0 {
		t.Error("the component's HealthCheck was never called")
	}
}

// recoveryCtxKey marks a context so a check or a recovery can report which one it was handed.
type recoveryCtxKey struct{}

// recoverableComponent is unhealthy, recoverable, and records the context value it sees in each role.
//
// Both fields are read by the test after performMonitoringCycle returns, and HealthCheck runs on one
// of RunAllChecks' goroutines, so the mutex is not decoration.
type recoverableComponent struct {
	mu           sync.Mutex
	checkedValue any
	recoverValue any
	recoverCalls int
}

func (c *recoverableComponent) GetComponentName() string { return "recoverable-comp" }
func (c *recoverableComponent) GetComponentType() string { return "storage" }

func (c *recoverableComponent) HealthCheck(ctx context.Context) error {
	c.mu.Lock()
	c.checkedValue = ctx.Value(recoveryCtxKey{})
	c.mu.Unlock()

	return errors.New("unhealthy on purpose, so auto-recovery runs")
}

func (c *recoverableComponent) Recover(ctx context.Context) error {
	c.mu.Lock()
	c.recoverValue = ctx.Value(recoveryCtxKey{})
	c.recoverCalls++
	c.mu.Unlock()

	return errors.New("recovery fails on purpose, so every attempt is used")
}

// newRecoveryMonitor returns a started Monitor with auto-recovery on and the given delay, plus the
// unhealthy recoverable component it monitors.
func newRecoveryMonitor(t *testing.T, delay time.Duration) (*Monitor, *recoverableComponent) {
	t.Helper()

	monitor, err := NewMonitor(&MonitorConfig{
		Enabled:          true,
		MonitorInterval:  time.Hour, // the test drives cycles itself
		AutoRecovery:     true,
		RecoveryAttempts: 2,
		RecoveryDelay:    delay,
		HealthCheckConfig: &Config{
			Enabled:       true,
			CheckInterval: time.Hour,
			Timeout:       10 * time.Second,
			HTTPEnabled:   false,
		},
	})
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	if err := monitor.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = monitor.Stop() })

	comp := &recoverableComponent{}
	if err := monitor.RegisterComponent(comp); err != nil {
		t.Fatalf("RegisterComponent() error = %v", err)
	}

	return monitor, comp
}

// TestMonitorCycle_CarriesTheCallersContext is what the contextcheck fix is worth beyond a linter
// count.
//
// monitorLoop took no context and every cycle built one from context.Background(), so the context
// Start was handed reached m.checker.Start and nothing else this type ran — checks and recoveries
// driven by the monitor's own ticker saw an empty context forever. This asserts the value plumbs all
// the way to both, which a mechanical `parent context.Context` parameter that was then ignored would
// not satisfy.
func TestMonitorCycle_CarriesTheCallersContext(t *testing.T) {
	t.Parallel()

	monitor, comp := newRecoveryMonitor(t, 0)

	ctx := context.WithValue(t.Context(), recoveryCtxKey{}, "from-the-caller")
	monitor.performMonitoringCycle(ctx)

	comp.mu.Lock()
	defer comp.mu.Unlock()

	if comp.checkedValue != "from-the-caller" {
		t.Errorf("the health check saw context value %v, want %q: the cycle's context does not "+
			"descend from the one it was given", comp.checkedValue, "from-the-caller")
	}

	if comp.recoverValue != "from-the-caller" {
		t.Errorf("Recover saw context value %v, want %q", comp.recoverValue, "from-the-caller")
	}

	if comp.recoverCalls != 2 {
		t.Errorf("Recover called %d times, want 2 (RecoveryAttempts), each having failed",
			comp.recoverCalls)
	}
}

// TestAutoRecovery_DelayEndsOnCancellation pins the other half: the wait between attempts is
// interruptible.
//
// It was time.Sleep(delay), which no cancellation could shorten — a component configured with a long
// RecoveryDelay held a goroutine retrying a recovery whose context had already expired and whose
// monitor may already have been stopped. With an hour of delay and a context canceled before the
// cycle starts, the old code takes an hour and the new one returns at once.
func TestAutoRecovery_DelayEndsOnCancellation(t *testing.T) {
	t.Parallel()

	monitor, comp := newRecoveryMonitor(t, time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		monitor.attemptAutoRecovery(ctx, map[string]*Result{
			comp.GetComponentName(): {Check: comp.GetComponentName(), Status: StatusUnhealthy},
		})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("attemptAutoRecovery is still waiting out RecoveryDelay 10s after its context was " +
			"canceled; the inter-attempt wait is not interruptible")
	}
}

// TestPriority_AtLeast pins the severity order, because Priority is a string and `>=` on it silently
// compares bytes: "critical" < "high" < "low" < "medium", which is neither the declaration order nor
// the severity order.
func TestPriority_AtLeast(t *testing.T) {
	t.Parallel()

	// Least severe first.
	order := []Priority{PriorityLow, PriorityMedium, PriorityHigh, PriorityCritical}

	for i, p := range order {
		for j, other := range order {
			want := i >= j
			if got := p.AtLeast(other); got != want {
				t.Errorf("Priority(%q).AtLeast(%q) = %v, want %v", p, other, got, want)
			}
		}
	}

	// The specific case the auto-remediation gate got wrong, spelled out: byte order puts "critical"
	// before "high", so `severity >= PriorityHigh` excluded the most severe priority there is.
	if !PriorityCritical.AtLeast(PriorityHigh) {
		t.Error("PriorityCritical is not AtLeast PriorityHigh, which is the inversion this method exists to prevent")
	}
	if PriorityMedium.AtLeast(PriorityHigh) {
		t.Error("PriorityMedium counts as AtLeast PriorityHigh")
	}

	// An unrecognized value must not outrank a real one — it ranks 0, below Low.
	if Priority("").AtLeast(PriorityLow) {
		t.Error(`Priority("") counts as AtLeast PriorityLow`)
	}
}

// TestDetectProblems_CriticalIssueRemediatesWithTheLoopsContext covers the auto-remediation path of
// EnhancedMonitor end to end, which nothing did.
//
// Two defects met here. The gate read `severity >= PriorityHigh` on a string Priority, so the two
// PriorityCritical arms of detectProblems — a critical error rate and a critical failure streak — were
// the only issues that never triggered remediation, while PriorityMedium did. And the remediation
// context came from context.Background(), so the problem-detection loop's context, which Start is
// handed, reached nothing.
func TestDetectProblems_CriticalIssueRemediatesWithTheLoopsContext(t *testing.T) {
	t.Parallel()

	monitor, err := NewEnhancedMonitor(&MonitorConfig{
		Enabled:         true,
		MonitorInterval: time.Hour,
		AutoRecovery:    true,
		HealthCheckConfig: &Config{
			Enabled:     true,
			Timeout:     10 * time.Second,
			HTTPEnabled: false,
		},
	})
	if err != nil {
		t.Fatalf("NewEnhancedMonitor() error = %v", err)
	}

	const component = "critical-comp"

	// A diagnosis with an automated fix that reports the context it was handed. Preloaded so
	// AttemptAutoRemediation does not have to run a health check to reach it.
	fixed := make(chan any, 1)
	monitor.diagnoses[component] = &ProblemDiagnosis{
		Check: component,
		Remediations: []*RemediationAction{{
			ID:        "record-the-context",
			Automated: true,
			AutoFix: func(ctx context.Context) error {
				fixed <- ctx.Value(recoveryCtxKey{})
				return nil
			},
		}},
	}

	// A failure streak past FailureStreakCritical, which is the PriorityCritical arm.
	monitor.analyzer.mu.Lock()
	monitor.analyzer.patterns[component] = &HealthPattern{
		ComponentName: component,
		FailureStreak: monitor.analyzer.thresholds.FailureStreakCritical + 1,
	}
	monitor.analyzer.mu.Unlock()

	ctx := context.WithValue(t.Context(), recoveryCtxKey{}, "from-the-detection-loop")
	monitor.detectProblems(ctx, component)

	select {
	case got := <-fixed:
		if got != "from-the-detection-loop" {
			t.Errorf("the auto-fix saw context value %v, want %q: remediation does not run on the "+
				"detection loop's context", got, "from-the-detection-loop")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no auto-remediation ran for a PriorityCritical issue with AutoRecovery enabled")
	}

	// The issue must also be recorded, and recorded as resolved once the fix succeeded.
	issues := monitor.GetDetectedIssues(true)
	if len(issues) != 1 {
		t.Fatalf("GetDetectedIssues() returned %d issues, want 1", len(issues))
	}
	if issues[0].Severity != PriorityCritical {
		t.Errorf("issue severity = %q, want %q", issues[0].Severity, PriorityCritical)
	}
}
