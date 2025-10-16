package s3

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	awsconfig "github.com/scttfrdmn/cargoship/pkg/aws/config"
	cargoships3 "github.com/scttfrdmn/cargoship/pkg/aws/s3"
)

// ClientManager handles S3 client creation and management
type ClientManager struct {
	client             *s3.Client
	acceleratedClient  *s3.Client // Client with Transfer Acceleration enabled
	standardClient     *s3.Client // Fallback client without acceleration
	pool               *ConnectionPool
	transporter        *cargoships3.Transporter
	config             *Config
	logger             *slog.Logger
	accelerationActive bool // Tracks if acceleration is currently active
}

// NewClientManager creates a new S3 client manager
func NewClientManager(ctx context.Context, bucket string, cfg *Config, logger *slog.Logger) (*ClientManager, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket name cannot be empty")
	}

	if cfg == nil {
		cfg = NewDefaultConfig()
	}

	// Load AWS configuration
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(cfg.Region),
		config.WithRetryMaxAttempts(cfg.MaxRetries),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create standard S3 client without acceleration
	standardClient := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		if cfg.ForcePathStyle {
			o.UsePathStyle = true
		}
		if cfg.UseDualStack {
			o.EndpointOptions.UseDualStackEndpoint = aws.DualStackEndpointStateEnabled
		}
	})

	// Create accelerated S3 client if Transfer Acceleration is enabled
	var acceleratedClient *s3.Client
	var primaryClient *s3.Client
	accelerationActive := false

	if cfg.UseAccelerate {
		acceleratedClient = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			if cfg.Endpoint != "" {
				o.BaseEndpoint = aws.String(cfg.Endpoint)
			}
			if cfg.ForcePathStyle {
				o.UsePathStyle = true
			}
			o.UseAccelerate = true
			if cfg.UseDualStack {
				o.EndpointOptions.UseDualStackEndpoint = aws.DualStackEndpointStateEnabled
			}
		})
		primaryClient = acceleratedClient
		accelerationActive = true
		logger.Info("S3 Transfer Acceleration enabled",
			"bucket", bucket,
			"fallback", "automatic")
	} else {
		primaryClient = standardClient
		logger.Info("S3 Transfer Acceleration disabled",
			"bucket", bucket)
	}

	// Create connection pool
	pool, err := NewConnectionPool(cfg.PoolSize, func() (*s3.Client, error) {
		return s3.NewFromConfig(awsCfg), nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Initialize CargoShip S3 transporter if enabled
	var transporter *cargoships3.Transporter
	if cfg.EnableCargoShipOptimization {
		// Create CargoShip S3 config with optimization settings
		cargoConfig := awsconfig.S3Config{
			Bucket:             bucket,
			StorageClass:       awsconfig.StorageClassIntelligentTiering, // Intelligent tiering
			MultipartThreshold: cfg.MultipartThreshold,                   // Use configured threshold
			MultipartChunkSize: cfg.MultipartChunkSize,                   // Use configured chunk size
			Concurrency:        cfg.MultipartConcurrency,                 // Use configured concurrency
		}

		// Use CargoShip's optimized transporter with BBR/CUBIC algorithms
		// Use accelerated client if available, otherwise use standard
		transporter = cargoships3.NewTransporter(primaryClient, cargoConfig)
		logger.Info("CargoShip S3 optimization enabled",
			"target_throughput", cfg.TargetThroughput,
			"multipart_threshold", cfg.MultipartThreshold,
			"chunk_size", cfg.MultipartChunkSize,
			"concurrency", cfg.MultipartConcurrency)
	}

	return &ClientManager{
		client:             primaryClient,
		acceleratedClient:  acceleratedClient,
		standardClient:     standardClient,
		pool:               pool,
		transporter:        transporter,
		config:             cfg,
		logger:             logger,
		accelerationActive: accelerationActive,
	}, nil
}

// GetClient returns the main S3 client
func (cm *ClientManager) GetClient() *s3.Client {
	return cm.client
}

// GetPooledClient gets a client from the connection pool
func (cm *ClientManager) GetPooledClient() *s3.Client {
	return cm.pool.Get()
}

// ReturnPooledClient returns a client to the connection pool
func (cm *ClientManager) ReturnPooledClient(client *s3.Client) {
	cm.pool.Put(client)
}

// GetTransporter returns the CargoShip transporter if available
func (cm *ClientManager) GetTransporter() *cargoships3.Transporter {
	return cm.transporter
}

// GetPool returns the connection pool for statistics
func (cm *ClientManager) GetPool() *ConnectionPool {
	return cm.pool
}

// IsCargoShipEnabled returns whether CargoShip optimization is enabled
func (cm *ClientManager) IsCargoShipEnabled() bool {
	return cm.transporter != nil
}

// HealthCheck verifies the client connection
func (cm *ClientManager) HealthCheck(ctx context.Context, bucket string) error {
	client := cm.GetPooledClient()
	defer cm.ReturnPooledClient(client)

	// Try to head the bucket
	input := &s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	}

	_, err := client.HeadBucket(ctx, input)
	if err != nil {
		return fmt.Errorf("S3 health check failed: %w", err)
	}

	return nil
}

// Close closes all client resources
func (cm *ClientManager) Close() error {
	// CargoShip transporter doesn't require explicit cleanup
	return cm.pool.Close()
}

// GetStats returns connection pool statistics
func (cm *ClientManager) GetStats() PoolStats {
	return cm.pool.Stats()
}

// GetAcceleratedClient returns the accelerated client if acceleration is active
func (cm *ClientManager) GetAcceleratedClient() *s3.Client {
	if cm.accelerationActive && cm.acceleratedClient != nil {
		return cm.acceleratedClient
	}
	return nil
}

// GetStandardClient returns the standard (non-accelerated) client
func (cm *ClientManager) GetStandardClient() *s3.Client {
	return cm.standardClient
}

// IsAccelerationActive returns whether Transfer Acceleration is currently active
func (cm *ClientManager) IsAccelerationActive() bool {
	return cm.accelerationActive
}

// DisableAcceleration temporarily disables Transfer Acceleration and falls back to standard client
func (cm *ClientManager) DisableAcceleration(reason string) {
	if cm.accelerationActive {
		cm.logger.Warn("Disabling S3 Transfer Acceleration",
			"reason", reason,
			"fallback_to", "standard_endpoint")
		cm.accelerationActive = false
		cm.client = cm.standardClient
	}
}

// EnableAcceleration re-enables Transfer Acceleration if configured
func (cm *ClientManager) EnableAcceleration() {
	if cm.config.UseAccelerate && cm.acceleratedClient != nil && !cm.accelerationActive {
		cm.logger.Info("Re-enabling S3 Transfer Acceleration")
		cm.accelerationActive = true
		cm.client = cm.acceleratedClient
	}
}
