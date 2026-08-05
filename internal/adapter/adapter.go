package adapter

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/fuse"
	"github.com/scttfrdmn/objectfs/internal/health"
	"github.com/scttfrdmn/objectfs/internal/metrics"
	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/vfs"
	"github.com/scttfrdmn/objectfs/pkg/retry"
	"github.com/scttfrdmn/objectfs/pkg/types"
	"github.com/scttfrdmn/objectfs/pkg/utils"
)

// Fallback used only if a configured size fails to parse here after having passed
// Configuration.Validate — see sizeOrDefault. It matches internal/config's NewDefault so that the
// fallback is the documented default rather than a third number.
//
// The two cache capacities were here too until the cache construction moved to cache.NewFromConfig
// (#178); their fallbacks live beside that mapping now, for the same reason the mapping does.
const defaultWriteBufferMemory = 512 << 20 // 512 MiB, matching NewDefault's "512MB"

// Adapter represents the main ObjectFS adapter
type Adapter struct {
	storageURI string
	mountPoint string
	config     *config.Configuration

	// Core components.
	//
	// cache is the interface and not *cache.MultiLevelCache, because which implementation a mount gets
	// is a configuration decision: `cluster.redis.enabled` selects a Redis-backed distributed cache
	// instead. Naming the concrete type here is what kept cache.NewFromConfig from having a caller
	// (#178) and so left the whole cluster: block unreachable — the field could not hold what the
	// function returns.
	backend     *s3.Backend
	cache       types.Cache
	writeBuffer *vfs.Writer
	mountMgr    fuse.PlatformFileSystem
	metrics     *metrics.Collector
	monitor     *health.Monitor

	// mountCtx bounds work that outlives the call that started it — a flush runs when the kernel
	// decides to, which is typically long after Start has returned. Start's own ctx is the wrong
	// lifetime for that: cmd/objectfs cancels it on shutdown before Stop finishes flushing.
	//
	// v0.10.0 hit this and worked around it by hardcoding context.Background() inside the flush
	// callback (#100), which made a flush uncancelable — an unmount could not interrupt one, and a
	// hung PUT hung the unmount.
	mountCtx    context.Context
	cancelMount context.CancelFunc

	// Internal state
	started    bool
	bucketName string
	s3Config   *s3.Config
}

// healthComponent adapts a closure to the health.HealthyComponent interface so
// that adapter-owned components can be registered with the health monitor without
// requiring them to implement the interface directly.
type healthComponent struct {
	name     string
	compType string
	fn       func(context.Context) error
}

func (h *healthComponent) HealthCheck(ctx context.Context) error { return h.fn(ctx) }
func (h *healthComponent) GetComponentName() string              { return h.name }
func (h *healthComponent) GetComponentType() string              { return h.compType }

// New creates a new ObjectFS adapter instance
func New(ctx context.Context, storageURI, mountPoint string, cfg *config.Configuration) (*Adapter, error) {
	// One parse, in the package that owns what a storage URI is. This used to validate the URI with a
	// local function and then url.Parse it a second time to recover the bucket, which is how the bucket
	// came to be taken as `strings.TrimPrefix(parsed.Host, "")` — a no-op call whose only effect was to
	// make an unvalidated field look checked. The validating parse now returns the bucket, so there is
	// no second reading to disagree with the first. internal/config performs the same check on
	// `mount.uri` at config load, against this same function (#134).
	uri, err := awsname.ParseStorageURI(storageURI)
	if err != nil {
		return nil, fmt.Errorf("invalid storage URI: %w", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	adapter := &Adapter{
		storageURI: storageURI,
		mountPoint: mountPoint,
		config:     cfg,
		bucketName: uri.Bucket,
	}

	return adapter, nil
}

// Start initializes and starts the adapter
func (a *Adapter) Start(ctx context.Context) error {
	if a.started {
		return fmt.Errorf("adapter already started")
	}

	slog.Info("starting ObjectFS adapter")
	slog.Info("adapter configuration", "storage_uri", a.storageURI, "mount_point", a.mountPoint, "cache_size", a.config.Performance.CacheSize, "max_concurrency", a.config.Performance.MaxConcurrency)

	// 1. Initialize and start the metrics collector.
	if err := a.startMetrics(ctx); err != nil {
		return err
	}

	// 2. Initialize S3 backend
	var err error
	a.s3Config = a.buildS3Config()
	slog.Info("S3 backend", "region", a.s3Config.Region)

	a.backend, err = s3.NewBackend(ctx, a.bucketName, a.s3Config)
	if err != nil {
		return fmt.Errorf("failed to initialize S3 backend: %w", err)
	}

	// 3. Initialize the cache.
	//
	// Through NewFromConfig, which is what makes the cluster: block reachable at all: it selects the
	// Redis-backed distributed cache when cluster.enabled and cluster.redis.enabled are both set, and
	// otherwise builds the in-process MultiLevelCache from the cache: block.
	//
	// This used to be a MultiLevelConfig literal built here, and NewFromConfig had no caller (#178). So
	// seven cluster keys plus a seven-key redis: sub-block were decoded, defaulted and validated while
	// no mount consulted any of them: a deployment that configured a shared Redis cache got a private
	// in-process one, with no error and no warning, and looked correct until two nodes disagreed about
	// a file. The mapping moved into internal/cache with the selection, because a duplicate of it here
	// is how the two came to disagree in the first place.
	a.cache, err = cache.NewFromConfig(ctx, a.config)
	if err != nil {
		return fmt.Errorf("failed to initialize cache: %w", err)
	}

	// 4. Initialize the write path.
	//
	// vfs.Writer tracks dirty byte ranges per path and flushes by read-modify-write: fetch the parts
	// of the object the pending writes do not cover, splice, then PUT the whole result. That is what
	// makes an offset write mean "modify these bytes" rather than "replace the object".
	//
	// It replaces internal/buffer.WriteBuffer, whose flush callback took the offset and dropped it:
	//
	//	flushCallback := func(key string, data []byte, offset int64) error {
	//	    return a.backend.PutObject(context.Background(), key, data)
	//	}
	//
	// PutObject replaces the whole object, so appending one byte to a 1 MiB file left a 1-byte object
	// and reported success. See internal/vfs for the other five defects that representation caused.
	//
	// The context is the mount's, not Start's: see the mountCtx field.
	a.mountCtx, a.cancelMount = context.WithCancel(context.WithoutCancel(ctx))

	//nolint:contextcheck // Deliberately not Start's context. The write path outlives Start by
	// design — it holds dirty ranges for as long as the mount does — so inheriting a context that
	// is canceled when Start returns would cancel every flush the moment the mount came up. The
	// WithoutCancel above is the whole point, and mountCtx is what Stop cancels instead.
	a.writeBuffer, err = vfs.NewWriterWithOptions(a.mountCtx, a.backend, a.buildWriterOptions())
	if err != nil {
		return fmt.Errorf("failed to initialize write path: %w", err)
	}

	// 5. Initialize platform-specific FUSE filesystem
	mountConfig := &fuse.MountConfig{
		MountPoint: a.mountPoint,
		Options:    a.buildMountOptions(),
		ReadAhead:  a.buildReadAheadConfig(),
	}

	a.mountMgr = fuse.CreatePlatformMountManager(a.backend, a.cache, a.writeBuffer, a.metrics, mountConfig)

	// 6. Initialize and start health monitor
	if a.config.Monitoring.HealthChecks.Enabled {
		monCfg := &health.MonitorConfig{
			Enabled:          true,
			MonitorInterval:  a.config.Monitoring.HealthChecks.Interval,
			AlertingEnabled:  false,
			AutoRecovery:     false,
			ReportingEnabled: false,
			HealthCheckConfig: &health.Config{
				Enabled:       true,
				CheckInterval: a.config.Monitoring.HealthChecks.Interval,
				Timeout:       a.config.Monitoring.HealthChecks.Timeout,
				MaxFailures:   3,
				// monitoring.health_checks.addr, which is a real setting as of #202 — global.health_port
				// was, and monitoring.health_check_addr, the one an operator would reach for to move
				// this listener off the wildcard, was declared, defaulted, documented and read by
				// nothing. HTTPEnabled is the enclosing `if`, not a second switch derived from a port
				// being non-zero: `health_port: 0` was how the endpoint got disabled, which is exactly
				// the overload an address cannot express and no longer has to.
				HTTPEnabled: true,
				HTTPAddr:    a.config.Monitoring.HealthChecks.Addr,
				HTTPPath:    "/health",
			},
		}
		a.monitor, err = health.NewMonitor(monCfg)
		if err != nil {
			return fmt.Errorf("failed to initialize health monitor: %w", err)
		}

		// Register per-component health checks using the adapter's live references.
		components := []health.HealthyComponent{
			&healthComponent{
				name:     "s3_backend",
				compType: "storage",
				fn: func(ctx context.Context) error {
					if a.backend == nil {
						return fmt.Errorf("s3 backend not initialized")
					}
					return nil
				},
			},
			&healthComponent{
				name:     "cache",
				compType: "cache",
				fn: func(ctx context.Context) error {
					if a.cache == nil {
						return fmt.Errorf("cache not initialized")
					}
					return nil
				},
			},
			&healthComponent{
				name:     "write_buffer",
				compType: "storage",
				fn: func(ctx context.Context) error {
					if a.writeBuffer == nil {
						return fmt.Errorf("write buffer not initialized")
					}
					return nil
				},
			},
		}
		for _, comp := range components {
			if err := a.monitor.RegisterComponent(comp); err != nil {
				slog.Warn("failed to register health check", "component", comp.GetComponentName(), "error", err)
			}
		}

		if err := a.monitor.Start(ctx); err != nil {
			return fmt.Errorf("failed to start health monitor: %w", err)
		}
		slog.Info("health monitor started", "addr", a.config.Monitoring.HealthChecks.Addr)
	}

	// 7. Mount filesystem
	if err := a.mountMgr.Mount(ctx); err != nil {
		return fmt.Errorf("failed to mount filesystem: %w", err)
	}

	a.started = true
	slog.Info("ObjectFS adapter started successfully")
	return nil
}

// Stop gracefully stops the adapter
func (a *Adapter) Stop(ctx context.Context) error {
	if !a.started {
		return fmt.Errorf("adapter not started")
	}

	slog.Info("stopping ObjectFS adapter")

	var lastErr error

	// 1. Unmount filesystem
	if a.mountMgr != nil && a.mountMgr.IsMounted() {
		if err := a.mountMgr.Unmount(); err != nil {
			slog.Error("error unmounting filesystem", "error", err)
			lastErr = err
		}
	}

	// 2. Flush the write path.
	//
	// This is the last point at which unflushed data can still be saved, so it runs with the caller's
	// ctx — a shutdown that is given a deadline should honor it — and before the mount context is
	// canceled. Closing the backend first, or canceling first, would turn every pending write into
	// silent loss.
	//
	// Close flushes too, but FlushAllContext is called explicitly so the error is attributed to
	// flushing rather than to closing, and so ctx is used rather than the mount's.
	if a.writeBuffer != nil {
		if err := a.writeBuffer.FlushAllContext(ctx); err != nil {
			slog.Error("error flushing write path; data may not be durable", "error", err)
			lastErr = err
		}
		if err := a.writeBuffer.Close(); err != nil {
			slog.Error("error closing write path", "error", err)
			lastErr = err
		}
	}

	// 3. Cancel the mount context, releasing anything still blocked on a backend call. Only after the
	// flush above: canceling first would abort the flush it exists to permit.
	if a.cancelMount != nil {
		a.cancelMount()
	}

	// 4. Close backend connections
	if a.backend != nil {
		if err := a.backend.Close(); err != nil {
			slog.Error("error closing backend", "error", err)
			lastErr = err
		}
	}

	// 5. Stop health monitor
	if a.monitor != nil {
		if err := a.monitor.Stop(); err != nil {
			slog.Error("error stopping health monitor", "error", err)
			lastErr = err
		}
	}

	// 6. Clear the cache, then release the goroutines behind it.
	//
	// The Close is what retires the prefetch workers. Prefetch is enabled unconditionally for the
	// in-process cache, so that mount wraps L1 in a predictive cache with four workers and a statistics
	// ticker, and until this call existed nothing ever stopped them: a process that mounted and
	// unmounted repeatedly accumulated a set per mount, each holding a reference to the cache it was
	// built over.
	//
	// Both are type assertions because types.Cache declares neither, and it should not: Clear on a
	// shared Redis cache would discard every *other* node's cached bytes as well, which is not what
	// unmounting one mount means, and Close on the interface would oblige every implementation to have
	// a lifecycle it may not have. So the interface stays the read/write contract and lifecycle is
	// asked for rather than assumed — the assertion failing is the correct outcome for a cache with no
	// local state to clear.
	if a.cache != nil {
		if clearer, ok := a.cache.(interface{ Clear() }); ok {
			clearer.Clear()
		}

		if closer, ok := a.cache.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				slog.Error("error closing cache", "error", err)
				lastErr = err
			}
		}
	}

	// 7. Stop metrics collection
	if a.metrics != nil {
		if err := a.metrics.Stop(ctx); err != nil {
			slog.Error("error stopping metrics collector", "error", err)
			lastErr = err
		}
	}

	a.started = false
	slog.Info("ObjectFS adapter stopped successfully")
	return lastErr
}

// startMetrics constructs the metrics collector and binds its HTTP endpoint.
//
// Start is what serves /metrics. Without it the collector recorded into a registry no one could read:
// `monitoring.metrics.enabled: true` and `global.metrics_port: 8080` were both honored as far as
// constructing the counters, the mount logged nothing amiss, and a scrape of the port got connection
// refused. Both SDKs' get_metrics(), every documented Prometheus example, and docs/monitoring were
// describing an endpoint that was never bound.
//
// It is a method of its own rather than nine lines inside Start because Start's remaining steps need a
// bucket, a mountable directory, and a FUSE-capable kernel, so a test that goes through Start cannot
// reach this at all — which is how the missing call survived. TestStartMetricsBindsTheEndpoint scrapes
// what this binds; deleting the Start call below fails it.
func (a *Adapter) startMetrics(ctx context.Context) error {
	var err error
	a.metrics, err = metrics.NewCollector(&metrics.Config{
		Enabled: a.config.Monitoring.Metrics.Enabled,
		// monitoring.metrics.addr, live as of #202. This read global.metrics_port, and an operator who
		// wanted the endpoint on loopback only had no way to say so: monitoring.metrics_addr existed for
		// exactly that and reached nothing.
		Addr:   a.config.Monitoring.Metrics.Addr,
		Labels: a.config.Monitoring.Metrics.CustomLabels,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize metrics collector: %w", err)
	}

	// The context governs the collector's periodic-update goroutine, so it must be one that lives as
	// long as the mount rather than a request-scoped one.
	if err := a.metrics.Start(ctx); err != nil {
		return fmt.Errorf("failed to start metrics collector: %w", err)
	}

	if a.config.Monitoring.Metrics.Enabled {
		// The bound address, not the configured one. They differ when the configured port is 0, and a
		// log line naming a port nothing is listening on is how the missing bind went unnoticed before.
		slog.Info("metrics server started", "addr", a.metrics.Addr(), "path", "/metrics")
	}

	return nil
}

// buildS3Config translates the loaded configuration into the backend's configuration.
//
// This function is audit finding D12, and the finding was not that it was wrong — it was that it was
// *short*. It mapped six of s3.Config's thirty fields, so on every real mount everything else
// arrived at the backend zero-valued: the storage tier, the connection pool size, the retry limit,
// the timeouts, the multipart settings, and the parallel-read threshold. Each of those is named in
// examples/config.yaml, and each of them did nothing.
//
// Two of the omissions were not merely inert. `PoolSize: 0` reached GetObjects and PutObjects as
// `make(chan struct{}, 0)`, an unbuffered semaphore whose first send blocks forever — so a batch
// operation on a stock mount hung rather than failed. And `ParallelReadThreshold: 0` closed the gate
// on parallel range GETs, which is the feature v0.10.0 was released for: the code existed, had
// tests, and was unreachable from a mount.
//
// So the shape of this function matters more than its content. It is written as one explicit
// assignment per field, ordered to match s3.Config's declaration, because the defect was a mapping
// somebody extended one field at a time and stopped extending.
// TestBuildS3ConfigMapsEveryConfiguredValue asserts the output values rather than recomputing the
// mapping — a test that reapplied the formula here would agree with any formula, including a wrong
// one.
//
// It cannot return an error. Every string it parses has already been through
// Configuration.Validate — utils.ParseBytes on the same value, by the same rules, reachable from
// [New] before a mount is attempted — so a parse failure here would mean Validate and this function
// disagree, which is the very seam this whole task is about. sizeOrDefault therefore falls back to
// the backend's default and says so in a log line, rather than silently substituting the way the old
// parseSize substituted 1 GiB.
func (a *Adapter) buildS3Config() *s3.Config {
	s3cfg := a.config.Storage.S3
	defaults := s3.NewDefaultConfig()

	cfg := &s3.Config{
		Region:         s3cfg.Region,
		Endpoint:       s3cfg.Endpoint,
		ForcePathStyle: s3cfg.ForcePathStyle,

		MaxRetries:     s3cfg.MaxRetries,
		ConnectTimeout: a.config.Network.Timeouts.Connect,
		RequestTimeout: a.config.Network.Timeouts.Read,
		PoolSize:       a.config.Performance.ConnectionPoolSize,

		RetryConfig: a.buildRetryConfig(),

		CircuitBreaker: s3.CircuitBreakerConfig{
			Enabled:          a.config.Network.CircuitBreaker.Enabled,
			FailureThreshold: a.config.Network.CircuitBreaker.FailureThreshold,
			Timeout:          a.config.Network.CircuitBreaker.Timeout,
		},

		UseAccelerate: s3cfg.UseAcceleration,

		MultipartThreshold: a.sizeOrDefault("storage.s3.multipart.threshold",
			s3cfg.Multipart.Threshold, defaults.MultipartThreshold),
		MultipartChunkSize: a.sizeOrDefault("storage.s3.multipart.chunk_size",
			s3cfg.Multipart.ChunkSize, defaults.MultipartChunkSize),
		MultipartConcurrency: s3cfg.Multipart.Concurrency,

		StorageTier: s3cfg.StorageTier,

		// Read from storage.s3.compression, not write_buffer.compression (#157). Nothing ever
		// compressed a write buffer; the block always configured the codec the S3 backend applies to
		// whole objects, and reading it from two sections away meant an operator tuning the write
		// buffer changed how objects were stored on the wire.
		Compression: s3.CompressionConfig{
			Enabled:   s3cfg.Compression.Enabled,
			Algorithm: s3cfg.Compression.Algorithm,
			Level:     s3cfg.Compression.Level,
			MinSize:   s3cfg.Compression.MinSize,
		},

		CongestionAlgorithm: a.config.Network.CongestionAlgorithm,

		// The one mapping in this function whose absence was itself the defect. `security.encryption`
		// was read here by nothing while `at_rest` defaulted to true, so every object was written with
		// no encryption header and the configuration said otherwise (audit finding P-7). The keys are
		// under `security` rather than `storage.s3` because the operator's question is "how is my data
		// encrypted", not "what does the S3 backend send"; the backend is where the answer is executed.
		Encryption: s3.EncryptionConfig{
			Mode:       a.config.Security.Encryption.Mode,
			KMSKeyID:   a.config.Security.Encryption.KMSKeyID,
			BucketKeys: a.config.Security.Encryption.BucketKeys,
		},
	}

	// Parallel reads. Enabled: false is expressed as a threshold of zero, which is how the backend
	// spells "disabled" — the gate there is `threshold > 0`. Mapping Enabled to a separate field
	// would give the backend two ways to say the same thing and a way for them to disagree.
	if a.config.Performance.ParallelRead.Enabled {
		cfg.ParallelReadThreshold = a.sizeOrDefault("performance.parallel_read.threshold",
			a.config.Performance.ParallelRead.Threshold, defaults.ParallelReadThreshold)
		cfg.ReadChunkSize = a.sizeOrDefault("performance.parallel_read.chunk_size",
			a.config.Performance.ParallelRead.ChunkSize, defaults.ReadChunkSize)
	}

	// Cost optimization: one field of the four, and the only one a mount can act on.
	//
	// SmallObjectsOnStandard is read on the PutObject path, so it decides the storage class of stored
	// objects on every mount. The other three — EnableAutoTiering, CostThreshold,
	// MonitorAccessPatterns — gate AnalyzeAndOptimize and the access-pattern report, and nothing on
	// the mount path calls either, so a YAML key for them would configure a feature no mount can
	// invoke. They stay reachable to callers using internal/storage/s3 as a library.
	//
	// This block was unmappable rather than merely unmapped until #203: the two structs shared no
	// field name, and `transition_to_ia: 30` had no reading that produced the []TransitionRule the
	// backend declared. Reconciling them meant deleting the fields nothing read on both sides, which
	// is a breaking change to keys that never had an effect — see S3CostOptimization.
	cfg.CostOptimization = s3.CostOptimization{
		SmallObjectsOnStandard: a.config.Storage.S3.CostOptimization.SmallObjectsOnStandard,
	}

	// TierConstraints, PricingConfig and the credential fields are deliberately left at their zero
	// values, and each for its own reason rather than by omission:
	//
	//   - TierConstraints is a policy floor, not a description of S3. Its MinObjectSize is the one
	//     value in the struct that still refuses a write, and it refuses writes S3 would accept —
	//     AWS's per-tier minimum is a billing floor, so a smaller object is stored and billed as the
	//     minimum (see TierValidator.ValidateWrite). Mapping it from a config file would mean a mount
	//     could be given a floor above zero by default, and a floor above zero rejects the zero-byte
	//     PUTs that create directories and empty files. If it is ever mapped, it has to stay opt-in
	//     and unset by default, and the deletion embargo needs the same care for the same reason.
	//
	//   - PricingConfig's Pricing API path was removed in v0.10.1; what remains is a custom rate
	//     table, which belongs in a file of its own (configs/discount-config.yaml) rather than in a
	//     mount configuration.
	//
	//   - AccessKeyID, SecretAccessKey and SessionToken have no config keys on purpose. Empty means
	//     the AWS default credential chain — environment, shared config, IMDS — which is what works
	//     on EC2 and for anyone using AWS_PROFILE. A YAML key for a long-lived secret invites it
	//     into version control.
	return cfg
}

// buildReadAheadConfig maps performance.read_ahead onto the read-ahead manager's configuration.
//
// This mapping is the whole of #176. The block was decoded, validated key by key, shipped in five
// preset files under examples/config/, and never reached the manager: NewFileSystem passed nil, so
// every mount ran the manager's own defaults no matter what the file said. The two structs also
// disagreed about what read-ahead was — twenty keys describing a strategy selector with ML hooks
// against six fields implementing a sequential detector — so the config block shrank to the
// implementation's knobs rather than the mapping growing to invent the rest.
//
// A disabled block returns a config with Enabled false rather than nil, because nil means "use the
// defaults" and the defaults have read-ahead on. Returning nil for `enabled: false` would turn
// read-ahead off in the configuration and leave it running.
func (a *Adapter) buildReadAheadConfig() *fuse.ReadAheadConfig {
	ra := a.config.Performance.ReadAhead
	defaults := fuse.DefaultReadAheadConfig()

	return &fuse.ReadAheadConfig{
		Enabled: ra.Enabled,
		WindowSize: a.sizeOrDefault("performance.read_ahead.window_size",
			ra.WindowSize, defaults.WindowSize),
		MinSequential:   ra.MinSequential,
		ConcurrentReads: ra.ConcurrentReads,
		TTL:             ra.TTL,
	}
}

// buildMountOptions maps the `fuse` config section onto the mount's options.
//
// A method rather than a struct literal at the one call site, so the mapping can be asserted without
// a mount. That is not a stylistic preference: this mapping is the seam #180 is about. Nine fields on
// fuse.MountOptions and fuse.Config named real FUSE capabilities and reached nothing, and they
// survived a release because there was no place to put a test — the literal lived inside Start,
// between initializing the write path and constructing the mount manager, and nothing short of a
// live mount could observe it.
//
// FSName, Subtype and MaxWrite are constants here rather than configuration. Their operator-facing
// keys were removed by #180 for lack of a reader; giving them one is separate work and neither has a
// use case behind it. Debug is false rather than mapped to global.log_level: go-fuse's Debug logs
// every FUSE request and reply, which is a different thing from an application log level and would
// make DEBUG unusable for anything else.
func (a *Adapter) buildMountOptions() *fuse.MountOptions {
	return &fuse.MountOptions{
		FSName:   "objectfs",
		Subtype:  "s3",
		MaxWrite: 128 * 1024,
		Debug:    false,

		// The three settings the `fuse` section carries — the first FUSE block any loader has read.
		// DirectIO and KeepCache travel further, to fuse.Config, because they are returned from every
		// open rather than set at mount time; CreatePlatformMountManager is what carries them. SyncRead
		// stops here, being a field on go-fuse's own fuse.MountOptions.
		DirectIO:  a.config.FUSE.DirectIO,
		KeepCache: a.config.FUSE.KeepCache,
		SyncRead:  a.config.FUSE.SyncRead,
	}
}

// buildRetryConfig maps network.retry onto the retryer's configuration.
//
// It starts from retry.DefaultConfig rather than building a retry.Config from the three configured
// fields, and that is load-bearing: retry.New backfills MaxAttempts, InitialDelay, MaxDelay and
// Multiplier when they are zero, but it does *not* backfill RetryableErrors. shouldRetry consults
// that list, so a config mapped field-for-field would produce a retryer that reports three attempts
// and retries almost nothing — a connection reset would fail the operation on the first try while
// the configuration said otherwise.
//
// Which errors are retryable is therefore not configurable, by design. The default list is seven
// ObjectFS error codes covering timeouts, connection failures and resource exhaustion; the values
// below control how many times and how long to wait, which is what an operator has a basis for
// choosing.
func (a *Adapter) buildRetryConfig() retry.Config {
	cfg := retry.DefaultConfig()

	if n := a.config.Network.Retry.MaxAttempts; n > 0 {
		cfg.MaxAttempts = n
	}
	if d := a.config.Network.Retry.BaseDelay; d > 0 {
		cfg.InitialDelay = d
	}
	if d := a.config.Network.Retry.MaxDelay; d > 0 {
		cfg.MaxDelay = d
	}

	return cfg
}

// sizeOrDefault parses a configured size, falling back to the backend's default for an empty value.
//
// The error arm is unreachable from a loaded configuration: Configuration.Validate parses every one
// of these strings with the same function and refuses to start on a bad one, and [New] calls
// Validate before anything here runs. It is handled rather than ignored because "unreachable" is a
// property of today's call graph, and the failure mode of assuming otherwise is the one
// internal/adapter.parseSize had — it substituted 1 GiB for anything it could not parse, silently,
// so `cache_size: 2G` and `cache_size: tpyo` both configured a 1 GiB cache with no message. Logging
// at Warn and using the default is the same fallback with the silence removed.
func (a *Adapter) sizeOrDefault(path, value string, fallback int64) int64 {
	if value == "" {
		return fallback
	}

	n, err := utils.ParseBytes(value)
	if err != nil {
		slog.Warn("configured size is not parseable; using the built-in default",
			"setting", path, "value", value, "default", fallback, "error", err)

		return fallback
	}

	return n
}

// buildWriterOptions maps the write_buffer configuration block onto the bounds [vfs.Writer] enforces.
//
// This is audit finding #205, and it is the same seam shape as buildS3Config's D12: the values were
// declared in internal/config, defaulted by NewDefault, and validated by validateSizes — and read by
// nothing. A grep for MaxMemory outside internal/config returned three hits, all of them the
// declaration, the default and the validator. So `write_buffer.max_memory: 512MB` described a ceiling
// the process did not have, on the one path that holds user data in memory before it is durable.
//
// Unbounded growth there ends with the OOM killer taking the mount, and every open file's unflushed
// dirty ranges with it — which makes this a data-loss defect rather than a resource-accounting one.
//
// A separate method rather than an inline literal in Start, so the mapping is assertable without a
// live backend. That is deliberate: the reason D12 survived 32,680 lines of tests is that its mapping
// was only reachable through a constructor nothing could call in a unit test.
func (a *Adapter) buildWriterOptions() vfs.WriterOptions {
	return vfs.WriterOptions{
		MaxMemory: a.sizeOrDefault("write_buffer.max_memory",
			a.config.WriteBuffer.MaxMemory, defaultWriteBufferMemory),
		MaxBuffers: a.config.WriteBuffer.MaxBuffers,
	}
}

// validateStorageURI validates the storage URI format.
//
// Delegates to [awsname.ValidateStorageURI], which is where the rules live as of #134 so that
// internal/config can apply the same ones to `mount.uri` without importing this package. Kept as a
// name because the tests and benchmarks in this package address it, and because the error it wraps
// says which of the two things a caller got wrong.
func validateStorageURI(uri string) error {
	return awsname.ValidateStorageURI(uri)
}
