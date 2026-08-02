package archive

import (
	"testing"

	archivepkg "github.com/scttfrdmn/objectfs/pkg/archive"
)

func TestTranslate_Empty(t *testing.T) {
	t.Parallel()
	p := Translate("")
	if p.IsArchivePath {
		t.Error("IsArchivePath = true for empty path, want false")
	}
	if p.RawPath != "" {
		t.Errorf("RawPath = %q, want empty", p.RawPath)
	}
}

func TestTranslate_PlainFile(t *testing.T) {
	t.Parallel()
	p := Translate("regular/file.txt")
	if p.IsArchivePath {
		t.Errorf("IsArchivePath = true for %q, want false", p.RawPath)
	}
}

func TestTranslate_ArchiveRoot_TarZstd(t *testing.T) {
	t.Parallel()
	p := Translate("data.tar.zst")
	if !p.IsArchivePath {
		t.Fatal("IsArchivePath = false, want true")
	}
	if p.ArchiveKey != "data.tar.zst" {
		t.Errorf("ArchiveKey = %q, want %q", p.ArchiveKey, "data.tar.zst")
	}
	if p.InnerPath != "" {
		t.Errorf("InnerPath = %q, want empty", p.InnerPath)
	}
	if p.ArchiveFormat != archivepkg.FormatTarZstd {
		t.Errorf("ArchiveFormat = %q, want %q", p.ArchiveFormat, archivepkg.FormatTarZstd)
	}
}

func TestTranslate_ArchiveRoot_TarGzip(t *testing.T) {
	t.Parallel()
	p := Translate("archive.tar.gz")
	if !p.IsArchivePath {
		t.Fatal("IsArchivePath = false, want true")
	}
	if p.ArchiveFormat != archivepkg.FormatTarGzip {
		t.Errorf("ArchiveFormat = %q, want %q", p.ArchiveFormat, archivepkg.FormatTarGzip)
	}
}

func TestTranslate_ArchiveRoot_Tgz(t *testing.T) {
	t.Parallel()
	p := Translate("archive.tgz")
	if !p.IsArchivePath {
		t.Fatal("IsArchivePath = false, want true")
	}
	if p.ArchiveFormat != archivepkg.FormatTarGzip {
		t.Errorf("ArchiveFormat = %q, want %q", p.ArchiveFormat, archivepkg.FormatTarGzip)
	}
}

func TestTranslate_ArchiveRoot_TarBzip2(t *testing.T) {
	t.Parallel()
	p := Translate("data.tar.bz2")
	if !p.IsArchivePath {
		t.Fatal("IsArchivePath = false, want true")
	}
	if p.ArchiveFormat != archivepkg.FormatTarBzip2 {
		t.Errorf("ArchiveFormat = %q, want %q", p.ArchiveFormat, archivepkg.FormatTarBzip2)
	}
}

func TestTranslate_InnerFile(t *testing.T) {
	t.Parallel()
	p := Translate("data.tar.gz/subdir/file.txt")
	if !p.IsArchivePath {
		t.Fatal("IsArchivePath = false, want true")
	}
	if p.ArchiveKey != "data.tar.gz" {
		t.Errorf("ArchiveKey = %q, want %q", p.ArchiveKey, "data.tar.gz")
	}
	if p.InnerPath != "subdir/file.txt" {
		t.Errorf("InnerPath = %q, want %q", p.InnerPath, "subdir/file.txt")
	}
}

func TestTranslate_NestedDirectory(t *testing.T) {
	t.Parallel()
	p := Translate("datasets/genomes/ref.tar.zst/chr1/reads.fasta")
	if !p.IsArchivePath {
		t.Fatal("IsArchivePath = false, want true")
	}
	if p.ArchiveKey != "datasets/genomes/ref.tar.zst" {
		t.Errorf("ArchiveKey = %q, want %q", p.ArchiveKey, "datasets/genomes/ref.tar.zst")
	}
	if p.InnerPath != "chr1/reads.fasta" {
		t.Errorf("InnerPath = %q, want %q", p.InnerPath, "chr1/reads.fasta")
	}
}

func TestTranslate_ArchiveTopLevelInnerFile(t *testing.T) {
	t.Parallel()
	p := Translate("archive.tar.gz/file.txt")
	if !p.IsArchivePath {
		t.Fatal("IsArchivePath = false, want true")
	}
	if p.InnerPath != "file.txt" {
		t.Errorf("InnerPath = %q, want %q", p.InnerPath, "file.txt")
	}
}

// TestTranslate_NonArchiveDirectory verifies that a plain directory name that
// happens to contain a file later isn't incorrectly detected as an archive.
func TestTranslate_NonArchiveDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
	}{
		{"dir/subdir/file.txt"},
		{"data.csv"},
		{"results/output.json"},
		{"report.pdf"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			p := Translate(tt.path)
			if p.IsArchivePath {
				t.Errorf("Translate(%q).IsArchivePath = true, want false", tt.path)
			}
		})
	}
}

func TestJoin_WithInnerPath(t *testing.T) {
	t.Parallel()
	got := Join("dir/data.tar.gz", "subdir/file.txt")
	want := "dir/data.tar.gz/subdir/file.txt"
	if got != want {
		t.Errorf("Join() = %q, want %q", got, want)
	}
}

func TestJoin_EmptyInnerPath(t *testing.T) {
	t.Parallel()
	got := Join("archive.tar.gz", "")
	if got != "archive.tar.gz" {
		t.Errorf("Join() = %q, want %q", got, "archive.tar.gz")
	}
}
