//go:build !integration

package buffer

import (
	"fmt"
	"testing"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// benchNoopFlush is a flush callback that discards data immediately.
func benchNoopFlush(_ string, _ []byte, _ int64) error { return nil }

func newBenchBuffer(b *testing.B) *WriteBuffer {
	b.Helper()
	cfg := &WriteBufferConfig{
		MaxBufferSize:  16 * 1024 * 1024, // 16 MB
		FlushThreshold: 12 * 1024 * 1024, // 75 %
		AsyncFlush:     false,            // synchronous for deterministic benchmarks
		MaxWriteDelay:  0,
	}
	wb, err := NewWriteBuffer(cfg, benchNoopFlush)
	if err != nil {
		b.Fatalf("NewWriteBuffer: %v", err)
	}
	return wb
}

// ─── Write benchmarks ─────────────────────────────────────────────────────────

// BenchmarkWriteBuffer_Write_1KB measures per-write latency for 1 KB payloads.
func BenchmarkWriteBuffer_Write_1KB(b *testing.B) {
	wb := newBenchBuffer(b)
	defer func() { _ = wb.Close() }()

	payload := make([]byte, 1024)

	b.ResetTimer()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench/key-%d", i%50)
		_ = wb.Write(key, 0, payload)
	}
}

// BenchmarkWriteBuffer_Write_1MB measures per-write latency for 1 MB payloads.
func BenchmarkWriteBuffer_Write_1MB(b *testing.B) {
	wb := newBenchBuffer(b)
	defer func() { _ = wb.Close() }()

	payload := make([]byte, 1024*1024)

	b.ResetTimer()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench/key-%d", i%10)
		_ = wb.Write(key, 0, payload)
	}
}

// ─── Flush benchmark ──────────────────────────────────────────────────────────

// BenchmarkWriteBuffer_Flush_1MB writes a 1 MB block and immediately flushes it.
func BenchmarkWriteBuffer_Flush_1MB(b *testing.B) {
	wb := newBenchBuffer(b)
	defer func() { _ = wb.Close() }()

	payload := make([]byte, 1024*1024)
	const key = "bench/flush-key"

	b.ResetTimer()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		_ = wb.Write(key, 0, payload)
		_ = wb.Flush(key)
	}
}

// ─── Concurrent Write benchmark ───────────────────────────────────────────────

// BenchmarkWriteBuffer_Concurrent_Write exercises parallel writes to different
// keys to measure lock contention behavior.
func BenchmarkWriteBuffer_Concurrent_Write(b *testing.B) {
	wb := newBenchBuffer(b)
	defer func() { _ = wb.Close() }()

	payload := make([]byte, 4*1024) // 4 KB

	b.ResetTimer()
	b.SetBytes(int64(len(payload)))
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("bench/par-key-%d", i%100)
			_ = wb.Write(key, 0, payload)
			i++
		}
	})
}

// BenchmarkWriteBuffer_FlushAll flushes all pending writes in a single call.
func BenchmarkWriteBuffer_FlushAll(b *testing.B) {
	const numKeys = 20
	payload := make([]byte, 64*1024) // 64 KB per key

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		wb := newBenchBuffer(b)
		for j := range numKeys {
			key := fmt.Sprintf("bench/fa-key-%d", j)
			_ = wb.Write(key, 0, payload)
		}
		b.StartTimer()

		_ = wb.FlushAll()
		_ = wb.Close()
	}
}
