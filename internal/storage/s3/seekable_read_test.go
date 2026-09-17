package s3_test

// The read half of seekable framing (#185 stage 3).
//
// Two things are being tested and they need different kinds of assertion. That a framed read returns
// the right bytes is asserted directly against the fixture. That it is *cheap* can only be asserted in
// bytes transferred — the whole feature is a byte-count change, it is measured against an in-process
// endpoint where a whole-object fetch and a two-frame fetch complete equally fast, and a latency
// assertion for it would be a flaky proxy for the thing it means to measure.
//
// The third thing, and the one with the most tests here, is the fallbacks. Every path that declines
// the framed read still returns correct bytes, so the feature can stop working entirely without a
// single failing test or a single error reaching a caller. What makes that visible is the fallback
// counter and its reason, so each decline is reached with a real fixture and the reason asserted —
// otherwise the reason strings are decoration and a decline taken for the wrong cause is invisible.

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/scttfrdmn/objectfs/internal/compression"
	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// framedObject is a written framed object plus everything a test needs to reason about its layout:
// the content it should read back as, the stored bytes, and the object's own descriptor and index.
//
// The descriptor and index are parsed with the production parsers rather than reconstructed here. A
// test that computed an expected frame offset with its own arithmetic would agree with a defect in
// that arithmetic, and these offsets are precisely what the tests below turn into Range assertions
// and fault matchers.
type framedObject struct {
	key      string
	content  []byte
	stored   []byte
	meta     map[string]string
	encoding string
	desc     compression.SeekableDescriptor
	idx      *compression.FrameIndex
}

// spanFor returns the stored byte range a framed read of [offset, offset+size) must fetch, which is
// what the frame index says and therefore what a fault matcher can be armed against.
func (f framedObject) spanFor(offset, size int64) (start, length int64) {
	frames, _ := f.idx.FramesCovering(offset, size)
	last := frames[len(frames)-1]
	start = frames[0].CompressedOffset

	return start, last.CompressedOffset + last.CompressedSize - start
}

// putFramedObject writes a framed object through the backend and reads back everything about how it
// landed. [putFramed] in seekable_upload_test.go is the stage-2 caller's narrower view of the same
// fixture and delegates here, so there is one place that knows how a framed fixture is built.
//
// It fails the test unless the object actually framed, and unless it framed into enough frames for a
// partial read to exist. Both are vacuity guards rather than paranoia: DeriveFrameSize has a 256 KiB
// floor and its own tests, so a fixture size chosen here can drift under it, and an object in one
// frame is read whole by design — every "this read was cheap" assertion below would then be measuring
// the whole-object path and passing or failing for reasons unrelated to framing.
func putFramedObject(t *testing.T, ts *testaws.TestServer, backend *s3.Backend, key string, size int) framedObject {
	t.Helper()

	ctx := context.Background()

	// Only ~50% compressible. compressible() shrinks a megabyte to a few hundred bytes, which puts
	// every interesting offset past the end of the stored body and collapses the cases below into one.
	content := semiCompressible(key, size)
	if err := backend.PutObject(ctx, key, content, nil); err != nil {
		t.Fatalf("PutObject(%q): %v", key, err)
	}

	meta := ts.ObjectMetadata(key)

	text, ok := meta[metaSeekableKey]
	if !ok {
		t.Fatalf("a %d-byte object carries no %s descriptor, so it was not framed and nothing below "+
			"is a test of the framed read path. Metadata: %v", size, metaSeekableKey, meta)
	}

	desc, err := compression.ParseSeekableDescriptor(text)
	if err != nil {
		t.Fatalf("the descriptor ObjectFS just wrote does not parse: %q: %v", text, err)
	}

	stored := ts.GetObject(key)

	idx, _, err := compression.ParseFrameIndex(stored)
	if err != nil {
		t.Fatalf("the index ObjectFS just wrote does not parse: %v", err)
	}

	if len(idx.Frames) < 3 {
		t.Fatalf("a %d-byte object framed into %d frames; at least 3 are needed for a read to cover "+
			"some frames and not others, which is the case these tests are about", size, len(idx.Frames))
	}

	return framedObject{
		key:      key,
		content:  content,
		stored:   stored,
		meta:     meta,
		encoding: "zstd",
		desc:     desc,
		idx:      idx,
	}
}

// reseed rewrites an object's stored bytes, preserving its metadata and Content-Encoding, after
// letting mutate change either the bytes or the metadata.
//
// This is how the damaged-object fixtures below are built. It goes through the raw client rather than
// the backend on purpose: the states being tested — a garbled descriptor, a damaged index, a frame
// that no longer matches its hash — are states ObjectFS will not write, and every one of them is
// reachable by anyone holding s3:PutObject. `aws s3 cp --metadata` carries a hand-edited descriptor
// through faithfully.
func reseed(t *testing.T, ts *testaws.TestServer, f framedObject, mutate func(stored []byte, meta map[string]string)) {
	t.Helper()

	stored := bytes.Clone(f.stored)

	meta := make(map[string]string, len(f.meta))
	for k, v := range f.meta {
		meta[k] = v
	}

	mutate(stored, meta)
	seedRaw(t, ts, f.key, stored, meta, f.encoding)
}

// seedRaw writes exactly these bytes, metadata and Content-Encoding, with no ObjectFS involvement.
func seedRaw(t *testing.T, ts *testaws.TestServer, key string, body []byte, meta map[string]string, encoding string) {
	t.Helper()

	_, err := ts.Client().PutObject(context.Background(), &awss3.PutObjectInput{
		Bucket:          aws.String(ts.Bucket),
		Key:             aws.String(key),
		Body:            bytes.NewReader(body),
		Metadata:        meta,
		ContentEncoding: aws.String(encoding),
	})
	if err != nil {
		t.Fatalf("seed %q: %v", key, err)
	}
}

// gzipBytes encodes data as gzip, for the fixture that puts a seekable descriptor on an object no
// frame decoder claims. The body has to be genuinely decodable or the whole-object fallback would fail
// and the test would prove only that something went wrong.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	return buf.Bytes()
}

// TestFramedReadIsCorrectAcrossFrameBoundaries is the correctness test for the read path, and the one
// aimed at its most likely defect.
//
// A framed read decodes whole frames, so the buffer it assembles starts at the first frame's
// uncompressed offset rather than at the offset the caller asked for. The slice back to the request is
// therefore frame-relative, and getting it wrong produces a uniform shift: every read returns
// plausible bytes from the right object at the wrong place. Nothing errors and the length is right.
//
// So the offsets are chosen to make that shift unavoidable rather than incidental — each frame
// boundary approached from both sides, spans of one, two and three frames, and the object's own edges.
// A read whose offset is a frame boundary is the one case where a frame-relative and an
// object-relative slice agree, which is why it cannot be the only case tested.
func TestFramedReadIsCorrectAcrossFrameBoundaries(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = true
		cfg.Compression.Algorithm = "zstd"
		cfg.Compression.Level = 3
		cfg.Compression.MinSize = "4KB"
		cfg.ParallelReadThreshold = 0
	})

	const size = 2 << 20

	f := putFramedObject(t, ts, backend, "seekable/boundaries", size)

	frame := f.idx.FrameSize
	content := int64(len(f.content))

	type read struct {
		offset, size int64
	}

	var reads []read

	// Around every internal frame boundary, from both sides and straddling it.
	for i := int64(1); i < int64(len(f.idx.Frames)); i++ {
		at := i * frame

		reads = append(reads,
			read{at - 4096, 4096},            // ends exactly on the boundary
			read{at, 4096},                   // starts exactly on it
			read{at - 2048, 4096},            // straddles it
			read{at - 1, 2},                  // the two bytes either side of it
			read{at + 7, frame},              // an unaligned span of two frames
			read{at - frame/2, 2*frame + 11}, // three frames, unaligned at both ends
		)
	}

	// The object's own edges, where an off-by-one has nothing to shift into.
	reads = append(reads,
		read{0, 1},
		read{0, 4096},
		read{content - 1, 1},
		read{content - 4096, 4096},
		read{content - 4096, 8192}, // runs past the end: short, not an error
		read{frame - 1, 3},
	)

	for _, r := range reads {
		name := fmt.Sprintf("offset_%d_size_%d", r.offset, r.size)

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := backend.GetObject(context.Background(), f.key, r.offset, r.size)
			if err != nil {
				t.Fatalf("GetObject(%d, %d): %v", r.offset, r.size, err)
			}

			want := f.content[r.offset:min(r.offset+r.size, content)]
			if bytes.Equal(got, want) {
				return
			}

			// A uniform shift is the failure this test exists for, so it is worth naming when it
			// happens: "wrong bytes" and "the same bytes from 4096 further along" are very different
			// diagnoses and only one of them points at the frame-relative slice.
			if at := bytes.Index(f.content, got); len(got) > 16 && at >= 0 && at != int(r.offset) {
				t.Fatalf("GetObject(%d, %d) returned the object's bytes from offset %d — a shift of "+
					"%d. The slice back to the request is relative to the first frame's uncompressed "+
					"offset, not to the object", r.offset, r.size, at, at-int(r.offset))
			}

			t.Fatalf("GetObject(%d, %d) returned %d bytes that are not the object's content there "+
				"(want %d bytes)", r.offset, r.size, len(got), len(want))
		})
	}
}

// TestFramedReadTransfersOnlyTheFramesItCovers is the byte-count assertion: the point of the feature.
//
// The budget is derived from the object's own index rather than written down, because the alternative
// is a constant that has to be revisited whenever DeriveFrameSize changes — and a constant loose
// enough to survive that is too loose to distinguish a two-frame fetch from a whole-body one.
//
// Two offsets, because GetObject reaches the framed path by two different routes and they cost
// different amounts. A read starting inside the stored body gets a 206 whose *body* is stored bytes
// the framed read then throws away — the encoding is discovered by spending a request on the wrong
// interpretation of the range. A read starting past the end of the stored body gets a 416, which
// carries no body, so that route is the cheaper one and it is also the common one: the stored body is
// a fraction of the content length the caller was told, so any read far enough into a well-compressed
// file lands past its end. Testing only one of them would leave the other's accounting unpinned.
func TestFramedReadTransfersOnlyTheFramesItCovers(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = true
		cfg.Compression.Algorithm = "zstd"
		cfg.Compression.Level = 3
		cfg.Compression.MinSize = "4KB"
		cfg.ParallelReadThreshold = 0
	})

	const (
		size     = 2 << 20
		readSize = 4096
	)

	f := putFramedObject(t, ts, backend, "seekable/budget", size)
	stored := int64(len(f.stored))

	routes := []struct {
		name   string
		offset int64
		// discovery is what the request that established the encoding transferred before the framed
		// path ran. On the 206 route that is the object bytes the range asked for, which the framed
		// read then discards. On the 416 route it is no object bytes at all — only S3's error
		// document, a few hundred bytes of XML, which BytesRead counts because it counts response
		// bodies. An allowance rather than an exact figure for that one, since the document's length
		// is the endpoint's business and not a property of this read path.
		discovery int64
		// wantInsideStored is asserted rather than assumed. These fixtures are ~50% compressible, so
		// if the ratio moves both cases become the same case and the coverage halves silently.
		wantInsideStored bool
	}{
		{"a 206 discovers the encoding", stored / 2, readSize, true},
		{"a 416 discovers the encoding", stored + (size-stored)/2, 1024, false},
	}

	//nolint:paralleltest // each case resets the shared request recorder; they must not overlap
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			if inside := r.offset < stored; inside != r.wantInsideStored {
				t.Fatalf("offset %d against a %d-byte stored body is inside=%v, want inside=%v; the "+
					"compression ratio moved and this case no longer exercises the route it names",
					r.offset, stored, inside, r.wantInsideStored)
			}

			// A fresh backend per route so the metrics below count this read alone.
			backend := ts.Backend(func(cfg *s3.Config) {
				cfg.Compression.Enabled = true
				cfg.Compression.Algorithm = "zstd"
				cfg.Compression.Level = 3
				cfg.Compression.MinSize = "4KB"
				cfg.ParallelReadThreshold = 0
			})

			_, spanLen := f.spanFor(r.offset, readSize)

			// What the framed read itself must fetch: the index frame plus the frames covering the
			// range, and nothing else.
			framed := f.desc.IndexLength + spanLen

			ts.ResetRequests()

			got, err := backend.GetObject(context.Background(), f.key, r.offset, readSize)
			if err != nil {
				t.Fatalf("GetObject(%d, %d): %v", r.offset, readSize, err)
			}

			if want := f.content[r.offset : r.offset+readSize]; !bytes.Equal(got, want) {
				t.Fatalf("the cheap read returned the wrong bytes, which makes the byte count below "+
					"irrelevant: %d bytes not matching content[%d:%d]",
					len(got), r.offset, r.offset+readSize)
			}

			n := ts.BytesRead(f.key)
			if budget := framed + r.discovery; n > budget {
				t.Errorf("a %d-byte read at offset %d transferred %d bytes, want at most %d — a "+
					"%d-byte index, %d bytes of frames, and up to %d bytes for the request that "+
					"discovered the encoding. The stored body is %d bytes, so %.1f%% of it crossed the "+
					"wire.\nRequests: %s",
					readSize, r.offset, n, budget, f.desc.IndexLength, spanLen, r.discovery, stored,
					float64(n)/float64(stored)*100, describe(ts.Requests()))
			}

			// And the counter says the read was served from frames, not merely that it was cheap.
			// Without this, a change that made the whole-object path cheap some other way would
			// satisfy the budget above while the framed path had quietly stopped being used.
			m := backend.GetMetrics()
			if m.SeekableReads != 1 {
				t.Errorf("SeekableReads = %d after one framed read, want 1 (fallbacks: %d, last "+
					"reason %q)", m.SeekableReads, m.SeekableWholeFallbacks,
					m.SeekableLastFallbackReason)
			}

			// Exact, not a bound. This metric is what an operator divides by SeekableReads to see what
			// framing is buying, so it has to be the stored bytes the framed read actually pulled —
			// counting the discovery request in it would flatter the 206 route, and counting the
			// returned content instead of the stored bytes would flatter both.
			if m.SeekableReadBytes != framed {
				t.Errorf("SeekableReadBytes = %d, want %d (a %d-byte index plus %d bytes of frames); "+
					"the metric is the stored bytes the framed read pulled over the wire",
					m.SeekableReadBytes, framed, f.desc.IndexLength, spanLen)
			}

			t.Logf("%d bytes at offset %d cost %d of %d stored bytes (%.1f%%), of which %d was the "+
				"framed read and %d the encoding discovery",
				readSize, r.offset, n, stored, float64(n)/float64(stored)*100, framed, r.discovery)
		})
	}
}

// TestFramedReadFallbackReasonsAreReachedAndReported is what keeps the fallback reasons honest.
//
// Each case builds an object in a state that must decline, asserts the read is still byte-for-byte
// correct, and asserts *which* reason was recorded. The last part is the point. Every one of these
// states declines, so a table that only checked "a fallback happened" would pass with every reason
// wired to the same string, or with two cases taking each other's branch — and the reason is the only
// thing an operator has to work from when reads get slow.
func TestFramedReadFallbackReasonsAreReachedAndReported(t *testing.T) {
	t.Parallel()

	const (
		size     = 2 << 20
		readSize = 4096
	)

	// Halfway in: past the end of the stored body for these ~50%-compressible fixtures, and never a
	// frame boundary, so the whole-object fallback has to slice correctly too.
	const readAt = size / 2

	cases := []struct {
		name string
		// why explains what the fixture models, since these are all states ObjectFS does not write.
		why string
		// prepare damages the object. nil means read it as written.
		prepare func(t *testing.T, ts *testaws.TestServer, f framedObject)
		// arm installs faults, and returns how many must have fired. A fault that matches nothing
		// produces a passing test that proves nothing, which is what FaultsFired guards.
		arm        func(t *testing.T, ts *testaws.TestServer, f framedObject) int
		readAt     int64
		readSize   int64
		wantReason string
	}{
		{
			name: "the range covers every frame",
			why: "reading a whole framed object. The framed path would fetch the index and then the " +
				"entire body in two requests where the whole-object path needs one, so declining is " +
				"the cheaper request rather than a missed optimization.",
			readAt:     0,
			readSize:   size,
			wantReason: "range_covers_every_frame",
		},
		{
			name: "the descriptor does not parse",
			why: "a descriptor edited by hand or mangled in transit. Anyone with s3:PutObject can " +
				"write this, so it is routine bad input rather than corruption.",
			prepare: func(t *testing.T, ts *testaws.TestServer, f framedObject) {
				reseed(t, ts, f, func(_ []byte, meta map[string]string) {
					meta[metaSeekableKey] = "1/262144/not-a-number/264"
				})
			},
			readAt:     readAt,
			readSize:   readSize,
			wantReason: "descriptor_unparsable",
		},
		{
			name: "the index frame does not verify",
			why: "bit-rot inside the index frame's payload. The frame is still structurally " +
				"skippable, so the object remains a legal zstd stream that decodes whole — which is " +
				"why declining here does not hide anything: the whole-object path checks " +
				"objectfs-sha256 over the result.",
			prepare: func(t *testing.T, ts *testaws.TestServer, f framedObject) {
				reseed(t, ts, f, func(stored []byte, _ map[string]string) {
					// Past the 8-byte skippable header, so the frame's magic and length survive and
					// only the payload the index's own hash covers is damaged.
					stored[16] ^= 0xFF
				})
			},
			readAt:     readAt,
			readSize:   readSize,
			wantReason: "index_unusable",
		},
		{
			name: "the object is not zstd",
			why: "a descriptor on an object whose Content-Encoding this build cannot frame-decode, " +
				"which is what a CopyObject that rewrote the encoding leaves behind.",
			prepare: func(t *testing.T, ts *testaws.TestServer, f framedObject) {
				// The same content and the same metadata — descriptor included — stored as gzip. The
				// integrity keys still describe the content, so the whole-object path verifies as
				// normal and the only thing wrong with the object is that its descriptor promises
				// frames its encoding cannot have.
				seedRaw(t, ts, f.key, gzipBytes(t, f.content), f.meta, "gzip")
			},
			readAt:     readAt,
			readSize:   readSize,
			wantReason: "no_frame_decoder",
		},
		{
			name: "the index cannot be fetched",
			why: "a ranged GET refused where an unranged one is allowed — a bucket policy with a " +
				"condition on the Range header, or an endpoint that answers prefix requests " +
				"differently. Non-retryable, so the SDK does not paper over it.",
			arm: func(t *testing.T, ts *testaws.TestServer, f framedObject) int {
				// bytes=0- is the index prefix. The read that discovers the encoding starts at readAt
				// and the whole-object fallback also sends bytes=0-, so the budget is exactly one:
				// the index attempt claims it and the fallback finds none left.
				ts.InjectFault(testaws.Fault{
					Method:      "GET",
					KeySuffix:   f.key,
					RangePrefix: "bytes=0-",
					Status:      403,
					Code:        "AccessDenied",
					Times:       1,
				})

				return 1
			},
			readAt:     readAt,
			readSize:   readSize,
			wantReason: "frame_fetch_failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			ts.RequireRangeGET()

			backend := ts.Backend(func(cfg *s3.Config) {
				cfg.Compression.Enabled = true
				cfg.Compression.Algorithm = "zstd"
				cfg.Compression.Level = 3
				cfg.Compression.MinSize = "4KB"
				cfg.ParallelReadThreshold = 0
			})

			f := putFramedObject(t, ts, backend, "seekable/fallback", size)

			if tc.prepare != nil {
				tc.prepare(t, ts, f)
			}

			wantFires := 0
			if tc.arm != nil {
				wantFires = tc.arm(t, ts, f)
			}

			got, err := backend.GetObject(context.Background(), f.key, tc.readAt, tc.readSize)
			if err != nil {
				t.Fatalf("GetObject(%d, %d) failed instead of falling back: %v\nA declined framed read "+
					"must still return the object: %s", tc.readAt, tc.readSize, err, tc.why)
			}

			// Correctness first. A fallback that returns the wrong bytes is worse than no fallback:
			// the reason counter would still say the right thing while the read was broken.
			if want := f.content[tc.readAt : tc.readAt+tc.readSize]; !bytes.Equal(got, want) {
				t.Fatalf("a declined framed read returned %d bytes that do not match content[%d:%d]. "+
					"%s", len(got), tc.readAt, tc.readAt+tc.readSize, tc.why)
			}

			if fired := ts.FaultsFired(); fired != wantFires {
				t.Fatalf("%d faults fired, want %d; a fault that matches nothing makes this case pass "+
					"without ever reaching the branch it names", fired, wantFires)
			}

			m := backend.GetMetrics()
			if m.SeekableWholeFallbacks != 1 {
				t.Errorf("SeekableWholeFallbacks = %d, want 1 (reads served from frames: %d). %s",
					m.SeekableWholeFallbacks, m.SeekableReads, tc.why)
			}

			if m.SeekableLastFallbackReason != tc.wantReason {
				t.Errorf("SeekableLastFallbackReason = %q, want %q.\nThe read fell back correctly but "+
					"for a different recorded cause than this fixture models, so an operator reading "+
					"the counter would be sent to the wrong explanation. %s",
					m.SeekableLastFallbackReason, tc.wantReason, tc.why)
			}
		})
	}
}

// TestReadOfAnUnframedObjectIsNotCountedAsAFallback pins the one decline that is deliberately silent.
//
// An object with no descriptor is the overwhelming majority of objects — every uncompressed one, every
// one written by another tool, every compressed one below the frame-count floor. Counting those would
// make SeekableWholeFallbacks track how much of the bucket is unframed, and bury the handful of
// fallbacks that mean something under a number nobody can act on.
func TestReadOfAnUnframedObjectIsNotCountedAsAFallback(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = true
		cfg.Compression.Algorithm = "zstd"
		cfg.Compression.Level = 3
		cfg.Compression.MinSize = "4KB"
		cfg.ParallelReadThreshold = 0
	})

	ctx := context.Background()

	// Small enough to compress into a single frame, so it is compressed but not framed — the case that
	// exercises the encoded read path with no descriptor to find.
	const (
		key      = "seekable/unframed"
		size     = 64 << 10
		readSize = 4096
	)

	content := semiCompressible(key, size)
	if err := backend.PutObject(ctx, key, content, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if _, ok := ts.ObjectMetadata(key)[metaSeekableKey]; ok {
		t.Fatalf("a %d-byte object was framed; this test needs a compressed object with no "+
			"descriptor, and DeriveFrameSize's floor is what normally guarantees one", size)
	}

	got, err := backend.GetObject(ctx, key, 4096, readSize)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}

	if want := content[4096 : 4096+readSize]; !bytes.Equal(got, want) {
		t.Fatalf("reading an unframed compressed object returned %d wrong bytes", len(got))
	}

	m := backend.GetMetrics()
	if m.SeekableWholeFallbacks != 0 {
		t.Errorf("SeekableWholeFallbacks = %d after reading an object that was never framed, want 0 "+
			"(last reason %q).\nThe counter is for framed objects that could not be served from their "+
			"frames. Counting unframed objects makes it a measure of the bucket instead, and the "+
			"fallbacks worth alerting on become invisible in it",
			m.SeekableWholeFallbacks, m.SeekableLastFallbackReason)
	}

	if m.SeekableReads != 0 {
		t.Errorf("SeekableReads = %d for an object with no frames, want 0", m.SeekableReads)
	}
}

// TestFramedReadDetectsAnOverwriteBetweenTheIndexAndTheFrames covers the race the two-request shape
// creates, and it is the one failure here with no symptom of its own.
//
// The index and the frames are separate requests, so an overwrite in between is read as one object
// when it is two: both requests succeed, the index parses, the lengths add up, and the frames get
// decoded against offsets from an index that no longer describes them. What the read would return is
// bytes from the new object interpreted through the old object's layout.
//
// The fault fires on the frame fetch and replaces the object from OnFire, which is what that hook
// exists for — a goroutine racing the read to arrange the same interleaving would be a flake. The 500
// is retryable, so the SDK re-issues the frame request and it succeeds against the new generation:
// index ETag from before, frame ETag from after.
//
// Without the ETag comparison this does not silently return wrong bytes — the per-frame hashes catch
// it and the read fails as corruption. That is the mutation this test was verified against, and it is
// why the assertion is that the read *succeeds*: an error here means the race is being reported to the
// caller as damaged data when the object is intact and the read is retryable in the cheapest possible
// way, by fetching it whole.
func TestFramedReadDetectsAnOverwriteBetweenTheIndexAndTheFrames(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = true
		cfg.Compression.Algorithm = "zstd"
		cfg.Compression.Level = 3
		cfg.Compression.MinSize = "4KB"
		cfg.ParallelReadThreshold = 0
	})

	ctx := context.Background()

	const (
		size     = 2 << 20
		readSize = 4096
		readAt   = size / 2
	)

	f := putFramedObject(t, ts, backend, "seekable/overwrite", size)

	// The replacement is a different framed object of the same size, so the frames at the byte offsets
	// the old index names are real frames — just the wrong ones. An object of a different size would
	// make the range short and the mismatch detectable for the wrong reason.
	replacement := semiCompressible("seekable/overwrite/second", size)

	spanStart, _ := f.spanFor(readAt, readSize)

	// Matched on the frame fetch specifically. The index fetch is bytes=0-, and the read that
	// discovers the encoding starts at readAt, so this prefix reaches neither.
	ts.InjectFault(testaws.Fault{
		Method:      "GET",
		KeySuffix:   f.key,
		RangePrefix: fmt.Sprintf("bytes=%d-", spanStart),
		Status:      500,
		Code:        "InternalError",
		Times:       1,
		OnFire: func() {
			if err := backend.PutObject(ctx, f.key, replacement, nil); err != nil {
				t.Errorf("OnFire: replacing the object mid-read: %v", err)
			}
		},
	})

	got, err := backend.GetObject(ctx, f.key, readAt, readSize)
	if err != nil {
		t.Fatalf("GetObject(%d, %d) after a mid-read overwrite: %v\nAn object replaced between the "+
			"index fetch and the frame fetch is not damaged data — one whole-object GET produces a "+
			"self-consistent view of whichever generation is current, so this should fall back rather "+
			"than fail", readAt, readSize, err)
	}

	if fired := ts.FaultsFired(); fired != 1 {
		t.Fatalf("%d faults fired, want 1: the fault is matched on the frame fetch's Range (%s), so "+
			"if it did not fire the overwrite never happened and this test proves nothing",
			fired, fmt.Sprintf("bytes=%d-", spanStart))
	}

	// The read must return the current generation, self-consistently. Returning the old object's bytes
	// would mean the read had served a generation that no longer exists; returning a mixture would mean
	// the ETag check had not worked.
	if want := replacement[readAt : readAt+readSize]; !bytes.Equal(got, want) {
		t.Errorf("after a mid-read overwrite the read returned %d bytes that are not the current "+
			"object's content at [%d:%d]", len(got), readAt, readAt+readSize)
	}

	m := backend.GetMetrics()
	if m.SeekableLastFallbackReason != "object_changed_mid_read" {
		t.Errorf("SeekableLastFallbackReason = %q, want %q (fallbacks %d, framed reads %d).\nThe read "+
			"recovered, but for a recorded cause other than the race it hit — and this reason is the "+
			"only evidence that an object is being overwritten under its readers",
			m.SeekableLastFallbackReason, "object_changed_mid_read",
			m.SeekableWholeFallbacks, m.SeekableReads)
	}
}

// TestFramedReadReportsDamagedFramesRatherThanFallingBack is the integrity boundary of this path, and
// the one place it returns an error instead of quietly costing more.
//
// Every other decline here is structural — something about the object's shape or metadata means the
// frames cannot be used, and the whole-object path is a correct answer to it. A frame whose bytes do
// not match the hash the index recorded is different in kind: the stored bytes are wrong, a retry
// decodes the same wrong bytes to the same mismatch, and falling back would return a value the
// object's own integrity data contradicts.
//
// It is also the case where a fallback would be actively harmful. The whole-object path is not a
// second opinion — the same damaged frame is part of that stream too — so the choice is not between a
// cheap wrong answer and an expensive right one. It is between reporting corruption and hiding it
// behind whatever the codec makes of the damage.
func TestFramedReadReportsDamagedFramesRatherThanFallingBack(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = true
		cfg.Compression.Algorithm = "zstd"
		cfg.Compression.Level = 3
		cfg.Compression.MinSize = "4KB"
		cfg.ParallelReadThreshold = 0
	})

	const (
		size     = 2 << 20
		readSize = 4096
		readAt   = size / 2
	)

	f := putFramedObject(t, ts, backend, "seekable/damaged-frame", size)

	// A byte inside the frame the read will need, chosen from the index rather than guessed, so the
	// damage is certain to be in the fetched span. Damaging some other frame would leave this read
	// succeeding and the test passing for no reason.
	spanStart, _ := f.spanFor(readAt, readSize)
	at := int(spanStart) + 8

	reseed(t, ts, f, func(stored []byte, _ map[string]string) {
		stored[at] ^= 0xFF
	})

	got, err := backend.GetObject(context.Background(), f.key, readAt, readSize)
	requireCorruptionError(t, err, got, readSize)
}
