package memmon

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestNewMemoryMonitor(t *testing.T) {
	config := DefaultMonitorConfig()
	monitor := NewMemoryMonitor(config)

	if monitor == nil {
		t.Fatal("Expected non-nil monitor")
	}

	if monitor.config.SampleInterval != config.SampleInterval {
		t.Errorf("Expected sample interval %v, got %v", config.SampleInterval, monitor.config.SampleInterval)
	}
}

func TestMemoryMonitor_StartStop(t *testing.T) {
	config := DefaultMonitorConfig()
	config.SampleInterval = 100 * time.Millisecond
	monitor := NewMemoryMonitor(config)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start monitoring
	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Verify it's running
	time.Sleep(300 * time.Millisecond)

	stats := monitor.GetStats()
	if stats.SampleCount < 2 {
		t.Errorf("Expected at least 2 samples, got %d", stats.SampleCount)
	}

	// Stop monitoring
	if err := monitor.Stop(); err != nil {
		t.Fatalf("Failed to stop monitor: %v", err)
	}
}

func TestMemoryMonitor_TakeSample(t *testing.T) {
	config := DefaultMonitorConfig()
	monitor := NewMemoryMonitor(config)

	monitor.takeSample()

	stats := monitor.GetStats()
	if stats.CurrentSample.Timestamp.IsZero() {
		t.Error("Expected non-zero timestamp")
	}

	if stats.CurrentSample.Alloc == 0 {
		t.Error("Expected non-zero allocation")
	}
}

// TestMemoryMonitor_MemoryGrowthDetection drives analyzeMemory from seeded samples rather than from
// real allocations.
//
// The version this replaces started the monitor, allocated 100 MB, slept 200 ms, and then accepted
// either outcome:
//
//	alerts := monitor.GetAlerts()
//	if len(alerts) == 0 {
//	    t.Log("No alerts generated (may be normal if memory growth is small)")
//	}
//
// So it asserted nothing. Deleting the entire growth-detection branch left it green, which makes it a
// test of the allocator's mood rather than of the code — and its own coverage flickered with GC
// timing: `analyzeMemory` measured 87.0% or 82.6% depending on whether `Alloc` happened to cross the
// threshold between two samples, in roughly one run in four. That is what put pkg/memmon below its
// coverage floor in CI on a commit that changed only YAML.
//
// Seeding baselineSample/currentSample makes the growth arithmetic the only variable, so the
// threshold comparison is exercised on every run and the assertions can be exact.
func TestMemoryMonitor_MemoryGrowthDetection(t *testing.T) {
	t.Parallel()

	const (
		baselineAlloc = 100 * 1024 * 1024
		growthPct     = 50.0
	)

	tests := []struct {
		name         string
		threshold    float64
		currentAlloc uint64
		wantAlert    bool
	}{
		{
			name:         "growth over threshold alerts",
			threshold:    10.0,
			currentAlloc: baselineAlloc * 3 / 2, // +50%, well past 10%
			wantAlert:    true,
		},
		{
			name:         "growth under threshold is silent",
			threshold:    75.0, // +50% does not reach it
			currentAlloc: baselineAlloc * 3 / 2,
			wantAlert:    false,
		},
		{
			name: "growth exactly at threshold is silent",
			// The comparison is `>`, not `>=`. Pinned because which one it is decides whether a
			// monitor configured at its steady-state growth rate alerts continuously or never.
			threshold:    growthPct,
			currentAlloc: baselineAlloc * 3 / 2,
			wantAlert:    false,
		},
		{
			name:         "shrinking memory does not alert",
			threshold:    10.0,
			currentAlloc: baselineAlloc / 2,
			wantAlert:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			config := DefaultMonitorConfig()
			config.AlertThreshold = tt.threshold
			monitor := NewMemoryMonitor(config)

			// analyzeMemory reads baselineSample and currentSample, and returns early unless the
			// baseline is set and at least two samples exist.
			baseline := MemorySample{Alloc: baselineAlloc, NumGoroutine: 10}
			current := MemorySample{Alloc: tt.currentAlloc, NumGoroutine: 10}

			monitor.mu.Lock()
			monitor.baselineSet = true
			monitor.baselineSample = baseline
			monitor.currentSample = current
			monitor.samples = []MemorySample{baseline, current}
			monitor.mu.Unlock()

			monitor.analyzeMemory()

			var growth []MemoryAlert
			for _, a := range monitor.GetAlerts() {
				if a.AlertType == AlertTypeMemoryGrowth {
					growth = append(growth, a)
				}
			}

			if !tt.wantAlert {
				if len(growth) != 0 {
					t.Fatalf("got %d memory-growth alerts, want none: %+v", len(growth), growth)
				}

				return
			}

			if len(growth) != 1 {
				t.Fatalf("got %d memory-growth alerts, want exactly 1: %+v", len(growth), growth)
			}

			// The alert has to carry the numbers it was raised from, or it tells an operator that
			// something grew without saying from what to what.
			got := growth[0]
			if got.BaselineMem != baselineAlloc {
				t.Errorf("BaselineMem = %d, want %d", got.BaselineMem, uint64(baselineAlloc))
			}
			if got.CurrentMem != tt.currentAlloc {
				t.Errorf("CurrentMem = %d, want %d", got.CurrentMem, tt.currentAlloc)
			}
			if got.GrowthPct != growthPct {
				t.Errorf("GrowthPct = %v, want %v", got.GrowthPct, growthPct)
			}
			if got.Message == "" {
				t.Error("Message is empty")
			}
		})
	}
}

// TestMemoryMonitor_AnalyzeMemoryNeedsTwoSamples pins the early return. A monitor that has taken one
// sample has no baseline to compare against, and comparing a sample to itself would report 0% growth
// as a fact rather than as an absence of data.
func TestMemoryMonitor_AnalyzeMemoryNeedsTwoSamples(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		baselineSet bool
		samples     int
	}{
		{name: "no baseline", baselineSet: false, samples: 2},
		{name: "one sample", baselineSet: true, samples: 1},
		{name: "no samples", baselineSet: true, samples: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			config := DefaultMonitorConfig()
			config.AlertThreshold = 1.0 // Would alert readily if it got that far.
			monitor := NewMemoryMonitor(config)

			sample := MemorySample{Alloc: 1024, NumGoroutine: 10}

			monitor.mu.Lock()
			monitor.baselineSet = tt.baselineSet
			monitor.baselineSample = sample
			monitor.currentSample = MemorySample{Alloc: 1024 * 1024, NumGoroutine: 10_000}
			monitor.samples = make([]MemorySample, tt.samples)
			monitor.mu.Unlock()

			monitor.analyzeMemory()

			if alerts := monitor.GetAlerts(); len(alerts) != 0 {
				t.Fatalf("got %d alerts from an incomplete sample set, want none: %+v", len(alerts), alerts)
			}
		})
	}
}

// TestMemoryMonitor_LiveSampling exercises the real Start/sample/Stop loop, which the seeded tests
// above deliberately bypass. It asserts on what is actually deterministic there — that sampling
// happens and a baseline is established — and not on whether any alert fired, which depends on the
// allocator.
func TestMemoryMonitor_LiveSampling(t *testing.T) {
	t.Parallel()

	config := DefaultMonitorConfig()
	config.SampleInterval = 10 * time.Millisecond
	monitor := NewMemoryMonitor(config)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Allocate while sampling runs, so the loop has something to observe.
	allocations := make([][]byte, 64)
	for i := range allocations {
		allocations[i] = make([]byte, 1024*1024)
	}

	deadline := time.Now().Add(time.Second)
	for {
		monitor.mu.RLock()
		n := len(monitor.samples)
		set := monitor.baselineSet
		monitor.mu.RUnlock()

		if n >= 2 && set {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("monitor took %d samples in 1s at a 10ms interval, baselineSet=%v; want at least 2", n, set)
		}

		time.Sleep(5 * time.Millisecond)
	}

	if err := monitor.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	_ = allocations
}

func TestMemoryMonitor_GoroutineLeakDetection(t *testing.T) {
	config := DefaultMonitorConfig()
	config.SampleInterval = 50 * time.Millisecond
	monitor := NewMemoryMonitor(config)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Start monitoring
	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Take baseline
	time.Sleep(100 * time.Millisecond)

	// Create goroutines to simulate a leak
	stopCh := make(chan struct{})
	for range 100 {
		go func() {
			<-stopCh
		}()
	}

	// Wait for monitoring
	time.Sleep(200 * time.Millisecond)

	// Stop goroutines
	close(stopCh)

	// Stop monitoring
	if err := monitor.Stop(); err != nil {
		t.Logf("Error stopping monitor: %v", err)
	}

	// Verify goroutine count increased
	stats := monitor.GetStats()
	if stats.CurrentSample.NumGoroutine > stats.BaselineSample.NumGoroutine {
		t.Logf("Goroutine count increased from %d to %d",
			stats.BaselineSample.NumGoroutine,
			stats.CurrentSample.NumGoroutine)
	}
}

func TestMemoryMonitor_TrackObject(t *testing.T) {
	config := DefaultMonitorConfig()
	monitor := NewMemoryMonitor(config)

	// Track an object type
	monitor.TrackObject("test-object", 100)

	// Increment objects
	for range 50 {
		monitor.IncrementObject("test-object", 1024)
	}

	// Get tracked objects
	objects := monitor.GetTrackedObjects()
	obj, exists := objects["test-object"]
	if !exists {
		t.Fatal("Expected test-object to be tracked")
	}

	if obj.Count != 50 {
		t.Errorf("Expected 50 objects, got %d", obj.Count)
	}

	expectedSize := int64(50 * 1024)
	if obj.Size != expectedSize {
		t.Errorf("Expected size %d, got %d", expectedSize, obj.Size)
	}

	// Decrement objects
	for range 30 {
		monitor.DecrementObject("test-object", 1024)
	}

	objects = monitor.GetTrackedObjects()
	obj = objects["test-object"]
	if obj.Count != 20 {
		t.Errorf("Expected 20 objects after decrement, got %d", obj.Count)
	}
}

func TestMemoryMonitor_ForceGC(t *testing.T) {
	config := DefaultMonitorConfig()
	monitor := NewMemoryMonitor(config)

	// Take initial sample
	monitor.takeSample()
	initialGC := monitor.GetStats().CurrentSample.NumGC

	// Force GC
	monitor.ForceGC()

	// Verify GC was run
	currentGC := monitor.GetStats().CurrentSample.NumGC
	if currentGC <= initialGC {
		t.Logf("GC count did not increase (initial: %d, current: %d)", initialGC, currentGC)
	}
}

func TestMemoryMonitor_ResetBaseline(t *testing.T) {
	config := DefaultMonitorConfig()
	monitor := NewMemoryMonitor(config)

	// Take initial sample
	monitor.takeSample()
	initialBaseline := monitor.GetStats().BaselineSample.Alloc

	// Allocate some memory
	allocation := make([]byte, 10*1024*1024) // 10MB
	_ = allocation

	// Take another sample
	monitor.takeSample()

	// Reset baseline
	monitor.ResetBaseline()

	// Verify baseline was reset
	newBaseline := monitor.GetStats().BaselineSample.Alloc
	if newBaseline == initialBaseline {
		t.Log("Baseline may not have changed significantly")
	}
}

func TestMemoryMonitor_GetMemoryProfile(t *testing.T) {
	config := DefaultMonitorConfig()
	monitor := NewMemoryMonitor(config)

	profile := monitor.GetMemoryProfile()

	if profile.Timestamp.IsZero() {
		t.Error("Expected non-zero timestamp")
	}

	if profile.Alloc == 0 {
		t.Error("Expected non-zero allocation")
	}

	if profile.NumGoroutine <= 0 {
		t.Error("Expected positive goroutine count")
	}
}

func TestMemorySample_Fields(t *testing.T) {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	sample := MemorySample{
		Timestamp:    time.Now(),
		Alloc:        memStats.Alloc,
		TotalAlloc:   memStats.TotalAlloc,
		Sys:          memStats.Sys,
		NumGC:        memStats.NumGC,
		NumGoroutine: runtime.NumGoroutine(),
	}

	if sample.Timestamp.IsZero() {
		t.Error("Expected non-zero timestamp")
	}

	if sample.Alloc == 0 {
		t.Error("Expected non-zero alloc")
	}

	if sample.NumGoroutine <= 0 {
		t.Error("Expected positive goroutine count")
	}
}

func TestAlertType_String(t *testing.T) {
	tests := []struct {
		alertType AlertType
		expected  string
	}{
		{AlertTypeMemoryGrowth, "memory_growth"},
		{AlertTypeGoroutineLeak, "goroutine_leak"},
		{AlertTypeGCPressure, "gc_pressure"},
		{AlertTypeHeapFragmentation, "heap_fragmentation"},
	}

	for _, tt := range tests {
		if got := tt.alertType.String(); got != tt.expected {
			t.Errorf("Expected %s, got %s", tt.expected, got)
		}
	}
}

func TestMemoryMonitor_ClearAlerts(t *testing.T) {
	config := DefaultMonitorConfig()
	monitor := NewMemoryMonitor(config)

	// Manually generate an alert (without holding lock, as generateAlert acquires it internally)
	monitor.generateAlert(AlertTypeMemoryGrowth, "test alert", 1000, 500, 100.0)

	// Verify alert exists
	alerts := monitor.GetAlerts()
	if len(alerts) != 1 {
		t.Fatalf("Expected 1 alert, got %d", len(alerts))
	}

	// Clear alerts
	monitor.ClearAlerts()

	// Verify alerts cleared
	alerts = monitor.GetAlerts()
	if len(alerts) != 0 {
		t.Errorf("Expected 0 alerts after clear, got %d", len(alerts))
	}
}

func TestMemoryMonitor_GetSamples(t *testing.T) {
	config := DefaultMonitorConfig()
	monitor := NewMemoryMonitor(config)

	// Take multiple samples
	for range 5 {
		monitor.takeSample()
		time.Sleep(10 * time.Millisecond)
	}

	samples := monitor.GetSamples()
	if len(samples) != 5 {
		t.Errorf("Expected 5 samples, got %d", len(samples))
	}
}

func TestMemoryMonitor_ConcurrentAccess(t *testing.T) {
	config := DefaultMonitorConfig()
	config.SampleInterval = 10 * time.Millisecond
	monitor := NewMemoryMonitor(config)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Start monitoring
	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Concurrent access to monitor
	done := make(chan bool, 10)
	for range 10 {
		go func() {
			for range 10 {
				monitor.GetStats()
				monitor.GetAlerts()
				monitor.GetSamples()
				monitor.IncrementObject("test", 100)
				monitor.DecrementObject("test", 100)
				time.Sleep(5 * time.Millisecond)
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for range 10 {
		<-done
	}

	// Stop monitoring
	if err := monitor.Stop(); err != nil {
		t.Logf("Error stopping monitor: %v", err)
	}

	// Verify no panics occurred
	t.Log("Concurrent access test completed successfully")
}
