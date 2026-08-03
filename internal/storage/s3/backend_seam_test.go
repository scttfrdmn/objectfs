package s3_test

// These tests exercise the real S3 backend against a real S3 endpoint over real HTTP. That is the
// whole point: the v0.10.0 audit found roughly forty-five defects that 32,680 lines of tests across
// 90 files had all missed, overwhelmingly because they were seam defects — a value produced
// correctly at one layer and dropped at the boundary. A mock on the far side of a seam is a
// restatement of what the caller believes, so it agrees with the caller by construction.
//
// The file lives in package s3_test rather than package s3 because the harness imports the backend,
// and an internal test would be an import cycle.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	objectfserrors "github.com/scttfrdmn/objectfs/pkg/errors"
)

// TestPooledOperationsReachTheConfiguredEndpoint is the regression test for #173, the defect that
// motivated the whole harness.
//
// Three of the four S3 clients ObjectFS built applied the configured endpoint; the connection pool's
// factory applied nothing. HeadObject, DeleteObject, ListObjects, and the health check all draw from
// the pool, so those four addressed real AWS while PutObject and GetObject addressed the configured
// endpoint — a MinIO, Ceph, or emulator deployment failed in a way that looked like a credentials
// problem. Every unit test passed.
//
// Asserting on the recorded requests rather than on the returned values is what makes this a real
// test: before the fix, these operations did not merely return wrong answers, they left no trace on
// the endpoint at all.
//
//nolint:paralleltest,tparallel // ordered subtests over a shared request recorder; see below
func TestPooledOperationsReachTheConfiguredEndpoint(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	const key = "pooled/probe"

	ts.PutObject(key, []byte("payload"))
	ts.ResetRequests()

	// The subtests deliberately do not call t.Parallel, and the sequence is load-bearing twice over:
	// each one calls ResetRequests on a recorder they all share, so a concurrent sibling's traffic
	// would land in the window being asserted; and DeleteObject removes the key the earlier three
	// need. Parallelizing these would not be a speedup, it would be a different test.
	t.Run("HeadObject", func(t *testing.T) {
		if _, err := backend.HeadObject(ctx, key); err != nil {
			t.Fatalf("HeadObject: %v", err)
		}
		assertEndpointSaw(t, ts, http.MethodHead, key)
	})

	t.Run("ListObjects", func(t *testing.T) {
		ts.ResetRequests()

		got, err := backend.ListObjects(ctx, "pooled/", 100)
		if err != nil {
			t.Fatalf("ListObjects: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("ListObjects returned %d objects, want 1", len(got))
		}
		assertEndpointSawMethod(t, ts, http.MethodGet)
	})

	t.Run("HealthCheck", func(t *testing.T) {
		ts.ResetRequests()

		if err := backend.HealthCheck(ctx); err != nil {
			t.Fatalf("HealthCheck: %v", err)
		}
		assertEndpointSawMethod(t, ts, http.MethodHead)
	})

	t.Run("DeleteObject", func(t *testing.T) {
		ts.ResetRequests()

		if err := backend.DeleteObject(ctx, key); err != nil {
			t.Fatalf("DeleteObject: %v", err)
		}
		assertEndpointSaw(t, ts, http.MethodDelete, key)

		if ts.ObjectExists(key) {
			t.Error("the object survived DeleteObject; the delete went somewhere else")
		}
	})
}

// assertEndpointSaw fails unless the configured endpoint observed a request of this method for this
// key. It is the assertion that distinguishes "returned a plausible answer" from "actually talked to
// the endpoint it was configured with".
func assertEndpointSaw(t *testing.T, ts *testaws.TestServer, method, key string) {
	t.Helper()

	for _, r := range ts.RequestsFor(key) {
		if r.Method == method {
			return
		}
	}

	t.Errorf("the configured endpoint never saw a %s for %q — the operation addressed a different "+
		"endpoint. Observed: %s", method, key, describe(ts.Requests()))
}

func assertEndpointSawMethod(t *testing.T, ts *testaws.TestServer, method string) {
	t.Helper()

	for _, r := range ts.Requests() {
		if r.Method == method {
			return
		}
	}

	t.Errorf("the configured endpoint never saw a %s — the operation addressed a different "+
		"endpoint. Observed: %s", method, describe(ts.Requests()))
}

func describe(requests []testaws.Request) string {
	if len(requests) == 0 {
		return "no requests at all"
	}

	var b strings.Builder
	for i, r := range requests {
		if i > 0 {
			b.WriteString(", ")
		}

		b.WriteString(r.Method)
		b.WriteString(" ")
		b.WriteString(r.Path)
	}

	return b.String()
}

// TestBackendRoundTripsBytesUnchanged pins the most basic integrity property there is, through the
// real backend and a real endpoint: what PutObject stored is what a client reading independently
// finds, and what GetObject returns is what an independent client stored.
//
// Reading back through the same layer that wrote cannot detect a symmetric encoding bug, so each
// direction is verified against the raw SDK.
func TestBackendRoundTripsBytesUnchanged(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	// Compression stays off here: this test is about the transport, and a codec in the path would
	// make a failure ambiguous between the two. The compression round trip is its own test.
	backend := ts.Backend(func(cfg *s3.Config) { cfg.Compression.Enabled = false })
	ctx := context.Background()

	sizes := []int{0, 1, 4095, 4096, 4097, 1 << 20}
	for _, size := range sizes {
		t.Run(sizeName(size), func(t *testing.T) {
			// Safe to parallelize: each case owns its keys, derived from its own size.
			t.Parallel()

			key := "roundtrip/" + sizeName(size)
			want := testaws.DeterministicBytes(key, size)

			if err := backend.PutObject(ctx, key, want, nil); err != nil {
				t.Fatalf("PutObject(%d bytes): %v", size, err)
			}

			// Direction 1: what the backend wrote, read by an independent client.
			if got := ts.GetObject(key); !bytes.Equal(got, want) {
				t.Errorf("what PutObject stored differs from what was handed to it: stored %d bytes, "+
					"gave %d", len(got), len(want))
			}

			// Direction 2: what an independent client wrote, read by the backend.
			indep := key + ".indep"
			ts.PutObject(indep, want)

			got, err := backend.GetObject(ctx, indep, 0, int64(len(want)))
			if err != nil {
				t.Fatalf("GetObject(%d bytes): %v", size, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("GetObject returned %d bytes, want %d, and the contents differ",
					len(got), len(want))
			}
		})
	}
}

func sizeName(n int) string {
	if n == 0 {
		return "empty"
	}

	return strconv.Itoa(n) + "bytes"
}

// TestGetObjectOnMissingKeyIsNotFound checks that absence is reported as absence. H11/M17 in the
// audit are both instances of the opposite: an error taxonomy that conflates "not there" with
// "something went wrong", which upstream turns into either a spurious failure or a silent empty
// read.
func TestGetObjectOnMissingKeyIsNotFound(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()

	_, err := backend.GetObject(context.Background(), "never/written", 0, 100)
	if err == nil {
		t.Fatal("GetObject on a missing key returned no error")
	}

	// Absence must be reported as absence, and be recognizable as such without string matching —
	// the callers that decide between "create it" and "fail" depend on this distinction, and
	// getting it wrong is how Create came to zero an intact object.
	var objErr *objectfserrors.ObjectFSError
	if !errors.As(err, &objErr) {
		t.Fatalf("GetObject returned an unstructured error, so no caller can classify it: %v", err)
	}
	if objErr.Code != objectfserrors.ErrCodeObjectNotFound {
		t.Errorf("error code = %q, want %q: %v", objErr.Code, objectfserrors.ErrCodeObjectNotFound, err)
	}

	// The key must be recoverable from the error, or a mount-time failure is undiagnosable. It
	// lives in Context rather than the message, which is why the message alone does not name it.
	if got := objErr.Context["key"]; got != "never/written" {
		t.Errorf("error context key = %q, want %q", got, "never/written")
	}
}

// The M17 no-op test that used to live here — deleting a key that is not there — moved to
// delete_absent_test.go, where it sits beside the case that constrains it from the other side: a
// HEAD that failed must not be read as absence. The two are one decision and were easy to get
// half-right.

// TestPoolSaturationDoesNotFailOperations exercises the pool through the backend, concurrently,
// with more callers than the pool has slots. Before the fix, Get returned a nil client once
// currentSize reached maxSize, which every call site dereferenced unchecked — saturation panicked
// and unmounted the filesystem.
func TestPoolSaturationDoesNotFailOperations(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) { cfg.PoolSize = 2 })
	ctx := context.Background()

	const key = "saturation/probe"

	ts.PutObject(key, []byte("payload"))

	const callers = 16

	errs := make(chan error, callers)

	for range callers {
		go func() {
			_, err := backend.HeadObject(ctx, key)
			errs <- err
		}()
	}

	for range callers {
		if err := <-errs; err != nil {
			t.Errorf("HeadObject under pool saturation failed: %v", err)
		}
	}
}

// TestHeadObjectReportsTheStoredSize guards the accounting the kernel depends on. If HeadObject
// reports a size that disagrees with what a read can produce, the kernel pads the difference with
// zeros and the user gets a silently corrupt file — the mechanism behind audit finding C2.
func TestHeadObjectReportsTheStoredSize(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) { cfg.Compression.Enabled = false })
	ctx := context.Background()

	const (
		key  = "sized/probe"
		size = 12000
	)

	data := testaws.DeterministicBytes(key, size)
	if err := backend.PutObject(ctx, key, data, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	info, err := backend.HeadObject(ctx, key)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	if info.Size != size {
		t.Errorf("HeadObject reports size %d, want %d", info.Size, size)
	}

	// And the reported size must be what a full read actually produces.
	got, err := backend.GetObject(ctx, key, 0, info.Size)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if int64(len(got)) != info.Size {
		t.Errorf("HeadObject promised %d bytes but GetObject produced %d; the kernel would pad "+
			"the difference with zeros", info.Size, len(got))
	}
}

// TestGetObjectNegativeSizeDoesNotPanic is the C3 regression test. data[offset:end] with size < 0
// computes end < offset and neither reset arm fires, so the slice expression panics and takes the
// mount process down, unmounting under every open fd. pkg/types.Backend assigns no semantics to
// size <= 0 at all, which is the root cause.
func TestGetObjectNegativeSizeDoesNotPanic(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) { cfg.Compression.Enabled = false })
	ctx := context.Background()

	const key = "negative/probe"

	data := testaws.DeterministicBytes(key, 4096)
	ts.PutObject(key, data)

	cases := []struct {
		name         string
		offset, size int64
	}{
		{"negative size", 100, -1},
		{"negative size large", 100, -4096},
		{"zero size", 100, 0},
		{"negative offset", -1, 100},
		{"offset past end", 8192, 100},
		{"size past end", 0, 1 << 20},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A panic here fails the test rather than the process, which is the only reason this
			// is observable at all.
			got, err := backend.GetObject(ctx, key, tc.offset, tc.size)

			// Either answer is acceptable for now — a defined error or defined bytes. What is not
			// acceptable is a panic, and what a later fix must add is a documented choice between
			// the two. See pkg/types.Backend.GetObject.
			if err != nil {
				t.Logf("GetObject(%d, %d) → error: %v", tc.offset, tc.size, err)

				return
			}

			t.Logf("GetObject(%d, %d) → %d bytes", tc.offset, tc.size, len(got))
		})
	}
}

// TestCopyObjectPreservesWhatMakesTheDestinationUsable is the seam test for #164's backend half.
//
// Rename is a copy then a delete, and the copy has to be server-side or renaming a 10 GiB file becomes
// 20 GiB of transfer. What the copy must carry across is not obvious, because S3's default is to carry
// almost none of it: a CopyObject that names only a source and a destination produces an object with
// the bucket's default storage class, no Content-Encoding, and — depending on the metadata directive —
// no user metadata. Each omission is a distinct user-visible failure:
//
//   - user metadata is where POSIX mode, ownership, and mtime live, so losing it resets a renamed
//     file's permissions, which is not a thing rename does;
//   - Content-Encoding is what the read path dispatches decoding on, and it fails closed on an
//     encoding it cannot handle, so losing it leaves a compressed object permanently unreadable with
//     its bytes intact;
//   - storage class defaults to STANDARD, so losing it silently promotes the object out of the tier
//     the user is paying for — audit finding L26.
//
// Asserted through an independent client rather than through the backend's own HeadObject, so a
// backend that dropped a property and then reported it from somewhere else could not pass.
func TestCopyObjectPreservesWhatMakesTheDestinationUsable(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) { cfg.Compression.Enabled = false })
	ctx := context.Background()

	const src = "copy/source.bin"
	const dst = "copy/destination.bin"

	want := testaws.DeterministicBytes(src, 4096)
	if err := backend.PutObject(ctx, src, want, map[string]string{
		"objectfs-mode": "384", // 0600, deliberately not the default
		"objectfs-uid":  "1234",
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if err := backend.CopyObject(ctx, src, dst); err != nil {
		t.Fatalf("CopyObject: %v", err)
	}

	if got := ts.GetObject(dst); !bytes.Equal(got, want) {
		t.Errorf("the copy holds %d bytes, want %d, and the contents differ", len(got), len(want))
	}

	// The source survives. A copy that moved the object would make rename's delete step a double
	// delete, and any failure between the two steps would lose the file outright.
	if got := ts.GetObject(src); !bytes.Equal(got, want) {
		t.Errorf("the source changed: %d bytes, want %d", len(got), len(want))
	}

	head, err := ts.ClientContext(ctx).HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(dst),
	})
	if err != nil {
		t.Fatalf("HeadObject on the copy: %v", err)
	}

	// Metadata keys round-trip with inconsistent case — real S3 lower-cases them, a Go http.Header
	// round trip title-cases them — so the lookup is case-insensitive. Comparing case-sensitively is
	// how a test passes against one endpoint and fails against another.
	for k, want := range map[string]string{"objectfs-mode": "384", "objectfs-uid": "1234"} {
		var got string
		for hk, hv := range head.Metadata {
			if strings.EqualFold(hk, k) {
				got = hv
			}
		}
		if got != want {
			t.Errorf("the copy's %s = %q, want %q. POSIX mode and ownership live in user metadata and "+
				"nowhere else, so a copy that drops it renames a file into different permissions",
				k, got, want)
		}
	}
}

// TestCopyObjectHandlesAKeyContainingAPlus is a regression test for a bug found by probing real S3,
// not by reading the code — and one that was already shipping.
//
// S3 reads x-amz-copy-source as a URL path, so it has to be escaped. url.PathEscape looks like the
// right tool and is not: it leaves "+" as itself, and S3 decodes "+" in this header as a space. So
// copying "a+b.txt" asks for "a b.txt" and gets 404 NoSuchKey. Verified against real S3 in us-west-2,
// where both url.PathEscape and (&url.URL{Path: …}).EscapedPath() fail on such a key and only %2B
// works; the substrate endpoint this test runs against reproduces both answers exactly.
//
// A "+" in a filename is ordinary — version numbers, C++ sources, ISO timestamps with a UTC offset —
// and the affected paths are ones a user expects to be invisible: SetObjectMetadata is how chmod
// persists, and the tier optimizer's transition is automatic. Both returned NoSuchKey for a file that
// plainly existed, and the tier transition surfaced it to nobody.
func TestCopyObjectHandlesAKeyContainingAPlus(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) { cfg.Compression.Enabled = false })
	ctx := context.Background()

	// Each key exercises a character url.PathEscape treats differently from what S3 expects, or — for
	// the space and the percent — one it handles correctly and which must keep working.
	keys := []string{
		"plus+key.bin",
		"dir+one/file+two.bin",
		"space name.bin",
		"pct%20literal.bin",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			want := testaws.DeterministicBytes(key, 128)
			if err := backend.PutObject(ctx, key, want, map[string]string{"objectfs-mode": "420"}); err != nil {
				t.Fatalf("PutObject(%q): %v", key, err)
			}

			dst := "copied/" + key
			if err := backend.CopyObject(ctx, key, dst); err != nil {
				t.Fatalf("CopyObject(%q): %v. The copy source is escaped wrongly for this key, so S3 is "+
					"being asked for an object that does not exist", key, err)
			}

			if got := ts.GetObject(dst); !bytes.Equal(got, want) {
				t.Errorf("the copy of %q holds %d bytes, want %d", key, len(got), len(want))
			}

			// SetObjectMetadata is a self-copy and carries the same escaping, which is how this bug
			// reached a shipped path: chmod on a file with a "+" in its name failed with NoSuchKey.
			if err := backend.SetObjectMetadata(ctx, key, map[string]string{"objectfs-mode": "384"}); err != nil {
				t.Fatalf("SetObjectMetadata(%q): %v. chmod persists through a self-copy, so it fails on "+
					"exactly the keys CopyObject does", key, err)
			}
		})
	}
}

// TestCopyObjectAboveTheSinglePartLimitUsesAMultipartCopy covers the branch a >5 GiB rename takes.
//
// S3's CopyObject fails outright above 5 GiB, so an object larger than that has to be copied part by
// part with UploadPartCopy. Reaching that branch honestly would mean creating a 5 GiB object, which
// costs real storage and hours of transfer — so in practice it does not get tested, and a mutation
// deleting the routing produced no failure at all. The thresholds are scaled down instead, which
// exercises the same code: the part loop, the CopySourceRange arithmetic, and the properties that have
// to be restated because MetadataDirective=COPY does not exist for a multipart upload.
//
// That last point is why this test is not redundant with the single-part one. A multipart upload's
// metadata is fixed at CreateMultipartUpload, so the source's map has to be carried across by hand —
// entirely separate code from the single-part path, with its own way to silently lose the POSIX
// attributes.
//
// The scaling has one honest limit: real S3 requires every non-final part to be at least 5 MB and
// answers EntityTooSmall at Complete otherwise. A few-kilobyte part violates that, so this verifies the
// mechanics of the path and not S3's part-size rule. The live integration suite is where a real
// oversized object would belong.
func TestCopyObjectAboveTheSinglePartLimitUsesAMultipartCopy(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireUploadPartCopy()

	backend := ts.Backend(func(cfg *s3.Config) { cfg.Compression.Enabled = false })
	ctx := context.Background()

	// 10 KiB in 3 KiB parts: four parts, the last one short. An uneven final part is the case where
	// off-by-one range arithmetic shows up — an exclusive/inclusive mixup on the last part either drops
	// a byte or asks for one past the end.
	const (
		size            = 10 << 10
		singlePartLimit = 4 << 10
		partSize        = 3 << 10
	)

	const src = "bigcopy/source.bin"
	const dst = "bigcopy/destination.bin"

	want := testaws.DeterministicBytes(src, size)
	if err := backend.PutObject(ctx, src, want, map[string]string{
		"objectfs-mode": "384",
		"objectfs-uid":  "1234",
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	s3.SetCopyThresholdsForTest(backend, singlePartLimit, partSize)

	if err := backend.CopyObject(ctx, src, dst); err != nil {
		t.Fatalf("CopyObject of a %d-byte object with a %d-byte single-part limit: %v",
			size, singlePartLimit, err)
	}

	// The multipart path really ran. Without this the test would pass identically if the routing were
	// deleted, which is the state it was written to fix.
	if n := ts.Operations("UploadPartCopy"); n != 4 {
		t.Errorf("the endpoint saw %d UploadPartCopy calls, want 4 (%d bytes in %d-byte parts). "+
			"Zero means the copy took the single-part path and this test proves nothing about multipart",
			n, size, partSize)
	}

	if got := ts.GetObject(dst); !bytes.Equal(got, want) {
		t.Errorf("the assembled copy holds %d bytes, want %d, and the contents differ", len(got), len(want))
	}

	if got := ts.GetObject(src); !bytes.Equal(got, want) {
		t.Errorf("the source changed: %d bytes, want %d", len(got), len(want))
	}

	// Restated by hand on this path, since CreateMultipartUpload fixes the metadata and there is no
	// COPY directive to fall back on.
	head, err := ts.ClientContext(ctx).HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(dst),
	})
	if err != nil {
		t.Fatalf("HeadObject on the assembled copy: %v", err)
	}

	for k, want := range map[string]string{"objectfs-mode": "384", "objectfs-uid": "1234"} {
		var got string
		for hk, hv := range head.Metadata {
			if strings.EqualFold(hk, k) {
				got = hv
			}
		}
		if got != want {
			t.Errorf("the assembled copy's %s = %q, want %q. A multipart upload's metadata is fixed at "+
				"CreateMultipartUpload, so this path has to restate it explicitly and is where losing it "+
				"is easiest", k, got, want)
		}
	}

	// No upload left behind. The parts of an abandoned multipart upload are billed and invisible to
	// ListObjects, so a leak here is one an operator cannot find — audit finding H10.
	if ids := ts.MultipartUploads(); len(ids) != 0 {
		t.Errorf("after a successful multipart copy %d multipart upload(s) are still open: %v. "+
			"Their parts are billed and no object listing shows them", len(ids), ids)
	}
}

// TestCopyObjectAbortsTheUploadWhenAPartCopyFails is the leak half of the multipart copy path.
//
// A copy that fails partway has already created a multipart upload, and possibly uploaded parts. Those
// parts are billed and invisible to ListObjects, so failing to abort leaks storage the operator has no
// way to discover — audit finding H10, on this same mechanism, where one failure path aborted and the
// other did not.
//
// The failure is provoked by deleting the source between the HEAD that sized it and the part copies that
// read it, which is a real race as well as a convenient one: another writer can remove a file while a
// rename of it is in flight.
func TestCopyObjectAbortsTheUploadWhenAPartCopyFails(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireUploadPartCopy()

	backend := ts.Backend(func(cfg *s3.Config) { cfg.Compression.Enabled = false })
	ctx := context.Background()

	const src = "abortcopy/source.bin"
	const dst = "abortcopy/destination.bin"

	want := testaws.DeterministicBytes(src, 10<<10)
	if err := backend.PutObject(ctx, src, want, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	head, err := backend.HeadObject(ctx, src)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if head.Size != int64(len(want)) {
		t.Fatalf("HeadObject reports %d bytes, want %d", head.Size, len(want))
	}

	s3.SetCopyThresholdsForTest(backend, 4<<10, 3<<10)

	// Removed after the size is known but before the copy: the copy heads the source itself, and this
	// only has to make the *part* copies fail, which a missing source does.
	if err := backend.DeleteObject(ctx, src); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if err := backend.CopyObject(ctx, src, dst); err == nil {
		t.Fatal("CopyObject of a source that no longer exists returned no error")
	}

	if ids := ts.MultipartUploads(); len(ids) != 0 {
		t.Errorf("after a failed multipart copy %d multipart upload(s) are still open: %v. The deferred "+
			"abort did not run, so their parts are billed and no object listing shows them",
			len(ids), ids)
	}

	// And no partial object at the destination. A rename whose copy failed must leave the destination
	// untouched, or the caller cannot retry it and cannot tell what happened.
	if ts.ObjectExists(dst) {
		t.Errorf("the destination %q exists after a failed copy; a partial object there is worse than "+
			"none, since a retry would have to distinguish it from a complete one", dst)
	}
}

// TestCopyObjectOfAMissingSourceIsClassifiableAsAbsence keeps rename able to answer ENOENT.
//
// Renaming a file that is not there is ENOENT, and the only way the FUSE layer can produce that answer
// is if the copy's failure is recognizable as absence rather than as a generic error. It is the same
// distinction that mattered for Lookup, where collapsing every HeadObject failure into "absent" let
// Create zero an intact object: a classifier that cannot tell absence from a throttle is dangerous in
// both directions.
func TestCopyObjectOfAMissingSourceIsClassifiableAsAbsence(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) { cfg.Compression.Enabled = false })

	err := backend.CopyObject(context.Background(), "never/written", "somewhere/else")
	if err == nil {
		t.Fatal("CopyObject of a missing source returned no error")
	}

	var objErr *objectfserrors.ObjectFSError
	if !errors.As(err, &objErr) {
		t.Fatalf("CopyObject returned an unstructured error, so no caller can classify it: %v", err)
	}
	if objErr.Code != objectfserrors.ErrCodeObjectNotFound {
		t.Errorf("error code = %q, want %q. Rename has to answer ENOENT for a source that is not there, "+
			"and it can only do that if absence is distinguishable here: %v",
			objErr.Code, objectfserrors.ErrCodeObjectNotFound, err)
	}
}
