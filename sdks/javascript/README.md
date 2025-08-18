# ObjectFS JavaScript/TypeScript SDK

High-performance POSIX filesystem for object storage with comprehensive
JavaScript/TypeScript API support.

## Features

- **TypeScript First**: Full TypeScript support with comprehensive type definitions
- **Easy Integration**: Simple, intuitive API for mounting and managing
  ObjectFS instances
- **Promise-based**: Modern async/await support for all operations
- **AWS S3 Deep Integration**: Optimized specifically for AWS S3 with intelligent tiering and cost management
- **Distributed Operations**: Built-in support for distributed clusters and replication
- **Monitoring & Metrics**: Comprehensive health checking and metrics collection
- **Event-driven**: EventEmitter-based architecture for real-time updates
- **Configuration Management**: Flexible configuration with presets and validation

## Installation

```bash
npm install @objectfs/sdk
```

For TypeScript projects:

```bash
npm install @objectfs/sdk @types/node
```

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

```javascript
async function storageExample() {
  const client = new ObjectFSClient();

  // List objects
  const result = await client.listObjects('s3://my-bucket', {
    prefix: 'data/',
    maxKeys: 100,
  });

  console.log(`Found ${result.objects.length} objects`);

  // Download object
  const bytes = await client.downloadObject(
    's3://my-bucket',
    'data/file.txt',
    '/tmp/downloaded-file.txt',
    {
      progressCallback: (downloaded, total) => {
        console.log(`Downloaded ${downloaded}/${total} bytes`);
      },
    }
  );

  // Upload object
  const success = await client.uploadObject(
    's3://my-bucket',
    'data/new-file.txt',
    '/tmp/local-file.txt',
    {
      metadata: { author: 'javascript-sdk' },
      progressCallback: (uploaded, total) => {
        console.log(`Uploaded ${uploaded}/${total} bytes`);
      },
    }
  );

  await client.close();
}
```

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

const client = new ObjectFSClient({ config });

// Join cluster
await client.joinCluster(['node1.example.com:8080', 'node2.example.com:8080']);

// Monitor cluster changes
client.on('cluster_change', (data) => {
  console.log(`Cluster change: ${data.action}`);
});

// Get cluster status
const status = await client.getClusterStatus();
console.log(`Cluster has ${status.nodeCount} nodes, leader: ${status.leader}`);
```

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

```typescript
// List objects
listObjects(storageUri: string, options?: ListObjectsOptions): Promise<ListObjectsResult>

// Get object information
getObjectInfo(storageUri: string, key: string): Promise<ObjectInfo>

// Download object
downloadObject(
  storageUri: string,
  key: string,
  localPath: string,
  options?: DownloadOptions
): Promise<number>

// Upload object
uploadObject(
  storageUri: string,
  key: string,
  localPath: string,
  options?: UploadOptions
): Promise<boolean>
```

#### Monitoring

```typescript
// Get health status
getHealth(endpoint?: string): Promise<HealthStatus>

// Get metrics
getMetrics(endpoint?: string): Promise<Metrics>

// Get performance statistics
getPerformanceStats(): Promise<PerformanceStats>

// Start monitoring (enables events)
startMonitoring(interval?: number): Promise<void>
```

#### Distributed Operations

```typescript
// Join cluster
joinCluster(seedNodes: string[], options?: JoinClusterOptions): Promise<boolean>

// Leave cluster
leaveCluster(): Promise<boolean>

// Get cluster status
getClusterStatus(): Promise<ClusterStatus>
```

#### Cache Management

```typescript
// Clear cache
clearCache(options?: CacheOptions): Promise<CacheClearResult>

// Warm cache
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

// Instance methods
merge(overrides: Partial<Configuration>): Configuration
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
- `health_change` - Health status changed
- `metrics_updated` - Metrics updated
- `cluster_change` - Cluster membership changed
- `error` - Error occurred

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

```yaml
# objectfs.yaml
global:
  logLevel: INFO
  logFile: /var/log/objectfs.log

storage:
  s3:
    region: us-east-1
    useAcceleration: true
    costOptimization:
      enabled: true
      tieringEnabled: true

performance:
  cacheSize: 8GB
  maxConcurrency: 500
  multilevelCaching: true

cluster:
  enabled: false
  replicationFactor: 3
  consistencyLevel: eventual

monitoring:
  enabled: true
  metricsAddr: ':9090'
  healthCheckAddr: ':8081'
```

### Environment Variables

```bash
export OBJECTFS_LOG_LEVEL=DEBUG
export OBJECTFS_CACHE_SIZE=16GB  
export OBJECTFS_S3_REGION=us-west-2
export OBJECTFS_CLUSTER_ENABLED=true
```

## Examples

See the [examples](examples/) directory for complete examples:

- [Basic mounting](examples/basic-mount.js)
- [TypeScript usage](examples/typescript-example.ts)
- [Configuration management](examples/config-example.js)
- [Storage operations](examples/storage-example.js)
- [Event handling](examples/events-example.js)
- [Distributed clusters](examples/cluster-example.js)

## Development

### Setup

```bash
git clone https://github.com/objectfs/objectfs.git
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

# Run specific test file
npm test -- mount.test.js

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

MIT License - see [LICENSE](../../LICENSE) for details.

## Contributing

See [CONTRIBUTING.md](../../CONTRIBUTING.md) for contribution guidelines.

## Support

- GitHub Issues: <https://github.com/objectfs/objectfs/issues>
- Documentation: <https://docs.objectfs.io/javascript>
- NPM Package: <https://www.npmjs.com/package/@objectfs/sdk>
