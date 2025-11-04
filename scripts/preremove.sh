#!/bin/bash
# ObjectFS pre-removal script

set -e

echo "ObjectFS pre-removal script"
echo

# Stop all running ObjectFS services
if command -v systemctl &> /dev/null; then
    echo "Stopping ObjectFS services..."
    for service in $(systemctl list-units --type=service --state=running | grep 'objectfs@' | awk '{print $1}'); do
        echo "  Stopping $service..."
        systemctl stop "$service" || true
    done

    # Disable all ObjectFS services
    for service in $(systemctl list-unit-files --type=service | grep 'objectfs@' | awk '{print $1}'); do
        echo "  Disabling $service..."
        systemctl disable "$service" || true
    done
fi

# Unmount any active ObjectFS mounts
echo "Checking for active ObjectFS mounts..."
if mount | grep -q 'type fuse.objectfs'; then
    echo "Found active ObjectFS mounts. Attempting to unmount..."
    mount | grep 'type fuse.objectfs' | awk '{print $3}' | while read -r mountpoint; do
        echo "  Unmounting $mountpoint..."
        fusermount3 -u "$mountpoint" 2>/dev/null || fusermount -u "$mountpoint" 2>/dev/null || umount "$mountpoint" 2>/dev/null || true
    done
fi

# Note about data preservation
echo
echo "📦 ObjectFS is being removed."
echo
echo "The following directories will be preserved:"
echo "  - /etc/objectfs/          (configuration files)"
echo "  - /var/cache/objectfs/    (cached data)"
echo "  - /var/log/objectfs/      (log files)"
echo
echo "To completely remove all ObjectFS data, manually delete these directories."
echo
