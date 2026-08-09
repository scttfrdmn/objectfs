//go:build s3compat

package s3_test

// Conditional-write semantics against a non-AWS S3-compatible endpoint.
//
// This is the suite behind docs/design/conditional-write-compatibility.md, and it exists because that
// matrix cannot be maintained by reading. Two of its four rows were documentation reads when #285 was
// filed, and both turned out to be wrong in a direction that mattered: MinIO's conditional writes were
// recorded as "source read only, server enforcement unverified" and are in fact enforced exactly as AWS
// enforces them, while Ceph RGW was recorded as "No — documentation absence" and in fact accepts
// conditional headers, answers 412 for a key that does not exist, and ignores preconditions on
// CompleteMultipartUpload entirely. The second is the dangerous shape: it passed the #282 capability
// probe as written.
//
// Deliberately not internal/testaws. An emulator agreeing with our expectations proves the emulator
// agrees; the whole point here is the endpoint. testaws remains right for #282's own unit tests, and
// the hermetic suite in conditional_test.go is not replaced by this one.
//
// Run against a local MinIO:
//
//	podman run -d --rm -p 9111:9000 -e MINIO_ROOT_USER=objectfs -e MINIO_ROOT_PASSWORD=objectfs123 \
//	  minio/minio server /data
//	OBJECTFS_COMPAT_ENDPOINT=http://127.0.0.1:9111 \
//	  OBJECTFS_COMPAT_ACCESS_KEY=objectfs OBJECTFS_COMPAT_SECRET_KEY=objectfs123 \
//	  go test -race -tags=s3compat -v -count=1 ./internal/storage/s3/
//
// Against a local Ceph RGW:
//
//	podman run -d --rm -p 9112:8080 -e CEPH_DEMO_UID=objectfs \
//	  -e CEPH_DEMO_ACCESS_KEY=objectfs -e CEPH_DEMO_SECRET_KEY=objectfs123 \
//	  -e CEPH_DEMO_BUCKET=seed -e RGW_NAME=localhost -e MON_IP=127.0.0.1 \
//	  -e CEPH_PUBLIC_NETWORK=0.0.0.0/0 -e RGW_FRONTEND_PORT=8080 quay.io/ceph/demo:latest
//	OBJECTFS_COMPAT_ENDPOINT=http://127.0.0.1:9112 ... go test -race -tags=s3compat ...
//
// Against a local RustFS:
//
//	podman run -d -p 9113:9000 -e RUSTFS_ACCESS_KEY=objectfs -e RUSTFS_SECRET_KEY=objectfs123 \
//	  -e RUSTFS_ADDRESS=0.0.0.0:9000 docker.io/rustfs/rustfs:latest
//	OBJECTFS_COMPAT_ENDPOINT=http://127.0.0.1:9113 \
//	  OBJECTFS_COMPAT_ACCESS_KEY=objectfs OBJECTFS_COMPAT_SECRET_KEY=objectfs123 \
//	  go test -race -tags=s3compat -v -count=1 ./internal/storage/s3/
//
// And against the real service, which is the reference every other row is compared to. The keys are
// optional given an endpoint, so the default credential chain supplies them; the region is not, since a
// bucket lives in one and CreateBucket signed for another is rejected:
//
//	AWS_PROFILE=aws OBJECTFS_COMPAT_ENDPOINT=https://s3.us-west-2.amazonaws.com \
//	  OBJECTFS_COMPAT_REGION=us-west-2 go test -race -tags=s3compat -v -count=1 ./internal/storage/s3/
//
// The endpoint is what gates the whole suite, deliberately: an unset one skips rather than defaulting to
// AWS, so a run intended for MinIO cannot silently sign against the real service and probe a real
// bucket. Each test creates its own bucket and removes it, including any incomplete multipart uploads,
// since a bucket holding one cannot be deleted and this suite creates uploads that may not complete.
//
// Against Wasabi, which has no local or emulated form — it needs a real account and the real service.
// The region is part of the endpoint host, and a bucket created in one is not reachable through another:
//
//	OBJECTFS_COMPAT_ENDPOINT=https://s3.wasabisys.com OBJECTFS_COMPAT_REGION=us-east-1 \
//	  OBJECTFS_COMPAT_ACCESS_KEY=... OBJECTFS_COMPAT_SECRET_KEY=... \
//	  go test -race -tags=s3compat -v -count=1 ./internal/storage/s3/
//
// Wasabi accepts every conditional header and evaluates none of them, so it is the endpoint that
// exercises the recorded-not-failed paths below: the capability probe reports unsupported, PutObjectIf
// returns ErrNotSupported, and TestCompatConcurrentAbsentWritersElectExactlyOne skips because a refused
// write leaves no contenders to arbitrate between. That is the fail-closed direction and the suite
// passes on it.
//
// Or against any other S3-compatible store, by pointing the same variables at it.
//
// Every test here is written to *record* what an endpoint does and fail only on what would be unsafe.
// So a store that declines conditional writes outright passes: refusing is harmless, since PutObjectIf
// then returns ErrNotSupported and a caller needing mutual exclusion refuses to start. What fails is
// an endpoint whose behavior would silently produce two winners, or whose behavior the #282 capability
// probe describes incorrectly.

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// compatEndpoint returns the endpoint under test, or skips.
//
// Three variables rather than reusing AWS_* deliberately: a run against MinIO must not silently pick up
// an AWS profile from the environment and probe a real bucket instead — which is a bill, and a matrix
// row attributed to the wrong store.
func compatEndpoint(t *testing.T) (endpoint, accessKey, secretKey string) {
	t.Helper()

	endpoint = os.Getenv("OBJECTFS_COMPAT_ENDPOINT")
	if endpoint == "" {
		t.Skip("set OBJECTFS_COMPAT_ENDPOINT (and OBJECTFS_COMPAT_ACCESS_KEY / _SECRET_KEY, or a usable " +
			"default credential chain) to probe an S3-compatible endpoint")
	}

	// The keys are optional; the endpoint is not. An unset endpoint skips rather than falling back to
	// AWS, because a run intended for MinIO that silently signed against the real service would probe a
	// real bucket — a bill, and a matrix row attributed to the wrong store. Given an explicit endpoint,
	// empty keys fall through to the default chain, which is how the AWS row is produced (the profile
	// there is `aws`, per CLAUDE.md, and there are no static keys to hand).
	return endpoint, os.Getenv("OBJECTFS_COMPAT_ACCESS_KEY"), os.Getenv("OBJECTFS_COMPAT_SECRET_KEY")
}

// compatRegion is what the SDK signs with, defaulting to us-east-1: S3-compatible stores generally
// accept any region in the signature, and it is the one MinIO and RGW both take. Real AWS does not —
// a bucket lives in a region and CreateBucket signed for the wrong one is rejected — so a run against
// the real service sets OBJECTFS_COMPAT_REGION.
func compatRegion() string {
	if r := os.Getenv("OBJECTFS_COMPAT_REGION"); r != "" {
		return r
	}

	return "us-east-1"
}

// compatClients builds a raw SDK client and an ObjectFS backend against a fresh bucket on the endpoint.
func compatClients(t *testing.T, ctx context.Context) (*awss3.Client, *s3.Backend, string) {
	t.Helper()

	endpoint, accessKey, secretKey := compatEndpoint(t)
	region := compatRegion()

	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if accessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		t.Fatalf("loading configuration for %s: %v", endpoint, err)
	}

	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		// Path style, not virtual host. A bucket-as-subdomain request against 127.0.0.1 does not resolve,
		// and the failure is a DNS error that says nothing about the endpoint.
		o.UsePathStyle = true
	})

	bucket := compatBucketName(t)

	createInput := &awss3.CreateBucketInput{Bucket: aws.String(bucket)}
	// Every region but us-east-1 requires the constraint, and us-east-1 rejects it. Harmless against
	// MinIO and RGW, which take a region-scoped create either way.
	if region != "us-east-1" {
		createInput.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}

	if _, err := client.CreateBucket(ctx, createInput); err != nil {
		t.Fatalf("creating bucket %q on %s: %v", bucket, endpoint, err)
	}
	t.Cleanup(func() { compatRemoveBucket(t, ctx, client, bucket) })

	backendCfg := s3.NewDefaultConfig()
	backendCfg.Region = region
	backendCfg.Endpoint = endpoint
	backendCfg.ForcePathStyle = true
	backendCfg.AccessKeyID = accessKey
	backendCfg.SecretAccessKey = secretKey
	backendCfg.Compression.Enabled = false

	backend, err := s3.NewBackend(ctx, bucket, backendCfg)
	if err != nil {
		t.Fatalf("building a backend against %s: %v", endpoint, err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("closing the backend: %v", err)
		}
	})

	return client, backend, bucket
}

func compatBucketName(t *testing.T) string {
	t.Helper()

	name := "objectfs-compat-" + strings.ToLower(strings.NewReplacer("_", "-", "/", "-").Replace(t.Name()))

	// S3 bucket naming, which MinIO and RGW both enforce: 63 characters, lowercase, no trailing dash.
	suffix := fmt.Sprintf("-%09d", time.Now().UnixNano()%1e9)
	if maxLen := 63 - len(suffix); len(name) > maxLen {
		name = name[:maxLen]
	}

	return strings.TrimRight(name, "-") + suffix
}

func compatRemoveBucket(t *testing.T, ctx context.Context, client *awss3.Client, bucket string) {
	t.Helper()

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	// Incomplete uploads first: a bucket holding one cannot be deleted, and this suite deliberately
	// creates uploads that may not complete.
	if ups, err := client.ListMultipartUploads(cleanupCtx,
		&awss3.ListMultipartUploadsInput{Bucket: aws.String(bucket)}); err == nil {
		for _, u := range ups.Uploads {
			_, _ = client.AbortMultipartUpload(cleanupCtx, &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(bucket),
				Key:      u.Key,
				UploadId: u.UploadId,
			})
		}
	}

	pages := awss3.NewListObjectsV2Paginator(client, &awss3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(cleanupCtx)
		if err != nil {
			t.Errorf("listing %q for cleanup: %v; the bucket may need removing by hand", bucket, err)

			return
		}

		for _, obj := range page.Contents {
			if _, err := client.DeleteObject(cleanupCtx,
				&awss3.DeleteObjectInput{Bucket: aws.String(bucket), Key: obj.Key}); err != nil {
				t.Errorf("deleting %q: %v", aws.ToString(obj.Key), err)
			}
		}
	}

	if _, err := client.DeleteBucket(cleanupCtx, &awss3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Errorf("deleting bucket %q: %v; it may need removing by hand", bucket, err)
	}
}

// compatCode renders an error the way the matrix records it: HTTP status and S3 code, never message
// text. Matching on message text is audit finding L27, and the codes are what translateConditionalError
// dispatches on — so a matrix row naming a code is a row a mapping can be checked against.
func compatCode(err error) string {
	if err == nil {
		return "success"
	}

	var (
		apiErr  smithy.APIError
		respErr *smithyhttp.ResponseError
	)

	status := 0
	if stderrors.As(err, &respErr) {
		status = respErr.HTTPStatusCode()
	}

	if stderrors.As(err, &apiErr) {
		return fmt.Sprintf("%d %s", status, apiErr.ErrorCode())
	}

	return fmt.Sprintf("%d <no S3 code> %v", status, err)
}

func compatGet(t *testing.T, ctx context.Context, client *awss3.Client, bucket, key string) []byte {
	t.Helper()

	out, err := client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject(%q): %v", key, err)
	}
	defer func() { _ = out.Body.Close() }()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(out.Body); err != nil {
		t.Fatalf("reading %q: %v", key, err)
	}

	return buf.Bytes()
}

// TestCompatCapabilityProbeMatchesObservedBehavior is the acceptance test #285 calls the one that
// matters most: every downstream feature trusts the probe, so the probe being right about this endpoint
// is the claim everything else rests on.
//
// It establishes the endpoint's real behavior with raw SDK calls and then asserts the probe agrees. The
// asymmetry is deliberate and is the whole point: a probe reporting an endpoint *less* capable than it
// is costs performance, since PutObjectIf refuses and a caller falls back or declines to start. A probe
// reporting it *more* capable than it is costs correctness, silently, at the moment of a race. So the
// second is a failure and the first is only logged.
//
// This is the assertion that caught the finding. RGW answers 412 for an If-Match against an absent key,
// the probe read any 412 as "the header was evaluated", and RGW then ignores preconditions on
// CompleteMultipartUpload — so the probe called capable an endpoint on which a large conditional write
// is unconditional.
func TestCompatCapabilityProbeMatchesObservedBehavior(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client, backend, bucket := compatClients(t, ctx)

	// Observed behavior, by attempt. Two writers assert absence of the same key in sequence; an endpoint
	// that evaluates the precondition declines the second.
	const key = "compat/probe/observed"

	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader([]byte("first")),
		IfNoneMatch: aws.String("*"),
	}); err != nil {
		t.Fatalf("If-None-Match: * against an absent key: %s", compatCode(err))
	}

	_, secondErr := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader([]byte("second")),
		IfNoneMatch: aws.String("*"),
	})
	enforcesAbsence := secondErr != nil
	t.Logf("If-None-Match: * over an existing key -> %s", compatCode(secondErr))

	// Recorded, not failed. An endpoint that ignores If-None-Match entirely is a matrix row, and the
	// probe below is what decides whether it is a problem: this is the fail-closed direction, so
	// PutObjectIf returns ErrNotSupported and nothing races. The check that matters is the probe
	// agreeing, which the switch at the end of this test makes.
	//
	// This was a t.Errorf until Wasabi was probed, and the message it printed was the finding rather
	// than a defect — the suite's contract is to fail only on what would be unsafe. It had never fired
	// because AWS, MinIO, RGW and RustFS all enforce absence on PutObject; Wasabi is the first endpoint
	// to reach it, and it made the run red for behavior the probe had already correctly refused.
	if !enforcesAbsence {
		if got := compatGet(t, ctx, client, bucket, key); !bytes.Equal(got, []byte("first")) {
			t.Logf("FINDING: the endpoint accepted If-None-Match: * over an existing key and replaced "+
				"its contents with %q. Two writers asserting absence would both believe they won, so "+
				"conditional writes must be reported unsupported — asserted below", got)
		}
	}

	// If-Match against a key that does not exist. The answer is the distinction the whole CAS series is
	// built on: 404 means "the object is gone, stop retrying", 412 means "you lost a race, re-read and
	// try again", and an endpoint that says 412 for absence leaves a caller unable to tell them apart.
	_, absentErr := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:  aws.String(bucket),
		Key:     aws.String("compat/probe/never-written"),
		Body:    bytes.NewReader([]byte("x")),
		IfMatch: aws.String(`"0123456789abcdef0123456789abcdef"`),
	})
	t.Logf("If-Match against an absent key -> %s", compatCode(absentErr))

	distinguishesAbsence := isCompatCode(absentErr, "NoSuchKey", "NotFound")

	// Preconditions on CompleteMultipartUpload, which is where a conditional write above
	// MultipartThreshold has to be evaluated: parts are not an object until they are assembled. An
	// endpoint that honors the header on PutObject and drops it here makes exactly the large writes
	// unconditional, which is invisible until two nodes write a big object at once.
	enforcesMultipart := compatMultipartPreconditionEnforced(t, ctx, client, bucket)
	t.Logf("precondition on CompleteMultipartUpload enforced -> %v", enforcesMultipart)

	// And what the probe says.
	caps := backend.Capabilities()
	t.Logf("#282 capability probe -> ConditionalWrite=%v detail=%q", caps.ConditionalWrite,
		caps.ConditionalWriteDetail)

	safe := enforcesAbsence && distinguishesAbsence && enforcesMultipart

	switch {
	case caps.ConditionalWrite && !safe:
		t.Errorf("the capability probe reports conditional writes supported, but this endpoint "+
			"enforces-absence=%v distinguishes-absent-from-stale=%v enforces-multipart-precondition=%v.\n"+
			"Every coordination feature trusts this probe, so reporting a partial implementation as a "+
			"complete one is how two nodes come to believe they each hold the same lease",
			enforcesAbsence, distinguishesAbsence, enforcesMultipart)

	case !caps.ConditionalWrite && safe:
		t.Errorf("the capability probe reports conditional writes unsupported (%q), but this endpoint "+
			"enforces every form probed. That is the fail-closed direction so nothing is unsafe, but "+
			"PutObjectIf refuses here and every coordination feature declines to start",
			caps.ConditionalWriteDetail)

	default:
		t.Logf("the probe agrees with observed behavior")
	}

	// The probe left nothing behind. It runs on the construction path of every mount, so an object at
	// this key would appear in every user's bucket — and it is the object whose absence the probe
	// asserts, so a second mount would read a different answer than the first.
	if _, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(s3.CapabilityProbeKeyForTest),
	}); err == nil {
		t.Errorf("the capability probe left an object at %q", s3.CapabilityProbeKeyForTest)
	}
}

// isCompatCode reports whether err carries one of the named S3 error codes.
func isCompatCode(err error, codes ...string) bool {
	if err == nil {
		return false
	}

	var apiErr smithy.APIError
	if !stderrors.As(err, &apiErr) {
		return false
	}

	return slices.Contains(codes, apiErr.ErrorCode())
}

// compatMultipartPreconditionEnforced reports whether the endpoint evaluates If-None-Match: * on
// CompleteMultipartUpload, established by completing an upload over a key that already exists.
//
// The stored bytes decide it, not the returned error. An endpoint could plausibly report a failure and
// have written anyway, and the direction that matters is whether the holder's object survived: that is
// what a lease is.
//
// One-byte parts, which is legal because a single-part upload's only part is also its highest-numbered
// one and S3 exempts that one from the 5 MiB minimum. This suite is run against local containers where
// transferring 5 MiB per case is real time for no added coverage.
func compatMultipartPreconditionEnforced(t *testing.T, ctx context.Context, client *awss3.Client,
	bucket string,
) bool {
	t.Helper()

	const key = "compat/probe/multipart-target"

	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte("holder")),
	}); err != nil {
		t.Fatalf("seeding the multipart target: %s", compatCode(err))
	}

	created, err := client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %s", compatCode(err))
	}

	part, err := client.UploadPart(ctx, &awss3.UploadPartInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(key),
		UploadId:   created.UploadId,
		PartNumber: aws.Int32(1),
		Body:       bytes.NewReader([]byte("contender")),
	})
	if err != nil {
		t.Fatalf("UploadPart: %s", compatCode(err))
	}

	_, completeErr := client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		UploadId: created.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: []s3types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}},
		},
		IfNoneMatch: aws.String("*"),
	})
	t.Logf("CompleteMultipartUpload with If-None-Match: * over an existing key -> %s",
		compatCode(completeErr))

	if completeErr != nil {
		// The upload was declined, so it is still open and this suite's cleanup would otherwise leave it.
		_, _ = client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
			Bucket:   aws.String(bucket),
			Key:      aws.String(key),
			UploadId: created.UploadId,
		})
	}

	stored := compatGet(t, ctx, client, bucket, key)
	if !bytes.Equal(stored, []byte("holder")) {
		t.Logf("the holder's object was replaced with %q: this endpoint does not evaluate preconditions "+
			"on CompleteMultipartUpload, so a conditional write above MultipartThreshold is unconditional",
			stored)

		return false
	}

	return true
}

// TestCompatIfMatchAcceptsTheETagTheStoreItselfReturned is a compare-and-swap loop's first step, and
// the one an endpoint can fail while enforcing everything else.
//
// The ETag from the write is sent back verbatim, with no HeadObject in between and no reformatting. That
// is how every CAS caller in this codebase does it — PutObjectIf returns the stored object's ETag
// precisely so a loop can continue from it without a second round trip — so an endpoint that rejects its
// own ETag cannot perform compare-and-swap at all, however correct its other answers are.
//
// Ceph RGW 19.2.0 fails exactly here: it returns a quoted ETag from PutObject and answers 412 when that
// same quoted value comes back as If-Match, accepting only the unquoted digest. AWS S3 and MinIO accept
// both. This is why the matrix records CAS as unavailable on RGW rather than merely partial.
func TestCompatIfMatchAcceptsTheETagTheStoreItselfReturned(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client, _, bucket := compatClients(t, ctx)

	const key = "compat/ifmatch/roundtrip"

	first, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte("v1")),
	})
	if err != nil {
		t.Fatalf("seeding: %s", compatCode(err))
	}

	returned := aws.ToString(first.ETag)
	t.Logf("PutObject returned ETag %s", returned)

	_, verbatimErr := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:  aws.String(bucket),
		Key:     aws.String(key),
		Body:    bytes.NewReader([]byte("v2")),
		IfMatch: first.ETag,
	})
	t.Logf("If-Match with that exact value -> %s", compatCode(verbatimErr))

	if verbatimErr == nil {
		// The happy path, and the one AWS and MinIO take. A CAS loop works here.
		if got := compatGet(t, ctx, client, bucket, key); !bytes.Equal(got, []byte("v2")) {
			t.Errorf("the conditional write reported success but the object holds %q, want %q", got, "v2")
		}

		return
	}

	// It was rejected. Whether the unquoted digest is accepted decides how the matrix describes the
	// endpoint — a quoting quirk a caller could work around, or no If-Match support at all — so it is
	// worth establishing rather than leaving as "rejected".
	bare := strings.Trim(returned, `"`)
	_, bareErr := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:  aws.String(bucket),
		Key:     aws.String(key),
		Body:    bytes.NewReader([]byte("v2-bare")),
		IfMatch: aws.String(bare),
	})
	t.Logf("If-Match with the unquoted digest %s -> %s", bare, compatCode(bareErr))

	// Not a t.Errorf. An endpoint that cannot do compare-and-swap is a fact about the endpoint, and the
	// safety of that is decided by whether the capability probe reports it — which
	// TestCompatCapabilityProbeMatchesObservedBehavior asserts. What this records is *why* the row in the
	// matrix says what it says, which reading the RGW source would not have told us.
	if bareErr == nil {
		t.Logf("FINDING: this endpoint rejects the quoted ETag it returned and accepts only the unquoted "+
			"digest. Compare-and-swap through PutObjectIf cannot work here: it passes Precondition.ETag "+
			"through verbatim, which is the value the store gave it. Matrix row: If-Match present but "+
			"unusable as returned (quoted %s rejected, bare %s accepted)", returned, bare)
	} else {
		t.Logf("FINDING: this endpoint rejects its own ETag in both quoted and unquoted form, so If-Match " +
			"cannot be satisfied at all and compare-and-swap is unavailable")
	}
}

// TestCompatConcurrentAbsentWritersElectExactlyOne is the property everything else reduces to,
// arbitrated by the endpoint rather than by any contender.
//
// Skipped rather than failed when the endpoint does not evaluate preconditions: PutObjectIf refuses
// there, so there is no race to arbitrate and the safety of that state is asserted by
// TestCompatCapabilityProbeMatchesObservedBehavior. What this covers is the endpoint that *claims* the
// capability — where exactly-one is the promise, and a second winner voids every guarantee built on it.
func TestCompatConcurrentAbsentWritersElectExactlyOne(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client, backend, bucket := compatClients(t, ctx)

	// Warmed before the gate opens. Capabilities probes once through a sync.Once, so letting eight
	// goroutines arrive unprobed would stagger seven of them behind the probe's round trip — measuring
	// the Once rather than the endpoint.
	caps := backend.Capabilities()
	if !caps.ConditionalWrite {
		t.Skipf("this endpoint does not evaluate write preconditions, so PutObjectIf refuses and there is "+
			"no race to arbitrate: %s", caps.ConditionalWriteDetail)
	}

	const (
		key        = "compat/race/lease"
		contenders = 8
	)

	var (
		start   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		winners []string
		losers  int
		other   []error
	)

	start.Add(1)
	for i := range contenders {
		done.Go(func() {
			// Every goroutine blocks here so the requests go out together. Staggered writes would let each
			// contender observe the previous winner's object and be declined for a reason that has nothing
			// to do with the endpoint arbitrating a race.
			start.Wait()

			value := fmt.Sprintf("node-%d", i)
			_, err := backend.PutObjectIf(ctx, key, []byte(value), nil, types.Precondition{Absent: true})

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err == nil:
				winners = append(winners, value)
			case stderrors.Is(err, types.ErrPreconditionFailed):
				losers++
			default:
				other = append(other, err)
			}
		})
	}

	start.Done()
	done.Wait()

	if len(other) > 0 {
		t.Fatalf("%d contender(s) failed for reasons other than losing the race: %v", len(other), other)
	}
	if len(winners) != 1 {
		t.Fatalf("%d contenders produced %d winners (%v), want exactly 1. The store — not any of them — "+
			"decides which proceeds, and everything downstream depends on it deciding once",
			contenders, len(winners), winners)
	}
	if losers != contenders-1 {
		t.Errorf("losers = %d, want %d", losers, contenders-1)
	}

	// The winner's bytes are the ones stored. A run where one write reported success while a different
	// contender's value landed would satisfy every count above.
	if got := compatGet(t, ctx, client, bucket, key); string(got) != winners[0] {
		t.Errorf("the stored object is %q but %q reported winning", got, winners[0])
	}
}

// TestCompatConditionalDeleteIsNotRelicdUpon records a behavior this codebase does not depend on, and
// says so in the one place someone would look before starting to depend on it.
//
// AWS documents If-Match on DeleteObject and answers 412 when it does not hold. MinIO accepts the header
// and deletes anyway — verified, not read: an object with a known ETag was deleted by a request carrying
// a deliberately wrong one. That would be a live defect if anything here issued a conditional delete,
// and nothing does: grep for IfMatch outside this package finds only PutObject and
// CompleteMultipartUpload.
//
// So this test asserts nothing about the endpoint. It exists because the failure mode is invisible — a
// conditional delete that silently ignores its condition looks exactly like one that honored it — and a
// future lease implementation that releases by conditional delete would be unsafe on MinIO for reasons
// no test would have reported. The recorded output is the warning.
func TestCompatConditionalDeleteIsNotReliedUpon(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client, _, bucket := compatClients(t, ctx)

	const key = "compat/delete/conditional"

	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte("keep-me")),
	}); err != nil {
		t.Fatalf("seeding: %s", compatCode(err))
	}

	_, delErr := client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket:  aws.String(bucket),
		Key:     aws.String(key),
		IfMatch: aws.String(`"deadbeefdeadbeefdeadbeefdeadbeef"`),
	})
	t.Logf("DeleteObject with a stale If-Match -> %s", compatCode(delErr))

	_, headErr := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})

	switch {
	case headErr == nil && delErr != nil:
		t.Logf("the precondition was honored: the delete was declined and the object survives")

	case headErr != nil && delErr == nil:
		t.Logf("FINDING: this endpoint accepted If-Match on DeleteObject and deleted the object anyway. " +
			"Nothing in ObjectFS issues a conditional delete, so this is not a live defect — but a lease " +
			"released by conditional delete would be unsafe here, and silently: the request succeeds")

	case headErr != nil && delErr != nil:
		t.Errorf("the delete reported %s and the object is gone anyway, which is the worst of both: a "+
			"caller told its delete was declined would leave the lease it thinks it still holds",
			compatCode(delErr))

	default:
		t.Logf("the delete succeeded and the object survives; this endpoint's DeleteObject is not "+
			"synchronous, which is outside what this suite can characterize (delete: %s)", compatCode(delErr))
	}
}
