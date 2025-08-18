# ObjectFS Python SDK

High-performance POSIX filesystem for object storage with comprehensive Python API support.

## Features

- **Easy Integration**: Simple, pythonic API for mounting and managing ObjectFS instances
- **Async Support**: Full async/await support for high-performance applications
- **Multiple Backends**: Support for S3, Google Cloud Storage, and Azure Blob Storage
- **Distributed Operations**: Built-in support for distributed clusters and replication
- **Monitoring & Metrics**: Comprehensive health checking and metrics collection
- **Configuration Management**: Flexible configuration with presets and validation
- **CLI Tools**: Command-line interface for common operations

## Installation

```bash
pip install objectfs
```

For development:

```bash
pip install objectfs[dev]
```

For monitoring features:

```bash
pip install objectfs[monitoring]
```

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

        # Get health status
        health = await client.get_health('http://localhost:8081')
        print(f"Health: {health['status']}")

        # Collect metrics
        metrics = await client.get_metrics('http://localhost:9090')
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

```python
async def storage_example():
    async with ObjectFSClient() as client:
        # List objects
        objects = await client.list_objects(
            's3://my-bucket',
            prefix='data/',
            max_keys=100
        )

        # Download object
        bytes_downloaded = await client.download_object(
            's3://my-bucket',
            'data/file.txt',
            '/tmp/downloaded-file.txt'
        )

        # Upload object  
        success = await client.upload_object(
            's3://my-bucket',
            'data/new-file.txt',
            '/tmp/local-file.txt',
            metadata={'author': 'python-sdk'}
        )

asyncio.run(storage_example())
```

### Distributed Clusters

```python
from objectfs import Configuration, ObjectFSClient

# Configure cluster
config = Configuration()
config.cluster.enabled = True
config.cluster.listen_addr = '0.0.0.0:8080'
config.cluster.seed_nodes = ['node1.example.com:8080', 'node2.example.com:8080']

async def cluster_example():
    async with ObjectFSClient(config=config) as client:
        # Join cluster
        await client.join_cluster(config.cluster.seed_nodes)

        # Get cluster status
        status = await client.get_cluster_status()
        print(f"Cluster nodes: {status['node_count']}")
        print(f"Leader: {status['leader']}")

asyncio.run(cluster_example())
```

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
objectfs-python metrics --endpoint http://localhost:9090 --format table

# Generate configuration
objectfs-python config generate --preset production --output config.yaml

# Storage operations
objectfs-python storage list s3://my-bucket --prefix data/
objectfs-python storage download s3://my-bucket file.txt ./local-file.txt

# Unmount filesystem
objectfs-python unmount /mnt/objectfs
```

## Configuration

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
      enabled: true
      tiering_enabled: true

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
  enabled: true
  metrics_addr: :9090
  health_check_addr: :8081
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

- `mount(storage_uri, mount_point, config_overrides=None, foreground=False)`: Mount filesystem
- `unmount(mount_point)`: Unmount filesystem  
- `list_mounts()`: List active mounts
- `is_mounted(mount_point)`: Check if path is mounted
- `validate_config(config=None)`: Validate configuration
- `generate_config(preset='production', output_path=None)`: Generate configuration

#### Async Methods

- `get_health(endpoint=None)`: Get health status
- `get_metrics(endpoint=None)`: Collect metrics
- `get_performance_stats()`: Get performance statistics
- `list_objects(storage_uri, prefix=None, max_keys=1000)`: List storage objects
- `download_object(storage_uri, key, local_path)`: Download object
- `upload_object(storage_uri, key, local_path, metadata=None)`: Upload object
- `join_cluster(seed_nodes, node_config=None)`: Join distributed cluster
- `get_cluster_status()`: Get cluster status
- `clear_cache(cache_type=None, keys=None)`: Clear filesystem cache
- `warm_cache(paths, recursive=False)`: Pre-load cache

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

See the [examples](examples/) directory for more detailed usage examples:

- [Basic mounting](examples/basic_mount.py)
- [Async operations](examples/async_example.py)  
- [Configuration management](examples/config_example.py)
- [Storage operations](examples/storage_example.py)
- [Distributed clusters](examples/cluster_example.py)
- [Monitoring and metrics](examples/monitoring_example.py)

## Development

### Setup

```bash
git clone https://github.com/objectfs/objectfs.git
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

MIT License - see [LICENSE](../../LICENSE) for details.

## Contributing

See [CONTRIBUTING.md](../../CONTRIBUTING.md) for contribution guidelines.

## Support

- GitHub Issues: <https://github.com/objectfs/objectfs/issues>
- Documentation: <https://docs.objectfs.io/python>
- Community: <https://community.objectfs.io>
