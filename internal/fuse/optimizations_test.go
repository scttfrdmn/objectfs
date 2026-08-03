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

// TestReadAheadManager_PrefetchScheduled_AfterEnoughHits pins the prefetch gate at MinSequential
// exactly, in both directions: nothing on the read before it, a prefetch on the read at it.
//
// The "nothing before" half is the point. This test used to read six times at MinSequential=3 and
// assert a prefetch, with a comment explaining that six was needed because of a confidence floor — so
// it documented #247 rather than catching it, and it would have passed just as well if MinSequential
// were ignored altogether. A gate test that only drives the satisfying case cannot tell a threshold
// of 3 from a threshold of 6.
func TestReadAheadManager_PrefetchScheduled_AfterEnoughHits(t *testing.T) {
	t.Parallel()

	const (
		blk    = int64(1024)
		path   = "prefetch.bin"
		minSeq = 3

		// The threshold lands on read minSeq, not minSeq+1, because the first read of a file counts as
		// sequential: a fresh ReadPattern has lastOffset and lastSize both zero, so `offset ==
		// lastOffset+lastSize` holds for a read at offset 0. That makes "min_sequential: 3" mean the
		// third read of a file, which is what the configuration documentation says. After a
		// non-sequential read resets the counter to zero it takes three more sequential reads, which is
		// the same rule counted from a different start.
		hitRead = minSeq
	)

	ram := newTestRAM(minSeq, 5*time.Minute)
	defer ram.Stop()

	// Reads up to but not including the one that reaches the threshold.
	for i := range hitRead - 1 {
		ram.OnRead(path, int64(i)*blk, blk)
	}

	if n := len(ram.prefetchQueue); n != 0 {
		t.Fatalf("%d prefetch(es) scheduled after %d sequential reads at min_sequential=%d, want 0. "+
			"The gate must not fire before the threshold, or the setting does not bound anything.",
			n, hitRead-1, minSeq)
	}

	// The read that reaches it.
	ram.OnRead(path, int64(hitRead-1)*blk, blk)

	select {
	case req := <-ram.prefetchQueue:
		if req.path != path {
			t.Errorf("expected prefetch for %q, got %q", path, req.path)
		}
		if want := int64(hitRead) * blk; req.offset != want {
			t.Errorf("prefetch offset=%d, want %d (one read past the last one performed)",
				req.offset, want)
		}
	default:
		t.Errorf("no prefetch scheduled on read %d at min_sequential=%d, want one. Before #247 the "+
			"first prefetch came on the sixth read whatever this was set to, because a confidence "+
			"floor over the same counter always won.", hitRead, minSeq)
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

// TestReadAheadConfig_DefaultValues pins the values a nil config falls back to.
//
// Written as a literal rather than a comparison against [DefaultReadAheadConfig] on purpose: this test
// exists to catch a change to those defaults, and comparing the function against itself would pass
// through any edit. internal/config's TestReadAheadDefaultsAreTheManagersOwn asserts the same numbers
// from the other side, so a change here without a change there fails there.
func TestReadAheadConfig_DefaultValues(t *testing.T) {
	t.Parallel()

	// NewReadAheadManager fills in nil config with the canonical defaults.
	ram := NewReadAheadManager(nil, nil)
	defer ram.Stop()

	want := ReadAheadConfig{
		Enabled:         true,
		WindowSize:      64 * 1024,
		MinSequential:   3,
		ConcurrentReads: 4,
		TTL:             5 * time.Minute,
	}

	if ram.config == nil {
		t.Fatal("a nil config left the manager with no config at all")
	}

	if *ram.config != want {
		t.Errorf("a nil config did not fall back to the documented defaults:\n got: %+v\nwant: %+v",
			*ram.config, want)
	}

	if got := DefaultReadAheadConfig(); got != want {
		t.Errorf("DefaultReadAheadConfig has drifted from the nil-config fallback, so a mount "+
			"(which goes through config) and a directly-constructed manager would prefetch "+
			"differently:\n got: %+v\nwant: %+v", got, want)
	}
}

// TestNewFileSystemPassesTheConfiguredReadAhead asserts the plumbed value reaches the manager.
//
// This is #176's seam. performance.read_ahead was decoded, validated, documented in five shipped preset
// files and read by nothing, because NewFileSystem constructed the manager with a literal nil. Deleting
// `config.ReadAhead` from that call — the mutation that reintroduces the defect — fails this test on
// every field.
func TestNewFileSystemPassesTheConfiguredReadAhead(t *testing.T) {
	t.Parallel()

	// Nothing here is a default: every value differs from DefaultReadAheadConfig, so a manager running
	// on the defaults fails on each field rather than coincidentally matching.
	want := ReadAheadConfig{
		Enabled:         true,
		WindowSize:      256 * 1024,
		MinSequential:   7,
		ConcurrentReads: 0, // no prefetch workers: the nil backend below is never dereferenced
		TTL:             90 * time.Second,
	}

	fs := NewFileSystem(nil, nil, nil, nil, &Config{
		MountPoint: t.TempDir(),
		ReadAhead:  &want,
	})
	t.Cleanup(fs.readAhead.Stop)

	if fs.readAhead == nil {
		t.Fatal("no read-ahead manager was constructed")
	}

	if fs.readAhead.config == nil {
		t.Fatal("the configured read-ahead settings did not reach the manager: its config is nil, " +
			"which is the state that made performance.read_ahead inert (#176)")
	}

	if *fs.readAhead.config != want {
		t.Errorf("the read-ahead manager is not running the configured settings, so every "+
			"performance.read_ahead key in a user's config file is ignored:\n got: %+v\nwant: %+v",
			*fs.readAhead.config, want)
	}
}

// TestNewFileSystemFallsBackWhenReadAheadIsUnset covers the other arm.
//
// Nil is a real state on this path — an internal caller that builds a [Config] by hand, and every
// mount before #176 — and it must mean "the manager's own defaults", not "no prefetching".
func TestNewFileSystemFallsBackWhenReadAheadIsUnset(t *testing.T) {
	t.Parallel()

	fs := NewFileSystem(nil, nil, nil, nil, &Config{MountPoint: t.TempDir()})
	t.Cleanup(fs.readAhead.Stop)

	if fs.readAhead == nil || fs.readAhead.config == nil {
		t.Fatal("a Config with no ReadAhead left the filesystem with no read-ahead manager")
	}

	if got := *fs.readAhead.config; got != DefaultReadAheadConfig() {
		t.Errorf("a Config with no ReadAhead did not fall back to DefaultReadAheadConfig:\n"+
			" got: %+v\nwant: %+v", got, DefaultReadAheadConfig())
	}
}
