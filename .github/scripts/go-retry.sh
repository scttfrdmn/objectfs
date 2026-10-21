#!/usr/bin/env bash
# go-retry.sh — run a `go` command that fetches modules, retrying only the failures that are not ours.
#
#   .github/scripts/go-retry.sh mod download
#   .github/scripts/go-retry.sh install golang.org/x/vuln/cmd/govulncheck@v1.9.0
#
# Go does not retry a module fetch. A single HTTP/2 stream error from proxy.golang.org or
# sum.golang.org is a build failure, the job dies before compiling anything, and the log reads like a
# code problem. Five correct PRs went red that way in October 2026 (#633), each time on a module main
# had just bumped, which is when a cache keyed on go.sum has nothing saved yet.
#
# Three outcomes, the same split ci.yml's deploy image check makes (#632):
#   - success: done.
#   - a transient answer from the module servers (stream INTERNAL_ERROR, a 5xx, a timeout, a reset):
#     retried, with backoff, and each retry is printed so the log shows it happened.
#   - anything else — a module or version that does not exist, a checksum mismatch, a syntax error —
#     fails on the first attempt. A checksum mismatch in particular must never be retried into
#     silence: sum.golang.org disagreeing with the bytes is the integrity check working.
# Still transient after the last attempt is a failure, not a warning: unlike an image that is only
# being checked for existence, a build cannot go ahead without its modules.
set -uo pipefail

attempts=${GO_RETRY_ATTEMPTS:-3}
backoff=${GO_RETRY_BACKOFF:-10}

transient='stream error|INTERNAL_ERROR|50[0-9] |Bad Gateway|Service Unavailable|Gateway Timeout|i/o timeout|TLS handshake timeout|connection reset|connection refused|unexpected EOF|context deadline exceeded'

for attempt in $(seq 1 "$attempts"); do
	log=$(mktemp)
	go "$@" >"$log" 2>&1
	status=$?
	cat "$log"

	if [ "$status" -eq 0 ]; then
		rm -f "$log"
		exit 0
	fi

	# A checksum mismatch is checked first and never retried, even if a transient pattern also
	# appears in the same output: retrying a SECURITY ERROR until it passes would defeat it.
	if grep -qE 'SECURITY ERROR|checksum mismatch|verifying .*: .*mismatch' "$log"; then
		echo "::error::go $*: checksum verification failed. Not retried; this is the module integrity check doing its job." >&2
		rm -f "$log"
		exit "$status"
	fi

	if ! grep -qE "$transient" "$log"; then
		echo "::error::go $* failed (exit $status), and not with a transient module-server error, so it is not retried." >&2
		rm -f "$log"
		exit "$status"
	fi

	rm -f "$log"

	if [ "$attempt" -lt "$attempts" ]; then
		wait=$((attempt * backoff))
		echo "::warning::go $*: transient module-server error on attempt $attempt of $attempts; retrying in ${wait}s." >&2
		sleep "$wait"
	fi
done

echo "::error::go $*: still failing with a transient module-server error after $attempts attempts." >&2
exit 1
