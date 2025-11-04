#!/bin/bash
# ObjectFS post-installation script

set -e

echo "ObjectFS post-installation script"
echo

# Create necessary directories
echo "Creating ObjectFS directories..."
mkdir -p /etc/objectfs
mkdir -p /var/cache/objectfs
mkdir -p /var/log/objectfs
mkdir -p /mnt/objectfs

# Set permissions
chmod 755 /etc/objectfs
chmod 755 /var/cache/objectfs
chmod 755 /var/log/objectfs
chmod 755 /mnt/objectfs

# Copy example configuration if config doesn't exist
if [ ! -f /etc/objectfs/config.yaml ]; then
    echo "Installing example configuration..."
    if [ -f /usr/share/objectfs/configs/example.yaml ]; then
        cp /usr/share/objectfs/configs/example.yaml /etc/objectfs/config.yaml
        chmod 600 /etc/objectfs/config.yaml
        echo "Example configuration installed at: /etc/objectfs/config.yaml"
        echo "Please edit this file with your S3 credentials and mount settings."
    fi
fi

# Reload systemd if available
if command -v systemctl &> /dev/null; then
    echo "Reloading systemd daemon..."
    systemctl daemon-reload
fi

# Check for FUSE3
if ! command -v fusermount3 &> /dev/null; then
    echo
    echo "⚠️  WARNING: FUSE3 is not installed."
    echo "ObjectFS requires FUSE3 to operate."
    echo
    echo "Install with:"
    echo "  Debian/Ubuntu: sudo apt-get install fuse3"
    echo "  RHEL/CentOS:   sudo yum install fuse3"
    echo "  Alpine:        sudo apk add fuse3"
    echo
fi

# Check kernel version for BBR support
KERNEL_VERSION=$(uname -r | cut -d. -f1-2)
KERNEL_MAJOR=$(echo $KERNEL_VERSION | cut -d. -f1)
KERNEL_MINOR=$(echo $KERNEL_VERSION | cut -d. -f2)

if [ "$KERNEL_MAJOR" -lt 4 ] || ([ "$KERNEL_MAJOR" -eq 4 ] && [ "$KERNEL_MINOR" -lt 9 ]); then
    echo
    echo "⚠️  WARNING: Linux kernel $KERNEL_VERSION detected."
    echo "BBR network optimization requires kernel 4.9 or later."
    echo "ObjectFS will work but won't benefit from BBR performance improvements."
    echo
fi

echo
echo "✅ ObjectFS installation complete!"
echo
echo "Quick start:"
echo "  1. Edit configuration: sudo nano /etc/objectfs/config.yaml"
echo "  2. Mount S3 bucket:    sudo objectfs mount s3://bucket /mnt/objectfs"
echo "  3. Or use systemd:     sudo systemctl start objectfs@mybucket.service"
echo
echo "Documentation: https://github.com/scttfrdmn/objectfs/tree/main/docs"
echo
