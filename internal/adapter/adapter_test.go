package adapter

import (
	"context"
	"testing"
	"time"

	"github.com/objectfs/objectfs/internal/config"
)

func TestValidateStorageURI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		uri         string
		wantErr     bool
		errContains string
	}{
		{
			name:    "valid s3 URI",
			uri:     "s3://my-bucket",
			wantErr: false,
		},
		{
			name:    "valid s3 URI with path",
			uri:     "s3://my-bucket/path/to/prefix",
			wantErr: false,
		},
		{
			name:        "s3 URI without bucket",
			uri:         "s3://",
			wantErr:     true,
			errContains: "bucket name",
		},
		{
			name:        "unsupported scheme",
			uri:         "gcs://my-bucket",
			wantErr:     true,
			errContains: "unsupported storage scheme",
		},
		{
			name:        "unsupported azure scheme",
			uri:         "azure://container",
			wantErr:     true,
			errContains: "unsupported storage scheme",
		},
		{
			name:        "http scheme not supported",
			uri:         "http://bucket",
			wantErr:     true,
			errContains: "unsupported storage scheme",
		},
		{
			name:        "https scheme not supported",
			uri:         "https://bucket",
			wantErr:     true,
			errContains: "unsupported storage scheme",
		},
		{
			name:        "invalid URI",
			uri:         "://invalid",
			wantErr:     true,
			errContains: "failed to parse URI",
		},
		{
			name:        "empty URI",
			uri:         "",
			wantErr:     true,
			errContains: "unsupported storage scheme",
		},
		{
			name:    "s3 URI with dots in bucket name",
			uri:     "s3://my.bucket.with.dots",
			wantErr: false,
		},
		{
			name:    "s3 URI with hyphens",
			uri:     "s3://my-bucket-name",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStorageURI(tt.uri)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateStorageURI() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !contains(err.Error(), tt.errContains) {
					t.Errorf("validateStorageURI() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sizeStr  string
		expected int64
	}{
		{
			name:     "gigabytes",
			sizeStr:  "2GB",
			expected: 2 * 1024 * 1024 * 1024,
		},
		{
			name:     "megabytes",
			sizeStr:  "512MB",
			expected: 512 * 1024 * 1024,
		},
		{
			name:     "kilobytes",
			sizeStr:  "100KB",
			expected: 100 * 1024,
		},
		{
			name:     "bytes",
			sizeStr:  "1024B",
			expected: 1024,
		},
		{
			name:     "lowercase gb",
			sizeStr:  "1gb",
			expected: 1 * 1024 * 1024 * 1024,
		},
		{
			name:     "lowercase mb",
			sizeStr:  "256mb",
			expected: 256 * 1024 * 1024,
		},
		{
			name:     "with spaces",
			sizeStr:  "  4GB  ",
			expected: 4 * 1024 * 1024 * 1024,
		},
		{
			name:     "single digit",
			sizeStr:  "1GB",
			expected: 1 * 1024 * 1024 * 1024,
		},
		{
			name:     "large number",
			sizeStr:  "10GB",
			expected: 10 * 1024 * 1024 * 1024,
		},
		{
			name:     "empty string defaults to 1GB",
			sizeStr:  "",
			expected: 1024 * 1024 * 1024,
		},
		{
			name:     "invalid format defaults to 1GB",
			sizeStr:  "invalid",
			expected: 1024 * 1024 * 1024,
		},
		{
			name:     "plain number is treated as bytes",
			sizeStr:  "1024",
			expected: 1024, // parseSize interprets plain numbers literally
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseSize(tt.sizeStr)
			if result != tt.expected {
				t.Errorf("parseSize(%q) = %d, expected %d", tt.sizeStr, result, tt.expected)
			}
		})
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("valid configuration", func(t *testing.T) {
		cfg := createTestConfig()
		adapter, err := New(ctx, "s3://test-bucket", "/mnt/test", cfg)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		if adapter == nil {
			t.Fatal("New() returned nil adapter")
		}
		if adapter.storageURI != "s3://test-bucket" {
			t.Errorf("adapter.storageURI = %q, want %q", adapter.storageURI, "s3://test-bucket")
		}
		if adapter.mountPoint != "/mnt/test" {
			t.Errorf("adapter.mountPoint = %q, want %q", adapter.mountPoint, "/mnt/test")
		}
		if adapter.bucketName != "test-bucket" {
			t.Errorf("adapter.bucketName = %q, want %q", adapter.bucketName, "test-bucket")
		}
		if adapter.started {
			t.Error("adapter.started = true, want false")
		}
	})

	t.Run("invalid storage URI", func(t *testing.T) {
		cfg := createTestConfig()
		_, err := New(ctx, "gcs://invalid", "/mnt/test", cfg)
		if err == nil {
			t.Error("New() with invalid URI should return error")
		}
		if !contains(err.Error(), "invalid storage URI") {
			t.Errorf("error should contain 'invalid storage URI', got %v", err)
		}
	})

	t.Run("empty bucket name", func(t *testing.T) {
		cfg := createTestConfig()
		_, err := New(ctx, "s3://", "/mnt/test", cfg)
		if err == nil {
			t.Error("New() with empty bucket should return error")
		}
		// Accept either error message since validateStorageURI catches it first
		if !contains(err.Error(), "bucket name") && !contains(err.Error(), "S3 URI must include bucket name") {
			t.Errorf("error should contain 'bucket name', got %v", err)
		}
	})

	t.Run("invalid configuration", func(t *testing.T) {
		cfg := &config.Configuration{
			// Invalid config with missing required fields
			Performance: config.PerformanceConfig{
				CacheSize:      "", // Invalid empty cache size
				MaxConcurrency: -1, // Invalid negative concurrency
			},
		}
		_, err := New(ctx, "s3://test-bucket", "/mnt/test", cfg)
		if err == nil {
			t.Error("New() with invalid config should return error")
		}
		if !contains(err.Error(), "invalid configuration") {
			t.Errorf("error should contain 'invalid configuration', got %v", err)
		}
	})

	t.Run("URI with path prefix", func(t *testing.T) {
		cfg := createTestConfig()
		adapter, err := New(ctx, "s3://test-bucket/path/prefix", "/mnt/test", cfg)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		if adapter.bucketName != "test-bucket" {
			t.Errorf("adapter.bucketName = %q, want %q", adapter.bucketName, "test-bucket")
		}
	})

	t.Run("bucket with dots", func(t *testing.T) {
		cfg := createTestConfig()
		adapter, err := New(ctx, "s3://my.bucket.with.dots", "/mnt/test", cfg)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		if adapter.bucketName != "my.bucket.with.dots" {
			t.Errorf("adapter.bucketName = %q, want %q", adapter.bucketName, "my.bucket.with.dots")
		}
	})
}

func TestAdapterDoubleStart(t *testing.T) {
	t.Parallel()

	// This test verifies that calling Start() twice returns an error
	// We can't actually start the adapter in tests without real dependencies,
	// but we can test the state management logic by manipulating the started flag

	cfg := createTestConfig()
	adapter := &Adapter{
		storageURI: "s3://test-bucket",
		mountPoint: "/mnt/test",
		config:     cfg,
		bucketName: "test-bucket",
		started:    true, // Manually set as started
	}

	ctx := context.Background()
	err := adapter.Start(ctx)
	if err == nil {
		t.Error("Start() on already started adapter should return error")
	}
	if !contains(err.Error(), "already started") {
		t.Errorf("error should contain 'already started', got %v", err)
	}
}

func TestAdapterStopNotStarted(t *testing.T) {
	t.Parallel()

	cfg := createTestConfig()
	adapter := &Adapter{
		storageURI: "s3://test-bucket",
		mountPoint: "/mnt/test",
		config:     cfg,
		bucketName: "test-bucket",
		started:    false,
	}

	ctx := context.Background()
	err := adapter.Stop(ctx)
	if err == nil {
		t.Error("Stop() on non-started adapter should return error")
	}
	if !contains(err.Error(), "not started") {
		t.Errorf("error should contain 'not started', got %v", err)
	}
}

// createTestConfig creates a valid test configuration
func createTestConfig() *config.Configuration {
	return &config.Configuration{
		Global: config.GlobalConfig{
			LogLevel:    "INFO",
			LogFile:     "",
			MetricsPort: 9090,
			HealthPort:  8080,
			ProfilePort: 6060,
		},
		Storage: config.StorageConfig{
			S3: config.S3Config{
				Region:          "us-east-1",
				Endpoint:        "",
				Profile:         "",
				UseAcceleration: false,
				ForcePathStyle:  false,
			},
		},
		Performance: config.PerformanceConfig{
			CacheSize:          "2GB",
			WriteBufferSize:    "16MB",
			MaxConcurrency:     100,
			ReadAheadSize:      "4MB",
			CompressionEnabled: true,
			ConnectionPoolSize: 8,
			PredictiveCaching:  false,
			MLModelPath:        "",
			MultilevelCaching:  false,
		},
		Cache: config.CacheConfig{
			EvictionPolicy: "lru",
			TTL:            5 * time.Minute,
			MaxEntries:     10000,
			PersistentCache: config.PersistentCacheConfig{
				Enabled:   true,
				Directory: "/tmp/objectfs-cache",
				MaxSize:   "10GB",
			},
		},
		WriteBuffer: config.WriteBufferConfig{
			FlushInterval: 30 * time.Second,
			MaxBuffers:    1000,
			MaxMemory:     "512MB",
			Compression: config.CompressionConfig{
				Enabled:   true,
				MinSize:   "1KB",
				Algorithm: "gzip",
				Level:     6,
			},
		},
		Network: config.NetworkConfig{
			Timeouts: config.TimeoutConfig{
				Connect: 10 * time.Second,
				Read:    60 * time.Second,
				Write:   60 * time.Second,
			},
			Retry: config.RetryConfig{
				MaxAttempts: 3,
				BaseDelay:   1 * time.Second,
				MaxDelay:    30 * time.Second,
			},
			CircuitBreaker: config.CircuitBreakerConfig{
				Enabled:          true,
				FailureThreshold: 5,
				Timeout:          60 * time.Second,
			},
		},
		Security: config.SecurityConfig{
			Enabled:     false,
			AuthMethod:  "none",
			TLSEnabled:  false,
			TLSCertPath: "",
			TLSKeyPath:  "",
			TLS: config.TLSConfig{
				VerifyCertificates: true,
				MinVersion:         "1.2",
			},
			Encryption: config.EncryptionConfig{
				InTransit: true,
				AtRest:    true,
			},
		},
		Monitoring: config.MonitoringConfig{
			Enabled:         true,
			MetricsAddr:     ":9090",
			EnablePprof:     false,
			HealthCheckAddr: ":8081",
			OpenTelemetry: config.OpenTelemetryConfig{
				Enabled:     false,
				Endpoint:    "localhost:4317",
				ServiceName: "objectfs",
			},
			Metrics: config.MetricsConfig{
				Enabled:      true,
				Prometheus:   true,
				CustomLabels: map[string]string{"env": "test"},
			},
			HealthChecks: config.HealthChecksConfig{
				Enabled:  true,
				Interval: 30 * time.Second,
				Timeout:  5 * time.Second,
			},
			Logging: config.LoggingConfig{
				Structured: true,
				Format:     "json",
				Sampling: config.SamplingConfig{
					Enabled: true,
					Rate:    1000,
				},
			},
		},
		Features: config.FeatureConfig{
			Prefetching:           true,
			BatchOperations:       true,
			SmallFileOptimization: true,
			MetadataCaching:       true,
			OfflineMode:           false,
		},
		Cluster: config.ClusterConfig{
			Enabled:           false,
			NodeID:            "",
			ListenAddr:        "0.0.0.0:8080",
			AdvertiseAddr:     "127.0.0.1:8080",
			SeedNodes:         []string{},
			ReplicationFactor: 3,
			ConsistencyLevel:  "eventual",
		},
	}
}

// contains checks if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
