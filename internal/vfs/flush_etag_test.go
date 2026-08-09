package vfs_test

// [vfs.Writer.FlushReportingETag] exists so a caller can name the version its own write produced (#141).
// What makes it worth its own file is that the value has exactly one valid source and three legitimate
// ways of being absent, and getting either wrong is not a cosmetic difference: the ETag is the key of a
// receiver's replay ledger, so a version naming somebody else's write suppresses an invalidation that
// has not been applied, and peers keep serving bytes that were replaced.

import (
	"errors"
	"sync"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// TestFlushReportingETagNamesTheVersionItWrote is the ordinary case, and it asserts against the
// backend's own record rather than against a literal.
//
// The fake derives its ETag from the stored length — "etag-8" for eight bytes — so a test comparing
// against "etag-8" would pass while reporting a version read from anywhere, including from a HeadObject
// issued after the fact. Comparing against what HeadObject says *now* is the property that matters: the
// value came out of this write.
func TestFlushReportingETagNamesTheVersionItWrote(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	const key = "f"

	if err := w.Write(key, 0, []byte("AAAABBBB")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	etag, err := w.FlushReportingETag(t.Context(), key)
	if err != nil {
		t.Fatalf("FlushReportingETag: %v", err)
	}

	info, err := backend.HeadObject(t.Context(), key)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	if etag == "" {
		t.Fatal("a flush that stored eight bytes reported no version, so an invalidation for it would " +
			"be unversioned and every receiver would apply it every time")
	}

	if etag != info.ETag {
		t.Errorf("flush reported version %q but the stored object is %q; a receiver's replay ledger is "+
			"keyed on this, so a version naming a different write suppresses an invalidation that was "+
			"never applied", etag, info.ETag)
	}
}

// TestFlushReportingETagAfterAWriteRacedTheUpload asserts the version reported describes the bytes that
// are actually in the bucket at the end.
//
// A write landing during the upload makes [vfs.Node.MarkFlushed] refuse, so the first attempt's ETag
// names four bytes that the retry immediately replaces with eight. Reporting that version would tell
// peers to evict at a version no reader can fetch, and would write a ledger entry under it.
//
// Be clear about what this does *not* cover: [vfs.Writer.FlushReportingETag]'s `if n.Dirty()` guard is
// not reachable from here. Flush's retry loop converges before returning, so the node is clean by the
// time the ETag is read — removing that guard leaves this test green, which is checked rather than
// assumed. The guard's own comment records that it is defensive.
func TestFlushReportingETagAfterAWriteRacedTheUpload(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	const key = "f"

	if err := w.Write(key, 0, []byte("AAAA")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var once sync.Once
	backend.mu.Lock()
	backend.onPut = func() {
		once.Do(func() {
			if err := w.Write(key, 4, []byte("BBBB")); err != nil {
				t.Errorf("racing write: %v", err)
			}
		})
	}
	backend.mu.Unlock()

	etag, err := w.FlushReportingETag(t.Context(), key)
	if err != nil {
		t.Fatalf("FlushReportingETag: %v", err)
	}

	stored, _ := backend.Object(key)
	if string(stored) != "AAAABBBB" {
		t.Fatalf("the racing write was lost: stored %q", stored)
	}

	info, err := backend.HeadObject(t.Context(), key)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	// The version of the eight-byte object, not the four-byte one the first attempt uploaded. Reporting
	// the first attempt's ETag would name a version that existed for the duration of one upload and is
	// no longer what any reader can fetch.
	if etag != info.ETag {
		t.Errorf("flush reported version %q after a write raced the upload, but the stored object is "+
			"%q — that names content the next flush already replaced", etag, info.ETag)
	}
}

// TestFlushReportingETagOnAnUnbufferedKey covers the first of the three legitimate empties.
//
// Nothing was buffered, so no write happened and there is no version to name. An error here would break
// fsync on a file with no pending writes and a second close(2), both of which are no-ops.
func TestFlushReportingETagOnAnUnbufferedKey(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)

	etag, err := w.FlushReportingETag(t.Context(), "never-written")
	if err != nil {
		t.Fatalf("FlushReportingETag on an unbuffered key: %v, want nil", err)
	}

	if etag != "" {
		t.Errorf("reported version %q for a key nothing was buffered for; there is no write for it to "+
			"name", etag)
	}

	if calls := backend.Calls(); len(calls) != 0 {
		t.Errorf("a flush of an unbuffered key issued %d requests: %v", len(calls), calls)
	}
}

// TestFlushReportingETagOnAnAttrOnlyFlushOfAnAbsentObject covers the second empty, and it is the one
// most easily mistaken for a bug.
//
// A file created and chmod'ed without ever being written has attributes pending and no object in
// storage. [vfs.Flusher] takes the attribute-only arm, SetObjectMetadata reports absence, and the node
// is deliberately left dirty so the pending mode survives to the flush that has content. Nothing was
// stored, so there is no version — and the flush reports success, because failing would make close(2)
// fail on a legal sequence.
func TestFlushReportingETagOnAnAttrOnlyFlushOfAnAbsentObject(t *testing.T) {
	t.Parallel()

	w, _ := newWriter(t)
	const key = "created-not-written"

	if err := w.SetAttr(t.Context(), key, true, false, false, vfs.Attr{Mode: 0o600}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	etag, err := w.FlushReportingETag(t.Context(), key)
	if err != nil {
		t.Fatalf("FlushReportingETag: %v", err)
	}

	if etag != "" {
		t.Errorf("reported version %q for a flush that stored nothing: the object does not exist, so no "+
			"version of it can be named", etag)
	}
}

// TestFlushReportingETagPropagatesTheFailure asserts the error path reports no version.
//
// An ETag alongside an error would be the worst combination available: a caller that checked the string
// before the error — which is the shape of every fire-and-forget invalidation — would broadcast a version
// for a write that never landed, and peers would evict bytes that are still correct.
//
// # Why the file is written twice
//
// The first flush succeeds, which is what puts a version *on the node*. Without it the node's ETag is
// empty whatever the code does — a file being created for the first time has no stored version — so a
// mutation returning `n.Attr().ETag` on the error path would pass, and the test would be asserting
// nothing. Verified by mutation: with a single write it survives; with the successful flush first it dies.
func TestFlushReportingETagPropagatesTheFailure(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	const key = "f"

	if err := w.Write(key, 0, []byte("AAAA")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	firstETag, err := w.FlushReportingETag(t.Context(), key)
	if err != nil {
		t.Fatalf("first FlushReportingETag: %v", err)
	}
	if firstETag == "" {
		t.Fatal("the first flush reported no version, so this test cannot tell a suppressed ETag from " +
			"an absent one")
	}

	// A second write, whose upload fails. The node now carries firstETag — the version the *previous*
	// write produced and the one still in the bucket, which is precisely the version peers must not be
	// told to evict.
	if err := w.Write(key, 4, []byte("BBBB")); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	wantErr := errors.New("AccessDenied")
	backend.mu.Lock()
	backend.putErr = wantErr
	backend.mu.Unlock()

	etag, err := w.FlushReportingETag(t.Context(), key)
	if !errors.Is(err, wantErr) {
		t.Fatalf("FlushReportingETag error = %v, want it to wrap %v", err, wantErr)
	}

	if etag != "" {
		t.Errorf("reported version %q from a flush that failed; the bucket still holds %q, so peers told "+
			"to evict at that version would drop bytes that are still current", etag, firstETag)
	}
}

// TestFlushContextStillReportsOnlyTheError pins the delegation, because FlushContext is what nearly
// every caller uses and it is now a wrapper.
//
// Trivial-looking, and kept for the reason kernel_options_test.go in internal/fuse rejects copy checks
// on principle: this one is not a copy check. It asserts the wrapper performs the work — the object is
// in the bucket afterwards — rather than that two fields agree.
func TestFlushContextStillReportsOnlyTheError(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	const key = "f"

	if err := w.Write(key, 0, []byte("AAAA")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := w.FlushContext(t.Context(), key); err != nil {
		t.Fatalf("FlushContext: %v", err)
	}

	if got, _ := backend.Object(key); string(got) != "AAAA" {
		t.Errorf("FlushContext stored %q, want %q", got, "AAAA")
	}

	if w.Dirty(key) {
		t.Error("the key is still dirty after a flush that reported success")
	}
}
