package s3

import (
	"log/slog"
	"math"
	"os"
	"testing"
)

func abs(x float64) float64 {
	return math.Abs(x)
}

func TestPricingManager_CustomPricing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	
	// Create pricing config with custom pricing
	config := PricingConfig{
		UsePricingAPI: false,
		Region:        "us-west-2",
		Currency:      "USD",
		CustomPricing: map[string]TierPricing{
			TierStandard: {
				StorageCostPerGBMonth: 0.020, // Custom rate lower than default
				RetrievalCostPerGB:    0.0,
				RequestCosts: RequestCosts{
					PutRequestCost: 0.0000004,
					GetRequestCost: 0.0000003,
				},
			},
		},
		DiscountConfig: DiscountConfig{
			EnterpriseDiscount: 10.0, // 10% enterprise discount
		},
	}
	
	manager := NewPricingManager(config, logger)

	t.Run("Uses Custom Pricing", func(t *testing.T) {
		pricing, err := manager.GetTierPricing(TierStandard)
		if err != nil {
			t.Fatalf("Failed to get tier pricing: %v", err)
		}

		// Should use custom pricing with enterprise discount applied
		expectedCost := 0.020 * 0.9 // 10% discount
		if abs(pricing.StorageCostPerGBMonth - expectedCost) > 0.000001 {
			t.Errorf("Expected storage cost %f, got %f", expectedCost, pricing.StorageCostPerGBMonth)
		}

		expectedPutCost := 0.0000004 // Request costs not discounted in this implementation
		if pricing.RequestCosts.PutRequestCost != expectedPutCost {
			t.Errorf("Expected PUT cost %f, got %f", expectedPutCost, pricing.RequestCosts.PutRequestCost)
		}
	})

	t.Run("Falls Back to Defaults", func(t *testing.T) {
		// Test tier not in custom pricing
		pricing, err := manager.GetTierPricing(TierStandardIA)
		if err != nil {
			t.Fatalf("Failed to get tier pricing: %v", err)
		}

		// Should use default pricing with enterprise discount
		defaultCost := StorageTiers[TierStandardIA].CostPerGBMonth
		expectedCost := defaultCost * 0.9 // 10% discount
		if pricing.StorageCostPerGBMonth != expectedCost {
			t.Errorf("Expected storage cost %f, got %f", expectedCost, pricing.StorageCostPerGBMonth)
		}
	})
}

func TestPricingManager_VolumeDiscounts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	
	config := PricingConfig{
		DiscountConfig: DiscountConfig{
			EnableVolumeDiscounts: true,
			VolumeTiers: []VolumeTier{
				{
					MinSizeGB:       0.0,
					MaxSizeGB:       1024.0, // 1TB
					DiscountPercent: 0.0,
					AppliesTo:       []string{"ALL"},
				},
				{
					MinSizeGB:       1024.0, // 1TB
					MaxSizeGB:       10240.0, // 10TB
					DiscountPercent: 5.0,
					AppliesTo:       []string{"STANDARD", "STANDARD_IA"},
				},
				{
					MinSizeGB:       10240.0, // 10TB
					MaxSizeGB:       -1, // Unlimited
					DiscountPercent: 10.0,
					AppliesTo:       []string{"ALL"},
				},
			},
		},
	}
	
	manager := NewPricingManager(config, logger)

	tests := []struct {
		name           string
		tier           string
		sizeGB         float64
		baseCost       float64
		expectedDiscount float64
	}{
		{
			name:           "No Volume Discount",
			tier:           TierStandard,
			sizeGB:         500.0, // 500GB
			baseCost:       100.0,
			expectedDiscount: 0.0, // No discount for <1TB
		},
		{
			name:           "5% Volume Discount",
			tier:           TierStandard,
			sizeGB:         5000.0, // 5TB
			baseCost:       100.0,
			expectedDiscount: 5.0, // 5% discount for 1-10TB
		},
		{
			name:           "10% Volume Discount",
			tier:           TierStandardIA,
			sizeGB:         50000.0, // 50TB
			baseCost:       100.0,
			expectedDiscount: 10.0, // 10% discount for >10TB
		},
		{
			name:           "Tier Not Applicable",
			tier:           TierGlacier,
			sizeGB:         5000.0, // 5TB
			baseCost:       100.0,
			expectedDiscount: 0.0, // Glacier not in 5% tier applies_to
		},
		{
			name:           "All Tiers Applicable",
			tier:           TierGlacier,
			sizeGB:         50000.0, // 50TB
			baseCost:       100.0,
			expectedDiscount: 10.0, // 10% tier applies to ALL
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			discountedCost := manager.CalculateVolumeDiscount(tt.tier, tt.sizeGB, tt.baseCost)
			expectedCost := tt.baseCost * (1.0 - tt.expectedDiscount/100.0)
			
			if discountedCost != expectedCost {
				t.Errorf("Expected cost %f, got %f", expectedCost, discountedCost)
			}
		})
	}
}

func TestPricingManager_MultipleDiscounts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	
	config := PricingConfig{
		CustomPricing: map[string]TierPricing{
			TierStandardIA: {
				StorageCostPerGBMonth: 0.015, // Custom rate
				RetrievalCostPerGB:    0.012,
			},
		},
		DiscountConfig: DiscountConfig{
			EnterpriseDiscount:      15.0, // 15% enterprise discount
			ReservedCapacityDiscount: 10.0, // 10% reserved capacity discount
			CustomDiscounts: map[string]float64{
				TierStandardIA: 5.0, // Additional 5% for Standard-IA
			},
		},
	}
	
	manager := NewPricingManager(config, logger)

	t.Run("Multiple Discounts Applied", func(t *testing.T) {
		pricing, err := manager.GetTierPricing(TierStandardIA)
		if err != nil {
			t.Fatalf("Failed to get tier pricing: %v", err)
		}

		// Calculate expected cost with all discounts:
		// Base: 0.015
		// Enterprise discount: 0.015 * (1 - 0.15) = 0.01275
		// Reserved capacity: 0.01275 * (1 - 0.10) = 0.011475  
		// Custom tier discount: 0.011475 * (1 - 0.05) = 0.01090125
		expectedStorageCost := 0.015 * 0.85 * 0.90 * 0.95
		
		if pricing.StorageCostPerGBMonth != expectedStorageCost {
			t.Errorf("Expected storage cost %f, got %f", expectedStorageCost, pricing.StorageCostPerGBMonth)
		}

		// Retrieval cost should also be discounted (enterprise + custom)
		expectedRetrievalCost := 0.012 * 0.85 * 0.95 // No reserved capacity discount on retrieval
		if pricing.RetrievalCostPerGB != expectedRetrievalCost {
			t.Errorf("Expected retrieval cost %f, got %f", expectedRetrievalCost, pricing.RetrievalCostPerGB)
		}
	})
}

func TestPricingManager_DefaultPricing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	
	// Minimal config - should use all defaults
	config := PricingConfig{
		UsePricingAPI: false,
	}
	
	manager := NewPricingManager(config, logger)

	t.Run("Uses Default Pricing", func(t *testing.T) {
		pricing, err := manager.GetTierPricing(TierStandard)
		if err != nil {
			t.Fatalf("Failed to get tier pricing: %v", err)
		}

		// Should match default storage tier info
		expectedCost := StorageTiers[TierStandard].CostPerGBMonth
		if pricing.StorageCostPerGBMonth != expectedCost {
			t.Errorf("Expected storage cost %f, got %f", expectedCost, pricing.StorageCostPerGBMonth)
		}

		// Should have request costs
		if pricing.RequestCosts.PutRequestCost <= 0 {
			t.Error("Expected non-zero PUT request cost")
		}
	})

	t.Run("Handles Minimum Sizes", func(t *testing.T) {
		pricing, err := manager.GetTierPricing(TierStandardIA)
		if err != nil {
			t.Fatalf("Failed to get tier pricing: %v", err)
		}

		expectedMinSize := StorageTiers[TierStandardIA].MinObjectSize
		if pricing.MinimumBillableSize != expectedMinSize {
			t.Errorf("Expected minimum size %d, got %d", expectedMinSize, pricing.MinimumBillableSize)
		}

		expectedMinDays := StorageTiers[TierStandardIA].MinimumStorageDays
		if pricing.MinimumBillableDays != expectedMinDays {
			t.Errorf("Expected minimum days %d, got %d", expectedMinDays, pricing.MinimumBillableDays)
		}
	})
}

func TestPricingManager_PricingSummary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	
	config := PricingConfig{
		UsePricingAPI: false,
		Region:        "us-west-2",
		Currency:      "USD",
		DiscountConfig: DiscountConfig{
			EnterpriseDiscount: 20.0,
		},
	}
	
	manager := NewPricingManager(config, logger)

	t.Run("Generates Summary", func(t *testing.T) {
		summary := manager.GetPricingSummary()

		if summary.Region != "us-west-2" {
			t.Errorf("Expected region us-west-2, got %s", summary.Region)
		}

		if summary.Currency != "USD" {
			t.Errorf("Expected currency USD, got %s", summary.Currency)
		}

		if summary.EnterpriseDiscount != 20.0 {
			t.Errorf("Expected enterprise discount 20.0, got %f", summary.EnterpriseDiscount)
		}

		if len(summary.TierPricing) == 0 {
			t.Error("Expected tier pricing in summary")
		}

		// Check that Standard tier is included
		if _, exists := summary.TierPricing[TierStandard]; !exists {
			t.Error("Expected Standard tier in pricing summary")
		}
	})
}

func TestPricingManager_ErrorHandling(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	
	config := PricingConfig{
		UsePricingAPI: true, // Will fail without internet/proper endpoint
		Region:        "invalid-region",
	}
	
	manager := NewPricingManager(config, logger)

	t.Run("Falls Back on API Failure", func(t *testing.T) {
		// Should fall back to defaults when API fails
		pricing, err := manager.GetTierPricing(TierStandard)
		if err != nil {
			t.Fatalf("Should not error on API failure, should fall back: %v", err)
		}

		// Should still return reasonable pricing
		if pricing.StorageCostPerGBMonth <= 0 {
			t.Error("Expected positive storage cost even on API failure")
		}
	})
}

func TestPricingManager_CurrencyAndRegion(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	
	config := PricingConfig{
		Currency: "EUR",
		Region:   "eu-west-1",
	}
	
	manager := NewPricingManager(config, logger)

	t.Run("Respects Currency and Region", func(t *testing.T) {
		summary := manager.GetPricingSummary()

		if summary.Currency != "EUR" {
			t.Errorf("Expected currency EUR, got %s", summary.Currency)
		}

		if summary.Region != "eu-west-1" {
			t.Errorf("Expected region eu-west-1, got %s", summary.Region)
		}
	})

	t.Run("Defaults Currency and Region", func(t *testing.T) {
		emptyConfig := PricingConfig{}
		defaultManager := NewPricingManager(emptyConfig, logger)
		summary := defaultManager.GetPricingSummary()

		if summary.Currency != "USD" {
			t.Errorf("Expected default currency USD, got %s", summary.Currency)
		}

		if summary.Region != "us-east-1" {
			t.Errorf("Expected default region us-east-1, got %s", summary.Region)
		}
	})
}

func TestPricingManager_ExternalDiscountConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	
	// Create temporary external discount config file
	tempFile, err := os.CreateTemp("", "discount-config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer func() { _ = os.Remove(tempFile.Name()) }()
	
	externalConfig := `
enable_volume_discounts: true
enterprise_discount: 25.0
reserved_capacity_discount: 20.0
volume_tiers:
  - min_size_gb: 0.0
    max_size_gb: 1000.0
    discount_percent: 0.0
    applies_to: ["ALL"]
  - min_size_gb: 1000.0
    max_size_gb: -1
    discount_percent: 15.0
    applies_to: ["ALL"]
custom_discounts:
  GLACIER: 40.0
`
	
	if _, err := tempFile.WriteString(externalConfig); err != nil {
		t.Fatalf("Failed to write temp file: %v", err)
	}
	_ = tempFile.Close()

	t.Run("Loads External Discount Config", func(t *testing.T) {
		config := PricingConfig{
			DiscountConfigFile: tempFile.Name(),
			DiscountConfig: DiscountConfig{
				EnterpriseDiscount: 10.0, // Should be overridden by external file
			},
		}
		
		manager := NewPricingManager(config, logger)
		
		// Verify external config was loaded
		if manager.config.DiscountConfig.EnterpriseDiscount != 25.0 {
			t.Errorf("Expected enterprise discount 25.0 from external file, got %f", 
				manager.config.DiscountConfig.EnterpriseDiscount)
		}
		
		if manager.config.DiscountConfig.ReservedCapacityDiscount != 20.0 {
			t.Errorf("Expected reserved capacity discount 20.0 from external file, got %f", 
				manager.config.DiscountConfig.ReservedCapacityDiscount)
		}
		
		if len(manager.config.DiscountConfig.VolumeTiers) != 2 {
			t.Errorf("Expected 2 volume tiers from external file, got %d", 
				len(manager.config.DiscountConfig.VolumeTiers))
		}
		
		if glacierDiscount, exists := manager.config.DiscountConfig.CustomDiscounts["GLACIER"]; !exists || glacierDiscount != 40.0 {
			t.Errorf("Expected Glacier custom discount 40.0 from external file, got %f", glacierDiscount)
		}
	})

	t.Run("Handles Missing External File", func(t *testing.T) {
		config := PricingConfig{
			DiscountConfigFile: "/non/existent/file.yaml",
			DiscountConfig: DiscountConfig{
				EnterpriseDiscount: 15.0, // Should fallback to this
			},
		}
		
		manager := NewPricingManager(config, logger)
		
		// Should fallback to inline config
		if manager.config.DiscountConfig.EnterpriseDiscount != 15.0 {
			t.Errorf("Expected fallback to inline enterprise discount 15.0, got %f", 
				manager.config.DiscountConfig.EnterpriseDiscount)
		}
	})
}