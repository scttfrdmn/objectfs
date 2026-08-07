package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// ── mock backend ──────────────────────────────────────────────────────────────

type mockBackend struct {
	objects map[string][]byte
}

func (m *mockBackend) GetObject(_ context.Context, key string, _, _ int64) ([]byte, error) {
	data, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("mockBackend: not found: %q", key)
	}
	return data, nil
}

func (m *mockBackend) PutObject(_ context.Context, _ string, _ []byte, _ map[string]string) error {
	return nil
}

// PutObjectIf refuses rather than pretending: nothing in this package issues a conditional write, and
// ErrNotSupported is the answer the interface requires of an implementation that cannot evaluate one.
func (m *mockBackend) PutObjectIf(_ context.Context, _ string, _ []byte, _ map[string]string,
	_ types.Precondition,
) (string, error) {
	return "", types.ErrNotSupported
}
func (m *mockBackend) SetObjectMetadata(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

// CopyObject copies the stored bytes rather than no-opping, because this mock's reads come out of the
// same map: a copy that recorded nothing would leave dst absent while reporting success, and a caller
// checking whether the copy landed would read a not-found as a copy bug.
func (m *mockBackend) CopyObject(_ context.Context, src, dst string) error {
	data, ok := m.objects[src]
	if !ok {
		return fmt.Errorf("mockBackend: not found: %q", src)
	}
	m.objects[dst] = data
	return nil
}
func (m *mockBackend) DeleteObject(_ context.Context, _ string) error { return nil }
func (m *mockBackend) HeadObject(_ context.Context, _ string) (*types.ObjectInfo, error) {
	return nil, nil
}
func (m *mockBackend) GetObjects(_ context.Context, _ []string) (map[string][]byte, error) {
	return nil, nil
}
func (m *mockBackend) PutObjects(_ context.Context, _ map[string][]byte) error { return nil }
func (m *mockBackend) ListObjects(_ context.Context, _ string, _ int) ([]types.ObjectInfo, error) {
	return nil, nil
}
func (m *mockBackend) HealthCheck(_ context.Context) error { return nil }

// ── test archive builder ──────────────────────────────────────────────────────

// tarEntry describes a single entry for makeTarGz.
type tarEntry struct {
	name    string // path inside archive (use trailing "/" for directories)
	content string // file content; ignored for directories
	modTime time.Time
}

// makeTarGz creates an in-memory gzip-compressed tar archive.
func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	epoch := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range entries {
		mt := e.modTime
		if mt.IsZero() {
			mt = epoch
		}
		hdr := &tar.Header{
			Name:    e.name,
			ModTime: mt,
		}
		isDir := len(e.name) > 0 && e.name[len(e.name)-1] == '/'
		if isDir {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0755
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Mode = 0644
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader %q: %v", e.name, err)
		}
		if !isDir && len(e.content) > 0 {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatalf("Write %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar.Writer.Close: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip.Writer.Close: %v", err)
	}
	return buf.Bytes()
}

// ── helpers ───────────────────────────────────────────────────────────────────

// ── tests ─────────────────────────────────────────────────────────────────────

func TestVFS_Stat_Root(t *testing.T) {
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{
		{name: "file.txt", content: "hello"},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"test.tar.gz": archiveData}})

	info, err := vfs.Stat(context.Background(), "test.tar.gz", "")
	if err != nil {
		t.Fatalf("Stat(root): %v", err)
	}
	if info.ContentType != "application/x-directory" {
		t.Errorf("ContentType = %q, want application/x-directory", info.ContentType)
	}
	if info.Key != "test.tar.gz" {
		t.Errorf("Key = %q, want test.tar.gz", info.Key)
	}
}

func TestVFS_Stat_File(t *testing.T) {
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{
		{name: "data/file.txt", content: "hello world"},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"archive.tar.gz": archiveData}})

	info, err := vfs.Stat(context.Background(), "archive.tar.gz", "data/file.txt")
	if err != nil {
		t.Fatalf("Stat(file): %v", err)
	}
	if info.Size != 11 {
		t.Errorf("Size = %d, want 11", info.Size)
	}
	if info.ContentType == "application/x-directory" {
		t.Error("ContentType = directory for a file entry")
	}
	if info.Key != "archive.tar.gz/data/file.txt" {
		t.Errorf("Key = %q, want archive.tar.gz/data/file.txt", info.Key)
	}
}

func TestVFS_Stat_ExplicitDir(t *testing.T) {
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{
		{name: "subdir/"},
		{name: "subdir/file.txt", content: "x"},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"a.tar.gz": archiveData}})

	info, err := vfs.Stat(context.Background(), "a.tar.gz", "subdir")
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if info.ContentType != "application/x-directory" {
		t.Errorf("ContentType = %q, want application/x-directory", info.ContentType)
	}
}

func TestVFS_Stat_VirtualDir(t *testing.T) {
	// Archives created without explicit directory entries should still have
	// virtual directories detected from file paths.
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{
		{name: "implicit/nested/file.txt", content: "data"},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"v.tar.gz": archiveData}})

	// "implicit" is a virtual directory (no explicit dir entry in archive).
	info, err := vfs.Stat(context.Background(), "v.tar.gz", "implicit")
	if err != nil {
		t.Fatalf("Stat(virtual dir): %v", err)
	}
	if info.ContentType != "application/x-directory" {
		t.Errorf("ContentType = %q, want application/x-directory", info.ContentType)
	}
}

func TestVFS_Stat_NotFound(t *testing.T) {
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{
		{name: "real.txt", content: "exists"},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"x.tar.gz": archiveData}})

	_, err := vfs.Stat(context.Background(), "x.tar.gz", "missing.txt")
	if err == nil {
		t.Fatal("expected error for missing entry, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want wrapping ErrNotFound", err)
	}
}

func TestVFS_Stat_UnknownArchive(t *testing.T) {
	t.Parallel()
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{}})
	_, err := vfs.Stat(context.Background(), "plain.txt", "")
	if err == nil {
		t.Fatal("expected error for non-archive key, got nil")
	}
}

func TestVFS_ReadDir_Root(t *testing.T) {
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{
		{name: "file1.txt", content: "a"},
		{name: "file2.txt", content: "bb"},
		{name: "subdir/nested.txt", content: "ccc"},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"test.tar.gz": archiveData}})

	entries, err := vfs.ReadDir(context.Background(), "test.tar.gz", "")
	if err != nil {
		t.Fatalf("ReadDir(root): %v", err)
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	sort.Strings(names)

	want := []string{"file1.txt", "file2.txt", "subdir"}
	if len(names) != len(want) {
		t.Fatalf("ReadDir names = %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, n, want[i])
		}
	}
}

func TestVFS_ReadDir_Subdir(t *testing.T) {
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{
		{name: "dir/a.txt", content: "1"},
		{name: "dir/b.txt", content: "22"},
		{name: "dir/deep/c.txt", content: "333"},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"s.tar.gz": archiveData}})

	entries, err := vfs.ReadDir(context.Background(), "s.tar.gz", "dir")
	if err != nil {
		t.Fatalf("ReadDir(dir): %v", err)
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	sort.Strings(names)

	want := []string{"a.txt", "b.txt", "deep"}
	if len(names) != len(want) {
		t.Fatalf("ReadDir(dir) names = %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, n, want[i])
		}
	}
}

func TestVFS_ReadDir_NoDuplicateDirs(t *testing.T) {
	// Multiple files under the same subdirectory should produce exactly one
	// synthetic directory entry.
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{
		{name: "shared/file1.txt", content: "1"},
		{name: "shared/file2.txt", content: "2"},
		{name: "shared/file3.txt", content: "3"},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"d.tar.gz": archiveData}})

	entries, err := vfs.ReadDir(context.Background(), "d.tar.gz", "")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name
		}
		t.Errorf("ReadDir returned %d entries %v, want 1 synthetic dir", len(entries), names)
	}
	if entries[0].Name != "shared" {
		t.Errorf("entry name = %q, want %q", entries[0].Name, "shared")
	}
	if !entries[0].IsDir {
		t.Error("synthetic directory entry has IsDir=false")
	}
}

func TestVFS_ReadFile_FullContent(t *testing.T) {
	t.Parallel()
	content := "hello, archive world!"
	archiveData := makeTarGz(t, []tarEntry{
		{name: "greet.txt", content: content},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"r.tar.gz": archiveData}})

	got, err := vfs.ReadFile(context.Background(), "r.tar.gz", "greet.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != content {
		t.Errorf("ReadFile = %q, want %q", got, content)
	}
}

func TestVFS_ReadFile_WithOffset(t *testing.T) {
	t.Parallel()
	content := "abcdefghij"
	archiveData := makeTarGz(t, []tarEntry{
		{name: "alpha.txt", content: content},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"o.tar.gz": archiveData}})

	got, err := vfs.ReadFile(context.Background(), "o.tar.gz", "alpha.txt", 2, 5)
	if err != nil {
		t.Fatalf("ReadFile(offset=2, size=5): %v", err)
	}
	want := "cdefg"
	if string(got) != want {
		t.Errorf("ReadFile = %q, want %q", got, want)
	}
}

func TestVFS_ReadFile_BeyondEOF(t *testing.T) {
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{
		{name: "tiny.txt", content: "hi"},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"e.tar.gz": archiveData}})

	// Offset beyond EOF should return nil/empty.
	got, err := vfs.ReadFile(context.Background(), "e.tar.gz", "tiny.txt", 100, 10)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadFile(beyond EOF) = %q, want empty", got)
	}
}

func TestVFS_ReadFile_NotFound(t *testing.T) {
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{
		{name: "exists.txt", content: "yes"},
	})
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{"nf.tar.gz": archiveData}})

	_, err := vfs.ReadFile(context.Background(), "nf.tar.gz", "absent.txt", 0, 0)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want wrapping ErrNotFound", err)
	}
}

func TestVFS_ReadFile_EmptyInnerPath(t *testing.T) {
	t.Parallel()
	vfs := NewVFS(&mockBackend{objects: map[string][]byte{}})
	_, err := vfs.ReadFile(context.Background(), "x.tar.gz", "", 0, 0)
	if err == nil {
		t.Fatal("expected error for empty innerPath, got nil")
	}
}

func TestVFS_IndexCaching(t *testing.T) {
	// The backend should only be called once per archive even with multiple Stat calls.
	t.Parallel()
	calls := 0
	backend := &countingBackend{
		mockBackend: &mockBackend{
			objects: map[string][]byte{
				"c.tar.gz": makeTarGz(t, []tarEntry{{name: "f.txt", content: "x"}}),
			},
		},
		onGet: func() { calls++ },
	}
	vfs := NewVFS(backend)
	ctx := context.Background()

	for i := range 5 {
		if _, err := vfs.Stat(ctx, "c.tar.gz", "f.txt"); err != nil {
			t.Fatalf("Stat iteration %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("backend.GetObject called %d times, want 1 (index should be cached)", calls)
	}
}

func TestVFS_ContentCaching(t *testing.T) {
	// Content extraction should only happen once per unique path.
	t.Parallel()
	calls := 0
	backend := &countingBackend{
		mockBackend: &mockBackend{
			objects: map[string][]byte{
				"cc.tar.gz": makeTarGz(t, []tarEntry{{name: "data.txt", content: "payload"}}),
			},
		},
		onGet: func() { calls++ },
	}
	vfs := NewVFS(backend)
	ctx := context.Background()

	for i := range 3 {
		if _, err := vfs.ReadFile(ctx, "cc.tar.gz", "data.txt", 0, 0); err != nil {
			t.Fatalf("ReadFile iteration %d: %v", i, err)
		}
	}
	// First call: one GetObject for index + one for content = 2.
	// Subsequent calls: index cached, content cached → 0 more.
	if calls > 2 {
		t.Errorf("backend.GetObject called %d times for 3 reads, want ≤ 2", calls)
	}
}

func TestVFS_Invalidate(t *testing.T) {
	t.Parallel()
	calls := 0
	archiveData := makeTarGz(t, []tarEntry{{name: "f.txt", content: "v1"}})
	backend := &countingBackend{
		mockBackend: &mockBackend{objects: map[string][]byte{"inv.tar.gz": archiveData}},
		onGet:       func() { calls++ },
	}
	vfs := NewVFS(backend)
	ctx := context.Background()

	// Prime the index cache.
	if _, err := vfs.Stat(ctx, "inv.tar.gz", "f.txt"); err != nil {
		t.Fatalf("Stat before invalidate: %v", err)
	}
	callsBefore := calls

	// Invalidate forces a reload.
	vfs.Invalidate("inv.tar.gz")
	if _, err := vfs.Stat(ctx, "inv.tar.gz", "f.txt"); err != nil {
		t.Fatalf("Stat after invalidate: %v", err)
	}
	if calls <= callsBefore {
		t.Errorf("backend calls did not increase after Invalidate (before=%d after=%d)", callsBefore, calls)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// countingBackend wraps mockBackend and calls onGet for each GetObject.
type countingBackend struct {
	*mockBackend
	onGet func()
}

func (c *countingBackend) GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error) {
	c.onGet()
	return c.mockBackend.GetObject(ctx, key, offset, size)
}
