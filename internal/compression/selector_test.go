package compression

import (
	"testing"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

func TestRuleSelector_AlreadyCompressed(t *testing.T) {
	t.Parallel()
	sel := NewRuleSelector()
	a := Analysis{ContentClass: ContentClassCompressed, CompressScore: 0.02}
	rec := sel.Select(a, AccessHintUnknown, 1024)
	if rec.Algorithm != comprpkg.AlgorithmNone {
		t.Errorf("algorithm = %q, want none", rec.Algorithm)
	}
}

func TestRuleSelector_IncompressibleData(t *testing.T) {
	t.Parallel()
	sel := NewRuleSelector()
	// High entropy, low compress score.
	a := Analysis{ContentClass: ContentClassBinary, CompressScore: 0.05, Entropy: 7.8}
	rec := sel.Select(a, AccessHintUnknown, 1024)
	if rec.Algorithm != comprpkg.AlgorithmNone {
		t.Errorf("algorithm = %q, want none for incompressible data", rec.Algorithm)
	}
}

func TestRuleSelector_HotAccess_UsesLZ4(t *testing.T) {
	t.Parallel()
	sel := NewRuleSelector()
	a := Analysis{ContentClass: ContentClassText, CompressScore: 0.7, Entropy: 3.0}
	rec := sel.Select(a, AccessHintHot, 1024)
	if rec.Algorithm != comprpkg.AlgorithmLZ4 {
		t.Errorf("algorithm = %q, want lz4 for hot access", rec.Algorithm)
	}
}

func TestRuleSelector_ColdAccess_UsesZstdLevel9(t *testing.T) {
	t.Parallel()
	sel := NewRuleSelector()
	a := Analysis{ContentClass: ContentClassText, CompressScore: 0.8, Entropy: 3.0}
	rec := sel.Select(a, AccessHintCold, 1024)
	if rec.Algorithm != comprpkg.AlgorithmZstd {
		t.Errorf("algorithm = %q, want zstd for cold access", rec.Algorithm)
	}
	if rec.Level != 9 {
		t.Errorf("level = %d, want 9 for cold access", rec.Level)
	}
}

func TestRuleSelector_LargeObject_ZstdLevel6(t *testing.T) {
	t.Parallel()
	sel := NewRuleSelector()
	a := Analysis{ContentClass: ContentClassBinary, CompressScore: 0.6, Entropy: 4.0}
	rec := sel.Select(a, AccessHintUnknown, 128*1024*1024) // 128 MiB
	if rec.Algorithm != comprpkg.AlgorithmZstd {
		t.Errorf("algorithm = %q, want zstd for large object", rec.Algorithm)
	}
	if rec.Level != 6 {
		t.Errorf("level = %d, want 6 for large object", rec.Level)
	}
}

func TestRuleSelector_Default_ZstdLevel3(t *testing.T) {
	t.Parallel()
	sel := NewRuleSelector()
	a := Analysis{ContentClass: ContentClassText, CompressScore: 0.5, Entropy: 4.5}
	rec := sel.Select(a, AccessHintUnknown, 1024)
	if rec.Algorithm != comprpkg.AlgorithmZstd {
		t.Errorf("algorithm = %q, want zstd default", rec.Algorithm)
	}
	if rec.Level != 3 {
		t.Errorf("level = %d, want 3 for default", rec.Level)
	}
}

func TestRuleSelector_WarmAccess_Default(t *testing.T) {
	t.Parallel()
	sel := NewRuleSelector()
	a := Analysis{ContentClass: ContentClassJSON, CompressScore: 0.6, Entropy: 3.5}
	rec := sel.Select(a, AccessHintWarm, 2048)
	if rec.Algorithm != comprpkg.AlgorithmZstd {
		t.Errorf("algorithm = %q, want zstd for warm access", rec.Algorithm)
	}
}

func TestRuleSelector_ReasonNonEmpty(t *testing.T) {
	t.Parallel()
	sel := NewRuleSelector()
	cases := []struct {
		class ContentClass
		hint  AccessHint
		score float64
		size  int64
	}{
		{ContentClassCompressed, AccessHintUnknown, 0.02, 100},
		{ContentClassText, AccessHintHot, 0.7, 100},
		{ContentClassText, AccessHintCold, 0.7, 100},
		{ContentClassText, AccessHintUnknown, 0.5, 100},
	}
	for _, tc := range cases {
		a := Analysis{ContentClass: tc.class, CompressScore: tc.score}
		rec := sel.Select(a, tc.hint, tc.size)
		if rec.Reason == "" {
			t.Errorf("Select(%v, %v) returned empty Reason", tc.class, tc.hint)
		}
	}
}
