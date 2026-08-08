package tests

import (
	"context"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// MockBackend implements a simple in-memory backend for testing.
//
// This file holds the fixture and nothing else. It used to be fuse_test.go, whose ten subtests
// asserted against this map rather than against internal/fuse: each built a fuse.FileSystem and
// then wrote `_ = filesystem // Use filesystem to avoid unused variable`, so the only subtest that
// touched it compared GetStats().Reads before and after a loop of direct backend.GetObject calls —
// 0 >= 0, which holds for any implementation of internal/fuse including one whose Read panics. A
// test whose subject is the mock it built is worse than no test, because the file's existence is
// what makes the gap invisible; see #378. The coverage those subtests appeared to provide lives in
// internal/fuse/read_path_test.go (the real read path, through the real node methods),
// internal/vfs/writer_test.go (write coalescing and offset writes), internal/cache and
// internal/metrics.
//
// Prefer internal/testaws to this mock for anything that is a claim about S3. A mock on the far
// side of a seam agrees with its caller by construction, which is why PutObjectIf below refuses
// rather than answering.
type MockBackend struct {
	mu      sync.RWMutex
	objects map[string][]byte

	// meta holds each object's user metadata. Stored rather than discarded because that is where POSIX
	// mode and ownership live: a backend that accepted attributes and dropped them would let an
	// attribute test pass while nothing was persisted.
	meta map[string]map[string]string
}

func NewMockBackend() *MockBackend {
	return &MockBackend{
		objects: make(map[string][]byte),
		meta:    make(map[string]map[string]string),
	}
}

func (b *MockBackend) GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	data, exists := b.objects[key]
	if !exists {
		return nil, os.ErrNotExist
	}

	if offset >= int64(len(data)) {
		return []byte{}, nil
	}

	end := offset + size
	if size == 0 || end > int64(len(data)) {
		end = int64(len(data))
	}

	return data[offset:end], nil
}

func (b *MockBackend) PutObject(ctx context.Context, key string, data []byte, meta map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.objects[key] = make([]byte, len(data))
	copy(b.objects[key], data)
	// Replaced wholesale, as S3's PutObject does.
	b.meta[key] = copyMeta(meta)
	return nil
}

// PutObjectIf refuses rather than pretending. This mock could evaluate a precondition against its own
// map, but an in-process map is not what a precondition is a claim about — the point of one is that a
// *remote* store arbitrates between writers this process cannot see. Implementing it here would produce
// a CAS that passes its tests and cannot exclude anything, which is the seam-agreement failure the
// project uses internal/testaws to avoid. Conditional writes are tested against a real S3 endpoint.
func (b *MockBackend) PutObjectIf(ctx context.Context, key string, data []byte, meta map[string]string,
	cond types.Precondition,
) (string, error) {
	return "", types.ErrNotSupported
}

func (b *MockBackend) SetObjectMetadata(ctx context.Context, key string, meta map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.objects[key]; !ok {
		return os.ErrNotExist
	}
	// The bytes are untouched. An implementation that rewrote them would be the defect
	// vfs.Flusher asserts against by rechecking the size after an attribute write.
	b.meta[key] = copyMeta(meta)
	return nil
}

// copyMeta returns a copy of m, so a caller reusing its map cannot mutate what the backend stored.
func copyMeta(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}

func (b *MockBackend) HeadObject(ctx context.Context, key string) (*types.ObjectInfo, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	data, exists := b.objects[key]
	if !exists {
		return nil, os.ErrNotExist
	}

	return &types.ObjectInfo{
		Key:          key,
		Size:         int64(len(data)),
		LastModified: time.Now(),
		Metadata:     copyMeta(b.meta[key]),
	}, nil
}

func (b *MockBackend) ListObjects(ctx context.Context, prefix string, maxKeys int) ([]types.ObjectInfo, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var objects []types.ObjectInfo
	count := 0

	for key, data := range b.objects {
		if (prefix == "" || strings.HasPrefix(key, prefix)) && count < maxKeys {
			objects = append(objects, types.ObjectInfo{
				Key:          key,
				Size:         int64(len(data)),
				LastModified: time.Now(),
			})
			count++
		}
	}

	return objects, nil
}

// CopyObject copies the bytes and the metadata together, which is the property rename depends on: the
// stored mode, ownership, and content encoding have to arrive at the destination or the renamed file
// comes back with a different mode, or — if it was compressed — unreadable.
func (b *MockBackend) CopyObject(ctx context.Context, src, dst string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	data, exists := b.objects[src]
	if !exists {
		return os.ErrNotExist
	}

	b.objects[dst] = append([]byte(nil), data...)
	b.meta[dst] = copyMeta(b.meta[src])
	return nil
}

func (b *MockBackend) DeleteObject(ctx context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.objects, key)
	return nil
}

func (b *MockBackend) GetObjects(ctx context.Context, keys []string) (map[string][]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make(map[string][]byte)
	for _, key := range keys {
		if data, exists := b.objects[key]; exists {
			result[key] = make([]byte, len(data))
			copy(result[key], data)
		}
	}

	return result, nil
}

func (b *MockBackend) PutObjects(ctx context.Context, objects map[string][]byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for key, data := range objects {
		b.objects[key] = make([]byte, len(data))
		copy(b.objects[key], data)
	}

	return nil
}

func (b *MockBackend) HealthCheck(ctx context.Context) error {
	return nil
}

func (b *MockBackend) Close() error {
	return nil
}
