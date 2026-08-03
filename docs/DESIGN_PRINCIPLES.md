# Design Principles: ObjectFS

## Core Principles

These principles guide every architectural and implementation decision in ObjectFS. When faced with trade-offs, we return to these principles to make consistent choices.

---

## 1. Transparency Over Cleverness

**Principle**: ObjectFS should be invisible to applications. The best filesystem is one you never think about.

### What This Means

- ✅ **A POSIX interface**: applications call `open`/`read`/`write` and get a filesystem, with no
  ObjectFS-specific API. Not full POSIX compliance — this said "Full POSIX compliance" against a
  fraction of the VFS surface, and the operations that are missing or refused are in the
  [README's supported-operations table](../README.md#supported-operations), which is the authority
  on the count as well as the list. The principle is that
  ObjectFS does not ask applications to change; where S3 cannot support an operation, the aim is to
  fail loudly rather than to appear compliant
- ✅ **No API changes required**: Existing code works unmodified
- ✅ **Predictable behavior**: Operations behave as expected
- ❌ **No magic**: No hidden state changes or surprising behaviors
- ❌ **No proprietary APIs**: Standard Linux interfaces only

### Examples

**Good**: Application calls `open()`, `read()`, `write()` - works transparently
```c
// Application code - unchanged
FILE *f = fopen("/mnt/objectfs/data.bam", "r");
fread(buffer, size, 1, f);
```

**Bad**: Application must use ObjectFS-specific API
```c
// BAD: Don't do this
objectfs_file *f = objectfs_open("s3://bucket/data.bam");
```

### Trade-offs

- **Performance**: Sometimes POSIX semantics are inefficient for object storage
- **Resolution**: Use caching and buffering to make POSIX fast, don't break compatibility

---

## 2. Performance Through Caching, Not Complexity

**Principle**: Achieve high performance through intelligent caching, not by complicating the code paths.

### What This Means

- ✅ **Multi-tier caching**: LRU, persistent, predictive strategies
- ✅ **Measured optimization**: Profile before optimizing
- ✅ **Simple hot paths**: Critical paths should be straightforward
- ❌ **Premature optimization**: Don't optimize without data
- ❌ **Complex synchronization**: Avoid intricate locking schemes

### Examples

**Good**: Use simple cache lookup with clear hit/miss paths
```go
// Simple, fast cache lookup
if data, ok := cache.Get(key); ok {
    return data // Fast path
}
return s3.GetObject(key) // Slow path
```

**Bad**: Complex multi-level locking and state management
```go
// BAD: Don't do this
mutex1.Lock()
if state == PENDING {
    mutex2.Lock()
    // Complex logic...
```

### Trade-offs

- **Memory usage**: Caching trades memory for speed
- **Resolution**: Configurable cache sizes with clear limits and eviction

---

## 3. Fail Loudly, Recover Gracefully

**Principle**: Make failures obvious, but recover automatically when possible.

### What This Means

- ✅ **Clear error messages**: Tell users what went wrong and how to fix it
- ✅ **Structured logging**: JSON logs with context
- ✅ **Health monitoring**: Detect problems proactively
- ✅ **Auto-remediation**: Fix common issues automatically
- ❌ **Silent failures**: Never fail silently
- ❌ **Cryptic errors**: No "error code 42" messages

### Examples

**Good**: Clear error with context and remediation
```go
log.Error().
    Str("bucket", bucket).
    Str("key", key).
    Err(err).
    Msg("Failed to fetch object from S3 after 3 retries. Check S3 permissions and network connectivity.")
```

**Bad**: Silent failure or cryptic error
```go
// BAD: Silent failure
if err != nil {
    return nil // User has no idea what happened
}

// BAD: Cryptic error
return fmt.Errorf("error: %d", 42)
```

### Trade-offs

- **Log volume**: Detailed logging can be verbose
- **Resolution**: Configurable log levels, structured fields for filtering

---

## 4. Data Integrity Over Performance

**Principle**: Never sacrifice data correctness for speed. Research data is irreplaceable.

### What This Means

- ✅ **Atomic per object**: a PUT either replaces the object or leaves it untouched
- ✅ **Checksums that are read**: every written object records a SHA-256, and the read path compares it
- ✅ **Errors reach the caller**: a failed PUT fails `close(2)`; it is not logged and swallowed
- ✅ **State the limits**: what is *not* guaranteed is documented as prominently as what is
- ❌ **Unsafe optimizations**: no "fast but unsafe" modes
- ❌ **Silent fallback**: never return data that could not be verified, or success for a write that failed

### Examples

**Good**: write, record the hash, verify it on the way back

```go
sum := sha256.Sum256(data)
_, err := s3.PutObject(ctx, &s3.PutObjectInput{
    Bucket:   &bucket,
    Key:      &key,
    Body:     bytes.NewReader(data),
    Metadata: map[string]string{"objectfs-sha256": hex.EncodeToString(sum[:])},
})
// ... and on the read side, the part that makes it worth anything:
if err := verifyChecksum(info.Metadata, got, key); err != nil {
    return nil, err   // an integrity error, not bytes with exit status 0
}
```

This is what `internal/storage/s3` does. A single `PutObject` **is** the atomic operation on S3 —
the object is either the old one or the new one, never a splice of both — so there is nothing a
temp-key dance would add.

**Bad**: recording a checksum and never reading it

```go
// BAD, and this was v0.10.0. The hash was computed on every upload and stored as
// user metadata, and no read path anywhere compared it against the bytes returned.
sum := sha256.Sum256(data)
s3.PutObject(ctx, withMetadata(key, data, sum))
// ... no verification on read. A codec mismatch, a lost Content-Encoding header,
// a truncated body, or bit-rot all came back with exit status 0.
```

Evidence that was written and never read is worse than no evidence, because it makes the system look
verified. This document previously presented a temp-key-plus-`CopyObject` sequence as the good
pattern and a bare `PutObject` as the bad one — while the bare `PutObject` *was* the implementation.
That inversion is worth naming: the pattern being labelled was not the defect. The missing read of
the checksum was.

**Also bad**: the temp-key dance the previous version of this document recommended

```go
// BAD: strictly less safe than a direct PUT, and more expensive.
tmpFile := fmt.Sprintf("%s.tmp.%s", key, uuid)
s3.PutObject(tmpFile, data)
s3.CopyObject(tmpFile, key)   // NOT atomic with anything; a second PUT under a second key
s3.DeleteObject(tmpFile)
```

Three requests instead of one, three failure points instead of one, an orphaned temp object on any
failure between them, and a `CopyObject` that does not inherit the source's `Content-Encoding` or its
server-side encryption. The direct PUT was already atomic.

### Trade-offs

- **Verification costs a read of the whole object.** The recorded hash is over the whole content, so
  a partial read of a large object cannot be verified without transferring all of it. ObjectFS does
  not do that, and says so rather than implying a guarantee it does not provide. Per-range checksums
  need a change to the stored object layout, tracked with the seekable-framing work.
- **Synchronous flush costs latency.** `close(2)` waits for the PUT. The alternative is returning
  success before the data is durable, which is not a trade-off this project makes.

---

## 5. Configuration Over Convention

**Principle**: Provide sensible defaults, but make everything configurable. Different workloads have different needs.

### What This Means

- ✅ **Clear defaults**: Works out-of-box for common cases
- ✅ **Override everything**: Every parameter is configurable
- ✅ **Validation**: Catch configuration errors early
- ✅ **Documentation**: Every option is documented with examples
- ❌ **Hidden magic**: No undocumented behaviors
- ❌ **One-size-fits-all**: Different workloads need different tuning

### Examples

**Good**: Documented configuration with validation
```yaml
cache:
  eviction_policy: weighted_lru   # Options: lru, lfu, weighted_lru
  persistent_cache:
    enabled: true
    max_size: 100GB               # Validated: must parse as a size
    directory: /var/cache/objectfs
```

**Bad**: Hard-coded values or undocumented options
```go
// BAD: Hard-coded cache size
const CacheSize = 50 * 1024 * 1024 * 1024
```

### Trade-offs

- **Complexity**: More options = more documentation needed
- **Resolution**: Group related options, provide profiles (small/medium/large workload)

---

## 6. Measure Everything

**Principle**: You can't optimize what you can't measure. Comprehensive metrics are essential.

### What This Means

- ✅ **Prometheus metrics**: Standard format, all components
- ✅ **Performance counters**: Latency, throughput, cache hits
- ✅ **Cost tracking**: Per-operation cost calculation
- ✅ **Health checks**: Proactive problem detection
- ❌ **Black boxes**: No unmeasurable components
- ❌ **Guesswork**: Decisions based on data, not intuition

### Examples

**Good**: Comprehensive metrics for every operation
```go
metrics.CacheHits.Inc()
metrics.S3Latency.Observe(duration.Seconds())
metrics.BytesRead.Add(float64(len(data)))
```

**Bad**: No instrumentation
```go
// BAD: No visibility into what's happening
data := cache.Get(key)
return data
```

### Trade-offs

- **Performance overhead**: Metrics collection has cost
- **Resolution**: Use efficient metrics library (Prometheus), sample high-frequency events

---

## 7. Secure by Default

**Principle**: Security should be the default, not an opt-in feature.

### What This Means

- ✅ **Least privilege**: Minimal permissions required
- ✅ **Encryption**: TLS by default for S3 connections
- ✅ **Input validation**: Sanitize all external inputs
- ✅ **Secure credentials**: Never log secrets
- ❌ **Insecure defaults**: No "disable TLS for speed" options
- ❌ **Credential exposure**: No secrets in logs, metrics, or errors

### Examples

**Good**: TLS by default, credentials from environment
```go
s3Client := s3.New(s3.Options{
    TLS:         true,  // Default
    Credentials: aws.NewCredentialsFromEnv(), // Not in config file
})
```

**Bad**: Insecure defaults or exposed credentials
```go
// BAD: Insecure default
s3Client := s3.New(s3.Options{
    TLS: false, // Dangerous default
})

// BAD: Logging credentials
log.Info("S3 key: %s", accessKey)
```

### Trade-offs

- **Convenience**: Security adds friction
- **Resolution**: Good UX for secure defaults (env vars, credential helpers)

---

## 8. Test Rigorously

**Principle**: Comprehensive testing prevents regressions and builds confidence.

### What This Means

- ✅ **Unit tests**: 80%+ coverage for business logic
- ✅ **Integration tests**: End-to-end scenarios
- ✅ **Stress tests**: High concurrency, large files, error injection
- ✅ **Race detector**: All tests run with `-race`
- ✅ **Benchmark tests**: Performance regression detection
- ❌ **Manual testing**: No "works on my machine"
- ❌ **Happy path only**: Test failure modes extensively

### Examples

**Good**: Comprehensive test coverage
```go
func TestCacheEviction(t *testing.T) {
    // Test normal eviction
    // Test concurrent eviction
    // Test eviction failure
    // Test edge cases (empty cache, single item, etc.)
}
```

**Bad**: Minimal or no tests
```go
// BAD: No tests for critical functionality
func CacheEvict(key string) error {
    // Complex logic, no tests
}
```

### Trade-offs

- **Development speed**: Writing tests takes time
- **Resolution**: Tests are mandatory, not optional - they save time long-term

---

## 9. Documentation is Code

**Principle**: Documentation is as important as the code itself.

### What This Means

- ✅ **Code comments**: Why, not what
- ✅ **Package docs**: Purpose and usage of every package
- ✅ **User docs**: Comprehensive guides and tutorials
- ✅ **Examples**: Real-world usage examples
- ✅ **Architecture docs**: System design and decisions
- ❌ **Self-documenting code**: Code alone is never enough
- ❌ **Stale docs**: Documentation must be maintained

### Examples

**Good**: Clear comments explaining decisions
```go
// Use persistent cache for frequently re-read reference genomes
// Performance impact: 10x faster than LRU for this workload
// Memory impact: 100GB cache size
if workloadType == ReferenceGenome {
    cache = NewPersistentCache(cacheConfig)
}
```

**Bad**: Obvious or missing comments
```go
// BAD: States the obvious
i++ // increment i

// BAD: No explanation for complex logic
cache = selectCacheStrategy(workload, config, metrics)
```

### Trade-offs

- **Maintenance burden**: Docs need updating
- **Resolution**: Docs in code (go doc), CI checks for doc coverage

---

## 10. Simple Over Clever

**Principle**: Simple, straightforward code beats clever, complex code.

### What This Means

- ✅ **Boring is good**: Prefer proven patterns over novel approaches
- ✅ **Readable code**: Code is read 10x more than written
- ✅ **Clear abstractions**: Obvious boundaries between components
- ✅ **Minimal dependencies**: Fewer dependencies = less complexity
- ❌ **Premature abstraction**: Don't abstract until you have 3+ use cases
- ❌ **Clever tricks**: No "look how smart I am" code

### Examples

**Good**: Simple, clear code
```go
// Simple cache lookup - anyone can understand this
if data, ok := cache.Get(key); ok {
    return data, nil
}
return s3.GetObject(key)
```

**Bad**: Clever but confusing code
```go
// BAD: Clever but hard to understand
return map[bool]func()string{
    true: func() string { return cache.Get(key) },
    false: func() string { return s3.GetObject(key) },
}[cache.Has(key)]()
```

### Trade-offs

- **Brevity**: Simple code may be more verbose
- **Resolution**: Verbosity is acceptable if it improves clarity

---

## Architectural Principles

### Layered Architecture

ObjectFS uses clean separation of concerns:

```
┌─────────────────────────────────────┐
│   FUSE Layer (POSIX Interface)      │
├─────────────────────────────────────┤
│   Metadata Manager                   │
├─────────────────────────────────────┤
│   Cache Manager                      │
│   (LRU / Persistent / Predictive)   │
├─────────────────────────────────────┤
│   Write Buffer Manager               │
├─────────────────────────────────────┤
│   S3 Backend                         │
│   (Multi-part, Retry, Circuit Breaker)│
├─────────────────────────────────────┤
│   Health Monitor & Metrics           │
└─────────────────────────────────────┘
```

**Each layer**:
- Has a clear, single responsibility
- Can be tested independently
- Has well-defined interfaces
- Can be mocked for testing

### Dependency Management

**Prefer standard library** over external dependencies:
- `net/http` instead of custom HTTP client
- `context` for cancellation
- `sync` for concurrency primitives

**When using external dependencies**:
- Vet thoroughly (popularity, maintenance, security)
- Wrap in abstraction layer (enables replacement)
- Pin versions (go.mod)
- Regular security audits (govulncheck)

### Error Handling

ObjectFS uses structured error handling:

```go
// Always provide context
if err != nil {
    return fmt.Errorf("failed to get object %s from bucket %s: %w",
        key, bucket, err)
}

// Use typed errors for handling
if errors.Is(err, ErrNotFound) {
    return nil // Not found is OK for cache
}

// Log with context
log.Error().
    Err(err).
    Str("operation", "s3.GetObject").
    Str("bucket", bucket).
    Str("key", key).
    Msg("S3 operation failed")
```

### Concurrency

**Principle**: Use channels and goroutines for clarity, mutexes when necessary.

```go
// Good: Channel-based coordination
results := make(chan Result)
for _, key := range keys {
    go func(k string) {
        results <- fetchObject(k)
    }(key)
}

// Good: Clear mutex boundaries
type SafeCache struct {
    mu    sync.RWMutex
    items map[string][]byte
}

func (c *SafeCache) Get(key string) ([]byte, bool) {
    c.mu.RLock()
    defer c.mu.RUnlock()
    data, ok := c.items[key]
    return data, ok
}
```

### Performance Guidelines

1. **Profile before optimizing**: Use pprof, don't guess
2. **Optimize hot paths**: Focus on 90% of time spent
3. **Cache intelligently**: Multi-tier caching with clear policies
4. **Batch operations**: Group S3 operations when possible
5. **Parallel I/O**: Concurrent reads/writes where safe

---

## Anti-Patterns to Avoid

### 1. God Objects

❌ **Bad**: Single object that does everything
```go
type ObjectFS struct {
    // Everything in one struct
    cache        Cache
    s3           S3Client
    metadata     MetadataStore
    health       HealthMonitor
    metrics      MetricsExporter
    // 50 more fields...
}
```

✅ **Good**: Focused, composable components
```go
type ObjectFS struct {
    fuse     *FUSELayer
    cache    *CacheManager
    backend  *S3Backend
    health   *HealthMonitor
}
```

### 2. Leaky Abstractions

❌ **Bad**: Implementation details leak through interface
```go
// BAD: Exposes S3-specific details
cache.PutWithS3ETag(key, data, etag)
```

✅ **Good**: Clean abstraction
```go
// Good: Generic interface
cache.Put(key, data, metadata)
```

### 3. Premature Optimization

❌ **Bad**: Optimizing without measurements
```go
// BAD: Complex caching without profiling
// (Turns out lookup() was fast, upload() was slow)
func lookup(key string) {
    // 500 lines of optimization
}
```

✅ **Good**: Measure first, optimize what matters
```go
// Good: Profile showed upload() was bottleneck
func upload(data []byte) {
    // Optimize the actual slow path
}
```

---

## Decision Framework

When making design decisions, ask:

1. **Transparency**: Does this break POSIX compatibility?
2. **Performance**: Have we measured the impact?
3. **Reliability**: What happens on failure?
4. **Security**: Is this secure by default?
5. **Simplicity**: Is this the simplest solution that works?
6. **Testability**: Can we test this thoroughly?
7. **Maintainability**: Will we understand this in 6 months?

If any answer is "no", reconsider the approach.

---

## Evolution of Principles

These principles are living guidelines, not immutable laws. As ObjectFS matures and our understanding deepens, we may refine or add principles.

**Last Updated**: November 2025
**Next Review**: Post v0.5.0 (Q2 2026)

---

*"Perfection is achieved, not when there is nothing more to add, but when there is nothing left to take away."* - Antoine de Saint-Exupéry
