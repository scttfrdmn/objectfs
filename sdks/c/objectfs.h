/*
 * objectfs.h — Public C API for the ObjectFS shared library.
 *
 * Copyright 2025-2026 Scott Friedman. Apache License 2.0.
 *
 * Build the library:
 *   Linux:  go build -buildmode=c-shared -o libobjectfs.so  ./sdks/c/
 *   macOS:  go build -buildmode=c-shared -o libobjectfs.dylib ./sdks/c/
 *
 * Link against it:
 *   gcc myapp.c -I/path/to/sdks/c -L. -lobjectfs -o myapp
 *
 * All functions are thread-safe. Different handles may be used concurrently
 * without synchronisation; concurrent calls on the same handle are also safe.
 */

#ifndef OBJECTFS_H
#define OBJECTFS_H

#include "objectfs_types.h"

#ifdef __cplusplus
extern "C" {
#endif

/* =======================================================================
 * Lifecycle
 * ======================================================================= */

/**
 * objectfs_new — open a connection to an S3 bucket.
 *
 * @param bucket      S3 bucket name (required, non-empty)
 * @param region      AWS region, e.g. "us-east-1" (NULL → default "us-east-1")
 * @param cache_bytes memory cache size in bytes (0 → library default 512 MB)
 * @return            opaque client handle on success; NULL on failure.
 *
 * On failure, call objectfs_last_error(NULL) for a diagnostic string.
 * Uses the standard AWS credential chain (env vars, ~/.aws/credentials,
 * instance profile).
 */
objectfs_client_t objectfs_new(const char *bucket,
                                const char *region,
                                long        cache_bytes);

/**
 * objectfs_free — unmount (if mounted) and close all resources.
 *
 * Safe to call with NULL (no-op). After this call the handle is invalid.
 */
void objectfs_free(objectfs_client_t client);

/* =======================================================================
 * Object operations — work without a FUSE mount
 * ======================================================================= */

/**
 * objectfs_get — download an object from S3.
 *
 * On success *data_out points to a C.malloc'd buffer of *len_out bytes.
 * The caller MUST release it with objectfs_free_data().
 * Returns OBJECTFS_ERR_NOT_FOUND if the key does not exist.
 */
int objectfs_get(objectfs_client_t  client,
                 const char        *key,
                 void             **data_out,
                 size_t            *len_out);

/**
 * objectfs_put — upload bytes to S3.
 *
 * data may be NULL when len is 0 (creates an empty object).
 */
int objectfs_put(objectfs_client_t  client,
                 const char        *key,
                 const void        *data,
                 size_t             len);

/**
 * objectfs_delete — remove an object from S3.
 *
 * Deleting a non-existent key is a no-op (returns OBJECTFS_OK).
 */
int objectfs_delete(objectfs_client_t client, const char *key);

/**
 * objectfs_head — fetch object metadata without downloading the body.
 *
 * Returns OBJECTFS_ERR_NOT_FOUND if the key does not exist.
 */
int objectfs_head(objectfs_client_t  client,
                  const char        *key,
                  objectfs_info_t   *info_out);

/**
 * objectfs_list — list objects whose keys begin with prefix.
 *
 * @param prefix     key prefix filter ("" to list all objects)
 * @param limit      max results (0 = no limit, up to S3 page maximum)
 * @param result_out filled on success; caller must free with objectfs_free_list()
 */
int objectfs_list(objectfs_client_t       client,
                  const char             *prefix,
                  int                     limit,
                  objectfs_list_result_t *result_out);

/* =======================================================================
 * FUSE mount — requires FUSE support on the host
 * ======================================================================= */

/**
 * objectfs_mount — attach a POSIX filesystem at mountpoint.
 *
 * The directory at mountpoint must already exist.
 * Returns OBJECTFS_ERR_MOUNTED if the client is already mounted.
 */
int objectfs_mount(objectfs_client_t client, const char *mountpoint);

/**
 * objectfs_unmount — detach the FUSE filesystem.
 *
 * Returns OBJECTFS_ERR_NOT_MOUNTED if not currently mounted.
 */
int objectfs_unmount(objectfs_client_t client);

/* =======================================================================
 * Error handling
 * ======================================================================= */

/**
 * objectfs_last_error — human-readable description of the last error.
 *
 * When client is NULL, returns the error from the most recent failed
 * objectfs_new() call. The returned pointer is valid until the next call
 * on the same handle. Do NOT free it.
 *
 * Returns "" if no error has occurred.
 */
const char *objectfs_last_error(objectfs_client_t client);

/* =======================================================================
 * Memory management
 * ======================================================================= */

/**
 * objectfs_free_data — release a buffer returned by objectfs_get().
 *
 * Safe to call with NULL (no-op).
 */
void objectfs_free_data(void *data);

/**
 * objectfs_free_list — release a result returned by objectfs_list().
 *
 * Safe to call with a zero-initialised struct or a NULL items pointer.
 */
void objectfs_free_list(objectfs_list_result_t *result);

#ifdef __cplusplus
}
#endif

#endif /* OBJECTFS_H */
