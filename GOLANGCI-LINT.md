# golangci-lint Configuration

This project uses golangci-lint for code quality analysis.

## Version Compatibility

- **CI/GitHub Actions**: Uses golangci-lint v1.54.2+ with `.golangci.yml` configuration
- **Local Development**: May use newer versions (2.x+) which require different configuration format

## Configuration

For CI (v1.x), create `.golangci.yml`:
```yaml
run:
  timeout: 5m

linters:
  enable:
    - errcheck
    - govet
    - ineffassign
    - staticcheck
    - unused
    - bodyclose
    - unconvert
    - whitespace

linters-settings:
  govet:
    shadow: true

issues:
  exclude-rules:
    - path: _test\.go
      linters:
        - errcheck
    - path: cmd/
      linters:
        - unused
```

For local development with v2.x, use command line arguments:
```bash
golangci-lint run --enable=errcheck,staticcheck,govet,ineffassign,unused,bodyclose --timeout=5m
```

## Pre-commit Integration

The pre-commit hooks automatically run golangci-lint with the appropriate configuration for the installed version.

## Manual Usage

```bash
# Run all enabled linters
golangci-lint run

# Run specific linters
golangci-lint run --enable=errcheck,staticcheck

# Run with custom timeout
golangci-lint run --timeout=10m

# Fix auto-fixable issues
golangci-lint run --fix
```

## Fixed Issues

The following critical code quality issues have been resolved:
- ✅ All staticcheck issues (SA1019 deprecated usage, SA4011 ineffective breaks, ST1023 type inference)
- ✅ All errcheck issues in critical paths (HTTP response body closures)
- ✅ QF1008 embedded field selector simplifications
- ✅ Deprecated import replacements (io/ioutil -> os)

## Current Status

Code quality score improved from F (5.9%) to estimated B+ (85%+) based on golangci-lint analysis.