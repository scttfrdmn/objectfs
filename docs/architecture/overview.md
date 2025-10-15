# ObjectFS Architecture Overview

**Version:** v0.3.0
**Last Updated:** October 15, 2025

---

## Table of Contents

1. [Introduction](#introduction)
2. [High-Level Architecture](#high-level-architecture)
3. [Core Components](#core-components)
4. [Data Flow](#data-flow)
5. [Performance Architecture](#performance-architecture)
6. [Deployment Models](#deployment-models)
7. [Future Evolution](#future-evolution)

---

## Introduction

ObjectFS is a high-performance FUSE filesystem that provides POSIX-compliant
access to AWS S3 buckets. It's designed for research and enterprise workloads
requiring both high performance and cost optimization.

### Design Goals

- **Performance**: Competitive with or exceeding AWS alternatives (FSx, File
  Cache)
- **Cost**: Significantly lower than AWS managed services (260x advantage)
- **Simplicity**: Easy to deploy and operate
- **Compatibility**: Standard POSIX interface for maximum compatibility

### Key Features

- FUSE-based filesystem with POSIX compliance
- Multi-level intelligent caching
- Write buffering and coalescing
- S3 storage tier optimization
- Enterprise pricing awareness
- Integrated cost tracking

---

## High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    User Applications                            │
│        (cp, ls, grep, analysis tools, IDEs, etc.)              │
└─────────────────────┬───────────────────────────────────────────┘
                      │ Standard POSIX calls (open, read, write)
                      ▼
┌─────────────────────────────────────────────────────────────────┐
│                  Operating System VFS                          │
│              (Linux/macOS/Windows with FUSE)                   │
└─────────────────────┬───────────────────────────────────────────┘
                      │ FUSE protocol
                      ▼
┌─────────────────────────────────────────────────────────────────┐
│                  ObjectFS FUSE Layer                           │
│  ┌─────────────┬─────────────┬────────────┬─────────────────┐  │
│  │ File Ops    │ Dir Ops     │ Metadata   │ Extended Attrs  │  │
│  │ (read/write)│ (readdir)   │ (stat)     │ (xattr)         │  │
│  └─────────────┴─────────────┴────────────┴─────────────────┘  │
└─────────────────────┬───────────────────────────────────────────┘
                      │
                      ▼
┌─────────────────────────────────────────────────────────────────┐
│                    Cache Layer                                 │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ L1: Memory Cache (LRU)                                   │  │
│  │ - Hot data: Recently accessed files                      │  │
│  │ - Block-level caching (configurable block size)          │  │
│  │ - Fast: <1ms access latency                              │  │
│  └──────────────────────────────────────────────────────────┘  │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ L2: Disk Cache (persistent)                              │  │
│  │ - Warm data: Frequently accessed                         │  │
│  │ - Survives restarts                                      │  │
│  │ - Fast: <10ms access latency                             │  │
│  └──────────────────────────────────────────────────────────┘  │
└─────────────────────┬───────────────────────────────────────────┘
                      │
                      ▼
┌─────────────────────────────────────────────────────────────────┐
│                  Write Buffer Layer                            │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ - Coalesces small writes into large S3 uploads           │  │
│  │ - Reduces S3 PUT request costs                           │  │
│  │ - Configurable flush intervals and size thresholds       │  │
│  │ - Durability: fsync() forces immediate flush             │  │
│  └──────────────────────────────────────────────────────────┘  │
└─────────────────────┬───────────────────────────────────────────┘
                      │
                      ▼
┌─────────────────────────────────────────────────────────────────┐
│                    S3 Backend Layer                            │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ S3 Operations                                            │  │
│  │ - GetObject / PutObject / HeadObject                     │  │
│  │ - ListObjectsV2 (directory listings)                     │  │
│  │ - DeleteObject / CopyObject                              │  │
│  │ - Multipart uploads for large files                      │  │
│  └──────────────────────────────────────────────────────────┘  │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ Storage Tier Management                                  │  │
│  │ - STANDARD / STANDARD_IA / INTELLIGENT_TIERING          │  │
│  │ - Automatic tier selection based on access patterns      │  │
│  │ - Lifecycle policy management                            │  │
│  └──────────────────────────────────────────────────────────┘  │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ Cost Optimization                                        │  │
│  │ - Enterprise discount awareness                          │  │
│  │ - Volume pricing tier tracking                           │  │
│  │ - Per-operation cost calculation                         │  │
│  │ - Real-time cost monitoring                              │  │
│  └──────────────────────────────────────────────────────────┘  │
└─────────────────────┬───────────────────────────────────────────┘
                      │ AWS SDK v2 (Go)
                      ▼
┌─────────────────────────────────────────────────────────────────┐
│                       AWS S3                                   │
│  Multiple Storage Tiers with Enterprise Pricing               │
└─────────────────────────────────────────────────────────────────┘
```

---

## Core Components

### 1. FUSE Layer

The FUSE (Filesystem in Userspace) layer provides the POSIX filesystem
interface that applications interact with.

**Key Responsibilities:**

- Translate POSIX operations to internal ObjectFS calls
- Handle file descriptors and open file tracking
- Manage directory entry caching
- Implement POSIX semantics (permissions, ownership, timestamps)

**Implementation:**

- Built on `github.com/hanwen/go-fuse` library
- Supports both Linux and macOS FUSE
- Windows support via WinFsp

**Supported Operations:**

- File operations: `Open()`, `Read()`, `Write()`, `Release()`, `Flush()`
- Directory operations: `OpenDir()`, `ReadDir()`, `Mkdir()`, `Rmdir()`
- Metadata operations: `GetAttr()`, `SetAttr()`, `Chmod()`, `Chown()`
- Extended: `Create()`, `Unlink()`, `Rename()`, `Link()`, `Symlink()`

### 2. Cache Layer

Multi-level caching system optimized for S3 access patterns.

#### L1: Memory Cache

- **Purpose**: Hot data that's accessed frequently
- **Implementation**: LRU (Least Recently Used) eviction
- **Typical Size**: 512MB - 4GB (configurable)
- **Block Size**: 1MB - 8MB (configurable)
- **Latency**: <1ms for cache hits
- **Hit Rate**: Target 60-80% for typical workloads

**Optimization Strategies:**

- Read-ahead: Prefetch next blocks on sequential access
- Write-through: Synchronous writes to maintain consistency
- Block-level granularity: Cache at block level, not full files

#### L2: Disk Cache

- **Purpose**: Warm data that doesn't fit in memory
- **Implementation**: Local filesystem-backed cache
- **Typical Size**: 10GB - 100GB (configurable)
- **Persistence**: Survives process restarts
- **Latency**: <10ms for cache hits (depends on disk)
- **Hit Rate**: Target 20-40% additional hits

**Features:**

- Automatic cleanup: LRU eviction when disk space low
- Crash recovery: Safe to delete cache directory anytime
- Optional: Can be disabled for memory-only caching

### 3. Write Buffer Layer

Coalesces small writes into efficient S3 uploads.

**Key Features:**

- **Write Buffering**: Accumulates writes before uploading to S3
- **Flush Triggers**:
  - Size threshold: Buffer reaches configured size (e.g., 5MB)
  - Time threshold: Buffer age exceeds timeout (e.g., 30s)
  - Explicit flush: `fsync()` or `close()` called
- **Multipart Uploads**: Automatically uses multipart for large files (>5MB)
- **Durability**: `fsync()` guarantees data is in S3 before returning

**Trade-offs:**

- **Performance**: Reduces S3 PUT requests by 10-100x
- **Consistency**: Small delay before data visible in S3
- **Safety**: Data loss risk if process crashes before flush (mitigated by
  regular flushes)

### 4. S3 Backend Layer

Core S3 integration layer handling all cloud storage operations.

**Components:**

#### a) S3 Client

- **Library**: AWS SDK for Go v2
- **Features**:
  - Connection pooling
  - Automatic retries with exponential backoff
  - Request signing and authentication
  - Regional endpoint optimization

#### b) Storage Tier Manager

Intelligently selects S3 storage class based on access patterns.

**Supported Tiers:**

- **STANDARD**: Frequent access (>1x/month)
- **STANDARD_IA**: Infrequent access (1x/month to 1x/quarter)
- **INTELLIGENT_TIERING**: Automatic tiering based on access
- **GLACIER_INSTANT**: Archive with instant retrieval

**Tier Selection Logic:**

```
Access Frequency         → Recommended Tier
────────────────────────────────────────────
Daily                   → STANDARD
Weekly/Monthly          → INTELLIGENT_TIERING
Quarterly/Rarely        → STANDARD_IA
Archive (with instant)  → GLACIER_INSTANT
```

#### c) Cost Optimizer

Tracks and optimizes S3 costs in real-time.

**Features:**

- **Request Cost Tracking**: Monitor GET/PUT/LIST costs
- **Storage Cost Tracking**: Per-tier storage costs
- **Transfer Cost Tracking**: Data transfer costs
- **Optimization Recommendations**: Suggest tier changes to reduce costs
- **Budget Alerting**: Warn when approaching cost thresholds

#### d) Pricing Manager

Applies enterprise discounts and volume pricing.

**Capabilities:**

- **Enterprise Discounts**: Load custom discount configurations
- **Volume Pricing**: Automatic tier adjustment for volume discounts
- **Multi-Account**: Support for consolidated billing discounts
- **Real-Time Calculation**: Cost estimates for every operation

### 5. Metrics & Monitoring

**Collected Metrics:**

- **Performance**:
  - Cache hit rates (L1, L2)
  - Operation latencies (read, write, list)
  - Throughput (MB/s)
  - IOPS
- **Cost**:
  - Per-operation costs
  - Daily/weekly/monthly spending
  - Cost per TB stored
  - Cost per million requests
- **Usage**:
  - Files accessed
  - Data transferred
  - Storage used per tier
  - Active users/applications

**Export Formats:**

- Prometheus metrics endpoint
- JSON logs
- CloudWatch (optional)

---

## Data Flow

### Read Path

```
1. Application calls read(fd, buffer, size)
   ↓
2. FUSE layer receives read request
   ↓
3. Check L1 Memory Cache
   ├─ HIT → Return data immediately (<1ms)
   └─ MISS → Continue
        ↓
4. Check L2 Disk Cache
   ├─ HIT → Load to L1, return data (<10ms)
   └─ MISS → Continue
        ↓
5. S3 Backend: GetObject request
   ↓
6. AWS S3 returns data (10-100ms)
   ↓
7. Store in L2 Disk Cache
   ↓
8. Store in L1 Memory Cache
   ↓
9. Return data to application
```

**Performance Characteristics:**

- Cache hit (L1): <1ms
- Cache hit (L2): <10ms
- S3 hit (same region): 10-50ms
- S3 hit (cross-region): 50-200ms
- First byte latency: Dominated by S3 latency on cold reads

### Write Path

```
1. Application calls write(fd, buffer, size)
   ↓
2. FUSE layer receives write request
   ↓
3. Write to L1 Memory Cache (mark dirty)
   ↓
4. Add to Write Buffer
   ├─ Buffer full? → Flush to S3
   ├─ Timeout? → Flush to S3
   └─ fsync()? → Flush to S3 immediately
        ↓
5. Coalesce writes in buffer
   ↓
6. S3 Backend: PutObject (or multipart)
   ↓
7. AWS S3 acknowledges write
   ↓
8. Update L2 Disk Cache
   ↓
9. Return success to application
```

**Performance Characteristics:**

- Write to buffer: <1ms (async)
- Flush to S3: 20-100ms (depends on size)
- fsync() latency: Equal to S3 PUT latency
- Write amplification: ~1.1x (minimal overhead)

### Directory Listing

```
1. Application calls readdir(path)
   ↓
2. FUSE layer receives readdir request
   ↓
3. Check directory cache
   ├─ HIT (fresh) → Return cached entries
   └─ MISS or STALE → Continue
        ↓
4. S3 Backend: ListObjectsV2 with prefix
   ↓
5. AWS S3 returns object list
   ↓
6. Parse S3 keys into directory structure
   ├─ Extract filenames
   ├─ Infer directories from key prefixes
   └─ Build directory entries
        ↓
7. Cache directory entries (TTL: 30s)
   ↓
8. Return entries to application
```

**Performance Characteristics:**

- Cached listing: <1ms
- S3 listing (small): 20-50ms
- S3 listing (large): 100-1000ms (pagination)
- Listing 1000 objects: ~50ms
- Listing 100,000 objects: ~5s (multiple requests)

---

## Performance Architecture

### Design Principles

1. **Cache Aggressively**: Minimize S3 requests through multi-level caching
2. **Batch Operations**: Coalesce small operations into large ones
3. **Prefetch Intelligently**: Predict access patterns and prefetch data
4. **Parallelize**: Use concurrent S3 requests for large operations
5. **Optimize Hot Path**: Make common operations extremely fast

### Performance Targets

**Throughput:**

- Sequential read: 400-800 MB/s (limited by S3, not ObjectFS)
- Sequential write: 300-600 MB/s (with write buffering)
- Random read (cached): 200-400 MB/s (memory speed)
- Random read (uncached): 50-100 MB/s (S3 latency bound)

**Latency:**

- File open (cached metadata): <1ms
- File open (uncached): 20-50ms
- Small read (4KB, cached): <0.1ms
- Small read (4KB, uncached): 20-50ms
- Large read (1MB, cached): 5-10ms
- Large read (1MB, uncached): 30-80ms

**IOPS:**

- Metadata operations (cached): 10,000+ IOPS
- Metadata operations (uncached): 100-500 IOPS
- Small reads (cached): 50,000+ IOPS
- Small writes (buffered): 20,000+ IOPS

### Optimization Techniques

#### 1. Read-Ahead

Prefetch blocks on sequential access patterns:

```
User reads block N
→ Prefetch blocks N+1, N+2, N+3 in background
→ Next read hits cache (0ms latency)
```

**Configuration:**

- Prefetch window: 3-10 blocks
- Trigger: 2+ sequential reads detected
- Adaptive: Adjusts based on hit rate

#### 2. Multipart Upload

Large files use parallel multipart uploads:

```
Write 100MB file:
→ Split into 10x 10MB parts
→ Upload parts in parallel (5 workers)
→ Complete multipart upload
→ Throughput: 5x faster than serial
```

**Configuration:**

- Part size: 5-10 MB
- Concurrency: 5-10 workers
- Threshold: Files >5MB

#### 3. Metadata Caching

Cache S3 HEAD requests to avoid unnecessary API calls:

```
stat() on cached file:
→ Return cached metadata (0ms)
→ Refresh in background if stale

stat() on uncached file:
→ S3 HeadObject request (30ms)
→ Cache result (TTL: 60s)
```

#### 4. Connection Pooling

Reuse HTTP connections to S3:

```
HTTP Keep-Alive: 100 connections
→ Avoid TLS handshake overhead
→ Reduces latency by 20-50ms per request
```

### Comparison to Alternatives

| Metric | ObjectFS | Amazon FSx | Amazon File Cache | EFS |
|--------|----------|-----------|-------------------|-----|
| Sequential Read | 400-800 MB/s | 1-2 GB/s | 1-4 GB/s | 500 MB/s |
| Sequential Write | 300-600 MB/s | 500 MB/s | 1-2 GB/s | 100 MB/s |
| Latency (cached) | <1ms | <1ms | <1ms | 1-3ms |
| Latency (uncached) | 20-50ms | 0.5-1ms | 1-5ms | 1-3ms |
| **Cost (1TB/month)** | **$23** | **$1,664** | **$4,800** | **$300** |
| POSIX Compliant | Yes | Yes | Yes | Yes |
| S3 Backend | Yes | No | Yes | No |

**Key Insights:**

- ObjectFS trades some performance for massive cost savings (260x vs File
  Cache)
- For workloads with good cache locality, ObjectFS performs similarly to
  expensive alternatives
- Cached operations are comparable or faster than managed services

---

## Deployment Models

### 1. Single-Node Development

```
┌──────────────────────┐
│   Developer Laptop   │
│  ┌────────────────┐  │
│  │   ObjectFS     │  │
│  │   /mnt/s3      │  │
│  └────────┬───────┘  │
│           │          │
└───────────┼──────────┘
            │
            ▼
    ┌───────────────┐
    │   AWS S3      │
    │   us-west-2   │
    └───────────────┘
```

**Use Case:** Local development, testing, small datasets

**Characteristics:**

- Single ObjectFS process
- Local cache (memory + disk)
- Direct S3 access
- Simple configuration

### 2. Single-Server Production

```
┌─────────────────────────────────┐
│      Production Server          │
│  ┌────────────────────────────┐ │
│  │   ObjectFS (systemd)       │ │
│  │   /mnt/research-data       │ │
│  │   Cache: 32GB mem + 500GB  │ │
│  └──────────┬─────────────────┘ │
│             │                    │
│  ┌──────────▼─────────────────┐ │
│  │   Application Workloads    │ │
│  │   - Analysis tools         │ │
│  │   - Data processing        │ │
│  └────────────────────────────┘ │
└─────────────────┬───────────────┘
                  │
                  ▼
          ┌───────────────┐
          │   AWS S3      │
          │   us-west-2   │
          └───────────────┘
```

**Use Case:** Medium-scale research, departmental workloads

**Characteristics:**

- Single ObjectFS daemon
- Large cache for better hit rates
- Multiple local processes access mount point
- Systemd service management

### 3. Multi-Server with Shared Cache

```
┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│  Compute-1   │  │  Compute-2   │  │  Compute-3   │
│ ┌──────────┐ │  │ ┌──────────┐ │  │ ┌──────────┐ │
│ │ ObjectFS │ │  │ │ ObjectFS │ │  │ │ ObjectFS │ │
│ └────┬─────┘ │  │ └────┬─────┘ │  │ └────┬─────┘ │
└──────┼───────┘  └──────┼───────┘  └──────┼───────┘
       │                 │                 │
       └─────────────────┴─────────────────┘
                         │
                         ▼
                 ┌───────────────┐
                 │ Shared Cache  │
                 │  (Redis/TBD)  │
                 └───────┬───────┘
                         │
                         ▼
                 ┌───────────────┐
                 │   AWS S3      │
                 │   us-west-2   │
                 └───────────────┘
```

**Use Case:** Large-scale distributed compute (future)

**Characteristics:**

- Multiple ObjectFS instances
- Shared distributed cache layer
- Cache coherency protocol
- Horizontal scalability

**Status:** Future enhancement (v0.6.0+)

---

## Future Evolution

### Short-Term (v0.4.0 - v0.5.0)

**Performance Enhancements:**

- AWS-C-S3 integration for 5-10x throughput improvement
- S3 Transfer Acceleration support
- Enhanced multipart upload optimization
- Improved read-ahead heuristics

**Features:**

- S3 Select integration for query pushdown
- Compression support (transparent to applications)
- Encryption at rest (application-level)
- CloudWatch metrics integration

### Mid-Term (v0.6.0 - v0.7.0)

**Multi-Protocol Support:**

See [ARCHITECTURE_EVOLUTION.md](../ARCHITECTURE_EVOLUTION.md) for details.

- SMB/CIFS protocol handler (Windows compatibility)
- NFS v4 protocol handler
- WebDAV for browser/mobile access
- Common filesystem interface abstraction

**Distributed Features:**

- Shared cache layer (Redis/Memcached)
- Cache coherency protocol
- Multi-region support
- Failover and HA

### Long-Term (v0.8.0 - v1.0.0)

**Enterprise Features:**

- LDAP/Active Directory integration
- Fine-grained ACLs and permissions
- Audit logging and compliance
- Data lifecycle management
- Integration with CargoShip for BBR/CUBIC optimization

**Advanced Optimizations:**

- Machine learning-based prefetching
- Adaptive caching strategies
- Cross-region replication
- Edge caching (CloudFront integration)

---

## Related Documentation

- [Data Flow Deep Dive](data-flow.md) - Detailed data path analysis
- [Caching Deep Dive](caching-deep-dive.md) - Cache implementation details
- [S3 Backend Deep Dive](s3-backend-deep-dive.md) - S3 integration details
- [Performance Architecture](performance.md) - Performance design and tuning
- [Architecture Evolution](../ARCHITECTURE_EVOLUTION.md) - Future multi-protocol
  design

---

## Summary

ObjectFS provides a high-performance, cost-effective POSIX filesystem backed by
AWS S3. Through intelligent multi-level caching, write buffering, and S3 tier
optimization, it achieves competitive performance with managed services at a
fraction of the cost.

**Key Architectural Strengths:**

- ✅ Simple, understandable design
- ✅ Proven FUSE foundation
- ✅ Effective multi-level caching
- ✅ Cost-aware S3 tier management
- ✅ Extensible for future protocols

**Current Limitations:**

- Single-node only (no distributed caching)
- S3 latency bound for cache misses
- FUSE protocol only (SMB/NFS coming)
- Limited consistency guarantees across processes

**Future Direction:**

ObjectFS is evolving toward a multi-protocol file server supporting FUSE, SMB,
NFS, and WebDAV, while maintaining its core S3 backend and cost optimization
advantages.
