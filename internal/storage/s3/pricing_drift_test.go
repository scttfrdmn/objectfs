package s3

import (
	"fmt"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// The #209 drift guard, which lived in internal/cost until that package was deleted for #226.
//
// #209's defect was not a wrong number in one place; it was the same number written in five places,
// two of which disagreed by a factor of ten, so what a write cost depended on which package the caller
// reached for. A fix that only corrects the values leaves the shape that produced them. So the guard
// asserts the shape: every rate a caller can reach through PricingManager is the one internal/awsrates
// holds, exactly, for every class the config loader accepts.
//
// It moved here rather than being deleted with the package because internal/storage/s3 is now the only
// place a rate reaches a caller — internal/cost held the second table and had no importer, and its
// version of this test compared *its* table to awsrates and to this one. With one table left, the
// half worth keeping is the half about this one.
//
// The neighbouring tests in pricing_manager_test.go check the *values* against AWS's published
// figures, stated as literals. This checks the *plumbing*: that the manager reads the canonical table
// instead of any literal of its own. Both are needed — a package that grows a private copy of the
// right numbers passes the value tests and fails here, and that is the state #209 was found in.

// TestEveryRateAManagerReportsComesFromAwsrates fails the moment this package grows a rate of its own.
//
// Exact comparison, not a tolerance. These are meant to be the same float64 read from the same map, so
// any difference at all means a value was recomputed or re-entered somewhere — which is precisely what
// is being prevented. A tolerance would let small versions of the defect through, and small is how
// this kind of drift starts.
//
// Every money field, not just storage: a plausible partial regression reads storage from awsrates and
// leaves requests on constants, and a storage-only assertion cannot see it. The default PricingConfig
// is used because that is the path a mount with no pricing configuration takes, and it is where the
// old private table was read.
func TestEveryRateAManagerReportsComesFromAwsrates(t *testing.T) {
	t.Parallel()

	manager := NewPricingManager(PricingConfig{Region: awsrates.DefaultRegion}, discardLogger())

	for _, class := range awsname.StorageClasses() {
		t.Run(class, func(t *testing.T) {
			t.Parallel()

			canonical, ok := awsrates.For(class)
			if !ok {
				t.Fatalf("internal/awsrates has no rate for %s, which the config loader accepts; "+
					"For fell back to Standard, so every comparison below is against another "+
					"tier's price", class)
			}

			pricing, err := manager.GetTierPricing(class)
			if err != nil {
				t.Fatalf("GetTierPricing(%s): %v", class, err)
			}

			// StorageRate is the accessor; GetTierPricing is the full path. Both, because the
			// region-awareness work (#161) added the accessor and a caller could reasonably use
			// either.
			if got := manager.StorageRate(class); got != canonical.StoragePerGBMonth {
				t.Errorf("StorageRate(%s) = %v, awsrates says %v; ratio %.4gx — the manager is "+
					"reading a rate from somewhere other than awsrates.ForRegion",
					class, got, canonical.StoragePerGBMonth, got/canonical.StoragePerGBMonth)
			}

			for _, field := range []struct {
				name       string
				got, want  float64
				whatItCost string
			}{
				{"StorageCostPerGBMonth", pricing.StorageCostPerGBMonth, canonical.StoragePerGBMonth,
					"holding a GB for a month"},
				{"RequestCosts.PutRequestCost", pricing.RequestCosts.PutRequestCost, canonical.PutRequest,
					"a write"},
				{"RequestCosts.GetRequestCost", pricing.RequestCosts.GetRequestCost, canonical.GetRequest,
					"a read"},
				{"RequestCosts.ListRequestCost", pricing.RequestCosts.ListRequestCost, canonical.ListRequest,
					"a directory listing"},
				{"RetrievalCostPerGB", pricing.RetrievalCostPerGB, canonical.RetrievalPerGB,
					"retrieving a GB"},

				// Egress is not here because TierPricing has no field for it: awsrates.EgressPerGB
				// is now a rate with no consumer, its only one having been internal/cost's
				// Calculator. Left in the table rather than deleted because it is generated from
				// AWS's price list and a mount that ever reports egress will want it — but nothing
				// prices egress today, so there is no plumbing here to guard.
			} {
				if field.got != field.want {
					ratio := "undefined (the canonical rate is zero)"
					if field.want != 0 {
						ratio = formatRatio(field.got / field.want)
					}

					t.Errorf("%s.%s = %v, awsrates says %v — what %s costs depends on which "+
						"table a caller reads, which is #209. Ratio: %s",
						class, field.name, field.got, field.want, field.whatItCost, ratio)
				}
			}
		})
	}
}

// formatRatio renders a drift ratio, because a round factor of ten in one names its own cause: a
// per-1,000 price stored in a per-request field.
func formatRatio(r float64) string {
	switch {
	case abs(r-10) < 1e-9:
		return "10x — a per-1,000 published price stored as if it were per-request"
	case abs(r-0.1) < 1e-9:
		return "0.1x — a per-request rate divided by 1,000 a second time"
	default:
		return fmt.Sprintf("%.4gx", r)
	}
}
