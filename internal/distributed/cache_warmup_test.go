package distributed

// Tests for cache warming on join (#143). The load-bearing ones are the two that measure rather than
// count: the issue specifies "max 256 entries", 256 entries seal to 7.6× the default limit, and a test
// that asserted a count would pass on exactly the broken value the issue asks for.
//
// There is precedent for that shape in this package.
// TestNewClusterManager_DefaultMaxGossipPacketHoldsAThreeNodeSync exists because a limit of 1024 could not
// carry the smallest cluster needing a quorum, and its comment notes that a test comparing the constant to
// 8192 would have passed on the broken value. Same lesson, same remedy: seal the message and measure it.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// warmupAnnouncements builds n announcements shaped like a real deployment's.
//
// The key length is what decides how many fit a datagram, so these are sized like keys a research bucket
// actually holds — a prefix, a sample identifier, a filename — rather than "k0". A test packing "k0".."k99"
// would fit several times more entries per message than production and would report a bound nobody will
// see.
func warmupAnnouncements(n int) []types.KeyAnnouncement {
	anns := make([]types.KeyAnnouncement, 0, n)
	for i := range n {
		anns = append(anns, types.KeyAnnouncement{
			Key:      fmt.Sprintf("genomics/project-alpha/sample-%05d/reads.sorted.bam", i),
			NodeID:   "compute-node-17.cluster.example.org",
			ETag:     fmt.Sprintf("%q", fmt.Sprintf("d41d8cd98f00b204e9800998ecf8427%d", i%10)),
			Size:     4 << 30,
			CachedAt: time.Now(),
			Offset:   int64(i) * (128 << 10),
			Length:   128 << 10,
		})
	}

	return anns
}

// TestMarshalWarmupChunk_EveryChunkFitsASealedDatagram is the bound, asserted the only way it can be: by
// sealing what would go on the wire and measuring it.
//
// It packs far more candidates than can fit, so the chunk returned is decided by the limit rather than by
// running out of input, and then checks the sealed length against MaxGossipPacket — the same comparison
// [GossipProtocol.sendMessage] makes, which is what would refuse an oversize datagram outright.
func TestMarshalWarmupChunk_EveryChunkFitsASealedDatagram(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "warmup-fits"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	// 256 is the count #143 specifies. Measured at 7.6× the default limit, so this is also the input that
	// makes the count-based bound visibly wrong.
	candidates := warmupAnnouncements(256)

	payload, fitted, err := cm.gossip.marshalWarmupChunk(candidates)
	if err != nil {
		t.Fatalf("marshalWarmupChunk: %v", err)
	}

	if fitted == 0 {
		t.Fatal("no announcement fit a datagram at the default max_gossip_packet, so a joining node would " +
			"be warmed with nothing at all")
	}
	if fitted >= len(candidates) {
		t.Fatalf("all %d announcements were packed into one message; the limit was never reached, so this "+
			"test is not measuring the bound", fitted)
	}

	sealed, err := cm.gossip.auth.seal(&GossipMessage{
		Type:      MessageTypeCacheWarmup,
		From:      cm.gossip.localNode.ID,
		Timestamp: time.Now(),
		MessageID: cm.gossip.generateMessageID(),
		Data:      payload,
	})
	if err != nil {
		t.Fatalf("sealing the chunk: %v", err)
	}

	if len(sealed) > cm.gossip.config.MaxGossipPacket {
		t.Errorf("a warmup chunk of %d entries seals to %d bytes, over max_gossip_packet of %d; "+
			"sendMessage refuses it and the joining node warms nothing",
			fitted, len(sealed), cm.gossip.config.MaxGossipPacket)
	}

	// And the payload really carries what was counted, so a chunk that fits by having dropped its contents
	// cannot pass.
	var m CacheWarmupMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshaling the chunk: %v", err)
	}
	if len(m.Keys) != fitted {
		t.Errorf("the chunk reports %d entries and carries %d", fitted, len(m.Keys))
	}
}

// TestMarshalWarmupChunk_TheCountVariesWithKeyLength is why the bound cannot be a constant.
//
// #143 asks for 256 entries; the measurement that refuted it found 32 at ~50-character keys. This asserts
// that 32 is not a constant either — the same limit fits materially more short keys than long ones, so any
// number written into the code is wasteful for one deployment and refused for another.
func TestMarshalWarmupChunk_TheCountVariesWithKeyLength(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "warmup-varies"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	pack := func(keyLen int) int {
		t.Helper()

		anns := warmupAnnouncements(256)
		for i := range anns {
			anns[i].Key = strings.Repeat("a", keyLen-4) + fmt.Sprintf("%04d", i)
		}

		_, fitted, err := cm.gossip.marshalWarmupChunk(anns)
		if err != nil {
			t.Fatalf("marshalWarmupChunk at key length %d: %v", keyLen, err)
		}

		return fitted
	}

	short, long := pack(16), pack(200)

	if short <= long {
		t.Errorf("%d entries fit at 16-byte keys and %d at 200-byte keys; if the count does not vary with "+
			"key length then this test is not measuring what bounds the message", short, long)
	}
	if long == 0 {
		t.Error("no 200-byte-key announcement fits a default datagram, which would mean deployments with " +
			"long prefixes cannot be warmed at all")
	}
}

// TestMarshalWarmupChunk_ReportsAnEntryThatCannotFitAlone asserts the zero-count answer rather than an
// empty message.
//
// A single announcement larger than the datagram cannot be helped by chunking, and the caller has to be
// able to tell that from "nothing to send" — one is a misconfiguration worth a Warn, the other is the
// ordinary state of an idle node. See [GossipProtocol.sendCacheWarmup].
func TestMarshalWarmupChunk_ReportsAnEntryThatCannotFitAlone(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "warmup-toobig"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	huge := types.KeyAnnouncement{
		Key:    strings.Repeat("k", cm.gossip.config.MaxGossipPacket),
		NodeID: "peer-1",
		ETag:   `"v1"`,
	}

	payload, fitted, err := cm.gossip.marshalWarmupChunk([]types.KeyAnnouncement{huge})
	if err != nil {
		t.Fatalf("marshalWarmupChunk: %v", err)
	}
	if fitted != 0 {
		t.Errorf("an announcement whose key alone exceeds max_gossip_packet was packed (%d entries), so "+
			"sendMessage would refuse the datagram", fitted)
	}
	if payload != nil {
		t.Errorf("a chunk that fits nothing returned a %d-byte payload, which a caller would send", len(payload))
	}
}

// TestMarshalWarmupChunk_HonoursAReconfiguredLimit asserts the bound follows MaxGossipPacket rather than
// the default it happens to be tested at.
//
// The plan calls for this explicitly: a byte bound that only works at 8192 is a constant with extra steps.
func TestMarshalWarmupChunk_HonoursAReconfiguredLimit(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, "warmup-reconfigured")
	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	candidates := warmupAnnouncements(256)

	_, atDefault, err := cm.gossip.marshalWarmupChunk(candidates)
	if err != nil {
		t.Fatalf("marshalWarmupChunk at the default limit: %v", err)
	}

	// Halved after construction, which is legitimate here because nothing has been sent: what is under
	// test is that the packing reads the configured value at pack time rather than a value baked in.
	cm.gossip.config.MaxGossipPacket /= 2

	payload, atHalf, err := cm.gossip.marshalWarmupChunk(candidates)
	if err != nil {
		t.Fatalf("marshalWarmupChunk at half the limit: %v", err)
	}

	if atHalf >= atDefault {
		t.Errorf("halving max_gossip_packet packed %d entries where the full limit packed %d; the bound is "+
			"not reading the configured size", atHalf, atDefault)
	}

	sealed, err := cm.gossip.auth.seal(&GossipMessage{
		Type:      MessageTypeCacheWarmup,
		From:      cm.gossip.localNode.ID,
		Timestamp: time.Now(),
		MessageID: cm.gossip.generateMessageID(),
		Data:      payload,
	})
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}
	if len(sealed) > cm.gossip.config.MaxGossipPacket {
		t.Errorf("a chunk sealed to %d bytes against a reconfigured limit of %d",
			len(sealed), cm.gossip.config.MaxGossipPacket)
	}
}

// TestSendCacheWarmup_SendsNothingWhenNothingIsHeld is #143's third acceptance criterion, and it is worth
// asserting rather than assuming: a datagram whose payload is `{"keys":[]}` is a message a receiver has to
// parse to learn there is nothing in it, sent by every node that has just started.
func TestSendCacheWarmup_SendsNothingWhenNothingIsHeld(t *testing.T) {
	t.Parallel()

	cm1, cm2 := startGossipPair(t, "warmup-empty-a", "warmup-empty-b")

	before := messagesOfType(cm2, MessageTypeCacheWarmup)

	cm1.gossip.sendCacheWarmup(cm2.gossip.LocalAddr())

	// Nothing to wait for, which is the assertion. A short poll rather than an immediate read, so that a
	// message in flight has time to arrive and fail this.
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := messagesOfType(cm2, MessageTypeCacheWarmup) - before; got != 0 {
			t.Fatalf("a node holding nothing sent %d warmup message(s)", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// messagesOfType reports how many messages of msgType node has received.
func messagesOfType(node *ClusterManager, msgType MessageType) int64 {
	node.gossip.stats.mu.Lock()
	defer node.gossip.stats.mu.Unlock()

	return node.gossip.stats.MessagesByType[string(msgType)]
}

// TestJoinWarmsTheJoiner is #143's headline criterion, restated as the plan requires: not a hit rate, but
// **where the keys came from** — the joiner learns holders it was never told about individually.
//
// The original criterion, "cache.Stats().HitRate for warmed keys is 100% before any S3 request", cannot
// pass against any implementation: [cache.MultiLevelCache.Warmup] is one GetObject per key, so it *is* the
// S3 traffic the criterion forbids. What warming actually delivers is the ownership map, and that is what
// this asserts.
//
// Driven through a real join over loopback UDP rather than by calling sendCacheWarmup directly, because the
// thing most likely to be wrong is not the packing but whether anything calls it at all — the same reason
// #140's tests go over the wire.
func TestJoinWarmsTheJoiner(t *testing.T) {
	t.Parallel()

	// A is the established node with a warm cache; B joins it.
	cmA, cmB := startGossipPair(t, "warmup-join-a", "warmup-join-b")

	held := []types.KeyAnnouncement{
		{Key: "genomics/sample-1/reads.bam", ETag: `"etag-1"`, Size: 1 << 30, Length: 128 << 10},
		{Key: "genomics/sample-2/reads.bam", ETag: `"etag-2"`, Size: 2 << 30, Length: 256 << 10},
		{Key: "genomics/sample-3/reads.bam", ETag: `"etag-3"`, Size: 3 << 30, Length: 512 << 10},
	}

	// B is taken out of A's memberlist *before* anything is announced, and that is what makes this test
	// mean what it says. startGossipPair puts B there so a broadcast reaches it — so with B still a member,
	// every announcement below is delivered to B directly and B ends up holding the right ownership map
	// whether the warmup exists or not. Clearing B's map after announcing does not fix it either: delivery
	// is asynchronous, on B's receive goroutine, so a broadcast still in flight lands after the clear. That
	// version of this test passed with recordSelfHolding removed entirely.
	//
	// With B gone from the memberlist, A has no peers, AnnounceKey sends nothing, and the join below is the
	// only channel by which B can learn any of this.
	cmA.gossip.mu.Lock()
	delete(cmA.gossip.memberlist, "warmup-join-b")
	cmA.gossip.mu.Unlock()

	// B advertises the port it actually bound. A join carries the joiner's own NodeInfo and A replies to
	// the Address in it, so this is what makes a reply reachable at all: testConfig sets AdvertiseAddr to
	// "127.0.0.1:0" — a request for any free port, which is right for ListenAddr and is not an address
	// anything can send to. A real deployment advertises a routable address, so this is the fixture catching
	// up with production rather than an accommodation.
	//
	// It matters more here than in the earlier tests in this file, which reach B by broadcast: startGossipPair
	// puts B's *bound* address in A's memberlist, so those never consult what B advertises.
	cmB.gossip.mu.Lock()
	cmB.gossip.localNode.Address = cmB.gossip.LocalAddr()
	cmB.gossip.mu.Unlock()

	// Announced, which is how a node comes to hold anything: the read path calls AnnounceKey after caching
	// bytes. Seeding selfAnnounced directly would let the test pass with AnnounceKey recording nothing,
	// which is the wiring most likely to be missing.
	for _, ann := range held {
		if err := cmA.GetCoordinator().AnnounceKey(t.Context(), ann); err != nil {
			t.Fatalf("AnnounceKey(%q): %v", ann.Key, err)
		}
	}

	// Nothing reached B, since A has nobody to broadcast to. Asserted rather than assumed: if a broadcast
	// did arrive, every assertion below would hold with no warmup implemented at all.
	if got := messagesOfType(cmB, MessageTypeCacheAnnounce); got != 0 {
		t.Fatalf("B received %d announcement(s) before joining; the assertions below would then pass "+
			"whether or not a join warms anything", got)
	}

	// A join from B, which A answers with a sync and then a warmup.
	if err := cmB.gossip.JoinNode(t.Context(), cmA.gossip.LocalAddr()); err != nil {
		t.Fatalf("JoinNode: %v", err)
	}

	for _, ann := range held {
		holders := waitForHolders(t, cmB, ann.Key, 1)

		if holders[0].NodeID != "warmup-join-a" {
			t.Errorf("%q is held by %q, want the warming node %q",
				ann.Key, holders[0].NodeID, "warmup-join-a")
		}
		if holders[0].ETag != ann.ETag {
			t.Errorf("%q arrived at ETag %q, want %q; a warmed entry whose version did not survive is one "+
				"a read cannot check its bytes against", ann.Key, holders[0].ETag, ann.ETag)
		}
		if holders[0].Size != ann.Size {
			t.Errorf("%q arrived with Size %d, want %d", ann.Key, holders[0].Size, ann.Size)
		}
		if holders[0].Length != ann.Length {
			t.Errorf("%q arrived with Length %d, want %d; the range is what tells a reader how much of the "+
				"object is worth warming", ann.Key, holders[0].Length, ann.Length)
		}
	}
}

// TestRecentHoldings_ExcludesPeersClaims is the separation between the two maps, asserted.
//
// A node must warm a joiner with what *it* holds. Answering with peers' claims would tell the joiner to
// fetch from nodes the sender merely heard about — and, in the two-node case, would tell B that B holds
// the keys, which is a self-claim recordAnnouncement then correctly discards, so the warming would silently
// do nothing.
func TestRecentHoldings_ExcludesPeersClaims(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "holdings-local")

	c.recordAnnouncement(types.KeyAnnouncement{Key: "peers.dat", NodeID: "peer-1", ETag: `"v1"`})
	c.recordSelfHolding(types.KeyAnnouncement{Key: "mine.dat", NodeID: "holdings-local", ETag: `"v2"`})

	got := c.recentHoldings(0)
	if len(got) != 1 {
		t.Fatalf("recentHoldings returned %d entries, want 1: %+v", len(got), got)
	}
	if got[0].Key != "mine.dat" {
		t.Errorf("recentHoldings returned %q; a node warms a joiner with what it holds itself, not with "+
			"what it has heard about", got[0].Key)
	}
}

// TestRecentHoldings_FreshestFirst is what makes a truncated warmup the useful half.
//
// The message is bounded by bytes, so the tail is dropped — and which entries end up in the tail is
// decided entirely by this order. Freshest first puts the keys most likely still cached in the prefix that
// gets sent.
func TestRecentHoldings_FreshestFirst(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "holdings-order")

	for i := range 5 {
		c.recordSelfHolding(types.KeyAnnouncement{
			Key: fmt.Sprintf("k%d", i), NodeID: "holdings-order", ETag: `"v1"`,
		})
		// recordSelfHolding stamps time.Now(), and five inserts can land inside one clock tick on a coarse
		// timer. A short sleep is what makes the order observable at all.
		time.Sleep(2 * time.Millisecond)
	}

	got := c.recentHoldings(0)
	if len(got) != 5 {
		t.Fatalf("recentHoldings returned %d entries, want 5", len(got))
	}

	for i, want := range []string{"k4", "k3", "k2", "k1", "k0"} {
		if got[i].Key != want {
			t.Fatalf("recentHoldings()[%d] is %q, want %q; the order decides which entries survive the "+
				"byte bound, so oldest-first would warm a joiner with the keys likeliest to be evicted "+
				"already. Got: %+v", i, got[i].Key, want, got)
		}
	}
}

// TestRecentHoldings_ExcludesExpiredClaims asserts the TTL is applied on read, not only by the sweep.
//
// Same reasoning as [Coordinator.QueryKeyOwnership]'s: filtering on read is what makes the TTL exact,
// since the sweep runs on a minute timer and a joiner arriving between ticks would otherwise be warmed
// with keys this node stopped believing in.
func TestRecentHoldings_ExcludesExpiredClaims(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, "holdings-expiry")
	cfg.AnnouncementTTL = time.Millisecond

	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	cm.coordinator.recordSelfHolding(types.KeyAnnouncement{
		Key: "stale.dat", NodeID: "holdings-expiry", ETag: `"v1"`,
	})

	time.Sleep(20 * time.Millisecond)

	if got := cm.coordinator.recentHoldings(0); len(got) != 0 {
		t.Errorf("recentHoldings returned %d expired entries: %+v", len(got), got)
	}
}

// TestRecentHoldings_ReplacesAnEarlierVersion is the same rule [Coordinator.recordAnnouncement] applies to
// peers, applied to this node: announcing a key twice does not make it two holdings, and the older ETag is
// the one this node has replaced.
func TestRecentHoldings_ReplacesAnEarlierVersion(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "holdings-replace")

	c.recordSelfHolding(types.KeyAnnouncement{Key: "k", NodeID: "holdings-replace", ETag: `"v1"`})
	c.recordSelfHolding(types.KeyAnnouncement{Key: "k", NodeID: "holdings-replace", ETag: `"v2"`})

	got := c.recentHoldings(0)
	if len(got) != 1 {
		t.Fatalf("one key announced twice is recorded as %d holdings: %+v", len(got), got)
	}
	if got[0].ETag != `"v2"` {
		t.Errorf("the holding is at %s, want the newer %s", got[0].ETag, `"v2"`)
	}
}

// TestRecordSelfHolding_BoundsTheMap drives the map past maxAnnouncedKeys and asserts it stops growing.
//
// The peer map has the same bound and its own test; this one is for the map that would otherwise grow one
// entry per key this mount has ever read, which on a traversal of a large bucket is every key in it.
func TestRecordSelfHolding_BoundsTheMap(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "holdings-bound")

	for i := range maxAnnouncedKeys + 1 {
		c.recordSelfHolding(types.KeyAnnouncement{
			Key: fmt.Sprintf("key-%d", i), NodeID: "holdings-bound", ETag: `"v1"`,
		})
	}

	c.announcementsMu.RLock()
	size := len(c.selfAnnounced)
	c.announcementsMu.RUnlock()

	if size > maxAnnouncedKeys {
		t.Errorf("selfAnnounced holds %d keys, over the bound of %d", size, maxAnnouncedKeys)
	}
	if size == 0 {
		t.Error("selfAnnounced was emptied entirely, so a joiner would be warmed with nothing")
	}
}

// TestSweepExpiredAnnouncements_SweepsBothMaps asserts the sweep reclaims this node's own holdings too.
//
// This is the leak that would be invisible: recentHoldings filters expired entries on read, so the map
// could grow for the life of the mount while every caller saw correct answers.
func TestSweepExpiredAnnouncements_SweepsBothMaps(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, "sweep-both")
	cfg.AnnouncementTTL = time.Millisecond

	cm, err := NewClusterManager(cfg)
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	c := cm.coordinator

	c.recordSelfHolding(types.KeyAnnouncement{Key: "mine", NodeID: "sweep-both", ETag: `"v1"`})
	c.recordAnnouncement(types.KeyAnnouncement{Key: "theirs", NodeID: "peer-1", ETag: `"v1"`})

	time.Sleep(20 * time.Millisecond)
	c.sweepExpiredAnnouncements()

	c.announcementsMu.RLock()
	self, peers := len(c.selfAnnounced), len(c.announcements)
	c.announcementsMu.RUnlock()

	if self != 0 {
		t.Errorf("the sweep left %d expired self-holding(s); nothing else reclaims them, so the map grows "+
			"for the life of the mount while recentHoldings filters them out of every answer", self)
	}
	if peers != 0 {
		t.Errorf("the sweep left %d expired peer claim(s)", peers)
	}
}

// TestHandleCacheWarmup_TheEnvelopeSenderWins asserts a member cannot warm a joiner with claims about a
// third node.
//
// Same defense as [GossipProtocol.handleCacheAnnounce]'s, and it needs its own test because a warmup
// message is a *batch*: a loop that credited each entry's own NodeID would look right and would let one
// peer populate a joiner's whole ownership map with fabricated holders.
func TestHandleCacheWarmup_TheEnvelopeSenderWins(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "warmup-envelope"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	payload, err := json.Marshal(&CacheWarmupMessage{Keys: []types.KeyAnnouncement{
		{Key: "a", NodeID: "innocent-third-node", ETag: `"v1"`},
		{Key: "b", NodeID: "another-third-node", ETag: `"v1"`},
	}})
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	cm.gossip.handleCacheWarmup(&GossipMessage{
		Type: MessageTypeCacheWarmup,
		From: "lying-peer",
		Data: payload,
	})

	for _, key := range []string{"a", "b"} {
		holders, err := cm.coordinator.QueryKeyOwnership(t.Context(), key)
		if err != nil {
			t.Fatalf("QueryKeyOwnership(%q): %v", key, err)
		}
		if len(holders) != 1 {
			t.Fatalf("%q has %d holders, want 1: %+v", key, len(holders), holders)
		}
		if holders[0].NodeID != "lying-peer" {
			t.Errorf("%q is credited to %q, want the peer the message came from, %q — a warmed read would "+
				"be sent to a node that never cached the bytes", key, holders[0].NodeID, "lying-peer")
		}
	}
}

// TestHandleCacheWarmup_DiscardsUnusableEntries asserts a warmup goes through the same validation an
// individual announcement does.
//
// An entry with no ETag is the one that matters: it would sit in the ownership map as a claim a reader
// cannot check its bytes against, which is the integrity failure #140's fail-closed rule exists to
// prevent. Batching must not be a way around it.
func TestHandleCacheWarmup_DiscardsUnusableEntries(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "warmup-unusable"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	payload, err := json.Marshal(&CacheWarmupMessage{Keys: []types.KeyAnnouncement{
		{Key: "no-etag", NodeID: "peer-1"},
		{Key: "", NodeID: "peer-1", ETag: `"v1"`},
		{Key: "good", NodeID: "peer-1", ETag: `"v1"`},
	}})
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	cm.gossip.handleCacheWarmup(&GossipMessage{
		Type: MessageTypeCacheWarmup,
		From: "peer-1",
		Data: payload,
	})

	if got := cm.coordinator.announcedKeys(); got != 1 {
		t.Errorf("a warmup carrying one usable entry and two unusable ones recorded %d keys, want 1", got)
	}

	holders, err := cm.coordinator.QueryKeyOwnership(t.Context(), "no-etag")
	if err != nil {
		t.Fatalf("QueryKeyOwnership: %v", err)
	}
	if len(holders) != 0 {
		t.Errorf("an entry with no ETag was recorded as a holder: %+v. A reader has nothing to check the "+
			"bytes it fetches against", holders)
	}
}

// TestHandleCacheWarmup_DiscardsAMalformedPayload asserts a truncated or corrupt message records nothing,
// rather than the zero-valued entries a partial unmarshal would leave.
func TestHandleCacheWarmup_DiscardsAMalformedPayload(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "warmup-malformed"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	cm.gossip.handleCacheWarmup(&GossipMessage{
		Type: MessageTypeCacheWarmup,
		From: "peer-1",
		Data: []byte(`{"keys":[{"key":"a","etag":`),
	})

	if got := cm.coordinator.announcedKeys(); got != 0 {
		t.Errorf("a malformed warmup message left %d key(s) recorded, want 0", got)
	}
}

// TestSendCacheWarmup_BoundsTheBurst asserts one join cannot produce an unbounded stream of datagrams.
//
// The per-message bound is bytes; this is the bound on how many messages, which is a different resource: a
// node holding tens of thousands of keys would otherwise answer one join with thousands of datagrams aimed
// at a node that has been alive for milliseconds and is still receiving its membership sync.
func TestSendCacheWarmup_BoundsTheBurst(t *testing.T) {
	t.Parallel()

	cmA, cmB := startGossipPair(t, "warmup-burst-a", "warmup-burst-b")

	// Well past what maxWarmupDatagrams can carry at any key length.
	for _, ann := range warmupAnnouncements(2000) {
		cmA.coordinator.recordSelfHolding(ann)
	}

	before := messagesOfType(cmB, MessageTypeCacheWarmup)

	cmA.gossip.sendCacheWarmup(cmB.gossip.LocalAddr())

	// Poll for a while after the sends complete, so a burst still in flight is counted.
	deadline := time.Now().Add(500 * time.Millisecond)
	var got int64
	for time.Now().Before(deadline) {
		got = messagesOfType(cmB, MessageTypeCacheWarmup) - before
		if got > maxWarmupDatagrams {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got > maxWarmupDatagrams {
		t.Errorf("one join produced %d warmup datagrams, over the bound of %d", got, maxWarmupDatagrams)
	}
	if got == 0 {
		t.Error("a node holding 2000 keys sent a joining node no warmup at all")
	}
}
