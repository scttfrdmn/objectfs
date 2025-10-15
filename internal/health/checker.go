package health

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Checker implements comprehensive health checking for ObjectFS components
type Checker struct {
	mu         sync.RWMutex
	config     *Config
	checks     map[string]*Check
	results    map[string]*Result
	stats      Stats
	stopCh     chan struct{}
	started    bool
	lastUpdate time.Time
}

// Config represents health checker configuration
type Config struct {
	// Basic settings
	Enabled       bool          `yaml:"enabled"`
	CheckInterval time.Duration `yaml:"check_interval"`
	Timeout       time.Duration `yaml:"timeout"`

	// Failure handling
	MaxFailures      int           `yaml:"max_failures"`
	FailureWindow    time.Duration `yaml:"failure_window"`
	RecoveryRequired int           `yaml:"recovery_required"`

	// Advanced settings
	EnableAlerts   bool `yaml:"enable_alerts"`
	AlertThreshold int  `yaml:"alert_threshold"`
	MetricsEnabled bool `yaml:"metrics_enabled"`

	// HTTP endpoint settings
	HTTPEnabled bool   `yaml:"http_enabled"`
	HTTPPort    int    `yaml:"http_port"`
	HTTPPath    string `yaml:"http_path"`
}

// Check represents a health check function
type Check struct {
	Name        string
	Description string
	Category    Category
	Priority    Priority
	Timeout     time.Duration
	Function    CheckFunction

	// State management
	enabled      bool
	lastRun      time.Time
	runCount     int64
	successCount int64
	failureCount int64
	consecutive  int
}

// CheckFunction defines the signature for health check functions
type CheckFunction func(ctx context.Context) error

// Result represents the result of a health check
type Result struct {
	Check     string        `json:"check"`
	Status    Status        `json:"status"`
	Message   string        `json:"message"`
	Duration  time.Duration `json:"duration"`
	Timestamp time.Time     `json:"timestamp"`
	Error     string        `json:"error,omitempty"`
}

// Stats tracks overall health check statistics
type Stats struct {
	TotalChecks      int64         `json:"total_checks"`
	SuccessfulChecks int64         `json:"successful_checks"`
	FailedChecks     int64         `json:"failed_checks"`
	AverageLatency   time.Duration `json:"average_latency"`
	LastCheck        time.Time     `json:"last_check"`

	// Status counts
	HealthyChecks   int `json:"healthy_checks"`
	UnhealthyChecks int `json:"unhealthy_checks"`
	UnknownChecks   int `json:"unknown_checks"`

	// System status
	OverallStatus Status        `json:"overall_status"`
	SystemUptime  time.Duration `json:"system_uptime"`
	LastFailure   time.Time     `json:"last_failure"`
}

// Enums for health check categorization
type Category string

const (
	CategoryCore        Category = "core"
	CategoryStorage     Category = "storage"
	CategoryCache       Category = "cache"
	CategoryNetwork     Category = "network"
	CategorySecurity    Category = "security"
	CategoryPerformance Category = "performance"
)

type Priority string

const (
	PriorityCritical Priority = "critical"
	PriorityHigh     Priority = "high"
	PriorityMedium   Priority = "medium"
	PriorityLow      Priority = "low"
)

type Status string

const (
	StatusHealthy   Status = "healthy"
	StatusUnhealthy Status = "unhealthy"
	StatusUnknown   Status = "unknown"
	StatusDegraded  Status = "degraded"
)

// NewChecker creates a new health checker
func NewChecker(config *Config) (*Checker, error) {
	if config == nil {
		config = &Config{
			Enabled:          true,
			CheckInterval:    30 * time.Second,
			Timeout:          10 * time.Second,
			MaxFailures:      3,
			FailureWindow:    5 * time.Minute,
			RecoveryRequired: 2,
			EnableAlerts:     true,
			AlertThreshold:   2,
			MetricsEnabled:   true,
			HTTPEnabled:      true,
			HTTPPort:         8081,
			HTTPPath:         "/health",
		}
	}

	checker := &Checker{
		config:  config,
		checks:  make(map[string]*Check),
		results: make(map[string]*Result),
		stats: Stats{
			OverallStatus: StatusUnknown,
		},
		stopCh: make(chan struct{}),
	}

	return checker, nil
}

// RegisterCheck registers a new health check
func (c *Checker) RegisterCheck(name string, description string, category Category, priority Priority, checkFunc CheckFunction) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.checks[name]; exists {
		return fmt.Errorf("health check %s already registered", name)
	}

	check := &Check{
		Name:        name,
		Description: description,
		Category:    category,
		Priority:    priority,
		Timeout:     c.config.Timeout,
		Function:    checkFunc,
		enabled:     true,
	}

	c.checks[name] = check
	return nil
}

// Start starts the health checker
func (c *Checker) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.config.Enabled {
		return nil
	}

	if c.started {
		return fmt.Errorf("health checker already started")
	}

	c.started = true
	c.lastUpdate = time.Now()

	// Start background check loop
	go c.checkLoop()

	// Start HTTP server if enabled
	if c.config.HTTPEnabled {
		go c.startHTTPServer()
	}

	return nil
}

// Stop stops the health checker
func (c *Checker) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.started {
		return fmt.Errorf("health checker not started")
	}

	close(c.stopCh)
	c.started = false

	return nil
}

// RunCheck executes a specific health check
func (c *Checker) RunCheck(ctx context.Context, name string) (*Result, error) {
	c.mu.RLock()
	check, exists := c.checks[name]
	c.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("health check %s not found", name)
	}

	if !check.enabled {
		return &Result{
			Check:     name,
			Status:    StatusUnknown,
			Message:   "Check disabled",
			Timestamp: time.Now(),
		}, nil
	}

	return c.executeCheck(ctx, check)
}

// RunAllChecks executes all registered health checks
func (c *Checker) RunAllChecks(ctx context.Context) (map[string]*Result, error) {
	c.mu.RLock()
	checks := make([]*Check, 0, len(c.checks))
	for _, check := range c.checks {
		if check.enabled {
			checks = append(checks, check)
		}
	}
	c.mu.RUnlock()

	results := make(map[string]*Result)

	// Run checks concurrently
	resultsChan := make(chan *Result, len(checks))

	for _, check := range checks {
		go func(ch *Check) {
			result, _ := c.executeCheck(ctx, ch)
			resultsChan <- result
		}(check)
	}

	// Collect results
	for i := 0; i < len(checks); i++ {
		result := <-resultsChan
		results[result.Check] = result
	}

	// Update stored results
	c.mu.Lock()
	for name, result := range results {
		c.results[name] = result
	}
	c.updateStats()
	c.mu.Unlock()

	return results, nil
}

// GetStatus returns the current health status
func (c *Checker) GetStatus() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()

	status := make(map[string]interface{})
	status["overall_status"] = c.stats.OverallStatus
	status["timestamp"] = time.Now()
	status["uptime"] = time.Since(c.lastUpdate)
	status["stats"] = c.stats

	// Add individual check results
	checks := make(map[string]*Result)
	for name, result := range c.results {
		checks[name] = result
	}
	status["checks"] = checks

	return status
}

// GetStats returns health check statistics
func (c *Checker) GetStats() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats
}

// EnableCheck enables a specific health check
func (c *Checker) EnableCheck(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	check, exists := c.checks[name]
	if !exists {
		return fmt.Errorf("health check %s not found", name)
	}

	check.enabled = true
	return nil
}

// DisableCheck disables a specific health check
func (c *Checker) DisableCheck(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	check, exists := c.checks[name]
	if !exists {
		return fmt.Errorf("health check %s not found", name)
	}

	check.enabled = false
	return nil
}

// IsHealthy returns whether the system is considered healthy
func (c *Checker) IsHealthy() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats.OverallStatus == StatusHealthy
}

// Helper methods

func (c *Checker) executeCheck(ctx context.Context, check *Check) (*Result, error) {
	start := time.Now()

	// Create timeout context
	checkCtx, cancel := context.WithTimeout(ctx, check.Timeout)
	defer cancel()

	// Execute the check
	err := check.Function(checkCtx)
	duration := time.Since(start)

	// Update check statistics
	c.mu.Lock()
	check.lastRun = start
	check.runCount++

	result := &Result{
		Check:     check.Name,
		Duration:  duration,
		Timestamp: start,
	}

	if err != nil {
		check.failureCount++
		check.consecutive++
		result.Status = StatusUnhealthy
		result.Message = "Check failed"
		result.Error = err.Error()
	} else {
		check.successCount++
		check.consecutive = 0
		result.Status = StatusHealthy
		result.Message = "Check passed"
	}
	c.mu.Unlock()

	return result, nil
}

func (c *Checker) checkLoop() {
	interval := c.config.CheckInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), c.config.Timeout*2)
			_, _ = c.RunAllChecks(ctx) // Ignore periodic check errors
			cancel()
		}
	}
}

func (c *Checker) updateStats() {
	// This should be called with the mutex locked
	c.stats.TotalChecks = 0
	c.stats.SuccessfulChecks = 0
	c.stats.FailedChecks = 0
	c.stats.HealthyChecks = 0
	c.stats.UnhealthyChecks = 0
	c.stats.UnknownChecks = 0

	var totalDuration time.Duration
	criticalFailures := 0

	for _, check := range c.checks {
		c.stats.TotalChecks += check.runCount
		c.stats.SuccessfulChecks += check.successCount
		c.stats.FailedChecks += check.failureCount
	}

	for _, result := range c.results {
		totalDuration += result.Duration

		switch result.Status {
		case StatusHealthy:
			c.stats.HealthyChecks++
		case StatusUnhealthy:
			c.stats.UnhealthyChecks++
			// Check if this is a critical check
			if check, exists := c.checks[result.Check]; exists && check.Priority == PriorityCritical {
				criticalFailures++
			}
		default:
			c.stats.UnknownChecks++
		}
	}

	// Calculate average latency
	totalResults := len(c.results)
	if totalResults > 0 {
		c.stats.AverageLatency = totalDuration / time.Duration(totalResults)
	}

	// Determine overall status
	if criticalFailures > 0 {
		c.stats.OverallStatus = StatusUnhealthy
	} else if c.stats.UnhealthyChecks > 0 {
		c.stats.OverallStatus = StatusDegraded
	} else if c.stats.HealthyChecks > 0 {
		c.stats.OverallStatus = StatusHealthy
	} else {
		c.stats.OverallStatus = StatusUnknown
	}

	c.stats.LastCheck = time.Now()
	c.stats.SystemUptime = time.Since(c.lastUpdate)
}

func (c *Checker) startHTTPServer() {
	// This would start an HTTP server for health check endpoints
	// Implementation omitted for brevity in this demo
}

// Common health check functions

// PingCheck creates a simple ping health check
func PingCheck() CheckFunction {
	return func(ctx context.Context) error {
		// Simple ping check - always passes
		return nil
	}
}

// StorageCheck creates a storage backend health check
func StorageCheck(testFunc func(ctx context.Context) error) CheckFunction {
	return func(ctx context.Context) error {
		return testFunc(ctx)
	}
}

// CacheCheck creates a cache system health check
func CacheCheck(testFunc func(ctx context.Context) error) CheckFunction {
	return func(ctx context.Context) error {
		return testFunc(ctx)
	}
}

// MemoryCheck creates a memory usage health check
func MemoryCheck(maxMemoryMB int64) CheckFunction {
	return func(ctx context.Context) error {
		// This would check actual memory usage
		// Simplified implementation
		return nil
	}
}

// DiskSpaceCheck creates a disk space health check
func DiskSpaceCheck(path string, minFreeGB int64) CheckFunction {
	return func(ctx context.Context) error {
		// This would check actual disk space
		// Simplified implementation
		return nil
	}
}

// NetworkCheck creates a network connectivity health check
func NetworkCheck(host string, port int) CheckFunction {
	return func(ctx context.Context) error {
		// This would test network connectivity
		// Simplified implementation
		return nil
	}
}

// ServiceStatus represents the health status of the entire service
type ServiceStatus struct {
	Status    Status                 `json:"status"`
	Timestamp time.Time              `json:"timestamp"`
	Uptime    time.Duration          `json:"uptime"`
	Version   string                 `json:"version,omitempty"`
	Checks    map[string]*Result     `json:"checks"`
	Stats     Stats                  `json:"stats"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// NewServiceStatus creates a comprehensive service status
func (c *Checker) NewServiceStatus(version string, metadata map[string]interface{}) *ServiceStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	status := &ServiceStatus{
		Status:    c.stats.OverallStatus,
		Timestamp: time.Now(),
		Uptime:    c.stats.SystemUptime,
		Version:   version,
		Checks:    make(map[string]*Result),
		Stats:     c.stats,
		Metadata:  metadata,
	}

	// Copy current results
	for name, result := range c.results {
		status.Checks[name] = result
	}

	return status
}
