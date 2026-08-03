//go:build (linux || darwin) && fuse_mount

package fuse

// The kernel's half of the direct-I/O contract, which is the only part of #180 a mount is required to
// observe.
//
// kernel_options_test.go asserts that FOPEN_DIRECT_IO reaches the kernel. That is necessary and it is
// not sufficient: the flag's whole purpose is to change what the *kernel* does with a second read(2) of
// bytes it already has, and no amount of inspecting OpenOut can show that. This file reads the same
// offset twice through a real mount and counts how many READ requests arrive.
//
// Behind the fuse_mount build tag, and therefore run by nothing by default, because it needs
// /dev/fuse — absent on GitHub's ubuntu-latest runners and absent on a macOS host without macFUSE.
// That is a real coverage gap and it is stated rather than papered over: CI gates the seams, and this
// runs where a kernel is available. `make test-fuse-mount` is the entry point.
//
// A test skipping itself for want of a device would be worse than a build tag: it would report success.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// liveMount brings up a real FUSE mount over a real S3 endpoint and returns the mount point.
func liveMount(t *testing.T, opts *MountOptions) (string, *MountManager) {
	t.Helper()

	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Fatalf("no /dev/fuse: %v. This file is behind the fuse_mount build tag precisely because it "+
			"needs one; it fails rather than skipping so a run that cannot test anything does not "+
			"report success", err)
	}

	srv := testaws.Start(t)

	writer, err := vfs.NewWriter(context.Background(), srv.Backend())
	if err != nil {
		t.Fatalf("vfs.NewWriter: %v", err)
	}

	mountPoint := t.TempDir()

	pfs := CreatePlatformMountManager(srv.Backend(), nil, writer, nil, &MountConfig{
		MountPoint: mountPoint,
		Options:    opts,
	})

	mm, ok := pfs.(*MountManager)
	if !ok {
		t.Fatalf("CreatePlatformMountManager returned %T, want *MountManager", pfs)
	}

	if err := mm.Mount(context.Background()); err != nil {
		t.Fatalf("Mount(%s): %v", mountPoint, err)
	}

	t.Cleanup(func() {
		if err := mm.Unmount(); err != nil {
			t.Errorf("Unmount(%s): %v", mountPoint, err)
		}
	})

	// Seeded after the mount so the read below is the first thing that touches the object.
	srv.PutObject("f.dat", bytes.Repeat([]byte("x"), 4096))

	return mountPoint, mm
}

// TestDirectIOMakesTheKernelReReadFromTheFilesystem is the effect assertion #180 asks for.
//
// Two read(2) calls at the same offset on one descriptor. Without FOPEN_DIRECT_IO the kernel serves the
// second from the page cache and the filesystem sees one READ; with it, the filesystem sees both. The
// number of READs that arrive is the observable, and it is the thing the flag exists to change.
func TestDirectIOMakesTheKernelReReadFromTheFilesystem(t *testing.T) {
	tests := []struct {
		name        string
		directIO    bool
		wantAtLeast int64
		wantAtMost  int64
		why         string
	}{
		{
			name:        "direct_io: both reads reach the filesystem",
			directIO:    true,
			wantAtLeast: 2,
			wantAtMost:  64,
			why:         "the kernel was told not to cache this file's data",
		},
		{
			name:        "without direct_io: the page cache serves the second read",
			directIO:    false,
			wantAtLeast: 1,
			wantAtMost:  1,
			why:         "the kernel caches by default, and 4096 bytes fit in one readahead window",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Not parallel: each subtest mounts, and a mount is a process-wide resource with a
			// unmount to sequence against.
			mountPoint, mm := liveMount(t, &MountOptions{
				FSName:       "objectfs",
				MaxWrite:     128 * 1024,
				AttrTimeout:  time.Second,
				EntryTimeout: time.Second,
				DirectIO:     tc.directIO,
			})

			f, err := os.Open(filepath.Join(mountPoint, "f.dat"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = f.Close() }()

			before := mm.GetStats().Reads

			// Same offset twice on one descriptor, so nothing but the kernel's caching decision differs
			// between the two calls.
			buf := make([]byte, 512)
			for i := range 2 {
				if _, err := f.ReadAt(buf, 0); err != nil {
					t.Fatalf("ReadAt #%d: %v", i+1, err)
				}
			}

			// An upper bound as well as a lower one. Direct I/O with no bound would pass on a
			// filesystem that issued a READ per page for reasons unrelated to the flag, and the
			// non-direct case is the one that actually pins the behavior: exactly one.
			got := mm.GetStats().Reads - before
			if got < tc.wantAtLeast || got > tc.wantAtMost {
				t.Errorf("two read(2) calls at the same offset produced %d READ requests, want between "+
					"%d and %d: %s", got, tc.wantAtLeast, tc.wantAtMost, tc.why)
			}
		})
	}
}

// TestKeepCacheSurvivesReopen is the kernel half of FOPEN_KEEP_CACHE.
//
// Read, close, reopen, read the same offset. Without the flag the kernel drops the file's cached pages
// at open(2) and the second read reaches the filesystem; with it, the pages survive and the filesystem
// sees nothing. As with direct I/O, the count of arriving READs is the observable.
func TestKeepCacheSurvivesReopen(t *testing.T) {
	tests := []struct {
		name      string
		keepCache bool
		wantReads int64
		why       string
	}{
		{
			name:      "keep_cache: the reopened read is served from the page cache",
			keepCache: true,
			wantReads: 0,
			why:       "the kernel was asked to keep the pages it already had across open(2)",
		},
		{
			name:      "without keep_cache: the reopen drops the cache",
			keepCache: false,
			wantReads: 1,
			why:       "the kernel invalidates a file's pages on open unless told otherwise",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mountPoint, mm := liveMount(t, &MountOptions{
				FSName:       "objectfs",
				MaxWrite:     128 * 1024,
				AttrTimeout:  time.Second,
				EntryTimeout: time.Second,
				KeepCache:    tc.keepCache,
			})

			path := filepath.Join(mountPoint, "f.dat")
			buf := make([]byte, 512)

			first, err := os.Open(path)
			if err != nil {
				t.Fatalf("first open: %v", err)
			}
			if _, err := first.ReadAt(buf, 0); err != nil {
				t.Fatalf("first read: %v", err)
			}
			if err := first.Close(); err != nil {
				t.Fatalf("first close: %v", err)
			}

			// Counted from after the first read, so what is measured is only what the reopen costs.
			before := mm.GetStats().Reads

			second, err := os.Open(path)
			if err != nil {
				t.Fatalf("second open: %v", err)
			}
			defer func() { _ = second.Close() }()

			if _, err := second.ReadAt(buf, 0); err != nil {
				t.Fatalf("second read: %v", err)
			}

			if got := mm.GetStats().Reads - before; got != tc.wantReads {
				t.Errorf("a read after reopen produced %d READ requests, want %d: %s",
					got, tc.wantReads, tc.why)
			}
		})
	}
}
