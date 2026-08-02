package compression

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

// GzipCodec implements Codec using the standard library's compress/gzip.
//
// gzip is slower and compresses worse than zstd at every level, so it is not the recommended
// algorithm. It exists for one reason the others cannot offer: "gzip" is a registered HTTP content
// coding, so an object ObjectFS stores this way is decoded by browsers, curl, and any HTTP client
// that sends Accept-Encoding — whereas a zstd or lz4 object read with `aws s3 cp` or boto3 lands on
// disk as an opaque compressed frame. Choosing gzip trades throughput for the property that the data
// is still reachable without ObjectFS.
//
// Both Compress and Decompress are safe for concurrent use: each call allocates its own
// Reader/Writer rather than sharing one.
type GzipCodec struct {
	level int
}

// mapGzipLevel converts a configured level to a compress/gzip level.
//
// The ranges differ between codecs — zstd accepts 0–22 — so a level that is valid for zstd is
// frequently not valid here. Rejecting out-of-range values rather than clamping them is deliberate:
// a config asking for gzip level 22 is a config written against the wrong algorithm, and silently
// substituting 9 would hide that from whoever has to explain the resulting throughput.
//
//	0 → gzip.DefaultCompression (6)
//	1 → best speed
//	9 → best compression
func mapGzipLevel(level int) (int, error) {
	switch {
	case level == 0:
		return gzip.DefaultCompression, nil
	case level >= 1 && level <= 9:
		return level, nil
	default:
		return 0, fmt.Errorf("gzip level %d out of range [0, 9] (0 selects the default, 6); "+
			"note zstd accepts 0-22, so this value may have been written for a different algorithm",
			level)
	}
}

// NewGzipCodec creates a GzipCodec at the given compression level, rejecting a level gzip cannot
// use. Pass 0 (or comprpkg.DefaultLevel) for the library default (level 6).
//
// mapGzipLevel is the only validation, deliberately: adding a second check here — constructing a
// throwaway writer to see whether it errors — would be a redundant authority on the same question,
// which is the shape of the defect this codec was written to close.
func NewGzipCodec(level int) (*GzipCodec, error) {
	mapped, err := mapGzipLevel(level)
	if err != nil {
		return nil, err
	}

	return &GzipCodec{level: mapped}, nil
}

// Compress compresses src using the gzip format.
func (c *GzipCodec) Compress(src []byte) ([]byte, error) {
	var buf bytes.Buffer

	buf.Grow(len(src) / 2)

	w, err := gzip.NewWriterLevel(&buf, c.level)
	if err != nil {
		return nil, fmt.Errorf("gzip writer at level %d: %w", c.level, err)
	}

	if _, err := w.Write(src); err != nil {
		return nil, fmt.Errorf("gzip compress: %w", err)
	}

	// Close flushes the final block and writes the trailer, so its error is a compression failure
	// and not a cleanup detail. A gzip stream missing its trailer fails CRC verification on read.
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("gzip compress close: %w", err)
	}

	return buf.Bytes(), nil
}

// Decompress decompresses a gzip stream.
func (c *GzipCodec) Decompress(src []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("gzip decompress: %w", err)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		// Truncated input and a CRC mismatch both surface here rather than from NewReader, which
		// only reads the header. Returning the error rather than the partial data is the point:
		// gzip's trailer is the only integrity check the format carries.
		return nil, fmt.Errorf("gzip decompress: %w", err)
	}

	if err := r.Close(); err != nil {
		return nil, fmt.Errorf("gzip decompress close: %w", err)
	}

	return data, nil
}

// Algorithm returns the algorithm identifier.
func (c *GzipCodec) Algorithm() comprpkg.Algorithm { return comprpkg.AlgorithmGzip }

// ContentEncoding returns the HTTP Content-Encoding token for gzip objects.
func (c *GzipCodec) ContentEncoding() string { return "gzip" }
