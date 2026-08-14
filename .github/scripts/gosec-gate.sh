#!/usr/bin/env bash
#
# gosec-gate.sh — fail the build on a gosec finding that is not in the baseline.
#
# The `Security Scan` job runs `gosec -no-fail`, and that stays: the SARIF upload needs the complete
# report, including the sdks/c findings the "Fix Gosec SARIF format" step back-fills a path onto. So
# the gate is this script reading the same SARIF afterwards, rather than gosec's exit status.
#
# That choice is what makes the gate report on both triggers. The check that used to go red — `gosec`,
# derived from the code-scanning upload — appears on pull requests and not on `push: main`, measured
# across four commits. A required context that does not always report blocks every PR forever (#413),
# so it could never be required. `Security Scan` is a job name: it reports on every trigger the
# workflow has.
#
# Compares (rule, file, count) against .github/gosec-baseline.txt, exactly, in both directions. A
# count that went up fails as a new finding; a count that went down fails as a stale baseline, because
# a slot kept after its finding was fixed is permanent headroom for the next one.
#
# Usage: gosec-gate.sh <sarif-file> <baseline-file>
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 <sarif-file> <baseline-file>" >&2
	exit 2
fi

sarif="$1"
baseline="$2"

for f in "$sarif" "$baseline"; do
	if [ ! -f "$f" ]; then
		echo "gosec-gate: $f does not exist" >&2
		exit 2
	fi
done

# A SARIF with no results at all is the shape a *failed* gosec run leaves behind as easily as a clean
# one, and the baseline is non-empty, so the diff below would report eight fixed findings and fail
# with a message about the wrong thing. Refuse it directly instead. gosec's own run step has
# `-no-fail`, which is precisely why its exit status cannot be relied on to have noticed.
total=$(jq '[.runs[]?.results[]?] | length' "$sarif")
if [ "$total" -eq 0 ]; then
	echo "gosec-gate: $sarif contains zero results, and the baseline is not empty." >&2
	echo "gosec-gate: gosec runs with -no-fail, so a scan that analysed nothing exits 0 and looks" >&2
	echo "gosec-gate: identical to a clean tree from here. Check the 'Run Gosec Security Scanner' log." >&2
	exit 1
fi

# Same key as the baseline: rule and file, not line. A line number moves whenever anything above it
# moves, so a line-keyed comparison reports on unrelated edits and never on the thing that changed.
#
# The uri default matches the back-fill in security.yml: gosec emits an empty artifactLocation for the
# cgo-generated intermediate, and sdks/c is the module's only cgo package. If that step is reached
# first the uri is already set and this default does nothing; it is here so the script is also correct
# run by hand against a raw report.
found=$(mktemp)
trap 'rm -f "$found"' EXIT

jq -r '
	[.runs[]?.results[]? | {
		rule: (.ruleId // "UNKNOWN"),
		file: (if ((.locations[0]?.physicalLocation.artifactLocation.uri // "") == "")
		       then "sdks/c/main.go"
		       else .locations[0].physicalLocation.artifactLocation.uri end)
	}]
	| group_by([.rule, .file])
	| map("\(.[0].rule) \(.[0].file) \(length)")
	| sort | .[]
' "$sarif" > "$found"

expected=$(mktemp)
trap 'rm -f "$found" "$expected"' EXIT
# `[[:space:]]` rather than `\s`, which is a GNU extension: this runs on ubuntu-latest but is also
# meant to be runnable by hand, and BSD grep does not match it. `|| true` because grep exits 1 when it
# selects nothing, which under `set -e` would kill the script on an all-comments baseline — a state
# that should reach the diff and be reported, not vanish.
grep -v '^[[:space:]]*\(#\|$\)' "$baseline" | sed 's/[[:space:]]*$//' | sort > "$expected" || true

echo "gosec-gate: $total findings in $sarif, $(wc -l < "$found" | tr -d ' ') (rule, file) groups"

if diff -u "$expected" "$found" > /dev/null; then
	echo "gosec-gate: matches .github/gosec-baseline.txt exactly"
	exit 0
fi

# Reached only on a failure, and everything below has to survive to be printed: the diff is the entire
# diagnosis, and a step that exits before emitting it leaves a log holding nothing but an errno. That
# happened once already in this repository, in the install-script job (#433), where a command
# substitution under `set -e` killed the step at the assignment and the `echo` below it never ran.
#
# Which is not a lesson this script learned from the comment — it made the same mistake. `diff` exits 1
# on a difference, `pipefail` promotes that to the pipeline's status, and `set -e` ended the script at
# the `diff | tail` below, so the first run against a mutant printed the diff and then none of the
# three paragraphs explaining what to do about it. Hence `|| true`, and hence running the failing path
# rather than only the passing one.
echo
echo "gosec findings no longer match the baseline."
echo
echo "  -  expected, from .github/gosec-baseline.txt"
echo "  +  found, from this run's gosec report"
echo
diff -u --label 'baseline' --label 'this run' "$expected" "$found" | tail -n +3 || true
echo
echo "A '+' line is a finding this baseline does not account for. Fix the code — do not add the line"
echo "to the baseline, which is what the baseline exists to stop being the reflex."
echo
echo "A '-' line means a baselined finding is gone. If you fixed it, remove its line from"
echo ".github/gosec-baseline.txt in the same commit; a slot kept after its finding was fixed is"
echo "headroom the next conversion in that file would land in silently."
echo
echo "Findings are keyed by (rule, file, count), so a count in a file that already appears has to be"
echo "edited rather than added: 'G115 internal/health/remediation.go 2' becomes 3, not a second line."

exit 1
