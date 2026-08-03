//go:build linux || darwin

package fuse

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// unmountAttemptTimeout bounds each individual unmount attempt.
//
// The command is the thing that hangs, not the thing that hangs on. `umount` on a mount whose server
// is gone can block in the kernel indefinitely, and `objectfs unmount` is what an operator reaches for
// precisely then — a systemd unit's ExecStop most of all, where a command that never returns turns a
// stop into a 90-second wait and then a SIGKILL.
const unmountAttemptTimeout = 15 * time.Second

// unmountCommand is one way to unmount a path, as an external program.
type unmountCommand struct {
	// name is the program, resolved through PATH.
	name string

	// args are its arguments, with the mount point already substituted.
	args []string

	// why says what this command is for, and appears in the combined error if every candidate fails.
	// Without it the failure is a list of exit statuses from programs the operator did not know were
	// being run.
	why string
}

// UnmountPath unmounts an ObjectFS filesystem by path, from a process that did not mount it.
//
// This is not [MountManager.Unmount] and cannot be. That method needs the go-fuse server handle held by
// the process that called Mount, which by construction is a different process here: `objectfs unmount
// /mnt/objectfs/research-data` is run by an operator at a shell, or by a systemd unit's ExecStop after
// the mounting process has already been signaled (#134, #135).
//
// It runs the platform's unmount programs rather than calling syscall.Unmount directly, and the reason
// is privilege. An unprivileged FUSE unmount on Linux has to go through `fusermount3`, which is setuid
// root and checks that the caller is the one who mounted it; syscall.Unmount from the same process
// returns EPERM. So the syscall is the last candidate rather than the first — it is what works when a
// helper is absent and the caller is root, which is the systemd case.
//
// Nothing is stat'ed first. Checking "is this actually a mount point?" before unmounting reads as
// defensive, but a mount whose server has died is one where stat blocks in the kernel forever, and that
// is the case this command exists for. So the attempt is the check: if no candidate can unmount it, the
// error says so and lists what was tried.
func UnmountPath(ctx context.Context, mountPoint string) error {
	if strings.TrimSpace(mountPoint) == "" {
		return errors.New("no mount point given")
	}

	abs, err := filepath.Abs(mountPoint)
	if err != nil {
		return fmt.Errorf("cannot resolve mount point %q: %w", mountPoint, err)
	}

	// filepath.Abs cleans, so "/mnt/x/" and "/mnt/x" are one path by here. That matters because the
	// kernel's mount table holds the cleaned form, and `umount /mnt/x/` fails on some systems while
	// `umount /mnt/x` succeeds — a trailing slash is what tab-completion produces.
	candidates := unmountCommands(abs)

	var attempts []string

	for _, c := range candidates {
		// Resolved before running so that an absent helper is reported as absent rather than as a
		// failed unmount. On a minimal container image fusermount3 genuinely is not installed, and
		// "fusermount3: not found" is a different problem from "the unmount was refused".
		if _, err := exec.LookPath(c.name); err != nil {
			attempts = append(attempts, fmt.Sprintf("%s (%s): not installed", c.name, c.why))

			continue
		}

		attemptCtx, cancel := context.WithTimeout(ctx, unmountAttemptTimeout)
		// gosec flags the operator-supplied mount point reaching an exec. It is not a shell — no shell
		// is spawned, so the path is one argv element and cannot become a second command however it is
		// punctuated — and the program name comes from the platform table above rather than from input.
		// What is left is that this unmounts the path it was given, which is the function.
		// Suppressed twice because there are two gosec runs reading different directives: golangci-lint
		// honors //nolint:gosec, and the standalone gosec whose SARIF feeds GitHub code scanning honors
		// only #nosec. One without the other passes the lint job and fails the security check.
		//nolint:gosec // no shell; the program is from a fixed table and the path is a single argv element
		out, err := exec.CommandContext(attemptCtx, c.name, c.args...).CombinedOutput() // #nosec G204 -- no shell; fixed program table, path is one argv element
		cancel()

		if err == nil {
			return nil
		}

		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}

		attempts = append(attempts, fmt.Sprintf("%s %s (%s): %s",
			c.name, strings.Join(c.args, " "), c.why, detail))
	}

	// The caller's own cancellation is reported as itself. Otherwise a `systemctl stop` that timed out
	// would be described as an unmount that every method refused, which sends the operator looking at
	// the mount instead of at the timeout.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("unmounting %s was interrupted: %w", abs, ctxErr)
	}

	syscallErr := unmountBySyscall(abs)
	if syscallErr == nil {
		return nil
	}

	attempts = append(attempts, fmt.Sprintf("umount(2) directly: %s (this is the one that needs root; "+
		"an unprivileged unmount has to go through a setuid helper)", syscallErr))

	return fmt.Errorf("could not unmount %s. Every method was tried:\n  - %s\n\nIf the path is not "+
		"mounted, there is nothing to do. If it is, the usual causes are a process with an open file "+
		"or a working directory under the mount — `lsof +D %s` names them — or a mount made by a "+
		"different user, which only that user or root can remove",
		abs, strings.Join(attempts, "\n  - "), abs)
}
