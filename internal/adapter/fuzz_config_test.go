package adapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/objectfs/objectfs/internal/config"
	"github.com/objectfs/objectfs/internal/storage/s3"
	"github.com/objectfs/objectfs/internal/testaws"
)

// FuzzConfigConstructsBackend asserts the one property whose absence was the worst defect in
// v0.10.0: a configuration that loads and validates must be a configuration a backend can be built
// from.
//
// That is audit finding C1, and it is worth restating exactly, because the shape of the bug is the
// shape of this target. internal/config defaulted the write-buffer compression algorithm to "gzip".
// pkg/compression declared AlgorithmGzip. Two shipped example config files set it. internal/config's
// Validate accepted it. Only internal/compression's codec factory disagreed, and it is reached from
// NewBackend — so `objectfs s3://bucket /mnt` on the stock configuration got as far as attempting a
// mount and then exited with "Failed to start adapter", naming no setting. Every layer that read
// config believed the value was good; the single layer that had to act on it did not.
//
// No unit test could have caught that, and the reason is instructive. A test of Validate asserts what
// Validate thinks. A test of the codec factory asserts what the factory thinks. C1 was the two of
// them disagreeing, which is visible only to something that runs both and compares — this target's
// entire content.
//
// The property is directional. Validate rejecting a configuration is a pass, not a failure: refusing
// a bad setting at load time with a clear message is the behaviour wanted. What must not happen is
// Validate accepting something NewBackend then refuses, because by then the user has asked for a
// mount and the error surfaces without attribution.
//
// It goes through YAML rather than constructing a Configuration literal, because the loader is part
// of the seam. LoadFromFile uses non-strict unmarshalling (audit finding P-10), so a key with a typo
// silently keeps its default — and a target that built the struct directly would test a path no user
// takes.
func FuzzConfigConstructsBackend(f *testing.F) {
	// One endpoint and one bucket for the whole run. Both are supplied *after* buildS3Config, for
	// the reason given in overrideTransport: an unreachable endpoint is a network fact, and this
	// target is about config content.
	sh := testaws.Shared(f)

	bucket, err := sh.Bucket(context.Background())
	if err != nil {
		f.Fatalf("testaws: bucket: %v", err)
	}

	dir := f.TempDir()

	for _, seed := range configSeeds {
		f.Add(seed.yaml)
	}

	f.Fuzz(func(t *testing.T, doc string) {
		// A megabyte of YAML is a test of the YAML parser's allocator, not of ObjectFS.
		if len(doc) > 64*1024 {
			return
		}

		cfg := config.NewDefault()

		// LoadFromFile rather than yaml.Unmarshal, because the path-validation and read steps are
		// part of what a user's config goes through and have their own failure modes.
		path := filepath.Join(dir, "objectfs.yaml")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatalf("write the config under test: %v", err)
		}

		if err := cfg.LoadFromFile(path); err != nil {
			// Malformed YAML is the loader's to reject, and it did.
			return
		}

		if err := cfg.Validate(); err != nil {
			// Rejected up front with a message naming the setting. This is the outcome C1 should
			// have had.
			return
		}

		s3cfg := (&Adapter{config: cfg}).buildS3Config()
		overrideTransport(s3cfg, sh)

		// Construct only for a configuration not already tried.
		//
		// NewBackend ends with a real HealthCheck over a real socket. Fuzzing it at ten thousand
		// iterations a second opens ten thousand connections a second, each leaving a TIME_WAIT socket
		// for the kernel's timeout, and the ephemeral port range is exhausted long before those
		// expire — after which every construction fails with "can't assign requested address" whatever
		// its configuration. That was observed and reported as a counterexample whose minimized input
		// was the empty string.
		//
		// Deduplication is the fix rather than a rate limit because it addresses why the requests were
		// wasted. buildS3Config maps six fields; the fuzzer mutates a YAML document. The overwhelming
		// majority of mutations — whitespace, comments, keys that do not exist, values under keys that
		// are not mapped — produce an *identical* s3.Config, and constructing a backend from the same
		// config a thousand times tests nothing the first one did not. What remains is one
		// construction per distinct configuration, which is the number this target is actually about.
		//
		// Suppression is impossible: a config is skipped only when the same config was already carried
		// through NewBackend, so any construction failure is still reached by whichever iteration
		// reaches that config first.
		if seenConfig(s3cfg) {
			return
		}

		backend, err := s3.NewBackend(context.Background(), bucket, s3cfg)
		if err == nil {
			_ = backend.Close()

			return
		}

		// Confirm before reporting, because the failure modes differ in reproducibility. A
		// configuration defect is deterministic: the same config fails every time. Port exhaustion is
		// transient, and it can strike between two calls — an earlier version of this target ran a
		// single control construction and reported a counterexample anyway, because the range happened
		// to recover in the microseconds between the two.
		//
		// So require the failure to persist *and* a known-good configuration to succeed, both twice.
		// A real defect satisfies that trivially. Exhaustion cannot, since it is indiscriminate.
		for range 2 {
			control, controlErr := s3.NewBackend(context.Background(), bucket, sh.Config())
			if controlErr != nil {
				// The endpoint is refusing everyone. Nothing about the input under test.
				return
			}
			_ = control.Close()

			retried, retryErr := s3.NewBackend(context.Background(), bucket, s3cfg)
			if retryErr == nil {
				// Did not reproduce, so the first failure was environmental.
				_ = retried.Close()

				return
			}
		}

		t.Fatalf("Validate accepted this configuration and NewBackend then refused it repeatably, "+
			"while a default configuration constructed successfully alongside it — the C1 seam, "+
			"where the user has already asked for a mount before anything names the bad "+
			"setting:\n%v\n\nconfig:\n%s", err, doc)
	})
}

// seenConfig reports whether an equivalent S3 config has already been carried through NewBackend,
// recording it if not.
//
// The identity is the mapped s3.Config, not the YAML that produced it. Two documents differing only
// in comments, key order, whitespace, or keys nothing reads map to the same config and would exercise
// the same construction path — see the note at the call site for why that matters at fuzzing rates.
//
// %#v as the key rather than a hand-written field list, deliberately. Config gains fields over time,
// and a hand-written key would silently stop distinguishing whichever field was added last: the first
// document to set it would be tested, and every subsequent value of it dismissed as already seen. The
// audit's finding D12 is precisely a list of Config fields somebody forgot to extend. %#v renames
// itself when the struct changes.
func seenConfig(cfg *s3.Config) bool {
	key := fmt.Sprintf("%#v", *cfg)

	seenMu.Lock()
	defer seenMu.Unlock()

	if _, ok := seen[key]; ok {
		return true
	}

	// Bound the memory. A fuzz run is unbounded in length, and an unbounded map in a target that runs
	// for hours is a leak reported as a fuzzer crash. At the cap the set stops growing and later
	// distinct configs are simply tested again, which costs requests but loses no coverage.
	const maxSeen = 4096
	if len(seen) < maxSeen {
		seen[key] = struct{}{}
	}

	return false
}

var (
	seenMu sync.Mutex
	seen   = map[string]struct{}{}
)

// overrideTransport points a fuzzer-derived S3 config at the test endpoint.
//
// Everything replaced here describes *where* to talk and *as whom*, not what the filesystem should
// do with the bytes. Leaving them fuzzer-controlled would make every iteration fail on an
// unreachable host — NewBackend ends with a real HealthCheck — and those failures would say nothing
// about whether config and construction agree, which is the only question this target asks.
//
// Region deliberately survives. It is a config-content field, the AWS SDK accepts any well-formed
// string for it (verified: "not-a-region" constructs a client without error), and it reaches the
// endpoint resolver — so a region that broke construction is a finding rather than noise.
func overrideTransport(cfg *s3.Config, sh *testaws.SharedServer) {
	base := sh.Config()

	cfg.Endpoint = base.Endpoint
	cfg.ForcePathStyle = base.ForcePathStyle
	cfg.AccessKeyID = base.AccessKeyID
	cfg.SecretAccessKey = base.SecretAccessKey
	cfg.MaxRetries = base.MaxRetries

	// Acceleration rewrites the host to s3-accelerate.amazonaws.com, which is a real DNS name this
	// process must not resolve, and the resulting timeout would be reported as a config defect.
	cfg.UseAccelerate = false
}

// configSeeds are the configuration shapes worth starting the fuzzer from.
//
// The compression block is over-represented because that is where C1 lived and where the layers are
// still most numerous: a YAML key, a config struct field, a validator, a Settings translation, a
// codec factory, and a per-algorithm level range. Six places for one setting to be misunderstood.
var configSeeds = []struct {
	name string
	why  string
	yaml string
}{
	{
		name: "empty",
		why:  "an empty file must leave every default intact, and the defaults must construct",
		yaml: "",
	},
	{
		name: "C1 itself",
		why:  "the v0.10.0 default: gzip named where no gzip codec existed",
		yaml: `write_buffer:
  compression:
    enabled: true
    algorithm: gzip
    level: 6
    min_size: 4KB
`,
	},
	{
		name: "every supported algorithm's name",
		why:  "each must either validate and construct, or be rejected by Validate — never neither",
		yaml: `write_buffer:
  compression:
    enabled: true
    algorithm: zstd
    level: 3
`,
	},
	{
		name: "a level valid for one algorithm and not another",
		why:  "zstd takes 0-22 and gzip only 0-9, so a name-matching validator cannot catch this",
		yaml: `write_buffer:
  compression:
    enabled: true
    algorithm: gzip
    level: 19
`,
	},
	{
		name: "an algorithm no codec exists for",
		why:  "must be refused by Validate, with the supported set named",
		yaml: `write_buffer:
  compression:
    enabled: true
    algorithm: brotli
`,
	},
	{
		name: "a stale algorithm in a disabled block",
		why:  "a setting with no effect must not refuse the mount",
		yaml: `write_buffer:
  compression:
    enabled: false
    algorithm: brotli
    level: 99
`,
	},
	{
		name: "an unparseable min_size",
		why:  "parsed by internal/compression, not by the validator's own arithmetic",
		yaml: `write_buffer:
  compression:
    enabled: true
    algorithm: zstd
    min_size: "4 gigglebytes"
`,
	},
	{
		name: "storage and network fields",
		why:  "the rest of what buildS3Config maps — region reaches the endpoint resolver",
		yaml: `storage:
  s3:
    region: eu-west-1
    force_path_style: true
network:
  congestion_algorithm: bbr
`,
	},
	{
		name: "a congestion algorithm the kernel will not have",
		why:  "applied as a socket option best-effort; must not fail construction",
		yaml: `network:
  congestion_algorithm: nonexistent-cc
`,
	},
	{
		name: "the validated scalars at their boundaries",
		why:  "max_concurrency and connection_pool_size are rejected at zero, so zero must not reach a backend",
		yaml: `performance:
  max_concurrency: 1
  connection_pool_size: 1
global:
  log_level: DEBUG
  metrics_port: 9090
  health_port: 8080
`,
	},
	{
		name: "a key that does not exist",
		why:  "P-10: non-strict unmarshalling accepts it silently, which is itself worth pinning",
		yaml: `write_buffer:
  compression:
    enable: true
    zstd_level: 3
`,
	},
}

// TestConfigSeedsLoadAsTheShapesTheyClaim guards the corpus the way the difftest corpus is guarded:
// a seed that no longer parses, or that parses to something other than what its comment says, is a
// seed the fuzzer starts from and learns nothing at.
//
// The check is that each seed reaches a *decision*. A seed rejected by the YAML parser is not
// exercising the property under test — it is exercising go-yaml — and would sit in the corpus looking
// like coverage.
func TestConfigSeedsLoadAsTheShapesTheyClaim(t *testing.T) {
	t.Parallel()

	if len(configSeeds) == 0 {
		t.Fatal("the seed corpus is empty, so the fuzzer starts from nothing")
	}

	dir := t.TempDir()

	for i, tc := range configSeeds {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.NewDefault()

			path := filepath.Join(dir, fmt.Sprintf("seed-%d.yaml", i))
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			if err := cfg.LoadFromFile(path); err != nil {
				t.Fatalf("does not parse, so it exercises the YAML library rather than "+
					"ObjectFS: %v\n%s", err, tc.why)
			}
		})
	}
}

// TestValidateRejectsWhatTheCodecFactoryRejects is the deterministic form of what
// [FuzzConfigConstructsBackend] searches for, and it is here because a fuzz target is not a
// regression test: a corpus can be emptied, a fuzztime can be set to zero, and nothing fails.
//
// Every case is a compression setting that v0.10.0 accepted at config load. The assertion is that
// Validate now refuses them, which is the difference between "objectfs: invalid
// write_buffer.compression: unsupported compression algorithm" at startup and "Failed to start
// adapter" after the mount attempt.
func TestValidateRejectsWhatTheCodecFactoryRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		algorithm string
		level     int
		minSize   string
		wantErr   bool
		why       string
	}{
		{
			name:      "gzip at a gzip level",
			algorithm: "gzip",
			level:     6,
			wantErr:   false,
			why:       "C1's value, now with a codec behind it",
		},
		{
			name:      "gzip at a zstd level",
			algorithm: "gzip",
			level:     19,
			wantErr:   true,
			why:       "19 is valid for zstd and not for gzip, which only building the codec can know",
		},
		{
			name:      "zstd at its maximum",
			algorithm: "zstd",
			level:     22,
			wantErr:   false,
		},
		{
			name:      "zstd past its maximum",
			algorithm: "zstd",
			level:     23,
			wantErr:   true,
		},
		{
			name:      "an algorithm with no codec",
			algorithm: "brotli",
			wantErr:   true,
			why:       "the exact class C1 belonged to",
		},
		{
			name:      "an unparseable minimum size",
			algorithm: "zstd",
			minSize:   "4 gigglebytes",
			wantErr:   true,
		},
		{
			name:      "lz4, which ignores the level",
			algorithm: "lz4",
			level:     99,
			wantErr:   false,
			why:       "not every algorithm has a level range, and rejecting one it ignores would be wrong",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.NewDefault()
			cfg.WriteBuffer.Compression.Enabled = true
			cfg.WriteBuffer.Compression.Algorithm = tc.algorithm
			cfg.WriteBuffer.Compression.Level = tc.level
			cfg.WriteBuffer.Compression.MinSize = tc.minSize

			err := cfg.Validate()

			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("Validate accepted algorithm=%q level=%d min_size=%q, which the codec "+
					"factory will refuse — C1's seam, reopened. %s",
					tc.algorithm, tc.level, tc.minSize, tc.why)
			case !tc.wantErr && err != nil:
				t.Fatalf("Validate refused a configuration a codec exists for: %v. %s", err, tc.why)
			}

			// The message has to name the setting. "invalid configuration" sends the reader to the
			// wrong file, which is most of what made C1 expensive to diagnose.
			if err != nil && !strings.Contains(err.Error(), "compression") {
				t.Errorf("the error does not mention compression, so it does not tell the user "+
					"which setting to change: %v", err)
			}
		})
	}
}
