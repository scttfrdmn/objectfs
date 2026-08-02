/**
 * TypeScript type definitions for ObjectFS SDK
 */

// Configuration types
export interface GlobalConfig {
  logLevel: 'DEBUG' | 'INFO' | 'WARN' | 'ERROR';
  logFile?: string;
  pidFile?: string;
  daemon: boolean;
}

export interface S3Config {
  region: string;
  endpoint?: string;
  profile?: string;
  useAcceleration: boolean;
  forcePathStyle: boolean;
  maxRetries: number;
  timeout: number;
  costOptimization: {
    enabled: boolean;
    tieringEnabled: boolean;
    lifecycleEnabled: boolean;
    transitionToIA: number;
    transitionToGlacier: number;
  };
}

export interface StorageConfig {
  s3: S3Config;
}

export interface PerformanceConfig {
  cacheSize: string;
  maxConcurrency: number;
  multilevelCaching: boolean;
  predictiveCaching: boolean;
  mlModelPath?: string;
  readAheadSize: string;
  writeBufferSize: string;
  maxWriteBuffer: string;
}

export interface ClusterConfig {
  enabled: boolean;
  nodeId?: string;
  listenAddr: string;
  advertiseAddr: string;
  seedNodes: string[];
  replicationFactor: number;
  consistencyLevel: 'eventual' | 'strong' | 'session';
  electionTimeout: string;
  heartbeatInterval: string;
  joinTimeout: string;
}

export interface SecurityConfig {
  enabled: boolean;
  authMethod: 'none' | 'basic' | 'oauth2' | 'oidc';
  tlsEnabled: boolean;
  tlsCertPath?: string;
  tlsKeyPath?: string;
  tlsCaPath?: string;
}

export interface MonitoringConfig {
  enabled: boolean;
  metricsAddr: string;
  healthCheckAddr: string;
  enablePprof: boolean;
  opentelemetry: {
    enabled: boolean;
    endpoint: string;
    serviceName: string;
    headers: Record<string, string>;
  };
}

export interface FUSEConfig {
  allowOther: boolean;
  allowRoot: boolean;
  defaultPermissions: boolean;
  uid: number;
  gid: number;
  umask: number;
}

// Operation types
export interface MountOptions {
  configOverrides?: Record<string, any>;
  foreground?: boolean;
  timeout?: number;
}

export interface UnmountOptions {
  force?: boolean;
  timeout?: number;
}

export interface MountInfo {
  device: string;
  mountpoint: string;
  fstype: string;
  opts: string;
  total?: number;
  used?: number;
  free?: number;
  percent?: number;
}

// Storage types
export interface ObjectInfo {
  key: string;
  size: number;
  lastModified: string;
  etag: string;
  contentType?: string;
  storageClass?: string;
  metadata?: Record<string, string>;
}

export interface ListObjectsOptions {
  prefix?: string;
  maxKeys?: number;
  continuationToken?: string;
}

export interface ListObjectsResult {
  objects: ObjectInfo[];
  truncated: boolean;
  nextContinuationToken?: string;
  totalCount: number;
}

export interface UploadOptions {
  metadata?: Record<string, string>;
  contentType?: string;
  progressCallback?: (uploaded: number, total: number) => void;
}

export interface DownloadOptions {
  progressCallback?: (downloaded: number, total: number) => void;
}

// Health and monitoring types
export interface HealthStatus {
  status: 'healthy' | 'unhealthy' | 'degraded';
  timestamp: number;
  version?: string;
  uptime?: number;
  checks: Record<string, HealthCheck>;
  healthy: boolean;
}

export interface HealthCheck {
  status: 'pass' | 'fail' | 'warn';
  message?: string;
  duration?: number;
}

/**
 * One sample from a Prometheus text exposition.
 *
 * Labels are part of a sample's identity, not decoration: every series ObjectFS exports
 * carries at least the operator's `custom_labels`, and `objectfs_cache_requests_total`
 * distinguishes a hit from a miss by nothing but its `type` label.
 */
export interface PrometheusSample {
  name: string;
  labels: Record<string, string>;
  value: number;
}

/** The parsed scrape. A list, because a metric name identifies a family, not a series. */
export interface RawMetrics {
  samples: PrometheusSample[];
}

/**
 * Metrics as returned by `getMetrics()`.
 *
 * Sections correspond to the metric families internal/metrics.Collector exports. There were
 * `network`, `storage` and `distributed` sections here, built from `objectfs_network_*`,
 * `objectfs_storage_*` and `objectfs_cluster_*` names that ObjectFS has never exported --
 * so each was permanently absent while its type advertised required fields.
 */
export interface Metrics {
  timestamp: number;
  cache?: CacheMetrics;
  io?: IOMetrics;
  operations?: OperationMetrics;
  errors?: ErrorMetrics;
  connections?: ConnectionMetrics;
  raw: RawMetrics;
}

/**
 * From `objectfs_cache_requests_total{type}` and `objectfs_cache_size_bytes{level}`.
 *
 * Every field is optional because an idle mount has served no cache request, and that is a
 * different fact from a hit rate of zero. A required `hits: number` forced the old code to
 * report 0 for "not measured", which reads as a cache that never hits.
 */
export interface CacheMetrics {
  hits?: number;
  misses?: number;
  hitRate?: number;
  size?: number;
  levels?: Record<string, number>;
}

/**
 * Read and write volume, projected from the operation metrics.
 *
 * There is no `objectfs_io_*` family; these come from `objectfs_operations_total` and
 * `objectfs_operation_size_bytes_sum`, both labelled by operation. As of v0.10.1 the FUSE
 * layer does not record `read`/`write` through RecordOperation, so this is `{}` against a
 * live mount -- the honest answer, and one that fills in on its own when that lands.
 */
export interface IOMetrics {
  readOperations?: number;
  writeOperations?: number;
  readBytes?: number;
  writeBytes?: number;
}

/** Per-operation counts and latency, from `objectfs_operations_total{operation,status}`. */
export interface OperationMetrics {
  total: number;
  successful: number;
  failed: number;
  byOperation: Record<string, OperationDetail>;
}

export interface OperationDetail {
  count: number;
  durationSeconds?: number;
  avgDurationSeconds?: number;
  bytes?: number;
}

/** From `objectfs_errors_total{operation,type}`, kept split by the collector's classification. */
export interface ErrorMetrics {
  total: number;
  byType: Record<string, number>;
  byOperation: Record<string, number>;
}

/** From `objectfs_active_connections`, an unlabelled gauge. */
export interface ConnectionMetrics {
  active: number;
}

export interface PerformanceStats {
  cache: CacheMetrics;
  io: IOMetrics;
  operations: OperationMetrics | Record<string, never>;
  errors: ErrorMetrics | Record<string, never>;
  connections: ConnectionMetrics | Record<string, never>;
}

// Cluster types
export interface ClusterStatus {
  nodeCount: number;
  leader: string;
  status: 'healthy' | 'unhealthy' | 'degraded';
  nodes: ClusterNode[];
}

export interface ClusterNode {
  id: string;
  address: string;
  status: 'alive' | 'suspect' | 'dead' | 'left';
  lastSeen: string;
  isLeader: boolean;
}

export interface JoinClusterOptions {
  nodeConfig?: Record<string, any>;
  timeout?: number;
}

// Event types
export type EventType =
  | 'mount'
  | 'unmount'
  | 'health_change'
  | 'metrics_updated'
  | 'cluster_change'
  | 'error';

export interface EventData {
  type: EventType;
  timestamp: number;
  data: any;
}

// Error types
export interface ErrorInfo {
  message: string;
  code?: string;
  details?: Record<string, any>;
}

// Client options
export interface ClientOptions {
  config?: Configuration | string | Record<string, any>;
  binaryPath?: string;
  apiEndpoint?: string;
  timeout?: number;
  retries?: number;
}

// Configuration preset types
export type ConfigurationPreset =
  | 'development'
  | 'production'
  | 'high-performance'
  | 'cost-optimized'
  | 'cluster';

// HTTP client types
export interface RequestOptions {
  timeout?: number;
  headers?: Record<string, string>;
  params?: Record<string, any>;
}

export interface Response<T = any> {
  data: T;
  status: number;
  statusText: string;
  headers: Record<string, string>;
}

// Cache management types
export interface CacheOptions {
  cacheType?: string;
  keys?: string[];
}

export interface WarmCacheOptions {
  recursive?: boolean;
}

export interface CacheClearResult {
  success: boolean;
  message?: string;
}

export interface WarmCacheResult {
  [path: string]: boolean;
}
