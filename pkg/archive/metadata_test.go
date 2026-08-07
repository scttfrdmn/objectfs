package archive

import (
	"testing"
	"time"
)

// The index API in this file has one producer, internal/archive.BuildIndexFromBytes, and no consumer
// at all: nothing reads an index back except that package's own tests. So these tests assert the
// contract directly rather than through a caller (issue #360). The distinction matters for one of
// them — see TestListDirectory_TrailingSlashEntriesAreNotProducedHere, which records that a whole
// branch of ListDirectory is unreachable from the only producer.

func TestIsArchive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		wantIs     bool
		wantFormat ArchiveFormat
	}{
		{name: "tar.zst", path: "data/set.tar.zst", wantIs: true, wantFormat: FormatTarZstd},
		{name: "tar.gz", path: "data/set.tar.gz", wantIs: true, wantFormat: FormatTarGzip},
		{name: "tgz", path: "set.tgz", wantIs: true, wantFormat: FormatTarGzip},
		{name: "tar.bz2", path: "set.tar.bz2", wantIs: true, wantFormat: FormatTarBzip2},

		{name: "plain tar is not a supported format", path: "set.tar", wantIs: false},
		{name: "bare zstd is not an archive", path: "set.zst", wantIs: false},
		{name: "no extension", path: "README", wantIs: false},
		{name: "empty", path: "", wantIs: false},

		// The length guards are the interesting part of this function: it indexes from the end
		// without checking that the prefix exists, so a name that is *only* the extension has to be
		// handled. ".tgz" is 4 characters and the function's first guard rejects anything under 5.
		{name: "extension with no basename, tgz", path: ".tgz", wantIs: false},
		{name: "extension with no basename, tar.gz", path: ".tar.gz", wantIs: true, wantFormat: FormatTarGzip},
		{name: "one character short of any match", path: "a.gz", wantIs: false},

		// Case matters. S3 keys are case-sensitive and so is this.
		{name: "uppercase extension does not match", path: "SET.TAR.GZ", wantIs: false},

		// A directory-ish key that merely contains an archive extension mid-path.
		{name: "extension in the middle of a key", path: "set.tar.gz/inner", wantIs: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotIs, gotFormat := IsArchive(tt.path)
			if gotIs != tt.wantIs {
				t.Errorf("IsArchive(%q) is-archive = %v, want %v", tt.path, gotIs, tt.wantIs)
			}
			if gotFormat != tt.wantFormat {
				t.Errorf("IsArchive(%q) format = %q, want %q", tt.path, gotFormat, tt.wantFormat)
			}
		})
	}
}

func TestArchiveIndex_AddAndGet(t *testing.T) {
	t.Parallel()

	idx := NewArchiveIndex()
	if idx.Files == nil {
		t.Fatal("NewArchiveIndex must return an index with a usable map, not a nil one")
	}
	if idx.TotalFiles != 0 || idx.TotalSize != 0 {
		t.Errorf("new index has TotalFiles=%d TotalSize=%d, want 0/0", idx.TotalFiles, idx.TotalSize)
	}

	modTime := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	file := &ArchiveEntry{Name: "a.txt", Path: "d/a.txt", Size: 100, Mode: 0o644, ModTime: modTime}
	dir := &ArchiveEntry{Name: "d", Path: "d", Size: 4096, IsDir: true}

	idx.AddEntry(file)
	idx.AddEntry(dir)

	// TotalFiles counts every entry including directories; TotalSize counts only non-directories.
	// That asymmetry is deliberate — a directory's tar-reported size is not content — and it is the
	// kind of thing that gets "cleaned up" into one rule, so it is asserted rather than assumed.
	if idx.TotalFiles != 2 {
		t.Errorf("TotalFiles = %d after two entries, want 2", idx.TotalFiles)
	}
	if idx.TotalSize != 100 {
		t.Errorf("TotalSize = %d, want 100: a directory's size must not be counted as content", idx.TotalSize)
	}

	got, ok := idx.GetEntry("d/a.txt")
	if !ok {
		t.Fatal("GetEntry(d/a.txt) reported absent")
	}
	if got != file {
		t.Error("GetEntry returned a different entry than the one added")
	}
	if !got.ModTime.Equal(modTime) {
		t.Errorf("ModTime = %v, want %v", got.ModTime, modTime)
	}

	if _, ok := idx.GetEntry("nope"); ok {
		t.Error("GetEntry reported a key that was never added as present")
	}
}

// TestArchiveIndex_AddEntryReplacesOnDuplicatePath pins what happens on a repeated path, because a
// tar can legally contain the same name twice and the index is a map. The counters are *not* rolled
// back for the replaced entry, so TotalFiles and TotalSize overcount. That is the current contract;
// this test exists so a change to it is visible rather than incidental.
func TestArchiveIndex_AddEntryReplacesOnDuplicatePath(t *testing.T) {
	t.Parallel()

	idx := NewArchiveIndex()
	idx.AddEntry(&ArchiveEntry{Path: "dup", Size: 10})
	idx.AddEntry(&ArchiveEntry{Path: "dup", Size: 20})

	got, ok := idx.GetEntry("dup")
	if !ok {
		t.Fatal("GetEntry(dup) reported absent")
	}
	if got.Size != 20 {
		t.Errorf("Size = %d, want 20: the later entry wins", got.Size)
	}

	if len(idx.Files) != 1 {
		t.Errorf("len(Files) = %d, want 1", len(idx.Files))
	}
	if idx.TotalFiles != 2 {
		t.Errorf("TotalFiles = %d; the counters do not roll back a replacement, so 2 is the "+
			"current behavior. If this is now 1, the contract changed on purpose and this test "+
			"should say so", idx.TotalFiles)
	}
	if idx.TotalSize != 30 {
		t.Errorf("TotalSize = %d; a replacement adds without subtracting, so 30 is the current "+
			"behavior", idx.TotalSize)
	}
}

func TestArchiveIndex_ListDirectory(t *testing.T) {
	t.Parallel()

	// Paths exactly as internal/archive.BuildIndexFromBytes writes them: path.Clean'd, so no entry
	// carries a trailing slash and there is no synthetic "." root.
	newIndex := func() *ArchiveIndex {
		idx := NewArchiveIndex()
		for _, e := range []*ArchiveEntry{
			{Path: "top.txt", Size: 5},
			{Path: "mydir", IsDir: true},
			{Path: "mydir/a.txt", Size: 10},
			{Path: "mydir/b.txt", Size: 20},
			{Path: "mydir/sub", IsDir: true},
			{Path: "mydir/sub/deep.txt", Size: 30},
		} {
			idx.AddEntry(e)
		}

		return idx
	}

	tests := []struct {
		name string
		dir  string
		want []string
		why  string
	}{
		{
			name: "root lists only top-level entries",
			dir:  "",
			want: []string{"mydir", "top.txt"},
			why:  "mydir/a.txt is two levels down and must not appear",
		},
		{
			name: "a directory lists its direct children",
			dir:  "mydir",
			want: []string{"mydir/a.txt", "mydir/b.txt", "mydir/sub"},
			why:  "mydir/sub/deep.txt is a grandchild and must not appear",
		},
		{
			name: "a trailing slash on the query is equivalent",
			dir:  "mydir/",
			want: []string{"mydir/a.txt", "mydir/b.txt", "mydir/sub"},
			why:  "the function appends the separator itself, so both spellings must agree",
		},
		{
			name: "a nested directory",
			dir:  "mydir/sub",
			want: []string{"mydir/sub/deep.txt"},
		},
		{
			name: "a directory that does not exist lists nothing",
			dir:  "absent",
			want: nil,
		},
		{
			name: "a file queried as a directory lists nothing",
			dir:  "top.txt",
			want: nil,
			why:  "top.txt/ is not a prefix of any path",
		},
		{
			name: "a prefix that is not a path component boundary lists nothing",
			dir:  "my",
			want: nil,
			why: "string-prefix matching would return mydir's children here; the separator the " +
				"function appends is what prevents it",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := newIndex().ListDirectory(tt.dir)
			assertPathSet(t, got, tt.want, tt.dir, tt.why)
		})
	}
}

// TestListDirectory_TrailingSlashEntriesAreNotProducedHere covers ListDirectory's second disjunct
// and records that it is unreachable from the only producer in the repository.
//
// The condition reads:
//
//	if !containsSlash(remaining) || (entry.IsDir && remaining[len(remaining)-1] == '/' && ...)
//
// The right-hand side needs an entry whose own Path ends in '/'. internal/archive.BuildIndexFromBytes
// runs every tar name through path.Clean, which strips exactly that — so no index built in this
// repository can reach it, and neither can containsSlashExceptLast, which only that branch calls.
//
// It is covered rather than deleted because ArchiveIndex is exported and a caller outside this
// repository may build an index by hand with tar's own directory convention, where a directory name
// does end in '/'. The test states which convention each half serves, so a future decision to drop
// the branch is a decision about the exported surface rather than a coverage cleanup.
func TestListDirectory_TrailingSlashEntriesAreNotProducedHere(t *testing.T) {
	t.Parallel()

	// tar's convention: directory entries keep their trailing slash.
	idx := NewArchiveIndex()
	for _, e := range []*ArchiveEntry{
		{Path: "d/", IsDir: true},
		{Path: "d/sub/", IsDir: true},
		{Path: "d/x.txt", Size: 1},
		{Path: "d/sub/deep.txt", Size: 2},
	} {
		idx.AddEntry(e)
	}

	// d/sub/ has a trailing slash and no other slash before it, so it is a direct child of d/ by
	// the second disjunct — the first would reject it, since "sub/" contains a slash.
	assertPathSet(t, idx.ListDirectory("d"), []string{"d/sub/", "d/x.txt"}, "d",
		"a trailing-slash directory entry is a direct child, which is what the second disjunct is for")

	// And the grandchildren are still excluded. Two shapes, because they fail the second disjunct at
	// different points: "sub/deep.txt" fails on its last character, while a grandchild *directory*
	// gets all the way to containsSlashExceptLast and is rejected there — the only thing that
	// distinguishes it from a child directory.
	idx.AddEntry(&ArchiveEntry{Path: "d/sub/deeper/", IsDir: true})
	assertPathSet(t, idx.ListDirectory("d"), []string{"d/sub/", "d/x.txt"}, "d",
		"d/sub/deeper/ is a grandchild directory; containsSlashExceptLast is what excludes it")
	assertPathSet(t, idx.ListDirectory("d/sub"), []string{"d/sub/deep.txt", "d/sub/deeper/"}, "d/sub", "")

	// A one-character remainder. containsSlashExceptLast has a len <= 1 guard, and this is the only
	// input that reaches it: the remainder must contain a slash and end in one, so it must be exactly
	// "/". Degenerate, but a tar can name an entry "/" and the guard is what keeps the loop bound
	// (len(s) - 1) from being 0 with an off-by-one read behind it.
	root := NewArchiveIndex()
	root.AddEntry(&ArchiveEntry{Path: "/", IsDir: true})
	assertPathSet(t, root.ListDirectory(""), []string{"/"}, "",
		"an entry named \"/\" is a direct child of the root, and reaching that answer runs "+
			"containsSlashExceptLast's length guard")
}

// assertPathSet compares an entry slice to an expected set of paths. ListDirectory iterates a map, so
// its order is not defined and must not be asserted.
func assertPathSet(t *testing.T, got []*ArchiveEntry, want []string, dir, why string) {
	t.Helper()

	gotPaths := make(map[string]bool, len(got))
	for _, e := range got {
		if e == nil {
			t.Fatalf("ListDirectory(%q) returned a nil entry", dir)
		}
		gotPaths[e.Path] = true
	}

	if len(gotPaths) != len(got) {
		t.Errorf("ListDirectory(%q) returned %d entries but only %d distinct paths",
			dir, len(got), len(gotPaths))
	}

	wantPaths := make(map[string]bool, len(want))
	for _, p := range want {
		wantPaths[p] = true
	}

	for p := range wantPaths {
		if !gotPaths[p] {
			t.Errorf("ListDirectory(%q) is missing %q", dir, p)
		}
	}
	for p := range gotPaths {
		if !wantPaths[p] {
			t.Errorf("ListDirectory(%q) unexpectedly returned %q", dir, p)
		}
	}

	if t.Failed() && why != "" {
		t.Logf("why: %s", why)
	}
}
