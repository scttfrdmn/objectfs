package cost

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/awsrates"
	"github.com/scttfrdmn/objectfs/internal/storage/s3"
)

// This file used to hold a literal named authoritativeStorageRates: a hand-maintained copy of
// StorageTierInfo.CostPerGBMonth from internal/storage/s3, compared against DefaultPrices here. Its
// comment explained that it was duplicated "rather than imported" because internal/cost "deliberately
// does not depend on internal/storage/s3".
//
// That rationale did not hold. internal/storage/s3 does not import internal/cost, so there is no
// cycle and never was — a test in this package can import s3 directly, which `go list -deps`
// confirms. The literal was therefore a third copy of the rates, added by a test whose whole purpose
// was to catch there being more than one. It also silently narrowed over time: it had no entry for
// REDUCED_REDUNDANCY, so that tier's rate went unchecked by the drift test that existed to check
// rates.
//
// Both tables now read from internal/awsrates, so a literal comparison would only be asserting that
// two copies of the same variable are equal. What is worth checking instead is that they still *do*
// read from it — that nobody reintroduces a local rate — and that is what these tests do, by
// comparing each table to awsrates and to each other for every class the config loader accepts.

// TestBothRateTablesReadFromAwsrates fails the moment either package grows its own copy of a rate
// again.
//
// This is the regression guard for #209. The defect there was not a wrong number in one place; it was
// the same number written in five places, two of which disagreed by a factor of ten, so what a write
// cost depended on which package the caller reached for. A fix that only corrects the value leaves
// that shape intact. This asserts the shape: for every storage class, both tables equal the canonical
// one, exactly.
//
// The comparison is exact — InDelta with a delta of zero — rather than within a tolerance, on
// purpose. These are meant to be the same float64 read from the same map, so any difference at all
// means a value was recomputed or re-entered somewhere, and that is precisely what is being
// prevented. A tolerance would let small versions of the defect through.
func TestBothRateTablesReadFromAwsrates(t *testing.T) {
	t.Parallel()

	for _, class := range awsname.StorageClasses() {
		t.Run(class, func(t *testing.T) {
			t.Parallel()

			canonical, ok := awsrates.For(class)
			assert.True(t, ok, "internal/awsrates has no rate for %s, which the config loader accepts", class)

			price, ok := DefaultPrices[class]
			assert.True(t, ok, "DefaultPrices is missing %s", class)
			assert.InDelta(t, canonical.StoragePerGBMonth, price.StoragePerGBMonth, 0,
				"internal/cost reports a different storage rate for %s than internal/awsrates; "+
					"DefaultPrices should be awsrates.All(), not a literal", class)

			tier, ok := s3.StorageTiers[class]
			assert.True(t, ok, "internal/storage/s3 StorageTiers is missing %s", class)
			assert.InDelta(t, canonical.StoragePerGBMonth, tier.CostPerGBMonth, 0,
				"internal/storage/s3 reports a different storage rate for %s than internal/awsrates; "+
					"CostPerGBMonth should be filled in by withRates, not written in the literal", class)
		})
	}
}

// TestNoTierIsPricedAtZero covers the failure mode that does not look like a failure.
//
// A missing rate produces $0/GB-month, and a cost report showing $0 reads as free storage rather than
// as a lookup that missed. Nothing downstream distinguishes the two, so this is the only place it can
// be caught.
func TestNoTierIsPricedAtZero(t *testing.T) {
	t.Parallel()

	for _, class := range awsname.StorageClasses() {
		price, ok := DefaultPrices[class]
		assert.True(t, ok, "%s has no pricing entry", class)
		assert.Positive(t, price.StoragePerGBMonth,
			"%s stores at $0/GB-month; no S3 class is free, and a zero rate makes every estimate for "+
				"this tier read as free rather than as missing", class)
		assert.Positive(t, price.PutRequest, "%s writes at $0 per request", class)
		assert.Positive(t, price.GetRequest, "%s reads at $0 per request", class)
	}
}
