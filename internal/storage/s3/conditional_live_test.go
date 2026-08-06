//go:build integration

package s3_test

// Conditional writes against real AWS S3.
//
// The hermetic suite in conditional_test.go runs against internal/testaws, which is a real S3 endpoint
// over real HTTP — but it is substrate's implementation of S3's semantics, not S3's. Everything
// PutObjectIf is built on is a claim about the store's behavior rather than about ObjectFS:
//
//   - that If-None-Match: * on an existing key is 412 and writes nothing,
//   - that If-Match against an *absent* key is 404 rather than 412, which is the distinction the whole
//     CAS series depends on and the one most likely to differ between an emulator and the real thing,
//   - that a precondition on CompleteMultipartUpload is evaluated at all, and
//   - that S3 — not any of the contenders — arbitrates a genuine race between concurrent writers.
//
// An emulator agreeing with all four is necessary and not sufficient. That is why the issue asks for
// this file specifically, and why the assertions here duplicate the hermetic ones rather than testing
// something else: the point is the endpoint, not the coverage.
//
// Run with:
//
//	AWS_PROFILE=aws AWS_REGION=us-west-2 go test -race -tags=integration ./internal/storage/s3/
//
// Cost is a handful of PUT requests plus, for the multipart case, ~12 MiB transferred and held for the
// length of the run. The bucket is created and removed by the test.

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	objerrors "github.com/scttfrdmn/objectfs/pkg/errors"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// TestLiveConditionalWriteCapabilityIsDetectedOnRealS3 is the probe against the endpoint whose behavior
// it was designed around.
//
// This is not a redundant check on a hermetic test. The probe asserts an If-Match that cannot match
// against a key expected to be absent, and reads a 404 as proof the header was evaluated — a shape
// chosen because it writes nothing. If real S3 answered that with anything else, every conditional
// write in the process would refuse with ErrNotSupported and the fail-closed direction would make the
// whole feature quietly unavailable in production while the emulator suite stayed green.
func TestLiveConditionalWriteCapabilityIsDetectedOnRealS3(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client, bucket := liveBucket(t, ctx)

	backend := liveBackend(t, ctx, bucket)

	caps := backend.Capabilities()
	if !caps.ConditionalWrite {
		t.Fatalf("the probe reported real S3 does not evaluate write preconditions: %s\n"+
			"Every PutObjectIf in the process refuses when this is false, so the CAS series would be "+
			"unavailable against AWS while the emulator suite passed", caps.ConditionalWriteDetail)
	}
	if caps.ConditionalWriteDetail != "" {
		t.Errorf("ConditionalWriteDetail = %q, want empty when the capability is present",
			caps.ConditionalWriteDetail)
	}

	// The probe wrote nothing. It runs on the construction path of every mount, so an object left at
	// this key would appear in every user's bucket — and it would be the object whose absence the probe
	// asserts, so the second mount would read a different answer than the first.
	if _, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(s3.CapabilityProbeKeyForTest),
	}); err == nil {
		t.Errorf("the capability probe left an object at %q", s3.CapabilityProbeKeyForTest)
	}
}

// TestLiveIfNoneMatchDeclinesAnExistingKeyAndStoresNothing is the lease-acquisition primitive, both
// directions, on real S3.
//
// The stored bytes are asserted rather than only the error: an implementation that wrote the object and
// then reported 412 would satisfy an error-only check while having overwritten the lease-holder's value
// with the loser's.
func TestLiveIfNoneMatchDeclinesAnExistingKeyAndStoresNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client, bucket := liveBucket(t, ctx)
	backend := liveBackend(t, ctx, bucket)

	const key = "live/cas/lease"

	first, err := backend.PutObjectIf(ctx, key, []byte("node-a"), nil, types.Precondition{Absent: true})
	if err != nil {
		t.Fatalf("PutObjectIf asserting absence of a key that does not exist: %v", err)
	}
	if first == "" {
		t.Error("the successful conditional write reported no ETag; a CAS loop has nothing to continue from")
	}

	_, err = backend.PutObjectIf(ctx, key, []byte("node-b"), nil, types.Precondition{Absent: true})
	if !stderrors.Is(err, types.ErrPreconditionFailed) {
		t.Fatalf("PutObjectIf asserting absence of an existing key: err = %v, want ErrPreconditionFailed", err)
	}

	if got := getAll(t, ctx, client, bucket, key); !bytes.Equal(got, []byte("node-a")) {
		t.Errorf("the stored object is %q after a declined write, want %q unchanged", got, "node-a")
	}
}

// TestLiveIfMatchSucceedsOnTheCurrentETagAndFailsOnAStaleOne is compare-and-swap proper.
//
// The ETag returned by the first write is used directly, with no HeadObject in between. That is as much
// the property under test as the outcomes are: a loop that had to re-HEAD between iterations would
// double the request count and reopen the window the precondition closes.
func TestLiveIfMatchSucceedsOnTheCurrentETagAndFailsOnAStaleOne(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client, bucket := liveBucket(t, ctx)
	backend := liveBackend(t, ctx, bucket)

	const key = "live/cas/state"

	v1, err := backend.PutObjectIf(ctx, key, []byte("v1"), nil, types.Precondition{Absent: true})
	if err != nil {
		t.Fatalf("initial conditional write: %v", err)
	}

	v2, err := backend.PutObjectIf(ctx, key, []byte("v2"), nil, types.Precondition{ETag: v1})
	if err != nil {
		t.Fatalf("PutObjectIf with the ETag real S3 just returned: %v\n"+
			"If this fails on quoting or on the multipart \"-N\" suffix, every CAS loop spins forever "+
			"against state it is reading correctly", err)
	}
	if v2 == v1 {
		t.Errorf("the ETag is still %q after writing different bytes; a CAS loop using it could not "+
			"detect its own write", v2)
	}

	// v1 is now stale: someone else — here, ourselves — moved the object on.
	_, err = backend.PutObjectIf(ctx, key, []byte("v2-by-us"), nil, types.Precondition{ETag: v1})
	if !stderrors.Is(err, types.ErrPreconditionFailed) {
		t.Fatalf("PutObjectIf with a stale ETag: err = %v, want ErrPreconditionFailed", err)
	}

	if got := getAll(t, ctx, client, bucket, key); !bytes.Equal(got, []byte("v2")) {
		t.Errorf("the stored object is %q after a declined write, want %q intact", got, "v2")
	}
}

// TestLiveIfMatchOnAnAbsentKeyIsNotFoundNotPreconditionFailed is the single most important assertion in
// this file.
//
// S3 answers 404 rather than 412 for an If-Match against a key that is not there, and everything
// downstream depends on the two being distinguishable: a lost race means re-read and retry, while a
// vanished object means the state being updated no longer exists, and a CAS loop that treats the second
// as the first spins forever against a key nobody is going to recreate.
//
// This is the claim least safe to take from an emulator, and it is also the claim that exposed a real
// defect: the SDK does not model NoSuchKey among PutObject's typed errors, so the 404 arrives as a bare
// API error, falls through translateError's typed arms to the pessimistic default, and became
// ErrCodeStorageRead — a service failure degrading s3-writes for a key that is simply absent. Finding
// that needed execution, not reading, which is the argument for this file existing.
func TestLiveIfMatchOnAnAbsentKeyIsNotFoundNotPreconditionFailed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, bucket := liveBucket(t, ctx)
	backend := liveBackend(t, ctx, bucket)

	_, err := backend.PutObjectIf(ctx, "live/cas/never-written", []byte("v1"), nil,
		types.Precondition{ETag: `"0123456789abcdef0123456789abcdef"`})
	if err == nil {
		t.Fatal("PutObjectIf asserting an ETag on an absent key succeeded against real S3; nothing was " +
			"excluded, and the object now exists with a precondition that could not have held")
	}

	if stderrors.Is(err, types.ErrPreconditionFailed) {
		t.Fatalf("real S3's answer to If-Match on an absent key was reported as ErrPreconditionFailed: %v\n"+
			"A caller cannot then tell a lost race from a deleted object, so a CAS loop retries forever "+
			"against a key that is gone", err)
	}

	var objErr *objerrors.ObjectFSError
	if !stderrors.As(err, &objErr) {
		t.Fatalf("err = %v (%T), want an *errors.ObjectFSError", err, err)
	}
	if objErr.Code != objerrors.ErrCodeObjectNotFound {
		t.Errorf("err code = %q, want %q. STORAGE_READ here means the 404 fell through to "+
			"translateError's pessimistic default, degrading s3-writes for an absent key",
			objErr.Code, objerrors.ErrCodeObjectNotFound)
	}
}

// TestLiveConcurrentAbsentWritersElectExactlyOne is the property the whole coordination series rests
// on, arbitrated by S3 itself.
//
// The hermetic version of this test proves substrate serializes conditional writes. This one proves AWS
// does — across a real network, with real request ordering, and with contenders whose requests genuinely
// overlap. A lease, a tier transition performed by one node, and the executeStrongConsistency
// replacement all reduce to exactly this: if two writers can win, every guarantee built above it is
// void.
func TestLiveConcurrentAbsentWritersElectExactlyOne(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client, bucket := liveBucket(t, ctx)
	backend := liveBackend(t, ctx, bucket)

	// Warmed before the gate opens. Capabilities probes once through a sync.Once, and letting eight
	// goroutines arrive at an unprobed backend would have seven of them blocked on the probe's round
	// trip — so their conditional writes would be staggered by it rather than concurrent, and the test
	// would be measuring the Once instead of S3.
	if caps := backend.Capabilities(); !caps.ConditionalWrite {
		t.Fatalf("real S3 reported as not evaluating preconditions: %s", caps.ConditionalWriteDetail)
	}

	const (
		key        = "live/cas/contended-lease"
		contenders = 8
	)

	var (
		start   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		winners []string
		losers  int
		other   []error
	)

	start.Add(1)
	for i := range contenders {
		done.Add(1)
		go func() {
			defer done.Done()

			// Every goroutine blocks here so the requests go out together. Staggered writes would let
			// each contender observe the previous winner's object and be declined for a reason that has
			// nothing to do with S3 arbitrating a race.
			start.Wait()

			value := fmt.Sprintf("node-%d", i)
			_, err := backend.PutObjectIf(ctx, key, []byte(value), nil, types.Precondition{Absent: true})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, value)
			case stderrors.Is(err, types.ErrPreconditionFailed):
				losers++
			default:
				other = append(other, err)
			}
		}()
	}

	start.Done()
	done.Wait()

	// ConditionalRequestConflict is not folded into the loser count. S3 returns 409 when two conditional
	// writes to the same key overlap closely enough that it cannot say which came first, and it is
	// retryable rather than definitive — so a run that produced them has not established exactly-one and
	// should say so rather than quietly counting them as losses.
	if len(other) > 0 {
		t.Fatalf("%d contender(s) failed for reasons other than losing the race: %v", len(other), other)
	}
	if len(winners) != 1 {
		t.Fatalf("%d contenders produced %d winners (%v), want exactly 1. S3 — not any of them — decides "+
			"which proceeds, and everything downstream depends on it deciding once",
			contenders, len(winners), winners)
	}
	if losers != contenders-1 {
		t.Errorf("losers = %d, want %d", losers, contenders-1)
	}

	// The winner's bytes are the ones stored. A run where one write reported success while a different
	// contender's value landed would satisfy every count above.
	if got := getAll(t, ctx, client, bucket, key); string(got) != winners[0] {
		t.Errorf("the stored object is %q but %q reported winning", got, winners[0])
	}
}

// TestLiveConditionalMultipartWriteIsStillConditional covers the branch where the assertion travels on a
// different request than the data.
//
// Above MultipartThreshold a precondition can only be evaluated at CompleteMultipartUpload, because
// parts are not an object until they are assembled. So a large conditional write is conditional by a
// second mechanism, and if S3 did not evaluate the header there — or if ObjectFS failed to send it — the
// write would simply succeed: every contender would upload its parts, every Complete would land, and the
// lease would belong to whoever finished last.
//
// It also asserts no upload is left open. A losing conditional multipart write has already transferred
// every part, so this is the case where an unaborted upload leaks the most, and the parts are billed
// while being invisible to ListObjects.
func TestLiveConditionalMultipartWriteIsStillConditional(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client, bucket := liveBucket(t, ctx)

	// Real part sizes: S3 requires every non-final part to be at least 5 MiB, so 12 MiB in 5 MiB parts is
	// the smallest legal three-part upload.
	const (
		size      = 12 << 20
		partSize  = 5 << 20
		threshold = 5 << 20
	)

	backend := liveBackend(t, ctx, bucket, func(cfg *s3.Config) {
		cfg.MultipartThreshold = threshold
		cfg.MultipartChunkSize = partSize
	})

	const key = "live/cas/big-lease"

	held := deterministic(size)
	winnerETag, err := backend.PutObjectIf(ctx, key, held, nil, types.Precondition{Absent: true})
	if err != nil {
		t.Fatalf("conditional multipart write to an absent key: %v", err)
	}
	if winnerETag == "" {
		t.Error("the successful conditional multipart write reported no ETag")
	}

	// The multipart path really ran, established from the object rather than from a request count real S3
	// does not offer: an object assembled from N parts has an ETag of the form "<hex>-N".
	if !bytes.Contains([]byte(winnerETag), []byte("-3")) {
		t.Errorf("the ETag is %s, not the \"<hex>-3\" form of an object assembled from three parts. The "+
			"write took the single-PUT path, so this test proves nothing about the multipart branch",
			winnerETag)
	}

	contender := deterministic(size)
	contender[0] ^= 0xff

	_, err = backend.PutObjectIf(ctx, key, contender, nil, types.Precondition{Absent: true})
	if !stderrors.Is(err, types.ErrPreconditionFailed) {
		t.Fatalf("a %d-byte conditional write over an existing key: err = %v, want ErrPreconditionFailed",
			size, err)
	}

	if got := getAll(t, ctx, client, bucket, key); !bytes.Equal(got, held) {
		t.Errorf("the stored object changed after a declined multipart write: %d bytes, want the "+
			"original %d", len(got), len(held))
	}

	if ids := openUploads(t, ctx, client, bucket); len(ids) != 0 {
		t.Errorf("after a declined conditional multipart write %d upload(s) are still open: %v. Every "+
			"part had already been transferred, so this is where a leak costs the most", len(ids), ids)
	}
}
