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

	objectfs <command> [options] [arguments]

Commands:

	mount     Mount a bucket on a directory
	unmount   Unmount a filesystem by path (also spelled umount)
	version   Print the version and exit
	help      Print the usage message

Basic Usage:

	# Mount S3 bucket with default settings
	objectfs mount s3://my-bucket /mnt/s3

	# Mount with custom configuration
	objectfs mount --config /etc/objectfs/config.yaml s3://data-bucket /mnt/data

	# High-performance configuration
	objectfs mount --cache-size 4GB --max-concurrency 200 s3://bucket /mnt/bucket

	# Dry run to validate configuration
	objectfs mount --dry-run --config config.yaml s3://test-bucket /tmp/mount

	# Unmount, from any process — an operator's shell or a unit's ExecStop
	objectfs unmount /mnt/s3

Flags come before the positional arguments, because Go's flag package stops parsing at the first
argument that is not a flag. `objectfs mount s3://b /mnt --debug` leaves --debug as a third
positional rather than setting it; the command reports that instead of ignoring it.

The form without a subcommand still works and is not deprecated:

	objectfs s3://my-bucket /mnt/s3
	objectfs --config /etc/objectfs/config.yaml s3://my-bucket /mnt/s3

It is what every invocation written before v0.11.0 looks like, including the ones in scripts nobody
is going to revisit. A first argument carrying a URI scheme or a leading dash is routed to mount; a
bare word that is not a command is a usage error naming itself, so a typo — `objectfs moutn s3://b
/mnt` — does not become an attempt to mount a bucket called "moutn".

Both arguments to mount are optional when the configuration file supplies them, as mount.uri and
mount.mount_point:

	objectfs mount --config /etc/objectfs/research-data.yaml --foreground

That is the form a systemd template unit needs. `systemctl start objectfs@research-data` gives the
unit only its instance name, and one unit file serves every instance, so the bucket cannot be on the
command line — it has to come from the per-instance file the instance name selects.

Exit codes, because a unit file and a shell script both branch on them:

	0   the operation succeeded
	1   the command was right and the operation failed
	2   the command line was wrong; nothing was attempted

# Configuration Management

Multi-source configuration with precedence hierarchy:

 1. Default values (lowest priority)
 2. Configuration files (YAML)
 3. Environment variables (OBJECTFS_*)
 4. Command-line arguments (highest priority)

Options for mount:

	--config string           Configuration file path
	--mount-point string      Directory to mount on, when not given as an argument
	--foreground              Stay in the foreground; see below
	--log-level string        Log level (DEBUG, INFO, WARN, ERROR) [default: INFO]
	--cache-size string       Cache size (e.g., 2GB, 512MB)
	--max-concurrency int     Maximum concurrent operations
	--dry-run                 Validate configuration and exit without mounting
	--debug                   Enable debug mode (sets log level to DEBUG)

--foreground names what already happens. ObjectFS does not fork: it serves the mount in the process
that started it until that process is signaled, and there is no background mode to ask for. The flag
is accepted because init systems and scripts pass it, and refusing it would break invocations that
are correct about the behavior. It is listed here rather than under "planned" — where a previous
version of this file put it, next to a parenthetical claiming daemon mode was the default — because a
flag that is accepted and does nothing is worth explaining exactly once. To run ObjectFS as a
service, use a systemd unit with Type=simple rather than backgrounding it with `&`, so that the
unmount on stop is the one ObjectFS performs rather than a SIGKILL.

These have no flag. Three have a configuration setting instead; the rest are not settable at all, and
are listed so the absence is a stated fact rather than a flag someone tries:

	--region                  storage.s3.region
	--endpoint                storage.s3.endpoint
	--profile                 storage.s3.profile, or AWS_PROFILE in the environment
	--allow-other             not settable. fuse.MountOptions.AllowOther exists as a field and
	                          internal/adapter's buildMountOptions does not set it, so there is
	                          nothing for a flag to reach
	--read-only               not settable, same shape: fuse.Config.ReadOnly is read on the mount
	                          path but arrives only from MountOptions.ReadOnly, which nothing
	                          populates from a config file
	--uid, --gid              not settable. MountConfig.Permissions is honored by
	                          CreatePlatformMountManager and is nil on the adapter's path, so files
	                          report the mounting process's uid and gid

A previous version of this section listed all of these under "Planned Features (Future Versions)".
That is the shape of claim this repository has been removing: a reader cannot tell a flag that is
coming from a flag nobody is working on, and four of these have a half-built path — a field, a yaml
tag, a constructor that honors it — that reads as plumbing when nothing fills it.

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
	objectfs mount s3://production-data /mnt/data

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
	objectfs mount --config config.yaml s3://data-bucket /mnt/data

Performance tuning:

	# High-performance configuration for large workloads
	objectfs mount \
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
	objectfs mount s3://env-configured-bucket /mnt/data

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
 1. Signal reception (SIGINT or SIGTERM)
 2. Graceful filesystem unmounting, which flushes buffered writes
 3. Component cleanup and resource release
 4. Exit, with a non-zero code if the unmount could not flush

Step 4 is the one worth stating: the exit code reports whether the shutdown flushed, because this is
the last point at which buffered writes reach S3. A shutdown that could not flush is a data-loss
event, and reporting it as a clean exit is what makes it silent — `systemctl stop` would show success.

Signal Handling:

	SIGINT (Ctrl+C)   - Graceful shutdown
	SIGTERM           - Graceful shutdown
	SIGHUP            - not handled; Go's default disposition terminates the process

The handler is registered before the mount is started, not after. A SIGTERM arriving during startup —
which any unit with a TimeoutStartSec will send — would otherwise take the default disposition and
kill the process with the mount half-established and nothing run to tear it down.

SIGHUP is deliberately not handled. It used to be, alongside SIGINT and SIGTERM, with all three
treated as shutdown — while the README advertised "zero-downtime configuration reloading", so `kill
-HUP` unmounted the filesystem of anyone who believed that. Reload is not implemented. Leaving SIGHUP
to Go's default still terminates the process, but it is no longer documented as something else; when
reload lands, this is where it goes.

# Error Handling

Every message below was captured from the binary rather than written here, because the version of
this section that was written here described a release ago: it showed `Error: Expected exactly 2
arguments (storage-uri and mount-point)` and `Error: Invalid storage URI: unsupported scheme
'file://' (only s3:// supported)`, and the command prints neither. Each is prefixed with the
subcommand it came from, so a journal line names the operation as well as the fault.

Command line wrong — exit 2, nothing attempted:

	objectfs mount: no storage URI. Give one as an argument — `objectfs mount s3://my-bucket
	  /mnt/point` — or set mount.uri in the config file
	objectfs mount: expected at most a storage URI and a mount point, got 3 arguments: s3://b
	  /tmp/mp-empty extra
	objectfs mount: storage URI "file:///x" uses scheme "file"; only s3:// is supported in this build
	objectfs mount: storage URI "s3://b": bucket name "b" is 1 character; S3 requires 3 to 63

Mount point rejected before anything is mounted — exit 2:

	objectfs mount: mount point /nonexistent/path does not exist; create it first
	  (mkdir -p /nonexistent/path)
	objectfs mount: mount point /tmp/data/x is not a directory
	objectfs mount: mount point /tmp/data is not empty (it contains "x"); mounting over it would
	  hide what is already there for as long as the mount lasts

Configuration — exit 2:

	objectfs mount: cannot load /tmp/bad.yaml: failed to parse config file /tmp/bad.yaml: yaml:
	  line 1: did not find expected node content

Each of these names the value it rejected and, where there is one, the command that fixes it. That
is a deliberate shape and not decoration: these are read out of `journalctl -u objectfs@instance`
by someone who did not write the config file, and a message that says a mount point is unusable
without saying which mount point sends them to the unit file to find out.

Two orderings are worth knowing, because they decide which of two real faults you are shown first.
The storage URI is validated before the mount point, so `objectfs mount s3://b /nonexistent` reports
the bucket name and says nothing about the directory. And the configuration file is loaded before
either, so a syntax error in it is reported even when the command line is also wrong.

Runtime failures — exit 1, the command was right and the operation was not. These come from
internal/adapter and reach stderr under the same prefix, wrapping the cause rather than summarizing
it, so an S3 AccessDenied, an absent macFUSE, and an EPERM on /dev/fuse stay three distinct
messages instead of one "the mount did not come up":

	objectfs mount: failed to initialize S3 backend: ...
	objectfs mount: failed to initialize cache: ...
	objectfs mount: failed to mount filesystem: ...

And the one at the other end of the process's life, on the shutdown path:

	objectfs: shutdown failed, so data may not have reached S3: ...

That one is an exit-1 as well, and it is the message this program exists to be able to print. Every
other failure here happened before anything was mounted, so nothing was lost; this one means the
mount served writes and the flush at unmount did not complete. See Application Lifecycle above.

# Configuration File Format

YAML configuration with comprehensive options:

	# ObjectFS Configuration File
	global:
	  log_level: INFO
	  log_file: /var/log/objectfs.log

	performance:
	  cache_size: "4GB"
	  write_buffer_size: "64MB"
	  max_concurrency: 200
	  connection_pool_size: 8
	  read_ahead:
	    enabled: true
	    window_size: "64KB"    # floor on the prefetch length
	    min_sequential: 3      # sequential reads before prefetching starts
	    concurrent_reads: 4    # prefetch workers
	    ttl: 5m

	cache:
	  ttl: 5m
	  max_entries: 100000
	  eviction_policy: "weighted_lru"
	  persistent_cache:
	    enabled: true
	    directory: "/var/cache/objectfs"
	    max_size: "10GB"

	monitoring:
	  metrics:
	    enabled: true
	    addr: 127.0.0.1:8080   # unauthenticated; loopback by default
	  health_checks:
	    enabled: true
	    addr: 127.0.0.1:8081   # unauthenticated; loopback by default

	# Additional sections: write_buffer, network, security, features

# Environment Variables

Comprehensive environment variable support:

Core Settings:

	OBJECTFS_LOG_LEVEL              Log level override
	OBJECTFS_CONFIG_FILE            Default configuration file path

Performance Settings:

	OBJECTFS_CACHE_SIZE                  Cache size (e.g., "4GB")
	OBJECTFS_MAX_CONCURRENCY             Maximum concurrent operations
	OBJECTFS_WRITE_BUFFER_SIZE           Write buffer size
	OBJECTFS_COMPRESSION_ENABLED         Enable compression (true/false)

Read-ahead Settings:

	OBJECTFS_READAHEAD_ENABLED           Prefetch ahead of a sequential reader (strictly "true")
	OBJECTFS_READAHEAD_WINDOW_SIZE       Floor on the prefetch length (e.g., "256KB")
	OBJECTFS_READAHEAD_MIN_SEQUENTIAL    Sequential reads before prefetching starts
	OBJECTFS_READAHEAD_CONCURRENT_READS  Prefetch workers; must be > 0

AWS Settings:

	AWS_ACCESS_KEY_ID               AWS access key
	AWS_SECRET_ACCESS_KEY           AWS secret key
	AWS_DEFAULT_REGION              Default AWS region
	AWS_PROFILE                     AWS CLI profile name
	AWS_SESSION_TOKEN               Session token for temporary credentials

Monitoring:

	OBJECTFS_METRICS_ENABLED        Serve the metrics endpoint (true/false)
	OBJECTFS_METRICS_ADDR           Metrics listener address (host:port)
	OBJECTFS_HEALTH_ENABLED         Serve the health endpoint (true/false)
	OBJECTFS_HEALTH_ADDR            Health listener address (host:port)

Both endpoints are unauthenticated and both are on by default, so the two _ENABLED variables are how
a mount closes one without editing a config file. Unlike the feature-flag variables above, a value
that is not a boolean fails startup and names the variable rather than being coerced to false.

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

configs/systemd/objectfs@.service is the shipped unit and the authority — install that rather than
retyping this. These are the four lines of it that this command's design is what it is because of,
and the pre-v0.11.0 version of this block had none of them:

	ExecStart=/usr/bin/objectfs mount --config /etc/objectfs/%i.yaml --mount-point /mnt/objectfs/%i --foreground
	ExecStop=/usr/bin/objectfs unmount /mnt/objectfs/%i
	TimeoutStopSec=90
	Restart=on-failure

The shipped unit wraps that ExecStart with a trailing backslash, which systemd honors; it is one line
here because a doc comment's code block and a continuation do not survive gofmt together.

  - The bucket comes from /etc/objectfs/%i.yaml as mount.uri, and only the mount point is on the
    command line. That is the split a template needs: one unit file serves every instance and
    receives only the instance name, so the two halves come from the two places that know them. The
    old block wrote `ExecStart=... s3://%i /mnt/objectfs/%i`, which makes the instance name and the
    bucket name one string — fine until two mounts of one bucket are wanted, or a prefix, or a
    bucket whose name is not a legal systemd instance.
  - ExecStop runs `objectfs unmount`. The old block ran `/bin/fusermount -u` directly. That is the
    first thing ObjectFS tries too, but it is one of several candidates — absent on a minimal image,
    spelled `fusermount` on libfuse 2 and `fusermount3` on libfuse 3, and unnecessary for a root
    caller who can use umount(2). Calling it from the unit gives systemd a bare exit status for all
    of those cases; `objectfs unmount` reports which methods ran, which were not installed, and the
    lsof invocation that names whatever is holding the mount open.
  - TimeoutStopSec is the flush window. SIGTERM makes this process unmount, which flushes buffered
    writes to S3, and the exit code says whether that completed. Too short a value here is a SIGKILL
    through buffered data.
  - Restart=on-failure, not always: `always` remounts a filesystem after a clean `systemctl stop`.

Docker Container:

	FROM alpine:latest
	RUN apk add --no-cache fuse3
	COPY objectfs /usr/local/bin/
	ENTRYPOINT ["/usr/local/bin/objectfs", "mount"]
	CMD ["s3://default-bucket", "/mnt/data"]

fuse3 rather than fuse: the mount goes through go-fuse, which execs fusermount3. The container needs
--device /dev/fuse and either --cap-add SYS_ADMIN or --privileged; without them the mount fails at
the FUSE device with an EPERM rather than anywhere in this program.

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
