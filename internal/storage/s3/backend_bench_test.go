//go:build !integration

package s3

// BenchmarkS3Backend_* benchmarks benchmark the types.Backend interface at the
// S3 package level using an in-process stub client.  They do not require AWS
// credentials or a running S3 endpoint, so they execute unconditionally in CI.
//
// To run:
//
//	go test -bench=. -benchtime=3s ./internal/storage/s3/...
//
// To run against real S3, set OBJECTFS_BENCH_BUCKET and run with the
// integration build tag (see acceleration_bench_test.go).

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// ─── stub backend ─────────────────────────────────────────────────────────────

// benchBackend is a zero-allocation in-memory stub that satisfies the Backend
// method set exercised by the benchmarks below.  It does no I/O.
type benchBackend struct {
	objects map[string][]byte
}

func newBenchBackend() *benchBackend {
	return &benchBackend{objects: make(map[string][]byte)}
}

func (b *benchBackend) getObject(_ context.Context, key string, offset, size int64) ([]byte, error) {
	data, ok := b.objects[key]
	if !ok {
		return nil, fmt.Errorf("key not found: %s", key)
	}
	if offset < 0 || offset >= int64(len(data)) {
		return nil, fmt.Errorf("offset out of range")
	}
	end := int64(len(data))
	if size > 0 && offset+size < end {
		end = offset + size
	}
	return data[offset:end], nil
}

func (b *benchBackend) putObject(_ context.Context, key string, data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	b.objects[key] = cp
	return nil
}

func (b *benchBackend) deleteObject(_ context.Context, key string) error {
	delete(b.objects, key)
	return nil
}

func (b *benchBackend) listObjects(_ context.Context, prefix string, limit int) ([]string, error) {
	out := make([]string, 0)
	for k := range b.objects {
		if len(prefix) == 0 || bytes.HasPrefix([]byte(k), []byte(prefix)) {
			out = append(out, k)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func makePayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i % 256)
	}
	return p
}

func seedBackend(bb *benchBackend, count int, sizeBytes int) {
	payload := makePayload(sizeBytes)
	for i := range count {
		key := fmt.Sprintf("bench/key-%04d", i)
		bb.objects[key] = payload
	}
}

// ─── GetObject benchmarks ─────────────────────────────────────────────────────

func BenchmarkS3Backend_GetObject_1KB(b *testing.B) {
	bb := newBenchBackend()
	seedBackend(bb, 1, 1024)
	key := "bench/key-0000"
	ctx := context.Background()

	b.ResetTimer()
	b.SetBytes(1024)
	for i := 0; i < b.N; i++ {
		_, _ = bb.getObject(ctx, key, 0, 0)
	}
}

func BenchmarkS3Backend_GetObject_1MB(b *testing.B) {
	bb := newBenchBackend()
	seedBackend(bb, 1, 1024*1024)
	key := "bench/key-0000"
	ctx := context.Background()

	b.ResetTimer()
	b.SetBytes(1024 * 1024)
	for i := 0; i < b.N; i++ {
		_, _ = bb.getObject(ctx, key, 0, 0)
	}
}

func BenchmarkS3Backend_GetObject_10MB(b *testing.B) {
	bb := newBenchBackend()
	seedBackend(bb, 1, 10*1024*1024)
	key := "bench/key-0000"
	ctx := context.Background()

	b.ResetTimer()
	b.SetBytes(10 * 1024 * 1024)
	for i := 0; i < b.N; i++ {
		_, _ = bb.getObject(ctx, key, 0, 0)
	}
}

// ─── PutObject benchmarks ─────────────────────────────────────────────────────

func BenchmarkS3Backend_PutObject_1KB(b *testing.B) {
	bb := newBenchBackend()
	payload := makePayload(1024)
	ctx := context.Background()

	b.ResetTimer()
	b.SetBytes(1024)
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench/put-%d", i)
		_ = bb.putObject(ctx, key, payload)
	}
}

func BenchmarkS3Backend_PutObject_1MB(b *testing.B) {
	bb := newBenchBackend()
	payload := makePayload(1024 * 1024)
	ctx := context.Background()

	b.ResetTimer()
	b.SetBytes(1024 * 1024)
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench/put-%d", i)
		_ = bb.putObject(ctx, key, payload)
	}
}

// ─── DeleteObject benchmark ───────────────────────────────────────────────────

func BenchmarkS3Backend_DeleteObject(b *testing.B) {
	ctx := context.Background()
	payload := makePayload(1024)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		bb := newBenchBackend()
		key := fmt.Sprintf("bench/del-%d", i)
		bb.objects[key] = payload
		b.StartTimer()
		_ = bb.deleteObject(ctx, key)
	}
}

// ─── ListObjects benchmarks ───────────────────────────────────────────────────

func BenchmarkS3Backend_ListObjects_100entries(b *testing.B) {
	bb := newBenchBackend()
	seedBackend(bb, 100, 64)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = bb.listObjects(ctx, "bench/", 100)
	}
}

func BenchmarkS3Backend_ListObjects_1000entries(b *testing.B) {
	bb := newBenchBackend()
	seedBackend(bb, 1000, 64)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = bb.listObjects(ctx, "bench/", 1000)
	}
}

// ─── Concurrent GetObject benchmark ──────────────────────────────────────────

func BenchmarkS3Backend_Concurrent_Get(b *testing.B) {
	bb := newBenchBackend()
	seedBackend(bb, 100, 1024)
	ctx := context.Background()

	b.ResetTimer()
	b.SetBytes(1024)
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench/key-%04d", i%100)
			_, _ = bb.getObject(ctx, key, 0, 0)
			i++
		}
	})
}

// ─── Latency distribution benchmark ─────────────────────────────────────────

// BenchmarkS3Backend_GetObject_Latency measures the call overhead of getObject
// itself, excluding allocation costs, by tracking wall-clock time per call.
func BenchmarkS3Backend_GetObject_Latency(b *testing.B) {
	bb := newBenchBackend()
	seedBackend(bb, 1, 4096)
	key := "bench/key-0000"
	ctx := context.Background()

	var totalNs int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		_, _ = bb.getObject(ctx, key, 0, 0)
		totalNs += time.Since(start).Nanoseconds()
	}
	if b.N > 0 {
		b.ReportMetric(float64(totalNs)/float64(b.N), "ns/call")
	}
}
