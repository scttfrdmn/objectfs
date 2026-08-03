//go:build integration

package s3_test

// The multipart copy path against real AWS S3.
//
// [Backend.CopyObject] routes objects above S3's 5 GiB single-part CopyObject limit through
// UploadPartCopy. That branch has no hermetic test: the substrate emulator dispatches on
// x-amz-copy-source before it checks uploadId, so it handles an UploadPartCopy as a whole-object copy
// and returns 200 (scttfrdmn/substrate#532). The seam tests therefore skip, which is honest but leaves
// the branch where a mistake is most expensive — abandoned parts are billed and invisible to
// ListObjects — resting on nothing but reading.
//
// This is what covers it. Run with:
//
//	AWS_PROFILE=aws AWS_REGION=us-west-2 go test -race -tags=integration ./internal/storage/s3/
//
// It works at real part sizes, so it exercises S3's actual rules rather than scaled-down ones. Two
// shapes, both deliberate: 12 MiB in 5 MiB parts is a legal three-part copy with a short final part,
// which is the case that catches inclusive/exclusive range mistakes; 12 MiB in 4 MiB parts is an
// *illegal* one, since only the highest-numbered part may fall below 5 MiB, and S3 enforces that at
// CompleteMultipartUpload — which is how the abort path gets a failure that happens after every part has
// already been uploaded. The scaled-down hermetic test cannot reach either rule.
//
// Cost is a few cents of PUT/COPY requests and a few MiB of storage held for the length of the run;
// the bucket is created and removed by the test.

import (
	"bytes"
	"context"
	"crypto/sha256"
	stderrors "errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
)

// liveRegion is where the test bucket is created. us-west-2 because that is the region CLAUDE.md names
// for integration work, and a bucket in the wrong region turns every subsequent call into a redirect.
const liveRegion = "us-west-2"

// TestLiveMultipartCopyPreservesEverything is the real-S3 counterpart to
// TestCopyObjectAboveTheSinglePartLimitUsesAMultipartCopy.
//
// What it establishes that the hermetic test cannot:
//
//   - UploadPartCopy is genuinely a *ranged* copy. The emulator's failure mode is to return a
//     plausible CopyPartResult for a whole-object copy, so distinct per-part ETags over disjoint
//     ranges is the assertion that separates the two.
//   - The assembled object is byte-identical to the source, checked by SHA-256 rather than by length.
//   - Every property survives, on the path where none of them is inherited: a multipart upload's
//     metadata is fixed at CreateMultipartUpload, so Content-Encoding, Content-Type, storage class,
//     and the POSIX attributes in user metadata are all restated by hand or lost.
//   - A short final part is legal and correct. S3 exempts only the highest-numbered part from the
//     5 MiB minimum, which is the constraint a scaled-down test violates.
func TestLiveMultipartCopyPreservesEverything(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client, bucket := liveBucket(t, ctx)

	// 12 MiB in 5 MiB parts: three parts, the last 2 MiB. The source key carries a "+" deliberately —
	// this is the path where the escaping bug lived, and CopySource is built once here and reused for
	// every part, so getting it wrong fails every part rather than one.
	const (
		size            = 12 << 20
		singlePartLimit = 8 << 20
		partSize        = 5 << 20
	)

	const src = "live/mpcopy/source+plus.bin"
	const dst = "live/mpcopy/destination+plus.bin"

	want := deterministic(size)
	wantSum := sha256.Sum256(want)

	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:          aws.String(bucket),
		Key:             aws.String(src),
		Body:            bytes.NewReader(want),
		Metadata:        map[string]string{"objectfs-mode": "384", "objectfs-uid": "1234"},
		ContentEncoding: aws.String("zstd"),
		ContentType:     aws.String("application/x-tar"),
		StorageClass:    s3types.StorageClassStandardIa,
	}); err != nil {
		t.Fatalf("seeding the source object: %v", err)
	}

	backend := liveBackend(t, ctx, bucket)
	s3.SetCopyThresholdsForTest(backend, singlePartLimit, partSize)

	if err := backend.CopyObject(ctx, src, dst); err != nil {
		t.Fatalf("CopyObject of a %d-byte object with a %d-byte single-part limit: %v",
			size, singlePartLimit, err)
	}

	// The bytes, by hash. A length check would pass on a copy that assembled the same parts in the
	// wrong order, which is exactly what a PartNumber off by one produces.
	got := getAll(t, ctx, client, bucket, dst)
	if gotSum := sha256.Sum256(got); gotSum != wantSum {
		t.Errorf("the assembled copy holds %d bytes with sha256 %x, want %d bytes with %x",
			len(got), gotSum[:8], len(want), wantSum[:8])
	}

	// The source survives; rename's delete step depends on it.
	if src := getAll(t, ctx, client, bucket, src); !bytes.Equal(src, want) {
		t.Errorf("the source changed: %d bytes, want %d", len(src), len(want))
	}

	head, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(dst),
	})
	if err != nil {
		t.Fatalf("HeadObject on the assembled copy: %v", err)
	}

	// The multipart path really ran, established from the object itself rather than from a request count
	// that real S3 does not offer: an object assembled from N parts has an ETag of the form "<hex>-N",
	// where a single-part PUT or CopyObject produces a bare "<hex>".
	//
	// Without this the test passes identically when the >5 GiB routing is deleted — verified by deleting
	// it — because a single-part CopyObject of a 12 MiB object preserves everything asserted below via
	// MetadataDirective=COPY. So every other assertion here would hold while the branch under test never
	// executed.
	if etag := aws.ToString(head.ETag); !strings.HasSuffix(strings.Trim(etag, `"`), "-3") {
		t.Errorf("the copy's ETag is %s, which is not the \"<hex>-3\" form of an object assembled from "+
			"three parts. The copy took the single-part path, so this test proves nothing about the "+
			"multipart branch", etag)
	}

	if enc := aws.ToString(head.ContentEncoding); enc != "zstd" {
		t.Errorf("the copy's Content-Encoding = %q, want %q. The read path dispatches decoding on this "+
			"header and fails closed, so losing it leaves the object permanently unreadable with its "+
			"bytes intact", enc, "zstd")
	}

	if ct := aws.ToString(head.ContentType); ct != "application/x-tar" {
		t.Errorf("the copy's Content-Type = %q, want %q", ct, "application/x-tar")
	}

	if head.StorageClass != s3types.StorageClassStandardIa {
		t.Errorf("the copy's storage class = %q, want %q. It defaults to STANDARD, so omitting it "+
			"silently promotes the object out of the tier the user is paying for — audit finding L26",
			head.StorageClass, s3types.StorageClassStandardIa)
	}

	for k, want := range map[string]string{"objectfs-mode": "384", "objectfs-uid": "1234"} {
		var got string
		for hk, hv := range head.Metadata {
			if strings.EqualFold(hk, k) {
				got = hv
			}
		}
		if got != want {
			t.Errorf("the copy's %s = %q, want %q. POSIX mode and ownership live in user metadata and "+
				"nowhere else, and a multipart upload's metadata is fixed at CreateMultipartUpload — so "+
				"this path has to restate it explicitly and is where losing it is easiest", k, got, want)
		}
	}

	if ids := openUploads(t, ctx, client, bucket); len(ids) != 0 {
		t.Errorf("after a successful multipart copy %d multipart upload(s) are still open: %v. Their "+
			"parts are billed and no object listing shows them", len(ids), ids)
	}
}

// TestLiveMultipartCopyLeavesNoOrphanedUploadWhenCompleteFails is the leak half, against real S3, on
// the specific failure H10 was.
//
// H10 was a multipart path that aborted on one failure and not on another, and the one it missed was
// the Complete-failure path — the one on which every part has already been uploaded, so it leaks the
// most. The parts of an abandoned upload are billed and absent from ListObjects, so that leak is
// invisible to the operator paying for it. ListMultipartUploads, not the returned error, is therefore
// the assertion.
//
// Reaching that exact path needs a failure that happens *at* Complete rather than before it, and S3
// supplies one: every part but the highest-numbered must be at least 5 MiB, and the check runs at
// Complete. Verified against real S3 — three 4 MiB part copies all succeed, Complete answers 400
// EntityTooSmall, and the upload is still listed afterwards:
//
//	part 1 bytes=0-4194303        ok
//	part 2 bytes=4194304-8388607  ok
//	part 3 bytes=8388608-12582911 ok
//	Complete → api error EntityTooSmall: Your proposed upload is smaller than the minimum allowed size
//	open uploads after the failed Complete: 1
//
// A test that instead made the *part copies* fail — by deleting the source, say — would pass without
// exercising the abort at all, because CopyObject heads the source first and returns before creating an
// upload. There is nothing to leak, so such a test asserts nothing. That is worth stating because it is
// the version this test was first written as.
//
// Choosing a sub-minimum part size deliberately is also the only way to reach S3's rule from here at a
// testable size, so this covers the constraint the scaled-down hermetic test explicitly cannot.
func TestLiveMultipartCopyLeavesNoOrphanedUploadWhenCompleteFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client, bucket := liveBucket(t, ctx)

	const src = "live/abort/source.bin"
	const dst = "live/abort/destination.bin"

	body := deterministic(12 << 20)
	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(src),
		Body:   bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("seeding the source object: %v", err)
	}

	backend := liveBackend(t, ctx, bucket)

	// 4 MiB parts over 12 MiB: three parts, and the first two are below S3's 5 MiB non-final-part
	// minimum. Every UploadPartCopy succeeds and Complete rejects the set.
	s3.SetCopyThresholdsForTest(backend, 8<<20, 4<<20)

	err := backend.CopyObject(ctx, src, dst)
	if err == nil {
		t.Fatal("CopyObject with sub-minimum part sizes returned no error; S3 rejects such an upload at " +
			"CompleteMultipartUpload with EntityTooSmall, so this test is no longer reaching the path it " +
			"exists to cover")
	}

	t.Logf("CopyObject failed at Complete as expected: %v", err)

	// Checked on the unwrapped chain, not on err.Error(): ObjectFSError.Error renders only its own
	// component, code, and message, and reaches the S3 API error through Unwrap. Matching the rendered
	// string would silently never match.
	//
	// This is how the test knows it is still covering the path it was written for. The abort it asserts
	// only means something if the failure happened at Complete, with every part already uploaded — an
	// earlier failure would leave no upload to leak, and the ListMultipartUploads check below would pass
	// while proving nothing.
	var apiErr interface{ ErrorCode() string }
	switch {
	case !stderrors.As(err, &apiErr):
		t.Errorf("the failure carried no S3 API error code, so the test cannot tell where it happened: %v",
			err)
	case apiErr.ErrorCode() != "EntityTooSmall":
		t.Errorf("the failure was S3 %s, want EntityTooSmall. EntityTooSmall is the one that arrives at "+
			"CompleteMultipartUpload, after every part is uploaded, which is the path H10 left unaborted: %v",
			apiErr.ErrorCode(), err)
	}

	if ids := openUploads(t, ctx, client, bucket); len(ids) != 0 {
		t.Errorf("after a failed multipart copy %d multipart upload(s) are still open: %v. The deferred "+
			"abort did not run, so their parts are billed and no object listing shows them", len(ids), ids)
	}

	if _, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(dst),
	}); err == nil {
		t.Errorf("the destination %q exists after a failed copy; a partial object there is worse than "+
			"none, since a retry cannot distinguish it from a complete one", dst)
	}

	// And the source is untouched. A failed rename must leave the file where it was.
	if got := getAll(t, ctx, client, bucket, src); !bytes.Equal(got, body) {
		t.Errorf("the source changed after a failed copy: %d bytes, want %d", len(got), len(body))
	}
}

// liveBucket creates a bucket for one test and removes it, and everything in it, when the test ends.
//
// Named per-test rather than shared so the tests can run in parallel and so a failure leaves exactly
// one bucket to inspect. The name embeds the test name and the account-scoped suffix S3 requires for
// global uniqueness.
func liveBucket(t *testing.T, ctx context.Context) (*awss3.Client, string) {
	t.Helper()

	if os.Getenv("AWS_PROFILE") == "" && os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Skip("no AWS credentials in the environment; run with AWS_PROFILE=aws AWS_REGION=us-west-2")
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(liveRegion))
	if err != nil {
		t.Fatalf("loading AWS configuration: %v", err)
	}

	client := awss3.NewFromConfig(cfg)

	// S3 caps a bucket name at 63 characters, and the suffix below adds ten, so the readable part gets
	// 53. Getting this wrong is an InvalidBucketName that says nothing about length.
	suffix := fmt.Sprintf("-%09d", time.Now().UnixNano()%1e9)

	name := "objectfs-live-" + strings.ToLower(strings.NewReplacer("_", "-", "/", "-").Replace(t.Name()))
	if max := 63 - len(suffix); len(name) > max {
		name = name[:max]
	}

	name = strings.TrimRight(name, "-") + suffix

	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{
		Bucket: aws.String(name),
		CreateBucketConfiguration: &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(liveRegion),
		},
	}); err != nil {
		t.Fatalf("creating bucket %q in %s: %v", name, liveRegion, err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()

		// Incomplete uploads first: a bucket holding one cannot be deleted, and leaving it would leak
		// exactly the billed-but-invisible storage these tests are about.
		for _, id := range openUploadsNoFail(cleanupCtx, client, name) {
			_, _ = client.AbortMultipartUpload(cleanupCtx, &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(name),
				Key:      aws.String(id.key),
				UploadId: aws.String(id.id),
			})
		}

		pages := awss3.NewListObjectsV2Paginator(client, &awss3.ListObjectsV2Input{
			Bucket: aws.String(name),
		})
		for pages.HasMorePages() {
			page, err := pages.NextPage(cleanupCtx)
			if err != nil {
				t.Errorf("listing %q for cleanup: %v; the bucket may need removing by hand", name, err)

				return
			}

			for _, obj := range page.Contents {
				if _, err := client.DeleteObject(cleanupCtx, &awss3.DeleteObjectInput{
					Bucket: aws.String(name),
					Key:    obj.Key,
				}); err != nil {
					t.Errorf("deleting %q: %v", aws.ToString(obj.Key), err)
				}
			}
		}

		if _, err := client.DeleteBucket(cleanupCtx, &awss3.DeleteBucketInput{
			Bucket: aws.String(name),
		}); err != nil {
			t.Errorf("deleting bucket %q: %v; it may need removing by hand", name, err)
		}
	})

	return client, name
}

// liveBackend builds an ObjectFS backend against a real bucket, with compression off so the bytes on
// the wire are the bytes under test.
func liveBackend(t *testing.T, ctx context.Context, bucket string) *s3.Backend {
	t.Helper()

	cfg := s3.NewDefaultConfig()
	cfg.Region = liveRegion
	cfg.Compression.Enabled = false

	backend, err := s3.NewBackend(ctx, bucket, cfg)
	if err != nil {
		t.Fatalf("building a backend for %q: %v", bucket, err)
	}

	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("closing the backend: %v", err)
		}
	})

	return backend
}

// deterministic returns n bytes that are the same on every run and not compressible into nothing —
// a constant fill would let a truncated part assemble into the right bytes by accident.
func deterministic(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*7 + i/251)
	}

	return out
}

func getAll(t *testing.T, ctx context.Context, client *awss3.Client, bucket, key string) []byte {
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

type liveUpload struct{ key, id string }

func openUploads(t *testing.T, ctx context.Context, client *awss3.Client, bucket string) []string {
	t.Helper()

	out, err := client.ListMultipartUploads(ctx, &awss3.ListMultipartUploadsInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}

	ids := make([]string, 0, len(out.Uploads))
	for _, u := range out.Uploads {
		ids = append(ids, fmt.Sprintf("%s (%s)", aws.ToString(u.Key), aws.ToString(u.UploadId)))
	}

	return ids
}

// openUploadsNoFail is the cleanup-path variant: a cleanup that called t.Fatalf would abandon the
// bucket it was in the middle of removing.
func openUploadsNoFail(ctx context.Context, client *awss3.Client, bucket string) []liveUpload {
	out, err := client.ListMultipartUploads(ctx, &awss3.ListMultipartUploadsInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return nil
	}

	ups := make([]liveUpload, 0, len(out.Uploads))
	for _, u := range out.Uploads {
		ups = append(ups, liveUpload{key: aws.ToString(u.Key), id: aws.ToString(u.UploadId)})
	}

	return ups
}
