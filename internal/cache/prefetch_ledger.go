package cache

import "sync"

// rangeLedger records ranges the cache has spoken for — bytes a prefetch worker stored, or bytes the
// predictor said would be read next — so that a later read can be attributed to one of them.
//
// It exists because attribution cannot be recovered after the fact. A cache hit looks identical whether
// the bytes arrived from the application's own earlier read or from a prefetch, and the predictor's
// accuracy is unknowable unless what it predicted is remembered until a read either matches it or
// displaces it. This is the memory that makes [PredictiveStats]'s ratios computable at all, which is why
// they were zero before it existed (#223).
//
// Bounded by construction: at most maxLedgerKeys keys and maxRangesPerKey ranges per key, both evicted
// oldest-first. An unbounded ledger on a filesystem cache is a leak with a slow fuse — it would grow
// with the number of distinct objects a mount ever touched, which for a research dataset is the whole
// bucket. Eviction is also where an unclaimed range is finally counted as waste: at that point nothing
// will read it, so the prefetch that fetched it is known to have been wrong.
const (
	maxLedgerKeys    = 1024
	maxRangesPerKey  = 16
	ledgerTrimTarget = maxLedgerKeys * 3 / 4 // how far back a trim goes, so it is not run on every insert
)

// ledgerRange is a byte range this cache spoke for.
type ledgerRange struct {
	offset int64
	length int64
}

// covers reports whether the range wholly contains [offset, offset+size).
//
// Containment, not overlap. A read half-covered by a prefetch was half-served by it, and there is no
// honest way to score that as a hit — the other half was a base-cache hit or a miss the layer above
// resolved. Requiring containment makes both PrefetchHits and PredictionsCorrect undercount rather than
// overcount, which is the direction that does not flatter the prefetcher.
func (r ledgerRange) covers(offset, size int64) bool {
	return offset >= r.offset && offset+size <= r.offset+r.length
}

// rangeLedger is safe for concurrent use: every prefetch worker records into it and every read consults
// it.
type rangeLedger struct {
	mu sync.Mutex

	ranges map[string][]ledgerRange

	// order is insertion order over the keys of ranges, for eviction. A slice rather than a
	// container/list because the whole thing is at most maxLedgerKeys entries and a trim is amortized
	// across ledgerTrimTarget inserts.
	order []string

	// unclaimed counts ranges evicted without ever being claimed. Read by the caller and folded into
	// PrefetchWaste; kept here because eviction is the only moment at which "never read" becomes known.
	unclaimed uint64
}

func newRangeLedger() *rangeLedger {
	return &rangeLedger{ranges: make(map[string][]ledgerRange)}
}

// record adds a range for key, evicting to stay within bounds.
func (l *rangeLedger) record(key string, offset, length int64) {
	if length <= 0 {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	existing, seen := l.ranges[key]
	if !seen {
		l.order = append(l.order, key)
	}

	// Oldest range for this key goes first once the per-key bound is reached. A reader moving through a
	// large object generates a range per prefetch, and only the recent ones can still be claimed by a
	// read that has not happened yet.
	if len(existing) >= maxRangesPerKey {
		if !existing[0].claimed() {
			l.unclaimed++
		}
		existing = existing[1:]
	}

	l.ranges[key] = append(existing, ledgerRange{offset: offset, length: length})

	if len(l.order) > maxLedgerKeys {
		l.trimLocked()
	}
}

// claim reports whether a recorded range covers the read, and consumes the range if one does.
//
// Consuming it is what keeps a single prefetch from being credited for every read that touches it: the
// prefetch fetched those bytes once, so it can be right once. Without that, a reader re-reading a hot
// range would drive PrefetchHits above PrefetchRequests and the efficiency ratio past 100%.
func (l *rangeLedger) claim(key string, offset, size int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	ranges := l.ranges[key]
	for i := range ranges {
		if ranges[i].claimed() || !ranges[i].covers(offset, size) {
			continue
		}

		ranges[i].length = 0 // marks it claimed; see ledgerRange.claimed
		l.ranges[key] = ranges

		return true
	}

	return false
}

// claimed reports whether this range has already been attributed to a read.
//
// Zero length is the marker rather than a separate bool, because record rejects a non-positive length, so
// no unclaimed range can have one. That keeps ledgerRange two words and the ledger's footprint at
// maxLedgerKeys × maxRangesPerKey × 16 bytes — 256 KiB at the bounds above.
func (r ledgerRange) claimed() bool {
	return r.length == 0
}

// takeUnclaimed returns the number of ranges evicted without being claimed since the last call, and
// resets the counter.
func (l *rangeLedger) takeUnclaimed() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	n := l.unclaimed
	l.unclaimed = 0

	return n
}

// trimLocked drops the oldest keys down to ledgerTrimTarget. Caller holds mu.
func (l *rangeLedger) trimLocked() {
	drop := len(l.order) - ledgerTrimTarget
	for _, key := range l.order[:drop] {
		for _, r := range l.ranges[key] {
			if !r.claimed() {
				l.unclaimed++
			}
		}
		delete(l.ranges, key)
	}

	// Copied rather than resliced: l.order[drop:] keeps the dropped keys alive in the backing array, which
	// on a mount that touches many distinct objects is the leak this bound exists to prevent.
	l.order = append([]string(nil), l.order[drop:]...)
}
