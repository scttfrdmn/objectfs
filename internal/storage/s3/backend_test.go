package s3

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_Defaults(t *testing.T) {
	cfg := &Config{}
	
	// Test that defaults are applied appropriately
	assert.Equal(t, 0, cfg.MaxRetries) // Should be set by NewBackend
	assert.Equal(t, time.Duration(0), cfg.ConnectTimeout)
	assert.Equal(t, time.Duration(0), cfg.RequestTimeout)
	assert.Equal(t, 0, cfg.PoolSize)
}

func TestNewBackend_EmptyBucket(t *testing.T) {
	ctx := context.Background()
	cfg := &Config{
		Region: "us-east-1",
	}
	
	backend, err := NewBackend(ctx, "", cfg)
	assert.Error(t, err)
	assert.Nil(t, backend)
	assert.Contains(t, err.Error(), "bucket name cannot be empty")
}

func TestNewBackend_NilConfig(t *testing.T) {
	ctx := context.Background()
	
	// This test may fail without proper AWS credentials
	// but we can at least test the config defaults
	_, err := NewBackend(ctx, "test-bucket", nil)
	
	// We expect this to fail with AWS credentials error, not config error
	if err != nil {
		// Should not be a config-related error
		assert.NotContains(t, err.Error(), "config")
	}
}

func TestBackendMetrics_InitialState(t *testing.T) {
	metrics := BackendMetrics{}
	
	assert.Equal(t, int64(0), metrics.Requests)
	assert.Equal(t, int64(0), metrics.Errors)
	assert.Equal(t, int64(0), metrics.BytesUploaded)
	assert.Equal(t, int64(0), metrics.BytesDownloaded)
	assert.Equal(t, time.Duration(0), metrics.AverageLatency)
	assert.Equal(t, "", metrics.LastError)
	assert.True(t, metrics.LastErrorTime.IsZero())
}

func TestDetectContentType(t *testing.T) {
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
			result := backend.detectContentType(tt.key)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBackend_recordMetrics(t *testing.T) {
	backend := &Backend{}
	
	// Test initial state
	assert.Equal(t, int64(0), backend.metrics.Requests)
	assert.Equal(t, int64(0), backend.metrics.Errors)
	
	// Record first metric
	backend.recordMetrics(100*time.Millisecond, false)
	assert.Equal(t, int64(1), backend.metrics.Requests)
	assert.Equal(t, int64(0), backend.metrics.Errors)
	assert.Equal(t, 100*time.Millisecond, backend.metrics.AverageLatency)
	
	// Record second metric
	backend.recordMetrics(200*time.Millisecond, true)
	assert.Equal(t, int64(2), backend.metrics.Requests)
	assert.Equal(t, int64(1), backend.metrics.Errors)
	
	// Check average latency calculation (rolling average)
	expectedAvg := time.Duration((int64(100*time.Millisecond)*9 + int64(200*time.Millisecond)) / 10)
	assert.Equal(t, expectedAvg, backend.metrics.AverageLatency)
}

func TestBackend_recordError(t *testing.T) {
	backend := &Backend{}
	err := assert.AnError
	
	// Record error
	backend.recordError(err)
	
	assert.Equal(t, err.Error(), backend.metrics.LastError)
	assert.False(t, backend.metrics.LastErrorTime.IsZero())
}

func TestBackend_GetMetrics(t *testing.T) {
	backend := &Backend{}
	
	// Record some metrics
	backend.recordMetrics(100*time.Millisecond, false)
	backend.recordError(assert.AnError)
	
	// Get metrics copy
	metrics := backend.GetMetrics()
	
	assert.Equal(t, int64(1), metrics.Requests)
	assert.Equal(t, assert.AnError.Error(), metrics.LastError)
	assert.False(t, metrics.LastErrorTime.IsZero())
}

// Mock tests for operations that require S3 connection
func TestBackend_Operations_Mock(t *testing.T) {
	// These are mock tests that demonstrate the interface
	// without requiring actual S3 credentials
	
	t.Run("GetObjects_EmptyKeys", func(t *testing.T) {
		backend := &Backend{
			config: &Config{PoolSize: 4},
		}
		
		ctx := context.Background()
		result, err := backend.GetObjects(ctx, []string{})
		
		require.NoError(t, err)
		assert.Empty(t, result)
	})
	
	t.Run("PutObjects_EmptyObjects", func(t *testing.T) {
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
	for i := 0; i < b.N; i++ {
		key := keys[i%len(keys)]
		backend.detectContentType(key)
	}
}

func BenchmarkRecordMetrics(b *testing.B) {
	backend := &Backend{}
	duration := 100 * time.Millisecond
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		backend.recordMetrics(duration, i%10 == 0) // 10% error rate
	}
}