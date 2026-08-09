package cache

import (
	"fmt"
	"sync"
	"testing"
)

// The ledger is what makes attribution possible, so these tests are about the properties the
// statistics rest on: a range is claimed at most once, a partial cover is not a claim, and the bounds
// hold under a workload that touches far more keys than the ledger can hold (#223).

// TestLedgerClaimsARecordedRange is the base case: a read inside a recorded range is attributed to it.
func TestLedgerClaimsARecordedRange(t *testing.T) {
	t.Parallel()

	l := newRangeLedger()
	l.record("data/x", 0, 4096)

	if !l.claim("data/x", 0, 4096) {
		t.Error("a read of exactly the recorded range was not attributed to it")
	}
}

// TestLedgerClaimsOnlyOnce is the property that keeps PrefetchEfficiency at or below 100%.
//
// A prefetch fetched those bytes once, so it can be right once. Without consuming the range, a reader
// re-reading a hot region would drive PrefetchHits past PrefetchRequests and the ratio past 1.0 — a
// number that tells an operator nothing except that the metric is wrong.
func TestLedgerClaimsOnlyOnce(t *testing.T) {
	t.Parallel()

	l := newRangeLedger()
	l.record("data/x", 0, 4096)

	if !l.claim("data/x", 0, 4096) {
		t.Fatal("the first claim failed, so what follows cannot distinguish consuming from never matching")
	}

	for i := range 4 {
		if l.claim("data/x", 0, 4096) {
			t.Errorf("re-read %d claimed the same range again; one prefetch would be credited for every "+
				"read that touches it, and efficiency would exceed 1.0", i+2)
		}
	}
}

// TestLedgerRequiresContainmentNotOverlap pins the direction the arithmetic errs in.
//
// A read half-covered by a prefetch was half-served by it, and there is no honest way to score that as
// a hit: the other half came from somewhere else. Requiring containment makes the counters undercount
// rather than overcount, which is the direction that does not flatter the prefetcher.
func TestLedgerRequiresContainmentNotOverlap(t *testing.T) {
	t.Parallel()

	// Each case overlaps [1024, 2048) without being contained by it.
	cases := []struct {
		name         string
		offset, size int64
	}{
		{name: "starts before", offset: 512, size: 1024},
		{name: "extends past the end", offset: 1536, size: 1024},
		{name: "strictly larger", offset: 0, size: 4096},
		{name: "adjacent after", offset: 2048, size: 512},
		{name: "adjacent before", offset: 512, size: 512},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l := newRangeLedger()
			l.record("data/x", 1024, 1024)

			if l.claim("data/x", tc.offset, tc.size) {
				t.Errorf("a read of [%d, %d) claimed the recorded [1024, 2048), which does not contain it",
					tc.offset, tc.offset+tc.size)
			}
		})
	}
}

// TestLedgerClaimsASubrange is the containment rule's other half: a read inside the range does count.
//
// Worth its own test because the strict version of containment — requiring the offsets to match — would
// pass every case above while scoring almost every real read as a miss. A prefetch of 1 MiB serves the
// 128 KiB reads the kernel actually issues, and that is the case that has to be credited.
func TestLedgerClaimsASubrange(t *testing.T) {
	t.Parallel()

	l := newRangeLedger()
	l.record("data/x", 0, 1<<20)

	if !l.claim("data/x", 512<<10, 128<<10) {
		t.Error("a 128 KiB read inside a 1 MiB prefetch was not attributed to it; every kernel-sized " +
			"read of a prefetched object would score as a miss")
	}
}

// TestLedgerKeysDoNotCrossOver asserts a range recorded for one object cannot be claimed by another.
//
// The keys are object paths, and a ledger that ignored them would credit any read at a matching offset
// — which on a filesystem where every object starts at offset 0 is most of them.
func TestLedgerKeysDoNotCrossOver(t *testing.T) {
	t.Parallel()

	l := newRangeLedger()
	l.record("data/x", 0, 4096)

	if l.claim("data/y", 0, 4096) {
		t.Error("a read of data/y claimed a range recorded for data/x")
	}
	if !l.claim("data/x", 0, 4096) {
		t.Error("the range for data/x was consumed by the read of data/y")
	}
}

// TestLedgerRejectsANonPositiveLength guards the marker that says "claimed".
//
// A zero length is how a claimed range is marked, so recording one inserts an entry indistinguishable
// from a consumed one; a negative length is worse, since it can never cover any read and is therefore
// counted as waste the moment it is evicted. A short or failed backend read is exactly where both come
// from.
//
// The assertions have to go past "is it claimable", because a zero-length entry is unclaimable either
// way — that is what makes it invisible. What it does instead is occupy a slot under the per-key bound,
// so the two observable consequences are a real range evicted early and, for a negative length, a
// phantom count of waste. Both are checked, because a test of only the first left dropping the guard
// entirely undetected.
func TestLedgerRejectsANonPositiveLength(t *testing.T) {
	t.Parallel()

	for _, length := range []int64{0, -1, -4096} {
		l := newRangeLedger()
		l.record("data/x", 0, length)

		if l.claim("data/x", 0, 0) {
			t.Errorf("recording a length of %d produced a claimable range; zero length is the "+
				"already-claimed marker, so such an entry is indistinguishable from a consumed one", length)
		}

		// Exactly the per-key bound in real ranges. If the bad entry took a slot, the first of these is
		// evicted to make room and the ledger has forgotten a range a read could still have claimed.
		for i := range maxRangesPerKey {
			l.record("data/x", int64(i+1)*4096, 4096)
		}

		for i := range maxRangesPerKey {
			if !l.claim("data/x", int64(i+1)*4096, 4096) {
				t.Errorf("after recording a length of %d, real range %d of %d is gone: the rejected entry "+
					"took a slot under the per-key bound and pushed out a claimable range",
					length, i, maxRangesPerKey)

				break
			}
		}

		if n := l.takeUnclaimed(); n != 0 {
			t.Errorf("recording a length of %d counted %d unclaimed range(s) as waste; a length no read "+
				"can ever cover would be charged to the prefetcher as bandwidth it wasted", length, n)
		}
	}
}

// TestLedgerBoundsRangesPerKey asserts a single object cannot grow an unbounded ledger.
//
// A reader streaming a large file generates one range per prefetch, and only the recent ones can still
// be claimed by a read that has not happened yet. The oldest goes first, so the entries kept are the
// ones with a future.
func TestLedgerBoundsRangesPerKey(t *testing.T) {
	t.Parallel()

	l := newRangeLedger()

	const overshoot = 4
	for i := range maxRangesPerKey + overshoot {
		l.record("stream", int64(i)*4096, 4096)
	}

	l.mu.Lock()
	held := len(l.ranges["stream"])
	l.mu.Unlock()

	if held > maxRangesPerKey {
		t.Errorf("the ledger holds %d ranges for one key, above the bound of %d", held, maxRangesPerKey)
	}

	// The evicted ones are the oldest, and they were never claimed, so they are waste.
	if n := l.takeUnclaimed(); n != overshoot {
		t.Errorf("takeUnclaimed = %d, want %d — an evicted range that was never read is a prefetch "+
			"known to have been wrong, and eviction is the only moment that becomes knowable", n, overshoot)
	}

	// The newest range survives; the oldest does not.
	if !l.claim("stream", int64(maxRangesPerKey+overshoot-1)*4096, 4096) {
		t.Error("the most recently recorded range is gone, so eviction dropped the wrong end")
	}
	if l.claim("stream", 0, 4096) {
		t.Error("the oldest range survived past the per-key bound")
	}
}

// TestLedgerBoundsKeys asserts the whole-ledger bound holds against a mount that touches many objects.
//
// This is the leak the bound exists to prevent: without it the ledger grows with the number of distinct
// objects a mount ever read, which for a research dataset is the whole bucket.
func TestLedgerBoundsKeys(t *testing.T) {
	t.Parallel()

	l := newRangeLedger()

	for i := range maxLedgerKeys * 3 {
		l.record(fmt.Sprintf("obj/%d", i), 0, 4096)
	}

	l.mu.Lock()
	keys, order := len(l.ranges), len(l.order)
	l.mu.Unlock()

	if keys > maxLedgerKeys {
		t.Errorf("the ledger holds %d keys, above the bound of %d", keys, maxLedgerKeys)
	}

	// order and ranges must agree, or a trim walks keys that are no longer there — and the order slice
	// becomes the unbounded thing the map is not.
	if order != keys {
		t.Errorf("order has %d entries and ranges has %d; a trim iterates order, so a disagreement "+
			"means either stale keys are walked or live ones are never evicted", order, keys)
	}

	// The most recent key survives a trim; one from the first batch does not.
	if !l.claim(fmt.Sprintf("obj/%d", maxLedgerKeys*3-1), 0, 4096) {
		t.Error("the most recently recorded key was trimmed, so the trim drops the wrong end")
	}
	if l.claim("obj/0", 0, 4096) {
		t.Error("the first key recorded survived 3× the key bound")
	}
}

// TestLedgerCountsTrimmedRangesAsUnclaimed asserts waste is counted when a whole key is trimmed, not
// just when a single range is pushed out.
//
// Two eviction paths reach the same conclusion — "nothing will ever read this" — and only one of them
// was obvious. A trim that forgot to count would make PrefetchWaste understate by however much the
// ledger's key pressure evicted, which on a mount that touches many objects is most of it.
func TestLedgerCountsTrimmedRangesAsUnclaimed(t *testing.T) {
	t.Parallel()

	l := newRangeLedger()

	// Two ranges per key, so the count is not confusable with a per-key count.
	for i := range maxLedgerKeys + 1 {
		key := fmt.Sprintf("obj/%d", i)
		l.record(key, 0, 4096)
		l.record(key, 4096, 4096)
	}

	// One trim happened, dropping down to ledgerTrimTarget, at two unclaimed ranges per key.
	wantKeys := maxLedgerKeys + 1 - ledgerTrimTarget
	if n := l.takeUnclaimed(); n != uint64(wantKeys*2) {
		t.Errorf("takeUnclaimed = %d after a trim of %d keys holding two ranges each, want %d",
			n, wantKeys, wantKeys*2)
	}
}

// TestLedgerDoesNotCountAClaimedRangeAsWaste is the pairing for the two tests above.
//
// Waste means fetched and never read. A range that was read and then evicted is a prefetch that
// worked, and counting it would make waste indistinguishable from throughput.
func TestLedgerDoesNotCountAClaimedRangeAsWaste(t *testing.T) {
	t.Parallel()

	l := newRangeLedger()

	// Fill one key past its bound, claiming every range as it goes.
	for i := range maxRangesPerKey + 4 {
		offset := int64(i) * 4096
		l.record("stream", offset, 4096)

		if !l.claim("stream", offset, 4096) {
			t.Fatalf("claim of range %d failed; the fixture is wrong, not the ledger", i)
		}
	}

	if n := l.takeUnclaimed(); n != 0 {
		t.Errorf("takeUnclaimed = %d with every range claimed before eviction; waste must mean "+
			"fetched-and-never-read, or it cannot answer whether prefetch earns its bandwidth", n)
	}
}

// TestLedgerTakeUnclaimedDrains asserts the counter is consumed, not accumulated twice.
//
// The caller folds the return value into a running total, so a takeUnclaimed that left the count in
// place would add it again on every read — turning PrefetchWaste into something that grows with read
// volume rather than with wasted prefetches.
func TestLedgerTakeUnclaimedDrains(t *testing.T) {
	t.Parallel()

	l := newRangeLedger()
	for i := range maxRangesPerKey + 2 {
		l.record("stream", int64(i)*4096, 4096)
	}

	first := l.takeUnclaimed()
	if first == 0 {
		t.Fatal("no waste was counted, so the drain cannot be observed")
	}
	if second := l.takeUnclaimed(); second != 0 {
		t.Errorf("a second takeUnclaimed returned %d; the count is not drained, so the caller's "+
			"running total would grow on every read", second)
	}
}

// TestLedgerIsSafeUnderConcurrentUse is the -race check for the access pattern it actually sees.
//
// Every prefetch worker records into the ledger while every read consults it, which is four writers
// against N readers on the live path. This is not a hypothetical: the ledger is the one piece of #223's
// state written from the prefetch goroutines and read from the FUSE ones.
func TestLedgerIsSafeUnderConcurrentUse(t *testing.T) {
	t.Parallel()

	l := newRangeLedger()

	var wg sync.WaitGroup

	for w := range 4 {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			key := fmt.Sprintf("stream/%d", w)
			for i := range 256 {
				l.record(key, int64(i)*4096, 4096)
			}
		}(w)
	}

	for r := range 4 {
		wg.Add(1)

		go func(r int) {
			defer wg.Done()

			key := fmt.Sprintf("stream/%d", r)
			for i := range 256 {
				l.claim(key, int64(i)*4096, 4096)
				l.takeUnclaimed()
			}
		}(r)
	}

	wg.Wait()
}
