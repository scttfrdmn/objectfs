package hashring

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The property under test is not "keys are spread evenly" — a slice prefix does that too, and a
// slice prefix is what this package replaces. It is that the mapping is *stable*: stable across
// calls, stable across the order nodes arrive in, and stable across the departure of a node that
// did not own the key. Those three are what a cache needs, and they are what the previous
// implementation — `return nodes[:count]`, over a slice built by ranging a map — did not have.

func nodeIDs(n int) []string {
	ids := make([]string, n)
	for i := range n {
		ids[i] = fmt.Sprintf("node-%02d", i)
	}

	return ids
}

func keys(n int) []string {
	ks := make([]string, n)
	for i := range n {
		ks[i] = fmt.Sprintf("objects/dataset-%04d.bin", i)
	}

	return ks
}

// TestRendezvousRingImplementsHashRing pins the interface at compile time. Stated as a test rather
// than a blank var so the reason is visible: the coordinator holds the interface, not the struct.
func TestRendezvousRingImplementsHashRing(t *testing.T) {
	t.Parallel()

	var _ HashRing = New("node-a")
}

// TestLookupIsDeterministic is the defect in #131 stated directly: the same key and the same node
// set must give the same answer regardless of the order the nodes were added.
//
// The permutations matter because the caller builds its node slice by ranging a map, whose order Go
// deliberately randomizes per iteration. A test that only calls Lookup twice on one ring would pass
// on an implementation that returned nodes[0].
func TestLookupIsDeterministic(t *testing.T) {
	t.Parallel()

	nodes := nodeIDs(8)

	// A ring built in ascending order is the reference.
	want := make(map[string]string, 64)
	ref := New(nodes...)
	for _, k := range keys(64) {
		want[k] = ref.Lookup(k)
	}

	// Descending, and then several rotations, cover the orderings a map iteration could produce
	// without depending on the map to produce them.
	orders := [][]string{reversed(nodes)}
	for shift := 1; shift < len(nodes); shift++ {
		orders = append(orders, rotated(nodes, shift))
	}

	for i, order := range orders {
		ring := New(order...)
		for k, wantNode := range want {
			if got := ring.Lookup(k); got != wantNode {
				t.Fatalf("order %d: Lookup(%q) = %q, want %q — insertion order changed the mapping",
					i, k, got, wantNode)
			}
		}
	}
}

// TestLookupNIsDeterministic extends the same requirement to the replica list, including its order.
// Replica order is load-bearing: targetNodes[0] is the node Session consistency executes on first,
// so a set that is right but ordered differently per call still moves work between nodes.
func TestLookupNIsDeterministic(t *testing.T) {
	t.Parallel()

	nodes := nodeIDs(6)
	ref := New(nodes...)
	shuffled := New(reversed(nodes)...)

	for _, k := range keys(32) {
		want := ref.LookupN(k, 3)
		got := shuffled.LookupN(k, 3)

		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("LookupN(%q, 3) = %v, want %v — insertion order changed the replica order",
				k, got, want)
		}
	}
}

// TestLookupAgreesWithLookupN checks the two entry points cannot drift apart. Lookup is a separate
// non-allocating loop, so nothing but a test keeps it consistent with the sort in LookupN.
func TestLookupAgreesWithLookupN(t *testing.T) {
	t.Parallel()

	ring := New(nodeIDs(10)...)

	for _, k := range keys(128) {
		single := ring.Lookup(k)
		first := ring.LookupN(k, 1)

		if len(first) != 1 || first[0] != single {
			t.Fatalf("Lookup(%q) = %q but LookupN(%q, 1) = %v", k, single, k, first)
		}
	}
}

// TestRemovingANonOwnerDoesNotRemap is the property that makes this worth doing. Removing a node
// must only move the keys that node owned; every other key must stay where it was, or the cache on
// every surviving node is invalidated by an unrelated node's departure.
func TestRemovingANonOwnerDoesNotRemap(t *testing.T) {
	t.Parallel()

	nodes := nodeIDs(5)
	ring := New(nodes...)

	before := make(map[string]string, 256)
	for _, k := range keys(256) {
		before[k] = ring.Lookup(k)
	}

	victim := nodes[2]
	ring.Remove(victim)

	moved := 0
	for k, owner := range before {
		got := ring.Lookup(k)

		switch {
		case owner == victim:
			// Its keys must go somewhere, and not to the node that just left.
			if got == victim {
				t.Fatalf("Lookup(%q) still returns the removed node %q", k, victim)
			}
			moved++
		case got != owner:
			t.Errorf("Lookup(%q) moved from %q to %q, but %q was not the removed node",
				k, owner, got, owner)
		}
	}

	if moved == 0 {
		t.Fatal("removing a node moved no keys, so the victim owned none — the test proves nothing")
	}
}

// TestAddingANodeOnlyTakesKeys is the mirror: a new node may claim keys, but no key may move
// between two nodes that were both already present. That is the bound on rebalancing cost.
func TestAddingANodeOnlyTakesKeys(t *testing.T) {
	t.Parallel()

	ring := New(nodeIDs(5)...)

	before := make(map[string]string, 256)
	for _, k := range keys(256) {
		before[k] = ring.Lookup(k)
	}

	const newcomer = "node-new"
	ring.Add(newcomer)

	claimed := 0
	for k, owner := range before {
		switch got := ring.Lookup(k); got {
		case owner:
		case newcomer:
			claimed++
		default:
			t.Errorf("Lookup(%q) moved from %q to %q — neither is the added node", k, owner, got)
		}
	}

	if claimed == 0 {
		t.Error("the new node claimed no keys out of 256, which is not a plausible share of 6")
	}
}

// TestLookupNReturnsDistinctNodes pins that replicas are distinct. Returning the same node twice
// would report a replication factor of 3 while storing one copy — the class of defect where a
// success is reported for something that did not happen.
func TestLookupNReturnsDistinctNodes(t *testing.T) {
	t.Parallel()

	ring := New(nodeIDs(7)...)

	for _, k := range keys(64) {
		got := ring.LookupN(k, 3)
		if len(got) != 3 {
			t.Fatalf("LookupN(%q, 3) returned %d nodes", k, len(got))
		}

		seen := make(map[string]bool, 3)
		for _, n := range got {
			if seen[n] {
				t.Fatalf("LookupN(%q, 3) = %v contains %q twice", k, got, n)
			}
			seen[n] = true
		}
	}
}

// TestLookupNClampsToRingSize covers asking for more replicas than there are nodes, which is what a
// ReplicationFactor of 3 on a two-node cluster does. Fewer nodes, no error, no padding with "".
func TestLookupNClampsToRingSize(t *testing.T) {
	t.Parallel()

	ring := New("node-a", "node-b")

	got := ring.LookupN("objects/data.bin", 5)
	if len(got) != 2 {
		t.Fatalf("LookupN with n > ring size returned %d nodes, want 2: %v", len(got), got)
	}
	for i, n := range got {
		if n == "" {
			t.Errorf("element %d is empty — the result was padded rather than clamped", i)
		}
	}
}

// TestEmptyRing pins that an empty ring is answered rather than panicked on. The coordinator
// already refuses an operation when no node is alive, but a ring that panics on an empty node set
// turns a routing decision into a crash of the mount process.
func TestEmptyRing(t *testing.T) {
	t.Parallel()

	ring := New()

	if got := ring.Lookup("objects/data.bin"); got != "" {
		t.Errorf("Lookup on an empty ring = %q, want \"\"", got)
	}

	got := ring.LookupN("objects/data.bin", 3)
	if got == nil {
		t.Error("LookupN on an empty ring returned nil; want an empty slice, so callers can range it")
	}
	if len(got) != 0 {
		t.Errorf("LookupN on an empty ring = %v, want empty", got)
	}
}

// TestNonPositiveCount pins the boundary on the other side. count reaches here from
// min(ReplicationFactor, len(aliveNodes)), and a misconfigured factor of 0 or a negative must not
// index a slice.
func TestNonPositiveCount(t *testing.T) {
	t.Parallel()

	ring := New(nodeIDs(3)...)

	for _, n := range []int{0, -1} {
		got := ring.LookupN("objects/data.bin", n)
		if len(got) != 0 {
			t.Errorf("LookupN(key, %d) = %v, want empty", n, got)
		}
	}
}

// TestAddIsIdempotentAndSorted covers membership bookkeeping: a node announced twice by gossip must
// not be counted twice, or it would receive two of the three replicas of a key.
func TestAddIsIdempotentAndSorted(t *testing.T) {
	t.Parallel()

	ring := New()
	for _, id := range []string{"node-c", "node-a", "node-b", "node-a", "node-c"} {
		ring.Add(id)
	}

	got := ring.Nodes()
	want := []string{"node-a", "node-b", "node-c"}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Nodes() = %v, want %v", got, want)
	}
	if ring.Len() != 3 {
		t.Errorf("Len() = %d, want 3", ring.Len())
	}

	// A duplicate must not be able to take two replica slots for one key.
	replicas := ring.LookupN("objects/data.bin", 3)
	seen := make(map[string]bool, 3)
	for _, n := range replicas {
		if seen[n] {
			t.Fatalf("LookupN = %v repeats %q, so a duplicate Add created a second slot", replicas, n)
		}
		seen[n] = true
	}
}

// TestRemoveUnknownIsANoOp covers the gossip path where a node leaves twice, or leaves before it was
// ever added.
func TestRemoveUnknownIsANoOp(t *testing.T) {
	t.Parallel()

	ring := New("node-a", "node-b")
	ring.Remove("node-z")
	ring.Remove("node-a")
	ring.Remove("node-a")

	got := ring.Nodes()
	if len(got) != 1 || got[0] != "node-b" {
		t.Errorf("Nodes() = %v, want [node-b]", got)
	}
}

// TestNodesReturnsACopy pins that a caller cannot mutate the ring's membership through the slice it
// hands out. Sorted order is an invariant Add and Remove rely on with binary search.
func TestNodesReturnsACopy(t *testing.T) {
	t.Parallel()

	ring := New("node-a", "node-b")

	snapshot := ring.Nodes()
	snapshot[0] = "node-mutated"

	if got := ring.Nodes(); got[0] != "node-a" {
		t.Errorf("mutating the returned slice changed the ring: Nodes() = %v", got)
	}
}

// TestSeparatorPreventsBoundaryCollisions is why score joins with "#" rather than concatenating.
// Without a separator, ("a", "bc") and ("ab", "c") hash identical bytes, so one node's ownership of
// a key would depend on another node's name — a bug that would look like a rare, unreproducible
// misroute rather than a hashing mistake.
func TestSeparatorPreventsBoundaryCollisions(t *testing.T) {
	t.Parallel()

	if score("a", "bc") == score("ab", "c") {
		t.Error("score(\"a\", \"bc\") == score(\"ab\", \"c\"): the node/key boundary is not separated")
	}
}

// TestOutranksBreaksTiesByNodeID asserts the tiebreak directly, because no input reachable through
// Lookup can produce a 64-bit score collision — so removing the tiebreak passes every other test in
// this file. That was verified by removing it. sort.Slice is not stable, so on a collision the
// winner without this rule is whichever element the sort left first, which is the input-order
// dependence the package exists to remove.
func TestOutranksBreaksTiesByNodeID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b scored
		want bool
	}{
		{"higher score wins", scored{"node-z", 2}, scored{"node-a", 1}, true},
		{"lower score loses", scored{"node-a", 1}, scored{"node-z", 2}, false},
		{"tie goes to the lower node ID", scored{"node-a", 7}, scored{"node-b", 7}, true},
		{"tie against the lower node ID loses", scored{"node-b", 7}, scored{"node-a", 7}, false},
		{"a node does not outrank itself", scored{"node-a", 7}, scored{"node-a", 7}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := outranks(tc.a, tc.b); got != tc.want {
				t.Errorf("outranks(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestDistributionIsNotWildlyUneven is a sanity bound, not a uniformity proof. Its job is to catch
// an implementation that is deterministic but degenerate — the previous one sent every key to
// nodes[0], which is deterministic once the input is sorted and still useless.
func TestDistributionIsNotWildlyUneven(t *testing.T) {
	t.Parallel()

	const (
		nodeCount = 10
		keyCount  = 10000
	)

	ring := New(nodeIDs(nodeCount)...)

	counts := make(map[string]int, nodeCount)
	for _, k := range keys(keyCount) {
		counts[ring.Lookup(k)]++
	}

	if len(counts) != nodeCount {
		t.Errorf("only %d of %d nodes received any key", len(counts), nodeCount)
	}

	// Generous bounds: half to double the fair share. A tighter bound would make this a test of
	// xxhash's distribution, which is not this package's contract, and would be flaky by design.
	expected := keyCount / nodeCount
	for node, n := range counts {
		if n < expected/2 || n > expected*2 {
			t.Errorf("node %s got %d keys, want roughly %d — distribution is degenerate",
				node, n, expected)
		}
	}
}

// TestRemovalMovesOnlyAFairShare bounds the disruption a departure causes. This is the quantitative
// version of TestRemovingANonOwnerDoesNotRemap: with n nodes, removing one should move about 1/n of
// the keys, not all of them. The old slice-prefix implementation would fail this outright.
func TestRemovalMovesOnlyAFairShare(t *testing.T) {
	t.Parallel()

	const (
		nodeCount = 10
		keyCount  = 10000
	)

	nodes := nodeIDs(nodeCount)
	ring := New(nodes...)

	before := make(map[string]string, keyCount)
	for _, k := range keys(keyCount) {
		before[k] = ring.Lookup(k)
	}

	ring.Remove(nodes[0])

	moved := 0
	for k, owner := range before {
		if ring.Lookup(k) != owner {
			moved++
		}
	}

	// The fair share is keyCount/nodeCount = 1000. Allow up to double before calling it a defect.
	if limit := 2 * keyCount / nodeCount; moved > limit {
		t.Errorf("removing 1 of %d nodes moved %d of %d keys, want at most %d",
			nodeCount, moved, keyCount, limit)
	}
	if moved == 0 {
		t.Error("removing a node moved no keys at all, which cannot be right")
	}
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}

	return out
}

func rotated(in []string, shift int) []string {
	out := make([]string, 0, len(in))
	out = append(out, in[shift:]...)
	out = append(out, in[:shift]...)

	return out
}

// BenchmarkLookup100Nodes measures the O(n) lookup at a node count well above what a cluster of
// this kind has, so the constant is visible rather than argued about. The point of the benchmark is
// the acceptance criterion in #131: if a lookup at 100 nodes is cheap relative to an S3 round trip
// — which is milliseconds — then O(n) buys simplicity for free and no ring structure is warranted.
//
// Measured on an Apple M4 Max, go1.26.5: 1555 ns/op at 100 nodes and 125 ns/op at 8, both with zero
// allocations. Three orders of magnitude below an S3 GET, so the answer is that O(n) is fine and a
// sorted ring with virtual nodes would be complexity spent on nothing. Recorded because a figure in
// a comment is checkable against a re-run, whereas "acceptable" is not.
func BenchmarkLookup100Nodes(b *testing.B) {
	ring := New(nodeIDs(100)...)
	ks := keys(256)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; b.Loop(); i++ {
		_ = ring.Lookup(ks[i%len(ks)])
	}
}

// BenchmarkLookupN3Of100Nodes is the write path's cost: a replication factor of 3 sorts all 100
// candidates. If this is where the O(n log n) shows up, this is the benchmark that says so.
func BenchmarkLookupN3Of100Nodes(b *testing.B) {
	ring := New(nodeIDs(100)...)
	ks := keys(256)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; b.Loop(); i++ {
		_ = ring.LookupN(ks[i%len(ks)], 3)
	}
}

// BenchmarkLookup8Nodes is the realistic case, for comparison against the 100-node figure.
func BenchmarkLookup8Nodes(b *testing.B) {
	ring := New(nodeIDs(8)...)
	ks := keys(256)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; b.Loop(); i++ {
		_ = ring.Lookup(ks[i%len(ks)])
	}
}

// FuzzLookupIsStable drives arbitrary node IDs and keys through the invariant that matters: the
// answer does not depend on insertion order. Hand-written cases use well-formed "node-NN" IDs; a
// fuzzer reaches the empty string, unicode, embedded separators, and duplicates.
func FuzzLookupIsStable(f *testing.F) {
	f.Add("node-a", "node-b", "node-c", "objects/data.bin")
	f.Add("", "", "", "")
	f.Add("a", "ab", "abc", "#")
	f.Add("node#1", "node#2", "1", "node")

	f.Fuzz(func(t *testing.T, a, b, c, key string) {
		forward := New(a, b, c)
		backward := New(c, b, a)

		if got, want := backward.Lookup(key), forward.Lookup(key); got != want {
			t.Fatalf("Lookup(%q) = %q forward, %q backward, for nodes %q/%q/%q",
				key, want, got, a, b, c)
		}

		// Membership must not depend on order either: both rings hold the same distinct IDs.
		fw, bw := forward.Nodes(), backward.Nodes()
		sort.Strings(fw)
		sort.Strings(bw)
		if strings.Join(fw, ",") != strings.Join(bw, ",") {
			t.Fatalf("membership differs by insertion order: %v vs %v", fw, bw)
		}

		// Replicas are distinct however many distinct nodes went in.
		replicas := forward.LookupN(key, 3)
		seen := make(map[string]bool, len(replicas))
		for _, n := range replicas {
			if seen[n] {
				t.Fatalf("LookupN(%q, 3) = %v repeats %q", key, replicas, n)
			}
			seen[n] = true
		}
		if len(replicas) != forward.Len() && len(replicas) != 3 {
			t.Fatalf("LookupN(%q, 3) returned %d nodes for a ring of %d",
				key, len(replicas), forward.Len())
		}
	})
}
