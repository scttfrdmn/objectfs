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
> | **D11** | `rm` reported success while the S3 object survived | go-fuse's default for an unimplemented `Unlink` is *success*, so v0.10.0 answered every `rm` with exit 0 and deleted nothing. `Unlink` and `Rmdir` are implemented now (#163) |
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
[Supported filesystem operations](#supported-filesystem-operations). S3 has no atomic rename, no
hard links, no partial object write, and no atomic anything across more than one object, and where a
POSIX operation cannot be implemented honestly on top of that, ObjectFS either returns an error
rather than pretending, or documents exactly how far short of the POSIX guarantee it falls.

**Platforms:** Linux and macOS (macOS needs [macFUSE](https://macfuse.io)). **Windows is not
supported** — there is no WinFsp binding, and none is claimed until one exists and runs in CI.

---

## AWS S3 is the target, not a lowest common denominator

**ObjectFS is built for AWS S3 specifically, and uses every S3 capability that benefits it.**
S3-compatible endpoints — MinIO, Ceph RGW, RustFS, Wasabi and the rest — are supported on a best-effort
basis: they get a fallback or a reduced capability, not a veto over what ObjectFS does on AWS.

This is a design decision with teeth, because the alternative is the default. A filesystem that
targets the intersection of every S3 implementation gets whole-object PUTs and unconditional writes,
which is a smaller and slower filesystem than S3 can support — and the cost is paid on the backend
almost every user is actually running.

So capabilities are established by **probing the endpoint in front of this process**, never from a
config flag, an endpoint-URL heuristic, or a version string. A store that *accepts* a header and
ignores it is indistinguishable from one that honours it by every means except the outcome, so
asking is not good enough; ObjectFS attempts the operation. See `types.BackendCapabilities`.

What happens when a capability is missing depends on **what kind of capability it is**, and the two
rules are deliberately different:

| | If AWS-only and the endpoint lacks it | Example |
|---|---|---|
| **Performance capability** | **Fall back silently.** Slower is a correct outcome | Transfer Acceleration falls back to the standard endpoint on error, and stays fallen back |
| **Correctness capability** | **Fail closed.** The feature refuses to start, with an operator-facing reason | Conditional writes: an endpoint that fails the probe gets `ErrNotSupported`, and coordination declines rather than running unguarded |

The second rule is the one that matters. A precondition an endpoint silently drops is worse than one
it refuses, because every contender for a lease is told it won. Degrading a correctness guarantee to
keep a feature available on a non-AWS store would be trading the guarantee for the feature, and the
guarantee is the feature.

Plain filesystem use is unaffected by any of this — reads, writes, and metadata work on any
S3-compatible endpoint. What varies is coordination. Measured per-endpoint results, probed rather
than documented, are in
[Conditional-write compatibility](docs/design/conditional-write-compatibility.md); AWS S3 is the only
row verified against the real service, and the only row that stays verified in CI.

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
| Extended attributes | `getxattr`, `setxattr`, `listxattr`, `removexattr`, `getfattr`, `setfattr` | On **files** only, stored as object metadata. See [Extended attributes](#extended-attributes) for the size budget, the namespaces that are refused, and the per-call cost |
| utimes (mtime) | `touch`, `utimensat` | mtime is stored; an atime-only update is accepted and not stored |
| statfs | `df` | Reports a fixed synthetic capacity — S3 has no size to report |
| Unlink | `unlink`, `rm` | Deletes the object. A missing file is `ENOENT`, not a silent success |
| Rmdir | `rmdir`, `rm -d` | Removes the marker object. A non-empty prefix is `ENOTEMPTY` — it will not orphan the objects under it |
| Rename / move | `rename`, `mv` | Server-side copy then delete, per object; a directory moves everything under its prefix. **Not atomic** — see [Rename is not atomic](#rename-is-not-atomic). `renameat2`'s `RENAME_EXCHANGE` and `RENAME_NOREPLACE` are refused with `EINVAL` |

#### Rename is not atomic

POSIX `rename` is atomic: an observer sees the old name or the new one, never both and never
neither. S3 offers nothing that can implement that. There is no atomic rename operation — the 2026
`RenameObject` API is directory-bucket (S3 Express) only, and ObjectFS does not support directory
buckets. A rename is therefore a server-side copy followed by a delete, per object.

What that means in practice:

- **A concurrent reader can see both names**, for as long as the copy-then-delete takes. It can also
  see the source after the destination already exists.
- **An interruption — crash, `SIGKILL`, lost network — leaves the data at the old name, the new
  name, or both. Never at neither.** Each source object is deleted only after its own copy has
  succeeded. Duplicated data is an operator's cleanup problem; missing data is not recoverable, so
  the ordering is fixed in that direction deliberately.
- **A partial directory move is resumable.** Re-running the same `mv` copies whatever is still at the
  source and deletes it, and objects already moved are simply absent from the source listing.
- **`renameat2` flags are refused rather than approximated.** `RENAME_EXCHANGE` and
  `RENAME_NOREPLACE` are atomicity promises — swap two names with no observable intermediate state,
  fail if the destination exists with no race — and copy-then-delete cannot keep either. They return
  `EINVAL`, which is what the kernel and libc expect for an unsupported flag, and `mv` and Git fall
  back correctly on it.
- **Renaming a directory costs one copy and one delete per object beneath it**, charged and rate-
  limited as such. It is not a metadata operation.
- **Renaming across mounts is `EXDEV`**, so `mv` falls back to copy-and-unlink through user space.

Anything that depends on rename atomicity — the write-temp-then-rename idiom used by editors, `git`,
and lockfile schemes — is therefore not safe here between concurrent writers. Single-writer use is
fine.

#### Extended attributes

`setfattr -n user.project -v atlas file` and `getfattr -n user.project file` work on files, and the
attributes survive a remount because they are stored on the object. Four properties are worth knowing
before you rely on them.

**A file's attributes share a 2 KB budget, and the usable part is 1758 bytes.** S3 caps an object's
total user metadata at 2 KB across all keys, and *rejects* a request that exceeds it rather than
truncating. ObjectFS already spends part of that on mode, uid, gid, mtime, the content checksum, and
the original size, leaving 1758 bytes for names and values together — measured from the widest form of
those keys, not estimated, and re-derived by a test so that adding a stored attribute cannot shrink the
budget silently. Names and values are encoded to survive an HTTP header, which costs about 60% on top
of a name and 33% on top of a value. Exceeding the budget is `E2BIG` for a single value too large for
any object, `ENOSPC` for a value that will not fit alongside what this file already has — the
distinction `setxattr(2)` draws, and the one a caller retrying after freeing space depends on.

**`security.*` and `system.*` are refused with `ENOTSUP`.** This is a privilege boundary rather than a
missing feature. The Linux kernel reads `security.capability` from the filesystem on every `exec` and
grants the file capabilities it names; the store behind an ObjectFS attribute is object metadata, which
anyone holding `s3:PutObject` on the bucket can write with the AWS CLI without touching the mount. So
honouring it would turn bucket write access into a route to file capabilities on every host that mounts
the bucket. `system.posix_acl_access` is refused for the milder version of the same problem: nothing in
this filesystem enforces an ACL, and one that `getfacl` reports while no access check consults it is
worse than a `setfacl` that fails. `user.*`, `trusted.*`, and macOS's `com.apple.*` are stored.

**Every `setfattr` is one `CopyObject` on the file.** `setxattr(2)` takes a path, not a descriptor, so
there is no `close` that could batch the change — it is made durable before the call returns, exactly
as `chmod` and `touch` are. Setting ten attributes on a file is ten metadata rewrites. That is the
honest cost of the operation; the alternative would be reporting success for a change S3 later refused,
with no caller left to tell.

**A removed attribute leaves a marker in the object's metadata.** A metadata replace merges over an
object's existing metadata rather than replacing it, so omitting a key cannot delete it. `removexattr`
therefore writes a tombstone: `getfattr` correctly stops showing the attribute, and `head-object` still
shows a key holding the marker. Deleting one attribute from a 10 GiB file would otherwise cost a full
rewrite of the object.

Directories have no extended attributes: `setfattr` on one is `ENOTSUP`, and a listing is empty. See
[Not implemented](#not-implemented).

One errno differs by platform, and it differs because the kernels do. A `setxattr` naming both
`XATTR_CREATE` and `XATTR_REPLACE` is `EINVAL` on macOS, which is also what `setxattr(2)`'s Linux man
page describes — but Linux does not implement it that way: `fs/xattr.c` has no combined-flag check, so
the flags are tested independently and the result is `ENODATA` when the attribute is missing and
`EEXIST` when it exists. ObjectFS matches whichever kernel it is running under rather than picking one,
so a program's behavior on an ObjectFS mount matches its behavior on the local filesystem of the same
host.

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
| `chmod` / `chown` on a **directory** | **`ENOTSUP`** — the marker object could carry the metadata, but `Getattr` does not read it back, so accepting the call would report a mode the next `stat` contradicts | [#165](https://github.com/scttfrdmn/objectfs/issues/165) |
| Symlinks (`symlink`, `readlink`) | **`ENOTSUP`** | No `NodeSymlinker`/`NodeReadlinker` |
| Extended attributes on a **directory** | `setxattr` is **`ENOTSUP`**; `getxattr` and `removexattr` report the attribute missing (**`ENODATA`** on Linux, **`ENOATTR`** on macOS); `listxattr` succeeds with an empty list. A directory that exists only because objects share a prefix has no object to hold an attribute, so accepting the call would store it for some directories and discard it for others. The empty listing keeps `cp -a`, `rsync -X`, and `ls -@` from erroring per directory | [#167](https://github.com/scttfrdmn/objectfs/issues/167) |
| `security.*` and `system.*` extended attributes | **`ENOTSUP`** — refused deliberately, on files as well as directories. Object metadata is writable by anyone with bucket write access, so an attribute the kernel acts on cannot be stored here. See [Extended attributes](#extended-attributes) | |
| `mknod` (devices, FIFOs, sockets) | **`ENOTSUP`** | |
| `fallocate` | **`ENOTSUP`** | |
| Locking (`flock`, POSIX record locks) | not forwarded to ObjectFS at all | The mount does not set go-fuse's `EnableLocks`, so the kernel never asks the filesystem to arbitrate a lock; it falls back to tracking locks locally on the mounting host. A lock therefore does not fail — it just means nothing to any other host, and no cross-node coordination exists that could make it mean anything |

### Tools known not to work

Not because of a bug, but because of the semantics above. Verified, not assumed:

- **SQLite, and anything using POSIX record locks** — locks are not forwarded to the filesystem, so they are host-local and invisible to any other mount of the same bucket. Two hosts will both believe they hold the same exclusive lock. A single writer with no concurrent readers may work; do not rely on it.
- **`git` on a repository inside the mount** — locking is host-local, and Git's index and lockfile updates rely on rename being atomic, which it is not. A single-writer clone will mostly work; two hosts against one bucket will corrupt the repository, and no error will say so.
- **Anything using the write-temp-then-rename idiom for atomic replacement** — editors that save that way, and lockfile schemes built on `RENAME_NOREPLACE`. The replacement happens, it just is not atomic; `RENAME_NOREPLACE` is refused outright with `EINVAL`. See [Rename is not atomic](#rename-is-not-atomic).
- **Anything expecting `mmap` writeback to be atomic or ordered.**
- **`rsync --delete` and `tar -x` over an existing tree** work now that unlink and rename do, but note that both make a directory rename cost one copy plus one delete per object underneath rather than a single metadata write.

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
- **Rename is not atomic, and a directory rename is not even single-object.** An interruption leaves
  the data at the old name, the new name, or both — never at neither, because each source is deleted
  only after its own copy succeeds. See [Rename is not atomic](#rename-is-not-atomic) for what a
  concurrent reader can observe.
- **There is no concurrent-writer safety.** Two nodes writing the same object will produce one
  winner and no error. Nothing detects the conflict, because `PutObject` carries no precondition
  today.
- **`df` output is fiction.** A fixed synthetic capacity, because S3 has no size to report. Do not
  use it for capacity planning or in a script that checks for free space.
- **On a non-AWS S3-compatible endpoint, conditional writes may not be available at all.** They are
  the primitive behind every coordination feature, and they are an AWS behavior rather than something
  an S3-compatible store necessarily implements: Ceph RGW 19.2.0 implements them *partially* — it
  ignores the precondition on multipart completion, so a large conditional write silently becomes
  unconditional. ObjectFS establishes this by probing the configured endpoint at mount time, not from
  a version string or a config flag, and an endpoint that fails the probe gets `ErrNotSupported`
  rather than an unconditional write — a coordination feature refuses to start rather than running
  unguarded. Measured per-endpoint results are in
  [Conditional-write compatibility](docs/design/conditional-write-compatibility.md).
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

### HPC sites: environment modules

Lmod and TCL Modules modulefiles are in [`configs/modules/`](configs/modules/). Install the one
matching the module system in use, into a `share/modulefiles` tree under the same prefix as the
binary, named for the version:

```bash
# Ask the binary rather than typing a number, so the two cannot disagree
V=$(objectfs version | awk '{print $3}')

# Lmod
install -D configs/modules/objectfs.lua "/usr/share/modulefiles/objectfs/$V.lua"

# TCL Modules — no .tcl extension: the filename IS the version
install -D configs/modules/objectfs.tcl "/usr/share/modulefiles/objectfs/$V"
```

The version is not written inside either file — each reads it from its own filename, which is why the
install path carries it and why the command above takes it from the binary instead of a literal. The
`version` constant in `cmd/objectfs/main.go` stays the only place it is recorded. Both files may live
in the same directory: Lmod reads the `.lua` and `modulecmd.tcl` reads the extensionless one, and
neither reports the other's file.

```bash
module use /usr/share/modulefiles
module load objectfs
objectfs version
module help objectfs     # usage, and what does not work
```

The modulefile puts the binary's directory on `PATH` only when that directory is not already on it,
and exports `OBJECTFS_VERSION` so a job script can record which build it ran against. If no
`objectfs` binary is found it **refuses to load**, listing the directories it searched, rather than
adding a `PATH` entry that leads nowhere — a module that loads successfully and leaves `objectfs`
as "command not found" inside a batch job is the failure this avoids.

For a build outside the module tree's own prefix — a center keeping modulefiles in `/sw/modulefiles`
and builds in `/sw/objectfs/<version>` — set `OBJECTFS_MODULE_PREFIX` to the prefix holding
`bin/objectfs`.

ObjectFS runs in the foreground and does not fork, so under a batch scheduler mount in a prologue or
background the mount in the job script, and unmount in the epilogue: the unmount is what flushes
buffered writes to S3, and a job killed with the mount still running loses whatever has not been
flushed.

## Usage

```bash
# Mount, using the built-in defaults
objectfs mount s3://my-bucket /mnt/s3

# With a configuration file
objectfs mount --config /etc/objectfs/config.yaml s3://my-bucket /mnt/s3

# Validate the configuration and exit without mounting
objectfs mount --dry-run --config /etc/objectfs/config.yaml s3://my-bucket /mnt/s3

# Unmount, from any process — an operator's shell, or a systemd unit's ExecStop
objectfs unmount /mnt/s3
```

The commands are `mount`, `unmount` (also spelled `umount`), `version`, and `help`. Flags come
before the positional arguments: Go's flag package stops parsing at the first non-flag argument, so
`objectfs mount s3://b /mnt --debug` would leave `--debug` unparsed — the command reports that
rather than ignoring it.

The form without a subcommand still works and is not deprecated, because it is what every invocation
written before v0.11.0 looks like:

```bash
objectfs s3://my-bucket /mnt/s3
```

A first argument carrying a URI scheme or a leading dash routes to `mount`; a bare word that is not
a command is a usage error naming itself, so `objectfs moutn s3://b /mnt` does not become an attempt
to mount a bucket called `moutn`.

Both arguments to `mount` are optional when the configuration file supplies them, as `mount.uri` and
`mount.mount_point`. That is the form a systemd template unit needs — see
[`configs/systemd/objectfs@.service`](configs/systemd/objectfs@.service), which passes
`--mount-point /mnt/objectfs/%i --foreground` and names the bucket in the per-instance config file,
since `systemctl start objectfs@research-data` gives the unit only its instance name.

Exit codes: `0` succeeded, `1` the command was right and the operation failed, `2` the command line
was wrong and nothing was attempted.

Credentials do not go in the config file. ObjectFS uses the standard AWS credential chain:
`AWS_PROFILE`, the environment variables, the shared credentials file, or an instance role.

To unmount, run `objectfs unmount /mnt/s3`, or send `SIGINT` or `SIGTERM` (`Ctrl-C`) to the mount
process. `objectfs unmount` is what a script or a unit file should use: it tries the FUSE helper, its
libfuse-2 spelling, `umount`, and finally `umount(2)`, and if none works it says which ran, which
were not installed, and the `lsof` invocation that names whatever is holding the mount open. None of
them unmounts lazily or forcibly — `fusermount3 -z` and `umount -l` detach the name while the
filesystem keeps serving open files, which reports a finished unmount with writes still in flight.

`SIGHUP` is **not** handled and is not a configuration reload. It used to be treated as a shutdown
request, so `kill -HUP` unmounted the filesystem of anyone who read the "zero-downtime reload" claim
this README used to make. There is no configuration reload; changing the file requires a remount.

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
storage:
  s3:
    compression:
      enabled: true
      algorithm: zstd   # none, zstd, lz4, gzip
      level: 3          # zstd 0-22, gzip 0-9; 0 selects the codec default
      min_size: 4KB     # smaller objects are stored as-is
```

Read [docs/features/compression.md](docs/features/compression.md) first. It covers what compresses
(most research data does not — it arrived compressed), why a partial read of a compressed object
transfers the whole object, which storage tiers make the saving zero, and why compression inside the
file format is usually the better answer.

### Metrics and health endpoints

A mount starts two HTTP listeners with the built-in defaults. Both are on, and neither is
authenticated, which is why both default to loopback:

```yaml
monitoring:
  metrics:
    enabled: true
    addr: 127.0.0.1:8080    # /metrics, /health, /debug/metrics, /debug/operations
  health_checks:
    enabled: true
    addr: 127.0.0.1:8081    # /health
    interval: 30s
    timeout: 5s
```

There are no `objectfs metrics` or `objectfs health` subcommands — the mount process serves both, so
`curl` is the interface:

```bash
curl -s 127.0.0.1:8080/metrics
curl -s 127.0.0.1:8081/health
```

`enabled: false` is the only way to turn a listener off. A port of `0` is rejected rather than read as
"off" — through v0.10.x `health_port: 0` disabled its listener while `metrics_port: 0` defaulted back
to 8080 and bound it, so the same value meant opposite things in adjacent blocks. Those port keys are
gone: a port cannot name an interface, so `:8080` — every interface the host has — was the only bind
either setting could produce, whatever it was set to. Prometheus scraping `localhost:8080` needs no
change; a scraper on another host needs an address written here deliberately, and
[SECURITY.md](SECURITY.md) covers what it then exposes.

An address that cannot be bound fails the mount and names the field, rather than logging and leaving a
mount up with a probe endpoint that answers nothing. `OBJECTFS_METRICS_ENABLED`,
`OBJECTFS_METRICS_ADDR`, `OBJECTFS_HEALTH_ENABLED`, and `OBJECTFS_HEALTH_ADDR` override all four
without a config file; a non-boolean in either `_ENABLED` fails startup instead of being coerced.

There is no pprof listener at any address. See
[#245](https://github.com/scttfrdmn/objectfs/issues/245).

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
- **Every build tag is compiled in CI**, one job per tag. A file behind a build tag is excluded from `go build ./...` and `go test ./...`, so it can stop compiling without any check going red — which is how four tagged suites in this repository came to carry code that did not build ([#240](https://github.com/scttfrdmn/objectfs/issues/240)).

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
