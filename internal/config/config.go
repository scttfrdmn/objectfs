package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/compression"
	"github.com/scttfrdmn/objectfs/pkg/utils"
)

// Boolean Constants
const (
	TrueValue = "true"
)

// Default bind addresses for the two monitoring listeners.
//
// Loopback, not the wildcard. Both endpoints are on by default, so a stock `objectfs s3://bucket /mnt`
// used to open two unauthenticated ports to every host that could route to it (#211); neither serves
// object contents, but between them they report per-operation counts, error rates, sizes and timings.
// Loopback keeps a same-host Prometheus scrape working and makes a cross-host one explicit.
//
// The ports are unchanged from the global.metrics_port and global.health_port these replaced, so an
// existing same-host scrape keeps working across the upgrade.
const (
	DefaultMetricsAddr = "127.0.0.1:8080"
	DefaultHealthAddr  = "127.0.0.1:8081"
)

// Configuration represents the complete application configuration
type Configuration struct {
	Global      GlobalConfig      `yaml:"global"`
	Mount       MountConfig       `yaml:"mount"`
	Storage     StorageConfig     `yaml:"storage"`
	Performance PerformanceConfig `yaml:"performance"`
	Cache       CacheConfig       `yaml:"cache"`
	WriteBuffer WriteBufferConfig `yaml:"write_buffer"`
	Network     NetworkConfig     `yaml:"network"`
	Security    SecurityConfig    `yaml:"security"`
	Monitoring  MonitoringConfig  `yaml:"monitoring"`
	Features    FeatureConfig     `yaml:"features"`
	Cluster     ClusterConfig     `yaml:"cluster"`
	FUSE        FUSEConfig        `yaml:"fuse"`
}

// FUSEConfig configures how the kernel is asked to cache and dispatch to this mount.
//
// This block is new in v0.11.0 and it is the first FUSE section any loader has ever read. Nine
// fields on internal/fuse.MountOptions and internal/fuse.Config carried yaml tags for a whole
// release and were decoded by nothing — Configuration had no `fuse` key, so a `fuse:` block in a
// config file was silently discarded, and under strict decoding it would now be rejected outright.
// They are the fields #180 removed.
//
// Only settings with a demonstrated effect on the kernel's behavior are here, which is why the block
// is three keys and not nine. Two of the four #180 nominated turned out not to be plumbable, and the
// evidence is recorded at the field that would have carried them — see DirectIO's comment for
// splice, and KeepCache's for the writeback cache.
//
// All three default to false, which is both the kernel's behavior in the absence of the flag and
// ObjectFS's behavior before this block existed. That is why NewDefault does not name this section:
// the zero value is the default, so a Configuration built as a literal and one built by NewDefault
// cannot describe different mounts. Each field is also named so that false is the uninteresting
// value — SyncRead rather than AsyncRead, for one — since a negated name would have made the zero
// value the surprising one.
type FUSEConfig struct {
	// DirectIO asks the kernel not to cache this mount's file data: FOPEN_DIRECT_IO on every open.
	//
	// Off by default. It costs the page cache — every read(2) becomes a READ request into
	// FileHandle.Read even when the same bytes were read a microsecond ago — and buys coherence with a
	// bucket other clients are writing. The internal byte-range cache still serves those reads, so
	// what is given up is the kernel's copy, not all caching. It also makes reads land at the offset
	// and length the application asked for rather than page-aligned and rounded up, which is what
	// makes it the setting to reach for when a reader does large strided reads of a huge object.
	//
	// `splice_read`, the fourth flag #180 named, is not here and is not plumbable as things stand.
	// go-fuse only splices a ReadResult that carries a file descriptor (fuse.ReadResultFd, via the
	// seekableResult/statefulResult interfaces in fuse/read.go); FileHandle.Read returns
	// fuse.ReadResultData at all four of its return sites, because the bytes come from S3 or from an
	// in-memory cache and there is no fd to splice from. trySplice returns errRecoverSplice for such
	// a result and go-fuse copies instead. Setting DisableSplice would therefore disable a path this
	// filesystem never takes — a config key whose effect is provably nothing, which is the defect
	// #180 exists to stop repeating.
	DirectIO bool `yaml:"direct_io"`

	// KeepCache asks the kernel to retain cached pages across open(2): FOPEN_KEEP_CACHE.
	//
	// Off by default, so an open(2) drops whatever the page cache held for that file and the next
	// read comes to this filesystem. Turning it on trades that for speed on a workload that opens the
	// same file repeatedly — and it is only safe when nothing else writes the bucket, because a page
	// the kernel keeps is a page ObjectFS cannot invalidate: see the writeback-cache note below for
	// why there is no notification path to invalidate it with.
	//
	// Ignored when DirectIO is set. The two are contradictory — one asks the kernel to cache harder
	// and the other asks it not to cache at all — and go-fuse's own bridge clears FOPEN_KEEP_CACHE
	// when it takes a passthrough path (fs/bridge.go:805). Which one wins is decided in internal/fuse
	// rather than left to the kernel; see FileNode.Open.
	//
	// `writeback_cache`, also named in #180, is not here. It maps to
	// fuse.MountOptions.ExplicitDataCacheControl, which asks the kernel *not* to invalidate data
	// caches automatically and makes the filesystem "fully responsible for invalidating data cache"
	// (go-fuse's own words). ObjectFS makes no invalidation calls at all: there is not one
	// NotifyContent, NotifyEntry, or NotifyInvalInode in the tree. Enabling it would hand the mount a
	// responsibility nothing discharges and turn stale pages into a permanent condition rather than
	// one bounded by cache.ttl. It needs the notification path first, tracked separately.
	KeepCache bool `yaml:"keep_cache"`

	// SyncRead makes the kernel keep at most one READ outstanding against a file.
	//
	// Off by default, so reads are asynchronous — the kernel's default and ObjectFS's. It reaches
	// go-fuse's SyncRead field, which go-fuse turns into a disabled CAP_ASYNC_READ at INIT
	// (fuse/server.go:187). #180 proposed exposing this as `async_read`, the name the removed field
	// had; it is `sync_read` instead, because AsyncRead is the negation of the flag that exists and a
	// negated key would make `async_read: false` — the value a half-filled config file produces — the
	// one that serializes every read on the mount.
	//
	// Turning it on costs read throughput on any sequential reader: kernel readahead is exactly the
	// mechanism CAP_ASYNC_READ enables. It is here for a backend that cannot serve concurrent reads of
	// one file. S3 can.
	SyncRead bool `yaml:"sync_read"`
}

// GlobalConfig represents global application settings.
//
// Its three port keys are gone as of v0.11.0, replaced by an address per listener beside that
// listener's own `enabled` flag (#202, #211, #212):
//
//	global.metrics_port  →  monitoring.metrics.addr
//	global.health_port   →  monitoring.health_checks.addr
//	global.profile_port  →  removed outright
//
// A port and an address were never two settings. `monitoring` already declared `metrics_addr` and
// `health_check_addr`, defaulted them, documented them — and read neither, while the ports two
// sections away were what the listeners used. So an operator who set `health_check_addr:
// 127.0.0.1:8081` to keep a diagnostic endpoint off the network got a wildcard bind and no warning:
// the setting that would have changed it was inert and the setting that was live could not express a
// host at all.
//
// An address subsumes a port, so keeping both would have preserved the disagreement. It also settles
// what a port could not: `health_port: 0` disabled the health endpoint while `metrics_port: 0`
// defaulted back to 8080 and bound it, so the two adjacent fields spelled "off" differently and the
// metrics one failed in the direction that leaves a port open. There is no `0` in an address, and each
// listener already has an `enabled` flag next to its new `addr` — one way to disable, per listener.
//
// global.profile_port is removed rather than wired, and there is now nothing left for it to have
// wired. It defaulted to 6060 and started nothing; the only pprof server in the tree was
// pkg/profiling's, which bound every interface and served mutating /memory/gc and /memory/free with
// no authentication. Binding a third unauthenticated listener inside the change that stops binding
// two of them was the wrong trade to make on the strength of a boolean nothing read — and #245
// resolved the other half by deleting the package, since nothing imported it. Restoring the setting
// therefore means building the server too, with an authenticated, non-wildcard bind.
type GlobalConfig struct {
	LogLevel string `yaml:"log_level"`
	LogFile  string `yaml:"log_file"`
}

// MountConfig names what to mount and where, for an invocation that cannot pass them as arguments.
//
// New in v0.11.0, and it exists because of a shape a command line cannot express. A systemd template
// unit is instantiated per bucket — `systemctl start objectfs@research-data` — and the only thing the
// unit file knows about the instance is `%i`. `configs/systemd/objectfs@.service` therefore ran
//
//	ExecStart=/usr/bin/objectfs mount --config ${OBJECTFS_CONFIG} --foreground
//
// with no URI and no mount point anywhere on the line (#134, #135). There was no `mount` subcommand
// either, so the unit failed at exec; but adding one would not have been enough, because a unit that
// serves every instance from one template has nowhere to put a per-instance bucket except the
// per-instance config file that `%i` already selects.
//
// Both keys stay optional and the command line wins. `objectfs mount s3://bucket /mnt` needs no config
// file at all, and a file that names a bucket can still be pointed at a different one for a one-off
// without editing it. What is refused is silence: a mount with neither a URI on the line nor one in the
// file fails naming both places, rather than defaulting to something.
type MountConfig struct {
	// URI is the object storage URI to mount, in the same form the command line takes: `s3://bucket`
	// or `s3://bucket/prefix`. Validated by the same code that validates the argument, so a scheme
	// this build cannot mount is rejected at config load rather than at mount.
	URI string `yaml:"uri"`

	// MountPoint is the directory to mount on. Empty means it must come from the command line.
	//
	// It is not defaulted to anything. A wrong mount point is not a mount that fails — it is a mount
	// that succeeds somewhere the operator is not looking, over whatever was already there.
	MountPoint string `yaml:"mount_point"`
}

// PerformanceConfig represents performance-related settings.
//
// Its `compression_enabled` key is gone (#157). It defaulted to **true**, was read by nothing, and
// sat two sections away from the `compression` block that actually controlled compression and
// defaulted to false — so the configuration contained a prominent `compression_enabled: true` that
// meant nothing, next to the real setting that said otherwise. It is removed rather than wired
// because there is nothing to wire it to: compression happens in the S3 backend, on the object, and
// is configured by `storage.s3.compression`. A second boolean over the same feature could only
// disagree with the first.
//
// Its `read_ahead_size` key is gone for the same reason (#176). It defaulted to 64MB, was read by
// nothing, and sat beside the `read_ahead` block that describes the same feature — so a configuration
// could name two different read-ahead sizes, and neither took effect. The block below is the one
// setting, and `window_size` inside it is the size.
type PerformanceConfig struct {
	CacheSize       string `yaml:"cache_size"`
	WriteBufferSize string `yaml:"write_buffer_size"`
	MaxConcurrency  int    `yaml:"max_concurrency"`
	// ConnectionPoolSize is the number of pooled S3 clients, and also the batch concurrency in
	// GetObjects/PutObjects and MaxIdleConnsPerHost on the HTTP transport. Validated > 0 below:
	// zero reached the batch paths as an unbuffered semaphore and blocked forever.
	ConnectionPoolSize int                `yaml:"connection_pool_size"`
	PredictiveCaching  bool               `yaml:"predictive_caching"`
	MLModelPath        string             `yaml:"ml_model_path"`
	MultilevelCaching  bool               `yaml:"multilevel_caching"`
	ReadAhead          ReadAheadConfig    `yaml:"read_ahead"`    // Advanced read-ahead configuration
	ParallelRead       ParallelReadConfig `yaml:"parallel_read"` // Parallel range GET configuration
}

// ParallelReadConfig controls fan-out of large object reads into concurrent range GETs.
//
// Reaches the backend as s3.Config.ParallelReadThreshold and ReadChunkSize. Enabled: false maps to
// a threshold of zero, which is how the backend spells "disabled" — see buildS3Config.
//
// This block was defined, defaulted and documented for a whole release while being read by nothing,
// which made the parallel range GET feature v0.10.0 was released for dead code on every mount: the
// backend's gate is `threshold > 0` and the mount path left the threshold at zero.
type ParallelReadConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Threshold string `yaml:"threshold"`  // e.g. "64MB"
	ChunkSize string `yaml:"chunk_size"` // e.g. "16MB"
}

// CacheConfig represents cache configuration
type CacheConfig struct {
	TTL             time.Duration         `yaml:"ttl"`
	MaxEntries      int                   `yaml:"max_entries"`
	EvictionPolicy  string                `yaml:"eviction_policy"`
	PersistentCache PersistentCacheConfig `yaml:"persistent_cache"`
}

// PersistentCacheConfig represents persistent cache settings
type PersistentCacheConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Directory string `yaml:"directory"`
	MaxSize   string `yaml:"max_size"`
}

// ReadAheadConfig configures the sequential-read detector that prefetches ahead of a reader.
//
// Reaches the read-ahead manager as fuse.ReadAheadConfig, which is the only consumer. The mount path
// previously constructed that manager with nil and used its built-in defaults, so every key here was
// validated at load and read by nothing (#176).
//
// This block used to have twenty keys and now has five, and the reduction is the fix rather than a
// simplification of it. The old set described a strategy selector with pattern-detection thresholds,
// a prefetch bandwidth cap, and ML hooks — `strategy: ml`, `enable_ml_prediction`,
// `ml_model_path`, `learning_rate`, `pattern_depth`, `model_update_interval`. No model loader exists
// anywhere in the repository, and there is no field-name overlap with the implementation beyond
// Enabled, so the two were not a plumbing omission that could be fixed by passing the value through:
// they disagreed about what read-ahead is. What exists is a sequential-access detector with a
// prefetch window, tuned against measured byte counts (see fuse.ReadAheadManager.prefetchLength).
// The keys below are that detector's, one per knob it has.
//
// Wiring the old block would have been worse than leaving it inert. `strategy: ml` with a validated
// model path reaching no model loader is a claim about the software, and `learning_rate` being
// range-checked to 0-1 is what made the whole set look load-bearing: a user whose file is rejected
// for an out-of-range value reasonably concludes the accepted values do something.
type ReadAheadConfig struct {
	// Enabled turns read-ahead off entirely. Off means a read fetches exactly what was asked for.
	Enabled bool `yaml:"enabled"`

	// WindowSize is the floor on how far ahead of a sequential reader to prefetch, and the prefetch is
	// the larger of this and the reader's own last read size.
	//
	// It is a floor rather than the window because a prefetch shorter than the read it anticipates
	// cannot satisfy that read: the cache answers only when it holds the whole requested range, so a
	// 64 KiB prefetch ahead of a 128 KiB reader produces an entry every subsequent read walks past, and
	// the bytes are paid for twice. That was measurable — a 3 MiB sequential traversal transferred
	// 4,325,644 bytes with zero prefetch hits.
	WindowSize string `yaml:"window_size"`

	// MinSequential is how many consecutive sequential reads must be seen before prefetching starts,
	// and it is the only threshold: at the default of 3, the third sequential read triggers the first
	// prefetch.
	//
	// It did not used to be. The detector also required a confidence above 0.5, where confidence was
	// sequentialHits/10 — the same counter in different units — so the effective gate was
	// max(MinSequential, 6) and every value from 1 to 5 behaved identically, including the default.
	// The confidence floor is gone (#247), which makes the default 3 mean what it says and prefetch
	// from the third read rather than the sixth.
	MinSequential int `yaml:"min_sequential"`

	// ConcurrentReads is the number of prefetch workers, each of which performs one GET at a time.
	// It bounds how much prefetch traffic a mount can have in flight, so it is also a cost control.
	ConcurrentReads int `yaml:"concurrent_reads"`

	// TTL is how long an idle read pattern is remembered. A reader that pauses longer than this starts
	// its sequential run over, which means the first few reads after a pause are not prefetched.
	TTL time.Duration `yaml:"ttl"`
}

// WriteBufferConfig represents write buffer configuration.
//
// Its `compression` subsection moved to `storage.s3.compression`, where the thing it configures
// lives (#157). It is gone rather than deprecated, and the loader is strict, so a file still setting
// `write_buffer.compression` fails to load with the key named — the same reasoning as the removed
// encryption booleans above. Silently accepting it under the old path would leave an operator
// believing they had configured a write buffer when what they had configured was the stored format
// of every object in their bucket, which is the misunderstanding the move exists to end.
type WriteBufferConfig struct {
	FlushInterval time.Duration `yaml:"flush_interval"`
	MaxBuffers    int           `yaml:"max_buffers"`
	MaxMemory     string        `yaml:"max_memory"`
}

// CompressionConfig configures transparent compression of stored S3 objects. See
// [S3Config.Compression], its only user.
type CompressionConfig struct {
	// Enabled turns compression on. Off by default: it is a storage-format decision rather than a
	// performance knob, and a compressed object is an opaque frame to every S3 client but ObjectFS.
	Enabled bool `yaml:"enabled"`
	// MinSize is the smallest object worth compressing, e.g. "4KB". Below it, framing overhead and
	// any per-tier billing floor dominate whatever the codec saves.
	MinSize string `yaml:"min_size"`
	// Algorithm names the codec: "none", "zstd", "lz4", or "gzip". The authority is
	// pkg/compression.SupportedAlgorithms, and Validate builds the codec rather than checking a list,
	// because building it is the only check that cannot drift.
	//
	// Safe to change on a bucket that already holds compressed objects: the read path decodes every
	// algorithm ObjectFS can write, chosen from each object's own Content-Encoding (#230). Through
	// v0.10.x it was not — a mount decoded only its configured algorithm.
	Algorithm string `yaml:"algorithm"`
	// Level is the codec-specific compression level; 0 selects the codec's default. The valid range
	// differs per algorithm — zstd accepts 0-22, gzip only 0-9 — so a level valid for one is often
	// invalid for another, and changing Algorithm may require changing this too.
	Level int `yaml:"level"`
}

// NetworkConfig represents network configuration
type NetworkConfig struct {
	Timeouts            TimeoutConfig        `yaml:"timeouts"`
	Retry               RetryConfig          `yaml:"retry"`
	CircuitBreaker      CircuitBreakerConfig `yaml:"circuit_breaker"`
	CongestionAlgorithm string               `yaml:"congestion_algorithm"` // "auto", "bbr", "cubic", "reno"
}

// TimeoutConfig represents timeout settings.
//
// Connect becomes the dialer's timeout and Read becomes the transport's ResponseHeaderTimeout —
// which is the time S3 has to *start* answering, not to finish. A response timeout would break
// ranged reads of large objects, whose bodies legitimately take minutes: the transport cannot
// distinguish a stalled connection from a slow one that is still delivering bytes.
//
// Write is not yet wired, and has no HTTP counterpart to be wired to: the transport sees a PUT as
// one request whose body it is streaming, with no separate notion of a write timeout. Where it
// belongs is the flush path's context in internal/vfs, which does not take one today.
type TimeoutConfig struct {
	Connect time.Duration `yaml:"connect"`
	Read    time.Duration `yaml:"read"`
	Write   time.Duration `yaml:"write"`
}

// RetryConfig represents retry settings for ObjectFS's own retry of a failed S3 operation.
//
// Distinct from storage.s3.max_retries, which is the AWS SDK's per-request attempt limit. The two
// compose: an operation is attempted MaxAttempts times here, and each of those attempts is itself
// retried up to max_retries times by the SDK.
//
// Which errors are retried is not configurable and is not derived from this block: it is
// pkg/retry.DefaultConfig's list of seven ObjectFS error codes (timeouts, connection failures,
// resource exhaustion). buildS3Config therefore starts from that default and overrides only the
// three fields below, because pkg/retry.New does *not* backfill RetryableErrors — a config mapped
// field-for-field into an empty retry.Config would retry almost nothing while reporting three
// attempts.
type RetryConfig struct {
	MaxAttempts int           `yaml:"max_attempts"`
	BaseDelay   time.Duration `yaml:"base_delay"`
	MaxDelay    time.Duration `yaml:"max_delay"`
}

// CircuitBreakerConfig represents circuit breaker settings.
//
// FailureThreshold is a count of failures within one Interval, and it becomes a ReadyToTrip closure
// rather than a field: internal/circuit.Config has no threshold field, only a predicate. Reading
// that struct's MaxRequests as the threshold is a mistake made during the audit and the reason this
// is spelled out — MaxRequests is the half-open probe limit.
//
// Enabled: false is expressed as a ReadyToTrip that never trips, so the breaker still counts and
// still reports state; it just never opens. There is no way to remove it from the call path, and
// pretending otherwise would mean a second code path with no test coverage.
type CircuitBreakerConfig struct {
	Enabled          bool          `yaml:"enabled"`
	FailureThreshold int           `yaml:"failure_threshold"`
	Timeout          time.Duration `yaml:"timeout"`
}

// SecurityConfig represents security settings
type SecurityConfig struct {
	Enabled     bool             `yaml:"enabled"`
	AuthMethod  string           `yaml:"auth_method"`
	TLSEnabled  bool             `yaml:"tls_enabled"`
	TLSCertPath string           `yaml:"tls_cert_path"`
	TLSKeyPath  string           `yaml:"tls_key_path"`
	TLS         TLSConfig        `yaml:"tls"`
	Encryption  EncryptionConfig `yaml:"encryption"`
}

// TLSConfig represents TLS settings
type TLSConfig struct {
	VerifyCertificates bool   `yaml:"verify_certificates"`
	MinVersion         string `yaml:"min_version"`
}

// EncryptionConfig selects the server-side encryption ObjectFS requests for the objects it writes.
//
// This block used to be two booleans, `in_transit` and `at_rest`, both defaulting to **true** and both
// read by nothing — audit finding P-7. A grep for ServerSideEncryption, SSEKMS, or aws:kms across the
// tree returned zero non-test hits while OBJECTFS.md documented a `kms_key:` ARN in this block. Every
// object was written with no encryption header at all, and the configuration said otherwise.
//
// Both keys are gone rather than deprecated, and since the loader decodes strictly, a config still
// setting them fails to load with the key named. That is the point: silently accepting `at_rest: true`
// under a new schema would leave the operator believing the same false thing they believed before,
// whereas an error is the one way the claim gets re-examined by whoever wrote it.
//
// `in_transit` is gone with nothing replacing it because there is nothing to replace it with. The AWS
// SDK speaks HTTPS to S3 unless an endpoint override says otherwise, so transit encryption is not
// ObjectFS's to switch on; the only thing that turns it off is pointing `storage.s3.endpoint` at an
// http:// URL, which the operator has to write explicitly. A boolean whose true value is unconditional
// and whose false value is unreachable is not a setting.
type EncryptionConfig struct {
	// Mode selects at-rest encryption: "off", "sse-s3", or "sse-kms". Empty means "off".
	//
	// "off" sends no header, which is not the same as unencrypted: S3 has applied SSE-S3 to all new
	// objects unconditionally since January 2023, so data in a default bucket is encrypted whether or
	// not ObjectFS asks. What "off" gives up is a key the institution controls — one that can be
	// audited, rotated, and revoked independently of the data.
	Mode string `yaml:"mode"`

	// KMSKeyID is the key "sse-kms" encrypts with: a key ID, an alias, or either ARN form. Required for
	// that mode and rejected beside any other, rather than ignored, because an ignored key is P-7 again.
	KMSKeyID string `yaml:"kms_key_id"`

	// BucketKeys requests S3 Bucket Keys, which cut SSE-KMS's per-object KMS calls by up to 99% by
	// deriving a bucket-level key. Recommended with "sse-kms" — KMS bills per call and rate-limits per
	// region, and a filesystem generates far more object operations than a backup tool does — and
	// rejected without it, since the setting does nothing on its own.
	BucketKeys bool `yaml:"bucket_keys"`
}

// The encryption modes the `mode` key accepts, aliased from awsname so this package and
// internal/storage/s3 cannot disagree about which modes exist. See [awsname.SSEModeOff] and siblings.
const (
	EncryptionModeOff = awsname.SSEModeOff
	EncryptionModeS3  = awsname.SSEModeS3
	EncryptionModeKMS = awsname.SSEModeKMS
)

// MonitoringConfig represents monitoring settings.
//
// Its `metrics_addr`, `health_check_addr` and `enable_pprof` keys are gone as of v0.11.0. The first
// two moved to `monitoring.metrics.addr` and `monitoring.health_checks.addr`, beside the `enabled`
// flag each governs — an address belongs to the listener it binds, and at the top of this block they
// were two sections away from the ports the listeners actually used, which is how both came to be
// read by nothing. `enable_pprof` is removed outright; see [GlobalConfig] for why no pprof listener
// was started in its place.
//
// `enabled` remains read by nothing. It is not the switch for this block — `metrics.enabled` and
// `health_checks.enabled` are, one per listener, and both are honored. Left in place rather than
// removed with the others because deciding whether a whole-block switch should exist is a design
// question, not a plumbing one.
type MonitoringConfig struct {
	Enabled       bool                `yaml:"enabled"`
	OpenTelemetry OpenTelemetryConfig `yaml:"opentelemetry"`
	Metrics       MetricsConfig       `yaml:"metrics"`
	HealthChecks  HealthChecksConfig  `yaml:"health_checks"`
	Logging       LoggingConfig       `yaml:"logging"`
}

// MetricsConfig represents metrics settings
type MetricsConfig struct {
	Enabled bool `yaml:"enabled"`

	// Addr is the host:port the Prometheus endpoint binds, default "127.0.0.1:8080".
	//
	// Loopback by default. /metrics and /debug/operations between them report per-operation counts,
	// error rates, sizes and timings — enough to characterize what a bucket is used for and when — and
	// neither has any authentication, so a wildcard bind exposed that to every host that could route
	// to the mount. A same-host Prometheus scrape still works; a cross-host one is now something an
	// operator writes down.
	//
	// Set "0.0.0.0:8080" for every interface, or ":8080", which means the same thing. Setting Enabled
	// false is how the endpoint is disabled — there is no port 0 to write, which is deliberate: this
	// field replaced global.metrics_port, where 0 read as "off" and defaulted back to 8080.
	Addr string `yaml:"addr"`

	Prometheus   bool              `yaml:"prometheus"`
	CustomLabels map[string]string `yaml:"custom_labels"`
}

// HealthChecksConfig represents health check settings
type HealthChecksConfig struct {
	Enabled bool `yaml:"enabled"`

	// Addr is the host:port the health endpoint binds, default "127.0.0.1:8081".
	//
	// Loopback by default, for the reason [MetricsConfig.Addr] gives: /health reports component names,
	// error strings and check timings with no authentication. This was found the hard way — a test
	// asserting a bind *collision* held 127.0.0.1:8081 and watched the server come up on [::]:8081 and
	// serve happily (#192).
	//
	// Setting Enabled false disables the endpoint and the checks behind it.
	Addr string `yaml:"addr"`

	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

// LoggingConfig represents logging settings
type LoggingConfig struct {
	Structured bool           `yaml:"structured"`
	Format     string         `yaml:"format"`
	Sampling   SamplingConfig `yaml:"sampling"`
}

// OpenTelemetryConfig represents OpenTelemetry settings
type OpenTelemetryConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Endpoint    string `yaml:"endpoint"`
	ServiceName string `yaml:"service_name"`
}

// SamplingConfig represents log sampling settings
type SamplingConfig struct {
	Enabled bool `yaml:"enabled"`
	Rate    int  `yaml:"rate"`
}

// FeatureConfig represents feature flags
type FeatureConfig struct {
	Prefetching           bool `yaml:"prefetching"`
	BatchOperations       bool `yaml:"batch_operations"`
	SmallFileOptimization bool `yaml:"small_file_optimization"`
	MetadataCaching       bool `yaml:"metadata_caching"`
	OfflineMode           bool `yaml:"offline_mode"`
}

// StorageConfig represents storage backend configuration
type StorageConfig struct {
	S3 S3Config `yaml:"s3"`
}

// S3Config represents AWS S3 configuration.
//
// Every field here has to be carried into the corresponding field of internal/storage/s3.Config by
// internal/adapter.buildS3Config, and TestBuildS3ConfigMapsEveryConfiguredValue asserts it does.
// That test is the other half of this struct: buildS3Config mapped six of the backend's thirty
// fields, so `storage_tier`, the pool size, the retry limit, the timeouts and the multipart and
// parallel-read settings were named in configuration, documented in examples/config.yaml, and left
// at their zero values on every real mount (audit finding D12).
//
// Zero is not a benign default for several of them. A pool size of zero is not a small pool — it is
// `make(chan struct{}, 0)` in GetObjects and PutObjects, so the first batch operation blocks
// forever. A parallel-read threshold of zero disables the feature v0.10.0 was released for. A
// storage tier of "" writes STANDARD whatever the file says.
type S3Config struct {
	Region          string `yaml:"region"`
	Endpoint        string `yaml:"endpoint"`
	Profile         string `yaml:"profile"`
	UseAcceleration bool   `yaml:"use_acceleration"`
	ForcePathStyle  bool   `yaml:"force_path_style"`

	// StorageTier is the S3 storage class objects are written with: STANDARD, STANDARD_IA,
	// ONEZONE_IA, GLACIER_IR, GLACIER, DEEP_ARCHIVE, INTELLIGENT_TIERING or REDUCED_REDUNDANCY.
	// Empty means STANDARD, which is also what S3 applies to a PUT that names no class.
	//
	// Validated at load by awsname.ValidateStorageClass, because a class the backend does not
	// recognize is silently replaced with STANDARD by NewTierValidator — so `STANDARD_1A`, a digit
	// one for a capital I, would be billed as STANDARD with nothing reporting a problem.
	StorageTier string `yaml:"storage_tier"`

	// MaxRetries is the AWS SDK's attempt limit per operation, passed to
	// config.WithRetryMaxAttempts. Zero means the backend's default of 3.
	//
	// This is the SDK's own retry of a single HTTP call. ObjectFS layers its own retry on top of it
	// (network.retry below), so the two multiply: 3 SDK attempts inside 3 ObjectFS attempts is up to
	// nine requests.
	MaxRetries int `yaml:"max_retries"`

	// `use_cargoship` was removed in v0.15.0 along with the upload path it selected (#362), and the
	// loader is strict, so a file still setting it fails at startup naming the key — the same
	// reasoning as the removed encryption booleans and the cost-optimization block above.
	//
	// The setting selected a second PutObject implementation that could not carry a Content-Encoding,
	// the configured encryption headers, a per-object storage class, or a Content-Type; three of those
	// had already been bypassed and the fourth was silently storing every small object as
	// application/octet-stream. It was also unreachable above `multipart.threshold`, so the 64 MiB
	// buffer it installed served an upload shape a mount cannot produce. There is one upload path now
	// and nothing to select, so the key names no choice rather than naming a choice that no longer
	// exists.

	// Multipart controls when an upload is split into parts and how those parts are sent.
	Multipart MultipartConfig `yaml:"multipart"`

	// Compression controls transparent compression of the objects ObjectFS writes to S3.
	//
	// It lives here, under the backend that stores the objects, because that is what it decides: the
	// stored format of an object in a bucket. It used to live under `write_buffer`, where the name
	// implied it compressed data held in memory before upload and where nothing else in the section
	// was about storage at all (#157). Nothing compressed a buffer — the setting was mapped straight
	// into the S3 backend's own compression config and always had been — so the key's location
	// described a feature that did not exist while the feature it did control was documented nowhere
	// near the backend it configures.
	//
	// That mattered beyond tidiness in two directions. An operator tuning write buffering had no
	// reason to expect they were changing the on-disk format of every object they wrote, and an
	// operator looking for object compression had no reason to look under `write_buffer`. Both are
	// how a storage-format decision gets made by accident.
	Compression CompressionConfig `yaml:"compression"`

	CostOptimization S3CostOptimization `yaml:"cost_optimization"`
}

// MultipartConfig controls S3 multipart upload behavior.
type MultipartConfig struct {
	// Threshold is the object size above which an upload is split into parts (e.g. "32MB").
	// Empty means the backend's default.
	Threshold string `yaml:"threshold"`

	// ChunkSize is the size of each part (e.g. "16MB"). S3 rejects any non-final part below 5 MB
	// with EntityTooSmall, and the backend raises a smaller value to that floor.
	ChunkSize string `yaml:"chunk_size"`

	// Concurrency is the number of parts uploaded at once. Zero means the backend's default.
	Concurrency int `yaml:"concurrency"`
}

// S3CostOptimization is the `storage.s3.cost_optimization` block.
//
// One key, where there were five. `enabled`, `tiering_enabled`, `lifecycle_enabled`,
// `transition_to_ia` and `transition_to_glacier` were removed rather than plumbed, and the reason is
// worth stating because the shape of this block was itself the defect (#203): this type and
// s3.CostOptimization, the struct the backend reads, shared not one field name. There was no mapping
// to write. `transition_to_ia: 30` had to become an element of a []TransitionRule carrying a source
// tier, a destination tier, an access pattern and a size filter, none of which the YAML supplied —
// and nothing in the repository read TransitionRules either, so the mapping would have invented four
// values in order to reach code with no caller.
//
// Removed, not deprecated, and the loader rejects unknown keys — so a config file still carrying any
// of the five fails at startup naming the key. That is the only way an operator learns the setting
// never did anything. A block that had no effect for its entire existence has no behavior to
// preserve, so the compatibility that matters here is telling the truth about that rather than
// continuing to accept the words.
type S3CostOptimization struct {
	// SmallObjectsOnStandard writes an object to STANDARD when `storage.s3.storage_tier` names a tier
	// that would bill the object as larger than it is — the 128 KB minimum billable size on
	// STANDARD_IA, ONEZONE_IA and GLACIER_IR.
	//
	// It is the only setting in this block with an effect, because it is the only cost decision made
	// on a path a mount takes: the write. Everything else the backend's cost machinery can do runs
	// through AnalyzeAndOptimize, which no mount calls.
	//
	// Off by default. It changes the storage class of stored objects, and a mount that silently
	// stored objects on a class other than the one `storage_tier` names would be the same kind of
	// surprise in the other direction.
	SmallObjectsOnStandard bool `yaml:"small_objects_on_standard"`
}

// ClusterConfig represents distributed cluster settings.
//
// There was a `consistency_level` key here, taking "eventual", "strong" or "session", and #284
// removed it. Because [Configuration.LoadFromFile] decodes strictly, a file that still sets it now
// fails at startup naming the key and line — which is the intended outcome rather than a regression:
// the alternative is accepting a setting that selects nothing.
//
// It selected nothing even before it was removed. All three levels issued the same unconditional
// PutObject and differed only in how many nodes issued it, so what an operator picked changed the
// request count and not the guarantee. What replaced it is per-operation rather than per-mount:
// distributed.DistributedOperation.Precondition, evaluated by S3 on the one write it guards.
type ClusterConfig struct {
	Enabled       bool     `yaml:"enabled"`
	NodeID        string   `yaml:"node_id"`
	ListenAddr    string   `yaml:"listen_addr"`
	AdvertiseAddr string   `yaml:"advertise_addr"`
	SeedNodes     []string `yaml:"seed_nodes"`

	// SecretFile names the file holding the shared cluster secret that authenticates gossip messages.
	// A cluster does not start without one, from here or from OBJECTFS_CLUSTER_SECRET, which takes
	// precedence — see distributed.LoadClusterSecret, whose error already told operators to set this
	// key before the key existed.
	//
	// The *path* is what goes here and never the secret. This file is installed world-readable by
	// packaging, so a secret written in it would be published to every user on the node; the file this
	// points at must be mode 0600 or startup refuses it. Generate one with
	// `openssl rand -hex 32 > /etc/objectfs/cluster.secret && chmod 600 /etc/objectfs/cluster.secret`.
	SecretFile string `yaml:"secret_file"`

	ReplicationFactor int         `yaml:"replication_factor"`
	Redis             RedisConfig `yaml:"redis"`
}

// RedisConfig represents Redis distributed cache settings
type RedisConfig struct {
	Enabled    bool          `yaml:"enabled"`
	Address    string        `yaml:"address"`
	Password   string        `yaml:"password"`
	DB         int           `yaml:"db"`
	KeyPrefix  string        `yaml:"key_prefix"`
	TTL        time.Duration `yaml:"ttl"`
	MaxRetries int           `yaml:"max_retries"`
}

// NewDefault returns a configuration with sensible defaults
func NewDefault() *Configuration {
	return &Configuration{
		Global: GlobalConfig{
			LogLevel: "INFO",
			LogFile:  "",
		},
		Storage: StorageConfig{
			S3: S3Config{
				Region:          "us-east-1",
				Endpoint:        "",
				Profile:         "",
				UseAcceleration: false,
				ForcePathStyle:  false,
				StorageTier:     awsname.StorageClassStandard,
				MaxRetries:      3,
				Multipart: MultipartConfig{
					Threshold:   "32MB",
					ChunkSize:   "16MB",
					Concurrency: 8,
				},
				// Compression is off by default. It is a storage-format decision, not a performance
				// knob: a compressed object is an opaque frame to `aws s3 cp`, boto3, and every other
				// S3 client, so enabling it by default would silently revoke the "my data is just
				// objects in S3" property that most users assume. It also makes a ranged read fetch
				// the whole object, since a compression frame cannot be sliced. Opt in when the
				// tradeoff is wanted.
				//
				// Off here and on in internal/storage/s3.NewDefaultConfig, because that constructor
				// serves the Go SDK, where the caller chose the S3 backend explicitly, and this file
				// serves a mount.
				//
				// The algorithm is named even though compression is disabled, so that flipping
				// Enabled to true does not also have to supply one. zstd/3/4KB matches what
				// NewDefaultConfig chooses, so the two entry points agree on everything but Enabled.
				Compression: CompressionConfig{
					Enabled:   false,
					MinSize:   "4KB",
					Algorithm: "zstd",
					Level:     3,
				},
				CostOptimization: S3CostOptimization{
					SmallObjectsOnStandard: false,
				},
			},
		},
		Performance: PerformanceConfig{
			CacheSize:          "2GB",
			WriteBufferSize:    "16MB",
			MaxConcurrency:     150,
			ConnectionPoolSize: 8,
			PredictiveCaching:  false,
			MLModelPath:        "",
			MultilevelCaching:  false,
			// These are the read-ahead manager's own defaults, restated here because this is now the
			// value that reaches it. They must stay equal to fuse.ReadAheadManager's nil-config
			// fallback, which TestReadAheadDefaultsMatchTheManagers asserts — a mount and a
			// directly-constructed manager reporting different behavior would be the same seam
			// #176 was.
			ReadAhead: ReadAheadConfig{
				Enabled:         true,
				WindowSize:      "64KB",
				MinSequential:   3,
				ConcurrentReads: 4,
				TTL:             5 * time.Minute,
			},
			ParallelRead: ParallelReadConfig{
				Enabled:   true,
				Threshold: "64MB",
				ChunkSize: "16MB",
			},
		},
		Cache: CacheConfig{
			TTL:            5 * time.Minute,
			MaxEntries:     100000,
			EvictionPolicy: "weighted_lru",
			PersistentCache: PersistentCacheConfig{
				Enabled:   false,
				Directory: "/var/cache/objectfs",
				MaxSize:   "10GB",
			},
		},
		WriteBuffer: WriteBufferConfig{
			FlushInterval: 30 * time.Second,
			MaxBuffers:    1000,
			MaxMemory:     "512MB",
		},
		Network: NetworkConfig{
			Timeouts: TimeoutConfig{
				Connect: 10 * time.Second,
				Read:    30 * time.Second,
				Write:   300 * time.Second,
			},
			Retry: RetryConfig{
				MaxAttempts: 3,
				BaseDelay:   1 * time.Second,
				MaxDelay:    30 * time.Second,
			},
			CircuitBreaker: CircuitBreakerConfig{
				Enabled:          true,
				FailureThreshold: 5,
				Timeout:          60 * time.Second,
			},
			CongestionAlgorithm: "auto",
		},
		Security: SecurityConfig{
			Enabled:     false,
			AuthMethod:  "none",
			TLSEnabled:  false,
			TLSCertPath: "",
			TLSKeyPath:  "",
			TLS: TLSConfig{
				VerifyCertificates: true,
				MinVersion:         "1.2",
			},
			// Encryption defaults to off, and this is the one field in this struct whose default is
			// chosen against the grain of "secure by default".
			//
			// The reason is that the secure-sounding default is what caused P-7. `at_rest: true` shipped
			// as the default, which is exactly why nobody questioned it — a default nobody sets is a
			// default nobody checks, and it went three releases claiming a property no code implemented.
			// Defaulting to sse-kms is impossible (there is no key to name), and defaulting to sse-s3
			// would request what S3 already does unconditionally while making the header look like the
			// reason it happened.
			//
			// Off, therefore, and the honest surface documentation says what off means: S3 encrypts the
			// objects anyway, with its own keys, and an institution that needs its own key has to say so.
			Encryption: EncryptionConfig{
				Mode: EncryptionModeOff,
			},
		},
		Monitoring: MonitoringConfig{
			Enabled: false,
			OpenTelemetry: OpenTelemetryConfig{
				Enabled:     false,
				Endpoint:    "localhost:4317",
				ServiceName: "objectfs",
			},
			Metrics: MetricsConfig{
				Enabled:    true,
				Addr:       DefaultMetricsAddr,
				Prometheus: true,
				CustomLabels: map[string]string{
					"service": "objectfs",
				},
			},
			HealthChecks: HealthChecksConfig{
				Enabled:  true,
				Addr:     DefaultHealthAddr,
				Interval: 30 * time.Second,
				Timeout:  5 * time.Second,
			},
			Logging: LoggingConfig{
				Structured: true,
				Format:     "json",
				Sampling: SamplingConfig{
					Enabled: true,
					Rate:    1000,
				},
			},
		},
		Features: FeatureConfig{
			Prefetching:           true,
			BatchOperations:       true,
			SmallFileOptimization: true,
			MetadataCaching:       true,
			OfflineMode:           false,
		},
		Cluster: ClusterConfig{
			Enabled:           false,
			NodeID:            "",
			ListenAddr:        "0.0.0.0:8080",
			AdvertiseAddr:     "127.0.0.1:8080",
			SeedNodes:         []string{},
			ReplicationFactor: 3,
			Redis: RedisConfig{
				Enabled:    false,
				Address:    "localhost:6379",
				Password:   "",
				DB:         0,
				KeyPrefix:  "objectfs",
				TTL:        5 * time.Minute,
				MaxRetries: 3,
			},
		},
		// FUSE is deliberately absent, unlike every other section above.
		//
		// All three of its fields are false by default and false is the kernel's own behavior for each,
		// so the zero value *is* the default and naming it here would only create a second place for
		// that default to live. TestFUSEZeroValueIsTheDefault pins the property this omission depends
		// on — that a Configuration built as a literal and one built here describe the same mount.
	}
}

// LoadFromFile loads configuration from a YAML file.
//
// Decoding is strict: a key the schema does not define is an error, not a silent omission. This
// is audit finding P-10, and it is a deliberate breaking change from v0.10.0 and earlier.
//
// What non-strict decoding cost. `configs/example.yaml` — the file the README told users to copy
// and the file scripts/postinstall.sh installed as /etc/objectfs/config.yaml — was 162 lines of
// commented settings of which, measured against NewDefault, exactly *one* field differed from the
// built-in default, and nothing read that field. It opened with a top-level `s3:` block where the
// schema has `storage.s3`, so `region: us-west-2` loaded as `us-east-1`. Every one of its
// `mount:`, `buffer:`, `compression:`, `metrics:`, `health:`, `logging:`, `archive:` and `cost:`
// blocks was discarded in silence. That is also why the compression block documented `enable`,
// `zstd_level` and `min_file_size` against the real `enabled`, `level` and `min_size` and nobody
// noticed for four releases: the whole block was already being thrown away, so correcting the
// names would not have changed the behavior either.
//
// A config file that is rejected costs a user a minute. A config file that is ignored lets them
// believe they configured a 100 GB cache in us-west-2 while running a 1 GB cache in us-east-1,
// and nothing anywhere will ever tell them otherwise. For a filesystem whose first job is to be
// trusted with data, the second failure mode is not acceptable and the first one is.
//
// The migration cost is real and it is bounded: a deployment whose config has a key this schema
// does not define was already not getting that setting, so nothing it relied on stops working —
// it starts being told. The error names the offending key and line, and this method appends the
// schema's top-level keys so the fix does not require reading the source.
func (c *Configuration) LoadFromFile(filename string) error {
	// Validate file path to prevent directory traversal
	if err := utils.ValidatePath(filename, true); err != nil {
		return fmt.Errorf("invalid config file path: %w", err)
	}

	cleanPath := filepath.Clean(filename)
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// Two passes, and they are not redundant.
	//
	// The first decodes into a zero-valued Configuration for the sole purpose of checking that
	// every key in the document is a key the schema defines. The second does the actual load,
	// overlaying the document onto the defaults already in c.
	//
	// One strict pass directly into c does not work, and the reason is worth recording because the
	// obvious simplification reintroduces a bug. yaml.v2's strict mode reports "key already set in
	// map" when a document assigns a map key that is already present in the destination map — and
	// c arrives here holding NewDefault's values, which include
	// monitoring.metrics.custom_labels: {service: objectfs}. A user adding their own label, or
	// overriding that one, would be told their file has a duplicate key. Decoding the strict pass
	// into a fresh value means no map is pre-populated, so the only duplicate keys it can report
	// are duplicates genuinely written in the document, which is a real mistake and worth
	// rejecting.
	var schemaCheck Configuration
	if err := yaml.UnmarshalStrict(data, &schemaCheck); err != nil {
		return fmt.Errorf("failed to parse config file %s: %w\n\nthe top-level keys this "+
			"version accepts are: %s\nsee examples/config.yaml for the full schema",
			cleanPath, err, strings.Join(TopLevelKeys(), ", "))
	}

	if err := yaml.Unmarshal(data, c); err != nil {
		return fmt.Errorf("failed to parse config file %s: %w", cleanPath, err)
	}

	return nil
}

// TopLevelKeys reports the YAML keys [Configuration] accepts, read from the struct tags.
//
// Read by reflection rather than written out, so it cannot fall out of step with the schema the
// way a hand-maintained list would — which is the same class of drift that produced P-10.
func TopLevelKeys() []string {
	t := reflect.TypeFor[Configuration]()

	keys := make([]string, 0, t.NumField())

	for field := range t.Fields() {
		tag := field.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}

		keys = append(keys, strings.Split(tag, ",")[0])
	}

	return keys
}

// envMapping defines environment variable mappings and setters
type envMapping struct {
	envVar string
	setter func(*Configuration, string) error
}

// getEnvMappings returns all environment variable mappings
func getEnvMappings() []envMapping {
	return []envMapping{
		// Global settings
		{"OBJECTFS_LOG_LEVEL", func(c *Configuration, val string) error {
			c.Global.LogLevel = val
			return nil
		}},
		{"OBJECTFS_LOG_FILE", func(c *Configuration, val string) error {
			c.Global.LogFile = val
			return nil
		}},
		// Repointed from the removed global.metrics_port, and renamed with it: the variable assigns an
		// address now, so a name saying PORT would be the same two-names-one-setting problem the ports
		// had. Both endpoints get one, because an operator who moves one listener off loopback usually
		// has to move the other.
		//
		// Assigned verbatim rather than parsed. Validate rejects a malformed address naming the field,
		// which is more use than a handler that swallows the error and leaves the default in place —
		// what OBJECTFS_METRICS_PORT did, so `OBJECTFS_METRICS_PORT=eighty` was silently ignored.
		{"OBJECTFS_METRICS_ADDR", func(c *Configuration, val string) error {
			c.Monitoring.Metrics.Addr = val
			return nil
		}},
		{"OBJECTFS_HEALTH_ADDR", func(c *Configuration, val string) error {
			c.Monitoring.HealthChecks.Addr = val
			return nil
		}},
		// OBJECTFS_METRICS_ENABLED was documented in cmd/objectfs/doc.go and OBJECTFS.md and assigned
		// nothing — the same defect as the addresses above, in the setting that turns the listener off
		// rather than the one that moves it. It is the only way to close an unauthenticated endpoint
		// without editing a config file, so it is wired rather than undocumented.
		//
		// strconv.ParseBool, and the error is returned, unlike the feature-flag handlers below which
		// coerce anything that is not "true" to false. That asymmetry is deliberate: these two govern
		// listeners that default to on, so a typo coerced to false silently removes an endpoint a probe
		// depends on, and coerced to true silently keeps one an operator asked to close. Both are worse
		// than refusing to start and naming the variable.
		{"OBJECTFS_METRICS_ENABLED", func(c *Configuration, val string) error {
			enabled, err := strconv.ParseBool(val)
			if err != nil {
				return fmt.Errorf("OBJECTFS_METRICS_ENABLED=%q is not a boolean: %w", val, err)
			}

			c.Monitoring.Metrics.Enabled = enabled

			return nil
		}},
		{"OBJECTFS_HEALTH_ENABLED", func(c *Configuration, val string) error {
			enabled, err := strconv.ParseBool(val)
			if err != nil {
				return fmt.Errorf("OBJECTFS_HEALTH_ENABLED=%q is not a boolean: %w", val, err)
			}

			c.Monitoring.HealthChecks.Enabled = enabled

			return nil
		}},

		// Performance settings
		{"OBJECTFS_CACHE_SIZE", func(c *Configuration, val string) error {
			c.Performance.CacheSize = val
			return nil
		}},
		{"OBJECTFS_WRITE_BUFFER_SIZE", func(c *Configuration, val string) error {
			c.Performance.WriteBufferSize = val
			return nil
		}},
		{"OBJECTFS_MAX_CONCURRENCY", func(c *Configuration, val string) error {
			if concurrency, err := strconv.Atoi(val); err == nil {
				c.Performance.MaxConcurrency = concurrency
			}
			return nil
		}},
		// Repointed from the removed performance.compression_enabled to the setting that actually
		// controls compression (#157). The variable's name was never wrong; what it assigned to was
		// read by nothing, so exporting it had no effect on whether objects were compressed.
		{"OBJECTFS_COMPRESSION_ENABLED", func(c *Configuration, val string) error {
			c.Storage.S3.Compression.Enabled = strings.ToLower(val) == TrueValue
			return nil
		}},
		{"OBJECTFS_CONNECTION_POOL_SIZE", func(c *Configuration, val string) error {
			if poolSize, err := strconv.Atoi(val); err == nil {
				c.Performance.ConnectionPoolSize = poolSize
			}
			return nil
		}},

		// Cache settings
		{"OBJECTFS_CACHE_TTL", func(c *Configuration, val string) error {
			if duration, err := time.ParseDuration(val); err == nil {
				c.Cache.TTL = duration
			}
			return nil
		}},

		// Feature flags
		{"OBJECTFS_PREFETCHING", func(c *Configuration, val string) error {
			c.Features.Prefetching = strings.ToLower(val) == TrueValue
			return nil
		}},
		{"OBJECTFS_BATCH_OPERATIONS", func(c *Configuration, val string) error {
			c.Features.BatchOperations = strings.ToLower(val) == TrueValue
			return nil
		}},
		{"OBJECTFS_OFFLINE_MODE", func(c *Configuration, val string) error {
			c.Features.OfflineMode = strings.ToLower(val) == TrueValue
			return nil
		}},

		// S3 storage settings
		// AWS_DEFAULT_REGION is processed first (lowest priority); AWS_REGION
		// overrides it; OBJECTFS_S3_REGION takes highest precedence.
		{"AWS_DEFAULT_REGION", func(c *Configuration, val string) error {
			if c.Storage.S3.Region == "" {
				c.Storage.S3.Region = val
			}
			return nil
		}},
		{"AWS_REGION", func(c *Configuration, val string) error {
			c.Storage.S3.Region = val
			return nil
		}},
		{"OBJECTFS_S3_REGION", func(c *Configuration, val string) error {
			c.Storage.S3.Region = val
			return nil
		}},
		{"OBJECTFS_S3_ENDPOINT", func(c *Configuration, val string) error {
			c.Storage.S3.Endpoint = val
			return nil
		}},
		{"OBJECTFS_S3_STORAGE_TIER", func(c *Configuration, val string) error {
			c.Storage.S3.StorageTier = val
			return nil
		}},

		// Read-ahead. Four variables where there were six, matching the four keys the manager reads
		// (#176). OBJECTFS_READAHEAD_STRATEGY, _PATTERN_DETECTION and _ML_PREDICTION are gone with the
		// settings they assigned to, and OBJECTFS_READ_AHEAD_SIZE is gone with performance.read_ahead_size
		// — its replacement is _READAHEAD_WINDOW_SIZE, which is the same quantity under the name the one
		// consumer uses.
		{"OBJECTFS_READAHEAD_ENABLED", func(c *Configuration, val string) error {
			c.Performance.ReadAhead.Enabled = strings.ToLower(val) == TrueValue
			return nil
		}},
		{"OBJECTFS_READAHEAD_WINDOW_SIZE", func(c *Configuration, val string) error {
			c.Performance.ReadAhead.WindowSize = val
			return nil
		}},
		{"OBJECTFS_READAHEAD_MIN_SEQUENTIAL", func(c *Configuration, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("OBJECTFS_READAHEAD_MIN_SEQUENTIAL=%q is not an integer: %w", val, err)
			}
			c.Performance.ReadAhead.MinSequential = n
			return nil
		}},
		{"OBJECTFS_READAHEAD_CONCURRENT_READS", func(c *Configuration, val string) error {
			n, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("OBJECTFS_READAHEAD_CONCURRENT_READS=%q is not an integer: %w", val, err)
			}
			c.Performance.ReadAhead.ConcurrentReads = n
			return nil
		}},
	}
}

// LoadFromEnv loads configuration from environment variables
func (c *Configuration) LoadFromEnv() error {
	for _, mapping := range getEnvMappings() {
		if val := os.Getenv(mapping.envVar); val != "" {
			if err := mapping.setter(c, val); err != nil {
				return fmt.Errorf("failed to set %s: %w", mapping.envVar, err)
			}
		}
	}
	return nil
}

// SaveToFile saves the configuration to a YAML file
func (c *Configuration) SaveToFile(filename string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(filename), 0750); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	if err := os.WriteFile(filename, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// Validate validates the configuration
func (c *Configuration) Validate() error {
	if c.Performance.MaxConcurrency <= 0 {
		return fmt.Errorf("max_concurrency must be greater than 0")
	}

	if c.Performance.ConnectionPoolSize <= 0 {
		return fmt.Errorf("connection_pool_size must be greater than 0")
	}

	// Only the enabled listener's address is checked. Refusing to start over a malformed address in a
	// block that binds nothing would fail a mount over a setting with no effect — the same reasoning as
	// the disabled-compression arm in validateCompressionConfig.
	if c.Monitoring.Metrics.Enabled {
		if err := validateListenAddr("monitoring.metrics.addr", c.Monitoring.Metrics.Addr); err != nil {
			return err
		}
	}
	if c.Monitoring.HealthChecks.Enabled {
		if err := validateListenAddr("monitoring.health_checks.addr", c.Monitoring.HealthChecks.Addr); err != nil {
			return err
		}
	}

	// Two listeners on one address is a startup failure with no error, not a conflict the OS resolves:
	// whichever binds second logs from a goroutine and returns, leaving the mount up with one endpoint
	// missing. Compared as strings deliberately — "127.0.0.1:8080" and ":8080" are different addresses
	// and do not collide, since the first binds one interface and the second every interface, so
	// normalizing them together would reject a pair that works.
	if c.Monitoring.Metrics.Enabled && c.Monitoring.HealthChecks.Enabled &&
		c.Monitoring.Metrics.Addr == c.Monitoring.HealthChecks.Addr {
		return fmt.Errorf("monitoring.metrics.addr and monitoring.health_checks.addr are both %q; "+
			"two listeners cannot share one address", c.Monitoring.Metrics.Addr)
	}

	validLogLevels := []string{"DEBUG", "INFO", "WARN", "ERROR"}
	logLevelValid := slices.Contains(validLogLevels, c.Global.LogLevel)
	if !logLevelValid {
		return fmt.Errorf("invalid log_level: %s (must be one of: %s)",
			c.Global.LogLevel, strings.Join(validLogLevels, ", "))
	}

	// Validate read-ahead configuration
	if err := c.validateReadAheadConfig(); err != nil {
		return fmt.Errorf("read_ahead configuration invalid: %w", err)
	}

	if err := validateCompressionConfig(c.Storage.S3.Compression); err != nil {
		return fmt.Errorf("storage.s3.compression configuration invalid: %w", err)
	}

	if err := c.validateSizes(); err != nil {
		return err
	}

	if err := c.validateDurations(); err != nil {
		return err
	}

	if err := c.validateS3Config(); err != nil {
		return err
	}

	// The region is checked here, at load, because nothing downstream checks it at all.
	//
	// FuzzConfigConstructsBackend found this: a region containing a space, a newline, or a slash
	// passed Validate, passed buildS3Config, built an AWS client, and then failed inside NewBackend's
	// health check — with "501 NotImplemented", "exceeded maximum number of attempts", or "resolve
	// auth scheme: resolve endpoint: endpoint rule error", none of which mentions the region. Against
	// real S3 in us-west-2, "US-WEST-2" returns 400 and "us west 2" fails endpoint resolution. It is
	// audit finding C1's exact shape in a second setting: accepted by every layer that reads
	// configuration, rejected only by the layer that acts on it, after a mount has been attempted.
	if err := awsname.ValidateRegion(c.Storage.S3.Region); err != nil {
		return fmt.Errorf("storage.s3 configuration invalid: %w", err)
	}

	// An empty region is only valid if something will resolve it, and here is where that can still be
	// said usefully. FuzzConfigConstructsBackend found the gap from the input `storage:` alone — and
	// found it on CI and not locally, because a developer's shell has AWS_REGION or AWS_PROFILE
	// exported and so resolves a region that a container or a systemd unit will not. See
	// [awsname.RegionIsResolvable] for what is and is not checked.
	if !awsname.RegionIsResolvable(c.Storage.S3.Region) {
		return fmt.Errorf("storage.s3 configuration invalid: no region: storage.s3.region is unset " +
			"and none can be resolved from AWS_REGION, AWS_DEFAULT_REGION, or a shared config file. " +
			"Set storage.s3.region (for example \"us-west-2\"), or export AWS_REGION. On EC2 a region " +
			"from instance metadata alone is not detected here, so set it explicitly")
	}

	if err := validateEncryptionConfig(c.Security.Encryption); err != nil {
		return err
	}

	if err := validateMountConfig(c.Mount); err != nil {
		return err
	}

	return nil
}

// validateMountConfig checks what the mount block names, when it names anything.
//
// Both keys are optional — the command line supplies them for an interactive mount — so an empty block
// is valid and says only "these come from the argument list". What is checked is a value that is
// present and unusable, and the point of checking it here is *when*: a systemd template unit reads this
// file to learn which bucket the instance mounts (#134), so a typo in `mount.uri` would otherwise be
// discovered by whichever S3 call ran first, after the mount point already existed, with a message that
// names neither the file nor the key.
func validateMountConfig(cfg MountConfig) error {
	if cfg.URI != "" {
		if err := awsname.ValidateStorageURI(cfg.URI); err != nil {
			return fmt.Errorf("mount.uri is invalid: %w", err)
		}
	}

	if cfg.MountPoint == "" {
		return nil
	}

	// Only the shape is checked, not the directory. Whether the path exists, is a directory, and is
	// empty is `objectfs mount`'s question and is asked there — those answers change between config load
	// and mount, and a systemd unit's ExecStartPre creates the directory after the config is read.
	if !filepath.IsAbs(cfg.MountPoint) {
		return fmt.Errorf("mount.mount_point is %q, which is relative; a mount point read from a "+
			"config file has no reliable working directory to be relative to — a systemd unit's is "+
			"whatever WorkingDirectory happens to be — so it must be absolute", cfg.MountPoint)
	}

	if cfg.MountPoint != filepath.Clean(cfg.MountPoint) {
		return fmt.Errorf("mount.mount_point is %q; write it as %q. A path containing \"..\" or a "+
			"doubled separator is the one shape worth refusing rather than cleaning, because the "+
			"cleaned form is what gets mounted over and the operator would be reading the other one",
			cfg.MountPoint, filepath.Clean(cfg.MountPoint))
	}

	if cfg.MountPoint == "/" {
		return fmt.Errorf("mount.mount_point is \"/\"; mounting over the root filesystem is not " +
			"something ObjectFS will do")
	}

	return nil
}

// validateEncryptionConfig rejects an encryption block that cannot mean what it says.
//
// The backend validates this too, in NewBackend, and the duplication is the same trade the region
// makes: this catches it at load with the YAML path in the message, and the backend catches the SDK
// path where a caller hand-builds an s3.Config and never passes through this loader. Neither is
// redundant, because the two entry points have no layer in common that could hold the check.
//
// What it does *not* do is check that the key exists, is enabled, is in the bucket's region, or grants
// this principal kms:GenerateDataKey. Those are questions only KMS can answer, and asking at load
// would make mounting depend on a second service being reachable. They surface on the first write with
// S3 naming the key — which is the difference from P-7, where nothing surfaced because nothing was
// sent.
func validateEncryptionConfig(cfg EncryptionConfig) error {
	if err := awsname.ValidateSSEMode(cfg.Mode); err != nil {
		return fmt.Errorf("security.encryption.mode is invalid: %w", err)
	}

	// Each arm below rejects a combination that is inert rather than wrong-on-the-wire, deliberately.
	// Ignoring a KMS key set beside mode "off" would send no header and report no problem, which is
	// P-7 reproduced exactly: the operator has written the word "encryption" in their config, named a
	// key, and been told everything is fine.
	switch cfg.Mode {
	case "", EncryptionModeOff:
		if cfg.KMSKeyID != "" {
			return fmt.Errorf("security.encryption: kms_key_id is set but mode is %q, so no encryption "+
				"header is sent and the key is unused; set mode to %q to encrypt with it, or remove it",
				cfg.Mode, EncryptionModeKMS)
		}

	case EncryptionModeS3:
		if cfg.KMSKeyID != "" {
			return fmt.Errorf("security.encryption: kms_key_id is set but mode is %q, which encrypts "+
				"with S3's own keys and cannot use a KMS key; set mode to %q to use the key, or remove it",
				EncryptionModeS3, EncryptionModeKMS)
		}

	case EncryptionModeKMS:
		if cfg.KMSKeyID == "" {
			return fmt.Errorf("security.encryption: mode is %q but kms_key_id is empty; S3 would fall "+
				"back to the AWS managed key aws/s3, which is shared with every other service in the "+
				"account and cannot be audited or revoked separately from the data — name a key, or use "+
				"mode %q if S3-managed keys are what you want", EncryptionModeKMS, EncryptionModeS3)
		}

		if err := awsname.ValidateKMSKeyID(cfg.KMSKeyID); err != nil {
			return fmt.Errorf("security.encryption: %w", err)
		}
	}

	if cfg.BucketKeys && cfg.Mode != EncryptionModeKMS {
		return fmt.Errorf("security.encryption: bucket_keys is set but mode is %q; bucket keys reduce "+
			"SSE-KMS's per-object KMS calls and do nothing without mode %q", cfg.Mode, EncryptionModeKMS)
	}

	return nil
}

// validateListenAddr rejects a monitoring listen address the runtime cannot bind.
//
// Both listeners now bind synchronously and return the error, so a malformed address does fail the
// mount without this check. What this adds is where the failure is reported: net.Listen can only say
// "address 127.0.0.1:99999: invalid port", naming neither the setting nor the file. Validation here
// names the field an operator has to edit, and does it before the backend is constructed and a bucket
// is contacted. Both servers used to bind on a goroutine and log — metrics from inside a `go func()`
// whose result nothing read, health from startHTTPServer, which logged and returned — so the mount
// came up working with no endpoint and one line in a log the operator had no reason to be reading.
//
// port is what this most needs to catch. `health_port: 99999` was a config an operator could write;
// it reached net.Listen as "[::]:99999" and failed there, producing exactly that silent
// no-endpoint (#192). net.SplitHostPort accepts it — "99999" is a syntactically fine port string —
// so the range is checked here explicitly.
//
// A name is allowed to fail resolution: "localhost" is the address an operator is most likely to
// write, and refusing anything that does not parse as an IP would reject it. What is checked is the
// shape, which is the part a typo breaks.
func validateListenAddr(field, addr string) error {
	if addr == "" {
		return fmt.Errorf("%s must be set to a host:port; leave it at its default or set enabled: false "+
			"to turn the endpoint off", field)
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s is not a host:port address: %q (%w)", field, addr, err)
	}

	// An empty host is the wildcard, which is legal and is what this change stopped doing by default.
	// Written explicitly it is a choice, so it is accepted.
	_ = host

	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("%s has a non-numeric port: %q. A service name is not looked up in "+
			"/etc/services here; write the number", field, port)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("%s port %d is outside 1-65535. Port 0 is not how these endpoints are "+
			"disabled — set the enabled flag beside this field to false", field, n)
	}

	return nil
}

// validateCompressionConfig rejects a compression configuration no codec can be built from.
//
// This check is the seam that produced the worst defect in v0.10.0. The algorithm was defaulted to
// "gzip" here, pkg/compression declared AlgorithmGzip, internal/storage/s3's config comment listed
// it, and two shipped example config files set it — while internal/compression's codec factory had
// no gzip case. Every layer that read config agreed the value was valid, so the disagreement only
// surfaced inside NewBackend, by which point the user had asked for a mount and got
// "Failed to start adapter" with no indication which setting was at fault.
//
// It validates by *building the codec* rather than by comparing against a list of names. A list here
// would be a second authority, free to drift from the factory exactly as the first one did, and it
// could not catch a level that is out of range for the chosen algorithm at all — zstd accepts 0-22
// and gzip only 0-9, so "level: 12" is valid for one and not the other. Construction is cheap and it
// is the same call the backend will make.
func validateCompressionConfig(cfg CompressionConfig) error {
	if !cfg.Enabled {
		// A disabled block is not consulted, and rejecting a stale algorithm in it would refuse to
		// start over a setting that has no effect.
		return nil
	}

	if _, err := compression.NewCompressor(compression.Settings{
		Enabled:   true,
		Algorithm: cfg.Algorithm,
		Level:     cfg.Level,
		MinSize:   cfg.MinSize,
	}); err != nil {
		return err
	}

	return nil
}

// validateS3Config rejects storage.s3 settings the backend cannot act on.
//
// Same reasoning as validateCompressionConfig above and awsname.ValidateRegion below: the value is
// checked by the layer that reads configuration, because the layer that acts on it either cannot
// report a useful error or does not check at all. Specifically, an unrecognized storage class is
// silently replaced with STANDARD inside NewTierValidator, so without this check `storage_tier:
// STANDARD_1A` mounts successfully and bills as STANDARD forever.
func (c *Configuration) validateS3Config() error {
	s3cfg := c.Storage.S3

	if err := awsname.ValidateStorageClass(s3cfg.StorageTier); err != nil {
		return fmt.Errorf("storage.s3.storage_tier is invalid: %w", err)
	}

	if s3cfg.MaxRetries < 0 {
		return fmt.Errorf("storage.s3.max_retries must not be negative, got %d", s3cfg.MaxRetries)
	}

	if s3cfg.Multipart.Concurrency < 0 {
		return fmt.Errorf("storage.s3.multipart.concurrency must not be negative, got %d",
			s3cfg.Multipart.Concurrency)
	}

	// Ordering matters and is asserted by a test: a chunk size above the threshold means the first
	// part of every multipart upload is the whole object, so multipart never engages at all.
	threshold, err := utils.ParseOptionalBytes(s3cfg.Multipart.Threshold)
	if err != nil {
		return fmt.Errorf("storage.s3.multipart.threshold is invalid: %w", err)
	}

	chunk, err := utils.ParseOptionalBytes(s3cfg.Multipart.ChunkSize)
	if err != nil {
		return fmt.Errorf("storage.s3.multipart.chunk_size is invalid: %w", err)
	}

	if threshold > 0 && chunk > threshold {
		return fmt.Errorf("storage.s3.multipart.chunk_size (%s) is larger than "+
			"storage.s3.multipart.threshold (%s), so an upload large enough to be split would "+
			"still be a single part", s3cfg.Multipart.ChunkSize, s3cfg.Multipart.Threshold)
	}

	return nil
}

// validateSizes rejects every size-valued setting the loader accepts and the adapter would then
// have to interpret.
//
// These are checked here rather than where they are used because of what "where they are used" did:
// internal/adapter.parseSize returned 1 GiB — silently, with no error — for any string it could not
// parse, so `cache_size: 2G` configured a 1 GiB cache, `cache_size: 64MiB` configured a 1 GiB cache,
// and `cache_size: tpyo` configured a 1 GiB cache. Three different mistakes, one wrong answer, no
// message. utils.ParseBytes is strict now, but a strict parser at the point of use still fails
// late — after a mount has been attempted — and several of these values are read in constructors
// that have nowhere to return an error to.
//
// Every entry names the YAML path rather than the Go field, because the operator's next action is to
// edit a line in a file.
func (c *Configuration) validateSizes() error {
	sizes := []struct {
		path  string
		value string

		// required marks a size that must be present. An optional one may be empty, meaning "use the
		// built-in default" — which is how a partial config file is meant to work.
		required bool
	}{
		{path: "performance.cache_size", value: c.Performance.CacheSize, required: true},
		{path: "performance.write_buffer_size", value: c.Performance.WriteBufferSize},
		{path: "performance.read_ahead.window_size", value: c.Performance.ReadAhead.WindowSize},
		{path: "performance.parallel_read.threshold", value: c.Performance.ParallelRead.Threshold},
		{path: "performance.parallel_read.chunk_size", value: c.Performance.ParallelRead.ChunkSize},
		{path: "cache.persistent_cache.max_size", value: c.Cache.PersistentCache.MaxSize},
		{path: "write_buffer.max_memory", value: c.WriteBuffer.MaxMemory},
		{path: "storage.s3.multipart.threshold", value: c.Storage.S3.Multipart.Threshold},
		{path: "storage.s3.multipart.chunk_size", value: c.Storage.S3.Multipart.ChunkSize},
	}

	for _, size := range sizes {
		if size.value == "" {
			if size.required {
				return fmt.Errorf("%s must be set (for example \"2GB\")", size.path)
			}

			continue
		}

		if _, err := utils.ParseBytes(size.value); err != nil {
			return fmt.Errorf("%s is invalid: %w", size.path, err)
		}
	}

	// storage.s3.compression.min_size is validated by validateCompressionConfig, which builds the
	// codec — and only when compression is enabled, since a disabled block is not consulted.

	return nil
}

// minSaneDuration is the floor below which a non-zero duration is taken to be a unit mistake rather
// than a deliberate setting.
//
// One millisecond, because nothing in this schema is a setting anyone would want below it — the
// smallest deliberate value in any shipped config is a 1-second retry delay, and the fastest of these
// is a health-check interval — while every value produced by the unit trap below is far under it. A
// millisecond timeout on an S3 request over the internet cannot succeed, so nothing is being refused
// that would have worked.
const minSaneDuration = time.Millisecond

// validateDurations rejects a duration that yaml.v2 read as nanoseconds because it carried no unit.
//
// This is the whole defect, and it is not about any one setting. gopkg.in/yaml.v2 decodes a bare
// integer into a time.Duration by taking it as a raw nanosecond count, with no error — so
// `read: 30`, which is what someone writing a 30-second timeout tries first, configures **30
// nanoseconds**, and `-5` is accepted as a negative duration. Nothing downstream catches it: every
// consumer defends against zero (internal/circuit.NewBreaker, Checker.checkLoop and
// Monitor.monitorLoop all substitute a default at <= 0) and a small positive passes every one of
// those guards.
//
// What that produced, found by FuzzConfigConstructsBackend from the three-line document
// `network:\n  timeouts:\n    read: 2`: the value becomes the transport's ResponseHeaderTimeout, so
// every request fails before S3 can answer, and the mount dies inside NewBackend's health check with
// "exceeded maximum number of attempts ... timeout awaiting response headers". That message names a
// network problem. The operator has a config file with a plausible-looking number in it and an error
// pointing at their network, which is audit finding C1's exact shape: accepted by every layer that
// reads configuration, fatal at the layer that acts on it, attributed to neither.
//
// It is checked here, at load, rather than each consumer clamping, because a clamp would silently
// substitute a duration the operator did not ask for — and the value they wrote is not a value they
// meant, so honoring any interpretation of it is worse than refusing.
//
// Zero remains valid throughout and means "use the built-in default", which is how a partial config
// file works. The message states the unit rule, since the fix is to add a suffix, not to pick a
// different number.
func (c *Configuration) validateDurations() error {
	durations := []struct {
		path  string
		value time.Duration
	}{
		{"network.timeouts.connect", c.Network.Timeouts.Connect},
		{"network.timeouts.read", c.Network.Timeouts.Read},
		{"network.timeouts.write", c.Network.Timeouts.Write},
		{"network.retry.base_delay", c.Network.Retry.BaseDelay},
		{"network.retry.max_delay", c.Network.Retry.MaxDelay},
		{"network.circuit_breaker.timeout", c.Network.CircuitBreaker.Timeout},
		{"monitoring.health_checks.interval", c.Monitoring.HealthChecks.Interval},
		{"monitoring.health_checks.timeout", c.Monitoring.HealthChecks.Timeout},
		{"cache.ttl", c.Cache.TTL},
		{"cluster.redis.ttl", c.Cluster.Redis.TTL},

		// Checked here and not in validateReadAheadConfig, even though that function checks the other
		// four read-ahead keys, because that one returns early on a disabled block and this trap does
		// not care whether the feature is on: `ttl: 5` is a mistake either way, and a mount that is
		// later enabled would inherit it silently. Same boundary validateSizes draws — syntax always,
		// semantics only where the value is read. This entry was added because the reflection walk in
		// validate_durations_test.go found it the moment #176 gave the block a duration at all.
		{"performance.read_ahead.ttl", c.Performance.ReadAhead.TTL},

		// Not wired — vfs.NewWriter takes no configuration and nothing drives a periodic flush — and
		// checked anyway. A value that is wrong now stays wrong when it is wired, and it would then be
		// wrong in a release that changed nothing about it. This entry was added because the
		// reflection walk in validate_durations_test.go found it and the hand-written list did not:
		// the field is declared with different alignment from the others, so the grep that produced
		// that list missed it. Which is the walk's whole point.
		{"write_buffer.flush_interval", c.WriteBuffer.FlushInterval},
	}

	for _, d := range durations {
		if d.value == 0 {
			continue
		}

		if d.value < 0 {
			return fmt.Errorf("%s must not be negative, got %s. Durations take a unit suffix "+
				"(\"30s\", \"5m\", \"1h\"); a bare number is read as nanoseconds", d.path, d.value)
		}

		if d.value < minSaneDuration {
			return fmt.Errorf("%s is %s, which is almost certainly a missing unit: a duration "+
				"written without a suffix is read as nanoseconds, so \"%d\" means %s and not %ds. "+
				"Write it as \"%ds\" (or \"%dms\", \"%dm\") — the accepted suffixes are ns, us, ms, "+
				"s, m, h", d.path, d.value, int64(d.value), d.value, int64(d.value), int64(d.value),
				int64(d.value), int64(d.value))
		}
	}

	// max_attempts is not a duration, but it is in the same block and has the same shape of failure:
	// negative is meaningless and pkg/retry would treat it as no attempts at all.
	if c.Network.Retry.MaxAttempts < 0 {
		return fmt.Errorf("network.retry.max_attempts must not be negative, got %d",
			c.Network.Retry.MaxAttempts)
	}

	if c.Network.CircuitBreaker.FailureThreshold < 0 {
		return fmt.Errorf("network.circuit_breaker.failure_threshold must not be negative, got %d",
			c.Network.CircuitBreaker.FailureThreshold)
	}

	return nil
}

// validateReadAheadConfig rejects a read-ahead configuration the manager cannot run with.
//
// Only the values the manager reads are checked, which is the point of the change that shrank this
// function. It used to range-check nine keys — sequential_threshold, learning_rate, pattern_depth,
// prefetch_bandwidth_mbs and the rest — none of which reached any code, so a mount could be refused
// over a setting that would have had no effect had it been accepted (#176).
//
// A disabled block is not validated, for the same reason a disabled listener's address is not: failing
// a mount over a value nothing will read is the defect above in a different direction.
func (c *Configuration) validateReadAheadConfig() error {
	ra := c.Performance.ReadAhead
	if !ra.Enabled {
		return nil
	}

	// ConcurrentReads is the worker count, and zero is not "no limit": NewReadAheadManager starts one
	// goroutine per worker, so zero starts none and every prefetch is queued and never performed —
	// read-ahead silently off while the configuration says it is on.
	if ra.ConcurrentReads <= 0 {
		return fmt.Errorf("performance.read_ahead.concurrent_reads must be greater than 0, got %d; "+
			"it is the prefetch worker count, and zero starts no workers rather than removing a limit",
			ra.ConcurrentReads)
	}

	if ra.MinSequential <= 0 {
		return fmt.Errorf("performance.read_ahead.min_sequential must be greater than 0, got %d; "+
			"prefetching before any sequential read has been observed would fetch ahead of a random "+
			"reader", ra.MinSequential)
	}

	if ra.TTL <= 0 {
		return fmt.Errorf("performance.read_ahead.ttl must be greater than 0, got %s; it is how long "+
			"an idle read pattern is remembered, and zero expires every pattern before the next read",
			ra.TTL)
	}

	// WindowSize is parsed here as well as in validateSizes, because that check accepts an empty string
	// and this one must not: an empty window is a zero floor, which leaves the prefetch equal to the
	// reader's last read and is a different behavior from the documented default.
	if ra.WindowSize == "" {
		return fmt.Errorf("performance.read_ahead.window_size must be set when read-ahead is enabled; " +
			"it is the floor on the prefetch length, and an empty value is a floor of zero")
	}

	if _, err := utils.ParseBytes(ra.WindowSize); err != nil {
		return fmt.Errorf("performance.read_ahead.window_size is not a size: %q (%w)", ra.WindowSize, err)
	}

	return nil
}
