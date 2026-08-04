#!/usr/bin/env bash
#
# fuzz-smoke.sh — fuzz one target for a fixed budget, then decide whether the failure was real.
#
# `go test -fuzz` fails for two unrelated reasons, and CI cannot tell them apart from the exit
# status alone:
#
#   1. It found a counterexample. That is the entire point of the job and must fail the build.
#   2. Its own coordinator lost a shutdown race, or the runner was preempted. Neither is a
#      property of the code under test, and failing on them trains people to re-run a red job
#      without reading it — which costs more than the gate is worth (#218).
#
# The two are distinguishable, because a real find leaves evidence a timeout cannot fabricate: a
# new file under testdata/fuzz/<Target>/ and the line `Failing input written to`. So this script
# keys on **the presence of a counterexample**, not on the exit status, and tolerates exactly two
# shapes of failure when no counterexample exists:
#
#   - `--- FAIL: <Target>` whose only detail is `context deadline exceeded`, reported after the
#     target actually ran for its budget. That is $GOROOT/src/internal/fuzz/fuzz.go:228 calling
#     `stop(ctx.Err())` while the suppression at :130 compares against the *child* context's
#     `Err()`, which can still be nil because `cancelCtx.cancel` closes its own `done` before
#     propagating to children. Reproduced standalone at 12 in 20,000.
#   - SIGTERM (143) or `The runner has received a shutdown signal` — GitHub reclaiming the runner.
#
# Everything else fails, including a deadline error that arrives early, a panic, a data race, a
# seed-corpus failure, and the OOM-shaped `fuzzing process hung or terminated unexpectedly`. That
# last one writes an input to testdata, so it is caught by the counterexample check rather than by
# a message match.
#
# Deliberately NOT done here: dropping -fuzztime, or making the job non-blocking. Either would
# turn a flaky gate into no gate.
#
# Usage: fuzz-smoke.sh <package> <FuzzTarget> <seconds>
#
# `set -e` is absent on purpose: the whole job of this script is to survive a failing command and
# look at what it left behind.
set -uo pipefail

if [ "$#" -ne 3 ]; then
	echo "usage: $0 <package> <FuzzTarget> <seconds>" >&2
	exit 2
fi

pkg=$1
target=$2
seconds=$3

case $seconds in
'' | *[!0-9]*)
	echo "fuzz-smoke: <seconds> must be a whole number of seconds, got '$seconds'" >&2
	exit 2
	;;
esac

# The corpus directory is derived from the package path rather than from `go list`, so that this
# script's decision-making can be exercised against a stub `go` on PATH — which is what
# internal/config/fuzz_smoke_script_test.go does.
corpus="${pkg#./}/testdata/fuzz/${target}"

log=$(mktemp)
before=$(mktemp)
after=$(mktemp)
trap 'rm -f "$log" "$before" "$after"' EXIT

list_corpus() {
	if [ -d "$corpus" ]; then
		find "$corpus" -type f | LC_ALL=C sort
	fi
}

list_corpus >"$before"

start=$SECONDS
# tee, not a redirect: a preempted runner is killed mid-run, and output buffered into a file it
# never gets to cat is output nobody sees. Streaming it is also what makes a 60-second step
# readable while it runs.
go test "$pkg" -run "^${target}\$" -fuzz "^${target}\$" -fuzztime="${seconds}s" 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
wall=$((SECONDS - start))

list_corpus >"$after"
new_inputs=$(comm -13 "$before" "$after")

fail() {
	echo "::error title=fuzz-smoke $target::$1"
	echo "fuzz-smoke: FAIL ($target, exit $status, ${wall}s wall) — $1" >&2
	exit 1
}

tolerate() {
	echo "::warning title=fuzz-smoke $target::$1 — tolerated, no counterexample was produced"
	echo "fuzz-smoke: tolerated ($target, exit $status, ${wall}s wall) — $1"
	exit 0
}

# A counterexample first, before the exit status is consulted at all. `go test` has reported a
# written input alongside a zero exit before (the minimizer's hung-process path), so this cannot
# be nested under the failure branch.
if [ -n "$new_inputs" ]; then
	echo "fuzz-smoke: new file(s) under $corpus:" >&2
	echo "$new_inputs" >&2
	fail "a new input was written to $corpus — that is a counterexample, commit it and fix the target"
fi

if grep -q 'Failing input written to' "$log"; then
	fail "output reports 'Failing input written to' — that is a counterexample"
fi

if [ "$status" -eq 0 ]; then
	echo "fuzz-smoke: ok ($target, ${wall}s wall)"
	exit 0
fi

# Runner preemption. 143 is SIGTERM; the message is what the runner logs when it is reclaimed.
if [ "$status" -eq 143 ] || grep -q 'The runner has received a shutdown signal' "$log"; then
	tolerate "the runner was preempted (exit $status)"
fi

# From here the only tolerated shape is the coordinator's shutdown race. Every condition below is
# required, so that nothing else can wear its clothes.
for marker in 'panic:' 'WARNING: DATA RACE' 'test timed out after' \
	'fuzzing process hung or terminated unexpectedly'; do
	if grep -qF "$marker" "$log"; then
		fail "output contains '$marker' — a real failure, not a shutdown race"
	fi
done

target_fails=$(grep -c "^--- FAIL: ${target} " "$log")
other_fails=$(grep -cE '^[[:space:]]+--- FAIL: ' "$log")

if [ "$target_fails" -ne 1 ] || [ "$other_fails" -ne 0 ]; then
	fail "expected exactly one '--- FAIL: $target' and no subtest failures, got $target_fails and $other_fails"
fi

if ! grep -q 'context deadline exceeded' "$log"; then
	fail "failed without a counterexample and without 'context deadline exceeded' — unexplained"
fi

# A deadline error is only the coordinator's shutdown race if the target actually *reached* its
# deadline. The same message arriving early is something else wearing it — a context canceled
# inside the target, say — and the duration is what tells them apart.
#
# The duration comes from the `--- FAIL: <Target> (60.11s)` line rather than from wall clock,
# because wall clock includes compilation and module loading and so is only ever an overestimate:
# a target that failed at 0.01s inside a run whose build took a minute would clear a wall-clock
# floor while being exactly the case this check exists to reject. The line is guaranteed present
# and unique by the count above.
#
# awk for the comparison — the duration is fractional and `[` is integer-only. 90% of the budget
# rather than the budget itself: the coordinator reports slightly *over* (60.11s of 60s), but a
# worker that finishes its last input just under is a normal shutdown too.
reported=$(sed -n "s/^--- FAIL: ${target} (\([0-9.]*\)s)\$/\1/p" "$log" | head -1)

if [ -z "$reported" ]; then
	fail "could not read a duration from the '--- FAIL: $target' line, so the deadline cannot be confirmed"
fi

if ! awk -v got="$reported" -v budget="$seconds" 'BEGIN { exit !(got >= budget * 0.9) }'; then
	fail "'context deadline exceeded' after ${reported}s of a ${seconds}s budget — too early to be the shutdown race"
fi

tolerate "'context deadline exceeded' at ${reported}s of a ${seconds}s budget, zero new inputs (go's fuzzing coordinator shutdown race, #218)"
