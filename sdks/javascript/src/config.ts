/**
 * ObjectFS Configuration Management
 *
 * Comprehensive configuration classes for ObjectFS JavaScript SDK.
 */

import * as fs from 'fs';
import * as path from 'path';
import * as yaml from 'yaml';
import {
  GlobalConfig,
  StorageConfig,
  PerformanceConfig,
  ClusterConfig,
  SecurityConfig,
  MonitoringConfig,
  FUSEConfig,
  S3Config,
  ConfigurationPreset,
} from './types';
import { ConfigurationError } from './errors';

export class Configuration {
  public global: GlobalConfig;
  public storage: StorageConfig;
  public performance: PerformanceConfig;
  public cluster: ClusterConfig;
  public security: SecurityConfig;
  public monitoring: MonitoringConfig;
  public fuse: FUSEConfig;

  constructor(config?: Partial<Configuration>) {
    this.global = {
      logLevel: 'INFO',
      logFile: '',
      pidFile: '',
      daemon: false,
      ...config?.global,
    };

    this.storage = {
      s3: {
        region: 'us-east-1',
        endpoint: '',
        profile: '',
        useAcceleration: false,
        forcePathStyle: false,
        maxRetries: 3,
        timeout: 30,
        costOptimization: {
          enabled: true,
          tieringEnabled: true,
          lifecycleEnabled: true,
          transitionToIA: 30,
          transitionToGlacier: 90,
        },
        ...config?.storage?.s3,
      },
      ...config?.storage,
    };

    this.performance = {
      cacheSize: '4GB',
      maxConcurrency: 200,
      multilevelCaching: true,
      predictiveCaching: false,
      mlModelPath: '',
      readAheadSize: '4MB',
      writeBufferSize: '4MB',
      maxWriteBuffer: '64MB',
      ...config?.performance,
    };

    this.cluster = {
      enabled: false,
      nodeId: '',
      listenAddr: '0.0.0.0:8080',
      advertiseAddr: '127.0.0.1:8080',
      seedNodes: [],
      replicationFactor: 3,
      consistencyLevel: 'eventual',
      electionTimeout: '5s',
      heartbeatInterval: '1s',
      joinTimeout: '30s',
      ...config?.cluster,
    };

    this.security = {
      enabled: false,
      authMethod: 'none',
      tlsEnabled: false,
      tlsCertPath: '',
      tlsKeyPath: '',
      tlsCaPath: '',
      ...config?.security,
    };

    this.monitoring = {
      enabled: false,
      metricsAddr: ':9090',
      healthCheckAddr: ':8081',
      enablePprof: false,
      opentelemetry: {
        enabled: false,
        endpoint: 'localhost:4317',
        serviceName: 'objectfs',
        headers: {},
      },
      ...config?.monitoring,
    };

    this.fuse = {
      allowOther: false,
      allowRoot: false,
      defaultPermissions: false,
      uid: -1,
      gid: -1,
      umask: 0o022,
      ...config?.fuse,
    };
  }

  /**
   * Load configuration from YAML file
   */
  static fromFile(filePath: string): Configuration {
    if (!fs.existsSync(filePath)) {
      throw new ConfigurationError(`Configuration file not found: ${filePath}`);
    }

    try {
      const content = fs.readFileSync(filePath, 'utf8');
      const data = yaml.parse(content);
      return Configuration.fromObject(data || {});
    } catch (error) {
      if (error instanceof yaml.YAMLError) {
        throw new ConfigurationError(`Invalid YAML in ${filePath}: ${error.message}`);
      }
      throw new ConfigurationError(`Error loading config from ${filePath}: ${error}`);
    }
  }

  /**
   * Create configuration from plain object
   */
  static fromObject(data: any): Configuration {
    return new Configuration({
      global: data.global,
      storage: data.storage,
      performance: data.performance,
      cluster: data.cluster,
      security: data.security,
      monitoring: data.monitoring,
      fuse: data.fuse,
    });
  }

  /**
   * Create configuration from preset
   */
  static fromPreset(preset: ConfigurationPreset): Configuration {
    const presets: Record<ConfigurationPreset, Partial<Configuration>> = {
      development: {
        global: { logLevel: 'DEBUG' },
        performance: { cacheSize: '1GB', maxConcurrency: 50 },
        storage: { s3: { region: 'us-east-1' } },
      },

      production: {
        global: { logLevel: 'INFO' },
        performance: {
          cacheSize: '8GB',
          maxConcurrency: 500,
          multilevelCaching: true,
        },
        storage: {
          s3: {
            useAcceleration: true,
            costOptimization: { enabled: true },
          },
        },
        monitoring: { enabled: true },
      },

      'high-performance': {
        global: { logLevel: 'WARN' },
        performance: {
          cacheSize: '16GB',
          maxConcurrency: 1000,
          predictiveCaching: true,
          multilevelCaching: true,
        },
        storage: { s3: { useAcceleration: true } },
      },

      'cost-optimized': {
        performance: { cacheSize: '2GB', maxConcurrency: 100 },
        storage: {
          s3: {
            costOptimization: {
              enabled: true,
              tieringEnabled: true,
              lifecycleEnabled: true,
              transitionToIA: 7,
              transitionToGlacier: 30,
            },
          },
        },
      },

      cluster: {
        global: { logLevel: 'INFO' },
        performance: { cacheSize: '4GB', maxConcurrency: 200 },
        cluster: {
          enabled: true,
          replicationFactor: 3,
          consistencyLevel: 'strong',
        },
        monitoring: { enabled: true },
        security: { enabled: true, tlsEnabled: true },
      },
    };

    const presetConfig = presets[preset];
    if (!presetConfig) {
      throw new ConfigurationError(`Unknown preset: ${preset}`);
    }

    return new Configuration(presetConfig);
  }

  /**
   * Load configuration from environment variables
   */
  static fromEnv(prefix = 'OBJECTFS_'): Configuration {
    const config = new Configuration();

    const envMappings: Record<string, [any, string]> = {
      [`${prefix}LOG_LEVEL`]: [config.global, 'logLevel'],
      [`${prefix}CACHE_SIZE`]: [config.performance, 'cacheSize'],
      [`${prefix}MAX_CONCURRENCY`]: [config.performance, 'maxConcurrency'],
      [`${prefix}S3_REGION`]: [config.storage.s3, 'region'],
      [`${prefix}S3_ENDPOINT`]: [config.storage.s3, 'endpoint'],
      [`${prefix}CLUSTER_ENABLED`]: [config.cluster, 'enabled'],
      [`${prefix}CLUSTER_LISTEN_ADDR`]: [config.cluster, 'listenAddr'],
    };

    for (const [envVar, [obj, key]] of Object.entries(envMappings)) {
      const value = process.env[envVar];
      if (value !== undefined) {
        // Convert string values to appropriate types
        if (key === 'enabled') {
          obj[key] = value.toLowerCase() === 'true';
        } else if (key === 'maxConcurrency') {
          obj[key] = parseInt(value, 10);
        } else {
          obj[key] = value;
        }
      }
    }

    return config;
  }

  /**
   * Merge configuration with overrides
   */
  merge(overrides: Partial<Configuration>): Configuration {
    const merged = this.toObject();

    function deepMerge(target: any, source: any): any {
      for (const key in source) {
        if (source[key] && typeof source[key] === 'object' && !Array.isArray(source[key])) {
          if (!target[key]) target[key] = {};
          target[key] = deepMerge(target[key], source[key]);
        } else {
          target[key] = source[key];
        }
      }
      return target;
    }

    return Configuration.fromObject(deepMerge(merged, overrides));
  }

  /**
   * Convert configuration to plain object
   */
  toObject(): any {
    return {
      global: this.global,
      storage: {
        s3: this.storage.s3,
      },
      performance: this.performance,
      cluster: this.cluster,
      security: this.security,
      monitoring: this.monitoring,
      fuse: this.fuse,
    };
  }

  /**
   * Convert configuration to YAML string
   */
  toYAML(): string {
    return yaml.stringify(this.toObject());
  }

  /**
   * Save configuration to YAML file
   */
  saveToFile(filePath: string): void {
    const dir = path.dirname(filePath);
    if (!fs.existsSync(dir)) {
      fs.mkdirSync(dir, { recursive: true });
    }

    fs.writeFileSync(filePath, this.toYAML(), 'utf8');
  }

  /**
   * Validate configuration
   */
  validate(): void {
    // Validate storage configuration
    if (!this.storage.s3.region) {
      throw new ConfigurationError('S3 region is required');
    }

    // Validate performance configuration
    if (this.performance.maxConcurrency <= 0) {
      throw new ConfigurationError('maxConcurrency must be positive');
    }

    // Validate cluster configuration
    if (this.cluster.enabled && !this.cluster.listenAddr) {
      throw new ConfigurationError('listenAddr required when cluster is enabled');
    }

    // Validate security configuration
    if (this.security.tlsEnabled) {
      if (!this.security.tlsCertPath || !this.security.tlsKeyPath) {
        throw new ConfigurationError('TLS certificate and key paths required');
      }
    }
  }
}

// Re-export configuration-related types for convenience
export { StorageConfig, PerformanceConfig, ClusterConfig, SecurityConfig, MonitoringConfig, FUSEConfig };
