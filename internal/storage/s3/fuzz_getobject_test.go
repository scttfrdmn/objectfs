package s3_test

import (
	"context"
	"testing"

	"github.com/objectfs/objectfs/internal/testaws"
)

// FuzzGetObjectRange drives the offset/size domain through the real GetObject against a live S3 endpoint,
// and asserts the bytes returned are the ones the object actually holds at that offset.
//
// This is the half FuzzSliceRange cannot cover. sliceRange is one function on one slice; GetObject turns
// an (offset, size) pair into a Range header, sends it, and reassembles what comes back — and the audit's
// read-path defects were in that translation rather than in the arithmetic. C3's call sites reach
// GetObject, not sliceRange. A ranged read that returns plausible bytes from the wrong offset is silent
// corruption, and only comparison against known content detects it.
//
// It lives in the external test package because internal/testaws imports the s3 package: an in-package
// test importing the harness is an import cycle. That constraint is useful here — the expectation cannot
// call sliceRange, so it has to be written independently, which is what makes this a check on GetObject
// rather than a restatement of it.
func FuzzGetObjectRange(f *testing.F) {
	const (
		key    = "fuzz/range.bin"
		length = 8192
	)

	// One object for the whole run. It never changes, and a PUT per iteration would turn a read-path
	// fuzzer into a write-path benchmark.
	backend, err := testaws.Shared(f).Backend(context.Background())
	if err != nil {
		f.Fatalf("testaws: backend: %v", err)
	}
	f.Cleanup(func() { _ = backend.Close() })

	content := make([]byte, length)
	for i := range content {
		// Position-dependent, so bytes from the wrong offset are detected rather than matching by luck.
		content[i] = byte(i*31 + i>>8)
	}

	if err := backend.PutObject(context.Background(), key, content, nil); err != nil {
		f.Fatalf("seed the object under test: %v", err)
	}

	// C3's shape first: a negative size, which is the documented "to the end" form.
	f.Add(int64(0), int64(-1))
	f.Add(int64(100), int64(-1))
	f.Add(int64(0), int64(0))

	// Boundaries: the last byte, exactly the end, past the end, and a range longer than the object.
	f.Add(int64(0), int64(4096))
	f.Add(int64(4096), int64(4096))
	f.Add(int64(length-1), int64(1))
	f.Add(int64(length), int64(1))
	f.Add(int64(0), int64(length*2))
	f.Add(int64(-1), int64(10))

	f.Fuzz(func(t *testing.T, offset, size int64) {
		got, err := backend.GetObject(context.Background(), key, offset, size)
		if err != nil {
			// An error is a legitimate answer to a nonsensical range. What is not legitimate is a panic
			// — which the fuzzer catches whether or not this returns — or bytes from the wrong place,
			// which cannot happen on a path that errored.
			return
		}

		// The expectation, in terms of the object's contents and nothing borrowed from the read path.
		start := min(max(offset, 0), length)

		end := int64(length)
		if size > 0 && size < length-start {
			end = start + size
		}

		want := content[start:end]

		if len(got) != len(want) {
			t.Fatalf("GetObject(offset %d, size %d) returned %d bytes, want %d",
				offset, size, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("GetObject(offset %d, size %d) byte %d is %#02x, want %#02x — the length is "+
					"right, so the data came from the wrong offset",
					offset, size, i, got[i], want[i])
			}
		}
	})
}
