package metrics

import (
	"testing"
	"time"
)

// Tests for the latency bucketing and the three percentiles computed from it (#222).
//
// Two of these are regression tests against specific arithmetic rather than against a feature. The
// bound they replaced was `int(latency.Milliseconds()) % 100`, which is a hash of the latency into 100
// slots, and the way to tell a bucketing from a hash is to record two latencies the modulo collides and
// assert they land apart. TestDistantLatenciesDoNotShareABucket and
// TestSubMillisecondLatenciesAreResolved are that assertion; both fail on the old line and neither can
// be satisfied by a scheme that keeps it.

// TestDistantLatenciesDoNotShareABucket is the direct regression test for the modulo.
//
// 50ms and 250ms both have `Milliseconds() % 100 == 50`, so the previous code incremented one bucket
// twice and any percentile drawn from it would have reported 250ms operations as 50ms ones. The pairs
// below are every collision class the old scheme had: same residue, and — for 400µs against 1ms — two
// latencies truncated to the same whole millisecond.
func TestDistantLatenciesDoNotShareABucket(t *testing.T) {
	t.Parallel()

	pairs := []struct {
		name   string
		fast   time.Duration
		slow   time.Duration
		reason string
	}{
		{
			name:   "50ms vs 250ms",
			fast:   50 * time.Millisecond,
			slow:   250 * time.Millisecond,
			reason: "both are 50 mod 100",
		},
		{
			name:   "1ms vs 101ms",
			fast:   time.Millisecond,
			slow:   101 * time.Millisecond,
			reason: "both are 1 mod 100",
		},
		{
			name:   "1ms vs 1001ms",
			fast:   time.Millisecond,
			slow:   1001 * time.Millisecond,
			reason: "both are 1 mod 100, three orders of magnitude apart",
		},
		{
			name:   "400us vs 1ms",
			fast:   400 * time.Microsecond,
			slow:   time.Millisecond,
			reason: "Milliseconds() truncates 400us to 0, and 0 mod 100 is 0",
		},
	}

	for _, pair := range pairs {
		t.Run(pair.name, func(t *testing.T) {
			t.Parallel()

			fast, slow := latencyBucket(pair.fast), latencyBucket(pair.slow)
			if fast == slow {
				t.Fatalf("%v and %v both landed in bucket %d (%s); a histogram that cannot separate them "+
					"cannot answer what fraction of operations were under a given latency",
					pair.fast, pair.slow, fast, pair.reason)
			}
			if fast >= slow {
				t.Fatalf("%v is in bucket %d and %v in bucket %d: the buckets are not ordered by latency",
					pair.fast, fast, pair.slow, slow)
			}
		})
	}
}

// TestSubMillisecondLatenciesAreResolved covers the case the modulo was worst at and that matters most:
// a cache hit.
//
// Milliseconds() truncates, so 20µs, 300µs and 999µs were indistinguishable — all bucket 0 — and so was
// an operation at exactly 100ms. An L1 hit is tens of microseconds, so under the old scheme the fast
// path had no resolution at all.
func TestSubMillisecondLatenciesAreResolved(t *testing.T) {
	t.Parallel()

	seen := map[int]time.Duration{}
	for _, latency := range []time.Duration{
		30 * time.Microsecond,
		120 * time.Microsecond,
		500 * time.Microsecond,
		900 * time.Microsecond,
	} {
		bucket := latencyBucket(latency)
		if previous, collided := seen[bucket]; collided {
			t.Errorf("%v and %v share bucket %d; sub-millisecond is the expected case for a cache hit",
				previous, latency, bucket)
		}
		seen[bucket] = latency
	}

	if got := len(seen); got < 4 {
		t.Errorf("four sub-millisecond latencies occupied %d bucket(s), want 4", got)
	}
}

// TestSlowOperationsOverflowRatherThanWrapping asserts the top of the range.
//
// Under the modulo, a latency past the top of the array wrapped to a *fast* bucket — the failure
// direction that matters, since it makes a slow filesystem look fast. The overflow bucket is the last
// index and nothing above it.
func TestSlowOperationsOverflowRatherThanWrapping(t *testing.T) {
	t.Parallel()

	bounds := LatencyBucketBounds()
	overflow := len(bounds)

	for _, latency := range []time.Duration{
		bounds[len(bounds)-1] + time.Nanosecond,
		10 * time.Minute,
		time.Hour,
	} {
		if got := latencyBucket(latency); got != overflow {
			t.Errorf("latencyBucket(%v) = %d, want the overflow bucket %d", latency, got, overflow)
		}
	}

	// And the last finite bound itself is *not* overflow: the bounds are inclusive upper bounds.
	if got := latencyBucket(bounds[len(bounds)-1]); got != overflow-1 {
		t.Errorf("latencyBucket(%v) = %d, want %d: the last bound belongs to its own bucket, not to overflow",
			bounds[len(bounds)-1], got, overflow-1)
	}
}

// TestEveryBucketIsReachable walks the bounds and asserts each one selects its own index.
//
// A scheme with an unreachable bucket wastes resolution silently, and an off-by-one in the search
// predicate — `<` instead of `<=` — shows up here and nowhere else in this file.
func TestEveryBucketIsReachable(t *testing.T) {
	t.Parallel()

	bounds := LatencyBucketBounds()
	for i, bound := range bounds {
		if got := latencyBucket(bound); got != i {
			t.Errorf("the upper bound %v of bucket %d selected bucket %d", bound, i, got)
		}

		// Just inside the bucket, from above, must also select it.
		if got := latencyBucket(bound - time.Nanosecond); got != i {
			t.Errorf("%v (one nanosecond under bucket %d's bound) selected bucket %d", bound-time.Nanosecond, i, got)
		}
	}

	if got := latencyBucket(0); got != 0 {
		t.Errorf("latencyBucket(0) = %d, want 0", got)
	}
}

// TestLatencyBucketBoundsCannotBeMutatedByACaller guards the exported accessor.
//
// It returns package state, and the histogram's meaning depends on those numbers, so handing out the
// backing array would let any caller silently redefine what every recorded count means.
func TestLatencyBucketBoundsCannotBeMutatedByACaller(t *testing.T) {
	t.Parallel()

	first := LatencyBucketBounds()
	original := first[0]
	first[0] = 999 * time.Hour

	if second := LatencyBucketBounds(); second[0] != original {
		t.Fatalf("a caller's write to the returned slice changed bucket 0's bound to %v", second[0])
	}
}

// TestPercentilesReflectTheTail is the assertion #222 asks for: 1ms ×95 and 500ms ×5, with P99 in the
// 500ms band rather than at zero or at 1ms.
//
// Zero is what the fields held before, and it is the reading that matters — zero percentiles beside a
// populated average_latency describe a filesystem with no tail latency, which is the most flattering
// possible wrong answer. 1ms is what a modulo-derived histogram would have said, since 500ms collides
// with 0ms under it.
func TestPercentilesReflectTheTail(t *testing.T) {
	t.Parallel()

	dpm := NewDetailedPerformanceMetrics(100, false)
	for range 95 {
		dpm.RecordOperation(OpRead, "", time.Millisecond, 1024, CacheSourceL1, nil)
	}
	for range 5 {
		dpm.RecordOperation(OpRead, "", 500*time.Millisecond, 1024, CacheSourceBackend, nil)
	}

	om := dpm.GetOperationMetrics(OpRead)
	if om == nil {
		t.Fatal("no metrics recorded for OpRead")
	}

	if om.P99Latency == 0 {
		t.Fatal("P99Latency is zero after 100 recorded operations; zero percentiles beside a populated " +
			"average read as a filesystem with no tail latency at all")
	}

	// The 100th-percentile rank falls in the 500ms band. Bucket widths at that scale are wide — 25µs·2^n
	// — so assert the band, not a figure.
	if om.P99Latency < 250*time.Millisecond || om.P99Latency > time.Second {
		t.Errorf("P99Latency = %v, want the 500ms band; 5%% of operations took 500ms", om.P99Latency)
	}

	// P50 is squarely in the 1ms population.
	if om.P50Latency < 500*time.Microsecond || om.P50Latency > 2*time.Millisecond {
		t.Errorf("P50Latency = %v, want ~1ms; 95%% of operations took 1ms", om.P50Latency)
	}

	// And the ordering that makes them percentiles at all.
	if om.P50Latency > om.P95Latency || om.P95Latency > om.P99Latency {
		t.Errorf("percentiles are not monotonic: p50=%v p95=%v p99=%v",
			om.P50Latency, om.P95Latency, om.P99Latency)
	}
}

// TestPercentilesAreBoundedByWhatWasMeasured asserts the estimate never claims a latency outside the
// observed range.
//
// Interpolating within the covering bucket is what keeps this true, and it is the property that makes
// the numbers usable: a p99 above MaxLatency is a measurement of nothing, and one below MinLatency
// under-reports the tail, which is the direction that hides a problem.
func TestPercentilesAreBoundedByWhatWasMeasured(t *testing.T) {
	t.Parallel()

	dpm := NewDetailedPerformanceMetrics(100, false)
	for _, latency := range []time.Duration{
		40 * time.Microsecond,
		2 * time.Millisecond,
		30 * time.Millisecond,
		700 * time.Millisecond,
		3 * time.Second,
	} {
		dpm.RecordOperation(OpWrite, "", latency, 4096, CacheSourceBackend, nil)
	}

	om := dpm.GetOperationMetrics(OpWrite)
	if om == nil {
		t.Fatal("no metrics recorded for OpWrite")
	}

	// The bucket covering the minimum starts below it, so an interpolated p50 can dip under MinLatency by
	// up to one bucket width. What must hold is that nothing exceeds the maximum.
	for _, p := range []struct {
		name  string
		value time.Duration
	}{
		{"P50Latency", om.P50Latency},
		{"P95Latency", om.P95Latency},
		{"P99Latency", om.P99Latency},
	} {
		if p.value > om.MaxLatency*2 {
			t.Errorf("%s = %v against a maximum observed latency of %v", p.name, p.value, om.MaxLatency)
		}
		if p.value <= 0 {
			t.Errorf("%s = %v after five recorded operations", p.name, p.value)
		}
	}
}

// TestASingleOperationGetsPercentilesInItsOwnBucket covers the smallest input.
//
// One sample means all three percentiles are that sample, and it is the case where an off-by-one in the
// rank arithmetic — `<` where `<=` belongs, or a rank of total rather than total-1 — produces zero
// instead, which is indistinguishable from the fields never being assigned.
func TestASingleOperationGetsPercentilesInItsOwnBucket(t *testing.T) {
	t.Parallel()

	dpm := NewDetailedPerformanceMetrics(100, false)
	dpm.RecordOperation(OpGetAttr, "", 8*time.Millisecond, 0, CacheSourceL1, nil)

	om := dpm.GetOperationMetrics(OpGetAttr)
	if om == nil {
		t.Fatal("no metrics recorded for OpGetAttr")
	}

	bucket := latencyBucket(8 * time.Millisecond)
	bounds := LatencyBucketBounds()
	lower := time.Duration(0)
	if bucket > 0 {
		lower = bounds[bucket-1]
	}
	upper := bounds[bucket]

	for _, p := range []struct {
		name  string
		value time.Duration
	}{
		{"P50Latency", om.P50Latency},
		{"P95Latency", om.P95Latency},
		{"P99Latency", om.P99Latency},
	} {
		if p.value <= lower || p.value > upper {
			t.Errorf("%s = %v, want within (%v, %v] — the bucket holding the single 8ms sample",
				p.name, p.value, lower, upper)
		}
	}
}

// TestPercentilesSurviveAnOverflowingLatency asserts a very slow operation reports as slow.
//
// The overflow bucket has no upper bound to interpolate toward, so the estimate saturates at the last
// finite bound. Saturating high is the correct direction; wrapping to a fast bucket, which the modulo
// did, is the one that hides the problem.
//
// Five overflowing samples in a hundred, not one: the p99 rank is the 99th sample, so a single outlier
// in a hundred does not reach it and 1ms would be the correct answer — Prometheus's histogram_quantile
// says the same. Five puts the rank inside the overflow bucket, which is what this test is about.
func TestPercentilesSurviveAnOverflowingLatency(t *testing.T) {
	t.Parallel()

	dpm := NewDetailedPerformanceMetrics(100, false)
	for range 95 {
		dpm.RecordOperation(OpRead, "", time.Millisecond, 1024, CacheSourceL1, nil)
	}
	for range 5 {
		dpm.RecordOperation(OpRead, "", 20*time.Minute, 1024, CacheSourceBackend, nil)
	}

	om := dpm.GetOperationMetrics(OpRead)
	if om == nil {
		t.Fatal("no metrics recorded for OpRead")
	}

	bounds := LatencyBucketBounds()
	top := bounds[len(bounds)-1]
	if om.P99Latency != top {
		t.Errorf("P99Latency = %v, want the top bound %v: the slowest 1%% overflowed the histogram, so the "+
			"honest estimate is \"at least this\"", om.P99Latency, top)
	}
	if om.P50Latency > 2*time.Millisecond {
		t.Errorf("P50Latency = %v, want ~1ms: one overflowing sample must not move the median", om.P50Latency)
	}
}

// TestHistogramCountsEveryOperation asserts the counts sum to Count.
//
// Cheap, and it catches a bucket function that returns an out-of-range index — which would panic — as
// well as a record path that buckets some operations and not others.
func TestHistogramCountsEveryOperation(t *testing.T) {
	t.Parallel()

	dpm := NewDetailedPerformanceMetrics(100, false)
	latencies := []time.Duration{
		0,
		15 * time.Microsecond,
		time.Millisecond,
		250 * time.Millisecond,
		50 * time.Millisecond,
		9 * time.Second,
		time.Hour,
	}
	for _, latency := range latencies {
		dpm.RecordOperation(OpList, "", latency, 0, CacheSourceBackend, nil)
	}

	om := dpm.GetOperationMetrics(OpList)
	if om == nil {
		t.Fatal("no metrics recorded for OpList")
	}

	total := int64(0)
	for _, count := range om.LatencyHistogram {
		total += count
	}
	if total != int64(len(latencies)) {
		t.Errorf("histogram counts sum to %d, want %d (Count = %d)", total, len(latencies), om.Count)
	}
	if om.Count != int64(len(latencies)) {
		t.Errorf("Count = %d, want %d", om.Count, len(latencies))
	}
}

// TestGetOperationMetricsCopiesTheHistogram guards the copy in GetOperationMetrics.
//
// The struct copy there is the only thing between a caller and the array RecordOperation increments, and
// copying a struct copies a slice header rather than its backing array — so without the explicit copy
// the doc comment's "return a copy to avoid race conditions" is false for exactly one field. Asserted by
// writing through the returned slice, which is also what a -race run would catch if the recorder ran
// concurrently.
func TestGetOperationMetricsCopiesTheHistogram(t *testing.T) {
	t.Parallel()

	dpm := NewDetailedPerformanceMetrics(100, false)
	dpm.RecordOperation(OpRead, "", 3*time.Millisecond, 1024, CacheSourceL1, nil)

	first := dpm.GetOperationMetrics(OpRead)
	bucket := latencyBucket(3 * time.Millisecond)
	first.LatencyHistogram[bucket] = 4242

	second := dpm.GetOperationMetrics(OpRead)
	if second.LatencyHistogram[bucket] != 1 {
		t.Fatalf("a caller's write reached the collector's histogram: bucket %d is now %d, want 1",
			bucket, second.LatencyHistogram[bucket])
	}
}

// TestPerFileMetricsCarryNoPercentiles pins what the per-file copies do, since they share the struct
// type but not the histogram.
//
// updateFileMetrics builds DetailedOperationMetrics without a histogram, so the percentile fields on a
// per-file entry stay zero. That is a deliberate limit rather than an oversight — a histogram per
// (file, operation) pair is 25 int64s times MaxTrackedFiles times 21 operations — and the assertion
// exists so that anyone who publishes GetTopFiles output learns it here rather than from a dashboard of
// zeros.
func TestPerFileMetricsCarryNoPercentiles(t *testing.T) {
	t.Parallel()

	dpm := NewDetailedPerformanceMetrics(10, true)
	for range 20 {
		dpm.RecordOperation(OpRead, "/genomics/sample.bam", 5*time.Millisecond, 1024, CacheSourceL1, nil)
	}

	files := dpm.GetTopFiles(1)
	if len(files) != 1 {
		t.Fatalf("GetTopFiles returned %d files, want 1", len(files))
	}

	// GetTopFiles does not copy Operations at all, so there is nothing to publish percentiles from — which
	// is the point. The aggregate metrics are where the percentiles live.
	if files[0].Operations != nil {
		t.Errorf("GetTopFiles copied Operations (%d entries); the percentiles in them are unpopulated by "+
			"design and would publish as zeros", len(files[0].Operations))
	}

	if om := dpm.GetOperationMetrics(OpRead); om == nil || om.P50Latency == 0 {
		t.Error("the aggregate P50Latency is unpopulated, so per-operation percentiles are not available " +
			"anywhere")
	}
}
