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
	Name string `json:"name"`

	// MinObjectSize is AWS's minimum *billable* object size: an object smaller than this is billed
	// as though it were this size. Zero where AWS publishes none, which is five of the eight classes.
	//
	// Only three classes have one — STANDARD_IA, ONEZONE_IA, and GLACIER_IR, all at 128 KB. This
	// field previously also carried 128 KB for INTELLIGENT_TIERING and 40 KB for GLACIER and
	// DEEP_ARCHIVE, and neither of those is a minimum billable size; see PerObjectOverheadBytes and
	// MonitoringEligibilityBytes below for what those two numbers actually are. AWS's storage class
	// comparison table lists "Min billable object size" as None for Intelligent-Tiering and NA for
	// both Glacier Flexible Retrieval and Deep Archive:
	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/storage-class-intro.html#sc-compare
	//
	// A floor and an overhead point opposite ways for anything that reasons about small objects,
	// which is why they cannot share a field. Under a floor, compressing a 30 KB object to 10 KB
	// saves nothing. Under an overhead, it saves 20 KB and the surcharge is unaffected.
	MinObjectSize int64 `json:"min_object_size"`

	// PerObjectOverheadBytes is storage AWS bills per object in addition to the object itself, zero
	// where there is none.
	//
	// Only the two archive classes have it, at 40 KB, and it is not billed at one rate: 32 KB is
	// charged at the archive class's own rate for the index and metadata Glacier maintains, and 8 KB
	// at the S3 Standard rate for the name and metadata S3 keeps so the object can be listed. The
	// split is why this is a poor fit for a single number, and callers that only need a size can use
	// the sum; ArchiveOverhead returns the two parts for callers that price them.
	//
	// It dominates for small objects: a 10 KB object on DEEP_ARCHIVE is billed for 10 KB of payload
	// plus 40 KB of overhead, so roughly 23× the payload's cost once the two rates are applied.
	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/storage-class-intro.html#sc-glacier
	PerObjectOverheadBytes int64 `json:"per_object_overhead_bytes"`

	// MonitoringEligibilityBytes is the size below which INTELLIGENT_TIERING does not monitor an
	// object, zero for every other class.
	//
	// 128 KB, the same number as the IA classes' billable minimum, which is very likely how it came
	// to be stored in MinObjectSize with the comment "128 KB minimum for optimization". It is not a
	// billing floor in either direction: an object below it is billed for its real size, is not
	// charged the per-object monitoring and automation fee, and stays in the Frequent Access tier
	// permanently rather than being auto-tiered. So it is a statement about what the tier will *do*
	// with an object, not about what the object costs.
	MonitoringEligibilityBytes int64 `json:"monitoring_eligibility_bytes"`

	DeletionEmbargo    time.Duration `json:"deletion_embargo"`
	RetrievalLatency   string        `json:"retrieval_latency"`
	RetrievalCost      bool          `json:"retrieval_cost"`
	MinimumStorageDays int           `json:"minimum_storage_days"`
	RecommendedUseCase string        `json:"recommended_use_case"`

	// CostPerGBMonth is the us-east-1 list price in USD, filled in from [awsrates] rather than
	// written in the literal below. See withRates.
	CostPerGBMonth float64 `json:"cost_per_gb_month"`
}

// Sizes AWS states in KB, spelled in binary KiB.
//
// AWS writes "128 KB" and "40 KB" in the pricing pages and the storage class table, and does not say
// which of the two a KB is. These constants use 1024, which makes 128 KB 131,072 bytes against a
// decimal reading of 128,000 — a 2.4% difference, in the direction that overstates the floor and the
// overhead, so every recommendation derived from them is conservative rather than optimistic.
//
// The choice is explicit here rather than left as a bare `128 * 1024` because the repository has been
// wrong about exactly this before, in the other direction and more expensively: internal/cost divided
// bytes by 2^30 to get GB with a comment asserting the binary reading was correct, and AWS bills
// GB-months in decimal GB, so every storage figure was 7.4% low. See [awsrates.GBFromBytes]. Rates
// are unambiguously decimal; these thresholds are not documented either way, which is why they get a
// stated assumption instead of a silent one.
const (
	// kib is 1024 bytes. Named to make the assumption visible at each use.
	kib = 1024

	// minBillableSize128KB applies to STANDARD_IA, ONEZONE_IA, and GLACIER_IR.
	minBillableSize128KB = 128 * kib

	// archiveOverheadGlacierKB is the 32 KB of per-object index and metadata GLACIER and
	// DEEP_ARCHIVE bill at their own rate.
	archiveOverheadGlacierKB = 32 * kib

	// archiveOverheadStandardKB is the 8 KB of per-object name and metadata the archive classes bill
	// at the S3 Standard rate, so the object stays listable.
	archiveOverheadStandardKB = 8 * kib

	// monitoringEligibility128KB is the size below which INTELLIGENT_TIERING leaves an object in
	// Frequent Access, unmonitored and not charged the automation fee.
	monitoringEligibility128KB = 128 * kib
)

// ArchiveOverhead returns the per-object overhead for a tier, split by the rate each part is billed
// at: archiveBytes at the tier's own rate, standardBytes at the S3 Standard rate.
//
// Both are zero for every class but GLACIER and DEEP_ARCHIVE. The split is exposed because pricing
// the 40 KB at one rate is wrong by about a factor of six on the 8 KB portion — Standard is $0.023
// per GB-month against Deep Archive's $0.00099 — so a caller that sums first and prices second gets
// the cheaper answer for the more expensive part.
func ArchiveOverhead(tier string) (archiveBytes, standardBytes int64) {
	info, ok := StorageTiers[tier]
	if !ok || info.PerObjectOverheadBytes == 0 {
		return 0, 0
	}

	return archiveOverheadGlacierKB, archiveOverheadStandardKB
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
		MinObjectSize:      minBillableSize128KB, // AWS publishes a 128 KB billable minimum
		DeletionEmbargo:    30 * 24 * time.Hour,  // 30 days minimum storage
		RetrievalLatency:   "instant",
		RetrievalCost:      true, // $0.01 per GB retrieval cost
		MinimumStorageDays: 30,
		RecommendedUseCase: "Infrequently accessed data that needs instant access",
	},
	TierOneZoneIA: {
		Name:               "One Zone-Infrequent Access",
		MinObjectSize:      minBillableSize128KB, // AWS publishes a 128 KB billable minimum
		DeletionEmbargo:    30 * 24 * time.Hour,  // 30 days minimum storage
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
		MinObjectSize:      minBillableSize128KB, // AWS publishes a 128 KB billable minimum
		DeletionEmbargo:    90 * 24 * time.Hour,  // 90 days minimum storage
		RetrievalLatency:   "instant",
		RetrievalCost:      true, // $0.03 per GB retrieval cost
		MinimumStorageDays: 90,
		RecommendedUseCase: "Archive data needing instant access",
	},
	TierGlacier: {
		Name: "Glacier Flexible Retrieval",
		// No billable minimum — AWS's storage class table says NA. The 40 KB this field used to hold
		// is per-object overhead, which is added to the object's size rather than raised to.
		MinObjectSize:          0,
		PerObjectOverheadBytes: archiveOverheadGlacierKB + archiveOverheadStandardKB,
		DeletionEmbargo:        90 * 24 * time.Hour, // 90 days minimum storage
		RetrievalLatency:       "minutes-hours",
		RetrievalCost:          true, // Variable retrieval costs
		MinimumStorageDays:     90,
		RecommendedUseCase:     "Long-term archive with flexible retrieval",
	},
	TierDeepArchive: {
		Name: "Glacier Deep Archive",
		// No billable minimum; same 40 KB per-object overhead as GLACIER. It matters more here,
		// because the storage rate is the cheapest AWS offers and the overhead is not discounted with
		// it: at $0.00099/GB-month for the payload, a 10 KB object costs about 23× its own size once
		// the 32 KB archive-rate and 8 KB Standard-rate portions are added.
		MinObjectSize:          0,
		PerObjectOverheadBytes: archiveOverheadGlacierKB + archiveOverheadStandardKB,
		DeletionEmbargo:        180 * 24 * time.Hour, // 180 days minimum storage
		RetrievalLatency:       "hours",
		RetrievalCost:          true, // Variable retrieval costs
		MinimumStorageDays:     180,
		RecommendedUseCase:     "Long-term archive rarely accessed",
	},
	TierIntelligent: {
		Name: "Intelligent Tiering",
		// No billable minimum — AWS's table says None. The 128 KB this field used to hold governs
		// whether an object is monitored and auto-tiered, not what it is billed.
		MinObjectSize:              0,
		MonitoringEligibilityBytes: monitoringEligibility128KB,
		DeletionEmbargo:            0,
		RetrievalLatency:           "variable",
		RetrievalCost:              false, // No retrieval charges
		MinimumStorageDays:         0,
		RecommendedUseCase:         "Automatic cost optimization for changing access patterns",
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

// ValidateWrite validates a write operation against tier constraints.
//
// Two different things are checked against a size here, and only one of them can refuse the write.
//
// AWS's per-tier minimum is a *billing* floor: S3 accepts a 1-byte STANDARD_IA object and bills it
// as 128 KiB. It never rejects the write. Refusing it here was therefore ObjectFS policy dressed as
// S3 behavior, and it made the filesystem unusable rather than expensive — `internal/fuse` creates
// both directory markers and new files by PUTting zero bytes, so on any tier with a minimum every
// `mkdir` and every `touch` failed, including the ones an IA-tier test needs to set itself up. It is
// a warning now, reporting the size that will be billed alongside the size that was written (#154).
//
// `TierConstraints.MinObjectSize` is the one that still errors. An operator who sets it has asked
// for a floor that is not AWS's, and a policy nobody configured is the only kind worth removing.
// Note the consequence of that split, because it is easy to configure by accident: setting the
// constraint to the tier's own published minimum restores the old behavior in full, zero-byte
// directory markers included.
//
// What #229 fixed is the input to all of this: the gate previously refused writes under 40 KB to
// GLACIER and DEEP_ARCHIVE and under 128 KB to INTELLIGENT_TIERING, and AWS publishes no minimum
// billable size for any of those three — so it was rejecting writes on the strength of numbers that
// were not minimums at all. Those three now have no minimum, and the two archive classes warn about
// their per-object overhead instead, which is the real cost and points the other way.
func (tv *TierValidator) ValidateWrite(key string, dataSize int64) error {
	// The operator's floor, if there is one: a deliberate policy, still enforced.
	if minSize := tv.constraints.MinObjectSize; minSize > 0 && dataSize < minSize {
		return fmt.Errorf("object size %d bytes is below the configured minimum of %d bytes for the "+
			"%s tier (tier_constraints.min_object_size); AWS would accept this write and bill it as "+
			"%d bytes, so this refusal is ObjectFS policy — unset that key to allow it",
			dataSize, minSize, tv.tier, minSize)
	}

	// AWS's billing floor: not a reason to refuse, but worth saying out loud, because the object
	// costs a multiple of what its size suggests and nothing downstream will mention it again.
	if minBillable := tv.tierInfo.MinObjectSize; minBillable > 0 && dataSize < minBillable {
		tv.logger.Warn("object is smaller than this tier's minimum billable size",
			"tier", tv.tier,
			"key", key,
			"size", dataSize,
			"billed_size", minBillable,
			"note", "AWS accepts the write and bills it as the minimum; a smaller object on this "+
				"tier costs the same as one at the minimum, so many small objects here are more "+
				"expensive than the same bytes on STANDARD")
	}

	// The archive classes bill 40 KB of metadata per object regardless of its size, so a small object
	// costs a multiple of what its bytes suggest. Worth saying at the point of the write, because the
	// remedy — pack small files into an archive before storing them — is not available afterward.
	if overhead := tv.tierInfo.PerObjectOverheadBytes; overhead > 0 && dataSize < overhead {
		tv.logger.Warn("object is smaller than the per-object overhead this tier bills",
			"tier", tv.tier,
			"key", key,
			"size", dataSize,
			"per_object_overhead", overhead,
			"note", "AWS bills 32 KB at the archive rate and 8 KB at the S3 Standard rate for every "+
				"archived object, in addition to the object; packing small files into one archive "+
				"avoids paying it per file")
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
	if objectSize < minBillableSize128KB {
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
