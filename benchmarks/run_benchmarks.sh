#!/bin/bash
#
# ObjectFS Benchmark Runner
# Runs comprehensive performance benchmarks for v0.4.0
#

set -e

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
    echo "# ObjectFS v0.4.0 Benchmark Results" > "${RESULTS_FILE}"
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

    echo -e "${BLUE}5/6 S3 acceleration benchmarks...${NC}"
    if [[ -n "${OBJECTFS_BENCH_BUCKET}" ]]; then
        go test -bench=BenchmarkGetObject -benchmem ./internal/storage/s3/ | tee -a "${RESULTS_FILE}"
        go test -bench=BenchmarkPutObject -benchmem ./internal/storage/s3/ | tee -a "${RESULTS_FILE}"
        go test -bench=BenchmarkFallback -benchmem ./internal/storage/s3/ | tee -a "${RESULTS_FILE}"
    else
        echo -e "${YELLOW}Skipping S3 benchmarks (OBJECTFS_BENCH_BUCKET not set)${NC}"
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
        echo "ObjectFS v0.4.0 Benchmark Summary"
        echo "=================================="
        echo ""
        echo "Date: $(date)"
        echo "Results: ${RESULTS_FILE}"
        echo ""
        echo "Key Performance Metrics:"
        echo ""

        # Extract key metrics if available
        if grep -q "BenchmarkGetObject" "${RESULTS_FILE}"; then
            echo "S3 GET Operations:"
            grep "BenchmarkGetObject" "${RESULTS_FILE}" | head -5
            echo ""
        fi

        if grep -q "BenchmarkPutObject" "${RESULTS_FILE}"; then
            echo "S3 PUT Operations:"
            grep "BenchmarkPutObject" "${RESULTS_FILE}" | head -5
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
    echo -e "${GREEN}   ObjectFS v0.4.0 Benchmark Suite     ${NC}"
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
