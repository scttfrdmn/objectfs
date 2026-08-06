//go:build linux || darwin

package fuse

// A mount on a tier with a minimum billable object size has to be able to create things.
//
// AWS publishes a 128 KiB minimum billable size for STANDARD_IA, ONEZONE_IA, and GLACIER_IR. It is a
// pricing floor — S3 stores a zero-byte object on those classes and bills it as 128 KiB — but
// `TierValidator.ValidateWrite` enforced it as though S3 would refuse the write. Both of the ways
// this filesystem brings a name into existence PUT zero bytes: `Mkdir` writes an empty marker object
// immediately, and `Create` records attributes that flush as an empty object. So on any of those
// three tiers, `mkdir` and `touch` both failed, and nothing on the mount could be created (#154).
//
// This lives in internal/fuse rather than beside the validator's own tests because the validator
// cannot see the sizes its callers pass. `internal/storage/s3` tests assert that a zero-byte write is
// accepted; only this layer establishes that zero bytes is what a mkdir actually sends, which is the
// step that turned a billing gate into an unusable filesystem.

import (
	"context"
	"testing"
	"time"

	gofuse "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	"github.com/scttfrdmn/objectfs/internal/vfs"
)

// TestMountOnATierWithABillingMinimumCanCreate runs mkdir and touch on every tier that has a
// minimum billable size, reading the list from StorageTiers so a tier that gains one later is
// covered without editing this test.
func TestMountOnATierWithABillingMinimumCanCreate(t *testing.T) {
	t.Parallel()

	var tiers []string

	for tier, info := range s3.StorageTiers {
		if info.MinObjectSize > 0 {
			tiers = append(tiers, tier)
		}
	}

	if len(tiers) == 0 {
		t.Fatal("no tier in s3.StorageTiers has a MinObjectSize, so this test asserts nothing")
	}

	for _, tier := range tiers {
		t.Run(tier, func(t *testing.T) {
			t.Parallel()

			srv := testaws.Start(t)
			backend := srv.Backend(func(cfg *s3.Config) {
				cfg.StorageTier = tier
				// Compression off: it changes the stored length, so leaving it on would mean a pass could
				// come from a body that happened to land above the minimum rather than from the gate.
				cfg.Compression.Enabled = false
			})

			ctx := context.Background()

			writer, err := vfs.NewWriter(ctx, backend)
			if err != nil {
				t.Fatalf("vfs.NewWriter: %v", err)
			}

			byteCache := cache.NewLRUCache(&cache.CacheConfig{
				MaxSize:    16 << 20,
				MaxEntries: 10000,
				TTL:        time.Hour,
			})
			t.Cleanup(func() { _ = byteCache.Close() })

			filesystem := NewFileSystem(t.Context(), backend, byteCache, writer, nil, &Config{
				DefaultMode:    0o644,
				DefaultDirMode: 0o755,
				DefaultUID:     1000,
				DefaultGID:     1000,
			})

			root, ok := filesystem.Root().(*DirectoryNode)
			if !ok {
				t.Fatalf("FileSystem.Root returned %T, want *DirectoryNode", filesystem.Root())
			}

			// The bridge, because Mkdir and Create both call NewInode through it and a node with no
			// bridge panics on the first child.
			timeout := filesystem.attrTimeout()
			_ = gofuse.NewNodeFS(root, &gofuse.Options{
				AttrTimeout:     &timeout,
				EntryTimeout:    &timeout,
				NullPermissions: true,
			})

			// mkdir: a zero-byte marker object, PUT synchronously.
			var mkdirOut fuse.EntryOut
			if _, errno := root.Mkdir(ctx, "data", 0o755, &mkdirOut); errno != 0 {
				t.Fatalf("mkdir on storage_tier %q returned %v.\nThe directory marker is a zero-byte "+
					"PUT and %q has a %d-byte minimum billable size — which AWS bills against, not "+
					"validates against. A mount that cannot mkdir cannot be used at all",
					tier, errno, tier, s3.StorageTiers[tier].MinObjectSize)
			}

			if !srv.ObjectExists("data/") {
				t.Errorf("mkdir on %q reported success and wrote no marker object; an empty prefix is "+
					"indistinguishable from one that was never created", tier)
			}

			// create: attributes recorded now, no PUT. So the tier gate is reached at flush rather than
			// here, which means a create that reports success is not yet evidence the file can land.
			var createOut fuse.EntryOut

			_, _, _, errno := root.Create(ctx, "small.txt", 0, 0o644, &createOut)
			if errno != 0 {
				t.Fatalf("create on storage_tier %q returned %v", tier, errno)
			}

			// A body far below the minimum, because that is the size the gate refused and the size a
			// research workload has thousands of.
			//
			// Nine bytes rather than zero, and the difference is a property of the write path rather
			// than of this tier: a file created and never written flushes through the attribute-only
			// path, which finds no object to carry the metadata and deliberately stores nothing
			// (see Flusher.attemptAttrOnly). So `touch f` alone cannot exercise a write gate at all —
			// the first flush with content is what reaches it.
			const body = "9 bytes!!"

			if err := filesystem.buffer.Write("small.txt", 0, []byte(body)); err != nil {
				t.Fatalf("buffered write on storage_tier %q: %v", tier, err)
			}

			if err := filesystem.buffer.FlushAll(); err != nil {
				t.Fatalf("flushing a %d-byte file on storage_tier %q: %v.\n%q has a %d-byte minimum "+
					"billable size, which AWS bills against rather than validates against — S3 accepts "+
					"this PUT. Refusing it here loses the write at close(2)",
					len(body), tier, err, tier, s3.StorageTiers[tier].MinObjectSize)
			}

			if !srv.ObjectExists("small.txt") {
				t.Fatalf("the flush on %q reported success and no object exists", tier)
			}

			if got := srv.ObjectSize("small.txt"); got != int64(len(body)) {
				t.Errorf("the flushed file is %d bytes, want %d; the write has to actually be below the "+
					"minimum for this test to exercise the gate", got, len(body))
			}
		})
	}
}
