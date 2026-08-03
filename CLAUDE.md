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
- **Projects**: <https://github.com/users/scttfrdmn/projects/12> — ObjectFS Technical Debt.
  `scttfrdmn` is a user account, so `/orgs/scttfrdmn/projects`, which this line used to say, is a
  404. Board #11, "ObjectFS v0.5.0 Development", is closed: its 14 items were all closed issues and
  its title named a release five versions back. Board #12 has the same problem in a milder form —
  all 8 of its items are closed and nothing has been added since February — but "technical debt" is
  a category rather than a release, so new issues can join it. Milestones, not boards, are what this
  project actually plans with
- **Labels**: `.github/labels.yml` is the authority — see the taxonomy notes below

Do not create local sprint/tracking/progress markdown files. Do not reference SPRINT_*.md, PROGRESS_REPORT.md, or similar. When tracking work, create GitHub issues and link them to the appropriate milestone and project.

## Changelog

This project uses **[Keep a Changelog](https://keepachangelog.com/en/1.0.0/)** format and **[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)**.

- Add all user-facing changes to the `## [Unreleased]` section of `CHANGELOG.md`
- Use the categories: `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, `Security`
- When cutting a release: promote Unreleased → `## [X.Y.Z] - YYYY-MM-DD`

## Architecture

```
User apps → Kernel VFS → FUSE (go-fuse) → internal/fuse → internal/vfs → Adapter → S3 Backend → AWS S3
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

Nothing to set. This section told you to export `GOPRIVATE` and `GONOSUMDB` for
`github.com/scttfrdmn/*` because the module was private; objectfs, cargoship, and substrate are all
public now, and `go get github.com/scttfrdmn/objectfs` resolves through the module proxy with both
variables explicitly empty — verified, not assumed. `sdks/c/Makefile` was the last place still
setting them; that has been removed too, so no copy of the claim survives to go stale.

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

**`.github/labels.yml` is the authority.** Every label, with its description and color, is defined
there. Read it rather than a list here: the list that used to be here named 12 of 22 `area:` labels
and did not mention five whole families, and a label enumeration in prose has the same problem as a
version number in prose — no way to be told it is stale.

Every issue should carry a `type:`, a `priority:`, and at least one `area:`. Those three families,
plus `status:`, `resolution:`, `effort:`, `persona:`, and `cargoship:`, are grouped and commented in
`labels.yml`. Two pairs are easy to confuse and both members of each are in use: `type: ci-cd` is
what kind of change it is, `area: ci-cd` is the part of the system it touches.

`.github/labels.yml` and the repository's actual labels are checked against each other by
`internal/config/labels_test.go`, in both directions, on every PR. When it fails,
`.github/scripts/sync-labels.sh` reports the drift and `--apply` fixes the half that is safe to fix
automatically. Do not add a label by way of `gh issue create --label` — that invents the label with
no description and the default grey, which is how two of them got onto the repository without ever
reaching the file.

## Milestones

<https://github.com/scttfrdmn/objectfs/milestones> is the authority; file against the milestone for
the next release. A table of milestone titles used to be here, and five of its six entries were
closed `v0.5.0 - *` milestones while every live one was absent — including the one the release in
progress was being filed against. Same reasoning as the version constant above: the guide links the
authoritative view, so it should not also transcribe it.
