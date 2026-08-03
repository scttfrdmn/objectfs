// Package cost provides real-time per-operation S3 cost calculation, per-tenant
// accumulation, ROI reporting, and budget-threshold alerting.
package cost

import (
	"maps"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// S3 storage tier identifiers — aliases of internal/awsname's storage classes rather than string
// literals, so there is one authority for which tiers exist. They were spelled out here until the
// rate consolidation; a second spelling is a second thing that can disagree.
const (
	TierStandard          = awsname.StorageClassStandard
	TierStandardIA        = awsname.StorageClassStandardIA
	TierOneZoneIA         = awsname.StorageClassOneZoneIA
	TierReducedRedundancy = awsname.StorageClassReducedRedundancy
	TierGlacierIR         = awsname.StorageClassGlacierIR
	TierGlacier           = awsname.StorageClassGlacier
	TierDeepArchive       = awsname.StorageClassDeepArchive
	TierIntelligent       = awsname.StorageClassIntelligent
)

// Price holds per-tier pricing in USD.
//
// It is an alias of [awsrates.Rate], not a separate struct. It used to be its own type carrying its
// own copy of the rates, which is how its PUT rate came to differ from the one in
// internal/storage/s3 by a factor of ten — what a write cost depended on which package the caller
// reached for. The alias keeps the name that reads well here (a Price, in a cost calculator) while
// there is exactly one definition of what a rate is and one table of values.
//
// All per-request fields are the cost of a single API call, not per 1,000. See [awsrates.Rate] for
// the field documentation, including which retrieval speed and volume band each figure represents.
type Price = awsrates.Rate

// DefaultPrices is the built-in rate table for [awsrates.DefaultRegion], read from [awsrates].
//
// It is a function of the canonical table rather than a literal, so it cannot drift from it. The
// previous literal claimed in its own comment to "match the values in internal/storage/s3/tiers.go
// and pricing_manager.go" and did not — which is the argument against comments that assert
// agreement between two tables instead of removing one of them.
//
// # It is us-east-1's table, and that is a decision rather than an oversight
//
// #161 made rates region-aware and plumbed the region through internal/storage/s3, which is on the
// mount path. This package was left region-blind, for two reasons that should be checked rather than
// assumed if either changes:
//
//   - Nothing imports it. `grep` for this package's import path across every non-test .go file in the
//     repository returns no hits, so no cost figure it produces reaches an operator today. Plumbing a
//     region through an unreachable package would be untested by construction — the region argument
//     would have no caller to be wrong for.
//   - The region belongs in a constructor, not here. A package-level var cannot take one, which is
//     exactly the shape of the defect #161 closed: [PriceTable] is the type that should carry a region
//     when this package acquires a caller, and this var is the us-east-1 default it starts from.
//
// So the honest statement is that these are us-east-1 list prices, named as such. A caller pricing a
// deployment elsewhere wants [awsrates.AllForRegion]; internal/storage/s3's PricingManager is the shape
// to copy, including the warn-once-on-fallback behavior.
var DefaultPrices = awsrates.All()

// PriceTable is an immutable pricing lookup with optional per-tier overrides.
type PriceTable struct {
	prices map[string]Price
}

// NewPriceTable creates a PriceTable starting from DefaultPrices, then applying
// any overrides provided.  Pass nil or an empty map to use defaults as-is.
func NewPriceTable(overrides map[string]Price) *PriceTable {
	merged := make(map[string]Price, len(DefaultPrices))
	maps.Copy(merged, DefaultPrices)
	maps.Copy(merged, overrides)
	return &PriceTable{prices: merged}
}

// Get returns the Price for tier and reports whether the tier was found.
// If the tier is unknown, the Standard price is returned as a fallback.
func (pt *PriceTable) Get(tier string) (Price, bool) {
	p, ok := pt.prices[tier]
	if !ok {
		return pt.prices[TierStandard], false
	}
	return p, true
}

// Tiers returns the list of tier names present in the table.
func (pt *PriceTable) Tiers() []string {
	tiers := make([]string, 0, len(pt.prices))
	for k := range pt.prices {
		tiers = append(tiers, k)
	}
	return tiers
}
