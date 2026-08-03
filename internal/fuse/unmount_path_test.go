//go:build linux || darwin

package fuse

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestUnmountPathRejectsAnEmptyPath covers the arm that runs no programs at all.
//
// It matters more than it looks: the mount point reaching UnmountPath comes from a systemd unit's
// ExecStop, where `objectfs unmount /mnt/objectfs/%i` with an unset %i expands to a bare directory or
// to nothing. Without this arm, filepath.Abs("") returns the working directory — so `unmount` with no
// argument would try to unmount whatever directory systemd happened to start in.
func TestUnmountPathRejectsAnEmptyPath(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"", " ", "\t", "\n"} {
		err := UnmountPath(context.Background(), path)
		if err == nil {
			t.Errorf("UnmountPath(%q) returned nil; an empty path is not a mount point, and "+
				"filepath.Abs would turn it into the caller's working directory", path)

			continue
		}

		if !strings.Contains(err.Error(), "no mount point given") {
			t.Errorf("UnmountPath(%q) = %v, want a message saying no mount point was given", path, err)
		}
	}
}

// TestUnmountPathOnSomethingNotMounted asserts what the failure says.
//
// Every candidate fails here, which is the interesting case rather than the boring one: this is the
// error an operator reads when `objectfs unmount` did not work, and the whole design of the function is
// about that message. It has to name the path, list what was tried and why each was tried, and say what
// usually causes it — because the alternative, which is what the first draft produced, is a bare exit
// status from a program the operator did not know was being run.
func TestUnmountPathOnSomethingNotMounted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := UnmountPath(context.Background(), dir)
	if err == nil {
		t.Fatalf("UnmountPath(%q) returned nil for a directory that is not a mount point; a caller "+
			"would report a successful unmount of something that was never mounted", dir)
	}

	msg := err.Error()

	if !strings.Contains(msg, dir) {
		t.Errorf("UnmountPath(%q) error does not name the path: %v", dir, err)
	}

	// Every candidate appears, with its reason. A list of exit statuses is not actionable; a list
	// saying "fusermount3: not installed" versus "fusermount3: refused" points at different problems.
	for _, c := range unmountCommands(dir) {
		if !strings.Contains(msg, c.name) {
			t.Errorf("UnmountPath error does not mention the %s attempt, so an operator cannot tell "+
				"whether it ran: %v", c.name, err)
		}
		if !strings.Contains(msg, c.why) {
			t.Errorf("UnmountPath error mentions %s without saying why it was tried: %v", c.name, err)
		}
	}

	// The syscall attempt is last and is reported too, because "this one needs root" is the answer for
	// the container case where no helper is installed.
	if !strings.Contains(msg, "umount(2)") {
		t.Errorf("UnmountPath error does not report the direct syscall attempt: %v", err)
	}

	if !strings.Contains(msg, "lsof") {
		t.Errorf("UnmountPath error does not name the command that finds what is holding the mount, "+
			"which is the next thing the operator needs: %v", err)
	}
}

// TestUnmountPathReportsTheAbsolutePath covers the filepath.Abs call.
//
// The kernel's mount table holds cleaned absolute paths, so `umount /mnt/x/` fails on some systems
// where `umount /mnt/x` succeeds — and a trailing slash is what shell tab-completion produces. The
// message has to name the resolved path rather than the one typed, or an operator comparing it against
// `mount` output is comparing two different strings.
func TestUnmountPathReportsTheAbsolutePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := UnmountPath(context.Background(), dir+string(filepath.Separator))
	if err == nil {
		t.Fatalf("UnmountPath(%q) returned nil for a directory that is not a mount point", dir)
	}

	if strings.Contains(err.Error(), dir+string(filepath.Separator)+" ") ||
		!strings.Contains(err.Error(), dir) {
		t.Errorf("UnmountPath error does not report the cleaned path %q: %v", dir, err)
	}
}

// TestUnmountPathReportsCancellationAsItself is the distinction the caller's context buys.
//
// A `systemctl stop` that hits TimeoutStopSec cancels this context. Reported as "every method was
// tried", that sends the operator looking at the mount — at lsof, at the FUSE helper, at permissions —
// when the actual finding is that the stop ran out of time. Two different problems must not share one
// message.
func TestUnmountPathReportsCancellationAsItself(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := UnmountPath(ctx, t.TempDir())
	if err == nil {
		t.Fatal("UnmountPath with a canceled context returned nil")
	}

	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("UnmountPath with a canceled context = %v, want a message saying it was "+
			"interrupted rather than one listing refused attempts", err)
	}

	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("UnmountPath = %v, want the wrapped cause so errors.Is(err, context.Canceled) "+
			"holds for a caller that branches on it", err)
	}
}

// TestUnmountPathDoesNotUnmountByAccident asserts the operation is not attempted on a path that is not
// a mount point.
//
// The programs themselves refuse — verified: `umount /tmp/somedir` exits 1 with "not currently
// mounted" — but the property worth holding is that the directory and its contents survive. A helper
// invoked with the wrong flag, or a future candidate that takes a device rather than a path, could
// plausibly act on something.
func TestUnmountPathDoesNotUnmountByAccident(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	file := filepath.Join(dir, "sentinel")
	if err := os.WriteFile(file, []byte("still here"), 0o600); err != nil {
		t.Fatalf("writing the sentinel file: %v", err)
	}

	_ = UnmountPath(context.Background(), dir)

	got, err := os.ReadFile(file) //nolint:gosec // a path under this test's own TempDir
	if err != nil {
		t.Fatalf("the sentinel file did not survive a failed unmount: %v", err)
	}

	if string(got) != "still here" {
		t.Errorf("the sentinel file reads %q after a failed unmount, want %q", got, "still here")
	}
}

// TestUnmountCommandsAreWellFormed checks the per-platform table.
//
// It is the platform files' only unit-testable surface, and three of the four properties it asserts
// are ones a plausible edit breaks silently. The fourth is the deliberate decision recorded in both
// platform files: no candidate unmounts lazily or forcibly. `fusermount3 -z`, `umount -l`, and
// `umount -f` all detach the name while the filesystem keeps serving whatever has it open, so they
// convert "I could not unmount this" into "I unmounted this" with writes in flight. Adding one would
// make every test above pass — the unmount would appear to succeed — which is exactly why it is
// asserted here rather than left to the comments.
func TestUnmountCommandsAreWellFormed(t *testing.T) {
	t.Parallel()

	const mountPoint = "/mnt/objectfs/research-data"

	commands := unmountCommands(mountPoint)
	if len(commands) == 0 {
		t.Fatal("unmountCommands returned nothing, so UnmountPath would fall straight through to the " +
			"syscall — which needs root, and the unprivileged case is the common one")
	}

	seen := make(map[string]bool, len(commands))

	for _, c := range commands {
		if c.name == "" {
			t.Error("an unmount candidate has no program name")
		}
		if c.why == "" {
			t.Errorf("the %s candidate has no reason, so it appears in the failure message as a bare "+
				"exit status from a program the operator did not know was run", c.name)
		}
		if seen[c.name] {
			t.Errorf("%s appears twice in the candidate list", c.name)
		}

		seen[c.name] = true

		if !slices.Contains(c.args, mountPoint) {
			t.Errorf("the %s candidate's arguments %v do not include the mount point, so it would "+
				"act on something else or on nothing", c.name, c.args)
		}

		for _, arg := range c.args {
			if lazyOrForcedFlags[arg] {
				t.Errorf("the %s candidate passes %s. That detaches the mount while the filesystem is "+
					"still serving open files, so an unmount that did not finish is reported as one "+
					"that did — with writes in flight. Both platform files say so; if this is wanted, "+
					"it belongs behind an explicit operator request, not in the default path.",
					c.name, arg)
			}
		}
	}
}

// lazyOrForcedFlags are the flags that make an unmount report success before it has finished.
var lazyOrForcedFlags = map[string]bool{
	"-z": true, "-uz": true, "-l": true, "-f": true, "--lazy": true, "--force": true,
	"force": true, // `diskutil unmount force` is the macOS spelling
}
