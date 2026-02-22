# ObjectFS — Claude Code Guide

## Project

Enterprise-grade POSIX-compliant FUSE filesystem for AWS S3, optimized for research computing and institutional deployments.

- **Module**: `github.com/objectfs/objectfs`
- **Go version**: 1.26.0
- **License**: Apache 2.0, Copyright 2025-2026 Scott Friedman
- **Current version**: 0.4.0

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
- `internal/fuse/` — POSIX filesystem operations
- `internal/storage/s3/` — AWS S3 backend + pricing
- `internal/cache/` — LRU + persistent + predictive cache
- `internal/buffer/` — write buffering with compression
- `internal/config/` — YAML + env configuration
- `internal/circuit/` — circuit breaker
- `internal/health/` — health monitoring
- `internal/distributed/` — multi-node coordination (experimental)
- `pkg/archive/` — archive format metadata (tar.zst, tar.gz, tar.bz2)
- `pkg/types/` — core interfaces

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
