// Copyright 2025-2026 Scott Friedman. Licensed under the Apache License 2.0.
package com.objectfs.sdk;

/**
 * Options for mounting an ObjectFS filesystem.
 *
 * <pre>{@code
 * MountOptions opts = new MountOptions.Builder()
 *     .readOnly(true)
 *     .cacheSize(256 * 1024 * 1024L)
 *     .build();
 * }</pre>
 */
public class MountOptions {

    private final boolean readOnly;
    private final long cacheSize;
    private final String uid;
    private final String gid;

    private MountOptions(Builder b) {
        this.readOnly  = b.readOnly;
        this.cacheSize = b.cacheSize;
        this.uid       = b.uid;
        this.gid       = b.gid;
    }

    public boolean isReadOnly()  { return readOnly; }
    public long getCacheSize()   { return cacheSize; }
    public String getUid()       { return uid; }
    public String getGid()       { return gid; }

    /** Returns a {@link Builder} with default values. */
    public static Builder builder() { return new Builder(); }

    public static class Builder {
        private boolean readOnly  = false;
        private long cacheSize    = 64 * 1024 * 1024L; // 64 MiB
        private String uid        = null;
        private String gid        = null;

        public Builder readOnly(boolean readOnly)   { this.readOnly = readOnly; return this; }
        public Builder cacheSize(long bytes)        { this.cacheSize = bytes;   return this; }
        public Builder uid(String uid)              { this.uid = uid;           return this; }
        public Builder gid(String gid)              { this.gid = gid;           return this; }

        public MountOptions build() { return new MountOptions(this); }
    }
}
