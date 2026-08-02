# Quick Start Guide

This guide will get you up and running with ObjectFS in minutes. We'll walk through
installation, basic configuration, and your first mount.

> **This page previously described four install channels and a `mount` subcommand, none of which
> exist.** The first command was `curl -sSL https://get.objectfs.io | sh` — a getting-started guide
> whose opening line is a domain that has never served anything. Also gone: an apt repository at
> `packages.objectfs.io`, a Homebrew tap, an AUR package, and "Windows with WSL2" as a supported
> prerequisite. Windows is not supported: every file in `internal/fuse` carries
> `//go:build linux || darwin`.
>
> The commands below were checked against `objectfs --help`. Everything on this page that names a
> flag or an argument now matches the binary.

## Prerequisites

Before you begin, ensure you have:

- Linux, or macOS with [macFUSE](https://macfuse.io) installed
- Permission to mount a FUSE filesystem (membership in the `fuse` group on Linux)
- An AWS account with S3 access
- Basic familiarity with command-line operations

## Installation

Build from source, or install with `go install`. Release binaries are attached to the
[GitHub releases page](https://github.com/scttfrdmn/objectfs/releases); that is the only prebuilt
artifact, and the release workflow builds no packages.

<CodeRunner language="bash">

```bash
git clone https://github.com/scttfrdmn/objectfs.git
cd objectfs
make build          # produces ./bin/objectfs

# or, without a checkout
go install github.com/scttfrdmn/objectfs/cmd/objectfs@latest

# Verify installation
objectfs --version
```

</CodeRunner>

On macOS, install macFUSE first — ObjectFS cannot mount without it:

<CodeRunner language="bash">

```bash
brew install --cask macfuse
```

</CodeRunner>

## First Mount

Let's mount your first S3 bucket as a local filesystem.

### 1. Set Up Credentials

#### AWS S3

<CodeRunner language="bash">

```bash
# Configure AWS credentials
export AWS_ACCESS_KEY_ID="your-access-key"
export AWS_SECRET_ACCESS_KEY="your-secret-key"
export AWS_REGION="us-east-1"

# Or use AWS CLI
aws configure
```

</CodeRunner>

### 2. Create a Mount Point

<CodeRunner language="bash">

```bash
# Create directory for mount
sudo mkdir -p /mnt/objectfs
sudo chown $(whoami):$(whoami) /mnt/objectfs
```

</CodeRunner>

### 3. Mount the Filesystem

<CodeRunner language="bash">

```bash
# Mount an S3 bucket. There are no subcommands: the binary takes exactly two
# positional arguments, the bucket URI and the mount point.
objectfs s3://your-bucket-name /mnt/objectfs

# Mount with a few flags
objectfs --cache-size 8GB --log-level INFO s3://your-bucket-name /mnt/objectfs
```

</CodeRunner>

### 4. Verify the Mount

<CodeRunner language="bash">

```bash
# Check if mounted
df -h /mnt/objectfs
mount | grep objectfs

# List contents
ls -la /mnt/objectfs

# Test read/write operations
echo "Hello ObjectFS!" > /mnt/objectfs/test.txt
cat /mnt/objectfs/test.txt
```

</CodeRunner>

## Basic Operations

Now that you have ObjectFS mounted, let's explore basic operations:

### File Operations

<CodeRunner language="bash">

```bash
# Copy files to object storage
cp /path/to/local/file.txt /mnt/objectfs/

# Create directories
mkdir /mnt/objectfs/my-folder
```

Two operations this section used to show do **not** work, and fail loudly rather than silently:

```bash
mv /mnt/objectfs/old.txt /mnt/objectfs/new.txt   # ENOTSUP — there is no rename
rm /mnt/objectfs/unwanted-file.txt               # EROFS   — delete is not implemented
```

`rm` returning an error is deliberate. go-fuse defaults an unimplemented `Unlink` to *success*, so
without the refusal `rm` would exit 0 while the object survived in S3 — the user believes the file is
gone and it is still there and still billing. The
[supported-operations table](https://github.com/scttfrdmn/objectfs#supported-operations) is the
authority on which operations are in which state.

</CodeRunner>

### Directory Operations

<CodeRunner language="bash">

```bash
# Recursive copy
cp -r /path/to/local/folder /mnt/objectfs/

# Find files
find /mnt/objectfs -name "*.txt" -type f

# Directory sizes
du -h /mnt/objectfs/my-folder
```

</CodeRunner>

### Advanced Operations

<CodeRunner language="bash">

```bash
# Stream large files
cat /mnt/objectfs/large-file.txt | grep "pattern"

# Compress and upload
tar -czf - /path/to/folder | cat > /mnt/objectfs/backup.tar.gz

# Download and extract
cat /mnt/objectfs/backup.tar.gz | tar -xzf -
```

</CodeRunner>

## Configuration

ObjectFS can be configured using command-line options, configuration files, or environment variables.

### Command-Line Options

<CodeRunner language="bash">

```bash
# The complete flag set, from `objectfs --help`. Everything else is configuration-file only.
objectfs \
  --cache-size 8GB \
  --max-concurrency 100 \
  --log-level DEBUG \
  s3://bucket /mnt/objectfs

# Validate a configuration and exit without mounting
objectfs --dry-run --config ~/.objectfs/config.yaml s3://bucket /mnt/objectfs
```

There is no `--enable-predictive-caching` and no `--cost-optimization` flag; both appeared here and
neither is parsed. Predictive caching is a configuration key. Cost optimization has a configuration
key and no code path reaches it — see the
[Not yet wired up table](https://github.com/scttfrdmn/objectfs/blob/main/docs/index.md#not-yet-wired-up).

</CodeRunner>

### Configuration File

Create a configuration file for persistent settings:

<CodeRunner language="yaml">

```yaml
# ~/.objectfs/config.yaml
global:
  log_level: info
  log_file: /var/log/objectfs.log

storage:
  s3:
    region: us-east-1
    use_acceleration: true
    cost_optimization:
      enabled: true
      tiering_enabled: true

performance:
  cache_size: 8GB
  max_concurrency: 100
  multilevel_caching: true
  predictive_caching: true

monitoring:
  enabled: true
  metrics_addr: :9090
  health_check_addr: :8081
```

</CodeRunner>

Every key above is one the loader defines, which is checked by
`TestDocumentedConfigYAMLMatchesTheSchema` — configuration is decoded strictly, so a key the schema
does not have fails at startup with the key named. Two of them set a value nothing reads:
`cost_optimization` is not mapped onto the backend (`internal/adapter/adapter.go` records why: the
config and backend types are disjoint), and `predictive_caching` reaches the cache but the predictor
it selects is never installed on the mount path.

<CodeRunner language="bash">

```bash
# Use configuration file
objectfs --config ~/.objectfs/config.yaml s3://bucket /mnt/objectfs
```

</CodeRunner>

## Monitoring

Two HTTP endpoints, both served by the mount process itself. There is no `objectfs health` or
`objectfs metrics` command — the binary has no subcommands, so `curl` is the interface.

<CodeRunner language="bash">

```bash
# Health, on monitoring.health_check_addr (:8081 by default)
curl http://localhost:8081/health

# Prometheus metrics, on monitoring.metrics_addr (:9090 in the config above)
curl http://localhost:9090/metrics
```

</CodeRunner>

Both listeners bind all interfaces and are unauthenticated. [SECURITY.md](https://github.com/scttfrdmn/objectfs/blob/main/SECURITY.md)
documents that and the switches that turn each off.

## Unmounting

When you're done, unmount the filesystem with the platform's own tool. `objectfs unmount` is not a
command:

<CodeRunner language="bash">

```bash
# Linux
fusermount -u /mnt/objectfs

# macOS
umount /mnt/objectfs

# Or signal the mount process, which unmounts and flushes on the way out
kill -TERM "$(pgrep -f 'objectfs .*/mnt/objectfs')"

# Verify
mount | grep objectfs
```

</CodeRunner>

## Common Issues

### Permission Denied

<CodeRunner language="bash">

```bash
# Ensure proper permissions
sudo usermod -a -G fuse $(whoami)

# Restart session or run
newgrp fuse
```

</CodeRunner>

### Mount Point Busy

<CodeRunner language="bash">

```bash
# Check for active processes
lsof /mnt/objectfs

# Force unmount
sudo fusermount -u /mnt/objectfs
```

</CodeRunner>

### Performance Issues

<CodeRunner language="bash">

```bash
# Increase cache size
objectfs --cache-size 16GB s3://bucket /mnt/objectfs

# Raise concurrency
objectfs --max-concurrency 200 s3://bucket /mnt/objectfs

# Check metrics for bottlenecks
curl -s http://localhost:9090/metrics | grep objectfs_
```

</CodeRunner>

Predictive caching is not a flag, and turning the configuration key on does not enable it: the
predictor is never installed on the mount path. It is listed in the
[Not yet wired up table](https://github.com/scttfrdmn/objectfs/blob/main/docs/index.md#not-yet-wired-up).

## Next Steps

This section listed four pages — Performance Tuning, Distributed Clusters, Monitoring, and Security
— and none of them was ever written. `docs-platform/guide/` contains this page and an introduction.
What does exist, in the repository:

- **[Supported operations and the integrity contract](https://github.com/scttfrdmn/objectfs/blob/main/README.md)**:
  what works, what fails by design, and which tools are known not to work. Read this before
  pointing a workload at a mount
- **[Configuration reference](https://github.com/scttfrdmn/objectfs/blob/main/docs/index.md)**:
  every key the loader accepts, and the **Not yet wired up** table for the features that have code
  but no path from a mount
- **[Benchmarks](https://github.com/scttfrdmn/objectfs/tree/main/benchmarks)**: runnable, which is
  the only kind of performance figure this project quotes

Security is IAM's and the bucket's: ObjectFS adds no authentication or authorization layer of its
own. It does write with server-side encryption — SSE-S3 or SSE-KMS, configurable — on every object.

## Getting Help

If you encounter issues or need help:

- Search [GitHub issues](https://github.com/scttfrdmn/objectfs/issues)
- Ask questions in [GitHub Discussions](https://github.com/scttfrdmn/objectfs/discussions)

There is no troubleshooting guide and no API reference site; both were linked here and neither
exists. For the Go API, `go doc ./internal/adapter` is the authority, with the caveat that these
packages are under `internal/` and importable only inside this module.

<InteractiveExample>

::: tip
Try the interactive examples above to see ObjectFS in action! Each code block can be executed directly in your browser.
:::

</InteractiveExample>
