//go:build !integration

package cache

import (
	"fmt"
	"testing"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

func newBenchCache(b *testing.B) *MultiLevelCache {
	b.Helper()
	c, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       64 * 1024 * 1024, // 64 MB
			MaxEntries: 10000,
			TTL:        0, // no expiry
			Prefetch:   false,
		},
		L2Config: &L2Config{Enabled: false},
		Policy:   "inclusive",
	})
	if err != nil {
		b.Fatalf("NewMultiLevelCache: %v", err)
	}
	return c
}

func makePayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i % 256)
	}
	return p
}

// ─── Get benchmarks ───────────────────────────────────────────────────────────

// BenchmarkMultiLevelCache_Get_HotPath measures cache-hit latency for a single
// frequently read key.
func BenchmarkMultiLevelCache_Get_HotPath(b *testing.B) {
	c := newBenchCache(b)
	payload := makePayload(4 * 1024) // 4 KB
	c.Put("hot-key", 0, payload)

	b.ResetTimer()
	b.SetBytes(int64(len(payload)))
	for range b.N {
		_ = c.Get("hot-key", 0, int64(len(payload)))
	}
}

// BenchmarkMultiLevelCache_Get_Miss measures the cost of a cache miss (key not present).
func BenchmarkMultiLevelCache_Get_Miss(b *testing.B) {
	c := newBenchCache(b)

	b.ResetTimer()
	for i := range b.N {
		key := fmt.Sprintf("miss-key-%d", i)
		_ = c.Get(key, 0, 4096)
	}
}

// ─── Set/Eviction benchmarks ──────────────────────────────────────────────────

// BenchmarkMultiLevelCache_Set_Eviction measures Put performance while the
// cache is at/near capacity so evictions are triggered continuously.
func BenchmarkMultiLevelCache_Set_Eviction(b *testing.B) {
	// Use a tiny cache so every write causes eviction.
	c, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       1 * 1024 * 1024, // 1 MB
			MaxEntries: 100,
			Prefetch:   false,
		},
		L2Config: &L2Config{Enabled: false},
		Policy:   "inclusive",
	})
	if err != nil {
		b.Fatalf("NewMultiLevelCache: %v", err)
	}

	payload := makePayload(32 * 1024) // 32 KB per entry

	b.ResetTimer()
	b.SetBytes(int64(len(payload)))
	for i := range b.N {
		key := fmt.Sprintf("evict-key-%d", i%200)
		c.Put(key, 0, payload)
	}
}

// ─── Warmup benchmark ────────────────────────────────────────────────────────

// BenchmarkMultiLevelCache_Warmup_10keys measures Warmup cost when no backend
// is configured (no-op path).
func BenchmarkMultiLevelCache_Warmup_10keys(b *testing.B) {
	c := newBenchCache(b)
	keys := make([]string, 10)
	for i := range keys {
		keys[i] = fmt.Sprintf("warm-key-%d", i)
	}

	b.ResetTimer()
	for range b.N {
		_ = c.Warmup(keys)
	}
}

// ─── Parallel Get benchmark ───────────────────────────────────────────────────

// BenchmarkMultiLevelCache_Get_Parallel exercises concurrent cache reads.
func BenchmarkMultiLevelCache_Get_Parallel(b *testing.B) {
	c := newBenchCache(b)
	payload := makePayload(4 * 1024)
	for i := range 100 {
		c.Put(fmt.Sprintf("par-key-%d", i), 0, payload)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(payload)))
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("par-key-%d", i%100)
			_ = c.Get(key, 0, int64(len(payload)))
			i++
		}
	})
}
