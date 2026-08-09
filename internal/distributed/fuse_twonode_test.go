//go:build linux || darwin

package distributed

// #141's acceptance criteria name two nodes: node A writes key X through the filesystem, and node B's
// cache entry for X is gone within a gossip round trip. Everything else about #141 is asserted in
// internal/fuse, against a recording coordinator, because that is where "which call, with which fields"
// belongs. What cannot be asserted there is the part that matters most to a user: that the call actually
// crosses the wire and evicts something.
//
// # Why it lives in this package
//
// It drives internal/fuse, internal/cache, internal/vfs and internal/testaws, and none of them import
// internal/distributed — verified rather than assumed, by compiling a probe that imports all four from
// here. The reverse would not work: internal/fuse cannot reach startGossipPair or a *ClusterManager's
// unexported gossip field, and exporting a join helper so a test in another package could reach it would
// widen the production API for a test's convenience.
//
// # What "real" means here
//
// The two nodes share one S3 endpoint and have separate caches, which is the deployment shape: two hosts
// mounting the same bucket. Both filesystems are real, the write goes through the real write path, the
// invalidation crosses a real UDP socket, and the assertion is on node B's cache no longer holding the
// bytes. Nothing is mocked; the only thing standing in for a second host is a second port on loopback.

import (
	"syscall"
	"testing"
	"time"

	gofuse "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scttfrdmn/objectfs/internal/cache"
	objectfuse "github.com/scttfrdmn/objectfs/internal/fuse"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/internal/vfs"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// clusterNode is one host: a filesystem, its own cache, and the cluster manager that connects it.
type clusterNode struct {
	fs    *objectfuse.FileSystem
	root  *objectfuse.DirectoryNode
	cache *cache.LRUCache
	cm    *ClusterManager
}

// write creates key, writes data to it, and flushes — through the same three node operations the kernel
// performs for `echo > file`, rather than through the write path directly.
//
// The entry points, because what #141 wired is the FUSE layer: a test that called vfs.Writer.Flush would
// bypass [objectfuse.FileHandle.Flush], which is where the invalidation lives, and would pass with the
// whole of #141 reverted.
func (n *clusterNode) write(t *testing.T, key, data string) {
	t.Helper()

	var out fuse.EntryOut
	_, handle, _, errno := n.root.Create(t.Context(), key, uint32(syscall.O_WRONLY), 0o644, &out)
	if errno != 0 {
		t.Fatalf("Create(%q): %v", key, errno)
	}

	fh, ok := handle.(*objectfuse.FileHandle)
	if !ok {
		t.Fatalf("Create returned a %T, want *fuse.FileHandle", handle)
	}

	if _, errno := fh.Write(t.Context(), []byte(data), 0); errno != 0 {
		t.Fatalf("Write(%q): %v", key, errno)
	}

	if errno := fh.Flush(t.Context()); errno != 0 {
		t.Fatalf("Flush(%q): %v", key, errno)
	}
}

// newClusterNode builds a filesystem on cm, sharing srv's bucket.
//
// The cache is handed to both the filesystem and the cluster manager, and it has to be the same one:
// [GossipProtocol.handleCacheInvalidate] evicts from cm's cache, and the read path serves from the
// filesystem's. Two caches here would make an invalidation that arrived and evicted nothing look exactly
// like one that worked.
func newClusterNode(t *testing.T, srv *testaws.TestServer, cm *ClusterManager) *clusterNode {
	t.Helper()

	backend := srv.Backend()

	writer, err := vfs.NewWriter(t.Context(), backend)
	if err != nil {
		t.Fatalf("vfs.NewWriter: %v", err)
	}

	// A TTL long enough that nothing expires on its own. The property under test is that an invalidation
	// evicts, not that staleness eventually times out — and a short TTL here would pass this test with the
	// gossip message deleted entirely.
	byteCache := cache.NewLRUCache(&cache.CacheConfig{
		MaxSize:    16 << 20,
		MaxEntries: 10000,
		TTL:        time.Hour,
	})
	t.Cleanup(func() { _ = byteCache.Close() })

	cm.SetBackend(backend)
	cm.SetCache(byteCache)

	fs := objectfuse.NewFileSystem(t.Context(), backend, byteCache, writer, nil, &objectfuse.Config{
		DefaultMode:    0o644,
		DefaultDirMode: 0o755,
		DefaultUID:     1000,
		DefaultGID:     1000,
		Coordinator:    cm.GetCoordinator(),
	})

	root, ok := fs.Root().(*objectfuse.DirectoryNode)
	if !ok {
		t.Fatalf("FileSystem.Root returned %T, want *fuse.DirectoryNode", fs.Root())
	}

	// Attached to a bridge, because Create builds a child inode through Inode.NewInode and dereferences
	// the bridge to do it. NullPermissions matches what internal/fuse's own mount options set.
	timeout := time.Minute
	_ = gofuse.NewNodeFS(root, &gofuse.Options{
		AttrTimeout:     &timeout,
		EntryTimeout:    &timeout,
		NullPermissions: true,
	})

	return &clusterNode{fs: fs, root: root, cache: byteCache, cm: cm}
}

// TestTwoNodes_AWriteEvictsThePeersCachedBytes is #141's end-to-end criterion.
//
// The failure it exists to catch is the one a single-node test cannot see and a user cannot easily
// diagnose: node B keeps serving the file's previous contents after node A rewrites it. Reading through
// B's own read path afterwards would not distinguish an eviction from a re-fetch, so the assertion is on
// the cache directly — the bytes are no longer there, which is the whole of what an invalidation can
// promise.
func TestTwoNodes_AWriteEvictsThePeersCachedBytes(t *testing.T) {
	t.Parallel()

	srv := testaws.Start(t)

	cm1, cm2 := startGossipPair(t, "fuse-write-node-a", "fuse-write-node-b")
	nodeA := newClusterNode(t, srv, cm1)
	nodeB := newClusterNode(t, srv, cm2)

	const (
		key      = "shared/dataset.bin"
		size     = 8192
		replaced = "node A replaced every byte of this object"
	)

	original := srv.SeedRandom(key, size)

	// Node B caches the object's first chunk, the way a read would. Through the cache's own Put rather
	// than through B's read path, because what is under test is the eviction: a read here would also
	// populate B's metadata cache and register a read-ahead pattern, and a prefetch landing after the
	// invalidation would refill the entry and fail this test for a reason that is not a defect.
	nodeB.cache.Put(key, 0, original[:4096])

	if got := nodeB.cache.Get(key, 0, 4096); len(got) != 4096 {
		t.Fatalf("node B's cache does not hold the bytes this test is about to have evicted: got %d", len(got))
	}

	// Node A rewrites the object and flushes, which is what broadcasts.
	nodeA.write(t, key, replaced)

	waitForEviction(t, nodeB.cache, key, 0, 4096)
}

// TestTwoNodes_ADeleteEvictsThePeersCachedBytes is the same criterion for the operation with no version
// to name.
//
// Worth its own test rather than a subtest of the above: a delete's invalidation carries an empty ETag,
// which takes a different branch in [GossipProtocol.handleCacheInvalidate] — an unversioned invalidation
// bypasses the applied-once ledger and is applied every time. A test that only covered the versioned path
// would leave the branch a delete actually takes uncovered end to end.
func TestTwoNodes_ADeleteEvictsThePeersCachedBytes(t *testing.T) {
	t.Parallel()

	srv := testaws.Start(t)

	cm1, cm2 := startGossipPair(t, "fuse-delete-node-a", "fuse-delete-node-b")
	nodeA := newClusterNode(t, srv, cm1)
	nodeB := newClusterNode(t, srv, cm2)

	const (
		key  = "shared/doomed.bin"
		size = 8192
	)

	original := srv.SeedRandom(key, size)
	nodeB.cache.Put(key, 0, original[:4096])

	if got := nodeB.cache.Get(key, 0, 4096); len(got) != 4096 {
		t.Fatalf("node B's cache does not hold the bytes this test is about to have evicted: got %d", len(got))
	}

	if errno := nodeA.root.Unlink(t.Context(), key); errno != 0 {
		t.Fatalf("Unlink on node A: %v", errno)
	}

	waitForEviction(t, nodeB.cache, key, 0, 4096)
}

// waitForEviction polls c until the range is gone, or fails.
//
// Polling for the same reason [waitForDeletion] does: this crosses loopback UDP and a receive goroutine,
// so a read taken immediately after the flush returns has no reason to see anything yet. The deadline is
// generous because the failure this catches — a message never sent, or sent for the wrong key — is not one
// more waiting fixes.
func waitForEviction(t *testing.T, c types.Cache, key string, off, length int64) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if c.Get(key, off, length) == nil {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("node B still holds [%d, %d) of %q two seconds after the peer that owns it wrote it. Every "+
		"process reading that file on this host is being served the previous contents, and will be until "+
		"the cache entry expires on its own", off, off+length, key)
}
