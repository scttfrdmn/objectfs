/*
Package testaws provides an in-process AWS S3 endpoint for ObjectFS tests.

It wraps [github.com/scttfrdmn/substrate/emulator].StartTestServer, which serves the S3 API over
loopback with no network, no credentials, and no charges. A test calls [Start] and gets back an
[github.com/scttfrdmn/objectfs/internal/storage/s3].Config already pointed at it, so exercising the
real backend against real HTTP costs a few milliseconds.

# Why an emulator and not a mock

The v0.10.0 audit found roughly forty-five defects that 32,680 lines of tests across 90 files had
all missed. They were overwhelmingly *seam* defects: a value produced correctly at one layer and
dropped at the boundary to the next. A test that mocks [github.com/scttfrdmn/objectfs/pkg/types].Backend
cannot see them, because the mock is on the far side of the seam — it is a restatement of what the
caller believes, so it agrees with the caller by construction.

The defect that motivated this package is the sharpest example. Three of the four S3 clients ObjectFS
built applied the configured endpoint; the connection pool's factory applied nothing, so HeadObject,
DeleteObject, ListObjects, and the health check addressed real AWS while PutObject and GetObject
addressed the configured endpoint. Every unit test passed. Nothing short of an actual endpoint that
the pooled client was supposed to reach could have caught it.

# Capabilities

The emulator implements a subset of S3, and the subset moves. A test that needs behavior the
running emulator does not have must **skip loudly, never pass quietly** — a harness that returns a
whole object for a ranged GET does not fail the read-path tests, it ratifies the bug. [Capabilities]
probes the running server and reports what is actually there, and helpers like
[TestServer.RequireRangeGET] turn a missing capability into a skip that names the upstream issue.

# Memory, under a fuzz target

A fuzz target using this package needs an explicit GOMEMLIMIT, and the reason is not that the target
allocates too much.

`go test -fuzz` runs one worker **process** per CPU. Each is an independent Go runtime, and each sizes
its heap against total machine memory, because none of them knows the others exist. On a 4-CPU runner
that is 4× over-commitment before any test code runs. Add a target whose every iteration is a real
read-modify-write over a real HTTP endpoint and the four runtimes will each decline to collect,
because from each one's point of view there is memory to spare. Measured on a 7 GB container:
7.2 GB RSS and a cgroup OOM kill at ~40 seconds.

The failure does not look like a memory failure. `go test` reports "fuzzing process hung or terminated
unexpectedly while minimizing: EOF", names an input, and writes it to the corpus — and that input
replays green, because it is simply whichever one the killed worker happened to be holding. The first
occurrence here was written off as runner preemption. See #193.

So `GOMEMLIMIT` is set for every fuzz target in CI, not just the one that died: the cause is the
per-worker runtime's view of memory, which every target shares, and a target that allocates little
never reaches the limit anyway. The cap is not a tax — the memory it reclaims is garbage the collector
had no pressure signal to collect, so bounding it returned 17× more executions in the same 60 seconds.

# What this package is not

It is not a substitute for the live AWS suite. Real S3 has consistency behavior, throttling,
multipart minimums, and error taxonomies the emulator approximates. Integration tests under the
`integration` build tag run against real S3 in us-west-2 and remain the final word; this package is
what makes the fast path fast enough to run on every commit.
*/
package testaws
