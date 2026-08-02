//go:build linux || darwin

package fuse

import (
	"sync"
	"testing"
	"time"
)

// ─── ReadAheadManager ───────────────────────────────────────────────────────

// newTestRAM builds a ReadAheadManager with ConcurrentReads=0 so the prefetch
// workers are never started.  The fs pointer is nil; no code path in
// pattern-detection dereferences it.
func newTestRAM(minSeq int, ttl time.Duration) *ReadAheadManager {
	cfg := &ReadAheadConfig{
		Enabled:         true,
		WindowSize:      64 * 1024,
		MaxDistance:     1024 * 1024,
		MinSequential:   minSeq,
		ConcurrentReads: 0, // no prefetch workers → nil fs is safe
		TTL:             ttl,
	}
	return NewReadAheadManager(nil, cfg)
}

func TestReadAheadManager_DefaultConfig(t *testing.T) {
	t.Parallel()

	// When no config is provided, defaults should be applied.
	// Use ConcurrentReads=0 to prevent workers from touching nil fs.
	cfg := &ReadAheadConfig{
		Enabled:         true,
		WindowSize:      64 * 1024,
		MaxDistance:     1024 * 1024,
		MinSequential:   3,
		ConcurrentReads: 0,
		TTL:             5 * time.Minute,
	}
	ram := NewReadAheadManager(nil, cfg)
	defer ram.Stop()

	if !ram.config.Enabled {
		t.Error("expected Enabled=true")
	}
	if ram.config.WindowSize != 64*1024 {
		t.Errorf("expected WindowSize=65536, got %d", ram.config.WindowSize)
	}
	if ram.config.MinSequential != 3 {
		t.Errorf("expected MinSequential=3, got %d", ram.config.MinSequential)
	}
}

func TestReadAheadManager_FirstRead_CreatesPattern(t *testing.T) {
	t.Parallel()

	ram := newTestRAM(3, 5*time.Minute)
	defer ram.Stop()

	ram.OnRead("data.bin", 0, 1024)

	ram.mu.RLock()
	pattern, exists := ram.activeReads["data.bin"]
	ram.mu.RUnlock()

	if !exists {
		t.Fatal("expected pattern to exist after first read")
	}
	if pattern.lastOffset != 0 {
		t.Errorf("expected lastOffset=0, got %d", pattern.lastOffset)
	}
	if pattern.lastSize != 1024 {
		t.Errorf("expected lastSize=1024, got %d", pattern.lastSize)
	}
	if pattern.predictedNext != 1024 {
		t.Errorf("expected predictedNext=1024, got %d", pattern.predictedNext)
	}
}

func TestReadAheadManager_SequentialIncrementsHits(t *testing.T) {
	t.Parallel()

	ram := newTestRAM(3, 5*time.Minute)
	defer ram.Stop()

	// 4 sequential reads: offsets 0, 1024, 2048, 3072
	for i := range 4 {
		ram.OnRead("seq.bin", int64(i)*1024, 1024)
	}

	ram.mu.RLock()
	p := ram.activeReads["seq.bin"]
	ram.mu.RUnlock()

	// After 4 reads the first one starts a new entry and then we have 3 more
	// sequential hits (reads 2, 3, 4 each increment sequentialHits).
	if p.sequentialHits < 2 {
		t.Errorf("expected >= 2 sequential hits, got %d", p.sequentialHits)
	}
}

func TestReadAheadManager_NonSequential_ResetsPattern(t *testing.T) {
	t.Parallel()

	ram := newTestRAM(3, 5*time.Minute)
	defer ram.Stop()

	// Build up some sequential hits.
	for i := range 4 {
		ram.OnRead("rnd.bin", int64(i)*1024, 1024)
	}

	// Random jump resets the counters.
	ram.OnRead("rnd.bin", 99999, 512)

	ram.mu.RLock()
	p := ram.activeReads["rnd.bin"]
	ram.mu.RUnlock()

	if p.sequentialHits != 0 {
		t.Errorf("expected sequentialHits=0 after non-sequential read, got %d", p.sequentialHits)
	}
	if p.confidence != 0.1 {
		t.Errorf("expected confidence=0.1 after reset, got %f", p.confidence)
	}
}

func TestReadAheadManager_Disabled_NoPatterns(t *testing.T) {
	t.Parallel()

	cfg := &ReadAheadConfig{
		Enabled:         false,
		MinSequential:   3,
		ConcurrentReads: 0,
		TTL:             5 * time.Minute,
	}
	ram := NewReadAheadManager(nil, cfg)
	defer ram.Stop()

	ram.OnRead("disabled.bin", 0, 1024)

	ram.mu.RLock()
	_, exists := ram.activeReads["disabled.bin"]
	ram.mu.RUnlock()

	if exists {
		t.Error("expected no pattern when manager is disabled")
	}
}

func TestReadAheadManager_PrefetchScheduled_AfterEnoughHits(t *testing.T) {
	t.Parallel()

	// Prefetch requires sequentialHits >= MinSequential AND confidence > 0.5.
	// confidence = sequentialHits / 10.0 → need sequentialHits >= 6 for both
	// conditions to be met when MinSequential=3.
	ram := newTestRAM(3, 5*time.Minute)
	defer ram.Stop()

	const blk = int64(1024)
	const path = "prefetch.bin"
	// 6 sequential reads puts sequentialHits=6, confidence=0.6.
	for i := range 6 {
		ram.OnRead(path, int64(i)*blk, blk)
	}

	// The prefetchQueue channel is buffered (cap 100); at least one request
	// must have been enqueued.
	select {
	case req := <-ram.prefetchQueue:
		if req.path != path {
			t.Errorf("expected prefetch for %q, got %q", path, req.path)
		}
		if req.offset != 6*blk {
			t.Errorf("expected prefetch offset=%d, got %d", 6*blk, req.offset)
		}
	default:
		t.Error("expected at least one prefetch request after 6 sequential reads")
	}
}

func TestReadAheadManager_PatternTTL_Cleanup(t *testing.T) {
	t.Parallel()

	ram := newTestRAM(3, 10*time.Millisecond) // very short TTL
	defer ram.Stop()

	ram.OnRead("short.bin", 0, 1024)

	// Verify pattern exists.
	ram.mu.RLock()
	_, exists := ram.activeReads["short.bin"]
	ram.mu.RUnlock()
	if !exists {
		t.Fatal("expected pattern before TTL expiry")
	}

	// Wait for TTL to lapse, then trigger cleanup explicitly.
	time.Sleep(30 * time.Millisecond)
	ram.cleanup()

	ram.mu.RLock()
	_, exists = ram.activeReads["short.bin"]
	ram.mu.RUnlock()
	if exists {
		t.Error("expected pattern to be cleaned up after TTL")
	}
}

// There are no WriteCoalescer tests because there is no WriteCoalescer. Thirteen of them lived here
// and passed while the type they covered corrupted data on a routine overwrite.
//
// TestWriteCoalescer_MergeOverlappingWrites is the instructive one: it wrote "hello!" at 0 and
// "LO_WORLD" at 3, then asserted only that the merge produced one write of eleven bytes. It never
// looked at the bytes. mergeWrites guarded its overlay with `if newEnd > currentEnd`, so a newer
// write shorter than what it overlapped was dropped entirely — `echo NEW > f` over a file holding
// OLD left the file reading OLD — and a length assertion cannot see that.
//
// Coalescing now lives in internal/vfs.ExtentList.Add, whose tests assert on resulting content.

// ─── Stats ───────────────────────────────────────────────────────────────────

func TestStats_ConcurrentReadWrites(t *testing.T) {
	t.Parallel()

	stats := &Stats{}
	const n = 200

	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			stats.mu.Lock()
			stats.Reads++
			stats.BytesRead += 512
			stats.mu.Unlock()
		})
	}
	wg.Wait()

	stats.mu.RLock()
	defer stats.mu.RUnlock()

	if stats.Reads != int64(n) {
		t.Errorf("expected %d reads, got %d", n, stats.Reads)
	}
	if stats.BytesRead != int64(n)*512 {
		t.Errorf("expected BytesRead=%d, got %d", int64(n)*512, stats.BytesRead)
	}
}

func TestStats_AllCounters(t *testing.T) {
	t.Parallel()

	stats := &Stats{}

	stats.mu.Lock()
	stats.Lookups = 1
	stats.Opens = 2
	stats.Reads = 3
	stats.Writes = 4
	stats.Creates = 5
	stats.Deletes = 6
	stats.BytesRead = 1024
	stats.BytesWritten = 2048
	stats.CacheHits = 10
	stats.CacheMisses = 5
	stats.Errors = 1
	stats.mu.Unlock()

	stats.mu.RLock()
	defer stats.mu.RUnlock()

	checks := []struct {
		name string
		got  int64
		want int64
	}{
		{"Lookups", stats.Lookups, 1},
		{"Opens", stats.Opens, 2},
		{"Reads", stats.Reads, 3},
		{"Writes", stats.Writes, 4},
		{"Creates", stats.Creates, 5},
		{"Deletes", stats.Deletes, 6},
		{"BytesRead", stats.BytesRead, 1024},
		{"BytesWritten", stats.BytesWritten, 2048},
		{"CacheHits", stats.CacheHits, 10},
		{"CacheMisses", stats.CacheMisses, 5},
		{"Errors", stats.Errors, 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: want %d, got %d", c.name, c.want, c.got)
		}
	}
}

// ─── Config ──────────────────────────────────────────────────────────────────

func TestReadAheadConfig_DefaultValues(t *testing.T) {
	t.Parallel()

	// NewReadAheadManager fills in nil config with the canonical defaults.
	ram := NewReadAheadManager(nil, nil)
	defer ram.Stop()

	cfg := ram.config
	if !cfg.Enabled {
		t.Error("expected Enabled=true")
	}
	if cfg.WindowSize != 64*1024 {
		t.Errorf("WindowSize: want %d, got %d", 64*1024, cfg.WindowSize)
	}
	if cfg.MaxDistance != 1024*1024 {
		t.Errorf("MaxDistance: want %d, got %d", 1024*1024, cfg.MaxDistance)
	}
	if cfg.MinSequential != 3 {
		t.Errorf("MinSequential: want 3, got %d", cfg.MinSequential)
	}
	if cfg.ConcurrentReads != 4 {
		t.Errorf("ConcurrentReads: want 4, got %d", cfg.ConcurrentReads)
	}
	if cfg.TTL != 5*time.Minute {
		t.Errorf("TTL: want 5m, got %v", cfg.TTL)
	}
}
