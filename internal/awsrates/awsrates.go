// Package awsrates holds the S3 list prices ObjectFS quotes, in one place, for every region AWS
// publishes them in.
//
// It exists for the same reason as internal/awsname, and is a leaf for the same reason: the rate for
// a tier is needed by internal/cost (per-operation accounting), internal/storage/s3 (tier
// comparison and transition decisions), and internal/analytics (savings modeling), and those three
// cannot import each other. Before this package the rates were spelled five times — in
// internal/cost/pricing.go, internal/cost/reporter.go, internal/storage/s3/tiers.go,
// internal/storage/s3/doc.go, and internal/analytics/model.go — and two of the copies disagreed.
// Not hypothetically: the PUT rate in internal/storage/s3 was a tenth of the PUT rate in
// internal/cost, so what a write cost depended on which package the caller happened to reach for.
//
// # Every number here is generated from AWS's published price list
//
// rates_generated.go is produced by internal/awsrates/offerfile from the public per-region offer
// files, and it covers 36 regions × 8 storage classes × 6 rates. It is not hand-maintained, and it is
// not the Pricing API: the offer files need no credentials, so `go generate ./internal/awsrates/...`
// refreshes every number in one command that anyone can run.
//
// Regenerating is the only supported way to change a rate. The rules that turn AWS's JSON into these
// numbers are documented in the offerfile package, and three of them exist because the obvious query
// returns a plausible number from the wrong SKU — a *restore* price where a PUT was meant, a
// *staging* charge 21× the real Deep Archive rate, and whichever volume band Go's map iteration
// happened to reach first. Editing a value by hand loses that.
//
// # Regions
//
// [ForRegion] is the real accessor. [For] is the us-east-1 form, kept because most callers are
// comparing tiers rather than pricing a specific deployment and because it is what every existing
// caller passed no region to.
//
// A region ObjectFS has no table for falls back to us-east-1 and says so, via [ForRegion]'s second
// return value. Falling back rather than returning zero is deliberate: a zero rate does not read as
// "unknown", it reads as free storage, which is a plausible enough answer to survive review. What the
// caller must not do is report a us-east-1 figure labeled with the operator's region — regional
// variation is not a rounding error. Standard storage ranges from $0.0225 in ap-east-2 to $0.0405 in
// sa-east-1, which is 76% above us-east-1, and GovCloud egress is 72% above it.
//
// # What these rates are for
//
// Comparing tiers and estimating what an access pattern costs — deciding whether moving an object
// to Glacier IR pays for itself. They are list prices, so they do not know about your Enterprise
// Discount Program agreement, Reserved Capacity, free tier, or the request charges that only appear
// once versioning or replication is on. Do not reconcile a bill against them.
package awsrates

//go:generate go run ./offerfile/cmd/genrates -o rates_generated.go

import (
	"maps"
	"slices"

	"github.com/scttfrdmn/objectfs/internal/awsname"
)

// DefaultRegion is the region [For] reports, and the one an unknown region falls back to.
//
// us-east-1 rather than the cheapest or the most common: it is the region AWS's own pricing pages
// default to, so a figure from it is the one an operator can check against what they have already
// read. It is also below most other regions, which means a fallback understates rather than
// overstates — recorded here because the opposite choice would have been defensible and someone will
// wonder.
const DefaultRegion = "us-east-1"

// Rate holds the per-tier list prices for one storage class, in USD.
//
// Per-request fields are the cost of a single call, not per thousand. AWS publishes them per
// thousand or per ten thousand depending on the tier, and converting at each use site is how the
// PUT rate ended up a tenth of what it should be — 0.0005 read as a per-request price when it was
// per-thousand. The unit is in the type, once.
type Rate struct {
	// StoragePerGBMonth is the cost to store one GB for a calendar month, where a GB is 10^9
	// bytes. See GBFromBytes: AWS bills decimal GB, and dividing by 2^30 understates every
	// storage figure by 7.4%.
	//
	// For tiers AWS prices in volume bands this is the first (most expensive) band, which is the
	// right default for a filesystem: the band boundary is 50 TB of stored data, so any deployment
	// below that pays exactly this, and one above it pays less. Erring toward the higher rate
	// means a tier-transition recommendation is conservative rather than optimistic.
	StoragePerGBMonth float64

	// PutRequest is the cost of one PUT, COPY, POST, or LIST — AWS's "Tier1" request group.
	PutRequest float64

	// GetRequest is the cost of one GET, HEAD, or other read request — AWS's "Tier2" group.
	GetRequest float64

	// ListRequest is the cost of one LIST. AWS bills LIST in the same Tier1 group as PUT, so this
	// equals PutRequest for every tier today. It is a separate field because the two are separate
	// operations in the cost accounting, and AWS has priced them apart before.
	ListRequest float64

	// RetrievalPerGB is the per-GB fee for reading bytes out of the tier, zero where there is
	// none. For the tiers with several retrieval speeds this is the Standard speed; Expedited and
	// Bulk differ, and modeling that needs a retrieval-speed concept the cost code does not have.
	RetrievalPerGB float64

	// EgressPerGB is the cost of transferring one GB out to the internet from this region. It is
	// not a property of the storage class — the same rate applies to all of them — and it is zero
	// when traffic stays inside the region, which is the case for the deployments ObjectFS targets.
	// It is here so a caller that does egress can price it without a second table.
	//
	// It comes from a different AWS offer file than everything else in this struct. S3's own
	// DataTransfer-Out-Bytes usagetype is the Multi-Region Access Point routing charge, not internet
	// egress; see the offerfile package.
	EgressPerGB float64
}

// Regions returns the region codes with a rate table, sorted.
func Regions() []string {
	return slices.Sorted(maps.Keys(regionalRates))
}

// HasRegion reports whether a region has its own rates.
//
// For deciding whether to warn once at startup rather than on every lookup, which is what
// internal/storage/s3 does with it: a cost figure is produced per object access, and a warning on
// each one would be the loudest thing in the log.
func HasRegion(region string) bool {
	_, ok := regionalRates[region]

	return ok
}

// ForRegion returns the rate for a storage class in a region, and whether both were known.
//
// The bool is false if the region is unknown, if the class is unknown, or both — a caller that only
// wants to know "is this number about the region I asked for" gets that from one check. Which of the
// two was missing is available from [HasRegion] and [StorageClasses] when it matters.
//
// An unknown region yields us-east-1's rate for the class. An unknown class yields Standard's rate.
// Both are wrong answers reported as such rather than zeros reported as certainties; Standard is the
// most expensive commonly-used tier, so guessing it errs toward overstating a cost.
func ForRegion(region, storageClass string) (Rate, bool) {
	table, regionKnown := regionalRates[region]
	if !regionKnown {
		table = regionalRates[DefaultRegion]
	}

	r, classKnown := table[storageClass]
	if !classKnown {
		r = table[awsname.StorageClassStandard]
	}

	return r, regionKnown && classKnown
}

// For returns the us-east-1 rate for a storage class and whether the class was found.
//
// Equivalent to ForRegion(DefaultRegion, storageClass). It is the right call for a comparison
// between tiers, where only the ratio matters and every region agrees on the ordering; it is the
// wrong call for telling an operator what their bucket costs.
func For(storageClass string) (Rate, bool) {
	return ForRegion(DefaultRegion, storageClass)
}

// AllForRegion returns a copy of one region's whole table, keyed by storage class.
//
// A copy, because a caller that mutated the shared map would change what every other package
// believes an object costs. An unknown region yields us-east-1's table and false.
func AllForRegion(region string) (map[string]Rate, bool) {
	table, ok := regionalRates[region]
	if !ok {
		table = regionalRates[DefaultRegion]
	}

	return maps.Clone(table), ok
}

// All returns a copy of the us-east-1 table, keyed by storage class.
func All() map[string]Rate {
	table, _ := AllForRegion(DefaultRegion)

	return table
}

// StorageClasses returns the classes that have a rate, warmest-first.
func StorageClasses() []string {
	return slices.Clone(awsname.StorageClasses())
}

// bytesPerGB is 10^9, because that is how AWS bills a GB-month.
//
// Not 2^30. S3 pricing is quoted in decimal GB, so treating a GB as 1,073,741,824 bytes understates
// every storage cost by 7.4% — and the code this replaced did exactly that, with a comment
// asserting the binary interpretation was correct, which is how it survived.
const bytesPerGB = 1e9

// GBFromBytes converts a byte count to the GB unit AWS bills in.
//
// Negative and zero inputs return 0 rather than a negative cost.
func GBFromBytes(b int64) float64 {
	if b <= 0 {
		return 0
	}

	return float64(b) / bytesPerGB
}
