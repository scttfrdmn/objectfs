# ObjectFS JavaScript/TypeScript SDK

High-performance POSIX filesystem for object storage with comprehensive
JavaScript/TypeScript API support.

## Features

What this SDK does is drive the `objectfs` binary and read what a running mount reports. It has no
S3 client of its own and no control-plane client.

- **Mount management**: spawn, unmount, enumerate and check ObjectFS mounts
- **Configuration management**: build, merge, validate and emit the daemon's YAML, with presets
- **Monitoring**: health checks and metrics scraped from the mount's Prometheus endpoint
- **Event-driven**: EventEmitter-based, for mount/unmount and monitoring events
- **TypeScript first**: type definitions for the whole surface, compiled by `npm run build`

Not implemented, and throwing rather than pretending: storage operations, cluster membership, and
cache control. See [#325](https://github.com/scttfrdmn/objectfs/issues/325) and the API reference
below — this list previously advertised "AWS S3 deep integration with intelligent tiering and cost
management" and "built-in support for distributed clusters and replication", neither of which any
code here has ever done.

## Installation

> **`@objectfs/sdk` is not on npm.** The registry returns 404 for it, and no workflow in this
> repository publishes it, so `npm install @objectfs/sdk` fails. Unlike the Python SDK — where the
> name is taken by an unrelated package and the install silently succeeds with someone else's code —
> this one at least fails loudly. Install from this repository instead. The `@objectfs/sdk` import
> specifier in the examples below is what the package name will be if it is published; today, use a
> relative import or `npm link`.

```bash
git clone https://github.com/scttfrdmn/objectfs.git
cd objectfs/sdks/javascript
npm install && npm run build
```

For TypeScript projects, add `@types/node`.

## Quick Start

### Basic Usage

```javascript
const { ObjectFSClient } = require('@objectfs/sdk');

// Create client
const client = new ObjectFSClient();

// Mount filesystem
const mountId = await client.mount('s3://my-bucket', '/mnt/objectfs');
console.log(`Mounted with ID: ${mountId}`);

// Use the filesystem
// Files in /mnt/objectfs are now backed by S3

// Unmount when done
await client.unmount('/mnt/objectfs');
await client.close();
```

### TypeScript Usage

```typescript
import { ObjectFSClient, Configuration } from '@objectfs/sdk';

const client = new ObjectFSClient({
  config: Configuration.fromPreset('production'),
  apiEndpoint: 'http://localhost:8081',
});

// Mount with options
await client.mount('s3://my-bucket', '/mnt/objectfs', {
  foreground: false,
  configOverrides: {
    performance: {
      cacheSize: '8GB',
      maxConcurrency: 500,
    },
  },
});

// Monitor health and metrics
client.on('health_change', (health) => {
  console.log(`Health status: ${health.status}`);
});

await client.startMonitoring();
```

### Configuration Management

```javascript
const { Configuration } = require('@objectfs/sdk');

// Load from file
const config = Configuration.fromFile('objectfs.yaml');

// Create from preset
const config = Configuration.fromPreset('high-performance');

// Create programmatically
const config = new Configuration({
  performance: {
    cacheSize: '16GB',
    maxConcurrency: 1000,
    predictiveCaching: true,
  },
  storage: {
    s3: {
      region: 'us-west-2',
      useAcceleration: true,
    },
  },
});

const client = new ObjectFSClient({ config });
```

### Storage Operations

**Not implemented.** `listObjects`, `getObjectInfo`, `downloadObject`, `uploadObject` and
`deleteObject` throw a `StorageError`. They used to return fabricated data — two invented objects
from `listObjects`, a fixed size and etag from `getObjectInfo`, `true` from `uploadObject` and
`deleteObject` for uploads and deletions that never happened — and `downloadObject` wrote
`Simulated file content from S3` over whatever file was at the local path it was given, then
reported a successful 30-byte transfer. This section documented all of it as working.

Tracked as [#325](https://github.com/scttfrdmn/objectfs/issues/325), which also covers the identical
code in the Python SDK. Until it lands, use the AWS SDK directly:

```javascript
import { S3Client, GetObjectCommand } from '@aws-sdk/client-s3';
```

or mount the bucket and use ordinary filesystem calls, which is what ObjectFS is for:

```javascript
const client = new ObjectFSClient();
await client.mount('s3://my-bucket', '/mnt/data');
const contents = await fs.promises.readFile('/mnt/data/data/file.txt');
await client.unmount('/mnt/data');
```

### Cluster and Cache Operations

**Not implemented**, same as above and same issue. `joinCluster`, `leaveCluster`,
`getClusterStatus`, `clearCache` and `warmCache` throw. `getClusterStatus` previously reported
`{nodeCount: 1, leader: 'self', status: 'healthy'}` for any configuration without querying
anything. Cluster membership is configured on the daemon, in the `cluster` section of the YAML
config; this SDK has no control-plane client.

### Event Handling

```javascript
const client = new ObjectFSClient();

// Listen to events
client.on('mount', (data) => {
  console.log(`Filesystem mounted: ${data.mountId}`);
});

client.on('unmount', (data) => {
  console.log(`Filesystem unmounted: ${data.mountPoint}`);
});

client.on('health_change', (health) => {
  if (health.status !== 'healthy') {
    console.warn(`Health status changed: ${health.status}`);
  }
});

client.on('error', (error) => {
  console.error('ObjectFS error:', error);
});

// Start monitoring to enable health/metrics events
await client.startMonitoring(5000); // Check every 5 seconds
```

### Distributed Clusters

What this SDK can do is *describe* a cluster — the config section is real and reaches the daemon
through the generated YAML. Membership operations (`joinCluster`, `getClusterStatus`) are not
implemented, and the `cluster_change` event is never emitted, so the last three statements of the
example that used to be here could not work:

```javascript
const { Configuration } = require('@objectfs/sdk');

const config = new Configuration({
  cluster: {
    enabled: true,
    listenAddr: '0.0.0.0:8080',
    seedNodes: ['node1.example.com:8080', 'node2.example.com:8080'],
    replicationFactor: 3,
    consistencyLevel: 'strong',
  },
});

// Written out, this is what the daemon reads. Mounting with it is how a node joins.
config.saveToFile('objectfs-cluster.yaml');
const client = new ObjectFSClient({ config });
await client.mount('s3://my-bucket', '/mnt/objectfs');
```

ObjectFS's own distributed layer is experimental (`internal/distributed`); see the top-level
README before depending on it for anything.

## API Reference

### ObjectFSClient

Main client class for interacting with ObjectFS.

#### Constructor

```typescript
new ObjectFSClient(options?: ClientOptions)
```

Options:

- `config?: Configuration | string | object` - Configuration object, file
  path, or plain object
- `binaryPath?: string` - Path to ObjectFS binary (default: searches PATH)
- `apiEndpoint?: string` - API endpoint for remote ObjectFS instances
- `timeout?: number` - Request timeout in milliseconds (default: 30000)
- `retries?: number` - Number of retry attempts (default: 3)

#### Mount Management

```typescript
// Mount filesystem
mount(storageUri: string, mountPoint: string, options?: MountOptions): Promise<string>

// Unmount filesystem
unmount(mountPoint: string, options?: UnmountOptions): Promise<boolean>

// List active mounts
listMounts(): Promise<MountInfo[]>

// Check if mounted
isMounted(mountPoint: string): Promise<boolean>
```

#### Configuration

```typescript
// Validate configuration
validateConfig(config?: Configuration): boolean

// Generate configuration from preset
generateConfig(preset?: string, outputPath?: string): string
```

#### Storage Methods

**All four throw `StorageError` — not implemented ([#325][i325]).** The signatures are what they
will have if they are implemented; nothing behind them talks to S3.

```typescript
// Not implemented; throws. Returned two invented objects, 'file1.txt' and 'file2.txt'.
listObjects(storageUri: string, options?: ListObjectsOptions): Promise<ListObjectsResult>

// Not implemented; throws. Returned a fixed size and etag for any key, existing or not.
getObjectInfo(storageUri: string, key: string): Promise<ObjectInfo>

// Not implemented; throws. Wrote 'Simulated file content from S3' over localPath, called
// progressCallback(30, 30), and returned 30 -- destroying an existing file and reporting success.
downloadObject(
  storageUri: string,
  key: string,
  localPath: string,
  options?: DownloadOptions
): Promise<number>

// Not implemented; throws. Returned true without transferring anything.
uploadObject(
  storageUri: string,
  key: string,
  localPath: string,
  options?: UploadOptions
): Promise<boolean>
```

`S3StorageAdapter` additionally has `deleteObject(storageUri, key)`, which the client does not
expose; it throws too, and previously returned `true` without deleting anything.

[i325]: https://github.com/scttfrdmn/objectfs/issues/325

#### Monitoring

```typescript
// Get health status
getHealth(endpoint?: string): Promise<HealthStatus>

// Get metrics from the mount's Prometheus endpoint (monitoring.metrics.addr, default
// 127.0.0.1:8080; requires monitoring.metrics.enabled: true). Returns cache, io, operations,
// errors and connections sections plus the parsed raw samples. A section is absent when the
// mount has not recorded that family -- absent is not zero.
getMetrics(endpoint?: string): Promise<Metrics>

// Not implemented; always throws. It returned fixed constants that looked like
// measurements. Use getMetrics().
getPerformanceStats(): Promise<PerformanceStats>

// Start monitoring (enables events)
startMonitoring(interval?: number): Promise<void>
```

#### Distributed Operations

**All three throw `DistributedError` — not implemented ([#325][i325]).** Cluster membership is
configured on the daemon; this SDK has no control-plane client.

```typescript
// Not implemented; throws. Returned true without contacting any node.
joinCluster(seedNodes: string[], options?: JoinClusterOptions): Promise<boolean>

// Not implemented; throws. Same.
leaveCluster(): Promise<boolean>

// Not implemented; throws. Reported {nodeCount: 1, leader: 'self', status: 'healthy'} for any
// configuration, without querying anything.
getClusterStatus(): Promise<ClusterStatus>
```

#### Cache Management

**Both throw `CacheError` — not implemented ([#325][i325]).**

```typescript
// Not implemented; throws. Returned {success: true} after a console.log.
clearCache(options?: CacheOptions): Promise<CacheClearResult>

// Not implemented; throws. Reported success for every path given, warming nothing.
warmCache(paths: string[], options?: WarmCacheOptions): Promise<WarmCacheResult>
```

### Configuration Classes

#### Configuration Class

Main configuration class with methods:

```typescript
// Factory methods
static fromFile(filePath: string): Configuration
static fromObject(data: any): Configuration
static fromPreset(preset: ConfigurationPreset): Configuration
static fromEnv(prefix?: string): Configuration

// Instance methods. `merge` and the constructor take DeepPartial, and merge deeply: naming
// storage.s3.region leaves the rest of storage.s3 at its default rather than replacing the
// section. A one-level spread here is what made two presets fail their own validate().
merge(overrides: DeepPartial<Configuration>): Configuration
toObject(): any
toYAML(): string
saveToFile(filePath: string): void
validate(): void
```

#### Configuration Presets

- `development` - Debug logging, smaller cache
- `production` - Optimized for production with monitoring
- `high-performance` - Maximum performance settings
- `cost-optimized` - Minimized costs with intelligent tiering
- `cluster` - Distributed cluster configuration

### Events

The client emits the following events:

- `mount` - Filesystem mounted
- `unmount` - Filesystem unmounted
- `health_change` - Health status changed; emitted by `startMonitoring`, which needs `apiEndpoint`
- `metrics_updated` - Metrics updated; same
- `error` - Error occurred

`cluster_change` is declared in `EventType` but nothing emits it: the only `emit('cluster_change')`
was in `joinCluster`, which now throws (see above). It is listed in the type for when cluster
operations are implemented.

### Error Handling

```typescript
import {
  ObjectFSError,        // Base error
  ConfigurationError,   // Configuration issues
  MountError,          // Mount/unmount failures
  StorageError,        // Storage operation failures
  DistributedError,    // Cluster operation failures
  NetworkError,        // Network connectivity issues
} from '@objectfs/sdk';

try {
  await client.mount('s3://invalid-bucket', '/mnt/objectfs');
} catch (error) {
  if (error instanceof MountError) {
    console.error('Mount failed:', error.message);
  } else if (error instanceof ConfigurationError) {
    console.error('Configuration error:', error.message);
  }
}
```

## Configuration Reference

### Configuration File

The config file is YAML with `snake_case` keys — the JavaScript API is camelCase, the file is not,
and a camelCase key in the file is rejected at startup with the key named.

```yaml
# objectfs.yaml
global:
  log_level: INFO
  log_file: /var/log/objectfs.log

storage:
  s3:
    region: us-east-1
    use_acceleration: true

performance:
  cache_size: 8GB
  max_concurrency: 500

cache:
  ttl: 5m
  eviction_policy: weighted_lru

monitoring:
  metrics:
    enabled: true
    prometheus: true
  health_checks:
    enabled: true
    interval: 30s
```

See [`examples/config.yaml`](../../examples/config.yaml) for the complete schema, including which
keys are not yet read on the mount path.

### Environment Variables

```bash
export OBJECTFS_LOG_LEVEL=DEBUG
export OBJECTFS_CACHE_SIZE=16GB  
export OBJECTFS_S3_REGION=us-west-2
export OBJECTFS_CLUSTER_ENABLED=true
```

## Examples

There is no `examples/` directory. This section listed six files in one, and none of them was ever
written — the runnable examples are the inline ones above:

- [Basic mounting](#basic-usage)
- [TypeScript usage](#typescript-usage)
- [Configuration management](#configuration-management)
- [Event handling](#event-handling)
- [Distributed clusters](#distributed-clusters) — note that multi-node coordination is
  experimental and not reachable from a mount today

## Development

### Setup

```bash
git clone https://github.com/scttfrdmn/objectfs.git
cd objectfs/sdks/javascript

# Install dependencies
npm install

# Build TypeScript
npm run build

# Run tests
npm test
```

### Scripts

```bash
npm run build          # Compile TypeScript
npm run build:watch    # Watch mode compilation
npm test              # Run tests
npm run test:watch    # Watch mode testing  
npm run test:coverage # Test with coverage
npm run lint          # Lint code
npm run format        # Format code
npm run docs          # Generate documentation
```

### Testing

```bash
# Run all tests
npm test

# Run specific test file. The suites are src/config.test.ts, src/prometheus.test.ts and
# src/storage.test.ts; there is no mount.test.js, which is what this line used to name.
npm test -- src/config.test.ts

# Run with coverage
npm run test:coverage

# Watch mode
npm run test:watch
```

## Browser Support

The SDK is designed for Node.js environments. For browser usage, you'll
need to:

1. Use a bundler like Webpack or Rollup
2. Provide polyfills for Node.js modules (`fs`, `child_process`, etc.)
3. Note that actual filesystem mounting won't work in browsers

## License

Apache License 2.0 - see [LICENSE](../../LICENSE) for details.

## Contributing

See [CONTRIBUTING.md](../../CONTRIBUTING.md) for contribution guidelines.

## Support

- GitHub Issues: <https://github.com/scttfrdmn/objectfs/issues>
- Documentation: the `docs/` tree in this repository
- NPM Package: not published — see [Installation](#installation)
