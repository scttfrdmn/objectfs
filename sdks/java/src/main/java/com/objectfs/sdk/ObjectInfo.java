// Copyright 2025-2026 Scott Friedman. Licensed under the Apache License 2.0.
package com.objectfs.sdk;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.time.Instant;

/**
 * Metadata for a single object stored in ObjectFS.
 */
@JsonIgnoreProperties(ignoreUnknown = true)
public class ObjectInfo {

    @JsonProperty("key")
    private String key;

    @JsonProperty("size")
    private long size;

    @JsonProperty("etag")
    private String etag;

    @JsonProperty("last_modified")
    private Instant lastModified;

    @JsonProperty("content_type")
    private String contentType;

    public ObjectInfo() {}

    public ObjectInfo(String key, long size, String etag, Instant lastModified, String contentType) {
        this.key = key;
        this.size = size;
        this.etag = etag;
        this.lastModified = lastModified;
        this.contentType = contentType;
    }

    public String getKey()              { return key; }
    public long getSize()               { return size; }
    public String getEtag()             { return etag; }
    public Instant getLastModified()    { return lastModified; }
    public String getContentType()      { return contentType; }

    public void setKey(String key)                      { this.key = key; }
    public void setSize(long size)                      { this.size = size; }
    public void setEtag(String etag)                    { this.etag = etag; }
    public void setLastModified(Instant lastModified)   { this.lastModified = lastModified; }
    public void setContentType(String contentType)      { this.contentType = contentType; }

    @Override
    public String toString() {
        return "ObjectInfo{key='" + key + "', size=" + size + ", etag='" + etag + "'}";
    }
}
