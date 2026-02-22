#!/usr/bin/env python3
"""
test_smoke.py — Python ctypes smoke test for libobjectfs.

Validates that the shared library loads and the C API works correctly
from Python ctypes. Integration tests require OBJECTFS_TEST_BUCKET and
AWS_ACCESS_KEY_ID to be set; they are skipped otherwise.

Usage:
    cd sdks/c
    make build
    LD_LIBRARY_PATH=. python3 tests/test_smoke.py          # Linux
    DYLD_LIBRARY_PATH=. python3 tests/test_smoke.py        # macOS
"""

import ctypes
import os
import platform
import sys


# ---------------------------------------------------------------------------
# Load library
# ---------------------------------------------------------------------------

def _load_library() -> ctypes.CDLL:
    lib_name = "libobjectfs.dylib" if platform.system() == "Darwin" else "libobjectfs.so"
    # Look in the parent directory of this test file (sdks/c/).
    search = [
        os.path.join(os.path.dirname(__file__), "..", lib_name),
        os.path.join(os.getcwd(), lib_name),
    ]
    for path in search:
        path = os.path.abspath(path)
        if os.path.exists(path):
            try:
                return ctypes.CDLL(path)
            except OSError as exc:
                print(f"SKIP: could not load {path}: {exc}", file=sys.stderr)
                sys.exit(0)
    print(
        f"SKIP: {lib_name} not found; run 'make build' in sdks/c/ first",
        file=sys.stderr,
    )
    sys.exit(0)


lib = _load_library()


# ---------------------------------------------------------------------------
# Function signatures
# ---------------------------------------------------------------------------

lib.objectfs_new.restype  = ctypes.c_void_p
lib.objectfs_new.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_long]

lib.objectfs_free.restype  = None
lib.objectfs_free.argtypes = [ctypes.c_void_p]

lib.objectfs_get.restype  = ctypes.c_int
lib.objectfs_get.argtypes = [
    ctypes.c_void_p,
    ctypes.c_char_p,
    ctypes.POINTER(ctypes.c_void_p),
    ctypes.POINTER(ctypes.c_size_t),
]

lib.objectfs_put.restype  = ctypes.c_int
lib.objectfs_put.argtypes = [
    ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p, ctypes.c_size_t
]

lib.objectfs_delete.restype  = ctypes.c_int
lib.objectfs_delete.argtypes = [ctypes.c_void_p, ctypes.c_char_p]

lib.objectfs_head.restype  = ctypes.c_int
lib.objectfs_head.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]

lib.objectfs_list.restype  = ctypes.c_int
lib.objectfs_list.argtypes = [
    ctypes.c_void_p,
    ctypes.c_char_p,
    ctypes.c_int,
    ctypes.c_void_p,
]

lib.objectfs_last_error.restype  = ctypes.c_char_p
lib.objectfs_last_error.argtypes = [ctypes.c_void_p]

lib.objectfs_free_data.restype  = None
lib.objectfs_free_data.argtypes = [ctypes.c_void_p]

lib.objectfs_free_list.restype  = None
lib.objectfs_free_list.argtypes = [ctypes.c_void_p]

# C struct mirrors
class ObjectfsInfo(ctypes.Structure):
    _fields_ = [
        ("key",          ctypes.c_char * 1024),
        ("size",         ctypes.c_int64),
        ("mtime_sec",    ctypes.c_int64),
        ("etag",         ctypes.c_char * 128),
        ("content_type", ctypes.c_char * 128),
    ]

class ObjectfsListResult(ctypes.Structure):
    _fields_ = [
        ("items", ctypes.POINTER(ObjectfsInfo)),
        ("count", ctypes.c_size_t),
    ]


# ---------------------------------------------------------------------------
# Test harness
# ---------------------------------------------------------------------------

_passed = 0
_failed = 0


def check(name: str, condition: bool, detail: str = "") -> None:
    global _passed, _failed
    if condition:
        print(f"  PASS: {name}")
        _passed += 1
    else:
        msg = f"  FAIL: {name}"
        if detail:
            msg += f"  ({detail})"
        print(msg)
        _failed += 1


def section(name: str) -> None:
    print(f"\n--- {name} ---")


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

def test_null_bucket() -> None:
    section("NULL / empty bucket")
    h = lib.objectfs_new(None, None, 0)
    check("objectfs_new(NULL) → None", h is None)

    h = lib.objectfs_new(b"", None, 0)
    check("objectfs_new('') → None", h is None)

    err = lib.objectfs_last_error(None)
    check("objectfs_last_error(NULL) returns bytes", isinstance(err, bytes))
    check("objectfs_last_error(NULL) non-empty after failure", len(err) > 0)


def test_free_null() -> None:
    section("objectfs_free / objectfs_free_data / objectfs_free_list with NULL")
    lib.objectfs_free(None)
    check("objectfs_free(None) does not crash", True)

    lib.objectfs_free_data(None)
    check("objectfs_free_data(None) does not crash", True)

    result = ObjectfsListResult()
    lib.objectfs_free_list(ctypes.byref(result))
    check("objectfs_free_list(zeroed) does not crash", True)

    lib.objectfs_free_list(None)
    check("objectfs_free_list(None) does not crash", True)


def test_ops_on_null_handle() -> None:
    section("Operations on NULL handle → OBJECTFS_ERR_INVALID (-1)")
    OBJECTFS_ERR_INVALID = -1

    data_ptr = ctypes.c_void_p(None)
    data_len = ctypes.c_size_t(0)
    rc = lib.objectfs_get(None, b"k", ctypes.byref(data_ptr), ctypes.byref(data_len))
    check("objectfs_get(NULL) == ERR_INVALID", rc == OBJECTFS_ERR_INVALID)

    rc = lib.objectfs_put(None, b"k", b"v", 1)
    check("objectfs_put(NULL) == ERR_INVALID", rc == OBJECTFS_ERR_INVALID)

    rc = lib.objectfs_delete(None, b"k")
    check("objectfs_delete(NULL) == ERR_INVALID", rc == OBJECTFS_ERR_INVALID)

    info = ObjectfsInfo()
    rc = lib.objectfs_head(None, b"k", ctypes.byref(info))
    check("objectfs_head(NULL) == ERR_INVALID", rc == OBJECTFS_ERR_INVALID)

    result = ObjectfsListResult()
    rc = lib.objectfs_list(None, b"", 0, ctypes.byref(result))
    check("objectfs_list(NULL) == ERR_INVALID", rc == OBJECTFS_ERR_INVALID)

    rc = lib.objectfs_mount(None, b"/tmp")
    check("objectfs_mount(NULL) == ERR_INVALID", rc == OBJECTFS_ERR_INVALID)

    rc = lib.objectfs_unmount(None)
    check("objectfs_unmount(NULL) == ERR_INVALID", rc == OBJECTFS_ERR_INVALID)


def test_integration() -> None:
    bucket = os.environ.get("OBJECTFS_TEST_BUCKET")
    region = os.environ.get("AWS_REGION", "us-east-1").encode()

    if not bucket or not os.environ.get("AWS_ACCESS_KEY_ID"):
        print(
            "\n--- Integration tests SKIPPED "
            "(set OBJECTFS_TEST_BUCKET + AWS_ACCESS_KEY_ID) ---"
        )
        return

    section("Integration: connect")
    bucket_b = bucket.encode()
    client = lib.objectfs_new(bucket_b, region, 0)
    if not client:
        err = lib.objectfs_last_error(None)
        check("objectfs_new", False, err.decode() if err else "no error")
        return
    check("objectfs_new", True)

    err = lib.objectfs_last_error(client)
    check("no error after successful new", err == b"")

    key     = b"objectfs-python-smoke/hello.txt"
    payload = b"hello from Python ctypes smoke test"

    # Put
    section("Integration: Put")
    rc = lib.objectfs_put(client, key, payload, len(payload))
    check(f"objectfs_put rc={rc}", rc == 0)

    # Get
    section("Integration: Get")
    data_ptr = ctypes.c_void_p(None)
    data_len = ctypes.c_size_t(0)
    rc = lib.objectfs_get(client, key, ctypes.byref(data_ptr), ctypes.byref(data_len))
    check(f"objectfs_get rc={rc}", rc == 0)
    if rc == 0 and data_ptr.value:
        buf = (ctypes.c_char * data_len.value).from_address(data_ptr.value)
        check("data matches payload", bytes(buf) == payload)
        lib.objectfs_free_data(data_ptr)

    # Head
    section("Integration: Head")
    info = ObjectfsInfo()
    rc = lib.objectfs_head(client, key, ctypes.byref(info))
    check(f"objectfs_head rc={rc}", rc == 0)
    if rc == 0:
        check("info.key matches", info.key == key)
        check("info.size correct", info.size == len(payload))
        check("info.mtime_sec > 0", info.mtime_sec > 0)

    # List
    section("Integration: List")
    result = ObjectfsListResult()
    rc = lib.objectfs_list(client, b"objectfs-python-smoke/", 10, ctypes.byref(result))
    check(f"objectfs_list rc={rc}", rc == 0)
    check("list count >= 1", result.count >= 1)
    lib.objectfs_free_list(ctypes.byref(result))

    # Get non-existent
    section("Integration: Get non-existent")
    OBJECTFS_ERR_NOT_FOUND = -2
    data_ptr = ctypes.c_void_p(None)
    data_len = ctypes.c_size_t(0)
    rc = lib.objectfs_get(
        client, b"objectfs-python-smoke/__no_such_key__",
        ctypes.byref(data_ptr), ctypes.byref(data_len),
    )
    check("missing key → ERR_NOT_FOUND", rc == OBJECTFS_ERR_NOT_FOUND)

    # Delete
    section("Integration: Delete")
    rc = lib.objectfs_delete(client, key)
    check(f"objectfs_delete rc={rc}", rc == 0)

    info2 = ObjectfsInfo()
    rc = lib.objectfs_head(client, key, ctypes.byref(info2))
    check("head after delete → ERR_NOT_FOUND", rc == OBJECTFS_ERR_NOT_FOUND)

    lib.objectfs_free(client)
    check("objectfs_free does not crash", True)


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

if __name__ == "__main__":
    print("=== ObjectFS Python ctypes smoke test ===")

    test_null_bucket()
    test_free_null()
    test_ops_on_null_handle()
    test_integration()

    print(f"\n=== Results: {_passed} passed, {_failed} failed ===")
    sys.exit(1 if _failed > 0 else 0)
