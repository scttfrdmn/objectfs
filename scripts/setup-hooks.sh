#!/bin/bash
# Install the pre-commit hooks used for ObjectFS development.
#
# This is the first command CONTRIBUTING.md tells a new contributor to run, and until now it failed
# on any current macOS or Debian host. Five separate defects, recorded because each one is a shape
# worth not reintroducing:
#
#   1. `pip3 install pre-commit` ran first and, on a Homebrew or Debian Python, fails with
#      `error: externally-managed-environment` (PEP 668). Because the installer was an if/elif
#      chain testing only whether each *command exists*, a failing pip3 never fell through to the
#      `brew` branch that would have worked. Now each method is tried until one succeeds, and pipx
#      — the tool PEP 668's own error message recommends — is preferred.
#   2. The script declared `set -euo pipefail` and still exited 0 after installing nothing, because
#      the failure happened inside a condition. Every install path now verifies the command is
#      actually on PATH afterwards and fails loudly if it is not.
#   3. It installed gosec from `github.com/securecodewarrior/gosec`, which is a 404. The real module
#      is `github.com/securego/gosec`, which is what .github/workflows/security.yml uses.
#   4. It pinned golangci-lint v1.55.2, while .golangci.yml is a `version: "2"` schema that only
#      v2.x can parse. So a contributor who ran this got a linter that could not read the repo's own
#      config, and a lint failure that looks like their fault.
#   5. It wrote a .golangci.yml if one was absent, containing linters removed from golangci-lint
#      years ago (deadcode, golint, interfacer, structcheck, varcheck). A committed .golangci.yml
#      now exists, so generating one is not just unnecessary but actively wrong: this script would
#      have been the thing that overwrote a real config with an unusable one.
#
# It also no longer overwrites .git/hooks/pre-commit with a hand-rolled wrapper. `pre-commit install`
# manages that file, and the wrapper blocked any commit touching a line matching `fmt.Print` or
# `TODO` — including in a string literal or a comment explaining why a TODO is deliberate.

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

print_status()  { echo -e "${BLUE}==>${NC} $1"; }
print_success() { echo -e "${GREEN}ok${NC}  $1"; }
print_warning() { echo -e "${YELLOW}warn${NC} $1"; }
print_error()   { echo -e "${RED}error${NC} $1" >&2; }

# The golangci-lint series that can read a `version: "2"` config. Keep in step with .golangci.yml
# and with the version ci.yml installs; a mismatch here hands contributors a lint failure the CI
# they are trying to satisfy does not have.
GOLANGCI_VERSION="v2.12.2"

if [ ! -d .git ]; then
    print_error "not in a git repository — run this from the ObjectFS root directory"
    exit 1
fi

print_status "setting up ObjectFS development hooks"

# --- pre-commit -------------------------------------------------------------------------------
#
# Tried in order of how well each coexists with a system Python. pipx and brew install into their
# own environments; plain pip is last because it is the one PEP 668 refuses.
install_pre_commit() {
    if command -v pipx &> /dev/null; then
        print_status "installing pre-commit with pipx"
        pipx install pre-commit && return 0
    fi
    if command -v brew &> /dev/null; then
        print_status "installing pre-commit with brew"
        brew install pre-commit && return 0
    fi
    if command -v apt-get &> /dev/null; then
        print_status "installing pre-commit with apt-get (requires sudo)"
        sudo apt-get update && sudo apt-get install -y pre-commit && return 0
    fi
    if command -v pip3 &> /dev/null; then
        print_status "installing pre-commit with pip3 --user"
        pip3 install --user pre-commit && return 0
    fi
    return 1
}

if ! command -v pre-commit &> /dev/null; then
    print_warning "pre-commit not found"

    # `|| true` so a failing installer reaches the explicit check below rather than tripping
    # `set -e` with no explanation of what to do next.
    install_pre_commit || true

    if ! command -v pre-commit &> /dev/null; then
        print_error "could not install pre-commit automatically. Install it one of these ways:"
        print_error "  pipx install pre-commit      # preferred: isolated, no PEP 668 conflict"
        print_error "  brew install pre-commit"
        print_error "  apt-get install pre-commit"
        print_error "and re-run this script."
        exit 1
    fi
fi
print_success "pre-commit $(pre-commit --version 2>/dev/null | awk '{print $2}')"

# --- golangci-lint ----------------------------------------------------------------------------
#
# Version-checked, not just presence-checked: a v1 binary already on PATH cannot parse this repo's
# config, so finding *a* golangci-lint is not enough.
install_golangci_lint() {
    local gobin
    gobin="$(go env GOPATH)/bin"
    print_status "installing golangci-lint $GOLANGCI_VERSION to $gobin"
    curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
        | sh -s -- -b "$gobin" "$GOLANGCI_VERSION"
    export PATH="$PATH:$gobin"
}

golangci_major() {
    command -v golangci-lint &> /dev/null || return 1
    golangci-lint --version 2>/dev/null | grep -oE 'version v?[0-9]+' | grep -oE '[0-9]+$'
}

if ! command -v golangci-lint &> /dev/null; then
    print_warning "golangci-lint not found"
    install_golangci_lint
elif [ "$(golangci_major || echo 0)" -lt 2 ]; then
    print_warning "golangci-lint $(golangci-lint --version | awk '{print $4}') cannot read .golangci.yml (schema version 2)"
    install_golangci_lint
fi

if ! command -v golangci-lint &> /dev/null; then
    print_error "golangci-lint is still not on PATH. Add \$(go env GOPATH)/bin to your PATH."
    exit 1
fi
print_success "golangci-lint $(golangci-lint --version 2>/dev/null | awk '{print $4}')"

# Prove it can actually parse the config, which is the property that matters and the one a version
# check only approximates.
if ! golangci-lint config verify &> /dev/null; then
    print_warning "golangci-lint cannot verify .golangci.yml — 'golangci-lint config verify' for details"
fi

# --- gosec ------------------------------------------------------------------------------------
if ! command -v gosec &> /dev/null; then
    print_warning "gosec not found, installing"
    go install github.com/securego/gosec/v2/cmd/gosec@latest
fi

if command -v gosec &> /dev/null; then
    print_success "gosec $(gosec --version 2>/dev/null | awk '/Version/{print $2}')"
else
    print_warning "gosec is not on PATH — add \$(go env GOPATH)/bin to your PATH (the security hook will skip)"
fi

# --- hooks ------------------------------------------------------------------------------------
print_status "installing git hooks"
pre-commit install
pre-commit install --hook-type commit-msg || print_warning "commit-msg hook not installed (optional)"
print_success "hooks installed"

print_status "running the hooks over all files — the first run builds environments and is slow"
if pre-commit run --all-files; then
    print_success "all hooks passed"
else
    print_warning "some hooks reported changes or failures"
    print_warning "hooks that reformat files leave the result staged — review, re-stage, and commit again"
fi

echo
print_success "setup complete"
cat <<'EOF'

  Hooks now run on every commit. Useful commands:

    pre-commit run --all-files     run every hook over the whole tree
    pre-commit autoupdate          bump hook versions
    git commit --no-verify         skip hooks (CI will still run them)

  What runs, and where it is configured:

    .pre-commit-config.yaml   whitespace, YAML/JSON, gofmt, go mod tidy, go build, golangci-lint
    .golangci.yml             lint rules — schema version 2, needs golangci-lint v2.x
    .coverage-floors          per-package coverage floors, enforced by scripts/coverage-gate.sh

EOF
