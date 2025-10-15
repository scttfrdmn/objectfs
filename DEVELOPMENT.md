# ObjectFS Development Guide

**Version:** 2.0 (Simplified for Solodev)
**Last Updated:** October 15, 2025

---

## Overview

This is a simplified development guide for ObjectFS as a solodev project.
The focus is on local development, explicit testing, and minimal CI/CD
complexity.

---

## Quick Start

```bash
# Clone and setup
git clone https://github.com/scttfrdmn/objectfs.git
cd objectfs
go mod download

# Run tests locally
make test

# Build
make build

# Run
./objectfs --help
```

---

## Testing Strategy

### Philosophy

- **Test locally first** - All testing should be done explicitly on your local machine
- **Use real AWS when needed** - No LocalStack; use real S3 for integration tests
- **Keep it simple** - Unit tests for logic, integration tests for AWS interactions

### Test Types

#### 1. Unit Tests (Local, Fast)

For pure Go logic without external dependencies:

```bash
# Run all unit tests
go test ./...

# Run with race detector
go test -race ./...

# Run with coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

#### 2. AWS Integration Tests (Local, Requires AWS)

For testing against real S3:

```bash
# Setup environment
export AWS_PROFILE=aws
export AWS_REGION=us-west-2
export OBJECTFS_TEST_BUCKET=objectfs-test-$(whoami)

# Create test bucket (one time)
aws s3 mb s3://$OBJECTFS_TEST_BUCKET

# Run integration tests
go test -v ./tests/aws/...

# Cleanup when done
aws s3 rm s3://$OBJECTFS_TEST_BUCKET --recursive
aws s3 rb s3://$OBJECTFS_TEST_BUCKET
```

**Test Best Practices:**

- Use `testing.Short()` to skip AWS tests: `if testing.Short() { t.Skip() }`
- Always cleanup test data in defer statements
- Use unique prefixes for each test run: `test-run-<timestamp>/`

### Running Tests

```bash
# Quick tests (skip AWS integration)
go test -short ./...

# All tests including AWS integration
go test ./...

# Specific package
go test -v ./internal/cache

# Specific test
go test -v -run TestCacheGetPut ./internal/cache

# Benchmarks
go test -bench=. ./...
```

---

## Code Quality

### Linting

```bash
# Install golangci-lint
brew install golangci-lint  # macOS
# or: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Run lint
golangci-lint run

# Auto-fix issues
golangci-lint run --fix
```

### Formatting

```bash
# Format all code
go fmt ./...

# More aggressive formatting with goimports
go install golang.org/x/tools/cmd/goimports@latest
goimports -w .
```

### Security Scanning

```bash
# Install gosec
go install github.com/securego/gosec/v2/cmd/gosec@latest

# Scan for security issues
gosec ./...
```

---

## Development Workflow

### Simple Git Workflow

```bash
# Work directly on main for small changes
git checkout main
git pull
# make changes
git add .
git commit -m "feat: add new feature"
git push

# Use feature branches for larger work
git checkout -b feature/my-feature
# make changes, commit
git checkout main
git merge feature/my-feature
git push
git branch -d feature/my-feature
```

### Commit Message Format

```
<type>: <subject>

<optional body>
```

**Types:**

- `feat`: New feature
- `fix`: Bug fix
- `refactor`: Code refactoring
- `test`: Test changes
- `docs`: Documentation
- `chore`: Maintenance

**Examples:**

```
feat: add S3 transfer acceleration support
fix: resolve cache memory leak
refactor: simplify buffer management
test: add integration tests for multipart upload
docs: update README with installation instructions
```

---

## AWS Setup for Testing

### Test Account Configuration

Create a dedicated AWS account or IAM user for testing:

```bash
# Configure AWS CLI with test credentials
aws configure --profile aws
# AWS Access Key ID: <your-test-key>
# AWS Secret Access Key: <your-test-secret>
# Default region name: us-west-2
# Default output format: json

# Verify configuration
aws sts get-caller-identity --profile aws
```

### IAM Policy for Testing

Create an IAM policy for testing (least privilege):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:CreateBucket",
        "s3:DeleteBucket",
        "s3:ListBucket",
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:PutBucketLifecycleConfiguration",
        "s3:GetBucketLifecycleConfiguration"
      ],
      "Resource": [
        "arn:aws:s3:::objectfs-test-*",
        "arn:aws:s3:::objectfs-test-*/*"
      ]
    }
  ]
}
```

### Test Bucket Lifecycle Policy

Set up automatic cleanup for test buckets:

```bash
# Create lifecycle policy
cat > lifecycle.json <<EOF
{
  "Rules": [{
    "Id": "DeleteOldTestData",
    "Status": "Enabled",
    "Prefix": "",
    "Expiration": {
      "Days": 7
    }
  }]
}
EOF

# Apply to test bucket
aws s3api put-bucket-lifecycle-configuration \
  --bucket objectfs-test-$(whoami) \
  --lifecycle-configuration file://lifecycle.json \
  --profile aws
```

---

## Performance Profiling

### CPU Profiling

```bash
# Generate CPU profile
go test -cpuprofile=cpu.prof -bench=.

# Analyze
go tool pprof cpu.prof
# (pprof) top10
# (pprof) list <function-name>
# (pprof) web  # requires graphviz
```

### Memory Profiling

```bash
# Generate memory profile
go test -memprofile=mem.prof -bench=.

# Analyze
go tool pprof mem.prof
# (pprof) top10
# (pprof) list <function-name>
```

### Trace Analysis

```bash
# Generate execution trace
go test -trace=trace.out -bench=BenchmarkOperation

# View in browser
go tool trace trace.out
```

---

## Building and Releases

### Local Build

```bash
# Build for current platform
go build -o objectfs ./cmd/objectfs

# Build with version info
VERSION=$(git describe --tags --always --dirty)
go build -ldflags "-X main.Version=$VERSION" -o objectfs ./cmd/objectfs

# Run
./objectfs --version
```

### Cross-Platform Build

```bash
# Linux AMD64
GOOS=linux GOARCH=amd64 go build -o objectfs-linux-amd64 ./cmd/objectfs

# macOS AMD64
GOOS=darwin GOARCH=amd64 go build -o objectfs-darwin-amd64 ./cmd/objectfs

# macOS ARM64 (M1/M2/M3)
GOOS=darwin GOARCH=arm64 go build -o objectfs-darwin-arm64 ./cmd/objectfs

# Windows
GOOS=windows GOARCH=amd64 go build -o objectfs-windows-amd64.exe ./cmd/objectfs
```

### Release Process

```bash
# 1. Update version
vim CHANGELOG.md  # Document changes
vim internal/version/version.go  # Update version constant

# 2. Commit changes
git add CHANGELOG.md internal/version/version.go
git commit -m "chore: bump version to v0.4.0"

# 3. Tag release
git tag -a v0.4.0 -m "Release v0.4.0"
git push origin v0.4.0
git push origin main

# 4. Build binaries
./scripts/build-release.sh

# 5. Create GitHub release (manual or via gh CLI)
gh release create v0.4.0 \
  objectfs-* \
  --title "ObjectFS v0.4.0" \
  --notes-file CHANGELOG.md
```

---

## AWS-C-S3 Integration (Future)

When ready to integrate aws-c-s3 for performance:

### 1. Research Phase

```bash
# Clone and build aws-c-s3
git clone https://github.com/awslabs/aws-c-s3.git
cd aws-c-s3
mkdir build && cd build
cmake .. -DCMAKE_BUILD_TYPE=Release
make
sudo make install
```

### 2. Create CGo Wrapper

Create `internal/backend/awscs3/` package with CGo bindings.

### 3. Benchmark Comparison

```bash
# Baseline with AWS SDK v2
go test -bench=BenchmarkS3 -benchmem -count=10 > baseline.txt

# With aws-c-s3
go test -bench=BenchmarkS3 -benchmem -count=10 -tags=awscs3 > awscs3.txt

# Compare
benchstat baseline.txt awscs3.txt
```

### 4. Integration

Add build tags to allow building with/without aws-c-s3:

```bash
# Build with aws-c-s3
go build -tags awscs3 ./cmd/objectfs

# Build without (standard AWS SDK)
go build ./cmd/objectfs
```

---

## Makefile Targets

Create a simple Makefile for common tasks:

```makefile
.PHONY: all build test lint clean

all: build

build:
 go build -o objectfs ./cmd/objectfs

test:
 go test ./...

test-quick:
 go test -short ./...

test-aws:
 go test -v ./tests/aws/...

lint:
 golangci-lint run

fmt:
 go fmt ./...
 goimports -w .

clean:
 rm -f objectfs coverage.out cpu.prof mem.prof trace.out
 go clean

install:
 go install ./cmd/objectfs
```

---

## CI/CD (Minimal)

The `.github/workflows/ci.yml` provides basic checks:

- Runs tests on push/PR to main
- Runs linting
- That's it - keep it simple!

All serious testing should be done locally before pushing.

---

## Project Structure

```
objectfs/
├── cmd/
│   └── objectfs/          # Main binary
├── internal/
│   ├── cache/             # Caching layer
│   ├── backend/           # Storage backends
│   │   ├── s3/           # AWS S3 backend
│   │   └── mock/         # Mock backend for testing
│   ├── buffer/            # Write buffer
│   ├── fuse/              # FUSE implementation
│   └── config/            # Configuration
├── tests/
│   ├── unit/              # Unit tests
│   └── aws/               # AWS integration tests
├── docs/                  # Documentation
├── scripts/               # Build/utility scripts
└── Makefile               # Common tasks
```

---

## Tips and Best Practices

### Local Development

1. **Use environment variables** for configuration during development
2. **Keep test buckets isolated** - use unique names per developer
3. **Clean up regularly** - delete old test buckets and data
4. **Profile often** - catch performance issues early

### Testing

1. **Write tests first** - TDD helps with design
2. **Test error paths** - don't just test happy paths
3. **Use table-driven tests** - makes adding cases easy
4. **Mock external dependencies** - for fast unit tests

### Performance

1. **Benchmark before optimizing** - measure first
2. **Use pprof** - find actual bottlenecks
3. **Pre-allocate when size is known** - avoid repeated allocations
4. **Use sync.Pool for buffers** - reuse memory

### Code Quality

1. **Run formatters** - go fmt, goimports
2. **Run linters** - golangci-lint catches issues
3. **Document public APIs** - godoc comments
4. **Keep functions small** - easier to test and understand

---

## Getting Help

- **Issues:** <https://github.com/scttfrdmn/objectfs/issues>
- **Discussions:** <https://github.com/scttfrdmn/objectfs/discussions>

---

## Summary

This is a **simple, pragmatic development guide** for solodev work:

- ✅ Test locally with explicit commands
- ✅ Use real AWS (no LocalStack complexity)
- ✅ Minimal CI/CD (just basic checks)
- ✅ Focus on code quality and performance
- ✅ Keep it simple and maintainable
