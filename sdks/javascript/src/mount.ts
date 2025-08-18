/**
 * ObjectFS Mount Manager
 *
 * Handles mounting and unmounting of ObjectFS filesystems.
 */

import { spawn, ChildProcess } from 'child_process';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import { promisify } from 'util';
import { Configuration } from './config';
import { MountError, ConfigurationError } from './errors';
import { MountOptions, UnmountOptions, MountInfo } from './types';

const writeFile = promisify(fs.writeFile);
const unlink = promisify(fs.unlink);
const access = promisify(fs.access);
const mkdir = promisify(fs.mkdir);
const readdir = promisify(fs.readdir);
const stat = promisify(fs.stat);

export class MountManager {
  constructor(
    private binaryPath: string,
    private config: Configuration
  ) {}

  /**
   * Mount ObjectFS filesystem
   */
  async mount(
    storageUri: string,
    mountPoint: string,
    config: Configuration,
    options: MountOptions = {}
  ): Promise<ChildProcess> {
    // Validate inputs
    this.validateMountInputs(storageUri, mountPoint);

    // Prepare mount point
    await this.prepareMountPoint(mountPoint);

    // Generate configuration file if needed
    let configFile: string | undefined;
    try {
      configFile = await this.createTempConfig(config);

      // Build command
      const cmd = this.buildMountCommand(
        storageUri,
        mountPoint,
        configFile,
        options.foreground || false
      );

      console.log(`Mounting ${storageUri} at ${mountPoint}`);
      console.log(`Mount command: ${cmd.join(' ')}`);

      // Start mount process
      const process = spawn(cmd[0], cmd.slice(1), {
        stdio: options.foreground ? 'inherit' : 'pipe',
      });

      if (options.foreground) {
        // For foreground mounts, wait for completion
        return new Promise((resolve, reject) => {
          process.on('close', code => {
            if (code === 0) {
              resolve(process);
            } else {
              reject(new MountError(`Mount failed with code ${code}`));
            }
          });

          process.on('error', error => {
            reject(new MountError(`Mount process error: ${error}`));
          });
        });
      } else {
        // For background mounts, wait for mount to be ready
        await this.waitForMount(mountPoint, options.timeout || 30000);
        return process;
      }
    } catch (error) {
      if (configFile) {
        try {
          await unlink(configFile);
        } catch (unlinkError) {
          // Ignore cleanup errors
        }
      }
      throw new MountError(`Failed to mount ${storageUri}: ${error}`);
    }
  }

  /**
   * Unmount ObjectFS filesystem
   */
  async unmount(
    mountPoint: string,
    options: UnmountOptions = {}
  ): Promise<boolean> {
    if (!(await this.isMounted(mountPoint))) {
      console.warn(`${mountPoint} is not mounted`);
      return true;
    }

    try {
      const timeout = options.timeout || 10000;

      // Try graceful unmount first
      const cmd = ['fusermount', '-u'];
      if (options.force) {
        cmd.push('-z'); // Lazy unmount
      }
      cmd.push(mountPoint);

      console.log(`Unmounting ${mountPoint}`);

      const process = spawn(cmd[0], cmd.slice(1), {
        stdio: 'pipe',
      });

      const result = await this.waitForProcess(process, timeout);

      if (result.code === 0) {
        // Wait for unmount to complete
        await this.waitForUnmount(mountPoint, timeout);
        console.log(`Successfully unmounted ${mountPoint}`);
        return true;
      } else {
        console.error(`Unmount failed: ${result.stderr}`);
        if (options.force) {
          return this.forceUnmount(mountPoint);
        }
        return false;
      }
    } catch (error) {
      console.error(`Unmount error for ${mountPoint}: ${error}`);
      return false;
    }
  }

  /**
   * Check if directory is mounted with ObjectFS
   */
  async isMounted(mountPoint: string): Promise<boolean> {
    try {
      const absolutePath = path.resolve(mountPoint);

      // Check if mount point exists and is a directory
      try {
        const stats = await stat(absolutePath);
        if (!stats.isDirectory()) {
          return false;
        }
      } catch (error) {
        return false;
      }

      // Check system mount table (Linux-specific)
      if (os.platform() === 'linux') {
        try {
          const mounts = fs.readFileSync('/proc/mounts', 'utf8');
          const lines = mounts.split('\n');

          for (const line of lines) {
            const parts = line.split(' ');
            if (parts.length >= 3 && parts[1] === absolutePath) {
              // Check if it's a FUSE mount (ObjectFS uses FUSE)
              return parts[2] === 'fuse' || parts[0].includes('objectfs');
            }
          }
        } catch (error) {
          // Fall back to other detection methods
        }
      }

      // Check using mount command (cross-platform)
      try {
        const { spawn } = require('child_process');
        const process = spawn('mount', [], { stdio: 'pipe' });

        return new Promise<boolean>((resolve) => {
          let output = '';
          process.stdout.on('data', (data: Buffer) => {
            output += data.toString();
          });

          process.on('close', () => {
            const lines = output.split('\n');
            for (const line of lines) {
              if (line.includes(absolutePath) &&
                  (line.includes('fuse') || line.includes('objectfs'))) {
                resolve(true);
                return;
              }
            }
            resolve(false);
          });

          process.on('error', () => resolve(false));
        });
      } catch (error) {
        console.debug(`Error checking mount status for ${mountPoint}: ${error}`);
        return false;
      }

      return false;
    } catch (error) {
      console.debug(`Error checking mount status for ${mountPoint}: ${error}`);
      return false;
    }
  }

  /**
   * List all ObjectFS mounts
   */
  async listMounts(): Promise<MountInfo[]> {
    const mounts: MountInfo[] = [];

    try {
      // Read mount information from /proc/mounts on Linux
      if (os.platform() === 'linux') {
        try {
          const mountsContent = fs.readFileSync('/proc/mounts', 'utf8');
          const lines = mountsContent.split('\n');

          for (const line of lines) {
            const parts = line.split(' ');
            if (parts.length >= 4 &&
                (parts[2] === 'fuse' || parts[0].includes('objectfs'))) {

              const mountInfo: MountInfo = {
                device: parts[0],
                mountpoint: parts[1],
                fstype: parts[2],
                opts: parts[3],
              };

              // Add usage statistics if available
              try {
                const stats = fs.statSync(parts[1]);
                if (stats.isDirectory()) {
                  // Try to get disk usage (simplified)
                  mountInfo.total = 0;
                  mountInfo.used = 0;
                  mountInfo.free = 0;
                  mountInfo.percent = 0;
                }
              } catch (error) {
                // Ignore errors getting usage stats
              }

              mounts.push(mountInfo);
            }
          }
        } catch (error) {
          console.error('Error reading /proc/mounts:', error);
        }
      }

      // Fallback: use mount command
      if (mounts.length === 0) {
        try {
          const { spawn } = require('child_process');
          const process = spawn('mount', [], { stdio: 'pipe' });

          await new Promise<void>((resolve) => {
            let output = '';
            process.stdout.on('data', (data: Buffer) => {
              output += data.toString();
            });

            process.on('close', () => {
              const lines = output.split('\n');
              for (const line of lines) {
                if (line.includes('fuse') || line.includes('objectfs')) {
                  // Parse mount line: device on mountpoint type fstype (opts)
                  const match = line.match(/^(.+?) on (.+?) type (.+?) \((.+?)\)$/);
                  if (match) {
                    mounts.push({
                      device: match[1],
                      mountpoint: match[2],
                      fstype: match[3],
                      opts: match[4],
                    });
                  }
                }
              }
              resolve();
            });

            process.on('error', () => resolve());
          });
        } catch (error) {
          console.error('Error listing mounts:', error);
        }
      }
    } catch (error) {
      console.error('Error listing mounts:', error);
    }

    return mounts;
  }

  /**
   * Get detailed information about a specific mount
   */
  async getMountInfo(mountPoint: string): Promise<MountInfo | null> {
    const absolutePath = path.resolve(mountPoint);
    const mounts = await this.listMounts();

    return mounts.find(mount => mount.mountpoint === absolutePath) || null;
  }

  // Private helper methods

  private validateMountInputs(storageUri: string, mountPoint: string): void {
    if (!storageUri) {
      throw new MountError('Storage URI cannot be empty');
    }

    // Validate storage URI format
    if (!storageUri.match(/^(s3|gs|az):\/\/.+/)) {
      throw new MountError(`Unsupported storage URI: ${storageUri}`);
    }

    // Validate mount point
    const parentDir = path.dirname(path.resolve(mountPoint));
    if (!fs.existsSync(parentDir)) {
      throw new MountError(`Mount point parent directory does not exist: ${parentDir}`);
    }
  }

  private async prepareMountPoint(mountPoint: string): Promise<void> {
    try {
      const absolutePath = path.resolve(mountPoint);

      // Create directory if it doesn't exist
      await mkdir(absolutePath, { recursive: true });

      // Check if directory is empty
      try {
        const files = await readdir(absolutePath);
        if (files.length > 0) {
          console.warn(`Mount point ${absolutePath} is not empty`);
        }
      } catch (error) {
        // Ignore readdir errors
      }

      // Check permissions
      try {
        await access(absolutePath, fs.constants.R_OK | fs.constants.W_OK);
      } catch (error) {
        throw new MountError(`Insufficient permissions for mount point: ${absolutePath}`);
      }
    } catch (error) {
      if (error instanceof MountError) throw error;
      throw new MountError(`Failed to prepare mount point ${mountPoint}: ${error}`);
    }
  }

  private async createTempConfig(config: Configuration): Promise<string> {
    try {
      config.validate();

      const tempDir = os.tmpdir();
      const configPath = path.join(tempDir, `objectfs-${Date.now()}.yaml`);

      await writeFile(configPath, config.toYAML(), 'utf8');
      return configPath;
    } catch (error) {
      throw new ConfigurationError(`Failed to create configuration file: ${error}`);
    }
  }

  private buildMountCommand(
    storageUri: string,
    mountPoint: string,
    configFile: string,
    foreground: boolean
  ): string[] {
    const cmd = [this.binaryPath];

    // Add configuration file
    if (configFile) {
      cmd.push('--config', configFile);
    }

    // Add foreground flag
    if (foreground) {
      cmd.push('--foreground');
    }

    // Add log level
    cmd.push('--log-level', this.config.global.logLevel);

    // Add storage URI and mount point
    cmd.push(storageUri, mountPoint);

    return cmd;
  }

  private async waitForMount(mountPoint: string, timeout: number): Promise<void> {
    const startTime = Date.now();

    while (Date.now() - startTime < timeout) {
      if (await this.isMounted(mountPoint)) {
        // Additional check: try to access the mount point
        try {
          await readdir(mountPoint);
          return;
        } catch (error) {
          // Continue waiting
        }
      }

      await new Promise(resolve => setTimeout(resolve, 100));
    }

    throw new MountError(`Mount timeout after ${timeout}ms`);
  }

  private async waitForUnmount(mountPoint: string, timeout: number): Promise<void> {
    const startTime = Date.now();

    while (Date.now() - startTime < timeout) {
      if (!(await this.isMounted(mountPoint))) {
        return;
      }
      await new Promise(resolve => setTimeout(resolve, 100));
    }

    throw new MountError(`Unmount timeout after ${timeout}ms`);
  }

  private async forceUnmount(mountPoint: string): Promise<boolean> {
    try {
      // Try lazy unmount
      const process = spawn('fusermount', ['-u', '-z', mountPoint], {
        stdio: 'pipe',
      });

      const result = await this.waitForProcess(process, 10000);

      if (result.code === 0) {
        console.log(`Force unmount successful for ${mountPoint}`);
        return true;
      } else {
        console.error(`Force unmount failed: ${result.stderr}`);
        return false;
      }
    } catch (error) {
      console.error(`Force unmount error: ${error}`);
      return false;
    }
  }

  private async waitForProcess(
    process: ChildProcess,
    timeout: number
  ): Promise<{ code: number | null; stdout: string; stderr: string }> {
    return new Promise((resolve, reject) => {
      let stdout = '';
      let stderr = '';

      const timeoutId = setTimeout(() => {
        process.kill('SIGKILL');
        reject(new Error(`Process timeout after ${timeout}ms`));
      }, timeout);

      if (process.stdout) {
        process.stdout.on('data', (data: Buffer) => {
          stdout += data.toString();
        });
      }

      if (process.stderr) {
        process.stderr.on('data', (data: Buffer) => {
          stderr += data.toString();
        });
      }

      process.on('close', (code) => {
        clearTimeout(timeoutId);
        resolve({ code, stdout, stderr });
      });

      process.on('error', (error) => {
        clearTimeout(timeoutId);
        reject(error);
      });
    });
  }
}
