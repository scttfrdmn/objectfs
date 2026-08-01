package s3

// Test-only accessors for unexported behavior that the external s3_test package has to reach.
//
// They live here rather than in the production files so that nothing shipped carries a hook whose
// only caller is a test. The external test package is unavoidable for most of these: it needs
// internal/testaws for a real S3 endpoint, and testaws imports this package, so an in-package test
// that used the harness would be an import cycle.

import "github.com/objectfs/objectfs/internal/circuit"

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

// ReadyToTripForTest exposes the CircuitBreakerConfig → predicate translation.
//
// A nil return is meaningful, not an absence: it is how the mapping says "use circuit's proportional
// default", and the alternative a careless implementation reaches for — a `failures >= 0` closure —
// opens the breaker before the first request. So the test needs to see the nil.
func ReadyToTripForTest(cfg CircuitBreakerConfig) func(circuit.Counts) bool {
	return readyToTrip(cfg)
}
