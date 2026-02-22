"""
ObjectFS Python SDK

High-performance POSIX filesystem for object storage with comprehensive
API support for mounting, configuration, and management.
"""

from .client import ObjectFSClient
from .config import Configuration, StorageConfig, PerformanceConfig, ClusterConfig
from .exceptions import (
    ObjectFSError,
    ConfigurationError,
    MountError,
    StorageError,
    DistributedError
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
    'MountManager',
    'MetricsCollector',
    'HealthChecker',
    'StorageAdapter',
]
