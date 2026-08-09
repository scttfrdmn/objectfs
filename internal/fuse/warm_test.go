//go:build linux || darwin

package fuse

// These tests cover what a cache miss does with a peer's claim (#142): read further, from S3, on the
// strength of another node holding more of the object than this read asked for.
//
// # Two kinds of assertion, and why both are here
//
// Most of them read the prefetch queue directly, with no workers draining it. That is the shape
// read_path_test.go arrived at for the same reason — a prefetch worker issues asynchronous GETs, so an
// assertion made against request counts while one is running depends on goroutine scheduling, which is
// how TestPrefetchSkipsRangeTheReaderHasAlreadyRead came to pass 28 times locally and fail on CI. What
// the queue holds is exactly what warmFromPeers decided, with the arithmetic visible and nothing racing.
//
// But a queued request is a decision, not an effect, and a decision nothing acts on is worth nothing. So
// TestWarmingReallyFetchesTheRange runs a real worker and asserts on observed GETs and on bytes landing
// in the cache, which is the property the feature exists for. Queue assertions alone would pass against
// a `warm` that queued onto a channel no worker read.
//
// # The coordinator is the only mock, as in coordinate_test.go
//
// A peer's claim is an input to this path, and producing one from a real second node would mean two
// gossip sockets, an announcement, and a wait — moving the assertion away from *which range this node
// decides to warm* and onto internal/distributed's delivery, which its own tests cover. The metadata
// cache is deliberately real, because the ETag every decision here turns on comes from it.

import (
	"errors"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// warmFixture is a coordFixture whose prefetch queue nothing drains.
//
// ConcurrentReads: 0 is what makes the tests below deterministic: schedulePrefetch fills the queue and
// no worker empties it, so every request in it was put there by the read under test and can be read back
// as a fact rather than caught in flight.
type warmFixture struct {
	*coordFixture
}

func newWarmFixture(t *testing.T) *warmFixture {
	t.Helper()

	f := newCoordFixture(t)

	f.fs.readAhead = NewReadAheadManager(t.Context(), f.fs, &ReadAheadConfig{
		Enabled:         true,
		WindowSize:      64 << 10,
		MinSequential:   3,
		ConcurrentReads: 0,
		TTL:             time.Minute,
	})

	return &warmFixture{coordFixture: f}
}

// queued drains the prefetch queue and returns what was in it.
func (f *warmFixture) queued() []PrefetchRequest {
	var reqs []PrefetchRequest

	for {
		select {
		case req := <-f.fs.readAhead.prefetchQueue:
			reqs = append(reqs, *req)
		default:
			return reqs
		}
	}
}

// warmed returns the single queued request, failing if there is not exactly one.
//
// Exactly one, because a read that queues two prefetches for one miss is the failure mode this feature
// could plausibly have: warmFromPeers and the read pattern both feed the same queue, and a test that
// picked the first matching entry would not notice the detector's own prefetch being counted as the warm.
// Every test here reads at offset 0 of a fresh pattern, where sequentialHits reaches 1 against a
// MinSequential of 3, so the detector queues nothing and the warm is the only entry there should be.
func (f *warmFixture) warmed(t *testing.T) PrefetchRequest {
	t.Helper()

	reqs := f.queued()
	if len(reqs) != 1 {
		t.Fatalf("the read queued %d prefetch requests, want exactly 1 (the warm): %+v", len(reqs), reqs)
	}

	return reqs[0]
}

// holder builds a peer's claim to hold [off, off+length) of key at etag.
func holder(key, etag string, size, off, length int64) types.KeyAnnouncement {
	return types.KeyAnnouncement{
		Key:      key,
		NodeID:   "peer-1",
		ETag:     etag,
		Size:     size,
		CachedAt: time.Now(),
		Offset:   off,
		Length:   length,
	}
}

// etagOf returns the version the store holds for key, which is the version the metadata cache will carry
// once the object has been stat'ed.
func etagOf(t *testing.T, f *coordFixture, key string) string {
	t.Helper()

	info, err := f.fs.backend.HeadObject(t.Context(), key)
	if err != nil {
		t.Fatalf("HeadObject(%q): %v", key, err)
	}

	if info.ETag == "" {
		t.Fatalf("the store reports no ETag for %q, so no claim about it can match", key)
	}

	return info.ETag
}

// TestReadWarmsPastWhatAPeerHolds is #142's headline: a miss on a key some peer already caches reads
// further than the application asked for, up to the end of the range that peer claims.
//
// The arithmetic is the assertion. The warm starts where the read ended — not at the read's own offset,
// which would re-fetch bytes just cached — and ends where the holder's range does, because that is the
// extent of the evidence. Both halves have a plausible wrong answer: starting at `off` duplicates the
// read, and taking the holder's Length as the size ignores where its range begins.
func TestReadWarmsPastWhatAPeerHolds(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)

	const (
		key      = "hot.dat"
		size     = 1 << 20
		readSize = 128 << 10
		peerEnd  = 512 << 10
	)

	f.srv.SeedRandom(key, size)
	f.stat(t, key)

	f.coord.queryResults = []types.KeyAnnouncement{holder(key, etagOf(t, f.coordFixture, key), size, 0, peerEnd)}

	if got := f.read(t, f.open(key), 0, readSize); len(got) != readSize {
		t.Fatalf("read %d bytes, want %d", len(got), readSize)
	}

	if queries := f.coord.queries(); len(queries) == 0 {
		t.Fatal("a cache miss asked no peer who holds the key, so a cold node can never learn what is hot " +
			"in the cluster")
	}

	req := f.warmed(t)

	if req.path != key {
		t.Errorf("warmed path %q, want %q", req.path, key)
	}

	if req.offset != readSize {
		t.Errorf("warmed from offset %d, want %d — the bytes below that were just read and cached, so a "+
			"warm starting at the read's own offset pays for them a second time", req.offset, readSize)
	}

	if want := int64(peerEnd - readSize); req.size != want {
		t.Errorf("warmed %d bytes, want %d: the evidence is a peer's range ending at %d, and the read "+
			"already covered up to %d. A size of %d would be the holder's Length, which ignores where its "+
			"range starts", req.size, want, peerEnd, readSize, peerEnd)
	}
}

// TestWarmingDoesNotDisturbTheReadPattern is the separation [ReadAheadManager.warm] promises.
//
// The pattern models *this* process's sequential behavior and decides whether it looks sequential
// enough to prefetch for. A warm folded into it would let another node's reads move this reader's
// predicted-next offset, and the visible consequence would be the detector prefetching from the wrong
// place for the rest of the traversal — a wrong offset for reasons nothing in the read history explains.
func TestWarmingDoesNotDisturbTheReadPattern(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)

	const (
		key      = "pattern.dat"
		size     = 1 << 20
		readSize = 128 << 10
	)

	f.srv.SeedRandom(key, size)
	f.stat(t, key)

	f.coord.queryResults = []types.KeyAnnouncement{
		holder(key, etagOf(t, f.coordFixture, key), size, 0, 512<<10),
	}

	f.read(t, f.open(key), 0, readSize)

	if req := f.warmed(t); req.size == 0 {
		t.Fatal("nothing was warmed, so this test is not measuring what a warm does to the pattern")
	}

	f.fs.readAhead.mu.RLock()
	pattern := f.fs.readAhead.activeReads[key]
	f.fs.readAhead.mu.RUnlock()

	if pattern == nil {
		t.Fatal("the read left no pattern at all")
	}

	if pattern.predictedNext != readSize {
		t.Errorf("after one %d-byte read the detector predicts the reader is at %d, want %d. A warm has "+
			"moved the prediction, so the next prefetch is aimed at a range chosen by another node's reads",
			readSize, pattern.predictedNext, readSize)
	}

	if pattern.sequentialHits != 1 {
		t.Errorf("sequentialHits is %d after one read, want 1: a warm counted as a read makes this reader "+
			"look sequential on another node's evidence", pattern.sequentialHits)
	}
}

// TestWarmingIgnoresAClaimAboutAnotherVersion is the fail-closed rule, and it is the assertion that
// matters most here.
//
// A holder's range describes the version *it* cached. If that is not the version this node is reading,
// the range says nothing about the object in hand — an overwrite can change its length entirely — so
// warming on it fetches bytes chosen by a claim about something else. Skipping costs exactly the read
// the application asked for, which is what a single-node mount pays.
func TestWarmingIgnoresAClaimAboutAnotherVersion(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)

	const (
		key  = "stale-claim.dat"
		size = 1 << 20
	)

	f.srv.SeedRandom(key, size)
	f.stat(t, key)

	f.coord.queryResults = []types.KeyAnnouncement{
		holder(key, `"0000000000000000000000000000000a"`, size, 0, 512<<10),
	}

	f.read(t, f.open(key), 0, 128<<10)

	if reqs := f.queued(); len(reqs) != 0 {
		t.Errorf("warmed %+v on a claim about a different version of the object. The holder's range "+
			"describes the bytes it cached; against a version this node is not reading, it does not even "+
			"establish that the object is that long", reqs)
	}
}

// TestWarmingNeedsAVersionOfItsOwn is the same rule from the other side: this node not knowing what it is
// reading.
//
// [FileHandle.Read] does not stat, and a handle outlives its metadata cache entry — the two entries have
// separate TTLs — so a read with no cached version is ordinary rather than contrived. With nothing to
// compare a claim against, every claim is unverifiable, and the honest answer is to warm nothing.
func TestWarmingNeedsAVersionOfItsOwn(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)

	const (
		key  = "unstatted-warm.dat"
		size = 1 << 20
	)

	f.srv.SeedRandom(key, size)

	// The peer's claim carries the real version. What is missing is this node's, since nothing has stat'ed
	// the object — so a match cannot be established even though one exists.
	f.coord.queryResults = []types.KeyAnnouncement{
		holder(key, etagOf(t, f.coordFixture, key), size, 0, 512<<10),
	}

	f.read(t, f.open(key), 0, 128<<10)

	if reqs := f.queued(); len(reqs) != 0 {
		t.Errorf("warmed %+v with no version known for the object this node is reading; a claim that "+
			"cannot be checked is not evidence", reqs)
	}
}

// TestWarmingTakesTheFurthestHolderOfTheRightVersion covers the loop over holders rather than its first
// entry.
//
// Holders arrive freshest first, and freshest is not furthest: the most recent claim can be a 4 KiB range
// while an older one covers the whole object. So every claim at the matching version is considered and
// the one reaching furthest wins. An implementation that stopped at holders[0] would pass every other
// test in this file.
//
// The wrong-version claim reaching furthest of all is in the list on purpose: it is what a first-match
// implementation, or one that checked the ETag only after choosing, would act on.
func TestWarmingTakesTheFurthestHolderOfTheRightVersion(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)

	const (
		key      = "many-holders.dat"
		size     = 4 << 20
		readSize = 128 << 10
		furthest = 1 << 20
	)

	f.srv.SeedRandom(key, size)
	f.stat(t, key)

	etag := etagOf(t, f.coordFixture, key)

	f.coord.queryResults = []types.KeyAnnouncement{
		holder(key, etag, size, 0, 256<<10),                               // freshest, and short
		holder(key, `"ffffffffffffffffffffffffffffffff"`, size, 0, 3<<20), // furthest, wrong version
		holder(key, etag, size, 512<<10, furthest-(512<<10)),              // the furthest right version
		holder(key, etag, size, 0, 300<<10),                               // middling
	}

	f.read(t, f.open(key), 0, readSize)

	req := f.warmed(t)

	if want := int64(furthest - readSize); req.size != want {
		t.Errorf("warmed %d bytes, want %d. The furthest claim at the version being read reaches %d; a "+
			"size of %d means only the first holder was considered, and one reaching %d means a claim "+
			"about another version was acted on",
			req.size, want, furthest, (256<<10)-readSize, (3<<20)-readSize)
	}
}

// TestWarmingTreatsLengthZeroAsTheWholeObject pins the convention [types.KeyAnnouncement] documents.
//
// A Length of 0 means the full object from Offset, and it is not a rare shape: an unranged GET announces
// it. Read literally as a length it is an empty range, so a holder of the entire object would look like a
// holder of nothing — the strongest possible claim silently producing no warm at all.
func TestWarmingTreatsLengthZeroAsTheWholeObject(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)

	const (
		key      = "whole-object.dat"
		size     = 1 << 20
		readSize = 128 << 10
	)

	f.srv.SeedRandom(key, size)
	f.stat(t, key)

	f.coord.queryResults = []types.KeyAnnouncement{
		holder(key, etagOf(t, f.coordFixture, key), size, 0, 0),
	}

	f.read(t, f.open(key), 0, readSize)

	req := f.warmed(t)

	if want := int64(size - readSize); req.size != want {
		t.Errorf("a holder claiming the whole %d-byte object produced a warm of %d bytes, want %d. Length "+
			"0 means the object's full extent from Offset; read as a literal length it is an empty range, "+
			"so the strongest claim there is would warm nothing", size, req.size, want)
	}
}

// TestWarmingIsCappedAtMaxPeerWarmBytes bounds the wasted egress.
//
// Warming reads bytes no application asked for, so the worst case is the whole warm being paid for and
// none of it read. A peer holding a 64 MiB object end to end is a claim that would otherwise pull all of
// it on one 128 KiB miss.
func TestWarmingIsCappedAtMaxPeerWarmBytes(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)

	const (
		key      = "huge.dat"
		size     = 64 << 20
		readSize = 128 << 10
	)

	// Not seeded with 64 MiB of random bytes: nothing here reads them. The metadata cache decides the
	// object's extent for this path, and cacheInfo is how the size gets there.
	f.srv.SeedRandom(key, 8<<10)

	etag := etagOf(t, f.coordFixture, key)
	f.fs.cacheInfo(key, &types.ObjectInfo{Key: key, Size: size, ETag: etag})

	f.coord.queryResults = []types.KeyAnnouncement{holder(key, etag, size, 0, 0)}

	f.fs.warmFromPeers(t.Context(), key, 0, readSize)

	if req := f.warmed(t); req.size != maxPeerWarmBytes {
		t.Errorf("a peer claiming the whole %d-byte object produced a warm of %d bytes, want the "+
			"%d-byte cap. Unbounded, one cache miss transfers the rest of the object on speculation",
			size, req.size, maxPeerWarmBytes)
	}
}

// TestNoWarmWhenNoPeerReachesPastTheRead is the ordinary case, and it is also why warming cannot loop.
//
// A peer that read the same range as this node holds exactly what was just read, so there is nothing
// further to fetch. That is what stops a warm's own GET from producing another: the bytes a warm fetches
// are cached, so the next miss is past them or does not happen.
func TestNoWarmWhenNoPeerReachesPastTheRead(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)

	const (
		key      = "same-range.dat"
		size     = 1 << 20
		readSize = 128 << 10
	)

	f.srv.SeedRandom(key, size)
	f.stat(t, key)

	etag := etagOf(t, f.coordFixture, key)

	for _, tc := range []struct {
		name   string
		holder types.KeyAnnouncement
	}{
		{"exactly the range that was read", holder(key, etag, size, 0, readSize)},
		{"less than was read", holder(key, etag, size, 0, 64<<10)},
		{"a range entirely behind the read", holder(key, etag, size, 0, 4<<10)},
	} {
		f.coord.queryResults = []types.KeyAnnouncement{tc.holder}

		f.fs.warmFromPeers(t.Context(), key, 0, readSize)

		if reqs := f.queued(); len(reqs) != 0 {
			t.Errorf("%s: warmed %+v, want nothing. There are no bytes past the read to fetch, and a warm "+
				"of a range already cached is the loop this design avoids", tc.name, reqs)
		}
	}
}

// TestNoPeersMeansNoWarm covers the empty answer, which is every mount whose peers have not read the key.
func TestNoPeersMeansNoWarm(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)

	const key = "unheld.dat"

	f.srv.SeedRandom(key, 1<<20)
	f.stat(t, key)

	f.read(t, f.open(key), 0, 128<<10)

	if reqs := f.queued(); len(reqs) != 0 {
		t.Errorf("warmed %+v with no peer claiming to hold anything; there is no evidence here, and read-"+
			"ahead's own prediction is what a solitary reader gets", reqs)
	}
}

// TestNoCoordinatorAsksNoPeers is the single-node path.
//
// Nearly every mount takes it. The assertion is on the *query*, not only on the warm: a nil coordinator
// cannot be called at all, so a guard missing here is a nil dereference on the read path rather than a
// missed optimization.
func TestNoCoordinatorAsksNoPeers(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)
	f.fs.coordinator = nil

	const key = "solo-warm.dat"

	content := f.srv.SeedRandom(key, 1<<20)
	f.stat(t, key)

	got := f.read(t, f.open(key), 0, 128<<10)
	if string(got) != string(content[:128<<10]) {
		t.Error("a read on a mount with no coordinator returned different bytes")
	}

	if queries := f.coord.queries(); len(queries) != 0 {
		t.Errorf("queried %v after the coordinator was set to nil", queries)
	}

	if reqs := f.queued(); len(reqs) != 0 {
		t.Errorf("queued %+v on a mount with no cluster", reqs)
	}
}

// TestReadAheadDisabledAsksNoPeers is an operator's setting being honored, and the query is the half
// worth asserting.
//
// [ReadAheadManager.warm] refuses to queue when read-ahead is off, which is the guarantee. But reaching
// that refusal costs a gossip broadcast per cache miss whose answer is then discarded — invisible in any
// assertion about warming, and visible in production as unexplained cluster traffic on a mount whose
// operator turned the feature off.
func TestReadAheadDisabledAsksNoPeers(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)

	f.fs.readAhead = NewReadAheadManager(t.Context(), f.fs, &ReadAheadConfig{
		Enabled:         false,
		WindowSize:      64 << 10,
		MinSequential:   3,
		ConcurrentReads: 0,
		TTL:             time.Minute,
	})

	const (
		key  = "no-readahead.dat"
		size = 1 << 20
	)

	f.srv.SeedRandom(key, size)
	f.stat(t, key)

	f.coord.queryResults = []types.KeyAnnouncement{
		holder(key, etagOf(t, f.coordFixture, key), size, 0, 512<<10),
	}

	f.read(t, f.open(key), 0, 128<<10)

	if queries := f.coord.queries(); len(queries) != 0 {
		t.Errorf("a mount with read-ahead disabled queried peer ownership for %v. The answer cannot be "+
			"acted on, so this is one gossip round trip per cache miss in exchange for nothing", queries)
	}

	if reqs := f.queued(); len(reqs) != 0 {
		t.Errorf("queued %+v with read-ahead disabled; an operator who turned it off has said not to read "+
			"bytes nobody asked for, and a peer's claim is not an exception", reqs)
	}

	// And warm itself refuses, which is the guard that keeps the promise rather than the one that saves the
	// round trip. Asserted directly because nothing else reaches it: with warmFromPeers returning early,
	// removing the check inside warm changes no observable behavior today — and it is the check that would
	// matter the moment a second caller appears.
	f.fs.readAhead.warm(key, 128<<10, 64<<10)

	if reqs := f.queued(); len(reqs) != 0 {
		t.Errorf("warm queued %+v with read-ahead disabled when called directly", reqs)
	}
}

// TestNilReadAheadManagerIsSafe covers the other nil on this path.
//
// Three tests in this package set `readAhead` to nil to take the prefetcher out of the way, and
// [ReadAheadManager.warm] tolerates a nil receiver for that reason. warmFromPeers now calls a second
// method on the same receiver before the query, so the nil case has to hold there too — otherwise those
// tests, and any mount that ever leaves the field unset, panic inside a read.
func TestNilReadAheadManagerIsSafe(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)
	f.fs.readAhead = nil

	const key = "nil-manager.dat"

	content := f.srv.SeedRandom(key, 64<<10)
	f.stat(t, key)

	f.coord.queryResults = []types.KeyAnnouncement{
		holder(key, etagOf(t, f.coordFixture, key), 64<<10, 0, 64<<10),
	}

	got := f.read(t, f.open(key), 0, 8<<10)
	if string(got) != string(content[:8<<10]) {
		t.Error("a read with no read-ahead manager returned different bytes")
	}
}

// TestReadSucceedsWhenTheOwnershipQueryFails is the fire-and-forget property for this direction.
//
// The read has already succeeded by the time this runs — the bytes are fetched and the buffer is filled —
// so a failed query costs a warm that would have been speculative anyway. Surfacing it would fail a
// read(2) that has its data.
func TestReadSucceedsWhenTheOwnershipQueryFails(t *testing.T) {
	t.Parallel()

	f := newWarmFixture(t)
	f.coord.queryErr = errors.New("no peer answered")

	const key = "query-fails.dat"

	content := f.srv.SeedRandom(key, 1<<20)
	f.stat(t, key)

	got := f.read(t, f.open(key), 0, 128<<10)
	if string(got) != string(content[:128<<10]) {
		t.Errorf("a read whose ownership query failed returned %d wrong bytes", len(got))
	}

	if queries := f.coord.queries(); len(queries) == 0 {
		t.Error("the query was not attempted, so this test asserts nothing about its failure")
	}

	if reqs := f.queued(); len(reqs) != 0 {
		t.Errorf("queued %+v from a failed query; there is no evidence to act on", reqs)
	}
}

// TestWarmingReallyFetchesTheRange is the one test here that lets a worker run, and it is what makes the
// queue assertions above worth anything.
//
// Every other test in this file asserts a decision. A decision is not an effect: `warm` could queue onto
// a channel no worker drains, or performPrefetch could discard the request, and all of them would still
// pass. So this one asserts where the bytes ended up — in this node's cache, fetched from S3, which is
// #142's rescope in one sentence.
//
// It polls rather than sleeping, so it neither races the worker nor waits for a fixed interval that is
// too short on a loaded machine and wasted on an idle one.
func TestWarmingReallyFetchesTheRange(t *testing.T) {
	t.Parallel()

	f := newCoordFixture(t)

	// One worker, so the queued warm is actually performed. Not four: a single worker makes the sequence
	// deterministic, and there is exactly one request to serve.
	f.fs.readAhead = NewReadAheadManager(t.Context(), f.fs, &ReadAheadConfig{
		Enabled:         true,
		WindowSize:      64 << 10,
		MinSequential:   3,
		ConcurrentReads: 1,
		TTL:             time.Minute,
	})

	const (
		key      = "warm-for-real.dat"
		size     = 1 << 20
		readSize = 128 << 10
		peerEnd  = 512 << 10
	)

	content := f.srv.SeedRandom(key, size)
	f.stat(t, key)

	f.coord.queryResults = []types.KeyAnnouncement{holder(key, etagOf(t, f, key), size, 0, peerEnd)}

	if got := f.read(t, f.open(key), 0, readSize); string(got) != string(content[:readSize]) {
		t.Fatal("the read itself returned the wrong bytes")
	}

	// The warmed range, held in full. Get answers only for a range it holds entirely, so this is the whole
	// property in one call: a warm that fetched a prefix, or nothing, cannot satisfy it.
	deadline := time.Now().Add(30 * time.Second)

	var held []byte
	for time.Now().Before(deadline) {
		if held = f.fs.cache.Get(key, readSize, peerEnd-readSize); held != nil {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	if held == nil {
		t.Fatalf("%d bytes from offset %d are not cached after a peer claimed to hold through %d. The warm "+
			"was decided but nothing fetched it, so #142 costs a gossip round trip and buys nothing. GETs: %d",
			peerEnd-readSize, readSize, peerEnd, len(f.srv.GETs(key)))
	}

	if string(held) != string(content[readSize:peerEnd]) {
		t.Error("the warmed range holds the wrong bytes: a warm fetches [read end, holder end) of this " +
			"object, and holding some other range under that key would serve them to a reader as file " +
			"content")
	}

	// And the bytes came from S3 rather than from the peer, which is the rescope. A gossip datagram carries
	// 5802 bytes of object at the default limit, so a transport that tried to serve 384 KiB over it would
	// fail as a 30-second timeout on this host (#399); the GET is the evidence it was never attempted.
	if gets := len(f.srv.GETs(key)); gets < 2 {
		t.Errorf("the object was fetched in %d GET(s); the read is one and the warm is another, so fewer "+
			"than two means the warmed bytes did not come from the object store", gets)
	}

	// Bounded: the warm is one fetch of one range, not a walk of the object. A warm whose own GET queried
	// ownership and queued another would show up here as a GET count climbing with the object's size.
	if got := f.srv.BytesRead(key); got > peerEnd {
		t.Errorf("a %d-byte read plus a warm through %d transferred %d bytes. Warming must not warm from "+
			"its own output: it is queued on the application's read alone, and a warm feeding itself walks "+
			"the whole object one window at a time", readSize, peerEnd, got)
	}
}
