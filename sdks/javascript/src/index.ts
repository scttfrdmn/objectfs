/**
 * ObjectFS JavaScript/TypeScript SDK
 *
 * High-performance POSIX filesystem for object storage with comprehensive
 * API support for mounting, configuration, and management.
 */

export { ObjectFSClient } from './client';
export { Configuration, StorageConfig, PerformanceConfig, ClusterConfig } from './config';
export {
  ObjectFSError,
  ConfigurationError,
  MountError,
  StorageError,
  DistributedError,
  NetworkError,
  TimeoutError
} from './errors';
export { MountManager } from './mount';
export { MetricsCollector, HealthChecker } from './monitoring';
export { StorageAdapter } from './storage';
export * from './types';

// Version info
export const VERSION = '0.1.0';
export const AUTHOR = 'ObjectFS Team';
export const LICENSE = 'MIT';
