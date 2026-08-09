package s3_test

// #226's end-to-end half: the request count a mount publishes is the count of requests it actually
// made, and the dollar figures derived from it use the rates the tier charges.
//
// The oracle is the harness's recording proxy, not the tally. That distinction is the point of the file:
// a test that drove the backend and then compared CostStats against a number the same code path
// produced would agree with itself no matter what either side did. testaws counts HTTP requests in a
// reverse proxy the backend does not know exists, so the two counts have independent provenance — which
// is the whole reason to count at the SDK layer rather than at the wrapper layer, and therefore the
// property this file has to be able to check.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/awsrates"
	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// TestTheRequestCountMatchesTheRequestsTheProxySaw is the count, checked against an independent witness.
func TestTheRequestCountMatchesTheRequestsTheProxySaw(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	// Both sides are read as deltas across the workload below, and neither can be zeroed instead.
	//
	// The proxy has already seen requests the backend did not make (testaws.Start creates the bucket
	// through its own raw client) and the backend has already made one the proxy would not attribute to
	// the workload (NewBackend's health check is a HeadBucket, which AWS bills like any other HEAD — so
	// counting it is correct, and expecting a virgin backend to report zero requests is not). Resetting
	// the proxy fixes the first and makes the second look like an overcount; the tally has no reset at
	// all, by design, since a monotonic counter is what a rate query needs.
	ts.ResetRequests()
	before := backend.CostStats()

	if err := backend.PutObject(ctx, "ledger/one.bin", []byte("first"), nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if err := backend.PutObject(ctx, "ledger/two.bin", []byte("second"), nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if _, err := backend.GetObject(ctx, "ledger/one.bin", 0, 0); err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if _, err := backend.ListObjects(ctx, "ledger/", 0); err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if err := backend.DeleteObject(ctx, "ledger/two.bin"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	counted := countedSince(before, backend.CostStats())
	observed := groupObservedRequests(ts.Requests())

	if counted.total != observed.total {
		t.Errorf("CostStats counted %d requests, the proxy saw %d\ncounted: %s\nobserved: %s\n"+
			"A gap in either direction means the middleware is not on every client the backend uses — "+
			"the pooled clients serve HeadObject, DeleteObject and ListObjects, so a factory that "+
			"skipped clientOptions would undercount exactly those.",
			counted.total, observed.total, counted, observed)
	}

	// Per group, since a total can be right while the classification is wrong — and the classification is
	// what decides which rate each request is priced at.
	if counted.writes != observed.writes {
		t.Errorf("writes: counted %d, proxy saw %d PUT/POST requests (%s)",
			counted.writes, observed.writes, observed)
	}
	if counted.lists != observed.lists {
		t.Errorf("lists: counted %d, proxy saw %d list requests (%s)",
			counted.lists, observed.lists, observed)
	}
	if counted.reads != observed.reads {
		t.Errorf("reads: counted %d, proxy saw %d GET/HEAD requests (%s)",
			counted.reads, observed.reads, observed)
	}
	if counted.frees != observed.frees {
		t.Errorf("frees: counted %d, proxy saw %d DELETE requests (%s)",
			counted.frees, observed.frees, observed)
	}
}

// TestAMultipartWriteIsCountedPerPart is the case a wrapper-level count gets wrong.
//
// One PutObject above the multipart threshold is one call into the backend and many billable requests at
// AWS: a create, a part upload each, and a complete. The count has to follow the requests, because the
// requests are what the bill follows. This is the defect the middleware exists to prevent, so it needs a
// test that fails if the count is ever moved back up to the wrapper.
func TestAMultipartWriteIsCountedPerPart(t *testing.T) {
	t.Parallel()

	const (
		partSize = 5 * 1024 * 1024
		parts    = 3
	)

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.MultipartThreshold = partSize
		cfg.MultipartChunkSize = partSize
		// Compression would shrink this highly compressible payload below the threshold and take the
		// single-part path instead, testing nothing.
		cfg.Compression.Enabled = false
	})
	ctx := context.Background()

	ts.ResetRequests()
	before := backend.CostStats()

	if err := backend.PutObject(ctx, "large.bin", make([]byte, partSize*parts), nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	counted := countedSince(before, backend.CostStats())
	observed := groupObservedRequests(ts.Requests())

	if counted.writes != observed.writes {
		t.Fatalf("one PutObject of %d parts: counted %d writes, the proxy saw %d (%s)",
			parts, counted.writes, observed.writes, observed)
	}

	// The number itself, not just the agreement: if both sides collapsed to 1 the comparison above would
	// still pass, and 1 is precisely the wrong answer a wrapper-level count gives here.
	if counted.writes <= parts {
		t.Errorf("counted %d write requests for a %d-part upload, want more than %d\n"+
			"A multipart upload is a create, a part upload each, and a complete. A count at or below "+
			"the part count means the fan-out is being reported as a single request, which understates "+
			"the cost of the operation that costs the most.",
			counted.writes, parts, parts)
	}
}

// TestTheCostFiguresUseTheTiersOwnRates checks the arithmetic against the published rates.
//
// The rates are stated here as AWS prints them — dollars per 1,000 requests, dollars per GB — and
// divided down in the test rather than copied in per-request form. That is the same construction
// pricing_manager_test.go uses, for the same reason: #209 was a per-1,000 figure stored as if it were
// per-request, and a test holding the already-divided number would have agreed with the defect.
func TestTheCostFiguresUseTheTiersOwnRates(t *testing.T) {
	t.Parallel()

	const (
		tier = "STANDARD_IA"

		// STANDARD_IA in us-east-1, as AWS's price list publishes them.
		putPer1000     = 0.01
		getPer1000     = 0.001
		retrievalPerGB = 0.01
		storagePerGB   = 0.0125

		body = "twenty-nine bytes of payload."
	)

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.StorageTier = tier
		// The diversion would put an object this small on STANDARD, which is the correct behavior and
		// the wrong tier for checking STANDARD_IA's rates.
		cfg.CostOptimization.SmallObjectsOnStandard = false
		cfg.Compression.Enabled = false
	})
	ctx := context.Background()

	ts.ResetRequests()

	if err := backend.PutObject(ctx, "priced.bin", []byte(body), nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if _, err := backend.GetObject(ctx, "priced.bin", 0, 0); err != nil {
		t.Fatalf("GetObject: %v", err)
	}

	stats := backend.CostStats()

	if stats.Tier != tier {
		t.Fatalf("Tier = %q, want %q: the rates below are only right for that tier", stats.Tier, tier)
	}
	if stats.Region != testaws.DefaultRegion {
		t.Fatalf("Region = %q, want %q: a cost figure has to name the region it was priced in",
			stats.Region, testaws.DefaultRegion)
	}

	assertRate(t, "per write request", stats.RatePerWriteRequest, putPer1000/1000)
	assertRate(t, "per read request", stats.RatePerReadRequest, getPer1000/1000)
	assertRate(t, "per GB retrieved", stats.RatePerGBRetrieved, retrievalPerGB)
	assertRate(t, "per GB month", stats.RatePerGBMonth, storagePerGB)

	// RequestCost is the counts times the rates, and the counts are whatever the SDK made — asserting a
	// literal dollar figure would pin the request count too, and that is the other test's job.
	wantRequestCost := float64(stats.WriteRequests)*(putPer1000/1000) +
		float64(stats.ListRequests)*stats.RatePerListRequest +
		float64(stats.ReadRequests)*(getPer1000/1000)
	assertRate(t, "request cost", stats.RequestCost, wantRequestCost)

	// The retrieval fee is charged on bytes off the wire, converted at 10^9 bytes to the GB.
	assertRate(t, "retrieval cost", stats.RetrievalCost,
		awsrates.GBFromBytes(stats.BytesRetrieved)*retrievalPerGB)

	if stats.StoredBytes != int64(len(body)) {
		t.Errorf("StoredBytes = %d, want %d (the object written)", stats.StoredBytes, len(body))
	}
	assertRate(t, "storage cost per month", stats.StorageCostPerMonth,
		awsrates.GBFromBytes(int64(len(body)))*storagePerGB)
}

// TestStandardHasNoRetrievalFee is the zero that has to be a real answer rather than a missing one.
func TestStandardHasNoRetrievalFee(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.StorageTier = "STANDARD"
		cfg.Compression.Enabled = false
	})
	ctx := context.Background()

	ts.PutObject("free-to-read.bin", []byte("no retrieval fee applies to these bytes"))
	if _, err := backend.GetObject(ctx, "free-to-read.bin", 0, 0); err != nil {
		t.Fatalf("GetObject: %v", err)
	}

	stats := backend.CostStats()

	if stats.BytesRetrieved == 0 {
		t.Fatal("BytesRetrieved = 0 after a read: the retrieval assertion below would be vacuous")
	}
	if stats.RetrievalCost != 0 {
		t.Errorf("RetrievalCost = %v on STANDARD, want 0: STANDARD has no retrieval fee, and a "+
			"non-zero here means the rate table is charging one", stats.RetrievalCost)
	}
	if stats.RequestCost == 0 {
		t.Error("RequestCost = 0 after a read and a write: requests are never free on STANDARD, so " +
			"this is the rates failing to load rather than a cheap workload")
	}
}

// TestAServerErrorIsNotBilled is the 5xx exclusion, driven through a real request rather than by calling
// record directly.
//
// The unit test covers the branch; this covers the wiring, and they can disagree: the SDK retries a 500,
// so what reaches the tally is one failed attempt and one successful one. The failure must not be
// counted and the success must be.
func TestAServerErrorIsNotBilled(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	ts.PutObject("retried.bin", []byte("served on the second attempt"))
	ts.ResetRequests()
	before := backend.CostStats()

	ts.InjectFault(testaws.Fault{
		Method:    http.MethodGet,
		KeySuffix: "retried.bin",
		Status:    http.StatusInternalServerError,
		Code:      "InternalError",
		Times:     1,
	})

	if _, err := backend.GetObject(ctx, "retried.bin", 0, 0); err != nil {
		t.Fatalf("GetObject: %v", err)
	}

	counted := countedSince(before, backend.CostStats())
	observed := groupObservedRequests(ts.Requests())

	if observed.serverErrors == 0 {
		t.Fatal("the proxy saw no 5xx: the fault did not fire, so this test proves nothing")
	}

	// Every request the proxy saw, minus the ones AWS would not bill for.
	wantBilled := observed.total - observed.serverErrors

	if counted.total != wantBilled {
		t.Errorf("counted %d billable requests; the proxy saw %d requests of which %d were 5xx, "+
			"so %d are billable\n%s\n"+
			"AWS does not charge for its own server errors, and counting them would spike the "+
			"reported cost during an S3 incident.",
			counted.total, observed.total, observed.serverErrors, wantBilled, observed)
	}
}

// TestCostStatsIsPublishableBeforeAnyFilesystemWork is the state at startup, which is when the first
// scrape arrives: the adapter publishes once before its first tick rather than leaving the family absent
// for thirty seconds.
//
// A fresh backend is not at zero, and that is the finding worth pinning. NewBackend's health check is a
// HeadBucket, which AWS bills like any other HEAD — so a mount that has served no filesystem request has
// still spent money, and a test asserting zero here would be asserting that the health check goes
// uncounted.
func TestCostStatsIsPublishableBeforeAnyFilesystemWork(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	stats := ts.Backend().CostStats()

	if stats.ReadRequests != 1 {
		t.Errorf("ReadRequests = %d on a fresh backend, want 1: the construction-time health check is a "+
			"HeadBucket, and AWS bills it", stats.ReadRequests)
	}
	if stats.WriteRequests != 0 || stats.ListRequests != 0 || stats.FreeRequests != 0 {
		t.Errorf("a backend that has served no filesystem request reports writes=%d lists=%d frees=%d, "+
			"want all zero", stats.WriteRequests, stats.ListRequests, stats.FreeRequests)
	}
	if stats.StoredBytes != 0 || stats.StorageCostPerMonth != 0 {
		t.Errorf("StoredBytes = %d and StorageCostPerMonth = %v before any write, want zero: this mount "+
			"is not responsible for what the bucket already holds",
			stats.StoredBytes, stats.StorageCostPerMonth)
	}
	if stats.RetrievalCost != 0 {
		t.Errorf("RetrievalCost = %v with no bytes retrieved, want 0", stats.RetrievalCost)
	}

	if stats.Region == "" {
		t.Error("Region is empty: a cost series has to be labeled with the region it was priced in, " +
			"and it is known at construction rather than at first request")
	}
	if stats.RatePerWriteRequest == 0 {
		t.Error("RatePerWriteRequest = 0 before any write: the rates come from the tier, not from the " +
			"workload, so they are publishable immediately — and a dashboard that has to wait for " +
			"traffic to learn the rate cannot show the arithmetic behind its first data point")
	}
}

// observedRequests is what the recording proxy saw, grouped the way [s3.CostStats] groups it.
//
// Classified from the HTTP method and query string rather than from an SDK operation name, because that
// is all an HTTP proxy has — and it is a genuinely independent derivation, which is what makes it worth
// comparing against.
type observedRequests struct {
	total        int64
	writes       int64
	lists        int64
	reads        int64
	frees        int64
	serverErrors int64
}

func (o observedRequests) String() string {
	return fmt.Sprintf("total=%d writes=%d lists=%d reads=%d frees=%d 5xx=%d",
		o.total, o.writes, o.lists, o.reads, o.frees, o.serverErrors)
}

// countedSince is what the tally counted between two snapshots, in the shape the proxy's own count comes
// in so the two can be compared field by field.
//
// A delta rather than an absolute because the tally has no reset and should not: it counts from
// construction, which includes the health check NewBackend performs, and a counter a test can zero is a
// counter something else can zero too.
func countedSince(before, after s3.CostStats) observedRequests {
	o := observedRequests{
		writes: after.WriteRequests - before.WriteRequests,
		lists:  after.ListRequests - before.ListRequests,
		reads:  after.ReadRequests - before.ReadRequests,
		frees:  after.FreeRequests - before.FreeRequests,
	}
	o.total = o.writes + o.lists + o.reads + o.frees

	return o
}

func groupObservedRequests(requests []testaws.Request) observedRequests {
	var o observedRequests

	for _, r := range requests {
		o.total++

		if r.Status >= 500 {
			o.serverErrors++

			// Not classified into a group: it is not billable, so which group it would have been in does
			// not arise.
			continue
		}

		switch r.Method {
		case http.MethodDelete:
			o.frees++

		case http.MethodGet, http.MethodHead:
			// A GET with no key is a list — under path-style addressing that is a request to "/bucket"
			// or "/bucket/" with the prefix in the query, which is what ListObjectsV2 sends.
			if isListRequest(r) {
				o.lists++
			} else {
				o.reads++
			}

		default:
			o.writes++
		}
	}

	return o
}

// isListRequest reports whether a GET was a bucket listing rather than an object read.
//
// list-type=2 is ListObjectsV2's marker; the "uploads" sub-resource is ListMultipartUploads. Falling
// back to "a GET at the bucket root" covers ListObjects v1, which sends neither.
func isListRequest(r testaws.Request) bool {
	if strings.Contains(r.Query, "list-type") || strings.Contains(r.Query, "uploads") {
		return true
	}

	// "/bucket" or "/bucket/" — no key component.
	return strings.Count(strings.Trim(r.Path, "/"), "/") == 0
}

// assertRate compares two dollar figures.
//
// 1e-12 rather than an exact comparison: these are products and sums of floats, so the last bit is not
// reproducible, but a real defect in this code is a factor of ten or a factor of 1.074 — both of them
// astronomically larger than the tolerance. The failure message reports the ratio, since that is what
// names the mistake: a round factor of a thousand is a per-1,000 price used as a per-request one, and
// 1.074 is a binary GB where AWS bills a decimal one.
func assertRate(t *testing.T, what string, got, want float64) {
	t.Helper()

	if abs(got-want) <= 1e-12 {
		return
	}

	ratio := "undefined (want is zero)"
	if want != 0 {
		ratio = fmt.Sprintf("%g", got/want)
	}

	t.Errorf("%s = %v, want %v (got/want = %s)", what, got, want, ratio)
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}

	return f
}
