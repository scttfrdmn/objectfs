package analytics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPatternAnalyzer_DefaultWindow(t *testing.T) {
	t.Parallel()
	pa := NewPatternAnalyzer(0) // 0 → default
	assert.Equal(t, 200, pa.windowSize)
}

func TestPatternAnalyzer_ObjectCount(t *testing.T) {
	t.Parallel()
	pa := NewPatternAnalyzer(10)
	now := time.Now()
	assert.Equal(t, 0, pa.ObjectCount())

	pa.RecordAccess("a", now, 0)
	pa.RecordAccess("b", now, 0)
	pa.RecordAccess("a", now.Add(time.Hour), 0) // duplicate key
	assert.Equal(t, 2, pa.ObjectCount())
}

func TestPatternAnalyzer_Features_UnknownKey(t *testing.T) {
	t.Parallel()
	pa := NewPatternAnalyzer(10)
	_, ok := pa.Features("never-seen")
	assert.False(t, ok)
}

func TestPatternAnalyzer_Features_RecencyAndAvgBytes(t *testing.T) {
	t.Parallel()
	pa := NewPatternAnalyzer(50)
	anchor := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	pa.RecordAccess("k", anchor, 1000)
	pa.RecordAccess("k", anchor.Add(time.Hour), 2000)

	now := anchor.Add(2 * time.Hour)
	fv, ok := pa.FeaturesAt("k", now)
	require.True(t, ok)

	assert.InDelta(t, 1.0, fv.HoursSinceLastAccess, 0.01)
	assert.InDelta(t, 1500.0, fv.AvgBytesPerAccess, 0.01)
	// DaysSinceFirstSeen: 2h = 2/24 days
	assert.InDelta(t, 2.0/24.0, fv.DaysSinceFirstSeen, 0.01)
}

func TestPatternAnalyzer_Features_AccessRates(t *testing.T) {
	t.Parallel()
	pa := NewPatternAnalyzer(500)
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Simulate 30 daily accesses (one per day for 30 days).
	for i := range 30 {
		pa.RecordAccess("daily", anchor.Add(time.Duration(i)*24*time.Hour), 0)
	}

	now := anchor.Add(30 * 24 * time.Hour) // just after the last access
	fv, ok := pa.FeaturesAt("daily", now)
	require.True(t, ok)

	// All 30 accesses fall within last 30 days → 30/30 = 1.0
	assert.InDelta(t, 1.0, fv.AccessRate30d, 0.05)
	// Only last 7 accesses fall in last 7 days → 7/7 = 1.0
	assert.InDelta(t, 1.0, fv.AccessRate7d, 0.05)
}

func TestPatternAnalyzer_Features_AccessRate1d(t *testing.T) {
	t.Parallel()
	pa := NewPatternAnalyzer(50)
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		pa.RecordAccess("k", anchor.Add(time.Duration(i)*time.Hour), 0)
	}
	now := anchor.Add(6 * time.Hour)
	fv, ok := pa.FeaturesAt("k", now)
	require.True(t, ok)
	assert.InDelta(t, 5.0, fv.AccessRate1d, 0.1) // 5 accesses in last 24h
}

func TestPatternAnalyzer_Features_IntervalStats(t *testing.T) {
	t.Parallel()
	pa := NewPatternAnalyzer(50)
	anchor := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 4 accesses, 2h apart → mean interval = 2h, variance = 0.
	for i := range 4 {
		pa.RecordAccess("k", anchor.Add(time.Duration(i)*2*time.Hour), 0)
	}
	now := anchor.Add(8 * time.Hour)
	fv, ok := pa.FeaturesAt("k", now)
	require.True(t, ok)
	assert.InDelta(t, 2.0, fv.IntervalMeanHours, 0.01)
	assert.InDelta(t, 0.0, fv.IntervalVariance, 0.001)
}

func TestPatternAnalyzer_Features_SlidingWindowCap(t *testing.T) {
	t.Parallel()
	// Window of 5: only the last 5 timestamps should be retained.
	pa := NewPatternAnalyzer(5)
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 10 {
		pa.RecordAccess("k", anchor.Add(time.Duration(i)*24*time.Hour), 0)
	}
	pa.mu.RLock()
	obj := pa.objects["k"]
	n := len(obj.recentTimes)
	pa.mu.RUnlock()
	assert.Equal(t, 5, n)
}

func TestPatternAnalyzer_Features_PeakHourFraction(t *testing.T) {
	t.Parallel()
	pa := NewPatternAnalyzer(200)
	anchor := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC) // 09:00 UTC
	for range 8 {
		pa.RecordAccess("k", anchor, 0) // all accesses at hour 9
	}
	now := anchor.Add(time.Hour)
	fv, ok := pa.FeaturesAt("k", now)
	require.True(t, ok)
	// All 8 accesses at hour 9 → top-4 fraction should be 1.0.
	assert.InDelta(t, 1.0, fv.PeakHourFraction, 0.01)
}

func TestTopKFraction_Zero(t *testing.T) {
	t.Parallel()
	counts := make([]int64, 7)
	assert.Equal(t, 0.0, topKFraction(counts, 2))
}
