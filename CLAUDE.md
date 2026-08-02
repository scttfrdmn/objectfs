# ObjectFS — Claude Code Guide

## Project

FUSE filesystem presenting a POSIX interface over AWS S3, for research computing and institutional
deployments. Not a POSIX-compliant filesystem — see the supported-operations table in `README.md`
for what works, what fails by design, and which tools are known not to work.

- **Module**: `github.com/scttfrdmn/objectfs`
- **Go version**: 1.26.0
- **License**: Apache 2.0, Copyright 2025-2026 Scott Friedman
- **Current version**: the `version` constant in `cmd/objectfs/main.go` is the only authority. Do
  not restate it here or in any roadmap: five files claimed five different versions (0.10.0, 0.7.0,
  v0.3.0, v0.2.0 twice), and a number copied into prose has no way to be told it is stale

## Project Tracking

**All work is tracked on GitHub — never in local files.**

- **Issues**: <https://github.com/scttfrdmn/objectfs/issues>
- **Milestones**: <https://github.com/scttfrdmn/objectfs/milestones>
- **Projects**: <https://github.com/orgs/scttfrdmn/projects> (ObjectFS v0.5.0 Development #11, ObjectFS Technical Debt #12)
- **Labels**: See label taxonomy below

Do not create local sprint/tracking/progress markdown files. Do not reference SPRINT_*.md, PROGRESS_REPORT.md, or similar. When tracking work, create GitHub issues and link them to the appropriate milestone and project.

## Changelog

This project uses **[Keep a Changelog](https://keepachangelog.com/en/1.0.0/)** format and **[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)**.

- Add all user-facing changes to the `## [Unreleased]` section of `CHANGELOG.md`
- Use the categories: `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, `Security`
- When cutting a release: promote Unreleased → `## [X.Y.Z] - YYYY-MM-DD`

## Architecture

```
User apps → Kernel VFS → FUSE (go-fuse/cgofuse) → Adapter → S3 Backend → AWS S3
```

Key internal packages:

- `internal/adapter/` — central coordinator
- `internal/vfs/` — POSIX-semantics core: attributes, handle table, dirty ranges, read-modify-write flush. Depends on nothing FUSE and is testable without a mount
- `internal/fuse/` — go-fuse binding: kernel types ⇄ `vfs` calls, error mapping
- `internal/storage/s3/` — AWS S3 backend + pricing
- `internal/cache/` — LRU + persistent + predictive cache
- `internal/config/` — YAML + env configuration
- `internal/circuit/` — circuit breaker
- `internal/health/` — health monitoring
- `internal/distributed/` — multi-node coordination (experimental)
- `pkg/archive/` — archive format metadata (tar.zst, tar.gz, tar.bz2)
- `pkg/types/` — core interfaces

Test infrastructure:

- `internal/testaws/` — the real S3 backend against an in-process [substrate](https://github.com/scttfrdmn/substrate) endpoint over real HTTP: no network, no credentials, no AWS account. Prefer this to a hand-written mock — a mock on the far side of a seam agrees with the caller by construction, which is why 32,680 lines of tests missed ~45 defects
- `internal/difftest/` — differential oracle: one operation sequence run against ObjectFS and against the local OS filesystem, asserting they agree on reads, sizes, and durable bytes

## Related Projects

- **CargoShip** (`/Users/scttfrdmn/src/cargoship`, `github.com/scttfrdmn/cargoship`) — streaming archive/upload pipeline; objectfs uses it for S3 throughput optimization
- **GlobalFS** (`/Users/scttfrdmn/src/globalfs`, `github.com/scttfrdmn/globalfs`) — global namespace orchestration layer built on top of objectfs + cargoship

## Development

### Build

```bash
make build          # build binary
make test           # run tests with race detector
make lint           # golangci-lint
go build ./...      # verify compilation
```

### Environment

Private module — set these for go get:

```bash
GONOSUMDB="github.com/scttfrdmn/*" GOPRIVATE="github.com/scttfrdmn/*"
```

### AWS Credentials

AWS is available via the named profile `aws` in region `us-west-2`:

```bash
AWS_PROFILE=aws AWS_REGION=us-west-2 go test -race -tags=integration ./...
```

Use this profile when running integration tests that require real S3 access.

### Testing

- All tests use `-race` flag
- Table-driven tests with `t.Parallel()`
- Integration tests hit real AWS S3 (no LocalStack); use `AWS_PROFILE=aws AWS_REGION=us-west-2`
- Coverage target: 80%+ per package

### Pre-commit

Pre-commit hooks run on every commit: trailing whitespace, YAML check (--unsafe for MkDocs), gofmt, go mod tidy, go build, golangci-lint, markdownlint. The hooks will auto-fix what they can; re-stage and commit again if they modify files.

## GitHub Label Taxonomy

### Type

| Label | Use |
|-------|-----|
| `type: bug` | Something broken |
| `type: enhancement` | New feature |
| `type: performance` | Perf optimization |
| `type: technical-debt` | Refactor/cleanup |
| `type: testing` | Test coverage |
| `type: documentation` | Docs |
| `type: ci-cd` | CI/CD |
| `type: security` | Security |

### Priority

`priority: critical` · `priority: high` · `priority: medium` · `priority: low`

### Area

`area: core` · `area: fuse` · `area: s3` · `area: buffer` · `area: cache` · `area: metadata` · `area: config` · `area: health` · `area: metrics` · `area: distributed` · `area: archive` · `area: sdk`

## Milestones

| Milestone | Purpose |
|-----------|---------|
| `v0.5.0 - Technical Debt` | Test coverage, stubs, hardcoded values |
| `v0.5.0 - Phase 1: CargoShip Integration` | Archive filesystem, BBR networking |
| `v0.5.0 - Phase 2: Advanced Compression` | ZSTD, adaptive compression |
| `v0.5.0 - Phase 3: Distributed (Experimental)` | Redis cache, distributed ops |
| `v0.5.0 - Phase 4: Cost Optimization` | ML predictions, cost tracking |
| `Future Work` | Longer-horizon features, not yet scheduled |
