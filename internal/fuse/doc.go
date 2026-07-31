/*
Package fuse translates POSIX filesystem operations into object storage operations.

# Platform support

Linux and macOS only. Every file in this package carries a `//go:build linux || darwin`
constraint.

Windows is not supported. A `cgofuse` build tag existed through v0.10.0 and never compiled —
`filesystem.go` carried no build constraint of its own, so under the tag `OpenFile` was declared
twice, and the resulting duplicate-symbol error was masked by a missing `fuse.h`. It was removed
in v0.10.1 along with the `github.com/winfsp/cgofuse` dependency. It was also a silently divergent
382-line subset of this package's 727 lines: it never received the Unlink/Rmdir fix, so under that
tag `rm` reported success while the S3 object survived.

If Windows support is wanted, the path is a second thin shim over `internal/vfs` — not a fork of
this package. See the README.

macOS requires macFUSE (`brew install --cask macfuse`). This is not a consequence of the binding
choice: go-fuse's darwin mount execs
`/Library/Filesystems/macfuse.fs/Contents/Resources/mount_macfuse`, and cgofuse needed the same
headers. The two had identical macOS requirements.

# Architecture

	┌─────────────────────────────────────────────┐
	│              User Applications              │
	│        (ls, cat, cp, vim, databases)        │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│               Kernel VFS Layer              │
	│            (POSIX System Calls)             │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│      FUSE Driver (kernel / macFUSE)         │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│  github.com/hanwen/go-fuse/v2               │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│            ObjectFS FUSE shim               │  ← This package
	│   go-fuse types ⇄ vfs calls, errno mapping  │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│               internal/vfs                  │
	│  inodes, attributes, handles, dirty ranges  │
	└─────────────────────────────────────────────┘
	                      │
	┌─────────────────────────────────────────────┐
	│         pkg/types.Backend (S3, …)           │
	└─────────────────────────────────────────────┘

POSIX semantics live in `internal/vfs`, which depends on `pkg/types.Backend` and nothing FUSE, so
it is testable without a mount. This package is the translation shim above it.

# Implemented operations

For the authoritative list of which POSIX operations work, which fail by design, and which are not
implemented, see the supported-operations table in the README. Do not infer support from the
presence of a method on `FilesystemInterface` — `internal/filesystem/interface.go` declares the
full surface and its only implementation is a test mock.

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

# Object storage mapping

	File path       → object key
	File content    → object data
	Directory path  → object key prefix
	Directory list  → prefix-based ListObjects
	Empty directory → zero-byte marker object at "<prefix>/"

Special files are not supported. Device nodes, named pipes, symlinks, and hard links have no
representation in S3 and none is synthesized — hard links never will be, since S3 has no such
concept. Symlink support, if added, will store the target in object metadata; that is not
implemented today.

# Permissions

Ownership and mode are not persisted. Every entry reports the configured `DefaultUID`,
`DefaultGID`, and `DefaultMode` from `MountConfig.Permissions`. `chmod` and `chown` are not
implemented, so their effects do not survive a remount.

Access control is enforced by the S3 credentials the process holds, not by the reported mode bits.
A mode of 0644 on a mount whose credentials grant `s3:PutObject` to everything does not make the
data read-only.

# Error handling

Backend errors are mapped to errno: network and unclassified failures to EIO, permission failures
to EACCES, absent objects to ENOENT. Retry and circuit breaking happen in the backend
(`internal/storage/s3`), not here.

Where an operation is not implemented, this package returns an explicit error rather than the
go-fuse default. That default is *success* for several operations, which is how `rm` came to report
deletions that never happened through v0.10.0.

# Thread safety

FUSE delivers concurrent Read and Write calls against the same open file descriptor, so per-handle
mutable state (`dirty`, `modified`, `size`, access tracking) is guarded by `OpenFile.accessMu` and
the handle table by `FileSystem.mu`. All exported entry points are safe for concurrent use.
*/
package fuse
