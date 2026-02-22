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

/* --------------------------------------------------------------------- */
/* Integration tests — skipped unless OBJECTFS_TEST_BUCKET is set         */
/* --------------------------------------------------------------------- */

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

    /* ----- Delete ----- */
    SECTION("Integration: Delete");
    rc = objectfs_delete(client, key);
    CHECK(rc == OBJECTFS_OK);

    /* Verify deletion */
    rc = objectfs_head(client, key, &info);
    CHECK(rc == OBJECTFS_ERR_NOT_FOUND);

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
    test_integration();

    printf("\n=== Results: %d passed, %d failed ===\n", g_passed, g_failed);
    return g_failed > 0 ? 1 : 0;
}
