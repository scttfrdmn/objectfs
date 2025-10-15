# ObjectFS Development Guidelines

**Version:** 1.0
**Last Updated:** October 15, 2025

---

## Table of Contents

1. [Branching Strategy](#branching-strategy)
2. [Testing Strategy](#testing-strategy)
3. [Feature Development Workflow](#feature-development-workflow)
4. [CI/CD Pipeline](#cicd-pipeline)
5. [Code Quality Standards](#code-quality-standards)
6. [Performance Optimization](#performance-optimization)
7. [AWS-C-S3 Integration Plan](#aws-c-s3-integration-plan)

---

## Branching Strategy

### Branch Hierarchy

```
main (protected)
  ├── develop (integration branch)
  │   ├── feature/v0.4.0-transfer-acceleration
  │   ├── feature/v0.4.0-multipart-optimization
  │   ├── feature/v0.4.0-cargoship-shared-components
  │   ├── feature/v0.5.0-bbr-optimization
  │   ├── feature/v0.5.0-distributed-cache
  │   └── feature/v0.6.0-multi-protocol-arch
  ├── release/v0.4.0 (release candidate)
  ├── release/v0.5.0
  └── hotfix/critical-bug-name
```

### Branch Types and Policies

#### `main` Branch

- **Purpose:** Production-ready code only
- **Protection Rules:**
  - Requires PR approval (2+ reviewers)
  - All CI checks must pass
  - No direct commits allowed
  - Signed commits required
  - Linear history (squash or rebase merge only)

#### `develop` Branch

- **Purpose:** Integration branch for ongoing development
- **Protection Rules:**
  - Requires PR approval (1+ reviewer)
  - All CI checks must pass
  - Feature branches merge here first
  - Regular sync with main

#### `feature/*` Branches

- **Naming Convention:** `feature/vX.Y.Z-descriptive-name`
  - Examples:
    - `feature/v0.4.0-s3-transfer-acceleration`
    - `feature/v0.5.0-redis-cache-backend`
    - `feature/v0.6.0-filesystem-interface-abstraction`

- **Lifecycle:**
  1. Branch from `develop`
  2. Develop with frequent commits
  3. Keep synced with `develop` (rebase regularly)
  4. PR to `develop` when ready
  5. Delete after merge

- **Requirements:**
  - Comprehensive unit tests
  - Integration tests (real AWS when needed)
  - Documentation updates
  - Performance benchmarks
  - No performance regression

#### `release/*` Branches

- **Naming Convention:** `release/vX.Y.Z`
- **Purpose:** Release candidate preparation
- **Process:**
  1. Branch from `develop` when feature complete
  2. Bug fixes and polish only
  3. Update CHANGELOG.md
  4. Update version numbers
  5. Final testing and validation
  6. Merge to both `main` and `develop`
  7. Tag release on `main`

#### `hotfix/*` Branches

- **Naming Convention:** `hotfix/critical-bug-description`
- **Purpose:** Critical production bug fixes
- **Process:**
  1. Branch from `main`
  2. Fix with minimal changes
  3. Add regression test
  4. Merge to both `main` and `develop`
  5. Tag patch release

### Branch Naming Examples

**Good:**

- `feature/v0.4.0-aws-c-s3-integration`
- `feature/v0.5.0-zstd-compression`
- `feature/v0.6.0-smb-protocol-handler`
- `hotfix/cache-memory-leak`
- `release/v0.4.0`

**Bad:**

- `feature/new-stuff` (too vague)
- `john-working-branch` (no context)
- `test` (not descriptive)
- `fix-bug` (which bug?)

---

## Testing Strategy

### No More LocalStack - Real AWS Testing

**Philosophy:** LocalStack has proven unreliable in CI. We're moving to a multi-tier testing approach that uses real AWS services for integration testing.

### Testing Tiers

#### Tier 1: Unit Tests (No External Dependencies)

**Run on:** Every commit, all branches
**Tools:** Go testing framework, mock interfaces
**Coverage Target:** 80%+ per package

```go
// Example: Test cache logic without S3
func TestCacheGetPut(t *testing.T) {
    cache := NewMemoryCache(1024 * 1024)

    key := "test-key"
    data := []byte("test data")

    cache.Put(key, 0, data)
    result := cache.Get(key, 0, int64(len(data)))

    assert.Equal(t, data, result)
}
```

**When to use:**

- Algorithm testing
- Data structure validation
- Business logic verification
- Edge case handling
- Error condition testing

#### Tier 2: Integration Tests with Mock Backend

**Run on:** Every PR, feature branches
**Tools:** In-memory mock backends
**Coverage Target:** Core workflows

```go
// Example: Test FUSE operations with mock S3
func TestFUSEReadWrite(t *testing.T) {
    mockBackend := NewMockS3Backend()
    fs := NewFileSystem(mockBackend, cacheConfig, bufferConfig)

    // Test read/write operations
    err := fs.Write("test.txt", []byte("content"))
    require.NoError(t, err)

    data, err := fs.Read("test.txt")
    require.NoError(t, err)
    assert.Equal(t, "content", string(data))
}
```

**When to use:**

- Component integration
- Workflow testing
- Performance benchmarking
- Concurrency testing

#### Tier 3: Real AWS Integration Tests

**Run on:** PR approval, pre-release, nightly builds
**Tools:** Real S3 buckets, test AWS account
**Coverage Target:** Critical paths and features

```go
// Example: Real S3 integration test
func TestRealS3Operations(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping real AWS test in short mode")
    }

    // Check for AWS credentials
    if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
        t.Skip("No AWS credentials - skipping real S3 test")
    }

    // Use dedicated test bucket
    bucket := os.Getenv("OBJECTFS_TEST_BUCKET") // e.g., "objectfs-ci-test"
    testPrefix := fmt.Sprintf("test-run-%d/", time.Now().Unix())

    backend := NewS3Backend(bucket, "us-west-2")
    defer cleanupTestData(backend, testPrefix)

    // Test real S3 operations
    testKey := testPrefix + "test-file.txt"
    testData := []byte("real S3 test data")

    err := backend.PutObject(context.Background(), testKey, testData)
    require.NoError(t, err)

    retrieved, err := backend.GetObject(context.Background(), testKey, 0, 0)
    require.NoError(t, err)
    assert.Equal(t, testData, retrieved)
}
```

**Test Environment Setup:**

```bash
# Required environment variables for real AWS tests
export AWS_REGION=us-west-2
export AWS_ACCESS_KEY_ID=<test-account-key>
export AWS_SECRET_ACCESS_KEY=<test-account-secret>
export OBJECTFS_TEST_BUCKET=objectfs-ci-test-$(whoami)

# Run tests with real AWS
go test -v -tags=integration ./...

# Run quick tests only (skip real AWS)
go test -v -short ./...
```

**AWS Test Infrastructure:**

- Dedicated S3 bucket: `objectfs-ci-test-*`
- Lifecycle policy: Auto-delete objects >7 days old
- Budget alerts: Alert if costs >$50/month
- IAM role: Restricted permissions (S3 only, specific bucket)

#### Tier 4: Performance & Stress Tests

**Run on:** Release candidates, manual trigger
**Tools:** Custom benchmarking suite, real AWS
**Coverage Target:** Performance regression detection

```go
// Example: Performance benchmark
func BenchmarkS3LargeFileRead(b *testing.B) {
    if testing.Short() {
        b.Skip("Skipping benchmark in short mode")
    }

    backend := setupRealS3Backend(b)
    testFile := "benchmark/1gb-file.dat"

    b.ResetTimer()
    b.SetBytes(1024 * 1024 * 1024) // 1GB

    for i := 0; i < b.N; i++ {
        data, err := backend.GetObject(context.Background(), testFile, 0, 0)
        if err != nil {
            b.Fatal(err)
        }
        if len(data) != 1024*1024*1024 {
            b.Fatal("Incorrect data size")
        }
    }

    // Report throughput
    mbps := float64(b.N*1024) / b.Elapsed().Seconds()
    b.ReportMetric(mbps, "MB/s")
}
```

### Test Organization

```
tests/
├── unit/                    # Tier 1: Pure unit tests
│   ├── cache_test.go
│   ├── buffer_test.go
│   └── metrics_test.go
├── integration/             # Tier 2: Mock integration tests
│   ├── fuse_test.go
│   ├── backend_mock_test.go
│   └── e2e_mock_test.go
├── aws/                     # Tier 3: Real AWS tests
│   ├── s3_real_test.go
│   ├── performance_test.go
│   └── stress_test.go
└── benchmarks/              # Tier 4: Performance benchmarks
    ├── throughput_test.go
    ├── latency_test.go
    └── concurrency_test.go
```

### Running Tests

```bash
# Quick validation (Tier 1 + 2)
make test-quick

# Full test suite including real AWS (Tier 1-3)
make test-full

# Performance benchmarks (Tier 4)
make test-benchmark

# Specific test with verbose output
go test -v -run TestSpecificFunction ./internal/cache

# Test with race detector
go test -race ./...

# Test with coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### CI Testing Matrix

**On every commit to feature branch:**

- Tier 1: Unit tests
- Tier 2: Integration tests (mock)
- Code quality checks (lint, format)
- Race detector

**On PR to develop:**

- All Tier 1 + 2 tests
- Tier 3: Real AWS tests (subset)
- Performance benchmarks (comparison)
- Documentation validation

**On PR to main (release):**

- All test tiers (1-4)
- Extended real AWS testing
- Full performance validation
- Security scanning
- Multi-platform build verification

**Nightly builds:**

- Full Tier 3 testing
- Extended stress tests
- Memory leak detection
- Long-running stability tests

---

## Feature Development Workflow

### Phase-Based Development Process

Each feature follows a structured development process with clear checkpoints.

### Phase 1: Design & Planning

**Duration:** 1-2 weeks
**Branch:** Design documented in issue/PR description

**Activities:**

1. Create GitHub issue with feature proposal
2. Design document in `docs/design/feature-name.md`
3. Architecture review with maintainers
4. Interface definitions and API design
5. Test plan creation
6. Performance baseline establishment

**Deliverables:**

- [ ] Design document approved
- [ ] Test plan defined
- [ ] Performance targets set
- [ ] Dependencies identified

**Example Design Document Structure:**

```markdown
# Feature: S3 Transfer Acceleration Support

## Objective
Enable S3 Transfer Acceleration for improved upload/download performance.

## Design
### Architecture Changes
- Add acceleration endpoint configuration
- Implement endpoint fallback logic
- Add acceleration metrics

### Interface Changes
```go
type S3Config struct {
    // ...existing fields
    TransferAcceleration bool
    AccelerationEndpoint string
}
```

### Testing Approach

- Unit tests for endpoint selection logic
- Real S3 tests with acceleration enabled
- Performance comparison benchmarks

### Performance Targets

- 2x+ improvement for cross-region transfers
- <5% overhead for same-region transfers
- Graceful degradation if acceleration unavailable

### Rollout Plan

1. Implement with feature flag (disabled by default)
2. Test with subset of users
3. Enable by default in v0.4.1

```

### Phase 2: Implementation
**Duration:** 2-6 weeks (varies by feature)
**Branch:** `feature/vX.Y.Z-feature-name`

**Development Checklist:**
- [ ] Create feature branch from `develop`
- [ ] Implement core functionality
- [ ] Write unit tests (Tier 1)
- [ ] Write integration tests (Tier 2)
- [ ] Update documentation
- [ ] Add performance benchmarks
- [ ] Test with real AWS (Tier 3)
- [ ] Code review with maintainers

**Daily Development Workflow:**
```bash
# Start of day: Sync with develop
git checkout develop
git pull origin develop
git checkout feature/v0.4.0-my-feature
git rebase develop

# During development: Commit frequently
git add -p  # Stage changes interactively
git commit -m "feat: implement X component"

# Run tests before pushing
make test-quick
make lint

# Push to remote
git push origin feature/v0.4.0-my-feature

# End of day/week: Sync again
git fetch origin develop
git rebase origin/develop
```

**Commit Message Convention:**

```
<type>(<scope>): <subject>

<body>

<footer>
```

**Types:**

- `feat`: New feature
- `fix`: Bug fix
- `perf`: Performance improvement
- `refactor`: Code refactoring
- `test`: Test additions/changes
- `docs`: Documentation changes
- `ci`: CI/CD changes
- `chore`: Maintenance tasks

**Examples:**

```
feat(s3): add transfer acceleration support

Implement S3 Transfer Acceleration with automatic fallback to standard
endpoints if acceleration is unavailable.

- Add TransferAcceleration config option
- Implement endpoint selection logic
- Add acceleration metrics
- Include performance benchmarks

Closes #123
```

### Phase 3: Testing & Validation

**Duration:** 1-2 weeks
**Branch:** Same feature branch

**Testing Checklist:**

- [ ] All unit tests passing (80%+ coverage)
- [ ] All integration tests passing
- [ ] Real AWS tests passing
- [ ] Performance benchmarks meet targets
- [ ] No performance regression (<5% overhead)
- [ ] Memory leak testing (run for 24+ hours)
- [ ] Race detector clean
- [ ] Cross-platform build verification

**Performance Validation:**

```bash
# Baseline benchmark (develop branch)
git checkout develop
go test -bench=BenchmarkFeature -benchmem -count=10 > baseline.txt

# Feature benchmark
git checkout feature/v0.4.0-my-feature
go test -bench=BenchmarkFeature -benchmem -count=10 > feature.txt

# Compare results
benchstat baseline.txt feature.txt
```

**Expected Output:**

```
name                old time/op    new time/op    delta
LargeFileUpload-8     1.50s ± 2%     0.75s ± 1%  -50.00%  (p=0.000 n=10+10)

name                old alloc/op   new alloc/op   delta
LargeFileUpload-8     500MB ± 0%     250MB ± 0%  -50.00%  (p=0.000 n=10+10)

name                old allocs/op  new allocs/op  delta
LargeFileUpload-8      1.5k ± 0%      1.5k ± 0%     ~     (all equal)
```

### Phase 4: Code Review & Iteration

**Duration:** 3-7 days
**Branch:** Same feature branch

**PR Checklist:**

```markdown
## Description
Brief description of the feature and its motivation.

## Changes
- List key changes
- Highlight breaking changes (if any)
- Note deprecated features

## Testing
- [ ] Unit tests added/updated (coverage: X%)
- [ ] Integration tests added/updated
- [ ] Real AWS tests passing
- [ ] Performance benchmarks included
- [ ] Manual testing completed

## Performance Impact
- Throughput: X% improvement/regression
- Latency: X ms average
- Memory: X MB average usage
- No performance regression verified

## Documentation
- [ ] Code comments updated
- [ ] README updated (if needed)
- [ ] CHANGELOG.md updated
- [ ] API documentation updated

## Checklist
- [ ] Code follows project style guidelines
- [ ] All tests passing
- [ ] No linting errors
- [ ] Commits are signed
- [ ] Branch is up-to-date with develop
```

**Review Process:**

1. Self-review: Review your own PR first
2. Address automated checks (CI, linting)
3. Request reviews from 1-2 maintainers
4. Address feedback promptly
5. Update tests/docs as needed
6. Get approval from required reviewers
7. Squash/rebase for clean history
8. Merge to develop

### Phase 5: Integration & Release

**Duration:** Varies by release cycle
**Branch:** `develop` → `release/vX.Y.Z` → `main`

**Release Process:**

1. Feature merged to `develop`
2. Integration testing on `develop`
3. Create release branch when ready
4. Final testing and bug fixes
5. Update version and CHANGELOG
6. Merge to `main` and tag release
7. Publish release notes
8. Monitor for issues

---

## CI/CD Pipeline

### GitHub Actions Workflow

#### Workflow: Feature Branch CI

**Trigger:** Push to `feature/*`
**File:** `.github/workflows/feature-ci.yml`

```yaml
name: Feature Branch CI

on:
  push:
    branches:
      - 'feature/**'
  pull_request:
    branches:
      - develop

jobs:
  test-quick:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        go-version: [1.22.x, 1.23.x, 1.24.x]

    steps:
    - uses: actions/checkout@v4

    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: ${{ matrix.go-version }}
        cache: true

    - name: Cache Go modules
      uses: actions/cache@v4
      with:
        path: |
          ~/.cache/go-build
          ~/go/pkg/mod
        key: ${{ runner.os }}-go-${{ matrix.go-version }}-${{ hashFiles('**/go.sum') }}

    - name: Install dependencies
      run: go mod download

    - name: Run unit tests
      run: go test -race -short -coverprofile=coverage.out -covermode=atomic ./...

    - name: Upload coverage
      uses: codecov/codecov-action@v4
      with:
        file: ./coverage.out
        flags: unittests-${{ matrix.go-version }}

  lint:
    runs-on: ubuntu-latest
    steps:
    - uses: actions/checkout@v4

    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: 1.23.x

    - name: Run golangci-lint
      uses: golangci/golangci-lint-action@v4
      with:
        version: latest
        args: --timeout=10m

  security:
    runs-on: ubuntu-latest
    steps:
    - uses: actions/checkout@v4

    - name: Run gosec
      run: |
        go install github.com/securego/gosec/v2/cmd/gosec@latest
        gosec -fmt sarif -out gosec.sarif -no-fail ./...

    - name: Upload SARIF
      uses: github/codeql-action/upload-sarif@v3
      if: always()
      continue-on-error: true
      with:
        sarif_file: gosec.sarif
```

#### Workflow: PR to Develop

**Trigger:** PR to `develop`
**File:** `.github/workflows/pr-develop.yml`

```yaml
name: PR to Develop

on:
  pull_request:
    branches:
      - develop

jobs:
  # Include all jobs from feature-ci.yml

  test-real-aws:
    runs-on: ubuntu-latest
    if: github.event.pull_request.draft == false

    steps:
    - uses: actions/checkout@v4

    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: 1.23.x

    - name: Configure AWS credentials
      uses: aws-actions/configure-aws-credentials@v4
      with:
        aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
        aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
        aws-region: us-west-2

    - name: Run real AWS integration tests
      env:
        OBJECTFS_TEST_BUCKET: objectfs-ci-test-${{ github.run_id }}
      run: |
        # Create test bucket
        aws s3 mb s3://$OBJECTFS_TEST_BUCKET

        # Run tests
        go test -v -tags=integration ./tests/aws/...

        # Cleanup
        aws s3 rb s3://$OBJECTFS_TEST_BUCKET --force

  benchmark:
    runs-on: ubuntu-latest
    steps:
    - uses: actions/checkout@v4
      with:
        fetch-depth: 0  # Need full history for comparison

    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: 1.23.x

    - name: Run benchmarks on base branch
      run: |
        git checkout ${{ github.base_ref }}
        go test -bench=. -benchmem -count=5 ./... > base-bench.txt

    - name: Run benchmarks on PR branch
      run: |
        git checkout ${{ github.head_ref }}
        go test -bench=. -benchmem -count=5 ./... > pr-bench.txt

    - name: Compare benchmarks
      run: |
        go install golang.org/x/perf/cmd/benchstat@latest
        benchstat base-bench.txt pr-bench.txt | tee benchmark-comparison.txt

    - name: Comment PR with results
      uses: actions/github-script@v7
      with:
        script: |
          const fs = require('fs');
          const results = fs.readFileSync('benchmark-comparison.txt', 'utf8');
          github.rest.issues.createComment({
            issue_number: context.issue.number,
            owner: context.repo.owner,
            repo: context.repo.repo,
            body: `## Performance Benchmark Results\n\n\`\`\`\n${results}\n\`\`\`\n`
          });
```

#### Workflow: Release

**Trigger:** PR to `main`
**File:** `.github/workflows/release.yml`

```yaml
name: Release

on:
  pull_request:
    branches:
      - main
  push:
    tags:
      - 'v*'

jobs:
  # All previous tests plus:

  test-full:
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest, windows-latest]
        go-version: [1.22.x, 1.23.x, 1.24.x]

    steps:
    - uses: actions/checkout@v4

    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: ${{ matrix.go-version }}

    - name: Run full test suite
      run: go test -v -race ./...

  stress-test:
    runs-on: ubuntu-latest
    timeout-minutes: 60

    steps:
    - uses: actions/checkout@v4

    - name: Run stress tests
      run: |
        go test -v -timeout=60m -tags=stress ./tests/stress/...

  build-release:
    runs-on: ubuntu-latest
    if: startsWith(github.ref, 'refs/tags/')
    needs: [test-full, stress-test]

    steps:
    - uses: actions/checkout@v4

    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: 1.23.x

    - name: Build binaries
      run: |
        GOOS=linux GOARCH=amd64 go build -o objectfs-linux-amd64 ./cmd/objectfs
        GOOS=darwin GOARCH=amd64 go build -o objectfs-darwin-amd64 ./cmd/objectfs
        GOOS=darwin GOARCH=arm64 go build -o objectfs-darwin-arm64 ./cmd/objectfs
        GOOS=windows GOARCH=amd64 go build -o objectfs-windows-amd64.exe ./cmd/objectfs

    - name: Create release
      uses: softprops/action-gh-release@v1
      with:
        files: |
          objectfs-*
        generate_release_notes: true
```

#### Workflow: Nightly Tests

**Trigger:** Cron schedule (2 AM UTC)
**File:** `.github/workflows/nightly.yml`

```yaml
name: Nightly Tests

on:
  schedule:
    - cron: '0 2 * * *'  # 2 AM UTC daily
  workflow_dispatch:  # Allow manual trigger

jobs:
  extended-aws-tests:
    runs-on: ubuntu-latest
    timeout-minutes: 180

    steps:
    - uses: actions/checkout@v4

    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: 1.23.x

    - name: Configure AWS credentials
      uses: aws-actions/configure-aws-credentials@v4
      with:
        aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
        aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
        aws-region: us-west-2

    - name: Run extended tests
      env:
        OBJECTFS_TEST_BUCKET: objectfs-nightly-test
      run: |
        go test -v -timeout=3h -tags=nightly ./...

  memory-leak-test:
    runs-on: ubuntu-latest
    timeout-minutes: 240

    steps:
    - uses: actions/checkout@v4

    - name: Run memory leak detection
      run: |
        go test -v -timeout=4h -memprofile=mem.prof ./...
        go tool pprof -top mem.prof

  notify-failures:
    needs: [extended-aws-tests, memory-leak-test]
    if: failure()
    runs-on: ubuntu-latest

    steps:
    - name: Notify team
      uses: actions/github-script@v7
      with:
        script: |
          github.rest.issues.create({
            owner: context.repo.owner,
            repo: context.repo.repo,
            title: '🚨 Nightly Tests Failed',
            body: 'Nightly test suite failed. Please investigate.',
            labels: ['bug', 'nightly-failure']
          });
```

---

## Code Quality Standards

### Go Style Guidelines

**Follow:**

- [Effective Go](https://golang.org/doc/effective_go)
- [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)
- [Uber Go Style Guide](https://github.com/uber-go/guide/blob/master/style.md)

### Key Principles

#### 1. Error Handling

```go
// Good: Wrap errors with context
func readConfig(path string) (*Config, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, fmt.Errorf("read config %s: %w", path, err)
    }
    // ...
    return config, nil
}

// Bad: Swallow or ignore errors
func readConfig(path string) *Config {
    data, _ := os.ReadFile(path)  // BAD: ignored error
    // ...
}
```

#### 2. Interfaces

```go
// Good: Small, focused interfaces
type ObjectGetter interface {
    GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error)
}

// Good: Accept interfaces, return structs
func NewCache(backend ObjectGetter) *Cache {
    return &Cache{backend: backend}
}

// Bad: Large, monolithic interfaces
type Everything interface {
    GetObject(...)
    PutObject(...)
    ListObjects(...)
    DeleteObject(...)
    HealthCheck(...)
    GetMetrics(...)
    // ... 20 more methods
}
```

#### 3. Concurrency

```go
// Good: Use sync primitives correctly
type SafeCounter struct {
    mu    sync.RWMutex
    count int64
}

func (c *SafeCounter) Inc() {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.count++
}

func (c *SafeCounter) Value() int64 {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.count
}

// Good: Use context for cancellation
func (s *Server) Serve(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case req := <-s.requests:
            s.handle(ctx, req)
        }
    }
}
```

#### 4. Performance-Critical Code

```go
// Good: Pre-allocate slices when size is known
func readChunks(size int) []byte {
    chunks := make([]byte, 0, size/chunkSize)  // Pre-allocate capacity
    // ...
    return chunks
}

// Good: Reuse buffers
var bufferPool = sync.Pool{
    New: func() interface{} {
        return make([]byte, 64*1024)
    },
}

func processData(data []byte) {
    buf := bufferPool.Get().([]byte)
    defer bufferPool.Put(buf)
    // Use buf...
}

// Good: Avoid allocations in hot paths
func (c *Cache) Get(key string, offset, size int64) []byte {
    // Use string key directly, don't format
    cacheKey := key + ":" + strconv.FormatInt(offset, 10)  // Allocates!

    // Better: Use bytes.Buffer or strings.Builder
    var b strings.Builder
    b.WriteString(key)
    b.WriteByte(':')
    b.WriteString(strconv.FormatInt(offset, 10))
    cacheKey := b.String()
    // ...
}
```

### Code Review Checklist

**For Reviewers:**

- [ ] Code follows Go style guidelines
- [ ] All functions have doc comments
- [ ] Error handling is correct and thorough
- [ ] Concurrency is safe (proper locking)
- [ ] No obvious performance issues
- [ ] Tests are comprehensive
- [ ] No obvious security issues
- [ ] Changes are well-documented

**For Authors:**

- [ ] Self-reviewed the code
- [ ] Added tests for new code
- [ ] Updated documentation
- [ ] Ran linter and fixed issues
- [ ] Ran tests locally
- [ ] No debug code or commented-out code
- [ ] Commit messages are clear

---

## Performance Optimization

### Profiling & Benchmarking

#### CPU Profiling

```bash
# Run with CPU profiling
go test -cpuprofile=cpu.prof -bench=.

# Analyze profile
go tool pprof cpu.prof
(pprof) top10
(pprof) list functionName
(pprof) web  # Requires graphviz
```

#### Memory Profiling

```bash
# Run with memory profiling
go test -memprofile=mem.prof -bench=.

# Analyze profile
go tool pprof mem.prof
(pprof) top10
(pprof) list functionName
```

#### Trace Analysis

```bash
# Generate trace
go test -trace=trace.out -bench=BenchmarkOperation

# View trace
go tool trace trace.out
```

### Performance Targets

**By Component:**

| Component | Metric | Target | Measurement |
|-----------|--------|--------|-------------|
| S3 Backend | Throughput | 400-800 MB/s | Large file sequential read |
| Cache L1 | Hit latency | <1ms | Cached object retrieval |
| Cache L2 | Hit latency | <10ms | Disk cached retrieval |
| Write Buffer | Flush time | <100ms | 1MB buffer flush |
| FUSE Operations | Latency | <10ms | readdir, stat, open |

**Regression Tolerance:**

- <5% performance degradation acceptable for major features
- <2% for refactoring and code quality improvements
- 0% for bug fixes (must not introduce regression)

---

## AWS-C-S3 Integration Plan

### Overview

AWS-C-S3 is a high-performance C library for S3 operations that provides:

- Automatic request splitting across multiple connections
- Parallel chunk uploads/downloads
- Advanced network optimization
- Significantly better throughput than standard SDKs

### Integration Strategy

#### Phase 1: Research & Prototyping (v0.4.0)

**Duration:** 4-6 weeks
**Branch:** `feature/v0.4.0-aws-c-s3-research`

**Goals:**

1. Build aws-c-s3 and dependencies
2. Create Go bindings using CGo
3. Prototype basic operations (Get, Put)
4. Benchmark against current AWS SDK v2
5. Identify integration challenges

**Tasks:**

- [ ] Build aws-c-s3 on Linux/macOS/Windows
- [ ] Create CGo wrapper for core operations
- [ ] Implement basic error handling
- [ ] Write proof-of-concept tests
- [ ] Performance comparison benchmarks

**Expected Outcome:**

- Working prototype demonstrating >2x throughput improvement
- Technical design document for full integration
- Risk assessment and mitigation plan

#### Phase 2: Backend Implementation (v0.5.0)

**Duration:** 6-8 weeks
**Branch:** `feature/v0.5.0-aws-c-s3-backend`

**Goals:**

1. Implement complete S3 backend using aws-c-s3
2. Feature parity with current AWS SDK v2 backend
3. Comprehensive testing
4. Performance validation

**Architecture:**

```go
// New aws-c-s3 backend
package awscs3

// #cgo LDFLAGS: -laws-c-s3 -laws-c-common -laws-checksums
// #include <aws/s3/s3.h>
import "C"

type Backend struct {
    client *C.struct_aws_s3_client
    config *BackendConfig
    // ...
}

func (b *Backend) GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error) {
    // CGo calls to aws-c-s3
    // ...
}

// Factory pattern for backend selection
type BackendFactory interface {
    CreateBackend(config *Config) (Backend, error)
}

// Allow runtime selection
func NewBackend(config *Config) (Backend, error) {
    switch config.BackendType {
    case "aws-sdk-v2":
        return s3.NewBackend(config)
    case "aws-c-s3":
        return awscs3.NewBackend(config)
    default:
        return s3.NewBackend(config)  // Default to SDK v2
    }
}
```

**Configuration:**

```yaml
backends:
  s3:
    bucket: "my-bucket"
    region: "us-west-2"

    # Backend selection
    backend_type: "aws-c-s3"  # or "aws-sdk-v2"

    # aws-c-s3 specific config
    aws_c_s3:
      part_size: 8388608  # 8MB
      max_connections: 10
      throughput_target_gbps: 10
```

**Testing Requirements:**

- [ ] All Tier 1-3 tests passing
- [ ] Feature parity with AWS SDK v2 backend
- [ ] >2x throughput improvement demonstrated
- [ ] No memory leaks (valgrind clean)
- [ ] Cross-platform build working

#### Phase 3: Optimization & Tuning (v0.6.0)

**Duration:** 4-6 weeks
**Branch:** `feature/v0.6.0-aws-c-s3-optimization`

**Goals:**

1. Optimize performance for common workloads
2. Tune connection pooling and chunking
3. Implement advanced features (multipart, etc.)
4. Production hardening

**Optimizations:**

- Connection pool sizing for different workloads
- Chunk size tuning based on file size
- Memory pool optimization
- Error handling and retry logic
- Graceful degradation

**Performance Targets:**

- 5-10x improvement for large file uploads (>100MB)
- 3-5x improvement for large file downloads
- 2x improvement for small file operations (<10MB)
- Maintain current performance for metadata operations

#### Phase 4: Production Rollout (v0.7.0)

**Duration:** Ongoing
**Branch:** N/A (merged to main)

**Rollout Strategy:**

1. **v0.5.0:** Available as opt-in experimental feature
2. **v0.6.0:** Beta quality, recommended for testing
3. **v0.7.0:** Production ready, default for new deployments
4. **v0.8.0:** Default for all deployments, AWS SDK v2 deprecated

**Migration Path:**

```yaml
# v0.5.0-v0.6.0: Opt-in
backends:
  s3:
    backend_type: "aws-c-s3"  # Explicit opt-in

# v0.7.0: Default changed, opt-out available
backends:
  s3:
    backend_type: "aws-sdk-v2"  # Explicit opt-out if needed

# v0.8.0+: aws-c-s3 only (aws-sdk-v2 removed)
backends:
  s3:
    # backend_type no longer needed
```

### Build & Dependency Management

#### CMake Integration

```cmake
# CMakeLists.txt for aws-c-s3 dependencies
cmake_minimum_required(VERSION 3.9)
project(objectfs-aws-c-s3)

# Find or build aws-c-s3 and dependencies
find_package(aws-c-s3 REQUIRED)
find_package(aws-c-common REQUIRED)
find_package(aws-checksums REQUIRED)

# Link libraries
target_link_libraries(objectfs-cgo
    AWS::aws-c-s3
    AWS::aws-c-common
    AWS::aws-checksums
)
```

#### Build Script

```bash
#!/bin/bash
# scripts/build-aws-c-s3.sh

set -e

# Build dependencies in order
build_dep() {
    local repo=$1
    local dir=$2

    echo "Building $repo..."
    git clone https://github.com/awslabs/$repo.git $dir
    cd $dir
    mkdir -p build
    cd build
    cmake .. -DCMAKE_INSTALL_PREFIX=/usr/local
    make -j$(nproc)
    sudo make install
    cd ../..
}

# Build in dependency order
build_dep "aws-c-common" "aws-c-common"
build_dep "aws-checksums" "aws-checksums"
build_dep "aws-c-cal" "aws-c-cal"
build_dep "aws-c-io" "aws-c-io"
build_dep "aws-c-compression" "aws-c-compression"
build_dep "aws-c-http" "aws-c-http"
build_dep "aws-c-auth" "aws-c-auth"
build_dep "aws-c-s3" "aws-c-s3"

echo "AWS-C-S3 build complete!"
```

#### Go Build Tags

```go
// +build awscs3

// backend_awscs3.go
package backend

// This file only compiles when 'awscs3' build tag is present
```

```bash
# Build with aws-c-s3 support
go build -tags awscs3 ./cmd/objectfs

# Build without (use AWS SDK v2)
go build ./cmd/objectfs
```

### Risk Mitigation

**Risk 1: Build Complexity**

- **Mitigation:** Provide pre-built binaries for common platforms
- **Fallback:** AWS SDK v2 backend always available

**Risk 2: Platform Compatibility**

- **Mitigation:** Extensive cross-platform testing in CI
- **Fallback:** Build tags allow platform-specific builds

**Risk 3: Memory Safety (CGo)**

- **Mitigation:** Comprehensive memory leak testing
- **Tools:** valgrind, AddressSanitizer, Go race detector

**Risk 4: Performance Regression**

- **Mitigation:** Extensive benchmarking at each phase
- **Requirement:** Must show >2x improvement to justify complexity

### Success Metrics

**Technical:**

- [ ] 5x+ throughput for large files (>100MB)
- [ ] 3x+ throughput for medium files (10-100MB)
- [ ] Zero memory leaks in 72-hour stress test
- [ ] Cross-platform builds working (Linux, macOS, Windows)

**Operational:**

- [ ] Feature parity with AWS SDK v2 backend
- [ ] Clear migration documentation
- [ ] Production deployments running successfully
- [ ] User satisfaction with performance

---

## Summary: Key Principles

1. **Branch Strategy:** Feature branches → develop → release → main
2. **No LocalStack:** Use real AWS for integration testing
3. **Test Tiers:** Unit (always) → Integration (PR) → Real AWS (release)
4. **Performance:** Benchmark everything, no regressions
5. **AWS-C-S3:** Phased integration with fallback to AWS SDK v2
6. **Quality:** 80%+ test coverage, all tests passing
7. **Documentation:** Update docs with code changes

---

## Questions or Feedback?

- **GitHub Issues:** <https://github.com/scttfrdmn/objectfs/issues>
- **Discussions:** <https://github.com/scttfrdmn/objectfs/discussions>

This document is a living guide and will be updated as we learn and improve our processes.
