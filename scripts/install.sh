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
# hosting decision, so it is what this installs. When a package repository exists, this script
# gains a branch that prefers it; the download path stays, because it is the one that works on a
# machine with no root, no package manager entry, and no network path to a third-party repo —
# which describes a large share of the HPC login nodes this project is for.
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

set -euo pipefail

readonly REPO="scttfrdmn/objectfs"
readonly PROGRAM="objectfs"

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
  --dry-run           Report what would happen; download and verify nothing
  -h, --help          This message

Environment:
  VERSION, PREFIX     Same as the flags above
  GITHUB_TOKEN        Sent as a bearer token when resolving the latest release, which raises the
                      API rate limit. Never required, and never sent to the download host.

The checksum is always verified and there is no option to skip it. Note what that does and does
not establish: the .sha256 travels the same channel as the tarball, so a mismatch means corruption
or a tampered mirror, not necessarily an authentic release.
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
        wget -q -O "$out" "$url"
    else
        die "neither curl nor wget is available, so nothing can be downloaded"
    fi
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
        # wget's header flag has a different shape, and the token is passed the same way.
        local hdr=()
        [ -n "${GITHUB_TOKEN:-}" ] && hdr=(--header="Authorization: Bearer $GITHUB_TOKEN")
        wget -q "${hdr[@]}" -O "$body" \
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

main() {
    local platform tag asset base

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

    # The published file is `<hash>  <name>`, which is sha256sum -c's format — but -c resolves the
    # name relative to the working directory, so comparing the hash field directly is what makes
    # this work regardless of where the file was staged. Extracting the field also means one code
    # path for sha256sum and shasum, whose -c output differs.
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

    # The tarball holds one file, named for the platform — objectfs-linux-amd64, not objectfs — so
    # the extract and the rename are separate steps and the destination name is spelled out. A
    # `tar -x` straight into the prefix would install a binary the user cannot invoke by name.
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

main "$@"
