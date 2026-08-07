//go:build linux || darwin

package fuse

// These tests cover the three kernel-facing settings #180 asked for: direct I/O, the page-cache
// retention flag, and asynchronous reads.
//
// Every assertion here is on a value the kernel receives, not on a field having been copied. That
// distinction is the whole point of the issue: nine fields with names matching real FUSE capabilities
// survived to a release because a test asserting `opts.DirectIO == cfg.DirectIO` passes just as well
// when nothing downstream reads the field. So:
//
//   - the open-time flags are read out of a real *fuse.OpenOut, filled by go-fuse's own bridge from a
//     real OPEN request, which is byte-for-byte what the kernel gets back;
//   - the mount-time flag is read out of the *fs.Options that go-fuse is handed at mount, and then
//     out of the DisabledCapabilities mask go-fuse itself derives from it — so the assertion is on
//     the capability being withheld from the kernel, not on the intermediate boolean.
//
// What is deliberately not asserted here, and cannot be: that a second read(2) of the same offset
// reaches FileHandle.Read under direct I/O. That is the kernel's half of the contract and it needs a
// real mount; there is no /dev/fuse in CI and none on the macOS development host. It belongs in the
// live-mount smoke suite. What these tests pin is everything up to the kernel boundary, which is
// where all nine of the removed fields failed.

import (
	"context"
	"testing"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// openThroughBridge issues one OPEN through go-fuse's raw bridge and returns what the kernel would
// receive, which is the OpenOut the bridge filled.
//
// Going through the bridge rather than calling FileNode.Open directly is what makes this a seam test.
// FileNode.Open returns the flags as its second result and the bridge is what copies them into
// OpenOut.OpenFlags (fs/bridge.go:756) — a return value nothing forwards would be exactly the defect
// class under test, so the forwarding is included rather than assumed.
func openThroughBridge(t *testing.T, fsys *FileSystem, key string) *fuse.OpenOut {
	t.Helper()

	sec := time.Second
	raw := gofs.NewNodeFS(fsys.Root(), &gofs.Options{
		AttrTimeout:  &sec,
		EntryTimeout: &sec,
	})

	// Node id 1 is the root, per go-fuse's bridge. Look the file up first so the bridge holds an
	// inode for it and can dispatch OPEN — the kernel does the same two calls in the same order.
	var entry fuse.EntryOut
	if status := raw.Lookup(nil, &fuse.InHeader{NodeId: 1}, key, &entry); !status.Ok() {
		t.Fatalf("Lookup(%q) through the bridge: %v", key, status)
	}

	var out fuse.OpenOut
	if status := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: entry.NodeId}}, &out); !status.Ok() {
		t.Fatalf("Open(%q) through the bridge: %v", key, status)
	}

	return &out
}

// newOptionsFixture is a FileSystem over a real S3 endpoint and a real write path, configured by the
// caller. No mocks: mount options are a seam, and a mock on the far side of one agrees by
// construction.
func newOptionsFixture(t *testing.T, configure func(*Config)) (*FileSystem, *testaws.TestServer) {
	t.Helper()

	srv := testaws.Start(t)

	writer, err := vfs.NewWriter(context.Background(), srv.Backend())
	if err != nil {
		t.Fatalf("vfs.NewWriter: %v", err)
	}

	cfg := &Config{
		DefaultMode:    0o644,
		DefaultDirMode: 0o755,
		DefaultUID:     1000,
		DefaultGID:     1000,
		CacheTTL:       time.Minute,
	}
	if configure != nil {
		configure(cfg)
	}

	return NewFileSystem(t.Context(), srv.Backend(), nil, writer, nil, cfg), srv
}

// TestOpenFlagsReachTheKernel is the assertion the nine removed fields never had.
//
// DirectIO and KeepCache existed on Config through v0.10.0 while FileNode.Open returned a literal 0
// for its fuseFlags, so both were settable and neither reached the kernel. Mutating openFlags to
// return 0 unconditionally — which is exactly what v0.10.0 did — fails the first two subtests.
func TestOpenFlagsReachTheKernel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		directIO  bool
		keepCache bool
		want      uint32
		why       string
	}{
		{
			name:     "direct_io sets FOPEN_DIRECT_IO",
			directIO: true,
			want:     fuse.FOPEN_DIRECT_IO,
			why:      "the kernel must be told not to cache this mount's data",
		},
		{
			name:      "keep_cache sets FOPEN_KEEP_CACHE",
			keepCache: true,
			want:      fuse.FOPEN_KEEP_CACHE,
			why:       "the kernel must be told it may keep cached pages across open(2)",
		},
		{
			name:      "direct_io wins over keep_cache",
			directIO:  true,
			keepCache: true,
			want:      fuse.FOPEN_DIRECT_IO,
			why: "the two ask for opposite things; sending both would leave which one applies to a " +
				"kernel version rather than to this configuration",
		},
		{
			name: "neither set sends no flags",
			want: 0,
			why:  "the default must be the kernel's own behavior, which is what a zero value means",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fsys, srv := newOptionsFixture(t, func(c *Config) {
				c.DirectIO = tc.directIO
				c.KeepCache = tc.keepCache
			})
			srv.PutObject("f.dat", []byte("contents"))

			out := openThroughBridge(t, fsys, "f.dat")

			if out.OpenFlags != tc.want {
				t.Errorf("OpenOut.OpenFlags = %#x, want %#x: %s", out.OpenFlags, tc.want, tc.why)
			}
		})
	}
}

// TestCreateReturnsTheSameOpenFlagsAsOpen pins the second path that hands flags to the kernel.
//
// A create is an open too — CREATE returns an OpenOut and the kernel caches whatever flags come back
// with it — so a direct-io mount whose Create returned 0 would cache the data of every file written
// through it while honoring the setting for every file merely read. Create delegates to Open for
// exactly this reason; this test is what would notice if it stopped.
func TestCreateReturnsTheSameOpenFlagsAsOpen(t *testing.T) {
	t.Parallel()

	fsys, _ := newOptionsFixture(t, func(c *Config) { c.DirectIO = true })

	sec := time.Second
	raw := gofs.NewNodeFS(fsys.Root(), &gofs.Options{AttrTimeout: &sec, EntryTimeout: &sec})

	var out fuse.CreateOut
	status := raw.Create(nil, &fuse.CreateIn{
		InHeader: fuse.InHeader{NodeId: 1},
		Mode:     0o644,
	}, "made.dat", &out)
	if !status.Ok() {
		t.Fatalf("Create through the bridge: %v", status)
	}

	// out.OpenFlags is CreateOut's embedded OpenOut's field. CreateOut embeds both EntryOut and
	// OpenOut, and only OpenOut carries OpenFlags, so this is the same field rawBridge.Create writes.
	if out.OpenFlags != fuse.FOPEN_DIRECT_IO {
		t.Errorf("CreateOut.OpenFlags = %#x, want FOPEN_DIRECT_IO (%#x): a file created on a "+
			"direct-io mount would have its data cached by the kernel",
			out.OpenFlags, fuse.FOPEN_DIRECT_IO)
	}
}

// TestMountOptionsReachTheOpenFlags covers the middle seam, which is the one that was empty.
//
// The kernel-facing flags are produced by FileSystem.openFlags from Config, but Config is not what a
// caller sets — internal/adapter builds a MountOptions and CreatePlatformMountManager derives the
// Config from it. That derivation is where nine fields died: it hardcoded uid, gid and mode for a whole
// release and discarded MountConfig.Permissions entirely, and DirectIO and KeepCache were on Config
// with nothing assigning them.
//
// So this starts where an operator's configuration arrives and asserts what the kernel receives, with
// no intermediate value trusted along the way.
func TestMountOptionsReachTheOpenFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts MountOptions
		want uint32
	}{
		{
			name: "direct_io",
			opts: MountOptions{DirectIO: true},
			want: fuse.FOPEN_DIRECT_IO,
		},
		{
			name: "keep_cache",
			opts: MountOptions{KeepCache: true},
			want: fuse.FOPEN_KEEP_CACHE,
		},
		{
			name: "neither",
			opts: MountOptions{},
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := testaws.Start(t)
			srv.PutObject("f.dat", []byte("contents"))

			writer, err := vfs.NewWriter(context.Background(), srv.Backend())
			if err != nil {
				t.Fatalf("vfs.NewWriter: %v", err)
			}

			opts := tc.opts
			opts.FSName = "objectfs"
			opts.MaxWrite = 128 * 1024

			pfs := CreatePlatformMountManager(t.Context(), srv.Backend(), nil, writer, nil, &MountConfig{
				MountPoint: t.TempDir(),
				Options:    &opts,
			})

			// The manager holds the filesystem it built; this test is in-package so it can ask. Nothing
			// is mounted — the assertion is on what an OPEN would return, and openThroughBridge issues a
			// real one through go-fuse's bridge without a kernel.
			mm, ok := pfs.(*MountManager)
			if !ok {
				t.Fatalf("CreatePlatformMountManager returned %T, want *MountManager", pfs)
			}

			out := openThroughBridge(t, mm.filesystem, "f.dat")

			if out.OpenFlags != tc.want {
				t.Errorf("OpenOut.OpenFlags = %#x, want %#x: the flag was set on MountOptions and did "+
					"not reach the kernel", out.OpenFlags, tc.want)
			}
		})
	}
}

// TestSyncReadReachesGoFuse is the one assertion in this file that is a copy check, and the reason is
// worth recording because #180 explicitly rejects copy checks.
//
// SyncRead's effect is a capability withheld from the kernel at INIT: go-fuse ORs CAP_ASYNC_READ into
// DisabledCapabilities in MountOptions.setDefaults (fuse/server.go:187). That method is unexported and
// NewServer runs it on a private copy of the options, so the derived mask is not reachable from a test
// without a mount — and Server.KernelSettings, the one exported window onto INIT, returns the kernel's
// *request*, not the server's reply, so it does not show it either.
//
// The effect assertion therefore lives in kernel_options_live_test.go, which mounts and reads INIT
// through the debug log. This test covers the remaining half: that the value arrives at the field
// go-fuse derives the mask from. It is not sufficient on its own, which is exactly why the live test
// exists — a mount is what tells the kernel, and the kernel is what has to be told.
func TestSyncReadReachesGoFuse(t *testing.T) {
	t.Parallel()

	for _, syncRead := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[syncRead], func(t *testing.T) {
			t.Parallel()

			mm := NewMountManager(nil, &MountConfig{
				MountPoint: t.TempDir(),
				Options: &MountOptions{
					FSName:   "objectfs",
					MaxWrite: 128 * 1024,
					SyncRead: syncRead,
				},
			})

			if got := mm.buildFUSEOptions().SyncRead; got != syncRead {
				t.Errorf("fuse.MountOptions.SyncRead = %v, want %v", got, syncRead)
			}
		})
	}
}
