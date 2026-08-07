package cost

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func newReporter(t *testing.T) *Reporter {
	t.Helper()
	return NewReporter(NewCalculator(NewPriceTable(nil)))
}

func TestReporter_NilCalc_UsesDefault(t *testing.T) {
	t.Parallel()
	r := NewReporter(nil)
	assert.NotNil(t, r)
}

func TestReporter_RecordOp_AccumulatesCost(t *testing.T) {
	t.Parallel()
	r := newReporter(t)
	c1 := r.RecordOp("alice", OpGet, TierStandard, 0, 0)
	c2 := r.RecordOp("alice", OpGet, TierStandard, 0, 0)

	// Exact equality on purpose, and testifylint's float-compare is suppressed rather than satisfied.
	// The claim is that two identical operations price bit-identically — determinism, not
	// approximation — and InEpsilon would let a pricing path that returned a slightly different number
	// for the same inputs pass. The InDelta below is the case where a tolerance *is* right: it compares
	// a sum against a doubled value, so it carries one floating-point addition's worth of error.
	//nolint:testifylint // exact: identical inputs must price identically, not merely closely
	assert.Equal(t, c1.TotalCost, c2.TotalCost)

	rep := r.Report("alice", 0)
	assert.InDelta(t, c1.TotalCost*2, rep.TotalCost, 1e-10)
}

func TestReporter_RecordOp_IsolateTenants(t *testing.T) {
	t.Parallel()
	r := newReporter(t)
	r.RecordOp("alice", OpPut, TierStandard, 0, 0)
	r.RecordOp("bob", OpGet, TierStandard, 0, 0)

	repAlice := r.Report("alice", 0)
	repBob := r.Report("bob", 0)
	assert.Greater(t, repAlice.TotalCost, 0.0)
	assert.Greater(t, repBob.TotalCost, 0.0)
	// PUT is more expensive than GET; Alice should cost more than Bob.
	assert.Greater(t, repAlice.TotalCost, repBob.TotalCost)
}

func TestReporter_RecordStorage_AddsStorageCost(t *testing.T) {
	t.Parallel()
	r := newReporter(t)
	oneBilledGB := int64(1_000_000_000) // AWS bills a GB as 10^9 bytes, not 2^30
	charge := r.RecordStorage("alice", TierStandard, oneBilledGB, 1.0)
	assert.InDelta(t, DefaultPrices[TierStandard].StoragePerGBMonth, charge, 1e-6)

	rep := r.Report("alice", 0)
	assert.InDelta(t, charge, rep.StorageCost, 1e-10)
	assert.InDelta(t, charge, rep.TotalCost, 1e-10)
}

func TestReporter_Report_UnknownTenant_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	r := newReporter(t)
	rep := r.Report("nobody", 0)
	assert.Equal(t, "nobody", rep.TenantID)
	assert.Zero(t, rep.TotalCost)
	assert.NotNil(t, rep.OpBreakdown)
	assert.NotNil(t, rep.OpCounts)
}

func TestReporter_Report_OpBreakdown(t *testing.T) {
	t.Parallel()
	r := newReporter(t)
	r.RecordOp("alice", OpGet, TierStandard, 0, 0)
	r.RecordOp("alice", OpGet, TierStandard, 0, 0)
	r.RecordOp("alice", OpPut, TierStandard, 0, 0)

	rep := r.Report("alice", 0)
	assert.Equal(t, int64(2), rep.OpCounts["GET"])
	assert.Equal(t, int64(1), rep.OpCounts["PUT"])
	assert.Greater(t, rep.OpBreakdown["GET"], 0.0)
	assert.Greater(t, rep.OpBreakdown["PUT"], 0.0)
}

func TestReporter_ReportAll(t *testing.T) {
	t.Parallel()
	r := newReporter(t)
	r.RecordOp("a", OpGet, TierStandard, 0, 0)
	r.RecordOp("b", OpGet, TierStandard, 0, 0)
	r.RecordOp("c", OpGet, TierStandard, 0, 0)

	reports := r.ReportAll(0)
	assert.Len(t, reports, 3)
	seen := make(map[string]bool)
	for _, rep := range reports {
		seen[rep.TenantID] = true
	}
	assert.True(t, seen["a"])
	assert.True(t, seen["b"])
	assert.True(t, seen["c"])
}

func TestReporter_Reset_ClearsData(t *testing.T) {
	t.Parallel()
	r := newReporter(t)
	r.RecordOp("alice", OpGet, TierStandard, 0, 0)
	r.Reset()
	rep := r.Report("alice", 0)
	assert.Zero(t, rep.TotalCost)
}

func TestReporter_ROI_SavingsPositive(t *testing.T) {
	t.Parallel()
	r := newReporter(t)
	oneBilledGB := int64(1_000_000_000)
	// Record storage at DEEP_ARCHIVE rate.
	r.RecordStorage("alice", TierDeepArchive, oneBilledGB, 1.0)

	// Pass baseline as Standard rate — Alice is saving vs Standard pricing.
	rep := r.Report("alice", DefaultPrices[TierStandard].StoragePerGBMonth)
	assert.Greater(t, rep.BaselineCost, rep.TotalCost)
	assert.Greater(t, rep.Savings, 0.0)
}

func TestReporter_OperationCost_ExcludesStorage(t *testing.T) {
	t.Parallel()
	r := newReporter(t)
	oneBilledGB := int64(1_000_000_000)
	opCost := r.RecordOp("alice", OpGet, TierStandard, 0, 0)
	r.RecordStorage("alice", TierStandard, oneBilledGB, 1.0)

	rep := r.Report("alice", 0)
	assert.InDelta(t, opCost.TotalCost, rep.OperationCost, 1e-10)
}
