// Package compression provides concrete codec implementations for ObjectFS
// transparent S3 compression.  The public types and the Codec interface are
// defined in pkg/compression; this package contains the implementations.
package compression

import (
	"fmt"

	comprpkg "github.com/objectfs/objectfs/pkg/compression"
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
// Supported algorithms: "none" / "", "zstd".
func New(algo comprpkg.Algorithm, level int) (comprpkg.Codec, error) {
	switch algo {
	case comprpkg.AlgorithmNone, "":
		return &nopCodec{}, nil
	case comprpkg.AlgorithmZstd:
		return NewZstdCodec(level)
	default:
		return nil, fmt.Errorf("unsupported compression algorithm %q", algo)
	}
}
