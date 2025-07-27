# ObjectFS Development Setup

## Quick Setup for Solo Development

ObjectFS uses **pre-commit hooks** for comprehensive development workflow instead of relying on CI/CD for basic checks.

### 1. Setup Development Environment

```bash
# Clone and setup
git clone https://github.com/objectfs/objectfs.git
cd objectfs

# Install and setup pre-commit hooks
./scripts/setup-hooks.sh
```

### 2. Development Workflow

Every commit automatically runs:
- 🔧 **Code formatting** (gofmt, goimports)
- 🔍 **Linting** (golangci-lint)
- 🧪 **Full test suite** (go test -race with coverage)
- 🔒 **Security scanning** (gosec)
- ⚡ **Performance benchmarks**
- 📊 **Integration tests** (if LocalStack available)

### 3. Manual Testing

```bash
# Run all checks manually
pre-commit run --all-files

# Run specific checks
pre-commit run go-test
pre-commit run gosec

# Skip hooks for emergency commits (not recommended)
git commit --no-verify
```

### 4. Integration Testing with LocalStack

```bash
# Start LocalStack
docker run --rm -d -p 4566:4566 \
  -e SERVICES=s3 \
  -e DEBUG=1 \
  --name localstack \
  localstack/localstack

# Export endpoint for tests
export AWS_ENDPOINT_URL=http://localhost:4566
export AWS_ACCESS_KEY_ID=test  
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1

# Now commits will run integration tests automatically
git commit -m "test integration"
```

## CI/CD (Security Only)

The GitHub Actions workflow is minimal and focused on:
- 🔒 **Weekly security scans**
- 📦 **Release builds** (only for tags)
- 🐳 **Docker images** (only for releases)
- 🔍 **Dependency vulnerability checks**

This keeps the feedback loop fast for solo development while ensuring security.

---

*The rest of your existing README content about ObjectFS features, usage, etc. would go here*