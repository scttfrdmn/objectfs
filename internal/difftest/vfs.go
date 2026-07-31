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
// It also serves as the executable specification of the flush protocol the adapter must follow, in
// the order that matters:
//
//  1. capture [vfs.Node.Generation]
//  2. ask for a [vfs.FlushPlan]
//  3. fetch the plan's ReadRanges from the object store
//  4. [vfs.Node.Splice] them with the pending writes
//  5. upload
//  6. [vfs.Node.MarkFlushed] with the generation from step 1
//
// Step 6 is why step 1 exists. A write that lands between steps 2 and 5 is not in the body that was
// uploaded, so clearing the pending state would discard it — which is exactly the v0.10.0 path where
// a write concurrent with a flush was dropped and accounted as flushed. MarkFlushed refuses when the
// generation has moved, and this type loops rather than reporting a success it did not achieve.
//
// The kernel is not modeled here: no page cache, no readahead, no writeback ordering. This drives
// vfs directly, because a divergence has to be attributable to vfs or to the backend rather than to
// three layers at once.
type VFS struct {
	backend types.Backend
	key     string

	table  *vfs.HandleTable
	handle *vfs.Handle
}

// NewVFS opens key on backend through a fresh handle table.
//
// The object need not exist; an absent object is a zero-length file, which is what the local
// reference is after open(2) creates it.
func NewVFS(ctx context.Context, backend types.Backend, key string) (*VFS, error) {
	v := &VFS{backend: backend, key: key}
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
	info, err := v.backend.HeadObject(ctx, v.key)
	if err != nil {
		if isNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("difftest: vfs head %q: %w", v.key, err)
	}
	if info.Size < 0 {
		return 0, fmt.Errorf("difftest: vfs head %q reported a negative size %d", v.key, info.Size)
	}
	return info.Size, nil
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

// Flush implements [FS], following the protocol documented on [VFS].
//
// It loops while MarkFlushed reports a lost race. A bounded loop rather than an unbounded one: a
// generation that keeps moving means a writer this harness does not have, so exhausting the attempts
// is a harness bug worth reporting rather than something to spin on. Nothing here reports success
// without a MarkFlushed that took.
func (v *VFS) Flush(ctx context.Context) error {
	const attempts = 8

	for range attempts {
		node := v.handle.Node

		// Before the plan, not after: a write landing between these two calls must invalidate the
		// upload, and it can only do that if the generation was read first.
		gen := node.Generation()

		plan, _, err := node.FlushPlan()
		if err != nil {
			return fmt.Errorf("difftest: vfs flush plan %q: %w", v.key, err)
		}
		if plan.Noop {
			return nil
		}

		var base []vfs.Extent
		for _, r := range plan.ReadRanges {
			data, err := v.fetch(ctx, r)
			if err != nil {
				return err
			}
			if len(data) == 0 {
				continue
			}
			base = append(base, vfs.Extent{Offset: r.Offset, Data: data})
		}

		body, err := node.Splice(plan.Size, base)
		if err != nil {
			return fmt.Errorf("difftest: vfs splice %q: %w", v.key, err)
		}
		if int64(len(body)) != plan.Size {
			return fmt.Errorf("difftest: vfs splice %q produced %d bytes, plan said %d",
				v.key, len(body), plan.Size)
		}

		if err := v.backend.PutObject(ctx, v.key, body); err != nil {
			return fmt.Errorf("difftest: vfs put %q (%d bytes): %w", v.key, len(body), err)
		}

		// Read the stored size back rather than trusting the length that was sent. A backend that
		// stores fewer bytes than it was handed is silent corruption, and it is the one failure a
		// write path cannot detect by looking at itself — v0.10.0's compressed-upload path stored
		// objects that HeadObject then described with a different size entirely.
		stored, err := v.storedSize(ctx)
		if err != nil {
			return err
		}
		if stored != plan.Size {
			return fmt.Errorf("difftest: vfs flush %q uploaded %d bytes but the object is %d",
				v.key, plan.Size, stored)
		}

		if node.MarkFlushed(gen, stored, "") {
			return nil
		}
		// The node changed during the upload. The bytes that were sent are stale; go round again.
	}

	return fmt.Errorf("difftest: vfs flush %q did not converge in %d attempts: the node is being "+
		"mutated concurrently, which this harness has no writer to do", v.key, attempts)
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
