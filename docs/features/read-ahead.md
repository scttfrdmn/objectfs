# Read-ahead

ObjectFS prefetches ahead of a reader that is reading sequentially, so the next read can be served from
cache instead of a round trip to S3. That is the whole of the feature: one detector, five settings.

This page used to describe three selectable strategies (`simple`, `predictive`, `ml`), a pattern
detector with a confidence threshold and a prediction window, a bandwidth-capped prefetcher, and an
online-learning model with a persistable `.bin` file and a tuning guide for its learning rate. None of
it existed. The `performance.read_ahead` block those keys belonged to was decoded, range-checked at
load, shipped in four preset files — and read by no code at all, because the mount constructed its
read-ahead manager with a nil config and ran that manager's own defaults
([#176](https://github.com/scttfrdmn/objectfs/issues/176)). The block is wired now and has been reduced
to the settings the prefetcher actually has. A file that still sets a removed key fails to load with
that key named, rather than having it silently discarded.

## How it decides to prefetch

Every read is reported to the detector, cache hit or miss. The detector keeps one pattern per open path:

1. A read whose offset is exactly where the previous read ended increments that path's sequential-hit
   count. Any other offset resets it to zero.
2. When the count reaches `min_sequential` *and* a derived confidence exceeds 0.5, a prefetch is queued
   for the bytes immediately after the current read.
3. A prefetch worker performs the GET and puts the result in the cache. The queue holds 100 requests;
   beyond that, prefetches are dropped rather than blocking the read that triggered them.
4. A pattern not touched for `ttl` is forgotten, and the next read starts a fresh sequential run.

There is no pattern classifier, no temporal or frequency analysis, and no model. A read is sequential or
it is not.

### How much is prefetched

The prefetch length is the **larger** of `window_size` and the reader's own last read size.

The floor is not the interesting half; the `max` is. A prefetch shorter than the read it anticipates
cannot satisfy that read, because the cache answers a `Get` only when it holds the *whole* requested
range — a partial hit is a miss, since it cannot distinguish a short object from a partially cached one.
So a 64 KB prefetch ahead of a reader taking 128 KB bites produces an entry that every subsequent read
walks straight past, and those bytes are paid for twice: once in egress, once in the cache capacity they
occupy while never being read.

That was measured rather than reasoned about. Reading a 3 MiB file sequentially at the kernel's 128 KB
`MaxRead` issued 24 reads plus 18 prefetches of 64 KB, of which **zero** were ever hit — 43 GETs and
4,325,644 bytes transferred for a 3,145,728-byte file. With the prefetch at the read size, the same
traversal issues 24 GETs and exactly 3,145,728 bytes, and 3 of the 24 reads are served from cache.

## Configuration

```yaml
performance:
  read_ahead:
    enabled: true
    window_size: 64KB     # floor on the prefetch length
    min_sequential: 3     # consecutive sequential reads before prefetching starts
    concurrent_reads: 4   # prefetch workers, one GET each at a time
    ttl: 5m               # how long an idle read pattern is remembered
```

Those are the defaults, and they are also what every mount ran before the block was wired — closing the
plumbing gap deliberately did not change behavior at the default configuration.

| Key | Meaning | Notes |
|---|---|---|
| `enabled` | Prefetching on or off | `false` is the only way to turn it off; see below |
| `window_size` | Floor on the prefetch length | Empty is rejected when enabled: an empty floor is a floor of zero, not the default |
| `min_sequential` | Sequential reads before prefetching | Values below 6 are inert; see [#247](https://github.com/scttfrdmn/objectfs/issues/247) |
| `concurrent_reads` | Prefetch workers | Must be > 0 when enabled; zero starts no workers |
| `ttl` | Idle pattern lifetime | Needs a unit suffix: `5` means five *nanoseconds* and is rejected |

### Three things worth knowing before tuning

**`min_sequential` below 6 does nothing.** The prefetch requires the hit count *and* a confidence above
0.5, and confidence is `sequentialHits / 10` — the same counter in different units. So 1, 2, 3, 4 and 5
all behave identically: the first prefetch happens on the sixth consecutive sequential read. The
shipped default of 3 is in that range, which means the documented default does not describe the default
behavior. Reconciling the two thresholds changes prefetch behavior at the default configuration, so it
is filed separately as [#247](https://github.com/scttfrdmn/objectfs/issues/247) rather than adjusted
here. Above 6, `min_sequential` governs alone and means what it says.

**`concurrent_reads: 0` is not "no limit".** It is the worker count, and each worker is a goroutine, so
zero starts none: every prefetch is queued and never performed. That is read-ahead silently off while
the configuration says it is on, so it is rejected at load rather than accepted.

**`enabled: false` is the only way off.** There is no second flag, and no combination of the numeric
settings that amounts to disabling it. Setting `window_size` to zero or `min_sequential` very high is
rejected or ineffective respectively.

`ttl` and `window_size` are checked for *syntax* whether read-ahead is enabled or not, while the other
three are checked only when it is enabled. The reason is that `ttl: 5` and `window_size: 64 kilobytes`
are typos rather than settings — the first is five nanoseconds, because a duration without a unit
suffix is read as a raw nanosecond count with no complaint — and reporting a typo only once the feature
is switched on means it surfaces as a validation failure over a line nobody touched, in a release that
changed nothing about it. The accepted suffixes are `ns`, `us`, `ms`, `s`, `m`, `h`.

### Environment variables

```bash
export OBJECTFS_READAHEAD_ENABLED=true          # strictly "true"; anything else is false
export OBJECTFS_READAHEAD_WINDOW_SIZE=256KB
export OBJECTFS_READAHEAD_MIN_SEQUENTIAL=6
export OBJECTFS_READAHEAD_CONCURRENT_READS=2
```

The two integer variables report a parse failure rather than silently keeping the default: a worker
count quietly reverting to 4 when you meant 1 is prefetch traffic you did not ask for. There is no
variable for `ttl`.

`OBJECTFS_READAHEAD_STRATEGY`, `OBJECTFS_READAHEAD_PATTERN_DETECTION` and
`OBJECTFS_READAHEAD_ML_PREDICTION` are gone with the fields they set. So is `OBJECTFS_READ_AHEAD_SIZE`,
whose target `performance.read_ahead_size` was a second, separately-defaulted name for the same
quantity as the block above — and was also read by nothing.

## Worked profiles

Three presets ship in `examples/config/`:

| File | For |
|---|---|
| `readahead-streaming.yaml` | Video, log processing, sequential whole-file reads. 1 MB floor, 8 workers, 30 m TTL |
| `readahead-low-bandwidth.yaml` | Metered or constrained links. 128 KB floor, 1 worker, 12-read threshold, 1 m TTL |
| `readahead-disabled.yaml` | Random access, databases on the mount, exact egress accounting |

`readahead-simple.yaml` became `readahead-disabled.yaml`: it configured `strategy: simple`, described as
"no pattern detection, no prefetching", and the honest spelling of that is read-ahead off.
`readahead-ml.yaml` was deleted rather than rewritten — it set a model path for a model loader that does
not exist, and a preset cannot be corrected into configuring a feature that was never built.

## Tuning

The one measurement that matters is **bytes transferred versus bytes read**. Prefetching is a bet, and
its cost is precisely the bytes fetched and never used; latency improvements are the payoff but they do
not tell you what the bet cost. A 4 KB read of a large object with a large `window_size` can transfer
far more than it returns.

- **Random-access workload** — turn it off. Prefetches behind random reads are never hit, and the
  detector's reset-on-non-sequential means they will rarely fire in the first place, so the honest
  setting is `enabled: false` rather than a conservative tune.
- **Reader taking small bites of a large file** — raise `window_size`. This is the case the floor exists
  for: at 4 KB reads, the prefetch equals the read and buys nothing.
- **Egress-billed link** — lower `concurrent_reads` to 1 and raise `min_sequential` above 6, where it
  starts having an effect. That combination makes each wrong bet small and rare.
- **Paused-then-resumed readers** — raise `ttl`. A log tailer between batches otherwise re-establishes
  its sequential run from scratch every time.

There are no read-ahead metrics on a mount. The predictive cache computes prediction accuracy and
prefetch efficiency, but the mount's instance is wrapped in an opaque `types.Cache` with no accessor
reaching past it, so those numbers have nowhere to go —
[#223](https://github.com/scttfrdmn/objectfs/issues/223). Until that lands, bytes transferred is
measured from outside: CloudWatch, or a request count against a known read volume.

## Not the same thing as parallel reads

`performance.parallel_read` fans a *single* large read into concurrent range GETs. Read-ahead fetches
bytes *no one has asked for yet*, ahead of a sequential reader. They are independent, both apply on the
same mount, and neither applies to compressed objects — a compression frame cannot be sliced, so a
ranged read of one has to fetch the whole object.

## The predictive cache is a different subsystem

`internal/cache/predictive.go` is a separate mechanism with its own pattern analysis, its own prefetch
workers, and a `Predictor` interface. It sits inside the multi-level cache, below the read-ahead manager
described on this page. Its `Predictor` is never set on the mount path, so its size heuristic is what
runs; there is no model, and nothing to train. It can be constructed directly:

```go
import "github.com/scttfrdmn/objectfs/internal/cache"

config := &cache.PredictiveCacheConfig{
    EnablePrediction:    true,
    PredictionWindow:    150,
    ConfidenceThreshold: 0.8,
    EnablePrefetch:      true,
    MaxConcurrentFetch:  6,
    PrefetchAhead:       4,
}

predictiveCache, err := cache.NewPredictiveCache(config)
if err != nil {
    log.Fatal(err)
}
defer predictiveCache.Close() // retires the prefetch workers; nothing else will

stats := predictiveCache.GetPredictiveStats()
fmt.Printf("Prediction Accuracy: %.2f%%\n", stats.PredictionAccuracy)
```

That is a *second* predictive cache, independent of the mount's. It observes the accesses you make
through it and nothing the filesystem does.

## Implementation

- `internal/fuse/optimizations.go` — the detector, the prefetch workers, and `prefetchLength`
- `internal/config/config.go` — `ReadAheadConfig`, its defaults and its validation
- `internal/adapter/adapter.go` — `buildReadAheadConfig`, the mapping from YAML to the manager

## Related

- [Configuration reference](../index.md) — every key the loader accepts, and the **Not yet wired up**
  table
- [Multipart Upload Optimization](./multipart-uploads.md)
