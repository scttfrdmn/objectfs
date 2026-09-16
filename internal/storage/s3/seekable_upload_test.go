package s3_test

// The write half of seekable framing (#185). What these tests establish is that a framed object is
// simultaneously two things: an object carrying enough metadata to seek within, and an ordinary zstd
// object that every existing reader — this backend's own whole-object path, `aws s3 cp`, zstd(1) —
// still decodes correctly. If it were only the first, shipping the write path ahead of the read path
// would corrupt every object written in between.
//
// The read path does not exist yet, so nothing here asserts a *cheaper* read. That belongs with the
// code that makes it cheaper, measured in bytes transferred rather than in latency. What is asserted
// is that the ranged reads a user issues today return the right bytes from a framed object, which is
// the property the framing must not have broken.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/scttfrdmn/objectfs/internal/compression"
	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// metaSeekableKey is spelled out rather than imported, for the reason given on metaChecksumKey: it is
// part of the stored object format. Renaming it makes every previously-written object read as
// unframed, which is silent — the object still decodes, it just costs a whole-object GET forever —
// and a test that imported the constant would follow the rename and ratify that.
const metaSeekableKey = "objectfs-seekable"

// framedObjectSize is large enough to hold many frames at DeriveFrameSize's 256 KiB floor, which is
// where every object a test can afford to build lands: the optimum only rises above the floor at
// around 100 MB of compressible content.
const framedObjectSize = 4 << 20

// framedObjectFrames is how many frames framedObjectSize must produce for the range table below to
// mean anything. Asserted rather than assumed, because the frame count is derived from the measured
// compression ratio and a change to either the fixture or the cost model moves it.
const framedObjectFrames = 4

// callerSuppliedDescriptor is what a caller attempting to set the descriptor sends. It is a
// well-formed, self-consistent descriptor for some *other* object — 64 frames of 1 MiB with a
// 2664-byte index — deliberately, because that is the strongest adversary: a malformed value would be
// rejected by the reader's own parse even if the filter let it through, so a test using one would pass
// whether or not the filter exists.
const callerSuppliedDescriptor = "1/1048576/64/2664"

// putFramed writes a framed object and returns its content, its parsed descriptor, and the stored
// bytes. It fails the test if framing did not engage, because every assertion after it would otherwise
// pass vacuously against an ordinary compressed object.
//
// The content is only ~50% compressible, deliberately. compressible() shrinks by about 5000:1, and
// DeriveFrameSize scales the frame size with the square root of the ratio — so that fixture produced a
// 1 MiB frame size, four frames of a few hundred stored bytes each, and a range table that ran off the
// end of the file. A realistic ratio is also the one a real object has.
func putFramed(t *testing.T, ts *testaws.TestServer, backend *s3.Backend, key string) (
	content []byte, desc compression.SeekableDescriptor, stored []byte,
) {
	t.Helper()

	content = semiCompressible(key, framedObjectSize)
	if err := backend.PutObject(context.Background(), key, content, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	meta := ts.ObjectMetadata(key)

	text, ok := meta[metaSeekableKey]
	if !ok {
		t.Fatalf("a %d-byte compressible object carries no %s, so it was stored as a single frame and "+
			"every seekability assertion here would pass without testing anything. Metadata was %v",
			framedObjectSize, metaSeekableKey, meta)
	}

	desc, err := compression.ParseSeekableDescriptor(text)
	if err != nil {
		t.Fatalf("the descriptor this backend stored does not parse: %q: %v", text, err)
	}

	if desc.FrameCount < framedObjectFrames {
		t.Fatalf("a %d-byte object framed into %d frames of %d, and the callers here need at least %d "+
			"to place a range that spans an interior boundary. Either the fixture's compressibility or "+
			"DeriveFrameSize's cost model moved", framedObjectSize, desc.FrameCount, desc.FrameSize,
			framedObjectFrames)
	}

	return content, desc, ts.GetObject(key)
}

// TestFramedUploadStoresADescriptorThatDescribesTheObject is the write path's whole contract. The
// descriptor is an accelerator: a reader trusts it to size one request and then finds the index where
// it said. So the assertion that matters is not that the descriptor round-trips — the compression
// package proves that — but that it agrees with the bytes this backend actually uploaded.
//
// Built on the shipped default configuration rather than a hand-assembled one. A config assembled
// field by field would keep passing if a default changed underneath it, and the default is what users
// get.
func TestFramedUploadStoresADescriptorThatDescribesTheObject(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	const key = "seekable/descriptor"

	want, desc, stored := putFramed(t, ts, backend, key)

	// The index has to parse from exactly the prefix the descriptor names. This is the one claim a
	// reader cannot check before spending a request on it, which makes it the one worth pinning: an
	// IndexLength too small costs a second round trip, and one too large costs up to 4 GiB of transfer.
	if int64(len(stored)) < desc.IndexLength {
		t.Fatalf("descriptor names a %d-byte index prefix but the stored object is only %d bytes",
			desc.IndexLength, len(stored))
	}

	idx, consumed, err := compression.ParseFrameIndex(stored[:desc.IndexLength])
	if err != nil {
		t.Fatalf("the index does not parse from the first %d bytes the descriptor names: %v",
			desc.IndexLength, err)
	}
	if int64(consumed) != desc.IndexLength {
		t.Errorf("the index occupies %d bytes but the descriptor names %d; a reader sized on the "+
			"descriptor would fetch the wrong prefix", consumed, desc.IndexLength)
	}

	if int64(len(idx.Frames)) != desc.FrameCount {
		t.Errorf("index holds %d frames, descriptor says %d", len(idx.Frames), desc.FrameCount)
	}
	if idx.FrameSize != desc.FrameSize {
		t.Errorf("index frame size %d, descriptor says %d", idx.FrameSize, desc.FrameSize)
	}
	if idx.UncompressedSize != int64(len(want)) {
		t.Errorf("index records %d uncompressed bytes for a %d-byte object",
			idx.UncompressedSize, len(want))
	}
	if desc.FrameCount < 2 {
		t.Fatalf("object framed into %d frame(s); there is nothing to seek to and the range subtests "+
			"would prove nothing", desc.FrameCount)
	}

	// The index's own hash and the metadata checksum are the same fact recorded twice, deliberately:
	// the index survives a CopyObject that drops metadata, and the metadata survives a reader with no
	// frame support. They must agree, or one of the two verification paths is checking against a value
	// the other would reject.
	sum := sha256.Sum256(want)
	if !idx.HasContentSHA256 || idx.ContentSHA256 != sum {
		t.Errorf("index content hash present=%v value=%x, want %x",
			idx.HasContentSHA256, idx.ContentSHA256, sum)
	}
	if got := ts.ObjectMetadata(key)[metaChecksumKey]; got != sha256Hex(want) {
		t.Errorf("%s = %q, want %q", metaChecksumKey, got, sha256Hex(want))
	}

	// A framed object is still a compressed object, so everything the compression path is responsible
	// for has to be there too. The original size especially: HeadObject reports it as the file size,
	// and the kernel truncates every read at whatever it says.
	meta := ts.ObjectMetadata(key)
	if got := meta[metaOriginalSizeKey]; got != strconv.Itoa(len(want)) {
		t.Errorf("%s = %q, want %q", metaOriginalSizeKey, got, strconv.Itoa(len(want)))
	}

	head, err := ts.Client().HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(ts.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if enc := aws.ToString(head.ContentEncoding); enc != "zstd" {
		t.Errorf("stored Content-Encoding = %q, want %q. The index frame is skippable, so an object "+
			"that lost its header is a raw zstd stream no client will decode", enc, "zstd")
	}

	if int64(len(stored)) >= int64(len(want)) {
		t.Errorf("framed body is %d bytes for %d bytes of content, which should have been declined "+
			"in favor of storing it uncompressed", len(stored), len(want))
	}

	info, err := backend.HeadObject(ctx, key)
	if err != nil {
		t.Fatalf("backend.HeadObject: %v", err)
	}
	if info.Size != int64(len(want)) {
		t.Errorf("HeadObject reports %d bytes for a %d-byte file", info.Size, len(want))
	}

	// And the compatibility guarantee: the existing whole-object read path, which knows nothing about
	// framing, must still return the file byte for byte. This is what makes the write path safe to ship
	// on its own — the leading index is a skippable frame, so a plain zstd decode walks straight past it.
	got, err := backend.GetObject(ctx, key, 0, info.Size)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("whole-object read of a framed object returned %d bytes that differ from the %d written",
			len(got), len(want))
	}

	t.Logf("%d bytes -> %d stored in %d frames of %d, descriptor %q (%d bytes of metadata)",
		len(want), len(stored), desc.FrameCount, desc.FrameSize, desc.String(), len(desc.String()))
}

// TestFramedObjectServesRangedReads is the regression test for the risk this stage actually carries.
// Framing changes the stored bytes of every compressible object a mount writes, and the reader is
// unchanged: it decodes the whole stored body and slices. So the leading index frame has to be
// invisible to that slicing, and an off-by-one in where the content begins would show up as every
// ranged read being shifted — which no whole-object test can see.
//
// The offsets are chosen against the frame layout rather than at round numbers, because a boundary is
// where the read path the next stage adds will be at risk, and the answers must not change when it
// lands.
func TestFramedObjectServesRangedReads(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireRangeGET()

	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	const key = "seekable/ranges"

	want, desc, stored := putFramed(t, ts, backend, key)
	frame := desc.FrameSize

	for _, tc := range []struct {
		name   string
		offset int64
		length int64
	}{
		{"first byte", 0, 1},
		{"head of the first frame", 0, 100},
		{"straddling the first frame boundary", frame - 1, 2},
		{"exactly at a frame boundary", frame, 1},
		{"one whole interior frame", frame, frame},
		{"spanning three frames", frame*2 - 10, frame + 20},
		{"the last frame", frame * (desc.FrameCount - 1), int64(len(want)) - frame*(desc.FrameCount-1)},
		{"final byte", int64(len(want)) - 1, 1},
		{"the whole file", 0, int64(len(want))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := backend.GetObject(ctx, key, tc.offset, tc.length)
			if err != nil {
				t.Fatalf("GetObject(%d, %d): %v", tc.offset, tc.length, err)
			}

			expect := want[tc.offset : tc.offset+tc.length]
			if !bytes.Equal(got, expect) {
				// Report where it went wrong rather than dumping megabytes. A uniform shift is the
				// failure mode this test is looking for, so the first differing index is the diagnosis.
				at := 0
				for at < len(got) && at < len(expect) && got[at] == expect[at] {
					at++
				}
				t.Errorf("GetObject(%d, %d) returned %d bytes, want %d, first differing at %d",
					tc.offset, tc.length, len(got), len(expect), at)
			}
		})
	}

	// Not an assertion. The read path is still whole-object, so a one-byte read transfers the entire
	// stored body; this records the amplification the next stage removes, in the units that stage will
	// be measured in.
	t.Logf("stored body is %d bytes; a single-byte read transfers all of it today, against %d bytes "+
		"of index plus one %d-byte frame once the read path lands",
		len(stored), desc.IndexLength, frame)
}

// TestObjectsBelowTheFramingThresholdCarryNoDescriptor covers the declines from the storage side.
// Each one is an ordinary outcome that reports nothing, and a stray descriptor is worse than a missing
// one: a reader would size a prefix GET from it and parse whatever bytes came back as an index.
func TestObjectsBelowTheFramingThresholdCarryNoDescriptor(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		data []byte
		why  string
	}{
		{
			name: "below the compression minimum",
			data: compressible(1024),
			why:  "nothing was compressed, so there are no frames",
		},
		{
			name: "compressed but a single frame",
			data: compressible(8192),
			why:  "8 KiB is far below the 256 KiB frame floor; an index over one frame buys nothing",
		},
		{
			name: "large but incompressible",
			data: testaws.DeterministicBytes("seekable/incompressible", framedObjectSize),
			why:  "the framed body is no smaller than the input, so it is discarded as in Compress",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			key := "seekable/declined/" + tc.name

			if err := backend.PutObject(ctx, key, tc.data, nil); err != nil {
				t.Fatalf("PutObject: %v", err)
			}

			if got, ok := ts.ObjectMetadata(key)[metaSeekableKey]; ok {
				t.Errorf("object carries %s=%q but should not have been framed: %s",
					metaSeekableKey, got, tc.why)
			}

			got, err := backend.GetObject(ctx, key, 0, int64(len(tc.data)))
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}
			if !bytes.Equal(got, tc.data) {
				t.Errorf("round trip of %d bytes returned %d that differ", len(tc.data), len(got))
			}
		})
	}
}

// TestTheDescriptorIsNotCallerWritable pins both halves of the ownership rule. The descriptor
// describes bytes only this backend has seen, so a caller must never be able to set it — and a caller
// round-tripping metadata it read from HeadObject will carry it, so the write must not fail either.
// Dropping the entry silently is the only behavior that satisfies both, and it is the behavior the
// two integrity keys already have.
func TestTheDescriptorIsNotCallerWritable(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	// An object that will not be framed, so nothing overwrites the caller's value: if the filter were
	// missing, the garbage would be what a reader found. A framed object would hide the bug behind the
	// real descriptor computed moments later.
	const key = "seekable/caller-supplied"

	want := compressible(8192)
	err := backend.PutObject(ctx, key, want, map[string]string{
		metaSeekableKey: callerSuppliedDescriptor,
		"objectfs-mode": "644",
	})
	if err != nil {
		t.Fatalf("PutObject with a caller-supplied descriptor failed; a caller round-tripping "+
			"metadata from HeadObject would be unable to write: %v", err)
	}

	meta := ts.ObjectMetadata(key)
	if got, ok := meta[metaSeekableKey]; ok {
		t.Errorf("caller-supplied %s survived as %q. A reader would size a prefix GET from it and "+
			"parse unrelated bytes as an index", metaSeekableKey, got)
	}
	if got := meta["objectfs-mode"]; got != "644" {
		t.Errorf("objectfs-mode = %q, want %q: the filter dropped more than the keys it owns",
			got, "644")
	}
}

// TestSetObjectMetadataPreservesTheDescriptor is the chmod case. A metadata rewrite is a CopyObject
// with MetadataDirective=REPLACE, which discards every user-metadata key the request does not restate
// — and the caller doing the chmod is internal/vfs, which knows nothing about framing and has no way
// to restate a descriptor it never read.
//
// A dropped descriptor is not corruption: the index is still in the object and the whole-object path
// still works. It is a permanent, silent performance regression, which is the harder kind to notice —
// so it is pinned here rather than left to be discovered as "reads got slow after someone ran chmod".
func TestSetObjectMetadataPreservesTheDescriptor(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	ts.RequireMetadataReplace()

	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	const key = "seekable/chmod"

	want, desc, _ := putFramed(t, ts, backend, key)

	// Exactly what a chmod sends: the POSIX attributes, and an attempt on the descriptor to prove the
	// filter is applied on this path too. Both must end the same way — the object's own value.
	err := backend.SetObjectMetadata(ctx, key, map[string]string{
		"objectfs-mode":  "600",
		"objectfs-uid":   "1000",
		metaSeekableKey:  callerSuppliedDescriptor,
		metaChecksumKey:  strings.Repeat("0", 64),
		"objectfs-xattr": "irrelevant",
	})
	if err != nil {
		t.Fatalf("SetObjectMetadata: %v", err)
	}

	meta := ts.ObjectMetadata(key)

	if got := meta[metaSeekableKey]; got != desc.String() {
		t.Errorf("%s = %q after a metadata rewrite, want %q. A caller's value must not win and the "+
			"object's must not be dropped", metaSeekableKey, got, desc.String())
	}
	if got := meta[metaOriginalSizeKey]; got != strconv.Itoa(len(want)) {
		t.Errorf("%s = %q after a metadata rewrite, want %q", metaOriginalSizeKey, got,
			strconv.Itoa(len(want)))
	}
	if got := meta[metaChecksumKey]; got != sha256Hex(want) {
		t.Errorf("%s = %q after a metadata rewrite; the caller's zeros won", metaChecksumKey, got)
	}
	if got := meta["objectfs-mode"]; got != "600" {
		t.Errorf("objectfs-mode = %q, want %q: the caller's own attributes must be written", got, "600")
	}

	// The descriptor is worthless if the bytes it describes moved, and REPLACE is a rewrite: it must
	// still name a prefix the index parses from, and the content must still read back.
	stored := ts.GetObject(key)
	if _, _, err := compression.ParseFrameIndex(stored[:desc.IndexLength]); err != nil {
		t.Errorf("after a metadata rewrite the index no longer parses from the %d bytes the "+
			"descriptor names: %v", desc.IndexLength, err)
	}

	got, err := backend.GetObject(ctx, key, 0, int64(len(want)))
	if err != nil {
		t.Fatalf("GetObject after a metadata rewrite: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("content changed across a metadata rewrite: read %d bytes of %d that differ",
			len(got), len(want))
	}
}
