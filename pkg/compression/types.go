// Package compression defines the public types and interfaces for ObjectFS
// transparent compression.  Concrete implementations live in
// internal/compression.
package compression

import (
	"strings"
	"time"
)

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

// SupportedAlgorithms lists every algorithm a codec exists for, in the order they should be
// presented to a user.
//
// This is the single authority on what is valid, and it is here — beside the constants — so that
// declaring a constant and forgetting the implementation cannot go unnoticed. v0.10.0 shipped with
// AlgorithmGzip declared, documented in two config files, and set as the default write-buffer
// algorithm, with no gzip codec anywhere: config validation, the YAML schema, and the S3 config
// comment all treated it as valid, and the only code that knew better was the codec factory, reached
// after the user had already asked for a mount.
//
// This list is for enumeration — error messages, documentation, and the test that round-trips every
// entry through its codec. It is deliberately not a validator: a layer checking a user's compression
// configuration should build the codec instead, which is the only check that cannot go stale and the
// only one that catches a level out of range for the chosen algorithm (zstd accepts 0-22, gzip only
// 0-9). A name-matching validator here would be a second authority on the same question, free to
// drift exactly as the first one did.
//
// AlgorithmNone is included: disabling compression is a valid choice, not an absent one.
func SupportedAlgorithms() []Algorithm {
	return []Algorithm{AlgorithmNone, AlgorithmZstd, AlgorithmLZ4, AlgorithmGzip}
}

// SupportedAlgorithmNames renders SupportedAlgorithms for an error message.
func SupportedAlgorithmNames() string {
	names := make([]string, 0, len(SupportedAlgorithms()))
	for _, a := range SupportedAlgorithms() {
		names = append(names, string(a))
	}

	return strings.Join(names, ", ")
}

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
