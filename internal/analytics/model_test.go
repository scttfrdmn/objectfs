package analytics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func newClassifier(t *testing.T) *TierClassifier {
	t.Helper()
	return NewTierClassifier()
}

// hotFV is a feature vector representative of a hot, frequently accessed object.
func hotFV() FeatureVector {
	return FeatureVector{
		HoursSinceLastAccess: 1.0, // 1 hour ago
		AccessRate7d:         3.0, // 3 accesses/day this week
		AccessRate30d:        2.0, // 2 accesses/day this month
	}
}

// warmFV is a feature vector for a moderately accessed object.
func warmFV() FeatureVector {
	return FeatureVector{
		HoursSinceLastAccess: 48.0, // 2 days ago
		AccessRate7d:         0.5,  // < 1/day
		AccessRate30d:        0.8,  // > 0.5/day — warm
	}
}

// coolFV is an object accessed occasionally but not in the past 30 days.
func coolFV() FeatureVector {
	return FeatureVector{
		HoursSinceLastAccess: 500.0, // ~21 days
		AccessRate7d:         0.0,
		AccessRate30d:        0.15, // ≥ 0.1/day — goes to GLACIER_IR
	}
}

// coldFV is an object accessed rarely within the last 30 days.
func coldFV() FeatureVector {
	return FeatureVector{
		HoursSinceLastAccess: 600.0, // ~25 days
		AccessRate7d:         0.0,
		AccessRate30d:        0.05, // < 0.1/day → GLACIER
	}
}

// archiveFV is an object not accessed for 90+ days.
func archiveFV() FeatureVector {
	return FeatureVector{
		HoursSinceLastAccess: 3000.0, // ~125 days
		AccessRate7d:         0.0,
		AccessRate30d:        0.0,
	}
}

// between30and90FV: last access 50 days ago.
func between30and90FV() FeatureVector {
	return FeatureVector{
		HoursSinceLastAccess: 1200.0, // 50 days
		AccessRate7d:         0.0,
		AccessRate30d:        0.0,
	}
}

func TestClassify_HotObject_AccessedToday(t *testing.T) {
	t.Parallel()
	tc := newClassifier(t)
	rec := tc.Classify("k", hotFV())
	assert.Equal(t, TierStandard, rec.Tier)
	assert.InDelta(t, 0.95, rec.Confidence, 0.001)
	assert.Equal(t, float64(0), rec.MonthlySavingsPerGB)
}

func TestClassify_HotObject_HighWeeklyRate(t *testing.T) {
	t.Parallel()
	tc := newClassifier(t)
	fv := FeatureVector{
		HoursSinceLastAccess: 30.0, // > 24h but active this week
		AccessRate7d:         2.5,
	}
	rec := tc.Classify("k", fv)
	assert.Equal(t, TierStandard, rec.Tier)
	assert.InDelta(t, 0.90, rec.Confidence, 0.001)
}

func TestClassify_WarmObject(t *testing.T) {
	t.Parallel()
	tc := newClassifier(t)
	rec := tc.Classify("warm", warmFV())
	assert.Equal(t, TierStandardIA, rec.Tier)
	assert.InDelta(t, 0.85, rec.Confidence, 0.001)
	// Savings should be Standard - StandardIA = 0.023 - 0.0125
	assert.InDelta(t, 0.0105, rec.MonthlySavingsPerGB, 0.0001)
}

func TestClassify_CoolObject_GlacierIR(t *testing.T) {
	t.Parallel()
	tc := newClassifier(t)
	rec := tc.Classify("cool", coolFV())
	assert.Equal(t, TierGlacierIR, rec.Tier)
	assert.InDelta(t, 0.82, rec.Confidence, 0.001)
}

func TestClassify_ColdObject_Glacier(t *testing.T) {
	t.Parallel()
	tc := newClassifier(t)
	rec := tc.Classify("cold", coldFV())
	assert.Equal(t, TierGlacier, rec.Tier)
}

func TestClassify_Between30And90Days_Glacier(t *testing.T) {
	t.Parallel()
	tc := newClassifier(t)
	rec := tc.Classify("mid", between30and90FV())
	assert.Equal(t, TierGlacier, rec.Tier)
	assert.InDelta(t, 0.80, rec.Confidence, 0.001)
}

func TestClassify_ArchiveObject(t *testing.T) {
	t.Parallel()
	tc := newClassifier(t)
	rec := tc.Classify("arch", archiveFV())
	assert.Equal(t, TierDeepArchive, rec.Tier)
	assert.InDelta(t, 0.88, rec.Confidence, 0.001)
	// Savings ≈ 0.023 - 0.00099
	assert.InDelta(t, 0.02201, rec.MonthlySavingsPerGB, 0.0001)
}

func TestClassify_ReasonIsSet(t *testing.T) {
	t.Parallel()
	tc := newClassifier(t)
	for _, fv := range []FeatureVector{hotFV(), warmFV(), coolFV(), coldFV(), archiveFV()} {
		rec := tc.Classify("k", fv)
		assert.NotEmpty(t, rec.Reason, "tier=%s should have a reason", rec.Tier)
	}
}

func TestClassify_ConfidenceInRange(t *testing.T) {
	t.Parallel()
	tc := newClassifier(t)
	tests := []FeatureVector{hotFV(), warmFV(), coolFV(), coldFV(), archiveFV(), between30and90FV()}
	for _, fv := range tests {
		rec := tc.Classify("k", fv)
		assert.GreaterOrEqual(t, rec.Confidence, 0.0)
		assert.LessOrEqual(t, rec.Confidence, 1.0)
		assert.GreaterOrEqual(t, rec.MonthlySavingsPerGB, 0.0)
	}
}

func TestClassify_KeyPropagated(t *testing.T) {
	t.Parallel()
	tc := newClassifier(t)
	rec := tc.Classify("my-object-key", hotFV())
	assert.Equal(t, "my-object-key", rec.Key)
}
