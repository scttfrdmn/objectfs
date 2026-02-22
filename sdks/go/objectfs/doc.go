/*
Package objectfs provides a Go SDK for the ObjectFS filesystem.

# Overview

ObjectFS exposes two modes of operation:

  - Object mode: direct S3 object operations without FUSE. Works everywhere.
  - Mount mode: full POSIX filesystem via FUSE. Requires FUSE support on the host.

Object mode is available immediately after calling New. Mount mode is activated
via Mount and deactivated via Unmount.

# Usage

Basic object operations (no FUSE required):

	client, err := objectfs.New(ctx, "my-bucket",
	    objectfs.WithRegion("us-west-2"),
	    objectfs.WithCacheSize("1GB"),
	)
	if err != nil {
	    log.Fatal(err)
	}
	defer client.Close()

	// Write an object
	if err := client.Put(ctx, "data/hello.txt", []byte("hello world")); err != nil {
	    log.Fatal(err)
	}

	// Read it back
	data, err := client.Get(ctx, "data/hello.txt", 0, 0)
	if err != nil {
	    log.Fatal(err)
	}

	// List objects under a prefix
	objs, err := client.List(ctx, "data/", 100)

FUSE mount (Linux/macOS with FUSE installed):

	if err := client.Mount(ctx, "/mnt/my-bucket"); err != nil {
	    log.Fatal(err)
	}
	defer client.Unmount()

	// Files are now accessible at /mnt/my-bucket

# Error Handling

The package exposes sentinel errors for common failure cases:

	data, err := client.Get(ctx, "missing-key", 0, 0)
	if errors.Is(err, objectfs.ErrNotFound) {
	    // object does not exist
	}

	err = client.Mount(ctx, "/mnt/x")
	err = client.Mount(ctx, "/mnt/x") // second call
	if errors.Is(err, objectfs.ErrAlreadyMounted) {
	    // already mounted
	}

Available sentinels: ErrNotFound, ErrAccessDenied, ErrNotMounted,
ErrAlreadyMounted, ErrInvalidConfig.
*/
package objectfs
