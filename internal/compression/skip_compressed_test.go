package compression

import (
	"bytes"
	"math/rand/v2"
	"testing"

	comprpkg "github.com/scttfrdmn/objectfs/pkg/compression"
)

// countingCodec wraps a real codec and records how many times Compress was entered.
//
// The count is the assertion this file is built around, and it is not interchangeable with comparing
// bytes. Compress already returns the original when the compressed form is no smaller, so an
// already-compressed object came back unchanged *before* #184 too — a test that only checked
// bytes.Equal(got, data) passed on the unfixed code and would have certified a fix that does nothing.
// What #184 changes is whether the codec runs at all, so that is what gets asserted.
type countingCodec struct {
	inner comprpkg.Codec
	calls int
}

func (c *countingCodec) Compress(src []byte) ([]byte, error) {
	c.calls++

	return c.inner.Compress(src)
}

func (c *countingCodec) Decompress(src []byte) ([]byte, error) { return c.inner.Decompress(src) }
func (c *countingCodec) Algorithm() comprpkg.Algorithm         { return c.inner.Algorithm() }
func (c *countingCodec) ContentEncoding() string               { return c.inner.ContentEncoding() }

// spyCompressor returns a Compressor whose write codec counts its invocations.
//
// The codec is swapped in after NewCompressor rather than injected through Settings, because Settings
// names an algorithm rather than carrying a codec, and adding a codec field to it for a test would put
// a seam in the production type that only tests cross.
func spyCompressor(t *testing.T) (*Compressor, *countingCodec) {
	t.Helper()

	c, err := NewCompressor(makeConfig(true, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	spy := &countingCodec{inner: c.codec}
	c.codec = spy

	return c, spy
}

// highEntropyTail returns n pseudo-random bytes from a fixed seed.
//
// Fixed rather than time-seeded so a failure reproduces from the same bytes, and high-entropy so that
// nothing here passes by way of the "compressed form was no smaller" fallback when the intent is to
// test the magic-byte skip.
func highEntropyTail(n int) []byte {
	r := rand.New(rand.NewPCG(0x9E3779B97F4A7C15, 0xBF58476D1CE4E5B9))

	tail := make([]byte, n)
	for i := range tail {
		tail[i] = byte(r.UintN(256))
	}

	return tail
}

// payload builds an object of the given total size beginning with prefix.
func payload(prefix string, total int) []byte {
	body := make([]byte, 0, total)
	body = append(body, prefix...)

	if rest := total - len(body); rest > 0 {
		body = append(body, highEntropyTail(rest)...)
	}

	return body
}

// TestCompressSkipsAlreadyCompressedData asserts the write path does not run the codec over data that
// arrived compressed (#184).
//
// The prefixes are the real magic bytes of each format, taken from what classifyByMagic recognizes,
// because the point of routing this through AlreadyCompressed rather than a private table in the write
// path is that the two cannot disagree — so the test states the formats a user will actually store and
// lets classifyByMagic be the thing that recognizes them.
//
// The final three cases are the controls. A plain tar, plain text, and a JSON document must still be
// compressed: a skip that fires on everything saves the same CPU and stores the same bytes as
// compression turned off, which is the failure mode this table exists to rule out.
func TestCompressSkipsAlreadyCompressedData(t *testing.T) {
	t.Parallel()

	// A genuine zstd frame rather than the four magic bytes with noise behind them, so that at least
	// one case in this table is a real object of a real format. ".tar.zst" is also the case with the
	// most direct claim on this project: it is what pkg/archive is about.
	realZstd := func() []byte {
		codec, err := New(comprpkg.AlgorithmZstd, comprpkg.DefaultLevel)
		if err != nil {
			t.Fatalf("building a zstd codec for the fixture: %v", err)
		}

		frame, err := codec.Compress(bytes.Repeat([]byte("chr1\t10583\trs58108140\tG\tA\t100\tPASS\n"), 512))
		if err != nil {
			t.Fatalf("compressing the fixture: %v", err)
		}

		return frame
	}()

	// A tar whose "ustar" magic sits at offset 257, which is what makes it an archive rather than an
	// unknown binary — and the reason the control below is meaningful: a tar is a container of whatever
	// it holds, so it is compressible and must not be skipped.
	tarHeader := func() []byte {
		block := make([]byte, 8192)
		copy(block, "reference/GRCh38.fa")
		copy(block[257:], "ustar\x0000")
		copy(block[1024:], bytes.Repeat([]byte(">chr1 assembled sequence\nACGTACGTACGTACGT\n"), 128))

		return block
	}()

	for _, tc := range []struct {
		name string
		data []byte
		// skip is whether Compress must return without entering the codec.
		skip bool
		why  string
	}{
		{"tar.zst", realZstd, true, "a real zstd frame, which is what pkg/archive stores"},
		{"tar.gz or BAM", payload("\x1f\x8b\x08\x00", 8192), true, "gzip, and BGZF inside a BAM is gzip per block"},
		{"tar.bz2", payload("BZh9", 8192), true, "bzip2"},
		{"lz4", payload("\x04\x22\x4d\x18", 8192), true, "an LZ4 frame"},
		{"xz", payload("\xfd7zXZ\x00", 8192), true, "xz"},
		{"zip or xlsx", payload("PK\x03\x04", 8192), true, "ZIP, and every Office and Zarr-in-zip file"},
		{"png", payload("\x89PNG\r\n\x1a\n", 8192), true, "PNG, which is DEFLATE per scanline"},
		{"jpeg", payload("\xff\xd8\xff\xe0", 8192), true, "JPEG"},
		{"mp4", append([]byte("\x00\x00\x00\x20ftypisom"), highEntropyTail(8172)...), true, "MP4/MOV, magic at offset 4"},
		{"mkv", payload("\x1a\x45\xdf\xa3", 8192), true, "Matroska"},
		{"pdf", payload("%PDF-1.7\n", 8192), true, "PDF, whose streams are DEFLATE"},

		{"plain tar", tarHeader, false, "an uncompressed container: compressible, and must not be skipped"},
		{
			"plain text",
			bytes.Repeat([]byte("chr1\t10583\trs58108140\tG\tA\t100\tPASS\n"), 256),
			false,
			"an uncompressed VCF, which is what this feature is for",
		},
		{
			"json",
			bytes.Repeat([]byte(`{"sample":"NA12878","depth":31,"pass":true},`), 256),
			false,
			"a JSON document",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, spy := spyCompressor(t)

			got, wasCompressed, err := c.Compress(tc.data)
			if err != nil {
				t.Fatalf("Compress: %v", err)
			}

			if tc.skip {
				if spy.calls != 0 {
					t.Errorf("the codec ran %d time(s) over %d bytes of %s.\n"+
						"#184 is about not spending a compression pass on data that is already at its "+
						"entropy limit — %s. Note that comparing the returned bytes would not have caught "+
						"this: the size fallback in Compress returns the original either way.",
						spy.calls, len(tc.data), tc.name, tc.why)
				}

				if wasCompressed {
					t.Error("wasCompressed is true for data that was not compressed")
				}

				if !bytes.Equal(got, tc.data) {
					t.Error("skipped data must come back byte-identical")
				}

				return
			}

			if spy.calls != 1 {
				t.Errorf("the codec ran %d time(s) over %s, want exactly 1.\n"+
					"This case is a control: %s. A skip that fires here is compression switched off "+
					"under the name of an optimization.", spy.calls, tc.name, tc.why)
			}

			if !wasCompressed {
				t.Errorf("%s did not compress, so this control cannot tell a skip from a codec that "+
					"declined; pick a more compressible fixture", tc.name)
			}
		})
	}
}

// TestCompressSkipHonoursTheSizeFloor asserts the two gates stay independent.
//
// Below min_size nothing is compressed and nothing is analyzed, which is worth pinning: Analyze is
// cheap but it is not free, and a write path that samples every 100-byte object to decide not to
// compress it has moved work into the hot path to answer a question the length already answered.
func TestCompressSkipHonoursTheSizeFloor(t *testing.T) {
	t.Parallel()

	c, err := NewCompressor(makeConfig(true, "zstd", "1MB", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	spy := &countingCodec{inner: c.codec}
	c.codec = spy

	// Compressible text, so the only reason not to compress it is the floor.
	small := bytes.Repeat([]byte("sequence\n"), 64)

	got, wasCompressed, err := c.Compress(small)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if wasCompressed || spy.calls != 0 || !bytes.Equal(got, small) {
		t.Errorf("below min_size: wasCompressed=%v codec calls=%d bytes preserved=%v; want false, 0, true",
			wasCompressed, spy.calls, bytes.Equal(got, small))
	}
}

// TestCompressDisabledDoesNotAnalyze asserts a disabled compressor still analyzes nothing.
//
// With compression off, Enabled() returns false on the first line and the object is never sampled.
// This is here because the ordering inside Compress is load-bearing and invisible from outside: moving
// the Analyze call above the Enabled check would make every write on every mount — the default
// configuration included — sample 4 KiB to reach a decision already made.
func TestCompressDisabledDoesNotAnalyze(t *testing.T) {
	t.Parallel()

	c, err := NewCompressor(makeConfig(false, "zstd", "0", 0))
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}

	spy := &countingCodec{inner: c.codec}
	c.codec = spy

	data := bytes.Repeat([]byte("sequence\n"), 4096)

	got, wasCompressed, err := c.Compress(data)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if wasCompressed || spy.calls != 0 || !bytes.Equal(got, data) {
		t.Errorf("disabled: wasCompressed=%v codec calls=%d bytes preserved=%v; want false, 0, true",
			wasCompressed, spy.calls, bytes.Equal(got, data))
	}
}

// TestAlreadyCompressedMatchesTheDocumentedFormatLists pins what docs/features/compression.md tells a
// user about their own bucket.
//
// That page has two lists — formats ObjectFS skips, and formats whose compression is internal and so
// invisible to a check on the container — and the second list is the one that decides whether to enable
// compression at all. A reader with a Parquet bucket needs the answer to be right, and a documentation
// claim about a magic-byte table is exactly the kind that goes stale silently. This test is the check.
func TestAlreadyCompressedMatchesTheDocumentedFormatLists(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		magic []byte
		// want is whether docs/features/compression.md says this format is skipped.
		want bool
	}{
		// Compressed at the container level: the page says these are skipped.
		{"BAM (BGZF, gzip magic at offset 0)", []byte("\x1f\x8b\x08\x04\x00\x00\x00\x00"), true},
		{"tar.gz", []byte("\x1f\x8b\x08\x00"), true},
		{"tar.zst", []byte("\x28\xb5\x2f\xfd"), true},
		{"tar.bz2", []byte("BZh9"), true},

		// Compression internal to the format: the page says these are *not* skipped, and says why.
		{"CRAM", []byte("CRAM\x03\x00"), false},
		{"Parquet", []byte("PAR1\x15\x04\x15"), false},
		{"ORC", []byte("ORC\x00\x00\x00"), false},
		{"HDF5 / NetCDF-4", []byte("\x89HDF\r\n\x1a\n"), false},
		{"NetCDF-3", []byte("CDF\x01\x00\x00"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data := append(append([]byte{}, tc.magic...), highEntropyTail(4096)...)

			if got := AlreadyCompressed(data); got != tc.want {
				t.Errorf("AlreadyCompressed(%s) = %v, want %v.\n"+
					"docs/features/compression.md sorts this format into the other list. Either the "+
					"magic-byte table changed and that page needs the same edit, or this expectation is "+
					"the stale one — but they must agree, because a user decides whether to enable "+
					"compression from that page.", tc.name, got, tc.want)
			}
		})
	}
}
