package s3_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"testing"

	"github.com/objectfs/objectfs/internal/storage/s3"
	"github.com/objectfs/objectfs/internal/testaws"
)

// FuzzRoundTrip asserts the property everything else in ObjectFS rests on: bytes written to an object
// come back identical, whatever happens to them in between.
//
// "Whatever happens in between" is the point. A PUT may compress, may not compress, may decide
// compression made things worse and store the original, may exceed the multipart threshold and be
// uploaded in parts, may be below the configured minimum size, and records a SHA-256 of the
// uncompressed content as user metadata. A GET reverses whichever of those actually occurred, decided
// from the object's own headers. Each combination is a path, and the audit found two of them silently
// returning wrong bytes:
//
//   - C2: Decompress returned data *unchanged* when the stored Content-Encoding did not match the
//     configured codec. Write with zstd, reconfigure to lz4, and `cat` emitted the raw zstd frame with
//     exit status 0 — while HeadObject reported the original size, so the kernel padded the difference
//     with zeros. Corruption with a successful exit status is the worst failure available.
//   - #170: the multipart path computed its checksum over different bytes than the single-part path,
//     so the same content produced two different recorded hashes depending only on its size.
//
// Both are round-trip failures and neither is visible to a test of one layer. This target composes
// them: real compression, real multipart, real HTTP endpoint, and one comparison at the end.
//
// The checksum is asserted as well as the bytes, because the checksum is what a future integrity
// check will trust. v0.10.0 wrote objectfs-sha256 on every upload and never read it — a hash nobody
// verifies is a hash nobody notices is wrong.
func FuzzRoundTrip(f *testing.F) {
	sh := testaws.Shared(f)

	// One backend per algorithm, built once. Rebuilding per iteration would spend the run on client
	// construction and health checks, and — measured — exhaust the ephemeral port range.
	backends := map[string]*s3.Backend{}

	for _, algo := range []string{"none", "zstd", "lz4", "gzip"} {
		backend, err := sh.Backend(context.Background(), func(cfg *s3.Config) {
			cfg.Compression.Enabled = algo != "none"
			cfg.Compression.Algorithm = algo

			// No minimum, so small inputs take the compressed path too. The default 4KB minimum would
			// send almost every fuzzer-generated input down the pass-through path and leave the codecs
			// untested.
			cfg.Compression.MinSize = "0"

			// A low multipart threshold, so the multipart path is reachable from inputs of a size a
			// fuzzer will actually generate. At the 32 MiB default it would never run: that is one
			// allocation the fuzzer avoids and exactly where #170's divergent checksum lived.
			cfg.MultipartThreshold = 5 * 1024 * 1024
		})
		if err != nil {
			f.Fatalf("testaws: backend for %s: %v", algo, err)
		}

		f.Cleanup(func() { _ = backend.Close() })

		backends[algo] = backend
	}

	// Seeds chosen for the branches they take, not for looking like data.
	f.Add([]byte{}, uint8(0))                                 // empty: a legitimate object, and an easy off-by-one
	f.Add([]byte{0}, uint8(1))                                // one byte
	f.Add(bytes.Repeat([]byte("A"), 4096), uint8(1))          // maximally compressible, zstd
	f.Add(bytes.Repeat([]byte("A"), 4096), uint8(2))          // the same, lz4
	f.Add(bytes.Repeat([]byte("A"), 4096), uint8(3))          // the same, gzip
	f.Add(incompressible(4096), uint8(1))                     // compression makes it larger: the discard path
	f.Add(bytes.Repeat([]byte("xyz"), 100), uint8(0))         // no codec configured
	f.Add(bytes.Repeat([]byte("B"), 5*1024*1024+1), uint8(0)) // over the multipart threshold: #170's path
	f.Add(bytes.Repeat([]byte("B"), 5*1024*1024+1), uint8(1)) // multipart *and* compressed
	f.Add([]byte{0xff, 0xfe, 0x00, 0x01}, uint8(2))           // bytes that are not text
	f.Add(bytes.Repeat([]byte{0}, 1<<16), uint8(1))           // a large run of zeros

	algorithms := []string{"none", "zstd", "lz4", "gzip"}

	var n int

	f.Fuzz(func(t *testing.T, content []byte, pick uint8) {
		// 8 MiB is past the multipart threshold set above with room to spare. Beyond that the target
		// measures the emulator's allocator.
		if len(content) > 8*1024*1024 {
			return
		}

		algo := algorithms[int(pick)%len(algorithms)]
		backend := backends[algo]

		// A fresh key per iteration. Reuse would let a previous iteration's object satisfy a read
		// whose write failed, turning a data-loss bug into a pass.
		n++
		key := fmt.Sprintf("roundtrip/%s-%d.bin", algo, n)

		if err := backend.PutObject(context.Background(), key, content, nil); err != nil {
			t.Fatalf("PutObject(%d bytes, %s) failed: %v", len(content), algo, err)
		}

		// Read it whole, the way the size is not known in advance.
		got, err := backend.GetObject(context.Background(), key, 0, -1)
		if err != nil {
			t.Fatalf("GetObject after a successful PutObject of %d bytes (%s): %v",
				len(content), algo, err)
		}

		if !bytes.Equal(got, content) {
			// Report the shape rather than the bytes: a megabyte of hex in a failure message is
			// unreadable, and the first differing offset is what identifies the defect.
			t.Fatalf("round trip through %s corrupted the object: wrote %d bytes, read %d, first "+
				"difference at offset %d", algo, len(content), len(got), firstDifference(content, got))
		}

		// HeadObject must report the *uncompressed* size. The kernel sizes reads from it, so a
		// compressed object reporting its stored length makes every read short — and reporting more
		// than the content makes the kernel pad with zeros, which is C2's silent corruption.
		info, err := backend.HeadObject(context.Background(), key)
		if err != nil {
			t.Fatalf("HeadObject after a successful PutObject: %v", err)
		}

		if info.Size != int64(len(content)) {
			t.Errorf("HeadObject reports %d bytes for a %d-byte object stored with %s — the kernel "+
				"sizes reads from this, so it truncates or zero-pads every read",
				info.Size, len(content), algo)
		}

		// The recorded checksum must be the hash of the content the caller wrote, not of whatever was
		// stored. That is what makes it verifiable later, and it is where the multipart path used to
		// disagree with the single-part one.
		want := sha256.Sum256(content)

		if info.Checksum != "" && info.Checksum != hex.EncodeToString(want[:]) {
			t.Errorf("the recorded objectfs-sha256 is %s but the content hashes to %s (%d bytes, %s) "+
				"— a checksum that does not match the content cannot be used to detect corruption",
				info.Checksum, hex.EncodeToString(want[:]), len(content), algo)
		}

		// A ranged read of the middle must agree with the same slice of the whole. This is the check
		// that catches a range applied to compressed bytes rather than decoded ones: the length can
		// look right while the offset is wrong.
		if len(content) >= 4 {
			offset := int64(len(content) / 4)
			size := int64(len(content) / 2)

			part, err := backend.GetObject(context.Background(), key, offset, size)
			if err != nil {
				t.Fatalf("ranged GetObject(%d, %d) on a %d-byte %s object: %v",
					offset, size, len(content), algo, err)
			}

			if wantPart := content[offset : offset+size]; !bytes.Equal(part, wantPart) {
				t.Errorf("a ranged read of a %s object returned the wrong bytes: offset %d size %d "+
					"of %d, first difference at %d — a range applied to the stored bytes rather than "+
					"the decoded ones",
					algo, offset, size, len(content), firstDifference(wantPart, part))
			}
		}
	})
}

// firstDifference returns the offset of the first differing byte, or the shorter length if one is a
// prefix of the other. It exists so a failure message names an offset rather than dumping megabytes.
func firstDifference(a, b []byte) int {
	n := min(len(a), len(b))

	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}

	return n
}

// incompressible returns n bytes that no codec can shrink, so [FuzzRoundTrip] reaches the branch
// where compression is attempted, makes the data larger, and the original is stored instead.
//
// That branch matters more than it looks: the object is then stored *without* a Content-Encoding
// while compression is configured and enabled. A read path that decides how to decode from its own
// configuration rather than from the object's headers gets this exactly wrong — which is audit
// finding C2's mechanism, reached without changing any configuration at all.
func incompressible(n int) []byte {
	out := make([]byte, n)

	// A linear congruential sequence: cheap, deterministic, and high-entropy enough that zstd, lz4 and
	// gzip all emit more bytes than they consume. Deterministic matters — a corpus entry built from a
	// random source would not reproduce.
	state := uint32(0x12345678)
	for i := range out {
		state = state*1664525 + 1013904223
		out[i] = byte(state >> 24)
	}

	return out
}

// TestRoundTripAcrossACodecChange is audit finding C2, pinned as an ordinary test.
//
// Write an object with one codec configured, then read it back through a backend configured with a
// different one. The bytes must come back correct, because the decision of how to decode belongs to
// the object — its stored Content-Encoding — and not to whatever the reader happens to be configured
// with now.
//
// v0.10.0 did the opposite: Decompress compared the stored encoding against the configured codec's
// token and returned the data *unchanged* when they differed. So this exact sequence produced a raw
// compressed frame presented as file content, with a successful exit status, padded to the original
// length by the kernel because HeadObject still reported the uncompressed size.
//
// It is a table test rather than a fuzz seed because a corpus can be emptied, a fuzztime can be set to
// zero, and neither failure is visible. A named test that fails is.
func TestRoundTripAcrossACodecChange(t *testing.T) {
	sh := testaws.Shared(t)

	// Compressible, so the write path definitely stores an encoded body — the case the reader has to
	// handle. Incompressible content would be stored raw and pass trivially.
	content := bytes.Repeat([]byte("the same line over and over\n"), 512)

	cases := []struct {
		wroteWith string
		readWith  string
	}{
		{"zstd", "lz4"},
		{"zstd", "gzip"},
		{"gzip", "zstd"},
		{"lz4", "zstd"},
		{"zstd", "none"},
		{"gzip", "none"},
		{"lz4", "none"},
		{"none", "zstd"},
	}

	for i, tc := range cases {
		t.Run(tc.wroteWith+"-then-"+tc.readWith, func(t *testing.T) {
			t.Parallel()

			writer, err := sh.Backend(context.Background(), compressedWith(tc.wroteWith))
			if err != nil {
				t.Fatalf("backend configured with %s: %v", tc.wroteWith, err)
			}
			defer func() { _ = writer.Close() }()

			key := fmt.Sprintf("codec-change/%d.bin", i)
			if err := writer.PutObject(context.Background(), key, content, nil); err != nil {
				t.Fatalf("PutObject with %s: %v", tc.wroteWith, err)
			}

			// A second backend on the *same bucket*, configured differently — the reconfiguration a
			// user performs by editing a config file and remounting.
			reader, err := sh.BackendOn(context.Background(), writer.Bucket(), compressedWith(tc.readWith))
			if err != nil {
				t.Fatalf("backend configured with %s: %v", tc.readWith, err)
			}
			defer func() { _ = reader.Close() }()

			got, err := reader.GetObject(context.Background(), key, 0, -1)
			if err != nil {
				// Failing closed is acceptable: an integrity error tells the user their data needs a
				// build that can decode it. Returning wrong bytes is not.
				t.Logf("read failed closed, which is acceptable: %v", err)

				return
			}

			if !bytes.Equal(got, content) {
				t.Fatalf("an object written with %s and read with %s configured came back wrong: "+
					"%d bytes instead of %d, first difference at %d. This is audit finding C2 — the "+
					"decode decision must come from the object's stored encoding, not the reader's "+
					"configuration.",
					tc.wroteWith, tc.readWith, len(got), len(content),
					firstDifference(content, got))
			}
		})
	}
}

// compressedWith returns a config mutator selecting an algorithm with no minimum size, so the test's
// modest payloads take the compressed path.
func compressedWith(algo string) func(*s3.Config) {
	return func(cfg *s3.Config) {
		cfg.Compression.Enabled = algo != "none"
		cfg.Compression.Algorithm = algo
		cfg.Compression.MinSize = "0"
	}
}

// TestPutObjectRecordsAVerifiableChecksum pins the checksum contract across the single-part and
// multipart paths, which is audit finding #170.
//
// The two paths computed their hash over different bytes, so identical content produced different
// recorded checksums depending only on whether it crossed the multipart threshold. A checksum that
// depends on how the object happened to be uploaded cannot be used to verify anything, and this is
// the test that would have caught it: the same content on both sides of the boundary, asserted against
// one independently computed hash.
func TestPutObjectRecordsAVerifiableChecksum(t *testing.T) {
	sh := testaws.Shared(t)

	const threshold = 5 * 1024 * 1024

	backend, err := sh.Backend(context.Background(), func(cfg *s3.Config) {
		cfg.MultipartThreshold = threshold
		cfg.MultipartChunkSize = threshold
	})
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer func() { _ = backend.Close() }()

	sizes := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"one byte", 1},
		{"just under the multipart threshold", threshold - 1},
		{"exactly the multipart threshold", threshold},
		{"just over the multipart threshold", threshold + 1},
		{"two parts", threshold*2 + 1},
	}

	for i, tc := range sizes {
		t.Run(tc.name, func(t *testing.T) {
			// Not parallel: several megabytes each, and running them together is a memory spike with
			// no benefit.

			content := make([]byte, tc.size)
			for j := range content {
				content[j] = byte(j*31 + j>>8)
			}

			key := fmt.Sprintf("checksum/%d.bin", i)
			if err := backend.PutObject(context.Background(), key, content, nil); err != nil {
				t.Fatalf("PutObject(%d bytes): %v", tc.size, err)
			}

			info, err := backend.HeadObject(context.Background(), key)
			if err != nil {
				t.Fatalf("HeadObject: %v", err)
			}

			want := sha256.Sum256(content)
			if got := info.Checksum; got != hex.EncodeToString(want[:]) {
				t.Errorf("a %d-byte object (%s the multipart threshold) recorded checksum %q, but "+
					"its content hashes to %q — the two upload paths disagree, so the checksum "+
					"cannot verify anything",
					tc.size, map[bool]string{true: "over", false: "under"}[tc.size >= threshold],
					got, hex.EncodeToString(want[:]))
			}

			// And the size, from the same metadata, since a compressed multipart object records both
			// and getting either wrong misreports the file.
			if info.Size != int64(tc.size) {
				t.Errorf("HeadObject reports %d bytes for a %s-byte object",
					info.Size, strconv.Itoa(tc.size))
			}
		})
	}
}
