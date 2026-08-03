package compression

// The decoder table is the fix for #230, and what makes it a fix rather than a workaround is that it
// is derived from pkg/compression.SupportedAlgorithms rather than listed by hand. The defect was that
// the set of algorithms ObjectFS can *write* and the set it can *read* were maintained
// independently — one codec on the read side, three on the write side — so these tests assert the
// derivation, not a snapshot of today's three codecs.

import (
	"bytes"
	"testing"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

// TestEveryWritableAlgorithmIsDecodable is the property the fix rests on: whatever a Compressor can
// produce, any Compressor can read back.
//
// Written as a matrix over SupportedAlgorithms in both directions so that adding an algorithm to that
// list and forgetting the read side fails here rather than in production, where it presents as
// objects written last week being unreadable today.
func TestEveryWritableAlgorithmIsDecodable(t *testing.T) {
	t.Parallel()

	original := bytes.Repeat([]byte("decoder table\n"), 500)

	for _, writeAlgo := range comprpkg.SupportedAlgorithms() {
		if writeAlgo == comprpkg.AlgorithmNone {
			// Stores nothing encoded, so there is no token to dispatch on. Covered by
			// TestDisabledCompressorStillDecodes, which is the case that matters.
			continue
		}

		t.Run("written_with_"+string(writeAlgo), func(t *testing.T) {
			t.Parallel()

			writer, err := NewCompressor(makeConfig(true, string(writeAlgo), "0", 0))
			if err != nil {
				t.Fatalf("NewCompressor(%q): %v", writeAlgo, err)
			}

			encoded, wasCompressed, err := writer.Compress(original)
			if err != nil {
				t.Fatalf("Compress with %q: %v", writeAlgo, err)
			}

			if !wasCompressed {
				t.Fatalf("%q did not compress %d bytes of repetitive text, so there is no encoded "+
					"object for a reader to decode", writeAlgo, len(original))
			}

			encoding := writer.ContentEncoding()
			if encoding == "" {
				t.Fatalf("%q reports an empty Content-Encoding while compressing; an object stored "+
					"this way records nothing a reader could dispatch on", writeAlgo)
			}

			for _, readAlgo := range comprpkg.SupportedAlgorithms() {
				t.Run("read_by_"+string(readAlgo), func(t *testing.T) {
					t.Parallel()

					// enabled follows the algorithm: "none" is only reachable as a disabled
					// compressor, since NewCompressor with Enabled true and AlgorithmNone is a
					// configuration nothing produces.
					enabled := readAlgo != comprpkg.AlgorithmNone

					reader, err := NewCompressor(makeConfig(enabled, string(readAlgo), "0", 0))
					if err != nil {
						t.Fatalf("NewCompressor(%q): %v", readAlgo, err)
					}

					got, decoded, err := reader.Decompress(encoded, encoding)
					if err != nil {
						t.Fatalf("a %q-configured compressor could not decode %q: %v",
							readAlgo, encoding, err)
					}

					if !decoded {
						t.Fatalf("a %q-configured compressor declined to decode %q, which it has a "+
							"codec for. Its decodable set is %v", readAlgo, encoding,
							reader.DecodableEncodings())
					}

					if !bytes.Equal(got, original) {
						t.Errorf("decoding %q with a %q-configured compressor returned %d bytes "+
							"that differ from the %d written", encoding, readAlgo, len(got),
							len(original))
					}
				})
			}
		})
	}
}

// TestDisabledCompressorStillDecodes is the configuration change an operator is most likely to make,
// and the one that used to orphan a bucket outright.
func TestDisabledCompressorStillDecodes(t *testing.T) {
	t.Parallel()

	c, err := NewCompressor(makeConfig(false, "none", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	if c.Enabled() {
		t.Fatal("a compressor built from disabled settings reports Enabled")
	}

	if got := len(c.DecodableEncodings()); got == 0 {
		t.Fatal("a disabled compressor decodes nothing. Turning compression off stops new objects " +
			"being compressed; it does not make the objects already in the bucket uncompressed")
	}

	original := bytes.Repeat([]byte("still decodable\n"), 500)

	writer, err := NewCompressor(makeConfig(true, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor(zstd): %v", err)
	}

	encoded, wasCompressed, err := writer.Compress(original)
	if err != nil || !wasCompressed {
		t.Fatalf("Compress: err=%v, wasCompressed=%v", err, wasCompressed)
	}

	got, decoded, err := c.Decompress(encoded, "zstd")
	if err != nil {
		t.Fatalf("a disabled compressor could not decode a zstd object: %v", err)
	}

	if !decoded || !bytes.Equal(got, original) {
		t.Errorf("decoded=%v, got %d bytes, want %d", decoded, len(got), len(original))
	}

	// It must still not compress, or "disabled" means nothing.
	out, wasCompressed, err := c.Compress(original)
	if err != nil {
		t.Fatalf("Compress on a disabled compressor: %v", err)
	}

	if wasCompressed || !bytes.Equal(out, original) {
		t.Error("a disabled compressor compressed its input")
	}
}

// TestDecodableEncodingsMatchesSupportedAlgorithms pins the derivation itself.
//
// A list of decoders maintained separately from the list of algorithms would reintroduce #230's whole
// class of gap, so the assertion is against SupportedAlgorithms rather than against {gzip, lz4, zstd}.
func TestDecodableEncodingsMatchesSupportedAlgorithms(t *testing.T) {
	t.Parallel()

	c, err := NewCompressor(makeConfig(true, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	decodable := make(map[string]bool)
	for _, token := range c.DecodableEncodings() {
		if token == "" {
			t.Error("the decoder table contains an empty Content-Encoding token; an empty encoding " +
				"means \"not encoded\" rather than naming a codec, and Decompress handles it before " +
				"consulting the table")
		}

		decodable[token] = true
	}

	for _, algo := range comprpkg.SupportedAlgorithms() {
		codec, err := New(algo, comprpkg.DefaultLevel)
		if err != nil {
			t.Fatalf("New(%q): %v", algo, err)
		}

		token := codec.ContentEncoding()
		if token == "" {
			continue
		}

		if !decodable[token] {
			t.Errorf("%q writes Content-Encoding %q and no decoder claims it, so an object written "+
				"with it cannot be read back. DecodableEncodings is %v",
				algo, token, c.DecodableEncodings())
		}
	}

	// Sorted, because callers put this in error messages and log fields.
	tokens := c.DecodableEncodings()
	for i := 1; i < len(tokens); i++ {
		if tokens[i-1] > tokens[i] {
			t.Errorf("DecodableEncodings is not sorted: %v", tokens)

			break
		}
	}
}

// TestDecompressReportsACorruptFrame separates the two failure modes a caller has to tell apart: a
// token nothing claims (bytes through, decoded false, no error — another tool's format) and a token a
// codec claims over a body it rejects (an error — the stored bytes are damaged).
func TestDecompressReportsACorruptFrame(t *testing.T) {
	t.Parallel()

	c, err := NewCompressor(makeConfig(true, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	garbage := []byte("this is not a zstd frame, whatever the header says")

	got, decoded, err := c.Decompress(garbage, "zstd")
	if err == nil {
		t.Fatalf("Decompress accepted %d bytes of non-zstd data labeled zstd and returned %d bytes "+
			"with decoded=%v; a caller cannot tell that from a successful decode",
			len(garbage), len(got), decoded)
	}

	if decoded {
		t.Error("Decompress reported decoded=true alongside an error")
	}

	if got != nil {
		t.Errorf("Decompress returned %d bytes alongside an error; a caller that checks the error "+
			"second would use them", len(got))
	}
}
