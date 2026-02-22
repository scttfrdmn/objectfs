"""
ObjectFS Monitoring Components

Health checking and metrics collection for ObjectFS instances.
"""

import asyncio
import json
import logging
import time
from typing import Dict, Any, Optional
import aiohttp

from .exceptions import NetworkError, TimeoutError

logger = logging.getLogger(__name__)


class HealthChecker:
    """
    Health status checker for ObjectFS instances.

    Provides health monitoring capabilities with configurable
    checks and automatic retry logic.
    """

    def __init__(self, timeout: int = 10, retries: int = 3):
        """
        Initialize health checker.

        Args:
            timeout: Request timeout in seconds
            retries: Number of retry attempts
        """
        self.timeout = timeout
        self.retries = retries

    async def get_health(
        self,
        endpoint: str,
        session: aiohttp.ClientSession
    ) -> Dict[str, Any]:
        """
        Get health status from ObjectFS instance.

        Args:
            endpoint: API endpoint URL
            session: HTTP client session

        Returns:
            Health status information

        Raises:
            NetworkError: If health check fails
        """
        health_url = f"{endpoint.rstrip('/')}/health"

        for attempt in range(self.retries):
            try:
                async with session.get(
                    health_url,
                    timeout=aiohttp.ClientTimeout(total=self.timeout)
                ) as response:
                    if response.status == 200:
                        data = await response.json()
                        return self._parse_health_response(data)
                    else:
                        logger.warning(
                            f"Health check failed with status {response.status} "
                            f"(attempt {attempt + 1}/{self.retries})"
                        )

            except asyncio.TimeoutError:
                logger.warning(f"Health check timeout (attempt {attempt + 1}/{self.retries})")
            except aiohttp.ClientError as e:
                logger.warning(f"Health check client error: {e} (attempt {attempt + 1}/{self.retries})")
            except Exception as e:
                logger.error(f"Unexpected health check error: {e}")
                break

            if attempt < self.retries - 1:
                await asyncio.sleep(2 ** attempt)  # Exponential backoff

        # If all retries failed, return unhealthy status
        return {
            'status': 'unhealthy',
            'timestamp': time.time(),
            'error': 'Health check failed after all retries',
            'checks': {}
        }

    async def check_readiness(
        self,
        endpoint: str,
        session: aiohttp.ClientSession
    ) -> bool:
        """
        Check if ObjectFS instance is ready to serve requests.

        Args:
            endpoint: API endpoint URL
            session: HTTP client session

        Returns:
            True if ready, False otherwise
        """
        try:
            health = await self.get_health(endpoint, session)
            return health.get('status') == 'healthy'
        except Exception as e:
            logger.error(f"Readiness check failed: {e}")
            return False

    async def wait_for_ready(
        self,
        endpoint: str,
        session: aiohttp.ClientSession,
        timeout: int = 60
    ) -> bool:
        """
        Wait for ObjectFS instance to become ready.

        Args:
            endpoint: API endpoint URL
            session: HTTP client session
            timeout: Maximum wait time in seconds

        Returns:
            True if becomes ready within timeout
        """
        start_time = time.time()

        while time.time() - start_time < timeout:
            if await self.check_readiness(endpoint, session):
                return True

            await asyncio.sleep(1)

        return False

    def _parse_health_response(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Parse and enrich health response data."""
        parsed = {
            'status': data.get('status', 'unknown'),
            'timestamp': time.time(),
            'version': data.get('version', 'unknown'),
            'uptime': data.get('uptime', 0),
            'checks': data.get('checks', {})
        }

        # Add derived fields
        if parsed['status'] == 'healthy':
            parsed['healthy'] = True
        else:
            parsed['healthy'] = False

        return parsed


class MetricsCollector:
    """
    Metrics collector for ObjectFS instances.

    Collects and processes performance metrics, statistics,
    and operational data from ObjectFS instances.
    """

    def __init__(self, timeout: int = 10):
        """
        Initialize metrics collector.

        Args:
            timeout: Request timeout in seconds
        """
        self.timeout = timeout
        self._cache = {}
        self._cache_ttl = 30  # Cache TTL in seconds

    async def collect_metrics(
        self,
        endpoint: str,
        session: aiohttp.ClientSession
    ) -> Dict[str, Any]:
        """
        Collect metrics from ObjectFS instance.

        Args:
            endpoint: API endpoint URL
            session: HTTP client session

        Returns:
            Collected metrics data

        Raises:
            NetworkError: If metrics collection fails
        """
        # Check cache first
        cache_key = f"metrics:{endpoint}"
        if self._is_cached(cache_key):
            return self._cache[cache_key]['data']

        metrics_url = f"{endpoint.rstrip('/')}/metrics"

        try:
            async with session.get(
                metrics_url,
                timeout=aiohttp.ClientTimeout(total=self.timeout)
            ) as response:
                if response.status == 200:
                    if response.content_type == 'application/json':
                        data = await response.json()
                    else:
                        # Assume Prometheus format
                        text = await response.text()
                        data = self._parse_prometheus_metrics(text)

                    processed_data = self._process_metrics(data)
                    self._cache_metrics(cache_key, processed_data)
                    return processed_data
                else:
                    raise NetworkError(f"Metrics request failed with status {response.status}")

        except asyncio.TimeoutError:
            raise TimeoutError("Metrics collection timeout")
        except aiohttp.ClientError as e:
            raise NetworkError(f"Metrics collection failed: {e}") from e
        except Exception as e:
            logger.error(f"Unexpected metrics collection error: {e}")
            raise NetworkError(f"Metrics collection failed: {e}") from e

    async def collect_performance_stats(
        self,
        endpoint: str,
        session: aiohttp.ClientSession
    ) -> Dict[str, Any]:
        """
        Collect performance-specific statistics.

        Args:
            endpoint: API endpoint URL
            session: HTTP client session

        Returns:
            Performance statistics
        """
        metrics = await self.collect_metrics(endpoint, session)

        return {
            'cache': self._extract_cache_stats(metrics),
            'io': self._extract_io_stats(metrics),
            'network': self._extract_network_stats(metrics),
            'storage': self._extract_storage_stats(metrics),
            'distributed': self._extract_distributed_stats(metrics)
        }

    async def get_cluster_metrics(
        self,
        endpoints: list,
        session: aiohttp.ClientSession
    ) -> Dict[str, Any]:
        """
        Collect metrics from multiple cluster nodes.

        Args:
            endpoints: List of API endpoints
            session: HTTP client session

        Returns:
            Aggregated cluster metrics
        """
        tasks = [
            self.collect_metrics(endpoint, session)
            for endpoint in endpoints
        ]

        results = await asyncio.gather(*tasks, return_exceptions=True)

        cluster_metrics = {
            'nodes': {},
            'aggregate': {
                'total_nodes': len(endpoints),
                'healthy_nodes': 0,
                'total_operations': 0,
                'total_cache_hits': 0,
                'total_cache_misses': 0
            }
        }

        for i, (endpoint, result) in enumerate(zip(endpoints, results)):
            if isinstance(result, Exception):
                cluster_metrics['nodes'][endpoint] = {'error': str(result)}
            else:
                cluster_metrics['nodes'][endpoint] = result
                cluster_metrics['aggregate']['healthy_nodes'] += 1

                # Aggregate key metrics
                if 'operations' in result:
                    cluster_metrics['aggregate']['total_operations'] += result['operations'].get('total', 0)
                if 'cache' in result:
                    cluster_metrics['aggregate']['total_cache_hits'] += result['cache'].get('hits', 0)
                    cluster_metrics['aggregate']['total_cache_misses'] += result['cache'].get('misses', 0)

        return cluster_metrics

    def _is_cached(self, key: str) -> bool:
        """Check if metrics are cached and still valid."""
        if key not in self._cache:
            return False

        cached_at = self._cache[key]['timestamp']
        return time.time() - cached_at < self._cache_ttl

    def _cache_metrics(self, key: str, data: Dict[str, Any]):
        """Cache metrics data."""
        self._cache[key] = {
            'data': data,
            'timestamp': time.time()
        }

    def _parse_prometheus_metrics(self, text: str) -> Dict[str, Any]:
        """Parse Prometheus-formatted metrics."""
        metrics = {}

        for line in text.split('\n'):
            line = line.strip()
            if not line or line.startswith('#'):
                continue

            try:
                # Simple parsing - in production would use prometheus_client
                if ' ' in line:
                    metric_name, value = line.split(' ', 1)
                    metrics[metric_name] = float(value)
            except ValueError:
                continue

        return metrics

    def _process_metrics(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Process and enrich metrics data."""
        processed = {
            'timestamp': time.time(),
            'raw': data
        }

        # Extract organized metrics
        processed.update({
            'cache': self._extract_cache_stats(data),
            'io': self._extract_io_stats(data),
            'network': self._extract_network_stats(data),
            'operations': self._extract_operation_stats(data)
        })

        return processed

    def _extract_cache_stats(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Extract cache-related statistics."""
        cache_stats = {}

        # Look for cache metrics in various formats
        cache_keys = [
            'cache_hits', 'cache_misses', 'cache_size', 'cache_entries',
            'objectfs_cache_hits_total', 'objectfs_cache_misses_total'
        ]

        for key in cache_keys:
            if key in data:
                cache_stats[key.replace('objectfs_cache_', '').replace('_total', '')] = data[key]

        # Calculate derived metrics
        if 'hits' in cache_stats and 'misses' in cache_stats:
            total_requests = cache_stats['hits'] + cache_stats['misses']
            if total_requests > 0:
                cache_stats['hit_rate'] = cache_stats['hits'] / total_requests

        return cache_stats

    def _extract_io_stats(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Extract I/O statistics."""
        io_stats = {}

        io_keys = [
            'read_operations', 'write_operations', 'read_bytes', 'write_bytes',
            'objectfs_io_read_operations_total', 'objectfs_io_write_operations_total'
        ]

        for key in io_keys:
            if key in data:
                io_stats[key.replace('objectfs_io_', '').replace('_total', '')] = data[key]

        return io_stats

    def _extract_network_stats(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Extract network statistics."""
        network_stats = {}

        network_keys = [
            'network_requests', 'network_errors', 'network_latency',
            'objectfs_network_requests_total', 'objectfs_network_errors_total'
        ]

        for key in network_keys:
            if key in data:
                network_stats[key.replace('objectfs_network_', '').replace('_total', '')] = data[key]

        return network_stats

    def _extract_storage_stats(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Extract storage statistics."""
        storage_stats = {}

        storage_keys = [
            'storage_operations', 'storage_errors', 'storage_latency',
            'objectfs_storage_operations_total'
        ]

        for key in storage_keys:
            if key in data:
                storage_stats[key.replace('objectfs_storage_', '').replace('_total', '')] = data[key]

        return storage_stats

    def _extract_distributed_stats(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Extract distributed operation statistics."""
        distributed_stats = {}

        dist_keys = [
            'cluster_nodes', 'cluster_operations', 'replication_tasks',
            'objectfs_cluster_nodes', 'objectfs_distributed_operations_total'
        ]

        for key in dist_keys:
            if key in data:
                distributed_stats[key.replace('objectfs_', '').replace('_total', '')] = data[key]

        return distributed_stats

    def _extract_operation_stats(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Extract general operation statistics."""
        operation_stats = {}

        op_keys = [
            'operations_total', 'operations_successful', 'operations_failed',
            'operation_latency', 'objectfs_operations_total'
        ]

        for key in op_keys:
            if key in data:
                operation_stats[key.replace('objectfs_', '').replace('operations_', '')] = data[key]

        return operation_stats
