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
                    # /metrics is a Prometheus text exposition on every endpoint ObjectFS
                    # serves it from -- both internal/metrics.Collector and pkg/api hand the
                    # request to promhttp. A JSON body therefore means something else is
                    # answering on this port, and parsing it as an exposition would yield
                    # zero samples and report an idle filesystem. Say so instead.
                    if response.content_type == 'application/json':
                        raise NetworkError(
                            f"{metrics_url} returned JSON; ObjectFS serves /metrics as a "
                            "Prometheus text exposition. Check that the port belongs to "
                            "ObjectFS (global.metrics_port) and not to another service."
                        )

                    text = await response.text()
                    data = self._parse_prometheus_metrics(text)

                    processed_data = self._process_metrics(data)
                    self._cache_metrics(cache_key, processed_data)
                    return processed_data
                else:
                    raise NetworkError(f"Metrics request failed with status {response.status}")

        except asyncio.TimeoutError:
            raise TimeoutError("Metrics collection timeout")
        except (NetworkError, TimeoutError):
            # Raised deliberately above, with a message that names what went wrong. The
            # catch-all below would re-wrap it as "Metrics collection failed: <the message
            # we just wrote>", burying the specific diagnosis one layer deep.
            raise
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

        Sections are the metric families the mount exports. There used to be
        ``network``, ``storage`` and ``distributed`` sections here as well, built from
        ``objectfs_network_*``, ``objectfs_storage_*`` and ``objectfs_cluster_*`` names
        that ObjectFS has never exported -- each returned an empty dict on every call, so
        the response advertised five kinds of telemetry and carried at most two. They are
        gone rather than stubbed: a caller can tell that a key is missing, and cannot tell
        that a present-but-empty one means "not implemented".

        Args:
            endpoint: API endpoint URL
            session: HTTP client session

        Returns:
            Performance statistics, keyed by section.
        """
        metrics = await self.collect_metrics(endpoint, session)
        raw = metrics.get('raw', {})

        return {
            'cache': self._extract_cache_stats(raw),
            'io': self._extract_io_stats(raw),
            'operations': self._extract_operation_stats(raw),
            'errors': self._extract_error_stats(raw),
            'connections': self._extract_connection_stats(raw),
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
        """Parse a Prometheus text exposition into a list of labelled samples.

        Returns a dict with one key, ``samples``, holding a list of
        ``{'name': str, 'labels': Dict[str, str], 'value': float}``.

        Every series ObjectFS exports carries labels, so this has to understand them.
        The previous implementation split each line on its first space and used the
        left half as a key, which for a real scrape line produces::

            'objectfs_cache_requests_total{service="objectfs",type="hit"}'

        -- name and labels fused into one string. Nothing downstream ever looked up a
        key of that shape, so every extractor below found nothing and returned {},
        for every metric, on every scrape. The samples are kept as a list rather than
        a dict because a metric name is not unique: hit and miss are two samples of
        one name, and collapsing them by name would silently keep whichever came last.
        """
        samples = []

        for line in text.split('\n'):
            line = line.strip()
            if not line or line.startswith('#'):
                continue

            name, labels, value = self._parse_sample_line(line)
            if name is None:
                continue

            samples.append({'name': name, 'labels': labels, 'value': value})

        return {'samples': samples}

    @staticmethod
    def _parse_sample_line(line: str):
        """Split one exposition line into (name, labels, value).

        Returns ``(None, {}, 0.0)`` for a line that is not a sample.

        Values may be ``NaN``, ``+Inf`` or ``-Inf`` per the exposition format, all of
        which float() accepts. A trailing timestamp is permitted by the format and is
        discarded -- it is the scrape time, not part of the value.
        """
        brace = line.find('{')
        if brace == -1:
            parts = line.split()
            if len(parts) < 2:
                return None, {}, 0.0
            try:
                return parts[0], {}, float(parts[1])
            except ValueError:
                return None, {}, 0.0

        close = line.rfind('}')
        if close < brace:
            return None, {}, 0.0

        name = line[:brace]
        labels = MetricsCollector._parse_labels(line[brace + 1:close])

        rest = line[close + 1:].split()
        if not rest:
            return None, {}, 0.0
        try:
            return name, labels, float(rest[0])
        except ValueError:
            return None, {}, 0.0

    @staticmethod
    def _parse_labels(text: str) -> Dict[str, str]:
        """Parse the inside of a label block into a dict.

        Scanned character by character rather than split on ',' because a label value
        is an escaped string and may legitimately contain a comma or an escaped quote
        -- an error message in an ``errors_total`` label, for instance. Splitting on
        ',' would break such a line into fragments and silently drop the metric.
        """
        labels: Dict[str, str] = {}
        i, n = 0, len(text)

        while i < n:
            eq = text.find('=', i)
            if eq == -1:
                break

            key = text[i:eq].strip()
            j = eq + 1
            if j >= n or text[j] != '"':
                break

            j += 1
            chars = []
            while j < n:
                ch = text[j]
                if ch == '\\' and j + 1 < n:
                    nxt = text[j + 1]
                    chars.append({'n': '\n', '"': '"', '\\': '\\'}.get(nxt, nxt))
                    j += 2
                    continue
                if ch == '"':
                    break
                chars.append(ch)
                j += 1

            labels[key] = ''.join(chars)

            # Past the closing quote, then past the separating comma if there is one.
            i = j + 1
            while i < n and text[i] in ', ':
                i += 1

        return labels

    def _process_metrics(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Organize parsed samples into per-subsystem sections.

        ``raw`` carries the parsed samples so a caller can read a metric this does not
        surface, without re-scraping.
        """
        return {
            'timestamp': time.time(),
            'raw': data,
            'cache': self._extract_cache_stats(data),
            'io': self._extract_io_stats(data),
            'operations': self._extract_operation_stats(data),
            'errors': self._extract_error_stats(data),
            'connections': self._extract_connection_stats(data),
        }

    @staticmethod
    def _samples(data: Dict[str, Any], name: str) -> list:
        """Return every sample of a metric family, in scrape order."""
        return [s for s in data.get('samples', []) if s['name'] == name]

    def _extract_cache_stats(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Extract cache statistics from ``objectfs_cache_*``.

        Reads ``objectfs_cache_requests_total{type="hit"|"miss"}`` and
        ``objectfs_cache_size_bytes{level}``, which is what the mount exports. This
        used to look for ``cache_hits`` and ``objectfs_cache_hits_total``, names no
        version of ObjectFS has ever exported -- so ``hit_rate``, the one number the
        SDK README shows in its example, was never present and that example raised
        KeyError against a healthy mount.

        Keys are absent rather than zero when the mount has not served a cache request
        yet, because a hit rate of 0.0 and no requests at all are different facts and a
        dashboard should be able to tell them apart.
        """
        cache_stats: Dict[str, Any] = {}

        for sample in self._samples(data, 'objectfs_cache_requests_total'):
            kind = sample['labels'].get('type')
            if kind == 'hit':
                cache_stats['hits'] = sample['value']
            elif kind == 'miss':
                cache_stats['misses'] = sample['value']

        levels = {
            s['labels'].get('level', ''): s['value']
            for s in self._samples(data, 'objectfs_cache_size_bytes')
        }
        if levels:
            cache_stats['levels'] = levels
            cache_stats['size'] = sum(levels.values())

        hits = cache_stats.get('hits', 0.0)
        misses = cache_stats.get('misses', 0.0)
        total = hits + misses
        if total > 0:
            cache_stats['hit_rate'] = hits / total

        return cache_stats

    def _extract_operation_stats(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Extract operation counts and latency from ``objectfs_operation*``.

        ``objectfs_operations_total`` is labelled by operation and status, so a total
        is a sum across samples rather than the value of any one of them -- reading a
        single sample would report whichever operation the exposition happened to list
        last.

        Average latency comes from the histogram's ``_sum``/``_count`` pair. It is the
        mean over the whole life of the mount, not a recent window: computing a rate
        needs two scrapes, which a single call cannot do. Use Prometheus for that.
        """
        stats: Dict[str, Any] = {}

        by_operation: Dict[str, Dict[str, float]] = {}
        total = successful = failed = 0.0

        for sample in self._samples(data, 'objectfs_operations_total'):
            operation = sample['labels'].get('operation', '')
            status = sample['labels'].get('status', '')
            value = sample['value']

            total += value
            if status == 'success':
                successful += value
            elif status == 'error':
                failed += value

            entry = by_operation.setdefault(operation, {'count': 0.0})
            entry['count'] += value

        if not by_operation:
            return stats

        for sample in self._samples(data, 'objectfs_operation_duration_seconds_sum'):
            operation = sample['labels'].get('operation', '')
            if operation in by_operation:
                by_operation[operation]['duration_seconds'] = sample['value']

        for sample in self._samples(data, 'objectfs_operation_size_bytes_sum'):
            operation = sample['labels'].get('operation', '')
            if operation in by_operation:
                by_operation[operation]['bytes'] = sample['value']

        for operation, entry in by_operation.items():
            if entry['count'] > 0 and 'duration_seconds' in entry:
                entry['avg_duration_seconds'] = entry['duration_seconds'] / entry['count']

        stats['total'] = total
        stats['successful'] = successful
        stats['failed'] = failed
        stats['by_operation'] = by_operation

        return stats

    def _extract_io_stats(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Extract read/write counts and byte totals from the operation metrics.

        There is no ``objectfs_io_*`` family -- the names this used to look for were
        invented. Read and write volume is carried by ``objectfs_operations_total`` and
        ``objectfs_operation_size_bytes_sum``, both labelled by operation, so I/O stats
        are a projection of those onto the ``read`` and ``write`` operation names.

        Only operations the mount actually records appear. As of v0.10.1 the FUSE layer
        records cache hits and misses and the prefetcher records ``prefetch``; ``read``
        and ``write`` are not yet recorded through RecordOperation, so this returns {}
        against a live mount. That is the honest answer, and it changes as soon as the
        recording lands -- which is the point of deriving it here rather than hardcoding
        a plausible number.
        """
        operations = self._extract_operation_stats(data)
        by_operation = operations.get('by_operation', {})

        io_stats: Dict[str, Any] = {}

        for direction in ('read', 'write'):
            entry = by_operation.get(direction)
            if entry is None:
                continue
            io_stats[f'{direction}_operations'] = entry['count']
            if 'bytes' in entry:
                io_stats[f'{direction}_bytes'] = entry['bytes']

        return io_stats

    def _extract_error_stats(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Extract error counts from ``objectfs_errors_total{operation,type}``.

        The collector classifies each error as timeout, connection, not_found,
        permission, throttling or other, which is more useful split out than summed:
        a mount failing on permissions and one failing on throttling need different
        responses.
        """
        stats: Dict[str, Any] = {}
        by_type: Dict[str, float] = {}
        by_operation: Dict[str, float] = {}
        total = 0.0

        for sample in self._samples(data, 'objectfs_errors_total'):
            value = sample['value']
            total += value
            by_type[sample['labels'].get('type', '')] = (
                by_type.get(sample['labels'].get('type', ''), 0.0) + value
            )
            operation = sample['labels'].get('operation', '')
            by_operation[operation] = by_operation.get(operation, 0.0) + value

        if not by_type:
            return stats

        stats['total'] = total
        stats['by_type'] = by_type
        stats['by_operation'] = by_operation

        return stats

    def _extract_connection_stats(self, data: Dict[str, Any]) -> Dict[str, Any]:
        """Extract ``objectfs_active_connections``, an unlabelled gauge."""
        samples = self._samples(data, 'objectfs_active_connections')
        if not samples:
            return {}

        return {'active': samples[0]['value']}
