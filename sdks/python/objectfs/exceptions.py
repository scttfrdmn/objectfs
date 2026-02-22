"""
ObjectFS Exception Classes

Custom exception hierarchy for ObjectFS Python SDK.
"""


class ObjectFSError(Exception):
    """Base exception for all ObjectFS errors."""

    def __init__(self, message: str, error_code: str = None):
        self.message = message
        self.error_code = error_code
        super().__init__(self.message)


class ConfigurationError(ObjectFSError):
    """Raised when there's a configuration error."""
    pass


class MountError(ObjectFSError):
    """Raised when mount/unmount operations fail."""
    pass


class StorageError(ObjectFSError):
    """Raised when storage operations fail."""
    pass


class DistributedError(ObjectFSError):
    """Raised when distributed operations fail."""
    pass


class CacheError(ObjectFSError):
    """Raised when cache operations fail."""
    pass


class NetworkError(ObjectFSError):
    """Raised when network operations fail."""
    pass


class AuthenticationError(ObjectFSError):
    """Raised when authentication fails."""
    pass


class AuthorizationError(ObjectFSError):
    """Raised when authorization fails."""
    pass


class TimeoutError(ObjectFSError):
    """Raised when operations timeout."""
    pass


class ValidationError(ObjectFSError):
    """Raised when data validation fails."""
    pass
