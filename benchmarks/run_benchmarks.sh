#!/bin/bash
#
# ObjectFS Benchmark Runner
#
# The version is read from the source rather than written here. It said v0.4.0 in four places,
# including the H1 of the results file this script writes — and a benchmark report is exactly the
# artifact someone keeps and compares against months later, so a stale label on it is worse than a
# stale label in a comment. Per CLAUDE.md the `version` constant in cmd/objectfs/main.go is the only
# authority, so this greps that constant instead of restating it.
#

set -e

# The constant's line is `	version = "0.11.0"`. Falling back rather than failing: an unrunnable
# benchmark runner because a grep missed would be a worse trade than an unlabelled report.
OBJECTFS_VERSION="$(grep -oE '^[[:space:]]*version = "[^"]+"' "$(dirname "$0")/../cmd/objectfs/main.go" 2>/dev/null | grep -oE '[0-9][^"]*' || echo "unknown")"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
RESULTS_DIR="benchmarks/results"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
RESULTS_FILE="${RESULTS_DIR}/bench_${TIMESTAMP}.txt"
BASELINE_FILE="${RESULTS_DIR}/baseline.txt"

# Check prerequisites
check_prerequisites() {
    echo -e "${BLUE}Checking prerequisites...${NC}"

    if [[ -z "${OBJECTFS_BENCH_BUCKET}" ]]; then
        echo -e "${YELLOW}⚠️  Warning: OBJECTFS_BENCH_BUCKET not set${NC}"
        echo "   Some benchmarks will be skipped. Set it to run full suite:"
        echo "   export OBJECTFS_BENCH_BUCKET=your-test-bucket"
    fi

    if [[ -z "${AWS_ACCESS_KEY_ID}" ]] && [[ -z "${AWS_PROFILE}" ]]; then
        echo -e "${YELLOW}⚠️  Warning: AWS credentials not configured${NC}"
        echo "   S3 benchmarks will be skipped."
    fi

    if ! command -v go &> /dev/null; then
        echo -e "${RED}❌ Error: go command not found${NC}"
        exit 1
    fi

    echo -e "${GREEN}✅ Prerequisites checked${NC}"
}

# Create results directory
setup_results_dir() {
    mkdir -p "${RESULTS_DIR}"
    echo -e "${BLUE}Results will be saved to: ${RESULTS_FILE}${NC}"
}

# Run benchmark suite
run_benchmarks() {
    local mode="${1:-full}"

    echo -e "${BLUE}Running benchmarks in ${mode} mode...${NC}"
    echo "# ObjectFS v${OBJECTFS_VERSION} Benchmark Results" > "${RESULTS_FILE}"
    echo "# Date: $(date)" >> "${RESULTS_FILE}"
    echo "# Mode: ${mode}" >> "${RESULTS_FILE}"
    echo "" >> "${RESULTS_FILE}"

    if [[ "${mode}" == "short" ]]; then
        echo -e "${YELLOW}Running in SHORT mode (faster, fewer iterations)${NC}"
        run_short_benchmarks
    else
        echo -e "${BLUE}Running FULL benchmark suite${NC}"
        run_full_benchmarks
    fi
}

# Short benchmark suite (for quick validation)
run_short_benchmarks() {
    echo -e "${BLUE}1/3 Configuration benchmarks...${NC}"
    go test -bench=. -benchmem -short ./internal/config/ | tee -a "${RESULTS_FILE}"

    echo -e "${BLUE}2/3 Adapter benchmarks...${NC}"
    go test -bench=. -benchmem -short ./internal/adapter/ | tee -a "${RESULTS_FILE}"

    echo -e "${BLUE}3/3 S3 benchmarks (sample)...${NC}"
    go test -bench=BenchmarkAccelerationOverhead -benchmem -short ./internal/storage/s3/ | tee -a "${RESULTS_FILE}"
}

# Full benchmark suite
run_full_benchmarks() {
    echo -e "${BLUE}1/6 Configuration benchmarks...${NC}"
    go test -bench=. -benchmem ./internal/config/ | tee -a "${RESULTS_FILE}"

    echo -e "${BLUE}2/6 Cache benchmarks...${NC}"
    go test -bench=. -benchmem ./internal/cache/ | tee -a "${RESULTS_FILE}" || true

    echo -e "${BLUE}3/6 Metrics benchmarks...${NC}"
    go test -bench=. -benchmem ./internal/metrics/ | tee -a "${RESULTS_FILE}" || true

    echo -e "${BLUE}4/6 Adapter benchmarks...${NC}"
    go test -bench=. -benchmem ./internal/adapter/ | tee -a "${RESULTS_FILE}" || true

    # The S3Backend_* set runs against an in-process stub — no credentials, no bucket, no network — so
    # it is outside the gate rather than inside it. It is also the only S3 block here that produced
    # numbers without a bucket, and it was not being run at all.
    #
    # #235 read the old block as matching nothing because of the `S3Backend_` infix. That is not what
    # happened: `-bench=BenchmarkGetObject` is an unanchored regex and it did match four benchmarks in
    # acceleration_bench_test.go. They skip on an unset OBJECTFS_BENCH_BUCKET, and `go test -bench`
    # reports `ok` with exit 0 for a skipped benchmark exactly as it does for one that ran. So the
    # names were fine and the gate was what silenced them — which is the same "reports success while
    # doing nothing" shape, arriving by a different route, and worth writing down because the obvious
    # fix (correct the names) would have changed nothing.
    echo -e "${BLUE}5/6 S3 backend benchmarks (stub, no credentials needed)...${NC}"
    go test -bench=BenchmarkS3Backend_ -benchmem ./internal/storage/s3/ | tee -a "${RESULTS_FILE}"

    echo -e "${BLUE}5b/6 S3 acceleration benchmarks...${NC}"
    if [[ -n "${OBJECTFS_BENCH_BUCKET}" ]]; then
        go test -bench='BenchmarkGetObject_|BenchmarkPutObject_|BenchmarkFallback' -benchmem \
            ./internal/storage/s3/ | tee -a "${RESULTS_FILE}"
    else
        echo -e "${YELLOW}Skipping acceleration benchmarks (OBJECTFS_BENCH_BUCKET not set)${NC}"
        # Runs without a bucket; the others in that file do not.
        go test -bench=BenchmarkAccelerationOverhead -benchmem ./internal/storage/s3/ | tee -a "${RESULTS_FILE}"
    fi

    echo -e "${BLUE}6/6 Multipart upload benchmarks...${NC}"
    if [[ -n "${OBJECTFS_BENCH_BUCKET}" ]]; then
        go test -bench=BenchmarkMultipart_32MB -benchmem ./internal/storage/s3/ | tee -a "${RESULTS_FILE}"
        go test -bench=BenchmarkMultipart_100MB -benchmem ./internal/storage/s3/ | tee -a "${RESULTS_FILE}"
    else
        echo -e "${YELLOW}Skipping multipart benchmarks (OBJECTFS_BENCH_BUCKET not set)${NC}"
    fi
}

# Compare with baseline
compare_with_baseline() {
    if [[ ! -f "${BASELINE_FILE}" ]]; then
        echo -e "${YELLOW}No baseline found. Saving current results as baseline...${NC}"
        cp "${RESULTS_FILE}" "${BASELINE_FILE}"
        echo -e "${GREEN}✅ Baseline saved to ${BASELINE_FILE}${NC}"
        return
    fi

    if ! command -v benchstat &> /dev/null; then
        echo -e "${YELLOW}⚠️  benchstat not found. Install with:${NC}"
        echo "   go install golang.org/x/perf/cmd/benchstat@latest"
        return
    fi

    echo -e "${BLUE}Comparing with baseline...${NC}"
    benchstat "${BASELINE_FILE}" "${RESULTS_FILE}" | tee "${RESULTS_DIR}/comparison_${TIMESTAMP}.txt"
}

# Generate summary
generate_summary() {
    echo -e "${BLUE}Generating summary...${NC}"

    local summary_file="${RESULTS_DIR}/summary_${TIMESTAMP}.txt"

    {
        echo "ObjectFS v${OBJECTFS_VERSION} Benchmark Summary"
        echo "=================================="
        echo ""
        echo "Date: $(date)"
        echo "Results: ${RESULTS_FILE}"
        echo ""
        echo "Key Performance Metrics:"
        echo ""

        # Extract key metrics if available.
        #
        # `Benchmark.*GetObject` rather than `BenchmarkGetObject`, because the stub benchmarks are named
        # BenchmarkS3Backend_GetObject_1KB and the literal string "BenchmarkGetObject" does not appear
        # in that — the infix breaks the match. The narrower pattern is why this section printed an
        # empty "Key Performance Metrics" heading in full mode even when nine benchmarks had just run
        # and written their numbers into the file two lines above.
        if grep -qE "Benchmark.*GetObject" "${RESULTS_FILE}"; then
            echo "S3 GET Operations:"
            grep -E "Benchmark.*GetObject" "${RESULTS_FILE}" | head -5
            echo ""
        fi

        if grep -qE "Benchmark.*PutObject" "${RESULTS_FILE}"; then
            echo "S3 PUT Operations:"
            grep -E "Benchmark.*PutObject" "${RESULTS_FILE}" | head -5
            echo ""
        fi

        if grep -q "BenchmarkMultipart" "${RESULTS_FILE}"; then
            echo "Multipart Uploads:"
            grep "BenchmarkMultipart" "${RESULTS_FILE}" | head -5
            echo ""
        fi

        echo "Full results: ${RESULTS_FILE}"
    } | tee "${summary_file}"

    echo -e "${GREEN}✅ Summary saved to ${summary_file}${NC}"
}

# Main function
main() {
    local mode="${1:-full}"

    echo -e "${GREEN}========================================${NC}"
    echo -e "${GREEN}   ObjectFS v${OBJECTFS_VERSION} Benchmark Suite     ${NC}"
    echo -e "${GREEN}========================================${NC}"
    echo ""

    check_prerequisites
    setup_results_dir
    run_benchmarks "${mode}"

    echo ""
    echo -e "${GREEN}========================================${NC}"
    echo -e "${GREEN}   Benchmark Complete!                 ${NC}"
    echo -e "${GREEN}========================================${NC}"
    echo ""

    generate_summary
    compare_with_baseline

    echo ""
    echo -e "${BLUE}Results saved to: ${RESULTS_FILE}${NC}"
    echo ""
    echo -e "${GREEN}Next steps:${NC}"
    echo "  - Review results: cat ${RESULTS_FILE}"
    echo "  - Compare with baseline: benchstat ${BASELINE_FILE} ${RESULTS_FILE}"
    echo "  - Update baseline: cp ${RESULTS_FILE} ${BASELINE_FILE}"
}

# Help text
show_help() {
    cat << EOF
ObjectFS Benchmark Runner

Usage: $0 [mode]

Modes:
  full     Run complete benchmark suite (default)
  short    Run abbreviated benchmark suite (faster)
  help     Show this help message

Examples:
  $0                  # Run full suite
  $0 short            # Run short suite
  $0 help             # Show help

Environment Variables:
  OBJECTFS_BENCH_BUCKET  S3 bucket for benchmarks (required for S3 tests)
  OBJECTFS_BENCH_REGION  AWS region (default: us-east-1)
  AWS_ACCESS_KEY_ID      AWS access key
  AWS_SECRET_ACCESS_KEY  AWS secret key
  AWS_PROFILE            AWS profile name (alternative to keys)

EOF
}

# Parse arguments
case "${1:-full}" in
    help|--help|-h)
        show_help
        exit 0
        ;;
    short|full)
        main "$1"
        ;;
    *)
        echo -e "${RED}❌ Invalid mode: $1${NC}"
        echo "Use 'full', 'short', or 'help'"
        exit 1
        ;;
esac
