package objectfs

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/config"
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
	// Loopback, and the same value internal/config defaults the file-based setting to, read from
	// there rather than restated: two constants for one default is how the ports these replaced came
	// to disagree with the config loader in the first place.
	if o.metricsAddr != config.DefaultMetricsAddr {
		t.Errorf("metricsAddr: got %q, want %q", o.metricsAddr, config.DefaultMetricsAddr)
	}
	if o.healthAddr != config.DefaultHealthAddr {
		t.Errorf("healthAddr: got %q, want %q", o.healthAddr, config.DefaultHealthAddr)
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

func TestWithMetricsAddr(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	WithMetricsAddr("0.0.0.0:9090")(&o)
	if o.metricsAddr != "0.0.0.0:9090" {
		t.Errorf("metricsAddr: got %q, want %q", o.metricsAddr, "0.0.0.0:9090")
	}
}

func TestWithHealthAddr(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	WithHealthAddr("0.0.0.0:9091")(&o)
	if o.healthAddr != "0.0.0.0:9091" {
		t.Errorf("healthAddr: got %q, want %q", o.healthAddr, "0.0.0.0:9091")
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

func TestValidateOptions_SameAddrs(t *testing.T) {
	t.Parallel()
	o := defaultOptions()
	o.metricsAddr = "127.0.0.1:9000"
	o.healthAddr = "127.0.0.1:9000"
	err := validateOptions(o)
	if err == nil {
		t.Fatal("expected error for the same metrics and health address")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("expected ErrInvalidConfig, got %v", err)
	}
}

// TestBuildConfigCarriesEveryOptionToWhereItIsRead asserts the values the consumers receive.
//
// Not that buildConfig was called, and not by recomputing its mapping — a test that restates
// `cfg.X = o.x` cannot fail. It asserts each field of the *output*, against a value that differs from
// the default the field would hold if the mapping were absent. That distinction is what #202 turns
// on: `monitoring.metrics_addr` was declared, defaulted, documented and mapped nowhere, and a test
// asserting it equalled its own default would have passed throughout.
//
// The two addresses in particular go through this function to reach a listener. WithMetricsAddr sets
// an option; only this mapping makes the option a bind address, and only Configuration.Validate makes
// a malformed one an error rather than a mount with a missing endpoint.
func TestBuildConfigCarriesEveryOptionToWhereItIsRead(t *testing.T) {
	t.Parallel()

	c := &Client{opts: clientOptions{
		region:         "eu-west-1",
		endpoint:       "https://minio.example.internal:9000",
		cacheSize:      "7GB",
		maxConcurrency: 77,
		logLevel:       "WARN",
		metricsAddr:    "0.0.0.0:19090",
		healthAddr:     "0.0.0.0:19091",
		tlsEnabled:     true,
	}}

	cfg := c.buildConfig()

	cases := []struct {
		field string
		got   any
		want  any
	}{
		{"storage.s3.region", cfg.Storage.S3.Region, "eu-west-1"},
		{"storage.s3.endpoint", cfg.Storage.S3.Endpoint, "https://minio.example.internal:9000"},
		{"performance.cache_size", cfg.Performance.CacheSize, "7GB"},
		{"performance.max_concurrency", cfg.Performance.MaxConcurrency, 77},
		{"global.log_level", cfg.Global.LogLevel, "WARN"},
		{"monitoring.metrics.addr", cfg.Monitoring.Metrics.Addr, "0.0.0.0:19090"},
		{"monitoring.health_checks.addr", cfg.Monitoring.HealthChecks.Addr, "0.0.0.0:19091"},
		{"security.tls_enabled", cfg.Security.TLSEnabled, true},
	}

	def := config.NewDefault()
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v — the option never reached the configuration the mount reads",
				tc.field, tc.got, tc.want)
		}
	}

	// And the fixtures must not equal the defaults, or the assertions above prove nothing.
	if cfg.Monitoring.Metrics.Addr == def.Monitoring.Metrics.Addr ||
		cfg.Monitoring.HealthChecks.Addr == def.Monitoring.HealthChecks.Addr {
		t.Error("a fixture in this test equals the value the field would hold with no mapping at all, " +
			"so the assertion for it cannot fail; pick a different one")
	}

	// The result has to survive the validation Mount performs, or the SDK builds a Configuration the
	// adapter rejects — which is how the SDK and the config loader would come to disagree about what a
	// valid address is.
	if err := cfg.Validate(); err != nil {
		t.Errorf("the configuration built from valid options does not validate: %v", err)
	}
}

// TestBuildConfigRejectsAMalformedAddressThroughValidate pins where SDK address checking happens.
//
// validateOptions deliberately checks only that the two addresses differ. Their shape is
// Configuration.Validate's job, so there is one implementation of "what is a valid listen address"
// and the SDK cannot come to a different conclusion than a config file would. This asserts the
// error does arrive, and names the field.
func TestBuildConfigRejectsAMalformedAddressThroughValidate(t *testing.T) {
	t.Parallel()

	o := defaultOptions()
	o.metricsAddr = "127.0.0.1:99999" // accepted by net.SplitHostPort; out of range for a port

	c := &Client{opts: o}

	err := c.buildConfig().Validate()
	if err == nil {
		t.Fatal("a port outside 1-65535 validated; the bind would fail on a goroutine and the mount " +
			"would come up with no metrics endpoint")
	}
	if !strings.Contains(err.Error(), "monitoring.metrics.addr") {
		t.Errorf("the error does not name the setting at fault: %v", err)
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
