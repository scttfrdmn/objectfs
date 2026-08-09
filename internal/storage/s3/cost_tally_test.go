package s3

import (
	"net/http"
	"testing"

	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// TestTheTallyCountsEachOperationInItsPricingGroup pins the classification, which is where a mispriced
// group would come from.
//
// LIST separate from PUT is the case worth having a test for: AWS's price list calls both "Tier1", and
// on DEEP_ARCHIVE a PUT is $0.05 per 1,000 while a LIST is $0.005 — so a version of this that folded
// lists in with writes would report every directory listing at ten times its price.
func TestTheTallyCountsEachOperationInItsPricingGroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		operation string
		want      requestGroup
		why       string
	}{
		{"PutObject", requestGroupWrite, "the plain write"},
		{"CreateMultipartUpload", requestGroupWrite, "billed as a write, and one per large PutObject"},
		{"UploadPart", requestGroupWrite, "one per part — the reason a wrapper-level count understates"},
		{"CompleteMultipartUpload", requestGroupWrite, "a POST, billed as a write"},
		{"UploadPartCopy", requestGroupWrite, "the multipart copy path"},
		{"CopyObject", requestGroupWrite, "COPY is in the write group, not the read group"},
		{"PutObjectTagging", requestGroupWrite, "a PUT of something other than object data"},

		{"ListObjectsV2", requestGroupList, "priced a tenth of a PUT on DEEP_ARCHIVE"},
		{"ListMultipartUploads", requestGroupList, "the orphaned-upload sweep"},

		{"GetObject", requestGroupRead, "the plain read"},
		{"HeadObject", requestGroupRead, "HEAD bills in the same group as GET"},
		{"HeadBucket", requestGroupRead, "the health check, which is billed like any other HEAD"},

		{"DeleteObject", requestGroupFree, "DELETE is free on every storage class"},
		{"DeleteObjects", requestGroupFree, "the batch delete, also free"},
		{"AbortMultipartUpload", requestGroupFree, "aborting costs nothing; the abandoned parts cost storage"},

		{
			"SomeOperationTheSDKAddedLater",
			requestGroupWrite,
			"an operation this code has never heard of is priced as the most expensive group, so a " +
				"missing case overstates the bill rather than hiding part of it",
		},
	}

	for _, tc := range tests {
		t.Run(tc.operation, func(t *testing.T) {
			t.Parallel()

			if got := requestGroupOf(tc.operation); got != tc.want {
				t.Errorf("requestGroupOf(%q) = %v, want %v: %s", tc.operation, got, tc.want, tc.why)
			}
		})
	}
}

// TestTheTallyCountsWhatAWSBills covers the three decisions in [costTally.record] about what a billable
// request is. Each case is a status or a response shape AWS treats differently from the others.
func TestTheTallyCountsWhatAWSBills(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation string
		response  *smithyhttp.Response
		want      costCounts
		why       string
	}{
		{
			name:      "a served read counts and its bytes count",
			operation: "GetObject",
			response:  responseWith(http.StatusOK, 4096),
			want:      costCounts{Reads: 1, Retrieved: 4096},
			why:       "the ordinary case",
		},
		{
			name:      "a 404 counts",
			operation: "HeadObject",
			response:  responseWith(http.StatusNotFound, 0),
			want:      costCounts{Reads: 1},
			why: "AWS bills a request for a key that does not exist. A mount doing lookups for absent " +
				"paths — which is every shell tab-completion — is spending money, and not counting it " +
				"hides the cost of the pattern most likely to be pathological",
		},
		{
			name:      "a 403 counts",
			operation: "PutObject",
			response:  responseWith(http.StatusForbidden, 0),
			want:      costCounts{Writes: 1},
			why:       "a misconfigured mount retrying a denial is billed for every attempt",
		},
		{
			name:      "a 500 does not count",
			operation: "PutObject",
			response:  responseWith(http.StatusInternalServerError, 0),
			want:      costCounts{},
			why: "AWS does not bill its own server errors. Counting them would inflate the reported " +
				"cost during exactly the incident an operator is using this metric to investigate",
		},
		{
			name:      "a 503 does not count",
			operation: "GetObject",
			response:  responseWith(http.StatusServiceUnavailable, 0),
			want:      costCounts{},
			why:       "SlowDown is throttling, not a request S3 accepted",
		},
		{
			name:      "a request that never reached S3 does not count",
			operation: "PutObject",
			response:  nil,
			want:      costCounts{},
			why:       "a connect or DNS failure has no response, and AWS bills nothing for it",
		},
		{
			name:      "a chunked read counts the request but no bytes",
			operation: "GetObject",
			response:  responseWith(http.StatusOK, -1),
			want:      costCounts{Reads: 1},
			why: "ContentLength is -1 when the length is unknown. Adding it would decrement the byte " +
				"total, making a monotonic counter go backwards",
		},
		{
			name:      "a free operation counts as free",
			operation: "DeleteObject",
			response:  responseWith(http.StatusNoContent, 0),
			want:      costCounts{Free: 1},
			why:       "counted, but never priced",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tally := &costTally{}

			// A nil *smithyhttp.Response has to be passed as a typed nil through an `any`, which is the
			// shape the middleware actually sees when the operation failed before a response existed.
			if tc.response == nil {
				tally.record(tc.operation, (*smithyhttp.Response)(nil))
			} else {
				tally.record(tc.operation, tc.response)
			}

			if got := tally.snapshot(); got != tc.want {
				t.Errorf("after %s returning %s: counts = %+v, want %+v\n%s",
					tc.operation, describeResponse(tc.response), got, tc.want, tc.why)
			}
		})
	}
}

// TestTheTallyIgnoresARawResponseThatIsNotHTTP guards the type assertion.
//
// smithy's RawResponse is an `any` because a protocol other than HTTP could put something else there.
// Nothing in this package uses one, so the branch is unreachable in production — but an assertion
// without the two-value form would panic instead of skipping, and a panic on the response path of every
// S3 call is not a failure mode worth leaving to chance.
func TestTheTallyIgnoresARawResponseThatIsNotHTTP(t *testing.T) {
	t.Parallel()

	tally := &costTally{}
	tally.record("GetObject", "not a response")
	tally.record("PutObject", nil)

	if got := tally.snapshot(); got != (costCounts{}) {
		t.Errorf("counts = %+v, want all zero: a non-HTTP raw response is not a billable request", got)
	}
}

func responseWith(status int, contentLength int64) *smithyhttp.Response {
	return &smithyhttp.Response{
		Response: &http.Response{
			StatusCode:    status,
			ContentLength: contentLength,
		},
	}
}

func describeResponse(r *smithyhttp.Response) string {
	if r == nil {
		return "no response"
	}

	return http.StatusText(r.StatusCode)
}
