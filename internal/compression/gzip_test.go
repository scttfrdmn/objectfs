package compression

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strconv"
	"testing"

	comprpkg "github.com/objectfs/objectfs/pkg/compression"
)

func TestGzipCodec_RoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"single byte", []byte{0x42}},
		{"compressible", bytes.Repeat([]byte("objectfs "), 1024)},
		{"incompressible", incompressibleBytes(4096)},
		{"binary with nulls", append(bytes.Repeat([]byte{0}, 512), 0xff, 0x00, 0xfe)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, err := NewGzipCodec(comprpkg.DefaultLevel)
			if err != nil {
				t.Fatalf("NewGzipCodec: %v", err)
			}

			encoded, err := c.Compress(tc.data)
			if err != nil {
				t.Fatalf("Compress: %v", err)
			}

			decoded, err := c.Decompress(encoded)
			if err != nil {
				t.Fatalf("Decompress: %v", err)
			}

			if !bytes.Equal(decoded, tc.data) {
				t.Errorf("round trip of %d bytes returned %d that differ", len(tc.data), len(decoded))
			}
		})
	}
}

// TestGzipCodec_ProducesStandardGzip is the reason to have this codec at all. Its output must be
// readable by anything that speaks gzip — otherwise it offers nothing over zstd, which compresses
// better and faster. Verified against the standard library rather than against itself, because
// reading back through the same codec that wrote cannot detect a non-standard encoding.
func TestGzipCodec_ProducesStandardGzip(t *testing.T) {
	t.Parallel()

	c, err := NewGzipCodec(comprpkg.DefaultLevel)
	if err != nil {
		t.Fatalf("NewGzipCodec: %v", err)
	}

	want := bytes.Repeat([]byte("a standard gzip stream "), 256)

	encoded, err := c.Compress(want)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	// The magic bytes any gzip reader looks for.
	if len(encoded) < 2 || encoded[0] != 0x1f || encoded[1] != 0x8b {
		t.Fatalf("output does not start with the gzip magic 1f 8b: % x",
			encoded[:min(len(encoded), 8)])
	}

	r, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("compress/gzip cannot read this codec's output: %v", err)
	}

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("compress/gzip read: %v", err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("compress/gzip close: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("compress/gzip decoded %d bytes that differ from the %d written", len(got), len(want))
	}

	// And the reverse direction: this codec must read a stream the standard library wrote. Each
	// direction is checked independently, because a symmetric encoding bug survives a round trip
	// through one implementation.
	var buf bytes.Buffer

	w := gzip.NewWriter(&buf)
	if _, err := w.Write(want); err != nil {
		t.Fatalf("stdlib gzip write: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("stdlib gzip close: %v", err)
	}

	decoded, err := c.Decompress(buf.Bytes())
	if err != nil {
		t.Fatalf("this codec cannot read compress/gzip's output: %v", err)
	}

	if !bytes.Equal(decoded, want) {
		t.Errorf("decoding a stdlib gzip stream returned %d bytes that differ from %d",
			len(decoded), len(want))
	}
}

func TestGzipCodec_Levels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		level   int
		wantErr bool
	}{
		{0, false}, // the codec's default
		{1, false}, // best speed
		{6, false},
		{9, false}, // best compression
		{-1, true}, // gzip.DefaultCompression's own numeric value; not a valid config input
		{10, true},
		{22, true}, // valid for zstd, so the likeliest wrong value to appear here
	}

	for _, tc := range cases {
		t.Run(levelName(tc.level), func(t *testing.T) {
			t.Parallel()

			c, err := NewGzipCodec(tc.level)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("NewGzipCodec(%d) succeeded; an out-of-range level must be refused at "+
						"construction, not silently clamped", tc.level)
				}

				return
			}

			if err != nil {
				t.Fatalf("NewGzipCodec(%d): %v", tc.level, err)
			}

			// Every accepted level must actually compress, so a level that constructs but produces
			// nothing usable cannot pass.
			want := bytes.Repeat([]byte("level check "), 512)

			encoded, err := c.Compress(want)
			if err != nil {
				t.Fatalf("Compress at level %d: %v", tc.level, err)
			}

			if len(encoded) >= len(want) {
				t.Errorf("level %d did not shrink highly compressible input: %d -> %d",
					tc.level, len(want), len(encoded))
			}

			decoded, err := c.Decompress(encoded)
			if err != nil {
				t.Fatalf("Decompress at level %d: %v", tc.level, err)
			}

			if !bytes.Equal(decoded, want) {
				t.Errorf("level %d round trip differs", tc.level)
			}
		})
	}
}

// TestGzipCodec_RejectsCorruptInput matters more for gzip than for the others: gzip carries a CRC32
// trailer, so it is the one codec that can actually detect corruption. Silently returning partial
// data would waste the only integrity check the format provides.
func TestGzipCodec_RejectsCorruptInput(t *testing.T) {
	t.Parallel()

	c, err := NewGzipCodec(comprpkg.DefaultLevel)
	if err != nil {
		t.Fatalf("NewGzipCodec: %v", err)
	}

	valid, err := c.Compress(bytes.Repeat([]byte("payload "), 512))
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	cases := []struct {
		name string
		data []byte
	}{
		{"not gzip at all", []byte("this is plain text, not a gzip stream")},
		{"empty", []byte{}},
		{"truncated mid-stream", valid[:len(valid)/2]},
		{"trailer removed", valid[:len(valid)-4]},
		{"corrupt payload byte", flipByte(valid, len(valid)/2)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := c.Decompress(tc.data)
			if err == nil {
				t.Errorf("Decompress accepted corrupt input and returned %d bytes; a caller cannot "+
					"tell this from a successful read", len(got))
			}
		})
	}
}

func TestGzipCodec_Identity(t *testing.T) {
	t.Parallel()

	c, err := NewGzipCodec(comprpkg.DefaultLevel)
	if err != nil {
		t.Fatalf("NewGzipCodec: %v", err)
	}

	if got := c.Algorithm(); got != comprpkg.AlgorithmGzip {
		t.Errorf("Algorithm() = %q, want %q", got, comprpkg.AlgorithmGzip)
	}

	// This token is stored on the object as its Content-Encoding, and it is the only record of how
	// to decode it. It must be the registered HTTP name, not a variant: "x-gzip" or "GZIP" would
	// make objects ObjectFS writes undecodable by other clients and by its own read path.
	if got := c.ContentEncoding(); got != "gzip" {
		t.Errorf("ContentEncoding() = %q, want %q", got, "gzip")
	}
}

// TestGzipCodec_ConcurrentUse pins the Codec interface's concurrency contract. One codec instance is
// shared by every operation on a backend, so a shared Writer would corrupt output under load rather
// than failing visibly.
func TestGzipCodec_ConcurrentUse(t *testing.T) {
	t.Parallel()

	c, err := NewGzipCodec(comprpkg.DefaultLevel)
	if err != nil {
		t.Fatalf("NewGzipCodec: %v", err)
	}

	const workers = 8

	errs := make(chan error, workers)

	for w := range workers {
		go func(w int) {
			// Distinct payloads per goroutine, so output crossing between them is detectable.
			want := bytes.Repeat([]byte{byte('a' + w)}, 8192)

			encoded, err := c.Compress(want)
			if err != nil {
				errs <- err

				return
			}

			decoded, err := c.Decompress(encoded)
			if err != nil {
				errs <- err

				return
			}

			if !bytes.Equal(decoded, want) {
				errs <- errMismatch

				return
			}

			errs <- nil
		}(w)
	}

	for range workers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent use: %v", err)
		}
	}
}

var errMismatch = errors.New("a concurrent round trip returned another goroutine's data")

// incompressibleBytes returns n bytes with no exploitable structure, so a codec that grows its input
// is exercised rather than only the happy path.
func incompressibleBytes(n int) []byte {
	data := make([]byte, n)

	// A xorshift sequence: deterministic, and without the repetition a simple counter would give.
	state := uint32(0x9e3779b9)
	for i := range data {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		data[i] = byte(state)
	}

	return data
}

func flipByte(data []byte, at int) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	out[at] ^= 0xff

	return out
}

func levelName(level int) string {
	return "level_" + strconv.Itoa(level)
}
