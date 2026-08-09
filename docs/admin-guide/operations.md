# Operations

Day-two procedures: upgrades, capacity changes, and the checks that tell you a change worked.

This page assumes a working deployment. For what clustering does, every `cluster:` key, and the
failure mode a misconfigured cluster has instead of an error, see
[Distributed mode](distributed.md) — the mechanics of growing and shrinking a cluster live there, and
this page covers the operational wrapper around them rather than repeating it.

## Before any change: know what you are running

```bash
objectfs version
objectfs mount --config /etc/objectfs/config.yaml --dry-run
objectfs cluster status            # if clustering is enabled
```

`--dry-run` needs no credentials, no network and no FUSE, so it is safe to run on a production host
mid-incident. It loads the file, applies the environment overrides, validates, prints what it
resolved, and exits.

Two things worth knowing about that output before you rely on it. It reports the cluster secret **by
source and never by value**, so it is safe to paste into a ticket. And its **warnings go to stderr
with exit code 0** — a config-management tool that treats a nonzero exit as the only signal will not
see them.

## Upgrading

### Single node

```bash
systemctl stop objectfs@research-data     # unmounts cleanly
# install the new package
systemctl start objectfs@research-data
```

The instance name is the config name: `objectfs@research-data` reads
`/etc/objectfs/research-data.yaml` and mounts `/mnt/objectfs/research-data`.

**Stop, do not kill.** A FUSE mount is unmounted by its own process on a signal; a `SIGKILL` leaves
the kernel holding a mount whose server is gone, and loses any dirty range that had not reached S3.
The unit's `ExecStop` runs `objectfs unmount` for exactly this reason, and `TimeoutStopSec=90` gives
it room to finish — a flush of a large dirty range is not instant.

The message that matters on the way down is:

```text
objectfs: shutdown failed, so data may not have reached S3: ...
```

That is an exit 1, and it is the one message this program exists to be able to print. If you see it,
**do not proceed to the next node** — find out what did not flush first.

Verify the mount came back before moving on:

```bash
mount | grep objectfs
ls /mnt/objectfs >/dev/null && echo "readable"
curl -sf http://127.0.0.1:8081/health && echo
```

### Rolling upgrade of a cluster

One node at a time. **There is no leader to upgrade last** — nothing elects one, so
`objectfs cluster status` prints `Role: n/a` on every node and the order is yours to choose.

For each node:

1. Stop it as above, and confirm the unmount was clean.
2. Install and start.
3. Confirm from a **different** node that the upgraded one is back:

   ```bash
   objectfs cluster status | grep -A5 Membership
   ```

   Wait for `0 suspect, 0 dead` before touching the next node. A node that has just restarted is
   suspect for a few gossip rounds, which is normal and transient; a node that stays suspect is not.

4. Then the next node.

Two cluster-specific cautions:

- **A mixed-version cluster is only as coordinated as its oldest node.** A node running a build that
  predates gossip authentication has its datagrams rejected, and the receiving node logs
  `rejected gossip message` with a hint naming a version mismatch rather than a secret problem. That
  hint exists so a rolling upgrade does not send you to check a secret that is correct.
- **`node_id` defaults to a fresh random value on every restart.** An upgraded node with no `node_id`
  set appears as a *new* member while the old entry ages out, so membership can read `4 nodes` in a
  three-node cluster for a while. Set `node_id` and this stops being confusing.

### Kubernetes

`deploy/kubernetes/statefulset.yaml` handles the ordering: a StatefulSet's default
`RollingUpdate` strategy replaces one pod at a time, and `terminationGracePeriodSeconds: 120` gives
the unmount time to finish — well above the default 30, for the reason above.

```bash
kubectl rollout status statefulset/objectfs
kubectl exec objectfs-0 -- objectfs cluster status
```

## Changing capacity

### Adding and removing nodes

See [Growing and shrinking a cluster](distributed.md#growing-and-shrinking-a-cluster) for the
configuration. Operationally, the parts to hold onto:

- **There is nothing to drain.** A departing node holds no data that only it has — every object is in
  S3. What is lost is its cache, so some keys stop being announced by anybody and the next read for
  them comes from S3. That is the pre-cluster behaviour, not a fault.
- **There is no quorum to preserve**, because there is no consensus. Removing two of three nodes is a
  capacity decision, not an availability one; the remaining node keeps working. `objectfs cluster
  status` exits nonzero for a **dead or suspect** node, and deliberately has no quorum condition.
- **Remove one at a time anyway**, and confirm the remaining nodes noticed. Not for correctness — for
  the ability to tell a planned removal from a node that failed during the change.

### Resizing a cache

`performance.cache_size` and `cache.persistent_cache.max_size` are read at startup, so a change needs
a restart of that node — which is a single-node upgrade, above. Size it against the *hot* set divided
by the node count, and see [sizing](distributed.md#sizing-the-cache) for why
`replication_factor` is not in that formula.

Check whether the change did anything, rather than assuming:

```bash
curl -s http://127.0.0.1:8080/metrics | grep -E 'objectfs_cache_(requests_total|size_bytes)'
```

The hit rate is `hit / (hit + miss)` from the two `objectfs_cache_requests_total` series. If it did
not move, the workload may have no reuse to capture — see
[low cache hit rate](troubleshooting.md#cache-hit-rate-is-low), which covers that case before it
covers the ones a bigger cache fixes.

## Rotating the cluster secret

The secret authenticates gossip, and a node with a different one has its datagrams rejected. There is
no in-place rotation that keeps the mesh formed: two secrets in one cluster is a partition by design.

The honest procedure is a brief coordinated restart:

1. Distribute the new secret to every node, without activating it (write the file, or stage the
   environment value).
2. Stop **all** nodes. This is the part that is not rolling.
3. Activate the new secret everywhere and start the nodes.
4. Confirm full membership from each node.

A rolling rotation would leave the two halves unable to exchange invalidations while both report
healthy — the silent-partition failure, self-inflicted. Reads and writes keep working throughout the
restart window; what stops is cache coherence, so a maintenance window is the right framing.

## What to watch

Neither endpoint has any authentication, which is why both default to loopback. In a pod they must
bind `0.0.0.0` (`OBJECTFS_METRICS_ADDR`, `OBJECTFS_HEALTH_ADDR`) because the kubelet probes over the
pod IP.

```bash
curl -sf http://127.0.0.1:8081/health          # liveness
curl -s  http://127.0.0.1:8080/metrics         # Prometheus
```

The handful worth an alert:

| Signal | Why |
| --- | --- |
| `objectfs_errors_total` rising | retried requests are billed requests, and a rising rate is the earliest sign of a credential or throttling problem |
| `objectfs_cache_requests_total` hit ratio falling | the working set outgrew the cache, or a node dropped out of the mesh |
| `objectfs_s3_cost` slope | the direct measure of what a change cost you |
| `objectfs cluster status` exit 1 | a node is dead or suspect, or the instance is unreachable |
| the shutdown-failed message in the journal | data may not have reached S3 |

`objectfs cluster status` is the one designed to be used from a script: exit **0** when no node is
dead or suspect *and also* when clustering is disabled (the default, not a fault), **1** when
unreachable or a node is dead or suspect, **2** for a bad command line. `--json` gives the same data
for `jq`.

## What this page does not cover

Two procedures an earlier plan for this page specified cannot be written, and are listed here rather
than approximated:

- **Leader-failure recovery and split-brain intervention.** Nothing elects a leader on a mount path.
  Coordination is compare-and-swap against S3, which needs no leader and no quorum, so there is no
  election to wait for and no split-brain state to resolve. See
  [Coordination — Conditional Writes vs Raft](../design/conditional-writes-vs-raft.md).
- **Log rotation and data-directory management.** There is no persistent log and no
  `cluster.persistent_log` or `cluster.data_dir` key. Cluster state is in memory and is rebuilt by
  gossiping on restart, which is safe because none of it is authoritative — S3 is.

## See also

- [Distributed mode](distributed.md) — the cluster configuration reference, sizing, and limitations
- [Troubleshooting](troubleshooting.md) — the decision tree for when a check above comes back wrong
- [Error handling and recovery](../error-handling-recovery.md) — how failures are classified and
  retried
