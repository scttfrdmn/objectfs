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
	awsconfig "github.com/scttfrdmn/cargoship/pkg/aws/config"
	cargoships3 "github.com/scttfrdmn/cargoship/pkg/aws/s3"

	"github.com/objectfs/objectfs/internal/network"
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

	// transport is retained so Close can release the sockets it is holding idle.
	//
	// Without this the manager had no reference to it: the transport went into an http.Client, the
	// client into the AWS SDK config, and Close drained the ConnectionPool — which pools *SDK
	// clients*, not TCP connections. The actual sockets live here, up to MaxIdleConns of them per
	// manager, and nothing released them. Measured against the emulator: 40 create-and-Close cycles
	// left 80 sockets open, and a fuzz run doing it in a loop exhausted the ephemeral port range and
	// failed with "can't assign requested address".
	transport *http.Transport
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
	//
	// ConnectTimeout and RequestTimeout are applied here, and until v0.10.1 they were applied
	// nowhere: both were defaulted by NewDefaultConfig, documented in the config schema, and read
	// only to be copied into an error-context map for display. A mount inherited network.NewDialer's
	// bare *net.Dialer, which has no timeout at all, so a connect to an unroutable address hung until
	// the kernel gave up — minutes, with a FUSE request blocked behind it.
	//
	// RequestTimeout becomes ResponseHeaderTimeout, not a whole-response deadline. The distinction is
	// the difference between a working filesystem and a broken one: a ranged GET of a large object
	// legitimately spends minutes streaming its body, and http.Client.Timeout or a context deadline
	// around the call would abort it as though S3 had stalled. ResponseHeaderTimeout bounds the part
	// that can actually hang — the wait for S3 to begin answering — and leaves the transfer alone.
	algo := network.Algorithm(cfg.CongestionAlgorithm)
	dialer := network.NewDialer(algo)
	dialer.Timeout = cfg.ConnectTimeout
	mon := network.NewMonitor(algo)
	transport := &http.Transport{
		DialContext:           mon.WrapDialContext(dialer.DialContext),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   cfg.PoolSize,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: cfg.RequestTimeout,
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
		// The storage class comes from the configured tier, not from a constant.
		//
		// cargoship's Transporter.optimizeStorageClass falls back to its config's StorageClass for an
		// Archive with no AccessPattern and no RetentionDays, which is every archive ObjectFS builds —
		// so a hardcoded value here is the class every object is stored under, whatever storage_tier
		// says. It read StorageClassIntelligentTiering, and CargoShip is on in the shipped defaults,
		// so `storage_tier: STANDARD_IA` silently stored INTELLIGENT_TIERING: no error, no log, and a
		// different bill from the one the config describes. Found by asserting the stored class at the
		// endpoint rather than the value passed in.
		cargoConfig := awsconfig.S3Config{
			Bucket:             bucket,
			StorageClass:       ConvertTierToCargoShipStorageClass(cfg.StorageTier),
			MultipartThreshold: cfg.MultipartThreshold,   // Use configured threshold
			MultipartChunkSize: cfg.MultipartChunkSize,   // Use configured chunk size
			Concurrency:        cfg.MultipartConcurrency, // Use configured concurrency
		}

		// The KMS key is passed through so objects that *do* go via CargoShip carry the same encryption
		// as the direct path. It is set only for sse-kms, because that is the only mode CargoShip can
		// express — its transporter hardcodes the algorithm to aws:kms and has no bucket-key field — and
		// PutObject diverts around the transporter for anything else. See cargoShipCanEncrypt: setting
		// the key here for a mode CargoShip cannot honor is what would let an object be stored under an
		// encryption nobody configured.
		if cfg.Encryption.Mode == EncryptionModeKMS {
			cargoConfig.KMSKeyID = cfg.Encryption.KMSKeyID
		}

		// Use CargoShip's optimized transporter with BBR/CUBIC algorithms
		// Use accelerated client if available, otherwise use standard
		transporter = cargoships3.NewTransporter(primaryClient, cargoConfig)
		logger.Info("CargoShip S3 optimization enabled",
			"multipart_threshold", cfg.MultipartThreshold,
			"chunk_size", cfg.MultipartChunkSize,
			"concurrency", cfg.MultipartConcurrency,
			"storage_class", cargoConfig.StorageClass)
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
		transport:          transport,
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
// Close releases the manager's pooled SDK clients and the TCP sockets its transport holds idle.
//
// Both halves are needed, and only the first used to happen. The ConnectionPool holds *s3.Client
// values, which are cheap structs sharing one transport; draining it frees no sockets at all. The
// sockets are the transport's idle connections — up to MaxIdleConns of them, kept for
// IdleConnTimeout, which is 90 seconds. A process that builds and closes backends in a loop
// therefore accumulated file descriptors until it ran out: measured at 2 leaked sockets per cycle
// against a local endpoint, and reported by a fuzz run as "can't assign requested address" after the
// ephemeral port range filled.
//
// CloseIdleConnections rather than anything more forceful: it closes connections not currently in
// use and leaves an in-flight request alone to finish. A caller closing a backend while still using
// it has a bug this cannot fix, and cutting the request short would turn it into a confusing I/O
// error instead.
func (cm *ClientManager) Close() error {
	// CargoShip transporter doesn't require explicit cleanup
	err := cm.pool.Close()

	if cm.transport != nil {
		cm.transport.CloseIdleConnections()
	}

	return err
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
