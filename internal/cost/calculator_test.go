package cost

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func newCalc(t *testing.T) *Calculator {
	t.Helper()
	return NewCalculator(NewPriceTable(nil))
}

// TestStorageCostUsesDecimalGB pins the billing unit against a hand-computed figure.
//
// This calculator divided by 2^30 for a GB, with a comment asserting the binary reading was correct,
// and every storage cost it produced was 7.4% low. The tests did not catch it because they passed
// 1024*1024*1024 bytes, called it "one GB", and asserted the rate came back — an expectation that
// agrees with whichever unit the code chose. A test that feeds 2^30 and expects the per-GB rate
// passes under both the right answer and the wrong one.
//
// So the expectations here are literal dollar figures worked out by hand, and the second case is the
// one that matters: a GiB of data is billed as 1.0737 GB, and the cost must reflect that.
func TestStorageCostUsesDecimalGB(t *testing.T) {
	t.Parallel()

	c := newCalc(t)

	cases := []struct {
		name  string
		bytes int64
		want  float64
		why   string
	}{
		{
			name:  "one billed GB costs exactly the published rate",
			bytes: 1_000_000_000,
			want:  0.023,
			why:   "$0.023/GB-month for Standard in us-east-1",
		},
		{
			name:  "one GiB costs more than one GB, because it is more data",
			bytes: 1_073_741_824,
			want:  0.023 * 1.073741824, // 0.0246960...
			why:   "the binary reading returns 0.023 here, which is the 7.4% understatement",
		},
		{
			name:  "a 500 MB object is half the rate",
			bytes: 500_000_000,
			want:  0.0115,
			why:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := c.CalculateStorageCost(TierStandard, tc.bytes, 1.0)

			msg := tc.why
			if msg != "" {
				msg = " — " + msg
			}

			assert.InDelta(t, tc.want, got, 1e-9,
				"storing %d bytes for a month should cost $%v, got $%v%s",
				tc.bytes, tc.want, got, msg)
		})
	}
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
	oneBilledGB := int64(1_000_000_000) // AWS bills a GB as 10^9 bytes, not 2^30
	cost := c.Calculate(OpGet, TierGlacierIR, oneBilledGB, 0)
	expectedRetrieval := DefaultPrices[TierGlacierIR].RetrievalPerGB * 1.0
	assert.InDelta(t, expectedRetrieval, cost.TransferCost, 1e-6)
}

func TestCalculator_EgressFee(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	oneBilledGB := int64(1_000_000_000)
	cost := c.Calculate(OpGet, TierStandard, oneBilledGB, oneBilledGB) // all bytes egress
	expectedEgress := DefaultPrices[TierStandard].EgressPerGB * 1.0
	assert.InDelta(t, expectedEgress, cost.EgressCost, 1e-6)
}

func TestCalculator_TotalCost_Sums(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	oneBilledGB := int64(1_000_000_000)
	cost := c.Calculate(OpGet, TierGlacierIR, oneBilledGB, oneBilledGB)
	expected := cost.RequestCost + cost.TransferCost + cost.EgressCost
	assert.InDelta(t, expected, cost.TotalCost, 1e-10)
}

func TestCalculator_StorageCost_OneGBOneMonth(t *testing.T) {
	t.Parallel()
	c := newCalc(t)
	oneBilledGB := int64(1_000_000_000)
	cost := c.CalculateStorageCost(TierStandard, oneBilledGB, 1.0)
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
