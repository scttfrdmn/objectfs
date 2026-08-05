# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **`sdks/python/requirements.txt`, which is both the pinned tree CI installs and the only reason
  anything scans this SDK's dependencies** ([#343]). 21 packages, transitives included, resolved on
  Python 3.12 to match `actions/setup-python`. The `sdk-metrics` job installs `-r requirements.txt`
  then the package with `--no-deps`, so what CI tests is a tree a reader can reproduce rather than a
  fresh resolution of six `>=` floors on every run — the defect #337 fixed for `docs-site`, in a
  different language.

  The scanning half is the more serious one, and it was measured. `trivy fs` over this repository
  reported four targets — `go.mod`, `sdks/java/pom.xml`, and the two `package-lock.json` files — and
  `sdks/python` was absent, because Trivy detects Python from `requirements.txt`, `poetry.lock`,
  `Pipfile.lock` or an installed `.dist-info`, and the only manifest here was `setup.py`. It now
  reports five targets. That is #201's complaint about the JavaScript SDK, in the one directory that
  closing #201 did not reach.

  **The filename is load-bearing, which cost a rewrite to discover.** This was first written as
  `requirements-ci.txt`, the more descriptive name — and Trivy does not detect that. Scanned in a
  scratch directory it reported no targets at all, so the file would have read as the fix while
  leaving the gap exactly where it was. `requirements-dev.txt`, `dev-requirements.txt` and
  `requirements/ci.txt` are equally invisible; only the literal `requirements.txt` matches. A bare
  `pyproject.toml` is not detected either, which is worth stating because it is the modern-practice
  answer and it does not solve this problem.

  `install_requires` deliberately stays as ranges: a library that pins its transitive tree pins it
  for every consumer. `pip check` now runs between install and test, which catches the one thing that
  split can get wrong — a pin here that does not satisfy a range there. Verified by mutation rather
  than assumed: pinning `psutil==5.7.0` against a `>=5.8.0` floor gives
  `objectfs 0.1.0 has requirement psutil>=5.8.0, but you have psutil 5.7.0` and exit 1.

  Two dependencies were removed rather than pinned, because neither was one. **`asyncio`** has been a
  stdlib module since Python 3.4 and this package requires >=3.8; the PyPI distribution is a 3.3-era
  backport, now a deliberate empty stub whose own summary reads "Deprecated backport of asyncio; use
  the stdlib package instead." Checked in a scratch venv: `asyncio.__file__` still resolves to the
  stdlib with it installed, so it shadowed nothing — but it declared a dependency this SDK does not
  have, and older resolutions of that range are not empty stubs. **`typing-extensions`** is imported
  nowhere in the package. The suite's 71 tests pass with both gone.

  A `pip` ecosystem entry for `/sdks/python` is added to `dependabot.yml`, which needed the pinned
  tree to exist first for the same reason `docs-platform`'s security updates needed a lockfile. Its
  `python` label was added to `.github/labels.yml` and synced from there rather than being created by
  the first PR that applies it — which is how `java` got onto the repository, and is the drift that
  file exists to prevent.

[#343]: https://github.com/scttfrdmn/objectfs/issues/343

- **A gate that fails when a `package.json` and its `package-lock.json` disagree, so `npm ci` cannot
  refuse to install in a later job** ([#332]). `TestNPMLockfilesAgreeWithTheirManifests` compares
  each manifest's four dependency tables against the same tables in the lockfile's `packages[""]`
  entry — npm's own record of what the manifest said when the lock was generated — and reports the
  directory and package name in seconds, in the `test` job, before anything installs.

  The failure it replaces was real and badly phrased. `npm ci` refuses to run on a mismatch, so a
  manifest edited without regenerating the lock surfaced as an npm error deep in whichever job
  installed first: `` `npm ci` can only install packages when your package.json and
  package-lock.json are in sync [...] Invalid: lock file's @types/node@18.19.130 does not satisfy
  @types/node@26.1.2 ``. Nothing in that names the actual mistake.

  Comparing the lockfile's own root entry, rather than diffing the two files a pull request touched,
  is deliberate. A changed-files check needs a base ref, does not work under a plain `go test`, and
  passes a hand edit that touches both files without regenerating the tree. This runs from a single
  checkout and asks the question npm asks.

  Both directions are checked — a range the lockfile does not record, a range recorded at a
  different version, and a package the lockfile still records after the manifest dropped it — and
  all three were mutation-checked against the real tree rather than predicted. A fourth subtest
  globs `git ls-files` for tracked `package.json` files and fails on any directory not in the list,
  so a third npm directory cannot be added without a lockfile gate; that one was mutation-checked
  too. What this does *not* verify is the resolved tree below the root: whether the locked versions
  satisfy the ranges, and whether every transitive dependency is present. Only `npm ci` answers
  that, and `sdk-metrics` and `docs-site` both run it.

[#332]: https://github.com/scttfrdmn/objectfs/issues/332

- **`build` and `lint` steps in CI for the JavaScript SDK, which is the gate whose absence let 48 type
  errors ship** ([#314]). The `sdk-metrics` job ran `npm ci && npm test` as a single step; `tsc` was
  never invoked by anything, in any job, ever. It now runs as its own step, so a type error fails the
  build rather than waiting for someone to run the compiler by hand.

- **A `mvn -B test` step for the Java SDK, the same gate for the same reason** ([#325]). Nothing in
  this repository had ever run Maven, and the SDK did not compile; see the `Fixed` entry below for the
  four errors that had been sitting there. The tests need no credentials and no network beyond
  dependency resolution — they use `MockWebServer` — so there is nothing here for a flake to come
  from, and `continue-on-error` is deliberately absent.

- **A `make test` step for the C SDK, closing the last of the four** ([#325]). This SDK differs from
  the JavaScript and Java ones in that it *did* build: `sdks/c` is a package of the main module, so
  `go build ./...` compiled its cgo Go side all along. What no job did was link a C program against the
  shared library and run it — so `objectfs.h`, `objectfs_types.h` and the assertions in
  `tests/test_basic.c` were unverified, and a header could declare a prototype the library does not
  export, or a struct field too narrow for the value written into it, with every job staying green.
  Which is what had happened; see the two `Fixed` entries below. Both suites skip their integration
  halves without `OBJECTFS_TEST_BUCKET`, so the step needs no credentials and reaches no network.

- **Go tests for the C SDK's Go side** (`sdks/c/main_test.go`), which had none — only
  `tests/test_basic.c` and `tests/test_smoke.py`, both of which reach the library through the C ABI and
  therefore cannot see a helper that never crosses it. `bytesToCacheString` was such a helper and was
  discarding up to half the requested cache size. The file cannot use cgo — `go test` rejects
  `import "C"` in a test file — so the return codes are pinned by parsing `objectfs_types.h` and
  comparing it against constants written out independently, which is strictly stronger than comparing
  `C.OBJECTFS_OK` to itself: it catches the header and the library disagreeing about what a code means,
  which nothing previously could, since the C test binary uses the macros on both sides of its own
  assertions. Field widths are checked the same way, against the longest value each field can receive.

  Two assertions live in `tests/test_basic.c` instead, because they are only observable from C: that a
  1024-byte key round-trips through `objectfs_head` and `objectfs_list` byte-for-byte, and that
  `objectfs_last_error` returns the same pointer on repeated calls. The first is in the
  credentials-gated section; the second is not, since a never-issued handle reaches the arm that
  leaked. Each fix was confirmed by reverting it and watching the specific assertions fail — the
  key-width revert reported `char[1024] ... holds 1023 bytes ... but must hold 1024`, and the leak
  revert failed exactly the two pointer-identity checks.

- **Tests for the JavaScript SDK's configuration and storage layers** (`src/config.test.ts`,
  `src/storage.test.ts`; 61 tests across three suites, up from one). The configuration assertions come
  in pairs — the override applied *and* its siblings survived — because a one-level spread passes any
  test that only checks the override, which is how the merge bug lived through a release. A loop
  asserting that every shipped preset passes `validate()` is what found the third broken preset after
  a hand-written probe had found two. The load-bearing storage test writes a real file, calls
  `downloadObject` against it, and asserts the file's contents are unchanged.

  `src/index.test.ts` covers the entry point, which nothing checked: it asserts that every class
  `src/errors.ts` declares is re-exported — as a loop, so a class added later is covered by existing
  code — that the re-exported binding is *the same* binding, since a re-export that diverged would
  silently break `instanceof` in a caller's `catch`, and that `LICENSE`/`VERSION` match
  `package.json`. The names are looked up dynamically rather than written as `index.CacheError`
  because a missing export is a TS2339 that fails the whole suite's compile, which would report a
  type error instead of the missing name and take every other assertion in the file down with it.

- **Tests for the Python SDK's storage, configuration and client layers** (`tests/test_storage.py`,
  `tests/test_config.py`, `tests/test_client.py`; 71 tests and 25 subtests, up from a single metrics
  suite). Same three shapes as the JavaScript tests, for the same reasons: a loop over every preset
  name asserting `validate()` passes, which is what found the `cluster` defect; paired merge assertions
  checking the override applied *and* its siblings survived; and a test that writes `REAL USER DATA` to
  a real file, calls `download_object` against that path, and asserts the bytes are still there. The
  client suite additionally asserts the not-implemented message reaches the caller unwrapped, since
  that fix has no other guard.

  Every one of these was verified by mutation rather than by inspection: restoring the fabricating
  `_download_s3_object` and the preset's `tls_enabled = True` produced exactly the five expected
  failures — including `test_existing_file_survives` and the `cluster` subtest — and restoring the
  double-wrap failed exactly the two assertions written for it. The CI step that runs them is renamed
  from "Python SDK metrics parsing" to "Python SDK tests", because it invokes `pytest tests/` and had
  been picking up more than its name claimed.

- **A written evaluation of S3 conditional writes as a replacement for Raft coordination**
  ([#169]), at `docs/design/conditional-writes-vs-raft.md`. The recommendation is to adopt per-key
  compare-and-swap for coordination, keep gossip for membership, and stop building toward a
  replicated log, on the grounds that there is nothing to replicate: Raft exists so that N nodes
  agree on state they each hold a copy of, and ObjectFS nodes hold no such state — S3 does. Every
  operation `internal/distributed` coordinates ends in a write to a key in one bucket that every
  node can already read.

  The CAS properties are verified by execution against a substrate endpoint over real HTTP rather
  than asserted from AWS's documentation: exactly one of 32 concurrent `If-None-Match: *` PUTs wins,
  a stale-ETag `If-Match` write gets `PreconditionFailed` and leaves the object unchanged, `If-Match`
  against an absent key gets `NoSuchKey` rather than 412, and eight workers running a read-then-CAS
  increment loop converge to exactly eight with no lost updates. `PutObject` also returns the *new*
  ETag, which is what lets a CAS loop make progress without a HEAD between iterations — one request
  per lease renewal rather than two.

  Two findings the issue did not anticipate, both of which change what a caller must do. First, the
  failure taxonomy is three-way, not two-way: `409 ConditionalRequestConflict` is distinct from `412`
  and means a delete interleaved, and while a `PutObject` may simply be retried, a
  `CompleteMultipartUpload` that gets a 409 has a dead upload ID and must be re-initiated from
  `CreateMultipartUpload`. A loop treating 409 as a synonym for 412 spins until it gives up, and
  substrate does not model 409, so the mistake is currently invisible in testing — filed as
  [scttfrdmn/substrate#540]. Second, Ceph RADOS Gateway does not support conditional writes at all:
  its PUT Object documents only `content-md5`, `content-type`, `x-amz-meta-*`, and `x-amz-acl`, and
  conditional headers are documented for reads only. So the compatibility rule has to be to fail
  closed — an unconditional fallback would turn "exactly one node performs this tier transition" into
  "every node does," which is the failure the coordination exists to prevent, occurring silently.

  **The recommendation has been adopted.** The Raft build-out is closed as not-the-direction — [#128]
  (`ConsensusLog`), [#130] (`PersistentState`, the only one labeled `priority: critical`), [#133]
  (real proposal broadcast), [#151] (log compaction) and [#150] (the bbolt implementations of the two
  interfaces #128 and #130 defined, which the design doc's own closure list had missed) — and the work
  is filed as [#282]
  (`Backend.PutObjectIf`, the sentinel errors, and a capability probe that detects by attempt rather
  than by configuration), [#283] (a lease whose every guarded action re-asserts the CAS), [#284]
  (replacing `executeStrongConsistency`'s N-identical-PUT fan-out and fixing the consistency
  taxonomy) and [#285] (verifying MinIO and Wasabi against real endpoints). Gossip stats ([#132]) and
  gossip authentication ([#206]) are unaffected; rendezvous hashing ([#131]) had already landed.

  Closing the issues stopped the build-out but did not remove the ~6,000 lines that already elect
  leaders and replicate nothing — [#284] is where the first of that comes out, and it is a breaking
  change to the distributed configuration surface, because the three consistency levels it removes
  turn out to be three names for two behaviours: `ConsistencySession` and `ConsistencyEventual` are
  now nearly the same function, both executing on `targetNodes[0]` and then replicating
  asynchronously, and `ConsistencyStrong` is the mislabeled fan-out.

### Security

- **Gossip messages are authenticated, and a cluster will not start without a shared secret**
  ([#206]). The gossip protocol runs over UDP and verified nothing about who sent a datagram, so any
  host that could reach the port could add itself to the cluster, announce that a node was dead, or
  announce that it held the current copy of a cached object — which is a path to a reading process
  being served bytes chosen by whoever sent the last announcement. Every message now carries an
  HMAC-SHA256 over its exact bytes, checked before the payload is parsed, so an unauthenticated
  datagram never reaches the decoding of any message type, let alone a handler.

  The secret comes from `OBJECTFS_CLUSTER_SECRET` or from a file named by `SecretFile`, and never
  from a YAML field: packaging installs the shipped configuration world-readable, so a secret there
  would be published to every user on the node. A secret file readable by anyone but its owner is
  refused, as is one shorter than 32 bytes, and both errors say how to generate a good one. If no
  secret is configured at all, `NewClusterManager` returns an error naming both sources rather than
  starting unauthenticated — running without authentication is the failure nobody notices, so it is
  refused at construction. That fail-closed behavior broke five existing tests when it landed, which
  is the evidence that it cannot be bypassed.

  A MAC authenticates the sender but not the moment, so messages also carry a timestamp and ID
  checked against a 30-second window with a nonce cache: a captured "node N owns key K" cannot be
  replayed later to undo the state that replaced it. Freshness is checked *after* the MAC, because a
  nonce cache writable by an unauthenticated sender could be flooded with random IDs to evict the
  real entries and re-open the window. Rejections are counted separately for a bad MAC, a replay,
  and an unknown envelope version, and each logs a different operator hint — a cluster of one with a
  rising unauthenticated count is a wrong secret, while the same cluster with a rising wrong-version
  count is a half-finished upgrade. What this does not do is stated in the package documentation:
  payloads are not encrypted, and because every member holds the same key, a compromised node can
  impersonate any other.

- **The documentation platform's three `vite` advisories are closed, by an `overrides` block rather
  than by the version bump they asked for.** Alerts 1–3 all name `vite` in
  `docs-platform/package.json`, and every one of them has its lowest patched release on the 6.x line
  (`<= 6.4.2` → 6.4.3, `<= 6.4.1` → 6.4.2). There is no patched 5.x, so `"vite": "^5.0.10"` was
  permanently vulnerable no matter how far it moved inside the major.

  Bumping the declared range to `^6.4.3` on its own does not close them, which is worth recording
  because it looks like it should. `vitepress@1.6.4` — the latest *stable* release — depends on
  `vite: ^5.4.14` as a regular dependency rather than a peer, so npm is free to satisfy it
  separately: the install produces `vite@6.4.3` at the top level and
  `node_modules/vitepress/node_modules/vite@5.4.21` underneath it, and the nested copy is the one
  that builds the docs. Verified by installing exactly that and running `npm audit`, which still
  reported `high vite <=6.4.2`. The manifest would have looked fixed while the vulnerable code was
  still on disk and still executing.

  An `overrides` block is what forces the transitive copy up too. With `vite: ^6.4.3` there,
  `npm audit` reports zero vite findings and no nested `vite` directory exists. This keeps
  `vitepress` on its current stable version; the alternative was `vitepress@2.0.0-alpha.19` with
  `vite@^7`, which also clears the alerts but makes a published pre-release a dependency of the docs
  build, and an override is the smaller commitment for the same result.

  `uuid: ^11.1.1` is overridden for the same structural reason — a missing buffer-bounds check in
  `uuid` v3/v5/v6 reachable through `dockerode` → `docker-modem`, which `src/api-server.js` uses to
  run playground containers. `dockerode@5.0.1` drops the dependency entirely, but that is a major
  bump of a package with live call sites; the override fixes the vulnerable code without touching
  them. `dockerode` and `docker-modem` were both confirmed to load and `uuid.v4()` to work under the
  forced version. `npm audit` in `docs-platform` now reports zero vulnerabilities of any severity,
  down from three high/moderate vite findings plus two moderate `uuid`/`dockerode` findings that no
  alert had been opened for.

  A caveat this does not fix, and which is the reason none of it can be verified by CI: **the docs
  platform does not build, on `main` or with these overrides**, and nothing in CI runs
  `vitepress build` to notice. Filed as [#317]. The override does move the failure later — vite 6
  loads the ESM-only `vitepress` that vite 5 refused with *"ESM file cannot be loaded by
  `require`"*, so the build now reaches page compilation instead of dying in config resolution —
  but "later" is not "passing", and the version claim here rests on `npm audit` and on module
  loading, not on a successful build.

[#206]: https://github.com/scttfrdmn/objectfs/issues/206
[#317]: https://github.com/scttfrdmn/objectfs/issues/317

### Removed

- **`tests/posix_test.go`, `tests/integration/`, and `pkg/optimization` — three suites and a package
  that had never compiled, run, or been imported** ([#197], [#240]).

  `tests/posix_test.go` was 400 lines of POSIX conformance assertions behind the `posix` tag. It did
  not fail; it was never built. Repairing it was tried first and abandoned on evidence: removing the
  duplicate `MockBackend` yields `unknown field files`, fixing that yields `undefined: vfs`, adding
  the import yields `not enough arguments in call to metrics.NewCollector`. Three API generations, one
  after another. Its own `MockBackend` copy had meanwhile been half-edited by some later sweep — it
  takes a `meta` argument and carries a rename-preserves-metadata comment — which is the clearest
  evidence available that someone updated this file without ever compiling it.

  What it asserted is now asserted elsewhere, and better. Its eight suite methods covered mount,
  file create/read/stat/delete, mkdir/readdir/rmdir, permissions, seek, concurrent reads, and stats.
  `internal/fuse` has 13 test files over Lookup, Readdir, Mkdir, Create, Unlink, Rmdir, Rename,
  Chmod, Setattr, Fsync, Statfs, attributes, the read path, cache invalidation and errno mapping —
  against a real in-process S3 endpoint rather than a map. `internal/difftest` runs write, read,
  truncate, flush, reopen and stat sequences against ObjectFS *and* the local OS filesystem and
  asserts they agree. `internal/fuse/kernel_options_live_test.go` covers what genuinely needs a
  kernel, behind `fuse_mount`, and is compiled in CI. The deleted file's `assert.NoError` on
  `manager.Mount` also meant it would have failed rather than skipped on any runner without
  `/dev/fuse`, so it could not have been added to CI as written.

  `tests/integration/` was a LocalStack suite. `CLAUDE.md`, `CONTRIBUTING.md` and `DEVELOPMENT.md`
  all rule LocalStack out, and nothing in the repository set the `AWS_ENDPOINT_URL` it required — so
  it never ran even in the era when it compiled. Its companion `mocks.go` existed to assert that
  `pkg/optimization`'s interfaces are satisfied by mocks written for that assertion, and those two
  files were `pkg/optimization`'s **only importers in the entire tree**, so the package went with
  them. A 284-line interface set whose sole consumer was a test of its own self-consistency.

  Two pieces of wiring went too, both broken independently of the deletions. The
  `integration-ready-check` pre-commit hook ran `go test -tags=integration ./tests/...` whenever
  `AWS_ENDPOINT_URL` was set, inferring LocalStack from an environment variable that means nothing of
  the kind. And `make test-integration` ran `go test -tags=integration ./test/integration/...` — a
  directory that has never existed, so the target failed with `lstat ./test/integration/: no such
  file or directory` for as long as git records it. The `integration` tag itself survives: it marks
  real-AWS tests inside `internal/awsrates` and `internal/storage/s3`, which `make test-aws` and the
  commands in `CONTRIBUTING.md` run.

[#197]: https://github.com/scttfrdmn/objectfs/issues/197

- **`markdown-it`, `markdown-it-anchor` and `markdown-it-container` from `docs-platform`, none of
  which the site was using** ([#299]). Dependabot proposed the `markdown-it` 15 major, which is what
  prompted looking at it. The answer is that the dependency should not be there at all, and the
  reason it looked load-bearing is worth writing down, because it is a trap the next person will hit
  in the same order:

  `.vitepress/config.js` had a `markdown.config` hook registering `markdown-it-container` for 'tip',
  'warning' and 'danger'. That reads as a genuine use of a genuine dependency. It is three kinds of
  redundant. VitePress ships those exact three containers built in. No page in this tree uses `:::`
  syntax at all — the 142 `custom-block` elements in the rendered output all come from VitePress's
  own handling. And the `md` object handed to that hook is VitePress's *bundled* markdown-it
  instance, not the top-level one: `vitepress` 1.6.4 does not list `markdown-it` in its
  `dependencies` and carries its own copy inside `dist/node/`. So the hook that appeared to justify
  the top-level `markdown-it` was never extending it.

  Measured rather than reasoned: removing all three from `node_modules` and rebuilding produces a
  site that builds, with all six pages byte-identical to the baseline once asset content hashes are
  normalized. Confirmed the other way too — leaving the hook in place with the package gone fails
  the build with `Cannot find module 'markdown-it-container'`, so the hook and the dependency are a
  matched pair and neither should be removed alone.

  This is also the answer to #299 on its merits. A major bump of a dependency nothing imports is a
  lockfile change with no behaviour attached, and holding or taking it was the wrong question.

[#299]: https://github.com/scttfrdmn/objectfs/pull/299

### Fixed

- **A `failure_threshold` above 2³² opened the circuit breaker before the first request, rejecting
  every S3 operation for the life of the mount** ([#264]).

  `readyToTrip` converted the configured `int` threshold to the `uint32` that
  `circuit.Counts.TotalFailures` is, guarded only by `if cfg.FailureThreshold <= 0`. That guard bounds
  the sign and not the width. On a 64-bit platform `failure_threshold: 4294967296` passes it, narrows
  to `0`, and yields the predicate `TotalFailures >= 0` — true on the zeroth failure. The breaker
  opens immediately and stays open, which the function's own doc comment already named as the outcome
  its zero case exists to avoid; it arrived by the one path that bypassed that case. `4294967297` is
  the quieter half: it narrows to a threshold of `1`, so the breaker trips on a single failure while
  the configuration says four billion.

  Now clamped. A threshold above `math.MaxUint32` becomes a predicate that never trips, because no
  count of failures can reach it when the counter is a `uint32` — unreachable is the honest reading of
  an unreachable number, and clamping keeps this a total function rather than adding a second
  validation site. `internal/config` already rejects a negative threshold at load.

  Four mutations of the clamp are detected by `TestCircuitBreakerConfigReachesTheBreaker`: removing
  it, `>` → `>=`, restoring the always-true predicate, and bounding at `MaxInt32` instead. The jumbo
  cases are behind `math.MaxInt > math.MaxUint32` because on `linux/386` and `linux/arm` the literals
  do not compile, and skipping at runtime would not be enough.

- **Twelve `// nolint` directives had a leading space, which makes them ordinary comments** ([#264]).

  `// nolint:gosec` is not a directive; only `//nolint:` is. Twelve sites across nine files carried
  the spaced form with a full paragraph of justification each, and every one of them was inert.
  Whether that mattered depended on the site: at `s3/config.go:202` the finding was also excluded
  repo-wide by `.golangci.yml`, so it was reported by neither run and turned out to be the real defect
  above. `gofmt` moves a `//nolint:` to the end of its comment block, so the four multi-line cases
  were restructured to put the prose first.

  Eleven further `//nolint` directives had no explanation, which `nolintlint`'s
  `require-explanation` reports. Five were `//nolint:staticcheck` on findings that were simply
  fixable — two `QF1008` embedded-field selectors, two `QF1003` if-chains that wanted a tagged switch,
  and a `QF1012` `WriteString(Sprintf(...))` — so those are fixed rather than annotated. `nolintlint`
  is now at 0 findings, down from 23.

- **The six gosec sites #264 lists are suppressed for the run that reports them, and the convention is
  written down in `.golangci.yml`** ([#264]).

  Two of the issue's premises do not hold, both measured:

  - **`#nosec` alone satisfies both runs, and writing both directives fails lint.** golangci-lint runs
    gosec's own analyser, which strips `#nosec` sites before golangci-lint sees them. So the dual
    annotation the issue prescribes leaves the `//nolint:gosec` with nothing to suppress, and
    `nolintlint` reports it unused — turning a suppression into a lint failure.
  - **`G703` does not exist in the gosec golangci-lint bundles.** Probed on a two-line program
    reading `os.Getenv`: standalone gosec reports `G703` and `G304`, golangci-lint reports `G304`
    alone. The `//nolint:gosec` at `internal/awsname/awsname.go` could not have worked even written
    correctly, because that run never had the finding.

  Also recorded there: no suppression directive of either form works in a cgo package (see the header
  of `sdks/c/main.go`), and `internal/network/congestion_linux.go` is linux-only, so a `//nolint` there
  is unverifiable from a macOS developer machine while the standalone run on ubuntu does report it.

- **`internal/testaws`'s hardcoded credential is now provably test-only** ([#264]).

  `SecretAccessKey` is AWS's own documentation example key and is accepted only by the substrate
  emulator, which is the whole `G101` argument and not the whole risk: the constant is at package
  scope in `testaws.go`, not a `_test.go` file, so nothing about the filename keeps it — or the
  recording proxy, or the embedded emulator — out of a shipped binary. What keeps them out is that no
  non-test package imports `testaws`, which is a property of the import graph and drifts silently.
  `TestNoNonTestPackageImportsThisOne` asserts it via `go list -deps`, whose `Imports` field is the
  non-test import set.

  Verifying that test took two attempts, and the first failure mode is recorded in it. `go test` runs
  each test binary in its own package directory, so the initial `./...` expanded to `internal/testaws`
  alone and reported no offenders while a deliberate violation sat in `internal/awsrates/offerfile`.
  It now runs `go list` from the module root. The mutation also needs `-count=1`: the violation lives
  in another package, so the test binary is byte-identical and `go test` serves a cached pass.
- **`objectfs_put` in the C SDK no longer narrows a `size_t` length to a `C.int`, which could store
  an empty object and report success** ([#200]).

  `objectfs_put` takes a `size_t` and passed `C.int(length)` to `C.GoBytes`. `size_t` is 64 bits on
  every platform this library ships for and `C.int` is 32, so the length was truncated. Measured in a
  standalone cgo probe rather than reasoned about, because the same narrowing fails three different
  ways:

  ```text
  length = 1<<32     (4 GiB)  ->  C.int 0            ->  len(goData) == 0
  length = (1<<32)+100        ->  C.int 100          ->  len(goData) == 100
  length = 1<<31     (2 GiB)  ->  C.int -2147483648  ->  panic: gobytes: length out of range
  ```

  The first is the dangerous one. A caller hands over 4 GiB, `objectfs_put` returns `OBJECTFS_OK`,
  and S3 holds an **empty object**. Nothing reports a short write, because from Go's side there was
  no short write — the length arrived as zero. Confirmed end to end against real S3 by removing the
  new guard and running `tests/test_basic.c`: the call returned `OBJECTFS_OK` and `aws s3api
  head-object` on the key reported `"ContentLength": 0`. The third case is worse in a shared library
  than it would be in a program: a panic in a `c-shared` build tears down the host process, so a C
  caller loses unrelated state of its own to one bad argument. In the same run it aborted the test
  binary with exit 134 *before stdout was flushed*, so the harness printed nothing at all — not the
  21 assertions that had already passed.

  A length that will not survive the conversion is now refused with `OBJECTFS_ERR_INVALID` and an
  `objectfs_last_error` message naming both the length and the limit, rather than converted and used.
  The bound is `math.MaxInt32` because that is what `C.GoBytes` can represent, not a policy about
  object size; the comparison is `>` rather than `>=` so the boundary itself stays usable.

- **`objectfs_list` rejects a negative `limit` instead of treating it as "no limit"** ([#200]).

  `objectfs.h` documents `limit` as "max results (0 = no limit)" and says nothing about a negative
  one. Downstream, `ListObjects` gates every use of `limit` on `limit > 0`, so `-1` meant "return the
  whole bucket" — which is what `-1` conventionally means in a good deal of C API design, and so
  would have looked deliberate, but it was an accident of a `> 0` comparison and nothing documented
  it. The `C.int` → `int` conversion itself was never unsafe; it widens on every target this builds
  for.

- **`security.yml` back-fills the cgo SARIF path instead of dropping the findings, and the audit
  those findings needed is done** ([#200]).

  gosec reported three G115 integer conversions in `sdks/c/main.go` with `"artifactLocation": {}`,
  which GitHub's SARIF ingester rejects — and it fails the *whole* upload, taking the other ~45
  findings with it. This workflow dropped them, on the stated grounds that gosec "cannot map the
  location back to a real file".

  That was half wrong, and the wrong half was the useful one. **The line numbers are exactly right.**
  cgo emits `//line` directives into the file it generates, so each reported line is a real
  `main.go` line — verified by inserting eight blank lines after the import block, which moved all
  three findings by exactly eight. Only the path is absent, and `sdks/c` is the only cgo package in
  the module, so there is one path it can be. The filter now sets it, scoped to results that have a
  region but no URI so a genuinely malformed location elsewhere is still dropped rather than silently
  relabelled; verified against the real 48-result SARIF plus three injected negatives (no region,
  no locations, `startLine: 0`), all three of which are still dropped.

  This matters because the alternative was measured and it was not "reviewed somewhere else":
  golangci-lint discards the same findings for the same reason, and says so in its own log —
  `runner/invalid_issue: issue related to file <go-build cache path> is skipped`. Three unreviewed
  integer conversions across a C ABI is precisely how the `objectfs_put` defect above survived.

  Two further findings from the same investigation, both stated in `sdks/c/main.go`'s header comment
  so they are not rediscovered:

  - **No gosec suppression directive works inside a cgo package.** Probed with four placements —
    above the call, first of a comment block, last of a block, and trailing the call. In a pure-Go
    package all four suppress; in a cgo package none do, and gosec reports `nosec: 0`, meaning it
    recognized no directive at all. cgo rewrites each `C.f(...)` call into an inline closure carrying
    `/*line :N:C*/` directives, which collapses the call and any nearby comment onto one synthetic
    position. The `#nosec` and `//nolint:gosec` annotations added during this work were therefore
    removed again in favour of plain comments that do not claim to be doing something.
  - **`CGO_ENABLED=0 gosec ./sdks/c/...`, which #200 suggested might resolve the paths, analyses
    zero files.** The package cannot build without cgo, so its clean report is a vacuous pass rather
    than a fix.

  Two `errcheck` findings in the same file were also fixed: `val.(*entry)` was a bare type assertion
  in `getEntry` and `objectfs_free`. Nothing but that file writes to the `sync.Map`, so neither could
  fail today — but a bare assertion converts any future violation into a panic, and a panic in a
  `c-shared` library takes the host process down rather than just the call.

- **`internal/fuse` compiles for 32-bit targets again, and `linux/armv7` is back in the release
  matrix** ([#198]).

  `safeIntToUint32` bounded its conversion with `if i > 0xFFFFFFFF`. `int` is 32 bits wide on 32-bit
  platforms and that constant is not, so the guard against overflow overflowed — a compile error, not
  a wrong answer:

  ```text
  $ CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build ./internal/fuse/
  internal/fuse/filesystem.go:37:9: 0xFFFFFFFF (untyped int constant 4294967295) overflows int
  ```

  It is now compared as `uint64(i) > math.MaxUint32`, which holds the value on every word width. The
  negative check stays first, and has to: `uint64(-1)` is 2^64-1, which would clamp to `MaxUint32`
  rather than to zero. `safeInt64ToUint64` beside it was audited too and has no equivalent defect —
  int64 and uint64 are both 64 bits everywhere Go targets, so it has no upper bound to test and no
  constant to overflow. That is now stated in its doc comment, because "why does the function above
  check two things and this one check one" is a reasonable question to have answered in place.

  **Why it survived to a release, which is the more interesting half.** That single line was the only
  error on the target — the whole tree cross-builds for `linux/arm` and `linux/386` with it fixed. But
  `linux/armv7` appeared in `release.yml`'s matrix and in nothing else: every `cross-build` cell in
  `ci.yml` was 64-bit, and no test on a 64-bit host can see this class of defect, because
  `i > 0xFFFFFFFF` compiles and behaves correctly on amd64 and arm64. So the break first appeared when
  a tag was pushed, where the cheapest response was to delete the cell — and in v0.10.1 that is what
  happened, taking the platform with it.

  So the durable fix is a coupling rather than a line. `ci.yml`'s `cross-build` matrix gains
  `linux/arm` (GOARM=7) and `linux/386` cells, and `TestEveryReleasePlatformIsCompiledInCI` fails if
  `release.yml` publishes a platform that matrix does not build. The relation is deliberately
  release ⊆ ci, not equality: `linux/386` is compiled and not published, as a second 32-bit
  word-width canary. `TestThirtyTwoBitIsStillInTheCrossBuildMatrix` asserts at least one cell has a
  32-bit `int` at all, stated by word width rather than by naming `arm`, so dropping one 32-bit
  platform while keeping another stays green and going all-64-bit does not. Same shape of gate as
  #240's build-tags matrix, and the same reasoning: an unbuilt target is not merely untested.

  Each cross-build cell also now runs `go vet ./internal/... ./pkg/... ./cmd/...` alongside its
  build. `go build` does not compile `_test.go` files, so a width bug in a test — `int(math.MaxUint32)`
  is the same error in the same shape, and the new test had to route around it — would still pass a
  green build cell. Scoped away from `sdks/c`, which is cgo and cannot be vetted under
  `CGO_ENABLED=0` for any target ([#200] covers that surface).

  `GOARM` is set explicitly rather than left to the toolchain default in both workflows: armv6 and
  armv7 binaries are not interchangeable and the artifact is named `linux-armv7`, so the flag should
  say 7 rather than trust that it will keep meaning 7.

  Verified five ways. Restoring the original comparison passes `go build`, `go vet` and the full test
  suite on this 64-bit host and fails `linux/arm`, `linux/386` and `./cmd/objectfs` for arm — the
  measurement that says where the gate has to live. Swapping the two branches, deleting the negative
  check, and deleting `safeInt64ToUint64`'s negative check each fail the new table tests. Removing the
  `linux/arm` cell from `ci.yml`, removing both 32-bit cells, adding an unbuilt platform to
  `release.yml`, and renaming the `cross-build` job all fail the coupling tests — the last one
  because a parser that reads nothing would otherwise pass vacuously, which is the failure mode a test
  that reads a workflow is most likely to have.

  One mutation is deliberately not claimed: `>` versus `>=` on the boundary is an equivalent mutant,
  since clamping `MaxUint32` to `MaxUint32` returns the same value either way.

  `DEVELOPMENT.md`'s cross-build section listed three of the five shipped platforms plus a
  `GOOS=windows` build that has never produced a binary — `internal/fuse` is `linux || darwin` and
  `platform_unsupported.go` fails deliberately. It now lists what `release.yml` publishes, notes the
  386 canary, and says why Windows is absent. Its local-build snippet also passed
  `-ldflags "-X main.Version=$VERSION"`, which does nothing: `version` is a `const` and the linker
  cannot rewrite a constant. Two other copies of that dead injection survive, in `Makefile:11` and
  `OBJECTFS.md:613`, and are filed rather than fixed here ([#353]).

[#198]: https://github.com/scttfrdmn/objectfs/issues/198
[#200]: https://github.com/scttfrdmn/objectfs/issues/200
[#353]: https://github.com/scttfrdmn/objectfs/issues/353

- **Every build tag is compiled in CI, and the two tagged files that had stopped compiling now
  compile** ([#240], [#197]).

  A file behind a build tag is excluded from the default build, so `go build ./...`, `go vet ./...`
  and `go test ./...` all pass without ever type-checking it. Not "untested" — unbuilt. Four of this
  repository's tags had rotted through that gap. `tests/aws_s3_test.go` called `PutObject` with three
  arguments at ten call sites after the method grew a fourth `map[string]string` parameter.
  `test/benchmarks/cache_test.go` called `fmt.Itoa`, which has never existed in any release of Go, so
  that file has not compiled since the day it was written. `tests/integration/` had the same
  `PutObject` breakage and `tests/posix_test.go` was three generations stale; both were deleted (see
  Removed). Each of the four was committed green.

  The gate is a `build-tags` job with one `go vet -tags=<tag> ./...` matrix cell per tag: `aws_s3`,
  `benchmark`, `distributed`, `e2e`, `integration`, `fuse_mount`. Vet rather than test, deliberately —
  several of these suites need real credentials, a bucket, or a `/dev/fuse` device and correctly
  refuse to run without one, but *compiling* them needs nothing, and compiling is what was missing.
  The bespoke `Compile the fuse_mount tag` step in the `test` job folded into this matrix; it was the
  one tag already handled correctly, and there is no reason for it to be handled differently.

  **The tag list is checked against the tree rather than maintained by hand**, because a hand-kept
  list of tags going stale is the defect this issue is about. `TestEveryBuildTagIsGated` walks every
  `//go:build` line in the repository, strips the boolean operators, discards the tags the toolchain
  selects on its own (GOOS, GOARCH, `cgo`, `race`), and fails if what remains is not in the matrix —
  naming the file, so the failure says where to look. `TestGatedTagsStillExist` is the converse and is
  not symmetric with it: a cell for a deleted tag does not fail, it *passes*, because
  `go vet -tags=gone ./...` selects no files and reports success. A green cell named after a suite
  that no longer exists is worse than a red one, so that is a failure too.

  Verified by mutation, five ways. Removing `e2e` from the matrix, adding a cell for the
  now-deleted `posix`, and introducing a new `//go:build sekrit` file each produce the intended
  failure. Restoring the original `fmt.Itoa` defect fails the `benchmark` cell. And the decisive one:
  a type error injected into `tests/e2e_test.go` passes `go build ./...` (exit 0), `go vet ./...`
  (exit 0) and `go test ./tests/` (exit 0, `ok`) while `go vet -tags=e2e ./...` exits 1. That gap,
  measured on this commit, is the whole issue.

[#240]: https://github.com/scttfrdmn/objectfs/issues/240

- **`fuzz-smoke` no longer fails on Go's own fuzzing-coordinator shutdown race, and still fails on
  every real find** ([#218]). The job failed roughly 1 run in 10 with `context deadline exceeded`
  and **zero** new crashers — the surveyed failure was `FuzzSliceRange`, a pure-function target over
  `sliceRange(data, offset, size)` whose test file contains no `context` at all, reported at 60.11s
  against `-fuzztime=60s`. The uploaded artifact held only the already-committed corpus.

  The cause is upstream, in `$GOROOT/src/internal/fuzz/fuzz.go`: the coordinator builds a timeout
  context and a child of it, waits on the *parent*'s channel, calls `stop(ctx.Err())` with the
  parent's error, and then suppresses that error only if it equals the *child*'s. Because
  `cancelCtx.cancel` closes its own `done` before propagating into `c.children`, the child's `Err()`
  can still be `nil` at that moment, the comparison misses, and a normal timeout is reported as a
  failure. Reproduced standalone at 12 in 20,000 trials. The window is scheduling-dependent, which
  is why CI saw it and a fast local machine did not — the runner managed 3,323 execs/sec against
  36,819 locally.

  The step now runs through `.github/scripts/fuzz-smoke.sh`, which keys on **the presence of a
  counterexample** rather than on the exit status: a new file under `testdata/fuzz/<Target>/`, or
  `Failing input written to` in the output, fails the job unconditionally — checked before the exit
  status is consulted at all, because `go test` has reported a written input alongside a zero exit.
  Only two shapes are tolerated, and only with no counterexample present: a lone
  `--- FAIL: <Target>` whose one detail is `context deadline exceeded` *and* which ran for at least
  90% of its budget, and SIGTERM/`The runner has received a shutdown signal` — the second failure
  mode the survey found, runner preemption on `FuzzOperationSequence`. A panic, a data race, a
  nested subtest failure (which is what a committed seed input failing looks like), a
  hung-worker/OOM message, a non-zero exit with no `--- FAIL:` line, and a deadline message arriving
  early are all still failures.

  The elapsed check reads `go test`'s own `--- FAIL: <Target> (60.11s)` line rather than wall clock,
  because wall clock includes compilation and would let a target that failed in 0.01s inside a slow
  build clear the floor — which is precisely the case that check exists to reject.

  `-fuzztime` is unchanged and the job stays blocking; the point was to make the gate readable, not
  to remove it. `TestFuzzSmokeScriptDistinguishesCounterexamplesFromShutdownNoise` drives the script
  against a stub `go` across ten cases, and each of its six checks was verified by mutation: deleting
  any one of them makes a named case fail. `TestCIRunsFuzzTargetsThroughTheSmokeScript` fails if the
  workflow goes back to calling `go test -fuzz` directly or drops the per-target artifact upload.

  Worth reporting upstream: the suppression should compare against the context it just read, or use
  `errors.Is(err, context.DeadlineExceeded)`. It is correct in intent and races in implementation.

[#218]: https://github.com/scttfrdmn/objectfs/issues/218

- **`docs-platform` commits a lockfile, so its dependency tree is reproducible and `npm audit` can
  run at all** ([#214]). 741 packages resolved into `docs-platform/package-lock.json`, and the
  `docs-site` job moves from `npm install` to `npm ci`. Before this the job resolved every `^x.y.z`
  range fresh on each run, which means it was gating a tree no reader could reproduce and a break
  could appear or vanish with no commit behind it.

  Two things this makes possible that were not possible before, both verified rather than assumed.
  `npm ci` into an empty `node_modules` builds the site (`build complete in 1.45s`, six pages),
  where `npm ci` against this directory previously failed outright, reporting that it can only
  install with an existing `package-lock.json`. And `npm audit` reports 0 vulnerabilities; with no
  lockfile it had no tree to audit.

  One correction to the record, because the tempting reading is wrong. This is *not* what closed the
  three `vite` alerts against this manifest — they closed earlier the same day, before the lockfile
  existed. `vite` is a direct devDependency at `^6.4.3` with an `overrides` entry forcing the same,
  and the advisories' vulnerable range is `<= 6.4.2` with `6.4.3` first patched, so every version
  the manifest admits is already patched. That is the "or pinned version requirement" half of
  Dependabot's own error message, and it is a different mechanism from the lockfile. Both are true
  here, which is exactly the situation in which one gets credited for the other.

- **The JavaScript SDK builds on TypeScript 6** ([#328], [#331]), which is the toolchain bump that
  #314 tracked as step 4 and then lost when it was closed. `typescript` ^5 → ^6, `typedoc` ^0.24 →
  ^0.28.20, `jest` and `@types/jest` ^29 → ^30, plus two `tsconfig.json` keys. `ts-jest` stays at
  ^29.1.0: its 29.4.12 already peers `<7`, so it admits 6.

  The two keys are the whole of the work, and the first one is a trap worth naming. TypeScript 6
  deprecates `moduleResolution: "node"` and raises **TS5107** for it — but TS5107 is a *config*-level
  error, so `tsc` exits before typechecking any source. It reports **1 error**, which reads like a
  nearly-clean bump. Behind it wait 64 more, all `@types/node` failing to resolve, because
  TypeScript 6 no longer auto-includes it: `Cannot find name 'process'`, `'console'`,
  `'child_process'`. Bumping `@types/node` does not help — the package is `typesVersions`-only with
  no `exports` map. `"ignoreDeprecations": "6.0"` and `"types": ["node"]` together take it to zero.

  Both keys were mutation-checked rather than assumed: removing `ignoreDeprecations` gives exactly
  the 1 TS5107, and removing `types` gives 64 errors across six diagnostic codes (29 × TS2584,
  18 × TS2591, 10 × TS2304, 4 × TS7006, 2 × TS2339, 1 × TS2503). #328 predicted 63 from a scratch
  copy; the real number here is 64, which is the sort of thing that only shows up by running it.

  `moduleResolution` deliberately does *not* move. It cannot move alone — `Node16`/`NodeNext`
  without a matching `module` is TS5110 — and moving both makes `ts-jest` warn TS151002 on every
  suite. Keeping `module: commonjs` and acknowledging the deprecation is the quieter option.

  Verified from a clean tree, since `sdk-metrics` installs with `npm ci`: `tsc --noEmit` 0 errors,
  65 tests in 4 suites, `npm run lint` 0 errors / 19 warnings. `npm run docs` was run by hand too —
  it is the one step of the four that CI does not gate, and `typedoc` moved four minors here.

  Running that ungated step found a second, unrelated thing, which is its own argument for running
  it. `TestDocumentedLinksResolve` walked the filesystem for `.md` files while its doc comment
  claimed it returned *tracked* ones, so TypeDoc's output was scanned as though the repository
  published it — and TypeDoc copies `CONTRIBUTING.md` into its media directory, where the copy's
  relative links resolve against the wrong base and fail on a link that is correct in the original.
  It now uses `git ls-files`, which inherits `.gitignore` and cannot drift from it, where the old
  skip-list could only ever name generators someone had already run. Same count of real documents
  checked (38); the two dropped files are both generated, TypeDoc's copy and a `.pytest_cache`
  artifact the old walk had also been reading. `/sdks/javascript/docs/` is gitignored to match.

- **The JavaScript SDK's lint configuration is read by the linter again, and two swallowed error
  causes are reported** ([#309]). The `eslint` 10 major could not be taken as Dependabot opened it,
  because eslint 10 does not read `package.json`'s `eslintConfig` block at all — it requires a flat
  `eslint.config.js`, and with no such file `npm run lint` lints with default rules or errors out
  depending on the invocation. Merging the version bump alone would have quietly undone the config
  fix that had just landed for the same file. So the block moved to `sdks/javascript/eslint.config.js`
  as a port rather than a rewrite: same five rules, same severities.

  Three points of the translation are worth recording, because none is visible from a version diff.
  Flat config has no `extends`, so `eslint:recommended` becomes `js.configs.recommended` and
  `plugin:@typescript-eslint/recommended` becomes a spread of `tseslint.configs.recommended` — the
  string forms are not accepted anywhere. `env: {node, es6}` was two things at once and splits into
  `languageOptions.globals` and `ecmaVersion`. And `.eslintignore` is no longer read, so `dist/**`
  has to appear in an `ignores` block or the emitted JavaScript gets linted as source.

  The bump then surfaced two genuine defects, because typescript-eslint 8 defaults
  `caughtErrors: 'all'` and so reports every discarded `catch` binding. Eight sites reported; six
  were deliberate and are now named `_error`, but two were throwing a new error while dropping the
  cause of the original. `findBinary` reported every failure of `execSync('which objectfs')` as
  "not found in PATH", which is the wrong place to look when the real cause was a permissions
  failure or an unwritable cwd; it now includes the underlying message. `prepareMountPoint` reported
  every `access` rejection as "insufficient permissions", including `ENOENT` for a mount point that
  does not exist; it now includes the errno.

  Test files are linted now, which they were not before, and that immediately found a third thing:
  `index.test.ts` carried an `eslint-disable-next-line @typescript-eslint/no-var-requires`, and
  typescript-eslint 8 renamed that rule to `no-require-imports`, so the directive had been
  suppressing nothing. Result is 0 errors and 19 warnings, all `no-explicit-any`, which the ported
  config sets to `warn` deliberately; the config was mutation-checked by introducing an unused
  binding and a formatting violation and confirming both are reported as errors.

[#309]: https://github.com/scttfrdmn/objectfs/pull/309

- **The Java SDK's OkHttp dependency names an artifact that still contains code** ([#291]).
  `okhttp.version` moves 4.12.0 → 5.4.0, and in the same commit the `artifactId` moves `okhttp` →
  `okhttp-jvm`, because OkHttp 5 went multiplatform and split the artifact: `okhttp-5.4.0.jar`
  contains **zero** `.class` files and the JVM classes are in `okhttp-jvm` (330 of them). Keeping the
  old coordinates compiles nothing and fails `src/main` with `package okhttp3 does not exist`, which
  reads like a sweeping API break rather than a renamed artifact — and `src/test` passes either way,
  since `mockwebserver` pulls `okhttp-jvm` in transitively at *test* scope. No source change was
  needed: with the right coordinates all 17 tests pass unmodified.

  Worth recording because it is a shape Dependabot cannot handle unaided: it bumps versions, and this
  release moved the coordinates, so #291 as opened could never have been correct no matter how often
  it was rebased. The `mvn -B test` step added above is what turns that from a silent breakage into a
  failing check.

- **The documentation site builds, for the first time in its history** ([#214], [#323]). `vitepress
  build` had never been run by anything, and three independent failures had accumulated behind that
  silence — each only visible once the previous one was fixed, which is why the fix that matters is
  the CI job rather than any of the three:

  1. `.vitepress/theme/index.js` imported `InteractiveExample.vue`, `PerformanceChart.vue` and
     `ConfigurationBuilder.vue`. **None of the three has ever existed** — not deleted, never written;
     `git log --all` finds no history for any of them. A missing import is a hard rollup error, so
     this alone made every page in the tree unbuildable. Two pages mounted the components anyway and
     now say what they would have shown; `PerformanceChart` was imported and used nowhere, which fits
     the hardcoded chart that was removed from `index.md` earlier in this release.
  2. `README.md` nested a ` ```python ` fence inside a ` ```markdown ` fence of the same length, so
     the inner fence *closed* the outer block and the `</CodeRunner>` below it reached the Vue
     compiler as a stray tag. It reported as `Invalid end tag` at a line and column in the middle of
     an unrelated heading, which points nowhere near the cause. The outer fence is four backticks now.
  3. `README.md` linked the license as `../LICENSE`, which leaves the VitePress site root: correct
     for a reader on GitHub, a dead link to the builder, and dead links fail the build. Anything
     outside `docs-platform/` has to be an absolute URL, and the file now says so. (Written out
     rather than quoted as markdown, because `TestDocumentedLinksResolve` reads a quoted link as a
     real one and this entry would otherwise fail the test it is describing a fix for.)

  Each fix was confirmed by reverting it against the otherwise-fixed tree and watching the build fail
  with that specific error — `Could not resolve "../components/PerformanceChart.vue"`, `Invalid end
  tag`, `Found dead link ./../LICENSE` — rather than by trusting that three green fixes explain a red
  build. The new `docs-site` job runs `npx vitepress build .`; it uses `npm install` rather than
  `npm ci` because this directory still commits no lockfile, which is the rest of [#214] and the
  reason its npm security updates cannot apply.

- **`github/codeql-action` moves to v4** ([#323]), pinned to the floating major like every other
  action in this repository. Dependabot proposed `@v4.37.4`, and `v4.37.5` had already shipped by the
  time it was reviewed — a patch pin on three SARIF uploaders defers every fix to the next merge and
  buys nothing, since `upload-sarif`'s inputs are unchanged across the major and v4's breaking changes
  are all in the analysis actions. The one action pinned exactly, `trivy-action`, is exact only
  because it publishes no major ref, and it needed a comment to say so; this one now carries the
  inverse note.

- **Two comments in `.github/dependabot.yml` that this release made false** ([#328]). They said the
  `sdk-metrics` job runs `npm install && npm test`, and that "nothing in CI runs `tsc`" — both true
  when written, neither true once the `npm ci`, build and lint steps above landed. These comments are
  load-bearing rather than decorative: they are what explains why the `typescript-toolchain` group
  exists and stops the next person from ungrouping it into bumps that cannot install. The same block
  now also records what the TypeScript 6 bump needs, measured rather than predicted, because one
  finding reads as its own opposite: `moduleResolution: "node"` raises `TS5107` under 6.0, and that
  is a *config-level* error, so `tsc` exits before typechecking any source and reports 1 error while
  63 wait behind it. The 63 are `@types/node` no longer being auto-included; `"types": ["node"]`
  takes them to zero.

- **The JavaScript SDK compiles, and two of its five configuration presets no longer fail their own
  validation** ([#314], [#325]). `npx tsc` reported 48 errors across seven files, and had done since
  before the SDK's first release; `npm run build` had therefore never succeeded and `dist/` had never
  existed. The reason it went unnoticed for that long is structural and is fixed separately below:
  `npm test` runs jest, and ts-jest only typechecks the files a test imports, so `src/mount.ts` — which
  no test reaches — was compiled by nothing at all, and `src/index.ts` could ship
  `export { StorageAdapter } from './storage'`, a name that has never existed in that module.

  Three of the errors were real defects rather than annotations:

  - `Configuration`'s constructor merged with a one-level object spread —
    `{ s3: {...defaults, ...cfg?.storage?.s3}, ...cfg?.storage }` — which puts the caller's whole
    `storage` last and discards the `s3` it had just merged. So every preset silently lost the
    defaults it did not itself name, and two could not pass `validate()` at all:
    `fromPreset('cost-optimized')` produced an `S3Config` with **no region**, and
    `fromPreset('cluster')` set `tlsEnabled: true`, which `validate()` rejects without certificate
    paths a preset cannot know. Both threw on a preset the SDK ships. Replaced with a shared deep
    merge; the constructor and `merge()` now take `DeepPartial<Configuration>`, which is what the
    preset table always meant — `Partial<T>` is one level deep, and the eleven TS2739 "is missing the
    following properties" errors were reporting exactly that. `cluster` no longer presets
    `tlsEnabled`, since the caller is the one holding the certificates.
  - `MountManager.isMounted` tested `/proc/mounts` for `fstype === 'fuse'` exactly, and ObjectFS
    mounts report `fuse.s3` — `internal/fuse/mount.go` sets `Subtype: "s3"`. Only the
    `device.includes('objectfs')` fallback was saving it. Both `isMounted` and `listMounts` now accept
    the `fuse.*` form.
  - `createClient(configPath, options)` built `{...options, config}` with `config` possibly
    `undefined`, which *sets* the key and so overrode a configuration the caller had passed in
    `options`. It now omits the key instead, which is both type-correct under
    `exactOptionalPropertyTypes` and what a caller passing both would expect.

  `npm run lint` also ran for the first time: `eslintConfig.extends` said
  `"@typescript-eslint/recommended"` without the required `plugin:` prefix, so eslint exited with
  "couldn't find the config to extend from" before reading a line of source. Zero errors once it
  could run, with 23 pre-existing `no-explicit-any` warnings left standing.

- **The Java SDK compiles, and its 17 tests run** ([#325]). `mvn compile` failed with four errors and
  had presumably always done, because nothing in this repository has ever invoked Maven — the same
  structural gap that let 48 `tsc` errors ship in the JavaScript SDK, found by going looking for it
  in the third SDK rather than by anyone reporting it:

  - `ObjectFSClient` imported `com.fasterxml.jackson.datatype.jsr310.JavaTimeModule` and registers it
    in its constructor, but `pom.xml` declared only `jackson-databind` — so
    `package com.fasterxml.jackson.datatype.jsr310 does not exist`. `jackson-datatype-jsr310` is now
    declared, on the same `${jackson.version}`.
  - Two `catch (NotFoundException | ObjectFSException e)` clauses, which javac rejects:
    *"Alternatives in a multi-catch statement cannot be related by subclassing."* `NotFoundException`
    extends `ObjectFSException`, so catching the supertype alone is both legal and exactly equivalent.

  With it building, the tests ran for the first time and one failed — a real expectation error, not a
  flake. `list_includesPrefixAndLimitParams` asserted the request path contains `prefix=my/prefix`,
  and okhttp percent-encodes `/` in a query *value*, so it carries `prefix=my%2Fprefix`. Verified by
  probing okhttp directly rather than by reading its documentation. The implementation was right; the
  test now asserts the encoded form **and** that the server decodes it back to `my/prefix`, so it
  still means "the prefix arrives intact" rather than "okhttp escapes slashes."

  The compiler plugin also moves from `source`+`target` 17 to `release` 17, which is what its own
  build warning asked for on every run: compiling on a newer JDK with `-source`/`-target` accepts
  calls to APIs that did not exist in 17, producing a jar that compiles clean and throws
  `NoSuchMethodError` on the version it claims to target.

- **A maximum-length S3 key came back from the C SDK one byte short** ([#325]). `objectfs_info_t.key`
  was `char[1024]` and an S3 key may be 1024 bytes when UTF-8 encoded, so the array could hold 1023 of
  them plus the terminator and `fillInfo` truncated the rest without returning an error. It is now
  `char[1025]`.

  The consequence is worst in `objectfs_list`, where the key is the *result* rather than something the
  caller passed in and so cannot be compared against anything: a truncated key names a different
  object or none at all, and handing it back to `objectfs_get` or `objectfs_delete` acts on the wrong
  key. Reachable with an ordinary long key, not only a hostile one.

  `fillInfo`'s capacity arguments were the literals `1024`/`128`/`128`; they now derive from the arrays
  themselves with `unsafe.Sizeof`, because widening the declaration alone would have left the one
  function that writes to it still truncating at 1023 — a declaration and its only writer silently
  disagreeing, which is the shape of the original bug.

- **`objectfs_last_error` leaked memory on every call for a freed handle** ([#325]). `objectfs.h`
  documents the returned pointer as *"valid until the next call on the same handle. Do NOT free it"*,
  which makes it the library's allocation — but the freed-or-never-issued arm returned
  `C.CString("invalid or freed handle")`, a fresh `malloc` each time. The caller must not free it and
  the library never did, so every call leaked. Verified by calling it three times and printing the
  pointers: three distinct addresses, now one. Error reporting is the path a program takes when it is
  already going wrong, frequently inside a retry loop.

- **The C SDK's `cache_bytes` argument silently lost up to half of the requested size** ([#325]).
  `objectfs.h` documents it as "memory cache size in bytes", and `bytesToCacheString` rendered it by
  integer-dividing by 1 GiB or 1 MiB and formatting the quotient — so 1.5 GiB became `"1GB"` and
  2047 MiB became `"1GB"`, losing 1023 MiB. It now emits a bare byte count, which `utils.ParseBytes`
  multiplies by 1 and therefore round-trips exactly. Invisible from C: nothing echoes the size back,
  and a cache half the requested size still works, just with a worse hit rate than was provisioned.

- **The JavaScript SDK's entry point exports the error its own client throws, and reports the right
  license** ([#325]). Two defects found by carrying the Python fixes back across:

  - `src/index.ts` re-exported 7 of the 11 classes `src/errors.ts` declares. `CacheError` was one of
    the four missing, and `clearCache`/`warmCache` throw it — so a caller could not name it in a
    `catch` and had no way to distinguish that failure from any other. All eleven are exported now.
  - `export const LICENSE = 'MIT'` — ObjectFS is Apache-2.0, as `package.json`'s own `license` field,
    the repository `LICENSE`, and every source header say. A consumer reading that constant to build
    an attribution list got a wrong answer from the SDK's public API. `src/index.test.ts` now asserts
    `LICENSE` and `VERSION` against `package.json`, so the two cannot drift apart again.

- **The Python SDK's `cluster` preset passes its own validation, and six of its exceptions can be
  imported** ([#325]). Two defects, both found the same way as their JavaScript counterparts:

  - `Configuration.from_preset('cluster')` set `security.tls_enabled = True`, and
    `SecurityConfig.validate()` requires `tls_cert_path` and `tls_key_path` whenever TLS is on — paths
    a preset cannot know. So one of the five shipped presets raised
    `ConfigurationError: TLS certificate and key paths required` on any caller that validated it, and
    `objectfs-python config generate --preset cluster` was one. The preset no longer sets the flag;
    enabling TLS is the caller's, with the paths, and the `merge()` call that does it is written out
    next to the line that used to set it. This was found by a test that loops over the preset names
    rather than by inspection — a hand-written probe of two presets would have missed it, which is
    exactly what happened on the JavaScript side before the same loop went in.
  - `from objectfs import NetworkError` was an `ImportError`. `NetworkError`, `CacheError`,
    `AuthenticationError`, `AuthorizationError`, `TimeoutError` and `ValidationError` were declared in
    `objectfs/exceptions.py` and re-exported by nothing, so the README's own error-handling example —
    which imports `NetworkError` from the package — could not run. All six are now in `__init__.py` and
    `__all__`. `CacheError` matters twice over: it is what `clear_cache` and `warm_cache` now raise, so
    a caller has to be able to name it in an `except`.

  A third, smaller one: both `StorageAdapter` and `ObjectFSClient` wrapped every exception from the
  layer below in `StorageError(f"Failed to <verb>: {e}")`, including the `StorageError`s the adapter
  raises deliberately. A caller saw `Failed to list objects: Failed to list objects: <the actual
  reason>` — and the CLI prints that string. Each of the nine handlers now re-raises `StorageError`
  unchanged before its generic arm, so an unsupported scheme, a malformed URI, or the not-implemented
  notice and its issue link arrive as themselves.

- **Dependabot auto-merge, cause five: GitHub Actions cannot approve pull requests** ([#305]). Fixing
  [#288] made `.github/dependabot.yml` valid, Dependabot re-evaluated it within minutes, and the
  `automerge` label arrived on all 14 PRs it then opened — the label plumbing works. And still nothing
  merged, because the step finally *ran* and failed: *"GitHub Actions is not permitted to approve pull
  requests"* (`can_approve_pull_request_reviews` is false at the repository level). The step body was
  `gh pr review --approve` followed by `gh pr merge --auto --squash`, under `bash -e` — so the failing
  first line aborted before the line that does the work.

  The approval was never needed. Branch protection on `main` requires status checks and has
  `required_pull_request_reviews: null`, so nothing waits on a review. The line is deleted rather than
  the permission enabled: allowing Actions to approve would let *every* workflow in the repository
  satisfy a review requirement, to buy something no rule asks for.

  That makes five independent causes for one symptom — 46 PRs opened, 0 merged — and the shape of it is
  the part worth keeping. Causes 1 and 4 both left the `if:` condition false, so the step was *skipped*
  and cause 5 could not produce a symptom at all; it became visible minutes after the config fix
  landed. All five are now enumerated in the workflow header, in the order they were found, with why
  each hid the next. A fix that should have worked and didn't usually means another cause, not a wrong
  diagnosis.

[#305]: https://github.com/scttfrdmn/objectfs/issues/305

- **Two JavaScript SDK dev-dependency bumps could not install, because Dependabot proposed half of a
  peer-coupled pair** ([#306]). `npm` enforces peer ranges, so these fail at `npm install` rather than
  at test time — the `sdk-metrics` job's `ERESOLVE`, not a test failure:

  - `typescript` 5.9.3 → 7.0.2 alone: `peer typescript@">=4.3 <7" from ts-jest@29.4.12`
  - `@typescript-eslint/eslint-plugin` 5.62.0 → 8.65.0 alone: the plugin wants
    `peer @typescript-eslint/parser@"^8.66.0"` while `parser` is still pinned `^5.59.0`

  Both are the right target version proposed in an order that cannot resolve. `groups:` on the
  `/sdks/javascript` npm entry now moves each set in one PR, which is the only form that installs. It
  does not make them automatic — they are still majors, so `dependabot-automerge.yml` comments and
  waits for a human, which is right for a jump of this size. (The TypeScript half of it turned out to
  need a third package and a lower target; see the `typedoc` entry below.)

  The `gomod` entry has had groups all along; neither npm entry did, which is why this surfaced only on
  the JavaScript side — and only now, since before [#288] the whole config was ignored and these were
  never proposed at all. `/docs-platform` was checked and is not affected: it has `jest` and
  `typescript` but no `ts-jest`, so nothing there declares a peer range on the TypeScript version.

  Recorded on [#306] while looking: the three `vite` alerts need a **`vitepress` major**, not a `vite`
  bump. `vitepress@1.6.4` depends on `vite: ^5.4.14`, so `vite` cannot reach the patched 6.4.3 while
  `vitepress` is on 1.x — the missing lockfile ([#214]) is a second blocker, not the only one.

[#306]: https://github.com/scttfrdmn/objectfs/issues/306
[#214]: https://github.com/scttfrdmn/objectfs/issues/214

- **`typedoc` joins the `typescript-toolchain` Dependabot group; it constrains the TypeScript version
  as tightly as `ts-jest` and was missed** ([#314]). The regrouped PR from [#306] still failed
  `npm install`, which is the useful part: a group is only as good as its membership, and `typedoc`
  gates the same range without being an obvious member of a "TypeScript toolchain."

  Two things fell out of measuring the actual peer ranges rather than assuming them. `typedoc@0.24`
  peers `typescript@"4.6.x || … || 5.1.x"`, so `typescript: ^5.0.0` in `package.json` has been
  resolving to **5.1.6** — three minors behind what the range reads as, and a 2023 compiler. And
  **TypeScript 7 is not reachable at any grouping**: `ts-jest`'s latest release peers `<7` and has
  nothing above it, so the ceiling is TypeScript 6, which `typedoc@0.28` permits. Verified by
  installing the four-package set, not by reading the manifests.

  [#314] records what this was hiding, which is larger than the grouping: `npm run build` in
  `sdks/javascript` fails with **48 `tsc` errors**, and nothing in CI or the Makefile runs `tsc` at
  all. Two of the 48 name identifiers that do not exist — `Configuration` in `types.ts:308` and a
  `StorageAdapter` re-export in `index.ts:32` where `storage.ts` exports `S3StorageAdapter` — both
  dating to the commit that added the SDK in 2025-08. `sdk-metrics` passes because `ts-jest`
  typechecks only the one test file and its imports, so the rest of the SDK is checked by nothing.

[#314]: https://github.com/scttfrdmn/objectfs/issues/314

- **Dependabot's `gomod` and `maven` queues were saturated, so outdated dependencies were silent
  rather than current.** `open-pull-requests-limit` caps discovery, not just display: once the slots
  are full Dependabot stops proposing, and there is nothing in the PR list to distinguish "nothing to
  update" from "no room to say so." This is the same condition that hid three outdated GitHub Actions
  majors behind [#254]-[#258] until #304 bumped them by hand.

  Measured rather than assumed: `go list -u -m all` reported **twelve** outdated direct modules
  against five slots, with `prometheus/client_golang`, `redis/go-redis`, `substrate` and
  `golang.org/x/sys` absent from every open PR — not current, just unproposed. `sdks/java/pom.xml`
  declares nine artifacts across seven version properties against three slots, and the three in
  flight left `slf4j`, `junit`, `mockito` and `maven-compiler-plugin` with no way to be raised.

  `gomod` goes 5 → 8 and `maven` 3 → 6, with the reasoning recorded in the config so a future
  reduction is a deliberate choice. Both npm limits are left at 3 on purpose: those queues are full
  of majors correctly waiting for a human, which is the limit doing its job rather than hiding work.

- **Three cache TTL tests were flaky in a way that reported as a cache defect.** They configured a
  100 ms TTL and then asserted that an entry exists "immediately after `Put`". But `Put` stamps
  `Timestamp: now` on entry (`internal/cache/persistent.go:305`) and *then* gzip-encodes and writes to
  disk, so the entry's clock starts before its bytes land. Any delay between `Put` returning and the
  following `Get` — GC, scheduler contention on a shared runner, a slow temp filesystem — spends the
  same 100 ms the assertion depends on.

  `TestPersistentCache_TTLExpiration` failed on CI for a pull request that changed nothing but
  `.github/dependabot.yml`, which is the tell. Reproduced deliberately by sleeping 120 ms after the
  `Put`: the entry is gone before it is ever read. `TestPersistentCache_Optimize` was the more
  fragile of the two — it asserts a final count of 1, so it needed a fourth `Put` *and* a full index
  sweep to finish inside the window, and losing that race looks like `Optimize` evicting a fresh
  entry. `TestLRUCache_TTLExpiration` has the same shape; being in-memory it is far less likely to
  miss, which makes it the one that would have outlived the other two.

  The tests are what changed, not the caches. TTL measured from the start of the write is the
  defensible semantic — an entry is as old as its data — so the fix is a 2 s budget wide enough that
  scheduling noise cannot reach it, plus an `expiryWait` helper that keeps runtime tied to the TTL
  rather than to a second hardcoded sleep. Each of the three was confirmed to fail against a
  `isExpired() { return false }` mutant before being called fixed.

- **`.github/dependabot.yml` was invalid, so none of it applied** ([#288]). `schedule.time: 09:00` was
  unquoted, and Dependabot's YAML 1.1 parser reads that as a sexagesimal integer where its schema
  requires a string: *"The property '#/updates/0/schedule/time' of type integer did not match the
  following type: string"*, once for each of the six ecosystem entries. An invalid configuration is not
  partially applied — it is ignored — so every `labels:`, `groups:`, `reviewers:` and `ignore:` block in
  the file was dead and Dependabot ran on its defaults. True since 2025-10-15, when the key was
  introduced unquoted.

  This is also why the earlier `automerge` fix never took effect. That label was added to
  `.github/labels.yml` and to the repository because Dependabot silently drops labels it cannot find
  and every approve/merge step in `dependabot-automerge.yml` is gated on it — and still no Dependabot
  PR carried it, because `labels:` sat inside a file being discarded wholesale. Two independent causes
  for one symptom, and fixing the visible one left the other invisible. Confirmed on the open PRs:
  #210, #249, #254 and #258 all arrived with `dependencies` plus an ecosystem label and no `automerge`.

  Guarded by `TestDependabotScheduleTimesAreQuoted`, which checks the **file text** rather than the
  decoded value. That is deliberate and was arrived at by mutation: `gopkg.in/yaml.v2` and
  `gopkg.in/yaml.v3` both decode `09:00`, `9:00`, `10:30` and `05:00` as Go `string`, because Go's YAML
  1.2 core schema has no sexagesimal integer type. So a decode-based assertion passes on exactly the
  input that breaks the file, and the first version of this test did.

[#288]: https://github.com/scttfrdmn/objectfs/issues/288

- **`min_sequential` now means what it says: read-ahead had two thresholds over the same counter, and
  only one was configurable** ([#247]). The prefetch gate required `sequentialHits >= MinSequential`
  *and* `confidence > 0.5`, where confidence was assigned `sequentialHits / 10.0` a few lines above — so
  the second condition was `sequentialHits > 5`, the same quantity in different units, and the stricter
  always won. The effective gate was `max(MinSequential, 6)`: setting 1, 2, 3, 4 or 5 all prefetched
  first on the sixth sequential read, and the shipped default is 3, so the documented default did not
  describe the default behavior. Above 6 the floor was redundant, meaning the setting worked as
  documented in exactly the upper part of its range.

  The confidence floor is gone rather than the defaults being raised to 6, and the measurement is why. A
  continuing traversal costs the same either way: 3 MiB read sequentially at the kernel's 128 KiB buffer
  transfers exactly 3,145,728 bytes at every `MinSequential` from 1 to 10, because `FileSystem.fetch`
  shares one flight between a prefetch and the read it anticipates, so a prefetch that is going to be
  read adds no bytes at all. What prefetching sooner actually costs is a run that *stops* before
  consuming what was fetched — and that cost is exactly one prefetch, not a growing tail: a reader that
  goes sequential and then walks away wastes 131,072 bytes whether it stops after three reads at
  `min_sequential: 3` or after six at `min_sequential: 6`. The floor never prevented a class of waste,
  it only moved the point at which the single unredeemed prefetch became possible. So the default
  configuration now prefetches from the third sequential read instead of the sixth, for one prefetch's
  worth of exposure on abandoned runs.

  The `confidence` field is deleted rather than left computed-and-unread: it was read at exactly one
  place, the gate, and a score derived from the threshold it guards is not independent evidence and
  cannot become any. The paragraph in `examples/config.yaml` explaining why the number did not mean what
  it said is deleted too, which is the test of whether this was actually fixed rather than documented.

  The gate test now drives both sides of the threshold — nothing on the read before it, a prefetch on
  the read at it. It previously read six times at `min_sequential: 3` and asserted a prefetch, with a
  comment explaining that six was required; so it documented the defect instead of catching it, and
  would have passed identically if `MinSequential` were ignored altogether. A gate test that only drives
  the satisfying case cannot tell a threshold of 3 from a threshold of 6.

- **A three-node cluster could not form at the default gossip packet size, and said the cluster secret
  was wrong** ([#277]). `MaxGossipPacket` defaulted to 1024 bytes. A sealed sync message costs about 200
  bytes of envelope plus about 415 bytes per member — measured, not estimated, because `seal` wraps the
  payload in JSON with a hex MAC and base64-encodes the byte field, so the payload-to-datagram ratio is
  not a constant one can reason to. 1024 bytes therefore held **two** members. A third node's sync was
  truncated by the receive buffer, the truncated JSON failed the authentication envelope parse, and the
  failure was counted and logged as `MessagesUnauthenticated` — whose documented meaning is a peer
  configured with a different cluster secret. So the symptom of a size limit was a message sending the
  operator to verify a secret that was correct, and membership stalled at two nodes.

  Three separable defects, each fixed on its own terms rather than by raising one number:

  The default is now 8192 bytes, which carries about nineteen members in a single datagram. Not 65507,
  the maximum UDP payload: a datagram over the path MTU is fragmented by IP and one lost fragment loses
  the whole datagram, so the effective loss rate rises with size. 8 KiB does exceed a 1500-byte MTU and
  will fragment — a deliberate trade, since six fragments for a sync that runs once per join is worth
  more than chunking every small cluster's membership across several round trips. Per-round alive
  messages are about 483 bytes sealed and never fragment.

  Truncation is now detected and reported as truncation. The receive buffer is sized `MaxGossipPacket+1`,
  because a buffer sized exactly to the limit cannot distinguish "at the limit" from "truncated" —
  `ReadFromUDP` discards an oversize datagram's tail and reports only what it copied, so with one spare
  byte `n > MaxGossipPacket` is unambiguous evidence. Such a datagram is dropped before it reaches the
  authenticator, counted as `MessagesTruncated`, and logged with the limit and the instruction to set
  the same value on every node. An oversize *send* is likewise refused after sealing, counted as
  `MessagesOversize`, and returns an error naming the size and the limit — the two directions are
  counted separately because the operator action differs: an oversize send is this node's memberlist
  outgrowing its own limit, while a truncated receive is a peer whose limit is larger than ours.

  Sync messages are now chunked rather than unbounded, so a memberlist larger than one datagram is sent
  as several. Each chunk is a complete `SyncMessage`; this is sound because `handleSyncMessage` merges
  per node rather than replacing wholesale, so a receiver applying two chunks reaches the same state as
  one applying their union. Chunk boundaries are computed over a sorted key order so they are a function
  of the membership rather than of Go's map iteration order, and each candidate chunk is sealed to
  measure it, so the budget includes the envelope, the MAC, and the message ID. A single member too
  large for any datagram is emitted alone and logged, so `sendMessage` refuses it loudly rather than a
  member being silently dropped.

  A fourth defect was found along the way: `NewClusterManager`'s nil-config literal restated sixteen
  fields that `applyConfigDefaults` sets to the same values two lines later, including a second
  `MaxGossipPacket: 1024`. That literal is now one field — `CacheReplication`, the only one it was ever
  load-bearing for, because it is a bool defaulting to true and a bool's zero value cannot be
  distinguished from unset. A duplicated default is how a stale copy survives a fix to the default.

  The `-tags=distributed` suite no longer overrides `MaxGossipPacket` anywhere, which is what makes it
  cover this: a test that sets the value cannot notice the default regressing. That suite now passes
  unaided, and the unit test for the default asserts that a three-member sync fits in one datagram —
  the property the issue was about — rather than asserting that a constant equals itself.

- `MountManager.Wait` and the FUSE serving goroutine read `m.server` without holding the mutex that
  `Unmount` writes it under. Both reads are also nil dereferences: if the unmount lands first,
  `m.server.Wait()` panics on a background goroutine, which is unrecoverable and takes the process
  down together with the mount — unmounting under every open file descriptor. The serving goroutine
  now waits on the server value already in scope, and `Wait` reads the field into a local under the
  lock before dereferencing it; the lock is not held across the wait itself, which would deadlock
  every real unmount. Caught by CI's race detector on a documentation-only pull request, so it was
  live on `main`, and it reproduces only on Linux — on darwin the racing goroutine needs a real
  macFUSE mount to start at all. The regression test drives both sides directly rather than through
  a mount, so it fails deterministically instead of once in many runs. ([#267])

- **`internal/distributed`'s package documentation described consistency guarantees the code does not
  provide** ([#169]). The Strong Consistency section claimed "Linearizable operations across cluster";
  what `executeStrongConsistency` does is fan N identical PUTs of the same bytes at the same key and
  succeed on a majority, which is a signal that most nodes could reach S3. Session Consistency
  claimed read-your-writes, which comes from reading through the write buffer — `internal/vfs` does
  that per descriptor and this package is not involved.

  Rewritten to say what each level does rather than what it is named after, with the structural reason
  they cannot do more stated once: every node writes the same key in the same bucket, so S3 holds the
  single copy and "replicate to the other nodes" means issuing the same PUT again. The levels differ
  in how many redundant requests are issued and whether the caller waits. The package warning also
  no longer points at "integration tests pending Sprint 4 (LocalStack)" — this project does not use
  LocalStack, and the sprint it named is long past; it now names the specific gap, which is that the
  consensus engine elects leaders and replicates nothing.

- **`StrategyConsistentHash` was not consistent, and could not have been** ([#131]).
  `selectConsistentHash` was `return nodes[:count]` under a comment saying a real ring belonged there,
  and its node slice is built by ranging a map — so the same key reached a different node on each
  call, which is the one property consistent hashing exists to provide. A per-node cache keyed on an
  assignment that changes per call cannot hit.

  It is now a rendezvous (highest-random-weight) ring in `internal/distributed/hashring`: score every
  node against the key, take the highest. Removing a node moves only the keys that node owned —
  measured at about 1/n, and asserted with that bound — rather than remapping everything. Lookup is
  O(n) in nodes, which the benchmark settles rather than argues: 1555 ns at 100 nodes and 125 ns at 8,
  zero allocations, three orders of magnitude below the S3 round trip it precedes, so a sorted ring
  with virtual nodes would be complexity spent on nothing.

  The fix needed a signature change the issue did not mention: `LoadBalancer.SelectNodes` never
  received the operation key, so no implementation behind it could have hashed by key. It now takes
  one, which makes the key mandatory at all five call sites instead of something a strategy could
  silently do without. `selectTargetNodes` also sorts its alive-node slice, because the ring sorts
  internally but round-robin and the least-load tiebreak index positionally — for those, the order the
  map happened to be iterated in *was* the decision. A keyless operation (a list with no elected
  leader, a batch) falls back to round-robin rather than hashing the empty string, which would
  concentrate every such operation on one node.

  The existing test for this strategy asserted only the returned count, and its own comment described
  the defect — "returns the requested number of nodes from the front of the list" — as the intended
  behavior. A count assertion cannot tell a hash ring from a slice prefix, so it now asserts that
  permuting the input does not change the output, which the prefix fails.

- **A node marked dead by one lost heartbeat could never rejoin, and every node's gossiped statistics
  were frozen at the first value ever received** ([#272]). Both follow from one omission: incarnation
  numbers were set to 1 at construction and nothing ever incremented them. An incarnation is how a
  SWIM-style protocol lets an accused node contradict a false report of its own failure, and every
  comparison that depends on one is strictly-greater — so with the number fixed at 1, all of them were
  false for every message after the one that discovered a node.

  What that cost is not a degraded metric, it is a lost node. `checkSuspicions` promotes suspect to
  dead and nothing demoted it; the only path back to alive was `handleJoinMessage`, reachable solely
  by a process restart. So a transient network problem — a few dropped datagrams, which is the normal
  condition of UDP — removed a healthy node from routing permanently. Proven by execution before
  fixing: a node discovered at state 0 was still at state 2 after an alive message from it arrived.

  A node now refutes an accusation about itself rather than recording it, publishing an incarnation
  above the accused one, which supersedes that accusation everywhere it has spread. Refusing to record
  it matters as much as the refutation: `performGossip` and `broadcastMessage` both skip nodes that are
  not alive, so a node that accepted a suspicion about itself would stop gossiping and make the report
  true. The new number is one past the *higher* of ours and theirs, not ours plus one, or a peer
  holding a stale higher incarnation would keep rejecting the refutation and the disagreement would
  never resolve; an accusation naming an incarnation already superseded is ignored, which is what makes
  the exchange converge instead of ring. Sync messages are answered too, since a full sync is often
  where a node learns it was written off during a partition. `GossipStats.SuspicionRefutations` counts
  these, because a cluster refuting steadily is healthy and misconfigured — a heartbeat interval below
  the network's real latency — not failing.

  The frozen statistics were the same guard seen from the other side. An incarnation orders claims
  about whether a node is alive; it says nothing about the freshness of the load and cache figures
  riding along with the claim, and a healthy node never raises its incarnation because it has nothing
  to refute. So gating the payload on a strict increase pinned every peer's stats to the moment it was
  discovered. An alive message at an equal incarnation now refreshes the payload without touching the
  state machine — only for a node already believed alive, so the refresh cannot become a second,
  unguarded path back from dead. `performGossip` also stamps its own `LastSeen` before announcing
  itself; it had been broadcasting the timestamp set at construction, and since `UpdateNodeInfo` copies
  that field and `performHealthChecks` compares it against three heartbeat intervals, every alive
  message a node sent was evidence it had not been heard from. Inert only while the guard discarded the
  payload, and a flap the moment it stopped.

  The acceptance test [#132] proposes for the stats half — wait two heartbeats, assert `MemoryUsage >
  0` — **passes on this bug**, because the first message is applied and the value stays non-zero
  forever after. The tests here assert that a *changed* value arrives on a second message, and that
  dead becomes alive; a single-transition assertion cannot distinguish a live feed from a permanently
  stale one, and a green test over a stale metric is worse than no test because it certifies what it
  cannot see. Nine mutations were checked, each reverting one part of the fix, and each fails a test
  that names the consequence.

- **Every node advertised itself as idle with an empty cache, for the life of the process** ([#132]).
  The six resource and cache fields on the local `NodeInfo` were set at construction and never written
  again, so the figures a peer received described a node that had just started and never done anything.
  Four of them now carry a measurement, taken once per gossip round and travelling with the alive
  message that immediately follows: heap in use as a fraction of heap obtained, and cache size, hit
  rate, and operation count from sources this package already had. Once per round rather than on a
  ticker of its own, because the only consumer is that message — and because `ReadMemStats` stops the
  world, so its frequency should be bounded by something.

  Three premises in the issue turned out not to hold, and the resulting scope is smaller than it asked
  for. There is no new message type, because the alive message already carries the full `NodeInfo` —
  the transport was never missing. `StrategyLeastLoad` was not reading these fields at all; it reads
  `LoadBalancerStats.NodeLoad`, which is populated, so the strategy was not broken in the way the
  issue described. And `CPUUsage` cannot be `HeapInuse/HeapSys` as proposed, because that is the same
  expression assigned to `MemoryUsage` — two fields holding one quantity under different names.

  So `CPUUsage`, `DiskUsage`, and `NetworkBandwidth` are left at zero, with the reason recorded on the
  fields and pinned by a test. Each needs a platform-specific source that is not in this repository:
  `/proc/stat` or `host_statistics`, `statfs` against a cache directory this package does not know
  about, interface counters sampled over an interval. A field filled with a proxy from an unrelated
  quantity is worse than an empty one — an obviously-zero `CPUUsage` prompts someone to implement it,
  while one carrying heap fragmentation looks like a measurement and gets used as one.

  Also fixed: `calculateClusterStats` summed a `totalCacheSize` and discarded it with `_ =`. The value
  was correct and simply never assigned anywhere, so `ClusterStats` reported an average hit rate with no
  way to say how much was cached. It is now a field, summed across alive nodes only — a dead node's
  cache is not reachable, so counting it would overstate what the cluster can serve without going to S3.

  This work was only observable because [#272] landed first: with incarnations frozen, a receiving node
  discarded every stats update after the first, so a live feed and a permanently stale one were
  indistinguishable from the outside.

- **A distributed operation that failed on every node was counted as a success** ([#269]). The three
  consistency executors each reported failure in `OperationResult.Success` and returned a nil error,
  and `ClusterManager.DistributeOperation` classified on the error — the ordinary thing to do in Go —
  so `SuccessfulOps` climbed for operations that did nothing. The counters are what an operator reads
  to decide whether coordination is working, and a cluster where every operation fails while the
  success count rises is worse than one with no metrics: it actively argues against investigating.

  `ExecuteOperation` now returns an error whenever the result is unsuccessful, carrying the failure
  text so the two representations cannot drift. Reconciled at that one choke point rather than in each
  executor, because it is the single point every consistency level passes through: a fourth level added
  later cannot reintroduce the disagreement, and no executor has to remember to report the same fact
  twice.

  The `-tags=distributed` suite that surfaced this was itself asserting against a misconfiguration —
  it never injected a backend, so every operation failed with `no backend configured` and
  `TestConcurrentOperations` printed "Failed 10 out of 10" and "Successful: 10, Failed: 0" in the same
  run. Those tests now run against a real S3 backend on an in-process substrate endpoint, seed the keys
  they read, and check the returned bytes rather than only the success flag, so the assertion covers a
  real operation. No CI job builds that tag, which is why the contradiction sat there unobserved — see
  [#240].

- **A single-node cluster never elected a leader** ([#275]). The majority check lived only in
  `handleVoteResponse`, which runs when a peer replies to a vote request. `startElection` casts this
  node's vote for itself and sets the count to 1 — already a majority of one — but nothing evaluated
  it, so with no peers the election timeout fired again, the term incremented, and the cycle repeated
  for the life of the process while `IsLeader` stayed false. A cluster of one is not a corner case: it
  is the first thing anyone runs, and the shape a deployment has while its second node is being
  provisioned.

  The count-and-check is now `checkVoteMajority`, called from both places where the vote count changes.
  Extracting it rather than duplicating the comparison is the point — the defect was that a check
  existed in one of the two places it belonged, and two copies would drift the same way. The
  candidate-state guard moved with it, so a vote arriving after a higher-term heartbeat has demoted
  this node cannot promote a follower on a stale count.

  A membership view of one is only a majority if this node really is the whole cluster, so the check
  now distinguishes the two cases that view cannot: a node whose `seed_nodes` names an address other
  than its own waits to hear from a peer, and a node that names none proceeds. Without that, a probe of
  three seeded nodes showed all three electing themselves at term 1, each on its own single vote, within
  the first second — before any had heard of the others. Gossip learns of a peer only from an inbound
  message and the election timer does not wait for one, so every node of a starting cluster spends its
  first few hundred milliseconds looking exactly like a cluster of one.

- **A deposed leader went on reporting itself as the leader** ([#275]). `becomeLeader` calls
  `SetLeader`; no step-down path did. So `ClusterManager.IsLeader` was effectively write-once — a node
  that saw a higher term became a follower inside the consensus engine while the accessor that callers
  actually ask, and that the coordinator routes on, kept returning true for the life of the process.
  That is what turned a momentary election race into a permanent two-leader state instead of something
  the next heartbeat resolved. Both step-down paths now inform the cluster manager: a heartbeat names
  the new leader, and a vote request at a higher term clears leadership without naming one, because a
  candidate has not won anything yet and claiming it had would be worse than admitting there is no
  leader.

- **`GetStats` reported zero nodes for the first five seconds after `Start`** ([#275]). The cluster
  statistics were computed only by a five-second ticker, so `GetStats().TotalNodes` was 0 while
  `GetNodes()` over the same membership correctly returned 1 — two public accessors disagreeing, with
  the one an operator, a health check, and a readiness probe reads being the wrong one. They are now
  computed once during `Start`, so they are current when it returns. The regression test asserts with
  no sleep, which is the whole of the test: one that slept first could not tell the fix from the
  defect.

  These were found by probe while fixing [#269], and they are why three tests in the
  `-tags=distributed` suite had been failing unobserved — no CI job builds that tag ([#240]).

- **Two more `-tags=distributed` tests were asserting against misconfigurations**, the same class as the
  missing backend above. `TestMultiNodeCluster` set no `SeedNodes`, and gossip has no other discovery
  mechanism — it learns of a peer from an inbound message, and the only unsolicited send is
  `joinCluster`, which runs only when seeds are configured. So its three managers were three clusters of
  one that had never heard of each other, which is what "Node *i* sees 1 nodes in cluster" three times
  was saying. It now seeds all three from node 0 and gives each a backend, because strong consistency
  fans the write out. It also raised `MaxGossipPacket` past the default that would otherwise truncate
  the sync — a workaround, since removed once the default itself was fixed ([#277]).
  `TestLoadBalancer_NodeSelection` registered four peers at addresses nothing listens on and
  then asserted a strong-consistency PUT across two of them succeeded; it timed out for 30 seconds and
  reported that as a defect. It now asserts what its name promises — that selection chooses from the
  whole membership, and that the operation executes and the bytes reach storage — and says in a comment
  why one reachable node out of five can carry one of those claims per operation but not both.

- **Four data races on the membership maps, all one mistake made four times** ([#278]). `maps.Copy` on a
  `map[string]*T` copies the pointers, so taking the lock, copying the map, dropping the lock and then
  reading through the copy is not synchronization — it reads the same structs the gossip receive
  goroutine is writing. `ClusterManager.calculateClusterStats` did this with `cm.nodes` against
  `UpdateNodeInfo`, and `GossipProtocol.sendSyncMessage` did it with `gp.memberlist`, where this node's
  own entry aliases `gp.localNode` itself. The same shape in pointer-slice form was in `performGossip`
  and `broadcastMessage`, which collected `*GossipNode` and read `.Info.Address` after unlocking. The
  fix in each case removes the aliasing rather than locking around it: tally, marshal, or resolve the
  address inside the critical section, and carry out only values. `GetNodes` and `GetMemberlist` had
  always done it correctly, twenty lines away in the same files.

  The consequence was not merely a stale read. `calculateClusterStats` classifies a node by `Status`
  and, in the same iteration, sums `CacheSize` and `CacheHitRate` from it — so a node counted alive on
  a torn read contributes figures from a different moment into the number a size-aware balancer routes
  on. And `handleSyncMessage` decides whether to merge an update by comparing incarnations, so an
  inconsistently-marshaled memberlist decides whether membership converges at all.

  The two regression tests are race tests, not assertion tests: each drives a gossip round and an
  inbound alive message concurrently, and under `-race` fails on the defect and passes after it.
  Without `-race` both pass either way, which is stated in their doc comments so nobody later reads a
  green run without the flag as coverage.

- **A three-node cluster now forms, elects a single leader, and does it race-free**, which is the thing
  all of the above adds up to and which had never been observed. Verified by probe: membership converges
  to 3 on every node and leadership settles on exactly one, stably, rather than the two or three
  simultaneous leaders that the single-node fix alone produced. `TestMultiNodeCluster` now passes under
  `-race`; the whole `-tags=distributed` suite is green for the first time.

[#128]: https://github.com/scttfrdmn/objectfs/issues/128
[#130]: https://github.com/scttfrdmn/objectfs/issues/130
[#131]: https://github.com/scttfrdmn/objectfs/issues/131
[#132]: https://github.com/scttfrdmn/objectfs/issues/132
[#133]: https://github.com/scttfrdmn/objectfs/issues/133
[#150]: https://github.com/scttfrdmn/objectfs/issues/150
[#151]: https://github.com/scttfrdmn/objectfs/issues/151
[#169]: https://github.com/scttfrdmn/objectfs/issues/169
[#282]: https://github.com/scttfrdmn/objectfs/issues/282
[#283]: https://github.com/scttfrdmn/objectfs/issues/283
[#284]: https://github.com/scttfrdmn/objectfs/issues/284
[#285]: https://github.com/scttfrdmn/objectfs/issues/285
[#267]: https://github.com/scttfrdmn/objectfs/issues/267
[#269]: https://github.com/scttfrdmn/objectfs/issues/269
[#272]: https://github.com/scttfrdmn/objectfs/issues/272
[#275]: https://github.com/scttfrdmn/objectfs/issues/275
[#277]: https://github.com/scttfrdmn/objectfs/issues/277
[#278]: https://github.com/scttfrdmn/objectfs/issues/278
[#291]: https://github.com/scttfrdmn/objectfs/pull/291
[#323]: https://github.com/scttfrdmn/objectfs/pull/323
[#325]: https://github.com/scttfrdmn/objectfs/issues/325
[#328]: https://github.com/scttfrdmn/objectfs/issues/328
[#331]: https://github.com/scttfrdmn/objectfs/pull/331
[scttfrdmn/substrate#540]: https://github.com/scttfrdmn/substrate/issues/540

### Changed

- **The JavaScript SDK's fabricated operations throw instead of returning invented data, and its
  README documents what the code does** ([#325]). Ten methods reported success for work they never
  performed. The worst wrote to disk: `S3StorageAdapter.downloadObject` did
  `fs.writeFile(localPath, 'Simulated file content from S3')`, called `progressCallback(30, 30)`, and
  returned `30` — so a caller following the README's own example, which passed
  `/tmp/downloaded-file.txt`, **destroyed whatever was at that path and was told the transfer
  succeeded.** `listObjects` returned two invented objects, `getObjectInfo` a fixed size and etag for
  any key whether or not it existed, and `uploadObject`/`deleteObject` `true` for transfers and
  deletions that never happened. On the client, `joinCluster`/`leaveCluster` returned `true` without
  contacting a node, `getClusterStatus` reported a healthy single-node cluster for any configuration
  without querying anything, and `clearCache`/`warmCache` reported success for every path given.

  All of them now throw a typed error naming [#325], following the precedent already set in this
  repository by `getPerformanceStats`, `internal/distributed/coordinator.go` and
  `internal/fuse/filesystem.go`'s EROFS stubs. This SDK has no S3 client and no control-plane client;
  until it has one, use the AWS SDK directly or mount the bucket, which is what ObjectFS is for. The
  README's feature list, storage section, cluster example, API reference and event list are corrected
  to match — it had been advertising "AWS S3 deep integration with intelligent tiering and cost
  management" and "built-in support for distributed clusters and replication" against code that did
  neither.

- **The Python SDK's fabricated operations raise instead of returning invented data** ([#325]), which
  closes the other half of the issue. The same ten operations were fabricated here, in the same
  shapes, and `StorageAdapter._download_s3_object` did the same damage —
  `open(local_path, 'wb').write(b"Simulated file content from S3")`, `progress_callback(30, 30)`,
  `return 30`. Proven by execution before it was touched: run against a file containing other text, it
  printed `returned 30 bytes; file now contains: 'Simulated file content from S3'`. The raise now
  happens before anything opens `local_path`, and a test writes `REAL USER DATA` to a real file and
  asserts it is still there afterwards.

  There were fifteen fabricating methods, not five, because the GCS and Azure backends were copies:
  `_list_gcs_objects`, `_list_azure_objects` and their eight siblings each called the S3 method, so a
  `gs://` or `az://` URI returned the same two invented *S3* objects under a docstring describing a
  "simplified implementation" of a different cloud. There is now one set of methods with the other ten
  names aliased onto it at class level, so the three schemes reach one honest raise rather than three
  copies of a fabrication. The scheme dispatch stays, because rejecting an unsupported scheme is a
  real thing to do.

  On the client, `join_cluster`/`leave_cluster`/`get_cluster_status` raise `DistributedError` and
  `clear_cache`/`warm_cache` raise `CacheError`, following `get_performance_stats`, which was already
  written this way in the same file. `objectfs-python storage list|download|upload` still exist so
  that they fail naming the reason rather than as an unrecognized-argument error, but `--help` now
  says `NOT IMPLEMENTED` on all three; it previously described them as working, and the README showed
  `storage download s3://my-bucket file.txt ./local-file.txt` as an example, which overwrote
  `./local-file.txt` and printed `Successfully downloaded 30 bytes`.

- **Every GitHub Action is on its current major, and CI no longer downloads a Go toolchain mid-build.**
  Eight actions moved: `setup-go` 5 → 7, `setup-node` 4 → 7, `setup-python` 5 → 7,
  `docker/metadata-action` 5 → 6, `docker/setup-buildx-action` 3 → 4, `docker/build-push-action`
  5 → 7, `docker/login-action` 3 → 4, `dependabot/fetch-metadata` 2 → 3. Five of these Dependabot had
  proposed and five is also `open-pull-requests-limit`, so the other three were never proposed at all —
  the queue was saturated, which is a thing to watch for rather than a thing to trust.

  The `setup-go` bump changes real behavior, so it was traced rather than assumed. v5 read the `go`
  line and installed go1.26.0; Go's own toolchain switching then fetched go1.26.5 on the first build,
  because `toolchain go1.26.5` says to. The right compiler was used, but it arrived at build time,
  over the network, once per job — the `tar: ... gotoolchain_local.txt: Cannot open: File exists`
  warnings in every job log were that download racing the module-cache restore. v6 reads the
  `toolchain` directive directly and exports `GOTOOLCHAIN=local`, so 1.26.5 is installed up front and
  nothing switches. Same compiler, one fewer moving part, and the log noise is gone. The tradeoff is
  recorded next to the directive in `go.mod`: under `GOTOOLCHAIN=local` a version setup-go cannot
  install now fails the build instead of silently falling back.

  Checked for each of the others rather than taken on the release notes: the deprecated `config`,
  `config-inline` and `install` inputs that buildx v4 removed are not used here; neither are the
  `DOCKER_BUILD_NO_SUMMARY`/`DOCKER_BUILD_EXPORT_RETENTION_DAYS` envs dropped by build-push-action v7
  or the `pip-install` input dropped by setup-python v7; and the setup-node v6 change limiting
  automatic caching to npm cannot apply, because no `cache:` input is set on the Node or Python steps.
  All eight now require Actions runner ≥ v2.327.1 for their Node 24 runtime, which `ubuntu-latest`
  satisfies.

  Node itself went 20 → 22 in the `sdk-metrics` job while it was open: 20 reached end of life in
  April 2026, and `sdks/javascript` declares `node >=16`, so nothing was holding the pin down except
  never having revisited it. ([#254], [#255], [#256], [#257], [#258])

[#254]: https://github.com/scttfrdmn/objectfs/pull/254
[#255]: https://github.com/scttfrdmn/objectfs/pull/255
[#256]: https://github.com/scttfrdmn/objectfs/pull/256
[#257]: https://github.com/scttfrdmn/objectfs/pull/257
[#258]: https://github.com/scttfrdmn/objectfs/pull/258

- Updated five direct dependencies: `aws-sdk-go-v2` 1.41.5 → 1.43.2, `aws-sdk-go-v2/config`
  1.31.12 → 1.32.33, `klauspost/compress` 1.18.7 → 1.19.1, `pierrec/lz4/v4` 4.1.22 → 4.1.27, and
  `redis/go-redis/v9` 9.18.0 → 9.21.0. Two consequences are worth naming rather than leaving in the
  lockfile: the AWS SDK pulled `smithy-go` to 1.27.5, which clears the ≥1.26.0 floor the real
  Pricing API work needs ([#183]), and the SDK restructured SSO authentication — `internal/ini` is
  gone and `service/signin` is new. Full suite green under `-race`, coverage gate and lint clean.
  ([#249], [#250], [#251], [#252], [#253])

[#183]: https://github.com/scttfrdmn/objectfs/issues/183
[#249]: https://github.com/scttfrdmn/objectfs/pull/249
[#250]: https://github.com/scttfrdmn/objectfs/pull/250
[#251]: https://github.com/scttfrdmn/objectfs/pull/251
[#252]: https://github.com/scttfrdmn/objectfs/pull/252
[#253]: https://github.com/scttfrdmn/objectfs/pull/253

## [0.11.0] - 2026-08-03

POSIX completeness and write-path safety: the operations a user reaches for first — `rm`, `rmdir`,
`mv`, `chmod` — do the thing rather than returning an error or, worse, succeeding without acting. All
28 issues on the milestone closed before this tag was cut.

Two themes run through it. The first is that a configuration key should select something: nine
blocks that a loader had never read now reach the code they name, and the pricing region — read at
exactly one line, and only to label a summary — now picks the prices, so a mount in `sa-east-1` no
longer reports us-east-1's rates under its own region's name. The second is that a fix is paired
with the mechanism that fails if it recurs, because most of what this release corrects was invisible
to the compiler, to vet, and to lint: a config key nothing reads, a cost figure nothing checks, a
gate that cannot fail.

This is the first release cut from the merge commit that closes its milestone, which is the rule
v0.10.3 established after two tags shipped without the work they were named for.

### Added

- **`rm` and `rmdir` work.** `Unlink` and `Rmdir` delete the object rather than returning EROFS
  ([#163]). The stub they replace was itself a fix — go-fuse defaults an unimplemented
  `NodeUnlinker` to *success*, so before it `rm` exited 0, the kernel dropped the inode, and the
  object stayed in the bucket billing with no path that reached it. Three details are load-bearing
  and are pinned by tests:
  - Deleting a file discards whatever the write path still holds for it. `echo x > f; rm f` is
    ordinary and the kernel does not guarantee a flush before the unlink, so a surviving dirty range
    would be PUT back by the next flush or by the unmount — the file returning from the dead at its
    written size, with no error anywhere.
  - `rmdir` refuses a non-empty directory with ENOTEMPTY. S3 has no directories, so removing a
    prefix's marker object while objects remain under it would succeed at the storage layer and leave
    every one of them present, billing, and unreachable through the filesystem.
  - A missing file is ENOENT, not success. The backend's `DeleteObject` no-ops a key that is not
    there — S3's contract — so absence is checked explicitly, and it is checked against both the
    bucket and the write path: `Create` records attributes without a PUT, so a just-created file is
    real and visible to `stat` with no object behind it yet.

- **`mv` works, and the README says exactly how far short of POSIX it falls.** `Rename` copies
  server-side and then deletes, per object; renaming a directory moves everything under its prefix
  ([#164]). Before this, go-fuse's default for an absent `NodeRenamer` answered every `mv` with
  `ENOTSUP`. Six properties are load-bearing, each pinned by a test that was verified by mutation —
  the implementation was broken in that specific way and the test watched to fail:
  - **Each source object is deleted only after its own copy has succeeded.** An interruption
    therefore leaves the data at the old name, the new name, or both — never at neither. Duplicated
    data is an operator's cleanup problem; missing data is not recoverable, so the ordering is fixed
    in that direction deliberately, and a partial directory move is resumable by re-running the same
    `mv`.
  - **The copy is server-side.** Reading and rewriting through the process would make renaming a
    10 GiB file cost 20 GiB of transfer, and renaming a directory that times the object count.
    Objects above S3's 5 GiB single-part `CopyObject` limit route through `UploadPartCopy`.
  - **The write path is flushed before the copy runs.** A copy acts on objects, so a file whose only
    content is dirty ranges in memory is invisible to it: `echo hi > a; mv a b` would have copied a
    key that did not exist yet, deleted the source, and then flushed the pending ranges back to the
    *old* name — the file landing at neither name the user asked for.
  - **The moved node is repointed, recursively for a directory.** `go-fuse`'s `MvChild` re-parents the
    *same* inode, so the `FileNode` survives a rename holding the path it was constructed with. A
    stored path is stale the moment a rename succeeds, and the consequence is silent in a way nothing
    would catch: after `mv a b`, a write to `b` flushed to `a` and recreated the source the rename
    had just deleted. A stale key is a valid key, so every S3 call succeeds.
  - **Prefix matching is on a path boundary.** `mv dir dir-new` must not move `dir2/file`. That is not
    a hypothetical spelling mistake — it is the same defect the cache's `keyMatches` had, found in the
    same audit.
  - **`renameat2`'s `RENAME_EXCHANGE` and `RENAME_NOREPLACE` are refused with `EINVAL`, not
    approximated.** Both are atomicity promises copy-then-delete cannot keep, `EINVAL` is what the
    kernel and libc expect for an unsupported flag, and `mv` and Git fall back correctly on it. A
    foreign `newParent` is `EXDEV`, which is what go-fuse's own `LoopbackNode` answers.

  There is no atomic alternative to reach for. S3's `RenameObject`, added in 2026, is
  directory-bucket (S3 Express) only, and object annotations — which ObjectFS needs for POSIX
  attributes — are unsupported on directory buckets, so the two features are mutually exclusive. The
  README now carries a **Rename is not atomic** section stating what a concurrent reader can observe,
  what an interruption leaves behind, and which tools that breaks: anything relying on the
  write-temp-then-rename idiom for atomic replacement is not safe here between concurrent writers.
- `types.Backend.CopyObject`, a server-side copy that preserves content encoding, content type,
  storage class, and user metadata. Each of the four is a requirement rather than a nicety, and the
  interface documentation says which failure each one prevents: the read path dispatches decoding on
  the stored `Content-Encoding` and fails closed on one it cannot handle, so dropping it would leave a
  compressed object permanently unreadable with its bytes intact; the storage-class default is
  `STANDARD`, so dropping it would silently promote the object out of the tier being paid for; and
  POSIX mode, ownership, and mtime live in user metadata and nowhere else, so dropping it would reset
  a file's permissions, which is not a thing `rename` does. Encryption is applied from configuration
  rather than copied from the source, so a key rotation reaches renamed objects.
- `internal/testaws`: a `DirectoryMarkerDelete` capability probe, and `RequireDirectoryMarkerDelete`
  to skip on its absence. Deleting a `dir/` marker while `dir/child` still exists panics the substrate
  emulator, because its object store is a filesystem abstraction where `dir/` is the *directory*
  holding the child, and removing it orphans the child (filed as
  [scttfrdmn/substrate#534](https://github.com/scttfrdmn/substrate/issues/534), reproduced against
  afero alone with no substrate involved). In S3 a key is an opaque string and `dir/` is an ordinary
  object, so this is the emulator's property and not ObjectFS's — which is the reason for a runtime
  probe rather than an assertion either way. The alternative was to assert the emulator's behavior as
  expected, which would encode a dependency's bug as this project's contract.
- `internal/storage/s3/copy_live_test.go` (`-tags=integration`) covers the multipart copy path against
  real AWS, at real part sizes. It has no hermetic equivalent: substrate dispatches on
  `x-amz-copy-source` before checking `uploadId`, so it answers an `UploadPartCopy` as a whole-object
  copy with a 200 (substrate#532). That leaves the branch where a mistake is most expensive resting on
  nothing but reading — abandoned multipart parts are billed and invisible to `ListObjects`. Two
  shapes, both deliberate: a legal three-part copy with a short final part, which is where
  inclusive/exclusive range mistakes surface, and an *illegal* one that S3 rejects at
  `CompleteMultipartUpload`, which is how the abort path gets a failure arriving after every part has
  already uploaded.

- **A `fuse:` section in the configuration file, and it is the first one any loader has read**
  ([#180]). `direct_io`, `keep_cache`, and `sync_read`. Nine fields on `internal/fuse.MountOptions` and
  `internal/fuse.Config` carried yaml tags for a whole release and were decoded by nothing, because
  `config.Configuration` had no `fuse` key at all — so a `fuse:` block in a config file was silently
  discarded, and the two flags that reach the kernel per-open were settable in Go and returned as the
  literal `0` from every `Open`. All three new fields default to false, false is the kernel's own
  behavior for each, and `NewDefault` therefore names no `fuse` section: the zero value *is* the
  default, and a second place for it to live is how the last set drifted.

  Each of the four seams between the YAML key and the value the kernel receives is now asserted by a
  test verified by mutation — the mapping was deleted or reverted to v0.10.0's code and the intended
  test watched to fail. That is the point of the change rather than a detail of it: every one of those
  nine fields was correct at the layer that declared it, and died at a boundary no test crossed. The
  adapter mapping in particular passed its package's whole suite with the three assignment lines
  removed, which is why it was extracted into a method that can be called without a mount.

  What the flags do to the *kernel* — whether a second `read(2)` at the same offset reaches the
  filesystem, whether cached pages survive `open(2)` — cannot be observed without `/dev/fuse`, so it
  lives behind a `fuse_mount` build tag with a `make test-fuse-mount` target. CI compiles the tag it
  cannot run, because a build tag nothing compiles is how four others in this repo came to carry code
  that does not build ([#240]). Those tests fail rather than skip when the device is absent: a test
  that skips itself reports success.

  Two of the four flags [#180] nominated are **not** plumbable, and both reasons are recorded at the
  field that would have carried each rather than being dropped in silence. Splice: go-fuse only splices
  a `ReadResult` backed by a file descriptor, and this filesystem's reads come from S3 or from memory
  and return `fuse.ReadResultData` at every return site, so `DisableSplice` would disable a path never
  taken — a config key whose effect is provably nothing. The writeback cache: it maps to
  `ExplicitDataCacheControl`, which makes the filesystem responsible for invalidating the kernel's data
  cache, and there is not one `NotifyContent`, `NotifyEntry`, or `NotifyInvalInode` call in the
  repository — enabling it would convert bounded staleness into permanent staleness.

- **Subcommands: `objectfs mount`, `objectfs unmount`, `objectfs version`, `objectfs help`**
  ([#134]). `unmount` is spelled both ways, since `umount` is what a decade of muscle memory types.

  `objectfs unmount /mnt/s3` is the one that did not exist before and had to. Unmounting was
  previously "signal the mount process", which a systemd unit's `ExecStop` cannot do once that
  process is already gone, so the shipped unit called `fusermount3 -u` directly — a program that is
  absent on a minimal image and spelled `fusermount` on libfuse 2, and whose failure in either case
  reaches systemd as a bare exit status. The subcommand tries the libfuse 3 helper, the libfuse 2
  helper, `umount`, and finally `umount(2)`, and when none works it reports which ran, which were not
  installed, and the `lsof +D` invocation that names whatever is holding the mount open. None of the
  candidates unmounts lazily or forcibly, and a test asserts that no candidate ever passes `-z`,
  `-l`, or `-f`: those detach the name while the filesystem keeps serving open files, so they report
  a finished unmount with writes in flight — and adding one would make every other unmount test pass,
  which is why the prohibition is a test rather than a comment.

  **The form without a subcommand still works and is not deprecated.** It is what every invocation
  written before this release looks like, including the ones in scripts nobody will revisit. A first
  argument carrying a URI scheme or a leading dash routes to `mount`; a bare word that is not a
  command is a usage error naming itself, so `objectfs moutn s3://b /mnt` does not become an attempt
  to mount a bucket called `moutn`.

  Flags now come before positionals, because Go's `flag` package stops parsing at the first non-flag
  argument — `objectfs mount s3://b /mnt --foreground` left `--foreground` as a third positional and
  silently did not apply it. Each subcommand gets its own `FlagSet` for the same reason:
  `flag.CommandLine` cannot parse a flag that appears after a positional at all.

  New: `--mount-point`, so a mount point can come from a flag instead of a positional, and
  `--foreground`, which names what already happens — ObjectFS does not fork, and the flag exists
  because init systems and scripts pass it and refusing it would break invocations that are correct
  about the behaviour. Exit codes are now defined: `0` succeeded, `1` the command was right and the
  operation failed, `2` the command line was wrong and nothing was attempted.

  `main()` is three lines around `run(args, stdout, stderr) int`, which is what makes any of this
  testable: it previously called `log.Fatalf` directly, and `log.Fatalf` calls `os.Exit`, which takes
  the test binary with it. `cmd/objectfs` therefore has a coverage floor for the first time (77%),
  replacing a note in `.coverage-floors` that recorded the package as untestable.

- **The systemd template unit mounts and unmounts the way the binary actually works** ([#135]).
  `configs/systemd/objectfs@.service` now runs
  `objectfs mount --config /etc/objectfs/%i.yaml --mount-point /mnt/objectfs/%i --foreground` and
  stops with `objectfs unmount /mnt/objectfs/%i`. What it replaces was valid systemd and wrong in
  four ways: `ExecStart=... s3://%i /mnt/objectfs/%i` made the instance name and the bucket name one
  string, which fails for a prefix, for two mounts of one bucket, or for a bucket whose name is not a
  legal unit instance; `ExecStop=/bin/fusermount3 -u` is the single-helper call described above;
  `Restart=always` remounted a filesystem after a clean `systemctl stop`; and `RequiresMountsFor` on
  the unit's own mount point asked systemd to wait for the mount this unit creates. `TimeoutStopSec`
  is now stated rather than inherited, because that is the flush window — SIGTERM makes the mount
  process unmount, which writes buffered ranges to S3, and too short a value there is a SIGKILL
  through buffered data.

  Two gates, checking different things. `TestSystemdUnit*` in `internal/config` parses the unit's
  `Exec*` lines through the same parser the documentation gate uses and checks every subcommand and
  flag against the sets scraped from `cmd/objectfs/main.go` — so a flag renamed in the binary breaks
  the unit's test, and no list is maintained by hand. A `systemd-unit` CI job additionally runs
  `systemd-analyze verify` on `objectfs@example.service` for the half a Go test cannot check. Neither
  alone would have caught the old unit: `systemd-analyze` passes it, and a Go test cannot tell whether
  `RequiresMountsFor` means what its author thought.

  Found while writing the Go half: joining `\` continuations is load-bearing. A loop over raw lines
  stops at `ExecStart=... \` and skips everything after it, so `--mount-point` and `--foreground` went
  unchecked — verified by mutation, changing `--mount-point` to `--mountpoint` left the test passing.

- **Region-aware S3 pricing, generated from AWS's published price list rather than typed in** ([#161]).
  `internal/awsrates` now holds 36 regions × 8 storage classes × 6 rates, produced by
  `go generate ./internal/awsrates/...` from the public per-region offer files. Those files need **no
  credentials**, so anyone can refresh every number in one command, and the accessors are
  `ForRegion(region, class)` and `AllForRegion(region)` — with the us-east-1 forms kept for callers
  comparing tiers, where only the ratio matters.

  Nothing on the mount path fetches anything: the table is compiled in, so pricing a tier needs no
  network and cannot fail a filesystem operation. That constraint is what makes the whole approach
  usable, and a test clears every AWS credential environment variable and asserts it holds.

  The offer file, not the Pricing API, is the source. The API needs credentials and would pull
  `aws-sdk-go-v2/service/pricing` into the module, whose transitive requirement moves `smithy-go`
  under the S3 client that serves every read and write — dependency risk on the data path in order to
  price a tier. The offer files also avoid three traps the API presents, each verified against live
  data rather than assumed: `productFamily` is absent from 315 of us-east-1's 381 S3 products, so
  filtering on it silently drops SKUs; filtering Deep Archive storage by `volumeType` returns a
  **staging SKU at 21× the real rate**; and `us-west-2` is not a pricing endpoint, but the SDK's
  resolver templates any well-formed region into an opaque DNS failure rather than saying so.

- **`internal/awsrates/offerfile`, the extraction rules as ordinary tested Go rather than a script
  someone ran once.** Every rule in it exists because the obvious version returns a plausible number
  from the wrong SKU, and each has a test named for the case that forced it:
  - **The region's usagetype prefix is derived, never assumed.** `USE1`, `USW2`, `APS8` are not region
    codes and have no published mapping, so the prefix is recovered structurally from the Standard
    storage product. us-east-1's prefix is the **empty string**, which is a case in its own right: a
    derivation bug returning `""` everywhere looks correct there, and us-east-1 is the default region
    and the fallback for every unknown one.
  - **Suffixes match exactly, never with `strings.HasSuffix`.** `Tables-`, `Annotation-`, `Files-` and
    `Vectors-TimedStorage-ByteHrs` all end in the Standard storage usagetype and all cost more. Found
    by mutation: swapping the exact comparison for a suffix match survived the entire suite, because
    the Standard query is shielded by its own `volumeType` clause. A probe of all 27 lookups found the
    single query where the exact match is load-bearing — Intelligent-Tiering storage, where
    `Tables-TimedStorage-INT-FA-ByteHrs` sits at $0.0265 against the correct $0.023 — and that is now
    the case the test asserts.
  - **An ambiguous query is an error, not a coin flip.** Where two SKUs on one query publish different
    prices at the same band, extraction fails and names both SKUs, rather than returning whichever the
    map iteration reached.
  - **Egress comes from the `AWSDataTransfer` file, keyed on `fromLocation`.** S3's own
    `DataTransfer-Out-Bytes` usagetype is the Multi-Region Access Point routing charge, not internet
    egress, and the transfer file publishes a $0.00 free-tier SKU on the same four attributes as the
    real one — so taking the lowest match prices every byte leaving the region as free.

  The package went from no tests to 89.9%, `internal/awsrates` from 76.2% to 100%, and the generator
  from no tests to 74.4%. `internal/awsrates/offerfile/offertest` builds the fixtures all three suites
  share, so a rule is stated once rather than transcribed per suite.

### Fixed

- **The compressed-upload bypass is pinned by a test that asserts the routing, not just its result**
  ([#153]). The corruption itself was fixed in 0.10.1 — a compressed object no longer goes through the
  CargoShip transporter, which cannot set `Content-Encoding` — but the test covering it asserted only
  that the stored object carried the header. That is the property users need and it is one step removed
  from the mechanism: it would also pass if the transporter had acquired header support, and it would
  keep passing if the bypass were replaced by anything else that happened to produce a correct object.

  The new test asserts which upload path ran, using the `cargoship-created-by` metadata the transporter
  stamps on everything it uploads. It has a control half that is equally load-bearing: a 1 KiB object,
  below the compression threshold, **must** carry the stamp. Without that half the test would pass on a
  build where the transporter never runs at all, silently measuring a disabled feature instead of the
  bypass. Both halves were verified by mutation — removing the bypass fails the assertion, and disabling
  CargoShip fails the control.

  Filed upstream as [scttfrdmn/cargoship#353]: `Archive` has no field that maps to `Content-Encoding`,
  and neither transporter sets the header, still true in v0.20.0. `CompressionType` looks like the field
  and is not — `buildMetadata` puts it in user metadata. Until that lands, ObjectFS gives up CargoShip's
  throughput for exactly the objects that compressed.

- **Two gosec findings the security check reported but `lint` did not.** There are two gosec runs in
  CI reading different suppression directives: golangci-lint's honors `//nolint:gosec`, while the
  standalone gosec whose SARIF becomes GitHub code scanning honors only `#nosec`. Both sites already
  carried a reasoned `//nolint`, so `lint` passed at 0 issues and the `gosec` check failed with two new
  alerts. Neither finding is real — the generator writes committed Go source holding published list
  prices, and the unmount helper spawns no shell, takes its program from a fixed platform table, and
  passes the mount point as one argv element — so both now carry `#nosec` alongside, with a note that
  the duplication is about two tools rather than two risks. Verified by installing the same gosec the
  workflow uses, reproducing both findings, and confirming an unsuppressed `0o644` write in the same
  function is still reported, so the suppression is line-scoped rather than file-wide. Six other sites
  have the same gap and are open code-scanning alerts today; filed as [#264] rather than swept, since
  each needs its own judgment about whether the finding is real.

- **`pricing.region` selected nothing, so every cost figure was us-east-1's, labeled with whatever
  region the operator configured** ([#161]). The rates lived in a map built at package init, and package
  init cannot see a configuration. `PricingConfig.Region` was read at exactly one line in
  `internal/storage/s3` — a summary field — while every number came from the region-blind map, and the
  assertion covering it compared a rate to itself, so it passed for eleven releases.

  That is worse than an unlabelled figure. `region: sa-east-1` above us-east-1 prices reads as correct,
  and sa-east-1 storage is **76% more expensive** than us-east-1's — an operator sizing a deployment
  there was reading a number 43% below what they would be billed. The spread across the fleet is
  material in both directions: Standard runs $0.0225/GB-month in ap-east-2, $0.023 in us-east-1 and
  us-west-2, $0.0245 in eu-central-1, and $0.0405 in sa-east-1.

  Rates are now generated for **36 regions × 8 storage classes × 6 fields** from AWS's public price
  list offer files, and `PricingManager` resolves each lookup through the configured region. A region
  with no published table falls back to us-east-1 and *says so* — one warning at construction naming
  the configured region, the region actually used, and what to do about it, rather than one per object
  access. `PricingSummary` now carries both, so a cost report cannot label us-east-1's numbers with a
  region that produced none of them.

  `StorageTierInfo.CostPerGBMonth` is gone rather than corrected; see **Removed**.

- **Glacier's PUT price was the price of thawing an object, 67% too high.** `Requests-Tier3` at
  $0.00005 is `RestoreObject`; a Glacier PUT is `Requests-GLACIER-Tier1` with `operation: PutObject` at
  $0.00003. The cause is worth recording because it is not arithmetic: **`usagetype` is not a unique
  key for an AWS rate.** `Requests-Tier3` carries three SKUs at two prices, `Standard-Retrieval-Bytes`
  two at two, and `Requests-GLACIER-Tier1` fifteen at two, separated only by the `operation` attribute.
  A query that omits it returns whichever price Go's map iteration reached first — a wrong number that
  changes between runs.

  The integration test that existed to catch exactly this agreed with the defect, because it spelled
  the same query a second time by hand. Two transcriptions of one intent check each other, not the
  intent. It now drives its queries from the single place that defines them and compares the whole
  committed table against a fresh extraction, field by field.

- **A bucket name one character long reported "is 1 characters".** `s3://b` is what someone types
  while testing, so the singular arm is a message operators read, and a grammatical error in an error
  message reads as a message nobody has looked at. The test now asserts both arms of the sentence
  rather than the substring after them.

- **The read-ahead trim is covered by tests rather than by luck.** `inflightFetches.unclaimedStart`
  and the arm of `performPrefetch` that drops a prefetch whose whole range is already in flight had no
  test of their own. Both are reached only when a read is outstanding at the instant a prefetch is
  scheduled, so an idle machine ran them by accident and a loaded one did not: `internal/fuse` measured
  67.5% alone and 66.4% under `go test ./...`, and the coverage gate failed on a commit that touched
  neither file. No behavior changed here — the point is that a branch nothing owns is a branch a
  refactor can delete in silence, and this one prevents a sequential read from paying for the same
  bytes twice.

  The drop arm is asserted with an explicit timeout rather than a byte count, which is what removing it
  actually does: a prefetch trimmed to a non-positive length waits on the very read it was trimmed
  against, so the failure is a parked prefetch worker, not an over-large GET. With every worker parked
  the read-ahead stops entirely and nothing reports it.

- **A data race between `ConsensusEngine.Stop` and an inbound heartbeat.** `Stop` read
  `ce.electionTimer` without holding `ce.mu` while `resetElectionTimer` was replacing it from the
  gossip receiver goroutine, which is where an `AppendEntries` RPC is handled. Neither shutdown signal
  ordered the two: the receiver does not watch the consensus engine's `stopCh`, and although
  `ClusterManager.Stop` stops gossip first, `GossipProtocol.Stop` closes the socket without waiting
  for the receiver, so a message already inside a handler keeps running. `Start`'s unlocked call to the
  same function was the second instance. The regression test drives `handleNetworkAppendEntries`
  concurrently with `Stop` rather than over UDP, because the two tests that caught this in CI hit it
  only when a heartbeat happened to land inside `Stop`'s window — reproducible under CI's load and not
  locally, which is the flake shape a `-race` gate is worst at.
- **Changing `compression.algorithm` no longer orphans every object already in the bucket.** A mount
  now decodes any algorithm ObjectFS can write, chosen from the object's stored `Content-Encoding`
  rather than from the configuration ([#230]). Before this, `Compressor` held exactly one codec and
  `Decompress` compared the stored encoding against that codec's token, so a mount could read back
  only what it was currently configured to write. Switching `zstd` to `lz4` made every existing zstd
  object unreadable — and so did setting `enabled: false`, which is how an operator turns compression
  off after deciding the read amplification was not worth it. Turning compression off stops new
  objects being compressed; it does not make the existing ones uncompressed, and it was the change
  most likely to be made and least likely to be expected to break anything.

  Nobody got wrong bytes: the read failed closed with a `DATA_CORRUPTION` error, because
  `checkFullyDecoded` cross-checks the decoded length against the recorded `objectfs-original-size`.
  That guard was compensating for a dispatch that could have succeeded — every codec was already
  linked into the same binary. The decoder table is built from
  `pkg/compression.SupportedAlgorithms` rather than listed by hand, so an algorithm added there is
  readable without a second edit; that derivation is what stops the defect's actual shape, which was
  a set of encoders and a set of decoders maintained independently. Pinned by the full
  write-algorithm × read-configuration matrix, including a disabled reader, and the fail-closed
  behavior still holds for the cases no dispatch can help: a `Content-Encoding` naming a coding
  ObjectFS does not implement, and a header stripped after the write by a `CopyObject` or a tier
  transition. A body its own declared codec rejects is now reported as non-retryable corruption
  rather than a bare error the retry layer would take at face value.

- **A mount on `STANDARD_IA`, `ONEZONE_IA`, or `GLACIER_IR` could not create anything at all.**
  `mkdir` and `touch` both failed, and so did writing any file smaller than 128 KiB ([#154]). AWS's
  per-tier minimum object size is a *billing* floor — S3 stores a zero-byte `STANDARD_IA` object and
  bills it as 128 KiB — but `TierValidator.ValidateWrite` enforced it as though S3 would reject the
  write, and it is called before anything else in `PutObject`. Both of the ways this filesystem
  brings a name into existence go under that floor: `Mkdir` writes a zero-byte marker object so an
  empty directory is distinguishable from a prefix that never existed, and a `Create` followed by a
  small write flushes a small object. So the three tiers most of the cost documentation recommends
  were the three a filesystem could not be used on, and an IA-tier integration test could not get
  past its own setup.

  It is a warning now, naming the size written alongside the size that will be billed — which is the
  actionable fact, since a tier that bills every object as 128 KiB is more expensive than `STANDARD`
  for a workload of small files, and nothing downstream would have mentioned it. What still refuses
  a write is `tier_constraints.min_object_size`: an operator who sets that has asked for a floor that
  is not AWS's, and a policy someone chose is the only kind worth enforcing. Note the consequence of
  the split, which is tested rather than left to be rediscovered — setting that key to the tier's own
  published minimum reinstates exactly the old gate, zero-byte directory markers included.

  The gate is enforced two layers below the operation that trips it, so it is pinned at both: the
  validator's own tests assert a zero-byte write is accepted *and* that the billing warning carries
  both numbers, and a test in `internal/fuse` drives real `Mkdir` and `Create` calls against a real
  endpoint on every tier that has a minimum, reading the tier list from `StorageTiers` so a class
  that gains one later is covered without editing the test. Only the second layer establishes that a
  `mkdir` is a zero-byte PUT, which is the step that turned a billing gate into an unusable mount.
- **`chmod` and automatic tier transitions worked on every key except the ones containing a `+`.**
  `x-amz-copy-source` is read by S3 as a URL path, and `url.PathEscape` leaves `+` as itself while S3
  decodes `+` in that header as a space — so a self-copy of `a+b.txt` asked for `a b.txt` and came
  back `404 NoSuchKey`. Both callers are operations a user expects to be invisible, so the symptom was
  a `chmod` failing with `ENOENT` on a file that plainly exists, and a storage-tier transition failing
  on a timer with nothing to attribute it to. A `+` in a filename is ordinary: version numbers, C++
  sources, and any timestamp written as `2026-08-01T00:00+00:00`. Escaping is now in one place
  (`Backend.copySource`) rather than open-coded at each call site, one of which built the header with
  no escaping at all. Verified against real S3 in `us-west-2` rather than reasoned about — both
  `url.PathEscape` and `(&url.URL{Path: …}).EscapedPath()` fail on such a key, `%2B` succeeds, and
  every other character `PathEscape` passes through (`~ * ( ) $ & = @ :`) was probed on the same
  endpoint and copies correctly.
- **`objectfs stats` reported zero for six counters that were being maintained correctly all along.**
  `GetStats` copies field by field, and its list named nine of fifteen fields: `Creates`, `Deletes`,
  and `Renames`, each incremented by its own operation, and the three latency averages, each
  maintained as an exponential moving average by `recordReadTime` and its siblings. All six were live
  and none reached the snapshot. This is a whole class of quiet defect — a field added to `Stats` and
  not added to the copy is not a compile error and not a test failure, just a number that reads zero
  forever — so the guard is a reflection test that sets every counter to a *distinct* non-zero value
  and asserts the snapshot reports each one. Distinct, so that a copy assigning the right field from
  the wrong source is caught too, which naming them all `1` would not be. An enumeration of field
  names would have had the same failure mode as the code it checks. `time.Duration`'s reflect kind is
  `Int64`, which is how the three latency fields were found.
- `README.md`: the not-implemented table still listed `unlink` and `rmdir` as `EROFS` and the
  tools-that-do-not-work list still said `mv` fails with `ENOTSUP` "because there is no rename" — both
  true when written and both false since. A row asserting an operation fails is as wrong as a row
  naming the wrong errno once the operation works, and it misleads in the worse direction: a reader
  avoids something that would have worked. `internal/fuse/unimplemented_test.go` is the mechanism for
  the errnos in that table, and rename's departure from it is now pinned from the other side — a test
  asserts the bridge *dispatches* to `Rename` rather than reaching go-fuse's `ENOTSUP` default, which
  is what a drifted signature or a build tag excluding `rename.go` would silently restore.

- **Eight documents outside the README described the pre-rename filesystem, and four of them told
  users an operation fails that works** ([#162]). `docs/architecture/overview.md` listed `unlink`,
  `rmdir`, and `rename` as unimplemented and said `rm` returns `EROFS`; `docs-platform/guide/`
  told users `mv` fails with `ENOTSUP` "because there is no rename"; the playground's benchmark
  script worked around `rm` by shelling out to `aws s3 rm`. Every one was an accurate description of
  v0.10.3 being read by users of a version where those operations work — understating rather than
  overstating, which is friendlier and still wrong, because it sends people to build workarounds for
  a problem that is fixed.

  Two mechanical gates now cover the class, because it has gone stale twice in the same place
  (`internal/config/docs_posix_test.go`):
  - **No document may state an operation count.** Eight files said "roughly 10 of ~40 VFS operations
    are implemented", each having copied it from the audit that measured it once; six operations
    landed across three releases and not one sentence changed. This is the version-constant problem
    exactly — one number, many copies, no way for a copy to learn it is wrong — so it gets the same
    answer: say a subset is implemented and point at the table. `CHANGELOG.md` is exempt, with the
    reason recorded in the code: a released section is an immutable record of what that release did,
    and editing its counts to match today would falsify the record.
  - **The README's "Not implemented" table may not name an operation whose go-fuse interface
    `internal/fuse` asserts.** It reads the `_ fs.NodeUnlinker = (*DirectoryNode)(nil)` assertions
    rather than the method set, because the assertion is what makes support real — go-fuse probes each
    interface with a type assertion and substitutes a default when it is absent, and for `Unlink` and
    `Rmdir` that default is *success*. A method with a drifted signature compiles and is silently
    never called; the assertion is what fails.

  The tempting third gate — flag any line pairing an implemented operation with a refusal errno,
  repo-wide — was written, measured, and rejected: ten hits, of which eight are changelog entries
  correctly describing past releases. A gate whose output is 80% false gets deleted. Both surviving
  gates were verified by mutation, and the first one's word list is written out in full because a
  first draft with `ten|twenty|thirty|forty` passed on "sixteen of forty VFS operations" — a narrow
  pattern that passes is indistinguishable from a correct repository.

- `internal/filesystem/interface.go` says what it is: a design sketch with **no importers anywhere in
  the tree**, whose only implementation is its own test mock. It reads like a capability list — it
  declares `Rename`, `Truncate`, `Chmod`, `Chown`, `Link`, `Symlink`, `Readlink`, four xattr methods,
  and `Statfs` — and a reader taking a method there as evidence of support would be wrong about
  several. That is not hypothetical: `internal/vfs`'s `FileType` comment already records this
  interface advertising `Symlink` and `Link` with nothing behind them as what went wrong in v0.10.0.
  Kept rather than deleted because the multi-protocol work it sketches is tracked ([#181]) and this is
  the record of its original shape.

- **`write_buffer.max_memory` is enforced.** It was declared in the config schema, defaulted to
  `"512MB"`, validated as a size string, and read by nothing ([#205]) — so every mount since the key
  appeared reported a write-buffer ceiling and enforced none, on the one path that holds user data in
  memory before it is durable. The bound reclaims before it refuses: at the ceiling with flushable
  data it flushes and accepts, because a limit that turned legal writes into ENOSPC would be worse
  than the unbounded growth it replaced — with the shipped 512 MB default that would mean failing
  every workload writing more than 512 MB in total. A single write larger than the entire limit is
  admitted, since `write(2)`'s ENOSPC means "filesystem full" and a caller retrying it would get the
  same answer forever. A refusal surfaces as ENOSPC through `vfs.ErrNoSpace`, not as EIO.
- **A single file can grow past the write buffer's memory bound.** Reclaiming flushes other keys and
  deliberately skips the one being written, since its pending writes are about to be extended and
  uploading them now guarantees a second upload moments later. As the only rule that made the bound
  refuse the most ordinary write there is: a program appending to one file has no other key to flush,
  so at the shipped 512 MB default, writing any file past 512 MB failed at exactly 512 MB with
  ENOSPC — sequentially writing a large file being the workload ObjectFS exists for. The target key
  is now flushed as a last resort, which is what streaming a large file through a bounded buffer
  looks like; a test writes a file to eight times its limit and asserts both that every write
  succeeds and that the resulting object is whole, so a lossy reclaim fails rather than passing
  quietly.

- **A cache that answered a ten-byte request with two bytes now reports a miss ([#178]).** The
  `types.Cache` contract is that a partial hit is a miss, and it is a contract about data integrity
  rather than about return values: `internal/fuse` passes a non-nil hit to the kernel verbatim as file
  content, so a short answer is a truncated read reported as a successful one, and the caller cannot
  distinguish a short cache entry from a short file. The Redis implementation used `GETRANGE`, which
  clamps to the stored value's length and returns what it can — `GETRANGE k 8 17` over a ten-byte value
  answers with two bytes and no indication that eight are missing. It had ten tests of its own, all
  passing, none of which asked for a range longer than what was stored.

  What found it is the durable part: **`internal/cache/cachetest` is a shared conformance suite that
  every `types.Cache` implementation is now run against.** There were five implementations, one
  contract, and no test in common — each was checked against the questions its own author thought to
  ask, which is why four of them satisfied a rule the fifth violated in the most consequential
  direction. Ten cases, each stating in its failure message what a caller would observe: exact-range
  hits, straddling and past-the-end reads as misses, a request longer than the entry as a miss, the
  open-ended `size <= 0` form, the returned slice not aliasing the cache's own storage, a newer `Put`
  winning where it overlaps, and `Delete` removing the key it names and nothing that merely shares a
  prefix with it. A sixth implementation is one enrollment away from being held to the same contract.

### Changed

- **One size parser reads every size in a configuration file ([#159]).** `pkg/utils.ParseBytes` is
  now the only implementation; the three surviving copies — `internal/compression.parseSize`,
  `internal/config.parseOptionalSize`, and a fourth in `tests/unit_test.go` — are deleted, and
  `utils.ParseOptionalBytes` handles the unset-means-zero case identically everywhere. Every size a
  config file names is therefore validated at load with a message naming the YAML key, and no size is
  substituted silently.

  Four parsers were four answers to the same string, and the disagreements were not cosmetic. Each
  one is verified by running the deleted code rather than by reading it:
  - `internal/compression`'s stopped its unit table at GB, so `min_size: 1TB` was an error while
    `1GB` worked; it accepted `-1MB` as a **negative** compression floor, which makes
    `len(data) < c.minSize` false for every input and compresses everything including the bytes the
    threshold exists to skip; and `99999999999GB` overflowed to `math.MaxInt64`, the same defect
    inverted — a floor nothing is ever below, so compression is configured on and never happens.
    Neither reported anything.
  - The copy in `tests/unit_test.go` fell through to `strconv.ParseFloat`, which accepts Go float
    syntax: `InfMB` parsed as `math.MaxInt64` and `1e3MB` as 1000 MB. It also rejected `1TB`. A test
    asserting against a private copy of a parser is a test that agrees with itself — this one passed
    while disagreeing with the parser the mount used.
  - `internal/adapter`'s, removed earlier in this release, returned 1 GiB and no error for anything
    it could not parse.

  `ParseBytes` is strict for the reason the loader is strict: it rejects trailing garbage (`4KiB`,
  the spelling someone who knows the units writes), negatives, `Inf`/`NaN`, exponent and hex-float
  notation, and any value that overflows `int64` once multiplied. The empty string is the one case
  with a second meaning, and `ParseOptionalBytes` is where it lives — unset means zero, which is the
  caller's signal to use its own default. It deliberately does not distinguish `""` from a literal
  `"0"`, because no caller in this repository does.

- **Each listener's address is one setting, beside the `enabled` flag that governs it
  ([#202], [#211], [#212]).** `global.metrics_port`, `global.health_port`, `global.profile_port`,
  `monitoring.metrics_addr`, `monitoring.health_check_addr` and `monitoring.enable_pprof` are all
  removed, replaced by `monitoring.metrics.addr` and `monitoring.health_checks.addr`, both defaulting
  to **loopback** — `127.0.0.1:8080` and `127.0.0.1:8081`. Same ports, so an existing same-host
  Prometheus scrape keeps working; the host is what changed.

  A port and an address were never two settings. `monitoring` declared the two addresses, defaulted
  them, documented them — and read neither, while the ports two sections away were what the listeners
  used. So an operator who set `health_check_addr: 127.0.0.1:8081` to keep an unauthenticated
  diagnostic endpoint off the network got a wildcard bind and no warning: the setting that would have
  changed it was inert, and the setting that was live could not express a host at all, because the bind
  was `fmt.Sprintf(":%d", port)`. Both endpoints are on by default, so a stock
  `objectfs s3://bucket /mnt` published per-operation counts, error rates, sizes and timings — and, on
  `/health`, component names and error strings — to anything that could route to the host.

  An address subsumes a port, so keeping both would have preserved the disagreement. It also settles
  what a port could not: `health_port: 0` disabled the health endpoint while `metrics_port: 0` was
  treated as unset and defaulted back to 8080 and bound it, so two adjacent fields spelled "off"
  differently and the metrics one failed in the direction that leaves a port open. There is no `0` in
  an address, and each listener already has an `enabled` flag next to its new `addr`.

  `global.enable_pprof` and `global.profile_port` are removed rather than wired. Nothing read either.
  The one pprof server in the tree is `pkg/profiling`'s, which has no importer, also binds every
  interface, and serves *mutating* `/memory/gc` and `/memory/free` handlers with no authentication —
  binding a third unauthenticated listener inside the change that stops binding two of them was the
  wrong trade to make on the strength of a boolean nothing read. Its fate is [#245].

  Three further consequences:

  - **A bind failure now fails startup and names the address.** Both servers used to bind on a
    goroutine and log, so a mount whose metrics port was taken came up with no endpoint and one line
    in the log to say why — an operator finds that out when a probe starts failing. This deliberately
    contradicts [#192]'s reasoning that non-fatal was "the right call for observability":
    `enabled: false` is already how you ask for no endpoint.
  - **Validation catches what a listener reports badly.** `net.SplitHostPort` accepts `"99999"`, so
    the port range is checked explicitly and the error names the field. `health_port: 99999` used to
    reach `net.Listen` from YAML unchecked.
  - **`OBJECTFS_METRICS_PORT`/`OBJECTFS_HEALTH_PORT` become `OBJECTFS_METRICS_ADDR`/`_HEALTH_ADDR`,**
    and `OBJECTFS_METRICS_ENABLED` — documented in two places and assigned by nothing, which is
    [#202]'s shape in the setting that *closes* an endpoint rather than the one that moves it — is now
    wired, along with a new `OBJECTFS_HEALTH_ENABLED`. Both parse strictly: a value that is not a
    boolean fails startup naming the variable, where the feature-flag variables coerce anything but
    `"true"` to false. These two govern unauthenticated endpoints that default to on, so silent
    coercion is wrong in whichever direction it picks.
  - **The endpoints are documented.** `grep -rn health_port docs/ README.md configs/ examples/` used to
    return nothing: the knobs existed, were read, changed behavior, and appeared in no shipped
    documentation or example config ([#192]). The README now has a *Metrics and health endpoints*
    section with both addresses, the `curl` that reaches each, the environment overrides, and why the
    defaults are loopback; `docs/index.md` names the addresses beside the features rather than listing
    "health monitoring" with nowhere to point a probe.

  The test gap is the more interesting half. `TestStartMetricsBindsTheEndpoint` scraped `127.0.0.1`
  and passed against a wildcard bind, because a wildcard bind answers on loopback too — so the tests
  asserted that *something* was listening and never that it was listening where the configuration
  said. `Collector.Addr()` now reports the bound address, and the regression tests assert two things:
  that it equals what was configured (a wildcard bind reports `0.0.0.0` or `[::]` here), and that the
  endpoint does *not* answer on a routable non-loopback address of the host. Verified by mutation —
  restoring the `":"+port` bind fails both halves while the old-shaped test stays green.

- **Compression is configured under `storage.s3.compression`, not `write_buffer.compression`
  ([#157]).** Nothing has ever compressed a write buffer. The block always configured the codec the
  S3 backend applies to a whole object on its way to the wire, and the misplacement mattered in both
  directions: an operator tuning the write buffer was changing how objects were stored, and an
  operator looking for how objects are stored had no reason to read the write-buffer section. It now
  sits under the backend that applies it. Defaults are unchanged — `enabled: false`, `zstd`, level 3,
  `min_size: 4KB`.

  `write_buffer.compression` and `performance.compression_enabled` are **removed rather than
  deprecated**, so a configuration file still setting either fails to load with the offending key
  named. That is deliberate, and follows the precedent set by the `security.encryption` booleans
  removed in v0.10.1: a key kept as an ignored field means an operator's compression settings
  silently stop applying on upgrade, which is the same failure as the unknown keys strict decoding was
  introduced to catch, arrived at by a different route.

  `performance.compression_enabled` is the more instructive of the two. It defaulted to **true**, was
  read by nothing, and sat two sections away from the real setting that defaulted to **false** — so
  the shipped configuration contained a prominent `compression_enabled: true` while no object was ever
  compressed, and anyone who read the file to find out came away with the opposite of the truth. It is
  removed rather than wired up because compression happens in the S3 backend, on the object, and a
  second boolean over one feature can only ever disagree with the first. `OBJECTFS_COMPRESSION_ENABLED`
  survives and now assigns `storage.s3.compression.enabled`: the variable's name was never wrong, only
  what it assigned to, and exporting it previously had no effect on whether anything was compressed.

  One assertion in the mapping test had to change with it, for a reason worth recording: it asserted
  `Algorithm: "zstd"`, which is also the default — so a `buildS3Config` that hardcoded `"zstd"` and
  ignored the configuration passed. Verified by making exactly that mutation. The test now uses `lz4`,
  because every value in a mapping test has to differ from the value the field would hold if the
  mapping were absent. That is the shape of the original config-plumbing defect: a field nothing mapped
  still arriving at a plausible value from somewhere else.

- **`performance.read_ahead` reaches the prefetcher, and has five keys instead of twenty ([#176]).**
  Every read-ahead setting was decoded, defaulted, range-checked at load, documented on its own page,
  and shipped in four preset config files — and read by nothing, because the mount constructed its
  read-ahead manager with a literal `nil` and ran that manager's built-in defaults. So a deployment
  that set `window_size: 128MB` for a streaming workload was prefetching 64 KB, and had no way to find
  that out.

  The reduction is the fix, not a simplification of it. The two sides did not disagree about a value;
  they disagreed about what read-ahead *is*. `internal/config` described a strategy selector
  (`strategy: simple|predictive|ml`) over a pattern detector with a confidence threshold and a
  prediction window, a bandwidth-capped prefetcher, and an online-learning model with
  `ml_model_path`, `learning_rate`, `pattern_depth` and `model_update_interval`. What exists in
  `internal/fuse` is a sequential-access detector with a prefetch window, five fields, tuned against
  measured byte counts. Beyond `enabled` there was no field-name overlap at all — nothing to pass
  through — so the block was cut down to the detector's own knobs and wired:
  `enabled`, `window_size`, `min_sequential`, `concurrent_reads`, `ttl`.

  Wiring the old block would have been worse than leaving it inert. A validated `ml_model_path`
  reaching no model loader is a claim about the software, and range-checking `learning_rate` to 0–1 is
  what made the whole set look load-bearing: a user whose config is rejected for an out-of-range value
  reasonably concludes the accepted values do something. Fifteen keys are **removed rather than
  deprecated**, so a file still setting one fails to load with the key named — same reasoning as
  `write_buffer.compression` above.

  `performance.read_ahead_size` is removed too, and it is `compression_enabled`'s twin: a prominent
  `64MB` default, read by nothing, sitting two lines above the block describing the same quantity with
  a different default. Two names for one setting can only ever disagree.
  `OBJECTFS_READ_AHEAD_SIZE` goes with it, and the six `OBJECTFS_READAHEAD_*` variables become four —
  the two counts now report a parse failure rather than silently keeping the default, because a worker
  count reverting to 4 when 1 was meant is prefetch traffic nobody asked for.

  Behavior at the default configuration is deliberately unchanged: `config.NewDefault`'s block is now
  exactly `fuse.DefaultReadAheadConfig`, which is what every mount has run all along, and tests on both
  sides of the seam assert those two remain equal. Two validation rules are new because the values now
  reach code — `concurrent_reads: 0` is rejected (it is the worker count, and zero starts no workers,
  so every prefetch is queued and never performed: read-ahead silently off while the config says on),
  and an empty `window_size` is rejected when enabled (an empty floor is a floor of zero, not the
  default). A **disabled** block is no longer validated at all, which is the same defect pointing the
  other way: a mount should not be refused over settings nothing will read. Two checks stay
  unconditional, because they catch a typo rather than a setting: a `window_size` that is not a size at
  all, and a `ttl` written without a unit — `ttl: 5` is five nanoseconds, silently, since yaml.v2 reads
  a bare integer into a `time.Duration` as a raw nanosecond count. Both would otherwise surface months
  later, when read-ahead is turned on, as a validation failure over a line nobody touched. The `ttl`
  omission was found by the reflection walk over the schema that pins every duration to
  `validateDurations`, the moment this change gave read-ahead a duration at all.

  Presets and docs were rewritten rather than relabeled. `readahead-simple.yaml` became
  `readahead-disabled.yaml` — it configured "no pattern detection, no prefetching", whose honest
  spelling is read-ahead off — and `readahead-ml.yaml` was **deleted**, because a preset cannot be
  corrected into configuring a model loader that does not exist. `docs/features/read-ahead.md` lost its
  ML training guide and its three-strategy comparison for the same reason.

  One thing the wiring exposed and did not fix: `min_sequential` has no effect below 6, because the
  prefetch also requires a confidence above 0.5 and confidence is `sequentialHits/10` — two thresholds
  over one counter, of which one is configurable. The shipped default of 3 is inside that range, so the
  documented default does not describe the default behavior. Reconciling them changes prefetch
  behavior at the default configuration, which wants a measurement rather than a number nudged in
  passing, so it is filed as [#247] and documented where the setting is.

- **The `cluster.redis` block selects the cache a mount uses, and the `cache` block reaches it
  ([#178]).** `cache.NewFromConfig` — the only reader of `cluster.redis.*` anywhere — had no caller.
  The adapter built a `MultiLevelConfig` literal of its own instead, so seven `cluster` keys plus a
  seven-key `redis` sub-block were decoded, defaulted, validated and documented while no mount
  consulted any of them: a deployment that configured a shared Redis cache got a private in-process
  one, with no error and no warning, and looked correct until two nodes disagreed about a file.

  Both halves of that mistake are fixed together, because they are the same mistake. `NewFromConfig`'s
  other arm passed a literal `nil` to `NewMultiLevelCache`, discarding the L1/L2 sizing, the TTL, the
  persistent-cache directory and the eviction policy its argument carried — so even with a caller, most
  of the `cache` block would still have been ignored. The mapping now lives in `internal/cache` beside
  the selection rather than in the adapter, since a second copy of it is how the two came to disagree
  in the first place, and `Adapter.cache` is typed as `types.Cache`: naming the concrete
  `*cache.MultiLevelCache` there is what made the function uncallable, as the field could not hold what
  it returns.

  **An unreachable Redis now fails the mount** rather than falling back to an in-process cache. Falling
  back is this same defect one layer out — both nodes come up, both believe the cache is shared, and
  nothing in either log explains the disagreement — so the error names `cluster.redis` and the mount
  does not start. This is the third instance of the shape [#156] and [#176] were: a config block whose
  every layer worked except the one that had to call it.

- **`storage.s3.cost_optimization` keeps one key, and it does what it says ([#203]).** The block had
  six; five are removed and one is new. `small_objects_on_standard` stores an object on STANDARD when
  the configured `storage_tier` would bill it as larger than it is *and* STANDARD is genuinely cheaper
  for it. Defaults to `false`, because it changes the storage class objects are written with, and an
  operator who set `storage_tier` should get that tier until they ask otherwise.

  Removed, not deprecated — configuration is decoded strictly, so a file still carrying one of these
  fails at startup naming the key rather than silently ignoring it:

  | Removed key | Why |
  |---|---|
  | `enabled` | Gated nothing. The backend has no such field |
  | `tiering_enabled` | Automatic tier transitions exist in the S3 backend; nothing on the mount path invokes them |
  | `lifecycle_enabled` | Lifecycle rules are a `PutBucketLifecycleConfiguration` call this backend never makes |
  | `transition_to_ia` | Same: a lifecycle rule, never written |
  | `transition_to_glacier` | Same |

  This is the fourth instance of the shape [#156], [#176] and the `cluster.redis` item above were, and
  the most direct: `internal/config.S3CostOptimization` and `internal/storage/s3.CostOptimization`
  shared **no field name at all**, so the block could not be mapped even in principle. `buildS3Config`
  carried a comment saying it was not mappable, which was true and is not a fix — the two types had
  drifted until the only honest options were to plumb a field that did not exist on both sides or to
  delete what nothing read.

  Two more `s3.CostOptimization` fields survive as Go struct fields with no YAML key, and the
  distinction is deliberate: `EnableAutoTiering` and `CostThreshold` are read by code an embedder can
  call directly, while a *mount* has no path to it. `MonitorAccessPatterns` is likewise unmapped, and
  for a second reason — the map it populates holds one entry per distinct key read and nothing evicts,
  so on a bucket with many objects it is unbounded growth for a report no mount displays.

- **The small-object rule compares prices instead of one size.** `HandleStandardTierOverhead` tested
  `objectSize < 128 KiB` with no reference to the configured tier, so it moved objects to STANDARD from
  tiers that publish no billing minimum at all — including DEEP_ARCHIVE, at ~23× the storage rate —
  and from the three that do at sizes where they are still much cheaper. Being under a floor does not
  make STANDARD cheaper: the crossover is at `minBillable × rateTier / rateStandard`, which at list
  prices is **69.6 KiB** for STANDARD_IA, **55.7 KiB** for ONEZONE_IA and **22.3 KiB** for GLACIER_IR.
  A 32 KiB object billed as 128 KiB of GLACIER_IR costs about a third of 32 KiB on STANDARD. Three
  conditions are now required — the tier publishes a minimum, the object is under it, and STANDARD is
  cheaper for this object at the prices this deployment pays, discounts and `CustomPricing` included.

  This also keeps GLACIER and DEEP_ARCHIVE out on a second ground that is not about money: their
  objects cannot be read without a restore, so diverting one to STANDARD would change what a read of
  that object *does*, not only what it costs. A cost heuristic must not decide retrieval semantics.

  `TierValidator.GetRecommendations` had the identical size-only rule and now uses the same three
  conditions at list rates. It has no mount-path caller — it is exported API — but advice nobody can
  act on wrongly is still advice someone will act on.

- **The billing-minimum warning names the tier the object is actually stored on.** `ValidateWrite`
  ran before the small-object diversion and described the configured tier's floor, so an operator who
  enabled `small_objects_on_standard` *because of* that warning still saw `billed_size=131072` on a
  16 KiB object that was about to be stored on STANDARD and billed as 16 KiB. The diversion logs at
  Debug, so at the default level the misleading half was the only half visible. The tier decision now
  precedes validation and `ValidateWriteToTier` takes the effective tier. `tier_constraints.min_object_size`
  deliberately does *not* follow it: that is a floor the operator configured for this mount, and an
  internal cost optimization is not a reason to stop enforcing it.

- **A per-object storage class bypasses the CargoShip transporter.** `Transporter.optimizeStorageClass`
  returns the transporter's *own* config storage class — fixed at construction from `storage_tier` — for
  any archive with no `AccessPattern` and no `RetentionDays`, which is every archive ObjectFS builds. It
  never reads `Archive.StorageClass`, the field whose comment says "Target storage class". So the
  diverted class was computed correctly and dropped at the boundary: the object stored fine, read back
  fine, and only the invoice differed. Objects whose class differs from the configured tier now take the
  direct `PutObject` path, joining the existing bypasses for compression and for encryption modes the
  transporter cannot express. The common case is unaffected and keeps CargoShip's throughput
  optimization. Filed upstream as [scttfrdmn/cargoship#352]; `OptimizedTransporter`, a different type
  ObjectFS does not construct, already honors the field.

  Found by asserting the storage class recorded at the S3 endpoint rather than the value passed in — the
  same technique that found the `INTELLIGENT_TIERING` default in v0.10.1, and the reason the seam test
  exists at all.

- **The live pricing drift test needs no AWS credentials and no longer names its own queries** ([#161]).
  It fetched from the Pricing API by shelling out to `aws pricing get-products`, so it skipped for
  anyone without a configured profile — including CI. A drift check that skips is not a drift check. It
  now fetches the same public offer files the generator reads, over plain HTTPS, and compares the whole
  committed table against a fresh extraction across five regions spanning every price band AWS
  publishes. `internal/cost`'s drift guard moves onto `PricingManager.StorageRate` for the same reason:
  the field it read no longer exists, and the manager is the path a caller actually takes.

- **The release security scan is a gate** ([#196]). `security-scan` in `release.yml` already scanned
  the exact binary `publish` attaches, which is the right shape, but it could not fail: trivy-action's
  `exit-code` has no default, so findings uploaded as SARIF and the step passed. It now exits 1 on
  `HIGH,CRITICAL`, with `ignore-unfixed: true` and `scanners: vuln`.

  Each of those is a decision rather than a default. MEDIUM and below still upload and stay visible on
  the security tab without stopping a publish, because MEDIUM in a transitive dependency of a
  filesystem binary is generally not worth delaying a release for. `ignore-unfixed` because a
  vulnerability with no released fix cannot be actioned by a release — blocking on one means the
  project cannot ship until an upstream maintainer acts, which is an availability problem wearing a
  security posture. And the SARIF upload is now `if: always()`, so the findings that failed the step
  are the ones that reach the security tab; without it a HIGH stopped the job before the upload and
  left whoever was cutting the release with an exit code and no way to see the cause.

  It was not a gate before now for a reason worth keeping, because it is the same reason it can be one
  now: the first real run of this scan found a MODERATE advisory in the pinned `aws-sdk-go-v2` ([#195]),
  and switching the gate on then would have blocked every release on a scan nobody had triaged. A gate
  turned on against existing findings is a broken build everyone learns to bypass. That advisory is
  fixed and the baseline is clean, which is the only state in which turning it on means anything.

  The asymmetry the issue names is resolved toward gating: `govulncheck` in `security.yml` exits
  non-zero and so has always been a hard gate on `main` and every PR, while the release — the artifact
  users actually download — was not gated at all. The repository-wide `trivy fs` scan in that file is
  deliberately still not a gate, and now says so: it reports on the source tree rather than on what
  ships, including dependencies reached only by tests, so the gates are `govulncheck` and the binary
  scan. It picks up the same severity floor and `ignore-unfixed` regardless, so a finding in one place
  means what a finding in the other means.

- **`klauspost/compress` v1.18.0 → v1.18.7**, for GO-2026-5841, an out-of-bounds read in the `s2`
  package. Not reachable from this code — `govulncheck` reported it under "packages you import" with
  zero called vulnerabilities — but present in the module the binary is built from, which is what a
  binary scan sees and what the new release gate above would have failed on. Found while verifying the
  baseline was clean before switching that gate on, which is the check being described.

### Removed

- **`StorageTierInfo.CostPerGBMonth`** ([#161]). A rate on a package-level struct cannot know which
  region it is for, and this field was the mechanism by which every cost `internal/storage/s3` reported
  was us-east-1's. Callers use `PricingManager.StorageRate(tier)` for the list rate in the manager's
  region, or `GetTierPricing` where discounts and overrides should apply.

  Removed rather than corrected, deliberately. Leaving the field and filling it from the configured
  region would put a region-specific number on a value shared process-wide by every manager, which is
  the same defect with an extra step. The compiler now finds the callers.

- **`awsrates.Region`** ([#161]). A constant alias for `awsrates.DefaultRegion`, added in the same
  change that made rates region-aware and kept so that callers wanting only to *label* a figure would
  keep compiling. There were no such callers: a grep across every `.go` file in the repository found
  zero uses. `awsrates` is an internal package, so nothing outside the module can reference it either,
  and a deprecation notice nobody can read is not a compatibility measure. Use `DefaultRegion`, or pass
  a region to `ForRegion`.

[scttfrdmn/cargoship#352]: https://github.com/scttfrdmn/cargoship/issues/352
[scttfrdmn/cargoship#353]: https://github.com/scttfrdmn/cargoship/issues/353
[#153]: https://github.com/scttfrdmn/objectfs/issues/153
[#154]: https://github.com/scttfrdmn/objectfs/issues/154
[#264]: https://github.com/scttfrdmn/objectfs/issues/264
[#134]: https://github.com/scttfrdmn/objectfs/issues/134
[#135]: https://github.com/scttfrdmn/objectfs/issues/135
[#195]: https://github.com/scttfrdmn/objectfs/issues/195
[#196]: https://github.com/scttfrdmn/objectfs/issues/196
[#159]: https://github.com/scttfrdmn/objectfs/issues/159
[#156]: https://github.com/scttfrdmn/objectfs/issues/156
[#157]: https://github.com/scttfrdmn/objectfs/issues/157
[#161]: https://github.com/scttfrdmn/objectfs/issues/161
[#162]: https://github.com/scttfrdmn/objectfs/issues/162
[#163]: https://github.com/scttfrdmn/objectfs/issues/163
[#164]: https://github.com/scttfrdmn/objectfs/issues/164
[#176]: https://github.com/scttfrdmn/objectfs/issues/176
[#178]: https://github.com/scttfrdmn/objectfs/issues/178
[#180]: https://github.com/scttfrdmn/objectfs/issues/180
[#181]: https://github.com/scttfrdmn/objectfs/issues/181
[#192]: https://github.com/scttfrdmn/objectfs/issues/192
[#202]: https://github.com/scttfrdmn/objectfs/issues/202
[#203]: https://github.com/scttfrdmn/objectfs/issues/203
[#205]: https://github.com/scttfrdmn/objectfs/issues/205
[#211]: https://github.com/scttfrdmn/objectfs/issues/211
[#212]: https://github.com/scttfrdmn/objectfs/issues/212
[#230]: https://github.com/scttfrdmn/objectfs/issues/230
[#240]: https://github.com/scttfrdmn/objectfs/issues/240
[#245]: https://github.com/scttfrdmn/objectfs/issues/245
[#247]: https://github.com/scttfrdmn/objectfs/issues/247

## [0.10.3] - 2026-08-02

Part 4 of the v0.10.0 audit: say only what the code does, and bill accurately. The audit found that
documentation, cost figures, and repository metadata each asserted things no mechanism checked, so
most of what follows is a correction paired with the gate that fails if it recurs — five of them,
now running on every PR.

This release exists because of a numbering defect worth stating plainly, since it is the second
instance of the same one. `v0.10.2` was tagged when the first of this milestone's twelve issues
closed, and the remaining eleven landed after it — so the tag published under that number holds the
packaging fix and none of the work below, exactly as `v0.10.1` was tagged two hours before the
module-path fix it was cut for. A tag is not a promise that can be revised: it is already in the
GitHub releases list and cached by the Go module proxy, which resolves a version to one tree
forever. So the fix is a new number rather than a moved tag, and the rule that prevents a third
instance is to cut the tag from the merge commit that closes the milestone, not from the one that
opens it.

### Added
- **A gate that fails when `CHANGELOG.md`'s version headings and its link definitions disagree.** Keep a Changelog puts each release in a bracketed heading and defines the link separately at the bottom of the file — two hand edits per release with nothing connecting them, and markdown fails silently in both directions. An undefined reference renders as the literal text `[0.10.2]` instead of a link; a definition with no section renders as nothing. Neither breaks a build, fails a lint, or produces a visibly wrong page, so the only witness is a reader noticing a heading stopped being clickable, which is not a thing readers report. Both halves had already broken: `[0.10.2]` was never defined at all, and `[Unreleased]` still compared from `v0.10.1` — so the link that answers "what is on `main` but not released" spanned two releases and 52 entries of already-released work. `internal/config/changelog_test.go` checks three properties: every section has a definition and every definition has a section, `[Unreleased]` compares from the version constant, and each release's diff starts at the release immediately before it. The third exists because that failure is the one that renders *and* resolves — copying the previous definition and editing only the right-hand side produces a real GitHub diff covering more releases than the section it is attached to. All four failure modes were verified by mutation, which is also how the orphan-definition message got fixed: it printed the URL where the version belonged. This is the same defect the release itself is about, one file over — a fact restated in a second place with no mechanism to notice the two have drifted
- **A gate that fails when documentation names a Go symbol or a CLI flag that does not exist.** `internal/config/docs_symbols_test.go` extracts `pkg.Symbol` references from fenced Go blocks — checked against the packages that same block imports, parsed with `go/ast` — and `objectfs` command lines from shell blocks, checked against the flags `cmd/objectfs/main.go` actually declares. This is the mechanism #182 asked for, and the point is that it fires at authoring time: correcting the nineteen files that issue cataloged only resets the clock, since a fenced code block is a string as far as the compiler, vet, and lint are concerned. It found eleven defects on its first run, listed under Fixed below, and a companion test compares the flag list against `main.go` so the two cannot drift apart silently. The admission rule — *which* references get checked — was chosen by measuring three candidates rather than guessed: checking every `lowercase.Uppercase` in every Go block gives 93 findings of which 3 are real (`s3.Client` is the AWS SDK, `errors.Is` is the standard library), file-scoped imports give 5 of which 3 are real, and block-scoped gives 3 of 3 with no false positives. Its one known blind spot is stated in the test rather than left to be discovered: a continuation block that uses a package imported by the block above it is not checked
- **A gate that fails when documentation links at a page that does not exist.** `internal/config/docs_links_test.go` extracts every relative markdown link from every tracked markdown file and resolves it on disk — relative paths against the linking file's directory, root-absolute paths in `docs-platform/` against VitePress's routing rule, where `/guide/installation` is served from `guide/installation.md` and `/api/` from `api/index.md`. This is #208's mechanism, and it is a Go test rather than the link checker in CI that issue proposed for three reasons recorded in the file: it needs no network and no new tool, so pre-commit and CI check at identical fidelity; it sits with the gates a contributor already satisfies; and an exemption can carry its reason in code, the way `docsExemptFromConfigSchema` does. **It found 45 dead links, not the 24 the issue catalogued** — because #208 was written by walking `docs/`, and two whole classes live outside it: 13 links into SDK `examples/` directories that have never existed, and 8 root-absolute VitePress routes. That gap is the finding, and it is the same shape as `docs_test.go`'s `nestedSectionNames`: scoping a gate to where the defects were already known is how the next cluster stays invisible. A link target is a path, not a symbol, which is why the symbol gate above cannot see it — a link written as `[tuning]` followed by `(./perf.md)` is prose to the compiler, to vet, and to lint, and stays prose after the file is renamed. A third test asserts the walk's *reach* rather than its findings, and it earns its place: a mutation that made the link regexp match nothing left the resolving test passing on zero links and green, and only the reach test caught it
- **A gate that checks `mkdocs.yml`'s nav against the tree, in both directions.** `TestMkDocsNavMatchesTheTree` asserts that every nav entry has a file and that every page under `docs/` is either in the nav or exempt with a stated reason. Both directions, because that is how the defect ran: 47 of 50 entries pointed at no file, *and* 14 of the 17 pages in the tree were missing from the nav. Checking only that entries resolve would have left the orphans, which is the half a reader loses — a page absent from the nav is a page nobody finds. A nav entry is a link target with a different syntax, and that syntax is why it went unchecked: the link gate walks markdown, and `nav:` is YAML. It is a line scan rather than a YAML parse for a stated reason — `mkdocs.yml` carries `!!python/name:` tags for the emoji and superfences extensions, so decoding it needs a custom resolver or unsafe mode, and the nav is a flat list of `- Title: path.md` lines that needs neither
- **`docs/features/compression.md` — what transparent compression costs, and when it saves nothing.** The question it answers is the one #186 was filed for: project-level compression saves bandwidth and end-to-end latency, so what *else* does it buy? Less than you would expect. It names four costs, each measured rather than asserted: a compressed object is not readable by anything but ObjectFS (`aws s3 cp` and boto3 both write the raw zstd frame to disk with a **successful exit status** — no error to notice); a 4 KiB read of a compressed object transfers the whole stored object, which is 1,836× / 7,344× / 29,380× amplification at 16/64/256 MiB; enabling compression turns off parallel range reads for every object in the bucket, compressed or not; and on the three tiers with a 128 KB billable floor, compressing a 100 KB object to 40 KB changes the invoice by zero. Byte counts are presented as the result and wall-clock only as an aside, for a reason stated on the page — bytes are a property of the design, latency is a property of the day, and the audit's 15.6×/43×/216.5× and this page's 3.0×/5.0×/12.3× are the same defect measured on different days. Every figure is either linked to the AWS page that publishes it or carries its bucket, region, date, and payload, which is `docs-platform/index.md`'s standard after its hardcoded chart was removed. **Two of the numbers #186 itself specified are wrong**, and the page states what AWS publishes instead: AWS applies no minimum billable object size to `GLACIER`, `DEEP_ARCHIVE`, or `INTELLIGENT_TIERING`. The archive classes' 40 KB is metadata *added* per object (32 KB at the archive rate, 8 KB at Standard), which points the opposite way from a floor — compression does reduce the bill there, it just cannot touch the surcharge, which is about 23× the payload for a 10 KB object on `DEEP_ARCHIVE`. Writing the page found three defects, filed as #228, #229, and #230
- **A gate that fails when `.github/labels.yml` and the repository's labels disagree — in both directions.** `internal/config/labels_test.go` is the fifth mechanical gate, and it exists because the file is a hand-maintained description of state held on GitHub and nothing compared the two, so they had drifted by nine labels. Both directions, because only one is intuitive: all nine existed on GitHub and were absent from the file, none the other way, so a sync that creates labels from the file is green on every one of them — it has nothing to create. That is the failure mode #190's own acceptance criteria name, and the test for the gate is the one they specify: create a label on GitHub without touching the file and confirm the job notices. Verified by doing exactly that, with a throwaway `zz-drift-probe`. Colors and descriptions are compared too, not just names — a label the file describes differently from the label that exists is drift with a longer fuse, because the name still filters correctly and nothing looks wrong. #190 proposed a `paths:`-filtered sync job that runs when `labels.yml` changes; measurement is why this one runs unconditionally instead. Every drift this repository has had originated *on GitHub* — two labels created by hand in the web UI, one invented by `gh issue create --label`, one created by Dependabot, six defaults never deleted — and none of those events touches the file, so a job keyed on the file changing would have fired on **zero of the nine**. Confirmed with `git log -S` against the exact `- name:` form of each rather than assumed. A companion offline test holds `.github/dependabot.yml`'s `labels:` blocks to the file, which is the seam that left 46 Dependabot PRs unmerged: `automerge` was named there, defined nowhere, and Dependabot drops a label it cannot find without reporting it
- `internal/awsrates` — one table of AWS S3 list prices, with a test that checks it against the live AWS Pricing API. Every rate in it was read from that API rather than from a pricing page, and `AWS_PROFILE=aws go test -tags=integration ./internal/awsrates/` re-reads 23 of them and fails on any difference, so a price change is something the suite reports rather than something a report quietly gets wrong. Two things are in the type rather than at the call sites, because both are where the errors below came from: per-request fields hold the cost of *one* call (AWS publishes per 1,000 or per 10,000), and `GBFromBytes` divides by 10⁹ (AWS bills decimal GB). The rates are us-east-1, first volume band, Standard retrieval speed on the Glacier classes — stated in the package doc, because a rate table that does not say which band it holds invites the comparison against a different one

### Fixed
- **`make -C sdks/c clean` deleted a tracked file.** The clean target removed `libobjectfs.h` along with the shared library, but only the library is gitignored — the cgo-generated header is checked in, because `sdks/c/README.md` lists it as the reference copy of the declarations for a reader who has not built anything. So building and then cleaning left `git status` reporting a deletion the contributor did not make. The committed header was verified byte-identical to a fresh generation before deciding which of the two to keep, since "checked-in artifact" and "stale checked-in artifact" call for opposite fixes. The same Makefile also still exported `GOPRIVATE` and `GONOSUMDB` for `github.com/scttfrdmn/*`, the last copy of a setting removed from `CLAUDE.md` as untrue — all three repositories are public — and the C SDK builds and passes its 15 tests without it
- **Configuring compression turned off parallel range reads for every object in the bucket, compressed or not.** The gate selecting the fan-out asked `b.compressor.Enabled()`, which reports the local *write* configuration and says nothing about the object being read — so a mount with compression enabled read large objects serially even when they had never been compressed, were below `min_size`, had not compressed usefully, or had been written by another tool entirely. Declining the fan-out is right for a *compressed* object, since a zstd or gzip frame must be decoded from its start and there is no set of independent ranges to assemble; the defect was the scope. The compounding part is who paid: most research data does not compress, so the objects that gained nothing from compression were the same ones losing parallel reads because of it, and v0.10.0's headline feature was off for the whole bucket on any mount that enabled compression. This is audit finding C4 one line above C4's own fix, and it survived for the reason C4 did not — C4 moved bytes that did not need moving, which a byte-count assertion catches, while this merely declined an optimization: nothing failed, nothing was logged. The decision is now the object's. Where `GetObject` already needs a `HEAD` for the chunk arithmetic the encoding comes free with it; otherwise the fan-out is attempted and abandoned when a chunk's response carries a `Content-Encoding`, which keeps the cost on compressed objects — which already pay a whole-object fetch — rather than adding a `HEAD` to every large read
- **A compressed object read past the end of its stored body reported `DATA_CORRUPTION` instead of falling back.** The stored body of a compressed object is a fraction of the size the caller is told, so a read at a high offset is a range no chunk can satisfy — every chunk gets a refusal and none ever sees a response header to learn the encoding from. The recorded finding for an unsatisfiable range is "the object shrank mid-read", which is the right diagnosis when the object is not encoded and the wrong one here. That ambiguity is now resolved with one `HEAD`, on a path that has already failed and only for the reads that hit it. Abandoning a fan-out also has to leave no mark on health: several chunks fail from one root cause, `s3-reads` degrades at a few consecutive errors, and a degraded component refuses reads at the top of `GetObject` — so one compressed object could have taken unrelated, perfectly readable objects offline. `TestFanOutFallbackLeavesNoHealthErrors` asserts `ConsecutiveErrors` directly rather than checking that a later read still succeeds, because `RecordSuccess` decrements the counter and the successful whole-object re-read that follows every fallback pushes it back down before the threshold — the weaker version passes even with the 416 misclassified as a service failure, which was checked by making that mutation
- **Three of the eight storage classes carried a minimum billable object size AWS does not publish, and two of the three numbers were real AWS figures used for the opposite purpose.** `StorageTierInfo.MinObjectSize` held 40 KB for `GLACIER` and `DEEP_ARCHIVE` and 128 KB for `INTELLIGENT_TIERING`; AWS's storage class table lists min billable object size as NA for the two archive classes and None for Intelligent-Tiering. The archive classes' 40 KB is per-object metadata AWS bills *in addition* to the object, and Intelligent-Tiering's 128 KB is the size below which an object is not monitored, not auto-tiered, and not charged the automation fee. A minimum and an overhead are arithmetically opposite and so cannot share a field: a minimum *replaces* a smaller object's size, an overhead *adds* to it, and under a floor compressing a 30 KB object to 10 KB saves nothing while under an overhead it saves every byte it removes. So the direction of every small-object recommendation was wrong on the two cheapest tiers, and `ValidateWrite` — which refuses writes below the minimum, itself a policy S3 does not have ([#154](https://github.com/scttfrdmn/objectfs/issues/154)) — was rejecting writes on the strength of numbers that were not minimums. The two real figures now have fields named for what they are, `PerObjectOverheadBytes` and `MonitoringEligibilityBytes`, and the archive classes warn about the surcharge at write time instead of refusing, because packing small files into one archive is a remedy available before the write and not after. What kept this in place was that each wrong value carried a confident comment stating its reason — "40 KB minimum", "128 KB minimum for optimization" — and a number with a stated reason reads as a number somebody checked
- **The archive classes' 40 KB is billed at two rates, and pricing it at one understates the smaller portion 23-fold.** 32 KB is charged at the archive class's own rate for the index Glacier maintains; 8 KB at the S3 Standard rate for the name and metadata S3 keeps so the object stays listable. Standard is $0.023/GB-month against Deep Archive's $0.00099, so a caller that sums the 40 KB and prices it once at the archive rate gets the cheap answer for the expensive part — on a 10 KB `DEEP_ARCHIVE` object that portion is 82% of the true total. `ArchiveOverhead` returns the split, and `calculateObjectCost` prices each part at its own rate
- **`calculateObjectCost` was 7.4% low on every figure it produced, and the helper written to prevent exactly that had no callers.** It divided bytes by 2³⁰ to get GB in three places while AWS bills GB-months in decimal GB, and `awsrates.GBFromBytes`, added for this reason, was used nowhere outside its own package. The test could not see it: it passed `1024*1024*1024`, called it "1GB", and asserted `1.0 * CostPerGBMonth` — an expectation that holds under both the right divisor and the wrong one, because the test made the same choice the code did. A test that recomputes the implementation's formula agrees with it by construction. The seven subtests that replaced it assert hand-computed dollar literals, and their failure messages name the wrong-answer signatures, so `$0.023` for a GiB reads as "something is dividing by 2³⁰ again" rather than as an unexplained mismatch. `TestTierSizeThresholdsMatchWhatAWSPublishes` pins all eight classes' thresholds with the AWS source URL in each failure message, in the shape of `internal/awsrates/rates_aws_test.go` — the rates can be re-read from the live Pricing API, but these thresholds are not published there, so a citation naming the page to open is the substitute
- **`.github/scripts/sync-labels.sh` reported success while syncing nothing, for its entire existence.** It parsed `labels.yml` with a bash regex requiring `- name: "..."` — double quotes — and the file has used single quotes or bare scalars since it was written, so the loop matched **0 of 78** entries and the script then printed `✅ Label sync complete!` and exited 0. #190 concluded from that symptom that `labels.yml` "is applied by nothing"; what was true is worse, and it is the reason the replacement is careful — the file was applied by something that reported success without doing anything, which is indistinguishable from a working sync in every log it produced. The fix is not a better regex: all three YAML scalar forms are legal, all three are present in this file, and a pattern written against one is blind to the other two. It now parses, reports drift by default and changes nothing without `--apply`, refuses to sync at all if the parse yields implausibly few labels, and *edits* labels that already exist rather than skipping them, since color and description drift is drift a create-only sync can never fix. Extras — on GitHub, absent from the file — are named with the command to remove each and are never deleted, because deleting a label removes it from every issue that carries it and that is not a thing to do as a side effect. The header of `labels.yml` also told the reader to run `gh label sync`, which is not a gh subcommand at all
- **Nine labels existed on GitHub and were absent from `.github/labels.yml`.** `area: sdk` and `area: ci-cd` were created by hand in the web UI — `area: ci-cd` arriving with a null description and the default grey, which is what `gh issue create --label` produces when it invents a label it cannot find rather than failing. `java` was created by Dependabot itself, on the first maven PR after `sdks/java` gained an ecosystem entry, which is to say the drift #190 documented grew by one while that issue sat open. All three are now in the file with their provenance recorded, and the repository's 81 labels now match it exactly, descriptions and colors included — 78 of them had never been applied from the file at all, because of the sync script above
- **`CLAUDE.md` described a GitHub repository that does not exist in four places.** It hand-transcribed the label taxonomy as two tables holding 8 type, 4 priority, and 12 `area:` labels against the 22 `area:` labels that exist — so the document a contributor consults to pick a label was missing ten of the choices, and would go stale again on the next label added. It listed six milestones, five of which are closed, and omitted every open one. Its project-board link used `/orgs/scttfrdmn/projects`, and `scttfrdmn` is a user account, so the URL 404s. And its "Private module — set these for `go get`" block with `GOPRIVATE`/`GONOSUMDB` describes a state that has not been true for some time: all three repositories are public, verified by fetching `objectfs@v0.10.2` with both variables explicitly empty. Each is now a pointer to the authority rather than a copy of it — `labels.yml` for labels, the milestones URL for milestones — because a transcribed list is a claim with no way to be told it is stale, which is the same defect the four documentation gates were built to catch in prose

- **`mkdocs.yml`'s navigation described a documentation site that was never written.** Fifty entries, three of which resolved: the other 47 named pages under `getting-started/`, `user-guides/`, `personas/`, `features/`, `operations/`, `architecture/`, `development/`, and `api/` that do not exist, plus `ROADMAP.md`, which is at the repository root and therefore outside `docs_dir`. `mkdocs build` fails on an entry with no file, so this configuration could never have produced a site — and nothing builds it, which is why it survived: mkdocs is not installed in this environment, no workflow runs it, and Pages is off on the repository. The nav now lists the pages that exist, grouped as the tree groups them, with the 47 recorded in a comment rather than recreated as stubs
- **Fourteen of the seventeen pages under `docs/` were absent from the navigation**, including the four most substantial — `architecture/overview.md`, `features/read-ahead.md`, `features/multipart-uploads.md`, and `s3-acceleration.md`. So the same file both described pages nobody had written and omitted almost all of the ones somebody had. All fourteen are now in the nav; `docs/README.md` is exempt with its reason, being build instructions for a contributor rather than a page about ObjectFS
- **Six directories under `docs/` held a `.gitkeep` and nothing else** — `api`, `development`, `getting-started`, `operations`, `personas`, `user-guides`. Removed. An empty directory is worse here than no directory: three of the six were targets of the dead links above, and the directory existing is what made those links look plausible to whoever wrote them. Two prose pointers into them survived the link sweep because prose is not a link — one in `OBJECTFS.md` and one in `docs/features/multipart-uploads.md`, both written during that same sweep, which is a fair illustration of how the empty directory misleads
- **Both SDK READMEs pointed at seven example programs apiece, and neither `examples/` directory has ever existed.** Thirteen dead links, in the section a reader goes to *after* deciding they want to use the SDK — the point of maximum invested attention. The tempting repair is thirteen stub files, which turns the gate green and teaches nothing; #208 names it ("the docs equivalent of the `echo \"Would update the Homebrew formula...\"` job that v0.10.1 deleted"), so a third test now fails if an `examples/` directory appears holding files too small to be working programs. Both sections now point at the inline examples above them, which do exist and do run. The Python README's monitoring pointer went to `internal/metrics/doc.go` after the obvious target turned out to be an empty directory — `docs/operations/` is one of six directories in `docs/` with no files in them
- **Eight links in `docs-platform/` were VitePress routes with no page behind them**: `/guide/installation`, `/guide/troubleshooting`, `/guide/configuration`, `/guide/performance`, `/api/`, and three more. These are invisible to a walk of `docs/` and to any checker that treats `/guide/installation` as a filesystem path, which is why they outlived #208's audit. Resolved the two ways that issue permits — point at the page that covers the topic, or delete the link — and in one case the deletion is the finding: there is no installation guide because installation is two commands, so it belongs in the Quick Start rather than in a page of its own
- **Twenty-four relative links in `docs/` resolved to files that were never written**, and four of them named the same one. `performance-tuning.md` was linked from four pages — read-ahead, multipart uploads, memory monitoring, and S3 acceleration — which is the pattern worth noting: four authors each assumed the page existed because the others linked it. If it is ever written it has to cite measurements, per the rule added to `CONTRIBUTING.md`, and that is likely why it never was. `ml-training.md` was the most misleading of the set, because there is no model to train: the `Predictor` interface `internal/cache` accepts is never set on the mount path. `docs/index.md` lost the whole audience section — five persona links into an empty directory — replaced with the persona names as plain text, since the `persona:` labels do exist on GitHub and the intent was real
- **`docs/features/read-ahead.md` documented the way to read predictive-cache statistics, and there is no way to read them.** The page called `cache.GetPredictiveCache()`, which exists under no name. The `PredictiveCache` a mount builds is wrapped inside `MultiLevelCache.initializeLevels` and stored as an opaque `types.Cache` in a level; `types.Cache` is six methods about bytes, `GetLevelStats("L1")` returns hits/misses/size, and no exported accessor reaches past either. So prediction accuracy, prefetch efficiency, and cache-hit improvement are computed on every read of every mount and then discarded at unmount. The page now says that, and shows the one way those numbers can be observed today — on a `PredictiveCache` the caller constructs, which watches the caller's own accesses and nothing the filesystem does. Filed as #223, where the recommendation is to export them through the Prometheus surface the rest of the cache telemetry already uses
- **The playground's Go example imported a package that does not exist and called an API that was never written.** `github.com/scttfrdmn/objectfs/pkg/client` is not in the tree, and neither are the `client.Mount`, `GetHealth`, `ListObjects`, and `Unmount` calls beneath it; `config.Config` is `config.Configuration`. Replaced with the real embedding API — `adapter.New`, `Start`, `Stop` — which was compiled to check it before publishing, along with the caveat that makes it honest: these types are under `internal/`, so the example builds inside this module and nowhere else, and running the binary is currently the only supported way to use ObjectFS from another program
- **`objectfs config validate`, `config diff`, and `config generate` were documented as runnable, and there are no subcommands.** `OBJECTFS.md`'s configuration-validation section offered four commands of which one exists; each of the other three exits 1 with an argument error. They are now shown struck through with what each would have done, since a config-diff and a config-generate remain reasonable things to want, and the one real command — `--dry-run`, which loads a file, runs every validation rule, and never touches the mount point — is marked as the one that works. `--profile high_latency` was doubly wrong: there is no profile generator and no profile mechanism of any kind
- `docs-platform/index.md`'s Quick Start opened with `curl -sSL https://get.objectfs.io | sh` followed by `objectfs mount` — an install script for a domain this project does not serve, then a subcommand that does not exist. It now builds from source and mounts with two positional arguments, and notes that the mount runs in the foreground, which the original did not and which is the next thing a reader would have been confused by
- **`OBJECTFS.md`'s table of contents listed eight sections that were never written, and omitted the one that exists.** Entries 8 through 15 — Product Family & Roadmap, Monitoring & Observability, Security & Compliance, Operations & Maintenance, Development Guide, API Documentation, Troubleshooting & Support, Appendices — have no corresponding heading at any level; the document ends at Advanced Features, which the list did not mention. So half the contents of a 1,900-line document were links to nothing and its last section was unreachable from the top. The list now matches the headings, records what the eight were, and points at the documents that do cover that ground
- `CONTRIBUTING.md`'s eight table-of-contents links all resolved to nothing, because every heading they targeted begins with an emoji: `## 🤝 Code of Conduct` anchors as `#-code-of-conduct`, not `#code-of-conduct`. Both files' broken links were found by markdownlint's MD051, which had been failing on them for as long as the rule has been enabled — the hook only reports files a commit touches, so a defect in a file nobody edits is a defect nobody sees
- **`pip install objectfs` installed an unrelated project.** The Python SDK's README documented three `pip install objectfs...` commands; the SDK has never been published — no workflow here publishes either SDK — and the name is taken on PyPI by a "Simple Python VFS module" from 2015 by a different author. So the command did not fail, it succeeded with somebody else's code, which is the worse of the two outcomes and indistinguishable from success until an import fails. `@objectfs/sdk` on npm at least 404s. Both READMEs now install from this repository and say why
- **The Python and JavaScript SDKs declared themselves MIT, and this project is Apache 2.0.** `setup.py`'s classifier and `package.json`'s `license` field, which are exactly the places a licence scanner or a dependency review looks — the Java SDK's `pom.xml` had it right, so the three shipped manifests disagreed with each other and two disagreed with `LICENSE`. Both now say Apache-2.0. The `team@objectfs.io` author address went with them: it is not a deliverable mailbox
- **`docs-platform/guide/index.md` was the last document claiming POSIX compliance**, and checking the rest of the page found more: hot configuration reloading (`SIGHUP` is registered alongside `SIGINT`/`SIGTERM` and treated as shutdown, so sending it **unmounts the filesystem**), "Authentication and authorization with RBAC" (there is no auth code — zero hits for `RBAC`, `Authorize`, or `Authenticate` outside tests), load balancing and distributed clusters as shipping features (nothing outside `internal/distributed` imports it), S3 lifecycle management (no lifecycle API call exists anywhere in the tree), Kubernetes persistent volumes (no CSI driver, no chart, no manifest), and "Compliance and governance" (no audit log). Its architecture diagram drew the distributed coordinator as a solid component and omitted both layers of the v0.10.1 refactor; it now shows `internal/fuse` → `internal/vfs` and draws the coordinator dashed, because that is what it is
- Dead-domain links removed from six files: a community forum at `community.objectfs.io`, a documentation site at `docs.objectfs.io`, and commercial support at `mailto:support@objectfs.io` — none resolves, so a reader with a problem was sent to three dead ends before reaching the issue tracker. `mkdocs.yml`'s `site_url` pointed at `objectfs.io`, a parked registrar page: no site is published from either documentation tree and no Pages workflow exists, so it named a domain this project does not serve in every canonical link and sitemap entry a build would emit. All now point at the repository, which is where the documentation is
- **`CONTRIBUTING.md` told contributors to use LocalStack.** This project does not use LocalStack and never has; `CLAUDE.md`, `DEVELOPMENT.md`, and the CI workflow all say to use real AWS or the in-process substrate emulator. The section now names `internal/testaws` as the default choice, real AWS behind `-tags=integration` for what an emulator cannot answer, and a hand-written mock as the last resort — with the reason, which is that a mock sits on the far side of a seam and agrees with its caller by construction, and that is how 32,680 lines of tests missed roughly 45 defects. Its "Running Tests" block also omitted `-race` from every command; it now shows what CI runs
- **The supported-operations table now names the errno for every unimplemented operation, and a test holds it to that.** Six rows said only "not implemented", which tells a user nothing about what their tool will print or branch on. The errnos are not ObjectFS's to choose — with no method on `DirectoryNode`, the answer is go-fuse's default for the absent interface, and those defaults are neither uniform nor guessable: `rename`, `symlink`, `link`, `mknod`, and `fallocate` give `ENOTSUP`; `getxattr` and `removexattr` give `ENOATTR`, which *is* `ENODATA` on Linux; `listxattr` succeeds with an empty list; and `unlink` and `rmdir` default to **success**, which is why this package implements them only to refuse. `mv` had in fact been documented as `ENOSYS` on the strength of a reasonable guess, and it is `ENOTSUP` — a difference that matters, because `ENOSYS` means "this filesystem will never do this" and a caller may stop asking. `internal/fuse/unimplemented_test.go` now drives the real `fuse.RawFileSystem` (no mount, no macFUSE, no privileges) and asserts each one, so a go-fuse upgrade that changes a default is something the suite reports rather than something the README quietly gets wrong
- **The locking row said "not implemented", and locks are not refused — they are host-local.** The mount does not set go-fuse's `EnableLocks`, so the kernel never negotiates `CAP_POSIX_LOCKS`/`CAP_FLOCK_LOCKS` and never asks ObjectFS to arbitrate; it tracks locks itself, on the mounting host. `flock` therefore *succeeds* and means nothing to any other mount of the same bucket, which is a worse failure than a refusal and a different one than the README described: two hosts will both believe they hold the same exclusive lock. Asserted alongside the errnos, in both directions — setting `EnableLocks` without implementing `Getlk`/`Setlk`/`Setlkw` would flip every locking caller to `ENOTSUP`, SQLite included
- **Every write was costed at a tenth of its price on the default configuration.** `internal/storage/s3` stored the Standard PUT rate as `0.0005`, which is what AWS charges per *1,000* requests, in a field the code then divided by 1,000 again — so a PUT was reported at $0.0000005 against a real $0.000005. `internal/cost` had the same rate right, which is the more instructive half: the two packages disagreed by 10×, so what an operation cost depended on which package a caller reached for, and neither was flagged by anything. Verified against the live Pricing API, which also turned up two more the issue had not recorded: Glacier Instant Retrieval PUT was 4× low and its GET 5× low
- **Storage costs were 7.4% low, everywhere.** `internal/cost` converted bytes to GB by dividing by 2³⁰, with a comment asserting the binary reading was correct. S3 quotes GB-months in decimal GB, so the correct divisor is 10⁹ and the ratio between them is exactly 1.0737. The comment is why it survived: it made a wrong unit look considered rather than mistaken, so a reader checking the line found a deliberate-looking choice and moved on. The tests could not catch it either — they passed `1024*1024*1024` bytes, called it "one GB", and asserted the per-GB rate came back, which is an expectation that holds under both the right divisor and the wrong one. The new tests state hand-computed dollar figures as literals instead
- Glacier Flexible Retrieval's retrieval rate was `0.02/GB` with the comment "Variable based on retrieval speed" — which is not a rate, it is a record that nobody had established which of the three retrieval speeds it was. AWS charges $0.01/GB for Standard retrieval; the table now says so, and the package doc names the speed each Glacier figure assumes
- `internal/cost/pricing_drift_test.go` compared `DefaultPrices` against its own hand-written copy of the rates, on the stated grounds that importing `internal/storage/s3` would create a cycle. There is no cycle — `go list -deps` confirms s3 does not import cost — so the literal was a third copy of the rate card, introduced by the test whose purpose was to catch there being more than one. It also had no entry for `REDUCED_REDUNDANCY`, so that tier went unchecked by the drift test that existed to check tiers. It now compares both tables directly against `internal/awsrates`, for every class the config loader accepts

### Changed
- **`make test` now runs the race detector**, which it did not, while `CLAUDE.md` and `CONTRIBUTING.md` both said every test in this project runs with it. The local gate was therefore weaker than the CI gate it stood in for — and this repository had sixteen concurrency bugs filed after a document declared it race-free, most of them found by the detector. `test-race` is kept as an alias so existing habits and scripts keep working
- **`CONTRIBUTING.md` states the rule for performance claims that no test can enforce.** A throughput, latency, or speedup figure in documentation must cite the benchmark that produced it by file and function, the parameters it ran with, and a copy-pasteable command; without all three, say nothing about throughput. This is the procedural half of #182's gate, and it is a rule rather than a test because a number in prose cannot be compared to a benchmark automatically. The audit's most-repeated false claim was "4.6x throughput improvement" in 21 places across 9 files, uncheckable by construction, attributed to a congestion-control implementation with no caller on any mount path — which is why it survived nine files' worth of review. The four gates that *are* mechanical are listed in a table alongside it, so a contributor whose PR fails one knows where it lives
- The five copies of the S3 rate card are now one. Rates lived in `internal/cost/pricing.go`, `internal/cost/reporter.go`, `internal/storage/s3/tiers.go`, `internal/storage/s3/doc.go`, and `internal/analytics/model.go`; all five now read from `internal/awsrates`. This is the shape fix rather than the value fix — correcting the 10× error without it would leave the arrangement that produced the error, and #209 named the consolidation as the prerequisite that makes wiring up the real Pricing API (#183) a single-site change instead of five. `StorageTierInfo.CostPerGBMonth` is now filled in at init and **panics** if a tier has no rate, because the alternative is a zero, and a cost report showing $0 reads as free storage rather than as a lookup that missed
- `internal/storage/s3`'s package doc no longer quotes a per-GB price for each tier, and the storage-class summary table has lost its Cost/GB column. A rate in a doc comment has no way to be told it is stale, so the only question is when it starts lying. What stays is the part that is S3 *behavior* rather than S3 *price* — minimum billable size, minimum storage duration, retrieval latency — which changes when AWS changes the product, not when AWS changes a number
- `internal/cost.Reporter.Report`'s doc said to "pass 0.023" for the ROI baseline, making it a sixth place the Standard rate was written down. There is now a `StandardBaselinePerGB` derived from the rate table, so savings-versus-Standard is measured against the same rate everything else is charged at. A baseline drifting from the live rate is the hardest kind of discrepancy to notice: cost figures stay right, savings figures go wrong, and only their difference is wrong
- **No document claims a throughput figure any more.** "4.6x" appeared 24 times across 10 files — in the `internal/storage/s3` package doc, in `docs/index.md`'s performance table, in the ROADMAP's success criteria — attributed variously to BBR, to CargoShip, or to both. It is CargoShip's number for CargoShip's workload, restated as a property of ObjectFS; nothing here measured it and no benchmark here can produce it. The mechanism it was credited to is not even on the path: `internal/network`'s BBR surface (`NewBBRDialer`, `BestAvailableDialer`, `IsBBRAvailable`) has no caller outside its own tests, and what runs is a best-effort `TCP_CONGESTION` socket option on Linux and a plain `net.Dialer` elsewhere. Each site now names the mechanism, or cites `benchmarks/` and the parameters a figure would need. The 0.1.0 changelog entry keeps its figures under a banner saying they were never measured, for the same reason the withdrawn 0.10.0 entry is still here: a changelog records what was published
- `docs/index.md` gained a **Not yet wired up** table, and the features in it left the feature list. Cost tracking, archive access, the REST API, detailed per-file metrics, ML tier prediction, the Redis cache, and multi-node coordination all have code and documentation pages and **no path from a mount that reaches them**. Verified by import graph rather than by reading the code, which is what caught the two that are subtler than "no importer": `internal/analytics` *is* imported by `internal/cache`, but the `Predictor` field is never set on the mount path, so the size heuristic always runs; `internal/cache/redis` *is* selected by `cache.NewFromConfig`, but nothing calls `NewFromConfig` — the adapter constructs `NewMultiLevelCache` directly. Listing them beats deleting the pages: the code exists and may be wired up, and a reader deserves to know which column a feature is in
- `docs/ARCHITECTURE_EVOLUTION.md`, `docs/PLATFORM_STRATEGY.md`, and `docs/CARGOSHIP_MODULARIZATION_REQUEST.md` open with banners recording that they are 2025 design sketches and where the code diverged from them, in both directions. The multi-protocol path is unimplemented — no SMB, no NFS, no `internal/protocols`. The `shared/aws-optimization` module was never built; ObjectFS imports `pkg/aws/s3` and `pkg/aws/config` from CargoShip directly. CargoShip now *does* export `pkg/s3optimization`, holding different components than the one requested, and ObjectFS imports none of it. The divergence is left visible rather than edited away — the Phase 1 layout proposed `internal/filesystem` + `internal/protocols/fuse` and what shipped is `internal/vfs` + `internal/fuse`, and saying so is more useful to a reader than a document that appears to have predicted itself
- `CLAUDE.md`'s architecture line still routed through `cgofuse` and skipped both layers of the v0.10.1 refactor. It now reads `Kernel VFS → FUSE (go-fuse) → internal/fuse → internal/vfs → Adapter`, which is what the code does
- **No document calls ObjectFS "POSIX-compliant".** It appeared in `OBJECTFS.md` three times, in `docs/VISION.md` three times, in `docs/DESIGN_PRINCIPLES.md` as "✅ **Full POSIX compliance**", and in `internal/adapter`'s package doc — against roughly ten of forty VFS operations, with no rename, no links, and no xattrs. Each site now says what is true: a POSIX *interface*, with the README's supported-operations table as the authority. `OBJECTFS.md` previously carried a banner *noting* the claim was wrong while the body went on making it three more times, which is the weaker fix — a reader who skips the banner reads the claim, so the sentences no longer make it
- `docs/VISION.md` listed "Compliance (HIPAA, FISMA, SOC 2)" as a medium-term objective. Removed rather than reworded: ObjectFS holds no certification under any of the three, adds no authorization layer of its own, and writes no audit log. The heading did say these were aspirations, and that is a real defence — but a procurement reviewer scanning bullets does not read headings, and the same claim was removed from the documentation platform's landing page in v0.10.1 for that reason
- `docs-platform/guide/getting-started.md` and `docs-platform/playground/index.md` had their shell commands corrected against `objectfs --help`. Between them they opened with `curl -sSL https://get.objectfs.io | sh` and went on to use `objectfs mount`, `objectfs unmount`, `objectfs list-mounts`, `objectfs status`, `objectfs health`, and `objectfs metrics --watch` — none of which exist; the binary has no subcommands and takes two positional arguments. Also gone: an apt repository, a Homebrew tap, an AUR package, "Windows with WSL2" as a prerequisite, and two flags (`--enable-predictive-caching`, `--cost-optimization`) that are not parsed. These pages are in a tree that **cannot build** (#214), and were corrected anyway because a markdown page is read on GitHub whether or not the site renders, and a wrong `curl | sh` is something a reader will type. Their SDK code blocks are deliberately left alone, with the reason stated in-page

### Removed
- **Six of GitHub's nine default labels**, which duplicated the `type:` family and had never been used: `bug`, `enhancement`, `documentation`, `duplicate`, `invalid`, and `wontfix`, each shadowed by `type: bug`, `type: enhancement`, `type: documentation`, `resolution: duplicate`, `resolution: invalid`, and `resolution: wontfix`. Confirmed at zero issues and zero PRs apiece before deleting; the seven issues carrying the bare `enhancement` label were relabelled `type: enhancement` first, since deleting a label removes it from everything that had it. `good first issue`, `help wanted`, and `question` are kept — they collide with nothing and `.github/labels.yml` declares all three. Project board #11, "ObjectFS v0.5.0 Development", is closed for a related reason: all 14 of its items were closed issues, nothing had touched it since February, and its title named a release five versions back. `CLAUDE.md` linked it as a place to track work
- `docs/performance-metrics.md` — 618 lines describing `internal/metrics/detailed.go`, with nine verified defects: a zero-argument constructor for a function taking four arguments, a `NewDetailedPerformanceMetricsWithOptions` that never existed, three getters that do not exist, `OpMkdir`/`OpRmdir`/`OpStatfs` against the real `OpMkDir`/`OpRmDir`/`OpStatFS`, an undeclared `CacheSourceNone`, a four-argument `RecordNetworkOperation`, and two behaviours documented as working that are not. It was absent from the site nav with no inbound links. Replaced by a short section in the `internal/metrics` package doc — short so that it can stay true, which a separately-maintained 618-line description of one 600-line file cannot. Two of the nine were *code* defects rather than documentation defects, and are recorded there and filed: `P50Latency`/`P95Latency`/`P99Latency` are declared and never assigned, so anything serializing the struct publishes zeros as percentiles (which reads as a fast filesystem); and `LatencyHistogram` is indexed by `int(latency.Milliseconds()) % 100`, so 1 ms, 101 ms, and 201 ms share a bucket — a modulo, not a bucketing

## [0.10.2] - 2026-08-02

A packaging release, cut for one reason: v0.10.1 was tagged two hours before the module-path fix
merged, so the published tag still declared `module github.com/objectfs/objectfs` and
`go get github.com/scttfrdmn/objectfs@v0.10.1` failed with *module declares its path as* — the exact
defect #213 was filed for. The fix existed on `main` and in no tag, which from a user's position is
indistinguishable from not being fixed. Everything else here is the packaging and contributor-path
work that landed alongside it.

### Added
- `SECURITY.md` — a security policy, with private vulnerability reporting enabled on the repository so a finding has somewhere to go that is not the public issue tracker. It documents what a reader cannot get from the code quickly: that the trust boundary is the mounting host and ObjectFS enforces no authorization of its own, that **two unauthenticated HTTP listeners bind all interfaces by default** (`:8080` metrics and `debug` endpoints, `:8081` health) with the switch that turns each off, that `mode: off` is the encryption default and what changed after the withdrawn v0.10.0 `at_rest` key, and both stated limits of the SHA-256 read verification — a partial read is not verified, and an object with no recorded checksum verifies trivially. Every claim in it was verified by execution rather than read off the configuration schema, which is how the two listener defects below were found

### Fixed
- **The test harness could record a request after the client already had the response, so its own assertions were load-dependent.** `internal/testaws` proxies every request and logs it, and the read-path suite asserts on that log: bytes transferred and GETs issued are how read amplification and cache behaviour are measured, because neither the AWS SDK nor the emulator reports them. The log entry was appended *after* `proxy.ServeHTTP` returned — but the proxy writes the body to the socket inside that call, so a client could hold every byte of a response whose request was not yet recorded. Measured at 45–70 of 640 concurrent ranged reads. The visible symptom was in a different package: `internal/fuse` `TestShortFileIsServedFromCache` failed on its *precondition* — "the first read issued no GET" — which reads as the read path serving bytes from a cache the fixture had just created empty, in a test whose entire subject is cache correctness. One CI run in seven. Requests are now published on arrival and their response fields filled in on completion, with the accessors waiting for anything still in flight; verified in both directions, since a regression test that cannot fail proves nothing
- **The module could not be imported under the name it gave for itself.** `go.mod` declared `module github.com/objectfs/objectfs`, and the code lives at `github.com/scttfrdmn/objectfs`. Go resolves an import path by fetching that path, so `go get github.com/scttfrdmn/objectfs` failed on the mismatch between the path requested and the path declared, while the declared path is a *different project* — an unrelated Python repository from 2017, 28 stars, last pushed 2019, in a single-repo organisation created the same day. Nothing published has ever existed at the declared path, which is why `pkg.go.dev` had nothing to index and the Go Reference badge rendered empty. The path is corrected in `go.mod` and in all 154 files that named it — 132 Go files, plus the `goimports` `local-prefixes` setting in `.golangci.yml`, the `Dockerfile` image-source label, and the repository URLs in the Python and JavaScript SDK manifests, which pointed contributors at the wrong project. Verified by building an external consumer module against the corrected path, rather than by grepping for the string. **This is breaking for any code that imported the old path**, though nothing could have: it was never fetchable
- **Dependabot could not update Go dependencies, and had never merged anything.** Two unrelated defects presenting as one symptom. The Go ecosystem failed on twelve consecutive weekly runs while `docker` succeeded in the same runs — Dependabot aborts per-ecosystem, so one broken ecosystem is silent unless the run list is read. The cause was upstream and is now resolved: `proxy.golang.org` had no `.mod` for the pinned `cargoship` version, Go fell through to direct git, and git reported the proxy's 404 as `could not read Username for 'https://github.com'` — an authentication message for what was not an authentication failure, which is what sent the previous diagnosis after a credential that was never missing. Separately and more consequentially, `.github/dependabot.yml` labelled every PR `automerge` and **that label did not exist**; Dependabot drops unknown labels without reporting it, and every approve and merge step in `dependabot-automerge.yml` was gated on it, so 46 PRs were opened and none were ever merged. The label is now declared in `.github/labels.yml` alongside the four others the config names
- `.github/dependabot.yml`: `maven` and `npm` ecosystems for `sdks/java`, `docs-platform`, and `sdks/javascript`. Eight open Dependabot alerts — five against `jackson-databind`, three against `vite`, three of the eight high severity — were against manifests no ecosystem entry covered, so nothing could act on them. `sdks/javascript` is included because CI runs `npm install && npm test` there on every PR, which makes its dependencies executed code. A ceiling worth stating: the npm *security* updates still cannot apply, because neither directory commits a lockfile and Dependabot cannot determine the installed version without one (#214)
- `.github/workflows/dependabot-automerge.yml` waited on `check-regexp: (test|lint|security).*`, which is case-sensitive and start-anchored, so it matched 2 of the 9 checks CI produces and ignored `coverage`, `config-examples`, every `cross-build` matrix leg, `sdk-metrics`, `fuzz-smoke`, and `Security Scan`. The wait step is removed rather than corrected: which checks must pass now lives in branch protection on `main`, which also governs human PRs and cannot drift from a regexp in a workflow file. Native auto-merge is enabled on the repository, without which the `--auto` flag would have failed even once the label matched
- `docs-platform/docker-compose.yml` was not valid YAML. Two `healthcheck` entries put a bare URL inside a flow sequence, where the scanner reads `http` as a plain scalar and then meets `:` in place of `,` or `]`. Docker's own parser is lenient enough to accept it, so it went unnoticed — but `pre-commit run check-yaml --all-files` failed on the file, which is the first thing a new contributor runs
- `.gitignore` did not cover `coverage/`, the directory `make coverage` writes into. The three bare `coverage.*` filenames only match a profile written to the repository root, which no target produces
- `scripts/setup-hooks.sh` — the first command `CONTRIBUTING.md` tells a contributor to run — failed on any current macOS or Debian host, and exited 0 having installed nothing. Five defects: `pip3 install pre-commit` ran first and dies with `externally-managed-environment` on a Homebrew or Debian Python (PEP 668), and because the installer was an if/elif chain testing only whether each *command exists*, a failing `pip3` never fell through to the `brew` branch that would have worked; the failure happened inside a condition, so `set -euo pipefail` did not catch it; it installed gosec from `github.com/securecodewarrior/gosec`, which is a 404 (the real module is `securego/gosec`, which `security.yml` already uses); it pinned golangci-lint v1.55.2 against a `version: "2"` config only v2.x can parse, handing contributors a lint failure that looks like their fault; and it *wrote* a `.golangci.yml` if none was present, containing linters removed from golangci-lint years ago — now that a real config is committed, that branch would have overwritten it with an unusable one. Each install method is now tried until one succeeds, `pipx` first, every path verifies the command is on `PATH` afterwards, and the golangci-lint check is version-aware rather than presence-only. It also no longer overwrites `.git/hooks/pre-commit` with a hand-rolled wrapper that blocked any commit touching a line matching `fmt.Print` or `TODO`, including inside a string literal or a comment explaining why a TODO is deliberate
- `make` printed four `overriding commands for target` warnings on **every** invocation, including `make help`. `BUILD_DIR := build` and `COVERAGE_DIR := coverage` made the directory-creation rule read `bin build dist coverage:`, colliding with the real `build` and `coverage` targets. The build worked — the later recipe wins, and both are `.PHONY` — but a build system that opens with four warnings reads as unmaintained, and the names would have genuinely collided the moment one stopped being `.PHONY`. Replaced with a `%/.mkdir` sentinel rule, which keeps the pattern out of the target namespace, declared as an order-only prerequisite so writing one binary does not rebuild its siblings
- `pre-commit run --all-files` could not complete: the `pretty-format-yaml` hook crashed on import, because the pinned rev imports `pkg_resources`, which modern setuptools no longer ships (Python 3.14 here). Bumped to a rev that does not. Fixing it exposed a second problem worth recording, since the obvious repair is the wrong one: `check-yaml` is PyYAML and follows YAML 1.1, where a bare URL in a *flow* sequence is a syntax error, while `pretty-format-yaml` is ruamel and follows YAML 1.2, where it is legal — so quoting the URL to satisfy the first makes the second strip the quotes straight back off, and the two hooks disagree forever. The `docker-compose.yml` healthchecks are now block sequences, the one form both parsers accept and where there are no quotes left to remove
- `.golangci.yml` is excluded from the `pretty-format-yaml` hook, which damages it: the formatter dedents every block sequence to its parent's column, destroying the nesting that makes `linters.settings.<name>` readable and pushing each trailing comment out of alignment with the entry it annotates — and those comments are the recorded reason each linter is on or off. It also unquotes the `path:`/`text:` regexes, leaving `fuzz_.*_test\.go` bare where the escapes were visibly protected. Both forms are valid YAML and golangci-lint parses either; the file is configuration a human has to read, and the formatter optimises for neither
- `.markdownlint.yaml`: MD010 (`no-hard-tabs`) now exempts code blocks. The hook runs `markdownlint --fix`, and at the rule's default the fixer silently rewrites tabs *inside fenced blocks* to spaces — so every Go sample in the repository was being converted to something `gofmt` would immediately change back. It had already done this to the `s3.NewBackend` example in `README.md`, on a commit that only touched badges. Tabs in prose are still flagged, which is the part of the rule worth having
- The security issue template pointed at "Security → Advisories → New draft advisory" as prose rather than a link, on a repository where private reporting was not enabled — so the instruction was both unfollowable and, until now, describing a feature that was off. It now links directly to the advisory form and to `SECURITY.md`

### Removed
- `README_CROSS_PLATFORM.md` and `RELEASE_NOTES_v0.1.0.md`. The first documented installing and running an `objectfs-windows.exe` and stated the filesystem "works on Windows/macOS/Linux"; Windows is unsupported, there is no Windows binding, and `make build-all` deliberately builds linux and darwin only. Nothing linked to either file
- Three README badges that reported nothing. `codecov` rendered "unknown" against a service nothing has ever uploaded to (coverage is gated per-package by `scripts/coverage-gate.sh` in CI, which the CI badge already covers), Go Report Card rendered "retired", and Go Reference rendered empty because `go.mod` declared a module path that was not where the code lives (fixed below, #213). A badge that renders "unknown" reads as a broken project to anyone who looks at it and as a passing check to anyone who does not

## [0.10.1] - 2026-08-02

Every entry below is user-facing, and the release is almost entirely one thing: the defects a deep
audit of v0.10.0 found, and the harness that would have caught them. v0.10.0 is withdrawn.

Four defects in v0.10.0 were verified by execution to lose or corrupt data, and one prevented the
shipped default configuration from mounting at all. They were not independent — they clustered in
three subsystems whose designs could not express what they were asked to do, which is why this
release adds `internal/vfs` rather than patching six call sites. The write path could not represent
an offset write, the read cache could not hit as keyed, and the FUSE node layer was missing most of
its contract.

The reason 32,680 lines of tests across 90 files caught none of it: every one was a *seam* defect —
a value correctly produced at one layer and silently dropped at the boundary to the next. A mock on
the far side of a seam agrees with its caller by construction. `internal/testaws` and
`internal/difftest` exist to remove that blind spot.

### Added
- `internal/vfs` — a backend-agnostic POSIX-semantics core, and the layer whose absence generated a whole class of v0.10.0 defects. It owns the attribute model, the handle table, dirty-range tracking, and the read-modify-write policy, returns typed errors (`vfs.ErrNotFound`, `vfs.ErrIntegrity`, …) rather than `syscall.Errno`, and depends on nothing FUSE. `internal/fuse` becomes a translation shim above it. The point is that POSIX semantics are now **testable without a mount**, which the previous design forbade: attributes could not persist because no type owned them, offset writes could not be buffered because no range model existed, and the go-fuse mode backstop was left disabled because nothing owned the default
- `internal/vfs.ExtentList` — an interval list of pending writes, replacing the single contiguous buffer plus offset that could not represent an offset write. It accepts any write at any offset, coalesces overlaps so later writes win, tracks truncation as a watermark rather than a flag, and produces a `FlushPlan` naming exactly which byte ranges of the stored object must be fetched to assemble the new one. A truncation records two sizes, not one: a boolean cannot express "truncate to 4, then write at 83," which must leave bytes 4–83 a hole rather than resurrecting the deleted content
- `internal/vfs.Attr` — one type owning size, mode, ownership, times, ETag, and the stored SHA-256, with round-trippable S3 user-metadata encoding. Metadata keys are matched case-insensitively, because S3 lower-cases them, MinIO title-cases them, and an `http.Header` round-trip canonicalises to `Objectfs-Mode` — a case-sensitive lookup passes unit tests and fails against real storage. Malformed metadata falls back to defaults rather than erroring, so setting `objectfs-mode` to `banana` cannot make a file unreadable; `MetadataWarnings` reports what was ignored
- `internal/vfs.Node` / `HandleTable` — per-path state shared by every handle open on it, because one path is one S3 object. `MarkFlushed` takes the generation counter captured when the flush was planned and refuses to clear pending state if a write landed during the upload, which is the specific v0.10.0 data-loss path where a write concurrent with a flush was discarded and accounted as flushed. `Release` keeps a dirty node rather than dropping it, and `Forget` refuses a node that is still dirty or still open — a caller whose flush failed must not be able to make the failure disappear
- `internal/vfs.Flusher` — a flush that does genuine read-modify-write, which is the concept v0.10.0 was missing and the reason six of its defects were one defect. S3's `PutObject` replaces an entire object while the POSIX contract is "modify a byte range," and nothing reconciled the two. The protocol is: capture the node's generation, take a flush plan, fetch the ranges of the stored object the pending writes do not cover, splice, upload, then mark flushed **with the generation from step one** — so a write that landed during the upload is not in the body that was sent and cannot be cleared. It reads the stored object back with a `HeadObject` and fails on a length mismatch rather than trusting the length it sent, because a backend that stores fewer bytes than it was handed is the one corruption a write path cannot detect by looking at itself. The race-retry loop is **bounded** at eight attempts and reports non-convergence: spinning forever inside `close(2)` is worse than saying writers are arriving faster than an upload completes, since a caller can retry but cannot recover from a syscall that never returns
- `internal/vfs.Writer` — the `types.WriteBuffer` implementation, one `Node` per path. `Flush` is synchronous and returns an error, `FlushAll` attempts every key after one fails and reports the first failure with a count of the others (it is the path unmount takes, and stopping at the first error would leave later keys unflushed with no indication which), `ReadAt` overlays pending writes on the stored object so a read observes a write on the same descriptor, and `FileSize` reports the logical length including pending writes rather than the object's stored size. It also distinguishes absence from every other backend failure at four separate seams — a `HEAD` that 404s is a zero-length file, a `HEAD` that throttles is `ErrBackend`; a ranged `GET` that 404s mid-flush is a hole spliced as zeros (a concurrent delete by another client is legitimate, and failing would strand the write), a ranged `GET` that throttles is not, because splicing zeros there would corrupt intact data. `Writer.Truncate` and `HandleTable`'s `O_TRUNC` handling exist here but are **not yet reachable through a mount**: `internal/fuse` implements no `Setattr`, which is its own audit finding, so `> file` still cannot shorten an object until that lands (#165). Stated rather than left to be discovered
- Reference-model property tests and three fuzz targets (`FuzzExtentList`, `FuzzAttrFromMetadata`, `FuzzNodeLifecycle`) for `internal/vfs`, at 98.4% statement coverage. Each asserts the implementation against a deliberately naive `map[int64]byte` model of the file, so a test cannot ratify the same off-by-one the implementation has. This found two real design defects in `ExtentList` that thirty-odd hand-written table cases had all passed: a truncate-then-write-past-the-end sequence re-fetched the whole stored object and resurrected deleted bytes, and a sparse read could not distinguish a hole inside the file from a read past EOF, returning zero bytes instead of zeros
- `internal/testaws` — a test harness that runs the real S3 backend against a real S3 endpoint (a [substrate](https://github.com/scttfrdmn/substrate) in-process emulator) over real HTTP, with no network, no credentials, and no AWS account. It exists because the v0.10.0 audit found roughly forty-five defects that 32,680 lines of tests across 90 files had all missed, overwhelmingly because they were *seam* defects — a value produced correctly at one layer and dropped at the boundary. A mock on the far side of a seam is a restatement of what the caller believes, so it agrees with the caller by construction. The harness paid for itself immediately, surfacing a live silent-corruption path on the shipped default configuration within an hour of first use
- `internal/testaws`: a byte-counting recording proxy in front of the endpoint, reporting per-key request counts, `Range` headers, response status, and **bytes transferred**. Read amplification is a byte-count property and neither the AWS SDK nor the emulator's event store reports one, so the alternative was asserting on latency — which would make the decisive read-path test a flaky proxy for the thing it means to measure. The audit measured a 4 KiB read of a 256 MiB object taking 49 seconds, but the defect is that it transferred 256 MiB, and that is what a regression test has to pin
- `internal/testaws`: the recording proxy releases its upstream connections before the emulator shuts down, which took five seconds off the teardown of every fixture that had lost a dial race. `http.Server.Shutdown` polls until each connection is idle or closed, and net/http grants one in `StateNew` — dialed, no request yet sent — a hardcoded five-second grace ([go.dev/issue/22682](https://go.dev/issue/22682)); the proxy's transport parks exactly such a connection whenever concurrency opens a second one the winner did not need. Measured on a 16 KiB read traversal: **5.44 s, of which 7 ms was the test** — and it came and went with the test's pacing rather than with anything it asserted, which is how a fixture cost gets attributed to the code under test. The whole repository's suite now runs in the time `internal/fuse` alone used to take
- `internal/compression/gzip.go`: a gzip codec, so `compression.algorithm: gzip` works instead of failing at mount. It was declared in `pkg/compression`, named as the write-buffer default, documented in the S3 config comment, and set in a shipped example config — with no implementation anywhere. gzip is slower and compresses worse than zstd at every level, so it is not recommended, but it is the one algorithm whose objects stay readable outside ObjectFS: `gzip` is a registered HTTP content coding, so browsers, `curl`, and any client sending `Accept-Encoding` decode it, whereas a zstd or lz4 object fetched with `aws s3 cp` lands on disk as an opaque frame. Verified against the standard library in **both** directions, because reading back through the same codec that wrote cannot detect a non-standard encoding
- `pkg/compression`: `SupportedAlgorithms` and `SupportedAlgorithmNames` — one authority on which algorithms exist, declared beside the constants so that adding a constant without an implementation is visible. A test walks the list and round-trips every entry through its codec, so an algorithm can no longer be advertised by the type system and rejected by the factory. It is deliberately an enumeration and not a validator: callers with a user's configuration in hand should build the codec instead, since a name check cannot catch a level the chosen algorithm will refuse
- `pkg/errors`: `ErrCodeDataCorruption` — a distinct, **never-retryable** error code for "the bytes on the wire cannot be reconciled with what the object says it holds." Retrying a corrupt read fetches the same bad bytes and only delays the report, so the code is excluded from `IsRetryableByDefault` and is registered in all six of the package's classification tables (HTTP status 422, user-facing, category, severity, and both string maps). A code missing from any one of them degrades quietly rather than failing to compile
- `internal/difftest` — a differential-testing oracle that runs one operation sequence against ObjectFS and against the local operating-system filesystem and asserts they agree on what each read returns, on the size each reports, and on the bytes that end up durable. The reference is not a model of POSIX; it is POSIX, which is the point: a hand-written expectation can encode the same misunderstanding as the implementation, and when it does, the test passes and says nothing. This is the mechanism the v0.10.0 write-path defects needed. None of them were subtle in effect — appending one byte to a 1 MiB file left a 1-byte object — but each was invisible to a unit test, because every layer was tested against a mock of its neighbour and the mock restated what the caller believed. The bug lived in the disagreement between layers, which is exactly what a mock removes. The oracle compares content, size, and durability only, and deliberately not errno values: ObjectFS may refuse an operation the local filesystem accepts, but it must refuse loudly, so an unexpected error is a failure with the same weight as wrong bytes
- `internal/difftest.Shrink` — reduces a failing program to a minimal one, which is what makes fuzz output usable: a 200-operation counterexample says nothing a human can act on, and the three-operation one hiding inside it usually says everything. It emits the survivor as pasteable Go. Its own test asserts the reduced program still fails on **the property that was planted**, not merely that it still fails — a shrinker that walks a counterexample into some unrelated divergence is worse than none, because it produces a confident bug report about a bug nobody saw. That check earned its keep on first contact: shrinking a 44-operation program reduced it to a single one-byte write and reported a *health* divergence rather than the durability defect the program contained, which turned out to be two real bugs (a missing object counted as a service failure, and `unavailable` having no recovery path), both now fixed and pinned. It also caught an error in the test's own strengthened assertion — the shrinker had found a strictly better counterexample than the two writes the author planted, and demanding the author's shape over the shrinker's is the wrong direction
- `internal/difftest.Legacy` — the write path v0.10.0 shipped, kept as a calibration target so the oracle has demonstrated teeth on real defects before it is trusted to guard against new ones. All seven of the audit's proven data-loss sequences diverge on their own named property at their own named operation; `Local` compared against itself never diverges. It is deleted along with that write path
- Four fuzz targets over the seams the v0.10.0 audit found defects in, with committed corpora so each one doubles as a regression test: `FuzzOperationSequence` (operation streams through the differential oracle), `FuzzSliceRange` and `FuzzGetObjectRange` (the full offset/size domain, negatives included), `FuzzConfigConstructsBackend` (YAML → `buildS3Config` → `NewBackend`, audit finding C1's seam), and `FuzzRoundTrip` (arbitrary bytes through compress → PUT → GET → decompress, asserting byte identity, the reported size, the recorded SHA-256, and that a ranged read agrees with the same slice of the whole). Three of the five found real shipped defects on their first run, all listed under Fixed below. Each target's corpus is committed selectively rather than wholesale — `FuzzRoundTrip` alone accumulated 32 MB of cache entries, several over 5 MB, so entries above a per-target size cap are left in the cache where the fuzzer can still use them
- `FuzzChunkAssembly`, with a committed 54-entry corpus — the end-to-end property for the re-keyed byte-range cache: whatever sequence of `Put`s lands, a `Get` that reports a hit must return exactly the bytes an authoritative copy holds at that range. It is the only check that covers splitting, coalescing, coverage testing, and `Get`'s assembly loop as a system, and the defect class it guards — bytes returned from the wrong offset — arises from their interaction rather than from any one of them. The two `Put`s read from **different versions** of the object and the assertion is against the newer, which is the whole design of the target: an earlier version sliced both from one buffer, so wherever the two writes overlapped they were byte-identical by construction, the overlap comparison could never disagree, and keeping the older run was indistinguishable from keeping the newer. Confirmed by mutation — with that comparison deleted, 3.3M executions passed while a three-case unit test failed immediately. Two inputs generated from one formula cannot detect a discarded input. The property is stated as three separate prohibitions, because a cache is allowed to do two of the three things that look like failure: it may miss, and it may be stale outside the range of the newer write (nothing has told it those bytes changed, and bounded staleness is what invalidation and the TTL are for), but it may never return a byte belonging to neither version, and it may never return the pre-write byte inside a range a later `Put` has just supplied
- Integrity tests for the SHA-256 read verification, which corrupt objects *behind* the backend with a raw SDK client so the bytes ObjectFS reads genuinely differ from the bytes that were hashed — bit flip, truncation, extension, and wholesale replacement at identical length, the case no size or length check can see. Each was confirmed non-vacuous by mutating the guard back and watching the right subtests die. `FuzzWholeObjectResponse` covers the `Content-Range` parse that gates the check, and found two real looseness bugs on its first run: a negative body length was accepted as a whole object, and `strconv.ParseInt` let `+4096` through where RFC 9110 spells the complete-length as `1*DIGIT`. One earlier version of the case-insensitivity test is worth recording as a caution — it seeded four metadata spellings through a client and asserted each was detected, and it *passed against a case-sensitive lookup*, because S3 and the emulator both lower-case keys in transit so all four arrived as one. It would have certified the exact failure it was written to prevent. The lookup is now tested directly, where the response map a foreign server produces can be constructed
- `internal/awsname` — a leaf package with one job: deciding whether a string is a syntactically valid AWS region. It exists as its own package because `pkg/types` aliases `internal/config` types and `internal/storage/s3` imports `pkg/types`, so the config layer cannot import the S3 layer, and the two layers that both act on the region had nowhere neutral to share a check. That is the same structural gap that produced C1, where config could not validate a compression algorithm by building the codec because the dependency ran the wrong way. It is deliberately a syntax check and not a list of known regions: a list compiled into a build rejects regions AWS launches after it. `RegionIsResolvable` is a second, separate question — not whether the string is well-formed but whether, given an empty one, the environment holds anything to fill it in — and it is separate because collapsing the two would mean either rejecting the legitimate empty region or accepting an unresolvable one. It stops short of parsing the shared config file: reproducing the SDK's profile precedence and getting it subtly wrong would refuse a configuration that works, so a non-empty file is taken at its word
- `internal/testaws.Shared` / `SharedServer` — a process-lifetime S3 endpoint for fuzz targets. `StartTestServer` releases its server through `t.Cleanup`, which is right for a test and wrong inside `f.Fuzz`, where the `*testing.T` is per-iteration: measured at 49 executions in 24 seconds before the ephemeral port range filled with `TIME_WAIT` sockets and the run died reporting a harness failure as a finding. One endpoint per process, with isolation coming from the bucket instead — the same boundary real S3 has. `BackendOn` additionally lets two differently-configured backends share one bucket, which is the only way to express a reconfiguration, and therefore the only way to test audit finding C2
- `internal/testaws`: runtime capability probing with loud skips. The emulator implements a subset of S3 and the subset moves, so `Capabilities()` establishes what is actually there by doing it, and `RequireRangeGET`/`RequireMultipartContentEncoding` **skip** a test whose capability is absent rather than letting it pass. This is the difference between a harness that reports the truth and one that ratifies whatever it is given: against an endpoint that ignores `Range`, every ranged-read assertion passes for the wrong reason, because the whole object contains the requested bytes. The probes are pinned to observable behavior rather than to a dependency version, so they stay correct whichever way the dependency moves — a test gated on a hardcoded version starts skipping for the wrong reason the moment it is bumped
- `.golangci.yml` — the first lint configuration this repository has had. `GOLANGCI-LINT.md` documented one and instructed contributors to run against it, and no such file existed anywhere in the tree, so every "make lint" run used the tool's bare defaults. It analyses test files, which is not the default-adjacent choice it looks like: the audit found the test suite was the least trustworthy code in the repo, and a discarded error in a test is how a test passes without asserting anything. Three checks are disabled with the evidence recorded in the file rather than silently omitted, and in each case the evidence is that they were run and their output read. `errorlint` rewrites `err.(*errors.ObjectFSError)` into `errors.As(...)`, which does not compile here because `pkg/errors` shadows the standard library's `errors` and exports no `As` — the finding is usually right and the fixer breaks the build. `dupword` flagged four things, all false, and its fixer deleted a token from test data: `want: "F F S"` in `internal/difftest` is a compact program notation (flush, flush, stat) that it rewrote to `"F "`, leaving a test asserting that a three-operation program decodes to one. `govet`'s `shadow` produced 23 findings of which 23 were `if err := f(); err != nil` — the scoped form the standard library uses throughout, and the form that *prevents* the bug it exists to find; it reported nothing at the shape that is dangerous. A check whose every finding is a false positive stops being read, and once it is not read it costs more than it detects
- `scripts/coverage-gate.sh` and `.coverage-floors` — a per-package statement-coverage floor, enforced in CI. A repo-wide number lets a well-tested package pay for an untested one, and the aggregate is precisely the figure that looks acceptable: this repo sits near 70% overall while `internal/fuse`, the layer every POSIX operation passes through, is at 17.7%. Averaging those hides the one thing worth knowing. The 30 floors start at what each package measures today, so the gate is green on the commit that introduces it and every floor is a ratchet — raising one is deliberate and lowering one has to be defended in a diff. A package with no floor is named in the output rather than silently ungated, and a floor whose package produces no coverage data **fails**, because a floor nothing measures has quietly stopped gating
- `.github/workflows/ci.yml`: seven jobs replacing the single `go test -race -short ./...`. The full `-race` suite without `-short`, the coverage gate, `golangci-lint` against the new config, a 60-second-per-target fuzz smoke run, a cross-build matrix that **fails** rather than echoing (several `Makefile` targets swallowed failures with `|| echo "Failed..."`), a job that loads every shipped config file, and `sdk-metrics`, which runs both SDK test suites. The test and lint jobs run with no AWS credentials and with the instance-metadata endpoint disabled, so a test that silently depends on the developer's ambient profile fails in CI instead of passing locally and nowhere else
- The `sdk-metrics` job closes the consumer half of the fixture contract, without which the contract is only half-enforced: a renamed metric fails the Go fixture test, someone regenerates the fixture to make it pass, and the SDK extractors that no longer match it stay green because nothing runs them. Verified by doing exactly that — renaming `cache_requests_total` to `cache_lookups_total` fails `TestSDKFixtureMatchesTheLiveScrape`, and the regenerated fixture then fails 5 Python and 6 TypeScript tests. The Python step installs the package rather than a hand-listed subset of its requirements, since `objectfs/__init__.py` imports the client and so the whole `install_requires` set has to resolve; that also makes this the only job that fails if `setup.py` stops being installable
- `internal/testaws`: `Fault.QueryKey`, matching on the presence of a query parameter — the dimension that distinguishes the sub-operations of a multipart upload, which method and path cannot. `CreateMultipartUpload` and `CompleteMultipartUpload` are both a `POST` to `/bucket/key`, differing only in `?uploads` versus `?uploadId=…`, so a fault aimed at Complete by method and path fires on the *create* instead — and since a failed create starts no upload, a test asserting "the failed upload left no orphaned parts" then passes because there was never anything to orphan. That is not hypothetical: it is how the field came to exist. The H10 regression test below was green, and a mutation that deleted the abort it was written to guard left it green. Matching is on presence rather than value, because an upload ID is generated per upload and a test cannot know it in advance
- `internal/awsname.SSEModes`, `ValidateSSEMode`, and `ValidateKMSKeyID` — the encryption mode set and the KMS key-form check, in the package that exists so that one fact has one authority. The loader reads them and the S3 backend's `EncryptionMode*` constants alias them, so a mode cannot be accepted by config that the backend has never heard of. `ValidateKMSKeyID` accepts the four forms S3 accepts (key ID, key ARN, `alias/name`, alias ARN) and rejects the shapes that only resemble them — notably a well-formed ARN for the wrong service, which S3 answers with a key-not-found complaint that never mentions the service field. Both are fuzzed for totality and for the property that anything accepted is fit to be interpolated into a request header
- `internal/testaws`: `Request.Header` captures the headers each request actually carried, and `ts.Writes(key)` returns every write (`PUT` and `POST`, a `CopyObject` being a `PUT` with `x-amz-copy-source`) for a key. Server-side encryption is a property whose *only* observable is a request header: the emulator models SSE nowhere, so an object written with the header reads back byte-identical to one written without it. Asserting on the object's bytes therefore passes with no header sent, and asserting on the SDK input struct only checks the arithmetic of the line that filled it in — the recorded request is the one claim that can fail for the right reason. The header map is cloned on capture, because the proxy hands the live map to the transport, which mutates it
- `internal/config`: `TopLevelKeys` reports the YAML keys the schema accepts, read by reflection over the struct tags so it cannot fall out of step with `Configuration` — the same class of drift that produced the shipped-config divergence below. It is what the loader's rejection message lists, and a test asserts every key it returns appears in that message
- **`chmod`, `chown`, `utimes`, and `truncate` work, and persist.** `internal/fuse` implemented no `Setattr` at all, so go-fuse's default answered every SETATTR with `ENOSYS` — `chmod 600 f` failed on a mounted filesystem, and there was no code path by which a mode could reach S3 even if it had not. `FileNode.Setattr` now applies exactly the fields the request's FATTR bitmask names and no others, which is the whole substance of the operation: applying all of them unconditionally would have `touch` reset a file's mode to whatever the caller's zero value happened to be. Mode, ownership, and mtime are persisted as `objectfs-mode`, `objectfs-uid`, `objectfs-gid`, and `objectfs-mtime` user metadata and read back on the next `stat`, so they survive a remount. The size arm is also the `O_TRUNC` path: `CAP_ATOMIC_O_TRUNC` is never negotiated, so the kernel implements `O_TRUNC` as a SETATTR carrying `FATTR_SIZE=0` before the open completes, which means `> file` shortening an object and `truncate(2)` are one implementation (#165)
- **`fsync(2)` makes data durable before it returns, which `docs/architecture/overview.md` has claimed since v0.4.0.** There was no `Fsync` anywhere in the package, so the kernel's answer was `ENOSYS`; a database or any program that relies on `fsync` for ordering was running against a filesystem that could not provide it. It is implemented on the node rather than on the handle, so it covers `fsync` on a descriptor and on a path, and it flushes through the same synchronous path `close(2)` uses — including the metadata verification below (#166)
- **`statfs(2)` reports a real filesystem rather than a zeroed struct.** `rawBridge.StatFs` leaves the `StatfsOut` entirely zeroed and returns success when the node implements nothing, so `df` printed a filesystem of zero blocks with a zero block size, and on darwin a zero block size is not merely uninformative — go-fuse's own documentation states an OSX filesystem must implement `Statfs` or the mount will not work. Object storage has no capacity to report: a bucket has no quota to read, no free-space figure, and no inode count, and the figures are therefore synthetic and documented as such. One pebibyte, large enough that no caller mistakes the mount for full and small enough that arithmetic on it cannot overflow (#166)
- **Server-side encryption is requested on every write, from a `security.encryption` block that means what it says.** `mode` selects `off`, `sse-s3`, or `sse-kms`; `kms_key_id` names the key; `bucket_keys` enables S3 Bucket Keys. The headers are applied on all four write paths — `PutObject`, the `CreateMultipartUpload` that begins a large upload, and both `CopyObject` call sites (an attribute change, and an automatic storage-tier transition). One resolver produces the header set and three one-line appliers assign it, rather than a `switch` per path: four places to forget is four chances to forget differently, and the zero value resolves to "send nothing," so every path applies it unconditionally and there is no arm to omit. The multipart header goes on the create and nowhere else, because S3 records the encryption for the upload as a whole and rejects an `UploadPart` that restates it. The copy paths matter more than they look: **a copy does not inherit the source object's encryption** — S3 encrypts the destination per the request, and a request that says nothing gets the bucket default — so before this, a `chmod` on an SSE-KMS object rewrote it under the bucket default, and a tier transition did the same on a timer with no operation to attribute it to (audit finding P-7)
- **`DirectoryNode.Getattr`** — see the mode-0000 fix below. Its absence was the single defect that made v0.10.0 unusable as a filesystem
- `internal/fuse/errno.go`: one translation from a `vfs` error to an `errno`, in one place. Every node method routes through it, so a new error classification is added once rather than at each of the twenty-odd sites that must map one. `ErrIntegrity` maps to `EIO` and not to anything softer: an integrity failure means the bytes or the attributes in storage cannot be reconciled with what was sent, and the only honest answer to a caller is that the I/O did not happen
- `internal/vfs.AttrFromMetadataWithDefaults` — attribute decoding that falls back to caller-supplied defaults rather than to package constants, which is what lets the mount's configured uid, gid, and mode apply to objects that carry no ObjectFS metadata. An object written by `aws s3 cp` or boto3, or one that predates ObjectFS, has no mode to read; answering with a hardcoded 0644 owned by uid 1000 is what v0.10.0 did, and it ignored `MountConfig.Permissions` entirely
- `internal/fuse`: 60 tests covering `Getattr`, `Setattr`, `Fsync`, `Statfs`, `Lookup`, `Readdir`, `Mkdir`, and `Create` against a substrate-backed S3 endpoint, raising the package from 37.7% to 56.6% and its coverage floor from 37 to 56. The mechanism is the structural change rather than diligence: the package is now a translation shim over `internal/vfs`, so its behaviour is reachable by calling a node method and asserting on what is in S3, where previously the only way in was through a mount. What remains uncovered is what a unit test cannot reach — the mount and unmount lifecycle, the signal handling around it, and the go-fuse option construction — which is the live integration suite's job and is stated in `.coverage-floors` rather than left as an unexplained gap
- `internal/testaws`: a `MetadataDirective=REPLACE` capability probe, and `RequireMetadataReplace` to skip on its absence. S3 has no metadata-update operation, so changing an object's attributes in place is a `CopyObject` self-copy with that directive — a compound operation with a silent no-op mode, since an endpoint that ignores the directive answers 200 and carries the old metadata forward. Three attribute-persistence tests would otherwise have failed against such an endpoint for a reason that is not ObjectFS's, and the skip message says which direction the failure lies in so it cannot be mistaken for a passing test. The pinned substrate honors the directive as of v0.82.0, so the probe now passes and those three tests run; it is kept rather than deleted because the same tests are meant to run against MinIO, Ceph, and Wasabi, where the answer is not known in advance
- `internal/testaws`: the recording proxy has its own `http.Transport` instead of the nil that means `http.DefaultTransport`, fixing a flake that blamed the wrong test. `httptest.Server.Close` calls `http.DefaultTransport.CloseIdleConnections()` unconditionally — the standard library says outright that it is doing this to "help out" users of the standard transport — so with the transport shared, one fixture's teardown broke in-flight requests belonging to every other fixture. Tests here are parallel and each holds its own endpoint, so this surfaced as `http: CloseIdleConnections called` roughly one run in six, attributed to whichever test happened to have a request open at the moment an unrelated one finished. Worth recording rather than fixing quietly: a harness that fails intermittently and names the wrong test is worse than one that fails consistently, because the reflex is to rerun it
- `internal/testaws.ObjectStorageClass` — a key's storage class read from the endpoint, normalizing the absent header to `STANDARD`. S3 "returns this header for all objects except for S3 Standard storage class objects", so an empty value is not unknown, and a test that treated the two differently would report a tier demotion as a missing header. This is the assertion that found the storage-class defect below
- `internal/storage/s3`: tests for the two operations the node contract added to the backend — an attribute-only write and a listing that follows continuation tokens — plus a dedicated pair for the configured storage tier, raising the package from 70.6% to 73.8% and its floor from 71 to 73. All of them assert on what the endpoint recorded rather than on a return value, because both operations are seams in the strict sense: `SetObjectMetadata` is judged by whether the object was rewritten (asserted as bytes on the wire, not as a request count) and by whether `Content-Encoding` and the storage class survived the `REPLACE`, and pagination is judged at 1001 keys, one past the boundary where a paginating implementation and one that stops still agree
- `internal/awsname`: `StorageClasses` and `ValidateStorageClass` — one authority on which S3 storage classes exist, and the class names themselves. `internal/storage/s3`'s `Tier*` constants are now aliases of them, and a test asserts the backend's billing table covers exactly the set the validator admits, so the table cannot grow a tier the loader rejects and the loader cannot admit one with no billing entry. It lives in the leaf package for the same reason the region check does: `storage_tier` is read by `internal/config` and acted on by `internal/storage/s3`, and the config layer cannot import the S3 layer
- `internal/config`: `storage.s3.storage_tier`, `storage.s3.max_retries`, `storage.s3.use_cargoship`, and a `storage.s3.multipart` block (`threshold`, `chunk_size`, `concurrency`). Also `OBJECTFS_S3_STORAGE_TIER`. These are the keys the backend has always had fields for and configuration had no way to set
- `internal/config`: `Validate` now rejects an invalid `storage_tier`, a negative `max_retries` or `multipart.concurrency`, a multipart chunk size larger than the multipart threshold, and any unparseable size in the ten size-valued settings — naming the YAML path in every message. Each of those was previously accepted and then silently reinterpreted: an unrecognized storage class became STANDARD inside `NewTierValidator`, a chunk size above the threshold made the first part of every upload the whole object so multipart never engaged, and an unparseable size became 1 GiB. The messages name the path rather than the Go field because the operator's next action is to edit one line in one file, and "invalid configuration" does not say which
- `internal/storage/s3`: `Config.CircuitBreaker`, carrying `enabled`, `failure_threshold`, and `timeout` from `network.circuit_breaker`. It is plain data rather than a `circuit.Config` because that type expresses the trip decision as a func field, which cannot be compared, printed, or round-tripped through YAML; `NewBackend` turns the three values into the predicate. `enabled: false` becomes a predicate that never trips rather than a bypass, so the breaker still counts and reports state — a second code path around every S3 operation with no coverage is worse than a counter that never fires. A zero threshold deliberately maps to `nil`, which selects the package's proportional default (20 requests in the interval with half failing); returning a `failures >= 0` closure instead would open the breaker before the first request and reject every operation for the life of the mount
- `internal/adapter`: `TestBuildS3ConfigMapsEveryConfiguredValue`, which asserts the mapping's **output values** against literals rather than recomputing the mapping. That distinction is the test: a check written as `want := utils.ParseBytes(cfg.…ChunkSize)` agrees with any formula, including one that reads the threshold into the chunk size, which is exactly the mistake a thirty-field mapping invites. Spelling `16MB` as `16777216` makes it fail when the mapping is wrong rather than when it is different
- `internal/storage/s3`: seam tests for the four values the mapping newly carries — a batch operation completing with an unset pool size, the parallel-read threshold actually driving fan-out (asserted on the range GETs the endpoint recorded), the storage tier reaching the stored object, and the breaker config reaching the breaker. All four assert against the endpoint rather than a return value, for the reason the storage-tier defect demonstrated: every layer agreed on the value while what arrived at S3 was something else
- `sdks/testdata/metrics-scrape.txt` — an executable contract between the Go exporter and its two SDK consumers. It is a real `/metrics` response captured from `internal/metrics.Collector`, not a hand-written sample: `TestSDKFixtureMatchesTheLiveScrape` regenerates it from a live scrape and compares on every Go test run (`-update-fixture` to accept a change), and both SDK suites parse the same file. So renaming a metric or dropping a label fails the Go suite, and the corrected fixture then fails the SDK suites in the same commit. The absence of that link is the whole reason the SDK parsers were broken — nothing anywhere compared what the SDKs expected against what the server produced, so two independently wrong halves sat in a shipped release. The recorded observations are fixed values chosen so the assertions can be sharp: three hits and one miss make a hit rate of **0.75**, which a broken parser cannot produce by accident the way 0.0, 0.5 or 1.0 can
- `sdks/python/tests/test_monitoring.py` (34 tests) and `sdks/javascript/src/prometheus.test.ts` (36 tests), both reading that fixture, and both asserting the same numbers off it case for case — two independently written parsers agreeing on one captured scrape is a stronger claim than either alone, and a divergence between them is a real defect in one of the two SDKs. Coverage includes the cases that actually broke: a label block separated from its name, a comma inside a label value, an escaped quote, a `}` inside a value, the `+Inf` bucket, a trailing timestamp, scientific notation on a float gauge, repeated names both kept, and the README's own example. Each suite was mutation-tested by restoring the first-space split — forcing the label-block search to miss, so the name absorbs its own labels — which fails 22 of 34 Python tests and 24 of 36 TypeScript tests, the README example among them. Both suites run under the runner each SDK already declares: `cd sdks/python && pytest tests/` and `cd sdks/javascript && npm test`. The TypeScript suite asserts with `node:assert` rather than jest's `expect`, because `prometheus.ts` deliberately imports no transport and the tests deliberately depend on nothing but the standard library and the module under test — jest supplies the harness and typechecks the file through `ts-jest`, but is not load-bearing for anything asserted
- `sdks/javascript/src/prometheus.ts` — the scrape parsing and metric extraction as free functions with no transport dependency, exported from the package index. That is what makes them testable: the code lived inside `MetricsCollector` beside an axios client, so exercising it meant standing up a server, and nothing did. The module typechecks clean under `strict`, `noUncheckedIndexedAccess` and `exactOptionalPropertyTypes`
- `internal/adapter/metrics_wiring_test.go` — three tests that scrape the endpoint over a real socket rather than inspecting `a.metrics`, because that distinction *is* the defect: a collector with no listener is indistinguishable from one with a listener by any field on the struct. They cover the missing `Start` call, the empty namespace, and the unread custom labels over the wire, and assert that a disabled configuration binds *nothing* — without which a `Start` that bound unconditionally would satisfy the other two. Deleting the `startMetrics` call from `Start` fails them with `connection refused — the adapter bound no metrics listener`
- `internal/config`: `TestNoDocumentRestatesTheCurrentVersion`, which fails when any document asserts what version ObjectFS currently is. The repository gave five different answers at once — 0.10.0 in the code, 0.7.0 in `CLAUDE.md`, v0.3.0 in `ROADMAP.md`, v0.2.0 in two more places — and none of them was a lie when it was written. Each was correct at the moment someone typed it and then silently became false, because prose has no mechanism for noticing that it is stale. Correcting the four numbers only resets that clock; the fix is one authority plus a test that fails when a second appears. The test flags claims about the present, not the presence of a number: `v0.6.0 — multi-protocol support` is a plan and passes, `**Current Release:** v0.3.0` does not. It matches three written forms rather than one because the first pattern found three stale claims and widening it found four more, in files that had passed — a narrow check that passes is indistinguishable from a correct repository until you widen it
- `internal/storage/s3`: `TestAccelerationFallbackIsRaceFree`, which drives the real `DisableAcceleration`, `EnableAcceleration`, `IsAccelerationActive`, and `GetAcceleratedClient` from 16 goroutines rather than a hand-rolled reproduction of what they do — a reproduction can be race-free while the code it stands in for is not. It also asserts the settled state is internally consistent, so it cannot pass by exercising nothing
- `internal/fuse`: a table test over `toErrno`, taking it from 16.2% to 97.3% and the package from 56.6% to 59.9%. It is the whole of the package's error contract and was previously reachable only incidentally, through whichever error an operation test happened to provoke. Each row records the caller decision that depends on the mapping rather than just the constant, because that is the property being tested: a wrong errno does not fail an operation, it fails it *differently* — v0.10.0 collapsed every `HeadObject` error to `ENOENT` and `Create` read that as "absent" and wrote an empty object over a throttled file. The ordering between `ErrBackend` and a coded cause is asserted separately, since `ErrBackend` wraps the cause and so every coded failure also satisfies `errors.Is(err, ErrBackend)`: which check runs first is what decides whether an IAM refusal reaches the user as `EACCES` or as an undiagnosable `EIO`
- **The release workflow verifies that the tag being released and the version the binary reports are the same string**, and fails the release naming both if they are not. This replaces `-ldflags "-X main.Version=$VERSION"`, which could never have worked: `cmd/objectfs/main.go` declares `version` inside a `const` block, and the linker cannot rewrite a constant — there is no `main.Version` variable and no `internal/version` package for either of the two spellings that were being passed. So every release ObjectFS has ever cut silently ignored the flag and shipped whatever was hardcoded in the source, and `--version` was right only when someone had remembered to edit the constant by hand. A check is the better shape than an injection here: the constant is the documented single authority for the version, so the release should assert that authority agrees with the tag rather than overwrite it at link time and leave the source disagreeing with the artifact
- **The container image is subject to the same tag/version check**, so a mislabeled image fails the build instead of being pushed to `ghcr.io`. The `Dockerfile` carried the identical dead injection — `-X main.Version` / `main.Commit` / `main.BuildTime`, three symbols that do not exist — and the workflow fed it `BUILD_TIME=${{ steps.meta.outputs.json }}`, which is a multi-line JSON document rather than a timestamp. Demonstrated by building with `--build-arg VERSION=NOT-THE-REAL-VERSION` and getting an image that reports `0.10.1`: the tag on the image and the version inside it were entirely unrelated values, and agreed only by the accident of someone having edited the constant. The build now runs `--version` against the binary it just linked and fails, naming both strings and the file to edit, if the reported version is not the tag's

### Removed
- `internal/buffer` — the write buffer, its manager, and its buffer pool, replaced by `internal/vfs`. Its per-file state was one contiguous `[]byte` plus a single offset, a representation that can express "a run of bytes starting somewhere" and nothing else. It could not hold two disjoint dirty ranges, so a filesystem built on it had to either refuse the second write or lose the first — and it did both, in different places. Every write-path defect below follows from that one missing concept, which is why this is a replacement rather than a patch: an interval map grafted onto a contiguous buffer would have been reworked anyway. Its 1,900 lines of tests went with it; they passed throughout
- `internal/fuse.WriteCoalescer` and its thirteen tests. It merged writes before handing them to the buffer, and `mergeWrites` guarded its overlay with `if newEnd > currentEnd` — so a newer write shorter than the region it overlapped was dropped entirely, and `echo NEW > f` over a file holding `OLDER CONTENT` left the file reading `OLDER CONTENT`. It also discarded the buffer's write errors. The instructive part is the test suite: `TestWriteCoalescer_MergeOverlappingWrites` wrote `"hello!"` at 0 and `"LO_WORLD"` at 3 and then asserted only that the merge produced *one write of eleven bytes*. It never looked at the bytes, and a length assertion cannot see content corruption. Coalescing now happens in `internal/vfs.ExtentList.Add`, in the one place that owns the dirty state, last-writer-wins, with tests that assert on resulting content
- `examples/config/readahead-predictive.yaml` — a "preset" that was value-for-value identical to the built-in defaults, so copying it configured nothing. The four remaining read-ahead presets are kept but now carry a header stating they are not yet wired (#176)
- `GOLANGCI-LINT.md` — 76 lines instructing contributors to hand-create a `.golangci.yml` for golangci-lint v1.x, in a repository that had no such file and whose real config (added above) is v2 format. Its advice now actively contradicts the shipped configuration, and a document telling people to write a file that exists is worse than no document
- `internal/storage/s3/pricing_manager.go`: The AWS Pricing API integration (`fetchFromPricingAPI`, `parsePricingData`, `mapTierToStorageClass`, `extractStorageCost`, `extractRetrievalCost`, and the `AWSPricingResponse`/`AWSProduct` types). The parser matched products on human-readable storage-class names that do not appear in the Pricing API payload, and both cost extractors ignored their arguments to return hardcoded constants — so enabling `use_pricing_api` fetched a multi-megabyte JSON document over HTTP and then discarded it in favour of `0.023`/`0.01` per GB. `GetTierPricing` now goes straight to the default rate table derived from `StorageTiers`, which is what the old path effectively returned anyway (#161)
- `internal/fuse/cgofuse_filesystem.go`, `cgofuse_mount.go`, `cgofuse_test.go`, `platform_cgofuse.go`, the `github.com/winfsp/cgofuse` dependency, the `build-cgofuse` / `build-all-cgofuse` / `build-{linux,darwin,windows}-cgofuse` Makefile targets, and `FUSE_MIGRATION.md`. The `cgofuse` build tag never compiled: `filesystem.go` carried no build constraint of its own, so under the tag `OpenFile` was declared twice, and that duplicate-symbol error was masked by a missing `fuse.h`. It was also a silently divergent 382-line subset of the 727-line go-fuse implementation, offering only Mount/Unmount/IsMounted/Getattr/Open/Read/Write/Release/Readdir — it never received the `Unlink`/`Rmdir` fix, so under that tag `rm` reported success while the S3 object survived. `FUSE_MIGRATION.md` justified the tag by "macOS compatibility issues" with go-fuse, which is not true: go-fuse's darwin mount execs `mount_macfuse` and cgofuse needs the same macFUSE headers, so the two have identical macOS requirements
- `Makefile`: the `build-windows` target and Windows from `build-all`. There is no Windows FUSE binding, so the binary it produced could not mount anything
- `internal/storage/s3.Config`: `DisableSSL`, `TargetThroughput`, and `OptimizationLevel`. All three were settable from YAML and from the SDK and read by nothing — `TargetThroughput` was defaulted to 800 MB/s and appeared only in a startup log line, so it read as a throughput target being applied when nothing applied it, and `DisableSSL` is the most misleading of the three: a field named for turning off transport encryption which never turned anything off. Use `endpoint` with an `http://` scheme if a plaintext endpoint is genuinely wanted. **Migration:** delete these keys if a config file sets them; strict decoding now rejects them, which is the point — a config that set `disable_ssl: true` was not getting it before either
- **BREAKING (both SDKs): the `network`, `storage` and `distributed` metric sections, and the TypeScript `NetworkMetrics`, `StorageMetrics` and `DistributedMetrics` types.** Each was built from a metric family — `objectfs_network_*`, `objectfs_storage_*`, `objectfs_cluster_*` — that no version of ObjectFS has exported, so each was permanently empty while its type advertised required fields and the docs advertised a whole subsystem's telemetry. They are removed rather than stubbed for the reason the rest of this release keeps applying: a caller can tell that a key is missing and cannot tell that a present-but-empty one means "not implemented". **Migration:** read `metrics.operations`, `metrics.errors` and `metrics.connections`, which come from families that exist. `metrics.raw` carries every parsed sample for anything the extractors do not surface
- **The SDK metric section types are now all-optional, and `io` is honestly empty.** `CacheMetrics.hits` was a required `number`, which forced the old code to report `0` for "not measured" — indistinguishable from a real zero. `IOMetrics` is `{}` against a live mount as of this release, because the FUSE layer records only `prefetch`, `cache_hit` and `cache_miss` through the collector and not `read`/`write`; that is stated in the type's own documentation rather than papered over, and it fills in on its own when the recording lands
- `internal/adapter.parseSize` — the fourth and last of the duplicate size parsers, replaced by `utils.ParseBytes`. It returned 1 GiB, silently and with no error, for anything it could not parse: `cache_size: 2G`, `cache_size: 64MiB`, and `cache_size: tpyo` all configured a 1 GiB cache. Three different mistakes, one wrong answer, no message anywhere
- **Nine inert fields on `internal/fuse.MountOptions` and nine on `internal/fuse.Config`** — `MaxRead`, `DirectIO`, `KeepCache`, `BigWrites`, `AsyncRead`, `WritebackCache`, the three splice flags, and on `Config` also `AllowOther`, `MaxWrite`, `ReadAhead`, `WriteBuffer`, and `Concurrency`. Each was settable, carried a yaml tag, named a plausible FUSE capability, and was read by nothing. The yaml tags are what made them look plumbed and they were never bound to anything: nothing decodes YAML into either type, since the only decode target is `config.Configuration`, whose ten top-level keys contain no FUSE section. So removing them breaks no config file — a `fuse:` block was never read, and under this release's strict decoding it would now be rejected outright. Two were also not implementable as written: `max_read` is not a field on go-fuse's `MountOptions` at all (go-fuse passes it as a string mount option set equal to `MaxWrite`, so the read size is not separately settable), and `BigWrites` named a capability unconditional since kernel 4.20. `internal/fuse/doc.go` had justified keeping `MaxRead` on the grounds that "removing a YAML key breaks existing config files" — a key no loader read. #180 tracks the four that name real go-fuse capabilities, each to land with a test asserting its *effect* rather than that the field was copied, which is the check whose absence let eighteen dead fields reach a release
- The local tracking and status documents: `RACE_CONDITION_AUDIT.md`, `PROGRESS_REPORT.md`, `SPRINT_2_TRACKING.md`, `REMEDIATION_ROADMAP.md`, `RELEASE_NOTES_v0.4.0.md`, `docs/v0.4.0-COMPLETION-SUMMARY.md`, `docs/v0.5.0-implementation-plan.md`, and `MULTI_PROTOCOL_ROADMAP.md`. Tracking belongs on GitHub, not in checked-in markdown that cannot be told it has gone stale. `RACE_CONDITION_AUDIT.md` is deleted rather than corrected, which is the more important half: it concluded "ZERO race conditions detected" and "the codebase is RACE-FREE", and sixteen concurrency defects were filed and fixed after it, mostly in the files it individually blessed `✅ SAFE`. It described `internal/cache/multilevel.go` as having "no internal state requiring synchronization" when that struct *begins* with `mu sync.RWMutex`, and the acceleration race fixed in this release is a live instance of the class it declared eliminated. A confidently wrong audit is worse than no audit, because it is cited: several downstream documents inherited their safety claims from it. `MULTI_PROTOCOL_ROADMAP.md` is refiled as #181 with its market projections and fabricated throughput figures dropped and its stale assumptions recorded as assumptions
- `.goreleaser.yaml` and `.goreleaser.yml` — two GoReleaser configurations, in the same directory, differing in almost every decision, invoked by nothing. No Makefile target, no workflow, and no script referenced either one; both arrived in a single commit and neither has been run. They disagreed about which platforms to build (one linux-only, the other adding darwin and windows), which symbol to inject the version into (`main.Version` in one, `internal/version.Version` in the other — neither exists), and even who owns the repository (`scttfrdmn` versus `objectfs`, so the release URLs and the Homebrew tap pointed at different places). The linux-only file also carried `vendor: Scott Freedman` and `maintainer: Scott Freedman <scott@example.com>`, which would have put a misspelled name and a placeholder address into the metadata of every `.deb` and `.rpm` — against a `LICENSE` reading `Copyright 2025-2026 Scott Friedman`. Both are deleted rather than one being corrected: `.github/workflows/release.yml` is the path that actually builds and uploads artifacts, and keeping a second unexercised definition of "how ObjectFS is released" alongside it is how the two came to disagree in the first place. Reintroducing GoReleaser means one file, wired to a workflow, with the tag/version check added above
- **Three release-workflow jobs whose green checks asserted work they did not do.** `update-packages` had three steps whose entire body was `echo "Would update the Homebrew formula for version ..."` and a comment reading "Implementation would go here" — it published to no tap and no repository, and reported success for all three on every release. `update-docs` generated a documentation site and deployed it to GitHub Pages, which **is not enabled on this repository**, so `actions/deploy-pages` could only ever fail; its generator also wrote `README.md` and `CHANGELOG.md` while the index page it wrote alongside them linked `README.html` and `CHANGELOG.html`, making every link on the page broken by construction. `notify` posted to Slack behind a webhook check and then wrote a summary asserting five tasks complete — including "Performed security scans" and "Updated documentation" — as unconditional text, regardless of what had run. `publish` now summarizes the assets it actually attached. None should return in the shape it had: when a Homebrew tap is real it needs a job that pushes to it, and publishing docs means using the `mkdocs.yml` already in the repository, from its own workflow, once Pages is turned on
- `docs-platform/index.md`: the `<PerformanceChart>` and the `performanceData` array behind it. The chart rendered four hardcoded numbers as though they were measurements — nothing produced them but the sentence "ObjectFS looks good on a chart" — and a reader has no way to tell a plotted literal from a benchmark result. The page now points at `benchmarks/`, which contains runnable benchmarks. The "Secure & Compliant / GDPR, HIPAA, SOC2" feature card is also gone: ObjectFS holds no compliance certification, adds no RBAC layer, and writes no audit log. It is replaced by what the code does — SSE-S3 and SSE-KMS on every write, with bucket keys, and access control that is IAM's

### Changed
- **BREAKING: a configuration file containing a key the schema does not define is now rejected at startup, naming the key.** Previously `LoadFromFile` decoded non-strictly, so an unknown key was discarded in silence. What that cost, measured rather than estimated: `configs/example.yaml` — the file the README told users to copy, the file `scripts/postinstall.sh` installs as `/etc/objectfs/config.yaml`, and the file the Dockerfile ships — was 162 lines of settings of which, compared field-by-field against the built-in defaults, exactly **one** differed, and nothing read that field. It opened with a top-level `s3:` block where the schema has `storage.s3`, so `region: us-west-2` loaded as `us-east-1`. Its `mount:`, `buffer:`, `compression:`, `metrics:`, `health:`, `logging:`, `archive:` and `cost:` blocks were all inert. That is also why its compression block documented `enable`, `zstd_level` and `min_file_size` against the real `enabled`, `level` and `min_size` for four releases with nobody noticing: the whole block was already being thrown away, so correcting the names would not have changed the behavior either. A rejected config costs a user a minute; an ignored one lets them believe they configured a 100 GB cache in us-west-2 while running a 1 GB cache in us-east-1, and nothing will ever tell them. **Migration:** the cost is real and it is bounded — a deployment whose config has a key this schema does not define was already not getting that setting, so nothing it relied on stops working; it starts being told. The error names the offending key and lists the schema's top-level keys
- **The release matrix builds the four platform/architecture pairs that compile, instead of seven of which three could not.** `internal/fuse` now carries `//go:build linux || darwin` and `platform_unsupported.go` fails the build by design on anything else, so `windows/amd64` and `freebsd/amd64` were guaranteed-red jobs — and `linux/armv7` failed for a separate reason that outlives the build tags: `0xFFFFFFFF` does not fit a 32-bit `int`, so `safeIntToUint32` will not compile there at all. What that produced was a release run where three jobs failed every time, on every tag, for reasons unrelated to the release — which is the condition under which a red job stops being read. The four remaining entries are `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`. Adding a platform back means writing a FUSE binding for it first, not adding a matrix row
- **`FuzzValidateRegion` discards inputs far past the length limit rather than adding them to its corpus.** The target runs at roughly 700,000 executions per second, because the function under test is a length check and one regular-expression match. At that rate the 60-second CI smoke run mutated its way to a couple of hundred corpus entries, nearly all long strings differing only past byte 64 and so telling the validator nothing it did not decide at byte 64 — and every one had to be minimized and written out when `-fuzztime` expired. On a shared runner that shutdown overran its grace period and the job failed with `context deadline exceeded` and **no counterexample**, which is indistinguishable at a glance from a hang in the code under test. A fuzz target cheap enough to outrun its own bookkeeping is a real failure mode, and the cost of it is worse than the wasted minute: a red job that reports nothing trains the reader to disregard it. The length path stays covered by the seed corpus, which carries a 64-character region and is replayed by every ordinary `go test` — a bound the fuzzer cannot erode
- **The release workflow builds with the Go version in `go.mod`** (`go-version-file`) rather than a hardcoded `1.24`, which is two minor versions behind the `1.26.0` the module declares. Releases were being compiled by a toolchain no CI job had ever run, so the one build whose output users download was the least tested configuration in the repository
- **The `linux/arm64` container image contains an arm64 binary.** The `Dockerfile` hardcoded `GOARCH=amd64` while the release workflow builds `platforms: linux/amd64,linux/arm64` from that one file, so the arm64 manifest was published wrapping an **x86-64 executable** — confirmed by ELF header, machine type `0x3e` inside an image reporting `arm64`. It is the worst available shape for a packaging defect, because it works everywhere it is likely to be tested and fails only where it is deployed: both Docker Desktop and podman's VM register qemu `binfmt_misc` handlers and emulate the wrong architecture silently, so `docker run` on a developer laptop prints a version banner and exits 0, while a Graviton instance or an arm64 Kubernetes node gets `exec format error` at container start. The build now takes `GOARCH` from the `TARGETARCH` the builder supplies
- **The release page is created after its artifacts exist, not before.** It was created first, by `actions/create-release@v1`, and every other job depended on it for an `upload_url` — so the irreversible, outward-facing half of a release happened before anything had been compiled, and a build failure left the page up holding nothing. **That is what happened to v0.10.0**: its published release has zero downloadable assets, because one matrix entry failed, the rest were cancelled with it, and the page had already been created. Every release in this project's history failed the same way. The build now runs first and hands its artifacts to a `publish` job through `actions/upload-artifact`, so a failed build produces no release at all — which is recoverable by fixing the build and re-pushing the tag, where an empty published page is not
- **Release assets are published with `gh release create`**, replacing `actions/create-release@v1` and `actions/upload-release-asset@v1`. Both are archived and run on Node 12, and the `upload_url` they required passing between jobs was itself the reason the release had to be created first. One command now creates the release and attaches every asset, so there is no window in which the page exists empty. Each archive ships alongside a `.sha256` file and the release body says how to check it
- **Release notes come from the `CHANGELOG.md` section for the version being tagged**, and a missing section fails the release rather than falling back. Previously the body was `git log --pretty=format:"* %s (%h)"` since the last tag: a reverse-ordered list of commit subjects, including the ones that fixed mistakes made earlier in the same release, saying nothing about what any of it means to someone deciding whether to upgrade. Sections beyond the API's body limit are summarized down to the release's opening prose plus a per-category entry count with a link to the full text — this release's own section is 175 KB across roughly 200 entries, which would have failed the publish step at the last possible moment, after the artifacts were built and the image pushed
- **The release's vulnerability scan runs against the artifact about to be published, before it is published.** It used to `wget` the asset from the release page, which required the release to already exist — so it could only ever scan something users could already download, and a finding annotated a shipped release instead of blocking it
- **`security.yml` no longer publishes releases.** It was named "Security & Release", triggered on `tags: [v*]`, and carried its own `build`, `docker` and `release` jobs — a second workflow racing `release.yml` to publish the same tag, and one that would have won badly: it publishes with `generate_release_notes: true`, replacing the CHANGELOG-derived notes with an auto-generated commit list, and it built linux only, so whichever job finished second decided what users could download. The race never actually ran, and the reason is worth recording rather than relying on: GitHub set the workflow to `disabled_inactivity` in January 2026, which disables *every* trigger and not just the schedule, so it has not fired on any of the ten version tags in this repository's history. The duplicate release machinery was dormant, not absent, and re-enabling the workflow to get its weekly scan back would have silently armed it
- `go.mod`: `github.com/hanwen/go-fuse/v2` upgraded from v2.8.0 to v2.11.0. No API changes were needed — ObjectFS uses 14 distinct `fs.*` symbols and the binding surface is unchanged
- `go.mod`: `github.com/scttfrdmn/substrate` added as a test dependency at v0.85.0. Two capabilities are load-bearing. Ranged `GetObject` (substrate#405, past v0.76.0): ObjectFS's entire read path is ranged GETs, so before it the harness could not test the read path at all — and the probe reported it absent and skipped, rather than serving whole objects and letting every ranged assertion pass for the wrong reason. `CopyObject` with `MetadataDirective=REPLACE`, and `x-amz-storage-class` on write and on `HeadObject` (substrate#421, v0.80.0): those are the whole of the attribute-only write path and the whole of the storage-tier question, and neither was observable against the previous pin. Two fixes in v0.83.0–v0.85.0 land directly under this release's integrity work: substrate no longer persists `aws-chunked` as an object's `Content-Encoding` (substrate#428), which is the exact header the read path now dispatches decoding on and fails closed against — so a streaming upload's transfer encoding would have presented as an unknown codec — and `HeadObject` now resolves task completions so a HEAD agrees with a GET (substrate#457), without which a size read straight after a write could disagree with the bytes available
- `internal/fuse`: every file now carries `//go:build linux || darwin`. **Windows is explicitly unsupported.** Previously `filesystem.go`, `mount.go`, and `optimizations.go` had no build constraint at all while `platform.go` had `!cgofuse`, which is what allowed the duplicate `OpenFile` declaration to go unnoticed. A new `platform_unsupported.go` makes a build for any other `GOOS` fail inside `internal/fuse` with a message naming the reason, instead of emitting a list of `undefined: fuse.PlatformFileSystem` errors from `internal/adapter` that read like a broken build
- **Transparent compression now defaults to off.** It is a storage-format decision, not a performance knob: a compressed object is an opaque frame to `aws s3 cp`, boto3, and every other S3 client, so enabling it silently voids the implicit "my data is just objects in S3" guarantee. It also makes a ranged read fetch the whole object, since a zstd frame cannot be sliced. When it is enabled the default algorithm is now `zstd` at level 3 rather than `gzip`, which is both faster and smaller at every level. Opt in when the tradeoff is wanted
- `internal/compression.NewCompressor` takes a `compression.Settings` instead of `config.CompressionConfig`, removing this package's dependency on the application config package. That dependency direction is the reason the config layer could not validate compression by the one means that cannot go stale — asking the factory to build it — and had to be trusted to keep a duplicate list of algorithm names in step instead
- `examples/config.yaml` and the `s3.CompressionConfig` doc comments no longer advertise a gzip default or a single level range. Both now name `pkg/compression.SupportedAlgorithms` as authoritative, and the level comment says outright that the valid range differs per algorithm, so changing one may require changing the other
- `internal/fuse/doc.go`: rewritten. It documented a "Cross-Platform Abstraction" selecting between go-fuse and cgofuse, cited the stale module path `github.com/billziss-gh/cgofuse` (the module has been `github.com/winfsp/cgofuse` for years), and listed as supported a large set of operations this package does not implement — `chmod`, `chown`, `utimes`, `truncate`, `fsync`, `rename`, `link`, `symlink`, and all four xattr calls — plus symlinks-as-metadata, hard-link reference counting, FSEvents integration, Spotlight compatibility, and Windows/NTFS optimizations, none of which exist. It now describes the actual platform support, the object-storage mapping, and the concurrency contract. Four of the operations it wrongly claimed — `chmod`, `chown`, `utimes`, `truncate` — are now implemented, and the note that ownership and mode are not persisted is removed because they are
- **BREAKING (`pkg/types.Backend`): `PutObject` takes a user-metadata map, and a new `SetObjectMetadata` replaces an object's metadata without rewriting its contents.** POSIX mode, ownership, and mtime have no native field in object storage, so user metadata is the only place they can live — a `Put` that cannot carry them makes `chmod` and `chown` unimplementable, which is exactly the state v0.10.0 was in. `SetObjectMetadata` exists because a chmod is not a write: persisting one through `PutObject` would mean reading a 10 GiB object back and uploading it again to change nine bits. Implementations still own the integrity keys (`objectfs-sha256`, `objectfs-original-size`) and must ignore caller-supplied values for them, since those describe the bytes after compression, which only the implementation has seen. **Migration:** add a `nil` final argument to existing `PutObject` calls; `nil` means "no attributes to record" and is the previous behaviour exactly
- `pkg/types.Backend`: the interface now states the contracts whose absence produced three of the defects below — that a `GetObject` size of zero or less means "to the end of the object" (assigning it no meaning is what made a negative size a reachable panic), that a `ListObjects` limit of zero or less means every object and that implementations **must paginate**, and that `SetObjectMetadata` must preserve `Content-Encoding` because the read path dispatches decoding on it and fails closed
- `pkg/utils.ParseBytes` is strict and is now the only size parser in the repository. There were four — this one, `internal/adapter.parseSize`, one in `internal/compression`, and a copy in tests — and they disagreed about the cases that matter, which is audit finding C1's mechanism in miniature: a value that means one thing to the layer validating it and another to the layer acting on it. The old implementation used `fmt.Sscanf`, which stops at the first character it cannot consume and reports success, so `12abc` parsed as 12 and `64MiB` — the spelling someone who knows the units writes — parsed as 64 bytes. It now rejects trailing garbage, negative sizes, `Inf` and `NaN` (which `ParseFloat` accepts and which became `math.MinInt64` on conversion), hex-float and exponent notation, and a value that overflows `int64` once multiplied. Internal whitespace is still tolerated, so `1.5 GB` parses
- `configs/example.yaml` now contains only keys that are read on the mount path, and says so as a property of the file rather than of the schema. Three that were not are gone rather than caveated: `performance.max_concurrency` (which appears in a log line and a `Printf` and nowhere else), the whole `write_buffer` block, and `monitoring.metrics.prometheus` (which has no field on `metrics.Config` at all). It gains `storage.s3.storage_tier`, `storage.s3.multipart`, `performance.parallel_read`, and a `network` block with the timeouts and retry settings that now work. The division of labour with `examples/config.yaml` is deliberate: this file is the copyable starting point and *excludes* what is not wired, while the reference lists every key at its default and *marks* what is not
- `examples/config.yaml`: the `not yet wired` markers on `network.timeouts.{connect,read}`, `network.retry`, `network.circuit_breaker`, and `performance.parallel_read` are gone, because those settings now take effect. The `write_buffer` block gains them, with the reason: `vfs.NewWriter` takes no configuration and nothing bounds total dirty bytes, so a large enough write set is bounded by available memory rather than by `max_memory`. Fixing that means backpressure in the writer, not a wider mapping — so the keys stay marked rather than being plumbed to a component that cannot honour them
- **BREAKING: `security.encryption.in_transit` and `security.encryption.at_rest` are replaced by `mode`, `kms_key_id`, and `bucket_keys`, and the default is `mode: "off"` rather than `at_rest: true`.** The two booleans both defaulted to **true** and were read by nothing: a grep for `ServerSideEncryption`, `SSEKMS`, `SSECustomer`, or `aws:kms` across the tree returned zero non-test hits while `OBJECTFS.md` documented a `kms_key:` ARN in that block, so every object was written with no encryption header while the shipped configuration said otherwise. They are **removed rather than deprecated**, which given the strict loader means a file still carrying them fails to start and names the key. That is the intent. An absent feature gets noticed — someone looks for encryption, does not find it, and asks — whereas a key that claims the property and sets no header ends the search, and what the search would have found is what an institutional review asks about, usually after the data is already there. `in_transit` is gone with nothing replacing it because there is nothing to replace it with: the AWS SDK has always used TLS for S3 and there is no plaintext mode to disable. The new default is `off` for the same reason the old default was the defect: a secure-sounding default that changes no behaviour is worse than an honest one, and `off` is honest rather than unencrypted — S3 has applied SSE-S3 to all new objects unconditionally since January 2023, so it means "with S3's keys, not requested by us," and it cannot satisfy an SSE-KMS requirement (audit finding P-7)
- **A configuration naming an encryption setting that cannot take effect is now refused at startup, in both the loader and `NewBackend`.** Every rejected combination is *inert* rather than wrong-on-the-wire — a KMS key beside `mode: off`, a KMS key under `sse-s3`, `sse-kms` with no key, `bucket_keys` without KMS — and ignoring any of them would send no header and report no problem, which is P-7 reproduced exactly: the operator has written the word "encryption" in their file, named a key, and been told nothing is wrong. `sse-kms` with an empty key is refused rather than passed through because S3 accepts it and silently substitutes the AWS managed `aws/s3` key, which is shared with every other service in the account and cannot be audited or revoked separately from the data; `alias/aws/s3` is accepted, so that choosing the fallback can be written down as a choice. The check runs in both places because the two entry points — a YAML mount and a Go caller building an `s3.Config` by hand — share no layer that could hold one check
- **Uploads divert around the CargoShip transporter when it cannot express the configured encryption.** CargoShip v0.13.0 hardcodes the algorithm to `aws:kms` and has no `BucketKeyEnabled` field, so `sse-s3` and bucket keys have no representation there. Sending the object through it anyway would store it under an encryption nobody configured, and it reads back identically either way — P-7 with an extra step and harder to notice. This is the same shape as the pre-existing `Content-Encoding` bypass: a transporter that cannot carry a header must not carry the object. The cost is small, because objects at or above the multipart threshold never reach the transporter — only small objects divert, where congestion control has least to win. Plain `sse-kms` is expressible, so those uploads still go through it, and the key is passed to its config
- `internal/fuse.CreatePlatformMountManager` takes a `*vfs.Writer` rather than a `types.WriteBuffer`. The FUSE layer needs `SetAttr`, `Truncate`, `Attr`, and the context-taking flushes, none of which the interface declares, and widening `types.WriteBuffer` to carry POSIX attribute operations would push filesystem semantics into the type every cache and buffer implements
- **No document restates what version ObjectFS is.** The `version` constant in `cmd/objectfs/main.go` is the only authority; `CLAUDE.md`, `ROADMAP.md`, `DEVELOPMENT.md`, `OBJECTFS.md`, and `docs/ARCHITECTURE_EVOLUTION.md` now point at it or at the releases page instead of naming a number. Seven lines across five files were asserting a current version and all seven were wrong, by between one and eight releases. Documents remain free to discuss versions — planned milestones, historical notes, upgrade paths — and the new test draws the distinction that matters: `v0.6.0 — multi-protocol support` is a plan, `**Current Release:** v0.3.0` is a claim about the present
- **`tests/e2e_test.go`'s `TestVersionAndBuildInfo` asserts the version instead of printing one.** It logged `✅ Version: 0.4.0 (defined in main.go)` and asserted nothing at all, having concluded in a comment that "we can't test it directly here" — it can: build the binary, run `--version`, compare against the constant in the source. What the old form did was hardcode a *fifth* answer to the version question, one that had been wrong for six releases, and print a checkmark beside it. That is worse than no test, because the checkmark is evidence to a reader that something was verified
- **`README.md` is rewritten around two tables that did not exist: supported filesystem operations, and the data integrity contract.** ObjectFS was described as "POSIX-compliant" in more than thirty places including the binary's own `--help`, against ten of roughly forty VFS operations — so a reader had no way to learn that `rm` returns EROFS, that there is no `rename`, or that `git`, `sqlite3`, and `tar -x` will not work on a mount, short of trying. The operations table has four sections, and the two that carry the information are **Errors by design** and **Not implemented**: an operation that is missing and an operation that is refused are different facts for someone choosing whether to point a workload at this. The integrity section states what is guaranteed, what is not, and — separately — what changes when the remaining write-path work lands, so the guarantee cannot silently strengthen or weaken without the document being edited. Also gone: the fabricated migration case studies, the documented first command (which could not run, since the binary takes exactly two positional arguments and has no subcommands), and "zero-downtime configuration reloading", which was the SIGHUP trap stated as a feature
- `docs/DESIGN_PRINCIPLES.md`: the data-integrity section had its **Good** and **Bad** examples inverted with respect to the code. It presented a temp-key write plus `verifyChecksum` plus `CopyObject` as the pattern to follow and a bare `PutObject(key, data)` as the anti-pattern — and the bare `PutObject` *was* the implementation. Both labels were wrong: a single `PutObject` is already the atomic operation on S3, so the temp-key dance adds nothing but a second object to leak, and the actual defect was elsewhere entirely. v0.10.0 computed a SHA-256 on every upload, stored it as user metadata, and never read it on any path, so a codec mismatch, a lost `Content-Encoding`, a truncated body, and bit-rot all returned exit status 0. The **Bad** example is now that: recording a checksum and never reading it. Evidence written and never read is worse than no evidence, because it makes a system look verified. The principles list is rewritten the same way, from aspirations ("Consistency guarantees: Clear consistency model") to claims that can be checked against a line of code
- `ROADMAP.md` opens with a banner stating that its schedule is stale and its feature-to-version mapping no longer holds. It plans v0.4.0 for Q1 2026 through v1.0 for Q2 2027; everything through v0.10.0 has shipped, in a different order and with different contents, and the multi-protocol work it scheduled for v0.6.0–v0.8.0 has not started. The banner points at GitHub milestones, which is where the plan actually lives, and says the file should be rewritten against them or deleted rather than re-dated — bumping "Last Updated" would make it look maintained without making it true
- `OBJECTFS.md` opens with a banner naming what is false in it: roughly 700 lines of Go that reference types which do not exist and in places is not valid Go, a YAML schema that was proposed and never implemented (and that strict decoding now rejects), "POSIX-compliant", and unmeasured performance and coverage figures. The fake "Build Passing" and "Coverage 95%" badges are removed. The file is kept rather than deleted because code comments in `internal/config/config.go`, `internal/storage/s3/config.go`, `encryption.go`, and three tests cite it as the evidence for a config key that nothing reads — it is the record of the original intent, and a citation to a deleted file explains nothing
- `docs/architecture/overview.md`, `cmd/objectfs/doc.go`, `benchmarks/README.md`, and `mkdocs.yml`: the "POSIX-compliant" claim, the `SetAttr`/`Chmod`/`Chown`/`Rename`/`Link`/`Symlink` support list (none of which existed when it was written), the `fsync()`-is-durable claim, and the navigation entries for the deleted tracking documents. `cmd/objectfs/doc.go` matters more than its length suggests: it is the binary's own `--help`, so it was the one place a user would look for this and the one place it was least true
- Documentation that described APIs, defaults, and output that do not exist. Each of these was found by trying to verify a claim rather than by reading for tone, and two of them turned out to be reporting real defects — the acceleration race below, and the config-documentation test that had been skipping the files it existed to check:
  - `docs/s3-acceleration.md` promised "automatic re-enable after successful standard operations" and a manual `backend.GetClientManager().EnableAcceleration()`. Nothing calls the re-enable path and `Backend` has no `GetClientManager` accessor, so neither the automatic nor the manual form existed. The section now states that the fallback is one-way for the life of the mount and why that is a deliberate trade: the error that triggers it — a bucket without the Transfer Acceleration configuration — does not resolve on its own, so retrying would cost a failed round-trip per request forever. Its "sample benchmark output" was four hand-written lines with no `ns/op` column, which every real `go test -bench` line has; it is replaced by instructions to measure, because acceleration's benefit depends on your distance from the bucket region and a number from someone else's network is not evidence about yours. Both metrics snippets are rewritten against the real `backend.GetMetrics()`, and now say that `AccelerationEnabled` reports configuration rather than effect — a mount can have it true, have fallen back on its first request, and have served everything over the standard endpoint since, so the rate belongs derived from the counters
  - `internal/config/doc.go` documented a `StartWatcher`/`Updates()` hot-reload API: not the function, not the channel, not the reloadable/non-reloadable distinction it drew between settings. The example was not even valid Go — it opened with a bare `:=`, a good sign nothing had ever compiled it. The section now says there is no reloading, and carries the trap that created: SIGHUP does not reload configuration, it unmounts the filesystem, because the binary treats any signal as a shutdown request. Its "Production Defaults" and "Development Defaults" tables implied an environment switch that has never existed — the development figures (DEBUG, a 512MB cache, `MaxConcurrency` 50, prefetching off) were returned by nothing. The real values are listed with the two that are not what their names suggest: `Performance.CompressionEnabled` is the *write-buffer* switch and is true while object compression is off, and `Global.ProfilePort` is 6060 and is read by nothing
  - `internal/storage/s3/doc.go` called `PutObject` with the wrong arity and `optimizer.AnalyzeObject`, which does not exist. Both are rewritten against the real signatures, and the metadata example now records that ObjectFS's `objectfs-sha256` and `objectfs-original-size` are written last and **win over** caller keys of the same name — the post-compression bytes differ from what the caller handed in, so its checksum would be wrong
  - `docs/features/multipart-uploads.md` carried five YAML blocks rooted at a bare `s3:` with four invented keys (`multipart_threshold`, `target_throughput`, `optimization_level`, `enable_cargoship_optimization`). They are rewritten to `storage.s3.multipart` with the string sizes the schema takes, and note that two of the four now fail startup under strict decoding rather than being ignored. Its LocalStack/MinIO section points at `internal/testaws` and substrate instead

### Fixed
- **Release binaries are built with the current Go patch release, not one five behind.** `go.mod` carried only `go 1.26.0`, and `actions/setup-go` with `go-version-file: go.mod` reads that line as an **exact version spec** — so every job in every workflow, including the four that produce release binaries, compiled with go1.26.0 (`Setup go version spec 1.26.0` / `go version go1.26.0 linux/amd64`) long after go1.26.5 was available. `govulncheck` reported **19 standard library advisories** against those builds: `crypto/tls` fixed in 1.26.5, `crypto/x509` in 1.26.2 and 1.26.4 across six advisories, `net/http` and `net/http/httputil` in 1.26.3, plus `net`, `net/textproto`, `html/template`, and `archive/tar`. None was a defect in this code; all were shipped by it, and `crypto/x509`/`crypto/tls` sit on the path to S3 on every single request. An explicit `toolchain go1.26.5` fixes it, because setup-go prefers `toolchain` over `go` when both are present — leaving the `go` line to mean what it should, the language minimum rather than a build pin. That conflation is the real defect: one directive was doing two jobs and the wrong one won. 19 advisories → 0
- **The AWS SDK is past GHSA-xmrv-pmrh-hhx2.** `aws-sdk-go-v2/aws/protocol/eventstream` v1.7.1 → v1.7.8 and `aws-sdk-go-v2/service/s3` v1.88.4 → v1.97.3 (MODERATE, CVSS 5.9): a malformed EventStream response frame carrying a header value type byte outside the valid range panics the decoder and terminates the host process. For a library that is one failed request; for a FUSE mount it unmounts the filesystem out from under every open file descriptor, and buffered writes not yet flushed are lost — the same failure shape as the shutdown panics fixed elsewhere in this release, which is why those were treated as data-integrity bugs rather than robustness ones. Reaching it requires an S3 endpoint that returns a malformed frame, so over TLS to real S3 that means AWS itself; the realistic exposure is a custom `endpoint_url` pointed at an untrusted S3-compatible service, which is a supported configuration. Nine minor versions of `service/s3` is enough surface to want evidence rather than a green build, so: the full `-race` suite passes, including `internal/testaws` and `internal/difftest` — the two that assert on wire-level behavior, headers sent and ranges requested and bytes transferred, and therefore the two an SDK bump could actually move — and the integration suite passes against real S3 in `us-west-2`. `govulncheck` now reports **0 vulnerabilities**
- **The license check asks about the binary that is distributed, not about test dependencies.** `go-licenses check ./...` failed on `modernc.org/mathutil`, which reaches this project only as `internal/testaws` → substrate → sqlite → libc → mathutil: `go list -deps ./cmd/objectfs` contains zero `modernc.org` packages, so it ships in nothing. License compliance is a question about distributed artifacts, and a check scoped to `./...` was asking it about code no user receives. The finding was also wrong on its own terms — mathutil's LICENSE is a textbook BSD-3-Clause with all three clauses verbatim, which `go-licenses` cannot classify only because the text is reflowed and wraps mid-clause. That left a choice between an allowlist entry asserting a license on someone else's behalf, which this project has no standing to do, and scoping the check to the artifact whose licensing anyone can be bound by. Verified that mathutil was the only unclassified license in the entire graph, so this narrows what is checked without suppressing a real finding
- **The release security scan runs, and its action pin resolves.** `aquasecurity/trivy-action` was pinned as `@0.28.0`. Every tag that action publishes is v-prefixed — v0.26.0 through v0.36.0 — so the reference did not resolve, and an unresolvable action fails at **"Set up job"**, before any step exists to log it: the failure presents as an infrastructure problem rather than a typo, with no step output to read. On the first `v0.10.1` tag it took down the scan job, which `publish` gates on, so nothing was released. That part is the restructure working as designed — all four `Build` jobs succeeded, the first time in this project's history, and no release page was created, where v0.10.0 published an **empty** one from a smaller failure. Pinned to `@v0.36.0` in both workflows after verifying the tag exists, that its `action.yaml` accepts `scan-type`/`scan-ref`, and that the equivalent `trivy rootfs` scan of a real stripped release binary exits 0 with valid SARIF. The scan is deliberately **not** a gate: trivy-action's `exit-code` has no default, so findings upload as SARIF and the step passes, and the comment above the job — which previously claimed a finding "blocks the release" — now says so and says why. Making every release contingent on a scan whose first output nobody had triaged would have blocked this one on a dependency bump it does not carry. Its first real run found a genuine MODERATE advisory in the pinned `aws-sdk-go-v2`, now filed rather than absorbed
- **The gosec SARIF upload no longer discards 45 findings because of 3.** `Analysis upload status is failed` / `locationFromSarifResult: expected artifact location`, three times, rejecting the **entire** submission. gosec emits three G115 integer-conversion findings with `"artifactLocation": {}` — no file path at all — because they originate in `sdks/c/main.go`, which is cgo: gosec analyses the generated intermediate and cannot map the location back to a real file. The step already stripped gosec's invalid `fixes` field for the same class of reason; it now also drops results with no resolvable location, taking 48 → 45 with every actionable finding preserved. Dropped rather than back-filled with a placeholder path, because inventing a location pins a finding to a line that has nothing to do with it. This is the first time either problem has been *observed*, and that is the point worth recording: the workflow was `disabled_inactivity` from January 2026 until it was rewritten this release, so the `fixes` deletion had been carried forward untested along with the rest of the file
- **A test that allocated 100 MB to check memory-growth detection, and then accepted either answer.** `TestMemoryMonitor_MemoryGrowthDetection` ended `if len(alerts) == 0 { t.Log("No alerts generated (may be normal if memory growth is small)") }` — so it asserted nothing, and deleting the entire growth-detection branch left it green. It surfaced as a coverage failure rather than as a test failure, which is the only reason it was found: `pkg/memmon` measured 51.0% or 50.6% run to run against a floor of 51, so a commit touching **only YAML** went red, having passed on the identical tree twelve minutes earlier. The variance was one statement — the alert branch in `analyzeMemory`, reached only when the test's own allocations happened to push `Alloc` past the threshold between two samples, i.e. on garbage-collector timing. Replaced by three tests that assert: the threshold comparison driven from seeded samples across four cases (over alerts, under is silent, **exactly at** is silent — the comparison is `>`, and which one it is decides whether a monitor configured at its steady-state growth rate alerts continuously or never, and shrinking does not alert), with the alert's `BaselineMem`/`CurrentMem`/`GrowthPct` checked because an alert saying something grew without saying from what to what is not actionable; the early return pinned for all three incomplete-sample states, since comparing a sample to itself would report 0% growth as a fact rather than as an absence of data; and the real `Start`/sample/`Stop` loop kept under test, asserting only what is deterministic there and polling to a deadline instead of sleeping a guessed interval. Fifteen consecutive runs now measure 51.9% exactly, the branch is covered on every one, and `analyzeMemory` goes 87.0% → 95.7%. The floor stays at 51 per the ratchet policy, so it clears with margin instead of landing on the number — a gate with zero margin is what made this visible at all
- **The differential-oracle fuzz job no longer OOM-kills its own workers and blames an innocent input.** `go test -fuzz` runs one worker *process* per CPU, and each is a separate Go runtime that sizes its heap against the whole machine, because none of them knows the other three exist. `FuzzOperationSequence` allocates hard — every iteration is a real read-modify-write over a real HTTP endpoint — so four workers reached 6 GB of RSS within four seconds and the cgroup killed one. What `go test` reported was **"fuzzing process hung or terminated unexpectedly while minimizing: EOF"**, naming whichever input the dead worker happened to be holding and writing it to `testdata/fuzz/`, which is what makes this failure mode so misleading: the recorded inputs all replay green, because none of them was ever the problem. Two consecutive CI runs failed this way and the first was written off as runner preemption — the giveaway was 24 seconds of `execs: 1443 (0/sec)` with all four workers producing nothing, then recovery, which is page thrash rather than a hang. `GOMEMLIMIT=768MiB` per worker fixes it, and the memory was garbage the collector had no pressure signal to reclaim rather than live data, so capping it costs nothing and gives back the time that was going into thrash: **24,679 execs and 48 new interesting inputs in 60 s, against 1443 execs and 3 in the run that died.** Peak 3.2 GB against the runner's 16 GB
- **The health endpoint no longer binds a fixed port during tests, and its handler is tested at all.** Eight tests in `internal/health` started a `Monitor` whose default `Config` enables the HTTP endpoint on port **8081**, so each `Start` raced its siblings for one port: the first won and the rest took `startHTTPServer`'s bind-error arm. How many did was a matter of goroutine scheduling, which made the package's measured coverage a function of machine load — 45.0% idle, 44.7% under CI's — so a per-package floor set from the luckier run failed CI while passing locally, over two statements nothing had deliberately tested. Meanwhile the endpoint an operator, a Kubernetes liveness probe, or a load balancer actually reads had **never served a request in a test**: its only coverage came from tests *failing* to reach it. The bind is now separated from the serving so a test can supply a listener on port 0; tests not about the endpoint no longer open one; and the handler is pinned on the property a probe depends on — 200 with a passing check, 503 with a failing one, the failing check named in the body — along with the port actually being released on `Stop` and the bind failure staying non-fatal, because a health endpoint is observability and taking a mount down over a diagnostic port inverts the priority. Coverage is now 48.5% at every `-cpu` value from 1 to 8
- **`internal/network`'s Linux congestion-control code is testable without writing to package state.** The procfs paths were package-level variables so a test could redirect them, which meant every case that did so was un-parallelizable — a subtest would have raced the variable its sibling had just assigned — and being un-parallelizable is a lint failure under this repo's own test conventions. They are constants again and the readers take the path as a parameter, so all six tests are parallel. This surfaced the more general trap it came from: **`golangci-lint` on macOS never compiled the file at all**, since it is `//go:build linux`, so a local run reporting `0 issues.` had inspected none of it and CI reported eleven. Linting the other platform is `GOOS=linux golangci-lint run`, and it reproduces CI exactly
- **The Prometheus endpoint is now actually served.** `monitoring.metrics.enabled: true` and `global.metrics_port: 8080` were both honored as far as constructing the counters, and the collector's `Start` — the method that binds the listener — was never called. So every operation was recorded into a registry nothing could read: a scrape got connection refused, while the mount logged nothing amiss. Both SDKs' metrics calls, every documented Prometheus and Grafana example, and the whole of `docs/monitoring` were describing an endpoint that did not exist. A collector with no listener gathers identically to one with a listener — the field is non-nil either way and only an HTTP request distinguishes them, which is why this survived a release and why deleting the call had left the adapter's tests green. The binding moved into an `Adapter.startMetrics` method for that reason and not for tidiness: `Start`'s remaining steps need a bucket, a mountable directory and a FUSE-capable kernel, so no test that went through `Start` could reach step 1 at all
- **The custom labels an operator configures are attached to the exported series.** `monitoring.metrics.custom_labels` was carried from YAML through `config.MetricsConfig`, mapped into `metrics.Config.Labels` — and then `initMetrics` never read the field, so a Prometheus scraping several nodes received series identical in every label and had no way to tell them apart. Along with it, `Namespace` defaulted to empty where the docs, the SDKs and every dashboard expect `objectfs`: the exported name was `operations_total`, not `objectfs_operations_total`. A label whose name collides with one of a metric's own dimensions (`operation`, `status`, `type`, `level`) is now rejected at collector construction, naming the offender, rather than failing later inside a registration error
- **Both SDKs can parse a scrape.** The Python and TypeScript metrics parsers split each exposition line on its first space and used the left half as the metric name — which for any real ObjectFS series yields `objectfs_cache_requests_total{service="objectfs",type="hit"}`, name and label block fused into one string that no lookup could match. Independently, the extractors above them looked up `cache_hits`, `objectfs_cache_hits_total`, `objectfs_io_read_operations_total`, `objectfs_network_requests_total`, `objectfs_storage_operations_total` and `objectfs_cluster_nodes`, none of which any version of ObjectFS has ever exported. Two bugs, each masking the other: fixing the parser alone still finds nothing, and fixing the names alone has nothing to look them up in. Both parsers are now label-aware — label values are escaped strings, so they are scanned character by character rather than split on `,`, and the closing brace is found from the right, since a comma, a quote or a `}` inside an error message in a label value is data. A parsed scrape is a **list** of samples rather than a map keyed by name, because a name identifies a family and not a series: `hit` and `miss` are two samples of one name and a map silently keeps whichever came last
- **A cache hit rate of zero is reported, and an unmeasured one is not.** The derivation was guarded by `if (hits && misses)`, false whenever hits is 0 — precisely the case an operator most needs the number for, a cache being asked and never answering, which was v0.10.0's actual live state since `RecordCacheHit` had no caller. It is now guarded on the request count, and omitted entirely when there have been no requests: an idle mount having served none is a different fact from a hit rate of zero, and reporting `0.0` for it reads as a cache that never hits
- **The SHA-256 every upload records is now actually compared against the bytes that come back.** v0.10.0 hashed the uncompressed content of every single object on the way up, stored it as the `objectfs-sha256` user-metadata key, and surfaced it on `HeadObject` as `ObjectInfo.Checksum` — and then no read path anywhere ever read it. The one piece of stored evidence that what came out is what went in was written and never used, which is why a codec mismatch, a lost `Content-Encoding` header, a truncated body, a mangled multipart assembly, and bit-rot in the bucket all returned corrupt content with a successful exit status. `GetObject` now recomputes the hash on every whole-object read and fails closed with `ErrCodeDataCorruption`, non-retryable, because a retry re-reads the same bad bytes. A malformed recorded value is also an error rather than a skip: unlike `objectfs-original-size`, where a bad value falls back to `ContentLength` so a bad mode cannot make a file unreadable, there is no safe fallback for a checksum — treating "I cannot tell whether this is corrupt" as "this is fine" is the exact reasoning that let the compression corruption ship. An object with no recorded checksum still reads normally, since refusing objects written by `aws s3 cp` or boto3 would make ObjectFS unable to read the buckets it exists to mount
- **Whole-object verification is decided by what S3 returned, not by whether a `Range` header was sent.** These differ for the most ordinary read a filesystem serves: `cat` of a 4 KiB file arrives at the backend as `offset=0, size=131072` — the kernel's read buffer, not the file's length — which sends a range and gets back every byte of the object. The first version of this guard keyed on the request shape and therefore declined to verify whole small files while reporting that reads were verified; coverage is now read from the `Content-Range` S3 itself returns. Genuine fragments remain unverified and that gap is stated plainly rather than implied away: the recorded hash covers the whole content, so checking a fragment would mean fetching the whole object, which is the read amplification the read path was just fixed to stop doing. Per-chunk checksums belong with the seekable-framing work, since both change the stored object's layout
- **User-metadata lookups in `internal/storage/s3` are case-insensitive.** S3 lower-cases metadata keys in transit, but the SDK preserves whatever case the *server* sent: MinIO title-cases them and a Go `http.Header` round-trip yields `Objectfs-Sha256`. Three case-sensitive lookups — the checksum, `objectfs-original-size` on `HeadObject`, and the still-encoded check — would each have found nothing against that storage. For an integrity check that is the worst available outcome, since finding no checksum means concluding the object came from another tool and verifying nothing: it stops checking without ever failing. `internal/vfs/attr.go` already had a case-insensitive helper for this reason; the S3 backend now has one too
- **An offset write no longer truncates the file to just the bytes written.** The write-buffer flush callback in `internal/adapter/adapter.go` received `(key, data, offset)` and called `backend.PutObject(ctx, key, data)`, discarding the offset — and because `PutObject` replaces the whole object, appending one byte to a 1 MiB file left a **1-byte object**, with `close(2)` reporting success. Writing `X` at offset 1048575 of a 1 MiB file destroyed 1,048,575 bytes. The flush now fetches the ranges of the stored object the pending writes do not cover, splices them, and uploads the whole result, so an offset write means "modify these bytes." The differential oracle pins it against the local filesystem byte-for-byte rather than against a hand-written expectation, because the hand-written expectation is exactly what agreed with the bug for four releases
- **A write that does not continue the previous one no longer returns `EIO`.** `canBufferWrite` refused any write that did not extend the single contiguous run, so ObjectFS returned an I/O error to the ordinary write patterns of SQLite (header at 0, page at 4096), `mmap` writeback, `tar`, and HDF5 — a working filesystem reporting hardware failure. There is now no contiguity requirement, no batch-size threshold that turns a large write into an error, and no sparse write that fails: pending writes are an interval list, and a hole between two of them is a hole, which reads as zeros exactly as it does on a local filesystem
- **`close(2)` and `fsync(2)` report a failed upload instead of success.** `FlushWithContext` scheduled a background flush and returned nil, so a `PutObject` rejected for AccessDenied incremented `stats.Errors` — a counter nothing read — while the process that wrote the data was told it was durable. Flush is now synchronous and returns the backend's error, `FileHandle.Release` returns that errno rather than the `_ = fh.Flush(ctx)` it used to discard, and a failed flush **leaves the data pending** so unmount tries again. `Adapter.Stop` flushes with the caller's context before canceling the mount context or closing the backend, and logs what it could not make durable. The v0.10.0 test named for this asserted `stats.Errors >= 0`, which is true of every `uint64` ever produced, including the zero it got
- **A write arriving during a flush is no longer annihilated.** `flushBuffer` deleted the buffer on a successful upload without rechecking for writes that had arrived in the meantime, so a write concurrent with an in-flight flush was discarded *and accounted as flushed*. The node now carries a generation counter: the flush captures it before building the plan, and the pending state is cleared only if it has not moved. When it has, the upload described stale content and the flush goes round again rather than reporting a success it did not achieve
- **A flush with no callback configured no longer counts as success.** `scheduleFlush` fell back to `flushBuffer(key, nil)`, and `flushBuffer` treated a nil callback as a successful flush — dropping every buffered byte and reporting durability. There is no callback to be nil: the flush path holds the backend directly
- **Directories no longer report mode 0000, which made the whole mount inaccessible to every non-root user.** Three things composed: `mount.go` sets `Options.NullPermissions`, which disables go-fuse's mode backstop; `internal/adapter` never set `DefaultPerms`, so `NullPermissions` was always true; and `DirectoryNode` implemented no `Getattr`, so nothing supplied a mode in the backstop's place. The result is `d---------` on every directory, and `EACCES` on any traversal by a user who is not root. This is the defect that made v0.10.0 unusable as a filesystem rather than merely unreliable, and it is why the audit's live-mount gate is `ls -ld` as a non-root user. `DirectoryNode.Getattr` now reports the configured directory mode, defaulting to 0755 — the execute bits being the load-bearing part, since a directory mode without them cannot be traversed however permissive the rest of it is
- **An attribute-only flush now reads the metadata back and fails closed, so a `chmod` cannot report success while storing nothing.** S3 has no metadata-update operation, so `SetObjectMetadata` is a `CopyObject` self-copy with `MetadataDirective=REPLACE` — a compound operation with a silent no-op mode. An endpoint that ignores the directive answers 200 and carries the *old* metadata forward, so `chmod 600 f` reports success, the next `stat` reports 644, and the divergence is invisible until someone notices permissions do not survive a remount. The flush already issued a confirming `HeadObject` to check the object's size had not moved, so verifying the attributes in that same response costs nothing. Found against a real endpoint that behaves exactly this way, within an hour of the attribute path first being testable; the check is not hypothetical. It reports **every** attribute that failed to land rather than the first, because "mode landed but ownership did not" is a different diagnosis from "nothing landed," and naming one key would make the two indistinguishable
- **A `stat` no longer fails on a file that has been created but not yet written.** `Create` used to make the file exist by PUTting a zero-byte object, which is the audit's worst data-loss path once composed with a `Lookup` that reported every failure as `ENOENT` — a throttled or `AccessDenied` stat of an existing file made the kernel believe it was absent, and the create that followed replaced it with nothing. The empty PUT is gone. What makes the file exist instead is an attribute record in the write path, which `Getattr` consults ahead of the object's stored metadata, so the window between `creat(2)` and the first write is covered without a request. The file is also owned by the calling process rather than by the mount's configured default: the kernel sends the caller's uid and gid with the request, and a multi-user mount on which every file belongs to whoever started the daemon is not a multi-user mount
- **`Lookup` reports a backend failure as itself instead of as "file absent".** Every `HeadObject` error was collapsed to `ENOENT`, so a throttle, a permission failure, or a network fault read as absence — and `Create` then wrote over a file that was merely temporarily unreachable. The classifier that decides this is deliberately strict in one direction: reporting a live object as absent invites an overwrite, while reporting an absent object as an error merely fails. The directory probe that follows a genuine 404 is held to the same rule, since `ENOENT` from an unanswered existence question invites a create over a directory that may be full of files
- **`Readdir` lists every entry in a directory, and lists each one once.** It passed a hard limit of 1000 to `ListObjects`, which in turn issued a single `ListObjectsV2` request — and S3 caps a single response at 1000 keys whatever `MaxKeys` says, so both halves truncated. A truncated listing is not a display problem: the entries past the cap do not exist as far as any caller is concerned, so `rm -rf` reports success having deleted a fraction, `cp -r` copies a fraction, and `du` understates a dataset. The backend now follows continuation tokens, and `Readdir` passes no limit. Separately, the deduplication set covered only the subdirectory branch on the reasoning that object keys are unique — but two distinct keys routinely produce the same *entry name*: a marker object at `dir/` and any object under `dir/` both yield `dir`, and ObjectFS's own `Mkdir` writes exactly such a marker. A duplicate name in a `DirStream` makes `readdir` return the same entry twice, which `ls` prints twice and `rsync` treats as a protocol error
- **The mount's configured ownership and permissions are used.** `CreatePlatformMountManager` hardcoded `DefaultUID: 1000`, `DefaultGID: 1000`, and `DefaultMode: 0644`, discarding `MountConfig.Permissions` entirely — so every file was reported as owned by whoever happened to be user 1000 on that host, and no permission setting in any config file had any effect. It also hardcoded `ReadOnly: false`, which meant `read_only: true` mounted a **writable** filesystem: the one setting whose failure a user cannot detect until something has already been overwritten. Ownership now defaults to the mounting process rather than to uid 1000 or to zero — a zero uid is not "unset" by the time it reaches a `stat`, it is root — and each field of `Permissions` falls back individually, because the adapter's path supplies no `Permissions` block at all. `Options.AttrTimeout` now also reaches the attribute timeout the nodes report, which previously was a hardcoded 60 seconds while go-fuse was told 1 second, so the kernel cached for a period no configuration named
- **A read no longer returns pre-write bytes on the same descriptor.** The read path consulted the cache and the backend and never the write buffer, so a write followed by a read at the same offset returned the old contents for up to the cache's five-minute TTL. `Writer.ReadAt` overlays pending writes on the stored object, and a range that pending writes fully cover is served without touching the network. `Getattr` had the same defect from the same cause — it took the size from the object's metadata, so the kernel truncated reads of a file being written at its pre-write length — and `FileSize` now reports the logical size including pending writes
- **Reads of objects that do not exist no longer take the mount offline.** Two independent defects composed into a total, permanent read outage on an entirely healthy S3: `pkg/health` counted a `NoSuchKey` as evidence the service was unwell, so ten reads of absent keys drove the `s3-reads` component to `unavailable`; and `unavailable` was a one-way door, because `GetObject` checks the availability gate *before* reaching `getObjectRange`, which holds the only `RecordSuccess("s3-reads")` call in the backend — from the state that needed a success, no success could be produced, and nothing in the repo calls `StartHealthChecks` to supply one either. Verified by execution: after ten reads of a missing key, a read of an object that existed was refused with `SERVICE_UNAVAILABLE`, and twenty further attempts never recovered. Ten `stat`s of absent paths is an ordinary minute in the life of a filesystem — shell tab-completion, a build system probing for headers, any `open(O_CREAT)` of a new file, and ObjectFS's own `Lookup` does one per path component (#175)
- `pkg/errors`: `IsServiceFailure` — one authority on whether an error is evidence the service is unwell or an ordinary answer to an ordinary request. A 404 for an object nobody wrote means S3 is up, reachable, authenticating, and answering correctly; it is the filesystem equivalent of a successful call. The listed codes are the non-failures and everything else counts, so a code added later defaults to counting: the failure mode of forgetting to update the list is a component that degrades too eagerly and recovers on its probe timer, not one that never notices an outage
- `internal/circuit`: `defaultIsSuccessful` asks `errors.IsServiceFailure` rather than testing `err == nil`. The circuit breaker had the same defect as the health tracker, in the second of the two mechanisms guarding the same call — with the health gate fixed, the `s3-get` breaker opened on the same sequence and produced the same refusal. Two mechanisms, one classification question, now one authority. The S3 backend's breaker config also says outright that `MaxRequests` is the half-open probe limit and that the trip decision belongs to `ReadyToTrip`, because reading that block and seeing only `MaxRequests` invites the conclusion that ten failures trip the breaker — it was read that way during the audit
- `pkg/health`: a component that has been refusing operations admits one probe after `ProbeAfter` (default 30s), which is the circuit breaker's half-open state that `internal/circuit` already implemented correctly and this layer lacked. A clean probe recovers the component outright rather than decrementing the error count, because the gate admits one operation per interval and recovery must not require ten of them; a failed probe leaves the state where it was and rearms the clock. Admission is decided on a timestamp rather than on the in-flight flag, so a probe whose outcome is never recorded costs one interval instead of latching forever — gating on the flag would rebuild the defect the mechanism exists to fix. Concurrent callers against a refusing component yield exactly one probe, so an outage is not met with full load. `ProbeAfter: 0` opts out
- **The shipped default configuration can construct a backend.** `internal/config` defaulted `write_buffer.compression.algorithm` to `gzip` while the codec factory implemented only `none`, `zstd`, and `lz4`, so `objectfs s3://bucket /mnt` exited with `Failed to start adapter` naming nothing — on stock config, with no way for a user to know which field was at fault. Two changes close it: a gzip codec now exists, and `Configuration.Validate()` rejects an unbuildable compression block at config load **by building the codec**, so the failure arrives before the mount is attempted and names the offending value. Validating against a list of algorithm names would have been a second authority on the same question, free to drift exactly as the first one did — and it could not catch the other half of this: a *level* out of range for the chosen algorithm, since zstd accepts 0–22 and gzip only 0–9, so `level: 22` is valid for one and not the other
- `internal/config`: a compression block that is disabled is no longer validated, so an unused algorithm name in a config file cannot block a mount that will never compress anything
- `internal/storage/s3/client.go`: **every** S3 client ObjectFS builds now applies the configured `endpoint`, `force_path_style`, and `use_dual_stack`. The connection pool's factory called `s3.NewFromConfig` with no options at all, so `HeadObject`, `DeleteObject`, `ListObjects`, the health check, and the non-accelerated path of `executeWithAccelerationFallback` addressed **real AWS S3** while `PutObject` and `GetObject` addressed the configured endpoint. ObjectFS could therefore not work against MinIO, Ceph, Wasabi, or any S3-compatible endpoint, and failed in a way that reads as a credentials problem. A single shared `clientOptions` mutator is now used by all four construction sites, and a test enumerates the manager's accessors so a client added later without it fails there rather than in production
- `internal/storage/s3/client.go`: `access_key_id` and `secret_access_key` are now used. Both fields were documented on `Config`, settable from YAML and from the SDK, and read by nothing — the client always fell through to the ambient credential chain, so a config naming explicit credentials authenticated as whatever profile happened to be in the environment. When both are set they are now supplied as static credentials; when either is empty the default chain still applies, so IAM roles and profiles are unaffected
- `internal/storage/s3/pool.go`: `ConnectionPool.Get`/`GetWithTimeout` return `(*s3.Client, error)` and **wait** for a connection instead of returning a nil client. Once `currentSize` reached `maxSize` the pool returned `nil` forever — the `select` had a `default` arm, so it never reached its own `time.After` case — and all six call sites dereferenced the result unchecked. On the default 8-connection pool the ninth concurrent operation panicked and unmounted the filesystem under every open descriptor. A saturated pool now blocks up to the timeout and, if it expires, returns an error naming the in-use count and the config knob to raise
- `internal/storage/s3/pool.go`: `Put` performs its `closed` check and its channel send in one critical section, and `Close` drains the channel rather than closing it. The two were a check-then-act race whose losing side was a send on a closed channel — a panic during unmount, on the path every deferred `Put` takes. A `Get` already blocked when `Close` runs now fails immediately rather than waiting out its full timeout
- `internal/storage/s3/pool.go`: the pool no longer overshoots `maxSize` under concurrency. The capacity check and the counter increment were taken under separate locks, so N concurrent callers all passed the check before any of them incremented: 16 concurrent readers against a pool of 4 constructed 16 clients and discarded 12 on return. Reserving a slot is now one atomic operation, and a factory failure releases the reservation instead of permanently shrinking the usable pool
- `internal/storage/s3/pool.go`: `Warmup` respects `maxSize`, reports factory failures with `errors.Join` instead of a bare count, stops on a cancelled context, and refuses a closed pool. `Resize` refuses to grow past the capacity the pool was constructed with — the idle channel's buffer is fixed, so a larger `maxSize` would let a reservation be made for a slot with no buffer space and deadlock the return — and tells the caller to raise `performance.connection_pool_size` instead. Shrinking now converges on the new size by dropping checked-out connections as they return
- `internal/storage/s3/backend.go`, `cost_optimizer.go`: the five pooled-client call sites propagate a pool acquisition failure as an error on the operation rather than dereferencing nil
- `internal/storage/s3/backend.go`: `PutObject` now records the uncompressed byte length as `objectfs-original-size` in S3 user metadata whenever transparent compression actually compressed the object, and `HeadObject` reports that value as `ObjectInfo.Size`. Previously `HeadObject` returned the *compressed* `ContentLength`, which the FUSE layer cached and handed to the kernel as the file size — truncating every read of a compressed file at the compressed length. A 40 MB object reported 4,556 bytes and a 64 KB object reported 87 bytes, making over 99.99% of the data unreachable. Objects written before this change fall back to `ContentLength` as before (#170)
- `internal/storage/s3/backend.go`, `multipart_upload.go`: The multipart upload path no longer recomputes `objectfs-sha256` from the post-compression bytes. `putObjectMultipart` and `initiateMultipartUpload` now receive the same metadata map the single-part path builds, so the checksum always describes the uncompressed content on both paths. Previously the two paths stored hashes of different byte streams under the same key, so a checksum written by one path could never be verified against the other (#170)
- `internal/fuse/filesystem.go`: `DirectoryNode` now implements `Unlink` and `Rmdir`, returning `EROFS` and logging a warning. go-fuse's `NodeUnlinker`/`NodeRmdirer` defaults return **success** for unimplemented operations, so `rm` and `rmdir` previously reported that files had been deleted while the S3 objects remained — and a subsequent `ls` showed them again. Deleting through a mount now fails loudly instead of silently lying. Full implementations are tracked separately (#163)
- `internal/storage/s3/backend.go`: compressed uploads no longer go through the CargoShip transporter, which silently corrupted them. `cargoships3.Archive` has no `ContentEncoding` field — its `CompressionType` becomes S3 *user metadata* — so a compressed object was stored with no `Content-Encoding` header, `GetObject` saw an empty encoding, skipped decompression, and returned the raw zstd frame while `HeadObject` still reported the uncompressed size from `objectfs-original-size`. Measured on the **shipped default configuration**, which enables both compression and the transporter: an 8,192-byte write read back as 29 bytes with a nil error. The audit classified this as latent because `EnableCargoShipOptimization` is set only in `NewDefaultConfig` and the mount path bypasses it; that was wrong, since `NewBackend(ctx, bucket, nil)`, `NewDefaultConfig()`, and the Go SDK all reach it. The encoding is now recoverable from the object itself, which is what every other S3 client reads too
- **The configured `storage_tier` reaches S3. On the shipped default configuration every object was stored as `INTELLIGENT_TIERING` whatever the tier said.** `NewClientManager` built CargoShip's `S3Config` with a hardcoded `StorageClass: StorageClassIntelligentTiering`, and `Transporter.optimizeStorageClass` falls back to exactly that field for an `Archive` with no `AccessPattern` and no `RetentionDays` — which is every archive ObjectFS constructs. Since `EnableCargoShipOptimization` is true in `NewDefaultConfig`, that was the live path: `storage_tier: STANDARD_IA` stored `INTELLIGENT_TIERING`, with no error, no log line, and no functional symptom. The class now comes from `ConvertTierToCargoShipStorageClass(cfg.StorageTier)`. Worth recording how it hid: every layer between the config and S3 already agreed on the tier — the tier's `ValidateWrite` enforced its 128 KiB billing minimum, the startup log named it, and `ConvertTierToStorageClass` had a passing unit test — because the class an object is actually written with is chosen inside the transporter from a config built alongside, not from the value those layers agree about. A tier defect is silent by nature: the object is readable, nothing fails, and the difference appears on an invoice. Found by asserting the class the endpoint recorded rather than the value passed in, while writing the missing test for `SetObjectMetadata`'s promise to restate the storage class — and now pinned per tier and per upload path, including the default combination that produced it
- `internal/storage/s3/backend.go`: `GetObject` **fails closed** on an object it cannot decode, instead of returning the encoded bytes as if they were the file. `Decompress` returns its input unchanged when the stored `Content-Encoding` is empty or names a codec this build is not configured for, so a raw compressed frame previously reached the caller with a nil error — and because `HeadObject` reports the original size, the kernel padded the shortfall with zeros and `cat` exited 0 on a corrupt file. The read path now dispatches decoding on the object's stored encoding rather than the write configuration, so objects stay readable across a change of codec, and cross-checks the decoded length against `objectfs-original-size`, returning a non-retryable `DATA_CORRUPTION` error when they disagree. This is the guard the SHA-256 added in v0.10.0 was for; it was written and never read
- `internal/storage/s3/backend.go`: a small read of a large object no longer downloads the whole object. The ranged-versus-whole decision was keyed on the compression *configuration*, so with compression merely enabled every read of every object in the bucket fetched the entire object — including objects never compressed, objects below `MinSize`, objects where compression did not help, and objects written by other tools entirely. Measured against real S3 with a fixed 4 KiB read: 15.6× amplification at 16 MiB, 43× at 64 MiB, 216× at 256 MiB (227 ms → 49.2 s), and a 4 KiB read of a 10 GiB object transferred all 10 GiB. The decision now comes from the object: the range is always requested, and the whole object is re-fetched only when the response says it is encoded, or when a 416 turns out to be a range past the end of a *compressed* body. A 416 on an uncompressed object is still reported as an error rather than retried as a whole-object fetch, which would have quietly reintroduced the defect for ordinary reads past EOF. The regression test asserts bytes transferred, not latency: amplification is a byte-count property and a timing assertion would be a flaky proxy for it
- `internal/storage/s3/backend.go`: `GetObject` cannot panic on a negative or out-of-range size. Slicing was `data[offset:offset+size]` with neither reset arm firing for `size < 0`, so `GetObject(ctx, key, 100, -1)` produced `slice bounds out of range [100:99]` — a panic in the FUSE server that unmounted the filesystem under every open descriptor. Reachable from the form documented in `doc.go`, from `internal/cache/multilevel.go`, `internal/cache/predictive.go`, and `internal/distributed/coordinator.go`. Slicing is now one helper that clamps a negative offset, returns empty past the end, and treats a non-positive size as "to the end of the data"
- `internal/storage/s3/backend.go`: `sliceRange` cannot overflow its own bounds check. The fix above compared `offset+size < end`, which is C3 a second time by another route: for a large size the sum wraps negative, compares below `end`, and produces `data[1:-9223372036854775808]`. `FuzzSliceRange` found it in the corrected code within seconds. The comparison is now `size < end-offset`, which cannot overflow because the offset is already clamped into `[0, len(data))` above. Sizes near `math.MaxInt64` are not hypothetical here: `GetObject`'s size is `int64` and reachable from a FUSE read length, from `internal/cache`, and from the distributed coordinator
- **A malformed `storage.s3.region` is rejected at config load instead of several layers below the mount.** A region containing a space, a newline, or a slash passed `Validate`, passed `buildS3Config`, built an AWS client, and then failed inside `NewBackend`'s health check with `501 NotImplemented`, `exceeded maximum number of attempts`, or `resolve auth scheme: resolve endpoint: endpoint rule error` — none of which names the region. Verified against real S3 in `us-west-2`: `US-WEST-2` returns 400, and `us west 2` fails endpoint resolution, because the region is interpolated into a hostname where a space or a slash cannot appear (a slash injects a path segment into it). Found by `FuzzConfigConstructsBackend` three seconds into its first run, which is audit finding C1's exact shape in a second setting: accepted by every layer that reads configuration, rejected only by the layer that acts on it. Checked in both `Configuration.Validate` and `NewBackend`, deliberately — the Go SDK reaches the constructor with a hand-built `&s3.Config{Region: …}` and never passes through the loader
- **`Backend.Close` releases the TCP sockets it was holding.** `ClientManager.Close` drained the `ConnectionPool`, which pools `*s3.Client` values — cheap structs sharing one `http.Transport` — so it freed no sockets at all. The sockets are the transport's idle connections, up to `MaxIdleConns` of them held for `IdleConnTimeout` (90 seconds), and nothing released them. Measured at 2 leaked descriptors per create-and-`Close` cycle, 80 after 40 cycles; a process that builds and closes backends in a loop exhausted the ephemeral port range and failed every subsequent operation with `can't assign requested address`, whatever its configuration. Found by `FuzzConfigConstructsBackend` reporting it as a config defect with a minimized input of the empty string, which is the signature of an environmental failure rather than a finding. `Close` now also calls `transport.CloseIdleConnections`, which leaves an in-flight request alone to finish rather than turning a caller's bug into a confusing I/O error
- **A data race on the S3 acceleration state, on the path of every GET and PUT.** `ClientManager.client` and `ClientManager.accelerationActive` were written by `DisableAcceleration` — reached from `executeWithAccelerationFallback`, so from any request goroutine — and read without synchronization by `IsAccelerationActive` and `GetAcceleratedClient` from every other in-flight request. There was no lock of any kind; `-race` reports the write/read pair immediately once the two are driven concurrently. The window is wide in exactly the circumstance that triggers it: a bucket without the Transfer Acceleration configuration fails *every* request, so the disable path runs on many goroutines at once under load, which is the opposite of a rare interleaving. Both fields are now behind an `RWMutex`, and the caller's check-then-act is collapsed — it asked `IsAccelerationActive` and then `GetAcceleratedClient`, and the fallback can run between the two, so it now asks for the client and tests the result, getting "active, and here it is" as one answer under one lock. `DisableAcceleration` re-checks under the write lock as well, so a burst of acceleration errors logs once rather than once per request. Found by trying to verify a documentation claim about re-enabling acceleration, and it is a live instance of the class `RACE_CONDITION_AUDIT.md` declared eliminated repo-wide
- `internal/storage/s3/backend.go`: `DeleteObject` on a key that does not exist is a no-op, matching the SDK's documented contract. The existence check tested for `*s3types.NoSuchKey`, but `HeadObject` reports absence as `NotFound` — the two are distinct S3 error shapes — so every delete of an absent key returned an error
- **The shipped configuration files now set what they say they set.** `configs/example.yaml` is rewritten as a 60-line copyable starting point in which every key has an effect and is at the schema's own path, and `examples/config.yaml` as the full-schema reference: every key ObjectFS accepts, at its built-in default. The reference marks each key that parses and validates but is read by nothing on the mount path as `not yet wired`, with an issue link — roughly forty of them, including all of `security`, all of `features`, all of `network.{timeouts,retry,circuit_breaker}`, `performance.{read_ahead,parallel_read,write_buffer_size,compression_enabled}`, and all of `cluster`. Disclosing the gap is worse reading than hiding it and better than implying the settings work: `security.encryption.at_rest` in particular defaults to `true` and there is no server-side-encryption header anywhere in the codebase, so the reference now says outright that this setting does not cause SSE and cannot be relied on for an SSE-KMS requirement (buckets have had SSE-S3 on by default since January 2023, so the data is very likely encrypted at rest — just not by ObjectFS). The dead 144-line `backends:` block, which named a schema the loader has never had, is gone
- **The configuration examples in the documentation now match the schema too.** A YAML block in a markdown file with `# /etc/objectfs/config.yaml` above it is a file a user will copy as readily as one under `configs/`, and 11 of the 31 documented blocks that claimed to be ObjectFS configuration did not match the schema. `docs/index.md` — the page the docs site opens on — offered a top-level `s3:` block and a 100 GB persistent cache under keys the loader has never had, which is the shipped-config defect in the one place a new user is most likely to start. `docs/DESIGN_PRINCIPLES.md` presented three nonexistent keys as its **Good: documented configuration with validation** example. `sdks/javascript/README.md` gave the whole file in camelCase, where the schema is snake_case, so every line of it was discarded. Two further blocks assigned the same top-level key twice, silently throwing away half of themselves. Under non-strict decoding these were quietly inert; under strict decoding they are breaking, which is the right trade for real config files and the reason to keep the documented ones correct by machine. `TestDocumentedConfigYAMLMatchesTheSchema` now strictly decodes every YAML block in every markdown file that uses at least one schema key, so this cannot rot again. One file is exempt by name with a reason — `OBJECTFS.md`, a design document describing a schema that was never built, where rewriting the YAML would falsify the record rather than fix it. Two more were exempt for the same reason and are deleted instead, which is the better outcome for an exemption of that shape: an exemption keeps a file a reader can still copy from while removing the one check that would have told them it no longer loads
- **The documented-config test no longer skips the files it exists to check.** Its admission gate asked whether a YAML block used at least one *top-level* schema key, so five blocks in `docs/features/multipart-uploads.md` rooted at a bare `s3:` — the right keys at the wrong depth, since the schema nests them under `storage.s3` — did not "claim to be ObjectFS config", were skipped, and carried their four invented keys straight through the exact test written to notice them. A sixth was in `docs/s3-acceleration.md`, and manual inspection had missed it; widening the gate found it. Wrong-depth YAML is a defect of the same kind as an invented key and has the same consequence for whoever copies it, more so under strict decoding where it now fails startup rather than being ignored. The gate admits nested section names (`s3`, `multipart`, `persistent_cache`, `circuit_breaker`, `retry`, `compression`) as well, and errs toward checking too much: a block with one real key and four invented ones is precisely the failure worth catching. The general lesson is recorded at the gate, because it is not specific to this test — an admission test that decides what to check is itself load-bearing, and "the check passed" means nothing until you know the check ran
- `README.md`: the two configuration examples are schema-correct and load, the setup instruction points at `configs/example.yaml` (the copyable starting point) rather than `examples/config.yaml` (the full-schema reference, which now carries ~40 `not yet wired` markers), and the credential note says outright that credentials do not go in the config file. The "Enterprise Cost Intelligence" and "Enterprise Configuration" sections presented `pricing_config:` as mount-configuration YAML; it is a field on the SDK's `s3.Config` that the mount path does not map, so it was never reachable from a config file and is now rejected outright. Both are rewritten as Go, which is how it is actually reached
- `internal/config`: two tests pin this shut. One asserts every file under `configs/` and `examples/` loads, validates, and strictly decodes — located by glob rather than by a hand-maintained list, because a file added to those directories is a file a user will copy. The other asserts the stronger property strict decoding cannot reach: that a shipped file actually **changes something**, by reflectively comparing the loaded result against the defaults. A file that parses cleanly and leaves every value at its default is still documentation of settings that do not apply. Files exempt from either check are named in a map with a written reason, so "not a config document" and "equals the defaults by design" are claims someone had to make rather than gaps in the check
- `internal/storage/s3/backend.go`: a retried GET can no longer return a previous attempt's `Content-Encoding` or metadata alongside this attempt's body. The retry closure assigned to variables captured from the enclosing scope without resetting them, so a failed attempt that had already set the encoding left it in place for the next one — latent, and one restructuring away from live decoding the wrong way
- **Files smaller than the kernel's read buffer are now served from cache.** The kernel reads a full buffer regardless of how much file remains, so every read of a file below `MaxRead` over-asks past the end: `cat` of a 10 KiB file arrives as `offset=0, size=131072`. v0.10.0 passed that over-ask straight to the cache, which cannot answer it — never told the object's length, it cannot distinguish "the file ends at 10240" from "only 10240 bytes are cached", and answering the short buffer is indistinguishable from a truncated file. So it missed, correctly, every time, and no file below the read-buffer size was ever served from cache no matter how many times it was read. The FUSE read path now clamps the request against the file's current length — from the write path, so a file with pending writes clamps against what it *is* rather than what the object was — before consulting the cache. Ten reads of a 10 KiB file now issue one GET rather than ten
- **A sequential read no longer transfers more bytes than the file contains.** The read-ahead window defaulted to 64 KiB against the kernel's 128 KiB `MaxRead`, and a prefetch shorter than the read it anticipates cannot satisfy that read: the cache answers only for a range it holds in full, so every prefetched entry was walked straight past and its bytes were fetched a second time by the read itself. Measured on a 3 MiB file read sequentially at 128 KiB: 24 reads plus 18 prefetches, **zero of which were ever hit** — 43 GETs and 4,325,644 bytes for a 3,145,728-byte file, a 1.38× amplification whose entire excess was prefetch, paid twice over in egress and in the cache capacity the useless entries occupied. The window is now at least one read's worth, with the configured `WindowSize` as a floor rather than a ceiling; the same traversal issues 24 GETs and exactly 3,145,728 bytes. The regression test asserts bytes transferred rather than the cache's hit rate, because a hit-rate assertion would have **passed** on the defect: the wasted entries were never hit, so they were never counted as misses either
- **Reading a file to its end no longer bills a guaranteed-failing S3 request.** A sequential reader's predicted next offset runs past EOF by construction — the last read of every complete traversal is followed by a prediction one read beyond the end — and v0.10.0 sent it, earning a `416 InvalidRange` on the reliability path: a billed request, an error log line, and a latency spike for a range that cannot exist. Exactly one per file traversal, every time. Prefetches are now clamped against the file's current length and dropped entirely when they start at or past it. The clamp is applied *before* the already-cached check, not after: asking the cache for the unclamped length would miss forever on the last stretch of every file, so each traversal would re-fetch its tail
- **The read-ahead detector now sees cache hits, so a successful prefetch no longer resets the pattern that scheduled it.** `OnRead` was called only on the cache-miss path, so a prefetch that worked hid the next read from the detector; the read after it was compared against the offset of the read *before* the gap, appeared non-contiguous, and reset the sequential-hit count to zero. The counter cycled 0→6→prefetch→0 forever, so exactly one prefetch landed per seven reads and a long sequential traversal never reached steady state. On a 3 MiB file read at 128 KiB this was the difference between 3 and 18 of 24 reads served without blocking on S3
- `internal/fuse/optimizations.go`: the `prefetch` metric reports the bytes actually transferred rather than the bytes requested. Those differ on the last prefetch of every file now that the range is clamped, and a prefetch metric that overstates its own egress is the wrong number to tune a prefetcher with
- `internal/vfs`: a read whose leading bytes are already covered by pending writes no longer fetches them. `Node.ReadRange` narrows the fetch at the head as well as the tail, so the common write pattern — a program that rewrites a file header and then reads further into the object, as an archive tool or a database updating a page after its superblock does — stops paying for bytes it is about to overwrite in memory. `Node.ReadInto` takes the fetched range's offset as a separate argument for this reason: the bytes belong at that offset into the caller's buffer, and splicing a narrowed range at the buffer's start shifts the whole object earlier with no error anywhere. `FuzzNodeLifecycle` catches that shift on its first seed
- **The read cache can hit. The requested length is no longer part of a cached entry's identity.** Both byte-range caches keyed entries as `fmt.Sprintf("%s:%d:%d", key, offset, size)`, where `size` is the length the *caller asked for* rather than any property of the bytes stored. Only a reader that happened to request exactly the length previously stored could ever hit, and the two readers that matter never do: a content read arrives with the kernel's buffer size (`Put("f", 0, <10240 bytes>)` stores `f:0:10240`; `Get("f", 0, 131072)` asks `f:0:131072`) and a short re-read of bytes already held asks for its own shorter length (`f:0:4096`). Both miss, forever, however often they repeat — verified by execution, not by reading. The FUSE metadata cache could not hit even in principle, since it stored a marshaled `ObjectInfo` of about 138 bytes under a `Get` that always asked for 8192, so every `stat` cost one S3 `HeadObject` per path component for the life of the mount. Entries are now identified by `(object key, chunk index)` in the new `internal/cache/chunking.go`, the single place either cache decides what a chunk is: length is a property of the request, which is the distinction the old key collapsed
- `internal/cache`: an entry holds one contiguous *run* of bytes within its chunk, not a whole chunk and not a chunk-aligned prefix. This is what the access pattern requires rather than an optimization: a sequential reader is handed 128 KiB at a time, so its second read starts at 131072 — partway into chunk 0 — and an entry that could only begin at its chunk's start would cache that reader's first buffer and then nothing at all for the rest of the file. Runs coalesce instead, so eight 128 KiB reads become one 1 MiB entry, and a random reader's 4 KiB read costs a 4 KiB entry rather than a 1 MiB one. Where a new run overlaps bytes already held and disagrees with them the newer run replaces the older outright, rather than being overlaid onto it: overwriting `OLD CONTENT HERE` with `NEW` would otherwise leave the cache holding `NEW CONTENT HERE`, which is not what the object contained before the write or after it. Serving stale bytes is what invalidation and the TTL exist to bound; serving bytes no version of the object ever held is caught by nothing
- **`Delete` on one object no longer discards other objects' cached bytes.** Both caches matched `cacheKey[:len(key)] == key` — a bare prefix with no delimiter — so `Delete("logs/app")` also flushed `logs/app2` and `logs/appendix`. Verified by execution: all three gone. Deletion now works from an explicit object-to-chunk index and never parses a composed key apart. The composed key additionally separates its two parts with NUL rather than `:`, because a prefix-with-delimiter scheme is still wrong with `:` — S3 object keys may contain `:` themselves, so `"logs/app" + ":"` is a prefix of the entry key for the distinct object `logs/app:0`. NUL is the one byte an S3 key cannot hold, so no object name can forge a boundary. Both defenses are deliberate: the index is the mechanism and would be correct either way, and the separator makes the key unambiguous on its own so a future reader who does reach for prefix matching gets a sound answer
- **The cache is invalidated on write.** There was no `cache.Delete` call anywhere in `internal/fuse`, so a path kept serving its pre-write bytes and its pre-write size until the TTL expired — five minutes on the default configuration. `FileSystem.invalidate` now drops both the content bytes and the metadata entry on every mutation, because a write changes the size and mtime `Lookup` reports as well as the bytes, and dropping one leaves a file whose `stat` size disagrees with what `read` returns
- **The persistent cache no longer undercounts what it wrote by up to 33×, so it stops filling the disk it was given a budget for.** `writeToFile` measured the entry with `file.Stat()` while the `gzip.Writer` wrapping the file was closed by a *deferred* call, so the size recorded was of a file still mostly buffered in memory: 10 bytes recorded for a 330-byte file, measured. `currentSize` drives `evictIfNeeded`, so the budget was never reached and eviction never fired; `Delete` then subtracted the same wrong figure, so the counter drifted negative over a mount's lifetime and the capacity stopped meaning anything. The compressor is now closed and the file synced before it is measured, and a zero-length file for a non-empty entry is refused rather than indexed. The regression test's first version was itself vacuous and is worth recording: it wrote 200 highly compressible payloads, which gzip reduced to 11,800 bytes against a 204,800-byte capacity, so eviction was never reached and the test would have passed with eviction deleted outright. It now writes incompressible bytes and fails loudly if nothing was evicted, naming which of the two possible causes it is
- `internal/cache`: a `Get` whose range cannot be bounded is refused rather than guessed at. `chunkSpan` returns `ok == false` for a negative offset or a non-positive size instead of defaulting to something plausible, and clamps an end that overflows `int64` — an unbounded request has no chunk range, and leaving that implicit is the same undefined-size hole that produced the C3 panic in the read path. A `size <= 0` means "whatever contiguous bytes are held from offset" and is answered from a single chunk; the contract now says so, and says that a caller reading file content is not one of the callers that may use it
- `internal/cache/persistent.go`: an index entry written by a version with different keying is discarded at load rather than kept. An index persists across restarts and across upgrades, so that loop is the one place another format's records are seen: a pre-chunking entry has no object name and no run length, so keeping it would put a record in the index that `Delete` cannot find and whose file the coverage check would read at the wrong offset. Both failures are silent and the recovery is free — drop the entry and re-fetch
- **The settings in a configuration file reach the S3 backend. Twenty-four of the backend's thirty fields were left at their zero values on every mount.** `buildS3Config` mapped six — region, endpoint, path style, acceleration, compression, and the congestion algorithm — and nothing else, so `storage_tier`, `connection_pool_size`, `max_retries`, both network timeouts, the multipart threshold, chunk size and concurrency, the retry settings, the circuit-breaker settings, and the parallel-read threshold were all named in configuration, documented in `examples/config.yaml`, validated at load, and then discarded at one function (audit finding D12). It is now one explicit assignment per field, ordered to match the backend's struct declaration, because the defect was a mapping somebody extended one field at a time and stopped extending. What each field now does, and what it did before, is worth separating out rather than summarising, since the consequences are not uniform:
  - **A batch read or write on a stock mount no longer hangs forever.** `connection_pool_size` was unmapped, so `PoolSize` reached `GetObjects` and `PutObjects` as `make(chan struct{}, 0)` — an unbuffered channel, not a small semaphore — and the first `semaphore <- struct{}{}` blocked with no receiver. Not a slow batch: a permanently wedged goroutine, with whatever FUSE request was above it. Two independent fixes, deliberately: `NewBackend` defaults `PoolSize`, and `batchConcurrency` floors it at 1 at the point of use. A batch that runs one at a time is slow; a batch that deadlocks is a wedged filesystem
  - **Parallel range GETs happen. The feature v0.10.0 was released for was unreachable from a mount.** The gate is `ParallelReadThreshold > 0`, `buildS3Config` never set it, and `NewBackend` deliberately does not backfill it because zero is how the package spells "off". So the code existed, had tests, and never ran outside them. `performance.parallel_read` now maps, with `enabled: false` expressed as a threshold of zero rather than as a second flag that could disagree with the threshold
  - **`connect` and `read` timeouts are applied to the HTTP transport.** Both were defaulted, documented, and read only to be copied into an error-context map for display. A mount inherited a bare `*net.Dialer` with no timeout, so a connect to an unroutable address hung until the kernel gave up — minutes, with a FUSE request blocked behind it. `read` becomes `ResponseHeaderTimeout` rather than a whole-response deadline, which is the distinction between a working filesystem and a broken one: a ranged GET of a large object legitimately spends minutes streaming its body, and `http.Client.Timeout` would abort it as though S3 had stalled
  - **`storage_tier` is configurable rather than always STANDARD.** The backend's own `StorageTier` field was reachable only from the Go SDK; a mount left it empty, and `NewBackend` filled in STANDARD. Combined with the CargoShip defect fixed above, the tier a config file named had two independent ways of not arriving
  - `max_retries`, `network.retry`, and `network.circuit_breaker` reach the SDK's retryer, ObjectFS's own retryer, and the breaker respectively. The retry mapping starts from `retry.DefaultConfig` rather than building a config from the three configured fields, and that is load-bearing: `retry.New` backfills the delays and the attempt count but **not** `RetryableErrors`, and `shouldRetry` consults that list — so a field-for-field mapping would have produced a retryer that reported three attempts and retried almost nothing, failing a connection reset on the first try while the configuration said otherwise
  - `use_cargoship` and the `multipart` block map. `use_cargoship` defaults to **false** here while `s3.NewDefaultConfig` has it true, and the split is deliberate — that constructor serves a caller who chose the S3 backend explicitly, while this file serves a mount, where the conservative path is the one with the most coverage. Since the flag was previously unreachable from a config file, defaulting it off preserves what mounts actually did rather than switching every deployment's write path on upgrade

  Four fields are still deliberately unmapped, each with its reason recorded at the mapping rather than left as an omission: `TierConstraints` (which would override billing minimums AWS enforces, so no value it could hold is correct), `CostOptimization` (`internal/config.S3CostOptimization` and `s3.CostOptimization` are disjoint types with no field in common, and the backend's tiering machinery has no caller either, so mapping them would wire a config block to an unreachable feature), `PricingConfig` (a rate table belongs in a file of its own), and the three credential fields (a YAML key for a long-lived secret invites it into version control; empty means the AWS default chain, which is what already works)
- **`NewBackend` fills in a default for every field whose zero value is not a usable setting, which is what its doc comment already promised.** It defaulted the four multipart and read-chunk fields and stopped, so the promise held for the shape it named and not for `PoolSize`, `MaxRetries`, `ConnectTimeout`, `RequestTimeout`, `CongestionAlgorithm`, or `RetryConfig.RetryableErrors` — and `&s3.Config{Region: "us-west-2"}`, the shape the Go SDK builds and the shape the comment cited, was exactly the one that got a zero pool size and a retryer that retried nothing (audit finding M18). `ParallelReadThreshold` remains deliberately absent from that list: zero is how this package spells "parallel reads off", so backfilling it would make the feature unswitchable from the SDK
- **A concurrent map write in the cost optimizer can no longer abort the process.** `CostOptimizer.accessPatterns` was a plain map written by `RecordAccess`, which `GetObject` calls on both the serial and the parallel read paths — so concurrent reads of distinct objects wrote the same map from several goroutines, and a concurrent map write is not a race Go tolerates: the runtime aborts with "concurrent map writes". On a mount that is not a failed read, it is the filesystem vanishing with every descriptor open on it (audit finding M12). It was latent only because `MonitorAccessPatterns` defaults false *and* the mount path mapped no part of the cost-optimization block, so the gate at the top of `RecordAccess` always returned early — which means fixing the plumbing is what makes it reachable, and the lock therefore lands in the same change rather than after it. The map and the `AccessPattern` values it holds are now behind a mutex, and nothing hands out a pointer into it: `snapshotPatterns` and `patternFor` return copies, because `RecordAccess` mutates a pattern in place and `analyzeObject` reads six of its fields, so returning the stored pointer would move the race outside the lock while looking fixed. `Backend.GetAccessPatternCount` delegates rather than taking `len()` of the map, which it previously did one call site away from the guard. The regression test drives the read path through `GetObject` against a real S3 endpoint rather than calling the optimizer, and asserts the resulting pattern count as well as the absence of a race — a mutex around a `return` also passes a race test
- **A parallel range read is retried, counted by the circuit breaker, and reported to the health tracker like every other read — so the largest reads are no longer the least protected ones.** The serial path puts every GET behind the retryer, the breaker, and `s3-reads`; `parallelGetObject` called the acceleration fallback directly, with none of the three, on exactly the reads big enough to be worth retrying (audit findings D14 and M13). A single transient 500 on one chunk of a 1 GiB read therefore failed a read that one retry would have completed, and a real S3 outage was invisible to the component whose job is to notice one — the health tracker saw nothing at all from the parallel path, in either direction. Every chunk now goes through `getObjectRange`, which is the one place that stack lives; the range arithmetic itself was correct and is unchanged
- **An assembled parallel read that is not the length it was asked for is an error rather than file content.** The old code joined whatever came back. A chunk answering 206 with fewer bytes than its range produced a short buffer, and `HeadObject` still reported the full size, so the missing tail read back as zeros — silent truncation, which is the worst outcome available on a read path. Each chunk's length is now checked against its range and the total against the requested size. Both forms the shortfall takes are the same error, which took a probe to establish rather than reasoning: S3 clamps a range straddling the end of the object and answers `416 InvalidRange` for one starting at or past it, so an object that shrank produces short chunks at the boundary and refusals after it, decided purely by where the chunks happen to line up. Reporting them as two codes would mean a truncation detected or missed according to chunk alignment
- **Every chunk of a parallel read is checked to have come from the same version of the object.** N ranged GETs are N points in time, and an overwrite landing between the first and the last returns success for each range with lengths that add up — assembling a splice of two generations of the file that never existed in the bucket. Nothing downstream can catch it: the recorded SHA-256 covers the whole object and cannot be checked against an assembled read, so the ETags now compared across chunks are the only integrity evidence a large read has. A backend that returns no ETag on a ranged GET leaves the question unanswered rather than failing the read, on the same reasoning the checksum applies to an object with no recorded hash
- **A failed chunk cancels its siblings, and the cancellation no longer degrades read health.** The old loop returned on the first error and left the remaining goroutines fetching an abandoned read to completion — measured at 7 MiB of an 8 MiB object, egress billed for bytes no caller receives, and on a wedged endpoint goroutines outliving the request. Cancelling turned out to have a trap in it that is worth recording, because the obvious implementation is the wrong one: `errgroup.WithContext` cancels with the failing goroutine's error as the context's *cause*, and Go's HTTP client reports `context.Cause` in preference to `context.Canceled` — so each abandoned chunk's interrupted body read surfaced carrying the *first* chunk's error as though it were its own finding. One shrunken object thus reported its single failure up to eight times into `s3-reads`, against an error threshold of three, taking the component degraded and starting to refuse reads of objects that were perfectly readable. The abandonment now uses an explicit cause that says only what is true of the abandoned request — that it was cancelled — and the real error still reaches the caller through the group's return value
- **Closing the cache while a read is in flight no longer crashes the process.** `PredictiveCache.Close` closed `prefetchQueue`, and that channel's senders are cache reads: `triggerPrefetch` is reached from every L1 `Get`, from arbitrary goroutines, with no way to know when the last one has returned (audit finding M19). A send on a closed channel panics — and it panics inside a `select` with a `default` arm too, since the default covers a full channel and not a closed one — so an unmount racing a read took the process down, which on a mount means the filesystem disappearing under every open descriptor. The queue is now never closed; the workers retire on `stopCh` instead and abandon whatever is queued, which costs a future cache miss and nothing else. `Close` is also idempotent, where before it closed `stopCh` unconditionally and so panicked on its own second call — and a shutdown path is exactly where a double call happens. A read arriving after the close is refused by a check that runs *before* the send is offered rather than as another arm of the same `select`: a `select` chooses uniformly among ready arms, so a closed `stopCh` beside a ready send wins only about half the time, measured at 25 of 64 reads still queuing. The regression test waits for the prefetcher to report queued work before closing, rather than sleeping a millisecond and assuming eight goroutines have accumulated three sequential accesses each by then — which held on an idle machine and failed about half the time under the rest of the package's load. Its own vacuity check is what caught that, reporting correctly that no send had raced the close and so the test had proved nothing; the wait is confirmed still to detect the defect, since restoring both halves of it panics with `send on closed channel` and takes the test binary down
- **Prefetch is no longer permanently starved, so the predictor, the pattern analysis, and the four workers are doing something.** The token bucket refilled `int64(elapsed.Seconds())` tokens — truncating to zero for any call less than a second after the previous one — while assigning `lastRefill = now` unconditionally, which discarded the elapsed time along with it (audit finding M20). At 1 Hz or faster, which is what a cache under load sees, it never refilled at all; and with `tokens` left at the zero value the bucket also started empty, so the first prefetch of a mount's life was refused too. The effect was a rate limiter that worked when idle and not when busy, which is backwards. The refill is now computed in nanoseconds, and `lastRefill` advances only by the span actually converted into tokens, so a remainder too small to be a token is carried rather than dropped on every call
- **The prefetch workers are stopped at unmount, where before they ran for the life of the process.** `PredictiveCache.Close` had no caller anywhere in the repository. `MultiLevelCache` had no `Close` to call it from — `types.Cache` is six methods about bytes and does not include one — and `Adapter.Stop` cleared the cache without releasing it. Since the mount path enables prefetch unconditionally, every mount started four workers and a statistics ticker that outlived it, and a process that mounted and unmounted repeatedly accumulated a set per mount, each pinning the cache it was built over. `MultiLevelCache.Close` now releases every level implementing the new `CacheCloser`, continuing past a level that fails rather than abandoning the rest — this runs on the unmount path, where stopping at the first error would leak the goroutines the method exists to reclaim
- **A multipart upload that does not complete no longer leaves its parts in the bucket.** Every exit that is not a completed upload now issues an `AbortMultipartUpload`, where before only the part-failure path did (audit finding H10). The asymmetry is what made this expensive: `CompleteMultipartUpload` is the last call, so by the time it fails *every part has already landed*, and the one path with no abort was the one that leaked the whole object — a 5 GiB write failing at Complete left 5 GiB. The parts are then invisible to every API a user or an operator is likely to look at: `ListObjects` does not show them, `HeadObject` reports the key absent, and they are billed as storage until an S3 lifecycle rule the operator has to know to write reaps them. Nothing in ObjectFS has ever called `ListMultipartUploads`, so from inside the filesystem the leak was undiscoverable. The abort is now a single `defer` keyed on a completion flag rather than a call on each error path, because the defect was an error path that forgot rather than an abort that was wrong
- **The cleanup abort runs on a context of its own, so it still happens when the caller's context is what failed.** A cancellation — an unmount, a FUSE interrupt, a deadline — is the most common reason a large upload fails, and an `AbortMultipartUpload` issued on the caller's canceled context is never sent, so cleanup would have been skipped on exactly the failure that happens most, making abandoned parts a *consequence of shutting down cleanly*. It uses `context.WithoutCancel` to keep the request-scoped values while shedding the cancellation, with a 30-second timeout of its own so an abort that hangs cannot hold up an unmount. The abort's own failure is logged with the upload ID rather than returned: the upload the caller asked for has already failed, and replacing that diagnosis with the symptom would lose it
- **A failed `rm` now says which object it failed on.** `DeleteObject` opens with a `HeadObject`, because the tier-constraint check needs the object's age, and that read's failure arm reported "failed to get object metadata for deletion validation" without naming the key — while the pool-failure arm three lines down had always named it. It is the arm a bulk delete fails on, so `rm -r` over a thousand objects left the operator with nothing to search for. The key was in the wrapped error's context map and not in its rendered message, and the message is what gets logged. Deleting a key that is *not* there needs no change and is now pinned from both sides: absence is a no-op, which is S3's contract and the Go SDK's documented behavior, but a `HeadObject` that *failed* must not be read as absence — swallowing a throttle there would report a successful delete for an object still in the bucket, and a caller told its delete succeeded has no reason to look again (audit finding M17)
- **A batch read that fetched some of its keys and failed on the rest no longer reports success.** `GetObjects` kept the first error and then discarded it unless *every* key had failed, so 999 keys fetched and one throttled returned a nil error (audit finding H11). The map is the only other channel a caller has, and a key that is missing from it reads as a nil slice — the same value an absent object produces — so there was no way, anywhere in the API, to distinguish "this object does not exist" from "this GET was throttled and is worth retrying". One key failing out of a thousand is both the likely case and the silent one. `GetObjects` now returns the objects it fetched *together with* a non-nil error naming every key it did not, which is the same shape `io.Reader` has: a caller that can use partial results reads the map, one that needs all of them checks the error. The failures are joined rather than formatted into a message, so `errors.As` still reaches the cause underneath and a caller can ask whether the batch failed only on absent objects. `PutObjects` gets the matching treatment — it has no partial-success channel, so its error is the entire report and it now names every object that failed instead of the first, with a count, and attempts all of them rather than stopping, since a caller cannot retry what it cannot learn was never tried. The contract is written on `pkg/types.Backend`, where a second implementation can read it
- **A duration written without a unit is refused at load, naming the key and the rule.** `gopkg.in/yaml.v2` decodes a bare integer into a `time.Duration` as a raw nanosecond count, with no error — so `read: 30`, which is what someone writing a 30-second timeout tries first, configured **30 nanoseconds**, and a negative was accepted as a negative duration. Nothing downstream caught it: every consumer defends against zero (`circuit.NewBreaker`, `Checker.checkLoop` and `Monitor.monitorLoop` all substitute a default at `<= 0`) and a small positive satisfies all of those guards. `FuzzConfigConstructsBackend` found it from a three-line document, `network:\n  timeouts:\n    read: 2` — the value becomes the transport's `ResponseHeaderTimeout`, so every request fails before S3 can begin answering and the mount dies inside `NewBackend`'s health check with "exceeded maximum number of attempts ... timeout awaiting response headers". That message names a network problem. The operator has a plausible-looking number in a file and an error pointing at their network, which is audit finding C1's shape again: accepted by every layer that reads configuration, fatal at the layer that acts on it, attributed to neither. All ten durations in the schema are checked, with a floor of one millisecond — below which nothing in this schema is a setting anyone would want, and above which nothing produced by the unit trap falls. It **refuses rather than clamps**, because a clamp substitutes a duration the operator did not choose and there is no reading of 30 nanoseconds that is what they were asking for. Zero stays valid everywhere and still means "use the built-in default", so a partial config file works as before. The error states the unit rule and shows the value they likely meant, since the fix is to add a suffix and nothing about the file suggests the number was read in nanoseconds. A reflection test walks the config structs and fails for any `time.Duration` the check does not cover — which found `write_buffer.flush_interval` immediately, missed by the grep that produced the hand-written list because the field is declared with different alignment from its neighbours
- **A configuration with no region is refused at load, instead of failing inside a health check.** An empty `storage.s3.region` is legitimate — the AWS SDK resolves it from `AWS_REGION`, the shared config file, or instance metadata — and validation accepted it on exactly that basis, without ever asking whether anything was there to resolve it. Where nothing was, the mount failed several layers down with "failed to resolve service endpoint, endpoint rule error, A region must be set when sending requests to S3", which names no key an operator could edit. `Validate` now refuses it, naming the setting and the environment variables it would accept instead, and `NewClientManager` checks the region the SDK actually resolved so the public Go API gets the same answer. The interesting part is where it was found: `FuzzConfigConstructsBackend` derived it from the single input `storage:` and it failed **on CI while passing locally**, because a developer's shell exports `AWS_PROFILE` or `AWS_REGION` and a container does not — so the defect was invisible precisely where it applied. IMDS is deliberately not probed, since that would put a network round trip with a timeout on the config-load path; an EC2 instance whose region comes only from metadata still needs the setting written down, and the error says so
- **A parallel read that finds a shrunken object reports the finding, not the cancellation it triggers.** The chunk that detects a short or refused range has to cancel its siblings before it can return, so its own error raced the abandonment it caused — and `errgroup` keeps whichever arrives first. When a sibling won, a read of an object that shrank mid-flight surfaced as `context canceled`, carrying `ErrCodeOperationCanceled`, which the health tracker *heals* on. The diagnosis was discarded in favour of its own consequence, and the only evidence that the assembled buffer would have been short went with it. The first real finding is now recorded under its own mutex and preferred over `errgroup`'s value. `TestParallelReadRefusesAShortChunk` caught this on CI after passing 160 consecutive local runs including under `-cpu=1` and `-race`: it needs a slower, more contended machine to lose the race with any regularity, which is the general lesson — a test whose outcome depends on scheduling is passing for the wrong reason on some machine
- **The lint job now runs. It never had.** `golangci/golangci-lint-action@v6` refuses to install golangci-lint v2 — `invalid version string 'v2.12.2', golangci-lint v2 is not supported by golangci-lint-action v6` — so the job errored out in about fifteen seconds, before reading a line of Go, on every commit since the gate was added. This is worse than a check that fails: a check that fails is read, and a check that dies during setup renders as a red job beside a wall of other red jobs and gets attributed to something else. Every commit in this release had therefore gone unlinted. Fixing it surfaced 70 findings in this branch's own new code, of which three were real defects — the two `defer`-versus-parallel-subtest lifetime bugs and the property-test distribution collapse, all three listed here. The action is now used with `install-only: true` and the binary invoked as ordinary shell in the steps that follow, because the action runs golangci-lint at most once and three steps need it: previously each step re-entered the action, and the whole-repo informational step ran a bare `golangci-lint run` that was `command not found`, since nothing but the action puts it on `PATH` — and its `|| true` made a missing binary and a clean repository the same observation. The blocking step's `--new-from-merge-base` is preceded by an explicit `git fetch` of the base branch: `fetch-depth: 0` fetches every branch on a push, but a `pull_request` checkout fetches the merge ref and does not guarantee the base is among them, so without it the gate can silently degrade to linting the entire repository and timing out
- **`go vet ./...` passes, so the CI step that runs it can gate anything.** `sdks/c/main.go`'s handle table converted an int64 index to and from `C.objectfs_client_t` with `unsafe.Pointer(uintptr(id))`, which vet reports as "possible misuse of unsafe.Pointer" — correctly, since the rule exists because Go's garbage collector may move an object and cannot update a reference it cannot see is one, and it has no way to express "this pointer is a token." Both lines carried a `//nolint:govet` that had never suppressed anything, because `go vet` runs outside golangci-lint; `.golangci.yml` had a path exclusion for the same reason, now removed. The cast moved into two `static inline` functions in the cgo preamble, which makes the property structural rather than asserted: the value never exists as a Go pointer at all. The `test` and `coverage` jobs had been failing on this warning, so a real test failure in either would have been indistinguishable from the standing one. Verified with `go build -buildmode=c-shared` and the C SDK's own suite — 16 C assertions and 15 Python ctypes assertions, including the NULL-handle arms that exercise the conversion
- **Two test fixtures were being torn down while parallel subtests still used them.** `TestUndecodableObjectFailsClosed` held `defer encoder.Close()` and `TestPooledOperationsReachTheConfiguredEndpoint`'s neighbour held `defer cm.ReturnPooledClient(pooled)`, in functions whose subtests call `t.Parallel()` — so the parent returned, the `defer` fired, and the zstd encoder was closed and the pooled client handed back to be drawn by another caller while the subtests were still using them. Both are now `t.Cleanup`, which is scheduled after the subtests rather than at the parent's return. Neither had ever failed, which is the point: the outcome depends on whether the runtime schedules a parallel subtest before or after the parent's `defer`, so both tests were passing for a reason unrelated to what they assert
- **Three property tests were exploring a fraction of the operation sequences they claim to.** `for i := 0; i < rng.IntN(7); i++` re-draws its bound on every pass, so the loop stops as soon as one roll lands at or below `i`. Measured over a million iterations rather than reasoned about: the mean falls from 3.000 operations to 2.016, and six-operation programs drop from 14.34% of runs to **0.62%** — a 23× under-sampling of exactly the longest sequences, which are where an extent list's splice and truncate interactions actually compose. The bound is now drawn once. This is the class of defect the whole release exists to fix, in the harness rather than the code: a test that reports a number of runs it did not perform, and reports success either way
- **A byte range S3 refuses no longer counts as a service failure.** `translateError` had no arm for `InvalidRange`, so its deliberately pessimistic default rendered it `ErrCodeStorageRead`, which `errors.IsServiceFailure` counts. That is a read *past* the end of a file, which is the most ordinary pattern there is — a caller sizes a read by its buffer, so the last read of every file asks for more than is there — and reads at exactly EOF ask for a range starting past the end. Three of those degraded `s3-reads`. It is now `ErrCodeValidationFailed`: a non-failure, and honest, since the request was invalid for this object and no retry changes that
- **`buildS3Config` carries the encryption block to the backend.** This is the seam that made P-7 a defect rather than merely a missing feature: even had the header code existed, the mapping did not, so the backend would have received the zero value on every mount. It is now asserted in `internal/adapter/config_mapping_test.go` field by field against written-out values, and mutation-tested — deleting the three-line mapping fails three named assertions rather than passing quietly, which is what it did before the assertions existed
- **A prefetch no longer fetches bytes the read it is anticipating fetches too.** Matching the read-ahead window to the read size — the fix above for a window too short to satisfy anything — left the prefetcher and the reader issuing byte-identical requests, since a prefetch covers the range the reader is predicted to want next at the length of the read that predicted it. Nothing deduplicated them, so which one reached S3 first was a race between a network round trip and the application's next `read(2)`: won, the prefetch was useful; lost, the same range was fetched and billed twice. Measured on a 3 MiB sequential traversal at 128 KiB reads, an idle machine transferred exactly the file and a loaded one transferred **5,373,952 bytes across 41 GETs** — 24 reads plus 17 prefetches, every prefetch duplicated. A GET that is **contained** by one already in flight now waits for it instead of being sent, so total bytes transferred is a property of the read pattern rather than of how busy the machine is. Containment and not equality, because the two requests are *not* identical whenever the reads are smaller than the read-ahead window: the window is a floor, so a 1 KiB reader gets a prefetch for the whole 64 KiB ahead of it — a strict superset, which a key made of `(path, offset, length)` hashes differently and collapses into nothing. Every measurement above used 128 KiB reads against the 64 KiB default, where the floor never engages and the ranges really are equal, so that case went unexercised until a 16 KiB file read in 1 KiB steps transferred **27,648 bytes, 1.7×, every excess byte a duplicate**. The re-check of the cache happens inside the shared flight as well, since a caller that missed and then waited may find the bytes already stored by the time it runs. Found by a test that passed locally and failed in CI, which is the correct outcome for a defect that only appears under load — and the reason the assertion is on bytes transferred rather than on a hit rate: the duplicated fetches were never counted as misses either
- **A prefetch that would re-fetch the read that predicted it now starts after it, rather than over it.** Containment above covers a prefetch that arrives while a read is in flight and a read that arrives while a prefetch is, but not the case where the prefetch range *contains* the in-flight read without being contained by it — which is what a reader smaller than the window produces every time it wins the race. Nothing can serve that prefetch from the read's result, since the read holds fewer bytes than the prefetch wants, so it issued a second overlapping GET and the bytes already in flight were paid for twice. The prefetch now advances its start past any in-flight fetch covering its first byte and shortens accordingly. Trimming rather than skipping, and the distinction is the whole point: skipping outright also transfers exactly the file, by never prefetching at all — a reader that consistently wins the race has a read in flight every time a prefetch is scheduled, so a 16 KiB file read in 1 KiB steps went to 16 GETs, one per read, with read-ahead contributing nothing. Trimmed, it is 8, the tail fetched once and ahead of the reader. Overlap that begins after the prefetch's own offset is deliberately left alone: truncating there would abandon the whole read-ahead window over a single small read outstanding in the middle of it, and splitting around it would issue several GETs where the point is to issue one
- **`internal/network`'s Linux implementation is tested, and its coverage floor is now set from Linux.** The package splits on `//go:build`: macOS compiles a 24-line stub where Linux compiles 66 lines of procfs parsing, socket options, and a dialer hook. The floor was measured on macOS, where it read a clean 100%, so the three functions only Linux compiles had no test at all — `setTCPCongestion` at 0%, `newPlatformDialer` at 20% with its `Control` closure never once invoked because nothing ever dialed, and both procfs readers missing their absent-file arms, which is the case that actually varies between a host and a container. CI measured 86.9% against a floor of 100 and had been failing on it. The new tests dial a real loopback connection so the hook runs, set and read `TCP_CONGESTION` back to confirm the option takes rather than merely that the call was made, and point the procfs paths at fixtures so the parse is checked against a kernel's actual output shape instead of against whichever single shape the host happens to have. Linux is now at 98.8%. The general lesson is recorded in `.coverage-floors`: a per-package floor set on one platform is a claim about a file set that platform does not compile, so CI is the authority for any package with a build-tag split
- **`internal/awsname` covers the default shared-config path, so its floor of 100% is honest.** Every case in `TestRegionIsResolvable` sets `AWS_CONFIG_FILE` explicitly, deliberately, so the result does not depend on whether the machine running the test has a config file — and the consequence was that the fallback to `~/.aws/config`, the arm that runs whenever the variable is unset, was the one path with no test. That is the common case in production. It is now exercised by redirecting `HOME` at a temporary directory, which keeps the test hermetic while making the fallback observable, along with the `os.UserHomeDir` failure arm and a directory at `AWS_CONFIG_FILE` — where the stat succeeds, so without the `IsDir` check the SDK would be handed a path it cannot read
- `pkg/types.Cache` and `pkg/types.WriteBuffer` now state their contracts, which is where the defects above were free to differ between two implementations of the same six methods. `Cache` says that a partial hit is a miss and why (the cache is never told an object's length, so it cannot distinguish "the object ends here" from "only this much is cached", and answering the short buffer is indistinguishable from a truncated file), that implementations must copy what they keep and must not retain the returned slice, that newer bytes win on an overlapping `Put`, and that `Delete` must remove every chunk of exactly the named object and nothing belonging to any other. `WriteBuffer` gains the `ReadAt` and `FileSize` methods the rebuilt write path added, with the reason a read path must prefer them to the backend. `internal/cache/doc.go` carries the three points a caller gets wrong, each of which was a shipped defect

### Deprecated
- `internal/storage/s3/config.go`: `pricing_config.use_pricing_api` no longer has any effect. Setting it logs a deprecation warning at startup. Use `pricing_config.custom_pricing` to override rates per tier (#161)

## [0.10.0] - 2026-02-23 — WITHDRAWN

**This release is withdrawn and must not be used.** A deep audit found defects that prevent the
shipped default configuration from mounting and that silently lose or corrupt user data. Every one
of them is fixed in 0.10.1 above; upgrade rather than pinning here.

- **Cannot mount on the default configuration.** `internal/config/config.go` defaults
  `compression.algorithm` to `gzip`, but `internal/compression/codec.go` implements only `none`,
  `zstd`, and `lz4`. Every code path that reads config treats `gzip` as valid — only the codec
  factory disagrees — so `objectfs s3://bucket /mnt` exits with `Failed to start adapter`.
- **Offset writes truncate the object.** The write-buffer flush callback in
  `internal/adapter/adapter.go` receives `(key, data, offset)` and calls
  `backend.PutObject(ctx, key, data)`, discarding the offset. Because `PutObject` is a
  whole-object replace, appending one byte to a 1 MiB file leaves a 1-byte object. Non-contiguous
  writes (SQLite, mmap writeback, `tar`, HDF5) return `EIO` instead. Flush errors are recorded to
  a stats counter and not returned, so `close(2)` reports success after a failed upload.
- **Read amplification on every object when compression is enabled.**
  `internal/storage/s3/backend.go` decides whole-object-versus-ranged fetch from the compression
  *configuration* rather than from the object being read, so a ranged read of any object — including
  objects never compressed and objects written by other tools — downloads the whole object and
  disables parallel reads bucket-wide. Measured against real S3 with a fixed 4 KiB read: 15.6× on a
  16 MiB object, 43× at 64 MiB, 216× at 256 MiB. A 4 KiB read of a 10 GiB object transfers 10 GiB.
- **Silent corruption when the codec configuration changes.** `Decompress` in
  `internal/compression/s3_integration.go` returns the payload unchanged when the stored
  `Content-Encoding` does not match the configured codec, so an object written with zstd and read
  after switching to lz4 emits the raw compressed frame with exit status 0. The
  `objectfs-sha256` metadata this release added is written and never read, so nothing catches it.
- **The read cache cannot hit and is never invalidated.** The cache key includes the requested
  *length*, so the `Lookup` metadata cache never hits, short reads at EOF are uncacheable, and the
  16 MB chunked cache population added in this release is unreachable. There are no `cache.Delete`
  calls in `internal/fuse`, so a read after a write on the same descriptor returns pre-write bytes
  for up to the 5-minute TTL.
- **The headline feature of this release is inactive in production.** `buildS3Config` maps 6 of
  roughly 30 `s3.Config` fields and does not map `ParallelReadThreshold`; `NewBackend` does not
  backfill it. The parallel range GET path is gated on `threshold > 0`, so it never runs on a real
  mount. `PoolSize` is likewise unmapped, leaving a zero-capacity semaphore that blocks forever in
  `GetObjects`/`PutObjects`.
- **`rm` and `rmdir` reported success without deleting** (fixed in `[Unreleased]`, see #163).
- **Windows is not supported.** The `cgofuse` build tag has never compiled.

### Added
- `internal/storage/s3/config.go`: Three new `Config` fields — `ParallelReadThreshold` (default 64 MB), `ReadChunkSize` (default 16 MB), `ParallelReadConcurrency` (default 0 = inherit `MultipartConcurrency`) — control parallel range GET fan-out for large objects (#128)
- `internal/storage/s3/backend.go`: `parallelGetObject()` — fans out a large read into N concurrent range GETs bounded by `ParallelReadConcurrency`, assembles results in order; used automatically when the object/read size exceeds `ParallelReadThreshold` and compression is inactive (#128)
- `internal/config/config.go`: `ParallelReadConfig` struct and `PerformanceConfig.ParallelRead` field for YAML/env configuration of the parallel-read feature (#128)
- `internal/storage/s3/backend.go`, `multipart_upload.go`: Content SHA-256 stored as `objectfs-sha256` in S3 user metadata on every `PutObject` (standard, CargoShip, and multipart paths); always computed from the uncompressed canonical content so the hash is stable regardless of storage encoding (#129)
- `internal/storage/s3/backend.go`: `HeadObject` now populates `ObjectInfo.Checksum` from the `objectfs-sha256` metadata key; returns empty string for objects written before v0.10.0 (backward compatible) (#129)

### Changed
- `internal/fuse/filesystem.go`, `cgofuse_filesystem.go`: Cache population after a backend read now splits large results into 16 MB chunks so future partial reads hit the cache instead of fetching from S3 again (#130)

## [0.9.0] - 2026-02-23

### Added
- `pkg/api/server.go`: `MountManager` interface and four REST endpoints (`POST /api/v1/mounts`, `DELETE /api/v1/mounts/{point}`, `GET /api/v1/mounts`, `GET /api/v1/mounts/{point}`) for remote mount/unmount operations; existing deployments without a `MountManager` receive `501 Not Implemented` (#123)
- `pkg/api/server.go`: `ServerConfig.Version` field; `GET /info` now reports the version supplied at construction time instead of a hardcoded `"0.6.0"` string; falls back to `"unknown"` when the field is empty (#118)
- `internal/cache/multilevel.go`: `MultiLevelCache.SetBackend(types.Backend)` setter and a working `Warmup([]string) error` implementation that fetches each key from the configured backend and populates all enabled cache levels; no-op when backend is nil (#120)
- `internal/fuse/cgofuse_filesystem.go`: Nine `sync/atomic.Int64` counter fields (`statsLookups`, `statsOpens`, `statsReads`, `statsWrites`, `statsBytesRead`, `statsBytesWritten`, `statsCacheHits`, `statsCacheMisses`, `statsErrors`) incremented on every operation; `GetStats()` now loads real values instead of returning zeros (#121)
- `internal/health/monitor.go`: `Recoverable` interface (`Recover(ctx context.Context) error`); `attemptAutoRecovery` now calls `Recover` on any registered component that implements it, retrying up to `MonitorConfig.RecoveryAttempts` times with `RecoveryDelay` between attempts and logging success/failure (#122)
- `internal/cache/multilevel_bench_test.go`: Four benchmarks (`Get_HotPath`, `Get_Miss`, `Set_Eviction`, `Warmup_10keys`) plus parallel Get — run with `go test -bench=. ./internal/cache/...` (#125)
- `internal/buffer/buffer_bench_test.go`: Five benchmarks (`Write_1KB`, `Write_1MB`, `Flush_1MB`, `Concurrent_Write`, `FlushAll`) — run with `go test -bench=. ./internal/buffer/...` (#125)
- `internal/adapter/adapter_bench_test.go`: Seven benchmarks covering `parseSize` and `validateStorageURI` under serial and parallel load — run with `go test -bench=. ./internal/adapter/...` (#125)
- `internal/filesystem/filesystem_test.go`: `mockFilesystem` and `mockFileHandle` stubs implementing all 28 methods of `FilesystemInterface`; compile-time satisfaction assertion (`var _ FilesystemInterface = (*mockFilesystem)(nil)`) plus 11 table-driven tests covering all method groups and helper utilities (#126)

### Changed
- `internal/storage/s3/cost_optimizer.go`: `applyOptimization` now calls `s3.CopyObject` in-place (same bucket and key) with the target `StorageClass`, replacing the previous log-only stub; updates local access-pattern tracking on success (#119)
- `internal/adapter/adapter.go`: `Stop()` now calls `a.cache.Clear()` and `a.metrics.Stop(ctx)`, replacing the two `// TODO` placeholder comments (#124)
- `sdks/go/objectfs/client.go`: `Client` gains a `sync.RWMutex` field; `Mount`, `Unmount`, `IsMounted`, and `Close` use the mutex to guard the `mounted` bool and `adptr` pointer — makes the SDK fully safe for concurrent use (#127)

## [0.8.0] - 2026-02-23

### Added
- `internal/distributed/cluster.go`, `internal/distributed/coordinator.go`: `ClusterManager.SetBackend` and `Coordinator.backend` wire the `types.Backend` S3 backend into `executeLocally`, replacing the in-process stub with real `GetObject`/`PutObject`/`DeleteObject`/`ListObjects` calls; nil backend returns a descriptive error instead of phantom data (#85)
- `internal/distributed/gossip.go`, `internal/distributed/cluster.go`: Distributed cache invalidation broadcast — new `MessageTypeCacheInvalidate` gossip message type, `ClusterManager.SetCache` / `InvalidateCacheKey` methods, and `handleIncomingMessage` dispatch that calls `cache.Delete(key)` on all peers within one gossip round-trip (#86)
- `internal/storage/s3/backend_bench_test.go`: Eight new S3 backend benchmarks (GetObject 1 KB / 1 MB / 10 MB, PutObject 1 KB / 1 MB, ListObjects 100 / 1000 entries, concurrent Get) using an in-process stub; run with `go test -bench=. ./internal/storage/s3/...` (#88)
- `scripts/pjdfstest.sh`: Shell harness that mounts ObjectFS against a test bucket and runs pjdfstest for POSIX compliance validation; `make test-posix` target added to Makefile (#89)
- `sdks/java/`: Java 17 SDK scaffold — `ObjectFSClient`, `ObjectFSConfig`, `ObjectInfo`, `MountOptions`, `ObjectFSException`, `NotFoundException`, Maven pom.xml, and unit tests using MockWebServer (#90)

### Changed
- `internal/distributed/coordinator.go`, `internal/distributed/gossip.go`, `internal/distributed/cluster.go`, `internal/distributed/consensus.go`, `internal/health/checker.go`, `internal/health/remediation.go`, `internal/fuse/cgofuse_filesystem.go`, `internal/fuse/filesystem.go`, `internal/fuse/mount.go`, `internal/adapter/adapter.go`, `internal/cache/redis/invalidation.go`, `pkg/profiling/memory.go`: All `log.Printf` calls migrated to structured `slog.Info`/`slog.Warn`/`slog.Error` with key-value attributes (#87)

## [0.7.3] - 2026-02-23

### Fixed
- `internal/storage/s3/multipart_state.go`: `UpdatePartStatus` now uses `m.mu.RLock()` and releases it before calling `state.MarkPartCompleted`/`state.MarkPartFailed`; previously held the manager write lock while those methods acquired the state lock — two-lock nesting chain that can deadlock under concurrent part uploads (#108)
- `internal/fuse/filesystem.go`: `FileHandle.Write` now updates `fh.file.size` inside the `accessMu` critical section alongside `dirty`, `modified`, and `lastAccess` — eliminates data race on `OpenFile.size` under concurrent FUSE writes (#109)
- `internal/fuse/filesystem.go`: `FileHandle.Flush` now reads and resets `fh.file.dirty` under `accessMu` — eliminates data race with concurrent `Write` calls that set `dirty` under the same lock (#110)
- `internal/fuse/optimizations.go`: `ReadAheadManager.Stop()` now uses `sync.Once` to prevent a panic on second call (`close of closed channel`) from defensive teardown paths (#111)
- `internal/buffer/writebuffer.go`: `WriteBuffer.Sync()` now checks `ctx.Done()` in its polling loop — the previous implementation ignored context cancellation, blocking the caller for the full `MaxWriteDelay * 2` timeout even after the context was cancelled (#112)
- `internal/buffer/writebuffer.go`: `flushStaleBuffers` now acquires `buf.mu.RLock()` before reading `buf.flushing`, `buf.dirty`, and `buf.lastWrite` — eliminates data race with concurrent `flushBuffer` calls that set `buf.flushing` under `buf.mu` (#113)
- `internal/distributed/cluster.go`: `performHealthChecks` now accepts and threads the cluster lifecycle context; `TriggerElection` is called with that context instead of `context.Background()`, so election goroutines exit cleanly when the manager is stopped (#114)
- `internal/distributed/gossip.go`: `performGossip` now skips member-list entries where `node.Info == nil` — previously dereferenced `node.Info.ID` unconditionally, panicking on nodes added via sync messages with a nil Info field (#115)
- `internal/distributed/gossip.go`: `calculateStats` no longer writes `time.Since(LastMessageReceived)` to `AvgMessageLatency`; that value measured cluster idle time (grows when quiet), not message round-trip latency; the field stays zero until real per-message timing is instrumented (#116)
- `internal/cache/multilevel.go`: `MultiLevelCache.Evict` now measures `level.Cache.Size()` before and after each level eviction and accumulates the difference — the previous code accumulated `levelStats.Size` (total level occupancy), which could falsely report `totalEvicted >= size` when nothing was freed (#117)

## [0.7.2] - 2026-02-23

### Fixed
- `internal/distributed/consensus.go`: `resetElectionTimer` now uses `time.NewTimer` instead of `time.AfterFunc` — `time.AfterFunc` returns a `*time.Timer` with a nil `.C` channel, so the election loop blocked forever on a nil channel and elections never fired (#101)
- `internal/distributed/gossip.go`: `receiveMessages` no longer busy-loops when `conn == nil` (10 ms sleep) and no longer blocks indefinitely in `ReadFromUDP` (100 ms `SetReadDeadline`); stop channels are checked at the top of each iteration so the goroutine exits cleanly on shutdown (#102)
- `internal/cache/persistent.go`: `Clear()` now captures `len(c.index)` before resetting the map; previously the count was read after the reset and always returned 0, so eviction stats were never updated (#103)
- `internal/cache/predictive.go`: `IntelligentPrefetcher` updates `stats.JobsQueued` and `stats.JobsCompleted` via `sync/atomic.AddUint64` instead of unprotected increments — eliminates data race under concurrent worker goroutines (#104)
- `internal/fuse/optimizations.go`: `performPrefetch` now captures `fetchStart := time.Now()` before calling `GetObject`; the previous `time.Since(time.Now())` evaluated to ~0 on every call, making prefetch latency metrics useless (#106)
- `internal/fuse/filesystem.go`: `OpenFile` gains an `accessMu sync.Mutex` field protecting `lastAccess` and `accessCount`; both `Read` and `Write` paths now acquire the lock before updating these fields — eliminates data race under concurrent FUSE I/O (#107)

## [0.7.1] - 2026-02-23

### Fixed
- `internal/fuse/mount.go`: `MountManager` gains a `sync.Mutex` field (`mu`) protecting the `mounted`, `currentOpID`, and `server` fields, which were previously accessed without synchronisation from `Mount()`, `Unmount()`, a background goroutine, `IsMounted()`, `GetCurrentOperation()`, and `Remount()` — eliminates data race detected by `-race` (#98)
- `internal/fuse/mount.go`: `MountWatcher.checkMount()` removed spurious `!` operator from `actuallyMounted := !w.manager.isAlreadyMounted()` (should be `isAlreadyMounted()`, not its negation) — prevents permanent false "unexpected unmount" log warnings on every watcher tick; `Remount()` double-negation also cleaned up (#99)
- `internal/adapter/adapter.go`: write buffer `MaxBufferSize` and `FlushThreshold` now use the configured `MaxMemory` value directly instead of dividing by 100 and 200 — the placeholder divisions reduced a 512 MB configured buffer to ~5 MiB, degrading write throughput by ~100× (#100)

## [0.7.0] - 2026-02-23

### Added
- `internal/fuse`: `getCachedInfo` / `cacheInfo` now serialize and deserialize `types.ObjectInfo` as JSON, replacing the placeholder string that always caused cache misses; repeated `stat` calls on a cached path no longer fall through to a backend `HeadObject`; 6 new unit tests (`filesystem_test.go`) cover round-trip fidelity for all fields including `time.Time`, `map[string]string` metadata, ETag, ContentType, Checksum; nil-cache and nil-info no-ops; and overwrite semantics — closes #79
- `internal/cache`: `MultiLevelCache` gains an optional `*analytics.Predictor` field (injected via `MultiLevelConfig.Predictor`); `Get` and `Put` call `predictor.RecordAccess` on every access so the predictor builds frequency/recency profiles; `shouldPromoteToL2` consults the predictor's tier recommendation (Standard/Standard-IA → promote, Glacier tiers → skip) with a size-based fallback when no access history exists; `generateEvictionCandidates` (previously a stub returning an empty slice) now iterates `AccessPredictor.patterns` and scores each entry using directly-computed recency and recent-hour frequency (clamping future timestamps, correct for patterns with < 2 accesses); `tests/MockBaseCache.Delete` now decrements `stats.Size` to correctly reflect intelligent eviction; 7 new tests (`predictive_test.go`, `multilevel_test.go`) cover candidate generation, eviction-score ordering, sequential-offset prefetch prediction, predictor access recording, predictor-driven L2 promotion, and size-fallback promotion — closes #82
- `internal/adapter`: health monitor lifecycle wiring — `Adapter` struct gains a `*health.Monitor` field; `Start()` constructs and starts an `internal/health.Monitor` (when `Monitoring.HealthChecks.Enabled` is true) that runs background `system_ping`, `memory_usage`, and `disk_space` checks plus per-component checks for `s3_backend`, `cache`, and `write_buffer`; the HTTP health endpoint is bound on `config.Global.HealthPort` (default 8081); `Stop()` cleanly shuts down the monitor; a new private `healthComponent` adapter struct implements `health.HealthyComponent` via a closure so adapter-owned objects need not implement the interface directly; 4 new tests cover initial nil state, graceful nil-monitor stop, full start/stop lifecycle, and `healthComponent` correctness — closes #74
- `internal/health`: implemented the previously stubbed `Checker.startHTTPServer()` — binds a `net.Listener` on the configured port (supports `:0` for ephemeral ports), serves `application/json` at `config.HTTPPath` with the current check status and `200`/`503` based on overall health, and shuts down cleanly when `stopCh` is closed
- `pkg/api`: `GET /metrics` endpoint now returns valid Prometheus text-format output — `Server` accepts a `prometheus.Gatherer` as a new fourth constructor parameter; `handleMetrics` delegates to `promhttp.HandlerFor`; when `nil` is passed the endpoint returns an empty 200 response; `internal/metrics.Collector` exposes a new `Gatherer() prometheus.Gatherer` accessor so callers can wire the existing registry directly into the API server; 3 new tests (`TestHandleMetrics_NilGatherer`, `TestHandleMetrics_MethodNotAllowed`, `TestHandleMetrics_WithGatherer`) verify correct HTTP status, `Content-Type`, `# HELP`/`# TYPE` comment lines, and presence of the three core metric families — closes #75
- `internal/distributed`: real UDP networking for consensus (Raft-like leader election) and coordinator (operation routing) — `GossipProtocol` now dispatches 6 new message types (`request_vote`, `request_vote_resp`, `append_entries`, `append_entries_resp`, `node_operation`, `node_operation_resp`) plus `sendConsensusMsg` helper and `LocalAddr()` accessor; `ConsensusEngine` replaces goroutine-sleep simulations with real UDP `RequestVote`/`AppendEntries` RPCs and handlers; `Coordinator` routes operations to remote nodes over UDP with request/response correlation via `pendingOps` channels and a `simulateReplication` fire-and-forget path; pre-existing election-timer data race fixed in `electionLoop`; two-node loopback tests added (`TestConsensusEngine_TriggerElection_WithPeer_BecomesLeader`, `TestConsensusEngine_TriggerElection_WhenLeader_IsNoOp`, `TestCoordinator_ExecuteOperation_TwoNodes_RealUDP`) — closes #84
- `internal/fuse`: `NewCgoFuseMountManager` now reads `DefaultUID`/`DefaultGID` from `MountConfig.Permissions` (when non-zero) instead of hardcoding 1000; `NewFileSystem` nil-config default also uses `os.Getuid()`/`os.Getgid()`; 4 new tests cover explicit permissions, nil permissions, and zero-value permissions (all fall back to process identity) — closes #78
- `github.com/scttfrdmn/globalfs` (`pkg/site`, `pkg/namespace`): wired objectfs v0.7.0 as the first real dependency in GlobalFS; `SiteMount` wraps an `ObjectFSClient` interface (List, Head, Health, Close) per configured site — production path uses `objectfssdk.New`, test path accepts any mock; `Namespace` provides a merged view across multiple `SiteMount`s with key deduplication (first/highest-priority site wins) and graceful skip of unreachable sites; 9 new tests in `pkg/site` and `pkg/namespace` cover delegation, deduplication, limit, unavailable-site tolerance, and dynamic site addition — closes #83

## [0.6.0] - 2026-02-22

### Added
- `internal/distributed`: 67 unit tests across 4 new files (`cluster_test.go`, `consensus_test.go`, `coordinator_test.go`, `gossip_test.go`) covering `ClusterManager` lifecycle and node management, `ConsensusEngine` election state machine (Follower→Candidate→Leader with peer simulation), `Coordinator` operation routing and load-balancing strategies (round-robin, least-load, consistent-hash), and `GossipProtocol` message handlers (join, alive, suspect, dead, sync, heartbeat) — closes #73
- `tests/aws_s3_test.go`: three new integration tests under the `aws_s3` build tag — `TestListObjects` (prefix listing + limit parameter), `TestMultipartUpload` (6 MB object with 5 MB threshold to force the multipart code path, partial-read verification), `TestZSTDCompression` (ZSTD round-trip, partial read, raw-storage verification that bytes are actually compressed on S3)
- `sdks/go/objectfs/client_test.go`: `TestIntegration_PutGetDeleteHead` (full + partial Get, Head metadata, Delete + confirmation), `TestIntegration_List` (prefix listing + limit + cleanup), `TestIntegration_Health`; helper functions `testBucket()` and `testRegion()` read from `$OBJECTFS_TEST_BUCKET` / `$AWS_REGION` instead of hard-coding; existing `TestNew_*` and `TestClose_NotMounted` updated to use the same helpers
- `Makefile`: `test-aws` target runs the `aws_s3` suite and Go SDK integration tests with credential validation; `test-release-check` target combines unit tests and AWS integration as a pre-release gate
- `DEVELOPMENT.md`: v0.6.0 pre-release integration test procedure — environment setup, per-test coverage table, FUSE smoke test procedure, post-run cleanup, and an acceptance checklist (closes #72)
- `internal/cost`: real-time per-operation S3 cost calculation, per-tenant accumulation, ROI reporting, and budget-threshold alerting — `PriceTable` holds immutable per-tier pricing with optional overrides (7 tiers, AWS us-east-1 defaults); `Calculator` computes `OpCost` (request + retrieval + egress fees) via `Calculate(op, tier, bytes, egressBytes)` and periodic `CalculateStorageCost`; `Reporter` accumulates `TenantRecord` state (op counts, op costs, storage cost, GB-months) with thread-safe `RecordOp`/`RecordStorage` and emits `CostReport` snapshots including ROI `BaselineCost`/`Savings` computed from actual GB-months; `AlertManager` evaluates `BudgetRule` (soft limit → WARNING, hard limit → CRITICAL, optional info fraction → INFO) after each cost event with wildcard `"*"` tenant fallback, duplicate-fire suppression, and async `AlertHandler` callbacks; 42 unit tests cover all operation types, retrieval fees, egress fees, tier storage ordering, ROI calculation, tenant isolation, reset, wildcard rules, handler dispatch, and severity deduplication (closes #65)
- `internal/analytics`: ML-based access pattern analysis and S3 tier recommendations — `PatternAnalyzer` tracks per-object access stats (access rates over 1d/7d/30d, recency, inter-access interval mean/variance, hour-of-day and day-of-week histograms) via a sliding window and extracts a `FeatureVector`; `TierClassifier` applies a calibrated decision tree to map features to an S3 storage tier (STANDARD, STANDARD_IA, GLACIER_IR, GLACIER, DEEP_ARCHIVE) with confidence score and estimated monthly savings per GB; `Predictor` facade exposes `RecordAccess`, `RecordAccessAt`, `Recommend`, `RecommendBatch`, and `Stats` with atomic counters; 30 unit tests cover feature extraction, all decision-tree branches, boundary conditions, batch recommendations, and stats accounting (closes #64)
- `internal/cache/redis`: Redis-backed distributed cache implementing `types.Cache` — `Cache` struct wraps `go-redis/v9`; `Get` uses `GETRANGE` for partial reads (offset/size support); `Put` stores full objects only (partial writes are silently dropped to maintain cache consistency); `Delete` removes a key; `Evict` increments the eviction counter and returns false (Redis manages eviction via its configured `maxmemory-policy`); `Size` reads `used_memory` from `INFO memory`; `Stats` returns atomic hit/miss/eviction counters and computed hit rate; `Close` closes the connection; `Client` exposes the underlying `*goredis.Client` for invalidation wiring; 13 unit tests using `miniredis/v2` cover connectivity, full/partial reads, partial-write rejection, delete, TTL expiry, namespace isolation, and stats
- `internal/cache/redis`: `Invalidator` — pub/sub cache-invalidation broadcaster on channel `"objectfs:invalidation"`; `Publish(ctx, key)` sends `"<nodeID>:<key>"` messages; `Subscribe(ctx)` starts a goroutine that deletes received keys from the local cache, skipping messages originating from the same node; 3 unit tests cover publish, remote invalidation, and self-publish suppression
- `internal/cache`: `NewFromConfig(cfg)` factory — returns a Redis `Cache` when `cfg.Cluster.Enabled && cfg.Cluster.Redis.Enabled`; falls back to `MultiLevelCache` with defaults otherwise; 3 unit tests cover all three routing paths
- `internal/config`: `RedisConfig` struct with `Enabled`, `Address` (default `localhost:6379`), `Password`, `DB`, `KeyPrefix` (default `"objectfs"`), `TTL` (default 5 min), `MaxRetries` (default 3); `ClusterConfig.Redis RedisConfig` field; defaults wired into `NewDefault()`; `RedisConfig` re-exported via `pkg/types` alias (closes #63)

## [0.5.0] - 2026-02-22

### Added
- `internal/compression`: adaptive algorithm selection — `Analyze(data)` detects content class (text, JSON, binary, compressed, archive) via magic bytes and Shannon entropy, returning a `CompressScore` ∈ [0,1]; `RuleSelector` maps content class + `AccessHint` (hot/warm/cold) + object size to a `Recommendation` (algorithm + level); `AdaptiveSelector` wraps `RuleSelector` with a per-`ContentClass` rolling-window feedback model — once `minSamples` (10) outcomes are recorded per algorithm it overrides the base recommendation with the empirically best choice (speed-optimised for hot, ratio-optimised for cold); `LZ4Codec` added as a fast-decompression option using `github.com/pierrec/lz4/v4` frame format; `New()` factory extended to support `"lz4"`; 37 new unit tests covering magic-byte detection for 15+ formats, entropy boundary conditions, rule table, adaptive learning, window eviction, and LZ4 round-trip (closes #62)
- `internal/archive`: archive detection and indexing API — `Detect([]ObjectInfo)` filters S3 listings to `[]ArchiveMetadata`; `DetectKeys` returns just the archive keys; `DetectInPrefix(ctx, backend, prefix, limit)` combines `ListObjects` + `Detect`; `IsArchiveKey` wraps `pkg/archive.IsArchive`; `BuildIndex(ctx, backend, key)` downloads and indexes an archive via `GetObject`, supplements with real S3 timestamps from `HeadObject` (non-fatal); `BuildIndexFromBytes(key, format, data)` builds an `ArchiveIndex` by walking tar headers without retaining file data; `openTar` shared by `BuildIndexFromBytes` and `VFS.extractFile`; `pkg/archive.IsArchive` minimum-length guard lowered from 7 to 5 to correctly handle `.tgz` filenames with single-character base names; 21 unit tests for indexing and 14 tests for detection (closes #58)
- `internal/archive`: virtual filesystem (VFS) layer for archive contents — `PathTranslator` splits FUSE paths at archive boundaries (`data.tar.zst/subdir/file.txt` → archiveKey + innerPath) for all three supported formats; `VFS` provides `Stat`, `ReadDir`, and `ReadFile` over archive contents backed by `types.Backend`; index built by walking tar headers on first access (no file data retained) and cached for all subsequent Stat/ReadDir calls; file content extracted on-demand and cached per entry; `Invalidate` clears both index and content caches; synthesises virtual directory entries for archives created without explicit directory records; `ErrNotFound` sentinel error; 28 unit tests cover all three formats, virtual directories, caching, offset reads, and not-found paths (closes #59)
- `internal/network`: TCP congestion control package — runtime detection of available algorithms (`/proc/sys/net/ipv4/tcp_available_congestion_control`), per-socket `TCP_CONGESTION` socket option (Linux ≥ 4.9) via `net.Dialer.Control`; `NewBBRDialer()`, `NewCUBICDialer()`, `BestAvailableDialer()` (BBR > CUBIC > system default); `Monitor` struct with atomic `BytesSent`/`BytesReceived`/`Connections`/`Errors` counters and `WrapDialContext` for transparent tracking; `BBRConfig` with 4 MiB send/receive buffers and ICW=10; build-tag stubs for non-Linux platforms; `NewDialer(algo)` wired into `s3.NewClientManager` via custom `*http.Transport.DialContext`; `CongestionAlgorithm` field added to `s3.Config` and `config.NetworkConfig` (default `"auto"`); unit tests cover algorithm selection, detection, Monitor lifecycle, WrapDialContext success/error, and BBR config (closes #60)
- `pkg/compression` + `internal/compression`: ZSTD compression engine with configurable levels (1–22) using `github.com/klauspost/compress/zstd`; public `Codec` interface (`Compress`, `Decompress`, `Algorithm`, `ContentEncoding`) and `Algorithm` constants (`zstd`, `gzip`, `lz4`, `none`); `ZstdCodec` using concurrency-safe `EncodeAll`/`DecodeAll`; `Compressor` wrapper with minimum-size threshold and incompressibility guard (discards compressed form when it is not smaller than the original); transparent S3 integration — `PutObject` compresses and sets `Content-Encoding: zstd`; `GetObject` fetches the full object when compression is active, decompresses, then slices to the requested byte range; multipart uploads propagate `Content-Encoding` through `CreateMultipartUpload`; default config: algorithm=zstd, level=3, minSize=4KB; unit tests (40 tests, no AWS required) cover round-trip, all levels 1–22, concurrent access, incompressible data, parseSize, and Compressor lifecycle (closes #61)
- C SDK (`sdks/c/`): shared library (`libobjectfs.so`/`.dylib`) via `go build -buildmode=c-shared`; public header `objectfs.h` with full documented API; opaque handle system with per-handle error strings; operations: New, Free, Get, Put, Delete, Head, List, Mount, Unmount, LastError, FreeData, FreeList; C test suite (16 tests) and Python ctypes smoke test (15 tests) both pass without AWS credentials; integration tests gated on `OBJECTFS_TEST_BUCKET` + `AWS_ACCESS_KEY_ID` (closes #71)
- Go SDK (`sdks/go/objectfs`): type-safe client for direct S3 object operations (Get, Put, Delete, List, Head) and optional FUSE mount/unmount; functional options (WithRegion, WithEndpoint, WithCacheSize, WithMaxConcurrency, WithLogLevel, WithMetricsPort, WithHealthPort, WithTLS); five sentinel errors (ErrNotFound, ErrAccessDenied, ErrNotMounted, ErrAlreadyMounted, ErrInvalidConfig) compatible with errors.Is; unit tests for all options and error-path logic without AWS credentials (closes #70)
- `pkg/archive`: new package with `ArchiveMetadata`, `ArchiveIndex`, and `ArchiveEntry` types; format detection via `IsArchive()` for tar.zst, tar.gz, and tar.bz2 — objectfs-side metadata types for CargoShip archive integration
- Python SDK (`sdks/python/objectfs`): async/await S3 client, CLI, monitoring, and configuration presets; GCS/Azure backends stubbed for future support
- `internal/fuse`: unit tests for `ReadAheadManager` (pattern detection, sequential/non-sequential reads, prefetch scheduling, TTL cleanup), `WriteCoalescer` (accumulation, merge, flush triggers), `Stats` (concurrent safety), and config defaults; cgofuse mock-based tests for Getattr, Open/Read, Write, Release, Readdir, and concurrent handle management (gated behind `//go:build cgofuse`)

### Changed
- Upgraded `github.com/scttfrdmn/cargoship` v0.4.5 → v0.13.0 (DVC pipeline auto-discovery, archive filesystem shell, Glacier restore, incremental sync, deduplication)
- Go toolchain bumped to 1.26.0 (pulled in by cargoship v0.13.0)
- Transitive upgrades: `aws-sdk-go-v2` v1.39.2 → v1.41.0, `testify` v1.10.0 → v1.11.1
- Dockerfile base image updated: `golang:1.24-alpine` → `golang:1.26-alpine`, `alpine:3.18` → `alpine:3.21`

### Fixed
- `internal/storage/s3`: three multipart upload correctness bugs exposed by real AWS integration tests — (1) zero-value `MultipartThreshold`/`ChunkSize`/`Concurrency` in partial configs now get defaults applied in `NewBackend` so every object no longer triggers multipart and the semaphore no longer deadlocks; (2) `MultipartUploadState` gains a `sync.RWMutex` to eliminate the data race between `MarkPartCompleted` writes and `GetProgress` reads; (3) `uploadParts` now sorts `completedParts` by `PartNumber` before returning — S3 `CompleteMultipartUpload` requires ascending order but goroutines complete in arbitrary order; (4) `CalculateOptimalChunkSize` enforces S3's 5 MB minimum non-last-part size — previously returned `baseChunkSize/2` for files just above threshold, producing parts rejected with `EntityTooSmall`
- `sdks/go/objectfs`: `requireAWS` test helper now accepts `AWS_PROFILE` in addition to `AWS_ACCESS_KEY_ID`; `TestNew_WithDefaults` and `TestClose_NotMounted` pass `testRegion()` to avoid `301 MovedPermanently` when test bucket is not in `us-east-1`
- `internal/storage/s3`: refactored `putObjectMultipart` (209 lines → 55 lines) by extracting five helper methods into `multipart_upload.go` — `initiateMultipartUpload`, `uploadSinglePart`, `uploadParts`, `abortMultipartUpload`, `completeMultipartUpload`; pure `partSlice` helper extracted and covered by `TestPartSlice` (5 subtests) (closes #55)
- `internal/adapter`: removed hardcoded `us-west-2` region; adapter now reads `Storage.S3.Region`, `Storage.S3.Endpoint`, `Storage.S3.ForcePathStyle`, and `Storage.S3.UseAcceleration` from `config.Configuration` via new `buildS3Config()` helper (closes #56)
- `internal/config`: added `AWS_DEFAULT_REGION`, `AWS_REGION`, `OBJECTFS_S3_REGION`, and `OBJECTFS_S3_ENDPOINT` environment variable mappings so region and endpoint are configurable without a config file; priority order is `AWS_DEFAULT_REGION` < `AWS_REGION` < `OBJECTFS_S3_REGION`
- `internal/health`: implemented all 8 `AutoFix` remediation closures — `memory_force_gc` (runtime.GC + debug.FreeOSMemory), `disk_clean_logs` (removes .log files older than 30 days), `disk_clean_cache` (removes objectfs-cache files older than 7 days), `disk_clean_temp` (removes objectfs-* temp files older than 24h), `network_retry` (context-aware 5 s wait); `s3_restart_connection`, `cache_clear`, and `memory_reduce_cache` log advisory messages until dependency injection is available
- License references updated from MIT to Apache 2.0 throughout README, SDKs, and docs
- Copyright year updated to 2025-2026
- Pre-commit `check-yaml` hook now uses `--unsafe` to handle MkDocs Python YAML tags
- Markdownlint config: relax line-length limit and allow emphasis for README badge lines

## [0.1.0] - 2025-07-27

> **The performance figures in this entry were never measured.** "4.6x performance improvement over
> direct S3 access" was CargoShip's number for CargoShip's workload; the throughput, cache-hit, and
> concurrency figures below came from no benchmark in this repository. They are left in place, like the
> withdrawn 0.10.0 entry above, because a changelog is a record of what was published rather than a
> document to be quietly corrected — but they should not be cited. `benchmarks/` is where a real
> number would come from, against a named bucket, region, and object size.

### Added
- **Complete S3 Backend**: Full AWS S3 integration with AWS SDK v2
- **FUSE Filesystem**: Complete POSIX filesystem operations (read, write, readdir, stat)
- **Multi-Level Cache**: L1 (memory) + L2 (persistent disk) cache hierarchy with intelligent eviction
- **Write Buffering System**: Async/sync write operations with intelligent batching and compression
- **Connection Pooling**: S3 client pool with health monitoring and automatic failover
- **Comprehensive Metrics**: Prometheus-compatible metrics for all operations and components
- **Configuration Management**: YAML-based configuration with environment variable overrides
- **Health Monitoring**: Built-in health checks and system monitoring endpoints
- **Enterprise Security**: KMS encryption support and secure credential handling
- **Performance Optimization**: 4.6x performance improvement over direct S3 access
- **Comprehensive Testing**: 95%+ test coverage with unit, integration, and performance tests

### Performance Improvements
- **Sequential Read**: 400-800 MB/s with intelligent caching
- **Sequential Write**: 300-600 MB/s with write buffering
- **Cache Hit Ratio**: >90% for typical workloads
- **Memory Efficiency**: <512MB memory usage for default configuration
- **Concurrent Operations**: Support for 1000+ concurrent users

### Technical Features
- **Multi-threaded Architecture**: Thread-safe design with configurable concurrency
- **Intelligent Prefetching**: Predictive data loading based on access patterns  
- **Adaptive Buffer Sizing**: Dynamic buffer sizing based on network conditions
- **Error Recovery**: Comprehensive retry logic and error handling
- **Observability**: Structured logging, metrics, and health monitoring
- **Docker Support**: Multi-stage Docker builds with security scanning
- **CI/CD Pipeline**: GitHub Actions with comprehensive testing and security checks

### Documentation
- **Complete README**: Usage instructions, configuration, and examples
- **API Documentation**: Comprehensive interface documentation
- **Deployment Guides**: Docker and Kubernetes deployment instructions
- **Performance Tuning**: Configuration guides for optimal performance

### Fixed
- AWS SDK v2 compatibility issues with error handling
- Write buffer timer initialization and configuration validation
- Persistent cache index loading and file management
- Prometheus metrics label cardinality consistency
- FUSE filesystem operation error handling

### Security
- KMS encryption for data at rest
- TLS encryption for data in transit
- Secure credential handling with AWS IAM integration
- Comprehensive audit logging for all operations

[Unreleased]: https://github.com/scttfrdmn/objectfs/compare/v0.11.0...HEAD
[0.11.0]: https://github.com/scttfrdmn/objectfs/compare/v0.10.3...v0.11.0
[0.10.3]: https://github.com/scttfrdmn/objectfs/compare/v0.10.2...v0.10.3
[0.10.2]: https://github.com/scttfrdmn/objectfs/compare/v0.10.1...v0.10.2
[0.10.1]: https://github.com/scttfrdmn/objectfs/compare/v0.10.0...v0.10.1
[0.10.0]: https://github.com/scttfrdmn/objectfs/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/scttfrdmn/objectfs/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/scttfrdmn/objectfs/compare/v0.7.3...v0.8.0
[0.7.3]: https://github.com/scttfrdmn/objectfs/compare/v0.7.2...v0.7.3
[0.7.2]: https://github.com/scttfrdmn/objectfs/compare/v0.7.1...v0.7.2
[0.7.1]: https://github.com/scttfrdmn/objectfs/compare/v0.7.0...v0.7.1
[0.7.0]: https://github.com/scttfrdmn/objectfs/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/scttfrdmn/objectfs/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/scttfrdmn/objectfs/compare/v0.1.0...v0.5.0
[0.1.0]: https://github.com/scttfrdmn/objectfs/releases/tag/v0.1.0
