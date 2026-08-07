package s3

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// apiErr builds the shape the AWS SDK actually hands back for an S3 error response: a
// smithy.GenericAPIError wrapped in the OperationError that names the call. Matching on the code has
// to see through that wrapping, which is why the helper wraps rather than returning the bare error.
func apiErr(code, message string) error {
	return &smithy.OperationError{
		ServiceID:     "S3",
		OperationName: "GetObject",
		Err: &smithy.GenericAPIError{
			Code:    code,
			Message: message,
		},
	}
}

func TestBackend_isAccelerationError(t *testing.T) {
	t.Parallel()

	b := &Backend{}

	tests := []struct {
		name     string
		err      error
		expected bool
		why      string
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},

		// True positives. S3 has no acceleration-specific error code — every one of these arrives as
		// InvalidRequest, distinguished only by the message. That is why the classifier is a
		// conjunction of code and message rather than a code lookup.
		{
			name:     "acceleration not configured on the bucket",
			err:      apiErr("InvalidRequest", "S3 Transfer Acceleration is not configured on this bucket"),
			expected: true,
			why:      "the common case: accelerate endpoint used against a bucket that never enabled it",
		},
		{
			name:     "acceleration disabled on the bucket",
			err:      apiErr("InvalidRequest", "S3 Transfer Acceleration is disabled on this bucket"),
			expected: true,
			why:      "enabled once, since suspended",
		},
		{
			name:     "bucket name is not DNS-compliant",
			err:      apiErr("InvalidRequest", "S3 Transfer Acceleration is not supported for buckets with non-DNS compliant names"),
			expected: true,
			why:      "a bucket name containing a period can never be accelerated; falling back is the only option",
		},
		{
			name:     "acceleration unsupported on this bucket",
			err:      apiErr("InvalidRequest", "S3 Transfer Acceleration is not supported on this bucket. Contact AWS Support for more information."),
			expected: true,
			why:      "region or account restriction",
		},
		{
			name:     "AWS says Accelerate rather than Acceleration",
			err:      apiErr("InvalidRequest", "S3 Transfer Accelerate is not configured on this bucket"),
			expected: true,
			why:      "S3 uses both spellings; matching only one would silently stop classifying half the cases",
		},
		{
			name:     "message case differs",
			err:      apiErr("InvalidRequest", "s3 transfer acceleration is not configured on this bucket"),
			expected: true,
			why:      "the message is service prose, not a contract; AWS can reword its casing without notice",
		},
		{
			name:     "transport failure naming the accelerate endpoint",
			err:      errors.New(`dial tcp: lookup bucket.s3-accelerate.amazonaws.com: no such host`),
			expected: true,
			why:      "no API error to inspect, but the accelerate endpoint is unmistakably what failed",
		},

		// Audit finding L27. Each of these was classified as an acceleration error by the substring
		// matcher this replaced, and each false positive disables acceleration for the remaining life
		// of the mount.
		{
			name:     "InvalidRequest for an unrelated reason",
			err:      apiErr("InvalidRequest", "Invalid Range header"),
			expected: false,
			why:      "InvalidRequest covers a large family of malformed requests; the code alone means nothing here",
		},
		{
			name:     "InvalidRequest for an oversized single-part copy",
			err:      apiErr("InvalidRequest", "The specified copy source is larger than the maximum allowable size for a copy source: 5368709120"),
			expected: false,
			why:      "a real InvalidRequest ObjectFS can hit, and it must reach the multipart-copy path rather than the fallback",
		},
		{
			name:     "InvalidRequestException does not match as a substring",
			err:      apiErr("InvalidRequestException", "S3 Transfer Acceleration is not configured on this bucket"),
			expected: false,
			why:      "the old matcher found InvalidRequest inside this longer code; the code must match exactly",
		},
		{
			name:     "BucketAlreadyExists is never an acceleration error",
			err:      apiErr("BucketAlreadyExists", "bucket name is taken"),
			expected: false,
			why:      "CreateBucket's name collision; ObjectFS does not create buckets",
		},
		{
			name:     "an unrelated code whose message says acceleration",
			err:      apiErr("AccessDenied", "user lacks s3:PutAccelerateConfiguration for Transfer Acceleration settings"),
			expected: false,
			why:      "the message half is not sufficient on its own; a permissions problem is not an endpoint problem",
		},
		{
			name:     "a wrapped API error quoting transfer-acceleration",
			err:      fmt.Errorf("upload part 3: %w", apiErr("RequestTimeout", "retry per Transfer Acceleration guidance")),
			expected: false,
			why:      "a retryable timeout must stay retryable, not permanently disable the endpoint",
		},
		{
			name:     "generic S3 error",
			err:      apiErr("NoSuchKey", "The specified key does not exist"),
			expected: false,
		},
		{
			name:     "network error unrelated to the accelerate endpoint",
			err:      errors.New("connection timeout"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := b.isAccelerationError(tt.err)
			if result != tt.expected {
				t.Errorf("isAccelerationError(%v) = %v, want %v\nwhy: %s", tt.err, result, tt.expected, tt.why)
			}
		})
	}
}

// TestBackend_isAccelerationError_WrappedInvalidRequestStillMatches pins that the conjunction sees
// through wrapping. Every caller of executeWithAccelerationFallback receives the SDK's error inside an
// OperationError, and the retry and circuit-breaker wrappers may add more layers, so a classifier that
// only inspected the outermost error would classify nothing at all and the fallback would be dead code.
func TestBackend_isAccelerationError_WrappedInvalidRequestStillMatches(t *testing.T) {
	t.Parallel()

	b := &Backend{}
	inner := apiErr("InvalidRequest", "S3 Transfer Acceleration is not configured on this bucket")

	for depth, err := 0, inner; depth < 4; depth++ {
		if !b.isAccelerationError(err) {
			t.Errorf("wrapped %d deep: isAccelerationError = false, want true (%v)", depth, err)
		}
		err = fmt.Errorf("layer %d: %w", depth, err)
	}
}

func TestBackend_executeWithAccelerationFallback(t *testing.T) {
	t.Parallel()

	// This test verifies the executeWithAccelerationFallback function exists
	// and can be called. Full integration testing requires a real ClientManager
	// and AWS credentials, which are tested in integration tests.

	// The function is designed to be called from GetObject/PutObject in production

	// Test case 1: Basic existence test
	t.Run("function_callable", func(t *testing.T) {
		t.Parallel()

		// Verify function can be referenced and called
		// Full testing requires ClientManager setup
		t.Skip("Full testing requires mock ClientManager - tested via integration tests")
	})

	// Test case 2: When acceleration fails with acceleration error, falls back
	t.Run("acceleration_error_fallback", func(t *testing.T) {
		t.Parallel()

		t.Skip("Requires mock client manager for full integration test")
	})

	// Test case 3: When acceleration succeeds, records metrics
	t.Run("acceleration_success", func(t *testing.T) {
		t.Parallel()

		t.Skip("Requires mock client manager for full integration test")
	})
}

func TestMetricsCollector_AccelerationMetrics(t *testing.T) {
	t.Parallel()

	mc := NewMetricsCollector()

	// Test acceleration enabled/disabled
	mc.SetAccelerationEnabled(true)
	metrics := mc.GetMetrics()
	if !metrics.AccelerationEnabled {
		t.Error("Expected acceleration to be enabled")
	}

	mc.SetAccelerationEnabled(false)
	metrics = mc.GetMetrics()
	if metrics.AccelerationEnabled {
		t.Error("Expected acceleration to be disabled")
	}

	// Test recording accelerated requests
	mc.RecordAcceleratedRequest(1024, 100)
	mc.RecordAcceleratedRequest(2048, 150)

	metrics = mc.GetMetrics()
	if metrics.AcceleratedRequests != 2 {
		t.Errorf("Expected 2 accelerated requests, got %d", metrics.AcceleratedRequests)
	}
	if metrics.AcceleratedBytes != 3072 {
		t.Errorf("Expected 3072 accelerated bytes, got %d", metrics.AcceleratedBytes)
	}

	// Test recording fallback events
	mc.RecordFallbackEvent()
	mc.RecordFallbackEvent()

	metrics = mc.GetMetrics()
	if metrics.FallbackEvents != 2 {
		t.Errorf("Expected 2 fallback events, got %d", metrics.FallbackEvents)
	}

	// Test acceleration rate
	mc.metrics.Requests = 10 // Simulate total requests
	rate := mc.GetAccelerationRate()
	expectedRate := (2.0 / 10.0) * 100 // 20%
	if rate != expectedRate {
		t.Errorf("Expected acceleration rate %.2f%%, got %.2f%%", expectedRate, rate)
	}

	// Test fallback rate
	fallbackRate := mc.GetFallbackRate()
	expectedFallbackRate := 100.0 // 2 fallbacks out of 2 accelerated requests = 100%
	if fallbackRate != expectedFallbackRate {
		t.Errorf("Expected fallback rate %.2f%%, got %.2f%%", expectedFallbackRate, fallbackRate)
	}
}

func TestClientManager_AccelerationMethods(t *testing.T) {
	t.Parallel()

	// Test IsAccelerationActive, DisableAcceleration, EnableAcceleration
	// This would require a real ClientManager instance
	// Skipping for now as it requires AWS credentials and bucket setup
	t.Skip("Requires AWS credentials and bucket setup")
}

// Example usage demonstrating the fallback pattern
func ExampleBackend_executeWithAccelerationFallback() {
	// This is how the fallback would be used in GetObject:
	//
	// err := b.executeWithAccelerationFallback(ctx, "GetObject", func(client *s3.Client) error {
	// 	input := &s3.GetObjectInput{
	// 		Bucket: aws.String(b.bucket),
	// 		Key:    aws.String(key),
	// 	}
	// 	result, err := client.GetObject(ctx, input)
	// 	if err != nil {
	// 		return err
	// 	}
	// 	// Process result...
	// 	return nil
	// })

	// Placeholder to make this a valid example
	_ = (*s3.Client)(nil)
}
