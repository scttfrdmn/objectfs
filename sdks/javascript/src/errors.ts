/**
 * ObjectFS Error Classes
 *
 * Custom error hierarchy for ObjectFS JavaScript SDK.
 */

export class ObjectFSError extends Error {
  // `string | undefined` rather than `code?:`. Under `exactOptionalPropertyTypes` an optional
  // property may be absent but may not be *assigned* undefined, and the constructor assigns it
  // unconditionally. Declaring the undefined says what actually happens: the property is always
  // present, and holds undefined when no code was given.
  public readonly code: string | undefined;

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
