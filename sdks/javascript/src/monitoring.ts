/**
 * ObjectFS Monitoring Components
 *
 * Health checking and metrics collection for ObjectFS instances.
 */

import axios, { AxiosInstance, AxiosResponse } from 'axios';
import { NetworkError, TimeoutError } from './errors';
import { HealthStatus, Metrics, CacheMetrics, IOMetrics, NetworkMetrics, StorageMetrics, DistributedMetrics, OperationMetrics } from './types';

export class HealthChecker {
  private client: AxiosInstance;

  constructor(timeout = 10000, private retries = 3) {
    this.client = axios.create({
      timeout,
      validateStatus: (status) => status < 500, // Don't throw on 4xx errors
    });
  }

  /**
   * Get health status from ObjectFS instance
   */
  async getHealth(endpoint: string): Promise<HealthStatus> {
    const healthUrl = `${endpoint.replace(/\/$/, '')}/health`;

    for (let attempt = 0; attempt < this.retries; attempt++) {
      try {
        const response = await this.client.get(healthUrl);

        if (response.status === 200) {
          return this.parseHealthResponse(response.data);
        } else {
          console.warn(
            `Health check failed with status ${response.status} ` +
            `(attempt ${attempt + 1}/${this.retries})`
          );
        }
      } catch (error) {
        if (axios.isAxiosError(error)) {
          if (error.code === 'ECONNABORTED') {
            console.warn(`Health check timeout (attempt ${attempt + 1}/${this.retries})`);
          } else {
            console.warn(`Health check client error: ${error.message} (attempt ${attempt + 1}/${this.retries})`);
          }
        } else {
          console.error(`Unexpected health check error: ${error}`);
          break;
        }

        if (attempt < this.retries - 1) {
          await new Promise(resolve => setTimeout(resolve, Math.pow(2, attempt) * 1000));
        }
      }
    }

    // If all retries failed, return unhealthy status
    return {
      status: 'unhealthy',
      timestamp: Date.now(),
      checks: {},
      healthy: false,
    };
  }

  /**
   * Check if ObjectFS instance is ready to serve requests
   */
  async checkReadiness(endpoint: string): Promise<boolean> {
    try {
      const health = await this.getHealth(endpoint);
      return health.status === 'healthy';
    } catch (error) {
      console.error(`Readiness check failed: ${error}`);
      return false;
    }
  }

  /**
   * Wait for ObjectFS instance to become ready
   */
  async waitForReady(endpoint: string, timeout = 60000): Promise<boolean> {
    const startTime = Date.now();

    while (Date.now() - startTime < timeout) {
      if (await this.checkReadiness(endpoint)) {
        return true;
      }

      await new Promise(resolve => setTimeout(resolve, 1000));
    }

    return false;
  }

  private parseHealthResponse(data: any): HealthStatus {
    const parsed: HealthStatus = {
      status: data.status || 'unknown',
      timestamp: Date.now(),
      version: data.version || 'unknown',
      uptime: data.uptime || 0,
      checks: data.checks || {},
      healthy: false,
    };

    // Add derived fields
    parsed.healthy = parsed.status === 'healthy';

    return parsed;
  }
}

export class MetricsCollector {
  private client: AxiosInstance;
  private cache = new Map<string, { data: Metrics; timestamp: number }>();
  private cacheTTL = 30000; // Cache TTL in milliseconds

  constructor(timeout = 10000) {
    this.client = axios.create({
      timeout,
      validateStatus: (status) => status < 500,
    });
  }

  /**
   * Collect metrics from ObjectFS instance
   */
  async collectMetrics(endpoint: string): Promise<Metrics> {
    // Check cache first
    const cacheKey = `metrics:${endpoint}`;
    if (this.isCached(cacheKey)) {
      return this.cache.get(cacheKey)!.data;
    }

    const metricsUrl = `${endpoint.replace(/\/$/, '')}/metrics`;

    try {
      const response = await this.client.get(metricsUrl);

      if (response.status === 200) {
        let data: any;

        if (response.headers['content-type']?.includes('application/json')) {
          data = response.data;
        } else {
          // Assume Prometheus format
          data = this.parsePrometheusMetrics(response.data);
        }

        const processedData = this.processMetrics(data);
        this.cacheMetrics(cacheKey, processedData);
        return processedData;
      } else {
        throw new NetworkError(`Metrics request failed with status ${response.status}`);
      }
    } catch (error) {
      if (axios.isAxiosError(error)) {
        if (error.code === 'ECONNABORTED') {
          throw new TimeoutError('Metrics collection timeout');
        } else {
          throw new NetworkError(`Metrics collection failed: ${error.message}`);
        }
      } else {
        console.error(`Unexpected metrics collection error: ${error}`);
        throw new NetworkError(`Metrics collection failed: ${error}`);
      }
    }
  }

  /**
   * Collect performance-specific statistics
   */
  async collectPerformanceStats(endpoint: string): Promise<{
    cache: CacheMetrics;
    io: IOMetrics;
    network: NetworkMetrics;
    storage: StorageMetrics;
    distributed: DistributedMetrics;
  }> {
    const metrics = await this.collectMetrics(endpoint);

    return {
      cache: this.extractCacheStats(metrics.raw),
      io: this.extractIOStats(metrics.raw),
      network: this.extractNetworkStats(metrics.raw),
      storage: this.extractStorageStats(metrics.raw),
      distributed: this.extractDistributedStats(metrics.raw),
    };
  }

  /**
   * Collect metrics from multiple cluster nodes
   */
  async getClusterMetrics(endpoints: string[]): Promise<{
    nodes: Record<string, Metrics | { error: string }>;
    aggregate: {
      totalNodes: number;
      healthyNodes: number;
      totalOperations: number;
      totalCacheHits: number;
      totalCacheMisses: number;
    };
  }> {
    const promises = endpoints.map(async (endpoint) => {
      try {
        return await this.collectMetrics(endpoint);
      } catch (error) {
        return { error: String(error) };
      }
    });

    const results = await Promise.all(promises);

    const clusterMetrics = {
      nodes: {} as Record<string, Metrics | { error: string }>,
      aggregate: {
        totalNodes: endpoints.length,
        healthyNodes: 0,
        totalOperations: 0,
        totalCacheHits: 0,
        totalCacheMisses: 0,
      },
    };

    endpoints.forEach((endpoint, i) => {
      const result = results[i];
      clusterMetrics.nodes[endpoint] = result;

      if (!('error' in result)) {
        clusterMetrics.aggregate.healthyNodes++;

        // Aggregate key metrics
        if (result.operations) {
          clusterMetrics.aggregate.totalOperations += result.operations.total || 0;
        }
        if (result.cache) {
          clusterMetrics.aggregate.totalCacheHits += result.cache.hits || 0;
          clusterMetrics.aggregate.totalCacheMisses += result.cache.misses || 0;
        }
      }
    });

    return clusterMetrics;
  }

  private isCached(key: string): boolean {
    const cached = this.cache.get(key);
    if (!cached) return false;

    return Date.now() - cached.timestamp < this.cacheTTL;
  }

  private cacheMetrics(key: string, data: Metrics): void {
    this.cache.set(key, {
      data,
      timestamp: Date.now(),
    });
  }

  private parsePrometheusMetrics(text: string): Record<string, number> {
    const metrics: Record<string, number> = {};

    const lines = text.split('\n');
    for (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed || trimmed.startsWith('#')) {
        continue;
      }

      try {
        // Simple parsing - in production would use prometheus-client
        const spaceIndex = trimmed.indexOf(' ');
        if (spaceIndex > 0) {
          const metricName = trimmed.substring(0, spaceIndex);
          const value = parseFloat(trimmed.substring(spaceIndex + 1));
          if (!isNaN(value)) {
            metrics[metricName] = value;
          }
        }
      } catch (error) {
        // Ignore parsing errors
      }
    }

    return metrics;
  }

  private processMetrics(data: Record<string, any>): Metrics {
    const processed: Metrics = {
      timestamp: Date.now(),
      raw: data,
    };

    // Extract organized metrics
    processed.cache = this.extractCacheStats(data);
    processed.io = this.extractIOStats(data);
    processed.network = this.extractNetworkStats(data);
    processed.operations = this.extractOperationStats(data);

    return processed;
  }

  private extractCacheStats(data: Record<string, any>): CacheMetrics {
    const cacheStats: Partial<CacheMetrics> = {};

    // Look for cache metrics in various formats
    const cacheKeys = [
      'cache_hits', 'cache_misses', 'cache_size', 'cache_entries',
      'objectfs_cache_hits_total', 'objectfs_cache_misses_total'
    ];

    for (const key of cacheKeys) {
      if (key in data) {
        const normalizedKey = key
          .replace('objectfs_cache_', '')
          .replace('_total', '') as keyof CacheMetrics;
        (cacheStats as any)[normalizedKey] = data[key];
      }
    }

    // Calculate derived metrics
    if (cacheStats.hits && cacheStats.misses) {
      const totalRequests = cacheStats.hits + cacheStats.misses;
      if (totalRequests > 0) {
        cacheStats.hitRate = cacheStats.hits / totalRequests;
      }
    }

    return cacheStats as CacheMetrics;
  }

  private extractIOStats(data: Record<string, any>): IOMetrics {
    const ioStats: Partial<IOMetrics> = {};

    const ioKeys = [
      'read_operations', 'write_operations', 'read_bytes', 'write_bytes',
      'objectfs_io_read_operations_total', 'objectfs_io_write_operations_total'
    ];

    for (const key of ioKeys) {
      if (key in data) {
        const normalizedKey = key
          .replace('objectfs_io_', '')
          .replace('_total', '') as keyof IOMetrics;
        (ioStats as any)[normalizedKey] = data[key];
      }
    }

    return ioStats as IOMetrics;
  }

  private extractNetworkStats(data: Record<string, any>): NetworkMetrics {
    const networkStats: Partial<NetworkMetrics> = {};

    const networkKeys = [
      'network_requests', 'network_errors', 'network_latency',
      'objectfs_network_requests_total', 'objectfs_network_errors_total'
    ];

    for (const key of networkKeys) {
      if (key in data) {
        const normalizedKey = key
          .replace('objectfs_network_', '')
          .replace('_total', '') as keyof NetworkMetrics;
        (networkStats as any)[normalizedKey] = data[key];
      }
    }

    return networkStats as NetworkMetrics;
  }

  private extractStorageStats(data: Record<string, any>): StorageMetrics {
    const storageStats: Partial<StorageMetrics> = {};

    const storageKeys = [
      'storage_operations', 'storage_errors', 'storage_latency',
      'objectfs_storage_operations_total'
    ];

    for (const key of storageKeys) {
      if (key in data) {
        const normalizedKey = key
          .replace('objectfs_storage_', '')
          .replace('_total', '') as keyof StorageMetrics;
        (storageStats as any)[normalizedKey] = data[key];
      }
    }

    return storageStats as StorageMetrics;
  }

  private extractDistributedStats(data: Record<string, any>): DistributedMetrics {
    const distributedStats: Partial<DistributedMetrics> = {};

    const distKeys = [
      'cluster_nodes', 'cluster_operations', 'replication_tasks',
      'objectfs_cluster_nodes', 'objectfs_distributed_operations_total'
    ];

    for (const key of distKeys) {
      if (key in data) {
        const normalizedKey = key
          .replace('objectfs_', '')
          .replace('_total', '') as keyof DistributedMetrics;
        (distributedStats as any)[normalizedKey] = data[key];
      }
    }

    return distributedStats as DistributedMetrics;
  }

  private extractOperationStats(data: Record<string, any>): OperationMetrics {
    const operationStats: Partial<OperationMetrics> = {};

    const opKeys = [
      'operations_total', 'operations_successful', 'operations_failed',
      'operation_latency', 'objectfs_operations_total'
    ];

    for (const key of opKeys) {
      if (key in data) {
        const normalizedKey = key
          .replace('objectfs_', '')
          .replace('operations_', '') as keyof OperationMetrics;
        (operationStats as any)[normalizedKey] = data[key];
      }
    }

    return operationStats as OperationMetrics;
  }
}
