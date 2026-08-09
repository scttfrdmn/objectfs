#!/bin/bash
# ObjectFS pre-removal scriptlet (deb prerm / rpm %preun).
#
# Wired into both packages by nfpm.yaml. Runs *before* the package's files are deleted, which is why
# it can still use /usr/bin/objectfs — that matters, because `objectfs unmount` is the only unmount
# path that reports which of several methods it tried and what is holding a mount open.
#
# THIS SCRIPT DOES NOT ALWAYS EXIT 0, AND THAT IS THE POINT.
#
# scripts/postinstall.sh exits 0 unconditionally: nothing it checks is a reason to leave a package
# half-configured, and a filesystem that cannot mount yet is a problem the mount command should
# report. Removal is the opposite case. If a mount survives removal, the machine is left with a live
# FUSE filesystem whose server binary has just been deleted — every read against it hangs or returns
# EIO, `ls` on the mount point blocks in the kernel, and the only way out is a manual `fusermount -u`
# by someone who has to work out that is what happened. Reporting a successful removal in that state
# is worse than refusing to remove: the refusal names the mount and the process holding it, while the
# success leaves no trace of the cause.
#
# So: every systemctl call keeps its `|| true`, and the unmount loop does not.
#
# The `|| true` on systemctl is correct because a unit that will not stop is not a mount that will
# not unmount. `systemctl stop` fails when the unit does not exist, when systemd is not the running
# init, when the unit is already inactive on some versions — and in the cases that matter it has
# already sent SIGTERM, which is what makes ObjectFS flush and unmount. The unmount check below is
# the authority on whether that worked, so a failed stop is at worst redundant information.
#
# Which package managers honour the refusal, precisely, because the two differ:
#
#   - dpkg: a non-zero prerm aborts the removal. The package stays installed (in "half-configured"),
#     apt reports it, and the next `apt remove` retries. This is exactly the intended behaviour.
#   - rpm: a non-zero %preun is reported as a scriptlet failure in the transaction output, and rpm
#     proceeds with the erasure anyway — `%preun` is not a veto the way `%pre` is. So on RPM systems
#     this is a loud, exit-status-bearing error rather than a refusal. Said out loud rather than
#     assumed, because "failing removal is better" is only literally available on one of the two.
#
# `set -e` is absent deliberately: the failure this script reports is decided at the end, after
# every mount has been attempted, and an early abort would skip the remaining mounts and report a
# less complete picture than it had.

set -u

# ROOT prefixes the paths this script reads, for internal/config/packaging_test.go. See the same
# variable in scripts/postinstall.sh. Empty in a package.
ROOT="${OBJECTFS_ROOT:-}"

# The package manager's action argument.
#
# This is load-bearing, not logging, and it is the one thing the previous version of this script got
# structurally wrong: it unmounted every live ObjectFS filesystem on *every* invocation, including
# during an upgrade. dpkg runs prerm with "upgrade <version>" when replacing a package, and rpm runs
# %preun with an instance count of 1 for the outgoing package of an upgrade. So `apt upgrade
# objectfs` tore down every mount on the machine, and nothing brought them back: the new package's
# postinst does not start units, and correctly does not, since it cannot know which were running.
#
# An upgrade replaces a binary. The running mount processes keep the old inode until they are
# restarted, which is the ordinary story for every daemon on the system, and `systemctl restart
# objectfs@<name>` at a moment the operator chooses is the correct way to pick up a new one. Stopping
# them here converts a package upgrade into an unannounced outage.
ACTION="${1:-}"

warn() {
    echo "objectfs: WARNING: $*" >&2
}

# is_upgrade reports whether this invocation is the outgoing half of an upgrade rather than a real
# removal.
#
# Both conventions, because one script serves both packages:
#
#   - dpkg passes "upgrade", "failed-upgrade", or "deconfigure"; a real removal is "remove" or
#     "purge".
#   - rpm passes the number of instances of the package that will remain. "1" means this erasure is
#     part of an upgrade; "0" is the last copy going away.
#
# An unrecognised or absent argument is treated as a removal. That is the conservative direction: it
# means a hand-run `scripts/preremove.sh` does the full teardown, and the cost of being wrong is
# stopping services that were going to be restarted, not leaving a mount behind.
is_upgrade() {
    case "$ACTION" in
    upgrade | failed-upgrade | deconfigure | 1) return 0 ;;
    *) return 1 ;;
    esac
}

# systemd_is_running distinguishes "systemctl exists" from "systemd is init".
#
# Both are needed: in a container or chroot with the package installed, the binary is present and
# every call fails with "System has not been booted with systemd", which would print a wall of
# warnings about units that cannot exist.
systemd_is_running() {
    [ -z "$ROOT" ] && command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]
}

# stop_and_disable_units stops every running objectfs@ instance and disables every enabled one.
#
# `--no-legend --no-pager --plain`, and matching on the first field, rather than the previous
# version's `| grep 'objectfs@' | awk '{print $1}'`. That pipeline read systemctl's *table*, whose
# header and footer lines it also matched, and whose first column is a bullet ("●") for a failed unit
# — so a failed objectfs@ instance yielded the unit name in field 2 and the script tried to stop a
# service literally named "●".
#
# Stopping is what flushes. `systemctl stop` sends SIGTERM, ObjectFS unmounts, and the unmount writes
# every dirty range to S3 inside TimeoutStopSec=90. So this runs before the unmount loop below, and
# the loop is the check on whether it succeeded rather than a parallel mechanism.
stop_and_disable_units() {
    if ! systemd_is_running; then
        return 0
    fi

    local unit

    while read -r unit _; do
        case "$unit" in
        objectfs@*.service) ;;
        *) continue ;;
        esac

        echo "objectfs: stopping $unit"
        systemctl stop "$unit" >/dev/null 2>&1 || true
    done < <(systemctl list-units --type=service --state=running --no-legend --no-pager --plain 2>/dev/null)

    while read -r unit _; do
        case "$unit" in
        objectfs@*.service) ;;
        *) continue ;;
        esac

        echo "objectfs: disabling $unit"
        systemctl disable "$unit" >/dev/null 2>&1 || true
    done < <(systemctl list-unit-files 'objectfs@*.service' --state=enabled --no-legend --no-pager --plain 2>/dev/null)
}

# objectfs_mountpoints prints the mount point of every live ObjectFS filesystem, one per line.
#
# Read from /proc/mounts rather than from `mount`, because the columns of `mount` output are prose
# ("objectfs on /mnt/x type fuse.objectfs (rw,...)") and differ between util-linux versions, while
# /proc/mounts is a fixed four-plus-field format.
#
# **The match is on three things, and the previous version matched on one that never fires.** It
# grepped for `type fuse.objectfs`, and ObjectFS mounts do not report that: internal/fuse sets
# Subtype "s3" alongside FSName "objectfs", so the kernel records the filesystem type as `fuse.s3`
# and the device as `objectfs`. That is recorded in this repository's own changelog, where the same
# assumption had already been found and fixed in the JavaScript SDK's isMounted. So the unmount loop
# this script was written to perform has never run on a real mount, which is the same class of defect
# as #207 itself — a correct-looking mechanism with nothing reaching it.
#
# Matching all three of `fuse.objectfs`, `fuse.s3`, and a device of `objectfs` is deliberate rather
# than belt-and-braces: the fstype depends on mount options a user can change, and the union is
# still narrow enough that no non-ObjectFS FUSE filesystem can satisfy it.
objectfs_mountpoints() {
    local mounts="${ROOT:-}/proc/mounts"
    local device point fstype

    if [ ! -r "$mounts" ]; then
        return 0
    fi

    while read -r device point fstype _; do
        case "$fstype" in
        fuse.objectfs | fuse.s3) ;;
        fuse.*)
            [ "$device" = "objectfs" ] || continue
            ;;
        *) continue ;;
        esac

        # /proc/mounts escapes space, tab, newline and backslash in octal. \040 is the one that
        # occurs in practice, and a mount point with a space in it that this printed raw would be
        # split into two arguments by the caller.
        printf '%b\n' "${point//\\040/\\0040}"
    done <"$mounts"
}

# unmount_one unmounts a path, preferring the tool that explains itself.
#
# `objectfs unmount` first because it is the only candidate that reports *why*: it tries fusermount3,
# its libfuse-2 spelling, umount, and umount(2) in turn, names which were not installed, and on a
# busy mount prints the `lsof +D` invocation that identifies the holder. The package's files are
# still on disk at prerm time, so it is available here — but not if the binary was removed by hand,
# hence the fallbacks.
#
# No -z, -l or -f anywhere. A lazy or forced unmount detaches the name while the filesystem keeps
# serving already-open files, so it would report a finished unmount with writes still in flight — the
# exact outcome the check at the bottom of this script exists to prevent, achieved by lying rather
# than by working.
unmount_one() {
    local point="$1"

    if command -v objectfs >/dev/null 2>&1; then
        objectfs unmount "$point" && return 0
    fi

    if command -v fusermount3 >/dev/null 2>&1; then
        fusermount3 -u "$point" 2>&1 && return 0
    fi

    if command -v fusermount >/dev/null 2>&1; then
        fusermount -u "$point" 2>&1 && return 0
    fi

    umount "$point" 2>&1 && return 0

    return 1
}

# unmount_all unmounts every live ObjectFS filesystem and returns non-zero if any survived.
unmount_all() {
    local point
    local -a surviving=()

    while read -r point; do
        [ -n "$point" ] || continue

        echo "objectfs: unmounting $point"

        if [ -n "$ROOT" ]; then
            # Under a test root there is no real mount to remove, and running umount against a
            # scratch path would be either a no-op or a mistake. The mount table the test provides is
            # what is being asserted on: which paths this script selects, and that a path it could
            # not unmount is reported. OBJECTFS_PREREMOVE_UNMOUNT_FAILS makes the second case
            # reachable.
            if [ -n "${OBJECTFS_PREREMOVE_UNMOUNT_FAILS:-}" ]; then
                surviving+=("$point")
            fi

            continue
        fi

        unmount_one "$point" || surviving+=("$point")
    done < <(objectfs_mountpoints)

    if [ ${#surviving[@]} -eq 0 ]; then
        return 0
    fi

    warn "${#surviving[@]} ObjectFS filesystem(s) could not be unmounted:"

    for point in "${surviving[@]}"; do
        echo "  - $point" >&2
    done

    echo "" >&2
    echo "  Removal is being reported as failed rather than leaving these behind. The package's" >&2
    echo "  binary is about to be deleted, and a FUSE mount whose server is gone hangs every read" >&2
    echo "  against it — including 'ls' on the mount point — until someone unmounts it by hand." >&2
    echo "" >&2
    echo "  The usual cause is a process with an open file or a working directory under the mount:" >&2

    for point in "${surviving[@]}"; do
        echo "    lsof +D $point" >&2
    done

    echo "" >&2
    echo "  Once nothing is holding them, retry the removal." >&2

    return 1
}

main() {
    if is_upgrade; then
        echo "objectfs: upgrade ($ACTION) — leaving running mounts and enabled units alone."
        echo "objectfs: restart instances when convenient: systemctl restart objectfs@<name>.service"

        return 0
    fi

    stop_and_disable_units

    local status=0
    unmount_all || status=1

    # Printed even on failure, because the operator retrying the removal should not have to wonder
    # whether the first attempt deleted their configuration.
    #
    # "contents", precisely, and the distinction is not pedantry. nfpm.yaml ships these three as
    # `type: dir` entries, so the package manager owns the directories themselves and will remove
    # each one that is empty. What it will not touch is anything inside: a directory holding a config
    # file or a cache is non-empty, so it survives with its contents. Claiming the directories are
    # "left in place" would be wrong for an empty /var/log/objectfs and would send an operator
    # looking for something that is already gone.
    echo "objectfs: file contents under these paths are kept, not removed:"
    echo "objectfs:   /etc/objectfs/          configuration"
    echo "objectfs:   /var/cache/objectfs/    cached object data"
    echo "objectfs:   /var/log/objectfs/      logs"
    echo "objectfs: delete them by hand to remove every trace."

    return "$status"
}

main
