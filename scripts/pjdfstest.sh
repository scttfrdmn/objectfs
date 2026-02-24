#!/usr/bin/env bash
# pjdfstest.sh — run the pjdfstest POSIX compliance suite against ObjectFS.
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
for i in $(seq 1 20); do
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
