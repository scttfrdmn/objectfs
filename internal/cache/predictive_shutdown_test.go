package cache

// Audit findings M19 and M20, and one more found while fixing them.
//
// The three defects share an origin: every pre-existing test in this package sets
// `EnablePrefetch: false`, with the comment "no background workers in unit tests". That is a
// reasonable thing to want and it meant the prefetcher — four workers, a queue, a rate limiter, and
// a shutdown path — had no test coverage of the code that only runs when it is switched on. The
// tests here switch it on.
//
//   M19  Close closed prefetchQueue, whose senders are cache reads. A send on a closed channel
//        panics, including inside a select with a default arm, so an unmount racing a read crashed
//        the process — and on a mount that is the filesystem vanishing under every open descriptor.
//        Close also closed stopCh unconditionally, so calling it twice panicked on its own.
//   M20  The token bucket refilled `int64(elapsed.Seconds())` tokens while assigning
//        `lastRefill = now` unconditionally, so any call less than a second after the previous one
//        refilled nothing and discarded the elapsed time. At 1 Hz or faster it never refilled.
//        Starting at the zero value, it also started empty.
//   —    Nothing anywhere called PredictiveCache.Close. MultiLevelCache had no Close to call it
//        from, so a mount's prefetch workers ran until the process exited.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// prefetchingConfig is predictiveCacheConfig with the prefetcher actually running.
func prefetchingConfig() *PredictiveCacheConfig {
	cfg := predictiveCacheConfig()
	cfg.EnablePrefetch = true
	cfg.MaxConcurrentFetch = 4

	return cfg
}

// TestPredictiveCacheCloseDuringConcurrentReads is the M19 regression test.
//
// It has to race reads against the close rather than sequencing them, because the defect is a race:
// with the queue closed, a send that has already passed the select's guard panics. A test that closed
// first and read after would have found nothing — triggerPrefetch's stopCh arm handles that case —
// and a test that read first and closed after would find nothing either.
//
// The reads go through Get, which is the real caller: triggerPrefetch is reached from every L1 read.
// Going through the exported path is what makes this a test of the defect rather than of a private
// channel send.
func TestPredictiveCacheCloseDuringConcurrentReads(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(prefetchingConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}

	// The read pattern is load-bearing and it took a probe to get right. PredictNextAccess is keyed
	// per object and returns nothing until it has three accesses of *that* key with a sequential
	// score above 0.7 — so a rotation of distinct keys read at offset 0, which is the obvious way to
	// write this, produces zero candidates, triggerPrefetch returns early on the empty slice, and the
	// send this test exists to race is never reached. Measured: 0 jobs queued that way, 14 with
	// ascending offsets within one key. Each reader therefore streams its own key.

	const readers = 8

	var wg sync.WaitGroup

	start := make(chan struct{})

	for r := range readers {
		wg.Add(1)

		go func(r int) {
			defer wg.Done()

			<-start

			// Enough iterations that some are still in flight when Close lands. A panic here fails
			// the test by killing the process, which is the point — it is what a mount would do.
			key := fmt.Sprintf("stream/%d", r)

			for i := range 512 {
				pc.Get(key, int64(i)*4096, 4096)
			}
		}(r)
	}

	close(start)

	// Wait for the prefetcher to have real work in flight, rather than sleeping a fixed interval and
	// hoping. This was `time.Sleep(time.Millisecond)`, which assumed eight goroutines would accumulate
	// three sequential accesses each inside a millisecond — true on an idle machine, false about half
	// the time when the rest of the package's tests are running alongside. The failure was not a
	// spurious one: the vacuity check below correctly reported that no send had raced the close, so the
	// test had proved nothing. Waiting for the condition the test depends on is the fix; sleeping
	// longer would only have made the same assumption with a wider margin.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadUint64(&pc.prefetcher.stats.JobsQueued) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no prefetch job was queued within 5s, so there is no send for the close to race; " +
				"either the prediction path stopped producing candidates or the queue stopped accepting them")
		}

		time.Sleep(100 * time.Microsecond)
	}

	// Close while the readers are running — they each have 512 iterations and the wait above returns on
	// the first queued job, so the great majority are still to come. Twice, because Close closing
	// stopCh unconditionally made a second call panic on its own, and a shutdown path is exactly where
	// a double call happens.
	if err := pc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := pc.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	wg.Wait()

	// A read after close must not panic either. It is allowed to miss — LRUCache.Close discards its
	// entries, so a closed cache is legitimately empty and this deliberately does not assert on the
	// bytes — but returning a miss and crashing the process are the two outcomes this whole test is
	// about telling apart.
	pc.Get("stream/0", 0, 4096)

	// The reads have to have actually produced prefetch work, or the race this test is named for
	// never happened and it passes for the wrong reason — which is precisely the state it was in
	// before a probe measured zero queued jobs.
	if queued := atomic.LoadUint64(&pc.prefetcher.stats.JobsQueued); queued == 0 {
		t.Error("no prefetch job was ever queued, so no send raced the close; the access pattern " +
			"produced no predictions and this test proved nothing")
	}
}

// TestPredictiveCacheGetAfterCloseDoesNotQueue covers the other half of M19: a read that arrives
// entirely after the close must not leave a job behind for a worker that will never take it.
//
// Enough of those would fill the 1000-entry queue, and the symptom would be prefetch silently
// ceasing on some later cache that shares the prefetcher — a failure arbitrarily far from its cause.
func TestPredictiveCacheGetAfterCloseDoesNotQueue(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(prefetchingConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}

	// Warm the predictor before closing, so the reads afterwards are ones that *would* queue. Reads
	// that produce no candidates would satisfy this assertion trivially.
	for i := range 16 {
		pc.Get("stream", int64(i)*4096, 4096)
	}

	if queued := atomic.LoadUint64(&pc.prefetcher.stats.JobsQueued); queued == 0 {
		t.Fatal("no job queued before Close; the fixture produces no predictions, so what follows " +
			"cannot distinguish a working guard from an inert access pattern")
	}

	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	queuedBefore := atomic.LoadUint64(&pc.prefetcher.stats.JobsQueued)

	for i := range 64 {
		pc.Get("stream", int64(16+i)*4096, 4096)
	}

	if queued := atomic.LoadUint64(&pc.prefetcher.stats.JobsQueued); queued != queuedBefore {
		t.Errorf("reads after Close queued %d prefetch job(s); a closed prefetcher has no worker to "+
			"take them, so they accumulate until the queue is full", queued-queuedBefore)
	}
}

// TestRateLimiterRefillsWithinOneSecond is the M20 regression test, and it is the whole defect in
// four calls: at sub-second intervals the old arithmetic refilled zero tokens and advanced
// lastRefill anyway, so the elapsed time was destroyed rather than accumulated.
func TestRateLimiterRefillsWithinOneSecond(t *testing.T) {
	t.Parallel()

	const rate = 1024 * 1024 // 1 MiB/s

	rl := &RateLimiter{
		capacity:   rate,
		refillRate: rate,
		tokens:     0,
		lastRefill: time.Now(),
	}

	// Empty bucket: the first request is refused, which is correct.
	if rl.Allow(rate / 2) {
		t.Fatal("an empty bucket allowed a transfer; the test's premise is wrong")
	}

	// Sub-second refills, which the old code truncated to zero. Ten of these are a whole second's
	// worth of budget, so at least one must be granted — under the old arithmetic none ever was,
	// because each call reset lastRefill and the next 100 ms started from scratch.
	var allowed int

	for range 10 {
		time.Sleep(50 * time.Millisecond)

		if rl.Allow(rate / 100) {
			allowed++
		}
	}

	if allowed == 0 {
		t.Error("no transfer was allowed across 500ms of refill at 1 MiB/s; sub-second elapsed time " +
			"is being truncated to zero tokens and discarded, so the bucket never refills under any " +
			"load that reads more than once a second — which is the load a cache exists for")
	}
}

// TestRateLimiterStartsFull pins the other half of M20. An empty initial bucket refused the first
// prefetch of a mount's life, and the budget could then only be earned by a second of idleness.
func TestRateLimiterStartsFull(t *testing.T) {
	t.Parallel()

	cfg := prefetchingConfig()

	pc, err := NewPredictiveCache(cfg)
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	defer func() { _ = pc.Close() }()

	if !pc.prefetcher.rateLimiter.Allow(1024) {
		t.Error("the rate limiter refused the first request of the cache's life; the bucket starts " +
			"at the zero value, so the whole prefetcher is inert until a second of idleness elapses")
	}
}

// TestRateLimiterCarriesTheSubTokenRemainder covers the reason lastRefill advances by the span that
// became tokens rather than to now. Integer division truncates, so assigning now would drop the
// remainder on every call — the original defect in a subtler form, still leaking budget on a caller
// that polls quickly.
func TestRateLimiterCarriesTheSubTokenRemainder(t *testing.T) {
	t.Parallel()

	// A rate low enough that a single fast call is worth well under one token.
	const rate = 10 // 10 bytes/sec

	rl := &RateLimiter{capacity: rate, refillRate: rate, tokens: 0, lastRefill: time.Now()}

	// Poll hard for a span worth about two tokens. Each individual call converts nothing, so if the
	// remainder were discarded the bucket would still be empty at the end.
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		rl.Allow(1 << 30) // refused; the point is the refill it performs
	}

	if !rl.Allow(1) {
		t.Error("250ms at 10 bytes/sec produced no usable token across many calls; the sub-token " +
			"remainder is being discarded on each one, so a fast poller starves a slow bucket")
	}
}

// TestMultiLevelCacheCloseStopsPrefetchWorkers covers the gap that made M19 unreachable from the
// outside and kept the workers alive: PredictiveCache.Close had no caller in the repository.
func TestMultiLevelCacheCloseStopsPrefetchWorkers(t *testing.T) {
	t.Parallel()

	c, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       8 * 1024 * 1024,
			MaxEntries: 100,
			TTL:        time.Minute,
			Prefetch:   true, // what the mount path sets unconditionally
		},
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache: %v", err)
	}

	// The L1 level must actually be a predictive cache, or this test is asserting on a plain LRU and
	// says nothing about the prefetcher.
	pc, ok := c.levels[0].Cache.(*PredictiveCache)
	if !ok {
		t.Fatalf("L1 level is %T, want *PredictiveCache — Prefetch: true did not wrap it, so this "+
			"test cannot see the workers it is about", c.levels[0].Cache)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// stopCh closed is what retires the workers, and reading it is the only way to observe that from
	// here: the workers have no completion signal to wait on, by design, since a prefetch is a GET
	// nobody is waiting for and blocking an unmount behind one buys nothing.
	select {
	case <-pc.prefetcher.stopCh:
	default:
		t.Error("MultiLevelCache.Close left the prefetch workers running; nothing else calls " +
			"PredictiveCache.Close, so they would outlive the mount")
	}

	// Idempotent, because the unmount path is where a double close happens.
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestMultiLevelCacheCloseWithoutPrefetch keeps Close honest on a level that owns no prefetcher: it
// must succeed, be idempotent, and leave the cache usable as a types.Cache rather than panicking on
// the next read. A plain LRU level does release its entries on Close — that is LRUCache's contract,
// not an accident — so this asserts on the absence of a failure and not on the bytes.
func TestMultiLevelCacheCloseWithoutPrefetch(t *testing.T) {
	t.Parallel()

	c, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled: true, Size: 1024 * 1024, MaxEntries: 10, TTL: time.Minute, Prefetch: false,
		},
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache: %v", err)
	}

	c.Put("k", 0, []byte("payload"))

	if got := c.Get("k", 0, 7); string(got) != "payload" {
		t.Fatalf("Get before Close = %q, want %q — the fixture is wrong, so what follows proves "+
			"nothing", got, "payload")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// Reads after close must be answerable rather than fatal. An LRU level drops its entries, so a
	// miss is correct here; a panic is not.
	c.Get("k", 0, 7)

	var _ types.Cache = c
}
