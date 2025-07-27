#!/bin/bash

# Test ObjectFS with real local data (no size restrictions)
set -e

echo "🚀 ObjectFS Real Data Test"
echo "=========================="

# Create a new test bucket for real data testing
BUCKET_NAME="objectfs-realdata-test-$(date +%s)"
echo "Creating test bucket: $BUCKET_NAME"

export AWS_PROFILE=aws
export OBJECTFS_TEST_BUCKET="$BUCKET_NAME"

# Create the bucket
aws s3 mb s3://$BUCKET_NAME --region us-west-2

echo "🪣 Test bucket: $OBJECTFS_TEST_BUCKET"
echo "🌍 Region: us-west-2" 
echo "📁 Testing with real files from ~/Downloads and ~/src"
echo "🌐 Network: 10Gbps local → 5Gbps+ internet"
echo "⚡ CargoShip optimization: ENABLED"
echo ""

# Run only the real data test
go test -tags=aws_s3 ./tests -v -run "TestAWSS3Integration/TestRealLocalData" -timeout=15m

# Cleanup
echo ""
echo "🧹 Cleaning up test bucket..."
aws s3 rm s3://$BUCKET_NAME --recursive 2>/dev/null || true
aws s3 rb s3://$BUCKET_NAME 2>/dev/null || true

echo "✅ Real data test completed!"