package difftest

import (
	"context"
	"fmt"

	"github.com/objectfs/objectfs/internal/vfs"
	"github.com/objectfs/objectfs/pkg/types"
)

// VFS is a [FS] built on [vfs.Node] over a real object-storage backend, doing genuine
// read-modify-write.
//
// It is the counterpart to [Legacy]: the same interface, the same backend, the same operation
// sequences, and a write path that represents an offset write instead of discarding it. Having both
// under one oracle is what makes the comparison mean something — [TestOracleCatchesLegacyDefects]
// shows the harness finds the v0.10.0 defects, and the fuzz target over this shows the replacement
// does not have them.
//
// The flush protocol it drives is [vfs.Flusher]'s, called rather than reimplemented. That is the
// point: this type once carried its own copy of the six-step sequence, which meant the oracle and the
// fuzzer proved the protocol was sound without proving the shipping code implemented it. A second
// implementation in the harness is the same class of mistake as the second FUSE binding that drifted
// until `rm` reported deletions that never happened.
//
// The kernel is not modeled here: no page cache, no readahead, no writeback ordering. This drives
// vfs directly, because a divergence has to be attributable to vfs or to the backend rather than to
// three layers at once.
type VFS struct {
	backend types.Backend
	key     string
	flusher *vfs.Flusher

	table  *vfs.HandleTable
	handle *vfs.Handle
}

// NewVFS opens key on backend through a fresh handle table.
//
// The object need not exist; an absent object is a zero-length file, which is what the local
// reference is after open(2) creates it.
func NewVFS(ctx context.Context, backend types.Backend, key string) (*VFS, error) {
	f, err := vfs.NewFlusher(backend)
	if err != nil {
		return nil, fmt.Errorf("difftest: %w", err)
	}

	v := &VFS{backend: backend, key: key, flusher: f}
	if err := v.open(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

// open establishes a handle against the object's current stored state.
func (v *VFS) open(ctx context.Context) error {
	storedSize, err := v.storedSize(ctx)
	if err != nil {
		return err
	}

	v.table = vfs.NewHandleTable()

	attr := vfs.Attr{Type: vfs.FileTypeRegular, Size: storedSize, Mode: 0o644}
	h, err := v.table.Open(v.key, vfs.OpenRead|vfs.OpenWrite, attr, storedSize)
	if err != nil {
		return fmt.Errorf("difftest: vfs open %q: %w", v.key, err)
	}
	v.handle = h
	return nil
}

// storedSize reports the length of the object as it exists in storage, treating absence as zero.
func (v *VFS) storedSize(ctx context.Context) (int64, error) {
	size, err := v.flusher.StoredSize(ctx, v.key)
	if err != nil {
		return 0, fmt.Errorf("difftest: vfs head %q: %w", v.key, err)
	}
	return size, nil
}

// WriteAt implements [FS].
func (v *VFS) WriteAt(_ context.Context, offset int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if _, err := v.handle.Node.Write(offset, data, false); err != nil {
		return fmt.Errorf("difftest: vfs write at %d: %w", offset, err)
	}
	return nil
}

// ReadAt implements [FS] by asking the node which stored bytes it needs, fetching exactly those, and
// letting the node overlay its pending writes.
//
// The fetch is a ranged GET of precisely the range asked for. That is the property v0.10.0 lost: with
// compression merely enabled it fetched whole objects for every read of every object, amplifying a
// 4 KiB read of a 256 MiB object by 216×.
func (v *VFS) ReadAt(ctx context.Context, buf []byte, offset int64) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}

	r, need, err := v.handle.Node.ReadRange(offset, len(buf))
	if err != nil {
		return 0, fmt.Errorf("difftest: vfs read range at %d: %w", offset, err)
	}

	var stored []byte
	if need {
		stored, err = v.fetch(ctx, r)
		if err != nil {
			return 0, err
		}
	}

	n, err := v.handle.Node.ReadInto(buf, offset, stored)
	if err != nil {
		return 0, fmt.Errorf("difftest: vfs read at %d: %w", offset, err)
	}
	return n, nil
}

// fetch reads one byte range of the stored object, treating absence as no bytes.
func (v *VFS) fetch(ctx context.Context, r vfs.Range) ([]byte, error) {
	data, err := v.backend.GetObject(ctx, v.key, r.Offset, r.Length)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("difftest: vfs fetch %q [%d,%d): %w", v.key, r.Offset, r.End(), err)
	}
	return data, nil
}

// Truncate implements [FS].
func (v *VFS) Truncate(_ context.Context, size int64) error {
	if err := v.handle.Node.Truncate(size); err != nil {
		return fmt.Errorf("difftest: vfs truncate to %d: %w", size, err)
	}
	return nil
}

// Flush implements [FS] by calling [vfs.Flusher], which is the production flush path.
//
// Delegating rather than reimplementing is what makes the oracle's verdict mean something about
// shipping code. When this function held its own copy of the six-step protocol, a defect in
// vfs.Flusher would have left every differential test and every fuzz iteration green.
func (v *VFS) Flush(ctx context.Context) error {
	if err := v.flusher.Flush(ctx, v.handle.Node); err != nil {
		return fmt.Errorf("difftest: vfs flush %q: %w", v.key, err)
	}
	return nil
}

// Reopen implements [FS] by discarding all in-memory state and re-establishing it from storage.
//
// Nothing is flushed first. Whatever a reopen loses is what a second process would never have seen,
// which is the only way to distinguish a flush that uploaded from one that merely said it did.
func (v *VFS) Reopen(ctx context.Context) error {
	return v.open(ctx)
}

// Size implements [FS], reporting the file's logical length including pending writes.
//
// Including pending writes is not a nicety. v0.10.0's Getattr read the object's metadata and never
// consulted the write buffer, so a file being written reported its pre-write size — and the kernel
// truncated reads of it at that size.
func (v *VFS) Size(_ context.Context) (int64, error) {
	return v.handle.Node.Size(), nil
}

// Durable implements [FS] by reading the whole object from storage, without consulting the node.
func (v *VFS) Durable(ctx context.Context) ([]byte, error) {
	data, err := v.backend.GetObject(ctx, v.key, 0, -1)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("difftest: vfs read back %q: %w", v.key, err)
	}
	return data, nil
}

// Close implements [FS]. It does not flush: [FS] says durability is asked for, not assumed.
func (v *VFS) Close() error {
	if v.handle == nil {
		return nil
	}
	if _, _, err := v.table.Release(v.handle.ID); err != nil {
		return fmt.Errorf("difftest: vfs release %q: %w", v.key, err)
	}
	v.handle = nil
	return nil
}
