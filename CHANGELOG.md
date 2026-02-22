# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
