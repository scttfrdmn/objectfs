package compression

import (
	"bytes"
	"testing"

	comprpkg "github.com/objectfs/objectfs/pkg/compression"
)

func TestNewZstdCodec_DefaultLevel(t *testing.T) {
	t.Parallel()
	c, err := NewZstdCodec(0)
	if err != nil {
		t.Fatalf("NewZstdCodec(0) error = %v", err)
	}
	if c.Algorithm() != comprpkg.AlgorithmZstd {
		t.Errorf("Algorithm() = %q, want %q", c.Algorithm(), comprpkg.AlgorithmZstd)
	}
	if c.ContentEncoding() != "zstd" {
		t.Errorf("ContentEncoding() = %q, want %q", c.ContentEncoding(), "zstd")
	}
}

func TestNewZstdCodec_InvalidLevel(t *testing.T) {
	t.Parallel()
	if _, err := NewZstdCodec(-1); err == nil {
		t.Error("expected error for level -1")
	}
	if _, err := NewZstdCodec(23); err == nil {
		t.Error("expected error for level 23")
	}
}

func TestZstdRoundtrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input []byte
	}{
		{"empty", []byte{}},
		{"single byte", []byte{0x42}},
		{"short string", []byte("hello, world")},
		{"repetitive text", bytes.Repeat([]byte("objectfs-compression-test\n"), 1000)},
		{"binary-like", func() []byte {
			b := make([]byte, 256)
			for i := range b {
				b[i] = byte(i)
			}
			return b
		}()},
	}

	c, err := NewZstdCodec(0)
	if err != nil {
		t.Fatalf("NewZstdCodec: %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			compressed, err := c.Compress(tt.input)
			if err != nil {
				t.Fatalf("Compress: %v", err)
			}
			got, err := c.Decompress(compressed)
			if err != nil {
				t.Fatalf("Decompress: %v", err)
			}
			if !bytes.Equal(got, tt.input) {
				t.Errorf("roundtrip mismatch: got len %d, want len %d", len(got), len(tt.input))
			}
		})
	}
}

func TestZstdLevels(t *testing.T) {
	t.Parallel()
	levels := []int{0, 1, 2, 5, 10, 15, 22}
	input := bytes.Repeat([]byte("level test data for objectfs\n"), 500)

	for _, lvl := range levels {
		t.Run(string(rune('0'+lvl%10)), func(t *testing.T) {
			t.Parallel()
			c, err := NewZstdCodec(lvl)
			if err != nil {
				t.Fatalf("NewZstdCodec(%d): %v", lvl, err)
			}
			compressed, err := c.Compress(input)
			if err != nil {
				t.Fatalf("Compress level %d: %v", lvl, err)
			}
			got, err := c.Decompress(compressed)
			if err != nil {
				t.Fatalf("Decompress level %d: %v", lvl, err)
			}
			if !bytes.Equal(got, input) {
				t.Errorf("level %d: roundtrip mismatch", lvl)
			}
		})
	}
}

func TestZstdConcurrency(t *testing.T) {
	t.Parallel()
	c, err := NewZstdCodec(0)
	if err != nil {
		t.Fatalf("NewZstdCodec: %v", err)
	}

	input := bytes.Repeat([]byte("concurrent compression test\n"), 200)
	const workers = 20

	errCh := make(chan error, workers)
	for range workers {
		go func() {
			compressed, err := c.Compress(input)
			if err != nil {
				errCh <- err
				return
			}
			got, err := c.Decompress(compressed)
			if err != nil {
				errCh <- err
				return
			}
			if !bytes.Equal(got, input) {
				errCh <- bytes.ErrTooLarge // sentinel: data mismatch
				return
			}
			errCh <- nil
		}()
	}

	for range workers {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent worker error: %v", err)
		}
	}
}

func TestZstdIncompressible(t *testing.T) {
	t.Parallel()
	// Random-looking data compresses poorly; round-trip must still work.
	input := make([]byte, 1024)
	for i := range input {
		input[i] = byte(i*7 + 13)
	}

	c, err := NewZstdCodec(0)
	if err != nil {
		t.Fatalf("NewZstdCodec: %v", err)
	}

	compressed, err := c.Compress(input)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	got, err := c.Decompress(compressed)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Error("incompressible data roundtrip mismatch")
	}
}

func TestMapZstdLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		level   int
		wantErr bool
	}{
		{-1, true},
		{0, false},
		{1, false},
		{3, false},
		{7, false},
		{11, false},
		{22, false},
		{23, true},
	}
	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			_, err := mapZstdLevel(tt.level)
			if (err != nil) != tt.wantErr {
				t.Errorf("mapZstdLevel(%d): err=%v, wantErr=%v", tt.level, err, tt.wantErr)
			}
		})
	}
}
