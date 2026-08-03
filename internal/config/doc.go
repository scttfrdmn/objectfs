/*
Package config provides comprehensive configuration management for ObjectFS with multi-source support.

This package implements a hierarchical configuration system that supports YAML files, environment
variables, and command-line overrides, with validation at load time. Configuration is read once at
startup and is not reloadable; see the Reloading section below.

Decoding is strict: a key the schema does not define fails the load with a message naming it. This
is deliberate and is a change in kind rather than degree. Under the previous non-strict decoding,
eight of the ten top-level keys in the example config users were told to copy did nothing at all,
and nobody noticed for several releases because a setting that is silently ignored looks exactly
like a setting that is applied. Failing at startup is louder and cheaper than a filesystem that
runs with settings the operator believes are in effect.

# Configuration Architecture

Multi-source configuration hierarchy with precedence:

	┌─────────────────────────────────────────────┐
	│          Runtime Overrides                 │ ← Highest Priority
	│        (CLI args, API calls)               │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│        Environment Variables                │           (OBJECTFS_*)                     │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│         Configuration Files                 │            (YAML format)                    │
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
	  connection_pool_size: 8

	storage:
	  s3:
	    compression:
	      enabled: false
	      algorithm: "zstd"
	      level: 3
	      min_size: "4KB"

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

# Reloading

There is none. Configuration is read once at startup; changing the file requires a remount.

This section previously described a StartWatcher/Updates() API for hot reload, with lists of which
settings were and were not reloadable. None of it exists — not the function, not the channel, not
the distinction — and the example was not valid Go (it opened with a bare `:=`), which is a good
sign that nothing had ever compiled it.

Be aware of the related trap this created: SIGHUP does not reload configuration. The binary
registers it and treats any signal as a shutdown request, so sending SIGHUP to ask for a reload
unmounts the filesystem instead. See the README.

# Default Configuration

[NewDefault] is the one set of defaults. There is no environment switch: this section used to list
"Production Defaults" and "Development Defaults" as though a mode selected between them, and no such
mechanism has ever existed — the "development" figures (LogLevel DEBUG, a 512MB cache,
MaxConcurrency 50, Prefetching off) were never returned by anything. Use --log-level, --cache-size,
and --debug, or a config file, to get that shape.

Read the current values from [NewDefault] rather than trusting a list here; a table in a comment has
no way to be told it is stale. As of writing:

	Global.LogLevel               "INFO"
	Global.MetricsPort            8080
	Global.HealthPort             8081
	Performance.CacheSize         "2GB"
	Performance.MaxConcurrency    150
	Performance.ConnectionPoolSize 8      // also the batch concurrency and MaxIdleConnsPerHost
	Cache.TTL                     5m
	Cache.EvictionPolicy          "weighted_lru"
	Features.Prefetching          true

Two settings are worth knowing because their names have misled before:

  - Storage.S3.Compression is the only compression switch, and it defaults to off. There were two
    others until v0.11.0 — Performance.CompressionEnabled, which defaulted to *true* and was read by
    nothing, and WriteBuffer.Compression, which is where this block used to live despite nothing ever
    compressing a write buffer. Both are removed rather than deprecated, so a config file that still
    sets either fails to load with the key named. Compression is off by default because it is a
    storage-format decision rather than a performance knob: a compressed object is not readable by
    `aws s3 cp`. Algorithm is safe to change on an existing bucket — the read path decodes every
    algorithm ObjectFS can write, whatever this is set to (#230).
  - Global.ProfilePort is 6060 but is read by nothing — no pprof listener is started at any port.

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
