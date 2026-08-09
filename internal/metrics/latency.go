package metrics

import (
	"sort"
	"time"
)

// Latency bucketing for [DetailedOperationMetrics.LatencyHistogram].
//
// The range that has to be covered is not a matter of taste: an L1 cache hit is tens of microseconds
// and a cold multipart GET is seconds, five orders of magnitude apart, so no linear scheme resolves
// both ends. Exponential buckets from 25µs with a factor of 2 put ~11 buckets under a millisecond,
// where cache hits live, and still reach 209s before overflowing.
//
// The bound this replaced was `int(latency.Milliseconds()) % 100` over 100 buckets, which is a hash
// rather than a bucketing: 1ms, 101ms and 201ms all incremented bucket 1, and truncation to whole
// milliseconds put every sub-millisecond operation — the expected case for a cache hit — in bucket 0
// alongside everything at exactly 100ms. An array indexed that way cannot answer "what fraction of
// operations were under N" for any N, which is why the percentiles it was allocated to serve were
// never computed from it (#222).
const (
	firstLatencyBucket = 25 * time.Microsecond
	latencyBucketCount = 24 // 25µs · 2^23 ≈ 209s at the top
)

// latencyBucketBounds are inclusive upper bounds: bucket i holds latencies in
// (latencyBucketBounds[i-1], latencyBucketBounds[i]], and bucket 0 starts at zero.
var latencyBucketBounds = func() []time.Duration {
	bounds := make([]time.Duration, latencyBucketCount)
	bound := firstLatencyBucket
	for i := range bounds {
		bounds[i] = bound
		bound *= 2
	}
	return bounds
}()

// LatencyBucketBounds returns the inclusive upper bound of each finite latency bucket, in order.
//
// Exported because a count is meaningless without the interval it counts, and
// [DetailedOperationMetrics.LatencyHistogram] carries only counts. The boundaries are not a field on
// the struct: the histogram itself is `json:"-"`, so serializing 24 constants next to counts that are
// not serialized would tell a JSON reader about numbers they cannot see. Anything that reads the
// histogram reads it in Go, where this function is reachable.
//
// The returned slice has one element fewer than the histogram. The extra, final histogram entry is the
// overflow bucket: everything slower than the last bound.
func LatencyBucketBounds() []time.Duration {
	return append([]time.Duration(nil), latencyBucketBounds...)
}

// newLatencyHistogram allocates the per-operation histogram, sized for the finite buckets plus one
// overflow bucket.
func newLatencyHistogram() []int64 {
	return make([]int64, len(latencyBucketBounds)+1)
}

// latencyBucket returns the histogram index a latency belongs in.
//
// A latency above every bound gets len(latencyBucketBounds) — the overflow bucket — so a 5-minute
// operation is recorded as "slower than 209s" rather than wrapping around to a fast bucket.
func latencyBucket(latency time.Duration) int {
	return sort.Search(len(latencyBucketBounds), func(i int) bool {
		return latency <= latencyBucketBounds[i]
	})
}

// updatePercentiles recomputes P50/P95/P99 from the histogram.
//
// Called on every record rather than on read. On read would be cheaper, but [DetailedPerformanceMetrics]
// serializes these structs directly through its own `operation_metrics` JSON tag, so anything that
// marshals the collector without going through [DetailedPerformanceMetrics.GetOperationMetrics] would
// publish zeros — which is the defect being fixed here, not a smaller version of it. Zero-valued
// percentiles alongside a populated average_latency read as a filesystem with no tail latency at all,
// the most flattering possible wrong answer.
//
// Recomputing costs four passes over 25 buckets. That is not the expensive part of recording an
// operation that just did I/O.
func updatePercentiles(om *DetailedOperationMetrics) {
	if len(om.LatencyHistogram) == 0 {
		return
	}

	total := int64(0)
	for _, count := range om.LatencyHistogram {
		total += count
	}
	if total == 0 {
		return
	}

	om.P50Latency = histogramQuantile(om.LatencyHistogram, total, 0.50)
	om.P95Latency = histogramQuantile(om.LatencyHistogram, total, 0.95)
	om.P99Latency = histogramQuantile(om.LatencyHistogram, total, 0.99)
}

// histogramQuantile estimates the q-quantile of the latencies counted in hist, whose counts sum to
// total.
//
// Within the bucket that covers the requested rank it interpolates linearly between the bucket's
// bounds, which is what Prometheus's histogram_quantile does and is the best an aggregate of counts
// supports — the individual latencies are gone. The result is therefore an estimate bounded by the
// covering bucket, and the bucket widths above are what set that error.
//
// A rank that falls in the overflow bucket returns the last finite bound, again as Prometheus does:
// there is no upper bound to interpolate toward, so the honest answer is "at least this".
func histogramQuantile(hist []int64, total int64, q float64) time.Duration {
	rank := q * float64(total)

	cumulative := int64(0)
	for i, count := range hist {
		if count == 0 {
			continue
		}

		previous := cumulative
		cumulative += count
		if float64(cumulative) < rank {
			continue
		}

		if i >= len(latencyBucketBounds) {
			return latencyBucketBounds[len(latencyBucketBounds)-1]
		}

		lower := time.Duration(0)
		if i > 0 {
			lower = latencyBucketBounds[i-1]
		}
		upper := latencyBucketBounds[i]

		position := (rank - float64(previous)) / float64(count)
		return lower + time.Duration(position*float64(upper-lower))
	}

	// Unreachable while total is the sum of hist: the loop must have covered every count by the time it
	// ends, and rank is at most total. Returning the top bound rather than zero keeps a future caller
	// that passes a mismatched total from publishing a fast percentile it did not measure.
	return latencyBucketBounds[len(latencyBucketBounds)-1]
}
