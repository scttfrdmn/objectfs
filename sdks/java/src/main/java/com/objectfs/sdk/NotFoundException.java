// Copyright 2025-2026 Scott Friedman. Licensed under the Apache License 2.0.
package com.objectfs.sdk;

/**
 * Thrown when a requested object key does not exist (HTTP 404).
 */
public class NotFoundException extends ObjectFSException {

    private final String key;

    public NotFoundException(String key) {
        super("object not found: " + key, 404);
        this.key = key;
    }

    public NotFoundException(String key, Throwable cause) {
        super("object not found: " + key, 404, cause);
        this.key = key;
    }

    /** Returns the key that was not found. */
    public String getKey() {
        return key;
    }
}
