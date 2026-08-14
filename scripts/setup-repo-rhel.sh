#!/usr/bin/env bash
# Add the ObjectFS rpm repository to a RHEL-family or SUSE machine.
#
# This is part (c) of #138, the rpm half. It imports the signing key into rpm's keyring and writes one
# .repo file. After it runs, `dnf install objectfs` (or `zypper install objectfs`) works.
#
# THE KEY IS IMPORTED GLOBALLY, AND THAT IS NOT A CHOICE THIS SCRIPT MAKES.
#
# setup-repo-debian.sh can scope its key to one repository with apt's `Signed-By:`, and it does. rpm
# has no equivalent: `rpm --import` adds the key to the single system-wide keyring in the rpm database,
# where it is a valid signer for *any* package rpm is asked to install, from any repository. There is no
# per-repository keyring, and `gpgkey=` in the stanza below does not create one — it only tells dnf
# where to fetch the key it will then import globally.
#
# So this is said out loud rather than papered over: on a RHEL-family machine, trusting this repository
# means trusting this key for everything. That is the same deal every third-party rpm repository offers
# and it is why the fingerprint is printed before the import, and why the import is the one step this
# script will not do quietly. The corresponding note is in the docs alongside the one-liner, not only
# here, because the person deciding is usually reading the docs and not the script.
#
# Idempotent in scripts/postinstall.sh's sense: a re-run does not re-import a key rpm already has, does
# not rewrite an already-correct .repo file, and does not touch a file it did not write.

set -euo pipefail

# The published addresses, overridable only for the CI harness — see setup-repo-debian.sh's note on
# why that is not a hole. The default is a literal, so the piped one-liner cannot be redirected.
readonly REPO_URL="${OBJECTFS_REPO_URL:-https://objectfs.io/yum}"
readonly KEY_URL="${OBJECTFS_KEY_URL:-https://objectfs.io/objectfs.asc}"
readonly KEY_PATH="/etc/pki/rpm-gpg/RPM-GPG-KEY-objectfs"

say() {
    echo "objectfs-repo: $*" >&2
}

die() {
    echo "objectfs-repo: error: $*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Add the ObjectFS rpm repository.

Usage: setup-repo-rhel.sh [options]

Options:
  --dry-run    Report what would be written; download, import and change nothing
  -h, --help   This message

Needs root, because it imports a key into the rpm database and writes to the repo directory.

Note that rpm has no per-repository keyring. Importing this key trusts it as a package signer
system-wide, for every repository, not only for this one. apt can scope a key with Signed-By and
setup-repo-debian.sh does; rpm cannot, and this script does not pretend otherwise.
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

# repo_dir picks the directory the local package manager actually reads.
#
# Not a constant, because /etc/yum.repos.d is the wrong answer on the SUSE image in #138's matrix.
# zypper reads /etc/zypp/repos.d and does *not* read /etc/yum.repos.d, so a script that writes only the
# yum path succeeds on opensuse/leap and configures nothing: `zypper install objectfs` then reports the
# package as not found and the .repo file sits there looking correct. That is the failure #421 kept
# producing — a clean run on an image where nothing took effect.
repo_dir() {
    if command -v zypper > /dev/null 2>&1 && [ -d /etc/zypp/repos.d ]; then
        echo /etc/zypp/repos.d
    else
        echo /etc/yum.repos.d
    fi
}

# preflight names everything missing at once, before anything is written or imported.
preflight() {
    local missing=()

    command -v curl > /dev/null 2>&1 || command -v wget > /dev/null 2>&1 \
        || missing+=("curl or wget (to download the signing key; install either one)")

    # gpg is needed to read the fingerprint out of the key before importing it. rpm --import would
    # accept the key without it, and that is the point: an import whose fingerprint nobody could see is
    # exactly the step this script refuses to take silently.
    command -v gpg > /dev/null 2>&1 \
        || missing+=("gpg (to read the key's fingerprint before importing it; there is no option to skip this)")

    command -v rpm > /dev/null 2>&1 \
        || missing+=("rpm (to import the signing key)")

    command -v dnf > /dev/null 2>&1 || command -v yum > /dev/null 2>&1 \
        || command -v zypper > /dev/null 2>&1 \
        || missing+=("dnf, yum or zypper (this is the rpm script; on Debian or Ubuntu use setup-repo-debian.sh)")

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

    local dir
    dir="$(repo_dir)"
    local repo_file="$dir/objectfs.repo"

    say "repository: $REPO_URL"
    say "key:        $KEY_URL -> $KEY_PATH, and imported into rpm's system-wide keyring"
    say "repo file:  $repo_file"

    if [ "$DRY_RUN" -eq 1 ]; then
        say "dry run, so nothing was downloaded, imported or written"
        return 0
    fi

    if [ "$(id -u)" -ne 0 ]; then
        die "this needs root to import the key and write $repo_file. Re-run with sudo, or use --dry-run to see what it would do"
    fi

    local work
    work="$(mktemp -d)"
    # shellcheck disable=SC2064 # $work is expanded now on purpose, so the trap names this directory
    # even if the variable is later reassigned.
    trap "rm -rf '$work'" EXIT

    say "downloading the signing key"
    fetch "$KEY_URL" "$work/objectfs.asc" \
        || die "could not download the signing key from $KEY_URL. Nothing was changed"

    # --show-keys parses; it does not import. A file that is not a key fails here, before rpm has been
    # asked to trust anything. Without this, an error page saved as a file reaches `rpm --import`, which
    # reports "error: <file>: key 1 import failed" — a message that says nothing about the download
    # being HTML.
    local fpr
    fpr="$(gpg --batch --with-colons --show-keys "$work/objectfs.asc" 2> /dev/null \
        | awk -F: '$1 == "fpr" { print $10; exit }')" \
        || die "the file downloaded from $KEY_URL is not an OpenPGP key. Nothing was changed"
    [ -n "$fpr" ] \
        || die "the file downloaded from $KEY_URL is not an OpenPGP key — no fingerprint in it. This is usually an error page saved as a file. Nothing was changed"

    say "key fingerprint: $fpr"
    say "  this key is about to become a trusted package signer for this machine, for every"
    say "  repository and not only ObjectFS's — rpm has no per-repository keyring. Verify the"
    say "  fingerprint against the value published at https://objectfs.io/docs/ if that matters to you"

    # The key on disk, so gpgkey= can name a local file. dnf fetching the key over HTTPS on every
    # refresh would work, but a local path means the key rpm verifies against is the one whose
    # fingerprint was printed above — not whatever the URL serves at the next refresh.
    install -d -m 0755 /etc/pki/rpm-gpg
    if [ -f "$KEY_PATH" ] && cmp -s "$work/objectfs.asc" "$KEY_PATH"; then
        say "$KEY_PATH is already this key, leaving it alone"
    else
        if [ -f "$KEY_PATH" ]; then
            say "note: $KEY_PATH exists with different contents and is being replaced. If you did not"
            say "      expect a key rotation, stop and check the fingerprint above"
        fi

        install -o root -g root -m 0644 "$work/objectfs.asc" "$KEY_PATH" \
            || die "could not write $KEY_PATH"
        say "installed $KEY_PATH"
    fi

    # Imported only if rpm does not already have it. `rpm --import` is idempotent in effect, but it is
    # also the step with the widest blast radius in this script, so it runs when it has something to do
    # and not otherwise — and when it does run, that is visible in the output.
    #
    # rpm records imported keys as packages named gpg-pubkey-<keyid>-<timestamp>, where keyid is the
    # low 8 hex digits of the fingerprint, lowercased.
    local keyid
    keyid="$(printf '%s' "${fpr: -8}" | tr 'A-F' 'a-f')"

    if rpm -q "gpg-pubkey-$keyid" > /dev/null 2>&1; then
        say "rpm already trusts this key (gpg-pubkey-$keyid), leaving the keyring alone"
    else
        rpm --import "$KEY_PATH" || die "could not import $KEY_PATH into rpm's keyring"
        say "imported the key into rpm's keyring as gpg-pubkey-$keyid"
    fi

    # The stanza. Two gpg settings, both on, and they check different things:
    #
    #   gpgcheck=1       every package's signature is verified before installation
    #   repo_gpgcheck=1  the repository's own metadata (repomd.xml) is verified against repomd.xml.asc
    #
    # repo_gpgcheck is off by default and is the one worth turning on deliberately. Without it, package
    # signatures are still checked, but the *index* is not: whoever can modify the metadata can hide a
    # package, pin an old vulnerable version as the newest, or point a filename at different content.
    # pages.yml signs repomd.xml, so this is a setting the repository can actually honour — and if the
    # signing key is ever absent at deploy time, no repository is published at all rather than an
    # unsigned one, so this line cannot start silently passing on nothing.
    local desired
    desired="$(
        cat <<EOF
[objectfs]
name=ObjectFS
baseurl=$REPO_URL
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file://$KEY_PATH
EOF
    )"

    if [ -f "$repo_file" ] && [ "$(cat "$repo_file")" = "$desired" ]; then
        say "$repo_file is already correct, leaving it alone"
    else
        printf '%s\n' "$desired" > "$repo_file" || die "could not write $repo_file"
        chmod 0644 "$repo_file"
        say "wrote $repo_file"
    fi

    # A .repo in the *other* directory would be read by nothing on this machine, which is harmless
    # until the machine gains the other package manager. Reported rather than removed: unlike the
    # legacy .list in the apt script, this file is not a second unscoped entry, and removing a file
    # this script did not write in this directory is not its business.
    local other
    if [ "$dir" = /etc/zypp/repos.d ]; then
        other=/etc/yum.repos.d/objectfs.repo
    else
        other=/etc/zypp/repos.d/objectfs.repo
    fi

    if [ -f "$other" ]; then
        say "note: $other also exists. This machine reads $dir, so that copy has no effect —"
        say "  it is left alone rather than removed, but it will drift"
    fi

    say "done. Next:"
    if command -v zypper > /dev/null 2>&1 && [ "$dir" = /etc/zypp/repos.d ]; then
        say "  zypper refresh && zypper install objectfs"
    elif command -v dnf > /dev/null 2>&1; then
        say "  dnf install objectfs"
    else
        say "  yum install objectfs"
    fi
}

main "$@"
