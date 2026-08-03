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

	performance:
	  cache_size: "2GB"
	  write_buffer_size: "16MB"
	  max_concurrency: 150
	  connection_pool_size: 8
	  read_ahead:
	    enabled: true
	    window_size: "64KB"
	    min_sequential: 3
	    concurrent_reads: 4
	    ttl: 5m

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

	monitoring:
	  metrics:
	    enabled: true
	    addr: 127.0.0.1:8080
	  health_checks:
	    enabled: true
	    addr: 127.0.0.1:8081

Environment variable mapping:

	# Global settings
	OBJECTFS_LOG_LEVEL="DEBUG"
	OBJECTFS_LOG_FILE="/var/log/objectfs.log"

	# Listeners. OBJECTFS_METRICS_PORT and OBJECTFS_HEALTH_PORT no longer exist; a port could not
	# name an interface, so every value of them bound all of them (#211). The two _ENABLED variables
	# take a boolean and refuse anything else, because they govern unauthenticated endpoints that
	# default to on: a typo coerced to false silently removes an endpoint a probe depends on.
	OBJECTFS_METRICS_ENABLED="false"
	OBJECTFS_METRICS_ADDR="127.0.0.1:9090"
	OBJECTFS_HEALTH_ENABLED="true"
	OBJECTFS_HEALTH_ADDR="127.0.0.1:9091"

	# Performance settings
	OBJECTFS_CACHE_SIZE="4GB"
	OBJECTFS_MAX_CONCURRENCY="200"
	OBJECTFS_COMPRESSION_ENABLED="true"

	# Read-ahead. The two counts report a parse failure rather than keeping the default: a worker
	# count that silently reverts to 4 when 1 was meant is prefetch traffic nobody asked for.
	# OBJECTFS_READ_AHEAD_SIZE, OBJECTFS_READAHEAD_STRATEGY, OBJECTFS_READAHEAD_PATTERN_DETECTION and
	# OBJECTFS_READAHEAD_ML_PREDICTION are gone with the settings they assigned to (#176).
	OBJECTFS_READAHEAD_ENABLED="true"
	OBJECTFS_READAHEAD_WINDOW_SIZE="256KB"
	OBJECTFS_READAHEAD_MIN_SEQUENTIAL="6"
	OBJECTFS_READAHEAD_CONCURRENT_READS="4"

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

Be aware of the related trap this created: SIGHUP does not reload configuration. It used to be worse
than that — the binary registered SIGHUP alongside SIGINT and SIGTERM and treated any of the three as
a shutdown request, so sending SIGHUP to ask for a reload unmounted the filesystem. It is no longer
registered at all, so Go's default disposition applies and the process dies without the unmount path
running. Either way, there is nothing to reload; see the README.

# Default Configuration

[NewDefault] is the one set of defaults. There is no environment switch: this section used to list
"Production Defaults" and "Development Defaults" as though a mode selected between them, and no such
mechanism has ever existed — the "development" figures (LogLevel DEBUG, a 512MB cache,
MaxConcurrency 50, Prefetching off) were never returned by anything. Use --log-level, --cache-size,
and --debug, or a config file, to get that shape.

Read the current values from [NewDefault] rather than trusting a list here; a table in a comment has
no way to be told it is stale. As of writing:

	Global.LogLevel               "INFO"
	Monitoring.Metrics.Addr       "127.0.0.1:8080"
	Monitoring.HealthChecks.Addr  "127.0.0.1:8081"
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
  - Monitoring.Metrics.Addr and Monitoring.HealthChecks.Addr are loopback, and each sits beside the
    `enabled` flag that governs its listener. They replaced Global.MetricsPort/HealthPort and the
    never-read Monitoring.MetricsAddr/HealthCheckAddr in v0.11.0: the ports were what the listeners
    bound, the addresses were read by nothing, and a port cannot name an interface — so every value of
    metrics_port bound all of them (#211). Both endpoints are unauthenticated, which is why the default
    is the narrowest thing that still lets a same-host scrape work. To turn one off, set its `enabled`
    to false; there is no `0`-means-off spelling to get wrong (#212).
  - No pprof listener is started at any address. Global.EnablePprof and Global.ProfilePort are removed
    rather than wired; see [GlobalConfig] and #245.

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

Starting points, not measurements. Each is a plausible shape for a workload; the numbers have not been
benchmarked against the alternatives, and cache_size in particular is bounded by the host rather than
by the link.

Low Latency (same region, high bandwidth):

	performance:
	  cache_size: "1GB"
	  max_concurrency: 100
	  connection_pool_size: 4
	  read_ahead:
	    window_size: "64KB"     # reads are cheap; a wrong prefetch is not worth much
	    concurrent_reads: 4

High Throughput (large sequential reads):

	performance:
	  cache_size: "8GB"
	  max_concurrency: 300
	  connection_pool_size: 16
	  read_ahead:
	    window_size: "1MB"      # read far ahead of a sequential reader
	    min_sequential: 6       # 6 is the effective floor whatever is set; see ReadAheadConfig
	    concurrent_reads: 8

High Latency / Satellite (a round trip is the dominant cost):

	performance:
	  cache_size: "16GB"
	  max_concurrency: 25
	  connection_pool_size: 2
	  read_ahead:
	    window_size: "4MB"      # amortize the round trip over as many bytes as possible
	    concurrent_reads: 2     # but few in flight: the link is the scarce resource
	    ttl: 30m
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
