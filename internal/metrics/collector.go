package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

)

// Collector implements comprehensive metrics collection
type Collector struct {
	mu            sync.RWMutex
	config        *Config
	registry      *prometheus.Registry
	
	// Prometheus metrics
	operationCounter    *prometheus.CounterVec
	operationDuration   *prometheus.HistogramVec
	operationSize       *prometheus.HistogramVec
	cacheHitCounter     *prometheus.CounterVec
	cacheSizeGauge      *prometheus.GaugeVec
	activeConnections   prometheus.Gauge
	errorCounter        *prometheus.CounterVec
	
	// Internal tracking
	operations        map[string]*OperationMetrics
	lastReset         time.Time
	
	// HTTP server for metrics endpoint
	server *http.Server
}

// Config represents metrics configuration
type Config struct {
	Enabled        bool              `yaml:"enabled"`
	Port           int               `yaml:"port"`
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

// NewCollector creates a new metrics collector
func NewCollector(config *Config) (*Collector, error) {
	if config == nil {
		config = &Config{
			Enabled:        true,
			Port:           8080,
			Path:           "/metrics",
			Namespace:      "objectfs",
			Subsystem:      "",
			UpdateInterval: 30 * time.Second,
			Labels:         make(map[string]string),
		}
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

// Start starts the metrics collection server
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

	c.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", c.config.Port),
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second, // Prevent Slowloris attacks
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Start server in background
	go func() {
		if err := c.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Metrics server error: %v\n", err)
		}
	}()

	// Start periodic updates
	go c.updateLoop(ctx)

	return nil
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
			Errors:        func() int64 { if success { return 0 } else { return 1 } }(),
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

// RecordCacheHit records a cache hit
func (c *Collector) RecordCacheHit(key string, size int64) {
	if !c.config.Enabled {
		return
	}

	c.cacheHitCounter.With(prometheus.Labels{
		"type":   "hit",
		"source": c.determineCacheSource(key),
	}).Inc()
}

// RecordCacheMiss records a cache miss
func (c *Collector) RecordCacheMiss(key string, size int64) {
	if !c.config.Enabled {
		return
	}

	c.cacheHitCounter.With(prometheus.Labels{
		"type":   "miss",
		"source": c.determineCacheSource(key),
	}).Inc()
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

// GetMetrics returns current metrics
func (c *Collector) GetMetrics() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()

	metrics := make(map[string]interface{})
	
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

// ResetMetrics resets all metrics
func (c *Collector) ResetMetrics() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.operations = make(map[string]*OperationMetrics)
	c.lastReset = time.Now()
}

// Helper methods

func (c *Collector) initMetrics() error {
	// Operation metrics
	c.operationCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: c.config.Namespace,
			Subsystem: c.config.Subsystem,
			Name:      "operations_total",
			Help:      "Total number of operations",
		},
		[]string{"operation", "status"},
	)

	c.operationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: c.config.Namespace,
			Subsystem: c.config.Subsystem,
			Name:      "operation_duration_seconds",
			Help:      "Duration of operations in seconds",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms to ~32s
		},
		[]string{"operation"},
	)

	c.operationSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: c.config.Namespace,
			Subsystem: c.config.Subsystem,
			Name:      "operation_size_bytes",
			Help:      "Size of operations in bytes",
			Buckets:   prometheus.ExponentialBuckets(1024, 2, 20), // 1KB to ~1GB
		},
		[]string{"operation"},
	)

	// Cache metrics
	c.cacheHitCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: c.config.Namespace,
			Subsystem: c.config.Subsystem,
			Name:      "cache_requests_total",
			Help:      "Total number of cache requests",
		},
		[]string{"type", "source"},
	)

	c.cacheSizeGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: c.config.Namespace,
			Subsystem: c.config.Subsystem,
			Name:      "cache_size_bytes",
			Help:      "Current cache size in bytes",
		},
		[]string{"level"},
	)

	// Connection metrics
	c.activeConnections = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: c.config.Namespace,
			Subsystem: c.config.Subsystem,
			Name:      "active_connections",
			Help:      "Number of active connections",
		},
	)

	// Error metrics
	c.errorCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: c.config.Namespace,
			Subsystem: c.config.Subsystem,
			Name:      "errors_total",
			Help:      "Total number of errors",
		},
		[]string{"operation", "type"},
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
	}

	for _, metric := range metrics {
		if err := c.registry.Register(metric); err != nil {
			return err
		}
	}

	return nil
}


func (c *Collector) determineCacheSource(key string) string {
	// Simple heuristic to determine cache level
	// In practice, this would be passed explicitly
	return "unknown"
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

func (c *Collector) updatePeriodicMetrics() {
	// This would update metrics that need periodic updates
	// For example, current cache sizes, connection counts, etc.
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
	writef := func(format string, args ...interface{}) { _, _ = fmt.Fprintf(w, format, args...) }
	
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
	writef := func(format string, args ...interface{}) { _, _ = fmt.Fprintf(w, format, args...) }
	
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