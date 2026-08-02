package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/objectfs/objectfs/internal/cache"
	"github.com/objectfs/objectfs/internal/config"
	"github.com/objectfs/objectfs/internal/fuse"
	"github.com/objectfs/objectfs/internal/health"
	"github.com/objectfs/objectfs/internal/metrics"
	"github.com/objectfs/objectfs/internal/storage/s3"
	"github.com/objectfs/objectfs/internal/vfs"
	"github.com/objectfs/objectfs/pkg/retry"
	"github.com/objectfs/objectfs/pkg/utils"
)

// Fallback sizes for the two cache capacities, used only if a configured value fails to parse here
// after having passed Configuration.Validate — see sizeOrDefault. They match internal/config's
// NewDefault so that the fallback is the documented default rather than a third number.
const (
	defaultCacheSize           = 2 << 30  // 2 GiB, matching NewDefault's "2GB"
	defaultPersistentCacheSize = 10 << 30 // 10 GiB, matching NewDefault's "10GB"
)

// Adapter represents the main ObjectFS adapter
type Adapter struct {
	storageURI string
	mountPoint string
	config     *config.Configuration

	// Core components
	backend     *s3.Backend
	cache       *cache.MultiLevelCache
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
	// Validate storage URI
	if err := validateStorageURI(storageURI); err != nil {
		return nil, fmt.Errorf("invalid storage URI: %w", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	// Parse S3 URI to extract bucket name
	parsed, err := url.Parse(storageURI)
	if err != nil {
		return nil, fmt.Errorf("failed to parse storage URI: %w", err)
	}

	bucketName := strings.TrimPrefix(parsed.Host, "")
	if bucketName == "" {
		return nil, fmt.Errorf("bucket name cannot be empty")
	}

	adapter := &Adapter{
		storageURI: storageURI,
		mountPoint: mountPoint,
		config:     cfg,
		bucketName: bucketName,
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

	// 1. Initialize metrics collector
	var err error
	a.metrics, err = metrics.NewCollector(&metrics.Config{
		Enabled: a.config.Monitoring.Metrics.Enabled,
		Port:    a.config.Global.MetricsPort,
		Labels:  a.config.Monitoring.Metrics.CustomLabels,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize metrics collector: %w", err)
	}

	// 2. Initialize S3 backend
	a.s3Config = a.buildS3Config()
	slog.Info("S3 backend", "region", a.s3Config.Region)

	a.backend, err = s3.NewBackend(ctx, a.bucketName, a.s3Config)
	if err != nil {
		return fmt.Errorf("failed to initialize S3 backend: %w", err)
	}

	// 3. Initialize cache system
	cacheConfig := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       a.sizeOrDefault("performance.cache_size", a.config.Performance.CacheSize, defaultCacheSize),
			MaxEntries: a.config.Cache.MaxEntries,
			TTL:        a.config.Cache.TTL,
			Prefetch:   true,
		},
		L2Config: &cache.L2Config{
			Enabled: a.config.Cache.PersistentCache.Enabled,
			Size: a.sizeOrDefault("cache.persistent_cache.max_size",
				a.config.Cache.PersistentCache.MaxSize, defaultPersistentCacheSize),
			Directory:   a.config.Cache.PersistentCache.Directory,
			TTL:         a.config.Cache.TTL,
			Compression: true,
		},
		Policy: a.config.Cache.EvictionPolicy,
	}

	a.cache, err = cache.NewMultiLevelCache(cacheConfig)
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

	a.writeBuffer, err = vfs.NewWriter(a.mountCtx, a.backend)
	if err != nil {
		return fmt.Errorf("failed to initialize write path: %w", err)
	}

	// 5. Initialize platform-specific FUSE filesystem
	mountConfig := &fuse.MountConfig{
		MountPoint: a.mountPoint,
		Options: &fuse.MountOptions{
			FSName:   "objectfs",
			Subtype:  "s3",
			MaxRead:  128 * 1024,
			MaxWrite: 128 * 1024,
			Debug:    false,
		},
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
				HTTPEnabled:   a.config.Global.HealthPort > 0,
				HTTPPort:      a.config.Global.HealthPort,
				HTTPPath:      "/health",
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
		slog.Info("health monitor started", "port", a.config.Global.HealthPort)
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
	// The Close is what retires the prefetch workers. Prefetch is enabled unconditionally above, so
	// every mount wraps L1 in a predictive cache with four workers and a statistics ticker, and until
	// this call existed nothing ever stopped them: a process that mounted and unmounted repeatedly
	// accumulated a set per mount, each holding a reference to the cache it was built over.
	if a.cache != nil {
		a.cache.Clear()

		if err := a.cache.Close(); err != nil {
			slog.Error("error closing cache", "error", err)
			lastErr = err
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

		EnableCargoShipOptimization: s3cfg.UseCargoShip,

		MultipartThreshold: a.sizeOrDefault("storage.s3.multipart.threshold",
			s3cfg.Multipart.Threshold, defaults.MultipartThreshold),
		MultipartChunkSize: a.sizeOrDefault("storage.s3.multipart.chunk_size",
			s3cfg.Multipart.ChunkSize, defaults.MultipartChunkSize),
		MultipartConcurrency: s3cfg.Multipart.Concurrency,

		StorageTier: s3cfg.StorageTier,

		Compression: s3.CompressionConfig{
			Enabled:   a.config.WriteBuffer.Compression.Enabled,
			Algorithm: a.config.WriteBuffer.Compression.Algorithm,
			Level:     a.config.WriteBuffer.Compression.Level,
			MinSize:   a.config.WriteBuffer.Compression.MinSize,
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

	// TierConstraints, CostOptimization, PricingConfig and the credential fields are deliberately
	// left at their zero values, and each for its own reason rather than by omission:
	//
	//   - TierConstraints overrides a tier's built-in minimum size and deletion embargo. Those come
	//     from StorageTiers, which is derived from what AWS actually enforces; a config file that
	//     could raise or lower them has no correct value to hold, and lowering one produces writes
	//     S3 rejects.
	//
	//   - CostOptimization is not mappable. internal/config.S3CostOptimization and
	//     s3.CostOptimization are disjoint types — {Enabled, TieringEnabled, LifecycleEnabled,
	//     TransitionToIA, TransitionToGlacier} against {EnableAutoTiering, TransitionRules,
	//     LifecycleManagement, IntelligentTiering, CostThreshold, MonitorAccessPatterns} — with no
	//     field in common. Nothing reads either one, and the backend's automatic-tiering machinery
	//     (AnalyzeAndOptimize, applyOptimization) has no caller either, so mapping the two would
	//     wire a config block to an unreachable feature. examples/config.yaml says so at the block.
	//     MonitorAccessPatterns is additionally held back because it writes an unsynchronized map
	//     from the read path and silently rewrites the storage class of objects under 128 KiB.
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

// validateStorageURI validates the storage URI format
func validateStorageURI(uri string) error {
	parsed, err := url.Parse(uri)
	if err != nil {
		return fmt.Errorf("failed to parse URI: %w", err)
	}

	switch parsed.Scheme {
	case "s3":
		if parsed.Host == "" {
			return fmt.Errorf("S3 URI must include bucket name")
		}
	default:
		return fmt.Errorf("unsupported storage scheme: %s (only s3:// supported)", parsed.Scheme)
	}

	return nil
}
