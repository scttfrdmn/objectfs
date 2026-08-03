package s3

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/scttfrdmn/objectfs/internal/awsrates"
	"github.com/scttfrdmn/objectfs/pkg/utils"
)

// Currency Constants
const (
	DefaultCurrency = "USD"
)

// PricingManager resolves S3 tier pricing from a built-in static rate table,
// applying operator-supplied overrides and discounts.
//
// Rates are us-east-1 list prices read from [awsrates], the one table in this repo checked against
// the live AWS Pricing API. They suit comparing tiers, not billing reconciliation: they are first
// volume band, they assume Standard retrieval speed on the Glacier classes, and PricingConfig.Region
// does not select them. Set PricingConfig.CustomPricing for exact, negotiated, or non-us-east-1
// rates.
type PricingManager struct {
	config      PricingConfig
	logger      *slog.Logger
	lastUpdated time.Time
}

// NewPricingManager creates a new pricing manager
func NewPricingManager(config PricingConfig, logger *slog.Logger) *PricingManager {
	if config.Currency == "" {
		config.Currency = DefaultCurrency
	}
	if config.Region == "" {
		config.Region = "us-east-1" // Default pricing region
	}

	if config.UsePricingAPI {
		logger.Warn("pricing_config.use_pricing_api is deprecated and ignored; " +
			"pricing is served from a built-in static rate table. " +
			"Set pricing_config.custom_pricing for exact or negotiated rates.")
	}

	// Load external discount config if specified
	if config.DiscountConfigFile != "" {
		externalDiscountConfig, err := loadDiscountConfigFromFile(config.DiscountConfigFile, logger)
		if err != nil {
			logger.Warn("Failed to load external discount config file, using inline config",
				"file", config.DiscountConfigFile, "error", err)
		} else {
			// Merge external config with inline config (external takes precedence)
			config.DiscountConfig = mergeDiscountConfigs(config.DiscountConfig, externalDiscountConfig)
			logger.Info("Loaded external discount configuration", "file", config.DiscountConfigFile)
		}
	}

	return &PricingManager{
		config: config,
		logger: logger,
	}
}

// GetTierPricing returns pricing for a specific tier with discounts applied
func (pm *PricingManager) GetTierPricing(tier string) (TierPricing, error) {
	// Check for custom pricing override first
	if customPricing, exists := pm.config.CustomPricing[tier]; exists {
		pm.logger.Debug("Using custom pricing override", "tier", tier)
		return pm.applyDiscounts(tier, customPricing), nil
	}

	// Apply discounts to the built-in rate table.
	return pm.applyDiscounts(tier, pm.getDefaultPricing(tier)), nil
}

// getDefaultPricing returns the built-in rate table for a tier.
//
// Every rate comes from [awsrates], which is verified against the live AWS Pricing API. They are
// us-east-1 list prices for the first volume band, intended for relative cost comparison between
// tiers rather than for billing reconciliation — an operator with negotiated rates, or outside
// us-east-1, should set CustomPricing.
//
// This function used to hold its own copies of the request and retrieval rates, in per-1,000 units
// that it divided down at return. That is where #209's defect lived: Standard PUT was written as
// 0.0005, which is a tenth of the $0.005-per-1,000 AWS charges, so every write on the default
// configuration was costed at 10% of its price. internal/cost/pricing.go had the same rate right,
// which meant the answer depended on which package the caller asked. Reading from one table in
// per-request units removes both the duplicate and the conversion that went wrong.
func (pm *PricingManager) getDefaultPricing(tier string) TierPricing {
	// StorageTiers supplies the tier *constraints*; awsrates supplies the money. Both fall back to
	// Standard for an unknown tier, and awsrates.For reports that it did.
	tierInfo, exists := StorageTiers[tier]
	if !exists {
		tierInfo = StorageTiers[TierStandard]
	}

	rate, known := awsrates.For(tier)
	if !known {
		pm.logger.Warn("no rate for storage tier; reporting Standard's prices for it",
			"tier", tier,
			"fallback", TierStandard)
	}

	return TierPricing{
		StorageCostPerGBMonth: rate.StoragePerGBMonth,
		RetrievalCostPerGB:    rate.RetrievalPerGB,
		RequestCosts: RequestCosts{
			PutRequestCost: rate.PutRequest,
			GetRequestCost: rate.GetRequest,
			// DELETE is free on S3, on every storage class.
			DeleteRequestCost: 0.0,
			ListRequestCost:   rate.ListRequest,
			// HEAD bills in the same Tier2 request group as GET.
			HeadRequestCost: rate.GetRequest,
		},
		MinimumBillableSize: tierInfo.MinObjectSize,
		// Carried separately from the minimum because it is added to the object's size rather than
		// substituted for it. See TierPricing's field comments; the two were one field until #229.
		PerObjectOverheadBytes:    tierInfo.PerObjectOverheadBytes,
		OverheadStandardRateBytes: overheadStandardRateBytes(tier),
		MinimumBillableDays:       tierInfo.MinimumStorageDays,
		TransitionCosts:           make(map[string]float64),
	}
}

// overheadStandardRateBytes is the part of a tier's per-object overhead billed at the S3 Standard
// rate, zero for every tier that has no overhead.
func overheadStandardRateBytes(tier string) int64 {
	_, standardBytes := ArchiveOverhead(tier)
	return standardBytes
}

// applyDiscounts applies configured discounts to base pricing
func (pm *PricingManager) applyDiscounts(tier string, basePricing TierPricing) TierPricing {
	discountedPricing := basePricing

	// Apply enterprise discount
	if pm.config.DiscountConfig.EnterpriseDiscount > 0 {
		discount := pm.config.DiscountConfig.EnterpriseDiscount / 100.0
		discountedPricing.StorageCostPerGBMonth *= (1.0 - discount)
		discountedPricing.RetrievalCostPerGB *= (1.0 - discount)
	}

	// Apply reserved capacity discount
	if pm.config.DiscountConfig.ReservedCapacityDiscount > 0 {
		discount := pm.config.DiscountConfig.ReservedCapacityDiscount / 100.0
		discountedPricing.StorageCostPerGBMonth *= (1.0 - discount)
	}

	// Apply spot discount
	if pm.config.DiscountConfig.SpotDiscount > 0 {
		discount := pm.config.DiscountConfig.SpotDiscount / 100.0
		discountedPricing.StorageCostPerGBMonth *= (1.0 - discount)
	}

	// Apply custom tier-specific discounts
	if customDiscount, exists := pm.config.DiscountConfig.CustomDiscounts[tier]; exists {
		discount := customDiscount / 100.0
		discountedPricing.StorageCostPerGBMonth *= (1.0 - discount)
		discountedPricing.RetrievalCostPerGB *= (1.0 - discount)
	}

	pm.logger.Debug("Applied discounts to tier pricing",
		"tier", tier,
		"original_storage_cost", basePricing.StorageCostPerGBMonth,
		"discounted_storage_cost", discountedPricing.StorageCostPerGBMonth,
		"enterprise_discount", pm.config.DiscountConfig.EnterpriseDiscount,
		"reserved_discount", pm.config.DiscountConfig.ReservedCapacityDiscount)

	return discountedPricing
}

// CalculateVolumeDiscount calculates volume-based discounts
func (pm *PricingManager) CalculateVolumeDiscount(tier string, sizeGB float64, baseCost float64) float64 {
	if !pm.config.DiscountConfig.EnableVolumeDiscounts {
		return baseCost
	}

	for _, volumeTier := range pm.config.DiscountConfig.VolumeTiers {
		// Check if this volume tier applies to the storage tier
		applies := false
		for _, applicableTier := range volumeTier.AppliesTo {
			if applicableTier == tier || applicableTier == "ALL" {
				applies = true
				break
			}
		}

		if !applies {
			continue
		}

		// Check if size falls within this volume tier
		if sizeGB >= volumeTier.MinSizeGB && (volumeTier.MaxSizeGB == -1 || sizeGB <= volumeTier.MaxSizeGB) {
			discount := volumeTier.DiscountPercent / 100.0
			discountedCost := baseCost * (1.0 - discount)

			pm.logger.Debug("Applied volume discount",
				"tier", tier,
				"size_gb", sizeGB,
				"discount_percent", volumeTier.DiscountPercent,
				"original_cost", baseCost,
				"discounted_cost", discountedCost)

			return discountedCost
		}
	}

	return baseCost
}

// RefreshPricing is retained for API compatibility and is a no-op.
//
// Pricing is served from a built-in static rate table, so there is nothing to
// refresh. The AWS Pricing API integration this method used to drive was removed:
// it downloaded the ~100 MB S3 offer index and then discarded the parse, returning
// two hardcoded us-east-1 constants for every tier — strictly worse than reading
// the static table directly.
func (pm *PricingManager) RefreshPricing(_ context.Context) error {
	pm.logger.Debug("RefreshPricing is a no-op; pricing is served from a static rate table")
	return nil
}

// GetPricingSummary returns a summary of current pricing configuration
func (pm *PricingManager) GetPricingSummary() PricingSummary {
	summary := PricingSummary{
		UsePricingAPI:      pm.config.UsePricingAPI,
		Region:             pm.config.Region,
		Currency:           pm.config.Currency,
		LastUpdated:        pm.lastUpdated,
		EnterpriseDiscount: pm.config.DiscountConfig.EnterpriseDiscount,
		TierPricing:        make(map[string]TierPricingSummary),
	}

	// Get pricing summary for each tier
	tiers := []string{TierStandard, TierStandardIA, TierOneZoneIA, TierGlacierIR, TierGlacier, TierDeepArchive, TierIntelligent}

	for _, tier := range tiers {
		if pricing, err := pm.GetTierPricing(tier); err == nil {
			summary.TierPricing[tier] = TierPricingSummary{
				StorageCostPerGBMonth: pricing.StorageCostPerGBMonth,
				RetrievalCostPerGB:    pricing.RetrievalCostPerGB,
				PutRequestCost:        pricing.RequestCosts.PutRequestCost,
				GetRequestCost:        pricing.RequestCosts.GetRequestCost,
			}
		}
	}

	return summary
}

// PricingSummary provides a summary of pricing configuration
type PricingSummary struct {
	UsePricingAPI      bool                          `json:"use_pricing_api"`
	Region             string                        `json:"region"`
	Currency           string                        `json:"currency"`
	LastUpdated        time.Time                     `json:"last_updated"`
	EnterpriseDiscount float64                       `json:"enterprise_discount"`
	TierPricing        map[string]TierPricingSummary `json:"tier_pricing"`
}

// TierPricingSummary provides a summary of tier pricing
type TierPricingSummary struct {
	StorageCostPerGBMonth float64 `json:"storage_cost_per_gb_month"`
	RetrievalCostPerGB    float64 `json:"retrieval_cost_per_gb"`
	PutRequestCost        float64 `json:"put_request_cost"`
	GetRequestCost        float64 `json:"get_request_cost"`
}

// loadDiscountConfigFromFile loads discount configuration from an external file
func loadDiscountConfigFromFile(filePath string, logger *slog.Logger) (DiscountConfig, error) {
	var discountConfig DiscountConfig

	// Validate file path to prevent directory traversal
	if err := utils.ValidatePath(filePath, true); err != nil {
		return discountConfig, fmt.Errorf("invalid discount config file path: %w", err)
	}

	// Resolve relative paths
	cleanPath := filepath.Clean(filePath)
	absPath, err := filepath.Abs(cleanPath)
	if err != nil {
		return discountConfig, fmt.Errorf("failed to resolve path %s: %w", filePath, err)
	}

	// Check if file exists
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		return discountConfig, fmt.Errorf("discount config file does not exist: %s", absPath)
	}

	// Read file - path has been validated above to prevent directory traversal
	data, err := os.ReadFile(absPath) // #nosec G304
	if err != nil {
		return discountConfig, fmt.Errorf("failed to read discount config file %s: %w", absPath, err)
	}

	// Parse YAML
	if err := yaml.Unmarshal(data, &discountConfig); err != nil {
		return discountConfig, fmt.Errorf("failed to parse discount config YAML from %s: %w", absPath, err)
	}

	logger.Debug("Successfully loaded discount config from file",
		"file", absPath,
		"enterprise_discount", discountConfig.EnterpriseDiscount,
		"volume_tiers", len(discountConfig.VolumeTiers))

	return discountConfig, nil
}

// mergeDiscountConfigs merges inline and external discount configurations
// External config takes precedence over inline config for non-zero values
func mergeDiscountConfigs(inline, external DiscountConfig) DiscountConfig {
	merged := inline

	// Override with external values if they are non-zero
	if external.EnableVolumeDiscounts {
		merged.EnableVolumeDiscounts = external.EnableVolumeDiscounts
	}

	if external.EnterpriseDiscount > 0 {
		merged.EnterpriseDiscount = external.EnterpriseDiscount
	}

	if external.ReservedCapacityDiscount > 0 {
		merged.ReservedCapacityDiscount = external.ReservedCapacityDiscount
	}

	if external.SpotDiscount > 0 {
		merged.SpotDiscount = external.SpotDiscount
	}

	// Use external volume tiers if provided
	if len(external.VolumeTiers) > 0 {
		merged.VolumeTiers = external.VolumeTiers
	}

	// Merge custom discounts (external takes precedence)
	if len(external.CustomDiscounts) > 0 {
		if merged.CustomDiscounts == nil {
			merged.CustomDiscounts = make(map[string]float64)
		}
		maps.Copy(merged.CustomDiscounts, external.CustomDiscounts)
	}

	return merged
}
