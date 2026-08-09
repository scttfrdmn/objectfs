package distributed

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// waitForHolders polls node's view of who holds key until want holders are visible, and returns them.
//
// Polling rather than a single read, and the reason is the same as [waitForDeletion]'s: this waits on
// loopback UDP plus a receive goroutine, so a read taken immediately after a broadcast has no reason to
// see anything yet. The deadline is generous because the failure this exists to catch — a message never
// sent, or a dispatch arm that does not exist — is not one that more waiting would fix.
func waitForHolders(t *testing.T, node *ClusterManager, key string, want int) []types.KeyAnnouncement {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	var last []types.KeyAnnouncement

	for time.Now().Before(deadline) {
		holders, err := node.GetCoordinator().QueryKeyOwnership(t.Context(), key)
		if err != nil {
			t.Fatalf("QueryKeyOwnership(%q) on %s: %v", key, node.GetNodeID(), err)
		}
		if len(holders) >= want {
			return holders
		}

		last = holders
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("%s never saw %d holder(s) of %q within 2s; it has %d: %+v",
		node.GetNodeID(), want, key, len(last), last)

	return nil
}

// TestAnnounceKey_TwoNodes is #140's own acceptance criterion: node A announces a key, node B — reachable
// only over loopback UDP — reports A as a holder.
//
// It asserts the fields as well as the count. An announcement whose ETag did not survive the wire is
// worse than one that never arrived: [types.KeyAnnouncement] requires the version precisely because a
// peer that fetches bytes it cannot place against an object hands them to a reading process as file
// content, so a warming path acting on an empty ETag is an integrity failure rather than a slow read.
func TestAnnounceKey_TwoNodes(t *testing.T) {
	t.Parallel()

	cm1, cm2 := startGossipPair(t, "announce-node-a", "announce-node-b")

	want := types.KeyAnnouncement{
		Key:    "datasets/reads.bam",
		ETag:   `"etag-announced"`,
		Size:   1 << 20,
		Offset: 0,
		Length: 64 * 1024,
	}

	if err := cm1.GetCoordinator().AnnounceKey(t.Context(), want); err != nil {
		t.Fatalf("AnnounceKey: %v", err)
	}

	holders := waitForHolders(t, cm2, want.Key, 1)
	got := holders[0]

	if got.NodeID != "announce-node-a" {
		t.Errorf("holder is %q, want the announcing node %q", got.NodeID, "announce-node-a")
	}
	if got.ETag != want.ETag {
		t.Errorf("ETag is %q, want %q — a peer cannot place bytes against a version it does not have",
			got.ETag, want.ETag)
	}
	if got.Size != want.Size {
		t.Errorf("Size is %d, want %d", got.Size, want.Size)
	}
	if got.Length != want.Length {
		t.Errorf("Length is %d, want %d; a range that does not survive the wire invites a peer to ask "+
			"for bytes the holder does not have", got.Length, want.Length)
	}
	if got.CachedAt.IsZero() {
		t.Error("CachedAt is zero; AnnounceKey should have stamped it, since a recipient comparing two " +
			"holders of one ETag has nothing else to prefer between them by")
	}
}

// TestAnnounceKey_DoesNotRecordItself is the departure from #140's specification, asserted.
//
// The spec says to record the announcement locally as well as broadcast it. This does not, because
// QueryKeyOwnership is called *from a cache miss on this very node* — so a self-entry is a holder
// guaranteed not to hold, returned in place of a peer that might, and a warming path would spend a round
// trip discovering what the cache lookup that preceded it already knew.
//
// Both halves are checked, because only the pair distinguishes the intended behavior from a broadcast
// that failed: the announcer must not list itself, and the peer must list it.
func TestAnnounceKey_DoesNotRecordItself(t *testing.T) {
	t.Parallel()

	cm1, cm2 := startGossipPair(t, "selfrec-node-a", "selfrec-node-b")

	ann := types.KeyAnnouncement{Key: "self.dat", ETag: `"etag-self"`, Size: 10}
	if err := cm1.GetCoordinator().AnnounceKey(t.Context(), ann); err != nil {
		t.Fatalf("AnnounceKey: %v", err)
	}

	// The peer first, so that reaching the self-check means the broadcast demonstrably happened.
	waitForHolders(t, cm2, ann.Key, 1)

	holders, err := cm1.GetCoordinator().QueryKeyOwnership(t.Context(), ann.Key)
	if err != nil {
		t.Fatalf("QueryKeyOwnership on the announcer: %v", err)
	}
	if len(holders) != 0 {
		t.Errorf("the announcing node lists %d holder(s) of its own announcement: %+v. A query happens "+
			"because this node's cache missed, so an entry naming this node cannot be a holder",
			len(holders), holders)
	}
}

// TestHandleCacheAnnounce_TheEnvelopeSenderWins asserts a member cannot announce that some *other* node
// holds a key.
//
// Both the envelope's From and the payload's NodeID are authenticated by the same MAC, so this is not a
// defense against an outsider — it is a defense against a member, compromised or merely buggy, sending
// peers to fetch bytes a third node never cached and has no way to correct. The receiver credits the node
// it actually heard from.
//
// Driven through handleCacheAnnounce rather than over the wire because what is being tested is a
// disagreement between two fields of one message, and [Coordinator.AnnounceKey] deliberately makes that
// disagreement unconstructible on the sending side.
func TestHandleCacheAnnounce_TheEnvelopeSenderWins(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "envelope-local"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	payload, err := json.Marshal(types.KeyAnnouncement{
		Key: "k", NodeID: "innocent-third-node", ETag: `"v1"`,
	})
	if err != nil {
		t.Fatalf("marshaling the announcement: %v", err)
	}

	cm.gossip.handleCacheAnnounce(&GossipMessage{
		Type: MessageTypeCacheAnnounce,
		From: "lying-peer",
		Data: payload,
	})

	holders, err := cm.coordinator.QueryKeyOwnership(t.Context(), "k")
	if err != nil {
		t.Fatalf("QueryKeyOwnership: %v", err)
	}
	if len(holders) != 1 {
		t.Fatalf("got %d holders, want 1: %+v", len(holders), holders)
	}
	if holders[0].NodeID != "lying-peer" {
		t.Errorf("the holder is recorded as %q, want %q — the payload's claim about a third node was "+
			"believed over the peer the message actually came from, which sends warming reads to a node "+
			"that never cached the bytes", holders[0].NodeID, "lying-peer")
	}
}

// TestHandleCacheAnnounce_DiscardsAMalformedPayload asserts a message whose payload is not a
// [types.KeyAnnouncement] is dropped rather than recorded as a zero-valued claim.
//
// A zero claim is the dangerous outcome, not a panic: it would carry an empty ETag, and an empty ETag is
// the one thing a warming path cannot check its bytes against.
func TestHandleCacheAnnounce_DiscardsAMalformedPayload(t *testing.T) {
	t.Parallel()

	cm, err := NewClusterManager(testConfig(t, "malformed-local"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	cm.gossip.handleCacheAnnounce(&GossipMessage{
		Type: MessageTypeCacheAnnounce,
		From: "peer-1",
		Data: []byte("{not json"),
	})

	if got := cm.coordinator.announcedKeys(); got != 0 {
		t.Errorf("a malformed announcement left %d key(s) recorded, want 0", got)
	}
}

// TestRecordAnnouncement_ReplacesAPeersEarlierClaim asserts a node announcing the same key twice is one
// holder at its newer ETag, not two holders at conflicting ones.
//
// Keeping the first would offer a caller a version that node has already replaced — which the caller
// would then request from it and not receive, since the whole point of the second announcement is that
// the first ETag is gone.
func TestRecordAnnouncement_ReplacesAPeersEarlierClaim(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "replace-local")

	c.recordAnnouncement(types.KeyAnnouncement{Key: "k", NodeID: "peer-1", ETag: `"v1"`})
	c.recordAnnouncement(types.KeyAnnouncement{Key: "k", NodeID: "peer-1", ETag: `"v2"`})

	holders, err := c.QueryKeyOwnership(t.Context(), "k")
	if err != nil {
		t.Fatalf("QueryKeyOwnership: %v", err)
	}

	if len(holders) != 1 {
		t.Fatalf("one node announced %q twice and is recorded as %d holders: %+v", "k", len(holders), holders)
	}
	if holders[0].ETag != `"v2"` {
		t.Errorf("holder is at ETag %s, want the newer %s — the older version is the one that node has "+
			"replaced", holders[0].ETag, `"v2"`)
	}
}

// TestRecordAnnouncement_KeepsDistinctPeers is the other half of the replacement rule: two *different*
// nodes holding one key are two holders, and collapsing them would throw away the choice warming exists
// to make.
func TestRecordAnnouncement_KeepsDistinctPeers(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "distinct-local")

	c.recordAnnouncement(types.KeyAnnouncement{Key: "k", NodeID: "peer-1", ETag: `"v1"`})
	c.recordAnnouncement(types.KeyAnnouncement{Key: "k", NodeID: "peer-2", ETag: `"v1"`})

	holders, err := c.QueryKeyOwnership(t.Context(), "k")
	if err != nil {
		t.Fatalf("QueryKeyOwnership: %v", err)
	}
	if len(holders) != 2 {
		t.Fatalf("two nodes announced %q and it is recorded as %d holder(s): %+v", "k", len(holders), holders)
	}
}

// TestRecordAnnouncement_RejectsUnusableClaims checks each field an announcement is useless without, and
// the self-claim.
//
// ETag is the one that matters beyond tidiness. A holder that cannot name its own version has nothing to
// announce, and recording the claim anyway would put an entry with an empty ETag in front of a warming
// path whose only defense against wrong bytes is comparing that ETag to the one it wanted.
func TestRecordAnnouncement_RejectsUnusableClaims(t *testing.T) {
	t.Parallel()

	const localID = "reject-local"

	tests := []struct {
		name string
		ann  types.KeyAnnouncement
	}{
		{"no key", types.KeyAnnouncement{NodeID: "peer-1", ETag: `"v1"`}},
		{"no node", types.KeyAnnouncement{Key: "k", ETag: `"v1"`}},
		{"no etag", types.KeyAnnouncement{Key: "k", NodeID: "peer-1"}},
		{"this node", types.KeyAnnouncement{Key: "k", NodeID: localID, ETag: `"v1"`}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := newAnnouncementCoordinator(t, localID)
			c.recordAnnouncement(tc.ann)

			if got := c.announcedKeys(); got != 0 {
				t.Errorf("an announcement with %s was recorded (%d key(s) retained): %+v", tc.name, got, tc.ann)
			}
		})
	}
}

// TestQueryKeyOwnership_FiltersExpiredClaims asserts the TTL is enforced on read rather than only by the
// background sweep.
//
// That split is the design: filtering on read is what makes the TTL exact — a query cannot see a claim
// one tick of the sweeper too late — and the sweep is only for memory. A test that waited for the sweeper
// would be testing the wrong one of the two, and would take a minute to do it.
func TestQueryKeyOwnership_FiltersExpiredClaims(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "expiry-local")
	c.config.AnnouncementTTL = time.Nanosecond

	c.recordAnnouncement(types.KeyAnnouncement{Key: "k", NodeID: "peer-1", ETag: `"v1"`})

	// Retained but not returned, which is the distinction announcedKeys exists to expose.
	if got := c.announcedKeys(); got != 1 {
		t.Fatalf("the claim was not recorded at all; announcedKeys is %d, want 1", got)
	}

	holders, err := c.QueryKeyOwnership(t.Context(), "k")
	if err != nil {
		t.Fatalf("QueryKeyOwnership: %v", err)
	}
	if len(holders) != 0 {
		t.Errorf("a claim past its %v TTL is still returned: %+v", c.announcementTTL(), holders)
	}
}

// TestSweepExpiredAnnouncements_ReclaimsTheKey is the memory half. QueryKeyOwnership already hides the
// expired claim, so what this asserts is that the map stops holding it — the leak the sweeper exists to
// prevent is a key nobody queries again, which no amount of read-side filtering reclaims.
func TestSweepExpiredAnnouncements_ReclaimsTheKey(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "sweep-local")
	c.config.AnnouncementTTL = time.Nanosecond

	c.recordAnnouncement(types.KeyAnnouncement{Key: "gone", NodeID: "peer-1", ETag: `"v1"`})
	c.recordAnnouncement(types.KeyAnnouncement{Key: "kept", NodeID: "peer-1", ETag: `"v1"`})

	if got := c.announcedKeys(); got != 2 {
		t.Fatalf("announcedKeys is %d before the sweep, want 2", got)
	}

	c.sweepExpiredAnnouncements()

	if got := c.announcedKeys(); got != 0 {
		t.Errorf("announcedKeys is %d after sweeping claims past a %v TTL, want 0", got, c.announcementTTL())
	}
}

// TestQueryKeyOwnership_OrdersByWhenThisNodeLearned asserts the freshest claim comes first, ordered by the
// *local* stamp.
//
// The peers here disagree wildly about the time, and that is the point: ordering by
// [types.KeyAnnouncement.CachedAt] would let a node with a fast clock sort itself to the front of every
// key in the cluster, which its own documentation forbids relying on. peer-1 announces with a CachedAt an
// hour in the future and is recorded first; peer-2 announces second with a CachedAt an hour in the past,
// and must still be preferred, because arriving later is the only freshness this node can actually
// observe.
func TestQueryKeyOwnership_OrdersByWhenThisNodeLearned(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "order-local")
	now := time.Now()

	c.recordAnnouncement(types.KeyAnnouncement{
		Key: "k", NodeID: "peer-1", ETag: `"v1"`, CachedAt: now.Add(time.Hour),
	})
	c.recordAnnouncement(types.KeyAnnouncement{
		Key: "k", NodeID: "peer-2", ETag: `"v2"`, CachedAt: now.Add(-time.Hour),
	})

	holders, err := c.QueryKeyOwnership(t.Context(), "k")
	if err != nil {
		t.Fatalf("QueryKeyOwnership: %v", err)
	}
	if len(holders) != 2 {
		t.Fatalf("got %d holders, want 2: %+v", len(holders), holders)
	}

	if holders[0].NodeID != "peer-2" {
		t.Errorf("holders are ordered %q then %q; want the most recently *received* claim first. "+
			"peer-1 claims a CachedAt an hour ahead of peer-2 — ordering by that field lets one node's "+
			"clock skew put it in front of the whole cluster",
			holders[0].NodeID, holders[1].NodeID)
	}
}

// TestQueryKeyOwnership_UncachedKeyIsEmptyAndNotNil pins the two things the interface contract says about
// the ordinary miss: a nil error, and a slice a caller can range over without a nil check. It says nothing
// about the bucket — only about what the cluster has cached — and returning an error here would tell a
// caller the query failed when the honest answer is that nobody has it.
func TestQueryKeyOwnership_UncachedKeyIsEmptyAndNotNil(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "miss-local")

	holders, err := c.QueryKeyOwnership(t.Context(), "never-announced")
	if err != nil {
		t.Fatalf("a key no peer has cached is not an error: %v", err)
	}
	if holders == nil {
		t.Error("holders is nil; the contract is an empty slice, so a caller need not distinguish the two")
	}
	if len(holders) != 0 {
		t.Errorf("got %d holders for a key nobody announced: %+v", len(holders), holders)
	}
}

// TestAnnounceKey_RefusesAnAnnouncementNoPeerCouldUse checks the two fields that make an announcement
// actionable, and — the part worth the test — that they are refused rather than sent.
//
// A receiver has no way to tell an announcement missing its ETag from one whose ETag is legitimately "".
// So sending it would push the problem onto every peer, each of which would record a holder that a
// warming path cannot verify. The error names the field, because the caller is the read path and the
// person reading it is debugging why warming does nothing.
func TestAnnounceKey_RefusesAnAnnouncementNoPeerCouldUse(t *testing.T) {
	t.Parallel()

	cm1, _ := startGossipPair(t, "refuse-node-a", "refuse-node-b")

	tests := []struct {
		name    string
		ann     types.KeyAnnouncement
		mustSay string
	}{
		{"no key", types.KeyAnnouncement{ETag: `"v1"`}, "unnamed key"},
		{"no etag", types.KeyAnnouncement{Key: "k"}, "ETag"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := cm1.GetCoordinator().AnnounceKey(t.Context(), tc.ann)
			if err == nil {
				t.Fatalf("AnnounceKey accepted an announcement with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("the error does not mention %q, so it does not name the missing field: %v",
					tc.mustSay, err)
			}
		})
	}
}

// TestAnnounceKey_WithoutGossipSaysSoRatherThanSucceeding is why these methods return an error where a nil
// would be simpler.
//
// #284 deleted a CacheReplicator whose success was indistinguishable from having sent nothing: it counted
// bytes as replicated when gossip was not running, and it survived four releases because the only test
// covering it asserted its field was non-nil. A caller told [types.ErrNotSupported] falls back to the
// object store, which is correct and merely slower; a caller told nil believes the cluster knows something
// it was never sent.
func TestAnnounceKey_WithoutGossipSaysSoRatherThanSucceeding(t *testing.T) {
	t.Parallel()

	// Not started, so no gossip socket exists — the state a mount is in before Start and after Stop.
	cm, err := NewClusterManager(testConfig(t, "nogossip-node"))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}
	cm.gossip = nil

	err = cm.GetCoordinator().AnnounceKey(t.Context(), types.KeyAnnouncement{Key: "k", ETag: `"v1"`})
	if err == nil {
		t.Fatal("AnnounceKey reported success with no gossip protocol; nothing was sent to anyone")
	}
	if !errors.Is(err, types.ErrNotSupported) {
		t.Errorf("error does not wrap types.ErrNotSupported, so a caller cannot tell this from a real "+
			"failure and fall back to the object store: %v", err)
	}
}

// TestCoordinatorWrapper_WithNoCoordinatorRefusesBoth covers the guards in the wrapper itself, which the
// unstarted-cluster test above cannot reach: a ClusterManager from [NewClusterManager] always has a
// coordinator, so a nil one means a value that did not come from the constructor.
//
// Guarded rather than left to panic because [ClusterManager.GetCoordinator] returns an interface, and an
// interface holding a nil pointer is not nil — so a caller's `if coord != nil` passes and the call
// dereferences. Both methods refuse, including the query, which is the one case where an empty slice would
// be wrong: it is the documented answer for a cold cluster, so giving it here would report "warming does
// not help" where the truth is "warming is not running".
func TestCoordinatorWrapper_WithNoCoordinatorRefusesBoth(t *testing.T) {
	t.Parallel()

	var coord types.DistributedCoordinator = &coordinatorWrapper{}

	err := coord.AnnounceKey(t.Context(), types.KeyAnnouncement{Key: "k", ETag: `"v1"`})
	if !errors.Is(err, types.ErrNotSupported) {
		t.Errorf("AnnounceKey = %v, want ErrNotSupported", err)
	}

	holders, err := coord.QueryKeyOwnership(t.Context(), "k")
	if !errors.Is(err, types.ErrNotSupported) {
		t.Errorf("QueryKeyOwnership = %v, want ErrNotSupported", err)
	}
	if holders != nil {
		t.Errorf("got %d holders alongside the error; a caller reading the slice first must find nothing",
			len(holders))
	}
}

// TestCoordinatorStats_ReportsRetainedAnnouncements asserts the count reaches the map an operator reads.
//
// The number is deliberately of *retained* keys rather than queryable ones — see
// [Coordinator.announcedKeys] — so the gap between it and a query is how far behind the sweep is running.
func TestCoordinatorStats_ReportsRetainedAnnouncements(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "stats-local")

	for i := range 3 {
		c.recordAnnouncement(types.KeyAnnouncement{
			Key: fmt.Sprintf("k-%d", i), NodeID: "peer-1", ETag: `"v1"`,
		})
	}

	stats := c.GetStats()
	got, ok := stats["announced_keys"]
	if !ok {
		t.Fatalf("GetStats has no announced_keys key, so the subsystem is invisible to an operator: %v", stats)
	}
	if got != 3 {
		t.Errorf("announced_keys is %v, want 3", got)
	}
}

// newAnnouncementCoordinator builds a Coordinator with a cluster and no network.
//
// The announcement map, its TTL, and the eviction rules are all local state, so these tests need a node
// identity — recordAnnouncement compares against it to reject a self-claim — and nothing else.
// startGossipPair exists for the half that genuinely crosses the wire and is used there; paying for two
// UDP sockets to test a map would make every one of these tests slower and none of them stronger.
func newAnnouncementCoordinator(t *testing.T, nodeID string) *Coordinator {
	t.Helper()

	cm, err := NewClusterManager(testConfig(t, nodeID))
	if err != nil {
		t.Fatalf("NewClusterManager: %v", err)
	}

	return cm.coordinator
}

// TestRecordAnnouncement_BoundsTheMap drives the map past maxAnnouncedKeys and asserts it stops growing.
//
// The bound has to be exercised rather than reasoned about, because what it protects against is the one
// failure mode a short test never reaches on its own: a mount that runs for weeks accumulating one entry
// per key any peer has ever cached, which is a leak proportional to the bucket rather than to the cache.
//
// Every claim here is live, so this is specifically the second, harder half of
// [Coordinator.evictAnnouncementsLocked] — the path where expiring nothing frees nothing and age is the
// only thing left to choose by.
func TestRecordAnnouncement_BoundsTheMap(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "bound-local")

	for i := range maxAnnouncedKeys + 1 {
		c.recordAnnouncement(types.KeyAnnouncement{
			Key: fmt.Sprintf("k-%d", i), NodeID: "peer-1", ETag: `"v1"`,
		})
	}

	got := c.announcedKeys()
	if got > maxAnnouncedKeys {
		t.Errorf("announcedKeys is %d after %d distinct keys, over the %d bound: the map grows without "+
			"limit for the lifetime of the mount", got, maxAnnouncedKeys+1, maxAnnouncedKeys)
	}

	// A tenth dropped and one inserted, so a bound that worked leaves roughly nine tenths. Asserted
	// loosely, because the exact figure is amortization policy; asserted at all, because an eviction that
	// emptied the map would satisfy the check above while destroying the subsystem's usefulness.
	if floor := maxAnnouncedKeys / 2; got < floor {
		t.Errorf("announcedKeys is %d, under %d: eviction is discarding far more than the tenth it "+
			"should, so warming loses claims it could have used", got, floor)
	}
}

// TestRecordAnnouncement_EvictsExpiredBeforeLiveClaims asserts the cheap half of eviction runs first.
//
// Dropping an expired claim costs nothing — QueryKeyOwnership filters it out anyway — while dropping a
// live one costs a read from S3. So a map that overflows while full of expired entries should lose exactly
// those and stop, rather than paying the sort and dropping a tenth of the claims warming could still use.
//
// The two paths are distinguishable by count, which is what makes this an assertion rather than a
// restatement: expiring everything leaves 1 key, and falling through to the oldest-tenth rule would leave
// roughly nine tenths of 65536. The entries are back-dated in the map instead of by waiting, since the
// alternative is a test that sleeps past a real TTL.
func TestRecordAnnouncement_EvictsExpiredBeforeLiveClaims(t *testing.T) {
	t.Parallel()

	c := newAnnouncementCoordinator(t, "evict-order-local")

	for i := range maxAnnouncedKeys {
		c.recordAnnouncement(types.KeyAnnouncement{
			Key: fmt.Sprintf("stale-%d", i), NodeID: "peer-1", ETag: `"v1"`,
		})
	}

	stale := time.Now().Add(-2 * defaultAnnouncementTTL)
	c.announcementsMu.Lock()
	for key, holders := range c.announcements {
		for i := range holders {
			holders[i].recordedAt = stale
		}
		c.announcements[key] = holders
	}
	c.announcementsMu.Unlock()

	c.recordAnnouncement(types.KeyAnnouncement{Key: "fresh", NodeID: "peer-1", ETag: `"v1"`})

	if got := c.announcedKeys(); got != 1 {
		t.Errorf("announcedKeys is %d after overflowing a map of expired claims, want 1: eviction is "+
			"discarding live claims while %d free ones sit there expired", got, maxAnnouncedKeys)
	}

	holders, err := c.QueryKeyOwnership(t.Context(), "fresh")
	if err != nil {
		t.Fatalf("QueryKeyOwnership: %v", err)
	}
	if len(holders) != 1 {
		t.Errorf("got %d holders of %q, want 1; the claim that triggered the eviction was itself dropped",
			len(holders), "fresh")
	}
}
