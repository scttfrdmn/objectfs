/**
 * Tests for S3StorageAdapter in ./storage.
 *
 * Every method of that class used to return fabricated data, and `downloadObject` wrote fabricated
 * data to the caller's disk: `fs.writeFile(localPath, 'Simulated file content from S3')`, followed
 * by a `progressCallback(30, 30)` and a return value of 30 that agreed with it. A caller following
 * the README destroyed whatever was at that path and was told the transfer succeeded.
 *
 * The methods throw now (#325). These tests assert the throw, and — the one that matters — that
 * `downloadObject` leaves an existing file untouched. That is the assertion the old code fails.
 *
 * Run with the repo's configured runner:
 *
 *     cd sdks/javascript && npm install && npm test
 */

import { describe, test } from '@jest/globals';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { Configuration } from './config';
import { S3StorageAdapter } from './storage';
import { StorageError } from './errors';

function adapter(): S3StorageAdapter {
  return new S3StorageAdapter(new Configuration().storage);
}

describe('the unimplemented operations', () => {
  test('downloadObject does not touch an existing local file', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'objectfs-sdk-'));
    const target = join(dir, 'precious.txt');
    writeFileSync(target, 'REAL USER DATA');

    await assert.rejects(
      () => adapter().downloadObject('s3://bucket', 'some/key', target),
      (e: unknown) =>
        e instanceof StorageError &&
        /not implemented/.test((e as Error).message)
    );

    // The load-bearing line: the old implementation replaced this with 30 bytes of placeholder.
    assert.equal(readFileSync(target, 'utf8'), 'REAL USER DATA');
  });

  test('downloadObject never invokes the progress callback', async () => {
    // The old code called progressCallback(30, 30), so a progress bar completed for a transfer
    // that never happened.
    let called = false;
    await assert.rejects(() =>
      adapter().downloadObject(
        's3://bucket',
        'k',
        join(mkdtempSync(join(tmpdir(), 'objectfs-sdk-')), 'f'),
        {
          progressCallback: () => {
            called = true;
          },
        }
      )
    );
    assert.equal(called, false);
  });

  test('uploadObject rejects rather than returning true', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'objectfs-sdk-'));
    const source = join(dir, 'upload.txt');
    writeFileSync(source, 'content');
    await assert.rejects(
      () => adapter().uploadObject('s3://bucket', 'k', source),
      (e: unknown) => e instanceof StorageError
    );
  });

  test('deleteObject rejects rather than returning true', async () => {
    await assert.rejects(
      () => adapter().deleteObject('s3://bucket', 'k'),
      (e: unknown) => e instanceof StorageError
    );
  });

  test('listObjects rejects rather than returning invented entries', async () => {
    await assert.rejects(
      () => adapter().listObjects('s3://bucket', { prefix: 'data/' }),
      (e: unknown) =>
        e instanceof StorageError &&
        /not implemented/.test((e as Error).message)
    );
  });

  test('getObjectInfo rejects rather than describing a key that may not exist', async () => {
    await assert.rejects(
      () => adapter().getObjectInfo('s3://bucket', 'never/created'),
      (e: unknown) => e instanceof StorageError
    );
  });

  test('the message names the issue, so a caller can find out why', async () => {
    await assert.rejects(
      () => adapter().deleteObject('s3://bucket', 'k'),
      (e: unknown) => /issues\/325/.test((e as Error).message)
    );
  });
});

describe('URI validation, which is the part that was always real', () => {
  test('a non-s3 scheme gets the specific message, not "Invalid S3 URI"', async () => {
    // The original wrapped the protocol check inside the same try/catch as URL construction, so
    // the "Only S3 URIs are supported" error it raised was caught one line later and re-reported
    // as "Invalid S3 URI" — the more specific of the two messages was unreachable.
    await assert.rejects(
      () => adapter().listObjects('http://bucket/key'),
      (e: unknown) => /Only S3 URIs are supported/.test((e as Error).message)
    );
  });

  test('a malformed URI is reported as malformed', async () => {
    await assert.rejects(
      () => adapter().listObjects('not a uri at all'),
      (e: unknown) => /Invalid S3 URI/.test((e as Error).message)
    );
  });

  test('URI validation happens before the not-implemented throw', async () => {
    // Ordering matters for diagnosis: a caller with both a bad URI and an unimplemented method
    // should hear about the URI, which is the thing they can fix.
    await assert.rejects(
      () => adapter().uploadObject('ftp://bucket', 'k', '/dev/null'),
      (e: unknown) => /Only S3 URIs are supported/.test((e as Error).message)
    );
  });
});
