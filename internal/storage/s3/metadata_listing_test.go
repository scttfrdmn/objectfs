package s3_test

// The two operations the FUSE node contract added to the backend: an attribute-only write, and a
// listing that does not stop at 1000 keys.
//
// Both are seam operations in the strict sense — the correctness question is entirely about what
// crosses the wire, not about what the function returns. SetObjectMetadata returns nil whether or not
// the endpoint stored anything, because S3's only metadata-update mechanism is a CopyObject that
// answers 200 either way; ListObjects returns a plausible slice whether or not it followed the
// continuation token, because a truncated listing is a valid listing of a smaller directory. Neither
// can be tested against a mock: a mock would return what the caller asked it to return.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	objectfserrors "github.com/scttfrdmn/objectfs/pkg/errors"
)

// TestSetObjectMetadataReplacesWithoutRewritingBytes is the property the whole attribute path rests
// on: a chmod changes nine permission bits and does not re-upload the object.
//
// This is why the operation is a CopyObject and not a read-modify-PUT. On a 10 GiB object the
// difference is not an optimization, it is the difference between a working chmod and one that costs
// a full egress-plus-ingress cycle and can fail halfway through with the object in an unknown state.
// The test asserts it by byte count at the endpoint, because that is the only place the distinction
// is visible — both implementations return nil.
func TestSetObjectMetadataReplacesWithoutRewritingBytes(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireMetadataReplace()

	// Compression off: this test is about metadata and transfer accounting, and a codec in the path
	// makes the byte assertion below ambiguous between "did not re-upload" and "re-uploaded, smaller".
	backend := ts.Backend(func(cfg *s3.Config) { cfg.Compression.Enabled = false })
	ctx := context.Background()

	const (
		key  = "attrs/plain"
		size = 64 * 1024
	)

	want := testaws.DeterministicBytes(key, size)
	if err := backend.PutObject(ctx, key, want, map[string]string{"objectfs-mode": "0644"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	ts.ResetRequests()

	if err := backend.SetObjectMetadata(ctx, key, map[string]string{
		"objectfs-mode": "0600",
		"objectfs-uid":  "4242",
	}); err != nil {
		t.Fatalf("SetObjectMetadata: %v", err)
	}

	// The values must actually be stored. Read through the raw SDK rather than through the backend:
	// the failure mode this guards is an endpoint that answers 200 and keeps the old metadata, and
	// asking the same layer that wrote would not see it.
	meta := ts.ObjectMetadata(key)
	if got := meta["objectfs-mode"]; got != "0600" {
		t.Errorf("objectfs-mode = %q, want %q — the chmod reported success and stored nothing, so the "+
			"mode will not survive a remount", got, "0600")
	}
	if got := meta["objectfs-uid"]; got != "4242" {
		t.Errorf("objectfs-uid = %q, want %q", got, "4242")
	}

	// The bytes must not have moved. A PUT of the body would show up as request bytes; a GET as
	// response bytes. Both are counted, because a read-modify-PUT implementation does both.
	var uploaded, downloaded int64
	for _, r := range ts.RequestsFor(key) {
		uploaded += r.RequestBytes
		downloaded += r.ResponseBytes
	}
	if uploaded >= size {
		t.Errorf("changing metadata on a %d-byte object sent %d body bytes to the endpoint; a "+
			"metadata write must not re-upload the object. Observed: %s",
			size, uploaded, describe(ts.Requests()))
	}
	if downloaded >= size {
		t.Errorf("changing metadata on a %d-byte object read %d body bytes back from the endpoint; "+
			"the copy is server-side and transfers nothing. Observed: %s",
			size, downloaded, describe(ts.Requests()))
	}

	// And the contents must be unchanged. The audit's shape for this defect is a metadata write that
	// truncates, which a byte-count assertion alone would not catch: writing zero bytes over the
	// object also sends no body.
	if got := ts.GetObject(key); string(got) != string(want) {
		t.Errorf("after a metadata-only write the object holds %d bytes, want %d — the write path "+
			"rewrote the body it was supposed to leave alone", len(got), len(want))
	}
}

// TestSetObjectMetadataPreservesContentEncoding is the integrity half, and the reason
// SetObjectMetadata restates properties it never asked to change.
//
// MetadataDirective=REPLACE discards every stored property, not only the metadata map. The read path
// dispatches decoding on the stored Content-Encoding and fails closed on an encoding it cannot
// handle, so an attribute write that dropped the header would leave a compressed object permanently
// unreadable — a chmod that destroys a file. The failure is silent at write time and appears later as
// an integrity error on a read nobody connects to the chmod.
func TestSetObjectMetadataPreservesContentEncoding(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireMetadataReplace()

	// Compression on, and a body well above the 4 KiB MinSize so it actually engages.
	backend := ts.Backend()
	ctx := context.Background()

	const key = "attrs/compressed"

	want := compressible(32 * 1024)
	if err := backend.PutObject(ctx, key, want, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Establish that the object really is stored encoded, so a pass below means the header survived
	// rather than that there was never anything to preserve.
	if stored := ts.ObjectSize(key); stored >= int64(len(want)) {
		t.Fatalf("the object stored %d bytes for a %d-byte compressible body, so compression did not "+
			"engage and this test would pass for the wrong reason", stored, len(want))
	}
	before := ts.ObjectMetadata(key)
	if before[metaChecksumKey] == "" {
		t.Fatalf("the object carries no %s, so there is no integrity metadata to preserve",
			metaChecksumKey)
	}

	if err := backend.SetObjectMetadata(ctx, key, map[string]string{"objectfs-mode": "0640"}); err != nil {
		t.Fatalf("SetObjectMetadata: %v", err)
	}

	// The decisive assertion: the object still reads back as its original bytes. This exercises the
	// stored Content-Encoding, because a dropped header means the read path returns the raw frame or
	// refuses outright.
	got, err := backend.GetObject(ctx, key, 0, int64(len(want)))
	if err != nil {
		t.Fatalf("GetObject after a metadata write: %v — the write dropped a property the read path "+
			"needs, so the object is now unreadable", err)
	}
	if string(got) != string(want) {
		t.Errorf("after a metadata write the object reads back as %d bytes, want %d", len(got), len(want))
	}

	// The integrity metadata is the backend's own and must survive a caller that knows nothing about
	// it. A caller setting a mode has not seen the bytes and could not recompute the checksum.
	after := ts.ObjectMetadata(key)
	if after[metaChecksumKey] != before[metaChecksumKey] {
		t.Errorf("%s = %q after the metadata write, was %q — the checksum the read path verifies "+
			"against was replaced by a caller that never saw the bytes",
			metaChecksumKey, after[metaChecksumKey], before[metaChecksumKey])
	}
	if after[metaOriginalSizeKey] != before[metaOriginalSizeKey] {
		t.Errorf("%s = %q after the metadata write, was %q — HeadObject now reports the compressed "+
			"length as the file size", metaOriginalSizeKey, after[metaOriginalSizeKey],
			before[metaOriginalSizeKey])
	}
	if got := after["objectfs-mode"]; got != "0640" {
		t.Errorf("objectfs-mode = %q, want %q", got, "0640")
	}
}

// TestSetObjectMetadataPreservesStorageClass is the cost half of the same restatement.
//
// REPLACE discards the storage class along with everything else, and an absent x-amz-storage-class on
// the copy means STANDARD — so an attribute write that did not restate it would silently promote every
// object it touched out of the tier the user chose, at roughly twice the per-GB rate for STANDARD_IA
// and far more against the archive tiers. Nothing fails, nothing logs, and the bill moves. This is the
// same defect shape as L26, where a tier transition stripped Content-Encoding.
func TestSetObjectMetadataPreservesStorageClass(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireMetadataReplace()

	// STANDARD_IA rather than an archive tier: the point is the class round-tripping, and a Glacier
	// object cannot be read back to confirm the body survived.
	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.StorageTier = s3.TierStandardIA
		cfg.Compression.Enabled = false
		cfg.EnableCargoShipOptimization = false
	})
	ctx := context.Background()

	// Above STANDARD_IA's 128 KiB billing minimum, so the write is not one ValidateWrite warns about.
	// The size is not load-bearing since #154 — a smaller object is stored, not refused — but keeping
	// it above the minimum keeps the log clean and the test about metadata.
	const (
		key  = "attrs/tiered"
		size = 192 * 1024
	)

	want := testaws.DeterministicBytes(key, size)
	if err := backend.PutObject(ctx, key, want, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Establish the precondition, so a pass means the class was preserved rather than that it was
	// STANDARD from the start and there was nothing to lose.
	if got := ts.ObjectStorageClass(key); got != s3.TierStandardIA {
		t.Fatalf("the object was stored as %q, want %q; this test cannot detect a demotion from a "+
			"tier the object was never in", got, s3.TierStandardIA)
	}

	if err := backend.SetObjectMetadata(ctx, key, map[string]string{"objectfs-mode": "0600"}); err != nil {
		t.Fatalf("SetObjectMetadata: %v", err)
	}

	if got := ts.ObjectStorageClass(key); got != s3.TierStandardIA {
		t.Errorf("after a metadata-only write the object is stored as %q, want %q — a chmod moved the "+
			"object to a different storage tier, which nothing reports and the user pays for",
			got, s3.TierStandardIA)
	}

	// The body has to survive the copy too. A restatement bug that got the class right by re-uploading
	// would pass the assertion above.
	if got := ts.GetObject(key); string(got) != string(want) {
		t.Errorf("the object holds %d bytes after the metadata write, want %d", len(got), len(want))
	}
}

// TestSetObjectMetadataRefusesToOverrideIntegrityKeys pins the one thing a caller is not allowed to
// change.
//
// objectfs-sha256 and objectfs-original-size are how the read path decides whether what came back is
// what was written. A caller that could set them could make any object verify against any bytes,
// which turns the integrity check into a formality. This is not a hypothetical caller: vfs.Flusher
// passes a whole attribute map through, and a bug there — or a future attribute named too close to
// these — would arrive here.
func TestSetObjectMetadataRefusesToOverrideIntegrityKeys(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireMetadataReplace()

	backend := ts.Backend()
	ctx := context.Background()

	const key = "attrs/integrity-keys"

	want := compressible(16 * 1024)
	if err := backend.PutObject(ctx, key, want, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	before := ts.ObjectMetadata(key)

	// A hostile-shaped call: the correct checksum for different bytes, plus a matching size, in the
	// casing a caller reading them back would naturally use.
	if err := backend.SetObjectMetadata(ctx, key, map[string]string{
		"objectfs-mode":       "0600",
		metaChecksumKey:       sha256Hex([]byte("not the object's bytes")),
		metaOriginalSizeKey:   "1",
		"OBJECTFS-SHA256":     sha256Hex([]byte("nor these")),
		"Objectfs-Original-S": "unrelated",
	}); err != nil {
		t.Fatalf("SetObjectMetadata: %v", err)
	}

	after := ts.ObjectMetadata(key)
	if after[metaChecksumKey] != before[metaChecksumKey] {
		t.Errorf("%s = %q, want the original %q: a caller replaced the checksum the read path "+
			"verifies against, so the object would now verify against bytes it does not hold",
			metaChecksumKey, after[metaChecksumKey], before[metaChecksumKey])
	}
	if after[metaOriginalSizeKey] != before[metaOriginalSizeKey] {
		t.Errorf("%s = %q, want the original %q", metaOriginalSizeKey,
			after[metaOriginalSizeKey], before[metaOriginalSizeKey])
	}

	// The rest of the map is still applied — the refusal is scoped to the two integrity keys, not a
	// blanket rejection that would make an attribute write fail on a caller's typo.
	if got := after["objectfs-mode"]; got != "0600" {
		t.Errorf("objectfs-mode = %q, want %q: the integrity refusal discarded the whole map", got, "0600")
	}

	// The bytes must still verify, which is the property all of the above exists to protect.
	got, err := backend.GetObject(ctx, key, 0, int64(len(want)))
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the object reads back as %d bytes, want %d", len(got), len(want))
	}
}

// TestSetObjectMetadataOnMissingKeyIsNotFound checks that the attribute path distinguishes absence,
// because the layer above it decides between reporting ENOENT and creating the object.
//
// The HEAD comes first in SetObjectMetadata precisely so this is answered before a CopyObject is
// attempted — a self-copy of a nonexistent key fails with NoSuchKey, which is a different code, from
// a different operation, that the translation layer would have to classify separately.
func TestSetObjectMetadataOnMissingKeyIsNotFound(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()

	err := backend.SetObjectMetadata(context.Background(), "attrs/never-written",
		map[string]string{"objectfs-mode": "0644"})
	if err == nil {
		t.Fatal("SetObjectMetadata on a missing key returned no error, so a chmod of a file that is " +
			"not there reports success")
	}

	var objErr *objectfserrors.ObjectFSError
	if !errors.As(err, &objErr) {
		t.Fatalf("SetObjectMetadata returned an unstructured error, so no caller can classify it "+
			"as absence: %v", err)
	}
	if objErr.Code != objectfserrors.ErrCodeObjectNotFound {
		t.Errorf("error code = %q, want %q: %v", objErr.Code,
			objectfserrors.ErrCodeObjectNotFound, err)
	}
}

// TestListObjectsFollowsContinuationTokens is the regression test for #171.
//
// S3 caps a single ListObjectsV2 response at 1000 keys whatever MaxKeys asks for, and v0.10.0 issued
// exactly one request. A directory with more than 1000 entries was therefore silently truncated —
// and a truncated listing is not a cosmetic problem, because the entries that do not appear do not
// exist as far as readdir is concerned, and therefore not as far as `cp -r` or `rm -r` are either.
// The recursive delete case is the one that loses data: `rm -r` removes what it was told about.
func TestListObjectsFollowsContinuationTokens(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	// Deliberately just over the 1000-key page cap. One key past the boundary is what distinguishes
	// a paginating implementation from one that stops: at exactly 1000 both agree.
	const (
		prefix = "page/"
		total  = 1001
	)

	writeNumberedObjects(t, backend, prefix, total)

	got, err := backend.ListObjects(ctx, prefix, 0)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(got) != total {
		t.Fatalf("ListObjects with no limit returned %d of %d objects. A short listing makes the "+
			"missing entries invisible to readdir, so `rm -r` skips them and `cp -r` leaves them "+
			"behind.", len(got), total)
	}

	// Every key exactly once. A pagination loop that reuses a stale continuation token, or that
	// re-reads the last page, returns the right count with the wrong contents.
	seen := make(map[string]int, len(got))
	for _, obj := range got {
		seen[obj.Key]++
	}
	if len(seen) != total {
		t.Errorf("the listing holds %d distinct keys across %d entries, so at least one key appears "+
			"twice and another is missing", len(seen), len(got))
	}
	for i := range total {
		key := numberedKey(prefix, i)
		switch n := seen[key]; n {
		case 1:
		case 0:
			t.Errorf("%q is absent from the listing", key)
		default:
			t.Errorf("%q appears %d times in the listing", key, n)
		}
	}
}

// TestListObjectsHonorsItsLimitExactly checks the other direction, which is the one Lookup depends
// on.
//
// Lookup probes a directory's existence with limit 1, and a Readdir asks for what it can use. An
// implementation that over-fetched would still return the right answer, so the assertion has to be on
// what was requested at the endpoint, not on what came back: the cost of a 1000-key page for a
// one-key question is paid whether or not the extra keys are discarded.
//
//nolint:tparallel // subtests share one endpoint's request log; see the loop below
func TestListObjectsHonorsItsLimitExactly(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	const (
		prefix = "limits/"
		total  = 1200
	)

	writeNumberedObjects(t, backend, prefix, total)

	// The subtests share one endpoint and therefore run serially, which is why they do not call
	// t.Parallel. The assertion is on the endpoint's request log, and that log is per-endpoint: two
	// concurrent subtests would each ResetRequests out from under the other and read the other's
	// listing requests as their own. The alternative — an endpoint each — means writing 1200 objects
	// five times over, and the setup already dominates this test's runtime.
	//nolint:paralleltest,tparallel // shared request log; see above
	for _, limit := range []int{1, 10, 999, 1000, 1001} {
		t.Run(fmt.Sprintf("limit%d", limit), func(t *testing.T) {
			ts.ResetRequests()

			got, err := backend.ListObjects(ctx, prefix, limit)
			if err != nil {
				t.Fatalf("ListObjects(limit %d): %v", limit, err)
			}
			if len(got) != limit {
				t.Errorf("ListObjects(limit %d) returned %d objects, want exactly %d — %d objects "+
					"exist under the prefix", limit, len(got), limit, total)
			}

			// A limit under the page cap must be asked for, not filtered after the fact.
			if limit < 1000 {
				assertRequestedMaxKeys(t, ts, limit)
			}
		})
	}
}

// TestListObjectsOnAnEmptyPrefixIsEmptyNotAnError separates "no entries" from "failed to list". An
// empty directory is an ordinary state and readdir has to render it as such.
func TestListObjectsOnAnEmptyPrefixIsEmptyNotAnError(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()

	got, err := backend.ListObjects(context.Background(), "nothing-here/", 0)
	if err != nil {
		t.Fatalf("ListObjects on an empty prefix: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListObjects on an empty prefix returned %d objects", len(got))
	}
}

// assertRequestedMaxKeys fails unless every listing request the endpoint saw asked for at most n
// keys. It reads the query string rather than the SDK input, because the point is what went over the
// wire — and because MaxKeys is the field whose omission made the single-request version look correct
// while S3 quietly capped it.
func assertRequestedMaxKeys(t *testing.T, ts *testaws.TestServer, n int) {
	t.Helper()

	var lists int

	for _, r := range ts.Requests() {
		if r.Method != http.MethodGet || !strings.Contains(r.Query, "list-type=2") {
			continue
		}

		lists++

		asked, ok := maxKeysOf(r.Query)
		if !ok {
			t.Errorf("a listing request carried no max-keys at all (query %q), so S3 applies its own "+
				"1000-key cap and a one-key probe pays for a thousand", r.Query)

			continue
		}
		if asked > n {
			t.Errorf("a listing for at most %d keys asked the endpoint for %d (query %q)",
				n, asked, r.Query)
		}
	}

	if lists == 0 {
		t.Errorf("the endpoint saw no ListObjectsV2 request at all. Observed: %s",
			describe(ts.Requests()))
	}
}

// maxKeysOf extracts the max-keys query parameter, reporting whether it was present.
func maxKeysOf(query string) (int, bool) {
	for part := range strings.SplitSeq(query, "&") {
		name, value, found := strings.Cut(part, "=")
		if !found || name != "max-keys" {
			continue
		}

		var n int
		if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
			return 0, false
		}

		return n, true
	}

	return 0, false
}

// writeNumberedObjects writes n one-byte objects under prefix, concurrently.
//
// One byte each because these tests are about the listing, not the bodies, and 1200 objects of any
// real size would make the setup dominate the run. Concurrently because serially this takes long
// enough to be worth avoiding — the emulator answers in microseconds but the round trips add up.
func writeNumberedObjects(t *testing.T, backend *s3.Backend, prefix string, n int) {
	t.Helper()

	ctx := context.Background()

	const parallelism = 16

	var (
		wg   sync.WaitGroup
		sem  = make(chan struct{}, parallelism)
		mu   sync.Mutex
		errs []error
	)

	for i := range n {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			// A one-byte body stays under the compression MinSize, so the stored object is the
			// literal byte whatever the config says.
			if err := backend.PutObject(ctx, numberedKey(prefix, i), []byte{'x'}, nil); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("put %d: %w", i, err))
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	if len(errs) > 0 {
		sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
		t.Fatalf("setting up %d objects failed (%d errors), first: %v", n, len(errs), errs[0])
	}
}

// numberedKey formats a listing key with a fixed-width index, so the emulator's lexical key order
// matches the numeric order a failure message reports.
func numberedKey(prefix string, i int) string {
	return fmt.Sprintf("%s%05d", prefix, i)
}
