# ObjectFS Python SDK

High-performance POSIX filesystem for object storage with comprehensive
Python API support.

## Features

- **Easy Integration**: Simple, pythonic API for mounting and managing ObjectFS
- **Async Support**: Full async/await support for high-performance applications
- **AWS S3 Deep Integration**: Optimized specifically for AWS S3 with intelligent tiering and cost management
- **Distributed Operations**: Built-in support for distributed clusters
- **Monitoring & Metrics**: Comprehensive health checking and metrics collection
- **Configuration Management**: Flexible configuration with presets and validation
- **CLI Tools**: Command-line interface for common operations

## Installation

> **`pip install objectfs` installs someone else's package.** This SDK has never been published to
> PyPI — no workflow in this repository publishes it — and the name `objectfs` is already taken
> there by an unrelated "Simple Python VFS module" from 2015 by a different author. So the three
> `pip install objectfs...` commands documented here did not fail; they succeeded and installed
> different software, which is the worse outcome. Install from this repository instead.

```bash
git clone https://github.com/scttfrdmn/objectfs.git
cd objectfs/sdks/python
pip install -e .
```

For development and monitoring extras, `pip install -e '.[dev]'` and `pip install -e '.[monitoring]'`
from that same directory.

## Quick Start

### Basic Usage

```python
import asyncio
from objectfs import ObjectFSClient

# Create client
client = ObjectFSClient()

# Mount filesystem
mount_id = client.mount('s3://my-bucket', '/mnt/objectfs')
print(f"Mounted with ID: {mount_id}")

# Use the filesystem
# Files in /mnt/objectfs are now backed by S3

# Unmount when done
client.unmount('/mnt/objectfs')
```

### Async Usage

```python
import asyncio
from objectfs import ObjectFSClient

async def main():
    async with ObjectFSClient() as client:
        # Mount filesystem
        mount_id = client.mount('s3://my-bucket', '/mnt/objectfs')

        # Get health status -- monitoring.health_checks.addr, default 127.0.0.1:8081
        health = await client.get_health('http://127.0.0.1:8081')
        print(f"Health: {health['status']}")

        # Collect metrics -- monitoring.metrics.addr, default 127.0.0.1:8080.
        # Requires monitoring.metrics.enabled: true; nothing is bound otherwise.
        metrics = await client.get_metrics('http://127.0.0.1:8080')

        # Present once the mount has served at least one cache request. An idle mount
        # reports no hit rate rather than a hit rate of zero, because those are different
        # facts.
        if 'hit_rate' in metrics['cache']:
            print(f"Cache hit rate: {metrics['cache']['hit_rate']:.2%}")

        # Unmount
        client.unmount('/mnt/objectfs')

asyncio.run(main())
```

### Configuration

```python
from objectfs import Configuration, ObjectFSClient

# Load from file
config = Configuration.from_file('objectfs.yaml')
client = ObjectFSClient(config=config)

# Create from preset
config = Configuration.from_preset('production')

# Create programmatically
config = Configuration()
config.performance.cache_size = '8GB'
config.performance.max_concurrency = 500
config.storage.s3.region = 'us-west-2'

client = ObjectFSClient(config=config)
```

### Storage Operations

**Not implemented.** `list_objects`, `get_object_info`, `download_object`, `upload_object` and
`delete_object` raise `StorageError`. They used to return fabricated data — two invented objects
from `list_objects`, a fixed size and etag from `get_object_info`, `True` from `upload_object` and
`delete_object` for transfers and deletions that never happened — and `download_object` wrote
`Simulated file content from S3` over whatever file was at the local path it was given, called the
progress callback to completion, and returned 30. This section documented all of it as working, with
`/tmp/downloaded-file.txt` as the download target.

Tracked as [#325](https://github.com/scttfrdmn/objectfs/issues/325), which also covers the identical
code in the JavaScript SDK. Until it lands, use boto3:

```python
import boto3
boto3.client('s3').download_file('my-bucket', 'data/file.txt', '/tmp/file.txt')
```

or mount the bucket and use ordinary file operations, which is what ObjectFS is for:

```python
async def read_through_a_mount():
    async with ObjectFSClient() as client:
        await client.mount('s3://my-bucket', '/mnt/data')
        with open('/mnt/data/data/file.txt', 'rb') as handle:
            return handle.read()
```

### Distributed Clusters

What this SDK can do is *describe* a cluster — the config section is real and reaches the daemon
through the generated YAML. Membership operations are not implemented: `join_cluster`,
`leave_cluster` and `get_cluster_status` raise `DistributedError`, so the last four statements of
the example that used to be here could not work. `get_cluster_status` in particular returned
`{'node_count': 1, 'leader': 'self', 'status': 'healthy', 'nodes': []}` for any configuration,
without querying anything.

```python
from objectfs import Configuration, ObjectFSClient

config = Configuration()
config.cluster.enabled = True
config.cluster.listen_addr = '0.0.0.0:8080'
config.cluster.seed_nodes = ['node1.example.com:8080', 'node2.example.com:8080']

# Written out, this is what the daemon reads. Mounting with it is how a node joins.
config.save_to_file('objectfs-cluster.yaml')

async def cluster_example():
    async with ObjectFSClient(config=config) as client:
        await client.mount('s3://my-bucket', '/mnt/objectfs')
```

ObjectFS's own distributed layer is experimental; see the top-level README before depending on it.

### Cache Operations

**Not implemented**, same issue. `clear_cache` and `warm_cache` raise `CacheError`; the first
returned `True` after a log line, and the second reported success for every path it was given.

## CLI Usage

The SDK includes a command-line interface:

```bash
# Mount filesystem
objectfs-python mount s3://my-bucket /mnt/objectfs

# List active mounts
objectfs-python list-mounts

# Check health
objectfs-python health --endpoint http://localhost:8081

# Get metrics
objectfs-python metrics --endpoint http://localhost:8080 --format table

# Generate configuration
objectfs-python config generate --preset production --output config.yaml

# Unmount filesystem
objectfs-python unmount /mnt/objectfs
```

`objectfs-python storage list|download|upload` exist but are **not implemented** — each exits 1
with the `StorageError` message. `--help` says so. This section previously showed
`storage download s3://my-bucket file.txt ./local-file.txt` as a working command; it wrote
placeholder bytes over `./local-file.txt` and printed `Successfully downloaded 30 bytes`.

## Configuration Reference

### Configuration File

```yaml
# objectfs.yaml
global:
  log_level: INFO
  log_file: /var/log/objectfs.log

storage:
  s3:
    region: us-east-1
    use_acceleration: true
    cost_optimization:
      # Store objects too small to be worth the configured tier's 128 KB billing
      # minimum on STANDARD instead. The four other keys this block used to show
      # were removed in v0.11.0 — they never reached the backend.
      small_objects_on_standard: true

performance:
  cache_size: 8GB
  max_concurrency: 500
  multilevel_caching: true
  predictive_caching: true

cluster:
  enabled: false
  replication_factor: 3
  consistency_level: eventual

monitoring:
  metrics:
    enabled: true             # required, or nothing binds the metrics endpoint
    addr: 127.0.0.1:8080      # loopback by default; the endpoint has no authentication
    custom_labels:
      service: objectfs       # attached to every exported series
  health_checks:
    enabled: true
    addr: 127.0.0.1:8081
```

### Environment Variables

Configuration can be overridden with environment variables:

```bash
export OBJECTFS_LOG_LEVEL=DEBUG
export OBJECTFS_CACHE_SIZE=16GB
export OBJECTFS_S3_REGION=us-west-2
export OBJECTFS_CLUSTER_ENABLED=true
```

### Configuration Presets

Available presets:

- `development`: Optimized for development with debug logging
- `production`: Production-ready with monitoring enabled
- `high-performance`: Maximum performance with large caches
- `cost-optimized`: Minimized costs with intelligent tiering
- `cluster`: Distributed cluster setup with replication

## API Reference

### ObjectFSClient

Main client class for interacting with ObjectFS.

#### Methods

- `mount(storage_uri, mount_point, config_overrides=None,
  foreground=False)`: Mount
- `unmount(mount_point)`: Unmount filesystem  
- `list_mounts()`: List active mounts
- `is_mounted(mount_point)`: Check if path is mounted
- `validate_config(config=None)`: Validate configuration
- `generate_config(preset='production', output_path=None)`: Generate config

#### Async Methods

- `get_health(endpoint=None)`: Get health status
- `get_metrics(endpoint=None)`: Collect metrics. Returns `cache`, `io`, `operations`,
  `errors` and `connections` sections plus the parsed `raw` samples. A section is absent
  when the mount has not recorded that family -- absent is not zero
- `get_performance_stats()`: **Not implemented; raises `NotImplementedError`.** It returned
  fixed constants that looked like measurements. Use `get_metrics()`
The rest raise, and did not always ([#325]). The signatures are what they will have if they are
implemented; nothing behind them talks to S3 or to another node:

- `list_objects(storage_uri, prefix=None, max_keys=1000)`: raises `StorageError`. Returned two
  invented objects, `test-file-1.txt` and `test-file-2.txt`
- `get_object_info(storage_uri, key)`: raises `StorageError`. Returned a fixed size and etag for any
  key, existing or not
- `download_object(storage_uri, key, local_path)`: raises `StorageError`. **Wrote
  `Simulated file content from S3` over `local_path`**, called the progress callback to completion,
  and returned 30 — destroying an existing file and reporting success
- `upload_object(storage_uri, key, local_path, metadata=None)`: raises `StorageError`. Returned
  `True` without transferring anything
- `delete_object(storage_uri, key)`: on `StorageAdapter`, not the client. Raises `StorageError`;
  returned `True` while the object remained in S3
- `join_cluster(seed_nodes, node_config=None)` / `leave_cluster()`: raise `DistributedError`.
  Returned `True` without contacting any node
- `get_cluster_status()`: raises `DistributedError`. Reported a healthy single-node cluster for any
  configuration, without querying anything
- `clear_cache(cache_type=None, keys=None)`: raises `CacheError`. Returned `True` after a log line
- `warm_cache(paths, recursive=False)`: raises `CacheError`. Reported success for every path given

[#325]: https://github.com/scttfrdmn/objectfs/issues/325

### Configuration Classes

- `Configuration`: Main configuration container
- `StorageConfig`: Storage backend configuration
- `PerformanceConfig`: Performance and caching settings
- `ClusterConfig`: Distributed cluster settings
- `SecurityConfig`: Security and authentication settings
- `MonitoringConfig`: Monitoring and observability settings

## Error Handling

The SDK provides specific exception types:

```python
from objectfs import (
    ObjectFSError,        # Base exception
    ConfigurationError,   # Configuration issues
    MountError,          # Mount/unmount failures
    StorageError,        # Storage operation failures
    DistributedError,    # Cluster operation failures
    NetworkError,        # Network connectivity issues
)

try:
    client.mount('s3://invalid-bucket', '/mnt/objectfs')
except MountError as e:
    print(f"Mount failed: {e}")
except ConfigurationError as e:
    print(f"Configuration error: {e}")
```

## Examples

There is no `examples/` directory. This section listed six files in one, and none of them was ever
written — the runnable examples are the inline ones above:

- [Basic mounting](#basic-usage)
- [Async operations](#async-usage)
- [Configuration management](#configuration)
- [Storage operations](#storage-operations)
- [Distributed clusters](#distributed-clusters) — note that multi-node coordination is
  experimental and not reachable from a mount today
- Monitoring and metrics: `internal/metrics/doc.go` documents the Prometheus endpoint. The SDK
  method that reported cache statistics returned a hardcoded `hit_rate`, so there is nothing here
  to point at yet

## Development

### Setup

```bash
git clone https://github.com/scttfrdmn/objectfs.git
cd objectfs/sdks/python

# Create virtual environment
python -m venv venv
source venv/bin/activate  # or venv\Scripts\activate on Windows

# Install in development mode
pip install -e .[dev]
```

### Testing

```bash
# Run tests
pytest

# Run with coverage
pytest --cov=objectfs

# Run specific test
pytest tests/test_client.py::TestClient::test_mount
```

### Code Quality

```bash
# Format code
black objectfs/
isort objectfs/

# Lint code
flake8 objectfs/

# Type checking
mypy objectfs/
```

## License

Apache License 2.0 - see [LICENSE](../../LICENSE) for details.

## Contributing

See [CONTRIBUTING.md](../../CONTRIBUTING.md) for contribution guidelines.

## Support

- GitHub Issues: <https://github.com/scttfrdmn/objectfs/issues>
- Documentation: the `docs/` tree in this repository
- Discussions: <https://github.com/scttfrdmn/objectfs/discussions>
