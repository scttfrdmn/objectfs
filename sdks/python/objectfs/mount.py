"""
ObjectFS Mount Manager

Handles mounting and unmounting of ObjectFS filesystems.
"""

import logging
import os
import psutil
import subprocess
import tempfile
import time
from pathlib import Path
from typing import Dict, List, Optional, Union, Any

from .config import Configuration
from .exceptions import MountError, ConfigurationError

logger = logging.getLogger(__name__)


class MountManager:
    """
    Manages ObjectFS filesystem mounts.

    Provides functionality to mount, unmount, and monitor ObjectFS
    filesystem instances with comprehensive error handling and validation.
    """

    def __init__(self, binary_path: str, config: Configuration):
        """
        Initialize mount manager.

        Args:
            binary_path: Path to ObjectFS binary
            config: ObjectFS configuration
        """
        self.binary_path = binary_path
        self.config = config

    def mount(
        self,
        storage_uri: str,
        mount_point: Union[str, Path],
        config: Optional[Configuration] = None,
        foreground: bool = False,
        timeout: int = 30
    ) -> subprocess.Popen:
        """
        Mount ObjectFS filesystem.

        Args:
            storage_uri: Storage URI (e.g., s3://bucket-name)
            mount_point: Local mount point directory
            config: Configuration overrides
            foreground: Run in foreground mode
            timeout: Mount timeout in seconds

        Returns:
            Process object for the mount

        Raises:
            MountError: If mount operation fails
        """
        mount_point = Path(mount_point).resolve()
        effective_config = config or self.config

        # Validate inputs
        self._validate_mount_inputs(storage_uri, mount_point)

        # Prepare mount point
        self._prepare_mount_point(mount_point)

        # Generate configuration file if needed
        config_file = None
        try:
            config_file = self._create_temp_config(effective_config)

            # Build command
            cmd = self._build_mount_command(
                storage_uri, mount_point, config_file, foreground
            )

            logger.info(f"Mounting {storage_uri} at {mount_point}")
            logger.debug(f"Mount command: {' '.join(cmd)}")

            # Start mount process
            process = subprocess.Popen(
                cmd,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                universal_newlines=True
            )

            if foreground:
                # For foreground mounts, wait for completion
                stdout, stderr = process.communicate()
                if process.returncode != 0:
                    raise MountError(f"Mount failed: {stderr}")
                return process
            else:
                # For background mounts, wait for mount to be ready
                self._wait_for_mount(mount_point, timeout)
                return process

        except Exception as e:
            if config_file and os.path.exists(config_file):
                os.unlink(config_file)
            raise MountError(f"Failed to mount {storage_uri}: {e}") from e

    def unmount(
        self,
        mount_point: Union[str, Path],
        force: bool = False,
        timeout: int = 10
    ) -> bool:
        """
        Unmount ObjectFS filesystem.

        Args:
            mount_point: Mount point to unmount
            force: Force unmount if busy
            timeout: Unmount timeout in seconds

        Returns:
            True if successfully unmounted

        Raises:
            MountError: If unmount operation fails
        """
        mount_point = Path(mount_point).resolve()

        if not self.is_mounted(mount_point):
            logger.warning(f"{mount_point} is not mounted")
            return True

        try:
            # Try graceful unmount first
            cmd = ['fusermount', '-u', str(mount_point)]
            if force:
                cmd.insert(1, '-z')  # Lazy unmount

            logger.info(f"Unmounting {mount_point}")
            result = subprocess.run(
                cmd,
                capture_output=True,
                text=True,
                timeout=timeout
            )

            if result.returncode == 0:
                # Wait for unmount to complete
                self._wait_for_unmount(mount_point, timeout)
                logger.info(f"Successfully unmounted {mount_point}")
                return True
            else:
                logger.error(f"Unmount failed: {result.stderr}")
                if force:
                    return self._force_unmount(mount_point)
                return False

        except subprocess.TimeoutExpired:
            logger.error(f"Unmount timed out for {mount_point}")
            if force:
                return self._force_unmount(mount_point)
            return False
        except Exception as e:
            logger.error(f"Unmount error for {mount_point}: {e}")
            return False

    def is_mounted(self, mount_point: Union[str, Path]) -> bool:
        """
        Check if directory is mounted with ObjectFS.

        Args:
            mount_point: Directory to check

        Returns:
            True if mounted with ObjectFS
        """
        mount_point = Path(mount_point).resolve()

        try:
            # Check if mount point exists and is a directory
            if not mount_point.exists() or not mount_point.is_dir():
                return False

            # Check system mount table
            for mount in psutil.disk_partitions():
                if mount.mountpoint == str(mount_point):
                    # Check if it's a FUSE mount (ObjectFS uses FUSE)
                    return mount.fstype == 'fuse' or 'objectfs' in mount.device

            return False

        except Exception as e:
            logger.debug(f"Error checking mount status for {mount_point}: {e}")
            return False

    def list_mounts(self) -> List[Dict[str, Any]]:
        """
        List all ObjectFS mounts.

        Returns:
            List of mount information dictionaries
        """
        mounts = []

        try:
            for mount in psutil.disk_partitions():
                if mount.fstype == 'fuse' or 'objectfs' in mount.device:
                    mount_info = {
                        'device': mount.device,
                        'mountpoint': mount.mountpoint,
                        'fstype': mount.fstype,
                        'opts': mount.opts
                    }

                    # Add usage statistics if available
                    try:
                        usage = psutil.disk_usage(mount.mountpoint)
                        mount_info.update({
                            'total': usage.total,
                            'used': usage.used,
                            'free': usage.free,
                            'percent': usage.percent
                        })
                    except Exception:
                        pass

                    mounts.append(mount_info)

        except Exception as e:
            logger.error(f"Error listing mounts: {e}")

        return mounts

    def get_mount_info(self, mount_point: Union[str, Path]) -> Optional[Dict[str, Any]]:
        """
        Get detailed information about a specific mount.

        Args:
            mount_point: Mount point to query

        Returns:
            Mount information dictionary or None if not found
        """
        mount_point = Path(mount_point).resolve()

        for mount in self.list_mounts():
            if mount['mountpoint'] == str(mount_point):
                return mount

        return None

    # Private helper methods

    def _validate_mount_inputs(self, storage_uri: str, mount_point: Path):
        """Validate mount inputs."""
        if not storage_uri:
            raise MountError("Storage URI cannot be empty")

        # Validate storage URI format
        if not storage_uri.startswith(('s3://', 'gs://', 'az://')):
            raise MountError(f"Unsupported storage URI: {storage_uri}")

        # Validate mount point
        if not mount_point.parent.exists():
            raise MountError(f"Mount point parent directory does not exist: {mount_point.parent}")

    def _prepare_mount_point(self, mount_point: Path):
        """Prepare mount point directory."""
        try:
            mount_point.mkdir(parents=True, exist_ok=True)

            # Check if directory is empty
            if any(mount_point.iterdir()):
                logger.warning(f"Mount point {mount_point} is not empty")

            # Check permissions
            if not os.access(mount_point, os.R_OK | os.W_OK):
                raise MountError(f"Insufficient permissions for mount point: {mount_point}")

        except PermissionError:
            raise MountError(f"Permission denied creating mount point: {mount_point}")
        except Exception as e:
            raise MountError(f"Failed to prepare mount point {mount_point}: {e}")

    def _create_temp_config(self, config: Configuration) -> str:
        """Create temporary configuration file."""
        try:
            config.validate()

            fd, config_path = tempfile.mkstemp(suffix='.yaml', prefix='objectfs-')
            with os.fdopen(fd, 'w') as f:
                f.write(config.to_yaml())

            return config_path

        except Exception as e:
            raise ConfigurationError(f"Failed to create configuration file: {e}")

    def _build_mount_command(
        self,
        storage_uri: str,
        mount_point: Path,
        config_file: str,
        foreground: bool
    ) -> List[str]:
        """Build mount command arguments."""
        cmd = [self.binary_path]

        # Add configuration file
        if config_file:
            cmd.extend(['--config', config_file])

        # Add foreground flag
        if foreground:
            cmd.append('--foreground')

        # Add log level
        cmd.extend(['--log-level', self.config.global_config.log_level])

        # Add storage URI and mount point
        cmd.extend([storage_uri, str(mount_point)])

        return cmd

    def _wait_for_mount(self, mount_point: Path, timeout: int):
        """Wait for mount to be ready."""
        start_time = time.time()

        while time.time() - start_time < timeout:
            if self.is_mounted(mount_point):
                # Additional check: try to access the mount point
                try:
                    list(mount_point.iterdir())
                    return
                except Exception:
                    pass

            time.sleep(0.1)

        raise MountError(f"Mount timeout after {timeout} seconds")

    def _wait_for_unmount(self, mount_point: Path, timeout: int):
        """Wait for unmount to complete."""
        start_time = time.time()

        while time.time() - start_time < timeout:
            if not self.is_mounted(mount_point):
                return
            time.sleep(0.1)

        raise MountError(f"Unmount timeout after {timeout} seconds")

    def _force_unmount(self, mount_point: Path) -> bool:
        """Force unmount using system commands."""
        try:
            # Try lazy unmount
            result = subprocess.run(
                ['fusermount', '-u', '-z', str(mount_point)],
                capture_output=True,
                text=True,
                timeout=10
            )

            if result.returncode == 0:
                logger.info(f"Force unmount successful for {mount_point}")
                return True
            else:
                logger.error(f"Force unmount failed: {result.stderr}")
                return False

        except Exception as e:
            logger.error(f"Force unmount error: {e}")
            return False
