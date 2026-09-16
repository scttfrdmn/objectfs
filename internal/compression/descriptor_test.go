package compression

import (
	"crypto/sha256"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestDescriptorRoundTripsAFramedObject is the property the descriptor exists for: everything a
// reader needs to fetch the index in one correctly-sized request comes back out of the text form.
//
// Built from a real framed object rather than from a struct literal, so the IndexLength under test is
// the one CompressFramed actually emitted. A descriptor that round-trips but disagrees with the
// object is the failure worth catching, and only an end-to-end fixture can catch it.
func TestDescriptorRoundTripsAFramedObject(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)

	for _, tc := range []struct {
		name      string
		size      int
		frameSize int64
	}{
		{"many frames", 300000, 4096},
		{"two frames", 8192, 4096},
		{"one frame", 1000, 4096},
		{"one byte per frame", 40, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src := compressibleBytes(tc.size)
			obj, idx := framed(t, c, src, tc.frameSize)

			indexLength := idx.Frames[0].CompressedOffset
			desc := DescribeFrameIndex(idx, indexLength)

			// The descriptor's IndexLength must name exactly the prefix a reader would fetch, so the
			// index has to parse out of precisely that many bytes and no more.
			if _, _, err := ParseFrameIndex(obj[:desc.IndexLength]); err != nil {
				t.Fatalf("the index does not parse from the first %d bytes the descriptor names: %v",
					desc.IndexLength, err)
			}

			got, err := ParseSeekableDescriptor(desc.String())
			if err != nil {
				t.Fatalf("ParseSeekableDescriptor(%q): %v", desc.String(), err)
			}
			if got != desc {
				t.Errorf("round trip of %q gave %+v, want %+v", desc.String(), got, desc)
			}

			if got.FrameSize != tc.frameSize {
				t.Errorf("FrameSize = %d, want %d", got.FrameSize, tc.frameSize)
			}
			if got.FrameCount != int64(len(idx.Frames)) {
				t.Errorf("FrameCount = %d, want %d", got.FrameCount, len(idx.Frames))
			}
			if got.Version != FrameIndexVersion {
				t.Errorf("Version = %d, want %d", got.Version, FrameIndexVersion)
			}

			t.Logf("%d bytes at frame %d: descriptor %q (%d bytes)",
				tc.size, tc.frameSize, desc.String(), len(desc.String()))
		})
	}
}

// TestParseSeekableDescriptorRejectsGarbage covers the whole refusal surface. Every case here is
// reachable from outside: S3 user metadata is writable by anyone holding s3:PutObject, and
// `aws s3 cp --metadata` carries an arbitrary value through unchanged, so a reader that spent
// IndexLength without checking it would issue a prefix GET of up to 4 GiB on a four-byte edit.
//
// Each row asserts *which* check rejected it, not merely that something did, and that is not
// belt-and-braces. The first version of this table hard-coded an index length of 2648 — copied from
// this file's own doc-comment example, which was itself wrong — and 2648 is not the consistent length
// for 64 frames, so a dozen rows were being rejected by the self-consistency check before reaching the
// thing they were named for. The `unknown version` row in particular passed while the version check
// was disabled entirely, which is how a mutation run found it. A table of rejections is the shape of
// test most likely to pass for the wrong reason, because every wrong reason is still a rejection.
func TestParseSeekableDescriptorRejectsGarbage(t *testing.T) {
	t.Parallel()

	// The baseline must parse, or every row below proves nothing: a table of rejections passes
	// trivially if the shape it is perturbing was never accepted.
	good := SeekableDescriptor{
		Version:     FrameIndexVersion,
		FrameSize:   1 << 20,
		FrameCount:  64,
		IndexLength: skippableHeaderSize + indexPayloadLen(64),
	}
	if _, err := ParseSeekableDescriptor(good.String()); err != nil {
		t.Fatalf("the baseline descriptor %q does not parse: %v", good.String(), err)
	}

	// The consistent index lengths, computed rather than written: 2664 for 64 frames and 2704 for 65.
	// Every row that is not about the index length uses the correct one, so that its own defect is the
	// only thing left for the parser to find.
	//
	// strconv.Itoa rather than this package's own itoa helper: that one lives in a file behind
	// `//go:build !integration`, so referencing it from here compiled fine under `go test` and broke
	// `go vet -tags=integration ./...` — a CI job, in a file with no build constraint of its own.
	consistent := strconv.Itoa(int(skippableHeaderSize + indexPayloadLen(64)))
	forOneMoreFrame := strconv.Itoa(int(skippableHeaderSize + indexPayloadLen(65)))

	for _, tc := range []struct {
		name string
		text string
		// want is a substring of the error, identifying which check fired.
		want string
	}{
		{"empty", "", "fields"},
		{"too few fields", "1/1048576/64", "fields"},
		{"too many fields", "1/1048576/64/" + consistent + "/0", "fields"},
		{"non-numeric version", "v1/1048576/64/" + consistent, "version"},
		{"non-numeric frame size", "1/1MiB/64/" + consistent, "frame size"},
		{"negative frame size", "1/-1048576/64/" + consistent, "frame size"},
		{"zero frame size", "1/0/64/" + consistent, "frame size is zero"},
		{"zero frame count", "1/1048576/0/" + consistent, "frame count is zero"},
		{"zero index length", "1/1048576/64/0", "index length is zero"},
		{"version over a byte", "256/1048576/64/" + consistent, "version"},
		{"frame size over uint32", "1/4294967296/64/" + consistent, "frame size"},
		{"frame count over uint32", "1/1048576/4294967296/" + consistent, "frame count"},
		{"index length over uint32", "1/1048576/64/4294967296", "index length"},
		{"index length off by one", "1/1048576/64/" + strconv.Itoa(int(skippableHeaderSize+indexPayloadLen(64))+1), "inconsistent"},
		{"index length for a different frame count", "1/1048576/64/" + forOneMoreFrame, "inconsistent"},
		{"unknown version", "2/1048576/64/" + consistent, "is not"},
		{"leading space", " 1/1048576/64/" + consistent, "version"},
		{"trailing newline", "1/1048576/64/" + consistent + "\n", "index length"},
		{"plus sign", "1/+1048576/64/" + consistent, "frame size"},
		{"empty field", "1//64/" + consistent, "frame size"},
		{"whole thing is separators", "///", "version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseSeekableDescriptor(tc.text)
			if err == nil {
				t.Fatalf("ParseSeekableDescriptor(%q) returned %+v, want an error", tc.text, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ParseSeekableDescriptor(%q) was rejected by the wrong check.\n got: %v\nwant an error mentioning %q",
					tc.text, err, tc.want)
			}
		})
	}
}

// TestMaxSeekableDescriptorLenBoundsEveryDescriptor is the assertion internal/vfs depends on. It
// reserves MaxSeekableDescriptorLen bytes of the 2 KB user-metadata budget for this key, and S3
// rejects an over-budget PUT rather than truncating it — so a descriptor wider than the reservation
// is a setfattr that succeeds and a flush that fails, reported to a caller that cannot act on it.
func TestMaxSeekableDescriptorLenBoundsEveryDescriptor(t *testing.T) {
	t.Parallel()

	// The widest values the format's fields can hold, which is a superset of what any policy limit
	// allows. Not the widest *reachable* values: MaxFrameSize and maxIndexPayload are both smaller,
	// and reserving against the wire rather than against current policy is what keeps the reservation
	// correct when a limit is tuned.
	widest := SeekableDescriptor{
		Version:     math.MaxUint8,
		FrameSize:   math.MaxUint32,
		FrameCount:  math.MaxUint32,
		IndexLength: math.MaxUint32,
	}
	if got := len(widest.String()); got != MaxSeekableDescriptorLen {
		t.Errorf("the widest descriptor is %d bytes but MaxSeekableDescriptorLen is %d",
			got, MaxSeekableDescriptorLen)
	}

	// And every descriptor a writer can actually produce is inside it. Swept over the frame-size range
	// at the frame counts the index ceiling permits, rather than asserted from the arithmetic, because
	// the arithmetic is what is under test.
	for frameSize := int64(MinFrameSize); frameSize <= MaxFrameSize; frameSize <<= 1 {
		for _, count := range []int64{1, 2, 1 << 10, 1 << 20, maxIndexPayload / frameRecordSize} {
			d := SeekableDescriptor{
				Version:     FrameIndexVersion,
				FrameSize:   frameSize,
				FrameCount:  count,
				IndexLength: skippableHeaderSize + indexPayloadLen(count),
			}
			if got := len(d.String()); got > MaxSeekableDescriptorLen {
				t.Errorf("descriptor %q is %d bytes, over the %d reserved",
					d.String(), got, MaxSeekableDescriptorLen)
			}
		}
	}

	t.Logf("reserved %d bytes; widest is %q", MaxSeekableDescriptorLen, widest.String())
}

// TestCompressorFramesWhatItShouldAndDeclinesWhatItShouldNot pins the five declines documented on
// CompressFramed. Each one is an ordinary outcome rather than an error, which means a bug in any of
// them is silent: framing something that should not be framed costs stored size, and declining
// something that should be framed restores the whole-object read amplification the format exists to
// remove. Neither reports anything on its own.
func TestCompressorFramesWhatItShouldAndDeclinesWhatItShouldNot(t *testing.T) {
	t.Parallel()

	// Large enough that DeriveFrameSize's floor still leaves several frames, and compressible so the
	// framed body is smaller than the input.
	big := compressibleBytes(4 << 20)

	for _, tc := range []struct {
		name       string
		settings   Settings
		data       []byte
		wantFramed bool
		why        string
	}{
		{
			name:       "zstd frames a large compressible object",
			settings:   Settings{Enabled: true, Algorithm: "zstd"},
			data:       big,
			wantFramed: true,
			why:        "this is the case the format exists for",
		},
		{
			name:       "compression disabled",
			settings:   Settings{Enabled: false, Algorithm: "zstd"},
			data:       big,
			wantFramed: false,
			why:        "a mount with compression off must not change what it stores",
		},
		{
			name:       "gzip",
			settings:   Settings{Enabled: true, Algorithm: "gzip"},
			data:       big,
			wantFramed: false,
			why:        "gzip has no skippable frame a standard decoder must ignore",
		},
		{
			name:       "lz4",
			settings:   Settings{Enabled: true, Algorithm: "lz4"},
			data:       big,
			wantFramed: false,
			why:        "as gzip",
		},
		{
			name:       "below the configured minimum",
			settings:   Settings{Enabled: true, Algorithm: "zstd", MinSize: "8MB"},
			data:       big,
			wantFramed: false,
			why:        "the size floor applies to framing exactly as it applies to compression",
		},
		{
			name:       "single frame",
			settings:   Settings{Enabled: true, Algorithm: "zstd"},
			data:       compressibleBytes(MinFrameSize),
			wantFramed: false,
			why:        "an index over one frame is 144 bytes buying nothing to seek to",
		},
		{
			name:       "already a compressed format",
			settings:   Settings{Enabled: true, Algorithm: "zstd"},
			data:       alreadyCompressedBytes(t, big),
			wantFramed: false,
			why:        "same prefix check Compress uses; re-framing a zstd file wastes the encode",
		},
		{
			name:       "incompressible",
			settings:   Settings{Enabled: true, Algorithm: "zstd"},
			data:       incompressibleBytes(4 << 20),
			wantFramed: false,
			why:        "a framed body no smaller than the input is discarded, as in Compress",
		},
		{
			name:       "empty",
			settings:   Settings{Enabled: true, Algorithm: "zstd"},
			data:       nil,
			wantFramed: false,
			why:        "nothing to seek within, and CompressFramed refuses an empty source",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			comp, err := NewCompressor(tc.settings)
			if err != nil {
				t.Fatalf("NewCompressor(%+v): %v", tc.settings, err)
			}

			body, desc, framedOK, err := comp.CompressFramed(tc.data, sha256.Sum256(tc.data))
			if err != nil {
				t.Fatalf("CompressFramed: %v", err)
			}
			if framedOK != tc.wantFramed {
				t.Fatalf("framed = %v, want %v: %s", framedOK, tc.wantFramed, tc.why)
			}

			if !framedOK {
				// A decline must return nothing usable, so a caller that ignores the boolean cannot
				// upload an empty body or record an all-zero descriptor.
				if body != nil {
					t.Errorf("declined to frame but returned %d bytes of body", len(body))
				}
				if desc != (SeekableDescriptor{}) {
					t.Errorf("declined to frame but returned descriptor %+v", desc)
				}
				return
			}

			if int64(len(body)) >= int64(len(tc.data)) {
				t.Errorf("framed body is %d bytes for %d bytes of input, which should have been declined",
					len(body), len(tc.data))
			}

			// The descriptor must describe the body that was actually returned.
			parsed, err := ParseSeekableDescriptor(desc.String())
			if err != nil {
				t.Fatalf("the descriptor this call produced does not parse: %q: %v", desc.String(), err)
			}
			idx, _, err := ParseFrameIndex(body[:parsed.IndexLength])
			if err != nil {
				t.Fatalf("the index does not parse from the %d-byte prefix the descriptor names: %v",
					parsed.IndexLength, err)
			}
			if int64(len(idx.Frames)) != parsed.FrameCount {
				t.Errorf("index has %d frames, descriptor says %d", len(idx.Frames), parsed.FrameCount)
			}
			if idx.FrameSize != parsed.FrameSize {
				t.Errorf("index frame size %d, descriptor says %d", idx.FrameSize, parsed.FrameSize)
			}
			if parsed.FrameCount < 2 {
				t.Errorf("framed into %d frames: framing is only worth doing above one",
					parsed.FrameCount)
			}
			if idx.ContentSHA256 != sha256.Sum256(tc.data) {
				t.Error("the index does not carry the whole-content hash it was given")
			}

			// And the whole thing still decodes as one stream, which is the compatibility guarantee.
			whole, decoded, err := comp.Decompress(body, comp.ContentEncoding())
			if err != nil {
				t.Fatalf("Decompress over the framed body: %v", err)
			}
			if !decoded {
				t.Fatalf("Decompress did not claim %q, so the framed body would reach a caller encoded",
					comp.ContentEncoding())
			}
			if len(whole) != len(tc.data) {
				t.Fatalf("whole-stream decode gave %d bytes, want %d", len(whole), len(tc.data))
			}

			t.Logf("%d bytes -> %d framed (%.1f%%), descriptor %q",
				len(tc.data), len(body), 100*float64(len(body))/float64(len(tc.data)), desc.String())
		})
	}
}

// TestEstimateRatioIsCloseEnoughForDeriveFrameSize is the claim EstimateRatio's doc comment rests on:
// a sample is good enough because DeriveFrameSize rounds to a power of two, so only a gross error
// moves the answer.
//
// Asserted as "the derived frame size agrees with the one the true ratio gives", not as "the ratio is
// within x%". The ratio's accuracy is not a property anyone depends on — the frame size is.
//
// The frame size is derived at *synthetic* object sizes rather than at the fixture's own size, and
// that is the difference between this test asserting something and asserting nothing. DeriveFrameSize
// clamps to MinFrameSize below roughly 100 MB of compressible content, so at any size a test can
// afford to allocate, every ratio in the table — true or estimated, right or wrong by 15x — produces
// exactly 262144 and the comparison is between two constants. The ratio is measured on a real buffer;
// the sizes it is then combined with span the range where the answer actually varies.
func TestEstimateRatioIsCloseEnoughForDeriveFrameSize(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)

	// Sizes chosen to land inside DeriveFrameSize's unclamped middle at the ratios below, so a bad
	// estimate can move the answer. 1 GiB up to 1 TiB is also the range of object where framing is
	// worth the most: the amplification a whole-object read pays scales with object size.
	sizes := []int64{1 << 30, 10 << 30, 100 << 30, 1 << 40}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"compressible", compressibleBytes(8 << 20)},
		{"incompressible", incompressibleBytes(8 << 20)},

		// An even split is representative by construction and proves little: zstd's ratio over a
		// concatenation is very nearly the sum of its parts, so these two rows measure the same 1.89
		// whichever order the halves are in. Kept as the control.
		{"incompressible half then compressible half", slices.Concat(
			incompressibleBytes(4<<20), compressibleBytes(4<<20))},
		{"compressible half then incompressible half", slices.Concat(
			compressibleBytes(4<<20), incompressibleBytes(4<<20))},

		// These four are the real test. A small unrepresentative region is the case that occurs in the
		// formats this project's users store: a tar whose first member is a JPEG, a CSV or FASTQ header
		// that looks nothing like the body, a trailing checksum block.
		//
		// The two "small head" rows are the ones that failed under prefix-only sampling, and they are
		// why ratioSampleWindows is three. Measured, prefix-only vs three-window:
		//
		//	small incompressible head   true 5.31   est 1.00 (0.19x)  ->  est 2.58 (0.49x)
		//	small compressible head     true 1.13   est 16.90 (15x)   ->  est 1.46 (1.28x)
		//
		// 0.19x and 15x are both about two powers of two of frame size, past what F's square-root
		// relationship to the ratio absorbs.
		{"small incompressible head", slices.Concat(
			incompressibleBytes(1<<20), compressibleBytes(7<<20))},
		{"small compressible head", slices.Concat(
			compressibleBytes(1<<20), incompressibleBytes(7<<20))},
		{"small incompressible tail", slices.Concat(
			compressibleBytes(7<<20), incompressibleBytes(1<<20))},
		{"small incompressible middle", slices.Concat(
			compressibleBytes(3<<20), incompressibleBytes(1<<20), compressibleBytes(4<<20))},

		{"smaller than the sample", compressibleBytes(64 << 10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			whole, err := c.Compress(tc.data)
			if err != nil {
				t.Fatalf("Compress: %v", err)
			}
			trueRatio := float64(len(tc.data)) / float64(len(whole))
			estimated := c.EstimateRatio(tc.data)

			t.Logf("ratio true %.2f estimated %.2f (%.2fx)", trueRatio, estimated, estimated/trueRatio)

			for _, size := range sizes {
				wantFrame := DeriveFrameSize(size, trueRatio)
				gotFrame := DeriveFrameSize(size, estimated)

				// One power of two of slack, which is the coarsest the answer can be wrong by and still be
				// the neighbor of the right one. Frame size goes as the square root of the ratio, so the
				// ratio has to be off by 4x to move the frame size by one step; the worst row here is
				// 0.49x on the ratio, which is 0.70x on the frame size. The bias is real, not absent —
				// this bound is what ratioSampleWindows was raised from one to three to satisfy.
				if gotFrame > wantFrame*2 || gotFrame*2 < wantFrame {
					t.Errorf("at %d bytes: estimated ratio %.2f gives frame size %d, true ratio %.2f "+
						"gives %d — more than one power of two apart",
						size, estimated, gotFrame, trueRatio, wantFrame)
				}
			}
		})
	}
}

// TestDeriveFrameSizeVariesOverTheSizesTheRatioTestUses is the guard on the test above. Its whole
// argument is that those synthetic sizes reach DeriveFrameSize's unclamped middle; if a change to
// MinFrameSize, MaxFrameSize, or the cost coefficient pushed them all against a clamp, that test
// would keep passing while comparing constants.
func TestDeriveFrameSizeVariesOverTheSizesTheRatioTestUses(t *testing.T) {
	t.Parallel()

	seen := map[int64]bool{}
	for _, size := range []int64{1 << 30, 10 << 30, 100 << 30, 1 << 40} {
		for _, ratio := range []float64{1, 2, 4, 8, 16} {
			seen[DeriveFrameSize(size, ratio)] = true
		}
	}

	if len(seen) < 3 {
		t.Errorf("DeriveFrameSize returns only %d distinct sizes over the ratio test's inputs (%v), "+
			"so that test compares constants and asserts nothing about the estimate", len(seen), seen)
	}
	t.Logf("%d distinct frame sizes over those inputs", len(seen))
}

// TestEstimateRatioOnDegenerateInput covers the two inputs with nothing to measure. Both must answer
// 1 rather than dividing by zero or returning an Inf that DeriveFrameSize would then have to defend
// against — it does defend against it, and this is the check that the defense is not the only one.
func TestEstimateRatioOnDegenerateInput(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := c.EstimateRatio(tc.data); got != 1 {
				t.Errorf("EstimateRatio(%s) = %v, want 1", tc.name, got)
			}
		})
	}
}

// alreadyCompressedBytes returns bytes AlreadyCompressed recognizes, built by actually compressing
// rather than by writing a magic number: the point is that a real zstd file is declined, and a
// hand-written prefix would pass the check while proving nothing about a real one.
func alreadyCompressedBytes(t *testing.T, src []byte) []byte {
	t.Helper()
	c := framedCodec(t)
	out, err := c.Compress(src)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if !AlreadyCompressed(out) {
		t.Fatal("AlreadyCompressed does not recognize this package's own zstd output, so the " +
			"already-compressed row would be testing the wrong decline")
	}
	return out
}

// TestSeekableDescriptorStringIsSeparatorSafe pins the one property the text form needs that is easy
// to lose: no field may contain the separator, or a descriptor would parse into the wrong fields
// rather than being rejected. Decimal digits cannot contain "/", so this is a check that no field
// gained a non-numeric rendering.
func TestSeekableDescriptorStringIsSeparatorSafe(t *testing.T) {
	t.Parallel()

	d := SeekableDescriptor{
		Version:     FrameIndexVersion,
		FrameSize:   MaxFrameSize,
		FrameCount:  1 << 20,
		IndexLength: skippableHeaderSize + indexPayloadLen(1<<20),
	}
	if got := strings.Count(d.String(), "/"); got != descriptorFields-1 {
		t.Errorf("descriptor %q has %d separators, want %d", d.String(), got, descriptorFields-1)
	}
}
