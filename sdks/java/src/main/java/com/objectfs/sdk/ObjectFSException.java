// Copyright 2025-2026 Scott Friedman. Licensed under the Apache License 2.0.
package com.objectfs.sdk;

/**
 * Base unchecked exception for all ObjectFS SDK errors.
 */
public class ObjectFSException extends RuntimeException {

    private final int statusCode;

    public ObjectFSException(String message) {
        super(message);
        this.statusCode = -1;
    }

    public ObjectFSException(String message, int statusCode) {
        super(message);
        this.statusCode = statusCode;
    }

    public ObjectFSException(String message, Throwable cause) {
        super(message, cause);
        this.statusCode = -1;
    }

    public ObjectFSException(String message, int statusCode, Throwable cause) {
        super(message, cause);
        this.statusCode = statusCode;
    }

    /**
     * Returns the HTTP status code associated with this error, or -1 if not
     * available.
     */
    public int getStatusCode() {
        return statusCode;
    }
}
