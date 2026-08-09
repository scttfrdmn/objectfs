#!/usr/bin/env bash
# pjdfstest.sh — run the pjdfstest POSIX compliance suite against ObjectFS.
#
# ON DEMAND ONLY. Nothing runs this automatically, and that is a statement about infrastructure
# rather than about the script, which works. It needs /dev/fuse, real AWS credentials, and a real
# bucket, so a GitHub-hosted runner cannot execute it: there is no scheduled real-AWS job in this
# repository at all (the only cron: in the tree belongs to the security scan). Wiring it into CI
# would mean adding one, with a bucket and a role, which is a decision with a cost attached and not
# something a script comment can make.
#
# Written down instead of left implied, because the failure mode of an unrun conformance suite is
# that it reads as a passing one. `internal/fuse/kernel_options_live_test.go` documents its own
# unrunnability for the same reason, and `make test-fuse-mount` says so beside its target. Tracked as
# https://github.com/scttfrdmn/objectfs/issues/352.
#
# Kept rather than deleted: this is the only *third-party* POSIX conformance suite this project has.
# `internal/difftest` is the closest thing that does run in CI, and it makes a weaker claim — it
# compares ObjectFS against the local OS filesystem over an operation sequence this repository chose,
# where pjdfstest is a suite nobody here wrote and cannot have tuned to what already works. Run it by
# hand before a release, and read README.md's supported-operations table for what is expected to fail
# by design: ObjectFS is not a POSIX-compliant filesystem, so a clean pjdfstest run is not the goal —
# knowing which cases fail, and that the set has not grown, is.
#
# Prerequisites:
#   - pjdfstest binary in $PATH (https://github.com/pjd/pjdfstest)
#   - ./bin/objectfs binary (run `make build` first)
#   - OBJECTFS_TEST_BUCKET env var set to an S3 bucket the caller has access to
#   - AWS credentials available in the environment or via a named profile
#
# Results are written to pjdfstest-results.txt in the current directory.
#
# Usage:
#   OBJECTFS_TEST_BUCKET=my-bucket ./scripts/pjdfstest.sh
set -euo pipefail

BUCKET="${OBJECTFS_TEST_BUCKET:-objectfs-posix-test}"
BINARY="${OBJECTFS_BINARY:-./bin/objectfs}"
RESULTS="${OBJECTFS_PJDFSTEST_RESULTS:-pjdfstest-results.txt}"

# Check prerequisites
if ! command -v pjdfstest &>/dev/null; then
    echo "ERROR: pjdfstest not found in PATH" >&2
    echo "       Install from https://github.com/pjd/pjdfstest" >&2
    exit 1
fi
if [[ ! -x "$BINARY" ]]; then
    echo "ERROR: objectfs binary not found at $BINARY" >&2
    echo "       Run 'make build' first" >&2
    exit 1
fi

MOUNT_DIR=$(mktemp -d)
MOUNT_PID=0

# cleanup is invoked by the `trap` below rather than by a call, which is why shellcheck reports SC2329
# ("this function is never invoked") against it. Suppressed rather than restructured: the trap is the
# invocation, and it has to be registered after MOUNT_DIR exists.
# shellcheck disable=SC2329
cleanup() {
    if [[ $MOUNT_PID -ne 0 ]]; then
        # Try platform-specific unmount first, then fall back to generic umount.
        if command -v fusermount &>/dev/null; then
            fusermount -u "$MOUNT_DIR" 2>/dev/null || umount "$MOUNT_DIR" 2>/dev/null || true
        else
            umount "$MOUNT_DIR" 2>/dev/null || true
        fi
        wait "$MOUNT_PID" 2>/dev/null || true
    fi
    rmdir "$MOUNT_DIR" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "Mounting s3://${BUCKET} at ${MOUNT_DIR} ..."
"$BINARY" mount "s3://${BUCKET}" "$MOUNT_DIR" &
MOUNT_PID=$!

# Wait for the mount to become ready (up to 10s).
#
# `for _ in` rather than `for i in $(seq 1 20)`: the counter was never read, and shellcheck reported
# it as SC2034. `seq` is also not POSIX and is absent on a minimal image, which a fixed-count brace
# expansion is not.
for _ in {1..20}; do
    if mountpoint -q "$MOUNT_DIR" 2>/dev/null || mount | grep -q "$MOUNT_DIR"; then
        break
    fi
    sleep 0.5
done

echo "Running pjdfstest against ${MOUNT_DIR} ..."
pjdfstest "$MOUNT_DIR" 2>&1 | tee "$RESULTS"
EXIT_CODE=${PIPESTATUS[0]}

echo ""
echo "Results written to ${RESULTS}"
exit "$EXIT_CODE"
