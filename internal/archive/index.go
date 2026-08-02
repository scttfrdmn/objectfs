package archive

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"time"

	"github.com/klauspost/compress/zstd"

	archivepkg "github.com/objectfs/objectfs/pkg/archive"
	"github.com/objectfs/objectfs/pkg/types"
)

// BuildIndex downloads archiveKey from backend, walks its tar headers to build
// a complete ArchiveIndex, and supplements the result with real timestamps from
// a HeadObject call.  File content is not retained in memory.
//
// BuildIndex is the primary entry point for callers that hold a types.Backend.
// For testing or when the caller already has the archive bytes, use
// BuildIndexFromBytes.
func BuildIndex(ctx context.Context, backend types.Backend, archiveKey string) (*archivepkg.ArchiveMetadata, error) {
	isArchive, format := archivepkg.IsArchive(archiveKey)
	if !isArchive {
		return nil, fmt.Errorf("BuildIndex: not a known archive format: %q", archiveKey)
	}

	data, err := backend.GetObject(ctx, archiveKey, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("BuildIndex: downloading %q: %w", archiveKey, err)
	}

	meta, err := BuildIndexFromBytes(archiveKey, format, data)
	if err != nil {
		return nil, err
	}

	// Supplement with real S3 metadata (timestamps, ETag) when available.
	// HeadObject is a lightweight request; a failure here is non-fatal.
	if info, herr := backend.HeadObject(ctx, archiveKey); herr == nil && info != nil {
		meta.LastModified = info.LastModified
		if meta.Checksum == "" {
			meta.Checksum = info.ETag
		}
	}

	return meta, nil
}

// BuildIndexFromBytes parses an in-memory archive identified by archiveKey and
// returns ArchiveMetadata with a fully populated ArchiveIndex.
//
// format must be one of the known ArchiveFormat constants.  The caller is
// responsible for providing the correct format (use pkg/archive.IsArchive to
// detect it from the key).
//
// This function is useful in tests or when the caller already holds the raw
// archive bytes and does not want to re-download them.
func BuildIndexFromBytes(archiveKey string, format archivepkg.ArchiveFormat, data []byte) (*archivepkg.ArchiveMetadata, error) {
	tr, closeFn, err := openTar(format, data)
	if err != nil {
		return nil, fmt.Errorf("BuildIndexFromBytes: opening archive %q: %w", archiveKey, err)
	}
	if closeFn != nil {
		defer closeFn()
	}

	idx := archivepkg.NewArchiveIndex()
	var totalSize int64
	var decompressedOffset int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("BuildIndexFromBytes: parsing archive %q: %w", archiveKey, err)
		}

		name := path.Clean(hdr.Name)
		if name == "." {
			// Skip the synthetic root entry produced by some tar implementations.
			continue
		}

		isDir := hdr.Typeflag == tar.TypeDir
		entry := &archivepkg.ArchiveEntry{
			Name:    path.Base(name),
			Path:    name,
			Size:    hdr.Size,
			Mode:    uint32(hdr.Mode),
			ModTime: hdr.ModTime,
			IsDir:   isDir,
			Offset:  decompressedOffset,
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			entry.Linkname = hdr.Linkname
		}

		idx.AddEntry(entry)
		if !isDir {
			totalSize += hdr.Size
			decompressedOffset += hdr.Size
		}
	}

	return &archivepkg.ArchiveMetadata{
		Path:             archiveKey,
		Format:           format,
		Size:             int64(len(data)),
		UncompressedSize: totalSize,
		FileCount:        idx.TotalFiles,
		LastModified:     time.Now(),
		Index:            idx,
	}, nil
}

// openTar wraps raw archive bytes in the appropriate decompressor and returns
// a *tar.Reader plus an optional close function.  It is shared by
// BuildIndexFromBytes and VFS.extractFile.
func openTar(format archivepkg.ArchiveFormat, data []byte) (*tar.Reader, func(), error) {
	r := bytes.NewReader(data)
	switch format {
	case archivepkg.FormatTarZstd:
		d, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return tar.NewReader(d), d.Close, nil

	case archivepkg.FormatTarGzip:
		g, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return tar.NewReader(g), func() { _ = g.Close() }, nil

	case archivepkg.FormatTarBzip2:
		return tar.NewReader(bzip2.NewReader(r)), nil, nil

	default:
		return nil, nil, fmt.Errorf("openTar: unsupported archive format: %q", format)
	}
}
