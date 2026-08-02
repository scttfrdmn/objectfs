package s3_test

// v0.10.0 computed a SHA-256 of every object's uncompressed content on upload, stored it as the
// objectfs-sha256 user-metadata key, and surfaced it on HeadObject as ObjectInfo.Checksum. No read
// path anywhere compared it against the bytes that came back. The one piece of stored evidence that
// what came out is what went in was written and never read.
//
// These tests are what makes it load-bearing. They corrupt objects at the storage layer — behind the
// backend, using a raw SDK client — so the bytes ObjectFS reads genuinely differ from the bytes that
// were hashed. That is not reproducible against a mock: a mock returns what it was handed, so the
// hash always matches and the check appears to work while testing nothing.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/scttfrdmn/objectfs/internal/testaws"
	objectfserrors "github.com/scttfrdmn/objectfs/pkg/errors"
)

// seedWithChecksum writes body directly to storage with the given objectfs-sha256 value, bypassing
// ObjectFS entirely. Passing a checksum that does not describe body is how these tests stand in for
// bit-rot in the bucket, a mangled multipart assembly, or an out-of-band overwrite — all of which
// produce exactly this state and all of which v0.10.0 returned with a successful exit status.
func seedWithChecksum(t *testing.T, ts *testaws.TestServer, key string, body []byte, checksum string) {
	t.Helper()

	_, err := ts.Client().PutObject(context.Background(), &awss3.PutObjectInput{
		Bucket:   aws.String(ts.Bucket),
		Key:      aws.String(key),
		Body:     bytes.NewReader(body),
		Metadata: map[string]string{metaChecksumKey: checksum},
	})
	if err != nil {
		t.Fatalf("seed %q: %v", key, err)
	}
}

// requireCorruptionError asserts the read failed in a way a caller can act on: structured, coded as
// corruption, and not retryable. A retry re-reads the same bad bytes, so marking corruption retryable
// converts a clear failure into a slow one.
func requireCorruptionError(t *testing.T, err error, got []byte, wantLen int) {
	t.Helper()

	if err == nil {
		t.Fatalf("GetObject returned %d bytes of a %d-byte corrupt object with no error; the caller "+
			"cannot tell this from a successful read", len(got), wantLen)
	}

	var objErr *objectfserrors.ObjectFSError
	if !errors.As(err, &objErr) {
		t.Fatalf("error is unstructured, so no caller can classify it: %v", err)
	}

	if objErr.Code != objectfserrors.ErrCodeDataCorruption {
		t.Errorf("error code = %q, want %q: %v", objErr.Code, objectfserrors.ErrCodeDataCorruption, err)
	}

	if objErr.Retryable {
		t.Error("a corruption error is marked retryable; retrying reads the same bad bytes")
	}
}

// TestChecksumMismatchFailsClosed is the test that turns the recorded hash from decoration into a
// guarantee. Each case stores content that does not match its recorded checksum and asserts the read
// fails rather than returning the bytes.
func TestChecksumMismatchFailsClosed(t *testing.T) {
	t.Parallel()

	content := testaws.DeterministicBytes("checksum-mismatch", 8192)

	cases := []struct {
		name     string
		body     []byte
		checksum string
		why      string
	}{
		{
			name:     "content replaced entirely",
			body:     testaws.DeterministicBytes("something-else", 8192),
			checksum: sha256Hex(content),
			why: "same length, different bytes — the case a length check cannot see, and the reason " +
				"objectfs-original-size is not sufficient on its own",
		},
		{
			name:     "one bit flipped",
			body:     flipBit(content, 4095*8+3),
			checksum: sha256Hex(content),
			why:      "bit-rot in the bucket. Nothing else in the read path would notice",
		},
		{
			name:     "content truncated",
			body:     content[:4096],
			checksum: sha256Hex(content),
			why: "a short body. HeadObject reports the recorded size, so the kernel pads the " +
				"shortfall with zeros and the caller reads a silently truncated file",
		},
		{
			name:     "content extended",
			body:     append(bytes.Clone(content), 0xFF),
			checksum: sha256Hex(content),
			why:      "a long body, which no size check on the recorded length would reject either",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			backend := defaultBackendAgainst(t, ts)

			key := "corrupt/" + strings.ReplaceAll(tc.name, " ", "-")
			seedWithChecksum(t, ts, key, tc.body, tc.checksum)

			got, err := backend.GetObject(context.Background(), key, 0, 0)
			requireCorruptionError(t, err, got, len(content))

			if err == nil {
				t.Log(tc.why)
			}
		})
	}
}

// TestMalformedChecksumFailsClosed pins the one case where this check is deliberately stricter than
// the object-size check beside it.
//
// HeadObject falls back to ContentLength for a malformed objectfs-original-size, because a bad
// metadata value must not make a file unreadable. There is no equivalent fallback for a checksum: a
// value that is not 64 hex characters was not written by this code, and treating "I cannot tell
// whether this is corrupt" as "this is fine" is the exact reasoning that let the compression
// corruption ship.
func TestMalformedChecksumFailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		checksum string
	}{
		{name: "not hex", checksum: strings.Repeat("z", 64)},
		{name: "too short", checksum: "abc123"},
		{name: "truncated by one character", checksum: strings.Repeat("a", 63)},
		{name: "one character too long", checksum: strings.Repeat("a", 65)},
		{name: "a sha-1 rather than a sha-256", checksum: strings.Repeat("a", 40)},
		{name: "whitespace padded", checksum: " " + strings.Repeat("a", 64)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			backend := defaultBackendAgainst(t, ts)

			content := testaws.DeterministicBytes("malformed", 4096)
			key := "malformed/" + strings.ReplaceAll(tc.name, " ", "-")
			seedWithChecksum(t, ts, key, content, tc.checksum)

			got, err := backend.GetObject(context.Background(), key, 0, 0)
			requireCorruptionError(t, err, got, len(content))
		})
	}
}

// TestChecksumVerificationCoversWholeObjectReads is the test that would have caught the first version
// of this guard, which gated on "was a Range header sent" and therefore verified almost nothing.
//
// A filesystem read of a whole small file arrives at the backend as offset=0 with a size of the
// kernel's read buffer — 131072 on Linux — not the file's length. That sends a Range header and gets
// back every byte of the object. Gating on the request shape declined to verify the single most
// common read a filesystem serves, while reporting that reads were verified.
func TestChecksumVerificationCoversWholeObjectReads(t *testing.T) {
	t.Parallel()

	content := testaws.DeterministicBytes("whole-object-shapes", 4096)
	corrupt := flipBit(content, 17)

	cases := []struct {
		name   string
		offset int64
		size   int64
		why    string
	}{
		{
			name:   "unranged read",
			offset: 0,
			size:   0,
			why:    "size <= 0 means whole object; no Range header is sent",
		},
		{
			name:   "size exactly the object length",
			offset: 0,
			size:   4096,
			why:    "a ranged request that happens to cover everything",
		},
		{
			name:   "size is the kernel read buffer",
			offset: 0,
			size:   131072,
			why: "the common cat(1) shape. S3 clamps the range to the object and returns all of it, " +
				"so this is a whole-object read wearing a ranged request",
		},
		{
			name:   "size far beyond the object",
			offset: 0,
			size:   1 << 30,
			why:    "same again at an extreme; coverage is decided by the response, not the ask",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			ts.RequireRangeGET()
			backend := defaultBackendAgainst(t, ts)

			key := "shapes/" + strings.ReplaceAll(tc.name, " ", "-")
			seedWithChecksum(t, ts, key, corrupt, sha256Hex(content))

			got, err := backend.GetObject(context.Background(), key, tc.offset, tc.size)
			if err == nil {
				t.Fatalf("GetObject(%d, %d) returned %d bytes of a corrupt object with no error. %s",
					tc.offset, tc.size, len(got), tc.why)
			}

			requireCorruptionError(t, err, got, len(content))
		})
	}
}

// TestPartialReadsAreNotVerified pins the gap the verification deliberately leaves, so that it is a
// documented boundary rather than something a later reader discovers and mistakes for a bug.
//
// The recorded hash covers the whole content, so checking a fragment means fetching the whole object
// — which is audit finding C4, the read amplification the read path was just fixed to stop doing.
// Verifying a 4 KiB read of a 10 GiB object would transfer 10 GiB. Per-chunk checksums are the real
// fix and belong with the seekable-framing work, since both change the stored object's layout.
//
// If this test ever fails because the read now errors, that is likely progress: per-chunk verification
// landed. Delete the test rather than restoring the gap it protects.
func TestPartialReadsAreNotVerified(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()
	backend := defaultBackendAgainst(t, ts)

	content := testaws.DeterministicBytes("partial-read", 16384)
	corrupt := flipBit(content, 12)

	const key = "partial/corrupt"
	seedWithChecksum(t, ts, key, corrupt, sha256Hex(content))

	// A tail fragment: does not start at zero, so the whole-object hash has nothing to compare to.
	got, err := backend.GetObject(context.Background(), key, 8192, 4096)
	if err != nil {
		t.Fatalf("a partial read failed: %v\nRanged reads are deliberately unverified — the recorded "+
			"hash covers the whole object, so checking a fragment would mean fetching all of it. If "+
			"per-chunk checksums have landed, delete this test rather than restoring the gap.", err)
	}

	if !bytes.Equal(got, corrupt[8192:12288]) {
		t.Errorf("partial read returned %d bytes that match neither the stored nor the original "+
			"content", len(got))
	}

	// A head fragment shorter than the object is equally unverifiable, and is the more tempting case
	// to get wrong: it starts at zero, so only the length distinguishes it from a whole-object read.
	got, err = backend.GetObject(context.Background(), key, 0, 4096)
	if err != nil {
		t.Fatalf("a head-fragment read failed: %v", err)
	}

	if !bytes.Equal(got, corrupt[:4096]) {
		t.Errorf("head-fragment read returned %d unexpected bytes", len(got))
	}
}

// TestObjectsWithoutChecksumsRemainReadable is the counterweight to the tests above. Failing closed
// is only correct where there is evidence to fail on.
//
// Objects written by aws s3 cp, by boto3, by a bucket that predates ObjectFS, or by any other tool
// carry no objectfs-sha256. Refusing to read them would make ObjectFS unable to read the buckets it
// exists to mount — so a missing checksum verifies trivially, and that is the only possible behavior
// rather than a weakened check.
func TestObjectsWithoutChecksumsRemainReadable(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	cases := []struct {
		name     string
		metadata map[string]string
	}{
		{name: "no metadata at all", metadata: nil},
		{name: "unrelated metadata", metadata: map[string]string{"author": "someone-else"}},
		{name: "empty checksum value", metadata: map[string]string{metaChecksumKey: ""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			key := "foreign/" + strings.ReplaceAll(tc.name, " ", "-")
			want := testaws.DeterministicBytes(key, 8192)

			_, err := ts.Client().PutObject(ctx, &awss3.PutObjectInput{
				Bucket:   aws.String(ts.Bucket),
				Key:      aws.String(key),
				Body:     bytes.NewReader(want),
				Metadata: tc.metadata,
			})
			if err != nil {
				t.Fatalf("seed: %v", err)
			}

			got, err := backend.GetObject(ctx, key, 0, 0)
			if err != nil {
				t.Fatalf("reading an object with no recorded checksum failed: %v\nObjectFS must be "+
					"able to read objects written by other tools.", err)
			}

			if !bytes.Equal(got, want) {
				t.Errorf("read %d bytes, want %d, and they differ", len(got), len(want))
			}
		})
	}
}

// TestChecksumVerificationAcceptsWhatObjectFSWrote closes the loop: the hash the write path records
// must be the hash the read path computes.
//
// A guard that fires on genuine corruption but also on ObjectFS's own output is not a guard, it is an
// outage — and the two halves are written independently, one over the uncompressed content before
// compression and one over the decompressed content after. Compression is what makes this
// non-trivial: hashing the wrong side of the codec would pass every mismatch test above while
// breaking every real read.
func TestChecksumVerificationAcceptsWhatObjectFSWrote(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	cases := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: []byte{}},
		{name: "single byte", data: []byte{0x42}},
		{name: "below the compression threshold", data: testaws.DeterministicBytes("tiny", 100)},
		{name: "compressible and above the threshold", data: compressible(8192)},
		{name: "incompressible and above the threshold", data: testaws.DeterministicBytes("random", 8192)},
		{name: "semi-compressible", data: semiCompressible("mixed", 65536)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			key := "roundtrip/" + strings.ReplaceAll(tc.name, " ", "-")

			if err := backend.PutObject(ctx, key, tc.data, nil); err != nil {
				t.Fatalf("PutObject: %v", err)
			}

			// Whether a checksum was recorded at all is worth asserting directly: if the write path
			// stopped writing one, every verification test above would still pass — vacuously — and
			// the read path would be back to checking nothing.
			meta := ts.ObjectMetadata(key)
			if _, ok := lookupCI(meta, metaChecksumKey); !ok {
				t.Fatalf("no %s recorded for a %d-byte object written by ObjectFS; with no stored "+
					"hash the read path has nothing to verify and every mismatch test passes "+
					"vacuously. Metadata: %v", metaChecksumKey, len(tc.data), meta)
			}

			got, err := backend.GetObject(ctx, key, 0, 0)
			if err != nil {
				t.Fatalf("reading back ObjectFS's own write failed verification: %v\nThe write path "+
					"hashes the uncompressed content; the read path must hash the decompressed "+
					"content. A guard that rejects our own output is an outage.", err)
			}

			if !bytes.Equal(got, tc.data) {
				t.Errorf("round trip returned %d bytes, wrote %d, and they differ", len(got), len(tc.data))
			}

			// And again through the ranged shape a real read uses.
			got, err = backend.GetObject(ctx, key, 0, 131072)
			if err != nil {
				t.Fatalf("a kernel-buffer-sized read of ObjectFS's own write failed: %v", err)
			}

			if !bytes.Equal(got, tc.data) {
				t.Errorf("buffer-sized read returned %d bytes, wrote %d, and they differ",
					len(got), len(tc.data))
			}
		})
	}
}

// TestChecksumIsFoundHoweverTheServerSpellsIt records what this emulator does with metadata key case,
// and why that means the case-insensitivity of the lookup cannot be tested from out here.
//
// The concern is real: S3 lower-cases user-metadata keys in transit, but the SDK's response map
// preserves whatever case the *server* sent. MinIO title-cases them, and a Go http.Header round-trip
// canonicalizes to Objectfs-Sha256. A case-sensitive lookup therefore finds nothing against those
// servers — and for an integrity check, finding nothing means silently ceasing to check while still
// reporting that reads are verified.
//
// But it cannot be provoked through a client, because the case a client *sends* is not the case the
// server *returns*. This test asserts that normalization rather than assuming it: if a future
// emulator version echoed keys verbatim, this would fail and say so, which is the signal that the
// end-to-end case test has become possible. The lookup itself is covered directly by
// TestVerifyChecksumIgnoresMetadataKeyCase, which can construct the response map a foreign server
// would produce.
//
// Worth stating plainly: the first version of this test seeded four spellings through the client and
// asserted each was detected. It passed against a case-sensitive lookup — vacuously, since all four
// arrived as one — and would have certified the exact failure it was written to prevent.
func TestChecksumIsFoundHoweverTheServerSpellsIt(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	for _, spelling := range []string{
		"objectfs-sha256",
		"Objectfs-Sha256",
		"OBJECTFS-SHA256",
		"ObjectFS-SHA256",
	} {
		key := "case/" + spelling

		_, err := ts.Client().PutObject(ctx, &awss3.PutObjectInput{
			Bucket:   aws.String(ts.Bucket),
			Key:      aws.String(key),
			Body:     bytes.NewReader([]byte("content")),
			Metadata: map[string]string{spelling: strings.Repeat("a", 64)},
		})
		if err != nil {
			t.Fatalf("seed %q: %v", spelling, err)
		}

		meta := ts.ObjectMetadata(key)
		if _, ok := meta[metaChecksumKey]; !ok {
			t.Errorf("sent metadata key %q and it came back as %v, not lower-cased. This endpoint no "+
				"longer normalizes key case, which is the behavior that makes a case-sensitive lookup "+
				"testable end-to-end — write that test.", spelling, meta)
		}
	}
}

// flipBit returns a copy of data with a single bit inverted, which is the smallest corruption a
// checksum has to catch and one no length or size check can see.
func flipBit(data []byte, bit int) []byte {
	out := bytes.Clone(data)
	out[(bit/8)%len(out)] ^= 1 << (bit % 8)

	return out
}

// lookupCI finds a metadata key case-insensitively, duplicating the backend's own helper rather than
// importing it. A test that shared the lookup would agree with a broken lookup by construction, which
// is the mistake that left the checksum unread in the first place.
func lookupCI(metadata map[string]string, key string) (string, bool) {
	for k, v := range metadata {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}

	return "", false
}
