package s3_test

// The parallel-read fan-out has to be decided from the *object*, not from the local compression
// config. These tests assert request counts and shapes against the substrate endpoint rather than
// latency, for the same reason read_amplification_test.go does: whether a read fanned out is a
// property of how many GETs crossed the wire, and both paths return identical bytes at identical
// speed against an in-process emulator.

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/pkg/health"
)

// TestFanOutIsDecidedByTheObjectNotTheConfig is the #228 regression test, and the one the issue's
// acceptance criteria name last: the same object, the same read, `Compression.Enabled` toggled, with
// the fan-out asserted unchanged.
//
// The gate in GetObject used to read `b.compressor == nil || !b.compressor.Enabled()`, which reports
// the local *write* configuration. It says nothing about the object named by the key, so configuring
// compression switched fan-out off for every object in the bucket — objects never compressed, objects
// below MinSize, objects where compression did not help, and objects written by other tools.
//
// That is audit finding C4 one line above C4's own fix, and it survived for the same reason it is
// easy to write: it fails quietly. C4 moved bytes that did not need moving, which a byte-count
// assertion catches loudly. This one merely declined an optimization — nothing failed, nothing was
// logged, and the only symptom was v0.10.0's headline feature being off.
//
// The object here is written by an independent client, so it carries no ObjectFS metadata and no
// Content-Encoding. That is the overwhelmingly common case in a bucket and the case the config-keyed
// gate got wrong.
func TestFanOutIsDecidedByTheObjectNotTheConfig(t *testing.T) {
	t.Parallel()

	const (
		objectSize = 8 << 20
		chunkSize  = 1 << 20
		threshold  = 1 << 20
		wantGETs   = objectSize / chunkSize
		key        = "fanout/uncompressed"
	)

	for _, compressionEnabled := range []bool{false, true} {
		name := "compression off"
		if compressionEnabled {
			name = "compression on"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			ts.RequireRangeGET()

			backend := ts.Backend(func(cfg *s3.Config) {
				cfg.ParallelReadThreshold = threshold
				cfg.ReadChunkSize = chunkSize
				cfg.ParallelReadConcurrency = 4

				cfg.Compression.Enabled = compressionEnabled
				cfg.Compression.Algorithm = "zstd"
				cfg.Compression.Level = 3
				cfg.Compression.MinSize = "4KB"
			})

			want := testaws.DeterministicBytes(key, objectSize)
			ts.PutObject(key, want)
			ts.ResetRequests()

			got, err := backend.GetObject(context.Background(), key, 0, objectSize)
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}

			// Correctness before mechanics. A fan-out that assembles chunks out of order is a far
			// worse defect than one that does not fan out at all.
			if !bytes.Equal(got, want) {
				t.Fatalf("GetObject returned %d bytes that do not match the %d-byte object",
					len(got), len(want))
			}

			if n := len(ts.GETs(key)); n != wantGETs {
				t.Errorf("GetObject issued %d GETs for an uncompressed %d-byte object with "+
					"compression.enabled=%v, want %d.\nThe object carries no Content-Encoding, so "+
					"the local write config must not change how it is read. One GET here means the "+
					"gate is keyed on b.compressor.Enabled() again, which is the v0.10.0 behavior "+
					"(#228).\nRequests: %s",
					n, objectSize, compressionEnabled, wantGETs, describe(ts.Requests()))
			}
		})
	}
}

// TestFanOutOnACompressedObjectFallsBackAndStaysCorrect is the other half: an object that *is*
// compressed must not be served from assembled ranges, because a zstd frame is not seekable and
// there is nothing to fan out across.
//
// Declining the fan-out for such an object was always the correct intent — the comment on the old
// gate said so. The defect was its scope. So the fix has to keep the intent while narrowing the
// scope, and this test is what holds it: the read has to return the right bytes, and it has to reach
// them through the whole-object path.
//
// Two offsets, because a compressed object reaches the fallback by two different routes and only one
// of them involves a response header. A read starting inside the stored body gets a 206 carrying
// Content-Encoding — the chunk sees the encoding directly. A read starting past the end of the stored
// body gets nothing but 416s, so no chunk ever sees a header, and the fallback has to be reached by
// asking HeadObject instead. The second is the harder case and it is not hypothetical: the stored
// body is a fraction of the size the caller was told, so ordinary reads land past its end constantly.
//
//nolint:tparallel // the subtests share a request recorder and must run in order; see below
func TestFanOutOnACompressedObjectFallsBackAndStaysCorrect(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const (
		key        = "fanout/compressed"
		objectSize = 8 << 20
		chunkSize  = 1 << 20
		threshold  = 1 << 20
		readSize   = 2 << 20
	)

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.ParallelReadThreshold = threshold
		cfg.ReadChunkSize = chunkSize
		cfg.ParallelReadConcurrency = 4

		cfg.Compression.Enabled = true
		cfg.Compression.Algorithm = "zstd"
		cfg.Compression.Level = 3
		cfg.Compression.MinSize = "4KB"
	})

	ctx := context.Background()

	// Only ~50% compressible, so the stored body stays large enough for one offset to fall inside it
	// and another past its end. compressible() shrinks 8 MiB to a few hundred bytes, which would put
	// every offset past the end and collapse the two cases below into one.
	body := semiCompressible(key, objectSize)
	if err := backend.PutObject(ctx, key, body, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	stored := ts.ObjectSize(key)
	if stored >= objectSize {
		t.Fatalf("stored size %d did not shrink below %d; compression did not engage and this test "+
			"proves nothing", stored, objectSize)
	}

	reads := []struct {
		name   string
		offset int64
		// wantInsideStored is asserted rather than assumed. If the compression ratio moves, one of
		// these cases silently becomes a duplicate of the other and the coverage quietly halves —
		// which is exactly the failure the equivalent assertion in read_amplification_test.go guards.
		wantInsideStored bool
	}{
		{"read starting inside the compressed body", 0, true},
		{"read starting past the end of the compressed body", objectSize - readSize, false},
	}

	// Not parallel: each case calls ResetRequests and then asserts on what the endpoint saw, so a
	// concurrent sibling's traffic would land inside the window under assertion. Counting requests is
	// the entire point here.
	//nolint:paralleltest // shared request recorder; the cases must run in order, see above
	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			if inside := r.offset < stored; inside != r.wantInsideStored {
				t.Fatalf("offset %d against a %d-byte stored body is inside=%v, want inside=%v; the "+
					"compression ratio moved and this case no longer exercises what it names",
					r.offset, stored, inside, r.wantInsideStored)
			}

			ts.ResetRequests()

			got, err := backend.GetObject(ctx, key, r.offset, readSize)
			if err != nil {
				t.Fatalf("GetObject(%d, %d): %v", r.offset, readSize, err)
			}

			// The assertion that matters most. Returning wrong bytes is what a naive "always fan out"
			// fix would do: the ranges would be applied to the encoded body rather than the decoded
			// content, and every byte would be wrong while every request succeeded.
			if want := body[r.offset : r.offset+readSize]; !bytes.Equal(got, want) {
				t.Fatalf("a %d-byte read at offset %d of a compressed object returned bytes that do "+
					"not match body[%d:%d] — the range was applied to the encoded bytes rather than "+
					"the decoded ones", readSize, r.offset, r.offset, r.offset+readSize)
			}

			// And it reached them by fetching the whole stored body, which is the only way to decode
			// any part of a zstd frame. Asserting it keeps the tradeoff explicit rather than implied:
			// if seekable framing ever makes this cheap, this is the assertion to revisit rather than
			// one to quietly satisfy.
			unranged := 0
			for _, g := range ts.GETs(key) {
				if !g.IsRanged() {
					unranged++
				}
			}

			if unranged == 0 {
				t.Errorf("no unranged GET was issued for a compressed object; the whole body has to "+
					"be fetched to decode any of it.\nRequests: %s", describe(ts.Requests()))
			}
		})
	}
}

// TestFanOutFallbackLeavesNoHealthErrors pins the property that makes the attempt-then-fall-back
// design safe, and it is the one that would make this fix worse than the defect if it were wrong.
//
// Abandoning a fan-out means several chunks fail at once: a short read at the boundary of the stored
// body, a refused range for every chunk past it. The s3-reads component degrades at ErrorThreshold
// consecutive failures and a degraded s3-reads refuses reads at the gate on GetObject's first line —
// so if those chunks counted, one compressed object would take unrelated, perfectly readable objects
// offline with it. The fallback has to be a routing decision that costs nothing but the wasted
// chunks.
//
// LastErrorMessage is the assertion, and two weaker ones were tried and measured first rather than
// reasoned about — both pass on a build where the 416 is deliberately classified as a service failure:
//
//   - "read the object repeatedly and check none of the reads errors" fails to catch it because
//     s3-reads never actually degrades here. The read has to reach the threshold in *consecutive*
//     errors and it cannot: the whole-object re-read that follows every fallback records a success.
//   - ConsecutiveErrors == 0 fails to catch it for the same reason. RecordSuccess *decrements*, and
//     the abandoned sibling chunks each record one too, so the counter is back at zero by the time
//     the read returns even though the failures were counted on the way through.
//
// Both were verified by making that mutation and watching them pass. What it produced was
// `ConsecutiveErrors=0`, `state=healthy`, and `LastErrorMessage="STORAGE_READ: S3 refused the
// requested byte range"` — so the only durable evidence at this scale is the recorded error itself.
// The counter is a poor witness precisely because a single read of a single object cannot exhaust it;
// what makes this worth pinning is a mount reading many compressed objects with no successes to
// spare in between, which no unit test reproduces and which the classification decides.
func TestFanOutFallbackLeavesNoHealthErrors(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const (
		key        = "fanout/health"
		objectSize = 8 << 20
		chunkSize  = 1 << 20
		threshold  = 1 << 20
		readSize   = 2 << 20
	)

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.ParallelReadThreshold = threshold
		cfg.ReadChunkSize = chunkSize
		cfg.ParallelReadConcurrency = 4

		cfg.Compression.Enabled = true
		cfg.Compression.Algorithm = "zstd"
		cfg.Compression.Level = 3
		cfg.Compression.MinSize = "4KB"
	})

	ctx := context.Background()

	body := semiCompressible(key, objectSize)
	if err := backend.PutObject(ctx, key, body, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Both offsets, because the two fallback routes fail differently. A read inside the stored body
	// sees the Content-Encoding on a chunk and abandons the rest, so the failures are cancellations. A
	// read past the end of it collects a 416 per chunk — which is the classification this test is
	// about — before the HEAD settles what they meant.
	for _, offset := range []int64{0, objectSize - readSize} {
		got, err := backend.GetObject(ctx, key, offset, readSize)
		if err != nil {
			t.Fatalf("offset %d: GetObject failed: %v", offset, err)
		}

		if want := body[offset : offset+readSize]; !bytes.Equal(got, want) {
			t.Fatalf("offset %d: returned bytes do not match the object", offset)
		}

		reads, healthErr := backend.GetComponentHealth("s3-reads")
		if healthErr != nil {
			t.Fatalf("GetComponentHealth(s3-reads): %v", healthErr)
		}

		if reads.LastErrorMessage != "" {
			t.Errorf("a read at offset %d that fell back from the fan-out left s3-reads holding a "+
				"failure: %q (state %v, consecutive %d).\nThe abandoned chunks are being counted "+
				"against the component. It degrades at a few consecutive failures and a degraded "+
				"s3-reads refuses reads at the top of GetObject, so a mount reading compressed "+
				"objects would take unrelated, perfectly readable objects offline with it.",
				offset, reads.LastErrorMessage, reads.State, reads.ConsecutiveErrors)
		}

		// Cheap, and it is the state the consequence above is actually about. It cannot fail while the
		// message is empty, so it is here as the statement of what matters rather than as coverage.
		if reads.State != health.StateHealthy {
			t.Errorf("after a read at offset %d that fell back from the fan-out, s3-reads is %v, "+
				"want healthy; last error %q", offset, reads.State, reads.LastErrorMessage)
		}
	}
}

// TestFanOutOnAnUncompressedObjectCostsNoExtraHead pins the cost side of the design choice #228
// asked for a decision on.
//
// The issue offered two ways to decide from the object: HEAD first when compression is configured, or
// attempt the fan-out and fall back. The second was chosen so the cost falls on compressed objects,
// which already pay a whole-object fetch, rather than on every large read. This test is what makes
// that choice checkable — without it, adding a HEAD to the common path would be a silent regression
// that no correctness assertion notices.
func TestFanOutOnAnUncompressedObjectCostsNoExtraHead(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const (
		key        = "fanout/no-extra-head"
		objectSize = 8 << 20
		chunkSize  = 1 << 20
		threshold  = 1 << 20
	)

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.ParallelReadThreshold = threshold
		cfg.ReadChunkSize = chunkSize
		cfg.ParallelReadConcurrency = 4

		cfg.Compression.Enabled = true
		cfg.Compression.Algorithm = "zstd"
		cfg.Compression.Level = 3
		cfg.Compression.MinSize = "4KB"
	})

	want := testaws.DeterministicBytes(key, objectSize)
	ts.PutObject(key, want)
	ts.ResetRequests()

	// A caller-supplied size, which is the case that has no HEAD to piggyback on. When the size is
	// not supplied, GetObject HEADs anyway for the chunk arithmetic and the encoding question comes
	// free with it.
	got, err := backend.GetObject(context.Background(), key, 0, objectSize)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Fatalf("GetObject returned %d bytes that do not match the object", len(got))
	}

	heads := 0
	for _, r := range ts.RequestsFor(key) {
		if r.Method == http.MethodHead {
			heads++
		}
	}

	if heads != 0 {
		t.Errorf("a large read of an uncompressed object issued %d HEAD requests, want 0.\nThe "+
			"fan-out decision is meant to come from the response, not from a probe: #228 chose "+
			"attempt-and-fall-back precisely so the cost lands on compressed objects rather than on "+
			"every read above the threshold.\nRequests: %s", heads, describe(ts.Requests()))
	}
}
