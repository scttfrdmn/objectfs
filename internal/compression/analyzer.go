package compression

import "math"

// ContentClass categorises object data for compression algorithm selection.
type ContentClass string

const (
	// ContentClassText covers plain text, CSV, TSV, XML, YAML, source code.
	// Highly compressible — prefer ZSTD for best ratio or LZ4 for speed.
	ContentClassText ContentClass = "text"

	// ContentClassJSON covers JSON documents (text with structural entropy).
	// Very compressible; distinguished from generic text for tuning purposes.
	ContentClassJSON ContentClass = "json"

	// ContentClassBinary covers generic binary data (executables, databases).
	// Moderately compressible; ZSTD default level works well.
	ContentClassBinary ContentClass = "binary"

	// ContentClassCompressed covers data already compressed at the byte level
	// (gzip, zstd, bzip2, lz4, xz, zip, PNG, JPEG, GIF, WebP, MP4, MKV, MP3).
	// Re-compression is wasteful — use AlgorithmNone.
	ContentClassCompressed ContentClass = "compressed"

	// ContentClassArchive covers uncompressed archive containers (tar, cpio).
	// May contain mixed content; treat as binary.
	ContentClassArchive ContentClass = "archive"

	// ContentClassUnknown is the fallback when no heuristic matches.
	ContentClassUnknown ContentClass = "unknown"
)

// Analysis holds the results of examining a sample of object data.
type Analysis struct {
	// ContentClass is the detected content category.
	ContentClass ContentClass

	// Entropy is the Shannon entropy of the sample in bits per byte [0, 8].
	// Low entropy (< 4) indicates repetitive / compressible content.
	Entropy float64

	// CompressScore estimates compressibility in [0, 1].
	// 1.0 means highly compressible; 0.0 means incompressible.
	CompressScore float64

	// SampleSize is the number of bytes examined.
	SampleSize int
}

// analysisSampleSize is the maximum number of bytes examined per object.
// 4 KiB gives sub-millisecond latency while remaining statistically reliable.
const analysisSampleSize = 4096

// Analyze examines up to analysisSampleSize bytes of data and returns an
// Analysis describing the content class and estimated compressibility.
// Passing nil or empty data returns a zero Analysis with ContentClassUnknown.
func Analyze(data []byte) Analysis {
	if len(data) == 0 {
		return Analysis{ContentClass: ContentClassUnknown}
	}

	sample := data
	if len(sample) > analysisSampleSize {
		sample = sample[:analysisSampleSize]
	}

	class := classifyByMagic(sample)
	entropy := shannonEntropy(sample)

	// For already-compressed content, entropy-based scoring is unreliable
	// (compressed bytes look random).  Override with near-zero score.
	if class == ContentClassCompressed {
		return Analysis{
			ContentClass:  class,
			Entropy:       entropy,
			CompressScore: 0.02,
			SampleSize:    len(sample),
		}
	}

	// Fall back to text/binary detection when magic bytes gave no match.
	if class == ContentClassUnknown {
		class = classifyByContent(sample)
	}

	score := compressScore(entropy)
	return Analysis{
		ContentClass:  class,
		Entropy:       entropy,
		CompressScore: score,
		SampleSize:    len(sample),
	}
}

// classifyByMagic detects content class from leading magic bytes.
func classifyByMagic(data []byte) ContentClass {
	if len(data) < 2 {
		return ContentClassUnknown
	}

	// Already-compressed formats.
	switch {
	case hasPrefix(data, "\x1f\x8b"): // gzip
		return ContentClassCompressed
	case hasPrefix(data, "\x28\xb5\x2f\xfd"): // zstd frame
		return ContentClassCompressed
	case hasPrefix(data, "BZh"): // bzip2
		return ContentClassCompressed
	case hasPrefix(data, "\x04\x22\x4d\x18"), // LZ4 frame (old magic)
		hasPrefix(data, "\x02\x21\x4c\x18"): // LZ4 frame (legacy)
		return ContentClassCompressed
	case hasPrefix(data, "\xfd7zXZ\x00"): // xz
		return ContentClassCompressed
	case hasPrefix(data, "PK\x03\x04"), hasPrefix(data, "PK\x05\x06"): // ZIP
		return ContentClassCompressed
	case hasPrefix(data, "\x89PNG\r\n\x1a\n"): // PNG
		return ContentClassCompressed
	case hasPrefix(data, "\xff\xd8\xff"): // JPEG
		return ContentClassCompressed
	case hasPrefix(data, "GIF8"): // GIF
		return ContentClassCompressed
	case len(data) >= 12 && hasPrefix(data[8:], "WEBP"): // WebP
		return ContentClassCompressed
	case hasPrefix(data, "\x1a\x45\xdf\xa3"): // Matroska/MKV
		return ContentClassCompressed
	case hasPrefix(data, "OggS"): // OGG (audio/video)
		return ContentClassCompressed
	case hasPrefix(data, "ID3"), hasPrefix(data, "\xff\xfb"), // MP3
		hasPrefix(data, "\xff\xf3"), hasPrefix(data, "\xff\xf2"):
		return ContentClassCompressed
	case len(data) >= 8 && string(data[4:8]) == "ftyp": // MP4/M4A/MOV
		return ContentClassCompressed

	// Uncompressed archive containers.
	case len(data) >= 262 && string(data[257:262]) == "ustar": // tar (POSIX)
		return ContentClassArchive
	case hasPrefix(data, "070701"), hasPrefix(data, "070702"): // cpio
		return ContentClassArchive

	// Structured text formats.
	case hasPrefix(data, "{"), hasPrefix(data, "[\n"), hasPrefix(data, "[ "):
		return ContentClassJSON
	case hasPrefix(data, "<?xml"), hasPrefix(data, "<html"), hasPrefix(data, "<!DOCTYPE"):
		return ContentClassText
	case hasPrefix(data, "%PDF"):
		return ContentClassCompressed // PDF is typically already compressed internally
	}

	return ContentClassUnknown
}

// classifyByContent inspects byte-level properties when magic bytes give no match.
func classifyByContent(data []byte) ContentClass {
	var printable int
	for _, b := range data {
		if b >= 0x09 && b <= 0x0d || b >= 0x20 && b <= 0x7e {
			printable++
		}
	}
	ratio := float64(printable) / float64(len(data))
	if ratio >= 0.85 {
		return ContentClassText
	}
	return ContentClassBinary
}

// shannonEntropy computes the Shannon entropy of data in bits per byte.
// Return range: [0, 8].
func shannonEntropy(data []byte) float64 {
	if len(data) == 0 {
		return 0
	}
	var freq [256]int
	for _, b := range data {
		freq[b]++
	}
	n := float64(len(data))
	var h float64
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// compressScore maps Shannon entropy to a compressibility score in [0, 1].
// Score 1.0 means highly compressible; 0.0 means essentially incompressible.
func compressScore(entropy float64) float64 {
	score := 1.0 - entropy/8.0
	if score < 0 {
		return 0
	}
	return score
}

// hasPrefix reports whether data starts with prefix.
func hasPrefix(data []byte, prefix string) bool {
	return len(data) >= len(prefix) && string(data[:len(prefix)]) == prefix
}
