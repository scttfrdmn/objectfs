# ObjectFS

<!-- Badges: each one must report something real. Two are still removed rather than fixed, because a
     badge that renders "unknown" or "retired" is worse than no badge — it reads as a broken project
     to anyone who looks, and as a passing check to anyone who does not.

       - codecov rendered "unknown": nothing has ever uploaded to it, and no workflow references
         codecov at all. Coverage is gated per-package by scripts/coverage-gate.sh against
         .coverage-floors, which is a job in ci.yml, so the CI badge already covers it. There is no
         separate percentage to display and no service to display it.
       - Go Report Card rendered "retired": the service no longer reports for this module.

     Go Reference is back, below. It rendered empty until v0.10.2 because go.mod declared
     github.com/objectfs/objectfs, which is not this project — it is an unrelated Python repository
     from 2017 — so the module could not be fetched under the name it gave for itself and pkg.go.dev
     had nothing to index (#213). Restored only after checking what a reader actually gets: the page
     resolves for v0.10.2 and documents the 16 packages under pkg/ and sdks/go, which is the whole
     public surface. Most of this module is internal/ and pkg.go.dev does not document that, by
     design — so the badge points at a real API reference rather than at an empty shell (#219).

     The GitHub stars badge is also gone: a star count is not a property of the software. -->

[![CI](https://github.com/scttfrdmn/objectfs/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/scttfrdmn/objectfs/actions/workflows/ci.yml)
[![Security](https://github.com/scttfrdmn/objectfs/actions/workflows/security.yml/badge.svg?branch=main)](https://github.com/scttfrdmn/objectfs/actions/workflows/security.yml)

[![Go Reference](https://pkg.go.dev/badge/github.com/scttfrdmn/objectfs.svg)](https://pkg.go.dev/github.com/scttfrdmn/objectfs)
[![Release](https://img.shields.io/github/v/release/scttfrdmn/objectfs)](https://github.com/scttfrdmn/objectfs/releases)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/scttfrdmn/objectfs)](go.mod)
[![GitHub issues](https://img.shields.io/github/issues/scttfrdmn/objectfs)](https://github.com/scttfrdmn/objectfs/issues)

> ## ⚠️ v0.10.0 is withdrawn — upgrade if you are on it
>
> A deep audit of v0.10.0 found defects that lose or corrupt user data. All of the ones below are
> fixed in the current release, each with a test that fails on the old code. **If you are running
> v0.10.0, upgrade and re-verify any data written through it** — the write-path defect corrupted
> objects silently, so a bucket can hold damage that nothing has reported yet.
>
> | | Defect in v0.10.0 | Effect |
> |---|---|---|
> | **C1** | The shipped default configuration selected a compression algorithm the codec factory rejects | `objectfs s3://bucket /mnt` **could not mount** — it exited with `Failed to start adapter` |
> | **H7** | The write-buffer flush callback discarded the write offset and issued a whole-object `PutObject` | **Silent data loss.** Appending one byte to a 1 MiB file left a 1-byte object |
> | **C4** | Read amplification was keyed off the compression *config*, not the object, so a ranged read fetched the whole object | A 4 KiB read of a 10 GiB object transferred 10 GiB. Measured 216× penalty on a 256 MiB object |
> | **C2** | Reading an object whose stored `Content-Encoding` did not match the configured codec returned raw compressed bytes with exit status 0 | Corruption presented as success. The `objectfs-sha256` the write path recorded was never read |
> | **H5/H6** | The read cache was keyed on request *length* and never invalidated on write | Structurally could not hit; read-after-write on one descriptor returned pre-write bytes |
> | **D11** | `rm` reported success while the S3 object survived | go-fuse's default for an unimplemented `Unlink` is *success*. Deletion now fails loudly (#163) |
>
> To check a bucket written by v0.10.0: compare each object's `objectfs-sha256` user-metadata against
> its content, and check that `HeadObject`'s `ContentLength` matches the size your application wrote.
> The C4 amplification cost egress but did not damage data.
>
> The same audit found that much of this README claimed things the code does not do. Those claims
> have been replaced with what is actually implemented — see
> [Supported filesystem operations](#supported-filesystem-operations) and
> [Data integrity](#data-integrity) below, which are the two sections to read before trusting
> ObjectFS with anything.
>
> ObjectFS is **not** a POSIX-compliant filesystem, and several defects from the same audit are still
> open — see [issues](https://github.com/scttfrdmn/objectfs/issues) and the
> [milestones](https://github.com/scttfrdmn/objectfs/milestones).

**A FUSE filesystem that mounts an S3 bucket as a directory, built for research computing.**

ObjectFS presents the objects in an S3 bucket as files, so tools that read and write files can work
against a bucket without being taught S3. It is aimed at research and institutional workloads: large
sequential reads of reference data, multi-terabyte datasets that will not fit on local disk, and
shared buckets read by many nodes.

It is **not** a general-purpose POSIX filesystem, and the gap is not small — see
[Supported filesystem operations](#supported-filesystem-operations). S3 has no rename, no hard
links, no partial object write, and no atomic anything, and where a POSIX operation cannot be
implemented honestly on top of that, ObjectFS returns an error rather than pretending.

**Platforms:** Linux and macOS (macOS needs [macFUSE](https://macfuse.io)). **Windows is not
supported** — there is no WinFsp binding, and none is claimed until one exists and runs in CI.

---

## Supported filesystem operations

This table is the contract. It is derived from the methods that exist in `internal/fuse` and
`internal/vfs`, not from intent, and a `❌` here means the syscall fails — it does not mean "degraded"
or "slow".

### Implemented

| Operation | Syscalls | Notes |
|---|---|---|
| Read | `open`, `read`, `pread` | Ranged GETs; large reads may be fanned out concurrently |
| Write | `open`, `write`, `pwrite` | Any offset. Buffered as dirty byte ranges and assembled by read-modify-write on flush |
| Truncate / extend | `truncate`, `ftruncate`, `open(O_TRUNC)`, `> file` | Shrink and grow; growing leaves a hole that reads as zeros |
| Flush / sync | `close`, `fsync`, `fdatasync` | Synchronous. An error is returned to `close(2)`, not logged and swallowed |
| Stat | `stat`, `lstat`, `fstat` | Size and mtime come from the object; a dirty file reports the size it will have |
| Create | `creat`, `open(O_CREAT)` | Writes a zero-byte object |
| Directory listing | `opendir`, `readdir`, `ls` | Fully paginated — no entry cap |
| Mkdir | `mkdir` | Writes a zero-byte marker object at `prefix/` |
| chmod / chown | `chmod`, `chown` | On **files** only, stored as object metadata. Permission bits only |
| utimes (mtime) | `touch`, `utimensat` | mtime is stored; an atime-only update is accepted and not stored |
| statfs | `df` | Reports a fixed synthetic capacity — S3 has no size to report |

### Errors by design

These fail, and the failure is the correct answer rather than a missing feature.

| Operation | Error | Why |
|---|---|---|
| setuid / setgid / sticky bit | `ENOTSUP` | Access here is decided by the AWS credentials the process holds, not by a mode bit. A stored setuid bit would promise an escalation that cannot happen |
| Hard links (`link`) | not implemented | S3 has no concept of two names for one object. This will never be supported; it is not on a roadmap |
| `utimes` on a **directory** | accepted, stores nothing | A directory is a key prefix with no object to hold an mtime, and its reported times are synthetic. Failing instead would make every `tar -x`, `cp -a`, and `rsync -a` report errors on every directory for an attribute with nowhere to go |
| `atime`-only update on a file | accepted, stores nothing | Persisting it would mean an object-metadata rewrite per read. POSIX permits a filesystem to keep atime only approximately |
| `fsync` on a **directory** | succeeds, does nothing | A prefix holds no state of its own to make durable, and the objects under it are made durable by their own `close`/`fsync`, which are synchronous. Success is the accurate answer here, not a convenient one |

### Not implemented

| Operation | Current behaviour | Tracked |
|---|---|---|
| `unlink` / `rm` | **`EROFS`** — fails loudly rather than reporting a delete that did not happen | [#163](https://github.com/scttfrdmn/objectfs/issues/163) |
| `rmdir` | **`EROFS`**, same reason | [#163](https://github.com/scttfrdmn/objectfs/issues/163) |
| `chmod` / `chown` on a **directory** | **`ENOTSUP`** — the marker object could carry the metadata, but `Getattr` does not read it back, so accepting the call would report a mode the next `stat` contradicts | [#165](https://github.com/scttfrdmn/objectfs/issues/165) |
| `rename` / `mv` | **`ENOTSUP`** — go-fuse's default for an absent `NodeRenamer` | S3 has no rename; it must be implemented as copy-then-delete, which is neither atomic nor free |
| Symlinks (`symlink`, `readlink`) | **`ENOTSUP`** | Same: no `NodeSymlinker`/`NodeReadlinker` |
| Extended attributes (`getxattr`, `setxattr`, `listxattr`) | **`ENODATA`** on Linux / **`ENOATTR`** on macOS for get and remove; `listxattr` returns an empty list | go-fuse's defaults. An empty list is the accurate answer — there are no xattrs — but note that `setxattr` reports "no such attribute" rather than "unsupported" |
| `mknod` (devices, FIFOs, sockets) | **`ENOTSUP`** | |
| `fallocate` | **`ENOTSUP`** | |
| Locking (`flock`, POSIX record locks) | not forwarded to ObjectFS at all | The mount does not set go-fuse's `EnableLocks`, so the kernel never asks the filesystem to arbitrate a lock; it falls back to tracking locks locally on the mounting host. A lock therefore does not fail — it just means nothing to any other host, and no cross-node coordination exists that could make it mean anything |

### Tools known not to work

Not because of a bug, but because of the semantics above. Verified, not assumed:

- **`rm`, `rm -rf`, and anything that unlinks** — `EROFS` until [#163](https://github.com/scttfrdmn/objectfs/issues/163). This includes `git checkout` of a branch that deletes files, and any build system that cleans. `mv` within the mount fails earlier, with `ENOTSUP`, because there is no rename.
- **SQLite, and anything using POSIX record locks** — locks are not forwarded to the filesystem, so they are host-local and invisible to any other mount of the same bucket. Two hosts will both believe they hold the same exclusive lock. A single writer with no concurrent readers may work; do not rely on it.
- **`git` on a repository inside the mount** — needs rename, unlink, and locking.
- **`tar -x` and `rsync --delete`** — both unlink.
- **Anything expecting `mmap` writeback to be atomic or ordered.**

Large sequential reads, `cp` into the mount, and read-only traversal of a dataset are the paths that
are tested hardest and the ones ObjectFS is for.

---

## Data integrity

Integrity is the project's first priority, ahead of performance. This section states what is
guaranteed and — more usefully — what is not.

### What is guaranteed

- **Every object ObjectFS writes records a SHA-256 of its uncompressed content** as the
  `objectfs-sha256` user-metadata key.
- **A read that returns a complete object verifies that hash and fails on mismatch.** A codec
  mismatch, a lost `Content-Encoding` header, a truncated body, a mangled multipart assembly, and
  bit-rot in the bucket all produce bytes that differ from what was hashed, and all of them are
  refused with an integrity error rather than returned with exit status 0. Note this covers a whole
  small file read through the kernel's buffer — "complete" means the response covered the whole
  object, not that no `Range` header was sent.
- **`close(2)` and `fsync(2)` return the error.** If the PUT failed — `AccessDenied`, a full
  quota, a network failure — the syscall fails. It does not log and return success.
- **A flush is a real read-modify-write.** Writing at an offset fetches the ranges of the stored
  object it needs, splices the dirty ranges over them, and PUTs the result. It does not replace the
  object with the fragment that was written.

### What is not guaranteed

- **A partial read of a large object is not checksum-verified.** The recorded hash is over the whole
  content, so verifying a 4 KiB read of a 10 GiB object would mean transferring 10 GiB — which is
  the read amplification the read path exists to avoid. Per-range checksums require changing the
  stored object layout and are tracked with the seekable-framing work.
- **Objects ObjectFS did not write are not verified.** Anything put in the bucket by `aws s3 cp`,
  boto3, or a bucket that predates ObjectFS carries no `objectfs-sha256`, and such an object reads
  without a checksum check. Refusing them would make ObjectFS unable to read the buckets it exists
  to mount.
- **There is no atomicity across a write.** A crash mid-flush leaves the object as it was, or as it
  will be — S3 PUTs are atomic per object — but a multi-object operation has no transaction, and
  nothing is journaled.
- **There is no concurrent-writer safety.** Two nodes writing the same object will produce one
  winner and no error. Nothing detects the conflict, because `PutObject` carries no precondition
  today.
- **`df` output is fiction.** A fixed synthetic capacity, because S3 has no size to report. Do not
  use it for capacity planning or in a script that checks for free space.
- **Compression, if enabled, breaks the "just objects in S3" property.** A compressed object is an
  opaque frame to `aws s3 cp`, boto3, and every other S3 client. Compression is **off by default**
  for this reason.

### What changes when the remaining write-path work lands

This section is written against the code as it is. When per-range checksums or conditional writes
land, the two "not guaranteed" items about partial reads and concurrent writers change, and this
section changes with them. If it ever disagrees with the code, the code is right and this is a bug
worth filing.

---

## Install

Linux, or macOS with [macFUSE](https://macfuse.io) installed.

```bash
git clone https://github.com/scttfrdmn/objectfs.git
cd objectfs
make build          # produces ./objectfs

# or
go install github.com/scttfrdmn/objectfs/cmd/objectfs@latest
```

## Usage

ObjectFS takes exactly two positional arguments — the bucket URI and the mount point. There are no
subcommands.

```bash
# Mount, using the built-in defaults
objectfs s3://my-bucket /mnt/s3

# With a configuration file
objectfs --config /etc/objectfs/config.yaml s3://my-bucket /mnt/s3

# Validate the configuration and exit without mounting
objectfs --dry-run --config /etc/objectfs/config.yaml s3://my-bucket /mnt/s3
```

Credentials do not go in the config file. ObjectFS uses the standard AWS credential chain:
`AWS_PROFILE`, the environment variables, the shared credentials file, or an instance role.

To unmount, send `SIGINT` or `SIGTERM` (`Ctrl-C`), or run `fusermount -u /mnt/s3` on Linux or
`umount /mnt/s3` on macOS. **`SIGHUP` also unmounts** — it is not a configuration reload. There is
no configuration reload; changing the file requires a remount.

---

## Configuration

ObjectFS starts from its built-in defaults, so a config file need only contain what it overrides.
The bucket is not a config key — it comes from the `s3://bucket` command-line argument. A key the
schema does not define is rejected at startup with the key named, rather than silently ignored.

### Basic research setup

```yaml
storage:
  s3:
    region: us-west-2

performance:
  cache_size: 8GB
```

### Large sequential reads

```yaml
storage:
  s3:
    region: us-west-2
    multipart:
      threshold: 64MB
      chunk_size: 32MB

performance:
  cache_size: 32GB
  connection_pool_size: 32
  parallel_read:
    enabled: true
    threshold: 64MB     # objects at least this large are fetched as concurrent range GETs
    chunk_size: 16MB

cache:
  ttl: 15m
  persistent_cache:
    enabled: true
    directory: /var/cache/objectfs
    max_size: 200GB
```

### Server-side encryption

```yaml
security:
  encryption:
    mode: sse-kms       # off (default), sse-s3, sse-kms
    kms_key_id: arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab
    bucket_keys: true   # cuts KMS requests by up to 99%; recommended with sse-kms
```

`mode: off` is the default and is not "unencrypted": S3 has applied SSE-S3 to all new objects
unconditionally since January 2023. What `off` lacks is a key of your own — nothing appears
per-object in CloudTrail and nothing can be revoked short of deleting the data. If a compliance
review asks for encryption at rest with a customer-managed key, `sse-kms` is the mode that answers
it.

### Transparent compression

Off by default. Turning it on is a decision with four non-obvious costs — most importantly that a
compressed object is not readable by `aws s3 cp`, boto3, or the S3 console, which hand back the
compressed bytes with a successful exit status.

```yaml
write_buffer:
  compression:
    enabled: true
    algorithm: zstd     # none, zstd, lz4, gzip
    level: 3            # zstd 0-22, gzip 0-9; 0 selects the codec default
    min_size: 4KB       # smaller objects are stored as-is
```

Read [docs/features/compression.md](docs/features/compression.md) first. It covers what compresses
(most research data does not — it arrived compressed), why a partial read of a compressed object
transfers the whole object, which storage tiers make the saving zero, and why compression inside the
file format is usually the better answer.

### The two shipped config files

- [`configs/example.yaml`](configs/example.yaml) — a short, copyable starting point. Every key in it is read on the mount path.
- [`examples/config.yaml`](examples/config.yaml) — the complete schema: every key ObjectFS accepts, at its default value. Keys that parse and validate but are not yet read on the mount path are marked `not yet wired`, so this file is also the honest inventory of what is and is not implemented.

### Cost estimation and institutional discounts

Storage-cost estimates use a built-in rate table, and an institution with negotiated rates can
supply its own discount table. **This is reachable from the Go SDK only, not from the mount
configuration file** — the discount table lives on `s3.Config.PricingConfig`, and the mount path
does not map it, so a `pricing_config:` block in a YAML file is not a setting ObjectFS has and is
rejected at startup.

```go
backend, err := s3.NewBackend(ctx, "your-enterprise-bucket", &s3.Config{
	Region: "us-west-2",
	PricingConfig: s3.PricingConfig{
		// Distributed by IT; see examples/discount-config.yaml for the format.
		DiscountConfigFile: "/shared/aws/institutional-discounts.yaml",
	},
})
```

Cost figures are estimates for planning, not billing, and the built-in rate table is a snapshot
rather than a live query. See
[examples/DISCOUNT_CONFIG_README.md](examples/DISCOUNT_CONFIG_README.md) for the format.

---

## Architecture

```
User apps → Kernel VFS → FUSE (go-fuse) → internal/vfs → Adapter → S3 backend → AWS S3
                                               │
                                       cache (LRU + persistent)
```

`internal/vfs` is the POSIX-semantics core: the attribute model, the handle table, dirty-range
tracking, and the read-modify-write flush policy. It depends on nothing FUSE and is testable without
a mount, which is deliberate — the absence of this layer in v0.10.0 is what let a whole class of
defects through a large test suite. `internal/fuse` is a translation shim above it: go-fuse types in,
`vfs` calls out, error mapping, and nothing else.

See [DEVELOPMENT.md](DEVELOPMENT.md) for the package layout and
[docs/architecture/overview.md](docs/architecture/overview.md) for more detail.

---

## Performance

No benchmark numbers are published here. The ones that used to be were not measurements, and
removing them is part of the v0.10.1 work — a fabricated number is worse than no number, because a
reader cannot tell that it needs checking.

What the code does, which is a different claim from how fast it is:

- Large reads are fanned out into concurrent range GETs above a configurable threshold.
- Reads are cached in memory, optionally spilling to local disk.
- Writes are buffered as dirty byte ranges and flushed when the kernel asks.
- Objects above a threshold are uploaded as concurrent multipart parts.

`benchmarks/run_benchmarks.sh` runs the Go benchmarks against real S3 if you want numbers for your
own bucket, region, and instance type — which are the only numbers worth having.

---

## Development

```bash
make build          # build the binary
make test           # go test -race ./...
make lint           # golangci-lint
```

Testing is layered, and the layers exist for a reason the audit made concrete — v0.10.0 had 32,680
lines of tests that missed every defect above, because each one was a *seam* defect: a value
correctly produced at one layer and dropped at the boundary to the next, invisible to any test that
mocks the neighbouring layer.

- **`internal/testaws`** — the real S3 backend against an in-process [substrate](https://github.com/scttfrdmn/substrate) endpoint over real HTTP. No network, no credentials, no AWS account. Preferred over a hand-written mock, because a mock on the far side of a seam agrees with its caller by construction.
- **`internal/difftest`** — a differential oracle: one operation sequence run against both ObjectFS and the local OS filesystem, asserting they agree on reads, sizes, and durable bytes.
- **Fuzz targets** with committed corpora over the write path, the range/slice domain, config loading, and the compress→PUT→GET→decompress round trip.
- **Live AWS integration tests** — `AWS_PROFILE=aws AWS_REGION=us-west-2 go test -race -tags=integration ./...`

Pre-commit hooks run formatting, `go build`, `golangci-lint`, and the test suite. Install them with
`./scripts/setup-hooks.sh`.

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Work is tracked in
[GitHub issues](https://github.com/scttfrdmn/objectfs/issues) and
[milestones](https://github.com/scttfrdmn/objectfs/milestones).

The most useful contribution right now is a report of a case where ObjectFS's behaviour differs from
the two contract sections above. Either the code is wrong or the documentation is, and both are
worth knowing.

## License

Apache 2.0 — see [LICENSE](LICENSE).

## Support

- **Issues**: [GitHub Issues](https://github.com/scttfrdmn/objectfs/issues)
- **Discussions**: [GitHub Discussions](https://github.com/scttfrdmn/objectfs/discussions)
- **Security**: [SECURITY.md](SECURITY.md) — report privately through a
  [security advisory](https://github.com/scttfrdmn/objectfs/security/advisories/new), not a public
  issue. The policy also documents what the default configuration exposes, which is worth reading
  before mounting on a shared host
