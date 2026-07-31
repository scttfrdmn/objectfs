package s3_test

// Compression turns "did the bytes survive" into a property with two independent halves: the write
// path has to record how to decode the object, and the read path has to actually decode it. When the
// halves disagree the failure is silent, because HeadObject reports the uncompressed size while
// GetObject returns the still-encoded body and the kernel pads the difference with zeros. The caller
// sees a successful read of a corrupt file.
//
// Every test here is a regression test for a real defect found by running this code against a real
// endpoint. None of them can be written against a mocked backend: the bug lives in what crosses the
// wire.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/klauspost/compress/zstd"

	"github.com/objectfs/objectfs/internal/storage/s3"
	"github.com/objectfs/objectfs/internal/testaws"
	objectfserrors "github.com/objectfs/objectfs/pkg/errors"
)

// The metadata keys are spelled out here rather than imported from the package under test. They are
// part of the stored object format: renaming one makes every previously-written object undecodable,
// so a test that follows the rename would ratify a breaking change. This duplication is the point.
const (
	metaChecksumKey     = "objectfs-sha256"
	metaOriginalSizeKey = "objectfs-original-size"
)

// compressible returns n bytes that zstd shrinks substantially, without being so degenerate that a
// codec bug could be mistaken for success. All-zero input compresses to a handful of bytes, which
// makes a length assertion pass for uninteresting reasons.
func compressible(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i % 7)
	}

	return data
}

// semiCompressible returns n bytes that zstd shrinks by roughly half: alternating runs of
// incompressible and highly compressible bytes. compressible() shrinks by ~1000:1, which makes it
// useless for reaching a size threshold measured on the *compressed* body.
func semiCompressible(seed string, n int) []byte {
	const run = 32

	data := testaws.DeterministicBytes(seed, n)
	for i := 0; i < n; i += 2 * run {
		end := i + run
		if end > n {
			end = n
		}

		for j := i; j < end; j++ {
			data[j] = 0
		}
	}

	return data
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// defaultBackendAgainst builds a backend from NewDefaultConfig with only the endpoint and credentials
// changed. That distinction matters: the shipped defaults enable both transparent compression and the
// CargoShip transporter, and it is the *combination* that corrupted data. A test that assembles a
// config field by field would not reproduce it.
func defaultBackendAgainst(t *testing.T, ts *testaws.TestServer) *s3.Backend {
	t.Helper()

	cfg := s3.NewDefaultConfig()
	cfg.Endpoint = ts.URL
	cfg.ForcePathStyle = true
	cfg.Region = testaws.DefaultRegion
	cfg.AccessKeyID = testaws.AccessKeyID
	cfg.SecretAccessKey = testaws.SecretAccessKey
	cfg.MaxRetries = 2

	backend, err := s3.NewBackend(context.Background(), ts.Bucket, cfg)
	if err != nil {
		t.Fatalf("build backend from default config: %v", err)
	}

	t.Cleanup(func() { _ = backend.Close() })

	return backend
}

// TestDefaultConfigDoesNotCorruptCompressedObjects is the regression test for the CargoShip
// encoding defect.
//
// cargoships3.Archive has no ContentEncoding field: its CompressionType becomes user metadata and
// nothing sets the HTTP header. So a compressed object uploaded through the transporter stored
// "content-encoding: zstd" as *metadata*, GetObject saw an empty result.ContentEncoding, skipped
// decompression, and returned the raw zstd frame — while HeadObject still reported the uncompressed
// size from objectfs-original-size.
//
// Measured before the fix, on the shipped default configuration: an 8192-byte write read back as
// 29 bytes with a nil error. The audit classified this as latent because
// EnableCargoShipOptimization is only set in NewDefaultConfig and the mount path bypasses it. That
// was wrong — NewBackend(ctx, bucket, nil) and NewDefaultConfig() are both documented entry points,
// and the Go SDK reaches the same defaulting.
func TestDefaultConfigDoesNotCorruptCompressedObjects(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	const key = "compressed/default-config"

	// Above the 4KB MinSize, so compression actually engages.
	want := compressible(8192)

	if err := backend.PutObject(ctx, key, want); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// The object must be stored with the encoding in the header, not only in metadata. Checking this
	// directly is what pins the fix: a future change that reroutes compressed uploads back through a
	// transporter without header support would fail here rather than silently corrupting reads.
	out, err := ts.Client().GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("raw GetObject: %v", err)
	}
	_ = out.Body.Close()

	if enc := aws.ToString(out.ContentEncoding); enc != "zstd" {
		t.Errorf("stored Content-Encoding header = %q, want %q. The encoding is only recoverable "+
			"from the header; storing it in user metadata makes the object undecodable by the read "+
			"path and by every other S3 client.", enc, "zstd")
	}

	// And the round trip must be exact.
	got, err := backend.GetObject(ctx, key, 0, int64(len(want)))
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("wrote %d bytes, read %d back and they differ — silent corruption on the default "+
			"configuration", len(want), len(got))
	}
}

// TestHeadObjectAgreesWithGetObject pins the invariant that makes the corruption silent rather than
// loud. The kernel sizes its reads from HeadObject, so if HeadObject promises more bytes than
// GetObject can produce, the shortfall becomes zeros in the user's file with no error anywhere.
func TestHeadObjectAgreesWithGetObject(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	const key = "compressed/size-agreement"

	want := compressible(8192)
	if err := backend.PutObject(ctx, key, want); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	info, err := backend.HeadObject(ctx, key)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	if info.Size != int64(len(want)) {
		t.Errorf("HeadObject reports %d bytes for a %d-byte object", info.Size, len(want))
	}

	// The stored object really is smaller — this test would be vacuous if compression had not
	// engaged, so assert that it did.
	if stored := ts.ObjectSize(key); stored >= int64(len(want)) {
		t.Fatalf("stored size %d is not smaller than the original %d; compression did not engage "+
			"and this test proves nothing", stored, len(want))
	}

	got, err := backend.GetObject(ctx, key, 0, info.Size)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}

	if int64(len(got)) != info.Size {
		t.Errorf("HeadObject promised %d bytes, GetObject produced %d; the kernel would pad the "+
			"difference with zeros and report success", info.Size, len(got))
	}
}

// TestUndecodableObjectFailsClosed covers the objects the write-path fix cannot help: ones already
// stored with the encoding in user metadata by an earlier build.
//
// Compressor.Decompress returns data *unchanged* when the encoding is empty or unrecognized (audit
// finding C2), so without a guard the read path hands back a raw zstd frame with exit status 0. The
// backend now cross-checks the decoded length against the recorded objectfs-original-size and
// refuses.
func TestUndecodableObjectFailsClosed(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("build zstd encoder: %v", err)
	}
	defer func() { _ = encoder.Close() }()

	original := compressible(8192)
	body := encoder.EncodeAll(original, nil)

	cases := []struct {
		name string
		// header is the HTTP Content-Encoding, which is the only place the read path looks.
		header   string
		metadata map[string]string
	}{
		{
			// Exactly what the broken CargoShip path produced: the encoding recorded where no S3
			// client will look for it.
			name:   "encoding in user metadata only",
			header: "",
			metadata: map[string]string{
				"content-encoding":  "zstd",
				metaOriginalSizeKey: "8192",
			},
		},
		{
			// Audit finding C2: the header is correct, but names a codec this build is not
			// configured for — which is what a change of compression algorithm leaves behind.
			// Decompress returns the data *unchanged* on a mismatch rather than failing.
			name:   "encoding names a codec this build cannot decode",
			header: "lz4",
			metadata: map[string]string{
				metaOriginalSizeKey: "8192",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			key := "undecodable/" + tc.name

			input := &awss3.PutObjectInput{
				Bucket:   aws.String(ts.Bucket),
				Key:      aws.String(key),
				Body:     bytes.NewReader(body),
				Metadata: tc.metadata,
			}
			if tc.header != "" {
				input.ContentEncoding = aws.String(tc.header)
			}

			if _, err := ts.Client().PutObject(ctx, input); err != nil {
				t.Fatalf("seed object: %v", err)
			}

			backend := defaultBackendAgainst(t, ts)

			got, err := backend.GetObject(ctx, key, 0, int64(len(original)))
			if err == nil {
				t.Fatalf("GetObject returned %d bytes of an %d-byte object with no error; the "+
					"caller cannot tell this from a successful read", len(got), len(original))
			}

			var objErr *objectfserrors.ObjectFSError
			if !errors.As(err, &objErr) {
				t.Fatalf("error is unstructured, so no caller can classify it: %v", err)
			}

			if objErr.Code != objectfserrors.ErrCodeDataCorruption {
				t.Errorf("error code = %q, want %q: %v",
					objErr.Code, objectfserrors.ErrCodeDataCorruption, err)
			}

			// Corruption must never be retried: the retry reads the same bad bytes and only delays
			// the report.
			if objErr.Retryable {
				t.Error("a corruption error is marked retryable; retrying reads the same bad bytes")
			}
		})
	}
}

// TestUncompressedObjectsAreUnaffected checks the guard does not fire on the ordinary case. The
// check keys on objectfs-original-size, which is written only for objects that actually compressed,
// so an uncompressed object — including one written by another tool entirely — must read normally.
func TestUncompressedObjectsAreUnaffected(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	// Written by a foreign tool: no ObjectFS metadata at all.
	const foreign = "foreign/object"

	want := testaws.DeterministicBytes(foreign, 8192)
	ts.PutObject(foreign, want)

	got, err := backend.GetObject(ctx, foreign, 0, int64(len(want)))
	if err != nil {
		t.Fatalf("reading an object written by another tool failed: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("read %d bytes of a foreign %d-byte object and they differ", len(got), len(want))
	}

	// Incompressible data below the threshold: ObjectFS writes it, but stores no original size.
	const small = "small/object"

	tiny := testaws.DeterministicBytes(small, 100)
	if err := backend.PutObject(ctx, small, tiny); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got, err = backend.GetObject(ctx, small, 0, int64(len(tiny)))
	if err != nil {
		t.Fatalf("reading a sub-threshold object failed: %v", err)
	}

	if !bytes.Equal(got, tiny) {
		t.Errorf("read %d bytes of a %d-byte object and they differ", len(got), len(tiny))
	}
}

// TestCompressionRoundTripAcrossSizes walks the boundaries where the compression decision changes:
// below MinSize, at it, above it, and across the multipart threshold is covered separately. Each
// size must survive the round trip byte for byte.
func TestCompressionRoundTripAcrossSizes(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	// MinSize defaults to 4KB, so 4095/4096/4097 straddle the decision.
	for _, size := range []int{0, 1, 4095, 4096, 4097, 65536} {
		t.Run(sizeName(size), func(t *testing.T) {
			key := "roundtrip/compressed/" + sizeName(size)
			want := compressible(size)

			if err := backend.PutObject(ctx, key, want); err != nil {
				t.Fatalf("PutObject(%d): %v", size, err)
			}

			got, err := backend.GetObject(ctx, key, 0, int64(size))
			if err != nil {
				t.Fatalf("GetObject(%d): %v", size, err)
			}

			if !bytes.Equal(got, want) {
				t.Errorf("round trip of %d bytes returned %d bytes that differ", size, len(got))
			}

			// Whatever the storage format, HeadObject must report the size a POSIX caller expects.
			info, err := backend.HeadObject(ctx, key)
			if err != nil {
				t.Fatalf("HeadObject(%d): %v", size, err)
			}

			if info.Size != int64(size) {
				t.Errorf("HeadObject reports %d for a %d-byte object", info.Size, size)
			}
		})
	}
}

// TestMultipartCompressedObjectRoundTrips covers the second write path. The multipart threshold is
// measured on the *compressed* size, so a compressible object large enough to trip it takes a
// completely separate code path — CreateMultipartUpload sets its own ContentEncoding and Metadata,
// and nothing else does.
//
// This is where audit #170's checksum divergence lived: two upload paths, two chances to record the
// wrong thing, and no test crossing the boundary.
func TestMultipartCompressedObjectRoundTrips(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireMultipartContentEncoding()

	// S3 requires every non-final part to be at least 5 MB, so the threshold cannot go lower than
	// that and still exercise a genuine multi-part upload.
	const (
		fiveMB = 5 * 1024 * 1024
		size   = 12 * 1024 * 1024
	)

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = true
		cfg.Compression.Algorithm = "zstd"
		cfg.Compression.Level = 3
		cfg.Compression.MinSize = "4KB"
		cfg.MultipartThreshold = fiveMB
		cfg.MultipartChunkSize = fiveMB
	})

	ctx := context.Background()

	const key = "multipart/compressed"

	// Only ~50% compressible, so the compressed body still clears the 5 MB threshold. Highly
	// compressible input would shrink below it and silently take the single-PUT path instead,
	// leaving this test measuring nothing.
	want := semiCompressible(key, size)

	if err := backend.PutObject(ctx, key, want); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	stored := ts.ObjectSize(key)
	if stored >= size {
		t.Fatalf("stored size %d did not shrink below the original %d; compression did not engage",
			stored, size)
	}
	if stored < fiveMB {
		t.Fatalf("compressed size %d is below the %d multipart threshold, so this took the "+
			"single-PUT path and the multipart path is untested", stored, fiveMB)
	}

	// The multipart path must set the header, exactly like the single-PUT path.
	meta := ts.ObjectMetadata(key)
	if got := meta[metaOriginalSizeKey]; got != strconv.Itoa(size) {
		t.Errorf("objectfs-original-size = %q, want %q", got, strconv.Itoa(size))
	}

	// A multipart ETag is not an MD5 of the content, which is exactly why the SHA-256 is recorded
	// separately — and it must be of the uncompressed content on both paths.
	if got, recorded := sha256Hex(want), meta[metaChecksumKey]; recorded != got {
		t.Errorf("recorded checksum %q is not the SHA-256 of the uncompressed content (%q)",
			recorded, got)
	}

	got, err := backend.GetObject(ctx, key, 0, size)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("multipart round trip returned %d bytes that differ from the %d written",
			len(got), len(want))
	}

	// No upload may be left open: an orphaned multipart upload is invisible through every other API
	// and bills until a lifecycle rule reaps it.
	if orphans := ts.MultipartUploads(); len(orphans) != 0 {
		t.Errorf("a completed multipart upload left %d upload(s) open: %v", len(orphans), orphans)
	}
}

// TestChecksumMetadataSurvivesCompression checks that the SHA-256 recorded on upload is of the
// *uncompressed* content, so it stays meaningful across a change of storage format. v0.10.0 writes
// this checksum and never reads it; it is the guard the read path needs, and it is worthless if it
// hashes the compressed bytes.
func TestChecksumMetadataSurvivesCompression(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	const key = "checksum/compressed"

	want := compressible(8192)
	if err := backend.PutObject(ctx, key, want); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	meta := ts.ObjectMetadata(key)

	recorded := meta[metaChecksumKey]
	if recorded == "" {
		t.Fatalf("no objectfs-sha256 recorded; metadata was %v", meta)
	}

	if got := sha256Hex(want); recorded != got {
		t.Errorf("recorded checksum %s is not the SHA-256 of the uncompressed content (%s); "+
			"a checksum over the stored bytes cannot survive a change of codec", recorded, got)
	}

	info, err := backend.HeadObject(ctx, key)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	if info.Checksum != recorded {
		t.Errorf("HeadObject reports checksum %q, but the object carries %q",
			info.Checksum, recorded)
	}
}
