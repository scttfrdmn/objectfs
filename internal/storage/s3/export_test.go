package s3

// Test-only accessors for unexported behavior that the external s3_test package has to reach.
//
// They live here rather than in the production files so that nothing shipped carries a hook whose
// only caller is a test. The external test package is unavoidable for most of these: it needs
// internal/testaws for a real S3 endpoint, and testaws imports this package, so an in-package test
// that used the harness would be an import cycle.

import (
	"context"

	"github.com/scttfrdmn/objectfs/internal/circuit"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// SetPoolSizeForTest overwrites a live backend's pool size.
//
// It exists to defeat NewBackend's defaulting. PoolSize zero is the value every v0.10.0 mount used
// and it deadlocks the batch paths, so a regression test has to be able to produce it — and passing
// it to NewBackend no longer can, because NewBackend now backfills it. Reinstating it afterwards
// tests the semaphore's own floor rather than the constructor's default.
func SetPoolSizeForTest(b *Backend, n int) {
	b.config.PoolSize = n
}

// BatchConcurrencyForTest returns the concurrency the batch paths would use.
func BatchConcurrencyForTest(b *Backend) int {
	return b.batchConcurrency()
}

// SeedAccessPatternForTest installs an access pattern on a live backend's cost optimizer.
//
// It exists because a tier transition is only reachable through analysis, and analysis refuses any
// object younger than 30 days. Without a way to plant an aged pattern, the CopyObject that performs
// the transition has no test at all — which is how it came to be the one write path that dropped the
// encryption header with nothing to notice. AccessPattern is already exported; only the locked
// installer is not, and going through it is the point: the map is written from every reader goroutine.
func SeedAccessPatternForTest(b *Backend, pattern AccessPattern) {
	b.costOptimizer.putPattern(pattern)
}

// SetCopyThresholdsForTest lowers the size at which CopyObject switches to a multipart copy, and the
// size of each part it then copies.
//
// Without it the multipart copy path needs an object larger than S3's 5 GiB single-part copy limit.
// Creating one costs real storage and hours of transfer, so that branch would go untested — and a
// mutation removing the routing entirely produced no test failure, which is exactly what "untested"
// looks like from the inside. Scaling the thresholds down instead exercises the same code with a few
// kilobytes: the part loop, the CopySourceRange arithmetic, the metadata that has to be restated
// because MetadataDirective=COPY is unavailable on a multipart upload, and the deferred abort.
//
// What it does not test is S3's 5 MB minimum for a non-final part, which a scaled-down part size
// violates. Real S3 answers EntityTooSmall at Complete; the emulator does not enforce it below the
// threshold, so this is a known limit of the scaled test rather than something it verifies.
func SetCopyThresholdsForTest(b *Backend, singlePartLimit, partSize int64) {
	b.singlePartCopyLimit = singlePartLimit
	b.copyPartSize = partSize
}

// ReadyToTripForTest exposes the CircuitBreakerConfig → predicate translation.
//
// A nil return is meaningful, not an absence: it is how the mapping says "use circuit's proportional
// default", and the alternative a careless implementation reaches for — a `failures >= 0` closure —
// opens the breaker before the first request. So the test needs to see the nil.
func ReadyToTripForTest(cfg CircuitBreakerConfig) func(circuit.Counts) bool {
	return readyToTrip(cfg)
}

// CapabilityProbeKeyForTest is the key the conditional-write probe asserts against.
//
// A test asserting the probe left nothing behind has to name the key, and hardcoding the literal in the
// test would let the two drift: renaming the constant would leave the test checking that an object does
// not exist at a key nothing ever writes to, which passes for the wrong reason.
const CapabilityProbeKeyForTest = capabilityProbeKey

// ProbeConditionalWriteForTest runs the capability probe against b's endpoint, bypassing the cache.
//
// Capabilities() caches through a sync.Once, so a test that needs to probe a *particular* endpoint —
// notably one that accepts conditional headers and ignores them — cannot get at the probe through the
// public method without constructing a fresh backend per case.
func ProbeConditionalWriteForTest(b *Backend) types.BackendCapabilities {
	return b.probeConditionalWrite(context.Background())
}
