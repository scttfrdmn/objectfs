"""
ObjectFS Storage Adapter

Storage backend abstraction for different cloud providers.
"""

import asyncio
import json
import logging
import os
from pathlib import Path
from typing import Dict, List, Optional, Union, Any, AsyncIterator
from urllib.parse import urlparse

import aiohttp
import requests

from .config import StorageConfig
from .exceptions import StorageError, ConfigurationError

logger = logging.getLogger(__name__)


class StorageAdapter:
    """
    Storage adapter for various cloud storage backends.

    Provides unified interface for interacting with different
    storage providers (S3, GCS, Azure Blob, etc.).
    """

    def __init__(self, config: StorageConfig):
        """
        Initialize storage adapter.

        Args:
            config: Storage configuration
        """
        self.config = config
        self._clients = {}

    async def list_objects(
        self,
        storage_uri: str,
        prefix: Optional[str] = None,
        max_keys: int = 1000,
        continuation_token: Optional[str] = None
    ) -> Dict[str, Any]:
        """
        List objects in storage backend.

        Args:
            storage_uri: Storage URI (e.g., s3://bucket/path)
            prefix: Object prefix filter
            max_keys: Maximum number of objects to return
            continuation_token: Token for paginated results

        Returns:
            Dictionary containing objects list and metadata

        Raises:
            StorageError: If listing fails
        """
        try:
            parsed_uri = self._parse_storage_uri(storage_uri)
            backend = parsed_uri['scheme']

            if backend == 's3':
                return await self._list_s3_objects(
                    parsed_uri, prefix, max_keys, continuation_token
                )
            elif backend == 'gs':
                return await self._list_gcs_objects(
                    parsed_uri, prefix, max_keys, continuation_token
                )
            elif backend == 'az':
                return await self._list_azure_objects(
                    parsed_uri, prefix, max_keys, continuation_token
                )
            else:
                raise StorageError(f"Unsupported storage backend: {backend}")

        except Exception as e:
            raise StorageError(f"Failed to list objects: {e}") from e

    async def get_object_info(
        self,
        storage_uri: str,
        key: str
    ) -> Dict[str, Any]:
        """
        Get metadata information for a specific object.

        Args:
            storage_uri: Storage URI
            key: Object key/path

        Returns:
            Object metadata dictionary

        Raises:
            StorageError: If operation fails
        """
        try:
            parsed_uri = self._parse_storage_uri(storage_uri)
            backend = parsed_uri['scheme']

            if backend == 's3':
                return await self._get_s3_object_info(parsed_uri, key)
            elif backend == 'gs':
                return await self._get_gcs_object_info(parsed_uri, key)
            elif backend == 'az':
                return await self._get_azure_object_info(parsed_uri, key)
            else:
                raise StorageError(f"Unsupported storage backend: {backend}")

        except Exception as e:
            raise StorageError(f"Failed to get object info: {e}") from e

    async def download_object(
        self,
        storage_uri: str,
        key: str,
        local_path: Union[str, Path],
        progress_callback: Optional[callable] = None
    ) -> int:
        """
        Download object from storage to local file.

        Args:
            storage_uri: Storage URI
            key: Object key/path
            local_path: Local destination file path
            progress_callback: Optional progress callback function

        Returns:
            Number of bytes downloaded

        Raises:
            StorageError: If download fails
        """
        try:
            parsed_uri = self._parse_storage_uri(storage_uri)
            backend = parsed_uri['scheme']
            local_path = Path(local_path)

            # Ensure parent directory exists
            local_path.parent.mkdir(parents=True, exist_ok=True)

            if backend == 's3':
                return await self._download_s3_object(
                    parsed_uri, key, local_path, progress_callback
                )
            elif backend == 'gs':
                return await self._download_gcs_object(
                    parsed_uri, key, local_path, progress_callback
                )
            elif backend == 'az':
                return await self._download_azure_object(
                    parsed_uri, key, local_path, progress_callback
                )
            else:
                raise StorageError(f"Unsupported storage backend: {backend}")

        except Exception as e:
            raise StorageError(f"Failed to download object: {e}") from e

    async def upload_object(
        self,
        storage_uri: str,
        key: str,
        local_path: Union[str, Path],
        metadata: Optional[Dict[str, str]] = None,
        content_type: Optional[str] = None,
        progress_callback: Optional[callable] = None
    ) -> bool:
        """
        Upload local file to storage backend.

        Args:
            storage_uri: Storage URI
            key: Object key/path
            local_path: Local source file path
            metadata: Optional object metadata
            content_type: Optional content type
            progress_callback: Optional progress callback function

        Returns:
            True if upload successful

        Raises:
            StorageError: If upload fails
        """
        try:
            parsed_uri = self._parse_storage_uri(storage_uri)
            backend = parsed_uri['scheme']
            local_path = Path(local_path)

            if not local_path.exists():
                raise StorageError(f"Local file does not exist: {local_path}")

            if backend == 's3':
                return await self._upload_s3_object(
                    parsed_uri, key, local_path, metadata, content_type, progress_callback
                )
            elif backend == 'gs':
                return await self._upload_gcs_object(
                    parsed_uri, key, local_path, metadata, content_type, progress_callback
                )
            elif backend == 'az':
                return await self._upload_azure_object(
                    parsed_uri, key, local_path, metadata, content_type, progress_callback
                )
            else:
                raise StorageError(f"Unsupported storage backend: {backend}")

        except Exception as e:
            raise StorageError(f"Failed to upload object: {e}") from e

    async def delete_object(
        self,
        storage_uri: str,
        key: str
    ) -> bool:
        """
        Delete object from storage backend.

        Args:
            storage_uri: Storage URI
            key: Object key/path to delete

        Returns:
            True if deletion successful

        Raises:
            StorageError: If deletion fails
        """
        try:
            parsed_uri = self._parse_storage_uri(storage_uri)
            backend = parsed_uri['scheme']

            if backend == 's3':
                return await self._delete_s3_object(parsed_uri, key)
            elif backend == 'gs':
                return await self._delete_gcs_object(parsed_uri, key)
            elif backend == 'az':
                return await self._delete_azure_object(parsed_uri, key)
            else:
                raise StorageError(f"Unsupported storage backend: {backend}")

        except Exception as e:
            raise StorageError(f"Failed to delete object: {e}") from e

    def _parse_storage_uri(self, storage_uri: str) -> Dict[str, str]:
        """Parse storage URI into components."""
        parsed = urlparse(storage_uri)

        if not parsed.scheme:
            raise StorageError(f"Invalid storage URI (missing scheme): {storage_uri}")

        return {
            'scheme': parsed.scheme,
            'bucket': parsed.netloc,
            'path': parsed.path.lstrip('/'),
            'full_uri': storage_uri
        }

    # S3-specific methods

    async def _list_s3_objects(
        self,
        parsed_uri: Dict[str, str],
        prefix: Optional[str],
        max_keys: int,
        continuation_token: Optional[str]
    ) -> Dict[str, Any]:
        """List objects in S3 bucket."""
        # Simulate S3 API response
        # In production, would use boto3 or similar

        objects = [
            {
                'Key': f"{prefix or ''}test-file-1.txt",
                'Size': 1024,
                'LastModified': '2024-01-01T00:00:00Z',
                'ETag': '"abc123"',
                'StorageClass': 'STANDARD'
            },
            {
                'Key': f"{prefix or ''}test-file-2.txt",
                'Size': 2048,
                'LastModified': '2024-01-01T01:00:00Z',
                'ETag': '"def456"',
                'StorageClass': 'STANDARD'
            }
        ]

        return {
            'objects': objects[:max_keys],
            'truncated': len(objects) > max_keys,
            'next_continuation_token': 'next-token' if len(objects) > max_keys else None,
            'total_count': len(objects)
        }

    async def _get_s3_object_info(
        self,
        parsed_uri: Dict[str, str],
        key: str
    ) -> Dict[str, Any]:
        """Get S3 object metadata."""
        return {
            'key': key,
            'size': 1024,
            'last_modified': '2024-01-01T00:00:00Z',
            'etag': '"abc123"',
            'content_type': 'text/plain',
            'storage_class': 'STANDARD',
            'metadata': {}
        }

    async def _download_s3_object(
        self,
        parsed_uri: Dict[str, str],
        key: str,
        local_path: Path,
        progress_callback: Optional[callable]
    ) -> int:
        """Download object from S3."""
        # Simulate download
        content = b"Simulated file content from S3"

        with open(local_path, 'wb') as f:
            f.write(content)

        if progress_callback:
            progress_callback(len(content), len(content))

        return len(content)

    async def _upload_s3_object(
        self,
        parsed_uri: Dict[str, str],
        key: str,
        local_path: Path,
        metadata: Optional[Dict[str, str]],
        content_type: Optional[str],
        progress_callback: Optional[callable]
    ) -> bool:
        """Upload object to S3."""
        file_size = local_path.stat().st_size

        # Simulate upload progress
        if progress_callback:
            progress_callback(file_size, file_size)

        logger.info(f"Simulated upload of {local_path} to s3://{parsed_uri['bucket']}/{key}")
        return True

    async def _delete_s3_object(
        self,
        parsed_uri: Dict[str, str],
        key: str
    ) -> bool:
        """Delete object from S3."""
        logger.info(f"Simulated deletion of s3://{parsed_uri['bucket']}/{key}")
        return True

    # GCS-specific methods (simplified implementations)

    async def _list_gcs_objects(self, parsed_uri, prefix, max_keys, continuation_token):
        """List objects in GCS bucket."""
        return await self._list_s3_objects(parsed_uri, prefix, max_keys, continuation_token)

    async def _get_gcs_object_info(self, parsed_uri, key):
        """Get GCS object metadata."""
        return await self._get_s3_object_info(parsed_uri, key)

    async def _download_gcs_object(self, parsed_uri, key, local_path, progress_callback):
        """Download object from GCS."""
        return await self._download_s3_object(parsed_uri, key, local_path, progress_callback)

    async def _upload_gcs_object(self, parsed_uri, key, local_path, metadata, content_type, progress_callback):
        """Upload object to GCS."""
        return await self._upload_s3_object(parsed_uri, key, local_path, metadata, content_type, progress_callback)

    async def _delete_gcs_object(self, parsed_uri, key):
        """Delete object from GCS."""
        return await self._delete_s3_object(parsed_uri, key)

    # Azure-specific methods (simplified implementations)

    async def _list_azure_objects(self, parsed_uri, prefix, max_keys, continuation_token):
        """List objects in Azure container."""
        return await self._list_s3_objects(parsed_uri, prefix, max_keys, continuation_token)

    async def _get_azure_object_info(self, parsed_uri, key):
        """Get Azure blob metadata."""
        return await self._get_s3_object_info(parsed_uri, key)

    async def _download_azure_object(self, parsed_uri, key, local_path, progress_callback):
        """Download blob from Azure."""
        return await self._download_s3_object(parsed_uri, key, local_path, progress_callback)

    async def _upload_azure_object(self, parsed_uri, key, local_path, metadata, content_type, progress_callback):
        """Upload blob to Azure."""
        return await self._upload_s3_object(parsed_uri, key, local_path, metadata, content_type, progress_callback)

    async def _delete_azure_object(self, parsed_uri, key):
        """Delete blob from Azure."""
        return await self._delete_s3_object(parsed_uri, key)
