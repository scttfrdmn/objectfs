"""
ObjectFS Storage Adapter

Storage backend abstraction for different cloud providers.
"""

# Nothing here performs I/O, so nothing here imports a transport. `aiohttp`, `requests`, `asyncio`,
# `json`, `os` and `logging` were all imported and all unused even before the fabricated methods were
# removed -- the fabrications logged and returned literals; they never opened a socket. Keeping the
# two HTTP clients in the import list of the module named `storage` implied otherwise.
from pathlib import Path
from typing import Dict, Optional, Union, Any
from urllib.parse import urlparse

from .config import StorageConfig
from .exceptions import StorageError

_NOT_IMPLEMENTED = (
    "is not implemented. The ObjectFS Python SDK has no S3, GCS or Azure client: this "
    "method previously returned fabricated data (and download_object overwrote the local "
    "path with placeholder content). See "
    "https://github.com/scttfrdmn/objectfs/issues/325. Use boto3 directly, or mount the "
    "bucket with ObjectFS and use ordinary filesystem calls."
)


def _not_implemented(method: str) -> 'StorageError':
    """Build the error every fabricated operation now raises.

    Returned rather than raised so call sites read ``raise _not_implemented(...)``, which keeps
    the traceback rooted at the method the caller actually invoked.
    """
    return StorageError(f"StorageAdapter.{method} {_NOT_IMPLEMENTED}")


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

        except StorageError:
            # Re-raised unchanged. The generic handler below wraps every exception in a new
            # StorageError, which for a StorageError means the specific message -- an unsupported
            # scheme, a malformed URI, or the not-implemented notice naming issue #325 -- arrives
            # nested inside a vaguer one. The specific message is the useful one.
            raise
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

        except StorageError:
            raise  # Already specific; see the note on _parse_storage_uri.
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

            # Ensure parent directory exists. Note this still runs even though the backend method
            # below raises, so a missing parent directory is created for a download that cannot
            # happen; that is a stray empty directory, not lost data, and the line is where it
            # belongs for when the download is real. What matters is that nothing opens
            # ``local_path`` itself, so an existing file there is left byte-for-byte intact.
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

        except StorageError:
            raise  # Already specific; see the note on _parse_storage_uri.
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

        except StorageError:
            raise  # Already specific; see the note on _parse_storage_uri.
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

        except StorageError:
            raise  # Already specific; see the note on _parse_storage_uri.
        except Exception as e:
            raise StorageError(f"Failed to delete object: {e}") from e

    def _parse_storage_uri(self, storage_uri: str) -> Dict[str, str]:
        """Parse storage URI into components.

        Every public method re-raises ``StorageError`` unchanged before its generic
        ``except Exception`` handler. Without that, the handler wraps the specific message this
        function raises -- and the unsupported-scheme and not-implemented messages too -- inside a
        vaguer "Failed to <verb>", so the useful text arrives nested one level down.
        """
        parsed = urlparse(storage_uri)

        if not parsed.scheme:
            raise StorageError(f"Invalid storage URI (missing scheme): {storage_uri}")

        return {
            'scheme': parsed.scheme,
            'bucket': parsed.netloc,
            'path': parsed.path.lstrip('/'),
            'full_uri': storage_uri
        }

    # Backend methods.
    #
    # There is one set, not three. The GCS and Azure methods that used to be here all delegated to
    # their S3 counterpart, so `gs://` and `az://` URIs returned the same two invented S3 objects
    # under a docstring claiming a "simplified implementation" of a different cloud. Nothing was
    # simplified; nothing was implemented. The dispatch in the public methods above still names the
    # three schemes, because rejecting an unsupported scheme is a real thing to do, and it now
    # arrives at a single honest raise instead of three copies of a fabrication.

    async def _list_s3_objects(
        self,
        parsed_uri: Dict[str, str],
        prefix: Optional[str],
        max_keys: int,
        continuation_token: Optional[str]
    ) -> Dict[str, Any]:
        """Not implemented; raises.

        Returned two invented objects -- ``test-file-1.txt`` at 1024 bytes and
        ``test-file-2.txt`` at 2048 -- keyed under the caller's own prefix, so the result looked
        derived from the request. ``total_count`` was 2 for every bucket in the world, including
        empty ones and ones that do not exist.
        """
        raise _not_implemented('list_objects')

    async def _get_s3_object_info(
        self,
        parsed_uri: Dict[str, str],
        key: str
    ) -> Dict[str, Any]:
        """Not implemented; raises.

        Returned ``size: 1024``, ``etag: '"abc123"'`` and ``content_type: 'text/plain'`` for any
        key, whether or not it existed -- so it could not report a missing object, which is most
        of what a caller asks this for.
        """
        raise _not_implemented('get_object_info')

    async def _download_s3_object(
        self,
        parsed_uri: Dict[str, str],
        key: str,
        local_path: Path,
        progress_callback: Optional[callable]
    ) -> int:
        """Not implemented; raises.

        This is the one that destroyed data. It did
        ``open(local_path, 'wb').write(b"Simulated file content from S3")``, called
        ``progress_callback(30, 30)``, and returned 30 -- so a caller following the README's own
        example lost whatever file was at that path and was told the transfer succeeded, with a
        completed progress bar. Verified by execution against a file containing other content.

        The raise happens before anything is opened, so an existing file at ``local_path`` is left
        exactly as it was.
        """
        raise _not_implemented('download_object')

    async def _upload_s3_object(
        self,
        parsed_uri: Dict[str, str],
        key: str,
        local_path: Path,
        metadata: Optional[Dict[str, str]],
        content_type: Optional[str],
        progress_callback: Optional[callable]
    ) -> bool:
        """Not implemented; raises.

        Called ``progress_callback(file_size, file_size)``, logged "Simulated upload", and returned
        True. The progress callback completing is what makes this worse than returning None: a
        caller with a progress bar watched it fill.
        """
        raise _not_implemented('upload_object')

    async def _delete_s3_object(
        self,
        parsed_uri: Dict[str, str],
        key: str
    ) -> bool:
        """Not implemented; raises.

        Logged "Simulated deletion" and returned True while the object remained in S3. A caller
        deleting on a retention or privacy obligation was told it was done.
        """
        raise _not_implemented('delete_object')

    # GCS and Azure use the same raises: see the note above.

    _list_gcs_objects = _list_azure_objects = _list_s3_objects
    _get_gcs_object_info = _get_azure_object_info = _get_s3_object_info
    _download_gcs_object = _download_azure_object = _download_s3_object
    _upload_gcs_object = _upload_azure_object = _upload_s3_object
    _delete_gcs_object = _delete_azure_object = _delete_s3_object
