package testaws_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// The harness is itself test infrastructure, so it needs its own tests. If the recorder silently
// stopped counting bytes, or the capability probe started reporting a capability the emulator does
// not have, every test built on it would keep passing while measuring nothing. These tests assert
// the harness's own guarantees.

func TestStartGivesAWorkingBucket(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	if ts.URL == "" {
		t.Fatal("URL is empty")
	}
	if ts.Bucket == "" {
		t.Fatal("Bucket is empty")
	}

	// The bucket name must be legal S3, since a real backend will address it. The test name here
	// contains capitals, which S3 forbids.
	for _, r := range ts.Bucket {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			t.Fatalf("bucket %q contains %q, which S3 does not allow", ts.Bucket, r)
		}
	}
	if n := len(ts.Bucket); n < 3 || n > 63 {
		t.Fatalf("bucket %q is %d characters; S3 allows 3–63", ts.Bucket, n)
	}

	if ts.ObjectExists("absent") {
		t.Error("ObjectExists reported a key that was never written")
	}

	ts.PutObject("present", []byte("hello"))

	if !ts.ObjectExists("present") {
		t.Error("ObjectExists reported a key that was just written as absent")
	}
	if got := ts.GetObject("present"); string(got) != "hello" {
		t.Errorf("GetObject = %q, want %q", got, "hello")
	}
}

func TestEachServerIsIsolated(t *testing.T) {
	t.Parallel()

	// Two servers in the same test must not see each other's objects, or a parallel test could be
	// contaminated by a sibling and a real failure would look like a flake.
	a := testaws.Start(t)
	b := testaws.Start(t)

	if a.URL == b.URL {
		t.Fatal("two Start calls returned the same endpoint")
	}

	a.PutObject("k", []byte("from-a"))

	if b.ObjectExists("k") {
		t.Error("an object written to one server is visible on another")
	}
}

func TestRecorderCountsBytesAndSeesRangeHeaders(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	const key = "counted"

	body := testaws.DeterministicBytes(key, 8192)
	ts.PutObject(key, body)

	// Discard the setup traffic so the assertion below is about the read alone.
	ts.ResetRequests()

	if got := ts.GetObject(key); !bytes.Equal(got, body) {
		t.Fatalf("round trip changed the bytes: got %d bytes, want %d", len(got), len(body))
	}

	gets := ts.GETs(key)
	if len(gets) != 1 {
		t.Fatalf("recorded %d GETs for one read, want 1: %+v", len(gets), gets)
	}
	if gets[0].IsRanged() {
		t.Errorf("an unranged GetObject was recorded as ranged (Range: %q)", gets[0].Range)
	}
	if gets[0].Status != http.StatusOK {
		t.Errorf("GET status = %d, want 200", gets[0].Status)
	}

	// The byte count is the whole reason the recorder exists: read amplification is a byte-count
	// property, and neither the SDK nor the emulator's event store reports one.
	if got := ts.BytesRead(key); got != int64(len(body)) {
		t.Errorf("BytesRead = %d, want %d — the recorder is not counting response bodies", got, len(body))
	}
}

func TestRecorderDoesNotCountHeadsAsReads(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	const key = "headed"

	ts.PutObject(key, testaws.DeterministicBytes(key, 4096))
	ts.ResetRequests()

	if got := ts.ObjectSize(key); got != 4096 {
		t.Fatalf("ObjectSize = %d, want 4096", got)
	}

	// A HEAD transfers no body. Counting it as a read would corrupt the byte accounting that read
	// amplification assertions depend on.
	if gets := ts.GETs(key); len(gets) != 0 {
		t.Errorf("a HEAD was recorded as a GET: %+v", gets)
	}
	if got := ts.BytesRead(key); got != 0 {
		t.Errorf("BytesRead after only a HEAD = %d, want 0", got)
	}
	if n := len(ts.RequestsFor(key)); n != 1 {
		t.Errorf("RequestsFor saw %d requests for the HEAD, want 1", n)
	}
}

func TestRecorderCountsRequestBodies(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	const key = "written"

	body := testaws.DeterministicBytes(key, 2048)

	ts.ResetRequests()
	ts.PutObject(key, body)

	observed := ts.RequestsFor(key)

	var put *testaws.Request

	for i := range observed {
		if observed[i].Method == http.MethodPut {
			put = &observed[i]

			break
		}
	}

	if put == nil {
		t.Fatalf("no PUT recorded for %q: %+v", key, observed)
	}
	if put.RequestBytes != int64(len(body)) {
		t.Errorf("RequestBytes = %d, want %d", put.RequestBytes, len(body))
	}
}

// TestRecorderLogsARequestBeforeItsCallerCanObserveTheResponse is the harness auditing itself.
//
// Everything in this package that asserts on transfer behavior — GETs, BytesRead, the read-amplification
// suite — reads the request log immediately after the operation it is measuring returns. That is only
// sound if a completed response implies a recorded request. It did not: the handler appended to the log
// after the proxy had already written the body to the socket, so a client could hold every byte of a
// response whose request was not yet in the log.
//
// The consequence was not a failure in this package. It was internal/fuse TestShortFileIsServedFromCache
// failing on its precondition — "the first read issued no GET" — which reads as the read path serving
// from a cache it had no way to populate, in a test whose whole subject is cache correctness. One CI run
// in seven. A harness that lags the behavior it observes makes every assertion built on it a coin flip
// weighted by machine load, and it blames the code under test.
//
// Concurrency is required to see it: the window is between two statements on the server goroutine, so a
// single caller alternating request and assertion never lands in it. At 16 workers the old recorder
// missed roughly 4 reads in 960.
func TestRecorderLogsARequestBeforeItsCallerCanObserveTheResponse(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	const (
		workers = 16
		reads   = 40
		size    = 10240
	)

	var missing atomic.Int64

	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range reads {
				key := fmt.Sprintf("logged-%d-%d", w, i)

				want := testaws.DeterministicBytes(key, size)
				ts.PutObject(key, want)

				// Ranged, because that is the shape the read-path assertions use.
				out, err := ts.Client().GetObject(context.Background(), &awss3.GetObjectInput{
					Bucket: aws.String(ts.Bucket),
					Key:    aws.String(key),
					Range:  aws.String(fmt.Sprintf("bytes=0-%d", size-1)),
				})
				if err != nil {
					t.Errorf("%s: ranged GetObject: %v", key, err)

					return
				}

				got, err := io.ReadAll(out.Body)
				_ = out.Body.Close()

				if err != nil {
					t.Errorf("%s: reading the body: %v", key, err)

					return
				}

				if !bytes.Equal(got, want) {
					t.Errorf("%s: read %d bytes, want %d", key, len(got), size)

					return
				}

				// The bytes are in hand, so the GET that carried them happened. It must be visible.
				if len(ts.GETs(key)) == 0 {
					missing.Add(1)
				}
			}
		})
	}

	wg.Wait()

	if n := missing.Load(); n != 0 {
		t.Errorf("%d of %d completed reads observed an empty GET log; a caller holding a whole "+
			"response body must be able to see the request that delivered it, or every assertion "+
			"built on the log is load-dependent",
			n, workers*reads)
	}
}

// TestWritesSeesEveryMethodThatStoresSomething is the harness's own coverage of Writes, which exists so
// that a caller asserting about what a write path sent does not enumerate the HTTP methods itself.
//
// The enumeration is where such an assertion goes wrong. A test that checked only PUT would pass while
// a multipart create sent nothing, and multipart is the path every large object takes — which is how
// the encryption tests came to need this helper. So the cases below are a plain PUT, a multipart create
// (a POST), and a HEAD and a GET that must not appear.
func TestWritesSeesEveryMethodThatStoresSomething(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()
	client := ts.Client()

	const (
		putKey       = "writes/put"
		multipartKey = "writes/multipart"
	)

	ts.ResetRequests()

	ts.PutObject(putKey, testaws.DeterministicBytes(putKey, 512))

	// A read and a HEAD of the same key, to pin that neither is a write. Read amplification and
	// "what did the write send" are different questions and the recorder must not conflate them.
	_ = ts.GetObject(putKey)
	_ = ts.ObjectSize(putKey)

	writes := ts.Writes(putKey)
	if len(writes) != 1 {
		t.Fatalf("Writes(%q) = %d requests, want exactly the one PUT: %+v", putKey, len(writes), writes)
	}
	if writes[0].Method != http.MethodPut {
		t.Errorf("Writes returned a %s; the GET and the HEAD must not count as writes", writes[0].Method)
	}

	// CreateMultipartUpload is a POST, and it is the request that decides the storage class, the
	// content encoding and the encryption for the whole upload — so a helper that missed POST would
	// hide every one of those.
	created, err := client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(multipartKey),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	t.Cleanup(func() {
		_, _ = client.AbortMultipartUpload(context.Background(), &awss3.AbortMultipartUploadInput{
			Bucket:   aws.String(ts.Bucket),
			Key:      aws.String(multipartKey),
			UploadId: created.UploadId,
		})
	})

	mpWrites := ts.Writes(multipartKey)
	if len(mpWrites) != 1 {
		t.Fatalf("Writes(%q) = %d requests, want the one CreateMultipartUpload POST: %+v",
			multipartKey, len(mpWrites), mpWrites)
	}
	if mpWrites[0].Method != http.MethodPost {
		t.Errorf("Writes returned a %s for the multipart create, want POST", mpWrites[0].Method)
	}

	// Keys do not bleed into each other: Writes filters on the path suffix, and "writes/put" is not a
	// suffix of "writes/multipart" or the reverse.
	if n := len(ts.Writes("writes/absent")); n != 0 {
		t.Errorf("Writes for a key nothing wrote returned %d requests", n)
	}
}

func TestResetRequestsKeepsObjects(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	ts.PutObject("kept", []byte("data"))
	ts.ResetRequests()

	if n := len(ts.Requests()); n != 0 {
		t.Errorf("ResetRequests left %d requests behind", n)
	}
	if !ts.ObjectExists("kept") {
		t.Error("ResetRequests deleted an object; it must only clear the request log")
	}
}

func TestResetClearsObjectsAndKeepsTheBucketUsable(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	ts.PutObject("gone", []byte("data"))
	ts.Reset()

	if ts.ObjectExists("gone") {
		t.Error("Reset left an object behind")
	}

	// The bucket must still exist afterwards: callers hold a Config naming it, and recreating it is
	// Reset's job precisely so that config stays valid.
	ts.PutObject("after", []byte("data"))

	if !ts.ObjectExists("after") {
		t.Error("the bucket is unusable after Reset")
	}
}

func TestConfigCarriesEverythingABackendNeeds(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	cfg := ts.Config()

	if cfg.Endpoint != ts.URL {
		t.Errorf("Endpoint = %q, want %q", cfg.Endpoint, ts.URL)
	}
	if !cfg.ForcePathStyle {
		t.Error("ForcePathStyle is false; virtual-host addressing would resolve bucket.localhost")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		t.Error("static credentials are unset, so the SDK would fall back to the ambient profile")
	}
	if cfg.Region != testaws.DefaultRegion {
		t.Errorf("Region = %q, want %q", cfg.Region, testaws.DefaultRegion)
	}
}

func TestListKeysReportsWhatWasWritten(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	ts.PutObject("dir/a", []byte("a"))
	ts.PutObject("dir/b", []byte("b"))
	ts.PutObject("other/c", []byte("c"))

	got := ts.ListKeys("dir/")
	if len(got) != 2 {
		t.Fatalf("ListKeys(%q) = %v, want 2 keys", "dir/", got)
	}

	for _, want := range []string{"dir/a", "dir/b"} {
		found := false

		for _, k := range got {
			if k == want {
				found = true
			}
		}

		if !found {
			t.Errorf("ListKeys(%q) is missing %q: %v", "dir/", want, got)
		}
	}
}

func TestOperationsCountsAPICalls(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	before := ts.Operations("PutObject")

	ts.PutObject("counted-op", []byte("data"))

	if after := ts.Operations("PutObject"); after != before+1 {
		t.Errorf("Operations(PutObject) went %d → %d, want an increase of 1", before, after)
	}
}

func TestMultipartUploadsSeesAnOrphan(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	if got := ts.MultipartUploads(); len(got) != 0 {
		t.Fatalf("a fresh bucket reports %d multipart uploads: %v", len(got), got)
	}

	// This is how the H10 regression test will prove an upload leaked: an abandoned upload is
	// invisible through every other API and bills until a lifecycle rule reaps it.
	created := startMultipart(t, ts, "orphan")

	got := ts.MultipartUploads()
	if len(got) != 1 || got[0] != created {
		t.Errorf("MultipartUploads = %v, want [%q]", got, created)
	}
}

func TestDeterministicBytesVariesByOffsetAndSeed(t *testing.T) {
	t.Parallel()

	a := testaws.DeterministicBytes("seed-a", 4096)
	b := testaws.DeterministicBytes("seed-b", 4096)

	if bytes.Equal(a, b) {
		t.Error("two different seeds produced identical bytes")
	}

	if again := testaws.DeterministicBytes("seed-a", 4096); !bytes.Equal(a, again) {
		t.Error("the same seed produced different bytes; the generator is not deterministic")
	}

	// The point of this generator is that an off-by-one in range arithmetic produces a mismatch
	// rather than a coincidence, which a run of zeros or a short repeating pattern would hide.
	// Every 4-byte window in the output must be distinct from the window one byte over.
	var windows, shifted int

	for i := 0; i+5 <= len(a); i++ {
		windows++

		if !bytes.Equal(a[i:i+4], a[i+1:i+5]) {
			shifted++
		}
	}

	if shifted != windows {
		t.Errorf("%d of %d windows are identical to their neighbor; the sequence repeats",
			windows-shifted, windows)
	}
}

// TestCapabilitiesFailsClosed is the harness's most important test.
//
// The emulator implements a subset of S3 and the subset moves, so the probe reports what is
// actually there. What must never happen is the probe reporting RangeGET true against an endpoint
// that ignores the Range header: ObjectFS's entire read path is ranged GETs, and against such an
// endpoint every ranged assertion passes for the wrong reason, because the whole object contains
// the requested bytes. This test pins the probe to the observable truth rather than to an expected
// answer, so it stays correct whichever way the dependency moves.
func TestCapabilitiesFailsClosed(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	caps := ts.Capabilities()

	// Independently establish the ground truth, without going through the probe.
	const key = "range-truth"

	body := []byte("0123456789abcdef")
	ts.PutObject(key, body)

	out, err := ts.Client().GetObject(context.Background(), rangedGet(ts.Bucket, key, "bytes=4-7"))
	if err != nil {
		t.Fatalf("ranged GetObject failed outright: %v", err)
	}
	defer func() { _ = out.Body.Close() }()

	served := make([]byte, 64)
	n, _ := out.Body.Read(served)
	served = served[:n]

	honorsRange := string(served) == "4567"

	if caps.RangeGET != honorsRange {
		t.Fatalf("Capabilities().RangeGET = %v but a ranged GET returned %q (want %q if honored); "+
			"the probe disagrees with the endpoint",
			caps.RangeGET, served, "4567")
	}

	if !caps.RangeGET {
		if caps.RangeGETDetail == "" {
			t.Error("RangeGET is false with no detail, so the skip message would say nothing")
		}

		t.Logf("this endpoint does not implement ranged GetObject: %s", caps.RangeGETDetail)
	}
}

// TestMultipartContentEncodingProbeMatchesTheEndpoint is the same fail-closed discipline applied to
// the multipart path, and pinned the same way: to what a raw multipart upload actually produces,
// never to an expected answer. When the probe and the endpoint disagree, the probe is wrong.
func TestMultipartContentEncodingProbeMatchesTheEndpoint(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	caps := ts.Capabilities()
	ctx := context.Background()

	const key = "mpu-encoding-truth"

	// Establish the ground truth independently of the probe.
	client := ts.Client()

	create, err := client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket:          aws.String(ts.Bucket),
		Key:             aws.String(key),
		ContentEncoding: aws.String("zstd"),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	part, err := client.UploadPart(ctx, &awss3.UploadPartInput{
		Bucket:     aws.String(ts.Bucket),
		Key:        aws.String(key),
		UploadId:   create.UploadId,
		PartNumber: aws.Int32(1),
		Body:       bytes.NewReader([]byte("truth")),
	})
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}

	if _, completeErr := client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(ts.Bucket),
		Key:      aws.String(key),
		UploadId: create.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: []s3types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}},
		},
	}); completeErr != nil {
		t.Fatalf("CompleteMultipartUpload: %v", completeErr)
	}

	head, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	preserved := aws.ToString(head.ContentEncoding) == "zstd"

	if caps.MultipartContentEncoding != preserved {
		t.Fatalf("Capabilities().MultipartContentEncoding = %v but a multipart upload with "+
			"Content-Encoding: zstd reports %q; the probe disagrees with the endpoint",
			caps.MultipartContentEncoding, aws.ToString(head.ContentEncoding))
	}

	if !preserved {
		if caps.MultipartContentEncodingDetail == "" {
			t.Error("MultipartContentEncoding is false with no detail, so the skip would say nothing")
		}

		t.Logf("this endpoint loses Content-Encoding across a multipart upload: %s",
			caps.MultipartContentEncodingDetail)
	}
}

// TestCapabilityProbesDoNotLeaveObjectsBehind guards a subtle way the harness could corrupt the
// tests built on it: the probes write to the same bucket the test uses, so a listing assertion or a
// leaked-multipart assertion would count the probe's own artifacts as the subject's.
func TestCapabilityProbesDoNotLeaveObjectsBehind(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	_ = ts.Capabilities()

	// The probe keys are reserved and dot-prefixed, so they cannot collide with a test's keys. What
	// matters is that no multipart upload is left open: MultipartUploads is how the H10 orphaned-part
	// regression test will assert, and a probe leftover would make it fail for the wrong reason.
	if got := ts.MultipartUploads(); len(got) != 0 {
		t.Errorf("the capability probes left %d multipart upload(s) open: %v", len(got), got)
	}
}

// TestRequireRangeGETSkipsRatherThanPassing documents the discipline: a test that needs a
// capability the endpoint lacks must skip loudly. If this test reports SKIP, ranged-read coverage
// is genuinely absent and the skip message says why. If it reports PASS, the capability is present.
func TestRequireRangeGETSkipsRatherThanPassing(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const key = "ranged"

	body := testaws.DeterministicBytes(key, 64*1024)
	ts.PutObject(key, body)
	ts.ResetRequests()

	out, err := ts.Client().GetObject(context.Background(), rangedGet(ts.Bucket, key, "bytes=1024-2047"))
	if err != nil {
		t.Fatalf("ranged GetObject: %v", err)
	}
	defer func() { _ = out.Body.Close() }()

	got := readAll(t, out.Body)
	if !bytes.Equal(got, body[1024:2048]) {
		t.Fatalf("ranged read returned %d bytes that do not match body[1024:2048]", len(got))
	}

	// The recorder must show the range, and must show only the ranged bytes crossing the wire.
	// This is the assertion the C4 read-amplification regression test is built out of.
	gets := ts.GETs(key)
	if len(gets) != 1 {
		t.Fatalf("recorded %d GETs, want 1: %+v", len(gets), gets)
	}
	if !gets[0].IsRanged() {
		t.Error("a ranged GET was recorded without a Range header")
	}
	if gets[0].Status != http.StatusPartialContent {
		t.Errorf("ranged GET status = %d, want 206", gets[0].Status)
	}
	if n := ts.BytesRead(key); n != 1024 {
		t.Errorf("BytesRead = %d for a 1 KiB range of a 64 KiB object; a whole-object transfer "+
			"would read %d", n, len(body))
	}
}

// TestInjectFaultFailsExactlyItsBudget is the fault injector's own regression test, and the reason
// it needs one is the same reason FaultsFired exists: a matcher that matches nothing produces a
// passing test indistinguishable from a working retry. The count has to be exact in both
// directions — one fewer and the failure under test never happens, one more and a retry budget the
// caller sized for one failure is exhausted by the harness.
func TestInjectFaultFailsExactlyItsBudget(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	const key = "faulted"
	ts.PutObject(key, []byte("real bytes"))
	ts.ResetRequests()

	// A raw client with retrying off. Both halves matter: a Backend retries, and so does the raw
	// SDK client — see noRetry. What is under test is the proxy, so each call has to make exactly
	// one request.
	client := ts.Client()

	ts.InjectFault(testaws.Fault{Method: "GET", KeySuffix: key, Times: 2})

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := client.GetObject(context.Background(), &awss3.GetObjectInput{
			Bucket: aws.String(ts.Bucket),
			Key:    aws.String(key),
		}, noRetry); err == nil {
			t.Fatalf("attempt %d succeeded against an armed fault", attempt)
		}
	}

	if fired := ts.FaultsFired(); fired != 2 {
		t.Fatalf("FaultsFired = %d after two matching requests, want 2", fired)
	}

	// Budget spent: the third request must reach the emulator and return the object's real bytes.
	// This is what makes "fail once, then succeed" expressible, which is the whole point of a
	// bounded fault rather than substrate's probabilistic one.
	if got := ts.GetObject(key); string(got) != "real bytes" {
		t.Errorf("after the budget was spent the object read %q, want %q", got, "real bytes")
	}
	if fired := ts.FaultsFired(); fired != 2 {
		t.Errorf("FaultsFired = %d after the budget was spent, want 2: the fault fired past its "+
			"budget", fired)
	}

	// The failed requests are recorded even though no response was written, because a test asserting
	// a retry happened has to be able to count the attempt that failed.
	if gets := ts.GETs(key); len(gets) != 3 {
		t.Errorf("recorded %d GETs, want 3 (two faulted, one served): %+v", len(gets), gets)
	}
}

// TestInjectFaultMatchesOnMethodKeyAndRange pins the matcher's selectivity. Over-matching is the
// dangerous direction and it is silent: a fault meant for one chunk of a parallel read that also
// matches the HEAD sizing the read, or the neighboring chunk, fails the operation somewhere other
// than where the test says it does.
func TestInjectFaultMatchesOnMethodKeyAndRange(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const key = "selective"
	ts.PutObject(key, testaws.DeterministicBytes(key, 4096))
	ts.PutObject("other", []byte("untouched"))
	ts.ResetRequests()

	// Armed for one specific chunk of one specific key, which is the shape every parallel-read test
	// uses.
	ts.InjectFault(testaws.Fault{
		Method:      "GET",
		KeySuffix:   key,
		RangePrefix: "bytes=1024-",
		Times:       1,
	})

	client := ts.Client()

	// Each of these differs from the armed fault in exactly one field, so a match by any of them
	// names which part of the matcher is too loose.
	nonMatching := []struct {
		name string
		call func() error
	}{
		{
			name: "a different method on the same key",
			call: func() error {
				_, err := client.HeadObject(context.Background(), &awss3.HeadObjectInput{
					Bucket: aws.String(ts.Bucket), Key: aws.String(key),
				})

				return err
			},
		},
		{
			name: "the same range on a different key",
			call: func() error {
				out, err := client.GetObject(context.Background(),
					rangedGet(ts.Bucket, "other", "bytes=0-8"))
				if err == nil {
					_ = out.Body.Close()
				}

				return err
			},
		},
		{
			name: "a different range on the same key",
			call: func() error {
				out, err := client.GetObject(context.Background(),
					rangedGet(ts.Bucket, key, "bytes=2048-3071"))
				if err == nil {
					_ = out.Body.Close()
				}

				return err
			},
		},
	}

	for _, tc := range nonMatching {
		if err := tc.call(); err != nil {
			t.Errorf("%s was faulted: %v", tc.name, err)
		}
	}

	if fired := ts.FaultsFired(); fired != 0 {
		t.Fatalf("FaultsFired = %d before any matching request, want 0", fired)
	}

	out, err := client.GetObject(context.Background(),
		rangedGet(ts.Bucket, key, "bytes=1024-2047"), noRetry)
	if err == nil {
		_ = out.Body.Close()

		t.Fatal("the matching request succeeded against an armed fault")
	}
	if fired := ts.FaultsFired(); fired != 1 {
		t.Errorf("FaultsFired = %d after the matching request, want 1", fired)
	}
}

// TestFaultOnFireRunsBeforeTheClientSeesTheFailure pins the hook's ordering, which is the property
// tests rely on it for: it turns an interleaving into a fixture. A read whose object is replaced
// between its first chunk and the retry is otherwise a race against a goroutine.
func TestFaultOnFireRunsBeforeTheClientSeesTheFailure(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	const key = "hooked"
	ts.PutObject(key, []byte("first generation"))
	ts.ResetRequests()

	ts.InjectFault(testaws.Fault{
		Method:    "GET",
		KeySuffix: key,
		Times:     1,
		OnFire: func() {
			// A write from inside the hook, which is the hook's actual use: the object the retry
			// reads is not the object the first attempt asked for.
			ts.PutObject(key, []byte("second generation"))
		},
	})

	client := ts.Client()

	if _, err := client.GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String(ts.Bucket), Key: aws.String(key),
	}, noRetry); err == nil {
		t.Fatal("the armed request succeeded")
	}

	// The effect must already be visible to the very next request, not eventually.
	if got := ts.GetObject(key); string(got) != "second generation" {
		t.Errorf("after the hook fired the object read %q, want %q — the hook's effect was not "+
			"visible to the request that followed the failure", got, "second generation")
	}
}

// TestClearFaultsDisarmsUnspentBudget matters for a fixture that arms a fault, asserts the failure,
// and then goes on to assert something about the healthy path: leftover budget would fail a request
// the test believes is unfaulted.
func TestClearFaultsDisarmsUnspentBudget(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	const key = "cleared"
	ts.PutObject(key, []byte("payload"))

	ts.InjectFault(testaws.Fault{Method: "GET", KeySuffix: key, Times: 5})
	ts.ClearFaults()

	if got := ts.GetObject(key); string(got) != "payload" {
		t.Errorf("GetObject = %q after ClearFaults, want %q", got, "payload")
	}
	if fired := ts.FaultsFired(); fired != 0 {
		t.Errorf("FaultsFired = %d after ClearFaults, want 0", fired)
	}
}

// TestFaultDefaultsAreTheUsefulOnes pins the zero-value behavior, and Times is the one that would
// otherwise bite: reading `Times: 0` as unlimited would turn an omitted field into a test that
// exhausts a retry budget and hangs, so it means one.
func TestFaultDefaultsAreTheUsefulOnes(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	const key = "defaulted"
	ts.PutObject(key, []byte("payload"))
	ts.ResetRequests()

	ts.InjectFault(testaws.Fault{Method: "GET", KeySuffix: key})

	client := ts.Client()

	out, err := client.GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String(ts.Bucket), Key: aws.String(key),
	}, noRetry)
	if err == nil {
		_ = out.Body.Close()

		t.Fatal("a Fault with Times unset failed nothing")
	}

	// 500 by default, because that is what the AWS SDK treats as a retryable server error — a fault
	// the SDK declines to retry makes a retry test fail for a reason unrelated to the retry.
	gets := ts.GETs(key)
	if len(gets) != 1 {
		t.Fatalf("recorded %d GETs, want 1: %+v", len(gets), gets)
	}
	if gets[0].Status != http.StatusInternalServerError {
		t.Errorf("faulted GET status = %d, want 500", gets[0].Status)
	}

	// Times defaulted to one, not unlimited: the next request is served.
	if got := ts.GetObject(key); string(got) != "payload" {
		t.Errorf("the request after a single-fire fault read %q, want %q", got, "payload")
	}
}

// TestFaultQueryKeyDistinguishesMultipartSubOperations covers the matcher dimension that method and
// path cannot supply.
//
// CreateMultipartUpload and CompleteMultipartUpload are both a POST to "/bucket/key". They differ
// only in the query string, so a Fault written without QueryKey and aimed at Complete fires on the
// create instead — and a test asserting "the failed upload left no orphan behind" then passes
// because the create failed and no upload was ever started. That happened; QueryKey exists because
// a mutation that removed the abort under test entirely did not fail the test that was meant to
// catch it.
func TestFaultQueryKeyDistinguishesMultipartSubOperations(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	const key = "multipart/query-key"

	client := ts.Client()

	// Armed for the complete. If QueryKey were ignored, this create would be what fails.
	ts.InjectFault(testaws.Fault{
		Method:    "POST",
		KeySuffix: key,
		QueryKey:  "uploadId",
	})

	created, err := client.CreateMultipartUpload(context.Background(), &awss3.CreateMultipartUploadInput{
		Bucket: aws.String(ts.Bucket), Key: aws.String(key),
	}, noRetry)
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed against a fault armed for uploadId; QueryKey did not "+
			"discriminate, so the two POSTs of a multipart upload are indistinguishable: %v", err)
	}
	if fired := ts.FaultsFired(); fired != 0 {
		t.Fatalf("the fault fired %d times on the create, want 0", fired)
	}

	uploadID := aws.ToString(created.UploadId)

	// A part is a PUT carrying partNumber, so neither armed matcher should touch it.
	part, err := client.UploadPart(context.Background(), &awss3.UploadPartInput{
		Bucket: aws.String(ts.Bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		PartNumber: aws.Int32(1), Body: bytes.NewReader(testaws.DeterministicBytes(key, 5*1024*1024)),
	}, noRetry)
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}

	_, err = client.CompleteMultipartUpload(context.Background(), &awss3.CompleteMultipartUploadInput{
		Bucket: aws.String(ts.Bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: []s3types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}},
		},
	}, noRetry)
	if err == nil {
		t.Fatal("CompleteMultipartUpload succeeded against a fault armed for uploadId")
	}
	if fired := ts.FaultsFired(); fired != 1 {
		t.Errorf("FaultsFired = %d after the complete, want 1", fired)
	}
}

// TestFaultQueryKeyMatchesRegardlessOfValue pins the matcher to presence rather than equality,
// which is what makes it usable: an upload ID is generated per upload, so a test cannot know the
// value to match, and "?uploadId=<whatever>" is the only form that is ever needed.
func TestFaultQueryKeyMatchesRegardlessOfValue(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	const key = "query-key/presence"
	ts.PutObject(key, []byte("payload"))
	ts.ResetRequests()

	// partNumber is absent from a plain GET, so this must not fire.
	ts.InjectFault(testaws.Fault{Method: "GET", KeySuffix: key, QueryKey: "partNumber"})

	if got := ts.GetObject(key); string(got) != "payload" {
		t.Errorf("GetObject = %q, want %q — a fault requiring an absent query key fired anyway",
			got, "payload")
	}
	if fired := ts.FaultsFired(); fired != 0 {
		t.Errorf("FaultsFired = %d, want 0", fired)
	}
}
