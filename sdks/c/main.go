// Package main implements the ObjectFS C shared library via CGO.
//
// Build as a shared library:
//
//	Linux:  go build -buildmode=c-shared -o libobjectfs.so  ./sdks/c/
//	macOS:  go build -buildmode=c-shared -o libobjectfs.dylib ./sdks/c/
//
// The generated libobjectfs.h contains the raw CGO-derived declarations;
// users should include objectfs.h instead (the clean public header).
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
	store   sync.Map // int64 → *entry
	counter int64    // monotonically increasing handle IDs; 0 = invalid
)

// Global error string for failed objectfs_new calls (no valid handle yet).
var (
	globalErrMu  sync.Mutex
	globalErrStr *C.char
)

func init() {
	// Pre-allocate empty strings so we never return nil from objectfs_last_error.
	globalErrStr = C.CString("")
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
		return val.(*entry)
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

// bytesToCacheString converts a byte count to a human-readable cache size string
// that objectfssdk.WithCacheSize accepts (e.g. "512MB", "2GB").
func bytesToCacheString(n int64) string {
	const (
		mb = 1024 * 1024
		gb = 1024 * 1024 * 1024
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%dGB", n/gb)
	case n >= mb:
		return fmt.Sprintf("%dMB", n/mb)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// fillCStr copies src into the C char array starting at dst, leaving room for
// a NUL terminator. capacity is the total array size in bytes.
func fillCStr(dst unsafe.Pointer, src string, capacity int) {
	if capacity <= 0 {
		return
	}
	n := len(src)
	if n > capacity-1 {
		n = capacity - 1
	}
	if n > 0 {
		cs := C.CString(src[:n])
		C.memcpy(dst, unsafe.Pointer(cs), C.size_t(n))
		C.free(unsafe.Pointer(cs))
	}
	// Null-terminate.
	*(*C.char)(unsafe.Pointer(uintptr(dst) + uintptr(n))) = 0
}

// fillInfo copies fields from a types.ObjectInfo into a C objectfs_info_t.
func fillInfo(dst *C.objectfs_info_t, key, etag, contentType string, size, mtimeSec int64) {
	C.memset(unsafe.Pointer(dst), 0, C.size_t(unsafe.Sizeof(*dst)))
	fillCStr(unsafe.Pointer(&dst.key), key, 1024)
	dst.size = C.int64_t(size)
	dst.mtime_sec = C.int64_t(mtimeSec)
	fillCStr(unsafe.Pointer(&dst.etag), etag, 128)
	fillCStr(unsafe.Pointer(&dst.content_type), contentType, 128)
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
	id := atomic.AddInt64(&counter, 1)
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
		e := val.(*entry)
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

	var goData []byte
	if length > 0 && data != nil {
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

	objects, err := e.client.List(context.Background(), C.GoString(prefix), int(limit))
	if err != nil {
		e.setErr(err)
		return codeFromErr(err)
	}

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
			item := (*C.objectfs_info_t)(unsafe.Pointer(
				uintptr(unsafe.Pointer(items)) + uintptr(i)*unsafe.Sizeof(C.objectfs_info_t{}),
			))
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
		// Handle ID was valid once but has been freed.
		return C.CString("invalid or freed handle")
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
