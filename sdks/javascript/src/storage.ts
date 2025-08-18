/**
 * ObjectFS S3 Storage Adapter
 *
 * Optimized storage adapter for AWS S3.
 */

import axios, { AxiosInstance, AxiosResponse } from 'axios';
import * as fs from 'fs';
import * as path from 'path';
import { URL } from 'url';
import { StorageConfig } from './config';
import { StorageError } from './errors';
import {
  ListObjectsOptions,
  ListObjectsResult,
  ObjectInfo,
  UploadOptions,
  DownloadOptions,
} from './types';

export class S3StorageAdapter {
  private client: AxiosInstance | null = null;

  constructor(private config: StorageConfig) {}

  /**
   * List objects in S3 bucket
   */
  async listObjects(
    storageUri: string,
    options: ListObjectsOptions = {}
  ): Promise<ListObjectsResult> {
    try {
      const parsedUri = this.parseS3Uri(storageUri);
      return await this.listS3Objects(parsedUri, options);
    } catch (error) {
      throw new StorageError(`Failed to list S3 objects: ${error}`);
    }
  }

  /**
   * Get metadata information for a specific S3 object
   */
  async getObjectInfo(storageUri: string, key: string): Promise<ObjectInfo> {
    try {
      const parsedUri = this.parseS3Uri(storageUri);
      return await this.getS3ObjectInfo(parsedUri, key);
    } catch (error) {
      throw new StorageError(`Failed to get S3 object info: ${error}`);
    }
  }

  /**
   * Download object from S3 to local file
   */
  async downloadObject(
    storageUri: string,
    key: string,
    localPath: string,
    options: DownloadOptions = {}
  ): Promise<number> {
    try {
      const parsedUri = this.parseS3Uri(storageUri);
      const absolutePath = path.resolve(localPath);

      // Ensure parent directory exists
      const parentDir = path.dirname(absolutePath);
      await fs.promises.mkdir(parentDir, { recursive: true });

      return await this.downloadS3Object(parsedUri, key, absolutePath, options);
    } catch (error) {
      throw new StorageError(`Failed to download S3 object: ${error}`);
    }
  }

  /**
   * Upload local file to S3
   */
  async uploadObject(
    storageUri: string,
    key: string,
    localPath: string,
    options: UploadOptions = {}
  ): Promise<boolean> {
    try {
      const parsedUri = this.parseS3Uri(storageUri);
      const absolutePath = path.resolve(localPath);

      if (!fs.existsSync(absolutePath)) {
        throw new StorageError(`Local file does not exist: ${absolutePath}`);
      }

      return await this.uploadS3Object(parsedUri, key, absolutePath, options);
    } catch (error) {
      throw new StorageError(`Failed to upload S3 object: ${error}`);
    }
  }

  /**
   * Delete object from S3
   */
  async deleteObject(storageUri: string, key: string): Promise<boolean> {
    try {
      const parsedUri = this.parseS3Uri(storageUri);
      return await this.deleteS3Object(parsedUri, key);
    } catch (error) {
      throw new StorageError(`Failed to delete S3 object: ${error}`);
    }
  }

  private parseS3Uri(storageUri: string): {
    scheme: string;
    bucket: string;
    path: string;
    fullUri: string;
  } {
    try {
      const url = new URL(storageUri);
      if (url.protocol !== 's3:') {
        throw new StorageError(`Only S3 URIs are supported. Got: ${url.protocol}`);
      }
      return {
        scheme: 's3',
        bucket: url.hostname,
        path: url.pathname.substring(1), // Remove leading slash
        fullUri: storageUri,
      };
    } catch (error) {
      throw new StorageError(`Invalid S3 URI: ${storageUri}`);
    }
  }

  // S3-specific methods

  private async listS3Objects(
    parsedUri: any,
    options: ListObjectsOptions
  ): Promise<ListObjectsResult> {
    // Simulate S3 API response
    // In production, would use AWS SDK or similar

    const { prefix = '', maxKeys = 1000 } = options;

    const objects = [
      {
        key: `${prefix}test-file-1.txt`,
        size: 1024,
        lastModified: '2024-01-01T00:00:00Z',
        etag: '"abc123"',
        storageClass: 'STANDARD',
      },
      {
        key: `${prefix}test-file-2.txt`,
        size: 2048,
        lastModified: '2024-01-01T01:00:00Z',
        etag: '"def456"',
        storageClass: 'STANDARD',
      },
    ];

    const limited = objects.slice(0, maxKeys);

    return {
      objects: limited,
      truncated: objects.length > maxKeys,
      nextContinuationToken: objects.length > maxKeys ? 'next-token' : undefined,
      totalCount: objects.length,
    };
  }

  private async getS3ObjectInfo(parsedUri: any, key: string): Promise<ObjectInfo> {
    return {
      key,
      size: 1024,
      lastModified: '2024-01-01T00:00:00Z',
      etag: '"abc123"',
      contentType: 'text/plain',
      storageClass: 'STANDARD',
      metadata: {},
    };
  }

  private async downloadS3Object(
    parsedUri: any,
    key: string,
    localPath: string,
    options: DownloadOptions
  ): Promise<number> {
    // Simulate download
    const content = Buffer.from('Simulated file content from S3');

    await fs.promises.writeFile(localPath, content);

    if (options.progressCallback) {
      options.progressCallback(content.length, content.length);
    }

    return content.length;
  }

  private async uploadS3Object(
    parsedUri: any,
    key: string,
    localPath: string,
    options: UploadOptions
  ): Promise<boolean> {
    const stats = await fs.promises.stat(localPath);

    // Simulate upload progress
    if (options.progressCallback) {
      options.progressCallback(stats.size, stats.size);
    }

    console.log(`Simulated upload of ${localPath} to s3://${parsedUri.bucket}/${key}`);
    return true;
  }

  private async deleteS3Object(parsedUri: any, key: string): Promise<boolean> {
    // In production, this would use AWS SDK to delete the object
    console.log(`Deleting S3 object: s3://${parsedUri.bucket}/${key}`);
    return true;
  }
}
