#!/usr/bin/env bash
#
# setup-aws-test.sh - Setup AWS environment for ObjectFS testing
#
# Usage:
#   ./scripts/setup-aws-test.sh           # Interactive setup
#   ./scripts/setup-aws-test.sh --auto    # Non-interactive with defaults
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

# Parse arguments
AUTO_MODE=false
if [[ "$1" == "--auto" ]]; then
    AUTO_MODE=true
fi

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

prompt() {
    local prompt_text="$1"
    local default_value="$2"
    local user_input

    if $AUTO_MODE; then
        echo "$default_value"
        return
    fi

    echo -n "$prompt_text [$default_value]: "
    read -r user_input
    echo "${user_input:-$default_value}"
}

check_command() {
    local cmd="$1"
    if ! command -v "$cmd" &> /dev/null; then
        error "Required command not found: $cmd"
        error "Please install $cmd and try again"
        exit 1
    fi
}

#------------------------------------------------------------------------------
# Pre-flight checks
#------------------------------------------------------------------------------

info "Checking prerequisites..."

# Check for required commands
check_command "aws"
check_command "jq"

success "All prerequisites met"

#------------------------------------------------------------------------------
# AWS Profile Setup
#------------------------------------------------------------------------------

echo ""
info "Setting up AWS profile..."

# Get AWS profile name
AWS_PROFILE=$(prompt "AWS profile name" "$DEFAULT_PROFILE")
export AWS_PROFILE

# Check if profile exists
if aws configure list --profile "$AWS_PROFILE" &> /dev/null; then
    success "AWS profile '$AWS_PROFILE' exists"

    # Test credentials
    if aws sts get-caller-identity --profile "$AWS_PROFILE" &> /dev/null; then
        CALLER_IDENTITY=$(aws sts get-caller-identity --profile "$AWS_PROFILE")
        ACCOUNT_ID=$(echo "$CALLER_IDENTITY" | jq -r '.Account')
        USER_ARN=$(echo "$CALLER_IDENTITY" | jq -r '.Arn')
        success "Credentials valid for account: $ACCOUNT_ID"
        info "Identity: $USER_ARN"
    else
        error "AWS credentials for profile '$AWS_PROFILE' are invalid"
        exit 1
    fi
else
    warning "AWS profile '$AWS_PROFILE' does not exist"
    info "Creating new profile..."

    if $AUTO_MODE; then
        error "Cannot create profile in auto mode"
        error "Please run 'aws configure --profile $AWS_PROFILE' manually"
        exit 1
    fi

    aws configure --profile "$AWS_PROFILE"
fi

#------------------------------------------------------------------------------
# Region Setup
#------------------------------------------------------------------------------

echo ""
info "Setting up AWS region..."

# Get region
CURRENT_REGION=$(aws configure get region --profile "$AWS_PROFILE" 2>/dev/null || echo "$DEFAULT_REGION")
AWS_REGION=$(prompt "AWS region" "$CURRENT_REGION")
export AWS_REGION

# Set region if different
if [[ "$AWS_REGION" != "$CURRENT_REGION" ]]; then
    aws configure set region "$AWS_REGION" --profile "$AWS_PROFILE"
    success "Region set to: $AWS_REGION"
else
    success "Using region: $AWS_REGION"
fi

#------------------------------------------------------------------------------
# Test Bucket Setup
#------------------------------------------------------------------------------

echo ""
info "Setting up test bucket..."

# Generate bucket name
USERNAME=$(whoami)
BUCKET_NAME="${DEFAULT_BUCKET_PREFIX}-${USERNAME}"
BUCKET_NAME=$(prompt "Test bucket name" "$BUCKET_NAME")
export OBJECTFS_TEST_BUCKET="$BUCKET_NAME"

# Check if bucket exists
if aws s3 ls "s3://$BUCKET_NAME" --profile "$AWS_PROFILE" &> /dev/null; then
    success "Test bucket exists: $BUCKET_NAME"
else
    info "Creating test bucket: $BUCKET_NAME"

    if [[ "$AWS_REGION" == "us-east-1" ]]; then
        # us-east-1 doesn't need LocationConstraint
        aws s3 mb "s3://$BUCKET_NAME" --profile "$AWS_PROFILE"
    else
        # Other regions need LocationConstraint
        aws s3 mb "s3://$BUCKET_NAME" \
            --region "$AWS_REGION" \
            --profile "$AWS_PROFILE"
    fi

    success "Test bucket created: $BUCKET_NAME"
fi

#------------------------------------------------------------------------------
# Bucket Lifecycle Policy
#------------------------------------------------------------------------------

echo ""
info "Setting up bucket lifecycle policy..."

# Create lifecycle policy for automatic cleanup
LIFECYCLE_POLICY=$(cat <<EOF
{
  "Rules": [
    {
      "Id": "DeleteOldTestData",
      "Status": "Enabled",
      "Filter": {
        "Prefix": ""
      },
      "Expiration": {
        "Days": 7
      },
      "NoncurrentVersionExpiration": {
        "NoncurrentDays": 1
      },
      "AbortIncompleteMultipartUpload": {
        "DaysAfterInitiation": 1
      }
    }
  ]
}
EOF
)

# Apply lifecycle policy
echo "$LIFECYCLE_POLICY" | aws s3api put-bucket-lifecycle-configuration \
    --bucket "$BUCKET_NAME" \
    --lifecycle-configuration file:///dev/stdin \
    --profile "$AWS_PROFILE"

success "Lifecycle policy applied (7-day expiration)"

#------------------------------------------------------------------------------
# Bucket Tags
#------------------------------------------------------------------------------

echo ""
info "Setting bucket tags..."

# Tag bucket for easy identification and cost tracking
aws s3api put-bucket-tagging \
    --bucket "$BUCKET_NAME" \
    --tagging "TagSet=[{Key=Project,Value=ObjectFS},{Key=Purpose,Value=Testing},{Key=Owner,Value=$USERNAME}]" \
    --profile "$AWS_PROFILE"

success "Bucket tagged for cost tracking"

#------------------------------------------------------------------------------
# Generate Environment File
#------------------------------------------------------------------------------

echo ""
info "Generating environment file..."

ENV_FILE=".env.test"

cat > "$ENV_FILE" <<EOF
# ObjectFS Test Environment
# Generated: $(date)

# AWS Configuration
export AWS_PROFILE="$AWS_PROFILE"
export AWS_REGION="$AWS_REGION"
export OBJECTFS_TEST_BUCKET="$BUCKET_NAME"

# Optional: Uncomment to use explicit credentials instead of profile
# export AWS_ACCESS_KEY_ID="your-access-key-id"
# export AWS_SECRET_ACCESS_KEY="your-secret-access-key"

# Test Configuration
export OBJECTFS_TEST_PREFIX="test-$(date +%Y%m%d)-"
export OBJECTFS_TEST_CLEANUP="true"

# Go Test Flags
export GOTEST_FLAGS="-v -count=1"
EOF

success "Environment file created: $ENV_FILE"

#------------------------------------------------------------------------------
# IAM Policy Recommendation
#------------------------------------------------------------------------------

echo ""
info "Generating IAM policy recommendation..."

IAM_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ObjectFSTestBucketAccess",
      "Effect": "Allow",
      "Action": [
        "s3:CreateBucket",
        "s3:DeleteBucket",
        "s3:ListBucket",
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:GetObjectVersion",
        "s3:DeleteObjectVersion",
        "s3:PutBucketLifecycleConfiguration",
        "s3:GetBucketLifecycleConfiguration",
        "s3:PutBucketTagging",
        "s3:GetBucketTagging",
        "s3:ListBucketMultipartUploads",
        "s3:AbortMultipartUpload",
        "s3:ListMultipartUploadParts"
      ],
      "Resource": [
        "arn:aws:s3:::$BUCKET_NAME",
        "arn:aws:s3:::$BUCKET_NAME/*",
        "arn:aws:s3:::${BUCKET_PREFIX}-*",
        "arn:aws:s3:::${BUCKET_PREFIX}-*/*"
      ]
    },
    {
      "Sid": "ObjectFSTestBucketList",
      "Effect": "Allow",
      "Action": [
        "s3:ListAllMyBuckets",
        "s3:GetBucketLocation"
      ],
      "Resource": "*"
    }
  ]
}
EOF
)

IAM_POLICY_FILE="iam-policy-objectfs-test.json"
echo "$IAM_POLICY" > "$IAM_POLICY_FILE"

success "IAM policy saved to: $IAM_POLICY_FILE"
info "To apply this policy to an IAM user or role, use:"
info "  aws iam put-user-policy --user-name <username> --policy-name ObjectFSTest --policy-document file://$IAM_POLICY_FILE"

#------------------------------------------------------------------------------
# Test Connection
#------------------------------------------------------------------------------

echo ""
info "Testing S3 connection..."

# Write a test object
TEST_KEY="test-$(date +%s).txt"
TEST_CONTENT="ObjectFS test at $(date)"

echo "$TEST_CONTENT" | aws s3 cp - "s3://$BUCKET_NAME/$TEST_KEY" --profile "$AWS_PROFILE"
success "Test write successful"

# Read back test object
RETRIEVED=$(aws s3 cp "s3://$BUCKET_NAME/$TEST_KEY" - --profile "$AWS_PROFILE")
if [[ "$RETRIEVED" == "$TEST_CONTENT" ]]; then
    success "Test read successful"
else
    error "Test read failed - content mismatch"
fi

# Delete test object
aws s3 rm "s3://$BUCKET_NAME/$TEST_KEY" --profile "$AWS_PROFILE"
success "Test cleanup successful"

#------------------------------------------------------------------------------
# Summary
#------------------------------------------------------------------------------

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${GREEN}Setup Complete!${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "AWS Configuration:"
echo "  Profile: $AWS_PROFILE"
echo "  Region: $AWS_REGION"
echo "  Test Bucket: $BUCKET_NAME"
echo ""
echo "To use this environment:"
echo "  source .env.test"
echo ""
echo "Run tests:"
echo "  # Quick tests (skip AWS integration)"
echo "  go test -short ./..."
echo ""
echo "  # All tests including AWS integration"
echo "  go test ./..."
echo ""
echo "  # Specific AWS integration tests"
echo "  go test -v ./tests/aws/..."
echo ""
echo "Cleanup:"
echo "  # Delete test bucket (when done)"
echo "  aws s3 rm s3://$BUCKET_NAME --recursive --profile $AWS_PROFILE"
echo "  aws s3 rb s3://$BUCKET_NAME --profile $AWS_PROFILE"
echo ""
echo "Cost Management:"
echo "  - Lifecycle policy: Auto-deletes objects >7 days old"
echo "  - Bucket tags: Enable cost tracking by Project/Owner"
echo "  - Monitor costs: aws ce get-cost-and-usage"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
