package profiling

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Alert type constants for testing
const (
	AlertTypeHeapSize      = "heap_size"
	AlertTypeHeapGrowth    = "heap_growth"
	AlertTypeGoroutineLeak = "goroutine_count"
)

func TestNewMemoryMonitor(t *testing.T) {
	config := DefaultMonitorConfig()
	config.Port = 16060
	thresholds := DefaultAlertThresholds()

	monitor := NewMemoryMonitor(config, thresholds)
	if monitor == nil {
		t.Fatal("Expected non-nil monitor")
	}

	if monitor.config.Port != config.Port {
		t.Errorf("Expected port %d, got %d", config.Port, monitor.config.Port)
	}

	if monitor.maxSamples != config.MaxSamples {
		t.Errorf("Expected maxSamples %d, got %d", config.MaxSamples, monitor.maxSamples)
	}
}

func TestMemoryMonitor_StartStop(t *testing.T) {
	config := DefaultMonitorConfig()
	config.Port = 16061
	config.SampleInterval = 100 * time.Millisecond

	thresholds := DefaultAlertThresholds()
	monitor := NewMemoryMonitor(config, thresholds)

	// Start monitoring
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Wait for a few samples
	time.Sleep(350 * time.Millisecond)

	// Stop monitoring
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := monitor.Stop(stopCtx); err != nil {
		t.Fatalf("Failed to stop monitor: %v", err)
	}

	// Check that we collected samples
	stats := monitor.GetMemoryStats()
	if stats.SampleCount == 0 {
		t.Error("Expected samples to be collected")
	}

	if stats.SampleCount < 2 {
		t.Errorf("Expected at least 2 samples, got %d", stats.SampleCount)
	}
}

func TestMemoryMonitor_Sampling(t *testing.T) {
	config := DefaultMonitorConfig()
	config.Port = 16062
	config.SampleInterval = 50 * time.Millisecond
	config.MaxSamples = 5

	thresholds := DefaultAlertThresholds()
	monitor := NewMemoryMonitor(config, thresholds)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Wait for samples
	<-ctx.Done()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := monitor.Stop(stopCtx); err != nil {
		t.Fatalf("Failed to stop monitor: %v", err)
	}

	samples := monitor.GetSamples()
	if len(samples) == 0 {
		t.Fatal("Expected samples to be collected")
	}

	// Verify sample structure
	for i, sample := range samples {
		if sample.Timestamp.IsZero() {
			t.Errorf("Sample %d has zero timestamp", i)
		}
		if sample.NumGoroutine == 0 {
			t.Errorf("Sample %d has zero goroutines", i)
		}
	}

	// Verify max samples limit
	if len(samples) > config.MaxSamples {
		t.Errorf("Expected max %d samples, got %d", config.MaxSamples, len(samples))
	}
}

func TestMemoryMonitor_PeakTracking(t *testing.T) {
	config := DefaultMonitorConfig()
	config.Port = 16063
	config.SampleInterval = 50 * time.Millisecond

	thresholds := DefaultAlertThresholds()
	monitor := NewMemoryMonitor(config, thresholds)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Allocate some memory to increase heap
	allocations := make([][]byte, 0, 100)
	for i := 0; i < 100; i++ {
		data := make([]byte, 1024*1024) // 1MB
		allocations = append(allocations, data)
		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(200 * time.Millisecond)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := monitor.Stop(stopCtx); err != nil {
		t.Fatalf("Failed to stop monitor: %v", err)
	}

	stats := monitor.GetMemoryStats()
	if stats.PeakHeap == 0 {
		t.Error("Expected peak heap to be tracked")
	}

	currentHeapMB := stats.Current.HeapAlloc / (1024 * 1024)
	peakHeapMB := stats.PeakHeap / (1024 * 1024)

	if peakHeapMB < currentHeapMB {
		t.Errorf("Peak heap (%d MB) should be >= current heap (%d MB)",
			peakHeapMB, currentHeapMB)
	}

	// Keep reference to avoid GC
	_ = allocations
}

func TestMemoryMonitor_AlertThresholds(t *testing.T) {
	config := DefaultMonitorConfig()
	config.Port = 16064
	config.SampleInterval = 50 * time.Millisecond

	// Set very low thresholds to trigger alerts
	thresholds := AlertThresholds{
		HeapSizeMB:        1, // 1 MB - will definitely trigger
		GoroutineCount:    5, // Very low - will trigger
		HeapGrowthPercent: 10.0,
	}
	monitor := NewMemoryMonitor(config, thresholds)

	alertTriggered := false
	var alertMu sync.Mutex

	monitor.AddAlertCallback(func(alert Alert) {
		alertMu.Lock()
		defer alertMu.Unlock()
		alertTriggered = true
		t.Logf("Alert triggered: %s - %s", alert.Type, alert.Message)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Allocate some memory to ensure thresholds are exceeded
	allocations := make([][]byte, 0, 50)
	for i := 0; i < 50; i++ {
		data := make([]byte, 100*1024) // 100KB
		allocations = append(allocations, data)
	}

	<-ctx.Done()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := monitor.Stop(stopCtx); err != nil {
		t.Fatalf("Failed to stop monitor: %v", err)
	}

	alertMu.Lock()
	triggered := alertTriggered
	alertMu.Unlock()

	if !triggered {
		t.Error("Expected at least one alert to be triggered")
	}

	// Keep reference to avoid GC
	_ = allocations
}

func TestMemoryMonitor_HTTPEndpoints(t *testing.T) {
	config := DefaultMonitorConfig()
	config.Port = 16065
	config.SampleInterval = 100 * time.Millisecond

	thresholds := DefaultAlertThresholds()
	monitor := NewMemoryMonitor(config, thresholds)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		if err := monitor.Stop(stopCtx); err != nil {
			t.Logf("Failed to stop monitor: %v", err)
		}
	}()

	// Wait for server to start
	time.Sleep(200 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", config.Port)

	tests := []struct {
		name     string
		endpoint string
		wantCode int
	}{
		{"pprof index", "/debug/pprof/", http.StatusOK},
		{"heap profile", "/debug/pprof/heap", http.StatusOK},
		{"goroutine profile", "/debug/pprof/goroutine", http.StatusOK},
		{"memory stats", "/memory/stats", http.StatusOK},
		{"memory samples", "/memory/samples", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(baseURL + tt.endpoint)
			if err != nil {
				t.Fatalf("Failed to fetch %s: %v", tt.endpoint, err)
			}
			defer func() {
				if err := resp.Body.Close(); err != nil {
					t.Logf("Failed to close response body: %v", err)
				}
			}()

			if resp.StatusCode != tt.wantCode {
				t.Errorf("Expected status %d for %s, got %d",
					tt.wantCode, tt.endpoint, resp.StatusCode)
			}
		})
	}
}

func TestMemoryMonitor_ForceGC(t *testing.T) {
	config := DefaultMonitorConfig()
	config.Port = 16066

	thresholds := DefaultAlertThresholds()
	monitor := NewMemoryMonitor(config, thresholds)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		if err := monitor.Stop(stopCtx); err != nil {
			t.Logf("Failed to stop monitor: %v", err)
		}
	}()

	time.Sleep(100 * time.Millisecond)

	// Get initial stats
	statsBefore := monitor.GetMemoryStats()

	// Force GC via HTTP endpoint
	baseURL := fmt.Sprintf("http://localhost:%d", config.Port)
	resp, err := http.Post(baseURL+"/memory/gc", "", nil)
	if err != nil {
		t.Fatalf("Failed to force GC: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("Failed to close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	time.Sleep(100 * time.Millisecond)

	// Get stats after GC
	statsAfter := monitor.GetMemoryStats()

	// Verify that GC actually ran
	if statsAfter.Current.NumGC <= statsBefore.Current.NumGC {
		t.Error("Expected GC count to increase after forced GC")
	}
}

// Long-running test to detect memory leaks
func TestMemoryMonitor_LeakDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping long-running leak detection test")
	}

	config := DefaultMonitorConfig()
	config.Port = 16067
	config.SampleInterval = 100 * time.Millisecond
	config.MaxSamples = 100

	// Set alert for significant growth
	thresholds := AlertThresholds{
		HeapGrowthPercent: 200.0, // Alert if heap grows by 200%
	}
	monitor := NewMemoryMonitor(config, thresholds)

	leakDetected := false
	var leakMu sync.Mutex

	monitor.AddAlertCallback(func(alert Alert) {
		if alert.Type == AlertTypeHeapGrowth {
			leakMu.Lock()
			leakDetected = true
			leakMu.Unlock()
			t.Logf("Potential leak detected: %s", alert.Message)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Simulate workload with proper cleanup (should NOT leak)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopCancel()
			if err := monitor.Stop(stopCtx); err != nil {
				t.Errorf("Failed to stop monitor: %v", err)
			}

			stats := monitor.GetMemoryStats()
			currentHeapMB := stats.Current.HeapAlloc / (1024 * 1024)
			peakHeapMB := stats.PeakHeap / (1024 * 1024)
			t.Logf("Final stats: Current heap: %d MB, Peak: %d MB, Growth: %.2f%%",
				currentHeapMB, peakHeapMB, stats.HeapGrowth)

			// With proper cleanup, we should NOT detect a leak
			leakMu.Lock()
			detected := leakDetected
			leakMu.Unlock()

			if detected {
				t.Log("Warning: Heap growth alert triggered - may indicate leak")
			}
			return

		case <-ticker.C:
			// Simulate work with temporary allocations
			data := make([]byte, 100*1024) // 100KB
			_ = data
			// Data goes out of scope and can be GC'd
		}
	}
}

// Test that simulates a memory leak
func TestMemoryMonitor_ActualLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping memory leak simulation test")
	}

	config := DefaultMonitorConfig()
	config.Port = 16068
	config.SampleInterval = 100 * time.Millisecond
	config.MaxSamples = 50

	// Set sensitive thresholds
	thresholds := AlertThresholds{
		HeapGrowthPercent: 50.0, // Alert on 50% growth
	}
	monitor := NewMemoryMonitor(config, thresholds)

	leakDetected := false
	var leakMu sync.Mutex

	monitor.AddAlertCallback(func(alert Alert) {
		if alert.Type == AlertTypeHeapGrowth {
			leakMu.Lock()
			leakDetected = true
			leakMu.Unlock()
			t.Logf("Leak detected: %s", alert.Message)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Simulate a memory leak by holding references
	leakedData := make([][]byte, 0, 1000)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopCancel()
			if err := monitor.Stop(stopCtx); err != nil {
				t.Errorf("Failed to stop monitor: %v", err)
			}

			stats := monitor.GetMemoryStats()
			currentHeapMB := stats.Current.HeapAlloc / (1024 * 1024)
			peakHeapMB := stats.PeakHeap / (1024 * 1024)
			t.Logf("Leak simulation stats: Current heap: %d MB, Peak: %d MB, Growth: %.2f%%",
				currentHeapMB, peakHeapMB, stats.HeapGrowth)

			leakMu.Lock()
			detected := leakDetected
			leakMu.Unlock()

			if !detected {
				t.Error("Expected leak to be detected but it wasn't")
			}

			// Keep reference to prevent compiler optimization
			_ = leakedData
			return

		case <-ticker.C:
			// Intentionally leak memory by keeping references
			data := make([]byte, 500*1024) // 500KB
			leakedData = append(leakedData, data)
		}
	}
}

// Stress test with concurrent operations
func TestMemoryMonitor_ConcurrentStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test")
	}

	config := DefaultMonitorConfig()
	config.Port = 16069
	config.SampleInterval = 50 * time.Millisecond
	config.MaxSamples = 200

	thresholds := DefaultAlertThresholds()
	monitor := NewMemoryMonitor(config, thresholds)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Run multiple concurrent goroutines doing work
	var wg sync.WaitGroup
	numWorkers := 10

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// Simulate work
					data := make([]byte, 50*1024) // 50KB
					for j := range data {
						data[j] = byte(id)
					}
					// Data goes out of scope
				}
			}
		}(i)
	}

	// Periodically check stats
	statsTicker := time.NewTicker(500 * time.Millisecond)
	defer statsTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopCancel()
			if err := monitor.Stop(stopCtx); err != nil {
				t.Errorf("Failed to stop monitor: %v", err)
			}

			stats := monitor.GetMemoryStats()
			t.Logf("Stress test final stats:")
			t.Logf("  Samples collected: %d", stats.SampleCount)
			t.Logf("  Current heap: %d MB", stats.Current.HeapAlloc/(1024*1024))
			t.Logf("  Peak heap: %d MB", stats.PeakHeap/(1024*1024))
			t.Logf("  Goroutines: %d", stats.Current.NumGoroutine)
			t.Logf("  Total GC cycles: %d", stats.Current.NumGC)

			if stats.SampleCount == 0 {
				t.Error("No samples collected during stress test")
			}

			return

		case <-statsTicker.C:
			stats := monitor.GetMemoryStats()
			currentHeapMB := stats.Current.HeapAlloc / (1024 * 1024)
			t.Logf("Interim: heap=%dMB goroutines=%d samples=%d",
				currentHeapMB, stats.Current.NumGoroutine, stats.SampleCount)
		}
	}
}

func TestMemoryMonitor_GoroutineGrowth(t *testing.T) {
	config := DefaultMonitorConfig()
	config.Port = 16070
	config.SampleInterval = 50 * time.Millisecond

	// Set goroutine threshold
	initialGoroutines := runtime.NumGoroutine()
	thresholds := AlertThresholds{
		GoroutineCount: initialGoroutines + 50, // Alert if 50+ more goroutines
	}
	monitor := NewMemoryMonitor(config, thresholds)

	alertTriggered := false
	var alertMu sync.Mutex

	monitor.AddAlertCallback(func(alert Alert) {
		if alert.Type == AlertTypeGoroutineLeak {
			alertMu.Lock()
			alertTriggered = true
			alertMu.Unlock()
			t.Logf("Goroutine growth alert: %s", alert.Message)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("Failed to start monitor: %v", err)
	}

	// Spawn many goroutines
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(1500 * time.Millisecond)
		}()
	}

	<-ctx.Done()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := monitor.Stop(stopCtx); err != nil {
		t.Errorf("Failed to stop monitor: %v", err)
	}
	wg.Wait()

	alertMu.Lock()
	triggered := alertTriggered
	alertMu.Unlock()

	if !triggered {
		t.Error("Expected goroutine growth alert to trigger")
	}
}

// Benchmark memory monitoring overhead
func BenchmarkMemoryMonitor_Sampling(b *testing.B) {
	config := DefaultMonitorConfig()
	config.Port = 16071
	config.SampleInterval = 10 * time.Millisecond

	thresholds := DefaultAlertThresholds()
	monitor := NewMemoryMonitor(config, thresholds)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		b.Fatalf("Failed to start monitor: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		if err := monitor.Stop(stopCtx); err != nil {
			b.Logf("Failed to stop monitor: %v", err)
		}
	}()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Simulate work
		data := make([]byte, 1024)
		_ = data
	}
}

func BenchmarkMemoryMonitor_GetStats(b *testing.B) {
	config := DefaultMonitorConfig()
	config.Port = 16072

	thresholds := DefaultAlertThresholds()
	monitor := NewMemoryMonitor(config, thresholds)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := monitor.Start(ctx); err != nil {
		b.Fatalf("Failed to start monitor: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		if err := monitor.Stop(stopCtx); err != nil {
			b.Logf("Failed to stop monitor: %v", err)
		}
	}()

	time.Sleep(100 * time.Millisecond)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = monitor.GetMemoryStats()
	}
}
