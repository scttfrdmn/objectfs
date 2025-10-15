// Package profiling provides memory profiling and leak detection capabilities
package profiling

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"runtime"
	"runtime/debug"
	"sync"
	"time"
)

// MemoryMonitor provides memory monitoring and leak detection
type MemoryMonitor struct {
	mu              sync.RWMutex
	config          MonitorConfig
	samples         []MemorySample
	maxSamples      int
	server          *http.Server
	alertThresholds AlertThresholds
	lastGC          time.Time
	baselineHeap    uint64
	peakHeap        uint64
	alertCallbacks  []AlertCallback
	metricsEnabled  bool
}

// MonitorConfig configures memory monitoring
type MonitorConfig struct {
	// Enabled turns on memory monitoring
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Port for pprof HTTP server
	Port int `yaml:"port" json:"port"`

	// SampleInterval for memory sampling
	SampleInterval time.Duration `yaml:"sample_interval" json:"sample_interval"`

	// MaxSamples to keep in history
	MaxSamples int `yaml:"max_samples" json:"max_samples"`

	// GCPercent sets GOGC (default 100)
	GCPercent int `yaml:"gc_percent" json:"gc_percent"`

	// EnablePprof enables pprof endpoints
	EnablePprof bool `yaml:"enable_pprof" json:"enable_pprof"`

	// EnableMetrics exports memory metrics
	EnableMetrics bool `yaml:"enable_metrics" json:"enable_metrics"`
}

// AlertThresholds defines memory alert thresholds
type AlertThresholds struct {
	// HeapSizeMB triggers alert when heap exceeds this size
	HeapSizeMB uint64 `yaml:"heap_size_mb" json:"heap_size_mb"`

	// HeapGrowthPercent triggers alert when heap grows by this percent
	HeapGrowthPercent float64 `yaml:"heap_growth_percent" json:"heap_growth_percent"`

	// GoroutineCount triggers alert when goroutines exceed this count
	GoroutineCount int `yaml:"goroutine_count" json:"goroutine_count"`

	// AllocationRateMBPerSec triggers alert when allocation rate exceeds this
	AllocationRateMBPerSec float64 `yaml:"allocation_rate_mb_per_sec" json:"allocation_rate_mb_per_sec"`
}

// MemorySample represents a point-in-time memory snapshot
type MemorySample struct {
	Timestamp     time.Time `json:"timestamp"`
	HeapAlloc     uint64    `json:"heap_alloc"`      // bytes allocated on heap
	HeapSys       uint64    `json:"heap_sys"`        // bytes obtained from system
	HeapIdle      uint64    `json:"heap_idle"`       // bytes in idle spans
	HeapInuse     uint64    `json:"heap_inuse"`      // bytes in non-idle spans
	HeapReleased  uint64    `json:"heap_released"`   // bytes released to OS
	NumGC         uint32    `json:"num_gc"`          // number of completed GC cycles
	NumGoroutine  int       `json:"num_goroutine"`   // number of goroutines
	Alloc         uint64    `json:"alloc"`           // bytes allocated (cumulative)
	TotalAlloc    uint64    `json:"total_alloc"`     // cumulative bytes allocated
	Sys           uint64    `json:"sys"`             // total bytes from system
	Mallocs       uint64    `json:"mallocs"`         // cumulative count of mallocs
	Frees         uint64    `json:"frees"`           // cumulative count of frees
	LiveObjects   uint64    `json:"live_objects"`    // mallocs - frees
	GCPauseNs     uint64    `json:"gc_pause_ns"`     // last GC pause time
	GCCPUFraction float64   `json:"gc_cpu_fraction"` // fraction of CPU time in GC
}

// AlertCallback is called when a memory alert is triggered
type AlertCallback func(alert Alert)

// Alert represents a memory alert
type Alert struct {
	Timestamp time.Time    `json:"timestamp"`
	Level     AlertLevel   `json:"level"`
	Type      string       `json:"type"`
	Message   string       `json:"message"`
	Current   MemorySample `json:"current"`
	Threshold interface{}  `json:"threshold"`
}

// AlertLevel represents alert severity
type AlertLevel int

const (
	// AlertInfo for informational alerts
	AlertInfo AlertLevel = iota
	// AlertWarning for warning alerts
	AlertWarning
	// AlertCritical for critical alerts
	AlertCritical
)

func (l AlertLevel) String() string {
	switch l {
	case AlertInfo:
		return "info"
	case AlertWarning:
		return "warning"
	case AlertCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// DefaultMonitorConfig returns default memory monitoring configuration
func DefaultMonitorConfig() MonitorConfig {
	return MonitorConfig{
		Enabled:        true,
		Port:           6060,
		SampleInterval: 10 * time.Second,
		MaxSamples:     1000,
		GCPercent:      100,
		EnablePprof:    true,
		EnableMetrics:  true,
	}
}

// DefaultAlertThresholds returns default alert thresholds
func DefaultAlertThresholds() AlertThresholds {
	return AlertThresholds{
		HeapSizeMB:             1024,  // 1GB
		HeapGrowthPercent:      50.0,  // 50% growth
		GoroutineCount:         10000, // 10k goroutines
		AllocationRateMBPerSec: 100.0, // 100MB/sec
	}
}

// NewMemoryMonitor creates a new memory monitor
func NewMemoryMonitor(config MonitorConfig, thresholds AlertThresholds) *MemoryMonitor {
	if config.MaxSamples <= 0 {
		config.MaxSamples = 1000
	}

	// Set GC percentage
	if config.GCPercent > 0 {
		debug.SetGCPercent(config.GCPercent)
	}

	return &MemoryMonitor{
		config:          config,
		samples:         make([]MemorySample, 0, config.MaxSamples),
		maxSamples:      config.MaxSamples,
		alertThresholds: thresholds,
		lastGC:          time.Now(),
		alertCallbacks:  make([]AlertCallback, 0),
		metricsEnabled:  config.EnableMetrics,
	}
}

// Start starts memory monitoring
func (m *MemoryMonitor) Start(ctx context.Context) error {
	if !m.config.Enabled {
		return nil
	}

	// Take baseline sample
	m.takeBaseline()

	// Start HTTP server with pprof endpoints
	if m.config.EnablePprof {
		if err := m.startPprofServer(); err != nil {
			return fmt.Errorf("failed to start pprof server: %w", err)
		}
	}

	// Start sampling loop
	go m.sampleLoop(ctx)

	// Start GC monitoring
	go m.gcMonitor(ctx)

	log.Printf("Memory monitoring started on :%d", m.config.Port)
	return nil
}

// Stop stops memory monitoring
func (m *MemoryMonitor) Stop(ctx context.Context) error {
	if m.server != nil {
		return m.server.Shutdown(ctx)
	}
	return nil
}

// AddAlertCallback adds a callback for memory alerts
func (m *MemoryMonitor) AddAlertCallback(callback AlertCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alertCallbacks = append(m.alertCallbacks, callback)
}

// GetCurrentSample returns current memory statistics
func (m *MemoryMonitor) GetCurrentSample() MemorySample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return m.createSample(&ms)
}

// GetSamples returns all memory samples
func (m *MemoryMonitor) GetSamples() []MemorySample {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]MemorySample, len(m.samples))
	copy(result, m.samples)
	return result
}

// GetSamplesSince returns samples since a given time
func (m *MemoryMonitor) GetSamplesSince(since time.Time) []MemorySample {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]MemorySample, 0)
	for _, sample := range m.samples {
		if sample.Timestamp.After(since) {
			result = append(result, sample)
		}
	}
	return result
}

// GetMemoryStats returns detailed memory statistics
func (m *MemoryMonitor) GetMemoryStats() MemoryStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	current := m.GetCurrentSample()

	stats := MemoryStats{
		Current:      current,
		BaselineHeap: m.baselineHeap,
		PeakHeap:     m.peakHeap,
		SampleCount:  len(m.samples),
	}

	// Calculate growth
	if m.baselineHeap > 0 {
		stats.HeapGrowth = float64(current.HeapAlloc-m.baselineHeap) / float64(m.baselineHeap) * 100.0
	}

	// Calculate allocation rate (bytes/sec)
	if len(m.samples) >= 2 {
		oldest := m.samples[0]
		duration := current.Timestamp.Sub(oldest.Timestamp).Seconds()
		if duration > 0 {
			stats.AllocationRate = float64(current.TotalAlloc-oldest.TotalAlloc) / duration
		}
	}

	return stats
}

// MemoryStats provides aggregated memory statistics
type MemoryStats struct {
	Current        MemorySample `json:"current"`
	BaselineHeap   uint64       `json:"baseline_heap"`
	PeakHeap       uint64       `json:"peak_heap"`
	HeapGrowth     float64      `json:"heap_growth_percent"`
	AllocationRate float64      `json:"allocation_rate_bytes_per_sec"`
	SampleCount    int          `json:"sample_count"`
}

// ForceGC triggers garbage collection
func (m *MemoryMonitor) ForceGC() {
	log.Println("Forcing garbage collection...")
	beforeHeap := m.GetCurrentSample().HeapAlloc
	runtime.GC()
	afterHeap := m.GetCurrentSample().HeapAlloc
	freed := beforeHeap - afterHeap
	log.Printf("GC completed: freed %d MB", freed/(1024*1024))
}

// FreeOSMemory returns memory to the OS
func (m *MemoryMonitor) FreeOSMemory() {
	log.Println("Returning memory to OS...")
	debug.FreeOSMemory()
	log.Println("Memory returned to OS")
}

// GetGoroutineCount returns current goroutine count
func (m *MemoryMonitor) GetGoroutineCount() int {
	return runtime.NumGoroutine()
}

// Private methods

func (m *MemoryMonitor) takeBaseline() {
	runtime.GC() // Force GC to get clean baseline
	sample := m.GetCurrentSample()
	m.baselineHeap = sample.HeapAlloc
	m.peakHeap = sample.HeapAlloc
	m.addSample(sample)
	log.Printf("Memory baseline: %.2f MB", float64(m.baselineHeap)/(1024*1024))
}

func (m *MemoryMonitor) sampleLoop(ctx context.Context) {
	ticker := time.NewTicker(m.config.SampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sample := m.GetCurrentSample()
			m.addSample(sample)
			m.checkAlerts(sample)
		}
	}
}

func (m *MemoryMonitor) gcMonitor(ctx context.Context) {
	// Monitor GC statistics
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)

			gcFraction := ms.GCCPUFraction
			if gcFraction > 0.10 { // More than 10% CPU time in GC
				m.triggerAlert(Alert{
					Timestamp: time.Now(),
					Level:     AlertWarning,
					Type:      "gc_cpu_high",
					Message:   fmt.Sprintf("GC CPU usage high: %.2f%%", gcFraction*100),
					Threshold: 10.0,
				})
			}
		}
	}
}

func (m *MemoryMonitor) addSample(sample MemorySample) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Update peak heap
	if sample.HeapAlloc > m.peakHeap {
		m.peakHeap = sample.HeapAlloc
	}

	// Add sample
	m.samples = append(m.samples, sample)

	// Trim if exceeds max
	if len(m.samples) > m.maxSamples {
		m.samples = m.samples[1:]
	}
}

func (m *MemoryMonitor) createSample(ms *runtime.MemStats) MemorySample {
	return MemorySample{
		Timestamp:     time.Now(),
		HeapAlloc:     ms.HeapAlloc,
		HeapSys:       ms.HeapSys,
		HeapIdle:      ms.HeapIdle,
		HeapInuse:     ms.HeapInuse,
		HeapReleased:  ms.HeapReleased,
		NumGC:         ms.NumGC,
		NumGoroutine:  runtime.NumGoroutine(),
		Alloc:         ms.Alloc,
		TotalAlloc:    ms.TotalAlloc,
		Sys:           ms.Sys,
		Mallocs:       ms.Mallocs,
		Frees:         ms.Frees,
		LiveObjects:   ms.Mallocs - ms.Frees,
		GCPauseNs:     ms.PauseNs[(ms.NumGC+255)%256],
		GCCPUFraction: ms.GCCPUFraction,
	}
}

func (m *MemoryMonitor) checkAlerts(sample MemorySample) {
	// Check heap size
	heapMB := sample.HeapAlloc / (1024 * 1024)
	if m.alertThresholds.HeapSizeMB > 0 && heapMB > m.alertThresholds.HeapSizeMB {
		m.triggerAlert(Alert{
			Timestamp: sample.Timestamp,
			Level:     AlertWarning,
			Type:      "heap_size",
			Message:   fmt.Sprintf("Heap size exceeded threshold: %d MB > %d MB", heapMB, m.alertThresholds.HeapSizeMB),
			Current:   sample,
			Threshold: m.alertThresholds.HeapSizeMB,
		})
	}

	// Check heap growth
	if m.baselineHeap > 0 && m.alertThresholds.HeapGrowthPercent > 0 {
		growth := float64(sample.HeapAlloc-m.baselineHeap) / float64(m.baselineHeap) * 100.0
		if growth > m.alertThresholds.HeapGrowthPercent {
			m.triggerAlert(Alert{
				Timestamp: sample.Timestamp,
				Level:     AlertCritical,
				Type:      "heap_growth",
				Message:   fmt.Sprintf("Heap growth exceeded threshold: %.2f%% > %.2f%%", growth, m.alertThresholds.HeapGrowthPercent),
				Current:   sample,
				Threshold: m.alertThresholds.HeapGrowthPercent,
			})
		}
	}

	// Check goroutine count
	if m.alertThresholds.GoroutineCount > 0 && sample.NumGoroutine > m.alertThresholds.GoroutineCount {
		m.triggerAlert(Alert{
			Timestamp: sample.Timestamp,
			Level:     AlertWarning,
			Type:      "goroutine_count",
			Message:   fmt.Sprintf("Goroutine count exceeded threshold: %d > %d", sample.NumGoroutine, m.alertThresholds.GoroutineCount),
			Current:   sample,
			Threshold: m.alertThresholds.GoroutineCount,
		})
	}
}

func (m *MemoryMonitor) triggerAlert(alert Alert) {
	log.Printf("[%s] Memory Alert: %s - %s", alert.Level, alert.Type, alert.Message)

	m.mu.RLock()
	callbacks := make([]AlertCallback, len(m.alertCallbacks))
	copy(callbacks, m.alertCallbacks)
	m.mu.RUnlock()

	for _, callback := range callbacks {
		go callback(alert)
	}
}

func (m *MemoryMonitor) startPprofServer() error {
	mux := http.NewServeMux()

	// Standard pprof endpoints
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))

	// Custom memory endpoints
	mux.HandleFunc("/memory/stats", m.handleMemoryStats)
	mux.HandleFunc("/memory/samples", m.handleMemorySamples)
	mux.HandleFunc("/memory/gc", m.handleForceGC)
	mux.HandleFunc("/memory/free", m.handleFreeMemory)

	m.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", m.config.Port),
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Memory profiling server error: %v", err)
		}
	}()

	return nil
}

// HTTP handlers

func (m *MemoryMonitor) handleMemoryStats(w http.ResponseWriter, r *http.Request) {
	stats := m.GetMemoryStats()

	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, `{
  "current": {
    "heap_alloc_mb": %.2f,
    "heap_sys_mb": %.2f,
    "heap_inuse_mb": %.2f,
    "num_goroutine": %d,
    "num_gc": %d,
    "gc_cpu_fraction": %.4f
  },
  "baseline_heap_mb": %.2f,
  "peak_heap_mb": %.2f,
  "heap_growth_percent": %.2f,
  "allocation_rate_mb_per_sec": %.2f,
  "sample_count": %d
}`,
		float64(stats.Current.HeapAlloc)/(1024*1024),
		float64(stats.Current.HeapSys)/(1024*1024),
		float64(stats.Current.HeapInuse)/(1024*1024),
		stats.Current.NumGoroutine,
		stats.Current.NumGC,
		stats.Current.GCCPUFraction,
		float64(stats.BaselineHeap)/(1024*1024),
		float64(stats.PeakHeap)/(1024*1024),
		stats.HeapGrowth,
		stats.AllocationRate/(1024*1024),
		stats.SampleCount,
	); err != nil {
		log.Printf("Failed to write memory stats: %v", err)
	}
}

func (m *MemoryMonitor) handleMemorySamples(w http.ResponseWriter, r *http.Request) {
	samples := m.GetSamples()

	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, `{"samples": [`); err != nil {
		log.Printf("Failed to write samples header: %v", err)
		return
	}

	for i, sample := range samples {
		if i > 0 {
			if _, err := fmt.Fprintf(w, ","); err != nil {
				log.Printf("Failed to write sample separator: %v", err)
				return
			}
		}
		if _, err := fmt.Fprintf(w, `
  {
    "timestamp": "%s",
    "heap_alloc_mb": %.2f,
    "num_goroutine": %d
  }`, sample.Timestamp.Format(time.RFC3339),
			float64(sample.HeapAlloc)/(1024*1024),
			sample.NumGoroutine); err != nil {
			log.Printf("Failed to write sample: %v", err)
			return
		}
	}

	if _, err := fmt.Fprintf(w, `
], "count": %d}`, len(samples)); err != nil {
		log.Printf("Failed to write samples footer: %v", err)
	}
}

func (m *MemoryMonitor) handleForceGC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	m.ForceGC()
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, `{"status": "gc_triggered"}`); err != nil {
		log.Printf("Failed to write GC response: %v", err)
	}
}

func (m *MemoryMonitor) handleFreeMemory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	m.FreeOSMemory()
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, `{"status": "memory_freed"}`); err != nil {
		log.Printf("Failed to write free memory response: %v", err)
	}
}
