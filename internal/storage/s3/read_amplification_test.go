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

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
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
// It used to also assert that this cost the whole stored body, because a single zstd stream is not
// seekable and there was no way to decode part of one. Seekable framing (#185) removed that
// constraint: the object is written as independently decodable frames with an index, so a range of
// the decoded content is served from the frames covering it. The test now asserts both halves —
// still the right bytes, and no longer the whole body — because the correctness half is the one a
// naive "always range" fix breaks, and it is the half that must survive any amplification work.
//
//nolint:tparallel // the subtests share a request recorder and must run in order; see below
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

	// The byte budget below only means something if the object was framed. Without this check, a
	// regression that stopped emitting descriptors — or a fixture that drifted under the frame-size
	// floor and came out as a single unframed stream — would make the reads fall back to whole-object
	// transfer, and the budget would then be measuring nothing while still passing or failing for
	// reasons unrelated to framing.
	if desc, ok := ts.ObjectMetadata(key)[metaSeekableKey]; !ok {
		t.Fatalf("the object carries no %s descriptor, so it was not framed and the transfer budget "+
			"below would not be an assertion about seekable reads. Metadata: %v",
			metaSeekableKey, ts.ObjectMetadata(key))
	} else {
		t.Logf("framed: %s = %s", metaSeekableKey, desc)
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

	// Not parallel: each case calls ResetRequests and then asserts on what the endpoint saw, so a
	// concurrent sibling's traffic would land inside the window under assertion. Counting requests is
	// the entire point of this test — it is what distinguishes a correct read from a correct read that
	// transferred the whole object — so serial is the requirement, not a limitation.
	//nolint:paralleltest // shared request recorder; the cases must run in order, see above
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

			// This assertion used to read the other way round: `n < stored` was the *failure*
			// condition, because a zstd stream cannot be sliced and the whole body had to cross the
			// wire to decode any of it. Its comment said that if a later change ever made this cheap,
			// this was the assertion to revisit rather than quietly satisfy. Seekable framing (#185)
			// is that change, and this is that revision.
			//
			// The bound is half the stored body rather than a precise byte count. What the read
			// actually transfers is the index frame plus the one data frame covering the offset — for
			// this fixture about 138 KiB against a 537 KiB body — and pinning that exactly would make
			// this test fail on any change to the frame-size derivation, which is a tuning decision
			// with its own tests. Half is far inside what framing buys and far outside what the
			// whole-object path could ever produce, so it distinguishes the two without pinning either.
			n := ts.BytesRead(key)
			if n >= stored/2 {
				t.Errorf("read %d bytes to serve %d bytes at offset %d of a framed object, against a "+
					"%d-byte stored body. A framed object should cost the index plus the frames the "+
					"range covers; this looks like a whole-object fetch. Requests: %s",
					n, readSize, r.offset, stored, describe(ts.Requests()))
			}

			t.Logf("%d bytes at offset %d transferred %d of %d stored bytes (%.1f%%)",
				readSize, r.offset, n, stored, float64(n)/float64(stored)*100)
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
