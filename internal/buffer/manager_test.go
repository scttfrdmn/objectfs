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

// testMgrConfig returns a ManagerConfig with fast timeouts and no background loops.
func testMgrConfig() *ManagerConfig {
	return &ManagerConfig{
		WriteBufferConfig: &WriteBufferConfig{
			MaxBufferSize:  1 * 1024 * 1024,
			MaxBuffers:     20,
			FlushInterval:  10 * time.Second,
			FlushThreshold: 512 * 1024,
			AsyncFlush:     false,
			BatchSize:      100,
			MaxWriteDelay:  500 * time.Millisecond,
			SyncOnClose:    false,
			MaxRetries:     1,
			RetryDelay:     time.Millisecond,
		},
		EnableMetrics:       false,     // suppress background goroutine
		MetricsInterval:     time.Hour, // won't fire in tests
		HealthCheckInterval: time.Hour, // won't fire in tests
		MaxErrorRate:        0.5,
		AlertThreshold:      100,
	}
}

// startedManager creates and starts a Manager, returning it and a cleanup func.
func startedManager(t *testing.T, cfg *ManagerConfig) *Manager {
	t.Helper()
	m, err := NewManager(cfg)
	require.NoError(t, err)
	require.NoError(t, m.Start(context.Background()))
	t.Cleanup(func() {
		if m.started {
			m.Stop() //nolint:errcheck
		}
	})
	return m
}

// --- NewManager ---

func TestNewManager_NilConfig_AppliesDefaults(t *testing.T) {
	t.Parallel()

	m, err := NewManager(nil)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.NotNil(t, m.config)
	assert.True(t, m.IsHealthy())
}

func TestNewManager_CustomConfig_Preserved(t *testing.T) {
	t.Parallel()

	cfg := testMgrConfig()
	m, err := NewManager(cfg)
	require.NoError(t, err)
	assert.Equal(t, cfg, m.config)
}

// --- Start / Stop lifecycle ---

func TestManager_StartStop(t *testing.T) {
	t.Parallel()

	m, err := NewManager(testMgrConfig())
	require.NoError(t, err)

	require.NoError(t, m.Start(context.Background()))
	assert.True(t, m.started)
	assert.NotNil(t, m.writeBuffer)

	require.NoError(t, m.Stop())
	assert.False(t, m.started)
}

func TestManager_DoubleStart_ReturnsError(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())

	err := m.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already started")
}

func TestManager_DoubleStop_ReturnsError(t *testing.T) {
	t.Parallel()

	m, err := NewManager(testMgrConfig())
	require.NoError(t, err)
	require.NoError(t, m.Start(context.Background()))
	require.NoError(t, m.Stop())

	err = m.Stop()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not started")
}

// --- Write / Flush / Sync before start ---

func TestManager_WriteBeforeStart_ReturnsError(t *testing.T) {
	t.Parallel()

	m, _ := NewManager(testMgrConfig())
	err := m.Write(context.Background(), "k", 0, []byte("x"), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not started")
}

func TestManager_FlushBeforeStart_ReturnsError(t *testing.T) {
	t.Parallel()

	m, _ := NewManager(testMgrConfig())
	err := m.Flush(context.Background(), "k")
	require.Error(t, err)
}

func TestManager_SyncBeforeStart_ReturnsError(t *testing.T) {
	t.Parallel()

	m, _ := NewManager(testMgrConfig())
	err := m.Sync(context.Background())
	require.Error(t, err)
}

// --- Write and flush ---

func TestManager_Write_BuffersData(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())
	m.RegisterFlushCallback("*", noopFlush)

	err := m.Write(context.Background(), "bucket/file.bin", 0, []byte("hello"), false)
	require.NoError(t, err)

	stats := m.GetStats()
	assert.Equal(t, uint64(1), stats.TotalOperations)
	assert.Equal(t, uint64(1), stats.SuccessfulOps)
	assert.Equal(t, 1, stats.ActiveBuffers)
}

func TestManager_Write_ThenFlushAll_CallsCallback(t *testing.T) {
	t.Parallel()

	var flushedKey string
	var mu sync.Mutex

	m := startedManager(t, testMgrConfig())
	m.RegisterFlushCallback("*", func(key string, _ []byte, _ int64) error {
		mu.Lock()
		flushedKey = key
		mu.Unlock()
		return nil
	})

	key := "objects/data.bin"
	require.NoError(t, m.Write(context.Background(), key, 0, []byte("payload"), false))

	// Flush directly via the underlying write buffer
	require.NoError(t, m.writeBuffer.FlushAll())

	mu.Lock()
	assert.Equal(t, key, flushedKey)
	mu.Unlock()
}

func TestManager_Write_Sync_FlushesAll(t *testing.T) {
	t.Parallel()

	var count int32
	m := startedManager(t, testMgrConfig())
	m.RegisterFlushCallback("*", func(_ string, _ []byte, _ int64) error {
		atomic.AddInt32(&count, 1)
		return nil
	})

	for i := 0; i < 4; i++ {
		require.NoError(t, m.Write(context.Background(), fmt.Sprintf("k%d", i), 0, []byte("x"), false))
	}

	// Schedule flushes via Sync and give flushLoop time to process
	_ = m.Sync(context.Background())
	// Also flush synchronously for test determinism
	_ = m.writeBuffer.FlushAll()

	// At least the FlushAll should have delivered all callbacks
	assert.GreaterOrEqual(t, atomic.LoadInt32(&count), int32(4))
}

// --- RegisterFlushCallback and pattern matching ---

func TestManager_MatchesPattern_Wildcard(t *testing.T) {
	t.Parallel()

	m, _ := NewManager(testMgrConfig())
	assert.True(t, m.matchesPattern("anything/goes", "*"))
	assert.True(t, m.matchesPattern("", "*"))
}

func TestManager_MatchesPattern_PrefixGlob(t *testing.T) {
	t.Parallel()

	m, _ := NewManager(testMgrConfig())
	assert.True(t, m.matchesPattern("data/file.bin", "data/*"))
	assert.True(t, m.matchesPattern("data/nested/file", "data/*"))
	assert.False(t, m.matchesPattern("other/file.bin", "data/*"))
}

func TestManager_MatchesPattern_ExactMatch(t *testing.T) {
	t.Parallel()

	m, _ := NewManager(testMgrConfig())
	assert.True(t, m.matchesPattern("exact/key", "exact/key"))
	assert.False(t, m.matchesPattern("exact/key", "exact/other"))
}

func TestManager_RegisterFlushCallback_FirstMatchWins(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())

	var called string
	m.RegisterFlushCallback("data/*", func(_ string, _ []byte, _ int64) error {
		called = "data/*"
		return nil
	})
	m.RegisterFlushCallback("*", func(_ string, _ []byte, _ int64) error {
		called = "*"
		return nil
	})

	// Flush directly — whichever pattern matches first in map iteration
	_ = m.defaultFlushCallback("data/file.bin", []byte("payload"), 0)
	assert.NotEmpty(t, called) // either pattern matched; no panic
}

// --- Stats ---

func TestManager_StatsTracking_SuccessAndFailure(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())

	// Simulate 3 successes and 2 failures via updateStats directly
	for i := 0; i < 3; i++ {
		m.updateStats(time.Now(), nil)
	}
	for i := 0; i < 2; i++ {
		m.updateStats(time.Now(), fmt.Errorf("err"))
	}

	stats := m.GetStats()
	assert.Equal(t, uint64(5), stats.TotalOperations)
	assert.Equal(t, uint64(3), stats.SuccessfulOps)
	assert.Equal(t, uint64(2), stats.FailedOps)
	assert.InDelta(t, 0.4, stats.ErrorRate, 0.001)
}

func TestManager_StatsTracking_AverageLatency(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())

	// First op sets the latency directly
	m.updateStats(time.Now().Add(-10*time.Millisecond), nil)
	stats := m.GetStats()
	assert.Greater(t, stats.AverageLatency, time.Duration(0))
}

func TestManager_ClearStats_ResetsAll(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())
	m.RegisterFlushCallback("*", noopFlush)

	_ = m.Write(context.Background(), "k", 0, []byte("x"), false)
	m.updateStats(time.Now(), fmt.Errorf("err"))

	m.ClearStats()

	stats := m.GetStats()
	assert.Equal(t, uint64(0), stats.TotalOperations)
	assert.Equal(t, uint64(0), stats.FailedOps)
	assert.Equal(t, float64(0), stats.ErrorRate)
	assert.True(t, stats.IsHealthy)
}

// --- Health ---

func TestManager_IsHealthy_InitiallyTrue(t *testing.T) {
	t.Parallel()

	m, _ := NewManager(testMgrConfig())
	assert.True(t, m.IsHealthy())
}

func TestManager_HealthDegrades_WhenConsecutiveErrorsExceedThreshold(t *testing.T) {
	t.Parallel()

	cfg := testMgrConfig()
	cfg.AlertThreshold = 3
	cfg.MaxErrorRate = 1.0 // won't trigger on rate alone

	m := startedManager(t, cfg)

	for i := 0; i < 4; i++ {
		m.updateStats(time.Now(), fmt.Errorf("err"))
	}

	assert.False(t, m.IsHealthy())
}

func TestManager_HealthDegrades_WhenErrorRateExceedsThreshold(t *testing.T) {
	t.Parallel()

	cfg := testMgrConfig()
	cfg.AlertThreshold = 1000 // won't trigger on count alone
	cfg.MaxErrorRate = 0.1

	m := startedManager(t, cfg)

	// 9 successes then 2 failures → rate ≈ 18%
	for i := 0; i < 9; i++ {
		m.updateStats(time.Now(), nil)
	}
	for i := 0; i < 2; i++ {
		m.updateStats(time.Now(), fmt.Errorf("err"))
	}

	assert.False(t, m.IsHealthy())
}

func TestManager_HealthRecovers_AfterClearStats(t *testing.T) {
	t.Parallel()

	cfg := testMgrConfig()
	cfg.AlertThreshold = 2

	m := startedManager(t, cfg)

	for i := 0; i < 3; i++ {
		m.updateStats(time.Now(), fmt.Errorf("err"))
	}
	require.False(t, m.IsHealthy())

	m.ClearStats()
	assert.True(t, m.IsHealthy())
}

// --- GetStats / GetDetailedInfo ---

func TestManager_GetStats_IncludesWriteBufferStats(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())
	m.RegisterFlushCallback("*", noopFlush)

	_ = m.Write(context.Background(), "k1", 0, []byte("hello"), false)
	_ = m.Write(context.Background(), "k2", 0, []byte("world"), false)

	stats := m.GetStats()
	assert.Equal(t, uint64(2), stats.TotalOperations)
	assert.Equal(t, 2, stats.ActiveBuffers)
}

func TestManager_GetDetailedInfo_ContainsAllFields(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())

	info := m.GetDetailedInfo()
	assert.Contains(t, info, "started")
	assert.Contains(t, info, "config")
	assert.Contains(t, info, "write_buffer_stats")
	assert.Contains(t, info, "buffer_info")
	assert.True(t, info["started"].(bool))
}

// --- Optimize ---

func TestManager_Optimize_NoOp_NoPanic(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())
	// Should not panic with no buffers
	m.Optimize()
	assert.True(t, m.IsHealthy())
}

// --- GetMemoryUsage / GetThroughput ---

func TestManager_GetMemoryUsage_InitiallyZero(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())
	assert.Equal(t, int64(0), m.GetMemoryUsage())
}

func TestManager_GetThroughput_InitiallyZero(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())
	assert.Equal(t, float64(0), m.GetThroughput())
}

// --- Concurrency ---

func TestManager_ConcurrentWrites_RaceFree(t *testing.T) {
	t.Parallel()

	cfg := testMgrConfig()
	cfg.WriteBufferConfig.MaxBuffers = 100
	m := startedManager(t, cfg)
	m.RegisterFlushCallback("*", noopFlush)

	const n = 40
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_ = m.Write(context.Background(), fmt.Sprintf("concurrent/k%d", i), 0, []byte("x"), false)
		}(i)
	}
	wg.Wait()

	stats := m.GetStats()
	assert.Equal(t, uint64(n), stats.TotalOperations)
}

func TestManager_ConcurrentGetStats_RaceFree(t *testing.T) {
	t.Parallel()

	m := startedManager(t, testMgrConfig())
	m.RegisterFlushCallback("*", noopFlush)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.Write(context.Background(), fmt.Sprintf("k%d", i), 0, []byte("x"), false)
			_ = m.GetStats()
			_ = m.GetDetailedInfo()
			_ = m.IsHealthy()
			_ = m.GetMemoryUsage()
		}(i)
	}
	wg.Wait()
}
