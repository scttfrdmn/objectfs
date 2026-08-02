package s3_test

// Audit findings D14 and M13: the parallel read path — the one that handles the largest objects —
// was the only S3 path in the backend with no retry, no circuit breaker, and no health tracking,
// and it assembled whatever bytes came back without checking that they added up.
//
// Every test here asserts on what the endpoint actually served, not on what the backend believes it
// did. That distinction is the whole reason this file exists: the defect was a fan-out that looked
// correct from inside the backend and was missing its reliability stack from the outside, and the
// only witness to the difference is the request log.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	objerrors "github.com/scttfrdmn/objectfs/pkg/errors"
	"github.com/scttfrdmn/objectfs/pkg/health"
)

// parallelReadConfig is the fan-out configuration these tests share: 1 MiB chunks over an 8 MiB
// object, so a read fans out into exactly eight GETs and a per-chunk assertion has something to
// point at.
const (
	parallelObjectSize = 8 << 20
	parallelChunkSize  = 1 << 20
	parallelChunks     = parallelObjectSize / parallelChunkSize
)

func parallelReadConfig(cfg *s3.Config) {
	cfg.ParallelReadThreshold = parallelChunkSize
	cfg.ReadChunkSize = parallelChunkSize
	cfg.ParallelReadConcurrency = 4

	// Compression gates the parallel path off entirely — a whole-object decompress cannot be
	// assembled from independent ranges — so it must be off for any of this to run.
	cfg.Compression.Enabled = false
}

// TestParallelReadRetriesATransientChunkFailure is the D14 regression test.
//
// One chunk of an eight-chunk read fails once with a retryable 500. The read must still return all
// 8 MiB, correct byte for byte, because the retryer the serial path has always had now covers this
// path too. On v0.10.0 the whole read failed: parallelGetObject called
// executeWithAccelerationFallback directly, so a single transient error on any chunk of a
// multi-gigabyte read failed the entire read that one retry would have completed.
//
// The fault has to fire for this to prove anything — a matcher that matches nothing produces an
// identical passing test — so FaultsFired is asserted alongside the bytes.
func TestParallelReadRetriesATransientChunkFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		rangePrefix string
		why         string
	}{
		{
			name:        "the first chunk",
			rangePrefix: "bytes=0-",
			why:         "chunk zero is the one whose result the assembly starts from",
		},
		{
			name:        "an interior chunk",
			rangePrefix: fmt.Sprintf("bytes=%d-", 3*parallelChunkSize),
			why: "an interior failure is the case where a wrong fix produces a buffer of the right " +
				"length with a hole in the middle",
		},
		{
			name:        "the final chunk",
			rangePrefix: fmt.Sprintf("bytes=%d-", (parallelChunks-1)*parallelChunkSize),
			why:         "the last chunk is the short one whenever the size is not a multiple",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			ts.RequireRangeGET()

			const key = "parallel/retry"
			want := testaws.DeterministicBytes(key, parallelObjectSize)
			ts.PutObject(key, want)
			ts.ResetRequests()

			backend := ts.Backend(parallelReadConfig)

			ts.InjectFault(testaws.Fault{
				Method:      "GET",
				KeySuffix:   key,
				RangePrefix: tc.rangePrefix,
				Times:       1,
			})

			got, err := backend.GetObject(context.Background(), key, 0, parallelObjectSize)
			if err != nil {
				t.Fatalf("GetObject failed after one transient chunk failure: %v\nThe serial path has "+
					"always retried this; before v0.10.1 the parallel path did not, so %s failing once "+
					"failed a whole multi-gigabyte read.", err, tc.name)
			}

			if n := ts.FaultsFired(); n != 1 {
				t.Fatalf("the injected fault fired %d times, want 1 — the Range matcher %q did not match "+
					"the chunk it was aimed at, so this test proved nothing about retry",
					n, tc.rangePrefix)
			}

			assertBytesEqual(t, got, want, tc.why)
		})
	}
}

// TestParallelReadHealthSurvivesOneFailedRead is the other half of D14: the reliability stack has to
// reach this path *without* one failed read taking the component down.
//
// The tension is real. Routing eight chunks through the health tracker means eight RecordError calls
// for one root cause, and s3-reads degrades at three consecutive errors — so the naive fix makes a
// single failed read of a large object mark reads degraded and start refusing them. The resolution
// is that a chunk canceled because a sibling failed is reported as ErrCodeOperationCanceled, which
// errors.IsServiceFailure classifies as a non-failure, so health.RecordError heals on it.
//
// A fault with no retry budget left is the way to produce one genuinely failed read: MaxRetries is 2
// in the harness config, so failing three times exhausts it.
func TestParallelReadHealthSurvivesOneFailedRead(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const key = "parallel/health"
	ts.PutObject(key, testaws.DeterministicBytes(key, parallelObjectSize))
	ts.ResetRequests()

	backend := ts.Backend(parallelReadConfig)

	// AccessDenied is not retryable, so this fails the read on the first attempt rather than after
	// the retry budget — which is the case that matters here: the fewest possible real failures,
	// against a threshold of three.
	ts.InjectFault(testaws.Fault{
		Method:      "GET",
		KeySuffix:   key,
		RangePrefix: fmt.Sprintf("bytes=%d-", 2*parallelChunkSize),
		Status:      403,
		Code:        "AccessDenied",
		Times:       1,
	})

	if _, err := backend.GetObject(context.Background(), key, 0, parallelObjectSize); err == nil {
		t.Fatal("GetObject succeeded with a chunk permanently failing; a read that cannot fetch part " +
			"of the range must fail rather than return a short buffer")
	}

	got, err := backend.GetComponentHealth("s3-reads")
	if err != nil {
		t.Fatalf("GetComponentHealth: %v", err)
	}

	// One real failure against a threshold of three. Seven sibling cancellations recorded as
	// service failures would put this at eight and the state at degraded — for one failed read of
	// one object.
	if got.ConsecutiveErrors > 1 {
		t.Errorf("one failed parallel read recorded %d consecutive errors against s3-reads; want at "+
			"most 1. Sibling chunks canceled by the failure are being counted as independent S3 "+
			"failures, so a single failed read of a large object degrades reads for every caller "+
			"(ErrorThreshold is %d).", got.ConsecutiveErrors, health.DefaultConfig().ErrorThreshold)
	}

	if got.State != health.StateHealthy {
		t.Errorf("s3-reads is %s after one failed read of one object, want %s",
			got.State, health.StateHealthy)
	}
}

// TestParallelReadStopsFetchingWhenAChunkFails asserts the failure cancels its siblings.
//
// The old loop returned on the first error from a channel and left every other goroutine fetching
// its chunk to completion — bytes billed as egress and delivered to nobody, and on a wedged endpoint
// goroutines outliving the request. The observable consequence is bytes served after the read
// already failed, which is what this counts.
//
// The budget below is loose on purpose, but not as loose as it once was. It began as "not the whole
// object", and mutation testing showed that could never fail: the chunk that failed serves no bytes,
// so a read with cancellation entirely removed still comes in a chunk short of the total. Measured,
// the difference is 1 MiB served with cancellation and 7 MiB without — two chunks is comfortably
// clear of the first and nowhere near the second, and it still tolerates the genuine race, which is a
// chunk that had already finished before the failure landed.
func TestParallelReadStopsFetchingWhenAChunkFails(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const key = "parallel/cancel"
	ts.PutObject(key, testaws.DeterministicBytes(key, parallelObjectSize))
	ts.ResetRequests()

	// Concurrency 1 makes the ordering deterministic enough to assert on: chunk 0 is fetched, then
	// chunk 1 fails, and the six chunks after it must never be requested at all. With a wider
	// fan-out the answer depends on scheduling.
	backend := ts.Backend(func(cfg *s3.Config) {
		parallelReadConfig(cfg)
		cfg.ParallelReadConcurrency = 1
	})

	ts.InjectFault(testaws.Fault{
		Method:      "GET",
		KeySuffix:   key,
		RangePrefix: fmt.Sprintf("bytes=%d-", parallelChunkSize),
		Status:      403,
		Code:        "AccessDenied",
		Times:       1,
	})

	if _, err := backend.GetObject(context.Background(), key, 0, parallelObjectSize); err == nil {
		t.Fatal("GetObject succeeded with a chunk permanently failing")
	}

	// The canceled chunks may have been *issued* — the group starts them before the failure lands —
	// but a canceled context aborts the transfer partway, so the bytes are what to count, not the
	// requests.
	const budget = 2 * parallelChunkSize
	if served := ts.BytesRead(key); served > budget {
		t.Errorf("a failed parallel read transferred %d bytes of an %d-byte object, over a budget of "+
			"%d; the chunks after the failure were fetched to completion after the read had already "+
			"failed, which is egress billed for bytes no caller receives", served,
			int64(parallelObjectSize), int64(budget))
	}
}

// TestParallelReadRefusesAShortChunk is the length-assertion regression test.
//
// The object is shorter than the size the read was told, which is what a truncation landing between
// the HEAD that sized the read and the GETs that serve it looks like from in here. Assembling anyway
// produces a buffer shorter than requested, and the kernel presents it as file content against a
// HeadObject that still reports the full size — so the missing tail reads back as zeros.
//
// The cases are the two forms one shrunken object produces, and which one a chunk gets depends only
// on where it lands relative to the new end: a chunk straddling it is clamped and answers 206 short,
// a chunk starting at or past it is refused with 416. Both must be the same corruption error, because
// otherwise a truncation is detected or missed according to chunk alignment. The probe that
// established this found the first fixture here reached *only* the 416 path — at half size with
// aligned chunks nothing straddles the boundary at all — which is why the sizes below are stated in
// terms of where the end falls rather than as a fraction.
func TestParallelReadRefusesAShortChunk(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		actualSize int
		why        string
	}{
		{
			name:       "the end falls inside a chunk",
			actualSize: parallelChunkSize*3 + parallelChunkSize/2,
			why: "chunk 3 straddles the end, so S3 clamps its range and answers 206 with half the " +
				"bytes requested — a successful response of the wrong length, which is the case no " +
				"error check catches",
		},
		{
			name:       "the end falls on a chunk boundary",
			actualSize: parallelChunkSize * 4,
			why: "no chunk straddles the end, so nothing is short: chunks 4 through 7 start past it " +
				"and are refused with 416 InvalidRange, which reaches translateError's default arm " +
				"as STORAGE_READ — a service failure that degrades s3-reads for an object that is " +
				"merely smaller than advertised",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			ts.RequireRangeGET()

			const key = "parallel/short"
			ts.PutObject(key, testaws.DeterministicBytes(key, tc.actualSize))
			ts.ResetRequests()

			backend := ts.Backend(parallelReadConfig)

			got, err := backend.GetObject(context.Background(), key, 0, parallelObjectSize)
			if err == nil {
				t.Fatalf("GetObject returned %d bytes for a %d-byte request against a %d-byte object, "+
					"with no error. A short assembled read is handed to the kernel as file content and "+
					"the missing tail reads back as zeros — silent truncation of user data.",
					len(got), parallelObjectSize, tc.actualSize)
			}

			assertCode(t, err, objerrors.ErrCodeDataCorruption)

			// The code is what health and the breaker read, so a corruption error must not also count
			// as a service failure — an object that shrank is not an outage.
			if h, herr := backend.GetComponentHealth("s3-reads"); herr == nil && h.ConsecutiveErrors > 0 {
				t.Errorf("s3-reads recorded %d consecutive errors for a shrunken object; reads of "+
					"healthy objects are refused after %d. %s", h.ConsecutiveErrors, 3, tc.why)
			}
		})
	}
}

// TestParallelReadRefusesAMixOfVersions asserts the ETag comparison.
//
// N ranged GETs of one object are N points in time. An overwrite landing between the first and the
// last returns 206 for every range, with lengths that add up perfectly, and assembles a file that
// never existed in the bucket — the head of one version spliced to the tail of another. Nothing
// downstream can catch it: the whole-object SHA-256 cannot be verified against an assembled read at
// all, so this comparison is the only integrity evidence a large read has.
//
// The overwrite is provoked by letting the first chunk fail once. Its retry is served after the
// object has been replaced, so the assembled read spans both versions — the same interleaving a
// concurrent writer produces, made deterministic.
func TestParallelReadRefusesAMixOfVersions(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const key = "parallel/versions"
	first := testaws.DeterministicBytes(key+"-v1", parallelObjectSize)
	second := testaws.DeterministicBytes(key+"-v2", parallelObjectSize)

	ts.PutObject(key, first)
	ts.ResetRequests()

	// Concurrency 1 makes the overwrite land in the middle of the read rather than racing it: chunk
	// 0 is served from v1, then the overwrite happens, then the remaining chunks come from v2.
	backend := ts.Backend(func(cfg *s3.Config) {
		parallelReadConfig(cfg)
		cfg.ParallelReadConcurrency = 1
	})

	overwritten := make(chan struct{})
	ts.InjectFault(testaws.Fault{
		Method:      "GET",
		KeySuffix:   key,
		RangePrefix: fmt.Sprintf("bytes=%d-", parallelChunkSize),
		Times:       1,
		OnFire: func() {
			ts.PutObject(key, second)
			close(overwritten)
		},
	})

	got, err := backend.GetObject(context.Background(), key, 0, parallelObjectSize)

	select {
	case <-overwritten:
	default:
		t.Fatal("the overwrite never happened, so this test did not exercise a mid-read version " +
			"change — the Range matcher did not match the chunk it was aimed at")
	}

	if err == nil {
		// The assembled buffer is the right length and every chunk answered 206. Spelling out what
		// it actually contains is the point: a length check alone cannot catch this.
		t.Fatalf("GetObject assembled %d bytes across a mid-read overwrite with no error. Chunk 0 came "+
			"from the old object and the rest from the new one, so the caller received a file that "+
			"never existed in the bucket — and an assembled read cannot be checked against the "+
			"whole-object SHA-256, so nothing downstream will catch it either.", len(got))
	}

	assertCode(t, err, objerrors.ErrCodeDataCorruption)
}

// TestParallelReadHonorsContextCancellation asserts a canceled caller is reported as canceled.
//
// The distinction matters because of where the error goes. ErrCodeOperationCanceled is the one code
// health.RecordError heals on, so a FUSE interrupt or an unmount — both of which cancel mid-read,
// routinely — must arrive with that code and not as a storage failure. A mount that unmounted under
// load would otherwise leave the read component degraded on the way out.
func TestParallelReadHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const key = "parallel/canceled"
	ts.PutObject(key, testaws.DeterministicBytes(key, parallelObjectSize))
	ts.ResetRequests()

	backend := ts.Backend(parallelReadConfig)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := backend.GetObject(ctx, key, 0, parallelObjectSize); err == nil {
		t.Fatal("GetObject succeeded with an already-canceled context")
	}

	got, err := backend.GetComponentHealth("s3-reads")
	if err != nil {
		t.Fatalf("GetComponentHealth: %v", err)
	}

	if got.ConsecutiveErrors != 0 {
		t.Errorf("a canceled read recorded %d errors against s3-reads; a caller withdrawing its "+
			"request says nothing about S3, and an unmount under load would otherwise degrade the "+
			"read component on its way out", got.ConsecutiveErrors)
	}
}

// TestParallelReadAssemblesNonAlignedSizes covers the arithmetic the audit verified by hand.
//
// The range math was one of the few things in this function that was already correct, and it is
// worth a test precisely because the new length assertions run on every chunk: an off-by-one in the
// final short chunk now fails the read outright rather than returning a byte too few, so a
// regression here is loud instead of silent. Sizes are chosen so the last chunk is short, the object
// is smaller than one chunk, and the size divides exactly.
func TestParallelReadAssemblesNonAlignedSizes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		size int64
		why  string
	}{
		{
			name: "exactly two chunks",
			size: 2 * parallelChunkSize,
			why:  "the aligned case: no short final chunk",
		},
		{
			name: "two chunks and one byte",
			size: 2*parallelChunkSize + 1,
			why:  "a final chunk of exactly one byte, the narrowest the arithmetic gets",
		},
		{
			name: "one byte short of three chunks",
			size: 3*parallelChunkSize - 1,
			why:  "a final chunk one byte short, which an inclusive/exclusive mixup gets wrong",
		},
		{
			name: "a prime number of bytes",
			size: 2_097_593,
			why:  "nothing divides evenly, so every boundary is computed rather than aligned",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			ts.RequireRangeGET()

			key := fmt.Sprintf("parallel/aligned-%d", tc.size)
			want := testaws.DeterministicBytes(key, int(tc.size))
			ts.PutObject(key, want)
			ts.ResetRequests()

			backend := ts.Backend(parallelReadConfig)

			got, err := backend.GetObject(context.Background(), key, 0, tc.size)
			if err != nil {
				t.Fatalf("GetObject of %d bytes: %v (%s)", tc.size, err, tc.why)
			}

			assertBytesEqual(t, got, want, tc.why)

			// The fan-out has to have happened, or this is a test of the serial path wearing the
			// parallel path's name.
			wantGETs := int((tc.size + parallelChunkSize - 1) / parallelChunkSize)
			if n := len(ts.GETs(key)); n != wantGETs {
				t.Errorf("read of %d bytes issued %d GETs, want %d — the fan-out did not split along "+
					"ReadChunkSize boundaries", tc.size, n, wantGETs)
			}
		})
	}
}

// TestParallelReadAtAnOffset asserts the fan-out starts where the caller asked.
//
// A read at an offset is what a large sequential reader actually issues, and the chunk arithmetic
// adds the offset to every range. Getting that wrong returns the head of the file for a read of its
// middle: right length, wrong bytes, and no error anywhere.
func TestParallelReadAtAnOffset(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const (
		key    = "parallel/offset"
		offset = 3*parallelChunkSize + 7 // deliberately not chunk-aligned
		length = 3 * parallelChunkSize
	)

	want := testaws.DeterministicBytes(key, parallelObjectSize)
	ts.PutObject(key, want)
	ts.ResetRequests()

	backend := ts.Backend(parallelReadConfig)

	got, err := backend.GetObject(context.Background(), key, offset, length)
	if err != nil {
		t.Fatalf("GetObject at offset %d: %v", offset, err)
	}

	assertBytesEqual(t, got, want[offset:offset+length],
		"a read at an offset must return the bytes at that offset, not the head of the object")
}

// TestParallelReadCompletesWithinItsBudget is the deadlock check.
//
// errgroup.SetLimit blocks Go until a slot frees, and the assembly waits on Wait — so a chunk that
// returns without releasing its slot, or a Wait that never sees a chunk finish, is a wedged
// goroutine holding whatever FUSE request is above it rather than a slow read. That failure mode
// does not produce a wrong value to assert on, so the deadline is the assertion.
func TestParallelReadCompletesWithinItsBudget(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const key = "parallel/budget"
	want := testaws.DeterministicBytes(key, parallelObjectSize)
	ts.PutObject(key, want)
	ts.ResetRequests()

	// Concurrency 1 against eight chunks is the shape that exercises the limiter hardest: every
	// chunk after the first has to wait for a slot to be released.
	backend := ts.Backend(func(cfg *s3.Config) {
		parallelReadConfig(cfg)
		cfg.ParallelReadConcurrency = 1
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		got, err := backend.GetObject(ctx, key, 0, parallelObjectSize)
		if err == nil && len(got) != len(want) {
			err = fmt.Errorf("returned %d bytes, want %d", len(got), len(want))
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("parallel read at concurrency 1: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("a parallel read at concurrency 1 did not complete within 30s — a chunk is not " +
			"releasing its slot, or Wait is not seeing one finish. This holds the FUSE request above " +
			"it for the life of the mount.")
	}
}

// assertBytesEqual compares two buffers and reports the first difference rather than dumping
// megabytes into the test log.
func assertBytesEqual(t *testing.T, got, want []byte, why string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("read returned %d bytes, want %d — %s", len(got), len(want), why)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d of %d differs (got %#x, want %#x); the assembled chunks are out of "+
				"order, overlapping, or from different fetches — %s", i, len(want), got[i], want[i], why)
		}
	}
}

// assertCode checks that an error carries a specific ObjectFS code.
//
// The code, not the message: these errors gate health tracking and the circuit breaker through
// errors.IsServiceFailure, which reads the code and nothing else, so an error with the right prose
// and the wrong code degrades the wrong thing.
func assertCode(t *testing.T, err error, want objerrors.ErrorCode) {
	t.Helper()

	var objErr *objerrors.ObjectFSError
	if !errors.As(err, &objErr) {
		t.Fatalf("error is not an *ObjectFSError, so it carries no code for the health tracker or "+
			"the circuit breaker to classify: %v", err)
	}

	if objErr.Code != want {
		t.Errorf("error code is %s, want %s: %v", objErr.Code, want, err)
	}
}
