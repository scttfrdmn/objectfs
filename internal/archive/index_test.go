package archive

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	archivepkg "github.com/scttfrdmn/objectfs/pkg/archive"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// ── BuildIndexFromBytes ───────────────────────────────────────────────────────

func TestBuildIndexFromBytes_TarGzip(t *testing.T) {
	t.Parallel()
	data := makeTarGz(t, []tarEntry{
		{name: "file1.txt", content: "hello"},
		{name: "file2.txt", content: "world!"},
		{name: "subdir/"},
		{name: "subdir/nested.txt", content: "nested"},
	})

	meta, err := BuildIndexFromBytes("test.tar.gz", archivepkg.FormatTarGzip, data)
	if err != nil {
		t.Fatalf("BuildIndexFromBytes: %v", err)
	}

	if meta.Path != "test.tar.gz" {
		t.Errorf("Path = %q, want test.tar.gz", meta.Path)
	}
	if meta.Format != archivepkg.FormatTarGzip {
		t.Errorf("Format = %q, want %q", meta.Format, archivepkg.FormatTarGzip)
	}
	if meta.Index == nil {
		t.Fatal("Index is nil")
	}
	if meta.Index.TotalFiles == 0 {
		t.Error("TotalFiles = 0, want > 0")
	}
	// UncompressedSize should cover file bytes (not dirs).
	wantSize := int64(len("hello") + len("world!") + len("nested"))
	if meta.UncompressedSize != wantSize {
		t.Errorf("UncompressedSize = %d, want %d", meta.UncompressedSize, wantSize)
	}
}

func TestBuildIndexFromBytes_FileContent(t *testing.T) {
	t.Parallel()
	modTime := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)
	data := makeTarGz(t, []tarEntry{
		{name: "data.bin", content: "binary", modTime: modTime},
	})

	meta, err := BuildIndexFromBytes("archive.tar.gz", archivepkg.FormatTarGzip, data)
	if err != nil {
		t.Fatalf("BuildIndexFromBytes: %v", err)
	}

	entry, ok := meta.Index.GetEntry("data.bin")
	if !ok {
		t.Fatal("entry data.bin not found in index")
	}
	if entry.Size != int64(len("binary")) {
		t.Errorf("Size = %d, want %d", entry.Size, len("binary"))
	}
	if entry.Name != "data.bin" {
		t.Errorf("Name = %q, want data.bin", entry.Name)
	}
	if entry.IsDir {
		t.Error("IsDir = true for file entry")
	}
	if !entry.ModTime.Equal(modTime) {
		t.Errorf("ModTime = %v, want %v", entry.ModTime, modTime)
	}
}

func TestBuildIndexFromBytes_DirEntry(t *testing.T) {
	t.Parallel()
	data := makeTarGz(t, []tarEntry{
		{name: "mydir/"},
		{name: "mydir/child.txt", content: "x"},
	})

	meta, err := BuildIndexFromBytes("a.tar.gz", archivepkg.FormatTarGzip, data)
	if err != nil {
		t.Fatalf("BuildIndexFromBytes: %v", err)
	}

	// The explicit dir entry "mydir" should be indexed.
	entry, ok := meta.Index.GetEntry("mydir")
	if !ok {
		t.Error("directory entry mydir not found in index")
	} else if !entry.IsDir {
		t.Error("mydir entry has IsDir=false")
	}
}

func TestBuildIndexFromBytes_SkipsRootDot(t *testing.T) {
	// Archives created with certain tools include a "." root entry;
	// BuildIndexFromBytes should silently skip it.
	t.Parallel()
	data := makeTarGz(t, []tarEntry{
		{name: "./", content: ""}, // root entry with Typeflag=TypeDir
		{name: "./file.txt", content: "contents"},
	})

	meta, err := BuildIndexFromBytes("root.tar.gz", archivepkg.FormatTarGzip, data)
	if err != nil {
		t.Fatalf("BuildIndexFromBytes: %v", err)
	}

	// Should find "file.txt" (cleaned from "./file.txt"), not "."
	if _, ok := meta.Index.GetEntry("."); ok {
		t.Error("index contains '.' entry, expected it to be skipped")
	}
}

func TestBuildIndexFromBytes_UnsupportedFormat(t *testing.T) {
	t.Parallel()
	_, err := BuildIndexFromBytes("x.tar.lz4", "tar.lz4", []byte("garbage"))
	if err == nil {
		t.Fatal("expected error for unsupported format, got nil")
	}
}

func TestBuildIndexFromBytes_CorruptData(t *testing.T) {
	t.Parallel()
	_, err := BuildIndexFromBytes("bad.tar.gz", archivepkg.FormatTarGzip, []byte("not a gzip"))
	if err == nil {
		t.Fatal("expected error for corrupt archive, got nil")
	}
}

func TestBuildIndexFromBytes_EmptyArchive(t *testing.T) {
	t.Parallel()
	// An archive with no entries (just end-of-archive blocks) is valid.
	data := makeTarGz(t, []tarEntry{})

	meta, err := BuildIndexFromBytes("empty.tar.gz", archivepkg.FormatTarGzip, data)
	if err != nil {
		t.Fatalf("BuildIndexFromBytes(empty): %v", err)
	}
	if meta.Index.TotalFiles != 0 {
		t.Errorf("TotalFiles = %d, want 0", meta.Index.TotalFiles)
	}
}

// ── BuildIndex ────────────────────────────────────────────────────────────────

func TestBuildIndex_Basic(t *testing.T) {
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{
		{name: "a.txt", content: "aaa"},
		{name: "b.txt", content: "bbbb"},
	})

	backend := &mockBackend{objects: map[string][]byte{"data.tar.gz": archiveData}}
	meta, err := BuildIndex(context.Background(), backend, "data.tar.gz")
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if meta.Index == nil {
		t.Fatal("Index is nil")
	}
	if _, ok := meta.Index.GetEntry("a.txt"); !ok {
		t.Error("a.txt not found in index")
	}
	if _, ok := meta.Index.GetEntry("b.txt"); !ok {
		t.Error("b.txt not found in index")
	}
}

func TestBuildIndex_NotAnArchive(t *testing.T) {
	t.Parallel()
	backend := &mockBackend{objects: map[string][]byte{}}
	_, err := BuildIndex(context.Background(), backend, "plain.txt")
	if err == nil {
		t.Fatal("expected error for non-archive key, got nil")
	}
}

func TestBuildIndex_BackendError(t *testing.T) {
	t.Parallel()
	// Empty backend — GetObject will fail.
	backend := &mockBackend{objects: map[string][]byte{}}
	_, err := BuildIndex(context.Background(), backend, "missing.tar.gz")
	if err == nil {
		t.Fatal("expected error when backend returns not-found, got nil")
	}
}

func TestBuildIndex_HeadObjectSupplementsTimestamp(t *testing.T) {
	// When HeadObject succeeds, the returned metadata should use the S3
	// LastModified timestamp instead of time.Now().
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{{name: "f.txt", content: "x"}})
	s3Time := time.Date(2023, 3, 15, 8, 0, 0, 0, time.UTC)
	backend := &headableBackend{
		mockBackend: &mockBackend{objects: map[string][]byte{"ts.tar.gz": archiveData}},
		headInfo: &types.ObjectInfo{
			Key:          "ts.tar.gz",
			LastModified: s3Time,
			ETag:         "abc123",
		},
	}

	meta, err := BuildIndex(context.Background(), backend, "ts.tar.gz")
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if !meta.LastModified.Equal(s3Time) {
		t.Errorf("LastModified = %v, want %v (from HeadObject)", meta.LastModified, s3Time)
	}
	if meta.Checksum != "abc123" {
		t.Errorf("Checksum = %q, want abc123", meta.Checksum)
	}
}

// ── performance smoke test ────────────────────────────────────────────────────

func TestBuildIndexFromBytes_LargeArchive(t *testing.T) {
	// Build an archive with 1000 files and verify the index is populated
	// and the operation completes in a reasonable time.  This is a smoke
	// test; the actual performance target (<1 s) is best measured in a
	// benchmark, but we at least verify correctness at scale.
	t.Parallel()
	entries := make([]tarEntry, 1000)
	for i := range entries {
		entries[i] = tarEntry{
			name:    fmt.Sprintf("file%04d.txt", i),
			content: fmt.Sprintf("content of file %d", i),
		}
	}
	data := makeTarGz(t, entries)

	meta, err := BuildIndexFromBytes("large.tar.gz", archivepkg.FormatTarGzip, data)
	if err != nil {
		t.Fatalf("BuildIndexFromBytes(1000 files): %v", err)
	}
	if meta.Index.TotalFiles != 1000 {
		t.Errorf("TotalFiles = %d, want 1000", meta.Index.TotalFiles)
	}
}

// ── test helpers ──────────────────────────────────────────────────────────────

// headableBackend extends mockBackend with a custom HeadObject response.
type headableBackend struct {
	*mockBackend
	headInfo *types.ObjectInfo
	headErr  error
}

func (h *headableBackend) HeadObject(_ context.Context, _ string) (*types.ObjectInfo, error) {
	if h.headErr != nil {
		return nil, h.headErr
	}
	return h.headInfo, nil
}

func TestBuildIndex_HeadObjectFailureIsNonFatal(t *testing.T) {
	// If HeadObject returns an error, BuildIndex should still succeed with
	// the time.Now() fallback timestamp.
	t.Parallel()
	archiveData := makeTarGz(t, []tarEntry{{name: "f.txt", content: "x"}})
	backend := &headableBackend{
		mockBackend: &mockBackend{objects: map[string][]byte{"hf.tar.gz": archiveData}},
		headErr:     errors.New("S3 access denied"),
	}

	meta, err := BuildIndex(context.Background(), backend, "hf.tar.gz")
	if err != nil {
		t.Fatalf("BuildIndex should not fail when HeadObject fails: %v", err)
	}
	if meta.Index == nil {
		t.Fatal("Index is nil after non-fatal HeadObject failure")
	}
}
