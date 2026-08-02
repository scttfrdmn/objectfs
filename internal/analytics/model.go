package analytics

import (
	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// S3 storage tier strings.
//
// Aliases of internal/awsname rather than string literals. They were spelled out here with a comment
// saying they "match the constants in internal/storage/s3/tiers.go" — a comment asserting agreement
// between two lists is a promise nothing checks, and the rate table below it did not in fact agree
// with the one it was copied from.
const (
	TierStandard    = awsname.StorageClassStandard
	TierStandardIA  = awsname.StorageClassStandardIA
	TierGlacierIR   = awsname.StorageClassGlacierIR
	TierGlacier     = awsname.StorageClassGlacier
	TierDeepArchive = awsname.StorageClassDeepArchive
)

// storagePerGBMonth returns the us-east-1 list price for a tier, used to estimate savings.
//
// Read from [awsrates] rather than held as a literal here. This package's own copy was the fifth in
// the repo; consolidating them is what #209 asked for, on the grounds that five copies is why two of
// them disagreed. Savings estimates are relative figures, so an absolute rate error moves every
// recommendation this classifier makes without changing its shape — nothing downstream would notice.
func storagePerGBMonth(tier string) float64 {
	rate, _ := awsrates.For(tier)
	return rate.StoragePerGBMonth
}

// Recommendation is the output of the TierClassifier.
type Recommendation struct {
	// Key is the object key this recommendation applies to.
	Key string

	// Tier is the recommended AWS S3 storage class (e.g., "STANDARD_IA").
	Tier string

	// Confidence is the classifier's confidence in the recommendation, in [0, 1].
	Confidence float64

	// Reason is a human-readable explanation of the decision path taken.
	Reason string

	// MonthlySavingsPerGB is the estimated savings in USD/GB/month relative to
	// Standard storage.  Zero for STANDARD recommendations.
	MonthlySavingsPerGB float64
}

// decisionNode is a node in the classification decision tree.
// Leaf nodes have Leaf != nil; internal nodes have Feature and one or two children.
type decisionNode struct {
	// featureName is a label used in Reason strings (e.g. "hours_since_last_access").
	featureName string
	// feature extracts the relevant scalar from a FeatureVector.
	feature   func(FeatureVector) float64
	threshold float64
	// left is followed when feature(fv) < threshold; right otherwise.
	left, right *decisionNode
	// leaf is non-nil for leaf nodes.
	leaf *leafOutcome
}

type leafOutcome struct {
	tier       string
	confidence float64
	reason     string
}

func (n *decisionNode) classify(fv FeatureVector) *leafOutcome {
	if n.leaf != nil {
		return n.leaf
	}
	if n.feature(fv) < n.threshold {
		return n.left.classify(fv)
	}
	return n.right.classify(fv)
}

// TierClassifier classifies objects into S3 storage tiers using a decision tree
// calibrated for research-computing workloads.
//
// Decision tree (condensed):
//
//	HoursSinceLastAccess < 24       → STANDARD       (0.95) — accessed today
//	AccessRate7d         ≥ 1.0      → STANDARD       (0.90) — daily access this week
//	AccessRate30d        ≥ 0.5      → STANDARD_IA    (0.85) — accessed >3×/week
//	HoursSinceLastAccess < 720
//	  AccessRate30d      ≥ 0.1      → GLACIER_IR     (0.82) — occasional, <30 days old
//	  else               → GLACIER  (0.80) — rare, <30 days old
//	HoursSinceLastAccess < 2160     → GLACIER        (0.80) — 30–90 days quiet
//	else                            → DEEP_ARCHIVE   (0.88) — 90+ days cold
type TierClassifier struct {
	root *decisionNode
}

// NewTierClassifier creates a TierClassifier with hard-coded research-computing rules.
func NewTierClassifier() *TierClassifier {
	// Helper shorthands.
	feature := func(name string, fn func(FeatureVector) float64, threshold float64,
		left, right *decisionNode) *decisionNode {
		return &decisionNode{featureName: name, feature: fn, threshold: threshold,
			left: left, right: right}
	}
	leaf := func(tier string, conf float64, reason string) *decisionNode {
		return &decisionNode{leaf: &leafOutcome{tier: tier, confidence: conf, reason: reason}}
	}

	// Build tree bottom-up.

	// Subtree for HoursSinceLastAccess ≥ 720 (30 days since last access)
	notAccessedIn30days := feature(
		"hours_since_last_access", func(fv FeatureVector) float64 { return fv.HoursSinceLastAccess },
		2160, // 90 days
		// < 2160 h (30–90 days without access)
		leaf(TierGlacier, 0.80, "30–90 days since last access"),
		// ≥ 2160 h (> 90 days without access)
		leaf(TierDeepArchive, 0.88, "90+ days since last access"),
	)

	// Subtree for HoursSinceLastAccess ∈ [24, 720) — less than 30 days since last access
	// but not hot enough for Standard/StandardIA
	recentButCold := feature(
		"access_rate_30d", func(fv FeatureVector) float64 { return fv.AccessRate30d },
		0.1, // < 3 accesses in last 30 days
		// < 0.1 acc/day → rare access within 30 days
		leaf(TierGlacier, 0.80, "< 3 accesses in last 30 days"),
		// ≥ 0.1 acc/day → occasional access, still recently touched
		leaf(TierGlacierIR, 0.82, "occasional access, last touched within 30 days"),
	)

	// Split: within 30 days or not
	withinOrBeyond30days := feature(
		"hours_since_last_access", func(fv FeatureVector) float64 { return fv.HoursSinceLastAccess },
		720, // 30 days
		recentButCold,
		notAccessedIn30days,
	)

	// Moderate access: AccessRate30d ≥ 0.5 but < daily (not hot enough for Standard)
	moderateAccess := feature(
		"access_rate_30d", func(fv FeatureVector) float64 { return fv.AccessRate30d },
		0.5, // < 0.5 acc/day → warm but infrequent
		withinOrBeyond30days,
		// ≥ 0.5 acc/day (>15 accesses last 30 days) — warm
		leaf(TierStandardIA, 0.85, "> 15 accesses in last 30 days"),
	)

	// Weekly hot split: AccessRate7d ≥ 1.0 → Standard
	weeklyHot := feature(
		"access_rate_7d", func(fv FeatureVector) float64 { return fv.AccessRate7d },
		1.0,
		moderateAccess,
		// ≥ 1 access/day this week → definitely hot
		leaf(TierStandard, 0.90, "≥ 1 access/day over last 7 days"),
	)

	// Root: accessed within the last 24 hours → Standard
	root := feature(
		"hours_since_last_access", func(fv FeatureVector) float64 { return fv.HoursSinceLastAccess },
		24,
		// < 24 h → hot
		leaf(TierStandard, 0.95, "accessed within the last 24 hours"),
		weeklyHot,
	)

	return &TierClassifier{root: root}
}

// Classify returns a Recommendation for key given its feature vector fv.
func (tc *TierClassifier) Classify(key string, fv FeatureVector) Recommendation {
	outcome := tc.root.classify(fv)
	savings := storagePerGBMonth(TierStandard) - storagePerGBMonth(outcome.tier)
	if savings < 0 {
		savings = 0
	}
	return Recommendation{
		Key:                 key,
		Tier:                outcome.tier,
		Confidence:          outcome.confidence,
		Reason:              outcome.reason,
		MonthlySavingsPerGB: savings,
	}
}
