"""
ObjectFS Python SDK

High-performance POSIX filesystem for object storage with comprehensive
API support for mounting, configuration, and management.
"""

from .client import ObjectFSClient
from .config import Configuration, StorageConfig, PerformanceConfig, ClusterConfig
# Every exception the package defines, not a subset. The README's error-handling example imports
# NetworkError from `objectfs` and could not run: it, CacheError, and the four below were declared
# in .exceptions and re-exported by nothing, so `from objectfs import NetworkError` was an
# ImportError. CacheError is what clear_cache and warm_cache now raise, so a caller needs to be able
# to name it.
from .exceptions import (
    ObjectFSError,
    ConfigurationError,
    MountError,
    StorageError,
    DistributedError,
    CacheError,
    NetworkError,
    AuthenticationError,
    AuthorizationError,
    TimeoutError,
    ValidationError,
)
from .mount import MountManager
from .monitoring import MetricsCollector, HealthChecker
from .storage import StorageAdapter

__version__ = '0.1.0'
__author__ = 'ObjectFS Team'
__email__ = 'team@objectfs.io'
__all__ = [
    'ObjectFSClient',
    'Configuration',
    'StorageConfig',
    'PerformanceConfig',
    'ClusterConfig',
    'ObjectFSError',
    'ConfigurationError',
    'MountError',
    'StorageError',
    'DistributedError',
    'CacheError',
    'NetworkError',
    'AuthenticationError',
    'AuthorizationError',
    'TimeoutError',
    'ValidationError',
    'MountManager',
    'MetricsCollector',
    'HealthChecker',
    'StorageAdapter',
]
