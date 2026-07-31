package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/objectfs/objectfs/internal/compression"
	"github.com/objectfs/objectfs/pkg/utils"
	"gopkg.in/yaml.v2"
)

// Boolean Constants
const (
	TrueValue = "true"
)

// Configuration represents the complete application configuration
type Configuration struct {
	Global      GlobalConfig      `yaml:"global"`
	Storage     StorageConfig     `yaml:"storage"`
	Performance PerformanceConfig `yaml:"performance"`
	Cache       CacheConfig       `yaml:"cache"`
	WriteBuffer WriteBufferConfig `yaml:"write_buffer"`
	Network     NetworkConfig     `yaml:"network"`
	Security    SecurityConfig    `yaml:"security"`
	Monitoring  MonitoringConfig  `yaml:"monitoring"`
	Features    FeatureConfig     `yaml:"features"`
	Cluster     ClusterConfig     `yaml:"cluster"`
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
	CacheSize          string             `yaml:"cache_size"`
	WriteBufferSize    string             `yaml:"write_buffer_size"`
	MaxConcurrency     int                `yaml:"max_concurrency"`
	ReadAheadSize      string             `yaml:"read_ahead_size"`
	CompressionEnabled bool               `yaml:"compression_enabled"`
	ConnectionPoolSize int                `yaml:"connection_pool_size"`
	PredictiveCaching  bool               `yaml:"predictive_caching"`
	MLModelPath        string             `yaml:"ml_model_path"`
	MultilevelCaching  bool               `yaml:"multilevel_caching"`
	ReadAhead          ReadAheadConfig    `yaml:"read_ahead"`    // Advanced read-ahead configuration
	ParallelRead       ParallelReadConfig `yaml:"parallel_read"` // Parallel range GET configuration
}

// ParallelReadConfig controls fan-out of large object reads into concurrent range GETs.
type ParallelReadConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Threshold string `yaml:"threshold"`  // e.g. "64MB"
	ChunkSize string `yaml:"chunk_size"` // e.g. "16MB"
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

// ReadAheadConfig represents advanced read-ahead and predictive caching settings
type ReadAheadConfig struct {
	// Basic read-ahead settings
	Enabled           bool   `yaml:"enabled"`             // Enable read-ahead
	Size              string `yaml:"size"`                // Read-ahead buffer size (e.g., "64MB")
	Strategy          string `yaml:"strategy"`            // "simple", "predictive", "ml"
	SequentialMinSize string `yaml:"sequential_min_size"` // Min size for sequential detection

	// Pattern detection settings
	EnablePatternDetection bool    `yaml:"enable_pattern_detection"` // Detect sequential/temporal patterns
	SequentialThreshold    float64 `yaml:"sequential_threshold"`     // Confidence threshold for sequential (0-1)
	PredictionWindow       int     `yaml:"prediction_window"`        // Number of accesses to analyze

	// Prefetch settings
	EnablePrefetch       bool    `yaml:"enable_prefetch"`        // Enable intelligent prefetching
	MaxConcurrentFetch   int     `yaml:"max_concurrent_fetch"`   // Max parallel prefetch operations
	PrefetchAhead        int     `yaml:"prefetch_ahead"`         // Number of blocks to prefetch
	PrefetchBandwidthMBs int     `yaml:"prefetch_bandwidth_mbs"` // Max prefetch bandwidth (MB/s)
	ConfidenceThreshold  float64 `yaml:"confidence_threshold"`   // Min confidence to trigger prefetch (0-1)

	// ML-based prediction settings (advanced)
	EnableMLPrediction bool    `yaml:"enable_ml_prediction"` // Use ML for access prediction
	MLModelPath        string  `yaml:"ml_model_path"`        // Path to trained ML model
	LearningRate       float64 `yaml:"learning_rate"`        // Model learning rate (0-1)
	PatternDepth       int     `yaml:"pattern_depth"`        // Analysis depth for patterns

	// Performance tuning
	MetricsEnabled      bool   `yaml:"metrics_enabled"`       // Track read-ahead effectiveness
	StatisticsInterval  string `yaml:"statistics_interval"`   // Stats collection interval
	ModelUpdateInterval string `yaml:"model_update_interval"` // ML model update frequency
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
	Timeouts            TimeoutConfig        `yaml:"timeouts"`
	Retry               RetryConfig          `yaml:"retry"`
	CircuitBreaker      CircuitBreakerConfig `yaml:"circuit_breaker"`
	CongestionAlgorithm string               `yaml:"congestion_algorithm"` // "auto", "bbr", "cubic", "reno"
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
	Enabled     bool             `yaml:"enabled"`
	AuthMethod  string           `yaml:"auth_method"`
	TLSEnabled  bool             `yaml:"tls_enabled"`
	TLSCertPath string           `yaml:"tls_cert_path"`
	TLSKeyPath  string           `yaml:"tls_key_path"`
	TLS         TLSConfig        `yaml:"tls"`
	Encryption  EncryptionConfig `yaml:"encryption"`
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
	Enabled         bool                `yaml:"enabled"`
	MetricsAddr     string              `yaml:"metrics_addr"`
	EnablePprof     bool                `yaml:"enable_pprof"`
	HealthCheckAddr string              `yaml:"health_check_addr"`
	OpenTelemetry   OpenTelemetryConfig `yaml:"opentelemetry"`
	Metrics         MetricsConfig       `yaml:"metrics"`
	HealthChecks    HealthChecksConfig  `yaml:"health_checks"`
	Logging         LoggingConfig       `yaml:"logging"`
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

// OpenTelemetryConfig represents OpenTelemetry settings
type OpenTelemetryConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Endpoint    string `yaml:"endpoint"`
	ServiceName string `yaml:"service_name"`
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

// StorageConfig represents storage backend configuration
type StorageConfig struct {
	S3 S3Config `yaml:"s3"`
}

// S3Config represents AWS S3 configuration
type S3Config struct {
	Region           string             `yaml:"region"`
	Endpoint         string             `yaml:"endpoint"`
	Profile          string             `yaml:"profile"`
	UseAcceleration  bool               `yaml:"use_acceleration"`
	ForcePathStyle   bool               `yaml:"force_path_style"`
	CostOptimization S3CostOptimization `yaml:"cost_optimization"`
}

// S3CostOptimization represents S3 cost optimization settings
type S3CostOptimization struct {
	Enabled             bool `yaml:"enabled"`
	TieringEnabled      bool `yaml:"tiering_enabled"`
	LifecycleEnabled    bool `yaml:"lifecycle_enabled"`
	TransitionToIA      int  `yaml:"transition_to_ia"`
	TransitionToGlacier int  `yaml:"transition_to_glacier"`
}

// ClusterConfig represents distributed cluster settings
type ClusterConfig struct {
	Enabled           bool        `yaml:"enabled"`
	NodeID            string      `yaml:"node_id"`
	ListenAddr        string      `yaml:"listen_addr"`
	AdvertiseAddr     string      `yaml:"advertise_addr"`
	SeedNodes         []string    `yaml:"seed_nodes"`
	ReplicationFactor int         `yaml:"replication_factor"`
	ConsistencyLevel  string      `yaml:"consistency_level"`
	Redis             RedisConfig `yaml:"redis"`
}

// RedisConfig represents Redis distributed cache settings
type RedisConfig struct {
	Enabled    bool          `yaml:"enabled"`
	Address    string        `yaml:"address"`
	Password   string        `yaml:"password"`
	DB         int           `yaml:"db"`
	KeyPrefix  string        `yaml:"key_prefix"`
	TTL        time.Duration `yaml:"ttl"`
	MaxRetries int           `yaml:"max_retries"`
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
		Storage: StorageConfig{
			S3: S3Config{
				Region:          "us-east-1",
				Endpoint:        "",
				Profile:         "",
				UseAcceleration: false,
				ForcePathStyle:  false,
				CostOptimization: S3CostOptimization{
					Enabled:             false,
					TieringEnabled:      false,
					LifecycleEnabled:    false,
					TransitionToIA:      30,
					TransitionToGlacier: 90,
				},
			},
		},
		Performance: PerformanceConfig{
			CacheSize:          "2GB",
			WriteBufferSize:    "16MB",
			MaxConcurrency:     150,
			ReadAheadSize:      "64MB",
			CompressionEnabled: true,
			ConnectionPoolSize: 8,
			PredictiveCaching:  false,
			MLModelPath:        "",
			MultilevelCaching:  false,
			ReadAhead: ReadAheadConfig{
				Enabled:                true,
				Size:                   "64MB",
				Strategy:               "predictive",
				SequentialMinSize:      "1MB",
				EnablePatternDetection: true,
				SequentialThreshold:    0.7,
				PredictionWindow:       100,
				EnablePrefetch:         true,
				MaxConcurrentFetch:     4,
				PrefetchAhead:          3,
				PrefetchBandwidthMBs:   10,
				ConfidenceThreshold:    0.7,
				EnableMLPrediction:     false,
				MLModelPath:            "",
				LearningRate:           0.01,
				PatternDepth:           1000,
				MetricsEnabled:         true,
				StatisticsInterval:     "30s",
				ModelUpdateInterval:    "5m",
			},
			ParallelRead: ParallelReadConfig{
				Enabled:   true,
				Threshold: "64MB",
				ChunkSize: "16MB",
			},
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
			// Compression is off by default. It is a storage-format decision, not a performance
			// knob: a compressed object is an opaque frame to `aws s3 cp`, boto3, and every other
			// S3 client, so enabling it by default would silently revoke the "my data is just
			// objects in S3" property that most users assume. It also makes a ranged read fetch the
			// whole object, since a zstd frame cannot be sliced. Opt in when the tradeoff is wanted.
			//
			// The algorithm is named even though compression is disabled, so that flipping Enabled
			// to true does not also have to supply an algorithm.
			Compression: CompressionConfig{
				Enabled:   false,
				MinSize:   "4KB",
				Algorithm: "zstd",
				Level:     3,
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
			CongestionAlgorithm: "auto",
		},
		Security: SecurityConfig{
			Enabled:     false,
			AuthMethod:  "none",
			TLSEnabled:  false,
			TLSCertPath: "",
			TLSKeyPath:  "",
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
			Enabled:         false,
			MetricsAddr:     ":9090",
			EnablePprof:     false,
			HealthCheckAddr: ":8081",
			OpenTelemetry: OpenTelemetryConfig{
				Enabled:     false,
				Endpoint:    "localhost:4317",
				ServiceName: "objectfs",
			},
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
		Cluster: ClusterConfig{
			Enabled:           false,
			NodeID:            "",
			ListenAddr:        "0.0.0.0:8080",
			AdvertiseAddr:     "127.0.0.1:8080",
			SeedNodes:         []string{},
			ReplicationFactor: 3,
			ConsistencyLevel:  "eventual",
			Redis: RedisConfig{
				Enabled:    false,
				Address:    "localhost:6379",
				Password:   "",
				DB:         0,
				KeyPrefix:  "objectfs",
				TTL:        5 * time.Minute,
				MaxRetries: 3,
			},
		},
	}
}

// LoadFromFile loads configuration from a YAML file
func (c *Configuration) LoadFromFile(filename string) error {
	// Validate file path to prevent directory traversal
	if err := utils.ValidatePath(filename, true); err != nil {
		return fmt.Errorf("invalid config file path: %w", err)
	}

	cleanPath := filepath.Clean(filename)
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, c); err != nil {
		return fmt.Errorf("failed to parse config file: %w", err)
	}

	return nil
}

// envMapping defines environment variable mappings and setters
type envMapping struct {
	envVar string
	setter func(*Configuration, string) error
}

// getEnvMappings returns all environment variable mappings
func getEnvMappings() []envMapping {
	return []envMapping{
		// Global settings
		{"OBJECTFS_LOG_LEVEL", func(c *Configuration, val string) error {
			c.Global.LogLevel = val
			return nil
		}},
		{"OBJECTFS_LOG_FILE", func(c *Configuration, val string) error {
			c.Global.LogFile = val
			return nil
		}},
		{"OBJECTFS_METRICS_PORT", func(c *Configuration, val string) error {
			if port, err := strconv.Atoi(val); err == nil {
				c.Global.MetricsPort = port
			}
			return nil
		}},

		// Performance settings
		{"OBJECTFS_CACHE_SIZE", func(c *Configuration, val string) error {
			c.Performance.CacheSize = val
			return nil
		}},
		{"OBJECTFS_WRITE_BUFFER_SIZE", func(c *Configuration, val string) error {
			c.Performance.WriteBufferSize = val
			return nil
		}},
		{"OBJECTFS_MAX_CONCURRENCY", func(c *Configuration, val string) error {
			if concurrency, err := strconv.Atoi(val); err == nil {
				c.Performance.MaxConcurrency = concurrency
			}
			return nil
		}},
		{"OBJECTFS_READ_AHEAD_SIZE", func(c *Configuration, val string) error {
			c.Performance.ReadAheadSize = val
			return nil
		}},
		{"OBJECTFS_COMPRESSION_ENABLED", func(c *Configuration, val string) error {
			c.Performance.CompressionEnabled = strings.ToLower(val) == TrueValue
			return nil
		}},
		{"OBJECTFS_CONNECTION_POOL_SIZE", func(c *Configuration, val string) error {
			if poolSize, err := strconv.Atoi(val); err == nil {
				c.Performance.ConnectionPoolSize = poolSize
			}
			return nil
		}},

		// Cache settings
		{"OBJECTFS_CACHE_TTL", func(c *Configuration, val string) error {
			if duration, err := time.ParseDuration(val); err == nil {
				c.Cache.TTL = duration
			}
			return nil
		}},

		// Feature flags
		{"OBJECTFS_PREFETCHING", func(c *Configuration, val string) error {
			c.Features.Prefetching = strings.ToLower(val) == TrueValue
			return nil
		}},
		{"OBJECTFS_BATCH_OPERATIONS", func(c *Configuration, val string) error {
			c.Features.BatchOperations = strings.ToLower(val) == TrueValue
			return nil
		}},
		{"OBJECTFS_OFFLINE_MODE", func(c *Configuration, val string) error {
			c.Features.OfflineMode = strings.ToLower(val) == TrueValue
			return nil
		}},

		// S3 storage settings
		// AWS_DEFAULT_REGION is processed first (lowest priority); AWS_REGION
		// overrides it; OBJECTFS_S3_REGION takes highest precedence.
		{"AWS_DEFAULT_REGION", func(c *Configuration, val string) error {
			if c.Storage.S3.Region == "" {
				c.Storage.S3.Region = val
			}
			return nil
		}},
		{"AWS_REGION", func(c *Configuration, val string) error {
			c.Storage.S3.Region = val
			return nil
		}},
		{"OBJECTFS_S3_REGION", func(c *Configuration, val string) error {
			c.Storage.S3.Region = val
			return nil
		}},
		{"OBJECTFS_S3_ENDPOINT", func(c *Configuration, val string) error {
			c.Storage.S3.Endpoint = val
			return nil
		}},

		// Read-ahead settings
		{"OBJECTFS_READAHEAD_ENABLED", func(c *Configuration, val string) error {
			c.Performance.ReadAhead.Enabled = strings.ToLower(val) == TrueValue
			return nil
		}},
		{"OBJECTFS_READAHEAD_SIZE", func(c *Configuration, val string) error {
			c.Performance.ReadAhead.Size = val
			return nil
		}},
		{"OBJECTFS_READAHEAD_STRATEGY", func(c *Configuration, val string) error {
			c.Performance.ReadAhead.Strategy = val
			return nil
		}},
		{"OBJECTFS_READAHEAD_PATTERN_DETECTION", func(c *Configuration, val string) error {
			c.Performance.ReadAhead.EnablePatternDetection = strings.ToLower(val) == TrueValue
			return nil
		}},
		{"OBJECTFS_READAHEAD_PREFETCH", func(c *Configuration, val string) error {
			c.Performance.ReadAhead.EnablePrefetch = strings.ToLower(val) == TrueValue
			return nil
		}},
		{"OBJECTFS_READAHEAD_ML_PREDICTION", func(c *Configuration, val string) error {
			c.Performance.ReadAhead.EnableMLPrediction = strings.ToLower(val) == TrueValue
			return nil
		}},
	}
}

// LoadFromEnv loads configuration from environment variables
func (c *Configuration) LoadFromEnv() error {
	for _, mapping := range getEnvMappings() {
		if val := os.Getenv(mapping.envVar); val != "" {
			if err := mapping.setter(c, val); err != nil {
				return fmt.Errorf("failed to set %s: %w", mapping.envVar, err)
			}
		}
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

	// Validate read-ahead configuration
	if err := c.validateReadAheadConfig(); err != nil {
		return fmt.Errorf("read_ahead configuration invalid: %w", err)
	}

	if err := validateCompressionConfig(c.WriteBuffer.Compression); err != nil {
		return fmt.Errorf("write_buffer.compression configuration invalid: %w", err)
	}

	return nil
}

// validateCompressionConfig rejects a compression configuration no codec can be built from.
//
// This check is the seam that produced the worst defect in v0.10.0. The algorithm was defaulted to
// "gzip" here, pkg/compression declared AlgorithmGzip, internal/storage/s3's config comment listed
// it, and two shipped example config files set it — while internal/compression's codec factory had
// no gzip case. Every layer that read config agreed the value was valid, so the disagreement only
// surfaced inside NewBackend, by which point the user had asked for a mount and got
// "Failed to start adapter" with no indication which setting was at fault.
//
// It validates by *building the codec* rather than by comparing against a list of names. A list here
// would be a second authority, free to drift from the factory exactly as the first one did, and it
// could not catch a level that is out of range for the chosen algorithm at all — zstd accepts 0-22
// and gzip only 0-9, so "level: 12" is valid for one and not the other. Construction is cheap and it
// is the same call the backend will make.
func validateCompressionConfig(cfg CompressionConfig) error {
	if !cfg.Enabled {
		// A disabled block is not consulted, and rejecting a stale algorithm in it would refuse to
		// start over a setting that has no effect.
		return nil
	}

	if _, err := compression.NewCompressor(compression.Settings{
		Enabled:   true,
		Algorithm: cfg.Algorithm,
		Level:     cfg.Level,
		MinSize:   cfg.MinSize,
	}); err != nil {
		return err
	}

	return nil
}

// validateReadAheadConfig validates read-ahead specific settings
func (c *Configuration) validateReadAheadConfig() error {
	ra := c.Performance.ReadAhead

	// Validate strategy
	validStrategies := []string{"simple", "predictive", "ml"}
	strategyValid := false
	for _, strategy := range validStrategies {
		if ra.Strategy == strategy {
			strategyValid = true
			break
		}
	}
	if !strategyValid {
		return fmt.Errorf("invalid strategy: %s (must be one of: %s)",
			ra.Strategy, strings.Join(validStrategies, ", "))
	}

	// Validate thresholds
	if ra.SequentialThreshold < 0 || ra.SequentialThreshold > 1 {
		return fmt.Errorf("sequential_threshold must be between 0 and 1, got %f", ra.SequentialThreshold)
	}

	if ra.ConfidenceThreshold < 0 || ra.ConfidenceThreshold > 1 {
		return fmt.Errorf("confidence_threshold must be between 0 and 1, got %f", ra.ConfidenceThreshold)
	}

	if ra.LearningRate < 0 || ra.LearningRate > 1 {
		return fmt.Errorf("learning_rate must be between 0 and 1, got %f", ra.LearningRate)
	}

	// Validate positive integers
	if ra.PredictionWindow < 0 {
		return fmt.Errorf("prediction_window must be non-negative, got %d", ra.PredictionWindow)
	}

	if ra.MaxConcurrentFetch <= 0 {
		return fmt.Errorf("max_concurrent_fetch must be greater than 0, got %d", ra.MaxConcurrentFetch)
	}

	if ra.PrefetchAhead < 0 {
		return fmt.Errorf("prefetch_ahead must be non-negative, got %d", ra.PrefetchAhead)
	}

	if ra.PrefetchBandwidthMBs < 0 {
		return fmt.Errorf("prefetch_bandwidth_mbs must be non-negative, got %d", ra.PrefetchBandwidthMBs)
	}

	if ra.PatternDepth < 0 {
		return fmt.Errorf("pattern_depth must be non-negative, got %d", ra.PatternDepth)
	}

	// Validate ML settings if enabled
	if ra.EnableMLPrediction && ra.MLModelPath == "" {
		return fmt.Errorf("ml_model_path must be specified when enable_ml_prediction is true")
	}

	return nil
}
