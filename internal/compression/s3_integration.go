package compression

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
	"github.com/scttfrdmn/objectfs/pkg/utils"
)

// Settings are the inputs needed to build a Compressor.
//
// This type exists rather than taking a config.CompressionConfig so that a codec package does not
// depend on the application's configuration package. The dependency used to run that way, and it
// meant the config layer could not validate an algorithm by the only means that cannot go stale —
// asking this package to build the codec — because doing so would have been an import cycle. That
// is why config defaulted to an algorithm with no implementation for an entire release: the check
// that would have caught it was structurally unavailable.
//
// MinSize is a string because it is a human-written size ("4KB") in every configuration format
// ObjectFS reads.
type Settings struct {
	// Enabled turns compression on. When false, the algorithm is not consulted.
	Enabled bool
	// Algorithm names the codec. See pkg/compression.SupportedAlgorithms.
	Algorithm string
	// Level is the codec-specific compression level; 0 selects the codec's default. Valid ranges
	// differ per algorithm — zstd accepts 0-22, gzip only 0-9.
	Level int
	// MinSize is the smallest object worth compressing, e.g. "4KB". Empty or "0" means no minimum.
	MinSize string
}

// Compressor wraps a Codec with minimum-size enforcement for transparent S3
// object compression.  A Compressor whose codec is AlgorithmNone acts as a
// pass-through and reports Enabled() == false.
//
// Writing and reading are deliberately asymmetric. One codec is used to write, chosen by
// configuration, and *every* codec is available to read, chosen by the object. A bucket accumulates
// objects across configuration changes and across the tools that wrote them, so the algorithm a
// mount is set to says nothing about the algorithm the object in front of it used.
type Compressor struct {
	codec   comprpkg.Codec
	minSize int64

	// decoders holds a codec per Content-Encoding token, built once at construction.
	//
	// Keyed on the token rather than on the [comprpkg.Algorithm] name because the token is what the
	// object carries and what the read path has in hand. They happen to be the same strings for all
	// three codecs, and that is a coincidence of naming rather than a guarantee — ContentEncoding is a
	// separate method from Algorithm precisely because a codec is free to have them differ.
	decoders map[string]comprpkg.Codec
}

// NewCompressor builds a Compressor from Settings.
// When cfg.Enabled is false a nop compressor is returned: it writes nothing compressed, and still
// decodes every algorithm, because objects already in the bucket do not stop being compressed when a
// mount turns compression off.
//
// Calling this is also how a caller validates a compression configuration: it is the only check that
// cannot drift from what the codecs actually support, because it is the code that builds them.
func NewCompressor(cfg Settings) (*Compressor, error) {
	decoders, err := buildDecoders()
	if err != nil {
		return nil, err
	}

	if !cfg.Enabled {
		return &Compressor{codec: &nopCodec{}, minSize: 0, decoders: decoders}, nil
	}

	codec, err := New(comprpkg.Algorithm(cfg.Algorithm), cfg.Level)
	if err != nil {
		return nil, fmt.Errorf("create %s codec: %w", cfg.Algorithm, err)
	}

	minSize, err := utils.ParseOptionalBytes(cfg.MinSize)
	if err != nil {
		return nil, fmt.Errorf("invalid min_size %q: %w", cfg.MinSize, err)
	}

	// The write codec is used for its own token rather than a second instance of the same algorithm,
	// so a configured level is not silently a different level on the way back in. It makes no
	// difference to the output — decoding does not consult the level — but two codecs for one
	// algorithm is two things that can be made to disagree.
	decoders[codec.ContentEncoding()] = codec

	return &Compressor{codec: codec, minSize: minSize, decoders: decoders}, nil
}

// buildDecoders constructs one codec per algorithm that has a Content-Encoding token.
//
// Built from [comprpkg.SupportedAlgorithms] rather than from a list here, so an algorithm added
// there is readable without a second edit. That matters more than it looks: the reason this map
// exists is that the read path had exactly one codec, and a list of decoders maintained separately
// from the list of algorithms would reintroduce the same class of gap — an algorithm ObjectFS can
// write and cannot read.
//
// Every codec is constructed at its default level. A level is an encoder parameter; all three
// formats are self-describing on the way back, so it has no bearing on decoding.
func buildDecoders() (map[string]comprpkg.Codec, error) {
	decoders := make(map[string]comprpkg.Codec)

	for _, algo := range comprpkg.SupportedAlgorithms() {
		codec, err := New(algo, comprpkg.DefaultLevel)
		if err != nil {
			return nil, fmt.Errorf("build decoder for %s: %w", algo, err)
		}

		// AlgorithmNone's token is empty, and an empty Content-Encoding means "not encoded" rather
		// than naming a codec. Registering it would make the map answer for a case Decompress handles
		// before it ever looks here.
		if token := codec.ContentEncoding(); token != "" {
			decoders[token] = codec
		}
	}

	return decoders, nil
}

// Enabled returns true when an active (non-nop) codec is in use.
func (c *Compressor) Enabled() bool {
	return c.codec.Algorithm() != comprpkg.AlgorithmNone
}

// Compress compresses data when compression is enabled, the data size meets the minimum threshold,
// and the data is not already compressed.  If the compressed form is larger than the original, the
// original is returned unchanged with wasCompressed == false.
//
// The already-compressed check is [AlreadyCompressed] on the first 4 KiB, and it is the difference
// between spending CPU and spending it for nothing: the formats this project's users store most —
// BAM, CRAM, tar.zst, JPEG, MP4 — are at their entropy limit, so without it the codec runs the whole
// object through and produces something the size check below then discards ([#184]). Measured on an
// M4 Max at zstd level 3: 1.85ms → 2.2µs on an 8 MiB zstd frame, 237µs → 2.1µs at 1 MiB, and 33 MiB of
// allocation per write down to zero. Data that *does* compress pays 16ns for the magic-byte comparison
// and is otherwise unchanged.
//
// Ordered after the size floor deliberately, and both gates are length or prefix comparisons rather
// than a scan of the object. See AlreadyCompressed for why it is that function and not Analyze, which
// costs 2.1µs more for an entropy figure this decision does not use.
//
// Returns: (data, wasCompressed, error)
//
// [#184]: https://github.com/scttfrdmn/objectfs/issues/184
func (c *Compressor) Compress(data []byte) ([]byte, bool, error) {
	if !c.Enabled() || int64(len(data)) < c.minSize {
		return data, false, nil
	}

	if AlreadyCompressed(data) {
		return data, false, nil
	}

	compressed, err := c.codec.Compress(data)
	if err != nil {
		return nil, false, fmt.Errorf("compress: %w", err)
	}

	// Discard the compressed form when it offers no space savings.
	if int64(len(compressed)) >= int64(len(data)) {
		return data, false, nil
	}

	return compressed, true, nil
}

// CompressFramed is [Compressor.Compress] for the seekable layout (#185): the returned body is a
// leading index frame followed by independently decodable zstd frames, and desc is the fixed-size
// summary to store in user metadata under the backend's seekable key.
//
// framed == false with a nil error means the object was not framed and the caller should take the
// ordinary Compress path. That is the common answer, not an error path, and there are five reasons
// for it:
//
//   - compression is disabled, or the object is below the configured minimum size;
//   - the object is already in a compressed format, by the same [AlreadyCompressed] prefix check
//     Compress uses;
//   - the write codec is not zstd. gzip and lz4 have no equivalent of a skippable frame that a
//     standard decoder is required to ignore, so framing them would produce an object only ObjectFS
//     could read, which is a portability cost the format does not pay for zstd;
//   - the object fits in one frame. An index over a single frame is 144 bytes buying nothing:
//     there is no other frame to seek to, and a reader that wants any part of it fetches the whole
//     body either way;
//   - the framed body came out no smaller than the input, which is Compress's own discard rule.
//
// Nothing here consults a configuration flag, and that is deliberate. A framed object is a legal
// zstd stream — `zstd -d` and any standard decoder read it, skipping the index — so the choice is
// not between two formats a reader has to be told about. It is between an object whose ranged reads
// cost one or two frames and one whose ranged reads cost the whole body, which for a 10 GiB object
// is a difference of four orders of magnitude. The cost is stored size: a frame boundary throws away
// the compressor's window, which measures under 1% on data with read locality and about 20% on
// self-similar text at the 256 KiB frame-size floor. That is the trade, and it is made once here
// rather than left as a knob whose wrong setting is invisible.
//
// contentSHA256 must be the hash of the whole uncompressed content, computed by the caller before
// any encoding. It goes into the index frame so that an object which loses all of its user metadata
// to a CopyObject is still self-verifying.
func (c *Compressor) CompressFramed(data []byte, contentSHA256 [sha256.Size]byte) (
	body []byte, desc SeekableDescriptor, framed bool, err error,
) {
	if !c.Enabled() || int64(len(data)) < c.minSize {
		return nil, SeekableDescriptor{}, false, nil
	}

	if AlreadyCompressed(data) {
		return nil, SeekableDescriptor{}, false, nil
	}

	// A type assertion rather than a Codec method, because framing is not a thing every codec can be
	// asked to do badly. Adding FrameSupported() to the Codec interface would put a method on gzip and
	// lz4 whose only correct implementation returns false forever.
	zstdCodec, ok := c.codec.(*ZstdCodec)
	if !ok {
		return nil, SeekableDescriptor{}, false, nil
	}

	size := int64(len(data))
	frameSize := DeriveFrameSize(size, zstdCodec.EstimateRatio(data))
	if size <= frameSize {
		return nil, SeekableDescriptor{}, false, nil
	}

	framedBody, idx, err := zstdCodec.CompressFramed(data, frameSize, contentSHA256)
	if err != nil {
		return nil, SeekableDescriptor{}, false, fmt.Errorf("frame: %w", err)
	}

	if int64(len(framedBody)) >= size {
		return nil, SeekableDescriptor{}, false, nil
	}

	// The index length is read off the object rather than recomputed, so the descriptor records what
	// was actually written. CompressFramed has already asserted that the two agree.
	indexLength := idx.Frames[0].CompressedOffset

	return framedBody, DescribeFrameIndex(idx, indexLength), true, nil
}

// Decompress decodes data according to contentEncoding, which is the object's own
// Content-Encoding — not the algorithm this Compressor writes with.
//
// The second return reports whether a codec was found and applied. False with a nil error means the
// data came back untouched, either because contentEncoding was empty or because no codec here claims
// that token; the caller decides what that means, since only the caller can see whether the object
// is one ObjectFS compressed. Passing a foreign encoding through is right — an object another tool
// wrote with `Content-Encoding: br` is that tool's format, and `aws s3 cp` hands back the same
// bytes — while an ObjectFS-compressed object arriving undecoded is an integrity failure, which is
// what `checkFullyDecoded` in the S3 backend exists to catch.
//
// Dispatching on the object rather than on the configuration is the whole point. This used to
// compare contentEncoding against the single configured codec's token and return the data unchanged
// on any mismatch (audit finding C2), which meant a mount could read back only what it was currently
// configured to write: switching `algorithm: zstd` to `lz4` — or to `enabled: false` — made every
// existing compressed object unreadable, with the code to read them linked into the same binary.
func (c *Compressor) Decompress(data []byte, contentEncoding string) ([]byte, bool, error) {
	if contentEncoding == "" {
		return data, false, nil
	}

	codec, ok := c.decoders[contentEncoding]
	if !ok {
		return data, false, nil
	}

	decompressed, err := codec.Decompress(data)
	if err != nil {
		return nil, false, fmt.Errorf("decompress %s: %w", contentEncoding, err)
	}

	return decompressed, true, nil
}

// FrameDecoder returns the codec able to decode individual frames of an object stored with
// contentEncoding, or nil when there is none.
//
// nil is the ordinary answer, not a failure: only zstd has a skippable frame, so a gzip or lz4
// object — or one written by another tool entirely — has no frames to decode and must be read whole.
// The caller's response to nil is to take the whole-object path, which is always correct.
//
// Keyed on the object's own encoding rather than on this Compressor's write codec, for the reason
// [Compressor.Decompress] gives: a bucket accumulates objects across configuration changes, and a
// mount currently writing gzip must still be able to seek within the zstd objects it wrote last week.
// The [Compressor.CompressFramed] version of this is a type assertion on c.codec because the write
// side genuinely does depend on the configured codec; the read side must not.
func (c *Compressor) FrameDecoder(contentEncoding string) *ZstdCodec {
	if zstdCodec, ok := c.decoders[contentEncoding].(*ZstdCodec); ok {
		return zstdCodec
	}

	return nil
}

// DecodeFrames decodes a contiguous run of frames and returns their concatenated content.
//
// body must hold precisely the stored bytes from frames[0].CompressedOffset through the end of the
// last frame, which is one Range because [ZstdCodec.CompressFramed] lays frames down contiguously.
// A caller that fetched a wider range must trim it before calling: the per-frame slicing here is
// relative to frames[0].CompressedOffset, so slack at the front shifts every frame, and
// [ZstdCodec.DecompressFrame] would reject the resulting slices against their recorded hashes rather
// than return a neighbour's content.
//
// The returned buffer starts at frames[0].UncompressedOffset, not at the offset the caller asked
// for. Slicing to the request is [FrameIndex.FramesCovering]'s second return value and is left to
// the caller, because that is the value the caller already has to hold on to.
func (c *Compressor) DecodeFrames(contentEncoding string, frames []Frame, body []byte) ([]byte, error) {
	codec := c.FrameDecoder(contentEncoding)
	if codec == nil {
		return nil, fmt.Errorf("no frame decoder for content-encoding %q", contentEncoding)
	}
	if len(frames) == 0 {
		return nil, errors.New("no frames to decode")
	}

	base := frames[0].CompressedOffset

	// Sized from the index rather than grown, and the total is checked against the fetched body first
	// so that a hostile or corrupt index cannot make this allocation arbitrarily large before any
	// frame's hash has been consulted.
	var wantCompressed, wantDecoded int64
	for _, f := range frames {
		wantCompressed += f.CompressedSize
		wantDecoded += f.UncompressedSize
	}

	if int64(len(body)) != wantCompressed {
		return nil, fmt.Errorf("%w: %d frames from offset %d need %d stored bytes, have %d",
			ErrIndexCorrupt, len(frames), base, wantCompressed, len(body))
	}

	out := make([]byte, 0, wantDecoded)
	for _, f := range frames {
		start := f.CompressedOffset - base
		decoded, err := codec.DecompressFrame(body[start:start+f.CompressedSize], f)
		if err != nil {
			return nil, err
		}

		out = append(out, decoded...)
	}

	return out, nil
}

// DecodableEncodings lists the Content-Encoding tokens this Compressor can decode, sorted.
//
// For error messages and for logging: an object that cannot be decoded is worth reporting alongside
// what could have been, since the two most likely explanations — a foreign tool's encoding and an
// ObjectFS object whose header was mangled — are told apart by exactly that comparison.
func (c *Compressor) DecodableEncodings() []string {
	tokens := make([]string, 0, len(c.decoders))
	for token := range c.decoders {
		tokens = append(tokens, token)
	}

	sort.Strings(tokens)

	return tokens
}

// ContentEncoding returns the HTTP Content-Encoding token set on compressed
// objects (e.g. "zstd").  Returns "" when compression is disabled.
func (c *Compressor) ContentEncoding() string {
	return c.codec.ContentEncoding()
}

// Algorithm returns the algorithm in use.
func (c *Compressor) Algorithm() comprpkg.Algorithm {
	return c.codec.Algorithm()
}

// Stats returns a Stats snapshot for a compression outcome.  Callers that
// want to record metrics should call this after Compress.
func (c *Compressor) Stats(original, compressed int64) comprpkg.Stats {
	return comprpkg.Stats{
		Algorithm:      c.codec.Algorithm(),
		OriginalSize:   original,
		CompressedSize: compressed,
	}
}

// The parseSize that used to be here is gone; [utils.ParseOptionalBytes] is called directly from
// NewCompressor (#159). It was one of four size parsers, and it accepted three things a compression
// floor must not be:
//
//   - "1TB" and "1PB" as *errors*, because its unit table stopped at GB — so a size larger than the
//     ones it knew was rejected while smaller malformed ones were accepted;
//   - "-1MB" as -1048576, a negative minimum, which makes `len(data) < c.minSize` true for nothing and
//     silently compresses every object including the ones below the floor an operator set;
//   - "99999999999GB" as math.MaxInt64, because the multiply overflowed unchecked — a floor no object
//     can reach, which is compression silently off while the configuration says it is on.
//
// The last two are the same defect in opposite directions and neither reports anything, which is the
// argument for one parser rather than four: this one was the *safe* copy, in that it returned an error
// at all.
