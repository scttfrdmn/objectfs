package compression

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// SeekableDescriptor is the fixed-size summary of a framed object that fits in one S3 user-metadata
// value. It is an accelerator, never the authority: the index frame inside the object is.
//
// What it buys is the cold read. Without it a reader that wants bytes from the middle of a
// compressed object must first learn whether the object is framed at all and how long the index is,
// which is one request it can only skip if something in the response it already had told it. A
// GetObject or HeadObject response carries user metadata on a 206 exactly as on a 200, so a
// descriptor in metadata is free — it arrives with the speculative ranged read the backend already
// makes — and it names the exact prefix length the index occupies, so the index fetch is one request
// of the right size rather than a guess plus a correction.
//
// Why it must not be authoritative: SetObjectMetadata is a CopyObject with
// MetadataDirective=REPLACE, which discards every stored property not restated, and a tier
// transition does the same. That mechanism has already made a chmod capable of dropping
// Content-Encoding in this codebase. If the descriptor were the only record that an object is
// framed, a chmod would silently turn a seekable object into a whole-object-read object — a
// permanent performance regression with nothing reporting it. Because the index is in the object,
// losing the descriptor costs one round trip and nothing else, and [ParseFrameIndex] over a prefix
// recovers everything here.
//
// The text form is `version/frameSize/frameCount/indexLength`, e.g. `1/1048576/64/2648`. Text
// rather than base64-packed binary for two reasons: S3 user metadata is a header value, so a text
// form survives every tool in the path unchanged and is readable in `aws s3api head-object` output;
// and at these magnitudes decimal is *shorter* than base64 of the equivalent fixed-width fields
// (36 bytes at the very widest against 60), which matters because the budget it spends is taken
// from extended attributes.
type SeekableDescriptor struct {
	// Version is the format version, mirroring [FrameIndex.Version]. It appears in both places
	// because the descriptor can be lost by a CopyObject and the frame cannot.
	Version uint8

	// FrameSize is the uncompressed bytes per frame, as chosen by [DeriveFrameSize] at write time.
	// Recorded rather than recomputed so that changing the derivation rule does not change how
	// existing objects are read.
	FrameSize int64

	// FrameCount is the number of data frames following the index.
	FrameCount int64

	// IndexLength is the total length of the leading index frame, skippable header included. This is
	// the field the reader actually spends: `GET bytes=0-(IndexLength-1)` is the index, exactly.
	IndexLength int64
}

// descriptorFields is the number of slash-separated fields in the text form. Named because both
// String and ParseSeekableDescriptor have to agree, and a mismatch would be a parser that silently
// accepts a truncated descriptor.
const descriptorFields = 4

// MaxSeekableDescriptorLen is the largest number of bytes [SeekableDescriptor.String] can return.
//
// Derived from the format's field widths rather than counted by hand, so that a change to the text
// form moves the figure that depends on it — internal/vfs reserves this many metadata bytes, and a
// stale reservation means either a wasted budget or a PUT that S3 rejects outright.
//
// The maxima used are the *wire* widths (uint32 per field), not the policy limits MaxFrameSize and
// maxIndexPayload. A policy limit can be relaxed in a patch release; a wire width cannot change
// without a format version, so reserving against the wire keeps the reservation correct across any
// tuning change.
var MaxSeekableDescriptorLen = len(SeekableDescriptor{
	Version:     math.MaxUint8,
	FrameSize:   math.MaxUint32,
	FrameCount:  math.MaxUint32,
	IndexLength: math.MaxUint32,
}.String())

// DescribeFrameIndex builds the descriptor for idx, whose index frame occupies indexLength bytes at
// the head of the stored object.
//
// indexLength is passed rather than recomputed so that the value in the descriptor is the one the
// writer actually emitted. Deriving it here from the frame count would produce a descriptor that
// agrees with a formula instead of with the object, which is the failure the descriptor's own
// self-consistency check below is meant to catch.
func DescribeFrameIndex(idx *FrameIndex, indexLength int64) SeekableDescriptor {
	return SeekableDescriptor{
		Version:     idx.Version,
		FrameSize:   idx.FrameSize,
		FrameCount:  int64(len(idx.Frames)),
		IndexLength: indexLength,
	}
}

// String renders the descriptor as the S3 user-metadata value.
func (d SeekableDescriptor) String() string {
	return strconv.FormatUint(uint64(d.Version), 10) + "/" +
		strconv.FormatInt(d.FrameSize, 10) + "/" +
		strconv.FormatInt(d.FrameCount, 10) + "/" +
		strconv.FormatInt(d.IndexLength, 10)
}

// ParseSeekableDescriptor decodes the text form.
//
// Every field is validated, and the two that are redundant are cross-checked, because a descriptor
// is read from a place a stranger can write: S3 user metadata is settable by anyone with
// s3:PutObject, and `aws s3 cp --metadata` will carry a garbled one through faithfully. A reader
// that trusted IndexLength would issue a prefix GET of an arbitrary size — up to 4 GiB from a
// four-byte edit — before anything in the object could contradict it. So the descriptor has to be
// self-consistent before it is spent, and even then what it produces is checked against the index's
// own hash.
//
// A parse failure is not a corruption signal about the object. The caller's correct response is to
// ignore the descriptor and fall back to reading the object's own index, which is authoritative;
// the only cost is a round trip. That is why this returns a plain error rather than
// [ErrIndexCorrupt].
func ParseSeekableDescriptor(s string) (SeekableDescriptor, error) {
	parts := strings.Split(s, "/")
	if len(parts) != descriptorFields {
		return SeekableDescriptor{}, fmt.Errorf("seekable descriptor %q has %d fields, want %d",
			s, len(parts), descriptorFields)
	}

	version, err := strconv.ParseUint(parts[0], 10, 8)
	if err != nil {
		return SeekableDescriptor{}, fmt.Errorf("seekable descriptor version %q: %w", parts[0], err)
	}

	// Each of the three sizes is a uint32 on the wire, so a value above that did not come from a
	// writer of this format. Parsed at 32 bits so an over-wide field is rejected here rather than
	// becoming a plausible-looking int64 that only fails later.
	nums := make([]int64, 0, descriptorFields-1)
	for i, name := range []string{"frame size", "frame count", "index length"} {
		n, numErr := strconv.ParseUint(parts[i+1], 10, 32)
		if numErr != nil {
			return SeekableDescriptor{}, fmt.Errorf("seekable descriptor %s %q: %w", name, parts[i+1], numErr)
		}
		if n == 0 {
			return SeekableDescriptor{}, fmt.Errorf("seekable descriptor %s is zero", name)
		}
		nums = append(nums, int64(n))
	}

	d := SeekableDescriptor{
		Version:     uint8(version),
		FrameSize:   nums[0],
		FrameCount:  nums[1],
		IndexLength: nums[2],
	}

	// The redundancy is the point. IndexLength is derivable from FrameCount, and storing both means a
	// descriptor that disagrees with itself is rejected without fetching a byte of the object — which
	// is the difference between a wasted round trip and a 4 GiB prefix GET.
	if want := skippableHeaderSize + indexPayloadLen(d.FrameCount); d.IndexLength != want {
		return SeekableDescriptor{}, fmt.Errorf(
			"seekable descriptor is inconsistent: %d frames need a %d-byte index frame, descriptor says %d",
			d.FrameCount, want, d.IndexLength)
	}

	// An unknown version is refused rather than read optimistically. The fields above happen to be
	// the same shape in version 1, so a future version could parse into this struct and mean
	// something else — a reader that guessed would issue confident ranged reads against a layout it
	// does not know. Checked last so the error names the worst problem the descriptor has.
	if d.Version != FrameIndexVersion {
		return SeekableDescriptor{}, fmt.Errorf("seekable descriptor version %d is not %d",
			d.Version, FrameIndexVersion)
	}

	return d, nil
}
