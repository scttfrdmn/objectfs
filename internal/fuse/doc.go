/*
Package fuse provides cross-platform FUSE filesystem implementation for ObjectFS.

This package implements POSIX-compliant filesystem operations that translate standard
file and directory operations into object storage operations. It supports multiple
FUSE implementations through build constraints, providing optimal performance and
compatibility across Linux, macOS, and Windows platforms.

# Architecture Overview

The FUSE layer acts as the bridge between POSIX applications and object storage:

	┌─────────────────────────────────────────────┐
	│              User Applications              │
	│        (ls, cat, cp, vim, databases)       │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│              Kernel VFS Layer              │
	│           (POSIX System Calls)             │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│               FUSE Driver                   │
	│          (Platform-specific)               │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│            ObjectFS FUSE Layer              │  ← This Package
	│  ┌─────────────────────────────────────────┐  │
	│  │        Cross-Platform Abstraction      │  │
	│  │  ┌─────────────┐ ┌─────────────────┐   │  │
	│  │  │ go-fuse     │ │ cgofuse         │   │  │
	│  │  │ (Linux)     │ │ (macOS/Windows) │   │  │
	│  │  └─────────────┘ └─────────────────┘   │  │
	│  └─────────────────────────────────────────┘  │
	│                     │                       │
	│  ┌─────────────────────────────────────────┐  │
	│  │         POSIX Operation Layer          │  │
	│  │  • File Operations  • Directory Ops   │  │
	│  │  • Metadata Ops     • Permission Mgmt │  │
	│  └─────────────────────────────────────────┘  │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│             Object Storage Backend          │
	│         (S3, GCS, Azure, etc.)            │
	└─────────────────────────────────────────────┘

# Platform Support

Multi-platform FUSE implementation with build constraints:

Default Build (go-fuse):
- Target: Linux (primary platform)
- Implementation: github.com/hanwen/go-fuse/v2
- Performance: Optimal for Linux environments
- Features: Full POSIX compliance, high performance

CGO Build (cgofuse):
- Target: macOS, Windows, Linux (fallback)
- Implementation: github.com/billziss-gh/cgofuse
- Performance: Cross-platform compatibility
- Features: Broader OS support, consistent behavior

Build Selection:
	// Linux with high performance
	go build -tags default ./...
	
	// Cross-platform compatibility
	go build -tags cgofuse ./...

# FileSystem Operations

Complete POSIX filesystem operation support:

File Operations:
- open(), read(), write(), close() - Standard file I/O
- lseek(), truncate() - File positioning and size management
- fsync(), fdatasync() - Data synchronization
- lock(), unlock() - File locking support

Directory Operations:
- opendir(), readdir(), closedir() - Directory enumeration
- mkdir(), rmdir() - Directory creation and removal
- rename() - File and directory renaming

Metadata Operations:
- stat(), fstat(), lstat() - File metadata retrieval
- chmod(), chown() - Permission and ownership changes
- utimes(), utime() - Timestamp modification
- link(), symlink(), readlink() - Link management

Extended Attributes:
- getxattr(), setxattr() - Custom attribute management
- listxattr(), removexattr() - Attribute enumeration and removal
- Support for object storage metadata mapping

# Configuration

Flexible mount configuration options:

	config := &fuse.MountConfig{
		MountPoint: "/mnt/objectfs",
		Options: &fuse.MountOptions{
			ReadOnly:     false,
			AllowOther:   true,
			AllowRoot:    false,
			
			// Performance tuning
			MaxRead:      128 * 1024,  // 128KB read buffer
			MaxWrite:     128 * 1024,  // 128KB write buffer
			
			// Caching
			AttrTimeout:  5 * time.Second,
			EntryTimeout: 10 * time.Second,
			
			// Platform-specific
			FSName:       "objectfs",
			Subtype:      "s3",
		},
		Permissions: &fuse.Permissions{
			DefaultUID:  1000,
			DefaultGID:  1000,
			DefaultMode: 0644,
			DirMode:     0755,
		},
	}

# Usage Examples

Basic filesystem mounting:

	// Create filesystem
	filesystem := fuse.NewFileSystem(backend, cache, writeBuffer, metrics, config)
	
	// Create mount manager
	mountManager := fuse.CreatePlatformMountManager(
		backend, 
		cache, 
		writeBuffer, 
		metrics, 
		config,
	)
	
	// Mount filesystem
	err := mountManager.Mount(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer mountManager.Unmount()

File operations through mounted filesystem:

	// Standard POSIX operations work transparently
	
	// Create file
	file, err := os.Create("/mnt/objectfs/data.txt")
	if err != nil {
		log.Fatal(err)
	}
	
	// Write data
	_, err = file.WriteString("Hello, ObjectFS!")
	if err != nil {
		log.Fatal(err)
	}
	file.Close()
	
	// Read file
	data, err := os.ReadFile("/mnt/objectfs/data.txt")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Content: %s\n", data)

Directory operations:

	// Create directory
	err := os.Mkdir("/mnt/objectfs/logs", 0755)
	
	// List directory contents
	entries, err := os.ReadDir("/mnt/objectfs")
	for _, entry := range entries {
		info, _ := entry.Info()
		fmt.Printf("%s %d %v\n", 
			entry.Name(), 
			info.Size(), 
			info.ModTime())
	}

# Performance Optimizations

Multiple performance optimization strategies:

Intelligent Caching:
- Metadata caching with configurable TTL
- Directory entry caching for fast lookups
- Negative caching for non-existent files
- Write-through and write-back caching modes

Read-Ahead:
- Sequential read pattern detection
- Predictive data prefetching
- Configurable read-ahead buffer sizes
- Background prefetching workers

Write Optimization:
- Write buffering and batching
- Delayed write synchronization
- Compression for suitable content types
- Multipart upload for large files

Connection Management:
- Connection pooling for concurrent operations
- Keep-alive connection reuse
- Automatic retry with exponential backoff
- Health monitoring and failover

# Object Storage Mapping

Translation between POSIX and object storage concepts:

Files to Objects:
- File path → Object key
- File content → Object data
- File metadata → Object metadata/tags
- File permissions → Mapped to metadata

Directories to Virtual Structure:
- Directory paths → Object key prefixes
- Directory listings → Prefix-based object enumeration
- Directory metadata → Synthetic metadata generation
- Empty directories → Zero-byte marker objects

Special Files:
- Symbolic links → Stored as object metadata
- Hard links → Reference counting in metadata
- Device files → Not supported (returns appropriate errors)
- Named pipes → Not supported (returns appropriate errors)

# Permission Model

POSIX permission mapping to object storage:

Permission Storage:
- POSIX permissions stored in object metadata
- ACLs mapped to object storage ACLs where supported
- Ownership information preserved in metadata
- Permission inheritance for new files/directories

Default Behavior:
- Configurable default UID/GID for all operations
- Consistent permission model across platforms
- Support for umask-style permission masking
- Administrative override capabilities

Security Considerations:
- Object storage credential-based access control
- FUSE-level permission enforcement
- Configurable access restrictions (allow_other, allow_root)
- Secure credential handling and rotation

# Error Handling

Comprehensive error handling and translation:

POSIX Error Mapping:
- Object storage errors → Standard errno values
- Network errors → EIO (I/O error)
- Permission errors → EACCES (Permission denied)
- Not found errors → ENOENT (No such file or directory)

Retry Logic:
- Transient error automatic retry
- Exponential backoff strategies
- Circuit breaker for persistent failures
- Graceful degradation modes

Error Recovery:
- Connection failure recovery
- Partial operation cleanup
- Consistent state maintenance
- User notification strategies

# Statistics and Monitoring

Comprehensive operation monitoring:

Operation Metrics:
- File operation counters (reads, writes, opens, closes)
- Throughput measurements (bytes/second)
- Latency distributions (operation duration)
- Error rate tracking

Cache Metrics:
- Cache hit/miss ratios
- Cache utilization statistics
- Eviction rates and patterns
- Cache effectiveness analysis

Performance Metrics:
- Concurrent operation tracking
- Queue depth monitoring
- Resource utilization (memory, connections)
- Background operation progress

Health Monitoring:
- Mount status verification
- Backend connectivity checks
- Performance threshold monitoring
- Automated health recovery

# Thread Safety

Designed for high-concurrency operation:

- All FUSE operations are inherently concurrent
- Thread-safe internal data structures
- Proper synchronization for shared resources
- Lock-free data paths where possible
- Connection pool thread safety

# Platform-Specific Features

Optimizations for different operating systems:

Linux Optimizations:
- Direct I/O support for large files
- Advanced caching strategies
- Memory-mapped file support
- Efficient directory iteration

macOS Optimizations:
- FSEvents integration for change monitoring
- Spotlight metadata compatibility
- Resource fork handling
- macOS-specific permission models

Windows Optimizations:
- Windows file attribute mapping
- NTFS stream support where applicable
- Windows-specific error code mapping
- Integration with Windows Security Model

This package provides the critical bridge between standard POSIX applications
and modern object storage systems, enabling transparent, high-performance
access to cloud storage through familiar filesystem interfaces.
*/
package fuse