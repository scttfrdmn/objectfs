package s3

import (
	"math"
	"time"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/circuit"
	"github.com/scttfrdmn/objectfs/pkg/retry"
)

// Parallel-read fallbacks, applied when a Config reaches [Backend.parallelGetObject] with these
// fields unset.
//
// They are named rather than inlined at the point of use because NewBackend's defaulting and the
// read path's own floors are two places that must agree about the same number: a chunk size that
// differs between them changes how many GETs a read issues, which is the property
// TestParallelReadThresholdDrivesFanOut asserts. ParallelReadThreshold is deliberately not among
// them — zero is how this package spells "parallel reads off".
const (
	// defaultReadChunkSize is the bytes per range GET, matching MultipartChunkSize so a read fans
	// out along the same boundaries a multipart write used.
	defaultReadChunkSize = 16 * 1024 * 1024

	// defaultParallelReadConcurrency is the fan-out width used when neither
	// ParallelReadConcurrency nor MultipartConcurrency is set. It matches the default PoolSize:
	// each concurrent chunk holds a pooled client for the length of its transfer, so a wider
	// fan-out than the pool would spend its extra width waiting on [ConnectionPool.Get].
	defaultParallelReadConcurrency = 8
)

// Config represents S3 backend configuration
type Config struct {
	Region          string `yaml:"region"`
	Endpoint        string `yaml:"endpoint"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	SessionToken    string `yaml:"session_token"`
	ForcePathStyle  bool   `yaml:"force_path_style"`

	// Performance settings
	MaxRetries     int           `yaml:"max_retries"`
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	PoolSize       int           `yaml:"pool_size"`

	// Retry configuration
	RetryConfig retry.Config `yaml:"retry_config"`

	// CircuitBreaker controls the breaker that fronts S3 operations.
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`

	// Advanced settings
	UseAccelerate bool `yaml:"use_accelerate"`
	UseDualStack  bool `yaml:"use_dual_stack"`

	// AccelerationRetry is how long the fallback stays in effect before one request is allowed to
	// try the accelerate endpoint again. Zero means the default below.
	//
	// The fallback used to be permanent — one acceleration error and every later request in the
	// mount's life used the standard endpoint (#204). The argument for that was sound as far as it
	// went: the usual trigger is a bucket without the Transfer Acceleration configuration, which
	// does not resolve on its own, so retrying it per request would pay a failed round trip forever.
	// What it missed is that the trigger set is not only that one. A DNS or TLS failure reaching
	// <bucket>.s3-accelerate.amazonaws.com matches too, and so does a transient InvalidRequest; those
	// do resolve, and a mount that lost acceleration to a thirty-second network fault kept paying for
	// it for weeks.
	//
	// A period rather than a per-request retry is what makes both cases affordable. At the default,
	// the unrecoverable case costs one failed request every five minutes — a rounding error against
	// the request rate of any mount doing enough traffic to want acceleration — and the recoverable
	// case comes back on its own within one period. See newAccelerationGate for the state machine;
	// it is [circuit.CircuitBreaker], not a second bespoke one.
	AccelerationRetry time.Duration `yaml:"acceleration_retry"`

	// Multipart upload configuration
	MultipartThreshold   int64 `yaml:"multipart_threshold"`   // Size threshold for multipart uploads (bytes)
	MultipartChunkSize   int64 `yaml:"multipart_chunk_size"`  // Chunk size for multipart uploads (bytes)
	MultipartConcurrency int   `yaml:"multipart_concurrency"` // Number of concurrent part uploads

	// Parallel read configuration — fan out large object reads into concurrent range GETs.
	ParallelReadThreshold   int64 `yaml:"parallel_read_threshold"`   // bytes; 0 = disabled
	ReadChunkSize           int64 `yaml:"read_chunk_size"`           // bytes per range GET
	ParallelReadConcurrency int   `yaml:"parallel_read_concurrency"` // 0 = MultipartConcurrency

	// S3 Storage Tier Configuration
	StorageTier      string           `yaml:"storage_tier"`      // "STANDARD", "STANDARD_IA", "ONEZONE_IA", etc.
	TierConstraints  TierConstraints  `yaml:"tier_constraints"`  // Tier-specific constraints
	CostOptimization CostOptimization `yaml:"cost_optimization"` // Cost optimization settings
	PricingConfig    PricingConfig    `yaml:"pricing_config"`    // Custom pricing configuration

	// Transparent object compression configuration
	Compression CompressionConfig `yaml:"compression"`

	// Server-side encryption applied to every object this backend writes.
	Encryption EncryptionConfig `yaml:"encryption"`

	// CongestionAlgorithm is the TCP congestion control algorithm to request
	// for each S3 connection: "auto" (detect best), "bbr", "cubic", "reno".
	// On non-Linux platforms the value is silently ignored.
	// Default: "auto".
	CongestionAlgorithm string `yaml:"congestion_algorithm"`
}

// CompressionConfig defines transparent compression settings for S3 objects.
// When Enabled is true, objects are compressed on upload (PutObject) and
// decompressed on download (GetObject) using the configured algorithm.
type CompressionConfig struct {
	// Enabled turns transparent S3 compression on or off.
	Enabled bool `yaml:"enabled"`
	// Algorithm selects the codec: "none", "zstd" (recommended), "lz4", or "gzip".
	// The authoritative list is pkg/compression.SupportedAlgorithms; this comment
	// is a convenience, and a value it disagrees with is a bug in the comment.
	Algorithm string `yaml:"algorithm"`
	// Level is the codec-specific compression level (0 = the codec's default).
	// The valid range differs per algorithm: zstd accepts 0-22 (3 is a good
	// default), gzip only 0-9. A level valid for one is often invalid for the
	// other, so changing Algorithm may require changing Level.
	Level int `yaml:"level"`
	// MinSize is the minimum object size to compress (e.g. "4KB").
	// Objects smaller than MinSize are stored uncompressed.
	MinSize string `yaml:"min_size"`
}

// Server-side encryption modes, as the values the `mode` config key accepts.
//
// Aliases of the awsname constants, exactly as the Tier* constants in tiers.go are: the mode is read
// by internal/config and acted on here, and config cannot import this package. One authority for the
// set of modes that exist, in a package both sides can reach. See [awsname.SSEModeOff] and its
// siblings for what each mode does and costs.
const (
	EncryptionModeOff = awsname.SSEModeOff
	EncryptionModeS3  = awsname.SSEModeS3
	EncryptionModeKMS = awsname.SSEModeKMS
)

// EncryptionConfig defines the server-side encryption ObjectFS requests on every object it writes.
//
// It exists because v0.10.0 shipped a `security.encryption.at_rest` key that defaulted to **true**
// and was read by nothing: a grep for ServerSideEncryption, SSEKMS, or aws:kms across the tree
// returned zero non-test hits, while OBJECTFS.md documented a `kms_key:` ARN (audit finding P-7). A
// configuration key that claims a security property and sets no header is worse than an absent
// feature, because an operator who reads it stops looking — and the thing they stopped looking for
// is the one an auditor will ask about.
//
// The mode is the whole of the decision and there is no separate boolean, deliberately. Two switches
// where one will do is how `at_rest: true` came to coexist with no header: a bool cannot say which
// of the three things a reader might mean, so it says the one that sounds safest.
type EncryptionConfig struct {
	// Mode selects the encryption to request: "off", "sse-s3", or "sse-kms". Empty means "off".
	Mode string `yaml:"mode"`

	// KMSKeyID is the key SSE-KMS encrypts with — a key ID, an alias, or a full ARN. Required when
	// Mode is "sse-kms" and rejected otherwise, rather than ignored: a key set beside a mode that
	// does not use it means the two disagree about what is being asked for, and silently honoring
	// the mode is how a configuration comes to name a KMS key and encrypt with something else.
	KMSKeyID string `yaml:"kms_key_id"`

	// BucketKeys requests S3 Bucket Keys, which reduce SSE-KMS's per-object KMS calls by up to 99%
	// by deriving a bucket-level key. Recommended with "sse-kms" and meaningless without it.
	//
	// This is a cost and throughput control rather than a security one, and it is the difference
	// between SSE-KMS being usable for a filesystem workload and not: without it, every object read
	// is a billed KMS Decrypt against a per-region rate limit, so a directory traversal can be
	// throttled by KMS while S3 is entirely idle.
	BucketKeys bool `yaml:"bucket_keys"`
}

// Enabled reports whether any encryption header should be sent.
func (e EncryptionConfig) Enabled() bool {
	return e.Mode != "" && e.Mode != EncryptionModeOff
}

// CircuitBreakerConfig defines the breaker that fronts S3 operations.
//
// Plain data rather than a circuit.Config, deliberately. circuit.Config expresses the trip decision
// as a ReadyToTrip predicate — a func field, which is the right shape for the breaker and the wrong
// shape for configuration: it cannot be compared, printed usefully, round-tripped through YAML, or
// carried through the config fuzzer's %#v dedup key. NewBackend turns these three values into that
// predicate, so the translation lives in one place and the config stays a value.
type CircuitBreakerConfig struct {
	// Enabled false means the breaker never opens. It stays in the call path counting and reporting
	// state; it just never rejects. That is not the same as removing it, and removing it is not an
	// option this config offers — a bypass would be a second code path through every S3 operation
	// with no test coverage.
	Enabled bool `yaml:"enabled"`

	// FailureThreshold is the number of failures within one Interval that opens the breaker. Zero
	// means the package default, which is proportional rather than absolute: at least 20 requests in
	// the interval with half of them failing.
	//
	// A failure here is what circuit.defaultIsSuccessful calls one — a service failure, per
	// errors.IsServiceFailure. A missing object is an answer, not an outage, and does not count.
	//
	// Values above maxFailureThreshold are clamped rather than honored; see readyToTrip.
	FailureThreshold int `yaml:"failure_threshold"`

	// Timeout is how long the breaker stays open before admitting probe requests. Zero means 30s.
	Timeout time.Duration `yaml:"timeout"`
}

// maxFailureThreshold is the largest failure count this config can express.
//
// It is math.MaxUint32 because circuit.Counts.TotalFailures is a uint32, so a threshold above it can
// never be reached. On a 64-bit platform `int` holds larger values, and readyToTrip clamps rather
// than narrowing them — see there for what the narrowing did.
//
// Typed int64 rather than untyped, because this package cross-builds for linux/386 and linux/arm,
// where `int` is 32 bits and comparing it against an untyped 4294967295 does not compile
// ("constant overflows int"). int64 holds every int on every target objectfs builds for, so the
// comparison in readyToTrip is exact on all of them.
const maxFailureThreshold int64 = math.MaxUint32

// readyToTrip turns a CircuitBreakerConfig into the predicate circuit.Config wants.
//
// Three cases, and the middle one is why this is a function rather than a field assignment:
//
//   - disabled: a predicate that never trips. See CircuitBreakerConfig.Enabled.
//   - a positive threshold: an absolute count of service failures in the interval.
//   - zero: nil, which NewCircuitBreaker replaces with its proportional default. Returning a
//     `failures >= 0` closure instead would open the breaker before the first request and keep every
//     S3 operation rejected for the life of the mount.
//
// The clamp is the fourth case, and it exists because the `<= 0` guard above does not bound the
// conversion the way the suppression it used to carry claimed. That comment read "guarded positive
// above", which is true and insufficient: `int` is 64-bit here and circuit.Counts.TotalFailures is a
// uint32, so a positive threshold can still wrap. failure_threshold: 4294967296 passed the guard,
// narrowed to 0, and made the predicate `TotalFailures >= 0` — always true, so the breaker opened
// before the first request and rejected every S3 operation for the life of the mount. That is
// precisely the outcome the zero case above is written to avoid, arriving by the one path that
// bypassed it. 4294967297 gave a threshold of 1, which is worse: not obviously broken, just wrong.
//
// Clamping rather than erroring keeps this a total function, which is what lets it be a value
// translation rather than a second validation site. internal/config rejects a negative threshold at
// load; an absurdly large one is not invalid, only unreachable, and the reachable maximum is the
// honest reading of it.
func readyToTrip(cfg CircuitBreakerConfig) func(circuit.Counts) bool {
	if !cfg.Enabled {
		return func(circuit.Counts) bool { return false }
	}

	if cfg.FailureThreshold <= 0 {
		return nil
	}

	if int64(cfg.FailureThreshold) > maxFailureThreshold {
		return func(circuit.Counts) bool { return false }
	}

	threshold := uint32(cfg.FailureThreshold) // #nosec G115 -- bounded by both guards above

	return func(counts circuit.Counts) bool {
		return counts.TotalFailures >= threshold
	}
}

// GetOptimalChunkSize returns the optimal chunk size for a given file size
func (c *Config) GetOptimalChunkSize(fileSize int64) int64 {
	return CalculateOptimalChunkSize(fileSize, c.MultipartThreshold, c.MultipartChunkSize)
}

// ShouldUseMultipart determines if a file should use multipart upload
func (c *Config) ShouldUseMultipart(fileSize int64) bool {
	return fileSize > c.MultipartThreshold
}

// TierConstraints defines tier-specific constraints and limitations
type TierConstraints struct {
	MinObjectSize      int64         `yaml:"min_object_size"`      // Minimum object size in bytes
	DeletionEmbargo    time.Duration `yaml:"deletion_embargo"`     // Minimum storage duration before deletion
	RetrievalLatency   string        `yaml:"retrieval_latency"`    // Expected retrieval latency ("instant", "minutes", "hours")
	RetrievalCost      bool          `yaml:"retrieval_cost"`       // Whether retrieval incurs additional charges
	MinimumStorageDays int           `yaml:"minimum_storage_days"` // Minimum billable storage period
	TransitionDelay    time.Duration `yaml:"transition_delay"`     // Delay before transitioning to this tier
}

// CostOptimization defines cost optimization settings.
//
// Three fields, where there were six. `TransitionRules []TransitionRule`, `LifecycleManagement` and
// `IntelligentTiering` were removed rather than plumbed: no code read any of the three, and the two
// bools were additionally misleading — S3 lifecycle rules and Intelligent-Tiering configuration are
// bucket-level settings applied with PutBucketLifecycleConfiguration and
// PutBucketIntelligentTieringConfiguration, neither of which this backend calls, so a `true` there
// described something ObjectFS has no code to do. `TransitionRules` was the reason
// internal/config.S3CostOptimization could not be mapped onto this type at all: there is no reading
// of `transition_to_ia: 30` that produces a []TransitionRule without inventing the rest of a rule
// (#203).
//
// What survives is separated by who can reach it, because that turned out to be the whole question:
// one field is read on the write path of every mount, and two gate an analyzer that only a caller
// holding a *Backend can invoke.
type CostOptimization struct {
	// SmallObjectsOnStandard stores an object on STANDARD when the configured tier would bill it as
	// larger than it is. Read on the PutObject path, and the one field in this struct a mount
	// configuration maps (`storage.s3.cost_optimization.small_objects_on_standard`).
	//
	// It changes the storage class of stored objects, which is why it is its own field and its own
	// key. This behavior used to be gated by MonitorAccessPatterns — a name that says the process
	// observes something, attached to a switch that silently rewrote the storage class of every
	// object under 128 KiB. See [CostOptimizer.HandleStandardTierOverhead] for which tiers it applies
	// to and why "under 128 KiB" was the wrong test.
	SmallObjectsOnStandard bool `yaml:"small_objects_on_standard"`

	// EnableAutoTiering lets [CostOptimizer.AnalyzeAndOptimize] issue the CopyObject calls that move
	// objects between tiers. Off by default and deliberately not mapped from a mount configuration:
	// nothing on the mount path calls AnalyzeAndOptimize, so a YAML key for it would configure a
	// feature no mount can invoke. It is reachable through [Backend.OptimizeStorageCosts] for callers
	// using this package as a library.
	EnableAutoTiering bool `yaml:"enable_auto_tiering"`

	// CostThreshold is the monthly saving in dollars below which a suggested transition is not worth
	// making. Read only by the analyzer above, so it is unmapped for the same reason.
	CostThreshold float64 `yaml:"cost_threshold"`

	// MonitorAccessPatterns records an access pattern per object key on every read, which is what
	// gives [Backend.GetCostOptimizationReport] something to report.
	//
	// Also unmapped, and this one for a reason that is not just reachability: the map it fills holds
	// one entry per distinct key read and nothing ever evicts from it, so on a bucket with many
	// objects it grows for the life of the mount. A key that trades unbounded memory for a report no
	// mount can display is not a key worth offering. Library callers that want the report accept that
	// trade knowingly.
	MonitorAccessPatterns bool `yaml:"monitor_access_patterns"`
}

// PricingConfig defines custom pricing configuration for S3 costs
type PricingConfig struct {
	// Deprecated: the AWS Pricing API integration was removed in v0.10.1 — it
	// downloaded the ~100 MB S3 offer index and then returned hardcoded
	// us-east-1 constants for every tier. Setting this now logs a warning and
	// has no other effect. Use CustomPricing for exact or negotiated rates.
	UsePricingAPI      bool                   `yaml:"use_pricing_api"`
	Region             string                 `yaml:"region"`               // Pricing region (may differ from bucket region)
	CustomPricing      map[string]TierPricing `yaml:"custom_pricing"`       // Override pricing per tier
	DiscountConfig     DiscountConfig         `yaml:"discount_config"`      // Volume discounts and enterprise rates
	DiscountConfigFile string                 `yaml:"discount_config_file"` // Path to external discount config file
	AdditionalCosts    AdditionalCosts        `yaml:"additional_costs"`     // Request costs, data transfer, etc.
	LastUpdated        string                 `yaml:"last_updated"`         // When pricing was last updated
	Currency           string                 `yaml:"currency"`             // USD, EUR, etc.
}

// TierPricing defines pricing for a specific storage tier
type TierPricing struct {
	StorageCostPerGBMonth float64      `yaml:"storage_cost_per_gb_month"` // $/GB/month for storage
	RetrievalCostPerGB    float64      `yaml:"retrieval_cost_per_gb"`     // $/GB for retrieval
	RequestCosts          RequestCosts `yaml:"request_costs"`             // Per-request pricing

	// MinimumBillableSize is the size an object smaller than it is billed as. Zero for the five
	// classes AWS publishes no minimum for — see [StorageTierInfo.MinObjectSize], which this mirrors.
	MinimumBillableSize int64 `yaml:"minimum_billable_size"`

	// PerObjectOverheadBytes is billed *in addition* to the object, not instead of a smaller size.
	// Only the two archive classes have it. It is a separate field from MinimumBillableSize because
	// the two are arithmetically opposite: a minimum replaces the size, an overhead adds to it. Both
	// were previously carried in MinimumBillableSize, which made the cost of a 10 KB DEEP_ARCHIVE
	// object 40 KB when it is 50 KB, and made compressing it look free of benefit when it is not.
	PerObjectOverheadBytes int64 `yaml:"per_object_overhead_bytes"`

	// OverheadStandardRateBytes is the portion of PerObjectOverheadBytes billed at the S3 Standard
	// rate rather than at this tier's rate — 8 KB of the archive classes' 40 KB. Priced at the tier
	// rate it would be understated about 23-fold on DEEP_ARCHIVE.
	OverheadStandardRateBytes int64 `yaml:"overhead_standard_rate_bytes"`

	MinimumBillableDays int                `yaml:"minimum_billable_days"` // Minimum billable period
	TransitionCosts     map[string]float64 `yaml:"transition_costs"`      // Cost to transition to other tiers
}

// RequestCosts defines per-request pricing
type RequestCosts struct {
	PutRequestCost    float64 `yaml:"put_request_cost"`    // Cost per PUT request
	GetRequestCost    float64 `yaml:"get_request_cost"`    // Cost per GET request
	DeleteRequestCost float64 `yaml:"delete_request_cost"` // Cost per DELETE request
	ListRequestCost   float64 `yaml:"list_request_cost"`   // Cost per LIST request
	HeadRequestCost   float64 `yaml:"head_request_cost"`   // Cost per HEAD request
}

// DiscountConfig defines volume discounts and enterprise pricing
type DiscountConfig struct {
	EnableVolumeDiscounts    bool               `yaml:"enable_volume_discounts"`    // Enable volume-based discounts
	VolumeTiers              []VolumeTier       `yaml:"volume_tiers"`               // Volume discount tiers
	EnterpriseDiscount       float64            `yaml:"enterprise_discount"`        // Overall enterprise discount (%)
	ReservedCapacityDiscount float64            `yaml:"reserved_capacity_discount"` // Reserved capacity discount (%)
	SpotDiscount             float64            `yaml:"spot_discount"`              // Spot pricing discount (%)
	CustomDiscounts          map[string]float64 `yaml:"custom_discounts"`           // Custom discounts per tier
}

// VolumeTier defines volume-based discount tiers
type VolumeTier struct {
	MinSizeGB       float64  `yaml:"min_size_gb"`      // Minimum size for this tier
	MaxSizeGB       float64  `yaml:"max_size_gb"`      // Maximum size for this tier (-1 = unlimited)
	DiscountPercent float64  `yaml:"discount_percent"` // Discount percentage for this tier
	AppliesTo       []string `yaml:"applies_to"`       // Which storage tiers this applies to
}

// AdditionalCosts defines additional cost factors
type AdditionalCosts struct {
	DataTransferOut   DataTransferPricing `yaml:"data_transfer_out"`  // Data transfer out costs
	ReplicationCosts  ReplicationPricing  `yaml:"replication_costs"`  // Cross-region replication
	CloudWatchMetrics float64             `yaml:"cloudwatch_metrics"` // CloudWatch metrics cost per metric
	InventoryReports  float64             `yaml:"inventory_reports"`  // S3 Inventory cost per object
	AccessLogging     float64             `yaml:"access_logging"`     // Access logging cost per request
}

// DataTransferPricing defines data transfer cost structure
type DataTransferPricing struct {
	FirstTBPerGB  float64 `yaml:"first_tb_per_gb"`  // First TB pricing
	Next9TBPerGB  float64 `yaml:"next_9tb_per_gb"`  // 1-10 TB pricing
	Next40TBPerGB float64 `yaml:"next_40tb_per_gb"` // 10-50 TB pricing
	Over50TBPerGB float64 `yaml:"over_50tb_per_gb"` // >50 TB pricing
}

// ReplicationPricing defines cross-region replication costs
type ReplicationPricing struct {
	ReplicationPerGB       float64 `yaml:"replication_per_gb"`       // Cost per GB replicated
	DestinationPutRequests float64 `yaml:"destination_put_requests"` // PUT request cost at destination
}

// s3MinPartSize is the minimum non-last part size enforced by S3.
// Parts smaller than 5 MB (except the final part) are rejected with EntityTooSmall.
const s3MinPartSize = 5 * 1024 * 1024 // 5 MB

// CalculateOptimalChunkSize calculates the optimal chunk size based on file size and network conditions
func CalculateOptimalChunkSize(fileSize int64, multipartThreshold int64, baseChunkSize int64) int64 {
	// If file is below multipart threshold, use full file as single chunk
	if fileSize <= multipartThreshold {
		return fileSize
	}

	var chunkSize int64

	// For very small files just over threshold, use smaller chunks
	if fileSize < multipartThreshold*2 {
		chunkSize = baseChunkSize / 2
	} else if fileSize < 1024*1024*1024 {
		// For medium files (up to 1GB), use base chunk size
		chunkSize = baseChunkSize // 16MB for default config
	} else if fileSize < 10*1024*1024*1024 {
		// For large files (1GB - 10GB), use larger chunks
		chunkSize = baseChunkSize * 2 // 32MB
	} else if fileSize < 100*1024*1024*1024 {
		// For very large files (10GB+), use even larger chunks
		chunkSize = baseChunkSize * 4 // 64MB
	} else {
		// For massive files (100GB+), use maximum practical chunk size
		// S3 allows up to 5GB per part, but 128MB is a good balance
		chunkSize = baseChunkSize * 8 // 128MB
	}

	// Enforce S3 minimum part size: all non-last parts must be >= 5 MB.
	if chunkSize < s3MinPartSize {
		chunkSize = s3MinPartSize
	}

	return chunkSize
}

// CalculatePartCount calculates the number of parts for a multipart upload
func CalculatePartCount(fileSize int64, chunkSize int64) int {
	if chunkSize == 0 {
		return 0
	}
	partCount := int(fileSize / chunkSize)
	if fileSize%chunkSize != 0 {
		partCount++
	}
	return partCount
}

// NewDefaultConfig returns a configuration with sensible defaults
func NewDefaultConfig() *Config {
	retryConfig := retry.DefaultConfig()
	retryConfig.MaxAttempts = 3
	retryConfig.InitialDelay = 100 * time.Millisecond
	retryConfig.MaxDelay = 30 * time.Second

	return &Config{
		MaxRetries:     3,
		ConnectTimeout: 10 * time.Second,
		RequestTimeout: 30 * time.Second,
		PoolSize:       8,
		RetryConfig:    retryConfig,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: true,
			// Zero: the package's proportional default. An absolute count has no defensible
			// value without knowing the request rate — ten failures is a broken bucket at 1 rps
			// and a rounding error at 1000.
			FailureThreshold: 0,
			Timeout:          30 * time.Second,
		},
		// Not UseAccelerate — that stays false — but the period that governs it once an operator turns
		// it on. Named here rather than left to newAccelerationGate's fallback so that a backend built
		// from this config reports the period it will actually use.
		AccelerationRetry:       defaultAccelerationRetry,
		MultipartThreshold:      32 * 1024 * 1024, // 32MB - trigger multipart for larger files
		MultipartChunkSize:      16 * 1024 * 1024, // 16MB - optimal chunk size for performance
		MultipartConcurrency:    8,                // Match pool size for concurrent uploads
		ParallelReadThreshold:   64 * 1024 * 1024, // 64MB - fan out reads above this size
		ReadChunkSize:           defaultReadChunkSize,
		ParallelReadConcurrency: 0,                 // 0 = inherit MultipartConcurrency
		StorageTier:             TierStandard,      // Default to Standard tier
		TierConstraints:         TierConstraints{}, // Use tier defaults
		Compression: CompressionConfig{
			Enabled:   true,
			Algorithm: "zstd",
			Level:     3,     // fast level with good ratio (~60% savings for text/JSON)
			MinSize:   "4KB", // skip compression for tiny objects
		},
		CongestionAlgorithm: "auto",
		// Every cost-optimization setting off. Two of them change what is stored or how much memory
		// the process holds, so a default that is on would be a decision made for the operator.
		CostOptimization: CostOptimization{
			SmallObjectsOnStandard: false,
			EnableAutoTiering:      false,
			MonitorAccessPatterns:  false,
		},
		PricingConfig: PricingConfig{
			UsePricingAPI: false,
			Region:        "us-east-1",
			Currency:      "USD",
			CustomPricing: make(map[string]TierPricing),
			DiscountConfig: DiscountConfig{
				EnableVolumeDiscounts: false,
				VolumeTiers:           []VolumeTier{},
				CustomDiscounts:       make(map[string]float64),
			},
		},
	}
}
