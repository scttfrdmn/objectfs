"""
ObjectFS Python CLI

Command-line interface for the ObjectFS Python SDK.
"""

import argparse
import asyncio
import json
import logging
import sys
from pathlib import Path
from typing import Optional

from . import ObjectFSClient, Configuration
from .exceptions import ObjectFSError


def setup_logging(level: str):
    """Setup logging configuration."""
    logging.basicConfig(
        level=getattr(logging, level.upper()),
        format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
    )


def create_parser() -> argparse.ArgumentParser:
    """Create argument parser."""
    parser = argparse.ArgumentParser(
        description='ObjectFS Python SDK CLI',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  objectfs-python mount s3://my-bucket /mnt/objectfs
  objectfs-python unmount /mnt/objectfs
  objectfs-python list-mounts
  objectfs-python health --endpoint http://localhost:8081
  objectfs-python metrics --endpoint http://localhost:9090
        """
    )

    parser.add_argument(
        '--config', '-c',
        type=str,
        help='Configuration file path'
    )

    parser.add_argument(
        '--log-level', '-l',
        choices=['DEBUG', 'INFO', 'WARN', 'ERROR'],
        default='INFO',
        help='Log level (default: INFO)'
    )

    parser.add_argument(
        '--endpoint', '-e',
        type=str,
        help='ObjectFS API endpoint'
    )

    subparsers = parser.add_subparsers(dest='command', help='Available commands')

    # Mount command
    mount_parser = subparsers.add_parser('mount', help='Mount ObjectFS filesystem')
    mount_parser.add_argument('storage_uri', help='Storage URI (e.g., s3://bucket)')
    mount_parser.add_argument('mount_point', help='Local mount point directory')
    mount_parser.add_argument('--foreground', '-f', action='store_true',
                             help='Run in foreground mode')

    # Unmount command
    unmount_parser = subparsers.add_parser('unmount', help='Unmount ObjectFS filesystem')
    unmount_parser.add_argument('mount_point', help='Mount point to unmount')
    unmount_parser.add_argument('--force', action='store_true',
                               help='Force unmount')

    # List mounts command
    subparsers.add_parser('list-mounts', help='List active ObjectFS mounts')

    # Health check command
    health_parser = subparsers.add_parser('health', help='Check ObjectFS health')
    health_parser.add_argument('--endpoint', help='Health check endpoint')

    # Metrics command
    metrics_parser = subparsers.add_parser('metrics', help='Get ObjectFS metrics')
    metrics_parser.add_argument('--endpoint', help='Metrics endpoint')
    metrics_parser.add_argument('--format', choices=['json', 'table'],
                               default='json', help='Output format')

    # Configuration commands
    config_parser = subparsers.add_parser('config', help='Configuration management')
    config_subparsers = config_parser.add_subparsers(dest='config_command')

    # Generate config
    gen_config = config_subparsers.add_parser('generate', help='Generate configuration')
    gen_config.add_argument('--preset', choices=['development', 'production',
                                                'high-performance', 'cost-optimized', 'cluster'],
                           default='production', help='Configuration preset')
    gen_config.add_argument('--output', '-o', help='Output file path')

    # Validate config
    val_config = config_subparsers.add_parser('validate', help='Validate configuration')
    val_config.add_argument('config_file', help='Configuration file to validate')

    # Storage operations
    storage_parser = subparsers.add_parser('storage', help='Storage operations')
    storage_subparsers = storage_parser.add_subparsers(dest='storage_command')

    # List objects
    list_parser = storage_subparsers.add_parser('list', help='List storage objects')
    list_parser.add_argument('storage_uri', help='Storage URI')
    list_parser.add_argument('--prefix', help='Object prefix filter')
    list_parser.add_argument('--max-keys', type=int, default=100,
                           help='Maximum objects to list')

    # Download object
    download_parser = storage_subparsers.add_parser('download', help='Download object')
    download_parser.add_argument('storage_uri', help='Storage URI')
    download_parser.add_argument('key', help='Object key')
    download_parser.add_argument('local_path', help='Local destination path')

    # Upload object
    upload_parser = storage_subparsers.add_parser('upload', help='Upload object')
    upload_parser.add_argument('local_path', help='Local file path')
    upload_parser.add_argument('storage_uri', help='Storage URI')
    upload_parser.add_argument('key', help='Object key')

    return parser


async def handle_mount(args, client: ObjectFSClient):
    """Handle mount command."""
    try:
        mount_id = client.mount(
            storage_uri=args.storage_uri,
            mount_point=args.mount_point,
            foreground=args.foreground
        )
        print(f"Successfully mounted {args.storage_uri} at {args.mount_point}")
        print(f"Mount ID: {mount_id}")
    except Exception as e:
        print(f"Mount failed: {e}", file=sys.stderr)
        return 1
    return 0


async def handle_unmount(args, client: ObjectFSClient):
    """Handle unmount command."""
    try:
        success = client.unmount(args.mount_point)
        if success:
            print(f"Successfully unmounted {args.mount_point}")
        else:
            print(f"Failed to unmount {args.mount_point}", file=sys.stderr)
            return 1
    except Exception as e:
        print(f"Unmount failed: {e}", file=sys.stderr)
        return 1
    return 0


async def handle_list_mounts(args, client: ObjectFSClient):
    """Handle list-mounts command."""
    try:
        mounts = client.list_mounts()
        if not mounts:
            print("No ObjectFS mounts found")
            return 0

        print(f"Found {len(mounts)} ObjectFS mount(s):")
        for mount in mounts:
            print(f"  {mount['device']} -> {mount['mountpoint']}")
            if 'total' in mount:
                used_pct = mount.get('percent', 0)
                print(f"    Usage: {used_pct:.1f}% ({mount['used']:,} / {mount['total']:,} bytes)")
    except Exception as e:
        print(f"Failed to list mounts: {e}", file=sys.stderr)
        return 1
    return 0


async def handle_health(args, client: ObjectFSClient):
    """Handle health check command."""
    try:
        endpoint = args.endpoint or client.api_endpoint
        if not endpoint:
            print("No endpoint specified. Use --endpoint or configure in client.",
                  file=sys.stderr)
            return 1

        health = await client.get_health(endpoint)
        print(json.dumps(health, indent=2))

        if health.get('status') != 'healthy':
            return 1
    except Exception as e:
        print(f"Health check failed: {e}", file=sys.stderr)
        return 1
    return 0


async def handle_metrics(args, client: ObjectFSClient):
    """Handle metrics command."""
    try:
        endpoint = args.endpoint or client.api_endpoint
        if not endpoint:
            print("No endpoint specified. Use --endpoint or configure in client.",
                  file=sys.stderr)
            return 1

        metrics = await client.get_metrics(endpoint)

        if args.format == 'json':
            print(json.dumps(metrics, indent=2))
        else:
            # Simple table format
            print("ObjectFS Metrics")
            print("================")
            for section, data in metrics.items():
                if isinstance(data, dict):
                    print(f"\n{section.upper()}:")
                    for key, value in data.items():
                        print(f"  {key}: {value}")
    except Exception as e:
        print(f"Failed to get metrics: {e}", file=sys.stderr)
        return 1
    return 0


async def handle_config_generate(args, client: ObjectFSClient):
    """Handle config generate command."""
    try:
        config_yaml = client.generate_config(
            preset=args.preset,
            output_path=args.output
        )

        if args.output:
            print(f"Configuration generated and saved to {args.output}")
        else:
            print(config_yaml)
    except Exception as e:
        print(f"Failed to generate configuration: {e}", file=sys.stderr)
        return 1
    return 0


async def handle_config_validate(args, client: ObjectFSClient):
    """Handle config validate command."""
    try:
        config = Configuration.from_file(args.config_file)
        config.validate()
        print(f"Configuration {args.config_file} is valid")
    except Exception as e:
        print(f"Configuration validation failed: {e}", file=sys.stderr)
        return 1
    return 0


async def handle_storage_list(args, client: ObjectFSClient):
    """Handle storage list command."""
    try:
        objects = await client.list_objects(
            storage_uri=args.storage_uri,
            prefix=args.prefix,
            max_keys=args.max_keys
        )

        if not objects.get('objects'):
            print("No objects found")
            return 0

        print(f"Found {len(objects['objects'])} object(s):")
        for obj in objects['objects']:
            key = obj.get('Key', obj.get('key', 'unknown'))
            size = obj.get('Size', obj.get('size', 0))
            modified = obj.get('LastModified', obj.get('last_modified', 'unknown'))
            print(f"  {key} ({size:,} bytes, modified: {modified})")
    except Exception as e:
        print(f"Failed to list objects: {e}", file=sys.stderr)
        return 1
    return 0


async def handle_storage_download(args, client: ObjectFSClient):
    """Handle storage download command."""
    try:
        bytes_downloaded = await client.download_object(
            storage_uri=args.storage_uri,
            key=args.key,
            local_path=args.local_path
        )
        print(f"Successfully downloaded {bytes_downloaded:,} bytes to {args.local_path}")
    except Exception as e:
        print(f"Download failed: {e}", file=sys.stderr)
        return 1
    return 0


async def handle_storage_upload(args, client: ObjectFSClient):
    """Handle storage upload command."""
    try:
        success = await client.upload_object(
            storage_uri=args.storage_uri,
            key=args.key,
            local_path=args.local_path
        )
        if success:
            file_size = Path(args.local_path).stat().st_size
            print(f"Successfully uploaded {file_size:,} bytes from {args.local_path}")
        else:
            print("Upload failed", file=sys.stderr)
            return 1
    except Exception as e:
        print(f"Upload failed: {e}", file=sys.stderr)
        return 1
    return 0


async def main():
    """Main CLI entry point."""
    parser = create_parser()
    args = parser.parse_args()

    setup_logging(args.log_level)

    if not args.command:
        parser.print_help()
        return 1

    # Create client
    try:
        config = Configuration.from_file(args.config) if args.config else None
        client = ObjectFSClient(
            config=config,
            api_endpoint=args.endpoint
        )
    except Exception as e:
        print(f"Failed to create client: {e}", file=sys.stderr)
        return 1

    try:
        async with client:
            # Handle commands
            if args.command == 'mount':
                return await handle_mount(args, client)
            elif args.command == 'unmount':
                return await handle_unmount(args, client)
            elif args.command == 'list-mounts':
                return await handle_list_mounts(args, client)
            elif args.command == 'health':
                return await handle_health(args, client)
            elif args.command == 'metrics':
                return await handle_metrics(args, client)
            elif args.command == 'config':
                if args.config_command == 'generate':
                    return await handle_config_generate(args, client)
                elif args.config_command == 'validate':
                    return await handle_config_validate(args, client)
            elif args.command == 'storage':
                if args.storage_command == 'list':
                    return await handle_storage_list(args, client)
                elif args.storage_command == 'download':
                    return await handle_storage_download(args, client)
                elif args.storage_command == 'upload':
                    return await handle_storage_upload(args, client)

            parser.print_help()
            return 1

    except KeyboardInterrupt:
        print("\nOperation cancelled by user")
        return 1
    except Exception as e:
        print(f"Command failed: {e}", file=sys.stderr)
        return 1


def cli_main():
    """Synchronous CLI entry point."""
    try:
        return asyncio.run(main())
    except KeyboardInterrupt:
        return 1


if __name__ == '__main__':
    sys.exit(cli_main())
