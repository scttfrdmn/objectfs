package s3

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// costTally counts the billable AWS requests this process has made, and the bytes it has retrieved.
//
// # Why this is a middleware and not a counter beside RecordMetrics
//
// The eight [Backend] methods that call [MetricsCollector.RecordMetrics] are not one-to-one with
// billable AWS calls, and the gap is not small. PutObject is one RecordMetrics and, above
// MultipartThreshold, one CreateMultipartUpload plus one UploadPart per part plus one
// CompleteMultipartUpload — a 5 GB write is 641 requests reported as one. CopyObject above
// [maxSinglePartCopy] fans out into UploadPartCopy the same way. GetObject on the parallel read path
// issues one ranged GET per chunk. The capability probes in conditional.go make calls nothing counts
// at all, and the retryer can turn any one of these into several.
//
// So a hand-maintained count at the wrapper layer would be wrong on the paths that cost the most, and
// wrong in the direction that flatters: every fan-out under-reports. Counting where the SDK serializes
// a request means the number cannot disagree with what S3 received, because it *is* what S3 received.
//
// # What counts as billable
//
// One attempt that reached S3 and came back with a status below 500. Three separate decisions:
//
//   - Per attempt, not per API call. The middleware sits in the Deserialize step, which the retryer
//     re-enters on each try, so three attempts at one PutObject are three requests. That is what AWS
//     bills: each request S3 receives is a request, regardless of what the SDK was doing about it.
//   - A response is required. A connect failure or a DNS error never reached S3 and is not billed;
//     err with no RawResponse is exactly that case.
//   - Below 500 only. AWS does not bill requests that fail with its own server errors, and does bill
//     4xx — a GET for a key that does not exist is a charged Tier2 request. Both halves matter: 5xx
//     during an S3 event would otherwise inflate the figure at the moment an operator is looking at
//     it hardest, and dropping 4xx would hide the cost of a misconfigured mount retrying a
//     403 forever.
//
// # The request groups
//
// Four counts, because [RequestCosts] prices four things and AWS does not price them together on every
// class: writes (PUT, COPY, POST), lists, reads (GET, HEAD), and the free operations.
//
// Lists are separate from writes even though AWS's own price list calls both "Tier1", because on
// DEEP_ARCHIVE they are not the same number: a PUT is $0.05 per 1,000 and a LIST is $0.005 per 1,000,
// a factor of ten. Counting them together and pricing the total at the PUT rate would overstate every
// directory listing on the class where that error is largest.
//
// DELETE is free on every storage class and is counted only so that "requests this mount made" and
// "requests this mount paid for" are separately visible — a mount doing a great deal of work for no
// money is a fact worth being able to see.
//
// Classification is by operation-name prefix rather than by an enumerated list, and an unrecognized
// operation is counted as a write. Both choices err toward overstating: the write rate is the highest
// of the three on every class, so a new SDK operation this code has never heard of makes the reported
// cost too high rather than too low. That is the same direction [awsrates] documents for its
// volume-band and unknown-class fallbacks, and the reason is the same — a cost figure that is quietly
// low is one an operator acts on.
type costTally struct {
	writes    atomic.Int64
	lists     atomic.Int64
	reads     atomic.Int64
	free      atomic.Int64
	retrieved atomic.Int64
}

// costCounts is a consistent-enough snapshot of a tally for a caller that is going to price it.
//
// "Consistent enough" is the honest description: the fields are read one at a time, so a request in
// flight during the read can land in one and not another. For monotonic counters published as gauges
// on a thirty-second tick that is invisible, and the alternative — a lock on the path of every AWS
// request — buys exactness nobody can observe.
type costCounts struct {
	// Writes, Lists, and Reads are billable request counts by pricing group; Free is the requests AWS
	// charges nothing for. Retrieved is bytes off the wire, which is what a retrieval fee is charged on.
	Writes    int64
	Lists     int64
	Reads     int64
	Free      int64
	Retrieved int64
}

// snapshot reads the current counts.
func (t *costTally) snapshot() costCounts {
	return costCounts{
		Writes:    t.writes.Load(),
		Lists:     t.lists.Load(),
		Reads:     t.reads.Load(),
		Free:      t.free.Load(),
		Retrieved: t.retrieved.Load(),
	}
}

// install adds this tally's accounting middleware to an S3 client's option set.
//
// Called from [clientOptions], so every client this package builds is counted: the standard client,
// the accelerated client, and every client the connection pool's factory produces. That last one is
// the reason this goes through clientOptions rather than being applied at each construction site —
// HeadObject, DeleteObject, ListObjects and the health check all draw from the pool, and a factory
// that skipped the middleware would leave four operations uncounted in a way that looks like a mount
// doing less work than it is.
func (t *costTally) install(o *s3.Options) {
	o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
		return stack.Deserialize.Add(middleware.DeserializeMiddlewareFunc(
			"ObjectFSCostTally",
			func(
				ctx context.Context,
				in middleware.DeserializeInput,
				next middleware.DeserializeHandler,
			) (middleware.DeserializeOutput, middleware.Metadata, error) {
				out, md, err := next.HandleDeserialize(ctx, in)

				t.record(middleware.GetOperationName(ctx), out.RawResponse)

				return out, md, err
			},
		), middleware.After)
	})
}

// record counts one attempt, if it was billable.
//
// The error from the operation is deliberately not a parameter: whether AWS bills is decided by the
// status S3 returned, and err carries the SDK's opinion of it. A 404 on HeadObject is an error to the
// caller, an answer to the filesystem, and a charged Tier2 request to AWS — reading the status is the
// only way to get all three right.
func (t *costTally) record(operation string, rawResponse any) {
	response, ok := rawResponse.(*smithyhttp.Response)
	if !ok || response == nil {
		// Nothing reached S3, so nothing is billed. This is a connect failure, a DNS failure, or a
		// context cancellation before the request went out.
		return
	}

	// AWS does not bill for its own server errors. Counting them would spike the reported cost during
	// exactly the incident an operator is using this metric to investigate.
	if response.StatusCode >= 500 {
		return
	}

	switch requestGroupOf(operation) {
	case requestGroupFree:
		t.free.Add(1)

	case requestGroupList:
		t.lists.Add(1)

	case requestGroupRead:
		t.reads.Add(1)

		// Bytes out of the tier, which is what the retrieval fee is charged on — not the bytes the
		// filesystem ends up handing a reader. The two differ whenever transparent compression is on,
		// and it is the wire figure AWS bills. ContentLength is -1 for a chunked response and 0 for a
		// HEAD, so only a positive value is added.
		if response.ContentLength > 0 {
			t.retrieved.Add(response.ContentLength)
		}

	default:
		t.writes.Add(1)
	}
}

// requestGroup names an AWS request pricing group.
type requestGroup int

const (
	// requestGroupWrite is PUT, COPY, and POST — including every multipart operation that creates or
	// completes an upload.
	requestGroupWrite requestGroup = iota

	// requestGroupList is LIST, priced apart from the other writes because DEEP_ARCHIVE charges a tenth
	// as much for it.
	requestGroupList

	// requestGroupRead is GET and HEAD.
	requestGroupRead

	// requestGroupFree is the operations AWS charges nothing for: DELETE, and aborting a multipart
	// upload.
	requestGroupFree
)

// requestGroupOf classifies an SDK operation name into its AWS pricing group.
//
// By prefix, because the alternative is a list of every S3 operation the SDK can issue and this package
// can only be wrong about the ones it forgot. The prefixes cover what a filesystem does: Get/Head are
// reads, List is lists, Delete/Abort are free, and Put/Create/Complete/Upload/Copy/Post are writes.
// Anything unrecognized is a write — see the type comment for why the expensive group is the safe
// default.
//
// Restore is the one operation this would misprice: AWS bills a Glacier thaw in the Tier3 group, which
// is 15× the write rate on DEEP_ARCHIVE. Nothing in this package calls RestoreObject — a restore is a
// bucket operation ObjectFS does not perform — so the case is named here rather than handled, and
// whoever adds it will find this comment when they look for where their requests went.
func requestGroupOf(operation string) requestGroup {
	switch {
	case strings.HasPrefix(operation, "Delete"), strings.HasPrefix(operation, "Abort"):
		return requestGroupFree

	case strings.HasPrefix(operation, "List"):
		return requestGroupList

	case strings.HasPrefix(operation, "Get"), strings.HasPrefix(operation, "Head"):
		return requestGroupRead

	default:
		return requestGroupWrite
	}
}
