package vfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	objerrors "github.com/objectfs/objectfs/pkg/errors"
	"github.com/objectfs/objectfs/pkg/types"
)

// DefaultFlushAttempts bounds how many times [Flusher.Flush] rebuilds an upload that lost a race
// with a concurrent writer.
//
// Bounded rather than unbounded. A generation that keeps moving means writers are arriving faster
// than an upload completes, and spinning forever inside close(2) is worse than reporting that the
// flush did not converge: the caller can retry, but it cannot recover from a syscall that never
// returns.
const DefaultFlushAttempts = 8

// Flusher makes a [Node] durable in an object store, doing genuine read-modify-write.
//
// # The defect this type exists to remove
//
// S3's PutObject replaces an entire object; the POSIX contract is "modify a byte range". v0.10.0 had
// nothing that reconciled the two. Its flush callback was handed the offset and dropped it:
//
//	flushCallback := func(key string, data []byte, offset int64) error {
//	    return a.backend.PutObject(context.Background(), key, data)   // offset discarded
//	}
//
// So every write at a non-zero offset replaced the whole object with just the bytes written.
// Appending one byte to a 1 MiB file left a 1-byte object, and reported success. That single
// discarded parameter is the root of six separate audit findings.
//
// # The protocol, in the order that matters
//
//  1. capture [Node.Generation]
//  2. ask for a [FlushPlan]
//  3. fetch the plan's ReadRanges from the object store
//  4. [Node.Splice] them with the pending writes
//  5. upload
//  6. [Node.MarkFlushed] with the generation from step 1
//
// Step 6 is why step 1 exists, and the order is not a stylistic preference. A write landing between
// steps 2 and 5 is not in the body that was uploaded, so clearing the pending state would discard it
// — which is exactly the v0.10.0 path where a write concurrent with a flush was dropped and
// accounted as flushed. MarkFlushed refuses when the generation has moved, and this type loops
// rather than reporting a success it did not achieve.
//
// # Known gap: attributes
//
// Flush persists content, not attributes. A node with only [Node.SetAttr] pending — a chmod with no
// write — produces a Noop plan and this returns nil without storing the mode. That is not yet a live
// data-loss path because no kernel binding calls SetAttr: internal/fuse implements no Setattr, which
// is its own audit finding. Attribute write-back lands with that work. This comment exists so the
// gap is stated rather than discovered.
type Flusher struct {
	backend types.Backend

	// attempts bounds the race-retry loop. Zero means [DefaultFlushAttempts].
	attempts int
}

// NewFlusher returns a Flusher writing through backend.
func NewFlusher(backend types.Backend) (*Flusher, error) {
	if backend == nil {
		return nil, fmt.Errorf("%w: nil backend", ErrInvalid)
	}
	return &Flusher{backend: backend}, nil
}

// Flush makes n durable, following the protocol documented on [Flusher].
//
// It returns nil only after a [Node.MarkFlushed] that took. Every other path returns an error, which
// is the property that lets close(2) and fsync(2) mean something: v0.10.0's FlushWithContext
// scheduled a background flush and returned nil, so a PUT rejected for AccessDenied incremented an
// error counter nobody read while close(2) reported success.
func (f *Flusher) Flush(ctx context.Context, n *Node) error {
	if n == nil {
		return fmt.Errorf("%w: nil node", ErrInvalid)
	}

	attempts := f.attempts
	if attempts <= 0 {
		attempts = DefaultFlushAttempts
	}

	for range attempts {
		done, err := f.attempt(ctx, n)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		// The node changed during the upload. The bytes that were sent are stale; go round again.
	}

	return fmt.Errorf("flush %q: did not converge in %d attempts: writers are arriving faster than "+
		"an upload completes", n.Path, attempts)
}

// attempt runs the protocol once, reporting whether the node was marked flushed.
//
// A false return with a nil error means only that the generation moved — the upload succeeded but
// described state that is no longer current. That is not a failure, and it must not be reported as
// one; it is the detection that v0.10.0 lacked.
func (f *Flusher) attempt(ctx context.Context, n *Node) (bool, error) {
	// Before the plan, not after: a write landing between these two calls must invalidate the
	// upload, and it can only do that if the generation was read first.
	generation := n.Generation()

	plan, _, err := n.FlushPlan()
	if err != nil {
		return false, fmt.Errorf("flush %q: %w", n.Path, err)
	}
	if plan.Noop {
		return true, nil
	}

	base, err := f.readBase(ctx, n.Path, plan)
	if err != nil {
		return false, err
	}

	body, err := n.Splice(plan.Size, base)
	if err != nil {
		return false, fmt.Errorf("flush %q: %w", n.Path, err)
	}
	if int64(len(body)) != plan.Size {
		// Unreachable unless Splice is broken. Checked anyway: the alternative to failing here is
		// uploading a body of the wrong length, which is the corruption this whole type exists to
		// prevent, and it would be indistinguishable afterwards from a backend fault.
		return false, fmt.Errorf("flush %q: spliced %d bytes but the plan said %d",
			n.Path, len(body), plan.Size)
	}

	if putErr := f.backend.PutObject(ctx, n.Path, body); putErr != nil {
		return false, fmt.Errorf("flush %q (%d bytes): %w", n.Path, len(body), putErr)
	}

	// Read the stored state back rather than trusting the length that was sent. A backend that
	// stores fewer bytes than it was handed is silent corruption, and it is the one failure a write
	// path cannot detect by looking at itself — v0.10.0's compressed-upload path stored objects that
	// HeadObject then described with a different size entirely.
	info, err := f.backend.HeadObject(ctx, n.Path)
	if err != nil {
		return false, fmt.Errorf("flush %q: confirm upload: %w", n.Path, err)
	}
	if info.Size != plan.Size {
		return false, fmt.Errorf("%w: flush %q uploaded %d bytes but the stored object is %d",
			ErrIntegrity, n.Path, plan.Size, info.Size)
	}

	return n.MarkFlushed(generation, info.Size, info.ETag), nil
}

// readBase fetches the ranges of the current object the plan says must be spliced with the pending
// writes.
//
// A plan with no ReadRanges — [FlushPlan.WholeObject] — reads nothing, which is the ordinary
// sequential-write and overwrite case. Ranges are fetched serially: a plan has one range per hole in
// the pending writes, which for real access patterns is one or two, and a bounded serial fetch is
// easier to reason about than a partially-failed parallel one. A file with enough holes for this to
// matter is the case for upload-part-copy, tracked separately.
func (f *Flusher) readBase(ctx context.Context, key string, plan FlushPlan) ([]Extent, error) {
	if len(plan.ReadRanges) == 0 {
		return nil, nil
	}

	base := make([]Extent, 0, len(plan.ReadRanges))
	for _, r := range plan.ReadRanges {
		data, err := f.backend.GetObject(ctx, key, r.Offset, r.Length)
		if err != nil {
			if IsNotFound(err) {
				// The object vanished between the plan and now. Its bytes are gone, so the range is a
				// hole, which Splice fills with zeros. Not an error: a concurrent delete is a valid
				// thing for another client to do, and failing the flush would strand the write.
				continue
			}
			return nil, fmt.Errorf("flush %q: read [%d,%d) for read-modify-write: %w",
				key, r.Offset, r.End(), err)
		}
		if len(data) == 0 {
			continue
		}
		base = append(base, Extent{Offset: r.Offset, Data: data})
	}
	return base, nil
}

// StoredSize reports the length of the object at key as it exists in storage, treating absence as
// zero.
//
// Absence is zero rather than an error because a file open for writing need not exist yet: open(2)
// with O_CREAT produces an empty file, and S3 has no way to represent a zero-byte object that has
// never been written. Treating absence as a failure would break every program that creates a file
// before writing it.
func (f *Flusher) StoredSize(ctx context.Context, key string) (int64, error) {
	info, err := f.backend.HeadObject(ctx, key)
	if err != nil {
		if IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("%w: stat %q: %w", ErrBackend, key, err)
	}
	if info.Size < 0 {
		return 0, fmt.Errorf("%w: stat %q reported a negative size %d", ErrIntegrity, key, info.Size)
	}
	return info.Size, nil
}

// errNotFoundSentinel is compared by [objerrors.ObjectFSError.Is], which matches on Code alone.
var errNotFoundSentinel = objerrors.NewError(objerrors.ErrCodeObjectNotFound, "")

// IsNotFound reports whether err means the object does not exist, as distinct from any other
// backend failure.
//
// This distinction is load-bearing and getting it wrong destroys data. v0.10.0's Lookup collapsed
// every HeadObject error to ENOENT, so a throttle or a permission failure read as "file absent" —
// and Create then wrote an empty object over a file that was merely temporarily unreachable. A
// classifier that is too generous here is worse than one that is too strict: reporting a live object
// as absent invites an overwrite, while reporting an absent object as an error merely fails.
//
// The typed check is authoritative. The substring fallback exists because the S3 backend wraps SDK
// errors in its own type and does not preserve an inspectable code on every path; it is deliberately
// limited to strings that cannot mean anything else.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}

	// fs.ErrNotExist is the standard library's canonical absence sentinel, and a types.Backend over a
	// local filesystem returns exactly it. Typed, so it cannot misclassify in the dangerous direction.
	if errors.Is(err, errNotFoundSentinel) || errors.Is(err, fs.ErrNotExist) {
		return true
	}

	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		switch coded.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}

	msg := strings.ToLower(err.Error())
	for _, want := range []string{
		"nosuchkey",
		"object not found",
		"status code: 404",
		"statuscode: 404",
	} {
		if strings.Contains(msg, want) {
			return true
		}
	}
	return false
}
