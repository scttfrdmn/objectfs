#!/usr/bin/env bash
#
# sync-labels.sh — reconcile the repository's labels with .github/labels.yml.
#
# Usage:
#   .github/scripts/sync-labels.sh              # report drift, change nothing
#   .github/scripts/sync-labels.sh --apply      # create and update to match the file
#
# The file is the source of truth for labels that should exist. It is deliberately NOT the source of
# truth for labels that should not: --apply creates and updates, and reports extras without touching
# them, because deleting a label removes it from every issue that carries it and that is not a thing
# to do as a side effect of a sync. The extras are named, with the command to remove each.
#
# What the previous version of this script did, since it is the reason this one is careful. It parsed
# labels.yml with a bash regex requiring `- name: "..."` — double quotes. This file has used single
# quotes or bare scalars for its entire history, so the loop matched 0 of its entries, and the script
# then printed "Label sync complete!" and exited 0. Issue 190 concluded from that symptom that
# labels.yml "is applied by nothing"; what was true is worse, that it was applied by something which
# reported success without doing anything. A regex over YAML has no notion of which of the three
# scalar forms an author used, so the fix is not a better regex — it is a parser.
#
# Drift is also checked by internal/config/labels_test.go, which runs in CI on every PR and fails in
# both directions. That test is the gate; this script is how you fix what it reports. Neither is
# keyed on labels.yml changing: every drift this repository has had originated on GitHub — created in
# the web UI, or invented by `gh issue create --label`, or created by Dependabot — so a job with a
# `paths:` filter on the file would have fired on none of them.

set -euo pipefail

REPO="${REPO:-scttfrdmn/objectfs}"
LABELS_FILE="${LABELS_FILE:-.github/labels.yml}"

apply=false
if [[ "${1:-}" == "--apply" ]]; then
  apply=true
elif [[ -n "${1:-}" ]]; then
  echo "usage: $0 [--apply]" >&2
  exit 2
fi

if [[ ! -f "$LABELS_FILE" ]]; then
  echo "sync-labels: no label file at $LABELS_FILE" >&2
  exit 2
fi

for tool in gh python3; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "sync-labels: $tool is required and not on PATH" >&2
    exit 2
  fi
done

# The parse. python3 rather than yq because yq is not installed here and python3 is, and rather than
# a bash regex for the reason in the header. PyYAML is not guaranteed present either — it is absent
# from the system Python on this host — so the reader is a small scanner over the three scalar forms
# rather than an import. It is still a parser and not a pattern: it tracks whether it is inside a
# sequence item, which is exactly what the bash version could not do.
read_labels() {
  python3 - "$LABELS_FILE" <<'PY'
import sys

def scalar(raw):
    raw = raw.strip()
    if len(raw) >= 2 and raw[0] == raw[-1] and raw[0] in "'\"":
        return raw[1:-1].replace("''", "'")
    # Strip a trailing comment from a bare scalar. A quoted one keeps its '#'.
    if "#" in raw:
        raw = raw.split("#", 1)[0]
    return raw.strip()

labels, current = [], None

for line in open(sys.argv[1], encoding="utf-8"):
    stripped = line.strip()
    if not stripped or stripped.startswith("#"):
        continue

    if stripped.startswith("- "):
        if current is not None:
            labels.append(current)
        current = {}
        stripped = stripped[2:].strip()

    if current is None or ":" not in stripped:
        continue

    key, _, value = stripped.partition(":")
    if key.strip() in ("name", "description", "color"):
        current[key.strip()] = scalar(value)

if current is not None:
    labels.append(current)

incomplete = [l for l in labels if not all(l.get(k) for k in ("name", "description", "color"))]
if incomplete:
    for l in incomplete:
        print(f"sync-labels: incomplete entry: {l}", file=sys.stderr)
    sys.exit(1)

if len(labels) < 50:
    print(f"sync-labels: parsed only {len(labels)} labels; the file defines many more, so the "
          f"reader has stopped working. Refusing to sync against a partial parse.", file=sys.stderr)
    sys.exit(1)

for l in labels:
    print("\t".join((l["name"], l["color"], l["description"])))
PY
}

declared_tsv="$(read_labels)"
declared_count="$(grep -c . <<<"$declared_tsv" || true)"

echo "sync-labels: $declared_count labels declared in $LABELS_FILE"

existing="$(gh label list --repo "$REPO" --limit 300 --json name --jq '.[].name' | sort)"

created=0 updated=0 unchanged=0

while IFS=$'\t' read -r name color description; do
  [[ -n "$name" ]] || continue

  if grep -Fxq "$name" <<<"$existing"; then
    if ! $apply; then
      unchanged=$((unchanged + 1))
      continue
    fi

    # An edit, not a skip. A label that exists with a different color or description is drift the
    # gate reports, and a sync that only creates can never fix it.
    if gh label edit "$name" --repo "$REPO" --color "$color" --description "$description" \
      >/dev/null 2>&1; then
      updated=$((updated + 1))
    else
      echo "  ! failed to update: $name" >&2
    fi
  else
    if ! $apply; then
      echo "  + would create: $name"
      created=$((created + 1))
      continue
    fi

    if gh label create "$name" --repo "$REPO" --color "$color" --description "$description" \
      >/dev/null 2>&1; then
      echo "  + created: $name"
      created=$((created + 1))
    else
      echo "  ! failed to create: $name" >&2
    fi
  fi
done <<<"$declared_tsv"

# Extras: on GitHub, absent from the file. Reported, never deleted — see the header.
extras="$(comm -23 <(printf '%s\n' "$existing") <(cut -f1 <<<"$declared_tsv" | sort))"

if [[ -n "$extras" ]]; then
  echo
  echo "sync-labels: these exist on GitHub and are not in $LABELS_FILE:"
  while IFS= read -r extra; do
    [[ -n "$extra" ]] || continue
    echo "  - $extra"
  done <<<"$extras"
  echo
  echo "Add each to $LABELS_FILE, or remove it from the repository. Check what carries it first —"
  echo "deleting a label removes it from every issue that has it:"
  echo "  gh issue list --repo $REPO --label '<name>' --state all"
  echo "  gh label delete '<name>' --repo $REPO"
fi

echo
if $apply; then
  echo "sync-labels: created $created, updated $updated"
else
  echo "sync-labels: $created to create, $unchanged already present. Re-run with --apply to change"
  echo "anything; this run modified nothing."
fi
