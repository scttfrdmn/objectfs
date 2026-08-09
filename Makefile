# ObjectFS Makefile
# Enterprise-Grade High-Performance POSIX Filesystem for Object Storage

# Build information
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0-dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%I:%M:%S%p')
GO_VERSION := $(shell go version | cut -d ' ' -f 3)

# Build flags
#
# No -X: main.Version, main.Commit and main.BuildTime do not exist. cmd/objectfs/main.go declares an
# untyped `version` *constant*, which the linker cannot rewrite, so all three flags were accepted and
# silently ignored — `-X main.Version=FAKE` builds a binary that still reports the constant. Every
# target below shares this variable, so that was six targets passing three dead symbols.
#
# The constant stays the single authority, which is what CLAUDE.md asks for and what
# .github/workflows/release.yml already does: it asserts the tag and the constant agree rather than
# injecting a version the source then disagrees with. VERSION, COMMIT and BUILD_TIME above are still
# real — `make version` prints them and the packaging targets name archives with them; they just do
# not reach the binary.
LDFLAGS := -ldflags="-s -w"
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
.PHONY: test-race test-aws test-release-check
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
	@echo "  $(COLOR_GREEN)test$(COLOR_RESET)                Run all unit tests"
	@echo "  $(COLOR_GREEN)test-aws$(COLOR_RESET)            Run AWS S3 integration tests (requires AWS creds + OBJECTFS_TEST_BUCKET)"
	@echo "  $(COLOR_GREEN)test-release-check$(COLOR_RESET)  Unit + AWS integration tests (pre-release gate)"
	@echo "  $(COLOR_GREEN)bench$(COLOR_RESET)               Run benchmarks"
	@echo "  $(COLOR_GREEN)coverage$(COLOR_RESET)            Generate test coverage report"
	@echo "  $(COLOR_GREEN)lint$(COLOR_RESET)           Run linters"
	@echo "  $(COLOR_GREEN)fmt$(COLOR_RESET)            Format Go code"
	@echo "  $(COLOR_GREEN)vet$(COLOR_RESET)            Run go vet"
	@echo "  $(COLOR_GREEN)check$(COLOR_RESET)          Run all checks (fmt, vet, lint, test)"
	@echo "  $(COLOR_GREEN)deps$(COLOR_RESET)           Download and tidy dependencies"
	@echo "  $(COLOR_GREEN)clean$(COLOR_RESET)          Clean build artifacts"
	@echo "  $(COLOR_GREEN)install$(COLOR_RESET)        Install binary to GOPATH/bin"
	@echo "  $(COLOR_GREEN)docker$(COLOR_RESET)         Build Docker image"
	@echo "  $(COLOR_GREEN)package$(COLOR_RESET)        Create distribution tarballs"
	@echo "  $(COLOR_GREEN)package-linux$(COLOR_RESET)  Create .deb and .rpm packages (nfpm, amd64 + arm64)"
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

# Create necessary directories.
#
# `%/.mkdir` rather than the directory names themselves. `BUILD_DIR := build` and
# `COVERAGE_DIR := coverage` made the old rule read `bin build dist coverage:`, which collides with
# the real `build` and `coverage` targets below — so make printed four warnings on *every*
# invocation, including `make build` and `make help`:
#
#   Makefile:129: warning: overriding commands for target `build'
#   Makefile:96: warning: ignoring old commands for target `build'
#
# The build still worked, because the later recipe wins and both targets are .PHONY, so the
# directory recipe was discarded and `mkdir -p` happened to be unnecessary for `build` (go build
# creates the parent of -o). But a build system that opens with four warnings reads as unmaintained,
# and the two names would have genuinely collided the moment one stopped being .PHONY.
#
# A sentinel file per directory keeps the rule pattern-based and out of the target namespace
# entirely. Order-only (`| $(BIN_DIR)/.mkdir`) is what callers want: the directory's mtime changes
# whenever anything is written into it, and a normal prerequisite would rebuild every binary each
# time a sibling was built.
%/.mkdir:
	@mkdir -p $(@D)
	@touch $@

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
build: | $(BIN_DIR)/.mkdir
	@echo "$(COLOR_BLUE)Building $(BINARY_NAME) $(VERSION) for $(GOOS)/$(GOARCH)...$(COLOR_RESET)"
	@CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build $(LDFLAGS) -tags $(TAGS) -o $(BINARY_PATH) ./cmd/objectfs
	@echo "$(COLOR_GREEN)Binary built: $(BINARY_PATH)$(COLOR_RESET)"

# Build debug binary
build-debug: | $(BIN_DIR)/.mkdir
	@echo "$(COLOR_BLUE)Building debug binary...$(COLOR_RESET)"
	@go build -tags $(DEBUG_TAGS) -o $(BIN_DIR)/$(BINARY_NAME)-debug ./cmd/objectfs

# Build binary with race detection
build-race: | $(BIN_DIR)/.mkdir
	@echo "$(COLOR_BLUE)Building race detection binary...$(COLOR_RESET)"
	@go build $(RACE_FLAGS) -o $(BIN_DIR)/$(BINARY_NAME)-race ./cmd/objectfs

# Build for every supported platform.
#
# Windows is absent deliberately: the FUSE layer is constrained to linux || darwin and there is no
# Windows binding. Do not add a windows target without a mount implementation that CI exercises.
build-all: build-linux build-darwin

# Build for Linux
build-linux: | $(BUILD_DIR)/.mkdir
	@echo "$(COLOR_BLUE)Building for Linux...$(COLOR_RESET)"
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build $(LDFLAGS) -tags $(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/objectfs
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build $(LDFLAGS) -tags $(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/objectfs

# Build for macOS
build-darwin: | $(BUILD_DIR)/.mkdir
	@echo "$(COLOR_BLUE)Building for macOS...$(COLOR_RESET)"
	@CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 \
		go build $(LDFLAGS) -tags $(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/objectfs
	@CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
		go build $(LDFLAGS) -tags $(TAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/objectfs

# Run tests, with the race detector — the same flags CI uses, so a green `make test`
# means something. It previously omitted -race while CLAUDE.md and CONTRIBUTING.md both
# said every test runs with it, which made the local gate weaker than the one it stood in
# for: sixteen concurrency bugs were filed in this repository after a document declared it
# race-free, and the detector is what found most of them.
test:
	@echo "$(COLOR_BLUE)Running tests with race detection...$(COLOR_RESET)"
	@go test -race -timeout 20m ./...

# Kept as an alias: scripts and muscle memory refer to it, and it now describes what
# `test` already does rather than a stricter mode.
test-race: test

# Run benchmarks
bench:
	@echo "$(COLOR_BLUE)Running benchmarks...$(COLOR_RESET)"
	@go test -bench=. -benchmem ./...

# Generate test coverage
coverage: | $(COVERAGE_DIR)/.mkdir
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
package: build-all | $(DIST_DIR)/.mkdir
	@echo "$(COLOR_BLUE)Creating distribution packages...$(COLOR_RESET)"
	@for binary in $(BUILD_DIR)/$(BINARY_NAME)-*; do \
		if [ -f "$$binary" ]; then \
			name=$$(basename "$$binary"); \
			echo "Packaging $$name..."; \
			tar -czf $(DIST_DIR)/$$name.tar.gz -C $(BUILD_DIR) $$name; \
		fi \
	done
	@echo "$(COLOR_GREEN)Packages created in $(DIST_DIR)/$(COLOR_RESET)"

# Build the Linux .deb and .rpm packages, via nfpm and nfpm.yaml.
#
# This is the target #207 is about. Before it, `scripts/preremove.sh` was a working uninstall script
# that nothing in the repository referenced — only a package manager can invoke a pre-removal hook,
# and there was no packaging system in the tree at all. `package` above makes tarballs, which have no
# scriptlets and no maintainer scripts.
#
# The version comes out of cmd/objectfs/main.go with the same sed expression
# .github/workflows/release.yml uses to check a tag against it. Not $(VERSION): that is `git describe`,
# which on a dirty tree or an untagged commit produces things like v0.12.0-14-gabc123-dirty, and
# neither dpkg nor rpm accepts a hyphenated version in that position. The constant is the authority
# CLAUDE.md names, and internal/config/packaging_test.go fails if a literal version ever appears in
# nfpm.yaml.
#
# Four packages: {deb, rpm} × {amd64, arm64}. Both formats from one config, which is why nfpm rather
# than a debian/ directory plus a .spec — two hand-maintained descriptions of the same install layout
# is two places for it to drift.
NFPM_VERSION := v2.47.0
NFPM ?= $(shell command -v nfpm 2>/dev/null || echo $(shell go env GOPATH)/bin/nfpm)
PKG_VERSION := $(shell sed -n 's/^[[:space:]]*version = "\(.*\)"/\1/p' cmd/objectfs/main.go)

.PHONY: package-linux
package-linux: build-linux | $(DIST_DIR)/.mkdir
	@if [ -z "$(PKG_VERSION)" ]; then \
		echo "$(COLOR_RED)could not read the version constant from cmd/objectfs/main.go$(COLOR_RESET)"; \
		exit 1; \
	fi
	@if [ ! -x "$(NFPM)" ]; then \
		echo "$(COLOR_YELLOW)nfpm not found; installing $(NFPM_VERSION)...$(COLOR_RESET)"; \
		go install github.com/goreleaser/nfpm/v2/cmd/nfpm@$(NFPM_VERSION); \
	fi
	@echo "$(COLOR_BLUE)Building deb and rpm packages for objectfs $(PKG_VERSION)...$(COLOR_RESET)"
	@for arch in amd64 arm64; do \
		for format in deb rpm; do \
			OBJECTFS_VERSION=$(PKG_VERSION) OBJECTFS_ARCH=$$arch \
				$(NFPM) package --config nfpm.yaml --packager $$format --target $(DIST_DIR)/ || exit 1; \
		done; \
	done
	@echo "$(COLOR_GREEN)Packages created in $(DIST_DIR)/$(COLOR_RESET)"

# Create release
release: clean check build-all package
	@echo "$(COLOR_GREEN)Release $(VERSION) ready!$(COLOR_RESET)"

# There was a `test-integration` target here running `go test -tags=integration ./test/integration/...`.
# That directory has never existed — the target failed with `lstat ./test/integration/: no such file
# or directory` for as long as git records. The `integration` tag's only files lived in `tests/`
# (note the s) and were a LocalStack suite, deleted in v0.15.0 along with this target (#240). The
# `integration` tag still marks real-AWS tests inside packages: `make test-aws` and the commands in
# CONTRIBUTING.md run those.

# AWS S3 integration tests — requires real AWS credentials and a test bucket.
#
# Usage:
#   export AWS_ACCESS_KEY_ID=...
#   export AWS_SECRET_ACCESS_KEY=...
#   export OBJECTFS_TEST_BUCKET=objectfs-test-<yourname>
#   export AWS_REGION=us-east-1          # optional, defaults to us-east-1
#   make test-aws
#
# Runs the full suite:
#   - internal/storage/s3 aws_s3 tests  (basic ops, list, multipart, ZSTD compression)
#   - sdks/go/objectfs integration tests (New, Get, Put, Delete, List, Head, Health)
test-aws:
	@if [ -z "$$AWS_ACCESS_KEY_ID" ] && [ -z "$$AWS_PROFILE" ]; then \
		echo "$(COLOR_RED)Error: no AWS credentials found$(COLOR_RESET)"; \
		echo "  Set AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY, or AWS_PROFILE"; \
		exit 1; \
	fi
	@if [ -z "$$OBJECTFS_TEST_BUCKET" ]; then \
		echo "$(COLOR_RED)Error: OBJECTFS_TEST_BUCKET not set$(COLOR_RESET)"; \
		echo "  Example: export OBJECTFS_TEST_BUCKET=objectfs-test-$$(whoami)"; \
		exit 1; \
	fi
	@echo "$(COLOR_BLUE)Running AWS S3 integration tests against bucket: $$OBJECTFS_TEST_BUCKET$(COLOR_RESET)"
	@echo "$(COLOR_BLUE)  Region: $${AWS_REGION:-us-east-1}$(COLOR_RESET)"
	@echo ""
	@echo "$(COLOR_BOLD)[1/2] internal/storage/s3 (aws_s3 build tag)$(COLOR_RESET)"
	@go test -race -tags=aws_s3 -v -count=1 -timeout=300s ./tests/... 2>&1 | tee /tmp/objectfs-aws-s3.log
	@echo ""
	@echo "$(COLOR_BOLD)[2/2] sdks/go/objectfs integration tests$(COLOR_RESET)"
	@go test -race -v -count=1 -timeout=120s \
		-run 'TestNew_WithDefaults|TestNew_WithRegion|TestClose_NotMounted|TestIntegration_' \
		./sdks/go/objectfs/... 2>&1 | tee /tmp/objectfs-sdk-go.log
	@echo ""
	@echo "$(COLOR_GREEN)AWS integration test run complete. Logs:$(COLOR_RESET)"
	@echo "  /tmp/objectfs-aws-s3.log"
	@echo "  /tmp/objectfs-sdk-go.log"

# Pre-release integration check for v0.6.0.
# Runs test-aws plus the local unit test suite with the race detector.
test-release-check: test test-aws
	@echo "$(COLOR_GREEN)Release check complete — unit + AWS integration tests passed.$(COLOR_RESET)"

# Kernel-observable FUSE behavior. Needs /dev/fuse, so it is not part of `make test`.
#
# These are the tests that cannot be written without a mount: whether the kernel re-reads under
# direct I/O, whether it keeps cached pages across open(2). Everything up to the kernel boundary is
# gated by the ordinary suite; this is the half a userspace test cannot reach.
#
# The tag is compile-checked by CI even where it cannot run (see the build-tags job), because a build
# tag nothing compiles is how four of them came to carry broken code — issue #240.
test-fuse-mount:
	@echo "$(COLOR_BLUE)Running kernel-observable FUSE tests (requires /dev/fuse)...$(COLOR_RESET)"
	@go test -race -tags=fuse_mount -run 'TestDirectIO|TestKeepCache' ./internal/fuse/

# Third-party POSIX conformance, via pjdfstest. ON DEMAND ONLY — no CI job runs this, and none can:
# it needs /dev/fuse, real AWS credentials and a real bucket, and this repository has no scheduled
# real-AWS job at all (the only cron: in the tree belongs to the security scan). Adding one means a
# bucket and a role, which is a decision with a cost attached — issue #352.
#
# Said out loud here and in the script's header because the failure mode of an unrun conformance suite
# is that it reads as a passing one. `make test-fuse-mount` above is the same situation for the same
# reason, and internal/difftest is what does run: a differential oracle over an operation sequence
# this repository chose, which is a weaker claim than a suite nobody here wrote.
#
# Requires: pjdfstest in PATH, OBJECTFS_TEST_BUCKET set. `build` runs first via the prerequisite.
# ObjectFS is not POSIX-compliant, so a clean run is not the goal — README.md's supported-operations
# table is what to read the results against, and the useful question is whether the failing set grew.
test-posix: build
	@echo "$(COLOR_BLUE)Running pjdfstest POSIX compliance suite...$(COLOR_RESET)"
	@scripts/pjdfstest.sh

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
