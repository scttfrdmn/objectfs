package testaws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/scttfrdmn/substrate/emulator"

	"github.com/objectfs/objectfs/internal/storage/s3"
)

// The built-in credential every substrate emulator accepts, and the account it maps to. Passing
// these explicitly rather than through the environment is what lets these tests run with
// t.Parallel: t.Setenv and t.Parallel are mutually exclusive, and an ambient AWS_PROFILE would
// otherwise leak a real account into what is supposed to be a hermetic test.
const (
	AccessKeyID = "AKIATEST12345678901"

	// nolint:gosec // G101 is right that this is a hardcoded credential and wrong that it matters:
	// it is the example key from AWS's own documentation, which every substrate emulator accepts and
	// no AWS account has ever used. Hardcoding it is what makes these tests hermetic — see above.
	SecretAccessKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"

	// DefaultRegion is the emulator's region. It is not us-west-2: nothing here talks to AWS,
	// and using the real deployment region would make an accidental escape to the live API
	// harder to notice in a request log.
	DefaultRegion = "us-east-1"
)

// TestServer is a running in-process AWS endpoint plus the bucket a test was given.
type TestServer struct {
	// URL is the endpoint to configure clients with, e.g. "http://127.0.0.1:54321". It is the
	// recording proxy's address, not the emulator's, so every request a test makes is counted.
	URL string

	// Bucket is a bucket created for this test and unique to it.
	Bucket string

	// Server is the underlying substrate server, for tests that need its time controller,
	// event store, or fault injection directly.
	Server *emulator.TestServer

	t   *testing.T
	rec *recorder

	capsOnce sync.Once
	caps     Capabilities
}

// Start brings up an in-process S3 endpoint with one bucket, and returns a handle to it. The server
// is shut down when the test ends.
//
// The bucket name is derived from the test name so a failure log says which test owned it. Each call
// gets its own server, so tests using this may run in parallel.
func Start(t *testing.T) *TestServer {
	t.Helper()

	srv := emulator.StartTestServer(t)
	proxyURL, rec := startRecorder(t, srv.URL)

	ts := &TestServer{
		URL:    proxyURL,
		Bucket: bucketNameFor(t),
		Server: srv,
		t:      t,
		rec:    rec,
	}

	client := ts.Client()
	if _, err := client.CreateBucket(context.Background(), &awss3.CreateBucketInput{
		Bucket: aws.String(ts.Bucket),
	}); err != nil {
		t.Fatalf("testaws: create bucket %q: %v", ts.Bucket, err)
	}

	return ts
}

// bucketNameFor derives a valid S3 bucket name from a test name. S3 allows 3–63 characters of
// lowercase letters, digits, hyphens, and dots, starting and ending alphanumeric — so subtest
// separators, capitals, and the underscores Go substitutes for spaces all have to go.
func bucketNameFor(t *testing.T) string {
	t.Helper()

	var b strings.Builder

	b.WriteString("objectfs-")

	for _, r := range strings.ToLower(t.Name()) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			// "/" from subtests, "_" from spaces, and anything else.
			b.WriteByte('-')
		}
	}

	name := strings.Trim(b.String(), "-")
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}

	return name
}

// Config returns an S3 backend config pointed at this server, with a bucket that exists. Callers
// adjust the returned value rather than building one from scratch, so a field the harness must set
// — the endpoint, path-style addressing, static credentials — cannot be forgotten.
//
// Path-style addressing is required: virtual-host style would resolve
// bucket.localhost:port, which does not exist.
func (ts *TestServer) Config() *s3.Config {
	cfg := baseConfig()
	cfg.Endpoint = ts.URL

	return cfg
}

// baseConfig is the emulator-facing part of a backend config, shared by [TestServer.Config] and
// [SharedServer.Config] so the two cannot drift. The caller sets the endpoint.
func baseConfig() *s3.Config {
	cfg := s3.NewDefaultConfig()
	cfg.ForcePathStyle = true
	cfg.Region = DefaultRegion
	cfg.AccessKeyID = AccessKeyID
	cfg.SecretAccessKey = SecretAccessKey

	// The emulator answers instantly, so a short retry budget keeps a genuine failure from
	// taking 30 seconds to surface as a timeout that hides its own cause.
	cfg.MaxRetries = 2

	return cfg
}

// Backend constructs an ObjectFS S3 backend against this server. Test-relevant config adjustments
// are applied by the optional mutators before the backend is built.
func (ts *TestServer) Backend(mutate ...func(*s3.Config)) *s3.Backend {
	ts.t.Helper()

	cfg := ts.Config()
	for _, m := range mutate {
		m(cfg)
	}

	backend, err := s3.NewBackend(context.Background(), ts.Bucket, cfg)
	if err != nil {
		ts.t.Fatalf("testaws: build backend: %v", err)
	}
	ts.t.Cleanup(func() { _ = backend.Close() })

	return backend
}

// Client returns a raw AWS SDK S3 client for this server. Use it to set up preconditions and to
// verify results independently of ObjectFS's own code — a test that both writes and reads through
// the layer under test cannot detect a symmetric encoding bug.
func (ts *TestServer) Client() *awss3.Client {
	ts.t.Helper()

	return ts.ClientContext(context.Background())
}

// ClientContext is [TestServer.Client] with an explicit context for the credential and config
// resolution the SDK performs while the client is built.
//
// It exists so that a method already holding a context does not have to construct its client from a
// detached one. Nothing in the config chain here does I/O — the credentials are static and the
// endpoint is a literal — so the context is not load-bearing today; the point is that a helper which
// silently substitutes context.Background() teaches every caller above it to stop propagating, and
// that habit is what produced several of the audit's cancellation findings.
func (ts *TestServer) ClientContext(ctx context.Context) *awss3.Client {
	ts.t.Helper()

	client, err := newClient(ctx, ts.URL)
	if err != nil {
		ts.t.Fatalf("testaws: %v", err)
	}

	return client
}

// newClient builds a raw SDK client against an emulator endpoint. It returns an error rather than
// calling t.Fatalf so [SharedServer], which has no *testing.T by design, can use it too.
func newClient(ctx context.Context, endpoint string) (*awss3.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(DefaultRegion),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(AccessKeyID, SecretAccessKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	return awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	}), nil
}

// PutObject writes an object directly, bypassing ObjectFS. Use it to establish what a read test
// should find.
func (ts *TestServer) PutObject(key string, data []byte) {
	ts.t.Helper()

	if _, err := ts.Client().PutObject(context.Background(), &awss3.PutObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	}); err != nil {
		ts.t.Fatalf("testaws: put %q: %v", key, err)
	}
}

// GetObject reads an object directly, bypassing ObjectFS. Use it to check what a write test
// actually stored.
func (ts *TestServer) GetObject(key string) []byte {
	ts.t.Helper()

	out, err := ts.Client().GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		ts.t.Fatalf("testaws: get %q: %v", key, err)
	}
	defer func() { _ = out.Body.Close() }()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		ts.t.Fatalf("testaws: read %q: %v", key, err)
	}

	return data
}

// ObjectExists reports whether a key is present, distinguishing absence from every other error.
func (ts *TestServer) ObjectExists(key string) bool {
	ts.t.Helper()

	_, err := ts.Client().HeadObject(context.Background(), &awss3.HeadObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true
	}

	// S3's HeadObject reports absence as NotFound, not NoSuchKey — a distinction ObjectFS's own
	// DeleteObject got wrong.
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return false
	}

	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return false
	}

	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return false
		}
	}

	ts.t.Fatalf("testaws: head %q: %v", key, err)

	return false
}

// Capabilities describes what the running emulator actually implements. The emulator's S3 subset
// grows over time, so this is probed at runtime rather than pinned to a version: a test gated on a
// hardcoded version number starts skipping for the wrong reason the moment the dependency moves.
type Capabilities struct {
	// RangeGET reports whether GetObject honors the Range header, answering 206 with a
	// Content-Range. When false, a ranged request is served the *whole* object with a 200,
	// which silently turns every read-path assertion into a tautology.
	RangeGET bool

	// RangeGETDetail explains a false RangeGET, for the skip message.
	RangeGETDetail string

	// MultipartContentEncoding reports whether Content-Encoding set on CreateMultipartUpload
	// survives to the assembled object. When false, a compressed object large enough to cross the
	// multipart threshold reads back as still-encoded — which is indistinguishable from the
	// application having failed to set the header at all.
	MultipartContentEncoding bool

	// MultipartContentEncodingDetail explains a false MultipartContentEncoding.
	MultipartContentEncodingDetail string

	// MetadataReplace reports whether CopyObject honors MetadataDirective=REPLACE. When false, a
	// self-copy answers 200 and carries the *source* object's metadata forward, so every
	// attribute-only write — chmod, chown, touch — appears to succeed and stores nothing.
	MetadataReplace bool

	// MetadataReplaceDetail explains a false MetadataReplace.
	MetadataReplaceDetail string
}

// Capabilities probes the running server once and caches the result.
func (ts *TestServer) Capabilities() Capabilities {
	ts.capsOnce.Do(func() { ts.caps = ts.probeCapabilities() })

	return ts.caps
}

// probeCapabilities asks the server what it can do, by doing it. The probe writes to a reserved key
// under this test's own bucket, so it cannot disturb a test's objects.
func (ts *TestServer) probeCapabilities() Capabilities {
	ts.t.Helper()

	const probeKey = ".objectfs-capability-probe"

	var caps Capabilities

	// A 16-byte object read as bytes 4-7 must come back as exactly "EFGH" with a 206.
	body := []byte("ABCDEFGHIJKLMNOP")
	ts.PutObject(probeKey, body)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := ts.ClientContext(ctx).GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(probeKey),
		Range:  aws.String("bytes=4-7"),
	})
	if err != nil {
		caps.RangeGETDetail = fmt.Sprintf("ranged GetObject failed: %v", err)

		return caps
	}
	defer func() { _ = out.Body.Close() }()

	got, err := io.ReadAll(out.Body)
	if err != nil {
		caps.RangeGETDetail = fmt.Sprintf("reading the ranged body failed: %v", err)

		return caps
	}

	switch {
	case string(got) != "EFGH":
		caps.RangeGETDetail = fmt.Sprintf(
			"a request for bytes 4-7 of a %d-byte object returned %d bytes (%q); the endpoint "+
				"is ignoring the Range header and serving whole objects",
			len(body), len(got), truncate(string(got), 32))
	case aws.ToInt64(out.ContentLength) != 4:
		caps.RangeGETDetail = fmt.Sprintf("ranged response reported Content-Length %d, want 4",
			aws.ToInt64(out.ContentLength))
	case aws.ToString(out.ContentRange) == "":
		caps.RangeGETDetail = "ranged response carried no Content-Range header"
	default:
		caps.RangeGET = true
	}

	caps.MultipartContentEncoding, caps.MultipartContentEncodingDetail =
		ts.probeMultipartContentEncoding(ctx)

	caps.MetadataReplace, caps.MetadataReplaceDetail = ts.probeMetadataReplace(ctx)

	return caps
}

// probeMetadataReplace checks whether a self-copy with MetadataDirective=REPLACE actually replaces the
// object's user metadata.
//
// This is the only way S3 offers to change metadata without re-uploading the body, so it is the path
// every chmod, chown, and touch takes. The probe writes an object with one metadata value, replaces it
// with another, and reads back: an endpoint that ignores the directive reports 200 and returns the
// original value.
func (ts *TestServer) probeMetadataReplace(ctx context.Context) (bool, string) {
	ts.t.Helper()

	const probeKey = ".objectfs-capability-probe-meta"

	if _, err := ts.ClientContext(ctx).PutObject(ctx, &awss3.PutObjectInput{
		Bucket:   aws.String(ts.Bucket),
		Key:      aws.String(probeKey),
		Body:     bytes.NewReader([]byte("probe")),
		Metadata: map[string]string{"objectfs-probe": "before"},
	}); err != nil {
		return false, fmt.Sprintf("PutObject failed: %v", err)
	}

	if _, err := ts.ClientContext(ctx).CopyObject(ctx, &awss3.CopyObjectInput{
		Bucket:            aws.String(ts.Bucket),
		Key:               aws.String(probeKey),
		CopySource:        aws.String(url.PathEscape(ts.Bucket + "/" + probeKey)),
		MetadataDirective: s3types.MetadataDirectiveReplace,
		Metadata:          map[string]string{"objectfs-probe": "after"},
	}); err != nil {
		return false, fmt.Sprintf("CopyObject with MetadataDirective=REPLACE failed: %v", err)
	}

	head, err := ts.ClientContext(ctx).HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(probeKey),
	})
	if err != nil {
		return false, fmt.Sprintf("HeadObject after the replace failed: %v", err)
	}

	var got string
	for k, v := range head.Metadata {
		if strings.EqualFold(k, "objectfs-probe") {
			got = v
		}
	}

	if got != "after" {
		return false, fmt.Sprintf(
			"after a self-copy with MetadataDirective=REPLACE setting objectfs-probe=after, the object "+
				"reports objectfs-probe=%q; the endpoint is ignoring the directive and carrying the "+
				"source metadata forward", got)
	}

	return true, ""
}

// RequireMetadataReplace skips the test unless the endpoint honors MetadataDirective=REPLACE.
//
// Skipping rather than asserting, for the same reason as Range: against an endpoint that ignores the
// directive, ObjectFS's attribute write path *correctly* reports an integrity failure — it reads the
// metadata back and finds the old value — so the test cannot distinguish a working chmod from a broken
// one. Both directions of assertion would be wrong.
func (ts *TestServer) RequireMetadataReplace() {
	ts.t.Helper()

	if caps := ts.Capabilities(); !caps.MetadataReplace {
		ts.t.Skipf("the test endpoint does not honor CopyObject MetadataDirective=REPLACE, which is the "+
			"only way S3 offers to change an object's metadata without re-uploading it, so an "+
			"attribute-only write here cannot be distinguished from one that never happened: %s\n"+
			"The pinned substrate does honor it as of v0.82.0 (scttfrdmn/substrate#421), so reaching this "+
			"skip means the endpoint under test is something else — an older substrate, or a third-party "+
			"S3 implementation. Real S3 honors the directive and the live integration suite covers it.\n"+
			"ObjectFS's own path verifies the metadata landed (internal/vfs.Flusher.attemptAttrOnly), which "+
			"is why the skip is correct here rather than an assertion in either direction.",
			caps.MetadataReplaceDetail)
	}
}

// probeMultipartContentEncoding checks whether Content-Encoding survives a multipart upload.
//
// The probe uploads a single small part rather than a real 5 MB one: the header is recorded at
// CreateMultipartUpload and applied at CompleteMultipartUpload, so a one-part upload exercises the
// whole path an object property has to travel. Real S3 exempts a sole part from the 5 MB minimum, so
// this is a legal upload there too.
func (ts *TestServer) probeMultipartContentEncoding(ctx context.Context) (bool, string) {
	ts.t.Helper()

	const probeKey = ".objectfs-capability-probe-mpu"

	create, err := ts.ClientContext(ctx).CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket:          aws.String(ts.Bucket),
		Key:             aws.String(probeKey),
		ContentEncoding: aws.String("zstd"),
	})
	if err != nil {
		return false, fmt.Sprintf("CreateMultipartUpload failed: %v", err)
	}

	part, err := ts.ClientContext(ctx).UploadPart(ctx, &awss3.UploadPartInput{
		Bucket:     aws.String(ts.Bucket),
		Key:        aws.String(probeKey),
		UploadId:   create.UploadId,
		PartNumber: aws.Int32(1),
		Body:       bytes.NewReader([]byte("probe")),
	})
	if err != nil {
		return false, fmt.Sprintf("UploadPart failed: %v", err)
	}

	if _, err := ts.ClientContext(ctx).CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(ts.Bucket),
		Key:      aws.String(probeKey),
		UploadId: create.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: []s3types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}},
		},
	}); err != nil {
		return false, fmt.Sprintf("CompleteMultipartUpload failed: %v", err)
	}

	head, err := ts.ClientContext(ctx).HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(probeKey),
	})
	if err != nil {
		return false, fmt.Sprintf("HeadObject on the assembled object failed: %v", err)
	}

	if enc := aws.ToString(head.ContentEncoding); enc != "zstd" {
		return false, fmt.Sprintf(
			"an object uploaded through CreateMultipartUpload with Content-Encoding: zstd reports "+
				"Content-Encoding %q; the endpoint drops the header between create and complete", enc)
	}

	return true, ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "..."
}

// RequireRangeGET skips the test unless the endpoint honors the Range header.
//
// Skipping is the only safe response. ObjectFS's entire read path is ranged GETs, so against an
// endpoint that ignores Range every ranged assertion passes for the wrong reason: the whole object
// contains the requested bytes, so a naive comparison of a prefix succeeds and a test that should
// have caught a read-path defect certifies it instead.
func (ts *TestServer) RequireRangeGET() {
	ts.t.Helper()

	if caps := ts.Capabilities(); !caps.RangeGET {
		ts.t.Skipf("the test endpoint does not support ranged GetObject, so a ranged read here "+
			"would pass for the wrong reason: %s\n"+
			"Tracked upstream as scttfrdmn/substrate#396; rerun once the dependency includes it.",
			caps.RangeGETDetail)
	}
}

// RequireMultipartContentEncoding skips the test unless Content-Encoding survives a multipart
// upload.
//
// Skipping is right for the same reason it is with Range: against an endpoint that drops the header,
// a correct application looks broken *and* an application that never set the header looks correct.
// Neither direction is worth asserting.
func (ts *TestServer) RequireMultipartContentEncoding() {
	ts.t.Helper()

	if caps := ts.Capabilities(); !caps.MultipartContentEncoding {
		ts.t.Skipf("the test endpoint loses Content-Encoding across a multipart upload, so this "+
			"test cannot distinguish a correct write path from one that never set the header: %s\n"+
			"Tracked upstream as scttfrdmn/substrate#406; rerun once the dependency includes it.\n"+
			"ObjectFS's own multipart path does set it (internal/storage/s3/multipart_upload.go), "+
			"and real S3 preserves it — the live integration suite covers this.",
			caps.MultipartContentEncodingDetail)
	}
}

// SeedRandom fills a key with n deterministic pseudo-random bytes and returns them. The content is
// derived from the key and offset, so a test that reassembles a range from the wrong offset gets
// bytes that do not match — which a run of zeros or a repeating pattern would hide.
func (ts *TestServer) SeedRandom(key string, n int) []byte {
	ts.t.Helper()

	data := DeterministicBytes(key, n)
	ts.PutObject(key, data)

	return data
}

// DeterministicBytes returns n bytes derived from seed. Every offset has a distinct value with a
// long period, so an off-by-one in range arithmetic produces a mismatch rather than a coincidence.
func DeterministicBytes(seed string, n int) []byte {
	// FNV-1a over the seed gives the starting state; a xorshift step gives the sequence.
	state := uint64(14695981039346656037)
	for i := range len(seed) {
		state ^= uint64(seed[i])
		state *= 1099511628211
	}
	if state == 0 {
		state = 1
	}

	out := make([]byte, n)
	for i := range out {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		out[i] = byte(state >> 24)
	}

	return out
}

// ObjectMetadata returns a key's S3 user metadata, with keys lowercased. ObjectFS records its
// checksum and original size there, so this is how a test checks them without trusting the code
// that wrote them.
func (ts *TestServer) ObjectMetadata(key string) map[string]string {
	ts.t.Helper()

	out, err := ts.Client().HeadObject(context.Background(), &awss3.HeadObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		ts.t.Fatalf("testaws: head %q: %v", key, err)
	}

	meta := make(map[string]string, len(out.Metadata))
	for k, v := range out.Metadata {
		meta[strings.ToLower(k)] = v
	}

	return meta
}

// ObjectStorageClass returns a key's storage class, normalizing the absent header to "STANDARD".
//
// S3 "returns this header for all objects except for S3 Standard storage class objects", so an empty
// value is not unknown — it is STANDARD, and a test that treated the two differently would report a
// tier demotion as a missing header. This matters because a demotion is silent and expensive: the
// object keeps working and the bill changes.
func (ts *TestServer) ObjectStorageClass(key string) string {
	ts.t.Helper()

	out, err := ts.Client().HeadObject(context.Background(), &awss3.HeadObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		ts.t.Fatalf("testaws: head %q: %v", key, err)
	}

	if out.StorageClass == "" {
		return string(s3types.StorageClassStandard)
	}

	return string(out.StorageClass)
}

// ObjectSize returns a key's stored ContentLength — the compressed length for a compressed object,
// which is exactly the distinction the v0.10.0 size defect turned on.
func (ts *TestServer) ObjectSize(key string) int64 {
	ts.t.Helper()

	out, err := ts.Client().HeadObject(context.Background(), &awss3.HeadObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		ts.t.Fatalf("testaws: head %q: %v", key, err)
	}

	return aws.ToInt64(out.ContentLength)
}

// ListKeys returns every key under a prefix, sorted by the emulator's own order.
func (ts *TestServer) ListKeys(prefix string) []string {
	ts.t.Helper()

	var keys []string

	paginator := awss3.NewListObjectsV2Paginator(ts.Client(), &awss3.ListObjectsV2Input{
		Bucket: aws.String(ts.Bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.Background())
		if err != nil {
			ts.t.Fatalf("testaws: list %q: %v", prefix, err)
		}
		for _, obj := range page.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
	}

	return keys
}

// MultipartUploads returns the upload IDs of in-progress multipart uploads. An orphaned upload is
// invisible through every other API and bills until a lifecycle rule reaps it, so this is how a
// test proves the abort path ran.
func (ts *TestServer) MultipartUploads() []string {
	ts.t.Helper()

	out, err := ts.Client().ListMultipartUploads(context.Background(), &awss3.ListMultipartUploadsInput{
		Bucket: aws.String(ts.Bucket),
	})
	if err != nil {
		ts.t.Fatalf("testaws: list multipart uploads: %v", err)
	}

	ids := make([]string, 0, len(out.Uploads))
	for _, u := range out.Uploads {
		ids = append(ids, aws.ToString(u.UploadId))
	}

	return ids
}

// Operations returns how many S3 API calls the emulator recorded for an operation, e.g. "GetObject".
// It answers at the API level; [TestServer.GETs] and [TestServer.BytesRead] answer at the HTTP level
// and are what read-path assertions want.
func (ts *TestServer) Operations(operation string) int {
	ts.t.Helper()

	events, err := ts.Server.Store().GetEvents(context.Background(), emulator.EventFilter{
		Service:   "s3",
		Operation: operation,
	})
	if err != nil {
		ts.t.Fatalf("testaws: query event store: %v", err)
	}

	return len(events)
}

// Reset clears every object and all recorded state, so one server can back a sequence of
// independent cases. The bucket is recreated, since dropping it would invalidate the config
// callers already hold.
func (ts *TestServer) Reset() {
	ts.t.Helper()

	ts.Server.ResetState(ts.t)
	ts.rec.reset()

	if _, err := ts.Client().CreateBucket(context.Background(), &awss3.CreateBucketInput{
		Bucket: aws.String(ts.Bucket),
	}); err != nil {
		ts.t.Fatalf("testaws: recreate bucket %q after reset: %v", ts.Bucket, err)
	}

	// A probe result describes the server, which Reset does not change — but the probe object
	// is gone, so re-probing would have to rewrite it. Keep the cached answer.
}

// HTTPClient returns a client for the server's own control endpoints, e.g. fault injection at
// POST /v1/fault/rules. It is not for S3 traffic; use [TestServer.Client] for that.
func (ts *TestServer) HTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}
