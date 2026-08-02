package objectfs

import (
	"context"
	"errors"
	"os"
	"testing"
)

// requireAWS skips the test unless AWS credentials are available.
// Accepts either AWS_ACCESS_KEY_ID (explicit keys) or AWS_PROFILE (named profile).
func requireAWS(t *testing.T) {
	t.Helper()
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" && os.Getenv("AWS_PROFILE") == "" {
		t.Skip("AWS_ACCESS_KEY_ID not set; skipping integration test")
	}
}

// testRegion returns the AWS region from $AWS_REGION, defaulting to us-east-1.
func testRegion() string {
	if r := os.Getenv("AWS_REGION"); r != "" {
		return r
	}
	return "us-east-1"
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

// --- Integration tests (require AWS credentials and a real bucket) ---
//
// Each of these calls New, which health-checks the bucket with a HeadBucket, so each needs a bucket
// that exists. They are therefore gated on requireTestBucket, not on requireAWS. An earlier version
// fell back to the conventional name "objectfs-test-bucket" when $OBJECTFS_TEST_BUCKET was unset, on
// the reasoning that a connectivity-only test performs no destructive operation — but a bucket name
// nobody owns fails HeadBucket with a 404, so all three tests failed for anyone with credentials in
// their environment and passed in CI only because CI has none. A test that skips where it is run and
// fails where it is developed is worse than one that skips in both.

func TestNew_WithDefaults(t *testing.T) {
	bucket := requireTestBucket(t)
	// Default region must be us-east-1; verify via defaultOptions (no S3 call needed).
	if got := defaultOptions().region; got != "us-east-1" {
		t.Errorf("default region: got %q, want %q", got, "us-east-1")
	}
	// Verify the client initializes successfully against the real test bucket.
	c, err := New(context.Background(), bucket, WithRegion(testRegion()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if c.IsMounted() {
		t.Error("IsMounted should be false after New")
	}
}

func TestNew_WithRegion(t *testing.T) {
	bucket := requireTestBucket(t)
	region := testRegion()
	c, err := New(context.Background(), bucket, WithRegion(region))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if c.opts.region != region {
		t.Errorf("region: got %q, want %q", c.opts.region, region)
	}
}

func TestClose_NotMounted(t *testing.T) {
	bucket := requireTestBucket(t)
	c, err := New(context.Background(), bucket, WithRegion(testRegion()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// --- Full CRUD integration tests (require OBJECTFS_TEST_BUCKET + AWS creds) ---

// requireTestBucket skips unless both AWS creds and OBJECTFS_TEST_BUCKET are set.
func requireTestBucket(t *testing.T) string {
	t.Helper()
	requireAWS(t)
	bucket := os.Getenv("OBJECTFS_TEST_BUCKET")
	if bucket == "" {
		t.Skip("OBJECTFS_TEST_BUCKET not set; skipping CRUD integration test")
	}
	return bucket
}

func TestIntegration_PutGetDeleteHead(t *testing.T) {
	bucket := requireTestBucket(t)
	ctx := context.Background()

	c, err := New(ctx, bucket, WithRegion(testRegion()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close() //nolint:errcheck

	key := "sdk-integration-test/put-get-delete-head"
	data := []byte("ObjectFS Go SDK integration test data — Put/Get/Delete/Head round-trip")

	// Put
	if err := c.Put(ctx, key, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Get — full object
	got, err := c.Get(ctx, key, 0, 0)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("Get data mismatch: got %q, want %q", got, data)
	}

	// Get — partial read
	partial, err := c.Get(ctx, key, 10, 7)
	if err != nil {
		t.Fatalf("Get (partial): %v", err)
	}
	if string(partial) != string(data[10:17]) {
		t.Errorf("partial Get: got %q, want %q", partial, data[10:17])
	}

	// Head
	info, err := c.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if info.Key != key {
		t.Errorf("Head key: got %q, want %q", info.Key, key)
	}
	if info.Size != int64(len(data)) {
		t.Errorf("Head size: got %d, want %d", info.Size, len(data))
	}

	// Delete
	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Confirm deletion — Get should error
	_, err = c.Get(ctx, key, 0, 0)
	if err == nil {
		t.Error("Get after Delete should return an error, got nil")
	}
}

func TestIntegration_List(t *testing.T) {
	bucket := requireTestBucket(t)
	ctx := context.Background()

	c, err := New(ctx, bucket, WithRegion(testRegion()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close() //nolint:errcheck

	prefix := "sdk-integration-test/list/"
	keys := []string{prefix + "a", prefix + "b", prefix + "c"}

	// Upload test objects.
	for _, k := range keys {
		if err := c.Put(ctx, k, []byte("list-test-"+k)); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	defer func() {
		for _, k := range keys {
			c.Delete(ctx, k) //nolint:errcheck
		}
	}()

	// List with prefix.
	results, err := c.List(ctx, prefix, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(results) < len(keys) {
		t.Errorf("List: got %d results, want at least %d", len(results), len(keys))
	}

	found := make(map[string]bool)
	for _, obj := range results {
		found[obj.Key] = true
	}
	for _, k := range keys {
		if !found[k] {
			t.Errorf("List: key %q not found in results", k)
		}
	}

	// Limit parameter.
	limited, err := c.List(ctx, prefix, 1)
	if err != nil {
		t.Fatalf("List (limit=1): %v", err)
	}
	if len(limited) > 1 {
		t.Errorf("List limit=1: got %d results, want ≤1", len(limited))
	}
}

func TestIntegration_Health(t *testing.T) {
	bucket := requireTestBucket(t)
	ctx := context.Background()

	c, err := New(ctx, bucket, WithRegion(testRegion()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close() //nolint:errcheck

	if err := c.Health(ctx); err != nil {
		t.Errorf("Health: %v", err)
	}
}
