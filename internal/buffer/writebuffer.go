package buffer

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// WriteBuffer implements intelligent write buffering for improved performance
type WriteBuffer struct {
	mu       sync.RWMutex
	config   *WriteBufferConfig
	buffers  map[string]*buffer
	stats    WriteBufferStats
	flushCh  chan string
	stopCh   chan struct{}
	stopped  chan struct{}
}

// WriteBufferConfig represents write buffer configuration
type WriteBufferConfig struct {
	// Buffer settings
	MaxBufferSize    int64         `yaml:"max_buffer_size"`
	MaxBuffers       int           `yaml:"max_buffers"`
	FlushInterval    time.Duration `yaml:"flush_interval"`
	FlushThreshold   int64         `yaml:"flush_threshold"`
	
	// Performance settings
	AsyncFlush       bool          `yaml:"async_flush"`
	BatchSize        int           `yaml:"batch_size"`
	MaxWriteDelay    time.Duration `yaml:"max_write_delay"`
	CompressionLevel int           `yaml:"compression_level"`
	
	// Reliability settings
	SyncOnClose      bool          `yaml:"sync_on_close"`
	VerifyWrites     bool          `yaml:"verify_writes"`
	MaxRetries       int           `yaml:"max_retries"`
	RetryDelay       time.Duration `yaml:"retry_delay"`
}

// WriteBufferStats tracks write buffer performance metrics
type WriteBufferStats struct {
	TotalWrites      uint64        `json:"total_writes"`
	TotalFlushes     uint64        `json:"total_flushes"`
	TotalBytes       int64         `json:"total_bytes"`
	PendingWrites    int           `json:"pending_writes"`
	PendingBytes     int64         `json:"pending_bytes"`
	AvgFlushTime     time.Duration `json:"avg_flush_time"`
	BufferHitRate    float64       `json:"buffer_hit_rate"`
	CompressionRatio float64       `json:"compression_ratio"`
	Errors           uint64        `json:"errors"`
	LastFlush        time.Time     `json:"last_flush"`
}

// buffer represents a single write buffer for a file
type buffer struct {
	key           string
	data          []byte
	offset        int64
	lastWrite     time.Time
	lastAccess    time.Time
	pendingWrites int
	dirty         bool
	flushing      bool
}

// WriteRequest represents a write operation request
type WriteRequest struct {
	Key    string
	Offset int64
	Data   []byte
	Sync   bool
}

// WriteResponse represents the result of a write operation
type WriteResponse struct {
	BytesWritten int
	Error        error
	Buffered     bool
	FlushTime    time.Duration
}

// FlushCallback is called when a buffer is flushed
type FlushCallback func(key string, data []byte, offset int64) error

// NewWriteBuffer creates a new write buffer instance
func NewWriteBuffer(config *WriteBufferConfig, flushCallback FlushCallback) (*WriteBuffer, error) {
	if config == nil {
		config = &WriteBufferConfig{
			MaxBufferSize:    64 * 1024 * 1024, // 64MB
			MaxBuffers:       1000,
			FlushInterval:    30 * time.Second,
			FlushThreshold:   16 * 1024 * 1024, // 16MB
			AsyncFlush:       true,
			BatchSize:        10,
			MaxWriteDelay:    5 * time.Second,
			CompressionLevel: 1,
			SyncOnClose:      true,
			VerifyWrites:     false,
			MaxRetries:       3,
			RetryDelay:       time.Second,
		}
	}

	// Apply defaults for zero values
	if config.FlushInterval <= 0 {
		config.FlushInterval = 30 * time.Second
	}
	if config.MaxBuffers <= 0 {
		config.MaxBuffers = 1000
	}
	if config.MaxRetries <= 0 {
		config.MaxRetries = 3
	}
	if config.RetryDelay <= 0 {
		config.RetryDelay = time.Second
	}

	wb := &WriteBuffer{
		config:  config,
		buffers: make(map[string]*buffer),
		stats:   WriteBufferStats{},
		flushCh: make(chan string, config.MaxBuffers),
		stopCh:  make(chan struct{}),
		stopped: make(chan struct{}),
	}

	// Start background flush goroutine
	go wb.flushLoop(flushCallback)

	return wb, nil
}

// Write buffers a write operation
func (wb *WriteBuffer) WriteWithRequest(ctx context.Context, req *WriteRequest) *WriteResponse {
	start := time.Now()
	response := &WriteResponse{}

	wb.mu.Lock()
	defer wb.mu.Unlock()

	// Update stats
	wb.stats.TotalWrites++
	wb.stats.TotalBytes += int64(len(req.Data))

	// Get or create buffer for this key
	buf, exists := wb.buffers[req.Key]
	if !exists {
		// Check if we have room for a new buffer
		if len(wb.buffers) >= wb.config.MaxBuffers {
			// Force flush least recently used buffer
			wb.evictLRUBuffer()
		}

		buf = &buffer{
			key:        req.Key,
			data:       make([]byte, 0, wb.config.MaxBufferSize),
			offset:     req.Offset,
			lastWrite:  time.Now(),
			lastAccess: time.Now(),
			dirty:      false,
		}
		wb.buffers[req.Key] = buf
	}

	// Update buffer access time
	buf.lastAccess = time.Now()

	// Handle write to buffer
	if wb.canBufferWrite(buf, req) {
		// Buffer the write
		response.Buffered = true
		response.BytesWritten = len(req.Data)
		
		wb.appendToBuffer(buf, req)
		wb.stats.PendingWrites++
		wb.stats.PendingBytes += int64(len(req.Data))

		// Check if we should trigger immediate flush
		if wb.shouldFlushBuffer(buf) || req.Sync {
			wb.scheduleFlush(buf.key)
		}
	} else {
		// Direct write (buffer full or other constraint)
		response.Buffered = false
		response.Error = fmt.Errorf("buffer full or write cannot be buffered")
	}

	response.FlushTime = time.Since(start)
	return response
}


// Sync ensures all buffered writes are flushed and synced
func (wb *WriteBuffer) Sync(ctx context.Context) error {
	wb.mu.Lock()
	keys := make([]string, 0, len(wb.buffers))
	for key := range wb.buffers {
		keys = append(keys, key)
	}
	wb.mu.Unlock()

	// Schedule flush for all buffers
	for _, key := range keys {
		wb.scheduleFlush(key)
	}

	// Wait for flushes to complete (simplified implementation)
	timeout := time.NewTimer(wb.config.MaxWriteDelay * 2)
	defer timeout.Stop()

	for {
		select {
		case <-timeout.C:
			return fmt.Errorf("sync timeout")
		default:
			wb.mu.RLock()
			pendingCount := len(wb.buffers)
			wb.mu.RUnlock()
			
			if pendingCount == 0 {
				return nil
			}
			
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// GetStats returns current buffer statistics
func (wb *WriteBuffer) GetStats() WriteBufferStats {
	wb.mu.RLock()
	defer wb.mu.RUnlock()

	stats := wb.stats
	stats.PendingWrites = len(wb.buffers)
	
	// Calculate pending bytes
	stats.PendingBytes = 0
	for _, buf := range wb.buffers {
		stats.PendingBytes += int64(len(buf.data))
	}

	return stats
}

// Close closes the write buffer and flushes all pending writes
func (wb *WriteBuffer) Close() error {
	if wb.config.SyncOnClose {
		if err := wb.Sync(context.Background()); err != nil {
			return err
		}
	}

	close(wb.stopCh)
	<-wb.stopped

	return nil
}

// Helper methods

func (wb *WriteBuffer) canBufferWrite(buf *buffer, req *WriteRequest) bool {
	// Check if adding this write would exceed buffer size
	newSize := int64(len(buf.data)) + int64(len(req.Data))
	if newSize > wb.config.MaxBufferSize {
		return false
	}

	// Check if write is contiguous (simplified logic)
	if len(buf.data) > 0 {
		expectedOffset := buf.offset + int64(len(buf.data))
		if req.Offset != expectedOffset {
			return false // Non-contiguous write
		}
	}

	return true
}

func (wb *WriteBuffer) appendToBuffer(buf *buffer, req *WriteRequest) {
	if len(buf.data) == 0 {
		buf.offset = req.Offset
	}

	buf.data = append(buf.data, req.Data...)
	buf.lastWrite = time.Now()
	buf.dirty = true
	buf.pendingWrites++
}

func (wb *WriteBuffer) shouldFlushBuffer(buf *buffer) bool {
	// Flush if buffer size exceeds threshold
	if int64(len(buf.data)) >= wb.config.FlushThreshold {
		return true
	}

	// Flush if buffer is old
	if time.Since(buf.lastWrite) > wb.config.FlushInterval {
		return true
	}

	// Flush if too many pending writes
	if buf.pendingWrites > wb.config.BatchSize {
		return true
	}

	return false
}

func (wb *WriteBuffer) scheduleFlush(key string) {
	select {
	case wb.flushCh <- key:
		// Successfully scheduled
	default:
		// Channel full, flush synchronously
		go wb.flushBuffer(key, nil)
	}
}

func (wb *WriteBuffer) evictLRUBuffer() {
	var oldestKey string
	var oldestTime time.Time
	first := true

	for key, buf := range wb.buffers {
		if first || buf.lastAccess.Before(oldestTime) {
			oldestKey = key
			oldestTime = buf.lastAccess
			first = false
		}
	}

	if oldestKey != "" {
		wb.scheduleFlush(oldestKey)
	}
}

func (wb *WriteBuffer) flushLoop(callback FlushCallback) {
	defer close(wb.stopped)

	ticker := time.NewTicker(wb.config.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-wb.stopCh:
			// Flush all remaining buffers before stopping
			wb.mu.RLock()
			keys := make([]string, 0, len(wb.buffers))
			for key := range wb.buffers {
				keys = append(keys, key)
			}
			wb.mu.RUnlock()

			for _, key := range keys {
				wb.flushBuffer(key, callback)
			}
			return

		case key := <-wb.flushCh:
			wb.flushBuffer(key, callback)

		case <-ticker.C:
			// Periodic flush of old buffers
			wb.flushStaleBuffers(callback)
		}
	}
}

func (wb *WriteBuffer) flushBuffer(key string, callback FlushCallback) {
	wb.mu.Lock()
	buf, exists := wb.buffers[key]
	if !exists || !buf.dirty || buf.flushing {
		wb.mu.Unlock()
		return
	}

	buf.flushing = true
	data := make([]byte, len(buf.data))
	copy(data, buf.data)
	offset := buf.offset
	wb.mu.Unlock()

	// Perform the actual flush
	start := time.Now()
	var err error
	
	if callback != nil {
		err = callback(key, data, offset)
	}

	flushTime := time.Since(start)

	// Update stats and clean up
	wb.mu.Lock()
	defer wb.mu.Unlock()

	if err == nil {
		// Successful flush
		delete(wb.buffers, key)
		wb.stats.TotalFlushes++
		wb.stats.PendingWrites--
		wb.stats.PendingBytes -= int64(len(data))
		wb.stats.LastFlush = time.Now()
		
		// Update average flush time
		if wb.stats.TotalFlushes == 1 {
			wb.stats.AvgFlushTime = flushTime
		} else {
			wb.stats.AvgFlushTime = time.Duration(
				(int64(wb.stats.AvgFlushTime)*9 + int64(flushTime)) / 10,
			)
		}
	} else {
		// Flush failed, mark buffer as not flushing for retry
		if buf, stillExists := wb.buffers[key]; stillExists {
			buf.flushing = false
		}
		wb.stats.Errors++
	}
}

func (wb *WriteBuffer) flushStaleBuffers(callback FlushCallback) {
	wb.mu.RLock()
	staleKeys := make([]string, 0)
	now := time.Now()

	for key, buf := range wb.buffers {
		if buf.dirty && !buf.flushing {
			if now.Sub(buf.lastWrite) > wb.config.FlushInterval {
				staleKeys = append(staleKeys, key)
			}
		}
	}
	wb.mu.RUnlock()

	for _, key := range staleKeys {
		wb.flushBuffer(key, callback)
	}
}

// BufferInfo provides information about a specific buffer
type BufferInfo struct {
	Key           string        `json:"key"`
	Size          int64         `json:"size"`
	Offset        int64         `json:"offset"`
	PendingWrites int           `json:"pending_writes"`
	LastWrite     time.Time     `json:"last_write"`
	LastAccess    time.Time     `json:"last_access"`
	Dirty         bool          `json:"dirty"`
	Flushing      bool          `json:"flushing"`
}

// GetBufferInfo returns information about all buffers
func (wb *WriteBuffer) GetBufferInfo() []BufferInfo {
	wb.mu.RLock()
	defer wb.mu.RUnlock()

	info := make([]BufferInfo, 0, len(wb.buffers))
	for _, buf := range wb.buffers {
		info = append(info, BufferInfo{
			Key:           buf.key,
			Size:          int64(len(buf.data)),
			Offset:        buf.offset,
			PendingWrites: buf.pendingWrites,
			LastWrite:     buf.lastWrite,
			LastAccess:    buf.lastAccess,
			Dirty:         buf.dirty,
			Flushing:      buf.flushing,
		})
	}

	return info
}

// OptimizeBuffers performs buffer optimization
func (wb *WriteBuffer) OptimizeBuffers() {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	// Force flush buffers that are taking up too much memory
	totalSize := int64(0)
	for _, buf := range wb.buffers {
		totalSize += int64(len(buf.data))
	}

	if totalSize > wb.config.MaxBufferSize*int64(wb.config.MaxBuffers)/2 {
		// Flush largest buffers first
		type bufferSize struct {
			key  string
			size int
		}

		sizes := make([]bufferSize, 0, len(wb.buffers))
		for key, buf := range wb.buffers {
			sizes = append(sizes, bufferSize{key: key, size: len(buf.data)})
		}

		// Simple bubble sort by size (descending)
		for i := 0; i < len(sizes)-1; i++ {
			for j := i + 1; j < len(sizes); j++ {
				if sizes[i].size < sizes[j].size {
					sizes[i], sizes[j] = sizes[j], sizes[i]
				}
			}
		}

		// Flush largest buffers
		flushCount := len(sizes) / 4 // Flush 25% of buffers
		for i := 0; i < flushCount && i < len(sizes); i++ {
			wb.scheduleFlush(sizes[i].key)
		}
	}
}

// Count returns the number of active buffers (required by types.WriteBuffer interface)
func (wb *WriteBuffer) Count() int {
	wb.mu.RLock()
	defer wb.mu.RUnlock()
	return len(wb.buffers)
}

// Size returns the total size of buffered data (required by types.WriteBuffer interface)
func (wb *WriteBuffer) Size() int64 {
	wb.mu.RLock()
	defer wb.mu.RUnlock()
	return wb.stats.PendingBytes
}

// Write performs a write operation (required by types.WriteBuffer interface)
func (wb *WriteBuffer) Write(key string, offset int64, data []byte) error {
	req := &WriteRequest{
		Key:    key,
		Offset: offset,
		Data:   data,
		Sync:   false,
	}
	resp := wb.WriteWithRequest(context.Background(), req)
	return resp.Error
}

// Flush flushes a specific buffer (required by types.WriteBuffer interface)
func (wb *WriteBuffer) Flush(key string) error {
	return wb.FlushWithContext(context.Background(), key)
}

// FlushWithContext flushes a specific buffer with context (original method)
func (wb *WriteBuffer) FlushWithContext(ctx context.Context, key string) error {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	if key == "" {
		// Flush all buffers
		for bufKey := range wb.buffers {
			wb.scheduleFlush(bufKey)
		}
		return nil
	}

	// Flush specific buffer
	if buf, exists := wb.buffers[key]; exists && buf.dirty {
		wb.scheduleFlush(key)
	}

	return nil
}

// FlushAll flushes all buffers (required by types.WriteBuffer interface)
func (wb *WriteBuffer) FlushAll() error {
	wb.mu.RLock()
	keys := make([]string, 0, len(wb.buffers))
	for key := range wb.buffers {
		keys = append(keys, key)
	}
	wb.mu.RUnlock()

	// Flush each buffer
	for _, key := range keys {
		if err := wb.Flush(key); err != nil {
			return err
		}
	}
	return nil
}