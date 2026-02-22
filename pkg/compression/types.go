// Package compression defines the public types and interfaces for ObjectFS
// transparent compression.  Concrete implementations live in
// internal/compression.
package compression

import "time"

// Algorithm identifies a compression algorithm.
type Algorithm string

const (
	// AlgorithmNone disables compression (pass-through).
	AlgorithmNone Algorithm = "none"
	// AlgorithmGzip selects gzip compression.
	AlgorithmGzip Algorithm = "gzip"
	// AlgorithmZstd selects Zstandard compression.
	AlgorithmZstd Algorithm = "zstd"
	// AlgorithmLZ4 selects LZ4 compression.
	AlgorithmLZ4 Algorithm = "lz4"
)

// DefaultLevel instructs the codec to use its built-in default level.
const DefaultLevel = 0

// Stats records the outcome of a single compress or decompress operation.
type Stats struct {
	Algorithm      Algorithm
	OriginalSize   int64
	CompressedSize int64
	Duration       time.Duration
}

// Ratio returns the compression ratio (< 1.0 means data shrank).
// Returns 1.0 when OriginalSize is zero.
func (s Stats) Ratio() float64 {
	if s.OriginalSize == 0 {
		return 1.0
	}
	return float64(s.CompressedSize) / float64(s.OriginalSize)
}

// SavedBytes returns the number of bytes saved by compression.
// Negative when the compressed form is larger than the original.
func (s Stats) SavedBytes() int64 {
	return s.OriginalSize - s.CompressedSize
}

// Codec compresses and decompresses byte slices.  Implementations must be
// safe for concurrent use by multiple goroutines.
type Codec interface {
	// Compress compresses src and returns the compressed data.
	Compress(src []byte) ([]byte, error)
	// Decompress decompresses src and returns the original data.
	Decompress(src []byte) ([]byte, error)
	// Algorithm returns the algorithm identifier for this codec.
	Algorithm() Algorithm
	// ContentEncoding returns the HTTP Content-Encoding token used when
	// storing compressed objects (e.g. "zstd", "gzip").  Returns "" for
	// the nop codec.
	ContentEncoding() string
}
