"""Tests for ObjectFSClient's error surface.

The client delegates the five storage operations to StorageAdapter and wraps whatever comes back
in ``StorageError(f"Failed to <verb>: {e}")``. Because the adapter's own handler already prefixes
the same phrase, a caller saw it twice with the useful text -- the not-implemented notice and its
issue link -- nested two levels down. The CLI prints exactly that string, so this was user-visible.
``SpecificErrorsReachTheCallerTest`` is what holds the fix in place.

The cluster and cache methods are here for the same reason as the storage ones: each returned a
value that looked like a measurement or an acknowledgement. ``get_cluster_status`` reporting
``'status': 'healthy'`` without querying anything is the one that would have done real damage.

Run from ``sdks/python``::

    pip install . pytest && pytest tests/ -q
"""

import asyncio
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..'))

from objectfs.client import ObjectFSClient  # noqa: E402
from objectfs.exceptions import (  # noqa: E402
    CacheError,
    DistributedError,
    ObjectFSError,
    StorageError,
)


def client() -> ObjectFSClient:
    """A client that does not need ObjectFS installed.

    ``binary_path`` is passed explicitly because the constructor otherwise calls
    ``shutil.which('objectfs')`` and raises ObjectFSError -- see BinaryDiscoveryTest. Nothing here
    executes the binary; the methods under test all raise before reaching it.
    """
    return ObjectFSClient(binary_path='/bin/sh')


def run(coro):
    """Drive one coroutine to completion; see the note in test_storage.py."""
    return asyncio.run(coro)


class SpecificErrorsReachTheCallerTest(unittest.TestCase):
    """The client must not re-wrap an error the adapter already made specific."""

    def test_not_implemented_notice_survives_the_client_layer(self):
        with self.assertRaises(StorageError) as caught:
            run(client().list_objects('s3://bucket'))
        message = str(caught.exception)
        # The adapter's message, verbatim and unnested.
        self.assertTrue(
            message.startswith('StorageAdapter.list_objects'),
            f'message was re-wrapped: {message!r}',
        )
        self.assertIn('issues/325', message)

    def test_the_phrase_appears_once_not_twice(self):
        """Two layers each prefixing "Failed to download object" is what this caught."""
        for uri in ('s3://bucket',):
            with self.subTest(uri=uri):
                with self.assertRaises(StorageError) as caught:
                    run(client().download_object(uri, 'k', '/tmp/objectfs-test-target'))
                self.assertEqual(str(caught.exception).count('Failed to download object'), 0)

    def test_uri_errors_survive_too(self):
        """A malformed URI is the error a caller can actually act on."""
        with self.assertRaises(StorageError) as caught:
            run(client().get_object_info('not-a-uri', 'k'))
        self.assertIn('missing scheme', str(caught.exception))

    def test_upload_reports_the_missing_local_file(self):
        """This path is real: the adapter checks existence before dispatching."""
        with self.assertRaises(StorageError) as caught:
            run(client().upload_object('s3://bucket', 'k', '/nonexistent/objectfs/path'))
        self.assertIn('does not exist', str(caught.exception))


class DownloadLeavesLocalFilesAloneTest(unittest.TestCase):
    """The client-level equivalent of the assertion in test_storage.py."""

    def test_existing_file_survives(self):
        with tempfile.TemporaryDirectory() as directory:
            target = os.path.join(directory, 'precious.txt')
            with open(target, 'w', encoding='utf-8') as handle:
                handle.write('REAL USER DATA')

            with self.assertRaises(StorageError):
                run(client().download_object('s3://bucket', 'some/key', target))

            with open(target, 'r', encoding='utf-8') as handle:
                self.assertEqual(handle.read(), 'REAL USER DATA')


class ClusterOperationsRaiseTest(unittest.TestCase):
    """None of these contacted a node; all three reported success."""

    def test_join_cluster(self):
        with self.assertRaises(DistributedError):
            run(client().join_cluster(['node1.example.com:8080']))

    def test_leave_cluster(self):
        with self.assertRaises(DistributedError):
            run(client().leave_cluster())

    def test_get_cluster_status_does_not_report_health(self):
        """It returned ``'status': 'healthy'`` for any configuration, having queried nothing."""
        with self.assertRaises(DistributedError) as caught:
            run(client().get_cluster_status())
        self.assertNotIn('healthy', str(caught.exception).split('previously')[0])


class CacheOperationsRaiseTest(unittest.TestCase):
    """``clear_cache`` returned True after a log line; ``warm_cache`` claimed every path."""

    def test_clear_cache(self):
        with self.assertRaises(CacheError):
            run(client().clear_cache())

    def test_warm_cache(self):
        with self.assertRaises(CacheError):
            run(client().warm_cache(['/mnt/objectfs/data']))


class PerformanceStatsRaisesTest(unittest.TestCase):
    """It returned fixed constants -- including a 0.85 cache hit rate -- shaped as telemetry."""

    def test_raises_and_points_at_get_metrics(self):
        with self.assertRaises(NotImplementedError) as caught:
            run(client().get_performance_stats())
        self.assertIn('get_metrics', str(caught.exception))


class EveryRaisingMethodNamesTheIssueTest(unittest.TestCase):
    """A caller should be able to find out why without reading the source."""

    def test_messages_carry_the_issue_link(self):
        cases = (
            ('list_objects', lambda c: c.list_objects('s3://bucket')),
            ('download_object', lambda c: c.download_object('s3://b', 'k', '/tmp/x')),
            ('join_cluster', lambda c: c.join_cluster(['n:8080'])),
            ('leave_cluster', lambda c: c.leave_cluster()),
            ('get_cluster_status', lambda c: c.get_cluster_status()),
            ('clear_cache', lambda c: c.clear_cache()),
            ('warm_cache', lambda c: c.warm_cache(['/p'])),
        )
        for name, call in cases:
            with self.subTest(method=name):
                with self.assertRaises(ObjectFSError) as caught:
                    run(call(client()))
                self.assertIn('issues/325', str(caught.exception))


class BinaryDiscoveryTest(unittest.TestCase):
    """Constructing a client without ObjectFS installed must say so, not fail later."""

    def test_missing_binary_is_reported_at_construction(self):
        original = os.environ.get('PATH', '')
        os.environ['PATH'] = ''
        try:
            with self.assertRaises(ObjectFSError) as caught:
                ObjectFSClient()
            self.assertIn('not found in PATH', str(caught.exception))
        finally:
            os.environ['PATH'] = original


if __name__ == '__main__':
    unittest.main()
