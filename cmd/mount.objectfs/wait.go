package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// terminateGrace is how long a timed-out child gets to unwind after SIGTERM before it is killed.
//
// `objectfs mount` handles SIGTERM to unmount and flush, which is why it is sent first: a mount that
// timed out may still have dirty bytes it is trying to write, and SIGKILL on a filesystem process
// discards them. Five seconds, then SIGKILL, because a helper that never returns is the failure mode
// this whole program exists to avoid.
const terminateGrace = 5 * time.Second

// logTailBytes bounds how much of the child's startup output is relayed when a mount fails.
//
// From the end, because that is where the failure is. The whole file stays on disk either way, and the
// relayed tail names its path.
const logTailBytes = 16 * 1024

// startAndWait runs `objectfs mount` as a detached child and returns a mount(8) exit code once the mount
// point is live, the child has exited, or the timeout has passed.
func startAndWait(binary string, p plan, inv invocation, stderr io.Writer) int {
	before, err := readMountState(inv.mountPoint)
	if err != nil {
		emit(stderr, "mount.objectfs: cannot mount on %s: %v\n", inv.mountPoint, err)

		return exitMountFailed
	}

	logFile, logName, offset := openMountLog(inv.mountPoint, stderr)
	defer func() { _ = logFile.Close() }()

	// exec.Command and not exec.CommandContext, which is the one place in this repository where that is
	// deliberate. CommandContext kills the child when the context is done, and this child is the mount: it
	// has to outlive this process by design, so there is no context whose lifetime it should share. The
	// deadline that does apply is enforced below by signaling the process group, which stops the child
	// without tying it to a context that ends when the helper exits.
	//
	// #nosec G204 -- binary is resolved by objectfsPath from a fixed set of paths, and p.argv is built by
	// translate from optionTable: an option's value reaches this as one element of argv, never as shell
	// text, and an option's name never reaches it at all.
	cmd := exec.Command(binary, p.argv...) //nolint:noctx // the child must outlive this process; see above
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Env = os.Environ()

	// A session of its own, so the mount outlives this helper. mount(8) is itself a child of whatever ran
	// `mount -a`, and without Setsid the mount would share that process group and take the SIGHUP or
	// SIGINT sent to it — which at boot means the mount dies with the unit that started it, and at a
	// terminal means Ctrl-C on an unrelated command unmounts the filesystem.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		emit(stderr, "mount.objectfs: cannot run %s: %v\n", binary, err)

		return exitMountFailed
	}

	// Reaped in a goroutine rather than waited on directly, because the success case is a child that is
	// still running: `objectfs mount` returns when the filesystem is unmounted, so a helper that called
	// Wait inline would block for the lifetime of the mount, which is the exact defect this design
	// exists to avoid.
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	deadline := time.NewTimer(p.timeout)
	defer deadline.Stop()

	poll := time.NewTicker(pollInterval)
	defer poll.Stop()

	for {
		select {
		case waitErr := <-waited:
			// The child exited before the mount appeared. It cannot have exited *because* the mount
			// succeeded — a successful mount is what keeps it running — so this is a failure however it
			// exited, and the reason is in its output rather than in its status.
			emit(stderr, "mount.objectfs: %s exited before %s was mounted: %v\n",
				binary, inv.mountPoint, describeExit(waitErr))
			relayLog(stderr, logFile, logName, offset)

			return exitMountFailed

		case <-poll.C:
			live, err := mountAppeared(inv.mountPoint, before)
			if err != nil {
				// Not fatal on its own: the mount point can be briefly unstattable while a FUSE mount is
				// being established. Keep polling and let the deadline decide.
				continue
			}
			if live {
				if inv.verbose {
					emit(stderr, "mount.objectfs: mounted %s on %s (pid %d, log %s)\n",
						inv.device, inv.mountPoint, cmd.Process.Pid, logName)
				}

				return exitOK
			}

		case <-deadline.C:
			// Killed rather than left running. A helper that reported failure while the mount quietly
			// came up a moment later would leave `mount -a` believing the filesystem is absent and the
			// kernel believing it is present, and the next `mount -a` would stack a second one on top.
			emit(stderr, "mount.objectfs: %s did not mount on %s within %s\n",
				inv.device, inv.mountPoint, p.timeout)
			terminate(cmd, waited)
			relayLog(stderr, logFile, logName, offset)
			emit(stderr, "mount.objectfs: raise the wait with -o mount-timeout=<duration> if the mount "+
				"is only slow\n")

			return exitMountFailed
		}
	}
}

// terminate stops a timed-out child, giving it a chance to unwind first.
//
// The signal goes to the process group, negative pid, because Setsid made the child a group leader and
// `objectfs mount` is not necessarily the only process in it — a fusermount3 helper it spawned holds the
// /dev/fuse fd, and signaling only the parent can leave the mount point wedged.
func terminate(cmd *exec.Cmd, waited <-chan error) {
	pgid := -cmd.Process.Pid

	_ = syscall.Kill(pgid, syscall.SIGTERM)

	select {
	case <-waited:
		return
	case <-time.After(terminateGrace):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	}
}

// describeExit turns exec's wait error into something an operator can read.
func describeExit(err error) string {
	if err == nil {
		return "exited successfully, which for a mount means it gave up before mounting"
	}

	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return fmt.Sprintf("exit status %d", exit.ExitCode())
	}

	return err.Error()
}

// mountState is what a mount point looks like before the mount, so that its appearing can be recognized.
type mountState struct {
	// entries is how many /proc/self/mountinfo lines already name this path. Counted rather than tested
	// for zero because mounts stack: mounting onto an existing mount point is legal, and a helper that
	// treated "something is mounted here" as success would report a mount it never made.
	entries int

	// dev is the mount point's st_dev, the fallback signal where /proc/self/mountinfo is not there.
	dev uint64

	// haveMountinfo records which of the two signals is in use, so that a /proc that disappears mid-poll
	// does not silently downgrade the check.
	haveMountinfo bool
}

// readMountState reads the mount point's state before the child starts.
//
// It also refuses the invocation here, rather than leaving it to `objectfs mount`, when the mount point
// is not a directory: a helper that forked a child to discover a typo in fstab reports the typo 60
// seconds later and through a log file.
func readMountState(mountPoint string) (mountState, error) {
	var s mountState

	info, err := os.Stat(mountPoint)
	if err != nil {
		return s, fmt.Errorf("mount point cannot be read: %w", err)
	}
	if !info.IsDir() {
		return s, errors.New("mount point is not a directory")
	}

	s.dev, err = deviceOf(mountPoint)
	if err != nil {
		return s, err
	}

	if entries, err := countMountinfoEntries(mountinfoPath, mountPoint); err == nil {
		s.entries, s.haveMountinfo = entries, true
	}

	return s, nil
}

// mountAppeared reports whether a new mount has been made on the mount point since before was taken.
func mountAppeared(mountPoint string, before mountState) (bool, error) {
	if before.haveMountinfo {
		entries, err := countMountinfoEntries(mountinfoPath, mountPoint)
		if err != nil {
			return false, err
		}

		return entries > before.entries, nil
	}

	// Without a mount table, the signal is that the path's device number changed: a mount replaces the
	// directory's inode with the root of another filesystem, which carries a different st_dev. Weaker
	// than the mount table — it cannot say *what* was mounted — but it cannot report a mount that did not
	// happen either, which is the property that matters here.
	dev, err := deviceOf(mountPoint)
	if err != nil {
		return false, err
	}

	return dev != before.dev, nil
}

// mountinfoPath is the kernel's mount table. A variable so a test can point it at a fixture.
var mountinfoPath = "/proc/self/mountinfo"

// deviceOf returns a path's st_dev.
func deviceOf(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, fmt.Errorf("cannot stat %s: %w", path, err)
	}

	// The conversion is a no-op on linux, where st.Dev is already uint64, and a widening one on darwin,
	// where it is int32 — so it cannot be written to satisfy both platforms at once, and which linter
	// objects depends on which one the lint ran under. CI lints GOOS=linux and sees a redundant
	// conversion; a darwin run sees a signed widening. Suppressing both is cheaper than a two-file
	// platform split for one expression, and this value is only ever compared against another result of
	// this same function, so its representation is what matters and its value is not.
	//
	// #nosec G115 -- signed widening on darwin; see above.
	return uint64(st.Dev), nil //nolint:unconvert // a no-op on linux only; see above
}

// countMountinfoEntries counts the lines of a mountinfo file whose mount point is target.
func countMountinfoEntries(path, target string) (int, error) {
	f, err := os.Open(path) // #nosec G304 -- mountinfoPath is a package constant in production; a test
	// substitutes a fixture path.
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	return countMountinfo(f, target)
}

// countMountinfo counts the entries in a /proc/*/mountinfo stream whose mount point is target.
//
// Split out as a pure function over an io.Reader so it is testable from a fixture, on a platform that
// has no /proc at all. The format is fixed by Documentation/filesystems/proc.rst:
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//	                       ^^^^^ field 5, the mount point
//
// with space, tab, newline and backslash in a path written as the octal escapes \040 \011 \012 \134 —
// which have to be decoded, because a mount point with a space in it reaches this program decoded and
// would otherwise never match its own entry.
func countMountinfo(r io.Reader, target string) (int, error) {
	target = filepath.Clean(target)

	var count int

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}

		if filepath.Clean(unescapeOctal(fields[4])) == target {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}

	return count, nil
}

// unescapeOctal decodes the four backslash escapes the kernel uses in a mountinfo path.
//
// Only those four, and only as exactly three octal digits, because that is all the kernel emits
// (fs/proc_namespace.c passes " \t\n\\" to seq_path). A general octal decoder would also rewrite a
// literal backslash sequence that the kernel had not escaped, turning a path that matched into one that
// does not.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) {
			switch s[i+1 : i+4] {
			case "040":
				b.WriteByte(' ')
				i += 4

				continue
			case "011":
				b.WriteByte('\t')
				i += 4

				continue
			case "012":
				b.WriteByte('\n')
				i += 4

				continue
			case "134":
				b.WriteByte('\\')
				i += 4

				continue
			}
		}

		b.WriteByte(s[i])
		i++
	}

	return b.String()
}

// logDir is where a mount's startup output is kept. A variable so a test can redirect it.
var logDir = "/var/log/objectfs"

// openMountLog opens the file the child's stdout and stderr go to, and returns it, its name, and the
// offset to relay from.
//
// A named, appended file rather than a pipe or an unlinked temporary, because the child keeps writing to
// it for the lifetime of the mount — long after this helper has exited. A pipe would fill and block the
// filesystem on its own log, and an unlinked file would grow where nobody could find it or rotate it.
// The offset is taken so that a failure relays this mount's output and not every previous attempt's.
//
// It never fails the mount: a filesystem that would not come up because its log could not be opened is a
// worse outcome than one that comes up with no log.
func openMountLog(mountPoint string, stderr io.Writer) (*os.File, string, int64) {
	discard := func(reason string) (*os.File, string, int64) {
		emit(stderr, "mount.objectfs: warning: %s, so startup output will be discarded; run "+
			"`objectfs mount` by hand to see it\n", reason)

		// os.DevNull rather than a nil *os.File: exec.Cmd with a nil Stdout opens /dev/null anyway, but
		// this function's callers write to the result.
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return nil, os.DevNull, 0
		}

		return f, os.DevNull, 0
	}

	dir := logDir
	if err := os.MkdirAll(dir, 0o750); err != nil {
		dir = os.TempDir()
	}

	name := filepath.Join(dir, "mount-"+sanitizePath(mountPoint)+".log")

	// 0o640, the ordinary mode for a file under /var/log: written by the mount, which is normally root, and
	// readable by the group an operator can be put in. 0600 would make every diagnostic this program exists
	// to preserve reachable only through sudo, and the file holds `objectfs mount`'s startup output — not a
	// credential, which comes from the AWS chain and is never echoed.
	//
	// #nosec G304,G302 -- the directory is fixed and the basename is sanitizePath of the mount point, which
	// is one path component with no separators; the mode is /var/log's convention, above.
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return discard(fmt.Sprintf("cannot write %s: %v", name, err))
	}

	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		offset = 0
	}

	return f, name, offset
}

// sanitizePath turns a mount point into one filename component.
func sanitizePath(mountPoint string) string {
	cleaned := strings.Trim(filepath.Clean(mountPoint), string(filepath.Separator))
	if cleaned == "" || cleaned == "." {
		return "root"
	}

	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_':
			return r
		default:
			return '-'
		}
	}, cleaned)
}

// relayLog copies what the child wrote since offset onto stderr, so that a failed fstab mount says
// something more than "exit status 1".
func relayLog(stderr io.Writer, logFile *os.File, name string, offset int64) {
	if logFile == nil || name == os.DevNull {
		return
	}

	// Reopened for reading: the handle the child inherited is write-only, and reading through it would
	// also move an offset the child is still appending at.
	f, err := os.Open(name) // #nosec G304 -- name is what openMountLog just built; see there.
	if err != nil {
		emit(stderr, "mount.objectfs: startup output is in %s\n", name)

		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return
	}

	from := offset
	if truncated := info.Size() - logTailBytes; truncated > from {
		from = truncated
		emit(stderr, "mount.objectfs: last %d bytes of %s:\n", logTailBytes, name)
	} else if info.Size() > from {
		emit(stderr, "mount.objectfs: from %s:\n", name)
	} else {
		emit(stderr, "mount.objectfs: the mount wrote nothing to %s\n", name)

		return
	}

	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		emit(stderr, "  %s\n", scanner.Text())
	}
}
