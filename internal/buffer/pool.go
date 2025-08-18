package buffer

import (
	"sync"
)

// BytePool provides object pooling for byte slices to reduce GC pressure
type BytePool struct {
	pools map[int]*sync.Pool
	sizes []int
	mu    sync.RWMutex
}

// NewBytePool creates a new byte pool with predefined size buckets
func NewBytePool() *BytePool {
	// Common buffer sizes for ObjectFS workloads
	sizes := []int{
		1024,     // 1KB
		4096,     // 4KB
		8192,     // 8KB
		16384,    // 16KB
		32768,    // 32KB
		65536,    // 64KB
		131072,   // 128KB
		262144,   // 256KB
		524288,   // 512KB
		1048576,  // 1MB
		4194304,  // 4MB
		16777216, // 16MB
		67108864, // 64MB
	}

	pools := make(map[int]*sync.Pool)
	for _, size := range sizes {
		size := size // capture loop variable
		pools[size] = &sync.Pool{
			New: func() interface{} {
				return make([]byte, size)
			},
		}
	}

	return &BytePool{
		pools: pools,
		sizes: sizes,
	}
}

// Get retrieves a byte slice of at least the specified size
func (p *BytePool) Get(size int) []byte {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Find the smallest bucket that can accommodate the requested size
	for _, bucketSize := range p.sizes {
		if bucketSize >= size {
			if pool, exists := p.pools[bucketSize]; exists {
				buf := pool.Get().([]byte)
				return buf[:size] // Return slice with requested length
			}
		}
	}

	// If no suitable pool exists, allocate directly
	return make([]byte, size)
}

// Put returns a byte slice to the pool for reuse
func (p *BytePool) Put(buf []byte) {
	if buf == nil {
		return
	}

	capacity := cap(buf)

	p.mu.RLock()
	defer p.mu.RUnlock()

	// Find matching pool by capacity
	if pool, exists := p.pools[capacity]; exists {
		// Reset length to capacity before putting back
		buf = buf[:capacity]
		// Clear the buffer to prevent data leaks
		for i := range buf {
			buf[i] = 0
		}
		// Store the slice back in the pool
		// nolint:staticcheck // SA6002: sync.Pool.Put requires interface{}, slice allocation is expected
		pool.Put(buf)
	}
	// If no matching pool, let GC handle it
}

// GetBuffer is an alias for Get for better API clarity
func (p *BytePool) GetBuffer(size int) []byte {
	return p.Get(size)
}

// PutBuffer is an alias for Put for better API clarity
func (p *BytePool) PutBuffer(buf []byte) {
	p.Put(buf)
}

// Stats returns statistics about pool usage
type PoolStats struct {
	PoolSizes     []int `json:"pool_sizes"`
	TotalPools    int   `json:"total_pools"`
	MaxBufferSize int   `json:"max_buffer_size"`
	MinBufferSize int   `json:"min_buffer_size"`
}

// GetStats returns current pool statistics
func (p *BytePool) GetStats() PoolStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stats := PoolStats{
		PoolSizes:  make([]int, len(p.sizes)),
		TotalPools: len(p.pools),
	}

	copy(stats.PoolSizes, p.sizes)

	if len(p.sizes) > 0 {
		stats.MinBufferSize = p.sizes[0]
		stats.MaxBufferSize = p.sizes[len(p.sizes)-1]
	}

	return stats
}

// Global pool instance
var defaultBytePool = NewBytePool()

// GetBuffer gets a buffer from the default global pool
func GetBuffer(size int) []byte {
	return defaultBytePool.Get(size)
}

// PutBuffer returns a buffer to the default global pool
func PutBuffer(buf []byte) {
	defaultBytePool.Put(buf)
}

// GetPoolStats returns statistics for the default global pool
func GetPoolStats() PoolStats {
	return defaultBytePool.GetStats()
}
