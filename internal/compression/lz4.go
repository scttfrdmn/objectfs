package compression

import (
	"bytes"
	"fmt"
	"io"

	"github.com/pierrec/lz4/v4"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

// LZ4Codec implements Codec using the LZ4 frame format via
// github.com/pierrec/lz4/v4.  The frame format is self-delimiting and
// portable across LZ4 implementations.  Both Compress and Decompress are
// safe for concurrent use (each call allocates its own Reader/Writer).
type LZ4Codec struct{}

// NewLZ4Codec returns a new LZ4Codec.
func NewLZ4Codec() *LZ4Codec { return &LZ4Codec{} }

// Compress compresses src using LZ4 frame format.
func (c *LZ4Codec) Compress(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(src) / 2)
	w := lz4.NewWriter(&buf)
	if _, err := w.Write(src); err != nil {
		return nil, fmt.Errorf("lz4 compress: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("lz4 compress close: %w", err)
	}
	return buf.Bytes(), nil
}

// Decompress decompresses an LZ4 frame-format byte slice.
func (c *LZ4Codec) Decompress(src []byte) ([]byte, error) {
	r := lz4.NewReader(bytes.NewReader(src))
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("lz4 decompress: %w", err)
	}
	return data, nil
}

// Algorithm returns the algorithm identifier.
func (c *LZ4Codec) Algorithm() comprpkg.Algorithm { return comprpkg.AlgorithmLZ4 }

// ContentEncoding returns the Content-Encoding token for LZ4-compressed objects.
func (c *LZ4Codec) ContentEncoding() string { return "lz4" }
