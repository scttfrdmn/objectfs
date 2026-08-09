# Distributed mode

Running more than one ObjectFS node over the same bucket, so that each one's cache knows what the
others hold.

This page is written against the code, not against a design. Where a mechanism named in an earlier
plan does not exist, this says so rather than describing it — see
[Limitations](#limitations-and-what-is-not-here) at the end, which is the section to read first if you
are deciding whether to deploy this.

## What clustering does, and what it does not

`cluster.enabled: true` starts one thing: a **gossip mesh** over UDP, used for cache coordination.
Nodes announce which keys they hold, and invalidate a key when they overwrite or delete it.

That is the whole of it, and the value is specific. A three-node cluster does not read faster than
three unclustered nodes on a warm cache — the bytes still come from S3, and each node still holds its
own copy. What it buys is **coherence**: without it, node B keeps serving a cached object for up to
`cache.ttl` after node A has overwritten it, and nothing tells anyone. With it, A's write invalidates
B's copy.

Three things a reader might reasonably expect are deliberately absent:

- **No leader, no elections, no replicated log.** Coordination is compare-and-swap against S3: a
  conditional write, evaluated by the store on a single request, needing no quorum. A Raft engine
  exists in `internal/distributed` and is *not started* by a mount — there is no configuration key
  that starts it. So `objectfs cluster status` prints `Role: n/a — leader election is not running`
  on every node of a perfectly healthy cluster, rather than telling you that you are a follower.
- **No data path between nodes.** A cache miss on node B does not fetch bytes from node A. It asks
  peers *which* keys they hold — metadata only — and then reads the bytes from S3. The reason is a
  measured one: the gossip transport is UDP with an 8192-byte sealed-datagram ceiling, which is about
  5.8 KB of payload, so a 128 KiB read cannot cross it. Knowing which keys are hot in the cluster is
  the part that is worth having; moving bytes over UDP is not.
- **No consistency level to choose.** `cluster.consistency_level` took `eventual`, `session` or
  `strong` and was removed in v0.12.0. All three issued the same unconditional PUT and differed only in how many
  nodes issued it, so the setting changed a request count and not a guarantee. A config file that
  still sets it now fails at load, naming the key. What replaced it is per-write rather than
  per-mount: a conditional write, with the precondition evaluated by S3. See
  [Coordination — Conditional Writes vs Raft](../design/conditional-writes-vs-raft.md).

## The failure mode this page exists for

A misconfigured cluster does not fail. It comes up as **a cluster of one**, per node.

The node mounts successfully, serves reads, answers its health endpoint, and reports itself alive. Its
cache announcements go to nobody and it receives no invalidation from any peer. So it will serve an
object that another node has already overwritten, for as long as its own `cache.ttl` allows — which is
a data-integrity outcome reached without a single error message anywhere.

Gossip is one-way UDP, which is why nothing reports it: there is no handshake to fail and no
acknowledgement to time out. Two settings are the usual cause, and neither can be refused outright
because both are correct in some deployments:

| Setting | Legitimate use | What it does otherwise |
| --- | --- | --- |
| `cluster.seed_nodes: []` | The first node of a new cluster | Every subsequent node forms its own cluster of one |
| `cluster.advertise_addr` on loopback | A single-host development cluster | Peers on other hosts dial their own loopback instead of this node |

Both are reported as warnings by `objectfs mount --dry-run`, which is the check to run before
deploying:

```console
$ objectfs mount --config /etc/objectfs/config.yaml --dry-run
Configuration is valid.
  storage URI:     s3://objectfs-example
  mount point:     /mnt/objectfs
  cache size:      2GB
  max concurrency: 150

Cluster config:
  enabled:            true
  node_id:            node-1
  listen_addr:        0.0.0.0:8080
  advertise_addr:     10.0.1.1:8080
  seed_nodes:         10.0.1.2:8080, 10.0.1.3:8080
  replication_factor: 3
  cluster secret:     from /etc/objectfs/cluster.secret
```

Warnings go to stderr and the exit code stays 0 — a tool that treated them as failures could not bring
up the first node of any cluster.

One related misconfiguration *is* refused at startup rather than warned about, because there is no
deployment in which it is right: an `advertise_addr` that is a wildcard (`0.0.0.0:8080`, `:8080`,
`[::]:8080`). A peer that dials a wildcard reaches its own loopback — measured, not inferred — so every
peer would silently talk to itself in place of this node. `listen_addr` is where `0.0.0.0` belongs.

## Configuration reference

Every key under `cluster:`. Defaults are `internal/config`'s, which is what a mount uses when the key
is absent.

| Key | Type | Default | Environment | Notes |
| --- | --- | --- | --- | --- |
| `enabled` | bool | `false` | `OBJECTFS_CLUSTER_ENABLED` | Starts the gossip mesh. Also selects the Redis cache when `redis.enabled` is set |
| `node_id` | string | `""` | `OBJECTFS_CLUSTER_NODE_ID` | Empty generates `node-<8 hex bytes>` at startup — **new on every restart**. Set it for anything you intend to identify in a report |
| `listen_addr` | host:port | `0.0.0.0:8080` | `OBJECTFS_CLUSTER_LISTEN_ADDR` | Where the gossip UDP socket binds. A wildcard belongs here. Port `0` is allowed and lets the kernel choose |
| `advertise_addr` | host:port | `127.0.0.1:8080` | `OBJECTFS_CLUSTER_ADVERTISE_ADDR` | Where peers are told to reach this node. Must not be a wildcard and must not be port `0`; both are refused at startup |
| `seed_nodes` | list of host:port | `[]` | `OBJECTFS_CLUSTER_SEEDS` | Any existing member. A node that seeds from itself is a no-op, so one well-known seed can be listed on every node. The environment form is comma-separated |
| `secret_file` | path | `""` | — (see below) | Path to the shared gossip secret. **The path, never the secret.** Must be mode 0600 or startup refuses it |
| `replication_factor` | int | `3` | — | How many nodes an operation *selects*, in preference order. Only the first writes — there is one copy of the bytes and S3 holds it — so this is a selection width, not a number of copies. `0` means the default; negative is refused |
| `redis.*` | — | disabled | — | A shared Redis cache, independent of gossip. See `examples/config.yaml` |

The environment column is not a convenience. One ConfigMap is shared by every replica of a
StatefulSet, and `node_id` and `advertise_addr` must differ per pod — so those two come from the
downward API rather than from the file, which is exactly what
`deploy/kubernetes/statefulset.yaml` does.

The gossip timers, fanout, packet ceiling, announcement TTL and retry settings have no YAML keys. They
are measured defaults in `internal/distributed` (for instance the 8192-byte packet ceiling and the
5-minute announcement TTL), and exposing them would be eight more keys that mostly should not be
touched.

### The cluster secret

A cluster **will not start** without a shared secret, and this is not a hardening option. The gossip
port is UDP and unauthenticated gossip means any host that can reach it may join the cluster and
announce that it holds the current copy of a cached object.

Two sources, environment first:

```bash
# A file, for a host install. The mode is checked, not fixed.
openssl rand -hex 32 > /etc/objectfs/cluster.secret
chmod 600 /etc/objectfs/cluster.secret
```

```bash
# Or the environment, which is what a container orchestrator injects.
export OBJECTFS_CLUSTER_SECRET="$(openssl rand -hex 32)"
```

`OBJECTFS_CLUSTER_SECRET` takes precedence over `secret_file` and has **no config-file key at all**,
deliberately: `/etc/objectfs/config.yaml` is installed world-readable, so a secret written there would
be published to every user on the node. Minimum length is 32 bytes.

Every node must hold the *same* secret. A node with a different one is not an error at either end —
its datagrams are rejected and counted, and the mismatch appears as a `rejected gossip message` warning
naming the peer, with the hint that a differing secret is the likely cause.

## Sizing the cache

The formula that matters is per node, and it is about the *hot* set rather than the dataset:

```
cache_size ≥ hot_dataset_size / node_count
```

Note what is **not** in it: `replication_factor`. An earlier version of this guidance multiplied by it,
which would be right if the cluster made copies — and it does not. Every node writes the same key in
the same bucket and S3 holds the single copy, so the replication factor selects how many nodes an
operation is offered to, not how many times the bytes exist.

Worked example. A genomics group has 40 TB in a bucket, of which about 1.2 TB is read repeatedly during
an analysis run, spread across three nodes:

```
hot set        1.2 TB
node_count     3
cache_size     ≥ 400 GB per node
```

That is a floor rather than a target, and it assumes reads distribute evenly across nodes, which is
true when a scheduler spreads a job and false when one node is doing all the work. Two adjustments:

- If nodes read **overlapping** data — the same reference genome on every node — each node needs its own
  copy of the overlap, because there is no shared cache between them. Size for the overlap once per
  node, not once per cluster.
- If the working set does not fit, `cache.ttl` matters more than `cache_size`: entries will be evicted
  before they expire, and the cluster's invalidation traffic is doing less for you.

`cache.persistent_cache` spills to local disk and is the cheaper way to raise the first number.

## Growing and shrinking a cluster

There is no membership list to edit and no quorum to preserve — which is a direct consequence of
there being no consensus. A node joins by gossiping with a seed, and leaves by stopping.

**1 → 3.** Bring up the new nodes with `seed_nodes` naming the existing one:

```yaml
cluster:
  enabled: true
  node_id: node-2
  advertise_addr: 10.0.1.2:8080
  seed_nodes:
    - 10.0.1.1:8080
```

Then verify from each node, rather than assuming:

```console
$ objectfs cluster status
```

The membership counts include the local node, so a healthy three-node cluster reports
`Membership: 3 nodes (3 alive, 0 suspect, 0 dead)` from every node. A node that reports `1 node` and
`No peers.` has not joined — check the warnings in `--dry-run`, then the secret.

Note that `No peers.` is followed by a line saying a cluster of one is a working configuration, and
that is true of coordination: CAS against S3 needs no second node. It is not true of the cache
coherence you enabled clustering for. Both statements are in the output because both are worth
knowing, and which one applies is a question about your intent that the command cannot answer.

**3 → 1.** Unmount the nodes you are removing, one at a time, and confirm the remaining nodes notice
before removing the next. There is nothing to drain: a departing node holds no data that only it has,
because every object is in S3. What is lost is its cache, and the effect on the rest of the cluster is
that some keys are no longer announced by anybody — so subsequent reads for them come from S3, which
is the pre-cluster behaviour.

**Rolling upgrade.** One node at a time; check `objectfs cluster status` shows all nodes alive before
moving to the next. There is no leader to upgrade last. Two things to be aware of:

- Unmount cleanly. A FUSE mount is unmounted by its own process on a signal, and a `SIGKILL` leaves the
  kernel holding a mount whose server is gone. In Kubernetes this means a
  `terminationGracePeriodSeconds` well above the default 30 — `deploy/kubernetes/statefulset.yaml`
  uses 120.
- A mixed-version cluster is only as coordinated as its oldest node. A node running a build that
  predates gossip authentication has its messages rejected with a version-mismatch hint rather than a
  secret one, which is what that hint is for.

## Diagnosing a cluster of one

In order, because each step rules out the one after it:

1. **`objectfs mount --dry-run`**, on the node's real config file. This catches an empty seed list, a
   loopback advertise address, and a missing secret — the three most common causes — before anything
   is running.
2. **`objectfs cluster status`** on each node. `No peers.` means this node has not joined. The exit
   code is 0 for a healthy cluster *and* for clustering being disabled, which is not a fault; use the
   output rather than the code to tell those apart.
3. **Is the secret the same everywhere?** A mismatch is logged as `rejected gossip message` at Warn,
   with the peer address and a hint, on the *receiving* node. If you collect logs from only one node
   you will not see it. The three hints send you to different places, so read the one you got:

   | Hint mentions | Cause |
   | --- | --- |
   | a different cluster secret, or a host that is not a member | secret mismatch, or an unrelated host on the port |
   | a build that predates gossip authentication | a mixed-version cluster mid-upgrade |
   | clock skew beyond the freshness window | NTP, on one or both hosts |

   The counts behind them are in `objectfs cluster status` under `Gossip:`, printed only when
   non-zero — so an empty anomaly section means none of the three has happened.
4. **Is UDP open between nodes on the gossip port?** The default is `8080`, not 7946 — that number
   appears in some older planning documents and in tests, and is not the default anywhere in the code.
   Both directions matter: gossip is one-way UDP, so a firewall that permits A→B and drops B→A gives
   you a cluster where each node believes something different.
5. **Does `advertise_addr` name an address peers can actually reach?** Wildcards are refused at
   startup and loopback is warned about, but a routable-looking address for the wrong interface is
   neither — and it fails identically.

## Limitations, and what is not here

Stated plainly, because the rest of this page describes what works:

- **`internal/distributed` is experimental.** The gossip and cache-coordination path is what a mount
  starts and what has tests; the consensus engine in the same package is not started by a mount and
  should not be treated as a supported feature.
- **No persistent log, and no `cluster.persistent_log` or `cluster.data_dir` keys.** They were
  specified in an earlier plan and do not exist. Cluster state is in memory and is rebuilt by
  gossiping on restart — which is fine, because none of it is authoritative: S3 is.
- **No peer data fetch.** Discussed above; the limit is the UDP transport, and a stream transport is
  its own piece of work.
- **`node_id` defaults to a fresh random value on every restart.** A node that restarts appears as a
  new member, and the old entry ages out. Set `node_id` in any deployment where you intend to
  correlate a report with a host.
- **Cross-node cache statistics are what each node reports about itself.** There is no per-key access
  counter reachable from the cluster layer, so `objectfs cluster status` reports how many keys this
  node retains peer claims for, and not "hottest keys" — a figure nothing in this repository measures.
- **Distributed mode has no integration test against real AWS across multiple hosts.** The tests are
  two nodes over loopback plus the differential oracle. `deploy/docker/docker-compose.yaml` stands up
  three nodes against MinIO for development.

## See also

- [`deploy/`](https://github.com/scttfrdmn/objectfs/tree/main/deploy) — Compose and Kubernetes
  manifests for a three-node cluster, including the per-pod identity a StatefulSet needs
- [Coordination — Conditional Writes vs Raft](../design/conditional-writes-vs-raft.md) — why there is
  no leader
- [Conditional-Write Compatibility](../design/conditional-write-compatibility.md) — which backends
  evaluate preconditions, and what happens on one that does not
- `examples/config.yaml` — the complete cluster block with every default
