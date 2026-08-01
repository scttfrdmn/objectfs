package s3_test

// Read amplification is a byte-count property, so these tests assert bytes transferred, not latency.
// The v0.10.0 audit measured a 4 KiB read of a 256 MiB object taking 49 seconds against real S3, but
// the defect is that it transferred 256 MiB — and a latency assertion for that would be a flaky
// proxy for the thing it means to measure.

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/objectfs/objectfs/internal/storage/s3"
	"github.com/objectfs/objectfs/internal/testaws"
)

// TestSmallReadOfLargeObjectDoesNotFetchTheWholeThing is the C4 regression test.
//
// backend.go set fetchOffset, fetchSize = 0, 0 whenever the compression *config* was enabled — for
// every object in the bucket, including objects never compressed, objects below MinSize, objects
// where compression did not help, and objects written by other tools entirely. A 4 KiB read of a
// 10 GiB object transferred 10 GiB.
//
// The decision has to come from the object, not the flag.
func TestSmallReadOfLargeObjectDoesNotFetchTheWholeThing(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	const (
		objectSize = 4 << 20
		readSize   = 4096
		readAt     = 1 << 20
	)

	cases := []struct {
		name string
		// compressionEnabled is the config flag. The point of the test is that it must not change
		// how an *uncompressed* object is read.
		compressionEnabled bool
	}{
		{"compression off", false},
		{"compression on", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			backend := ts.Backend(func(cfg *s3.Config) {
				cfg.Compression.Enabled = tc.compressionEnabled
				cfg.Compression.Algorithm = "zstd"
				cfg.Compression.Level = 3
				cfg.Compression.MinSize = "4KB"

				// Parallel reads are their own path with their own accounting; this test is about
				// the serial one.
				cfg.ParallelReadThreshold = 0
			})

			// Written by an independent client, so the object carries no ObjectFS metadata and no
			// Content-Encoding — the overwhelmingly common case in a bucket, and the one the
			// config-keyed decision got wrong.
			const key = "amplification/uncompressed"

			body := testaws.DeterministicBytes(key, objectSize)
			ts.PutObject(key, body)
			ts.ResetRequests()

			got, err := backend.GetObject(context.Background(), key, readAt, readSize)
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}

			if !bytes.Equal(got, body[readAt:readAt+readSize]) {
				t.Fatalf("read %d bytes at offset %d that do not match the object", len(got), readAt)
			}

			// The assertion that matters. A whole-object fetch reads objectSize bytes; a correct
			// ranged read reads readSize. Allowing 4x headroom keeps this from being brittle about
			// chunking or a probe read, while still failing loudly on a 1024x whole-object fetch.
			const budget = readSize * 4

			if n := ts.BytesRead(key); n > budget {
				t.Errorf("a %d-byte read of a %d-byte object transferred %d bytes (%.1fx "+
					"amplification, budget %d). Requests: %s",
					readSize, objectSize, n, float64(n)/float64(readSize), budget,
					describe(ts.Requests()))
			}

			// And it must actually be a ranged request, not a whole-object fetch the recorder
			// happened to see truncated.
			gets := ts.GETs(key)
			if len(gets) == 0 {
				t.Fatalf("no GET was recorded for %q", key)
			}

			for _, g := range gets {
				if !g.IsRanged() {
					t.Errorf("GET %s was unranged; a small read of a large object must send a "+
						"Range header", g.Path)
				}
			}
		})
	}
}

// TestSmallReadOfCompressedObjectStaysCorrect is the other half of the C4 fix: reading a range of an
// object that *is* compressed still has to return the right bytes.
//
// A zstd frame is not seekable, so a range of the decoded content cannot be served from a range of
// the stored bytes — the whole object must be fetched and decoded. Amplification is the price of
// compression here, and it is a documented tradeoff rather than a defect. What would be a defect is
// returning the wrong bytes, which is what a naive "always range" fix would do.
func TestSmallReadOfCompressedObjectStaysCorrect(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = true
		cfg.Compression.Algorithm = "zstd"
		cfg.Compression.Level = 3
		cfg.Compression.MinSize = "4KB"
		cfg.ParallelReadThreshold = 0
	})

	ctx := context.Background()

	const (
		key        = "amplification/compressed"
		objectSize = 1 << 20
		readSize   = 4096
		readAt     = 4096 * 3
	)

	// Only ~50% compressible. compressible() shrinks 1 MiB to about 130 bytes, which would put every
	// offset past the end of the stored body and collapse the two cases below into one.
	body := semiCompressible(key, objectSize)
	if err := backend.PutObject(ctx, key, body, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if stored := ts.ObjectSize(key); stored >= objectSize {
		t.Fatalf("stored size %d did not shrink below %d; compression did not engage and this test "+
			"proves nothing", stored, objectSize)
	}

	// Two reads: one whose range falls inside the compressed body, and one whose range falls past
	// the end of it. Both are legitimate reads of the decoded content, and they reach the whole-object
	// re-fetch by different routes — an encoded 206 and a 416 respectively. Only the second one
	// existed as a bug; the first is here so a change that fixes one and breaks the other is caught.
	stored := ts.ObjectSize(key)

	reads := []struct {
		name   string
		offset int64
		// wantInsideStored says whether this offset should fall within the compressed body. It is
		// asserted rather than assumed: if the compression ratio moves, one of these cases silently
		// becomes a duplicate of the other and the coverage quietly halves.
		wantInsideStored bool
	}{
		{"range inside the compressed body", 4096, true},
		{"range past the end of the compressed body", objectSize - readSize, false},
	}

	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			if inside := r.offset < stored; inside != r.wantInsideStored {
				t.Fatalf("offset %d against a %d-byte stored body is inside=%v, want inside=%v; "+
					"the compression ratio moved and this case no longer exercises what it names",
					r.offset, stored, inside, r.wantInsideStored)
			}

			ts.ResetRequests()

			got, err := backend.GetObject(ctx, key, r.offset, readSize)
			if err != nil {
				t.Fatalf("GetObject(%d, %d): %v (unwrapped: %v)",
					r.offset, readSize, err, errors.Unwrap(err))
			}

			if want := body[r.offset : r.offset+readSize]; !bytes.Equal(got, want) {
				t.Errorf("a ranged read of a compressed object returned %d bytes that do not match "+
					"body[%d:%d] — the range was applied to the encoded bytes rather than the "+
					"decoded ones", len(got), r.offset, r.offset+readSize)
			}

			// The whole object had to cross the wire, since a zstd frame cannot be sliced. Asserting
			// it keeps the tradeoff explicit: if a later change makes this cheap, this assertion is
			// the one that should be revisited rather than silently satisfied.
			if n := ts.BytesRead(key); n < stored {
				t.Errorf("read %d bytes to decode a %d-byte compressed body; the whole body is "+
					"needed. Requests: %s", n, stored, describe(ts.Requests()))
			}
		})
	}
}

// TestReadPastEndOfObjectIsShortNotAnError pins the EOF behavior a POSIX read depends on: asking
// for more than the object holds returns what is there. The kernel routinely asks for a full
// MaxRead-sized block at the tail of a file.
func TestReadPastEndOfObjectIsShortNotAnError(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	backend := ts.Backend(func(cfg *s3.Config) {
		cfg.Compression.Enabled = false
		cfg.ParallelReadThreshold = 0
	})

	ctx := context.Background()

	const (
		key  = "eof/probe"
		size = 10240
	)

	body := testaws.DeterministicBytes(key, size)
	ts.PutObject(key, body)

	// The kernel's typical tail read: a full 128 KiB block against a 10 KiB file.
	got, err := backend.GetObject(ctx, key, 0, 128*1024)
	if err != nil {
		t.Fatalf("a read longer than the object failed: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Errorf("a %d-byte read of a %d-byte object returned %d bytes that differ from the object",
			128*1024, size, len(got))
	}

	// And a read starting inside the object but running past its end.
	got, err = backend.GetObject(ctx, key, size-100, 4096)
	if err != nil {
		t.Fatalf("a read straddling EOF failed: %v", err)
	}

	if !bytes.Equal(got, body[size-100:]) {
		t.Errorf("a read straddling EOF returned %d bytes, want the final 100", len(got))
	}
}
