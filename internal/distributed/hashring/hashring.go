// Package hashring assigns keys to cluster nodes so that the same key lands on the same node, and so
// that adding or removing a node moves as few keys as possible.
//
// That second property is the point, and it is what "consistent" means here. A cache is only worth
// having if a key's owner is stable: if the mapping changes, the data sitting on the old owner is
// dead weight and the new owner has to fetch from S3. With N nodes and a modulo assignment, losing
// one node remaps roughly every key. With the scheme in this package, it remaps only the keys the
// departed node owned.
//
// The implementation is rendezvous hashing, also called highest random weight (HRW): to place a key,
// hash (node, key) for every node and take the highest score. Nothing is precomputed and there is no
// ring structure to rebalance, so Add and Remove are trivial and there are no virtual nodes to tune.
// The cost is that a lookup is O(n) in the number of nodes rather than O(log n) — which for the node
// counts a filesystem cluster actually has is not a cost worth a more complicated data structure.
// See the benchmark.
//
// Ties are broken by node ID, so a lookup does not depend on the order nodes were added or on Go's
// map iteration order. That determinism is the whole property being bought and is asserted directly
// by the tests: it is the specific thing the previous implementation — a slice prefix, over a slice
// built by ranging a map — did not have.
package hashring

import (
	"sort"

	"github.com/cespare/xxhash/v2"
)

// HashRing maps keys to nodes.
//
// An implementation must be deterministic: for the same key and the same set of nodes, Lookup and
// LookupN return the same answer, whatever order the nodes were added in.
type HashRing interface {
	// Add adds a node. Adding a node already present is a no-op.
	Add(nodeID string)

	// Remove removes a node. Removing a node not present is a no-op.
	Remove(nodeID string)

	// Lookup returns the node that owns key, or "" if there are no nodes.
	Lookup(key string) string

	// LookupN returns the n nodes with the highest scores for key, in descending score order, so
	// that element 0 is the primary owner and the rest are replicas in a stable preference order.
	// Fewer than n are returned when the ring holds fewer than n nodes, and the result is never nil
	// for n > 0 — an empty ring yields an empty slice, not a panic.
	LookupN(key string, n int) []string
}

// RendezvousRing is a HashRing using highest-random-weight hashing.
//
// It holds no lock. A ring is built from a snapshot of the alive nodes and used for one selection,
// which is how the coordinator uses it; sharing one across goroutines while calling Add or Remove
// needs external synchronization. Stated rather than solved with a mutex nothing would contend on.
type RendezvousRing struct {
	// nodes is kept sorted so that iteration order is fixed. Determinism does not depend on this —
	// ties are broken explicitly — but it makes the invariant hold by construction rather than by
	// argument, and it makes Add/Remove/String cheap to reason about.
	nodes []string
}

// New returns a ring holding the given nodes. Duplicates are collapsed.
func New(nodes ...string) *RendezvousRing {
	r := &RendezvousRing{}
	for _, n := range nodes {
		r.Add(n)
	}

	return r
}

// Len returns the number of nodes in the ring.
func (r *RendezvousRing) Len() int { return len(r.nodes) }

// Nodes returns the ring's nodes in sorted order. The returned slice is a copy.
func (r *RendezvousRing) Nodes() []string {
	out := make([]string, len(r.nodes))
	copy(out, r.nodes)

	return out
}

// Add adds a node, keeping the node list sorted and free of duplicates.
func (r *RendezvousRing) Add(nodeID string) {
	i := sort.SearchStrings(r.nodes, nodeID)
	if i < len(r.nodes) && r.nodes[i] == nodeID {
		return
	}

	r.nodes = append(r.nodes, "")
	copy(r.nodes[i+1:], r.nodes[i:])
	r.nodes[i] = nodeID
}

// Remove removes a node.
func (r *RendezvousRing) Remove(nodeID string) {
	i := sort.SearchStrings(r.nodes, nodeID)
	if i >= len(r.nodes) || r.nodes[i] != nodeID {
		return
	}

	r.nodes = append(r.nodes[:i], r.nodes[i+1:]...)
}

// Lookup returns the owner of key, or "" when the ring is empty.
//
// This is LookupN(key, 1) without allocating, since it is the common case: a read picks one node.
func (r *RendezvousRing) Lookup(key string) string {
	if len(r.nodes) == 0 {
		return ""
	}

	best := scored{node: r.nodes[0], score: score(r.nodes[0], key)}

	// outranks, not an inlined comparison, so that a tie resolves the same way here as it does in
	// LookupN. Two implementations of one rule is how the two entry points would drift apart.
	for _, node := range r.nodes[1:] {
		if cand := (scored{node: node, score: score(node, key)}); outranks(cand, best) {
			best = cand
		}
	}

	return best.node
}

// LookupN returns the n highest-scoring nodes for key, in descending score order.
func (r *RendezvousRing) LookupN(key string, n int) []string {
	if n <= 0 || len(r.nodes) == 0 {
		return []string{}
	}

	all := make([]scored, len(r.nodes))
	for i, node := range r.nodes {
		all[i] = scored{node: node, score: score(node, key)}
	}

	sort.Slice(all, func(i, j int) bool { return outranks(all[i], all[j]) })

	if n > len(all) {
		n = len(all)
	}

	out := make([]string, n)
	for i := range n {
		out[i] = all[i].node
	}

	return out
}

// scored is a node with its weight for one key.
type scored struct {
	node  string
	score uint64
}

// outranks reports whether a should be placed before b: higher score wins, and equal scores are
// broken by the lower node ID.
//
// The tiebreak is the reason this is a named function rather than an inline closure. Two nodes
// colliding on a 64-bit score is not reachable from any input a test can construct, so a test
// driving Lookup cannot exercise it — and sort.Slice is not stable, so without the tiebreak a
// collision would resolve to whichever element the sort happened to leave first, which is exactly
// the input-order dependence this package exists to remove. Extracting it makes the rule assertable
// directly instead of relying on a collision that will not occur.
func outranks(a, b scored) bool {
	if a.score != b.score {
		return a.score > b.score
	}

	return a.node < b.node
}

// score is the rendezvous weight of a (node, key) pair.
//
// The separator matters: without it, node "a" with key "bc" and node "ab" with key "c" hash the same
// bytes and therefore score the same, which would make one node's ownership depend on another node's
// name. "#" is used because a node ID is a hostname or a generated "node-<hex>" identifier and
// contains neither "#" nor a NUL.
//
// xxhash rather than a cryptographic hash because this is a placement decision, not a security
// boundary: an adversary who can choose keys can already choose which node to talk to. It is also
// already in the module graph as a transitive dependency, so this adds no new one.
func score(nodeID, key string) uint64 {
	h := xxhash.New()
	_, _ = h.WriteString(nodeID) // xxhash's Write never returns an error
	_, _ = h.WriteString("#")
	_, _ = h.WriteString(key)

	return h.Sum64()
}
