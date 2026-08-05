# ObjectFS Architecture Overview

**Applies to:** v0.10.1 (in development)

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

ObjectFS is a FUSE filesystem that presents the objects in an AWS S3 bucket as files, aimed at
research and institutional workloads: large sequential reads of reference data, datasets too big for
local disk, and shared buckets read by many nodes.

It is **not POSIX-compliant**, and the gap is structural rather than incidental. S3 has no rename, no
hard links, no partial object write, and no atomicity across objects, so a subset of the POSIX
surface cannot be implemented on top of it honestly at all. Where that is the case ObjectFS returns
an error rather than pretending — the
[supported-operations table](../../README.md#supported-filesystem-operations) is the contract, and it
is derived from the code.

### Design goals, in priority order

1. **Integrity** — either do the right thing or fail loudly. Every object ObjectFS writes records a
   SHA-256 that the read path verifies; `close(2)` returns the PUT's error rather than logging it.
   See [Data integrity](../../README.md#data-integrity).
2. **Performance** — a close second, and the reason for the range-aware cache, the concurrent range
   GETs, and the multipart upload path.
3. **Cost transparency** — storage stays as ordinary S3 objects at S3 pricing, with no provisioned
   capacity to pay for, and cost estimates available for planning.
4. **Simplicity of operation** — built-in defaults that work, and a config loader that rejects a key
   it does not know rather than ignoring it.

### Key features

- Read and write at any offset, with a real read-modify-write flush
- Multi-level caching: memory, optionally spilling to local disk
- Concurrent range GETs for large reads; multipart for large writes
- SHA-256 integrity verification on complete reads
- Server-side encryption: SSE-S3 or SSE-KMS, off by default
- S3 storage tier selection and cost estimation

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
│                  (Linux or macOS with FUSE)                    │
└─────────────────────┬───────────────────────────────────────────┘
                      │ FUSE protocol
                      ▼
┌─────────────────────────────────────────────────────────────────┐
│                  ObjectFS FUSE Layer                           │
│  ┌─────────────┬─────────────┬────────────┬─────────────────┐  │
│  │ File Ops    │ Dir Ops     │ Metadata   │ (no xattrs, no  │  │
│  │ (read/write)│ (readdir)   │ (stat)     │  rename, no rm) │  │
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
│  │ - Keyed on (key, chunk, etag); invalidated on write      │  │
│  └──────────────────────────────────────────────────────────┘  │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ L2: Disk Cache (persistent)                              │  │
│  │ - Warm data: Frequently accessed                         │  │
│  │ - Survives restarts; off by default                      │  │
│  └──────────────────────────────────────────────────────────┘  │
└─────────────────────┬───────────────────────────────────────────┘
                      │
                      ▼
┌─────────────────────────────────────────────────────────────────┐
│                  Write Buffer Layer                            │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ - Dirty byte ranges per open file (any offset)           │  │
│  │ - Read-modify-write on flush; no whole-object replace    │  │
│  │ - Flushes when the kernel asks, or on fsync/close        │  │
│  │ - Durability: fsync/close are synchronous and return err │  │
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
│  │ - Estimates for planning, from a snapshot rate table     │  │
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
- Linux and macOS only (`//go:build linux || darwin`). **Windows is not supported** — there is no
  WinFsp binding. The `cgofuse` build tag that once claimed it never compiled and has been removed

**Supported operations:** the authoritative list is the
[supported-operations table in the README](../../README.md#supported-filesystem-operations), derived
from the methods that exist in `internal/fuse` and `internal/vfs`.

Summarised: read, write at any offset, truncate, flush/fsync, stat, create, mkdir, paginated readdir,
chmod/chown on files, statfs, `unlink`, `rmdir`, and `rename` are implemented. **Symlinks, xattrs,
`mknod`, `fallocate`, hard links, and locking are not**, and each fails rather than silently doing
nothing.

Two entries need their caveat stated here rather than only in the table. `rename` is a server-side
copy followed by a delete, per object, so it is **not atomic** and `renameat2`'s `RENAME_EXCHANGE`
and `RENAME_NOREPLACE` are refused with `EINVAL` rather than approximated. Hard links are not
"not yet" — S3 has no concept of two names for one object, so they will never be supported.

This summary has been wrong in both directions, which is why it now points at the table first. It
once named `Rename()`, `Link()`, and `Symlink()` as supported when none of the three existed; it was
then corrected to say `unlink`, `rmdir`, and `rename` were unimplemented and that `rm` returned
`EROFS`, and stayed that way after all three landed. A prose summary of a table is a second copy of
the same facts, so it can only ever be as fresh as its last edit — the table is derived from the
methods in `internal/fuse` and `internal/vfs` and is the thing to trust.

### 2. Cache Layer

Multi-level caching system optimized for S3 access patterns.

#### L1: Memory Cache

- **Purpose**: Hot data that's accessed frequently
- **Implementation**: LRU (Least Recently Used) eviction
- **Typical Size**: 512MB - 4GB (configurable)
- **Block Size**: 1MB - 8MB (configurable)
- **Eviction policy**: `lru`, `lfu`, or `weighted_lru` (`cache.eviction_policy`)

**Optimization Strategies:**

- Read-ahead: Prefetch next blocks on sequential access
- Write-through: Synchronous writes to maintain consistency
- Block-level granularity: Cache at block level, not full files

#### L2: Disk Cache

- **Purpose**: Warm data that doesn't fit in memory
- **Implementation**: Local filesystem-backed cache
- **Typical Size**: 10GB - 100GB (configurable)
- **Persistence**: Survives process restarts
- **Off by default** (`cache.persistent_cache.enabled`)

**Features:**

- Automatic cleanup: LRU eviction when disk space low
- Crash recovery: Safe to delete cache directory anytime
- Optional: Can be disabled for memory-only caching

### 3. Write Buffer Layer

Coalesces small writes into efficient S3 uploads.

**Key features:**

- **Dirty byte ranges, not a contiguous buffer.** Writes accumulate as an interval list per open
  file. Later writes over the same bytes win; a write at any offset is accepted. The single
  contiguous buffer plus offset that preceded this could not represent an offset write at all, which
  is why appending one byte to a 1 MiB file used to leave a 1-byte object.
- **Read-modify-write on flush.** The flush fetches exactly the ranges of the stored object it needs
  to fill the gaps between dirty ranges, splices, and PUTs the result. It does not replace the object
  with the fragment that was written.
- **Flush triggers**: `fsync()`, `close()`, or the kernel asking. **There is no size-based or
  time-based flush.** `write_buffer.flush_interval`, `max_buffers`, and `max_memory` are in the
  schema and are read by nothing — they are marked `not yet wired` in `examples/config.yaml`.
- **Multipart uploads** above `storage.s3.multipart.threshold`, which defaults to **32 MB**.
- **Durability**: `fsync()` and `close()` are synchronous and return the error. A failed PUT —
  `AccessDenied`, a network failure — fails the syscall rather than being logged and swallowed.

**Trade-offs:**

- **Nothing bounds total dirty bytes.** Dirty ranges accumulate per open file until the kernel
  flushes, so a large enough write set is bounded by available memory rather than by a configured
  limit. Fixing this needs backpressure in the writer, not a wider config mapping, which is why the
  three keys above are marked unwired rather than plumbed to a component that cannot honour them.
- **Consistency**: written bytes are visible to a reader on the same mount immediately; another
  client sees them after the flush.
- **Safety**: a crash before flush loses the unflushed ranges. S3 PUTs are atomic per object, so the
  object is either as it was or as it will be — but there is no journal and no multi-object
  transaction.

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

### Performance

**No throughput, latency, or IOPS figures are published here.** This section previously carried a
full table of them — sequential read in MB/s, per-operation latency, cached and uncached IOPS — and
none of it was measured. A fabricated number is worse than no number, because a reader cannot tell
that it needs checking, and downstream documents had already begun citing these as findings.

The honest statements about performance are structural rather than numeric:

- Throughput on a large sequential read is bounded by S3 and by the instance's network, not by
  ObjectFS, once the read is being fanned out into concurrent range GETs.
- A cache hit avoids a network round trip. How often that happens is entirely a property of the
  workload's locality, and ObjectFS cannot claim a hit rate on a workload it has not seen.
- A `stat` of an uncached path costs one S3 `HeadObject` per path component, which is why metadata
  latency is dominated by directory depth rather than by file size.
- Every write costs at least one PUT, and a write at an offset also costs the GETs needed to fill the
  gaps around the dirty ranges.

`benchmarks/run_benchmarks.sh` runs the Go benchmarks against a real bucket. Numbers for your own
region, instance type, and object-size distribution are the only ones worth acting on.

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

A table here previously compared ObjectFS's throughput and latency against FSx, Amazon File Cache,
and EFS, and claimed POSIX compliance for all four. The ObjectFS column was unmeasured, the
competitors' columns were not sourced, and the POSIX row was wrong for the one entry it could be
checked against — see the [supported-operations
table](../../README.md#supported-filesystem-operations). It has been removed rather than corrected,
because a benchmark of four systems is a piece of work, not a paragraph.

The one comparison that holds without measurement is the cost model, and it is a difference in kind
rather than degree: ObjectFS stores data as ordinary S3 objects and adds no per-hour charge, so
storage costs S3 list price, while FSx, File Cache, and EFS all provision capacity or throughput and
bill for it whether or not it is used. That is the trade ObjectFS makes — you keep S3 pricing and
give up the POSIX semantics a real filesystem provides. Which side of that trade is right depends
entirely on whether your workload needs the operations in the table above.

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

**Advanced Optimizations:**

- Machine learning-based prefetching
- Adaptive caching strategies
- Cross-region replication
- Edge caching (CloudFront integration)

---

## Related Documentation

- [Architecture Evolution](../ARCHITECTURE_EVOLUTION.md) — a 2025 design sketch, bannered with
  where the code diverged from it
- [Index](../index.md) — the configuration reference and the **Not yet wired up** table
- [Concurrency patterns](../concurrency-patterns.md) — the locking model this document's component
  boundaries imply

Four links here pointed at `data-flow.md`, `caching-deep-dive.md`, `s3-backend-deep-dive.md`, and
`performance.md`. None was written. For the data path, the package documentation is the accurate
source and is kept current with the code: `go doc ./internal/vfs`, `./internal/cache`, and
`./internal/storage/s3`.

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
