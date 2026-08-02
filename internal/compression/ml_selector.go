package compression

import (
	"fmt"
	"sync"
	"time"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

// outcomeRecord stores the observed result of a single compression operation.
type outcomeRecord struct {
	algorithm comprpkg.Algorithm
	ratio     float64       // compressed / original; < 1.0 is good
	elapsed   time.Duration // time to compress
}

// AdaptiveSelector wraps a base Selector and refines its recommendations
// using a rolling window of observed compression outcomes per ContentClass.
//
// The adaptive logic is a simple online multi-armed-bandit approach: for each
// ContentClass it tracks the mean compression ratio and mean elapsed time per
// algorithm.  Once windowSize outcomes have been collected it replaces the base
// recommendation with the algorithm that achieves the best trade-off for the
// given AccessHint:
//   - Hot: minimize elapsed time
//   - Cold / Warm / Unknown: minimize compression ratio (= maximize savings)
//
// RecordOutcome must be called after each compress operation to feed results
// back into the model.  AdaptiveSelector is safe for concurrent use.
type AdaptiveSelector struct {
	base       Selector
	windowSize int

	mu      sync.RWMutex
	records map[ContentClass][]outcomeRecord
}

// NewAdaptiveSelector wraps base with an adaptive learning layer.
// windowSize is the maximum number of outcomes retained per ContentClass;
// older records are evicted FIFO.  Reasonable values are 50–200.
func NewAdaptiveSelector(base Selector, windowSize int) *AdaptiveSelector {
	if windowSize <= 0 {
		windowSize = 100
	}
	return &AdaptiveSelector{
		base:       base,
		windowSize: windowSize,
		records:    make(map[ContentClass][]outcomeRecord),
	}
}

// Select returns a recommendation.  If the adaptive model has enough data for
// the given ContentClass it overrides the base recommendation; otherwise it
// falls through to the base Selector.
func (s *AdaptiveSelector) Select(analysis Analysis, hint AccessHint, size int64) Recommendation {
	base := s.base.Select(analysis, hint, size)

	// Never override a "none" recommendation: if the data is incompressible
	// the base rule is authoritative.
	if base.Algorithm == comprpkg.AlgorithmNone {
		return base
	}

	s.mu.RLock()
	records := s.records[analysis.ContentClass]
	s.mu.RUnlock()

	best, ok := s.bestAlgorithm(records, hint)
	if !ok {
		// Not enough data yet — fall through to base.
		return base
	}

	if best == base.Algorithm {
		return base
	}

	return Recommendation{
		Algorithm: best,
		Level:     base.Level,
		Reason: fmt.Sprintf("adaptive: learned %s outperforms %s for %s content (%d samples)",
			best, base.Algorithm, analysis.ContentClass, len(records)),
	}
}

// RecordOutcome feeds a compression result back into the adaptive model.
//
//   - class: the ContentClass returned by Analyze
//   - algo: the algorithm that was used
//   - stats: the Stats snapshot from Compressor.Stats
//   - elapsed: wall-clock time of the Compress call
func (s *AdaptiveSelector) RecordOutcome(
	class ContentClass,
	algo comprpkg.Algorithm,
	stats comprpkg.Stats,
	elapsed time.Duration,
) {
	if stats.OriginalSize == 0 {
		return
	}
	rec := outcomeRecord{
		algorithm: algo,
		ratio:     float64(stats.CompressedSize) / float64(stats.OriginalSize),
		elapsed:   elapsed,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	buf := s.records[class]
	buf = append(buf, rec)
	if len(buf) > s.windowSize {
		buf = buf[len(buf)-s.windowSize:]
	}
	s.records[class] = buf
}

// Stats returns the number of outcome records held per ContentClass.
// Useful for monitoring and testing.
func (s *AdaptiveSelector) Stats() map[ContentClass]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[ContentClass]int, len(s.records))
	for k, v := range s.records {
		out[k] = len(v)
	}
	return out
}

// minSamples is the minimum number of outcomes required per algorithm before
// the adaptive model considers its data reliable.
const minSamples = 10

// bestAlgorithm scans records and returns the algorithm with the best
// observed performance for the given hint.  Returns (algo, true) when
// enough data is available, or ("", false) when it is not.
func (s *AdaptiveSelector) bestAlgorithm(records []outcomeRecord, hint AccessHint) (comprpkg.Algorithm, bool) {
	type stats struct {
		totalRatio   float64
		totalElapsed time.Duration
		count        int
	}
	m := make(map[comprpkg.Algorithm]*stats)

	for i := range records {
		r := &records[i]
		if m[r.algorithm] == nil {
			m[r.algorithm] = &stats{}
		}
		m[r.algorithm].totalRatio += r.ratio
		m[r.algorithm].totalElapsed += r.elapsed
		m[r.algorithm].count++
	}

	// Require at least minSamples per algorithm to make a decision.
	for _, st := range m {
		if st.count < minSamples {
			return "", false
		}
	}
	if len(m) == 0 {
		return "", false
	}

	var best comprpkg.Algorithm
	bestScore := -1.0

	for algo, st := range m {
		var score float64
		if hint == AccessHintHot {
			// Lower elapsed time is better; invert so higher score = faster.
			avgMs := float64(st.totalElapsed/time.Millisecond) / float64(st.count)
			if avgMs == 0 {
				avgMs = 0.001
			}
			score = 1.0 / avgMs
		} else {
			// Lower ratio is better (more bytes saved); invert for scoring.
			avgRatio := st.totalRatio / float64(st.count)
			if avgRatio == 0 {
				avgRatio = 0.001
			}
			score = 1.0 / avgRatio
		}
		if score > bestScore {
			bestScore = score
			best = algo
		}
	}

	return best, best != ""
}
