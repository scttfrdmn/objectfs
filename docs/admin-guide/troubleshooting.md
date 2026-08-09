# Troubleshooting

A decision tree for the four things that go wrong, each branch ending at a command that answers the
question rather than at advice.

Two conventions used throughout. Commands are what this binary actually accepts — checked by a test,
not transcribed from a plan. And where a branch has no diagnostic, it says so: knowing that a
question cannot be answered from outside is worth more than a suggestion that reads like one.

## Mount fails

Read the message before anything here. ObjectFS distinguishes its failures on purpose, so the message
already narrows this tree to one branch:

| Message | Exit | Branch |
| --- | --- | --- |
| `objectfs mount: ...` naming a value, or a config line | 2 | [Configuration](#configuration) |
| `failed to initialize S3 backend: ...` | 1 | [Credentials and bucket](#credentials-and-bucket) |
| `failed to initialize cache: ...` | 1 | [Cache directory](#cache-directory) |
| `failed to mount filesystem: ...` | 1 | [FUSE](#fuse) |

Exit **2 is a wrong command line or config file** and exit **1 is a correct command whose operation
failed** — so the exit code alone tells you whether to re-read your config or look at the host.

Two orderings decide which of two real faults you see first, and neither is arbitrary: the config
file is loaded before either positional argument, so a syntax error in it is reported even when the
command line is also wrong; and the storage URI is validated before the mount point, so `objectfs
mount s3://b /nonexistent` names the bucket and says nothing about the directory.

### Configuration

```bash
objectfs mount --config /etc/objectfs/config.yaml --dry-run
```

This is the first command to run for any mount problem, and it needs no credentials, no network and
no FUSE. It loads the file, applies the environment overrides, validates everything, prints what it
resolved, and exits without mounting.

Unknown keys are **rejected**, naming the key and the line. A misspelled key was silently discarded
before v0.10.1, which is how a config file could be 162 lines of settings that did nothing — so if
you are carrying a file forward from an older version, this is the command that finds out.

### Credentials and bucket

ObjectFS uses the standard AWS credential chain, so:

```bash
aws sts get-caller-identity            # are there credentials at all
aws s3 ls s3://your-bucket/            # can they reach this bucket
```

If `aws s3 ls` works and ObjectFS does not, the difference is almost always the **region** or an
**endpoint**: `storage.s3.region` has a default, and a bucket in another region answers with a
redirect that surfaces as an initialization failure. `--dry-run` prints the resolved storage URI.

### Cache directory

Only reachable with `cache.persistent_cache.enabled`. The directory must exist and be writable by the
mounting user; under systemd that is the unit's user, not yours.

### FUSE

```bash
ls -l /dev/fuse                        # present?
sudo modprobe fuse                     # if not, on Linux
which fusermount3                      # needed for an unprivileged mount
```

`fusermount3` is needed only for an **unprivileged** mount — a root mount goes through `mount(2)`
directly. That is why the `.deb` and `.rpm` packages make `fuse3` a Recommends rather than a Depends,
and it is why a minimal container image can fail here while the same binary works on the host.

In a container, the mount needs `--device /dev/fuse` and `--cap-add SYS_ADMIN`. Neither
`deploy/kubernetes/` manifest uses `privileged: true`, which would grant every capability and device
for the sake of that one.

## Cache hit rate is low

First, get the number rather than estimating it. From the metrics endpoint:

```bash
curl -s http://127.0.0.1:8080/metrics | grep objectfs_cache_requests_total
```

```text
objectfs_cache_requests_total{service="objectfs",type="hit"} 3
objectfs_cache_requests_total{service="objectfs",type="miss"} 1
```

The hit rate is `hit / (hit + miss)`. There is deliberately no `objectfs_cache_hit_rate` gauge: a
ratio computed at scrape time loses the counts, and Prometheus can divide.

**Before treating a low number as a problem, check that the workload can be cached at all.** A
single-pass traversal — one `tar` over a dataset larger than the cache, or a checksum run — has no
reuse, so its hit rate is *correctly* near zero and no amount of cache will change it. The metric
that matters there is throughput, not hit rate.

If there is reuse:

- **Single node** — the working set does not fit. Raise `performance.cache_size`, or enable
  `cache.persistent_cache` to spill to local disk, which is the cheaper of the two.
- **Multi-node** — each node caches independently and there is no shared cache. See
  [sizing](distributed.md#sizing-the-cache), whose formula divides the hot set by the node count and
  deliberately does *not* multiply by `replication_factor`.
- **Entries evicted before they expire** — `cache.max_entries` bounds the count independently of
  `performance.cache_size`, so a workload of many small objects can hit the entry limit with the byte
  budget half empty.

Per-level numbers, when the read cache is multi-level:

```bash
curl -s http://127.0.0.1:8080/metrics | grep objectfs_cache_size_bytes
```

## Gossip is not forming

**This is the failure with no error message**, and it is the reason
[distributed.md](distributed.md#the-failure-mode-this-page-exists-for) leads with it. Each node comes
up as a cluster of one: it mounts, serves reads, reports healthy, and receives no cache invalidation
from any peer — so it can serve an object another node has already overwritten. Gossip is one-way
UDP, so there is no handshake to fail and nothing to time out.

In order, because each step rules out the next:

**1. Is the configuration what you think it is?**

```bash
objectfs mount --config /etc/objectfs/config.yaml --dry-run
```

Warnings go to stderr and the exit code stays 0. The two that matter here are an empty
`cluster.seed_nodes` and a loopback `cluster.advertise_addr` — both legal, both a cluster of one on
any node that is not the first of a new cluster. A **wildcard** `advertise_addr` is refused outright,
because a peer dialing a wildcard reaches its own loopback.

**2. What does each node think its membership is?**

```bash
objectfs cluster status
objectfs cluster status --json | jq '.membership, .peers'
```

`No peers.` means this node has not joined. Run it on **every** node — two nodes can disagree, and
that asymmetry is itself the diagnosis: a firewall permitting A→B and dropping B→A gives you exactly
that, because gossip is one-way.

Note the exit codes, which differ from what you might assume: **0** when no node is dead or suspect
*and also* when clustering is disabled, because disabled is the default rather than a fault; **1**
when the instance could not be reached or a node is dead or suspect; **2** for a bad command line.
There is no quorum condition, because nothing elects a leader.

**3. Is the secret the same on every node?**

A mismatch is logged on the **receiving** node, so collecting logs from one node will not show it:

```text
level=WARN msg="rejected gossip message" peer=10.0.1.2:54123 hint="a peer with a different cluster secret, ..."
```

The hint is one of three and they send you to different places:

| Hint mentions | Cause |
| --- | --- |
| a different cluster secret, or a host that is not a member | secret mismatch, or an unrelated host on the port |
| a build that predates gossip authentication | a mixed-version cluster mid-upgrade |
| clock skew beyond the freshness window | NTP, on one or both hosts |

The counts are under `Gossip:` in `objectfs cluster status`, printed only when non-zero — so an empty
anomaly section means none of the three has happened, which is a stronger statement than a block of
zeros.

**4. Is UDP open between the nodes, both ways?**

The port is **8080** by default, not 7946 — the same number the metrics endpoint uses, which looks
like a misconfiguration and is not. Metrics is TCP and gossip is UDP, and the two are separate port
namespaces: `127.0.0.1:8080/tcp` and `0.0.0.0:8080/udp` both bind at the same time, verified rather
than assumed. What this does mean is that a firewall rule written for "port 8080" without a protocol
may open only the one you were not thinking about.

```bash
# From node B, aimed at node A's advertise_addr:
nc -zuv 10.0.1.1 8080
```

`nc -u` proves a datagram left, not that anything received it — UDP has nothing to report. The
authoritative check is step 2 on the far node.

**5. Does `advertise_addr` name an address peers can actually reach?**

Wildcards are refused and loopback is warned about, but a routable-looking address on the wrong
interface is neither — and it fails identically. Verify from another node:

```bash
ping -c1 10.0.1.1
```

## S3 costs are higher than expected

A mount publishes what it is spending, so start with the number:

```bash
curl -s http://127.0.0.1:8080/metrics | grep objectfs_s3_cost
```

Then, in the order that usually pays:

1. **The cache hit rate**, above. Every miss is a GET, and a GET is both a request charge and egress
   if it leaves the region. This is nearly always the whole answer.
2. **Request count versus bytes.** `objectfs_operations_total` against
   `objectfs_operation_size_bytes_sum` tells you whether you are paying for many small requests or
   for volume. Many small ones point at `performance.parallel_read` thresholds or at a workload of
   small files; volume points at the cache.
3. **Storage class.** `STANDARD` is the default for a reason: IA and the Glacier classes bill a 128 KB
   minimum per object and a 30-day minimum duration, so a mount that writes many small or short-lived
   files costs **more** on them, not less.
4. **Transfer Acceleration**, if enabled. It carries a per-GB surcharge, and it is a performance
   capability that falls back silently — so it can be off without anything failing:

   ```bash
   curl -s http://127.0.0.1:8080/metrics | grep objectfs_s3_acceleration
   ```

5. **Errors, which are also billed.** A retried request is a charged request:

   ```bash
   curl -s http://127.0.0.1:8080/metrics | grep objectfs_errors_total
   ```

`objectfs_s3_cost` is computed from a rate table verified against the live AWS Pricing API. It is an
estimate of what this mount did, and it does not know your negotiated rates, your free tier, or
anything happening in the bucket that did not come through this mount.

## When none of this helps

Include, in an issue:

- `objectfs version`
- `objectfs mount --config <your config> --dry-run` output, **stdout and stderr** — it prints no
  secrets, only their source
- `objectfs cluster status --json` from every node, if clustering is on
- the exact error message and its exit code
- `uname -a`, and whether the mount is containerized

## See also

- [Distributed mode](distributed.md) — the cluster configuration reference and what clustering does
  not do
- [Operations](operations.md) — rolling upgrades, growing and shrinking, and what to check when
- [Error handling and recovery](../error-handling-recovery.md) — how ObjectFS classifies and retries
  failures
