package compression

import (
	"bytes"
	"testing"

	comprpkg "github.com/objectfs/objectfs/pkg/compression"
)

func makeConfig(enabled bool, algo, minSize string, level int) Settings {
	return Settings{
		Enabled:   enabled,
		Algorithm: algo,
		MinSize:   minSize,
		Level:     level,
	}
}

func TestNewCompressor_Disabled(t *testing.T) {
	t.Parallel()
	c, err := NewCompressor(makeConfig(false, "zstd", "1KB", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	if c.Enabled() {
		t.Error("Enabled() should be false when cfg.Enabled = false")
	}
	if c.ContentEncoding() != "" {
		t.Errorf("ContentEncoding() = %q, want empty", c.ContentEncoding())
	}
}

func TestNewCompressor_InvalidAlgo(t *testing.T) {
	t.Parallel()
	_, err := NewCompressor(makeConfig(true, "bogus", "1KB", 0))
	if err == nil {
		t.Error("expected error for unknown algorithm")
	}
}

func TestNewCompressor_InvalidMinSize(t *testing.T) {
	t.Parallel()
	_, err := NewCompressor(makeConfig(true, "zstd", "not-a-size", 0))
	if err == nil {
		t.Error("expected error for invalid min_size")
	}
}

func TestCompressor_Enabled(t *testing.T) {
	t.Parallel()
	c, err := NewCompressor(makeConfig(true, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	if !c.Enabled() {
		t.Error("Enabled() should be true when cfg.Enabled = true")
	}
	if c.ContentEncoding() != "zstd" {
		t.Errorf("ContentEncoding() = %q, want %q", c.ContentEncoding(), "zstd")
	}
	if c.Algorithm() != comprpkg.AlgorithmZstd {
		t.Errorf("Algorithm() = %q, want %q", c.Algorithm(), comprpkg.AlgorithmZstd)
	}
}

func TestCompressor_BelowMinSize(t *testing.T) {
	t.Parallel()
	c, err := NewCompressor(makeConfig(true, "zstd", "1MB", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	small := []byte("tiny payload")
	got, wasCompressed, err := c.Compress(small)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if wasCompressed {
		t.Error("wasCompressed should be false for data below minSize")
	}
	if !bytes.Equal(got, small) {
		t.Error("Compress should return original data unchanged below minSize")
	}
}

func TestCompressor_AboveMinSize(t *testing.T) {
	t.Parallel()
	c, err := NewCompressor(makeConfig(true, "zstd", "1KB", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	// Highly compressible data above minSize.
	large := bytes.Repeat([]byte("objectfs zstd compression test data\n"), 200)
	got, wasCompressed, err := c.Compress(large)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if !wasCompressed {
		t.Error("wasCompressed should be true for compressible data above minSize")
	}
	if len(got) >= len(large) {
		t.Errorf("compressed size (%d) >= original size (%d)", len(got), len(large))
	}
}

func TestCompressor_Incompressible(t *testing.T) {
	t.Parallel()
	c, err := NewCompressor(makeConfig(true, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	// Incompressible: pseudo-random bytes.
	noisy := make([]byte, 512)
	for i := range noisy {
		noisy[i] = byte(i*7 + 3)
	}

	got, wasCompressed, err := c.Compress(noisy)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	// Incompressible data may not compress; wasCompressed could be false.
	if wasCompressed && len(got) >= len(noisy) {
		t.Error("if wasCompressed, compressed must be smaller than original")
	}
	if !wasCompressed && !bytes.Equal(got, noisy) {
		t.Error("when not compressed, original data should be returned")
	}
}

func TestCompressor_DecompressMatchingEncoding(t *testing.T) {
	t.Parallel()
	c, err := NewCompressor(makeConfig(true, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	original := bytes.Repeat([]byte("decompress test\n"), 100)
	compressed, wasCompressed, err := c.Compress(original)
	if err != nil || !wasCompressed {
		t.Fatalf("Compress: err=%v, wasCompressed=%v", err, wasCompressed)
	}

	got, err := c.Decompress(compressed, "zstd")
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("roundtrip mismatch: got len %d, want len %d", len(got), len(original))
	}
}

func TestCompressor_DecompressNoEncoding(t *testing.T) {
	t.Parallel()
	c, err := NewCompressor(makeConfig(true, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	data := []byte("not compressed")
	got, err := c.Decompress(data, "")
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("Decompress with no encoding should return data unchanged")
	}
}

func TestCompressor_DecompressMismatchEncoding(t *testing.T) {
	t.Parallel()
	c, err := NewCompressor(makeConfig(true, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	data := []byte("gzip encoded data (pretend)")
	got, err := c.Decompress(data, "gzip")
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	// encoding doesn't match our codec → pass through unchanged
	if !bytes.Equal(got, data) {
		t.Error("Decompress with mismatched encoding should return data unchanged")
	}
}

func TestParseSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"1", 1, false},
		{"512", 512, false},
		{"1KB", 1024, false},
		{"4KB", 4096, false},
		{"1MB", 1024 * 1024, false},
		{"2GB", 2 * 1024 * 1024 * 1024, false},
		{"1.5MB", int64(1.5 * 1024 * 1024), false},
		{"not-a-size", 0, true},
		{"1ZB", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got, err := parseSize(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseSize(%q): err=%v, wantErr=%v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestCompressor_Stats(t *testing.T) {
	t.Parallel()
	c, err := NewCompressor(makeConfig(true, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	s := c.Stats(1000, 400)
	if s.Algorithm != comprpkg.AlgorithmZstd {
		t.Errorf("Stats.Algorithm = %q, want %q", s.Algorithm, comprpkg.AlgorithmZstd)
	}
	if s.OriginalSize != 1000 {
		t.Errorf("Stats.OriginalSize = %d, want 1000", s.OriginalSize)
	}
	if s.CompressedSize != 400 {
		t.Errorf("Stats.CompressedSize = %d, want 400", s.CompressedSize)
	}
	ratio := s.Ratio()
	if ratio < 0 || ratio > 1 {
		t.Errorf("Stats.Ratio() = %f, expected 0 < ratio <= 1 for 1000→400", ratio)
	}
	if s.SavedBytes() != 600 {
		t.Errorf("Stats.SavedBytes() = %d, want 600", s.SavedBytes())
	}
}
