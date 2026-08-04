/**
 * ObjectFS Monitoring Components
 *
 * Health checking and metrics collection for ObjectFS instances.
 */

import axios, { AxiosInstance } from 'axios';
import { NetworkError, TimeoutError } from './errors';
import { HealthStatus, Metrics, RawMetrics, PerformanceStats } from './types';
import {
  parseScrape,
  processMetrics,
  extractCacheStats,
  extractIOStats,
  extractOperationStats,
  extractErrorStats,
  extractConnectionStats,
} from './prometheus';

export class HealthChecker {
  private client: AxiosInstance;

  constructor(
    timeout = 10000,
    private retries = 3
  ) {
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
            console.warn(
              `Health check timeout (attempt ${attempt + 1}/${this.retries})`
            );
          } else {
            console.warn(
              `Health check client error: ${error.message} (attempt ${
                attempt + 1
              }/${this.retries})`
            );
          }
        } else {
          console.error(`Unexpected health check error: ${error}`);
          break;
        }

        if (attempt < this.retries - 1) {
          await new Promise((resolve) =>
            setTimeout(resolve, Math.pow(2, attempt) * 1000)
          );
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

      await new Promise((resolve) => setTimeout(resolve, 1000));
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
        // ObjectFS serves /metrics through promhttp, which is always the text exposition
        // format. A JSON body means this endpoint is something else -- a reverse proxy's error
        // page, an API gateway, the wrong port -- and parsing it as metrics would report an
        // empty but successful scrape. The old code took a JSON body as metrics directly.
        // Axios types a header value as string | number | boolean | string[] | AxiosHeaders, so
        // `.includes` is not available on it without narrowing. Content-Type is a single-value
        // header in practice; String() covers the array case rather than dropping it.
        const contentType = response.headers['content-type'];
        if (
          contentType !== undefined &&
          String(contentType).includes('application/json')
        ) {
          throw new NetworkError(
            `${metricsUrl} returned JSON, not the Prometheus text format ObjectFS serves. ` +
              `Check that this is an ObjectFS metrics endpoint (monitoring.metrics.addr, default 127.0.0.1:8080).`
          );
        }

        const processedData = this.processMetrics(
          this.parsePrometheusMetrics(response.data)
        );
        this.cacheMetrics(cacheKey, processedData);
        return processedData;
      } else {
        throw new NetworkError(
          `Metrics request failed with status ${response.status}`
        );
      }
    } catch (error) {
      // Ahead of the catch-all: these were thrown deliberately just above, and re-wrapping
      // them yields "Metrics collection failed: <the message we just wrote>".
      if (error instanceof NetworkError || error instanceof TimeoutError) {
        throw error;
      }
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
   * Collect performance-specific statistics.
   *
   * There were `network`, `storage` and `distributed` sections here as well, read off
   * `objectfs_network_*`, `objectfs_storage_*` and `objectfs_cluster_*` names that no version
   * of ObjectFS has exported. They are gone rather than stubbed: a caller can tell that a key
   * is missing, and cannot tell that a present-but-empty one means "not implemented".
   */
  async collectPerformanceStats(endpoint: string): Promise<PerformanceStats> {
    const metrics = await this.collectMetrics(endpoint);

    return {
      cache: extractCacheStats(metrics.raw),
      io: extractIOStats(metrics.raw),
      operations: extractOperationStats(metrics.raw) ?? {},
      errors: extractErrorStats(metrics.raw) ?? {},
      connections: extractConnectionStats(metrics.raw) ?? {},
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

    // Zipped rather than indexed by position: `results[i]` is `Metrics | {error} | undefined` under
    // `noUncheckedIndexedAccess`, and every use of it below then needed a non-null assertion. The
    // lengths do match — `results` comes from `Promise.all` over `endpoints.map` — so pairing the
    // two arrays states that invariant instead of asserting past it.
    results.forEach((result, i) => {
      const endpoint = endpoints[i];
      if (endpoint === undefined) {
        return;
      }
      clusterMetrics.nodes[endpoint] = result;

      if (!('error' in result)) {
        clusterMetrics.aggregate.healthyNodes++;

        // Aggregate key metrics
        if (result.operations) {
          clusterMetrics.aggregate.totalOperations +=
            result.operations.total || 0;
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

  /**
   * Parse a Prometheus text exposition into labelled samples.
   *
   * Kept as a method for backward compatibility; the implementation lives in ./prometheus,
   * which has no transport dependency and is therefore covered directly by
   * prometheus.test.ts against a real captured scrape.
   */
  parsePrometheusMetrics(text: string): RawMetrics {
    return parseScrape(text);
  }

  private processMetrics(data: RawMetrics): Metrics {
    return processMetrics(data, Date.now());
  }
}
