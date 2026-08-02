// Package compression provides concrete codec implementations for ObjectFS
// transparent S3 compression.  The public types and the Codec interface are
// defined in pkg/compression; this package contains the implementations.
package compression

import (
	"fmt"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

// nopCodec is a pass-through codec that performs no compression.
// It is used when compression is disabled.
type nopCodec struct{}

func (n *nopCodec) Compress(src []byte) ([]byte, error)   { return src, nil }
func (n *nopCodec) Decompress(src []byte) ([]byte, error) { return src, nil }
func (n *nopCodec) Algorithm() comprpkg.Algorithm         { return comprpkg.AlgorithmNone }
func (n *nopCodec) ContentEncoding() string               { return "" }

// New returns a Codec for the requested algorithm at the given level.
// Use level 0 (DefaultLevel) to select the algorithm's built-in default.
//
// The set of algorithms this function accepts is reported by
// pkg/compression.SupportedAlgorithms. Anything that validates an algorithm name must consult that
// list rather than keeping its own: config defaulted the algorithm to "gzip" while this factory had
// no gzip case, so every layer that read config believed the value was valid and only the factory
// disagreed — at which point the mount had already been attempted and the process exited with
// "Failed to start adapter". One list, one authority.
func New(algo comprpkg.Algorithm, level int) (comprpkg.Codec, error) {
	switch algo {
	case comprpkg.AlgorithmNone, "":
		return &nopCodec{}, nil
	case comprpkg.AlgorithmZstd:
		return NewZstdCodec(level)
	case comprpkg.AlgorithmLZ4:
		return NewLZ4Codec(), nil
	case comprpkg.AlgorithmGzip:
		return NewGzipCodec(level)
	default:
		return nil, fmt.Errorf("unsupported compression algorithm %q (supported: %s)",
			algo, comprpkg.SupportedAlgorithmNames())
	}
}
