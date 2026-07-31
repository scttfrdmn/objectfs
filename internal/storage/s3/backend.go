package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderr "errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	cargoships3 "github.com/scttfrdmn/cargoship/pkg/aws/s3"

	"github.com/objectfs/objectfs/internal/awsname"
	"github.com/objectfs/objectfs/internal/circuit"
	"github.com/objectfs/objectfs/internal/compression"
	"github.com/objectfs/objectfs/pkg/errors"
	"github.com/objectfs/objectfs/pkg/health"
	"github.com/objectfs/objectfs/pkg/retry"
	"github.com/objectfs/objectfs/pkg/types"
)

// S3 user-metadata keys written by ObjectFS on upload.
const (
	// metaChecksum holds the hex-encoded SHA-256 of the *uncompressed* content.
	// Stable across storage formats, so it survives a change of compression codec.
	metaChecksum = "objectfs-sha256"

	// metaOriginalSize holds the decimal byte length of the *uncompressed*
	// content, written only when transparent compression actually compressed the
	// object. Without it, HeadObject would report the compressed ContentLength as
	// the file size and the kernel would truncate every read at that length.
	metaOriginalSize = "objectfs-original-size"
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

	// Transparent object compression
	compressor *compression.Compressor
}

// NewBackend creates a new S3 backend instance
func NewBackend(ctx context.Context, bucket string, cfg *Config) (*Backend, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket name cannot be empty")
	}

	if cfg == nil {
		cfg = NewDefaultConfig()
	}

	// Reject a malformed region before building a client from it.
	//
	// internal/config validates this too, and the duplication is deliberate: this constructor is
	// public API that the Go SDK reaches with a hand-built &Config{Region: ...}, never passing
	// through the config loader. Validating only at the loader would leave the SDK path with the
	// behaviour FuzzConfigConstructsBackend found — a space in the region producing "exceeded maximum
	// number of attempts" several layers below anything that could name the cause.
	if err := awsname.ValidateRegion(cfg.Region); err != nil {
		return nil, fmt.Errorf("invalid S3 configuration: %w", err)
	}

	// Apply defaults for zero-value critical fields so that partial configs
	// (e.g. created with &Config{Region: "us-west-2"}) behave correctly.
	defaults := NewDefaultConfig()
	if cfg.MultipartThreshold == 0 {
		cfg.MultipartThreshold = defaults.MultipartThreshold
	}
	if cfg.MultipartChunkSize == 0 {
		cfg.MultipartChunkSize = defaults.MultipartChunkSize
	}
	if cfg.MultipartConcurrency == 0 {
		cfg.MultipartConcurrency = defaults.MultipartConcurrency
	}
	if cfg.ReadChunkSize == 0 {
		cfg.ReadChunkSize = defaults.ReadChunkSize
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

	// Initialize transparent compression
	compressor, err := compression.NewCompressor(compression.Settings{
		Enabled:   cfg.Compression.Enabled,
		Algorithm: cfg.Compression.Algorithm,
		Level:     cfg.Compression.Level,
		MinSize:   cfg.Compression.MinSize,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize compression: %w", err)
	}
	backend.compressor = compressor
	if compressor.Enabled() {
		logger.Info("S3 transparent compression enabled",
			"algorithm", cfg.Compression.Algorithm,
			"level", cfg.Compression.Level,
			"min_size", cfg.Compression.MinSize)
	}

	// Initialize circuit breaker manager.
	//
	// MaxRequests is the half-open probe limit, not a failure count — the trip decision belongs to
	// ReadyToTrip, which is left at the package default (20 requests in the interval with at least
	// half failing). Naming that here rather than relying on the zero value being filled in, because
	// reading this block and seeing only MaxRequests invites the conclusion that ten failures trip
	// the breaker, and it was read that way during the audit.
	//
	// What counts as a failure is circuit.defaultIsSuccessful, which asks
	// errors.IsServiceFailure: a missing object is an answer, not an outage.
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

	// Parallel range GET fast-path: fan out large reads into concurrent chunks.
	// Skipped when transparent compression is active (whole-object decompress
	// path must remain intact) or when the threshold is disabled.
	threshold := b.config.ParallelReadThreshold
	if threshold > 0 && (b.compressor == nil || !b.compressor.Enabled()) {
		readSize := size
		if readSize <= 0 {
			// Need the object size to calculate chunk count — one HEAD call.
			if info, headErr := b.HeadObject(ctx, key); headErr == nil {
				readSize = info.Size - offset
			}
		}
		if readSize > threshold {
			return b.parallelGetObject(ctx, key, offset, readSize)
		}
	}

	// Ask S3 for exactly the bytes the caller wants.
	//
	// Whether this object needs whole-object decoding is a property of *the object*, not of the
	// local compression config, and the response reports it: S3 returns Content-Encoding on a 206
	// just as it does on a 200. So request the range, and re-fetch whole only if the answer comes
	// back encoded.
	//
	// The alternative — fetching whole whenever compression is configured — is audit finding C4, and
	// it is catastrophic rather than merely wasteful. It applied to every object in the bucket,
	// including ones never compressed, ones below MinSize, ones where compression did not help, and
	// ones written by other tools. Measured on real S3: a 4 KiB read of a 256 MiB object took 49
	// seconds against 227 ms, and a 4 KiB read of a 10 GiB object transferred all 10 GiB —
	// 2,621,440x amplification. Compressed objects are the rare case and are the only ones that pay.
	fetchOffset, fetchSize := offset, size

	data, contentEncoding, objectMeta, err := b.getObjectRange(ctx, key, fetchOffset, fetchSize)

	// A ranged read has two ways of landing on a compressed object, and both mean the same thing: the
	// range was applied to the *stored* bytes when the caller meant the *decoded* ones. A zstd or
	// gzip frame is not seekable, so neither can be served from a range — the whole object has to be
	// fetched and decoded. That costs one extra request, paid only by compressed objects read in
	// part; the seekable-zstd issue is the fix that removes it.
	//
	//  1. The read succeeded and came back encoded: the range fell inside the compressed body.
	//  2. The read failed 416: the range fell past the end of the compressed body. A
	//     legitimately-sized read of the decoded content routinely does, since the body is a
	//     fraction of the size the caller was told. S3 is right to refuse; the request was wrong.
	//
	// Case 2 is ambiguous, though, and resolving it wrong is expensive in one direction. A 416 also
	// means "you read past the end", which for an uncompressed object is an ordinary EOF read that
	// must return no bytes — and fetching a 10 GiB object to discover that would reintroduce exactly
	// the amplification this code removed. So ask HeadObject, which answers in one cheap request
	// whether the object is compressed and how long its decoded content is.
	ranged := fetchOffset > 0 || fetchSize > 0

	switch {
	case ranged && err == nil && contentEncoding != "":
		fetchOffset, fetchSize = 0, 0
		data, contentEncoding, objectMeta, err = b.getObjectRange(ctx, key, fetchOffset, fetchSize)

		b.logger.Debug("Re-fetched whole object: a ranged read cannot slice an encoded body",
			"key", key, "content_encoding", contentEncoding, "offset", offset, "size", size)

	case ranged && isInvalidRange(err):
		info, headErr := b.HeadObject(ctx, key)
		if headErr != nil {
			// Report the range failure, not the HEAD's: the range request is what the caller made,
			// and the HEAD was our own attempt to interpret it.
			return nil, err
		}

		// HeadObject reports the *decoded* size, so this is the caller's coordinate space. Past the
		// end of that is a real EOF read whatever the storage format, and costs nothing more.
		if offset >= info.Size {
			return []byte{}, nil
		}

		if !isCompressed(info.Metadata) {
			// Inside an uncompressed object but S3 refused the range: not something to paper over
			// with a whole-object fetch.
			return nil, err
		}

		fetchOffset, fetchSize = 0, 0
		data, contentEncoding, objectMeta, err = b.getObjectRange(ctx, key, fetchOffset, fetchSize)

		b.logger.Debug("Re-fetched whole object: the range fell past the end of a compressed body",
			"key", key, "decoded_size", info.Size, "offset", offset, "size", size)
	}

	if err != nil {
		return nil, err
	}

	// Decompress if the object was stored with transparent compression. The encoding comes from the
	// object's own header rather than the write config, so objects stay readable across a config
	// change (audit finding C2 is the opposite: dispatching on the configured codec silently returned
	// the raw frame for anything else).
	if b.compressor != nil && contentEncoding != "" {
		decompressed, decompErr := b.compressor.Decompress(data, contentEncoding)
		if decompErr != nil {
			return nil, fmt.Errorf("decompress object %q: %w", key, decompErr)
		}
		data = decompressed
	}

	// Fail closed if the object is still encoded after the decode above.
	//
	// objectfs-original-size is written only for objects that were actually compressed, so its
	// presence is an assertion by the writer that the caller must receive that many bytes. If the
	// data on hand is shorter, decompression did not happen — either the stored Content-Encoding was
	// lost (which is what CargoShip's metadata-only upload did), or it names a codec this build
	// cannot decode, or compression was reconfigured since the write.
	//
	// Returning the bytes anyway is the worst option available: HeadObject reports the uncompressed
	// size, so the kernel pads the shortfall with zeros and the caller gets a silently corrupt file
	// with a successful exit status. Compressor.Decompress does exactly that today for an encoding it
	// does not recognize, which is why the check lives here rather than there.
	if err := checkFullyDecoded(objectMeta, data, offset, size, contentEncoding, key); err != nil {
		b.metricsCollector.RecordError(err)

		return nil, err
	}

	// Slice locally only when the fetch covered more than the caller asked for, which now happens
	// only on the encoded re-fetch above. An ordinary ranged read is already exactly the requested
	// bytes and must not be sliced again — the offset would be applied twice.
	if fetchOffset != offset || fetchSize != size {
		data = sliceRange(data, offset, size)
	}

	// Record access pattern for cost optimization
	b.costOptimizer.RecordAccess(key, int64(len(data)))

	return data, nil
}

// sliceRange returns data[offset : offset+size], clamped to what data holds.
//
// It is a function so the bounds arithmetic has one home and one set of tests. The inline version was
// audit finding C3: with size < 0 it computed end < offset, neither clamp arm fired, and the slice
// expression panicked — "slice bounds out of range [100:99]" — taking the mount process down and
// unmounting under every open fd. A size of 0 or less is treated as "to the end", matching the range
// header the fetch would have sent.
//
// The end is derived by subtraction rather than by addition, and that is not a stylistic preference.
// `offset+size < end` is the C3 panic again by another route: the sum overflows for a large size, wraps
// negative, compares below end, and produces `data[1:-9223372036854775808]`. FuzzSliceRange found it in
// the fixed code. Subtracting cannot overflow here, because offset is already clamped into
// [0, len(data)) above, so len(data)-offset is positive and no larger than len(data).
func sliceRange(data []byte, offset, size int64) []byte {
	if offset < 0 {
		offset = 0
	}

	if offset >= int64(len(data)) {
		return []byte{}
	}

	end := int64(len(data))
	if size > 0 && size < end-offset {
		end = offset + size
	}

	return data[offset:end]
}

// getObjectRange fetches a byte range of an object, or the whole object when size is not positive,
// and reports the encoding and user metadata S3 returned with it.
//
// The reliability stack lives here rather than at the call site so both the direct read and the
// encoded re-fetch get it: retry, circuit breaker, health tracking, metrics, and error translation.
func (b *Backend) getObjectRange(
	ctx context.Context,
	key string,
	offset, size int64,
) (data []byte, contentEncoding string, metadata map[string]string, err error) {
	var rangeHeader *string

	switch {
	case size > 0:
		rangeHeader = aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+size-1))
	case offset > 0:
		rangeHeader = aws.String(fmt.Sprintf("bytes=%d-", offset))
	}

	breaker := b.circuitManager.GetBreaker("s3-get")

	err = b.retryer.DoWithContext(ctx, func(retryCtx context.Context) error {
		return breaker.ExecuteWithContext(retryCtx, func(ctx context.Context) error {
			// Reset per attempt. A retry that fails after a partial read would otherwise leave the
			// previous attempt's bytes in place, and the caller cannot tell a stale buffer from a
			// fresh one (audit finding L24).
			data, contentEncoding, metadata = nil, "", nil

			input := &s3.GetObjectInput{
				Bucket: aws.String(b.bucket),
				Key:    aws.String(key),
				Range:  rangeHeader,
			}

			return b.executeWithAccelerationFallback(ctx, "GetObject", func(client *s3.Client) error {
				result, getErr := client.GetObject(ctx, input)
				if getErr != nil {
					b.metricsCollector.RecordError(getErr)
					translatedErr := b.translateError(getErr, "GetObject", key)
					b.healthTracker.RecordError("s3-reads", translatedErr)

					return translatedErr
				}
				defer func() { _ = result.Body.Close() }()

				contentEncoding = aws.ToString(result.ContentEncoding)
				metadata = result.Metadata

				body, readErr := io.ReadAll(result.Body)
				if readErr != nil {
					b.metricsCollector.RecordError(readErr)
					wrapped := fmt.Errorf("failed to read object body: %w", readErr)
					b.healthTracker.RecordError("s3-reads", wrapped)

					return wrapped
				}

				data = body

				b.metricsCollector.RecordBytesDownloaded(int64(len(data)))
				b.healthTracker.RecordSuccess("s3-reads")

				return nil
			})
		})
	})
	if err != nil {
		return nil, "", nil, err
	}

	return data, contentEncoding, metadata, nil
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

	// Compute SHA-256 of the uncompressed canonical content before any
	// encoding so the hash is stable regardless of storage format.
	rawHash := sha256.Sum256(data)
	checksumHex := hex.EncodeToString(rawHash[:])

	// Apply transparent compression before upload.
	uploadData := data
	contentEncoding := ""
	compressed := false
	if b.compressor != nil {
		compressedData, wasCompressed, comprErr := b.compressor.Compress(data)
		if comprErr != nil {
			return fmt.Errorf("compress object %q: %w", key, comprErr)
		}
		if wasCompressed {
			uploadData = compressedData
			contentEncoding = b.compressor.ContentEncoding()
			compressed = true
			b.logger.Debug("Object compressed for upload",
				"key", key,
				"original_size", len(data),
				"compressed_size", len(uploadData),
				"ratio", float64(len(uploadData))/float64(len(data)))
		}
	}

	// Build the user metadata common to every upload path. The original size is
	// recorded only for compressed objects, so HeadObject can report the size the
	// kernel needs for reads rather than the compressed ContentLength.
	objectMeta := map[string]string{metaChecksum: checksumHex}
	if compressed {
		objectMeta[metaOriginalSize] = strconv.FormatInt(int64(len(data)), 10)
	}

	breaker := b.circuitManager.GetBreaker("s3-put")

	err := breaker.ExecuteWithContext(ctx, func(ctx context.Context) error {
		// Check if we should use multipart upload based on compressed size.
		dataSize := int64(len(uploadData))
		if dataSize >= b.config.MultipartThreshold {
			b.logger.Debug("Using multipart upload for large object",
				"key", key,
				"size", dataSize,
				"threshold", b.config.MultipartThreshold)
			return b.putObjectMultipart(ctx, key, uploadData, effectiveTier, contentEncoding, objectMeta)
		}

		// Get storage class for effective tier
		storageClass := ConvertTierToStorageClass(effectiveTier)

		input := &s3.PutObjectInput{
			Bucket:        aws.String(b.bucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(uploadData),
			ContentLength: aws.Int64(int64(len(uploadData))),
			ContentType:   aws.String(b.detectContentType(key)),
			StorageClass:  storageClass,
			Metadata:      objectMeta,
		}
		if contentEncoding != "" {
			input.ContentEncoding = aws.String(contentEncoding)
		}

		// Use the CargoShip transporter for optimized uploads, but never for a compressed object.
		//
		// cargoships3.Archive has no ContentEncoding field — its CompressionType becomes user
		// metadata (transporter.go:184) and nothing sets the HTTP header. Uploading a compressed
		// object through it therefore stored the encoding as metadata only, so GetObject saw an
		// empty result.ContentEncoding, skipped decompression, and returned the raw zstd frame while
		// HeadObject still reported the uncompressed size. An 8 KiB write read back as 29 bytes with
		// no error, and the kernel zero-padded the difference: silent corruption on the default
		// configuration, which enables both compression and CargoShip.
		//
		// The direct path below sets ContentEncoding properly, so compressed objects take it. This
		// costs CargoShip's throughput optimization only for objects that compressed — and only
		// until the transporter can carry the header.
		transporter := b.clientManager.GetTransporter()
		if contentEncoding != "" && transporter != nil {
			b.logger.Debug("Bypassing CargoShip for a compressed object: the transporter cannot set Content-Encoding",
				"key", key,
				"content_encoding", contentEncoding)

			transporter = nil
		}

		if transporter != nil {
			// Use CargoShip's optimized upload with BBR/CUBIC algorithms
			cargoStorageClass := ConvertTierToCargoShipStorageClass(effectiveTier)
			cargoMeta := map[string]string{
				"objectfs-upload": "true",
				"content-type":    b.detectContentType(key),
				"storage-tier":    effectiveTier,
				"configured-tier": b.currentTier,
			}
			for k, v := range objectMeta {
				cargoMeta[k] = v
			}
			if contentEncoding != "" {
				cargoMeta["content-encoding"] = contentEncoding
			}
			archive := cargoships3.Archive{
				Key:          key,
				Reader:       bytes.NewReader(uploadData),
				Size:         int64(len(uploadData)),
				StorageClass: cargoStorageClass,
				Metadata:     cargoMeta,
			}

			result, uploadErr := transporter.Upload(ctx, archive)
			if uploadErr == nil {
				b.logger.Debug("CargoShip optimized upload completed",
					"key", key,
					"size", len(uploadData),
					"throughput", result.Throughput,
					"duration", result.Duration)
				b.metricsCollector.RecordBytesUploaded(int64(len(uploadData)))
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

			b.metricsCollector.RecordBytesUploaded(int64(len(uploadData)))
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

	// Get object metadata to check creation time for tier validation. Deleting a key that is not
	// there is a no-op, which is both S3's contract and what the Go SDK documents.
	objectInfo, err := b.HeadObject(ctx, key)
	if err != nil {
		if isNotFound(err) {
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

	client, err := b.clientManager.GetPooledClient()
	if err != nil {
		b.metricsCollector.RecordError(err)
		return fmt.Errorf("delete %q: %w", key, err)
	}
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

	client, err := b.clientManager.GetPooledClient()
	if err != nil {
		b.metricsCollector.RecordError(err)
		return nil, fmt.Errorf("head %q: %w", key, err)
	}
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

	// Populate Checksum from objectfs-sha256 metadata key (set on upload).
	// Empty string for objects written before this feature — backward compatible.
	if v, ok := result.Metadata[metaChecksum]; ok {
		info.Checksum = v
	}

	// For compressed objects ContentLength is the *compressed* length, which is
	// not the file size a POSIX caller expects — the kernel would truncate reads
	// at that offset. Prefer the recorded uncompressed size when present.
	info.Size = originalSize(result.Metadata, info.Size, key, b.logger)

	return info, nil
}

// originalSize returns the uncompressed object size recorded in metadata, falling
// back to contentLength when the key is absent (objects written before
// objectfs-original-size existed, or uncompressed objects) or unparseable.
func originalSize(metadata map[string]string, contentLength int64, key string, logger *slog.Logger) int64 {
	v, ok := metadata[metaOriginalSize]
	if !ok {
		return contentLength
	}
	size, err := strconv.ParseInt(v, 10, 64)
	if err != nil || size < 0 {
		logger.Warn("Ignoring malformed objectfs-original-size metadata; falling back to ContentLength",
			"key", key,
			"value", v,
			"content_length", contentLength)
		return contentLength
	}
	return size
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

	client, err := b.clientManager.GetPooledClient()
	if err != nil {
		b.metricsCollector.RecordError(err)
		return nil, fmt.Errorf("list %q: %w", prefix, err)
	}
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

	// NOTE: Size here is the *stored* length from the list response. For compressed
	// objects that is the compressed size — ListObjectsV2 returns no user metadata,
	// so objectfs-original-size is unavailable without a HeadObject per entry. This
	// only affects sizes shown in directory listings; the kernel sizes reads from
	// Lookup → HeadObject, which does report the uncompressed size. Adding a
	// HeadObject fan-out here would cost one request per entry and is deliberately
	// not done.
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

// parallelGetObject fans out a large read into N concurrent range GETs bounded
// by ParallelReadConcurrency (defaults to MultipartConcurrency when 0).
// All chunks are assembled in order before returning.
func (b *Backend) parallelGetObject(ctx context.Context, key string, offset, totalSize int64) ([]byte, error) {
	chunkSize := b.config.ReadChunkSize
	if chunkSize <= 0 {
		chunkSize = 16 * 1024 * 1024
	}
	concurrency := b.config.ParallelReadConcurrency
	if concurrency <= 0 {
		concurrency = b.config.MultipartConcurrency
	}
	if concurrency <= 0 {
		concurrency = 8
	}

	numChunks := (totalSize + chunkSize - 1) / chunkSize

	type chunkResult struct {
		index int
		data  []byte
		err   error
	}
	resultCh := make(chan chunkResult, numChunks)
	semaphore := make(chan struct{}, concurrency)

	for i := int64(0); i < numChunks; i++ {
		go func(idx int64) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			start := offset + idx*chunkSize
			end := start + chunkSize
			if end > offset+totalSize {
				end = offset + totalSize
			}
			rangeHdr := aws.String(fmt.Sprintf("bytes=%d-%d", start, end-1))

			var chunk []byte
			err := b.executeWithAccelerationFallback(ctx, "GetObject", func(c *s3.Client) error {
				res, e := c.GetObject(ctx, &s3.GetObjectInput{
					Bucket: aws.String(b.bucket),
					Key:    aws.String(key),
					Range:  rangeHdr,
				})
				if e != nil {
					return b.translateError(e, "GetObject", key)
				}
				defer func() { _ = res.Body.Close() }()
				var readErr error
				chunk, readErr = io.ReadAll(res.Body)
				if readErr == nil {
					b.metricsCollector.RecordBytesDownloaded(int64(len(chunk)))
				}
				return readErr
			})
			resultCh <- chunkResult{index: int(idx), data: chunk, err: err}
		}(i)
	}

	chunks := make([][]byte, numChunks)
	for i := int64(0); i < numChunks; i++ {
		r := <-resultCh
		if r.err != nil {
			return nil, r.err
		}
		chunks[r.index] = r.data
	}

	b.costOptimizer.RecordAccess(key, totalSize)
	return bytes.Join(chunks, nil), nil
}

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

// checkFullyDecoded reports an integrity error when an object recorded an uncompressed size that the
// data in hand cannot account for.
//
// It only fires for a whole-object read — a ranged read legitimately returns fewer bytes than the
// object holds, and distinguishing "this range is short because it is a range" from "this range is
// short because it is still compressed" is not possible from the length alone. That is acceptable
// because the whole-object path is where the encoding is resolved: when compression is enabled the
// backend fetches the entire object and slices after decoding, so a ranged caller reaches this with
// data that already went through the decode above.
func checkFullyDecoded(metadata map[string]string, data []byte, offset, size int64, contentEncoding, key string) error {
	recorded, ok := metadata[metaOriginalSize]
	if !ok {
		return nil
	}

	want, parseErr := strconv.ParseInt(recorded, 10, 64)
	if parseErr != nil || want < 0 {
		// HeadObject warns and falls back to ContentLength for a malformed value; matching that
		// here keeps the two from disagreeing about the same object.
		return nil
	}

	wholeObject := offset == 0 && (size <= 0 || size >= want)
	if !wholeObject || int64(len(data)) >= want {
		return nil
	}

	detail := "the stored Content-Encoding was lost, so the object was never decompressed"
	if contentEncoding != "" {
		detail = fmt.Sprintf("the object is encoded as %q, which this build cannot decode", contentEncoding)
	}

	return errors.NewError(errors.ErrCodeDataCorruption,
		"object is still encoded after decompression; refusing to return partial content").
		WithComponent("s3-backend").
		WithOperation("GetObject").
		WithContext("key", key).
		WithContext("content_encoding", contentEncoding).
		WithDetail("recorded_size", want).
		WithDetail("decoded_size", len(data)).
		WithDetail("cause", detail).
		WithDetail("suggestion", "The object was written by a build that stored the encoding as user "+
			"metadata instead of the Content-Encoding header. Rewrite it, or read it with a build "+
			"that recognizes that layout.")
}

// isNotFound reports whether err means "that key is not there", whatever layer produced it.
//
// Three spellings have to be recognized. S3 answers a missing key with NoSuchKey on GetObject but
// with NotFound on HeadObject — a distinction that made DeleteObject error on a key that was already
// gone, contradicting both S3's contract and the Go SDK's documented no-op. And once translateError
// has run, the SDK type is wrapped inside an ObjectFSError, so a caller downstream of a Backend
// method sees neither raw type; matching on the code is what works there.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}

	if isErrorType[*s3types.NoSuchKey](err) || isErrorType[*s3types.NotFound](err) {
		return true
	}

	var objErr *errors.ObjectFSError
	if stderr.As(err, &objErr) {
		return objErr.Code == errors.ErrCodeObjectNotFound
	}

	// A backend behind a non-AWS endpoint may report absence only in the API error code.
	var apiErr smithy.APIError
	if stderr.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}

	return false
}

// isInvalidRange reports whether err is S3 refusing a Range that falls outside the object — HTTP 416
// with an InvalidRange code.
//
// The Go SDK models this as a bare API error rather than a typed shape, so the code string is the
// only thing to match on. Matching the *code* rather than searching the message is what keeps this
// from being audit finding L27, where substring-matching an error message made unrelated failures
// look like this one.
func isInvalidRange(err error) bool {
	if err == nil {
		return false
	}

	var apiErr smithy.APIError
	if stderr.As(err, &apiErr) {
		return apiErr.ErrorCode() == "InvalidRange"
	}

	return false
}

// isCompressed reports whether an object's metadata says its stored bytes are encoded.
//
// objectfs-original-size is written only for objects that actually compressed, which makes its
// presence the writer's own record that the stored length and the content length differ.
func isCompressed(metadata map[string]string) bool {
	for k := range metadata {
		if strings.EqualFold(k, metaOriginalSize) {
			return true
		}
	}

	return false
}

// Bucket returns the bucket this backend operates on.
//
// The bucket is fixed at construction and already appears in every log line and error this backend
// produces, so exposing it reveals nothing new. It is here because a caller holding a backend
// otherwise has no way to name the bucket it is using — which a second backend over the same objects,
// configured differently, needs in order to be constructed at all.
func (b *Backend) Bucket() string {
	return b.bucket
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
		client, err := b.clientManager.GetPooledClient()
		if err != nil {
			return fmt.Errorf("%s: %w", operation, err)
		}
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

// putObjectMultipart performs a multipart upload for large objects with parallel
// chunk uploads. The upload logic is split across helpers in multipart_upload.go:
//   - initiateMultipartUpload  – CreateMultipartUpload → uploadID
//   - uploadParts              – concurrent UploadPart fan-out
//   - abortMultipartUpload     – AbortMultipartUpload on failure
//   - completeMultipartUpload  – CompleteMultipartUpload on success
//
// objectMeta carries the S3 user metadata built by the caller (checksum over the
// uncompressed content, plus the original size when compressed). It must not be
// recomputed here: data is the post-compression payload, so hashing it would
// store a checksum of the compressed bytes and diverge from the single-part path.
func (b *Backend) putObjectMultipart(ctx context.Context, key string, data []byte, tier, contentEncoding string, objectMeta map[string]string) error {
	dataSize := int64(len(data))
	chunkSize := CalculateOptimalChunkSize(dataSize, b.config.MultipartThreshold, b.config.MultipartChunkSize)
	storageClass := ConvertTierToStorageClass(tier)
	contentType := b.detectContentType(key)

	b.logger.Debug("Starting multipart upload",
		"key", key, "total_size", dataSize, "chunk_size", chunkSize, "tier", tier)

	uploadID, err := b.initiateMultipartUpload(ctx, key, contentType, contentEncoding, objectMeta, storageClass)
	if err != nil {
		return fmt.Errorf("failed to initiate multipart upload: %w", err)
	}

	uploadState := NewMultipartUploadState(uploadID, b.bucket, key, dataSize, chunkSize)
	b.multipartManager.TrackUpload(uploadState)
	defer b.multipartManager.RemoveUpload(uploadID)

	totalParts := CalculatePartCount(dataSize, chunkSize)
	b.logger.Debug("Multipart upload initiated", "upload_id", uploadID, "total_parts", totalParts)

	completedParts, totalBytesUploaded, err := b.uploadParts(ctx, key, uploadID, data, chunkSize, totalParts, uploadState)
	if err != nil {
		b.multipartManager.MarkUploadFailed(uploadID)
		if abortErr := b.abortMultipartUpload(ctx, key, uploadID); abortErr != nil {
			b.logger.Warn("Failed to abort multipart upload after part failures",
				"upload_id", uploadID, "abort_error", abortErr)
		}
		return fmt.Errorf("multipart upload failed: %w", err)
	}

	if err := b.completeMultipartUpload(ctx, key, uploadID, completedParts); err != nil {
		b.multipartManager.MarkUploadFailed(uploadID)
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	b.multipartManager.MarkUploadCompleted(uploadID)
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
