// Package analytics provides ML-based access pattern analysis and S3 tier recommendations.
package analytics

import (
	"math"
	"sort"
	"sync"
	"time"
)

// objectStats holds aggregated access statistics for a single object key.
type objectStats struct {
	key            string
	firstSeen      time.Time
	lastAccess     time.Time
	accessCount    int64
	totalBytesRead int64
	recentTimes    []time.Time // sliding window, capped at windowSize
	dayOfWeek      [7]int64    // access counts per weekday (0=Sunday)
	hourOfDay      [24]int64   // access counts per hour of day (UTC)
}

// FeatureVector holds the numeric features extracted for the tier classifier.
type FeatureVector struct {
	// Frequency: accesses per day over different windows.
	AccessRate30d float64 // accesses/day over last 30 days
	AccessRate7d  float64 // accesses/day over last 7 days
	AccessRate1d  float64 // accesses/day over last 24 h

	// Recency
	HoursSinceLastAccess float64
	DaysSinceFirstSeen   float64

	// Temporal pattern
	IntervalMeanHours float64 // mean inter-access interval in hours
	IntervalVariance  float64 // variance of inter-access intervals (hours²)
	PeakHourFraction  float64 // fraction of accesses in the top-4 hours of day
	PeakDayFraction   float64 // fraction of accesses in the top-2 weekdays

	// Volume
	AvgBytesPerAccess float64
}

// PatternAnalyzer tracks per-object access statistics and extracts feature vectors.
// It is safe for concurrent use.
type PatternAnalyzer struct {
	mu         sync.RWMutex
	objects    map[string]*objectStats
	windowSize int // max timestamps retained per object
}

// NewPatternAnalyzer creates a PatternAnalyzer retaining the last windowSize access
// timestamps per object.  A windowSize of 0 uses the default of 200.
func NewPatternAnalyzer(windowSize int) *PatternAnalyzer {
	if windowSize <= 0 {
		windowSize = 200
	}
	return &PatternAnalyzer{
		objects:    make(map[string]*objectStats),
		windowSize: windowSize,
	}
}

// RecordAccess records an access event for key at time t with bytesRead bytes transferred.
func (pa *PatternAnalyzer) RecordAccess(key string, t time.Time, bytesRead int64) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	obj, ok := pa.objects[key]
	if !ok {
		obj = &objectStats{key: key, firstSeen: t}
		pa.objects[key] = obj
	}

	obj.accessCount++
	obj.totalBytesRead += bytesRead
	obj.lastAccess = t

	// Sliding window of recent timestamps.
	obj.recentTimes = append(obj.recentTimes, t)
	if len(obj.recentTimes) > pa.windowSize {
		obj.recentTimes = obj.recentTimes[1:]
	}

	// Update temporal histograms.
	wd := int(t.UTC().Weekday())
	hr := t.UTC().Hour()
	obj.dayOfWeek[wd]++
	obj.hourOfDay[hr]++
}

// Features returns a FeatureVector for key based on access history observed up to now.
// The second return value is false when key has never been seen.
func (pa *PatternAnalyzer) Features(key string) (FeatureVector, bool) {
	pa.mu.RLock()
	defer pa.mu.RUnlock()

	obj, ok := pa.objects[key]
	if !ok {
		return FeatureVector{}, false
	}

	now := time.Now()
	return pa.computeFeatures(obj, now), true
}

// FeaturesAt is like Features but computes the vector relative to a caller-supplied now.
// Useful for deterministic tests.
func (pa *PatternAnalyzer) FeaturesAt(key string, now time.Time) (FeatureVector, bool) {
	pa.mu.RLock()
	defer pa.mu.RUnlock()

	obj, ok := pa.objects[key]
	if !ok {
		return FeatureVector{}, false
	}
	return pa.computeFeatures(obj, now), true
}

// ObjectCount returns the number of distinct tracked keys.
func (pa *PatternAnalyzer) ObjectCount() int {
	pa.mu.RLock()
	defer pa.mu.RUnlock()
	return len(pa.objects)
}

// computeFeatures derives a FeatureVector from obj relative to now.
// Caller must hold at least a read lock.
func (pa *PatternAnalyzer) computeFeatures(obj *objectStats, now time.Time) FeatureVector {
	var fv FeatureVector

	// --- recency ---
	fv.HoursSinceLastAccess = now.Sub(obj.lastAccess).Hours()
	fv.DaysSinceFirstSeen = now.Sub(obj.firstSeen).Hours() / 24.0

	// --- access rates from the sliding window ---
	last1d := now.Add(-24 * time.Hour)
	last7d := now.Add(-7 * 24 * time.Hour)
	last30d := now.Add(-30 * 24 * time.Hour)

	var n1d, n7d, n30d int
	for _, t := range obj.recentTimes {
		if !t.Before(last30d) {
			n30d++
		}
		if !t.Before(last7d) {
			n7d++
		}
		if !t.Before(last1d) {
			n1d++
		}
	}
	fv.AccessRate30d = float64(n30d) / 30.0
	fv.AccessRate7d = float64(n7d) / 7.0
	fv.AccessRate1d = float64(n1d)

	// --- inter-access interval statistics ---
	if len(obj.recentTimes) >= 2 {
		sorted := make([]time.Time, len(obj.recentTimes))
		copy(sorted, obj.recentTimes)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })

		intervals := make([]float64, len(sorted)-1)
		for i := 1; i < len(sorted); i++ {
			intervals[i-1] = sorted[i].Sub(sorted[i-1]).Hours()
		}

		sum := 0.0
		for _, v := range intervals {
			sum += v
		}
		mean := sum / float64(len(intervals))
		fv.IntervalMeanHours = mean

		varSum := 0.0
		for _, v := range intervals {
			d := v - mean
			varSum += d * d
		}
		fv.IntervalVariance = varSum / float64(len(intervals))
	}

	// --- peak-hour and peak-day fractions ---
	fv.PeakHourFraction = topKFraction(obj.hourOfDay[:], 4)
	fv.PeakDayFraction = topKFraction(obj.dayOfWeek[:], 2)

	// --- volume ---
	if obj.accessCount > 0 {
		fv.AvgBytesPerAccess = float64(obj.totalBytesRead) / float64(obj.accessCount)
	}

	return fv
}

// topKFraction returns the fraction of total counts accounted for by the top-k slots.
// It returns 0 when the total is zero.
func topKFraction(counts []int64, k int) float64 {
	if k <= 0 {
		return 0
	}
	// Copy so we can sort without mutating.
	tmp := make([]int64, len(counts))
	copy(tmp, counts)
	sort.Slice(tmp, func(i, j int) bool { return tmp[i] > tmp[j] })

	var topSum, total int64
	for i, v := range tmp {
		total += v
		if i < k {
			topSum += v
		}
	}
	if total == 0 {
		return 0
	}
	return math.Min(1.0, float64(topSum)/float64(total))
}
