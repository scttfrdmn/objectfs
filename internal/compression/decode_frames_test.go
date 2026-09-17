package compression

// [Compressor.DecodeFrames] and [Compressor.FrameDecoder] are the read half of framing as the S3
// backend sees it: the backend fetches one Range covering a run of frames and hands the bytes here.
//
// They were added with the backend's own tests as their only coverage, and a mutation run showed what
// that costs. Deleting the check that the fetched body is the length the index says produced a build
// the whole S3 suite still passed, because that length never disagrees on a path where the backend
// computed the Range from the same index. The check exists for the paths where something else computed
// it — a truncated response, a caller that fetched a wider range, a hostile index — and none of those
// are reachable from the backend's happy path. So they are tested here, against the function.

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

// framedThroughCompressor builds a framed object with the codec the S3 backend's writer uses and
// returns the Compressor that will read it, so the two halves under test are the two halves that ship
// together.
func framedThroughCompressor(t *testing.T, src []byte, frameSize int64) (*Compressor, []byte, *FrameIndex) {
	t.Helper()

	c, err := NewCompressor(Settings{
		Enabled:   true,
		Algorithm: string(comprpkg.AlgorithmZstd),
		Level:     comprpkg.DefaultLevel,
	})
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	obj, idx := framed(t, framedCodec(t), src, frameSize)

	return c, obj, idx
}

// TestDecodeFramesReturnsTheCoveredSpan is the happy path, asserted against the source rather than
// against a round trip through the same code.
//
// The returned buffer starts at the *first frame's* uncompressed offset, not at the offset the caller
// asked for. That is the contract the S3 backend's frame-relative slice depends on, and stating it here
// is what makes the backend's use of FramesCovering's second return value checkable rather than
// idiomatic.
func TestDecodeFramesReturnsTheCoveredSpan(t *testing.T) {
	t.Parallel()

	const (
		size      = 1 << 20
		frameSize = 64 << 10
	)

	src := compressibleBytes(size)
	c, obj, idx := framedThroughCompressor(t, src, frameSize)

	cases := []struct {
		name         string
		offset, size int64
	}{
		{"inside one frame", frameSize + 100, 200},
		{"across one boundary", frameSize - 50, 100},
		{"three whole frames", frameSize, 3 * frameSize},
		{"from the start", 0, 10},
		{"to the end", size - 10, 10},
		{"the whole object", 0, size},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			frames, offsetInFirst := idx.FramesCovering(tc.offset, tc.size)
			if len(frames) == 0 {
				t.Fatalf("FramesCovering(%d, %d) found no frames", tc.offset, tc.size)
			}

			last := frames[len(frames)-1]
			start := frames[0].CompressedOffset
			end := last.CompressedOffset + last.CompressedSize

			got, err := c.DecodeFrames("zstd", frames, obj[start:end])
			if err != nil {
				t.Fatalf("DecodeFrames: %v", err)
			}

			// The whole span of the frames, starting where the first frame starts.
			from := frames[0].UncompressedOffset
			to := last.UncompressedOffset + last.UncompressedSize

			if want := src[from:to]; !bytes.Equal(got, want) {
				t.Fatalf("DecodeFrames returned %d bytes, want the source's [%d:%d] (%d bytes)",
					len(got), from, to, len(want))
			}

			// And the second return value places the request inside it. A caller that ignored this and
			// sliced by tc.offset would be off by exactly the first frame's offset, which is the one
			// defect in this API that returns plausible bytes rather than an error.
			if want := tc.offset - from; offsetInFirst != want {
				t.Errorf("offsetInFirst = %d, want %d", offsetInFirst, want)
			}

			if want := src[tc.offset : tc.offset+tc.size]; !bytes.Equal(got[offsetInFirst:offsetInFirst+tc.size], want) {
				t.Errorf("slicing the decoded span at offsetInFirst=%d did not produce the requested "+
					"bytes", offsetInFirst)
			}
		})
	}
}

// TestDecodeFramesRejectsABodyThatIsNotTheLengthTheIndexSays is the check a mutation showed nothing
// else covers, and the reason it is worth having is that it runs *before* any frame is decoded.
//
// The frames' hashes would catch a wrong body eventually — a short body slices differently and fails
// verification. But the allocation does not wait for that: DecodeFrames sizes its output buffer from
// the index's own uncompressed totals, so an index claiming huge frames gets a huge allocation before a
// single hash has been consulted. Comparing the totals against the bytes actually in hand is what makes
// that allocation trustworthy, and it also turns three genuinely different mistakes into one clear
// error instead of a confusing checksum failure.
func TestDecodeFramesRejectsABodyThatIsNotTheLengthTheIndexSays(t *testing.T) {
	t.Parallel()

	const (
		size      = 256 << 10
		frameSize = 32 << 10
	)

	src := compressibleBytes(size)
	c, obj, idx := framedThroughCompressor(t, src, frameSize)

	frames, _ := idx.FramesCovering(frameSize, 2*frameSize)
	if len(frames) < 2 {
		t.Fatalf("need at least two covering frames, got %d", len(frames))
	}

	last := frames[len(frames)-1]
	start := frames[0].CompressedOffset
	end := last.CompressedOffset + last.CompressedSize
	exact := obj[start:end]

	cases := []struct {
		name string
		body []byte
		why  string
	}{
		{
			name: "truncated",
			body: exact[:len(exact)-1],
			why: "a short read, or a store that answered a Range with fewer bytes than it was asked " +
				"for. Without the length check the last frame's slice runs short and fails its hash, " +
				"which reports corruption for what is really a transport problem.",
		},
		{
			name: "one byte too long",
			body: append(bytes.Clone(exact), 0),
			why: "a caller that fetched a wider range than the frames need. Slack at the *front* would " +
				"shift every frame; slack at the back is silent, and the frames would all verify — so " +
				"this is the case that would pass and quietly establish that a sloppy Range is fine.",
		},
		{
			name: "the whole object",
			body: obj,
			why: "the most plausible caller error: passing the whole stored body with frame offsets " +
				"that are relative to the first frame of the run, not to the object.",
		},
		{
			name: "empty",
			body: nil,
			why:  "nothing arrived at all.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := c.DecodeFrames("zstd", frames, tc.body)
			if err == nil {
				t.Fatalf("DecodeFrames accepted a %d-byte body where the index says %d, returning %d "+
					"bytes. %s", len(tc.body), len(exact), len(got), tc.why)
			}

			// The specific error, not just an error. Every one of these cases would eventually fail
			// some check, so a test that accepted any error would pass with the length check deleted.
			if !errors.Is(err, ErrIndexCorrupt) {
				t.Errorf("error is %v, want one wrapping ErrIndexCorrupt — the length disagreement "+
					"should be reported as such rather than as whatever a mis-sliced frame decodes "+
					"to. %s", err, tc.why)
			}
		})
	}
}

// TestDecodeFramesRejectsNoFrames covers the empty-slice guard. It is not a hypothetical caller
// mistake: FramesCovering returns an empty slice for an offset at or past the end of the content, so
// any reader that does not check gets here.
func TestDecodeFramesRejectsNoFrames(t *testing.T) {
	t.Parallel()

	c, _, _ := framedThroughCompressor(t, compressibleBytes(64<<10), 8<<10)

	if _, err := c.DecodeFrames("zstd", nil, nil); err == nil {
		t.Error("DecodeFrames accepted an empty frame list; frames[0] is dereferenced immediately " +
			"after, so the alternative to this error is a panic")
	}
}

// TestFrameDecoderAnswersForTheObjectNotTheConfiguredCodec is the asymmetry the whole read path is
// built on, and the one this project has already got wrong once.
//
// Audit finding C2 was a read path keyed on the configured codec: switching `algorithm` made every
// existing compressed object unreadable, with the code to read them linked into the same binary. The
// frame decoder has the same shape and the same hazard — a mount writing gzip today must still be able
// to seek within the zstd objects it wrote last week.
func TestFrameDecoderAnswersForTheObjectNotTheConfiguredCodec(t *testing.T) {
	t.Parallel()

	cases := []struct {
		writeAlgorithm string
		enabled        bool
	}{
		{"zstd", true},
		{"gzip", true},
		{"lz4", true},
		{"none", false},
	}

	for _, tc := range cases {
		t.Run(tc.writeAlgorithm, func(t *testing.T) {
			t.Parallel()

			c, err := NewCompressor(Settings{
				Enabled:   tc.enabled,
				Algorithm: tc.writeAlgorithm,
				Level:     comprpkg.DefaultLevel,
			})
			if err != nil {
				t.Fatalf("NewCompressor(%s): %v", tc.writeAlgorithm, err)
			}

			// zstd frames are decodable whatever this mount writes.
			if c.FrameDecoder("zstd") == nil {
				t.Errorf("a mount configured to write %q cannot frame-decode zstd. Objects do not stop "+
					"being framed when a mount's algorithm changes, and every frame in the bucket "+
					"would become unreachable by range", tc.writeAlgorithm)
			}

			// And nothing else is, because nothing else has a skippable frame to hide an index in.
			for _, encoding := range []string{"gzip", "lz4", "br", "", "ZSTD", "zstd;q=1"} {
				if got := c.FrameDecoder(encoding); got != nil {
					t.Errorf("FrameDecoder(%q) returned a decoder; only zstd has frames, and claiming "+
						"otherwise sends a whole-object read down the frame path", encoding)
				}
			}
		})
	}
}

// TestDecodeFramesRefusesAnEncodingItCannotFrame pins the error rather than a nil dereference. The S3
// backend checks FrameDecoder before calling this, so reaching here means that check was removed or
// bypassed — which is precisely when a clear error matters.
func TestDecodeFramesRefusesAnEncodingItCannotFrame(t *testing.T) {
	t.Parallel()

	const frameSize = 8 << 10

	src := compressibleBytes(64 << 10)
	c, obj, idx := framedThroughCompressor(t, src, frameSize)

	frames, _ := idx.FramesCovering(0, frameSize)
	body := obj[frames[0].CompressedOffset : frames[0].CompressedOffset+frames[0].CompressedSize]

	for _, encoding := range []string{"gzip", "lz4", "br", ""} {
		_, err := c.DecodeFrames(encoding, frames, body)
		if err == nil {
			t.Errorf("DecodeFrames(%q) decoded zstd frames under an encoding that has no frame "+
				"decoder", encoding)

			continue
		}

		if !strings.Contains(err.Error(), encoding) && encoding != "" {
			t.Errorf("DecodeFrames(%q) failed with %v, which does not name the encoding — the two "+
				"likely causes, a foreign tool's encoding and a mangled one, are told apart by "+
				"exactly that", encoding, err)
		}
	}
}

// TestDecodeFramesReportsADamagedFrameRatherThanReturningIt is the integrity guarantee the S3 read
// path relies on in place of the whole-content hash, which a partial read cannot check.
func TestDecodeFramesReportsADamagedFrameRatherThanReturningIt(t *testing.T) {
	t.Parallel()

	const (
		size      = 256 << 10
		frameSize = 32 << 10
	)

	src := compressibleBytes(size)
	c, obj, idx := framedThroughCompressor(t, src, frameSize)

	// Two frames, and the damage is in the second one — so the first decodes cleanly and the failure
	// cannot be mistaken for "the run was rejected before anything was tried".
	frames, _ := idx.FramesCovering(0, 2*frameSize)
	if len(frames) < 2 {
		t.Fatalf("need at least two covering frames, got %d", len(frames))
	}

	last := frames[len(frames)-1]
	start := frames[0].CompressedOffset
	body := bytes.Clone(obj[start : last.CompressedOffset+last.CompressedSize])

	// Inside the second frame's compressed bytes, past its header.
	body[last.CompressedOffset-start+4] ^= 0xFF

	got, err := c.DecodeFrames("zstd", frames, body)
	if err == nil {
		t.Fatalf("DecodeFrames returned %d bytes for a run whose second frame does not match the hash "+
			"the index recorded. A partial read cannot check the whole-content hash, so these per-frame "+
			"hashes are the only thing standing between damaged stored bytes and a caller", len(got))
	}
}
