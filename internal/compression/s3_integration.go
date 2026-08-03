package compression

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
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

	minSize, err := parseSize(cfg.MinSize)
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

// Compress compresses data when compression is enabled and the data size
// meets the minimum threshold.  If the compressed form is larger than the
// original, the original is returned unchanged with wasCompressed == false.
//
// Returns: (data, wasCompressed, error)
func (c *Compressor) Compress(data []byte) ([]byte, bool, error) {
	if !c.Enabled() || int64(len(data)) < c.minSize {
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

// parseSize converts human-readable size strings (e.g. "4KB", "1MB") to bytes.
// Accepted suffixes (case-insensitive): B, KB, MB, GB.
// An empty or "0" string returns 0.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}

	upper := strings.ToUpper(s)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"GB", 1024 * 1024 * 1024},
		{"MB", 1024 * 1024},
		{"KB", 1024},
		{"B", 1},
	}

	for _, u := range units {
		if strings.HasSuffix(upper, u.suffix) {
			numStr := strings.TrimSpace(s[:len(s)-len(u.suffix)])
			n, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("cannot parse %q: %w", s, err)
			}
			return int64(n * float64(u.mult)), nil
		}
	}

	// Plain integer (bytes)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse %q as size", s)
	}
	return n, nil
}
