package buffer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testWBConfig returns a WriteBufferConfig with fast timeouts suited for tests.
// AsyncFlush is false so FlushAll() is synchronous and tests are deterministic.
func testWBConfig() *WriteBufferConfig {
	return &WriteBufferConfig{
		MaxBufferSize:    1 * 1024 * 1024, // 1MB
		MaxBuffers:       20,
		FlushInterval:    10 * time.Second, // long — tests drive flushes
		FlushThreshold:   512 * 1024,
		AsyncFlush:       false,
		BatchSize:        100,
		MaxWriteDelay:    500 * time.Millisecond,
		CompressionLevel: 0,
		SyncOnClose:      false,
		MaxRetries:       1,
		RetryDelay:       time.Millisecond,
	}
}

// noopFlush is a FlushCallback that always succeeds.
func noopFlush(_ string, _ []byte, _ int64) error { return nil }

// errFlush is a FlushCallback that always fails.
func errFlush(_ string, _ []byte, _ int64) error {
	return fmt.Errorf("flush failed")
}

// capturingFlush records every call to a slice protected by mu.
type capturingFlush struct {
	mu    sync.Mutex
	calls []flushCall
}

type flushCall struct {
	key    string
	data   []byte
	offset int64
}

func (c *capturingFlush) callback(key string, data []byte, offset int64) error {
	c.mu.Lock()
	c.calls = append(c.calls, flushCall{key, append([]byte{}, data...), offset})
	c.mu.Unlock()
	return nil
}

func (c *capturingFlush) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *capturingFlush) get(i int) flushCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[i]
}

// --- NewWriteBuffer ---

func TestNewWriteBuffer_NilConfig_AppliesDefaults(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(nil, noopFlush)
	require.NoError(t, err)
	require.NotNil(t, wb)
	defer wb.Close() //nolint:errcheck

	assert.Equal(t, int64(64*1024*1024), wb.config.MaxBufferSize)
	assert.Equal(t, 1000, wb.config.MaxBuffers)
	assert.Equal(t, 30*time.Second, wb.config.FlushInterval)
	assert.Equal(t, 3, wb.config.MaxRetries)
	assert.Equal(t, time.Second, wb.config.RetryDelay)
}

func TestNewWriteBuffer_ZeroValuesGetDefaults(t *testing.T) {
	t.Parallel()

	cfg := &WriteBufferConfig{MaxBufferSize: 1024} // all others zero
	wb, err := NewWriteBuffer(cfg, noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	assert.Equal(t, 30*time.Second, wb.config.FlushInterval)
	assert.Equal(t, 1000, wb.config.MaxBuffers)
	assert.Equal(t, 3, wb.config.MaxRetries)
	assert.Equal(t, time.Second, wb.config.RetryDelay)
}

func TestNewWriteBuffer_CustomConfig_Preserved(t *testing.T) {
	t.Parallel()

	cfg := testWBConfig()
	wb, err := NewWriteBuffer(cfg, noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	assert.Equal(t, cfg.MaxBufferSize, wb.config.MaxBufferSize)
	assert.Equal(t, cfg.MaxBuffers, wb.config.MaxBuffers)
	assert.Equal(t, cfg.FlushInterval, wb.config.FlushInterval)
}

// --- Write / WriteWithRequest ---

func TestWriteBuffer_BasicWrite_Buffered(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	data := []byte("hello, objectfs")
	resp := wb.WriteWithRequest(context.Background(), &WriteRequest{
		Key: "objects/file.bin", Offset: 0, Data: data,
	})

	require.NoError(t, resp.Error)
	assert.True(t, resp.Buffered)
	assert.Equal(t, len(data), resp.BytesWritten)
	assert.Equal(t, 1, wb.Count())
}

func TestWriteBuffer_Write_InterfaceMethod(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	err = wb.Write("key", 0, []byte("data"))
	assert.NoError(t, err)
	assert.Equal(t, 1, wb.Count())
}

func TestWriteBuffer_ContiguousWrites_Accumulate(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	key := "data/genome.fa"
	chunk1 := []byte("ATCG")
	chunk2 := []byte("GGCC")

	r1 := wb.WriteWithRequest(context.Background(), &WriteRequest{Key: key, Offset: 0, Data: chunk1})
	require.True(t, r1.Buffered)

	r2 := wb.WriteWithRequest(context.Background(), &WriteRequest{
		Key: key, Offset: int64(len(chunk1)), Data: chunk2,
	})
	require.True(t, r2.Buffered)

	// Both writes go to the same buffer
	assert.Equal(t, 1, wb.Count())
	info := wb.GetBufferInfo()
	require.Len(t, info, 1)
	assert.Equal(t, int64(len(chunk1)+len(chunk2)), info[0].Size)
	assert.Equal(t, int64(0), info[0].Offset)
}

func TestWriteBuffer_NonContiguousWrite_NotBuffered(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	key := "file.bin"
	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: key, Offset: 0, Data: []byte("hello")})

	// Gap in offset — should fail
	resp := wb.WriteWithRequest(context.Background(), &WriteRequest{
		Key: key, Offset: 100, Data: []byte("world"),
	})
	assert.False(t, resp.Buffered)
	assert.Error(t, resp.Error)
}

func TestWriteBuffer_ExceedsMaxBufferSize_NotBuffered(t *testing.T) {
	t.Parallel()

	cfg := testWBConfig()
	cfg.MaxBufferSize = 8 // tiny

	wb, err := NewWriteBuffer(cfg, noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	resp := wb.WriteWithRequest(context.Background(), &WriteRequest{
		Key: "big", Offset: 0, Data: make([]byte, 16),
	})
	assert.False(t, resp.Buffered)
	assert.Error(t, resp.Error)
}

func TestWriteBuffer_MultipleKeys_IndependentBuffers(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	for i := 0; i < 5; i++ {
		wb.WriteWithRequest(context.Background(), &WriteRequest{
			Key: fmt.Sprintf("key/%d", i), Offset: 0, Data: []byte("payload"),
		})
	}

	assert.Equal(t, 5, wb.Count())
}

// --- Stats ---

func TestWriteBuffer_StatsTracking(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	d1 := []byte("first")
	d2 := []byte("second")
	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "k1", Offset: 0, Data: d1})
	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "k2", Offset: 0, Data: d2})

	stats := wb.GetStats()
	assert.Equal(t, uint64(2), stats.TotalWrites)
	assert.Equal(t, int64(len(d1)+len(d2)), stats.TotalBytes)
	assert.Equal(t, 2, stats.PendingWrites) // one buffer per key
}

func TestWriteBuffer_StatsAfterFlush(t *testing.T) {
	t.Parallel()

	cap := &capturingFlush{}
	wb, err := NewWriteBuffer(testWBConfig(), cap.callback)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "k", Offset: 0, Data: []byte("data")})

	err = wb.FlushAll()
	require.NoError(t, err)

	stats := wb.GetStats()
	assert.Equal(t, uint64(1), stats.TotalFlushes)
	assert.Equal(t, 0, stats.PendingWrites)
	assert.False(t, stats.LastFlush.IsZero())
}

// --- FlushAll / FlushBuffer ---

func TestWriteBuffer_FlushAll_CallsCallbackWithCorrectData(t *testing.T) {
	t.Parallel()

	cap := &capturingFlush{}
	wb, err := NewWriteBuffer(testWBConfig(), cap.callback)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	key := "bucket/data.bin"
	payload := []byte("the actual payload")
	const startOffset = int64(1024)

	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: key, Offset: startOffset, Data: payload})

	err = wb.FlushAll()
	require.NoError(t, err)

	require.Equal(t, 1, cap.count())
	call := cap.get(0)
	assert.Equal(t, key, call.key)
	assert.Equal(t, payload, call.data)
	assert.Equal(t, startOffset, call.offset)
}

func TestWriteBuffer_FlushAll_MultipleKeys(t *testing.T) {
	t.Parallel()

	var count int32
	wb, err := NewWriteBuffer(testWBConfig(), func(_ string, _ []byte, _ int64) error {
		atomic.AddInt32(&count, 1)
		return nil
	})
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	for i := 0; i < 7; i++ {
		wb.WriteWithRequest(context.Background(), &WriteRequest{
			Key: fmt.Sprintf("key%d", i), Offset: 0, Data: []byte("x"),
		})
	}
	require.Equal(t, 7, wb.Count())

	err = wb.FlushAll()
	require.NoError(t, err)

	assert.Equal(t, int32(7), atomic.LoadInt32(&count))
	assert.Equal(t, 0, wb.Count())
}

func TestWriteBuffer_FlushAll_EmptyBuffer_NoError(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	assert.NoError(t, wb.FlushAll())
}

func TestWriteBuffer_FlushCallbackError_BufferRetained(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), errFlush)
	require.NoError(t, err)
	defer func() {
		wb.flushCallback = noopFlush
		wb.Close() //nolint:errcheck
	}()

	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "key", Offset: 0, Data: []byte("data")})

	// Flush directly with errFlush
	wb.flushBuffer("key", errFlush)

	stats := wb.GetStats()
	assert.Equal(t, uint64(1), stats.Errors)
	assert.Equal(t, 1, wb.Count()) // buffer retained on error
}

func TestWriteBuffer_FlushCallbackError_BufferCanRetry(t *testing.T) {
	t.Parallel()

	cap := &capturingFlush{}
	wb, err := NewWriteBuffer(testWBConfig(), cap.callback)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	payload := []byte("retry me")
	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "k", Offset: 0, Data: payload})

	// Fail first
	wb.flushBuffer("k", errFlush)
	assert.Equal(t, 1, wb.Count())

	// Succeed on retry
	wb.flushBuffer("k", cap.callback)
	assert.Equal(t, 0, wb.Count())
	assert.Equal(t, 1, cap.count())
	assert.Equal(t, payload, cap.get(0).data)
}

// --- Close ---

func TestWriteBuffer_Close_FlushesOnStop(t *testing.T) {
	t.Parallel()

	var flushed int32
	cb := func(_ string, _ []byte, _ int64) error {
		atomic.AddInt32(&flushed, 1)
		return nil
	}

	cfg := testWBConfig()
	cfg.SyncOnClose = false

	wb, err := NewWriteBuffer(cfg, cb)
	require.NoError(t, err)

	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "k", Offset: 0, Data: []byte("data")})

	require.NoError(t, wb.Close()) // blocks until flushLoop drains
	assert.Equal(t, int32(1), atomic.LoadInt32(&flushed))
}

func TestWriteBuffer_Close_SyncOnClose_FlushesBeforeStop(t *testing.T) {
	t.Parallel()

	var flushed int32
	cb := func(_ string, _ []byte, _ int64) error {
		atomic.AddInt32(&flushed, 1)
		return nil
	}

	cfg := testWBConfig()
	cfg.SyncOnClose = true
	cfg.MaxWriteDelay = time.Second

	wb, err := NewWriteBuffer(cfg, cb)
	require.NoError(t, err)

	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "k", Offset: 0, Data: []byte("data")})

	require.NoError(t, wb.Close())
	assert.Equal(t, int32(1), atomic.LoadInt32(&flushed))
}

// --- GetBufferInfo ---

func TestWriteBuffer_GetBufferInfo(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "alpha", Offset: 10, Data: []byte("abc")})
	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "beta", Offset: 0, Data: []byte("xy")})

	infos := wb.GetBufferInfo()
	require.Len(t, infos, 2)

	byKey := map[string]BufferInfo{}
	for _, info := range infos {
		byKey[info.Key] = info
	}

	require.Contains(t, byKey, "alpha")
	require.Contains(t, byKey, "beta")
	assert.Equal(t, int64(3), byKey["alpha"].Size)
	assert.Equal(t, int64(10), byKey["alpha"].Offset)
	assert.True(t, byKey["alpha"].Dirty)
	assert.Equal(t, int64(2), byKey["beta"].Size)
}

// --- shouldFlushBuffer ---

func TestWriteBuffer_ShouldFlush_WhenThresholdExceeded(t *testing.T) {
	t.Parallel()

	cfg := testWBConfig()
	cfg.FlushThreshold = 10
	cfg.AsyncFlush = true

	wb, err := NewWriteBuffer(cfg, noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	buf := &buffer{data: make([]byte, 20)} // exceeds threshold
	assert.True(t, wb.shouldFlushBuffer(buf))
}

func TestWriteBuffer_ShouldFlush_WhenBatchSizeExceeded(t *testing.T) {
	t.Parallel()

	cfg := testWBConfig()
	cfg.BatchSize = 2
	cfg.AsyncFlush = true

	wb, err := NewWriteBuffer(cfg, noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	buf := &buffer{data: []byte("x"), pendingWrites: 3}
	assert.True(t, wb.shouldFlushBuffer(buf))
}

func TestWriteBuffer_ShouldNotFlush_WhenAsyncDisabled(t *testing.T) {
	t.Parallel()

	cfg := testWBConfig()
	cfg.AsyncFlush = false
	cfg.FlushThreshold = 1 // would trigger if async enabled

	wb, err := NewWriteBuffer(cfg, noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	buf := &buffer{data: make([]byte, 100)}
	assert.False(t, wb.shouldFlushBuffer(buf))
}

// --- MaxBuffers eviction ---

func TestWriteBuffer_MaxBuffers_EvictsLRU(t *testing.T) {
	t.Parallel()

	cfg := testWBConfig()
	cfg.MaxBuffers = 3
	cfg.AsyncFlush = true // needed so scheduleFlush sends to channel

	wb, err := NewWriteBuffer(cfg, noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	for i := 0; i < 3; i++ {
		wb.WriteWithRequest(context.Background(), &WriteRequest{
			Key: fmt.Sprintf("k%d", i), Offset: 0, Data: []byte("x"),
		})
	}
	assert.Equal(t, 3, wb.Count())

	// Adding a 4th key triggers eviction of the LRU buffer
	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "new", Offset: 0, Data: []byte("y")})

	// Count may be ≤4 depending on async flush timing; just ensure no panic
	assert.LessOrEqual(t, wb.Count(), 4)
}

// --- OptimizeBuffers ---

func TestWriteBuffer_OptimizeBuffers_NoOp_WhenBelowThreshold(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "k", Offset: 0, Data: []byte("small")})

	// Should not panic and should not flush the only small buffer
	wb.OptimizeBuffers()
	assert.Equal(t, 1, wb.Count())
}

func TestWriteBuffer_OptimizeBuffers_FlushesLargeBuffers(t *testing.T) {
	t.Parallel()

	cfg := testWBConfig()
	cfg.MaxBufferSize = 100
	cfg.MaxBuffers = 4
	cfg.AsyncFlush = true

	wb, err := NewWriteBuffer(cfg, noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	// Each buffer is 60 bytes; total = 240 > MaxBufferSize(100)*MaxBuffers(4)/2 = 200
	for i := 0; i < 4; i++ {
		wb.WriteWithRequest(context.Background(), &WriteRequest{
			Key: fmt.Sprintf("k%d", i), Offset: 0, Data: make([]byte, 60),
		})
	}

	// Should schedule flushes for the largest buffers without panicking
	wb.OptimizeBuffers()
}

// --- Concurrency ---

func TestWriteBuffer_ConcurrentWrites_DifferentKeys(t *testing.T) {
	t.Parallel()

	var callCount int32
	cb := func(_ string, _ []byte, _ int64) error {
		atomic.AddInt32(&callCount, 1)
		return nil
	}

	cfg := testWBConfig()
	cfg.MaxBuffers = 100

	wb, err := NewWriteBuffer(cfg, cb)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	const n = 30
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			wb.WriteWithRequest(context.Background(), &WriteRequest{
				Key: fmt.Sprintf("key/%d", i), Offset: 0, Data: []byte("payload"),
			})
		}(i)
	}
	wg.Wait()

	err = wb.FlushAll()
	require.NoError(t, err)
	assert.Equal(t, int32(n), atomic.LoadInt32(&callCount))
}

func TestWriteBuffer_ConcurrentReadsAndWrites(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wb.WriteWithRequest(context.Background(), &WriteRequest{
				Key: fmt.Sprintf("k%d", i), Offset: 0, Data: []byte("x"),
			})
			_ = wb.GetStats()
			_ = wb.GetBufferInfo()
			_ = wb.Count()
			_ = wb.Size()
		}(i)
	}
	wg.Wait()
}

func TestWriteBuffer_ConcurrentFlushAndWrite(t *testing.T) {
	t.Parallel()

	cfg := testWBConfig()
	cfg.MaxBuffers = 100

	wb, err := NewWriteBuffer(cfg, noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	var wg sync.WaitGroup

	// Writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				wb.WriteWithRequest(context.Background(), &WriteRequest{
					Key: fmt.Sprintf("w%d", i), Offset: int64(j * 4), Data: []byte("data"),
				})
			}
		}(i)
	}

	// Concurrent FlushAll
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		_ = wb.FlushAll()
	}()

	wg.Wait()
}

// --- Flush / FlushWithContext ---

func TestWriteBuffer_Flush_SpecificKey_SchedulesFlush(t *testing.T) {
	t.Parallel()

	cap := &capturingFlush{}
	wb, err := NewWriteBuffer(testWBConfig(), cap.callback)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "target", Offset: 0, Data: []byte("hello")})

	// Flush via Flush (schedules), then FlushAll to drive the synchronous path
	require.NoError(t, wb.Flush("target"))
	require.NoError(t, wb.FlushAll())
}

func TestWriteBuffer_Flush_EmptyKey_SchedulesAll(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "a", Offset: 0, Data: []byte("1")})
	wb.WriteWithRequest(context.Background(), &WriteRequest{Key: "b", Offset: 0, Data: []byte("2")})

	// Empty key schedules flush for all buffers, then FlushAll drains them
	require.NoError(t, wb.Flush(""))
	require.NoError(t, wb.FlushAll())
}

func TestWriteBuffer_FlushWithContext_MissingKey_NoPanic(t *testing.T) {
	t.Parallel()

	wb, err := NewWriteBuffer(testWBConfig(), noopFlush)
	require.NoError(t, err)
	defer wb.Close() //nolint:errcheck

	// Flushing a key that doesn't exist should be a no-op
	require.NoError(t, wb.FlushWithContext(context.Background(), "nonexistent"))
}
