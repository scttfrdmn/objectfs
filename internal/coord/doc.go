// Package coord provides mutual exclusion between ObjectFS nodes using the object store itself as
// the arbiter, via [types.Backend.PutObjectIf].
//
// # A lease is a timeout, not a guarantee
//
// This is the limitation to understand before using anything here. A node that loses its network but
// keeps running believes it holds a lease it has actually lost, and the next holder can take it while
// the first is still executing. Raft has the same problem — a partitioned leader also does not know
// it — but Raft at least fails closed on writes, because it cannot reach a quorum.
//
// A CAS design fails closed only if every guarded action itself re-asserts the CAS. That is a design
// obligation rather than a property that comes for free, and it is the thing this package is
// organized around: there is no exported method on [Lease] that writes. The only way to act under a
// lease is [Lease.Do], which re-asserts before invoking the action, and the [Guard] it hands the
// action re-asserts again before every individual write. An action guarded by "I checked my lease a
// moment ago" is not guarded, so this package does not let you write that.
//
// What survives after all of that is a residual window: between a successful re-assert and the write
// it guards, S3 has no way to reject the write on the strength of the lease, because conditional
// writes cannot span two keys. The window is one round trip rather than one lease period, which is
// the improvement available; it is not zero. A caller whose correctness needs zero must make its own
// write conditional on state only the current holder could have set, which is what [Guard.PutIf]
// takes a [types.Precondition] for.
//
// # No absolute clock is trusted
//
// Two nodes with skewed clocks disagreeing about when a lease expired is exactly how two holders
// arise, so this package never compares one node's wall clock against a timestamp another node wrote.
// It does not compare wall clocks at all.
//
// Takeover of an apparently abandoned lease is decided by observing that the lease object's ETag has
// not changed across a full lease period, measured as a duration on the observer's own monotonic
// clock. A holder renews well inside that period and every renewal changes the ETag, so an unchanged
// ETag across the whole window means the holder stopped renewing. Durations on one clock are immune
// to skew between clocks, which absolute comparisons are not.
//
// It is also why a holder's own renewals are serialized against each other. Two of them reading the
// same ETag and both asserting it means one loses to the other, and a lost CAS is indistinguishable
// from a takeover — so the loser would drop a claim the node still holds. [Lease.Do]'s renewal ticker
// and every [Guard] method's re-assert both renew, so this is the ordinary case rather than an unusual
// caller.
//
// This is why a renewal writes a fresh nonce. S3's ETag is the MD5 of the content — verified by
// execution, not assumed — so a renewal rewriting identical bytes would leave the ETag unchanged and
// a live holder would be indistinguishable from a stopped one, which is the exact confusion the
// takeover rule exists to resolve. Renewals must move the ETag or the mechanism silently inverts.
//
// The cost is that takeover is slow by construction: an abandoned lease is not claimable until a full
// period of observation has passed. That is the correct trade for a mechanism whose failure mode is
// two simultaneous holders.
//
// # Cost
//
// AWS bills failed conditional requests at the normal request rate — the documentation is explicit
// that there is no additional charge for conditional requests but that you are charged existing rates
// "including for failed requests." So a contended key costs one request per lost race, and N
// contenders on one key is O(N) requests rather than O(1).
//
// Two consequences are built in here. Acquisition retries are bounded and jittered, never an
// unbounded spin. And [Lease] takes a key per resource: callers must not funnel unrelated resources
// through one global lock key, which would convert every operation in the system into contention on
// a single object.
//
// # Refusing to start
//
// A store that accepts a conditional header and ignores it is indistinguishable from one that honors
// it, from every angle except the outcome of a race. [New] therefore asks the backend what its
// endpoint actually implements, via [types.CapabilityReporter], and refuses to construct a lease when
// the answer is no or when there is no answer to be had. Running unguarded is not offered as a
// fallback: it would turn "exactly one node performs this transition" into "every node does",
// silently.
package coord
