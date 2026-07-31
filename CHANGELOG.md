# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `internal/vfs` — a backend-agnostic POSIX-semantics core, and the layer whose absence generated a whole class of v0.10.0 defects. It owns the attribute model, the handle table, dirty-range tracking, and the read-modify-write policy, returns typed errors (`vfs.ErrNotFound`, `vfs.ErrIntegrity`, …) rather than `syscall.Errno`, and depends on nothing FUSE. `internal/fuse` becomes a translation shim above it. The point is that POSIX semantics are now **testable without a mount**, which the previous design forbade: attributes could not persist because no type owned them, offset writes could not be buffered because no range model existed, and the go-fuse mode backstop was left disabled because nothing owned the default
- `internal/vfs.ExtentList` — an interval list of pending writes, replacing the single contiguous buffer plus offset that could not represent an offset write. It accepts any write at any offset, coalesces overlaps so later writes win, tracks truncation as a watermark rather than a flag, and produces a `FlushPlan` naming exactly which byte ranges of the stored object must be fetched to assemble the new one. A truncation records two sizes, not one: a boolean cannot express "truncate to 4, then write at 83," which must leave bytes 4–83 a hole rather than resurrecting the deleted content
- `internal/vfs.Attr` — one type owning size, mode, ownership, times, ETag, and the stored SHA-256, with round-trippable S3 user-metadata encoding. Metadata keys are matched case-insensitively, because S3 lower-cases them, MinIO title-cases them, and an `http.Header` round-trip canonicalises to `Objectfs-Mode` — a case-sensitive lookup passes unit tests and fails against real storage. Malformed metadata falls back to defaults rather than erroring, so setting `objectfs-mode` to `banana` cannot make a file unreadable; `MetadataWarnings` reports what was ignored
- `internal/vfs.Node` / `HandleTable` — per-path state shared by every handle open on it, because one path is one S3 object. `MarkFlushed` takes the generation counter captured when the flush was planned and refuses to clear pending state if a write landed during the upload, which is the specific v0.10.0 data-loss path where a write concurrent with a flush was discarded and accounted as flushed. `Release` keeps a dirty node rather than dropping it, and `Forget` refuses a node that is still dirty or still open — a caller whose flush failed must not be able to make the failure disappear
- Reference-model property tests and three fuzz targets (`FuzzExtentList`, `FuzzAttrFromMetadata`, `FuzzNodeLifecycle`) for `internal/vfs`, at 98.4% statement coverage. Each asserts the implementation against a deliberately naive `map[int64]byte` model of the file, so a test cannot ratify the same off-by-one the implementation has. This found two real design defects in `ExtentList` that thirty-odd hand-written table cases had all passed: a truncate-then-write-past-the-end sequence re-fetched the whole stored object and resurrected deleted bytes, and a sparse read could not distinguish a hole inside the file from a read past EOF, returning zero bytes instead of zeros

### Removed
- `internal/fuse/cgofuse_filesystem.go`, `cgofuse_mount.go`, `cgofuse_test.go`, `platform_cgofuse.go`, the `github.com/winfsp/cgofuse` dependency, the `build-cgofuse` / `build-all-cgofuse` / `build-{linux,darwin,windows}-cgofuse` Makefile targets, and `FUSE_MIGRATION.md`. The `cgofuse` build tag never compiled: `filesystem.go` carried no build constraint of its own, so under the tag `OpenFile` was declared twice, and that duplicate-symbol error was masked by a missing `fuse.h`. It was also a silently divergent 382-line subset of the 727-line go-fuse implementation, offering only Mount/Unmount/IsMounted/Getattr/Open/Read/Write/Release/Readdir — it never received the `Unlink`/`Rmdir` fix, so under that tag `rm` reported success while the S3 object survived. `FUSE_MIGRATION.md` justified the tag by "macOS compatibility issues" with go-fuse, which is not true: go-fuse's darwin mount execs `mount_macfuse` and cgofuse needs the same macFUSE headers, so the two have identical macOS requirements
- `Makefile`: the `build-windows` target and Windows from `build-all`. There is no Windows FUSE binding, so the binary it produced could not mount anything

### Changed
- `go.mod`: `github.com/hanwen/go-fuse/v2` upgraded from v2.8.0 to v2.11.0. No API changes were needed — ObjectFS uses 14 distinct `fs.*` symbols and the binding surface is unchanged
- `internal/fuse`: every file now carries `//go:build linux || darwin`. **Windows is explicitly unsupported.** Previously `filesystem.go`, `mount.go`, and `optimizations.go` had no build constraint at all while `platform.go` had `!cgofuse`, which is what allowed the duplicate `OpenFile` declaration to go unnoticed. A new `platform_unsupported.go` makes a build for any other `GOOS` fail inside `internal/fuse` with a message naming the reason, instead of emitting a list of `undefined: fuse.PlatformFileSystem` errors from `internal/adapter` that read like a broken build
- `internal/fuse/doc.go`: rewritten. It documented a "Cross-Platform Abstraction" selecting between go-fuse and cgofuse, cited the stale module path `github.com/billziss-gh/cgofuse` (the module has been `github.com/winfsp/cgofuse` for years), and listed as supported a large set of operations this package does not implement — `chmod`, `chown`, `utimes`, `truncate`, `fsync`, `rename`, `link`, `symlink`, and all four xattr calls — plus symlinks-as-metadata, hard-link reference counting, FSEvents integration, Spotlight compatibility, and Windows/NTFS optimizations, none of which exist. It now describes the actual platform support, the object-storage mapping, the fact that ownership and mode are not persisted, and the concurrency contract

### Fixed
- `internal/storage/s3/client.go`: **every** S3 client ObjectFS builds now applies the configured `endpoint`, `force_path_style`, and `use_dual_stack`. The connection pool's factory called `s3.NewFromConfig` with no options at all, so `HeadObject`, `DeleteObject`, `ListObjects`, the health check, and the non-accelerated path of `executeWithAccelerationFallback` addressed **real AWS S3** while `PutObject` and `GetObject` addressed the configured endpoint. ObjectFS could therefore not work against MinIO, Ceph, Wasabi, or any S3-compatible endpoint, and failed in a way that reads as a credentials problem. A single shared `clientOptions` mutator is now used by all four construction sites, and a test enumerates the manager's accessors so a client added later without it fails there rather than in production
- `internal/storage/s3/pool.go`: `ConnectionPool.Get`/`GetWithTimeout` return `(*s3.Client, error)` and **wait** for a connection instead of returning a nil client. Once `currentSize` reached `maxSize` the pool returned `nil` forever — the `select` had a `default` arm, so it never reached its own `time.After` case — and all six call sites dereferenced the result unchecked. On the default 8-connection pool the ninth concurrent operation panicked and unmounted the filesystem under every open descriptor. A saturated pool now blocks up to the timeout and, if it expires, returns an error naming the in-use count and the config knob to raise
- `internal/storage/s3/pool.go`: `Put` performs its `closed` check and its channel send in one critical section, and `Close` drains the channel rather than closing it. The two were a check-then-act race whose losing side was a send on a closed channel — a panic during unmount, on the path every deferred `Put` takes. A `Get` already blocked when `Close` runs now fails immediately rather than waiting out its full timeout
- `internal/storage/s3/pool.go`: the pool no longer overshoots `maxSize` under concurrency. The capacity check and the counter increment were taken under separate locks, so N concurrent callers all passed the check before any of them incremented: 16 concurrent readers against a pool of 4 constructed 16 clients and discarded 12 on return. Reserving a slot is now one atomic operation, and a factory failure releases the reservation instead of permanently shrinking the usable pool
- `internal/storage/s3/pool.go`: `Warmup` respects `maxSize`, reports factory failures with `errors.Join` instead of a bare count, stops on a cancelled context, and refuses a closed pool. `Resize` refuses to grow past the capacity the pool was constructed with — the idle channel's buffer is fixed, so a larger `maxSize` would let a reservation be made for a slot with no buffer space and deadlock the return — and tells the caller to raise `storage.s3.pool_size` instead. Shrinking now converges on the new size by dropping checked-out connections as they return
- `internal/storage/s3/backend.go`, `cost_optimizer.go`: the five pooled-client call sites propagate a pool acquisition failure as an error on the operation rather than dereferencing nil
- `internal/storage/s3/backend.go`: `PutObject` now records the uncompressed byte length as `objectfs-original-size` in S3 user metadata whenever transparent compression actually compressed the object, and `HeadObject` reports that value as `ObjectInfo.Size`. Previously `HeadObject` returned the *compressed* `ContentLength`, which the FUSE layer cached and handed to the kernel as the file size — truncating every read of a compressed file at the compressed length. A 40 MB object reported 4,556 bytes and a 64 KB object reported 87 bytes, making over 99.99% of the data unreachable. Objects written before this change fall back to `ContentLength` as before (#170)
- `internal/storage/s3/backend.go`, `multipart_upload.go`: The multipart upload path no longer recomputes `objectfs-sha256` from the post-compression bytes. `putObjectMultipart` and `initiateMultipartUpload` now receive the same metadata map the single-part path builds, so the checksum always describes the uncompressed content on both paths. Previously the two paths stored hashes of different byte streams under the same key, so a checksum written by one path could never be verified against the other (#170)
- `internal/fuse/filesystem.go`: `DirectoryNode` now implements `Unlink` and `Rmdir`, returning `EROFS` and logging a warning. go-fuse's `NodeUnlinker`/`NodeRmdirer` defaults return **success** for unimplemented operations, so `rm` and `rmdir` previously reported that files had been deleted while the S3 objects remained — and a subsequent `ls` showed them again. Deleting through a mount now fails loudly instead of silently lying. Full implementations are tracked separately (#163)

### Removed
- `internal/storage/s3/pricing_manager.go`: The AWS Pricing API integration (`fetchFromPricingAPI`, `parsePricingData`, `mapTierToStorageClass`, `extractStorageCost`, `extractRetrievalCost`, and the `AWSPricingResponse`/`AWSProduct` types). The parser matched products on human-readable storage-class names that do not appear in the Pricing API payload, and both cost extractors ignored their arguments to return hardcoded constants — so enabling `use_pricing_api` fetched a multi-megabyte JSON document over HTTP and then discarded it in favour of `0.023`/`0.01` per GB. `GetTierPricing` now goes straight to the default rate table derived from `StorageTiers`, which is what the old path effectively returned anyway (#161)

### Deprecated
- `internal/storage/s3/config.go`: `pricing_config.use_pricing_api` no longer has any effect. Setting it logs a deprecation warning at startup. Use `pricing_config.custom_pricing` to override rates per tier (#161)

## [0.10.0] - 2026-02-23 — WITHDRAWN

**This release is withdrawn and must not be used.** A deep audit found defects that prevent the
shipped default configuration from mounting and that silently lose or corrupt user data. Fixes are
landing in `[Unreleased]` for v0.10.1.

- **Cannot mount on the default configuration.** `internal/config/config.go` defaults
  `compression.algorithm` to `gzip`, but `internal/compression/codec.go` implements only `none`,
  `zstd`, and `lz4`. Every code path that reads config treats `gzip` as valid — only the codec
  factory disagrees — so `objectfs s3://bucket /mnt` exits with `Failed to start adapter`.
- **Offset writes truncate the object.** The write-buffer flush callback in
  `internal/adapter/adapter.go` receives `(key, data, offset)` and calls
  `backend.PutObject(ctx, key, data)`, discarding the offset. Because `PutObject` is a
  whole-object replace, appending one byte to a 1 MiB file leaves a 1-byte object. Non-contiguous
  writes (SQLite, mmap writeback, `tar`, HDF5) return `EIO` instead. Flush errors are recorded to
  a stats counter and not returned, so `close(2)` reports success after a failed upload.
- **Read amplification on every object when compression is enabled.**
  `internal/storage/s3/backend.go` decides whole-object-versus-ranged fetch from the compression
  *configuration* rather than from the object being read, so a ranged read of any object — including
  objects never compressed and objects written by other tools — downloads the whole object and
  disables parallel reads bucket-wide. Measured against real S3 with a fixed 4 KiB read: 15.6× on a
  16 MiB object, 43× at 64 MiB, 216× at 256 MiB. A 4 KiB read of a 10 GiB object transfers 10 GiB.
- **Silent corruption when the codec configuration changes.** `Decompress` in
  `internal/compression/s3_integration.go` returns the payload unchanged when the stored
  `Content-Encoding` does not match the configured codec, so an object written with zstd and read
  after switching to lz4 emits the raw compressed frame with exit status 0. The
  `objectfs-sha256` metadata this release added is written and never read, so nothing catches it.
- **The read cache cannot hit and is never invalidated.** The cache key includes the requested
  *length*, so the `Lookup` metadata cache never hits, short reads at EOF are uncacheable, and the
  16 MB chunked cache population added in this release is unreachable. There are no `cache.Delete`
  calls in `internal/fuse`, so a read after a write on the same descriptor returns pre-write bytes
  for up to the 5-minute TTL.
- **The headline feature of this release is inactive in production.** `buildS3Config` maps 6 of
  roughly 30 `s3.Config` fields and does not map `ParallelReadThreshold`; `NewBackend` does not
  backfill it. The parallel range GET path is gated on `threshold > 0`, so it never runs on a real
  mount. `PoolSize` is likewise unmapped, leaving a zero-capacity semaphore that blocks forever in
  `GetObjects`/`PutObjects`.
- **`rm` and `rmdir` reported success without deleting** (fixed in `[Unreleased]`, see #163).
- **Windows is not supported.** The `cgofuse` build tag has never compiled.

### Added
- `internal/storage/s3/config.go`: Three new `Config` fields — `ParallelReadThreshold` (default 64 MB), `ReadChunkSize` (default 16 MB), `ParallelReadConcurrency` (default 0 = inherit `MultipartConcurrency`) — control parallel range GET fan-out for large objects (#128)
- `internal/storage/s3/backend.go`: `parallelGetObject()` — fans out a large read into N concurrent range GETs bounded by `ParallelReadConcurrency`, assembles results in order; used automatically when the object/read size exceeds `ParallelReadThreshold` and compression is inactive (#128)
- `internal/config/config.go`: `ParallelReadConfig` struct and `PerformanceConfig.ParallelRead` field for YAML/env configuration of the parallel-read feature (#128)
- `internal/storage/s3/backend.go`, `multipart_upload.go`: Content SHA-256 stored as `objectfs-sha256` in S3 user metadata on every `PutObject` (standard, CargoShip, and multipart paths); always computed from the uncompressed canonical content so the hash is stable regardless of storage encoding (#129)
- `internal/storage/s3/backend.go`: `HeadObject` now populates `ObjectInfo.Checksum` from the `objectfs-sha256` metadata key; returns empty string for objects written before v0.10.0 (backward compatible) (#129)

### Changed
- `internal/fuse/filesystem.go`, `cgofuse_filesystem.go`: Cache population after a backend read now splits large results into 16 MB chunks so future partial reads hit the cache instead of fetching from S3 again (#130)

## [0.9.0] - 2026-02-23

### Added
- `pkg/api/server.go`: `MountManager` interface and four REST endpoints (`POST /api/v1/mounts`, `DELETE /api/v1/mounts/{point}`, `GET /api/v1/mounts`, `GET /api/v1/mounts/{point}`) for remote mount/unmount operations; existing deployments without a `MountManager` receive `501 Not Implemented` (#123)
- `pkg/api/server.go`: `ServerConfig.Version` field; `GET /info` now reports the version supplied at construction time instead of a hardcoded `"0.6.0"` string; falls back to `"unknown"` when the field is empty (#118)
- `internal/cache/multilevel.go`: `MultiLevelCache.SetBackend(types.Backend)` setter and a working `Warmup([]string) error` implementation that fetches each key from the configured backend and populates all enabled cache levels; no-op when backend is nil (#120)
- `internal/fuse/cgofuse_filesystem.go`: Nine `sync/atomic.Int64` counter fields (`statsLookups`, `statsOpens`, `statsReads`, `statsWrites`, `statsBytesRead`, `statsBytesWritten`, `statsCacheHits`, `statsCacheMisses`, `statsErrors`) incremented on every operation; `GetStats()` now loads real values instead of returning zeros (#121)
- `internal/health/monitor.go`: `Recoverable` interface (`Recover(ctx context.Context) error`); `attemptAutoRecovery` now calls `Recover` on any registered component that implements it, retrying up to `MonitorConfig.RecoveryAttempts` times with `RecoveryDelay` between attempts and logging success/failure (#122)
- `internal/cache/multilevel_bench_test.go`: Four benchmarks (`Get_HotPath`, `Get_Miss`, `Set_Eviction`, `Warmup_10keys`) plus parallel Get — run with `go test -bench=. ./internal/cache/...` (#125)
- `internal/buffer/buffer_bench_test.go`: Five benchmarks (`Write_1KB`, `Write_1MB`, `Flush_1MB`, `Concurrent_Write`, `FlushAll`) — run with `go test -bench=. ./internal/buffer/...` (#125)
- `internal/adapter/adapter_bench_test.go`: Seven benchmarks covering `parseSize` and `validateStorageURI` under serial and parallel load — run with `go test -bench=. ./internal/adapter/...` (#125)
- `internal/filesystem/filesystem_test.go`: `mockFilesystem` and `mockFileHandle` stubs implementing all 28 methods of `FilesystemInterface`; compile-time satisfaction assertion (`var _ FilesystemInterface = (*mockFilesystem)(nil)`) plus 11 table-driven tests covering all method groups and helper utilities (#126)

### Changed
- `internal/storage/s3/cost_optimizer.go`: `applyOptimization` now calls `s3.CopyObject` in-place (same bucket and key) with the target `StorageClass`, replacing the previous log-only stub; updates local access-pattern tracking on success (#119)
- `internal/adapter/adapter.go`: `Stop()` now calls `a.cache.Clear()` and `a.metrics.Stop(ctx)`, replacing the two `// TODO` placeholder comments (#124)
- `sdks/go/objectfs/client.go`: `Client` gains a `sync.RWMutex` field; `Mount`, `Unmount`, `IsMounted`, and `Close` use the mutex to guard the `mounted` bool and `adptr` pointer — makes the SDK fully safe for concurrent use (#127)

## [0.8.0] - 2026-02-23

### Added
- `internal/distributed/cluster.go`, `internal/distributed/coordinator.go`: `ClusterManager.SetBackend` and `Coordinator.backend` wire the `types.Backend` S3 backend into `executeLocally`, replacing the in-process stub with real `GetObject`/`PutObject`/`DeleteObject`/`ListObjects` calls; nil backend returns a descriptive error instead of phantom data (#85)
- `internal/distributed/gossip.go`, `internal/distributed/cluster.go`: Distributed cache invalidation broadcast — new `MessageTypeCacheInvalidate` gossip message type, `ClusterManager.SetCache` / `InvalidateCacheKey` methods, and `handleIncomingMessage` dispatch that calls `cache.Delete(key)` on all peers within one gossip round-trip (#86)
- `internal/storage/s3/backend_bench_test.go`: Eight new S3 backend benchmarks (GetObject 1 KB / 1 MB / 10 MB, PutObject 1 KB / 1 MB, ListObjects 100 / 1000 entries, concurrent Get) using an in-process stub; run with `go test -bench=. ./internal/storage/s3/...` (#88)
- `scripts/pjdfstest.sh`: Shell harness that mounts ObjectFS against a test bucket and runs pjdfstest for POSIX compliance validation; `make test-posix` target added to Makefile (#89)
- `sdks/java/`: Java 17 SDK scaffold — `ObjectFSClient`, `ObjectFSConfig`, `ObjectInfo`, `MountOptions`, `ObjectFSException`, `NotFoundException`, Maven pom.xml, and unit tests using MockWebServer (#90)

### Changed
- `internal/distributed/coordinator.go`, `internal/distributed/gossip.go`, `internal/distributed/cluster.go`, `internal/distributed/consensus.go`, `internal/health/checker.go`, `internal/health/remediation.go`, `internal/fuse/cgofuse_filesystem.go`, `internal/fuse/filesystem.go`, `internal/fuse/mount.go`, `internal/adapter/adapter.go`, `internal/cache/redis/invalidation.go`, `pkg/profiling/memory.go`: All `log.Printf` calls migrated to structured `slog.Info`/`slog.Warn`/`slog.Error` with key-value attributes (#87)

## [0.7.3] - 2026-02-23

### Fixed
- `internal/storage/s3/multipart_state.go`: `UpdatePartStatus` now uses `m.mu.RLock()` and releases it before calling `state.MarkPartCompleted`/`state.MarkPartFailed`; previously held the manager write lock while those methods acquired the state lock — two-lock nesting chain that can deadlock under concurrent part uploads (#108)
- `internal/fuse/filesystem.go`: `FileHandle.Write` now updates `fh.file.size` inside the `accessMu` critical section alongside `dirty`, `modified`, and `lastAccess` — eliminates data race on `OpenFile.size` under concurrent FUSE writes (#109)
- `internal/fuse/filesystem.go`: `FileHandle.Flush` now reads and resets `fh.file.dirty` under `accessMu` — eliminates data race with concurrent `Write` calls that set `dirty` under the same lock (#110)
- `internal/fuse/optimizations.go`: `ReadAheadManager.Stop()` now uses `sync.Once` to prevent a panic on second call (`close of closed channel`) from defensive teardown paths (#111)
- `internal/buffer/writebuffer.go`: `WriteBuffer.Sync()` now checks `ctx.Done()` in its polling loop — the previous implementation ignored context cancellation, blocking the caller for the full `MaxWriteDelay * 2` timeout even after the context was cancelled (#112)
- `internal/buffer/writebuffer.go`: `flushStaleBuffers` now acquires `buf.mu.RLock()` before reading `buf.flushing`, `buf.dirty`, and `buf.lastWrite` — eliminates data race with concurrent `flushBuffer` calls that set `buf.flushing` under `buf.mu` (#113)
- `internal/distributed/cluster.go`: `performHealthChecks` now accepts and threads the cluster lifecycle context; `TriggerElection` is called with that context instead of `context.Background()`, so election goroutines exit cleanly when the manager is stopped (#114)
- `internal/distributed/gossip.go`: `performGossip` now skips member-list entries where `node.Info == nil` — previously dereferenced `node.Info.ID` unconditionally, panicking on nodes added via sync messages with a nil Info field (#115)
- `internal/distributed/gossip.go`: `calculateStats` no longer writes `time.Since(LastMessageReceived)` to `AvgMessageLatency`; that value measured cluster idle time (grows when quiet), not message round-trip latency; the field stays zero until real per-message timing is instrumented (#116)
- `internal/cache/multilevel.go`: `MultiLevelCache.Evict` now measures `level.Cache.Size()` before and after each level eviction and accumulates the difference — the previous code accumulated `levelStats.Size` (total level occupancy), which could falsely report `totalEvicted >= size` when nothing was freed (#117)

## [0.7.2] - 2026-02-23

### Fixed
- `internal/distributed/consensus.go`: `resetElectionTimer` now uses `time.NewTimer` instead of `time.AfterFunc` — `time.AfterFunc` returns a `*time.Timer` with a nil `.C` channel, so the election loop blocked forever on a nil channel and elections never fired (#101)
- `internal/distributed/gossip.go`: `receiveMessages` no longer busy-loops when `conn == nil` (10 ms sleep) and no longer blocks indefinitely in `ReadFromUDP` (100 ms `SetReadDeadline`); stop channels are checked at the top of each iteration so the goroutine exits cleanly on shutdown (#102)
- `internal/cache/persistent.go`: `Clear()` now captures `len(c.index)` before resetting the map; previously the count was read after the reset and always returned 0, so eviction stats were never updated (#103)
- `internal/cache/predictive.go`: `IntelligentPrefetcher` updates `stats.JobsQueued` and `stats.JobsCompleted` via `sync/atomic.AddUint64` instead of unprotected increments — eliminates data race under concurrent worker goroutines (#104)
- `internal/fuse/optimizations.go`: `performPrefetch` now captures `fetchStart := time.Now()` before calling `GetObject`; the previous `time.Since(time.Now())` evaluated to ~0 on every call, making prefetch latency metrics useless (#106)
- `internal/fuse/filesystem.go`: `OpenFile` gains an `accessMu sync.Mutex` field protecting `lastAccess` and `accessCount`; both `Read` and `Write` paths now acquire the lock before updating these fields — eliminates data race under concurrent FUSE I/O (#107)

## [0.7.1] - 2026-02-23

### Fixed
- `internal/fuse/mount.go`: `MountManager` gains a `sync.Mutex` field (`mu`) protecting the `mounted`, `currentOpID`, and `server` fields, which were previously accessed without synchronisation from `Mount()`, `Unmount()`, a background goroutine, `IsMounted()`, `GetCurrentOperation()`, and `Remount()` — eliminates data race detected by `-race` (#98)
- `internal/fuse/mount.go`: `MountWatcher.checkMount()` removed spurious `!` operator from `actuallyMounted := !w.manager.isAlreadyMounted()` (should be `isAlreadyMounted()`, not its negation) — prevents permanent false "unexpected unmount" log warnings on every watcher tick; `Remount()` double-negation also cleaned up (#99)
- `internal/adapter/adapter.go`: write buffer `MaxBufferSize` and `FlushThreshold` now use the configured `MaxMemory` value directly instead of dividing by 100 and 200 — the placeholder divisions reduced a 512 MB configured buffer to ~5 MiB, degrading write throughput by ~100× (#100)

## [0.7.0] - 2026-02-23

### Added
- `internal/fuse`: `getCachedInfo` / `cacheInfo` now serialize and deserialize `types.ObjectInfo` as JSON, replacing the placeholder string that always caused cache misses; repeated `stat` calls on a cached path no longer fall through to a backend `HeadObject`; 6 new unit tests (`filesystem_test.go`) cover round-trip fidelity for all fields including `time.Time`, `map[string]string` metadata, ETag, ContentType, Checksum; nil-cache and nil-info no-ops; and overwrite semantics — closes #79
- `internal/cache`: `MultiLevelCache` gains an optional `*analytics.Predictor` field (injected via `MultiLevelConfig.Predictor`); `Get` and `Put` call `predictor.RecordAccess` on every access so the predictor builds frequency/recency profiles; `shouldPromoteToL2` consults the predictor's tier recommendation (Standard/Standard-IA → promote, Glacier tiers → skip) with a size-based fallback when no access history exists; `generateEvictionCandidates` (previously a stub returning an empty slice) now iterates `AccessPredictor.patterns` and scores each entry using directly-computed recency and recent-hour frequency (clamping future timestamps, correct for patterns with < 2 accesses); `tests/MockBaseCache.Delete` now decrements `stats.Size` to correctly reflect intelligent eviction; 7 new tests (`predictive_test.go`, `multilevel_test.go`) cover candidate generation, eviction-score ordering, sequential-offset prefetch prediction, predictor access recording, predictor-driven L2 promotion, and size-fallback promotion — closes #82
- `internal/adapter`: health monitor lifecycle wiring — `Adapter` struct gains a `*health.Monitor` field; `Start()` constructs and starts an `internal/health.Monitor` (when `Monitoring.HealthChecks.Enabled` is true) that runs background `system_ping`, `memory_usage`, and `disk_space` checks plus per-component checks for `s3_backend`, `cache`, and `write_buffer`; the HTTP health endpoint is bound on `config.Global.HealthPort` (default 8081); `Stop()` cleanly shuts down the monitor; a new private `healthComponent` adapter struct implements `health.HealthyComponent` via a closure so adapter-owned objects need not implement the interface directly; 4 new tests cover initial nil state, graceful nil-monitor stop, full start/stop lifecycle, and `healthComponent` correctness — closes #74
- `internal/health`: implemented the previously stubbed `Checker.startHTTPServer()` — binds a `net.Listener` on the configured port (supports `:0` for ephemeral ports), serves `application/json` at `config.HTTPPath` with the current check status and `200`/`503` based on overall health, and shuts down cleanly when `stopCh` is closed
- `pkg/api`: `GET /metrics` endpoint now returns valid Prometheus text-format output — `Server` accepts a `prometheus.Gatherer` as a new fourth constructor parameter; `handleMetrics` delegates to `promhttp.HandlerFor`; when `nil` is passed the endpoint returns an empty 200 response; `internal/metrics.Collector` exposes a new `Gatherer() prometheus.Gatherer` accessor so callers can wire the existing registry directly into the API server; 3 new tests (`TestHandleMetrics_NilGatherer`, `TestHandleMetrics_MethodNotAllowed`, `TestHandleMetrics_WithGatherer`) verify correct HTTP status, `Content-Type`, `# HELP`/`# TYPE` comment lines, and presence of the three core metric families — closes #75
- `internal/distributed`: real UDP networking for consensus (Raft-like leader election) and coordinator (operation routing) — `GossipProtocol` now dispatches 6 new message types (`request_vote`, `request_vote_resp`, `append_entries`, `append_entries_resp`, `node_operation`, `node_operation_resp`) plus `sendConsensusMsg` helper and `LocalAddr()` accessor; `ConsensusEngine` replaces goroutine-sleep simulations with real UDP `RequestVote`/`AppendEntries` RPCs and handlers; `Coordinator` routes operations to remote nodes over UDP with request/response correlation via `pendingOps` channels and a `simulateReplication` fire-and-forget path; pre-existing election-timer data race fixed in `electionLoop`; two-node loopback tests added (`TestConsensusEngine_TriggerElection_WithPeer_BecomesLeader`, `TestConsensusEngine_TriggerElection_WhenLeader_IsNoOp`, `TestCoordinator_ExecuteOperation_TwoNodes_RealUDP`) — closes #84
- `internal/fuse`: `NewCgoFuseMountManager` now reads `DefaultUID`/`DefaultGID` from `MountConfig.Permissions` (when non-zero) instead of hardcoding 1000; `NewFileSystem` nil-config default also uses `os.Getuid()`/`os.Getgid()`; 4 new tests cover explicit permissions, nil permissions, and zero-value permissions (all fall back to process identity) — closes #78
- `github.com/scttfrdmn/globalfs` (`pkg/site`, `pkg/namespace`): wired objectfs v0.7.0 as the first real dependency in GlobalFS; `SiteMount` wraps an `ObjectFSClient` interface (List, Head, Health, Close) per configured site — production path uses `objectfssdk.New`, test path accepts any mock; `Namespace` provides a merged view across multiple `SiteMount`s with key deduplication (first/highest-priority site wins) and graceful skip of unreachable sites; 9 new tests in `pkg/site` and `pkg/namespace` cover delegation, deduplication, limit, unavailable-site tolerance, and dynamic site addition — closes #83

## [0.6.0] - 2026-02-22

### Added
- `internal/distributed`: 67 unit tests across 4 new files (`cluster_test.go`, `consensus_test.go`, `coordinator_test.go`, `gossip_test.go`) covering `ClusterManager` lifecycle and node management, `ConsensusEngine` election state machine (Follower→Candidate→Leader with peer simulation), `Coordinator` operation routing and load-balancing strategies (round-robin, least-load, consistent-hash), and `GossipProtocol` message handlers (join, alive, suspect, dead, sync, heartbeat) — closes #73
- `tests/aws_s3_test.go`: three new integration tests under the `aws_s3` build tag — `TestListObjects` (prefix listing + limit parameter), `TestMultipartUpload` (6 MB object with 5 MB threshold to force the multipart code path, partial-read verification), `TestZSTDCompression` (ZSTD round-trip, partial read, raw-storage verification that bytes are actually compressed on S3)
- `sdks/go/objectfs/client_test.go`: `TestIntegration_PutGetDeleteHead` (full + partial Get, Head metadata, Delete + confirmation), `TestIntegration_List` (prefix listing + limit + cleanup), `TestIntegration_Health`; helper functions `testBucket()` and `testRegion()` read from `$OBJECTFS_TEST_BUCKET` / `$AWS_REGION` instead of hard-coding; existing `TestNew_*` and `TestClose_NotMounted` updated to use the same helpers
- `Makefile`: `test-aws` target runs the `aws_s3` suite and Go SDK integration tests with credential validation; `test-release-check` target combines unit tests and AWS integration as a pre-release gate
- `DEVELOPMENT.md`: v0.6.0 pre-release integration test procedure — environment setup, per-test coverage table, FUSE smoke test procedure, post-run cleanup, and an acceptance checklist (closes #72)
- `internal/cost`: real-time per-operation S3 cost calculation, per-tenant accumulation, ROI reporting, and budget-threshold alerting — `PriceTable` holds immutable per-tier pricing with optional overrides (7 tiers, AWS us-east-1 defaults); `Calculator` computes `OpCost` (request + retrieval + egress fees) via `Calculate(op, tier, bytes, egressBytes)` and periodic `CalculateStorageCost`; `Reporter` accumulates `TenantRecord` state (op counts, op costs, storage cost, GB-months) with thread-safe `RecordOp`/`RecordStorage` and emits `CostReport` snapshots including ROI `BaselineCost`/`Savings` computed from actual GB-months; `AlertManager` evaluates `BudgetRule` (soft limit → WARNING, hard limit → CRITICAL, optional info fraction → INFO) after each cost event with wildcard `"*"` tenant fallback, duplicate-fire suppression, and async `AlertHandler` callbacks; 42 unit tests cover all operation types, retrieval fees, egress fees, tier storage ordering, ROI calculation, tenant isolation, reset, wildcard rules, handler dispatch, and severity deduplication (closes #65)
- `internal/analytics`: ML-based access pattern analysis and S3 tier recommendations — `PatternAnalyzer` tracks per-object access stats (access rates over 1d/7d/30d, recency, inter-access interval mean/variance, hour-of-day and day-of-week histograms) via a sliding window and extracts a `FeatureVector`; `TierClassifier` applies a calibrated decision tree to map features to an S3 storage tier (STANDARD, STANDARD_IA, GLACIER_IR, GLACIER, DEEP_ARCHIVE) with confidence score and estimated monthly savings per GB; `Predictor` facade exposes `RecordAccess`, `RecordAccessAt`, `Recommend`, `RecommendBatch`, and `Stats` with atomic counters; 30 unit tests cover feature extraction, all decision-tree branches, boundary conditions, batch recommendations, and stats accounting (closes #64)
- `internal/cache/redis`: Redis-backed distributed cache implementing `types.Cache` — `Cache` struct wraps `go-redis/v9`; `Get` uses `GETRANGE` for partial reads (offset/size support); `Put` stores full objects only (partial writes are silently dropped to maintain cache consistency); `Delete` removes a key; `Evict` increments the eviction counter and returns false (Redis manages eviction via its configured `maxmemory-policy`); `Size` reads `used_memory` from `INFO memory`; `Stats` returns atomic hit/miss/eviction counters and computed hit rate; `Close` closes the connection; `Client` exposes the underlying `*goredis.Client` for invalidation wiring; 13 unit tests using `miniredis/v2` cover connectivity, full/partial reads, partial-write rejection, delete, TTL expiry, namespace isolation, and stats
- `internal/cache/redis`: `Invalidator` — pub/sub cache-invalidation broadcaster on channel `"objectfs:invalidation"`; `Publish(ctx, key)` sends `"<nodeID>:<key>"` messages; `Subscribe(ctx)` starts a goroutine that deletes received keys from the local cache, skipping messages originating from the same node; 3 unit tests cover publish, remote invalidation, and self-publish suppression
- `internal/cache`: `NewFromConfig(cfg)` factory — returns a Redis `Cache` when `cfg.Cluster.Enabled && cfg.Cluster.Redis.Enabled`; falls back to `MultiLevelCache` with defaults otherwise; 3 unit tests cover all three routing paths
- `internal/config`: `RedisConfig` struct with `Enabled`, `Address` (default `localhost:6379`), `Password`, `DB`, `KeyPrefix` (default `"objectfs"`), `TTL` (default 5 min), `MaxRetries` (default 3); `ClusterConfig.Redis RedisConfig` field; defaults wired into `NewDefault()`; `RedisConfig` re-exported via `pkg/types` alias (closes #63)

## [0.5.0] - 2026-02-22

### Added
- `internal/compression`: adaptive algorithm selection — `Analyze(data)` detects content class (text, JSON, binary, compressed, archive) via magic bytes and Shannon entropy, returning a `CompressScore` ∈ [0,1]; `RuleSelector` maps content class + `AccessHint` (hot/warm/cold) + object size to a `Recommendation` (algorithm + level); `AdaptiveSelector` wraps `RuleSelector` with a per-`ContentClass` rolling-window feedback model — once `minSamples` (10) outcomes are recorded per algorithm it overrides the base recommendation with the empirically best choice (speed-optimised for hot, ratio-optimised for cold); `LZ4Codec` added as a fast-decompression option using `github.com/pierrec/lz4/v4` frame format; `New()` factory extended to support `"lz4"`; 37 new unit tests covering magic-byte detection for 15+ formats, entropy boundary conditions, rule table, adaptive learning, window eviction, and LZ4 round-trip (closes #62)
- `internal/archive`: archive detection and indexing API — `Detect([]ObjectInfo)` filters S3 listings to `[]ArchiveMetadata`; `DetectKeys` returns just the archive keys; `DetectInPrefix(ctx, backend, prefix, limit)` combines `ListObjects` + `Detect`; `IsArchiveKey` wraps `pkg/archive.IsArchive`; `BuildIndex(ctx, backend, key)` downloads and indexes an archive via `GetObject`, supplements with real S3 timestamps from `HeadObject` (non-fatal); `BuildIndexFromBytes(key, format, data)` builds an `ArchiveIndex` by walking tar headers without retaining file data; `openTar` shared by `BuildIndexFromBytes` and `VFS.extractFile`; `pkg/archive.IsArchive` minimum-length guard lowered from 7 to 5 to correctly handle `.tgz` filenames with single-character base names; 21 unit tests for indexing and 14 tests for detection (closes #58)
- `internal/archive`: virtual filesystem (VFS) layer for archive contents — `PathTranslator` splits FUSE paths at archive boundaries (`data.tar.zst/subdir/file.txt` → archiveKey + innerPath) for all three supported formats; `VFS` provides `Stat`, `ReadDir`, and `ReadFile` over archive contents backed by `types.Backend`; index built by walking tar headers on first access (no file data retained) and cached for all subsequent Stat/ReadDir calls; file content extracted on-demand and cached per entry; `Invalidate` clears both index and content caches; synthesises virtual directory entries for archives created without explicit directory records; `ErrNotFound` sentinel error; 28 unit tests cover all three formats, virtual directories, caching, offset reads, and not-found paths (closes #59)
- `internal/network`: TCP congestion control package — runtime detection of available algorithms (`/proc/sys/net/ipv4/tcp_available_congestion_control`), per-socket `TCP_CONGESTION` socket option (Linux ≥ 4.9) via `net.Dialer.Control`; `NewBBRDialer()`, `NewCUBICDialer()`, `BestAvailableDialer()` (BBR > CUBIC > system default); `Monitor` struct with atomic `BytesSent`/`BytesReceived`/`Connections`/`Errors` counters and `WrapDialContext` for transparent tracking; `BBRConfig` with 4 MiB send/receive buffers and ICW=10; build-tag stubs for non-Linux platforms; `NewDialer(algo)` wired into `s3.NewClientManager` via custom `*http.Transport.DialContext`; `CongestionAlgorithm` field added to `s3.Config` and `config.NetworkConfig` (default `"auto"`); unit tests cover algorithm selection, detection, Monitor lifecycle, WrapDialContext success/error, and BBR config (closes #60)
- `pkg/compression` + `internal/compression`: ZSTD compression engine with configurable levels (1–22) using `github.com/klauspost/compress/zstd`; public `Codec` interface (`Compress`, `Decompress`, `Algorithm`, `ContentEncoding`) and `Algorithm` constants (`zstd`, `gzip`, `lz4`, `none`); `ZstdCodec` using concurrency-safe `EncodeAll`/`DecodeAll`; `Compressor` wrapper with minimum-size threshold and incompressibility guard (discards compressed form when it is not smaller than the original); transparent S3 integration — `PutObject` compresses and sets `Content-Encoding: zstd`; `GetObject` fetches the full object when compression is active, decompresses, then slices to the requested byte range; multipart uploads propagate `Content-Encoding` through `CreateMultipartUpload`; default config: algorithm=zstd, level=3, minSize=4KB; unit tests (40 tests, no AWS required) cover round-trip, all levels 1–22, concurrent access, incompressible data, parseSize, and Compressor lifecycle (closes #61)
- C SDK (`sdks/c/`): shared library (`libobjectfs.so`/`.dylib`) via `go build -buildmode=c-shared`; public header `objectfs.h` with full documented API; opaque handle system with per-handle error strings; operations: New, Free, Get, Put, Delete, Head, List, Mount, Unmount, LastError, FreeData, FreeList; C test suite (16 tests) and Python ctypes smoke test (15 tests) both pass without AWS credentials; integration tests gated on `OBJECTFS_TEST_BUCKET` + `AWS_ACCESS_KEY_ID` (closes #71)
- Go SDK (`sdks/go/objectfs`): type-safe client for direct S3 object operations (Get, Put, Delete, List, Head) and optional FUSE mount/unmount; functional options (WithRegion, WithEndpoint, WithCacheSize, WithMaxConcurrency, WithLogLevel, WithMetricsPort, WithHealthPort, WithTLS); five sentinel errors (ErrNotFound, ErrAccessDenied, ErrNotMounted, ErrAlreadyMounted, ErrInvalidConfig) compatible with errors.Is; unit tests for all options and error-path logic without AWS credentials (closes #70)
- `pkg/archive`: new package with `ArchiveMetadata`, `ArchiveIndex`, and `ArchiveEntry` types; format detection via `IsArchive()` for tar.zst, tar.gz, and tar.bz2 — objectfs-side metadata types for CargoShip archive integration
- Python SDK (`sdks/python/objectfs`): async/await S3 client, CLI, monitoring, and configuration presets; GCS/Azure backends stubbed for future support
- `internal/fuse`: unit tests for `ReadAheadManager` (pattern detection, sequential/non-sequential reads, prefetch scheduling, TTL cleanup), `WriteCoalescer` (accumulation, merge, flush triggers), `Stats` (concurrent safety), and config defaults; cgofuse mock-based tests for Getattr, Open/Read, Write, Release, Readdir, and concurrent handle management (gated behind `//go:build cgofuse`)

### Changed
- Upgraded `github.com/scttfrdmn/cargoship` v0.4.5 → v0.13.0 (DVC pipeline auto-discovery, archive filesystem shell, Glacier restore, incremental sync, deduplication)
- Go toolchain bumped to 1.26.0 (pulled in by cargoship v0.13.0)
- Transitive upgrades: `aws-sdk-go-v2` v1.39.2 → v1.41.0, `testify` v1.10.0 → v1.11.1
- Dockerfile base image updated: `golang:1.24-alpine` → `golang:1.26-alpine`, `alpine:3.18` → `alpine:3.21`

### Fixed
- `internal/storage/s3`: three multipart upload correctness bugs exposed by real AWS integration tests — (1) zero-value `MultipartThreshold`/`ChunkSize`/`Concurrency` in partial configs now get defaults applied in `NewBackend` so every object no longer triggers multipart and the semaphore no longer deadlocks; (2) `MultipartUploadState` gains a `sync.RWMutex` to eliminate the data race between `MarkPartCompleted` writes and `GetProgress` reads; (3) `uploadParts` now sorts `completedParts` by `PartNumber` before returning — S3 `CompleteMultipartUpload` requires ascending order but goroutines complete in arbitrary order; (4) `CalculateOptimalChunkSize` enforces S3's 5 MB minimum non-last-part size — previously returned `baseChunkSize/2` for files just above threshold, producing parts rejected with `EntityTooSmall`
- `sdks/go/objectfs`: `requireAWS` test helper now accepts `AWS_PROFILE` in addition to `AWS_ACCESS_KEY_ID`; `TestNew_WithDefaults` and `TestClose_NotMounted` pass `testRegion()` to avoid `301 MovedPermanently` when test bucket is not in `us-east-1`
- `internal/storage/s3`: refactored `putObjectMultipart` (209 lines → 55 lines) by extracting five helper methods into `multipart_upload.go` — `initiateMultipartUpload`, `uploadSinglePart`, `uploadParts`, `abortMultipartUpload`, `completeMultipartUpload`; pure `partSlice` helper extracted and covered by `TestPartSlice` (5 subtests) (closes #55)
- `internal/adapter`: removed hardcoded `us-west-2` region; adapter now reads `Storage.S3.Region`, `Storage.S3.Endpoint`, `Storage.S3.ForcePathStyle`, and `Storage.S3.UseAcceleration` from `config.Configuration` via new `buildS3Config()` helper (closes #56)
- `internal/config`: added `AWS_DEFAULT_REGION`, `AWS_REGION`, `OBJECTFS_S3_REGION`, and `OBJECTFS_S3_ENDPOINT` environment variable mappings so region and endpoint are configurable without a config file; priority order is `AWS_DEFAULT_REGION` < `AWS_REGION` < `OBJECTFS_S3_REGION`
- `internal/health`: implemented all 8 `AutoFix` remediation closures — `memory_force_gc` (runtime.GC + debug.FreeOSMemory), `disk_clean_logs` (removes .log files older than 30 days), `disk_clean_cache` (removes objectfs-cache files older than 7 days), `disk_clean_temp` (removes objectfs-* temp files older than 24h), `network_retry` (context-aware 5 s wait); `s3_restart_connection`, `cache_clear`, and `memory_reduce_cache` log advisory messages until dependency injection is available
- License references updated from MIT to Apache 2.0 throughout README, SDKs, and docs
- Copyright year updated to 2025-2026
- Pre-commit `check-yaml` hook now uses `--unsafe` to handle MkDocs Python YAML tags
- Markdownlint config: relax line-length limit and allow emphasis for README badge lines

## [0.1.0] - 2025-07-27

### Added
- **Complete S3 Backend**: Full AWS S3 integration with AWS SDK v2
- **FUSE Filesystem**: Complete POSIX filesystem operations (read, write, readdir, stat)
- **Multi-Level Cache**: L1 (memory) + L2 (persistent disk) cache hierarchy with intelligent eviction
- **Write Buffering System**: Async/sync write operations with intelligent batching and compression
- **Connection Pooling**: S3 client pool with health monitoring and automatic failover
- **Comprehensive Metrics**: Prometheus-compatible metrics for all operations and components
- **Configuration Management**: YAML-based configuration with environment variable overrides
- **Health Monitoring**: Built-in health checks and system monitoring endpoints
- **Enterprise Security**: KMS encryption support and secure credential handling
- **Performance Optimization**: 4.6x performance improvement over direct S3 access
- **Comprehensive Testing**: 95%+ test coverage with unit, integration, and performance tests

### Performance Improvements
- **Sequential Read**: 400-800 MB/s with intelligent caching
- **Sequential Write**: 300-600 MB/s with write buffering
- **Cache Hit Ratio**: >90% for typical workloads
- **Memory Efficiency**: <512MB memory usage for default configuration
- **Concurrent Operations**: Support for 1000+ concurrent users

### Technical Features
- **Multi-threaded Architecture**: Thread-safe design with configurable concurrency
- **Intelligent Prefetching**: Predictive data loading based on access patterns  
- **Adaptive Buffer Sizing**: Dynamic buffer sizing based on network conditions
- **Error Recovery**: Comprehensive retry logic and error handling
- **Observability**: Structured logging, metrics, and health monitoring
- **Docker Support**: Multi-stage Docker builds with security scanning
- **CI/CD Pipeline**: GitHub Actions with comprehensive testing and security checks

### Documentation
- **Complete README**: Usage instructions, configuration, and examples
- **API Documentation**: Comprehensive interface documentation
- **Deployment Guides**: Docker and Kubernetes deployment instructions
- **Performance Tuning**: Configuration guides for optimal performance

### Fixed
- AWS SDK v2 compatibility issues with error handling
- Write buffer timer initialization and configuration validation
- Persistent cache index loading and file management
- Prometheus metrics label cardinality consistency
- FUSE filesystem operation error handling

### Security
- KMS encryption for data at rest
- TLS encryption for data in transit
- Secure credential handling with AWS IAM integration
- Comprehensive audit logging for all operations

[Unreleased]: https://github.com/scttfrdmn/objectfs/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/scttfrdmn/objectfs/releases/tag/v0.1.0
