package config

import (
	"strings"
	"testing"
)

// TestValidateRejectsUnusableS3Settings covers the keys that gained validation in v0.10.1, when
// buildS3Config started carrying them to the backend.
//
// Validating them here rather than at the point of use is a deliberate choice with a specific
// history. Every one of these values was previously accepted by the loader and then either silently
// replaced or silently ignored:
//
//   - an unrecognized storage class is replaced with STANDARD inside NewTierValidator, so
//     `STANDARD_1A` — digit one for capital I — mounted successfully and billed as STANDARD forever;
//   - a size string internal/adapter.parseSize could not parse became 1 GiB, with no message, so
//     `2G`, `64MiB` and `tpyo` all configured the same 1 GiB cache;
//   - a chunk size above the multipart threshold means the first part of every upload is the whole
//     object, so multipart never engages — no error, just a feature that does not happen.
//
// The common shape is a bad value producing a working mount that behaves differently from what the
// file says. That is the failure mode this whole task exists to remove, and the only place it can be
// caught with a message naming the YAML path is the layer that reads YAML.
//
// Each case asserts the message mentions the offending key, because "invalid configuration" is not
// an actionable error — the operator's next action is to edit one line in one file.
func TestValidateRejectsUnusableS3Settings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		mutate   func(*Configuration)
		wantText string
		why      string
	}{
		{
			name: "a misspelled storage class",
			mutate: func(c *Configuration) {
				c.Storage.S3.StorageTier = "STANDARD_1A"
			},
			wantText: "storage.s3.storage_tier",
			why: "NewTierValidator falls back to STANDARD for anything it does not recognize, so " +
				"without this check the mount succeeds and bills as STANDARD",
		},
		{
			name: "a lowercase storage class",
			mutate: func(c *Configuration) {
				c.Storage.S3.StorageTier = "standard_ia"
			},
			wantText: "storage.s3.storage_tier",
			why:      "S3 storage class names are case-sensitive on the wire",
		},
		{
			name: "a negative SDK retry limit",
			mutate: func(c *Configuration) {
				c.Storage.S3.MaxRetries = -1
			},
			wantText: "storage.s3.max_retries",
			why:      "it reaches config.WithRetryMaxAttempts, which has no meaning for a negative count",
		},
		{
			name: "a negative multipart concurrency",
			mutate: func(c *Configuration) {
				c.Storage.S3.Multipart.Concurrency = -2
			},
			wantText: "storage.s3.multipart.concurrency",
			why:      "it sizes a semaphore; a negative capacity panics make()",
		},
		{
			name: "a chunk size larger than the multipart threshold",
			mutate: func(c *Configuration) {
				c.Storage.S3.Multipart.Threshold = "16MB"
				c.Storage.S3.Multipart.ChunkSize = "32MB"
			},
			wantText: "chunk_size",
			why: "the first part would be the whole object, so an upload past the threshold is " +
				"still a single PUT and multipart never engages",
		},
		{
			name: "an unparseable multipart threshold",
			mutate: func(c *Configuration) {
				c.Storage.S3.Multipart.Threshold = "32 megabytes"
			},
			wantText: "storage.s3.multipart.threshold",
			why:      "parseOptionalSize is strict; the old parser would have substituted 1 GiB",
		},
		{
			name: "an unparseable multipart chunk size",
			mutate: func(c *Configuration) {
				c.Storage.S3.Multipart.ChunkSize = "16MiB"
			},
			wantText: "storage.s3.multipart.chunk_size",
			why: "MiB is not accepted — the binary suffixes are MB/GB, and silently treating an " +
				"unknown suffix as valid is how 64MiB became 1 GiB",
		},
		{
			name: "an unparseable parallel-read threshold",
			mutate: func(c *Configuration) {
				c.Performance.ParallelRead.Threshold = "sixty-four megabytes"
			},
			wantText: "performance.parallel_read.threshold",
			why:      "it reaches s3.Config.ParallelReadThreshold, where zero means the feature is off",
		},
		{
			name: "a negative parallel-read chunk size",
			mutate: func(c *Configuration) {
				c.Performance.ParallelRead.ChunkSize = "-16MB"
			},
			wantText: "performance.parallel_read.chunk_size",
			why: "it becomes the length of each range GET; a negative one produces a Range header " +
				"S3 rejects, or a slice bound that panics",
		},
		{
			name: "an empty required cache size",
			mutate: func(c *Configuration) {
				c.Performance.CacheSize = ""
			},
			wantText: "performance.cache_size",
			why: "the only size with no sensible absent-value default: an empty read cache is not " +
				"the same request as a default-sized one",
		},
		{
			name: "an unparseable persistent cache size",
			mutate: func(c *Configuration) {
				c.Cache.PersistentCache.MaxSize = "200 gigs"
			},
			wantText: "cache.persistent_cache.max_size",
			why:      "it bounds an on-disk cache; getting it wrong fills the disk",
		},
		{
			name: "a compression algorithm no codec implements",
			mutate: func(c *Configuration) {
				c.Storage.S3.Compression.Enabled = true
				c.Storage.S3.Compression.Algorithm = "brotli"
			},
			wantText: "brotli",
			why: "this is audit finding C1's shape — the shipped default named an algorithm the " +
				"codec factory rejected, so the binary exited on its own default configuration",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := NewDefault()
			tc.mutate(cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted this configuration, so it reaches the backend and takes "+
					"effect as something other than what the file says. %s", tc.why)
			}

			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("Validate rejected it but the message does not mention %q, so the operator "+
					"is not told which line to edit:\n%v", tc.wantText, err)
			}
		})
	}
}

// TestValidateAcceptsAbsentOptionalSizes pins the other half of the size contract: empty means "use
// the built-in default", and only performance.cache_size is required.
//
// Without this, the natural way to write validateSizes — reject anything that does not parse,
// including "" — would make a partial config file impossible. Overriding one key would require
// supplying every size in the schema, which is the opposite of how these files are meant to work
// and would have broken every existing deployment on upgrade.
//
// performance.read_ahead.window_size is deliberately absent from this list: it is the one size that
// must be set when its feature is enabled, because empty is a prefetch floor of zero rather than the
// documented default, and validateReadAheadConfig rejects it by name.
// TestValidateRejectsEachWayReadAheadCanBeWrong covers that direction.
func TestValidateAcceptsAbsentOptionalSizes(t *testing.T) {
	t.Parallel()

	cfg := NewDefault()

	cfg.Performance.WriteBufferSize = ""
	cfg.Performance.ParallelRead.Threshold = ""
	cfg.Performance.ParallelRead.ChunkSize = ""
	cfg.Cache.PersistentCache.MaxSize = ""
	cfg.WriteBuffer.MaxMemory = ""
	cfg.Storage.S3.Multipart.Threshold = ""
	cfg.Storage.S3.Multipart.ChunkSize = ""

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a config that omits every optional size, so overriding one "+
			"setting would require supplying all of them: %v", err)
	}
}

// TestValidateAcceptsEveryStorageClassTheBackendWrites walks the classes rather than spot-checking
// one, because the failure this guards against is an allow-list that drifts from what the backend
// supports — and it drifts silently in the direction of refusing a valid config at startup.
func TestValidateAcceptsEveryStorageClassTheBackendWrites(t *testing.T) {
	t.Parallel()

	classes := []string{
		"STANDARD",
		"STANDARD_IA",
		"ONEZONE_IA",
		"INTELLIGENT_TIERING",
		"GLACIER_IR",
		"GLACIER",
		"DEEP_ARCHIVE",
		"REDUCED_REDUNDANCY",
	}

	for _, class := range classes {
		t.Run(class, func(t *testing.T) {
			t.Parallel()

			cfg := NewDefault()
			cfg.Storage.S3.StorageTier = class

			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate rejected storage class %q, which S3 accepts on a PUT: %v", class, err)
			}
		})
	}
}

// TestValidateAcceptsAnAbsentStorageClass covers the case a config file most often actually has.
//
// Empty must mean STANDARD, matching what S3 applies to a PUT that names no class, so that a file
// which omits the key gets the same behavior as one that sets it explicitly. A validator that
// demanded a non-empty class would reject every config written before the key existed.
func TestValidateAcceptsAnAbsentStorageClass(t *testing.T) {
	t.Parallel()

	cfg := NewDefault()
	cfg.Storage.S3.StorageTier = ""

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected an absent storage_tier, which must mean STANDARD: %v", err)
	}
}

// TestValidateIgnoresACompressionAlgorithmInADisabledBlock is the deliberate hole in
// validateCompressionConfig, asserted so that closing it has to be a decision.
//
// Refusing to start over an algorithm name in a block that is switched off would turn a stale
// comment into an outage — and the shipped default has compression disabled, so the value in that
// block is not consulted by anything.
func TestValidateIgnoresACompressionAlgorithmInADisabledBlock(t *testing.T) {
	t.Parallel()

	cfg := NewDefault()
	cfg.Storage.S3.Compression.Enabled = false
	cfg.Storage.S3.Compression.Algorithm = "no-such-codec"

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected an unusable algorithm inside a disabled compression block, so a "+
			"setting with no effect can stop the filesystem from mounting: %v", err)
	}
}
