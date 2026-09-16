package compression

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// framedCodec returns a codec at the library default level.
func framedCodec(t *testing.T) *ZstdCodec {
	t.Helper()
	c, err := NewZstdCodec(0)
	if err != nil {
		t.Fatalf("NewZstdCodec: %v", err)
	}
	return c
}

// compressibleBytes builds n bytes that actually compress, with enough local structure that frame
// boundaries are not free. Random bytes would make every frame incompressible and would hide any
// bug that only shows up when a frame's compressed size differs from its uncompressed size.
//
// Runs of a repeated byte, with both the run length and the byte drawn from a cheap LCG: long runs
// compress well, and the sequence does not repeat. The non-repetition is load-bearing. A periodic
// generator — this was `byte('a' + (i/64+i/4096)%26)` — makes frames 1 and 2 at a frame size of
// 8192 byte-for-byte identical, so the test that swaps one frame's bytes for another's was
// asserting that identical bytes fail a checksum, and it failed for the right reason.
func compressibleBytes(n int) []byte {
	out := make([]byte, n)
	seed := uint32(1)
	for i := 0; i < n; {
		seed = seed*1664525 + 1013904223
		run := int(seed>>16)%64 + 1
		b := byte(seed >> 8)
		for j := 0; j < run && i < n; j++ {
			out[i] = b
			i++
		}
	}
	return out
}

// framed compresses src and returns the stored object plus its index.
func framed(t *testing.T, c *ZstdCodec, src []byte, frameSize int64) ([]byte, *FrameIndex) {
	t.Helper()
	obj, idx, err := c.CompressFramed(src, frameSize, sha256.Sum256(src))
	if err != nil {
		t.Fatalf("CompressFramed(%d bytes, frame %d): %v", len(src), frameSize, err)
	}
	return obj, idx
}

// readRangeThroughFrames is the reader this format exists to enable: find the covering frames,
// fetch exactly their compressed extents, verify and decode each, and slice the requested span out
// of the result. It deliberately re-parses the index from the object rather than using the index
// CompressFramed returned, so the tests exercise the encode/parse pair rather than one side twice.
func readRangeThroughFrames(t *testing.T, c *ZstdCodec, obj []byte, offset, size int64) ([]byte, int64) {
	t.Helper()

	idx, _, err := ParseFrameIndex(obj)
	if err != nil {
		t.Fatalf("ParseFrameIndex: %v", err)
	}

	frames, offsetInFirst := idx.FramesCovering(offset, size)
	if len(frames) == 0 {
		return nil, 0
	}

	var fetched int64
	var buf []byte
	for _, f := range frames {
		// Exactly the bytes a ranged GET of [CompressedOffset, CompressedSize) returns.
		extent := obj[f.CompressedOffset : f.CompressedOffset+f.CompressedSize]
		fetched += int64(len(extent))

		out, err := c.DecompressFrame(extent, f)
		if err != nil {
			t.Fatalf("DecompressFrame at %d: %v", f.CompressedOffset, err)
		}
		buf = append(buf, out...)
	}

	// Slice frame-relative, never object-relative. This is the arithmetic FramesCovering's second
	// return value exists to get right.
	end := min(offsetInFirst+size, int64(len(buf)))
	return buf[offsetInFirst:end], fetched
}

func TestCompressFramedRoundTripsEveryFrame(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)

	for _, tc := range []struct {
		name      string
		size      int
		frameSize int64
	}{
		{"single partial frame", 1000, 4096},
		{"exactly one frame", 4096, 4096},
		{"exact multiple", 8192, 4096},
		{"partial final frame", 10000, 4096},
		{"one byte", 1, 4096},
		{"one byte per frame", 5, 1},
		{"frame larger than object", 100, 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := compressibleBytes(tc.size)
			obj, idx := framed(t, c, src, tc.frameSize)

			wantFrames := (tc.size + int(tc.frameSize) - 1) / int(tc.frameSize)
			if len(idx.Frames) != wantFrames {
				t.Errorf("frame count = %d, want %d", len(idx.Frames), wantFrames)
			}
			if idx.UncompressedSize != int64(tc.size) {
				t.Errorf("UncompressedSize = %d, want %d", idx.UncompressedSize, tc.size)
			}
			if idx.ContentSHA256 != sha256.Sum256(src) {
				t.Error("ContentSHA256 does not match the source content")
			}

			// Every frame decodes in isolation, and the concatenation is the original.
			var whole []byte
			for i, f := range idx.Frames {
				extent := obj[f.CompressedOffset : f.CompressedOffset+f.CompressedSize]
				out, err := c.DecompressFrame(extent, f)
				if err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
				whole = append(whole, out...)
			}
			if !bytes.Equal(whole, src) {
				t.Errorf("reassembled %d bytes, want %d, equal=%v", len(whole), len(src), bytes.Equal(whole, src))
			}
		})
	}
}

// TestFramedObjectIsAValidWholeZstdStream is the compatibility guarantee: framing must not cost the
// ability to decode the object in one shot, because that is the fallback path for any reader
// without frame support and for an endpoint that ignores Range.
func TestFramedObjectIsAValidWholeZstdStream(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)
	src := compressibleBytes(300000)
	obj, _ := framed(t, c, src, 64<<10)

	got, err := c.Decompress(obj)
	if err != nil {
		t.Fatalf("Decompress over the framed object: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("whole-stream decode returned %d bytes, want %d", len(got), len(src))
	}
}

// TestZstdCLIReadsAFramedObject checks the same property against the reference implementation
// rather than only against klauspost, because "a concatenation of frames behind a skippable frame
// is a legal zstd stream" is a claim about the format, not about one library.
func TestZstdCLIReadsAFramedObject(t *testing.T) {
	t.Parallel()

	zstdBin, err := exec.LookPath("zstd")
	if err != nil {
		t.Skip("zstd(1) not on PATH")
	}

	c := framedCodec(t)
	src := compressibleBytes(300000)
	obj, idx := framed(t, c, src, 64<<10)

	path := filepath.Join(t.TempDir(), "obj.zst")
	if err := os.WriteFile(path, obj, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// zstdBin comes from exec.LookPath and path from t.TempDir; neither is attacker-influenced.
	out, err := exec.CommandContext(t.Context(), zstdBin, "-d", "-c", path).Output() //nolint:gosec // G204: fixed args, LookPath result
	if err != nil {
		t.Fatalf("zstd -d: %v", err)
	}
	if !bytes.Equal(out, src) {
		t.Errorf("zstd -d returned %d bytes, want %d", len(out), len(src))
	}

	// `zstd -l` must see the index as a skipped frame, not as data.
	listing, err := exec.CommandContext(t.Context(), zstdBin, "-l", "-v", path).CombinedOutput() //nolint:gosec // G204: as above
	if err != nil {
		t.Fatalf("zstd -l: %v\n%s", err, listing)
	}
	// `zstd -l -v` prints "# Skippable Frames: 1". Asserted against the CLI's actual output rather
	// than a predicted string: the first version of this line looked for "Skips", which is not what
	// v1.5.7 emits in verbose mode, so it failed against a correct object.
	if !bytes.Contains(listing, []byte("Skippable Frames: 1")) {
		t.Errorf("zstd -l did not report exactly one skippable frame:\n%s", listing)
	}
	if !bytes.Contains(listing, []byte(fmt.Sprintf("Zstandard Frames: %d", len(idx.Frames)))) {
		t.Errorf("zstd -l did not report %d data frames:\n%s", len(idx.Frames), listing)
	}
	t.Logf("frames=%d, zstd -l says:\n%s", len(idx.Frames), listing)
}

// TestPlainCompressIsNotFramed is the no-migration guarantee. Every object written before #185 is a
// single zstd frame with no index, and it must be reported as unframed — not as corrupt — so the
// caller degrades to the whole-object path it already has.
func TestPlainCompressIsNotFramed(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)

	plain, err := c.Compress(compressibleBytes(10000))
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if _, _, err := ParseFrameIndex(plain); !errors.Is(err, ErrNotFramed) {
		t.Errorf("ParseFrameIndex over a plain object: got %v, want ErrNotFramed", err)
	}
	if _, err := IndexFrameLength(plain); !errors.Is(err, ErrNotFramed) {
		t.Errorf("IndexFrameLength over a plain object: got %v, want ErrNotFramed", err)
	}
}

// TestIndexFrameLengthNeedsOnlyEightBytes is what makes a cold read one request rather than two: a
// reader over-fetches a guessed prefix and learns the true index length from the first 8 bytes.
func TestIndexFrameLengthNeedsOnlyEightBytes(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)
	obj, idx := framed(t, c, compressibleBytes(500000), 64<<10)

	wantLen := skippableHeaderSize + int(indexPayloadLen(int64(len(idx.Frames))))
	got, err := IndexFrameLength(obj[:skippableHeaderSize])
	if err != nil {
		t.Fatalf("IndexFrameLength over 8 bytes: %v", err)
	}
	if got != wantLen {
		t.Errorf("IndexFrameLength = %d, want %d", got, wantLen)
	}

	// Below 8 bytes it must say how many it needs, not guess.
	var short *ShortIndexError
	if _, err := IndexFrameLength(obj[:7]); !errors.As(err, &short) {
		t.Fatalf("7-byte prefix: got %v, want *ShortIndexError", err)
	} else if short.Need != skippableHeaderSize || short.Have != 7 {
		t.Errorf("ShortIndexError = %+v, want Have=7 Need=%d", short, skippableHeaderSize)
	}

	// A prefix long enough for the header but not the payload reports the full requirement, so the
	// second fetch is the last one.
	if _, _, err := ParseFrameIndex(obj[:wantLen-1]); !errors.As(err, &short) {
		t.Fatalf("truncated index: got %v, want *ShortIndexError", err)
	} else if short.Need != wantLen {
		t.Errorf("ShortIndexError.Need = %d, want %d", short.Need, wantLen)
	}

	// And exactly the index frame, with no data behind it, parses.
	if _, n, err := ParseFrameIndex(obj[:wantLen]); err != nil {
		t.Errorf("ParseFrameIndex over exactly the index frame: %v", err)
	} else if n != wantLen {
		t.Errorf("index frame length = %d, want %d", n, wantLen)
	}
}

// TestDecompressFrameFailsClosedOnACorruptFrame is the correctness capability from CLAUDE.md: a
// frame whose bytes do not match the index is an error, never a quiet fall back.
func TestDecompressFrameFailsClosedOnACorruptFrame(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)
	src := compressibleBytes(40000)
	obj, idx := framed(t, c, src, 8192)
	f := idx.Frames[1]

	extent := func() []byte {
		return bytes.Clone(obj[f.CompressedOffset : f.CompressedOffset+f.CompressedSize])
	}

	t.Run("flipped byte in the payload", func(t *testing.T) {
		t.Parallel()
		bad := extent()
		bad[len(bad)/2] ^= 0xFF
		_, err := c.DecompressFrame(bad, f)
		if !errors.Is(err, ErrFrameChecksumMismatch) {
			t.Errorf("got %v, want ErrFrameChecksumMismatch", err)
		}
	})

	t.Run("flipped byte in the zstd magic", func(t *testing.T) {
		t.Parallel()
		// Caught by the hash before the decoder is asked for an opinion, which is the ordering that
		// keeps hostile input away from the decompressor.
		bad := extent()
		bad[0] ^= 0xFF
		_, err := c.DecompressFrame(bad, f)
		if !errors.Is(err, ErrFrameChecksumMismatch) {
			t.Errorf("got %v, want ErrFrameChecksumMismatch", err)
		}
	})

	t.Run("truncated extent", func(t *testing.T) {
		t.Parallel()
		_, err := c.DecompressFrame(extent()[:f.CompressedSize-1], f)
		if !errors.Is(err, ErrFrameChecksumMismatch) {
			t.Errorf("got %v, want ErrFrameChecksumMismatch", err)
		}
	})

	t.Run("bytes from a different frame", func(t *testing.T) {
		t.Parallel()
		// What an overwrite between the index fetch and the frame fetch looks like: every request
		// succeeded and the lengths can even add up, but the bytes are from another generation.
		other := idx.Frames[2]
		if other.CompressedSize != f.CompressedSize {
			t.Skipf("frames 1 and 2 differ in size (%d vs %d); this case needs equal sizes",
				f.CompressedSize, other.CompressedSize)
		}
		swapped := bytes.Clone(obj[other.CompressedOffset : other.CompressedOffset+other.CompressedSize])
		if _, err := c.DecompressFrame(swapped, f); !errors.Is(err, ErrFrameChecksumMismatch) {
			t.Errorf("got %v, want ErrFrameChecksumMismatch", err)
		}
	})
}

// TestDecompressFrameRejectsASlackSlice guards the trap the DecompressFrame doc comment describes:
// DecodeAll over a concatenation decodes the *following* frames too. A caller that passed a slice
// running past the frame would get a neighbour's content appended, and every downstream length
// check would be computed from that same wrong buffer.
func TestDecompressFrameRejectsASlackSlice(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)
	obj, idx := framed(t, c, compressibleBytes(40000), 8192)
	f := idx.Frames[0]

	slack := obj[f.CompressedOffset:] // frame 0 plus every frame after it
	if _, err := c.DecompressFrame(slack, f); !errors.Is(err, ErrFrameChecksumMismatch) {
		t.Errorf("got %v, want ErrFrameChecksumMismatch", err)
	}

	// Prove the trap is real, so this test is known to be guarding something: decoding the slack
	// slice directly returns more than the frame's content.
	loose, err := c.Decompress(slack)
	if err != nil {
		t.Fatalf("Decompress over the slack slice: %v", err)
	}
	if int64(len(loose)) <= f.UncompressedSize {
		t.Fatalf("slack slice decoded to %d bytes, not more than one frame's %d — the trap this "+
			"test guards would not fire, so the test proves nothing", len(loose), f.UncompressedSize)
	}
}

// TestAppendFrameIndexRejectsAnUnencodableIndex covers the checks that stand between an in-memory
// int64 and the format's fixed-width fields. AppendFrameIndex is exported and takes a caller-built
// FrameIndex, so a value that wrapped on the way onto the wire would produce an object whose index
// disagrees with its own bytes — reported as corruption arbitrarily later, on another machine.
func TestAppendFrameIndexRejectsAnUnencodableIndex(t *testing.T) {
	t.Parallel()

	good := func() *FrameIndex {
		return &FrameIndex{
			Version:          FrameIndexVersion,
			FrameSize:        4096,
			UncompressedSize: 4096,
			Frames: []Frame{{
				CompressedOffset:   112,
				CompressedSize:     100,
				UncompressedOffset: 0,
				UncompressedSize:   4096,
			}},
		}
	}

	// The baseline must encode, or every case below would pass for the wrong reason.
	if _, err := AppendFrameIndex(nil, good()); err != nil {
		t.Fatalf("the baseline index does not encode, so this table proves nothing: %v", err)
	}

	for _, tc := range []struct {
		name   string
		break_ func(idx *FrameIndex)
	}{
		{"zero frame size", func(idx *FrameIndex) { idx.FrameSize = 0 }},
		{"negative frame size", func(idx *FrameIndex) { idx.FrameSize = -4096 }},
		{"negative content size", func(idx *FrameIndex) { idx.UncompressedSize = -1 }},
		{"zero compressed extent", func(idx *FrameIndex) { idx.Frames[0].CompressedSize = 0 }},
		{"negative compressed extent", func(idx *FrameIndex) { idx.Frames[0].CompressedSize = -1 }},
		{"compressed extent above uint32", func(idx *FrameIndex) { idx.Frames[0].CompressedSize = 1 << 32 }},
		{"uncompressed extent above uint32", func(idx *FrameIndex) { idx.Frames[0].UncompressedSize = 1 << 32 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx := good()
			tc.break_(idx)
			if _, err := AppendFrameIndex(nil, idx); err == nil {
				t.Error("encoded an index that cannot be represented on the wire")
			}
		})
	}
}

// TestAppendFrameIndexRefusesAnIndexOverTheCeiling exercises the guard that keeps a truncated
// SkippableSize from ever being written. It lowers maxIndexPayload rather than building an index big
// enough to trip the real one: see the comment on that variable. Not parallel, because it mutates
// package state — top-level parallel tests are all still paused while this one runs.
//
//nolint:paralleltest // mutates maxIndexPayload; see above
func TestAppendFrameIndexRefusesAnIndexOverTheCeiling(t *testing.T) {
	restore := maxIndexPayload
	t.Cleanup(func() { maxIndexPayload = restore })

	idx := &FrameIndex{
		Version:          FrameIndexVersion,
		FrameSize:        1,
		UncompressedSize: 4,
		Frames: []Frame{
			{CompressedOffset: 200, CompressedSize: 20, UncompressedOffset: 0, UncompressedSize: 1},
			{CompressedOffset: 220, CompressedSize: 20, UncompressedOffset: 1, UncompressedSize: 1},
			{CompressedOffset: 240, CompressedSize: 20, UncompressedOffset: 2, UncompressedSize: 1},
			{CompressedOffset: 260, CompressedSize: 20, UncompressedOffset: 3, UncompressedSize: 1},
		},
	}

	// One byte of headroom above what four records need: it must still encode.
	maxIndexPayload = indexPayloadLen(4)
	if _, err := AppendFrameIndex(nil, idx); err != nil {
		t.Fatalf("refused an index that fits exactly: %v", err)
	}

	// One byte short of it: it must refuse rather than write a truncated length.
	maxIndexPayload = indexPayloadLen(4) - 1
	if _, err := AppendFrameIndex(nil, idx); err == nil {
		t.Fatal("encoded an index over the ceiling, which would truncate SkippableSize")
	}

	// And CompressFramed must inherit the same refusal rather than carrying its own looser rule.
	maxIndexPayload = fixedHeaderSize + sha256.Size
	if _, _, err := framedOrErr(framedCodec(t), compressibleBytes(4096), 1); err == nil {
		t.Error("CompressFramed produced an object whose index is over the ceiling")
	}
}

// framedOrErr is CompressFramed without the t.Fatalf, for the cases that want the error.
func framedOrErr(c *ZstdCodec, src []byte, frameSize int64) ([]byte, *FrameIndex, error) {
	return c.CompressFramed(src, frameSize, sha256.Sum256(src))
}

func TestParseFrameIndexRejectsCorruption(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)
	src := compressibleBytes(40000)

	// payloadAt returns the index payload's offset within the object, for tests that damage a
	// specific header field.
	const payloadAt = skippableHeaderSize

	for _, tc := range []struct {
		name    string
		damage  func(obj []byte, idx *FrameIndex)
		wantErr error
	}{
		{
			name: "self-hash mismatch",
			damage: func(obj []byte, idx *FrameIndex) {
				// The last byte of the index *payload*, not of the object — the index leads, so
				// obj[len(obj)-1] is the tail of the final data frame and the index still verifies.
				obj[payloadAt+int(indexPayloadLen(int64(len(idx.Frames))))-1] ^= 0xFF
			},
			wantErr: ErrIndexCorrupt,
		},
		{
			name: "flipped bit in a frame checksum",
			damage: func(obj []byte, _ *FrameIndex) {
				obj[payloadAt+fixedHeaderSize+8] ^= 0xFF
			},
			wantErr: ErrIndexCorrupt, // caught by the self-hash, before any offset is trusted
		},
		{
			name:    "payload magic",
			damage:  func(obj []byte, _ *FrameIndex) { obj[payloadAt] = 'X' },
			wantErr: ErrNotFramed,
		},
		{
			name:    "unknown version",
			damage:  func(obj []byte, _ *FrameIndex) { obj[payloadAt+4] = FrameIndexVersion + 1 },
			wantErr: ErrNotFramed, // a future format degrades to whole-object, it is not corruption
		},
		{
			name:    "unknown flag bit",
			damage:  func(obj []byte, _ *FrameIndex) { obj[payloadAt+5] |= 0x80 },
			wantErr: ErrIndexCorrupt,
		},
		{
			name:    "reserved byte 6",
			damage:  func(obj []byte, _ *FrameIndex) { obj[payloadAt+6] = 1 },
			wantErr: ErrIndexCorrupt,
		},
		{
			name:    "reserved bytes 28:32",
			damage:  func(obj []byte, _ *FrameIndex) { obj[payloadAt+28] = 1 },
			wantErr: ErrIndexCorrupt,
		},
		{
			name: "frame count disagrees with the record bytes",
			damage: func(obj []byte, idx *FrameIndex) {
				binary.LittleEndian.PutUint32(obj[payloadAt+24:payloadAt+28], uint32(len(idx.Frames)+1))
			},
			wantErr: ErrIndexCorrupt,
		},
		{
			name: "frame size zero",
			damage: func(obj []byte, _ *FrameIndex) {
				binary.LittleEndian.PutUint64(obj[payloadAt+8:payloadAt+16], 0)
			},
			wantErr: ErrIndexCorrupt,
		},
		{
			name: "frame size that does not tile the content",
			damage: func(obj []byte, _ *FrameIndex) {
				binary.LittleEndian.PutUint64(obj[payloadAt+8:payloadAt+16], 8191)
			},
			wantErr: ErrIndexCorrupt,
		},
		{
			name: "frame size above MaxInt64",
			damage: func(obj []byte, _ *FrameIndex) {
				// The wire field is a uint64 and the in-memory field is an int64, so without the
				// bound this arrives as a negative FrameSize and FramesCovering divides by it.
				binary.LittleEndian.PutUint64(obj[payloadAt+8:payloadAt+16], 1<<63)
			},
			wantErr: ErrIndexCorrupt,
		},
		{
			name: "total size above MaxInt64",
			damage: func(obj []byte, _ *FrameIndex) {
				binary.LittleEndian.PutUint64(obj[payloadAt+16:payloadAt+24], math.MaxUint64)
			},
			wantErr: ErrIndexCorrupt,
		},
		{
			name: "total size disagrees with the frames",
			damage: func(obj []byte, _ *FrameIndex) {
				binary.LittleEndian.PutUint64(obj[payloadAt+16:payloadAt+24], 99999)
			},
			wantErr: ErrIndexCorrupt,
		},
		{
			name: "zero-extent frame",
			damage: func(obj []byte, _ *FrameIndex) {
				binary.LittleEndian.PutUint32(obj[payloadAt+fixedHeaderSize:], 0)
			},
			wantErr: ErrIndexCorrupt,
		},
		{
			name: "skippable id belonging to another tool",
			damage: func(obj []byte, _ *FrameIndex) {
				obj[0] = 0x50 | 0xE // the Zstandard Seekable Format's id
			},
			wantErr: ErrNotFramed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obj, idx := framed(t, c, src, 8192)

			// Damaging a field the self-hash covers must be re-hashed for the test to reach the
			// check it is aiming at; otherwise every case below would stop at the self-hash and
			// this table would assert one thing thirteen times.
			tc.damage(obj, idx)
			if tc.name != "self-hash mismatch" && tc.name != "flipped bit in a frame checksum" {
				resealIndex(t, obj)
			}

			_, _, err := ParseFrameIndex(obj)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// resealIndex recomputes the index's self-hash after a test has altered a covered field. Without
// it, a test aiming at the tiling check would be satisfied by the self-hash check instead — the
// assertion would pass while the code it meant to exercise never ran.
func resealIndex(t *testing.T, obj []byte) {
	t.Helper()

	frameLen, err := IndexFrameLength(obj)
	if err != nil {
		// Cases that damage the skippable header itself have no index to reseal.
		return
	}
	payload := obj[skippableHeaderSize:frameLen]
	body := payload[:len(payload)-sha256.Size]
	sum := sha256.Sum256(body)
	copy(payload[len(payload)-sha256.Size:], sum[:])
}

func TestFramesCovering(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)

	// 10 bytes at a frame size of 4: frames span [0,4), [4,8), [8,10).
	_, idx := framed(t, c, compressibleBytes(10), 4)
	if len(idx.Frames) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(idx.Frames))
	}

	for _, tc := range []struct {
		name         string
		offset, size int64
		wantFirst    int64 // UncompressedOffset of the first covering frame
		wantCount    int
		wantInFirst  int64
	}{
		{"whole object", 0, 10, 0, 3, 0},
		{"inside one frame", 1, 2, 0, 1, 1},
		{"exactly one frame", 4, 4, 4, 1, 0},
		{"straddling two", 3, 2, 0, 2, 3},
		{"ending on a boundary", 0, 4, 0, 1, 0},
		{"starting on a boundary", 8, 2, 8, 1, 0},
		{"last byte", 9, 1, 8, 1, 1},
		{"size past the end is clamped", 9, 100, 8, 1, 1},
		{"spanning all three", 2, 7, 0, 3, 2},
		{"zero length", 4, 0, 0, 0, 0},
		{"negative length", 4, -1, 0, 0, 0},
		{"offset at the end", 10, 1, 0, 0, 0},
		{"offset past the end", 11, 1, 0, 0, 0},
		{"negative offset", -1, 4, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			frames, inFirst := idx.FramesCovering(tc.offset, tc.size)
			if len(frames) != tc.wantCount {
				t.Fatalf("covered %d frames, want %d", len(frames), tc.wantCount)
			}
			if tc.wantCount == 0 {
				return
			}
			if frames[0].UncompressedOffset != tc.wantFirst {
				t.Errorf("first frame at %d, want %d", frames[0].UncompressedOffset, tc.wantFirst)
			}
			if inFirst != tc.wantInFirst {
				t.Errorf("offsetInFirst = %d, want %d", inFirst, tc.wantInFirst)
			}
		})
	}
}

// TestFramedRangeReadMatchesTheSource walks every (offset, size) over a small object, which is
// cheap enough to be exhaustive and so covers the boundary cases a hand-written table would pick
// by guessing.
func TestFramedRangeReadMatchesTheSource(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)

	const size = 200
	src := compressibleBytes(size)
	for _, frameSize := range []int64{1, 7, 16, 64, 199, 200, 201, 4096} {
		t.Run(fmt.Sprintf("frame=%d", frameSize), func(t *testing.T) {
			t.Parallel()
			obj, _ := framed(t, c, src, frameSize)

			for offset := int64(0); offset <= size; offset++ {
				for n := int64(0); offset+n <= size; n++ {
					got, _ := readRangeThroughFrames(t, c, obj, offset, n)
					want := src[offset : offset+n]
					if n == 0 || offset == size {
						want = nil
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("range [%d,+%d): got %q, want %q", offset, n, got, want)
					}
				}
			}
		})
	}
}

// TestFramedReadTransfersOnlyTheCoveringFrames asserts bytes transferred rather than latency, which
// is #185's explicit requirement: a latency assertion passes on a fast link even when the
// implementation fetched the whole object, so it would not detect the defect being fixed.
func TestFramedReadTransfersOnlyTheCoveringFrames(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)

	const (
		objectSize = 8 << 20
		frameSize  = 256 << 10
		readSize   = 128 << 10 // the kernel's default MaxRead
	)
	src := compressibleBytes(objectSize)
	obj, idx := framed(t, c, src, frameSize)

	// A read wholly inside one frame fetches one frame; the worst case straddles two.
	for _, tc := range []struct {
		name      string
		offset    int64
		maxFrames int
	}{
		{"aligned read touches one frame", frameSize * 4, 1},
		{"straddling read touches two", frameSize*4 - readSize/2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, fetched := readRangeThroughFrames(t, c, obj, tc.offset, readSize)
			if !bytes.Equal(got, src[tc.offset:tc.offset+readSize]) {
				t.Fatalf("content mismatch at offset %d", tc.offset)
			}

			// The bound that matters: compressed bytes for at most maxFrames frames, versus the
			// whole stored object, which is what today's code transfers for this read. The budget
			// is maxFrames times the largest frame in the object, not the frames actually read —
			// taking it from the frames read would make the assertion true by construction.
			var largest int64
			for _, f := range idx.Frames {
				if f.CompressedSize > largest {
					largest = f.CompressedSize
				}
			}
			budget := largest * int64(tc.maxFrames)
			if fetched > budget {
				t.Errorf("fetched %d bytes, budget for %d frames is %d", fetched, tc.maxFrames, budget)
			}
			if fetched >= int64(len(obj)) {
				t.Errorf("fetched %d bytes of a %d-byte object — no better than a whole-object read",
					fetched, len(obj))
			}
			t.Logf("read %d bytes at offset %d: fetched %d compressed bytes of a %d-byte stored "+
				"object (%.0fx less)", readSize, tc.offset, fetched, len(obj),
				float64(len(obj))/float64(fetched))
		})
	}
}

func TestCompressFramedRejectsBadInput(t *testing.T) {
	t.Parallel()
	c := framedCodec(t)

	if _, _, err := c.CompressFramed(nil, 4096, [sha256.Size]byte{}); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("empty src: got %v, want errors.ErrUnsupported", err)
	}
	if _, _, err := c.CompressFramed([]byte("x"), 0, [sha256.Size]byte{}); err == nil {
		t.Error("frame size 0: got nil, want an error")
	}
	if _, _, err := c.CompressFramed([]byte("x"), math.MaxUint32+1, [sha256.Size]byte{}); err == nil {
		t.Error("frame size above MaxUint32: got nil, want an error")
	}
}

func TestDeriveFrameSize(t *testing.T) {
	t.Parallel()

	const (
		gib = 1 << 30
		mib = 1 << 20
		kib = 1 << 10
	)

	for _, tc := range []struct {
		name  string
		size  int64
		ratio float64
		want  int64
	}{
		// The closed form is F = sqrt(40*S*r), rounded to a power of two and clamped. At r=3.67
		// (the measured ratio on the FASTQ corpus in #185) the unrounded optimum is 388 KiB for
		// 1 GiB and 1.20 MiB for 10 GiB, which round to 512 KiB and 1 MiB.
		{"1 GiB", gib, 3.67, 512 * kib},
		{"10 GiB", 10 * gib, 3.67, mib},
		{"100 GiB", 100 * gib, 3.67, 4 * mib},
		{"1 TiB", 1024 * gib, 3.67, 16 * mib},

		// Clamping at both ends.
		{"tiny object clamps to the floor", 1024, 3.67, MinFrameSize},
		{"64 MiB clamps to the floor", 64 * mib, 3.67, MinFrameSize},
		{"2 TiB clamps to the ceiling", 2048 * gib, 3.67, MaxFrameSize},
		{"absurd ratio clamps to the ceiling", gib, 1e12, MaxFrameSize},

		// Degenerate inputs must not produce a zero or a panic: a zero frame size would divide by
		// zero in FramesCovering.
		{"zero size", 0, 3.67, MinFrameSize},
		{"zero ratio", gib, 0, MinFrameSize},
		{"negative ratio", gib, -1, MinFrameSize},
		{"NaN ratio", gib, math.NaN(), MinFrameSize},
		{"Inf ratio", gib, math.Inf(1), MinFrameSize},
		{"max size", math.MaxInt64, 3.67, MaxFrameSize},

		// A negative size is only reachable because the parameter is signed, which it is so that
		// callers holding an int64 file size do not have to convert. It must clamp like any other
		// unusable input rather than reaching the sqrt.
		{"negative size", -1, 3.67, MinFrameSize},
		{"most negative size", math.MinInt64, 3.67, MinFrameSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DeriveFrameSize(tc.size, tc.ratio)
			if got != tc.want {
				t.Errorf("DeriveFrameSize(%d, %v) = %d, want %d", tc.size, tc.ratio, got, tc.want)
			}
			if got == 0 {
				t.Fatal("frame size 0 would divide by zero in FramesCovering")
			}
			if got&(got-1) != 0 {
				t.Errorf("DeriveFrameSize returned %d, which is not a power of two", got)
			}
			if got < MinFrameSize || got > MaxFrameSize {
				t.Errorf("DeriveFrameSize returned %d, outside [%d, %d]", got, MinFrameSize, MaxFrameSize)
			}
		})
	}
}

// FuzzFramedRangeRead is the round-trip fuzzer #185 asks for over (offset, size), extended to fuzz
// the frame size and the content too. The invariant is the only one that matters: a framed ranged
// read returns exactly the same bytes as slicing the original.
func FuzzFramedRangeRead(f *testing.F) {
	f.Add([]byte("hello, frames"), uint32(4), int64(2), int64(5))
	f.Add(compressibleBytes(5000), uint32(1024), int64(0), int64(5000))
	f.Add(compressibleBytes(5000), uint32(1024), int64(4999), int64(1))
	f.Add(compressibleBytes(100), uint32(1), int64(50), int64(0))
	f.Add(compressibleBytes(4096), uint32(4096), int64(4095), int64(2))

	c, err := NewZstdCodec(0)
	if err != nil {
		f.Fatalf("NewZstdCodec: %v", err)
	}

	f.Fuzz(func(t *testing.T, src []byte, frameSize uint32, offset, size int64) {
		// Normalize the inputs into the range worth exercising rather than skipping outside it.
		//
		// Skipping looks equivalent and is not. frameSize arrives as an arbitrary uint32, so a
		// `frameSize > 1<<17 { t.Skip() }` bound discards all but ~0.003% of mutations — and Go does
		// not count a skipped input as an exec, so the engine reported 17847 execs in the first few
		// seconds and then 0/sec for the rest of the run while it mutated its way through inputs
		// that never reached CompressFramed. Clamping keeps every mutation useful. (Nothing here is
		// slow: the worst shape these bounds allow, 64 KiB at frameSize 16, is 4096 frames and
		// measures 9.7ms to encode.)
		if len(src) == 0 {
			t.Skip("no content to frame")
		}
		if len(src) > 1<<16 {
			src = src[:1<<16]
		}
		// Frame size in [1, 1<<17], and never so small relative to src that the frame count
		// explodes. offset and size stay arbitrary on purpose — out-of-range values are the cases
		// FramesCovering has to get right.
		frameSize = frameSize%(1<<17) + 1
		if minFrame := uint32(len(src)/4096) + 1; frameSize < minFrame {
			frameSize = minFrame
		}

		obj, idx, err := c.CompressFramed(src, int64(frameSize), sha256.Sum256(src))
		if err != nil {
			t.Fatalf("CompressFramed(%d bytes, frame %d): %v", len(src), frameSize, err)
		}

		// The stored object must always be a valid whole zstd stream, whatever the framing.
		whole, err := c.Decompress(obj)
		if err != nil {
			t.Fatalf("whole-stream decode: %v", err)
		}
		if !bytes.Equal(whole, src) {
			t.Fatal("whole-stream decode does not match the source")
		}

		// And the index must survive a parse, with the tiling invariant intact.
		parsed, frameLen, err := ParseFrameIndex(obj)
		if err != nil {
			t.Fatalf("ParseFrameIndex: %v", err)
		}
		if parsed.UncompressedSize != int64(len(src)) || len(parsed.Frames) != len(idx.Frames) {
			t.Fatalf("parsed index disagrees with the written one: %d/%d bytes, %d/%d frames",
				parsed.UncompressedSize, len(src), len(parsed.Frames), len(idx.Frames))
		}
		if parsed.ContentSHA256 != sha256.Sum256(src) {
			t.Fatal("parsed ContentSHA256 does not match the source")
		}
		if parsed.Frames[0].CompressedOffset != int64(frameLen) {
			t.Fatalf("first frame at %d, index frame ends at %d",
				parsed.Frames[0].CompressedOffset, frameLen)
		}

		got, _ := readRangeThroughFrames(t, c, obj, offset, size)

		// The expectation, computed independently of the code under test.
		var want []byte
		if size > 0 && offset >= 0 && offset < int64(len(src)) {
			want = src[offset:min(offset+size, int64(len(src)))]
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("range [%d,+%d) of %d bytes at frame %d: got %d bytes, want %d",
				offset, size, len(src), frameSize, len(got), len(want))
		}
	})
}
