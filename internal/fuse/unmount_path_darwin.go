//go:build darwin

package fuse

import "syscall"

// unmountCommands lists macOS's unmount programs, in the order they should be tried.
//
// `umount` first. macFUSE mounts are unmounted by the ordinary program on macOS — there is no setuid
// FUSE helper in the Linux sense, because macFUSE's kernel extension permits the mount's owner to
// remove it.
//
// `diskutil unmount` second. It goes through diskarbitrationd, which is what the Finder and Spotlight
// use, so it can succeed where a bare umount is refused because a system agent has the volume open —
// diskarbitrationd asks those agents to let go first. It is also the only one of the two that reliably
// tears down the volume's entry in the Finder sidebar.
//
// Neither is forced. `umount -f` on a FUSE volume abandons whatever the filesystem was still writing,
// so it turns an unmount that could not complete into one that reports success with data unwritten. An
// operator who needs it can run it; ObjectFS will not choose it on their behalf.
func unmountCommands(mountPoint string) []unmountCommand {
	return []unmountCommand{
		{
			name: "umount",
			args: []string{mountPoint},
			why:  "the ordinary macOS unmount, which is what macFUSE volumes take",
		},
		{
			name: "diskutil",
			args: []string{"unmount", mountPoint},
			why: "goes through diskarbitrationd, so it can succeed where umount is refused by Spotlight " +
				"or the Finder holding the volume open",
		},
	}
}

// unmountBySyscall calls unmount(2) with no flags.
//
// Not MNT_FORCE, for the reason the command table gives: forcing discards in-flight writes and reports
// success. This is the last resort for a root caller, and if it fails the honest answer is that the
// volume is busy.
func unmountBySyscall(mountPoint string) error {
	return syscall.Unmount(mountPoint, 0)
}
