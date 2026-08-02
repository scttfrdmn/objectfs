package cost

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// authoritativeStorageRates mirrors StorageTierInfo.CostPerGBMonth in
// internal/storage/s3/tiers.go, which is the source pricing_manager.go serves
// from. DefaultPrices here carries its own copy of the same storage rates plus
// the per-request and egress fees that tiers.go does not model.
//
// The two tables are independent, so they can silently drift apart and report
// different costs for the same object depending on which package a caller
// reached for. This test fails when they diverge.
//
// Duplicated here rather than imported: internal/cost deliberately does not
// depend on internal/storage/s3 (see the mirrored tier constants at the top of
// pricing.go), so the check is a literal comparison by design.
var authoritativeStorageRates = map[string]float64{
	TierStandard:    0.023,
	TierStandardIA:  0.0125,
	TierOneZoneIA:   0.01,
	TierGlacierIR:   0.004,
	TierGlacier:     0.0036,
	TierDeepArchive: 0.00099,
	TierIntelligent: 0.023,
}

// TestDefaultPricesMatchStorageTiers guards against the two rate tables drifting.
// If this fails, reconcile DefaultPrices with StorageTierInfo.CostPerGBMonth in
// internal/storage/s3/tiers.go and update authoritativeStorageRates above.
func TestDefaultPricesMatchStorageTiers(t *testing.T) {
	t.Parallel()

	for tier, want := range authoritativeStorageRates {
		t.Run(tier, func(t *testing.T) {
			t.Parallel()

			price, ok := DefaultPrices[tier]
			assert.True(t, ok, "DefaultPrices is missing tier %s", tier)
			assert.InDelta(t, want, price.StoragePerGBMonth, 1e-9,
				"storage rate for %s has drifted from internal/storage/s3/tiers.go", tier)
		})
	}
}

// TestDefaultPricesCoversAllTiers ensures no tier is left without pricing, which
// would make Calculator silently return zero cost for it.
func TestDefaultPricesCoversAllTiers(t *testing.T) {
	t.Parallel()

	for tier := range authoritativeStorageRates {
		price, ok := DefaultPrices[tier]
		assert.True(t, ok, "tier %s has no pricing entry", tier)
		assert.Greater(t, price.StoragePerGBMonth, 0.0,
			"tier %s has zero storage cost", tier)
	}
}
