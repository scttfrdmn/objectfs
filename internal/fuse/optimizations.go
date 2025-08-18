package fuse

import (
	"context"
	"sync"
	"time"
)

// ReadAheadManager implements intelligent read-ahead strategies
type ReadAheadManager struct {
	mu             sync.RWMutex
	activeReads    map[string]*ReadPattern
	fs             *FileSystem
	config         *ReadAheadConfig
	prefetchQueue  chan *PrefetchRequest
	stopCh         chan struct{}
}

// ReadAheadConfig configures read-ahead behavior
type ReadAheadConfig struct {
	Enabled        bool          `yaml:"enabled"`
	WindowSize     int64         `yaml:"window_size"`     // Read-ahead window size
	MaxDistance    int64         `yaml:"max_distance"`    // Maximum read-ahead distance
	MinSequential  int           `yaml:"min_sequential"`  // Minimum sequential reads to trigger
	ConcurrentReads int          `yaml:"concurrent_reads"` // Max concurrent prefetch operations
	TTL            time.Duration `yaml:"ttl"`             // Pattern TTL
}

// ReadPattern tracks access patterns for intelligent prefetching
type ReadPattern struct {
	path           string
	lastOffset     int64
	lastSize       int64
	sequentialHits int
	lastAccess     time.Time
	predictedNext  int64
	confidence     float64
}

// PrefetchRequest represents a prefetch operation
type PrefetchRequest struct {
	path   string
	offset int64
	size   int64
}

// NewReadAheadManager creates a new read-ahead manager
func NewReadAheadManager(fs *FileSystem, config *ReadAheadConfig) *ReadAheadManager {
	if config == nil {
		config = &ReadAheadConfig{
			Enabled:         true,
			WindowSize:      64 * 1024,  // 64KB
			MaxDistance:     1024 * 1024, // 1MB
			MinSequential:   3,
			ConcurrentReads: 4,
			TTL:             5 * time.Minute,
		}
	}

	ram := &ReadAheadManager{
		activeReads:   make(map[string]*ReadPattern),
		fs:            fs,
		config:        config,
		prefetchQueue: make(chan *PrefetchRequest, 100),
		stopCh:        make(chan struct{}),
	}

	// Start prefetch workers
	for i := 0; i < config.ConcurrentReads; i++ {
		go ram.prefetchWorker()
	}

	// Start cleanup goroutine
	go ram.cleanupWorker()

	return ram
}

// OnRead records a read operation and triggers prefetching if patterns are detected
func (ram *ReadAheadManager) OnRead(path string, offset, size int64) {
	if !ram.config.Enabled {
		return
	}

	ram.mu.Lock()
	defer ram.mu.Unlock()

	pattern, exists := ram.activeReads[path]
	if !exists {
		pattern = &ReadPattern{
			path:       path,
			lastAccess: time.Now(),
		}
		ram.activeReads[path] = pattern
	}

	// Update pattern
	if offset == pattern.lastOffset+pattern.lastSize {
		// Sequential read detected
		pattern.sequentialHits++
		pattern.confidence = float64(pattern.sequentialHits) / 10.0
		if pattern.confidence > 1.0 {
			pattern.confidence = 1.0
		}
	} else {
		// Non-sequential read, reset
		pattern.sequentialHits = 0
		pattern.confidence = 0.1
	}

	pattern.lastOffset = offset
	pattern.lastSize = size
	pattern.lastAccess = time.Now()
	pattern.predictedNext = offset + size

	// Trigger prefetch if pattern is strong enough
	if pattern.sequentialHits >= ram.config.MinSequential && pattern.confidence > 0.5 {
		ram.schedulePrefetch(path, pattern.predictedNext, ram.config.WindowSize)
	}
}

// schedulePrefetch schedules a prefetch operation
func (ram *ReadAheadManager) schedulePrefetch(path string, offset, size int64) {
	select {
	case ram.prefetchQueue <- &PrefetchRequest{
		path:   path,
		offset: offset,
		size:   size,
	}:
	default:
		// Queue full, skip prefetch
	}
}

// prefetchWorker handles prefetch requests
func (ram *ReadAheadManager) prefetchWorker() {
	for {
		select {
		case req := <-ram.prefetchQueue:
			ram.performPrefetch(req)
		case <-ram.stopCh:
			return
		}
	}
}

// performPrefetch executes a prefetch operation
func (ram *ReadAheadManager) performPrefetch(req *PrefetchRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check if data is already cached
	if ram.fs.cache.Get(req.path, req.offset, req.size) != nil {
		return // Already cached
	}

	// Fetch data from backend
	data, err := ram.fs.backend.GetObject(ctx, req.path, req.offset, req.size)
	if err != nil {
		return // Prefetch failed, not critical
	}

	// Store in cache
	ram.fs.cache.Put(req.path, req.offset, data)

	// Record metrics
	if ram.fs.metrics != nil {
		ram.fs.metrics.RecordOperation("prefetch", time.Since(time.Now()), req.size, true)
	}
}

// cleanupWorker removes expired patterns
func (ram *ReadAheadManager) cleanupWorker() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ram.cleanup()
		case <-ram.stopCh:
			return
		}
	}
}

// cleanup removes expired read patterns
func (ram *ReadAheadManager) cleanup() {
	ram.mu.Lock()
	defer ram.mu.Unlock()

	now := time.Now()
	for path, pattern := range ram.activeReads {
		if now.Sub(pattern.lastAccess) > ram.config.TTL {
			delete(ram.activeReads, path)
		}
	}
}

// Stop stops the read-ahead manager
func (ram *ReadAheadManager) Stop() {
	close(ram.stopCh)
}

// WriteCoalescer optimizes write operations by coalescing small writes
type WriteCoalescer struct {
	mu            sync.RWMutex
	pendingWrites map[string]*CoalescedWrite
	fs            *FileSystem
	config        *WriteCoalescerConfig
	flushTimer    *time.Timer
}

// WriteCoalescerConfig configures write coalescing behavior
type WriteCoalescerConfig struct {
	Enabled       bool          `yaml:"enabled"`
	WindowSize    int64         `yaml:"window_size"`    // Size window for coalescing
	MaxDelay      time.Duration `yaml:"max_delay"`      // Maximum delay before forced flush
	MinWrites     int           `yaml:"min_writes"`     // Minimum writes to trigger coalescing
	BufferSize    int64         `yaml:"buffer_size"`    // Maximum buffer size per file
}

// CoalescedWrite represents a coalesced write operation
type CoalescedWrite struct {
	path       string
	writes     []WriteOp
	totalSize  int64
	firstTime  time.Time
	lastTime   time.Time
}

// WriteOp represents a single write operation
type WriteOp struct {
	offset int64
	data   []byte
	time   time.Time
}

// NewWriteCoalescer creates a new write coalescer
func NewWriteCoalescer(fs *FileSystem, config *WriteCoalescerConfig) *WriteCoalescer {
	if config == nil {
		config = &WriteCoalescerConfig{
			Enabled:    true,
			WindowSize: 64 * 1024,  // 64KB
			MaxDelay:   100 * time.Millisecond,
			MinWrites:  3,
			BufferSize: 1024 * 1024, // 1MB
		}
	}

	return &WriteCoalescer{
		pendingWrites: make(map[string]*CoalescedWrite),
		fs:            fs,
		config:        config,
	}
}

// CoalesceWrite attempts to coalesce a write operation
func (wc *WriteCoalescer) CoalesceWrite(path string, offset int64, data []byte) bool {
	if !wc.config.Enabled {
		return false
	}

	wc.mu.Lock()
	defer wc.mu.Unlock()

	cw, exists := wc.pendingWrites[path]
	if !exists {
		// Start new coalesced write
		cw = &CoalescedWrite{
			path:      path,
			writes:    make([]WriteOp, 0, 10),
			firstTime: time.Now(),
		}
		wc.pendingWrites[path] = cw
	}

	// Check if this write can be coalesced
	if wc.canCoalesce(cw, offset, int64(len(data))) {
		cw.writes = append(cw.writes, WriteOp{
			offset: offset,
			data:   append([]byte(nil), data...), // Copy data
			time:   time.Now(),
		})
		cw.totalSize += int64(len(data))
		cw.lastTime = time.Now()

		// Check if we should flush now
		if wc.shouldFlush(cw) {
			wc.flushCoalescedWrite(cw)
			delete(wc.pendingWrites, path)
		}

		return true
	}

	return false
}

// canCoalesce checks if a write can be coalesced with existing writes
func (wc *WriteCoalescer) canCoalesce(cw *CoalescedWrite, offset, size int64) bool {
	// Check buffer size limit
	if cw.totalSize+size > wc.config.BufferSize {
		return false
	}

	// Check time limit
	if time.Since(cw.firstTime) > wc.config.MaxDelay {
		return false
	}

	// Check if writes are within window
	if len(cw.writes) > 0 {
		lastWrite := cw.writes[len(cw.writes)-1]
		distance := offset - (lastWrite.offset + int64(len(lastWrite.data)))
		if distance > wc.config.WindowSize {
			return false
		}
	}

	return true
}

// shouldFlush determines if coalesced writes should be flushed
func (wc *WriteCoalescer) shouldFlush(cw *CoalescedWrite) bool {
	// Check minimum writes threshold
	if len(cw.writes) >= wc.config.MinWrites {
		return true
	}

	// Check time limit
	if time.Since(cw.firstTime) >= wc.config.MaxDelay {
		return true
	}

	// Check buffer size limit
	if cw.totalSize >= wc.config.BufferSize {
		return true
	}

	return false
}

// flushCoalescedWrite flushes a coalesced write to the buffer
func (wc *WriteCoalescer) flushCoalescedWrite(cw *CoalescedWrite) {
	// Sort writes by offset to ensure proper ordering
	for i := 0; i < len(cw.writes)-1; i++ {
		for j := i + 1; j < len(cw.writes); j++ {
			if cw.writes[i].offset > cw.writes[j].offset {
				cw.writes[i], cw.writes[j] = cw.writes[j], cw.writes[i]
			}
		}
	}

	// Merge overlapping/adjacent writes
	merged := wc.mergeWrites(cw.writes)

	// Write to buffer
	for _, write := range merged {
		_ = wc.fs.buffer.Write(cw.path, write.offset, write.data)
	}
}

// mergeWrites merges overlapping and adjacent writes
func (wc *WriteCoalescer) mergeWrites(writes []WriteOp) []WriteOp {
	if len(writes) <= 1 {
		return writes
	}

	merged := make([]WriteOp, 0, len(writes))
	current := writes[0]

	for i := 1; i < len(writes); i++ {
		next := writes[i]
		currentEnd := current.offset + int64(len(current.data))

		if next.offset <= currentEnd {
			// Overlapping or adjacent, merge
			newEnd := next.offset + int64(len(next.data))
			if newEnd > currentEnd {
				// Extend current write
				newData := make([]byte, newEnd-current.offset)
				copy(newData, current.data)
				copy(newData[next.offset-current.offset:], next.data)
				current.data = newData
			}
		} else {
			// Not adjacent, add current and start new
			merged = append(merged, current)
			current = next
		}
	}

	merged = append(merged, current)
	return merged
}

// FlushAll flushes all pending coalesced writes
func (wc *WriteCoalescer) FlushAll() {
	wc.mu.Lock()
	defer wc.mu.Unlock()

	for path, cw := range wc.pendingWrites {
		wc.flushCoalescedWrite(cw)
		delete(wc.pendingWrites, path)
	}
}