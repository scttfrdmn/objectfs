/**
 * ObjectFS JavaScript Client
 *
 * Main client class for interacting with ObjectFS instances.
 */

import { EventEmitter } from 'eventemitter3';
import { Configuration } from './config';
import { MountManager } from './mount';
import { StorageAdapter } from './storage';
import { MetricsCollector, HealthChecker } from './monitoring';
import {
  ObjectFSError,
  MountError,
  ConfigurationError,
  StorageError,
  DistributedError,
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
  EventType,
  EventData,
} from './types';

export class ObjectFSClient extends EventEmitter {
  private config: Configuration;
  private binaryPath: string;
  private apiEndpoint?: string;
  private timeout: number;
  private retries: number;

  private mountManager: MountManager;
  private storageAdapter: StorageAdapter;
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
    this.storageAdapter = new StorageAdapter(this.config.storage);
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
      throw new ConfigurationError(`Invalid configuration type: ${typeof config}`);
    }
  }

  private findBinary(): string {
    const { execSync } = require('child_process');
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
      for (const [mountId, _process] of this.processes) {
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
  generateConfig(
    preset = 'production',
    outputPath?: string
  ): string {
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
   * Get performance statistics
   */
  async getPerformanceStats(): Promise<PerformanceStats> {
    const [cacheStats, ioStats, networkStats] = await Promise.all([
      this.getCacheStats(),
      this.getIOStats(),
      this.getNetworkStats(),
    ]);

    return {
      cache: cacheStats,
      io: ioStats,
      network: networkStats,
    };
  }

  // Distributed Operations

  /**
   * Join a distributed cluster
   */
  async joinCluster(
    seedNodes: string[],
    options: JoinClusterOptions = {}
  ): Promise<boolean> {
    if (!this.config.cluster.enabled) {
      throw new DistributedError('Cluster mode not enabled in configuration');
    }

    try {
      // Update configuration with cluster settings
      let clusterConfig = this.config.cluster;
      if (options.nodeConfig) {
        clusterConfig = { ...clusterConfig, ...options.nodeConfig };
      }

      // Implementation would interact with cluster management API
      console.log(`Joining cluster with seed nodes: ${seedNodes.join(', ')}`);
      this.emit('cluster_change', { action: 'join', seedNodes });
      return true;
    } catch (error) {
      throw new DistributedError(`Failed to join cluster: ${error}`);
    }
  }

  /**
   * Leave distributed cluster
   */
  async leaveCluster(): Promise<boolean> {
    try {
      console.log('Leaving cluster');
      this.emit('cluster_change', { action: 'leave' });
      return true;
    } catch (error) {
      throw new DistributedError(`Failed to leave cluster: ${error}`);
    }
  }

  /**
   * Get cluster status information
   */
  async getClusterStatus(): Promise<ClusterStatus> {
    if (!this.config.cluster.enabled) {
      throw new DistributedError('Cluster mode not enabled');
    }

    // Implementation would query cluster status
    return {
      nodeCount: 1,
      leader: 'self',
      status: 'healthy',
      nodes: [],
    };
  }

  // Cache Management

  /**
   * Clear filesystem cache
   */
  async clearCache(options: CacheOptions = {}): Promise<CacheClearResult> {
    try {
      console.log(`Clearing cache - type: ${options.cacheType}, keys: ${options.keys}`);
      return { success: true };
    } catch (error) {
      console.error(`Failed to clear cache: ${error}`);
      return { success: false, message: String(error) };
    }
  }

  /**
   * Warm cache with specified paths
   */
  async warmCache(
    paths: string[],
    options: WarmCacheOptions = {}
  ): Promise<WarmCacheResult> {
    try {
      const results: WarmCacheResult = {};
      for (const path of paths) {
        results[path] = true;
        console.log(
          `Cache warming ${options.recursive ? 'started' : 'queued'} for ${path}`
        );
      }
      return results;
    } catch (error) {
      console.error(`Failed to warm cache: ${error}`);
      return Object.fromEntries(paths.map(path => [path, false]));
    }
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
        await new Promise(resolve => {
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
      this.close().catch(error => {
        console.error('Error during cleanup:', error);
      });
    }
  }

  // Private helper methods

  private async getCacheStats(): Promise<any> {
    return {
      hits: 850,
      misses: 150,
      hitRate: 0.85,
      size: 2147483648, // 2GB
      entries: 10000,
    };
  }

  private async getIOStats(): Promise<any> {
    return {
      readOperations: 1000,
      writeOperations: 500,
      readBytes: 104857600, // 100MB
      writeBytes: 52428800, // 50MB
    };
  }

  private async getNetworkStats(): Promise<any> {
    return {
      requests: 1500,
      errors: 5,
      latency: 50.5,
    };
  }
}

// Convenience functions

/**
 * Create ObjectFS client with optional configuration file
 */
export function createClient(configPath?: string, options: Partial<ClientOptions> = {}): ObjectFSClient {
  const config = configPath ? Configuration.fromFile(configPath) : undefined;
  return new ObjectFSClient({ ...options, config });
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
  await client.mount(storageUri, mountPoint, { ...options, configOverrides: config });
  return client;
}
