package s3_test

// A bucket outlives its configuration. Objects accumulate across mounts, across releases, and across
// whatever the operator had `algorithm` set to at the time, so the algorithm a mount is configured to
// *write* says nothing about the algorithm of the object in front of it.
//
// Through v0.11.0 the read path did not accept that. `Compressor.Decompress` compared the object's
// stored `Content-Encoding` against the single configured codec's token and returned the data
// unchanged on any mismatch, so a mount could read back only what it was currently set to write.
// Changing `algorithm: zstd` to `lz4` — or turning compression off entirely — made every existing
// compressed object unreadable, with the code to read them linked into the same binary. Measured
// before the fix, one object written under zstd and read by four backends differing only in that
// field: zstd returned the 22,000 bytes; gzip, lz4, and `enabled: false` each returned a
// DATA_CORRUPTION error (#230, audit finding C2).
//
// Failing closed was the correct half of that. `checkFullyDecoded` compares the decoded length
// against the recorded `objectfs-original-size`, so nobody got a raw frame with exit status 0. But it
// was compensating for a dispatch that could have succeeded.

import (
	"bytes"
	"context"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

// writableAlgorithms returns the algorithms that produce an encoded object, read from
// pkg/compression rather than listed here so an algorithm added there is covered without an edit.
//
// AlgorithmNone is excluded on the write side because it stores nothing encoded, and there is
// therefore no encoding for a reader to dispatch on. It appears on the *read* side below as the
// `enabled: false` backend, which is the configuration that actually matters: turning compression
// off must not make what is already in the bucket unreadable.
func writableAlgorithms(t *testing.T) []comprpkg.Algorithm {
	t.Helper()

	var algos []comprpkg.Algorithm

	for _, algo := range comprpkg.SupportedAlgorithms() {
		if algo != comprpkg.AlgorithmNone {
			algos = append(algos, algo)
		}
	}

	if len(algos) < 2 {
		t.Fatalf("pkg/compression reports %d writable algorithms; a cross-algorithm read test needs "+
			"at least two to prove anything", len(algos))
	}

	return algos
}

// compressorBackend builds a backend whose compression settings are the only thing that varies.
//
// Level is left at the codec default deliberately: the valid range differs per algorithm (zstd 0-22,
// gzip 0-9), so a fixed non-zero level would be invalid for some of the codecs this test enumerates.
func compressorBackend(t *testing.T, ts *testaws.TestServer, algo string, enabled bool) *s3.Backend {
	t.Helper()

	return ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = enabled
		cfg.Compression.Algorithm = algo
		cfg.Compression.Level = 0
		cfg.Compression.MinSize = "1KB"
	})
}

// TestEveryConfiguredAlgorithmReadsEveryStoredEncoding is the issue's failure table, generalized: for
// each algorithm an object may have been written with, every reader configuration must return the
// original bytes.
//
// The matrix is every writable algorithm × (every writable algorithm + compression disabled). It is
// built from comprpkg.SupportedAlgorithms in both directions, so an algorithm added to that list is
// covered as a writer and as a reader with no edit here — which is the point, since the defect was
// precisely that the set of decoders and the set of encoders were maintained independently.
func TestEveryConfiguredAlgorithmReadsEveryStoredEncoding(t *testing.T) {
	t.Parallel()

	algos := writableAlgorithms(t)

	// Big enough to clear MinSize by a margin and to compress under all three codecs, so a pass
	// cannot come from an object that was never encoded in the first place.
	want := compressible(22000)

	for _, writeAlgo := range algos {
		t.Run("written_with_"+string(writeAlgo), func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			ctx := context.Background()

			key := "cross-algorithm/" + string(writeAlgo)

			writer := compressorBackend(t, ts, string(writeAlgo), true)
			if err := writer.PutObject(ctx, key, want, nil); err != nil {
				t.Fatalf("PutObject with algorithm %q: %v", writeAlgo, err)
			}

			// Without this the whole subtest is vacuous: an object stored uncompressed reads back
			// correctly under every configuration for reasons that have nothing to do with the fix.
			stored := ts.ObjectSize(key)
			if stored >= int64(len(want)) {
				t.Fatalf("algorithm %q stored %d bytes for a %d-byte object, so it did not compress "+
					"and no reader has anything to dispatch on", writeAlgo, stored, len(want))
			}

			// Readers: every algorithm, plus compression off.
			readers := make([]struct {
				name    string
				algo    string
				enabled bool
			}, 0, len(algos)+1)

			for _, readAlgo := range algos {
				readers = append(readers, struct {
					name    string
					algo    string
					enabled bool
				}{name: "algorithm_" + string(readAlgo), algo: string(readAlgo), enabled: true})
			}

			readers = append(readers, struct {
				name    string
				algo    string
				enabled bool
			}{name: "compression_disabled", algo: string(comprpkg.AlgorithmNone), enabled: false})

			for _, reader := range readers {
				t.Run(reader.name, func(t *testing.T) {
					t.Parallel()

					backend := compressorBackend(t, ts, reader.algo, reader.enabled)

					got, err := backend.GetObject(ctx, key, 0, int64(len(want)))
					if err != nil {
						t.Fatalf("an object written with %q could not be read by a mount configured "+
							"%s: %v\nEvery codec is linked into this binary, so the object is "+
							"decodable and the configuration is the only thing refusing it. A mount "+
							"that cannot read what an earlier configuration wrote orphans the bucket",
							writeAlgo, reader.name, err)
					}

					if !bytes.Equal(got, want) {
						t.Errorf("an object written with %q read back under %s as %d bytes that "+
							"differ from the %d written", writeAlgo, reader.name, len(got), len(want))
					}
				})
			}
		})
	}
}

// TestDisablingCompressionStillReadsCompressedObjects is the case above that an operator is most
// likely to hit, pulled out on its own because it is the one that reads as a *configuration* change
// rather than a format change.
//
// Setting `enabled: false` is how someone turns compression off after deciding it was not worth the
// read amplification. It stops new objects being compressed. It cannot make the existing ones
// uncompressed, and before #230 it made them all unreadable — which is a considerably worse outcome
// than the setting they were trying to leave.
func TestDisablingCompressionStillReadsCompressedObjects(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	const key = "disabled-reader/object"

	want := compressible(22000)

	writer := compressorBackend(t, ts, "zstd", true)
	if err := writer.PutObject(ctx, key, want, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	reader := compressorBackend(t, ts, "none", false)

	got, err := reader.GetObject(ctx, key, 0, int64(len(want)))
	if err != nil {
		t.Fatalf("a mount with compression disabled could not read a compressed object: %v\n"+
			"Turning compression off stops new objects being compressed; it does not make the "+
			"existing ones uncompressed", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("read %d bytes, want %d", len(got), len(want))
	}

	// And it must still write uncompressed, or "disabled" would not mean anything.
	const plain = "disabled-reader/written-after"

	if err := reader.PutObject(ctx, plain, want, nil); err != nil {
		t.Fatalf("PutObject with compression disabled: %v", err)
	}

	if stored := ts.ObjectSize(plain); stored != int64(len(want)) {
		t.Errorf("a mount with compression disabled stored %d bytes for a %d-byte object; it is "+
			"still compressing", stored, len(want))
	}
}

// TestPartialReadSurvivesAnAlgorithmChange covers the same fix on the other read shape.
//
// A compressed object cannot serve a range from a range of the stored bytes, so the backend fetches
// the whole body and slices after decoding. That slice is arithmetic on the decoded buffer, so it is
// only correct if the decode happened — and a ranged read is where a silent failure hides best,
// because `checkFullyDecoded` deliberately does not fire on one: a short range is legitimately short,
// and the length alone cannot distinguish it from a still-encoded body.
func TestPartialReadSurvivesAnAlgorithmChange(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ctx := context.Background()

	const key = "cross-algorithm/ranged"

	want := compressible(22000)

	writer := compressorBackend(t, ts, "zstd", true)
	if err := writer.PutObject(ctx, key, want, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Offset chosen away from any boundary, so a decode that silently did not happen produces bytes
	// from the middle of a zstd frame rather than something that could coincide with the plaintext.
	const (
		offset = 9001
		length = 4096
	)

	for _, readAlgo := range []string{"gzip", "lz4", "none"} {
		t.Run("read_as_"+readAlgo, func(t *testing.T) {
			t.Parallel()

			backend := compressorBackend(t, ts, readAlgo, readAlgo != "none")

			got, err := backend.GetObject(ctx, key, offset, length)
			if err != nil {
				t.Fatalf("ranged read of a zstd object by a %q-configured mount: %v", readAlgo, err)
			}

			if !bytes.Equal(got, want[offset:offset+length]) {
				t.Errorf("ranged read at offset %d returned %d bytes that are not the object's "+
					"content there. A range is sliced out of the decoded buffer, so this is what a "+
					"decode that silently did not happen looks like — and checkFullyDecoded cannot "+
					"catch it, because a short range is legitimately short", offset, len(got))
			}
		})
	}
}
