package s3_test

// The parallel-read fan-out has to be decided from the *object*, not from the local compression
// config. These tests assert request counts and shapes against the substrate endpoint rather than
// latency, for the same reason read_amplification_test.go does: whether a read fanned out is a
// property of how many GETs crossed the wire, and both paths return identical bytes at identical
// speed against an in-process emulator.

import (
	"bytes"
	"context"
	"fmt"
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
// compressed must not be served from assembled ranges, because the fan-out's chunk boundaries are
// offsets into the encoded body and the caller asked for offsets into the decoded content.
//
// Declining the fan-out for such an object was always the correct intent — the comment on the old
// gate said so. The defect was its scope. So the fix has to keep the intent while narrowing the
// scope, and this test is what holds it: the read has to return the right bytes, and it has to reach
// them somewhere other than the fan-out.
//
// Where that "somewhere" is has changed. Before #185 it was a whole-object fetch, and this test
// asserted an unranged GET to say so. A framed object is now served from the frames covering the
// range, so the assertion is inverted: every GET must be ranged. The fan-out itself is unchanged and
// still declined.
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

	// The all-GETs-are-ranged assertion below is only about framing if the object was framed. An
	// unframed compressed object legitimately takes the whole-object path and would fail it, so
	// without this the test could not tell "framing regressed" from "the fixture stopped framing".
	if _, ok := ts.ObjectMetadata(key)[metaSeekableKey]; !ok {
		t.Fatalf("the object carries no %s descriptor, so it was not framed and a whole-object fetch "+
			"would be the correct way to read it. Metadata: %v",
			metaSeekableKey, ts.ObjectMetadata(key))
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

			// This assertion has been inverted. It used to require that an *unranged* GET was issued,
			// because fetching the whole stored body was the only way to decode any part of a zstd
			// stream; its comment said that if seekable framing ever made this cheap, this was the
			// assertion to revisit rather than quietly satisfy. #185 is that change.
			//
			// The fan-out is still declined — the correctness assertion above is what holds that, and
			// it is the stronger witness: had the chunks been assembled, the ranges would have been
			// applied to the encoded body and every byte would be wrong. What is new is where the
			// declined fan-out lands. It used to land on one whole-object GET; it now lands on the
			// framed path, which issues only ranged GETs. So an unranged GET here means framing
			// stopped engaging on this object and the read regressed to whole-body transfer.
			for _, g := range ts.GETs(key) {
				if !g.IsRanged() {
					t.Errorf("an unranged GET was issued for a framed object, so the read fetched the "+
						"whole stored body. A declined fan-out should fall through to the framed path, "+
						"which fetches the index and the frames covering the range.\nRequests: %s",
						describe(ts.Requests()))

					break
				}
			}

			// The transferred total is logged, not asserted, and the reason is worth writing down: it
			// includes the fan-out chunks that were fetched and then abandoned. #228 chose
			// attempt-then-fall-back so the cost of the encoding probe lands on compressed objects
			// rather than on every large read, and at the time a compressed object was already paying
			// a whole-body fetch, so the wasted chunks were free. With framing they are no longer
			// free — they are most of what this read now transfers. Asserting a budget here would be
			// asserting the size of that waste, which is #514's subject, not this test's.
			t.Logf("%d bytes at offset %d transferred %d bytes over %d GETs against a %d-byte stored "+
				"body, including the abandoned fan-out chunks",
				readSize, r.offset, ts.BytesRead(key), len(ts.GETs(key)), stored)
		})
	}
}

// TestFanOutOnACompressedObjectProbesWithOneChunk is #514: an abandoned fan-out must not have already
// transferred the bytes seekable framing exists to avoid.
//
// The number the sibling test above logs is what this asserts. A 2 MiB read at offset 0 of an 8 MiB
// object stored in 4,293,520 bytes used to transfer 4,221,321 of them — 98% — and then run the framed
// read anyway. Two things added up to that, and both are requests made to learn something already
// known:
//
//   - Every chunk was launched at once, so ParallelReadConcurrency × ReadChunkSize was in flight before
//     any chunk's headers came back to say the object was encoded.
//   - The abandoned fan-out reported only *that* the object was encoded, so GetObject then issued a
//     ranged GET to discover the seekable descriptor — which the abandoned chunk's own response headers
//     had already carried. That GET comes back encoded, so its bytes are stored bytes nobody uses.
//
// Neither was a mistake when written. #228 chose attempt-then-fall-back over HEAD-first so that
// establishing whether an object is encoded costs compressed objects instead of every large read, and a
// compressed object was already paying a whole-body fetch — the chunks were free. #185 removed that
// fetch and made them the entire cost.
//
// So the assertions are about *requests not made*, not about latency: chunk 1's range is never asked
// for, no GET covers the whole read as the discovery fetch did, and the transferred total lands near
// what the framed read alone costs rather than near the stored body.
//
// The offsets are the sibling test's two, for the same reason: a read starting inside the stored body
// learns the encoding from a response header, and one starting past its end gets nothing but 416s and
// has to learn it from a HEAD. Only the first route has headers to hand back, so they are the two halves
// of this fix and not one case twice.
//
//nolint:tparallel // the subtests share a request recorder and must run in order; see below
func TestFanOutOnACompressedObjectProbesWithOneChunk(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const (
		key        = "fanout/probe"
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

	stored := ts.ObjectSize(key)

	// Without framing the fallback is a whole-body fetch and the budget below is unmeetable by
	// construction, so a fixture that stopped framing would fail this test as though the probe
	// regressed. Asserted rather than assumed, for the same reason the sibling test asserts it.
	if _, ok := ts.ObjectMetadata(key)[metaSeekableKey]; !ok {
		t.Fatalf("the object carries no %s descriptor, so it was not framed and a whole-body fetch is "+
			"the correct way to read it. Metadata: %v", metaSeekableKey, ts.ObjectMetadata(key))
	}

	// Ranges the fix is about. chunk1 is the one the old code had in flight before it knew anything;
	// discovery is the ranged GET that used to follow the abandoned fan-out to re-learn the descriptor.
	// Matching on the header text is exact — these are the strings getObjectRange builds.
	chunk1 := fmt.Sprintf("bytes=%d-%d", chunkSize, 2*chunkSize-1)

	transferred := map[int64]int64{}

	//nolint:paralleltest // shared request recorder; the cases reset and then assert on it, in order
	for _, r := range []struct {
		name   string
		offset int64
	}{
		{"read starting inside the compressed body", 0},
		{"read starting past the end of the compressed body", objectSize - readSize},
	} {
		t.Run(r.name, func(t *testing.T) {
			if inside := r.offset < stored; inside != (r.offset == 0) {
				t.Fatalf("offset %d against a %d-byte stored body is inside=%v; the compression ratio "+
					"moved and these two cases are no longer the two routes this test names",
					r.offset, stored, inside)
			}

			ts.ResetRequests()

			got, err := backend.GetObject(ctx, key, r.offset, readSize)
			if err != nil {
				t.Fatalf("GetObject(%d, %d): %v", r.offset, readSize, err)
			}

			// First, because a cheaper read that returns the wrong bytes is not an improvement.
			if want := body[r.offset : r.offset+readSize]; !bytes.Equal(got, want) {
				t.Fatalf("a %d-byte read at offset %d returned bytes that do not match body[%d:%d]",
					readSize, r.offset, r.offset, r.offset+readSize)
			}

			discovery := fmt.Sprintf("bytes=%d-%d", r.offset, r.offset+readSize-1)

			for _, g := range ts.GETs(key) {
				switch g.Range {
				case chunk1:
					t.Errorf("the fan-out requested chunk 1 (%s) on a compressed object.\nChunk 0's "+
						"response headers are what settle whether this object can be read from ranges "+
						"at all, so the remaining chunks must wait for them. Launching every chunk at "+
						"once is #514: it transferred 98%% of the stored body before learning the "+
						"fan-out was the wrong path.\nRequests: %s", chunk1, describe(ts.Requests()))

				case discovery:
					t.Errorf("a GET covered the whole requested range (%s) on a compressed object.\n"+
						"That is the discovery fetch the abandoned fan-out used to need in order to "+
						"find the seekable descriptor — which the abandoned chunk's own response "+
						"already carried. It comes back encoded, so its bytes are stored bytes this "+
						"read never uses.\nRequests: %s", discovery, describe(ts.Requests()))
				}
			}

			transferred[r.offset] = ts.BytesRead(key)

			t.Logf("%d bytes at offset %d transferred %d bytes over %d GETs against a %d-byte stored body",
				readSize, r.offset, ts.BytesRead(key), len(ts.GETs(key)), stored)
		})
	}

	// The byte assertion, and it is calibrated against the other read rather than against a constant.
	//
	// A fixed budget was tried first and is too blunt to be worth having. The framed read of 2 MiB costs
	// about a quarter of the stored body here, so any budget loose enough to survive the compression
	// ratio moving is also loose enough to hide a whole 1 MiB chunk — and "chunk 0's body was
	// transferred anyway" is one of the three things this fix does. Verified by mutation: with the
	// probe's body-declining removed, a `stored*3/4` budget passes.
	//
	// The read past the end of the stored body is the calibration. Every chunk of it is refused with a
	// 416, so it cannot transfer a chunk byte even in principle: what it transfers *is* the framed cost
	// of a 2 MiB read of this object. The read at offset 0 must cost the same thing, because framing is
	// the path both of them end up on. Anything more is bytes moved before the read knew where it was
	// going.
	inside, insideOK := transferred[0]
	pastEnd, pastEndOK := transferred[objectSize-readSize]

	if !insideOK || !pastEndOK {
		t.Fatalf("both reads have to have run for the comparison below to mean anything; measured %v",
			transferred)
	}

	// A quarter of a chunk. Large enough for the two framed reads to differ in how many frames they
	// touch and in the 416 bodies the second one collects, far smaller than the chunk or the discovery
	// GET that either regression would add.
	if allowance := int64(chunkSize / 4); inside > pastEnd+allowance {
		t.Errorf("a %d-byte read at offset 0 transferred %d bytes; the same-sized read past the end of "+
			"the %d-byte stored body transferred %d, and both are served by the framed path.\n"+
			"The %d extra bytes are the fan-out's — chunks launched before chunk 0's headers came "+
			"back, chunk 0's own body transferred after they said the object was encoded, or a "+
			"discovery GET re-learning the descriptor those headers already carried. That is #514: "+
			"the measured figure was 4,221,321 of a 4,293,520-byte body, 98%%.\nRequests: %s",
			readSize, inside, stored, pastEnd, inside-pastEnd, describe(ts.Requests()))
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
