# Quick Start Guide

This guide will get you up and running with ObjectFS in minutes. We'll walk through
installation, basic configuration, and your first mount.

## Prerequisites

Before you begin, ensure you have:

- Linux, macOS, or Windows with WSL2
- Root access (for FUSE mounting)
- An object storage account (AWS S3, Google Cloud Storage, or Azure Blob Storage)
- Basic familiarity with command-line operations

## Installation

### Option 1: Quick Install Script

<CodeRunner language="bash">

```bash
# Download and install ObjectFS
curl -sSL https://get.objectfs.io | sh

# Add to PATH (if not done automatically)
export PATH="/usr/local/bin:$PATH"

# Verify installation
objectfs --version
```

</CodeRunner>

### Option 2: Package Manager

#### Ubuntu/Debian

<CodeRunner language="bash">

```bash
# Add ObjectFS repository
curl -fsSL https://packages.objectfs.io/gpg | sudo apt-key add -
echo "deb https://packages.objectfs.io/ubuntu $(lsb_release -cs) main" | sudo tee /etc/apt/sources.list.d/objectfs.list

# Install ObjectFS
sudo apt update
sudo apt install objectfs
```

</CodeRunner>

#### macOS (Homebrew)

<CodeRunner language="bash">

```bash
# Install via Homebrew
brew tap objectfs/tap
brew install objectfs

# Install macFUSE dependency
brew install --cask macfuse
```

</CodeRunner>

#### Arch Linux

<CodeRunner language="bash">

```bash
# Install from AUR
yay -S objectfs-bin

# Or using makepkg
git clone https://aur.archlinux.org/objectfs-bin.git
cd objectfs-bin
makepkg -si
```

</CodeRunner>

## First Mount

Let's mount your first object storage bucket as a local filesystem.

### 1. Set Up Credentials

#### AWS S3

<CodeRunner language="bash">

```bash
# Configure AWS credentials
export AWS_ACCESS_KEY_ID="your-access-key"
export AWS_SECRET_ACCESS_KEY="your-secret-key"
export AWS_REGION="us-east-1"

# Or use AWS CLI
aws configure
```

</CodeRunner>

#### Google Cloud Storage

<CodeRunner language="bash">

```bash
# Authenticate with Google Cloud
gcloud auth application-default login

# Set project
gcloud config set project your-project-id
```

</CodeRunner>

#### Azure Blob Storage

<CodeRunner language="bash">

```bash
# Set Azure credentials
export AZURE_STORAGE_ACCOUNT="your-account"
export AZURE_STORAGE_KEY="your-key"

# Or use Azure CLI
az login
```

</CodeRunner>

### 2. Create a Mount Point

<CodeRunner language="bash">

```bash
# Create directory for mount
sudo mkdir -p /mnt/objectfs
sudo chown $(whoami):$(whoami) /mnt/objectfs
```

</CodeRunner>

### 3. Mount the Filesystem

<CodeRunner language="bash">

```bash
# Mount an S3 bucket
objectfs mount s3://your-bucket-name /mnt/objectfs

# Mount with custom configuration
objectfs mount s3://your-bucket-name /mnt/objectfs \
  --cache-size 8GB \
  --log-level info \
  --foreground
```

</CodeRunner>

### 4. Verify the Mount

<CodeRunner language="bash">

```bash
# Check if mounted
df -h /mnt/objectfs
mount | grep objectfs

# List contents
ls -la /mnt/objectfs

# Test read/write operations
echo "Hello ObjectFS!" > /mnt/objectfs/test.txt
cat /mnt/objectfs/test.txt
```

</CodeRunner>

## Basic Operations

Now that you have ObjectFS mounted, let's explore basic operations:

### File Operations

<CodeRunner language="bash">

```bash
# Copy files to object storage
cp /path/to/local/file.txt /mnt/objectfs/

# Create directories
mkdir /mnt/objectfs/my-folder

# Move files
mv /mnt/objectfs/old-name.txt /mnt/objectfs/new-name.txt

# Delete files
rm /mnt/objectfs/unwanted-file.txt
```

</CodeRunner>

### Directory Operations

<CodeRunner language="bash">

```bash
# Recursive copy
cp -r /path/to/local/folder /mnt/objectfs/

# Find files
find /mnt/objectfs -name "*.txt" -type f

# Directory sizes
du -h /mnt/objectfs/my-folder
```

</CodeRunner>

### Advanced Operations

<CodeRunner language="bash">

```bash
# Stream large files
cat /mnt/objectfs/large-file.txt | grep "pattern"

# Compress and upload
tar -czf - /path/to/folder | cat > /mnt/objectfs/backup.tar.gz

# Download and extract
cat /mnt/objectfs/backup.tar.gz | tar -xzf -
```

</CodeRunner>

## Configuration

ObjectFS can be configured using command-line options, configuration files, or environment variables.

### Command-Line Options

<CodeRunner language="bash">

```bash
objectfs mount s3://bucket /mnt/objectfs \
  --cache-size 8GB \
  --max-concurrency 100 \
  --log-level debug \
  --enable-predictive-caching \
  --cost-optimization
```

</CodeRunner>

### Configuration File

Create a configuration file for persistent settings:

<CodeRunner language="yaml">

```yaml
# ~/.objectfs/config.yaml
global:
  log_level: info
  log_file: /var/log/objectfs.log

storage:
  s3:
    region: us-east-1
    use_acceleration: true
    cost_optimization:
      enabled: true
      tiering_enabled: true

performance:
  cache_size: 8GB
  max_concurrency: 100
  multilevel_caching: true
  predictive_caching: true

monitoring:
  enabled: true
  metrics_addr: :9090
  health_check_addr: :8081
```

</CodeRunner>

<CodeRunner language="bash">

```bash
# Use configuration file
objectfs mount s3://bucket /mnt/objectfs --config ~/.objectfs/config.yaml
```

</CodeRunner>

## Monitoring

ObjectFS provides comprehensive monitoring capabilities:

### Health Check

<CodeRunner language="bash">

```bash
# Check health status
curl http://localhost:8081/health

# Detailed health information
objectfs health --endpoint http://localhost:8081
```

</CodeRunner>

### Metrics

<CodeRunner language="bash">

```bash
# View metrics
curl http://localhost:9090/metrics

# Using ObjectFS CLI
objectfs metrics --format table
objectfs metrics --format json
```

</CodeRunner>

## Unmounting

When you're done, unmount the filesystem:

<CodeRunner language="bash">

```bash
# Graceful unmount
objectfs unmount /mnt/objectfs

# Force unmount if needed
objectfs unmount /mnt/objectfs --force

# Verify unmount
mount | grep objectfs
```

</CodeRunner>

## Common Issues

### Permission Denied

<CodeRunner language="bash">

```bash
# Ensure proper permissions
sudo usermod -a -G fuse $(whoami)

# Restart session or run
newgrp fuse
```

</CodeRunner>

### Mount Point Busy

<CodeRunner language="bash">

```bash
# Check for active processes
lsof /mnt/objectfs

# Force unmount
sudo fusermount -u /mnt/objectfs
```

</CodeRunner>

### Performance Issues

<CodeRunner language="bash">

```bash
# Increase cache size
objectfs mount s3://bucket /mnt/objectfs --cache-size 16GB

# Enable predictive caching
objectfs mount s3://bucket /mnt/objectfs --enable-predictive-caching

# Check metrics for bottlenecks
objectfs metrics --format table
```

</CodeRunner>

## Next Steps

Now that you have ObjectFS running, explore these advanced features:

- **[Performance Tuning](/guide/performance)**: Optimize ObjectFS for your workload
- **[Distributed Clusters](/guide/distributed)**: Set up multi-node deployments  
- **[Monitoring](/guide/monitoring)**: Configure comprehensive observability
- **[Security](/guide/security)**: Implement authentication and authorization

## Getting Help

If you encounter issues or need help:

- Check our [troubleshooting guide](/guide/troubleshooting)
- Search [GitHub issues](https://github.com/objectfs/objectfs/issues)
- Ask questions on our [community forum](https://community.objectfs.io)
- Review the [API documentation](/api/)

<InteractiveExample>

::: tip
Try the interactive examples above to see ObjectFS in action! Each code block can be executed directly in your browser.
:::

</InteractiveExample>
