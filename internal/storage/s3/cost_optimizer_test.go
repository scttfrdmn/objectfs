package s3

import (
	"log/slog"
	"math"
	"os"
	"testing"
	"time"
)

func TestCostOptimizer_StandardTierOverhead(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create cost optimizer with monitoring enabled
	config := CostOptimization{
		EnableAutoTiering:     false,
		MonitorAccessPatterns: true,
		CostThreshold:         0.001, // $0.001/GB/month threshold
	}

	// Create mock backend
	backend := &Backend{
		currentTier: TierStandardIA,
		config: &Config{
			CostOptimization: config,
		},
	}

	// Initialize pricing manager for the backend
	backend.pricingManager = NewPricingManager(PricingConfig{}, logger)

	optimizer := NewCostOptimizer(backend, config, logger)

	t.Run("Small Object Uses Standard Tier", func(t *testing.T) {
		// Small object (64KB) should use Standard tier to avoid IA minimum charges
		effectiveTier := optimizer.HandleStandardTierOverhead("small.txt", 64*1024)

		if effectiveTier != TierStandard {
			t.Errorf("Expected Standard tier for small object, got %s", effectiveTier)
		}
	})

	t.Run("Large Object Uses Configured Tier", func(t *testing.T) {
		// Large object (1MB) should use configured tier (Standard-IA)
		effectiveTier := optimizer.HandleStandardTierOverhead("large.txt", 1024*1024)

		if effectiveTier != TierStandardIA {
			t.Errorf("Expected Standard-IA tier for large object, got %s", effectiveTier)
		}
	})
}

func TestCostOptimizer_AccessPatternRecording(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	config := CostOptimization{
		MonitorAccessPatterns: true,
	}

	backend := &Backend{
		currentTier: TierStandard,
	}

	// Initialize pricing manager for the backend
	backend.pricingManager = NewPricingManager(PricingConfig{}, logger)

	optimizer := NewCostOptimizer(backend, config, logger)

	t.Run("Records Access Pattern", func(t *testing.T) {
		// Record multiple accesses
		optimizer.RecordAccess("test.txt", 1024)
		optimizer.RecordAccess("test.txt", 1024)
		optimizer.RecordAccess("test.txt", 1024)

		// Check that pattern was recorded. Read through patternFor rather than indexing the map:
		// accessPatterns is guarded by co.mu, and a test that reaches past the lock is the template
		// the next test copies.
		pattern, exists := optimizer.patternFor("test.txt")
		if !exists {
			t.Fatal("Access pattern should be recorded")
		}

		if pattern.AccessCount != 3 {
			t.Errorf("Expected 3 accesses, got %d", pattern.AccessCount)
		}

		if pattern.ObjectSize != 1024 {
			t.Errorf("Expected object size 1024, got %d", pattern.ObjectSize)
		}
	})

	t.Run("Skips Recording When Disabled", func(t *testing.T) {
		disabledConfig := CostOptimization{
			MonitorAccessPatterns: false,
		}

		// Create separate backend for disabled test
		disabledBackend := &Backend{
			currentTier: TierStandard,
		}
		disabledBackend.pricingManager = NewPricingManager(PricingConfig{}, logger)

		disabledOptimizer := NewCostOptimizer(disabledBackend, disabledConfig, logger)
		disabledOptimizer.RecordAccess("disabled.txt", 1024)

		if disabledOptimizer.PatternCount() != 0 {
			t.Error("Should not record access patterns when disabled")
		}
	})
}

func TestCostOptimizer_AccessFrequencyCategories(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	config := CostOptimization{}
	backend := &Backend{currentTier: TierStandard}
	backend.pricingManager = NewPricingManager(PricingConfig{}, logger)
	optimizer := NewCostOptimizer(backend, config, logger)

	tests := []struct {
		name         string
		accessCount  int64
		objectAge    time.Duration
		expectedFreq string
	}{
		{
			name:         "Frequent Access",
			accessCount:  100,
			objectAge:    30 * 24 * time.Hour, // 30 days
			expectedFreq: "frequent",
		},
		{
			name:         "Infrequent Access",
			accessCount:  5,
			objectAge:    30 * 24 * time.Hour, // 30 days
			expectedFreq: "infrequent",
		},
		{
			name:         "Archive Access",
			accessCount:  2,
			objectAge:    120 * 24 * time.Hour, // 120 days
			expectedFreq: "archive",
		},
		{
			name:         "Cold Access",
			accessCount:  1,
			objectAge:    200 * 24 * time.Hour, // 200 days
			expectedFreq: "cold",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pattern := &AccessPattern{
				AccessCount:     tt.accessCount,
				FirstAccessTime: time.Now().Add(-tt.objectAge),
				ObjectSize:      1024 * 1024, // 1MB
			}

			freq := optimizer.categorizeAccessFrequency(pattern)
			if freq != tt.expectedFreq {
				t.Errorf("Expected frequency %s, got %s", tt.expectedFreq, freq)
			}
		})
	}
}

func TestCostOptimizer_CostCalculation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	config := CostOptimization{}
	backend := &Backend{currentTier: TierStandard}
	backend.pricingManager = NewPricingManager(PricingConfig{}, logger)
	optimizer := NewCostOptimizer(backend, config, logger)

	// Every expectation below is a hand-computed dollar figure written as a literal, not a formula
	// over the same fields the code reads.
	//
	// That is the point, and this test is here because the previous version demonstrates why. It
	// passed 1024*1024*1024 bytes, called it "1GB", and asserted `1.0 * CostPerGBMonth` — an
	// expectation that holds whether the code divides by 10^9 or by 2^30, because the test made the
	// same choice the code did. AWS bills GB-months in decimal GB, so 2^30 understates every storage
	// figure by 7.4%, and this assertion could not see it. A test that recomputes the implementation's
	// formula agrees with the implementation by construction.
	const tolerance = 1e-12

	t.Run("one decimal GB in Standard", func(t *testing.T) {
		t.Parallel()

		// Exactly 10^9 bytes at $0.023/GB-month.
		cost := optimizer.calculateObjectCost(1_000_000_000, TierStandard)

		if math.Abs(cost-0.023) > tolerance {
			t.Errorf("1 decimal GB in Standard = $%.10f, want $0.023", cost)
		}
	})

	t.Run("one binary GiB in Standard costs 7.4% more than a GB", func(t *testing.T) {
		t.Parallel()

		// 2^30 bytes is 1.073741824 decimal GB, so $0.023 × 1.073741824 = $0.024696061952.
		// This is the case the old test asserted as $0.023 flat.
		cost := optimizer.calculateObjectCost(1024*1024*1024, TierStandard)

		if math.Abs(cost-0.024696061952) > tolerance {
			t.Errorf("1 GiB in Standard = $%.12f, want $0.024696061952 — if this reads $0.023, "+
				"something is dividing bytes by 2^30 to get GB again", cost)
		}
	})

	t.Run("a 64 KB object in Standard-IA is billed at the 128 KB minimum", func(t *testing.T) {
		t.Parallel()

		// 131,072 bytes billable (the minimum replaces the object's 65,536) = 1.31072e-4 GB
		// × $0.0125/GB-month = $0.000001638400.
		cost := optimizer.calculateObjectCost(64*kib, TierStandardIA)

		if math.Abs(cost-0.0000016384) > tolerance {
			t.Errorf("64 KB in Standard-IA = $%.12f, want $0.000001638400 (billed as 128 KB)", cost)
		}
	})

	t.Run("the minimum does not raise an object already above it", func(t *testing.T) {
		t.Parallel()

		// 1 MiB = 1,048,576 bytes = 1.048576e-3 GB × $0.0125 = $0.0000131072.
		cost := optimizer.calculateObjectCost(1024*kib, TierStandardIA)

		if math.Abs(cost-0.0000131072) > tolerance {
			t.Errorf("1 MiB in Standard-IA = $%.12f, want $0.000013107200", cost)
		}
	})

	// The two archive classes have no minimum billable size and do have a per-object overhead, which
	// is the #229 fix. These assertions are the ones that would fail if the 40 KB went back into
	// MinObjectSize: under a minimum, a 10 KB object is billed as 40 KB; under an overhead it is
	// billed as 50 KB, split across two rates.
	t.Run("a small Deep Archive object pays its bytes plus the 40 KB overhead", func(t *testing.T) {
		t.Parallel()

		// 10,240 bytes payload + 32,768 bytes at the Deep Archive rate = 43,008 bytes
		//   → 4.3008e-5 GB × $0.00099 = $0.0000000425779200
		// 8,192 bytes at the S3 Standard rate
		//   → 8.192e-6 GB × $0.023   = $0.0000001884160000
		// total                        = $0.0000002309939200
		cost := optimizer.calculateObjectCost(10*kib, TierDeepArchive)

		if math.Abs(cost-0.00000023099392) > tolerance {
			// Both signatures below were checked by making the mutation, not predicted: putting the
			// 40 KB back in MinObjectSize gives exactly $0.00000004055040, and pricing the whole 40 KB
			// at the archive rate gives $0.00000005068800.
			t.Errorf("10 KB in Deep Archive = $%.14f, want $0.00000023099392.\nIf this came back "+
				"$0.0000000406 the 40 KB is being treated as a minimum billable size again, and if it "+
				"came back $0.0000000507 the 8 KB portion is being priced at the archive rate instead "+
				"of Standard's", cost)
		}
	})

	t.Run("the overhead is a surcharge, so a larger object pays it too", func(t *testing.T) {
		t.Parallel()

		// 1 MiB + 32 KiB at the Deep Archive rate = 1,081,344 bytes
		//   → 1.081344e-3 GB × $0.00099 = $0.00000107053056
		// plus the same 8 KiB at Standard = $0.000000188416
		// total                          = $0.00000125894656
		cost := optimizer.calculateObjectCost(1024*kib, TierDeepArchive)

		if math.Abs(cost-0.00000125894656) > tolerance {
			t.Errorf("1 MiB in Deep Archive = $%.14f, want $0.00000125894656 — the overhead applies "+
				"at every size, not only below 40 KB", cost)
		}
	})

	t.Run("Intelligent-Tiering has no minimum, so a small object is billed for its bytes", func(t *testing.T) {
		t.Parallel()

		// 10,240 bytes = 1.024e-5 GB × $0.023 = $0.00000023552. Under the old table this object was
		// billed as 128 KB, about 12.8× too much, on the strength of a number that governs monitoring
		// eligibility rather than billing.
		cost := optimizer.calculateObjectCost(10*kib, TierIntelligent)

		if math.Abs(cost-0.00000023552) > tolerance {
			t.Errorf("10 KB in Intelligent-Tiering = $%.14f, want $0.00000023552", cost)
		}
	})
}

func TestCostOptimizer_OptimalTierSelection(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	config := CostOptimization{}
	backend := &Backend{currentTier: TierStandard}
	backend.pricingManager = NewPricingManager(PricingConfig{}, logger)
	optimizer := NewCostOptimizer(backend, config, logger)

	tests := []struct {
		name         string
		objectSize   int64
		accessFreq   string
		expectedTier string
	}{
		{
			name:         "Small Frequent Object",
			objectSize:   64 * 1024, // 64KB
			accessFreq:   "frequent",
			expectedTier: TierStandard,
		},
		{
			name:         "Large Infrequent Object",
			objectSize:   1024 * 1024, // 1MB
			accessFreq:   "infrequent",
			expectedTier: TierStandardIA,
		},
		{
			name:         "Small Infrequent Object",
			objectSize:   64 * 1024, // 64KB
			accessFreq:   "infrequent",
			expectedTier: TierStandard, // Avoid IA minimum charges
		},
		{
			name:         "Archive Object",
			objectSize:   1024 * 1024, // 1MB
			accessFreq:   "archive",
			expectedTier: TierGlacierIR,
		},
		{
			name:         "Cold Large Object",
			objectSize:   2 * 1024 * 1024 * 1024, // 2GB
			accessFreq:   "cold",
			expectedTier: TierGlacier,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pattern := &AccessPattern{
				ObjectSize: tt.objectSize,
			}

			tier := optimizer.findOptimalTier(pattern, tt.accessFreq)
			if tier != tt.expectedTier {
				t.Errorf("Expected tier %s, got %s", tt.expectedTier, tier)
			}
		})
	}
}

func TestCostOptimizer_StandardTierOverheadEstimation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	config := CostOptimization{}
	backend := &Backend{currentTier: TierStandardIA}
	backend.pricingManager = NewPricingManager(PricingConfig{}, logger)
	optimizer := NewCostOptimizer(backend, config, logger)

	t.Run("Standard More Expensive Than IA", func(t *testing.T) {
		// For large objects, Standard is more expensive than IA
		overhead := optimizer.EstimateStandardTierOverhead(1024*1024*1024, TierStandardIA) // 1GB

		if overhead <= 0 {
			t.Error("Should have overhead when Standard is more expensive")
		}
	})

	t.Run("No Overhead When Standard is Cheaper", func(t *testing.T) {
		// For small objects where IA has minimum charges, no overhead
		overhead := optimizer.EstimateStandardTierOverhead(64*1024, TierStandardIA) // 64KB

		if overhead != 0 {
			t.Error("Should have no overhead when Standard is cheaper due to IA minimum charges")
		}
	})
}

func TestCostOptimizer_OptimizationReport(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	config := CostOptimization{
		CostThreshold: 0.000001, // Very low threshold for testing (1 micro-dollar)
	}
	backend := &Backend{currentTier: TierStandard}
	backend.pricingManager = NewPricingManager(PricingConfig{}, logger)
	optimizer := NewCostOptimizer(backend, config, logger)

	// Create a pattern that should be optimized (infrequent access on Standard tier)
	oldTime := time.Now().Add(-90 * 24 * time.Hour) // 90 days ago (older than 30 day minimum)
	optimizer.putPattern(AccessPattern{
		ObjectKey:       "optimize-me.txt",
		AccessCount:     5, // Infrequent but not too low
		FirstAccessTime: oldTime,
		LastAccessTime:  time.Now().Add(-10 * 24 * time.Hour), // 10 days ago
		ObjectSize:      1024 * 1024,                          // 1MB (large enough for IA)
		CurrentTier:     TierStandard,
		EstimatedCost:   optimizer.calculateObjectCost(1024*1024, TierStandard),
	})

	report := optimizer.GetOptimizationReport()

	if report.TotalObjects != 1 {
		t.Errorf("Expected 1 total object, got %d", report.TotalObjects)
	}

	// Debug information
	if len(report.OptimizationResults) == 0 {
		// Calculate expected costs to debug
		standardCost := optimizer.calculateObjectCost(1024*1024, TierStandard)
		iaCost := optimizer.calculateObjectCost(1024*1024, TierStandardIA)
		savings := standardCost - iaCost

		t.Logf("Debug: Standard cost=%f, IA cost=%f, savings=%f, threshold=%f",
			standardCost, iaCost, savings, config.CostThreshold)

		// If there are actually savings but no optimizations, it might be the threshold
		if savings > 0 {
			t.Error("Should have optimization suggestions - positive savings but no recommendations")
		} else {
			t.Skip("No optimization possible - IA tier might be more expensive for this object size")
		}
	}

	if len(report.OptimizationResults) > 0 && report.TotalPotentialSavings <= 0 {
		t.Error("Should have potential savings when optimizations exist")
	}
}
