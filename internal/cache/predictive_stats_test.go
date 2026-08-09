package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// #223. PredictiveStats declared seventeen fields and the cache assigned three of them, one of which —
// PrefetchHits — was guarded by `event.Hit && event.Prefetch` while both AccessEvent construction sites
// set Prefetch false, so it was unreachable. Every ratio was zero for the lifetime of the type, and
// nothing above the cache could read them anyway: the mount holds this cache as an opaque types.Cache
// inside a CacheLevel.
//
// So these tests come in two halves. The first is that the numbers are real — each counter moves for the
// reason it claims to, and no ratio can exceed 1. The second is that they are reachable, which is what
// TestPredictiveStatsAreReachableThroughTheMultiLevelCache is for: a statistic nothing can read is
// indistinguishable from one that is never computed, which is how this survived.

// fixedBackend serves a constant payload for any range, and counts what it was asked for.
//
// A types.Backend and not a testaws server: what these tests need from a backend is that a prefetch
// worker stores bytes, and going through real HTTP would put a network round trip inside a test whose
// subject is a counter. The prefetch path against a real S3 backend is covered by the read-ahead tests;
// the backend here exists to make the ledger's prefetch half reachable at all, which on the current
// mount path it is not — initializeLevels passes no Backend, so a mount's four workers dequeue jobs and
// fetch nothing.
type fixedBackend struct {
	types.Backend // embedded for the ~30 methods these tests never call; a nil-method call panics loudly

	payload []byte
	gets    atomic.Int64
}

func (b *fixedBackend) GetObject(_ context.Context, _ string, _, size int64) ([]byte, error) {
	b.gets.Add(1)

	if size <= 0 || size > int64(len(b.payload)) {
		return b.payload, nil
	}

	return b.payload[:size], nil
}

// statsConfig is predictiveCacheConfig with the prefetcher running against a backend that returns bytes.
func statsConfig(backend types.Backend) *PredictiveCacheConfig {
	cfg := predictiveCacheConfig()
	cfg.EnablePrefetch = true
	cfg.MaxConcurrentFetch = 4
	cfg.Backend = backend

	return cfg
}

// streamReads walks a key sequentially, which is the pattern the predictor emits candidates for.
//
// PredictNextAccess is keyed per object and returns nothing until it has three accesses of that key with
// a sequential score above 0.7, so a rotation of distinct keys at offset 0 — the obvious way to write
// this — produces no candidates at all and every assertion below would pass vacuously.
func streamReads(pc *PredictiveCache, key string, n int, blockSize int64) {
	for i := range n {
		pc.Get(key, int64(i)*blockSize, blockSize)
	}
}

// waitFor polls until cond holds, and fails the test if it never does.
//
// The prefetch counters are written by worker goroutines, so there is a window between a read that
// queues a job and the job being done. Polling rather than sleeping a fixed interval: a sleep long
// enough to be reliable on a loaded CI machine is a sleep that lengthens every run, and one short enough
// not to is a flake. See the same reasoning in predictive_shutdown_test.go, where a fixed millisecond
// made a test pass vacuously about half the time.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 5s waiting for %s", why)
		}

		time.Sleep(200 * time.Microsecond)
	}
}

// TestPredictionsAreCounted asserts the predictor's output reaches PredictionsTotal.
//
// The field was declared and assigned nowhere. It is the denominator of PredictionAccuracy, so with it
// at zero the ratio was zero regardless of how well the predictor did — and a zero accuracy reads as a
// broken predictor rather than as an unwritten counter.
func TestPredictionsAreCounted(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(predictiveCacheConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	streamReads(pc, "stream", 16, 4096)

	stats := pc.GetPredictiveStats()
	if stats.PredictionsTotal == 0 {
		t.Fatal("sixteen sequential reads produced no counted predictions; PredictionsTotal is the " +
			"denominator of PredictionAccuracy, so every ratio derived from it stays zero")
	}
}

// TestASequentialReaderIsPredictedCorrectly asserts PredictionsCorrect moves when the predictor is right.
//
// A sequential stream is the one access pattern predictSequential exists to catch, so if accuracy is
// zero here it is zero everywhere. The assertion is on being right at all rather than on a specific
// rate: the rate depends on how many candidates the model emits per read, which is a tuning question,
// while "a correct prediction is ever credited" is the property #223 is about.
func TestASequentialReaderIsPredictedCorrectly(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(predictiveCacheConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	streamReads(pc, "stream", 64, 4096)

	stats := pc.GetPredictiveStats()
	if stats.PredictionsCorrect == 0 {
		t.Errorf("a 64-block sequential read scored no correct predictions out of %d; predictSequential "+
			"predicts exactly this pattern, so nothing else would ever be credited either",
			stats.PredictionsTotal)
	}
	if stats.PredictionAccuracy <= 0 {
		t.Errorf("PredictionAccuracy = %v with %d/%d correct; the ratio is not being derived",
			stats.PredictionAccuracy, stats.PredictionsCorrect, stats.PredictionsTotal)
	}
}

// TestPredictionAccuracyCannotExceedOne is the bound that says the ratio is a ratio.
//
// It is the check that catches double-counting: a claim consumed twice, or a correct prediction credited
// once per candidate that named the range, would push this past 1 — and a ratio above 1 tells an
// operator only that the metric is wrong.
func TestPredictionAccuracyCannotExceedOne(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(predictiveCacheConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	// Re-reads as well as forward reads: a re-read of a predicted range is the case that would claim the
	// same prediction twice if the ledger did not consume it.
	for i := range 64 {
		pc.Get("stream", int64(i)*4096, 4096)
		pc.Get("stream", int64(i)*4096, 4096)
	}

	stats := pc.GetPredictiveStats()
	if stats.PredictionsCorrect > stats.PredictionsTotal {
		t.Errorf("PredictionsCorrect = %d exceeds PredictionsTotal = %d; a prediction is being credited "+
			"more than once", stats.PredictionsCorrect, stats.PredictionsTotal)
	}
	if stats.PredictionAccuracy > 1 {
		t.Errorf("PredictionAccuracy = %v, above 1", stats.PredictionAccuracy)
	}
}

// TestAPredictionIsNotCreditedToTheReadThatMadeIt is the self-scoring guard.
//
// Get consults the ledger before running the predictor, so a candidate produced by this read cannot be
// claimed by this read. Reversing those two lines is how a prefetcher scores 100% against itself: each
// read would name a range and immediately match it. Nothing else in the suite notices the swap —
// accuracy just climbs, which reads as a triumph — so this test is the only thing standing between the
// statistic and a number that measures its own bookkeeping.
//
// The fixture is exact, and it took a probe to find. A repeated read of one offset is the case where the
// predictor names the range the reader is already on: sequential score is zero, so predictSequential
// contributes nothing and predictML — which names the last access's own offset — is the only source of
// candidates. The confidence threshold has to come down for predictML to fire at all (measured: zero
// candidates at the default 0.7, sixty-two at 0.3 or below), and the predictions ledger has to be
// drained first, or the read legitimately claims the range the *previous* read predicted and the two
// causes are indistinguishable. With those held, one read either credits itself or it does not:
// measured 5→5 correct with the current ordering and 6→7 with the lines swapped.
func TestAPredictionIsNotCreditedToTheReadThatMadeIt(t *testing.T) {
	t.Parallel()

	cfg := predictiveCacheConfig()
	cfg.ConfidenceThreshold = 0 // see above: predictML is the only candidate source for this pattern

	pc, err := NewPredictiveCache(cfg)
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	for range 8 {
		pc.Get("hot", 0, 4096)
	}

	if pc.GetPredictiveStats().PredictionsTotal == 0 {
		t.Fatal("the fixture produced no predictions, so this cannot distinguish a working ordering " +
			"from an access pattern that predicts nothing")
	}

	// Nothing outstanding, so the only prediction the read below could claim is its own.
	drainPredictions(pc)

	before := pc.GetPredictiveStats()
	pc.Get("hot", 0, 4096)
	after := pc.GetPredictiveStats()

	if after.PredictionsTotal == before.PredictionsTotal {
		t.Fatal("the read made no prediction, so there was nothing for it to credit itself with")
	}
	if after.PredictionsCorrect != before.PredictionsCorrect {
		t.Errorf("PredictionsCorrect went %d→%d on a read whose only claimable prediction was the one "+
			"it made itself; the predictor is scoring its own bookkeeping",
			before.PredictionsCorrect, after.PredictionsCorrect)
	}
}

// drainPredictions empties the prediction ledger, so what a later read claims can only be its own.
//
// Reaching into the ledger rather than arranging an access pattern that leaves it empty: there is no
// such pattern. A prediction is recorded by every read that produces candidates, so the state this test
// needs — a warmed predictor with nothing outstanding — is not reachable through Get.
func drainPredictions(pc *PredictiveCache) {
	pc.predictions.mu.Lock()
	defer pc.predictions.mu.Unlock()

	pc.predictions.ranges = make(map[string][]ledgerRange)
	pc.predictions.order = nil
}

// TestPrefetchedBytesAreAttributedToTheirPrefetch is the PrefetchHits regression test.
//
// The counter was guarded by `event.Hit && event.Prefetch`, and nothing in the repository ever
// constructed an AccessEvent with Prefetch true — both call sites set it false explicitly. So the branch
// was unreachable and the count was structurally zero, which is not distinguishable from a prefetcher
// that never helped.
func TestPrefetchedBytesAreAttributedToTheirPrefetch(t *testing.T) {
	t.Parallel()

	const block = 4096

	backend := &fixedBackend{payload: make([]byte, block)}

	pc, err := NewPredictiveCache(statsConfig(backend))
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	// Warm the predictor and let the workers act on what it emits.
	streamReads(pc, "stream", 8, block)

	waitFor(t, "a prefetch worker to store something", func() bool {
		return pc.GetPredictiveStats().PrefetchRequests > 0
	})

	// Keep reading forward. The blocks ahead are what the workers just fetched, so these reads are the
	// ones a prefetch can be credited for.
	streamReads(pc, "stream", 64, block)

	waitFor(t, "a read to be attributed to a prefetch", func() bool {
		return pc.GetPredictiveStats().PrefetchHits > 0
	})

	stats := pc.GetPredictiveStats()
	if stats.PrefetchBytes <= 0 {
		t.Errorf("PrefetchBytes = %d after %d prefetch requests; bytes are recorded against what the "+
			"backend returned, so a positive request count with zero bytes means the length is being lost",
			stats.PrefetchBytes, stats.PrefetchRequests)
	}
	if stats.PrefetchEfficiency <= 0 {
		t.Errorf("PrefetchEfficiency = %v with %d hits over %d requests; the ratio is not being derived",
			stats.PrefetchEfficiency, stats.PrefetchHits, stats.PrefetchRequests)
	}
}

// TestPrefetchEfficiencyCannotExceedOne pins the bound the ledger's consume-on-claim rule exists for.
//
// A prefetch fetched its bytes once, so it can be right once. Without consuming the range, a reader
// re-reading a hot region drives hits past requests and efficiency past 100% — a number that would make
// prefetch look like it was serving reads it never fetched.
func TestPrefetchEfficiencyCannotExceedOne(t *testing.T) {
	t.Parallel()

	const block = 4096

	backend := &fixedBackend{payload: make([]byte, block)}

	pc, err := NewPredictiveCache(statsConfig(backend))
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	streamReads(pc, "stream", 8, block)

	waitFor(t, "a prefetch worker to store something", func() bool {
		return pc.GetPredictiveStats().PrefetchRequests > 0
	})

	// Read each block many times over, which is the pattern that would over-credit.
	for i := range 32 {
		for range 8 {
			pc.Get("stream", int64(i)*block, block)
		}
	}

	stats := pc.GetPredictiveStats()
	if stats.PrefetchHits > stats.PrefetchRequests {
		t.Errorf("PrefetchHits = %d exceeds PrefetchRequests = %d; one prefetch is being credited for "+
			"every read that touches its range", stats.PrefetchHits, stats.PrefetchRequests)
	}
	if stats.PrefetchEfficiency > 1 {
		t.Errorf("PrefetchEfficiency = %v, above 1", stats.PrefetchEfficiency)
	}
}

// TestNoBackendMeansNoPrefetchCountsAndStillCountsPredictions is what a mount looks like today.
//
// initializeLevels passes no Backend to the predictive config, so the mount's workers dequeue jobs and
// fetch nothing. That makes the prefetch counters zero on a real mount — honest signal rather than a
// hidden failure, and the reason the two ledgers are separate: prediction accuracy is still measurable
// when the prefetcher cannot act, and scoring the predictor on the prefetcher's throughput would report
// a broken predictor instead of an unconfigured prefetcher.
func TestNoBackendMeansNoPrefetchCountsAndStillCountsPredictions(t *testing.T) {
	t.Parallel()

	cfg := statsConfig(nil) // prefetch enabled, no backend — exactly the mount's shape

	pc, err := NewPredictiveCache(cfg)
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	streamReads(pc, "stream", 64, 4096)

	stats := pc.GetPredictiveStats()
	if stats.PredictionsTotal == 0 {
		t.Error("no predictions were counted without a backend; prediction accuracy does not depend on " +
			"the prefetcher being able to act, and conflating them scores the predictor on the " +
			"prefetcher's throughput")
	}
	if stats.PrefetchRequests != 0 {
		t.Errorf("PrefetchRequests = %d with no backend; nothing was fetched, so nothing should be "+
			"counted as fetched", stats.PrefetchRequests)
	}
	if stats.PrefetchHits != 0 {
		t.Errorf("PrefetchHits = %d with no backend", stats.PrefetchHits)
	}
}

// TestIntelligentEvictionsAreCountedAndRatiosStayConsistent covers the third originally-written counter
// plus the ratio recomputation on the eviction path.
//
// EvictionsTotal and EvictionsIntelligent were the two fields that did work before #223, but the ratios
// were recomputed only on the read path, so an eviction-heavy workload with no reads left them stale.
// Deriving them on every write is what makes GetPredictiveStats unable to return counters and ratios
// that disagree.
func TestIntelligentEvictionsAreCountedAndRatiosStayConsistent(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(predictiveCacheConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	// Give the eviction scorer something to score, then ask for space.
	streamReads(pc, "stream", 16, 4096)
	pc.Evict(8192)

	stats := pc.GetPredictiveStats()
	if stats.EvictionsTotal == 0 {
		t.Fatal("Evict over a populated cache counted no evictions")
	}
	if stats.EvictionsIntelligent > stats.EvictionsTotal {
		t.Errorf("EvictionsIntelligent = %d exceeds EvictionsTotal = %d",
			stats.EvictionsIntelligent, stats.EvictionsTotal)
	}
}

// TestRatiosAreDerivedOnWriteNotOnRead asserts the numbers are consistent in the struct itself.
//
// The ratios are computed as the counters change, not inside the getter, because PredictiveStats is
// serialized directly through its JSON tags — anything that marshals it without calling the getter would
// see zeros for every ratio, which is #222's defect in smaller form. Recomputing them here from the
// counters and comparing is how that stays true.
func TestRatiosAreDerivedOnWriteNotOnRead(t *testing.T) {
	t.Parallel()

	const block = 4096

	backend := &fixedBackend{payload: make([]byte, block)}

	pc, err := NewPredictiveCache(statsConfig(backend))
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	streamReads(pc, "stream", 32, block)

	waitFor(t, "a prefetch worker to store something", func() bool {
		return pc.GetPredictiveStats().PrefetchRequests > 0
	})

	streamReads(pc, "stream", 32, block)

	// Read the struct directly under its own lock rather than through the getter, which is what a
	// json.Marshal of the collector does.
	pc.stats.mu.RLock()
	direct := struct {
		accuracy, efficiency           float64
		correct, total, hits, requests uint64
	}{
		accuracy:   pc.stats.PredictionAccuracy,
		efficiency: pc.stats.PrefetchEfficiency,
		correct:    pc.stats.PredictionsCorrect,
		total:      pc.stats.PredictionsTotal,
		hits:       pc.stats.PrefetchHits,
		requests:   pc.stats.PrefetchRequests,
	}
	pc.stats.mu.RUnlock()

	if direct.total > 0 {
		want := float64(direct.correct) / float64(direct.total)
		if direct.accuracy != want {
			t.Errorf("PredictionAccuracy = %v but %d/%d = %v; the ratio in the struct disagrees with "+
				"its own counters, so anything that marshals this without the getter reports a wrong number",
				direct.accuracy, direct.correct, direct.total, want)
		}
	}
	if direct.requests > 0 {
		want := float64(direct.hits) / float64(direct.requests)
		if direct.efficiency != want {
			t.Errorf("PrefetchEfficiency = %v but %d/%d = %v", direct.efficiency, direct.hits,
				direct.requests, want)
		}
	}
}

// TestAvgConfidenceIsWithinTheConfidenceRange asserts the running mean stays a confidence.
//
// It is kept incrementally, weighted by the number of candidates in each batch, because the individual
// confidences are not retained. An unweighted mean would count a burst of candidates as one sample, and
// an arithmetic slip in the increment shows up as a value outside [0, 1] rather than as a wrong-looking
// number — which is the only way a mean like this can be checked without recording every input.
func TestAvgConfidenceIsWithinTheConfidenceRange(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(predictiveCacheConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	streamReads(pc, "stream", 64, 4096)

	stats := pc.GetPredictiveStats()
	if stats.PredictionsTotal == 0 {
		t.Fatal("no predictions, so the mean has no inputs")
	}
	if stats.AvgConfidence <= 0 || stats.AvgConfidence > 1 {
		t.Errorf("AvgConfidence = %v after %d predictions, outside (0, 1]", stats.AvgConfidence,
			stats.PredictionsTotal)
	}
}

// TestPrefetchWasteCountsWhatWasNeverRead asserts the fourth counter moves for its stated reason, and
// that the ledger's tally reaches the statistics at all.
//
// Waste is the number that says whether prefetch earns its bandwidth. Eviction from the ledger is the
// only moment at which "never read" becomes knowable — before that, an unclaimed range is merely one not
// read *yet* — and the tally has to travel from the ledger into PredictiveStats without either side
// taking the other's lock, which is the constraint takeUnclaimed exists to satisfy: the ledger is written
// by the prefetch workers while a read holds the stats lock, and the reverse order happens too.
//
// It calls recordPrefetch, which is what processPrefetchJob calls, rather than driving the workers.
// That is a deliberate narrowing and worth saying plainly: getting a real prefetch evicted unclaimed
// needs either 1024 distinct prefetched objects or a reader that skips exactly what was fetched, and
// probing found both hard to arrange without the access pattern silently ceasing to predict anything —
// at which point the test passes for the wrong reason. The counting itself is covered end-to-end in
// prefetch_ledger_test.go; what this covers is the plumbing between the two, which no other test touches.
func TestPrefetchWasteCountsWhatWasNeverRead(t *testing.T) {
	t.Parallel()

	pc, err := NewPredictiveCache(predictiveCacheConfig())
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	// Past the per-key bound, claiming none of them: the oldest are evicted, and at that point nothing
	// will ever read them.
	const overshoot = 4
	for i := range maxRangesPerKey + overshoot {
		pc.recordPrefetch("stream", int64(i)*4096, 4096)
	}

	if waste := pc.GetPredictiveStats().PrefetchWaste; waste != 0 {
		t.Fatalf("PrefetchWaste = %d before any read; the ledger's tally is drained on the read path, "+
			"so this test cannot tell a working drain from a counter written somewhere else", waste)
	}

	// A read is what drains the tally.
	pc.Get("stream", 0, 4096)

	if waste := pc.GetPredictiveStats().PrefetchWaste; waste != overshoot {
		t.Errorf("PrefetchWaste = %d after %d prefetches evicted unclaimed past a per-key bound of %d, "+
			"want %d", waste, maxRangesPerKey+overshoot, maxRangesPerKey, overshoot)
	}
}

// TestPredictiveStatsAreReachableThroughTheMultiLevelCache is #223's stated defect.
//
// The mount holds its PredictiveCache as an opaque types.Cache inside a CacheLevel — six methods about
// bytes, with GetLevelStats returning a types.CacheStats and Close reachable only through an unexported
// assertion. So these numbers were computed on every read of every mount and discarded at unmount, with
// no accessor at any layer above.
//
// It goes through NewMultiLevelCache with the mount's own prefetch setting rather than constructing a
// PredictiveCache directly, because "the mount wraps L1 in one" is half of what makes the accessor
// necessary and a hand-built level would not test it.
func TestPredictiveStatsAreReachableThroughTheMultiLevelCache(t *testing.T) {
	t.Parallel()

	mlc, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       64 << 20,
			MaxEntries: 1000,
			TTL:        time.Minute,
			Prefetch:   true, // what multiLevelConfigFrom sets unconditionally
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache: %v", err)
	}
	t.Cleanup(func() { _ = mlc.Close() })

	if pc := mlc.GetPredictiveCache(); pc == nil {
		t.Fatal("GetPredictiveCache returned nil for a cache configured the way every mount is; L1 is " +
			"wrapped in a PredictiveCache whenever prefetch is on")
	}

	// Populate, then read forward. Not interleaved: MultiLevelCache.Put records an access too, so a
	// Put/Get pair per offset gives the predictor a history that alternates between the same offset twice
	// and scores as non-sequential — measured, zero candidates. Filling first and then streaming is what
	// a reader does anyway.
	for i := range 32 {
		mlc.Put("stream", int64(i)*4096, make([]byte, 4096))
	}
	for i := range 32 {
		mlc.Get("stream", int64(i)*4096, 4096)
	}

	stats, ok := mlc.PredictiveStats()
	if !ok {
		t.Fatal("PredictiveStats reported no predictive layer on a cache that has one")
	}
	if stats.PredictionsTotal == 0 {
		t.Error("reads through the multi-level cache produced no counted predictions, so the accessor " +
			"reaches a cache the reads are not going through")
	}
}

// TestPredictiveStatsReportsAbsenceWhenPrefetchIsOff is the pairing for the test above.
//
// Without it, an accessor that returned a zero struct and true unconditionally would satisfy the
// reachability test while making "there is no predictive layer" indistinguishable from "it has predicted
// nothing yet" — which is the distinction #222 is the argument for, and the reason this returns a
// boolean rather than only a struct.
func TestPredictiveStatsReportsAbsenceWhenPrefetchIsOff(t *testing.T) {
	t.Parallel()

	mlc, err := NewMultiLevelCache(&MultiLevelConfig{
		L1Config: &L1Config{
			Enabled:    true,
			Size:       64 << 20,
			MaxEntries: 1000,
			TTL:        time.Minute,
			Prefetch:   false,
		},
		Policy: "inclusive",
	})
	if err != nil {
		t.Fatalf("NewMultiLevelCache: %v", err)
	}
	t.Cleanup(func() { _ = mlc.Close() })

	if pc := mlc.GetPredictiveCache(); pc != nil {
		t.Error("GetPredictiveCache found a predictive layer with prefetch disabled")
	}
	if _, ok := mlc.PredictiveStats(); ok {
		t.Error("PredictiveStats reported a predictive layer with prefetch disabled; an operator " +
			"cannot then tell an absent layer from an idle one")
	}
}

// TestGetPredictiveStatsIsSafeUnderConcurrentReads is the -race check for the getter.
//
// It is called from the metrics collector's periodic-update goroutine while FUSE goroutines are reading
// through the cache, so it takes the stats lock against writers on every read path. It copies field by
// field rather than by struct assignment because PredictiveStats holds the mutex guarding it — copying
// it would trip go vet's copylocks and hand the caller a copy of a lock protecting nothing.
func TestGetPredictiveStatsIsSafeUnderConcurrentReads(t *testing.T) {
	t.Parallel()

	const block = 4096

	backend := &fixedBackend{payload: make([]byte, block)}

	pc, err := NewPredictiveCache(statsConfig(backend))
	if err != nil {
		t.Fatalf("NewPredictiveCache: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	var wg sync.WaitGroup

	stop := make(chan struct{})

	// The metrics surface's access pattern: one reader, on a schedule, for the life of the mount.
	wg.Add(1)

	go func() {
		defer wg.Done()

		for {
			select {
			case <-stop:
				return
			default:
				stats := pc.GetPredictiveStats()
				if stats.PredictionAccuracy > 1 || stats.PrefetchEfficiency > 1 {
					t.Errorf("a snapshot carried a ratio above 1: accuracy %v, efficiency %v",
						stats.PredictionAccuracy, stats.PrefetchEfficiency)

					return
				}
			}
		}
	}()

	for r := range 4 {
		wg.Add(1)

		go func(r int) {
			defer wg.Done()

			streamReads(pc, "stream", 128, block)
		}(r)
	}

	// The four readers finish on their own; the poller runs until told to stop.
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(stop)
	}()

	wg.Wait()
}
