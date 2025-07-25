# Contributing to ObjectFS

We love your input! We want to make contributing to ObjectFS as easy and transparent as possible, whether it's:

- Reporting a bug
- Discussing the current state of the code
- Submitting a fix
- Proposing new features
- Becoming a maintainer

## We Develop with Github

We use GitHub to host code, to track issues and feature requests, as well as accept pull requests.

## We Use [Github Flow](https://guides.github.com/introduction/flow/index.html), So All Code Changes Happen Through Pull Requests

Pull requests are the best way to propose changes to the codebase. We actively welcome your pull requests:

1. Fork the repo and create your branch from `main`.
2. If you've added code that should be tested, add tests.
3. If you've changed APIs, update the documentation.
4. Ensure the test suite passes.
5. Make sure your code lints.
6. Issue that pull request!

## Development Environment Setup

### Prerequisites

- Go 1.19 or later
- Linux with FUSE support (`sudo modprobe fuse`)
- Make
- Git
- Docker (optional, for integration tests)

### Setup

```bash
# Clone your fork
git clone https://github.com/your-username/objectfs.git
cd objectfs

# Add upstream remote
git remote add upstream https://github.com/objectfs/objectfs.git

# Install dependencies
make deps

# Run tests to verify setup
make test
```

## Code Standards

### Go Code Style

We follow standard Go conventions:

- Use `gofmt` for formatting
- Use `go vet` for static analysis
- Follow [Effective Go](https://golang.org/doc/effective_go.html) guidelines
- Use meaningful variable and function names
- Add comments for exported functions and types

### Code Organization

```
objectfs/
├── cmd/                    # Main applications
│   └── objectfs/          # Main CLI application
├── internal/              # Private application code
│   ├── adapter/           # Core adapter logic
│   ├── cache/             # Caching implementations
│   ├── config/            # Configuration management
│   ├── fuse/              # FUSE filesystem operations
│   ├── metrics/           # Metrics collection
│   └── storage/           # Storage backend implementations
├── pkg/                   # Public library code
│   ├── types/             # Type definitions and interfaces
│   └── utils/             # Utility functions
├── test/                  # Test files
│   ├── integration/       # Integration tests
│   └── benchmarks/        # Performance benchmarks
├── scripts/               # Build and deployment scripts
├── docs/                  # Documentation
├── deploy/                # Deployment configurations
│   ├── docker/            # Docker configurations
│   └── kubernetes/        # Kubernetes manifests
└── examples/              # Example configurations
```

### Testing

- Write unit tests for all new functionality
- Use table-driven tests where appropriate
- Mock external dependencies
- Aim for >90% test coverage
- Write integration tests for critical paths
- Include benchmarks for performance-critical code

Example test structure:
```go
func TestMyFunction(t *testing.T) {
    tests := []struct {
        name     string
        input    string
        expected string
        wantErr  bool
    }{
        {
            name:     "valid input",
            input:    "test",
            expected: "result",
            wantErr:  false,
        },
        // Add more test cases
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result, err := MyFunction(tt.input)
            if (err != nil) != tt.wantErr {
                t.Errorf("MyFunction() error = %v, wantErr %v", err, tt.wantErr)
                return
            }
            if result != tt.expected {
                t.Errorf("MyFunction() = %v, want %v", result, tt.expected)
            }
        })
    }
}
```

### Documentation

- Use godoc-style comments for all exported functions and types
- Update README.md if you change functionality
- Add examples in the `examples/` directory
- Update configuration documentation for new config options

## Commit Messages

We follow the [Conventional Commits](https://www.conventionalcommits.org/) specification:

```
<type>[optional scope]: <description>

[optional body]

[optional footer(s)]
```

Types:
- `feat`: A new feature
- `fix`: A bug fix
- `docs`: Documentation only changes
- `style`: Changes that do not affect the meaning of the code
- `refactor`: A code change that neither fixes a bug nor adds a feature
- `perf`: A code change that improves performance
- `test`: Adding missing tests or correcting existing tests
- `chore`: Changes to the build process or auxiliary tools

Examples:
```
feat(cache): add LRU cache implementation

fix(fuse): handle permission errors correctly

docs: update installation instructions

test(storage): add unit tests for S3 backend
```

## Pull Request Process

1. **Create a branch**: Create a feature branch from `main`
   ```bash
   git checkout -b feat/my-new-feature
   ```

2. **Make changes**: Implement your feature or fix

3. **Test your changes**: Run the full test suite
   ```bash
   make check  # Runs fmt, vet, lint, and test
   ```

4. **Update documentation**: Update relevant documentation

5. **Commit your changes**: Use conventional commit messages
   ```bash
   git commit -m "feat(cache): add distributed cache support"
   ```

6. **Push to your fork**:
   ```bash
   git push origin feat/my-new-feature
   ```

7. **Create Pull Request**: Open a PR against the `main` branch

### Pull Request Template

When creating a pull request, please use this template:

```markdown
## Description
Brief description of the changes

## Type of Change
- [ ] Bug fix (non-breaking change which fixes an issue)
- [ ] New feature (non-breaking change which adds functionality)
- [ ] Breaking change (fix or feature that would cause existing functionality to not work as expected)
- [ ] Documentation update

## Testing
- [ ] Unit tests pass
- [ ] Integration tests pass (if applicable)
- [ ] Manual testing completed

## Checklist
- [ ] My code follows the style guidelines of this project
- [ ] I have performed a self-review of my own code
- [ ] I have commented my code, particularly in hard-to-understand areas
- [ ] I have made corresponding changes to the documentation
- [ ] My changes generate no new warnings
- [ ] I have added tests that prove my fix is effective or that my feature works
- [ ] New and existing unit tests pass locally with my changes
```

## Performance Considerations

ObjectFS is a performance-critical system. When contributing:

- **Profile your changes**: Use Go's built-in profiler to ensure no performance regressions
- **Benchmark critical paths**: Add benchmarks for performance-sensitive code
- **Memory efficiency**: Be mindful of memory allocations in hot paths
- **Concurrency**: Ensure thread-safety without unnecessary locking
- **Caching**: Consider caching implications of your changes

### Running Benchmarks

```bash
# Run all benchmarks
make bench

# Run specific benchmarks
go test -bench=BenchmarkCacheGet ./internal/cache/

# Profile memory usage
go test -bench=. -memprofile=mem.prof ./internal/cache/
go tool pprof mem.prof
```

## Security Considerations

- Never commit secrets, API keys, or credentials
- Use secure coding practices
- Validate all inputs
- Handle errors appropriately
- Consider security implications of new features
- Report security issues privately to security@objectfs.io

## Issue Reporting

When reporting bugs, please include:

1. **Environment details**: OS, Go version, ObjectFS version
2. **Steps to reproduce**: Clear steps to reproduce the issue
3. **Expected behavior**: What you expected to happen
4. **Actual behavior**: What actually happened
5. **Configuration**: Relevant configuration files (sanitized)
6. **Logs**: Relevant log output
7. **Stack trace**: If applicable

Use the issue templates provided in the repository.

## Feature Requests

When requesting features:

1. **Use case**: Describe the problem you're trying to solve
2. **Proposed solution**: Your suggested approach
3. **Alternatives**: Other solutions you've considered
4. **Impact**: How this would benefit users
5. **Implementation**: Any implementation ideas (optional)

## Code Review

All submissions require review. We use GitHub pull requests for this purpose.

### Review Criteria

- **Correctness**: Does the code do what it's supposed to do?
- **Performance**: Does it meet performance requirements?
- **Style**: Does it follow Go conventions and project style?
- **Tests**: Are there adequate tests?
- **Documentation**: Is it properly documented?
- **Security**: Are there any security concerns?

### Reviewer Guidelines

- Be constructive and respectful
- Explain the reasoning behind feedback
- Suggest improvements rather than just pointing out problems
- Focus on the code, not the person
- Approve changes that improve the codebase, even if not perfect

## Getting Help

- **Chat**: Join our discussions on GitHub
- **Issues**: Create an issue for bugs or feature requests
- **Email**: Contact maintainers at maintainers@objectfs.io

## Recognition

Contributors will be:
- Listed in the CONTRIBUTORS file
- Mentioned in release notes for significant contributions
- Invited to become maintainers based on sustained contributions

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.

Thank you for contributing to ObjectFS! 🚀