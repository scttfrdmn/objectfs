package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A real /proc/self/mountinfo excerpt, including the two cases a naive parser gets wrong: a mount point
// with a space in it, written by the kernel as \040, and the same path mounted twice.
const mountinfoFixture = `23 28 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
25 0 254:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw
36 25 0:32 / /mnt/my\040data rw,relatime shared:29 - fuse.objectfs objectfs rw,user_id=0,group_id=0
37 25 0:33 / /mnt/data rw,relatime shared:30 - fuse.objectfs objectfs rw,user_id=0,group_id=0
38 25 0:34 / /mnt/data rw,relatime shared:31 - fuse.objectfs objectfs rw,user_id=0,group_id=0
39 25 0:35 / /mnt/tab\011bed rw,relatime shared:32 - fuse.objectfs objectfs rw
short line
`

func TestCountMountinfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
		want   int
	}{
		{name: "not mounted", target: "/mnt/absent", want: 0},
		{name: "mounted once", target: "/proc", want: 1},
		{
			// Mounts stack, and the count is what makes a helper able to tell "this mount appeared" from
			// "something was already here".
			name:   "mounted twice",
			target: "/mnt/data",
			want:   2,
		},
		{
			// The path reaches this program decoded, so a parser that did not decode \040 would never
			// match its own entry and would report the mount as never appearing.
			name:   "a space, which the kernel writes as an octal escape",
			target: "/mnt/my data",
			want:   1,
		},
		{name: "a tab", target: "/mnt/tab\tbed", want: 1},
		{name: "trailing slash is the same mount point", target: "/mnt/data/", want: 2},
		{
			// The device field also holds paths. Matching on it instead of field 5 would count the wrong
			// lines, and / is where that shows up.
			name:   "the root mount matches once, not once per device path",
			target: "/",
			want:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := countMountinfo(strings.NewReader(mountinfoFixture), tt.target)
			if err != nil {
				t.Fatalf("countMountinfo: %v", err)
			}
			if got != tt.want {
				t.Errorf("countMountinfo(%q) = %d, want %d", tt.target, got, tt.want)
			}
		})
	}
}

func TestUnescapeOctal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{"/mnt/data", "/mnt/data"},
		{`/mnt/my\040data`, "/mnt/my data"},
		{`/mnt/a\011b`, "/mnt/a\tb"},
		{`/mnt/a\012b`, "/mnt/a\nb"},
		{`/mnt/a\134b`, `/mnt/a\b`},
		{`/mnt/\040\040`, "/mnt/  "},
		{
			// Only the four the kernel emits. A general octal decoder would rewrite this, turning a path
			// that matches into one that does not.
			in:   `/mnt/a\101b`,
			want: `/mnt/a\101b`,
		},
		{
			// A trailing backslash with too few digits after it is left alone rather than read past the
			// end of the string.
			in:   `/mnt/a\04`,
			want: `/mnt/a\04`,
		},
		{in: `\`, want: `\`},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			if got := unescapeOctal(tt.in); got != tt.want {
				t.Errorf("unescapeOctal(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{"/mnt/data", "mnt-data"},
		{"/mnt/my data", "mnt-my-data"},
		{"/", "root"},
		{"", "root"},
		{"/scratch/user_01/set.2", "scratch-user_01-set.2"},
		{
			// The whole point: whatever comes in, what comes out is one path component. A mount point that
			// produced a separator here would write the log somewhere other than the log directory.
			in:   "/a/../../etc/passwd",
			want: "etc-passwd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got := sanitizePath(tt.in)
			if got != tt.want {
				t.Errorf("sanitizePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.ContainsRune(got, filepath.Separator) {
				t.Errorf("sanitizePath(%q) = %q, which contains a path separator", tt.in, got)
			}
		})
	}
}

func TestReadMountState(t *testing.T) {
	t.Parallel()

	t.Run("a directory", func(t *testing.T) {
		t.Parallel()

		s, err := readMountState(t.TempDir())
		if err != nil {
			t.Fatalf("readMountState: %v", err)
		}
		if s.dev == 0 {
			t.Error("dev = 0, want the mount point's device number")
		}
	})

	t.Run("a missing mount point", func(t *testing.T) {
		t.Parallel()

		_, err := readMountState(filepath.Join(t.TempDir(), "absent"))
		if err == nil {
			t.Fatal("readMountState succeeded on a missing mount point")
		}
		if !strings.Contains(err.Error(), "cannot be read") {
			t.Errorf("error = %q, want it to say the mount point cannot be read", err)
		}
	})

	t.Run("a file, which is the fstab typo worth catching early", func(t *testing.T) {
		t.Parallel()

		file := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := readMountState(file)
		if err == nil {
			t.Fatal("readMountState succeeded on a file")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("error = %q, want it to say the mount point is not a directory", err)
		}
	})
}

func TestMountAppeared(t *testing.T) {
	table := filepath.Join(t.TempDir(), "mountinfo")
	mountPoint := t.TempDir()
	withMountinfo(t, table)

	writeTable := func(lines string) {
		if err := os.WriteFile(table, []byte(lines), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeTable("")

	before, err := readMountState(mountPoint)
	if err != nil {
		t.Fatalf("readMountState: %v", err)
	}
	if !before.haveMountinfo {
		t.Fatal("haveMountinfo = false with a readable mount table")
	}

	if appeared, err := mountAppeared(mountPoint, before); err != nil || appeared {
		t.Errorf("mountAppeared before anything was mounted = %v (err %v), want false", appeared, err)
	}

	writeTable("36 25 0:32 / " + mountPoint + " rw,relatime shared:29 - fuse.objectfs objectfs rw\n")

	if appeared, err := mountAppeared(mountPoint, before); err != nil || !appeared {
		t.Errorf("mountAppeared after the mount = %v (err %v), want true", appeared, err)
	}
}

// TestMountAppearedIgnoresAMountThatWasAlreadyThere is the reason mountState counts entries rather than
// testing for zero.
//
// Mounting onto a path that is already a mount point is legal, so a helper that read "something is mounted
// here" as success would report a mount it never made — and at boot, `mount -a` would record a filesystem
// as present that nothing had mounted.
func TestMountAppearedIgnoresAMountThatWasAlreadyThere(t *testing.T) {
	table := filepath.Join(t.TempDir(), "mountinfo")
	mountPoint := t.TempDir()
	withMountinfo(t, table)

	existing := "36 25 0:32 / " + mountPoint + " rw,relatime shared:29 - ext4 /dev/vdb rw\n"
	if err := os.WriteFile(table, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	before, err := readMountState(mountPoint)
	if err != nil {
		t.Fatalf("readMountState: %v", err)
	}
	if before.entries != 1 {
		t.Fatalf("entries = %d, want 1", before.entries)
	}

	if appeared, err := mountAppeared(mountPoint, before); err != nil || appeared {
		t.Errorf("mountAppeared = %v (err %v), want false: the mount that is there is not the one this "+
			"helper made", appeared, err)
	}
}

// TestMountAppearedFallsBackToTheDeviceNumber covers the path taken where there is no /proc: the mount
// table is unreadable, so the signal is the mount point's st_dev changing.
func TestMountAppearedFallsBackToTheDeviceNumber(t *testing.T) {
	mountPoint := t.TempDir()
	withMountinfo(t, filepath.Join(t.TempDir(), "there-is-no-mount-table-here"))

	before, err := readMountState(mountPoint)
	if err != nil {
		t.Fatalf("readMountState: %v", err)
	}
	if before.haveMountinfo {
		t.Fatal("haveMountinfo = true with no mount table")
	}

	// Nothing has been mounted and the device number has not changed, so the weaker signal has to say so
	// rather than defaulting to true — a fallback that reported success unconditionally would make every
	// mount look fine on a platform without /proc.
	if appeared, err := mountAppeared(mountPoint, before); err != nil || appeared {
		t.Errorf("mountAppeared = %v (err %v), want false", appeared, err)
	}

	// A different device is what a mount looks like to this check. /tmp and a temp directory are not
	// reliably on different filesystems, so the comparison is made against the recorded baseline directly.
	before.dev++
	if appeared, err := mountAppeared(mountPoint, before); err != nil || !appeared {
		t.Errorf("mountAppeared with a changed device number = %v (err %v), want true", appeared, err)
	}
}

func TestStartAndWait(t *testing.T) {
	t.Run("the mount comes up", func(t *testing.T) {
		table := filepath.Join(t.TempDir(), "mountinfo")
		mountPoint := t.TempDir()
		pidFile := filepath.Join(t.TempDir(), "pid")
		withMountinfo(t, table)
		withLogDir(t, t.TempDir())

		if err := os.WriteFile(table, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		// A stand-in for `objectfs mount`: it registers the mount and then blocks for its lifetime, which
		// is what makes waiting-for-the-mount rather than waiting-for-the-process the right design.
		binary := fakeObjectfs(t, `
			echo $$ > `+pidFile+`
			echo "mounting" >&2
			echo "36 25 0:32 / `+mountPoint+` rw - fuse.objectfs objectfs rw" >> `+table+`
			sleep 30`)
		t.Cleanup(func() { killGroupFromFile(t, pidFile) })

		var stderr strings.Builder
		code := startAndWait(binary, plan{argv: []string{"mount"}, timeout: 10 * time.Second},
			invocation{device: "s3://bucket", mountPoint: mountPoint, verbose: true}, &stderr)

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d; stderr: %s", code, exitOK, stderr.String())
		}
		if !strings.Contains(stderr.String(), "mounted s3://bucket on "+mountPoint) {
			t.Errorf("stderr = %q, want it to report the mount under -v", stderr.String())
		}

		// The child outlives the helper. That is the whole design: `objectfs mount` runs for the lifetime
		// of the filesystem, and a helper that took it down on exit would unmount what it just mounted.
		if _, err := os.Stat(pidFile); err != nil {
			t.Fatalf("the child never started: %v", err)
		}
		if !processAlive(t, pidFile) {
			t.Error("the child exited when the helper returned; the mount would not survive")
		}
	})

	t.Run("the child exits before the mount appears", func(t *testing.T) {
		table := filepath.Join(t.TempDir(), "mountinfo")
		mountPoint := t.TempDir()
		logs := t.TempDir()
		withMountinfo(t, table)
		withLogDir(t, logs)

		if err := os.WriteFile(table, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		binary := fakeObjectfs(t, `echo "AccessDenied: the credentials cannot list the bucket" >&2; exit 1`)

		var stderr strings.Builder
		code := startAndWait(binary, plan{argv: []string{"mount"}, timeout: 10 * time.Second},
			invocation{device: "s3://bucket", mountPoint: mountPoint}, &stderr)

		if code != exitMountFailed {
			t.Fatalf("exit code = %d, want %d", code, exitMountFailed)
		}

		got := stderr.String()
		if !strings.Contains(got, "exited before "+mountPoint+" was mounted") {
			t.Errorf("stderr = %q, want it to say the child exited first", got)
		}
		if !strings.Contains(got, "exit status 1") {
			t.Errorf("stderr = %q, want it to report the exit status", got)
		}
		// The reason a log file exists at all: without the relay, a failed fstab mount reports an exit
		// status and nothing about why, which sends an operator to check the network.
		if !strings.Contains(got, "AccessDenied") {
			t.Errorf("stderr = %q, want it to relay what the child said", got)
		}

		logName := filepath.Join(logs, "mount-"+sanitizePath(mountPoint)+".log")
		if _, err := os.Stat(logName); err != nil {
			t.Errorf("the log was not left behind at %s: %v", logName, err)
		}
		if !strings.Contains(got, logName) {
			t.Errorf("stderr = %q, want it to name the log file", got)
		}
	})

	t.Run("the mount never comes up", func(t *testing.T) {
		table := filepath.Join(t.TempDir(), "mountinfo")
		mountPoint := t.TempDir()
		pidFile := filepath.Join(t.TempDir(), "pid")
		withMountinfo(t, table)
		withLogDir(t, t.TempDir())

		if err := os.WriteFile(table, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		// Alive, quiet, and never mounting: the case that would hang `mount -a` forever if the helper
		// waited on the process instead of on the mount.
		binary := fakeObjectfs(t, "echo $$ > "+pidFile+"; sleep 60")
		t.Cleanup(func() { killGroupFromFile(t, pidFile) })

		var stderr strings.Builder
		start := time.Now()
		code := startAndWait(binary, plan{argv: []string{"mount"}, timeout: 300 * time.Millisecond},
			invocation{device: "s3://bucket", mountPoint: mountPoint}, &stderr)
		elapsed := time.Since(start)

		if code != exitMountFailed {
			t.Fatalf("exit code = %d, want %d", code, exitMountFailed)
		}
		if elapsed > terminateGrace+5*time.Second {
			t.Errorf("took %s to give up on a 300ms timeout", elapsed)
		}

		got := stderr.String()
		if !strings.Contains(got, "did not mount on "+mountPoint+" within 300ms") {
			t.Errorf("stderr = %q, want it to report the timeout", got)
		}
		if !strings.Contains(got, "mount-timeout=") {
			t.Errorf("stderr = %q, want it to name the option that raises the wait", got)
		}

		// Killed, not abandoned. A helper that reported failure and left the mount coming up behind it
		// would leave mount(8) and the kernel disagreeing about whether the filesystem exists.
		deadline := time.Now().Add(5 * time.Second)
		for processAlive(t, pidFile) {
			if time.Now().After(deadline) {
				t.Fatal("the timed-out child is still running")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("a binary that cannot be executed", func(t *testing.T) {
		withMountinfo(t, filepath.Join(t.TempDir(), "mountinfo"))
		withLogDir(t, t.TempDir())

		notABinary := filepath.Join(t.TempDir(), "objectfs")
		if err := os.WriteFile(notABinary, []byte("\x00\x01not a program"), 0o700); err != nil {
			t.Fatal(err)
		}

		var stderr strings.Builder
		code := startAndWait(notABinary, plan{argv: []string{"mount"}, timeout: 2 * time.Second},
			invocation{device: "s3://bucket", mountPoint: t.TempDir()}, &stderr)

		if code != exitMountFailed {
			t.Errorf("exit code = %d, want %d", code, exitMountFailed)
		}
	})

	t.Run("a mount point that is not a directory fails before forking", func(t *testing.T) {
		withMountinfo(t, filepath.Join(t.TempDir(), "mountinfo"))
		withLogDir(t, t.TempDir())

		file := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		binary := fakeObjectfs(t, `echo "the mount should not have been attempted" >&2; sleep 30`)

		var stderr strings.Builder
		start := time.Now()
		code := startAndWait(binary, plan{argv: []string{"mount"}, timeout: time.Minute},
			invocation{device: "s3://bucket", mountPoint: file}, &stderr)

		if code != exitMountFailed {
			t.Errorf("exit code = %d, want %d", code, exitMountFailed)
		}
		// Immediately, rather than after the full timeout: the check that catches an fstab typo is worth
		// nothing if it reports a minute later and through a log file.
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("took %s to reject a mount point that is not a directory", elapsed)
		}
		if !strings.Contains(stderr.String(), "not a directory") {
			t.Errorf("stderr = %q, want it to say the mount point is not a directory", stderr.String())
		}
	})
}

func TestOpenMountLog(t *testing.T) {
	t.Run("the offset skips a previous attempt's output", func(t *testing.T) {
		logs := t.TempDir()
		withLogDir(t, logs)

		name := filepath.Join(logs, "mount-mnt-data.log")
		if err := os.WriteFile(name, []byte("output from an earlier mount\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		var warnings strings.Builder
		f, gotName, offset := openMountLog("/mnt/data", &warnings)
		defer func() { _ = f.Close() }()

		if gotName != name {
			t.Errorf("log name = %q, want %q", gotName, name)
		}
		if want := int64(len("output from an earlier mount\n")); offset != want {
			t.Errorf("offset = %d, want %d (the end of what was already there)", offset, want)
		}
		if warnings.String() != "" {
			t.Errorf("unexpected warning: %q", warnings.String())
		}

		if _, err := f.WriteString("this attempt\n"); err != nil {
			t.Fatal(err)
		}

		var stderr strings.Builder
		relayLog(&stderr, f, gotName, offset)

		got := stderr.String()
		if !strings.Contains(got, "this attempt") {
			t.Errorf("relayed output %q does not contain this attempt's line", got)
		}
		// The point of the offset: a failure should not relay every previous attempt, which for a mount
		// that has been failing since boot is the entire file.
		if strings.Contains(got, "earlier mount") {
			t.Errorf("relayed output %q includes a previous attempt's line", got)
		}
	})

	// /var/log is root's, and a mount can be made by a user through fstab's `user` option, so the log
	// directory not being creatable is the ordinary case rather than the exceptional one.
	t.Run("an unwritable log directory falls back to the temp directory", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root can create a directory anywhere")
		}

		unwritable := unwritableDir(t)
		withLogDir(t, filepath.Join(unwritable, "logs"))

		var warnings strings.Builder
		f, name, offset := openMountLog("/mnt/data", &warnings)
		defer func() { _ = f.Close() }()

		if want := filepath.Join(os.TempDir(), "mount-mnt-data.log"); name != want {
			t.Errorf("log name = %q, want %q", name, want)
		}
		t.Cleanup(func() { _ = os.Remove(name) })

		if offset < 0 {
			t.Errorf("offset = %d, want the end of the file", offset)
		}
		if warnings.String() != "" {
			t.Errorf("warning = %q, want none: falling back to the temp directory is the normal path",
				warnings.String())
		}
	})

	t.Run("nowhere to write at all warns and does not fail the mount", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root can create a directory anywhere")
		}

		// Both destinations unwritable: the configured directory and the temp directory it falls back to.
		unwritable := unwritableDir(t)
		withLogDir(t, filepath.Join(unwritable, "logs"))
		t.Setenv("TMPDIR", unwritable)

		var warnings strings.Builder
		f, name, offset := openMountLog("/mnt/data", &warnings)
		if f != nil {
			defer func() { _ = f.Close() }()
		}

		// A filesystem that would not come up because its log could not be opened is a worse outcome than
		// one that comes up with no log, so this is a warning and a usable writer.
		if f == nil {
			t.Fatal("openMountLog returned no writer; the mount would have nowhere to send its output")
		}
		if name != os.DevNull {
			t.Errorf("log name = %q, want %q", name, os.DevNull)
		}
		if offset != 0 {
			t.Errorf("offset = %d, want 0", offset)
		}
		if !strings.Contains(warnings.String(), "will be discarded") {
			t.Errorf("warning = %q, want it to say output will be discarded", warnings.String())
		}

		// relayLog has to be a no-op rather than an error when there is nothing to relay from.
		var stderr strings.Builder
		relayLog(&stderr, f, name, offset)
		if stderr.String() != "" {
			t.Errorf("relayLog wrote %q for a discarded log", stderr.String())
		}
	})
}

// unwritableDir returns a directory this process cannot create files or subdirectories in.
func unwritableDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	// Restored so that t.TempDir's own cleanup can remove it.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	return dir
}

func TestRelayLogTruncatesToTheTail(t *testing.T) {
	logs := t.TempDir()
	withLogDir(t, logs)

	var warnings strings.Builder
	f, name, offset := openMountLog("/mnt/data", &warnings)
	defer func() { _ = f.Close() }()

	// One marker at the start and one at the end, either side of more than the tail budget. The failure is
	// at the end, which is why the tail rather than the head is what gets relayed.
	if _, err := f.WriteString("FIRST\n" + strings.Repeat("filler line to pad the log out\n", 2000) + "LAST\n"); err != nil {
		t.Fatal(err)
	}

	var stderr strings.Builder
	relayLog(&stderr, f, name, offset)

	got := stderr.String()
	if !strings.Contains(got, "LAST") {
		t.Error("the relayed tail does not include the end of the log, which is where the failure is")
	}
	if strings.Contains(got, "FIRST") {
		t.Errorf("the relay included the start of a %d-byte log; it should be bounded to the last %d bytes",
			len(got), logTailBytes)
	}
	if !strings.Contains(got, "last "+strconv.Itoa(logTailBytes)+" bytes") {
		t.Errorf("stderr = %q, want it to say the output was truncated", firstLine(got))
	}
}

func TestDescribeExit(t *testing.T) {
	t.Parallel()

	// A child that exits 0 without mounting is still a failure, and saying "exit status 0" would read as
	// though the mount had worked.
	if got := describeExit(nil); !strings.Contains(got, "gave up before mounting") {
		t.Errorf("describeExit(nil) = %q, want it to explain that success without a mount is a failure", got)
	}
}

// withMountinfo points the mount-table reader at a path for the duration of one test.
func withMountinfo(t *testing.T, path string) {
	t.Helper()

	previous := mountinfoPath
	mountinfoPath = path
	t.Cleanup(func() { mountinfoPath = previous })
}

// withLogDir points the mount log directory at a path for the duration of one test.
func withLogDir(t *testing.T, path string) {
	t.Helper()

	previous := logDir
	logDir = path
	t.Cleanup(func() { logDir = previous })
}

// processAlive reports whether the pid in a file is still running.
func processAlive(t *testing.T, pidFile string) bool {
	t.Helper()

	pid, ok := readPid(pidFile)
	if !ok {
		return false
	}

	return syscall.Kill(pid, 0) == nil
}

// killGroupFromFile takes down a fake child's whole process group, so a test that leaves one sleeping does
// not leave it sleeping past the run.
func killGroupFromFile(t *testing.T, pidFile string) {
	t.Helper()

	if pid, ok := readPid(pidFile); ok {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func readPid(pidFile string) (int, bool) {
	raw, err := os.ReadFile(pidFile) // #nosec G304 -- a path this test built under t.TempDir.
	if err != nil {
		return 0, false
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}

	return pid, true
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}

	return s
}
