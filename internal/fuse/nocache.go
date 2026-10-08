package fuse

import "github.com/scttfrdmn/objectfs/pkg/types"

// noCache is the cache a FileSystem uses when it is given none: it holds nothing, so every Get misses
// and every read goes to the backend.
//
// It exists so that "no cache" has one meaning everywhere in this package. Before it, NewFileSystem
// stored a nil cache as given; the metadata cache helpers checked for nil and the data path did not,
// so a FileSystem built without a cache served stat(2) and then failed its first read(2) with a nil
// dereference that go-fuse turns into EIO (#543). Substituting this at construction makes every call
// site correct, including ones not yet written, without each having to remember a nil check.
type noCache struct{}

var _ types.Cache = noCache{}

func (noCache) Get(string, int64, int64) []byte { return nil }
func (noCache) Put(string, int64, []byte)       {}
func (noCache) Delete(string)                   {}
func (noCache) Evict(int64) bool                { return false }
func (noCache) Size() int64                     { return 0 }
func (noCache) Stats() types.CacheStats         { return types.CacheStats{} }
