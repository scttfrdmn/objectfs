package compression

import (
	"fmt"
	"strconv"
	"strings"

	comprpkg "github.com/objectfs/objectfs/pkg/compression"
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
type Compressor struct {
	codec   comprpkg.Codec
	minSize int64
}

// NewCompressor builds a Compressor from Settings.
// When cfg.Enabled is false a nop compressor is returned (no-overhead
// pass-through).
//
// Calling this is also how a caller validates a compression configuration: it is the only check that
// cannot drift from what the codecs actually support, because it is the code that builds them.
func NewCompressor(cfg Settings) (*Compressor, error) {
	if !cfg.Enabled {
		return &Compressor{codec: &nopCodec{}, minSize: 0}, nil
	}

	codec, err := New(comprpkg.Algorithm(cfg.Algorithm), cfg.Level)
	if err != nil {
		return nil, fmt.Errorf("create %s codec: %w", cfg.Algorithm, err)
	}

	minSize, err := parseSize(cfg.MinSize)
	if err != nil {
		return nil, fmt.Errorf("invalid min_size %q: %w", cfg.MinSize, err)
	}

	return &Compressor{codec: codec, minSize: minSize}, nil
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

// Decompress decompresses data when contentEncoding matches the codec's
// Content-Encoding token.  Returns data unchanged when contentEncoding is
// empty or does not match.
func (c *Compressor) Decompress(data []byte, contentEncoding string) ([]byte, error) {
	if contentEncoding == "" || contentEncoding != c.codec.ContentEncoding() {
		return data, nil
	}

	decompressed, err := c.codec.Decompress(data)
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	return decompressed, nil
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
