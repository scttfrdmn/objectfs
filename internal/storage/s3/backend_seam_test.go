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
