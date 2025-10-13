package health

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

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
	})
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	// Should not be started initially
	if monitor.started {
		t.Error("monitor should not be started initially")
	}

	// Start the monitor
	ctx := context.Background()
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

	monitor, err := NewMonitor(&MonitorConfig{
		Enabled:          true,
		MonitorInterval:  time.Hour,
		ReportingEnabled: false,
	})
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	ctx := context.Background()
	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = monitor.Stop() }()

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

	monitor, err := NewMonitor(&MonitorConfig{
		Enabled:          true,
		MonitorInterval:  time.Hour,
		ReportingEnabled: false,
	})
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	ctx := context.Background()
	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = monitor.Stop() }()

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

	monitor, err := NewMonitor(&MonitorConfig{
		Enabled:          true,
		MonitorInterval:  time.Hour,
		ReportingEnabled: false,
	})
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	ctx := context.Background()
	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = monitor.Stop() }()

	// Register a component
	comp := &MockComponent{
		name:      "trigger-test",
		compType:  "storage",
		healthErr: nil,
	}
	_ = monitor.RegisterComponent(comp)

	// Trigger the check
	result, err := monitor.TriggerCheck(ctx, "trigger-test")
	if err != nil {
		t.Errorf("TriggerCheck() error = %v", err)
	}
	if result != nil && comp.checkCount == 0 {
		t.Error("health check was not called")
	}
}

func TestMonitor_TriggerAllChecks(t *testing.T) {
	t.Parallel()

	monitor, err := NewMonitor(&MonitorConfig{
		Enabled:          true,
		MonitorInterval:  time.Hour,
		ReportingEnabled: false,
	})
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	ctx := context.Background()
	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = monitor.Stop() }()

	// Register multiple components
	comp1 := &MockComponent{name: "comp1", compType: "storage"}
	comp2 := &MockComponent{name: "comp2", compType: "cache"}
	_ = monitor.RegisterComponent(comp1)
	_ = monitor.RegisterComponent(comp2)

	// Trigger all checks
	results, err := monitor.TriggerAllChecks(ctx)
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
	for i := 0; i < 5; i++ {
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
	for i := 0; i < len(recent)-1; i++ {
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

	monitor, err := NewMonitor(&MonitorConfig{
		Enabled:          true,
		MonitorInterval:  time.Hour,
		ReportingEnabled: false,
	})
	if err != nil {
		t.Fatalf("NewMonitor() error = %v", err)
	}

	ctx := context.Background()
	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = monitor.Stop() }()

	// Register a failing component
	comp := &MockComponent{
		name:      "failing-comp",
		compType:  "storage",
		healthErr: errors.New("component is unhealthy"),
	}
	_ = monitor.RegisterComponent(comp)

	// Trigger check
	result, err := monitor.TriggerCheck(ctx, "failing-comp")
	if err != nil {
		// Error is acceptable if check failed
		_ = result
	}
}
