package s3

import (
	"io"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	awsconfig "github.com/scttfrdmn/cargoship/pkg/aws/config"

	"github.com/scttfrdmn/objectfs/internal/awsname"
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
		found := slices.Contains(recommendations, "Consider Standard tier for small objects to avoid IA minimum charges")
		if !found {
			t.Error("Should recommend Standard tier for small objects")
		}
	})

	t.Run("Access Pattern Recommendations", func(t *testing.T) {
		validator := NewTierValidator(TierStandard, TierConstraints{}, logger)

		// Infrequent access should recommend IA tiers
		recommendations := validator.GetRecommendations(1024*1024, "infrequent") // 1MB
		found := slices.Contains(recommendations, "Consider Standard-IA or One Zone-IA for cost savings")
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

// TestStorageTiersCoversEveryStorageClass closes the loop between the two halves of storage_tier.
//
// internal/config validates the configured tier with awsname.ValidateStorageClass, and this package
// acts on it via StorageTiers. Those are different data structures in different packages, and the
// import cycle (see the awsname package comment) means neither can consult the other at run time.
// So this test is the only thing holding them together.
//
// A gap in either direction is a silent wrong answer, not a build failure:
//
//   - a class awsname admits with no entry here passes validation, misses the map in
//     NewTierValidator, and is silently replaced with STANDARD — the object is billed as STANDARD
//     while the operator believes they configured something else;
//   - an entry here that awsname does not admit is a tier this package can price and validate
//     against but that the loader rejects, so the feature is unreachable and looks implemented.
func TestStorageTiersCoversEveryStorageClass(t *testing.T) {
	t.Parallel()

	for _, class := range awsname.StorageClasses() {
		info, ok := StorageTiers[class]
		if !ok {
			t.Errorf("awsname admits storage class %q but StorageTiers has no entry, so a config "+
				"naming it would be accepted at load and silently downgraded to STANDARD by "+
				"NewTierValidator", class)
			continue
		}

		// An entry that exists but is blank is the same defect wearing a disguise: the lookup
		// succeeds, so no fallback fires, and the tier is then validated against a zero minimum and
		// priced at zero.
		if info.Name == "" {
			t.Errorf("StorageTiers[%q] has no Name, so logs and reports identify the tier as \"\"",
				class)
		}
		if info.CostPerGBMonth <= 0 {
			t.Errorf("StorageTiers[%q] has CostPerGBMonth %v; no S3 class is free, and a zero rate "+
				"makes every cost estimate for this tier read as $0", class, info.CostPerGBMonth)
		}
		if info.RecommendedUseCase == "" {
			t.Errorf("StorageTiers[%q] has no RecommendedUseCase, which is what GetRecommendations "+
				"and the cost report show an operator choosing a tier", class)
		}
	}

	for class := range StorageTiers {
		if err := awsname.ValidateStorageClass(class); err != nil {
			t.Errorf("StorageTiers has an entry for %q, which the config loader rejects (%v) — the "+
				"tier is priced and validated here but cannot be selected", class, err)
		}
	}

	if len(StorageTiers) != len(awsname.StorageClasses()) {
		t.Errorf("StorageTiers has %d entries against %d storage classes", len(StorageTiers),
			len(awsname.StorageClasses()))
	}
}

// TestTierConversionsCoverEveryStorageClass asserts both converters make a real decision per class.
//
// Both end in `default: STANDARD`, which is the right shape for an unparseable string arriving from
// outside and the wrong shape for a class this package is supposed to know: a ninth tier added to
// awsname and StorageTiers would compile, validate, price correctly, and then be written to S3 as
// STANDARD by the converter, with nothing anywhere reporting a problem. exhaustive cannot catch it
// because these switch on a plain string, not a named type.
//
// The expectations are spelled out rather than derived so that the two deliberate collapses stay
// deliberate: REDUCED_REDUNDANCY and GLACIER_IR have no CargoShip counterpart, and each is written
// here with the reason. A new tier makes this table fail until someone chooses.
func TestTierConversionsCoverEveryStorageClass(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		sdk       s3types.StorageClass
		cargoship awsconfig.StorageClass
		why       string
	}{
		TierStandard: {
			sdk:       s3types.StorageClassStandard,
			cargoship: awsconfig.StorageClassStandard,
		},
		TierStandardIA: {
			sdk:       s3types.StorageClassStandardIa,
			cargoship: awsconfig.StorageClassStandardIA,
		},
		TierOneZoneIA: {
			sdk:       s3types.StorageClassOnezoneIa,
			cargoship: awsconfig.StorageClassOneZoneIA,
		},
		TierGlacierIR: {
			sdk:       s3types.StorageClassGlacierIr,
			cargoship: awsconfig.StorageClassGlacier,
			why: "CargoShip has no Glacier Instant Retrieval class. The collapse is a real " +
				"downgrade — GLACIER retrieval takes minutes to hours where GLACIER_IR is " +
				"instant — so an archive written through the CargoShip path is slower to read " +
				"than the configured tier promises",
		},
		TierGlacier: {
			sdk:       s3types.StorageClassGlacier,
			cargoship: awsconfig.StorageClassGlacier,
		},
		TierDeepArchive: {
			sdk:       s3types.StorageClassDeepArchive,
			cargoship: awsconfig.StorageClassDeepArchive,
		},
		TierIntelligent: {
			sdk:       s3types.StorageClassIntelligentTiering,
			cargoship: awsconfig.StorageClassIntelligentTiering,
		},
		TierReducedRedundancy: {
			sdk:       s3types.StorageClassReducedRedundancy,
			cargoship: awsconfig.StorageClassStandard,
			why: "CargoShip has no REDUCED_REDUNDANCY class. AWS deprecated it and prices it " +
				"above STANDARD, so falling back to STANDARD is both cheaper and more durable",
		},
	}

	for _, class := range awsname.StorageClasses() {
		want, ok := cases[class]
		if !ok {
			t.Errorf("storage class %q has no conversion expectation here, so nothing checks that "+
				"ConvertTierToStorageClass does anything but fall through to STANDARD", class)
			continue
		}

		if got := ConvertTierToStorageClass(class); got != want.sdk {
			t.Errorf("ConvertTierToStorageClass(%q) = %q, want %q. %s", class, got, want.sdk, want.why)
		}
		if got := ConvertTierToCargoShipStorageClass(class); got != want.cargoship {
			t.Errorf("ConvertTierToCargoShipStorageClass(%q) = %q, want %q. %s",
				class, got, want.cargoship, want.why)
		}
	}
}

// TestNewTierValidatorFallsBackToStandard documents the behavior the loader now stands in front of.
//
// The fallback is not being changed: by the time a tier reaches this constructor the mount is coming
// up, and refusing to construct would take the filesystem down over a config typo that
// awsname.ValidateStorageClass should already have caught with a message naming the setting. What
// makes it safe is that it is now unreachable from a loaded config — this test pins the behavior so
// the guarantee is stated somewhere, and pins that the substitution is visible in GetTierInfo rather
// than leaving the validator claiming the tier it was asked for.
func TestNewTierValidatorFallsBackToStandard(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// The digit-one typo. awsname.ValidateStorageClass rejects it at config load; anything that
	// bypasses the loader lands here.
	validator := NewTierValidator("STANDARD_1A", TierConstraints{}, logger)

	info := validator.GetTierInfo()
	if info.Name != StorageTiers[TierStandard].Name {
		t.Fatalf("an unknown tier produced tier info %q; it must fall back to Standard so the "+
			"validator's minimum-size and embargo checks are at least self-consistent", info.Name)
	}

	// The point of the fallback being STANDARD specifically: its minimum object size is zero, so a
	// mount that reached here still writes small files rather than rejecting them. Falling back to
	// an IA tier would turn a config typo into EIO on every write under 128 KiB.
	if err := validator.ValidateWrite("small.txt", 1); err != nil {
		t.Errorf("the fallback tier rejected a 1-byte write, so a config typo would surface as a "+
			"write failure rather than a storage-class difference: %v", err)
	}

	if err := awsname.ValidateStorageClass("STANDARD_1A"); err == nil {
		t.Error("awsname.ValidateStorageClass accepts STANDARD_1A, so this fallback is reachable " +
			"from a loaded config and an operator's configured tier can be silently ignored")
	}
}
