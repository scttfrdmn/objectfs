#!/usr/bin/env bash
#
# cleanup-aws-test.sh - Cleanup AWS test resources
#
# Usage:
#   ./scripts/cleanup-aws-test.sh                    # Clean default bucket
#   ./scripts/cleanup-aws-test.sh --bucket <name>    # Clean specific bucket
#   ./scripts/cleanup-aws-test.sh --all              # Clean all test buckets
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default values
DEFAULT_PROFILE="aws"
DEFAULT_REGION="us-west-2"
DEFAULT_BUCKET_PREFIX="objectfs-test"

#------------------------------------------------------------------------------
# Helper functions
#------------------------------------------------------------------------------

info() {
    echo -e "${BLUE}ℹ${NC} $*"
}

success() {
    echo -e "${GREEN}✓${NC} $*"
}

warning() {
    echo -e "${YELLOW}⚠${NC} $*"
}

error() {
    echo -e "${RED}✗${NC} $*"
}

#------------------------------------------------------------------------------
# Parse arguments
#------------------------------------------------------------------------------

CLEANUP_MODE="default"
SPECIFIC_BUCKET=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --all)
            CLEANUP_MODE="all"
            shift
            ;;
        --bucket)
            CLEANUP_MODE="specific"
            SPECIFIC_BUCKET="$2"
            shift 2
            ;;
        --profile)
            AWS_PROFILE="$2"
            shift 2
            ;;
        --region)
            AWS_REGION="$2"
            shift 2
            ;;
        -h|--help)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --all                  Clean all objectfs-test-* buckets"
            echo "  --bucket <name>        Clean specific bucket"
            echo "  --profile <profile>    AWS profile to use"
            echo "  --region <region>      AWS region to use"
            echo "  -h, --help             Show this help"
            echo ""
            echo "Examples:"
            echo "  $0"
            echo "  $0 --bucket objectfs-test-myuser"
            echo "  $0 --all --profile aws"
            exit 0
            ;;
        *)
            error "Unknown option: $1"
            exit 1
            ;;
    esac
done

#------------------------------------------------------------------------------
# Load environment or use defaults
#------------------------------------------------------------------------------

if [[ -f .env.test ]]; then
    source .env.test
    info "Loaded environment from .env.test"
fi

export AWS_PROFILE="${AWS_PROFILE:-$DEFAULT_PROFILE}"
export AWS_REGION="${AWS_REGION:-$DEFAULT_REGION}"

#------------------------------------------------------------------------------
# Verify AWS access
#------------------------------------------------------------------------------

info "Verifying AWS access..."

if ! aws sts get-caller-identity --profile "$AWS_PROFILE" &> /dev/null; then
    error "AWS credentials invalid for profile: $AWS_PROFILE"
    exit 1
fi

success "AWS access verified"

#------------------------------------------------------------------------------
# Delete bucket function
#------------------------------------------------------------------------------

delete_bucket() {
    local bucket_name="$1"

    info "Cleaning bucket: $bucket_name"

    # Check if bucket exists
    if ! aws s3 ls "s3://$bucket_name" --profile "$AWS_PROFILE" &> /dev/null; then
        warning "Bucket does not exist: $bucket_name"
        return
    fi

    # Get object count
    OBJECT_COUNT=$(aws s3 ls "s3://$bucket_name" --recursive --profile "$AWS_PROFILE" | wc -l | tr -d ' ')

    if [[ "$OBJECT_COUNT" -gt 0 ]]; then
        info "Deleting $OBJECT_COUNT objects..."

        # Delete all objects
        aws s3 rm "s3://$bucket_name" --recursive --profile "$AWS_PROFILE"

        success "Objects deleted"
    else
        info "Bucket is already empty"
    fi

    # Delete bucket
    info "Deleting bucket..."
    aws s3 rb "s3://$bucket_name" --profile "$AWS_PROFILE"

    success "Bucket deleted: $bucket_name"
}

#------------------------------------------------------------------------------
# Main cleanup logic
#------------------------------------------------------------------------------

case $CLEANUP_MODE in
    default)
        # Clean default bucket
        USERNAME=$(whoami)
        BUCKET_NAME="${OBJECTFS_TEST_BUCKET:-${DEFAULT_BUCKET_PREFIX}-${USERNAME}}"

        echo ""
        warning "This will delete bucket: $BUCKET_NAME"
        echo -n "Are you sure? (yes/no): "
        read -r CONFIRM

        if [[ "$CONFIRM" != "yes" ]]; then
            info "Cleanup cancelled"
            exit 0
        fi

        delete_bucket "$BUCKET_NAME"
        ;;

    specific)
        # Clean specific bucket
        if [[ -z "$SPECIFIC_BUCKET" ]]; then
            error "No bucket specified"
            exit 1
        fi

        echo ""
        warning "This will delete bucket: $SPECIFIC_BUCKET"
        echo -n "Are you sure? (yes/no): "
        read -r CONFIRM

        if [[ "$CONFIRM" != "yes" ]]; then
            info "Cleanup cancelled"
            exit 0
        fi

        delete_bucket "$SPECIFIC_BUCKET"
        ;;

    all)
        # Find and clean all test buckets
        info "Finding all test buckets with prefix: $DEFAULT_BUCKET_PREFIX"

        # List all buckets and filter for test buckets
        TEST_BUCKETS=$(aws s3 ls --profile "$AWS_PROFILE" | awk '{print $3}' | grep "^${DEFAULT_BUCKET_PREFIX}-")

        if [[ -z "$TEST_BUCKETS" ]]; then
            info "No test buckets found"
            exit 0
        fi

        BUCKET_COUNT=$(echo "$TEST_BUCKETS" | wc -l | tr -d ' ')

        echo ""
        warning "Found $BUCKET_COUNT test bucket(s):"
        echo "$TEST_BUCKETS" | while read -r bucket; do
            echo "  - $bucket"
        done

        echo ""
        warning "This will delete ALL test buckets listed above"
        echo -n "Are you sure? (yes/no): "
        read -r CONFIRM

        if [[ "$CONFIRM" != "yes" ]]; then
            info "Cleanup cancelled"
            exit 0
        fi

        echo ""
        echo "$TEST_BUCKETS" | while read -r bucket; do
            delete_bucket "$bucket"
            echo ""
        done
        ;;
esac

#------------------------------------------------------------------------------
# Summary
#------------------------------------------------------------------------------

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${GREEN}Cleanup Complete!${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
