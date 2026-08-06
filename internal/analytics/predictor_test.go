package analytics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPredictor_Defaults(t *testing.T) {
	t.Parallel()
	p := NewPredictor()
	assert.NotNil(t, p)
	assert.Equal(t, 200, p.analyzer.windowSize)
}

func TestWithWindowSize(t *testing.T) {
	t.Parallel()
	p := NewPredictor(WithWindowSize(50))
	assert.Equal(t, 50, p.analyzer.windowSize)
}

func TestPredictor_Recommend_NoHistory(t *testing.T) {
	t.Parallel()
	p := NewPredictor()
	rec := p.Recommend("unseen-key")
	assert.Equal(t, TierStandard, rec.Tier)
	assert.Zero(t, rec.Confidence)
	assert.Equal(t, "no access history", rec.Reason)
}

func TestPredictor_RecordAndRecommend_Hot(t *testing.T) {
	t.Parallel()
	p := NewPredictor()
	now := time.Now()
	// 10 accesses in the last hour → very hot.
	for i := range 10 {
		p.RecordAccessAt("hot-key", now.Add(-time.Duration(i)*5*time.Minute), 4096)
	}
	rec := p.Recommend("hot-key")
	assert.Equal(t, TierStandard, rec.Tier)
	assert.Greater(t, rec.Confidence, 0.5)
}

func TestPredictor_RecordAndRecommend_Cold(t *testing.T) {
	t.Parallel()
	p := NewPredictor()
	anchor := time.Now().Add(-100 * 24 * time.Hour) // 100 days ago
	p.RecordAccessAt("cold-key", anchor, 1024)

	rec := p.Recommend("cold-key")
	assert.Equal(t, TierDeepArchive, rec.Tier)
	assert.Greater(t, rec.Confidence, 0.5)
	assert.Greater(t, rec.MonthlySavingsPerGB, 0.0)
}

func TestPredictor_RecommendBatch(t *testing.T) {
	t.Parallel()
	p := NewPredictor()
	now := time.Now()
	p.RecordAccessAt("hot", now.Add(-1*time.Hour), 0)
	p.RecordAccessAt("cold", now.Add(-2000*time.Hour), 0)

	recs := p.RecommendBatch([]string{"hot", "cold", "unseen"})
	require.Len(t, recs, 3)
	assert.Equal(t, "hot", recs[0].Key)
	assert.Equal(t, TierStandard, recs[0].Tier)
	assert.Equal(t, "cold", recs[1].Key)
	assert.NotEqual(t, TierStandard, recs[1].Tier)
	assert.Equal(t, "unseen", recs[2].Key)
	assert.Equal(t, TierStandard, recs[2].Tier) // default for no-history
}

func TestPredictor_Stats_Counters(t *testing.T) {
	t.Parallel()
	p := NewPredictor()
	now := time.Now()
	p.RecordAccessAt("a", now, 0)
	p.RecordAccessAt("a", now.Add(time.Hour), 0)
	p.RecordAccessAt("b", now, 0)

	_ = p.Recommend("a")
	_ = p.Recommend("b")
	_ = p.Recommend("unseen")

	s := p.Stats()
	assert.Equal(t, int64(2), s.TotalObjects)
	assert.Equal(t, int64(3), s.TotalAccesses)
	assert.Equal(t, int64(3), s.TotalRecommendations)
	// "a" and "b" are hot (accessed recently), "unseen" defaults to STANDARD.
	assert.Equal(t, int64(3), s.RecommendationsByTier[TierStandard])
}

func TestPredictor_Stats_BatchCounted(t *testing.T) {
	t.Parallel()
	p := NewPredictor()
	_ = p.RecommendBatch([]string{"x", "y", "z"})
	assert.Equal(t, int64(3), p.Stats().TotalRecommendations)
}

func TestPredictor_Stats_AllTiersInitialized(t *testing.T) {
	t.Parallel()
	p := NewPredictor()
	s := p.Stats()
	for _, tier := range []string{TierStandard, TierStandardIA, TierGlacierIR, TierGlacier, TierDeepArchive} {
		_, ok := s.RecommendationsByTier[tier]
		assert.True(t, ok, "Stats().RecommendationsByTier should contain %s", tier)
	}
}

func TestPredictor_RecordAccess_UsesNow(t *testing.T) {
	t.Parallel()
	p := NewPredictor()
	// RecordAccess (no explicit time) should not panic and should be tracked.
	p.RecordAccess("k", 100)
	assert.Equal(t, int64(1), p.Stats().TotalAccesses)
}
