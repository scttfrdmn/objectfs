package archive

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"path"
	"time"

	"github.com/klauspost/compress/zstd"

	archivepkg "github.com/scttfrdmn/objectfs/pkg/archive"
	"github.com/scttfrdmn/objectfs/pkg/types"
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

		// hdr.Mode is an int64 read from a 12-byte field of an archive this process did not write —
		// octal, or GNU base-256 which can carry a negative — and ArchiveEntry.Mode is a uint32.
		// Truncating is the defect and not the warning: 0x1_0000_0644 narrows to 0644, so a crafted
		// archive gets to state one mode and have a different, entirely plausible one recorded, with
		// nothing downstream able to tell. Integrity first: an entry whose mode cannot be recorded as
		// what it says stops the index rather than being recorded as something else.
		//
		// The bound is representability, not the permission mask, and that is a deliberate choice
		// against the stricter one. internal/vfs rejects a mode carrying bits outside 0o7777, but a tar
		// mode field legitimately carries file-type bits: Apache Commons Compress writes 0100644 for a
		// regular file, so masking here would reject archives from a mainstream, non-hostile producer.
		// Inside [0, MaxUint32] the value is recorded exactly as the archive states it — this is a
		// metadata surface reporting what an archive says, and no production path reads the field to
		// make an access decision.
		if hdr.Mode < 0 || hdr.Mode > math.MaxUint32 {
			return nil, fmt.Errorf("BuildIndexFromBytes: archive %q: entry %q states mode %#o, which does "+
				"not fit the 32 bits a mode is recorded in, so it cannot be recorded as stated: %w",
				archiveKey, name, hdr.Mode, ErrMalformedEntry)
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
