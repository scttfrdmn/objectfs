//go:build linux || darwin

package fuse

import (
	"context"
	"sync"
	"time"
)

// ReadAheadManager implements intelligent read-ahead strategies
type ReadAheadManager struct {
	mu            sync.RWMutex
	activeReads   map[string]*ReadPattern
	fs            *FileSystem
	config        *ReadAheadConfig
	prefetchQueue chan *PrefetchRequest
	stopCh        chan struct{}
	stopOnce      sync.Once
}

// ReadAheadConfig configures read-ahead behavior
type ReadAheadConfig struct {
	Enabled         bool          `yaml:"enabled"`
	WindowSize      int64         `yaml:"window_size"`      // Read-ahead window size
	MaxDistance     int64         `yaml:"max_distance"`     // Maximum read-ahead distance
	MinSequential   int           `yaml:"min_sequential"`   // Minimum sequential reads to trigger
	ConcurrentReads int           `yaml:"concurrent_reads"` // Max concurrent prefetch operations
	TTL             time.Duration `yaml:"ttl"`              // Pattern TTL
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
			WindowSize:      64 * 1024,   // 64KB
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
	for range config.ConcurrentReads {
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
	fetchStart := time.Now()
	data, err := ram.fs.backend.GetObject(ctx, req.path, req.offset, req.size)
	if err != nil {
		return // Prefetch failed, not critical
	}

	// Store in cache
	ram.fs.cache.Put(req.path, req.offset, data)

	// Record metrics using the captured start time (#104).
	// time.Since(time.Now()) always evaluates to ~0 because time.Now() is
	// evaluated at the point of the call, not at fetch start.
	if ram.fs.metrics != nil {
		ram.fs.metrics.RecordOperation("prefetch", time.Since(fetchStart), req.size, true)
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

// Stop stops the read-ahead manager. Safe to call multiple times (#104).
func (ram *ReadAheadManager) Stop() {
	ram.stopOnce.Do(func() {
		close(ram.stopCh)
	})
}
