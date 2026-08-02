// Package awsrates holds the S3 list prices ObjectFS quotes, in one place.
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
// # Every number here was read from the AWS Pricing API
//
// Not from the pricing web pages, and not carried over from the tables this package replaces. The
// query for each rate is recorded in rates_aws_test.go, which re-runs them against the live API
// under -tags=integration and fails on any drift. That test is the reason to trust these values, and
// running it is how you find out AWS has changed them.
//
// The region is us-east-1, and that is a real limitation rather than a placeholder: these are the
// only rates in the repository, so a deployment elsewhere gets us-east-1 numbers. It is exported as
// [Region] so a caller can report which region a figure describes instead of implying it matches
// theirs. Making rates region-aware is the Pricing API work in #183; this package is the single site
// that work has to change, which is most of why it exists.
//
// # What these rates are for
//
// Comparing tiers and estimating what an access pattern costs — deciding whether moving an object
// to Glacier IR pays for itself. They are list prices, so they do not know about your Enterprise
// Discount Program agreement, Reserved Capacity, free tier, or the request charges that only appear
// once versioning or replication is on. Do not reconcile a bill against them.
package awsrates

import (
	"maps"
	"slices"

	"github.com/scttfrdmn/objectfs/internal/awsname"
)

// Region is the AWS region these rates were read from.
//
// Exported because a caller that reports a cost should be able to say which region's prices produced
// it. A summary labeled with the operator's configured region while carrying us-east-1 numbers is
// worse than one that names us-east-1, because it looks correct.
const Region = "us-east-1"

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

	// EgressPerGB is the cost of transferring one GB out to the internet. It is not a property of
	// the storage class — the same rate applies to all of them, and it is zero when traffic stays
	// inside the region, which is the case for the deployments ObjectFS targets. It is here so a
	// caller that does egress can price it without a sixth table.
	EgressPerGB float64
}

// rates is the canonical table.
//
// Ordered warmest-first, matching awsname.StorageClasses, because that is the order someone
// choosing a tier reads them in.
var rates = map[string]Rate{
	awsname.StorageClassStandard: {
		StoragePerGBMonth: 0.023,     // TimedStorage-ByteHrs, first 50 TB band
		PutRequest:        0.000005,  // Requests-Tier1: $0.005 / 1,000
		GetRequest:        0.0000004, // Requests-Tier2: $0.004 / 10,000
		ListRequest:       0.000005,  // Tier1, same group as PUT
		RetrievalPerGB:    0.0,       // no retrieval fee
		EgressPerGB:       0.09,      // DataTransfer-Out, first 10 TB band
	},
	awsname.StorageClassIntelligent: {
		StoragePerGBMonth: 0.023,     // TimedStorage-INT-FA-ByteHrs, frequent-access band
		PutRequest:        0.000005,  // Requests-INT-Tier1
		GetRequest:        0.0000004, // Requests-INT-Tier2
		ListRequest:       0.000005,
		RetrievalPerGB:    0.0, // no retrieval fee in any access tier
		EgressPerGB:       0.09,
	},
	awsname.StorageClassStandardIA: {
		StoragePerGBMonth: 0.0125,   // TimedStorage-SIA-ByteHrs
		PutRequest:        0.00001,  // Requests-SIA-Tier1: $0.01 / 1,000
		GetRequest:        0.000001, // Requests-SIA-Tier2: $0.01 / 10,000
		ListRequest:       0.00001,
		RetrievalPerGB:    0.01, // Retrieval-SIA
		EgressPerGB:       0.09,
	},
	awsname.StorageClassOneZoneIA: {
		StoragePerGBMonth: 0.01,     // TimedStorage-ZIA-ByteHrs
		PutRequest:        0.00001,  // Requests-ZIA-Tier1
		GetRequest:        0.000001, // Requests-ZIA-Tier2
		ListRequest:       0.00001,
		RetrievalPerGB:    0.01, // Retrieval-ZIA
		EgressPerGB:       0.09,
	},
	awsname.StorageClassGlacierIR: {
		StoragePerGBMonth: 0.004,   // TimedStorage-GIR-ByteHrs
		PutRequest:        0.00002, // Requests-GIR-Tier1: $0.02 / 1,000
		GetRequest:        0.00001, // Requests-GIR-Tier2: $0.1 / 10,000
		ListRequest:       0.00002,
		RetrievalPerGB:    0.03, // Retrieval-GIR
		EgressPerGB:       0.09,
	},
	awsname.StorageClassGlacier: {
		StoragePerGBMonth: 0.0036,    // TimedStorage-GlacierByteHrs
		PutRequest:        0.00005,   // Requests-Tier3: $0.05 / 1,000 lifecycle requests
		GetRequest:        0.0000004, // Tier2 for the request itself; the bytes cost RetrievalPerGB
		ListRequest:       0.000005,  // LIST is billed at the Standard Tier1 rate
		RetrievalPerGB:    0.01,      // Standard-Retrieval-Bytes; Expedited 0.03, Bulk 0.0
		EgressPerGB:       0.09,
	},
	awsname.StorageClassDeepArchive: {
		StoragePerGBMonth: 0.00099, // TimedStorage-GDA-ByteHrs
		PutRequest:        0.00005, // Requests-Tier3
		GetRequest:        0.0000004,
		ListRequest:       0.000005,
		RetrievalPerGB:    0.02, // Standard retrieval; Bulk is 0.0025
		EgressPerGB:       0.09,
	},
	awsname.StorageClassReducedRedundancy: {
		// Deprecated by AWS and priced above Standard, which is why nothing should choose it. The
		// entry exists because awsname admits the class, and a tier with no rate would silently
		// cost zero — see TestEveryStorageClassHasARate.
		StoragePerGBMonth: 0.024,    // TimedStorage-RRS-ByteHrs, first band
		PutRequest:        0.000005, // RRS has no distinct request usagetype; billed as Tier1
		GetRequest:        0.0000004,
		ListRequest:       0.000005,
		RetrievalPerGB:    0.0,
		EgressPerGB:       0.09,
	},
}

// For returns the rate for a storage class and whether it was found.
//
// An unknown class yields the Standard rate with false. Returning the zero Rate would make every
// cost for it come out to zero, which reads as "free" rather than "unknown" — and a caller that
// ignores the bool would then report a confident zero. Standard is the most expensive commonly-used
// tier, so guessing it errs toward overstating a cost.
func For(storageClass string) (Rate, bool) {
	r, ok := rates[storageClass]
	if !ok {
		return rates[awsname.StorageClassStandard], false
	}

	return r, true
}

// All returns a copy of the whole table, keyed by storage class.
//
// A copy, because a caller that mutated the shared map would change what every other package
// believes an object costs.
func All() map[string]Rate {
	return maps.Clone(rates)
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
