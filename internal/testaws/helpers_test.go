package testaws_test

import (
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/objectfs/objectfs/internal/testaws"
)

// rangedGet builds a GetObjectInput carrying a Range header. It exists so the ranged-read tests
// state the range they mean and nothing else.
func rangedGet(bucket, key, byteRange string) *awss3.GetObjectInput {
	return &awss3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Range:  aws.String(byteRange),
	}
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	return data
}

// startMultipart begins a multipart upload and deliberately does not complete or abort it, so the
// caller can observe the orphan. The upload is aborted at test end so the emulator's state does not
// carry it into an unrelated assertion.
func startMultipart(t *testing.T, ts *testaws.TestServer, key string) string {
	t.Helper()

	client := ts.Client()

	out, err := client.CreateMultipartUpload(context.Background(), &awss3.CreateMultipartUploadInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("create multipart upload for %q: %v", key, err)
	}

	uploadID := aws.ToString(out.UploadId)

	t.Cleanup(func() {
		_, _ = client.AbortMultipartUpload(context.Background(), &awss3.AbortMultipartUploadInput{
			Bucket:   aws.String(ts.Bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
		})
	})

	return uploadID
}

// noRetry disables the SDK's own retrying for one operation.
//
// It exists because the raw client is not the naive caller it looks like. `newClient` leaves the
// SDK's default retryer in place — three attempts — so an injected fault armed for two fires is
// spent by attempts one and two and the operation *succeeds* on the third, with the caller seeing no
// error at all. That is the trap this whole mechanism exists to avoid, reappearing one layer up: a
// test asserting "the request failed" would pass or fail depending on how the fault budget compares
// against a retry budget nobody mentioned. Faults are counted at the proxy, so a test about the
// proxy has to make exactly one request per call.
func noRetry(o *awss3.Options) {
	o.Retryer = aws.NopRetryer{}
}
