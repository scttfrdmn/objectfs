package compression

import (
	"bytes"
	"compress/gzip"
	"testing"
)

// ── Analyze ────────────────────────────────────────────────────────────────

func TestAnalyze_Empty(t *testing.T) {
	t.Parallel()
	a := Analyze(nil)
	if a.ContentClass != ContentClassUnknown {
		t.Errorf("ContentClass = %q, want %q", a.ContentClass, ContentClassUnknown)
	}
	if a.SampleSize != 0 {
		t.Errorf("SampleSize = %d, want 0", a.SampleSize)
	}
}

func TestAnalyze_TextContent(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 50)
	a := Analyze(data)
	if a.ContentClass != ContentClassText {
		t.Errorf("ContentClass = %q, want %q", a.ContentClass, ContentClassText)
	}
	if a.CompressScore < 0.3 {
		t.Errorf("CompressScore = %.2f, want >= 0.3 for text", a.CompressScore)
	}
}

func TestAnalyze_JSONContent(t *testing.T) {
	t.Parallel()
	data := []byte(`{"name":"Alice","age":30,"scores":[1,2,3]}`)
	a := Analyze(data)
	if a.ContentClass != ContentClassJSON {
		t.Errorf("ContentClass = %q, want %q", a.ContentClass, ContentClassJSON)
	}
}

func TestAnalyze_GzipCompressed(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(bytes.Repeat([]byte("data"), 200))
	_ = w.Close()

	a := Analyze(buf.Bytes())
	if a.ContentClass != ContentClassCompressed {
		t.Errorf("ContentClass = %q, want %q", a.ContentClass, ContentClassCompressed)
	}
	if a.CompressScore > 0.1 {
		t.Errorf("CompressScore = %.2f, want near 0 for gzip data", a.CompressScore)
	}
}

func TestAnalyze_ZstdMagic(t *testing.T) {
	t.Parallel()
	// zstd magic bytes: 0xFD2FB528 (little-endian)
	data := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x04, 0x00, 0x00}
	a := Analyze(data)
	if a.ContentClass != ContentClassCompressed {
		t.Errorf("ContentClass = %q, want compressed", a.ContentClass)
	}
}

func TestAnalyze_PNGMagic(t *testing.T) {
	t.Parallel()
	data := []byte("\x89PNG\r\n\x1a\nsome png data follows here")
	a := Analyze(data)
	if a.ContentClass != ContentClassCompressed {
		t.Errorf("ContentClass = %q, want compressed", a.ContentClass)
	}
}

func TestAnalyze_JPEGMagic(t *testing.T) {
	t.Parallel()
	data := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}
	a := Analyze(data)
	if a.ContentClass != ContentClassCompressed {
		t.Errorf("ContentClass = %q, want compressed", a.ContentClass)
	}
}

func TestAnalyze_ZIPMagic(t *testing.T) {
	t.Parallel()
	data := []byte("PK\x03\x04some zip content here")
	a := Analyze(data)
	if a.ContentClass != ContentClassCompressed {
		t.Errorf("ContentClass = %q, want compressed", a.ContentClass)
	}
}

func TestAnalyze_TarArchive(t *testing.T) {
	t.Parallel()
	// TAR POSIX: "ustar" at offset 257.
	data := make([]byte, 512)
	copy(data[257:], "ustar")
	a := Analyze(data)
	if a.ContentClass != ContentClassArchive {
		t.Errorf("ContentClass = %q, want archive", a.ContentClass)
	}
}

func TestAnalyze_SampleCapped(t *testing.T) {
	t.Parallel()
	// 16 KiB of text — only 4 KiB should be sampled.
	data := bytes.Repeat([]byte("x"), 16*1024)
	a := Analyze(data)
	if a.SampleSize > analysisSampleSize {
		t.Errorf("SampleSize = %d, want <= %d", a.SampleSize, analysisSampleSize)
	}
}

// ── shannonEntropy ────────────────────────────────────────────────────────

func TestShannonEntropy_AllSame(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte{0xAA}, 256)
	h := shannonEntropy(data)
	if h != 0.0 {
		t.Errorf("entropy(all same) = %.4f, want 0.0", h)
	}
}

func TestShannonEntropy_Uniform(t *testing.T) {
	t.Parallel()
	// All 256 byte values appear exactly once → entropy = 8.
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	h := shannonEntropy(data)
	if h < 7.99 || h > 8.01 {
		t.Errorf("entropy(uniform) = %.4f, want ~8.0", h)
	}
}

func TestShannonEntropy_Empty(t *testing.T) {
	t.Parallel()
	if h := shannonEntropy(nil); h != 0 {
		t.Errorf("entropy(nil) = %v, want 0", h)
	}
}

// ── compressScore ─────────────────────────────────────────────────────────

func TestCompressScore_Range(t *testing.T) {
	t.Parallel()
	for _, entropy := range []float64{0, 2, 4, 6, 8, 9} {
		s := compressScore(entropy)
		if s < 0 || s > 1 {
			t.Errorf("compressScore(%v) = %.4f, want in [0,1]", entropy, s)
		}
	}
}

func TestCompressScore_Monotone(t *testing.T) {
	t.Parallel()
	prev := compressScore(0.0)
	for e := 1.0; e <= 8.0; e++ {
		curr := compressScore(e)
		if curr > prev {
			t.Errorf("compressScore not monotone decreasing: score(%v)=%.4f > score(%v)=%.4f",
				e, curr, e-1, prev)
		}
		prev = curr
	}
}
