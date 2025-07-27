package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	cargoships3 "github.com/scttfrdmn/cargoship/pkg/aws/s3"
	awsconfig "github.com/scttfrdmn/cargoship/pkg/aws/config"

	"github.com/objectfs/objectfs/pkg/types"
)

// Backend implements the S3 storage backend with CargoShip optimization
type Backend struct {
	client     *s3.Client
	bucket     string
	region     string
	endpoint   string
	pathStyle  bool
	
	// Connection pool
	pool       *ConnectionPool
	
	// Configuration
	config     *Config
	
	// CargoShip S3 Optimization (4.6x performance)
	transporter *cargoships3.Transporter
	logger      *slog.Logger
	
	// Metrics
	mu         sync.RWMutex
	metrics    BackendMetrics
}

// Config represents S3 backend configuration
type Config struct {
	Region          string `yaml:"region"`
	Endpoint        string `yaml:"endpoint"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	SessionToken    string `yaml:"session_token"`
	ForcePathStyle  bool   `yaml:"force_path_style"`
	
	// Performance settings
	MaxRetries      int           `yaml:"max_retries"`
	ConnectTimeout  time.Duration `yaml:"connect_timeout"`
	RequestTimeout  time.Duration `yaml:"request_timeout"`
	PoolSize        int           `yaml:"pool_size"`
	
	// Advanced settings
	UseAccelerate   bool   `yaml:"use_accelerate"`
	UseDualStack    bool   `yaml:"use_dual_stack"`
	DisableSSL      bool   `yaml:"disable_ssl"`
	
	// CargoShip optimization settings
	EnableCargoShipOptimization bool `yaml:"enable_cargoship_optimization"`
	TargetThroughput           float64 `yaml:"target_throughput"`          // MB/s
	OptimizationLevel         string   `yaml:"optimization_level"`        // "standard", "aggressive"
}

// BackendMetrics tracks S3 backend performance metrics
type BackendMetrics struct {
	Requests        int64         `json:"requests"`
	Errors          int64         `json:"errors"`
	BytesUploaded   int64         `json:"bytes_uploaded"`
	BytesDownloaded int64         `json:"bytes_downloaded"`
	AverageLatency  time.Duration `json:"average_latency"`
	LastError       string        `json:"last_error"`
	LastErrorTime   time.Time     `json:"last_error_time"`
}

// NewBackend creates a new S3 backend instance
func NewBackend(ctx context.Context, bucket string, cfg *Config) (*Backend, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket name cannot be empty")
	}

	if cfg == nil {
		cfg = &Config{
			MaxRetries:     3,
			ConnectTimeout: 10 * time.Second,
			RequestTimeout: 30 * time.Second,
			PoolSize:       8,
			EnableCargoShipOptimization: true,
			TargetThroughput: 800.0, // 800 MB/s target for ObjectFS
			OptimizationLevel: "standard",
		}
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
			o.UseDualstack = true
		}
	})

	// Create connection pool
	pool, err := NewConnectionPool(cfg.PoolSize, func() (*s3.Client, error) {
		return s3.NewFromConfig(awsCfg), nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Initialize logger
	logger := slog.Default().With("component", "s3-backend", "bucket", bucket)
	
	// Initialize CargoShip S3 transporter if enabled
	var transporter *cargoships3.Transporter
	if cfg.EnableCargoShipOptimization {
		// Create CargoShip S3 config with optimization settings
		cargoConfig := awsconfig.S3Config{
			Bucket:             bucket,
			StorageClass:       awsconfig.StorageClassIntelligentTiering, // Intelligent tiering
			MultipartThreshold: 32 * 1024 * 1024,    // 32MB threshold
			MultipartChunkSize: 16 * 1024 * 1024,    // 16MB chunks for optimization
			Concurrency:        cfg.PoolSize,         // Match pool size
		}
		
		// Use CargoShip's optimized transporter with BBR/CUBIC algorithms
		transporter = cargoships3.NewTransporter(client, cargoConfig)
		logger.Info("CargoShip S3 optimization enabled", "target_throughput", cfg.TargetThroughput, "chunk_size", "16MB", "concurrency", cfg.PoolSize)
	}
	
	backend := &Backend{
		client:    client,
		bucket:    bucket,
		region:    cfg.Region,
		endpoint:  cfg.Endpoint,
		pathStyle: cfg.ForcePathStyle,
		pool:      pool,
		config:    cfg,
		transporter: transporter,
		logger:    logger,
		metrics:   BackendMetrics{},
	}

	// Test connection
	if err := backend.HealthCheck(ctx); err != nil {
		return nil, fmt.Errorf("S3 backend health check failed: %w", err)
	}

	return backend, nil
}

// GetObject retrieves an object or part of an object from S3 with CargoShip optimization
func (b *Backend) GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error) {
	start := time.Now()
	defer func() {
		b.recordMetrics(time.Since(start), false)
	}()

	// Build range header if needed
	var rangeHeader *string
	if offset > 0 || size > 0 {
		if size > 0 {
			rangeHeader = aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+size-1))
		} else {
			rangeHeader = aws.String(fmt.Sprintf("bytes=%d-", offset))
		}
	}

	input := &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
		Range:  rangeHeader,
	}

	// Use standard S3 client for reads (CargoShip optimizes uploads)
	client := b.pool.Get()
	defer b.pool.Put(client)
	
	result, err := client.GetObject(ctx, input)
	
	if err != nil {
		b.recordError(err)
		return nil, b.translateError(err, "GetObject", key)
	}
	defer result.Body.Close()

	data, err := io.ReadAll(result.Body)
	if err != nil {
		b.recordError(err)
		return nil, fmt.Errorf("failed to read object body: %w", err)
	}

	b.mu.Lock()
	b.metrics.BytesDownloaded += int64(len(data))
	b.mu.Unlock()

	return data, nil
}

// PutObject stores an object in S3 with CargoShip optimization
func (b *Backend) PutObject(ctx context.Context, key string, data []byte) error {
	start := time.Now()
	defer func() {
		b.recordMetrics(time.Since(start), false)
	}()

	input := &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(b.detectContentType(key)),
	}

	// Use CargoShip transporter if available for optimized uploads (4.6x performance)
	var err error
	
	if b.transporter != nil {
		// Use CargoShip's optimized upload with BBR/CUBIC algorithms
		archive := cargoships3.Archive{
			Key:    key,
			Reader: bytes.NewReader(data),
			Size:   int64(len(data)),
			StorageClass: awsconfig.StorageClassStandard, // Use standard for filesystem data
			Metadata: map[string]string{
				"objectfs-upload": "true",
				"content-type":    b.detectContentType(key),
			},
		}
		
		result, uploadErr := b.transporter.Upload(ctx, archive)
		if uploadErr == nil {
			b.logger.Debug("CargoShip optimized upload completed", 
				"key", key, 
				"size", len(data), 
				"throughput", result.Throughput,
				"duration", result.Duration)
			return nil
		}
		
		b.logger.Warn("CargoShip optimization failed, falling back to standard S3", "key", key, "error", uploadErr)
	}
	
	// Fallback to standard S3 client
	client := b.pool.Get()
	defer b.pool.Put(client)
	_, err = client.PutObject(ctx, input)
	
	if err != nil {
		b.recordError(err)
		return b.translateError(err, "PutObject", key)
	}

	b.mu.Lock()
	b.metrics.BytesUploaded += int64(len(data))
	b.mu.Unlock()

	return nil
}

// DeleteObject removes an object from S3
func (b *Backend) DeleteObject(ctx context.Context, key string) error {
	start := time.Now()
	defer func() {
		b.recordMetrics(time.Since(start), false)
	}()

	client := b.pool.Get()
	defer b.pool.Put(client)

	input := &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}

	_, err := client.DeleteObject(ctx, input)
	if err != nil {
		b.recordError(err)
		return b.translateError(err, "DeleteObject", key)
	}

	return nil
}

// HeadObject retrieves metadata about an object
func (b *Backend) HeadObject(ctx context.Context, key string) (*types.ObjectInfo, error) {
	start := time.Now()
	defer func() {
		b.recordMetrics(time.Since(start), false)
	}()

	client := b.pool.Get()
	defer b.pool.Put(client)

	input := &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}

	result, err := client.HeadObject(ctx, input)
	if err != nil {
		b.recordError(err)
		return nil, b.translateError(err, "HeadObject", key)
	}

	info := &types.ObjectInfo{
		Key:          key,
		Size:         aws.ToInt64(result.ContentLength),
		LastModified: aws.ToTime(result.LastModified),
		ETag:         aws.ToString(result.ETag),
		ContentType:  aws.ToString(result.ContentType),
		Metadata:     make(map[string]string),
	}

	// Copy metadata
	for k, v := range result.Metadata {
		info.Metadata[k] = v
	}

	return info, nil
}

// GetObjects retrieves multiple objects in batch with CargoShip optimization
func (b *Backend) GetObjects(ctx context.Context, keys []string) (map[string][]byte, error) {
	if len(keys) == 0 {
		return make(map[string][]byte), nil
	}

	// Use parallel individual requests (CargoShip focuses on upload optimization)
	results := make(map[string][]byte, len(keys))
	
	type result struct {
		key  string
		data []byte
		err  error
	}

	resultCh := make(chan result, len(keys))
	semaphore := make(chan struct{}, b.config.PoolSize)

	for _, key := range keys {
		go func(k string) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			data, err := b.GetObject(ctx, k, 0, 0)
			resultCh <- result{key: k, data: data, err: err}
		}(key)
	}

	var firstError error
	for i := 0; i < len(keys); i++ {
		res := <-resultCh
		if res.err != nil {
			if firstError == nil {
				firstError = res.err
			}
			continue
		}
		results[res.key] = res.data
	}

	if firstError != nil && len(results) == 0 {
		return nil, firstError
	}

	return results, nil
}

// PutObjects stores multiple objects in batch with CargoShip optimization
func (b *Backend) PutObjects(ctx context.Context, objects map[string][]byte) error {
	if len(objects) == 0 {
		return nil
	}

	// Use parallel individual requests (each will use CargoShip if available)
	type result struct {
		key string
		err error
	}

	resultCh := make(chan result, len(objects))
	semaphore := make(chan struct{}, b.config.PoolSize)

	for key, data := range objects {
		go func(k string, d []byte) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			err := b.PutObject(ctx, k, d)
			resultCh <- result{key: k, err: err}
		}(key, data)
	}

	var errors []string
	for i := 0; i < len(objects); i++ {
		res := <-resultCh
		if res.err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", res.key, res.err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("batch put failed for %d objects: %s", len(errors), strings.Join(errors, "; "))
	}

	return nil
}

// ListObjects lists objects in the bucket with the given prefix
func (b *Backend) ListObjects(ctx context.Context, prefix string, limit int) ([]types.ObjectInfo, error) {
	start := time.Now()
	defer func() {
		b.recordMetrics(time.Since(start), false)
	}()

	client := b.pool.Get()
	defer b.pool.Put(client)

	var maxKeys *int32
	if limit > 0 {
		// Safe conversion to prevent overflow
		if limit > 0x7FFFFFFF {
			maxKeys = aws.Int32(0x7FFFFFFF)
		} else {
			maxKeys = aws.Int32(int32(limit))
		}
	}

	input := &s3.ListObjectsV2Input{
		Bucket:  aws.String(b.bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: maxKeys,
	}

	result, err := client.ListObjectsV2(ctx, input)
	if err != nil {
		b.recordError(err)
		return nil, b.translateError(err, "ListObjects", prefix)
	}

	objects := make([]types.ObjectInfo, 0, len(result.Contents))
	for _, obj := range result.Contents {
		info := types.ObjectInfo{
			Key:          aws.ToString(obj.Key),
			Size:         aws.ToInt64(obj.Size),
			LastModified: aws.ToTime(obj.LastModified),
			ETag:         aws.ToString(obj.ETag),
			Metadata:     make(map[string]string),
		}
		objects = append(objects, info)
	}

	return objects, nil
}

// HealthCheck verifies the backend connection
func (b *Backend) HealthCheck(ctx context.Context) error {
	client := b.pool.Get()
	defer b.pool.Put(client)

	// Try to head the bucket
	input := &s3.HeadBucketInput{
		Bucket: aws.String(b.bucket),
	}

	_, err := client.HeadBucket(ctx, input)
	if err != nil {
		return fmt.Errorf("S3 health check failed: %w", err)
	}

	return nil
}

// GetMetrics returns current backend metrics
func (b *Backend) GetMetrics() BackendMetrics {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.metrics
}

// Close closes the backend and releases resources
func (b *Backend) Close() error {
	// CargoShip transporter doesn't require explicit cleanup
	
	return b.pool.Close()
}

// Helper methods

func (b *Backend) recordMetrics(duration time.Duration, isError bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	b.metrics.Requests++
	if isError {
		b.metrics.Errors++
	}
	
	// Calculate rolling average latency
	if b.metrics.Requests == 1 {
		b.metrics.AverageLatency = duration
	} else {
		b.metrics.AverageLatency = time.Duration(
			(int64(b.metrics.AverageLatency)*9 + int64(duration)) / 10,
		)
	}
}

func (b *Backend) recordError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	b.metrics.LastError = err.Error()
	b.metrics.LastErrorTime = time.Now()
}

func (b *Backend) translateError(err error, operation, key string) error {
	switch {
	case isErrorType[*s3types.NoSuchKey](err):
		return fmt.Errorf("object not found: %s", key)
	case isErrorType[*s3types.NoSuchBucket](err):
		return fmt.Errorf("bucket not found: %s", b.bucket)
	default:
		return fmt.Errorf("%s failed for %s: %w", operation, key, err)
	}
}

func (b *Backend) detectContentType(key string) string {
	switch {
	case strings.HasSuffix(key, ".json"):
		return "application/json"
	case strings.HasSuffix(key, ".xml"):
		return "application/xml"
	case strings.HasSuffix(key, ".html"):
		return "text/html"
	case strings.HasSuffix(key, ".txt"):
		return "text/plain"
	case strings.HasSuffix(key, ".jpg"), strings.HasSuffix(key, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(key, ".png"):
		return "image/png"
	case strings.HasSuffix(key, ".pdf"):
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

// isErrorType checks if an error is of a specific type
func isErrorType[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}