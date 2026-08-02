package s3

import (
	"io"
	"log/slog"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

// discardLogger returns a logger that writes nowhere, for tests that exercise
// warning paths without polluting test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestOriginalSize covers the metadata lookup that keeps HeadObject from
// reporting a compressed ContentLength as the file size. Reporting the
// compressed size makes the kernel truncate every read at that offset, so the
// fallback behavior here is load-bearing for read correctness.
func TestOriginalSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		metadata      map[string]string
		contentLength int64
		want          int64
	}{
		{
			name:          "compressed object reports uncompressed size",
			metadata:      map[string]string{metaOriginalSize: "10485760"},
			contentLength: 4096, // compressed
			want:          10485760,
		},
		{
			name:          "uncompressed object falls back to ContentLength",
			metadata:      map[string]string{metaChecksum: "abc123"},
			contentLength: 2048,
			want:          2048,
		},
		{
			name:          "legacy object without the key falls back",
			metadata:      map[string]string{},
			contentLength: 512,
			want:          512,
		},
		{
			name:          "nil metadata falls back",
			metadata:      nil,
			contentLength: 777,
			want:          777,
		},
		{
			name:          "malformed value falls back rather than reporting zero",
			metadata:      map[string]string{metaOriginalSize: "not-a-number"},
			contentLength: 1024,
			want:          1024,
		},
		{
			name:          "negative value is rejected",
			metadata:      map[string]string{metaOriginalSize: "-1"},
			contentLength: 1024,
			want:          1024,
		},
		{
			name:          "empty value falls back",
			metadata:      map[string]string{metaOriginalSize: ""},
			contentLength: 1024,
			want:          1024,
		},
		{
			name:          "zero-length original size is honored",
			metadata:      map[string]string{metaOriginalSize: "0"},
			contentLength: 0,
			want:          0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := originalSize(tt.metadata, tt.contentLength, "test/key", discardLogger())
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestOriginalSize_RoundTripsLargeValues guards the ParseInt width: sizes beyond
// int32 must survive, since the whole point is large-file correctness.
func TestOriginalSize_RoundTripsLargeValues(t *testing.T) {
	t.Parallel()

	const large = int64(8) << 30 // 8 GiB
	metadata := map[string]string{metaOriginalSize: strconv.FormatInt(large, 10)}

	got := originalSize(metadata, 1024, "big/object", discardLogger())
	assert.Equal(t, large, got)
}

// TestMetadataKeys pins the on-the-wire metadata key names. Changing them
// silently orphans every object already written, so this is a deliberate
// tripwire rather than a tautology.
func TestMetadataKeys(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "objectfs-sha256", metaChecksum)
	assert.Equal(t, "objectfs-original-size", metaOriginalSize)
}
