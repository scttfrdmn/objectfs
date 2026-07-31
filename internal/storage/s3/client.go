package s3

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/objectfs/objectfs/internal/network"
	awsconfig "github.com/scttfrdmn/cargoship/pkg/aws/config"
	cargoships3 "github.com/scttfrdmn/cargoship/pkg/aws/s3"
)

// clientOptions returns the s3.Options mutator that applies the endpoint and addressing settings
// from cfg. Every S3 client ObjectFS builds must go through this, including the connection pool's
// factory.
//
// It exists because the pool's factory previously called s3.NewFromConfig with no options at all
// while the direct clients applied Endpoint and ForcePathStyle. HeadObject, DeleteObject,
// ListObjects, and the health check draw from the pool, so those four operations addressed real AWS
// S3 while PutObject and GetObject addressed the configured endpoint — making a MinIO, Ceph, or
// emulator deployment fail in a way that looks like a credentials problem. One mutator, used
// everywhere, is what stops the two from drifting apart again.
func clientOptions(cfg *Config) func(*s3.Options) {
	return func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		if cfg.ForcePathStyle {
			o.UsePathStyle = true
		}
		if cfg.UseDualStack {
			o.EndpointOptions.UseDualStackEndpoint = aws.DualStackEndpointStateEnabled
		}
	}
}

// ClientManager handles S3 client creation and management
type ClientManager struct {
	client             *s3.Client
	acceleratedClient  *s3.Client // Client with Transfer Acceleration enabled
	standardClient     *s3.Client // Fallback client without acceleration
	pool               *ConnectionPool
	transporter        *cargoships3.Transporter
	config             *Config
	logger             *slog.Logger
	accelerationActive bool             // Tracks if acceleration is currently active
	networkMonitor     *network.Monitor // Tracks bytes/connections for this client
}

// NewClientManager creates a new S3 client manager
func NewClientManager(ctx context.Context, bucket string, cfg *Config, logger *slog.Logger) (*ClientManager, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket name cannot be empty")
	}

	if cfg == nil {
		cfg = NewDefaultConfig()
	}

	// Build a congestion-aware HTTP transport for the AWS SDK.
	algo := network.Algorithm(cfg.CongestionAlgorithm)
	dialer := network.NewDialer(algo)
	mon := network.NewMonitor(algo)
	transport := &http.Transport{
		DialContext:           mon.WrapDialContext(dialer.DialContext),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   cfg.PoolSize,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	httpClient := &http.Client{Transport: transport}

	// Load AWS configuration
	loadOpts := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
		config.WithRetryMaxAttempts(cfg.MaxRetries),
		config.WithHTTPClient(httpClient),
	}

	// Static credentials, when configured. configs/example.yaml has documented
	// access_key_id/secret_access_key since the first release and nothing read them, so a
	// deployment that set them silently fell through to the default chain — and failed with
	// "no credentials" or, worse, picked up an unrelated ambient profile. Leaving them empty
	// keeps the default chain (environment, shared config, IMDS), which is the right default
	// for EC2 and for anyone using AWS_PROFILE.
	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken),
		))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create standard S3 client without acceleration
	standardClient := s3.NewFromConfig(awsCfg, clientOptions(cfg))

	// Create accelerated S3 client if Transfer Acceleration is enabled
	var acceleratedClient *s3.Client
	var primaryClient *s3.Client
	accelerationActive := false

	if cfg.UseAccelerate {
		acceleratedClient = s3.NewFromConfig(awsCfg, clientOptions(cfg), func(o *s3.Options) {
			o.UseAccelerate = true
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

	// Create connection pool. The factory must apply the same options as the direct clients above:
	// HeadObject, DeleteObject, ListObjects, and HealthCheck all draw from this pool, so a factory
	// that skips the endpoint sends them to real AWS S3 while the rest of the backend talks to the
	// configured endpoint.
	pool, err := NewConnectionPool(cfg.PoolSize, func() (*s3.Client, error) {
		return s3.NewFromConfig(awsCfg, clientOptions(cfg)), nil
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
		networkMonitor:     mon,
	}, nil
}

// GetClient returns the main S3 client
func (cm *ClientManager) GetClient() *s3.Client {
	return cm.client
}

// GetPooledClient gets a client from the connection pool.
//
// It returns an error rather than a nil client: callers dereference the result immediately, and a nil
// here previously panicked and unmounted the filesystem once the pool was saturated.
func (cm *ClientManager) GetPooledClient() (*s3.Client, error) {
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
	client, err := cm.GetPooledClient()
	if err != nil {
		return fmt.Errorf("S3 health check failed: %w", err)
	}
	defer cm.ReturnPooledClient(client)

	// Try to head the bucket
	input := &s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	}

	if _, err := client.HeadBucket(ctx, input); err != nil {
		return fmt.Errorf("S3 health check failed: %w", err)
	}

	return nil
}

// GetNetworkMonitor returns the network monitor that tracks bytes and
// connection counts for all S3 connections made through this client.
func (cm *ClientManager) GetNetworkMonitor() *network.Monitor {
	return cm.networkMonitor
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
