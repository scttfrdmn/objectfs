# Cross-Platform FUSE Support

ObjectFS supports multiple FUSE implementations for maximum compatibility across research environments.

## Platform Support

### Linux (Default)
Uses `hanwen/go-fuse/v2` for optimal performance and latest FUSE protocol support.

```bash
# Install FUSE development headers
sudo apt-get install libfuse-dev  # Ubuntu/Debian
sudo dnf install fuse-devel       # RHEL/CentOS/Fedora

# Build and run
go build ./cmd/objectfs
./objectfs s3://my-bucket ~/data
```

### macOS with cgofuse (Recommended for Research)
Uses `winfsp/cgofuse` for better macOS compatibility.

```bash
# Install macFUSE
brew install --cask macfuse

# Build with cgofuse
go build -tags cgofuse ./cmd/objectfs
./objectfs s3://my-bucket ~/data
```

### Windows (Research Labs)
Uses `winfsp/cgofuse` with WinFsp.

```cmd
REM Install WinFsp from https://winfsp.dev/
REM Download WinFsp-1.12.msi and install

REM Build with cgofuse
go build -tags cgofuse -o objectfs.exe ./cmd/objectfs
objectfs.exe s3://my-bucket C:\data
```

## Build Options

### Default Build (Linux-optimized)
```bash
go build ./cmd/objectfs
```
- Uses hanwen/go-fuse/v2
- Best performance
- Latest FUSE features
- Requires FUSE headers

### Cross-Platform Build (Research-friendly)
```bash
go build -tags cgofuse ./cmd/objectfs
```
- Uses winfsp/cgofuse
- Works on Windows/macOS/Linux
- Simpler installation
- Requires platform-specific FUSE implementation

## Research User Workflow

### Quick Start for Researchers

**macOS (Most Common):**
```bash
# One-time setup
brew install --cask macfuse

# Install ObjectFS
go install -tags cgofuse github.com/objectfs/objectfs/cmd/objectfs@latest

# Mount genomics data
objectfs s3://1000genomes ~/genomes &

# Use with standard tools
ls ~/genomes/
samtools view ~/genomes/sample.bam | head
```

**Windows (Lab Machines):**
```cmd
REM Download and install WinFsp-1.12.msi
REM Download objectfs-windows.exe

objectfs.exe s3://research-data C:\data

REM Use with Windows tools
dir C:\data
type C:\data\results.txt
```

## Performance Comparison

| Platform | FUSE Library | Protocol | Performance | Installation |
|----------|--------------|----------|-------------|--------------|
| Linux    | hanwen/go-fuse/v2 | 7.28 | Excellent | Moderate |
| macOS    | cgofuse | 2.8 | Good | Simple |
| Windows  | cgofuse | N/A | Good | Simple |

## Troubleshooting

### macOS: "fuse.h not found"
```bash
# Install macFUSE first
brew install --cask macfuse

# Restart terminal and try again
go build -tags cgofuse ./cmd/objectfs
```

### Windows: "WinFsp not found"
1. Download WinFsp from https://winfsp.dev/
2. Install WinFsp-1.12.msi
3. Restart command prompt
4. Build with: `go build -tags cgofuse`

### Linux: Permission denied
```bash
# Add user to fuse group
sudo usermod -a -G fuse $USER

# Or run with sudo (not recommended)
sudo ./objectfs s3://bucket ~/mount
```

## Docker Alternative (All Platforms)

For consistent behavior across all platforms:

```bash
# Pull ObjectFS image
docker pull objectfs/objectfs:latest

# Run with volume mounting
docker run --rm --device /dev/fuse --cap-add SYS_ADMIN \
  -v ~/data:/data objectfs/objectfs:latest \
  s3://my-bucket /data
```

This approach guarantees Linux FUSE behavior on all platforms but requires Docker.

## Architecture Decision

ObjectFS prioritizes **research user experience** over enterprise performance:

1. **Default**: hanwen/go-fuse for Linux servers
2. **Research**: cgofuse for cross-platform desktop use
3. **Future**: Automatic platform detection

This strategy serves both high-performance computing environments (Linux) and researcher desktop workflows (macOS/Windows).