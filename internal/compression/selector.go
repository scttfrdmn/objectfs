package compression

import comprpkg "github.com/objectfs/objectfs/pkg/compression"

// AccessHint provides context about the expected read pattern for an object.
// Hot objects should favor fast decompression (LZ4); cold objects can afford
// slower decompression in exchange for a better compression ratio (ZSTD).
type AccessHint int

const (
	// AccessHintUnknown defers to the rule-based defaults.
	AccessHintUnknown AccessHint = iota
	// AccessHintHot indicates the object is read frequently — prefer speed.
	AccessHintHot
	// AccessHintWarm indicates moderate access — prefer balanced compression.
	AccessHintWarm
	// AccessHintCold indicates infrequent access — prefer maximum ratio.
	AccessHintCold
)

// Recommendation is the output of a Selector: the algorithm and level to use,
// plus a human-readable reason string for logging and debugging.
type Recommendation struct {
	Algorithm comprpkg.Algorithm
	Level     int
	Reason    string
}

// Selector chooses a compression algorithm for a given object.
type Selector interface {
	// Select returns a compression recommendation for the given data analysis,
	// access hint, and object size in bytes.
	Select(analysis Analysis, hint AccessHint, size int64) Recommendation
}

// RuleSelector is a stateless, deterministic selector based on content-class
// rules and access hints.  It requires no warm-up period and forms the base
// for AdaptiveSelector.
type RuleSelector struct{}

// NewRuleSelector returns a new RuleSelector.
func NewRuleSelector() *RuleSelector { return &RuleSelector{} }

// Select applies deterministic rules to return a compression recommendation.
//
// Decision table (evaluated in order):
//  1. Already-compressed or near-random data (CompressScore < 0.1) → none
//  2. Hot access hint → LZ4 (fast decompression)
//  3. Cold access hint + compressible → ZSTD level 9 (maximum ratio)
//  4. Large object (> 64 MiB) + compressible → ZSTD level 6
//  5. Default → ZSTD level 3 (balanced)
func (r *RuleSelector) Select(analysis Analysis, hint AccessHint, size int64) Recommendation {
	// Never compress already-compressed or essentially-random data.
	if analysis.ContentClass == ContentClassCompressed || analysis.CompressScore < 0.1 {
		return Recommendation{
			Algorithm: comprpkg.AlgorithmNone,
			Level:     0,
			Reason:    "content is already compressed or incompressible",
		}
	}

	// Hot path: favor LZ4's fast decompression for frequently read objects.
	if hint == AccessHintHot {
		return Recommendation{
			Algorithm: comprpkg.AlgorithmLZ4,
			Level:     0,
			Reason:    "hot access pattern — prefer LZ4 decompression speed",
		}
	}

	// Cold path: pay the compression cost once for long-term storage savings.
	if hint == AccessHintCold && analysis.CompressScore >= 0.3 {
		return Recommendation{
			Algorithm: comprpkg.AlgorithmZstd,
			Level:     9,
			Reason:    "cold access pattern — prefer ZSTD level 9 ratio",
		}
	}

	// Large compressible objects benefit from a higher ZSTD level.
	const largeCutoff = 64 * 1024 * 1024 // 64 MiB
	if size >= largeCutoff && analysis.CompressScore >= 0.4 {
		return Recommendation{
			Algorithm: comprpkg.AlgorithmZstd,
			Level:     6,
			Reason:    "large compressible object — ZSTD level 6",
		}
	}

	// Default: balanced ZSTD at level 3.
	return Recommendation{
		Algorithm: comprpkg.AlgorithmZstd,
		Level:     3,
		Reason:    "default — ZSTD level 3 balanced",
	}
}
