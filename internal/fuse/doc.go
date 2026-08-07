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
			MaxWrite:     128 * 1024,  // 128KB write buffer

			// Caching. AttrTimeout also becomes the timeout the nodes report from Getattr and
			// Setattr, so the two cannot disagree.
			AttrTimeout:  5 * time.Second,
			EntryTimeout: 10 * time.Second,

			// What the kernel is told about caching and dispatch. All three default to off, and off
			// is the kernel's own behavior in each case, so an ordinary mount sets none of them.
			// DirectIO and KeepCache are returned from every Open rather than set at mount time, and
			// DirectIO wins if both are set. SyncRead is go-fuse's field; it withholds
			// CAP_ASYNC_READ, which is what kernel readahead depends on.
			DirectIO:  false,
			KeepCache: false,
			SyncRead:  false,

			// Platform-specific
			FSName:       "objectfs",
			Subtype:      "s3",
		},
		Permissions: &fuse.Permissions{
			UID:      1000,
			GID:      1000,
			FileMode: 0644,
			DirMode:  0755,
		},
	}

Every option above takes effect, and that is a property of the type rather than a claim about this
example. `MountOptions` and `Config` between them carried fourteen fields — `MaxRead`, `DirectIO`,
`KeepCache`, `BigWrites`, `AsyncRead`, `WritebackCache`, the three splice flags, `AllowOther` on
`Config`, `ReadAhead`, `WriteBuffer`, and `Concurrency` — that were read by nothing. Three of them are
back, plumbed and tested (#180); the rest are gone. `MaxWrite` is the only size that is settable:
go-fuse derives `max_read` and `MaxPages` from it, so the read size is not independently configurable.

Two of the four #180 nominated were not plumbable, and both reasons are worth stating because each
would otherwise look like an omission:

  - Splice. go-fuse only splices a [fuse.ReadResult] backed by a file descriptor, and
    [FileHandle.Read] returns [fuse.ReadResultData] at every return site — the bytes come from S3 or
    from an in-memory cache, and there is no fd to splice from. Setting `DisableSplice` would disable
    a path this filesystem never takes.
  - The writeback cache. It maps to `fuse.MountOptions.ExplicitDataCacheControl`, which asks the
    kernel to stop invalidating data caches automatically and makes the filesystem responsible for
    doing it. There is not one `NotifyContent`, `NotifyEntry`, or `NotifyInvalInode` call in this
    repository, so enabling it would make stale pages permanent rather than bounded.

The yaml tags on these structs still bind to nothing, and that is now deliberate rather than an
oversight. As of v0.11.0 [config.Configuration] does have a `fuse` section — the first one any loader
has read — and it reaches these structs through `internal/adapter`, not by being decoded into them.
The operator-facing names are [config.FUSEConfig]'s.

# Usage Examples

Basic filesystem mounting:

	// Create filesystem. mountCtx is the mount's lifetime, not this call's: canceling it is what
	// stops the read-ahead manager's prefetch workers.
	filesystem := fuse.NewFileSystem(mountCtx, backend, cache, writeBuffer, metrics, config)

	// Create mount manager
	mountManager := fuse.CreatePlatformMountManager(
		mountCtx,
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

For a regular file, ownership and mode are persisted as object user metadata
(`objectfs-uid`, `objectfs-gid`, `objectfs-mode`) and survive a remount. `chmod`, `chown`, and
`touch` are implemented and flush synchronously, because both take a path rather than a descriptor:
there is no handle whose release would later make the change durable, so a change not written
immediately would never be written at all. An object carrying none of those keys — one written by
`aws s3 cp`, boto3, or any other tool — reports the values from `MountConfig.Permissions`, defaulting
to the mounting user and 0644.

Setuid, setgid, and sticky are refused with ENOTSUP rather than stored. `vfs.Attr.Mode` holds
permission bits only, and a setuid bit that appeared to persist would promise an escalation this
filesystem cannot perform.

A directory is a key prefix, so it has no metadata to hold anything: it reports
`Permissions.DirMode` (default 0755), and `chmod` on it returns ENOTSUP rather than reporting a
change that the next stat would contradict. Its times are synthetic — `utimes` on a directory is
accepted and stores nothing, because failing it would make every `tar -x` and `cp -a` report errors
for an attribute with nowhere to go.

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
mutable state (`dirty` and the access counters) is guarded by `OpenFile.accessMu` and the handle
table by `FileSystem.mu`. All exported entry points are safe for concurrent use.

A handle holds no size and no mode. Both were per-handle fields through v0.10.0, and both were a
second source of truth for a value another descriptor can change underneath: a read is clamped
against the size stat reported, so a handle's stale copy silently truncated reads of a file that had
grown. Size comes from `internal/vfs` (which knows the pending writes) or from the object's metadata;
mode comes from the object's metadata.
*/
package fuse
