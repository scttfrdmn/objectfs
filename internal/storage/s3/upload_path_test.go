package s3_test

// What PutObject stores has to be what PutObject computed, and the only place to check that is the
// stored object.
//
// These tests exist because the CargoShip transporter branch was a second upload path that agreed with
// the first about everything it was asked and differed in what it actually stored. It could not carry a
// Content-Encoding (silent corruption, fixed by bypassing it), could not carry the encryption headers
// (bypassed), could not carry a per-object storage class (bypassed), and could not carry a Content-Type
// — which nothing caught, because every layer above the boundary had the right value and the assertion
// had never been made against the endpoint.
//
// The branch is gone now. These assertions are not: they hold for whatever upload path is live, so a
// future second path has to satisfy them rather than inherit an untested boundary.

import (
	"context"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// TestDetectedContentTypeReachesTheStoredObject asserts the header, for object sizes on both sides of
// the multipart threshold.
//
// Both sides matter and only one was broken, which is the reason to parameterize rather than pick a
// size. The multipart path always set ContentType on CreateMultipartUpload; the single-part path had
// the CargoShip branch in front of it, and that branch put the value in user metadata where no client
// looks. Since the transporter was only ever reachable *below* the threshold — PutObject returns into
// putObjectMultipart first — every object a FUSE mount writes was on the broken side and every object
// large enough to be worth optimizing was not.
func TestDetectedContentTypeReachesTheStoredObject(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	// A threshold low enough that a test can afford an object on the far side of it, and a chunk size
	// at S3's 5 MiB part minimum rather than below it.
	//
	// The chunk size is load-bearing and the first draft of this test had it at 1 MiB, which made the
	// single-part assertions pass for the wrong reason: the AWS uploader rejects a part size under
	// 5 MiB, so every CargoShip upload failed, PutObject logged "CargoShip optimization failed, falling
	// back to standard S3", and the fallback set the header correctly. A test that silently exercises
	// the fallback instead of the path under test is worse than no test.
	const (
		threshold = 8 << 20
		chunkSize = 5 << 20
	)

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.MultipartThreshold = threshold
		cfg.MultipartChunkSize = chunkSize

		// Compression off: a compressed object carries a Content-Encoding, and the interaction between
		// the two headers is a different question with its own test.
		cfg.Compression.Enabled = false
	})

	// The suffixes detectContentType actually distinguishes, plus one it does not. The unmapped case is
	// not filler — application/octet-stream is also what the dropped-header bug produced, so a test that
	// only checked mapped suffixes could not tell "correctly defaulted" from "silently lost".
	for _, tc := range []struct {
		name string
		key  string
		want string
	}{
		{name: "text", key: "notes.txt", want: "text/plain"},
		{name: "json", key: "index.json", want: "application/json"},
		{name: "png", key: "figure.png", want: "image/png"},
		{name: "unmapped", key: "reads.bam", want: "application/octet-stream"},
	} {
		for _, size := range []struct {
			name string
			n    int
		}{
			{name: "single-part", n: 64 << 10},
			{name: "multipart", n: threshold + (1 << 20)},
		} {
			t.Run(tc.name+"/"+size.name, func(t *testing.T) {
				t.Parallel()

				key := size.name + "/" + tc.key
				data := testaws.DeterministicBytes(key, size.n)

				if err := backend.PutObject(ctx, key, data, nil); err != nil {
					t.Fatalf("PutObject %q: %v", key, err)
				}

				if got := ts.ObjectContentType(key); got != tc.want {
					t.Errorf("stored %q with Content-Type %q, want %q. Nothing reports this: the "+
						"object is readable and every layer above the upload boundary had the right "+
						"value", key, got, tc.want)
				}

				// A header is worth nothing if the body took a different path than the one asserted.
				if got := ts.GetObject(key); len(got) != len(data) {
					t.Errorf("stored %d bytes, want %d", len(got), len(data))
				}
			})
		}
	}
}

// TestStoredMetadataIsOnlyWhatObjectFSWrote pins the user metadata of a plain object.
//
// The CargoShip branch added four keys of its own to every small object: cargoship-created-by,
// cargoship-upload-time, and two that were structurally empty because ObjectFS never populated the
// Archive fields they came from — cargoship-original-size was always "0" and
// cargoship-compression-type always "". A timestamp in user metadata also made otherwise-identical
// objects differ, which is a real cost for anything comparing or deduplicating them.
//
// Asserted as an exact key set rather than as the absence of a cargoship- prefix. A prefix check would
// pass for any future path that stamped its own provenance under a different name, and the property
// worth holding is that a caller's object carries the caller's metadata plus ObjectFS's integrity keys
// and nothing else.
func TestStoredMetadataIsOnlyWhatObjectFSWrote(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = false
	})

	const key = "meta/plain.bin"

	if err := backend.PutObject(ctx, key, testaws.DeterministicBytes(key, 8<<10), nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// objectfs-sha256 alone: objectfs-original-size is written only for a compressed object, and
	// compression is off here.
	want := map[string]bool{"objectfs-sha256": true}

	for k := range ts.ObjectMetadata(key) {
		if !want[k] {
			t.Errorf("stored user metadata key %q, which ObjectFS did not write and no caller asked "+
				"for. User metadata is billed, returned on every HEAD, and part of what makes two "+
				"objects differ", k)
		}
	}

	for k := range want {
		if _, ok := ts.ObjectMetadata(key)[k]; !ok {
			t.Errorf("no %q in the stored metadata; the integrity keys are the ones that must be there", k)
		}
	}
}

// TestCallerMetadataSurvivesTheUploadPath is the other half: removing a path that added keys must not
// remove the mechanism that carries the caller's.
//
// vfs passes attribute metadata through PutObject's meta argument, so a regression here loses file
// modes and ownership rather than a provenance stamp.
func TestCallerMetadataSurvivesTheUploadPath(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = false
	})

	const key = "meta/with-caller-keys.bin"

	meta := map[string]string{
		"objectfs-mode": "0644",
		"objectfs-uid":  "1000",
	}

	if err := backend.PutObject(ctx, key, testaws.DeterministicBytes(key, 8<<10), meta); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	stored := ts.ObjectMetadata(key)

	for k, v := range meta {
		if stored[k] != v {
			t.Errorf("caller metadata %q = %q, want %q", k, stored[k], v)
		}
	}

	if _, ok := stored["objectfs-sha256"]; !ok {
		t.Error("caller metadata displaced the integrity checksum")
	}
}
