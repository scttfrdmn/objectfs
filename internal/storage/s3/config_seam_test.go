package s3_test

// These are the runtime half of audit finding D12. internal/adapter's TestBuildS3ConfigMapsEveryConfiguredValue
// asserts that a configured value reaches s3.Config; these assert what the backend then does with it,
// against a real S3 endpoint. A mapping test alone would not have caught either defect below: both
// were live for a whole release with the *mapping* absent, and the way you notice a missing mapping
// from inside the backend is that the behavior it configures does not happen.

import (
	"context"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/circuit"
	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// TestBatchOperationsCompleteWithAnUnsetPoolSize is the deadlock regression test.
//
// GetObjects and PutObjects build `make(chan struct{}, PoolSize)` as a concurrency semaphore. With
// PoolSize zero that is an unbuffered channel and the first `semaphore <- struct{}{}` blocks with no
// receiver, forever. Not a slow batch — a wedged goroutine, holding whatever FUSE request was above
// it, for the life of the mount.
//
// It was reachable on every mounted filesystem until v0.10.1, because buildS3Config did not map
// performance.connection_pool_size and nothing in NewBackend defaulted it. Two things now stand in
// front of it: NewBackend defaults PoolSize, and the semaphore construction takes a floor of 1. This
// test defeats the first to exercise the second, then checks the ordinary path as well.
//
// The timeout is the assertion. A deadlocked batch does not fail this test by returning a wrong
// value; it fails by never returning, so the check has to be "did this finish", which means an
// explicit deadline rather than the package timeout — a hang under `go test` without one is 10
// minutes of no output and a panic dump naming the wrong goroutine.
func TestBatchOperationsCompleteWithAnUnsetPoolSize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		poolSize int
		why      string
	}{
		{
			name:     "zero, the value every v0.10.0 mount used",
			poolSize: 0,
			why: "make(chan struct{}, 0) is unbuffered; the first send has no receiver and blocks " +
				"forever",
		},
		{
			name:     "negative, which make would reject outright",
			poolSize: -1,
			why:      "make(chan struct{}, -1) panics; the floor has to handle this too",
		},
		{
			name:     "one",
			poolSize: 1,
		},
		{
			name:     "the default",
			poolSize: 8,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)

			// NewBackend defaults PoolSize, so the zero has to be reinstated after construction to
			// reach the semaphore code at all. Setting it through the mutator would test the default,
			// not the floor.
			backend := ts.Backend()
			s3.SetPoolSizeForTest(backend, tc.poolSize)

			keys := []string{"batch/a", "batch/b", "batch/c", "batch/d", "batch/e"}
			objects := make(map[string][]byte, len(keys))
			for _, k := range keys {
				objects[k] = testaws.DeterministicBytes(k, 1024)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			done := make(chan error, 1)
			go func() {
				if err := backend.PutObjects(ctx, objects); err != nil {
					done <- err
					return
				}

				got, err := backend.GetObjects(ctx, keys)
				if err != nil {
					done <- err
					return
				}
				if len(got) != len(keys) {
					t.Errorf("GetObjects returned %d of %d objects", len(got), len(keys))
				}
				done <- nil
			}()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("batch operations with PoolSize %d failed: %v", tc.poolSize, err)
				}
			case <-ctx.Done():
				// Deliberately not t.Fatal from a select on a goroutine that is still blocked: the
				// goroutine leaks either way, and naming the cause matters more than tidiness.
				t.Fatalf("batch operations with PoolSize %d did not complete within 30s — the "+
					"concurrency semaphore is not usable at this pool size. %s", tc.poolSize, tc.why)
			}
		})
	}
}

// TestParallelReadThresholdDrivesFanOut asserts the configured threshold decides how a large read is
// issued, measured in requests to S3 rather than in latency.
//
// This is the feature v0.10.0 was released for, and it was unreachable from a mount for the whole
// release: the gate in GetObject is `threshold > 0`, buildS3Config did not map the threshold, and
// NewBackend deliberately does not default it — zero is how this package spells "off". So every
// mounted filesystem read large objects serially while the configuration, the changelog and
// examples/config.yaml all described fan-out.
//
// Request count is the assertion because it is the thing that differs. Both paths return identical
// bytes and the emulator answers instantly, so a timing assertion would measure nothing and flake.
func TestParallelReadThresholdDrivesFanOut(t *testing.T) {
	t.Parallel()

	const (
		objectSize = 8 << 20
		chunkSize  = 1 << 20
		key        = "parallel/object"
	)

	cases := []struct {
		name      string
		threshold int64
		wantGETs  int
		why       string
	}{
		{
			name:      "disabled by a zero threshold",
			threshold: 0,
			wantGETs:  1,
			why: "zero is how the backend spells off, and it is what parallel_read.enabled false " +
				"maps to — one whole-object GET",
		},
		{
			name:      "above the object size",
			threshold: objectSize * 2,
			wantGETs:  1,
			why:       "the object is below the threshold, so it is not worth fanning out",
		},
		{
			name:      "below the object size",
			threshold: 1 << 20,
			wantGETs:  objectSize / chunkSize,
			why: "the read is split into ReadChunkSize ranges — eight 1 MiB GETs for an 8 MiB " +
				"object. One GET here means the fan-out gate never opened, which is the v0.10.0 " +
				"behavior",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			ts.RequireRangeGET()

			want := testaws.DeterministicBytes(key, objectSize)
			ts.PutObject(key, want)
			ts.ResetRequests()

			backend := ts.Backend(func(cfg *s3.Config) {
				cfg.ParallelReadThreshold = tc.threshold
				cfg.ReadChunkSize = chunkSize
				cfg.ParallelReadConcurrency = 4

				// Off so this test is about the threshold and nothing else. It no longer has to be:
				// the fan-out decision is keyed on the object rather than on this flag as of #228,
				// and TestFanOutIsDecidedByTheObjectNotTheConfig asserts exactly that by running
				// the same read with the flag both ways. Kept off here to hold the variables down
				// to the one under test.
				cfg.Compression.Enabled = false
			})

			got, err := backend.GetObject(context.Background(), key, 0, objectSize)
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}

			// Correctness before mechanics: a fan-out that assembles chunks in the wrong order, or
			// drops one, is a far worse defect than one that does not fan out at all. The audit found
			// the assembly arithmetic correct and the length assertion missing.
			if len(got) != len(want) {
				t.Fatalf("GetObject returned %d bytes, want %d — an assembled read that is short is "+
					"handed to the kernel as file content", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("byte %d differs (got %#x, want %#x); the assembled chunks are out of "+
						"order or overlapping", i, got[i], want[i])
				}
			}

			if n := len(ts.GETs(key)); n != tc.wantGETs {
				t.Errorf("GetObject issued %d GETs for a %d-byte object at threshold %d, want %d. %s",
					n, objectSize, tc.threshold, tc.wantGETs, tc.why)
			}
		})
	}
}

// TestStorageTierReachesTheStoredObject asserts the configured tier is what S3 records.
//
// storage_tier had the longest chain of any key in the audit and the quietest failure. It was named
// in the config schema, documented in examples/config.yaml, validated by nothing, unmapped by
// buildS3Config, defaulted to STANDARD by NewBackend, and then written. So `storage_tier:
// GLACIER_IR` produced STANDARD objects — no error, no warning, and a bill that silently differed
// from the one the configuration described. There is no way to notice that from inside ObjectFS;
// the only witness is the storage class on the stored object.
func TestStorageTierReachesTheStoredObject(t *testing.T) {
	t.Parallel()

	for _, tier := range []string{s3.TierStandard, s3.TierStandardIA, s3.TierGlacierIR} {
		t.Run(tier, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			backend := ts.Backend(func(cfg *s3.Config) {
				cfg.StorageTier = tier
			})

			const key = "tier/object"
			data := testaws.DeterministicBytes(key, 256*1024)

			if err := backend.PutObject(context.Background(), key, data, nil); err != nil {
				t.Fatalf("PutObject at tier %s: %v", tier, err)
			}

			if got := ts.ObjectStorageClass(key); got != tier {
				t.Errorf("object stored with class %q, want %q — the configured tier did not reach "+
					"S3, so the object is billed as something other than what the config says",
					got, tier)
			}
		})
	}
}

// TestCircuitBreakerConfigReachesTheBreaker asserts the configured failure threshold is the count
// that opens the circuit.
//
// The mapping is not a copy: circuit.Config has no threshold field, only a ReadyToTrip predicate, so
// the count has to become a closure. Getting that wrong has two failure modes, and both are quiet.
// Reading MaxRequests as the threshold — a mistake made during the audit; it is the half-open probe
// limit — leaves the breaker at its proportional default while the config says otherwise. Building
// `failures >= 0` for an unset threshold opens the breaker before the first request and rejects every
// S3 operation for the life of the mount.
func TestCircuitBreakerConfigReachesTheBreaker(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		cfg       s3.CircuitBreakerConfig
		failures  int
		wantTrip  bool
		wantNilFn bool
		why       string
	}{
		{
			name:      "an unset threshold takes the package default",
			cfg:       s3.CircuitBreakerConfig{Enabled: true},
			wantNilFn: true,
			why: "nil means NewCircuitBreaker installs its proportional default. A `failures >= 0` " +
				"closure would trip on the zeroth failure and wedge the mount",
		},
		{
			name:     "a configured threshold is an absolute count",
			cfg:      s3.CircuitBreakerConfig{Enabled: true, FailureThreshold: 3},
			failures: 3,
			wantTrip: true,
		},
		{
			name:     "below the threshold does not trip",
			cfg:      s3.CircuitBreakerConfig{Enabled: true, FailureThreshold: 3},
			failures: 2,
			wantTrip: false,
		},
		{
			name:     "disabled never trips",
			cfg:      s3.CircuitBreakerConfig{Enabled: false, FailureThreshold: 1},
			failures: 1000,
			wantTrip: false,
			why: "enabled: false is a predicate that never opens the circuit, not a bypass — the " +
				"breaker stays in the call path counting and reporting state",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fn := s3.ReadyToTripForTest(tc.cfg)

			if tc.wantNilFn {
				if fn != nil {
					t.Fatalf("readyToTrip returned a predicate for an unset threshold; it must be "+
						"nil. %s", tc.why)
				}
				return
			}

			if fn == nil {
				t.Fatal("readyToTrip returned nil for a configured threshold, so the configured " +
					"count is silently replaced by the package default")
			}

			//nolint:gosec // small test values
			got := fn(circuit.Counts{TotalFailures: uint32(tc.failures)})
			if got != tc.wantTrip {
				t.Errorf("with %d failures and threshold %d, ReadyToTrip = %v, want %v. %s",
					tc.failures, tc.cfg.FailureThreshold, got, tc.wantTrip, tc.why)
			}
		})
	}
}
