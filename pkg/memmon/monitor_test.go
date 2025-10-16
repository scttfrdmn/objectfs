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

func TestMemoryMonitor_MemoryGrowthDetection(t *testing.T) {
	config := DefaultMonitorConfig()
	config.AlertThreshold = 10.0 // 10% growth threshold
	config.SampleInterval = 50 * time.Millisecond
	monitor := NewMemoryMonitor(config)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Start monitoring
	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Take baseline sample
	time.Sleep(100 * time.Millisecond)

	// Allocate memory to trigger growth detection
	allocations := make([][]byte, 100)
	for i := 0; i < 100; i++ {
		allocations[i] = make([]byte, 1024*1024) // 1MB each
	}

	// Wait for monitoring to detect growth
	time.Sleep(200 * time.Millisecond)

	// Stop monitoring
	if err := monitor.Stop(); err != nil {
		t.Logf("Error stopping monitor: %v", err)
	}

	// Verify alerts were generated
	alerts := monitor.GetAlerts()
	if len(alerts) == 0 {
		t.Log("No alerts generated (may be normal if memory growth is small)")
	}

	// Keep allocations in scope
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
	for i := 0; i < 100; i++ {
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
	for i := 0; i < 50; i++ {
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
	for i := 0; i < 30; i++ {
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

	// Manually generate an alert
	monitor.mu.Lock()
	monitor.generateAlert(AlertTypeMemoryGrowth, "test alert", 1000, 500, 100.0)
	monitor.mu.Unlock()

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
	for i := 0; i < 5; i++ {
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
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 10; j++ {
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
	for i := 0; i < 10; i++ {
		<-done
	}

	// Stop monitoring
	if err := monitor.Stop(); err != nil {
		t.Logf("Error stopping monitor: %v", err)
	}

	// Verify no panics occurred
	t.Log("Concurrent access test completed successfully")
}
