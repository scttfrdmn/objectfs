#!/usr/bin/env bash
#
# coverage-gate.sh — enforce a per-package statement-coverage floor.
#
# Usage:
#   ./scripts/coverage-gate.sh coverage.out .coverage-floors
#
# Why per-package and not repo-wide. A repo-wide number lets a well-tested package pay for an
# untested one, and the aggregate is the figure that looks acceptable: this repo sits near 70%
# overall while internal/fuse — the layer every POSIX operation passes through — is at 17.7%.
# Averaging those two produces a number that hides exactly the thing worth knowing.
#
# Why a floors file and not one global threshold. A single threshold across a repo with a real
# spread has to be set at the floor of the worst package, at which point it gates nothing. A file
# of per-package floors starts at what each package measures today, so the gate is green on the
# commit that introduces it, and every floor becomes a ratchet: raising one is a deliberate act
# and lowering one has to be defended in a diff.
#
# A package with no entry has no floor. That is deliberate — a package added tomorrow should not
# fail CI for not being in a list nobody told its author about — but `--report-unlisted` names
# them so the omission is visible rather than silent.

set -euo pipefail

profile="${1:-coverage.out}"
floors="${2:-.coverage-floors}"

if [[ ! -f "$profile" ]]; then
  echo "coverage-gate: no coverage profile at $profile" >&2
  exit 2
fi

if [[ ! -f "$floors" ]]; then
  echo "coverage-gate: no floors file at $floors" >&2
  exit 2
fi

# go tool cover reports per-function; per-package is the sum of statements, not the mean of
# percentages. Weighting by statement count matters: a package with one fully-covered one-line
# function and one uncovered hundred-line function is not at 50%.
#
# The profile's format is:  <import path>/<file>:<line>.<col>,<line>.<col> <statements> <count>
measured="$(mktemp)"
trap 'rm -f "$measured"' EXIT

awk '
  NR == 1 && $0 ~ /^mode:/ { next }
  {
    split($1, loc, ":")
    path = loc[1]
    # Strip the trailing filename to get the package import path.
    sub(/\/[^\/]+$/, "", path)
    # And the module prefix, so the floors file reads as repo-relative paths.
    sub(/^github\.com\/objectfs\/objectfs\//, "", path)
    sub(/^github\.com\/objectfs\/objectfs$/, ".", path)

    total[path] += $2
    if ($3 > 0) covered[path] += $2
  }
  END {
    for (p in total) {
      pct = total[p] > 0 ? (covered[p] * 100.0 / total[p]) : 0
      printf "%s %.1f\n", p, pct
    }
  }
' "$profile" | sort > "$measured"

failed=0
checked=0

while read -r pkg floor; do
  # Skip blank lines and comments.
  [[ -z "$pkg" || "$pkg" == \#* ]] && continue

  actual="$(awk -v p="$pkg" '$1 == p { print $2 }' "$measured")"

  if [[ -z "$actual" ]]; then
    # A floor for a package the profile does not mention. Either the package was deleted or
    # renamed and the floor is stale, or it has no test files at all and produced no profile
    # lines. Both are worth failing on: a floor nothing measures is a gate that has quietly
    # stopped gating.
    echo "FAIL  $pkg — a floor of ${floor}% is set but the coverage profile has no data for it" >&2
    failed=1
    continue
  fi

  checked=$((checked + 1))

  if awk -v a="$actual" -v f="$floor" 'BEGIN { exit !(a + 0 < f + 0) }'; then
    echo "FAIL  $pkg — ${actual}%, below its floor of ${floor}%" >&2
    failed=1
  else
    printf 'ok    %-34s %5s%%  (floor %s%%)\n' "$pkg" "$actual" "$floor"
  fi
done < "$floors"

# Name the packages with no floor. Not a failure — see the header — but an omission that should
# not be invisible.
unlisted="$(
  comm -23 \
    <(awk '{print $1}' "$measured") \
    <(awk '!/^#/ && NF { print $1 }' "$floors" | sort)
)"

if [[ -n "$unlisted" ]]; then
  echo
  echo "no floor set (add to $floors to gate them):"
  while read -r pkg; do
    [[ -z "$pkg" ]] && continue
    printf '  %-34s %5s%%\n' "$pkg" "$(awk -v p="$pkg" '$1 == p { print $2 }' "$measured")"
  done <<< "$unlisted"
fi

echo
if [[ "$failed" -ne 0 ]]; then
  echo "coverage-gate: at least one package is below its floor" >&2
  exit 1
fi

echo "coverage-gate: $checked package(s) at or above their floor"
