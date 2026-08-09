//go:build linux || darwin

package fuse

import (
	"context"
	"sync"
	"time"
)

// ReadAheadManager implements intelligent read-ahead strategies.
//
// There is no Stop, and no stop channel. The mount context is the one shutdown signal — see
// [NewReadAheadManager] for why, and #376 for why the second one is gone.
type ReadAheadManager struct {
	mu            sync.RWMutex
	activeReads   map[string]*ReadPattern
	fs            *FileSystem
	config        *ReadAheadConfig
	prefetchQueue chan *PrefetchRequest
}

// ReadAheadConfig configures read-ahead behavior.
//
// Built from performance.read_ahead by [CreatePlatformMountManager], which is what makes those keys
// take effect: the mount path used to construct the manager with nil and run on the defaults below,
// so the whole configuration block was validated at load and read by nothing (#176).
//
// No yaml tags. They were here and they bound to nothing — configuration is decoded into
// config.Configuration, which has no FUSE section — which is exactly what made the block look
// plumbed. config.ReadAheadConfig owns the YAML names now; this type is the internal shape it maps to.
//
// MaxDistance was removed with them. It was a field, a default of 1 MiB, and a yaml tag, and no code
// in this package ever read it: the detector's decision to prefetch is MinSequential, and how far ahead
// it reads is prefetchLength. A cap on read-ahead distance is a plausible knob, but it was never one.
type ReadAheadConfig struct {
	Enabled         bool
	WindowSize      int64         // floor on the prefetch length; see prefetchLength
	MinSequential   int           // sequential reads required before prefetching starts
	ConcurrentReads int           // prefetch workers, each performing one GET at a time
	TTL             time.Duration // how long an idle read pattern is remembered
}

// ReadPattern tracks access patterns for intelligent prefetching.
//
// There was a confidence float64 here, assigned sequentialHits/10 and clamped to 1.0. It was read at
// exactly one place — the prefetch gate, as `confidence > 0.5` alongside the MinSequential check — so
// the two conditions were the same counter in different units and the stricter always won. Removed
// with the gate rather than left as an unread field, because a score derived from the threshold it
// guards is not independent evidence and cannot become any (#247).
type ReadPattern struct {
	path           string
	lastOffset     int64
	lastSize       int64
	sequentialHits int
	lastAccess     time.Time
	predictedNext  int64
}

// PrefetchRequest represents a prefetch operation
type PrefetchRequest struct {
	path   string
	offset int64
	size   int64
}

// DefaultReadAheadConfig is what a nil config becomes, and what performance.read_ahead defaults to.
//
// Exported so config.NewDefault's read-ahead block and this one can be asserted equal rather than
// maintained in parallel by hand. They were not equal before they were wired: config defaulted to a
// 64 MB read-ahead size with a "predictive" strategy while the manager ran a 64 KiB window, and the
// mount used the manager's values because the config never reached it (#176).
func DefaultReadAheadConfig() ReadAheadConfig {
	return ReadAheadConfig{
		Enabled:         true,
		WindowSize:      64 * 1024, // 64KB
		MinSequential:   3,
		ConcurrentReads: 4,
		TTL:             5 * time.Minute,
	}
}

// NewReadAheadManager creates a new read-ahead manager. A nil config takes
// [DefaultReadAheadConfig].
//
// ctx is the mount's, and it does two things. It is the parent of every prefetch GET, so a value the
// mount was configured with reaches the speculative reads made on its behalf — each prefetch used to
// build its own context from context.Background(), which is the one context nothing can carry anything
// into and nothing can cancel.
//
// And it is the shutdown signal — the only one there is. Cancel it and the prefetch workers and the
// cleanup ticker return; there is nothing else to call, which is the point.
//
// That is the resolution of #376, and it is a deletion rather than a wiring-up. There was a `Stop`,
// exported, `sync.Once`-guarded, closing a channel both workers selected on — and called by nothing
// outside tests: not [MountManager.Unmount], not Adapter.Stop, not anything on the unmount path.
// Measured: five goroutines per filesystem, `ConcurrentReads` prefetch workers plus the cleanup ticker,
// surviving every unmount for the life of the process, each holding its FileSystem and through it the
// backend and the cache. A long-running host that remounts, which is what the mount watcher does,
// accumulated them.
//
// The mount context fixed that. What was left was an exported lifecycle method production never called,
// and calling it from Unmount was rejected because it reintroduces the "someone must remember this on
// every mount path" obligation whose being missed was the leak. The remaining case for keeping it was
// that a test might need to stop a manager whose context it does not own — but neutering the channel's
// select arm broke no test, so no test needed it either. A second shutdown path with no caller on either
// side is not a seam worth preserving: two ways to stop the same goroutines is how they came to be
// stopped by neither.
func NewReadAheadManager(ctx context.Context, fs *FileSystem, config *ReadAheadConfig) *ReadAheadManager {
	if config == nil {
		defaults := DefaultReadAheadConfig()
		config = &defaults
	}

	ram := &ReadAheadManager{
		activeReads:   make(map[string]*ReadPattern),
		fs:            fs,
		config:        config,
		prefetchQueue: make(chan *PrefetchRequest, 100),
	}

	// Start prefetch workers
	for range config.ConcurrentReads {
		go ram.prefetchWorker(ctx)
	}

	// Start cleanup goroutine
	go ram.cleanupWorker(ctx)

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

	if offset == pattern.lastOffset+pattern.lastSize {
		pattern.sequentialHits++
	} else {
		// Non-sequential read: the run is over, and a new one has to earn MinSequential again.
		pattern.sequentialHits = 0
	}

	pattern.lastOffset = offset
	pattern.lastSize = size
	pattern.lastAccess = time.Now()
	pattern.predictedNext = offset + size

	// MinSequential alone, which is what it says it is.
	//
	// This was `sequentialHits >= MinSequential && confidence > 0.5`, and confidence was
	// sequentialHits/10 — so the second condition was `sequentialHits > 5`, the same quantity in
	// different units, and the stricter of the two always won. The effective gate was
	// max(MinSequential, 6): setting 1, 2, 3, 4 or 5 all prefetched first on the sixth sequential read,
	// so the shipped default of 3 did not describe the shipped behavior, and the setting worked as
	// documented only above 6 (#247).
	//
	// Dropping the floor rather than raising the defaults to 6, and the measurement is why. A
	// continuing traversal costs the same either way — 3 MiB read sequentially at the kernel's 128 KiB
	// buffer transfers exactly 3,145,728 bytes at every MinSequential from 1 to 10, because
	// FileSystem.fetch shares one flight between a prefetch and the read it anticipates, so a prefetch
	// that is going to be read adds no bytes. The cost of prefetching sooner is a run that *stops*
	// before consuming what was fetched, and that cost is exactly one prefetch: a reader that goes
	// sequential and then walks away wastes 131,072 bytes whether it stops after 3 reads at
	// MinSequential=3 or after 6 at MinSequential=6. The floor did not prevent a class of waste, it
	// only moved when the single unredeemed prefetch became possible. Trading one prefetch's bytes for
	// prefetching from the third read instead of the sixth is worth it, and now the number a user sets
	// is the number the detector uses.
	if pattern.sequentialHits >= ram.config.MinSequential {
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

// consumedThrough returns the first offset of path that the reader has not read yet, or 0 if this
// manager has no pattern for it.
//
// This is [ReadPattern.predictedNext] under a different name, and the rename is the point: the detector
// stores it as a prediction of where the reader is going, and [ReadAheadManager.performPrefetch] needs
// it as a fact about where the reader has been. Both readings are the same number — the end of the last
// read reported — but only one of them is a bound it is safe to skip bytes on the strength of, and
// naming it here keeps a future change to the prediction from silently moving the bound.
//
// It reads the detector's state rather than probing the cache, and the difference matters twice. A
// cache probe for each candidate prefix would allocate and copy the bytes it found only to discard
// them, and — the reason it is not merely wasteful — every probe would land in the cache's hit and miss
// counters, so the prefetcher would be scored as a reader in a statistic the read path exports.
//
// The bound is conservative in the safe direction. A read the detector never saw leaves this behind
// where the reader actually is, so a prefetch trims less than it could and duplicates a range; it can
// never run *ahead* of the reader, which would skip bytes nobody has fetched and leave a hole the
// following read pays for at full price.
func (ram *ReadAheadManager) consumedThrough(path string) int64 {
	ram.mu.RLock()
	defer ram.mu.RUnlock()

	pattern, exists := ram.activeReads[path]
	if !exists {
		return 0
	}

	return pattern.predictedNext
}

// warm queues a fetch of [offset, offset+size) on evidence from outside this reader's own pattern — a
// peer holding more of the object than this node just read (#142). See [FileSystem.warmFromPeers].
//
// It goes through the same queue and the same workers as a predicted prefetch, which is the point: those
// workers already clamp to EOF, skip what is cached, trim past reads in flight, and fetch through the
// shared path. What it deliberately does *not* do is touch the read pattern. The pattern models this
// process's sequential behavior, and folding a cluster-derived range into it would let another node's
// reads decide whether this reader looks sequential.
//
// A nil manager and a disabled config are tolerated, as in [ReadAheadManager.OnRead], so the call site
// needs no guard. Disabled means disabled: an operator who turned read-ahead off has said not to read
// bytes nobody asked for, and a peer's claim is not an exception to that.
func (ram *ReadAheadManager) warm(path string, offset, size int64) {
	if !ram.warmingEnabled() || size <= 0 {
		return
	}

	ram.schedulePrefetch(path, offset, size)
}

// warmingEnabled reports whether a call to [ReadAheadManager.warm] could queue anything.
//
// It exists so [FileSystem.warmFromPeers] can answer that question *before* it asks peers who holds a
// key, rather than after. The guard in warm is the one that keeps the promise; this one keeps a mount
// with read-ahead off from paying a gossip round trip per cache miss to reach it — which on a
// read-heavy workload is one broadcast per miss whose answer is discarded, the sort of cost that
// presents as unexplained cluster traffic rather than as a wrong result.
//
// Both checks are kept deliberately. Collapsing them into this one would leave warm callable with
// read-ahead disabled, and warm is the guarantee an operator's setting rests on.
func (ram *ReadAheadManager) warmingEnabled() bool {
	return ram != nil && ram.config.Enabled
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

// prefetchWorker handles prefetch requests until the mount's context ends.
//
// One shutdown signal, not two. See [NewReadAheadManager].
func (ram *ReadAheadManager) prefetchWorker(mount context.Context) {
	for {
		select {
		case req := <-ram.prefetchQueue:
			ram.performPrefetch(mount, req)
		case <-mount.Done():
			return
		}
	}
}

// performPrefetch executes a prefetch operation.
//
// mount is the mount's context, and the 5-second budget is derived from it rather than from
// context.Background(). Five seconds is the right scope for a speculative read either way — what
// changes is that a prefetch in flight when the mount goes away is canceled instead of running to its
// own deadline against a backend that is being closed underneath it.
func (ram *ReadAheadManager) performPrefetch(mount context.Context, req *PrefetchRequest) {
	ctx, cancel := context.WithTimeout(mount, 5*time.Second)
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

	// Advance past bytes the reader has already read, and drop a prefetch left entirely behind it.
	//
	// A prefetch is queued, and the reader does not wait for it. Between schedulePrefetch and a worker
	// picking the request up, the reader may have read the front of the very range predicted for it —
	// and those reads are then *finished*: absent from the in-flight set, because finish removes them,
	// and no longer a whole-range cache hit either, because the tail beyond them was never cached and
	// [types.Cache.Get] answers only for a range it holds in full. So neither the check above nor the
	// trim below sees them, and the prefetch re-fetches bytes already paid for.
	//
	// Measured on CI, which is loaded enough for the queue to fall behind — a 16 KiB file read in 1 KiB
	// steps transferred 18432 bytes, across GETs of 0-1023 … 4096-5119 and then 3072-16383. That last
	// request re-read 2048 bytes the reads at 3072 and 4096 had already fetched. The same traversal
	// passed locally 28 times in a row, because there the worker kept up and the trim below caught it.
	//
	// This is the trim below one lifecycle stage later, and both are needed: the reader's own reads move
	// from in-flight to consumed as it advances, while a *concurrent* flight on the same path — another
	// handle, or another prefetch — is only ever in the in-flight set.
	if start := ram.consumedThrough(req.path); start > req.offset {
		length -= start - req.offset
		req.offset = start

		if length <= 0 {
			return
		}
	}

	// Trim this prefetch past the end of any read already in flight, and drop it if nothing is left.
	//
	// [FileSystem.fetch] deduplicates by containment, which covers a prefetch that arrives while a read
	// is in flight and a read that arrives while a prefetch is. What neither covers is the third case: a
	// prefetch whose range *contains* an in-flight read but is not contained by it. Nothing can serve
	// that from the read's result — the read holds fewer bytes than the prefetch wants — so it issues a
	// second, overlapping GET and the bytes the read is already fetching are paid for twice. Measured on
	// a 16 KiB file read in 1 KiB steps with the reader winning every race: 17,408 bytes for a 16,384
	// byte file.
	//
	// Trimming rather than skipping, and that distinction is the whole value of this block. Skipping
	// entirely also produces the right byte count — but by never prefetching at all, since a reader that
	// consistently wins the race has a read in flight every time a prefetch is scheduled: the same
	// traversal then issues 16 GETs of 1 KiB instead of 7, one per read, with the prefetcher contributing
	// nothing. Advancing past the in-flight read keeps the read-ahead while fetching each byte once.
	if start := ram.fs.fetches.unclaimedStart(req.path, req.offset); start > req.offset {
		length -= start - req.offset
		req.offset = start

		if length <= 0 {
			return
		}
	}

	// Fetch through the shared path, which caches what it reads and joins a covering request already in
	// flight rather than duplicating it.
	//
	// That sharing is the point here rather than an incidental benefit. A prefetch is issued for the
	// range the reader is predicted to want next, so the read that follows wants bytes this request is
	// already fetching — and whichever of the two reaches S3 second used to fetch them again. Under load
	// the reader wins that race, which is when prefetch stops helping and starts doubling every read:
	// measured at 5,373,952 bytes for a 3,145,728-byte sequential traversal, exactly 41 GETs where 24
	// were needed.
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

// cleanupWorker removes expired patterns until the mount's context ends.
func (ram *ReadAheadManager) cleanupWorker(mount context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ram.cleanup()
		case <-mount.Done():
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
