// Copyright 2025-2026 Scott Friedman. Licensed under the Apache License 2.0.
package com.objectfs.sdk;

import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.fasterxml.jackson.datatype.jsr310.JavaTimeModule;
import okhttp3.*;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.io.IOException;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;

/**
 * Client for the ObjectFS coordinator REST API.
 *
 * <p>This client mirrors the Go SDK in {@code sdks/go/objectfs/client.go} and
 * the Python SDK in {@code sdks/python/objectfs/}.  It communicates with the
 * ObjectFS coordinator daemon over HTTP.
 *
 * <pre>{@code
 * ObjectFSConfig config = ObjectFSConfig.builder()
 *     .baseUrl("http://coordinator.example.com:8080")
 *     .apiKey("secret")
 *     .build();
 *
 * try (ObjectFSClient client = new ObjectFSClient(config)) {
 *     client.put("data/sample.txt", "hello world".getBytes());
 *     byte[] data = client.get("data/sample.txt");
 *     System.out.println(new String(data));
 * }
 * }</pre>
 */
public class ObjectFSClient implements AutoCloseable {

    private static final Logger log = LoggerFactory.getLogger(ObjectFSClient.class);
    private static final MediaType BINARY = MediaType.parse("application/octet-stream");

    private final ObjectFSConfig config;
    private final OkHttpClient http;
    private final ObjectMapper json;

    /**
     * Creates a new client using the provided configuration.
     *
     * @param config client configuration (non-null)
     */
    public ObjectFSClient(ObjectFSConfig config) {
        if (config == null) {
            throw new IllegalArgumentException("config must not be null");
        }
        this.config = config;
        long timeoutMs = config.getTimeout().toMillis();
        this.http = new OkHttpClient.Builder()
                .connectTimeout(timeoutMs, TimeUnit.MILLISECONDS)
                .readTimeout(timeoutMs, TimeUnit.MILLISECONDS)
                .writeTimeout(timeoutMs, TimeUnit.MILLISECONDS)
                .build();
        this.json = new ObjectMapper()
                .registerModule(new JavaTimeModule())
                .disable(SerializationFeature.WRITE_DATES_AS_TIMESTAMPS);
    }

    // ── Object operations ────────────────────────────────────────────────────

    /**
     * Retrieves the full content of the object at {@code key}.
     *
     * @param key object key
     * @return object data
     * @throws NotFoundException  if the object does not exist
     * @throws ObjectFSException  on any other error
     */
    public byte[] get(String key) throws ObjectFSException {
        return get(key, -1, -1);
    }

    /**
     * Retrieves a byte range from the object at {@code key}.
     * Pass {@code offset=-1} and {@code length=-1} to fetch the whole object.
     *
     * @param key    object key
     * @param offset byte offset (0-based); use -1 for full object
     * @param length number of bytes; use -1 for remainder
     * @return object data
     * @throws NotFoundException if the object does not exist
     * @throws ObjectFSException on any other error
     */
    public byte[] get(String key, long offset, long length) throws ObjectFSException {
        String url = config.getBaseUrl() + "/api/v1/objects/" + urlEncode(key);
        Request.Builder req = new Request.Builder().url(url).get();
        if (offset >= 0 && length > 0) {
            req.header("Range", "bytes=" + offset + "-" + (offset + length - 1));
        }
        addAuth(req);
        try (Response resp = http.newCall(req.build()).execute()) {
            if (resp.code() == 404) {
                throw new NotFoundException(key);
            }
            assertSuccess(resp, "get " + key);
            ResponseBody body = resp.body();
            return body != null ? body.bytes() : new byte[0];
        } catch (NotFoundException | ObjectFSException e) {
            throw e;
        } catch (IOException e) {
            throw new ObjectFSException("get " + key + ": " + e.getMessage(), e);
        }
    }

    /**
     * Stores {@code data} under {@code key}.
     *
     * @param key  object key
     * @param data object content
     * @throws ObjectFSException on error
     */
    public void put(String key, byte[] data) throws ObjectFSException {
        String url = config.getBaseUrl() + "/api/v1/objects/" + urlEncode(key);
        RequestBody body = RequestBody.create(data, BINARY);
        Request.Builder req = new Request.Builder().url(url).put(body);
        addAuth(req);
        try (Response resp = http.newCall(req.build()).execute()) {
            assertSuccess(resp, "put " + key);
        } catch (ObjectFSException e) {
            throw e;
        } catch (IOException e) {
            throw new ObjectFSException("put " + key + ": " + e.getMessage(), e);
        }
    }

    /**
     * Deletes the object at {@code key}.
     *
     * @param key object key
     * @throws ObjectFSException on error
     */
    public void delete(String key) throws ObjectFSException {
        String url = config.getBaseUrl() + "/api/v1/objects/" + urlEncode(key);
        Request.Builder req = new Request.Builder().url(url).delete();
        addAuth(req);
        try (Response resp = http.newCall(req.build()).execute()) {
            assertSuccess(resp, "delete " + key);
        } catch (ObjectFSException e) {
            throw e;
        } catch (IOException e) {
            throw new ObjectFSException("delete " + key + ": " + e.getMessage(), e);
        }
    }

    /**
     * Lists objects with the given prefix, returning at most {@code limit}
     * results.  Pass {@code limit=0} for all objects.
     *
     * @param prefix object key prefix
     * @param limit  maximum number of results; 0 = unlimited
     * @return list of {@link ObjectInfo}
     * @throws ObjectFSException on error
     */
    public List<ObjectInfo> list(String prefix, int limit) throws ObjectFSException {
        HttpUrl.Builder urlBuilder = HttpUrl.parse(config.getBaseUrl() + "/api/v1/objects").newBuilder();
        if (prefix != null && !prefix.isEmpty()) {
            urlBuilder.addQueryParameter("prefix", prefix);
        }
        if (limit > 0) {
            urlBuilder.addQueryParameter("limit", String.valueOf(limit));
        }
        Request.Builder req = new Request.Builder().url(urlBuilder.build()).get();
        addAuth(req);
        try (Response resp = http.newCall(req.build()).execute()) {
            assertSuccess(resp, "list prefix=" + prefix);
            ResponseBody body = resp.body();
            String bodyStr = body != null ? body.string() : "[]";
            return json.readValue(bodyStr, new TypeReference<List<ObjectInfo>>() {});
        } catch (ObjectFSException e) {
            throw e;
        } catch (IOException e) {
            throw new ObjectFSException("list prefix=" + prefix + ": " + e.getMessage(), e);
        }
    }

    /**
     * Returns metadata for the object at {@code key} without fetching the
     * data.
     *
     * @param key object key
     * @return object metadata
     * @throws NotFoundException if the object does not exist
     * @throws ObjectFSException on any other error
     */
    public ObjectInfo head(String key) throws ObjectFSException {
        String url = config.getBaseUrl() + "/api/v1/objects/" + urlEncode(key) + "/meta";
        Request.Builder req = new Request.Builder().url(url).get();
        addAuth(req);
        try (Response resp = http.newCall(req.build()).execute()) {
            if (resp.code() == 404) {
                throw new NotFoundException(key);
            }
            assertSuccess(resp, "head " + key);
            ResponseBody body = resp.body();
            String bodyStr = body != null ? body.string() : "{}";
            return json.readValue(bodyStr, ObjectInfo.class);
        } catch (NotFoundException | ObjectFSException e) {
            throw e;
        } catch (IOException e) {
            throw new ObjectFSException("head " + key + ": " + e.getMessage(), e);
        }
    }

    // ── Filesystem operations ────────────────────────────────────────────────

    /**
     * Mounts the ObjectFS filesystem at {@code mountPoint}.
     *
     * @param mountPoint local path where the filesystem will be mounted
     * @param opts       mount options (may be null for defaults)
     * @throws ObjectFSException on error
     */
    public void mount(String mountPoint, MountOptions opts) throws ObjectFSException {
        Map<String, Object> body = new HashMap<>();
        body.put("mount_point", mountPoint);
        if (opts != null) {
            body.put("read_only", opts.isReadOnly());
            body.put("cache_size", opts.getCacheSize());
            if (opts.getUid() != null) body.put("uid", opts.getUid());
            if (opts.getGid() != null) body.put("gid", opts.getGid());
        }
        post("/api/v1/mount", body, "mount " + mountPoint);
    }

    /**
     * Unmounts the filesystem at {@code mountPoint}.
     *
     * @param mountPoint local path where the filesystem is mounted
     * @throws ObjectFSException on error
     */
    public void unmount(String mountPoint) throws ObjectFSException {
        Map<String, Object> body = new HashMap<>();
        body.put("mount_point", mountPoint);
        post("/api/v1/unmount", body, "unmount " + mountPoint);
    }

    // ── Health ───────────────────────────────────────────────────────────────

    /**
     * Returns {@code true} if the coordinator reports a healthy status.
     */
    public boolean isHealthy() {
        String url = config.getBaseUrl() + "/healthz";
        Request req = new Request.Builder().url(url).get().build();
        try (Response resp = http.newCall(req).execute()) {
            return resp.isSuccessful();
        } catch (IOException e) {
            log.debug("health check failed: {}", e.getMessage());
            return false;
        }
    }

    // ── AutoCloseable ────────────────────────────────────────────────────────

    @Override
    public void close() {
        http.dispatcher().executorService().shutdown();
        http.connectionPool().evictAll();
    }

    // ── Internal helpers ─────────────────────────────────────────────────────

    private void addAuth(Request.Builder req) {
        if (config.getApiKey() != null && !config.getApiKey().isBlank()) {
            req.header("X-ObjectFS-API-Key", config.getApiKey());
        }
    }

    private void assertSuccess(Response resp, String operation) throws ObjectFSException {
        if (!resp.isSuccessful()) {
            String detail = "";
            try {
                ResponseBody b = resp.body();
                if (b != null) detail = ": " + b.string();
            } catch (IOException ignored) {}
            throw new ObjectFSException(operation + " failed (HTTP " + resp.code() + ")" + detail, resp.code());
        }
    }

    private void post(String path, Object payload, String operation) throws ObjectFSException {
        String url = config.getBaseUrl() + path;
        String bodyStr;
        try {
            bodyStr = json.writeValueAsString(payload);
        } catch (IOException e) {
            throw new ObjectFSException(operation + ": failed to serialize request: " + e.getMessage(), e);
        }
        RequestBody body = RequestBody.create(bodyStr, MediaType.parse("application/json"));
        Request.Builder req = new Request.Builder().url(url).post(body);
        addAuth(req);
        try (Response resp = http.newCall(req.build()).execute()) {
            assertSuccess(resp, operation);
        } catch (ObjectFSException e) {
            throw e;
        } catch (IOException e) {
            throw new ObjectFSException(operation + ": " + e.getMessage(), e);
        }
    }

    /** URL-encodes a path segment, preserving forward slashes. */
    private static String urlEncode(String key) {
        return key.replace(" ", "%20");
    }
}
