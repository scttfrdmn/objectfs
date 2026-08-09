package adapter

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/distributed"
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

	// clusterMgr is nil unless `cluster.enabled` is set, which is the ordinary case. It is the gossip
	// layer — membership, cache invalidation, and the key announcements a cold node warms from — and
	// **not** the Raft engine: see [distributed.ClusterConfig.EnableConsensus], which this deliberately
	// leaves off.
	//
	// This is the field #139 was filed for. Every part of internal/distributed built and tested and
	// reached no mount, because nothing here constructed one: `cluster.enabled: true` selected a Redis
	// cache through cache.NewFromConfig and otherwise did nothing at all, so a two-node deployment got
	// no invalidation and no warming while its configuration said it was clustered.
	clusterMgr *distributed.ClusterManager

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

	a.exportPredictiveStats()
	a.exportAccelerationStats()
	a.exportCostStats()

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

	// 5. Start cluster coordination, when configured.
	//
	// Before the mount, so that the coordinator the filesystem is handed is already running: a mount
	// serves reads the moment Mount returns, and a coordinator started afterwards would have a window
	// in which invalidations were silently dropped.
	//
	//nolint:contextcheck // a.mountCtx for the same reason as the write path and read-ahead above: the
	// gossip receive loop and the membership ticker live as long as the mount, not as long as Start.
	if err := a.startCluster(a.mountCtx); err != nil {
		return err
	}

	// 6. Initialize platform-specific FUSE filesystem
	mountConfig := &fuse.MountConfig{
		MountPoint: a.mountPoint,
		Options:    a.buildMountOptions(),
		ReadAhead:  a.buildReadAheadConfig(),
		// Nil when clustering is off, which is what every single-node path branches on. See
		// FileSystem.coordinator.
		Coordinator: a.clusterCoordinator(),
	}

	//nolint:contextcheck // a.mountCtx, not ctx, for the same reason as the write path above and with
	// the same consequence if it were Start's: the read-ahead manager's prefetch workers live as long
	// as the mount does, so inheriting a context canceled when Start returns would leave four workers
	// running that could never fetch anything. mountCtx is a WithCancel over WithoutCancel(ctx), so it
	// does carry the caller's values — what it deliberately does not inherit is the caller's
	// cancellation, which is the whole point. Stop cancels it, and that is now what stops them.
	a.mountMgr = fuse.CreatePlatformMountManager(a.mountCtx, a.backend, a.cache, a.writeBuffer, a.metrics, mountConfig)

	// 7. Initialize and start health monitor
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

	// 8. Mount filesystem
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

	// 3. Stop cluster coordination.
	//
	// After the flush and before the backend closes, which is the only window that is correct in both
	// directions. The gossip receive loop executes operations on behalf of peers against a.backend and
	// evicts from a.cache, so it must stop before either of those is closed — otherwise a peer's
	// message arriving during teardown reaches a closed backend or a closed cache. And it must stop
	// after the flush rather than before the unmount, because the flush is the last chance for this
	// node's own pending writes to become durable and nothing about a running cluster impedes it.
	if a.clusterMgr != nil {
		if err := a.clusterMgr.Stop(); err != nil {
			slog.Error("error stopping cluster manager", "error", err)
			lastErr = err
		}
	}

	// 4. Cancel the mount context, releasing anything still blocked on a backend call. Only after the
	// flush above: canceling first would abort the flush it exists to permit.
	if a.cancelMount != nil {
		a.cancelMount()
	}

	// 5. Close backend connections
	if a.backend != nil {
		if err := a.backend.Close(); err != nil {
			slog.Error("error closing backend", "error", err)
			lastErr = err
		}
	}

	// 6. Stop health monitor
	if a.monitor != nil {
		if err := a.monitor.Stop(); err != nil {
			slog.Error("error stopping health monitor", "error", err)
			lastErr = err
		}
	}

	// 7. Clear the cache, then release the goroutines behind it.
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

	// 8. Stop metrics collection
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

// startCluster brings up gossip-based cluster coordination when `cluster.enabled` is set, and is a
// no-op otherwise.
//
// A failure here fails the mount rather than degrading to a single node, and that asymmetry is
// deliberate. Coordination is a correctness capability, not a performance one: a node that believes
// it is clustered and is not will serve cached bytes that a peer has already overwritten, with
// nothing in its logs to say why. The same reasoning is why the Redis cache fails the mount rather
// than falling back — see cache.NewFromConfig — and it is the project thesis's rule for the kind of
// capability this is.
//
// The commonest failure by far is the missing cluster secret, which [distributed.LoadClusterSecret]
// reports naming both places it can come from.
func (a *Adapter) startCluster(ctx context.Context) error {
	if !a.config.Cluster.Enabled {
		return nil
	}

	clusterMgr, err := distributed.NewClusterManager(a.buildClusterConfig())
	if err != nil {
		return fmt.Errorf("failed to initialize cluster manager: %w", err)
	}

	// Both injections before Start, so the gossip receive loop cannot dispatch a peer's message at a
	// nil backend or a nil cache. Both setters are safe to call afterwards as well — that is what
	// [distributed.ClusterManager.SetBackend]'s locking is for — but "before Start" is the ordering
	// that needs no argument at all.
	clusterMgr.SetBackend(a.backend)
	clusterMgr.SetCache(a.cache)

	if err := clusterMgr.Start(ctx); err != nil {
		return fmt.Errorf("failed to start cluster manager: %w", err)
	}

	a.clusterMgr = clusterMgr
	slog.Info("cluster coordination started",
		"node_id", clusterMgr.GetNodeID(),
		"listen_addr", a.config.Cluster.ListenAddr,
		"seed_nodes", len(a.config.Cluster.SeedNodes))

	return nil
}

// clusterCoordinator returns the coordinator the filesystem should use, or nil when clustering is off.
//
// The nil is the point, and it is why this is a method rather than a call to GetCoordinator at the use
// site. Two things go wrong without the guard. GetCoordinator reads cm.coordinator, so calling it on a
// nil manager panics — that is what a mount with clustering disabled would do, on every start. And
// moving the check inside GetCoordinator would not be enough either: it returns
// `&coordinatorWrapper{...}`, a non-nil [types.DistributedCoordinator] whatever it wraps, so a mount
// handed one would take the coordinated branch at every `if fs.coordinator != nil` in internal/fuse
// and dereference the nothing inside. A nil interface is the only value those guards read correctly.
func (a *Adapter) clusterCoordinator() types.DistributedCoordinator {
	if a.clusterMgr == nil {
		return nil
	}

	return a.clusterMgr.GetCoordinator()
}

// buildClusterConfig maps the operator-facing cluster: block onto the distributed package's own
// configuration.
//
// The two ClusterConfig types are disjoint and this is the only conversion between them, which is
// most of why #139 existed: internal/config.ClusterConfig has seven fields an operator can set,
// internal/distributed.ClusterConfig has nineteen, and there was no function anywhere that turned one
// into the other. Fields left unset here are filled by applyConfigDefaults — the timeouts, the gossip
// triple, and the concurrency and retry settings are all tuning for a subsystem whose defaults are
// measured (see defaultMaxGossipPacket), so exposing them as YAML before anyone needs to change one
// would be eight more keys that mostly should not be touched.
//
// AnnouncementTTL (#140) is the newest of those and is left unset for the same reason, with one
// difference worth recording: its default is applied at the point of use in
// [distributed.Coordinator.announcementTTL] rather than in applyConfigDefaults, because a zero there is
// not a slower cluster but a silently inert one — every announcement would expire on arrival.
//
// EnableConsensus is deliberately not set and has no key in the config schema. See
// [distributed.ClusterConfig.EnableConsensus]: coordination in ObjectFS is compare-and-swap against
// S3, so a mount has no use for a leader, and putting an election on the path of a filesystem read
// would make a cluster that cannot hold a quorum degrade a mount that never needed one.
func (a *Adapter) buildClusterConfig() *distributed.ClusterConfig {
	c := a.config.Cluster

	return &distributed.ClusterConfig{
		NodeID:        c.NodeID,
		ListenAddr:    c.ListenAddr,
		AdvertiseAddr: c.AdvertiseAddr,
		SeedNodes:     c.SeedNodes,

		// The path only, never the secret — see [config.ClusterConfig.SecretFile]. May be empty, in
		// which case OBJECTFS_CLUSTER_SECRET is the only source and LoadClusterSecret fails naming
		// both if it is unset too.
		SecretFile: c.SecretFile,

		ReplicationFactor: c.ReplicationFactor,
	}
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

// exportPredictiveStats publishes the predictive cache's statistics to the metrics surface on every
// collector tick, and does nothing when there is no predictive layer to ask.
//
// This is the caller [cache.MultiLevelCache.GetPredictiveCache] was missing. The statistics were computed
// on every read and thrown away at unmount, because nothing above the cache could reach them: types.Cache
// is six methods about bytes (#223). An accessor with no caller would have been half the fix — a second
// path nothing exercises — so the accessor and its consumer land together, and the consumer is the
// metrics surface because that is where an operator can actually see the numbers.
//
// Registered as a periodic callback rather than pushed at the moment of the operation: these are totals
// the cache holds, so the only way to publish them is to ask on a schedule. The callback re-reads the
// cache each tick rather than closing over a PredictiveStats value, since a snapshot taken here would be
// the mount's opening zeros forever.
//
// The type assertion is the honest form of "when there is a predictive layer". NewFromConfig returns the
// Redis-backed distributed cache when cluster.redis is on, which has no predictive layer and no way to
// grow one; and the multi-level cache only wraps L1 when prefetch is enabled. Both are ordinary
// configurations, so neither is an error — the metric family is simply absent, which is distinguishable
// from present-and-zero by a scrape.
//
// Note what these numbers will and will not show on a mount today: prediction and eviction counters
// populate, and the prefetch counters read zero, because initializeLevels passes no Backend to the
// predictive config, so its workers dequeue jobs and fetch nothing. That is honest signal rather than a
// hidden failure — prefetch_requests staying at 0 while predictions_total climbs is exactly what "the
// prefetcher has no backend" looks like — and it is the argument for exporting these at all: the gap was
// invisible for as long as nothing could read them.
func (a *Adapter) exportPredictiveStats() {
	if a.metrics == nil {
		return
	}

	mlc, ok := a.cache.(*cache.MultiLevelCache)
	if !ok {
		return
	}

	publish := func() {
		stats, ok := mlc.PredictiveStats()
		if !ok {
			return
		}

		// Keys become the `statistic` label, so they are a contract two SDKs read through
		// sdks/testdata/metrics-scrape.txt. They mirror the struct's JSON tags rather than inventing a
		// second vocabulary for the same numbers.
		a.metrics.UpdatePredictiveCache(map[string]float64{
			"predictions_total":     float64(stats.PredictionsTotal),
			"predictions_correct":   float64(stats.PredictionsCorrect),
			"prediction_accuracy":   stats.PredictionAccuracy,
			"avg_confidence":        stats.AvgConfidence,
			"prefetch_requests":     float64(stats.PrefetchRequests),
			"prefetch_hits":         float64(stats.PrefetchHits),
			"prefetch_bytes":        float64(stats.PrefetchBytes),
			"prefetch_waste":        float64(stats.PrefetchWaste),
			"prefetch_efficiency":   stats.PrefetchEfficiency,
			"evictions_total":       float64(stats.EvictionsTotal),
			"evictions_intelligent": float64(stats.EvictionsIntelligent),
		})
	}

	// Once now, and on every tick after. Without the first call the family is absent from a scrape until
	// the update interval elapses — thirty seconds by default — and an absent family and a disabled
	// predictive layer look the same to whoever is looking.
	publish()
	a.metrics.OnPeriodicUpdate(publish)
}

// exportAccelerationStats publishes the S3 Transfer Acceleration state to the metrics surface on every
// collector tick.
//
// This is the "surface the state" half of #204, and the state it surfaces is one an operator could not
// previously obtain by any means. Acceleration fell back on the first acceleration error and stayed
// fallen back for the life of the mount; s3.Backend tracked that accurately, in
// BackendMetrics.AccelerationEnabled — a field whose only writer was NewBackend, passing the config flag
// — behind s3.Backend.GetMetrics, which had no caller outside its own package. So a mount serving every
// byte over the standard endpoint reported acceleration enabled, and the throughput loss showed up
// nowhere but in a throughput graph nobody had reason to compare against.
//
// Registered unconditionally, including when acceleration is not configured, and that is deliberate.
// `configured 0` is a different fact from an absent metric family — the first says the mount was asked
// not to accelerate, the second says this build does not report it — and the question an operator asks
// when investigating slow reads is exactly which of those they are looking at.
//
// A periodic callback rather than a push at the moment of the fallback, for two reasons. The gate's
// transitions happen inside the s3 package under a breaker lock, so a push would mean handing that lock
// a metrics dependency; and `active` is a gauge whose value is the state of another subsystem, which is
// the shape OnPeriodicUpdate exists for. Reading it on a tick also advances the gate's open→half-open
// transition, which is harmless: it recovers a few seconds earlier than the next request would.
func (a *Adapter) exportAccelerationStats() {
	if a.metrics == nil || a.backend == nil {
		return
	}

	publish := func() {
		stats := a.backend.AccelerationStats()

		// Keys become the `statistic` label, so they are the contract two SDKs read through
		// sdks/testdata/metrics-scrape.txt.
		//
		// `configured` and `active` are both here because they answer different questions and the
		// difference between them *is* the fallback: configured 1 with active 0 means this mount was
		// asked to accelerate and is not. Neither one alone can say that.
		a.metrics.UpdateS3Acceleration(map[string]float64{
			"configured":           boolGauge(stats.Configured),
			"active":               boolGauge(stats.Active),
			"requests":             float64(stats.Requests),
			"bytes":                float64(stats.Bytes),
			"fallbacks":            float64(stats.Fallbacks),
			"avg_latency_seconds":  stats.AvgLatency.Seconds(),
			"retry_period_seconds": stats.RetryPeriod.Seconds(),
		})
	}

	publish()
	a.metrics.OnPeriodicUpdate(publish)
}

// exportCostStats publishes what this mount is spending at AWS to the metrics surface on every tick.
//
// #226: the cost machinery was accurate and unobservable. The rates are checked against AWS's published
// price list, the arithmetic handles billing minimums and the archive classes' per-object overhead, and
// no path led to a user — CostOptimizer's report was gated behind a config key no mount could act on,
// internal/cost had no importer, and metrics.RecordCost had no caller. This is the path.
//
// Registered unconditionally, on the same reasoning as the acceleration family: `write_requests 0` is a
// different fact from an absent family, and an operator asking "is this mount expensive" needs to know
// which of the two they are looking at.
//
// The request counts come from a smithy middleware on every S3 client rather than from the backend's
// operation wrappers, because the two disagree by more than a little: one PutObject above the multipart
// threshold is one wrapper call and hundreds of billable requests. See s3.CostStats.
func (a *Adapter) exportCostStats() {
	if a.metrics == nil || a.backend == nil {
		return
	}

	publish := func() {
		stats := a.backend.CostStats()

		// Keys become the `statistic` label, so they are the contract two SDKs read through
		// sdks/testdata/metrics-scrape.txt.
		//
		// The rates are published alongside the costs they produced so a dashboard can show the
		// arithmetic and not only its result. #209 was a per-1,000 price stored as if it were
		// per-request — a tenfold error that lived in code no scrape could contradict; a mount that
		// publishes the rate it used makes that class of mistake visible in a graph.
		a.metrics.UpdateS3Cost(stats.Region, stats.Tier, map[string]float64{
			"write_requests":                 float64(stats.WriteRequests),
			"list_requests":                  float64(stats.ListRequests),
			"read_requests":                  float64(stats.ReadRequests),
			"free_requests":                  float64(stats.FreeRequests),
			"bytes_retrieved":                float64(stats.BytesRetrieved),
			"bytes_stored":                   float64(stats.StoredBytes),
			"request_cost_dollars":           stats.RequestCost,
			"retrieval_cost_dollars":         stats.RetrievalCost,
			"storage_cost_dollars_per_month": stats.StorageCostPerMonth,
			"rate_per_write_request":         stats.RatePerWriteRequest,
			"rate_per_list_request":          stats.RatePerListRequest,
			"rate_per_read_request":          stats.RatePerReadRequest,
			"rate_per_gb_retrieved":          stats.RatePerGBRetrieved,
			"rate_per_gb_month":              stats.RatePerGBMonth,
		})
	}

	publish()
	a.metrics.OnPeriodicUpdate(publish)
}

// boolGauge renders a boolean as the 1/0 Prometheus convention for state.
//
// Named rather than inlined as a conditional expression per field, because the two callers above are the
// two halves of one distinction and writing them the same way is what keeps them comparable.
func boolGauge(b bool) float64 {
	if b {
		return 1
	}

	return 0
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

		// Zero passes through to newAccelerationGate's default rather than being substituted here, so
		// there is one place that decides the period.
		AccelerationRetry: s3cfg.AccelerationRetry,

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
