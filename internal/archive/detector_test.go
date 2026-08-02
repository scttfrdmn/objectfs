package archive

import (
	"context"
	"testing"
	"time"

	archivepkg "github.com/scttfrdmn/objectfs/pkg/archive"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func makeObjectInfo(key string, size int64) types.ObjectInfo {
	return types.ObjectInfo{
		Key:          key,
		Size:         size,
		LastModified: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		ETag:         "etag-" + key,
	}
}

// ── Detect ────────────────────────────────────────────────────────────────────

func TestDetect_Empty(t *testing.T) {
	t.Parallel()
	got := Detect(nil)
	if len(got) != 0 {
		t.Errorf("Detect(nil) = %d items, want 0", len(got))
	}
}

func TestDetect_NoArchives(t *testing.T) {
	t.Parallel()
	objects := []types.ObjectInfo{
		makeObjectInfo("README.md", 100),
		makeObjectInfo("data/file.txt", 200),
		makeObjectInfo("image.png", 300),
	}
	got := Detect(objects)
	if len(got) != 0 {
		t.Errorf("Detect(no archives) = %d items, want 0", len(got))
	}
}

func TestDetect_AllArchives(t *testing.T) {
	t.Parallel()
	objects := []types.ObjectInfo{
		makeObjectInfo("data.tar.zst", 1000),
		makeObjectInfo("logs.tar.gz", 2000),
		makeObjectInfo("backup.tgz", 3000),
		makeObjectInfo("archive.tar.bz2", 4000),
	}
	got := Detect(objects)
	if len(got) != 4 {
		t.Fatalf("Detect(all archives) = %d items, want 4", len(got))
	}
}

func TestDetect_MixedObjects(t *testing.T) {
	t.Parallel()
	objects := []types.ObjectInfo{
		makeObjectInfo("readme.txt", 50),
		makeObjectInfo("dataset.tar.zst", 5000),
		makeObjectInfo("config.yaml", 200),
		makeObjectInfo("results.tar.gz", 3000),
		makeObjectInfo("model.pkl", 900),
	}
	got := Detect(objects)
	if len(got) != 2 {
		t.Fatalf("Detect(mixed) = %d items, want 2", len(got))
	}
}

func TestDetect_PopulatesFields(t *testing.T) {
	t.Parallel()
	modTime := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	objects := []types.ObjectInfo{
		{
			Key:          "genomes/ref.tar.zst",
			Size:         123456,
			LastModified: modTime,
			ETag:         "abc-etag",
		},
	}
	got := Detect(objects)
	if len(got) != 1 {
		t.Fatalf("want 1 result, got %d", len(got))
	}
	m := got[0]
	if m.Path != "genomes/ref.tar.zst" {
		t.Errorf("Path = %q, want genomes/ref.tar.zst", m.Path)
	}
	if m.Format != archivepkg.FormatTarZstd {
		t.Errorf("Format = %q, want %q", m.Format, archivepkg.FormatTarZstd)
	}
	if m.Size != 123456 {
		t.Errorf("Size = %d, want 123456", m.Size)
	}
	if !m.LastModified.Equal(modTime) {
		t.Errorf("LastModified = %v, want %v", m.LastModified, modTime)
	}
	if m.Checksum != "abc-etag" {
		t.Errorf("Checksum = %q, want abc-etag", m.Checksum)
	}
	// Index is nil until BuildIndex is called.
	if m.Index != nil {
		t.Error("Index should be nil from Detect (not yet built)")
	}
}

func TestDetect_FormatsDetected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key    string
		format archivepkg.ArchiveFormat
	}{
		{"a.tar.zst", archivepkg.FormatTarZstd},
		{"b.tar.gz", archivepkg.FormatTarGzip},
		{"c.tgz", archivepkg.FormatTarGzip},
		{"d.tar.bz2", archivepkg.FormatTarBzip2},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			objects := []types.ObjectInfo{makeObjectInfo(tt.key, 100)}
			got := Detect(objects)
			if len(got) != 1 {
				t.Fatalf("Detect(%q) = %d results, want 1", tt.key, len(got))
			}
			if got[0].Format != tt.format {
				t.Errorf("Format = %q, want %q", got[0].Format, tt.format)
			}
		})
	}
}

// ── DetectKeys ────────────────────────────────────────────────────────────────

func TestDetectKeys_Empty(t *testing.T) {
	t.Parallel()
	if keys := DetectKeys(nil); len(keys) != 0 {
		t.Errorf("DetectKeys(nil) = %v, want empty", keys)
	}
}

func TestDetectKeys_MixedObjects(t *testing.T) {
	t.Parallel()
	objects := []types.ObjectInfo{
		makeObjectInfo("file.txt", 10),
		makeObjectInfo("data.tar.gz", 100),
		makeObjectInfo("other.csv", 20),
		makeObjectInfo("backup.tar.zst", 200),
	}
	keys := DetectKeys(objects)
	if len(keys) != 2 {
		t.Fatalf("DetectKeys = %v, want 2 keys", keys)
	}
	if keys[0] != "data.tar.gz" {
		t.Errorf("keys[0] = %q, want data.tar.gz", keys[0])
	}
	if keys[1] != "backup.tar.zst" {
		t.Errorf("keys[1] = %q, want backup.tar.zst", keys[1])
	}
}

func TestDetectKeys_PreservesOrder(t *testing.T) {
	t.Parallel()
	objects := []types.ObjectInfo{
		makeObjectInfo("z.tar.gz", 10),
		makeObjectInfo("a.tar.gz", 20),
		makeObjectInfo("m.tar.bz2", 30),
	}
	keys := DetectKeys(objects)
	want := []string{"z.tar.gz", "a.tar.gz", "m.tar.bz2"}
	if len(keys) != len(want) {
		t.Fatalf("DetectKeys = %v, want %v", keys, want)
	}
	for i, k := range keys {
		if k != want[i] {
			t.Errorf("keys[%d] = %q, want %q", i, k, want[i])
		}
	}
}

// ── DetectInPrefix ────────────────────────────────────────────────────────────

func TestDetectInPrefix_Basic(t *testing.T) {
	t.Parallel()
	backend := &listableBackend{
		objects: []types.ObjectInfo{
			makeObjectInfo("datasets/genome.tar.zst", 1000),
			makeObjectInfo("datasets/readme.txt", 50),
			makeObjectInfo("datasets/results.tar.gz", 2000),
		},
	}
	got, err := DetectInPrefix(context.Background(), backend, "datasets/", 100)
	if err != nil {
		t.Fatalf("DetectInPrefix: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("DetectInPrefix = %d archives, want 2", len(got))
	}
}

func TestDetectInPrefix_DefaultLimit(t *testing.T) {
	t.Parallel()
	backend := &listableBackend{objects: nil}
	// limit=0 should use default 1000 (not panic or error).
	if _, err := DetectInPrefix(context.Background(), backend, "", 0); err != nil {
		t.Fatalf("DetectInPrefix(limit=0): %v", err)
	}
	if backend.lastLimit != 1000 {
		t.Errorf("limit passed to ListObjects = %d, want 1000", backend.lastLimit)
	}
}

func TestDetectInPrefix_BackendError(t *testing.T) {
	t.Parallel()
	backend := &errorBackend{}
	_, err := DetectInPrefix(context.Background(), backend, "x/", 10)
	if err == nil {
		t.Fatal("expected error from backend, got nil")
	}
}

// ── IsArchiveKey ──────────────────────────────────────────────────────────────

func TestIsArchiveKey_Archive(t *testing.T) {
	t.Parallel()
	ok, fmt := IsArchiveKey("data.tar.zst")
	if !ok {
		t.Error("IsArchiveKey(data.tar.zst) = false, want true")
	}
	if fmt != archivepkg.FormatTarZstd {
		t.Errorf("format = %q, want %q", fmt, archivepkg.FormatTarZstd)
	}
}

func TestIsArchiveKey_NonArchive(t *testing.T) {
	t.Parallel()
	ok, _ := IsArchiveKey("data.csv")
	if ok {
		t.Error("IsArchiveKey(data.csv) = true, want false")
	}
}

// ── stub backends ─────────────────────────────────────────────────────────────

// listableBackend has a configurable ListObjects response.
type listableBackend struct {
	mockBackend
	objects   []types.ObjectInfo
	lastLimit int
}

func (b *listableBackend) ListObjects(_ context.Context, _ string, limit int) ([]types.ObjectInfo, error) {
	b.lastLimit = limit
	return b.objects, nil
}

// errorBackend always returns an error from ListObjects.
type errorBackend struct{ mockBackend }

func (b *errorBackend) ListObjects(_ context.Context, _ string, _ int) ([]types.ObjectInfo, error) {
	return nil, context.DeadlineExceeded
}
