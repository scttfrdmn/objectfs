package distributed

// Cache warming on join (#143): a node that receives a join tells the joiner which keys it holds, so the
// new node starts with a view of what is hot in this cluster instead of discovering it one cache miss at
// a time.
//
// # What crosses the wire, and what does not
//
// Announcements. Metadata, not object bytes — the same split [MessageTypeCacheAnnounce] is under, and for
// the same measured reason: a sealed datagram at the default limit can carry 5802 bytes of object, so a
// single 128 KiB read is 21× over (#399). What the joiner does with them is record them in the ownership
// map, exactly as if the peers had announced each key normally; the bytes still come from S3 when a read
// actually wants them. The value is knowing *which* keys are worth reading, which is precisely what a
// cold node cannot learn on its own.
//
// This is why the issue's `cache.Warmup(keys)` call is not made here. [cache.MultiLevelCache.Warmup] is
// one GetObject per key, so calling it on a warmup message would have a joining node issue a whole-object
// GET for every key in it — turning the cold-start cost this feature exists to reduce into a burst of
// egress before the node serves a single read, and, on a large-object workload, gigabytes of it. #143's
// own acceptance criterion ("100% hit rate before any S3 request") cannot be met by that function under
// any bound, since Warmup *is* the S3 traffic. Recording the announcements meets what the criterion was
// reaching for and costs nothing.
//
// # Why the bound is bytes and never a count
//
// #143 specifies "max 256 entries per warmup message to bound UDP payload". Measured against the real
// seal path at the default 8192-byte limit, with 52-character keys shaped like a research bucket's: 256
// announcements seal to 65631 bytes, 8× over, so [GossipProtocol.sendMessage] refuses the datagram
// outright and a joining node warms nothing at all — with the failure visible only in whatever the caller
// does with the error. 31 of them fit.
//
// And 31 is not a constant to write down instead. It is a function of key length, so a deployment with
// long prefixes gets fewer and one with short keys gets more; TestMarshalWarmupChunk_TheCountVariesWithKeyLength
// asserts exactly that, because it is the reason no number here would be right. A count cannot bound a
// byte limit. So each chunk is grown one entry at a time and the whole message sealed to check, which is
// what [GossipProtocol.syncChunkFits] already does for membership sync.

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// maxWarmupDatagrams bounds how many warmup messages one join produces.
//
// This is a bound on a burst, which is a real resource, rather than a proxy for a byte limit — the
// per-message size is decided by measurement below. A node holding 65536 keys would otherwise answer a
// single join with two thousand datagrams at the 31 entries each fits, aimed at a node that has been alive
// for milliseconds and is simultaneously receiving a membership sync.
//
// Four is sized against what it is for: ~124 keys at that measured density, which is a working set rather
// than a bucket listing. The joiner learns the rest of the cluster's hot set the ordinary way, from the
// announcements peers make as they read, over at most one announcementTTL; warmup exists to shorten that,
// not to replace it. What the joiner gets here is the freshest keys — see [Coordinator.recentHoldings] — so
// the prefix that fits is the useful part rather than an arbitrary one.
const maxWarmupDatagrams = 4

// maxWarmupCandidates bounds the slice built to pack from.
//
// Only a guard against materializing 65536 announcements to send a few dozen; the number of entries
// actually sent is decided by sealed size and maxWarmupDatagrams. Generously above what four datagrams
// can hold at any plausible key length, so it never becomes the effective bound.
const maxWarmupCandidates = 1024

// sendCacheWarmup tells the node at addr which keys this node holds.
//
// Best-effort throughout, and it returns nothing: a joiner that hears no warmup reads from S3, which is
// correct and merely slower. Every failure is logged at Debug for that reason — with one exception, an
// entry too large to send at all, which is a misconfiguration rather than a lost packet.
func (gp *GossipProtocol) sendCacheWarmup(addr string) {
	coordinator := gp.cluster.getCoordinator()
	if coordinator == nil {
		return
	}

	candidates := coordinator.recentHoldings(maxWarmupCandidates)

	gp.mu.RLock()
	from := gp.localNode.ID
	gp.mu.RUnlock()

	sent := 0
	for range maxWarmupDatagrams {
		// Also the "this node holds nothing" case, which is every node that has just started, and #143 asks
		// for it explicitly: an empty warmup message is a datagram a receiver has to parse to learn there is
		// nothing in it. There is deliberately no separate guard above for that — a second check of the same
		// condition is one a test cannot distinguish from this one, so removing it would look untested while
		// being covered, which is worse than having one place to look.
		if len(candidates) == 0 {
			break
		}

		payload, fitted, err := gp.marshalWarmupChunk(candidates)
		if err != nil {
			slog.Debug("could not build a cache warmup message", "peer", addr, "error", err)

			return
		}

		if fitted == 0 {
			// One announcement alone does not fit a datagram. No chunking can help, and skipping it
			// silently would drop the freshest key in the cluster with nothing logged — so it is reported
			// and the rest are attempted, since the entries behind it may be shorter.
			slog.Warn("a single cache announcement does not fit max_gossip_packet, so it cannot be sent "+
				"to a joining node; that node will read this key from the object store",
				"key", candidates[0].Key, "max_gossip_packet", gp.config.MaxGossipPacket)

			candidates = candidates[1:]

			continue
		}

		if err := gp.sendMessage(addr, &GossipMessage{
			Type:      MessageTypeCacheWarmup,
			From:      from,
			Timestamp: time.Now(),
			MessageID: gp.generateMessageID(),
			Data:      payload,
		}); err != nil {
			slog.Debug("could not send a cache warmup message to a joining node",
				"peer", addr, "keys", fitted, "error", err)

			return
		}

		sent += fitted
		candidates = candidates[fitted:]
	}

	// Logged rather than dropped quietly: a bound that silently truncates reads as "everything was sent"
	// to whoever debugs a joiner that is warm for some keys and cold for others.
	if len(candidates) > 0 {
		slog.Debug("sent a joining node the freshest of this node's cached keys; the rest will reach it "+
			"through ordinary announcements",
			"peer", addr, "sent", sent, "held_back", len(candidates))
	}
}

// marshalWarmupChunk packs as many of anns as fit one sealed datagram, returning the payload and how many
// entries went into it. A zero count with a nil error means the first entry alone does not fit.
//
// The measurement is of the whole sealed message, not of the payload: seal wraps it in JSON with a hex MAC
// and base64-encodes the data field, so the ratio between the two is not a constant anyone should
// hardcode — it was measured at 38.9% for object bytes, where base64 alone would suggest 33%. Growing the
// chunk one entry at a time and sealing each candidate is what [GossipProtocol.syncChunkFits] does for
// membership, and this runs on the same path: once per join, not per gossip round.
func (gp *GossipProtocol) marshalWarmupChunk(anns []types.KeyAnnouncement) ([]byte, int, error) {
	var (
		best  []byte
		count int
	)

	for i := range anns {
		payload, err := json.Marshal(&CacheWarmupMessage{Keys: anns[:i+1]})
		if err != nil {
			return nil, 0, err
		}

		fits, err := gp.warmupChunkFits(payload)
		if err != nil {
			return nil, 0, err
		}

		if !fits {
			break
		}

		best = payload
		count = i + 1
	}

	return best, count, nil
}

// warmupChunkFits reports whether a warmup message carrying payload would fit MaxGossipPacket once
// sealed.
//
// The same message sendMessage would build, so the measurement includes the envelope, the MAC and the
// message ID rather than a guess at their combined size. MessageID is generated here and discarded: it
// varies in content but not in length.
func (gp *GossipProtocol) warmupChunkFits(payload []byte) (bool, error) {
	gp.mu.RLock()
	from := gp.localNode.ID
	gp.mu.RUnlock()

	sealed, err := gp.auth.seal(&GossipMessage{
		Type:      MessageTypeCacheWarmup,
		From:      from,
		Timestamp: time.Now(),
		MessageID: gp.generateMessageID(),
		Data:      payload,
	})
	if err != nil {
		return false, err
	}

	return len(sealed) <= gp.config.MaxGossipPacket, nil
}

// handleCacheWarmup records what a peer says it holds, as if it had announced each key individually.
//
// Through [Coordinator.recordAnnouncement] rather than into the map directly, which is what makes the two
// paths impossible to drift apart: that function is where a self-claim is refused, where an entry replaces
// an earlier one from the same node, where the bound and the local timestamp are applied, and where an
// announcement missing its ETag is discarded. A warmup message is a batch of announcements and gets
// exactly the same treatment.
//
// The envelope's sender overrides each entry's NodeID for the reason [GossipProtocol.handleCacheAnnounce]
// does it: both are authenticated by the same MAC, so this is not a defense against an outsider but
// against a *member* claiming that some third node holds a key — which would send the joiner to fetch
// bytes that node never cached, with no way for it to correct the record.
//
// Not re-broadcast, and not answered with a warmup of the joiner's own. It has nothing to tell anyone yet,
// which is why it was sent one.
func (gp *GossipProtocol) handleCacheWarmup(msg *GossipMessage) {
	coordinator := gp.cluster.getCoordinator()
	if coordinator == nil {
		return
	}

	var m CacheWarmupMessage
	if err := json.Unmarshal(msg.Data, &m); err != nil {
		slog.Debug("discarding a malformed cache warmup message", "peer", msg.From, "error", err)

		return
	}

	for _, ann := range m.Keys {
		if msg.From != "" {
			ann.NodeID = msg.From
		}

		coordinator.recordAnnouncement(ann)
	}
}
