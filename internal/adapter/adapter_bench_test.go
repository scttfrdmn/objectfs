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

// The four BenchmarkParseSize_* benchmarks that were here are gone with the function they measured.
// adapter.parseSize is now utils.ParseBytes, and there is nothing to benchmark: every call happens
// once, during Start, on a string from a config file. Measuring it under b.RunParallel described a
// load that does not exist.

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
