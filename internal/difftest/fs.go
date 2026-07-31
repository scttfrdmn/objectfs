package difftest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FS is the surface a differential test drives. Both the reference and the system under test
// implement it.
//
// It is deliberately narrow — one file, bytes in and bytes out — because everything wider dilutes what
// a disagreement means. Errors are returned rather than translated to errno: the oracle compares
// bytes and sizes, and treats an error on either side as a result to be reported, not matched.
type FS interface {
	// WriteAt writes data at offset. It is pwrite(2) semantics: writing past the end of the file
	// extends it, and the gap reads as zeros.
	WriteAt(ctx context.Context, offset int64, data []byte) error

	// ReadAt reads up to len(buf) bytes at offset and returns how many are valid. A read wholly past
	// the end of the file returns 0 with a nil error, as read(2) does — not an error.
	ReadAt(ctx context.Context, buf []byte, offset int64) (int, error)

	// Truncate resizes the file, zero-filling when it grows.
	Truncate(ctx context.Context, size int64) error

	// Flush makes every pending write durable, and must return an error if it cannot. This is the
	// operation v0.10.0 got most dangerously wrong: it recorded upload failures to a stats counter
	// and returned nil, so close(2) reported success after a failed PUT.
	Flush(ctx context.Context) error

	// Reopen closes and reopens the file, discarding any state held only in memory. Whatever survives
	// is what a second process would see.
	Reopen(ctx context.Context) error

	// Size returns the file's current length as the implementation would report it to stat(2),
	// including writes not yet durable.
	Size(ctx context.Context) (int64, error)

	// Durable returns the file's full contents as they exist in the backing store right now, without
	// consulting any in-memory state.
	//
	// This is the assertion that catches a flush which reports success without uploading. Reading
	// back through the same buffers that hold the pending writes would return the right bytes from
	// memory and confirm nothing.
	Durable(ctx context.Context) ([]byte, error)

	// Close releases resources. It does not flush; a test that wants durability asks for it.
	Close() error
}

// Local is the reference implementation: an actual file on the actual operating-system filesystem.
//
// It is the oracle's authority, and it is authoritative precisely because it is not a model. A model
// of POSIX write semantics written alongside an ObjectFS write path can share that path's
// misunderstanding of them, in which case both agree and the test reports success. The kernel does not
// share anyone's misunderstanding.
type Local struct {
	path string
	f    *os.File
}

// NewLocal creates a reference file under dir.
func NewLocal(dir string) (*Local, error) {
	path := filepath.Join(dir, "reference.bin")

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("difftest: create reference file: %w", err)
	}

	return &Local{path: path, f: f}, nil
}

// WriteAt implements [FS].
func (l *Local) WriteAt(_ context.Context, offset int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if _, err := l.f.WriteAt(data, offset); err != nil {
		return fmt.Errorf("difftest: reference write at %d: %w", offset, err)
	}
	return nil
}

// ReadAt implements [FS].
//
// io.EOF is not an error here. os.File.ReadAt reports a short read at the end of the file by
// returning io.EOF alongside the bytes it did read, and read(2) does not — a test that propagated it
// would report the reference as failing on every read that reaches the end of the file.
func (l *Local) ReadAt(_ context.Context, buf []byte, offset int64) (int, error) {
	n, err := l.f.ReadAt(buf, offset)
	if err != nil && n == 0 {
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		return 0, fmt.Errorf("difftest: reference read at %d: %w", offset, err)
	}
	return n, nil
}

// Truncate implements [FS].
func (l *Local) Truncate(_ context.Context, size int64) error {
	if err := l.f.Truncate(size); err != nil {
		return fmt.Errorf("difftest: reference truncate to %d: %w", size, err)
	}
	return nil
}

// Flush implements [FS].
func (l *Local) Flush(_ context.Context) error {
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("difftest: reference sync: %w", err)
	}
	return nil
}

// Reopen implements [FS].
func (l *Local) Reopen(_ context.Context) error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("difftest: reference close: %w", err)
	}

	f, err := os.OpenFile(l.path, os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("difftest: reference reopen: %w", err)
	}
	l.f = f
	return nil
}

// Size implements [FS].
func (l *Local) Size(_ context.Context) (int64, error) {
	st, err := l.f.Stat()
	if err != nil {
		return 0, fmt.Errorf("difftest: reference stat: %w", err)
	}
	return st.Size(), nil
}

// Durable implements [FS] by reading the file from the filesystem rather than through the open
// descriptor, so it reflects what another process would see.
func (l *Local) Durable(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return nil, fmt.Errorf("difftest: reference read back: %w", err)
	}
	return data, nil
}

// Close implements [FS].
func (l *Local) Close() error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("difftest: reference close: %w", err)
	}
	return nil
}
