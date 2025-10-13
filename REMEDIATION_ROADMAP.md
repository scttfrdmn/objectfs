# ObjectFS Remediation Roadmap

**Document Version**: 1.0
**Created**: 2025-10-13
**Status**: In Progress
**Owner**: Development Team

---

## 🎯 Executive Summary

This roadmap addresses critical CI/CD failures, test coverage gaps, dependency updates, and technical debt discovered during the October 2025 audit. The project currently has:

- ❌ **CI Pipeline**: Failing on all Dependabot PRs
- ⚠️ **Test Coverage**: ~40% actual (claimed 95%)
- ⚠️ **Blocked Updates**: 6+ dependency PRs stuck
- ⚠️ **Security Scans**: Failing due to deprecated action

**Target Completion**: 3-4 weeks
**Expected Outcome**: Healthy CI/CD, 90%+ test coverage, up-to-date dependencies

---

## 🔴 Critical Issues Identified

### Issue #1: Build Failures in Cache Module

**Status**: 🔴 BLOCKING
**Severity**: CRITICAL
**Impact**: All CI builds failing

**Error**:

```
internal/cache/multilevel.go:303:14: undefined: NewLRUCache
internal/cache/multilevel.go:303:27: undefined: CacheConfig
internal/cache/multilevel.go:346:19: undefined: NewPersistentCache
internal/cache/multilevel.go:346:39: undefined: PersistentCacheConfig
```

**Root Cause**: Functions exist in local codebase but may be missing or not exported in Dependabot PR branches.

**Resolution Steps**:

- [ ] Checkout PR #41 branch and verify file state
- [ ] Check for circular dependencies or import cycles
- [ ] Verify all functions are properly exported (capitalized)
- [ ] Ensure package declarations are consistent
- [ ] Test build locally before pushing fix

**Assignee**: TBD
**Due Date**: Day 1
**Estimated Effort**: 2-3 hours

---

### Issue #2: Security Workflow Failure

**Status**: 🔴 BLOCKING
**Severity**: CRITICAL
**Impact**: Security scans not running

**Error**:

```
##[error]Unable to resolve action securecodewarrior/github-action-gosec, repository not found
```

**Root Cause**: GitHub Action `securecodewarrior/github-action-gosec` is deprecated/removed.

**Resolution Steps**:

- [ ] Update `.github/workflows/ci.yml` to use direct gosec installation
- [ ] Update `.github/workflows/security.yml` similarly
- [ ] Test workflow in a PR
- [ ] Verify SARIF upload still works

**Fix Code**:

```yaml
# Replace lines 76-79 in ci.yml
- name: Run gosec Security Scanner
  run: |
    go install github.com/securecodewarrior/gosec/v2/cmd/gosec@latest
    gosec -fmt sarif -out gosec.sarif -no-fail ./...

- name: Upload Gosec SARIF
  uses: github/codeql-action/upload-sarif@v3
  with:
    sarif_file: gosec.sarif
```

**Assignee**: TBD
**Due Date**: Day 1
**Estimated Effort**: 30 minutes

---

### Issue #3: Go Version Inconsistencies

**Status**: 🟡 HIGH PRIORITY
**Severity**: HIGH
**Impact**: Confusing toolchain behavior, potential compatibility issues

**Current State**:

- `go.mod`: go 1.23.0
- `toolchain`: go1.24.5
- CI matrix: 1.21.x, 1.22.x, 1.23.x
- Dockerfile: golang:1.21-alpine
- Dependabot wants: golang:1.25-alpine

**Target State**:

- `go.mod`: go 1.23.2
- `toolchain`: Remove or set to go1.23.12
- CI matrix: 1.22.x, 1.23.x, 1.24.x
- Dockerfile: golang:1.23-alpine
- Security workflow: GO_VERSION: '1.23'

**Resolution Steps**:

- [ ] Update go.mod: `go mod edit -go=1.23.2`
- [ ] Update or remove toolchain directive
- [ ] Update CI matrix in `.github/workflows/ci.yml`
- [ ] Update Dockerfile base image
- [ ] Update security.yml GO_VERSION
- [ ] Test builds on all versions

**Assignee**: TBD
**Due Date**: Day 2
**Estimated Effort**: 1 hour

---

### Issue #4: Missing Test Coverage

**Status**: 🟡 HIGH PRIORITY
**Severity**: HIGH
**Impact**: Claimed 95% coverage, actual ~40%

**Packages with 0% Coverage**:

- `cmd/objectfs/` - CLI tests
- `internal/adapter/` - Integration tests
- `internal/batch/` - Batch processing
- `internal/buffer/` - Buffer management
- `internal/cache/` - **CRITICAL** - Cache system tests
- `internal/circuit/` - Circuit breaker tests
- `internal/distributed/` - Distributed system tests
- `internal/filesystem/` - Filesystem interface
- `internal/fuse/` - FUSE operations
- `internal/health/` - Health checks
- `internal/metrics/` - Metrics collection

**Current Coverage**:

- `internal/config`: 84.4%
- `internal/storage/s3`: 42.3%
- `pkg/utils`: 78.6%
- **Overall**: ~40%

**Target Coverage**: 90%+

**Resolution Phases**:

- [ ] Phase 1: Cache tests (Days 4-5) - 8 hours
- [ ] Phase 2: Core internal packages (Days 6-7) - 8 hours
- [ ] Phase 3: Integration tests (Week 2) - 8 hours
- [ ] Phase 4: Edge cases and remaining packages (Week 3) - 8 hours

**Assignee**: TBD
**Due Date**: End of Week 2
**Estimated Effort**: 32 hours total

---

### Issue #5: Blocked AWS SDK Updates

**Status**: 🟡 HIGH PRIORITY
**Severity**: HIGH
**Impact**: Security vulnerabilities may be unpatched

**Blocked Dependabot PRs**:

- PR #41: aws-sdk-go-v2/service/s3 v1.82.0 → v1.88.4
- PR #40: aws-sdk-go-v2/config v1.29.17 → v1.31.12
- PR #38: aws-sdk-go-v2 v1.36.5 → v1.39.2
- PR #30: prometheus/client_golang v1.22.0 → v1.23.2
- PR #29: actions/setup-go v4 → v6
- PR #25: docker golang 1.21-alpine → 1.25-alpine

**Version Gaps** (current → available):

```
aws-sdk-go-v2: v1.36.5 → v1.39.2 (3 minor versions)
aws-sdk-go-v2/config: v1.29.17 → v1.31.12 (2 minor versions)
aws-sdk-go-v2/service/s3: v1.82.0 → v1.88.4 (6 minor versions)
```

**Resolution Strategy**: Staged Updates

- [ ] Stage 1: Update core SDK (Day 8)
- [ ] Stage 2: Update config package (Day 9)
- [ ] Stage 3: Update S3 service (Day 9)
- [ ] Stage 4: Test with real AWS S3 (Day 10)
- [ ] Stage 5: Merge Dependabot PRs (Day 10)

**Breaking Changes to Monitor**:

- Error type modifications
- Context handling updates
- Credential provider changes
- Retry policy modifications

**Assignee**: TBD
**Due Date**: End of Week 2
**Estimated Effort**: 8 hours

---

## 📅 Sprint Planning

### Sprint 1: Critical Fixes (Days 1-3)

#### Day 1 - Emergency Repairs

**Goal**: Fix blocking CI issues

**Tasks**:

- [ ] 🔴 Fix security workflow action (30 min)
  - Update ci.yml with direct gosec install
  - Update security.yml
  - Test in PR
  - Merge to main

- [ ] 🔴 Investigate cache build failure on PR #41 (2 hours)
  - Checkout PR branch
  - Verify file existence and exports
  - Check for import cycles
  - Identify root cause

- [ ] 🔴 Implement and test cache fix (1 hour)
  - Apply fix locally
  - Run full test suite
  - Push and verify CI

**Success Criteria**:

- ✅ Security workflow runs successfully
- ✅ Cache build issue identified
- ✅ Fix tested locally

**Status**: ⏳ Not Started

---

#### Day 2 - Version Alignment

**Goal**: Standardize Go versions across project

**Tasks**:

- [ ] 🟡 Update go.mod and toolchain (20 min)

  ```bash
  go mod edit -go=1.23.2
  go mod edit -droprequire toolchain
  go mod tidy
  ```

- [ ] 🟡 Update CI workflow matrix (20 min)
  - Edit `.github/workflows/ci.yml`
  - Change to: `[1.22.x, 1.23.x, 1.24.x]`

- [ ] 🟡 Update Dockerfile (10 min)
  - Change base: `FROM golang:1.23-alpine`

- [ ] 🟡 Update security workflow (10 min)
  - Set `GO_VERSION: '1.23'`

- [ ] 🟡 Test builds on all versions (30 min)
  - Run locally with different Go versions
  - Verify CI passes

- [ ] 🟡 Merge cache fix from Day 1 (30 min)

**Success Criteria**:

- ✅ Go version consistent across all config files
- ✅ CI builds successfully on all Go versions
- ✅ Cache fix merged

**Status**: ⏳ Not Started

---

#### Day 3 - Validation & PR Cleanup

**Goal**: Verify fixes and start clearing Dependabot backlog

**Tasks**:

- [ ] ✅ Verify CI passes on main branch (30 min)
  - Check all workflows
  - Confirm security scans running
  - Verify build succeeds

- [ ] 🟢 Close and reopen Dependabot PRs (30 min)
  - Trigger fresh CI runs
  - Monitor for new failures

- [ ] 🟢 Review and merge safe PRs (1 hour)
  - PR #29: actions/setup-go v4 → v6
  - Any other non-AWS updates

- [ ] 📝 Document fixes in changelog (30 min)

**Success Criteria**:

- ✅ Main branch CI fully green
- ✅ At least 2 Dependabot PRs merged
- ✅ No blocking issues remain

**Status**: ⏳ Not Started

---

### Sprint 2: Test Coverage (Days 4-7)

#### Days 4-5 - Core Package Tests

**Goal**: Test cache, adapter, and buffer packages

**Tasks**:

- [ ] 🧪 Create `internal/cache/lru_test.go` (2 hours)
  - Test cache creation with various configs
  - Test Put/Get operations
  - Test eviction policies
  - Test TTL expiration
  - Test concurrent access

- [ ] 🧪 Create `internal/cache/persistent_test.go` (2 hours)
  - Test disk cache creation
  - Test compression
  - Test checksum validation
  - Test index persistence
  - Test cleanup routines

- [ ] 🧪 Create `internal/cache/multilevel_test.go` (2 hours)
  - Test multi-level hierarchy
  - Test promotion between levels
  - Test inclusive/exclusive policies
  - Test statistics aggregation

- [ ] 🧪 Create `internal/adapter/adapter_test.go` (2 hours)
  - Test adapter initialization
  - Test backend integration
  - Test mount/unmount operations

**Success Criteria**:

- ✅ Cache package coverage > 80%
- ✅ Adapter package coverage > 70%
- ✅ All tests passing

**Status**: ⏳ Not Started

---

#### Days 6-7 - System Tests

**Goal**: Test FUSE, health, metrics, and circuit breaker

**Tasks**:

- [ ] 🧪 Create `internal/fuse/filesystem_test.go` (2 hours)
  - Test FUSE operation handlers
  - Test file operations (read/write/delete)
  - Test directory operations
  - Mock backend interactions

- [ ] 🧪 Create `internal/circuit/breaker_test.go` (1 hour)
  - Test circuit states (closed/open/half-open)
  - Test failure thresholds
  - Test timeout behavior
  - Test recovery

- [ ] 🧪 Create `internal/health/monitor_test.go` (1 hour)
  - Test health check execution
  - Test status reporting
  - Test failure detection

- [ ] 🧪 Create `internal/metrics/collector_test.go` (1 hour)
  - Test metric collection
  - Test Prometheus export
  - Test aggregation

- [ ] 🧪 Create `internal/buffer/manager_test.go` (1 hour)
  - Test buffer allocation
  - Test pooling
  - Test concurrent access

- [ ] 🧪 Create `internal/batch/processor_test.go` (1 hour)
  - Test batch operations
  - Test error handling
  - Test concurrency limits

**Success Criteria**:

- ✅ FUSE package coverage > 70%
- ✅ Health/metrics/circuit packages > 80%
- ✅ Buffer/batch packages > 80%
- ✅ Overall coverage > 70%

**Status**: ⏳ Not Started

---

### Sprint 3: AWS SDK Upgrade (Days 8-10)

#### Day 8 - Core SDK Update

**Goal**: Update core AWS SDK without breaking changes

**Tasks**:

- [ ] 🔄 Create upgrade branch (5 min)

  ```bash
  git checkout -b feature/aws-sdk-upgrade
  ```

- [ ] 🔄 Update core SDK (30 min)

  ```bash
  go get github.com/aws/aws-sdk-go-v2@v1.39.2
  go mod tidy
  ```

- [ ] 🧪 Run full test suite (30 min)

  ```bash
  go test -race -cover ./...
  ```

- [ ] 🔍 Review breaking changes (1 hour)
  - Check AWS SDK changelog
  - Identify deprecated methods
  - Review error handling changes

- [ ] 🛠️ Fix any breaking changes (1 hour)
  - Update error type handling
  - Fix deprecated method calls
  - Update context usage if needed

**Success Criteria**:

- ✅ Core SDK updated to v1.39.2
- ✅ All tests passing
- ✅ No regressions

**Status**: ⏳ Not Started

---

#### Day 9 - Config & S3 Updates

**Goal**: Update config and S3 service packages

**Tasks**:

- [ ] 🔄 Update config package (1 hour)

  ```bash
  go get github.com/aws/aws-sdk-go-v2/config@v1.31.12
  go mod tidy
  go test ./internal/storage/s3/...
  ```

- [ ] 🔄 Update S3 service (1 hour)

  ```bash
  go get github.com/aws/aws-sdk-go-v2/service/s3@v1.88.4
  go mod tidy
  go test ./internal/storage/s3/...
  ```

- [ ] 🛠️ Fix S3-specific breaking changes (2 hours)
  - Update PutObject calls
  - Update GetObject calls
  - Update ListObjects calls
  - Update error handling
  - Test with mocked S3

- [ ] 🔄 Update related packages (30 min)

  ```bash
  go get -u github.com/aws/aws-sdk-go-v2/...
  go mod tidy
  ```

**Success Criteria**:

- ✅ Config updated to v1.31.12
- ✅ S3 service updated to v1.88.4
- ✅ All S3 backend tests passing
- ✅ No compilation errors

**Status**: ⏳ Not Started

---

#### Day 10 - Integration Testing & PR Merge

**Goal**: Validate with real AWS and merge updates

**Tasks**:

- [ ] 🧪 Test with real AWS S3 (2 hours)
  - Create test bucket
  - Run integration tests
  - Test all storage tiers
  - Test CargoShip integration
  - Verify cost optimizer

- [ ] 📝 Update documentation (1 hour)
  - Update README if needed
  - Update CHANGELOG.md
  - Document any breaking changes
  - Update dependency versions

- [ ] 🔄 Create PR and merge (1 hour)
  - Push upgrade branch
  - Create PR with detailed description
  - Wait for CI
  - Merge to main

- [ ] ✅ Merge Dependabot PRs (1 hour)
  - Close now-outdated PRs
  - Re-run any that are still relevant
  - Merge successfully passing PRs

**Success Criteria**:

- ✅ AWS SDK fully updated and tested
- ✅ Integration tests passing
- ✅ All Dependabot PRs resolved
- ✅ Documentation updated

**Status**: ⏳ Not Started

---

### Sprint 4: CI/CD Polish (Days 11-14)

#### Days 11-12 - CI Improvements

**Goal**: Optimize CI pipeline and add automation

**Tasks**:

- [ ] ⚡ Implement advanced caching (1 hour)

  ```yaml
  - uses: actions/cache@v4
    with:
      path: |
        ~/go/pkg/mod
        ~/.cache/go-build
      key: ${{ runner.os }}-go-${{ hashFiles('**/go.sum') }}
  ```

- [ ] 🔒 Add pre-merge validation (2 hours)
  - Create `.github/workflows/pr-checks.yml`
  - Add build verification
  - Add breaking change detection
  - Add license compliance check

- [ ] 🤖 Setup auto-merge for Dependabot (1 hour)
  - Create auto-merge workflow
  - Configure for patch/minor updates only
  - Require all checks to pass

- [ ] 🧪 Add LocalStack integration to CI (2 hours)
  - Add LocalStack service to CI
  - Enable integration tests in CI
  - Tag tests appropriately

**Success Criteria**:

- ✅ CI pipeline < 5 minutes
- ✅ Pre-merge checks prevent broken code
- ✅ Dependabot auto-merges safe updates
- ✅ Integration tests run in CI

**Status**: ⏳ Not Started

---

#### Days 13-14 - Documentation

**Goal**: Comprehensive project documentation

**Tasks**:

- [ ] 📖 Create `.github/README.md` (1 hour)
  - Document all workflows
  - Explain CI/CD pipeline
  - Local testing instructions
  - Troubleshooting guide

- [ ] 📖 Create `docs/TROUBLESHOOTING.md` (2 hours)
  - Common CI failures
  - Local debugging
  - AWS credential setup
  - Performance issues

- [ ] 📖 Create `docs/TESTING.md` (1 hour)
  - Testing strategy
  - Running tests locally
  - Writing new tests
  - Coverage requirements

- [ ] 📖 Update `CONTRIBUTING.md` (1 hour)
  - Add CI/CD requirements
  - Add testing requirements
  - Add pre-commit hook info

- [ ] 🎨 Create architecture diagrams (2 hours)
  - System architecture
  - Cache hierarchy
  - CI/CD pipeline
  - Deployment flow

- [ ] 📝 Update CHANGELOG.md (30 min)
  - Document all fixes
  - Note breaking changes
  - Credit contributors

**Success Criteria**:

- ✅ Comprehensive CI/CD documentation
- ✅ Clear troubleshooting guides
- ✅ Visual architecture diagrams
- ✅ Updated CHANGELOG

**Status**: ⏳ Not Started

---

## 📊 Progress Tracking

### Overall Progress: 0% Complete

```
[░░░░░░░░░░░░░░░░░░░░] 0/60 tasks completed
```

### Sprint Status

| Sprint | Status | Progress | Due Date |
|--------|--------|----------|----------|
| Sprint 1: Critical Fixes | ⏳ Not Started | 0/12 tasks | Day 3 |
| Sprint 2: Test Coverage | ⏳ Not Started | 0/17 tasks | Day 7 |
| Sprint 3: AWS SDK Upgrade | ⏳ Not Started | 0/14 tasks | Day 10 |
| Sprint 4: CI/CD Polish | ⏳ Not Started | 0/17 tasks | Day 14 |

### Issue Resolution Status

| Issue | Priority | Status | Progress |
|-------|----------|--------|----------|
| Cache Build Failures | 🔴 CRITICAL | ⏳ Not Started | 0% |
| Security Workflow | 🔴 CRITICAL | ⏳ Not Started | 0% |
| Go Version Inconsistencies | 🟡 HIGH | ⏳ Not Started | 0% |
| Missing Test Coverage | 🟡 HIGH | ⏳ Not Started | 0% |
| Blocked AWS SDK Updates | 🟡 HIGH | ⏳ Not Started | 0% |

### Coverage Progress

| Package | Current | Target | Status |
|---------|---------|--------|--------|
| internal/cache | 0% | 80% | ⏳ |
| internal/adapter | 0% | 70% | ⏳ |
| internal/storage/s3 | 42.3% | 85% | ⏳ |
| internal/config | 84.4% | 90% | ✅ |
| internal/fuse | 0% | 70% | ⏳ |
| internal/health | 0% | 80% | ⏳ |
| internal/metrics | 0% | 80% | ⏳ |
| **Overall** | **~40%** | **90%** | ⏳ |

### Dependabot PRs

| PR | Package | Status | Action |
|----|---------|--------|--------|
| #41 | aws-sdk-go-v2/service/s3 v1.88.4 | 🔴 Failing | Fix build issue |
| #40 | aws-sdk-go-v2/config v1.31.12 | 🔴 Failing | Fix build issue |
| #38 | aws-sdk-go-v2 v1.39.2 | 🔴 Failing | Fix build issue |
| #30 | prometheus/client_golang v1.23.2 | 🟡 Open | Review & merge |
| #29 | actions/setup-go v6 | 🟡 Open | Review & merge |
| #25 | docker golang 1.25-alpine | 🟡 Open | Update to 1.23 |

---

## 🎯 Success Metrics

### Sprint 1 Success Criteria

- ✅ All CI workflows passing on main branch
- ✅ Security scans running successfully
- ✅ Cache build issue resolved
- ✅ Go versions standardized
- ✅ At least 2 Dependabot PRs merged

### Sprint 2 Success Criteria

- ✅ Test coverage > 75%
- ✅ All critical packages have tests
- ✅ Integration tests implemented
- ✅ CI includes test coverage reporting

### Sprint 3 Success Criteria

- ✅ AWS SDK updated to latest versions
- ✅ All Dependabot PRs resolved
- ✅ Integration tests passing with real AWS
- ✅ No regressions introduced

### Sprint 4 Success Criteria

- ✅ CI pipeline < 5 minutes
- ✅ Automated Dependabot merging working
- ✅ Comprehensive documentation complete
- ✅ Architecture diagrams published

### Overall Project Success

- ✅ Test coverage > 90%
- ✅ Zero failing CI workflows
- ✅ Zero open Dependabot PRs
- ✅ All dependencies up-to-date
- ✅ Security scans passing
- ✅ Documentation complete and current

---

## 🛠️ Quick Reference Commands

### Local Testing

```bash
# Run all tests with coverage
go test -race -coverprofile=coverage.out -covermode=atomic ./...

# View coverage report
go tool cover -html=coverage.out

# Run specific package tests
go test -v ./internal/cache/...

# Run with race detection
go test -race ./...

# Run integration tests
go test -tags=integration ./tests/integration/...
```

### CI/CD Commands

```bash
# Trigger CI locally (act)
act -j test

# Check workflow syntax
actionlint .github/workflows/*.yml

# View recent workflow runs
gh run list --limit 20

# View specific workflow run
gh run view <run-id>

# Re-run failed workflows
gh run rerun <run-id>
```

### Dependency Management

```bash
# Update go dependencies
go get -u ./...
go mod tidy

# Check for vulnerabilities
govulncheck ./...

# Update specific package
go get github.com/aws/aws-sdk-go-v2@v1.39.2

# List available updates
go list -u -m all
```

### Security Scanning

```bash
# Run gosec locally
gosec ./...

# Run with SARIF output
gosec -fmt sarif -out gosec.sarif ./...

# Run Trivy locally (requires Docker)
docker run --rm -v $(pwd):/src aquasecurity/trivy fs /src
```

### Coverage Analysis

```bash
# Generate coverage by package
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out

# Find packages with low coverage
go tool cover -func=coverage.out | grep -E '^[^t].*\s+[0-9]{1,2}\.[0-9]+%'

# Coverage for specific package
go test -coverprofile=cache-coverage.out ./internal/cache/...
go tool cover -html=cache-coverage.out
```

---

## 📝 Notes & Decisions

### 2025-10-13: Initial Assessment

- Discovered critical CI failures affecting all Dependabot PRs
- Security workflow using deprecated GitHub Action
- Significant gap between claimed (95%) and actual (~40%) test coverage
- Multiple AWS SDK versions behind latest
- Go version inconsistencies across project files

**Decision**: Prioritize CI fixes in Sprint 1 before addressing test coverage

### Risks & Mitigation

**Risk**: AWS SDK updates introduce breaking changes
**Mitigation**: Staged updates with testing after each stage

**Risk**: Test coverage improvements take longer than estimated
**Mitigation**: Prioritize critical packages first, extend Sprint 2 if needed

**Risk**: Cache build issue root cause unclear
**Mitigation**: Allocate extra investigation time, prepare rollback plan

**Risk**: CI improvements break existing workflows
**Mitigation**: Test all changes in feature branches first

---

## 🔗 Related Documents

- [README.md](README.md) - Project overview
- [CHANGELOG.md](CHANGELOG.md) - Version history
- [CONTRIBUTING.md](CONTRIBUTING.md) - Contribution guidelines
- [.github/workflows/](. github/workflows/) - CI/CD workflows
- [examples/](examples/) - Configuration examples

---

## 📞 Support & Contact

- **Issues**: [GitHub Issues](https://github.com/scttfrdmn/objectfs/issues)
- **Discussions**: [GitHub Discussions](https://github.com/scttfrdmn/objectfs/discussions)
- **Documentation**: [Project Wiki](https://github.com/scttfrdmn/objectfs/wiki)

---

## 📅 Changelog

### 2025-10-13

- ✅ Initial roadmap created
- ✅ All issues identified and documented
- ✅ Sprint plans drafted
- ⏳ Sprint 1 ready to begin

---

**Last Updated**: 2025-10-13
**Next Review**: End of Sprint 1 (Day 3)
**Document Status**: Active
