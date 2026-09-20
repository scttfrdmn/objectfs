#!/usr/bin/env bash
# ObjectFS installer: fetch a release binary from GitHub, verify its checksum, install it.
#
# This is part (b) of #138, and it is deliberately not the script that issue specifies. #138's
# install.sh detects the package manager and adds an apt or yum repository at
# packages.objectfs.io — a host that does not exist. A script written to that spec could not be
# tested, could not be run, and would fail at its first curl with a TLS error naming a domain the
# reader has no way to interpret.
#
# objectfs.io itself now serves the landing page and the documentation at /docs/, but that changes
# nothing here: Porkbun answers a wildcard, so packages.objectfs.io resolves exactly like every other
# name under the domain and still completes no TLS handshake. Resolution was never the evidence.
#
# What GitHub already hosts is a tarball per platform and a SHA-256 beside it. That needs no
# hosting decision, so it is what this installs — and it is now the only thing that will. The
# repository branch this comment used to promise is not coming: the apt and yum repositories were
# built, kept green in CI for several releases, and never published a byte, so the machinery has been
# removed rather than left waiting for a hosting decision nobody was going to make. The download path
# was in any case the one that works on a machine with no root, no package manager entry, and no
# network path to a third-party repo — which describes a large share of the HPC login nodes this
# project is for. Releases also carry a .deb and an .rpm for anyone who wants one; `apt install
# ./objectfs_*.deb` takes a file directly and needs no repository.
#
# THE CHECKSUM IS NOT OPTIONAL AND THERE IS NO FLAG TO SKIP IT.
#
# The whole reason to publish a checksum is that the download path is not trusted, and an installer
# that treats verification as a step that may be skipped when inconvenient has spent the cost of
# checksums without buying the property. So a missing .sha256, an unreadable one, or a mismatch
# each abort before anything is installed. The one thing this cannot do is establish that the
# checksum itself is authentic — it comes down the same channel as the tarball, so it detects
# corruption and a mirror that mangled the file, not a compromise of the release host. That is a
# signature's job, and this script says so rather than implying more than it delivers.
#
# Everything is staged in a temporary directory and moved into place at the end, so a failure at
# any point leaves no half-installed binary. An interrupted install that leaves `objectfs` on PATH
# as a truncated file is worse than one that leaves nothing.
#
# Two things this does besides installing one binary, both added by #533:
#
#   - the /etc/fstab mount helper. Releases from v0.17.0 carry `mount.objectfs` in the Linux tarballs,
#     and this installs it beside `objectfs` and registers it at /sbin/mount.objectfs when it safely
#     can. See link_mount_helper for what "safely" rules out, which is more than it looks.
#   - `--uninstall`. There was no counterpart to scripts/preremove.sh, so a tarball install left the
#     symlink behind with nothing to remove it.

set -euo pipefail

readonly REPO="scttfrdmn/objectfs"
readonly PROGRAM="objectfs"

# The mount helper's filename is its registration. mount(8) resolves an unknown `-t TYPE` by exec'ing
# /sbin/mount.$TYPE — a path compiled into util-linux, not searched for on PATH and not configurable —
# so this string appears in .goreleaser.yml's `mount-helper` build, in scripts/postinstall.sh, in
# scripts/preremove.sh and here, and all four have to spell it identically.
readonly HELPER="mount.objectfs"

# The default prefix is ~/.local, not /usr/local, and that is a deliberate reversal of the usual
# installer convention. This project's users are frequently on a shared login node where they have
# no root at all, and an installer whose default requires sudo teaches them to run the whole thing
# under sudo — which then writes a root-owned binary and a root-owned cache directory into a home
# they share with their own jobs. ~/.local/bin is on PATH by default on every distribution this
# targets, and --prefix is one flag away for a site install.
PREFIX="${PREFIX:-$HOME/.local}"

# Empty means "whatever the latest release is". Resolved through the API rather than by following
# the /latest/download redirect, because the resolved tag is worth printing: an installer that says
# "installed objectfs" without saying which version has told the user nothing they can check.
VERSION="${VERSION:-}"

DRY_RUN=0

# Whether to install the /etc/fstab mount helper beside the binary. On by default, because a tarball
# install that silently lacks it is the gap #533 is about; `--no-mount-helper` is for an install into a
# prefix that is only ever going to be used interactively.
MOUNT_HELPER=1

# --uninstall. A mode of this script rather than a second script, because the thing that has to be
# undone is the /sbin symlink *this* script created, and the rule for when not to touch it is written
# once, here, next to the rule for when to create it.
UNINSTALL=0

# OBJECTFS_ROOT prefixes /sbin, and nothing else.
#
# Empty everywhere a user runs this. It is the same seam scripts/postinstall.sh and
# scripts/preremove.sh read, for the same reason: internal/config's tests drive the real file against a
# scratch root rather than a copy of it with the paths rewritten, which is the failure mode where a
# test agrees with itself and the shipped artifact is never checked. The prefix needs no equivalent —
# --prefix already points anywhere.
ROOT="${OBJECTFS_ROOT:-}"

# say prints progress to stderr, not stdout.
#
# So that `install.sh --print-url` style composition stays possible and, more importantly, so that
# a user piping this script's output somewhere does not get progress chatter mixed into it. The
# convention matches scripts/postinstall.sh, which sends warnings to stderr for the same reason.
say() {
    echo "objectfs-install: $*" >&2
}

die() {
    echo "objectfs-install: error: $*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Install ObjectFS from a GitHub release, verifying its SHA-256 checksum.

Usage: install.sh [options]

Options:
  --version VERSION   Release to install, with or without a leading v (default: latest)
  --prefix PATH       Install under PATH/bin (default: ~/.local)
  --no-mount-helper   Do not install mount.objectfs or register /sbin/mount.objectfs
  --uninstall         Remove what this script installed under --prefix, and its /sbin symlink
  --dry-run           Report what would happen; download, verify and remove nothing
  -h, --help          This message

Environment:
  VERSION, PREFIX     Same as the flags above
  GITHUB_TOKEN        Sent as a bearer token when resolving the latest release, which raises the
                      API rate limit. Never required, and never sent to the download host.

The checksum is always verified and there is no option to skip it. Note what that does and does
not establish: the .sha256 travels the same channel as the tarball, so a mismatch means corruption
or a tampered mirror, not necessarily an authentic release.

The Linux tarballs also carry mount.objectfs, the helper that makes `mount -t objectfs` and an
/etc/fstab entry work. Registering it means creating /sbin/mount.objectfs, which needs root and is
refused when the binary it would point at is not root-owned — mount(8) exec's that path as root. A
non-root install still installs the helper and prints the one command left to run.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            [ $# -ge 2 ] || die "--version needs a value"
            VERSION="$2"
            shift 2
            ;;
        --version=*)
            VERSION="${1#--version=}"
            shift
            ;;
        --prefix)
            [ $# -ge 2 ] || die "--prefix needs a value"
            PREFIX="$2"
            shift 2
            ;;
        --prefix=*)
            PREFIX="${1#--prefix=}"
            shift
            ;;
        --no-mount-helper)
            MOUNT_HELPER=0
            shift
            ;;
        --uninstall)
            UNINSTALL=1
            shift
            ;;
        --dry-run)
            DRY_RUN=1
            shift
            ;;
        -h | --help)
            usage
            exit 0
            ;;
        *)
            usage >&2
            die "unrecognized argument: $1"
            ;;
    esac
done

# The asset name is platform-<arch>, and the arch names are the release workflow's, not uname's.
#
# This mapping is the part most likely to rot, so it is written as a translation from what uname
# reports to what release.yml publishes, with both sides visible. `uname -m` says x86_64 where the
# release says amd64 and aarch64 where it says arm64, and getting that backwards produces a 404
# from the download rather than a clear error — which is why an unrecognized machine is refused
# here, by name, with the list of what exists.
detect_platform() {
    local os arch
    os="$(uname -s)"
    arch="$(uname -m)"

    case "$os" in
        Linux) os="linux" ;;
        Darwin) os="darwin" ;;
        *) die "unsupported operating system: $os. Releases are built for Linux and macOS" ;;
    esac

    case "$arch" in
        x86_64 | amd64) arch="amd64" ;;
        aarch64 | arm64) arch="arm64" ;;
        # armv7l is published for Linux only, and naming it here rather than letting the download
        # 404 is the difference between "no arm build exists" and "no arm build exists for macOS".
        armv7l | armv7)
            [ "$os" = "linux" ] || die "armv7 is published for Linux only, and this is $os"
            arch="armv7"
            ;;
        *)
            die "unsupported machine: $arch. Releases are built for amd64, arm64, and armv7 (Linux only)"
            ;;
    esac

    echo "${os}-${arch}"
}

# preflight refuses to start when a tool this needs is missing, naming all of them at once.
#
# This exists because of two failures found by running the script in the containers #138 names, and
# both were of the expensive kind: the error blamed the wrong thing.
#
#   - ubuntu:24.04 ships neither curl nor wget. resolve_latest's wget branch is reached whenever
#     curl is absent, so the message was "could not reach the GitHub API to find the latest
#     release. Pass --version to skip this step" — advice that cannot work, on a machine whose
#     actual problem is that nothing can download anything.
#   - opensuse/leap:15.6 ships no tar. The extract failed *after* a successful download and a
#     verified checksum, reporting "this is a tar that cannot read the archive rather than a
#     corrupt file" — a confident claim about the archive, when tar was not installed.
#
# An error that names the wrong cause is worse than a blunt one: it sends the reader to check the
# release, the network, or the file, and the one thing it does not mention is the missing package.
# So every requirement is checked before the first byte is fetched, all of them are reported
# together rather than one per run, and each carries the package to install. Checking up front also
# means a machine that cannot finish never gets as far as writing to the prefix.
preflight() {
    local missing=()

    command -v curl > /dev/null 2>&1 || command -v wget > /dev/null 2>&1 \
        || missing+=("curl or wget (to download; install either one)")

    # No skip-verification flag exists, so a machine with no digest tool cannot install. That is
    # the intended outcome and it is stated as a refusal rather than reported as a tool error.
    command -v sha256sum > /dev/null 2>&1 || command -v shasum > /dev/null 2>&1 \
        || missing+=("sha256sum or shasum (to verify the download; there is no option to skip verification)")

    command -v tar > /dev/null 2>&1 || missing+=("tar (to unpack the release archive)")

    # gzip separately from tar, because `tar -xzf` does not decompress by itself — it shells out to
    # gzip, and reports the child's failure as its own. opensuse/leap:15.6 has neither by default,
    # and installing only tar there produced `tar: Child returned status 2` plus this script's
    # confident and wrong "a tar that cannot read the archive". That is the same misattribution the
    # missing tar caused, one layer down: the tool that failed is not the tool that is missing.
    command -v gzip > /dev/null 2>&1 || command -v gunzip > /dev/null 2>&1 \
        || missing+=("gzip (tar shells out to it to decompress a .tar.gz, and reports its failure as a tar error)")

    if [ ${#missing[@]} -gt 0 ]; then
        say "this machine is missing something needed to install objectfs:"
        local item
        for item in "${missing[@]}"; do
            say "  - $item"
        done
        say "install what is listed above and run this again. Nothing was downloaded."
        exit 1
    fi
}

# fetch writes a URL to a path, or fails.
#
# curl and wget are both handled because neither is universally present: the minimal Debian and
# Alpine images this is tested against ship wget and not curl, while RHEL-family images ship curl
# and not wget. A one-liner documented with curl that then requires curl to also be the downloader
# would fail on exactly the container it was piped into.
#
# --fail matters more than it looks. Without it curl writes GitHub's 404 HTML page to the output
# file and exits 0, and the install then fails at the checksum with a mismatch — which reads as
# corruption when the real problem is a version that does not exist.
fetch() {
    local url="$1" out="$2"

    if command -v curl > /dev/null 2>&1; then
        curl -fsSL --retry 3 --retry-delay 1 -o "$out" "$url"
    elif command -v wget > /dev/null 2>&1; then
        # shellcheck disable=SC2046 # wget_retry_flags returns a word list on purpose.
        wget -q $(wget_retry_flags) -O "$out" "$url"
    else
        die "neither curl nor wget is available, so nothing can be downloaded"
    fi
}

# wget_retry_flags prints the flags that make wget retry a transient HTTP error, or nothing.
#
# The two branches of fetch were not equivalent, and the difference was measured rather than
# reasoned about: against a server returning two 503s and then a 200, `curl -fsSL --retry 3`
# recovers and `wget -q -O` fails on the first response with exit 8. Same for 429. wget's own
# --tries only covers network-level failures — a response that arrives and carries an error status
# is not a failed attempt as far as wget is concerned, so --tries=20, its default, retries it zero
# times. --retry-on-http-error is what changes that.
#
# This matters because release-asset downloads are unauthenticated: GitHub rate limits them by IP,
# and a shared CI runner shares that IP. A 429 on the .sha256 request is what produced
# "objectfs-linux-amd64.tar.gz exists for v0.13.0 but its .sha256 does not" against a release whose
# .sha256 files were all present — the refusal was correct code reaching a wrong conclusion from a
# fetch that gave up instantly.
#
# 403 is deliberately not in the list. It is an authorization answer, not a transient one, and
# retrying it turns an immediate clear failure into a slow identical one. curl does not retry it
# either, so both branches agree on that.
#
# The flag is probed rather than assumed, because an unrecognised option makes wget exit 2 without
# downloading — which would turn a hardening change into a total failure on an old wget. It has
# existed since wget 1.19 (2017) and every image this is tested against has it; the probe is for
# the machine that is not one of those images.
#
# The match is a `case` on the help text rather than a pipe into grep, so the probe depends on no
# external command but wget itself. That is not hypothetical tidiness: the first harness written for
# this function ran it under a PATH holding only wget, grep was therefore missing, the probe returned
# no flags, and the measurement reported the retry fix as not working. A probe whose failure mode is
# "silently claim the capability is absent" should not have a dependency it does not need.
wget_retry_flags() {
    case "$(wget --help 2>&1)" in
        *--retry-on-http-error*)
            printf '%s' "--tries=4 --waitretry=1 --retry-connrefused --retry-on-http-error=429,500,502,503,504"
            ;;
    esac
}

# resolve_latest returns the tag of the most recent release.
#
# Through the API rather than by following the /latest/download redirect, so the tag can be
# printed and recorded. The parse is a grep for the first "tag_name" rather than a jq invocation,
# because jq is not present on the minimal images this must run on, and adding a dependency to
# read one field of one response is the wrong trade.
resolve_latest() {
    local body auth=()
    body="$(mktemp)"

    # A token is used when one is present and never required. Unauthenticated API calls are rate
    # limited by IP, which is fine for a person and not fine for CI on a shared runner.
    if [ -n "${GITHUB_TOKEN:-}" ]; then
        auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
    fi

    if command -v curl > /dev/null 2>&1; then
        curl -fsSL --retry 3 --retry-delay 1 "${auth[@]}" \
            "https://api.github.com/repos/$REPO/releases/latest" -o "$body" \
            || die "could not reach the GitHub API to find the latest release. Pass --version to skip this step"
    else
        # wget, which preflight has already established is present if curl is not. Reaching this
        # branch with neither installed is what produced the "could not reach the GitHub API"
        # message on a machine that had no downloader at all.
        #
        # wget's header flag has a different shape, and the token is passed the same way. The retry
        # flags are the same ones fetch uses and for the same reason — see wget_retry_flags. The curl
        # branch above has had --retry 3 all along, so without them the two branches disagreed about
        # how many attempts an unauthenticated, IP-rate-limited API call gets.
        local hdr=()
        [ -n "${GITHUB_TOKEN:-}" ] && hdr=(--header="Authorization: Bearer $GITHUB_TOKEN")
        # shellcheck disable=SC2046 # wget_retry_flags returns a word list on purpose.
        wget -q $(wget_retry_flags) "${hdr[@]}" -O "$body" \
            "https://api.github.com/repos/$REPO/releases/latest" \
            || die "could not reach the GitHub API to find the latest release. Pass --version to skip this step"
    fi

    local tag
    tag="$(grep -m1 '"tag_name"' "$body" | sed -e 's/.*"tag_name"[[:space:]]*:[[:space:]]*"//' -e 's/".*//')"
    rm -f "$body"

    [ -n "$tag" ] || die "the GitHub API returned no tag_name, so the latest release could not be identified"

    echo "$tag"
}

# sha256_of prints the SHA-256 of a file.
#
# sha256sum on Linux, shasum -a 256 on macOS. Both are checked for rather than branching on uname,
# since a Linux image can have either and a macOS machine with coreutils installed has both — and
# what matters is which command exists, not which OS this is.
sha256_of() {
    local file="$1"

    if command -v sha256sum > /dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
    elif command -v shasum > /dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
    else
        die "neither sha256sum nor shasum is available, so the download cannot be verified. Refusing to install an unverified binary"
    fi
}

# ----------------------------------------------------------------------------------------------------
# The /etc/fstab mount helper (#533).
#
# Everything below is a second implementation of scripts/postinstall.sh's link_mount_helper and
# scripts/preremove.sh's unlink_mount_helper, and that is a deliberate choice rather than an oversight,
# so it is worth saying why the obvious alternative is not available.
#
# The issue asks for one shared shell function, on the correct reasoning that a second copy is how two
# implementations drift. There is nothing for this file to share it *through*. It is fetched over HTTPS
# and piped straight into bash — that is its documented invocation and the acceptance criterion of the
# issue that created it — so there is no library beside it on disk, and downloading one would mean
# fetching and running a second script that nothing has verified, inside an installer whose entire
# header is about why the download path is not trusted. The package scriptlets cannot source it either:
# dpkg copies maintainer scripts into /var/lib/dpkg/info and runs prerm while the package's own files
# are in whatever state a `--force` left them, and a scriptlet whose unlink step silently no-ops because
# a library file was already gone leaves exactly the dangling /sbin/mount.objectfs that function exists
# to prevent.
#
# So the drift is guarded by a test instead of by a file: internal/config/mount_helper_test.go runs this
# copy and the scriptlet's through one shared table of cases — nothing there, our own link, a foreign
# link, a real file, no /sbin — and asserts both reach the same outcome. That is stronger than a shared
# function would have been, because a shared function is exercised once and these are exercised twice.
# ----------------------------------------------------------------------------------------------------

# may_register decides whether this machine is one where /sbin/mount.objectfs may be created, printing
# the reason it is not. Returns 0 to proceed.
#
# The ownership half is a security property, not tidiness. mount(8) exec's /sbin/mount.objectfs **as
# root**, so a symlink from there into a directory whose owner is not root lets that owner replace the
# binary and run code as root the next time anybody mounts anything. `sudo ./install.sh` with the
# default prefix is precisely that shape — a root-created link into $HOME/.local/bin — and it is the
# most likely way someone reaches this code, which is why it is refused rather than warned about.
#
# `-O` is "owned by the effective uid"; the effective uid is 0 by the time it is evaluated, so it asks
# whether root owns the path. Both the binary and its directory, because a root-owned file inside a
# directory someone else owns can be swapped out by replacing the directory entry.
may_register() {
    local target="$1"

    # Under a scratch root there is neither a real /sbin to protect nor a root to protect it from, and
    # the tests that drive the conservatism rules below run unprivileged. The two checks skipped here
    # are about the real filesystem and are asserted directly instead.
    if [ -n "$ROOT" ]; then
        return 0
    fi

    if [ "$(id -u)" != 0 ]; then
        say "note: not root, so /sbin/$HELPER was not created and /etc/fstab entries do not work yet."
        say "  To register it: sudo ln -s $target /sbin/$HELPER"

        return 1
    fi

    if [ ! -O "$target" ] || [ ! -O "$(dirname "$target")" ]; then
        say "note: $target is not owned by root, so /sbin/$HELPER was not created."
        say "  mount(8) runs /sbin/$HELPER as root, and a link from there into a directory its owner can"
        say "  write would let that owner run code as root. Refusing rather than creating it."
        say "  For a registered system-wide install: sudo ./install.sh --prefix /usr/local"

        return 1
    fi

    return 0
}

# link_mount_helper registers the helper at /sbin/mount.objectfs.
#
# mount(8) resolves a filesystem type it does not know by exec'ing /sbin/mount.$TYPE. That path is
# compiled into util-linux: it is not searched for on PATH, there is no configuration for it, and the
# filename is the entire registration mechanism. So an installed mount.objectfs that is not *at*
# /sbin/mount.objectfs does nothing at all, and an fstab entry fails with "unknown filesystem type
# 'objectfs'" and nothing else.
#
# Every branch returns 0. Failing to register a helper is not a reason to fail an install that has
# already put a working `objectfs` on PATH, and the mount command is where a missing helper should be
# complained about, with the mount point in hand. Same posture as scripts/postinstall.sh, for the same
# reason its header gives.
link_mount_helper() {
    local target="$1"
    local link="$ROOT/sbin/$HELPER"

    if [ ! -x "$target" ]; then
        say "note: $target is not there, so 'mount -t objectfs' and /etc/fstab entries will not work"

        return 0
    fi

    if ! may_register "$target"; then
        return 0
    fi

    if [ ! -d "$ROOT/sbin" ]; then
        say "note: /sbin does not exist, so the mount helper could not be registered"
        say "  Fix: ln -s $target /sbin/$HELPER"

        return 0
    fi

    if [ -L "$link" ]; then
        # Already ours, which is every re-run. Compared rather than replaced with `ln -sf`, so that a
        # link an operator repointed deliberately — or one a package install made, pointing at
        # /usr/bin/mount.objectfs — is reported instead of silently overwritten.
        local current
        current=$(readlink "$link" 2>/dev/null) || current=""

        if [ "$current" = "$target" ]; then
            return 0
        fi

        say "note: /sbin/$HELPER is a symlink to $current, not to $target; leaving it alone"
        say "  Fix, if that is not deliberate: ln -sf $target /sbin/$HELPER"

        return 0
    fi

    if [ -e "$link" ]; then
        say "note: /sbin/$HELPER exists and is not a symlink; leaving it alone"
        say "  Fix, if it is stale: rm /sbin/$HELPER && ln -s $target /sbin/$HELPER"

        return 0
    fi

    if ! ln -s "$target" "$link" 2>/dev/null; then
        say "note: could not create /sbin/$HELPER, so /etc/fstab entries will not work"
        say "  Fix: ln -s $target /sbin/$HELPER"

        return 0
    fi

    say "registered /sbin/$HELPER -> $target"
}

# unlink_mount_helper removes the symlink link_mount_helper created, and only that one.
#
# Only if it is a symlink, and only if it points where this script would have pointed it. A real file,
# or a link to /usr/bin/mount.objectfs that a .deb or .rpm created, is something this script did not
# make — and on a machine with both a package and a tarball install, removing the package's link would
# break an fstab entry that has nothing to do with the prefix being uninstalled. Same rule on the way
# out as on the way in.
#
# Left behind wrongly, the link is a dangling /sbin/mount.objectfs, and an fstab entry that worked
# before the uninstall then fails at `mount -a` with mount(8)'s own "no such file or directory" against
# a helper path — a considerably worse message than "unknown filesystem type".
unlink_mount_helper() {
    local target="$1"
    local link="$ROOT/sbin/$HELPER"

    if [ ! -L "$link" ]; then
        return 0
    fi

    local current
    current=$(readlink "$link" 2>/dev/null) || current=""

    if [ "$current" != "$target" ]; then
        say "note: /sbin/$HELPER points at $current, which this script did not create; leaving it"

        return 0
    fi

    if rm -f "$link" 2>/dev/null; then
        say "removed /sbin/$HELPER"
    else
        say "note: could not remove /sbin/$HELPER, so it is now a dangling link"
        say "  Fix: sudo rm /sbin/$HELPER"
    fi
}

# install_mount_helper puts the helper beside the binary and registers it.
#
# Absent from the archive is not a failure on darwin and is worth saying on Linux, so the two are
# distinguished. .goreleaser.yml builds mount.objectfs for linux only — mount(8)'s helper protocol is
# util-linux's and there is no /sbin/mount.TYPE on macOS — and releases before v0.17.0 carried it in no
# tarball at all, which is what a Linux user installing an older version is seeing.
install_mount_helper() {
    local work="$1" platform="$2"
    local src="$work/$HELPER"
    local dst="$PREFIX/bin/$HELPER"

    if [ "$MOUNT_HELPER" -eq 0 ]; then
        return 0
    fi

    if [ ! -f "$src" ]; then
        case "$platform" in
            linux-*)
                say "note: this tarball carries no $HELPER, so 'mount -t objectfs' and /etc/fstab"
                say "  entries will not work. Releases from v0.17.0 carry it; earlier ones ship it only"
                say "  in the .deb and .rpm."
                ;;
        esac

        return 0
    fi

    chmod 0755 "$src"

    if command -v install > /dev/null 2>&1; then
        install -m 0755 "$src" "$dst" || {
            say "note: could not install $dst, so /etc/fstab entries will not work"

            return 0
        }
    else
        cp -f "$src" "$dst" || {
            say "note: could not install $dst, so /etc/fstab entries will not work"

            return 0
        }
        chmod 0755 "$dst"
    fi

    say "installed $dst"

    link_mount_helper "$dst"
}

# live_mounts prints the mount point of every live ObjectFS filesystem, one per line.
#
# Read from /proc/mounts rather than from `mount`, whose output is prose ("objectfs on /mnt/x type
# fuse.objectfs (rw,...)") and differs between util-linux versions.
#
# The match is on three things, and the obvious single one never fires. internal/fuse sets Subtype "s3"
# alongside FSName "objectfs", so the kernel records the type as `fuse.s3` with a device of `objectfs` —
# a grep for `fuse.objectfs` alone matched nothing on a real mount, which is a defect
# scripts/preremove.sh shipped with for several releases and this copy must not reintroduce.
# internal/config/mount_helper_test.go asserts both scripts still match all three.
live_mounts() {
    local mounts="$ROOT/proc/mounts"
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

        # /proc/mounts escapes space, tab, newline and backslash in octal. \040 is the one that occurs
        # in practice, and a mount point with a space in it printed raw would be split by the caller.
        printf '%b\n' "${point//\\040/\\0040}"
    done < "$mounts"
}

# uninstall removes what this script installed under --prefix, plus the /sbin symlink.
#
# This is the one path here that fails rather than warning, and the reason is the same one
# scripts/preremove.sh gives for being the only scriptlet that does not exit 0 unconditionally. A FUSE
# filesystem whose server binary has been deleted hangs every read against it — `ls` on the mount point
# blocks in the kernel — and the way out is a manual fusermount -u by someone who first has to work out
# that is what happened. `objectfs unmount` is also the only unmount path that reports which methods it
# tried and what is holding the mount open, and it is still installed right up until this function
# deletes it. So a live mount stops the uninstall and names the command.
#
# What it does not remove: caches, configuration, and anything under a path it did not create. An
# uninstaller that deletes a cache directory nobody asked it to delete is the failure preremove.sh
# prints the same paragraph to avoid.
uninstall() {
    local binary="$PREFIX/bin/$PROGRAM"
    local helper="$PREFIX/bin/$HELPER"

    say "uninstalling from $PREFIX/bin"

    local -a live=()
    local point

    while read -r point; do
        [ -n "$point" ] || continue
        live+=("$point")
    done < <(live_mounts)

    if [ "${#live[@]}" -gt 0 ]; then
        say "these ObjectFS filesystems are still mounted:"

        for point in "${live[@]}"; do
            say "  - $point"
        done

        say "Unmount them first. Deleting the binary under a live mount hangs every read against the"
        say "mount point, including 'ls', until someone unmounts it by hand:"

        for point in "${live[@]}"; do
            say "  $PROGRAM unmount $point"
        done

        die "nothing was removed"
    fi

    if [ "$DRY_RUN" -eq 1 ]; then
        say "  would remove: $binary"
        say "  would remove: $helper"
        say "  would remove: /sbin/$HELPER, if it is a symlink to $helper"
        say "dry run, so nothing was removed"

        return 0
    fi

    # Before the binaries, not after. The link's "is it ours" test reads the target path, not the file,
    # so the order does not matter for correctness — but a failure partway through leaves a dangling
    # link if the link goes second, and leaves an unregistered helper if it goes first. The second is
    # the recoverable one.
    unlink_mount_helper "$helper"

    local removed=0 path

    for path in "$helper" "$binary"; do
        if [ ! -e "$path" ]; then
            continue
        fi

        if rm -f "$path" 2>/dev/null; then
            say "removed $path"
            removed=$((removed + 1))
        else
            say "note: could not remove $path"
        fi
    done

    if [ "$removed" -eq 0 ]; then
        say "nothing to remove under $PREFIX/bin; was it installed with a different --prefix?"
    fi

    say "not removed, because this script did not create them: /etc/objectfs, /var/cache/objectfs,"
    say "  ~/.cache/objectfs. Delete them by hand to remove every trace."
}

main() {
    local platform tag asset base

    # Ahead of preflight, because an uninstall downloads nothing, unpacks nothing and verifies nothing:
    # it needs neither a downloader, nor tar, nor a digest tool. Refusing to remove a binary because the
    # machine has no curl would be a check firing on a path it knows nothing about.
    if [ "$UNINSTALL" -eq 1 ]; then
        uninstall

        return 0
    fi

    # Before the platform check, because a missing tar is a fact about this machine that does not
    # depend on which release exists, and before the dry run for a reason worth stating: the point
    # of --dry-run is to report whether this would work. A dry run that says nothing about the
    # missing tar has answered the question wrongly.
    preflight

    platform="$(detect_platform)"

    if [ -n "$VERSION" ]; then
        # Accept 0.13.0 and v0.13.0 both, since the tag carries the v and the version constant
        # does not, and a user reading `objectfs version` output will type it without.
        case "$VERSION" in
            v*) tag="$VERSION" ;;
            *) tag="v$VERSION" ;;
        esac
    else
        say "resolving the latest release"
        tag="$(resolve_latest)"
    fi

    asset="$PROGRAM-$platform.tar.gz"
    base="https://github.com/$REPO/releases/download/$tag"

    say "release $tag, platform $platform"
    say "  archive:  $base/$asset"
    say "  checksum: $base/$asset.sha256"
    say "  install:  $PREFIX/bin/$PROGRAM"

    if [ "$DRY_RUN" -eq 1 ]; then
        say "dry run, so nothing was downloaded"
        return 0
    fi

    # Everything happens in here and is removed on any exit, successful or not. The trap is set
    # before the first download rather than at the top of the script so it cannot fire against an
    # unset variable.
    local work
    work="$(mktemp -d)"
    # shellcheck disable=SC2064 # $work is expanded now on purpose: the trap must name this
    # directory even if a later assignment changes the variable.
    trap "rm -rf '$work'" EXIT

    say "downloading"
    fetch "$base/$asset" "$work/$asset" \
        || die "could not download $asset for $tag. Check that this release exists and publishes a $platform build: https://github.com/$REPO/releases"

    # The checksum is fetched second and its absence is fatal. A release missing a .sha256 is a
    # broken release, not a reason to install without checking — the release workflow writes one
    # per asset in the same step that builds it, so a tarball with no checksum means something
    # went wrong upstream of this script.
    fetch "$base/$asset.sha256" "$work/$asset.sha256" \
        || die "$asset exists for $tag but its .sha256 does not, so the download cannot be verified. Refusing to install an unverified binary"

    # The published file is a bare 64-character digest with no filename and no trailing newline —
    # that is what goreleaser's `checksum: split: true` writes, so `sha256sum -c` cannot read it at
    # all ("no properly formatted checksum lines found"). Comparing the first field directly is what
    # makes this work: it reads a bare digest and a `<hash>  <name>` line identically, so it survived
    # the format changing underneath it. Do not "simplify" this to `-c`. Extracting the field also
    # means one code path for sha256sum and shasum, whose -c output differs.
    #
    # `release.yml` made exactly that mistake on the same files and failed the v0.15.0 publish;
    # `internal/config/release_checksums_test.go` now couples this format to both consumers.
    local want got
    want="$(awk '{print $1}' "$work/$asset.sha256")"
    [ -n "$want" ] || die "the checksum file for $asset is empty or malformed. Refusing to install an unverified binary"

    got="$(sha256_of "$work/$asset")"

    if [ "$want" != "$got" ]; then
        die "checksum mismatch for $asset.
  published: $want
  download:  $got
Nothing was installed. This means the download does not match what the release publishes — a
truncated transfer, a caching proxy that mangled it, or a tampered copy. Retrying may fix the
first two."
    fi

    say "checksum verified"

    # The main binary in the tarball is named for the platform — objectfs-linux-amd64, not objectfs —
    # so the extract and the rename are separate steps and the destination name is spelled out. A
    # `tar -x` straight into the prefix would install a binary the user cannot invoke by name.
    #
    # It is no longer the only member. The Linux tarballs also carry `mount.objectfs`, whose name is
    # already the one it has to be installed under, so it is handled separately by install_mount_helper
    # rather than renamed. Which member exists is read from the extracted directory and not inferred
    # from the platform — an older release's tarball carries only the binary.
    tar -xzf "$work/$asset" -C "$work" \
        || die "could not extract $asset. The download passed its checksum, so this is a tar that cannot read the archive rather than a corrupt file"

    local binary="$work/$PROGRAM-$platform"
    [ -f "$binary" ] || die "$asset does not contain $PROGRAM-$platform. The release layout has changed and this script needs updating"

    mkdir -p "$PREFIX/bin" || die "could not create $PREFIX/bin. Pass --prefix to install somewhere writable"

    chmod 0755 "$binary"

    # install(1) where it exists, because it replaces the destination atomically-ish and does not
    # trip over a running binary; cp -f as the fallback for images that lack it. Overwriting an
    # existing objectfs is the intended behaviour on a re-run, which is what makes this idempotent.
    if command -v install > /dev/null 2>&1; then
        install -m 0755 "$binary" "$PREFIX/bin/$PROGRAM" \
            || die "could not install to $PREFIX/bin/$PROGRAM"
    else
        cp -f "$binary" "$PREFIX/bin/$PROGRAM" || die "could not install to $PREFIX/bin/$PROGRAM"
        chmod 0755 "$PREFIX/bin/$PROGRAM"
    fi

    say "installed $PREFIX/bin/$PROGRAM"

    # After the main binary and before the PATH note, so that a failure to register the helper cannot
    # prevent the thing a user came for from being reported as installed.
    install_mount_helper "$work" "$platform"

    # And say so when it is not reachable. An installer that succeeds and leaves the user with
    # "command not found" has produced the same experience as one that failed, minus the
    # explanation — this is the same failure the modulefiles refuse to load rather than cause.
    case ":$PATH:" in
        *":$PREFIX/bin:"*) ;;
        *)
            say "note: $PREFIX/bin is not on PATH. Add it:"
            say "  export PATH=\"$PREFIX/bin:\$PATH\""
            ;;
    esac

    # Run it. The version it prints is the authority on what was installed, and it is also the
    # only check here that the binary actually executes on this machine — a wrong-architecture
    # download that somehow passed its checksum surfaces as an exec format error right here,
    # rather than the first time the user tries to mount something.
    if "$PREFIX/bin/$PROGRAM" --version > /dev/null 2>&1; then
        say "$("$PREFIX/bin/$PROGRAM" --version 2>&1 | head -1)"
    else
        die "$PREFIX/bin/$PROGRAM was installed but does not run. This is usually a binary built for a different architecture than this machine"
    fi

    say "next: objectfs mount s3://your-bucket /mnt/point   (see https://github.com/$REPO#quick-start)"
}

# OBJECTFS_INSTALL_SOURCE_ONLY makes this file sourceable, so that internal/config's tests can call
# link_mount_helper, unlink_mount_helper, live_mounts and may_register directly.
#
# It exists because those four are a second copy of logic scripts/postinstall.sh and
# scripts/preremove.sh also carry — see the banner above may_register for why there is no shared file
# to source — and the guard against the two drifting is running both through one table of cases. There
# is no way to reach this script's copy through `main`: the link is made after a download, a checksum
# and an extract, none of which a unit test can or should perform.
#
# Empty for every user, and the variable is read rather than the invocation being restructured for the
# same reason OBJECTFS_ROOT is: the test then exercises this exact file rather than a copy of it.
if [ -z "${OBJECTFS_INSTALL_SOURCE_ONLY:-}" ]; then
    main "$@"
fi
