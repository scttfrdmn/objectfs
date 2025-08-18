/**
 * ObjectFS Error Classes
 *
 * Custom error hierarchy for ObjectFS JavaScript SDK.
 */

export class ObjectFSError extends Error {
  public readonly code?: string;

  constructor(message: string, code?: string) {
    super(message);
    this.name = this.constructor.name;
    this.code = code;

    // Maintain proper stack trace for where our error was thrown
    if (Error.captureStackTrace) {
      Error.captureStackTrace(this, this.constructor);
    }
  }
}

export class ConfigurationError extends ObjectFSError {
  constructor(message: string, code?: string) {
    super(message, code);
  }
}

export class MountError extends ObjectFSError {
  constructor(message: string, code?: string) {
    super(message, code);
  }
}

export class StorageError extends ObjectFSError {
  constructor(message: string, code?: string) {
    super(message, code);
  }
}

export class DistributedError extends ObjectFSError {
  constructor(message: string, code?: string) {
    super(message, code);
  }
}

export class CacheError extends ObjectFSError {
  constructor(message: string, code?: string) {
    super(message, code);
  }
}

export class NetworkError extends ObjectFSError {
  constructor(message: string, code?: string) {
    super(message, code);
  }
}

export class AuthenticationError extends ObjectFSError {
  constructor(message: string, code?: string) {
    super(message, code);
  }
}

export class AuthorizationError extends ObjectFSError {
  constructor(message: string, code?: string) {
    super(message, code);
  }
}

export class TimeoutError extends ObjectFSError {
  constructor(message: string, code?: string) {
    super(message, code);
  }
}

export class ValidationError extends ObjectFSError {
  constructor(message: string, code?: string) {
    super(message, code);
  }
}
