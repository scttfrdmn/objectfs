# Security Policy

## Reporting a vulnerability

Report privately, through GitHub's private vulnerability reporting:

**<https://github.com/scttfrdmn/objectfs/security/advisories/new>**

That opens a draft advisory visible only to you and the maintainers. Please do not open a public
issue for anything exploitable — the issue tracker is public, and
[`.github/ISSUE_TEMPLATE/security_issue.yml`](.github/ISSUE_TEMPLATE/security_issue.yml) is for
hardening suggestions and non-exploitable concerns only.

Useful in a report, in rough order of value: the version (`objectfs --version`), the configuration
that reproduces it with credentials removed, whether it is reachable without local access to the
mounting host, and what an attacker gets. A reproduction beats a description.

Expect an acknowledgement within a week. This is a single-maintainer project, so a fix timeline
depends on what the finding is; the acknowledgement will say what the plan is rather than leave you
guessing. If you would like credit in the advisory and the release notes, say so — the default is to
credit the reporter by whatever name or handle they give.

## Supported versions

| Version | Supported |
|---|---|
| 0.13.0 | ✅ |
| 0.12.0 | ❌ Superseded. No known security defect. Note that a mount on it never started cluster coordination at all (#139), so its `cluster.*` settings had no security posture to speak of — 0.13.0 is the first release where they do, and it refuses to start a cluster without a gossip secret |
| 0.11.0 | ❌ Superseded. No known security defect, but its gossip protocol has no message authentication (#206), so a cluster on an untrusted network is only as safe as that network |
| 0.10.3 | ❌ Superseded. No known security defect; it simply predates the release above, and only the latest release gets fixes |
| 0.10.2 | ❌ Superseded. Importable and does not lose data, but it was tagged before eleven of its twelve milestone issues merged, so it lacks all of the correctness and cost fixes in v0.10.3 |
| 0.10.1 | ❌ Superseded. Not importable as a Go module — the tag declares a module path that is not this repository (#213), so `go get` fails on it |
| 0.10.0 | ❌ **Withdrawn** — see the banner in [README.md](README.md); it loses data |
| < 0.10.0 | ❌ |

Only the latest release gets fixes. Pre-1.0 means no backports: a security fix ships in the next
patch release from `main`.

## What ObjectFS is, for threat-modelling purposes

A FUSE filesystem that mounts an S3 bucket. It runs as a user process on the mounting host and
talks to S3 with credentials that host already has. It is not a network service and does not
authenticate clients — its trust boundary is the host it runs on.

Two consequences worth being explicit about:

**Anything with access to the mountpoint has whatever access the credentials grant.** ObjectFS
enforces no authorization of its own. `mount.options.allow_other` (`false` by default) is the switch
that widens the mountpoint beyond the mounting user; turning it on hands every local user the
bucket. Scope this at the IAM policy, not in ObjectFS.

**Two HTTP listeners are on by default, on loopback.** With the shipped defaults
(`monitoring.metrics.enabled: true`, `monitoring.health_checks.enabled: true`) a mount listens on
`127.0.0.1:8080` and `127.0.0.1:8081`:

| Address | Path | Serves | How to turn it off |
|---|---|---|---|
| `monitoring.metrics.addr` (127.0.0.1:8080) | `/metrics`, `/health`, `/debug/metrics`, `/debug/operations` | Prometheus counters, latencies, cache statistics; per-operation counts, errors, sizes and timings | `monitoring.metrics.enabled: false` |
| `monitoring.health_checks.addr` (127.0.0.1:8081) | `/health` | Component health status, including component names and error strings | `monitoring.health_checks.enabled: false` |

Neither is authenticated. They expose operational telemetry, not object contents, keys, or
credentials, but the volumes and error rates are enough to characterize a workload — and `/health`
names components and repeats their error strings. Set `enabled: false`, or point the address
somewhere deliberate. Anything wider than loopback is now something an operator writes down: through
v0.10.x both listeners bound `:port` — every interface — whatever the configuration said, because the
setting was a port and a port cannot name an interface
([#211](https://github.com/scttfrdmn/objectfs/issues/211)).

**Turning a listener off is `enabled: false`, and only that.** `metrics_port: 0` used to read as
"unset" and default back to 8080, so an operator who wrote `0` meaning "off" got a listener on the
default port ([#212](https://github.com/scttfrdmn/objectfs/issues/212)); `health_port: 0` disabled its
listener, so the same value meant opposite things in the two blocks. An address cannot be overloaded
that way, and there is no second spelling of "off". Setting both addresses equal fails validation
rather than starting one listener and losing the other.

A bind failure is now fatal to startup and names the address. Both servers used to bind on a goroutine
and log the error, so a mount whose metrics port was taken came up with no endpoint and one line in the
log to say why — an operator finds that out when a probe starts failing.

No pprof listener is started at any address, and there is no longer any code in the repository that
could start one. `global.enable_pprof` and `global.profile_port` were removed rather than wired
because the server they would have started — `pkg/profiling` — bound `:6060` on every interface and
served mutating `/memory/gc` and `/memory/free` handlers with no authentication. That package has
since been deleted along with `pkg/memmon` ([#245](https://github.com/scttfrdmn/objectfs/issues/245));
neither had an importer, so nothing in a shipped binary ever reached them. Profile a build with
`go test -cpuprofile`/`-memprofile`, or add `net/http/pprof` behind a loopback bind in a local
branch — do not ship one.

## Credentials

ObjectFS resolves AWS credentials through the standard AWS SDK chain: environment, shared config,
IMDS/container roles. Prefer that. `storage.s3.access_key_id` / `secret_access_key` exist in the
config schema for endpoints that need them, and writing long-lived keys into a YAML file is worse
than the alternatives — the file is as protected as its filesystem permissions, and ObjectFS does
not check them on load.

ObjectFS logs no credential values. It also does not mask them, because it never logs them; if you
find a path that does, that is a vulnerability worth reporting.

## Data at rest and integrity

Both are configuration, and both default to something weaker than the maximum:

**Encryption** (`security.encryption.mode`) defaults to `off`, which sends no SSE header. That is
not the same as unencrypted — S3 has applied SSE-S3 to all new objects unconditionally since
January 2023 — but it means the key is Amazon's, not your institution's. `sse-s3` and `sse-kms` are
implemented and applied on every write path (`PutObject`, multipart, both `CopyObject` sites); see
[`internal/storage/s3/encryption.go`](internal/storage/s3/encryption.go). If you need a key you can
audit, rotate, and revoke, set `mode: sse-kms` with `kms_key_id`, and turn on `bucket_keys`.

In v0.10.0 a `security.encryption.at_rest` key defaulted to `true` and was read by nothing — every
object was written with no encryption header while the configuration said otherwise. That key is now
removed rather than deprecated, and the loader decodes strictly, so a config still setting it fails
to load and names the key. If you are migrating a v0.10.0 config, that error is the point.

**Integrity.** Every object ObjectFS writes carries an `objectfs-sha256` user-metadata checksum, and
since v0.10.1 the read path verifies it and fails closed on a mismatch or on a recorded value it
cannot parse. Objects written by v0.10.0 carry the checksum but were never verified against it — see
the README banner for how to audit a bucket written by that release.

Two limits, because a partial guarantee stated precisely is worth more than a broad one:

- **A read that covers less than the whole object is not verified.** The hash is over the entire
  content, so checking a 4 KiB fragment of a 10 GiB object would mean transferring 10 GiB. A complete
  object is verified at any size — including a small file read through a large kernel buffer — and the
  large-file random read is the case that is not. Per-chunk checksums belong with the seekable-framing
  work, which changes the stored layout.
- **An object with no recorded checksum verifies trivially.** Objects written by `aws s3 cp`, boto3,
  or anything predating ObjectFS carry no `objectfs-sha256`, and refusing them would make ObjectFS
  unable to read the buckets it exists to mount.

Neither is a defence against a bucket an attacker can write to. The checksum is user metadata, so
anyone who can rewrite the object can rewrite the hash alongside it; it detects corruption, not
tampering. Integrity against a hostile writer is an IAM property.

## Transport

The AWS SDK speaks HTTPS to S3. The one thing that turns that off is pointing
`storage.s3.endpoint` at an `http://` URL, which you have to write explicitly. There is no
configuration key that silently downgrades transport.

`internal/distributed` (experimental) gossips over **unauthenticated UDP** — no HMAC, no signature,
no shared secret — and is not safe on an untrusted network. It is not reachable from the `objectfs`
binary at all today (`go list -deps ./cmd/objectfs` contains no `internal/distributed` package), so
nothing a released binary does opens that port; it matters to anyone importing the package directly,
and it has to be fixed before the package is wired up. That is
[#206](https://github.com/scttfrdmn/objectfs/issues/206), which is a stated prerequisite for the
cache-warming work precisely because warming would turn an unauthenticated gossip channel into a
content-injection path.

## What is out of scope

- **S3's own behaviour.** Bucket policies, public-access settings, and IAM are the operator's;
  ObjectFS cannot make an over-permissive bucket safe.
- **Denial of service by way of a large object or a slow read.** A filesystem read of a 10 GiB
  object transfers 10 GiB. Cost and time are properties of the workload.
- **Local users on a host where you enabled `allow_other`.** That is the documented effect of the
  setting.
- **Anything in a `_test.go` file, `internal/testaws`, or `internal/testhttp`.** None of it ships in
  the binary.

## What runs on every change

- `gosec`, `govulncheck`, and Trivy on the repository, in
  [`.github/workflows/security.yml`](.github/workflows/security.yml), on every push and PR to `main`
  plus weekly.
- Trivy on the release binary itself, *before* the release is published, in
  [`.github/workflows/release.yml`](.github/workflows/release.yml). Findings currently annotate a
  release rather than block it; making it a gate is
  [#196](https://github.com/scttfrdmn/objectfs/issues/196).
- A license-compliance check scoped to `./cmd/objectfs` — what is actually distributed.
- Dependabot alerts, Dependabot security updates, secret scanning, and push protection are enabled
  on the repository.
- Release assets carry a `.sha256` alongside each archive: `sha256sum -c objectfs-<platform>.tar.gz.sha256`.

Known-open security-labelled work is tracked with `type: security` in the
[issue tracker](https://github.com/scttfrdmn/objectfs/issues?q=is%3Aissue+is%3Aopen+label%3A%22type%3A+security%22).
It is deliberately public: an absent feature that someone can look for and ask about is safer than a
configuration key that claims the property and does nothing, which is the defect this project
already shipped once.
