package health

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Monitor provides system-wide health monitoring for ObjectFS
type Monitor struct {
	mu       sync.RWMutex
	checker  *Checker
	config   *MonitorConfig
	alerts   *AlertManager
	started  bool
	stopCh   chan struct{}
	
	// Component references (would be injected)
	components map[string]HealthyComponent
}

// MonitorConfig represents monitor configuration
type MonitorConfig struct {
	// Basic settings
	Enabled           bool          `yaml:"enabled"`
	MonitorInterval   time.Duration `yaml:"monitor_interval"`
	HealthCheckConfig *Config       `yaml:"health_check"`
	
	// Alerting settings
	AlertingEnabled   bool          `yaml:"alerting_enabled"`
	AlertConfig       *AlertConfig  `yaml:"alert_config"`
	
	// Recovery settings
	AutoRecovery      bool          `yaml:"auto_recovery"`
	RecoveryAttempts  int           `yaml:"recovery_attempts"`
	RecoveryDelay     time.Duration `yaml:"recovery_delay"`
	
	// Reporting settings
	ReportingEnabled  bool          `yaml:"reporting_enabled"`
	ReportInterval    time.Duration `yaml:"report_interval"`
	ReportFormat      string        `yaml:"report_format"`
	
	// Integration settings
	MetricsIntegration bool         `yaml:"metrics_integration"`
	LoggingIntegration bool         `yaml:"logging_integration"`
}

// AlertConfig represents alerting configuration
type AlertConfig struct {
	Enabled         bool          `yaml:"enabled"`
	Channels        []string      `yaml:"channels"`
	Severity        string        `yaml:"severity"`
	Cooldown        time.Duration `yaml:"cooldown"`
	RetryAttempts   int           `yaml:"retry_attempts"`
	RetryInterval   time.Duration `yaml:"retry_interval"`
}

// HealthyComponent defines the interface for components that can report health
type HealthyComponent interface {
	HealthCheck(ctx context.Context) error
	GetComponentName() string
	GetComponentType() string
}

// Alert represents a health alert
type Alert struct {
	ID          string    `json:"id"`
	Component   string    `json:"component"`
	Check       string    `json:"check"`
	Severity    string    `json:"severity"`
	Message     string    `json:"message"`
	Timestamp   time.Time `json:"timestamp"`
	Resolved    bool      `json:"resolved"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

// AlertManager manages health alerts
type AlertManager struct {
	mu      sync.RWMutex
	config  *AlertConfig
	alerts  map[string]*Alert
	channels map[string]AlertChannel
}

// AlertChannel defines the interface for alert delivery
type AlertChannel interface {
	SendAlert(alert *Alert) error
	GetChannelName() string
}

// NewMonitor creates a new health monitor
func NewMonitor(config *MonitorConfig) (*Monitor, error) {
	if config == nil {
		config = &MonitorConfig{
			Enabled:            true,
			MonitorInterval:    time.Minute,
			HealthCheckConfig:  nil, // Will use defaults
			AlertingEnabled:    true,
			AutoRecovery:       false,
			RecoveryAttempts:   3,
			RecoveryDelay:      30 * time.Second,
			ReportingEnabled:   true,
			ReportInterval:     5 * time.Minute,
			ReportFormat:       "json",
			MetricsIntegration: true,
			LoggingIntegration: true,
		}
	}

	// Create health checker
	checker, err := NewChecker(config.HealthCheckConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create health checker: %w", err)
	}

	// Create alert manager
	alertManager, err := NewAlertManager(config.AlertConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create alert manager: %w", err)
	}

	monitor := &Monitor{
		checker:    checker,
		config:     config,
		alerts:     alertManager,
		components: make(map[string]HealthyComponent),
		stopCh:     make(chan struct{}),
	}

	return monitor, nil
}

// Start starts the health monitor
func (m *Monitor) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.config.Enabled {
		return nil
	}

	if m.started {
		return fmt.Errorf("monitor already started")
	}

	// Start health checker
	if err := m.checker.Start(ctx); err != nil {
		return fmt.Errorf("failed to start health checker: %w", err)
	}

	// Register default health checks
	if err := m.registerDefaultChecks(); err != nil {
		return fmt.Errorf("failed to register default checks: %w", err)
	}

	m.started = true

	// Start monitoring loops
	go m.monitorLoop()
	
	if m.config.ReportingEnabled {
		go m.reportLoop()
	}

	return nil
}

// Stop stops the health monitor
func (m *Monitor) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return fmt.Errorf("monitor not started")
	}

	close(m.stopCh)
	
	if err := m.checker.Stop(); err != nil {
		return fmt.Errorf("failed to stop health checker: %w", err)
	}

	m.started = false
	return nil
}

// RegisterComponent registers a component for health monitoring
func (m *Monitor) RegisterComponent(component HealthyComponent) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	name := component.GetComponentName()
	if _, exists := m.components[name]; exists {
		return fmt.Errorf("component %s already registered", name)
	}

	m.components[name] = component

	// Register health check for this component
	checkFunc := func(ctx context.Context) error {
		return component.HealthCheck(ctx)
	}

	category := m.mapComponentTypeToCategory(component.GetComponentType())
	priority := m.mapComponentTypeToPriority(component.GetComponentType())

	return m.checker.RegisterCheck(
		name,
		fmt.Sprintf("Health check for %s component", name),
		category,
		priority,
		checkFunc,
	)
}

// GetStatus returns the current system health status
func (m *Monitor) GetStatus() *ServiceStatus {
	version := "1.0.0" // This would come from build info
	metadata := map[string]interface{}{
		"service": "objectfs",
		"components": len(m.components),
		"monitor_config": m.config,
	}

	return m.checker.NewServiceStatus(version, metadata)
}

// GetDetailedStatus returns detailed health information
func (m *Monitor) GetDetailedStatus() map[string]interface{} {
	status := make(map[string]interface{})

	// Add basic status
	status["status"] = m.checker.GetStatus()

	// Add component information
	m.mu.RLock()
	components := make(map[string]interface{})
	for name, component := range m.components {
		components[name] = map[string]interface{}{
			"name": component.GetComponentName(),
			"type": component.GetComponentType(),
		}
	}
	m.mu.RUnlock()
	status["components"] = components

	// Add recent alerts
	status["alerts"] = m.alerts.GetRecentAlerts(10)

	// Add configuration
	status["config"] = m.config

	return status
}

// IsHealthy returns whether the system is healthy
func (m *Monitor) IsHealthy() bool {
	return m.checker.IsHealthy()
}

// TriggerCheck manually triggers a specific health check
func (m *Monitor) TriggerCheck(ctx context.Context, checkName string) (*Result, error) {
	return m.checker.RunCheck(ctx, checkName)
}

// TriggerAllChecks manually triggers all health checks
func (m *Monitor) TriggerAllChecks(ctx context.Context) (map[string]*Result, error) {
	return m.checker.RunAllChecks(ctx)
}

// Helper methods

func (m *Monitor) registerDefaultChecks() error {
	// Register system-level health checks
	checks := []struct {
		name        string
		description string
		category    Category
		priority    Priority
		checkFunc   CheckFunction
	}{
		{
			name:        "system_ping",
			description: "Basic system availability check",
			category:    CategoryCore,
			priority:    PriorityCritical,
			checkFunc:   PingCheck(),
		},
		{
			name:        "memory_usage",
			description: "System memory usage check",
			category:    CategoryPerformance,
			priority:    PriorityHigh,
			checkFunc:   MemoryCheck(1024), // 1GB limit
		},
		{
			name:        "disk_space",
			description: "Available disk space check",
			category:    CategoryCore,
			priority:    PriorityHigh,
			checkFunc:   DiskSpaceCheck("/tmp", 1), // 1GB minimum
		},
	}

	for _, check := range checks {
		err := m.checker.RegisterCheck(
			check.name,
			check.description,
			check.category,
			check.priority,
			check.checkFunc,
		)
		if err != nil {
			return fmt.Errorf("failed to register check %s: %w", check.name, err)
		}
	}

	return nil
}

func (m *Monitor) mapComponentTypeToCategory(componentType string) Category {
	switch componentType {
	case "storage", "s3":
		return CategoryStorage
	case "cache", "lru", "multilevel":
		return CategoryCache
	case "network", "http", "tcp":
		return CategoryNetwork
	case "security", "auth":
		return CategorySecurity
	case "metrics", "monitoring":
		return CategoryPerformance
	default:
		return CategoryCore
	}
}

func (m *Monitor) mapComponentTypeToPriority(componentType string) Priority {
	switch componentType {
	case "storage", "core":
		return PriorityCritical
	case "cache", "network":
		return PriorityHigh
	case "metrics", "monitoring":
		return PriorityMedium
	default:
		return PriorityLow
	}
}

func (m *Monitor) monitorLoop() {
	interval := m.config.MonitorInterval
	if interval <= 0 {
		interval = time.Minute
	}
	
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.performMonitoringCycle()
		}
	}
}

func (m *Monitor) performMonitoringCycle() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Run all health checks
	results, err := m.checker.RunAllChecks(ctx)
	if err != nil {
		// Log error but continue
		return
	}

	// Process results and generate alerts if needed
	if m.config.AlertingEnabled {
		m.processResultsForAlerts(results)
	}

	// Attempt auto-recovery if enabled
	if m.config.AutoRecovery {
		m.attemptAutoRecovery(results)
	}
}

func (m *Monitor) processResultsForAlerts(results map[string]*Result) {
	for checkName, result := range results {
		if result.Status == StatusUnhealthy {
			// Check if we should generate an alert
			alert := &Alert{
				ID:        fmt.Sprintf("%s-%d", checkName, time.Now().Unix()),
				Component: "objectfs",
				Check:     checkName,
				Severity:  "warning",
				Message:   fmt.Sprintf("Health check %s failed: %s", checkName, result.Message),
				Timestamp: result.Timestamp,
				Resolved:  false,
			}

			m.alerts.ProcessAlert(alert)
		}
	}
}

func (m *Monitor) attemptAutoRecovery(results map[string]*Result) {
	// Auto-recovery logic would be implemented here
	// This is a simplified placeholder
	for checkName, result := range results {
		if result.Status == StatusUnhealthy {
			// Attempt recovery for the failed component
			m.mu.RLock()
			if component, exists := m.components[checkName]; exists {
				// Could call a recovery method on the component
				_ = component // Placeholder
			}
			m.mu.RUnlock()
		}
	}
}

func (m *Monitor) reportLoop() {
	interval := m.config.ReportInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.generateHealthReport()
		}
	}
}

func (m *Monitor) generateHealthReport() {
	// Generate and log/send health report
	status := m.GetStatus()
	
	// This would typically log or send the report
	_ = status // Placeholder
}

// NewAlertManager creates a new alert manager
func NewAlertManager(config *AlertConfig) (*AlertManager, error) {
	if config == nil {
		config = &AlertConfig{
			Enabled:       true,
			Channels:      []string{"console"},
			Severity:      "warning",
			Cooldown:      5 * time.Minute,
			RetryAttempts: 3,
			RetryInterval: time.Minute,
		}
	}

	manager := &AlertManager{
		config:   config,
		alerts:   make(map[string]*Alert),
		channels: make(map[string]AlertChannel),
	}

	// Register default console channel
	manager.channels["console"] = &ConsoleAlertChannel{}

	return manager, nil
}

// ProcessAlert processes a new alert
func (am *AlertManager) ProcessAlert(alert *Alert) {
	am.mu.Lock()
	defer am.mu.Unlock()

	if !am.config.Enabled {
		return
	}

	// Store the alert
	am.alerts[alert.ID] = alert

	// Send alert through configured channels
	for _, channelName := range am.config.Channels {
		if channel, exists := am.channels[channelName]; exists {
			go func(ch AlertChannel, a *Alert) {
				_ = ch.SendAlert(a) // Ignore alert sending errors to prevent blocking
			}(channel, alert)
		}
	}
}

// GetRecentAlerts returns recent alerts
func (am *AlertManager) GetRecentAlerts(limit int) []*Alert {
	am.mu.RLock()
	defer am.mu.RUnlock()

	alerts := make([]*Alert, 0, len(am.alerts))
	for _, alert := range am.alerts {
		alerts = append(alerts, alert)
	}

	// Sort by timestamp (most recent first)
	for i := 0; i < len(alerts)-1; i++ {
		for j := i + 1; j < len(alerts); j++ {
			if alerts[i].Timestamp.Before(alerts[j].Timestamp) {
				alerts[i], alerts[j] = alerts[j], alerts[i]
			}
		}
	}

	// Limit results
	if len(alerts) > limit {
		alerts = alerts[:limit]
	}

	return alerts
}

// ConsoleAlertChannel implements console-based alerting
type ConsoleAlertChannel struct{}

func (c *ConsoleAlertChannel) SendAlert(alert *Alert) error {
	fmt.Printf("[ALERT] %s: %s - %s (Component: %s, Check: %s)\n",
		alert.Severity, alert.Timestamp.Format(time.RFC3339),
		alert.Message, alert.Component, alert.Check)
	return nil
}

func (c *ConsoleAlertChannel) GetChannelName() string {
	return "console"
}

// HealthEndpoints provides HTTP endpoints for health checking
type HealthEndpoints struct {
	monitor *Monitor
}

// NewHealthEndpoints creates new health endpoints
func NewHealthEndpoints(monitor *Monitor) *HealthEndpoints {
	return &HealthEndpoints{monitor: monitor}
}

// GetHealthStatus returns basic health status (for load balancers)
func (he *HealthEndpoints) GetHealthStatus() map[string]interface{} {
	if he.monitor.IsHealthy() {
		return map[string]interface{}{
			"status": "healthy",
			"timestamp": time.Now(),
		}
	}
	
	return map[string]interface{}{
		"status": "unhealthy",
		"timestamp": time.Now(),
	}
}

// GetDetailedHealth returns detailed health information
func (he *HealthEndpoints) GetDetailedHealth() map[string]interface{} {
	return he.monitor.GetDetailedStatus()
}