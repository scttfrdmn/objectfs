"""Tests for StorageAdapter in objectfs.storage.

Every backend method of that class used to return fabricated data, and ``download_object`` wrote
fabricated data to the caller's disk: ``open(local_path, 'wb').write(b"Simulated file content from
S3")``, followed by a ``progress_callback(30, 30)`` and a return value of 30 that agreed with it. A
caller following the README destroyed whatever was at that path and was told the transfer
succeeded, with a completed progress bar.

The methods raise now (#325). These tests assert the raise and -- the one that matters -- that
``download_object`` leaves an existing file untouched. That is the assertion the old code fails.

Run from ``sdks/python``::

    pip install . pytest && pytest tests/ -q
"""

import asyncio
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..'))

from objectfs.config import StorageConfig  # noqa: E402
from objectfs.exceptions import StorageError  # noqa: E402
from objectfs.storage import StorageAdapter  # noqa: E402


def adapter() -> StorageAdapter:
    return StorageAdapter(StorageConfig())


def run(coro):
    """Drive one coroutine to completion.

    ``asyncio.run`` per test rather than IsolatedAsyncioTestCase, to match the plain
    ``unittest.TestCase`` style of tests/test_monitoring.py and to keep this suite runnable under
    any Python the SDK claims to support.
    """
    return asyncio.run(coro)


class DownloadDoesNotTouchLocalFilesTest(unittest.TestCase):
    """The load-bearing behaviour: a failed download must not be a destructive one."""

    def test_existing_file_survives(self):
        with tempfile.TemporaryDirectory() as directory:
            target = os.path.join(directory, 'precious.txt')
            with open(target, 'w', encoding='utf-8') as handle:
                handle.write('REAL USER DATA')

            with self.assertRaises(StorageError) as caught:
                run(adapter().download_object('s3://bucket', 'some/key', target))
            self.assertIn('not implemented', str(caught.exception))

            # The old implementation replaced this with 30 bytes of placeholder.
            with open(target, 'r', encoding='utf-8') as handle:
                self.assertEqual(handle.read(), 'REAL USER DATA')

    def test_progress_callback_is_never_invoked(self):
        """A progress bar that fills is a claim that bytes moved."""
        calls = []

        with tempfile.TemporaryDirectory() as directory:
            target = os.path.join(directory, 'f')
            with self.assertRaises(StorageError):
                run(adapter().download_object(
                    's3://bucket', 'k', target,
                    progress_callback=lambda done, total: calls.append((done, total)),
                ))

        self.assertEqual(calls, [])

    def test_no_file_is_created_where_none_existed(self):
        with tempfile.TemporaryDirectory() as directory:
            target = os.path.join(directory, 'nested', 'f')
            with self.assertRaises(StorageError):
                run(adapter().download_object('s3://bucket', 'k', target))
            self.assertFalse(os.path.exists(target))


class UnimplementedOperationsRaiseTest(unittest.TestCase):
    """Each of these returned a value that a caller could not tell from a real one."""

    def test_list_objects(self):
        with self.assertRaises(StorageError) as caught:
            run(adapter().list_objects('s3://bucket', prefix='data/'))
        message = str(caught.exception)
        self.assertIn('not implemented', message)
        # Not wrapped in "Failed to list objects: ..." -- the specific message is the useful one.
        self.assertTrue(message.startswith('StorageAdapter.list_objects'), message)

    def test_get_object_info(self):
        with self.assertRaises(StorageError):
            run(adapter().get_object_info('s3://bucket', 'never/created'))

    def test_upload_object(self):
        with tempfile.TemporaryDirectory() as directory:
            source = os.path.join(directory, 'upload.txt')
            with open(source, 'w', encoding='utf-8') as handle:
                handle.write('content')
            with self.assertRaises(StorageError):
                run(adapter().upload_object('s3://bucket', 'k', source))

    def test_delete_object(self):
        with self.assertRaises(StorageError):
            run(adapter().delete_object('s3://bucket', 'k'))

    def test_message_names_the_issue(self):
        """So a caller who hits this can find out why without reading the source."""
        with self.assertRaises(StorageError) as caught:
            run(adapter().delete_object('s3://bucket', 'k'))
        self.assertIn('issues/325', str(caught.exception))

    def test_gcs_and_azure_raise_too(self):
        """They delegated to the S3 methods, so they returned invented *S3* objects."""
        for uri in ('gs://bucket', 'az://container'):
            with self.subTest(uri=uri):
                with self.assertRaises(StorageError) as caught:
                    run(adapter().list_objects(uri))
                self.assertIn('not implemented', str(caught.exception))


class UriValidationTest(unittest.TestCase):
    """The part of this class that was always real."""

    def test_missing_scheme_is_reported_as_such(self):
        with self.assertRaises(StorageError) as caught:
            run(adapter().list_objects('not-a-uri'))
        self.assertIn('missing scheme', str(caught.exception))

    def test_unsupported_scheme_names_the_scheme(self):
        with self.assertRaises(StorageError) as caught:
            run(adapter().list_objects('ftp://host/path'))
        message = str(caught.exception)
        self.assertIn('Unsupported storage backend', message)
        self.assertIn('ftp', message)

    def test_validation_precedes_the_not_implemented_raise(self):
        """A caller with both problems should hear about the one they can fix."""
        with self.assertRaises(StorageError) as caught:
            run(adapter().upload_object('ftp://host', 'k', __file__))
        self.assertIn('Unsupported storage backend', str(caught.exception))
        self.assertNotIn('not implemented', str(caught.exception))


if __name__ == '__main__':
    unittest.main()
