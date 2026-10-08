package fuse

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// TestNilCacheReadsThroughToTheBackend asserts a FileSystem built with no cache serves reads.
//
// NewFileSystem accepted a nil cache without complaint, and half of this package treated nil as "no
// cache": the metadata cache helpers check for it. The data path did not, so the first read through
// such a mount dereferenced nil in FileHandle.Read. go-fuse recovers a handler panic and answers EIO, so
// the symptom was not a crash but a mount where every read failed with an I/O error. The fuse_mount
// suite was the first caller to pass nil, and the first CI run of it was four EIOs and four nil-pointer
// traces (#543). The adapter always passes a cache, so no shipped mount was affected; the constructor's
// contract was the defect.
//
// No mount here, so this runs everywhere `go test` does. The reads go through FileHandle.Read exactly as
// the kernel's would: a small read, the same offset again (which a cache would have served), and a
// sequential run long enough to schedule read-ahead, whose worker also consults the cache.
func TestNilCacheReadsThroughToTheBackend(t *testing.T) {
	t.Parallel()

	srv := testaws.Start(t)
	backend := srv.Backend()

	writer, err := vfs.NewWriter(context.Background(), backend)
	if err != nil {
		t.Fatalf("vfs.NewWriter: %v", err)
	}

	mount, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	fs := NewFileSystem(mount, backend, nil, writer, nil, &Config{
		DefaultMode: 0o644,
		DefaultUID:  1000,
		DefaultGID:  1000,
	})

	const size = 1 << 20
	content := srv.SeedRandom("f.dat", size)

	fh := &FileHandle{
		fs:     fs,
		handle: 1,
		file:   &OpenFile{path: "f.dat", lastAccess: time.Now(), accessCount: 1},
	}

	read := func(off int64, n int) []byte {
		t.Helper()

		dest := make([]byte, n)

		result, errno := fh.Read(context.Background(), dest, off)
		if errno != 0 {
			t.Fatalf("Read(off=%d, n=%d) on a FileSystem with no cache: errno %v. A nil cache must mean "+
				"\"read from the backend\", not a nil dereference that go-fuse reports as EIO", off, n, errno)
		}

		got, status := result.Bytes(dest)
		if !status.Ok() {
			t.Fatalf("ReadResult.Bytes(off=%d): %v", off, status)
		}

		return got
	}

	if got := read(0, 512); !bytes.Equal(got, content[:512]) {
		t.Fatalf("first read returned %d bytes that do not match the object", len(got))
	}

	// The same offset again: with a cache this would be a hit, so it is the second place a nil cache is
	// consulted.
	if got := read(0, 512); !bytes.Equal(got, content[:512]) {
		t.Fatalf("repeat read returned %d bytes that do not match the object", len(got))
	}

	// Sequential reads across the object, so the read-ahead manager sees a pattern and schedules work.
	const chunk = 128 << 10
	for off := int64(0); off < size; off += chunk {
		if got := read(off, chunk); !bytes.Equal(got, content[off:off+chunk]) {
			t.Fatalf("sequential read at %d returned bytes that do not match the object", off)
		}
	}
}
