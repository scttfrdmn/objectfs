# FUSE Library Migration Plan

## Current State
- Using `github.com/hanwen/go-fuse/v2` 
- Causing macOS compatibility issues
- Enterprise-focused, not research-user-friendly

## Target State  
- Switch to `github.com/winfsp/cgofuse`
- Full cross-platform support (Windows/macOS/Linux)
- Research-user optimized

## Migration Strategy

### Phase 1: Add cgofuse Implementation (Week 1)

```go
// go.mod changes
- github.com/hanwen/go-fuse/v2 v2.4.2
+ github.com/winfsp/cgofuse v1.5.0
```

### Phase 2: Create Platform-Agnostic Interface

```go
// internal/fuse/interface.go
type ObjectFS interface {
    Mount(ctx context.Context) error
    Unmount() error  
    IsMounted() bool
}

// internal/fuse/cgofuse_impl.go  
type CgoFuseFS struct {
    host *cgofuse.FileSystemHost
    // ... implementation
}
```

### Phase 3: Research User Experience

```bash
# macOS - Simple installation
brew install macfuse
go install github.com/objectfs/objectfs/cmd/objectfs@latest
objectfs s3://my-bucket ~/data

# Windows - WinFsp installer + binary
# Download: WinFsp-1.12.msi
# Download: objectfs-windows-amd64.exe
objectfs.exe s3://my-bucket C:\data

# Linux - Native support
sudo apt install fuse3 # or dnf install fuse3
go install github.com/objectfs/objectfs/cmd/objectfs@latest  
objectfs s3://my-bucket ~/data
```

## Benefits for Research Users

### Simplified Installation
- **No kernel compilation**
- **Package manager friendly** 
- **Single binary distribution**
- **Docker images** for consistency

### Academic Workflow Integration
```bash
# Mount genomics data from S3
objectfs s3://1000genomes-bucket ~/genomes

# Access with standard tools
ls ~/genomes/phase3/
samtools view ~/genomes/phase3/sample.bam

# Automatic caching for repeated access
# Background sync for writes
```

### Multi-Platform Lab Support
- **PI's MacBook**: Development and analysis
- **Student Windows laptops**: Access same data  
- **HPC Linux clusters**: High-performance compute
- **Cloud instances**: Scalable processing

## Implementation Priority

1. **High**: cgofuse basic implementation
2. **High**: macOS testing and documentation
3. **Medium**: Windows support and installer
4. **Medium**: Research user documentation
5. **Low**: Performance optimization

## Success Metrics

- ✅ Works on researcher's MacBook (primary)
- ✅ Simple installation (< 5 commands)
- ✅ Compatible with standard tools (ls, cp, samtools, etc.)
- ✅ Reasonable performance (> 100 MB/s for genomics)
- ✅ Handles network interruptions gracefully

## Timeline

- **Week 1**: cgofuse implementation
- **Week 2**: macOS testing and polish  
- **Week 3**: Windows support
- **Week 4**: Documentation and examples

This positions ObjectFS as the **go-to S3 filesystem for research**, competing with tools like s3fs-fuse but with better performance and reliability.