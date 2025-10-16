package memmon

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"time"
)

// Profiler provides memory profiling utilities
type Profiler struct {
	outputDir string
}

// NewProfiler creates a new memory profiler
func NewProfiler(outputDir string) *Profiler {
	// Create output directory if it doesn't exist
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Printf("Error creating profile directory: %v\n", err)
	}

	return &Profiler{
		outputDir: outputDir,
	}
}

// WriteHeapProfile writes a heap profile to disk
func (p *Profiler) WriteHeapProfile(filename string) error {
	if filename == "" {
		filename = fmt.Sprintf("heap_%d.prof", time.Now().Unix())
	}

	filepath := fmt.Sprintf("%s/%s", p.outputDir, filename)
	f, err := os.Create(filepath)
	if err != nil {
		return fmt.Errorf("create heap profile: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			fmt.Printf("Error closing file: %v\n", cerr)
		}
	}()

	// Force GC to get accurate profile
	runtime.GC()

	if err := pprof.WriteHeapProfile(f); err != nil {
		return fmt.Errorf("write heap profile: %w", err)
	}

	fmt.Printf("Heap profile written to %s\n", filepath)
	return nil
}

// WriteGoroutineProfile writes a goroutine profile to disk
func (p *Profiler) WriteGoroutineProfile(filename string) error {
	if filename == "" {
		filename = fmt.Sprintf("goroutine_%d.prof", time.Now().Unix())
	}

	filepath := fmt.Sprintf("%s/%s", p.outputDir, filename)
	f, err := os.Create(filepath)
	if err != nil {
		return fmt.Errorf("create goroutine profile: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			fmt.Printf("Error closing file: %v\n", cerr)
		}
	}()

	profile := pprof.Lookup("goroutine")
	if profile == nil {
		return fmt.Errorf("goroutine profile not found")
	}

	if err := profile.WriteTo(f, 2); err != nil {
		return fmt.Errorf("write goroutine profile: %w", err)
	}

	fmt.Printf("Goroutine profile written to %s\n", filepath)
	return nil
}

// WriteAllProfiles writes all available profiles to disk
func (p *Profiler) WriteAllProfiles(prefix string) error {
	timestamp := time.Now().Unix()

	// Heap profile
	if err := p.WriteHeapProfile(fmt.Sprintf("%s_heap_%d.prof", prefix, timestamp)); err != nil {
		return err
	}

	// Goroutine profile
	if err := p.WriteGoroutineProfile(fmt.Sprintf("%s_goroutine_%d.prof", prefix, timestamp)); err != nil {
		return err
	}

	// Block profile
	if err := p.writeBlockProfile(fmt.Sprintf("%s_block_%d.prof", prefix, timestamp)); err != nil {
		return err
	}

	// Mutex profile
	if err := p.writeMutexProfile(fmt.Sprintf("%s_mutex_%d.prof", prefix, timestamp)); err != nil {
		return err
	}

	return nil
}

// writeBlockProfile writes a block profile to disk
func (p *Profiler) writeBlockProfile(filename string) error {
	filepath := fmt.Sprintf("%s/%s", p.outputDir, filename)
	f, err := os.Create(filepath)
	if err != nil {
		return fmt.Errorf("create block profile: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			fmt.Printf("Error closing file: %v\n", cerr)
		}
	}()

	profile := pprof.Lookup("block")
	if profile == nil {
		return fmt.Errorf("block profile not found")
	}

	if err := profile.WriteTo(f, 0); err != nil {
		return fmt.Errorf("write block profile: %w", err)
	}

	fmt.Printf("Block profile written to %s\n", filepath)
	return nil
}

// writeMutexProfile writes a mutex profile to disk
func (p *Profiler) writeMutexProfile(filename string) error {
	filepath := fmt.Sprintf("%s/%s", p.outputDir, filename)
	f, err := os.Create(filepath)
	if err != nil {
		return fmt.Errorf("create mutex profile: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			fmt.Printf("Error closing file: %v\n", cerr)
		}
	}()

	profile := pprof.Lookup("mutex")
	if profile == nil {
		return fmt.Errorf("mutex profile not found")
	}

	if err := profile.WriteTo(f, 0); err != nil {
		return fmt.Errorf("write mutex profile: %w", err)
	}

	fmt.Printf("Mutex profile written to %s\n", filepath)
	return nil
}

// ProfileMemoryUsage profiles memory usage over a duration
func (p *Profiler) ProfileMemoryUsage(duration time.Duration, interval time.Duration) ([]MemorySample, error) {
	samples := make([]MemorySample, 0)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	deadline := time.Now().Add(duration)

	for time.Now().Before(deadline) {
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
			GCCPUFraction: memStats.GCCPUFraction,
		}

		samples = append(samples, sample)

		<-ticker.C
	}

	return samples, nil
}

// DetectLeaks analyzes memory samples to detect potential leaks
func (p *Profiler) DetectLeaks(samples []MemorySample, threshold float64) []LeakDetection {
	if len(samples) < 2 {
		return nil
	}

	detections := make([]LeakDetection, 0)

	baseline := samples[0]
	final := samples[len(samples)-1]

	// Check memory growth
	if baseline.Alloc > 0 {
		growthPct := (float64(final.Alloc) - float64(baseline.Alloc)) / float64(baseline.Alloc) * 100
		if growthPct > threshold {
			detections = append(detections, LeakDetection{
				LeakType:    "memory_growth",
				Description: fmt.Sprintf("Memory grew by %.2f%% (from %d to %d bytes)", growthPct, baseline.Alloc, final.Alloc),
				StartValue:  baseline.Alloc,
				EndValue:    final.Alloc,
				GrowthPct:   growthPct,
			})
		}
	}

	// Check goroutine growth
	goroutineGrowthPct := (float64(final.NumGoroutine) - float64(baseline.NumGoroutine)) / float64(baseline.NumGoroutine) * 100
	if goroutineGrowthPct > threshold {
		detections = append(detections, LeakDetection{
			LeakType:    "goroutine_leak",
			Description: fmt.Sprintf("Goroutines grew by %.2f%% (from %d to %d)", goroutineGrowthPct, baseline.NumGoroutine, final.NumGoroutine),
			StartValue:  uint64(baseline.NumGoroutine),
			EndValue:    uint64(final.NumGoroutine),
			GrowthPct:   goroutineGrowthPct,
		})
	}

	// Check heap fragmentation
	if final.HeapSys > 0 {
		idlePct := float64(final.HeapIdle) / float64(final.HeapSys) * 100
		if idlePct > 50 {
			detections = append(detections, LeakDetection{
				LeakType:    "heap_fragmentation",
				Description: fmt.Sprintf("Heap fragmentation detected: %.2f%% idle", idlePct),
				StartValue:  baseline.HeapIdle,
				EndValue:    final.HeapIdle,
				GrowthPct:   idlePct,
			})
		}
	}

	return detections
}

// LeakDetection represents a detected memory leak
type LeakDetection struct {
	LeakType    string
	Description string
	StartValue  uint64
	EndValue    uint64
	GrowthPct   float64
}

// CompareProfiles compares two memory profiles and returns the diff
func CompareProfiles(before, after MemoryProfile) ProfileDiff {
	diff := ProfileDiff{
		TimeDiff:        after.Timestamp.Sub(before.Timestamp),
		AllocDiff:       int64(after.Alloc) - int64(before.Alloc),
		TotalAllocDiff:  int64(after.TotalAlloc) - int64(before.TotalAlloc),
		SysDiff:         int64(after.Sys) - int64(before.Sys),
		NumGCDiff:       int32(after.NumGC) - int32(before.NumGC),
		GoroutineDiff:   after.NumGoroutine - before.NumGoroutine,
		HeapAllocDiff:   int64(after.HeapAlloc) - int64(before.HeapAlloc),
		HeapObjectsDiff: int64(after.HeapObjects) - int64(before.HeapObjects),
	}

	if before.Alloc > 0 {
		diff.AllocGrowthPct = (float64(after.Alloc) - float64(before.Alloc)) / float64(before.Alloc) * 100
	}

	if before.NumGoroutine > 0 {
		diff.GoroutineGrowthPct = (float64(after.NumGoroutine) - float64(before.NumGoroutine)) / float64(before.NumGoroutine) * 100
	}

	return diff
}

// ProfileDiff represents the difference between two memory profiles
type ProfileDiff struct {
	TimeDiff           time.Duration
	AllocDiff          int64
	AllocGrowthPct     float64
	TotalAllocDiff     int64
	SysDiff            int64
	NumGCDiff          int32
	GoroutineDiff      int
	GoroutineGrowthPct float64
	HeapAllocDiff      int64
	HeapObjectsDiff    int64
}

// String returns a human-readable string representation of the diff
func (d ProfileDiff) String() string {
	return fmt.Sprintf(`Memory Profile Diff:
  Time Elapsed: %v
  Alloc: %+d bytes (%.2f%%)
  TotalAlloc: %+d bytes
  Sys: %+d bytes
  NumGC: %+d cycles
  Goroutines: %+d (%.2f%%)
  HeapAlloc: %+d bytes
  HeapObjects: %+d objects`,
		d.TimeDiff,
		d.AllocDiff, d.AllocGrowthPct,
		d.TotalAllocDiff,
		d.SysDiff,
		d.NumGCDiff,
		d.GoroutineDiff, d.GoroutineGrowthPct,
		d.HeapAllocDiff,
		d.HeapObjectsDiff,
	)
}
