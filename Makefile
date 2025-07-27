# ObjectFS Makefile
# Enterprise-Grade High-Performance POSIX Filesystem for Object Storage

# Build information
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0-dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%I:%M:%S%p')
GO_VERSION := $(shell go version | cut -d ' ' -f 3)

# Build flags
LDFLAGS := -ldflags="-s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)"
TAGS := release,netgo
DEBUG_TAGS := debug
RACE_FLAGS := -race

# Directories
BIN_DIR := bin
BUILD_DIR := build
DIST_DIR := dist
COVERAGE_DIR := coverage

# Binary names
BINARY_NAME := objectfs
BINARY_PATH := $(BIN_DIR)/$(BINARY_NAME)

# Go build settings
CGO_ENABLED ?= 0
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

# Colors for output
COLOR_RESET = \033[0m
COLOR_BOLD = \033[1m
COLOR_GREEN = \033[32m
COLOR_YELLOW = \033[33m
COLOR_BLUE = \033[34m
COLOR_RED = \033[31m

.PHONY: all build clean test bench lint fmt vet check deps install uninstall
.PHONY: build-all build-linux build-darwin build-windows build-debug build-race
.PHONY: docker docker-build docker-push package release
.PHONY: coverage coverage-html coverage-report
.PHONY: setup-hooks pre-commit-run pre-commit-all
.PHONY: help version

# Default target - now includes hook setup
all: setup-hooks clean fmt vet test build

# Print help information
help:
	@echo "$(COLOR_BOLD)ObjectFS Build System$(COLOR_RESET)"
	@echo ""
	@echo "$(COLOR_BOLD)Available targets:$(COLOR_RESET)"
	@echo "  $(COLOR_GREEN)build$(COLOR_RESET)          Build the binary for current platform"
	@echo "  $(COLOR_GREEN)build-all$(COLOR_RESET)      Build binaries for all platforms"
	@echo "  $(COLOR_GREEN)build-debug$(COLOR_RESET)    Build debug binary with symbols"
	@echo "  $(COLOR_GREEN)build-race$(COLOR_RESET)     Build binary with race detection"
	@echo "  $(COLOR_GREEN)test$(COLOR_RESET)           Run all tests"
	@echo "  $(COLOR_GREEN)bench$(COLOR_RESET)          Run benchmarks"
	@echo "  $(COLOR_GREEN)coverage$(COLOR_RESET)       Generate test coverage report"
	@echo "  $(COLOR_GREEN)lint$(COLOR_RESET)           Run linters"
	@echo "  $(COLOR_GREEN)fmt$(COLOR_RESET)            Format Go code"
	@echo "  $(COLOR_GREEN)vet$(COLOR_RESET)            Run go vet"
	@echo "  $(COLOR_GREEN)check$(COLOR_RESET)          Run all checks (fmt, vet, lint, test)"
	@echo "  $(COLOR_GREEN)deps$(COLOR_RESET)           Download and tidy dependencies"
	@echo "  $(COLOR_GREEN)clean$(COLOR_RESET)          Clean build artifacts"
	@echo "  $(COLOR_GREEN)install$(COLOR_RESET)        Install binary to GOPATH/bin"
	@echo "  $(COLOR_GREEN)docker$(COLOR_RESET)         Build Docker image"
	@echo "  $(COLOR_GREEN)package$(COLOR_RESET)        Create distribution packages"
	@echo "  $(COLOR_GREEN)version$(COLOR_RESET)        Show version information"
	@echo ""
	@echo "$(COLOR_BOLD)Development workflow (solo dev):$(COLOR_RESET)"
	@echo "  $(COLOR_GREEN)setup-hooks$(COLOR_RESET)    Setup pre-commit hooks for development"
	@echo "  $(COLOR_GREEN)pre-commit-run$(COLOR_RESET) Run pre-commit hooks on staged files"
	@echo "  $(COLOR_GREEN)pre-commit-all$(COLOR_RESET) Run pre-commit hooks on all files"
	@echo "  $(COLOR_GREEN)dev-check$(COLOR_RESET)      Complete development workflow check"
	@echo ""
	@echo "$(COLOR_BOLD)Environment variables:$(COLOR_RESET)"
	@echo "  $(COLOR_YELLOW)VERSION$(COLOR_RESET)        Override version (default: git describe)"
	@echo "  $(COLOR_YELLOW)CGO_ENABLED$(COLOR_RESET)    Enable/disable CGO (default: 0)"
	@echo "  $(COLOR_YELLOW)GOOS$(COLOR_RESET)           Target OS (default: current)"
	@echo "  $(COLOR_YELLOW)GOARCH$(COLOR_RESET)         Target architecture (default: current)"

# Show version information
version:
	@echo "Version: $(VERSION)"
	@echo "Commit: $(COMMIT)"
	@echo "Build Time: $(BUILD_TIME)"
	@echo "Go Version: $(GO_VERSION)"

# Create necessary directories
$(BIN_DIR) $(BUILD_DIR) $(DIST_DIR) $(COVERAGE_DIR):
	@mkdir -p $@

# Download and tidy dependencies
deps:
	@echo "$(COLOR_BLUE)Downloading dependencies...$(COLOR_RESET)"
	@go mod download
	@go mod tidy
	@go mod verify

# Format Go code
fmt:
	@echo "$(COLOR_BLUE)Formatting Go code...$(COLOR_RESET)"
	@go fmt ./...

# Run go vet
vet:
	@echo "$(COLOR_BLUE)Running go vet...$(COLOR_RESET)"
	@go vet ./...

# Run linters (requires golangci-lint)
lint:
	@echo "$(COLOR_BLUE)Running linters...$(COLOR_RESET)"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "$(COLOR_YELLOW)golangci-lint not found, skipping...$(COLOR_RESET)"; \
	fi

# Run all checks
check: fmt vet lint test

# Build the binary
build: $(BIN_DIR)
	@echo "$(COLOR_BLUE)Building $(BINARY_NAME) $(VERSION) for $(GOOS)/$(GOARCH)...$(COLOR_RESET)"
	@CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build $(LDFLAGS) -tags $(TAGS) -o $(BINARY_PATH) ./cmd/objectfs
	@echo "$(COLOR_GREEN)Binary built: $(BINARY_PATH)$(COLOR_RESET)"

# Build cross-platform binary with cgofuse
build-cgofuse: $(BIN_DIR)
	@echo "$(COLOR_BLUE)Building cross-platform $(BINARY_NAME) $(VERSION) for $(GOOS)/$(GOARCH)...$(COLOR_RESET)"
	@CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build $(LDFLAGS) -tags cgofuse,$(TAGS) -o $(BIN_DIR)/$(BINARY_NAME)-cgofuse ./cmd/objectfs
	@echo "$(COLOR_GREEN)Cross-platform binary built: $(BIN_DIR)/$(BINARY_NAME)-cgofuse$(COLOR_RESET)"

# Build debug binary
build-debug: $(BIN_DIR)
	@echo "$(COLOR_BLUE)Building debug binary...$(COLOR_RESET)"
	@go build -tags $(DEBUG_TAGS) -o $(BIN_DIR)/$(BINARY_NAME)-debug ./cmd/objectfs

# Build binary with race detection
build-race: $(BIN_DIR)
	@echo "$(COLOR_BLUE)Building race detection binary...$(COLOR_RESET)"
	@go build $(RACE_FLAGS) -o $(BIN_DIR)/$(BINARY_NAME)-race ./cmd/objectfs

# Build for all platforms (standard and cgofuse versions)
build-all: build-linux build-darwin build-windows build-all-cgofuse

# Build all platforms with cgofuse for research users
build-all-cgofuse: build-darwin-cgofuse build-windows-cgofuse build-linux-cgofuse

# Build for Linux
build-linux: $(BUILD_DIR)
	@echo "$(COLOR_BLUE)Building for Linux...$(COLOR_RESET)"
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build $(LDFLAGS) -tags $(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/objectfs
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build $(LDFLAGS) -tags $(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/objectfs

# Build for macOS
build-darwin: $(BUILD_DIR)
	@echo "$(COLOR_BLUE)Building for macOS...$(COLOR_RESET)"
	@CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 \
		go build $(LDFLAGS) -tags $(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/objectfs
	@CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
		go build $(LDFLAGS) -tags $(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/objectfs

# Build for Windows
build-windows: $(BUILD_DIR)
	@echo "$(COLOR_BLUE)Building for Windows...$(COLOR_RESET)"
	@CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
		go build $(LDFLAGS) -tags $(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe ./cmd/objectfs

# Build cross-platform cgofuse versions
build-linux-cgofuse: $(BUILD_DIR)
	@echo "$(COLOR_BLUE)Building cgofuse for Linux...$(COLOR_RESET)"
	@CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
		go build $(LDFLAGS) -tags cgofuse,$(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64-cgofuse ./cmd/objectfs

build-darwin-cgofuse: $(BUILD_DIR)
	@echo "$(COLOR_BLUE)Building cgofuse for macOS (requires macFUSE)...$(COLOR_RESET)"
	@echo "$(COLOR_YELLOW)Note: This requires macFUSE to be installed for compilation$(COLOR_RESET)"
	@CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 \
		go build $(LDFLAGS) -tags cgofuse,$(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64-cgofuse ./cmd/objectfs || \
		echo "$(COLOR_RED)Failed to build macOS cgofuse version. Install macFUSE with: brew install --cask macfuse$(COLOR_RESET)"
	@CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
		go build $(LDFLAGS) -tags cgofuse,$(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64-cgofuse ./cmd/objectfs || \
		echo "$(COLOR_RED)Failed to build macOS ARM64 cgofuse version. Install macFUSE with: brew install --cask macfuse$(COLOR_RESET)"

build-windows-cgofuse: $(BUILD_DIR)
	@echo "$(COLOR_BLUE)Building cgofuse for Windows (requires WinFsp)...$(COLOR_RESET)"
	@echo "$(COLOR_YELLOW)Note: This requires WinFsp to be installed for compilation$(COLOR_RESET)"
	@CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
		go build $(LDFLAGS) -tags cgofuse,$(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64-cgofuse.exe ./cmd/objectfs || \
		echo "$(COLOR_RED)Failed to build Windows cgofuse version. Install WinFsp from: https://winfsp.dev/$(COLOR_RESET)"

# Run tests
test:
	@echo "$(COLOR_BLUE)Running tests...$(COLOR_RESET)"
	@go test -v ./...

# Run tests with race detection
test-race:
	@echo "$(COLOR_BLUE)Running tests with race detection...$(COLOR_RESET)"
	@go test -race -v ./...

# Run benchmarks
bench:
	@echo "$(COLOR_BLUE)Running benchmarks...$(COLOR_RESET)"
	@go test -bench=. -benchmem ./...

# Generate test coverage
coverage: $(COVERAGE_DIR)
	@echo "$(COLOR_BLUE)Generating test coverage...$(COLOR_RESET)"
	@go test -coverprofile=$(COVERAGE_DIR)/coverage.out ./...
	@go tool cover -func=$(COVERAGE_DIR)/coverage.out

# Generate HTML coverage report
coverage-html: coverage
	@echo "$(COLOR_BLUE)Generating HTML coverage report...$(COLOR_RESET)"
	@go tool cover -html=$(COVERAGE_DIR)/coverage.out -o $(COVERAGE_DIR)/coverage.html
	@echo "$(COLOR_GREEN)Coverage report: $(COVERAGE_DIR)/coverage.html$(COLOR_RESET)"

# Clean build artifacts
clean:
	@echo "$(COLOR_BLUE)Cleaning build artifacts...$(COLOR_RESET)"
	@rm -rf $(BIN_DIR) $(BUILD_DIR) $(DIST_DIR) $(COVERAGE_DIR)
	@go clean

# Install binary to GOPATH/bin
install: build
	@echo "$(COLOR_BLUE)Installing $(BINARY_NAME)...$(COLOR_RESET)"
	@go install $(LDFLAGS) -tags $(TAGS) ./cmd/objectfs

# Uninstall binary from GOPATH/bin
uninstall:
	@echo "$(COLOR_BLUE)Uninstalling $(BINARY_NAME)...$(COLOR_RESET)"
	@rm -f $(shell go env GOPATH)/bin/$(BINARY_NAME)

# Build Docker image
docker: docker-build

docker-build:
	@echo "$(COLOR_BLUE)Building Docker image...$(COLOR_RESET)"
	@docker build -t objectfs:$(VERSION) -t objectfs:latest .

# Push Docker image
docker-push:
	@echo "$(COLOR_BLUE)Pushing Docker image...$(COLOR_RESET)"
	@docker push objectfs:$(VERSION)
	@docker push objectfs:latest

# Create distribution packages
package: build-all $(DIST_DIR)
	@echo "$(COLOR_BLUE)Creating distribution packages...$(COLOR_RESET)"
	@for binary in $(BUILD_DIR)/$(BINARY_NAME)-*; do \
		if [ -f "$$binary" ]; then \
			name=$$(basename "$$binary"); \
			echo "Packaging $$name..."; \
			tar -czf $(DIST_DIR)/$$name.tar.gz -C $(BUILD_DIR) $$name; \
		fi \
	done
	@echo "$(COLOR_GREEN)Packages created in $(DIST_DIR)/$(COLOR_RESET)"

# Create release
release: clean check build-all package
	@echo "$(COLOR_GREEN)Release $(VERSION) ready!$(COLOR_RESET)"

# Integration tests (requires running infrastructure)
test-integration:
	@echo "$(COLOR_BLUE)Running integration tests...$(COLOR_RESET)"
	@go test -tags=integration -v ./test/integration/...

# Performance benchmarks
bench-performance:
	@echo "$(COLOR_BLUE)Running performance benchmarks...$(COLOR_RESET)"
	@go test -tags=benchmark -bench=. -benchmem -benchtime=10s ./test/benchmarks/...

# Generate documentation
docs:
	@echo "$(COLOR_BLUE)Generating documentation...$(COLOR_RESET)"
	@go doc -all ./... > docs/godoc.txt

# Verify dependencies
verify:
	@echo "$(COLOR_BLUE)Verifying dependencies...$(COLOR_RESET)"
	@go mod verify

# Update dependencies
update-deps:
	@echo "$(COLOR_BLUE)Updating dependencies...$(COLOR_RESET)"
	@go get -u ./...
	@go mod tidy

# Validate semantic versioning
validate-version:
	@echo "$(COLOR_BLUE)Validating semantic version...$(COLOR_RESET)"
	@if [ -z "$(VERSION)" ]; then \
		echo "$(COLOR_RED)VERSION is not set$(COLOR_RESET)"; \
		exit 1; \
	fi
	@if [[ ! $(VERSION) =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?(\+[a-zA-Z0-9.-]+)?$$ ]]; then \
		echo "$(COLOR_RED)Invalid semantic version: $(VERSION)$(COLOR_RESET)"; \
		exit 1; \
	fi
	@echo "$(COLOR_GREEN)Valid semantic version: $(VERSION)$(COLOR_RESET)"

# Validate changelog format
validate-changelog:
	@echo "$(COLOR_BLUE)Validating CHANGELOG.md format...$(COLOR_RESET)"
	@if ! grep -q "## \[Unreleased\]" CHANGELOG.md; then \
		echo "$(COLOR_RED)CHANGELOG.md missing [Unreleased] section$(COLOR_RESET)"; \
		exit 1; \
	fi
	@if ! grep -q "### Added\|### Changed\|### Deprecated\|### Removed\|### Fixed\|### Security" CHANGELOG.md; then \
		echo "$(COLOR_RED)CHANGELOG.md missing required sections (Added, Changed, etc.)$(COLOR_RESET)"; \
		exit 1; \
	fi
	@echo "$(COLOR_GREEN)CHANGELOG.md format is valid$(COLOR_RESET)"

# Check for breaking changes in API
check-breaking-changes:
	@echo "$(COLOR_BLUE)Checking for API breaking changes...$(COLOR_RESET)"
	@if command -v gorelease >/dev/null 2>&1; then \
		gorelease -base=$(shell git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0") -version=$(VERSION); \
	else \
		echo "$(COLOR_YELLOW)gorelease not found, skipping API compatibility check$(COLOR_RESET)"; \
	fi

# Development workflow with pre-commit hooks
setup-hooks:
	@echo "$(COLOR_BLUE)Setting up development hooks...$(COLOR_RESET)"
	@if [ ! -f ".git/hooks/pre-commit" ] || [ ! -f ".pre-commit-config.yaml" ]; then \
		./scripts/setup-hooks.sh; \
	else \
		echo "$(COLOR_GREEN)Hooks already configured$(COLOR_RESET)"; \
	fi

# Run pre-commit hooks manually
pre-commit-run:
	@echo "$(COLOR_BLUE)Running pre-commit hooks on staged files...$(COLOR_RESET)"
	@pre-commit run

# Run pre-commit hooks on all files
pre-commit-all:
	@echo "$(COLOR_BLUE)Running pre-commit hooks on all files...$(COLOR_RESET)"
	@pre-commit run --all-files

# Solo dev workflow - comprehensive local checks
dev-check: setup-hooks pre-commit-all
	@echo "$(COLOR_GREEN)Development checks completed!$(COLOR_RESET)"

# Pre-release validation
pre-release: validate-version validate-changelog check-breaking-changes check build-all test
	@echo "$(COLOR_GREEN)Pre-release validation passed!$(COLOR_RESET)"