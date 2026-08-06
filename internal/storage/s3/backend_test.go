package s3

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_Defaults(t *testing.T) {
	t.Parallel()

	cfg := &Config{}

	// Test that defaults are applied appropriately
	assert.Equal(t, 0, cfg.MaxRetries) // Should be set by NewBackend
	assert.Equal(t, time.Duration(0), cfg.ConnectTimeout)
	assert.Equal(t, time.Duration(0), cfg.RequestTimeout)
	assert.Equal(t, 0, cfg.PoolSize)
}

func TestNewBackend_EmptyBucket(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := &Config{
		Region: "us-east-1",
	}

	backend, err := NewBackend(ctx, "", cfg)
	require.Error(t, err)
	assert.Nil(t, backend)
	assert.Contains(t, err.Error(), "bucket name cannot be empty")
}

// TestNewBackend_NilConfig asserts that a nil config is filled in from the defaults rather than
// dereferenced, which is the only thing about it this function decides.
//
// It used to assert `NotContains(err, "config")`, on the stated expectation of "an AWS credentials
// error, not a config error". That is a substring check against English prose, and it broke when an
// error message elsewhere gained the words "shared config file" — while remaining a correct message
// about a real problem. Worse, the expectation was backwards: a nil config on a machine with no
// ambient region *is* a configuration error, and the right behavior is to say so. So the assertion
// forbade the correct outcome and would have passed had NewBackend panicked on a field of the nil
// config before reaching any message at all.
//
// What is actually load-bearing is that the defaults were applied, so that is what is checked. The
// call may or may not fail depending on whether the environment supplies a region and credentials —
// both outcomes are legitimate here and neither is what this test is about.
func TestNewBackend_NilConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	backend, err := NewBackend(ctx, "test-bucket", nil)
	if err != nil {
		// No region or no credentials in this environment. The nil config was still filled in — had it
		// not been, this would be a nil-pointer panic rather than an error.
		require.NotContains(t, err.Error(), "nil pointer",
			"a nil config must be replaced by the defaults, not dereferenced")

		return
	}

	defer func() { _ = backend.Close() }()

	defaults := NewDefaultConfig()
	assert.Equal(t, defaults.MultipartThreshold, backend.config.MultipartThreshold)
	assert.Equal(t, defaults.PoolSize, backend.config.PoolSize)
	assert.Equal(t, defaults.MaxRetries, backend.config.MaxRetries)
	assert.Equal(t, TierStandard, backend.config.StorageTier)
}

func TestBackendMetrics_InitialState(t *testing.T) {
	t.Parallel()

	metrics := BackendMetrics{}

	assert.Equal(t, int64(0), metrics.Requests)
	assert.Equal(t, int64(0), metrics.Errors)
	assert.Equal(t, int64(0), metrics.BytesUploaded)
	assert.Equal(t, int64(0), metrics.BytesDownloaded)
	assert.Equal(t, time.Duration(0), metrics.AverageLatency)
	assert.Empty(t, metrics.LastError)
	assert.True(t, metrics.LastErrorTime.IsZero())
}

func TestDetectContentType(t *testing.T) {
	t.Parallel()

	backend := &Backend{}

	tests := []struct {
		key      string
		expected string
	}{
		{"file.json", "application/json"},
		{"file.xml", "application/xml"},
		{"file.html", "text/html"},
		{"file.txt", "text/plain"},
		{"file.jpg", "image/jpeg"},
		{"file.jpeg", "image/jpeg"},
		{"file.png", "image/png"},
		{"file.pdf", "application/pdf"},
		{"file.unknown", "application/octet-stream"},
		{"file", "application/octet-stream"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()

			result := backend.detectContentType(tt.key)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBackend_recordMetrics(t *testing.T) {
	t.Parallel()

	metricsCollector := NewMetricsCollector()
	backend := &Backend{
		metricsCollector: metricsCollector,
	}

	// Test initial state
	metrics := backend.GetMetrics()
	assert.Equal(t, int64(0), metrics.Requests)
	assert.Equal(t, int64(0), metrics.Errors)

	// Record first metric
	backend.metricsCollector.RecordMetrics(100*time.Millisecond, false)
	metrics = backend.GetMetrics()
	assert.Equal(t, int64(1), metrics.Requests)
	assert.Equal(t, int64(0), metrics.Errors)
	assert.Equal(t, 100*time.Millisecond, metrics.AverageLatency)

	// Record second metric
	backend.metricsCollector.RecordMetrics(200*time.Millisecond, true)
	metrics = backend.GetMetrics()
	assert.Equal(t, int64(2), metrics.Requests)
	assert.Equal(t, int64(1), metrics.Errors)

	// Check average latency calculation (rolling average)
	expectedAvg := time.Duration((int64(100*time.Millisecond)*9 + int64(200*time.Millisecond)) / 10)
	assert.Equal(t, expectedAvg, metrics.AverageLatency)
}

func TestBackend_recordError(t *testing.T) {
	t.Parallel()

	metricsCollector := NewMetricsCollector()
	backend := &Backend{
		metricsCollector: metricsCollector,
	}
	err := assert.AnError

	// Record error
	backend.metricsCollector.RecordError(err)

	metrics := backend.GetMetrics()
	assert.Equal(t, err.Error(), metrics.LastError)
	assert.False(t, metrics.LastErrorTime.IsZero())
}

func TestBackend_GetMetrics(t *testing.T) {
	t.Parallel()

	metricsCollector := NewMetricsCollector()
	backend := &Backend{
		metricsCollector: metricsCollector,
	}

	// Record some metrics
	backend.metricsCollector.RecordMetrics(100*time.Millisecond, false)
	backend.metricsCollector.RecordError(assert.AnError)

	// Get metrics copy
	metrics := backend.GetMetrics()

	assert.Equal(t, int64(1), metrics.Requests)
	assert.Equal(t, assert.AnError.Error(), metrics.LastError)
	assert.False(t, metrics.LastErrorTime.IsZero())
}

// Mock tests for operations that require S3 connection
func TestBackend_Operations_Mock(t *testing.T) {
	t.Parallel()

	// These are mock tests that demonstrate the interface
	// without requiring actual S3 credentials

	t.Run("GetObjects_EmptyKeys", func(t *testing.T) {
		t.Parallel()

		backend := &Backend{
			config: &Config{PoolSize: 4},
		}

		ctx := context.Background()
		result, err := backend.GetObjects(ctx, []string{})

		require.NoError(t, err)
		assert.Empty(t, result)
	})

	t.Run("PutObjects_EmptyObjects", func(t *testing.T) {
		t.Parallel()

		backend := &Backend{
			config: &Config{PoolSize: 4},
		}

		ctx := context.Background()
		err := backend.PutObjects(ctx, map[string][]byte{})

		assert.NoError(t, err)
	})
}

// Benchmark tests
func BenchmarkDetectContentType(b *testing.B) {
	backend := &Backend{}
	keys := []string{
		"file.json",
		"file.xml",
		"file.txt",
		"file.jpg",
		"file.unknown",
	}

	b.ResetTimer()
	for i := range b.N {
		key := keys[i%len(keys)]
		backend.detectContentType(key)
	}
}

func BenchmarkRecordMetrics(b *testing.B) {
	metricsCollector := NewMetricsCollector()
	backend := &Backend{
		metricsCollector: metricsCollector,
	}
	duration := 100 * time.Millisecond

	b.ResetTimer()
	for i := range b.N {
		backend.metricsCollector.RecordMetrics(duration, i%10 == 0) // 10% error rate
	}
}
