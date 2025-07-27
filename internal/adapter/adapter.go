package adapter

import (
	"context"
	"fmt"
	"log"
	"net/url"

	"github.com/objectfs/objectfs/internal/config"
)

// Adapter represents the main ObjectFS adapter
type Adapter struct {
	storageURI string
	mountPoint string
	config     *config.Configuration
	
	// Components will be added as we implement them
	// backend     Backend
	// cache       Cache
	// fuse        FUSEServer
	// metrics     MetricsCollector
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

	adapter := &Adapter{
		storageURI: storageURI,
		mountPoint: mountPoint,
		config:     cfg,
	}

	return adapter, nil
}

// Start initializes and starts the adapter
func (a *Adapter) Start(ctx context.Context) error {
	log.Printf("Starting ObjectFS adapter...")
	log.Printf("Storage URI: %s", a.storageURI)
	log.Printf("Mount Point: %s", a.mountPoint)
	log.Printf("Cache Size: %s", a.config.Performance.CacheSize)
	log.Printf("Max Concurrency: %d", a.config.Performance.MaxConcurrency)

	// TODO: Initialize components
	// 1. Initialize S3 backend
	// 2. Initialize cache
	// 3. Initialize write buffer
	// 4. Initialize FUSE filesystem
	// 5. Initialize metrics collector
	// 6. Start health checks
	// 7. Mount filesystem

	log.Printf("ObjectFS adapter started successfully")
	return nil
}

// Stop gracefully stops the adapter
func (a *Adapter) Stop(ctx context.Context) error {
	log.Printf("Stopping ObjectFS adapter...")

	// TODO: Graceful shutdown
	// 1. Unmount filesystem
	// 2. Flush write buffers
	// 3. Close backend connections
	// 4. Stop metrics collection
	// 5. Clean up resources

	log.Printf("ObjectFS adapter stopped successfully")
	return nil
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