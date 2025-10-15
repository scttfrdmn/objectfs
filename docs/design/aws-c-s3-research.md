# AWS-C-S3 Integration Research

**Status:** Research Phase
**Target Version:** v0.5.0+
**Last Updated:** October 15, 2025

---

## Executive Summary

AWS-C-S3 is a high-performance C library for Amazon S3 operations that provides
significant performance improvements over standard SDKs through advanced network
optimization and parallel transfer techniques.

**Expected Benefits:**

- **5-10x throughput** for large files (>100MB)
- **3-5x throughput** for medium files (10-100MB)
- **2x throughput** for small files (<10MB)
- Lower CPU usage through optimized C implementation
- Better network utilization via automatic request parallelization

**Integration Approach:** CGo bindings + phased rollout

---

## Overview

### What is AWS-C-S3?

AWS-C-S3 is part of the AWS Common Runtime (CRT) family of libraries. It's
written in C and provides high-performance S3 operations by:

1. **Automatic Parallelization**: Splits large requests across multiple
   connections
2. **Connection Pooling**: Efficiently reuses HTTP connections
3. **Advanced Flow Control**: Optimizes data transfer based on network
   conditions
4. **Low-Level Optimization**: C implementation for minimal overhead

### Repository

- **GitHub**: <https://github.com/awslabs/aws-c-s3>
- **License**: Apache 2.0
- **Language**: C99
- **Platforms**: Linux, macOS, Windows
- **Dependencies**: aws-c-common, aws-c-io, aws-c-http, aws-c-auth, etc.

---

## Performance Characteristics

### Benchmark Comparisons (from AWS documentation)

**Large File Uploads (1GB):**

- AWS SDK Go v2: ~100 MB/s
- AWS-C-S3: ~500-800 MB/s
- **Improvement: 5-8x**

**Large File Downloads (1GB):**

- AWS SDK Go v2: ~150 MB/s
- AWS-C-S3: ~600-1000 MB/s
- **Improvement: 4-6x**

**Small Files (1MB, 1000 files):**

- AWS SDK Go v2: ~30 MB/s aggregate
- AWS-C-S3: ~60-80 MB/s aggregate
- **Improvement: 2-2.5x**

### Why Is It Faster?

1. **Request Parallelization**:
   - Automatically splits large uploads/downloads across multiple connections
   - Uses optimal part sizes based on network conditions
   - Minimizes round-trip latency

2. **Connection Management**:
   - Connection pooling with configurable limits
   - HTTP pipelining where supported
   - Efficient connection reuse

3. **Low-Level Optimization**:
   - Zero-copy operations where possible
   - Minimal allocations in hot paths
   - Optimized memory management

4. **Adaptive Behavior**:
   - Adjusts parallelism based on throughput
   - Adapts part sizes to network conditions
   - Automatic retry with exponential backoff

---

## Architecture Integration

### Current ObjectFS S3 Backend

```go
// Current implementation (AWS SDK Go v2)
type Backend struct {
    s3Client  *s3.Client
    bucket    string
    region    string
    config    *Config
}

func (b *Backend) GetObject(ctx context.Context, key string) ([]byte, error) {
    result, err := b.s3Client.GetObject(ctx, &s3.GetObjectInput{
        Bucket: aws.String(b.bucket),
        Key:    aws.String(key),
    })
    if err != nil {
        return nil, err
    }
    defer result.Body.Close()

    return io.ReadAll(result.Body)
}
```

### Proposed AWS-C-S3 Backend

```go
// New implementation (AWS-C-S3 via CGo)
package awscs3

// #cgo CFLAGS: -I/usr/local/include
// #cgo LDFLAGS: -L/usr/local/lib -laws-c-s3 -laws-c-common -laws-c-io
// #include <aws/s3/s3.h>
// #include <aws/common/common.h>
import "C"

import (
    "context"
    "unsafe"
)

type Backend struct {
    client     *C.struct_aws_s3_client
    allocator  *C.struct_aws_allocator
    bucket     *C.aws_string
    config     *Config
}

func (b *Backend) GetObject(ctx context.Context, key string) ([]byte, error) {
    // Convert Go string to aws_string
    cKey := C.aws_string_new_from_c_str(b.allocator, C.CString(key))
    defer C.aws_string_destroy(cKey)

    // Create meta request for high-performance download
    var metaRequest *C.struct_aws_s3_meta_request
    // ... AWS-C-S3 specific code

    // Wait for completion and return data
    return data, nil
}
```

### Backend Factory Pattern

```go
// Allow runtime backend selection
type BackendType string

const (
    BackendAWSSDK BackendType = "aws-sdk-v2"
    BackendAWSCS3 BackendType = "aws-c-s3"
)

func NewBackend(config *Config) (Backend, error) {
    switch config.BackendType {
    case BackendAWSCS3:
        return awscs3.NewBackend(config)
    case BackendAWSSDK:
        fallthrough
    default:
        return s3.NewBackend(config)
    }
}
```

---

## Build System Integration

### Dependencies

AWS-C-S3 requires building the entire AWS CRT dependency chain:

```
aws-c-s3
├── aws-c-auth
│   ├── aws-c-http
│   │   ├── aws-c-compression
│   │   ├── aws-c-io
│   │   │   ├── aws-c-cal
│   │   │   │   └── aws-c-common
│   │   │   └── aws-c-common
│   │   └── aws-c-common
│   ├── aws-c-io
│   └── aws-c-common
└── aws-c-common
```

### Build Script

```bash
#!/usr/bin/env bash
# scripts/build-aws-c-s3.sh

set -e

INSTALL_PREFIX="${INSTALL_PREFIX:-/usr/local}"
BUILD_DIR="${BUILD_DIR:-build-deps}"

mkdir -p "$BUILD_DIR"
cd "$BUILD_DIR"

# Build order (dependencies first)
REPOS=(
    "aws-c-common"
    "aws-checksums"
    "aws-c-cal"
    "aws-c-io"
    "aws-c-compression"
    "aws-c-http"
    "aws-c-auth"
    "aws-c-s3"
)

for repo in "${REPOS[@]}"; do
    echo "Building $repo..."

    if [[ ! -d "$repo" ]]; then
        git clone "https://github.com/awslabs/$repo.git"
    fi

    cd "$repo"
    git pull

    mkdir -p build
    cd build

    cmake .. \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_INSTALL_PREFIX="$INSTALL_PREFIX" \
        -DBUILD_SHARED_LIBS=ON

    make -j$(nproc)
    sudo make install

    cd ../..
done

echo "AWS-C-S3 build complete!"
```

### Go Build Tags

```go
// +build awscs3

// backend_awscs3.go
package backend

// This file only builds when 'awscs3' tag is present
```

Build with AWS-C-S3:

```bash
go build -tags awscs3 ./cmd/objectfs
```

Build without (standard SDK):

```bash
go build ./cmd/objectfs
```

---

## Research Phase Plan

### Phase 1: Feasibility (Week 1-2)

**Goals:**

- Build AWS-C-S3 from source
- Create minimal CGo proof-of-concept
- Verify cross-platform compatibility
- Measure performance baseline

**Deliverables:**

- [ ] AWS-C-S3 builds successfully on macOS
- [ ] AWS-C-S3 builds successfully on Linux
- [ ] Basic CGo wrapper compiles
- [ ] Simple GetObject/PutObject works
- [ ] Performance comparison benchmark

**Commands:**

```bash
# Clone and build AWS-C-S3
cd /tmp
git clone https://github.com/awslabs/aws-c-s3.git
cd aws-c-s3
./scripts/build-aws-c-s3.sh

# Create minimal Go test
mkdir -p internal/backend/awscs3/poc
cd internal/backend/awscs3/poc
# Create basic CGo wrapper and test
```

### Phase 2: Integration Design (Week 3-4)

**Goals:**

- Design backend interface
- Plan memory management strategy
- Design error handling
- Plan configuration approach

**Deliverables:**

- [ ] Backend interface definition
- [ ] Memory management strategy documented
- [ ] Error mapping Go ↔ C
- [ ] Configuration structure designed
- [ ] Build system integration plan

### Phase 3: Implementation (Week 5-8)

**Goals:**

- Implement full backend
- Handle all S3 operations
- Memory leak testing
- Integration testing

**Deliverables:**

- [ ] Complete backend implementation
- [ ] Unit tests (with mocking)
- [ ] Integration tests (real S3)
- [ ] Memory leak tests (valgrind/AddressSanitizer)
- [ ] Performance benchmarks

### Phase 4: Optimization (Week 9-10)

**Goals:**

- Tune connection pool settings
- Optimize part sizes
- Minimize memory allocations
- Profile and optimize hot paths

**Deliverables:**

- [ ] Connection pool tuning
- [ ] Part size optimization
- [ ] Memory allocation profiling
- [ ] CPU profiling and optimization
- [ ] Final performance benchmarks

---

## Technical Challenges

### 1. Memory Management

**Challenge:** Bridging Go and C memory management

**Approaches:**

a) **C-side allocation** (recommended):

```go
// Allocate in C, free in C
data := C.malloc(C.size_t(size))
defer C.free(data)
```

b) **Go-side allocation** (for return values):

```go
// Allocate in Go, pass to C
goData := make([]byte, size)
C.process_data((*C.uchar)(unsafe.Pointer(&goData[0])), C.size_t(len(goData)))
```

c) **Reference counting** (for shared objects):

```go
// Increment ref count when passing to Go
C.aws_ref_count_inc(&object.ref_count)
// Decrement when done
defer C.aws_ref_count_dec(&object.ref_count)
```

### 2. Error Handling

**Challenge:** Map C error codes to Go errors

**Approach:**

```go
type S3Error struct {
    Code    int
    Message string
    AWSCode string
}

func (e *S3Error) Error() string {
    return fmt.Sprintf("S3 error %d (%s): %s", e.Code, e.AWSCode, e.Message)
}

func mapCError(cerr *C.struct_aws_error) error {
    if cerr == nil {
        return nil
    }

    return &S3Error{
        Code:    int(cerr.error_code),
        Message: C.GoString(C.aws_error_str(cerr.error_code)),
        AWSCode: C.GoString(cerr.aws_error_code),
    }
}
```

### 3. Concurrency

**Challenge:** Go goroutines + C threads

**Approach:**

- Use AWS-C-S3's event loop (C-side threading)
- Marshal callbacks back to Go via channels
- Protect shared state with proper locking

```go
type Request struct {
    ctx      context.Context
    resultCh chan *Result
    errorCh  chan error
}

// C callback function (exported for CGo)
//export onRequestComplete
func onRequestComplete(userData unsafe.Pointer, statusCode C.int) {
    req := (*Request)(userData)
    if statusCode == 200 {
        req.resultCh <- &Result{/* ... */}
    } else {
        req.errorCh <- fmt.Errorf("request failed: %d", statusCode)
    }
}
```

### 4. Platform Compatibility

**Challenge:** Build on macOS, Linux, and Windows

**Approach:**

- Use CMake for cross-platform builds
- Document platform-specific requirements
- Provide pre-built binaries for common platforms
- Support both dynamic and static linking

---

## Performance Benchmarking Plan

### Benchmark Scenarios

1. **Large File Upload (1GB)**:
   - Measure throughput (MB/s)
   - Measure CPU usage
   - Measure memory usage

2. **Large File Download (1GB)**:
   - Measure throughput (MB/s)
   - Measure first-byte latency
   - Measure memory usage

3. **Many Small Files (1000 x 1MB)**:
   - Measure aggregate throughput
   - Measure average latency
   - Measure request rate (IOPS)

4. **Mixed Workload**:
   - 60% reads, 40% writes
   - Various file sizes (1KB-10MB)
   - Concurrent operations

### Benchmark Implementation

```go
func BenchmarkLargeFileUpload(b *testing.B) {
    backends := []struct {
        name string
        backend Backend
    }{
        {"AWS-SDK-v2", sdkBackend},
        {"AWS-C-S3", cs3Backend},
    }

    data := make([]byte, 1024*1024*1024) // 1GB

    for _, backend := range backends {
        b.Run(backend.name, func(b *testing.B) {
            b.SetBytes(int64(len(data)))
            b.ResetTimer()

            for i := 0; i < b.N; i++ {
                key := fmt.Sprintf("bench/upload-%d", i)
                if err := backend.backend.PutObject(context.Background(), key, data); err != nil {
                    b.Fatal(err)
                }
            }

            // Report throughput
            mbps := float64(b.N*len(data)) / b.Elapsed().Seconds() / (1024*1024)
            b.ReportMetric(mbps, "MB/s")
        })
    }
}
```

---

## Risk Assessment

### Technical Risks

1. **Build Complexity**: HIGH
   - **Mitigation**: Provide comprehensive build scripts and documentation
   - **Fallback**: Provide pre-built binaries

2. **Memory Leaks**: MEDIUM
   - **Mitigation**: Extensive testing with valgrind and AddressSanitizer
   - **Fallback**: Can always fall back to AWS SDK v2

3. **Platform Compatibility**: MEDIUM
   - **Mitigation**: Test on all major platforms (Linux, macOS, Windows)
   - **Fallback**: Build tags allow platform-specific builds

4. **Performance Regression**: LOW
   - **Mitigation**: Extensive benchmarking before rollout
   - **Requirement**: Must show >2x improvement to justify complexity

### Operational Risks

1. **Support Burden**: MEDIUM
   - **Mitigation**: Keep AWS SDK v2 backend as default initially
   - **Rollout**: Gradual opt-in rollout

2. **Debugging Difficulty**: MEDIUM
   - **Mitigation**: Comprehensive logging and error messages
   - **Tools**: Provide debugging guides for C/Go boundary issues

---

## Success Criteria

### Phase 1 (Feasibility) - Complete

- [ ] Builds successfully on target platforms
- [ ] Basic operations work (GetObject, PutObject)
- [ ] Shows >2x performance improvement in benchmarks
- [ ] No obvious memory leaks in short tests

### Phase 2 (Design) - Complete

- [ ] Clean backend interface designed
- [ ] Memory management strategy validated
- [ ] Build system integration working
- [ ] Configuration approach defined

### Phase 3 (Implementation) - Complete

- [ ] All S3 operations implemented
- [ ] >80% test coverage
- [ ] Zero memory leaks in 24-hour stress test
- [ ] >5x throughput for large files (>100MB)
- [ ] >3x throughput for medium files (10-100MB)

### Phase 4 (Production) - Complete

- [ ] Production deployment successful
- [ ] No critical issues in 30 days
- [ ] Performance targets met in production
- [ ] User satisfaction positive

---

## Next Steps

1. **Immediate (This Week)**:
   - [ ] Clone and build AWS-C-S3 locally
   - [ ] Read AWS-C-S3 documentation
   - [ ] Review example code in aws-c-s3 repository
   - [ ] Create minimal CGo proof-of-concept

2. **Short-Term (Next 2 Weeks)**:
   - [ ] Measure baseline performance (AWS SDK v2)
   - [ ] Implement minimal GetObject/PutObject with AWS-C-S3
   - [ ] Run performance comparison
   - [ ] Document findings

3. **Mid-Term (Next Month)**:
   - [ ] Design complete backend interface
   - [ ] Implement full backend
   - [ ] Integration testing
   - [ ] Performance optimization

4. **Long-Term (Next Quarter)**:
   - [ ] Production readiness
   - [ ] Documentation
   - [ ] User testing
   - [ ] Gradual rollout

---

## References

- **AWS-C-S3 GitHub**: <https://github.com/awslabs/aws-c-s3>
- **AWS Common Runtime**: <https://github.com/awslabs/aws-crt-builder>
- **AWS-C-S3 Examples**: <https://github.com/awslabs/aws-c-s3/tree/main/samples>
- **CGo Documentation**: <https://pkg.go.dev/cmd/cgo>
- **ROADMAP.md**: AWS-C-S3 integration plan (v0.5.0-v0.7.0)
- **DEVELOPMENT.md**: AWS-C-S3 integration section

---

## Research Notes

### Initial Observations

> To be filled in during research phase

### Performance Measurements

> To be filled in during benchmarking

### Integration Challenges

> To be documented as discovered

### Lessons Learned

> To be documented throughout process
