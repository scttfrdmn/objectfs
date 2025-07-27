package buffer

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Manager coordinates multiple write buffers and provides higher-level functionality
type Manager struct {
	mu          sync.RWMutex
	writeBuffer *WriteBuffer
	config      *ManagerConfig
	stats       ManagerStats
	callbacks   map[string]FlushCallback
	started     bool
	stopCh      chan struct{}
}

// ManagerConfig represents buffer manager configuration
type ManagerConfig struct {
	// Buffer settings
	WriteBufferConfig *WriteBufferConfig `yaml:"write_buffer"`
	
	// Manager settings
	EnableMetrics     bool          `yaml:"enable_metrics"`
	MetricsInterval   time.Duration `yaml:"metrics_interval"`
	
	// Health monitoring
	HealthCheckInterval time.Duration `yaml:"health_check_interval"`
	MaxErrorRate        float64       `yaml:"max_error_rate"`
	AlertThreshold      int           `yaml:"alert_threshold"`
	
	// Advanced features
	EnableCompression   bool    `yaml:"enable_compression"`
	EnableDeduplication bool    `yaml:"enable_deduplication"`
	EnableEncryptionn   bool    `yaml:"enable_encryption"`
	CompressionLevel    int     `yaml:"compression_level"`
	
	// Performance tuning
	WorkerThreads       int           `yaml:"worker_threads"`
	QueueSize          int           `yaml:"queue_size"`
	BatchTimeout       time.Duration `yaml:"batch_timeout"`
}

// ManagerStats tracks manager-level statistics
type ManagerStats struct {
	WriteBufferStats WriteBufferStats `json:"write_buffer_stats"`
	
	// Manager-specific stats
	TotalOperations  uint64        `json:"total_operations"`
	SuccessfulOps    uint64        `json:"successful_ops"`
	FailedOps        uint64        `json:"failed_ops"`
	AverageLatency   time.Duration `json:"average_latency"`
	ErrorRate        float64       `json:"error_rate"`
	
	// Health metrics
	IsHealthy        bool      `json:"is_healthy"`
	LastHealthCheck  time.Time `json:"last_health_check"`
	ConsecutiveErrs  int       `json:"consecutive_errors"`
	
	// Resource usage
	MemoryUsage      int64     `json:"memory_usage"`
	ActiveBuffers    int       `json:"active_buffers"`
	QueuedOperations int       `json:"queued_operations"`
	
	// Performance metrics
	ThroughputMBps   float64   `json:"throughput_mbps"`
	CompressionRatio float64   `json:"compression_ratio"`
	DedupeRatio      float64   `json:"dedupe_ratio"`
}

// Operation represents a buffered operation
type Operation struct {
	Type      string
	Key       string
	Offset    int64
	Data      []byte
	Context   context.Context
	Callback  func(error)
	Timestamp time.Time
}

// NewManager creates a new buffer manager
func NewManager(config *ManagerConfig) (*Manager, error) {
	if config == nil {
		config = &ManagerConfig{
			WriteBufferConfig: nil, // Will use defaults
			EnableMetrics:     true,
			MetricsInterval:   30 * time.Second,
			HealthCheckInterval: time.Minute,
			MaxErrorRate:      0.05, // 5%
			AlertThreshold:    10,
			EnableCompression: true,
			CompressionLevel:  1,
			WorkerThreads:     4,
			QueueSize:         1000,
			BatchTimeout:      100 * time.Millisecond,
		}
	}

	manager := &Manager{
		config:    config,
		callbacks: make(map[string]FlushCallback),
		stats:     ManagerStats{IsHealthy: true},
		stopCh:    make(chan struct{}),
	}

	return manager, nil
}

// Start starts the buffer manager
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return fmt.Errorf("manager already started")
	}

	// Initialize write buffer with default callback
	var err error
	m.writeBuffer, err = NewWriteBuffer(m.config.WriteBufferConfig, m.defaultFlushCallback)
	if err != nil {
		return fmt.Errorf("failed to create write buffer: %w", err)
	}

	m.started = true

	// Start background goroutines
	if m.config.EnableMetrics {
		go m.metricsLoop()
	}
	
	go m.healthCheckLoop()

	return nil
}

// Stop stops the buffer manager
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return fmt.Errorf("manager not started")
	}

	close(m.stopCh)

	if m.writeBuffer != nil {
		if err := m.writeBuffer.Close(); err != nil {
			return fmt.Errorf("failed to close write buffer: %w", err)
		}
	}

	m.started = false
	return nil
}

// RegisterFlushCallback registers a callback for a specific key pattern
func (m *Manager) RegisterFlushCallback(pattern string, callback FlushCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callbacks[pattern] = callback
}

// Write performs a buffered write operation
func (m *Manager) Write(ctx context.Context, key string, offset int64, data []byte, sync bool) error {
	start := time.Now()
	
	m.mu.RLock()
	if !m.started {
		m.mu.RUnlock()
		return fmt.Errorf("manager not started")
	}
	wb := m.writeBuffer
	m.mu.RUnlock()

	// Create write request
	req := &WriteRequest{
		Key:    key,
		Offset: offset,
		Data:   data,
		Sync:   sync,
	}

	// Perform the write
	response := wb.WriteWithRequest(ctx, req)
	
	// Update stats
	m.updateStats(start, response.Error)

	return response.Error
}

// Flush flushes buffers for the specified key (or all if key is empty)
func (m *Manager) Flush(ctx context.Context, key string) error {
	m.mu.RLock()
	if !m.started {
		m.mu.RUnlock()
		return fmt.Errorf("manager not started")
	}
	wb := m.writeBuffer
	m.mu.RUnlock()

	return wb.FlushWithContext(ctx, key)
}

// Sync ensures all buffered writes are flushed and synced
func (m *Manager) Sync(ctx context.Context) error {
	m.mu.RLock()
	if !m.started {
		m.mu.RUnlock()
		return fmt.Errorf("manager not started")
	}
	wb := m.writeBuffer
	m.mu.RUnlock()

	return wb.Sync(ctx)
}

// GetStats returns current manager statistics
func (m *Manager) GetStats() ManagerStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := m.stats
	if m.writeBuffer != nil {
		stats.WriteBufferStats = m.writeBuffer.GetStats()
		stats.ActiveBuffers = len(m.writeBuffer.GetBufferInfo())
	}

	return stats
}

// GetDetailedInfo returns detailed information about the buffer state
func (m *Manager) GetDetailedInfo() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	info := make(map[string]interface{})
	info["manager_stats"] = m.stats
	info["started"] = m.started
	info["config"] = m.config

	if m.writeBuffer != nil {
		info["write_buffer_stats"] = m.writeBuffer.GetStats()
		info["buffer_info"] = m.writeBuffer.GetBufferInfo()
	}

	return info
}

// Optimize performs buffer optimization
func (m *Manager) Optimize() {
	m.mu.RLock()
	wb := m.writeBuffer
	m.mu.RUnlock()

	if wb != nil {
		wb.OptimizeBuffers()
	}

	// Update health status
	m.mu.Lock()
	m.checkHealth()
	m.mu.Unlock()
}

// IsHealthy returns whether the manager is in a healthy state
func (m *Manager) IsHealthy() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stats.IsHealthy
}

// Helper methods

func (m *Manager) defaultFlushCallback(key string, data []byte, offset int64) error {
	// Find the appropriate callback for this key
	m.mu.RLock()
	defer m.mu.RUnlock()

	for pattern, callback := range m.callbacks {
		if m.matchesPattern(key, pattern) {
			return callback(key, data, offset)
		}
	}

	// No specific callback found, this is an error in configuration
	return fmt.Errorf("no flush callback registered for key: %s", key)
}

func (m *Manager) matchesPattern(key, pattern string) bool {
	// Simple pattern matching - in practice you'd use glob or regex
	if pattern == "*" {
		return true
	}
	
	// Check prefix match
	if len(pattern) > 0 && pattern[len(pattern)-1] == '*' {
		prefix := pattern[:len(pattern)-1]
		return len(key) >= len(prefix) && key[:len(prefix)] == prefix
	}
	
	// Exact match
	return key == pattern
}

func (m *Manager) updateStats(start time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.stats.TotalOperations++
	duration := time.Since(start)

	if err == nil {
		m.stats.SuccessfulOps++
		m.stats.ConsecutiveErrs = 0
	} else {
		m.stats.FailedOps++
		m.stats.ConsecutiveErrs++
	}

	// Update average latency
	if m.stats.TotalOperations == 1 {
		m.stats.AverageLatency = duration
	} else {
		m.stats.AverageLatency = time.Duration(
			(int64(m.stats.AverageLatency)*9 + int64(duration)) / 10,
		)
	}

	// Update error rate
	if m.stats.TotalOperations > 0 {
		m.stats.ErrorRate = float64(m.stats.FailedOps) / float64(m.stats.TotalOperations)
	}

	// Check if we need to update health status
	if m.stats.ConsecutiveErrs >= m.config.AlertThreshold ||
		m.stats.ErrorRate > m.config.MaxErrorRate {
		m.stats.IsHealthy = false
	}
}

func (m *Manager) checkHealth() {
	// Simple health check logic
	m.stats.LastHealthCheck = time.Now()
	
	// Reset health if error rate is acceptable
	if m.stats.ErrorRate <= m.config.MaxErrorRate && 
		m.stats.ConsecutiveErrs < m.config.AlertThreshold {
		m.stats.IsHealthy = true
	}
}

func (m *Manager) metricsLoop() {
	interval := m.config.MetricsInterval
	if interval <= 0 {
		interval = 30 * time.Second // Default metrics interval
	}
	
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.collectMetrics()
		}
	}
}

func (m *Manager) healthCheckLoop() {
	interval := m.config.HealthCheckInterval
	if interval <= 0 {
		interval = time.Minute // Default health check interval
	}
	
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.mu.Lock()
			m.checkHealth()
			m.mu.Unlock()
		}
	}
}

func (m *Manager) collectMetrics() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.writeBuffer != nil {
		wbStats := m.writeBuffer.GetStats()
		m.stats.WriteBufferStats = wbStats
		
		// Calculate throughput
		if wbStats.TotalFlushes > 0 && !wbStats.LastFlush.IsZero() {
			timeSinceStart := time.Since(wbStats.LastFlush)
			if timeSinceStart > 0 {
				bytesPerSecond := float64(wbStats.TotalBytes) / timeSinceStart.Seconds()
				m.stats.ThroughputMBps = bytesPerSecond / (1024 * 1024)
			}
		}
		
		// Update memory usage estimate
		m.stats.MemoryUsage = wbStats.PendingBytes
		m.stats.ActiveBuffers = int(wbStats.PendingWrites)
		m.stats.CompressionRatio = wbStats.CompressionRatio
	}
}

// Advanced features

// EnableAdvancedFeatures enables advanced buffer features
func (m *Manager) EnableAdvancedFeatures() {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.config.EnableCompression = true
	m.config.EnableDeduplication = true
}

// GetMemoryUsage returns current memory usage
func (m *Manager) GetMemoryUsage() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stats.MemoryUsage
}

// GetThroughput returns current throughput in MB/s
func (m *Manager) GetThroughput() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stats.ThroughputMBps
}

// ClearStats resets all statistics
func (m *Manager) ClearStats() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats = ManagerStats{IsHealthy: true}
}