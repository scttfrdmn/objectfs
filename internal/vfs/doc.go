/*
Package vfs holds ObjectFS's POSIX-semantics core, independent of any kernel binding.

It owns the file model — attributes, open handles, and dirty byte ranges — and the policy for
turning a sequence of POSIX operations into object-storage operations. It depends on
[github.com/scttfrdmn/objectfs/pkg/types].Backend and on nothing FUSE, so it builds and tests on
every platform and needs no mount.

# Why this package exists

Through v0.10.0 there was no such layer. internal/fuse coupled POSIX semantics directly to go-fuse
types — fuse.EntryOut, fs.Inode, syscall.Errno — leaving nowhere to express "a file has a size, an
attribute set, dirty ranges, and open handles." An audit of that release found roughly forty-five
defects, and the clustering was not accidental:

  - Six write-path defects were one missing concept. S3's PutObject is a whole-object replace while
    the FUSE contract is "modify a byte range," and the buffer that stood between them was a single
    contiguous []byte plus one offset. The flush callback then dropped even that offset, so
    appending one byte to a 1 MiB file left a 1-byte object. Non-contiguous writes — SQLite, mmap
    writeback, tar, HDF5 — were rejected outright with EIO.

  - Attributes could not persist because no type owned them.

  - The mode backstop was missing for the same reason, so directories reported mode 0000 and every
    non-root user got EACCES.

  - A second kernel binding drifted silently for want of a shared core to bind to. It never
    received the Unlink/Rmdir fix, so under that build tag rm reported success while the object
    survived in S3.

The deeper problem was testability. Those are all *seam* defects — a value correctly produced at
one layer and silently dropped at the boundary to the next — and the v0.10.0 suite's 32,680 lines
across 90 files caught none of them, because a unit test that mocks the neighboring layer cannot
observe a seam by construction. Semantics reachable only through a live mount are semantics that do
not get tested.

# Structure

[ExtentList] is the write path's core: an ordered, coalesced, non-overlapping set of dirty byte
ranges. Later writes win over earlier ones on overlap, which is the property v0.10.0's mergeWrites
inverted — it guarded the overlay with "is this write longer than what I have," so echoing new
content over a longer old file left the old content in place.

[ExtentList.Plan] answers the question the flush path must ask and previously could not: given what
is in the object store and what the file should now be, can this be written with a single PUT, or
must the current object be fetched and spliced first? Read-modify-write is not an optimisation
detail here — without it, every offset write is data loss.

Errors are this package's own ([ErrNotFound] and friends), never syscall.Errno. Mapping to errno is
the caller's job, so that a second binding — Windows, or an NFS loopback — is a translation shim
over a tested core rather than a fork.
*/
package vfs
