package buffer

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- NewBytePool ---

func TestNewBytePool_CreatesAllBuckets(t *testing.T) {
	t.Parallel()

	p := NewBytePool()
	require.NotNil(t, p)

	stats := p.GetStats()
	assert.Equal(t, 13, stats.TotalPools)
	assert.Len(t, stats.PoolSizes, 13)
	assert.Equal(t, 1024, stats.MinBufferSize)
	assert.Equal(t, 67108864, stats.MaxBufferSize) // 64MB
}

// --- Get ---

func TestBytePool_Get_ExactBucketSize(t *testing.T) {
	t.Parallel()

	p := NewBytePool()
	sizes := []int{1024, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576}

	for _, size := range sizes {
		buf := p.Get(size)
		assert.Lenf(t, buf, size, "Get(%d) returned wrong length", size)
	}
}

func TestBytePool_Get_BetweenBuckets_UsesNextLarger(t *testing.T) {
	t.Parallel()

	p := NewBytePool()

	// Between 4096 and 8192 — pool returns 8192 bucket, sliced to requested size
	buf := p.Get(5000)
	assert.Len(t, buf, 5000)
}

func TestBytePool_Get_SmallerThanSmallestBucket(t *testing.T) {
	t.Parallel()

	p := NewBytePool()

	buf := p.Get(128) // smaller than 1024 minimum bucket
	assert.Len(t, buf, 128)
}

func TestBytePool_Get_ZeroSize(t *testing.T) {
	t.Parallel()

	p := NewBytePool()
	buf := p.Get(0)
	assert.Empty(t, buf)
}

func TestBytePool_Get_LargerThanAllBuckets_DirectAlloc(t *testing.T) {
	t.Parallel()

	p := NewBytePool()

	// 128MB exceeds 64MB max bucket
	buf := p.Get(128 * 1024 * 1024)
	assert.Len(t, buf, 128*1024*1024)
}

// --- Put ---

func TestBytePool_Put_Nil_NoPanic(t *testing.T) {
	t.Parallel()

	p := NewBytePool()
	// Must not panic
	p.Put(nil)
}

func TestBytePool_Put_MatchingCapacity_ReturnsToPool(t *testing.T) {
	t.Parallel()

	p := NewBytePool()

	buf := make([]byte, 4096) // capacity matches a bucket
	// Should not panic
	p.Put(buf)
}

func TestBytePool_Put_ClearsBufferContents(t *testing.T) {
	t.Parallel()

	p := NewBytePool()

	buf := p.Get(4096)
	for i := range buf {
		buf[i] = 0xFF
	}

	// Grow to full capacity so Put can match the pool
	full := buf[:cap(buf)]
	p.Put(full)

	// Get again from the pool — contents should be zeroed
	buf2 := p.Get(4096)
	for i, b := range buf2 {
		assert.Equalf(t, byte(0), b, "byte at index %d should be zero after Put", i)
		break // first nonzero is enough to fail
	}
}

func TestBytePool_Put_NonMatchingCapacity_NoPanic(t *testing.T) {
	t.Parallel()

	p := NewBytePool()

	// 777 doesn't match any bucket — should be silently dropped (GC handles it)
	buf := make([]byte, 777)
	p.Put(buf) // no panic
}

// --- Reuse via Get → Put → Get ---

func TestBytePool_Reuse_SameBucket(t *testing.T) {
	t.Parallel()

	p := NewBytePool()

	// Get and put back
	buf1 := p.Get(4096)
	assert.Len(t, buf1, 4096)

	p.Put(buf1[:cap(buf1)])

	// Get again — may or may not be the same backing array, but must be correct length
	buf2 := p.Get(4096)
	assert.Len(t, buf2, 4096)
}

// --- Aliases ---

func TestBytePool_GetBuffer_Alias(t *testing.T) {
	t.Parallel()

	p := NewBytePool()
	buf := p.GetBuffer(8192)
	assert.Len(t, buf, 8192)
}

func TestBytePool_PutBuffer_Alias_NoPanic(t *testing.T) {
	t.Parallel()

	p := NewBytePool()
	buf := p.GetBuffer(8192)
	p.PutBuffer(buf[:cap(buf)]) // no panic
}

// --- GetStats ---

func TestBytePool_GetStats_AllFields(t *testing.T) {
	t.Parallel()

	p := NewBytePool()
	stats := p.GetStats()

	assert.Equal(t, 13, stats.TotalPools)
	assert.Equal(t, 1024, stats.MinBufferSize)
	assert.Equal(t, 67108864, stats.MaxBufferSize)
	assert.Len(t, stats.PoolSizes, 13)

	// Verify the known bucket sizes are present
	sizeSet := map[int]bool{}
	for _, s := range stats.PoolSizes {
		sizeSet[s] = true
	}
	assert.True(t, sizeSet[4096])
	assert.True(t, sizeSet[1048576])
}

// --- Global pool functions ---

func TestGlobalPool_GetBuffer(t *testing.T) {
	t.Parallel()

	buf := GetBuffer(16384)
	assert.Len(t, buf, 16384)
}

func TestGlobalPool_PutBuffer_NoPanic(t *testing.T) {
	t.Parallel()

	buf := GetBuffer(16384)
	PutBuffer(buf[:cap(buf)]) // no panic
}

func TestGlobalPool_GetPoolStats(t *testing.T) {
	t.Parallel()

	stats := GetPoolStats()
	assert.Equal(t, 13, stats.TotalPools)
	assert.Equal(t, 1024, stats.MinBufferSize)
}

// --- Concurrency ---

func TestBytePool_ConcurrentGetPut_RaceFree(t *testing.T) {
	t.Parallel()

	p := NewBytePool()

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			size := 4096 * (1 + i%4) // vary sizes across 4096, 8192, 12288→next bucket, 16384
			buf := p.Get(size)
			for j := range buf {
				buf[j] = byte(i % 256)
			}
			p.Put(buf[:cap(buf)])
		}(i)
	}
	wg.Wait()
}

func TestGlobalPool_ConcurrentAccess_RaceFree(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			buf := GetBuffer(4096)
			buf[0] = byte(i)
			PutBuffer(buf[:cap(buf)])
		}(i)
	}
	wg.Wait()
}
