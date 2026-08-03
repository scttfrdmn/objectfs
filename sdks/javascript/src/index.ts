/**
 * ObjectFS JavaScript/TypeScript SDK
 *
 * High-performance POSIX filesystem for object storage with comprehensive
 * API support for mounting, configuration, and management.
 */

export { ObjectFSClient } from './client';
export {
  Configuration,
  StorageConfig,
  PerformanceConfig,
  ClusterConfig,
} from './config';
export {
  ObjectFSError,
  ConfigurationError,
  MountError,
  StorageError,
  DistributedError,
  NetworkError,
  TimeoutError,
} from './errors';
export { MountManager } from './mount';
export { MetricsCollector, HealthChecker } from './monitoring';
export {
  parseScrape,
  parseSampleLine,
  parseLabels,
  processMetrics,
  extractCacheStats,
  extractIOStats,
  extractOperationStats,
  extractErrorStats,
  extractConnectionStats,
} from './prometheus';
// S3StorageAdapter, not StorageAdapter: the latter name has never existed in './storage', so this
// line was a hard TS2724 and `npm run build` could not have produced a dist/ — which is how it
// survived a release. There is no adapter interface to hide behind either; the class is the API.
export { S3StorageAdapter } from './storage';
export * from './types';

// Version info
export const VERSION = '0.1.0';
export const AUTHOR = 'ObjectFS Team';
export const LICENSE = 'MIT';
