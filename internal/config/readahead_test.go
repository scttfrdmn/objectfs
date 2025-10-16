package config

import (
	"os"
	"testing"
)

// TestReadAheadConfig_Defaults tests the default read-ahead configuration
func TestReadAheadConfig_Defaults(t *testing.T) {
	cfg := NewDefault()

	if !cfg.Performance.ReadAhead.Enabled {
		t.Error("Expected read-ahead to be enabled by default")
	}

	if cfg.Performance.ReadAhead.Size != "64MB" {
		t.Errorf("Expected default read-ahead size to be 64MB, got %s", cfg.Performance.ReadAhead.Size)
	}

	if cfg.Performance.ReadAhead.Strategy != "predictive" {
		t.Errorf("Expected default strategy to be 'predictive', got %s", cfg.Performance.ReadAhead.Strategy)
	}

	if !cfg.Performance.ReadAhead.EnablePatternDetection {
		t.Error("Expected pattern detection to be enabled by default")
	}

	if cfg.Performance.ReadAhead.SequentialThreshold != 0.7 {
		t.Errorf("Expected default sequential threshold to be 0.7, got %f", cfg.Performance.ReadAhead.SequentialThreshold)
	}

	if !cfg.Performance.ReadAhead.EnablePrefetch {
		t.Error("Expected prefetch to be enabled by default")
	}

	if cfg.Performance.ReadAhead.MaxConcurrentFetch != 4 {
		t.Errorf("Expected default max concurrent fetch to be 4, got %d", cfg.Performance.ReadAhead.MaxConcurrentFetch)
	}

	if cfg.Performance.ReadAhead.PrefetchAhead != 3 {
		t.Errorf("Expected default prefetch ahead to be 3, got %d", cfg.Performance.ReadAhead.PrefetchAhead)
	}

	if cfg.Performance.ReadAhead.PrefetchBandwidthMBs != 10 {
		t.Errorf("Expected default prefetch bandwidth to be 10 MB/s, got %d", cfg.Performance.ReadAhead.PrefetchBandwidthMBs)
	}

	if cfg.Performance.ReadAhead.ConfidenceThreshold != 0.7 {
		t.Errorf("Expected default confidence threshold to be 0.7, got %f", cfg.Performance.ReadAhead.ConfidenceThreshold)
	}

	if cfg.Performance.ReadAhead.EnableMLPrediction {
		t.Error("Expected ML prediction to be disabled by default")
	}

	if cfg.Performance.ReadAhead.LearningRate != 0.01 {
		t.Errorf("Expected default learning rate to be 0.01, got %f", cfg.Performance.ReadAhead.LearningRate)
	}

	if !cfg.Performance.ReadAhead.MetricsEnabled {
		t.Error("Expected metrics to be enabled by default")
	}
}

// TestReadAheadConfig_Validation tests validation of read-ahead configuration
func TestReadAheadConfig_Validation(t *testing.T) {
	tests := []struct {
		name        string
		modifier    func(*Configuration)
		expectError bool
	}{
		{
			name: "valid configuration",
			modifier: func(c *Configuration) {
				// Default config is valid
			},
			expectError: false,
		},
		{
			name: "invalid strategy",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.Strategy = "invalid"
			},
			expectError: true,
		},
		{
			name: "sequential threshold too low",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.SequentialThreshold = -0.1
			},
			expectError: true,
		},
		{
			name: "sequential threshold too high",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.SequentialThreshold = 1.5
			},
			expectError: true,
		},
		{
			name: "confidence threshold too low",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.ConfidenceThreshold = -0.1
			},
			expectError: true,
		},
		{
			name: "confidence threshold too high",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.ConfidenceThreshold = 1.5
			},
			expectError: true,
		},
		{
			name: "learning rate too low",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.LearningRate = -0.1
			},
			expectError: true,
		},
		{
			name: "learning rate too high",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.LearningRate = 1.5
			},
			expectError: true,
		},
		{
			name: "negative prediction window",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.PredictionWindow = -1
			},
			expectError: true,
		},
		{
			name: "zero max concurrent fetch",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.MaxConcurrentFetch = 0
			},
			expectError: true,
		},
		{
			name: "negative max concurrent fetch",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.MaxConcurrentFetch = -1
			},
			expectError: true,
		},
		{
			name: "negative prefetch ahead",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.PrefetchAhead = -1
			},
			expectError: true,
		},
		{
			name: "negative prefetch bandwidth",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.PrefetchBandwidthMBs = -1
			},
			expectError: true,
		},
		{
			name: "negative pattern depth",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.PatternDepth = -1
			},
			expectError: true,
		},
		{
			name: "ML prediction enabled without model path",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.EnableMLPrediction = true
				c.Performance.ReadAhead.MLModelPath = ""
			},
			expectError: true,
		},
		{
			name: "ML prediction enabled with model path",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.EnableMLPrediction = true
				c.Performance.ReadAhead.MLModelPath = "/path/to/model"
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewDefault()
			tt.modifier(cfg)

			err := cfg.Validate()
			if tt.expectError && err == nil {
				t.Error("Expected validation error, got nil")
			}
			if !tt.expectError && err != nil {
				t.Errorf("Expected no validation error, got: %v", err)
			}
		})
	}
}

// TestReadAheadConfig_EnvironmentVariables tests environment variable loading
func TestReadAheadConfig_EnvironmentVariables(t *testing.T) {
	// Save original environment and restore after test
	originalEnv := map[string]string{
		"OBJECTFS_READAHEAD_ENABLED":           os.Getenv("OBJECTFS_READAHEAD_ENABLED"),
		"OBJECTFS_READAHEAD_SIZE":              os.Getenv("OBJECTFS_READAHEAD_SIZE"),
		"OBJECTFS_READAHEAD_STRATEGY":          os.Getenv("OBJECTFS_READAHEAD_STRATEGY"),
		"OBJECTFS_READAHEAD_PATTERN_DETECTION": os.Getenv("OBJECTFS_READAHEAD_PATTERN_DETECTION"),
		"OBJECTFS_READAHEAD_PREFETCH":          os.Getenv("OBJECTFS_READAHEAD_PREFETCH"),
		"OBJECTFS_READAHEAD_ML_PREDICTION":     os.Getenv("OBJECTFS_READAHEAD_ML_PREDICTION"),
	}
	defer func() {
		for key, val := range originalEnv {
			if val != "" {
				if err := os.Setenv(key, val); err != nil {
					t.Errorf("Failed to restore env var %s: %v", key, err)
				}
			} else {
				if err := os.Unsetenv(key); err != nil {
					t.Errorf("Failed to unset env var %s: %v", key, err)
				}
			}
		}
	}()

	tests := []struct {
		name     string
		envVars  map[string]string
		validate func(*testing.T, *Configuration)
	}{
		{
			name: "enabled via environment",
			envVars: map[string]string{
				"OBJECTFS_READAHEAD_ENABLED": "true",
			},
			validate: func(t *testing.T, cfg *Configuration) {
				if !cfg.Performance.ReadAhead.Enabled {
					t.Error("Expected read-ahead to be enabled via environment variable")
				}
			},
		},
		{
			name: "disabled via environment",
			envVars: map[string]string{
				"OBJECTFS_READAHEAD_ENABLED": "false",
			},
			validate: func(t *testing.T, cfg *Configuration) {
				if cfg.Performance.ReadAhead.Enabled {
					t.Error("Expected read-ahead to be disabled via environment variable")
				}
			},
		},
		{
			name: "custom size via environment",
			envVars: map[string]string{
				"OBJECTFS_READAHEAD_SIZE": "128MB",
			},
			validate: func(t *testing.T, cfg *Configuration) {
				if cfg.Performance.ReadAhead.Size != "128MB" {
					t.Errorf("Expected read-ahead size to be 128MB, got %s", cfg.Performance.ReadAhead.Size)
				}
			},
		},
		{
			name: "strategy via environment",
			envVars: map[string]string{
				"OBJECTFS_READAHEAD_STRATEGY": "ml",
			},
			validate: func(t *testing.T, cfg *Configuration) {
				if cfg.Performance.ReadAhead.Strategy != "ml" {
					t.Errorf("Expected strategy to be 'ml', got %s", cfg.Performance.ReadAhead.Strategy)
				}
			},
		},
		{
			name: "pattern detection disabled via environment",
			envVars: map[string]string{
				"OBJECTFS_READAHEAD_PATTERN_DETECTION": "false",
			},
			validate: func(t *testing.T, cfg *Configuration) {
				if cfg.Performance.ReadAhead.EnablePatternDetection {
					t.Error("Expected pattern detection to be disabled via environment variable")
				}
			},
		},
		{
			name: "prefetch disabled via environment",
			envVars: map[string]string{
				"OBJECTFS_READAHEAD_PREFETCH": "false",
			},
			validate: func(t *testing.T, cfg *Configuration) {
				if cfg.Performance.ReadAhead.EnablePrefetch {
					t.Error("Expected prefetch to be disabled via environment variable")
				}
			},
		},
		{
			name: "ML prediction enabled via environment",
			envVars: map[string]string{
				"OBJECTFS_READAHEAD_ML_PREDICTION": "true",
			},
			validate: func(t *testing.T, cfg *Configuration) {
				if !cfg.Performance.ReadAhead.EnableMLPrediction {
					t.Error("Expected ML prediction to be enabled via environment variable")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear environment
			for key := range originalEnv {
				if err := os.Unsetenv(key); err != nil {
					t.Fatalf("Failed to unset env var %s: %v", key, err)
				}
			}

			// Set test environment variables
			for key, val := range tt.envVars {
				if err := os.Setenv(key, val); err != nil {
					t.Fatalf("Failed to set env var %s: %v", key, err)
				}
			}

			cfg := NewDefault()
			if err := cfg.LoadFromEnv(); err != nil {
				t.Fatalf("Failed to load from environment: %v", err)
			}

			tt.validate(t, cfg)
		})
	}
}

// TestReadAheadConfig_Strategies tests different strategy configurations
func TestReadAheadConfig_Strategies(t *testing.T) {
	tests := []struct {
		name                string
		strategy            string
		enablePrediction    bool
		enableML            bool
		mlModelPath         string
		expectValidationErr bool
	}{
		{
			name:                "simple strategy",
			strategy:            "simple",
			enablePrediction:    false,
			enableML:            false,
			mlModelPath:         "",
			expectValidationErr: false,
		},
		{
			name:                "predictive strategy",
			strategy:            "predictive",
			enablePrediction:    true,
			enableML:            false,
			mlModelPath:         "",
			expectValidationErr: false,
		},
		{
			name:                "ml strategy with model",
			strategy:            "ml",
			enablePrediction:    true,
			enableML:            true,
			mlModelPath:         "/path/to/model",
			expectValidationErr: false,
		},
		{
			name:                "ml strategy without model",
			strategy:            "ml",
			enablePrediction:    true,
			enableML:            true,
			mlModelPath:         "",
			expectValidationErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewDefault()
			cfg.Performance.ReadAhead.Strategy = tt.strategy
			cfg.Performance.ReadAhead.EnablePatternDetection = tt.enablePrediction
			cfg.Performance.ReadAhead.EnableMLPrediction = tt.enableML
			cfg.Performance.ReadAhead.MLModelPath = tt.mlModelPath

			err := cfg.Validate()
			if tt.expectValidationErr && err == nil {
				t.Error("Expected validation error, got nil")
			}
			if !tt.expectValidationErr && err != nil {
				t.Errorf("Expected no validation error, got: %v", err)
			}
		})
	}
}

// TestReadAheadConfig_BoundaryValues tests boundary value conditions
func TestReadAheadConfig_BoundaryValues(t *testing.T) {
	tests := []struct {
		name     string
		modifier func(*Configuration)
		valid    bool
	}{
		{
			name: "sequential threshold at 0",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.SequentialThreshold = 0.0
			},
			valid: true,
		},
		{
			name: "sequential threshold at 1",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.SequentialThreshold = 1.0
			},
			valid: true,
		},
		{
			name: "confidence threshold at 0",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.ConfidenceThreshold = 0.0
			},
			valid: true,
		},
		{
			name: "confidence threshold at 1",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.ConfidenceThreshold = 1.0
			},
			valid: true,
		},
		{
			name: "learning rate at 0",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.LearningRate = 0.0
			},
			valid: true,
		},
		{
			name: "learning rate at 1",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.LearningRate = 1.0
			},
			valid: true,
		},
		{
			name: "prediction window at 0",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.PredictionWindow = 0
			},
			valid: true,
		},
		{
			name: "max concurrent fetch at 1",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.MaxConcurrentFetch = 1
			},
			valid: true,
		},
		{
			name: "prefetch ahead at 0",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.PrefetchAhead = 0
			},
			valid: true,
		},
		{
			name: "prefetch bandwidth at 0",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.PrefetchBandwidthMBs = 0
			},
			valid: true,
		},
		{
			name: "pattern depth at 0",
			modifier: func(c *Configuration) {
				c.Performance.ReadAhead.PatternDepth = 0
			},
			valid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewDefault()
			tt.modifier(cfg)

			err := cfg.Validate()
			if tt.valid && err != nil {
				t.Errorf("Expected validation to pass, got error: %v", err)
			}
			if !tt.valid && err == nil {
				t.Error("Expected validation to fail, got nil")
			}
		})
	}
}
