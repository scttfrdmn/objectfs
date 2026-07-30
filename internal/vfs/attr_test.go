package vfs

import (
	"errors"
	"io/fs"
	"strconv"
	"testing"
	"time"
)

func TestFileTypeString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		typ  FileType
		want string
	}{
		{FileTypeRegular, "file"},
		{FileTypeDir, "dir"},
		{FileType(99), "FileType(99)"},
	}

	for _, tc := range tests {
		if got := tc.typ.String(); got != tc.want {
			t.Errorf("FileType(%d).String() = %q, want %q", uint8(tc.typ), got, tc.want)
		}
	}
}

func TestAttrFileMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		attr Attr
		want fs.FileMode
	}{
		{
			name: "regular file keeps its permissions and no type bits",
			attr: Attr{Type: FileTypeRegular, Mode: 0o644},
			want: 0o644,
		},
		{
			name: "directory gains the directory bit",
			attr: Attr{Type: FileTypeDir, Mode: 0o755},
			want: fs.ModeDir | 0o755,
		},
		{
			// The mode field holds permissions only. If a stray type bit got in, FileMode must not
			// pass it through — a mode with ModeSymlink set would make the kernel treat an object as
			// a symlink, and ObjectFS has no symlinks.
			name: "stray non-permission bits are stripped",
			attr: Attr{Type: FileTypeRegular, Mode: fs.ModeSymlink | fs.ModeSetuid | 0o600},
			want: 0o600,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.attr.FileMode(); got != tc.want {
				t.Errorf("FileMode() = %v (%#o), want %v (%#o)", got, got, tc.want, tc.want)
			}
			if got, want := tc.attr.IsDir(), tc.want.IsDir(); got != want {
				t.Errorf("IsDir() = %v, want %v", got, want)
			}
		})
	}
}

// A directory that reports mode 0000 is unusable for every non-root user, which is precisely what
// v0.10.0 shipped. The default must be traversable.
func TestDefaultModesAreUsable(t *testing.T) {
	t.Parallel()

	if DefaultFileMode.Perm() == 0 {
		t.Error("DefaultFileMode is 0000; no user could read a file")
	}
	if DefaultFileMode&0o400 == 0 {
		t.Errorf("DefaultFileMode %#o is not owner-readable", DefaultFileMode)
	}
	if DefaultDirMode&0o100 == 0 {
		t.Errorf("DefaultDirMode %#o lacks owner execute; the directory cannot be traversed", DefaultDirMode)
	}
	if DefaultDirMode&0o400 == 0 {
		t.Errorf("DefaultDirMode %#o is not owner-readable; it cannot be listed", DefaultDirMode)
	}
}

func TestAttrNlinkAndBlocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		attr       Attr
		wantNlink  uint32
		wantBlocks int64
	}{
		{
			name:       "empty file",
			attr:       Attr{Type: FileTypeRegular, Size: 0},
			wantNlink:  1,
			wantBlocks: 0,
		},
		{
			name:       "one byte occupies one block",
			attr:       Attr{Type: FileTypeRegular, Size: 1},
			wantNlink:  1,
			wantBlocks: 1,
		},
		{
			name:       "exactly one block",
			attr:       Attr{Type: FileTypeRegular, Size: 512},
			wantNlink:  1,
			wantBlocks: 1,
		},
		{
			name:       "one byte over rounds up",
			attr:       Attr{Type: FileTypeRegular, Size: 513},
			wantNlink:  1,
			wantBlocks: 2,
		},
		{
			name:       "a mebibyte",
			attr:       Attr{Type: FileTypeRegular, Size: 1 << 20},
			wantNlink:  1,
			wantBlocks: 2048,
		},
		{
			name:       "directory reports two links and no blocks",
			attr:       Attr{Type: FileTypeDir},
			wantNlink:  2,
			wantBlocks: 0,
		},
		{
			// Defensive: a negative size is invalid, but Blocks is called from the stat path where a
			// panic unmounts the filesystem. It must return something, not crash.
			name:       "negative size reports no blocks rather than panicking",
			attr:       Attr{Type: FileTypeRegular, Size: -1},
			wantNlink:  1,
			wantBlocks: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.attr.Nlink(); got != tc.wantNlink {
				t.Errorf("Nlink() = %d, want %d", got, tc.wantNlink)
			}
			if got := tc.attr.Blocks(); got != tc.wantBlocks {
				t.Errorf("Blocks() = %d, want %d", got, tc.wantBlocks)
			}
		})
	}
}

func TestAttrValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		attr    Attr
		wantErr bool
	}{
		{
			name: "valid file",
			attr: Attr{Type: FileTypeRegular, Size: 100, Mode: 0o644},
		},
		{
			name: "valid directory",
			attr: Attr{Type: FileTypeDir, Mode: 0o755},
		},
		{
			name:    "unknown type",
			attr:    Attr{Type: FileType(7)},
			wantErr: true,
		},
		{
			name:    "negative size",
			attr:    Attr{Type: FileTypeRegular, Size: -1},
			wantErr: true,
		},
		{
			name:    "directory with a size",
			attr:    Attr{Type: FileTypeDir, Size: 4096},
			wantErr: true,
		},
		{
			name:    "mode carrying type bits",
			attr:    Attr{Type: FileTypeRegular, Mode: fs.ModeDir | 0o644},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.attr.Validate()
			if tc.wantErr {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("Validate() = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestAttrFromMetadata(t *testing.T) {
	t.Parallel()

	mtime := time.Date(2026, 3, 14, 15, 9, 26, 535000000, time.UTC)
	stored := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		meta       map[string]string
		storedSize int64
		want       Attr
	}{
		{
			name:       "no metadata falls back to defaults",
			storedSize: 1234,
			want: Attr{
				Type:  FileTypeRegular,
				Size:  1234,
				Mode:  DefaultFileMode,
				Mtime: stored,
			},
		},
		{
			name: "full metadata is honoured",
			meta: map[string]string{
				metaMode:     "600",
				metaUID:      "1001",
				metaGID:      "2002",
				metaMtime:    mtime.Format(time.RFC3339Nano),
				metaChecksum: "abc123",
			},
			storedSize: 1234,
			want: Attr{
				Type:     FileTypeRegular,
				Size:     1234,
				Mode:     0o600,
				UID:      1001,
				GID:      2002,
				Mtime:    mtime,
				Checksum: "abc123",
			},
		},
		{
			// Without objectfs-original-size a compressed object reports its compressed length as
			// the file size and the kernel truncates every read at it.
			name:       "original-size overrides the stored length",
			meta:       map[string]string{metaOriginalSize: "12000"},
			storedSize: 48,
			want: Attr{
				Type:  FileTypeRegular,
				Size:  12000,
				Mode:  DefaultFileMode,
				Mtime: stored,
			},
		},
		{
			// S3 lower-cases user-metadata keys, MinIO title-cases them, and an http.Header
			// round-trip canonicalises to Objectfs-Mode. A case-sensitive lookup passes unit tests
			// and fails against real storage.
			name: "keys are matched case-insensitively",
			meta: map[string]string{
				"Objectfs-Mode": "700",
				"OBJECTFS-UID":  "42",
			},
			storedSize: 10,
			want: Attr{
				Type:  FileTypeRegular,
				Size:  10,
				Mode:  0o700,
				UID:   42,
				Mtime: stored,
			},
		},
		{
			// A bad metadata value must not make the object unreadable. A wrong mode is recoverable;
			// a file that cannot be opened is not.
			name: "malformed values fall back to defaults",
			meta: map[string]string{
				metaMode:         "banana",
				metaUID:          "-1",
				metaGID:          "99999999999999999999",
				metaMtime:        "yesterday",
				metaOriginalSize: "not-a-number",
			},
			storedSize: 77,
			want: Attr{
				Type:  FileTypeRegular,
				Size:  77,
				Mode:  DefaultFileMode,
				Mtime: stored,
			},
		},
		{
			name:       "negative original-size is ignored",
			meta:       map[string]string{metaOriginalSize: "-5"},
			storedSize: 77,
			want: Attr{
				Type:  FileTypeRegular,
				Size:  77,
				Mode:  DefaultFileMode,
				Mtime: stored,
			},
		},
		{
			name:       "mode metadata is octal, not decimal",
			meta:       map[string]string{metaMode: "644"},
			storedSize: 1,
			want: Attr{
				Type:  FileTypeRegular,
				Size:  1,
				Mode:  0o644,
				Mtime: stored,
			},
		},
		{
			name:       "stored mode with type bits is masked to permissions",
			meta:       map[string]string{metaMode: "40755"},
			storedSize: 1,
			want: Attr{
				Type:  FileTypeRegular,
				Size:  1,
				Mode:  0o755,
				Mtime: stored,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := AttrFromMetadata(tc.meta, tc.storedSize, stored, "etag-v1")

			want := tc.want
			want.ETag = "etag-v1"
			want.Atime = want.Mtime
			want.Ctime = want.Mtime

			if got != want {
				t.Fatalf("AttrFromMetadata =\n  %+v\nwant\n  %+v", got, want)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("AttrFromMetadata produced an invalid Attr: %v", err)
			}
		})
	}
}

func TestMetadataWarnings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		meta      map[string]string
		wantCount int
	}{
		{
			name:      "no metadata warns about nothing",
			wantCount: 0,
		},
		{
			name: "valid metadata warns about nothing",
			meta: map[string]string{
				metaMode:  "644",
				metaUID:   "0",
				metaGID:   "0",
				metaMtime: time.Now().UTC().Format(time.RFC3339Nano),
			},
			wantCount: 0,
		},
		{
			name:      "one bad value warns once",
			meta:      map[string]string{metaMode: "banana"},
			wantCount: 1,
		},
		{
			name:      "an unparseable original-size warns",
			meta:      map[string]string{metaOriginalSize: "not-a-number"},
			wantCount: 1,
		},
		{
			name: "every bad value warns",
			meta: map[string]string{
				metaMode:         "banana",
				metaUID:          "-1",
				metaGID:          "x",
				metaMtime:        "yesterday",
				metaOriginalSize: "-5",
			},
			wantCount: 5,
		},
		{
			name:      "an absent key is not a warning",
			meta:      map[string]string{"unrelated": "value"},
			wantCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := MetadataWarnings(tc.meta)
			if len(got) != tc.wantCount {
				t.Fatalf("MetadataWarnings returned %d warnings, want %d: %v", len(got), tc.wantCount, got)
			}
		})
	}
}

// Whatever Metadata writes, AttrFromMetadata must read back identically. If the pair disagrees, a
// chmod appears to work and is lost on the next stat — which is what "attributes could not persist"
// meant in v0.10.0.
func TestAttrMetadataRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []Attr{
		{Type: FileTypeRegular, Size: 0, Mode: 0o644},
		{Type: FileTypeRegular, Size: 1, Mode: 0o600, UID: 1000, GID: 1000},
		{Type: FileTypeRegular, Size: 1 << 30, Mode: 0o444, UID: 65534, GID: 65534},
		{Type: FileTypeRegular, Size: 42, Mode: 0o000},
		{Type: FileTypeRegular, Size: 42, Mode: 0o777, UID: 4294967295, GID: 4294967295},
		{
			Type:  FileTypeRegular,
			Size:  99,
			Mode:  0o640,
			UID:   1,
			GID:   2,
			Mtime: time.Date(2026, 7, 29, 12, 34, 56, 789012345, time.UTC),
		},
	}

	for i, want := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()

			meta := want.Metadata()
			if warns := MetadataWarnings(meta); len(warns) != 0 {
				t.Fatalf("Metadata() produced values it cannot read back: %v", warns)
			}

			// Size and checksum are the storage layer's to write, so supply them as it would.
			got := AttrFromMetadata(meta, want.Size, want.Mtime, want.ETag)

			if got.Mode != want.Mode {
				t.Errorf("Mode = %#o, want %#o", got.Mode, want.Mode)
			}
			if got.UID != want.UID {
				t.Errorf("UID = %d, want %d", got.UID, want.UID)
			}
			if got.GID != want.GID {
				t.Errorf("GID = %d, want %d", got.GID, want.GID)
			}
			if !got.Mtime.Equal(want.Mtime) {
				t.Errorf("Mtime = %v, want %v", got.Mtime, want.Mtime)
			}
			if got.Size != want.Size {
				t.Errorf("Size = %d, want %d", got.Size, want.Size)
			}
		})
	}
}

// Metadata must not claim ownership of size or checksum. Two writers for one integrity value is how
// the multipart checksum came to diverge from the single-part one.
func TestAttrMetadataOmitsStorageOwnedFields(t *testing.T) {
	t.Parallel()

	a := Attr{Type: FileTypeRegular, Size: 12345, Mode: 0o644, Checksum: "deadbeef"}
	meta := a.Metadata()

	for _, key := range []string{metaOriginalSize, metaChecksum} {
		if _, ok := lookupMeta(meta, key); ok {
			t.Errorf("Metadata() wrote %s, which the storage layer owns", key)
		}
	}
}

// Mtime must survive as an instant, not as a wall-clock reading in whatever zone the process happens
// to be in. A file written in one timezone and read in another must report the same time.
func TestAttrMetadataMtimeIsZoneIndependent(t *testing.T) {
	t.Parallel()

	loc := time.FixedZone("UTC-7", -7*3600)
	want := time.Date(2026, 7, 29, 5, 0, 0, 0, loc)

	a := Attr{Type: FileTypeRegular, Mode: 0o644, Mtime: want}
	got := AttrFromMetadata(a.Metadata(), 0, time.Time{}, "")

	if !got.Mtime.Equal(want) {
		t.Fatalf("Mtime = %v, want the same instant as %v", got.Mtime, want)
	}
}

func TestAttrMetadataOmitsZeroMtime(t *testing.T) {
	t.Parallel()

	a := Attr{Type: FileTypeRegular, Mode: 0o644}
	if v, ok := lookupMeta(a.Metadata(), metaMtime); ok {
		t.Fatalf("Metadata() wrote a zero mtime as %q; an absent key means 'use LastModified'", v)
	}
}

func TestDirAttr(t *testing.T) {
	t.Parallel()

	mtime := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	t.Run("explicit mode", func(t *testing.T) {
		t.Parallel()

		got := DirAttr(0o700, 1000, 1000, mtime)
		if got.Type != FileTypeDir {
			t.Errorf("Type = %v, want dir", got.Type)
		}
		if got.Mode != 0o700 {
			t.Errorf("Mode = %#o, want 0700", got.Mode)
		}
		if got.FileMode() != fs.ModeDir|0o700 {
			t.Errorf("FileMode() = %v, want %v", got.FileMode(), fs.ModeDir|0o700)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("Validate() = %v", err)
		}
	})

	t.Run("zero mode falls back to the traversable default", func(t *testing.T) {
		t.Parallel()

		got := DirAttr(0, 0, 0, mtime)
		if got.Mode != DefaultDirMode {
			t.Fatalf("Mode = %#o, want the default %#o — 0000 is what made v0.10.0 unusable",
				got.Mode, DefaultDirMode)
		}
	})

	t.Run("type bits in the mode are stripped", func(t *testing.T) {
		t.Parallel()

		got := DirAttr(fs.ModeDir|0o750, 0, 0, mtime)
		if got.Mode != 0o750 {
			t.Errorf("Mode = %#o, want 0750", got.Mode)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("Validate() = %v", err)
		}
	})

	t.Run("times all track mtime", func(t *testing.T) {
		t.Parallel()

		got := DirAttr(0o755, 0, 0, mtime)
		if !got.Atime.Equal(mtime) || !got.Ctime.Equal(mtime) {
			t.Errorf("Atime %v / Ctime %v, want both %v", got.Atime, got.Ctime, mtime)
		}
	})
}

func TestLookupMeta(t *testing.T) {
	t.Parallel()

	meta := map[string]string{
		"objectfs-mode": "644",
		"Objectfs-Uid":  "1000",
		"OBJECTFS-GID":  "1000",
	}

	tests := []struct {
		key     string
		want    string
		wantHit bool
	}{
		{key: "objectfs-mode", want: "644", wantHit: true},
		{key: "objectfs-uid", want: "1000", wantHit: true},
		{key: "objectfs-gid", want: "1000", wantHit: true},
		{key: "objectfs-mtime", wantHit: false},
		{key: "", wantHit: false},
	}

	for _, tc := range tests {
		got, ok := lookupMeta(meta, tc.key)
		if ok != tc.wantHit || got != tc.want {
			t.Errorf("lookupMeta(%q) = (%q, %v), want (%q, %v)", tc.key, got, ok, tc.want, tc.wantHit)
		}
	}
}

// FuzzAttrFromMetadata asserts the property that matters for arbitrary metadata: whatever an object
// carries — written by another tool, by an older ObjectFS, or by someone editing metadata by hand —
// the resulting Attr is valid and the file is usable. No input may produce a mode of 0000 by
// accident, a negative size, or a panic.
func FuzzAttrFromMetadata(f *testing.F) {
	f.Add("644", "1000", "1000", "2026-07-29T12:00:00Z", "1024")
	f.Add("", "", "", "", "")
	f.Add("banana", "-1", "99999999999999999999", "yesterday", "-5")
	f.Add("40755", "0", "0", "0000-01-01T00:00:00Z", "0")

	f.Fuzz(func(t *testing.T, mode, uid, gid, mtime, origSize string) {
		meta := map[string]string{
			metaMode:         mode,
			metaUID:          uid,
			metaGID:          gid,
			metaMtime:        mtime,
			metaOriginalSize: origSize,
		}

		a := AttrFromMetadata(meta, 4096, time.Unix(0, 0).UTC(), "etag")

		if err := a.Validate(); err != nil {
			t.Fatalf("invalid Attr from metadata %v: %v", meta, err)
		}
		if a.Type != FileTypeRegular {
			t.Fatalf("Type = %v, want a regular file", a.Type)
		}
		if a.Size < 0 {
			t.Fatalf("Size = %d", a.Size)
		}
		if a.Blocks() < 0 {
			t.Fatalf("Blocks() = %d", a.Blocks())
		}
		if a.Mode&^fs.ModePerm != 0 {
			t.Fatalf("Mode %#o carries non-permission bits", a.Mode)
		}
		if !a.Atime.Equal(a.Mtime) || !a.Ctime.Equal(a.Mtime) {
			t.Fatalf("Atime %v / Ctime %v do not track Mtime %v", a.Atime, a.Ctime, a.Mtime)
		}

		// Whatever came in, what this Attr writes back must be re-readable — otherwise one bad
		// inherited value would poison every subsequent write of the file.
		if warns := MetadataWarnings(a.Metadata()); len(warns) != 0 {
			t.Fatalf("Attr round-tripped from %v produces unreadable metadata: %v", meta, warns)
		}
	})
}
