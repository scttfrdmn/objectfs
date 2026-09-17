package s3

// The read half of seekable framing (#185). Stages 1 and 2 made ranged reads of a compressed object
// *possible* to serve cheaply; this is what makes them cheap.
//
// What it replaces: a ranged read landing on a compressed object had to fetch and decode the entire
// stored body, because a zstd stream is not seekable. Measured on real S3 before framing existed, a
// 4 KiB read of a 10 GiB compressed object transferred all 10 GiB. With frames it transfers the index
// plus one frame — at the 256 KiB floor, about 100 KiB of stored bytes.
//
// Every path out of here that is not "served from frames" is a *correct* read of the object, and that
// is the hazard this file is written around. Nothing fails when framing does not engage; the read
// just costs what it cost before. So each decline is counted and logged with a reason
// (RecordSeekableWholeFallback), because otherwise the only symptom of the feature quietly ceasing to
// work is that someone eventually notices reads are slow, with no evidence attached.

import (
	"context"
	"strings"

	"github.com/scttfrdmn/objectfs/internal/compression"
	"github.com/scttfrdmn/objectfs/pkg/errors"
)

// Reasons a framed object is read whole anyway. Short fixed strings: they are counter labels to
// aggregate on, so nothing here interpolates a key, an offset or a size.
//
// The first four are ordinary and expected — most objects are not framed at all. The last two are
// worth alerting on: they mean framing engaged and then could not be used, which is either a
// permission problem on ranged GETs or a busy object being overwritten under readers.
const (
	seekableNoDescriptor    = "no_descriptor"
	seekableBadDescriptor   = "descriptor_unparsable"
	seekableNoFrameDecoder  = "no_frame_decoder"
	seekableCoversWholeBody = "range_covers_every_frame"
	seekableIndexUnusable   = "index_unusable"
	seekableFetchFailed     = "frame_fetch_failed"
	seekableObjectChanged   = "object_changed_mid_read"
)

// readFramed serves [offset, offset+size) of a framed object's content by fetching only the frames
// that cover it.
//
// ok == false means "this read must be served the old way": the caller re-fetches the object whole
// and decodes it, which is always correct. A non-nil error is reserved for the cases where falling
// back would paper over damaged stored bytes — see the integrity note at the frame decode below.
//
// metadata comes from a response the caller already has, which is what makes the descriptor free: S3
// returns user metadata on a 206 exactly as on a 200, so the speculative ranged read that discovered
// the object was encoded also carried the descriptor naming the index's length. Without it this path
// would need a HEAD before it could size its first request.
//
// contentEncoding is a hint and may be empty. A caller that has it — one holding a GetObject response —
// lets this decline a non-zstd object before spending a request; a caller that only has a HeadObject's
// metadata passes "" and the encoding is taken from the index fetch instead, which is the authority
// either way.
func (b *Backend) readFramed(
	ctx context.Context,
	key string,
	offset, size int64,
	metadata map[string]string,
	contentEncoding string,
) ([]byte, bool, error) {
	if b.compressor == nil {
		return nil, false, nil
	}

	// Not counted, and deliberately: an object with no descriptor is the overwhelming majority of
	// objects, including every uncompressed one and everything written by another tool. Counting it
	// would bury the fallbacks that mean something under a number that only tracks how much of the
	// bucket is unframed.
	text, ok := lookupMetaValue(metadata, metaSeekable)
	if !ok {
		return nil, false, nil
	}

	desc, err := compression.ParseSeekableDescriptor(text)
	if err != nil {
		// Reachable by anyone with s3:PutObject — `aws s3 cp --metadata` will carry a garbled
		// descriptor through faithfully — so this is a routine bad-input path, not corruption. The
		// object's own index is authoritative and the whole-object read still returns every byte.
		b.declineSeekable(key, seekableBadDescriptor, "descriptor", text, "error", err)

		return nil, false, nil
	}

	// Only when the caller supplied one. An empty hint is "unknown", not "unencoded" — declining on it
	// would send every caller that has metadata but no response headers down the whole-object path
	// forever, which is the kind of gate that makes a feature look absent rather than broken.
	if contentEncoding != "" && b.compressor.FrameDecoder(contentEncoding) == nil {
		// A descriptor on an object whose encoding this build cannot frame-decode. Either the object
		// lost its Content-Encoding to a CopyObject, or a descriptor was copied onto an object that is
		// not zstd.
		b.declineSeekable(key, seekableNoFrameDecoder, "content_encoding", contentEncoding)

		return nil, false, nil
	}

	idxRead, err := b.getObjectRange(ctx, key, 0, desc.IndexLength)
	if err != nil {
		b.declineSeekable(key, seekableFetchFailed, "index_length", desc.IndexLength, "error", err)

		return nil, false, nil
	}

	// The response is the authority on the object's encoding, so an unhinted call resolves it here and
	// a hinted one is re-checked against what the object actually says.
	contentEncoding = idxRead.contentEncoding
	if b.compressor.FrameDecoder(contentEncoding) == nil {
		b.declineSeekable(key, seekableNoFrameDecoder, "content_encoding", contentEncoding)

		return nil, false, nil
	}

	idx, _, err := compression.ParseFrameIndex(idxRead.data)
	if err != nil {
		// The index carries its own hash, so this is the one place a fallback is arguably hiding
		// something. It is still the right answer: the index is metadata *about* the content, and a
		// whole-object read does not consult it — it decodes the data frames as one zstd stream and
		// checks the result against objectfs-sha256. So the content's integrity is still verified by
		// the path taken instead, and refusing the read here would turn a recoverable index problem
		// into an unreadable file.
		b.declineSeekable(key, seekableIndexUnusable, "index_length", desc.IndexLength, "error", err)

		return nil, false, nil
	}

	// A read past the end of the content is an ordinary EOF, and the index answers it without another
	// request. Falling back here would fetch the whole stored body to return nothing.
	if offset >= idx.UncompressedSize {
		return []byte{}, true, nil
	}

	// A non-positive size means "to the end of the object" in this backend's range convention, which
	// FramesCovering does not implement — it takes a span. Resolved from the index rather than from a
	// HEAD, since the index already states the content length.
	want := size
	if want <= 0 {
		want = idx.UncompressedSize - offset
	}

	frames, offsetInFirst := idx.FramesCovering(offset, want)
	if len(frames) == 0 {
		b.declineSeekable(key, seekableIndexUnusable, "offset", offset, "size", want,
			"uncompressed_size", idx.UncompressedSize)

		return nil, false, nil
	}

	// When the span needs every frame, the framed path transfers the whole body *plus* the index in
	// two requests where the whole-object path needs one. Declining is not a tuning choice; it is the
	// cheaper request, and it is the common case for `cat` of a small file.
	if len(frames) == len(idx.Frames) {
		b.declineSeekable(key, seekableCoversWholeBody, "frames", len(frames))

		return nil, false, nil
	}

	// One Range for the whole run: CompressFramed lays frames down contiguously, so the frames
	// covering a contiguous span of content are a contiguous span of stored bytes. Fetching them
	// individually would be one request per frame for no benefit.
	last := frames[len(frames)-1]
	spanStart := frames[0].CompressedOffset
	spanLen := last.CompressedOffset + last.CompressedSize - spanStart

	bodyRead, err := b.getObjectRange(ctx, key, spanStart, spanLen)
	if err != nil {
		b.declineSeekable(key, seekableFetchFailed,
			"span_start", spanStart, "span_length", spanLen, "error", err)

		return nil, false, nil
	}

	// Two requests, so two chances to be looking at different objects. An overwrite between the index
	// fetch and the frame fetch is otherwise completely silent: both requests succeed, the lengths add
	// up, and the frames get decoded against offsets from an index that no longer describes them.
	//
	// ETag comparison rather than If-Match on the second request, for the reason CLAUDE.md gives about
	// establishing capabilities by probing: a store that accepts If-Match and ignores it is
	// indistinguishable from one that honors it, and a precondition silently dropped is worse than no
	// precondition, because it reports a guarantee it is not providing. A comparison of two values this
	// process holds cannot be silently dropped by anything. It is also the pattern the parallel read
	// path already uses across its chunks.
	//
	// A fallback rather than an error, which is where this differs from the parallel path: there,
	// recovering means redoing the whole fan-out, so it reports the race to the caller. Here one
	// whole-object GET produces a self-consistent view of whichever generation is current, so the read
	// can simply succeed.
	if idxRead.etag != "" && bodyRead.etag != "" && idxRead.etag != bodyRead.etag {
		b.declineSeekable(key, seekableObjectChanged,
			"index_etag", idxRead.etag, "frame_etag", bodyRead.etag)

		return nil, false, nil
	}

	// Integrity, and the one place this path returns an error rather than falling back.
	//
	// DecodeFrames verifies each frame's SHA-256 against the index before decoding it, and asserts the
	// decoded length the index recorded. A failure means the stored bytes do not match the index that
	// was fetched alongside them, and a retry decodes the same bytes to the same mismatch — so this is
	// corruption, reported non-retryable, exactly as the whole-object path reports a body its codec
	// rejects.
	//
	// What this path cannot check is the whole-content hash, because it deliberately never holds the
	// whole content: objectfs-sha256 and the index's own ContentSHA256 are both over all of it. The
	// per-frame hashes are what replaces it, and for a partial read they are stronger — they cover
	// exactly the bytes being returned, at frame granularity, whereas a whole-object hash can only say
	// that some byte somewhere differs. A whole-file read still gets the whole-content check, because
	// the clause above sends every read that covers all frames down the whole-object path.
	content, err := b.compressor.DecodeFrames(contentEncoding, frames, bodyRead.data)
	if err != nil {
		corrupt := errors.NewError(errors.ErrCodeDataCorruption,
			"a frame of a seekable object does not match the index stored with it").
			WithComponent("s3-backend").
			WithOperation("GetObject").
			WithContext("key", key).
			WithContext("content_encoding", contentEncoding).
			WithDetail("offset", offset).
			WithDetail("size", size).
			WithDetail("frames", len(frames)).
			WithDetail("span_start", spanStart).
			WithDetail("span_length", spanLen).
			WithDetail("cause", err.Error()).
			WithDetail("suggestion", "The stored frames disagree with the object's own frame index. "+
				"Either the object was overwritten in place by a writer that does not understand "+
				"framing, or the stored bytes are damaged. Read the object whole to see whether its "+
				"objectfs-sha256 still verifies.")

		b.metricsCollector.RecordError(corrupt)
		b.healthTracker.RecordError("s3-reads", corrupt)

		return nil, false, corrupt
	}

	// content starts at frames[0].UncompressedOffset, so the slice is frame-relative. Applying the
	// object-relative offset here is the defect FramesCovering's second return value exists to prevent,
	// and it would show up as every ranged read on a framed object returning shifted bytes.
	out := sliceRange(content, offsetInFirst, size)

	b.metricsCollector.RecordSeekableRead(desc.IndexLength + spanLen)

	b.logger.Debug("Served a ranged read from frames",
		"key", key,
		"offset", offset,
		"size", size,
		"frames", len(frames),
		"of_frames", len(idx.Frames),
		"stored_bytes", desc.IndexLength+spanLen,
		"returned_bytes", len(out))

	return out, true, nil
}

// declineSeekable counts and logs one whole-object fallback.
//
// Warn rather than Debug, and the reason is the whole point of the function: a fallback leaves the
// read correct, so at Debug it would be invisible in every deployment that has not turned Debug on —
// which is all of them — and the feature could stop working entirely without a single line to show
// for it. The two reasons that are merely "this object is not framed" never reach here.
func (b *Backend) declineSeekable(key, reason string, args ...any) {
	b.metricsCollector.RecordSeekableWholeFallback(reason)

	b.logger.Warn("Reading a framed object whole: "+strings.ReplaceAll(reason, "_", " "),
		append([]any{"key", key, "reason", reason}, args...)...)
}
