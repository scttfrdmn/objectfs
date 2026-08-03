/*
 * objectfs_types.h — type definitions and constants for objectfs.h and
 * the CGO preamble in main.go.
 *
 * This header contains ONLY type definitions and constant macros —
 * no function declarations. It is included by:
 *   - objectfs.h  (the public API header)
 *   - main.go     (the CGO preamble, which cannot include function declarations
 *                  because CGO generates its own declarations for //export funcs)
 *
 * Copyright 2025-2026 Scott Friedman. Apache License 2.0.
 */

#ifndef OBJECTFS_TYPES_H
#define OBJECTFS_TYPES_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Opaque client handle. NULL represents an invalid / uninitialised client. */
typedef void *objectfs_client_t;

/* Object metadata — filled by objectfs_head() and objectfs_list(). */
typedef struct objectfs_info {
    /*
     * NUL-terminated object key (path).
     *
     * 1025, not 1024: an S3 key may be up to 1024 bytes when UTF-8 encoded, and a 1024-byte array
     * holding a NUL-terminated string can only carry 1023 of them. So the previous size could not
     * hold a legal maximum-length key, and fillCStr truncated it to 1023 bytes without saying so.
     *
     * That matters most in objectfs_list, where the key is the *result* rather than something the
     * caller passed in: a truncated key names a different object, or none, and the caller has no way
     * to tell. Passing it back to objectfs_get or objectfs_delete would act on the wrong key.
     * Reachable with an ordinary long key, not only a hostile one.
     */
    char    key[1025];
    int64_t size;              /* object size in bytes */
    int64_t mtime_sec;         /* last-modified, Unix seconds */
    char    etag[128];         /* S3 ETag, NUL-terminated */
    char    content_type[128]; /* MIME content-type, NUL-terminated */
} objectfs_info_t;

/* List result — release with objectfs_free_list(). */
typedef struct objectfs_list_result {
    objectfs_info_t *items; /* array of count items; NULL when count == 0 */
    size_t           count;
} objectfs_list_result_t;

/* Return codes */
#define OBJECTFS_OK               0
#define OBJECTFS_ERR_INVALID     -1  /* bad argument (NULL handle, empty key…) */
#define OBJECTFS_ERR_NOT_FOUND   -2  /* object does not exist */
#define OBJECTFS_ERR_ACCESS      -3  /* credentials / IAM permission error */
#define OBJECTFS_ERR_IO          -4  /* network or I/O error */
#define OBJECTFS_ERR_NOT_MOUNTED -5  /* FUSE operation before objectfs_mount() */
#define OBJECTFS_ERR_MOUNTED     -6  /* objectfs_mount() called twice */
#define OBJECTFS_ERR_INTERNAL    -9  /* unexpected internal error */

#ifdef __cplusplus
}
#endif

#endif /* OBJECTFS_TYPES_H */
