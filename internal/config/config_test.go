package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Test Constants
const (
	TestDebugLevel = "DEBUG"
	TestCacheSize  = "8GB"
)

func TestNewDefault(t *testing.T) {
	cfg := NewDefault()

	// Test global defaults
	if cfg.Global.LogLevel != "INFO" {
		t.Errorf("Expected LogLevel to be INFO, got %s", cfg.Global.LogLevel)
	}
	// Loopback, not the wildcard, and inside the block whose enabled flag governs the listener. Both
	// endpoints are unauthenticated: /metrics and /debug/operations report per-operation counts, error
	// rates and timings, /health reports component names and error strings. The ports are the ones
	// global.metrics_port and global.health_port defaulted to, so a scrape config keeps working; the
	// host is what changed (#211).
	if cfg.Monitoring.Metrics.Addr != DefaultMetricsAddr {
		t.Errorf("monitoring.metrics.addr = %q, want %q", cfg.Monitoring.Metrics.Addr, DefaultMetricsAddr)
	}
	if cfg.Monitoring.HealthChecks.Addr != DefaultHealthAddr {
		t.Errorf("monitoring.health_checks.addr = %q, want %q",
			cfg.Monitoring.HealthChecks.Addr, DefaultHealthAddr)
	}

	// Test performance defaults
	if cfg.Performance.CacheSize != "2GB" {
		t.Errorf("Expected CacheSize to be 2GB, got %s", cfg.Performance.CacheSize)
	}
	if cfg.Performance.MaxConcurrency != 150 {
		t.Errorf("Expected MaxConcurrency to be 150, got %d", cfg.Performance.MaxConcurrency)
	}
	// Object compression lives under the backend that stores the objects (#157), and is off by
	// default: it is a storage-format decision, not a performance knob — a compressed object is not
	// readable by `aws s3 cp`. The algorithm and level are still asserted, because they are what a
	// mount uses the moment an operator flips enabled.
	if cfg.Storage.S3.Compression.Enabled {
		t.Error("Expected storage.s3.compression.enabled to default to false")
	}
	if cfg.Storage.S3.Compression.Algorithm != "zstd" {
		t.Errorf("Expected compression algorithm zstd, got %s", cfg.Storage.S3.Compression.Algorithm)
	}
	if cfg.Storage.S3.Compression.Level != 3 {
		t.Errorf("Expected compression level 3, got %d", cfg.Storage.S3.Compression.Level)
	}
	if cfg.Storage.S3.Compression.MinSize != "4KB" {
		t.Errorf("Expected compression min_size 4KB, got %s", cfg.Storage.S3.Compression.MinSize)
	}

	// Test cache defaults
	if cfg.Cache.TTL != 5*time.Minute {
		t.Errorf("Expected Cache TTL to be 5 minutes, got %v", cfg.Cache.TTL)
	}
	if cfg.Cache.EvictionPolicy != "weighted_lru" {
		t.Errorf("Expected EvictionPolicy to be weighted_lru, got %s", cfg.Cache.EvictionPolicy)
	}

	// Test feature flags
	if !cfg.Features.Prefetching {
		t.Error("Expected Prefetching to be enabled by default")
	}
	if !cfg.Features.BatchOperations {
		t.Error("Expected BatchOperations to be enabled by default")
	}
	if cfg.Features.OfflineMode {
		t.Error("Expected OfflineMode to be disabled by default")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  func() *Configuration
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid config",
			config: func() *Configuration {
				return NewDefault()
			},
			wantErr: false,
		},
		{
			name: "invalid max concurrency",
			config: func() *Configuration {
				cfg := NewDefault()
				cfg.Performance.MaxConcurrency = 0
				return cfg
			},
			wantErr: true,
			errMsg:  "max_concurrency must be greater than 0",
		},
		{
			name: "invalid connection pool size",
			config: func() *Configuration {
				cfg := NewDefault()
				cfg.Performance.ConnectionPoolSize = 0
				return cfg
			},
			wantErr: true,
			errMsg:  "connection_pool_size must be greater than 0",
		},
		{
			name: "same metrics and health addresses",
			config: func() *Configuration {
				cfg := NewDefault()
				cfg.Monitoring.Metrics.Enabled = true
				cfg.Monitoring.HealthChecks.Enabled = true
				cfg.Monitoring.Metrics.Addr = "127.0.0.1:8080"
				cfg.Monitoring.HealthChecks.Addr = "127.0.0.1:8080"
				return cfg
			},
			wantErr: true,
			errMsg:  "two listeners cannot share one address",
		},
		{
			name: "invalid log level",
			config: func() *Configuration {
				cfg := NewDefault()
				cfg.Global.LogLevel = "INVALID"
				return cfg
			},
			wantErr: true,
			errMsg:  "invalid log_level",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.config()
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" {
				if err.Error() != tt.errMsg && !contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error = %v, want error containing %v", err, tt.errMsg)
				}
			}
		})
	}
}

func TestLoadFromFile(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	configContent := `
global:
  log_level: DEBUG

monitoring:
  metrics:
    addr: 127.0.0.1:9090
  health_checks:
    addr: 127.0.0.1:9091

performance:
  cache_size: 4GB
  max_concurrency: 200

storage:
  s3:
    compression:
      enabled: true
      algorithm: lz4

features:
  prefetching: false
  offline_mode: true
`

	err := os.WriteFile(configFile, []byte(configContent), 0600)
	if err != nil {
		t.Fatalf("Failed to write test config file: %v", err)
	}

	cfg := NewDefault()
	err = cfg.LoadFromFile(configFile)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}

	// Verify loaded values
	if cfg.Global.LogLevel != TestDebugLevel {
		t.Errorf("Expected LogLevel to be DEBUG, got %s", cfg.Global.LogLevel)
	}
	if cfg.Monitoring.Metrics.Addr != "127.0.0.1:9090" {
		t.Errorf("monitoring.metrics.addr = %q, want 127.0.0.1:9090", cfg.Monitoring.Metrics.Addr)
	}
	if cfg.Monitoring.HealthChecks.Addr != "127.0.0.1:9091" {
		t.Errorf("monitoring.health_checks.addr = %q, want 127.0.0.1:9091",
			cfg.Monitoring.HealthChecks.Addr)
	}
	if cfg.Performance.CacheSize != "4GB" {
		t.Errorf("Expected CacheSize to be 4GB, got %s", cfg.Performance.CacheSize)
	}
	if cfg.Performance.MaxConcurrency != 200 {
		t.Errorf("Expected MaxConcurrency to be 200, got %d", cfg.Performance.MaxConcurrency)
	}
	if !cfg.Storage.S3.Compression.Enabled {
		t.Error("Expected storage.s3.compression.enabled to be true")
	}
	if cfg.Storage.S3.Compression.Algorithm != "lz4" {
		t.Errorf("Expected compression algorithm lz4, got %s", cfg.Storage.S3.Compression.Algorithm)
	}
	// Not set in the file, so the default survives the overlay. This is the half of the loader a
	// partial section can break: yaml.Unmarshal into an already-defaulted struct leaves absent keys
	// alone, and a file that names only `algorithm` must not zero the level to an invalid 0.
	if cfg.Storage.S3.Compression.Level != 3 {
		t.Errorf("Expected compression level to keep its default 3, got %d",
			cfg.Storage.S3.Compression.Level)
	}
	if cfg.Features.Prefetching {
		t.Error("Expected Prefetching to be false")
	}
	if !cfg.Features.OfflineMode {
		t.Error("Expected OfflineMode to be true")
	}
}

func TestLoadFromFileNonExistent(t *testing.T) {
	cfg := NewDefault()
	err := cfg.LoadFromFile("/nonexistent/config.yaml")
	if err == nil {
		t.Error("Expected error when loading non-existent config file")
	}
}

func TestLoadFromEnv(t *testing.T) {
	// Set up environment variables
	testEnvVars := map[string]string{
		"OBJECTFS_LOG_LEVEL":           "ERROR",
		"OBJECTFS_METRICS_ADDR":        "127.0.0.1:9090",
		"OBJECTFS_HEALTH_ADDR":         "127.0.0.1:9091",
		"OBJECTFS_CACHE_SIZE":          TestCacheSize,
		"OBJECTFS_MAX_CONCURRENCY":     "300",
		"OBJECTFS_COMPRESSION_ENABLED": "true",
		"OBJECTFS_PREFETCHING":         "false",
		"OBJECTFS_BATCH_OPERATIONS":    "false",
		"OBJECTFS_OFFLINE_MODE":        "true",
		"OBJECTFS_CACHE_TTL":           "10m",
	}

	// Set environment variables
	for key, value := range testEnvVars {
		t.Setenv(key, value)
	}

	cfg := NewDefault()
	err := cfg.LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}

	// Verify loaded values
	if cfg.Global.LogLevel != "ERROR" {
		t.Errorf("Expected LogLevel to be ERROR, got %s", cfg.Global.LogLevel)
	}
	if cfg.Monitoring.Metrics.Addr != "127.0.0.1:9090" {
		t.Errorf("OBJECTFS_METRICS_ADDR did not reach monitoring.metrics.addr: got %q",
			cfg.Monitoring.Metrics.Addr)
	}
	if cfg.Monitoring.HealthChecks.Addr != "127.0.0.1:9091" {
		t.Errorf("OBJECTFS_HEALTH_ADDR did not reach monitoring.health_checks.addr: got %q",
			cfg.Monitoring.HealthChecks.Addr)
	}
	if cfg.Performance.CacheSize != TestCacheSize {
		t.Errorf("Expected CacheSize to be 8GB, got %s", cfg.Performance.CacheSize)
	}
	if cfg.Performance.MaxConcurrency != 300 {
		t.Errorf("Expected MaxConcurrency to be 300, got %d", cfg.Performance.MaxConcurrency)
	}
	// OBJECTFS_COMPRESSION_ENABLED assigns storage.s3.compression.enabled as of #157. It previously
	// assigned performance.compression_enabled, which was read by nothing — so exporting this
	// variable had no effect on whether objects were compressed. Asserted as true against a default
	// of false: "false" would pass whether or not the handler ran at all.
	if !cfg.Storage.S3.Compression.Enabled {
		t.Error("OBJECTFS_COMPRESSION_ENABLED=true did not reach storage.s3.compression.enabled")
	}
	if cfg.Features.Prefetching {
		t.Error("Expected Prefetching to be false")
	}
	if cfg.Features.BatchOperations {
		t.Error("Expected BatchOperations to be false")
	}
	if !cfg.Features.OfflineMode {
		t.Error("Expected OfflineMode to be true")
	}
	if cfg.Cache.TTL != 10*time.Minute {
		t.Errorf("Expected Cache TTL to be 10 minutes, got %v", cfg.Cache.TTL)
	}
}

// TestValidateRejectsEachWayAListenAddressCanBeWrong covers validateListenAddr's arms individually.
//
// This validation is [#192]'s half of the listener change, and each arm has a distinct failure it
// prevents. The reason it is here rather than left to the bind is that the bind used to happen on a
// goroutine that logged and returned, so every one of these produced a mount that came up with no
// endpoint and one line in the log — the operator finds out when a probe starts failing.
//
// The range arm is the load-bearing one. net.SplitHostPort accepts "99999" — it is a syntactically
// fine port string — so nothing before this caught a config an operator could plausibly write.
func TestValidateRejectsEachWayAListenAddressCanBeWrong(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		addr    string
		mustSay string
	}{
		{
			name:    "empty",
			addr:    "",
			mustSay: "must be set to a host:port",
		},
		{
			name:    "a bare host with no port",
			addr:    "127.0.0.1",
			mustSay: "is not a host:port address",
		},
		{
			name:    "a service name rather than a number",
			addr:    "127.0.0.1:http",
			mustSay: "non-numeric port",
		},
		{
			// Accepted by net.SplitHostPort, refused by the kernel at bind time. This is the exact value
			// #192 reported: health_port: 99999 reached net.Listen as "[::]:99999" and failed in the
			// address parse.
			name:    "a port above 65535",
			addr:    "127.0.0.1:99999",
			mustSay: "outside 1-65535",
		},
		{
			// Port 0 is a real thing to write — it asks the kernel for any free port — which is why it is
			// refused explicitly rather than accepted: a listener on a port nobody can predict is
			// indistinguishable from no listener to whatever was meant to scrape it, and "off" is the
			// enabled flag.
			name:    "port zero",
			addr:    "127.0.0.1:0",
			mustSay: "Port 0 is not how these endpoints are disabled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Both fields, because they are validated by two separate call sites and an omission in
			// either is silent: the block would accept any address and the endpoint would land wherever.
			for field, set := range map[string]func(*Configuration){
				"monitoring.metrics.addr": func(c *Configuration) {
					c.Monitoring.Metrics.Enabled = true
					c.Monitoring.Metrics.Addr = tc.addr
				},
				"monitoring.health_checks.addr": func(c *Configuration) {
					c.Monitoring.HealthChecks.Enabled = true
					c.Monitoring.HealthChecks.Addr = tc.addr
				},
			} {
				cfg := NewDefault()
				set(cfg)

				err := cfg.Validate()
				if err == nil {
					t.Errorf("%s = %q validated. The bind fails and the mount comes up with no endpoint",
						field, tc.addr)

					continue
				}
				if !contains(err.Error(), tc.mustSay) {
					t.Errorf("%s = %q: the error does not explain what is wrong (want %q): %v",
						field, tc.addr, tc.mustSay, err)
				}
				if !contains(err.Error(), field) {
					t.Errorf("%s = %q: the error does not name the field at fault: %v",
						field, tc.addr, err)
				}
			}
		})
	}
}

// TestValidateIgnoresTheAddressOfADisabledListener pins the other half of that contract.
//
// A block that binds nothing must not fail a mount over a setting with no effect. This is the same
// reasoning as validateCompressionConfig's disabled arm, and it is what makes `enabled: false` a
// usable answer to a bad address rather than a second thing to fix.
func TestValidateIgnoresTheAddressOfADisabledListener(t *testing.T) {
	t.Parallel()

	cfg := NewDefault()
	cfg.Monitoring.Metrics.Enabled = false
	cfg.Monitoring.Metrics.Addr = "this is not an address"
	cfg.Monitoring.HealthChecks.Enabled = false
	cfg.Monitoring.HealthChecks.Addr = ""

	if err := cfg.Validate(); err != nil {
		t.Errorf("a malformed address in a block whose enabled flag is false failed validation, so "+
			"turning an endpoint off is not on its own a way to stop caring about its address: %v", err)
	}
}

// TestLoadFromEnvGovernsTheListeners covers the four monitoring variables together.
//
// OBJECTFS_METRICS_ENABLED was documented in cmd/objectfs/doc.go and OBJECTFS.md and assigned nothing,
// which is #202's shape in the setting that *closes* an unauthenticated endpoint rather than the one
// that moves it. An operator who exported it meaning "off" got a listener.
//
// Both _ENABLED cases assert false against a default of true. The direction matters: "false" is the
// only value whose effect a defaulted-true field can distinguish from the handler never running at
// all, so asserting true here would pass with the mapping deleted.
func TestLoadFromEnvGovernsTheListeners(t *testing.T) {
	// t.Setenv forbids t.Parallel.

	t.Run("each variable reaches the field the listener reads", func(t *testing.T) {
		t.Setenv("OBJECTFS_METRICS_ENABLED", "false")
		t.Setenv("OBJECTFS_METRICS_ADDR", "10.0.0.1:19090")
		t.Setenv("OBJECTFS_HEALTH_ENABLED", "false")
		t.Setenv("OBJECTFS_HEALTH_ADDR", "10.0.0.1:19091")

		cfg := NewDefault()
		if !cfg.Monitoring.Metrics.Enabled || !cfg.Monitoring.HealthChecks.Enabled {
			t.Fatal("both listeners must default to enabled for the assertions below to mean anything")
		}

		if err := cfg.LoadFromEnv(); err != nil {
			t.Fatalf("LoadFromEnv() error = %v", err)
		}

		if cfg.Monitoring.Metrics.Enabled {
			t.Error("OBJECTFS_METRICS_ENABLED=false did not close the metrics listener, so an operator " +
				"who exported it to stop publishing an unauthenticated endpoint still has one")
		}
		if cfg.Monitoring.HealthChecks.Enabled {
			t.Error("OBJECTFS_HEALTH_ENABLED=false did not close the health listener")
		}
		if cfg.Monitoring.Metrics.Addr != "10.0.0.1:19090" {
			t.Errorf("monitoring.metrics.addr = %q, want the exported address", cfg.Monitoring.Metrics.Addr)
		}
		if cfg.Monitoring.HealthChecks.Addr != "10.0.0.1:19091" {
			t.Errorf("monitoring.health_checks.addr = %q, want the exported address",
				cfg.Monitoring.HealthChecks.Addr)
		}
	})

	// A value that is not a boolean must fail startup and name the variable, not be coerced. These two
	// govern endpoints that are on by default, so silent coercion fails in whichever direction the
	// coercion happens to pick: to false it removes an endpoint a probe depends on, to true it keeps one
	// the operator asked to close. The feature-flag variables coerce, and that asymmetry is the point.
	for _, v := range []string{"OBJECTFS_METRICS_ENABLED", "OBJECTFS_HEALTH_ENABLED"} {
		t.Run(v+" refuses a non-boolean", func(t *testing.T) {
			t.Setenv(v, "yes-please")

			err := NewDefault().LoadFromEnv()
			if err == nil {
				t.Fatalf("%s=yes-please was accepted; whichever way it coerced, an unauthenticated "+
					"listener is in a state nobody asked for and nothing reported", v)
			}
			if !contains(err.Error(), v) {
				t.Errorf("the error does not name the variable at fault: %v", err)
			}
		})
	}
}

func TestLoadFromEnv_S3Region(t *testing.T) {
	// t.Setenv requires sequential execution; no t.Parallel here.

	t.Run("OBJECTFS_S3_REGION overrides default", func(t *testing.T) {
		t.Setenv("OBJECTFS_S3_REGION", "ap-southeast-1")
		cfg := NewDefault()
		if err := cfg.LoadFromEnv(); err != nil {
			t.Fatalf("LoadFromEnv() error = %v", err)
		}
		if cfg.Storage.S3.Region != "ap-southeast-1" {
			t.Errorf("Region: got %q, want %q", cfg.Storage.S3.Region, "ap-southeast-1")
		}
	})

	t.Run("AWS_REGION sets region", func(t *testing.T) {
		t.Setenv("AWS_REGION", "eu-central-1")
		cfg := NewDefault()
		if err := cfg.LoadFromEnv(); err != nil {
			t.Fatalf("LoadFromEnv() error = %v", err)
		}
		if cfg.Storage.S3.Region != "eu-central-1" {
			t.Errorf("Region: got %q, want %q", cfg.Storage.S3.Region, "eu-central-1")
		}
	})

	t.Run("OBJECTFS_S3_REGION beats AWS_REGION", func(t *testing.T) {
		t.Setenv("AWS_REGION", "us-east-1")
		t.Setenv("OBJECTFS_S3_REGION", "sa-east-1")
		cfg := NewDefault()
		if err := cfg.LoadFromEnv(); err != nil {
			t.Fatalf("LoadFromEnv() error = %v", err)
		}
		if cfg.Storage.S3.Region != "sa-east-1" {
			t.Errorf("Region: got %q, want %q", cfg.Storage.S3.Region, "sa-east-1")
		}
	})

	t.Run("AWS_REGION beats AWS_DEFAULT_REGION", func(t *testing.T) {
		t.Setenv("AWS_DEFAULT_REGION", "us-west-1")
		t.Setenv("AWS_REGION", "us-west-2")
		cfg := NewDefault()
		// Clear default so AWS_DEFAULT_REGION would take effect if order were wrong.
		cfg.Storage.S3.Region = ""
		if err := cfg.LoadFromEnv(); err != nil {
			t.Fatalf("LoadFromEnv() error = %v", err)
		}
		if cfg.Storage.S3.Region != "us-west-2" {
			t.Errorf("Region: got %q, want %q", cfg.Storage.S3.Region, "us-west-2")
		}
	})

	t.Run("OBJECTFS_S3_ENDPOINT sets endpoint", func(t *testing.T) {
		t.Setenv("OBJECTFS_S3_ENDPOINT", "http://localhost:9000")
		cfg := NewDefault()
		if err := cfg.LoadFromEnv(); err != nil {
			t.Fatalf("LoadFromEnv() error = %v", err)
		}
		if cfg.Storage.S3.Endpoint != "http://localhost:9000" {
			t.Errorf("Endpoint: got %q, want %q", cfg.Storage.S3.Endpoint, "http://localhost:9000")
		}
	})
}

func TestSaveToFile(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "saved_config.yaml")

	cfg := NewDefault()
	cfg.Global.LogLevel = TestDebugLevel
	cfg.Performance.CacheSize = TestCacheSize

	err := cfg.SaveToFile(configFile)
	if err != nil {
		t.Fatalf("SaveToFile() error = %v", err)
	}

	// Verify file exists
	if _, statErr := os.Stat(configFile); os.IsNotExist(statErr) {
		t.Error("Config file was not created")
	}

	// Load the saved config and verify
	newCfg := NewDefault()
	err = newCfg.LoadFromFile(configFile)
	if err != nil {
		t.Fatalf("Failed to load saved config: %v", err)
	}

	if newCfg.Global.LogLevel != TestDebugLevel {
		t.Errorf("Expected LogLevel to be DEBUG, got %s", newCfg.Global.LogLevel)
	}
	if newCfg.Performance.CacheSize != TestCacheSize {
		t.Errorf("Expected CacheSize to be 8GB, got %s", newCfg.Performance.CacheSize)
	}
}

func TestSaveToFileCreateDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "subdir", "config.yaml")

	cfg := NewDefault()
	err := cfg.SaveToFile(configFile)
	if err != nil {
		t.Fatalf("SaveToFile() error = %v", err)
	}

	// Verify file exists
	if _, statErr := os.Stat(configFile); os.IsNotExist(statErr) {
		t.Error("Config file was not created")
	}

	// Verify directory was created
	if _, err := os.Stat(filepath.Dir(configFile)); os.IsNotExist(err) {
		t.Error("Config directory was not created")
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			indexOf(s, substr) >= 0)))
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
