package cost

import (
	"math"

	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// OpType classifies an S3 API operation for billing purposes.
type OpType int

const (
	// OpGet covers GET, SELECT, and HEAD requests.
	OpGet OpType = iota
	// OpPut covers PUT, COPY, POST, and INITIATE-MULTIPART requests.
	OpPut
	// OpDelete covers DELETE requests (free on S3, included for completeness).
	OpDelete
	// OpList covers LIST requests.
	OpList
	// OpHead is an alias for OpGet in the pricing model (same per-request cost).
	OpHead
)

// opNames maps OpType to a human-readable label.
var opNames = map[OpType]string{
	OpGet:    "GET",
	OpPut:    "PUT",
	OpDelete: "DELETE",
	OpList:   "LIST",
	OpHead:   "HEAD",
}

// String returns the human-readable label for the operation type.
func (op OpType) String() string {
	if s, ok := opNames[op]; ok {
		return s
	}
	return "UNKNOWN"
}

// OpCost holds the cost breakdown for a single operation.
type OpCost struct {
	// RequestCost is the per-API-call fee.
	RequestCost float64

	// TransferCost is the data-transfer fee for the operation
	// (RetrievalPerGB × bytesTransferred for IA/Glacier reads).
	TransferCost float64

	// EgressCost is the internet egress fee (EgressPerGB × bytesTransferred).
	// Callers should set bytesEgress to 0 for intra-region traffic.
	EgressCost float64

	// TotalCost is RequestCost + TransferCost + EgressCost.
	TotalCost float64
}

// Calculator computes per-operation and per-storage S3 costs.
// It is safe for concurrent use after construction.
type Calculator struct {
	table *PriceTable
}

// NewCalculator creates a Calculator using the supplied PriceTable.
// Pass NewPriceTable(nil) to use default pricing.
func NewCalculator(table *PriceTable) *Calculator {
	if table == nil {
		table = NewPriceTable(nil)
	}
	return &Calculator{table: table}
}

// Calculate returns the cost breakdown for a single operation of type op on
// tier tier, transferring bytesTransferred bytes.
//
//   - bytesTransferred applies to the retrieval fee (reads from IA/Glacier).
//   - bytesEgress is the subset of bytesTransferred that leaves the AWS region;
//     set to 0 for same-region/same-account traffic.
func (c *Calculator) Calculate(op OpType, tier string, bytesTransferred, bytesEgress int64) OpCost {
	price, _ := c.table.Get(tier)

	var requestCost float64
	switch op {
	case OpPut:
		requestCost = price.PutRequest
	case OpList:
		requestCost = price.ListRequest
	case OpDelete:
		requestCost = 0 // DELETE is free on S3
	default: // OpGet, OpHead
		requestCost = price.GetRequest
	}

	gbTransferred := byteToGB(bytesTransferred)
	gbEgress := byteToGB(bytesEgress)

	transferCost := 0.0
	if op == OpGet || op == OpHead {
		transferCost = price.RetrievalPerGB * gbTransferred
	}

	egressCost := price.EgressPerGB * gbEgress

	total := requestCost + transferCost + egressCost
	return OpCost{
		RequestCost:  requestCost,
		TransferCost: transferCost,
		EgressCost:   egressCost,
		TotalCost:    total,
	}
}

// CalculateStorageCost returns the cost to store sizeBytes for durationMonths
// calendar months on tier.
func (c *Calculator) CalculateStorageCost(tier string, sizeBytes int64, durationMonths float64) float64 {
	price, _ := c.table.Get(tier)
	gb := byteToGB(sizeBytes)
	return price.StoragePerGBMonth * gb * math.Max(0, durationMonths)
}

// byteToGB converts bytes to the GB unit AWS bills in, which is decimal: 1 GB = 10^9 bytes.
//
// It used to divide by 2^30, with a comment stating the binary reading was correct. It is not — S3
// quotes GB-months in decimal GB — and every storage cost this package produced was 7.4% low as a
// result. The comment is why it survived: it made the wrong unit look considered rather than
// mistaken, so a reader checking the code found a deliberate-looking choice and moved on.
//
// The conversion now lives in [awsrates.GBFromBytes], next to the rates it has to agree with, so
// there is one place where the unit is decided. This wrapper stays because three call sites read
// better with the short name.
func byteToGB(bytes int64) float64 {
	return awsrates.GBFromBytes(bytes)
}
