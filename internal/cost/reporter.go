package cost

import (
	"math"
	"sync"
	"time"
)

// TenantRecord accumulates cost data for a single tenant.
type TenantRecord struct {
	TenantID string

	// OpCounts counts the number of each operation type recorded.
	OpCounts map[OpType]int64

	// OpCosts accumulates the total spend per operation type.
	OpCosts map[OpType]float64

	// TotalCost is the sum of all operation costs.
	TotalCost float64

	// StorageCost is the accumulated periodic storage charge.
	StorageCost float64

	// StorageGBMonths is the total GB-months of storage recorded, used for ROI.
	StorageGBMonths float64

	// FirstSeen is the time of the first RecordOp call for this tenant.
	FirstSeen time.Time

	// LastSeen is the time of the most recent RecordOp call.
	LastSeen time.Time
}

// CostReport is an immutable snapshot of costs for one or more tenants.
type CostReport struct {
	// TenantID is empty for aggregate reports.
	TenantID string

	// Period is the time range covered by the report.
	PeriodStart, PeriodEnd time.Time

	// TotalCost is the aggregate cost across all operations and storage.
	TotalCost float64

	// StorageCost is the storage portion of TotalCost.
	StorageCost float64

	// OperationCost is TotalCost - StorageCost.
	OperationCost float64

	// BaselineCost is the equivalent cost if all objects stayed on STANDARD tier.
	// Used for ROI calculation.
	BaselineCost float64

	// Savings is BaselineCost - TotalCost (positive means tiering saved money).
	Savings float64

	// OpBreakdown maps OpType.String() → total cost for that operation class.
	OpBreakdown map[string]float64

	// OpCounts maps OpType.String() → count of operations.
	OpCounts map[string]int64
}

// Reporter accumulates per-operation costs by tenant and emits CostReports.
// It is safe for concurrent use.
type Reporter struct {
	mu         sync.RWMutex
	calc       *Calculator
	tenants    map[string]*TenantRecord
	reportedAt time.Time
}

// NewReporter creates a Reporter backed by the supplied Calculator.
// If calc is nil, a default-priced Calculator is used.
func NewReporter(calc *Calculator) *Reporter {
	if calc == nil {
		calc = NewCalculator(nil)
	}
	return &Reporter{
		calc:       calc,
		tenants:    make(map[string]*TenantRecord),
		reportedAt: time.Now(),
	}
}

// RecordOp records a single S3 operation for tenantID and returns its cost breakdown.
// bytesTransferred and bytesEgress follow the same semantics as Calculator.Calculate.
func (r *Reporter) RecordOp(tenantID string, op OpType, tier string, bytesTransferred, bytesEgress int64) OpCost {
	cost := r.calc.Calculate(op, tier, bytesTransferred, bytesEgress)

	now := time.Now()
	r.mu.Lock()
	rec := r.tenant(tenantID, now)
	rec.OpCounts[op]++
	rec.OpCosts[op] += cost.TotalCost
	rec.TotalCost += cost.TotalCost
	rec.LastSeen = now
	r.mu.Unlock()

	return cost
}

// RecordStorage records a periodic storage charge for tenantID.
// sizeBytes is the object size; durationMonths is the billing period.
func (r *Reporter) RecordStorage(tenantID, tier string, sizeBytes int64, durationMonths float64) float64 {
	charge := r.calc.CalculateStorageCost(tier, sizeBytes, durationMonths)

	now := time.Now()
	gbMonths := byteToGB(sizeBytes) * math.Max(0, durationMonths)

	r.mu.Lock()
	rec := r.tenant(tenantID, now)
	rec.StorageCost += charge
	rec.StorageGBMonths += gbMonths
	rec.TotalCost += charge
	rec.LastSeen = now
	r.mu.Unlock()

	return charge
}

// Report returns a CostReport snapshot for tenantID.
// If the tenant has never been seen, an empty report is returned.
//
// baselineStoragePerGB is the rate the ROI Savings field is measured against — what the same bytes
// would have cost on the tier the caller is comparing to. Pass [StandardBaselinePerGB] for the usual
// "versus Standard" comparison; the doc here used to say "pass 0.023", which was a sixth place in this
// repo where the Standard rate was written down and one more thing to update when AWS moves it.
func (r *Reporter) Report(tenantID string, baselineStoragePerGB float64) CostReport {
	r.mu.RLock()
	rec, ok := r.tenants[tenantID]
	if !ok {
		r.mu.RUnlock()
		return CostReport{TenantID: tenantID, OpBreakdown: map[string]float64{}, OpCounts: map[string]int64{}}
	}
	// snapshot
	snap := *rec
	start := r.reportedAt
	r.mu.RUnlock()

	return buildReport(tenantID, snap, start, time.Now(), baselineStoragePerGB)
}

// StandardBaselinePerGB is the STANDARD storage rate, for callers computing ROI against it.
//
// It is a function of [DefaultPrices] rather than a literal so that "savings versus Standard" is
// measured against the same rate everything else charges at. A hardcoded baseline drifting from the
// live rate would misstate every savings figure while every cost figure stayed right, which is the
// hardest kind of discrepancy to notice: both numbers look plausible and only their difference is
// wrong.
var StandardBaselinePerGB = DefaultPrices[TierStandard].StoragePerGBMonth

// ReportAll returns CostReports for every tracked tenant.
//
// baselineStoragePerGB has the same meaning as in [Reporter.Report].
func (r *Reporter) ReportAll(baselineStoragePerGB float64) []CostReport {
	r.mu.RLock()
	ids := make([]string, 0, len(r.tenants))
	snaps := make([]TenantRecord, 0, len(r.tenants))
	for id, rec := range r.tenants {
		ids = append(ids, id)
		snaps = append(snaps, *rec)
	}
	start := r.reportedAt
	r.mu.RUnlock()

	now := time.Now()
	reports := make([]CostReport, len(ids))
	for i, id := range ids {
		reports[i] = buildReport(id, snaps[i], start, now, baselineStoragePerGB)
	}
	return reports
}

// Reset clears all accumulated data and resets the report period start time.
func (r *Reporter) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tenants = make(map[string]*TenantRecord)
	r.reportedAt = time.Now()
}

// tenant returns (or creates) the TenantRecord for id.
// Caller must hold r.mu write lock.
func (r *Reporter) tenant(id string, now time.Time) *TenantRecord {
	rec, ok := r.tenants[id]
	if !ok {
		rec = &TenantRecord{
			TenantID:  id,
			OpCounts:  make(map[OpType]int64),
			OpCosts:   make(map[OpType]float64),
			FirstSeen: now,
			LastSeen:  now,
		}
		r.tenants[id] = rec
	}
	return rec
}

// buildReport converts a TenantRecord snapshot to a CostReport.
func buildReport(tenantID string, rec TenantRecord, start, end time.Time, baselineStoragePerGB float64) CostReport {
	opBreakdown := make(map[string]float64, len(rec.OpCosts))
	opCounts := make(map[string]int64, len(rec.OpCounts))
	for op, c := range rec.OpCosts {
		opBreakdown[op.String()] = c
	}
	for op, n := range rec.OpCounts {
		opCounts[op.String()] = n
	}

	opCost := rec.TotalCost - rec.StorageCost

	// ROI: baseline assumes all stored GB-months were charged at baselineStoragePerGB.
	// This lets callers compute savings vs. keeping everything on Standard tier.
	var baselineCost, savings float64
	if baselineStoragePerGB > 0 && rec.StorageGBMonths > 0 {
		baselineCost = (baselineStoragePerGB * rec.StorageGBMonths) + opCost
		savings = baselineCost - rec.TotalCost
	} else {
		baselineCost = rec.TotalCost
		savings = 0
	}

	return CostReport{
		TenantID:      tenantID,
		PeriodStart:   start,
		PeriodEnd:     end,
		TotalCost:     rec.TotalCost,
		StorageCost:   rec.StorageCost,
		OperationCost: opCost,
		BaselineCost:  baselineCost,
		Savings:       savings,
		OpBreakdown:   opBreakdown,
		OpCounts:      opCounts,
	}
}
