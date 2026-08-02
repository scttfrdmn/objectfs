package archive

import (
	"context"
	"fmt"

	archivepkg "github.com/scttfrdmn/objectfs/pkg/archive"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// Detect scans a slice of S3 object metadata and returns lightweight
// ArchiveMetadata for every object whose key ends with a known archive
// extension (.tar.zst, .tar.gz, .tgz, .tar.bz2).
//
// The returned entries have Path, Format, Size, LastModified, and Checksum
// populated from the ObjectInfo; the ArchiveIndex field is nil until
// BuildIndex is called.
func Detect(objects []types.ObjectInfo) []archivepkg.ArchiveMetadata {
	var found []archivepkg.ArchiveMetadata
	for _, obj := range objects {
		ok, format := archivepkg.IsArchive(obj.Key)
		if !ok {
			continue
		}
		found = append(found, archivepkg.ArchiveMetadata{
			Path:         obj.Key,
			Format:       format,
			Size:         obj.Size,
			LastModified: obj.LastModified,
			Checksum:     obj.ETag,
		})
	}
	return found
}

// DetectKeys returns the S3 keys from a listing that match a known archive
// extension, preserving the order of objects.
func DetectKeys(objects []types.ObjectInfo) []string {
	var keys []string
	for _, obj := range objects {
		if ok, _ := archivepkg.IsArchive(obj.Key); ok {
			keys = append(keys, obj.Key)
		}
	}
	return keys
}

// DetectInPrefix lists objects under prefix in backend (up to limit results)
// and returns lightweight ArchiveMetadata for any archives found.
//
// A limit of 0 uses a sensible default of 1000.
func DetectInPrefix(ctx context.Context, backend types.Backend, prefix string, limit int) ([]archivepkg.ArchiveMetadata, error) {
	if limit <= 0 {
		limit = 1000
	}
	objects, err := backend.ListObjects(ctx, prefix, limit)
	if err != nil {
		return nil, fmt.Errorf("DetectInPrefix: listing %q: %w", prefix, err)
	}
	return Detect(objects), nil
}

// IsArchiveKey reports whether key ends with a known archive extension and
// returns the detected ArchiveFormat.  It is a convenience wrapper around
// pkg/archive.IsArchive for callers that only have a key string.
func IsArchiveKey(key string) (bool, archivepkg.ArchiveFormat) {
	return archivepkg.IsArchive(key)
}
