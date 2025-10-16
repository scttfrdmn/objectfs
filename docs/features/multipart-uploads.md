# S3 Multipart Upload Optimization

ObjectFS includes comprehensive support for S3 multipart uploads with intelligent chunking,
progress tracking, and resume capability.

## Overview

Multipart uploads allow large files to be uploaded to S3 in chunks, enabling:

- **Parallel uploads** for improved throughput
- **Resume capability** for interrupted uploads
- **Intelligent chunking** based on file size
- **Progress tracking** and metrics
- **Automatic optimization** via CargoShip integration

## Features

### 1. Configurable Thresholds

ObjectFS allows you to configure when multipart uploads are used and how they're chunked:

```yaml
s3:
  multipart_threshold: 33554432    # 32MB - files larger than this use multipart
  multipart_chunk_size: 16777216   # 16MB - default chunk size
  multipart_concurrency: 8         # Number of concurrent part uploads
```

**Default Configuration:**

- **Threshold**: 32MB - Files larger than this trigger multipart uploads
- **Chunk Size**: 16MB - Optimal for most network conditions
- **Concurrency**: 8 - Matches the default connection pool size

### 2. Intelligent Chunking

ObjectFS automatically adjusts chunk sizes based on file size for optimal performance:

| File Size | Chunk Size | Rationale |
|-----------|------------|-----------|
| < 32MB | Full file | Single upload (no multipart) |
| 32-64MB | 8MB | Smaller chunks for files just over threshold |
| 64MB-1GB | 16MB | Standard chunk size |
| 1-10GB | 32MB | Larger chunks for better efficiency |
| 10-100GB | 64MB | Reduced part count |
| > 100GB | 128MB | Maximum practical chunk size |

#### Example Usage

```go
import "github.com/objectfs/objectfs/internal/storage/s3"

cfg := s3.NewDefaultConfig()

// Check if a file should use multipart
fileSize := int64(100 * 1024 * 1024) // 100MB
if cfg.ShouldUseMultipart(fileSize) {
    // Get optimal chunk size for this file
    chunkSize := cfg.GetOptimalChunkSize(fileSize)
    // chunkSize will be 16MB for a 100MB file
}
```

### 3. Upload State Tracking

ObjectFS tracks the state of multipart uploads for monitoring and resume capability:

#### Multipart Upload States

- **`initiated`** - Upload has been started with S3
- **`in_progress`** - Parts are being uploaded
- **`completed`** - All parts uploaded successfully
- **`failed`** - Upload failed
- **`aborted`** - Upload was aborted

#### State Management

```go
// Create a state manager
manager := s3.NewMultipartStateManager()

// Track a new upload
state := s3.NewMultipartUploadState(
    uploadID,
    bucket,
    key,
    totalSize,
    chunkSize,
)
manager.TrackUpload(state)

// Update part status as uploads complete
manager.UpdatePartStatus(uploadID, partNumber, size, etag, nil)

// Check progress
state, _ := manager.GetUploadState(uploadID)
progress := state.GetProgress() // Returns 0-100
```

#### Upload State Features

- **Progress tracking**: Real-time upload progress (0-100%)
- **Part tracking**: Individual part status with ETags
- **Retry tracking**: Number of retry attempts per part
- **Error tracking**: Last error for each failed part
- **Resume support**: Get list of remaining parts to upload

### 4. Metrics and Monitoring

ObjectFS collects comprehensive metrics on multipart uploads:

#### Multipart Metrics

```go
mc := s3.NewMetricsCollector()

// Record multipart operations
mc.RecordMultipartUploadStart()
mc.RecordMultipartUploadPart(partSize int64)
mc.RecordMultipartUploadComplete(totalBytes, duration)
mc.RecordMultipartUploadFailed()

// Get metrics
metrics := mc.GetMetrics()
```

#### Available Metrics

| Metric | Description |
|--------|-------------|
| `multipart_uploads` | Total number of multipart uploads initiated |
| `multipart_uploads_parts` | Total number of parts uploaded |
| `multipart_uploads_completed` | Successfully completed uploads |
| `multipart_uploads_failed` | Failed uploads |
| `multipart_bytes` | Total bytes uploaded via multipart |
| `average_part_size` | Rolling average part size |
| `multipart_latency` | Average multipart upload latency |

#### Calculated Metrics

```go
// Get usage rate (percentage of requests using multipart)
usageRate := mc.GetMultipartUsageRate()

// Get success rate
successRate := mc.GetMultipartSuccessRate()

// Get average parts per upload
avgParts := mc.GetAveragePartsPerUpload()
```

### 5. CargoShip Integration

When CargoShip optimization is enabled, multipart uploads benefit from:

- **BBR/CUBIC congestion control** for optimal throughput
- **Intelligent connection management**
- **Automatic parallelization** based on concurrency settings
- **Network-aware optimization**

```yaml
s3:
  enable_cargoship_optimization: true
  target_throughput: 800.0  # MB/s
  optimization_level: "standard"
  multipart_concurrency: 8  # Used by CargoShip for parallel uploads
```

## Configuration Examples

### High-Performance Configuration

For environments with high bandwidth and large files:

```yaml
s3:
  multipart_threshold: 52428800     # 50MB
  multipart_chunk_size: 33554432    # 32MB
  multipart_concurrency: 16         # Higher concurrency
  enable_cargoship_optimization: true
  target_throughput: 1600.0         # 1.6 GB/s
```

### Conservative Configuration

For environments with limited bandwidth or small files:

```yaml
s3:
  multipart_threshold: 104857600    # 100MB (higher threshold)
  multipart_chunk_size: 8388608     # 8MB (smaller chunks)
  multipart_concurrency: 4          # Lower concurrency
  enable_cargoship_optimization: true
  target_throughput: 200.0          # 200 MB/s
```

### Development/Testing Configuration

For local development with LocalStack or MinIO:

```yaml
s3:
  endpoint: "http://localhost:4566"
  force_path_style: true
  multipart_threshold: 10485760     # 10MB
  multipart_chunk_size: 5242880     # 5MB
  multipart_concurrency: 2
  enable_cargoship_optimization: false
```

## Performance Considerations

### Optimal Chunk Size Selection

The optimal chunk size depends on several factors:

1. **Network Latency**: Higher latency benefits from larger chunks
2. **Bandwidth**: Higher bandwidth can handle larger chunks
3. **File Size**: Larger files should use larger chunks to reduce part count
4. **S3 Limits**: S3 allows up to 10,000 parts per upload

### S3 Multipart Limits

- **Minimum part size**: 5MB (except last part)
- **Maximum part size**: 5GB
- **Maximum parts**: 10,000
- **Maximum object size**: 5TB

ObjectFS automatically respects these limits with intelligent chunking.

### Performance Tips

1. **Use appropriate concurrency**: Match your network capacity
   - 1 Gbps: 8-16 concurrent uploads
   - 10 Gbps: 16-32 concurrent uploads

2. **Enable CargoShip optimization**: Provides 4.6x average performance improvement

3. **Monitor metrics**: Use the metrics API to identify bottlenecks

4. **Tune chunk size**: Larger chunks reduce overhead, but smaller chunks improve parallelization

## Resume Capability

ObjectFS tracks upload state, enabling resume of interrupted uploads:

```go
// Get remaining parts for an interrupted upload
state, exists := manager.GetUploadState(uploadID)
if exists && !state.IsComplete() {
    remaining := state.GetRemainingParts()
    for _, partNum := range remaining {
        // Resume upload for this part
    }
}
```

### Cleanup Old Uploads

Clean up completed or failed uploads after a certain time:

```go
// Remove uploads completed/failed more than 1 hour ago
removed := manager.CleanupOldUploads(1 * time.Hour)
```

## API Reference

### Configuration Functions

```go
// Calculate optimal chunk size for a file
chunkSize := s3.CalculateOptimalChunkSize(fileSize, threshold, baseChunkSize)

// Calculate number of parts needed
partCount := s3.CalculatePartCount(fileSize, chunkSize)

// Check if multipart should be used
shouldUse := cfg.ShouldUseMultipart(fileSize)

// Get optimal chunk size from config
chunkSize := cfg.GetOptimalChunkSize(fileSize)
```

### State Management Functions

```go
// Create state manager
manager := s3.NewMultipartStateManager()

// Create upload state
state := s3.NewMultipartUploadState(uploadID, bucket, key, totalSize, chunkSize)

// Track upload
manager.TrackUpload(state)

// Update part status
manager.UpdatePartStatus(uploadID, partNum, size, etag, err)

// Mark upload complete/failed
manager.MarkUploadCompleted(uploadID)
manager.MarkUploadFailed(uploadID)

// Query uploads
state, exists := manager.GetUploadState(uploadID)
allUploads := manager.GetAllUploads()
inProgress := manager.GetInProgressUploads()

// Cleanup
removed := manager.CleanupOldUploads(maxAge)
manager.RemoveUpload(uploadID)
```

### Metrics Functions

```go
// Create metrics collector
mc := s3.NewMetricsCollector()

// Record operations
mc.RecordMultipartUploadStart()
mc.RecordMultipartUploadPart(size)
mc.RecordMultipartUploadComplete(totalBytes, duration)
mc.RecordMultipartUploadFailed()

// Get metrics
metrics := mc.GetMetrics()
usageRate := mc.GetMultipartUsageRate()
successRate := mc.GetMultipartSuccessRate()
avgParts := mc.GetAveragePartsPerUpload()
```

## Testing

ObjectFS includes comprehensive tests for multipart functionality:

```bash
# Run multipart tests
go test ./internal/storage/s3/... -run TestMultipart

# Run with coverage
go test ./internal/storage/s3/... -cover -coverprofile=coverage.out

# Run benchmarks
go test ./internal/storage/s3/... -bench=BenchmarkMultipart
```

## Troubleshooting

### Common Issues

1. **Parts too small**: S3 requires minimum 5MB per part (except last)
   - Solution: Increase `multipart_chunk_size` to at least 5MB

2. **Too many parts**: S3 limits uploads to 10,000 parts
   - Solution: ObjectFS automatically uses larger chunks for large files

3. **High latency**: Small chunks increase overhead
   - Solution: Increase chunk size for high-latency connections

4. **Low throughput**: Not enough parallelization
   - Solution: Increase `multipart_concurrency` and `pool_size`

### Debugging

Enable debug logging to see multipart upload details:

```go
logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
}))

backend := s3.NewBackend(bucket, cfg, logger)
```

## Future Enhancements

Planned improvements for multipart uploads:

- **Adaptive chunking**: Dynamically adjust chunk size based on network conditions
- **Bandwidth throttling**: Limit upload speed per multipart upload
- **Persistent state**: Save upload state to disk for recovery across restarts
- **Progress callbacks**: User-defined callbacks for upload progress
- **Compression**: Optional compression of parts before upload
- **Encryption**: Client-side encryption of parts

## Related Documentation

- [S3 Backend Configuration](../configuration/s3.md)
- [CargoShip Optimization](./cargoship.md)
- [Performance Tuning Guide](./performance.md)
- [AWS S3 Multipart Upload Documentation](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html)

## See Also

- Transfer Acceleration support (automatic fallback to standard endpoints)
- Connection pooling for efficient resource usage
- Intelligent tiering for cost optimization
- Circuit breaker pattern for resilience
