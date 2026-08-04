# S3 Conditional Writes as a Coordination Primitive

**Status:** **adopted 2026-08-03.** The recommendation below is the project's direction. The Raft
build-out is closed ([#128], [#130], [#133], [#151]) and the CAS work is filed ([#282], [#283],
[#284], [#285]) — see [§5, Consequences](#5-consequences), which records two corrections found while
acting on it.
**Issue:** [#169](https://github.com/scttfrdmn/objectfs/issues/169)

## Recommendation, first

**Adopt the hybrid: per-key compare-and-swap for coordination, gossip for membership, no replicated
log.** Stop building toward Raft.

The one-line reason: **Raft replicates a log so that N nodes agree on state they each hold a copy
of. ObjectFS nodes hold no such state — S3 does.** Every operation `internal/distributed` coordinates
ends in a write to a key in one bucket that every node can already read. A consensus protocol whose
purpose is agreeing on the contents of a replicated state machine has nothing to replicate when the
state machine is a bucket all the participants share.

That is not an argument that Raft is the wrong protocol. It is an argument that there is no
replication problem here to solve, and that the problem there *is* — "two nodes must not both decide
they are the one doing this" — is exactly what a per-key CAS gives you in one round trip against a
service that is already a dependency.

What this costs and saves is in [§5, Consequences](#5-consequences). What it cannot do is in
[The ceiling](#the-ceiling-where-cas-is-not-enough), and that section is the one to read before
agreeing.

---

## 1. Which guarantees does the filesystem actually need?

The starting point has to be what the code does, not what the package documentation says, because
those differ substantially. Verified against the tree at the time of writing:

**There is no state machine.** `applyLogEntry` (`internal/distributed/consensus.go:756`) is three
empty arms:

```go
switch entry.Type {
case EntryTypeLeaderElection:
	// Leader election entry applied
case EntryTypeConfigChange:
	// Apply configuration change
case EntryTypeOperation:
	// Apply operation
}
```

Nothing anywhere appends `EntryTypeOperation` or `EntryTypeConfigChange`. The log holds one bootstrap
noop plus one entry per election win. **The consensus layer elects leaders and replicates nothing.**

**Consensus is not on the data path.** `Coordinator.executeLocally` calls the backend directly and
never consults the log, `commitIndex`, term, or leadership.

**"Strong consistency" is not linearizable.** `executeStrongConsistency`
(`internal/distributed/coordinator.go:351`) fans an operation to N nodes and succeeds on
`successCount >= len(targetNodes)/2+1`. But every node writes the *same* S3 key with the *same*
bytes, so those are N redundant identical PUTs. Majority success is a reachability signal — it says
"most nodes could talk to S3" — not a consistency guarantee. `doc.go:64` claims "Linearizable
operations across cluster"; nothing supports that, and it is inaccurate today regardless of which
direction this evaluation had recommended.

**`broadcastProposal` is a simulation stub** (`consensus.go:771`) that logs, sleeps, and accepts its
own proposal. **`selectConsistentHash`** (`coordinator.go:918`) returns `nodes[:count]` — there is no
hashing.

So the honest inventory of what the filesystem needs from coordination:

| # | Requirement | Scope | Does Raft provide it? | Does per-key CAS? |
|---|---|---|---|---|
| R1 | Two nodes must not both believe they own a task (tier transitions, cache warming, scrubs) | one key | yes, via leader election | **yes**, `If-None-Match: *` on a lease key |
| R2 | A metadata update must not silently lose a concurrent one | one key | yes, via the log | **yes**, `If-Match: <etag>` |
| R3 | Nodes must know who else is alive | cluster | no — Raft needs membership, it does not discover it | no — this is gossip's job either way |
| R4 | A cache invalidation must not be applied out of order relative to the write it invalidates | one key | yes, via log order | **yes**, if the invalidation carries the ETag it was computed from |
| R5 | Two keys must change atomically together | multiple keys | yes | **no** — see the ceiling |
| R6 | A decision must survive the loss of any minority of nodes | cluster | yes, with persistent state | **yes, and more cheaply** — the decision lives in S3, which is 11-nines durable and outlives every node |

R6 is where the asymmetry is starkest and it is worth being explicit about, because it inverts the
usual argument for consensus. Raft's durability story requires each node to fsync its log before
acknowledging — that is what [#130](https://github.com/scttfrdmn/objectfs/issues/130) exists to
build, and it is correctly labeled a safety prerequisite, because a Raft node that forgets its vote
after a restart can elect a second leader in the same term. A CAS-based design has no such
prerequisite: the lease is an object in the bucket. A node that restarts has nothing to remember,
because it never held authoritative state. **The hardest correctness obligation in the Raft direction
does not exist in the other one.**

R1–R4 and R6 are per-key. R5 is the only genuine multi-key requirement, and nothing in ObjectFS
currently needs it.

---

## 2. What conditional writes can and cannot do

### Verified behavior

All of the following was executed against a substrate emulator over real HTTP (substrate v0.85.0,
which implements conditional writes; the emulator holds a per-key striped mutex across
check-then-write, so an exactly-one-winner result is meaningful rather than an artifact of
last-write-wins):

| Probe | Result |
|---|---|
| 32 concurrent `If-None-Match: *` PUTs to one key | **exactly 1 winner**, 31 × `PreconditionFailed`; the stored body is the winner's |
| `If-Match: <current etag>` | succeeds, and `PutObject` **returns the new ETag** |
| `If-Match: <stale etag>` | `412 PreconditionFailed`; the stored object is unchanged |
| `If-Match` against an absent key | `404 NoSuchKey` — not 412 |
| `If-None-Match: *` against an existing key | `412 PreconditionFailed` |
| 8 workers running a read-ETag → increment → CAS loop | converges to exactly **8**, 36 attempts, 28 lost races, **zero lost updates** |

That PUT returns a fresh ETag matters more than it looks: it means a CAS loop does not need a HEAD
between iterations to make progress, so the steady-state cost of holding and renewing a lease is one
request per renewal rather than two.

### The error taxonomy, which is three-way and not two-way

Per AWS's [conditional write behavior](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html),
a failed conditional write has outcomes that select *different* recovery paths:

| Response | Meaning | Correct recovery |
|---|---|---|
| `412 PreconditionFailed` | another writer won | re-read, recompute, retry the CAS |
| `409 ConditionalRequestConflict` on `PutObject` | a delete interleaved | retry the PUT as-is |
| `409` on `CompleteMultipartUpload` | a delete interleaved | **the upload ID is dead** — re-do `CreateMultipartUpload` and every part |
| `404 NoSuchKey` on `If-Match` | the object is gone | re-upload; do not retry the CAS |

A loop that treats 409 as a synonym for 412 and re-sends `CompleteMultipartUpload` with the same
upload ID will spin until it gives up. Substrate does not model 409 at all, so that mistake is
invisible in testing today — filed upstream as
[scttfrdmn/substrate#540](https://github.com/scttfrdmn/substrate/issues/540), with fault injection as
the suggested shape, since 409 arises from a timing race rather than from a state a request can
assert.

Two further documented rules, both verified present in substrate:

- `If-None-Match` on a write accepts **only `*`**. Any other value is not a supported form.
- Conditional writes **do not consider in-progress multipart uploads**. A `PutObject` can land in the
  middle of someone's MPU, and their `CompleteMultipartUpload` then fails. So an MPU is not a way to
  hold a claim on a key while assembling one.

And one constraint on deployment: conditional writes **require SigV4**. Not an issue here — the
AWS SDK v2 signs with SigV4 unconditionally — but it rules out any SigV2-only endpoint.

### The ceiling: where CAS is not enough

Being explicit, since this is the part that decides whether the recommendation holds as the project
grows:

1. **No multi-key atomicity.** Two keys cannot be changed together. If a future feature needs "rename
   this directory's 10,000 objects atomically," CAS cannot do it and neither can Raft-over-S3 without
   the log becoming the source of truth — which would mean the bucket is no longer authoritative, a
   much larger change than adding consensus.

2. **No liveness guarantee on leadership.** A lease is a timeout. A node that loses network but keeps
   running believes it holds a lease it has actually lost, and the next holder can take it while the
   first is still acting. Raft has the same problem — a partitioned leader also does not know it — but
   Raft at least *fails closed* on writes, because it cannot reach a quorum. A CAS design fails closed
   only if every guarded action itself re-asserts the CAS. **That is a design obligation, not a
   property you get for free**, and it is the single most important thing to get right if this is
   adopted: an action guarded by "I checked my lease a moment ago" is not guarded.

3. **No membership consensus.** Who is in the cluster is not decided by CAS. Gossip
   ([#132](https://github.com/scttfrdmn/objectfs/issues/132),
   [#133](https://github.com/scttfrdmn/objectfs/issues/133)) remains the mechanism, and its lack of
   message authentication ([#206](https://github.com/scttfrdmn/objectfs/issues/206)) remains a real
   security defect that this evaluation does not touch.

4. **Cost and latency are per-attempt.** A contended CAS loop issues a request per lost race, and
   AWS bills failed conditional requests at the normal request rate — the documentation is explicit
   that there is no additional charge for conditional requests but that you are "charged existing
   rates for the applicable requests, including for failed requests." So a hot key with N contenders
   is O(N) requests, not O(1). Coordination keys must be chosen so contention is low; a single global
   lock key would be the wrong shape.

---

## 3. The interface change required

`Backend.PutObject` (`pkg/types/interfaces.go:25`) takes no precondition and returns no ETag:

```go
PutObject(ctx context.Context, key string, data []byte, meta map[string]string) error
```

There are zero hits for `If-None-Match` / `If-Match` anywhere in the repository. The read half is
already plumbed: `types.ObjectInfo.ETag` exists and is populated by `HeadObject` and `ListObjects`.
So the change is write-side only.

**Sketch — a separate method, not a changed signature:**

```go
// PutObjectIf stores data as key only if cond holds, reporting the new ETag.
//
// A precondition that does not hold is [ErrPreconditionFailed] and the stored object is unchanged —
// that is the mechanism, not an error path: a caller racing for a lease learns it lost by getting
// this back. A caller that wants an unconditional write uses PutObject.
PutObjectIf(ctx context.Context, key string, data []byte, meta map[string]string,
	cond Precondition) (etag string, err error)

// Precondition is an assertion about a key's current state, evaluated by the backend at write time.
// The zero value asserts nothing and is therefore invalid for PutObjectIf — an empty precondition
// there is a caller that meant to write unconditionally and reached for the wrong method.
type Precondition struct {
	// Absent asserts the key does not currently exist. Sent as If-None-Match: *.
	Absent bool

	// ETag asserts the key currently has this ETag. Sent as If-Match.
	//
	// An absent key is ErrNotFound rather than ErrPreconditionFailed, because those want different
	// recovery: a lost race means recompute and retry, a vanished object means the state being
	// updated no longer exists.
	ETag string
}
```

Why a new method rather than an extra parameter on `PutObject`:

- `PutObject` has **one production implementation** (`internal/storage/s3/backend.go:808`) and **four
  test doubles** (`tests/fuse_test.go:61`, `tests/predictive_cache_test.go:54`,
  `tests/integration_test.go:517`, `pkg/types/interfaces_test.go:32`). Changing the signature edits
  all five and every one of the nine non-test call sites, most of which have no precondition to
  express. A new method leaves them alone.
- Not every backend can implement it (see §4), and a backend that cannot must say so rather than
  silently ignore a precondition. A distinct method makes "unsupported" expressible as
  `ErrNotSupported` at the one place that matters.
- The return type differs. `PutObject` returns `error`; a CAS needs the new ETag to continue a loop
  without a HEAD. Bolting a second return value onto the common path to serve the rare one is the
  wrong trade.

Two sentinel errors are needed, and the distinction is load-bearing rather than tidy:

```go
// ErrPreconditionFailed means the assertion did not hold and nothing was written. For a caller
// racing for a lease this is the expected outcome, not a failure.
var ErrPreconditionFailed = errors.New("precondition failed")

// ErrConditionalConflict means the write raced a delete. Unlike ErrPreconditionFailed, the caller's
// view of the state is not necessarily stale — retrying the same write may simply succeed. On a
// multipart completion it additionally means the upload ID can no longer be completed.
var ErrConditionalConflict = errors.New("conditional request conflict")
```

The S3 implementation maps `PreconditionFailed` → `ErrPreconditionFailed`,
`ConditionalRequestConflict` → `ErrConditionalConflict`, and `NoSuchKey`/`NotFound` → the existing
not-found error, matching on `smithy.APIError`'s code rather than on message text.

---

## 4. Non-AWS compatibility

| Backend | Conditional write on PUT | Evidence | Degradation |
|---|---|---|---|
| **AWS S3** | **Yes** — `If-None-Match: *` and `If-Match` on `PutObject`, `CompleteMultipartUpload`, `CopyObject`; conditional deletes too | [AWS docs](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html) | none |
| **MinIO** | **Yes**, both. `PutObjectOptions.SetMatchETag` / `SetMatchETagExcept` (`minio-go/v7@v7.2.1/api-put-object.go:126,140`) emit `If-Match` / `If-None-Match` | source | its own doc comment calls this "a MinIO specific extension to support optimistic locking semantics" — so treat the *semantics* as needing verification against a real MinIO, not just the header's presence |
| **Ceph RGW** | **No.** PUT Object documents only `content-md5`, `content-type`, `x-amz-meta-*`, `x-amz-acl`. Conditional headers are documented for GET/HEAD only; the S3 compatibility page does not mention conditional writes in either direction | [Ceph objectops](https://docs.ceph.com/en/latest/radosgw/s3/objectops/) | **must fail closed** |
| **Wasabi** | **Unverified.** Wasabi's knowledge base moved and its current S3 API reference did not resolve at the time of writing; no citable statement either way | — | treat as Ceph until probed against a real endpoint |

This matters because [#169](https://github.com/scttfrdmn/objectfs/issues/169) notes the v0.14.0
metadata-fallback path exists precisely for these backends. The degradation rule has to be:

**A backend that cannot do conditional writes returns `ErrNotSupported`, and a coordination feature
that needs one refuses to start rather than running unguarded.** Falling back to an unconditional PUT
would turn "exactly one node performs this tier transition" into "every node does," which is worse
than the feature being unavailable — it is the failure mode the coordination exists to prevent,
occurring silently. There is precedent in this codebase for that choice: the read path fails closed
on a `Content-Encoding` it cannot handle rather than returning bytes it cannot decode.

Detection should be **by attempt, not by configuration**. A probe — `If-Match` against a key known
absent, expecting `NoSuchKey` rather than success — is one request at startup and cannot be wrong
about the endpoint in front of it, whereas a config flag or an endpoint-URL heuristic can. A backend
that *succeeds* at that probe is silently ignoring preconditions, which is the dangerous case and the
one a feature-flag approach would never notice.

---

## 5. Consequences

### Adopted — what actually happened

This section was written as "if adopted." It was adopted on 2026-08-03, and acting on it surfaced two
corrections to what is below. Both are recorded here rather than silently edited into the lists,
because a plan that turns out to have been slightly wrong is more useful with the correction visible
than with the seam smoothed over.

1. **The closure list of four below was incomplete — it should have named
   [#150](https://github.com/scttfrdmn/objectfs/issues/150)** (`BboltPersistentState` /
   `BboltConsensusLog`). That issue's own premise is *"the `ConsensusLog` and `PersistentState`
   interfaces were defined in A1-1 and A1-3"* — i.e. #128 and #130, both closed here — so nothing
   remained for it to implement. It would also have added `go.etcd.io/bbolt` and a
   `/var/lib/objectfs` data path for durable Raft storage, neither of which CAS needs, since
   coordination state lives in S3 objects guarded by `If-Match`. **Closed 2026-08-03**, after being
   flagged separately rather than swept in with the other four, because it was outside the set the
   decision authorized. So the closure list is five, and the list below reads as it was written plus
   this correction.
2. **The `doc.go` correction below was already done** before adoption. Grep for `Linearizable`
   returns nothing.

One finding worth carrying into item 3 of the "Opened" list: **`ConsistencySession` and
`ConsistencyEventual` are now nearly the same function.** Both execute on `targetNodes[0]` and then
`replicateAsync` the remainder; session's only distinguishing feature is a comment promising it will
"try to use the same node for related operations," and no session state exists to make that true. So
the taxonomy is not merely three levels differing in PUT count — it is three names for two
behaviours, one of which is mislabeled.

**Closed as not-the-direction** (the Raft build-out, ~4 issues):

- [#128](https://github.com/scttfrdmn/objectfs/issues/128) — `ConsensusLog` interface and in-memory
  implementation. No log is needed if there is nothing to replicate.
- [#130](https://github.com/scttfrdmn/objectfs/issues/130) — `PersistentState` interface, "Raft
  safety prerequisite." Not needed: nodes hold no authoritative state to lose. This is the largest
  single saving, and it is the issue currently labeled `priority: critical`.
- [#133](https://github.com/scttfrdmn/objectfs/issues/133) — real UDP `broadcastProposal`. Proposals
  are a consensus concept; gossip still needs real broadcast for membership, so the *networking* work
  survives even though the proposal framing does not.
- [#151](https://github.com/scttfrdmn/objectfs/issues/151) — serializing state-machine state, whose
  premise is a state machine that does not exist.

**Kept, unchanged** — none of these are consensus work:

- [#132](https://github.com/scttfrdmn/objectfs/issues/132) — `NodeInfo` stats via gossip heartbeat.
- [#131](https://github.com/scttfrdmn/objectfs/issues/131) — replace the fake consistent hash with
  rendezvous (HRW) hashing. Still needed, and arguably *more*: work distribution across nodes is how
  contention on coordination keys is kept low, which §2's cost note makes load-bearing.
- [#206](https://github.com/scttfrdmn/objectfs/issues/206) — gossip message authentication. A real
  security defect either way.

Also note [#131](https://github.com/scttfrdmn/objectfs/issues/131) has since landed, so
`selectConsistentHash` is a real rendezvous/HRW ring and no longer `nodes[:count]`.

**Opened** — filed as [#282], [#283], [#284], [#285] respectively:

1. [#282] — `Backend.PutObjectIf` plus the two sentinel errors and the S3 implementation, with the
   capability probe. The unit of work §3 sketches. Blocks the other three.
2. [#283] — A lease type built on it — acquire, renew, release, and the rule that **every guarded
   action re-asserts the CAS** rather than trusting a prior check. The ceiling's item 2 is the whole
   reason this is its own issue and not a footnote on the first.
3. [#284] — Replace `executeStrongConsistency`'s N-redundant-PUT fan-out with a single conditional
   write, and delete the `ConsistencyStrong` / `ConsistencySession` / `ConsistencyEventual` taxonomy
   or redefine it against what the code does. It currently offers three levels that differ in how
   many identical PUTs are issued. Coordinates with
   [#129](https://github.com/scttfrdmn/objectfs/issues/129), which proposes promoting that taxonomy
   into `pkg/types`, and [#144](https://github.com/scttfrdmn/objectfs/issues/144), which is defined
   entirely in terms of it.
4. [#285] — Verify conditional-write semantics against a real MinIO and a real Wasabi endpoint, since
   §4 rests on a source read for one and nothing for the other.

[#128]: https://github.com/scttfrdmn/objectfs/issues/128
[#130]: https://github.com/scttfrdmn/objectfs/issues/130
[#133]: https://github.com/scttfrdmn/objectfs/issues/133
[#151]: https://github.com/scttfrdmn/objectfs/issues/151
[#282]: https://github.com/scttfrdmn/objectfs/issues/282
[#283]: https://github.com/scttfrdmn/objectfs/issues/283
[#284]: https://github.com/scttfrdmn/objectfs/issues/284
[#285]: https://github.com/scttfrdmn/objectfs/issues/285

### `doc.go` — done before adoption

`internal/distributed/doc.go:64` claimed "Linearizable operations across cluster" under Strong
Consistency. That was inaccurate on today's code whichever direction was chosen — N identical PUTs to
one key, accepted on a majority, is a reachability signal — so it was fixed without waiting for the
decision. Grep for `Linearizable` returns nothing.

### The road not taken

Kept as written, because the case against a decision is worth preserving next to the decision — and
because if CAS hits the ceiling in §2, this is where the alternative is already costed. This describes
what *would* have followed from declining; it did not happen.

The Raft direction is coherent and the issues are well-specified; nothing here says it *cannot* be
built. The cost is roughly: a persistent log with fsync-before-ack ([#130]), a real state machine with
apply semantics, real proposal broadcast, and the test infrastructure to show that a partitioned or
restarted node cannot elect a second leader in one term — against a problem whose state already lives
in a service with 11 nines of durability that every node can reach.

The reason to decide now rather than later was that the decision gets more expensive with every issue
in the [#128]–[#133] series that lands. At the time of the decision the sunk cost was ~6,000 lines
that, per §1, elects leaders and replicates nothing. That code is still there: closing the issues
stopped the build-out, it did not remove what exists. [#284] is where the first of it comes out.
