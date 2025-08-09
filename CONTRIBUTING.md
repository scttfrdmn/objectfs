# Contributing to ObjectFS

Thank you for your interest in contributing to ObjectFS! We welcome contributions from the community and are pleased to have you join us.

## 📋 Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [Making Changes](#making-changes)
- [Testing](#testing)
- [Submitting Changes](#submitting-changes)
- [Code Style](#code-style)
- [Documentation](#documentation)

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

```bash
# Run all tests
go test ./...

# Run tests with coverage
go test -race -coverprofile=coverage.out ./...

# Run specific package tests
go test ./internal/storage/s3/

# Run with verbose output
go test -v ./...
```

### Writing Tests

- Add tests for all new functionality
- Maintain or improve test coverage
- Use table-driven tests where appropriate
- Mock external dependencies (AWS API calls)
- Test both success and error cases

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

For integration tests requiring AWS resources:
- Use LocalStack when possible
- Provide clear setup instructions
- Make tests optional if external resources required
- Clean up resources after tests

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