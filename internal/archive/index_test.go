package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
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

// TestBuildIndexFromBytesRejectsAModeItCannotRecord covers the one G115 site of #525 that was a defect
// rather than a warning, and the bound it settles on is the point of the table.
//
// hdr.Mode is an int64 read from an archive this process did not write; ArchiveEntry.Mode is a uint32.
// The conversion between them used to be unchecked, so a mode outside 32 bits was recorded as a
// different one — and a plausible one. 0x1_0000_0644 narrows to 0644 and -1 narrows to 0777 with every
// special bit set: a crafted archive got to state one mode and have another presented, with nothing
// downstream able to notice.
//
// Both of those are writable and readable by Go's own archive/tar in its default format, measured
// rather than assumed — only the PAX writer refuses them — so this is reachable with a file, not a
// hypothetical about some other tar implementation.
//
// The accepted rows are what stops the fix from being a stricter one. internal/vfs rejects a mode
// carrying bits outside 0o7777 and that would be the obvious rule to copy here, but 0100644 is what
// Apache Commons Compress writes for a regular file, so the strict rule rejects archives from a
// mainstream producer. The bound is representability: inside it, recorded exactly as stated.
func TestBuildIndexFromBytesRejectsAModeItCannotRecord(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		mode int64
		want uint32 // only read when the entry is accepted
		ok   bool
	}{
		{"an ordinary permission mode", 0o644, 0o644, true},
		{"the file-type bits Apache Commons Compress writes", 0o100644, 0o100644, true},
		{"the largest mode a uint32 can hold", math.MaxUint32, math.MaxUint32, true},
		{"one more than that, which used to be recorded as 0644", math.MaxUint32 + 0o644 + 1, 0, false},
		{"a negative mode, which used to be recorded as 0777 with every special bit", -1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data := tarGzWithMode(t, "f.txt", tc.mode)

			meta, err := BuildIndexFromBytes("modes.tar.gz", archivepkg.FormatTarGzip, data)

			if !tc.ok {
				if err == nil {
					entry, _ := meta.Index.GetEntry("f.txt")
					t.Fatalf("an entry stating mode %#o (%d) was indexed as %#o. It does not fit the "+
						"32 bits a mode is recorded in, so what was recorded is not what the archive "+
						"says — which is the whole defect: the number that comes out is a valid-looking "+
						"mode nobody can tell apart from a stated one", tc.mode, tc.mode, entry.Mode)
				}

				if !errors.Is(err, ErrMalformedEntry) {
					t.Errorf("rejected with %v, which does not wrap ErrMalformedEntry. The sentinel is "+
						"how a caller tells a header it cannot represent from a read or decompression "+
						"failure, and a rejection for the right reason is not the same as a rejection",
						err)
				}

				if !strings.Contains(err.Error(), "f.txt") {
					t.Errorf("the rejection %q does not name the entry. An operator holding a rejected "+
						"archive needs to know which of its entries to look at", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("mode %#o is representable in 32 bits and must be indexed as stated: %v",
					tc.mode, err)
			}

			entry, ok := meta.Index.GetEntry("f.txt")
			if !ok {
				t.Fatalf("f.txt is absent from the index built from an archive containing it")
			}

			if entry.Mode != tc.want {
				t.Errorf("mode %#o was recorded as %#o, want %#o", tc.mode, entry.Mode, tc.want)
			}
		})
	}
}

// tarGzWithMode builds a one-entry tar.gz whose header states mode exactly.
//
// Not makeTarGz, which hardcodes 0644 and 0755: the mode is the subject here. The format is left
// unset so archive/tar picks one that can encode the value — PAX cannot encode any of the
// out-of-range rows, and asking for it would make the test fail at the fixture instead of at the
// assertion.
func tarGzWithMode(t *testing.T, name string, mode int64) []byte {
	t.Helper()

	var buf bytes.Buffer

	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Typeflag: tar.TypeReg,
		Mode:     mode,
		ModTime:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("WriteHeader(mode %#o): %v", mode, err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tar.Writer.Close: %v", err)
	}

	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip.Writer.Close: %v", err)
	}

	return buf.Bytes()
}
