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

// OnRead records a read operation and triggers prefetching if patterns are detected.
//
// Every read must be reported, hit or miss. The nil receiver is tolerated so that a caller with
// read-ahead disabled — or a test that turned it off — does not need a guard at each call site; there
// are two, and the one on the cache-hit path was originally missing altogether.
func (ram *ReadAheadManager) OnRead(path string, offset, size int64) {
	if ram == nil || !ram.config.Enabled {
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
		ram.schedulePrefetch(path, pattern.predictedNext, ram.prefetchLength(size))
	}
}

// prefetchLength is how much to read ahead for a reader whose last read was size bytes.
//
// It is at least one read's worth, because a prefetch shorter than the read it anticipates cannot
// satisfy that read. The cache answers a Get only when it holds the whole requested range — a partial
// hit is a miss, since it cannot tell a short object from a partially-cached one — so fetching 64 KiB
// ahead of a 128 KiB reader produces an entry that every subsequent read walks straight past. The read
// then fetches the full 128 KiB itself and the prefetched half is paid for twice: once in egress, once
// in the cache capacity it occupies.
//
// That was the shipped default, and it is measurable rather than theoretical. Reading a 3 MiB file
// sequentially at the kernel's 128 KiB MaxRead issued 24 reads plus 18 prefetches of 64 KiB, of which
// zero were ever hit: 43 GETs and 4,325,644 bytes transferred for a 3,145,728-byte file, a 1.38x
// amplification whose entire excess was prefetch. With the window at the read size, the same traversal
// issues 24 GETs and exactly 3,145,728 bytes, and 3 of the 24 reads are served from cache.
//
// WindowSize remains the floor, so a deployment can prefetch further ahead than one read but not less.
func (ram *ReadAheadManager) prefetchLength(size int64) int64 {
	if size > ram.config.WindowSize {
		return size
	}

	return ram.config.WindowSize
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

	// Clamp to the end of the file, and drop a prefetch that starts at or past it.
	//
	// A sequential reader's predicted next offset runs off the end of the file by construction: the
	// last read of every complete traversal is followed by a prediction one read beyond EOF. Sending
	// that to S3 earns a 416 InvalidRange — a billed request, an error-log line, and a latency spike
	// on the reliability path, for a range that cannot exist. Reading a 3 MiB file to its end produced
	// exactly one, every time.
	//
	// The size comes from the write path so a file with pending writes is prefetched against its
	// current length rather than the object's. If it cannot be determined, skip: a prefetch is an
	// optimization and has no business failing a read or guessing at a bound.
	size, err := ram.fs.buffer.FileSize(ctx, req.path)
	if err != nil {
		return
	}

	if req.offset >= size {
		return
	}

	length := min(req.size, size-req.offset)

	// Only now check the cache, against the clamped length rather than the requested one. Asking for
	// the unclamped length would miss forever on the last stretch of every file — the cache holds what
	// it was given and cannot answer for bytes past EOF — so each traversal would re-fetch that tail.
	if ram.fs.cache.Get(req.path, req.offset, length) != nil {
		return // Already cached
	}

	// Fetch through the shared path, which caches what it reads and collapses this request into an
	// identical one already in flight.
	//
	// That sharing is the point here rather than an incidental benefit. A prefetch is issued for the
	// range the reader is predicted to want next, at the length of the read that predicted it, so the
	// read that follows is the same request — and whichever of the two reaches S3 second used to fetch
	// the same bytes again. Under load the reader wins that race, which is when prefetch stops helping
	// and starts doubling every read: measured at 5,373,952 bytes for a 3,145,728-byte sequential
	// traversal, exactly 41 GETs where 24 were needed.
	fetchStart := time.Now()
	data, err := ram.fs.fetch(ctx, req.path, req.offset, length)
	if err != nil {
		return // Prefetch failed, not critical
	}

	// Record metrics using the captured start time (#104).
	// time.Since(time.Now()) always evaluates to ~0 because time.Now() is
	// evaluated at the point of the call, not at fetch start.
	//
	// The size reported is what was transferred, not what was asked for. Those differ on the last
	// prefetch of every file, and a prefetch metric that overstates its own egress is the wrong number
	// to tune a prefetcher with.
	if ram.fs.metrics != nil {
		ram.fs.metrics.RecordOperation("prefetch", time.Since(fetchStart), int64(len(data)), true)
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
