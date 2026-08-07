package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
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
	HTTPEnabled bool `yaml:"http_enabled"`

	// HTTPAddr is the host:port the endpoint binds, default "127.0.0.1:8081".
	//
	// This was HTTPPort, an int, which could only ever produce ":%d" — the wildcard. /health reports
	// component names, error strings and check timings with no authentication, and it is on by default,
	// so a stock mount published that to anything that could route to it (#211). The port range is also
	// checked at config load now rather than at bind: 99999 is a config an operator could write, and it
	// failed in the address parse on this goroutine, logged, and returned, leaving the mount up with no
	// endpoint (#192).
	HTTPAddr string `yaml:"http_addr"`

	HTTPPath string `yaml:"http_path"`
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

// Category labels which subsystem a health check covers, for grouping in reports.
//
// It is a label and not a switch: nothing in this package branches on a Category. It is assigned by
// [Monitor.mapComponentTypeToCategory] from a component-type string, stored on the check, and
// serialized — so adding a category needs no dispatch updated, and a check filed under the wrong one
// still runs identically. [Priority] and [Status] below are the same kind of string enum; Status is
// the one that carries meaning, being what [Checker] aggregates into an overall verdict.
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

// severityRank orders the priorities, most severe highest. Unknown values rank below Low.
//
// A map and not the constants' own ordering, because these are strings and `>=` on them compares
// bytes: "critical" < "high" < "low" < "medium" is the order the language sees, which is neither the
// declaration order nor the severity order and gets Critical and High exactly backwards.
var severityRank = map[Priority]int{
	PriorityCritical: 4,
	PriorityHigh:     3,
	PriorityMedium:   2,
	PriorityLow:      1,
}

// AtLeast reports whether p is at least as severe as other.
//
// This exists because `severity >= PriorityHigh` was written once, in EnhancedMonitor's auto-remediation
// gate, and Priority is a string: that expression asked whether the byte sequence sorted at or after
// "high", so it was true for "high", "low" and "medium" and false for "critical" — the two Critical
// issues detectProblems raises, a critical error rate and a critical failure streak, were the only ones
// auto-remediation refused to act on, while a merely elevated latency got it. Verified by execution.
//
// [Status] is the other string enum here and is never ordered — Checker.updateStats switches on it.
// [Category] is a label. Priority is the only one of the three with a severity order, so it is the only
// one that needs this.
func (p Priority) AtLeast(other Priority) bool {
	return severityRank[p] >= severityRank[other]
}

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
			HTTPAddr:         "127.0.0.1:8081",
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

	// Start the background check loop, on the caller's context values but not its cancellation.
	//
	// It used to build each round's context from context.Background(), which discarded ctx entirely:
	// anything the caller attached — a trace ID, a deadline-bearing parent, a test's t.Context — was
	// invisible to every check function this loop ever ran, even though Start was handed it. Deriving
	// from ctx directly is the other wrong answer, because Stop is what ends this loop and a
	// request-scoped caller would otherwise silently take health checking down with it.
	//
	// context.WithoutCancel is the same reasoning serveHealth below applies to the HTTP listener, for
	// the same lifetime mismatch. The per-round timeout is still derived in checkLoop.
	go c.checkLoop(context.WithoutCancel(ctx))

	// Start the HTTP endpoint if enabled.
	//
	// The bind happens here, before returning, and its error fails Start. It used to happen on a
	// goroutine that logged and returned, which left the mount running with no health endpoint and one
	// line in the log to say why — and #192, which reported the unvalidated port, called that
	// non-fatal behavior "the right call for observability". It is the opposite: an operator who asked
	// for the endpoint and did not get it learns from a scrape failing later, and `enabled: false` is
	// already how you ask for no endpoint. Config validation now rejects a malformed or out-of-range
	// address at load naming the field, so what reaches here is a real bind failure — the address is
	// taken, or the OS refused it — which is worth a startup error the operator can act on immediately.
	//
	// context.WithoutCancel: the listener outlives the Start call, and this ctx is the caller's
	// request-scoped one. Binding it directly would tear the endpoint down the moment the caller
	// canceled — Stop is what ends this server.
	if c.config.HTTPEnabled {
		serveCtx := context.WithoutCancel(ctx)

		ln, err := c.listenHealth(serveCtx)
		if err != nil {
			c.started = false
			return err
		}

		go c.serveHealth(serveCtx, ln)
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
	for range checks {
		result := <-resultsChan
		results[result.Check] = result
	}

	// Update stored results
	c.mu.Lock()
	maps.Copy(c.results, results)
	c.updateStats()
	c.mu.Unlock()

	return results, nil
}

// GetStatus returns the current health status
func (c *Checker) GetStatus() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()

	status := make(map[string]any)
	status["overall_status"] = c.stats.OverallStatus
	status["timestamp"] = time.Now()
	status["uptime"] = time.Since(c.lastUpdate)
	status["stats"] = c.stats

	// Add individual check results
	checks := make(map[string]*Result)
	maps.Copy(checks, c.results)
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

// checkLoop runs every registered check on a ticker until Stop closes stopCh.
//
// parent carries the values Start was called with and none of its cancellation — see Start for why the
// two are separated. Each round gets its own timeout derived from it, so a check function that hangs
// bounds one round rather than the loop.
func (c *Checker) checkLoop(parent context.Context) {
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
			ctx, cancel := context.WithTimeout(parent, c.config.Timeout*2)
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

// listenHealth binds the configured address for the health endpoint.
//
// The bind is separated from the serving so that each half is testable, and so that Start can return
// the bind error rather than log it from a goroutine. Before the split the only coverage this had came
// from tests *failing* to bind: eight tests in this package start a Monitor with the default Config,
// which enabled the endpoint on the fixed port 8081, so the first to reach it won and the rest took
// the error arm. How many did was a matter of goroutine scheduling — the package measured 45.0%
// coverage on an idle machine and 44.7% under CI's load, which is how a per-package floor came to be
// set half a statement above what the tests reliably reach.
//
// The address, not a port. fmt.Sprintf(":%d", HTTPPort) could only bind every interface; see
// Config.HTTPAddr.
func (c *Checker) listenHealth(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", c.config.HTTPAddr)
	if err != nil {
		return nil, fmt.Errorf("binding the health endpoint on %s: %w", c.config.HTTPAddr, err)
	}

	return ln, nil
}

// serveHealth serves the health endpoint on ln, returning when Stop closes stopCh.
//
// Split out from startHTTPServer so a test can supply its own listener on port 0 and address the
// endpoint it gets back. Binding a fixed port in a test is what made this package's coverage a
// function of how many parallel tests collided on 8081.
//
// ctx is the parent of the shutdown deadline below rather than a second stop signal: closing stopCh
// is what ends this server. Deriving the shutdown timeout from it instead of from context.Background
// means a caller that has already given up does not get an extra five seconds of graceful drain it
// is no longer waiting for.
func (c *Checker) serveHealth(ctx context.Context, ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc(c.config.HTTPPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := c.GetStatus()
		if c.IsHealthy() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"status":    status["overall_status"],
			"timestamp": status["timestamp"],
			"checks":    status["checks"],
		}); err != nil {
			slog.Error("health: error writing HTTP response", "error", err)
		}
	})

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-c.stopCh
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	slog.Info("health: HTTP server listening", "addr", ln.Addr())
	if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
		slog.Error("health: HTTP server error", "error", err)
	}
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
	Status    Status             `json:"status"`
	Timestamp time.Time          `json:"timestamp"`
	Uptime    time.Duration      `json:"uptime"`
	Version   string             `json:"version,omitempty"`
	Checks    map[string]*Result `json:"checks"`
	Stats     Stats              `json:"stats"`
	Metadata  map[string]any     `json:"metadata,omitempty"`
}

// NewServiceStatus creates a comprehensive service status
func (c *Checker) NewServiceStatus(version string, metadata map[string]any) *ServiceStatus {
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
	maps.Copy(status.Checks, c.results)

	return status
}
