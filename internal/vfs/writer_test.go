package vfs_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/vfs"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// The tests in this file are the audit's decisive write-path cases. Each one fails against the
// v0.10.0 write path — internal/buffer.WriteBuffer wired to the adapter's flush callback — and passes
// against [vfs.Writer]. The comparison is not hypothetical: internal/difftest.Legacy wires the old
// path to a real backend, and internal/difftest's oracle asserts these same divergences are found.
//
// They use a fake backend rather than the substrate emulator because what is under test is the write
// path's arithmetic and its flush protocol, and a fake makes the S3 traffic assertable: "did this
// flush issue a ranged GET, and for which range" is the difference between read-modify-write and
// silent truncation, and it is invisible if the only observable is the final object.
// internal/difftest covers the same properties against a real S3 endpoint.

// fakeBackend is an in-memory [types.Backend] that records every call.
type fakeBackend struct {
	mu      sync.Mutex
	objects map[string][]byte

	// meta holds each object's user metadata, kept as a real store rather than discarded. A fake that
	// accepted metadata and dropped it would agree with every caller about attributes being written
	// while nothing was — which is the shape of the defect the attribute path exists to fix, so the
	// fake has to be able to expose it.
	meta  map[string]map[string]string
	calls []string

	// putErr, when set, fails every PutObject. This is M22: a rejected upload must surface at
	// close(2) rather than incrementing a counter nobody reads.
	putErr error

	// setMetaErr fails SetObjectMetadata, which is the attribute-only write path.
	setMetaErr error

	// setMetaSilentlyIgnores makes SetObjectMetadata return success and store nothing. This is not a
	// hypothetical: S3 has no metadata-update operation, so the real implementation is a self-copy with
	// MetadataDirective=REPLACE, and an endpoint that does not implement the directive answers 200 while
	// carrying the source object's metadata forward (scttfrdmn/substrate#435). The write path has to
	// catch that, because "chmod reports success and does nothing" is invisible from the caller's side.
	setMetaSilentlyIgnores bool

	// canonicalizeMetaKeys makes HeadObject return metadata keys title-cased, the way a Go http.Header
	// round trip and MinIO both do. Real S3 lower-cases them. A read-back that compares keys
	// case-sensitively passes against one and fails against the other, which is the seam shape this
	// whole audit was about, so the fake can produce both.
	canonicalizeMetaKeys bool

	// headErr and getErr fail HeadObject and GetObject with something that is not an absence. They
	// exist to prove the write path distinguishes "this object does not exist" from "I could not find
	// out", which v0.10.0 did not: it read every HeadObject failure as absence.
	headErr error
	getErr  error

	// headSize, when non-nil, overrides the size HeadObject reports, simulating a backend that
	// stored something other than what it was handed.
	headSize *int64

	// onPut runs after a successful PutObject, while no lock is held. It is how a test injects a
	// write that races an in-flight flush.
	onPut func()
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		objects: make(map[string][]byte),
		meta:    make(map[string]map[string]string),
	}
}

func (f *fakeBackend) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

// Calls returns the recorded call log.
func (f *fakeBackend) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Object returns the stored bytes for key.
func (f *fakeBackend) Object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	return append([]byte(nil), data...), ok
}

// Meta returns the stored user metadata for key.
func (f *fakeBackend) Meta(key string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.meta[key]))
	maps.Copy(out, f.meta[key])
	return out
}

// Put stores an object directly, bypassing the recording, to set up a test's initial state.
func (f *fakeBackend) Put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), data...)
}

// PutWithMeta is [fakeBackend.Put] for an object that already carries attributes, which is how a test
// sets up a file that exists in storage owned by somebody.
func (f *fakeBackend) PutWithMeta(key string, data []byte, meta map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), data...)
	f.meta[key] = meta
}

func (f *fakeBackend) GetObject(_ context.Context, key string, offset, size int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("GET %s [%d,%d)", key, offset, offset+size)

	if f.getErr != nil {
		return nil, f.getErr
	}

	data, ok := f.objects[key]
	if !ok {
		return nil, errNotFound
	}
	if offset >= int64(len(data)) {
		return nil, nil
	}
	end := int64(len(data))
	if size >= 0 && offset+size < end {
		end = offset + size
	}
	return append([]byte(nil), data[offset:end]...), nil
}

func (f *fakeBackend) PutObject(_ context.Context, key string, data []byte, meta map[string]string) error {
	f.mu.Lock()
	f.record("PUT %s (%d bytes, %d meta)", key, len(data), len(meta))
	if f.putErr != nil {
		err := f.putErr
		f.mu.Unlock()
		return err
	}
	f.objects[key] = append([]byte(nil), data...)
	// A PUT replaces metadata wholesale, as S3's does. Merging instead would hide a caller that
	// forgot to carry the attributes forward.
	f.meta[key] = meta
	onPut := f.onPut
	f.mu.Unlock()

	if onPut != nil {
		onPut()
	}
	return nil
}

func (f *fakeBackend) SetObjectMetadata(_ context.Context, key string, meta map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("SETMETA %s (%d meta)", key, len(meta))

	if f.setMetaErr != nil {
		return f.setMetaErr
	}
	if _, ok := f.objects[key]; !ok {
		// Absence, not a generic failure: the attribute path must treat "no object yet" as a legal
		// state and keep the attributes pending rather than failing close(2).
		return errNotFound
	}
	if f.setMetaSilentlyIgnores {
		return nil
	}
	f.meta[key] = meta
	return nil
}

func (f *fakeBackend) HeadObject(_ context.Context, key string) (*types.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("HEAD %s", key)

	if f.headErr != nil {
		return nil, f.headErr
	}

	data, ok := f.objects[key]
	if !ok {
		return nil, errNotFound
	}
	size := int64(len(data))
	if f.headSize != nil {
		size = *f.headSize
	}
	meta := make(map[string]string, len(f.meta[key]))
	for k, v := range f.meta[key] {
		if f.canonicalizeMetaKeys {
			k = http.CanonicalHeaderKey(k)
		}
		meta[k] = v
	}
	return &types.ObjectInfo{
		Key:      key,
		Size:     size,
		ETag:     fmt.Sprintf("etag-%d", len(data)),
		Metadata: meta,
	}, nil
}

func (f *fakeBackend) DeleteObject(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *fakeBackend) GetObjects(context.Context, []string) (map[string][]byte, error) {
	return nil, errors.New("not used")
}
func (f *fakeBackend) PutObjects(context.Context, map[string][]byte) error {
	return errors.New("not used")
}
func (f *fakeBackend) ListObjects(context.Context, string, int) ([]types.ObjectInfo, error) {
	return nil, errors.New("not used")
}
func (f *fakeBackend) HealthCheck(context.Context) error { return nil }

// errNotFound is shaped like the S3 backend's not-found error: an ErrorCode method, which is what
// [vfs.IsNotFound] matches on.
var errNotFound = notFoundError{}

type notFoundError struct{}

func (notFoundError) Error() string     { return "NoSuchKey: the specified key does not exist" }
func (notFoundError) ErrorCode() string { return "NoSuchKey" }

// newWriter returns a Writer over a fresh fake backend.
func newWriter(t *testing.T) (*vfs.Writer, *fakeBackend) {
	t.Helper()

	backend := newFakeBackend()
	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	return w, backend
}

// TestWriteAtOffsetDoesNotTruncateTheObject is audit finding H7, the write path's worst defect.
//
// The v0.10.0 flush callback took an offset and discarded it:
//
//	flushCallback := func(key string, data []byte, offset int64) error {
//	    return a.backend.PutObject(context.Background(), key, data)
//	}
//
// PutObject replaces the whole object, so a write at a non-zero offset replaced the file with only
// the bytes written — and reported success. Both cases below are from the audit; the second is the
// one that shows the scale, because a 1 MiB file becoming 1 byte is not a partial failure.
func TestWriteAtOffsetDoesNotTruncateTheObject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		initial string
		writes  []struct {
			offset int64
			data   string
		}
		want string
	}{
		{
			// The plan's first decisive case: two sequential writes must append, not replace.
			// v0.10.0 produced "BBBB".
			name: "two sequential writes append",
			writes: []struct {
				offset int64
				data   string
			}{
				{0, "AAAA"},
				{4, "BBBB"},
			},
			want: "AAAABBBB",
		},
		{
			// The plan's second case, scaled down: one byte at the end of an existing object.
			// v0.10.0 left a 1-byte object where there had been a full one.
			name:    "one byte appended to an existing object",
			initial: strings.Repeat("o", 1024),
			writes: []struct {
				offset int64
				data   string
			}{
				{1024, "X"},
			},
			want: strings.Repeat("o", 1024) + "X",
		},
		{
			// A write into the middle of an existing object: the bytes on both sides must survive.
			// This is the read-modify-write case proper — it cannot be answered without fetching.
			name:    "write into the middle of an existing object",
			initial: "0123456789",
			writes: []struct {
				offset int64
				data   string
			}{
				{4, "ab"},
			},
			want: "0123ab6789",
		},
		{
			// A sparse write past the end. The gap is a hole, which POSIX says reads as zeros.
			// v0.10.0 rejected this outright with EIO — see TestSparseWriteIsNotRefused.
			name: "sparse write leaves a zero-filled hole",
			writes: []struct {
				offset int64
				data   string
			}{
				{0, "AB"},
				{6, "CD"},
			},
			want: "AB\x00\x00\x00\x00CD",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, backend := newWriter(t)
			const key = "file"

			if tt.initial != "" {
				backend.Put(key, []byte(tt.initial))
			}

			for _, wr := range tt.writes {
				if err := w.Write(key, wr.offset, []byte(wr.data)); err != nil {
					t.Fatalf("Write at %d: %v", wr.offset, err)
				}
			}

			if err := w.Flush(key); err != nil {
				t.Fatalf("Flush: %v", err)
			}

			got, ok := backend.Object(key)
			if !ok {
				t.Fatal("the object does not exist after a flush that reported success")
			}
			if string(got) != tt.want {
				t.Errorf("the flushed object is wrong, which is data loss, not a formatting issue\n"+
					" got: %q (%d bytes)\nwant: %q (%d bytes)\ncalls: %v",
					got, len(got), tt.want, len(tt.want), backend.Calls())
			}
		})
	}
}

// TestSparseWriteIsNotRefused is audit finding H8.
//
// v0.10.0's canBufferWrite rejected any write that did not continue its single contiguous buffer,
// returning EIO. The rejected pattern — a header at offset 0, then a page much further in — is what
// SQLite, mmap writeback, tar, and HDF5 all do, so those tools could not write through the
// filesystem at all. The plan's case is verbatim: pwrite(hdr,0) then pwrite(page,65536).
func TestSparseWriteIsNotRefused(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	const key = "db.sqlite"

	header := []byte("SQLite format 3\x00")
	page := []byte(strings.Repeat("p", 4096))

	if err := w.Write(key, 0, header); err != nil {
		t.Fatalf("writing the header failed, which is H8 — v0.10.0 returned EIO here: %v", err)
	}
	if err := w.Write(key, 65536, page); err != nil {
		t.Fatalf("writing a page at 65536 failed, which is H8: a filesystem that refuses a "+
			"non-contiguous write cannot host SQLite, tar, HDF5, or mmap writeback: %v", err)
	}

	if err := w.Flush(key); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, ok := backend.Object(key)
	if !ok {
		t.Fatal("no object after flush")
	}

	wantSize := int64(65536 + len(page))
	if int64(len(got)) != wantSize {
		t.Fatalf("object is %d bytes, want %d", len(got), wantSize)
	}
	if !strings.HasPrefix(string(got), string(header)) {
		t.Errorf("the header is not at offset 0: got %q", got[:len(header)])
	}
	if string(got[65536:]) != string(page) {
		t.Error("the page at 65536 is not intact")
	}
	for i := len(header); i < 65536; i++ {
		if got[i] != 0 {
			t.Fatalf("byte %d of the hole is %#x, want 0: a hole must read as zeros", i, got[i])
		}
	}
}

// TestShorterOverwriteReplacesLongerContent is the mergeWrites defect.
//
// v0.10.0's WriteCoalescer.mergeWrites guarded its overlay with "is the new end past the current
// end", so writing shorter new content over longer old content kept the old content. The plan's case
// is `echo NEW > f` over a file holding OLD, which left the file reading OLD with no error anywhere.
//
// Two spellings of it, because they fail differently: overlapping buffered writes exercise
// [vfs.ExtentList.Add]'s last-writer-wins, while truncate-then-write exercises the truncation state
// that stops a flush re-fetching bytes the truncation destroyed.
func TestShorterOverwriteReplacesLongerContent(t *testing.T) {
	t.Parallel()

	t.Run("shorter write over a longer buffered write", func(t *testing.T) {
		t.Parallel()

		w, backend := newWriter(t)
		const key = "f"

		if err := w.Write(key, 0, []byte("OLDCONTENT")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Write(key, 0, []byte("NEW")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Flush(key); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		// Only the first three bytes were rewritten, so the rest of the old content remains — that is
		// correct POSIX behavior for a plain pwrite and is not the defect.
		got, _ := backend.Object(key)
		if string(got) != "NEWCONTENT" {
			t.Errorf("later write did not win on overlap: got %q, want %q", got, "NEWCONTENT")
		}
	})

	t.Run("truncate then write, as a shell redirect does", func(t *testing.T) {
		t.Parallel()

		w, backend := newWriter(t)
		const key = "f"
		backend.Put(key, []byte("OLDCONTENT"))

		// `> f` is O_TRUNC then write. This is the case v0.10.0 could not express at all: it had no
		// truncate anywhere, so the file could only ever grow.
		if err := w.Truncate(context.Background(), key, 0); err != nil {
			t.Fatalf("Truncate: %v", err)
		}
		if err := w.Write(key, 0, []byte("NEW")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Flush(key); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		got, _ := backend.Object(key)
		if string(got) != "NEW" {
			t.Errorf("the file still holds old content after `> f`: got %q, want %q\n"+
				"this is the mergeWrites defect: `echo NEW > f` over OLD read OLD", got, "NEW")
		}
	})
}

// TestReadAfterWriteSeesTheWrite is audit finding H5.
//
// v0.10.0's read path consulted the cache and the backend and never the write buffer, so a read after
// a write on the same descriptor returned pre-write bytes for up to the cache's five-minute TTL. A
// read fully covered by pending writes must also issue no GET at all, which the call log asserts:
// correctness and the absence of a network round trip are the same property here.
func TestReadAfterWriteSeesTheWrite(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	w, backend := newWriter(t)
	const key = "f"
	backend.Put(key, []byte("OLDDATA"))

	if err := w.Write(key, 0, []byte("NEWDATA")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	buf := make([]byte, 7)
	n, err := w.ReadAt(ctx, key, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if got := string(buf[:n]); got != "NEWDATA" {
		t.Errorf("read after write returned %q, want %q: v0.10.0 returned the pre-write bytes here "+
			"for up to five minutes", got, "NEWDATA")
	}

	for _, c := range backend.Calls() {
		if strings.HasPrefix(c, "GET") {
			t.Errorf("a read fully covered by pending writes issued %q; it should need no network", c)
		}
	}
}

// TestSizeIncludesPendingWrites is the Getattr half of the same defect.
//
// v0.10.0's Getattr read the object's metadata and never consulted the write buffer, so a file being
// written reported its pre-write size — and the kernel truncated reads of it at that offset, which
// makes the data unreachable even though it is buffered correctly.
func TestSizeIncludesPendingWrites(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	w, backend := newWriter(t)
	const key = "f"
	backend.Put(key, []byte("0123456789"))

	if err := w.Write(key, 10, []byte("abcdef")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	size, err := w.FileSize(ctx, key)
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != 16 {
		t.Errorf("FileSize = %d, want 16: a stat that ignores pending writes makes the kernel "+
			"truncate reads at the old length", size)
	}
}

// TestFlushReportsUploadFailure is audit finding M22, and it is the one that makes close(2) mean
// something.
//
// v0.10.0's FlushWithContext scheduled a background flush and returned nil. A PUT rejected for
// AccessDenied incremented stats.Errors and nothing else, so close(2) returned success on data that
// was never stored. The plan's gate is that the error name reaches the caller: "sync timeout" or a
// bare EIO sends an operator to the wrong subsystem.
func TestFlushReportsUploadFailure(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	const key = "f"

	backend.mu.Lock()
	backend.putErr = errors.New("AccessDenied: User is not authorized to perform s3:PutObject")
	backend.mu.Unlock()

	if err := w.Write(key, 0, []byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	err := w.Flush(key)
	if err == nil {
		t.Fatal("Flush returned nil after every PutObject was rejected: this is M22, and it means " +
			"close(2) reports success on data that was never stored")
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("the error does not name the cause: %v\nan operator given "+
			"\"sync timeout\" or a bare EIO goes looking in the wrong subsystem", err)
	}

	// And the node must still be dirty: a failed flush that clears the pending state has converted a
	// recoverable error into data loss.
	if !w.Dirty(key) {
		t.Error("the key is not dirty after a failed flush, so the buffered write has been discarded")
	}
}

// TestFlushDoesNotDropAWriteThatRacedTheUpload is the D9-adjacent defect: v0.10.0's flush deleted the
// buffer on success without rechecking whether anything had arrived during the upload, so a write
// concurrent with a flush was discarded and accounted as flushed.
//
// The race is made deterministic by injecting the write from inside PutObject, which is precisely the
// window that matters — after the body was assembled, before the pending state is cleared.
func TestFlushDoesNotDropAWriteThatRacedTheUpload(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	const key = "f"

	if err := w.Write(key, 0, []byte("AAAA")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Inject exactly one write during the first upload. Once, or the flush would never converge.
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

	if err := w.Flush(key); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, _ := backend.Object(key)
	if string(got) != "AAAABBBB" {
		t.Errorf("a write that landed during the upload was lost: got %q, want %q\n"+
			"v0.10.0 deleted the buffer on success without rechecking, so this write was discarded "+
			"and counted as flushed", got, "AAAABBBB")
	}
	if w.Dirty(key) {
		t.Error("the key is still dirty after a flush that reported success")
	}
}

// TestFlushFailsWhenTheBackendStoredADifferentSize is the read-back check.
//
// A backend that stores fewer bytes than it was handed is silent corruption, and it is the one
// failure a write path cannot detect by looking at itself. v0.10.0's compressed-upload path stored
// objects that HeadObject then described with a different size entirely. Reporting success here would
// mean the pending state is cleared against an object that does not hold the data.
func TestFlushFailsWhenTheBackendStoredADifferentSize(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	const key = "f"

	short := int64(2)
	backend.mu.Lock()
	backend.headSize = &short
	backend.mu.Unlock()

	if err := w.Write(key, 0, []byte("AAAA")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	err := w.Flush(key)
	if err == nil {
		t.Fatal("Flush reported success though the stored object is a different size than was uploaded")
	}
	if !errors.Is(err, vfs.ErrIntegrity) {
		t.Errorf("error is not ErrIntegrity, so a caller cannot distinguish corruption from a "+
			"transient fault: %v", err)
	}
	if !w.Dirty(key) {
		t.Error("the key is not dirty after a failed flush")
	}
}

// TestFlushOfACleanKeyIssuesNoUpload pins the Noop arm of the plan.
//
// [vfs.FlushPlan] distinguishes Noop from WholeObject deliberately, and conflating them destroys
// data: a caller treating "not a whole-object write" as "splice and PUT" would, for an empty plan,
// upload zero bytes over an intact object. fsync on a clean file and a second close(2) both take this
// path, and both are common.
func TestFlushOfACleanKeyIssuesNoUpload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	w, backend := newWriter(t)
	const key = "f"
	backend.Put(key, []byte("intact"))

	// Touch the key so a node exists, without dirtying it.
	if _, err := w.FileSize(ctx, key); err != nil {
		t.Fatalf("FileSize: %v", err)
	}

	if err := w.Flush(key); err != nil {
		t.Fatalf("Flush of a clean key: %v", err)
	}
	if err := w.Flush(key); err != nil {
		t.Fatalf("second Flush: %v", err)
	}

	for _, c := range backend.Calls() {
		if strings.HasPrefix(c, "PUT") {
			t.Errorf("flushing a clean key issued %q: an empty plan treated as a whole-object write "+
				"uploads zero bytes over an intact object", c)
		}
	}
	if got, _ := backend.Object(key); string(got) != "intact" {
		t.Errorf("the object changed: got %q", got)
	}
}

// TestFlushFetchesOnlyTheRangesItNeeds is the performance half of read-modify-write, which the
// project ranks a close second to integrity.
//
// A correct RMW implementation could fetch the whole object every time and still be correct. That
// would reintroduce audit finding C4's cost — a 4 KiB read of a 256 MiB object amplified 216× — on
// the write path. The assertion is on the recorded ranges, not on latency, because bytes transferred
// is the thing that is billed.
func TestFlushFetchesOnlyTheRangesItNeeds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		stored    int
		writes    []struct{ offset, length int }
		wantGets  []string
		wantNoGet bool
	}{
		{
			// Overwriting the whole object determines every byte, so nothing needs fetching.
			name:      "whole-object overwrite fetches nothing",
			stored:    100,
			writes:    []struct{ offset, length int }{{0, 100}},
			wantNoGet: true,
		},
		{
			// A write past the end determines the tail; the head must be fetched.
			name:     "append fetches only the head",
			stored:   100,
			writes:   []struct{ offset, length int }{{100, 10}},
			wantGets: []string{"GET f [0,100)"},
		},
		{
			// A write in the middle leaves a gap on each side.
			name:     "middle write fetches both sides",
			stored:   100,
			writes:   []struct{ offset, length int }{{40, 20}},
			wantGets: []string{"GET f [0,40)", "GET f [60,100)"},
		},
		{
			// Two writes with a gap between them: three ranges, not one whole object.
			name:     "two writes fetch three ranges",
			stored:   100,
			writes:   []struct{ offset, length int }{{10, 10}, {50, 10}},
			wantGets: []string{"GET f [0,10)", "GET f [20,50)", "GET f [60,100)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, backend := newWriter(t)
			const key = "f"
			backend.Put(key, []byte(strings.Repeat("o", tt.stored)))

			for _, wr := range tt.writes {
				if err := w.Write(key, int64(wr.offset), []byte(strings.Repeat("n", wr.length))); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}
			if err := w.Flush(key); err != nil {
				t.Fatalf("Flush: %v", err)
			}

			var gets []string
			for _, c := range backend.Calls() {
				if strings.HasPrefix(c, "GET") {
					gets = append(gets, c)
				}
			}

			if tt.wantNoGet {
				if len(gets) != 0 {
					t.Errorf("expected no GET, got %v: the pending writes determine every byte", gets)
				}
				return
			}

			if len(gets) != len(tt.wantGets) {
				t.Fatalf("GETs = %v, want %v", gets, tt.wantGets)
			}
			for i, want := range tt.wantGets {
				if gets[i] != want {
					t.Errorf("GET %d = %q, want %q\nall: %v", i, gets[i], want, gets)
				}
			}
		})
	}
}

// TestFlushAllReportsEveryFailure covers the unmount path.
//
// FlushAll must attempt every key even after one fails, and say how many failed. Stopping at the
// first error would leave later keys unflushed with no indication which — and this is the path
// unmount takes, the point at which unflushed data is lost for good.
func TestFlushAllReportsEveryFailure(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)

	for _, key := range []string{"a", "b", "c"} {
		if err := w.Write(key, 0, []byte("data")); err != nil {
			t.Fatalf("Write %s: %v", key, err)
		}
	}
	if got := w.Count(); got != 3 {
		t.Fatalf("Count = %d, want 3", got)
	}
	if got := w.Size(); got != 12 {
		t.Errorf("Size = %d, want 12 (3 keys × 4 bytes)", got)
	}

	backend.mu.Lock()
	backend.putErr = errors.New("AccessDenied: not authorized")
	backend.mu.Unlock()

	err := w.FlushAll()
	if err == nil {
		t.Fatal("FlushAll reported success with every upload rejected")
	}
	if !strings.Contains(err.Error(), "3 of 3") {
		t.Errorf("the error does not say how many keys failed: %v\nan unmount that lost three "+
			"files must not report one", err)
	}
}

// TestWriterRejectsMalformedArguments checks the guards that turn a caller bug into an error rather
// than into corruption at offset 0, where corruption is maximally destructive.
func TestWriterRejectsMalformedArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(*vfs.Writer) error
	}{
		{"negative write offset", func(w *vfs.Writer) error { return w.Write("f", -1, []byte("x")) }},
		{"empty key on write", func(w *vfs.Writer) error { return w.Write("", 0, []byte("x")) }},
		{"empty key on flush", func(w *vfs.Writer) error { return w.Flush("") }},
		{"empty key on truncate", func(w *vfs.Writer) error {
			return w.Truncate(context.Background(), "", 0)
		}},
		{"empty key on read", func(w *vfs.Writer) error {
			_, err := w.ReadAt(context.Background(), "", make([]byte, 4), 0)
			return err
		}},
		{"negative truncate size", func(w *vfs.Writer) error {
			return w.Truncate(context.Background(), "f", -1)
		}},
		{"negative read offset", func(w *vfs.Writer) error {
			_, err := w.ReadAt(context.Background(), "f", make([]byte, 4), -1)
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, _ := newWriter(t)
			if err := tt.call(w); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

// TestNewWriterRejectsNilDependencies keeps a nil backend from becoming a nil-pointer panic inside a
// flush, which would kill the mount process and unmount under every open descriptor.
func TestNewWriterRejectsNilDependencies(t *testing.T) {
	t.Parallel()

	if _, err := vfs.NewWriter(context.Background(), nil); err == nil {
		t.Error("NewWriter accepted a nil backend")
	}
	//nolint:staticcheck // passing a nil context is the thing under test
	if _, err := vfs.NewWriter(nil, newFakeBackend()); err == nil {
		t.Error("NewWriter accepted a nil context")
	}
}

// TestConcurrentWritesToOneKeyAllSurvive is the property the differential fuzzer explores and this
// pins cheaply: N goroutines writing disjoint ranges of one file must all reach the object.
//
// One path is one S3 object, so this only works because [vfs.Node] is shared per path rather than
// per handle. If each writer buffered its own ranges, the last flush would replace the object with a
// body assembled from stale bytes and the others' writes would vanish with no error.
func TestConcurrentWritesToOneKeyAllSurvive(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	const (
		key      = "f"
		writers  = 16
		chunkLen = 64
	)

	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			chunk := []byte(strings.Repeat(string(rune('a'+i)), chunkLen))
			if err := w.Write(key, int64(i*chunkLen), chunk); err != nil {
				t.Errorf("write %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	if err := w.Flush(key); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, _ := backend.Object(key)
	if len(got) != writers*chunkLen {
		t.Fatalf("object is %d bytes, want %d", len(got), writers*chunkLen)
	}
	for i := range writers {
		want := strings.Repeat(string(rune('a'+i)), chunkLen)
		if g := string(got[i*chunkLen : (i+1)*chunkLen]); g != want {
			t.Errorf("chunk %d = %q, want %q", i, g, want)
		}
	}
}

// TestIsNotFoundDistinguishesAbsenceFromOtherFailures guards a classifier whose over-generosity
// destroyed data.
//
// v0.10.0's Lookup collapsed every HeadObject error to ENOENT, so a throttle or a permission failure
// read as "file absent" — and Create then wrote an empty object over a file that was merely
// temporarily unreachable. Reporting a live object as absent invites an overwrite; reporting an
// absent object as an error merely fails. The asymmetry is why the negative cases below matter more
// than the positive ones.
func TestIsNotFoundDistinguishesAbsenceFromOtherFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed NoSuchKey", errNotFound, true},
		{"message with status 404", errors.New("request failed, status code: 404"), true},
		{"AccessDenied", errors.New("AccessDenied: not authorized to perform s3:GetObject"), false},
		{"throttled", errors.New("SlowDown: please reduce your request rate"), false},
		{"timeout", errors.New("context deadline exceeded"), false},
		{"no such bucket", errors.New("NoSuchBucket: the specified bucket does not exist"), false},
		{"internal error", errors.New("InternalError: we encountered an internal error"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := vfs.IsNotFound(tt.err); got != tt.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestBackendFailuresSurfaceRatherThanBecomingSilentSuccess covers the paths that reach a failing
// backend from somewhere other than a flush's PUT.
//
// Each of these is a place where returning a zero value with a nil error would be indistinguishable
// from a legitimate answer, and every one of them is a variant of the mistake this package exists to
// undo: v0.10.0 read a HeadObject failure as "the file is absent" and then created an empty object
// over it. A stat that reports 0 bytes because S3 was throttling, or a read that reports 0 bytes of a
// file that has content, is that same bug with a different blast radius.
func TestBackendFailuresSurfaceRatherThanBecomingSilentSuccess(t *testing.T) {
	t.Parallel()

	// headErr fails HeadObject with something that is not an absence, which is what StoredSize must
	// refuse to treat as a zero-length file.
	throttled := errors.New("SlowDown: please reduce your request rate")

	tests := []struct {
		name string
		call func(*vfs.Writer) error
	}{
		{"write cannot size the object", func(w *vfs.Writer) error {
			return w.Write("f", 0, []byte("data"))
		}},
		{"truncate cannot size the object", func(w *vfs.Writer) error {
			return w.Truncate(context.Background(), "f", 4)
		}},
		{"read cannot size the object", func(w *vfs.Writer) error {
			_, err := w.ReadAt(context.Background(), "f", make([]byte, 4), 0)
			return err
		}},
		{"stat cannot size the object", func(w *vfs.Writer) error {
			_, err := w.FileSize(context.Background(), "f")
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backend := newFakeBackend()
			backend.headErr = throttled
			w, err := vfs.NewWriter(context.Background(), backend)
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}

			err = tt.call(w)
			if err == nil {
				t.Fatal("a throttled HEAD was treated as a zero-length file")
			}
			if !errors.Is(err, vfs.ErrBackend) {
				t.Errorf("error is not ErrBackend: %v\nthe classification is what keeps a transient "+
					"failure from being read as absence", err)
			}
		})
	}
}

// TestReadReportsABackendFailureAndToleratesAbsence separates the two ways a ranged GET can fail
// during a read.
//
// They must not be handled alike. An absent object is a legitimate zero-length file — open(2) with
// O_CREAT produces one, and S3 cannot represent it — so a read of it returns the pending writes over
// zeros. Any other failure means the stored bytes exist and could not be fetched, and serving zeros
// there would hand the caller fabricated file content.
func TestReadReportsABackendFailureAndToleratesAbsence(t *testing.T) {
	t.Parallel()

	t.Run("absent object reads as pending writes over zeros", func(t *testing.T) {
		t.Parallel()

		w, _ := newWriter(t)
		if err := w.Write("f", 4, []byte("tail")); err != nil {
			t.Fatalf("Write: %v", err)
		}

		buf := make([]byte, 8)
		n, err := w.ReadAt(context.Background(), "f", buf, 0)
		if err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if got, want := string(buf[:n]), "\x00\x00\x00\x00tail"; got != want {
			t.Errorf("read %q, want %q", got, want)
		}
	})

	t.Run("failed GET is reported", func(t *testing.T) {
		t.Parallel()

		backend := newFakeBackend()
		backend.Put("f", []byte("stored content"))
		w, err := vfs.NewWriter(context.Background(), backend)
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}

		backend.mu.Lock()
		backend.getErr = errors.New("AccessDenied: not authorized to perform s3:GetObject")
		backend.mu.Unlock()

		if _, err := w.ReadAt(context.Background(), "f", make([]byte, 8), 0); err == nil {
			t.Fatal("a rejected GET produced a successful read; the caller would receive zeros as " +
				"file content")
		}
	})
}

// TestCloseFlushesAndReportsWhatItCouldNot covers the last durability point there is.
//
// An unmount that exits silently while a node is dirty loses data with no record of which file, so
// Close must both flush and report. The failure arm matters more than the success one: it is the only
// signal a user gets that a file they wrote never reached storage.
func TestCloseFlushesAndReportsWhatItCouldNot(t *testing.T) {
	t.Parallel()

	t.Run("flushes pending writes", func(t *testing.T) {
		t.Parallel()

		w, backend := newWriter(t)
		if err := w.Write("f", 0, []byte("written but never explicitly flushed")); err != nil {
			t.Fatalf("Write: %v", err)
		}

		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		got, ok := backend.Object("f")
		if !ok {
			t.Fatal("Close returned success without uploading anything")
		}
		if string(got) != "written but never explicitly flushed" {
			t.Errorf("stored %q", got)
		}
	})

	t.Run("reports a failed flush", func(t *testing.T) {
		t.Parallel()

		w, backend := newWriter(t)
		if err := w.Write("f", 0, []byte("data")); err != nil {
			t.Fatalf("Write: %v", err)
		}

		backend.mu.Lock()
		backend.putErr = errors.New("AccessDenied: not authorized")
		backend.mu.Unlock()

		if err := w.Close(); err == nil {
			t.Fatal("Close reported success with the upload rejected")
		}
	})
}

// TestWriteOfNoBytesIsANoop pins the zero-length case, which the kernel does issue.
//
// It must not create a node: doing so would make a zero-byte write(2) enough to mark a file dirty and
// have unmount PUT it, replacing whatever is stored with the same bytes for no reason — or, if the
// stored size were misjudged, with fewer.
func TestWriteOfNoBytesIsANoop(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	if err := w.Write("f", 0, nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := w.Count(); got != 0 {
		t.Errorf("Count = %d, want 0: a zero-length write created a node", got)
	}
	if _, err := w.ReadAt(context.Background(), "f", nil, 0); err != nil {
		t.Errorf("ReadAt with an empty buffer: %v", err)
	}
	if calls := backend.Calls(); len(calls) != 0 {
		t.Errorf("a no-op write issued backend calls: %v", calls)
	}
}

// TestFlushAllOfASingleFailureDoesNotSayTwoOfThree pins the arm that reports one failed key.
//
// FlushAll wraps its error with a count only when more than one key failed, so a single failure
// surfaces the backend's own message. That matters at close(2): "AccessDenied" tells a user what to
// fix, and "1 of 1 keys failed" tells them nothing they did not already know.
func TestFlushAllOfASingleFailureDoesNotSayTwoOfThree(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	if err := w.Write("only", 0, []byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	backend.mu.Lock()
	backend.putErr = errors.New("AccessDenied: not authorized")
	backend.mu.Unlock()

	err := w.FlushAll()
	if err == nil {
		t.Fatal("FlushAll reported success with the only upload rejected")
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("the backend's message did not survive: %v", err)
	}
	if strings.Contains(err.Error(), "of 1") {
		t.Errorf("a single failure was wrapped in a count: %v", err)
	}
}

// TestFlushDoesNotConvergeWhenWritesNeverStop pins the retry bound.
//
// The flush protocol reuploads when a write lands between the plan and the PUT, because the bytes it
// sent no longer describe the file. That loop has to be bounded: a generation that keeps moving means
// writers are arriving faster than an upload completes, and spinning forever inside close(2) is worse
// than reporting that the flush did not converge — a caller can retry, but it cannot recover from a
// syscall that never returns.
//
// The error must say so rather than reporting success. A flush that gave up is a flush whose data is
// not durable.
func TestFlushDoesNotConvergeWhenWritesNeverStop(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	const key = "f"

	if err := w.Write(key, 0, []byte("first")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Every upload is followed by another write, so the generation has always moved by the time
	// MarkFlushed is consulted. This is a pathological writer, not a realistic one — the point is that
	// the loop terminates.
	var n int
	backend.mu.Lock()
	backend.onPut = func() {
		n++
		if err := w.Write(key, int64(n*8), []byte("racing!")); err != nil {
			t.Errorf("racing write %d: %v", n, err)
		}
	}
	backend.mu.Unlock()

	err := w.Flush(key)
	if err == nil {
		t.Fatal("Flush reported success although every attempt lost the race; close(2) would report " +
			"durability the object does not have")
	}
	if !strings.Contains(err.Error(), "did not converge") {
		t.Errorf("the error does not say the flush gave up: %v", err)
	}
	if n < 2 {
		t.Errorf("only %d attempts were made; the retry loop is not retrying", n)
	}
}

// TestFlushToleratesTheObjectVanishingMidFlush covers a concurrent delete by another S3 client.
//
// A read-modify-write fetches the ranges its pending writes do not cover. If the object is deleted
// between the plan and that fetch, those bytes are gone — the range becomes a hole, which splices as
// zeros. Failing here instead would strand the write: the data the caller handed us would be lost
// because a different client did something it is entitled to do.
func TestFlushToleratesTheObjectVanishingMidFlush(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.Put("f", []byte("0123456789"))

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// A write covering only the tail, so the flush must fetch [0,6) from the object.
	if err := w.Write("f", 6, []byte("TAIL")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Delete it after the node has taken its stored size but before the flush reads the base.
	if err := backend.DeleteObject(context.Background(), "f"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if err := w.Flush("f"); err != nil {
		t.Fatalf("Flush: %v\na concurrent delete stranded the pending write", err)
	}

	got, ok := backend.Object("f")
	if !ok {
		t.Fatal("nothing was uploaded")
	}
	if want := "\x00\x00\x00\x00\x00\x00TAIL"; string(got) != want {
		t.Errorf("object = %q, want %q (the vanished range reads as zeros)", got, want)
	}
}

// TestFlushReportsAFailedRangeFetch is the counterpart to the vanishing-object case above.
//
// An absent object is a hole; a GET that fails for any other reason is not. Treating a throttle or a
// permission failure as a hole would splice zeros into the middle of a file and upload it — silent
// corruption of data that was intact a moment earlier, and the worst outcome available at this point
// in the protocol.
func TestFlushReportsAFailedRangeFetch(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.Put("f", []byte("0123456789"))

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Write("f", 6, []byte("TAIL")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	backend.mu.Lock()
	backend.getErr = errors.New("SlowDown: please reduce your request rate")
	backend.mu.Unlock()

	if err := w.Flush("f"); err == nil {
		t.Fatal("a failed range fetch produced a successful flush; the object would have been " +
			"overwritten with zeros where the fetch failed")
	}

	// And nothing was uploaded: a flush that cannot read the base must not write a partial one.
	got, _ := backend.Object("f")
	if string(got) != "0123456789" {
		t.Errorf("the stored object was modified to %q by a flush that failed", got)
	}
}

// TestStoredSizeRejectsANegativeSize guards against a backend that reports something impossible.
//
// A negative size would flow into the splice arithmetic as a length. There is no correct behavior
// available at that point, so the only safe answer is to refuse — and to classify it as an integrity
// failure rather than a transient one, because retrying will not help.
func TestStoredSizeRejectsANegativeSize(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.Put("f", []byte("content"))
	negative := int64(-1)
	backend.headSize = &negative

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	err = w.Write("f", 0, []byte("x"))
	if err == nil {
		t.Fatal("a negative stored size was accepted")
	}
	if !errors.Is(err, vfs.ErrIntegrity) {
		t.Errorf("error is not ErrIntegrity: %v", err)
	}
}

// TestIsNotFoundRecognizesTheStandardLibrarySentinel covers fs.ErrNotExist.
//
// A types.Backend over a local filesystem returns exactly it — the mock backend in tests/ does — so
// without this arm every such backend would report a missing object as a hard failure. Being a typed
// check it cannot misclassify in the dangerous direction.
func TestIsNotFoundRecognizesTheStandardLibrarySentinel(t *testing.T) {
	t.Parallel()

	if !vfs.IsNotFound(fs.ErrNotExist) {
		t.Error("fs.ErrNotExist is not recognized as absence")
	}
	if !vfs.IsNotFound(fmt.Errorf("open %q: %w", "f", fs.ErrNotExist)) {
		t.Error("a wrapped fs.ErrNotExist is not recognized as absence")
	}
}

// TestFlusherRejectsANilNode keeps a caller bug from becoming a nil dereference inside a flush, which
// would kill the mount process and take every open descriptor with it.
func TestFlusherRejectsANilNode(t *testing.T) {
	t.Parallel()

	f, err := vfs.NewFlusher(newFakeBackend())
	if err != nil {
		t.Fatalf("NewFlusher: %v", err)
	}
	if err := f.Flush(context.Background(), nil); err == nil {
		t.Error("Flush accepted a nil node")
	}
	if _, err := vfs.NewFlusher(nil); err == nil {
		t.Error("NewFlusher accepted a nil backend")
	}
}

// TestFlushFailsWhenTheUploadCannotBeConfirmed covers the read-back step.
//
// The flush reads the stored size back rather than trusting the length it sent, because a backend that
// stores fewer bytes than it was handed is silent corruption — the one failure a write path cannot
// detect by looking at itself. If that confirming HEAD fails, the flush does not know whether the
// object is intact, and reporting success would mean close(2) vouching for something unverified.
func TestFlushFailsWhenTheUploadCannotBeConfirmed(t *testing.T) {
	t.Parallel()

	w, backend := newWriter(t)
	if err := w.Write("f", 0, []byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Break HEAD only once the PUT has landed, so the failure is isolated to the confirmation.
	backend.mu.Lock()
	backend.onPut = func() {
		backend.mu.Lock()
		backend.headErr = errors.New("SlowDown: please reduce your request rate")
		backend.mu.Unlock()
	}
	backend.mu.Unlock()

	err := w.Flush("f")
	if err == nil {
		t.Fatal("a flush that could not confirm its upload reported success")
	}
	if !strings.Contains(err.Error(), "confirm upload") {
		t.Errorf("the error does not identify the confirmation step: %v", err)
	}

	// The data stays pending, so unmount gets another chance at it.
	if !w.Dirty("f") {
		t.Error("an unconfirmed flush dropped the pending write")
	}
}

// TestFlushTreatsAnEmptyRangeFetchAsAHole covers a GET that succeeds with no bytes.
//
// A backend may answer a range that starts at or past the end of the object with an empty body rather
// than an error. That is not corruption and not a failure: those bytes do not exist, so the range is a
// hole and splices as zeros. Appending it as a zero-length extent instead would put an empty extent in
// the splice list, and the arithmetic there assumes extents have content.
func TestFlushTreatsAnEmptyRangeFetchAsAHole(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.Put("f", []byte("0123456789"))

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Take the node's stored size at 10 bytes, then shrink the object so the [0,6) fetch the flush
	// needs returns nothing.
	if err := w.Write("f", 6, []byte("TAIL")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	backend.Put("f", nil)

	if err := w.Flush("f"); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, _ := backend.Object("f")
	if want := "\x00\x00\x00\x00\x00\x00TAIL"; string(got) != want {
		t.Errorf("object = %q, want %q", got, want)
	}
}

// TestReadOfADeletedObjectServesPendingWritesOverZeros covers the absence arm of a read's range fetch.
//
// The node knows the object was ten bytes when it opened, so a read consults storage for the range its
// pending writes do not cover. If the object has since been deleted, those bytes are gone — zeros,
// not an error. Failing would make an unrelated client's delete break a read of data this process
// itself wrote and can still see.
func TestReadOfADeletedObjectServesPendingWritesOverZeros(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.Put("f", []byte("0123456789"))

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if writeErr := w.Write("f", 6, []byte("TAIL")); writeErr != nil {
		t.Fatalf("Write: %v", writeErr)
	}

	if delErr := backend.DeleteObject(context.Background(), "f"); delErr != nil {
		t.Fatalf("DeleteObject: %v", delErr)
	}

	buf := make([]byte, 10)
	n, readErr := w.ReadAt(context.Background(), "f", buf, 0)
	if readErr != nil {
		t.Fatalf("ReadAt: %v", readErr)
	}
	if want := "\x00\x00\x00\x00\x00\x00TAIL"; string(buf[:n]) != want {
		t.Errorf("read %q, want %q", buf[:n], want)
	}
}

// TestAttrOnlyFlushFailsWhenTheBackendStoredNothing is the confirmation step of the attribute path.
//
// S3 has no metadata-update operation. Changing an object's user metadata means a self-copy with
// MetadataDirective=REPLACE, which is a compound operation with a silent no-op mode: an endpoint that
// does not implement the directive answers 200 and carries the *source* object's metadata forward.
// Found against a real endpoint that does exactly this (scttfrdmn/substrate#435).
//
// So a chmod would report success, the next stat would report the old mode, and nothing would connect
// the two. The flush reads the metadata back — the confirming HEAD is already being issued for the
// size check — and refuses to mark the node clean, which keeps the attributes pending for the next
// flush instead of discarding them.
func TestAttrOnlyFlushFailsWhenTheBackendStoredNothing(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.PutWithMeta("f", []byte("contents"), map[string]string{"objectfs-mode": "644"})
	backend.setMetaSilentlyIgnores = true

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err = w.SetAttr(context.Background(), "f", true, false, false,
		vfs.Attr{Mode: fs.FileMode(0o600)}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	err = w.Flush("f")
	if err == nil {
		t.Fatal("a chmod the backend silently discarded reported success; that is a mode that does not " +
			"survive a remount, with nothing to indicate it")
	}
	if !errors.Is(err, vfs.ErrIntegrity) {
		t.Errorf("error is not an integrity failure: %v", err)
	}
	if !strings.Contains(err.Error(), "objectfs-mode") {
		t.Errorf("the error does not name the attribute that did not land: %v", err)
	}

	// The change stays pending. Dropping it would turn a detected failure into the silent one.
	if !w.Dirty("f") {
		t.Error("the unverified attribute change was discarded")
	}
}

// TestAttrOnlyFlushIgnoresMetadataItDoesNotOwn covers the other direction of the same check.
//
// The confirming read-back compares only the keys the flush set. A backend legitimately carries keys
// this layer does not own — the integrity keys objectfs-sha256 and objectfs-original-size, which the
// S3 backend computes and deliberately refuses to let a caller override, plus anything another tool
// put there. Comparing whole maps would make every chmod of a compressed object fail.
func TestAttrOnlyFlushIgnoresMetadataItDoesNotOwn(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.PutWithMeta("f", []byte("contents"), map[string]string{
		"objectfs-sha256":        "deadbeef",
		"objectfs-original-size": "8",
		"some-other-tool":        "value",
	})

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err := w.SetAttr(context.Background(), "f", true, false, false,
		vfs.Attr{Mode: fs.FileMode(0o600)}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	if err := w.Flush("f"); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := backend.Meta("f")["objectfs-mode"]; got != "600" {
		t.Errorf("stored objectfs-mode = %q, want 600", got)
	}
}

// TestAttrOnlyFlushToleratesCaseFoldedMetadataKeys is the case-sensitivity trap.
//
// S3 lower-cases user-metadata keys, MinIO title-cases them, and a Go http.Header round trip
// canonicalises them to Objectfs-Mode. A case-sensitive read-back would therefore pass against a fake
// and report a spurious integrity failure — a chmod that cannot succeed — against real storage. That
// is the exact seam shape this whole audit was about, so it is asserted rather than assumed.
func TestAttrOnlyFlushToleratesCaseFoldedMetadataKeys(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.PutWithMeta("f", []byte("contents"), nil)
	backend.canonicalizeMetaKeys = true

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err := w.SetAttr(context.Background(), "f", true, false, false,
		vfs.Attr{Mode: fs.FileMode(0o600)}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	if err := w.Flush("f"); err != nil {
		t.Fatalf("a backend that title-cases metadata keys failed the read-back: %v", err)
	}
	if w.Dirty("f") {
		t.Error("the node is still dirty after a flush that reported success")
	}
}

// TestWriterAttrReportsPendingChangesAndAbsence covers [vfs.Writer.Attr], which is what a stat asks.
//
// The two returns are the whole contract: an Attr, and whether the write path holds anything for the
// key at all. A false second return is not an error — it means nothing is buffered, so the caller's
// stored metadata is the current answer. Reporting an empty Attr as authoritative instead would make
// a stat of an unopened file report mode 0000, which is the defect that made every directory in
// v0.10.0 untraversable.
func TestWriterAttrReportsPendingChangesAndAbsence(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.PutWithMeta("f", []byte("0123456789"), map[string]string{"objectfs-mode": "644"})

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Nothing buffered yet.
	if _, ok := w.Attr("f"); ok {
		t.Error("Attr claimed to hold state for a key nothing has touched")
	}

	// A pending chmod is visible before it is durable. This is what makes `chmod 600 f && stat f`
	// report 600 rather than the object's still-unchanged metadata.
	if err := w.SetAttr(context.Background(), "f", true, false, false,
		vfs.Attr{Mode: fs.FileMode(0o600)}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	got, ok := w.Attr("f")
	if !ok {
		t.Fatal("Attr reported no state for a key with a pending chmod")
	}
	if got.Mode.Perm() != 0o600 {
		t.Errorf("Attr reports mode %#o, want 0600", got.Mode.Perm())
	}
	if got.Size != 10 {
		t.Errorf("Attr reports size %d, want 10: a chmod must not change the size", got.Size)
	}

	// A pending write moves the size the same way, which is the value the kernel clamps reads to.
	if err := w.Write("f", 10, []byte("MORE")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, _ := w.Attr("f"); got.Size != 14 {
		t.Errorf("Attr reports size %d after appending 4 bytes to a 10-byte file, want 14", got.Size)
	}

	// And after a flush the node is dropped, so Attr goes back to reporting nothing — the object's own
	// metadata is authoritative again.
	if err := w.Flush("f"); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, ok := w.Attr("f"); ok {
		t.Error("Attr still claims state for a key that flushed clean; the node was not released")
	}
}

// TestSetAttrAppliesOnlyTheFieldsTheMaskNames is the FATTR mask, checked in the direction that loses
// data.
//
// A SETATTR request carries a bitmask saying which fields the caller set; the rest hold whatever was
// in the struct, which in practice is zero. Applying them all would turn every `touch` into a chown to
// root and a chmod to 0000. The three booleans carry that mask down to here, which is the one place
// that owns the merge.
func TestSetAttrAppliesOnlyTheFieldsTheMaskNames(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.PutWithMeta("f", []byte("contents"), map[string]string{
		"objectfs-mode": "644",
		"objectfs-uid":  "4242",
		"objectfs-gid":  "4343",
	})

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Mode only. Uid and Gid are zero in the request, as the kernel leaves them.
	if err := w.SetAttr(context.Background(), "f", true, false, false,
		vfs.Attr{Mode: fs.FileMode(0o600), UID: 0, GID: 0}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	got, ok := w.Attr("f")
	if !ok {
		t.Fatal("no pending state after SetAttr")
	}
	if got.Mode.Perm() != 0o600 {
		t.Errorf("mode is %#o, want 0600: the field the mask named was not applied", got.Mode.Perm())
	}
	if got.UID != 4242 || got.GID != 4343 {
		t.Errorf("ownership is %d:%d, want 4242:4343. The request's unset zero fields were applied as "+
			"though the caller had set them, which turns every chmod into a chown to root.",
			got.UID, got.GID)
	}

	// And an mtime rides along on a non-zero value rather than on a boolean, because a zero time is
	// unambiguously "not set" in a way a zero uid is not.
	when := got.Mtime
	if err := w.SetAttr(context.Background(), "f", false, false, false, vfs.Attr{}); err != nil {
		t.Fatalf("SetAttr with nothing set: %v", err)
	}
	if after, _ := w.Attr("f"); !after.Mtime.Equal(when) {
		t.Errorf("a SetAttr with a zero Mtime changed it from %v to %v", when, after.Mtime)
	}
}

// TestTruncateRecordsAPendingResize covers [vfs.Writer.Truncate] in both directions.
//
// v0.10.0 had no truncate anywhere — no Setattr, no Truncate, no O_TRUNC handling — so `> file` could
// not shorten an object. Both directions matter and they fail differently: shortening must not leave
// the tail of the old object behind, and extending must zero-fill rather than leaving a short object
// that a stat describes as long.
func TestTruncateRecordsAPendingResize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		size int64
		want string
	}{
		{name: "shorten", size: 4, want: "0123"},
		{name: "to zero", size: 0, want: ""},
		{name: "extend zero-fills", size: 14, want: "0123456789\x00\x00\x00\x00"},
		{name: "unchanged", size: 10, want: "0123456789"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backend := newFakeBackend()
			backend.Put("f", []byte("0123456789"))

			w, err := vfs.NewWriter(context.Background(), backend)
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}

			if err := w.Truncate(context.Background(), "f", tt.size); err != nil {
				t.Fatalf("Truncate(%d): %v", tt.size, err)
			}

			// The new size is visible before the flush: a stat between truncate(2) and the flush must
			// report the length the file now has, not the one it had.
			if got, _ := w.Attr("f"); got.Size != tt.size {
				t.Errorf("Attr reports size %d after truncating to %d", got.Size, tt.size)
			}

			if err := w.Flush("f"); err != nil {
				t.Fatalf("Flush: %v", err)
			}

			stored, _ := backend.Object("f")
			if string(stored) != tt.want {
				t.Errorf("object = %q, want %q", stored, tt.want)
			}
		})
	}
}

// TestTruncateAndSetAttrRejectAnEmptyKey pins the argument validation on the two entry points the node
// contract added, matching what the others already do.
func TestTruncateAndSetAttrRejectAnEmptyKey(t *testing.T) {
	t.Parallel()

	w, _ := newWriter(t)
	ctx := context.Background()

	if err := w.Truncate(ctx, "", 0); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("Truncate with an empty key: %v, want ErrInvalid", err)
	}
	if err := w.SetAttr(ctx, "", true, false, false, vfs.Attr{}); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("SetAttr with an empty key: %v, want ErrInvalid", err)
	}
}

// TestStoredSizeTreatsAbsenceAsZeroAndReportsOtherFailures covers [vfs.Flusher.StoredSize].
//
// Absence is zero rather than an error because a file open for writing need not exist yet: open(2)
// with O_CREAT produces an empty file, and S3 cannot represent a zero-byte object that has never been
// written. But a HEAD that failed for any other reason is *not* absence, and reporting zero for it
// would make the flush splice against a size it never confirmed — v0.10.0 read every HeadObject
// failure as absence, which is how a throttled request came to look like a missing file.
func TestStoredSizeTreatsAbsenceAsZeroAndReportsOtherFailures(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.Put("here", []byte("0123456789"))

	flusher, err := vfs.NewFlusher(backend)
	if err != nil {
		t.Fatalf("NewFlusher: %v", err)
	}
	ctx := context.Background()

	if got, err := flusher.StoredSize(ctx, "here"); err != nil || got != 10 {
		t.Errorf("StoredSize of a 10-byte object = (%d, %v), want (10, nil)", got, err)
	}

	if got, err := flusher.StoredSize(ctx, "absent"); err != nil || got != 0 {
		t.Errorf("StoredSize of an absent object = (%d, %v), want (0, nil)", got, err)
	}

	backend.mu.Lock()
	backend.headErr = errors.New("SlowDown: please reduce your request rate")
	backend.mu.Unlock()

	if _, err := flusher.StoredSize(ctx, "here"); !errors.Is(err, vfs.ErrBackend) {
		t.Errorf("StoredSize with a failing HEAD: %v, want ErrBackend. Reporting zero would make a "+
			"throttled request indistinguishable from a missing file.", err)
	}

	// A negative size is not a size. It cannot come from S3, but it can come from a backend with a
	// signedness bug, and splicing against it would produce arithmetic nobody checked.
	backend.mu.Lock()
	backend.headErr = nil
	negative := int64(-1)
	backend.headSize = &negative
	backend.mu.Unlock()

	if _, err := flusher.StoredSize(ctx, "here"); !errors.Is(err, vfs.ErrIntegrity) {
		t.Errorf("StoredSize with a negative reported size: %v, want ErrIntegrity", err)
	}
}

// TestAttrOnlyFlushErrorPaths covers each way the metadata-only write can fail.
//
// They are separated because they mean different things to a caller and must not collapse into one
// answer. Absence is legal and keeps the change pending; a rejected request and an unconfirmable one
// both fail, and a size that moved is corruption.
func TestAttrOnlyFlushErrorPaths(t *testing.T) {
	t.Parallel()

	// A chmod on a file that has never been written is legal: open(2) with O_CREAT then fchmod, before
	// anything is flushed. There is no object to carry the attributes, so the change stays pending for
	// the flush that has content, and close(2) must not fail.
	t.Run("absence keeps the change pending", func(t *testing.T) {
		t.Parallel()

		backend := newFakeBackend()
		w, err := vfs.NewWriter(context.Background(), backend)
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}

		if err := w.SetAttr(context.Background(), "new", true, false, false,
			vfs.Attr{Mode: fs.FileMode(0o600)}); err != nil {
			t.Fatalf("SetAttr: %v", err)
		}

		if err := w.Flush("new"); err != nil {
			t.Fatalf("flushing a chmod on a file with no object yet failed: %v", err)
		}
		if !w.Dirty("new") {
			t.Error("the pending mode was discarded; the next flush with content will not carry it")
		}
	})

	// A rejected request — AccessDenied on the copy, most likely — must surface. v0.10.0 incremented a
	// counter and reported success.
	t.Run("a rejected request surfaces", func(t *testing.T) {
		t.Parallel()

		backend := newFakeBackend()
		backend.Put("f", []byte("contents"))
		backend.setMetaErr = errors.New("AccessDenied: user is not authorized to perform s3:PutObject")

		w, err := vfs.NewWriter(context.Background(), backend)
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if err = w.SetAttr(context.Background(), "f", true, false, false,
			vfs.Attr{Mode: fs.FileMode(0o600)}); err != nil {
			t.Fatalf("SetAttr: %v", err)
		}

		err = w.Flush("f")
		if err == nil {
			t.Fatal("a chmod the backend refused reported success")
		}
		if !strings.Contains(err.Error(), "persist attributes") {
			t.Errorf("the error does not identify what failed: %v", err)
		}
		if !w.Dirty("f") {
			t.Error("the refused change was discarded rather than kept for a retry")
		}
	})

	// An unconfirmable one likewise: the request was accepted, but without the read-back this layer does
	// not know what is stored, and vouching for it anyway is what a durability guarantee cannot do.
	t.Run("an unconfirmable write surfaces", func(t *testing.T) {
		t.Parallel()

		backend := newFakeBackend()
		backend.Put("f", []byte("contents"))

		w, err := vfs.NewWriter(context.Background(), backend)
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if err = w.SetAttr(context.Background(), "f", true, false, false,
			vfs.Attr{Mode: fs.FileMode(0o600)}); err != nil {
			t.Fatalf("SetAttr: %v", err)
		}

		// Break HEAD only now, so the node's stored size was already taken and the failure is isolated to
		// the confirmation.
		backend.mu.Lock()
		backend.headErr = errors.New("SlowDown: please reduce your request rate")
		backend.mu.Unlock()

		err = w.Flush("f")
		if err == nil {
			t.Fatal("a chmod that could not be confirmed reported success")
		}
		if !strings.Contains(err.Error(), "confirm attributes") {
			t.Errorf("the error does not identify the confirmation step: %v", err)
		}
	})

	// A size that moved is the corruption case. SetObjectMetadata must not touch the object's bytes, and
	// an implementation that rewrote it — dropping a Content-Encoding, say — shows up here as a length
	// that changed. A chmod that can corrupt a file must not be silent.
	t.Run("a size that moved is an integrity failure", func(t *testing.T) {
		t.Parallel()

		backend := newFakeBackend()
		backend.Put("f", []byte("contents"))

		w, err := vfs.NewWriter(context.Background(), backend)
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if err = w.SetAttr(context.Background(), "f", true, false, false,
			vfs.Attr{Mode: fs.FileMode(0o600)}); err != nil {
			t.Fatalf("SetAttr: %v", err)
		}

		backend.mu.Lock()
		moved := int64(3)
		backend.headSize = &moved
		backend.mu.Unlock()

		err = w.Flush("f")
		if !errors.Is(err, vfs.ErrIntegrity) {
			t.Fatalf("a chmod that changed the object's length: %v, want ErrIntegrity", err)
		}
		if !strings.Contains(err.Error(), "size changed") {
			t.Errorf("the error does not say the size moved: %v", err)
		}
	})
}

// TestNodeCreationFailurePropagates covers the one path every writer entry point shares.
//
// Each of Write, Truncate, SetAttr, ReadAt, and FileSize begins by resolving the key to a node, which
// on first use HEADs the object for its stored attributes. If that fails for anything but absence, the
// write path does not know the size it would splice against or the ownership it would preserve, and
// proceeding would mean guessing at both. Every entry point has to report it, not just the one that
// happened to be tested.
func TestNodeCreationFailurePropagates(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.headErr = errors.New("SlowDown: please reduce your request rate")

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	ctx := context.Background()

	calls := map[string]func() error{
		"Write":    func() error { return w.Write("f", 0, []byte("x")) },
		"Truncate": func() error { return w.Truncate(ctx, "f", 0) },
		"SetAttr": func() error {
			return w.SetAttr(ctx, "f", true, false, false, vfs.Attr{Mode: fs.FileMode(0o600)})
		},
		"ReadAt":   func() error { _, err := w.ReadAt(ctx, "f", make([]byte, 4), 0); return err },
		"FileSize": func() error { _, err := w.FileSize(ctx, "f"); return err },
	}

	for name, call := range calls {
		if err := call(); !errors.Is(err, vfs.ErrBackend) {
			t.Errorf("%s with a failing HEAD returned %v, want ErrBackend. Treating it as absence would "+
				"make the flush splice against a size it never confirmed.", name, err)
		}
	}
}

// TestSetAttrRejectsModeBitsOutsideThePermissionMask pins the refusal at the layer that owns Attr.
//
// vfs.Attr.Mode is documented as permission bits only, so setuid, setgid, and sticky have nowhere to be
// stored. The FUSE shim refuses them too, with ENOTSUP — but this is the layer that would have to
// persist them, so the invariant belongs here as well rather than only at the boundary that happens to
// be in front of it today.
func TestSetAttrRejectsModeBitsOutsideThePermissionMask(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	backend.Put("f", []byte("contents"))

	w, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	err = w.SetAttr(context.Background(), "f", true, false, false,
		vfs.Attr{Mode: fs.ModeSetuid | 0o755})
	if !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("SetAttr with a setuid bit: %v, want ErrInvalid. Accepting it would promise an "+
			"escalation this filesystem cannot perform.", err)
	}
}
