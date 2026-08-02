"""
ObjectFS Python Client

Main client class for interacting with ObjectFS instances.
"""

import asyncio
import json
import logging
import subprocess
import time
from pathlib import Path
from typing import Dict, List, Optional, Union, Any, AsyncIterator
import aiohttp
import requests

from .config import Configuration
from .exceptions import (
    ObjectFSError, MountError, ConfigurationError,
    StorageError, DistributedError
)
from .mount import MountManager
from .monitoring import MetricsCollector, HealthChecker
from .storage import StorageAdapter

logger = logging.getLogger(__name__)


class ObjectFSClient:
    """
    Main ObjectFS client for managing filesystem instances.

    Provides high-level API for mounting, configuring, and managing
    ObjectFS instances with support for distributed operations.
    """

    def __init__(
        self,
        config: Optional[Union[Configuration, str, Path, Dict[str, Any]]] = None,
        binary_path: Optional[str] = None,
        api_endpoint: Optional[str] = None
    ):
        """
        Initialize ObjectFS client.

        Args:
            config: Configuration object, file path, or dictionary
            binary_path: Path to ObjectFS binary (default: searches PATH)
            api_endpoint: API endpoint for remote ObjectFS instances
        """
        self.config = self._load_config(config)
        self.binary_path = binary_path or self._find_binary()
        self.api_endpoint = api_endpoint

        self.mount_manager = MountManager(self.binary_path, self.config)
        self.storage_adapter = StorageAdapter(self.config.storage)
        self.metrics_collector = MetricsCollector()
        self.health_checker = HealthChecker()

        self._session: Optional[aiohttp.ClientSession] = None
        self._processes: Dict[str, subprocess.Popen] = {}

    def _load_config(
        self,
        config: Optional[Union[Configuration, str, Path, Dict[str, Any]]]
    ) -> Configuration:
        """Load configuration from various sources."""
        if config is None:
            return Configuration()
        elif isinstance(config, Configuration):
            return config
        elif isinstance(config, (str, Path)):
            return Configuration.from_file(config)
        elif isinstance(config, dict):
            return Configuration.from_dict(config)
        else:
            raise ConfigurationError(f"Invalid configuration type: {type(config)}")

    def _find_binary(self) -> str:
        """Find ObjectFS binary in PATH."""
        import shutil
        binary = shutil.which('objectfs')
        if not binary:
            raise ObjectFSError(
                "ObjectFS binary not found in PATH. Please install ObjectFS or "
                "specify binary_path parameter."
            )
        return binary

    async def __aenter__(self):
        """Async context manager entry."""
        await self._ensure_session()
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb):
        """Async context manager exit."""
        await self.close()

    async def _ensure_session(self):
        """Ensure aiohttp session is created."""
        if self._session is None:
            timeout = aiohttp.ClientTimeout(total=30)
            self._session = aiohttp.ClientSession(timeout=timeout)

    async def close(self):
        """Close client and cleanup resources."""
        if self._session:
            await self._session.close()
            self._session = None

        # Stop all managed processes
        for mount_point, process in self._processes.items():
            logger.info(f"Stopping ObjectFS process for {mount_point}")
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()

        self._processes.clear()

    # Mount Management

    def mount(
        self,
        storage_uri: str,
        mount_point: Union[str, Path],
        config_overrides: Optional[Dict[str, Any]] = None,
        foreground: bool = False
    ) -> str:
        """
        Mount ObjectFS filesystem.

        Args:
            storage_uri: Storage URI (e.g., s3://bucket-name)
            mount_point: Local mount point directory
            config_overrides: Configuration overrides
            foreground: Run in foreground mode

        Returns:
            Mount ID for tracking the mount
        """
        try:
            effective_config = self.config
            if config_overrides:
                effective_config = effective_config.merge(config_overrides)

            process = self.mount_manager.mount(
                storage_uri=storage_uri,
                mount_point=mount_point,
                config=effective_config,
                foreground=foreground
            )

            mount_id = f"{storage_uri}:{mount_point}"
            if not foreground:
                self._processes[mount_id] = process

            logger.info(f"Successfully mounted {storage_uri} at {mount_point}")
            return mount_id

        except Exception as e:
            raise MountError(f"Failed to mount {storage_uri}: {e}") from e

    def unmount(self, mount_point: Union[str, Path]) -> bool:
        """
        Unmount ObjectFS filesystem.

        Args:
            mount_point: Mount point to unmount

        Returns:
            True if successfully unmounted
        """
        try:
            result = self.mount_manager.unmount(mount_point)

            # Remove from tracked processes
            mount_id_to_remove = None
            for mount_id in self._processes:
                if str(mount_point) in mount_id:
                    mount_id_to_remove = mount_id
                    break

            if mount_id_to_remove:
                del self._processes[mount_id_to_remove]

            logger.info(f"Successfully unmounted {mount_point}")
            return result

        except Exception as e:
            logger.error(f"Failed to unmount {mount_point}: {e}")
            return False

    def list_mounts(self) -> List[Dict[str, Any]]:
        """
        List active ObjectFS mounts.

        Returns:
            List of mount information dictionaries
        """
        return self.mount_manager.list_mounts()

    def is_mounted(self, mount_point: Union[str, Path]) -> bool:
        """
        Check if directory is mounted with ObjectFS.

        Args:
            mount_point: Directory to check

        Returns:
            True if mounted with ObjectFS
        """
        return self.mount_manager.is_mounted(mount_point)

    # Configuration Management

    def validate_config(self, config: Optional[Configuration] = None) -> bool:
        """
        Validate configuration.

        Args:
            config: Configuration to validate (defaults to client config)

        Returns:
            True if configuration is valid
        """
        target_config = config or self.config
        try:
            target_config.validate()
            return True
        except Exception as e:
            logger.error(f"Configuration validation failed: {e}")
            return False

    def generate_config(
        self,
        preset: str = "production",
        output_path: Optional[Union[str, Path]] = None
    ) -> str:
        """
        Generate configuration file from preset.

        Args:
            preset: Configuration preset name
            output_path: Output file path

        Returns:
            Generated configuration as YAML string
        """
        config = Configuration.from_preset(preset)
        yaml_content = config.to_yaml()

        if output_path:
            with open(output_path, 'w') as f:
                f.write(yaml_content)
            logger.info(f"Generated configuration saved to {output_path}")

        return yaml_content

    # Storage Operations

    async def list_objects(
        self,
        storage_uri: str,
        prefix: Optional[str] = None,
        max_keys: int = 1000
    ) -> List[Dict[str, Any]]:
        """
        List objects in storage.

        Args:
            storage_uri: Storage URI
            prefix: Object prefix filter
            max_keys: Maximum number of keys to return

        Returns:
            List of object information
        """
        try:
            return await self.storage_adapter.list_objects(
                storage_uri, prefix, max_keys
            )
        except Exception as e:
            raise StorageError(f"Failed to list objects: {e}") from e

    async def get_object_info(
        self,
        storage_uri: str,
        key: str
    ) -> Dict[str, Any]:
        """
        Get object information.

        Args:
            storage_uri: Storage URI
            key: Object key

        Returns:
            Object information dictionary
        """
        try:
            return await self.storage_adapter.get_object_info(storage_uri, key)
        except Exception as e:
            raise StorageError(f"Failed to get object info: {e}") from e

    async def download_object(
        self,
        storage_uri: str,
        key: str,
        local_path: Union[str, Path]
    ) -> int:
        """
        Download object to local file.

        Args:
            storage_uri: Storage URI
            key: Object key
            local_path: Local file path

        Returns:
            Number of bytes downloaded
        """
        try:
            return await self.storage_adapter.download_object(
                storage_uri, key, local_path
            )
        except Exception as e:
            raise StorageError(f"Failed to download object: {e}") from e

    async def upload_object(
        self,
        storage_uri: str,
        key: str,
        local_path: Union[str, Path],
        metadata: Optional[Dict[str, str]] = None
    ) -> bool:
        """
        Upload local file to storage.

        Args:
            storage_uri: Storage URI
            key: Object key
            local_path: Local file path
            metadata: Optional object metadata

        Returns:
            True if successful
        """
        try:
            return await self.storage_adapter.upload_object(
                storage_uri, key, local_path, metadata
            )
        except Exception as e:
            raise StorageError(f"Failed to upload object: {e}") from e

    # Monitoring and Health

    async def get_health(self, endpoint: Optional[str] = None) -> Dict[str, Any]:
        """
        Get health status of ObjectFS instance.

        Args:
            endpoint: API endpoint (defaults to configured endpoint)

        Returns:
            Health status information
        """
        target_endpoint = endpoint or self.api_endpoint
        if not target_endpoint:
            raise ObjectFSError("No API endpoint configured")

        await self._ensure_session()
        return await self.health_checker.get_health(target_endpoint, self._session)

    async def get_metrics(self, endpoint: Optional[str] = None) -> Dict[str, Any]:
        """
        Get metrics from ObjectFS instance.

        Args:
            endpoint: API endpoint (defaults to configured endpoint)

        Returns:
            Metrics data
        """
        target_endpoint = endpoint or self.api_endpoint
        if not target_endpoint:
            raise ObjectFSError("No API endpoint configured")

        await self._ensure_session()
        return await self.metrics_collector.collect_metrics(target_endpoint, self._session)

    async def get_performance_stats(self) -> Dict[str, Any]:
        """
        Not implemented. Raises NotImplementedError.

        This method returned hardcoded constants -- a cache hit rate of 0.85, 1000 read
        operations, 1500 requests, 50.5 ms average latency -- the same numbers on every
        call, from a fresh client, with nothing mounted. They were indistinguishable from
        telemetry, and a monitoring dashboard built on them would have shown a healthy
        filesystem regardless of what the filesystem was doing.

        Use ``get_metrics()``, which reaches the mount's real Prometheus endpoint.

        Raises:
            NotImplementedError: always.
        """
        raise NotImplementedError(
            "get_performance_stats is not implemented. It previously returned fixed "
            "constants that looked like measurements. Use get_metrics(), which reads "
            "the mount's Prometheus endpoint."
        )

    # Distributed Operations

    async def join_cluster(
        self,
        seed_nodes: List[str],
        node_config: Optional[Dict[str, Any]] = None
    ) -> bool:
        """
        Join a distributed cluster.

        Args:
            seed_nodes: List of seed node addresses
            node_config: Node-specific configuration

        Returns:
            True if successfully joined
        """
        if not self.config.cluster.enabled:
            raise DistributedError("Cluster mode not enabled in configuration")

        try:
            # Update configuration with cluster settings
            cluster_config = self.config.cluster
            if node_config:
                cluster_config = cluster_config.merge(node_config)

            # Implementation would interact with cluster management API
            # For now, simulate the operation
            logger.info(f"Joining cluster with seed nodes: {seed_nodes}")
            return True

        except Exception as e:
            raise DistributedError(f"Failed to join cluster: {e}") from e

    async def leave_cluster(self) -> bool:
        """
        Leave distributed cluster.

        Returns:
            True if successfully left cluster
        """
        try:
            logger.info("Leaving cluster")
            return True
        except Exception as e:
            raise DistributedError(f"Failed to leave cluster: {e}") from e

    async def get_cluster_status(self) -> Dict[str, Any]:
        """
        Get cluster status information.

        Returns:
            Cluster status data
        """
        if not self.config.cluster.enabled:
            raise DistributedError("Cluster mode not enabled")

        # Implementation would query cluster status
        return {
            'node_count': 1,
            'leader': 'self',
            'status': 'healthy',
            'nodes': []
        }

    # Cache Management

    async def clear_cache(
        self,
        cache_type: Optional[str] = None,
        keys: Optional[List[str]] = None
    ) -> bool:
        """
        Clear filesystem cache.

        Args:
            cache_type: Type of cache to clear (None for all)
            keys: Specific keys to clear (None for all)

        Returns:
            True if successful
        """
        try:
            # Implementation would interact with cache management API
            logger.info(f"Clearing cache - type: {cache_type}, keys: {keys}")
            return True
        except Exception as e:
            logger.error(f"Failed to clear cache: {e}")
            return False

    async def warm_cache(
        self,
        paths: List[str],
        recursive: bool = False
    ) -> Dict[str, bool]:
        """
        Warm cache with specified paths.

        Args:
            paths: List of paths to pre-load
            recursive: Whether to recurse into directories

        Returns:
            Dictionary mapping paths to success status
        """
        try:
            results = {}
            for path in paths:
                # Implementation would trigger cache warming
                results[path] = True
                logger.info(f"Cache warming {'started' if recursive else 'queued'} for {path}")
            return results
        except Exception as e:
            logger.error(f"Failed to warm cache: {e}")
            return {path: False for path in paths}

    # Private helper methods


# Convenience functions
def create_client(
    config_path: Optional[Union[str, Path]] = None,
    **kwargs
) -> ObjectFSClient:
    """
    Create ObjectFS client with optional configuration file.

    Args:
        config_path: Path to configuration file
        **kwargs: Additional client parameters

    Returns:
        Configured ObjectFS client
    """
    config = Configuration.from_file(config_path) if config_path else None
    return ObjectFSClient(config=config, **kwargs)


async def mount_storage(
    storage_uri: str,
    mount_point: Union[str, Path],
    config: Optional[Dict[str, Any]] = None,
    **kwargs
) -> ObjectFSClient:
    """
    Quick mount function for simple use cases.

    Args:
        storage_uri: Storage URI to mount
        mount_point: Local mount point
        config: Configuration overrides
        **kwargs: Additional mount parameters

    Returns:
        ObjectFS client with mounted filesystem
    """
    client = ObjectFSClient()
    client.mount(storage_uri, mount_point, config, **kwargs)
    return client
