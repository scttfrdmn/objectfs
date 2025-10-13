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
	if cfg.Global.MetricsPort != 8080 {
		t.Errorf("Expected MetricsPort to be 8080, got %d", cfg.Global.MetricsPort)
	}
	if cfg.Global.HealthPort != 8081 {
		t.Errorf("Expected HealthPort to be 8081, got %d", cfg.Global.HealthPort)
	}

	// Test performance defaults
	if cfg.Performance.CacheSize != "2GB" {
		t.Errorf("Expected CacheSize to be 2GB, got %s", cfg.Performance.CacheSize)
	}
	if cfg.Performance.MaxConcurrency != 150 {
		t.Errorf("Expected MaxConcurrency to be 150, got %d", cfg.Performance.MaxConcurrency)
	}
	if !cfg.Performance.CompressionEnabled {
		t.Error("Expected CompressionEnabled to be true")
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
			name: "same metrics and health ports",
			config: func() *Configuration {
				cfg := NewDefault()
				cfg.Global.MetricsPort = 8080
				cfg.Global.HealthPort = 8080
				return cfg
			},
			wantErr: true,
			errMsg:  "metrics_port and health_port cannot be the same",
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
  metrics_port: 9090
  health_port: 9091

performance:
  cache_size: 4GB
  max_concurrency: 200
  compression_enabled: false

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
	if cfg.Global.MetricsPort != 9090 {
		t.Errorf("Expected MetricsPort to be 9090, got %d", cfg.Global.MetricsPort)
	}
	if cfg.Performance.CacheSize != "4GB" {
		t.Errorf("Expected CacheSize to be 4GB, got %s", cfg.Performance.CacheSize)
	}
	if cfg.Performance.MaxConcurrency != 200 {
		t.Errorf("Expected MaxConcurrency to be 200, got %d", cfg.Performance.MaxConcurrency)
	}
	if cfg.Performance.CompressionEnabled {
		t.Error("Expected CompressionEnabled to be false")
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
		"OBJECTFS_METRICS_PORT":        "9090",
		"OBJECTFS_CACHE_SIZE":          TestCacheSize,
		"OBJECTFS_MAX_CONCURRENCY":     "300",
		"OBJECTFS_COMPRESSION_ENABLED": "false",
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
	if cfg.Global.MetricsPort != 9090 {
		t.Errorf("Expected MetricsPort to be 9090, got %d", cfg.Global.MetricsPort)
	}
	if cfg.Performance.CacheSize != TestCacheSize {
		t.Errorf("Expected CacheSize to be 8GB, got %s", cfg.Performance.CacheSize)
	}
	if cfg.Performance.MaxConcurrency != 300 {
		t.Errorf("Expected MaxConcurrency to be 300, got %d", cfg.Performance.MaxConcurrency)
	}
	if cfg.Performance.CompressionEnabled {
		t.Error("Expected CompressionEnabled to be false")
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
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
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
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
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
