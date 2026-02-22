package compression

import (
	"bytes"
	"testing"

	comprpkg "github.com/objectfs/objectfs/pkg/compression"
)

func TestLZ4Codec_RoundTrip(t *testing.T) {
	t.Parallel()
	c := NewLZ4Codec()
	original := bytes.Repeat([]byte("hello lz4 world "), 100)

	compressed, err := c.Compress(original)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if len(compressed) >= len(original) {
		t.Errorf("expected compression: compressed=%d original=%d", len(compressed), len(original))
	}

	recovered, err := c.Decompress(compressed)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(recovered, original) {
		t.Error("round-trip: recovered data does not match original")
	}
}

func TestLZ4Codec_Algorithm(t *testing.T) {
	t.Parallel()
	c := NewLZ4Codec()
	if c.Algorithm() != comprpkg.AlgorithmLZ4 {
		t.Errorf("Algorithm = %q, want %q", c.Algorithm(), comprpkg.AlgorithmLZ4)
	}
}

func TestLZ4Codec_ContentEncoding(t *testing.T) {
	t.Parallel()
	if got := NewLZ4Codec().ContentEncoding(); got != "lz4" {
		t.Errorf("ContentEncoding = %q, want lz4", got)
	}
}

func TestLZ4Codec_EmptyInput(t *testing.T) {
	t.Parallel()
	c := NewLZ4Codec()
	compressed, err := c.Compress([]byte{})
	if err != nil {
		t.Fatalf("Compress(empty): %v", err)
	}
	recovered, err := c.Decompress(compressed)
	if err != nil {
		t.Fatalf("Decompress(empty): %v", err)
	}
	if len(recovered) != 0 {
		t.Errorf("expected empty result, got %d bytes", len(recovered))
	}
}

func TestNew_LZ4(t *testing.T) {
	t.Parallel()
	codec, err := New(comprpkg.AlgorithmLZ4, 0)
	if err != nil {
		t.Fatalf("New(lz4): %v", err)
	}
	if codec.Algorithm() != comprpkg.AlgorithmLZ4 {
		t.Errorf("Algorithm = %q, want lz4", codec.Algorithm())
	}
}
