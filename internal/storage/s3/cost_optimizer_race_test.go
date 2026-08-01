package s3_test

// The concurrency half of audit finding M12. cost_optimizer_test.go covers what RecordAccess
// computes; this covers whether it is safe to call, which is a different question and the one that
// was wrong.
//
// It is an external test on purpose — it drives RecordAccess the way production does, through
// GetObject against a real S3 endpoint, rather than by calling it directly. The map was written from
// the read path; a test that called the optimizer straight would have proven the lock works without
// proving the path that needs it is the path that has it.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/objectfs/objectfs/internal/storage/s3"
	"github.com/objectfs/objectfs/internal/testaws"
)

// TestAccessPatternTrackingIsSafeUnderConcurrentReads is a race regression test, so it is only
// meaningful under -race — which is how this repo runs every test.
//
// CostOptimizer.accessPatterns was a plain map written by RecordAccess, called from GetObject on both
// the serial and the parallel read paths. Concurrent reads of distinct objects therefore wrote the
// same map from several goroutines at once, and a concurrent map write is not a race Go tolerates:
// the runtime aborts the process with "concurrent map writes". On a mount that is not a failed read,
// it is the filesystem disappearing with every open descriptor on it.
//
// It was latent in v0.10.0 for a reason that is itself a defect. MonitorAccessPatterns defaults false
// and buildS3Config mapped no part of the cost-optimization block, so the gate at the top of
// RecordAccess always returned early — the map was never written, and the knob that would have
// written it did nothing. Fixing the plumbing is what makes this reachable, so the lock lands with
// the plumbing rather than after it.
//
// Reads of *distinct* keys are what matters: same-key reads take the update branch, but the map
// insert on the first access of each key is the write that grows the map, and that is the one the
// runtime aborts on. So every goroutine here reads a different object.
func TestAccessPatternTrackingIsSafeUnderConcurrentReads(t *testing.T) {
	t.Parallel()

	const (
		readers    = 16
		objectSize = 4096
	)

	ts := testaws.Start(t)

	keys := make([]string, readers)
	for i := range keys {
		keys[i] = fmt.Sprintf("patterns/object-%02d", i)
		ts.PutObject(keys[i], testaws.DeterministicBytes(keys[i], objectSize))
	}

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.CostOptimization.MonitorAccessPatterns = true

		// Compression off so the read path is the plain one; the compressed path has its own
		// coverage and would only add variables.
		cfg.Compression.Enabled = false
	})

	ctx := context.Background()

	// The readers race each other and the report readers. Both directions are needed: RecordAccess
	// against RecordAccess is the map-write race, and RecordAccess against a range over the map is
	// the read-during-write race, which the detector reports separately.
	var wg sync.WaitGroup

	for _, key := range keys {
		wg.Go(func() {
			for range 4 {
				if _, err := backend.GetObject(ctx, key, 0, objectSize); err != nil {
					t.Errorf("GetObject(%q): %v", key, err)
					return
				}
			}
		})
	}

	for range 4 {
		wg.Go(func() {
			for range 8 {
				// Both accessors range over or measure the map. GetAccessPatternCount used to take
				// len() of it directly from the backend, one call site away from the lock.
				_ = backend.GetCostOptimizationReport()
				_ = backend.GetAccessPatternCount()
			}
		})
	}

	wg.Wait()

	// Every key was read, so every key must be tracked. This is the assertion that the lock did not
	// accidentally make the whole feature a no-op: a mutex around a `return` also passes a race test.
	if got := backend.GetAccessPatternCount(); got != readers {
		t.Errorf("GetAccessPatternCount = %d after reading %d distinct objects, want %d — access "+
			"patterns are being dropped, not merely tracked safely", got, readers, readers)
	}
}

// TestAccessPatternTrackingStaysOffWhenNotConfigured pins the gate that made M12 latent.
//
// MonitorAccessPatterns false must mean no tracking at all, not tracking that is merely unused: the
// map is unbounded and keyed by object key, so a mount that reads a large bucket would accumulate
// one AccessPattern per object read, for the life of the process, to feed a report nothing requests.
// That is the reason the default is off, and it is worth a test because the gate is a single early
// return that a refactor could drop without failing anything else.
func TestAccessPatternTrackingStaysOffWhenNotConfigured(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	const key = "patterns/untracked"
	ts.PutObject(key, testaws.DeterministicBytes(key, 4096))

	// Default config: MonitorAccessPatterns is absent, which is what every mount gets today.
	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = false
	})

	if _, err := backend.GetObject(context.Background(), key, 0, 4096); err != nil {
		t.Fatalf("GetObject: %v", err)
	}

	if got := backend.GetAccessPatternCount(); got != 0 {
		t.Errorf("GetAccessPatternCount = %d with monitoring unconfigured, want 0 — the read path is "+
			"accumulating an unbounded per-object map nothing consumes", got)
	}
}
