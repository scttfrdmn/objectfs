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
	client      *s3.Client
	pool        *ConnectionPool
	transporter *cargoships3.Transporter
	config      *Config
	logger      *slog.Logger
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

	// Create S3 client with custom options
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		if cfg.ForcePathStyle {
			o.UsePathStyle = true
		}
		if cfg.UseAccelerate {
			o.UseAccelerate = true
		}
		if cfg.UseDualStack {
			o.EndpointOptions.UseDualStackEndpoint = aws.DualStackEndpointStateEnabled
		}
	})

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
			MultipartThreshold: 32 * 1024 * 1024,                         // 32MB threshold
			MultipartChunkSize: 16 * 1024 * 1024,                         // 16MB chunks for optimization
			Concurrency:        cfg.PoolSize,                             // Match pool size
		}

		// Use CargoShip's optimized transporter with BBR/CUBIC algorithms
		transporter = cargoships3.NewTransporter(client, cargoConfig)
		logger.Info("CargoShip S3 optimization enabled",
			"target_throughput", cfg.TargetThroughput,
			"chunk_size", "16MB",
			"concurrency", cfg.PoolSize)
	}

	return &ClientManager{
		client:      client,
		pool:        pool,
		transporter: transporter,
		config:      cfg,
		logger:      logger,
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
