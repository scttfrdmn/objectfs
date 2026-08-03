//go:build linux

package fuse

import "syscall"

// unmountCommands lists Linux's unmount programs, in the order they should be tried.
//
// fusermount3 first, and its `-u` rather than `-uz`: it is the FUSE helper, it is setuid root so it
// works for the unprivileged user who made the mount, and a clean unmount is the one that lets the
// mounting process see its server close and run its own shutdown. libfuse 3 ships it as `fusermount3`;
// libfuse 2 shipped it as `fusermount`, and enough systems still have only the latter that both are
// worth trying.
//
// `umount` last of the three, because it needs privilege for a FUSE mount and so will usually fail
// where a helper would have worked — but it is what exists on a system with no libfuse installed at
// all, which a container running ObjectFS as root can be.
//
// Nothing here unmounts lazily. `fusermount3 -z` and `umount -l` both detach the name while leaving the
// filesystem serving whatever still has it open, so they turn "I could not unmount this" into "I
// unmounted this" while writes are still in flight — reported success for an operation that has not
// finished, which is the failure mode this project treats as worse than the failure itself. An operator
// who wants that can run it themselves; ObjectFS will not choose it for them.
func unmountCommands(mountPoint string) []unmountCommand {
	return []unmountCommand{
		{
			name: "fusermount3",
			args: []string{"-u", mountPoint},
			why:  "the libfuse 3 helper, setuid root, and the only unmount an unprivileged user can do",
		},
		{
			name: "fusermount",
			args: []string{"-u", mountPoint},
			why:  "the libfuse 2 name for the same helper",
		},
		{
			name: "umount",
			args: []string{mountPoint},
			why:  "needs privilege for a FUSE mount, but is present where libfuse is not",
		},
	}
}

// unmountBySyscall calls umount(2) with no flags.
//
// No MNT_DETACH and no MNT_FORCE, for the reason the command table gives: a detached mount still serves
// its open files under a name that no longer exists, so it reports success for an unmount that has not
// happened. This is the last resort for a root caller on a system with no helper, and if it fails the
// honest answer is that the filesystem is busy.
func unmountBySyscall(mountPoint string) error {
	return syscall.Unmount(mountPoint, 0)
}
