// Package memmon provides comprehensive memory monitoring and leak detection for ObjectFS
package memmon

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/objectfs/objectfs/pkg/utils"
)

// MonitorConfig configures memory monitoring behavior
type MonitorConfig struct {
	// SampleInterval is how often to collect memory stats
	SampleInterval time.Duration

	// AlertThreshold is the percentage of memory growth that triggers an alert
	AlertThreshold float64

	// MaxSamples is the number of samples to keep in history
	MaxSamples int

	// EnableGCStats enables garbage collection statistics tracking
	EnableGCStats bool

	// EnableStackTrace enables stack trace collection for allocations
	EnableStackTrace bool

	// GCPercentage sets GOGC percentage (default 100, 0 = disable)
	GCPercentage int

	// Logger for monitoring events
	Logger *utils.StructuredLogger
}

// DefaultMonitorConfig returns sensible defaults
func DefaultMonitorConfig() MonitorConfig {
	return MonitorConfig{
		SampleInterval:   30 * time.Second,
		AlertThreshold:   20.0, // Alert on 20% growth
		MaxSamples:       100,
		EnableGCStats:    true,
		EnableStackTrace: false,
		GCPercentage:     100,
	}
}

// MemoryMonitor tracks memory usage and detects potential leaks
type MemoryMonitor struct {
	config MonitorConfig
	logger *utils.StructuredLogger

	mu              sync.RWMutex
	samples         []MemorySample
	baselineSet     bool
	baselineSample  MemorySample
	currentSample   MemorySample
	alerts          []MemoryAlert
	trackingObjects map[string]*TrackedObject

	stopCh chan struct{}
	wg     sync.WaitGroup
	active int32
}

// MemorySample represents a memory usage sample
type MemorySample struct {
	Timestamp     time.Time
	Alloc         uint64      // bytes allocated and still in use
	TotalAlloc    uint64      // bytes allocated (even if freed)
	Sys           uint64      // bytes obtained from system
	NumGC         uint32      // number of completed GC cycles
	NumGoroutine  int         // number of goroutines
	HeapAlloc     uint64      // bytes allocated in heap
	HeapSys       uint64      // bytes obtained from system for heap
	HeapIdle      uint64      // bytes in idle spans
	HeapInuse     uint64      // bytes in in-use spans
	StackInuse    uint64      // bytes used by stack allocator
	MSpanInuse    uint64      // bytes of allocated mspan structures
	MCacheInuse   uint64      // bytes of allocated mcache structures
	GCCPUFraction float64     // fraction of CPU time used by GC
	PauseNs       [256]uint64 // circular buffer of recent GC pause durations
	PauseTotalNs  uint64      // cumulative nanoseconds in GC stop-the-world pauses
}

// MemoryAlert represents a memory alert
type MemoryAlert struct {
	Timestamp   time.Time
	AlertType   AlertType
	Message     string
	CurrentMem  uint64
	BaselineMem uint64
	GrowthPct   float64
}

// AlertType represents the type of memory alert
type AlertType int

const (
	AlertTypeMemoryGrowth AlertType = iota
	AlertTypeGoroutineLeak
	AlertTypeGCPressure
	AlertTypeHeapFragmentation
)

// String returns the string representation of alert type
func (t AlertType) String() string {
	switch t {
	case AlertTypeMemoryGrowth:
		return "memory_growth"
	case AlertTypeGoroutineLeak:
		return "goroutine_leak"
	case AlertTypeGCPressure:
		return "gc_pressure"
	case AlertTypeHeapFragmentation:
		return "heap_fragmentation"
	default:
		return "unknown"
	}
}

// TrackedObject represents an object being tracked for leaks
type TrackedObject struct {
	Name           string
	Count          int64
	Size           int64
	LastIncrement  time.Time
	LastDecrement  time.Time
	AlertThreshold int64
}

// NewMemoryMonitor creates a new memory monitor
func NewMemoryMonitor(config MonitorConfig) *MemoryMonitor {
	if config.Logger == nil {
		loggerConfig := utils.DefaultStructuredLoggerConfig()
		logger, _ := utils.NewStructuredLogger(loggerConfig)
		config.Logger = logger
	}

	// Set GOGC if specified
	if config.GCPercentage > 0 {
		debug.SetGCPercent(config.GCPercentage)
	}

	return &MemoryMonitor{
		config:          config,
		logger:          config.Logger,
		samples:         make([]MemorySample, 0, config.MaxSamples),
		alerts:          make([]MemoryAlert, 0),
		trackingObjects: make(map[string]*TrackedObject),
		stopCh:          make(chan struct{}),
	}
}

// Start begins memory monitoring
func (mm *MemoryMonitor) Start(ctx context.Context) error {
	if !atomic.CompareAndSwapInt32(&mm.active, 0, 1) {
		return fmt.Errorf("monitor already running")
	}

	mm.logger.Info("Starting memory monitor", map[string]interface{}{
		"sample_interval": mm.config.SampleInterval,
		"alert_threshold": mm.config.AlertThreshold,
	})

	mm.wg.Add(1)
	go mm.monitorLoop(ctx)

	return nil
}

// Stop stops memory monitoring
func (mm *MemoryMonitor) Stop() error {
	if !atomic.CompareAndSwapInt32(&mm.active, 1, 0) {
		return nil // Already stopped
	}

	mm.logger.Info("Stopping memory monitor", nil)
	close(mm.stopCh)
	mm.wg.Wait()

	return nil
}

// monitorLoop runs the monitoring loop
func (mm *MemoryMonitor) monitorLoop(ctx context.Context) {
	defer mm.wg.Done()

	ticker := time.NewTicker(mm.config.SampleInterval)
	defer ticker.Stop()

	// Take initial baseline
	mm.takeSample()

	for {
		select {
		case <-ctx.Done():
			return
		case <-mm.stopCh:
			return
		case <-ticker.C:
			mm.takeSample()
			mm.analyzeMemory()
		}
	}
}

// takeSample collects a memory sample
func (mm *MemoryMonitor) takeSample() {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	sample := MemorySample{
		Timestamp:     time.Now(),
		Alloc:         memStats.Alloc,
		TotalAlloc:    memStats.TotalAlloc,
		Sys:           memStats.Sys,
		NumGC:         memStats.NumGC,
		NumGoroutine:  runtime.NumGoroutine(),
		HeapAlloc:     memStats.HeapAlloc,
		HeapSys:       memStats.HeapSys,
		HeapIdle:      memStats.HeapIdle,
		HeapInuse:     memStats.HeapInuse,
		StackInuse:    memStats.StackInuse,
		MSpanInuse:    memStats.MSpanInuse,
		MCacheInuse:   memStats.MCacheInuse,
		GCCPUFraction: memStats.GCCPUFraction,
		PauseNs:       memStats.PauseNs,
		PauseTotalNs:  memStats.PauseTotalNs,
	}

	mm.mu.Lock()
	defer mm.mu.Unlock()

	// Set baseline if not set
	if !mm.baselineSet {
		mm.baselineSample = sample
		mm.baselineSet = true
	}

	mm.currentSample = sample

	// Add to sample history
	mm.samples = append(mm.samples, sample)
	if len(mm.samples) > mm.config.MaxSamples {
		mm.samples = mm.samples[1:]
	}
}

// analyzeMemory analyzes memory usage for potential issues
func (mm *MemoryMonitor) analyzeMemory() {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	if !mm.baselineSet || len(mm.samples) < 2 {
		return
	}

	baseline := mm.baselineSample
	current := mm.currentSample

	// Check for memory growth
	if baseline.Alloc > 0 {
		growthPct := (float64(current.Alloc) - float64(baseline.Alloc)) / float64(baseline.Alloc) * 100
		if growthPct > mm.config.AlertThreshold {
			mm.generateAlert(AlertTypeMemoryGrowth, fmt.Sprintf(
				"Memory usage increased by %.2f%% (from %d to %d bytes)",
				growthPct, baseline.Alloc, current.Alloc,
			), current.Alloc, baseline.Alloc, growthPct)
		}
	}

	// Check for goroutine leaks (>50% increase from baseline)
	goroutineGrowthPct := (float64(current.NumGoroutine) - float64(baseline.NumGoroutine)) / float64(baseline.NumGoroutine) * 100
	if goroutineGrowthPct > 50 {
		mm.generateAlert(AlertTypeGoroutineLeak, fmt.Sprintf(
			"Goroutine count increased by %.2f%% (from %d to %d)",
			goroutineGrowthPct, baseline.NumGoroutine, current.NumGoroutine,
		), uint64(current.NumGoroutine), uint64(baseline.NumGoroutine), goroutineGrowthPct)
	}

	// Check for GC pressure (GC using >5% of CPU)
	if mm.config.EnableGCStats && current.GCCPUFraction > 0.05 {
		mm.generateAlert(AlertTypeGCPressure, fmt.Sprintf(
			"GC using %.2f%% of CPU time (threshold 5%%)",
			current.GCCPUFraction*100,
		), uint64(current.GCCPUFraction*100), 5, current.GCCPUFraction*100)
	}

	// Check for heap fragmentation (idle heap > 50% of total heap)
	if current.HeapSys > 0 {
		idlePct := float64(current.HeapIdle) / float64(current.HeapSys) * 100
		if idlePct > 50 {
			mm.generateAlert(AlertTypeHeapFragmentation, fmt.Sprintf(
				"Heap fragmentation detected: %.2f%% idle (from %d total)",
				idlePct, current.HeapSys,
			), current.HeapIdle, current.HeapSys, idlePct)
		}
	}
}

// generateAlert generates a memory alert (must be called with lock held)
func (mm *MemoryMonitor) generateAlert(alertType AlertType, message string, current, baseline uint64, growthPct float64) {
	alert := MemoryAlert{
		Timestamp:   time.Now(),
		AlertType:   alertType,
		Message:     message,
		CurrentMem:  current,
		BaselineMem: baseline,
		GrowthPct:   growthPct,
	}

	mm.alerts = append(mm.alerts, alert)

	mm.logger.Warn("Memory alert", map[string]interface{}{
		"type":       alertType.String(),
		"message":    message,
		"current":    current,
		"baseline":   baseline,
		"growth_pct": growthPct,
	})
}

// GetStats returns current memory statistics
func (mm *MemoryMonitor) GetStats() MemoryStats {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	stats := MemoryStats{
		CurrentSample:  mm.currentSample,
		BaselineSample: mm.baselineSample,
		SampleCount:    len(mm.samples),
		AlertCount:     len(mm.alerts),
	}

	if mm.baselineSet && mm.baselineSample.Alloc > 0 {
		stats.GrowthSinceBaseline = (float64(mm.currentSample.Alloc) - float64(mm.baselineSample.Alloc)) / float64(mm.baselineSample.Alloc) * 100
	}

	return stats
}

// MemoryStats provides memory statistics
type MemoryStats struct {
	CurrentSample       MemorySample
	BaselineSample      MemorySample
	SampleCount         int
	AlertCount          int
	GrowthSinceBaseline float64
}

// GetAlerts returns all memory alerts
func (mm *MemoryMonitor) GetAlerts() []MemoryAlert {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	alerts := make([]MemoryAlert, len(mm.alerts))
	copy(alerts, mm.alerts)
	return alerts
}

// GetSamples returns memory sample history
func (mm *MemoryMonitor) GetSamples() []MemorySample {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	samples := make([]MemorySample, len(mm.samples))
	copy(samples, mm.samples)
	return samples
}

// TrackObject starts tracking an object type for leak detection
func (mm *MemoryMonitor) TrackObject(name string, alertThreshold int64) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	mm.trackingObjects[name] = &TrackedObject{
		Name:           name,
		Count:          0,
		Size:           0,
		AlertThreshold: alertThreshold,
	}
}

// IncrementObject increments the count of a tracked object
func (mm *MemoryMonitor) IncrementObject(name string, size int64) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	if obj, exists := mm.trackingObjects[name]; exists {
		obj.Count++
		obj.Size += size
		obj.LastIncrement = time.Now()

		// Check if threshold exceeded
		if obj.AlertThreshold > 0 && obj.Count > obj.AlertThreshold {
			mm.generateAlert(AlertTypeMemoryGrowth, fmt.Sprintf(
				"Tracked object %s exceeded threshold: %d objects (threshold: %d)",
				name, obj.Count, obj.AlertThreshold,
			), uint64(obj.Count), uint64(obj.AlertThreshold), float64(obj.Count-obj.AlertThreshold)/float64(obj.AlertThreshold)*100)
		}
	}
}

// DecrementObject decrements the count of a tracked object
func (mm *MemoryMonitor) DecrementObject(name string, size int64) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	if obj, exists := mm.trackingObjects[name]; exists {
		obj.Count--
		obj.Size -= size
		obj.LastDecrement = time.Now()
	}
}

// GetTrackedObjects returns all tracked objects
func (mm *MemoryMonitor) GetTrackedObjects() map[string]TrackedObject {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	objects := make(map[string]TrackedObject)
	for name, obj := range mm.trackingObjects {
		objects[name] = *obj
	}
	return objects
}

// ForceGC forces a garbage collection cycle
func (mm *MemoryMonitor) ForceGC() {
	mm.logger.Info("Forcing garbage collection", nil)
	runtime.GC()
	mm.takeSample()
}

// ResetBaseline resets the baseline to current memory usage
func (mm *MemoryMonitor) ResetBaseline() {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	mm.baselineSample = mm.currentSample
	mm.logger.Info("Baseline reset", map[string]interface{}{
		"alloc":         mm.baselineSample.Alloc,
		"num_goroutine": mm.baselineSample.NumGoroutine,
	})
}

// ClearAlerts clears all alerts
func (mm *MemoryMonitor) ClearAlerts() {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	mm.alerts = make([]MemoryAlert, 0)
}

// GetMemoryProfile returns a profile of current memory usage
func (mm *MemoryMonitor) GetMemoryProfile() MemoryProfile {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	profile := MemoryProfile{
		Timestamp:     time.Now(),
		Alloc:         memStats.Alloc,
		TotalAlloc:    memStats.TotalAlloc,
		Sys:           memStats.Sys,
		Lookups:       memStats.Lookups,
		Mallocs:       memStats.Mallocs,
		Frees:         memStats.Frees,
		HeapAlloc:     memStats.HeapAlloc,
		HeapSys:       memStats.HeapSys,
		HeapIdle:      memStats.HeapIdle,
		HeapInuse:     memStats.HeapInuse,
		HeapReleased:  memStats.HeapReleased,
		HeapObjects:   memStats.HeapObjects,
		StackInuse:    memStats.StackInuse,
		StackSys:      memStats.StackSys,
		MSpanInuse:    memStats.MSpanInuse,
		MSpanSys:      memStats.MSpanSys,
		MCacheInuse:   memStats.MCacheInuse,
		MCacheSys:     memStats.MCacheSys,
		GCSys:         memStats.GCSys,
		OtherSys:      memStats.OtherSys,
		NextGC:        memStats.NextGC,
		LastGC:        memStats.LastGC,
		NumGC:         memStats.NumGC,
		NumForcedGC:   memStats.NumForcedGC,
		GCCPUFraction: memStats.GCCPUFraction,
		NumGoroutine:  runtime.NumGoroutine(),
	}

	return profile
}

// MemoryProfile represents a detailed memory profile
type MemoryProfile struct {
	Timestamp     time.Time `json:"timestamp"`
	Alloc         uint64    `json:"alloc"`
	TotalAlloc    uint64    `json:"total_alloc"`
	Sys           uint64    `json:"sys"`
	Lookups       uint64    `json:"lookups"`
	Mallocs       uint64    `json:"mallocs"`
	Frees         uint64    `json:"frees"`
	HeapAlloc     uint64    `json:"heap_alloc"`
	HeapSys       uint64    `json:"heap_sys"`
	HeapIdle      uint64    `json:"heap_idle"`
	HeapInuse     uint64    `json:"heap_inuse"`
	HeapReleased  uint64    `json:"heap_released"`
	HeapObjects   uint64    `json:"heap_objects"`
	StackInuse    uint64    `json:"stack_inuse"`
	StackSys      uint64    `json:"stack_sys"`
	MSpanInuse    uint64    `json:"mspan_inuse"`
	MSpanSys      uint64    `json:"mspan_sys"`
	MCacheInuse   uint64    `json:"mcache_inuse"`
	MCacheSys     uint64    `json:"mcache_sys"`
	GCSys         uint64    `json:"gc_sys"`
	OtherSys      uint64    `json:"other_sys"`
	NextGC        uint64    `json:"next_gc"`
	LastGC        uint64    `json:"last_gc"`
	NumGC         uint32    `json:"num_gc"`
	NumForcedGC   uint32    `json:"num_forced_gc"`
	GCCPUFraction float64   `json:"gc_cpu_fraction"`
	NumGoroutine  int       `json:"num_goroutine"`
}
