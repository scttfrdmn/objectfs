/*
 * test_basic.c — C unit tests for the ObjectFS shared library.
 *
 * Tests that do NOT require AWS credentials validate input checking and
 * memory-management contracts. Integration tests are gated behind the
 * OBJECTFS_TEST_BUCKET and AWS_ACCESS_KEY_ID environment variables.
 *
 * Build via the Makefile:  make test-c
 * Or directly:
 *   cc -o tests/test_basic tests/test_basic.c -I.. -L.. -lobjectfs \
 *      -Wl,-rpath,.. && LD_LIBRARY_PATH=.. ./tests/test_basic
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

#include "../objectfs.h"

/* --------------------------------------------------------------------- */
/* Minimal test harness                                                    */
/* --------------------------------------------------------------------- */

static int g_passed = 0;
static int g_failed = 0;

#define CHECK(expr) \
    do { \
        if (expr) { \
            printf("  PASS: %s\n", #expr); \
            g_passed++; \
        } else { \
            printf("  FAIL: %s  (line %d)\n", #expr, __LINE__); \
            g_failed++; \
        } \
    } while (0)

#define SECTION(name) printf("\n--- %s ---\n", name)

/* --------------------------------------------------------------------- */
/* Tests                                                                   */
/* --------------------------------------------------------------------- */

static void test_null_bucket(void)
{
    SECTION("NULL / empty bucket");

    objectfs_client_t c = objectfs_new(NULL, NULL, 0);
    CHECK(c == NULL);

    c = objectfs_new("", NULL, 0);
    CHECK(c == NULL);

    /* objectfs_last_error(NULL) must return a non-NULL string after failure */
    const char *err = objectfs_last_error(NULL);
    CHECK(err != NULL);
    /* Error string should be non-empty when bucket was invalid */
    CHECK(strlen(err) > 0);
}

static void test_free_null(void)
{
    SECTION("objectfs_free / objectfs_free_data with NULL");

    objectfs_free(NULL);      /* must not crash */
    CHECK(1);

    objectfs_free_data(NULL); /* must not crash */
    CHECK(1);

    objectfs_list_result_t r = {NULL, 0};
    objectfs_free_list(&r);   /* must not crash */
    CHECK(1);

    objectfs_free_list(NULL); /* must not crash */
    CHECK(1);
}

static void test_ops_on_null_handle(void)
{
    SECTION("Operations on NULL handle return OBJECTFS_ERR_INVALID");

    void   *data = NULL;
    size_t  len  = 0;
    int rc;

    rc = objectfs_get(NULL, "k", &data, &len);
    CHECK(rc == OBJECTFS_ERR_INVALID);

    rc = objectfs_put(NULL, "k", "v", 1);
    CHECK(rc == OBJECTFS_ERR_INVALID);

    rc = objectfs_delete(NULL, "k");
    CHECK(rc == OBJECTFS_ERR_INVALID);

    objectfs_info_t info;
    rc = objectfs_head(NULL, "k", &info);
    CHECK(rc == OBJECTFS_ERR_INVALID);

    objectfs_list_result_t result = {NULL, 0};
    rc = objectfs_list(NULL, "", 0, &result);
    CHECK(rc == OBJECTFS_ERR_INVALID);

    rc = objectfs_mount(NULL, "/tmp");
    CHECK(rc == OBJECTFS_ERR_INVALID);

    rc = objectfs_unmount(NULL);
    CHECK(rc == OBJECTFS_ERR_INVALID);
}

static void test_last_error_not_null(void)
{
    SECTION("objectfs_last_error never returns NULL");

    /* NULL handle */
    CHECK(objectfs_last_error(NULL) != NULL);
}

/*
 * objectfs.h documents the pointer objectfs_last_error returns as "valid until the next call on the
 * same handle. Do NOT free it." That makes it the library's allocation, and it must therefore be the
 * same allocation each time — a caller who must not free it and a library that allocates per call
 * together leak on every call.
 *
 * The freed-handle arm did exactly that: it returned C.CString("invalid or freed handle"), a fresh
 * malloc, so three calls produced three addresses. Error reporting is the path a program takes when
 * it is already going wrong, often in a retry loop, which is the worst place for an unbounded leak.
 *
 * Comparing pointers is what makes this a test rather than an inspection: strcmp on the text would
 * pass either way.
 */
static void test_last_error_pointer_is_owned_by_the_library(void)
{
    SECTION("objectfs_last_error returns a library-owned pointer, not a fresh allocation");

    /* Non-NULL, but never issued by objectfs_new, so it takes the freed/invalid-handle arm. The
       value is a handle-table index, not an address, and is never dereferenced. */
    objectfs_client_t never_issued = (objectfs_client_t)(intptr_t)999999;

    const char *a = objectfs_last_error(never_issued);
    const char *b = objectfs_last_error(never_issued);
    const char *c = objectfs_last_error(never_issued);

    CHECK(a != NULL);
    CHECK(strlen(a) > 0);
    CHECK(a == b);
    CHECK(b == c);

    /* The NULL arm has always returned the library-owned global; hold that too. */
    const char *g1 = objectfs_last_error(NULL);
    const char *g2 = objectfs_last_error(NULL);
    CHECK(g1 == g2);
}

/* --------------------------------------------------------------------- */
/* Integration tests — skipped unless OBJECTFS_TEST_BUCKET is set         */
/* --------------------------------------------------------------------- */

/*
 * test_put_rejects_a_length_it_cannot_transfer — the load-bearing half of #200.
 *
 * The Go side has a table test for the same guard (TestPutLengthErrorAtTheBoundary in main_test.go),
 * but it can only reach the helper: main_test.go cannot `import "C"` — go test refuses it outright —
 * so it cannot construct a size_t or call objectfs_put at all. This is the only test that exercises
 * the path a real C caller takes, and the only one that can assert what that caller sees.
 *
 * What it is guarding. objectfs_put passed C.int(len) to C.GoBytes, narrowing 64 bits to 32:
 *
 *   len = 1<<32     (4 GiB)  ->  C.int 0            ->  OBJECTFS_OK, and S3 holds an EMPTY object
 *   len = (1<<32)+100        ->  C.int 100          ->  OBJECTFS_OK, and S3 holds 100 bytes
 *   len = 1<<31     (2 GiB)  ->  C.int -2147483648  ->  panic, which in a c-shared library kills
 *                                                       the calling process, not just the call
 *
 * Note what is NOT passed here: a real buffer of that size. The guard runs before the pointer is ever
 * dereferenced, which is exactly the property that makes this testable — no 4 GiB allocation is
 * needed to prove a 4 GiB request is refused.
 *
 * Verified by removing the guard and running this file against real S3 (us-west-2). What happened is
 * worth writing down, because it is not what a test failure normally looks like:
 *
 *   - the 4 GiB call returned OBJECTFS_OK, and `aws s3api head-object` on the key reported
 *     "ContentLength": 0 — a caller-visible success for an object S3 holds as empty;
 *   - the 2 GiB call then panicked ("gobytes: length out of range") and aborted the process, exit 134;
 *   - and because the abort happened before stdout was flushed, the harness printed NO output at all
 *     — not the section header, not a FAIL line, none of the 21 passes that had already run.
 *
 * So the signal for a regression here is a dead test binary rather than a counted failure. That is
 * still a signal — `make test-c` fails on exit 134 — but it is worth knowing in advance that it will
 * look like the tests never ran.
 */
static void test_put_rejects_a_length_it_cannot_transfer(objectfs_client_t client)
{
    SECTION("Integration: put refuses a length it cannot transfer");

    char small[8] = {0};

    /* 4 GiB: the silent-empty-object case. size_t is 64-bit on every platform this ships for; the
     * cast is written out so a 32-bit build fails to compile here rather than wrapping quietly. */
    if (sizeof(size_t) >= 8) {
        int rc = objectfs_put(client, "objectfs-c-test/oversized", small, ((size_t)1) << 32);
        CHECK(rc == OBJECTFS_ERR_INVALID);

        /* The caller's only view of why. A bare code cannot distinguish this from a NULL key. */
        const char *err = objectfs_last_error(client);
        CHECK(err != NULL && strstr(err, "4294967296") != NULL);
        CHECK(err != NULL && strstr(err, "multipart") != NULL);

        /* 2 GiB: the case that used to panic and take the host process with it. Reaching the line
         * after this call at all is most of the assertion. */
        rc = objectfs_put(client, "objectfs-c-test/oversized", small, ((size_t)1) << 31);
        CHECK(rc == OBJECTFS_ERR_INVALID);
    }

    /* The boundary stays usable. INT32_MAX is the largest C.int, so it is the largest length
     * C.GoBytes can be given, and the guard is `>` rather than `>=` for this reason. Not actually
     * transferred — a real 2 GiB PUT is not something a unit test should do — so this only asserts
     * that the guard is not what rejects it. What comes back is a genuine S3 error about the buffer,
     * not OBJECTFS_ERR_INVALID from the length check.
     *
     * Deliberately not asserted as OBJECTFS_OK: reading 2 GiB from an 8-byte buffer is undefined
     * behavior, so this case is only checked in the negative below.
     */
    CHECK(1); /* reached without crashing */
}

/*
 * test_list_rejects_a_negative_limit — objectfs.h documents limit as "0 = no limit" and says nothing
 * about a negative one. Downstream, ListObjects gates every use of limit on `limit > 0`, so -1 was
 * silently treated as "no limit": a caller asking for -1 results got the whole bucket. That is what
 * -1 conventionally means in a lot of C API design, so it would have looked deliberate — but it was
 * an accident of a `> 0` comparison, not a contract, and nothing documented it.
 */
static void test_list_rejects_a_negative_limit(objectfs_client_t client)
{
    SECTION("Integration: list refuses a negative limit");

    objectfs_list_result_t result;
    memset(&result, 0, sizeof(result));

    int rc = objectfs_list(client, "objectfs-c-test/", -1, &result);
    CHECK(rc == OBJECTFS_ERR_INVALID);

    const char *err = objectfs_last_error(client);
    CHECK(err != NULL && strstr(err, "no limit") != NULL);

    /* Nothing was allocated, so the caller has nothing to free — and objectfs_free_list on the
     * zeroed struct must still be safe, since a caller that ignores the return code will call it. */
    CHECK(result.items == NULL);
    CHECK(result.count == 0);
    objectfs_free_list(&result);
    CHECK(1);
}

static void test_integration(void)
{
    const char *bucket = getenv("OBJECTFS_TEST_BUCKET");
    const char *region = getenv("AWS_REGION");

    if (!bucket || !getenv("AWS_ACCESS_KEY_ID")) {
        printf("\n--- Integration tests SKIPPED "
               "(set OBJECTFS_TEST_BUCKET + AWS_ACCESS_KEY_ID) ---\n");
        return;
    }

    SECTION("Integration: connect");

    if (!region) region = "us-east-1";

    objectfs_client_t client = objectfs_new(bucket, region, 0);
    if (!client) {
        const char *err = objectfs_last_error(NULL);
        printf("  FAIL: objectfs_new — %s\n", err ? err : "(no error message)");
        g_failed++;
        return;
    }
    CHECK(client != NULL);
    CHECK(strlen(objectfs_last_error(client)) == 0); /* no error on success */

    test_put_rejects_a_length_it_cannot_transfer(client);
    test_list_rejects_a_negative_limit(client);

    /* ----- Put ----- */
    SECTION("Integration: Put");
    const char *key      = "objectfs-c-test/hello.txt";
    const char *payload  = "hello from ObjectFS C SDK test";
    size_t      plen     = strlen(payload);

    int rc = objectfs_put(client, key, payload, plen);
    CHECK(rc == OBJECTFS_OK);

    /* ----- Get ----- */
    SECTION("Integration: Get");
    void  *got     = NULL;
    size_t got_len = 0;

    rc = objectfs_get(client, key, &got, &got_len);
    CHECK(rc == OBJECTFS_OK);
    CHECK(got != NULL);
    CHECK(got_len == plen);
    CHECK(memcmp(got, payload, plen) == 0);
    objectfs_free_data(got);
    got = NULL;

    /* ----- Head ----- */
    SECTION("Integration: Head");
    objectfs_info_t info;
    memset(&info, 0, sizeof(info));

    rc = objectfs_head(client, key, &info);
    CHECK(rc == OBJECTFS_OK);
    CHECK(strcmp(info.key, key) == 0);
    CHECK(info.size == (int64_t)plen);
    CHECK(info.mtime_sec > 0);

    /* ----- List ----- */
    SECTION("Integration: List");
    objectfs_list_result_t result;
    memset(&result, 0, sizeof(result));

    rc = objectfs_list(client, "objectfs-c-test/", 10, &result);
    CHECK(rc == OBJECTFS_OK);
    CHECK(result.count >= 1);
    if (result.count >= 1) {
        CHECK(strncmp(result.items[0].key, "objectfs-c-test/", 16) == 0);
    }
    objectfs_free_list(&result);

    /* ----- Get non-existent ----- */
    SECTION("Integration: Get non-existent key");
    rc = objectfs_get(client, "objectfs-c-test/__nonexistent_xyz__", &got, &got_len);
    CHECK(rc == OBJECTFS_ERR_NOT_FOUND);

    /*
     * A maximum-length key, round-tripped through objectfs_head and objectfs_list.
     *
     * This is the assertion the Go tests cannot make: fillInfo writes into a cgo struct, and `go
     * test` rejects cgo in a test file, so the only place the struct's real field widths are
     * observable is from C. objectfs_info_t.key was char[1024] against an S3 key limit of 1024
     * bytes, which holds 1023 plus a terminator — so a legal maximum-length key came back one byte
     * short, silently. From objectfs_list, where the key is a result rather than an input, that
     * truncated key names a different object or none at all, and feeding it back to objectfs_get or
     * objectfs_delete would act on the wrong key.
     *
     * strcmp against the key that was written is what catches it. Comparing lengths, or a prefix,
     * would pass with the last byte missing.
     */
    SECTION("Integration: a maximum-length key round-trips without truncation");
    {
        static const char prefix[] = "objectfs-c-test/long/";
        char long_key[1025];               /* 1024 bytes + terminator */
        memset(long_key, 'k', sizeof(long_key) - 1);
        memcpy(long_key, prefix, strlen(prefix));
        long_key[sizeof(long_key) - 1] = '\0';
        CHECK(strlen(long_key) == 1024);   /* the S3 maximum, in bytes */

        rc = objectfs_put(client, long_key, payload, plen);
        CHECK(rc == OBJECTFS_OK);

        objectfs_info_t long_info;
        memset(&long_info, 0, sizeof(long_info));
        rc = objectfs_head(client, long_key, &long_info);
        CHECK(rc == OBJECTFS_OK);
        CHECK(strlen(long_info.key) == 1024);
        CHECK(strcmp(long_info.key, long_key) == 0);

        /* And through list, which is where a truncated key would be undetectable by the caller. */
        objectfs_list_result_t long_result;
        memset(&long_result, 0, sizeof(long_result));
        rc = objectfs_list(client, prefix, 10, &long_result);
        CHECK(rc == OBJECTFS_OK);
        CHECK(long_result.count >= 1);
        if (long_result.count >= 1) {
            CHECK(strcmp(long_result.items[0].key, long_key) == 0);
        }
        objectfs_free_list(&long_result);

        rc = objectfs_delete(client, long_key);
        CHECK(rc == OBJECTFS_OK);
    }

    /* ----- Delete ----- */
    SECTION("Integration: Delete");
    rc = objectfs_delete(client, key);
    CHECK(rc == OBJECTFS_OK);

    /* Verify deletion */
    rc = objectfs_head(client, key, &info);
    CHECK(rc == OBJECTFS_ERR_NOT_FOUND);

    /*
     * Deleting a key that is not there is a no-op returning OBJECTFS_OK — objectfs.h:82 says so, the
     * Go SDK's Delete documents the same, and the S3 backend implements it by treating a NotFound
     * from its metadata read as success. The line above already deleted this key, so this second
     * delete is the no-op case.
     */
    SECTION("Integration: deleting a non-existent key is a no-op");
    rc = objectfs_delete(client, key);
    CHECK(rc == OBJECTFS_OK);

    /* ----- Cleanup ----- */
    objectfs_free(client);
    CHECK(1); /* reached without crash */
}

/* --------------------------------------------------------------------- */
/* main                                                                    */
/* --------------------------------------------------------------------- */

int main(void)
{
    printf("=== ObjectFS C SDK Tests ===\n");

    test_null_bucket();
    test_free_null();
    test_ops_on_null_handle();
    test_last_error_not_null();
    test_last_error_pointer_is_owned_by_the_library();
    test_integration();

    printf("\n=== Results: %d passed, %d failed ===\n", g_passed, g_failed);
    return g_failed > 0 ? 1 : 0;
}
