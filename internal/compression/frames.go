package compression

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/klauspost/compress/zstd"
)

// Seekable zstd framing (#185): the stored object is a concatenation of independently decodable
// zstd frames, preceded by a skippable frame holding an index over them. A reader that wants
// [offset, offset+size) fetches the index, finds the frames covering that span, and ranged-GETs
// only those — instead of the whole stored body, which is what happens today.
//
// Three properties of this layout are load-bearing and each has a reason that is not obvious:
//
//  1. **The index leads, it does not trail.** CargoShip's equivalent format (2.1, "random-access
//     frame index") puts its index in a sidecar manifest and, where it is in-stream, at the end —
//     it has to, because it streams a tar pipeline through the encoder and cannot know the index
//     until the stream is done. ObjectFS does not stream: Compress takes a whole buffer and
//     prepareUpload compresses before the multipart dispatch, so the entire compressed object is
//     in memory at write time at any size. Leading means one prefix `GET bytes=0-K` can carry the
//     index *and* the first data frames, where a trailing index needs two requests. Most reads of
//     a file start at offset 0.
//
//  2. **Per-frame checksums are over the compressed bytes, and are checked before the decoder
//     runs.** That is exactly what a ranged GET of [CompressedOffset, CompressedSize) returns, so
//     a corrupt or hostile frame never reaches the decompressor — which is code fed
//     attacker-controlled length fields. It also means the check catches every way a store can
//     mis-serve a range: a clamped unsatisfiable range, an off-by-one range, or a range served
//     from a *different generation* of an overwritten object all fail the frame hash. That last
//     one is why this is safe to attempt on an endpoint that ignores If-Match.
//
//  3. **The object is the authority, never the metadata.** A fixed-size descriptor in S3 user
//     metadata is an accelerator only. SetObjectMetadata is a CopyObject with
//     MetadataDirective=REPLACE, which discards everything not restated — the same mechanism that
//     once made a chmod capable of dropping Content-Encoding and rendering a compressed object
//     permanently unreadable. If the descriptor were authoritative, a chmod would silently
//     de-seek an object; because the index is in the object, losing the descriptor costs one
//     round trip.
//
// A plain `zstd -d` still reads the whole object: skippable frames are skipped by definition, and
// a concatenation of frames is a legal zstd stream.

const (
	// FrameIndexSkippableID is the zstd skippable-frame ID (0x184D2A50 | id) carrying the index.
	//
	// Deliberately not 0xE. The Zstandard Seekable Format convention claims 0x184D2A5E for its own
	// seek table, and this is not that format — its table records a 32-bit XXH64 truncation over
	// each frame's *decompressed* bytes, which is both weaker than SHA-256 and on the wrong side of
	// the decoder to protect it. Claiming 0xE would make two incompatible layouts answer to one ID.
	FrameIndexSkippableID = 0xD

	// FrameIndexVersion is the version written into both the index frame and the descriptor.
	// It lives in two places because a CopyObject can drop the descriptor but not the frame.
	FrameIndexVersion = 1

	// MinFrameSize and MaxFrameSize bound DeriveFrameSize. Below the floor the index dominates
	// the transfer; above the ceiling a single frame does.
	MinFrameSize = 256 << 10
	MaxFrameSize = 16 << 20

	// skippableHeaderSize is the zstd skippable frame header: 4-byte magic, 4-byte payload length.
	skippableHeaderSize = 8

	// frameIndexMagic identifies the payload as ObjectFS's index rather than some other tool's
	// skippable frame, which the skippable ID alone cannot establish.
	frameIndexMagic = "OFSI"

	// fixedHeaderSize is the index payload's header, ahead of the per-frame records.
	//
	//	[0:4]   magic "OFSI"
	//	[4]     version
	//	[5]     flags
	//	[6:8]   reserved, must be zero
	//	[8:16]  frame size (uncompressed bytes per frame, little-endian)
	//	[16:24] total uncompressed size (little-endian)
	//	[24:28] frame count (little-endian)
	//	[28:32] reserved, must be zero
	//	[32:64] SHA-256 over the whole uncompressed content
	fixedHeaderSize = 64

	// frameRecordSize is one per-frame record: compressed size, uncompressed size, checksum.
	// The two offsets are a prefix sum and are not stored — see appendFrameIndex.
	frameRecordSize = 4 + 4 + sha256.Size

	// flagContentSHA256 marks bytes [32:64] of the header as carrying the whole-object hash.
	flagContentSHA256 = 1 << 0

	// indexRecordCostBytes is frameRecordSize, named separately because DeriveFrameSize uses it as
	// a cost coefficient rather than as a layout constant.
	indexRecordCostBytes = frameRecordSize
)

// maxIndexPayload bounds the encoded index payload.
//
// The bound is load-bearing, not defensive. A skippable frame's size field is a uint32, so an index
// that encoded to more than that would be written with a truncated length: the frame would claim to
// end in the middle of itself, every CompressedOffset behind it would be wrong, and the object would
// be silently corrupt at write time. MaxInt32 rather than MaxUint32 because IndexFrameLength refuses
// anything above MaxInt32 on the way back in, and a writer that can emit what no reader accepts is
// worse than one that refuses. At 40 bytes a record this still allows ~53M frames, which is 13 TiB at
// the 256 KiB floor.
//
// A var rather than a const for one reason: the real ceiling is only reachable at ~53M frames, which
// is a multi-gigabyte allocation of Frame values, so a test at the true value is not runnable. The
// guard is lowered by TestAppendFrameIndexRefusesAnIndexOverTheCeiling instead. An untested branch
// inside a silent-corruption guard is the branch most likely to be wrong.
var maxIndexPayload int64 = math.MaxInt32

// Errors reported by this file. They are sentinels rather than objectfs *errors.Error values
// because internal/compression has no dependency on the error-code taxonomy; the storage layer
// maps ErrIndexCorrupt and ErrFrameChecksumMismatch onto ErrCodeDataCorruption at its boundary,
// which is where an operator-facing code belongs.
var (
	// ErrNotFramed means the bytes do not begin with an ObjectFS index frame. It is not a
	// corruption signal: every object written before #185 legitimately looks like this, and the
	// caller's correct response is the whole-object path.
	ErrNotFramed = errors.New("not a framed objectfs object")

	// ErrIndexCorrupt means the index frame was found but does not describe a consistent object —
	// a bad self-hash, a frame table that does not tile the object, or a reserved field in use.
	ErrIndexCorrupt = errors.New("frame index is corrupt")

	// ErrFrameChecksumMismatch means a frame's compressed bytes do not hash to the value the index
	// recorded. This is never recoverable by retrying a whole-object read: it is reported so the
	// caller fails closed.
	ErrFrameChecksumMismatch = errors.New("frame checksum mismatch")
)

// ShortIndexError reports that a prefix was too short to parse, and how many bytes are needed.
// It exists so a reader can over-fetch a guessed prefix and correct itself in one further hop
// rather than probing.
type ShortIndexError struct {
	// Have is how many bytes the caller supplied; Need is the total required from offset 0.
	Have, Need int
}

func (e *ShortIndexError) Error() string {
	return fmt.Sprintf("frame index needs %d bytes from offset 0, have %d", e.Need, e.Have)
}

// Frame describes one independently decodable zstd frame within a stored object.
//
// CompressedOffset is relative to the start of the stored object, so it already includes the
// leading index frame and is directly usable as a Range. UncompressedOffset is relative to the
// start of the file's content.
// The four sizes are int64 even though the format stores them as uint32 and uint64. The wire widths
// are the format's business and are narrowed to in AppendFrameIndex, behind range checks; in memory
// these values are offsets and lengths that every caller uses as int64 — a Range header, a ReadAt, a
// slice bound — so unsigned fields here would buy nothing and cost a conversion at each of those
// call sites. Conversions are where truncation bugs live, and the safest number of them is none.
type Frame struct {
	CompressedOffset   int64
	CompressedSize     int64
	UncompressedOffset int64
	UncompressedSize   int64
	Checksum           [sha256.Size]byte
}

// FrameIndex is the decoded contents of the leading skippable frame.
type FrameIndex struct {
	Version uint8

	// FrameSize is the uncompressed span of every frame but the last, which holds the remainder.
	FrameSize int64

	// UncompressedSize is the file's content length, i.e. what objectfs-original-size records.
	UncompressedSize int64

	// ContentSHA256 is SHA-256 over the whole uncompressed content — the same value, with the same
	// meaning, as the objectfs-sha256 metadata key. It is duplicated here so that an object whose
	// metadata was dropped by a CopyObject is still fully verifiable from its own bytes.
	ContentSHA256    [sha256.Size]byte
	HasContentSHA256 bool

	Frames []Frame
}

// DeriveFrameSize returns the frame size that minimizes the bytes a cold read of one frame
// transfers, rounded to a power of two and clamped to [MinFrameSize, MaxFrameSize].
//
// For uncompressed size S, compression ratio r, and frame size F, a cold read costs
// indexRecordCostBytes*S/F bytes of index plus F/r bytes of frame. That is U-shaped in F and
// minimized at F = sqrt(indexRecordCostBytes * S * r).
//
// This is why neither #185's original guess of 1–4 MiB nor CargoShip's 16 MiB is simply adopted.
// Both are correct for their own cost model: CargoShip fetches one manifest per archive and gets
// every chunk's index with it, so index size is a rounding error there, while ObjectFS pays for an
// index on the cold read of each object, making it a first-order term. CargoShip's 16 MiB is in
// fact the optimum this formula gives for a single object of roughly 2 TiB.
//
// Ratio matters far less than transfer does. Across a 16x range of frame size, stored size moves
// under 1% on locality-bearing data and ~20% on self-similar text, while the bytes a single cold
// read transfers move by an order of magnitude. So this optimizes transfer and lets ratio fall
// where it may.
func DeriveFrameSize(uncompressedSize int64, ratio float64) int64 {
	if uncompressedSize <= 0 || ratio <= 0 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return MinFrameSize
	}

	optimum := math.Sqrt(indexRecordCostBytes * float64(uncompressedSize) * ratio)
	if math.IsNaN(optimum) || math.IsInf(optimum, 0) || optimum <= MinFrameSize {
		return MinFrameSize
	}
	if optimum >= MaxFrameSize {
		return MaxFrameSize
	}

	// Round to the nearest power of two in the exponent, not the value: halfway between 512 KiB
	// and 1 MiB should land on the one whose log2 is closer, which is 724 KiB, not 768 KiB.
	rounded := math.Ldexp(1, int(math.Round(math.Log2(optimum))))
	// The floor arm is load-bearing: an optimum just above MinFrameSize can round *down* below it.
	// The ceiling arm is not currently observable, because MaxFrameSize is itself a power of two and
	// the guard above already returned for anything at or over it, so rounding can only reach exactly
	// MaxFrameSize — which the default arm would also return. It stays because that stops being true
	// the moment MaxFrameSize is changed to something that is not a power of two.
	switch {
	case rounded <= MinFrameSize:
		return MinFrameSize
	case rounded >= MaxFrameSize:
		return MaxFrameSize
	default:
		return int64(rounded)
	}
}

// ratioSampleBytes is how much of an object is trial-encoded by [ZstdCodec.EstimateRatio].
//
// 1 MiB, and the size is chosen against how much precision [DeriveFrameSize] can actually use rather
// than against how accurate an estimate is achievable. F is proportional to sqrt(r) and is then
// rounded to a power of two, so r has to be wrong by a factor of four before the answer moves by a
// single step — a 2x error moves F by 1.41x, which rounds to the same exponent more often than not.
// Paying a full extra encode of a multi-gigabyte object to refine an input that coarse is not a trade
// worth making.
const ratioSampleBytes = 1 << 20

// ratioSampleWindows is how many places in the object the sample is drawn from.
//
// Three, and not one, because a prefix is a systematically unrepresentative sample of the files this
// project's users store. A BAM or a compressed tar has an incompressible member at the front and may
// be compressible after it; a CSV or a FASTQ has a header line that looks nothing like the body. A
// prefix-only sample of the adversarial cases measured 15x off in one direction and 5x in the other,
// which is two powers of two of frame size — past the point where the square root absorbs it. Head,
// middle, and tail for the same total encode cost brings both inside one step.
const ratioSampleWindows = 3

// EstimateRatio returns an approximate compression ratio for src, computed by encoding a sample of
// it. It exists to give [DeriveFrameSize] its r without compressing the whole object twice.
//
// A ratio of 1 is returned when there is nothing to measure, which DeriveFrameSize reads as "assume
// no compression" and answers with the frame-size floor.
//
// The sample is a concatenation of [ratioSampleWindows] evenly spaced windows rather than one
// contiguous run, which trades a small systematic *under*-estimate for the removal of a large
// positional bias. Splitting the sample gives the encoder less window to reuse, so the measured ratio
// comes out slightly below the true one; that direction is the safe one, because it makes F smaller
// and a frame size below the optimum costs transfer at worst, where one above it costs transfer on
// every read.
func (c *ZstdCodec) EstimateRatio(src []byte) float64 {
	if len(src) == 0 {
		return 1
	}

	sample := src
	if len(src) > ratioSampleBytes {
		// Windows are laid out so the first starts at 0 and the last ends at len(src): stride is the
		// gap between window starts, and with ratioSampleWindows-1 strides the final one lands exactly
		// at the end. An object between ratioSampleBytes and ratioSampleWindows*window would otherwise
		// have overlapping windows, which is harmless but would double-count bytes.
		window := ratioSampleBytes / ratioSampleWindows
		stride := (len(src) - window) / (ratioSampleWindows - 1)

		sample = make([]byte, 0, window*ratioSampleWindows)
		for i := range ratioSampleWindows {
			start := i * stride
			sample = append(sample, src[start:start+window]...)
		}
	}

	encoded := c.encoder.EncodeAll(sample, make([]byte, 0, len(sample)/2))
	if len(encoded) == 0 {
		return 1
	}

	return float64(len(sample)) / float64(len(encoded))
}

// CompressFramed encodes src as a leading index frame followed by independently decodable zstd
// frames of frameSize uncompressed bytes each, and returns the stored object together with its
// index.
//
// contentSHA256 is required rather than computed here: prepareUpload already hashes the
// uncompressed content before encoding anything, and hashing a multi-gigabyte buffer twice to
// spare one parameter is not a trade worth making. Requiring it also keeps the whole-object
// attestation from being an optional field that a caller can forget.
//
// An empty src is not framed — there is nothing to seek within — and is reported as
// errors.ErrUnsupported so the caller takes the whole-object path.
func (c *ZstdCodec) CompressFramed(src []byte, frameSize int64, contentSHA256 [sha256.Size]byte) ([]byte, *FrameIndex, error) {
	switch {
	case len(src) == 0:
		return nil, nil, fmt.Errorf("framing an empty object: %w", errors.ErrUnsupported)
	case frameSize <= 0:
		return nil, nil, fmt.Errorf("frame size must be positive, got %d", frameSize)
	case frameSize > math.MaxUint32:
		// Per-frame sizes are uint32 in the record, and a frame that big defeats the point.
		return nil, nil, fmt.Errorf("frame size %d exceeds %d", frameSize, int64(math.MaxUint32))
	}

	size := int64(len(src))
	frameCount := (size + frameSize - 1) / frameSize

	// The index's own length depends only on the frame count, which is known before any encoding,
	// so CompressedOffset can be filled in as frames are produced rather than fixed up afterwards.
	payloadLen := indexPayloadLen(frameCount)
	if payloadLen > maxIndexPayload {
		return nil, nil, fmt.Errorf("%d bytes at a frame size of %d needs %d frames and a %d-byte index, over the %d-byte ceiling",
			size, frameSize, frameCount, payloadLen, maxIndexPayload)
	}
	indexLen := skippableHeaderSize + payloadLen

	idx := &FrameIndex{
		Version:          FrameIndexVersion,
		FrameSize:        frameSize,
		UncompressedSize: size,
		ContentSHA256:    contentSHA256,
		HasContentSHA256: true,
		Frames:           make([]Frame, 0, frameCount),
	}

	// Encode first, then emit the index in front. Building the compressed frames into their final
	// position would mean either guessing the total size or copying it.
	body := make([]byte, 0, size/2+frameCount*skippableHeaderSize)
	for start := int64(0); start < size; start += frameSize {
		end := min(start+frameSize, size)

		before := int64(len(body))
		body = c.encoder.EncodeAll(src[start:end], body)
		encoded := body[before:]

		idx.Frames = append(idx.Frames, Frame{
			CompressedOffset:   indexLen + before,
			CompressedSize:     int64(len(encoded)),
			UncompressedOffset: start,
			UncompressedSize:   end - start,
			Checksum:           sha256.Sum256(encoded),
		})
	}

	out, err := AppendFrameIndex(make([]byte, 0, indexLen+int64(len(body))), idx)
	if err != nil {
		return nil, nil, err
	}
	if int64(len(out)) != indexLen {
		// Would mean indexPayloadLen and AppendFrameIndex disagree, which would corrupt every
		// CompressedOffset above. Cheap to assert, silent and total if it ever happens.
		return nil, nil, fmt.Errorf("%w: index frame is %d bytes, expected %d",
			ErrIndexCorrupt, len(out), indexLen)
	}

	return append(out, body...), idx, nil
}

// DecompressFrame decodes exactly one frame, verifying its compressed bytes against the index
// before the decoder sees them.
//
// frameBytes must be precisely the bytes at [f.CompressedOffset, f.CompressedSize) and nothing
// else. Passing a longer slice is rejected rather than tolerated: DecodeAll over a concatenation
// happily decodes the following frames too, so a slack slice would silently return a neighbour's
// content and pass every length check that came after.
// The length check is a fast reject, not the guarantee: any length difference also changes the hash,
// so the checksum below would catch a slack slice on its own. It runs first so the error names the
// mismatch precisely and so a hostile CompressedSize cannot make this hash an arbitrarily large
// buffer before being refused.
func (c *ZstdCodec) DecompressFrame(frameBytes []byte, f Frame) ([]byte, error) {
	if int64(len(frameBytes)) != f.CompressedSize {
		return nil, fmt.Errorf("%w: frame at %d is %d bytes, index says %d",
			ErrFrameChecksumMismatch, f.CompressedOffset, len(frameBytes), f.CompressedSize)
	}
	if got := sha256.Sum256(frameBytes); got != f.Checksum {
		return nil, fmt.Errorf("%w: frame at %d hashes to %x, index says %x",
			ErrFrameChecksumMismatch, f.CompressedOffset, got[:8], f.Checksum[:8])
	}

	out, err := c.decoder.DecodeAll(frameBytes, make([]byte, 0, f.UncompressedSize))
	if err != nil {
		return nil, fmt.Errorf("decode frame at %d: %w", f.CompressedOffset, err)
	}
	if int64(len(out)) != f.UncompressedSize {
		return nil, fmt.Errorf("%w: frame at %d decoded to %d bytes, index says %d",
			ErrIndexCorrupt, f.CompressedOffset, len(out), f.UncompressedSize)
	}
	return out, nil
}

// FramesCovering returns the frames overlapping [offset, offset+size) of the uncompressed content,
// and the offset of the requested span within the first of them.
//
// The second return value is the reason this is a method rather than arithmetic at the call site.
// A reader that has fetched a frame holds that frame's bytes, not the object's, so it must slice
// at offset-frame.UncompressedOffset. Applying the object-relative offset to a frame-relative
// buffer is the defect this signature exists to make hard to write.
//
// A size of zero or less, or an offset at or past the end, returns no frames.
func (idx *FrameIndex) FramesCovering(offset, size int64) (frames []Frame, offsetInFirst int64) {
	if size <= 0 || offset < 0 || offset >= idx.UncompressedSize || len(idx.Frames) == 0 {
		return nil, 0
	}

	end := min(offset+size, idx.UncompressedSize)

	// Safe as division because ParseFrameIndex has already established that the frames tile the
	// content contiguously at exactly FrameSize, so frame i spans [i*FrameSize, ...).
	first := offset / idx.FrameSize
	last := min((end-1)/idx.FrameSize, int64(len(idx.Frames))-1)

	return idx.Frames[first : last+1], offset - idx.Frames[first].UncompressedOffset
}

// indexPayloadLen is the encoded payload length for n frames: header, records, and the trailing
// self-hash that lets a reader detect an index damaged in transit before it trusts any offset.
func indexPayloadLen(n int64) int64 {
	return fixedHeaderSize + n*frameRecordSize + sha256.Size
}

// AppendFrameIndex appends idx, encoded as a zstd skippable frame, to dst.
//
// The skippable header comes from zstd.Header.AppendTo rather than being written by hand. #185's
// design comment claimed klauspost/compress exports no skippable-frame writer and that ObjectFS
// would have to emit the 8-byte header itself; that is wrong as of v1.20.0 — AppendTo is exported
// and is the exact inverse of Header.Decode, which is what the reader uses.
// This is also the only place an in-memory int64 is narrowed to one of the format's fixed-width
// fields, and every such narrowing is range-checked here rather than trusted. That is deliberate: a
// FrameIndex can be built by any caller, and a value that wrapped on the way onto the wire would
// produce an object whose index disagrees with its own bytes — which the per-frame checksums would
// then report as corruption at read time, arbitrarily later and on a different machine.
func AppendFrameIndex(dst []byte, idx *FrameIndex) ([]byte, error) {
	payloadLen := indexPayloadLen(int64(len(idx.Frames)))
	switch {
	case payloadLen > maxIndexPayload:
		return nil, fmt.Errorf("an index for %d frames is %d bytes, over the %d-byte ceiling",
			len(idx.Frames), payloadLen, maxIndexPayload)
	case idx.FrameSize <= 0:
		return nil, fmt.Errorf("frame size must be positive, got %d", idx.FrameSize)
	case idx.UncompressedSize < 0:
		return nil, fmt.Errorf("uncompressed size must not be negative, got %d", idx.UncompressedSize)
	}

	// Narrowed in one statement so the checks above cover the whole set, rather than each conversion
	// having to restate which bound protects it.
	// #nosec G115 -- FrameSize and UncompressedSize checked non-negative above; payloadLen, and so
	// the frame count derived from the same records, is bounded by maxIndexPayload = MaxInt32.
	var (
		wireFrameSize  = uint64(idx.FrameSize)
		wireTotalSize  = uint64(idx.UncompressedSize)
		wireFrameCount = uint32(len(idx.Frames))
		wirePayloadLen = uint32(payloadLen)
	)

	payload := make([]byte, fixedHeaderSize+len(idx.Frames)*frameRecordSize, payloadLen)
	copy(payload, frameIndexMagic)
	payload[4] = idx.Version
	if idx.HasContentSHA256 {
		payload[5] |= flagContentSHA256
	}
	binary.LittleEndian.PutUint64(payload[8:16], wireFrameSize)
	binary.LittleEndian.PutUint64(payload[16:24], wireTotalSize)
	binary.LittleEndian.PutUint32(payload[24:28], wireFrameCount)
	copy(payload[32:64], idx.ContentSHA256[:])

	for i, f := range idx.Frames {
		if f.CompressedSize <= 0 || f.CompressedSize > math.MaxUint32 ||
			f.UncompressedSize <= 0 || f.UncompressedSize > math.MaxUint32 {
			return nil, fmt.Errorf("frame %d spans %d compressed and %d uncompressed bytes; the format stores each as a uint32",
				i, f.CompressedSize, f.UncompressedSize)
		}
		// #nosec G115 -- both extents range-checked immediately above
		wireCompressed, wireUncompressed := uint32(f.CompressedSize), uint32(f.UncompressedSize)

		rec := payload[fixedHeaderSize+i*frameRecordSize:]
		binary.LittleEndian.PutUint32(rec[0:4], wireCompressed)
		binary.LittleEndian.PutUint32(rec[4:8], wireUncompressed)
		copy(rec[8:8+sha256.Size], f.Checksum[:])
	}

	selfHash := sha256.Sum256(payload)
	payload = append(payload, selfHash[:]...)

	header := zstd.Header{
		Skippable:     true,
		SkippableID:   FrameIndexSkippableID,
		SkippableSize: wirePayloadLen,
	}
	dst, err := header.AppendTo(dst)
	if err != nil {
		return nil, fmt.Errorf("encode skippable header: %w", err)
	}
	return append(dst, payload...), nil
}

// IndexFrameLength reports the total length of the leading index frame, header included, from as
// few as the first 8 bytes.
//
// This is what makes a single-request cold read possible: a reader over-fetches a guessed prefix,
// reads the true length out of the first 8 bytes, and either already has the whole index or knows
// exactly how much more to ask for — no probe round trip.
func IndexFrameLength(prefix []byte) (int, error) {
	if len(prefix) < skippableHeaderSize {
		return 0, &ShortIndexError{Have: len(prefix), Need: skippableHeaderSize}
	}

	var h zstd.Header
	if err := h.Decode(prefix[:skippableHeaderSize]); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrNotFramed, err)
	}
	if !h.Skippable || h.SkippableID != FrameIndexSkippableID {
		return 0, ErrNotFramed
	}
	if h.SkippableSize > math.MaxInt32 {
		return 0, fmt.Errorf("%w: index frame payload claims %d bytes", ErrIndexCorrupt, h.SkippableSize)
	}
	return h.HeaderSize + int(h.SkippableSize), nil
}

// ParseFrameIndex decodes the leading index frame from a prefix of a stored object, returning the
// index and the total length of the index frame — which is where the first data frame begins.
//
// Every consistency check the format allows happens here, so that FramesCovering downstream can be
// plain arithmetic. A reader that gets a *FrameIndex back may treat its frame table as tiling the
// content contiguously at FrameSize.
func ParseFrameIndex(prefix []byte) (*FrameIndex, int, error) {
	frameLen, err := IndexFrameLength(prefix)
	if err != nil {
		return nil, 0, err
	}
	if len(prefix) < frameLen {
		return nil, 0, &ShortIndexError{Have: len(prefix), Need: frameLen}
	}

	payload := prefix[skippableHeaderSize:frameLen]
	if len(payload) < fixedHeaderSize+sha256.Size {
		return nil, 0, fmt.Errorf("%w: payload is %d bytes, minimum is %d",
			ErrIndexCorrupt, len(payload), fixedHeaderSize+sha256.Size)
	}
	if string(payload[0:4]) != frameIndexMagic {
		return nil, 0, ErrNotFramed
	}

	// Verify the index's own bytes before reading a single offset out of them. Everything below
	// this point is trusted arithmetic, so it has to be after the hash, not before.
	body := payload[:len(payload)-sha256.Size]
	want := payload[len(payload)-sha256.Size:]
	if got := sha256.Sum256(body); !bytes.Equal(got[:], want) {
		return nil, 0, fmt.Errorf("%w: index self-hash mismatch", ErrIndexCorrupt)
	}

	version := payload[4]
	if version != FrameIndexVersion {
		// A future version is not corruption. Report it as unframed so the caller degrades to the
		// whole-object path, which stays correct for any format that keeps a skippable index.
		return nil, 0, fmt.Errorf("%w: index version %d, this build understands %d",
			ErrNotFramed, version, FrameIndexVersion)
	}
	if payload[5]&^byte(flagContentSHA256) != 0 || payload[6] != 0 || payload[7] != 0 ||
		binary.LittleEndian.Uint32(payload[28:32]) != 0 {
		return nil, 0, fmt.Errorf("%w: reserved field is set", ErrIndexCorrupt)
	}

	// The two sizes are uint64 on the wire and int64 in memory, so a value above MaxInt64 is rejected
	// here rather than being allowed to appear as a negative length downstream. Nothing legitimate is
	// refused: an object that large cannot be addressed by a Range header, whose offsets are decimal
	// int64 by RFC 9110.
	wireFrameSize := binary.LittleEndian.Uint64(payload[8:16])
	wireTotalSize := binary.LittleEndian.Uint64(payload[16:24])
	if wireFrameSize > math.MaxInt64 || wireTotalSize > math.MaxInt64 {
		return nil, 0, fmt.Errorf("%w: frame size %d and content size %d must both be at most %d",
			ErrIndexCorrupt, wireFrameSize, wireTotalSize, int64(math.MaxInt64))
	}

	// #nosec G115 -- both bounded to MaxInt64 immediately above
	idx := &FrameIndex{
		Version:          version,
		FrameSize:        int64(wireFrameSize),
		UncompressedSize: int64(wireTotalSize),
		HasContentSHA256: payload[5]&flagContentSHA256 != 0,
	}
	copy(idx.ContentSHA256[:], payload[32:64])

	// int64 rather than int for the declared count: on a 32-bit build a uint32 read off the wire does
	// not fit an int, and the comparison this feeds would be against a wrapped value.
	count := int64(binary.LittleEndian.Uint32(payload[24:28]))
	records := int64(len(body) - fixedHeaderSize)
	if count != records/frameRecordSize || records%frameRecordSize != 0 {
		return nil, 0, fmt.Errorf("%w: %d frames declared, %d bytes of records",
			ErrIndexCorrupt, count, records)
	}
	if idx.FrameSize == 0 {
		return nil, 0, fmt.Errorf("%w: frame size is zero", ErrIndexCorrupt)
	}

	// Establish the tiling invariant that FramesCovering relies on, rather than assuming it: every
	// frame but the last spans exactly FrameSize, the last holds the remainder, and the compressed
	// extents follow the index frame contiguously. An index that disagrees with itself is
	// corruption, not a slow path.
	idx.Frames = make([]Frame, count)
	nextCompressed := int64(frameLen)
	var uncompressed int64
	for i := range idx.Frames {
		rec := body[fixedHeaderSize+i*frameRecordSize:]
		f := Frame{
			CompressedOffset:   nextCompressed,
			CompressedSize:     int64(binary.LittleEndian.Uint32(rec[0:4])),
			UncompressedOffset: uncompressed,
			UncompressedSize:   int64(binary.LittleEndian.Uint32(rec[4:8])),
		}
		copy(f.Checksum[:], rec[8:8+sha256.Size])

		if f.CompressedSize == 0 || f.UncompressedSize == 0 {
			return nil, 0, fmt.Errorf("%w: frame %d has a zero extent", ErrIndexCorrupt, i)
		}
		expected := idx.FrameSize
		if i == len(idx.Frames)-1 {
			expected = idx.UncompressedSize - uncompressed
		}
		if f.UncompressedSize != expected {
			return nil, 0, fmt.Errorf("%w: frame %d spans %d uncompressed bytes, tiling requires %d",
				ErrIndexCorrupt, i, f.UncompressedSize, expected)
		}

		idx.Frames[i] = f
		nextCompressed += f.CompressedSize
		uncompressed += f.UncompressedSize
	}

	if uncompressed != idx.UncompressedSize {
		return nil, 0, fmt.Errorf("%w: frames cover %d uncompressed bytes, header says %d",
			ErrIndexCorrupt, uncompressed, idx.UncompressedSize)
	}
	return idx, frameLen, nil
}
