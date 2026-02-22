# ObjectFS C SDK

C shared library (`libobjectfs`) that exposes ObjectFS S3 object operations
and optional FUSE filesystem mounting through a stable C ABI.

The library is built from Go source using `go build -buildmode=c-shared`,
so no C rewrite is needed — the Go runtime is embedded in the `.so`/`.dylib`.

Because the C ABI is stable, the library can be used from any language with
a C FFI: Python (`ctypes`/`cffi`), Ruby, Julia, R, Rust, and others.

---

## Prerequisites

| Tool | Version |
|------|---------|
| Go   | ≥ 1.26  |
| gcc / clang | any modern version |
| Python | ≥ 3.8 (for smoke test) |
| AWS credentials | for integration tests |

On Linux, FUSE 3 is required for `objectfs_mount` / `objectfs_unmount`:

```bash
# Debian / Ubuntu
sudo apt install libfuse3-dev fuse3

# RHEL / Fedora
sudo dnf install fuse3-devel
```

---

## Building

Run from the **repository root**:

```bash
# Linux
go build -buildmode=c-shared -o sdks/c/libobjectfs.so  ./sdks/c/

# macOS
go build -buildmode=c-shared -o sdks/c/libobjectfs.dylib ./sdks/c/

# Or use the Makefile (auto-detects OS)
cd sdks/c && make build
```

The build produces two files in `sdks/c/`:

| File | Description |
|------|-------------|
| `libobjectfs.so` / `.dylib` | Shared library to link against |
| `libobjectfs.h` | Raw CGO-generated declarations (for reference) |
| `objectfs.h` | **Use this** — clean documented public header |

---

## API overview

```c
#include "objectfs.h"

/* Lifecycle */
objectfs_client_t objectfs_new(const char *bucket, const char *region,
                                long cache_bytes);
void              objectfs_free(objectfs_client_t client);

/* Object operations */
int objectfs_get(objectfs_client_t, const char *key,
                 void **data_out, size_t *len_out);
int objectfs_put(objectfs_client_t, const char *key,
                 const void *data, size_t len);
int objectfs_delete(objectfs_client_t, const char *key);
int objectfs_head(objectfs_client_t, const char *key,
                  objectfs_info_t *info_out);
int objectfs_list(objectfs_client_t, const char *prefix, int limit,
                  objectfs_list_result_t *result_out);

/* FUSE mount */
int objectfs_mount(objectfs_client_t, const char *mountpoint);
int objectfs_unmount(objectfs_client_t);

/* Error / memory */
const char *objectfs_last_error(objectfs_client_t);
void        objectfs_free_data(void *data);
void        objectfs_free_list(objectfs_list_result_t *result);
```

Return codes: `OBJECTFS_OK` (0) on success; negative on error.
See `objectfs.h` for the full constant list.

---

## Usage in C

```c
#include <stdio.h>
#include <string.h>
#include "objectfs.h"

int main(void) {
    objectfs_client_t c = objectfs_new("my-bucket", "us-west-2", 0);
    if (!c) {
        fprintf(stderr, "connect failed: %s\n", objectfs_last_error(NULL));
        return 1;
    }

    /* Upload */
    const char *data = "hello, world";
    if (objectfs_put(c, "hello.txt", data, strlen(data)) != OBJECTFS_OK) {
        fprintf(stderr, "put failed: %s\n", objectfs_last_error(c));
    }

    /* Download */
    void  *buf = NULL;
    size_t len = 0;
    if (objectfs_get(c, "hello.txt", &buf, &len) == OBJECTFS_OK) {
        printf("got %zu bytes\n", len);
        objectfs_free_data(buf);
    }

    /* List */
    objectfs_list_result_t result;
    if (objectfs_list(c, "", 100, &result) == OBJECTFS_OK) {
        for (size_t i = 0; i < result.count; i++)
            printf("  %s  (%lld bytes)\n",
                   result.items[i].key,
                   (long long)result.items[i].size);
        objectfs_free_list(&result);
    }

    objectfs_free(c);
    return 0;
}
```

Compile and link:

```bash
# Linux
gcc example.c -I sdks/c -L sdks/c -lobjectfs \
    -Wl,-rpath,'$ORIGIN' -o example

# macOS
clang example.c -I sdks/c -L sdks/c -lobjectfs \
    -Wl,-rpath,@loader_path -o example
```

---

## Usage in Python (ctypes)

```python
import ctypes, os, sys

lib = ctypes.CDLL("./libobjectfs.so")  # or .dylib on macOS

lib.objectfs_new.restype  = ctypes.c_void_p
lib.objectfs_new.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_long]
lib.objectfs_put.restype  = ctypes.c_int
lib.objectfs_put.argtypes = [ctypes.c_void_p, ctypes.c_char_p,
                              ctypes.c_void_p, ctypes.c_size_t]
lib.objectfs_free.restype  = None
lib.objectfs_free.argtypes = [ctypes.c_void_p]

client = lib.objectfs_new(b"my-bucket", b"us-west-2", 0)
if not client:
    print("connect failed")
    sys.exit(1)

data = b"hello from Python"
rc = lib.objectfs_put(client, b"hello.txt", data, len(data))
print("put rc:", rc)

lib.objectfs_free(client)
```

See `tests/test_smoke.py` for a complete example with all operations.

---

## Running tests

```bash
cd sdks/c

# Build the shared library first
make build

# Unit tests only (no AWS credentials needed)
make test-c
make test-python

# Integration tests (requires real S3 bucket)
export AWS_PROFILE=aws
export AWS_REGION=us-west-2
export OBJECTFS_TEST_BUCKET=my-test-bucket
make test
```

---

## Memory ownership rules

| Function | Caller responsibility |
|----------|-----------------------|
| `objectfs_get` | Free `*data_out` with `objectfs_free_data()` |
| `objectfs_list` | Free `result` with `objectfs_free_list()` |
| `objectfs_last_error` | **Do not free** the returned pointer |
| All other strings | Passed in by caller; no ownership transfer |
