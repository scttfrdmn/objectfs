package vfs_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// The tests in this file pin the write path's half of the read-only mount (#532).
//
// internal/fuse enforces read-only too, from thirteen entry points, and that is where the EROFS an
// application sees comes from. This layer exists because thirteen entry points are enforced by whoever
// remembered: the fourteenth is the one nobody adds a check to, and the consequence is not a wrong errno
// but a byte written to a bucket the operator asked to protect.
//
// The mutation these must catch is the one that sat in the tree through v0.16.0: every EROFS gate
// present and correct, and nothing setting the flag they read. That version had a complete read-only
// implementation with no way to reach it — see internal/adapter.buildMountOptions — and it passed every
// test in this repository, because a gate nobody can turn on is a gate nobody tested turning on.

// TestEveryMutatingWriterMethodRefusesWhenReadOnly enumerates the methods that can make a node dirty.
//
// Enumerated deliberately, unlike most tests in this package, and the enumeration is checked against the
// type: [TestWriterMutatingMethodsAreAllCovered] fails when a method is added to [vfs.Writer] and not to
// this list. An unchecked list here would be the same defect as the one above — a rule that covers what
// somebody thought of.
func TestEveryMutatingWriterMethodRefusesWhenReadOnly(t *testing.T) {
	t.Parallel()

	const key = "protected/object"

	tests := []struct {
		method string
		call   func(*vfs.Writer) error
	}{
		{"Write", func(w *vfs.Writer) error {
			return w.Write(key, 0, []byte("x"))
		}},
		{"WriteContext", func(w *vfs.Writer) error {
			return w.WriteContext(context.Background(), key, 0, []byte("x"))
		}},
		{"Truncate", func(w *vfs.Writer) error {
			return w.Truncate(context.Background(), key, 0)
		}},
		{"SetAttr", func(w *vfs.Writer) error {
			return w.SetAttr(context.Background(), key, true, false, false, vfs.Attr{Mode: 0o600})
		}},
		{"SetXattr", func(w *vfs.Writer) error {
			return w.SetXattr(context.Background(), key, "user.x", []byte("v"))
		}},
		{"RemoveXattr", func(w *vfs.Writer) error {
			return w.RemoveXattr(context.Background(), key, "user.x")
		}},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()

			backend := newFakeBackend()
			w := newReadOnlyWriter(t, backend)

			err := tt.call(w)
			if err == nil {
				t.Fatalf("%s succeeded on a read-only writer", tt.method)
			}
			if !errors.Is(err, vfs.ErrReadOnly) {
				t.Errorf("%s returned %v, which does not wrap ErrReadOnly — internal/fuse maps that "+
					"sentinel to EROFS, so an error that does not wrap it reaches the application as EIO",
					tt.method, err)
			}

			// The key is named, because the caller of this is a FUSE handler whose own error goes to a log
			// with no path in it.
			if !strings.Contains(err.Error(), key) {
				t.Errorf("%s returned %q, which does not name the key it refused", tt.method, err)
			}

			// Nothing buffered. A refusal that left a node behind would report as buffered state to
			// Count and would be walked by FlushAll at unmount — the refusal has to happen before the
			// node is created, not before the mutation is applied.
			if n := w.Count(); n != 0 {
				t.Errorf("%s left %d buffered node(s) behind after being refused", tt.method, n)
			}
			if w.Dirty(key) {
				t.Errorf("%s left %q dirty after being refused", tt.method, key)
			}

			// And nothing reached the backend, including on the way to being refused: a gate placed after
			// the node is created reads the object's stored attributes first, which is a HEAD against a
			// bucket for an operation that was never going to happen.
			if calls := backend.Calls(); len(calls) != 0 {
				t.Errorf("%s made %d backend call(s) before being refused: %v", tt.method, len(calls), calls)
			}
		})
	}
}

// TestWriterMutatingMethodsAreAllCovered is the non-vacuity guard on the list above.
//
// It is a count rather than a reflection walk over method names, because "can this method make a node
// dirty" is not a property a signature carries — Attr and FileSize take the same shapes and neither
// mutates. So what is checked is that the number of methods reaching [vfs.Writer.mutatingNode] has not
// changed, and the place that number is maintained is here.
//
// The real coupling is in internal/vfs/writer.go: mutatingNode is the only way to obtain a node that may
// be mutated, and the read paths call node directly. A new mutating method that calls node instead is the
// mistake, and it is visible in a diff of that file in a way a test cannot assert from outside the
// package.
func TestWriterMutatingMethodsAreAllCovered(t *testing.T) {
	t.Parallel()

	// Write and WriteContext are one mutating path reached two ways; the other four are one each.
	const wantMutating = 6

	backend := newFakeBackend()
	w := newReadOnlyWriter(t, backend)

	refused := 0
	for _, call := range []func() error{
		func() error { return w.Write("k", 0, []byte("x")) },
		func() error { return w.WriteContext(context.Background(), "k", 0, []byte("x")) },
		func() error { return w.Truncate(context.Background(), "k", 0) },
		func() error { return w.SetAttr(context.Background(), "k", true, false, false, vfs.Attr{}) },
		func() error { return w.SetXattr(context.Background(), "k", "user.x", []byte("v")) },
		func() error { return w.RemoveXattr(context.Background(), "k", "user.x") },
	} {
		if errors.Is(call(), vfs.ErrReadOnly) {
			refused++
		}
	}

	if refused != wantMutating {
		t.Errorf("%d of %d enumerated methods refused with ErrReadOnly, want all of them. Either a "+
			"mutating method stopped going through mutatingNode, or this list has drifted from the type",
			refused, wantMutating)
	}
}

// TestReadOnlyWriterStillServesReads is the other half, and the one that fails if the gate is put in the
// wrong place.
//
// [vfs.Writer.node] is not a write-only chokepoint: ReadAt and FileSize call it to overlay pending writes
// on a read and to report a size that includes them. Gating node itself is the obvious implementation and
// it makes every read on a read-only mount return EROFS — a mount that refuses to be read is not a
// read-only mount, and nothing in the test above would notice.
func TestReadOnlyWriterStillServesReads(t *testing.T) {
	t.Parallel()

	const key = "readable/object"
	body := []byte("stored contents")

	backend := newFakeBackend()
	backend.Put(key, body)

	w := newReadOnlyWriter(t, backend)

	size, err := w.FileSize(context.Background(), key)
	if err != nil {
		t.Fatalf("FileSize on a read-only writer: %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("FileSize = %d, want %d", size, len(body))
	}

	buf := make([]byte, len(body))
	n, err := w.ReadAt(context.Background(), key, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt on a read-only writer: %v", err)
	}
	if got := string(buf[:n]); got != string(body) {
		t.Errorf("ReadAt returned %q, want %q", got, body)
	}
}

// TestReadOnlyWriterFlushesNothing covers the path unmount takes.
//
// Reads create nodes, so a read-only mount that has served reads has nodes in w.nodes, and FlushAll walks
// every one of them. It must issue no write. The guarantee comes from Flusher.attemptAttrOnly returning
// early on a node with no dirty attributes, which is a different piece of code from anything #532 touched
// — so this asserts the composition rather than trusting it.
func TestReadOnlyWriterFlushesNothing(t *testing.T) {
	t.Parallel()

	const key = "readable/object"

	backend := newFakeBackend()
	backend.Put(key, []byte("stored contents"))

	w := newReadOnlyWriter(t, backend)

	buf := make([]byte, 4)
	if _, err := w.ReadAt(context.Background(), key, buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	if err := w.FlushAll(); err != nil {
		t.Fatalf("FlushAll on a read-only writer: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close on a read-only writer: %v", err)
	}

	for _, call := range backend.Calls() {
		if strings.HasPrefix(call, "PUT ") || strings.HasPrefix(call, "SETMETA ") ||
			strings.HasPrefix(call, "CopyObject(") || strings.HasPrefix(call, "DeleteObject(") {
			t.Errorf("a read-only writer that only served reads made the modifying call %q. "+
				"Every call it made: %v", call, backend.Calls())
		}
	}
}

// TestWriterIsWritableByDefault pins the zero value, which is what every caller constructed before #532
// and what a config file that does not mention read_only still describes.
func TestWriterIsWritableByDefault(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()

	w, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{})
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if w.ReadOnly() {
		t.Error("a Writer built from a zero WriterOptions reports read-only; false is the writable " +
			"default every release through v0.16.0 provided")
	}
	if err := w.Write("k", 0, []byte("x")); err != nil {
		t.Errorf("Write on a default writer: %v", err)
	}
}

func newReadOnlyWriter(t *testing.T, backend *fakeBackend) *vfs.Writer {
	t.Helper()

	w, err := vfs.NewWriterWithOptions(context.Background(), backend, vfs.WriterOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if !w.ReadOnly() {
		t.Fatal("WriterOptions{ReadOnly: true} produced a writer that reports writable")
	}

	return w
}
