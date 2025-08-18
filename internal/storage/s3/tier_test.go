package s3

import (
	"log/slog"
	"os"
	"testing"
	"time"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	awsconfig "github.com/scttfrdmn/cargoship/pkg/aws/config"
)

func TestStorageTiers(t *testing.T) {
	tests := []struct {
		name            string
		tier            string
		expectedName    string
		expectedMinSize int64
		expectedEmbargo time.Duration
		expectedCost    float64
	}{
		{
			name:            "Standard Tier",
			tier:            TierStandard,
			expectedName:    "Standard",
			expectedMinSize: 0,
			expectedEmbargo: 0,
			expectedCost:    0.023,
		},
		{
			name:            "Standard-IA Tier",
			tier:            TierStandardIA,
			expectedName:    "Standard-Infrequent Access",
			expectedMinSize: 128 * 1024,
			expectedEmbargo: 30 * 24 * time.Hour,
			expectedCost:    0.0125,
		},
		{
			name:            "One Zone-IA Tier",
			tier:            TierOneZoneIA,
			expectedName:    "One Zone-Infrequent Access",
			expectedMinSize: 128 * 1024,
			expectedEmbargo: 30 * 24 * time.Hour,
			expectedCost:    0.01,
		},
		{
			name:            "Glacier Instant Retrieval",
			tier:            TierGlacierIR,
			expectedName:    "Glacier Instant Retrieval",
			expectedMinSize: 128 * 1024,
			expectedEmbargo: 90 * 24 * time.Hour,
			expectedCost:    0.004,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tierInfo, exists := StorageTiers[tt.tier]
			if !exists {
				t.Fatalf("Tier %s not found in StorageTiers", tt.tier)
			}

			if tierInfo.Name != tt.expectedName {
				t.Errorf("Expected name %s, got %s", tt.expectedName, tierInfo.Name)
			}

			if tierInfo.MinObjectSize != tt.expectedMinSize {
				t.Errorf("Expected min size %d, got %d", tt.expectedMinSize, tierInfo.MinObjectSize)
			}

			if tierInfo.DeletionEmbargo != tt.expectedEmbargo {
				t.Errorf("Expected embargo %v, got %v", tt.expectedEmbargo, tierInfo.DeletionEmbargo)
			}

			if tierInfo.CostPerGBMonth != tt.expectedCost {
				t.Errorf("Expected cost %f, got %f", tt.expectedCost, tierInfo.CostPerGBMonth)
			}
		})
	}
}

func TestTierValidator(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	t.Run("Standard Tier Validation", func(t *testing.T) {
		validator := NewTierValidator(TierStandard, TierConstraints{}, logger)

		// Should allow any size object
		err := validator.ValidateWrite("test.txt", 1)
		if err != nil {
			t.Errorf("Standard tier should allow 1-byte object: %v", err)
		}

		// Should allow immediate deletion
		err = validator.ValidateDelete("test.txt", 0)
		if err != nil {
			t.Errorf("Standard tier should allow immediate deletion: %v", err)
		}
	})

	t.Run("Standard-IA Tier Validation", func(t *testing.T) {
		validator := NewTierValidator(TierStandardIA, TierConstraints{}, logger)

		// Should reject small objects
		err := validator.ValidateWrite("small.txt", 1024) // 1KB < 128KB minimum
		if err == nil {
			t.Error("Standard-IA tier should reject objects smaller than 128KB")
		}

		// Should allow objects >= 128KB
		err = validator.ValidateWrite("large.txt", 128*1024)
		if err != nil {
			t.Errorf("Standard-IA tier should allow 128KB objects: %v", err)
		}

		// Should reject deletion before 30 days
		err = validator.ValidateDelete("test.txt", 15*24*time.Hour) // 15 days
		if err == nil {
			t.Error("Standard-IA tier should reject deletion before 30 days")
		}

		// Should allow deletion after 30 days
		err = validator.ValidateDelete("test.txt", 31*24*time.Hour) // 31 days
		if err != nil {
			t.Errorf("Standard-IA tier should allow deletion after 30 days: %v", err)
		}
	})

	t.Run("Custom Constraints Override", func(t *testing.T) {
		constraints := TierConstraints{
			MinObjectSize:   256 * 1024,          // 256KB custom minimum
			DeletionEmbargo: 60 * 24 * time.Hour, // 60 days custom embargo
		}
		validator := NewTierValidator(TierStandardIA, constraints, logger)

		// Should use custom minimum size
		err := validator.ValidateWrite("test.txt", 128*1024) // 128KB < 256KB custom minimum
		if err == nil {
			t.Error("Custom constraints should override tier defaults")
		}

		// Should use custom deletion embargo
		err = validator.ValidateDelete("test.txt", 45*24*time.Hour) // 45 days < 60 days custom embargo
		if err == nil {
			t.Error("Custom constraints should override tier defaults for deletion")
		}
	})
}

func TestTierRecommendations(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	t.Run("Size-based Recommendations", func(t *testing.T) {
		validator := NewTierValidator(TierStandardIA, TierConstraints{}, logger)

		// Small objects should recommend Standard tier
		recommendations := validator.GetRecommendations(64*1024, "unknown") // 64KB
		found := false
		for _, rec := range recommendations {
			if rec == "Consider Standard tier for small objects to avoid IA minimum charges" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Should recommend Standard tier for small objects")
		}
	})

	t.Run("Access Pattern Recommendations", func(t *testing.T) {
		validator := NewTierValidator(TierStandard, TierConstraints{}, logger)

		// Infrequent access should recommend IA tiers
		recommendations := validator.GetRecommendations(1024*1024, "infrequent") // 1MB
		found := false
		for _, rec := range recommendations {
			if rec == "Consider Standard-IA or One Zone-IA for cost savings" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Should recommend IA tiers for infrequent access")
		}
	})
}

func TestStorageClassConversion(t *testing.T) {
	// Test AWS SDK conversion
	if ConvertTierToStorageClass(TierStandard) != s3types.StorageClassStandard {
		t.Error("Standard tier should convert to STANDARD storage class")
	}

	if ConvertTierToStorageClass(TierStandardIA) != s3types.StorageClassStandardIa {
		t.Error("Standard-IA tier should convert to STANDARD_IA storage class")
	}

	// Test CargoShip conversion
	if ConvertTierToCargoShipStorageClass(TierStandard) != awsconfig.StorageClassStandard {
		t.Error("Standard tier should convert to CargoShip STANDARD storage class")
	}
}

func TestTierCostCalculation(t *testing.T) {
	// Test cost calculation
	standardTier := StorageTiers[TierStandard]
	expectedCost := 100.0 * standardTier.CostPerGBMonth // 100GB

	if expectedCost != 100.0*0.023 {
		t.Errorf("Expected cost calculation %f, got %f", 100.0*0.023, expectedCost)
	}
}
