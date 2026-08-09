//go:build !integration

package compression

import (
	"bytes"
	"math/rand/v2"
	"testing"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

// BenchmarkCompressAlreadyCompressed measures what the #184 skip is worth on the write path.
//
// Run it both ways to get the comparison, since the skip is not switchable at runtime — that is
// deliberate, a config key for "compress data that cannot be compressed" is not a choice anyone should
// be offered:
//
//	go test ./internal/compression/ -run '^$' -bench BenchmarkCompressAlreadyCompressed -benchmem
//	# then delete the Analyze block in Compress and run it again
//	benchstat before.txt after.txt
//
// The payload is a real zstd frame, built from a synthetic VCF, which is the ".tar.zst already in the
// bucket" case rather than a magic-byte prefix over noise: a prefix would understate the saving,
// because zstd gives up on random bytes faster than it gives up on a frame it already produced.
//
// A number rather than a plausible argument is the point. This repository has a documented history of
// optimizations described without measurement — the throughput table on the docs index claimed
// 800-1200 MB/s that no benchmark in the tree produced — so an optimization landing here states its
// measured effect or it does not land.
func BenchmarkCompressAlreadyCompressed(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20, 8 << 20} {
		b.Run(byteSizeName(size), func(b *testing.B) {
			c := benchCompressor(b)
			data := zstdFrameOfAbout(b, size)

			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				if _, _, err := c.Compress(data); err != nil {
					b.Fatalf("Compress: %v", err)
				}
			}
		})
	}
}

// BenchmarkCompressCompressibleText is the control.
//
// The skip adds an AlreadyCompressed call to every write above min_size, so the data compression *does*
// help has to pay for it. This benchmark is what says how much, and it is the reason the gate is
// AlreadyCompressed rather than Analyze: with Analyze it cost +53% at 64 KiB, because Analyze's entropy
// pass is 2.1µs against 6.7µs of codec time. Measured, then fixed — see BenchmarkGateVersusAnalyze.
func BenchmarkCompressCompressibleText(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20, 8 << 20} {
		b.Run(byteSizeName(size), func(b *testing.B) {
			c := benchCompressor(b)
			data := bytes.Repeat([]byte("chr1\t10583\trs58108140\tG\tA\t100\tPASS\n"), size/35+1)

			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				if _, _, err := c.Compress(data); err != nil {
					b.Fatalf("Compress: %v", err)
				}
			}
		})
	}
}

// BenchmarkGateVersusAnalyze isolates the cost the skip adds, independent of any codec, and states why
// the write path calls AlreadyCompressed instead of Analyze.
//
// Both classify through the same magic-byte table; Analyze additionally runs a Shannon entropy pass
// over the 4 KiB sample, and that pass is the entire difference. Reported per object rather than per
// byte, because that is what it is — both cap the sample at analysisSampleSize, so an 8 MiB object and
// a 4 KiB one cost the same.
//
// The two sub-benchmarks are on compressible text rather than on a compressed frame on purpose. That is
// the case where the difference is paid: a compressed object gets its whole codec pass skipped either
// way, while text pays the gate and then compresses regardless.
func BenchmarkGateVersusAnalyze(b *testing.B) {
	data := bytes.Repeat([]byte("chr1\t10583\trs58108140\tG\tA\t100\tPASS\n"), 2000)

	b.Run("AlreadyCompressed", func(b *testing.B) {
		b.ReportAllocs()

		for range b.N {
			if AlreadyCompressed(data) {
				b.Fatal("text classified as already compressed")
			}
		}
	})

	b.Run("Analyze", func(b *testing.B) {
		b.ReportAllocs()

		for range b.N {
			if Analyze(data).ContentClass == ContentClassCompressed {
				b.Fatal("text classified as already compressed")
			}
		}
	})
}

// benchCompressor builds the compressor the mount path would build: zstd, no size floor.
func benchCompressor(b *testing.B) *Compressor {
	b.Helper()

	c, err := NewCompressor(makeBenchSettings())
	if err != nil {
		b.Fatalf("NewCompressor: %v", err)
	}

	return c
}

// makeBenchSettings mirrors the test helper makeConfig, which lives in a _test.go file this build
// shares, but is spelled out here so the benchmark reads without cross-referencing it.
func makeBenchSettings() Settings {
	return Settings{Enabled: true, Algorithm: string(comprpkg.AlgorithmZstd), MinSize: "0", Level: 0}
}

// zstdFrameOfAbout returns a real zstd frame of roughly target bytes.
//
// "Roughly" because the only way to hit an exact compressed size is to pad with incompressible bytes,
// which would make the frame less like the archives this models. The uncompressed input is a mix of
// repetitive records and high-entropy bytes so the frame does not collapse to a few hundred bytes.
func zstdFrameOfAbout(b *testing.B, target int) []byte {
	b.Helper()

	codec, err := New(comprpkg.AlgorithmZstd, comprpkg.DefaultLevel)
	if err != nil {
		b.Fatalf("building the fixture codec: %v", err)
	}

	r := rand.New(rand.NewPCG(0x9E3779B97F4A7C15, 0xBF58476D1CE4E5B9))

	// Grow the input until the frame reaches the target. Roughly 2× target of half-random input lands
	// close; the loop makes that an outcome rather than an assumption.
	var frame []byte

	for chunks := target / 2048; len(frame) < target; chunks *= 2 {
		raw := make([]byte, 0, chunks*2048)
		for range chunks {
			raw = append(raw, bytes.Repeat([]byte("chr1\t10583\trs58108140\tG\tA\t100\tPASS\n"), 29)...)

			noise := make([]byte, 1024)
			for i := range noise {
				noise[i] = byte(r.UintN(256))
			}

			raw = append(raw, noise...)
		}

		if frame, err = codec.Compress(raw); err != nil {
			b.Fatalf("compressing the fixture: %v", err)
		}
	}

	return frame
}

// byteSizeName renders a power-of-two size as a benchmark sub-name.
func byteSizeName(n int) string {
	switch {
	case n >= 1<<20:
		return itoa(n>>20) + "MiB"
	case n >= 1<<10:
		return itoa(n>>10) + "KiB"
	default:
		return itoa(n) + "B"
	}
}

// itoa avoids pulling strconv in for one call in a benchmark file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}

	return string(digits)
}
