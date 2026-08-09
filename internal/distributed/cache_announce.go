package distributed

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// heldKey is one peer's claim to hold one key, with the moment this node learned of it.
//
// recordedAt is stamped from the *local* clock on receipt, and is deliberately not
// [types.KeyAnnouncement.CachedAt]. That field's own documentation forbids what expiry needs it for:
// it is the holder's clock, nodes are not synchronized, and it "must never be compared against a local
// deadline to decide validity". A peer whose clock runs an hour behind would have every announcement
// expire on arrival; one running an hour ahead would have none of them ever expire. The local stamp
// measures the only thing this node can measure — how long ago the message arrived.
//
// CachedAt is still carried, in ann, because it is what a *recipient comparing two holders* wants: of
// two peers claiming the same ETag, the one that cached it later is likelier to still hold it. Advisory
// for that, unusable for expiry.
type heldKey struct {
	ann        types.KeyAnnouncement
	recordedAt time.Time
}

// defaultAnnouncementTTL is how long a peer's claim to hold a key is believed.
//
// Five minutes is a bound on wasted work rather than on correctness: what an expired announcement costs
// is one attempt to warm from a peer that has since evicted, and [types.DistributedCoordinator] already
// requires every caller to check what it receives against the ETag it wanted, precisely because a
// holder can evict, replace, or never have held what it announced. So the failure mode of believing a
// stale claim is a fetch that misses, and the failure mode of forgetting a live one is a read from S3 —
// both merely slower.
//
// It is sized against the read patterns warming exists for. A traversal that walks a dataset over
// minutes wants a claim made at its start to still be good at its end; a claim older than that names a
// key the cluster has moved on from, and following it is likelier to waste a round trip than to save
// one.
const defaultAnnouncementTTL = 5 * time.Minute

// maxAnnouncedKeys bounds how many distinct keys this node remembers peer claims for.
//
// The bound has to exist and it has to be on keys rather than on total entries: the number of peers is
// bounded by the cluster, so entries per key are bounded too, while the number of keys a busy cluster
// announces over an uptime is not. Without it a long-lived mount accumulates one entry per key any peer
// has ever cached, which is a leak proportional to the bucket.
//
// 65536 keys at a key string plus a node ID and an ETag is a few tens of megabytes at the extreme,
// against a cache measured in gigabytes. Overflow drops the oldest claims, which costs a read from S3.
const maxAnnouncedKeys = 65536

// announcementCleanupInterval is how often expired claims are swept.
//
// The sweep is for memory, not for correctness: [Coordinator.QueryKeyOwnership] filters expired entries
// on every read, so a caller never sees one whether the sweep has run or not. That split is deliberate.
// Filtering on read is what makes the TTL exact — it cannot be late — and sweeping on a timer is what
// keeps a key nobody ever queries again from being remembered forever.
const announcementCleanupInterval = time.Minute

// recordAnnouncement stores a peer's claim to hold ann's bytes, replacing any earlier claim from the
// same node for the same key.
//
// Replace rather than append, because a node announcing a key twice has not become two holders: the
// second announcement is the current state of one cache, and keeping the first would offer a caller an
// ETag that node has already replaced.
//
// It refuses an announcement naming this node. Such a message is what a node's own broadcast looks like
// coming back — and, more importantly, admitting it would corrupt the answer to the only question
// [Coordinator.QueryKeyOwnership] is asked: a caller queries *because it just missed its own cache*, so
// a self-claim is a holder guaranteed not to hold, returned in place of a peer that does. See
// [Coordinator.AnnounceKey], which for the same reason does not record what it sends.
func (c *Coordinator) recordAnnouncement(ann types.KeyAnnouncement) {
	// Every field checked here is one the announcement is useless without, and ETag is the one worth
	// being explicit about: [types.KeyAnnouncement] requires it where an invalidation does not, because
	// a peer that fetches bytes it cannot place against a version hands them to a reading process as
	// file content. A holder that cannot name its own version has nothing to announce.
	if ann.Key == "" || ann.NodeID == "" || ann.ETag == "" {
		return
	}

	if ann.NodeID == c.cluster.GetNodeID() {
		return
	}

	now := time.Now()

	c.announcementsMu.Lock()
	defer c.announcementsMu.Unlock()

	if c.announcements == nil {
		c.announcements = make(map[string][]heldKey)
	}

	holders := c.announcements[ann.Key]
	for i := range holders {
		if holders[i].ann.NodeID == ann.NodeID {
			holders[i] = heldKey{ann: ann, recordedAt: now}
			c.announcements[ann.Key] = holders

			return
		}
	}

	// Bounded before the insert, and only when this is a new key — a replacement above cannot grow the
	// map, so charging it the eviction cost would drop live claims for no gain.
	if len(c.announcements) >= maxAnnouncedKeys {
		c.evictAnnouncementsLocked(now)
	}

	c.announcements[ann.Key] = append(holders, heldKey{ann: ann, recordedAt: now})
}

// evictAnnouncementsLocked makes room in the announcements map. c.announcementsMu must be held.
//
// Expired entries first, since dropping those costs nothing at all — they would be filtered out of
// every read anyway. Only if that frees nothing does it drop live claims, oldest first, because the
// oldest claim is the one likeliest to have been evicted by its holder already.
//
// Sorting to find the oldest is O(n log n) against a map that has just reached 65536 keys, and it runs
// only on overflow. The alternative — the invalidation ledger's "drop an arbitrary tenth" — is right
// there because that ledger records versions with no age to prefer between; these have one, and
// dropping the freshest claim by luck would discard exactly the entry warming is most likely to use.
func (c *Coordinator) evictAnnouncementsLocked(now time.Time) {
	ttl := c.announcementTTL()

	for key, holders := range c.announcements {
		live := holders[:0]
		for _, held := range holders {
			if now.Sub(held.recordedAt) <= ttl {
				live = append(live, held)
			}
		}

		if len(live) == 0 {
			delete(c.announcements, key)

			continue
		}

		c.announcements[key] = live
	}

	if len(c.announcements) < maxAnnouncedKeys {
		return
	}

	// Still full: every entry is live, so age is the only thing left to choose by. A key's age is its
	// freshest claim, since that is what a query would return.
	type keyAge struct {
		key      string
		freshest time.Time
	}

	ages := make([]keyAge, 0, len(c.announcements))
	for key, holders := range c.announcements {
		newest := holders[0].recordedAt
		for _, held := range holders[1:] {
			if held.recordedAt.After(newest) {
				newest = held.recordedAt
			}
		}

		ages = append(ages, keyAge{key: key, freshest: newest})
	}

	sort.Slice(ages, func(i, j int) bool { return ages[i].freshest.Before(ages[j].freshest) })

	// A tenth, so overflow is amortized rather than paid on every subsequent insert.
	for _, entry := range ages[:max(1, len(ages)/10)] {
		delete(c.announcements, entry.key)
	}
}

// announcementTTL is how long a claim is believed, from configuration or the default.
//
// This is the *only* place the default is applied, and deliberately not applyConfigDefaults, where the
// cluster's other sixteen defaults live. A [Coordinator] can be constructed with a ClusterConfig that
// never passed through that function — every test in this package does exactly that — and a zero TTL
// there is not a slower cluster but a broken one: every announcement expires the instant it arrives, so
// QueryKeyOwnership always returns empty and the subsystem looks like it works while doing nothing.
// Reading the default at the point of use cannot be bypassed that way.
//
// One place, for the reason recorded beside the deleted default literal in cluster.go: MaxGossipPacket
// was once defaulted twice, the copies disagreed, and fixing one left the other stale (#277).
func (c *Coordinator) announcementTTL() time.Duration {
	if c.config != nil && c.config.AnnouncementTTL > 0 {
		return c.config.AnnouncementTTL
	}

	return defaultAnnouncementTTL
}

// AnnounceKey tells peers this node has ann's bytes cached.
//
// It broadcasts and does not record locally, which is a deliberate departure from #140's specification.
// The map answers [Coordinator.QueryKeyOwnership], and that is called *because a read missed this node's
// own cache* — so an entry naming this node is a holder guaranteed not to hold, offered in place of a
// peer that might. The local cache is already the authority on what this node has; a second copy of that
// answer in the ownership map could only ever disagree with it.
//
// Failing when gossip is not running rather than returning nil, for the reason in
// [ClusterManager.invalidateCacheKey]: #284 deleted a replicator whose success was indistinguishable
// from having sent nothing. A caller told [types.ErrNotSupported] falls back to S3, which is correct.
//
// A running cluster with no peers is a nil error and no sends, since there is nobody to tell.
func (c *Coordinator) AnnounceKey(_ context.Context, ann types.KeyAnnouncement) error {
	// Both required, and the ETag is the one that needs saying. [types.KeyAnnouncement] demands it where
	// an invalidation does not: a peer that fetches bytes it cannot place against a version hands them to
	// a reading process as file content. Refused rather than sent with the field empty, because a
	// receiver has no way to tell an announcement missing its version from one whose version is "".
	if ann.Key == "" {
		return fmt.Errorf("refusing to announce an unnamed key: an announcement names the key a peer " +
			"would fetch, so there is nothing to send")
	}

	if ann.ETag == "" {
		return fmt.Errorf("refusing to announce %q with no ETag: a peer cannot place bytes it fetches "+
			"against a version of the object, and would hand them to a reading process as file content",
			ann.Key)
	}

	if c.cluster == nil {
		return fmt.Errorf("cannot announce %q: this coordinator has no cluster: %w", ann.Key,
			types.ErrNotSupported)
	}

	c.cluster.mu.RLock()
	gossip := c.cluster.gossip
	nodeID := c.cluster.nodeID
	c.cluster.mu.RUnlock()

	if gossip == nil {
		return fmt.Errorf("cannot announce %q to peers: this cluster has no gossip protocol, so it has "+
			"no way to reach them: %w", ann.Key, types.ErrNotSupported)
	}

	// Overwritten rather than trusted from the caller. A node announcing under another node's ID would
	// send peers to fetch from a host that never cached the bytes, and the caller here is the read path,
	// which has no reason to name anyone but itself.
	ann.NodeID = nodeID

	if ann.CachedAt.IsZero() {
		ann.CachedAt = time.Now()
	}

	payload, err := json.Marshal(ann)
	if err != nil {
		return fmt.Errorf("marshaling the announcement for %q: %w", ann.Key, err)
	}

	// Wrapped so the key is in the message. broadcastMessage names the message *type*, which is all it
	// knows, and "cannot broadcast cache_announce" in a log tells an operator debugging cold reads nothing
	// about which object went unannounced. The %w keeps [types.ErrNotSupported] reachable through both
	// layers, which is what a caller deciding whether to fall back permanently tests for.
	if err := gossip.broadcastMessage(&GossipMessage{
		Type:      MessageTypeCacheAnnounce,
		From:      nodeID,
		Timestamp: time.Now(),
		MessageID: gossip.generateMessageID(),
		Data:      payload,
	}); err != nil {
		return fmt.Errorf("announcing %q to peers: %w", ann.Key, err)
	}

	return nil
}

// QueryKeyOwnership reports the peers claiming to hold key, freshest first.
//
// Local map only, no network call: the announcements arrived by gossip as peers made them, so this is a
// read of what has already been learned. That is what makes it callable from a cache miss — a read that
// had to wait for a round of queries before falling back to S3 would be slower than not warming at all.
//
// Ordered by when *this node* recorded each claim, never by [types.KeyAnnouncement.CachedAt]. Peer
// clocks are not synchronized, so ordering by CachedAt lets a node with a fast clock sort itself to the
// front of every key in the cluster. See [heldKey.recordedAt].
//
// Expired claims are filtered here rather than only swept on a timer, so the TTL cannot be late.
//
// An empty slice with a nil error is the ordinary answer for a key no peer has cached, and per the
// interface contract callers must read it as "fetch from the object store" rather than "the key does not
// exist" — this reports what is cached in the cluster and says nothing about what the bucket holds.
func (c *Coordinator) QueryKeyOwnership(_ context.Context, key string) ([]types.KeyAnnouncement, error) {
	if key == "" {
		return nil, fmt.Errorf("cannot query which peers hold an unnamed key")
	}

	ttl := c.announcementTTL()
	now := time.Now()

	c.announcementsMu.RLock()
	holders := c.announcements[key]

	live := make([]heldKey, 0, len(holders))
	for _, held := range holders {
		if now.Sub(held.recordedAt) <= ttl {
			live = append(live, held)
		}
	}
	c.announcementsMu.RUnlock()

	sort.Slice(live, func(i, j int) bool { return live[i].recordedAt.After(live[j].recordedAt) })

	// Non-nil even when empty, matching the interface's "empty slice and a nil error is the ordinary
	// answer" — a caller ranging over it should not have to distinguish the two.
	out := make([]types.KeyAnnouncement, 0, len(live))
	for _, held := range live {
		out = append(out, held.ann)
	}

	return out, nil
}

// cleanupAnnouncements sweeps expired claims until the coordinator stops.
//
// For memory only. QueryKeyOwnership filters on read, so nothing this reclaims was reachable by a
// caller — see [announcementCleanupInterval].
func (c *Coordinator) cleanupAnnouncements(ctx context.Context) {
	ticker := time.NewTicker(announcementCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.sweepExpiredAnnouncements()
		}
	}
}

// sweepExpiredAnnouncements drops every claim past its TTL, and every key left with none.
func (c *Coordinator) sweepExpiredAnnouncements() {
	ttl := c.announcementTTL()
	now := time.Now()

	c.announcementsMu.Lock()
	defer c.announcementsMu.Unlock()

	for key, holders := range c.announcements {
		live := holders[:0]
		for _, held := range holders {
			if now.Sub(held.recordedAt) <= ttl {
				live = append(live, held)
			}
		}

		if len(live) == 0 {
			delete(c.announcements, key)

			continue
		}

		c.announcements[key] = live
	}
}

// announcedKeys is how many keys this node currently holds peer claims for, expired ones included.
//
// Expired ones included, because this reports what is *retained* rather than what a query would return,
// and the gap between the two is the thing worth being able to see: it is how far behind the sweep is
// running. See [Coordinator.GetStats], which is where an operator reads it.
func (c *Coordinator) announcedKeys() int {
	c.announcementsMu.RLock()
	defer c.announcementsMu.RUnlock()

	return len(c.announcements)
}

// handleCacheAnnounce records a peer's claim to hold a key.
//
// The shape follows [GossipProtocol.handleCacheInvalidate] — validate, then hand to the subsystem that
// owns the state — with one difference worth naming: there is no applied-once ledger here, and none is
// needed. An invalidation is an *action*, so applying a retransmission of an older one throws away bytes
// legitimately re-cached since. An announcement is a *fact* about a (key, node) pair, and recording the
// same fact twice is idempotent by construction — recordAnnouncement replaces that node's entry rather
// than appending. A retransmission overwrites an identical value.
//
// Not re-broadcast. One hop to every alive peer is what [GossipProtocol.broadcastMessage] does, and a
// peer that never hears an announcement reads from S3, which is correct and merely slower. Forwarding
// would turn each announcement into a flood over a UDP transport with no de-duplication above the replay
// window.
func (gp *GossipProtocol) handleCacheAnnounce(msg *GossipMessage) {
	coordinator := gp.cluster.getCoordinator()
	if coordinator == nil {
		return
	}

	var ann types.KeyAnnouncement
	if err := json.Unmarshal(msg.Data, &ann); err != nil {
		slog.Debug("discarding a malformed cache announcement", "peer", msg.From, "error", err)

		return
	}

	// The envelope's sender wins over the payload's claim about itself. Both are authenticated by the
	// same MAC, so this is not a defense against an outsider — it is a defense against a *member*
	// announcing that some third node holds a key, which would send peers to fetch bytes that node never
	// cached and has no way to correct.
	if msg.From != "" {
		ann.NodeID = msg.From
	}

	coordinator.recordAnnouncement(ann)
}
