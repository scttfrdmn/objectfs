//go:build !integration

package adapter

// BenchmarkAdapter_* benchmarks test the adapter package helpers and
// configuration plumbing that can be exercised without a live S3 backend.
// They do not require AWS credentials and run unconditionally in CI.
//
// To run:
//
//	go test -bench=. -benchtime=3s ./internal/adapter/...

import (
	"fmt"
	"testing"
)

// ─── parseSize benchmarks ─────────────────────────────────────────────────────

// BenchmarkParseSize measures the throughput of the human-readable size parser
// used when converting config values (e.g. "2GB") into byte counts.
func BenchmarkParseSize_GB(b *testing.B) {
	for range b.N {
		_ = parseSize("2GB")
	}
}

func BenchmarkParseSize_MB(b *testing.B) {
	for range b.N {
		_ = parseSize("512MB")
	}
}

func BenchmarkParseSize_KB(b *testing.B) {
	for range b.N {
		_ = parseSize("128KB")
	}
}

// BenchmarkParseSize_Parallel measures parseSize under concurrent load.
func BenchmarkParseSize_Parallel(b *testing.B) {
	sizes := []string{"1GB", "512MB", "256KB", "4096B", "2GB"}
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = parseSize(sizes[i%len(sizes)])
			i++
		}
	})
}

// ─── validateStorageURI benchmarks ────────────────────────────────────────────

// BenchmarkValidateStorageURI measures URI validation for valid s3:// URIs.
func BenchmarkValidateStorageURI_Valid(b *testing.B) {
	for range b.N {
		_ = validateStorageURI("s3://my-bucket")
	}
}

// BenchmarkValidateStorageURI_Invalid measures validation for invalid URIs
// (expected to return an error quickly).
func BenchmarkValidateStorageURI_Invalid(b *testing.B) {
	for i := range b.N {
		_ = validateStorageURI(fmt.Sprintf("gs://bucket-%d", i%10))
	}
}

// BenchmarkValidateStorageURI_Parallel exercises concurrent URI validation.
func BenchmarkValidateStorageURI_Parallel(b *testing.B) {
	uris := []string{
		"s3://bucket-a",
		"s3://bucket-b/prefix/",
		"s3://my-corp-data",
	}
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = validateStorageURI(uris[i%len(uris)])
			i++
		}
	})
}
