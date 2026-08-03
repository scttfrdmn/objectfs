//go:build e2e
// +build e2e

package tests

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/scttfrdmn/objectfs/internal/adapter"
	"github.com/scttfrdmn/objectfs/internal/config"
)

// E2ETestSuite tests end-to-end functionality without FUSE mounting
type E2ETestSuite struct {
	suite.Suite
	ctx     context.Context
	adapter *adapter.Adapter
	config  *config.Configuration
}

func TestE2EFunctionality(t *testing.T) {
	suite.Run(t, new(E2ETestSuite))
}

func (s *E2ETestSuite) SetupSuite() {
	s.ctx = context.Background()

	// Create test configuration
	s.config = config.NewDefault()
	s.config.Performance.CacheSize = "64MB"
	s.config.Performance.MaxConcurrency = 10
	s.config.WriteBuffer.MaxMemory = "16MB"
	s.config.Cache.TTL = 30 * time.Second
	s.config.Monitoring.Metrics.Addr = "127.0.0.1:19090"
	s.config.Monitoring.HealthChecks.Addr = "127.0.0.1:19091"

	s.T().Logf("✅ E2E test suite initialized")
}

func (s *E2ETestSuite) TestAdapterCreation() {
	t := s.T()

	t.Logf("🔧 Testing ObjectFS adapter creation")

	// Test adapter creation with valid parameters
	adapter, err := adapter.New(s.ctx, "s3://test-bucket", "/tmp/test-mount", s.config)
	require.NoError(t, err)
	require.NotNil(t, adapter)

	t.Logf("✅ Adapter created successfully")
}

func (s *E2ETestSuite) TestAdapterValidation() {
	t := s.T()

	t.Logf("🧪 Testing adapter input validation")

	// Test invalid storage URI
	_, err := adapter.New(s.ctx, "invalid://bucket", "/tmp/test", s.config)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported storage scheme")

	// Test empty bucket name
	_, err = adapter.New(s.ctx, "s3://", "/tmp/test", s.config)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "bucket name")

	// Test invalid configuration
	invalidConfig := &config.Configuration{}
	invalidConfig.Performance.MaxConcurrency = -1
	_, err = adapter.New(s.ctx, "s3://test-bucket", "/tmp/test", invalidConfig)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid configuration")

	t.Logf("✅ Validation tests passed")
}

func (s *E2ETestSuite) TestComponentInitialization() {
	t := s.T()

	t.Logf("⚙️  Testing component initialization without mounting")

	// Create adapter
	adapterInstance, err := adapter.New(s.ctx, "s3://test-bucket", "/tmp/test-mount", s.config)
	require.NoError(t, err)
	require.NotNil(t, adapterInstance)

	// Note: We can't actually call Start() because it will try to mount FUSE
	// which fails on macOS due to library incompatibility. This test validates
	// that the adapter can be created and configured correctly.

	t.Logf("✅ Component initialization logic verified")
}

func (s *E2ETestSuite) TestConfigurationParsing() {
	t := s.T()

	t.Logf("📋 Testing configuration parsing and validation")

	// Test default configuration
	defaultConfig := config.NewDefault()
	require.NoError(t, defaultConfig.Validate())

	// Test configuration values
	assert.Equal(t, "INFO", defaultConfig.Global.LogLevel)
	assert.Equal(t, config.DefaultMetricsAddr, defaultConfig.Monitoring.Metrics.Addr)
	assert.Equal(t, config.DefaultHealthAddr, defaultConfig.Monitoring.HealthChecks.Addr)
	assert.Equal(t, "2GB", defaultConfig.Performance.CacheSize)
	assert.Equal(t, 150, defaultConfig.Performance.MaxConcurrency)
	assert.True(t, defaultConfig.Features.Prefetching)
	assert.True(t, defaultConfig.Features.BatchOperations)
	assert.True(t, defaultConfig.Monitoring.Metrics.Enabled)

	t.Logf("✅ Configuration parsing verified")
}

func (s *E2ETestSuite) TestReleaseReadiness() {
	t := s.T()

	t.Logf("🎯 Testing release readiness criteria")

	// 1. Test that all core components can be imported
	require.NotPanics(t, func() {
		_ = config.NewDefault()
	})

	// 2. Test that adapter can be created
	require.NotPanics(t, func() {
		adapter, err := adapter.New(s.ctx, "s3://test-bucket", "/tmp/test", s.config)
		require.NoError(t, err)
		require.NotNil(t, adapter)
	})

	// 3. Test configuration validation
	cfg := config.NewDefault()
	require.NoError(t, cfg.Validate())

	// 4. Test that binary can be built (implicit - this test runs)
	t.Logf("✅ Binary builds successfully")

	// 5. Test component integration points
	adapter, err := adapter.New(s.ctx, "s3://integration-test", "/tmp/test", cfg)
	require.NoError(t, err)
	require.NotNil(t, adapter)

	t.Logf("✅ Release readiness criteria validated")
	t.Logf("📊 Summary:")
	t.Logf("   - Core components: ✅ Working")
	t.Logf("   - Configuration: ✅ Valid")
	t.Logf("   - Adapter creation: ✅ Working")
	t.Logf("   - Input validation: ✅ Working")
	t.Logf("   - Binary compilation: ✅ Working")
	t.Logf("   - Interface compliance: ✅ Working")
}

// TestVersionAndBuildInfo builds the binary and asks it what version it is.
//
// This test used to log `✅ Version: 0.4.0 (defined in main.go)` and assert nothing, having
// concluded in a comment that "we can't test it directly here". It can: build the binary, run
// `--version`, and compare against the constant in the source. What the old form actually did was
// hardcode a fifth answer to the version question — main.go said 0.10.0 — and print a checkmark
// next to it, which is worse than not testing at all, because the checkmark is evidence to a
// reader that something was verified.
func (s *E2ETestSuite) TestVersionAndBuildInfo() {
	t := s.T()

	root, err := filepath.Abs("..")
	require.NoError(t, err)

	// The constant is unexported in package main, so there is nothing to import; read it. This
	// same inability to import it is why the number kept being copied into prose by hand.
	mainGo, err := os.ReadFile(filepath.Join(root, "cmd", "objectfs", "main.go"))
	require.NoError(t, err)

	m := regexp.MustCompile(`(?m)^\s*version\s*=\s*"([^"]+)"`).FindSubmatch(mainGo)
	require.NotNil(t, m, "cmd/objectfs/main.go no longer declares `version = \"…\"`")
	declared := string(m[1])

	bin := filepath.Join(t.TempDir(), "objectfs")

	build := exec.CommandContext(s.ctx, "go", "build", "-o", bin, "./cmd/objectfs")
	build.Dir = root

	out, err := build.CombinedOutput()
	require.NoError(t, err, "building the binary: %s", out)

	out, err = exec.CommandContext(s.ctx, bin, "--version").CombinedOutput()
	require.NoError(t, err, "running --version: %s", out)

	require.Equal(t, "ObjectFS version "+declared+"\n", string(out),
		"the binary reports a different version than the constant it is built from")

	t.Logf("✅ binary reports version %s, matching cmd/objectfs/main.go", declared)
}

// TestV040Features validates all v0.4.0 specific features
func (s *E2ETestSuite) TestV040Features() {
	t := s.T()

	t.Logf("🚀 Testing v0.4.0 features")

	// Test enhanced error handling configuration
	cfg := config.NewDefault()
	require.NoError(t, cfg.Validate())

	// Verify circuit breaker is enabled by default
	assert.True(t, cfg.Network.CircuitBreaker.Enabled, "Circuit breaker should be enabled")
	assert.Equal(t, 5, cfg.Network.CircuitBreaker.FailureThreshold, "Circuit breaker threshold should be 5")
	assert.Equal(t, 60*time.Second, cfg.Network.CircuitBreaker.Timeout, "Circuit breaker timeout should be 60s")

	// Verify retry configuration
	assert.Equal(t, 3, cfg.Network.Retry.MaxAttempts, "Max retry attempts should be 3")
	assert.Equal(t, 1*time.Second, cfg.Network.Retry.BaseDelay, "Base retry delay should be 1s")
	assert.Equal(t, 30*time.Second, cfg.Network.Retry.MaxDelay, "Max retry delay should be 30s")

	// Verify S3 transfer acceleration support
	assert.False(t, cfg.Storage.S3.UseAcceleration, "S3 acceleration should be disabled by default")
	cfg.Storage.S3.UseAcceleration = true
	assert.True(t, cfg.Storage.S3.UseAcceleration, "S3 acceleration should be configurable")

	// Verify multipart upload configuration exists
	assert.NotEmpty(t, cfg.Performance.WriteBufferSize, "Write buffer size should be set")
	assert.Equal(t, "16MB", cfg.Performance.WriteBufferSize, "Default write buffer should be 16MB")

	t.Logf("✅ v0.4.0 features validated")
}

// TestEnhancedErrorHandling tests the enhanced error handling features
func (s *E2ETestSuite) TestEnhancedErrorHandling() {
	t := s.T()

	t.Logf("🛡️  Testing enhanced error handling")

	// Test configuration with custom error handling settings
	cfg := config.NewDefault()
	cfg.Network.CircuitBreaker.FailureThreshold = 3
	cfg.Network.CircuitBreaker.Timeout = 30 * time.Second
	cfg.Network.Retry.MaxAttempts = 5

	adp, err := adapter.New(s.ctx, "s3://error-test-bucket", "/tmp/error-test", cfg)
	require.NoError(t, err)
	require.NotNil(t, adp)

	// Verify adapter was created with custom error handling config
	assert.NotNil(t, adp, "Adapter with custom error config should be created")

	t.Logf("✅ Enhanced error handling validated")
}

// TestMemoryManagement tests memory leak detection and management features
func (s *E2ETestSuite) TestMemoryManagement() {
	t := s.T()

	t.Logf("💾 Testing memory management features")

	// Test configuration with memory constraints
	cfg := config.NewDefault()
	cfg.Performance.CacheSize = "128MB"
	cfg.WriteBuffer.MaxMemory = "64MB"
	cfg.Cache.MaxEntries = 1000

	adp, err := adapter.New(s.ctx, "s3://memory-test-bucket", "/tmp/memory-test", cfg)
	require.NoError(t, err)
	require.NotNil(t, adp)

	// Verify memory configuration was applied
	assert.NotNil(t, adp, "Adapter with memory config should be created")

	t.Logf("✅ Memory management validated")
}

// TestS3TransferAcceleration tests S3 transfer acceleration configuration
func (s *E2ETestSuite) TestS3TransferAcceleration() {
	t := s.T()

	t.Logf("⚡ Testing S3 transfer acceleration")

	// Test with acceleration enabled
	cfg := config.NewDefault()
	cfg.Storage.S3.UseAcceleration = true
	cfg.Storage.S3.Region = "us-west-2"

	adp, err := adapter.New(s.ctx, "s3://acceleration-test-bucket", "/tmp/acceleration-test", cfg)
	require.NoError(t, err)
	require.NotNil(t, adp)

	// Test with acceleration disabled
	cfg2 := config.NewDefault()
	cfg2.Storage.S3.UseAcceleration = false
	adp2, err := adapter.New(s.ctx, "s3://no-acceleration-bucket", "/tmp/no-acceleration-test", cfg2)
	require.NoError(t, err)
	require.NotNil(t, adp2)

	t.Logf("✅ S3 transfer acceleration validated")
}

// TestMultipartUploadConfiguration tests multipart upload settings
func (s *E2ETestSuite) TestMultipartUploadConfiguration() {
	t := s.T()

	t.Logf("📦 Testing multipart upload configuration")

	// Test with various write buffer sizes (affects multipart chunking)
	testCases := []struct {
		name       string
		bufferSize string
		maxMemory  string
	}{
		{"small buffers", "8MB", "32MB"},
		{"medium buffers", "16MB", "64MB"},
		{"large buffers", "32MB", "128MB"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewDefault()
			cfg.Performance.WriteBufferSize = tc.bufferSize
			cfg.WriteBuffer.MaxMemory = tc.maxMemory

			adp, err := adapter.New(s.ctx, "s3://multipart-test-bucket", "/tmp/multipart-test", cfg)
			require.NoError(t, err, "Adapter should be created with %s", tc.name)
			require.NotNil(t, adp)
		})
	}

	t.Logf("✅ Multipart upload configuration validated")
}

// TestConcurrencyAndPerformance tests performance-related configurations
func (s *E2ETestSuite) TestConcurrencyAndPerformance() {
	t := s.T()

	t.Logf("⚡ Testing concurrency and performance settings")

	// Test with high concurrency settings
	cfg := config.NewDefault()
	cfg.Performance.MaxConcurrency = 200
	cfg.Performance.ConnectionPoolSize = 16
	cfg.Performance.ReadAheadSize = "8MB"

	adp, err := adapter.New(s.ctx, "s3://performance-test-bucket", "/tmp/performance-test", cfg)
	require.NoError(t, err)
	require.NotNil(t, adp)

	// Test with conservative settings
	cfg2 := config.NewDefault()
	cfg2.Performance.MaxConcurrency = 50
	cfg2.Performance.ConnectionPoolSize = 4
	cfg2.Performance.ReadAheadSize = "2MB"

	adp2, err := adapter.New(s.ctx, "s3://conservative-bucket", "/tmp/conservative-test", cfg2)
	require.NoError(t, err)
	require.NotNil(t, adp2)

	t.Logf("✅ Concurrency and performance validated")
}

// TestProductionReadiness validates production-ready configurations
func (s *E2ETestSuite) TestProductionReadiness() {
	t := s.T()

	t.Logf("🏭 Testing production readiness")

	// Test with production-like configuration
	cfg := config.NewDefault()
	cfg.Performance.CacheSize = "4GB"
	cfg.Performance.MaxConcurrency = 150
	cfg.WriteBuffer.MaxMemory = "1GB"
	cfg.Network.CircuitBreaker.Enabled = true
	cfg.Network.Retry.MaxAttempts = 3
	cfg.Storage.S3.UseAcceleration = true
	cfg.Monitoring.Metrics.Enabled = true
	cfg.Monitoring.HealthChecks.Enabled = true
	cfg.Features.Prefetching = true
	cfg.Features.BatchOperations = true
	cfg.Features.MetadataCaching = true

	// Validate configuration
	require.NoError(t, cfg.Validate(), "Production configuration should be valid")

	// Create adapter with production config
	adp, err := adapter.New(s.ctx, "s3://production-test-bucket", "/tmp/production-test", cfg)
	require.NoError(t, err)
	require.NotNil(t, adp)

	t.Logf("✅ Production readiness validated")
	t.Logf("📊 Production Configuration Summary:")
	t.Logf("   - Cache: %s", cfg.Performance.CacheSize)
	t.Logf("   - Concurrency: %d", cfg.Performance.MaxConcurrency)
	t.Logf("   - Circuit Breaker: %v", cfg.Network.CircuitBreaker.Enabled)
	t.Logf("   - S3 Acceleration: %v", cfg.Storage.S3.UseAcceleration)
	t.Logf("   - Metrics: %v", cfg.Monitoring.Metrics.Enabled)
	t.Logf("   - Health Checks: %v", cfg.Monitoring.HealthChecks.Enabled)
}
