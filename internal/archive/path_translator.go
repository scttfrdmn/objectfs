package archive

import (
	"path"
	"strings"

	archivepkg "github.com/objectfs/objectfs/pkg/archive"
)

// ParsedPath represents a filesystem path that may pass through an archive
// file boundary.  When IsArchivePath is true the path component before the
// boundary is an archive object in S3 and InnerPath is the entry inside it.
type ParsedPath struct {
	// RawPath is the original path supplied by the caller.
	RawPath string

	// IsArchivePath is true when the path crosses a known archive extension.
	IsArchivePath bool

	// ArchiveKey is the FUSE-relative S3 key of the archive, e.g.
	// "datasets/data.tar.zst".  Empty when IsArchivePath is false.
	ArchiveKey string

	// ArchiveFormat is the detected archive format (tar.zst, tar.gz, tar.bz2).
	ArchiveFormat archivepkg.ArchiveFormat

	// InnerPath is the path of the entry within the archive, e.g.
	// "subdir/file.txt".  An empty string refers to the archive root.
	InnerPath string
}

// Translate splits a FUSE-relative path at the first path segment (prefix)
// that ends with a recognised archive extension.  Path segments are scanned
// left-to-right so that the outermost archive is always selected.
//
// Examples:
//
//	Translate("data.tar.zst")                 → IsArchivePath:true, ArchiveKey:"data.tar.zst",          InnerPath:""
//	Translate("data.tar.zst/dir/file.txt")    → IsArchivePath:true, ArchiveKey:"data.tar.zst",          InnerPath:"dir/file.txt"
//	Translate("dir/arch.tar.gz/sub/f.txt")    → IsArchivePath:true, ArchiveKey:"dir/arch.tar.gz",       InnerPath:"sub/f.txt"
//	Translate("regular/file.txt")             → IsArchivePath:false
//	Translate("")                             → IsArchivePath:false
func Translate(fuseRelPath string) ParsedPath {
	if fuseRelPath == "" {
		return ParsedPath{RawPath: fuseRelPath}
	}

	clean := path.Clean(fuseRelPath)
	parts := strings.Split(clean, "/")

	for i := range parts {
		candidate := strings.Join(parts[:i+1], "/")
		ok, fmt := archivepkg.IsArchive(candidate)
		if ok {
			inner := strings.Join(parts[i+1:], "/")
			return ParsedPath{
				RawPath:       fuseRelPath,
				IsArchivePath: true,
				ArchiveKey:    candidate,
				ArchiveFormat: fmt,
				InnerPath:     inner,
			}
		}
	}

	return ParsedPath{RawPath: fuseRelPath}
}

// Join reconstructs a full FUSE path from an archive key and an inner path.
// If innerPath is empty, the archive key itself is returned unchanged.
func Join(archiveKey, innerPath string) string {
	if innerPath == "" {
		return archiveKey
	}
	return archiveKey + "/" + innerPath
}
