// Package cost provides real-time per-operation S3 cost calculation, per-tenant
// accumulation, ROI reporting, and budget-threshold alerting.
package cost

// S3 storage tier identifiers — mirror constants in internal/storage/s3/tiers.go.
const (
	TierStandard    = "STANDARD"
	TierStandardIA  = "STANDARD_IA"
	TierOneZoneIA   = "ONEZONE_IA"
	TierGlacierIR   = "GLACIER_IR"
	TierGlacier     = "GLACIER"
	TierDeepArchive = "DEEP_ARCHIVE"
	TierIntelligent = "INTELLIGENT_TIERING"
)

// Price holds per-tier pricing in USD.
// All per-request costs are for a single API call (not per-1000).
type Price struct {
	// StoragePerGBMonth is the cost to store 1 GB for one calendar month.
	StoragePerGBMonth float64

	// GetRequest is the cost per GET, SELECT, or HEAD API call.
	GetRequest float64

	// PutRequest is the cost per PUT, COPY, POST, or INITIATE-MULTIPART call.
	PutRequest float64

	// ListRequest is the cost per LIST API call.
	ListRequest float64

	// RetrievalPerGB is the per-GB retrieval fee for reads from IA/Glacier tiers.
	// Zero for Standard and Intelligent-Tiering.
	RetrievalPerGB float64

	// EgressPerGB is the per-GB transfer-out-to-internet fee.
	// Set to 0 when traffic stays within the same AWS region/account.
	EgressPerGB float64
}

// DefaultPrices contains built-in pricing calibrated to AWS us-east-1 rates (2026).
// These match the values in internal/storage/s3/tiers.go and pricing_manager.go.
var DefaultPrices = map[string]Price{
	TierStandard: {
		StoragePerGBMonth: 0.023,
		GetRequest:        0.0000004, // $0.0004 / 1000
		PutRequest:        0.000005,  // $0.005 / 1000
		ListRequest:       0.000005,
		RetrievalPerGB:    0.0,
		EgressPerGB:       0.09,
	},
	TierStandardIA: {
		StoragePerGBMonth: 0.0125,
		GetRequest:        0.000001, // $0.001 / 1000
		PutRequest:        0.00001,  // $0.01 / 1000
		ListRequest:       0.000005,
		RetrievalPerGB:    0.01,
		EgressPerGB:       0.09,
	},
	TierOneZoneIA: {
		StoragePerGBMonth: 0.01,
		GetRequest:        0.000001,
		PutRequest:        0.00001,
		ListRequest:       0.000005,
		RetrievalPerGB:    0.01,
		EgressPerGB:       0.09,
	},
	TierGlacierIR: {
		StoragePerGBMonth: 0.004,
		GetRequest:        0.000002, // $0.002 / 1000
		PutRequest:        0.000005,
		ListRequest:       0.000005,
		RetrievalPerGB:    0.03,
		EgressPerGB:       0.09,
	},
	TierGlacier: {
		StoragePerGBMonth: 0.0036,
		GetRequest:        0.0000004,
		PutRequest:        0.000005, // POST: $0.05 / 1000 for archive operations
		ListRequest:       0.000005,
		RetrievalPerGB:    0.02, // Expedited: $0.03, Standard: $0.01, Bulk: $0.0025
		EgressPerGB:       0.09,
	},
	TierDeepArchive: {
		StoragePerGBMonth: 0.00099,
		GetRequest:        0.0000004,
		PutRequest:        0.000005,
		ListRequest:       0.000005,
		RetrievalPerGB:    0.02, // Standard: $0.02/GB
		EgressPerGB:       0.09,
	},
	TierIntelligent: {
		StoragePerGBMonth: 0.023, // frequent tier rate (auto-moves to IA at 0.0125)
		GetRequest:        0.0000004,
		PutRequest:        0.000005,
		ListRequest:       0.000005,
		RetrievalPerGB:    0.0,
		EgressPerGB:       0.09,
	},
}

// PriceTable is an immutable pricing lookup with optional per-tier overrides.
type PriceTable struct {
	prices map[string]Price
}

// NewPriceTable creates a PriceTable starting from DefaultPrices, then applying
// any overrides provided.  Pass nil or an empty map to use defaults as-is.
func NewPriceTable(overrides map[string]Price) *PriceTable {
	merged := make(map[string]Price, len(DefaultPrices))
	for k, v := range DefaultPrices {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
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
