package archive

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

	archivepkg "github.com/objectfs/objectfs/pkg/archive"
	"github.com/objectfs/objectfs/pkg/types"
)

// VFS provides a virtual filesystem view into archive contents stored as S3
// objects.  It indexes archives on first access and caches the index for all
// subsequent Stat/ReadDir calls.  Individual file content is extracted and
// cached on first ReadFile access.
//
// All exported methods are safe for concurrent use.
type VFS struct {
	backend types.Backend

	// indexMu protects indexCache.
	indexMu    sync.RWMutex
	indexCache map[string]*archivepkg.ArchiveMetadata // archiveKey → metadata

	// contentMu protects contentCache.
	contentMu    sync.RWMutex
	contentCache map[string][]byte // "archiveKey\x00innerPath" → file bytes
}

// NewVFS creates a new archive VFS backed by the given storage backend.
func NewVFS(backend types.Backend) *VFS {
	return &VFS{
		backend:      backend,
		indexCache:   make(map[string]*archivepkg.ArchiveMetadata),
		contentCache: make(map[string][]byte),
	}
}

// Stat returns POSIX-style metadata for the entry at innerPath within the
// archive identified by archiveKey.
//
// An empty innerPath refers to the archive root (always a virtual directory).
// A non-empty innerPath must name a file or directory within the archive.
//
// Returns an error wrapping ErrNotFound when the entry does not exist.
func (v *VFS) Stat(ctx context.Context, archiveKey, innerPath string) (*types.ObjectInfo, error) {
	meta, err := v.getIndex(ctx, archiveKey)
	if err != nil {
		return nil, err
	}

	// Archive root — always a virtual directory.
	if innerPath == "" {
		return &types.ObjectInfo{
			Key:          archiveKey,
			Size:         meta.UncompressedSize,
			LastModified: meta.LastModified,
			ContentType:  "application/x-directory",
		}, nil
	}

	clean := path.Clean(innerPath)

	// Try exact match first (covers both files and explicit dir entries).
	if entry, ok := meta.Index.GetEntry(clean); ok {
		return entryToObjectInfo(archiveKey, entry), nil
	}
	// Also try with trailing slash for archives that index dirs as "dir/".
	if entry, ok := meta.Index.GetEntry(clean + "/"); ok {
		return entryToObjectInfo(archiveKey, entry), nil
	}

	// Check for a virtual (implicit) directory: any entry whose path starts
	// with clean+"/" counts as evidence that the directory exists.
	prefix := clean + "/"
	for p := range meta.Index.Files {
		if strings.HasPrefix(p, prefix) {
			return &types.ObjectInfo{
				Key:          Join(archiveKey, clean),
				Size:         0,
				LastModified: meta.LastModified,
				ContentType:  "application/x-directory",
			}, nil
		}
	}

	return nil, fmt.Errorf("stat %q in %q: %w", innerPath, archiveKey, ErrNotFound)
}

// ReadDir lists the direct children of the virtual directory at innerPath
// within the archive identified by archiveKey.  Use an empty innerPath for
// the archive root.
//
// The returned entries are deduplicated: files that share the same immediate
// parent directory component produce a single synthetic directory entry.
func (v *VFS) ReadDir(ctx context.Context, archiveKey, innerPath string) ([]*archivepkg.ArchiveEntry, error) {
	meta, err := v.getIndex(ctx, archiveKey)
	if err != nil {
		return nil, err
	}

	prefix := ""
	if innerPath != "" {
		prefix = path.Clean(innerPath) + "/"
	}

	seen := make(map[string]struct{})
	var entries []*archivepkg.ArchiveEntry

	for entryPath, entry := range meta.Index.Files {
		// Determine the relative portion under prefix.
		var rel string
		if prefix == "" {
			rel = entryPath
		} else if strings.HasPrefix(entryPath, prefix) {
			rel = entryPath[len(prefix):]
		} else {
			continue
		}

		// Skip empty remainder (the directory entry itself).
		rel = strings.TrimSuffix(rel, "/")
		if rel == "" {
			continue
		}

		// Split on the first "/" to get the immediate child name.
		before, _, ok := strings.Cut(rel, "/")
		if !ok {
			// Direct child — include as-is.
			if _, ok := seen[rel]; !ok {
				seen[rel] = struct{}{}
				entries = append(entries, entry)
			}
		} else {
			// Deeper descendant — synthesize a virtual directory entry.
			childName := before
			if _, ok := seen[childName]; !ok {
				seen[childName] = struct{}{}
				entries = append(entries, &archivepkg.ArchiveEntry{
					Name:    childName,
					Path:    prefix + childName,
					IsDir:   true,
					Mode:    0755,
					ModTime: meta.LastModified,
				})
			}
		}
	}

	return entries, nil
}

// ReadFile reads size bytes starting at offset from the file at innerPath
// within the archive identified by archiveKey.
//
// The complete decompressed content of each (archiveKey, innerPath) pair is
// cached on first access; subsequent reads are served from the cache without
// network I/O.
//
// Passing size=0 returns all content from offset to EOF.
func (v *VFS) ReadFile(ctx context.Context, archiveKey, innerPath string, offset, size int64) ([]byte, error) {
	if innerPath == "" {
		return nil, fmt.Errorf("ReadFile: innerPath must not be empty")
	}

	cacheKey := archiveKey + "\x00" + path.Clean(innerPath)

	// Fast path — already cached.
	v.contentMu.RLock()
	cached, ok := v.contentCache[cacheKey]
	v.contentMu.RUnlock()
	if !ok {
		content, err := v.extractFile(ctx, archiveKey, innerPath)
		if err != nil {
			return nil, err
		}
		v.contentMu.Lock()
		v.contentCache[cacheKey] = content
		v.contentMu.Unlock()
		cached = content
	}

	return byteSlice(cached, offset, size), nil
}

// Invalidate removes cached index and file content for archiveKey, forcing a
// full reload on the next access.  Use this when the archive object in S3 may
// have been updated.
func (v *VFS) Invalidate(archiveKey string) {
	v.indexMu.Lock()
	delete(v.indexCache, archiveKey)
	v.indexMu.Unlock()

	prefix := archiveKey + "\x00"
	v.contentMu.Lock()
	for k := range v.contentCache {
		if strings.HasPrefix(k, prefix) {
			delete(v.contentCache, k)
		}
	}
	v.contentMu.Unlock()
}

// ── internal helpers ──────────────────────────────────────────────────────────

// getIndex returns the cached ArchiveMetadata for archiveKey, loading it if
// necessary.  It delegates index construction to BuildIndex in index.go.
func (v *VFS) getIndex(ctx context.Context, archiveKey string) (*archivepkg.ArchiveMetadata, error) {
	v.indexMu.RLock()
	meta, ok := v.indexCache[archiveKey]
	v.indexMu.RUnlock()
	if ok {
		return meta, nil
	}

	meta, err := BuildIndex(ctx, v.backend, archiveKey)
	if err != nil {
		return nil, err
	}

	v.indexMu.Lock()
	v.indexCache[archiveKey] = meta
	v.indexMu.Unlock()

	return meta, nil
}

// extractFile streams through the decompressed archive to extract a single
// file's content.  It performs a linear scan, which is efficient for
// sequential access patterns but proportional to entry position for random
// access.  Content caching in ReadFile amortizes repeated accesses.
func (v *VFS) extractFile(ctx context.Context, archiveKey, innerPath string) ([]byte, error) {
	_, format := archivepkg.IsArchive(archiveKey)
	data, err := v.backend.GetObject(ctx, archiveKey, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("reading archive %q: %w", archiveKey, err)
	}

	tr, closeFn, err := openTar(format, data)
	if err != nil {
		return nil, fmt.Errorf("opening archive %q: %w", archiveKey, err)
	}
	if closeFn != nil {
		defer closeFn()
	}

	target := path.Clean(innerPath)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("file %q not found in archive %q: %w", innerPath, archiveKey, ErrNotFound)
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive %q: %w", archiveKey, err)
		}
		if path.Clean(hdr.Name) == target {
			content, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("reading %q from archive %q: %w", innerPath, archiveKey, err)
			}
			return content, nil
		}
	}
}

// entryToObjectInfo converts an ArchiveEntry to a types.ObjectInfo.
func entryToObjectInfo(archiveKey string, entry *archivepkg.ArchiveEntry) *types.ObjectInfo {
	ct := "application/octet-stream"
	if entry.IsDir {
		ct = "application/x-directory"
	}
	return &types.ObjectInfo{
		Key:          Join(archiveKey, entry.Path),
		Size:         entry.Size,
		LastModified: entry.ModTime,
		ContentType:  ct,
	}
}

// byteSlice returns data[offset : offset+size].  A size of 0 means "to EOF".
// Both offset and size are clamped to the data bounds.
func byteSlice(data []byte, offset, size int64) []byte {
	if offset < 0 {
		offset = 0
	}
	if offset >= int64(len(data)) {
		return nil
	}
	data = data[offset:]
	if size > 0 && size < int64(len(data)) {
		data = data[:size]
	}
	return data
}
