//go:build benchmark

package benchmarks

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// MockCache represents a simple cache implementation for benchmarking
type MockCache struct {
	data map[string][]byte
	size int64
}

// NewMockCache creates a new mock cache
func NewMockCache() *MockCache {
	return &MockCache{
		data: make(map[string][]byte),
		size: 0,
	}
}

// Get retrieves data from cache
func (c *MockCache) Get(key string) []byte {
	return c.data[key]
}

// Put stores data in cache
func (c *MockCache) Put(key string, data []byte) {
	if existing, exists := c.data[key]; exists {
		c.size -= int64(len(existing))
	}
	c.data[key] = data
	c.size += int64(len(data))
}

// Delete removes data from cache
func (c *MockCache) Delete(key string) {
	if existing, exists := c.data[key]; exists {
		c.size -= int64(len(existing))
		delete(c.data, key)
	}
}

// Size returns total cache size
func (c *MockCache) Size() int64 {
	return c.size
}

// BenchmarkCacheGet benchmarks cache get operations
func BenchmarkCacheGet(b *testing.B) {
	cache := NewMockCache()
	
	// Pre-populate cache with test data
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("key-%d", i)
		data := make([]byte, 1024) // 1KB per entry
		rand.Read(data)
		cache.Put(key, data)
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%1000)
			_ = cache.Get(key)
			i++
		}
	})
}

// BenchmarkCachePut benchmarks cache put operations
func BenchmarkCachePut(b *testing.B) {
	cache := NewMockCache()
	data := make([]byte, 1024) // 1KB per entry
	rand.Read(data)

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i)
			cache.Put(key, data)
			i++
		}
	})
}

// BenchmarkCacheGetMiss benchmarks cache get operations with misses
func BenchmarkCacheGetMiss(b *testing.B) {
	cache := NewMockCache()

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("nonexistent-key-%d", i)
			_ = cache.Get(key)
			i++
		}
	})
}

// BenchmarkCacheMixed benchmarks mixed cache operations
func BenchmarkCacheMixed(b *testing.B) {
	cache := NewMockCache()
	data := make([]byte, 1024) // 1KB per entry
	rand.Read(data)

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%100)
			
			// 70% reads, 25% writes, 5% deletes
			switch i % 20 {
			case 0: // 5% deletes
				cache.Delete(key)
			case 1, 2, 3, 4, 5: // 25% writes
				cache.Put(key, data)
			default: // 70% reads
				_ = cache.Get(key)
			}
			i++
		}
	})
}

// BenchmarkCacheVariousDataSizes benchmarks cache operations with different data sizes
func BenchmarkCacheVariousDataSizes(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096, 16384, 65536} // 64B to 64KB

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size-%dB", size), func(b *testing.B) {
			cache := NewMockCache()
			data := make([]byte, size)
			rand.Read(data)

			b.ResetTimer()
			b.ReportAllocs()

			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					key := fmt.Sprintf("key-%d", i%100)
					
					if i%2 == 0 {
						cache.Put(key, data)
					} else {
						_ = cache.Get(key)
					}
					i++
				}
			})
		})
	}
}

// BenchmarkCacheConcurrency benchmarks cache operations under different concurrency levels
func BenchmarkCacheConcurrency(b *testing.B) {
	concurrency := []int{1, 2, 4, 8, 16, 32}

	for _, p := range concurrency {
		b.Run(fmt.Sprintf("procs-%d", p), func(b *testing.B) {
			cache := NewMockCache()
			data := make([]byte, 1024)
			rand.Read(data)

			// Pre-populate cache
			for i := 0; i < 100; i++ {
				key := fmt.Sprintf("key-%d", i)
				cache.Put(key, data)
			}

			b.SetParallelism(p)
			b.ResetTimer()
			b.ReportAllocs()

			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					key := fmt.Sprintf("key-%d", i%100)
					_ = cache.Get(key)
					i++
				}
			})
		})
	}
}

// BenchmarkCacheEviction benchmarks cache eviction performance
func BenchmarkCacheEviction(b *testing.B) {
	cache := NewMockCache()
	data := make([]byte, 1024)
	rand.Read(data)

	// Fill cache beyond capacity
	for i := 0; i < 10000; i++ {
		key := fmt.Sprintf("key-%d", i)
		cache.Put(key, data)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Simulate eviction by deleting random entries
		keyToDelete := fmt.Sprintf("key-%d", rand.Intn(10000))
		cache.Delete(keyToDelete)
		
		// Add new entry
		newKey := fmt.Sprintf("new-key-%d", i)
		cache.Put(newKey, data)
	}
}

// BenchmarkMemoryAllocation benchmarks memory allocation patterns
func BenchmarkMemoryAllocation(b *testing.B) {
	sizes := []int{1024, 4096, 16384, 65536}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("alloc-%dB", size), func(b *testing.B) {
			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				data := make([]byte, size)
				// Simulate some work with the data
				data[0] = byte(i)
				data[size-1] = byte(i)
			}
		})
	}
}

// BenchmarkStringFormatting benchmarks key generation performance
func BenchmarkStringFormatting(b *testing.B) {
	b.Run("sprintf", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			_ = fmt.Sprintf("key-%d", i)
		}
	})

	b.Run("string-concat", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			_ = "key-" + fmt.Itoa(i)
		}
	})
}

// Helper function to generate random data
func generateRandomData(size int) []byte {
	data := make([]byte, size)
	rand.Read(data)
	return data
}

// BenchmarkDataCopy benchmarks data copying performance
func BenchmarkDataCopy(b *testing.B) {
	sizes := []int{1024, 4096, 16384, 65536}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("copy-%dB", size), func(b *testing.B) {
			src := generateRandomData(size)
			
			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				dst := make([]byte, size)
				copy(dst, src)
			}
		})
	}
}

// BenchmarkTimeOperations benchmarks time-related operations
func BenchmarkTimeOperations(b *testing.B) {
	b.Run("time-now", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			_ = time.Now()
		}
	})

	b.Run("time-since", func(b *testing.B) {
		start := time.Now()
		
		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			_ = time.Since(start)
		}
	})

	b.Run("time-unix", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			_ = time.Now().Unix()
		}
	})
}