package compression

import (
	"fmt"

	"github.com/klauspost/compress/zstd"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

// ZstdCodec implements Codec using github.com/klauspost/compress/zstd.
// Both Compress and Decompress are safe for concurrent use.
type ZstdCodec struct {
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

// mapZstdLevel converts an integer level in [0, 22] to a zstd.EncoderLevel.
//
//	0        → SpeedDefault   (zstd level 3 — balanced)
//	1        → SpeedFastest   (zstd level 1 — best speed)
//	2–4      → SpeedDefault
//	5–9      → SpeedBetterCompression
//	10–22    → SpeedBestCompression
func mapZstdLevel(level int) (zstd.EncoderLevel, error) {
	switch {
	case level < 0:
		return 0, fmt.Errorf("zstd level %d out of range [0, 22]", level)
	case level == 0:
		return zstd.SpeedDefault, nil
	case level == 1:
		return zstd.SpeedFastest, nil
	case level <= 4:
		return zstd.SpeedDefault, nil
	case level <= 9:
		return zstd.SpeedBetterCompression, nil
	case level <= 22:
		return zstd.SpeedBestCompression, nil
	default:
		return 0, fmt.Errorf("zstd level %d out of range [0, 22]", level)
	}
}

// NewZstdCodec creates a ZstdCodec at the given compression level.
// Pass 0 (or comprpkg.DefaultLevel) for the library default (level 3).
func NewZstdCodec(level int) (*ZstdCodec, error) {
	encoderLevel, err := mapZstdLevel(level)
	if err != nil {
		return nil, err
	}

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(encoderLevel))
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}

	dec, err := zstd.NewReader(nil)
	if err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}

	return &ZstdCodec{encoder: enc, decoder: dec}, nil
}

// Compress compresses src using the Zstandard algorithm.
// EncodeAll is concurrency-safe within the same Encoder.
func (c *ZstdCodec) Compress(src []byte) ([]byte, error) {
	dst := make([]byte, 0, len(src)/2)
	return c.encoder.EncodeAll(src, dst), nil
}

// Decompress decompresses src.
// DecodeAll is concurrency-safe within the same Decoder.
func (c *ZstdCodec) Decompress(src []byte) ([]byte, error) {
	dst, err := c.decoder.DecodeAll(src, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd decompress: %w", err)
	}
	return dst, nil
}

// Algorithm returns the algorithm identifier.
func (c *ZstdCodec) Algorithm() comprpkg.Algorithm { return comprpkg.AlgorithmZstd }

// ContentEncoding returns the HTTP Content-Encoding token for ZSTD objects.
func (c *ZstdCodec) ContentEncoding() string { return "zstd" }
