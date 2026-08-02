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
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	cargoships3 "github.com/scttfrdmn/cargoship/pkg/aws/s3"
	"golang.org/x/sync/errgroup"

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
	// behavior FuzzConfigConstructsBackend found — a space in the region producing "exceeded maximum
	// number of attempts" several layers below anything that could name the cause.
	if err := awsname.ValidateRegion(cfg.Region); err != nil {
		return nil, fmt.Errorf("invalid S3 configuration: %w", err)
	}

	// Apply defaults for zero-value critical fields so that partial configs
	// (e.g. created with &Config{Region: "us-west-2"}) behave correctly.
	//
	// The list below is exhaustive over the fields whose zero value is not a usable setting, and it
	// became exhaustive as audit finding M18: it previously covered the four multipart and read-chunk
	// fields and stopped, so the doc comment's promise held for the shape it named and not for the
	// fields that break when unset. PoolSize is the sharp one — zero is not a small pool, it is
	// `make(chan struct{}, 0)` in GetObjects and PutObjects, an unbuffered semaphore whose first send
	// never returns.
	//
	// ParallelReadThreshold is deliberately absent. Zero is how this package spells "parallel reads
	// off" — the gate in GetObject is `threshold > 0` — so backfilling it would make the feature
	// unswitchable-off from the SDK. What made it a defect was the mount path leaving it at zero
	// while configuration said otherwise, and that is fixed in the mapping, not here.
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
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = defaults.PoolSize
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = defaults.MaxRetries
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = defaults.ConnectTimeout
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaults.RequestTimeout
	}
	if cfg.CongestionAlgorithm == "" {
		cfg.CongestionAlgorithm = defaults.CongestionAlgorithm
	}

	// retry.New backfills the delays and the attempt count but not RetryableErrors, and shouldRetry
	// consults that list — so an empty retry.Config is a retryer that reports three attempts and
	// retries nothing. A &Config{Region: ...} from the SDK is exactly that shape.
	if len(cfg.RetryConfig.RetryableErrors) == 0 {
		cfg.RetryConfig.RetryableErrors = defaults.RetryConfig.RetryableErrors
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
	// ReadyToTrip. Naming that here rather than relying on the zero value being filled in, because
	// reading this block and seeing only MaxRequests invites the conclusion that ten failures trip
	// the breaker, and it was read that way during the audit.
	//
	// What counts as a failure is circuit.defaultIsSuccessful, which asks
	// errors.IsServiceFailure: a missing object is an answer, not an outage.
	circuitConfig := circuit.Config{
		MaxRequests: 10,
		Interval:    60 * time.Second,
		Timeout:     cfg.CircuitBreaker.Timeout,
		ReadyToTrip: readyToTrip(cfg.CircuitBreaker),
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

	read, err := b.getObjectRange(ctx, key, fetchOffset, fetchSize)

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
	case ranged && err == nil && read.contentEncoding != "":
		fetchOffset, fetchSize = 0, 0
		read, err = b.getObjectRange(ctx, key, fetchOffset, fetchSize)

		b.logger.Debug("Re-fetched whole object: a ranged read cannot slice an encoded body",
			"key", key, "content_encoding", read.contentEncoding, "offset", offset, "size", size)

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
		read, err = b.getObjectRange(ctx, key, fetchOffset, fetchSize)

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
	data := read.data
	if b.compressor != nil && read.contentEncoding != "" {
		decompressed, decompErr := b.compressor.Decompress(data, read.contentEncoding)
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
	if err := checkFullyDecoded(read.metadata, data, offset, size, read.contentEncoding, key); err != nil {
		b.metricsCollector.RecordError(err)

		return nil, err
	}

	// Verify the stored SHA-256 against the bytes actually in hand.
	//
	// Placed before the slice below, not after: the recorded hash is over the whole uncompressed
	// content, so it can only be checked against a buffer holding all of it. See verifyChecksum for
	// what that leaves uncovered.
	//
	// The question asked is "does this buffer hold the entire object", and read.whole answers it from
	// what S3 reported rather than from the shape of the request. Those differ constantly and in the
	// direction that matters: a cat(1) of a 4 KiB file arrives here as offset=0, size=131072 — the
	// kernel's MaxRead, not the file's length — which sends a Range header and returns every byte of
	// the object. Keying on "was a range requested" would decline to verify the single most common
	// read a filesystem serves, and would have left this check dark for whole files while reporting
	// that reads are verified.
	if read.whole {
		if err := verifyChecksum(read.metadata, data, key); err != nil {
			b.metricsCollector.RecordError(err)
			b.healthTracker.RecordError("s3-reads", err)

			return nil, err
		}
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

// objectRead is what one GET returned: the bytes, what S3 said about them, and whether they are the
// whole object.
//
// A struct rather than a fourth and fifth return value because whole is the one field a caller can
// get wrong silently. Deriving it at the call site means re-deriving S3's answer from the request,
// and the two disagree for the most ordinary read there is — a full-file read whose size is the
// kernel's buffer rather than the file's length. Computed once, where the response is in hand.
type objectRead struct {
	data            []byte
	contentEncoding string
	metadata        map[string]string

	// whole reports that data holds every byte of the stored object, so a whole-object hash can be
	// checked against it. It is a claim about the *stored* bytes: for a compressed object it means the
	// complete compressed body, which is what decodes to the complete content.
	whole bool

	// etag is S3's ETag for the object this response was served from.
	//
	// It is here for the parallel read path, which issues several GETs for one logical read and has
	// no other way to tell that they all came from the same object. An overwrite landing between the
	// first chunk and the last is otherwise completely silent: every range succeeds, the lengths add
	// up, and the assembled buffer is a splice of two generations of the file that never existed on
	// either side.
	etag string
}

// getObjectRange fetches a byte range of an object, or the whole object when size is not positive,
// and reports the encoding, user metadata, and whole-object coverage S3 returned with it.
//
// The reliability stack lives here rather than at the call site so both the direct read and the
// encoded re-fetch get it: retry, circuit breaker, health tracking, metrics, and error translation.
func (b *Backend) getObjectRange(
	ctx context.Context,
	key string,
	offset, size int64,
) (objectRead, error) {
	var (
		data            []byte
		contentEncoding string
		metadata        map[string]string
		whole           bool
		etag            string
	)

	var rangeHeader *string

	switch {
	case size > 0:
		rangeHeader = aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+size-1))
	case offset > 0:
		rangeHeader = aws.String(fmt.Sprintf("bytes=%d-", offset))
	}

	breaker := b.circuitManager.GetBreaker("s3-get")

	err := b.retryer.DoWithContext(ctx, func(retryCtx context.Context) error {
		return breaker.ExecuteWithContext(retryCtx, func(ctx context.Context) error {
			// Reset per attempt. A retry that fails after a partial read would otherwise leave the
			// previous attempt's bytes in place, and the caller cannot tell a stale buffer from a
			// fresh one (audit finding L24).
			data, contentEncoding, metadata, whole, etag = nil, "", nil, false, ""

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
				etag = aws.ToString(result.ETag)

				body, readErr := io.ReadAll(result.Body)
				if readErr != nil {
					b.metricsCollector.RecordError(readErr)

					// Through translateError rather than a bare fmt.Errorf: a body read that stops
					// because the context was canceled has to reach the health tracker as
					// ErrCodeOperationCanceled, and an unclassified error counts as a service
					// failure. This is where a canceled read spends most of its time — the GET
					// returns as soon as the headers arrive, and the body is the part that takes
					// long enough to be interrupted — so it is the arm that matters most.
					translatedErr := b.translateError(readErr, "GetObject", key)
					b.healthTracker.RecordError("s3-reads", translatedErr)

					return translatedErr
				}

				data = body

				// Did this response carry the entire stored object?
				//
				// A 200 with no Content-Range did by definition. A 206 did only if the range it reports
				// spans the whole thing, which is the case for any read whose requested length met or
				// exceeded the object — routine, since callers size reads by buffer rather than by file.
				// S3 states the total after the slash in "bytes 0-4095/4096", so it is read from there
				// rather than guessed from the request.
				whole = wholeObjectResponse(aws.ToString(result.ContentRange), int64(len(body)))

				b.metricsCollector.RecordBytesDownloaded(int64(len(data)))
				b.healthTracker.RecordSuccess("s3-reads")

				return nil
			})
		})
	})
	if err != nil {
		return objectRead{}, err
	}

	return objectRead{
		data:            data,
		contentEncoding: contentEncoding,
		metadata:        metadata,
		whole:           whole,
		etag:            etag,
	}, nil
}

// wholeObjectResponse reports whether a GET response body is the entire stored object, given the
// Content-Range header S3 returned and the number of bytes actually read.
//
// An empty header means a 200: the whole object, whatever its length. Otherwise the header is
// "bytes <first>-<last>/<total>" and the body is the whole object exactly when it starts at zero and
// runs to total. Comparing the body length against total is what makes this safe against a truncated
// read: a short body reports false and simply goes unverified, rather than being hashed as if
// complete and failing as corruption.
//
// Anything unparseable returns false. This gates an integrity check, so an unrecognized header must
// mean "cannot confirm" — and false only forgoes verification, while a wrong true would report a
// fragment as corrupt and fail a legitimate read.
func wholeObjectResponse(contentRange string, bodyLen int64) bool {
	// A negative length is not a body. The current caller passes len(body) so it cannot happen, but
	// this function's answer decides whether an integrity check runs, and "the caller is correct" is
	// not the assumption to rest that on — FuzzWholeObjectResponse asserts the total function, not the
	// one reachable path.
	if bodyLen < 0 {
		return false
	}

	if contentRange == "" {
		return true
	}

	// RFC 9110 grammar, which admits no whitespace of its own:
	//
	//	Content-Range = "bytes" SP incl-range "/" complete-length
	//	              | "bytes" SP "*" "/" complete-length      ; the 416 form
	//
	// Parsed exactly, because leniency here is not free in either direction. Accepting shapes no
	// server sends makes the parse harder to reason about, and every tolerance is another way to
	// conclude "whole object" from a response that is not one. The outer trim is the sole concession,
	// and only because it costs nothing: header values reach here through http.Header, which has
	// trimmed them already.
	rest, ok := strings.CutPrefix(strings.TrimSpace(contentRange), "bytes ")
	if !ok {
		return false
	}

	slash := strings.LastIndex(rest, "/")
	if slash < 0 {
		return false
	}

	// "bytes */1234" is the 416 form: there is no satisfied range, so nothing was covered. Requiring
	// the range to begin at zero rejects it along with every genuine tail fragment.
	first, _, found := strings.Cut(rest[:slash], "-")
	if !found || first != "0" {
		return false
	}

	// complete-length is 1*DIGIT. strconv.ParseInt is looser — it accepts "+4096" and "-1" — and a
	// signed total has no meaning against a byte count, so require plain digits rather than letting a
	// sign through to be compared against a length.
	digits := rest[slash+1:]
	if digits == "" || strings.ContainsFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) {
		return false
	}

	total, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		// Overflows int64: no real object is that long, so this is not a response to draw conclusions
		// from.
		return false
	}

	return bodyLen == total
}

// PutObject stores an object in S3 with CargoShip optimization.
//
// meta is merged into the object's user metadata. The integrity keys this method computes —
// objectfs-sha256 and objectfs-original-size — are written last and win over anything meta supplies:
// they describe the bytes being uploaded, which the caller has not seen after compression.
func (b *Backend) PutObject(ctx context.Context, key string, data []byte, meta map[string]string) error {
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

	// Build the user metadata common to every upload path: the caller's attributes first, then the
	// integrity keys, which are this method's to own and must not be overridable. The original size is
	// recorded only for compressed objects, so HeadObject can report the size the kernel needs for
	// reads rather than the compressed ContentLength.
	objectMeta := make(map[string]string, len(meta)+2)
	for k, v := range meta {
		if strings.EqualFold(k, metaChecksum) || strings.EqualFold(k, metaOriginalSize) {
			// Not an error: a caller round-tripping metadata it read from HeadObject will carry these,
			// and refusing the write would make the obvious way to preserve attributes fail. They are
			// simply recomputed below.
			continue
		}
		objectMeta[k] = v
	}
	objectMeta[metaChecksum] = checksumHex
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
			maps.Copy(cargoMeta, objectMeta)
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

// SetObjectMetadata replaces key's user metadata in place, without rewriting its contents.
//
// S3 has no metadata-update operation, so this is a CopyObject onto the same key with
// MetadataDirective=REPLACE. The object's bytes are never transferred — the copy happens
// server-side — which is the whole reason a chmod does not read and rewrite a 10 GiB file.
//
// Every other stored property has to be restated, because REPLACE discards all of them and not just
// the metadata map. Content-Encoding is the one that matters for integrity: the read path dispatches
// decoding on the stored encoding and fails closed on an encoding it cannot handle, so a chmod that
// dropped the header would leave a compressed object permanently unreadable. Storage class is
// restated because the default is STANDARD, so omitting it would silently promote an object out of
// the tier the user is paying for — the same defect shape as L26.
func (b *Backend) SetObjectMetadata(ctx context.Context, key string, meta map[string]string) error {
	start := time.Now()
	defer func() {
		b.metricsCollector.RecordMetrics(time.Since(start), false)
	}()

	if !b.healthTracker.CanWrite("s3-writes") {
		state := b.healthTracker.GetState("s3-writes")
		return errors.NewError(errors.ErrCodeServiceUnavailable, "S3 write operations are unavailable").
			WithComponent("s3-backend").
			WithOperation("SetObjectMetadata").
			WithContext("health_state", state.String()).
			WithContext("bucket", b.bucket).
			WithContext("key", key)
	}

	// Read the object's current state first. Its metadata is merged under the caller's — the integrity
	// keys live there and are this backend's to preserve, and a caller setting a mode has no way to
	// recompute a checksum over bytes it never saw.
	head, err := b.headRaw(ctx, key)
	if err != nil {
		return err
	}

	merged := make(map[string]string, len(head.Metadata)+len(meta))
	maps.Copy(merged, head.Metadata)
	for k, v := range meta {
		if strings.EqualFold(k, metaChecksum) || strings.EqualFold(k, metaOriginalSize) {
			continue
		}
		merged[k] = v
	}

	client, err := b.clientManager.GetPooledClient()
	if err != nil {
		b.metricsCollector.RecordError(err)
		return fmt.Errorf("set metadata on %q: %w", key, err)
	}
	defer b.clientManager.ReturnPooledClient(client)

	// The source must be URL-escaped: S3 reads x-amz-copy-source as a path, so an unescaped key
	// containing a space, a "+", or a "?" names a different object or fails outright.
	//
	// StorageClass is copied through as-is, including empty: HeadObject omits the header for STANDARD,
	// and an empty StorageClass on the request means STANDARD too, so the round-trip is faithful at
	// both ends.
	input := &s3.CopyObjectInput{
		Bucket:            aws.String(b.bucket),
		Key:               aws.String(key),
		CopySource:        aws.String(url.PathEscape(b.bucket + "/" + key)),
		MetadataDirective: s3types.MetadataDirectiveReplace,
		Metadata:          merged,
		StorageClass:      head.StorageClass,
	}
	if enc := aws.ToString(head.ContentEncoding); enc != "" {
		input.ContentEncoding = aws.String(enc)
	}
	if ct := aws.ToString(head.ContentType); ct != "" {
		input.ContentType = aws.String(ct)
	}

	if _, err := client.CopyObject(ctx, input); err != nil {
		b.metricsCollector.RecordError(err)
		translated := b.translateError(err, "SetObjectMetadata", key)
		b.healthTracker.RecordError("s3-writes", translated)
		return translated
	}

	b.healthTracker.RecordSuccess("s3-writes")

	return nil
}

// headRaw is HeadObject without ObjectFS's interpretation of the result: the SDK output, whose
// Content-Encoding and StorageClass [Backend.SetObjectMetadata] must restate and which
// types.ObjectInfo does not carry.
func (b *Backend) headRaw(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	client, err := b.clientManager.GetPooledClient()
	if err != nil {
		b.metricsCollector.RecordError(err)
		return nil, fmt.Errorf("head %q: %w", key, err)
	}
	defer b.clientManager.ReturnPooledClient(client)

	out, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		b.metricsCollector.RecordError(err)
		return nil, b.translateError(err, "HeadObject", key)
	}

	return out, nil
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
	maps.Copy(info.Metadata, result.Metadata)

	// Populate Checksum from objectfs-sha256 metadata key (set on upload).
	// Empty string for objects written before this feature — backward compatible.
	if v, ok := lookupMetaValue(result.Metadata, metaChecksum); ok {
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
	v, ok := lookupMetaValue(metadata, metaOriginalSize)
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

// batchConcurrency is the number of concurrent operations GetObjects and PutObjects allow.
//
// It exists because `make(chan struct{}, n)` with n <= 0 is not a small semaphore, it is an
// unbuffered channel: the first `semaphore <- struct{}{}` blocks with no receiver and the batch never
// returns. That is not a hypothetical. Until v0.10.1 the mount path did not map PoolSize at all, so
// every mounted filesystem reached these two functions with PoolSize zero — a batch read or write
// hung the calling goroutine forever, holding whatever FUSE request was above it.
//
// NewBackend now defaults PoolSize, so this is the second line of defense rather than the first, and
// it is deliberately a floor rather than an assertion: a batch that runs one-at-a-time is slow, and a
// batch that deadlocks is a wedged filesystem.
func (b *Backend) batchConcurrency() int {
	if n := b.config.PoolSize; n > 0 {
		return n
	}

	return 1
}

// GetObjects fetches the named objects concurrently, up to batchConcurrency at a time.
//
// It returns every object it fetched together with an error naming every one it did not, so a
// non-nil error alongside a non-empty map is the normal way a partial batch is reported. See the
// contract on [types.Backend] for what a caller may assume.
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
	semaphore := make(chan struct{}, b.batchConcurrency())

	for _, key := range keys {
		go func(k string) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			data, err := b.GetObject(ctx, k, 0, 0)
			resultCh <- result{key: k, data: data, err: err}
		}(key)
	}

	var failures []error

	for range keys {
		res := <-resultCh
		if res.err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", res.key, res.err))

			continue
		}

		results[res.key] = res.data
	}

	// Every failure is reported, and the successes are returned alongside it. The old code returned a
	// nil error unless *every* key failed, which made a failed fetch indistinguishable from an object
	// that is not there — and since the map is the only other channel, a caller reading `results[k]`
	// got a nil slice either way and no way to tell "absent" from "the GET was throttled" (audit
	// finding H11). One key failing out of a thousand is the case that matters and the case that was
	// silent.
	//
	// A non-nil error with a non-empty map is deliberate, and it is the same shape io.Reader has: a
	// caller that wants partial results reads the map, and a caller that wants all-or-nothing checks
	// the error. errors.Join keeps every failure inspectable with errors.Is and errors.As rather than
	// flattening them into a string, so a caller can still ask whether the failures were all
	// ObjectNotFound.
	return results, stderr.Join(failures...)
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
	semaphore := make(chan struct{}, b.batchConcurrency())

	for key, data := range objects {
		go func(k string, d []byte) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			err := b.PutObject(ctx, k, d, nil)
			resultCh <- result{key: k, err: err}
		}(key, data)
	}

	var failures []error

	for range objects {
		res := <-resultCh
		if res.err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", res.key, res.err))
		}
	}

	// Joined rather than formatted into one string, so a caller can still ask what kind of failure it
	// was — errors.Is against an AccessDenied is actionable, "batch put failed for 3 objects: ..." is
	// not. The count is worth keeping in the message, since the join alone does not say how many of
	// the batch survived.
	if len(failures) > 0 {
		return fmt.Errorf("batch put failed for %d of %d objects: %w", len(failures), len(objects),
			stderr.Join(failures...))
	}

	return nil
}

// ListObjects lists objects in the bucket with the given prefix, following continuation tokens until
// the limit is met or the prefix is exhausted. A limit of zero or less means every object.
//
// Pagination is not an optimization. S3 caps a single ListObjectsV2 response at 1000 keys regardless
// of MaxKeys, and v0.10.0 issued exactly one request — so a directory with more than 1000 entries was
// silently truncated, and a truncated listing is not a cosmetic problem: the missing entries do not
// exist as far as readdir, and therefore as far as `cp -r` or `rm -r`, are concerned.
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

	// NOTE: Size below is the *stored* length from the list response. For compressed
	// objects that is the compressed size — ListObjectsV2 returns no user metadata,
	// so objectfs-original-size is unavailable without a HeadObject per entry. This
	// only affects sizes shown in directory listings; the kernel sizes reads from
	// Lookup → HeadObject, which does report the uncompressed size. Adding a
	// HeadObject fan-out here would cost one request per entry and is deliberately
	// not done.
	var (
		objects           []types.ObjectInfo
		continuationToken *string
	)

	for {
		input := &s3.ListObjectsV2Input{
			Bucket:            aws.String(b.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		}

		// Ask for only what is still wanted. Requesting more than the remainder would work — the extra
		// keys are discarded below — but it makes the last page needlessly large on a small limit, and
		// Lookup's existence probe passes limit 1 precisely to keep that page cheap.
		if limit > 0 {
			remaining := limit - len(objects)
			input.MaxKeys = aws.Int32(int32(min(remaining, maxKeysPerRequest)))
		}

		result, err := client.ListObjectsV2(ctx, input)
		if err != nil {
			b.metricsCollector.RecordError(err)
			return nil, b.translateError(err, "ListObjects", prefix)
		}

		for _, obj := range result.Contents {
			objects = append(objects, types.ObjectInfo{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified),
				ETag:         aws.ToString(obj.ETag),
				Metadata:     make(map[string]string),
			})

			if limit > 0 && len(objects) >= limit {
				return objects, nil
			}
		}

		// IsTruncated is the only authoritative "there is more". A page can be short of MaxKeys and
		// still be followed by another — S3 documents no minimum page size — so deciding from the count
		// would drop entries on exactly the buckets where it is hardest to notice.
		if !aws.ToBool(result.IsTruncated) || aws.ToString(result.NextContinuationToken) == "" {
			return objects, nil
		}
		continuationToken = result.NextContinuationToken
	}
}

// maxKeysPerRequest is the largest page S3 will return whatever MaxKeys asks for. Requesting more is
// not an error and not honored, which is why the single-request version silently truncated.
const maxKeysPerRequest = 1000

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

// parallelGetObject fans out a large read into concurrent range GETs and assembles them in order.
//
// # Why this is not just a loop with goroutines
//
// It was, and that made the largest reads the least protected ones in the backend. The serial path
// puts every GET behind the retryer, the circuit breaker, and the health tracker
// ([Backend.getObjectRange]); this path called executeWithAccelerationFallback directly — no retry,
// no breaker, no health signal — so a transient 500 on one chunk of a 1 GiB read failed the whole
// read that a single retry would have completed, and a genuine S3 outage was invisible to the
// component whose job is to notice one (audit findings D14 and M13). Every chunk now goes through
// getObjectRange, which is the one place that stack lives.
//
// Three further properties this owes the caller, none of which the original had:
//
//   - **A short assembly is an error, not a short buffer.** The old code joined whatever came back.
//     A chunk that answered 206 with fewer bytes than its range produced a buffer shorter than
//     totalSize, which the kernel presents as file content — with HeadObject still reporting the
//     full size, so the shortfall reads back as zeros. Silent truncation of user data is the worst
//     outcome available here, so each chunk's length is checked against the range asked for and the
//     total is checked against totalSize.
//   - **One ETag across every chunk.** N GETs of one object are N points in time. An overwrite
//     landing between the first and the last returns success for every range, with lengths that add
//     up, and assembles a splice of two generations of the file that never existed in the bucket.
//     Nothing downstream can detect that: the whole-object SHA-256 cannot be checked against an
//     assembled read (see [verifyChecksum]), so this comparison is the only integrity evidence a
//     large read has.
//   - **A failure cancels its siblings.** The old code returned on the first error and left the
//     remaining goroutines fetching chunks of an abandoned read to completion — up to
//     ParallelReadConcurrency × ReadChunkSize of egress billed for bytes nobody receives, and on a
//     wedged endpoint, goroutines outliving the request that spawned them.
//
// Routing every chunk through the reliability stack has a consequence worth naming, because getting
// it wrong makes this function worse than what it replaced: N chunks failing from one root cause must
// not record N health failures. s3-reads has an ErrorThreshold of 3 and a read of a large object has
// more chunks than that, so one shrunken object could take the component degraded and start refusing
// reads of objects that are perfectly readable.
//
// What keeps that from happening is that an abandoned chunk reports only its own truth — that it was
// canceled — rather than inheriting the failure that abandoned it. [errReadAbandoned] is the whole
// mechanism, and it is less obvious than it looks; the reasoning is there.
func (b *Backend) parallelGetObject(ctx context.Context, key string, offset, totalSize int64) ([]byte, error) {
	chunkSize := b.config.ReadChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultReadChunkSize
	}

	concurrency := b.config.ParallelReadConcurrency
	if concurrency <= 0 {
		concurrency = b.config.MultipartConcurrency
	}
	if concurrency <= 0 {
		concurrency = defaultParallelReadConcurrency
	}

	numChunks := (totalSize + chunkSize - 1) / chunkSize

	// errgroup gives the error half of what this needs: the first non-nil error wins and the rest are
	// discarded. The cancellation half is deliberately *not* errgroup.WithContext's.
	//
	// WithContext cancels with the failing chunk's error as the context's cause, and Go's HTTP client
	// reports context.Cause in preference to context.Canceled. So a sibling's interrupted body read
	// surfaces carrying the *first* chunk's error — verified: a chunk abandoned over a truncated object
	// returned that object's ErrCodeDataCorruption as though it were its own finding, which
	// translateError then had no arm for and classified as a service failure. One truncated object
	// degraded s3-reads, because the read reported its single finding up to numChunks times.
	//
	// Canceling with an explicit sentinel keeps the abandonment while making it say only what is true
	// of the abandoned request: it never got an answer. The first chunk's real error still reaches the
	// caller, through errgroup's own return value rather than through its siblings.
	group := new(errgroup.Group)
	group.SetLimit(concurrency)

	groupCtx, abandonSiblings := context.WithCancelCause(ctx)
	defer abandonSiblings(errReadAbandoned)

	chunks := make([][]byte, numChunks)
	etags := make([]string, numChunks)

	for i := range numChunks {
		group.Go(func() error {
			start := offset + i*chunkSize
			want := min(chunkSize, offset+totalSize-start)

			// getObjectRange, not a bare GetObject: retry, circuit breaker, health tracking,
			// metrics, and error translation all live in there, and duplicating any of them here is
			// how they drifted apart in the first place.
			read, err := b.getObjectRange(groupCtx, key, start, want)

			// A refused range is the same condition as a short one, reported differently because the
			// chunk fell entirely past the end rather than partly. S3 clamps a range that straddles
			// the end and answers 416 for one that starts at or beyond it, so an object that shrank
			// beneath the size this read was given produces both — short chunks at the boundary,
			// InvalidRange for every chunk after it, decided only by where the chunk lines up.
			//
			// Every range here was computed from totalSize, which the caller supplied, so there is no
			// other way to read a 416 here: unlike the serial path, this cannot be an ordinary read
			// past EOF that should return no bytes. That is what makes the reclassification safe —
			// translateError has to leave a refused range as a mere validation failure, because for
			// the serial path it usually is one, and only here is it known to mean the object shrank.
			if err != nil {
				// abandonSiblings, not errgroup's own cancellation: this is a plain Group, so nothing
				// else stops the remaining chunks. See the sentinel's rationale above.
				abandonSiblings(errReadAbandoned)

				if isInvalidRange(err) {
					return shrunkMidRead(key, b.bucket, start, want, err)
				}

				return err
			}

			// S3 clamps a range that runs past the end of the object rather than refusing it, so a
			// short answer here means the object is shorter than the read was told it was — an
			// overwrite between the HEAD and this GET, or a size the caller supplied that never
			// matched. Either way the assembled buffer would be short, and a short buffer is
			// indistinguishable from a file that ends there.
			if int64(len(read.data)) != want {
				abandonSiblings(errReadAbandoned)

				return shrunkMidRead(key, b.bucket, start, want, nil).
					WithDetail("returned_bytes", len(read.data))
			}

			chunks[i] = read.data
			etags[i] = read.etag

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	if err := sameGeneration(etags, key, b.bucket); err != nil {
		b.metricsCollector.RecordError(err)

		return nil, err
	}

	assembled := bytes.Join(chunks, nil)

	// The per-chunk checks above make this unreachable, which is the point of keeping it: it is the
	// invariant the caller actually depends on, and it costs one comparison against a read that
	// just transferred megabytes. Returning a buffer of the wrong length is silent data corruption
	// — the caller has no way to distinguish it from the file's real contents.
	if int64(len(assembled)) != totalSize {
		return nil, errors.NewError(errors.ErrCodeDataCorruption,
			"assembled parallel read is not the requested length").
			WithComponent("s3-backend").
			WithOperation("GetObject").
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithDetail("requested_bytes", totalSize).
			WithDetail("assembled_bytes", len(assembled)).
			WithDetail("chunk_count", numChunks)
	}

	b.costOptimizer.RecordAccess(key, totalSize)

	return assembled, nil
}

// errReadAbandoned is the cancellation cause for a chunk of a parallel read that was dropped because
// another chunk failed.
//
// It wraps context.Canceled rather than being a bare sentinel, and that is the whole point: Go's HTTP
// client reports context.Cause on an interrupted body read, so this is what the abandoned chunk's
// error chain carries — and errors.Is(err, context.Canceled) has to keep matching it, because that is
// what routes it to translateError's cancellation arm and thus to ErrCodeOperationCanceled, the one
// code the health tracker heals on rather than degrades.
var errReadAbandoned = fmt.Errorf("parallel read chunk abandoned after a sibling chunk failed: %w",
	context.Canceled)

// shrunkMidRead is the error for a chunk of a parallel read whose range the object could not
// satisfy, in either of the two forms that takes: fewer bytes than requested, or a refusal.
//
// One constructor for both because they are one condition — the object is shorter than the size this
// read was given — and which form it takes says nothing about severity, only about where the chunk
// happened to land relative to the new end. Reporting them as two different codes would mean a
// truncation detected or missed depending on chunk alignment.
//
// cause is the underlying API error for a refusal and nil for a short read, where there is no
// underlying error: the request succeeded and returned the wrong length.
func shrunkMidRead(key, bucket string, offset, want int64, cause error) *errors.ObjectFSError {
	err := errors.NewError(errors.ErrCodeDataCorruption,
		"parallel read chunk could not be satisfied: the object is shorter than the read's size").
		WithComponent("s3-backend").
		WithOperation("GetObject").
		WithContext("bucket", bucket).
		WithContext("key", key).
		WithDetail("range_offset", offset).
		WithDetail("requested_bytes", want).
		WithDetail("suggestion", "The object changed size while it was being read, or the size "+
			"supplied to the read was stale. Retry the read.")

	if cause != nil {
		err = err.WithCause(cause)
	}

	return err
}

// sameGeneration reports whether every chunk of a parallel read came from the same version of the
// object, by comparing the ETags the responses carried.
//
// An empty ETag from any chunk means the question cannot be answered, so it is not answered: a
// backend that does not return ETags on ranged GETs would otherwise make every large read fail. That
// is the same reasoning [verifyChecksum] applies to an object with no recorded hash — an absent piece
// of evidence is not evidence of corruption — and it is why this check is a supplement to the
// whole-object checksum rather than a replacement for it.
func sameGeneration(etags []string, key, bucket string) error {
	if len(etags) < 2 {
		return nil
	}

	first := etags[0]
	if first == "" {
		return nil
	}

	for i, tag := range etags[1:] {
		if tag == "" {
			return nil
		}

		if tag != first {
			return errors.NewError(errors.ErrCodeDataCorruption,
				"object changed while it was being read in parallel; refusing to return a mix of versions").
				WithComponent("s3-backend").
				WithOperation("GetObject").
				WithContext("bucket", bucket).
				WithContext("key", key).
				WithDetail("first_chunk_etag", first).
				WithDetail("differing_chunk", i+1).
				WithDetail("differing_chunk_etag", tag).
				WithDetail("suggestion", "Another writer replaced the object mid-read. Retry the read; "+
					"the assembled bytes would have been a splice of two versions that never existed.")
		}
	}

	return nil
}

// translateError turns an error from the AWS SDK into a classified [errors.ObjectFSError].
//
// The classification is not cosmetic. errors.IsServiceFailure reads the code to decide whether the
// health tracker degrades the component and the circuit breaker counts a trip, and
// errors.IsRetryableByDefault reads it to decide whether the retryer tries again — so a
// misclassified error either refuses reads of healthy objects or retries something that cannot
// succeed. The default arm is deliberately pessimistic (ErrCodeStorageRead: a service failure, not
// retryable), which makes adding an arm the way to fix a wrong classification.
func (b *Backend) translateError(err error, operation, key string) error {
	// Check for specific S3 error types and create rich error objects
	switch {
	case stderr.Is(err, context.Canceled), stderr.Is(err, context.DeadlineExceeded):
		// A withdrawn request, classified before anything else because the alternative is not merely
		// an imprecise message.
		//
		// Without this arm a cancellation falls through to the default and becomes
		// ErrCodeStorageRead, which errors.IsServiceFailure counts as a failure — so the health
		// tracker degrades s3-reads and the circuit breaker counts a trip toward opening it. Both
		// then refuse reads of objects that are perfectly readable, because something canceled a
		// read. And cancellation is not exceptional here: a FUSE interrupt, a killed reader, an
		// unmount, and a parallel read abandoning its siblings all arrive this way, routinely and in
		// bursts. ErrorThreshold is 3.
		//
		// ErrCodeOperationCanceled is the one code health.RecordError heals on, which is what makes
		// this the right code rather than merely a better message.
		return errors.NewError(errors.ErrCodeOperationCanceled, "S3 operation canceled").
			WithComponent("s3-backend").
			WithOperation(operation).
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithCause(err)

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

	case isInvalidRange(err):
		// A range S3 would not serve, which for the read path is ordinary rather than exceptional: a
		// caller sizes a read by its buffer, so the last read of any file asks for more than is there,
		// and a read at exactly EOF asks for a range that starts past the end. GetObject answers that
		// with no bytes and no error, which is correct — but the classification still matters, because
		// the health tracker and the breaker are fed inside getObjectRange, before the caller gets to
		// decide the read succeeded.
		//
		// Without this arm the default renders it ErrCodeStorageRead, a service failure. So reading to
		// the end of three files degrades s3-reads and moves the breaker toward open, and it happens on
		// the most ordinary read pattern there is. ErrCodeValidationFailed is a non-failure per
		// errors.IsServiceFailure, which is what stops that, and it is also honest: the request was
		// invalid for this object, and no retry changes that.
		return errors.NewError(errors.ErrCodeValidationFailed, "S3 refused the requested byte range").
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
				WithDetail("timeout_config", map[string]any{
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
	recorded, ok := lookupMetaValue(metadata, metaOriginalSize)
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

// verifyChecksum recomputes the SHA-256 of a whole object's uncompressed content and compares it
// against the objectfs-sha256 the writer recorded, returning an integrity error on any mismatch.
//
// # Why this exists
//
// v0.10.0 computed this hash on every single upload, stored it as user metadata, and surfaced it on
// HeadObject as ObjectInfo.Checksum — and then no read path anywhere ever compared it against bytes
// that came back. The one piece of stored evidence that what came out is what went in was written
// and never read. That is what makes this the guard the compression findings needed: a codec
// mismatch, a lost Content-Encoding header, a truncated body, a mangled multipart assembly, and
// bit-rot in the bucket all produce bytes that differ from what was hashed, and all of them were
// previously returned with a successful exit status.
//
// # What it deliberately does not cover
//
// A partial read. The hash is over the entire content, so there is nothing to check a fragment
// against without fetching the whole object — which is exactly the read amplification the read path
// was just fixed to stop doing. Verifying a 4 KiB read of a 10 GiB object would mean transferring
// 10 GiB. So a genuine fragment goes unverified, and stating that plainly is better than implying a
// guarantee that does not hold. Per-chunk checksums are the real fix and belong with the
// seekable-framing work, since both change the stored object's layout.
//
// Note that "partial" means the response covered less than the object, not that a Range header was
// sent. A read of a whole small file is a ranged request — its size is the kernel's buffer, not the
// file's length — and is verified, because the response came back complete. Callers must not read
// this as "small reads are checked and large ones are not": what is checked is a complete object,
// at any size, and the large-file random read is precisely the case that is not.
//
// An object with no recorded checksum verifies trivially. That is not a weakened check but the only
// possible behavior: objects written by aws s3 cp, by boto3, by a bucket that predates ObjectFS, or
// by any other tool carry no objectfs-sha256, and refusing to read them would make ObjectFS unable
// to read the buckets it exists to mount.
//
// A malformed recorded value is an error, not a skip. Unlike objectfs-original-size — where
// HeadObject falls back to ContentLength because a bad mode must not make a file unreadable — there
// is no safe fallback for a checksum. A value that is not 64 hex characters was not written by this
// code, and treating "I cannot tell whether this is corrupt" as "this is fine" is the exact reasoning
// that let the compression corruption ship.
func verifyChecksum(metadata map[string]string, data []byte, key string) error {
	recorded, ok := lookupMetaValue(metadata, metaChecksum)
	if !ok || recorded == "" {
		return nil
	}

	want, decodeErr := hex.DecodeString(recorded)
	if decodeErr != nil || len(want) != sha256.Size {
		return errors.NewError(errors.ErrCodeDataCorruption,
			"object records a malformed SHA-256; refusing to return content that cannot be verified").
			WithComponent("s3-backend").
			WithOperation("GetObject").
			WithContext("key", key).
			WithDetail("recorded_checksum", recorded).
			WithDetail("suggestion", "The objectfs-sha256 metadata value is not 64 hex characters, so it "+
				"was not written by ObjectFS. Remove or correct the metadata, or rewrite the object.")
	}

	got := sha256.Sum256(data)
	if bytes.Equal(got[:], want) {
		return nil
	}

	return errors.NewError(errors.ErrCodeDataCorruption,
		"object content does not match its recorded SHA-256").
		WithComponent("s3-backend").
		WithOperation("GetObject").
		WithContext("key", key).
		WithDetail("recorded_checksum", recorded).
		WithDetail("computed_checksum", hex.EncodeToString(got[:])).
		WithDetail("bytes_read", len(data)).
		WithDetail("suggestion", "The stored object differs from what was uploaded. Do not treat the "+
			"returned length as authoritative; restore the object from a version or backup.")
}

// lookupMetaValue finds a user-metadata key case-insensitively.
//
// S3 lower-cases user-metadata keys in transit, but the SDK's response map preserves whatever case
// the server sent: MinIO title-cases them and a Go http.Header round-trip canonicalizes to
// Objectfs-Sha256. A case-sensitive lookup therefore passes every unit test and then silently finds
// nothing against real storage — which for an integrity check means it stops checking without ever
// failing, the worst available outcome.
func lookupMetaValue(metadata map[string]string, key string) (string, bool) {
	if v, ok := metadata[key]; ok {
		return v, true
	}
	for k, v := range metadata {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
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

// GetAccessPatternCount returns the number of objects with a tracked access pattern.
//
// It delegates rather than taking len() of the map directly: the map is written from every reader
// goroutine when MonitorAccessPatterns is on, and a bare len() of a map being written concurrently
// is a race the runtime can abort the process for.
func (b *Backend) GetAccessPatternCount() int {
	return b.costOptimizer.PatternCount()
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
//
// # Every exit that is not a completed upload aborts it
//
// An initiated upload holds its parts in the bucket, billed at the storage rate, until it is
// completed or aborted. They are invisible: they do not appear in ListObjects, a HeadObject of the
// key reports the object absent, and nothing in ObjectFS ever called ListMultipartUploads, so an
// abandoned upload was undiscoverable from inside the filesystem and reapable only by an S3 lifecycle
// rule the operator had to know to write.
//
// The failure this fixes is the one where the leak is largest. The part-upload failure path aborted,
// but the Complete failure path did not — and Complete fails *after* every part has landed, so the
// case that leaked the whole object was the only case that leaked at all (audit finding H10). It also
// dropped the only record of the upload on the way out, via a deferred RemoveUpload, so nothing could
// clean up afterwards either.
//
// So the abort is a defer keyed on a completion flag rather than a call on each error path: a path
// that forgets to abort is the defect, and the only way to stop adding new ones is to make aborting
// the default. It runs on a fresh context for the same reason — the common cause of a failed upload
// is the caller's context being canceled, and an abort issued on that context cannot be sent, which
// would leak on exactly the failure that happens most.
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

	var completed bool

	defer func() {
		b.multipartManager.RemoveUpload(uploadID)

		if completed {
			return
		}

		b.multipartManager.MarkUploadFailed(uploadID)
		b.abandonMultipartUpload(ctx, key, uploadID)
	}()

	totalParts := CalculatePartCount(dataSize, chunkSize)
	b.logger.Debug("Multipart upload initiated", "upload_id", uploadID, "total_parts", totalParts)

	completedParts, totalBytesUploaded, err := b.uploadParts(ctx, key, uploadID, data, chunkSize, totalParts, uploadState)
	if err != nil {
		return fmt.Errorf("multipart upload failed: %w", err)
	}

	if err := b.completeMultipartUpload(ctx, key, uploadID, completedParts); err != nil {
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	completed = true

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
