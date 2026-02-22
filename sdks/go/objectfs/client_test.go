package objectfs

import (
	"context"
	"errors"
	"os"
	"testing"
)

// requireAWS skips the test unless AWS_ACCESS_KEY_ID is set.
// Consistent with the project's pattern: integration tests require real AWS credentials.
func requireAWS(t *testing.T) {
	t.Helper()
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Skip("AWS_ACCESS_KEY_ID not set; skipping integration test")
	}
}

// --- Option tests (no backend required) ---

func TestDefaultOptions(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	if o.region != "us-east-1" {
		t.Errorf("region: got %q, want %q", o.region, "us-east-1")
	}
	if o.cacheSize != "512MB" {
		t.Errorf("cacheSize: got %q, want %q", o.cacheSize, "512MB")
	}
	if o.maxConcurrency != 32 {
		t.Errorf("maxConcurrency: got %d, want 32", o.maxConcurrency)
	}
	if o.logLevel != "INFO" {
		t.Errorf("logLevel: got %q, want %q", o.logLevel, "INFO")
	}
	if o.metricsPort != 8080 {
		t.Errorf("metricsPort: got %d, want 8080", o.metricsPort)
	}
	if o.healthPort != 8081 {
		t.Errorf("healthPort: got %d, want 8081", o.healthPort)
	}
	if o.tlsEnabled {
		t.Error("tlsEnabled: got true, want false")
	}
}

func TestWithRegion(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	WithRegion("eu-west-1")(&o)
	if o.region != "eu-west-1" {
		t.Errorf("region: got %q, want %q", o.region, "eu-west-1")
	}
}

func TestWithEndpoint(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	WithEndpoint("http://localhost:9000")(&o)
	if o.endpoint != "http://localhost:9000" {
		t.Errorf("endpoint: got %q, want %q", o.endpoint, "http://localhost:9000")
	}
}

func TestWithCacheSize(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	WithCacheSize("2GB")(&o)
	if o.cacheSize != "2GB" {
		t.Errorf("cacheSize: got %q, want %q", o.cacheSize, "2GB")
	}
}

func TestWithMaxConcurrency(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	WithMaxConcurrency(64)(&o)
	if o.maxConcurrency != 64 {
		t.Errorf("maxConcurrency: got %d, want 64", o.maxConcurrency)
	}
}

func TestWithLogLevel(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	WithLogLevel("DEBUG")(&o)
	if o.logLevel != "DEBUG" {
		t.Errorf("logLevel: got %q, want %q", o.logLevel, "DEBUG")
	}
}

func TestWithMetricsPort(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	WithMetricsPort(9090)(&o)
	if o.metricsPort != 9090 {
		t.Errorf("metricsPort: got %d, want 9090", o.metricsPort)
	}
}

func TestWithHealthPort(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	WithHealthPort(9091)(&o)
	if o.healthPort != 9091 {
		t.Errorf("healthPort: got %d, want 9091", o.healthPort)
	}
}

func TestWithTLS(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	WithTLS()(&o)
	if !o.tlsEnabled {
		t.Error("tlsEnabled: got false, want true")
	}
}

// --- Validation tests (no backend required) ---

func TestNew_EmptyBucket(t *testing.T) {
	t.Parallel()
	_, err := New(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty bucket, got nil")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestValidateOptions_ZeroConcurrency(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	o.maxConcurrency = 0
	err := validateOptions(o)
	if err == nil {
		t.Fatal("expected error for zero maxConcurrency")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestValidateOptions_SamePorts(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	o.metricsPort = 9000
	o.healthPort = 9000
	err := validateOptions(o)
	if err == nil {
		t.Fatal("expected error for same metrics/health port")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("expected ErrInvalidConfig, got %v", err)
	}
}

// --- Mount/Unmount state tests (no backend/FUSE required) ---

func TestIsMounted_False(t *testing.T) {
	t.Parallel()
	c := &Client{bucket: "test", mounted: false}
	if c.IsMounted() {
		t.Error("IsMounted: got true, want false for new client")
	}
}

func TestMount_AlreadyMounted(t *testing.T) {
	t.Parallel()
	// Simulate a client that is already mounted by setting the flag directly.
	c := &Client{bucket: "test", mounted: true}
	err := c.Mount(context.Background(), "/tmp/objectfs-test")
	if err == nil {
		t.Fatal("expected ErrAlreadyMounted, got nil")
	}
	if !errors.Is(err, ErrAlreadyMounted) {
		t.Errorf("expected ErrAlreadyMounted, got %v", err)
	}
}

func TestUnmount_NotMounted(t *testing.T) {
	t.Parallel()
	c := &Client{bucket: "test", mounted: false}
	err := c.Unmount()
	if err == nil {
		t.Fatal("expected ErrNotMounted, got nil")
	}
	if !errors.Is(err, ErrNotMounted) {
		t.Errorf("expected ErrNotMounted, got %v", err)
	}
}

func TestClose_NilBackend(t *testing.T) {
	t.Parallel()
	// A client with no backend (e.g. partially constructed) should Close without panic.
	c := &Client{bucket: "test", mounted: false, backend: nil}
	if err := c.Close(); err != nil {
		t.Errorf("Close with nil backend: %v", err)
	}
}

// --- Sentinel error identity tests ---

func TestSentinelErrors_Is(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		sentinel error
	}{
		{"ErrNotFound", ErrNotFound},
		{"ErrAccessDenied", ErrAccessDenied},
		{"ErrNotMounted", ErrNotMounted},
		{"ErrAlreadyMounted", ErrAlreadyMounted},
		{"ErrInvalidConfig", ErrInvalidConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !errors.Is(tt.sentinel, tt.sentinel) {
				t.Errorf("%s: errors.Is(sentinel, sentinel) == false", tt.name)
			}
		})
	}
}

// --- Integration tests (require AWS credentials) ---

func TestNew_WithDefaults(t *testing.T) {
	requireAWS(t)
	c, err := New(context.Background(), "objectfs-test-bucket")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if c.opts.region != "us-east-1" {
		t.Errorf("region: got %q, want %q", c.opts.region, "us-east-1")
	}
	if c.IsMounted() {
		t.Error("IsMounted should be false after New")
	}
}

func TestNew_WithRegion(t *testing.T) {
	requireAWS(t)
	c, err := New(context.Background(), "objectfs-test-bucket",
		WithRegion("us-west-2"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if c.opts.region != "us-west-2" {
		t.Errorf("region: got %q, want %q", c.opts.region, "us-west-2")
	}
}

func TestClose_NotMounted(t *testing.T) {
	requireAWS(t)
	c, err := New(context.Background(), "objectfs-test-bucket")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
