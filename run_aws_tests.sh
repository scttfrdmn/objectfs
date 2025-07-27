#!/bin/bash

# ObjectFS AWS S3 Performance Testing Script
# Tests CargoShip optimization against real AWS S3 in us-west-2

set -e

echo "🚀 ObjectFS + CargoShip AWS S3 Performance Tests"
echo "=================================================="

# Check if test bucket is provided
if [ -z "$OBJECTFS_TEST_BUCKET" ]; then
    echo "❌ Error: OBJECTFS_TEST_BUCKET environment variable not set"
    echo ""
    echo "Usage:"
    echo "  export OBJECTFS_TEST_BUCKET=your-test-bucket-name"
    echo "  export AWS_PROFILE=aws  # Optional, defaults to 'aws'"
    echo "  ./run_aws_tests.sh"
    echo ""
    echo "The bucket should be in us-west-2 region and you should have read/write access."
    exit 1
fi

# Set default AWS profile if not provided
if [ -z "$AWS_PROFILE" ]; then
    export AWS_PROFILE=aws
    echo "📋 Using default AWS profile: aws"
else
    echo "📋 Using AWS profile: $AWS_PROFILE"
fi

echo "🪣 Test bucket: $OBJECTFS_TEST_BUCKET"
echo "🌍 Region: us-west-2"
echo "🌐 Network: 10Gbps local → 5Gbps+ internet"
echo "⚡ CargoShip optimization: ENABLED (4.6x performance target)"
echo ""

# Check if astrapi.local is mounted
if [ -d "/Volumes/Public/genomics_training" ]; then
    echo "🧬 Real genomics data available from astrapi.local"
else
    echo "⚠️  astrapi.local genomics data not available (will use synthetic data)"
fi

echo ""
echo "Starting tests..."
echo ""

# Run the AWS S3 tests with CargoShip optimization
go test -tags=aws_s3 ./tests -v -run "TestAWSS3Integration" -timeout=30m

echo ""
echo "✅ AWS S3 performance tests completed!"
echo ""
echo "Key metrics to watch:"
echo "  - Upload throughput approaching 800 MB/s target"
echo "  - CargoShip optimization enabled in logs"
echo "  - Data integrity validation passed"
echo "  - Real genomics data performance results"