package s3

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/scttfrdmn/cargoship/pkg/aws/config"
)

// S3 Storage Tier Constants
const (
	TierStandard          = "STANDARD"
	TierStandardIA        = "STANDARD_IA"
	TierOneZoneIA         = "ONEZONE_IA"
	TierReducedRedundancy = "REDUCED_REDUNDANCY"
	TierGlacierIR         = "GLACIER_IR"
	TierGlacier           = "GLACIER"
	TierDeepArchive       = "DEEP_ARCHIVE"
	TierIntelligent       = "INTELLIGENT_TIERING"
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
	CostPerGBMonth     float64       `json:"cost_per_gb_month"` // Approximate cost in USD
}

// Predefined storage tier information with AWS constraints
var StorageTiers = map[string]StorageTierInfo{
	TierStandard: {
		Name:               "Standard",
		MinObjectSize:      0,
		DeletionEmbargo:    0,
		RetrievalLatency:   "instant",
		RetrievalCost:      false,
		MinimumStorageDays: 0,
		RecommendedUseCase: "Frequently accessed data",
		CostPerGBMonth:     0.023, // Approximate USD
	},
	TierStandardIA: {
		Name:               "Standard-Infrequent Access",
		MinObjectSize:      128 * 1024,          // 128 KB minimum
		DeletionEmbargo:    30 * 24 * time.Hour, // 30 days minimum storage
		RetrievalLatency:   "instant",
		RetrievalCost:      true, // $0.01 per GB retrieval cost
		MinimumStorageDays: 30,
		RecommendedUseCase: "Infrequently accessed data that needs instant access",
		CostPerGBMonth:     0.0125,
	},
	TierOneZoneIA: {
		Name:               "One Zone-Infrequent Access",
		MinObjectSize:      128 * 1024,          // 128 KB minimum
		DeletionEmbargo:    30 * 24 * time.Hour, // 30 days minimum storage
		RetrievalLatency:   "instant",
		RetrievalCost:      true, // $0.01 per GB retrieval cost
		MinimumStorageDays: 30,
		RecommendedUseCase: "Infrequently accessed data in single AZ",
		CostPerGBMonth:     0.01,
	},
	TierReducedRedundancy: {
		Name:               "Reduced Redundancy",
		MinObjectSize:      0,
		DeletionEmbargo:    0,
		RetrievalLatency:   "instant",
		RetrievalCost:      false,
		MinimumStorageDays: 0,
		RecommendedUseCase: "Non-critical, reproducible data (deprecated)",
		CostPerGBMonth:     0.024,
	},
	TierGlacierIR: {
		Name:               "Glacier Instant Retrieval",
		MinObjectSize:      128 * 1024,          // 128 KB minimum
		DeletionEmbargo:    90 * 24 * time.Hour, // 90 days minimum storage
		RetrievalLatency:   "instant",
		RetrievalCost:      true, // $0.03 per GB retrieval cost
		MinimumStorageDays: 90,
		RecommendedUseCase: "Archive data needing instant access",
		CostPerGBMonth:     0.004,
	},
	TierGlacier: {
		Name:               "Glacier Flexible Retrieval",
		MinObjectSize:      40 * 1024,           // 40 KB minimum
		DeletionEmbargo:    90 * 24 * time.Hour, // 90 days minimum storage
		RetrievalLatency:   "minutes-hours",
		RetrievalCost:      true, // Variable retrieval costs
		MinimumStorageDays: 90,
		RecommendedUseCase: "Long-term archive with flexible retrieval",
		CostPerGBMonth:     0.0036,
	},
	TierDeepArchive: {
		Name:               "Glacier Deep Archive",
		MinObjectSize:      40 * 1024,            // 40 KB minimum
		DeletionEmbargo:    180 * 24 * time.Hour, // 180 days minimum storage
		RetrievalLatency:   "hours",
		RetrievalCost:      true, // Variable retrieval costs
		MinimumStorageDays: 180,
		RecommendedUseCase: "Long-term archive rarely accessed",
		CostPerGBMonth:     0.00099,
	},
	TierIntelligent: {
		Name:               "Intelligent Tiering",
		MinObjectSize:      128 * 1024, // 128 KB minimum for optimization
		DeletionEmbargo:    0,
		RetrievalLatency:   "variable",
		RetrievalCost:      false, // No retrieval charges
		MinimumStorageDays: 0,
		RecommendedUseCase: "Automatic cost optimization for changing access patterns",
		CostPerGBMonth:     0.023, // Plus monitoring charges
	},
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
