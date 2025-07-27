package adapter

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/objectfs/objectfs/internal/buffer"
	"github.com/objectfs/objectfs/internal/cache"
	"github.com/objectfs/objectfs/internal/config"
	"github.com/objectfs/objectfs/internal/fuse"
	"github.com/objectfs/objectfs/internal/metrics"
	"github.com/objectfs/objectfs/internal/storage/s3"
)

// Adapter represents the main ObjectFS adapter
type Adapter struct {
	storageURI string
	mountPoint string
	config     *config.Configuration
	
	// Core components
	backend     *s3.Backend
	cache       *cache.MultiLevelCache
	writeBuffer *buffer.WriteBuffer
	mountMgr    fuse.PlatformFileSystem
	metrics     *metrics.Collector
	
	// Internal state
	started     bool
	bucketName  string
	s3Config    *s3.Config
}

// New creates a new ObjectFS adapter instance
func New(ctx context.Context, storageURI, mountPoint string, cfg *config.Configuration) (*Adapter, error) {
	// Validate storage URI
	if err := validateStorageURI(storageURI); err != nil {
		return nil, fmt.Errorf("invalid storage URI: %w", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	// Parse S3 URI to extract bucket name
	parsed, err := url.Parse(storageURI)
	if err != nil {
		return nil, fmt.Errorf("failed to parse storage URI: %w", err)
	}

	bucketName := strings.TrimPrefix(parsed.Host, "")
	if bucketName == "" {
		return nil, fmt.Errorf("bucket name cannot be empty")
	}

	adapter := &Adapter{
		storageURI: storageURI,
		mountPoint: mountPoint,
		config:     cfg,
		bucketName: bucketName,
	}

	return adapter, nil
}

// Start initializes and starts the adapter
func (a *Adapter) Start(ctx context.Context) error {
	if a.started {
		return fmt.Errorf("adapter already started")
	}

	log.Printf("Starting ObjectFS adapter...")
	log.Printf("Storage URI: %s", a.storageURI)
	log.Printf("Mount Point: %s", a.mountPoint)
	log.Printf("Cache Size: %s", a.config.Performance.CacheSize)
	log.Printf("Max Concurrency: %d", a.config.Performance.MaxConcurrency)

	// 1. Initialize metrics collector
	var err error
	a.metrics, err = metrics.NewCollector(&metrics.Config{
		Enabled: a.config.Monitoring.Metrics.Enabled,
		Port:    a.config.Global.MetricsPort,
		Labels:  a.config.Monitoring.Metrics.CustomLabels,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize metrics collector: %w", err)
	}

	// 2. Initialize S3 backend
	a.s3Config = &s3.Config{
		Region:   "us-west-2", // Default, should be configurable
		Endpoint: "",          // Use default AWS endpoint
	}

	a.backend, err = s3.NewBackend(ctx, a.bucketName, a.s3Config)
	if err != nil {
		return fmt.Errorf("failed to initialize S3 backend: %w", err)
	}

	// 3. Initialize cache system
	cacheConfig := &cache.MultiLevelConfig{
		L1Config: &cache.L1Config{
			Enabled:    true,
			Size:       parseSize(a.config.Performance.CacheSize),
			MaxEntries: a.config.Cache.MaxEntries,
			TTL:        a.config.Cache.TTL,
			Prefetch:   true,
		},
		L2Config: &cache.L2Config{
			Enabled:     a.config.Cache.PersistentCache.Enabled,
			Size:        parseSize(a.config.Cache.PersistentCache.MaxSize),
			Directory:   a.config.Cache.PersistentCache.Directory,
			TTL:         a.config.Cache.TTL,
			Compression: true,
		},
		Policy: a.config.Cache.EvictionPolicy,
	}
	
	a.cache, err = cache.NewMultiLevelCache(cacheConfig)
	if err != nil {
		return fmt.Errorf("failed to initialize cache: %w", err)
	}

	// 4. Initialize write buffer - use simple WriteBuffer for now
	writeBufferConfig := &buffer.WriteBufferConfig{
		MaxBufferSize:  int64(parseSize(a.config.WriteBuffer.MaxMemory) / 100), // Reasonable default
		FlushThreshold: int64(parseSize(a.config.WriteBuffer.MaxMemory) / 200),
		AsyncFlush:     true,
		MaxWriteDelay:  a.config.WriteBuffer.FlushInterval,
	}

	// Create a simple flush callback that writes to S3
	flushCallback := func(key string, data []byte, offset int64) error {
		return a.backend.PutObject(ctx, key, data)
	}

	a.writeBuffer, err = buffer.NewWriteBuffer(writeBufferConfig, flushCallback)
	if err != nil {
		return fmt.Errorf("failed to initialize write buffer: %w", err)
	}

	// 5. Initialize platform-specific FUSE filesystem
	mountConfig := &fuse.MountConfig{
		MountPoint: a.mountPoint,
		Options: &fuse.MountOptions{
			FSName:   "objectfs",
			Subtype:  "s3",
			MaxRead:  128 * 1024,
			MaxWrite: 128 * 1024,
			Debug:    false,
		},
	}

	a.mountMgr = fuse.CreatePlatformMountManager(a.backend, a.cache, a.writeBuffer, a.metrics, mountConfig)

	// 7. Initialize health monitor (simplified for now)
	// TODO: Implement proper health monitoring when components are ready

	// 8. Mount filesystem
	if err := a.mountMgr.Mount(ctx); err != nil {
		return fmt.Errorf("failed to mount filesystem: %w", err)
	}

	a.started = true
	log.Printf("ObjectFS adapter started successfully")
	return nil
}

// Stop gracefully stops the adapter
func (a *Adapter) Stop(ctx context.Context) error {
	if !a.started {
		return fmt.Errorf("adapter not started")
	}

	log.Printf("Stopping ObjectFS adapter...")

	var lastErr error

	// 1. Unmount filesystem
	if a.mountMgr != nil && a.mountMgr.IsMounted() {
		if err := a.mountMgr.Unmount(); err != nil {
			log.Printf("Error unmounting filesystem: %v", err)
			lastErr = err
		}
	}

	// 2. Flush write buffers
	if a.writeBuffer != nil {
		if err := a.writeBuffer.FlushAll(); err != nil {
			log.Printf("Error flushing write buffers: %v", err)
			lastErr = err
		}
		if err := a.writeBuffer.Close(); err != nil {
			log.Printf("Error closing write buffer: %v", err)
			lastErr = err
		}
	}

	// 3. Close backend connections
	if a.backend != nil {
		if err := a.backend.Close(); err != nil {
			log.Printf("Error closing backend: %v", err)
			lastErr = err
		}
	}

	// 4. Clear cache (simplified)
	// TODO: Implement proper cache clearing when available

	// 5. Stop metrics collection (simplified)
	// TODO: Implement proper metrics stopping

	a.started = false
	log.Printf("ObjectFS adapter stopped successfully")
	return lastErr
}

// validateStorageURI validates the storage URI format
func validateStorageURI(uri string) error {
	parsed, err := url.Parse(uri)
	if err != nil {
		return fmt.Errorf("failed to parse URI: %w", err)
	}

	switch parsed.Scheme {
	case "s3":
		if parsed.Host == "" {
			return fmt.Errorf("S3 URI must include bucket name")
		}
	default:
		return fmt.Errorf("unsupported storage scheme: %s (only s3:// supported)", parsed.Scheme)
	}

	return nil
}

// parseSize parses a human-readable size string (e.g., "2GB", "512MB") to bytes
func parseSize(sizeStr string) int64 {
	// Simple implementation - in practice you'd use a proper parsing library
	sizeStr = strings.ToUpper(strings.TrimSpace(sizeStr))
	
	var multiplier int64 = 1
	var numStr string
	
	if strings.HasSuffix(sizeStr, "GB") {
		multiplier = 1024 * 1024 * 1024
		numStr = strings.TrimSuffix(sizeStr, "GB")
	} else if strings.HasSuffix(sizeStr, "MB") {
		multiplier = 1024 * 1024
		numStr = strings.TrimSuffix(sizeStr, "MB")
	} else if strings.HasSuffix(sizeStr, "KB") {
		multiplier = 1024
		numStr = strings.TrimSuffix(sizeStr, "KB")
	} else if strings.HasSuffix(sizeStr, "B") {
		multiplier = 1
		numStr = strings.TrimSuffix(sizeStr, "B")
	} else {
		numStr = sizeStr
	}
	
	// Parse the numeric part
	var num int64 = 1024 * 1024 * 1024 // Default 1GB
	if numStr != "" {
		if parsed, err := fmt.Sscanf(numStr, "%d", &num); err != nil || parsed != 1 {
			return 1024 * 1024 * 1024 // Default 1GB on error
		}
	}
	
	return num * multiplier
}