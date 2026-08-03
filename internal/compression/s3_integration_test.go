package compression

import (
	"bytes"
	"testing"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
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

	got, decoded, err := c.Decompress(compressed, "zstd")
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !decoded {
		t.Error("Decompress reported it did not decode an object encoded with its own algorithm")
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
	got, decoded, err := c.Decompress(data, "")
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if decoded {
		t.Error("Decompress claimed to decode an object with no Content-Encoding")
	}
	if !bytes.Equal(got, data) {
		t.Error("Decompress with no encoding should return data unchanged")
	}
}

// TestCompressor_DecompressUnknownEncoding covers a token no codec in this build claims.
//
// Passing the bytes through is deliberate: an object another tool wrote with `Content-Encoding: br`
// is that tool's format, and returning it unchanged is what every other S3 client does. What must
// not happen is a silent pass-through that the caller cannot detect — hence the second return, which
// is how the S3 backend knows to cross-check against objectfs-original-size and fail closed for an
// object ObjectFS itself compressed.
func TestCompressor_DecompressUnknownEncoding(t *testing.T) {
	t.Parallel()
	c, err := NewCompressor(makeConfig(true, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	data := []byte("brotli encoded data (pretend)")
	got, decoded, err := c.Decompress(data, "br")
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if decoded {
		t.Error("Decompress claimed to decode \"br\", which no codec in this build implements")
	}
	if !bytes.Equal(got, data) {
		t.Error("an encoding this build does not implement should pass through unchanged")
	}
}

// TestMinSizeIsParsedByTheSharedParser replaces TestParseSize, which tested a copy of a parser this
// package no longer has (#159).
//
// The subject is the floor that reaches the Compressor, not the parser — [utils.ParseBytes] has its own
// table test and fuzz target in pkg/utils, and duplicating them here is what let the deleted copy
// disagree with the real parser for a release without failing anything. What is asserted here is the
// seam: that NewCompressor's min_size goes through the shared parser, so the cases the local copy got
// wrong are now impossible.
//
// Three of the rows below are those cases, and each is a floor that silently disables or silently
// bypasses compression rather than reporting anything:
//
//   - "1TB" was an *error*, because the local unit table stopped at GB. A size too large to be a
//     sensible floor was rejected while smaller malformed strings were accepted.
//   - "-1MB" parsed to a negative floor, which no object is below, so every object was compressed
//     including the ones an operator set the floor to exclude.
//   - "99999999999GB" overflowed to math.MaxInt64, a floor no object can reach — compression off while
//     the configuration says it is on.
func TestMinSizeIsParsedByTheSharedParser(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		minSize string
		want    int64
		wantErr bool
		why     string
	}{
		{name: "empty means no floor", minSize: "", want: 0,
			why: "an unset min_size is the common case and must not be an error"},
		{name: "zero means no floor", minSize: "0", want: 0,
			why: "\"0\" and \"\" agree, which is what utils.ParseOptionalBytes exists to guarantee"},
		{name: "bare number is bytes", minSize: "512", want: 512},
		{name: "kilobytes", minSize: "4KB", want: 4096},
		{name: "megabytes", minSize: "1MB", want: 1024 * 1024},
		{name: "fractional", minSize: "1.5MB", want: int64(1.5 * 1024 * 1024)},
		{name: "terabytes are a unit", minSize: "1TB", want: 1 << 40,
			why: "the deleted copy's unit table stopped at GB, so this was an error"},
		{name: "negative is rejected", minSize: "-1MB", wantErr: true,
			why: "the deleted copy returned -1048576, a floor below every object, so the floor was " +
				"silently ignored and everything was compressed"},
		{name: "overflow is rejected", minSize: "99999999999GB", wantErr: true,
			why: "the deleted copy overflowed to math.MaxInt64, a floor no object reaches, so " +
				"compression was silently off"},
		{name: "malformed is rejected", minSize: "not-a-size", wantErr: true},
		{name: "unknown unit is rejected", minSize: "1ZB", wantErr: true},
		{name: "trailing garbage is rejected", minSize: "4KiB", wantErr: true,
			why: "the spelling someone who knows the units writes; accepting it as 4 bytes is worse " +
				"than refusing it"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, err := NewCompressor(makeConfig(true, "zstd", tt.minSize, 0))
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewCompressor(min_size: %q): err=%v, wantErr=%v. %s",
					tt.minSize, err, tt.wantErr, tt.why)
			}
			if tt.wantErr {
				return
			}
			if c.minSize != tt.want {
				t.Errorf("min_size %q became a floor of %d bytes, want %d. %s",
					tt.minSize, c.minSize, tt.want, tt.why)
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
