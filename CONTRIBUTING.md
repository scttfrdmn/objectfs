# Contributing to ObjectFS

Thank you for your interest in contributing to ObjectFS! We welcome contributions from the community and are pleased to have you join us.

## 📋 Table of Contents

- [Code of Conduct](#-code-of-conduct)
- [Getting Started](#-getting-started)
- [Development Setup](#-development-setup)
- [Making Changes](#-making-changes)
- [Testing](#-testing)
- [Submitting Changes](#-submitting-changes)
- [Code Style](#-code-style)
- [Documentation](#-documentation)

## 🤝 Code of Conduct

This project and everyone participating in it is governed by our commitment to creating a welcoming and inclusive environment. Please be respectful and professional in all interactions.

## 🚀 Getting Started

### Prerequisites

- Go 1.21 or later
- Git
- Basic understanding of FUSE filesystems and AWS S3
- Familiarity with Go development practices

### Areas for Contribution

We welcome contributions in several areas:

#### 🐛 **Bug Fixes**

- S3 operation edge cases
- FUSE filesystem compatibility issues
- Performance bottlenecks
- Memory leaks or resource management issues

#### ✨ **Feature Enhancements**

- Additional S3 storage tier support
- Enhanced cost optimization algorithms
- Improved caching strategies
- Cross-platform compatibility improvements

#### 📚 **Documentation**

- API documentation improvements
- Usage examples and tutorials
- Enterprise deployment guides
- Performance tuning documentation

#### 🧪 **Testing**

- Unit test coverage improvements
- Integration tests for various S3 configurations
- Performance benchmarks
- Edge case testing

## 🛠 Development Setup

1. **Fork and Clone**

   ```bash
   git clone https://github.com/YOUR-USERNAME/objectfs.git
   cd objectfs
   ```

2. **Install Pre-commit Hooks**

   ```bash
   ./scripts/setup-hooks.sh
   ```

3. **Install Dependencies**

   ```bash
   go mod download
   ```

4. **Verify Setup**

   ```bash
   go test ./...
   pre-commit run --all-files
   ```

## 🔧 Making Changes

### Branch Naming Convention

Use descriptive branch names with prefixes:

- `feature/` - New features
- `fix/` - Bug fixes  
- `docs/` - Documentation updates
- `test/` - Test improvements
- `refactor/` - Code refactoring

Examples:

- `feature/glacier-deep-archive-support`
- `fix/cache-memory-leak`
- `docs/enterprise-pricing-guide`

### Commit Messages

Follow conventional commit format:

```
type(scope): brief description

Detailed explanation if needed

Fixes #123
```

Types: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `chore`

Examples:

- `feat(s3): add Glacier Deep Archive storage tier support`
- `fix(cache): resolve memory leak in LRU cache implementation`
- `docs(pricing): add enterprise discount configuration guide`

## 🧪 Testing

### Running Tests

Always with `-race`. Sixteen concurrency bugs were filed and fixed in this repository after a
document declared it race-free, and the detector found most of them.

```bash
# What CI runs
go test -race -timeout 20m ./...

# With coverage
go test -race -coverprofile=coverage.out ./...

# One package
go test -race ./internal/storage/s3/

# Against real AWS
AWS_PROFILE=aws AWS_REGION=us-west-2 go test -race -tags=integration ./...
```

### Writing Tests

- Add tests for all new functionality
- Maintain or improve test coverage — the target is 80%+ per package
- Table-driven, with `t.Parallel()` in both the parent and each subtest
- Test both success and error cases
- Prefer `internal/testaws` to a hand-written mock (see [Integration Tests](#integration-tests))

**Verify that a new test can fail.** Break the thing it asserts, watch it fail, then put it back. A
test that passes for the wrong reason is worse than no test, because it is counted as coverage: the
audit above found assertions that had never once been able to fail.

Example test structure:

```go
func TestNewFeature(t *testing.T) {
    tests := []struct {
        name     string
        input    InputType
        expected ExpectedType
        wantErr  bool
    }{
        {
            name:     "valid input",
            input:    validInput,
            expected: expectedOutput,
            wantErr:  false,
        },
        // ... more test cases
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Test implementation
        })
    }
}
```

### Integration Tests

**Do not use LocalStack, and prefer not to write a mock.** This section said "use LocalStack when
possible"; the project does not use it and never has. There are two better options, in this order:

1. **`internal/testaws`** — the real S3 backend against an in-process
   [substrate](https://github.com/scttfrdmn/substrate) endpoint over real HTTP. No network, no
   credentials, no AWS account, and fast enough for ordinary unit tests. This is the default choice.
2. **Real AWS**, behind `-tags=integration`, with `AWS_PROFILE=aws AWS_REGION=us-west-2`. For the
   things an emulator cannot answer: eventual-consistency behaviour, real multipart limits, IAM
   errors, and throughput.

A hand-written mock is the option of last resort, and the reason is worth stating: a mock sits on the
far side of a seam and agrees with its caller by construction. 32,680 lines of tests across 90 files
missed roughly 45 defects in the v0.10.0 audit, and nearly every one was a value produced correctly
at one layer and dropped at the boundary to the next — invisible to any test that mocked the
neighbouring layer.

If substrate is missing a capability you need, [file an issue against
it](https://github.com/scttfrdmn/substrate/issues) rather than working around the gap with a mock.

For tests that do reach real AWS:

- Provide clear setup instructions
- Skip rather than fail when credentials are absent
- Clean up every object and bucket the test creates, including on failure

## 📤 Submitting Changes

### Pull Request Process

1. **Ensure Quality**

   ```bash
   # Pre-commit hooks will run automatically, but you can run manually:
   pre-commit run --all-files
   ```

2. **Update Documentation**
   - Update README if adding new features
   - Add/update code comments for complex logic
   - Update examples if configuration changes

3. **Create Pull Request**
   - Use descriptive PR title and description
   - Reference related issues
   - Include testing instructions
   - Add screenshots for UI changes (if applicable)

### PR Template

```markdown
## Description
Brief description of changes

## Type of Change
- [ ] Bug fix
- [ ] New feature
- [ ] Breaking change
- [ ] Documentation update

## Testing
- [ ] Tests pass locally
- [ ] Added tests for new functionality
- [ ] Manual testing completed

## Checklist
- [ ] Code follows style guidelines
- [ ] Self-review completed
- [ ] Documentation updated
- [ ] No new warnings or errors
```

## 📝 Code Style

### Go Style Guidelines

Follow standard Go practices:

- Use `go fmt` and `goimports`
- Follow effective Go guidelines
- Use meaningful variable and function names
- Add comments for exported functions and types
- Keep functions focused and small
- Handle errors appropriately

### Pre-commit Hooks

Our pre-commit hooks automatically enforce:

- Code formatting (gofmt, goimports)
- Linting (golangci-lint)
- Security scanning (gosec)
- Test execution
- Import organization

### Architecture Guidelines

- Follow existing patterns in the codebase
- Use interfaces for testability
- Implement proper error handling
- Add appropriate logging
- Consider performance implications
- Maintain backward compatibility

## 📚 Documentation

### Code Documentation

- Document all exported functions and types
- Use clear, concise descriptions
- Include usage examples where helpful
- Document complex algorithms or business logic

### External Documentation

- Update README.md for new features
- Add configuration examples
- Update deployment guides
- Create tutorials for complex features

### Documentation may only claim what the code does

A v0.10.0 audit found roughly 190 overclaims across 60+ files: APIs that were never written, CLI
subcommands the binary does not have, features whose packages nothing imports, and throughput figures
no benchmark produced. Almost none of them were lies when typed. Each was written from intent, was
true of the plan, and then had no mechanism for noticing that the plan changed.

Three of these are now checked mechanically. A PR that breaks one fails in CI, and the failure
message says what to do:

| Gate | What it enforces |
|---|---|
| `TestDocumentedGoSymbolsExist` | a `pkg.Symbol` in a fenced Go block, where that block imports an ObjectFS package, must be exported by it |
| `TestDocumentedCLIInvocationsAreRunnable` | an `objectfs` command line must use flags the binary parses, and no subcommand — there are none |
| `TestDocumentedConfigYAMLMatchesTheSchema` | a YAML block that claims to be ObjectFS configuration must strict-decode against the schema |
| `TestNoDocumentRestatesTheCurrentVersion` | only `cmd/objectfs/main.go` may say which version is current |

All four live in `internal/config/`. If one is wrong about your change, fix the gate — do not add an
exemption without a reason recorded in the exemption itself, as `docsExemptFromConfigSchema` does.

**Performance figures are not machine-checkable, so this part is a rule rather than a test.** A
throughput, latency, or speedup number in documentation must cite:

1. the benchmark that produced it, by file and function — something under `benchmarks/` or a
   `func Benchmark…` a reader can run;
2. the parameters: bucket region, object size, concurrency, and instance type or `-cpu` if either
   matters;
3. the command, copy-pasteable.

Without all three, say nothing about throughput. That is not a stylistic preference — the audit's
single most-repeated false claim was "4.6x throughput improvement", in 21 places across 9 files,
attributed to a congestion-control implementation with no caller on any mount path. It was uncheckable
by construction, which is why it survived nine files' worth of review. A number that cannot be
reproduced is worse than no number, because a reader plans capacity around it.

The same applies to features. If a package is not reachable from a mount, documenting it as a
capability of the shipping product is a defect regardless of how complete the code is — see the
[Not yet wired up](docs/index.md#not-yet-wired-up) table, which exists so that such code can be
described honestly rather than deleted or oversold.

## 🏆 Recognition

Contributors will be:

- Added to the contributors list
- Credited in release notes for significant contributions
- Invited to participate in project discussions

## ❓ Questions?

- **General Questions**: [GitHub Discussions](https://github.com/scttfrdmn/objectfs/discussions)
- **Bug Reports**: [GitHub Issues](https://github.com/scttfrdmn/objectfs/issues)
- **Feature Requests**: [GitHub Issues](https://github.com/scttfrdmn/objectfs/issues) with `enhancement` label

## 🎯 Good First Issues

Look for issues labeled `good first issue` for contribution opportunities that are:

- Well-documented
- Limited in scope
- Good introduction to the codebase
- Have clear acceptance criteria

Thank you for contributing to ObjectFS! 🚀
