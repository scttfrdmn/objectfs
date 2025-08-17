package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
	
	"gopkg.in/yaml.v2"
)

// PricingManager handles AWS S3 pricing with custom discounts and overrides
type PricingManager struct {
	config          PricingConfig
	logger          *slog.Logger
	cachedPricing   map[string]TierPricing
	lastUpdated     time.Time
	httpClient      *http.Client
}

// NewPricingManager creates a new pricing manager
func NewPricingManager(config PricingConfig, logger *slog.Logger) *PricingManager {
	if config.Currency == "" {
		config.Currency = "USD"
	}
	if config.Region == "" {
		config.Region = "us-east-1" // Default pricing region
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
		config:        config,
		logger:        logger,
		cachedPricing: make(map[string]TierPricing),
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}
}

// GetTierPricing returns pricing for a specific tier with discounts applied
func (pm *PricingManager) GetTierPricing(tier string) (TierPricing, error) {
	// Check for custom pricing override first
	if customPricing, exists := pm.config.CustomPricing[tier]; exists {
		pm.logger.Debug("Using custom pricing override", "tier", tier)
		return pm.applyDiscounts(tier, customPricing), nil
	}

	// Get base pricing (from API or defaults)
	basePricing, err := pm.getBasePricing(tier)
	if err != nil {
		return TierPricing{}, fmt.Errorf("failed to get base pricing for tier %s: %w", tier, err)
	}

	// Apply discounts to base pricing
	return pm.applyDiscounts(tier, basePricing), nil
}

// getBasePricing retrieves base pricing from AWS API or uses defaults
func (pm *PricingManager) getBasePricing(tier string) (TierPricing, error) {
	if pm.config.UsePricingAPI {
		// Try to fetch from AWS Pricing API
		if pricing, err := pm.fetchFromPricingAPI(tier); err == nil {
			pm.cachedPricing[tier] = pricing
			pm.lastUpdated = time.Now()
			return pricing, nil
		} else {
			pm.logger.Warn("Failed to fetch pricing from AWS API, using defaults", 
				"tier", tier, "error", err)
		}
	}

	// Fall back to default pricing
	return pm.getDefaultPricing(tier), nil
}

// fetchFromPricingAPI fetches current pricing from AWS Pricing API
func (pm *PricingManager) fetchFromPricingAPI(tier string) (TierPricing, error) {
	// AWS Pricing API endpoint for S3
	url := fmt.Sprintf("https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonS3/current/%s/index.json", pm.config.Region)
	
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return TierPricing{}, fmt.Errorf("failed to create pricing API request: %w", err)
	}

	resp, err := pm.httpClient.Do(req)
	if err != nil {
		return TierPricing{}, fmt.Errorf("failed to fetch pricing data: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return TierPricing{}, fmt.Errorf("pricing API returned status %d", resp.StatusCode)
	}

	var pricingData AWSPricingResponse
	if err := json.NewDecoder(resp.Body).Decode(&pricingData); err != nil {
		return TierPricing{}, fmt.Errorf("failed to decode pricing response: %w", err)
	}

	return pm.parsePricingData(tier, pricingData)
}

// AWSPricingResponse represents the AWS Pricing API response structure
type AWSPricingResponse struct {
	FormatVersion   string                    `json:"formatVersion"`
	Disclaimer      string                    `json:"disclaimer"`
	OfferCode       string                    `json:"offerCode"`
	Version         string                    `json:"version"`
	PublicationDate string                    `json:"publicationDate"`
	Products        map[string]AWSProduct     `json:"products"`
	Terms           map[string]interface{}    `json:"terms"`
}

// AWSProduct represents a product in the AWS pricing data
type AWSProduct struct {
	SKU           string                 `json:"sku"`
	ProductFamily string                 `json:"productFamily"`
	Attributes    map[string]string      `json:"attributes"`
}

// parsePricingData extracts pricing information from AWS API response
func (pm *PricingManager) parsePricingData(tier string, data AWSPricingResponse) (TierPricing, error) {
	// This is a simplified parser - actual AWS pricing API parsing is complex
	// In production, you'd need more sophisticated parsing logic
	
	storageClass := pm.mapTierToStorageClass(tier)
	
	// Look for storage pricing
	var storageCost = 0.023 // Default fallback
	var retrievalCost = 0.0
	
	for _, product := range data.Products {
		if product.Attributes["storageClass"] == storageClass {
			// Extract pricing from terms (simplified)
			storageCost = pm.extractStorageCost(product, data.Terms)
			retrievalCost = pm.extractRetrievalCost(product, data.Terms)
			break
		}
	}

	return TierPricing{
		StorageCostPerGBMonth: storageCost,
		RetrievalCostPerGB:    retrievalCost,
		RequestCosts: RequestCosts{
			PutRequestCost:    pm.getDefaultRequestCost("PUT", tier),
			GetRequestCost:    pm.getDefaultRequestCost("GET", tier),
			DeleteRequestCost: 0.0, // Usually free
			ListRequestCost:   pm.getDefaultRequestCost("LIST", tier),
			HeadRequestCost:   pm.getDefaultRequestCost("HEAD", tier),
		},
		MinimumBillableSize: pm.getDefaultMinimumSize(tier),
		MinimumBillableDays: pm.getDefaultMinimumDays(tier),
		TransitionCosts:     make(map[string]float64),
	}, nil
}

// Helper methods for AWS API parsing
func (pm *PricingManager) mapTierToStorageClass(tier string) string {
	mapping := map[string]string{
		TierStandard:    "General Purpose",
		TierStandardIA:  "Infrequent Access",
		TierOneZoneIA:   "One Zone - Infrequent Access",
		TierGlacierIR:   "Glacier Instant Retrieval",
		TierGlacier:     "Glacier Flexible Retrieval",
		TierDeepArchive: "Glacier Deep Archive",
		TierIntelligent: "Intelligent-Tiering",
	}
	return mapping[tier]
}

func (pm *PricingManager) extractStorageCost(product AWSProduct, terms map[string]interface{}) float64 {
	// Simplified extraction - real implementation would parse the complex terms structure
	return 0.023 // Fallback
}

func (pm *PricingManager) extractRetrievalCost(product AWSProduct, terms map[string]interface{}) float64 {
	// Simplified extraction
	return 0.01 // Fallback for IA tiers
}

// getDefaultPricing returns default pricing when API is unavailable
func (pm *PricingManager) getDefaultPricing(tier string) TierPricing {
	// Use the existing StorageTiers data as defaults
	tierInfo, exists := StorageTiers[tier]
	if !exists {
		tierInfo = StorageTiers[TierStandard]
	}

	return TierPricing{
		StorageCostPerGBMonth: tierInfo.CostPerGBMonth,
		RetrievalCostPerGB:    pm.getDefaultRetrievalCost(tier),
		RequestCosts: RequestCosts{
			PutRequestCost:    pm.getDefaultRequestCost("PUT", tier),
			GetRequestCost:    pm.getDefaultRequestCost("GET", tier),
			DeleteRequestCost: 0.0,
			ListRequestCost:   pm.getDefaultRequestCost("LIST", tier),
			HeadRequestCost:   pm.getDefaultRequestCost("HEAD", tier),
		},
		MinimumBillableSize: tierInfo.MinObjectSize,
		MinimumBillableDays: tierInfo.MinimumStorageDays,
		TransitionCosts:     make(map[string]float64),
	}
}

// Helper methods for default pricing
func (pm *PricingManager) getDefaultRetrievalCost(tier string) float64 {
	costs := map[string]float64{
		TierStandard:    0.0,
		TierStandardIA:  0.01,
		TierOneZoneIA:   0.01,
		TierGlacierIR:   0.03,
		TierGlacier:     0.02, // Variable based on retrieval speed
		TierDeepArchive: 0.05, // Variable based on retrieval speed
		TierIntelligent: 0.0,  // No retrieval charges
	}
	return costs[tier]
}

func (pm *PricingManager) getDefaultRequestCost(requestType, tier string) float64 {
	// Default request costs per 1000 requests
	costs := map[string]map[string]float64{
		"PUT": {
			TierStandard:    0.0005,
			TierStandardIA:  0.001,
			TierOneZoneIA:   0.001,
			TierGlacierIR:   0.002,
			TierGlacier:     0.0025,
			TierDeepArchive: 0.005,
			TierIntelligent: 0.0005,
		},
		"GET": {
			TierStandard:    0.0004,
			TierStandardIA:  0.0004,
			TierOneZoneIA:   0.0004,
			TierGlacierIR:   0.0004,
			TierGlacier:     0.0004,
			TierDeepArchive: 0.0004,
			TierIntelligent: 0.0004,
		},
		"LIST": {
			TierStandard:    0.005,
			TierStandardIA:  0.005,
			TierOneZoneIA:   0.005,
			TierGlacierIR:   0.005,
			TierGlacier:     0.005,
			TierDeepArchive: 0.005,
			TierIntelligent: 0.005,
		},
		"HEAD": {
			TierStandard:    0.0004,
			TierStandardIA:  0.0004,
			TierOneZoneIA:   0.0004,
			TierGlacierIR:   0.0004,
			TierGlacier:     0.0004,
			TierDeepArchive: 0.0004,
			TierIntelligent: 0.0004,
		},
	}
	
	if tierCosts, exists := costs[requestType]; exists {
		if cost, exists := tierCosts[tier]; exists {
			return cost / 1000.0 // Convert to cost per request
		}
	}
	return 0.0005 / 1000.0 // Default fallback
}

func (pm *PricingManager) getDefaultMinimumSize(tier string) int64 {
	sizes := map[string]int64{
		TierStandard:    0,
		TierStandardIA:  128 * 1024,
		TierOneZoneIA:   128 * 1024,
		TierGlacierIR:   128 * 1024,
		TierGlacier:     40 * 1024,
		TierDeepArchive: 40 * 1024,
		TierIntelligent: 0,
	}
	return sizes[tier]
}

func (pm *PricingManager) getDefaultMinimumDays(tier string) int {
	days := map[string]int{
		TierStandard:    0,
		TierStandardIA:  30,
		TierOneZoneIA:   30,
		TierGlacierIR:   90,
		TierGlacier:     90,
		TierDeepArchive: 180,
		TierIntelligent: 0,
	}
	return days[tier]
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

// RefreshPricing forces a refresh of pricing data from AWS API
func (pm *PricingManager) RefreshPricing(ctx context.Context) error {
	if !pm.config.UsePricingAPI {
		return fmt.Errorf("pricing API is disabled")
	}

	pm.logger.Info("Refreshing pricing data from AWS API")
	
	// Clear cached pricing to force refresh
	pm.cachedPricing = make(map[string]TierPricing)
	
	// Refresh pricing for all tiers
	tiers := []string{TierStandard, TierStandardIA, TierOneZoneIA, TierGlacierIR, TierGlacier, TierDeepArchive, TierIntelligent}
	
	for _, tier := range tiers {
		if _, err := pm.getBasePricing(tier); err != nil {
			pm.logger.Warn("Failed to refresh pricing for tier", "tier", tier, "error", err)
		}
	}

	pm.logger.Info("Pricing refresh completed", "last_updated", pm.lastUpdated)
	return nil
}

// GetPricingSummary returns a summary of current pricing configuration
func (pm *PricingManager) GetPricingSummary() PricingSummary {
	summary := PricingSummary{
		UsePricingAPI:       pm.config.UsePricingAPI,
		Region:              pm.config.Region,
		Currency:            pm.config.Currency,
		LastUpdated:         pm.lastUpdated,
		EnterpriseDiscount:  pm.config.DiscountConfig.EnterpriseDiscount,
		TierPricing:         make(map[string]TierPricingSummary),
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
	UsePricingAPI      bool                           `json:"use_pricing_api"`
	Region             string                         `json:"region"`
	Currency           string                         `json:"currency"`
	LastUpdated        time.Time                      `json:"last_updated"`
	EnterpriseDiscount float64                        `json:"enterprise_discount"`
	TierPricing        map[string]TierPricingSummary  `json:"tier_pricing"`
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
	
	// Resolve relative paths
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return discountConfig, fmt.Errorf("failed to resolve path %s: %w", filePath, err)
	}
	
	// Check if file exists
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		return discountConfig, fmt.Errorf("discount config file does not exist: %s", absPath)
	}
	
	// Read file
	data, err := os.ReadFile(absPath)
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
		for tier, discount := range external.CustomDiscounts {
			merged.CustomDiscounts[tier] = discount
		}
	}
	
	return merged
}