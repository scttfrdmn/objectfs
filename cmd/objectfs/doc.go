/*
Package main provides the ObjectFS command-line interface and application entry point.

This package implements the primary user-facing interface for ObjectFS: a command-line tool for
mounting object storage buckets as filesystems. It handles configuration management, command-line
argument processing, and application lifecycle management.

ObjectFS presents a POSIX *interface* over object storage; it is not a POSIX-compliant filesystem.
There is no rename, no hard or symbolic links, no locking, and no atomic anything, because S3
provides none of them and this filesystem does not emulate them. Where an operation cannot be
supported it returns an error rather than appearing to succeed. The README's supported-operations
table is the authoritative list of what works, what fails by design, and which tools are known not
to work against a mount — consult it before pointing a workload at one.

# Command-Line Interface

ObjectFS follows standard Unix command-line conventions:

	objectfs [options] <storage-uri> <mount-point>

Basic Usage:

	# Mount S3 bucket with default settings
	objectfs s3://my-bucket /mnt/s3

	# Mount with custom configuration
	objectfs --config /etc/objectfs/config.yaml s3://data-bucket /mnt/data

	# High-performance configuration
	objectfs --cache-size 4GB --max-concurrency 200 s3://bucket /mnt/bucket

	# Dry run to validate configuration
	objectfs --dry-run --config config.yaml s3://test-bucket /tmp/mount

# Configuration Management

Multi-source configuration with precedence hierarchy:

 1. Default values (lowest priority)
 2. Configuration files (YAML)
 3. Environment variables (OBJECTFS_*)
 4. Command-line arguments (highest priority)

Command-Line Options:

Core Options:

	--config string           Configuration file path
	--log-level string        Log level (DEBUG, INFO, WARN, ERROR) [default: INFO]
	--version                 Show version information
	--help                    Show help information
	--dry-run                 Validate configuration without mounting
	--debug                   Enable debug mode (sets log level to DEBUG)

Performance Options:

	--cache-size string       Cache size (e.g., 2GB, 512MB)
	--max-concurrency int     Maximum concurrent operations

Planned Features (Future Versions):

	--foreground              Run in foreground mode (daemon mode default)
	--region string           AWS region override
	--endpoint string         Custom S3 endpoint URL
	--profile string          AWS profile name
	--allow-other             Allow other users to access mount
	--read-only               Mount filesystem in read-only mode
	--uid int                 Override file owner UID
	--gid int                 Override file owner GID

# Storage URI Format

Supported storage URI formats:

AWS S3:

	s3://bucket-name                    # Default region
	s3://bucket-name/path/prefix        # With path prefix

Future Support:

	gs://bucket-name                    # Google Cloud Storage
	azure://container-name              # Azure Blob Storage
	minio://endpoint/bucket-name        # MinIO compatible

# Usage Examples

Basic mounting:

	# Simple bucket mount
	objectfs s3://production-data /mnt/data

	# Now use standard POSIX commands
	ls -la /mnt/data
	cat /mnt/data/logs/app.log
	cp /local/file.txt /mnt/data/uploads/

Configuration file usage:

	# Create configuration file
	cat > config.yaml << EOF
	performance:
	  cache_size: "8GB"
	  max_concurrency: 300
	cache:
	  ttl: 300s
	  eviction_policy: "weighted_lru"
	monitoring:
	  metrics:
	    enabled: true
	    prometheus: true
	EOF

	# Use configuration
	objectfs --config config.yaml s3://data-bucket /mnt/data

Performance tuning:

	# High-performance configuration for large workloads
	objectfs \
	  --cache-size 16GB \
	  --max-concurrency 500 \
	  --debug \
	  s3://high-traffic-bucket /mnt/fast-data

Environment variable configuration:

	# Set environment variables
	export OBJECTFS_CACHE_SIZE="4GB"
	export OBJECTFS_MAX_CONCURRENCY="200"
	export OBJECTFS_LOG_LEVEL="DEBUG"

	# Simple mount command
	objectfs s3://env-configured-bucket /mnt/data

# Application Lifecycle

Structured startup and shutdown sequence:

Startup Process:
 1. Command-line argument parsing and validation
 2. Configuration loading and merging
 3. Path validation for mount point
 4. Logging system initialization
 5. Adapter creation and component initialization
 6. Signal handler registration
 7. Filesystem mounting
 8. Ready for operations

Shutdown Process:
 1. Signal reception (SIGINT, SIGTERM, SIGHUP)
 2. Graceful filesystem unmounting
 3. Component cleanup and resource release
 4. Configuration persistence (if applicable)
 5. Clean exit

Signal Handling:

	SIGINT (Ctrl+C)   - Graceful shutdown
	SIGTERM           - Graceful shutdown
	SIGHUP            - Configuration reload (future)

# Error Handling

Comprehensive error handling with user-friendly messages:

Configuration Errors:

	Error: Invalid mount point: directory does not exist: /nonexistent/path
	Error: Failed to load configuration: yaml: unmarshal errors
	Error: Expected exactly 2 arguments (storage-uri and mount-point)

Validation Errors:

	Error: Invalid storage URI: unsupported scheme 'file://' (only s3:// supported)
	Error: Invalid mount point: /home/user/file.txt (not a directory)
	Error: Mount point is not empty: /mnt/data

Runtime Errors:

	Error: Failed to create adapter: S3 bucket 'nonexistent' not found
	Error: Failed to start adapter: insufficient permissions for /mnt/data
	Error: Failed to mount filesystem: FUSE not available

# Configuration File Format

YAML configuration with comprehensive options:

	# ObjectFS Configuration File
	global:
	  log_level: INFO
	  log_file: /var/log/objectfs.log
	  metrics_port: 8080
	  health_port: 8081

	performance:
	  cache_size: "4GB"
	  write_buffer_size: "64MB"
	  max_concurrency: 200
	  read_ahead_size: "128MB"
	  connection_pool_size: 8

	cache:
	  ttl: 5m
	  max_entries: 100000
	  eviction_policy: "weighted_lru"
	  persistent_cache:
	    enabled: true
	    directory: "/var/cache/objectfs"
	    max_size: "10GB"

	# Additional sections: write_buffer, network, security, monitoring, features

# Environment Variables

Comprehensive environment variable support:

Core Settings:

	OBJECTFS_LOG_LEVEL              Log level override
	OBJECTFS_CONFIG_FILE            Default configuration file path

Performance Settings:

	OBJECTFS_CACHE_SIZE             Cache size (e.g., "4GB")
	OBJECTFS_MAX_CONCURRENCY        Maximum concurrent operations
	OBJECTFS_WRITE_BUFFER_SIZE      Write buffer size
	OBJECTFS_READ_AHEAD_SIZE        Read-ahead buffer size
	OBJECTFS_COMPRESSION_ENABLED    Enable compression (true/false)

AWS Settings:

	AWS_ACCESS_KEY_ID               AWS access key
	AWS_SECRET_ACCESS_KEY           AWS secret key
	AWS_DEFAULT_REGION              Default AWS region
	AWS_PROFILE                     AWS CLI profile name
	AWS_SESSION_TOKEN               Session token for temporary credentials

Monitoring:

	OBJECTFS_METRICS_ENABLED        Enable metrics collection
	OBJECTFS_METRICS_PORT           Metrics HTTP server port
	OBJECTFS_HEALTH_PORT            Health check HTTP server port

# Validation and Safety

Comprehensive input validation and safety checks:

Path Validation:
  - Mount point must exist and be a directory
  - Mount point must be empty
  - Path traversal prevention (no ".." components)
  - Write permissions required for mount point
  - FUSE availability verification

URI Validation:
  - Supported scheme validation (s3://, gs://, azure://)
  - Bucket name format validation
  - Path component validation
  - Reserved name prevention

Configuration Validation:
  - YAML syntax validation
  - Required field presence
  - Value range validation (e.g., positive integers)
  - Resource limit validation (memory, connections)
  - Dependency validation (feature prerequisites)

# Development and Debugging

Built-in debugging and development support:

Debug Mode:

	--debug                         Enable comprehensive debug logging
	--dry-run                       Validate without mounting
	--log-level DEBUG               Detailed operation logging

Debug Information:
  - Configuration loading details
  - Component initialization progress
  - Mount operation details
  - Performance metrics
  - Error context and stack traces

Development Features:
  - Configuration validation without side effects
  - Detailed startup logging
  - Component health verification
  - Resource usage reporting

# Integration Examples

Common integration patterns:

Systemd Service:

	[Unit]
	Description=ObjectFS for %i
	After=network.target

	[Service]
	Type=simple
	User=objectfs
	ExecStart=/usr/local/bin/objectfs s3://%i /mnt/objectfs/%i
	ExecStop=/bin/fusermount -u /mnt/objectfs/%i
	Restart=always
	RestartSec=5

	[Install]
	WantedBy=multi-user.target

Docker Container:

	FROM alpine:latest
	RUN apk add --no-cache fuse
	COPY objectfs /usr/local/bin/
	ENTRYPOINT ["/usr/local/bin/objectfs"]
	CMD ["s3://default-bucket", "/mnt/data"]

Kubernetes DaemonSet:

	apiVersion: apps/v1
	kind: DaemonSet
	metadata:
	  name: objectfs
	spec:
	  template:
	    spec:
	      containers:
	      - name: objectfs
	        image: objectfs:latest
	        securityContext:
	          privileged: true
	        volumeMounts:
	        - name: objectfs-mount
	          mountPath: /mnt/objectfs
	          mountPropagation: Bidirectional

# Performance Considerations

Optimized for production deployment:

Startup Performance:
  - Fast configuration parsing and validation
  - Parallel component initialization where possible
  - Lazy loading of expensive resources
  - Early validation to fail fast

Memory Usage:
  - Minimal memory footprint for CLI parsing
  - Configurable memory limits for caches and buffers
  - Memory pool reuse for frequent operations
  - Garbage collection tuning for long-running operation

CPU Usage:
  - Minimal CPU overhead for argument parsing
  - Efficient configuration file parsing
  - Optimized logging with sampling
  - Background operation scheduling

This package provides the primary user interface to ObjectFS, ensuring ease of use
while maintaining the flexibility and power needed for enterprise deployments.
*/
package main
