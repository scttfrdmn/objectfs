package adapter

import (
	"reflect"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/pkg/retry"
)

// A syntactically valid KMS key ARN, in the documentation-reserved account 111122223333. Only its
// shape matters: nothing here reaches KMS, and validateEncryptionConfig checks the form. The region
// matches the bucket region the fixture sets, because SSE-KMS requires the key and the bucket to be in
// the same region — a fixture that got that wrong would be asserting the mapping of a configuration
// S3 would reject.
const testKMSKeyARN = "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

// TestBuildS3ConfigMapsEveryConfiguredValue is the other half of internal/config.S3Config.
//
// buildS3Config is audit finding D12: it mapped six of s3.Config's thirty fields, so a mount got
// zero values for the storage tier, the connection pool size, the retry limit, both timeouts, the
// multipart settings and the parallel-read threshold — each of them a documented key in
// examples/config.yaml that did nothing. Two of those zeros were worse than inert: PoolSize zero
// deadlocked GetObjects and PutObjects on an unbuffered semaphore, and ParallelReadThreshold zero
// turned off the feature v0.10.0 was released for.
//
// Every value below is written out rather than computed from the input. That is the whole design of
// this test and the reason the plan specified it that way: a test that said
//
//	want := utils.ParseBytes(cfg.Storage.S3.Multipart.ChunkSize)
//
// would agree with any mapping formula, including one that read the threshold into the chunk size —
// which is exactly the shape of mistake a field-by-field mapping of thirty fields invites. Spelling
// "16MB" as 16777216 means the test fails when the mapping is wrong, not when it is different.
func TestBuildS3ConfigMapsEveryConfiguredValue(t *testing.T) {
	t.Parallel()

	// Deliberately distinct from every default and from each other, so a field crossed with its
	// neighbor is visible rather than coincidentally equal. NewDefaultConfig's threshold and chunk
	// size are 32MB/16MB and the pool size is 8; none of those numbers appear here.
	cfg := createTestConfig()
	cfg.Storage.S3.Region = "eu-west-1"
	cfg.Storage.S3.Endpoint = "https://s3.example.invalid"
	cfg.Storage.S3.ForcePathStyle = true
	cfg.Storage.S3.UseAcceleration = true
	cfg.Storage.S3.StorageTier = "GLACIER_IR"
	cfg.Storage.S3.MaxRetries = 7
	cfg.Storage.S3.UseCargoShip = true
	cfg.Storage.S3.Multipart = config.MultipartConfig{
		Threshold:   "48MB",
		ChunkSize:   "12MB",
		Concurrency: 5,
	}

	cfg.Performance.ConnectionPoolSize = 11
	cfg.Performance.ParallelRead = config.ParallelReadConfig{
		Enabled:   true,
		Threshold: "80MB",
		ChunkSize: "20MB",
	}

	cfg.Network.CongestionAlgorithm = "bbr"
	cfg.Network.Timeouts.Connect = 3 * time.Second
	cfg.Network.Timeouts.Read = 41 * time.Second
	cfg.Network.Timeouts.Write = 99 * time.Second // has no S3 counterpart; see the checks below
	cfg.Network.Retry.MaxAttempts = 9
	cfg.Network.Retry.BaseDelay = 250 * time.Millisecond
	cfg.Network.Retry.MaxDelay = 17 * time.Second
	cfg.Network.CircuitBreaker.Enabled = true
	cfg.Network.CircuitBreaker.FailureThreshold = 13
	cfg.Network.CircuitBreaker.Timeout = 45 * time.Second

	// lz4 rather than zstd. Every value in this test has to differ from the default it would take if
	// the mapping were absent, and zstd *is* the default — so `Algorithm: "zstd"` hardcoded in
	// buildS3Config passes an assertion written against zstd. That is the exact shape of D12, where a
	// field the mapping never touched still arrived at a plausible value from somewhere else.
	cfg.Storage.S3.Compression.Enabled = true
	cfg.Storage.S3.Compression.Algorithm = "lz4"
	cfg.Storage.S3.Compression.Level = 5
	cfg.Storage.S3.Compression.MinSize = "8KB"

	// sse-kms with bucket keys because it is the only mode that populates all three fields, so a
	// mapping that carried the mode and dropped the key — the shape of P-7 that is hardest to see,
	// since S3 accepts it and silently substitutes the AWS managed key — fails here.
	cfg.Security.Encryption = config.EncryptionConfig{
		Mode:       config.EncryptionModeKMS,
		KMSKeyID:   testKMSKeyARN,
		BucketKeys: true,
	}

	// The configuration under test must be one the loader would accept, or this test asserts the
	// mapping of a config that cannot exist.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the test configuration does not validate, so the mapping asserted below is of a "+
			"config no operator could load: %v", err)
	}

	a := &Adapter{config: cfg}
	got := a.buildS3Config()

	checks := []struct {
		field string
		got   any
		want  any
		why   string
	}{
		{field: "Region", got: got.Region, want: "eu-west-1"},
		{field: "Endpoint", got: got.Endpoint, want: "https://s3.example.invalid"},
		{field: "ForcePathStyle", got: got.ForcePathStyle, want: true},
		{field: "UseAccelerate", got: got.UseAccelerate, want: true},

		{
			field: "StorageTier",
			got:   got.StorageTier,
			want:  "GLACIER_IR",
			why: "unmapped, this arrived as \"\" and NewBackend substituted STANDARD — so every " +
				"object was written and billed as STANDARD whatever storage_tier said, with no error " +
				"and no log line",
		},
		{
			field: "MaxRetries",
			got:   got.MaxRetries,
			want:  7,
			why:   "the SDK's per-request attempt limit, passed to config.WithRetryMaxAttempts",
		},
		{
			field: "EnableCargoShipOptimization",
			got:   got.EnableCargoShipOptimization,
			want:  true,
			why: "mapped from use_cargoship. Plumbed only after the Content-Encoding corruption in " +
				"the CargoShip upload path was fixed: activating this first would have made a latent " +
				"defect live on every mount that set the key",
		},

		{
			field: "MultipartThreshold",
			got:   got.MultipartThreshold,
			want:  int64(48 * 1024 * 1024),
			why:   "48MB in bytes; binary units, as everywhere else in ObjectFS",
		},
		{
			field: "MultipartChunkSize",
			got:   got.MultipartChunkSize,
			want:  int64(12 * 1024 * 1024),
			why:   "12MB in bytes, and distinct from the threshold so a crossed mapping is visible",
		},
		{field: "MultipartConcurrency", got: got.MultipartConcurrency, want: 5},

		{
			field: "PoolSize",
			got:   got.PoolSize,
			want:  11,
			why: "mapped from performance.connection_pool_size. Unmapped, this reached GetObjects " +
				"and PutObjects as make(chan struct{}, 0) — an unbuffered semaphore whose first send " +
				"blocks forever — and set MaxIdleConnsPerHost to Go's default of 2",
		},

		{
			field: "ParallelReadThreshold",
			got:   got.ParallelReadThreshold,
			want:  int64(80 * 1024 * 1024),
			why: "the gate in GetObject is `threshold > 0`, so leaving this at zero made v0.10.0's " +
				"parallel range GETs unreachable from a mount",
		},
		{
			field: "ReadChunkSize",
			got:   got.ReadChunkSize,
			want:  int64(20 * 1024 * 1024),
		},

		{
			field: "ConnectTimeout",
			got:   got.ConnectTimeout,
			want:  3 * time.Second,
			why:   "becomes the dialer's Timeout in NewClientManager",
		},
		{
			field: "RequestTimeout",
			got:   got.RequestTimeout,
			want:  41 * time.Second,
			why: "becomes the transport's ResponseHeaderTimeout — the time S3 has to start " +
				"answering. Deliberately not a whole-response deadline, which would abort the ranged " +
				"reads of large objects that legitimately take minutes",
		},

		{field: "CongestionAlgorithm", got: got.CongestionAlgorithm, want: "bbr"},

		{field: "Compression.Enabled", got: got.Compression.Enabled, want: true},
		{
			field: "Compression.Algorithm",
			got:   got.Compression.Algorithm,
			want:  "lz4",
			why: "deliberately not zstd, which is the default this field would hold anyway if the " +
				"mapping dropped it",
		},
		{field: "Compression.Level", got: got.Compression.Level, want: 5},
		{
			field: "Compression.MinSize",
			got:   got.Compression.MinSize,
			want:  "8KB",
			why:   "carried as the string it was written as; the codec parses it",
		},

		{field: "CircuitBreaker.Enabled", got: got.CircuitBreaker.Enabled, want: true},
		{
			field: "CircuitBreaker.FailureThreshold",
			got:   got.CircuitBreaker.FailureThreshold,
			want:  13,
			why: "a count of failures per interval. It becomes a ReadyToTrip predicate in NewBackend " +
				"rather than a field, because circuit.Config has no threshold field — only that " +
				"predicate. MaxRequests is the half-open probe limit and is not this",
		},
		{field: "CircuitBreaker.Timeout", got: got.CircuitBreaker.Timeout, want: 45 * time.Second},

		{
			field: "Encryption.Mode",
			got:   got.Encryption.Mode,
			want:  s3.EncryptionModeKMS,
			why: "audit finding P-7. security.encryption was not mapped at all, so the backend saw " +
				"the zero value, sent no encryption header, and the mount came up reporting nothing " +
				"wrong — while the config file said at_rest: true and OBJECTFS.md documented a KMS ARN",
		},
		{
			field: "Encryption.KMSKeyID",
			got:   got.Encryption.KMSKeyID,
			want:  testKMSKeyARN,
			why: "the mode without the key is worse than neither: S3 accepts aws:kms with no key and " +
				"substitutes the AWS managed aws/s3 key, which is shared across the account and cannot " +
				"be audited or revoked separately from the data. NewBackend now refuses that config, " +
				"but only if the key reaches it",
		},
		{
			field: "Encryption.BucketKeys",
			got:   got.Encryption.BucketKeys,
			want:  true,
			why: "unmapped, an operator's KMS cost control silently did nothing — bucket keys cut " +
				"per-object KMS requests, which is the difference between one KMS call and one per object",
		},

		{field: "RetryConfig.MaxAttempts", got: got.RetryConfig.MaxAttempts, want: 9},
		{
			field: "RetryConfig.InitialDelay",
			got:   got.RetryConfig.InitialDelay,
			want:  250 * time.Millisecond,
			why:   "network.retry.base_delay; the name differs between the two structs",
		},
		{field: "RetryConfig.MaxDelay", got: got.RetryConfig.MaxDelay, want: 17 * time.Second},
	}

	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("buildS3Config().%s = %v, want %v. %s", c.field, c.got, c.want, c.why)
		}
	}

	// RetryableErrors is not configurable, and the mapping must not leave it empty. retry.New
	// backfills the attempt count and the delays but not this list, and shouldRetry consults it — so
	// a config mapped field-for-field into a bare retry.Config produces a retryer that reports nine
	// attempts and retries nothing. A connection reset would fail the operation on the first try.
	if len(got.RetryConfig.RetryableErrors) == 0 {
		t.Error("RetryConfig.RetryableErrors is empty, so shouldRetry matches nothing and the " +
			"retryer reports attempts it will never make")
	}
	if !reflect.DeepEqual(got.RetryConfig.RetryableErrors, retry.DefaultConfig().RetryableErrors) {
		t.Errorf("RetryConfig.RetryableErrors = %v, want the pkg/retry default %v — the mapping "+
			"seeds from DefaultConfig and overrides three fields, so this list should pass through "+
			"unchanged", got.RetryConfig.RetryableErrors, retry.DefaultConfig().RetryableErrors)
	}

	// Fields with no config key, asserted zero on purpose rather than left unmentioned. Each is a
	// decision recorded in buildS3Config, and an accidental mapping of any of them is a behavior
	// change nothing else would catch.
	if got.AccessKeyID != "" || got.SecretAccessKey != "" || got.SessionToken != "" {
		t.Error("credentials arrived from the config file; there are no YAML keys for them on " +
			"purpose, so that empty means the AWS default chain and a long-lived secret is not " +
			"invited into version control")
	}
	if !reflect.DeepEqual(got.TierConstraints, s3.TierConstraints{}) {
		t.Error("TierConstraints is non-zero; it overrides what AWS itself enforces for a tier, and " +
			"lowering a minimum object size produces writes S3 rejects")
	}
	if !reflect.DeepEqual(got.CostOptimization, s3.CostOptimization{}) {
		t.Error("CostOptimization is non-zero. internal/config.S3CostOptimization and " +
			"s3.CostOptimization share no field, and the backend's tiering machinery has no caller — " +
			"mapping them would wire a config block to an unreachable feature")
	}

	// write_buffer.max_memory, max_buffers and flush_interval have no reader: vfs.NewWriter takes no
	// configuration. network.timeouts.write likewise has no HTTP counterpart. Neither is asserted
	// here because there is nothing to assert against; both are marked in examples/config.yaml.
}

// TestBuildS3ConfigDisablesParallelReadsWhenTheConfigSaysSo pins the one field whose mapping is not
// a copy.
//
// The backend has a single way to express "parallel reads off": a threshold of zero, because its gate
// is `threshold > 0`. The config has a separate Enabled bool. Mapping the bool into its own backend
// field would give the backend two sources of truth and a way for them to disagree — enabled with a
// zero threshold, or disabled with a large one — so Enabled: false collapses to threshold zero here.
func TestBuildS3ConfigDisablesParallelReadsWhenTheConfigSaysSo(t *testing.T) {
	t.Parallel()

	cfg := createTestConfig()
	cfg.Performance.ParallelRead = config.ParallelReadConfig{
		// The sharp case: disabled, but with sizes still written in the file. An operator turns the
		// feature off by flipping one line, not by deleting the block, and the stale sizes must not
		// resurrect it.
		Enabled:   false,
		Threshold: "64MB",
		ChunkSize: "16MB",
	}

	got := (&Adapter{config: cfg}).buildS3Config()

	if got.ParallelReadThreshold != 0 {
		t.Errorf("ParallelReadThreshold = %d with parallel_read.enabled false; the backend's gate is "+
			"`threshold > 0`, so a non-zero value here fans out reads the config disabled",
			got.ParallelReadThreshold)
	}

	// ReadChunkSize is left at zero for NewBackend to default. It is not only the parallel-read chunk
	// size — GetObject's whole-object path uses it too — so zero here means "unset", and NewBackend
	// backfills it. Asserting zero rather than 16 MiB records that the chunk size is not the switch.
	if got.ReadChunkSize != 0 {
		t.Errorf("ReadChunkSize = %d; with parallel reads off this is left unset for NewBackend to "+
			"default, and the threshold alone carries the on/off decision", got.ReadChunkSize)
	}
}

// TestBuildS3ConfigOnDefaultsConstructsAViableBackendConfig checks the shipped defaults specifically.
//
// Two of the plan's stated gates are about the default path rather than a hand-built config: the
// mapping must not leave PoolSize at zero, because that deadlocks GetObjects and PutObjects, and it
// must not leave ParallelReadThreshold at zero, because that was v0.10.0's headline feature being
// dead code on every mount. Both are properties of what config.NewDefault produces, so they are
// asserted against it and not against a fixture chosen to make them pass.
func TestBuildS3ConfigOnDefaultsConstructsAViableBackendConfig(t *testing.T) {
	t.Parallel()

	cfg := config.NewDefault()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the shipped default configuration does not validate: %v", err)
	}

	got := (&Adapter{config: cfg}).buildS3Config()

	if got.PoolSize <= 0 {
		t.Errorf("PoolSize = %d on the default config. GetObjects and PutObjects build "+
			"make(chan struct{}, PoolSize); at zero that is unbuffered and the first send never "+
			"returns, so a batch operation hangs the calling goroutine", got.PoolSize)
	}

	if got.ParallelReadThreshold <= 0 {
		t.Errorf("ParallelReadThreshold = %d on the default config, which disables parallel range "+
			"GETs — the feature v0.10.0 was released for — on every stock mount",
			got.ParallelReadThreshold)
	}

	if got.ReadChunkSize <= 0 {
		t.Errorf("ReadChunkSize = %d with parallel reads enabled; the fan-out would divide by it",
			got.ReadChunkSize)
	}

	if got.MultipartChunkSize > got.MultipartThreshold {
		t.Errorf("MultipartChunkSize (%d) exceeds MultipartThreshold (%d), so an upload large enough "+
			"to be split would still be sent as a single part",
			got.MultipartChunkSize, got.MultipartThreshold)
	}

	if got.StorageTier == "" {
		t.Error("StorageTier is empty on the default config")
	}
	if got.ConnectTimeout <= 0 || got.RequestTimeout <= 0 {
		t.Errorf("timeouts are unset (connect %v, request %v); unset means the dialer has no "+
			"deadline and a connect to an unroutable address hangs until the kernel gives up",
			got.ConnectTimeout, got.RequestTimeout)
	}
	if got.MaxRetries <= 0 {
		t.Errorf("MaxRetries = %d on the default config", got.MaxRetries)
	}

	// The compression split between the two packages is deliberate and easy to "fix" by accident:
	// internal/config defaults compression off and s3.NewDefaultConfig defaults it on, because the
	// former serves a mount and the latter serves the Go SDK. Same for use_cargoship.
	if got.Compression.Enabled {
		t.Error("compression is on in the default mount configuration. It is opt-in: a zstd object " +
			"is not readable by `aws s3 cp` or boto3, which voids the implicit \"my data is just " +
			"objects in S3\" guarantee, so turning it on has to be a choice")
	}
}

// TestBuildRetryConfigKeepsTheDefaultRetryableErrors is the same property as the assertion inside
// TestBuildS3ConfigMapsEveryConfiguredValue, isolated so a failure names the cause directly.
func TestBuildRetryConfigKeepsTheDefaultRetryableErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   config.RetryConfig
		want retry.Config
	}{
		{
			name: "an empty block takes the pkg/retry defaults whole",
			in:   config.RetryConfig{},
			want: retry.DefaultConfig(),
		},
		{
			name: "a configured block overrides three fields and nothing else",
			in: config.RetryConfig{
				MaxAttempts: 4,
				BaseDelay:   50 * time.Millisecond,
				MaxDelay:    9 * time.Second,
			},
			want: func() retry.Config {
				c := retry.DefaultConfig()
				c.MaxAttempts = 4
				c.InitialDelay = 50 * time.Millisecond
				c.MaxDelay = 9 * time.Second

				return c
			}(),
		},
		{
			name: "a partially configured block leaves the rest at the defaults",
			in:   config.RetryConfig{MaxAttempts: 2},
			want: func() retry.Config {
				c := retry.DefaultConfig()
				c.MaxAttempts = 2

				return c
			}(),
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := createTestConfig()
			cfg.Network.Retry = tt.in

			got := (&Adapter{config: cfg}).buildRetryConfig()

			// Compared whole rather than field by field. The defect this guards is a field that
			// silently stays zero — Multiplier, Jitter, RetryableErrors — and enumerating the fields
			// to check is how the omission happened in the first place.
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildRetryConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestBuildWriterOptionsMapsTheWriteBufferBounds is #205's half of the same seam D12 lived in.
//
// write_buffer.max_memory and write_buffer.max_buffers were declared in internal/config, defaulted by
// NewDefault to "512MB" and 1000, and checked by validateSizes — and read by nothing. Every mount
// therefore reported a write-buffer ceiling it did not enforce, on the path that holds dirty byte
// ranges in memory until they are flushed.
//
// Values are written out rather than computed, for the reason the test above documents: deriving the
// expectation from the input would agree with a mapping that read max_buffers into max_memory.
func TestBuildWriterOptionsMapsTheWriteBufferBounds(t *testing.T) {
	t.Parallel()

	cfg := createTestConfig()
	cfg.WriteBuffer.MaxMemory = "256MB"
	cfg.WriteBuffer.MaxBuffers = 42

	got := (&Adapter{config: cfg}).buildWriterOptions()

	if got.MaxMemory != 268435456 {
		t.Errorf("buildWriterOptions().MaxMemory = %d, want 268435456 (\"256MB\"). A zero here is the "+
			"defect: an unbounded write path while the configuration reports a limit", got.MaxMemory)
	}
	if got.MaxBuffers != 42 {
		t.Errorf("buildWriterOptions().MaxBuffers = %d, want 42", got.MaxBuffers)
	}
}

// TestBuildWriterOptionsFallsBackToTheDocumentedDefault pins the empty-value path.
//
// An absent write_buffer block must yield the default NewDefault documents, not zero. Zero means
// unbounded in vfs.WriterOptions, so a partial config file — the shape a partial config is meant to
// have — would silently restore the very defect #205 fixed.
func TestBuildWriterOptionsFallsBackToTheDocumentedDefault(t *testing.T) {
	t.Parallel()

	cfg := createTestConfig()
	cfg.WriteBuffer.MaxMemory = ""

	got := (&Adapter{config: cfg}).buildWriterOptions()

	if got.MaxMemory != defaultWriteBufferMemory {
		t.Errorf("buildWriterOptions().MaxMemory = %d for an empty max_memory, want %d. Zero would "+
			"mean unbounded, which is what a config file omitting the block must not get",
			got.MaxMemory, defaultWriteBufferMemory)
	}
}

// TestDefaultConfigProducesABoundedWritePath is the end-to-end statement of #205.
//
// Not a mapping assertion: this one asks whether the *shipped defaults* bound the write path. It would
// have failed for every release through v0.10.3, at the only configuration most users ever run.
func TestDefaultConfigProducesABoundedWritePath(t *testing.T) {
	t.Parallel()

	got := (&Adapter{config: config.NewDefault()}).buildWriterOptions()

	if got.MaxMemory <= 0 {
		t.Errorf("the default configuration produces MaxMemory=%d, an unbounded write path. "+
			"config.NewDefault sets write_buffer.max_memory to \"512MB\"; a mount that does not "+
			"enforce it is the defect #205 exists to close", got.MaxMemory)
	}
	if got.MaxBuffers <= 0 {
		t.Errorf("the default configuration produces MaxBuffers=%d, an unbounded node count", got.MaxBuffers)
	}
}
