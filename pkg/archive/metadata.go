package archive

import (
	"time"
)

// ArchiveFormat represents the supported archive formats.
type ArchiveFormat string

const (
	// FormatTarZstd represents TAR archives compressed with Zstandard.
	FormatTarZstd ArchiveFormat = "tar.zst"

	// FormatTarGzip represents TAR archives compressed with gzip.
	FormatTarGzip ArchiveFormat = "tar.gz"

	// FormatTarBzip2 represents TAR archives compressed with bzip2.
	FormatTarBzip2 ArchiveFormat = "tar.bz2"
)

// ArchiveMetadata contains metadata about an archive file.
type ArchiveMetadata struct {
	// Path is the S3 key of the archive.
	Path string `json:"path"`

	// Format is the archive format.
	Format ArchiveFormat `json:"format"`

	// Size is the compressed size of the archive in bytes.
	Size int64 `json:"size"`

	// UncompressedSize is the total size of all files in the archive.
	UncompressedSize int64 `json:"uncompressed_size"`

	// FileCount is the number of files in the archive.
	FileCount int `json:"file_count"`

	// Created is when the archive was created.
	Created time.Time `json:"created"`

	// LastModified is when the archive was last modified.
	LastModified time.Time `json:"last_modified"`

	// Checksum is the SHA256 checksum of the archive.
	Checksum string `json:"checksum"`

	// Index is the in-memory index of archive contents.
	Index *ArchiveIndex `json:"-"`
}

// ArchiveIndex contains the file listing and offsets within an archive.
type ArchiveIndex struct {
	// Files maps file paths within the archive to their entries.
	Files map[string]*ArchiveEntry `json:"files"`

	// TotalFiles is the total number of files indexed.
	TotalFiles int `json:"total_files"`

	// TotalSize is the total uncompressed size of all files.
	TotalSize int64 `json:"total_size"`
}

// ArchiveEntry represents a single file within an archive.
type ArchiveEntry struct {
	// Name is the file name within the archive.
	Name string `json:"name"`

	// Path is the full path within the archive.
	Path string `json:"path"`

	// Size is the uncompressed size of the file.
	Size int64 `json:"size"`

	// Mode is the file mode/permissions.
	Mode uint32 `json:"mode"`

	// ModTime is the file modification time.
	ModTime time.Time `json:"mod_time"`

	// IsDir indicates if this is a directory entry.
	IsDir bool `json:"is_dir"`

	// Offset is the byte offset of this entry in the archive.
	// This is used for efficient seeking.
	Offset int64 `json:"offset"`

	// CompressedSize is the size of this entry when compressed.
	CompressedSize int64 `json:"compressed_size,omitempty"`

	// Linkname is the target for symlinks.
	Linkname string `json:"linkname,omitempty"`
}

// IsArchive checks if a file path represents a known archive format.
func IsArchive(path string) (bool, ArchiveFormat) {
	if len(path) < 5 {
		return false, ""
	}

	// Check for .tar.zst
	if len(path) >= 8 && path[len(path)-8:] == ".tar.zst" {
		return true, FormatTarZstd
	}

	// Check for .tar.gz
	if len(path) >= 7 && path[len(path)-7:] == ".tar.gz" {
		return true, FormatTarGzip
	}

	// Check for .tgz
	if len(path) >= 4 && path[len(path)-4:] == ".tgz" {
		return true, FormatTarGzip
	}

	// Check for .tar.bz2
	if len(path) >= 8 && path[len(path)-8:] == ".tar.bz2" {
		return true, FormatTarBzip2
	}

	return false, ""
}

// NewArchiveIndex creates a new empty archive index.
func NewArchiveIndex() *ArchiveIndex {
	return &ArchiveIndex{
		Files: make(map[string]*ArchiveEntry),
	}
}

// AddEntry adds an entry to the archive index.
func (idx *ArchiveIndex) AddEntry(entry *ArchiveEntry) {
	idx.Files[entry.Path] = entry
	idx.TotalFiles++
	if !entry.IsDir {
		idx.TotalSize += entry.Size
	}
}

// GetEntry retrieves an entry from the index by path.
func (idx *ArchiveIndex) GetEntry(path string) (*ArchiveEntry, bool) {
	entry, ok := idx.Files[path]
	return entry, ok
}

// ListDirectory returns all entries within a directory path.
func (idx *ArchiveIndex) ListDirectory(dirPath string) []*ArchiveEntry {
	var entries []*ArchiveEntry

	// Ensure dirPath ends with /
	if dirPath != "" && dirPath[len(dirPath)-1] != '/' {
		dirPath += "/"
	}

	for path, entry := range idx.Files {
		// Check if this entry is directly under dirPath
		if len(path) > len(dirPath) && path[:len(dirPath)] == dirPath {
			// Check if there are no additional slashes (direct child)
			remaining := path[len(dirPath):]
			if !containsSlash(remaining) || (entry.IsDir && remaining[len(remaining)-1] == '/' && !containsSlashExceptLast(remaining)) {
				entries = append(entries, entry)
			}
		}
	}

	return entries
}

// containsSlash checks if a string contains a slash character.
func containsSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}

// containsSlashExceptLast checks if a string contains a slash except for the last character.
func containsSlashExceptLast(s string) bool {
	if len(s) <= 1 {
		return false
	}
	for i := 0; i < len(s)-1; i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}
