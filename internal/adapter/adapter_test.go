package adapter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/health"
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

// TestSizeOrDefault replaces TestParseSize, whose case names pinned the behavior that had to go:
// "empty string defaults to 1GB", "invalid format defaults to 1GB". adapter.parseSize returned 1 GiB
// for any string it could not read, so `cache_size: 2G` — a real spelling, missing only the B — and
// `cache_size: tpyo` both configured a 1 GiB cache, silently, with the configured value discarded and
// nothing logged. Three other parsers disagreed with it about the same inputs.
//
// utils.ParseBytes is now the only size parser in the repository, and the two behaviors the adapter
// still owns are the ones asserted here: an empty value means "use the default", and a value that
// somehow reaches Start unparseable falls back to the default rather than to a wrong number. The
// second arm is unreachable through New — Configuration.Validate rejects it first, which
// TestValidateRejectsUnparseableSizes covers — so this asserts the backstop, not the path.
func TestSizeOrDefault(t *testing.T) {
	t.Parallel()

	const fallback = 7 << 20 // a value no default anywhere shares, so a match means this arm ran

	tests := []struct {
		name  string
		value string
		want  int64
		why   string
	}{
		{
			name:  "empty means the default",
			value: "",
			want:  fallback,
			why:   "an unset key must take the documented default, not zero",
		},
		{
			name:  "a configured size is used as written",
			value: "512MB",
			want:  512 << 20,
		},
		{
			name:  "units are binary and case-insensitive",
			value: "2gb",
			want:  2 << 30,
		},
		{
			name:  "a plain number is bytes",
			value: "4096",
			want:  4096,
		},
		{
			name:  "zero is a value, not an absence",
			value: "0",
			want:  0,
			why: "several callers read zero as \"disabled\" or \"no limit\"; substituting the " +
				"default here would make the feature unswitchable-off",
		},
		{
			name:  "an unparseable size falls back rather than guessing",
			value: "tpyo",
			want:  fallback,
			why:   "parseSize returned 1 GiB here, which is neither the default nor what was written",
		},
		{
			name:  "the near-miss spelling falls back rather than truncating",
			value: "64MiB",
			want:  fallback,
			why: "fmt.Sscanf read this as 64 bytes — it consumed the digits, stopped at the M, and " +
				"reported success, so a 64 MiB setting became a 64-byte one",
		},
	}

	a := &Adapter{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := a.sizeOrDefault("test.setting", tt.value, fallback)
			if got != tt.want {
				t.Errorf("sizeOrDefault(%q) = %d, want %d. %s", tt.value, got, tt.want, tt.why)
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

func TestBuildS3Config(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		region         string
		endpoint       string
		forcePathStyle bool
		useAccelerate  bool
	}{
		{
			name:   "default us-east-1",
			region: "us-east-1",
		},
		{
			name:   "eu-west-1",
			region: "eu-west-1",
		},
		{
			name:           "custom endpoint with force path style",
			region:         "us-east-1",
			endpoint:       "http://localhost:9000",
			forcePathStyle: true,
		},
		{
			name:          "acceleration enabled",
			region:        "us-west-2",
			useAccelerate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := createTestConfig()
			cfg.Storage.S3.Region = tt.region
			cfg.Storage.S3.Endpoint = tt.endpoint
			cfg.Storage.S3.ForcePathStyle = tt.forcePathStyle
			cfg.Storage.S3.UseAcceleration = tt.useAccelerate

			adapter := &Adapter{config: cfg}
			s3cfg := adapter.buildS3Config()

			if s3cfg.Region != tt.region {
				t.Errorf("Region: got %q, want %q", s3cfg.Region, tt.region)
			}
			if s3cfg.Endpoint != tt.endpoint {
				t.Errorf("Endpoint: got %q, want %q", s3cfg.Endpoint, tt.endpoint)
			}
			if s3cfg.ForcePathStyle != tt.forcePathStyle {
				t.Errorf("ForcePathStyle: got %v, want %v", s3cfg.ForcePathStyle, tt.forcePathStyle)
			}
			if s3cfg.UseAccelerate != tt.useAccelerate {
				t.Errorf("UseAccelerate: got %v, want %v", s3cfg.UseAccelerate, tt.useAccelerate)
			}
		})
	}
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
			ReadAhead: config.ReadAheadConfig{
				Strategy:           "predictive",
				MaxConcurrentFetch: 4,
			},
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
			// sse-s3 rather than the zero value, so this fixture carries a mode that produces a header
			// and the tests using it exercise the encryption mapping rather than skipping past it. The
			// two booleans this replaces — in_transit and at_rest, both true — were the fields that
			// made audit finding P-7 look configured while nothing read them.
			Encryption: config.EncryptionConfig{
				Mode: config.EncryptionModeS3,
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

// Health monitor integration tests

func TestAdapterHealthMonitor_InitiallyNil(t *testing.T) {
	t.Parallel()
	cfg := createTestConfig()
	a, err := New(context.Background(), "s3://test-bucket", "/mnt/test", cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if a.monitor != nil {
		t.Error("expected monitor to be nil immediately after New()")
	}
}

func TestAdapterStop_NilMonitor_OK(t *testing.T) {
	t.Parallel()
	// Verify Stop() handles a nil monitor without panicking.
	a := &Adapter{
		config:     createTestConfig(),
		bucketName: "test-bucket",
		started:    true,
		// monitor is nil — the nil guard must protect against this
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop() with nil monitor returned unexpected error: %v", err)
	}
}

func TestAdapterStop_StopsHealthMonitor(t *testing.T) {
	t.Parallel()

	// Build a real health.Monitor (no HTTP, no background checks firing quickly)
	// and attach it to a manually constructed "started" adapter.
	mon, err := health.NewMonitor(&health.MonitorConfig{
		Enabled:          true,
		MonitorInterval:  30 * time.Second,
		AlertingEnabled:  false,
		ReportingEnabled: false,
		HealthCheckConfig: &health.Config{
			Enabled:       true,
			CheckInterval: 30 * time.Second,
			Timeout:       5 * time.Second,
			HTTPEnabled:   false, // don't bind a port in tests
		},
	})
	if err != nil {
		t.Fatalf("NewMonitor: %v", err)
	}
	if err := mon.Start(context.Background()); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}

	a := &Adapter{
		config:     createTestConfig(),
		bucketName: "test-bucket",
		started:    true,
		monitor:    mon,
	}

	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}
	if a.started {
		t.Error("adapter.started should be false after Stop()")
	}
}

func TestHealthComponent_Interface(t *testing.T) {
	t.Parallel()

	called := false
	hc := &healthComponent{
		name:     "test-component",
		compType: "storage",
		fn: func(_ context.Context) error {
			called = true
			return nil
		},
	}

	if hc.GetComponentName() != "test-component" {
		t.Errorf("GetComponentName() = %q, want %q", hc.GetComponentName(), "test-component")
	}
	if hc.GetComponentType() != "storage" {
		t.Errorf("GetComponentType() = %q, want %q", hc.GetComponentType(), "storage")
	}
	if err := hc.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck() = %v, want nil", err)
	}
	if !called {
		t.Error("expected health check function to be called")
	}
}

func TestHealthComponent_Error(t *testing.T) {
	t.Parallel()

	hc := &healthComponent{
		name:     "failing-component",
		compType: "cache",
		fn: func(_ context.Context) error {
			return fmt.Errorf("component unavailable")
		},
	}

	if err := hc.HealthCheck(context.Background()); err == nil {
		t.Error("HealthCheck() should return an error for a failing component")
	}
}

// TestWriteBufferConfig_CorrectSizes is deleted rather than ported, and what it was guarding is worth
// recording because the reason it can go is not "the bug is fixed".
//
// It asserted that MaxBufferSize was parseSize(MaxMemory) and FlushThreshold was three quarters of
// that, after a bug divided them by 100 and 200. Both fields belonged to internal/buffer.WriteBuffer,
// which the write-path rebuild replaced with internal/vfs.Writer — and vfs.NewWriter takes no
// configuration at all. So the test was computing a formula over a config value in a package that no
// longer reads it: parseSize("512MB")*3/4 == 384 MiB is arithmetic, and it passed for the same reason
// 2+2 does.
//
// The live fact underneath is that write_buffer.max_memory, max_buffers and flush_interval currently
// have no reader. Nothing bounds how many dirty bytes vfs.Writer accumulates. That is a real gap and
// is not this task's — it needs backpressure in the writer, not a mapping — so the keys are marked
// "not yet wired" in examples/config.yaml, which is the form of honesty available without inventing a
// limit here. A test that asserted a bound would assert one that does not exist.

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
