package s3

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/scttfrdmn/cargoship/pkg/aws/config"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// S3 Storage Tier Constants.
//
// These are aliases of internal/awsname's storage classes rather than independent string literals.
// The tier is named in configuration, validated at load by internal/config, and acted on here — and
// internal/config cannot import this package (see the awsname package comment for the cycle). So the
// names have to live somewhere both sides reach, and if they were spelled twice the two spellings
// could disagree: a tier this table knows about but the loader rejects, or worse, one the loader
// accepts and this table has no entry for. TestStorageTiersCoversEveryStorageClass pins the other
// direction — that every class awsname admits has a billing entry here.
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

// Access Pattern Constants
const (
	AccessFrequent   = "frequent"
	AccessInfrequent = "infrequent"
	AccessArchive    = "archive"
)

// StorageTierInfo contains tier-specific information and constraints
type StorageTierInfo struct {
	Name               string        `json:"name"`
	MinObjectSize      int64         `json:"min_object_size"`
	DeletionEmbargo    time.Duration `json:"deletion_embargo"`
	RetrievalLatency   string        `json:"retrieval_latency"`
	RetrievalCost      bool          `json:"retrieval_cost"`
	MinimumStorageDays int           `json:"minimum_storage_days"`
	RecommendedUseCase string        `json:"recommended_use_case"`

	// CostPerGBMonth is the us-east-1 list price in USD, filled in from [awsrates] rather than
	// written in the literal below. See withRates.
	CostPerGBMonth float64 `json:"cost_per_gb_month"`
}

// StorageTiers holds the per-tier constraints and list price for every S3 storage class.
//
// The literal carries the tier *constraints* — minimum size, embargo, retrieval latency — which are
// S3 behavior and belong here. It deliberately does not carry CostPerGBMonth: rates are the one
// thing this table used to state for itself, and stating them here made it the third of five copies
// of the S3 rate card in this repo. Two of the five disagreed by a factor of ten, so what a write
// cost depended on which package the caller reached for. withRates reads each rate from
// [awsrates] instead, which is the only table checked against the live AWS Pricing API.
var StorageTiers = withRates(map[string]StorageTierInfo{
	TierStandard: {
		Name:               "Standard",
		MinObjectSize:      0,
		DeletionEmbargo:    0,
		RetrievalLatency:   "instant",
		RetrievalCost:      false,
		MinimumStorageDays: 0,
		RecommendedUseCase: "Frequently accessed data",
	},
	TierStandardIA: {
		Name:               "Standard-Infrequent Access",
		MinObjectSize:      128 * 1024,          // 128 KB minimum
		DeletionEmbargo:    30 * 24 * time.Hour, // 30 days minimum storage
		RetrievalLatency:   "instant",
		RetrievalCost:      true, // $0.01 per GB retrieval cost
		MinimumStorageDays: 30,
		RecommendedUseCase: "Infrequently accessed data that needs instant access",
	},
	TierOneZoneIA: {
		Name:               "One Zone-Infrequent Access",
		MinObjectSize:      128 * 1024,          // 128 KB minimum
		DeletionEmbargo:    30 * 24 * time.Hour, // 30 days minimum storage
		RetrievalLatency:   "instant",
		RetrievalCost:      true, // $0.01 per GB retrieval cost
		MinimumStorageDays: 30,
		RecommendedUseCase: "Infrequently accessed data in single AZ",
	},
	TierReducedRedundancy: {
		Name:               "Reduced Redundancy",
		MinObjectSize:      0,
		DeletionEmbargo:    0,
		RetrievalLatency:   "instant",
		RetrievalCost:      false,
		MinimumStorageDays: 0,
		RecommendedUseCase: "Non-critical, reproducible data (deprecated)",
	},
	TierGlacierIR: {
		Name:               "Glacier Instant Retrieval",
		MinObjectSize:      128 * 1024,          // 128 KB minimum
		DeletionEmbargo:    90 * 24 * time.Hour, // 90 days minimum storage
		RetrievalLatency:   "instant",
		RetrievalCost:      true, // $0.03 per GB retrieval cost
		MinimumStorageDays: 90,
		RecommendedUseCase: "Archive data needing instant access",
	},
	TierGlacier: {
		Name:               "Glacier Flexible Retrieval",
		MinObjectSize:      40 * 1024,           // 40 KB minimum
		DeletionEmbargo:    90 * 24 * time.Hour, // 90 days minimum storage
		RetrievalLatency:   "minutes-hours",
		RetrievalCost:      true, // Variable retrieval costs
		MinimumStorageDays: 90,
		RecommendedUseCase: "Long-term archive with flexible retrieval",
	},
	TierDeepArchive: {
		Name:               "Glacier Deep Archive",
		MinObjectSize:      40 * 1024,            // 40 KB minimum
		DeletionEmbargo:    180 * 24 * time.Hour, // 180 days minimum storage
		RetrievalLatency:   "hours",
		RetrievalCost:      true, // Variable retrieval costs
		MinimumStorageDays: 180,
		RecommendedUseCase: "Long-term archive rarely accessed",
	},
	TierIntelligent: {
		Name:               "Intelligent Tiering",
		MinObjectSize:      128 * 1024, // 128 KB minimum for optimization
		DeletionEmbargo:    0,
		RetrievalLatency:   "variable",
		RetrievalCost:      false, // No retrieval charges
		MinimumStorageDays: 0,
		RecommendedUseCase: "Automatic cost optimization for changing access patterns",
	},
})

// withRates fills in CostPerGBMonth for every tier from [awsrates], and panics if any tier has no
// rate there.
//
// The panic is deliberate and it is at init time, which is the only point where this can be a build
// failure rather than a wrong number in a cost report. The alternative — leaving the field zero —
// prices that tier at $0/GB-month, and a cost estimate of zero does not look like a missing entry.
// It looks like free storage, which is a plausible-enough answer to survive review. awsrates covers
// every class in awsname.StorageClasses and TestStorageTiersCoversEveryStorageClass pins this map to
// the same set, so reaching the panic means one of those two invariants has just been broken by the
// change being compiled.
func withRates(tiers map[string]StorageTierInfo) map[string]StorageTierInfo {
	for class, info := range tiers {
		rate, ok := awsrates.For(class)
		if !ok {
			panic(fmt.Sprintf("s3: StorageTiers has an entry for %q but internal/awsrates has no rate "+
				"for it; add one there rather than writing a literal here, or every cost reported for "+
				"this tier is $0", class))
		}

		info.CostPerGBMonth = rate.StoragePerGBMonth
		tiers[class] = info
	}

	return tiers
}

// TierValidator validates operations against storage tier constraints
type TierValidator struct {
	tier        string
	constraints TierConstraints
	tierInfo    StorageTierInfo
	logger      *slog.Logger
}

// NewTierValidator creates a new tier validator
func NewTierValidator(tier string, constraints TierConstraints, logger *slog.Logger) *TierValidator {
	tierInfo, exists := StorageTiers[tier]
	if !exists {
		// Default to Standard tier if unknown
		tierInfo = StorageTiers[TierStandard]
		tier = TierStandard
	}

	return &TierValidator{
		tier:        tier,
		constraints: constraints,
		tierInfo:    tierInfo,
		logger:      logger,
	}
}

// ValidateWrite validates a write operation against tier constraints
func (tv *TierValidator) ValidateWrite(key string, dataSize int64) error {
	// Check minimum object size constraint
	minSize := tv.tierInfo.MinObjectSize
	if tv.constraints.MinObjectSize > 0 {
		minSize = tv.constraints.MinObjectSize
	}

	if dataSize < minSize {
		return fmt.Errorf("object size %d bytes is below minimum %d bytes for %s tier",
			dataSize, minSize, tv.tier)
	}

	// Log tier-specific warnings
	if tv.tierInfo.RetrievalCost {
		tv.logger.Debug("Writing to tier with retrieval costs",
			"tier", tv.tier,
			"key", key,
			"size", dataSize)
	}

	return nil
}

// ValidateDelete validates a delete operation against tier constraints
func (tv *TierValidator) ValidateDelete(key string, objectAge time.Duration) error {
	// Check deletion embargo
	embargo := tv.tierInfo.DeletionEmbargo
	if tv.constraints.DeletionEmbargo > 0 {
		embargo = tv.constraints.DeletionEmbargo
	}

	if embargo > 0 && objectAge < embargo {
		return fmt.Errorf("object %s cannot be deleted before %v (current age: %v) due to %s tier constraints",
			key, embargo, objectAge, tv.tier)
	}

	// Warn about minimum storage charges
	if tv.tierInfo.MinimumStorageDays > 0 && objectAge < time.Duration(tv.tierInfo.MinimumStorageDays)*24*time.Hour {
		tv.logger.Warn("Deleting object before minimum storage period - charges may still apply",
			"tier", tv.tier,
			"key", key,
			"age", objectAge,
			"minimum_days", tv.tierInfo.MinimumStorageDays)
	}

	return nil
}

// GetTierInfo returns information about the current tier
func (tv *TierValidator) GetTierInfo() StorageTierInfo {
	return tv.tierInfo
}

// GetRecommendations returns tier recommendations based on access patterns
func (tv *TierValidator) GetRecommendations(objectSize int64, accessFrequency string) []string {
	recommendations := make([]string, 0, 3)

	// Size-based recommendations
	if objectSize < 128*1024 {
		recommendations = append(recommendations, "Consider Standard tier for small objects to avoid IA minimum charges")
	}

	// Access pattern recommendations
	switch accessFrequency {
	case AccessFrequent:
		if tv.tier != TierStandard {
			recommendations = append(recommendations, "Consider Standard tier for frequently accessed data")
		}
	case AccessInfrequent:
		if tv.tier == TierStandard {
			recommendations = append(recommendations, "Consider Standard-IA or One Zone-IA for cost savings")
		}
	case AccessArchive:
		if tv.tier != TierGlacierIR && tv.tier != TierGlacier {
			recommendations = append(recommendations, "Consider Glacier tiers for archive data")
		}
	case "unknown":
		if tv.tier != TierIntelligent {
			recommendations = append(recommendations, "Consider Intelligent Tiering for unknown access patterns")
		}
	}

	return recommendations
}

// ConvertTierToStorageClass converts our tier constants to AWS SDK storage class types
func ConvertTierToStorageClass(tier string) types.StorageClass {
	switch tier {
	case TierStandard:
		return types.StorageClassStandard
	case TierStandardIA:
		return types.StorageClassStandardIa
	case TierOneZoneIA:
		return types.StorageClassOnezoneIa
	case TierReducedRedundancy:
		return types.StorageClassReducedRedundancy
	case TierGlacierIR:
		return types.StorageClassGlacierIr
	case TierGlacier:
		return types.StorageClassGlacier
	case TierDeepArchive:
		return types.StorageClassDeepArchive
	case TierIntelligent:
		return types.StorageClassIntelligentTiering
	default:
		return types.StorageClassStandard
	}
}

// ConvertTierToCargoShipStorageClass converts our tier constants to CargoShip storage class types
func ConvertTierToCargoShipStorageClass(tier string) config.StorageClass {
	switch tier {
	case TierStandard:
		return config.StorageClassStandard
	case TierStandardIA:
		return config.StorageClassStandardIA
	case TierOneZoneIA:
		return config.StorageClassOneZoneIA
	case TierReducedRedundancy:
		return config.StorageClassStandard // Fallback to Standard (deprecated tier)
	case TierGlacierIR:
		return config.StorageClassGlacier // Use Glacier for instant retrieval (CargoShip limitation)
	case TierGlacier:
		return config.StorageClassGlacier
	case TierDeepArchive:
		return config.StorageClassDeepArchive
	case TierIntelligent:
		return config.StorageClassIntelligentTiering
	default:
		return config.StorageClassStandard
	}
}
