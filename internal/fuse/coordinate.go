//go:build linux || darwin

package fuse

// This file is the whole of what the read and write paths say to the rest of the cluster (#141): an
// announcement when bytes land in this node's cache, and an invalidation when a write or a delete makes
// what a peer holds wrong.
//
// # Both directions are fire-and-forget, and only one of them is an optimization
//
// An announcement that is not sent costs a peer an S3 read it could have avoided. That is slower and
// nothing more, so a failure is logged at Debug and the read returns normally.
//
// An invalidation that is not sent is not symmetric with that, and it is worth being explicit because
// the two calls look alike at every call site. A peer that never hears an invalidation keeps serving
// the bytes this node just replaced, for as long as its cache TTL — up to five minutes on the default
// config. Gossip has no acknowledgement to offer, so there is no version of this call that could tell
// the caller its peers were reached; failing the write(2) that prompted it would be strictly worse,
// since the object *is* durable and the caller's data *is* stored. What makes the gap bounded rather
// than unbounded is the TTL, and what makes it detectable is the log line. It is recorded at Warn
// rather than Debug for that reason: a mount whose invalidations are all failing is serving stale reads
// cluster-wide, and that is an operator's problem, not a debugging detail.
//
// # Neither call blocks the syscall, and neither is on a goroutine
//
// Inline, on the FUSE request's own goroutine. A `go` per invalidation would decouple the send from the
// write, which sounds like the safer shape and is not: the ordering between an invalidation and the
// next read of the same key is the entire mechanism, and a goroutine scheduled after a peer has already
// re-read the key evicts bytes that are once again correct while leaving the stale window open. The
// send itself is a marshal plus a UDP write per peer — [distributed.GossipProtocol.broadcastMessage]
// already fans out to a goroutine per target and does not wait — so inline costs microseconds and buys
// the ordering.

import (
	"context"
	"log/slog"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// announceCached tells peers this node now holds [off, off+len(data)) of path in its cache.
//
// # Why this reads a version it did not fetch
//
// [types.KeyAnnouncement] requires an ETag and [types.DistributedCoordinator.AnnounceKey] refuses an
// announcement without one, because a peer that fetches bytes it cannot place against a version of the
// object hands them to a reading process as file content. #141's specification does not carry the field
// — its sketch fills in Key, NodeID, Size, CachedAt, Offset and Length and stops — so implemented as
// written, every announcement this made would be refused by the layer below it.
//
// The bytes come from [types.Backend.GetObject], which returns no version: the S3 backend has the ETag
// in the GET response and discards it at that boundary, and the whole read path below this point is
// []byte. So the version comes from the metadata cache instead, which holds the [types.ObjectInfo] from
// the HEAD that the kernel's stat issued before the read — the stat always precedes the read, because
// the kernel needs a size before it will issue one.
//
// That makes the announcement's ETag *the version this node last stat'ed*, not provably the version the
// bytes came from. The gap is real: an overwrite between the stat and the GET makes the two disagree,
// and the ETag would name the older object. What bounds the damage is what a recipient does with it —
// [types.DistributedCoordinator.QueryKeyOwnership] documents every announcement as a claim to be
// checked against the ETag the fetcher wanted, precisely because a holder can have evicted, replaced,
// or never held what it announced. A wrong ETag here therefore costs a peer a rejected fetch and an S3
// read, which is the outcome it would have had with no announcement at all.
//
// What is not acceptable is inventing one. With no cached metadata this returns without announcing,
// which is #140's fail-closed direction applied one layer up: no announcement is a peer reading S3, and
// a fabricated version is a peer serving bytes under a name that does not describe them.
func (fs *FileSystem) announceCached(ctx context.Context, path string, off int64, data []byte) {
	if fs.coordinator == nil || len(data) == 0 {
		return
	}

	// The metadata cache, not a HEAD. Issuing a request here would put one S3 round trip on every cache
	// miss to enable an optimization whose entire purpose is removing S3 round trips, and it would pay
	// it on single-key workloads that no peer will ever ask about.
	info := fs.getCachedInfo(path)
	if info == nil || info.ETag == "" {
		slog.Debug("not announcing cached bytes: no version is known for the object",
			"path", path, "offset", off, "length", len(data))

		return
	}

	// Size is the object's, Length is the range's. [types.KeyAnnouncement] keeps them apart on purpose: a
	// peer weighing a fetch needs the range to know what it can get here and the size to know what
	// fraction of the object that is. Collapsing them — announcing len(data) as both — would tell a peer
	// a 128 KiB range of a 10 GiB object is the whole file.
	if err := fs.coordinator.AnnounceKey(ctx, types.KeyAnnouncement{
		Key:      path,
		ETag:     info.ETag,
		Size:     info.Size,
		CachedAt: time.Now(),
		Offset:   off,
		Length:   int64(len(data)),
	}); err != nil {
		// Debug: a peer that never hears this reads from S3, which is correct and merely slower. NodeID is
		// deliberately unset above — AnnounceKey overwrites it with the cluster's own node ID, and this
		// package has no access to it.
		slog.Debug("could not announce cached bytes to peers",
			"path", path, "offset", off, "length", len(data), "error", err)
	}
}

// invalidateCluster tells peers to evict path, because the version named by etag replaced what they
// hold. An empty etag means this caller cannot name a version, which [types.DistributedCoordinator]
// documents as legal and applies on every receipt rather than once.
//
// # Two keys locally, one key on a peer
//
// [FileSystem.invalidate] deletes both path and metaCacheKey(path), because a write changes the bytes
// *and* the size and mtime that a stat reports; dropping only one leaves a file whose stat size does not
// match what read returns. A peer receiving this evicts one key —
// [distributed.GossipProtocol.handleCacheInvalidate] calls cache.Delete(m.Key) — so this sends both, and
// the metadata key is not an afterthought. Without it, a peer that has stat'ed the object serves its
// pre-write size from cache for the full metadata TTL while serving post-write content, which is the
// same disagreement locally, across a node boundary.
//
// The metadata key travels with no ETag even when the content key has one. The ETag names a version of
// the object's *bytes*, and the receiver's replay ledger is keyed on (key, etag): reusing the content
// version for the metadata key would be a second entry claiming to describe bytes that key does not
// hold. Unversioned means "evict whatever you hold", which is exactly right for an attribute record and
// costs a redundant eviction at worst.
func (fs *FileSystem) invalidateCluster(ctx context.Context, path, etag string) {
	if fs.coordinator == nil {
		return
	}

	if err := fs.coordinator.InvalidateKey(ctx, path, etag); err != nil {
		// Warn, unlike the announcement above. See the file comment: peers keep serving bytes this node
		// replaced until their cache TTL expires, and nothing else reports it.
		slog.Warn("could not tell peers to evict a key this node just wrote, so they may serve its "+
			"previous contents until their cache entries expire",
			"path", path, "etag", etag, "error", err)
	}

	if err := fs.coordinator.InvalidateKey(ctx, metaCacheKey(path), ""); err != nil {
		slog.Warn("could not tell peers to evict a key's cached attributes, so they may report its "+
			"previous size until their cache entries expire",
			"path", path, "error", err)
	}
}

// invalidateBoth drops path from this node's caches and from every peer's.
//
// It exists because the local and remote halves must not be able to drift apart. Every mutation in this
// package already calls [FileSystem.invalidate]; a cluster call sitting beside each of those as a
// separate statement is a pair that a future mutation path can add one half of — and the half easier to
// forget is the remote one, whose absence is invisible on a single-node mount and on every test that
// does not run two nodes.
//
// etag is the version the caller wrote, empty where it has none. A delete has no version to name, and
// [types.DistributedCoordinator.InvalidateKey] is explicit that empty is legal and that inventing one
// is not.
func (fs *FileSystem) invalidateBoth(ctx context.Context, path, etag string) {
	fs.invalidate(path)
	fs.invalidateCluster(ctx, path, etag)
}

// flushReportingETag makes path durable and reports the version storage now holds, empty when there is
// no version to name.
//
// A thin wrapper over [vfs.Writer.FlushReportingETag] whose only work is the nil-buffer guard, which
// every flush site in this package needs: a read-only mount has no write path, and [FileNode.Fsync]
// already returns zero for that case rather than an error. Kept here beside the coordination calls
// because the ETag exists for them — nothing else in this package reads it.
func (fs *FileSystem) flushReportingETag(ctx context.Context, path string) (string, error) {
	if fs.buffer == nil {
		return "", nil
	}

	return fs.buffer.FlushReportingETag(ctx, path)
}
