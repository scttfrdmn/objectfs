package s3

import (
	"bytes"
	"context"
	stderr "errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	cargoships3 "github.com/scttfrdmn/cargoship/pkg/aws/s3"

	"github.com/objectfs/objectfs/internal/circuit"
	"github.com/objectfs/objectfs/pkg/errors"
	"github.com/objectfs/objectfs/pkg/health"
	"github.com/objectfs/objectfs/pkg/retry"
	"github.com/objectfs/objectfs/pkg/types"
)

// Backend implements the S3 storage backend with CargoShip optimization
type Backend struct {
	bucket string

	// Core components
	clientManager    *ClientManager
	metricsCollector *MetricsCollector
	logger           *slog.Logger

	// Configuration
	config *Config

	// Storage Tier Management
	currentTier    string
	tierInfo       StorageTierInfo
	tierValidator  *TierValidator
	costOptimizer  *CostOptimizer
	pricingManager *PricingManager

	// Circuit breaker for resilience
	circuitManager *circuit.Manager

	// Retry logic for error recovery
	retryer *retry.Retryer

	// Health tracking for graceful degradation
	healthTracker *health.Tracker

	// Multipart upload management
	multipartManager *MultipartStateManager
}

// NewBackend creates a new S3 backend instance
func NewBackend(ctx context.Context, bucket string, cfg *Config) (*Backend, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket name cannot be empty")
	}

	if cfg == nil {
		cfg = NewDefaultConfig()
	}

	// Set default storage tier if not specified
	if cfg.StorageTier == "" {
		cfg.StorageTier = TierStandard
	}

	// Initialize logger
	logger := slog.Default().With("component", "s3-backend", "bucket", bucket)

	// Initialize client manager
	clientManager, err := NewClientManager(ctx, bucket, cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create client manager: %w", err)
	}

	// Initialize metrics collector
	metricsCollector := NewMetricsCollector()
	metricsCollector.SetAccelerationEnabled(cfg.UseAccelerate)

	// Initialize tier validator
	tierValidator := NewTierValidator(cfg.StorageTier, cfg.TierConstraints, logger)
	tierInfo := tierValidator.GetTierInfo()

	backend := &Backend{
		bucket:           bucket,
		clientManager:    clientManager,
		metricsCollector: metricsCollector,
		logger:           logger,
		config:           cfg,
		currentTier:      cfg.StorageTier,
		tierInfo:         tierInfo,
		tierValidator:    tierValidator,
	}

	// Initialize pricing manager
	backend.pricingManager = NewPricingManager(cfg.PricingConfig, logger)

	// Initialize cost optimizer
	backend.costOptimizer = NewCostOptimizer(backend, cfg.CostOptimization, logger)

	// Initialize multipart upload manager
	backend.multipartManager = NewMultipartStateManager()

	// Initialize circuit breaker manager
	circuitConfig := circuit.Config{
		MaxRequests: 10,
		Interval:    60 * time.Second,
		Timeout:     30 * time.Second,
		OnStateChange: func(name string, from circuit.State, to circuit.State) {
			logger.Info("Circuit breaker state changed",
				"breaker", name,
				"from", from.String(),
				"to", to.String())
		},
	}
	backend.circuitManager = circuit.NewManager(circuitConfig)

	// Initialize retryer with logging callback
	retryConfig := cfg.RetryConfig
	retryConfig.OnRetry = func(attempt int, err error, delay time.Duration) {
		logger.Warn("Retrying S3 operation",
			"attempt", attempt,
			"delay", delay,
			"error", err)
	}
	backend.retryer = retry.New(retryConfig)

	// Initialize health tracker for graceful degradation
	healthConfig := health.DefaultConfig()
	backend.healthTracker = health.NewTracker(healthConfig)
	backend.healthTracker.RegisterComponent("s3-reads")
	backend.healthTracker.RegisterComponent("s3-writes")
	backend.healthTracker.RegisterComponent("s3-deletes")
	backend.healthTracker.RegisterComponent("s3-lists")

	// Add health state change callbacks
	backend.healthTracker.AddStateChangeCallback(health.StateReadOnly, func(component string, oldState, newState health.HealthState, err error) {
		logger.Warn("S3 component transitioned to read-only mode",
			"component", component,
			"old_state", oldState.String(),
			"new_state", newState.String(),
			"error", err)
	})

	backend.healthTracker.AddStateChangeCallback(health.StateUnavailable, func(component string, oldState, newState health.HealthState, err error) {
		logger.Error("S3 component became unavailable",
			"component", component,
			"old_state", oldState.String(),
			"new_state", newState.String(),
			"error", err)
	})

	backend.healthTracker.AddStateChangeCallback(health.StateHealthy, func(component string, oldState, newState health.HealthState, err error) {
		if oldState != health.StateHealthy {
			logger.Info("S3 component recovered to healthy state",
				"component", component,
				"old_state", oldState.String())
		}
	})

	// Log tier configuration
	logger.Info("S3 storage tier configured",
		"tier", cfg.StorageTier,
		"tier_name", tierInfo.Name,
		"min_object_size", tierInfo.MinObjectSize,
		"deletion_embargo", tierInfo.DeletionEmbargo,
		"retrieval_cost", tierInfo.RetrievalCost,
		"cost_per_gb_month", tierInfo.CostPerGBMonth)

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
		b.metricsCollector.RecordMetrics(time.Since(start), false)
	}()

	// Check if reads are available in current health state
	if !b.healthTracker.CanRead("s3-reads") {
		state := b.healthTracker.GetState("s3-reads")
		return nil, errors.NewError(errors.ErrCodeServiceUnavailable, "S3 read operations are unavailable").
			WithComponent("s3-backend").
			WithOperation("GetObject").
			WithContext("health_state", state.String()).
			WithContext("bucket", b.bucket).
			WithContext("key", key)
	}

	breaker := b.circuitManager.GetBreaker("s3-get")
	var data []byte

	// Wrap with retry logic
	err := b.retryer.DoWithContext(ctx, func(retryCtx context.Context) error {
		return breaker.ExecuteWithContext(retryCtx, func(ctx context.Context) error {
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

			// Use acceleration fallback pattern for reads
			err := b.executeWithAccelerationFallback(ctx, "GetObject", func(client *s3.Client) error {
				result, err := client.GetObject(ctx, input)
				if err != nil {
					b.metricsCollector.RecordError(err)
					translatedErr := b.translateError(err, "GetObject", key)
					b.healthTracker.RecordError("s3-reads", translatedErr)
					return translatedErr
				}
				defer func() { _ = result.Body.Close() }()

				data, err = io.ReadAll(result.Body)
				if err != nil {
					b.metricsCollector.RecordError(err)
					readErr := fmt.Errorf("failed to read object body: %w", err)
					b.healthTracker.RecordError("s3-reads", readErr)
					return readErr
				}

				b.metricsCollector.RecordBytesDownloaded(int64(len(data)))
				b.healthTracker.RecordSuccess("s3-reads")
				return nil
			})

			return err
		})
	})

	if err != nil {
		return nil, err
	}

	// Record access pattern for cost optimization
	b.costOptimizer.RecordAccess(key, int64(len(data)))

	return data, nil
}

// PutObject stores an object in S3 with CargoShip optimization
func (b *Backend) PutObject(ctx context.Context, key string, data []byte) error {
	start := time.Now()
	defer func() {
		b.metricsCollector.RecordMetrics(time.Since(start), false)
	}()

	// Check if writes are available in current health state
	if !b.healthTracker.CanWrite("s3-writes") {
		state := b.healthTracker.GetState("s3-writes")
		return errors.NewError(errors.ErrCodeServiceUnavailable, "S3 write operations are unavailable").
			WithComponent("s3-backend").
			WithOperation("PutObject").
			WithContext("health_state", state.String()).
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithDetail("suggestion", "System is in read-only mode. Writes will be available once service recovers.")
	}

	// Validate write operation against tier constraints
	if err := b.tierValidator.ValidateWrite(key, int64(len(data))); err != nil {
		b.metricsCollector.RecordError(err)
		return fmt.Errorf("tier validation failed: %w", err)
	}

	// Handle Standard tier overhead for cost optimization
	effectiveTier := b.currentTier
	if b.config.CostOptimization.MonitorAccessPatterns {
		effectiveTier = b.costOptimizer.HandleStandardTierOverhead(key, int64(len(data)))
		if effectiveTier != b.currentTier {
			b.logger.Debug("Using Standard tier to avoid IA overhead",
				"object", key,
				"size", len(data),
				"configured_tier", b.currentTier,
				"effective_tier", effectiveTier)
		}
	}

	breaker := b.circuitManager.GetBreaker("s3-put")

	err := breaker.ExecuteWithContext(ctx, func(ctx context.Context) error {
		// Check if we should use multipart upload based on size threshold
		dataSize := int64(len(data))
		if dataSize >= b.config.MultipartThreshold {
			b.logger.Debug("Using multipart upload for large object",
				"key", key,
				"size", dataSize,
				"threshold", b.config.MultipartThreshold)
			return b.putObjectMultipart(ctx, key, data, effectiveTier)
		}

		// Get storage class for effective tier
		storageClass := ConvertTierToStorageClass(effectiveTier)

		input := &s3.PutObjectInput{
			Bucket:        aws.String(b.bucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(data),
			ContentLength: aws.Int64(int64(len(data))),
			ContentType:   aws.String(b.detectContentType(key)),
			StorageClass:  storageClass,
		}

		// Use CargoShip transporter if available for optimized uploads (4.6x performance)
		if transporter := b.clientManager.GetTransporter(); transporter != nil {
			// Use CargoShip's optimized upload with BBR/CUBIC algorithms
			cargoStorageClass := ConvertTierToCargoShipStorageClass(effectiveTier)
			archive := cargoships3.Archive{
				Key:          key,
				Reader:       bytes.NewReader(data),
				Size:         int64(len(data)),
				StorageClass: cargoStorageClass,
				Metadata: map[string]string{
					"objectfs-upload": "true",
					"content-type":    b.detectContentType(key),
					"storage-tier":    effectiveTier,
					"configured-tier": b.currentTier,
				},
			}

			result, uploadErr := transporter.Upload(ctx, archive)
			if uploadErr == nil {
				b.logger.Debug("CargoShip optimized upload completed",
					"key", key,
					"size", len(data),
					"throughput", result.Throughput,
					"duration", result.Duration)
				b.metricsCollector.RecordBytesUploaded(int64(len(data)))
				b.healthTracker.RecordSuccess("s3-writes")
				return nil
			}

			b.logger.Warn("CargoShip optimization failed, falling back to standard S3", "key", key, "error", uploadErr)
		}

		// Fallback to standard S3 client with acceleration support
		return b.executeWithAccelerationFallback(ctx, "PutObject", func(client *s3.Client) error {
			_, err := client.PutObject(ctx, input)
			if err != nil {
				b.metricsCollector.RecordError(err)
				translatedErr := b.translateError(err, "PutObject", key)
				b.healthTracker.RecordError("s3-writes", translatedErr)
				return translatedErr
			}

			b.metricsCollector.RecordBytesUploaded(int64(len(data)))
			b.healthTracker.RecordSuccess("s3-writes")
			return nil
		})
	})

	return err
}

// DeleteObject removes an object from S3
func (b *Backend) DeleteObject(ctx context.Context, key string) error {
	start := time.Now()
	defer func() {
		b.metricsCollector.RecordMetrics(time.Since(start), false)
	}()

	// Get object metadata to check creation time for tier validation
	objectInfo, err := b.HeadObject(ctx, key)
	if err != nil {
		// If object doesn't exist, that's ok for delete operation
		var notFound *s3types.NoSuchKey
		if stderr.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("failed to get object metadata for deletion validation: %w", err)
	}

	// Validate deletion against tier constraints
	objectAge := time.Since(objectInfo.LastModified)
	if err := b.tierValidator.ValidateDelete(key, objectAge); err != nil {
		b.metricsCollector.RecordError(err)
		return fmt.Errorf("tier validation failed: %w", err)
	}

	client := b.clientManager.GetPooledClient()
	defer b.clientManager.ReturnPooledClient(client)

	input := &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}

	_, err = client.DeleteObject(ctx, input)
	if err != nil {
		b.metricsCollector.RecordError(err)
		return b.translateError(err, "DeleteObject", key)
	}

	return nil
}

// HeadObject retrieves metadata about an object
func (b *Backend) HeadObject(ctx context.Context, key string) (*types.ObjectInfo, error) {
	start := time.Now()
	defer func() {
		b.metricsCollector.RecordMetrics(time.Since(start), false)
	}()

	client := b.clientManager.GetPooledClient()
	defer b.clientManager.ReturnPooledClient(client)

	input := &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}

	result, err := client.HeadObject(ctx, input)
	if err != nil {
		b.metricsCollector.RecordError(err)
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
		b.metricsCollector.RecordMetrics(time.Since(start), false)
	}()

	client := b.clientManager.GetPooledClient()
	defer b.clientManager.ReturnPooledClient(client)

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
		b.metricsCollector.RecordError(err)
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
	return b.clientManager.HealthCheck(ctx, b.bucket)
}

// GetMetrics returns current backend metrics
func (b *Backend) GetMetrics() BackendMetrics {
	return b.metricsCollector.GetMetrics()
}

// Close closes the backend and releases resources
func (b *Backend) Close() error {
	return b.clientManager.Close()
}

// Helper methods

func (b *Backend) translateError(err error, operation, key string) error {
	// Check for specific S3 error types and create rich error objects
	switch {
	case isErrorType[*s3types.NoSuchKey](err):
		return errors.NewError(errors.ErrCodeObjectNotFound, "object not found").
			WithComponent("s3-backend").
			WithOperation(operation).
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithCause(err)

	case isErrorType[*s3types.NoSuchBucket](err):
		return errors.NewError(errors.ErrCodeBucketNotFound, "bucket not found").
			WithComponent("s3-backend").
			WithOperation(operation).
			WithContext("bucket", b.bucket).
			WithContext("region", b.config.Region).
			WithCause(err)

	case isErrorType[*s3types.NotFound](err):
		return errors.NewError(errors.ErrCodeObjectNotFound, "resource not found").
			WithComponent("s3-backend").
			WithOperation(operation).
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithCause(err)

	case isErrorType[*s3types.InvalidObjectState](err):
		return errors.NewError(errors.ErrCodeInvalidState, "object in invalid state for operation").
			WithComponent("s3-backend").
			WithOperation(operation).
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithDetail("storage_class", b.currentTier).
			WithCause(err)

	default:
		// Check error message for common patterns
		errMsg := err.Error()

		// Timeout errors
		if strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "deadline exceeded") {
			return errors.NewError(errors.ErrCodeOperationTimeout, "S3 operation timed out").
				WithComponent("s3-backend").
				WithOperation(operation).
				WithContext("bucket", b.bucket).
				WithContext("key", key).
				WithDetail("timeout_config", map[string]interface{}{
					"connect_timeout": b.config.ConnectTimeout,
					"request_timeout": b.config.RequestTimeout,
				}).
				WithCause(err)
		}

		// Network errors
		if strings.Contains(errMsg, "connection") || strings.Contains(errMsg, "network") ||
			strings.Contains(errMsg, "dial") || strings.Contains(errMsg, "EOF") {
			return errors.NewError(errors.ErrCodeNetworkError, "network error during S3 operation").
				WithComponent("s3-backend").
				WithOperation(operation).
				WithContext("bucket", b.bucket).
				WithContext("key", key).
				WithContext("endpoint", b.config.Endpoint).
				WithContext("region", b.config.Region).
				WithCause(err)
		}

		// Access denied / permission errors
		if strings.Contains(errMsg, "AccessDenied") || strings.Contains(errMsg, "Forbidden") ||
			strings.Contains(errMsg, "403") {
			return errors.NewError(errors.ErrCodeAccessDenied, "access denied to S3 resource").
				WithComponent("s3-backend").
				WithOperation(operation).
				WithContext("bucket", b.bucket).
				WithContext("key", key).
				WithDetail("required_permissions", []string{
					"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket",
				}).
				WithCause(err)
		}

		// Generic error with context
		return errors.NewError(errors.ErrCodeStorageRead, fmt.Sprintf("%s operation failed", operation)).
			WithComponent("s3-backend").
			WithOperation(operation).
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithCause(err)
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
	return stderr.As(err, &target)
}

// GetCurrentTier returns the current storage tier information
func (b *Backend) GetCurrentTier() StorageTierInfo {
	return b.tierInfo
}

// GetAllTiers returns information about all available storage tiers
func (b *Backend) GetAllTiers() map[string]StorageTierInfo {
	return StorageTiers
}

// GetTierRecommendations returns tier recommendations for an object
func (b *Backend) GetTierRecommendations(objectSize int64, accessFrequency string) []string {
	return b.tierValidator.GetRecommendations(objectSize, accessFrequency)
}

// SetStorageTier changes the storage tier (requires restarting backend for full effect)
func (b *Backend) SetStorageTier(tier string, constraints TierConstraints) error {
	tierInfo, exists := StorageTiers[tier]
	if !exists {
		return fmt.Errorf("unsupported storage tier: %s", tier)
	}

	// Update tier validator
	b.tierValidator = NewTierValidator(tier, constraints, b.logger)

	// Update backend state
	b.currentTier = tier
	b.tierInfo = tierInfo
	b.config.StorageTier = tier
	b.config.TierConstraints = constraints

	b.logger.Info("Storage tier changed",
		"tier", tier,
		"tier_name", tierInfo.Name,
		"min_object_size", tierInfo.MinObjectSize,
		"deletion_embargo", tierInfo.DeletionEmbargo,
		"cost_per_gb_month", tierInfo.CostPerGBMonth)

	return nil
}

// ValidateObjectForTier validates if an object meets current tier requirements
func (b *Backend) ValidateObjectForTier(key string, size int64) error {
	return b.tierValidator.ValidateWrite(key, size)
}

// GetTierConstraints returns the current tier constraints
func (b *Backend) GetTierConstraints() TierConstraints {
	return b.config.TierConstraints
}

// GetTierCostEstimate estimates monthly storage cost for given data size
func (b *Backend) GetTierCostEstimate(sizeGB float64) float64 {
	return sizeGB * b.tierInfo.CostPerGBMonth
}

// GetCostOptimizationReport generates a cost optimization analysis report
func (b *Backend) GetCostOptimizationReport() OptimizationReport {
	report := b.costOptimizer.GetOptimizationReport()
	report.GeneratedAt = time.Now()
	return report
}

// OptimizeStorageCosts analyzes and applies cost optimizations
func (b *Backend) OptimizeStorageCosts(ctx context.Context) error {
	return b.costOptimizer.AnalyzeAndOptimize(ctx)
}

// EstimateStandardTierOverhead calculates potential overhead from Standard tier usage
func (b *Backend) EstimateStandardTierOverhead(objectSize int64, targetTier string) float64 {
	return b.costOptimizer.EstimateStandardTierOverhead(objectSize, targetTier)
}

// GetAccessPatterns returns access pattern data for cost analysis
func (b *Backend) GetAccessPatternCount() int {
	return len(b.costOptimizer.accessPatterns)
}

// GetPricingSummary returns current pricing configuration and rates
func (b *Backend) GetPricingSummary() PricingSummary {
	return b.pricingManager.GetPricingSummary()
}

// RefreshPricing forces a refresh of pricing data from AWS API
func (b *Backend) RefreshPricing(ctx context.Context) error {
	return b.pricingManager.RefreshPricing(ctx)
}

// GetTierPricingWithDiscounts returns pricing for a tier with all discounts applied
func (b *Backend) GetTierPricingWithDiscounts(tier string) (TierPricing, error) {
	return b.pricingManager.GetTierPricing(tier)
}

// CalculateCostWithVolume calculates cost for a specific volume and tier
func (b *Backend) CalculateCostWithVolume(tier string, sizeGB float64) (float64, error) {
	tierPricing, err := b.pricingManager.GetTierPricing(tier)
	if err != nil {
		return 0, err
	}

	baseCost := sizeGB * tierPricing.StorageCostPerGBMonth
	return b.pricingManager.CalculateVolumeDiscount(tier, sizeGB, baseCost), nil
}

// Health Status Management Methods

// GetHealthStatus returns the overall health status of the S3 backend
func (b *Backend) GetHealthStatus() health.HealthState {
	return b.healthTracker.GetOverallHealth()
}

// GetComponentHealth returns health status for a specific S3 operation component
func (b *Backend) GetComponentHealth(component string) (*health.ComponentHealth, error) {
	return b.healthTracker.GetComponentHealth(component)
}

// GetAllComponentsHealth returns health status for all S3 operation components
func (b *Backend) GetAllComponentsHealth() map[string]*health.ComponentHealth {
	return b.healthTracker.GetAllComponents()
}

// IsReadAvailable checks if read operations are currently available
func (b *Backend) IsReadAvailable() bool {
	return b.healthTracker.CanRead("s3-reads")
}

// IsWriteAvailable checks if write operations are currently available
func (b *Backend) IsWriteAvailable() bool {
	return b.healthTracker.CanWrite("s3-writes")
}

// IsFullyHealthy checks if all components are in healthy state
func (b *Backend) IsFullyHealthy() bool {
	return b.healthTracker.GetOverallHealth() == health.StateHealthy
}

// isAccelerationError checks if an error is related to Transfer Acceleration
func (b *Backend) isAccelerationError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := err.Error()

	// Common acceleration-specific errors
	accelerationErrors := []string{
		"InvalidRequest",         // Acceleration not enabled on bucket
		"acceleration",           // Generic acceleration error
		"s3-accelerate",          // Acceleration endpoint error
		"transfer-acceleration",  // Explicit acceleration error
		"AccelerateNotSupported", // Bucket doesn't support acceleration
		"BucketAlreadyExists",    // Sometimes returned for acceleration errors
	}

	for _, errPattern := range accelerationErrors {
		if strings.Contains(errMsg, errPattern) {
			return true
		}
	}

	return false
}

// executeWithAccelerationFallback executes an S3 operation with automatic fallback
func (b *Backend) executeWithAccelerationFallback(
	ctx context.Context,
	operation string,
	fn func(client *s3.Client) error,
) error {
	// If acceleration is not active, just execute with standard client
	if !b.clientManager.IsAccelerationActive() {
		client := b.clientManager.GetPooledClient()
		defer b.clientManager.ReturnPooledClient(client)
		return fn(client)
	}

	// Try with accelerated client first
	acceleratedClient := b.clientManager.GetAcceleratedClient()
	if acceleratedClient != nil {
		start := time.Now()
		err := fn(acceleratedClient)
		duration := time.Since(start)

		if err == nil {
			// Success with acceleration
			b.metricsCollector.RecordAcceleratedRequest(0, duration)
			return nil
		}

		// Check if this is an acceleration-specific error
		if b.isAccelerationError(err) {
			b.logger.Warn("S3 Transfer Acceleration error detected, falling back to standard endpoint",
				"operation", operation,
				"error", err.Error())
			b.metricsCollector.RecordFallbackEvent()
			b.clientManager.DisableAcceleration(fmt.Sprintf("acceleration error: %v", err))

			// Retry with standard client
			standardClient := b.clientManager.GetStandardClient()
			return fn(standardClient)
		}

		// Not an acceleration error, return as-is
		return err
	}

	// No accelerated client available, use standard
	standardClient := b.clientManager.GetStandardClient()
	return fn(standardClient)
}

// putObjectMultipart performs a multipart upload for large objects with parallel chunk uploads
func (b *Backend) putObjectMultipart(ctx context.Context, key string, data []byte, tier string) error {
	dataSize := int64(len(data))

	// Calculate optimal chunk size based on file size
	chunkSize := CalculateOptimalChunkSize(dataSize, b.config.MultipartThreshold, b.config.MultipartChunkSize)

	b.logger.Debug("Starting multipart upload",
		"key", key,
		"total_size", dataSize,
		"chunk_size", chunkSize,
		"tier", tier)

	// Get storage class for tier
	storageClass := ConvertTierToStorageClass(tier)
	contentType := b.detectContentType(key)

	// Initiate multipart upload
	var uploadID string
	err := b.executeWithAccelerationFallback(ctx, "CreateMultipartUpload", func(client *s3.Client) error {
		createInput := &s3.CreateMultipartUploadInput{
			Bucket:       aws.String(b.bucket),
			Key:          aws.String(key),
			ContentType:  aws.String(contentType),
			StorageClass: storageClass,
		}

		result, err := client.CreateMultipartUpload(ctx, createInput)
		if err != nil {
			b.metricsCollector.RecordError(err)
			return b.translateError(err, "CreateMultipartUpload", key)
		}

		uploadID = aws.ToString(result.UploadId)
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to initiate multipart upload: %w", err)
	}

	// Create upload state tracker
	uploadState := NewMultipartUploadState(uploadID, b.bucket, key, dataSize, chunkSize)
	b.multipartManager.TrackUpload(uploadState)
	defer b.multipartManager.RemoveUpload(uploadID)

	// Calculate number of parts
	totalParts := CalculatePartCount(dataSize, chunkSize)

	b.logger.Debug("Multipart upload initiated",
		"upload_id", uploadID,
		"total_parts", totalParts)

	// Upload parts in parallel with controlled concurrency
	type partResult struct {
		partNumber int
		etag       string
		size       int64
		err        error
	}

	resultCh := make(chan partResult, totalParts)
	semaphore := make(chan struct{}, b.config.MultipartConcurrency)

	// Launch goroutines for each part
	for partNum := 1; partNum <= totalParts; partNum++ {
		go func(pn int) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// Calculate part boundaries
			startOffset := int64(pn-1) * chunkSize
			endOffset := startOffset + chunkSize
			if endOffset > dataSize {
				endOffset = dataSize
			}

			partData := data[startOffset:endOffset]
			partSize := int64(len(partData))

			// Upload this part with retry logic
			var etag string
			uploadErr := b.retryer.DoWithContext(ctx, func(retryCtx context.Context) error {
				return b.executeWithAccelerationFallback(retryCtx, "UploadPart", func(client *s3.Client) error {
					uploadPartInput := &s3.UploadPartInput{
						Bucket:        aws.String(b.bucket),
						Key:           aws.String(key),
						UploadId:      aws.String(uploadID),
						PartNumber:    aws.Int32(int32(pn)),
						Body:          bytes.NewReader(partData),
						ContentLength: aws.Int64(partSize),
					}

					uploadResult, err := client.UploadPart(retryCtx, uploadPartInput)
					if err != nil {
						b.metricsCollector.RecordError(err)
						return b.translateError(err, "UploadPart", key)
					}

					etag = aws.ToString(uploadResult.ETag)
					b.multipartManager.UpdatePartStatus(uploadID, pn, partSize, etag, nil)

					b.logger.Debug("Part uploaded successfully",
						"upload_id", uploadID,
						"part_number", pn,
						"size", partSize,
						"progress", fmt.Sprintf("%.1f%%", uploadState.GetProgress()))

					return nil
				})
			})

			if uploadErr != nil {
				b.multipartManager.UpdatePartStatus(uploadID, pn, 0, "", uploadErr)
			}

			resultCh <- partResult{
				partNumber: pn,
				etag:       etag,
				size:       partSize,
				err:        uploadErr,
			}
		}(partNum)
	}

	// Collect all part results
	completedParts := make([]s3types.CompletedPart, 0, totalParts)
	var uploadErrors []error
	var totalBytesUploaded int64

	for i := 0; i < totalParts; i++ {
		result := <-resultCh
		if result.err != nil {
			uploadErrors = append(uploadErrors, fmt.Errorf("part %d failed: %w", result.partNumber, result.err))
			continue
		}

		completedParts = append(completedParts, s3types.CompletedPart{
			PartNumber: aws.Int32(int32(result.partNumber)),
			ETag:       aws.String(result.etag),
		})

		totalBytesUploaded += result.size
	}

	// If any parts failed, abort the multipart upload
	if len(uploadErrors) > 0 {
		b.multipartManager.MarkUploadFailed(uploadID)

		// Abort the multipart upload
		abortErr := b.executeWithAccelerationFallback(ctx, "AbortMultipartUpload", func(client *s3.Client) error {
			abortInput := &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(b.bucket),
				Key:      aws.String(key),
				UploadId: aws.String(uploadID),
			}
			_, err := client.AbortMultipartUpload(ctx, abortInput)
			return err
		})

		if abortErr != nil {
			b.logger.Warn("Failed to abort multipart upload after part failures",
				"upload_id", uploadID,
				"abort_error", abortErr)
		}

		return fmt.Errorf("multipart upload failed: %d parts failed: %v", len(uploadErrors), uploadErrors[0])
	}

	// Complete the multipart upload
	err = b.executeWithAccelerationFallback(ctx, "CompleteMultipartUpload", func(client *s3.Client) error {
		completeInput := &s3.CompleteMultipartUploadInput{
			Bucket:   aws.String(b.bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
			MultipartUpload: &s3types.CompletedMultipartUpload{
				Parts: completedParts,
			},
		}

		_, err := client.CompleteMultipartUpload(ctx, completeInput)
		if err != nil {
			b.metricsCollector.RecordError(err)
			return b.translateError(err, "CompleteMultipartUpload", key)
		}

		return nil
	})

	if err != nil {
		b.multipartManager.MarkUploadFailed(uploadID)
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	// Mark upload as completed
	b.multipartManager.MarkUploadCompleted(uploadID)

	// Record metrics
	b.metricsCollector.RecordBytesUploaded(totalBytesUploaded)
	b.healthTracker.RecordSuccess("s3-writes")

	b.logger.Info("Multipart upload completed successfully",
		"key", key,
		"upload_id", uploadID,
		"total_size", dataSize,
		"total_parts", totalParts,
		"bytes_uploaded", totalBytesUploaded)

	return nil
}
