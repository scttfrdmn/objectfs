#!/bin/bash
# ObjectFS post-installation scriptlet (deb postinst / rpm %post).
#
# Wired into both packages by nfpm.yaml. Everything here is a *guard check plus an action*, never a
# bare action, because a package scriptlet is not run once: dpkg runs postinst on every
# reconfiguration and rpm runs %post on every upgrade. The previous version of this file ran
# `mkdir -p` and `chmod 755` unconditionally over four directories, so an operator who had tightened
# /etc/objectfs to 0750 got it widened back to 0755 by the next `apt upgrade` — with no message, and
# with the config file that lives in it holding whatever the operator put there.
#
# So the rule below is stronger than "do not fail on a second run": a mode or a file this script did
# not create, it does not touch. Verified by mutation in internal/config/packaging_test.go, which
# runs the script twice against a scratch root, tightens permissions between the runs, and asserts
# the second run left them alone.
#
# THIS SCRIPT ALWAYS EXITS 0.
#
# Not out of tidiness — it is the difference between a warning and a broken install. A non-zero
# postinst leaves dpkg with a package in "half-configured" state, which blocks every subsequent
# apt operation until someone runs `dpkg --configure -a`; a non-zero %post is reported by rpm but
# leaves the package installed and the transaction dirty. None of the checks here is a reason for
# that. ObjectFS installs perfectly well on a machine with no FUSE and no systemd — a build server,
# a container image being prepared for later use — and the things it cannot do there are things it
# cannot do at *mount* time, where the mount command is what should complain, with the mount point
# in hand.
#
# `set -e` is deliberately absent for the same reason: with it, any check that happened to return
# non-zero would abort the script and take the exit status with it.
#
# Warnings go to stderr. stdout of a package scriptlet is interleaved into apt/dnf's own progress
# output, where a multi-line remediation block is noise; stderr is what a `2>` redirect and a CI log
# scraper can separate. Every warning carries the exact command that fixes it, because the operator
# reading it is mid-install and will not go looking for documentation.

set -u

# OBJECTFS_ROOT prefixes every path this script touches. Empty in a package — the scriptlet runs
# against the real filesystem — and set to a scratch directory by internal/config/packaging_test.go,
# which is the only way to assert the idempotency claims above without being root on a throwaway
# machine.
#
# Nothing in nfpm.yaml sets it. It is read rather than hardcoded to "" so that the test exercises
# this exact file rather than a copy of it with the paths rewritten.
ROOT="${OBJECTFS_ROOT:-}"

# The action the package manager is performing, where it tells us. dpkg passes "configure" on a
# fresh install and on reconfiguration; rpm passes the number of package instances now present, so
# "1" is an install and "2" an upgrade. Recorded rather than branched on: every action below is
# idempotent, so there is nothing to skip, and the value is only worth having in the log.
ACTION="${1:-}"

# The file limit ObjectFS wants. Matches LimitNOFILE in configs/systemd/objectfs@.service — one
# number, and a mismatch between the two would mean the warning below fires on a system where the
# unit is already correct.
readonly WANTED_NOFILE=65536

# warn prints a warning to stderr. Every call site pairs it with a remediation line.
warn() {
    echo "objectfs: WARNING: $*" >&2
}

# ensure_dir creates a directory at the given mode, and does nothing at all if it already exists.
#
# The "does nothing" half is the point. chmod-on-every-run is what widened /etc/objectfs back to
# 0755 on upgrade; a directory that is already there is a directory whose mode is the operator's
# business.
ensure_dir() {
    local path="$1" mode="$2"

    if [ -d "$path" ]; then
        return 0
    fi

    if ! mkdir -p "$path" 2>/dev/null; then
        warn "could not create $path"
        echo "  Fix: mkdir -p $path" >&2
        return 0
    fi

    chmod "$mode" "$path" 2>/dev/null || warn "could not set mode $mode on $path"
}

# install_example_config copies the shipped example to /etc/objectfs/config.yaml, once.
#
# Once, and never again: the file is the operator's credentials-adjacent configuration, and an
# upgrade that overwrote it would be data loss of the kind that is only noticed at the next mount.
# This is also why the example is shipped to /usr/share/objectfs/configs/ and copied here rather
# than being installed to /etc directly as a packaged file — a packaged /etc file is a dpkg conffile,
# which prompts on upgrade, and an interactive prompt in the middle of an unattended apt run is its
# own failure. The package owns the copy under /usr/share; the operator owns the one under /etc.
#
# Mode 0600. The example ships no secrets — ObjectFS uses the AWS credential chain and
# configs/example.yaml says so — but this is the file an operator will paste a key into if they are
# going to paste one anywhere.
install_example_config() {
    local example="$ROOT/usr/share/objectfs/configs/example.yaml"
    local target="$ROOT/etc/objectfs/config.yaml"

    if [ -f "$target" ]; then
        return 0
    fi

    if [ ! -f "$example" ]; then
        warn "$example is missing, so no starting configuration was installed"
        echo "  Fix: copy configs/example.yaml from the source tree to $target" >&2
        return 0
    fi

    if ! cp "$example" "$target" 2>/dev/null; then
        warn "could not write $target"
        return 0
    fi

    chmod 600 "$target" 2>/dev/null || warn "could not set mode 600 on $target"

    echo "objectfs: starting configuration written to /etc/objectfs/config.yaml"
    echo "objectfs: edit it before mounting — the region and cache settings are the ones that matter"
}

# check_fuse_module reports on the FUSE prerequisites ObjectFS cannot supply for itself.
#
# Three separate things, and they fail for different reasons:
#
#   - the kernel module. `modprobe fuse` is attempted because on a freshly booted minimal image it
#     is simply not loaded yet and loading it is free. It is allowed to fail silently: it fails in a
#     container (no CAP_SYS_MODULE), it fails when fuse is built in rather than modular (in which
#     case /sys/module/fuse exists and there is nothing to do), and neither is a problem.
#   - the userspace helper. fusermount3 is what an *unprivileged* mount goes through. A root mount
#     does not need it — internal/fuse tries a direct mount and then umount(2) — so its absence is a
#     warning about which users can mount, not about whether ObjectFS works.
#   - user_allow_other in /etc/fuse.conf. Without it, `allow_other` is refused for non-root callers,
#     which is the single most common silent failure operators hit: the mount succeeds, and then
#     every other user on the machine gets EACCES on the mount point with nothing in any log to say
#     why.
check_fuse_module() {
    if [ -z "$ROOT" ]; then
        modprobe fuse >/dev/null 2>&1 || true
    fi

    if ! command -v fusermount3 >/dev/null 2>&1 && ! command -v fusermount >/dev/null 2>&1; then
        warn "no fusermount3 (or fusermount) on PATH; only root will be able to mount"
        echo "  Fix, Debian/Ubuntu: apt-get install fuse3" >&2
        echo "  Fix, RHEL/Rocky/Fedora: dnf install fuse3" >&2
        echo "  Fix, Alpine: apk add fuse3" >&2
    fi

    local conf="$ROOT/etc/fuse.conf"

    if [ ! -f "$conf" ]; then
        warn "$conf does not exist, so 'allow_other' mounts will be refused for non-root users"
        echo "  Fix: echo user_allow_other >> /etc/fuse.conf" >&2
        return 0
    fi

    # Anchored, and tolerating leading whitespace, because libfuse parses the directive at the start
    # of a line and a commented-out `#user_allow_other` — which is how every distribution ships the
    # file — must not count as present. An unanchored grep matches the comment, which would make
    # this check pass on exactly the systems it exists for.
    if ! grep -Eq '^[[:space:]]*user_allow_other([[:space:]]|$)' "$conf"; then
        warn "/etc/fuse.conf does not enable user_allow_other, so 'allow_other' mounts will be" \
            "refused for non-root users"
        echo "  Fix: echo user_allow_other >> /etc/fuse.conf" >&2
    fi
}

# check_system_limits reports on the open-file limit.
#
# A FUSE filesystem over S3 holds a descriptor per open file plus the HTTP connection pool, and the
# common default soft limit of 1024 is reached by a parallel build or an rsync of a large tree — as
# EMFILE, from inside a filesystem, which surfaces to the application as an I/O error rather than as
# anything mentioning limits.
#
# The remediation is deliberately *not* the one issue #137 specifies. That issue asks for a hint to
# add DefaultLimitNOFILE=65536 to /etc/systemd/system.conf, and for objectfs@ units that would be
# redundant: configs/systemd/objectfs@.service already sets LimitNOFILE=65536 itself, so a
# systemd-managed mount has the limit regardless of the system default — and changing the system
# default raises it for every other service on the box, which is a larger decision than installing
# a filesystem. The limit that is genuinely low is the one an *interactive* `objectfs mount` or a
# user's own process inherits, and /etc/security/limits.conf is where that is set.
check_system_limits() {
    local soft
    soft=$(ulimit -Sn 2>/dev/null) || return 0

    # "unlimited" is not a number and compares as a string; it is also the answer we want.
    if [ "$soft" = "unlimited" ]; then
        return 0
    fi

    case "$soft" in
    '' | *[!0-9]*) return 0 ;;
    esac

    if [ "$soft" -ge "$WANTED_NOFILE" ]; then
        return 0
    fi

    warn "the open-file soft limit is $soft; ObjectFS wants $WANTED_NOFILE for interactive mounts"
    echo "  systemd-managed mounts are unaffected: objectfs@.service sets LimitNOFILE=$WANTED_NOFILE." >&2
    echo "  For shell sessions, add to /etc/security/limits.conf:" >&2
    echo "    *  soft  nofile  $WANTED_NOFILE" >&2
    echo "    *  hard  nofile  $WANTED_NOFILE" >&2
}

# reload_systemd tells systemd about the unit this package just installed.
#
# Two conditions, not one. `command -v systemctl` says the binary exists; /run/systemd/system says
# systemd is the running init. Both are needed because the first is true and the second false in
# every container and chroot that has the package installed but is not booted under it, and
# `systemctl daemon-reload` there fails with "System has not been booted with systemd".
reload_systemd() {
    if [ -n "$ROOT" ]; then
        return 0
    fi

    if ! command -v systemctl >/dev/null 2>&1 || [ ! -d /run/systemd/system ]; then
        return 0
    fi

    systemctl daemon-reload >/dev/null 2>&1 ||
        warn "systemctl daemon-reload failed; run it before starting objectfs@<name>.service"
}

# check_kernel_congestion_control reports when BBR is unavailable.
#
# Purely informational, and scoped much more narrowly than the version of this check it replaces.
# That one parsed `uname -r` and warned below 4.9, which is the kernel version BBR was *merged* in —
# but availability is a build and module question, not a version question, and internal/network
# answers it by reading procfs at dial time and falling back to CUBIC. So this reads the same file
# the code reads instead of inferring from a version string, and says nothing at all when it cannot.
#
# The old check also had two shellcheck findings and a subshell-grouped `||` whose precedence was
# only accidentally right.
check_kernel_congestion_control() {
    local available="$ROOT/proc/sys/net/ipv4/tcp_available_congestion_control"

    if [ ! -r "$available" ]; then
        return 0
    fi

    if grep -qw bbr "$available" 2>/dev/null; then
        return 0
    fi

    echo "objectfs: note: TCP BBR is not available on this kernel, so transfers will use the"
    echo "objectfs: system default congestion control. ObjectFS detects this at connect time and"
    echo "objectfs: falls back automatically; throughput on high-latency paths will be lower."
    echo "objectfs: To enable it: modprobe tcp_bbr (kernel 4.9 or later)"
}

main() {
    ensure_dir "$ROOT/etc/objectfs" 755
    ensure_dir "$ROOT/var/cache/objectfs" 755
    ensure_dir "$ROOT/var/log/objectfs" 755
    ensure_dir "$ROOT/mnt/objectfs" 755

    install_example_config

    reload_systemd

    check_fuse_module
    check_system_limits
    check_kernel_congestion_control

    echo "objectfs: installed${ACTION:+ ($ACTION)}. Next:"
    echo "objectfs:   1. edit /etc/objectfs/config.yaml"
    echo "objectfs:   2. objectfs mount s3://your-bucket /mnt/objectfs"
    echo "objectfs:   or, per instance: cp /etc/objectfs/config.yaml /etc/objectfs/<name>.yaml"
    echo "objectfs:                     systemctl enable --now objectfs@<name>.service"
    echo "objectfs: Docs: https://github.com/scttfrdmn/objectfs/tree/main/docs"
}

main

# Explicit, and the last line for a reason: main's exit status is that of its final echo, which is
# fine today and is one edit away from being a check's status instead.
exit 0
