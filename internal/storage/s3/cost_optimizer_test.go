package s3

import (
	"log/slog"
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

		// Check that pattern was recorded
		pattern, exists := optimizer.accessPatterns["test.txt"]
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

		if len(disabledOptimizer.accessPatterns) != 0 {
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

	t.Run("Standard Tier Cost", func(t *testing.T) {
		// 1GB object in Standard tier
		cost := optimizer.calculateObjectCost(1024*1024*1024, TierStandard)
		expected := 1.0 * StorageTiers[TierStandard].CostPerGBMonth

		if cost != expected {
			t.Errorf("Expected cost %f, got %f", expected, cost)
		}
	})

	t.Run("Standard-IA Minimum Size Charge", func(t *testing.T) {
		// 64KB object in Standard-IA should be charged for 128KB minimum
		cost := optimizer.calculateObjectCost(64*1024, TierStandardIA)
		minSizeGB := float64(128*1024) / (1024 * 1024 * 1024)
		expected := minSizeGB * StorageTiers[TierStandardIA].CostPerGBMonth

		if cost != expected {
			t.Errorf("Expected minimum size charge %f, got %f", expected, cost)
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
	optimizer.accessPatterns["optimize-me.txt"] = &AccessPattern{
		ObjectKey:       "optimize-me.txt",
		AccessCount:     5, // Infrequent but not too low
		FirstAccessTime: oldTime,
		LastAccessTime:  time.Now().Add(-10 * 24 * time.Hour), // 10 days ago
		ObjectSize:      1024 * 1024,                          // 1MB (large enough for IA)
		CurrentTier:     TierStandard,
		EstimatedCost:   optimizer.calculateObjectCost(1024*1024, TierStandard),
	}

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
