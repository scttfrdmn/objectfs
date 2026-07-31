package difftest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/objectfs/objectfs/internal/buffer"
	"github.com/objectfs/objectfs/pkg/types"
)

// Legacy is the write path ObjectFS v0.10.0 shipped, wired to a real backend exactly as
// internal/adapter wires it.
//
// It exists so the oracle can be shown to catch the defects it was built for, on the real code that
// had them, before it is trusted to guard anything. A harness whose teeth are asserted rather than
// demonstrated is how 32,680 lines of tests came to miss forty-five defects.
//
// The one line that matters is in the flush callback, reproduced verbatim from
// internal/adapter/adapter.go:153-155:
//
//	flushCallback := func(key string, data []byte, offset int64) error {
//	    return a.backend.PutObject(context.Background(), key, data)
//	}
//
// PutObject replaces the whole object, so discarding the offset truncates the file to just the bytes
// written. Appending one byte to a 1 MiB file leaves a 1-byte object and reports success.
//
// Legacy is deleted along with the write path it models. Until then it is the regression fixture that
// keeps the oracle honest: [TestOracleCatchesLegacyDefects] asserts these divergences are *found*, so
// a change that blinds the harness fails there rather than silently passing everything.
type Legacy struct {
	backend types.Backend
	key     string
	wb      *buffer.WriteBuffer
}

// NewLegacy wires a WriteBuffer to backend under key, with the flush callback the adapter used.
func NewLegacy(backend types.Backend, key string) (*Legacy, error) {
	l := &Legacy{backend: backend, key: key}

	// Verbatim from internal/adapter/adapter.go: the offset parameter is accepted and dropped.
	flushCallback := func(key string, data []byte, _ int64) error {
		return backend.PutObject(context.Background(), key, data)
	}

	// AsyncFlush off so a flush is synchronous and the comparison is deterministic. This is the
	// generous reading of the legacy path: with async on, the oracle would also be racing the
	// background flush loop, and a divergence could be dismissed as a timing artifact rather than the
	// data loss it is.
	wb, err := buffer.NewWriteBuffer(&buffer.WriteBufferConfig{
		MaxBufferSize:  64 * 1024 * 1024,
		MaxBuffers:     16,
		FlushInterval:  time.Hour,
		FlushThreshold: 64 * 1024 * 1024,
		AsyncFlush:     false,
		BatchSize:      1 << 20,
		SyncOnClose:    false,
	}, flushCallback)
	if err != nil {
		return nil, fmt.Errorf("difftest: create legacy write buffer: %w", err)
	}
	l.wb = wb

	return l, nil
}

// WriteAt implements [FS].
func (l *Legacy) WriteAt(_ context.Context, offset int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if err := l.wb.Write(l.key, offset, data); err != nil {
		return fmt.Errorf("difftest: legacy write at %d: %w", offset, err)
	}
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

// Flush implements [FS].
func (l *Legacy) Flush(_ context.Context) error {
	if err := l.wb.FlushAll(); err != nil {
		return fmt.Errorf("difftest: legacy flush: %w", err)
	}
	return nil
}

// Reopen implements [FS]. The buffer is discarded without flushing, which is what losing in-memory
// state means.
func (l *Legacy) Reopen(_ context.Context) error {
	if err := l.wb.Close(); err != nil {
		return fmt.Errorf("difftest: legacy close on reopen: %w", err)
	}

	next, err := NewLegacy(l.backend, l.key)
	if err != nil {
		return err
	}
	l.wb = next.wb
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

// Close implements [FS].
func (l *Legacy) Close() error {
	if err := l.wb.Close(); err != nil {
		return fmt.Errorf("difftest: legacy close: %w", err)
	}
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
