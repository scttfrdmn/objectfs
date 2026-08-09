package s3

import (
	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// CostStats is what this mount has spent at AWS, as an exporter sees it.
//
// A purpose-built struct for the same reason [AccelerationStats] is one: the consumer is the metrics
// surface, and the choice of what to publish — along with every unit conversion — belongs here beside
// the rates rather than in internal/adapter. See [Backend.CostStats].
type CostStats struct {
	// Region is the region the rates were read for, which is not always the configured region. Publish
	// this rather than PricingConfig.Region: when the configured region has no published rates the
	// manager falls back to [awsrates.DefaultRegion], and a dollar figure labeled with the region that
	// was asked for instead of the one it was priced in is a figure that cannot be checked.
	Region string

	// Tier is the storage class this mount writes to, which decides every rate below. Two mounts of the
	// same bucket at different tiers have different per-request costs, so a cost series without this is
	// not interpretable across a fleet.
	Tier string

	// Request counts by pricing group, since construction. Free is the requests AWS bills nothing for.
	WriteRequests int64
	ListRequests  int64
	ReadRequests  int64
	FreeRequests  int64

	// BytesRetrieved is bytes off the wire, the quantity a retrieval fee is charged on.
	BytesRetrieved int64

	// RequestCost is the dollars the counted requests have cost, at this tier's per-request rates.
	RequestCost float64

	// RetrievalCost is the dollars BytesRetrieved has cost. Zero on STANDARD and INTELLIGENT_TIERING,
	// which have no retrieval fee — a zero here is a real answer and not a missing one.
	RetrievalCost float64

	// StoredBytes is the object bytes this mount has written, and StorageCostPerMonth what holding them
	// for a month costs at this tier's rate.
	//
	// Not the bucket's size. Nothing here lists the bucket — that would be a request per tick, billed,
	// to publish a metric — so this is what this process has uploaded since it started, which on a
	// long-lived mount of a large bucket is a small fraction of what is stored. It answers "what is this
	// mount adding to the bill", not "what does the bucket cost".
	StoredBytes         int64
	StorageCostPerMonth float64

	// RatePerWriteRequest and the two below it are the rates the costs above were computed at, published
	// so a dashboard can show the arithmetic rather than only its result. They are dollars per single
	// request, not per thousand: #209 was a per-1,000 figure stored as if it were per-request, and a
	// mount that publishes the rate it used makes that class of error visible in a scrape instead of
	// only in a bill.
	RatePerWriteRequest float64
	RatePerListRequest  float64
	RatePerReadRequest  float64
	RatePerGBRetrieved  float64
	RatePerGBMonth      float64
}

// CostStats returns what this mount has spent at AWS, priced at the current tier's rates.
//
// # What the figures are
//
// Costs incurred by *this process since it started*, at list prices for the first volume band with any
// configured discounts applied. Not a bill, and not a reconciliation of one: nothing here knows about
// the bucket's existing contents, other mounts, cross-region transfer, or the free tier. What it is
// good for is the question an operator actually asks — is this mount's access pattern expensive, and
// which part of it — and for that a figure that moves with the workload matters more than one that ties
// out to the invoice.
//
// # Why requests are counted at the SDK layer
//
// See [costTally]. Briefly: a 5 GB write is one PutObject to this package and 641 billable requests to
// AWS, so a count taken at the wrapper layer understates exactly the operations that cost the most.
//
// Every dollar figure is monotonic, because the counts it derives from are. That is deliberate: a cost
// series that can decrease cannot have a rate-of-change query written against it, and rate-of-change is
// the form every useful alert on this takes.
func (b *Backend) CostStats() CostStats {
	counts := b.clientManager.RequestCounts()
	tier := b.currentTier

	stats := CostStats{
		Region:         b.pricingManager.Region(),
		Tier:           tier,
		WriteRequests:  counts.Writes,
		ListRequests:   counts.Lists,
		ReadRequests:   counts.Reads,
		FreeRequests:   counts.Free,
		BytesRetrieved: counts.Retrieved,
		StoredBytes:    b.metricsCollector.GetMetrics().BytesUploaded,
	}

	// GetTierPricing's error is unreachable for any tier — it falls back to Standard's rates and warns —
	// but it is part of the signature, and a zeroed TierPricing here would publish a cost of exactly
	// zero for a mount doing real work. Reporting the counts with the rates left at zero is the honest
	// degradation: a dashboard showing thousands of requests at $0.00 reads as broken, which it is,
	// whereas a plausible small number reads as cheap.
	pricing, err := b.pricingManager.GetTierPricing(tier)
	if err != nil {
		b.logger.Warn("no pricing for the current storage tier; publishing request counts without costs",
			"tier", tier,
			"region", stats.Region,
			"error", err)

		return stats
	}

	stats.RatePerWriteRequest = pricing.RequestCosts.PutRequestCost
	stats.RatePerListRequest = pricing.RequestCosts.ListRequestCost
	stats.RatePerReadRequest = pricing.RequestCosts.GetRequestCost
	stats.RatePerGBRetrieved = pricing.RetrievalCostPerGB
	stats.RatePerGBMonth = pricing.StorageCostPerGBMonth

	stats.RequestCost = float64(counts.Writes)*stats.RatePerWriteRequest +
		float64(counts.Lists)*stats.RatePerListRequest +
		float64(counts.Reads)*stats.RatePerReadRequest

	// GBFromBytes, not a division by 1 << 30: AWS bills decimal GB, and the binary reading understates
	// every one of these figures by 7.4%.
	stats.RetrievalCost = awsrates.GBFromBytes(counts.Retrieved) * stats.RatePerGBRetrieved
	stats.StorageCostPerMonth = awsrates.GBFromBytes(stats.StoredBytes) * stats.RatePerGBMonth

	return stats
}
