package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Collector implements comprehensive metrics collection
type Collector struct {
	mu       sync.RWMutex
	config   *Config
	registry *prometheus.Registry

	// Prometheus metrics
	operationCounter  *prometheus.CounterVec
	operationDuration *prometheus.HistogramVec
	operationSize     *prometheus.HistogramVec
	cacheHitCounter   *prometheus.CounterVec
	cacheSizeGauge    *prometheus.GaugeVec
	activeConnections prometheus.Gauge
	errorCounter      *prometheus.CounterVec
	predictiveGauge   *prometheus.GaugeVec
	accelerationGauge *prometheus.GaugeVec

	// periodic are the callbacks updateLoop invokes on every tick, registered by OnPeriodicUpdate.
	periodic []func()

	// Internal tracking
	operations map[string]*OperationMetrics
	lastReset  time.Time

	// HTTP server for metrics endpoint
	server *http.Server

	// boundAddr is what the listener actually bound, read through Addr. Guarded by mu: Start writes it
	// before the serving goroutine exists, and a test reads it from another goroutine.
	boundAddr string
}

// Config represents metrics configuration
type Config struct {
	Enabled bool `yaml:"enabled"`

	// Addr is the host:port the endpoint binds, default "127.0.0.1:8080".
	//
	// This was Port, an int, and the change is not cosmetic. A port cannot express an interface, so
	// Start built "fmt.Sprintf(\":%d\", Port)" and bound every one of them — /metrics and
	// /debug/operations, unauthenticated, on any host that could route to the mount (#211). Nor could a
	// port say "off": zero meant "unset" to the defaulting below and came back as 8080, so an operator
	// writing 0 to close the port got it opened on the default (#212). Enabled is the only disable
	// switch now, and there is no value of this field that quietly means something else.
	Addr string `yaml:"addr"`

	Path           string            `yaml:"path"`
	Labels         map[string]string `yaml:"labels"`
	Namespace      string            `yaml:"namespace"`
	Subsystem      string            `yaml:"subsystem"`
	UpdateInterval time.Duration     `yaml:"update_interval"`
}

// OperationMetrics tracks metrics for a specific operation type
type OperationMetrics struct {
	Count         int64         `json:"count"`
	TotalDuration time.Duration `json:"total_duration"`
	TotalSize     int64         `json:"total_size"`
	Errors        int64         `json:"errors"`
	LastOperation time.Time     `json:"last_operation"`
	AvgDuration   time.Duration `json:"avg_duration"`
	AvgSize       float64       `json:"avg_size"`
}

// DefaultConfig returns the metrics configuration ObjectFS uses when nothing overrides it.
//
// Exported so a caller building a partial Config can see what the unset fields become, and so the
// defaults exist in exactly one place rather than being restated by each constructor arm.
func DefaultConfig() *Config {
	return &Config{
		Enabled:        true,
		Addr:           "127.0.0.1:8080",
		Path:           "/metrics",
		Namespace:      "objectfs",
		Subsystem:      "",
		UpdateInterval: 30 * time.Second,
		Labels:         make(map[string]string),
	}
}

// NewCollector creates a new metrics collector.
//
// Unset fields are filled from DefaultConfig field-by-field, not only when config is nil. The
// all-or-nothing form this replaced was reachable and fatal: internal/adapter builds a Config
// setting Enabled, Port and Labels and nothing else, which left Path empty — and an empty pattern
// makes http.ServeMux.Handle panic with "invalid pattern" — and UpdateInterval zero, which makes
// time.NewTicker panic with "non-positive interval". Both fire inside Start, one of them on a
// goroutine where no recover can reach it. Namespace was empty on the same path, so every metric
// would have been exported unprefixed: cache_requests_total rather than the documented
// objectfs_cache_requests_total that every dashboard and both SDKs look for.
//
// This is the same shape as s3.NewBackend's defaulting and the same reasoning: a constructor that
// honors its defaults only for callers who pass nothing is a constructor whose documented behavior
// is false for every caller who passes something.
func NewCollector(config *Config) (*Collector, error) {
	if config == nil {
		config = DefaultConfig()
	}

	defaults := DefaultConfig()
	if config.Addr == "" {
		config.Addr = defaults.Addr
	}
	if config.Path == "" {
		config.Path = defaults.Path
	}
	if config.Namespace == "" {
		config.Namespace = defaults.Namespace
	}
	if config.UpdateInterval <= 0 {
		config.UpdateInterval = defaults.UpdateInterval
	}

	if !config.Enabled {
		return &Collector{config: config}, nil
	}

	// Create Prometheus registry
	registry := prometheus.NewRegistry()

	collector := &Collector{
		config:     config,
		registry:   registry,
		operations: make(map[string]*OperationMetrics),
		lastReset:  time.Now(),
	}

	// Initialize Prometheus metrics
	if err := collector.initMetrics(); err != nil {
		return nil, fmt.Errorf("failed to initialize metrics: %w", err)
	}

	// Register metrics with registry
	if err := collector.registerMetrics(); err != nil {
		return nil, fmt.Errorf("failed to register metrics: %w", err)
	}

	return collector, nil
}

// Start binds the metrics endpoint and starts the periodic-update loop.
//
// The listener is created here and not on the goroutine below, so a bind failure is returned to the
// caller. It used to be inside the goroutine, where the error went to stdout with fmt.Printf and
// nothing propagated: a port already in use, or an address the OS refused, left the mount running with
// no endpoint and one line of output the operator had no reason to be watching. Returning it lets
// adapter.Start fail the mount, which is the only place that can say which setting was at fault.
func (c *Collector) Start(ctx context.Context) error {
	if !c.config.Enabled {
		return nil
	}

	// Create HTTP server for metrics endpoint
	mux := http.NewServeMux()
	mux.Handle(c.config.Path, promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))

	// Add health check endpoint
	mux.HandleFunc("/health", c.healthHandler)

	// Add debug endpoints
	mux.HandleFunc("/debug/metrics", c.debugMetricsHandler)
	mux.HandleFunc("/debug/operations", c.debugOperationsHandler)

	// Addr, not a port. fmt.Sprintf(":%d", Port) bound every interface, and there was no way to ask for
	// one — see Config.Addr.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", c.config.Addr)
	if err != nil {
		return fmt.Errorf("binding the metrics endpoint on %s: %w", c.config.Addr, err)
	}

	c.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second, // Prevent Slowloris attacks
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// The bound address, which is not necessarily the configured one: a test asking for port 0 gets
	// whatever the kernel assigned, and Addr is how it finds out.
	c.boundAddr = ln.Addr().String()

	// Start server in background
	go func() {
		if err := c.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server stopped serving", "addr", c.config.Addr, "error", err)
		}
	}()

	// Start periodic updates
	go c.updateLoop(ctx)

	return nil
}

// Addr returns the address the endpoint is bound to, or "" if Start has not bound one.
//
// It exists so a test can assert *where* the listener is rather than that a listener exists. The
// distinction is the whole of #211: the previous wiring test scraped 127.0.0.1 and passed identically
// against a loopback bind and a wildcard bind, because a wildcard bind answers on loopback too.
func (c *Collector) Addr() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.boundAddr
}

// Stop stops the metrics collection server
func (c *Collector) Stop(ctx context.Context) error {
	if c.server != nil {
		return c.server.Shutdown(ctx)
	}
	return nil
}

// RecordOperation records an operation with its metrics
func (c *Collector) RecordOperation(operation string, duration time.Duration, size int64, success bool) {
	if !c.config.Enabled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Update internal tracking
	if metrics, exists := c.operations[operation]; exists {
		metrics.Count++
		metrics.TotalDuration += duration
		metrics.TotalSize += size
		if !success {
			metrics.Errors++
		}
		metrics.LastOperation = time.Now()
		metrics.AvgDuration = time.Duration(int64(metrics.TotalDuration) / metrics.Count)
		metrics.AvgSize = float64(metrics.TotalSize) / float64(metrics.Count)
	} else {
		c.operations[operation] = &OperationMetrics{
			Count:         1,
			TotalDuration: duration,
			TotalSize:     size,
			Errors: func() int64 {
				if success {
					return 0
				} else {
					return 1
				}
			}(),
			LastOperation: time.Now(),
			AvgDuration:   duration,
			AvgSize:       float64(size),
		}
	}

	// Update Prometheus metrics
	c.operationCounter.With(prometheus.Labels{
		"operation": operation,
		"status":    map[bool]string{true: "success", false: "error"}[success],
	}).Inc()
	c.operationDuration.With(prometheus.Labels{
		"operation": operation,
	}).Observe(duration.Seconds())

	if size > 0 {
		c.operationSize.With(prometheus.Labels{
			"operation": operation,
		}).Observe(float64(size))
	}

	if !success {
		c.errorCounter.With(prometheus.Labels{
			"operation": operation,
			"type":      "failure",
		}).Inc()
	}
}

// RecordCacheHit records a cache hit.
func (c *Collector) RecordCacheHit(key string, size int64) {
	if !c.config.Enabled {
		return
	}

	c.cacheHitCounter.With(prometheus.Labels{"type": "hit"}).Inc()
}

// RecordCacheMiss records a cache miss.
func (c *Collector) RecordCacheMiss(key string, size int64) {
	if !c.config.Enabled {
		return
	}

	c.cacheHitCounter.With(prometheus.Labels{"type": "miss"}).Inc()
}

// RecordError records an error
func (c *Collector) RecordError(operation string, err error) {
	if !c.config.Enabled {
		return
	}

	c.errorCounter.With(prometheus.Labels{
		"operation": operation,
		"type":      c.classifyError(err),
	}).Inc()
}

// UpdateCacheSize updates cache size metrics
func (c *Collector) UpdateCacheSize(level string, size int64) {
	if !c.config.Enabled {
		return
	}

	c.cacheSizeGauge.With(prometheus.Labels{
		"level": level,
	}).Set(float64(size))
}

// UpdateActiveConnections updates active connection count
func (c *Collector) UpdateActiveConnections(count int) {
	if !c.config.Enabled {
		return
	}

	c.activeConnections.Set(float64(count))
}

// UpdatePredictiveCache publishes the predictive cache's statistics, one series per named statistic.
//
// Takes a map rather than a cache.PredictiveStats so that internal/metrics does not import
// internal/cache. The adapter imports both and is where the two meet; a metrics package that knows the
// cache's types is one the cache cannot later import, and it makes this family's shape a matter of
// agreement between two packages rather than of one struct.
//
// Names come from the caller and reach the scrape as label values, so they are the contract:
// sdks/testdata/metrics-scrape.txt captures them and TestSDKFixtureMatchesTheLiveScrape fails on a
// rename, which is what makes both SDK suites notice.
func (c *Collector) UpdatePredictiveCache(stats map[string]float64) {
	if !c.config.Enabled {
		return
	}

	for name, value := range stats {
		c.predictiveGauge.With(prometheus.Labels{"statistic": name}).Set(value)
	}
}

// UpdateS3Acceleration publishes the S3 Transfer Acceleration state, one series per named statistic.
//
// A map for the same reason UpdatePredictiveCache takes one: internal/metrics must not import
// internal/storage/s3. The adapter imports both and is where they meet.
//
// The names are the contract, captured in sdks/testdata/metrics-scrape.txt and asserted by
// TestSDKFixtureMatchesTheLiveScrape, so both SDK suites fail on a rename.
func (c *Collector) UpdateS3Acceleration(stats map[string]float64) {
	if !c.config.Enabled {
		return
	}

	for name, value := range stats {
		c.accelerationGauge.With(prometheus.Labels{"statistic": name}).Set(value)
	}
}

// OnPeriodicUpdate registers a callback for updateLoop to invoke on every tick.
//
// This is how a gauge whose value lives elsewhere gets refreshed. Counters and histograms are pushed at
// the moment of the operation, but the predictive cache's totals are state held by the cache, and
// scraping them means asking — so something has to ask on a schedule. updatePeriodicMetrics was an empty
// function with a comment saying "this would update metrics that need periodic updates"; this is that,
// with the caller supplying what to ask rather than this package knowing.
//
// Callbacks run on the update goroutine, sequentially, and must not block for long: one that does delays
// every later callback by the same amount. Registering after Start is safe — updatePeriodicMetrics reads
// the list under the lock on each tick — which the adapter relies on: it starts the collector first,
// because a bind failure should fail the mount before anything else is built, and the cache whose
// statistics it registers does not exist until several steps later.
func (c *Collector) OnPeriodicUpdate(update func()) {
	if update == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.periodic = append(c.periodic, update)
}

// GetMetrics returns current metrics
func (c *Collector) GetMetrics() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()

	metrics := make(map[string]any)

	// Copy operation metrics
	operations := make(map[string]*OperationMetrics)
	for k, v := range c.operations {
		operations[k] = &OperationMetrics{
			Count:         v.Count,
			TotalDuration: v.TotalDuration,
			TotalSize:     v.TotalSize,
			Errors:        v.Errors,
			LastOperation: v.LastOperation,
			AvgDuration:   v.AvgDuration,
			AvgSize:       v.AvgSize,
		}
	}

	metrics["operations"] = operations
	metrics["last_reset"] = c.lastReset
	metrics["uptime"] = time.Since(c.lastReset)

	return metrics
}

// Gatherer returns the Prometheus Gatherer for this collector's registry.
// Returns nil if the collector is disabled.
func (c *Collector) Gatherer() prometheus.Gatherer {
	return c.registry
}

// ResetMetrics resets all metrics
func (c *Collector) ResetMetrics() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.operations = make(map[string]*OperationMetrics)
	c.lastReset = time.Now()
}

// Helper methods

func (c *Collector) initMetrics() error {
	// Operator-supplied labels are attached to every metric as constant labels, which is what
	// monitoring.metrics.custom_labels in the config has always promised. Reading the field here is
	// the whole of that promise: it was declared, defaulted to {service: objectfs}, documented in
	// examples/config.yaml as "attached to every exported metric", mapped through the adapter — and
	// then read by nothing, so the labels appeared on no metric.
	//
	// An unusable label name (or one colliding with a variable label below) makes Register return an
	// error rather than panicking, so a bad value fails the mount at construction with the name in
	// the message instead of exporting silently-unlabelled metrics.
	labels := prometheus.Labels(c.config.Labels)

	// Operation metrics
	c.operationCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   c.config.Namespace,
			Subsystem:   c.config.Subsystem,
			Name:        "operations_total",
			Help:        "Total number of operations",
			ConstLabels: labels,
		},
		[]string{"operation", "status"},
	)

	c.operationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace:   c.config.Namespace,
			Subsystem:   c.config.Subsystem,
			Name:        "operation_duration_seconds",
			Help:        "Duration of operations in seconds",
			Buckets:     prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms to ~32s
			ConstLabels: labels,
		},
		[]string{"operation"},
	)

	c.operationSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace:   c.config.Namespace,
			Subsystem:   c.config.Subsystem,
			Name:        "operation_size_bytes",
			Help:        "Size of operations in bytes",
			Buckets:     prometheus.ExponentialBuckets(1024, 2, 20), // 1KB to ~1GB
			ConstLabels: labels,
		},
		[]string{"operation"},
	)

	// Cache metrics.
	//
	// Labeled by "type" (hit or miss) only. There was a second "source" label meant to carry the
	// cache level, but determineCacheSource returned the constant "unknown" for every key — its own
	// comment said "in practice, this would be passed explicitly" — so the label added a dimension
	// with one value to every series and told a reader nothing. Recording the level means threading it
	// out of internal/cache, which knows it; until that happens, one honest label beats two where one
	// is a placeholder. Per-level sizes are already available on cache_size_bytes{level}.
	c.cacheHitCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   c.config.Namespace,
			Subsystem:   c.config.Subsystem,
			Name:        "cache_requests_total",
			Help:        "Total number of cache requests, by hit or miss",
			ConstLabels: labels,
		},
		[]string{"type"},
	)

	c.cacheSizeGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   c.config.Namespace,
			Subsystem:   c.config.Subsystem,
			Name:        "cache_size_bytes",
			Help:        "Current cache size in bytes",
			ConstLabels: labels,
		},
		[]string{"level"},
	)

	// Connection metrics
	c.activeConnections = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace:   c.config.Namespace,
			Subsystem:   c.config.Subsystem,
			Name:        "active_connections",
			Help:        "Number of active connections",
			ConstLabels: labels,
		},
	)

	// Error metrics
	c.errorCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   c.config.Namespace,
			Subsystem:   c.config.Subsystem,
			Name:        "errors_total",
			Help:        "Total number of errors",
			ConstLabels: labels,
		},
		[]string{"operation", "type"},
	)

	// Predictive cache metrics.
	//
	// One gauge family labeled by "statistic" rather than a metric per number. The values are a mix of
	// monotonic counters and ratios derived from them, and the set will change as the predictive layer
	// does — a label keeps that from being a metric-name change each time, which is a change both SDKs and
	// every dashboard would have to follow.
	//
	// A gauge and not a counter even for the counting ones: they are read by scraping a snapshot of the
	// cache's own totals rather than incremented here, and prometheus.Counter has no Set.
	c.predictiveGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   c.config.Namespace,
			Subsystem:   c.config.Subsystem,
			Name:        "predictive_cache",
			Help:        "Predictive cache statistics, by statistic name",
			ConstLabels: labels,
		},
		[]string{"statistic"},
	)

	// S3 Transfer Acceleration metrics.
	//
	// Same shape as the predictive family above and for the same reasons: one gauge labeled by
	// "statistic", so the set can grow without a metric-name change that both SDKs and every dashboard
	// would have to follow.
	//
	// The family exists because the acceleration state was reachable by nothing outside the s3 package
	// (#204). A mount could fall back to the standard endpoint on its first request and serve everything
	// after that at standard throughput, and no scrape, log line, or health check said so — so the
	// series that matters most here is `active`, which is 0 exactly when the fallback is in effect.
	c.accelerationGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   c.config.Namespace,
			Subsystem:   c.config.Subsystem,
			Name:        "s3_acceleration",
			Help:        "S3 Transfer Acceleration state and counters, by statistic name",
			ConstLabels: labels,
		},
		[]string{"statistic"},
	)

	return nil
}

func (c *Collector) registerMetrics() error {
	metrics := []prometheus.Collector{
		c.operationCounter,
		c.operationDuration,
		c.operationSize,
		c.cacheHitCounter,
		c.cacheSizeGauge,
		c.activeConnections,
		c.errorCounter,
		c.predictiveGauge,
		c.accelerationGauge,
	}

	for _, metric := range metrics {
		if err := c.registry.Register(metric); err != nil {
			return err
		}
	}

	return nil
}

func (c *Collector) classifyError(err error) string {
	errStr := err.Error()
	switch {
	case contains(errStr, "timeout"):
		return "timeout"
	case contains(errStr, "connection"):
		return "connection"
	case contains(errStr, "not found"):
		return "not_found"
	case contains(errStr, "permission"):
		return "permission"
	case contains(errStr, "throttl"):
		return "throttling"
	default:
		return "other"
	}
}

func (c *Collector) updateLoop(ctx context.Context) {
	ticker := time.NewTicker(c.config.UpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.updatePeriodicMetrics()
		}
	}
}

// updatePeriodicMetrics invokes every callback registered through OnPeriodicUpdate.
//
// The callbacks are copied out under the lock and run without it: a callback reads state from another
// subsystem, which may take that subsystem's own lock, and holding this one across that is how two
// packages that each look correct deadlock together.
func (c *Collector) updatePeriodicMetrics() {
	c.mu.RLock()
	callbacks := append([]func(){}, c.periodic...)
	c.mu.RUnlock()

	for _, update := range callbacks {
		update()
	}
}

// HTTP handlers

func (c *Collector) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"healthy","service":"objectfs-metrics"}`)) // Ignore write error for health check
}

func (c *Collector) debugMetricsHandler(w http.ResponseWriter, r *http.Request) {
	metrics := c.GetMetrics()

	w.Header().Set("Content-Type", "application/json")

	// Simple JSON encoding - using helper to avoid errcheck issues
	writef := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

	writef("{\n")
	writef("  \"uptime\": \"%v\",\n", metrics["uptime"])
	writef("  \"last_reset\": \"%v\",\n", metrics["last_reset"])
	writef("  \"operations\": {\n")

	if operations, ok := metrics["operations"].(map[string]*OperationMetrics); ok {
		first := true
		for name, op := range operations {
			if !first {
				writef(",\n")
			}
			writef("    \"%s\": {\n", name)
			writef("      \"count\": %d,\n", op.Count)
			writef("      \"errors\": %d,\n", op.Errors)
			writef("      \"avg_duration\": \"%v\",\n", op.AvgDuration)
			writef("      \"avg_size\": %.2f\n", op.AvgSize)
			writef("    }")
			first = false
		}
	}

	writef("\n  }\n")
	writef("}\n")
}

func (c *Collector) debugOperationsHandler(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain")

	// Helper to avoid errcheck issues
	writef := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

	writef("ObjectFS Operations Summary\n")
	writef("==========================\n\n")
	writef("Uptime: %v\n", time.Since(c.lastReset))
	writef("Last Reset: %v\n\n", c.lastReset)

	if len(c.operations) == 0 {
		writef("No operations recorded.\n")
		return
	}

	writef("%-20s %10s %10s %12s %12s %10s\n",
		"Operation", "Count", "Errors", "Avg Duration", "Avg Size", "Last Op")
	writef("%-20s %10s %10s %12s %12s %10s\n",
		"----------", "-----", "------", "------------", "--------", "-------")

	for name, op := range c.operations {
		writef("%-20s %10d %10d %12v %12.0f %10s\n",
			name, op.Count, op.Errors, op.AvgDuration,
			op.AvgSize, op.LastOperation.Format("15:04:05"))
	}
}

// Utility functions

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		(len(s) > len(substr) && indexOf(s, substr) >= 0))
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
