package cost

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func newCalc(t *testing.T) *Calculator {
	t.Helper()
	return NewCalculator(NewPriceTable(nil))
}

func TestCalculator_NilTable_UsesDefaults(t *testing.T) {
	t.Parallel()
	c := NewCalculator(nil)
	assert.NotNil(t, c)
}

func TestCalculator_GetRequest_Standard(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	cost := c.Calculate(OpGet, TierStandard, 0, 0)
	assert.InDelta(t, DefaultPrices[TierStandard].GetRequest, cost.RequestCost, 1e-10)
	assert.Equal(t, 0.0, cost.TransferCost)
	assert.Equal(t, 0.0, cost.EgressCost)
	assert.InDelta(t, cost.RequestCost, cost.TotalCost, 1e-10)
}

func TestCalculator_PutRequest_Standard(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	cost := c.Calculate(OpPut, TierStandard, 1024*1024, 0) // 1 MiB put
	assert.InDelta(t, DefaultPrices[TierStandard].PutRequest, cost.RequestCost, 1e-10)
	assert.Equal(t, 0.0, cost.TransferCost) // PUT has no retrieval fee
	assert.Equal(t, 0.0, cost.EgressCost)
}

func TestCalculator_ListRequest(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	cost := c.Calculate(OpList, TierStandard, 0, 0)
	assert.InDelta(t, DefaultPrices[TierStandard].ListRequest, cost.RequestCost, 1e-10)
}

func TestCalculator_DeleteRequest_Free(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	cost := c.Calculate(OpDelete, TierStandard, 0, 0)
	assert.Equal(t, 0.0, cost.RequestCost)
	assert.Equal(t, 0.0, cost.TotalCost)
}

func TestCalculator_HeadRequest(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	cost := c.Calculate(OpHead, TierStandard, 0, 0)
	assert.InDelta(t, DefaultPrices[TierStandard].GetRequest, cost.RequestCost, 1e-10)
}

func TestCalculator_GlacierIR_RetrievalFee(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	oneGB := int64(1024 * 1024 * 1024)
	cost := c.Calculate(OpGet, TierGlacierIR, oneGB, 0)
	expectedRetrieval := DefaultPrices[TierGlacierIR].RetrievalPerGB * 1.0
	assert.InDelta(t, expectedRetrieval, cost.TransferCost, 1e-6)
}

func TestCalculator_EgressFee(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	oneGB := int64(1024 * 1024 * 1024)
	cost := c.Calculate(OpGet, TierStandard, oneGB, oneGB) // all bytes egress
	expectedEgress := DefaultPrices[TierStandard].EgressPerGB * 1.0
	assert.InDelta(t, expectedEgress, cost.EgressCost, 1e-6)
}

func TestCalculator_TotalCost_Sums(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	oneGB := int64(1024 * 1024 * 1024)
	cost := c.Calculate(OpGet, TierGlacierIR, oneGB, oneGB)
	expected := cost.RequestCost + cost.TransferCost + cost.EgressCost
	assert.InDelta(t, expected, cost.TotalCost, 1e-10)
}

func TestCalculator_StorageCost_OneGBOneMonth(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	oneGB := int64(1024 * 1024 * 1024)
	cost := c.CalculateStorageCost(TierStandard, oneGB, 1.0)
	assert.InDelta(t, DefaultPrices[TierStandard].StoragePerGBMonth, cost, 1e-6)
}

func TestCalculator_StorageCost_ZeroDuration(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	cost := c.CalculateStorageCost(TierStandard, 1024*1024*1024, 0)
	assert.Equal(t, 0.0, cost)
}

func TestCalculator_StorageCost_NegativeDuration(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	cost := c.CalculateStorageCost(TierStandard, 1024*1024*1024, -1)
	assert.Equal(t, 0.0, cost)
}

func TestOpType_String(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "GET", OpGet.String())
	assert.Equal(t, "PUT", OpPut.String())
	assert.Equal(t, "DELETE", OpDelete.String())
	assert.Equal(t, "LIST", OpList.String())
	assert.Equal(t, "HEAD", OpHead.String())
}
