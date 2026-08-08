# Conditional-write compatibility

**Probed 2026-08-08.** Every row below was established by running requests against the endpoint named
in it. Nothing here is read from documentation or inferred from source, because the two rows that
*were* documentation reads when this work started turned out to be wrong in both directions — and one
of them was wrong in the direction that loses data.

The suite that produces these rows is `internal/storage/s3/conditional_compat_test.go`, behind the
`s3compat` build tag. It is the artifact; this page is its output. Re-run it rather than trusting the
date at the top:

```bash
OBJECTFS_COMPAT_ENDPOINT=http://127.0.0.1:9111 \
  OBJECTFS_COMPAT_ACCESS_KEY=... OBJECTFS_COMPAT_SECRET_KEY=... \
  go test -race -tags=s3compat -v -count=1 ./internal/storage/s3/
```

## Why this page exists

ObjectFS uses S3 conditional writes as its coordination primitive — see
[Coordination — Conditional Writes vs Raft](conditional-writes-vs-raft.md). Every guarantee in that
design reduces to one property: **when N nodes assert the same precondition on the same key, the store
lets exactly one through.** That property is not in the S3 API contract as far as an S3-compatible
store is concerned. It is a behavior, and behaviors vary.

The dangerous variation is not an endpoint that rejects conditional headers. That one is harmless:
`PutObjectIf` returns `ErrNotSupported`, a coordination feature declines to start, and an operator
sees why. The dangerous one is an endpoint that **accepts the header and does not enforce it** —
indistinguishable from a correct one from every angle except the outcome of a race, which is to say
indistinguishable until it matters.

## Endpoints probed

| Endpoint | Version | How |
|---|---|---|
| AWS S3 | `us-west-2`, 2026-08-08 | the real service; each test creates its own bucket and removes it |
| MinIO | `RELEASE.2025-09-07T16-13-09Z` (commit `07c3a429bfed433e49018cb0f78a52145d4bedeb`) | container, local |
| Ceph RGW | `19.2.0 squid` (commit `16063ff2022298c9300e49a547a16ffda59baf13`) | `quay.io/ceph/demo`, local |
| Wasabi | — | **not probed.** No endpoint was available. See [Wasabi](#wasabi) below. |

## The matrix

Cells are the HTTP status and S3 error code as returned, because that pair is what
`translateConditionalError` dispatches on. A row naming a code is a row a mapping can be checked
against; a row naming a message is not, since message text is not stable.

| Check | AWS S3 | MinIO | Ceph RGW 19.2.0 |
|---|---|---|---|
| `If-None-Match: *`, key absent | success | success | success |
| `If-None-Match: *`, key exists | `412 PreconditionFailed`, holder's bytes intact | `412 PreconditionFailed`, intact | `412 PreconditionFailed`, intact |
| `If-Match` with the **quoted** ETag the store just returned | success | success | **`412 PreconditionFailed`** |
| `If-Match` with the bare hex digest | success | success | success |
| `If-Match: *` | **`501 NotImplemented`** | success | success |
| `If-Match` with a **stale** ETag | `412 PreconditionFailed` | `412 PreconditionFailed` | `412 PreconditionFailed` |
| `If-Match`, key **absent** | `404 NoSuchKey` | `404 NoSuchKey` | **`412 PreconditionFailed`** |
| 8 concurrent `If-None-Match: *` on one key | 1 winner, 7 × `412` | 1 winner, 7 × `412` | 1 winner, 7 × `412` |
| Precondition on `CompleteMultipartUpload`, key exists | `412`, holder intact | `412`, holder intact | **success — contender's bytes land** |
| Multipart ETag (`<hex>-<N>`) reused in a later `If-Match` | — | success | success |
| `DeleteObject` with a stale `If-Match` | `412 PreconditionFailed`, object survives | **success — object deleted** | **success — object deleted** |
| **ObjectFS capability probe verdict** | supported | supported | **unsupported** |

## What the exceptional cells mean

### Ceph RGW answers 412 for a key that does not exist

AWS and MinIO both answer `404 NoSuchKey` to an `If-Match` against an absent key. RGW answers `412
PreconditionFailed`.

That is the distinction every compare-and-swap loop in this codebase is built on. `404` means *the
object you are updating is gone — stop*. `412` means *you lost a race — re-read and try again*. An
endpoint that conflates them leaves a caller unable to choose, and the wrong choice is a loop
retrying forever against a key nobody is going to recreate.

This is also the cell that broke the probe. Until 2026-08-08 the probe read **any** 412 as proof the
header had been evaluated, so RGW passed — and RGW then turned out to have the next problem too.

### Ceph RGW ignores preconditions on `CompleteMultipartUpload`

RGW enforces `If-None-Match: *` on `PutObject` and ignores it on `CompleteMultipartUpload`. Probed by
completing an upload over a key that already held bytes: the request succeeded and the contender's
bytes replaced the holder's.

Multipart is not an exotic path. `PutObjectIf` above `MultipartThreshold` has to evaluate the
precondition at Complete, because parts are not an object until they are assembled. So on RGW **a
conditional write large enough to be multipart is silently unconditional** — the same call, the same
API, the same success, and the guarantee gone for exactly the writes where a race matters most.

This is the finding that made the probe fix a correctness fix rather than a documentation exercise,
and it is the shape a configuration flag or an endpoint-URL allowlist would never have caught.

### Ceph RGW rejects the ETag it just returned

RGW's `PutObject` returns `"6654c734ccab8f440ff0825eb443dc7f"` — quoted, as S3 does. Sending that
value back verbatim as `If-Match` gets `412`. The bare hex digest, same characters without the
quotes, succeeds. Verified at the wire with a request dumper, not inferred from a mismatch.

`PutObjectIf` passes `Precondition.ETag` through unchanged — deliberately, since the value a caller
holds is the one the store gave it and reformatting a token is how a token stops matching. So
compare-and-swap on RGW cannot be performed at all, independent of the two findings above.

### AWS answers 501 to `If-Match: *`

`If-Match: *` — "an object exists here, whatever it is" — is `501 NotImplemented` on AWS S3 and
accepted by both MinIO and RGW. ObjectFS does not send it (`Precondition` has `Absent` and `ETag`, and
neither maps to `If-Match: *`), and it should stay that way: the portable form of that assertion is
`If-Match` with a specific ETag.

### MinIO and RGW ignore `If-Match` on `DeleteObject`

AWS answers `412 PreconditionFailed` and leaves the object in place — probed, not taken from the
documentation. MinIO and RGW both accept the header, ignore it, and delete the object: success, and a
following `HEAD` returns 404.

**Not a live defect.** Nothing in ObjectFS issues a conditional delete; `If-Match` appears only on
`PutObject` and `CompleteMultipartUpload`. It is recorded because the failure mode is invisible — a
conditional delete that drops its condition looks exactly like one that honored it — and a future
lease that *released* by conditional delete would be unsafe on both endpoints with nothing reporting
it. `TestCompatConditionalDeleteIsNotReliedUpon` exists to keep that from being discovered the hard
way.

### Wasabi

**Unverified. No endpoint was available when this page was written**, and Wasabi has no local or
emulated form to stand up — it needs a real account against the real service.

Recorded as unverified rather than filled in from Wasabi's documentation, deliberately. Two rows on
this page were documentation reads before 2026-08-08 and both were wrong: MinIO was recorded as
"source read, server enforcement unverified" and enforces everything AWS enforces, while Ceph RGW was
recorded as "no conditional write support — documentation absence" and in fact implements it
*partially*, which is strictly more dangerous than not implementing it. A documented behavior is a
claim about an endpoint; only a probe is an observation of one.

The capability probe treats Wasabi like any other unknown endpoint — it establishes the answer at
mount time from the endpoint in front of it, so an unprobed store is not an unguarded one. Filling in
this row needs credentials and the suite above; it does not need any new code.

## What ObjectFS does about it

**The capability probe decides, at mount time, from the endpoint actually configured.** Not from a
version string, not from the endpoint URL, not from a config flag — each of which would have called
RGW capable. `probeConditionalWrite` asserts an unmatchable `If-Match` against a key expected to be
absent and classifies the answer; a 412 is then disambiguated by a `HEAD`, because a 412 alone cannot
distinguish an endpoint misreporting absence from a leftover object at the probe key.

**An unsupported endpoint is refused, never downgraded.** `PutObjectIf` returns
`types.ErrNotSupported` rather than falling back to an unconditional `PutObject`. Falling back is the
one behavior that must never happen here: it would report success to every contender for a lease, which
is the exact outcome the mechanism exists to prevent. A coordination feature on such an endpoint
declines to start, and the operator gets `ConditionalWriteDetail` naming what the endpoint did.

**Unknown means unsupported.** Every probe answer that is not positively "the precondition was
evaluated" — a timeout, an `AccessDenied`, an unrecognized code, a `HEAD` that could not be completed
— leaves the capability false. An answer not established is not an answer, and false is the direction
where the failure is loud.

## Practical guidance

| If you run | Then |
|---|---|
| **AWS S3** | Coordination features work. This is the reference implementation. |
| **MinIO** ≥ `RELEASE.2025-09-07` | Coordination features work. Enforcement is real, verified here rather than assumed — but it is an extension of the S3 API, not a contract, so the version above is part of the claim. |
| **Ceph RGW** ≤ 19.2.0 | Coordination features **refuse to start**, correctly. Conditional writes are partially implemented in a way that cannot be worked around from the client: no CAS (ETag rejected), and multipart writes unconditional. Use a coordination backend other than the object store. |
| **anything else** | The probe decides at mount time. If it reports unsupported, `ConditionalWriteDetail` says what the endpoint did; add a row here by running the `s3compat` suite against it. |

## Keeping this page honest

A matrix in a document rots the same way a version number in prose does — there is no way to tell it
it is stale. Three things guard against that:

- The rows come from a **committed, runnable suite**, not from a session's notes. Anyone can re-run
  it against any endpoint and compare.
- The suite **compiles in CI** (the `s3compat` cell of the `build-tags` matrix), so it cannot silently
  stop building as the code under it moves — which is how four other tags in this repo rotted.
- The suite's central assertion is not "RGW behaves as recorded below" but **"the capability probe
  agrees with what this endpoint actually does"**, which stays meaningful as endpoints change. It
  fails when a probe claims more than the endpoint delivers, and only logs when it claims less: the
  first costs correctness, the second only performance.

Reverting the 412 fix and re-running the suite against RGW fails with
`the capability probe reports conditional writes supported, but this endpoint enforces-absence=true
distinguishes-absent-from-stale=false enforces-multipart-precondition=false` — so the suite is known
to catch the defect it was written for, rather than assumed to.
