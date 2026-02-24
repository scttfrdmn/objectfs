// Copyright 2025-2026 Scott Friedman. Licensed under the Apache License 2.0.
package com.objectfs.sdk;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.fasterxml.jackson.datatype.jsr310.JavaTimeModule;
import okhttp3.mockwebserver.MockResponse;
import okhttp3.mockwebserver.MockWebServer;
import okhttp3.mockwebserver.RecordedRequest;
import org.junit.After;
import org.junit.Before;
import org.junit.Test;

import java.io.IOException;
import java.time.Instant;
import java.util.Arrays;
import java.util.List;

import static org.junit.Assert.*;

/**
 * Unit tests for {@link ObjectFSClient} using {@link MockWebServer}.
 */
public class ObjectFSClientTest {

    private MockWebServer server;
    private ObjectFSClient client;
    private ObjectMapper json;

    @Before
    public void setUp() throws IOException {
        server = new MockWebServer();
        server.start();
        json = new ObjectMapper()
                .registerModule(new JavaTimeModule())
                .disable(SerializationFeature.WRITE_DATES_AS_TIMESTAMPS);

        ObjectFSConfig config = ObjectFSConfig.builder()
                .baseUrl(server.url("/").toString().replaceAll("/$", ""))
                .apiKey("test-key")
                .build();
        client = new ObjectFSClient(config);
    }

    @After
    public void tearDown() throws IOException {
        client.close();
        server.shutdown();
    }

    // ── get ──────────────────────────────────────────────────────────────────

    @Test
    public void get_returnsData_onSuccess() throws Exception {
        server.enqueue(new MockResponse().setBody("hello").setResponseCode(200));

        byte[] data = client.get("docs/hello.txt");

        assertArrayEquals("hello".getBytes(), data);
        RecordedRequest req = server.takeRequest();
        assertEquals("GET", req.getMethod());
        assertTrue(req.getPath().contains("docs/hello.txt"));
        assertEquals("test-key", req.getHeader("X-ObjectFS-API-Key"));
    }

    @Test(expected = NotFoundException.class)
    public void get_throwsNotFoundException_on404() throws Exception {
        server.enqueue(new MockResponse().setResponseCode(404).setBody("not found"));
        client.get("missing/key");
    }

    @Test(expected = ObjectFSException.class)
    public void get_throwsObjectFSException_on500() throws Exception {
        server.enqueue(new MockResponse().setResponseCode(500).setBody("internal server error"));
        client.get("bad/key");
    }

    @Test
    public void get_withRange_sendsRangeHeader() throws Exception {
        server.enqueue(new MockResponse().setBody("ell").setResponseCode(206));

        byte[] data = client.get("file.txt", 1, 3);

        assertArrayEquals("ell".getBytes(), data);
        RecordedRequest req = server.takeRequest();
        assertEquals("bytes=1-3", req.getHeader("Range"));
    }

    // ── put ──────────────────────────────────────────────────────────────────

    @Test
    public void put_sendsBody_onSuccess() throws Exception {
        server.enqueue(new MockResponse().setResponseCode(200));

        client.put("data/value.bin", new byte[]{1, 2, 3});

        RecordedRequest req = server.takeRequest();
        assertEquals("PUT", req.getMethod());
        assertArrayEquals(new byte[]{1, 2, 3}, req.getBody().readByteArray());
    }

    @Test(expected = ObjectFSException.class)
    public void put_throwsOnError() throws Exception {
        server.enqueue(new MockResponse().setResponseCode(503));
        client.put("key", new byte[0]);
    }

    // ── delete ───────────────────────────────────────────────────────────────

    @Test
    public void delete_sendsDeleteRequest() throws Exception {
        server.enqueue(new MockResponse().setResponseCode(204));

        client.delete("remove/me");

        RecordedRequest req = server.takeRequest();
        assertEquals("DELETE", req.getMethod());
        assertTrue(req.getPath().contains("remove/me"));
    }

    // ── list ─────────────────────────────────────────────────────────────────

    @Test
    public void list_returnsObjectInfoList() throws Exception {
        List<ObjectInfo> objects = Arrays.asList(
                new ObjectInfo("prefix/a", 100, "etag1", Instant.parse("2026-01-01T00:00:00Z"), "text/plain"),
                new ObjectInfo("prefix/b", 200, "etag2", Instant.parse("2026-01-02T00:00:00Z"), "application/json")
        );
        server.enqueue(new MockResponse()
                .setBody(json.writeValueAsString(objects))
                .setResponseCode(200)
                .addHeader("Content-Type", "application/json"));

        List<ObjectInfo> result = client.list("prefix/", 10);

        assertEquals(2, result.size());
        assertEquals("prefix/a", result.get(0).getKey());
        assertEquals(100L, result.get(0).getSize());
    }

    @Test
    public void list_includesPrefixAndLimitParams() throws Exception {
        server.enqueue(new MockResponse().setBody("[]").setResponseCode(200));

        client.list("my/prefix", 5);

        RecordedRequest req = server.takeRequest();
        assertTrue(req.getPath().contains("prefix=my/prefix"));
        assertTrue(req.getPath().contains("limit=5"));
    }

    // ── head ─────────────────────────────────────────────────────────────────

    @Test
    public void head_returnsObjectInfo() throws Exception {
        ObjectInfo info = new ObjectInfo("mykey", 512, "abc", Instant.parse("2026-01-15T12:00:00Z"), "text/plain");
        server.enqueue(new MockResponse()
                .setBody(json.writeValueAsString(info))
                .setResponseCode(200)
                .addHeader("Content-Type", "application/json"));

        ObjectInfo result = client.head("mykey");

        assertNotNull(result);
        assertEquals("mykey", result.getKey());
        assertEquals(512L, result.getSize());
    }

    @Test(expected = NotFoundException.class)
    public void head_throwsNotFoundException_on404() throws Exception {
        server.enqueue(new MockResponse().setResponseCode(404));
        client.head("gone");
    }

    // ── mount / unmount ──────────────────────────────────────────────────────

    @Test
    public void mount_postsToMountEndpoint() throws Exception {
        server.enqueue(new MockResponse().setResponseCode(200));

        client.mount("/mnt/s3", MountOptions.builder().readOnly(true).build());

        RecordedRequest req = server.takeRequest();
        assertEquals("POST", req.getMethod());
        assertTrue(req.getPath().equals("/api/v1/mount"));
        String body = req.getBody().readUtf8();
        assertTrue(body.contains("mount_point"));
        assertTrue(body.contains("/mnt/s3"));
    }

    @Test
    public void unmount_postsToUnmountEndpoint() throws Exception {
        server.enqueue(new MockResponse().setResponseCode(200));

        client.unmount("/mnt/s3");

        RecordedRequest req = server.takeRequest();
        assertEquals("POST", req.getMethod());
        assertTrue(req.getPath().equals("/api/v1/unmount"));
    }

    // ── isHealthy ────────────────────────────────────────────────────────────

    @Test
    public void isHealthy_returnsTrue_on200() throws Exception {
        server.enqueue(new MockResponse().setBody("OK").setResponseCode(200));
        assertTrue(client.isHealthy());
    }

    @Test
    public void isHealthy_returnsFalse_on503() throws Exception {
        server.enqueue(new MockResponse().setBody("DEGRADED").setResponseCode(503));
        assertFalse(client.isHealthy());
    }

    // ── config validation ────────────────────────────────────────────────────

    @Test(expected = IllegalArgumentException.class)
    public void config_rejectsBlankBaseUrl() {
        ObjectFSConfig.builder().baseUrl("").build();
    }

    @Test(expected = IllegalArgumentException.class)
    public void client_rejectsNullConfig() {
        new ObjectFSClient(null);
    }
}
