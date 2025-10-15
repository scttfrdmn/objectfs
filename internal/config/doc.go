/*
Package config provides comprehensive configuration management for ObjectFS with multi-source support.

This package implements a hierarchical configuration system that supports YAML files, environment
variables, and runtime overrides. It provides validation, type safety, and hot-reloading
capabilities for all ObjectFS components.

# Configuration Architecture

Multi-source configuration hierarchy with precedence:

	┌─────────────────────────────────────────────┐
	│          Runtime Overrides                 │ ← Highest Priority
	│        (CLI args, API calls)               │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│        Environment Variables                │
	│           (OBJECTFS_*)                     │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│         Configuration Files                 │
	│            (YAML format)                    │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│           Default Values                    │ ← Lowest Priority
	│        (Compiled-in defaults)              │
	└─────────────────────────────────────────────┘

# Configuration Structure

Comprehensive configuration sections:

Global Settings:
- Logging configuration (level, file, format)
- Service ports (metrics, health, profiling)
- Runtime behavior settings

Performance Settings:
- Cache sizes and policies
- Concurrency limits
- Buffer configurations
- Compression settings

Network Configuration:
- Timeout settings
- Retry policies
- Circuit breaker parameters
- Connection pool settings

Security Configuration:
- TLS settings
- Encryption parameters
- Authentication configuration
- Access control settings

Monitoring Configuration:
- Metrics collection settings
- Health check parameters
- Logging configuration
- Alert thresholds

Feature Flags:
- Experimental feature toggles
- Performance optimization flags
- Compatibility settings
- Debug features

# Usage Examples

Loading configuration:

	// Create with defaults
	config := config.NewDefault()

	// Load from file
	if err := config.LoadFromFile("/etc/objectfs/config.yaml"); err != nil {
		log.Fatal(err)
	}

	// Load environment variables
	if err := config.LoadFromEnv(); err != nil {
		log.Fatal(err)
	}

	// Apply command-line overrides
	config.Performance.CacheSize = "4GB"
	config.Global.LogLevel = "DEBUG"

	// Validate final configuration
	if err := config.Validate(); err != nil {
		log.Fatal(err)
	}

Configuration file format:

	# ObjectFS Configuration
	global:
	  log_level: INFO
	  log_file: "/var/log/objectfs.log"
	  metrics_port: 8080
	  health_port: 8081
	  profile_port: 6060

	performance:
	  cache_size: "2GB"
	  write_buffer_size: "16MB"
	  max_concurrency: 150
	  read_ahead_size: "64MB"
	  compression_enabled: true
	  connection_pool_size: 8

	cache:
	  ttl: 5m
	  max_entries: 100000
	  eviction_policy: "weighted_lru"
	  persistent_cache:
	    enabled: false
	    directory: "/var/cache/objectfs"
	    max_size: "10GB"

Environment variable mapping:

	# Global settings
	OBJECTFS_LOG_LEVEL="DEBUG"
	OBJECTFS_LOG_FILE="/var/log/objectfs.log"
	OBJECTFS_METRICS_PORT="9090"

	# Performance settings
	OBJECTFS_CACHE_SIZE="4GB"
	OBJECTFS_MAX_CONCURRENCY="200"
	OBJECTFS_COMPRESSION_ENABLED="true"

	# Feature flags
	OBJECTFS_PREFETCHING="true"
	OBJECTFS_BATCH_OPERATIONS="true"
	OBJECTFS_OFFLINE_MODE="false"

# Validation System

Comprehensive configuration validation:

Type Validation:
- String format validation (sizes, durations, etc.)
- Numeric range validation
- Boolean value validation
- Enum value validation

Dependency Validation:
- Feature prerequisite checking
- Resource requirement validation
- Component compatibility verification
- Platform-specific validation

Business Logic Validation:
- Performance setting reasonableness
- Resource limit consistency
- Security setting compatibility
- Operational parameter validation

Example validation:

	func (c *Configuration) Validate() error {
		// Validate global settings
		if c.Global.LogLevel != "" {
			if _, err := utils.ParseLogLevel(c.Global.LogLevel); err != nil {
				return fmt.Errorf("invalid log level: %w", err)
			}
		}

		// Validate performance settings
		if c.Performance.MaxConcurrency < 1 || c.Performance.MaxConcurrency > 10000 {
			return fmt.Errorf("max_concurrency must be between 1 and 10000")
		}

		// Validate cache settings
		if c.Cache.TTL < 0 {
			return fmt.Errorf("cache TTL cannot be negative")
		}

		return nil
	}

# Hot Reloading

Dynamic configuration updates without restart:

Watch Configuration:

	config := config.NewDefault()

	// Set up file watcher
	watcher := config.StartWatcher("/etc/objectfs/config.yaml")
	defer watcher.Stop()

	// Handle updates
	go func() {
		for update := range watcher.Updates() {
			log.Printf("Configuration updated: %s", update.Section)
			// Apply hot-reloadable changes
		}
	}()

Reloadable Settings:
- Log level changes
- Cache size adjustments
- Timeout modifications
- Feature flag toggles

Non-Reloadable Settings:
- Network ports
- Storage backends
- Core component settings
- Security credentials

# Default Configuration

Sensible defaults for all environments:

Production Defaults:

	Global: {
		LogLevel:    "INFO",
		MetricsPort: 8080,
		HealthPort:  8081,
	},
	Performance: {
		CacheSize:         "2GB",
		MaxConcurrency:    150,
		CompressionEnabled: true,
		ConnectionPoolSize: 8,
	},
	Cache: {
		TTL:            5 * time.Minute,
		EvictionPolicy: "weighted_lru",
	}

Development Defaults:

	Global: {
		LogLevel:    "DEBUG",
		ProfilePort: 6060, // pprof enabled
	},
	Performance: {
		CacheSize:      "512MB",
		MaxConcurrency: 50,
	},
	Features: {
		Prefetching: false, // Simpler debugging
	}

# Security Considerations

Secure configuration handling:

Credential Management:
- Environment variable preference for secrets
- File permission validation (0600 for config files)
- Credential masking in logs
- Secure default values

Path Validation:
- Directory traversal prevention
- Absolute path enforcement where required
- Permission checking for directories
- Safe temporary file handling

Access Control:
- Configuration file access restrictions
- Runtime modification controls
- Audit logging for configuration changes
- Role-based configuration sections

# Performance Tuning Profiles

Pre-configured performance profiles:

Low Latency Profile:

	performance:
	  cache_size: "1GB"
	  max_concurrency: 100
	  read_ahead_size: "32MB"
	  connection_pool_size: 4

High Throughput Profile:

	performance:
	  cache_size: "8GB"
	  max_concurrency: 300
	  read_ahead_size: "256MB"
	  connection_pool_size: 16

High Latency/Satellite Profile:

	performance:
	  cache_size: "16GB"
	  max_concurrency: 25
	  read_ahead_size: "1GB"
	  connection_pool_size: 2
	write_buffer:
	  flush_interval: 300s
	  max_memory: "1GB"

# Configuration Best Practices

Recommended configuration practices:

File Organization:
- Use versioned configuration files
- Separate environment-specific configs
- Document all custom settings
- Use configuration validation in CI/CD

Environment Variables:
- Prefer environment variables for secrets
- Use consistent naming conventions
- Document all supported variables
- Validate environment variable formats

Performance Tuning:
- Start with default settings
- Monitor resource usage
- Adjust based on workload characteristics
- Test configuration changes in staging

Security:
- Use restrictive file permissions (0600)
- Rotate credentials regularly
- Audit configuration changes
- Use encrypted storage for sensitive configs

This package provides the foundation for flexible, secure, and maintainable
configuration management across all ObjectFS deployments.
*/
package config
