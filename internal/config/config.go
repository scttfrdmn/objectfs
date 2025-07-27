package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// Configuration represents the complete application configuration
type Configuration struct {
	Global      GlobalConfig      `yaml:"global"`
	Performance PerformanceConfig `yaml:"performance"`
	Cache       CacheConfig       `yaml:"cache"`
	WriteBuffer WriteBufferConfig `yaml:"write_buffer"`
	Network     NetworkConfig     `yaml:"network"`
	Security    SecurityConfig    `yaml:"security"`
	Monitoring  MonitoringConfig  `yaml:"monitoring"`
	Features    FeatureConfig     `yaml:"features"`
}

// GlobalConfig represents global application settings
type GlobalConfig struct {
	LogLevel    string `yaml:"log_level"`
	LogFile     string `yaml:"log_file"`
	MetricsPort int    `yaml:"metrics_port"`
	HealthPort  int    `yaml:"health_port"`
	ProfilePort int    `yaml:"profile_port"`
}

// PerformanceConfig represents performance-related settings
type PerformanceConfig struct {
	CacheSize          string `yaml:"cache_size"`
	WriteBufferSize    string `yaml:"write_buffer_size"`
	MaxConcurrency     int    `yaml:"max_concurrency"`
	ReadAheadSize      string `yaml:"read_ahead_size"`
	CompressionEnabled bool   `yaml:"compression_enabled"`
	ConnectionPoolSize int    `yaml:"connection_pool_size"`
}

// CacheConfig represents cache configuration
type CacheConfig struct {
	TTL             time.Duration         `yaml:"ttl"`
	MaxEntries      int                   `yaml:"max_entries"`
	EvictionPolicy  string                `yaml:"eviction_policy"`
	PersistentCache PersistentCacheConfig `yaml:"persistent_cache"`
}

// PersistentCacheConfig represents persistent cache settings
type PersistentCacheConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Directory string `yaml:"directory"`
	MaxSize   string `yaml:"max_size"`
}

// WriteBufferConfig represents write buffer configuration
type WriteBufferConfig struct {
	FlushInterval time.Duration     `yaml:"flush_interval"`
	MaxBuffers    int               `yaml:"max_buffers"`
	MaxMemory     string            `yaml:"max_memory"`
	Compression   CompressionConfig `yaml:"compression"`
}

// CompressionConfig represents compression settings
type CompressionConfig struct {
	Enabled   bool   `yaml:"enabled"`
	MinSize   string `yaml:"min_size"`
	Algorithm string `yaml:"algorithm"`
	Level     int    `yaml:"level"`
}

// NetworkConfig represents network configuration
type NetworkConfig struct {
	Timeouts       TimeoutConfig        `yaml:"timeouts"`
	Retry          RetryConfig          `yaml:"retry"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
}

// TimeoutConfig represents timeout settings
type TimeoutConfig struct {
	Connect time.Duration `yaml:"connect"`
	Read    time.Duration `yaml:"read"`
	Write   time.Duration `yaml:"write"`
}

// RetryConfig represents retry settings
type RetryConfig struct {
	MaxAttempts int           `yaml:"max_attempts"`
	BaseDelay   time.Duration `yaml:"base_delay"`
	MaxDelay    time.Duration `yaml:"max_delay"`
}

// CircuitBreakerConfig represents circuit breaker settings
type CircuitBreakerConfig struct {
	Enabled          bool          `yaml:"enabled"`
	FailureThreshold int           `yaml:"failure_threshold"`
	Timeout          time.Duration `yaml:"timeout"`
}

// SecurityConfig represents security settings
type SecurityConfig struct {
	TLS        TLSConfig        `yaml:"tls"`
	Encryption EncryptionConfig `yaml:"encryption"`
}

// TLSConfig represents TLS settings
type TLSConfig struct {
	VerifyCertificates bool   `yaml:"verify_certificates"`
	MinVersion         string `yaml:"min_version"`
}

// EncryptionConfig represents encryption settings
type EncryptionConfig struct {
	InTransit bool `yaml:"in_transit"`
	AtRest    bool `yaml:"at_rest"`
}

// MonitoringConfig represents monitoring settings
type MonitoringConfig struct {
	Metrics      MetricsConfig      `yaml:"metrics"`
	HealthChecks HealthChecksConfig `yaml:"health_checks"`
	Logging      LoggingConfig      `yaml:"logging"`
}

// MetricsConfig represents metrics settings
type MetricsConfig struct {
	Enabled      bool              `yaml:"enabled"`
	Prometheus   bool              `yaml:"prometheus"`
	CustomLabels map[string]string `yaml:"custom_labels"`
}

// HealthChecksConfig represents health check settings
type HealthChecksConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

// LoggingConfig represents logging settings
type LoggingConfig struct {
	Structured bool           `yaml:"structured"`
	Format     string         `yaml:"format"`
	Sampling   SamplingConfig `yaml:"sampling"`
}

// SamplingConfig represents log sampling settings
type SamplingConfig struct {
	Enabled bool `yaml:"enabled"`
	Rate    int  `yaml:"rate"`
}

// FeatureConfig represents feature flags
type FeatureConfig struct {
	Prefetching           bool `yaml:"prefetching"`
	BatchOperations       bool `yaml:"batch_operations"`
	SmallFileOptimization bool `yaml:"small_file_optimization"`
	MetadataCaching       bool `yaml:"metadata_caching"`
	OfflineMode           bool `yaml:"offline_mode"`
}

// NewDefault returns a configuration with sensible defaults
func NewDefault() *Configuration {
	return &Configuration{
		Global: GlobalConfig{
			LogLevel:    "INFO",
			LogFile:     "",
			MetricsPort: 8080,
			HealthPort:  8081,
			ProfilePort: 6060,
		},
		Performance: PerformanceConfig{
			CacheSize:          "2GB",
			WriteBufferSize:    "16MB",
			MaxConcurrency:     150,
			ReadAheadSize:      "64MB",
			CompressionEnabled: true,
			ConnectionPoolSize: 8,
		},
		Cache: CacheConfig{
			TTL:            5 * time.Minute,
			MaxEntries:     100000,
			EvictionPolicy: "weighted_lru",
			PersistentCache: PersistentCacheConfig{
				Enabled:   false,
				Directory: "/var/cache/objectfs",
				MaxSize:   "10GB",
			},
		},
		WriteBuffer: WriteBufferConfig{
			FlushInterval: 30 * time.Second,
			MaxBuffers:    1000,
			MaxMemory:     "512MB",
			Compression: CompressionConfig{
				Enabled:   true,
				MinSize:   "1KB",
				Algorithm: "gzip",
				Level:     6,
			},
		},
		Network: NetworkConfig{
			Timeouts: TimeoutConfig{
				Connect: 10 * time.Second,
				Read:    30 * time.Second,
				Write:   300 * time.Second,
			},
			Retry: RetryConfig{
				MaxAttempts: 3,
				BaseDelay:   1 * time.Second,
				MaxDelay:    30 * time.Second,
			},
			CircuitBreaker: CircuitBreakerConfig{
				Enabled:          true,
				FailureThreshold: 5,
				Timeout:          60 * time.Second,
			},
		},
		Security: SecurityConfig{
			TLS: TLSConfig{
				VerifyCertificates: true,
				MinVersion:         "1.2",
			},
			Encryption: EncryptionConfig{
				InTransit: true,
				AtRest:    true,
			},
		},
		Monitoring: MonitoringConfig{
			Metrics: MetricsConfig{
				Enabled:    true,
				Prometheus: true,
				CustomLabels: map[string]string{
					"service": "objectfs",
				},
			},
			HealthChecks: HealthChecksConfig{
				Enabled:  true,
				Interval: 30 * time.Second,
				Timeout:  5 * time.Second,
			},
			Logging: LoggingConfig{
				Structured: true,
				Format:     "json",
				Sampling: SamplingConfig{
					Enabled: true,
					Rate:    1000,
				},
			},
		},
		Features: FeatureConfig{
			Prefetching:           true,
			BatchOperations:       true,
			SmallFileOptimization: true,
			MetadataCaching:       true,
			OfflineMode:           false,
		},
	}
}

// LoadFromFile loads configuration from a YAML file
func (c *Configuration) LoadFromFile(filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, c); err != nil {
		return fmt.Errorf("failed to parse config file: %w", err)
	}

	return nil
}

// LoadFromEnv loads configuration from environment variables
func (c *Configuration) LoadFromEnv() error {
	// Global settings
	if val := os.Getenv("OBJECTFS_LOG_LEVEL"); val != "" {
		c.Global.LogLevel = val
	}
	if val := os.Getenv("OBJECTFS_LOG_FILE"); val != "" {
		c.Global.LogFile = val
	}
	if val := os.Getenv("OBJECTFS_METRICS_PORT"); val != "" {
		if port, err := strconv.Atoi(val); err == nil {
			c.Global.MetricsPort = port
		}
	}

	// Performance settings
	if val := os.Getenv("OBJECTFS_CACHE_SIZE"); val != "" {
		c.Performance.CacheSize = val
	}
	if val := os.Getenv("OBJECTFS_WRITE_BUFFER_SIZE"); val != "" {
		c.Performance.WriteBufferSize = val
	}
	if val := os.Getenv("OBJECTFS_MAX_CONCURRENCY"); val != "" {
		if concurrency, err := strconv.Atoi(val); err == nil {
			c.Performance.MaxConcurrency = concurrency
		}
	}
	if val := os.Getenv("OBJECTFS_READ_AHEAD_SIZE"); val != "" {
		c.Performance.ReadAheadSize = val
	}
	if val := os.Getenv("OBJECTFS_COMPRESSION_ENABLED"); val != "" {
		c.Performance.CompressionEnabled = strings.ToLower(val) == "true"
	}
	if val := os.Getenv("OBJECTFS_CONNECTION_POOL_SIZE"); val != "" {
		if poolSize, err := strconv.Atoi(val); err == nil {
			c.Performance.ConnectionPoolSize = poolSize
		}
	}

	// Cache settings
	if val := os.Getenv("OBJECTFS_CACHE_TTL"); val != "" {
		if duration, err := time.ParseDuration(val); err == nil {
			c.Cache.TTL = duration
		}
	}

	// Feature flags
	if val := os.Getenv("OBJECTFS_PREFETCHING"); val != "" {
		c.Features.Prefetching = strings.ToLower(val) == "true"
	}
	if val := os.Getenv("OBJECTFS_BATCH_OPERATIONS"); val != "" {
		c.Features.BatchOperations = strings.ToLower(val) == "true"
	}
	if val := os.Getenv("OBJECTFS_OFFLINE_MODE"); val != "" {
		c.Features.OfflineMode = strings.ToLower(val) == "true"
	}

	return nil
}

// SaveToFile saves the configuration to a YAML file
func (c *Configuration) SaveToFile(filename string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(filename), 0750); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	if err := os.WriteFile(filename, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// Validate validates the configuration
func (c *Configuration) Validate() error {
	if c.Performance.MaxConcurrency <= 0 {
		return fmt.Errorf("max_concurrency must be greater than 0")
	}

	if c.Performance.ConnectionPoolSize <= 0 {
		return fmt.Errorf("connection_pool_size must be greater than 0")
	}

	if c.Global.MetricsPort == c.Global.HealthPort {
		return fmt.Errorf("metrics_port and health_port cannot be the same")
	}

	validLogLevels := []string{"DEBUG", "INFO", "WARN", "ERROR"}
	logLevelValid := false
	for _, level := range validLogLevels {
		if c.Global.LogLevel == level {
			logLevelValid = true
			break
		}
	}
	if !logLevelValid {
		return fmt.Errorf("invalid log_level: %s (must be one of: %s)", 
			c.Global.LogLevel, strings.Join(validLogLevels, ", "))
	}

	return nil
}