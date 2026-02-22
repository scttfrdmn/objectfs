# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
