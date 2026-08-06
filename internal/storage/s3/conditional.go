package s3

// conditional.go — the write half of S3 compare-and-swap: PutObjectIf, the header mapping that
// carries a precondition onto a request, the error mapping that brings the store's answer back, and
// the construction-time probe that establishes whether the endpoint evaluates preconditions at all.
//
// It is a separate file from backend.go because the reason this code exists is not "S3 has two more
// headers". A precondition is the only thing in this package whose *failure* is the intended outcome
// for its caller, and almost every mechanism the backend wraps a request in — the retryer, the
// circuit breaker, the health tracker — is built to treat a failed request as evidence of trouble.
// Getting a conditional write right is mostly a matter of keeping those three from reacting to a
// lost race, and that reasoning is easier to keep straight in one place than scattered through the
// unconditional paths.

import (
	"bytes"
	"context"
	stderr "errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/scttfrdmn/objectfs/pkg/errors"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// capabilityProbeKey is the key the conditional-write probe asserts against.
//
// Nothing is ever written to it. The probe asserts If-Match against a key it expects to be absent,
// so the only outcomes are "not found" (the store evaluated the header) and "success" (it did not) —
// and the success case is the one that would create the object, which is why the key is namespaced
// out of any plausible user path rather than being something like ".probe".
const capabilityProbeKey = ".objectfs-conditional-write-probe"

// capabilityProbeTimeout bounds the probe.
//
// It is short because the probe is on the construction path: a mount waiting on a hung endpoint
// should fail to come up rather than hang, and an endpoint that cannot answer one HEAD-sized request
// in this long is not one a coordination feature should be trusting anyway. A timeout leaves
// ConditionalWrite false, which is the fail-closed direction.
const capabilityProbeTimeout = 10 * time.Second

// Capabilities implements [types.CapabilityReporter], reporting what the endpoint in front of this
// process actually implements.
//
// Probed once, lazily, and cached — not re-probed per call. A conditional write is on a coordination
// path where an extra round trip per attempt is a real cost, and the answer cannot change under a
// running process: the endpoint is fixed at construction.
//
// Lazily rather than in NewBackend deliberately. Every backend pays construction, including the ones
// in tests and the ones that will never issue a conditional write, and a probe that ran there would
// put a request on the wire before the caller had asked for anything — turning a wrong endpoint into
// a startup failure for a feature nobody in that process is using.
func (b *Backend) Capabilities() types.BackendCapabilities {
	b.capsOnce.Do(func() { b.caps = b.probeConditionalWrite(context.Background()) })

	return b.caps
}

// probeConditionalWrite establishes by attempt whether the endpoint evaluates write preconditions.
//
// The probe is an If-Match against a key expected to be absent, and the *expected* answer is an
// error: NoSuchKey or NotFound. That shape is chosen over the more obvious "If-None-Match: * on an
// absent key, expect success" for two reasons. It writes nothing, so a probe on the construction path
// cannot leave an object behind or race a real writer for the same key. And it is the direction that
// catches the dangerous failure — a store that accepts conditional headers and ignores them answers
// this probe with a *success*, having created an empty object, which is exactly the endpoint a
// configuration flag would have called capable.
//
// Anything else — a timeout, a network failure, an unexpected code — leaves the capability false. A
// probe that could not establish the answer has not established it, and the fail-closed direction is
// the only safe default for a mechanism whose whole purpose is mutual exclusion.
func (b *Backend) probeConditionalWrite(ctx context.Context) types.BackendCapabilities {
	ctx, cancel := context.WithTimeout(ctx, capabilityProbeTimeout)
	defer cancel()

	client, err := b.clientManager.GetPooledClient()
	if err != nil {
		return types.BackendCapabilities{
			ConditionalWriteDetail: fmt.Sprintf("could not obtain an S3 client to probe with: %v", err),
		}
	}
	defer b.clientManager.ReturnPooledClient(client)

	// An ETag no object can have. S3 ETags are hex — for a single-part object the MD5 of its bytes,
	// for a multipart one a hex digest with a "-N" part-count suffix — so a value with a non-hex
	// character cannot match anything, whatever is at the key. This matters because the probe key is
	// only *expected* to be absent: if a previous run of a broken endpoint left an object there, an
	// If-Match that could match would report the endpoint capable by accident.
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(capabilityProbeKey),
		Body:          bytes.NewReader(nil),
		ContentLength: aws.Int64(0),
		IfMatch:       aws.String(`"objectfs-conditional-write-probe-no-object-can-match-this"`),
	})

	switch {
	case err == nil:
		// The write succeeded, which means the If-Match header was ignored — and an empty object now
		// exists at the probe key. Clean it up on a best-effort basis: the capability is already
		// established as absent, and a failed cleanup does not change that answer.
		_, delErr := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(b.bucket),
			Key:    aws.String(capabilityProbeKey),
		})
		if delErr != nil {
			b.logger.Warn("could not remove the object left behind by an endpoint that ignored a "+
				"conditional write header",
				"key", capabilityProbeKey,
				"error", delErr)
		}

		b.logger.Error("this endpoint accepted a conditional write header and ignored it; "+
			"treating conditional writes as unsupported",
			"endpoint", b.config.Endpoint,
			"bucket", b.bucket)

		return types.BackendCapabilities{
			ConditionalWriteDetail: "the endpoint accepted an If-Match that could not possibly match and " +
				"performed the write anyway, so it does not evaluate preconditions",
		}

	case isNotFoundCode(err):
		// The expected answer: the header was evaluated, and the assertion failed because the key is
		// absent. Note it is 404 rather than 412 — verified behavior, and the distinction the
		// NoSuchKey arm of translateConditionalError preserves.
		return types.BackendCapabilities{ConditionalWrite: true}

	case isPreconditionFailedCode(err):
		// The header was evaluated against an object that does exist at the probe key — a leftover
		// from a previous probe against a broken endpoint. The capability is established either way:
		// something evaluated the assertion and declined the write.
		return types.BackendCapabilities{ConditionalWrite: true}

	default:
		return types.BackendCapabilities{
			ConditionalWriteDetail: fmt.Sprintf("the conditional-write probe could not establish an answer, "+
				"so preconditions are treated as unsupported: %v", err),
		}
	}
}

// isNotFoundCode reports whether err is S3 saying the key is not there.
//
// Three checks rather than one because the SDK models absence three ways depending on the operation:
// NoSuchKey for GET-shaped requests, NotFound for HEAD-shaped ones, and a bare API error carrying the
// code for everything the SDK has no typed shape for — which includes the conditional PutObject this
// probe issues. Checking only the typed shapes is the mistake DeleteObject made (audit finding M17).
func isNotFoundCode(err error) bool {
	if err == nil {
		return false
	}

	var apiErr smithy.APIError
	if stderr.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}

	return false
}

// isPreconditionFailedCode reports whether err is S3 declining a write because a precondition did not
// hold — HTTP 412 with a PreconditionFailed code.
func isPreconditionFailedCode(err error) bool {
	if err == nil {
		return false
	}

	var apiErr smithy.APIError
	if stderr.As(err, &apiErr) {
		return apiErr.ErrorCode() == "PreconditionFailed"
	}

	return false
}

// conditionalCapable is one of the request types that can carry a precondition. PutObject is where
// a small conditional write lands; CompleteMultipartUpload is where a large one does, since parts
// are not an object until they are assembled.
type conditionalCapable interface {
	*s3.PutObjectInput | *s3.CompleteMultipartUploadInput
}

// applyPrecondition sets the HTTP conditional headers cond describes on input.
//
// A zero cond sets nothing, which makes this safe to call from the unconditional paths and is why
// putObjectMultipart takes a Precondition rather than growing a second function. Validation is the
// caller's: by the time a request is being built the decision to make it conditional has been made,
// and rejecting a bad precondition here would be a failure discovered after the body was compressed.
func applyPrecondition[T conditionalCapable](input T, cond types.Precondition) {
	switch in := any(input).(type) {
	case *s3.PutObjectInput:
		if cond.Absent {
			in.IfNoneMatch = aws.String("*")
		}
		if cond.ETag != "" {
			in.IfMatch = aws.String(cond.ETag)
		}
	case *s3.CompleteMultipartUploadInput:
		if cond.Absent {
			in.IfNoneMatch = aws.String("*")
		}
		if cond.ETag != "" {
			in.IfMatch = aws.String(cond.ETag)
		}
	}
}

// translateConditionalError classifies an error from a request that carried a precondition.
//
// It runs *before* translateError rather than as two more arms inside it, because the same HTTP
// status means different things depending on whether the request asserted anything: a 412 can only
// arise from a precondition, and translateError is called from every unconditional path in this
// package where that arm would be dead code inviting the reader to wonder when it fires.
//
// Matching is on the API error *code*, never on message text. That is the idiom isInvalidRange
// established, and the reason is audit finding L27: substring-matching a message made unrelated
// failures look like the one being matched for.
func (b *Backend) translateConditionalError(err error, operation, key string, cond types.Precondition) error {
	if err == nil {
		return nil
	}

	var apiErr smithy.APIError
	if !stderr.As(err, &apiErr) {
		return b.translateError(err, operation, key)
	}

	switch apiErr.ErrorCode() {
	case "PreconditionFailed":
		// The store evaluated the assertion and it did not hold. Nothing was written.
		//
		// ErrCodePreconditionFailed is in errors.IsServiceFailure's non-failure set, which is what
		// keeps this off the health tracker and out of the breaker's failure count — see the comment
		// there. It is also absent from the retryable set, because the answer is definitive: the
		// object's state is not what the caller asserted, and asking again cannot change that. Only a
		// caller that re-reads the state has anything new to say.
		return errors.NewError(errors.ErrCodePreconditionFailed, "S3 declined the write: the precondition did not hold").
			WithComponent("s3-backend").
			WithOperation(operation).
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithContext("asserted_absent", strconv.FormatBool(cond.Absent)).
			WithContext("asserted_etag", cond.ETag).
			WithDetail("suggestion", "Re-read the object's current state and decide again; do not retry this write.").
			WithCause(stderr.Join(types.ErrPreconditionFailed, err))

	case "ConditionalRequestConflict":
		// The write raced a delete or another conditional write. Distinct from the above because the
		// caller's view of the state may still be current, so the same write may simply succeed —
		// which is why ErrCodeConditionalConflict is in retry.DefaultConfig's retryable set and
		// ErrCodePreconditionFailed is not.
		return errors.NewError(errors.ErrCodeConditionalConflict, "S3 reported a conflicting concurrent request").
			WithComponent("s3-backend").
			WithOperation(operation).
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithCause(stderr.Join(types.ErrConditionalConflict, err))

	case "NoSuchKey", "NotFound":
		// An If-Match against a key that is not there. S3 answers 404 rather than 412 here — verified
		// against both real S3 and the emulator, not inferred from the specification — and preserving
		// that distinction is the point of this arm: a lost race means recompute and retry, while a
		// vanished object means the state being updated no longer exists and a CAS loop that treats
		// the two alike will spin forever against a key nobody is going to recreate.
		//
		// Built here rather than delegated to translateError, which cannot classify it. That function
		// matches absence with isErrorType[*s3types.NoSuchKey] — a *typed* shape — and the SDK does not
		// model NoSuchKey among PutObject's errors, because an unconditional PutObject has no way to
		// produce one. So a conditional PutObject's 404 arrives as a bare API error carrying the code,
		// falls through translateError's arms to the pessimistic default, and becomes
		// ErrCodeStorageRead: a service failure that degrades s3-writes and moves the breaker toward
		// open, for a key that simply is not there. Verified by execution, which is how it was found.
		return errors.NewError(errors.ErrCodeObjectNotFound, "object not found").
			WithComponent("s3-backend").
			WithOperation(operation).
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithContext("asserted_etag", cond.ETag).
			WithDetail("suggestion", "The object being updated no longer exists. This is not a lost race: "+
				"re-reading will not produce a current ETag, because there is no object.").
			WithCause(err)

	default:
		return b.translateError(err, operation, key)
	}
}

// PutObjectIf implements [types.Backend].
//
// The contract is on the interface; what is worth recording here is the machinery this deliberately
// does *not* reuse from PutObject, and why.
//
// It is not wrapped in b.retryer. The retryer decides what to retry from an error code, and the two
// outcomes a conditional write exists to produce sit on opposite sides of that decision: a
// precondition failure must never be retried, and a conflict may be. Leaving the retry to the caller
// is the honest arrangement, because a CAS caller cannot retry a *write* anyway — it has to re-read
// the state and recompute the bytes first, which is a loop only it can run. A retryer resending the
// same body against the same asserted ETag would be spending requests to be told the same thing.
//
// It does run inside the circuit breaker and does feed the health tracker, on success and on genuine
// failures. Those two mechanisms are about whether S3 is reachable, and a conditional write is
// evidence about that like any other request. What they must not see is a lost race, and they do not:
// ErrCodePreconditionFailed is a non-failure per errors.IsServiceFailure, which both consult. Without
// that, N contenders for one lease would take writes offline for all of them — ErrorThreshold is 3 —
// under exactly the contention the precondition is there to arbitrate.
func (b *Backend) PutObjectIf(ctx context.Context, key string, data []byte, meta map[string]string,
	cond types.Precondition,
) (string, error) {
	start := time.Now()
	defer func() {
		b.metricsCollector.RecordMetrics(time.Since(start), false)
	}()

	// The precondition is checked before anything else, including the health gate. An unusable
	// precondition is a bug in the calling code and says nothing about the store, so it should report
	// the same way whether or not S3 happens to be reachable at that moment.
	if err := cond.Validate(); err != nil {
		return "", errors.NewError(errors.ErrCodeValidationFailed, "unusable precondition for a conditional write").
			WithComponent("s3-backend").
			WithOperation("PutObjectIf").
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithContext("asserted_absent", strconv.FormatBool(cond.Absent)).
			WithContext("asserted_etag", cond.ETag).
			WithDetail("suggestion", "Assert absence or an ETag, not both and not neither; "+
				"an unconditional write is PutObject.").
			WithCause(err)
	}

	// A backend whose endpoint does not evaluate preconditions refuses, rather than writing.
	//
	// This is the fail-closed direction and the whole reason the probe exists. An endpoint that
	// accepts a conditional header and ignores it is indistinguishable from one that honors it, from
	// every angle except the outcome of a race — so falling back to an unconditional write would turn
	// "exactly one node performs this transition" into "every node does", silently, at the moment it
	// matters most. The read path already sets this precedent by refusing a Content-Encoding it cannot
	// decode rather than handing back bytes it cannot interpret.
	if caps := b.Capabilities(); !caps.ConditionalWrite {
		return "", errors.NewError(errors.ErrCodeOperationFailed, "this endpoint does not evaluate write preconditions").
			WithComponent("s3-backend").
			WithOperation("PutObjectIf").
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithContext("endpoint", b.config.Endpoint).
			WithContext("probe_detail", caps.ConditionalWriteDetail).
			WithDetail("suggestion", "Conditional writes are unavailable against this endpoint. A feature "+
				"needing mutual exclusion must refuse to start rather than proceed unguarded.").
			WithCause(types.ErrNotSupported)
	}

	if !b.healthTracker.CanWrite("s3-writes") {
		state := b.healthTracker.GetState("s3-writes")
		return "", errors.NewError(errors.ErrCodeServiceUnavailable, "S3 write operations are unavailable").
			WithComponent("s3-backend").
			WithOperation("PutObjectIf").
			WithContext("health_state", state.String()).
			WithContext("bucket", b.bucket).
			WithContext("key", key).
			WithDetail("suggestion", "System is in read-only mode. Writes will be available once service recovers.")
	}

	// Tier selection and validation, matching PutObject. A conditional write is a write, and an
	// object that arrives by way of a precondition is billed and constrained like any other — so the
	// small-object diversion and the tier validator apply for the same reasons they do there.
	effectiveTier := b.currentTier
	if b.config.CostOptimization.SmallObjectsOnStandard {
		effectiveTier = b.costOptimizer.HandleStandardTierOverhead(key, int64(len(data)))
	}

	if err := b.tierValidator.ValidateWriteToTier(key, int64(len(data)), effectiveTier); err != nil {
		b.metricsCollector.RecordError(err)
		return "", fmt.Errorf("tier validation failed: %w", err)
	}

	uploadData, contentEncoding, objectMeta, err := b.prepareUpload(key, data, meta)
	if err != nil {
		return "", err
	}

	var etag string

	breaker := b.circuitManager.GetBreaker("s3-put")

	err = breaker.ExecuteWithContext(ctx, func(ctx context.Context) error {
		etag = ""

		if int64(len(uploadData)) >= b.config.MultipartThreshold {
			b.logger.Debug("Using conditional multipart upload for large object",
				"key", key,
				"size", len(uploadData),
				"threshold", b.config.MultipartThreshold)

			mpETag, mpErr := b.putObjectMultipart(ctx, key, uploadData, effectiveTier, contentEncoding,
				objectMeta, cond)
			if mpErr != nil {
				// putObjectMultipart's inner calls have already translated and recorded. Health is fed
				// here rather than there so that a precondition failure reaching this point is offered
				// to the tracker as the non-failure it is — RecordError forwards a non-failure to
				// RecordSuccess — instead of being counted against s3-writes.
				b.healthTracker.RecordError("s3-writes", mpErr)
				return mpErr
			}

			etag = mpETag
			return nil
		}

		input := &s3.PutObjectInput{
			Bucket: aws.String(b.bucket),
			Key:    aws.String(key),
			// bytes.NewReader, not any io.Reader over the same bytes. The SDK computes an input header
			// checksum and rewinds the body to do it, so an unseekable reader fails with "unseekable
			// stream is not supported without TLS and trailing checksum" before a request is ever sent.
			// That error names TLS and reads like an endpoint limitation; it is not.
			Body:          bytes.NewReader(uploadData),
			ContentLength: aws.Int64(int64(len(uploadData))),
			ContentType:   aws.String(b.detectContentType(key)),
			StorageClass:  ConvertTierToStorageClass(effectiveTier),
			Metadata:      objectMeta,
		}
		if contentEncoding != "" {
			input.ContentEncoding = aws.String(contentEncoding)
		}

		applyEncryptionPut(input, b.config.Encryption)
		applyPrecondition(input, cond)

		return b.executeWithAccelerationFallback(ctx, "PutObjectIf", func(client *s3.Client) error {
			out, putErr := client.PutObject(ctx, input)
			if putErr != nil {
				b.metricsCollector.RecordError(putErr)
				translatedErr := b.translateConditionalError(putErr, "PutObjectIf", key, cond)
				b.healthTracker.RecordError("s3-writes", translatedErr)

				return translatedErr
			}

			etag = aws.ToString(out.ETag)
			b.metricsCollector.RecordBytesUploaded(int64(len(uploadData)))
			b.healthTracker.RecordSuccess("s3-writes")

			return nil
		})
	})
	if err != nil {
		return "", err
	}

	return etag, nil
}
