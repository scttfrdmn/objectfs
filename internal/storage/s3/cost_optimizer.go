package s3

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// Access Frequency Constants
const (
	AccessNever = "never"
	AccessCold  = "cold"
)

// CostOptimizer handles cost optimization decisions and Standard tier overhead management
type CostOptimizer struct {
	backend        *Backend
	config         CostOptimization
	logger         *slog.Logger
	accessPatterns map[string]*AccessPattern
	costThreshold  float64
}

// AccessPattern tracks object access patterns for cost optimization
type AccessPattern struct {
	ObjectKey       string        `json:"object_key"`
	AccessCount     int64         `json:"access_count"`
	LastAccessTime  time.Time     `json:"last_access_time"`
	FirstAccessTime time.Time     `json:"first_access_time"`
	AvgAccessGap    time.Duration `json:"avg_access_gap"`
	ObjectSize      int64         `json:"object_size"`
	CurrentTier     string        `json:"current_tier"`
	EstimatedCost   float64       `json:"estimated_cost"`
}

// NewCostOptimizer creates a new cost optimizer
func NewCostOptimizer(backend *Backend, config CostOptimization, logger *slog.Logger) *CostOptimizer {
	return &CostOptimizer{
		backend:        backend,
		config:         config,
		logger:         logger,
		accessPatterns: make(map[string]*AccessPattern),
		costThreshold:  config.CostThreshold,
	}
}

// RecordAccess records an access pattern for cost optimization analysis
func (co *CostOptimizer) RecordAccess(objectKey string, objectSize int64) {
	if !co.config.MonitorAccessPatterns {
		return
	}

	now := time.Now()
	pattern, exists := co.accessPatterns[objectKey]

	if !exists {
		pattern = &AccessPattern{
			ObjectKey:       objectKey,
			AccessCount:     1,
			LastAccessTime:  now,
			FirstAccessTime: now,
			AvgAccessGap:    0,
			ObjectSize:      objectSize,
			CurrentTier:     co.backend.currentTier,
			EstimatedCost:   co.calculateObjectCost(objectSize, co.backend.currentTier),
		}
		co.accessPatterns[objectKey] = pattern
	} else {
		// Update access pattern
		pattern.AccessCount++
		pattern.LastAccessTime = now

		// Calculate rolling average access gap
		if pattern.AccessCount > 1 {
			totalTime := now.Sub(pattern.FirstAccessTime)
			pattern.AvgAccessGap = totalTime / time.Duration(pattern.AccessCount-1)
		}
	}

	co.logger.Debug("Access pattern recorded",
		"object", objectKey,
		"access_count", pattern.AccessCount,
		"avg_gap", pattern.AvgAccessGap,
		"current_tier", pattern.CurrentTier)
}

// AnalyzeAndOptimize analyzes access patterns and suggests/applies optimizations
func (co *CostOptimizer) AnalyzeAndOptimize(ctx context.Context) error {
	if !co.config.EnableAutoTiering {
		return nil
	}

	optimizations := make([]TierOptimization, 0)

	for _, pattern := range co.accessPatterns {
		optimization := co.analyzeObject(pattern)
		if optimization != nil {
			optimizations = append(optimizations, *optimization)
		}
	}

	// Apply optimizations
	for _, opt := range optimizations {
		if err := co.applyOptimization(ctx, opt); err != nil {
			co.logger.Error("Failed to apply tier optimization",
				"object", opt.ObjectKey,
				"from_tier", opt.FromTier,
				"to_tier", opt.ToTier,
				"error", err)
		} else {
			co.logger.Info("Applied tier optimization",
				"object", opt.ObjectKey,
				"from_tier", opt.FromTier,
				"to_tier", opt.ToTier,
				"cost_savings", opt.EstimatedMonthlySavings)
		}
	}

	return nil
}

// TierOptimization represents a suggested tier optimization
type TierOptimization struct {
	ObjectKey               string  `json:"object_key"`
	FromTier                string  `json:"from_tier"`
	ToTier                  string  `json:"to_tier"`
	Reason                  string  `json:"reason"`
	EstimatedMonthlySavings float64 `json:"estimated_monthly_savings"`
	ConfidenceLevel         float64 `json:"confidence_level"`
	ObjectSize              int64   `json:"object_size"`
	AccessFrequency         string  `json:"access_frequency"`
}

// analyzeObject analyzes a single object's access pattern and suggests optimization
func (co *CostOptimizer) analyzeObject(pattern *AccessPattern) *TierOptimization {
	// Skip analysis if object is too young (less than 30 days)
	objectAge := time.Since(pattern.FirstAccessTime)
	if objectAge < 30*24*time.Hour {
		return nil
	}

	// Determine access frequency
	accessFreq := co.categorizeAccessFrequency(pattern)
	currentCost := co.calculateObjectCost(pattern.ObjectSize, pattern.CurrentTier)

	// Find optimal tier based on access pattern
	optimalTier := co.findOptimalTier(pattern, accessFreq)
	if optimalTier == pattern.CurrentTier {
		return nil // Already optimal
	}

	optimalCost := co.calculateObjectCost(pattern.ObjectSize, optimalTier)
	savings := currentCost - optimalCost

	// Only suggest optimization if savings exceed threshold
	if savings <= 0 || savings < co.costThreshold {
		return nil
	}

	return &TierOptimization{
		ObjectKey:               pattern.ObjectKey,
		FromTier:                pattern.CurrentTier,
		ToTier:                  optimalTier,
		Reason:                  co.generateOptimizationReason(pattern, accessFreq),
		EstimatedMonthlySavings: savings,
		ConfidenceLevel:         co.calculateConfidence(pattern),
		ObjectSize:              pattern.ObjectSize,
		AccessFrequency:         accessFreq,
	}
}

// categorizeAccessFrequency categorizes access patterns
func (co *CostOptimizer) categorizeAccessFrequency(pattern *AccessPattern) string {
	if pattern.AccessCount == 0 {
		return AccessNever
	}

	// Calculate accesses per day
	objectAge := time.Since(pattern.FirstAccessTime)
	accessesPerDay := float64(pattern.AccessCount) / objectAge.Hours() * 24

	if accessesPerDay >= 1.0 {
		return AccessFrequent
	} else if accessesPerDay >= 0.1 { // At least once per 10 days
		return AccessInfrequent
	} else if pattern.AccessCount > 0 && objectAge > 90*24*time.Hour && accessesPerDay >= 0.01 {
		return AccessArchive
	} else {
		return AccessCold
	}
}

// findOptimalTier finds the most cost-effective tier for an access pattern
func (co *CostOptimizer) findOptimalTier(pattern *AccessPattern, accessFreq string) string {
	objectSizeGB := float64(pattern.ObjectSize) / (1024 * 1024 * 1024)

	// Handle Standard tier overhead: small objects often stay in Standard
	if pattern.ObjectSize < 128*1024 && accessFreq != AccessNever {
		return TierStandard // Avoid IA minimum charges for small, accessed objects
	}

	switch accessFreq {
	case AccessFrequent:
		return TierStandard
	case AccessInfrequent:
		if pattern.ObjectSize >= 128*1024 { // Meet IA minimum size
			return TierStandardIA
		}
		return TierStandard
	case AccessArchive:
		if pattern.ObjectSize >= 128*1024 {
			return TierGlacierIR
		}
		return TierStandardIA
	case AccessCold, AccessNever:
		if objectSizeGB > 1.0 { // Large objects benefit more from deep archive
			return TierGlacier
		}
		return TierGlacierIR
	default:
		return TierIntelligent // Let AWS decide
	}
}

// calculateObjectCost calculates monthly storage cost for an object in a tier
func (co *CostOptimizer) calculateObjectCost(objectSize int64, tier string) float64 {
	// Use pricing manager to get accurate pricing with discounts
	tierPricing, err := co.backend.pricingManager.GetTierPricing(tier)
	if err != nil {
		co.logger.Warn("Failed to get tier pricing, using defaults", "tier", tier, "error", err)
		// Fall back to default pricing
		tierInfo, exists := StorageTiers[tier]
		if !exists {
			tierInfo = StorageTiers[TierStandard]
		}
		tierPricing = TierPricing{
			StorageCostPerGBMonth: tierInfo.CostPerGBMonth,
			MinimumBillableSize:   tierInfo.MinObjectSize,
		}
	}

	objectSizeGB := float64(objectSize) / (1024 * 1024 * 1024)

	// Handle minimum object size charges
	if objectSize < tierPricing.MinimumBillableSize {
		// Charge for minimum size
		minSizeGB := float64(tierPricing.MinimumBillableSize) / (1024 * 1024 * 1024)
		baseCost := minSizeGB * tierPricing.StorageCostPerGBMonth
		// Apply volume discounts
		return co.backend.pricingManager.CalculateVolumeDiscount(tier, minSizeGB, baseCost)
	}

	baseCost := objectSizeGB * tierPricing.StorageCostPerGBMonth
	// Apply volume discounts
	return co.backend.pricingManager.CalculateVolumeDiscount(tier, objectSizeGB, baseCost)
}

// generateOptimizationReason generates a human-readable reason for optimization
func (co *CostOptimizer) generateOptimizationReason(pattern *AccessPattern, accessFreq string) string {
	switch accessFreq {
	case AccessFrequent:
		return "High access frequency - Standard tier optimal"
	case AccessInfrequent:
		return "Infrequent access pattern - IA tier more cost-effective"
	case AccessArchive:
		return "Archive access pattern - Glacier tier significant savings"
	case AccessCold, AccessNever:
		return "Rarely accessed - Deep archive substantial cost reduction"
	default:
		return "Access pattern suggests tier optimization opportunity"
	}
}

// calculateConfidence calculates confidence level for optimization suggestion
func (co *CostOptimizer) calculateConfidence(pattern *AccessPattern) float64 {
	// Base confidence on data quality
	confidence := 0.5 // Base confidence

	// More accesses = higher confidence
	if pattern.AccessCount >= 10 {
		confidence += 0.2
	} else if pattern.AccessCount >= 5 {
		confidence += 0.1
	}

	// Longer observation period = higher confidence
	objectAge := time.Since(pattern.FirstAccessTime)
	if objectAge >= 90*24*time.Hour {
		confidence += 0.2
	} else if objectAge >= 30*24*time.Hour {
		confidence += 0.1
	}

	// Consistent access pattern = higher confidence
	if pattern.AvgAccessGap > 0 {
		confidence += 0.1
	}

	return math.Min(confidence, 1.0)
}

// applyOptimization applies a tier optimization (placeholder for S3 API integration)
func (co *CostOptimizer) applyOptimization(ctx context.Context, opt TierOptimization) error {
	// In a real implementation, this would use S3 API to change object storage class
	// For now, just log the optimization
	co.logger.Info("Would apply tier optimization",
		"object", opt.ObjectKey,
		"from_tier", opt.FromTier,
		"to_tier", opt.ToTier,
		"savings", opt.EstimatedMonthlySavings,
		"confidence", opt.ConfidenceLevel)

	// Update local tracking
	if pattern, exists := co.accessPatterns[opt.ObjectKey]; exists {
		pattern.CurrentTier = opt.ToTier
		pattern.EstimatedCost = co.calculateObjectCost(pattern.ObjectSize, opt.ToTier)
	}

	return nil
}

// GetOptimizationReport generates a cost optimization report
func (co *CostOptimizer) GetOptimizationReport() OptimizationReport {
	report := OptimizationReport{
		TotalObjects:          len(co.accessPatterns),
		OptimizationResults:   make([]TierOptimization, 0),
		TotalPotentialSavings: 0,
	}

	for _, pattern := range co.accessPatterns {
		if opt := co.analyzeObject(pattern); opt != nil {
			report.OptimizationResults = append(report.OptimizationResults, *opt)
			report.TotalPotentialSavings += opt.EstimatedMonthlySavings
		}
	}

	return report
}

// OptimizationReport contains cost optimization analysis results
type OptimizationReport struct {
	TotalObjects          int                `json:"total_objects"`
	OptimizationResults   []TierOptimization `json:"optimization_results"`
	TotalPotentialSavings float64            `json:"total_potential_savings"`
	GeneratedAt           time.Time          `json:"generated_at"`
}

// Standard tier overhead handling helpers

// HandleStandardTierOverhead manages Standard tier cost overhead for small objects
func (co *CostOptimizer) HandleStandardTierOverhead(objectKey string, objectSize int64) string {
	// For objects smaller than 128KB, Standard tier avoids IA minimum charges
	if objectSize < 128*1024 {
		co.logger.Debug("Using Standard tier to avoid IA minimum charges",
			"object", objectKey,
			"size", objectSize,
			"threshold", 128*1024)
		return TierStandard
	}

	// For larger objects, use configured tier
	return co.backend.currentTier
}

// EstimateStandardTierOverhead calculates potential overhead from using Standard tier
func (co *CostOptimizer) EstimateStandardTierOverhead(objectSize int64, targetTier string) float64 {
	standardCost := co.calculateObjectCost(objectSize, TierStandard)
	targetCost := co.calculateObjectCost(objectSize, targetTier)

	if standardCost > targetCost {
		return standardCost - targetCost
	}

	return 0 // No overhead if Standard is cheaper
}
