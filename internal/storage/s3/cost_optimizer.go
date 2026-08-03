package s3

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// Access Frequency Constants
const (
	AccessNever = "never"
	AccessCold  = "cold"
)

// CostOptimizer handles cost optimization decisions and Standard tier overhead management
type CostOptimizer struct {
	backend       *Backend
	config        CostOptimization
	logger        *slog.Logger
	costThreshold float64

	// mu guards accessPatterns and the AccessPattern values it holds.
	//
	// RecordAccess is called from GetObject — from both the serial and the parallel read paths — so
	// the map is written from every goroutine that reads an object, and GetOptimizationReport and
	// AnalyzeAndOptimize range over it from whatever caller asks for a report. That is a concurrent
	// map write, which is not a race the runtime tolerates: it aborts the process with "concurrent
	// map writes", taking the mount and every open file descriptor with it.
	//
	// It was latent rather than live, because MonitorAccessPatterns defaults false and the mount path
	// did not map the cost-optimization block at all — so the gate at the top of RecordAccess always
	// returned early. A config knob that silently does nothing is its own defect, and the fix for
	// that one turns this one on, so the lock lands first (audit finding M12).
	//
	// The pointers are the subtle part. RecordAccess mutates *AccessPattern in place, and
	// analyzeObject reads six of its fields, so handing a caller a pointer would move the data race
	// outside the lock. Nothing here returns one.
	mu             sync.RWMutex
	accessPatterns map[string]*AccessPattern
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

// RecordAccess records an access pattern for cost optimization analysis.
//
// This is called from GetObject, on both the serial and the parallel read paths, so it runs on
// every reader goroutine — see the mu field for why that requires a lock rather than merely
// benefiting from one.
func (co *CostOptimizer) RecordAccess(objectKey string, objectSize int64) {
	if !co.config.MonitorAccessPatterns {
		return
	}

	now := time.Now()

	// The tier is read from the backend rather than passed in, so read it before taking the lock:
	// nothing here should hold co.mu while reaching into another object.
	currentTier := co.backend.currentTier

	co.mu.Lock()

	pattern, exists := co.accessPatterns[objectKey]
	if !exists {
		pattern = &AccessPattern{
			ObjectKey:       objectKey,
			AccessCount:     1,
			LastAccessTime:  now,
			FirstAccessTime: now,
			AvgAccessGap:    0,
			ObjectSize:      objectSize,
			CurrentTier:     currentTier,
			EstimatedCost:   co.calculateObjectCost(objectSize, currentTier),
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

	// Snapshot what the log line needs. Logging under the lock would hold it across a formatting
	// call and a write to the handler's io.Writer; logging *after* unlocking without a snapshot
	// would read pattern's fields with no lock at all, which is the race this method is fixing.
	accessCount, avgGap, tier := pattern.AccessCount, pattern.AvgAccessGap, pattern.CurrentTier

	co.mu.Unlock()

	co.logger.Debug("Access pattern recorded",
		"object", objectKey,
		"access_count", accessCount,
		"avg_gap", avgGap,
		"current_tier", tier)
}

// snapshotPatterns returns a copy of every tracked access pattern.
//
// Copies, not pointers: analyzeObject reads six fields of a pattern and RecordAccess writes three of
// them from any reader goroutine, so handing out the live pointer would move the race outside the
// lock while looking like it had been fixed. Copying also keeps the analysis — which reaches the
// pricing manager once per candidate tier — off the lock, so a report cannot stall the read path.
//
// The consequence is that analysis works from a consistent-per-object but not
// consistent-across-objects view. That is the right trade here: this drives an advisory tiering
// suggestion, not a correctness decision, and the alternative is holding the lock across every
// pricing lookup in the bucket.
func (co *CostOptimizer) snapshotPatterns() []AccessPattern {
	co.mu.RLock()
	defer co.mu.RUnlock()

	snapshot := make([]AccessPattern, 0, len(co.accessPatterns))
	for _, pattern := range co.accessPatterns {
		snapshot = append(snapshot, *pattern)
	}

	return snapshot
}

// PatternCount reports how many objects currently have a tracked access pattern.
func (co *CostOptimizer) PatternCount() int {
	co.mu.RLock()
	defer co.mu.RUnlock()

	return len(co.accessPatterns)
}

// putPattern installs an access pattern, replacing any pattern already tracked for the same key.
//
// It exists so that callers with a pattern in hand — today only tests, which need one aged past
// analyzeObject's 30-day floor without waiting a month — go through the lock. Taking a copy means
// the caller keeps no reference to what the map holds.
func (co *CostOptimizer) putPattern(pattern AccessPattern) {
	co.mu.Lock()
	defer co.mu.Unlock()

	co.accessPatterns[pattern.ObjectKey] = &pattern
}

// patternFor returns a copy of the tracked pattern for objectKey, if one exists.
//
// A copy for the same reason snapshotPatterns copies: the stored *AccessPattern is mutated in place
// by RecordAccess, so returning it would hand the caller a pointer into data under the lock.
func (co *CostOptimizer) patternFor(objectKey string) (AccessPattern, bool) {
	co.mu.RLock()
	defer co.mu.RUnlock()

	pattern, ok := co.accessPatterns[objectKey]
	if !ok {
		return AccessPattern{}, false
	}

	return *pattern, true
}

// AnalyzeAndOptimize analyzes access patterns and suggests/applies optimizations
func (co *CostOptimizer) AnalyzeAndOptimize(ctx context.Context) error {
	if !co.config.EnableAutoTiering {
		return nil
	}

	optimizations := make([]TierOptimization, 0)

	snapshot := co.snapshotPatterns()
	for i := range snapshot {
		optimization := co.analyzeObject(&snapshot[i])
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
	objectSizeGB := awsrates.GBFromBytes(pattern.ObjectSize)

	// Handle Standard tier overhead: small objects often stay in Standard
	if pattern.ObjectSize < minBillableSize128KB && accessFreq != AccessNever {
		return TierStandard // Avoid IA minimum charges for small, accessed objects
	}

	switch accessFreq {
	case AccessFrequent:
		return TierStandard
	case AccessInfrequent:
		if pattern.ObjectSize >= minBillableSize128KB { // Meet IA minimum size
			return TierStandardIA
		}
		return TierStandard
	case AccessArchive:
		if pattern.ObjectSize >= minBillableSize128KB {
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

// calculateObjectCost calculates monthly storage cost for an object in a tier.
//
// Three things AWS bills that this has to keep separate, because two of them were previously the same
// field and the third was the wrong unit:
//
//   - A minimum billable size *replaces* a smaller object's size. Three classes have one, at 128 KB.
//   - A per-object overhead is *added* to the object's size. Only the two archive classes have one,
//     at 40 KB, and 8 KB of that 40 is billed at the S3 Standard rate rather than the archive rate —
//     which is a 23× difference on DEEP_ARCHIVE, not a rounding detail.
//   - A GB is 10^9 bytes. This function divided by 2^30, understating every figure by 7.4%.
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

		archiveBytes, standardBytes := ArchiveOverhead(tier)

		tierPricing = TierPricing{
			StorageCostPerGBMonth:     tierInfo.CostPerGBMonth,
			MinimumBillableSize:       tierInfo.MinObjectSize,
			PerObjectOverheadBytes:    archiveBytes + standardBytes,
			OverheadStandardRateBytes: standardBytes,
		}
	}

	// The minimum, where the tier has one, is what a smaller object is billed as. It is zero on the
	// five classes AWS publishes none for, and max with zero is the object's own size.
	billableSize := max(objectSize, tierPricing.MinimumBillableSize)

	// The overhead is charged on top, split across two rates. The portion at this tier's own rate is
	// folded into billableSize; the Standard-rate portion is priced separately below, because pricing
	// it at an archive rate is where the 23× understatement would come from.
	overheadAtStandardRate := tierPricing.OverheadStandardRateBytes
	billableSize += tierPricing.PerObjectOverheadBytes - overheadAtStandardRate

	billableGB := awsrates.GBFromBytes(billableSize)
	baseCost := billableGB * tierPricing.StorageCostPerGBMonth

	// Volume discounts apply to the tier's own storage, so they are computed on that portion alone.
	cost := co.backend.pricingManager.CalculateVolumeDiscount(tier, billableGB, baseCost)

	if overheadAtStandardRate > 0 {
		standardRate, ok := awsrates.For(TierStandard)
		if ok {
			cost += awsrates.GBFromBytes(overheadAtStandardRate) * standardRate.StoragePerGBMonth
		}
	}

	return cost
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

// applyOptimization applies a tier optimization by calling S3 CopyObject with
// the target storage class.  The object is copied in-place (same bucket and
// key) so only its storage class changes; no data is moved.
func (co *CostOptimizer) applyOptimization(ctx context.Context, opt TierOptimization) error {
	client, err := co.backend.clientManager.GetPooledClient()
	if err != nil {
		return fmt.Errorf("tier transition %s→%s for %q: %w", opt.FromTier, opt.ToTier, opt.ObjectKey, err)
	}
	defer co.backend.clientManager.ReturnPooledClient(client)

	copySource := fmt.Sprintf("%s/%s", co.backend.bucket, opt.ObjectKey)
	input := &s3.CopyObjectInput{
		Bucket:            aws.String(co.backend.bucket),
		CopySource:        aws.String(copySource),
		Key:               aws.String(opt.ObjectKey),
		StorageClass:      s3types.StorageClass(opt.ToTier),
		MetadataDirective: s3types.MetadataDirectiveCopy,
	}

	// MetadataDirectiveCopy carries the metadata across but not the encryption: S3 encrypts a copy's
	// destination according to the request, whatever the source was stored under. So a tier transition
	// — which is a rewrite of the object, just without moving bytes — would drop an SSE-KMS object onto
	// the bucket default. That is the worst instance of the copy problem, because nobody asked for it:
	// the transition is automatic, so an object silently changes encryption on a timer.
	applyEncryptionCopy(input, co.backend.config.Encryption)

	_, err = client.CopyObject(ctx, input)
	if err != nil {
		return fmt.Errorf("CopyObject for tier transition %s→%s: %w", opt.FromTier, opt.ToTier, err)
	}

	// Update local access-pattern tracking to reflect the new tier.
	//
	// The cost is computed before taking the lock: calculateObjectCost reaches the pricing manager,
	// and ObjectSize is immutable once the pattern exists — RecordAccess only ever writes
	// AccessCount, LastAccessTime and AvgAccessGap — so reading it from the snapshot the caller
	// analyzed is equivalent to reading it from the map.
	cost := co.calculateObjectCost(opt.ObjectSize, opt.ToTier)

	co.mu.Lock()
	if pattern, exists := co.accessPatterns[opt.ObjectKey]; exists {
		pattern.CurrentTier = opt.ToTier
		pattern.EstimatedCost = cost
	}
	co.mu.Unlock()

	return nil
}

// GetOptimizationReport generates a cost optimization report
func (co *CostOptimizer) GetOptimizationReport() OptimizationReport {
	snapshot := co.snapshotPatterns()

	report := OptimizationReport{
		TotalObjects:          len(snapshot),
		OptimizationResults:   make([]TierOptimization, 0),
		TotalPotentialSavings: 0,
	}

	for i := range snapshot {
		if opt := co.analyzeObject(&snapshot[i]); opt != nil {
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
	// Below the IA classes' billable minimum, Standard is cheaper than a tier that would round the
	// object up to 128 KB. Named constant rather than a repeated literal: this file spelled 128*1024
	// twice while tiers.go spelled it once, and the threshold is the same fact in all three places.
	if objectSize < minBillableSize128KB {
		co.logger.Debug("Using Standard tier to avoid IA minimum charges",
			"object", objectKey,
			"size", objectSize,
			"threshold", minBillableSize128KB)
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
