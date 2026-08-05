// Package main implements the ObjectFS C shared library via CGO.
//
// Build as a shared library:
//
//	Linux:  go build -buildmode=c-shared -o libobjectfs.so  ./sdks/c/
//	macOS:  go build -buildmode=c-shared -o libobjectfs.dylib ./sdks/c/
//
// The generated libobjectfs.h contains the raw CGO-derived declarations;
// users should include objectfs.h instead (the clean public header).
//
// # Integer conversions across the C boundary
//
// Every width conversion in this file was audited against the C signature it feeds (#200). The
// summary, because "which of these can truncate" is not answerable by reading any one line:
//
//   - objectfs_put's length was the one real defect. It took a size_t and passed C.int(length) to
//     C.GoBytes, narrowing 64 bits to 32. See the comment at the call site; it is now range-checked
//     and rejected rather than narrowed.
//   - Lengths derived from a Go slice (fillCStr, objectfs_get, objectfs_list) convert int to size_t.
//     A Go len is non-negative by construction and cannot exceed maxAllocSize, so these are
//     genuinely unreachable. Recorded here rather than at each site, because an unexplained
//     conversion is indistinguishable from an unexamined one and five identical comments would say
//     less than one list.
//   - Handle IDs are int64 both sides (objectfs_id_to_handle takes int64_t), so no narrowing occurs.
//   - objectfs_new's cache_bytes is C.long → int64. long is 64 bits on the LP64 targets this library
//     ships for and 32 on ILP32; either way it widens or stays, never narrows.
//   - objectfs_list's limit is C.int → int, which widens on every target this builds for, so the
//     conversion is safe. The *value* was not: a negative limit reached ListObjects, whose every use
//     of limit is gated on `limit > 0`, so it meant "no limit" by accident. Now rejected.
//
// The two guards are `putLengthError` and `listLimitError` rather than inline `if`s, so that
// main_test.go can reach them: that file cannot `import "C"` (go test refuses it), so a Go test in
// this package can neither build a C.size_t nor call an exported function. tests/test_basic.c covers
// the same two guards from the C side, which is the half that proves the return code and the error
// string actually arrive.
//
// # Why there are no suppression directives in this file
//
// The notes at the conversions below are plain comments. `#nosec` and `//nolint:gosec` were both tried
// and neither does anything here — a property of cgo rather than of how they were written, worth
// stating so the next person does not add one and assume it took effect.
//
// cgo rewrites every `C.f(...)` call into an inline closure carrying `/*line :N:C*/` directives, so by
// the time either tool sees the code, the call and any comment near it have collapsed onto the same
// synthetic position and the association between them is gone. Measured on a four-case probe — the
// directive on the line above the call, first of a comment block, last of a comment block, and
// trailing the call itself. In a pure-Go package all four suppress. In a cgo package none do: gosec
// reported every finding with `nosec: 0`, meaning it recognized no directive at all.
//
// The two runs then diverge in what they do with the findings neither can suppress:
//
//   - golangci-lint discards them. Its own log says so — `runner/invalid_issue: issue related to file
//     <go-build cache path> is skipped` — because the analyzed file is a build-cache artifact and not a
//     path in the repository. `lint` is therefore green here no matter what this file contains, and
//     that is not the `.golangci.yml` G115 exclusion doing it: lifting that exclusion still yields 0.
//   - security.yml's standalone gosec keeps them, with `"artifactLocation": {}` for the same reason.
//     GitHub's SARIF ingester rejects a result with no location and fails the whole upload, so that
//     workflow filters them out by hand.
//
// Which is the whole reason #200 had to exist: these conversions were invisible to both runs, so only
// a hand audit could reach them, and one of the three was a real defect.
//
// `CGO_ENABLED=0 gosec ./sdks/c/...`, which #200 suggested might resolve the paths, reports zero issues
// over zero files — the package cannot build without cgo, so that is a vacuous pass rather than a fix.
package main

/*
#cgo CFLAGS: -I.
#include "objectfs_types.h"
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// objectfs_client_t is an opaque token, not an address: it carries an int64 index into a Go-side
// handle table and is never dereferenced on either side. Converting between the two is done here,
// in C, rather than in Go with unsafe.Pointer(uintptr(id)).
//
// go vet reports that Go form as "possible misuse of unsafe.Pointer" and it is right to: the rule
// it enforces is that an integer must not become a pointer, because Go's garbage collector may move
// an object and will not update a value it cannot see is a reference. It has no way to express "this
// pointer is a token", and it runs outside golangci-lint, so a //nolint directive does not silence
// it — the two lines that carried one had never suppressed anything.
//
// Doing the arithmetic in C makes that structurally true instead of asserted. The value never exists
// as a Go pointer, so there is nothing for the collector to misinterpret, and the intent is stated
// by which language the cast is written in.
//
// They are typed objectfs_client_t rather than void *: cgo gives the typedef its own Go type, so a
// void * helper would return unsafe.Pointer and need a conversion at every call site — which is the
// conversion this exists to remove.
static inline objectfs_client_t objectfs_id_to_handle(int64_t id) { return (objectfs_client_t)(intptr_t)id; }
static inline int64_t objectfs_handle_to_id(objectfs_client_t h)  { return (int64_t)(intptr_t)h; }
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"unsafe"

	objectfssdk "github.com/scttfrdmn/objectfs/sdks/go/objectfs"
)

// entry holds per-handle state.
type entry struct {
	client  *objectfssdk.Client
	mu      sync.Mutex
	errCStr *C.char // C.malloc'd; freed and reallocated on each error change
}

var (
	store   sync.Map     // int64 → *entry
	counter atomic.Int64 // monotonically increasing handle IDs; 0 = invalid
)

// maxPutLength is the largest length objectfs_put accepts, and it is math.MaxInt32 because that is
// what C.GoBytes's C.int parameter can represent — not a policy choice about object size.
//
// Written as a math constant rather than 1<<31-1 so it stays correct if it is ever compared against a
// differently-sized type, and stated as its own name so the error message and the guard cannot drift
// apart.
const maxPutLength = math.MaxInt32

// putLengthError reports why a length cannot cross into C.GoBytes, or nil if it can.
//
// Split out of objectfs_put as a plain Go function taking a uint64 so it can be tested. main_test.go
// deliberately does not `import "C"` — `go test` refuses it outright ("use of cgo in test
// main_test.go not supported") — so a test in this package cannot construct a C.size_t or call the
// exported function at all. Everything above this line in objectfs_put is nil-checks; this is the
// only decision it makes about the length, and putting it here is what makes that decision reachable
// from a Go test rather than only from tests/test_basic.c.
func putLengthError(length uint64) error {
	if length > maxPutLength {
		return fmt.Errorf("length %d exceeds the maximum this API can transfer in one call (%d bytes); "+
			"split the object or use multipart", length, uint64(maxPutLength))
	}

	return nil
}

// listLimitError reports why a limit is not acceptable, or nil if it is.
//
// Takes an int64 rather than a C.int for the same reason putLengthError takes a uint64: so a Go test
// can call it. int64 holds every C.int on every target, so no case is lost in the widening.
func listLimitError(limit int64) error {
	if limit < 0 {
		return fmt.Errorf("limit %d is negative; pass 0 for no limit, or a positive count", limit)
	}

	return nil
}

// Global error string for failed objectfs_new calls (no valid handle yet).
var (
	globalErrMu  sync.Mutex
	globalErrStr *C.char
)

// freedHandleStr is the string objectfs_last_error returns for a handle that is non-NULL but has no
// entry — freed, or never issued. Allocated once, at init, and never freed.
//
// It has to be a single owned allocation because objectfs.h:133-135 promises the returned pointer is
// "valid until the next call on the same handle. Do NOT free it." That arm used to return
// C.CString("invalid or freed handle"), a fresh malloc on every call: the caller cannot free it
// without contradicting the documented contract, and the library never freed it either, so every
// call leaked 24 bytes. Verified by calling it three times on a bogus handle and printing the
// pointers — three distinct addresses. Use-after-free error handling is exactly the path a program
// takes when it is already going wrong, and often in a loop.
var freedHandleStr *C.char

func init() {
	// Pre-allocate the fixed strings so we never return nil from objectfs_last_error, and never
	// allocate inside it.
	globalErrStr = C.CString("")
	freedHandleStr = C.CString("invalid or freed handle")
}

// main is required for buildmode=c-shared but is never called.
func main() {}

// --- Internal helpers ---------------------------------------------------

// toHandle and fromHandle convert between a handle-table index and the opaque token C sees. The
// cast itself lives in the cgo preamble; see the comment there for why it is not written in Go.
func toHandle(id int64) C.objectfs_client_t {
	return C.objectfs_id_to_handle(C.int64_t(id))
}

func fromHandle(h C.objectfs_client_t) int64 {
	return int64(C.objectfs_handle_to_id(h))
}

func getEntry(h C.objectfs_client_t) *entry {
	if unsafe.Pointer(h) == nil {
		return nil
	}
	id := fromHandle(h)
	if val, ok := store.Load(id); ok {
		// Comma-ok rather than a bare assertion. store is a sync.Map, so its value type is `any` and
		// the compiler cannot help; nothing but this file writes to it, and it writes only *entry, but
		// a bare assertion would turn any future violation of that into a panic — and a panic in a
		// c-shared library takes the host process down, not just this call. Returning nil lands the
		// caller on the same path a freed handle takes, which is already handled everywhere.
		e, ok := val.(*entry)
		if !ok {
			return nil
		}

		return e
	}

	return nil
}

// setErr stores the error message in a C.malloc'd string on the entry.
// Passing nil clears the error.
func (e *entry) setErr(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.errCStr != nil {
		C.free(unsafe.Pointer(e.errCStr))
	}
	e.errCStr = C.CString(msg)
}

// getErrCStr returns the C string pointer; never nil.
func (e *entry) getErrCStr() *C.char {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.errCStr
}

func setGlobalErr(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	globalErrMu.Lock()
	defer globalErrMu.Unlock()
	if globalErrStr != nil {
		C.free(unsafe.Pointer(globalErrStr))
	}
	globalErrStr = C.CString(msg)
}

// codeFromErr maps a Go SDK error to a C return code.
func codeFromErr(err error) C.int {
	if err == nil {
		return C.OBJECTFS_OK
	}
	switch {
	case errors.Is(err, objectfssdk.ErrNotFound):
		return C.OBJECTFS_ERR_NOT_FOUND
	case errors.Is(err, objectfssdk.ErrAccessDenied):
		return C.OBJECTFS_ERR_ACCESS
	case errors.Is(err, objectfssdk.ErrNotMounted):
		return C.OBJECTFS_ERR_NOT_MOUNTED
	case errors.Is(err, objectfssdk.ErrAlreadyMounted):
		return C.OBJECTFS_ERR_MOUNTED
	case errors.Is(err, objectfssdk.ErrInvalidConfig):
		return C.OBJECTFS_ERR_INVALID
	default:
		return C.OBJECTFS_ERR_IO
	}
}

// bytesToCacheString renders a byte count in the spelling objectfssdk.WithCacheSize accepts.
//
// A bare byte count, not a unit-scaled one. This used to divide by 1 GiB or 1 MiB and format the
// integer quotient, which discards the remainder: objectfs_new(bucket, region, 1610612736) — the
// 1.5 GiB that objectfs.h documents as "memory cache size in bytes" — became "1GB", so the caller
// got 1 GiB and lost half of what it asked for, silently. 2047 MiB became "1GB", losing 1023 MiB.
// The rounding was invisible from C: nothing echoes the size back, and a cache that is half the
// requested size still works, just with a worse hit rate than the caller provisioned for.
//
// utils.ParseBytes, which is what consumes this, accepts a plain count with a "B" suffix and
// multiplies by 1, so "1610612736B" round-trips exactly. Verified across the range, including
// math.MaxInt64. (Its float64 multiply means a *unit-scaled* spelling can be inexact above 2^53;
// with multiplier 1 there is no multiply to lose precision in.)
func bytesToCacheString(n int64) string {
	return fmt.Sprintf("%dB", n)
}

// fillCStr copies src into the C char array starting at dst, leaving room for
// a NUL terminator. capacity is the total array size in bytes.
func fillCStr(dst unsafe.Pointer, src string, capacity int) {
	if capacity <= 0 {
		return
	}
	n := min(len(src), capacity-1)
	if n > 0 {
		cs := C.CString(src[:n])
		// G115, and unreachable: n is in [1, capacity-1], and capacity comes from unsafe.Sizeof of a
		// fixed C array, so it can be neither negative nor larger than size_t. No suppression
		// directive — see the header comment for why one would not work here.
		C.memcpy(dst, unsafe.Pointer(cs), C.size_t(n))
		C.free(unsafe.Pointer(cs))
	}
	// Null-terminate.
	*(*C.char)(unsafe.Add(dst, n)) = 0
}

// fillInfo copies fields from a types.ObjectInfo into a C objectfs_info_t.
//
// Each capacity is taken from the array it describes with unsafe.Sizeof rather than written as a
// literal. The literals were 1024/128/128 and objectfs_types.h is what decides those numbers, so
// widening key from 1024 to 1025 there would have left this file still truncating at 1023 — the
// declaration and its one writer silently disagreeing, which is the shape of the bug that made the
// widening necessary. Derived, they cannot drift.
func fillInfo(dst *C.objectfs_info_t, key, etag, contentType string, size, mtimeSec int64) {
	C.memset(unsafe.Pointer(dst), 0, C.size_t(unsafe.Sizeof(*dst)))
	fillCStr(unsafe.Pointer(&dst.key), key, int(unsafe.Sizeof(dst.key)))
	dst.size = C.int64_t(size)
	dst.mtime_sec = C.int64_t(mtimeSec)
	fillCStr(unsafe.Pointer(&dst.etag), etag, int(unsafe.Sizeof(dst.etag)))
	fillCStr(unsafe.Pointer(&dst.content_type), contentType, int(unsafe.Sizeof(dst.content_type)))
}

// --- Exported functions -------------------------------------------------

//export objectfs_new
func objectfs_new(bucket *C.char, region *C.char, cacheBytes C.long) C.objectfs_client_t {
	if bucket == nil || C.GoString(bucket) == "" {
		setGlobalErr(fmt.Errorf("bucket name is required"))
		return nil
	}

	opts := []objectfssdk.Option{}
	if region != nil {
		if r := C.GoString(region); r != "" {
			opts = append(opts, objectfssdk.WithRegion(r))
		}
	}
	if cacheBytes > 0 {
		opts = append(opts, objectfssdk.WithCacheSize(bytesToCacheString(int64(cacheBytes))))
	}

	client, err := objectfssdk.New(context.Background(), C.GoString(bucket), opts...)
	if err != nil {
		setGlobalErr(err)
		return nil
	}

	e := &entry{
		client:  client,
		errCStr: C.CString(""),
	}
	id := counter.Add(1)
	store.Store(id, e)
	setGlobalErr(nil) // clear global error on success
	return toHandle(id)
}

//export objectfs_free
func objectfs_free(handle C.objectfs_client_t) {
	if unsafe.Pointer(handle) == nil {
		return
	}
	id := fromHandle(handle)
	if val, ok := store.LoadAndDelete(id); ok {
		e, ok := val.(*entry)
		if !ok {
			// Already removed from the table by LoadAndDelete, so there is nothing to leak and nothing
			// further to do. See the note in getEntry for why this is not a bare assertion.
			return
		}

		e.mu.Lock()
		_ = e.client.Close()
		if e.errCStr != nil {
			C.free(unsafe.Pointer(e.errCStr))
			e.errCStr = nil
		}
		e.mu.Unlock()
	}
}

//export objectfs_get
func objectfs_get(handle C.objectfs_client_t, key *C.char, dataOut *unsafe.Pointer, lenOut *C.size_t) C.int {
	e := getEntry(handle)
	if e == nil {
		return C.OBJECTFS_ERR_INVALID
	}
	if key == nil || dataOut == nil || lenOut == nil {
		e.setErr(fmt.Errorf("key, data_out, and len_out are required"))
		return C.OBJECTFS_ERR_INVALID
	}

	data, err := e.client.Get(context.Background(), C.GoString(key), 0, 0)
	if err != nil {
		e.setErr(err)
		return codeFromErr(err)
	}

	n := len(data)
	if n == 0 {
		*dataOut = nil
		*lenOut = 0
		e.setErr(nil)
		return C.OBJECTFS_OK
	}

	buf := C.malloc(C.size_t(n))
	if buf == nil {
		e.setErr(fmt.Errorf("out of memory allocating %d bytes", n))
		return C.OBJECTFS_ERR_IO
	}
	C.memcpy(buf, unsafe.Pointer(&data[0]), C.size_t(n))
	*dataOut = buf
	*lenOut = C.size_t(n)
	e.setErr(nil)
	return C.OBJECTFS_OK
}

//export objectfs_put
func objectfs_put(handle C.objectfs_client_t, key *C.char, data unsafe.Pointer, length C.size_t) C.int {
	e := getEntry(handle)
	if e == nil {
		return C.OBJECTFS_ERR_INVALID
	}
	if key == nil {
		e.setErr(fmt.Errorf("key is required"))
		return C.OBJECTFS_ERR_INVALID
	}

	// The length is refused when it will not survive the conversion, rather than converted and used.
	//
	// C.GoBytes's second parameter is a C.int — 32 bits on every target this library builds for —
	// while `length` is a size_t, which is 64 on all of them. `C.int(length)` therefore narrowed, and
	// the results are not merely wrong but wrong in three different directions. Measured, not reasoned:
	//
	//	length = 1<<32     (4 GiB)  -> C.int 0            -> len(goData) == 0
	//	length = (1<<32)+100        -> C.int 100          -> len(goData) == 100
	//	length = 1<<31     (2 GiB)  -> C.int -2147483648  -> panic: gobytes: length out of range
	//
	// The first is the dangerous one and is exactly this issue's stated concern: a caller hands over
	// 4 GiB, `objectfs_put` returns OBJECTFS_OK, and S3 holds an **empty object**. Nothing reports a
	// short write, because from Go's side there was no short write — the length arrived as zero. The
	// third is worse in a shared library than in a program: a panic in a c-shared build tears down the
	// host process, so a C caller loses its own unrelated state to a bad argument.
	//
	// maxPutLength is the largest value C.GoBytes can be given, and the check is `>` rather than `>=`
	// so the boundary itself stays usable. A 2 GiB single-object PUT is not something this SDK should
	// be doing regardless — S3's own single-request limit is 5 GiB and multipart exists for a reason —
	// but the API has to say so rather than round the request down to nothing.
	if err := putLengthError(uint64(length)); err != nil {
		e.setErr(err)

		return C.OBJECTFS_ERR_INVALID
	}

	var goData []byte
	if length > 0 && data != nil {
		// G115. Bounded by putLengthError immediately above, which is the whole subject of this
		// function's comment; the conversion is safe only because of that check.
		goData = C.GoBytes(data, C.int(length))
	}

	err := e.client.Put(context.Background(), C.GoString(key), goData)
	e.setErr(err)

	return codeFromErr(err)
}

//export objectfs_delete
func objectfs_delete(handle C.objectfs_client_t, key *C.char) C.int {
	e := getEntry(handle)
	if e == nil {
		return C.OBJECTFS_ERR_INVALID
	}
	if key == nil {
		e.setErr(fmt.Errorf("key is required"))
		return C.OBJECTFS_ERR_INVALID
	}

	err := e.client.Delete(context.Background(), C.GoString(key))
	e.setErr(err)
	return codeFromErr(err)
}

//export objectfs_head
func objectfs_head(handle C.objectfs_client_t, key *C.char, infoOut *C.objectfs_info_t) C.int {
	e := getEntry(handle)
	if e == nil {
		return C.OBJECTFS_ERR_INVALID
	}
	if key == nil || infoOut == nil {
		e.setErr(fmt.Errorf("key and info_out are required"))
		return C.OBJECTFS_ERR_INVALID
	}

	info, err := e.client.Head(context.Background(), C.GoString(key))
	if err != nil {
		e.setErr(err)
		return codeFromErr(err)
	}

	fillInfo(infoOut, info.Key, info.ETag, info.ContentType, info.Size, info.LastModified.Unix())
	e.setErr(nil)
	return C.OBJECTFS_OK
}

//export objectfs_list
func objectfs_list(handle C.objectfs_client_t, prefix *C.char, limit C.int, resultOut *C.objectfs_list_result_t) C.int {
	e := getEntry(handle)
	if e == nil {
		return C.OBJECTFS_ERR_INVALID
	}
	if prefix == nil || resultOut == nil {
		e.setErr(fmt.Errorf("prefix and result_out are required"))
		return C.OBJECTFS_ERR_INVALID
	}

	// objectfs.h documents limit as "max results (0 = no limit, up to S3 page maximum)", and says
	// nothing about a negative one. Downstream, ListObjects gates every use of limit on `limit > 0`, so
	// a negative value is silently treated as "no limit" — the caller asks for -1 results and gets the
	// whole bucket. That is a plausible thing for a C caller to pass, since -1 means "unlimited" in a
	// good deal of C API design, and here it happens to mean the same thing by accident rather than by
	// contract. Rejecting it says which one it is.
	//
	// The conversion itself is safe in both directions: C.int is 32 bits and Go's int is at least that
	// on every target this builds for, so int(limit) widens or matches and never narrows.
	if err := listLimitError(int64(limit)); err != nil {
		e.setErr(err)

		return C.OBJECTFS_ERR_INVALID
	}

	objects, err := e.client.List(context.Background(), C.GoString(prefix), int(limit))
	if err != nil {
		e.setErr(err)

		return codeFromErr(err)
	}

	// len of a Go slice, so non-negative and far below size_t's range on every target.
	n := len(objects)
	resultOut.count = C.size_t(n)
	resultOut.items = nil

	if n > 0 {
		infoSize := C.size_t(unsafe.Sizeof(C.objectfs_info_t{}))

		items := (*C.objectfs_info_t)(C.malloc(C.size_t(n) * infoSize))
		if items == nil {
			e.setErr(fmt.Errorf("out of memory for list result"))

			return C.OBJECTFS_ERR_IO
		}

		for i, obj := range objects {
			item := (*C.objectfs_info_t)(unsafe.Add(unsafe.Pointer(items), uintptr(i)*unsafe.Sizeof(C.objectfs_info_t{})))
			fillInfo(item, obj.Key, obj.ETag, obj.ContentType, obj.Size, obj.LastModified.Unix())
		}

		resultOut.items = items
	}

	e.setErr(nil)

	return C.OBJECTFS_OK
}

//export objectfs_mount
func objectfs_mount(handle C.objectfs_client_t, mountpoint *C.char) C.int {
	e := getEntry(handle)
	if e == nil {
		return C.OBJECTFS_ERR_INVALID
	}
	if mountpoint == nil {
		e.setErr(fmt.Errorf("mountpoint is required"))
		return C.OBJECTFS_ERR_INVALID
	}

	err := e.client.Mount(context.Background(), C.GoString(mountpoint))
	e.setErr(err)
	return codeFromErr(err)
}

//export objectfs_unmount
func objectfs_unmount(handle C.objectfs_client_t) C.int {
	e := getEntry(handle)
	if e == nil {
		return C.OBJECTFS_ERR_INVALID
	}

	err := e.client.Unmount()
	e.setErr(err)
	return codeFromErr(err)
}

//export objectfs_last_error
func objectfs_last_error(handle C.objectfs_client_t) *C.char {
	if unsafe.Pointer(handle) == nil {
		globalErrMu.Lock()
		s := globalErrStr
		globalErrMu.Unlock()
		return s
	}
	e := getEntry(handle)
	if e == nil {
		// Handle ID was valid once but has been freed. Returns the shared allocation rather than a
		// fresh C.CString; see the note on freedHandleStr.
		return freedHandleStr
	}
	return e.getErrCStr()
}

//export objectfs_free_data
func objectfs_free_data(data unsafe.Pointer) {
	if data != nil {
		C.free(data)
	}
}

//export objectfs_free_list
func objectfs_free_list(result *C.objectfs_list_result_t) {
	if result == nil {
		return
	}
	if result.items != nil {
		C.free(unsafe.Pointer(result.items))
		result.items = nil
	}
	result.count = 0
}
