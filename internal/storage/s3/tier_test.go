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

// The two AWS pages that publish the figures the next test pins, named once so each failure can cite
// the one a reader has to open.
const (
	awsStorageClassTable = "https://docs.aws.amazon.com/AmazonS3/latest/userguide/" +
		"storage-class-intro.html#sc-compare"
	awsS3Pricing = "https://aws.amazon.com/s3/pricing/"
)

// TestTierSizeThresholdsMatchWhatAWSPublishes pins every size threshold in StorageTiers against the
// AWS page that publishes it, for all eight classes rather than the four TestStorageTiers covers.
//
// It exists because three of these values were wrong and were wrong in a way that reads as
// deliberate. MinObjectSize held 40 KB for GLACIER and DEEP_ARCHIVE and 128 KB for
// INTELLIGENT_TIERING, each with a confident comment ("40 KB minimum", "128 KB minimum for
// optimization"), and AWS publishes no minimum billable object size for any of the three. The
// comments are why it survived review: a number with a stated reason looks checked.
//
// Both of the wrong numbers are real AWS numbers used for the wrong thing, which is the harder error
// to see. The archive classes' 40 KB is per-object metadata AWS bills *in addition* to the object —
// 32 KB at the archive rate, 8 KB at Standard's — and Intelligent-Tiering's 128 KB is the size below
// which an object is not monitored, not auto-tiered, and not charged the automation fee. A floor and
// a surcharge point opposite ways for every small-object recommendation: under a floor, compressing a
// 30 KB object saves nothing, and under a surcharge it saves every byte it removes.
//
// The threshold is asserted zero where AWS publishes none, not merely "not 128 KB", because zero is
// what the arithmetic in calculateObjectCost and the gate in ValidateWrite both read.
//
// This is the shape of internal/awsrates/rates_aws_test.go applied to the constraints rather than the
// rates: that file exists because a table someone believed is a different thing from a table
// something checked. The rates can be re-read from the live Pricing API; these thresholds cannot,
// since the Pricing API does not publish them, so the citation in the failure message is the
// substitute — it names the page to open rather than asserting the reader will remember which.
func TestTierSizeThresholdsMatchWhatAWSPublishes(t *testing.T) {
	t.Parallel()

	// 128 KB and 40 KB as AWS writes them, read here as binary KiB. See the constants in tiers.go for
	// why that choice is explicit: AWS does not say which a KB is, and 1024 overstates the threshold
	// by 2.4%, which is the conservative direction for both a floor and a surcharge.
	const (
		kb128 = 128 * 1024
		kb40  = 40 * 1024
	)

	cases := map[string]struct {
		minBillable   int64
		overhead      int64
		monitoringMin int64
		source        string
		why           string
	}{
		TierStandard: {
			source: awsS3Pricing,
			why:    "no minimum billable size, no per-object overhead",
		},
		TierReducedRedundancy: {
			source: awsS3Pricing,
			why:    "deprecated, and never had a minimum",
		},
		TierStandardIA: {
			minBillable: kb128,
			source:      awsS3Pricing,
			why:         "AWS bills a 128 KB minimum on Standard-IA",
		},
		TierOneZoneIA: {
			minBillable: kb128,
			source:      awsS3Pricing,
			why:         "AWS bills a 128 KB minimum on One Zone-IA",
		},
		TierGlacierIR: {
			minBillable: kb128,
			source:      awsS3Pricing,
			why:         "AWS bills a 128 KB minimum on Glacier Instant Retrieval",
		},
		TierGlacier: {
			overhead: kb40,
			source:   awsStorageClassTable,
			why: "the storage class table lists min billable object size as NA for Glacier Flexible " +
				"Retrieval; its 40 KB is per-object metadata billed on top of the object",
		},
		TierDeepArchive: {
			overhead: kb40,
			source:   awsStorageClassTable,
			why: "the storage class table lists min billable object size as NA for Deep Archive; its " +
				"40 KB is per-object metadata billed on top of the object",
		},
		TierIntelligent: {
			monitoringMin: kb128,
			source:        awsStorageClassTable,
			why: "the storage class table lists min billable object size as None for " +
				"Intelligent-Tiering; its 128 KB bounds auto-tiering monitoring, not billing",
		},
	}

	// Every class, so a ninth tier cannot arrive without a decision about its thresholds.
	for _, class := range awsname.StorageClasses() {
		if _, ok := cases[class]; !ok {
			t.Errorf("awsname admits storage class %q and this test has no expectation for its size "+
				"thresholds. Add one, citing the AWS page: %s", class, awsStorageClassTable)
		}
	}

	for tier, want := range cases {
		t.Run(tier, func(t *testing.T) {
			t.Parallel()

			info, ok := StorageTiers[tier]
			if !ok {
				t.Fatalf("StorageTiers has no entry for %q", tier)
			}

			if info.MinObjectSize != want.minBillable {
				t.Errorf("StorageTiers[%q].MinObjectSize = %d, want %d.\n%s\nSource: %s\n\n"+
					"This field is AWS's minimum *billable* object size and nothing else. A value here "+
					"makes ValidateWrite refuse smaller writes and makes calculateObjectCost bill them "+
					"as this size — so a number that is not a minimum produces a refusal AWS would not "+
					"make and a cost AWS would not charge.",
					tier, info.MinObjectSize, want.minBillable, want.why, want.source)
			}

			if info.PerObjectOverheadBytes != want.overhead {
				t.Errorf("StorageTiers[%q].PerObjectOverheadBytes = %d, want %d.\n%s\nSource: %s\n\n"+
					"This is billed in addition to the object, not instead of a smaller size. Only "+
					"GLACIER and DEEP_ARCHIVE have it.",
					tier, info.PerObjectOverheadBytes, want.overhead, want.why, want.source)
			}

			if info.MonitoringEligibilityBytes != want.monitoringMin {
				t.Errorf("StorageTiers[%q].MonitoringEligibilityBytes = %d, want %d.\n%s\nSource: %s\n\n"+
					"Only INTELLIGENT_TIERING has one, and it governs whether an object is monitored "+
					"and auto-tiered — not what it is billed.",
					tier, info.MonitoringEligibilityBytes, want.monitoringMin, want.why, want.source)
			}

			// The two are mutually exclusive on every class AWS publishes, and the exclusion is the
			// invariant that broke: the 40 KB sat in MinObjectSize because there was no other field for
			// it. If a future class has both, this assertion is the place to record why.
			if info.MinObjectSize > 0 && info.PerObjectOverheadBytes > 0 {
				t.Errorf("StorageTiers[%q] has both a minimum billable size (%d) and a per-object "+
					"overhead (%d). No S3 class published today has both, and the two are "+
					"arithmetically opposite — a minimum replaces the object's size, an overhead adds "+
					"to it. If AWS has introduced a class with both, calculateObjectCost needs to be "+
					"read again before this assertion is relaxed.",
					tier, info.MinObjectSize, info.PerObjectOverheadBytes)
			}
		})
	}

	// The split, asserted separately: it is what keeps the 8 KB portion from being priced at an
	// archive rate, which understates it about 23-fold on DEEP_ARCHIVE.
	for _, tier := range []string{TierGlacier, TierDeepArchive} {
		archiveBytes, standardBytes := ArchiveOverhead(tier)

		if archiveBytes != 32*1024 || standardBytes != 8*1024 {
			t.Errorf("ArchiveOverhead(%q) = (%d, %d), want (32768, 8192).\nAWS bills 32 KB of the 40 "+
				"at the archive class's rate and 8 KB at the S3 Standard rate.\nSource: %s",
				tier, archiveBytes, standardBytes, awsStorageClassTable)
		}

		if archiveBytes+standardBytes != StorageTiers[tier].PerObjectOverheadBytes {
			t.Errorf("ArchiveOverhead(%q) sums to %d but PerObjectOverheadBytes is %d; the split and "+
				"the total have to agree or one of the two callers is pricing a different object",
				tier, archiveBytes+standardBytes, StorageTiers[tier].PerObjectOverheadBytes)
		}
	}

	// And zero for everything else, so the helper cannot start reporting an overhead for a class that
	// has none.
	for _, class := range awsname.StorageClasses() {
		if class == TierGlacier || class == TierDeepArchive {
			continue
		}

		if a, s := ArchiveOverhead(class); a != 0 || s != 0 {
			t.Errorf("ArchiveOverhead(%q) = (%d, %d), want (0, 0) — only the two archive classes bill "+
				"a per-object overhead", class, a, s)
		}
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
