package cost

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultPrices_AllTiersPresent(t *testing.T) {
	t.Parallel()
	tiers := []string{
		TierStandard, TierStandardIA, TierOneZoneIA,
		TierGlacierIR, TierGlacier, TierDeepArchive, TierIntelligent,
	}
	for _, tier := range tiers {
		_, ok := DefaultPrices[tier]
		assert.True(t, ok, "tier %s should be in DefaultPrices", tier)
	}
}

func TestDefaultPrices_PositiveValues(t *testing.T) {
	t.Parallel()
	for tier, p := range DefaultPrices {
		assert.Greater(t, p.StoragePerGBMonth, 0.0, "tier %s StoragePerGBMonth", tier)
		assert.Greater(t, p.GetRequest, 0.0, "tier %s GetRequest", tier)
		assert.Greater(t, p.PutRequest, 0.0, "tier %s PutRequest", tier)
		assert.GreaterOrEqual(t, p.RetrievalPerGB, 0.0, "tier %s RetrievalPerGB", tier)
		assert.GreaterOrEqual(t, p.EgressPerGB, 0.0, "tier %s EgressPerGB", tier)
	}
}

func TestDefaultPrices_StorageCostOrdering(t *testing.T) {
	t.Parallel()
	// STANDARD > STANDARD_IA > GLACIER_IR > GLACIER > DEEP_ARCHIVE
	assert.Greater(t, DefaultPrices[TierStandard].StoragePerGBMonth, DefaultPrices[TierStandardIA].StoragePerGBMonth)
	assert.Greater(t, DefaultPrices[TierStandardIA].StoragePerGBMonth, DefaultPrices[TierGlacierIR].StoragePerGBMonth)
	assert.Greater(t, DefaultPrices[TierGlacierIR].StoragePerGBMonth, DefaultPrices[TierGlacier].StoragePerGBMonth)
	assert.Greater(t, DefaultPrices[TierGlacier].StoragePerGBMonth, DefaultPrices[TierDeepArchive].StoragePerGBMonth)
}

func TestNewPriceTable_DefaultsWithNilOverrides(t *testing.T) {
	t.Parallel()
	pt := NewPriceTable(nil)
	for tier, want := range DefaultPrices {
		got, ok := pt.Get(tier)
		assert.True(t, ok)
		assert.Equal(t, want, got, "tier %s", tier)
	}
}

func TestNewPriceTable_OverrideApplied(t *testing.T) {
	t.Parallel()
	custom := Price{StoragePerGBMonth: 0.001, GetRequest: 0.000001}
	pt := NewPriceTable(map[string]Price{TierStandard: custom})
	got, ok := pt.Get(TierStandard)
	assert.True(t, ok)
	assert.Equal(t, custom, got)
	// Other tiers remain at defaults.
	ia, ok := pt.Get(TierStandardIA)
	assert.True(t, ok)
	assert.Equal(t, DefaultPrices[TierStandardIA], ia)
}

func TestPriceTable_Get_UnknownTierFallback(t *testing.T) {
	t.Parallel()
	pt := NewPriceTable(nil)
	got, ok := pt.Get("UNKNOWN_TIER")
	assert.False(t, ok)
	assert.Equal(t, DefaultPrices[TierStandard], got)
}

func TestPriceTable_Tiers(t *testing.T) {
	t.Parallel()
	pt := NewPriceTable(nil)
	tiers := pt.Tiers()
	assert.Len(t, tiers, len(DefaultPrices))
	// All default tiers must be present.
	tierSet := make(map[string]bool, len(tiers))
	for _, tr := range tiers {
		tierSet[tr] = true
	}
	for tier := range DefaultPrices {
		assert.True(t, tierSet[tier], "tier %s missing from Tiers()", tier)
	}
}
