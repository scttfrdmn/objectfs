/**
 * ObjectFS JavaScript Client
 *
 * Main client class for interacting with ObjectFS instances.
 */

import { execSync } from 'child_process';
import { EventEmitter } from 'eventemitter3';
import { Configuration } from './config';
import { MountManager } from './mount';
import { S3StorageAdapter } from './storage';
import { MetricsCollector, HealthChecker } from './monitoring';
import {
  ObjectFSError,
  MountError,
  ConfigurationError,
  StorageError,
  DistributedError,
  CacheError,
} from './errors';
import {
  ClientOptions,
  MountOptions,
  UnmountOptions,
  MountInfo,
  HealthStatus,
  Metrics,
  PerformanceStats,
  ClusterStatus,
  JoinClusterOptions,
  ListObjectsOptions,
  ListObjectsResult,
  ObjectInfo,
  UploadOptions,
  DownloadOptions,
  CacheOptions,
  WarmCacheOptions,
  CacheClearResult,
  WarmCacheResult,
} from './types';

export class ObjectFSClient extends EventEmitter {
  private config: Configuration;
  private binaryPath: string;
  // Assigned unconditionally in the constructor, so `string | undefined` rather than `?:` —
  // see the note on ObjectFSError.code in errors.ts.
  private apiEndpoint: string | undefined;
  private timeout: number;
  private retries: number;

  private mountManager: MountManager;
  private storageAdapter: S3StorageAdapter;
  private metricsCollector: MetricsCollector;
  private healthChecker: HealthChecker;

  private processes: Map<string, any> = new Map();
  private closed = false;

  constructor(options: ClientOptions = {}) {
    super();

    this.config = this.loadConfig(options.config);
    this.binaryPath = options.binaryPath || this.findBinary();
    this.apiEndpoint = options.apiEndpoint;
    this.timeout = options.timeout || 30000;
    this.retries = options.retries || 3;

    this.mountManager = new MountManager(this.binaryPath, this.config);
    this.storageAdapter = new S3StorageAdapter(this.config.storage);
    this.metricsCollector = new MetricsCollector(this.timeout);
    this.healthChecker = new HealthChecker(this.timeout, this.retries);

    // Set up cleanup on process exit
    process.on('exit', () => this.cleanup());
    process.on('SIGINT', () => this.cleanup());
    process.on('SIGTERM', () => this.cleanup());
  }

  private loadConfig(
    config?: Configuration | string | Record<string, any>
  ): Configuration {
    if (!config) {
      return new Configuration();
    } else if (config instanceof Configuration) {
      return config;
    } else if (typeof config === 'string') {
      return Configuration.fromFile(config);
    } else if (typeof config === 'object') {
      return Configuration.fromObject(config);
    } else {
      throw new ConfigurationError(
        `Invalid configuration type: ${typeof config}`
      );
    }
  }

  private findBinary(): string {
    try {
      const result = execSync('which objectfs', { encoding: 'utf8' });
      return result.trim();
    } catch (error) {
      throw new ObjectFSError(
        'ObjectFS binary not found in PATH. Please install ObjectFS or ' +
          'specify binaryPath in options.'
      );
    }
  }

  // Mount Management

  /**
   * Mount ObjectFS filesystem
   */
  async mount(
    storageUri: string,
    mountPoint: string,
    options: MountOptions = {}
  ): Promise<string> {
    if (this.closed) {
      throw new ObjectFSError('Client is closed');
    }

    try {
      const effectiveConfig = options.configOverrides
        ? this.config.merge(options.configOverrides)
        : this.config;

      const process = await this.mountManager.mount(
        storageUri,
        mountPoint,
        effectiveConfig,
        options
      );

      const mountId = `${storageUri}:${mountPoint}`;
      if (!options.foreground) {
        this.processes.set(mountId, process);
      }

      this.emit('mount', { storageUri, mountPoint, mountId });
      return mountId;
    } catch (error) {
      throw new MountError(`Failed to mount ${storageUri}: ${error}`);
    }
  }

  /**
   * Unmount ObjectFS filesystem
   */
  async unmount(
    mountPoint: string,
    options: UnmountOptions = {}
  ): Promise<boolean> {
    try {
      const result = await this.mountManager.unmount(mountPoint, options);

      // Remove from tracked processes
      for (const mountId of this.processes.keys()) {
        if (mountId.includes(mountPoint)) {
          this.processes.delete(mountId);
          break;
        }
      }

      if (result) {
        this.emit('unmount', { mountPoint });
      }

      return result;
    } catch (error) {
      console.error(`Failed to unmount ${mountPoint}: ${error}`);
      return false;
    }
  }

  /**
   * List active ObjectFS mounts
   */
  async listMounts(): Promise<MountInfo[]> {
    return this.mountManager.listMounts();
  }

  /**
   * Check if directory is mounted with ObjectFS
   */
  async isMounted(mountPoint: string): Promise<boolean> {
    return this.mountManager.isMounted(mountPoint);
  }

  // Configuration Management

  /**
   * Validate configuration
   */
  validateConfig(config?: Configuration): boolean {
    const targetConfig = config || this.config;
    try {
      targetConfig.validate();
      return true;
    } catch (error) {
      console.error(`Configuration validation failed: ${error}`);
      return false;
    }
  }

  /**
   * Generate configuration from preset
   */
  generateConfig(preset = 'production', outputPath?: string): string {
    const config = Configuration.fromPreset(preset as any);
    const yamlContent = config.toYAML();

    if (outputPath) {
      config.saveToFile(outputPath);
      console.log(`Generated configuration saved to ${outputPath}`);
    }

    return yamlContent;
  }

  // Storage Operations

  /**
   * List objects in storage
   */
  async listObjects(
    storageUri: string,
    options: ListObjectsOptions = {}
  ): Promise<ListObjectsResult> {
    try {
      return await this.storageAdapter.listObjects(storageUri, options);
    } catch (error) {
      throw new StorageError(`Failed to list objects: ${error}`);
    }
  }

  /**
   * Get object information
   */
  async getObjectInfo(storageUri: string, key: string): Promise<ObjectInfo> {
    try {
      return await this.storageAdapter.getObjectInfo(storageUri, key);
    } catch (error) {
      throw new StorageError(`Failed to get object info: ${error}`);
    }
  }

  /**
   * Download object to local file
   */
  async downloadObject(
    storageUri: string,
    key: string,
    localPath: string,
    options: DownloadOptions = {}
  ): Promise<number> {
    try {
      return await this.storageAdapter.downloadObject(
        storageUri,
        key,
        localPath,
        options
      );
    } catch (error) {
      throw new StorageError(`Failed to download object: ${error}`);
    }
  }

  /**
   * Upload local file to storage
   */
  async uploadObject(
    storageUri: string,
    key: string,
    localPath: string,
    options: UploadOptions = {}
  ): Promise<boolean> {
    try {
      return await this.storageAdapter.uploadObject(
        storageUri,
        key,
        localPath,
        options
      );
    } catch (error) {
      throw new StorageError(`Failed to upload object: ${error}`);
    }
  }

  // Monitoring and Health

  /**
   * Get health status of ObjectFS instance
   */
  async getHealth(endpoint?: string): Promise<HealthStatus> {
    const targetEndpoint = endpoint || this.apiEndpoint;
    if (!targetEndpoint) {
      throw new ObjectFSError('No API endpoint configured');
    }

    return this.healthChecker.getHealth(targetEndpoint);
  }

  /**
   * Get metrics from ObjectFS instance
   */
  async getMetrics(endpoint?: string): Promise<Metrics> {
    const targetEndpoint = endpoint || this.apiEndpoint;
    if (!targetEndpoint) {
      throw new ObjectFSError('No API endpoint configured');
    }

    return this.metricsCollector.collectMetrics(targetEndpoint);
  }

  /**
   * Not implemented. Always throws.
   *
   * This returned hardcoded constants -- a cache hit rate of 0.85, 1000 read operations,
   * 1500 requests, 50.5 ms latency -- the same numbers on every call, from a fresh client,
   * with nothing mounted. They were indistinguishable from telemetry, and a dashboard built
   * on them would have shown a healthy filesystem regardless of what the filesystem was
   * doing.
   *
   * Use `getMetrics()`, which reaches the mount's real Prometheus endpoint.
   */
  async getPerformanceStats(): Promise<PerformanceStats> {
    throw new ObjectFSError(
      'getPerformanceStats is not implemented. It previously returned fixed constants ' +
        "that looked like measurements. Use getMetrics(), which reads the mount's " +
        'Prometheus endpoint.'
    );
  }

  // Distributed Operations

  /**
   * Join a distributed cluster
   */
  async joinCluster(
    seedNodes: string[],
    options: JoinClusterOptions = {}
  ): Promise<boolean> {
    // Reported success for work it never did: it merged options.nodeConfig into a local variable,
    // discarded it, console.log'd, emitted a 'cluster_change' event, and returned true. A caller
    // was told the node had joined a cluster it had never contacted. There is no cluster management
    // API in this SDK to contact -- see #325 for the same pattern in the storage adapter.
    void seedNodes;
    void options;
    throw new DistributedError(
      'joinCluster is not implemented. It previously returned true without contacting any node. ' +
        'ObjectFS cluster membership is configured on the daemon (see the cluster section of the ' +
        'YAML config); this SDK has no control-plane client. ' +
        'https://github.com/scttfrdmn/objectfs/issues/325'
    );
  }

  /**
   * Leave distributed cluster.
   *
   * @throws DistributedError always — see joinCluster.
   */
  async leaveCluster(): Promise<boolean> {
    throw new DistributedError(
      'leaveCluster is not implemented. It previously returned true without contacting any node. ' +
        'https://github.com/scttfrdmn/objectfs/issues/325'
    );
  }

  /**
   * Get cluster status information.
   *
   * @throws DistributedError always — see joinCluster.
   */
  async getClusterStatus(): Promise<ClusterStatus> {
    // Returned `{nodeCount: 1, leader: 'self', status: 'healthy', nodes: []}` unconditionally --
    // a healthy single-node cluster with no nodes in it, for any configuration, with no query
    // performed. 'healthy' from a function that cannot observe health is the worst of the four.
    throw new DistributedError(
      'getClusterStatus is not implemented. It previously reported a healthy single-node cluster ' +
        'without querying anything. https://github.com/scttfrdmn/objectfs/issues/325'
    );
  }

  // Cache Management

  /**
   * Clear filesystem cache.
   *
   * @throws CacheError always — see #325.
   */
  async clearCache(options: CacheOptions = {}): Promise<CacheClearResult> {
    // `return {success: true}` after a console.log. The try/catch around it could not fail, so the
    // documented `{success: false, message}` branch was unreachable.
    void options;
    throw new CacheError(
      'clearCache is not implemented. It previously returned {success: true} after logging, ' +
        'without clearing anything. https://github.com/scttfrdmn/objectfs/issues/325'
    );
  }

  /**
   * Warm cache with specified paths.
   *
   * @throws CacheError always — see #325.
   */
  async warmCache(
    paths: string[],
    options: WarmCacheOptions = {}
  ): Promise<WarmCacheResult> {
    // Set `results[path] = true` for every path given, so the result was a function of the input
    // alone and every path always succeeded.
    void paths;
    void options;
    throw new CacheError(
      'warmCache is not implemented. It previously reported success for every path given, ' +
        'without warming anything. https://github.com/scttfrdmn/objectfs/issues/325'
    );
  }

  // Event Management

  /**
   * Start monitoring and event emission
   */
  async startMonitoring(interval = 10000): Promise<void> {
    if (!this.apiEndpoint) {
      console.warn('No API endpoint configured, monitoring disabled');
      return;
    }

    const monitor = async () => {
      if (this.closed) return;

      try {
        const [health, metrics] = await Promise.all([
          this.getHealth(),
          this.getMetrics(),
        ]);

        this.emit('health_change', health);
        this.emit('metrics_updated', metrics);
      } catch (error) {
        this.emit('error', { type: 'monitoring', error });
      }

      if (!this.closed) {
        setTimeout(monitor, interval);
      }
    };

    monitor();
  }

  // Lifecycle Management

  /**
   * Close client and cleanup resources
   */
  async close(): Promise<void> {
    if (this.closed) return;

    this.closed = true;

    // Stop all managed processes
    for (const [mountId, process] of this.processes) {
      console.log(`Stopping ObjectFS process for ${mountId}`);
      try {
        process.kill('SIGTERM');
        // Wait for graceful shutdown
        await new Promise((resolve) => {
          const timeout = setTimeout(() => {
            process.kill('SIGKILL');
            resolve(undefined);
          }, 10000);

          process.on('exit', () => {
            clearTimeout(timeout);
            resolve(undefined);
          });
        });
      } catch (error) {
        console.error(`Error stopping process for ${mountId}: ${error}`);
      }
    }

    this.processes.clear();
    this.removeAllListeners();
  }

  private cleanup(): void {
    if (!this.closed) {
      this.close().catch((error) => {
        console.error('Error during cleanup:', error);
      });
    }
  }
}

// Convenience functions

/**
 * Create ObjectFS client with optional configuration file
 */
export function createClient(
  configPath?: string,
  options: Partial<ClientOptions> = {}
): ObjectFSClient {
  // `{...options, config}` with config possibly undefined would *set* config to undefined and so
  // override a config the caller passed in options. Omitting the key when there is no path both
  // satisfies exactOptionalPropertyTypes and preserves options.config, which is what a caller
  // passing both a file and an options object would expect.
  if (configPath === undefined) {
    return new ObjectFSClient(options);
  }
  return new ObjectFSClient({
    ...options,
    config: Configuration.fromFile(configPath),
  });
}

/**
 * Quick mount function for simple use cases
 */
export async function mountStorage(
  storageUri: string,
  mountPoint: string,
  config?: Record<string, any>,
  options: MountOptions = {}
): Promise<ObjectFSClient> {
  const client = new ObjectFSClient();
  // As in createClient: omit configOverrides rather than assign undefined to it.
  await client.mount(
    storageUri,
    mountPoint,
    config === undefined ? options : { ...options, configOverrides: config }
  );
  return client;
}
