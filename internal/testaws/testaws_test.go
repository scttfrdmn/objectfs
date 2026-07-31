package testaws_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/objectfs/objectfs/internal/testaws"
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

	honoursRange := string(served) == "4567"

	if caps.RangeGET != honoursRange {
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

	if _, err := client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(ts.Bucket),
		Key:      aws.String(key),
		UploadId: create.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: []s3types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}},
		},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
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
