package difftest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/objectfs/objectfs/pkg/types"
)

// Legacy is the write path ObjectFS v0.10.0 shipped, wired to a real backend exactly as
// internal/adapter wired it.
//
// It exists so the oracle can be shown to catch the defects it was built for, on the behavior that
// had them, before it is trusted to guard anything. A harness whose teeth are asserted rather than
// demonstrated is how 32,680 lines of tests came to miss forty-five defects.
//
// # Why this is a copy and not a call
//
// This type once wired up the real internal/buffer.WriteBuffer. That package is now deleted, and
// reaching for it is no longer possible — but even while it existed, depending on it made the fixture
// track the code it was supposed to be frozen against. A regression fixture that follows its subject
// is not a fixture. The three behaviors reproduced below are therefore transcribed, with the
// original line numbers, and will not change again:
//
//   - the flush callback took the offset and discarded it (adapter.go:153-155), so PutObject replaced
//     the whole object and an offset write truncated the file to just the bytes written;
//   - canBufferWrite (writebuffer.go:271-290) refused any write that did not continue the single
//     contiguous run, returning "buffer full or write cannot be buffered" — EIO to the caller;
//   - reads went to the backend without consulting the buffer (H5), and Getattr took the size from
//     the object's metadata for the same reason.
//
// [TestOracleCatchesLegacyDefects] asserts these divergences are *found*, so a change that blinds the
// harness fails there rather than silently passing everything.
type Legacy struct {
	backend types.Backend
	key     string

	// buf and bufOffset are v0.10.0's entire per-file write state: one contiguous run of bytes and
	// the offset it starts at. That is the representation from which every write-path defect follows.
	// It cannot express two disjoint dirty ranges, so a filesystem built on it must either refuse the
	// second write or lose the first.
	buf       []byte
	bufOffset int64
}

// legacyMaxBufferSize is the MaxBufferSize the adapter configured, in bytes. A write that would take
// the run past it was refused rather than flushed.
const legacyMaxBufferSize = 64 * 1024 * 1024

// NewLegacy wires the v0.10.0 write path to backend under key.
//
// The original ran with AsyncFlush disabled here so that a flush is synchronous and the comparison is
// deterministic. This is the generous reading of the legacy path: with async on, the oracle would also
// be racing the background flush loop, and a divergence could be dismissed as a timing artifact rather
// than the data loss it is.
func NewLegacy(backend types.Backend, key string) (*Legacy, error) {
	if backend == nil {
		return nil, fmt.Errorf("difftest: legacy needs a backend")
	}
	return &Legacy{backend: backend, key: key}, nil
}

// WriteAt implements [FS] with v0.10.0's canBufferWrite, transcribed from
// internal/buffer/writebuffer.go:271-290:
//
//	if len(buf.data) > 0 {
//	    expectedOffset := buf.offset + int64(len(buf.data))
//	    if req.Offset != expectedOffset {
//	        return false // Non-contiguous write
//	    }
//	}
//
// A refusal became `fmt.Errorf("buffer full or write cannot be buffered")`, which the FUSE layer
// turned into EIO. That is defect H8, and the access pattern it rejects is the one SQLite, mmap
// writeback, tar, and HDF5 all use.
func (l *Legacy) WriteAt(_ context.Context, offset int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}

	if int64(len(l.buf))+int64(len(data)) > legacyMaxBufferSize {
		return fmt.Errorf("difftest: legacy write at %d: buffer full or write cannot be buffered", offset)
	}
	if len(l.buf) > 0 && offset != l.bufOffset+int64(len(l.buf)) {
		return fmt.Errorf("difftest: legacy write at %d: buffer full or write cannot be buffered", offset)
	}

	if len(l.buf) == 0 {
		l.bufOffset = offset
	}
	l.buf = append(l.buf, data...)
	return nil
}

// ReadAt implements [FS] the way v0.10.0's read path did: straight to the backend, without consulting
// the write buffer. That is the H5 defect — a read after a write on the same descriptor returns
// pre-write bytes — and reproducing it is the point.
func (l *Legacy) ReadAt(ctx context.Context, buf []byte, offset int64) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}

	data, err := l.backend.GetObject(ctx, l.key, offset, int64(len(buf)))
	if err != nil {
		if isNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("difftest: legacy read at %d: %w", offset, err)
	}
	return copy(buf, data), nil
}

// Truncate implements [FS]. v0.10.0 had no truncate at all — no Setattr, no Truncate — so the honest
// model of it is a refusal. The oracle records a refusal as a divergence from the local filesystem,
// which is correct: a filesystem that cannot truncate is broken, just visibly rather than silently.
func (l *Legacy) Truncate(_ context.Context, size int64) error {
	return fmt.Errorf("difftest: legacy path cannot truncate to %d: v0.10.0 implemented no Setattr or Truncate", size)
}

// Flush implements [FS] with the adapter's flush callback, transcribed verbatim from
// internal/adapter/adapter.go:153-155:
//
//	flushCallback := func(key string, data []byte, offset int64) error {
//	    return a.backend.PutObject(context.Background(), key, data)
//	}
//
// The offset parameter is accepted and dropped, and PutObject replaces the whole object. So appending
// one byte to a 1 MiB file leaves a 1-byte object — and reports success, because the callback returned
// nil and flushBuffer then deleted the buffer as flushed.
func (l *Legacy) Flush(ctx context.Context) error {
	if len(l.buf) == 0 {
		return nil
	}

	// The offset is in scope and unused, exactly as it was.
	if err := l.backend.PutObject(ctx, l.key, l.buf); err != nil {
		return fmt.Errorf("difftest: legacy flush: %w", err)
	}

	l.buf = nil
	l.bufOffset = 0
	return nil
}

// Reopen implements [FS]. The buffer is discarded without flushing, which is what losing in-memory
// state means.
func (l *Legacy) Reopen(_ context.Context) error {
	l.buf = nil
	l.bufOffset = 0
	return nil
}

// Size implements [FS] as v0.10.0's Getattr did: from the object's metadata, ignoring pending writes.
func (l *Legacy) Size(ctx context.Context) (int64, error) {
	info, err := l.backend.HeadObject(ctx, l.key)
	if err != nil {
		if isNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("difftest: legacy stat: %w", err)
	}
	return info.Size, nil
}

// Durable implements [FS].
func (l *Legacy) Durable(ctx context.Context) ([]byte, error) {
	data, err := l.backend.GetObject(ctx, l.key, 0, -1)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("difftest: legacy read back: %w", err)
	}
	return data, nil
}

// Close implements [FS]. SyncOnClose was off, so the buffer is dropped rather than flushed.
func (l *Legacy) Close() error {
	l.buf = nil
	return nil
}

// isNotFound reports whether err means the object does not exist.
//
// An absent object is an empty file for oracle purposes: the local reference is created empty by
// open(2), and S3 has no way to represent a zero-byte object that has never been written. Treating
// absence as an error would make every program that reads before writing diverge for a reason that
// says nothing about the write path.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}

	var notFound interface{ ErrorCode() string }
	if errors.As(err, &notFound) {
		switch notFound.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}

	// Fall back to the message. The S3 backend wraps SDK errors in its own types and does not
	// consistently preserve an inspectable code, so the alternative is treating a missing object as a
	// hard failure and losing every read-before-write program.
	msg := err.Error()
	for _, want := range []string{"NoSuchKey", "NotFound", "not found", "status code: 404", "StatusCode: 404"} {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(want)) {
			return true
		}
	}
	return false
}
