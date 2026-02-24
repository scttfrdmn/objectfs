// Copyright 2025-2026 Scott Friedman. Licensed under the Apache License 2.0.
package com.objectfs.sdk;

import java.time.Duration;

/**
 * Configuration for {@link ObjectFSClient}.
 *
 * <pre>{@code
 * ObjectFSConfig config = new ObjectFSConfig.Builder()
 *     .baseUrl("http://localhost:8080")
 *     .apiKey("secret")
 *     .timeout(Duration.ofSeconds(30))
 *     .build();
 * }</pre>
 */
public class ObjectFSConfig {

    private final String baseUrl;
    private final String apiKey;
    private final Duration timeout;
    private final int maxRetries;

    private ObjectFSConfig(Builder b) {
        this.baseUrl    = b.baseUrl;
        this.apiKey     = b.apiKey;
        this.timeout    = b.timeout;
        this.maxRetries = b.maxRetries;
    }

    public String getBaseUrl()      { return baseUrl; }
    public String getApiKey()       { return apiKey; }
    public Duration getTimeout()    { return timeout; }
    public int getMaxRetries()      { return maxRetries; }

    /** Returns a {@link Builder} with sensible defaults. */
    public static Builder builder() { return new Builder(); }

    public static class Builder {
        private String baseUrl    = "http://localhost:8080";
        private String apiKey     = null;
        private Duration timeout  = Duration.ofSeconds(30);
        private int maxRetries    = 3;

        public Builder baseUrl(String baseUrl)      { this.baseUrl = baseUrl;       return this; }
        public Builder apiKey(String apiKey)        { this.apiKey = apiKey;         return this; }
        public Builder timeout(Duration timeout)    { this.timeout = timeout;       return this; }
        public Builder maxRetries(int maxRetries)   { this.maxRetries = maxRetries; return this; }

        public ObjectFSConfig build() {
            if (baseUrl == null || baseUrl.isBlank()) {
                throw new IllegalArgumentException("baseUrl must not be blank");
            }
            return new ObjectFSConfig(this);
        }
    }
}
