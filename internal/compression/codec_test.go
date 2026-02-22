package compression

import (
	"testing"

	comprpkg "github.com/objectfs/objectfs/pkg/compression"
)

func TestNew_None(t *testing.T) {
	t.Parallel()
	for _, algo := range []comprpkg.Algorithm{comprpkg.AlgorithmNone, ""} {
		algo := algo
		t.Run(string(algo), func(t *testing.T) {
			t.Parallel()
			c, err := New(algo, 0)
			if err != nil {
				t.Fatalf("New(%q, 0) error = %v", algo, err)
			}
			if c.Algorithm() != comprpkg.AlgorithmNone {
				t.Errorf("Algorithm() = %q, want %q", c.Algorithm(), comprpkg.AlgorithmNone)
			}
			if c.ContentEncoding() != "" {
				t.Errorf("ContentEncoding() = %q, want %q", c.ContentEncoding(), "")
			}
		})
	}
}

func TestNew_Zstd(t *testing.T) {
	t.Parallel()
	c, err := New(comprpkg.AlgorithmZstd, 0)
	if err != nil {
		t.Fatalf("New(zstd, 0) error = %v", err)
	}
	if c.Algorithm() != comprpkg.AlgorithmZstd {
		t.Errorf("Algorithm() = %q, want %q", c.Algorithm(), comprpkg.AlgorithmZstd)
	}
}

func TestNew_InvalidAlgorithm(t *testing.T) {
	t.Parallel()
	_, err := New("bogus-algo", 0)
	if err == nil {
		t.Error("expected error for unknown algorithm")
	}
}

func TestNopCodec_Passthrough(t *testing.T) {
	t.Parallel()
	input := []byte("hello, objectfs")
	n := &nopCodec{}

	got, err := n.Compress(input)
	if err != nil {
		t.Fatalf("nopCodec.Compress: %v", err)
	}
	if string(got) != string(input) {
		t.Errorf("Compress changed data: got %q, want %q", got, input)
	}

	got2, err := n.Decompress(input)
	if err != nil {
		t.Fatalf("nopCodec.Decompress: %v", err)
	}
	if string(got2) != string(input) {
		t.Errorf("Decompress changed data: got %q, want %q", got2, input)
	}
}
