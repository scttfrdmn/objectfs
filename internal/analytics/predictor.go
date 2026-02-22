package analytics

import (
	"sync/atomic"
	"time"
)

// Stats holds aggregate statistics for the Predictor.
type Stats struct {
	// TotalObjects is the number of distinct keys tracked.
	TotalObjects int64

	// TotalAccesses is the total number of RecordAccess calls.
	TotalAccesses int64

	// TotalRecommendations is the total number of Recommend/RecommendBatch calls.
	TotalRecommendations int64

	// RecommendationsByTier is a count of recommendations issued per tier.
	// The map is always non-nil and contains entries for all five tier constants.
	RecommendationsByTier map[string]int64
}

// Predictor combines a PatternAnalyzer and a TierClassifier into a single facade.
// It is safe for concurrent use.
type Predictor struct {
	analyzer   *PatternAnalyzer
	classifier *TierClassifier

	totalAccesses    atomic.Int64
	totalRecommended atomic.Int64
	recsByTier       [5]atomic.Int64 // indexed by tierIndex
}

// tierOrder maps tier strings to a stable index for the per-tier atomic counters.
var tierOrder = []string{
	TierStandard,
	TierStandardIA,
	TierGlacierIR,
	TierGlacier,
	TierDeepArchive,
}

func tierIndex(tier string) int {
	for i, t := range tierOrder {
		if t == tier {
			return i
		}
	}
	return 0 // fallback to Standard
}

// Option configures a Predictor.
type Option func(*Predictor)

// WithWindowSize sets the number of access timestamps retained per object.
// The default is 200.
func WithWindowSize(n int) Option {
	return func(p *Predictor) {
		p.analyzer = NewPatternAnalyzer(n)
	}
}

// NewPredictor creates a Predictor with default settings, optionally modified by opts.
func NewPredictor(opts ...Option) *Predictor {
	p := &Predictor{
		analyzer:   NewPatternAnalyzer(0), // 0 → default window 200
		classifier: NewTierClassifier(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// RecordAccess records an access event for key with bytesRead bytes, timestamped now.
func (p *Predictor) RecordAccess(key string, bytesRead int64) {
	p.RecordAccessAt(key, time.Now(), bytesRead)
}

// RecordAccessAt records an access event at an explicit time t.
// This is provided for deterministic testing and backfilling historical data.
func (p *Predictor) RecordAccessAt(key string, t time.Time, bytesRead int64) {
	p.analyzer.RecordAccess(key, t, bytesRead)
	p.totalAccesses.Add(1)
}

// Recommend returns a tier recommendation for key.
// For a key with no access history, it returns a STANDARD recommendation with
// confidence 0 and reason "no access history".
func (p *Predictor) Recommend(key string) Recommendation {
	p.totalRecommended.Add(1)

	fv, ok := p.analyzer.FeaturesAt(key, time.Now())
	if !ok {
		rec := Recommendation{
			Key:        key,
			Tier:       TierStandard,
			Confidence: 0,
			Reason:     "no access history",
		}
		p.recsByTier[tierIndex(rec.Tier)].Add(1)
		return rec
	}

	rec := p.classifier.Classify(key, fv)
	p.recsByTier[tierIndex(rec.Tier)].Add(1)
	return rec
}

// RecommendBatch returns tier recommendations for multiple keys.
// Keys not yet seen return a STANDARD recommendation with confidence 0.
func (p *Predictor) RecommendBatch(keys []string) []Recommendation {
	now := time.Now()
	recs := make([]Recommendation, len(keys))
	p.totalRecommended.Add(int64(len(keys)))

	for i, key := range keys {
		fv, ok := p.analyzer.FeaturesAt(key, now)
		if !ok {
			recs[i] = Recommendation{
				Key:        key,
				Tier:       TierStandard,
				Confidence: 0,
				Reason:     "no access history",
			}
			p.recsByTier[tierIndex(TierStandard)].Add(1)
			continue
		}
		rec := p.classifier.Classify(key, fv)
		p.recsByTier[tierIndex(rec.Tier)].Add(1)
		recs[i] = rec
	}
	return recs
}

// Stats returns a snapshot of aggregate predictor statistics.
func (p *Predictor) Stats() Stats {
	byTier := make(map[string]int64, len(tierOrder))
	for i, tier := range tierOrder {
		byTier[tier] = p.recsByTier[i].Load()
	}
	return Stats{
		TotalObjects:          int64(p.analyzer.ObjectCount()),
		TotalAccesses:         p.totalAccesses.Load(),
		TotalRecommendations:  p.totalRecommended.Load(),
		RecommendationsByTier: byTier,
	}
}
