package s3

import (
	"log/slog"
	"math"
	"os"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/awsname"
)

func abs(x float64) float64 {
	return math.Abs(x)
}

func TestPricingManager_CustomPricing(t *testing.T) {
	t.Parallel()

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
		t.Parallel()

		pricing, err := manager.GetTierPricing(TierStandard)
		if err != nil {
			t.Fatalf("Failed to get tier pricing: %v", err)
		}

		// Should use custom pricing with enterprise discount applied
		expectedCost := 0.020 * 0.9 // 10% discount
		if abs(pricing.StorageCostPerGBMonth-expectedCost) > 0.000001 {
			t.Errorf("Expected storage cost %f, got %f", expectedCost, pricing.StorageCostPerGBMonth)
		}

		expectedPutCost := 0.0000004 // Request costs not discounted in this implementation
		if pricing.RequestCosts.PutRequestCost != expectedPutCost {
			t.Errorf("Expected PUT cost %f, got %f", expectedPutCost, pricing.RequestCosts.PutRequestCost)
		}
	})

	t.Run("Falls Back to Defaults", func(t *testing.T) {
		t.Parallel()

		// Test tier not in custom pricing
		pricing, err := manager.GetTierPricing(TierStandardIA)
		if err != nil {
			t.Fatalf("Failed to get tier pricing: %v", err)
		}

		// Should use default pricing with enterprise discount.
		//
		// The undiscounted rate is asked of the same manager, in its own region. It used to be read
		// off StorageTiers, which held a rate filled at package init — so this comparison was
		// always against us-east-1 no matter what region the manager resolved.
		defaultCost := manager.StorageRate(TierStandardIA)
		expectedCost := defaultCost * 0.9 // 10% discount
		if pricing.StorageCostPerGBMonth != expectedCost {
			t.Errorf("Expected storage cost %f, got %f", expectedCost, pricing.StorageCostPerGBMonth)
		}
	})
}

func TestPricingManager_VolumeDiscounts(t *testing.T) {
	t.Parallel()

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
					MinSizeGB:       1024.0,  // 1TB
					MaxSizeGB:       10240.0, // 10TB
					DiscountPercent: 5.0,
					AppliesTo:       []string{"STANDARD", "STANDARD_IA"},
				},
				{
					MinSizeGB:       10240.0, // 10TB
					MaxSizeGB:       -1,      // Unlimited
					DiscountPercent: 10.0,
					AppliesTo:       []string{"ALL"},
				},
			},
		},
	}

	manager := NewPricingManager(config, logger)

	tests := []struct {
		name             string
		tier             string
		sizeGB           float64
		baseCost         float64
		expectedDiscount float64
	}{
		{
			name:             "No Volume Discount",
			tier:             TierStandard,
			sizeGB:           500.0, // 500GB
			baseCost:         100.0,
			expectedDiscount: 0.0, // No discount for <1TB
		},
		{
			name:             "5% Volume Discount",
			tier:             TierStandard,
			sizeGB:           5000.0, // 5TB
			baseCost:         100.0,
			expectedDiscount: 5.0, // 5% discount for 1-10TB
		},
		{
			name:             "10% Volume Discount",
			tier:             TierStandardIA,
			sizeGB:           50000.0, // 50TB
			baseCost:         100.0,
			expectedDiscount: 10.0, // 10% discount for >10TB
		},
		{
			name:             "Tier Not Applicable",
			tier:             TierGlacier,
			sizeGB:           5000.0, // 5TB
			baseCost:         100.0,
			expectedDiscount: 0.0, // Glacier not in 5% tier applies_to
		},
		{
			name:             "All Tiers Applicable",
			tier:             TierGlacier,
			sizeGB:           50000.0, // 50TB
			baseCost:         100.0,
			expectedDiscount: 10.0, // 10% tier applies to ALL
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			discountedCost := manager.CalculateVolumeDiscount(tt.tier, tt.sizeGB, tt.baseCost)
			expectedCost := tt.baseCost * (1.0 - tt.expectedDiscount/100.0)

			if discountedCost != expectedCost {
				t.Errorf("Expected cost %f, got %f", expectedCost, discountedCost)
			}
		})
	}
}

func TestPricingManager_MultipleDiscounts(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	config := PricingConfig{
		CustomPricing: map[string]TierPricing{
			TierStandardIA: {
				StorageCostPerGBMonth: 0.015, // Custom rate
				RetrievalCostPerGB:    0.012,
			},
		},
		DiscountConfig: DiscountConfig{
			EnterpriseDiscount:       15.0, // 15% enterprise discount
			ReservedCapacityDiscount: 10.0, // 10% reserved capacity discount
			CustomDiscounts: map[string]float64{
				TierStandardIA: 5.0, // Additional 5% for Standard-IA
			},
		},
	}

	manager := NewPricingManager(config, logger)

	t.Run("Multiple Discounts Applied", func(t *testing.T) {
		t.Parallel()

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
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Minimal config - should use all defaults
	config := PricingConfig{
		UsePricingAPI: false,
	}

	manager := NewPricingManager(config, logger)

	t.Run("Uses Default Pricing", func(t *testing.T) {
		t.Parallel()

		pricing, err := manager.GetTierPricing(TierStandard)
		if err != nil {
			t.Fatalf("Failed to get tier pricing: %v", err)
		}

		// Should match the manager's own storage rate for the tier, with no discounts configured.
		expectedCost := manager.StorageRate(TierStandard)
		if pricing.StorageCostPerGBMonth != expectedCost {
			t.Errorf("Expected storage cost %f, got %f", expectedCost, pricing.StorageCostPerGBMonth)
		}

		// Should have request costs
		if pricing.RequestCosts.PutRequestCost <= 0 {
			t.Error("Expected non-zero PUT request cost")
		}
	})

	t.Run("Handles Minimum Sizes", func(t *testing.T) {
		t.Parallel()

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
	t.Parallel()

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
		t.Parallel()

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
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	config := PricingConfig{
		UsePricingAPI: true, // Will fail without internet/proper endpoint
		Region:        "invalid-region",
	}

	manager := NewPricingManager(config, logger)

	t.Run("Falls Back on API Failure", func(t *testing.T) {
		t.Parallel()

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
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	config := PricingConfig{
		Currency: "EUR",
		Region:   "eu-west-1",
	}

	manager := NewPricingManager(config, logger)

	t.Run("Respects Currency and Region", func(t *testing.T) {
		t.Parallel()

		summary := manager.GetPricingSummary()

		if summary.Currency != "EUR" {
			t.Errorf("Expected currency EUR, got %s", summary.Currency)
		}

		if summary.Region != "eu-west-1" {
			t.Errorf("Expected region eu-west-1, got %s", summary.Region)
		}
	})

	t.Run("Defaults Currency and Region", func(t *testing.T) {
		t.Parallel()

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
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create temporary external discount config file
	tempFile, err := os.CreateTemp(t.TempDir(), "discount-config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	// No defer removing this file. It lives in t.TempDir(), which the framework deletes after the
	// test *and its parallel subtests* finish; a plain defer here would run when this function
	// returns, which is before a parallel child has read the path it was handed.

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
		t.Parallel()

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
		t.Parallel()

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

// TestDefaultPricingHoldsThePublishedRequestPrices is the test whose absence let #209 happen.
//
// getDefaultPricing carried its own request-rate table in per-1,000 units and divided by 1,000 on the
// way out. Standard PUT was written 0.0005 where AWS charges $0.005 per 1,000, so the default
// configuration costed every write at a tenth of its price. Nothing here caught it: the existing
// tests assert PutRequestCost > 0, or compare it to StorageTiers, or recompute the discount formula —
// all of which agree with a rate that is uniformly wrong.
//
// So the expectations are the published price and the published divisor, stated separately as
// literals, and the product is compared to what the manager returns. Dividing the published figure by
// hand here would just repeat whatever mistake the code made.
//
// This overlaps internal/awsrates' own test deliberately. That one checks the table; this one checks
// that the path a caller actually takes — GetTierPricing, with no custom pricing and no discounts —
// reaches it. The defect was not in a table, it was in a second table that a caller reached instead.
func TestDefaultPricingHoldsThePublishedRequestPrices(t *testing.T) {
	t.Parallel()

	manager := NewPricingManager(PricingConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cases := []struct {
		tier        string
		op          string
		published   float64 // dollars, as AWS prints them
		per         float64 // per how many requests
		description string
	}{
		{TierStandard, "PUT", 0.005, 1_000, "$0.005 per 1,000 PUT, COPY, POST, or LIST requests"},
		{TierStandard, "GET", 0.004, 10_000, "$0.004 per 10,000 GET and all other requests"},
		{TierStandardIA, "PUT", 0.01, 1_000, "$0.01 per 1,000 PUT to Standard-Infrequent Access"},
		{TierStandardIA, "GET", 0.01, 10_000, "$0.01 per 10,000 GET from Standard-Infrequent Access"},
		{TierOneZoneIA, "PUT", 0.01, 1_000, "$0.01 per 1,000 PUT to One Zone-Infrequent Access"},
		{TierGlacierIR, "PUT", 0.02, 1_000, "$0.02 per 1,000 PUT to Glacier Instant Retrieval"},
		{TierGlacierIR, "GET", 0.1, 10_000, "$0.1 per 10,000 GET from Glacier Instant Retrieval"},
		// $0.03, not the $0.05 this line asserted until the generated rate table disagreed with
		// it. Both were reading Requests-Tier3 operation=RestoreObject — a *thaw*, 67% above the
		// write — and this test passed because it checked a table built from the same query.
		{TierGlacier, "PUT", 0.03, 1_000, "$0.03 per 1,000 PUT requests to Glacier Flexible Retrieval"},

		// Deep Archive genuinely is the lifecycle rate: AWS publishes no PUT usagetype for it.
		{TierDeepArchive, "PUT", 0.05, 1_000, "$0.05 per 1,000 Lifecycle Transition requests into Deep Archive"},
		{TierIntelligent, "PUT", 0.005, 1_000, "$0.005 per 1,000 PUT to Intelligent-Tiering"},
	}

	for _, tc := range cases {
		t.Run(tc.tier+"/"+tc.op, func(t *testing.T) {
			t.Parallel()

			pricing, err := manager.GetTierPricing(tc.tier)
			if err != nil {
				t.Fatalf("GetTierPricing(%s): %v", tc.tier, err)
			}

			got := pricing.RequestCosts.PutRequestCost
			if tc.op == "GET" {
				got = pricing.RequestCosts.GetRequestCost
			}

			want := tc.published / tc.per
			if abs(got-want) > 1e-12 {
				t.Errorf("%s %s costs %v per request, want %v (%s)\n"+
					"  ratio to expected: %.4gx — a round factor of ten here is the per-1,000 price "+
					"stored as if it were per-request, which is exactly the #209 defect",
					tc.tier, tc.op, got, want, tc.description, got/want)
			}
		})
	}
}

// TestDefaultPricingHoldsThePublishedStorageAndRetrievalRates covers the other two money fields.
//
// Same reasoning: literal published figures, not a formula. The retrieval rates are the ones most
// likely to be quietly wrong, because Glacier and Deep Archive charge per retrieval *speed* and a
// single number has to name which one it is — the table this replaced said 0.02 for Glacier with the
// comment "Variable based on retrieval speed", which is not a rate, it is an admission that nobody
// knew which rate it was. AWS charges $0.01/GB for Glacier Standard retrieval.
func TestDefaultPricingHoldsThePublishedStorageAndRetrievalRates(t *testing.T) {
	t.Parallel()

	manager := NewPricingManager(PricingConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cases := []struct {
		tier      string
		storage   float64
		retrieval float64
		note      string
	}{
		{TierStandard, 0.023, 0.0, "no retrieval fee"},
		{TierStandardIA, 0.0125, 0.01, ""},
		{TierOneZoneIA, 0.01, 0.01, ""},
		{TierGlacierIR, 0.004, 0.03, ""},
		{TierGlacier, 0.0036, 0.01, "Standard retrieval speed"},
		{TierDeepArchive, 0.00099, 0.02, "Standard retrieval speed"},
		{TierIntelligent, 0.023, 0.0, "frequent-access tier; monitoring charges are separate"},
		{TierReducedRedundancy, 0.024, 0.0, "deprecated, and dearer than Standard — that is not a typo"},
	}

	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			t.Parallel()

			pricing, err := manager.GetTierPricing(tc.tier)
			if err != nil {
				t.Fatalf("GetTierPricing(%s): %v", tc.tier, err)
			}

			if abs(pricing.StorageCostPerGBMonth-tc.storage) > 1e-12 {
				t.Errorf("%s stores at %v/GB-month, want %v %s", tc.tier,
					pricing.StorageCostPerGBMonth, tc.storage, tc.note)
			}

			if abs(pricing.RetrievalCostPerGB-tc.retrieval) > 1e-12 {
				t.Errorf("%s retrieves at %v/GB, want %v %s", tc.tier,
					pricing.RetrievalCostPerGB, tc.retrieval, tc.note)
			}
		})
	}
}

// TestEveryTierPricesEveryMoneyField fails when a tier reports $0 for something S3 charges for.
//
// A zero rate is the worst kind of wrong answer here, because it does not look like a missing entry —
// it looks like free, which is plausible enough to survive a glance at a cost report. This walks
// every class the config loader accepts, not a hand-written list, so a class added to awsname without
// a rate fails here rather than reporting zero.
func TestEveryTierPricesEveryMoneyField(t *testing.T) {
	t.Parallel()

	manager := NewPricingManager(PricingConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	for _, tier := range awsname.StorageClasses() {
		t.Run(tier, func(t *testing.T) {
			t.Parallel()

			pricing, err := manager.GetTierPricing(tier)
			if err != nil {
				t.Fatalf("GetTierPricing(%s): %v", tier, err)
			}

			if pricing.StorageCostPerGBMonth <= 0 {
				t.Errorf("%s stores at %v/GB-month; no S3 class is free to store",
					tier, pricing.StorageCostPerGBMonth)
			}

			if pricing.RequestCosts.PutRequestCost <= 0 {
				t.Errorf("%s writes at %v per request; every class charges for PUT",
					tier, pricing.RequestCosts.PutRequestCost)
			}

			if pricing.RequestCosts.GetRequestCost <= 0 {
				t.Errorf("%s reads at %v per request; every class charges for GET",
					tier, pricing.RequestCosts.GetRequestCost)
			}

			if pricing.RequestCosts.ListRequestCost <= 0 {
				t.Errorf("%s lists at %v per request; LIST bills in the Tier1 group",
					tier, pricing.RequestCosts.ListRequestCost)
			}

			// HEAD and GET bill in the same request group, so they must agree. If they diverge, one of
			// the two is reading a rate that does not exist.
			if pricing.RequestCosts.HeadRequestCost != pricing.RequestCosts.GetRequestCost {
				t.Errorf("%s bills HEAD at %v and GET at %v; both are Tier2 requests and AWS charges "+
					"one price for the group", tier, pricing.RequestCosts.HeadRequestCost,
					pricing.RequestCosts.GetRequestCost)
			}

			// DELETE is free on every class, and reporting a cost for it would overstate every
			// cleanup operation.
			if pricing.RequestCosts.DeleteRequestCost != 0 {
				t.Errorf("%s bills DELETE at %v; DELETE is free on S3",
					tier, pricing.RequestCosts.DeleteRequestCost)
			}
		})
	}
}
