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
// Every error class ./errors declares, not seven of eleven. CacheError is the one that had to be
// added: `clearCache` and `warmCache` throw it, so a caller needed to name it in a catch and could
// not import it from the package. AuthenticationError, AuthorizationError and ValidationError are
// unthrown today, but a consumer of a package cannot see a class the entry point does not export,
// so leaving them out means they may as well not exist.
export {
  ObjectFSError,
  ConfigurationError,
  MountError,
  StorageError,
  DistributedError,
  CacheError,
  NetworkError,
  AuthenticationError,
  AuthorizationError,
  TimeoutError,
  ValidationError,
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

// Version info.
//
// LICENSE said 'MIT'. ObjectFS is Apache-2.0 — package.json's own `license` field says so, as do
// the repository LICENSE file and every source header. A consumer reading this constant to build an
// attribution or compliance list got the wrong answer from the SDK's own API, which is the kind of
// wrong that reaches a legal review rather than a bug tracker.
export const VERSION = '0.1.0';
export const AUTHOR = 'ObjectFS Team';
export const LICENSE = 'Apache-2.0';
