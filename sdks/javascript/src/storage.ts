/**
 * ObjectFS S3 Storage Adapter
 *
 * Every method in this class used to return invented data, and one of them wrote invented data to
 * the caller's disk:
 *
 *   listObjects   → two hardcoded entries, test-file-1.txt and test-file-2.txt, keyed under the
 *                   caller's own prefix so the result looked derived from the request
 *   getObjectInfo → size 1024, etag '"abc123"', for any key, including keys that do not exist
 *   downloadObject→ fs.writeFile(localPath, 'Simulated file content from S3'), then a
 *                   progressCallback(30, 30) and a return value of 30 that agreed with it
 *   uploadObject  → console.log and `return true`; nothing was uploaded
 *   deleteObject  → console.log and `return true`; the object was still in S3
 *
 * They throw now. A caller who reaches one finds out at the call, which is the whole difference:
 * `downloadObject` pointed at a real path destroyed what was there and reported a successful
 * 30-byte transfer, and nothing in the SDK, its one test file, or CI would have said so.
 *
 * Implementing these for real means @aws-sdk/client-s3, which this package does not depend on, plus
 * credential resolution, list pagination, and multipart upload. That is tracked as #325 along with
 * the identical fabrication in the Python SDK. The signatures are kept so the break is a thrown
 * error naming the issue rather than a TypeError on an undefined method.
 *
 * The pattern is the one internal/distributed/coordinator.go:547-602 and gossip.go:823-828 already
 * use in the Go tree: decline to return a value you do not have, and say which issue covers it.
 */

import * as path from 'path';
import { URL } from 'url';
import { StorageError } from './errors';
import {
  ListObjectsOptions,
  ListObjectsResult,
  ObjectInfo,
  UploadOptions,
  DownloadOptions,
} from './types';
import { StorageConfig } from './types';

const NOT_IMPLEMENTED =
  'is not implemented. The ObjectFS JavaScript SDK has no S3 client: this method previously ' +
  'returned fabricated data (and downloadObject overwrote the local path with placeholder ' +
  'content). See https://github.com/scttfrdmn/objectfs/issues/325. Use the AWS SDK directly, or ' +
  'mount the bucket with ObjectFS and use ordinary filesystem calls.';

function notImplemented(method: string): never {
  throw new StorageError(
    `S3StorageAdapter.${method} ${NOT_IMPLEMENTED}`,
    'ENOTIMPLEMENTED'
  );
}

export class S3StorageAdapter {
  constructor(private config: StorageConfig) {}

  /**
   * List objects in S3 bucket.
   *
   * @throws StorageError always — see #325.
   */
  async listObjects(
    storageUri: string,
    options: ListObjectsOptions = {}
  ): Promise<ListObjectsResult> {
    // Validated before throwing so a caller with a malformed URI still learns that first; the URI
    // parser below is the one part of this class that was ever real.
    this.parseS3Uri(storageUri);
    void options;
    notImplemented('listObjects');
  }

  /**
   * Get metadata information for a specific S3 object.
   *
   * @throws StorageError always — see #325.
   */
  async getObjectInfo(storageUri: string, key: string): Promise<ObjectInfo> {
    this.parseS3Uri(storageUri);
    void key;
    notImplemented('getObjectInfo');
  }

  /**
   * Download object from S3 to local file.
   *
   * @throws StorageError always — see #325. This method used to write
   * 'Simulated file content from S3' to `localPath`, destroying any file already there.
   */
  async downloadObject(
    storageUri: string,
    key: string,
    localPath: string,
    options: DownloadOptions = {}
  ): Promise<number> {
    this.parseS3Uri(storageUri);
    void key;
    void path.resolve(localPath);
    void options;
    notImplemented('downloadObject');
  }

  /**
   * Upload local file to S3.
   *
   * @throws StorageError always — see #325.
   */
  async uploadObject(
    storageUri: string,
    key: string,
    localPath: string,
    options: UploadOptions = {}
  ): Promise<boolean> {
    this.parseS3Uri(storageUri);
    void key;
    void path.resolve(localPath);
    void options;
    notImplemented('uploadObject');
  }

  /**
   * Delete object from S3.
   *
   * @throws StorageError always — see #325.
   */
  async deleteObject(storageUri: string, key: string): Promise<boolean> {
    this.parseS3Uri(storageUri);
    void key;
    notImplemented('deleteObject');
  }

  private parseS3Uri(storageUri: string): {
    scheme: string;
    bucket: string;
    path: string;
    fullUri: string;
  } {
    let url: URL;
    try {
      url = new URL(storageUri);
    } catch {
      // Only URL construction is guarded. The original wrapped the protocol check in the same try,
      // so an http:// URI reported "Invalid S3 URI" instead of the specific "Only S3 URIs are
      // supported" message it had raised one line earlier.
      throw new StorageError(`Invalid S3 URI: ${storageUri}`);
    }
    if (url.protocol !== 's3:') {
      throw new StorageError(
        `Only S3 URIs are supported. Got: ${url.protocol}`
      );
    }
    return {
      scheme: 's3',
      bucket: url.hostname,
      path: url.pathname.substring(1), // Remove leading slash
      fullUri: storageUri,
    };
  }
}
