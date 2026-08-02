package s3_test

// Audit finding P-7: the encryption that was configured and never requested.
//
// v0.10.0 shipped `security.encryption.at_rest` defaulting to **true**, read by nothing. A grep for
// ServerSideEncryption, SSEKMS, SSECustomer, or aws:kms across the tree returned zero non-test hits
// while OBJECTFS.md documented a `kms_key:` ARN in that block. Every object went to S3 with no
// encryption header.
//
// These tests assert on the headers the endpoint received, and that is the whole design of the file.
// The substrate emulator does not model server-side encryption at all — zero hits for
// ServerSideEncryption or x-amz-server-side-encryption in substrate v0.82.0 — so an object written
// with the header reads back byte-identical to one written without it. Which means:
//
//   - asserting on the object's bytes passes with no header sent;
//   - asserting on the SDK input struct checks the arithmetic of the line that filled it in;
//   - asserting on the request the emulator received is the only claim that can fail for the right
//     reason.
//
// It is also why the harness grew Request.Header for this work. A property whose only observable is a
// header needs the header observed; there is nothing downstream to catch it, which is exactly how the
// original defect survived three releases with a shipped default that claimed it.
//
// Filed upstream as substrate#475: modeling SSE would let these tests additionally assert that the
// *stored* object carries the encryption, which is a stronger claim than that the request asked for it —
// and "the request asked for encryption" is one inference away from "the object is encrypted," which is
// the exact inference P-7 made.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// The header names S3 spells encryption with. Written out rather than taken from the SDK because the
// point of these tests is what went over the wire, and an SDK constant would let a change in the SDK's
// spelling move the assertion along with the code.
const (
	hdrSSE        = "X-Amz-Server-Side-Encryption"
	hdrSSEKMSKey  = "X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"
	hdrBucketKeys = "X-Amz-Server-Side-Encryption-Bucket-Key-Enabled"
)

// hdrCargoShip is the user-metadata header CargoShip's transporter stamps on every object it uploads,
// and it is the only thing on the wire that says which of the two write paths an object took.
//
// Without it, the bypass tests are half-blind. The direct path and the transporter send identical
// encryption headers when both can express the mode, so an assertion on the encryption alone passes
// whether the object was diverted or not — which means "always bypass" satisfies every encryption
// assertion in this file while quietly disabling CargoShip for anyone who configures encryption at
// all. A performance regression that the security tests certify as correct. Found by mutating
// cargoShipCanEncrypt to `return !cfg.Enabled()` and watching the suite stay green.
const hdrCargoShip = "X-Amz-Meta-Cargoship-Created-By"

// A syntactically valid key ARN, in the us-west-2 the rest of the suite uses. The account is the
// documentation-reserved 111122223333.
const testKMSKeyARN = "arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

// encryptionMutator returns a Config mutator that sets the encryption block and, deliberately, turns
// CargoShip off.
//
// CargoShip is disabled because PutObject diverts around the transporter for any mode it cannot
// express, so leaving it on would mean the sse-s3 and bucket-keys cases test the bypass while the
// sse-kms case tests the transporter — three tests exercising two different code paths, with which
// one silently depending on the mode. TestCargoShipIsBypassedWhenItCannotSendTheHeaders covers the
// bypass itself, on purpose, in one place.
func encryptionMutator(enc s3.EncryptionConfig) func(*s3.Config) {
	return func(cfg *s3.Config) {
		noCompression(cfg)
		cfg.EnableCargoShipOptimization = false
		cfg.Encryption = enc
	}
}

// TestPutObjectSendsTheConfiguredEncryptionHeaders is the direct answer to P-7.
//
// Each case asserts both halves: the headers a mode must send, and that the ones it must not send are
// absent. The negative half is not padding — sending SSEKMSKeyId alongside AES256 is a 400 from S3,
// and a bucket-key header sent when nothing asked for it overrides the bucket's own setting, which is
// a change made to someone's account by a filesystem that was told nothing about it.
func TestPutObjectSendsTheConfiguredEncryptionHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		enc  s3.EncryptionConfig
		// want is the headers that must be present with these exact values.
		want map[string]string
		// absent is the headers that must not appear at all.
		absent []string
	}{
		{
			name: "off sends nothing",
			enc:  s3.EncryptionConfig{Mode: s3.EncryptionModeOff},
			// No header, and that is correct rather than a gap: S3 has applied SSE-S3 to all new
			// objects unconditionally since January 2023, so "off" is not "in the clear" — it is "with
			// S3's keys, not asked for by us".
			absent: []string{hdrSSE, hdrSSEKMSKey, hdrBucketKeys},
		},
		{
			name: "an empty mode is off",
			// The zero value, which is what any caller who never set the field has. It must behave as
			// "off" and not as "unset, therefore skip the check" — an unvalidated zero value that
			// silently means something is how at_rest: true happened.
			enc:    s3.EncryptionConfig{},
			absent: []string{hdrSSE, hdrSSEKMSKey, hdrBucketKeys},
		},
		{
			name: "sse-s3 sends AES256 and no key",
			enc:  s3.EncryptionConfig{Mode: s3.EncryptionModeS3},
			want: map[string]string{hdrSSE: "AES256"},
			// A KMS key header beside AES256 is a 400. So is a bucket-key header, which has no meaning
			// without KMS.
			absent: []string{hdrSSEKMSKey, hdrBucketKeys},
		},
		{
			name: "sse-kms sends aws:kms and the key",
			enc: s3.EncryptionConfig{
				Mode:     s3.EncryptionModeKMS,
				KMSKeyID: testKMSKeyARN,
			},
			want: map[string]string{
				hdrSSE:       "aws:kms",
				hdrSSEKMSKey: testKMSKeyARN,
			},
			// Absent, not "false". S3 reads a missing bucket-key header as "use the bucket's setting",
			// which is the right deference: an account that enabled bucket keys at the bucket should not
			// have them switched off by a filesystem that was never asked about them.
			absent: []string{hdrBucketKeys},
		},
		{
			name: "sse-kms with bucket keys sends all three",
			enc: s3.EncryptionConfig{
				Mode:       s3.EncryptionModeKMS,
				KMSKeyID:   testKMSKeyARN,
				BucketKeys: true,
			},
			want: map[string]string{
				hdrSSE:        "aws:kms",
				hdrSSEKMSKey:  testKMSKeyARN,
				hdrBucketKeys: "true",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			backend := ts.Backend(encryptionMutator(tc.enc))
			ctx := context.Background()

			const key = "encryption/put"

			if err := backend.PutObject(ctx, key, []byte("encrypt me"), nil); err != nil {
				t.Fatalf("PutObject: %v", err)
			}

			writes := ts.Writes(key)
			if len(writes) != 1 {
				t.Fatalf("expected exactly one write request for %q, got %d; the assertions below "+
					"would be reading the wrong request", key, len(writes))
			}

			assertHeaders(t, writes[0], tc.want, tc.absent)
		})
	}
}

// TestMultipartUploadSendsTheEncryptionHeadersOnTheCreate is the same claim for the path large objects
// take.
//
// It is a separate test rather than another case above because the header goes on a different request:
// S3 records the encryption for the upload as a whole on CreateMultipartUpload, and an UploadPart that
// restated it is rejected. So the create is the single request that decides whether a multi-gigabyte
// object is encrypted, and a fix applied only to PutObject would leave every large object — which is
// to say every object worth encrypting — without a header.
//
// The parts are checked too, in the negative direction, for the same reason.
func TestMultipartUploadSendsTheEncryptionHeadersOnTheCreate(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	// No capability gate. The other multipart tests here need RequireMultipartContentEncoding because
	// they assert on what the *stored* object carries; this one asserts on the requests the endpoint
	// received, which the recording proxy captures whatever the emulator then does with them. That is
	// the same reason these tests can cover SSE at all while substrate models none of it.
	const (
		key       = "encryption/multipart"
		chunkSize = 5 * 1024 * 1024
	)

	backend := ts.Backend(func(cfg *s3.Config) {
		encryptionMutator(s3.EncryptionConfig{
			Mode:       s3.EncryptionModeKMS,
			KMSKeyID:   testKMSKeyARN,
			BucketKeys: true,
		})(cfg)

		// Forced down the multipart path, and with a threshold low enough that the payload is a
		// manageable size for a test: two parts, since S3 requires every part but the last to be at
		// least 5 MiB.
		cfg.MultipartThreshold = chunkSize
		cfg.MultipartChunkSize = chunkSize
	})

	data := testaws.DeterministicBytes("encryption-multipart", 2*chunkSize)

	if err := backend.PutObject(context.Background(), key, data, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	var create, complete int

	var parts []testaws.Request

	for _, r := range ts.Writes(key) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.Query, "uploads"):
			create++

			assertHeaders(t, r, map[string]string{
				hdrSSE:        "aws:kms",
				hdrSSEKMSKey:  testKMSKeyARN,
				hdrBucketKeys: "true",
			}, nil)

		case r.Method == http.MethodPut && strings.Contains(r.Query, "partNumber"):
			parts = append(parts, r)

		case r.Method == http.MethodPost && strings.Contains(r.Query, "uploadId"):
			complete++
		}
	}

	if create != 1 {
		t.Fatalf("expected exactly one CreateMultipartUpload, got %d; if it is zero the object did "+
			"not take the multipart path and this test asserted nothing", create)
	}

	if complete != 1 {
		t.Errorf("expected exactly one CompleteMultipartUpload, got %d", complete)
	}

	if len(parts) < 2 {
		t.Fatalf("expected at least two UploadPart requests, got %d; with one part the create is "+
			"still the only place the header could go, but the negative assertion below is vacuous",
			len(parts))
	}

	// The parts must *not* carry it. S3 rejects an UploadPart that restates the upload's encryption,
	// so applying the headers everywhere would be a broken write path rather than a belt-and-braces
	// one — and it would fail only above the multipart threshold, which is where testing is thinnest.
	for i, part := range parts {
		assertHeaders(t, part, nil, []string{hdrSSE, hdrSSEKMSKey, hdrBucketKeys})

		if t.Failed() {
			t.Fatalf("part %d of %d carried encryption headers; S3 rejects an UploadPart that "+
				"restates the upload's encryption", i+1, len(parts))
		}
	}
}

// TestSetObjectMetadataKeepsTheObjectEncrypted covers the sharpest case: an object that was encrypted
// correctly when written and stops being so when something changes its attributes.
//
// A CopyObject does not inherit the source's encryption. S3 encrypts the destination according to the
// request, and a request that says nothing gets the bucket default — so a chmod on an SSE-KMS object
// silently rewrote it off the customer managed key. Nothing reports that: the object is still there,
// still readable, still the right bytes.
func TestSetObjectMetadataKeepsTheObjectEncrypted(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	// Gated because SetObjectMetadata has to *succeed* before there is a copy request to inspect: it
	// heads the object, builds the copy, and returns an error if the endpoint rejects the directive.
	ts.RequireMetadataReplace()

	backend := ts.Backend(encryptionMutator(s3.EncryptionConfig{
		Mode:     s3.EncryptionModeKMS,
		KMSKeyID: testKMSKeyARN,
	}))
	ctx := context.Background()

	const key = "encryption/metadata"

	if err := backend.PutObject(ctx, key, []byte("attributes will change"), nil); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Reset so the assertion reads the copy and not the put that created the object. Without this the
	// test would pass on the seeding request's headers while the copy sent none — an assertion
	// answered by the setup rather than by the code under test.
	ts.ResetRequests()

	if err := backend.SetObjectMetadata(ctx, key, map[string]string{"objectfs-mode": "0644"}); err != nil {
		t.Fatalf("SetObjectMetadata: %v", err)
	}

	writes := ts.Writes(key)
	if len(writes) != 1 {
		t.Fatalf("expected exactly one write for the metadata update, got %d", len(writes))
	}

	copyReq := writes[0]
	if copyReq.Header.Get("X-Amz-Copy-Source") == "" {
		t.Fatalf("the observed write is not a CopyObject (no x-amz-copy-source), so this test is "+
			"asserting about the wrong request: %+v", copyReq)
	}

	assertHeaders(t, copyReq, map[string]string{
		hdrSSE:       "aws:kms",
		hdrSSEKMSKey: testKMSKeyARN,
	}, nil)
}

// TestTierTransitionKeepsTheObjectEncrypted covers the copy nobody asked for.
//
// SetObjectMetadata's copy at least happens because someone changed something. This one happens on a
// timer: AnalyzeAndOptimize walks the tracked access patterns and issues a CopyObject with a new
// storage class, so an object that was written correctly under a customer managed key drifts onto the
// bucket default weeks later with no operation to attribute it to. Of the four write paths it is the
// one where the drop is least likely to be noticed and hardest to explain afterwards.
//
// It is also the path that had no test at all — found by deleting the applyEncryptionCopy call in
// applyOptimization and watching the whole suite stay green, which is the same mutation that caught
// the CargoShip bypass. The 30-day floor in analyzeObject is why: nothing could reach the transition
// without a pattern aged past it, so the copy was unreachable from any test and therefore unasserted.
func TestTierTransitionKeepsTheObjectEncrypted(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)

	backend := ts.Backend(func(cfg *s3.Config) {
		noCompression(cfg)
		cfg.EnableCargoShipOptimization = false
		cfg.Encryption = s3.EncryptionConfig{
			Mode:     s3.EncryptionModeKMS,
			KMSKeyID: testKMSKeyARN,
		}

		// A threshold near zero so any positive saving qualifies. The alternative is tuning the object
		// size against the pricing table until the arithmetic clears a real threshold, which would make
		// this test fail when a rate changes — for a reason having nothing to do with encryption.
		cfg.CostOptimization.EnableAutoTiering = true
		cfg.CostOptimization.CostThreshold = 1e-12
	})

	ctx := context.Background()

	const key = "encryption/tiered"

	// 1 MiB: above the 128 KB floor in findOptimalTier, so a colder tier is a legal destination rather
	// than being bounced back to STANDARD to avoid the IA minimum-size charge.
	if err := backend.PutObject(ctx, key, testaws.DeterministicBytes(key, 1024*1024), nil); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Aged well past analyzeObject's 30-day floor, with an access count low enough to categorize as
	// archive or cold — which for an object this size both resolve to GLACIER_IR, so the fixture does
	// not depend on which side of the 0.01-accesses-per-day line the arithmetic lands. The destination
	// itself is incidental; what matters is that *a* transition happens, since the transition is the
	// only way to reach the CopyObject. The pattern is planted rather than accumulated because
	// accumulating it would take four months.
	now := time.Now()
	s3.SeedAccessPatternForTest(backend, s3.AccessPattern{
		ObjectKey:       key,
		AccessCount:     5,
		FirstAccessTime: now.Add(-120 * 24 * time.Hour),
		LastAccessTime:  now.Add(-10 * 24 * time.Hour),
		ObjectSize:      1024 * 1024,
		CurrentTier:     s3.TierStandard,
	})

	// Reset so the assertion reads the transition's copy and not the put that created the object.
	ts.ResetRequests()

	if err := backend.OptimizeStorageCosts(ctx); err != nil {
		t.Fatalf("OptimizeStorageCosts: %v", err)
	}

	writes := ts.Writes(key)
	if len(writes) != 1 {
		// Zero writes means the optimizer declined the transition, which makes the test vacuous rather
		// than passing: an assertion that never runs is indistinguishable from one that succeeds. If a
		// pricing change makes IA no longer cheaper for this size, the fixture above needs revisiting —
		// not the encryption code.
		t.Fatalf("expected exactly one write for the tier transition, got %d; the optimizer declined "+
			"to transition, so this test asserted nothing", len(writes))
	}

	copyReq := writes[0]
	if copyReq.Header.Get("X-Amz-Copy-Source") == "" {
		t.Fatalf("the observed write is not a CopyObject (no x-amz-copy-source), so this test is "+
			"asserting about the wrong request: %+v", copyReq)
	}

	assertHeaders(t, copyReq, map[string]string{
		hdrSSE:       "aws:kms",
		hdrSSEKMSKey: testKMSKeyARN,
	}, nil)
}

// TestCargoShipIsBypassedWhenItCannotSendTheHeaders pins the divert, because the alternative to
// diverting is worse than not optimizing.
//
// CargoShip's transporter hardcodes the algorithm to aws:kms and has no bucket-key field
// (pkg/aws/s3/transporter.go:105-109 in v0.13.0), so sse-s3 and bucket keys have no representation
// there. Uploading through it anyway would store the object under an encryption nobody configured —
// and the object reads back fine either way, so nothing would ever surface the difference. That is
// P-7's failure mode with an extra step.
//
// This is the one test in the file that leaves CargoShip enabled.
func TestCargoShipIsBypassedWhenItCannotSendTheHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		enc  s3.EncryptionConfig
		want map[string]string
	}{
		{
			// The mode CargoShip cannot express at all: it only ever writes aws:kms.
			name: "sse-s3",
			enc:  s3.EncryptionConfig{Mode: s3.EncryptionModeS3},
			want: map[string]string{hdrSSE: "AES256"},
		},
		{
			// The mode it half-expresses. Without the bypass the object would be encrypted with the
			// right key and no bucket-key header — correct-looking, and billing a KMS call per read
			// against a per-region rate limit that bucket keys exist to avoid.
			name: "sse-kms with bucket keys",
			enc: s3.EncryptionConfig{
				Mode:       s3.EncryptionModeKMS,
				KMSKeyID:   testKMSKeyARN,
				BucketKeys: true,
			},
			want: map[string]string{
				hdrSSE:        "aws:kms",
				hdrSSEKMSKey:  testKMSKeyARN,
				hdrBucketKeys: "true",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			backend := ts.Backend(func(cfg *s3.Config) {
				noCompression(cfg)
				cfg.Encryption = tc.enc
				cfg.EnableCargoShipOptimization = true
			})

			const key = "encryption/cargoship"

			if err := backend.PutObject(context.Background(), key, []byte("via the bypass"), nil); err != nil {
				t.Fatalf("PutObject: %v", err)
			}

			writes := ts.Writes(key)
			if len(writes) != 1 {
				t.Fatalf("expected exactly one write, got %d", len(writes))
			}

			// Both halves, because the encryption assertion alone cannot tell the paths apart: they send
			// the same headers when both can express the mode. This is the one that says the object was
			// actually diverted rather than that it happened to end up encrypted.
			assertHeaders(t, writes[0], tc.want, []string{hdrCargoShip})
		})
	}
}

// TestCargoShipCarriesTheKeyForTheModeItSupports is the other side of the bypass.
//
// Plain sse-kms is expressible by the transporter, so the object goes through it and must still carry
// the headers. Without this the bypass could be "always divert", which would pass every other test in
// this file while quietly disabling CargoShip for anyone who configures encryption at all — a
// performance regression that no encryption assertion would notice.
func TestCargoShipCarriesTheKeyForTheModeItSupports(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(func(cfg *s3.Config) {
		noCompression(cfg)
		cfg.EnableCargoShipOptimization = true
		cfg.Encryption = s3.EncryptionConfig{
			Mode:     s3.EncryptionModeKMS,
			KMSKeyID: testKMSKeyARN,
		}
	})

	const key = "encryption/cargoship-kms"

	if err := backend.PutObject(context.Background(), key, []byte("through the transporter"), nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	writes := ts.Writes(key)
	if len(writes) != 1 {
		t.Fatalf("expected exactly one write, got %d", len(writes))
	}

	assertHeaders(t, writes[0], map[string]string{
		hdrSSE:       "aws:kms",
		hdrSSEKMSKey: testKMSKeyARN,

		// The claim that makes this test the counterweight to the bypass test: the object went *through*
		// the transporter, and carried the encryption anyway. Asserting only the encryption headers here
		// would leave "divert everything" passing, since a diverted object is also correctly encrypted.
		hdrCargoShip: "cargoship",
	}, []string{hdrBucketKeys})
}

// TestNewBackendRejectsAnEncryptionConfigItCannotHonour asserts the validation, which is the half of
// this fix that has to fail loudly.
//
// Every case below is a configuration that would otherwise be *inert*: the mount comes up, objects are
// written, reads succeed, and the encryption the operator named does not happen. That is P-7 exactly,
// so each of these must be a refusal to start rather than a warning. "The mount succeeded" is the
// evidence someone would cite for believing encryption was on.
func TestNewBackendRejectsAnEncryptionConfigItCannotHonour(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		enc  s3.EncryptionConfig
		// wants are substrings the error must contain. Asserted rather than just non-nil because the
		// message is the whole value of failing here: an operator who is told "invalid configuration"
		// has to find which of thirty settings.
		wants []string
	}{
		{
			name: "an unknown mode",
			enc:  s3.EncryptionConfig{Mode: "sse-c"},
			// SSE-C is a real S3 feature ObjectFS does not implement, which makes it the most likely
			// wrong value here — and the one where silently falling through to "no encryption" would be
			// least noticed, since the operator knows the term is real.
			wants: []string{"sse-c", "not one of the modes"},
		},
		{
			name: "an upper-cased mode",
			enc:  s3.EncryptionConfig{Mode: "SSE-KMS", KMSKeyID: testKMSKeyARN},
			// The likely typo, because storage_tier two lines away is upper-case. The message has to
			// say what the value should be, not just that it is wrong.
			wants: []string{"lower case", "sse-kms"},
		},
		{
			name: "sse-kms with no key",
			enc:  s3.EncryptionConfig{Mode: s3.EncryptionModeKMS},
			// S3 would fall back to the AWS managed aws/s3 key, which is shared with every service in
			// the account and cannot be revoked separately from the data. Accepting this would deliver
			// something weaker than what was asked for, silently.
			wants: []string{"kms_key_id is empty", "aws/s3"},
		},
		{
			name: "a key beside mode off",
			enc:  s3.EncryptionConfig{Mode: s3.EncryptionModeOff, KMSKeyID: testKMSKeyARN},
			// The contradiction that matters most. Someone wrote an ARN intending it to be used; the
			// two readings are "encrypt with this key" and "do not encrypt", and guessing either is
			// worse than refusing.
			wants: []string{"kms_key_id is set", "off"},
		},
		{
			name:  "a key beside sse-s3",
			enc:   s3.EncryptionConfig{Mode: s3.EncryptionModeS3, KMSKeyID: testKMSKeyARN},
			wants: []string{"kms_key_id is set", "S3's own keys"},
		},
		{
			name: "bucket keys without kms",
			enc:  s3.EncryptionConfig{Mode: s3.EncryptionModeS3, BucketKeys: true},
			// Inert rather than dangerous, but a config that reads "we enabled the KMS cost control"
			// while no KMS is in use is the same false comfort at_rest: true was.
			wants: []string{"bucket_keys is set", "sse-kms"},
		},
		{
			name: "a key that is not a key",
			enc: s3.EncryptionConfig{
				Mode: s3.EncryptionModeKMS,
				// A well-formed ARN for the wrong service, which is what the clipboard holds after
				// looking at a bucket. S3 answers this with a complaint that does not point at the
				// service field.
				KMSKeyID: "arn:aws:s3:::my-bucket",
			},
			wants: []string{"not for kms"},
		},
		{
			name: "an alias missing its prefix",
			enc: s3.EncryptionConfig{
				Mode: s3.EncryptionModeKMS,
				// The console shows aliases without the prefix, so this is what gets copied. The error
				// has to name the fix rather than just reject the value.
				KMSKeyID: "objectfs-research-data",
			},
			wants: []string{"alias/objectfs-research-data"},
		},
		{
			name: "a key with trailing whitespace",
			enc: s3.EncryptionConfig{
				Mode: s3.EncryptionModeKMS,
				// YAML makes this easy to produce and hard to see, and every pattern rejects it — so
				// without a specific message the reader inspects the key rather than the space.
				KMSKeyID: testKMSKeyARN + " ",
			},
			wants: []string{"whitespace"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)

			cfg := ts.Config()
			cfg.Encryption = tc.enc

			backend, err := s3.NewBackend(context.Background(), ts.Bucket, cfg)
			if err == nil {
				if backend != nil {
					_ = backend.Close()
				}

				t.Fatalf("NewBackend accepted %+v. This configuration asks for encryption it cannot "+
					"deliver, and it fails silently: the mount comes up, writes succeed, and the "+
					"encryption never happens — which is audit finding P-7 verbatim", tc.enc)
			}

			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q, so it does not tell the operator which "+
						"setting to change or what to change it to: %v", want, err)
				}
			}
		})
	}
}

// TestNewBackendAcceptsEveryValidEncryptionConfig is the control.
//
// Without it, the validation above is satisfied by a function that rejects everything — and the
// resulting filesystem would refuse to mount for every operator who configured encryption correctly,
// which is a worse outcome than the defect. The key-form cases matter individually: all four are legal
// on the wire, and a validator that accepted only the ARN would reject what the console shows.
func TestNewBackendAcceptsEveryValidEncryptionConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		enc  s3.EncryptionConfig
	}{
		{"the zero value", s3.EncryptionConfig{}},
		{"off", s3.EncryptionConfig{Mode: s3.EncryptionModeOff}},
		{"sse-s3", s3.EncryptionConfig{Mode: s3.EncryptionModeS3}},
		{
			"sse-kms with a bare key ID",
			s3.EncryptionConfig{
				Mode:     s3.EncryptionModeKMS,
				KMSKeyID: "1234abcd-12ab-34cd-56ef-1234567890ab",
			},
		},
		{
			"sse-kms with a key ARN",
			s3.EncryptionConfig{Mode: s3.EncryptionModeKMS, KMSKeyID: testKMSKeyARN},
		},
		{
			"sse-kms with an alias",
			s3.EncryptionConfig{Mode: s3.EncryptionModeKMS, KMSKeyID: "alias/objectfs-research"},
		},
		{
			// The AWS managed key, named explicitly. Legal, and different from leaving the key empty:
			// the operator has said which key rather than arriving at it by omission.
			"sse-kms with the aws/s3 alias",
			s3.EncryptionConfig{Mode: s3.EncryptionModeKMS, KMSKeyID: "alias/aws/s3"},
		},
		{
			"sse-kms with an alias ARN",
			s3.EncryptionConfig{
				Mode:     s3.EncryptionModeKMS,
				KMSKeyID: "arn:aws:kms:us-west-2:111122223333:alias/objectfs-research",
			},
		},
		{
			"sse-kms with bucket keys",
			s3.EncryptionConfig{
				Mode:       s3.EncryptionModeKMS,
				KMSKeyID:   testKMSKeyARN,
				BucketKeys: true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)

			cfg := ts.Config()
			cfg.Encryption = tc.enc

			backend, err := s3.NewBackend(context.Background(), ts.Bucket, cfg)
			if err != nil {
				t.Fatalf("NewBackend rejected the valid configuration %+v: %v", tc.enc, err)
			}

			t.Cleanup(func() { _ = backend.Close() })
		})
	}
}

// assertHeaders checks that r carries want exactly and none of absent at all.
//
// Both directions in one helper because a test that only checked presence would pass while sending a
// KMS key header alongside AES256 — which S3 answers with a 400, so the write fails in production and
// the test is green.
func assertHeaders(t *testing.T, r testaws.Request, want map[string]string, absent []string) {
	t.Helper()

	for name, value := range want {
		got := r.Header.Get(name)
		if got == "" {
			// The encryption headers get the extra sentence because their absence is the defect itself.
			// hdrCargoShip's absence means something different — the object was diverted around the
			// transporter — and saying "P-7" there would send the reader to the wrong code.
			because := ""
			if name != hdrCargoShip {
				because = " — the request asked for no encryption, which is audit finding P-7"
			}

			t.Errorf("%s %s: header %s is absent, want %q%s", r.Method, r.Path, name, value, because)

			continue
		}

		if got != value {
			t.Errorf("%s %s: header %s is %q, want %q", r.Method, r.Path, name, got, value)
		}
	}

	for _, name := range absent {
		if got := r.Header.Get(name); got != "" {
			t.Errorf("%s %s: header %s is %q, want it absent", r.Method, r.Path, name, got)
		}
	}
}
