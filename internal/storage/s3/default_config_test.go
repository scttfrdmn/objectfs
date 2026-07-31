package s3_test

// C1: the shipped default configuration could not construct a backend.
//
// internal/config defaulted compression.algorithm to "gzip"; internal/compression's codec factory
// implemented only none, zstd, and lz4. pkg/compression declared AlgorithmGzip, the S3 config
// comment listed gzip as valid, and two shipped example configs set it — so every layer that read
// config believed the value was good, and the only code that knew better was the factory, reached
// after the user had already asked for a mount. `objectfs s3://bucket /mnt` exited with
// "Failed to start adapter" naming nothing.
//
// These tests approach it from both ends: the defaults must construct, and a bad value must be
// refused at config load rather than at mount time.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/objectfs/objectfs/internal/compression"
	"github.com/objectfs/objectfs/internal/config"
	"github.com/objectfs/objectfs/internal/storage/s3"
	"github.com/objectfs/objectfs/internal/testaws"
	comprpkg "github.com/objectfs/objectfs/pkg/compression"
)

// TestShippedDefaultsConstructABackend is the C1 regression test.
//
// It reads the compression settings from config.NewDefault() — the same values a user gets with no
// config file at all — and hands them to a backend, mirroring what the adapter does on mount. The
// endpoint and credentials have to be overridden to reach the test server, but nothing about the
// compression block is, because that block is the defect.
func TestShippedDefaultsConstructABackend(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	defaults := config.NewDefault()

	cfg := s3.NewDefaultConfig()
	cfg.Endpoint = ts.URL
	cfg.ForcePathStyle = true
	cfg.Region = testaws.DefaultRegion
	cfg.AccessKeyID = testaws.AccessKeyID
	cfg.SecretAccessKey = testaws.SecretAccessKey

	// The compression block comes from the application defaults verbatim. Assembling it by hand
	// would be the mistake that let C1 ship: the value under test is the one the user actually gets.
	cfg.Compression = s3.CompressionConfig{
		Enabled:   defaults.WriteBuffer.Compression.Enabled,
		Algorithm: defaults.WriteBuffer.Compression.Algorithm,
		Level:     defaults.WriteBuffer.Compression.Level,
		MinSize:   defaults.WriteBuffer.Compression.MinSize,
	}

	backend, err := s3.NewBackend(context.Background(), ts.Bucket, cfg)
	if err != nil {
		t.Fatalf("the shipped default configuration cannot construct a backend: %v\n"+
			"This is what a user gets from `objectfs s3://bucket /mnt` with no config file.", err)
	}

	t.Cleanup(func() { _ = backend.Close() })

	// And it must actually work, not merely construct.
	ctx := context.Background()

	const key = "defaults/roundtrip"

	want := testaws.DeterministicBytes(key, 64*1024)
	if err := backend.PutObject(ctx, key, want); err != nil {
		t.Fatalf("PutObject on the default configuration: %v", err)
	}

	got, err := backend.GetObject(ctx, key, 0, int64(len(want)))
	if err != nil {
		t.Fatalf("GetObject on the default configuration: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("round trip on the default configuration returned %d bytes that differ from the "+
			"%d written", len(got), len(want))
	}
}

// TestDefaultConfigurationValidates closes the other half of the seam. Validate() runs at startup
// before anything is mounted, so an algorithm the codecs cannot build must be refused there — and
// the defaults must pass their own validation, which they did not.
func TestDefaultConfigurationValidates(t *testing.T) {
	t.Parallel()

	if err := config.NewDefault().Validate(); err != nil {
		t.Fatalf("the shipped defaults fail their own validation: %v", err)
	}
}

// TestCompressionValidationRejectsWhatTheCodecsCannotBuild asserts the two ways a compression block
// can be unbuildable, and that validation catches both. The level cases matter as much as the
// algorithm: zstd accepts 0–22 and gzip only 0–9, so a level copied between algorithms is a live
// misconfiguration rather than a hypothetical one — and validating the algorithm alone would miss it.
func TestCompressionValidationRejectsWhatTheCodecsCannotBuild(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*config.CompressionConfig)
		wantErr bool
		// wantMentions is a substring the message must contain, so the error names the offending
		// value rather than saying only that configuration is invalid.
		wantMentions string
	}{
		{
			name:         "an algorithm with no codec",
			mutate:       func(c *config.CompressionConfig) { c.Enabled = true; c.Algorithm = "brotli" },
			wantErr:      true,
			wantMentions: "brotli",
		},
		{
			// The exact value v0.10.0 shipped as the default. It is valid now, and this case
			// documents that it became valid by gaining a codec rather than by being dropped.
			name:    "gzip, which v0.10.0 defaulted to without implementing",
			mutate:  func(c *config.CompressionConfig) { c.Enabled = true; c.Algorithm = "gzip"; c.Level = 6 },
			wantErr: false,
		},
		{
			name:         "a zstd level applied to gzip",
			mutate:       func(c *config.CompressionConfig) { c.Enabled = true; c.Algorithm = "gzip"; c.Level = 22 },
			wantErr:      true,
			wantMentions: "gzip level 22",
		},
		{
			name:         "a level past zstd's range",
			mutate:       func(c *config.CompressionConfig) { c.Enabled = true; c.Algorithm = "zstd"; c.Level = 99 },
			wantErr:      true,
			wantMentions: "99",
		},
		{
			name:         "an unparseable min_size",
			mutate:       func(c *config.CompressionConfig) { c.Enabled = true; c.Algorithm = "zstd"; c.MinSize = "4 furlongs" },
			wantErr:      true,
			wantMentions: "furlongs",
		},
		{
			// A disabled block is never consulted, so refusing to start over a stale value in it
			// would be a false alarm — and would make disabling compression harder than fixing it.
			name:    "a bad algorithm in a disabled block",
			mutate:  func(c *config.CompressionConfig) { c.Enabled = false; c.Algorithm = "brotli" },
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.NewDefault()
			tc.mutate(&cfg.WriteBuffer.Compression)

			err := cfg.Validate()

			if tc.wantErr && err == nil {
				t.Fatalf("Validate accepted %+v; the failure would surface at mount time as "+
					"\"Failed to start adapter\"", cfg.WriteBuffer.Compression)
			}

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Validate rejected a usable configuration %+v: %v",
						cfg.WriteBuffer.Compression, err)
				}

				return
			}

			if tc.wantMentions != "" && !strings.Contains(err.Error(), tc.wantMentions) {
				t.Errorf("error %q does not mention %q, so it does not tell the user which value "+
					"to change", err, tc.wantMentions)
			}
		})
	}
}

// TestEveryDeclaredAlgorithmHasACodec is the structural guard against C1 recurring. An exported
// Algorithm constant with no codec behind it is exactly what shipped: the declaration is what every
// other layer trusts, so a constant and an implementation must arrive together.
func TestEveryDeclaredAlgorithmHasACodec(t *testing.T) {
	t.Parallel()

	for _, algo := range comprpkg.SupportedAlgorithms() {
		t.Run(string(algo), func(t *testing.T) {
			t.Parallel()

			codec, err := compression.New(algo, comprpkg.DefaultLevel)
			if err != nil {
				t.Fatalf("pkg/compression declares %q but internal/compression cannot build it: %v",
					algo, err)
			}

			if got := codec.Algorithm(); got != algo {
				t.Errorf("New(%q) returned a codec reporting %q", algo, got)
			}

			// Round trip through the codec, so a codec that exists but does not work cannot satisfy
			// this test. The payload is compressible so that a codec silently returning its input
			// is still caught by the equality check rather than by a size assertion.
			want := bytes.Repeat([]byte("objectfs codec round trip "), 512)

			encoded, err := codec.Compress(want)
			if err != nil {
				t.Fatalf("Compress: %v", err)
			}

			decoded, err := codec.Decompress(encoded)
			if err != nil {
				t.Fatalf("Decompress: %v", err)
			}

			if !bytes.Equal(decoded, want) {
				t.Errorf("%q round trip returned %d bytes that differ from the %d written",
					algo, len(decoded), len(want))
			}

			// Every codec that actually encodes must name an HTTP content coding, because that
			// token is the only record of how to decode the stored object. A compressing codec with
			// an empty Content-Encoding writes objects nothing can read back — which is the shape
			// of the CargoShip corruption, arrived at from the codec side.
			if algo != comprpkg.AlgorithmNone && codec.ContentEncoding() == "" {
				t.Errorf("codec %q reports no Content-Encoding, so objects it writes record no way "+
					"to decode them", algo)
			}
		})
	}
}
