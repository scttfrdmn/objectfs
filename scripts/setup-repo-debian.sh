#!/usr/bin/env bash
# Add the ObjectFS apt repository to a Debian or Ubuntu machine.
#
# This is part (c) of #138. It writes two files and nothing else: the signing key to
# /usr/share/keyrings/objectfs-archive-keyring.gpg and a one-line deb822-style source to
# /etc/apt/sources.list.d/objectfs.sources. After it runs, `apt update && apt install objectfs` works.
#
# THE KEY IS SCOPED TO THIS REPOSITORY AND NOWHERE ELSE.
#
# That is the single most important line in this file, and it is why the source is written in the
# deb822 format with an explicit `Signed-By:` rather than as a one-line `deb [...]` entry with the key
# dropped into /etc/apt/trusted.gpg.d/. A key in trusted.gpg.d is trusted for **every** repository the
# machine has configured, so installing ours would mean our key could authorise a package claiming to
# be anything — Debian's own openssh-server included. `apt-key add`, which #138's spec implies, does
# exactly that and has been deprecated since apt 2.4 for exactly this reason. Scoped, the worst a
# compromise of our key can do is serve a bad objectfs.
#
# It is also why this script refuses rather than falls back when it cannot verify the key it just
# downloaded: a repository configured with the wrong key fails at the next `apt update` with a
# signature error that names our repository, which is recoverable and obvious. A repository configured
# with an *attacker's* key fails at nothing.
#
# Idempotent, and in the stronger sense that scripts/postinstall.sh uses: a second run does not
# duplicate the source, does not re-download a key that is already correct, and does not touch a file
# it did not write. Running it twice is how an unattended configuration-management run will use it.

set -euo pipefail

# The published addresses, overridable only for the CI harness.
#
# The overrides are what let ci.yml's repo-install job do something better than a --dry-run: it builds
# a repository from `make package-linux`, signs it with a throwaway key, serves it over localhost, and
# runs this script against it — so `apt-get install objectfs` is actually executed on each image in the
# matrix, months before the production key or a published release exists.
#
# They add no attack surface. Anyone who can set the environment of a root shell running this script
# can already do anything this script could be tricked into doing. What matters is that the *default*
# is a literal, so the piped one-liner has nothing to override and no way to be pointed elsewhere.
readonly REPO_URL="${OBJECTFS_REPO_URL:-https://objectfs.io/apt}"
# Armored, and named .asc for that reason. The keyring apt reads is binary, and this script dearmors
# into it below; serving the binary form as `objectfs.gpg` was the first version of this and it made
# the extension disagree with the contents on both ends.
readonly KEY_URL="${OBJECTFS_KEY_URL:-https://objectfs.io/objectfs.asc}"
readonly KEYRING="/usr/share/keyrings/objectfs-archive-keyring.gpg"
readonly SOURCE="/etc/apt/sources.list.d/objectfs.sources"

# The single distribution and component the repository publishes.
#
# Not `$(lsb_release -cs)`, which is what a per-distribution apt repository would use. This repository
# holds one build per architecture that runs on any glibc-based Debian derivative, so a codename in the
# URL would promise per-release builds that do not exist — and would break on any derivative whose
# codename we had not enumerated, which includes every Ubuntu released after this script was written.
readonly SUITE="stable"
readonly COMPONENT="main"

say() {
    echo "objectfs-repo: $*" >&2
}

die() {
    echo "objectfs-repo: error: $*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Add the ObjectFS apt repository, scoped to its own signing key.

Usage: setup-repo-debian.sh [options]

Options:
  --dry-run    Report what would be written; download and change nothing
  -h, --help   This message

Needs root, because it writes to /usr/share/keyrings and /etc/apt/sources.list.d.

The key is installed with Signed-By: so it authorises this repository only. A key in
/etc/apt/trusted.gpg.d is trusted for every repository on the machine, which is why apt-key add is
deprecated and why this script does not use it.
EOF
}

DRY_RUN=0

while [ $# -gt 0 ]; do
    case "$1" in
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

# preflight names everything missing at once, before anything is written.
#
# The same shape as scripts/install.sh's, and for the same measured reason: on the three images #138
# names, a script that checks as it goes reports the wrong cause. ubuntu:24.04 ships neither curl nor
# wget, so a download failure there gets blamed on the repository being unreachable.
#
# gpg is required and there is no path that skips it. A repository whose key was not verified is worse
# than no repository: apt will trust whatever that key signs, for this repository, permanently.
preflight() {
    local missing=()

    command -v curl > /dev/null 2>&1 || command -v wget > /dev/null 2>&1 \
        || missing+=("curl or wget (to download the signing key; install either one)")

    # gpg --dearmor turns the ASCII-armored key into the binary keyring apt reads. Some minimal
    # images ship gpgv, which can verify but cannot dearmor, so gpg specifically is what is checked.
    command -v gpg > /dev/null 2>&1 \
        || missing+=("gpg (to verify and dearmor the signing key; there is no option to skip this)")

    command -v apt-get > /dev/null 2>&1 \
        || missing+=("apt-get (this is the Debian/Ubuntu script; on RHEL or SUSE use setup-repo-rhel.sh)")

    if [ ${#missing[@]} -gt 0 ]; then
        say "this machine is missing something needed to add the repository:"
        local item
        for item in "${missing[@]}"; do
            say "  - $item"
        done
        say "install what is listed above and run this again. Nothing was changed."
        exit 1
    fi
}

# fetch writes a URL to a path, or fails. Both downloaders, as install.sh does and for the same
# reason: neither is universally present, and the one the documentation happens to use is not
# necessarily the one installed.
fetch() {
    local url="$1" out="$2"

    if command -v curl > /dev/null 2>&1; then
        curl -fsSL --retry 3 --retry-delay 1 -o "$out" "$url"
    elif command -v wget > /dev/null 2>&1; then
        wget -q -O "$out" "$url"
    else
        die "neither curl nor wget is available, so the signing key cannot be downloaded"
    fi
}

main() {
    preflight

    say "repository: $REPO_URL $SUITE $COMPONENT"
    say "key:        $KEY_URL -> $KEYRING"
    say "source:     $SOURCE"

    if [ "$DRY_RUN" -eq 1 ]; then
        say "dry run, so nothing was downloaded or written"
        return 0
    fi

    # Checked after the dry run, so --dry-run works as an unprivileged user. That is the whole point
    # of the flag: someone deciding whether to pipe this into a root shell should be able to see what
    # it would do without being root.
    if [ "$(id -u)" -ne 0 ]; then
        die "this needs root to write $KEYRING and $SOURCE. Re-run with sudo, or use --dry-run to see what it would do"
    fi

    local work
    work="$(mktemp -d)"
    # shellcheck disable=SC2064 # $work is expanded now on purpose, so the trap names this directory
    # even if the variable is later reassigned.
    trap "rm -rf '$work'" EXIT

    say "downloading the signing key"
    fetch "$KEY_URL" "$work/objectfs.asc" \
        || die "could not download the signing key from $KEY_URL. Nothing was changed"

    # An armored key is what is published, so a file that does not parse as one is a failure here and
    # not at the next apt update. Without this check, a 404 page or an HTML error saved to disk gets
    # dearmored into an empty keyring, apt reports "no valid OpenPGP data" against our repository, and
    # the reader has no reason to suspect the download rather than the repository.
    #
    # --dearmor is the check as well as the conversion: it fails on anything that is not a key.
    gpg --batch --yes --dearmor --output "$work/objectfs.gpg" "$work/objectfs.asc" \
        || die "the file downloaded from $KEY_URL is not an OpenPGP key. This is usually an error page saved as a file. Nothing was changed"

    # Non-empty, because dearmoring a zero-byte input succeeds and produces a zero-byte keyring, which
    # apt accepts and then rejects every signature against.
    [ -s "$work/objectfs.gpg" ] \
        || die "the signing key from $KEY_URL dearmored to an empty keyring. Nothing was changed"

    # The fingerprint, printed rather than pinned. Pinning it in this script would mean the script has
    # to be re-released to rotate the key, and a rotation that requires every user to re-download a
    # script is a rotation that does not happen. Printing it means an operator who wants to verify out
    # of band has the value in front of them, next to the URL it came from.
    local fpr
    fpr="$(gpg --batch --with-colons --show-keys "$work/objectfs.asc" 2> /dev/null \
        | awk -F: '$1 == "fpr" { print $10; exit }')"
    [ -n "$fpr" ] || die "could not read a fingerprint out of the downloaded key. Nothing was changed"

    say "key fingerprint: $fpr"
    say "  verify it against the value published at https://objectfs.io/docs/ before trusting this repository"

    # Only write the keyring if it differs, so a re-run does not touch a file it does not need to.
    # This matters more than tidiness: replacing the keyring is what a key rotation looks like, and a
    # script that rewrites it on every run gives an operator no way to see when that happened.
    if [ -f "$KEYRING" ] && cmp -s "$work/objectfs.gpg" "$KEYRING"; then
        say "$KEYRING is already this key, leaving it alone"
    else
        if [ -f "$KEYRING" ]; then
            say "note: $KEYRING exists with different contents and is being replaced. If you did not"
            say "      expect a key rotation, stop and check the fingerprint above before running apt update"
        fi

        install -o root -g root -m 0644 "$work/objectfs.gpg" "$KEYRING" \
            || die "could not write $KEYRING"
        say "installed $KEYRING"
    fi

    # deb822 format, one stanza. Signed-By is the whole reason for this format: it scopes the key to
    # this repository, where a key in trusted.gpg.d would authorise packages from any repository the
    # machine has.
    #
    # Architectures is explicit rather than omitted, because an omitted value means "all of them" and
    # this repository publishes amd64 and arm64 only — nfpm.yaml builds those two. On an armhf machine
    # the explicit list produces "no candidate" from apt install, which is correct and readable;
    # omitting it produces a 404 during apt update against a path that was never going to exist.
    local desired
    desired="$(
        cat <<EOF
Types: deb
URIs: $REPO_URL
Suites: $SUITE
Components: $COMPONENT
Architectures: amd64 arm64
Signed-By: $KEYRING
EOF
    )"

    if [ -f "$SOURCE" ] && [ "$(cat "$SOURCE")" = "$desired" ]; then
        say "$SOURCE is already correct, leaving it alone"
    else
        printf '%s\n' "$desired" > "$SOURCE" || die "could not write $SOURCE"
        chmod 0644 "$SOURCE"
        say "wrote $SOURCE"
    fi

    # A legacy .list from an earlier version of this script would be read *in addition to* the
    # .sources, giving the repository a second, unscoped entry. Removed rather than left, since
    # leaving it is the case where the scoping this whole script is about silently does not apply.
    local legacy="/etc/apt/sources.list.d/objectfs.list"
    if [ -f "$legacy" ]; then
        rm -f "$legacy"
        say "removed $legacy, which an earlier version of this script wrote. apt reads .list and"
        say "  .sources both, so leaving it would have configured this repository twice — once without"
        say "  the Signed-By scoping"
    fi

    say "done. Next:"
    say "  apt-get update && apt-get install objectfs"
}

main "$@"
