package s3_test

// Audit finding H10: a multipart upload that failed at CompleteMultipartUpload left every one of its
// parts in the bucket, billed as storage, invisible to every other API, and with the only in-process
// record of the upload deleted on the way out.
//
// The asymmetry is what made it hard to see by reading. The part-upload failure path aborted, so the
// case that leaked *nothing much* was handled and the case that leaked the whole object was not:
// Complete is the last call, so by the time it fails every part has landed. A 12 MiB write that
// failed at Complete leaked 12 MiB.
//
// Every test here asserts through ListMultipartUploads, because that is the only API that can see an
// orphan — ListObjects does not show it and HeadObject reports the key absent — and because nothing in
// ObjectFS itself has ever called it, so a leak was undiscoverable from inside the filesystem.

import (
	"context"
	"errors"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// Multipart fixture sizes. S3 requires every non-final part to be at least 5 MB, so a genuine
// multi-part upload cannot be arranged below that.
const (
	multipartChunk  = 5 * 1024 * 1024
	multipartObject = 12 * 1024 * 1024
)

func multipartConfig(cfg *s3.Config) {
	cfg.MultipartThreshold = multipartChunk
	cfg.MultipartChunkSize = multipartChunk

	// Compression off: a compressible payload could shrink below the threshold and take the
	// single-PUT path, leaving these tests measuring nothing.
	cfg.Compression.Enabled = false
}

// TestMultipartUploadAbortsWhenCompleteFails is the H10 regression test.
//
// The fault targets CompleteMultipartUpload specifically, which is both the leak's location and the
// hardest case to reach any other way: every part has to have succeeded first.
func TestMultipartUploadAbortsWhenCompleteFails(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	backend := ts.Backend(multipartConfig)

	const key = "multipart/complete-fails"

	if got := ts.MultipartUploads(); len(got) != 0 {
		t.Fatalf("the bucket already has %d multipart uploads before the test wrote anything: %v",
			len(got), got)
	}

	// QueryKey is what aims this at Complete rather than at Create. Both are a POST to the same
	// path and differ only in the query string, so method and path alone hit the create — and a
	// failed create leaves no upload to orphan, which is a passing test that proves nothing. It
	// took a mutation that removed the abort outright, and did not fail this test, to expose that.
	//
	// 403 rather than 500: AccessDenied is not retryable, so the upload fails once and stays failed.
	// A retryable status would be retried by ObjectFS's own retryer and the fault's budget would
	// decide the outcome rather than the code under test.
	ts.InjectFault(testaws.Fault{
		Method:    "POST",
		KeySuffix: key,
		QueryKey:  "uploadId",
		Status:    403,
		Code:      "AccessDenied",
		Times:     1,
	})

	err := backend.PutObject(context.Background(), key,
		testaws.DeterministicBytes(key, multipartObject), nil)
	if err == nil {
		t.Fatal("PutObject succeeded with CompleteMultipartUpload failing; a write that could not " +
			"assemble its parts must report the failure")
	}

	// The fault has to have fired for this test to prove anything. A matcher aimed at the wrong
	// request produces a failed upload for a different reason and an identical passing test.
	if fired := ts.FaultsFired(); fired != 1 {
		t.Fatalf("the injected fault fired %d times, want 1 — it did not match "+
			"CompleteMultipartUpload, so this test says nothing about the Complete failure path",
			fired)
	}

	if orphans := ts.MultipartUploads(); len(orphans) != 0 {
		t.Errorf("a failed CompleteMultipartUpload left %d upload(s) in the bucket: %v\n"+
			"Every part had already been uploaded by the time Complete ran, so this is the whole "+
			"object's worth of storage, billed indefinitely, invisible to ListObjects and to "+
			"HeadObject, and reapable only by an S3 lifecycle rule the operator has to know to write.",
			len(orphans), orphans)
	}

	// The failed key must not exist either: an abort means the object was never assembled, and a
	// caller that saw an error must not find a partial object under the name it wrote.
	if ts.ObjectExists(key) {
		t.Errorf("the key exists after a failed multipart upload; PutObject reported an error, so " +
			"there must be no object under that name")
	}
}

// TestMultipartUploadAbortsWhenAPartFails covers the path that always aborted, so that the
// restructure into a single deferred abort cannot silently lose it. It is the same assertion against
// a different failure point, which is the property being protected: aborting is now the default for
// every exit that is not a completed upload, rather than something each error path remembers to do.
func TestMultipartUploadAbortsWhenAPartFails(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	backend := ts.Backend(multipartConfig)

	const key = "multipart/part-fails"

	// A PUT carrying partNumber is an UploadPart. Times is generous because the part upload is
	// retried, and the point here is a part that never succeeds — a single fire would be absorbed
	// by the retry and the upload would complete.
	ts.InjectFault(testaws.Fault{
		Method:    "PUT",
		KeySuffix: key,
		QueryKey:  "partNumber",
		Status:    403,
		Code:      "AccessDenied",
		Times:     100,
	})

	err := backend.PutObject(context.Background(), key,
		testaws.DeterministicBytes(key, multipartObject), nil)
	if err == nil {
		t.Fatal("PutObject succeeded with every part upload failing")
	}

	if fired := ts.FaultsFired(); fired == 0 {
		t.Fatal("the injected fault never fired, so no part upload failed and this test proved nothing")
	}

	if orphans := ts.MultipartUploads(); len(orphans) != 0 {
		t.Errorf("a failed part upload left %d upload(s) in the bucket: %v", len(orphans), orphans)
	}
}

// TestMultipartUploadAbortsAfterTheCallerCancels is the case the abort's detached context exists for,
// and it is the most common one in production: an unmount or a FUSE interrupt cancels the context
// mid-upload.
//
// An abort issued on the caller's canceled context is never sent, so cleanup would be skipped on
// exactly the failure that happens most — and the abandoned parts would be a *consequence of shutting
// down cleanly*. Nothing about the leak would appear in a test that only ever fails uploads by
// injecting errors.
func TestMultipartUploadAbortsAfterTheCallerCancels(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	backend := ts.Backend(multipartConfig)

	const key = "multipart/caller-cancels"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel from inside the request sequence rather than from a goroutine racing it: the hook runs
	// on the proxy's goroutine while the request that triggered it is held, so every request issued
	// after it is already running against a canceled context. A time.AfterFunc would make which
	// requests get canceled depend on scheduling.
	//
	// It has to fire on a part and not on the create, and that is the whole difficulty of writing
	// this test. Canceling during the create means the create fails, which means there is no
	// upload — and "no upload was orphaned" is then true for the uninteresting reason. Only after
	// the create has returned an upload ID is there anything for the abort to clean up, so
	// QueryKey pins the fault to an UploadPart.
	ts.InjectFault(testaws.Fault{
		Method:    "PUT",
		KeySuffix: key,
		QueryKey:  "partNumber",
		Status:    500,
		Times:     1,
		OnFire:    cancel,
	})

	err := backend.PutObject(ctx, key, testaws.DeterministicBytes(key, multipartObject), nil)
	if err == nil {
		t.Fatal("PutObject succeeded after its context was canceled")
	}
	if !errors.Is(err, context.Canceled) {
		// Not fatal: the upload may fail on the injected 500 before the cancellation is observed,
		// and either way an incomplete upload must be aborted. Worth reporting, because it means
		// the detached-context path is not what this run exercised.
		t.Logf("the upload failed with %v rather than a cancellation; the abort's detached context "+
			"may not have been exercised on this run", err)
	}

	if orphans := ts.MultipartUploads(); len(orphans) != 0 {
		t.Errorf("a canceled multipart upload left %d upload(s) in the bucket: %v\n"+
			"The abort has to run on a context of its own — issued on the caller's canceled "+
			"context it would never be sent, so an unmount during a large write would leak its "+
			"parts every time.", len(orphans), orphans)
	}
}

// TestMultipartUploadLeavesNoUploadOnSuccess is the control for the `completed` flag: a successful
// upload must issue no abort.
//
// What the flag is worth is narrower than it first appears, and the difference is worth recording
// because it is the difference between a data-integrity guard and a hygiene one. Mutating the flag
// away — aborting unconditionally, even after Complete returned — does *not* destroy the object.
// CompleteMultipartUpload consumes the upload ID, so the abort that follows it addresses an upload
// that no longer exists: S3 answers NoSuchUpload, the object is untouched, and the only visible
// consequences are one wasted request per multipart write and a warning log on every successful
// upload telling the operator their parts have leaked when they have not.
//
// So this asserts on the request, not on the object. The Operations count is the only thing that
// can see the difference, which is exactly why the assertion is phrased that way — the earlier
// version of this test checked that the bytes read back were intact, and that holds either way.
func TestMultipartUploadLeavesNoUploadOnSuccess(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	backend := ts.Backend(multipartConfig)

	const key = "multipart/succeeds"

	want := testaws.DeterministicBytes(key, multipartObject)

	if err := backend.PutObject(context.Background(), key, want, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if orphans := ts.MultipartUploads(); len(orphans) != 0 {
		t.Errorf("a successful multipart upload left %d upload(s) in the bucket: %v",
			len(orphans), orphans)
	}

	// The load-bearing assertion. An upload that completed has nothing to abort, and issuing one
	// anyway would log a leak warning on every successful large write — noise that trains an
	// operator to ignore the message that matters.
	if aborts := ts.Operations("AbortMultipartUpload"); aborts != 0 {
		t.Errorf("a successful multipart upload issued %d AbortMultipartUpload call(s), want 0; "+
			"Complete consumed the upload ID, so the abort is a wasted round trip and a warning "+
			"log claiming parts were leaked when none were", aborts)
	}

	// And the object is readable, which is the baseline this whole path exists to deliver.
	got, err := backend.GetObject(context.Background(), key, 0, int64(len(want)))
	if err != nil {
		t.Fatalf("GetObject after a successful multipart upload: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read back %d bytes, want %d", len(got), len(want))
	}
	if string(got) != string(want) {
		t.Error("the bytes read back differ from the bytes written")
	}
}
