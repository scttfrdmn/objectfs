package compression

import (
	"testing"
	"time"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

func TestAdaptiveSelector_FallsThroughToBase(t *testing.T) {
	t.Parallel()
	base := NewRuleSelector()
	sel := NewAdaptiveSelector(base, 50)
	a := Analysis{ContentClass: ContentClassText, CompressScore: 0.7, Entropy: 3.0}

	// No outcomes recorded yet — should use base recommendation.
	rec := sel.Select(a, AccessHintUnknown, 1024)
	baseRec := base.Select(a, AccessHintUnknown, 1024)
	if rec.Algorithm != baseRec.Algorithm {
		t.Errorf("before learning: algorithm = %q, want base %q", rec.Algorithm, baseRec.Algorithm)
	}
}

func TestAdaptiveSelector_IncompatibleNotOverridden(t *testing.T) {
	t.Parallel()
	base := NewRuleSelector()
	sel := NewAdaptiveSelector(base, 50)

	// Base returns "none" for compressed content — adaptive must not override.
	a := Analysis{ContentClass: ContentClassCompressed, CompressScore: 0.02}
	rec := sel.Select(a, AccessHintUnknown, 1024)
	if rec.Algorithm != comprpkg.AlgorithmNone {
		t.Errorf("algorithm = %q, want none (base should not be overridden)", rec.Algorithm)
	}
}

func TestAdaptiveSelector_LearnsFasterAlgorithm(t *testing.T) {
	t.Parallel()
	base := NewRuleSelector()
	sel := NewAdaptiveSelector(base, 200)

	// Feed minSamples outcomes showing LZ4 is much faster for hot text access.
	lz4Stats := comprpkg.Stats{
		Algorithm:      comprpkg.AlgorithmLZ4,
		OriginalSize:   1000,
		CompressedSize: 600,
	}
	zstdStats := comprpkg.Stats{
		Algorithm:      comprpkg.AlgorithmZstd,
		OriginalSize:   1000,
		CompressedSize: 500,
	}

	for range minSamples {
		sel.RecordOutcome(ContentClassText, comprpkg.AlgorithmLZ4, lz4Stats, 1*time.Millisecond)
		sel.RecordOutcome(ContentClassText, comprpkg.AlgorithmZstd, zstdStats, 50*time.Millisecond)
	}

	a := Analysis{ContentClass: ContentClassText, CompressScore: 0.7}
	rec := sel.Select(a, AccessHintHot, 1024)
	if rec.Algorithm != comprpkg.AlgorithmLZ4 {
		t.Errorf("hot access after learning: algorithm = %q, want lz4", rec.Algorithm)
	}
}

func TestAdaptiveSelector_LearnsLowerRatio(t *testing.T) {
	t.Parallel()
	base := NewRuleSelector()
	sel := NewAdaptiveSelector(base, 200)

	// Feed minSamples outcomes showing ZSTD achieves lower ratio for cold binary.
	lz4Stats := comprpkg.Stats{
		Algorithm:      comprpkg.AlgorithmLZ4,
		OriginalSize:   1000,
		CompressedSize: 700, // 0.7 ratio
	}
	zstdStats := comprpkg.Stats{
		Algorithm:      comprpkg.AlgorithmZstd,
		OriginalSize:   1000,
		CompressedSize: 450, // 0.45 ratio — better
	}

	for range minSamples {
		sel.RecordOutcome(ContentClassBinary, comprpkg.AlgorithmLZ4, lz4Stats, 5*time.Millisecond)
		sel.RecordOutcome(ContentClassBinary, comprpkg.AlgorithmZstd, zstdStats, 30*time.Millisecond)
	}

	a := Analysis{ContentClass: ContentClassBinary, CompressScore: 0.5}
	rec := sel.Select(a, AccessHintCold, 1024)
	if rec.Algorithm != comprpkg.AlgorithmZstd {
		t.Errorf("cold access after learning: algorithm = %q, want zstd", rec.Algorithm)
	}
}

func TestAdaptiveSelector_WindowEviction(t *testing.T) {
	t.Parallel()
	base := NewRuleSelector()
	windowSize := 20
	sel := NewAdaptiveSelector(base, windowSize)

	stats := comprpkg.Stats{OriginalSize: 100, CompressedSize: 50}
	for range 100 {
		sel.RecordOutcome(ContentClassText, comprpkg.AlgorithmZstd, stats, time.Millisecond)
	}

	counts := sel.Stats()
	if counts[ContentClassText] > windowSize {
		t.Errorf("window not evicted: %d records, want <= %d", counts[ContentClassText], windowSize)
	}
}

func TestAdaptiveSelector_Stats_Empty(t *testing.T) {
	t.Parallel()
	sel := NewAdaptiveSelector(NewRuleSelector(), 50)
	if counts := sel.Stats(); len(counts) != 0 {
		t.Errorf("Stats() = %v, want empty map", counts)
	}
}

func TestAdaptiveSelector_ZeroOriginalSize_Skipped(t *testing.T) {
	t.Parallel()
	sel := NewAdaptiveSelector(NewRuleSelector(), 50)
	// OriginalSize=0 should be a no-op.
	sel.RecordOutcome(ContentClassText, comprpkg.AlgorithmZstd, comprpkg.Stats{}, time.Millisecond)
	if counts := sel.Stats(); counts[ContentClassText] != 0 {
		t.Errorf("zero-size outcome should not be recorded, got %d", counts[ContentClassText])
	}
}

func TestNewAdaptiveSelector_ZeroWindowSize_UsesDefault(t *testing.T) {
	t.Parallel()
	sel := NewAdaptiveSelector(NewRuleSelector(), 0)
	if sel.windowSize != 100 {
		t.Errorf("windowSize = %d, want 100", sel.windowSize)
	}
}
