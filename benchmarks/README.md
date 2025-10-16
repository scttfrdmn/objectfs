# ObjectFS Performance Benchmarks

This directory contains comprehensive performance benchmarks for ObjectFS v0.4.0.

## Overview

The benchmark suite validates performance improvements introduced in v0.4.0:

- **S3 Transfer Acceleration**: Cross-region transfer performance
- **Multipart Upload Optimization**: Large file upload performance
- **Enhanced Error Handling**: Circuit breaker and retry mechanism overhead
- **Memory Management**: Memory allocation and leak detection overhead
- **Cache Performance**: Multi-level caching efficiency
- **Concurrency**: Parallel operation performance

## Running Benchmarks

### Prerequisites

1. **AWS Credentials**: Configure AWS credentials with S3 access
   ```bash
   export AWS_ACCESS_KEY_ID=your_key
   export AWS_SECRET_ACCESS_KEY=your_secret
   ```

2. **S3 Bucket**: Create a test bucket with Transfer Acceleration enabled
   ```bash
   export OBJECTFS_BENCH_BUCKET=your-test-bucket
   export OBJECTFS_BENCH_REGION=us-west-2
   ```

### Quick Start

Run all benchmarks:
```bash
./benchmarks/run_benchmarks.sh
```

Run specific benchmark suite:
```bash
# S3 acceleration benchmarks
go test -bench=. -benchmem ./internal/storage/s3/

# Configuration validation benchmarks
go test -bench=. -benchmem ./internal/config/

# Adapter initialization benchmarks
go test -bench=. -benchmem ./internal/adapter/
```

### Benchmark Options

- **Short mode** (faster, fewer iterations):
  ```bash
  go test -bench=. -short ./...
  ```

- **Detailed memory profiling**:
  ```bash
  go test -bench=. -benchmem -memprofile=mem.out ./...
  ```

- **CPU profiling**:
  ```bash
  go test -bench=. -cpuprofile=cpu.out ./...
  ```

- **Run specific benchmark**:
  ```bash
  go test -bench=BenchmarkMultipart_100MB_Accelerated ./internal/storage/s3/
  ```

## Benchmark Suites

### 1. S3 Transfer Performance

**Location**: `internal/storage/s3/acceleration_bench_test.go`

Tests standard vs. accelerated transfer performance:

```bash
go test -bench=BenchmarkGetObject ./internal/storage/s3/
go test -bench=BenchmarkPutObject ./internal/storage/s3/
```

**Key Benchmarks**:
- `BenchmarkGetObject_Standard` - Standard S3 downloads
- `BenchmarkGetObject_Accelerated` - Accelerated downloads
- `BenchmarkPutObject_Standard` - Standard uploads
- `BenchmarkPutObject_Accelerated` - Accelerated uploads

**Expected Results** (cross-region):
- GET operations: 30-60% faster with acceleration
- PUT operations: 40-70% faster with acceleration

### 2. Multipart Upload Performance

Tests multipart upload optimization for large files:

```bash
go test -bench=BenchmarkMultipart ./internal/storage/s3/
```

**Key Benchmarks**:
- `BenchmarkMultipart_32MB` - Threshold size (standard)
- `BenchmarkMultipart_32MB_Accelerated` - Threshold with acceleration
- `BenchmarkMultipart_100MB` - Medium files
- `BenchmarkMultipart_500MB` - Large files
- `BenchmarkMultipartConcurrency` - Concurrency level comparison

**Expected Results**:
- 32MB: ~2-3x faster than single-part
- 100MB: ~3-4x faster than single-part
- 500MB: ~4-5x faster than single-part
- Optimal concurrency: 8-16 parallel parts

### 3. Error Handling Overhead

Tests the performance overhead of enhanced error handling:

```bash
go test -bench=BenchmarkFallback ./internal/storage/s3/
go test -bench=BenchmarkAccelerationOverhead ./internal/storage/s3/
```

**Key Benchmarks**:
- `BenchmarkFallback` - Fallback mechanism latency
- `BenchmarkAccelerationOverhead` - Error detection overhead

**Expected Results**:
- Fallback latency: <50ms
- Error detection overhead: <1µs per operation

### 4. Configuration Performance

Tests configuration parsing and validation overhead:

```bash
go test -bench=. ./internal/config/
```

**Expected Results**:
- Config parsing: <1ms
- Config validation: <100µs

### 5. Comprehensive Suite

Runs all benchmark combinations:

```bash
go test -bench=BenchmarkSuite ./internal/storage/s3/
```

Tests all combinations of:
- Object sizes: 1KB, 1MB, 10MB
- Transfer modes: Standard, Accelerated
- Operations: GET, PUT

## Interpreting Results

### Understanding Output

```
BenchmarkGetObject_Standard-8       100    12345678 ns/op    1048576 B/op    42 allocs/op
```

- `BenchmarkGetObject_Standard-8`: Benchmark name with 8 parallel goroutines
- `100`: Number of iterations (N)
- `12345678 ns/op`: Average time per operation (nanoseconds)
- `1048576 B/op`: Bytes allocated per operation
- `42 allocs/op`: Number of allocations per operation

### Performance Metrics

Good performance indicators for v0.4.0:

1. **S3 Transfer Acceleration**
   - Cross-region latency reduction: >30%
   - Throughput increase: >40%

2. **Multipart Upload**
   - >100MB files: >3x faster than single-part
   - Memory usage: <10% increase per concurrent part

3. **Error Handling**
   - Circuit breaker overhead: <5% latency increase
   - Fallback latency: <100ms

4. **Memory Efficiency**
   - Cache overhead: <2GB for default configuration
   - No memory leaks over sustained operations

## Benchmark Baseline (v0.4.0)

Expected performance characteristics:

### S3 Operations (us-west-2, 1MB objects)

| Operation | Standard | Accelerated | Improvement |
|-----------|----------|-------------|-------------|
| GET       | ~150ms   | ~90ms       | 40%         |
| PUT       | ~180ms   | ~100ms      | 44%         |

### Multipart Uploads (us-west-2)

| Size  | Single-Part | Multipart (8 parts) | Improvement |
|-------|-------------|---------------------|-------------|
| 32MB  | ~1.5s       | ~550ms              | 2.7x        |
| 100MB | ~5.2s       | ~1.4s               | 3.7x        |
| 500MB | ~28s        | ~6.5s               | 4.3x        |

### Memory Usage

| Configuration    | Cache Size | Write Buffers | Total Heap |
|-----------------|------------|---------------|------------|
| Default         | 2GB        | 512MB         | ~2.5GB     |
| High Throughput | 4GB        | 1GB           | ~5.2GB     |
| Conservative    | 512MB      | 128MB         | ~650MB     |

## Continuous Benchmarking

To track performance regressions:

```bash
# Run and save baseline
go test -bench=. -benchmem ./... | tee benchmarks/baseline.txt

# Compare against baseline
go test -bench=. -benchmem ./... | tee benchmarks/current.txt
benchstat benchmarks/baseline.txt benchmarks/current.txt
```

Install benchstat:
```bash
go install golang.org/x/perf/cmd/benchstat@latest
```

## CI/CD Integration

Benchmarks are automatically run in CI for:
- Pull requests (short mode)
- Release branches (full suite)
- Nightly builds (comprehensive + profiling)

See `.github/workflows/benchmarks.yml` for configuration.

## Troubleshooting

### Benchmark Skipped

If you see "Skipping benchmark: set OBJECTFS_BENCH_BUCKET environment variable":

1. Set required environment variables
2. Ensure AWS credentials are configured
3. Verify S3 bucket exists and is accessible

### Inconsistent Results

For consistent results:

1. Run on a quiet system (minimal background processes)
2. Use a dedicated benchmark machine
3. Run multiple times and compare results
4. Use fixed CPU frequency (disable turbo boost)

### Out of Memory

For large benchmarks (500MB+):

1. Increase available memory
2. Run in short mode: `go test -bench=. -short`
3. Reduce concurrency levels
4. Run benchmarks sequentially, not in parallel

## Contributing

When adding new benchmarks:

1. Follow naming convention: `Benchmark<Feature>_<Variant>`
2. Use `testing.B.SetBytes()` for throughput measurements
3. Include setup/cleanup in `b.ResetTimer()` / `b.StopTimer()`
4. Add documentation to this README
5. Update expected baseline results

## Resources

- [Go Benchmarking Guide](https://golang.org/pkg/testing/#hdr-Benchmarks)
- [AWS S3 Transfer Acceleration](https://docs.aws.amazon.com/AmazonS3/latest/userguide/transfer-acceleration.html)
- [ObjectFS v0.4.0 Release Notes](../RELEASE_NOTES_v0.4.0.md)
