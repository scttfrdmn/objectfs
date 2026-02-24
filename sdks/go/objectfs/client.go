package objectfs

import (
	"context"
	"fmt"
	"sync"

	"github.com/objectfs/objectfs/internal/adapter"
	"github.com/objectfs/objectfs/internal/config"
	"github.com/objectfs/objectfs/internal/storage/s3"
	pkgerrors "github.com/objectfs/objectfs/pkg/errors"
	"github.com/objectfs/objectfs/pkg/types"
)

// Client provides access to an ObjectFS S3 bucket.
//
// After construction (New), all object operations (Get, Put, Delete, List, Head)
// are immediately available and communicate directly with S3 — no FUSE required.
//
// Call Mount to layer a POSIX filesystem on top; call Unmount to remove it.
// Close cleans up both the FUSE layer (if mounted) and the S3 backend.
//
// Client is safe for concurrent use.
type Client struct {
	bucket  string
	opts    clientOptions
	backend *s3.Backend
	mu      sync.RWMutex
	adptr   *adapter.Adapter
	mounted bool
}

// New creates a Client connected to the named S3 bucket.
//
// It validates the bucket name, applies options, and performs a lightweight
// S3 health check to confirm credentials and connectivity. The caller must
// call Close when done.
func New(ctx context.Context, bucket string, opts ...Option) (*Client, error) {
	if bucket == "" {
		return nil, pkgerrors.NewError(pkgerrors.ErrCodeInvalidConfig, "bucket name cannot be empty").
			WithComponent("objectfs-sdk").
			WithOperation("New")
	}

	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}

	if err := validateOptions(o); err != nil {
		return nil, err
	}

	s3cfg := &s3.Config{
		Region:   o.region,
		Endpoint: o.endpoint,
	}

	backend, err := s3.NewBackend(ctx, bucket, s3cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialise S3 backend: %w", err)
	}

	return &Client{
		bucket:  bucket,
		opts:    o,
		backend: backend,
	}, nil
}

// validateOptions checks that option values are self-consistent.
func validateOptions(o clientOptions) error {
	if o.maxConcurrency <= 0 {
		return pkgerrors.NewError(pkgerrors.ErrCodeInvalidConfig, "max_concurrency must be greater than 0").
			WithComponent("objectfs-sdk").
			WithOperation("New")
	}
	if o.metricsPort == o.healthPort {
		return pkgerrors.NewError(pkgerrors.ErrCodeInvalidConfig, "metrics_port and health_port cannot be the same").
			WithComponent("objectfs-sdk").
			WithOperation("New")
	}
	return nil
}

// buildConfig constructs a *config.Configuration from the client options.
func (c *Client) buildConfig() *config.Configuration {
	cfg := config.NewDefault()
	cfg.Storage.S3.Region = c.opts.region
	cfg.Storage.S3.Endpoint = c.opts.endpoint
	cfg.Performance.CacheSize = c.opts.cacheSize
	cfg.Performance.MaxConcurrency = c.opts.maxConcurrency
	cfg.Global.LogLevel = c.opts.logLevel
	cfg.Global.MetricsPort = c.opts.metricsPort
	cfg.Global.HealthPort = c.opts.healthPort
	cfg.Security.TLSEnabled = c.opts.tlsEnabled
	return cfg
}

// Get retrieves object data from S3.
//
// offset and size control partial reads: pass 0 for both to fetch the entire object.
// Returns ErrNotFound if the key does not exist.
func (c *Client) Get(ctx context.Context, key string, offset, size int64) ([]byte, error) {
	data, err := c.backend.GetObject(ctx, key, offset, size)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Put stores data under key in S3.
func (c *Client) Put(ctx context.Context, key string, data []byte) error {
	return c.backend.PutObject(ctx, key, data)
}

// Delete removes the object at key from S3.
//
// Deleting a non-existent key is a no-op (returns nil).
func (c *Client) Delete(ctx context.Context, key string) error {
	return c.backend.DeleteObject(ctx, key)
}

// List returns up to limit ObjectInfo entries whose keys begin with prefix.
//
// Pass limit ≤ 0 to retrieve all matching objects (up to S3's page maximum).
func (c *Client) List(ctx context.Context, prefix string, limit int) ([]types.ObjectInfo, error) {
	return c.backend.ListObjects(ctx, prefix, limit)
}

// Head returns metadata for the object at key without fetching its content.
//
// Returns ErrNotFound if the key does not exist.
func (c *Client) Head(ctx context.Context, key string) (*types.ObjectInfo, error) {
	return c.backend.HeadObject(ctx, key)
}

// Mount attaches a POSIX FUSE filesystem at mountPoint, backed by the S3 bucket.
//
// The mountPoint directory must already exist. Mount returns ErrAlreadyMounted
// if the client is already mounted. Object operations (Get, Put, etc.) continue
// to work after mounting.
func (c *Client) Mount(ctx context.Context, mountPoint string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.mounted {
		return pkgerrors.NewError(pkgerrors.ErrCodeAlreadyStarted, "filesystem already mounted").
			WithComponent("objectfs-sdk").
			WithOperation("Mount").
			WithContext("bucket", c.bucket).
			WithContext("mount_point", mountPoint)
	}

	cfg := c.buildConfig()

	adptr, err := adapter.New(ctx, "s3://"+c.bucket, mountPoint, cfg)
	if err != nil {
		return fmt.Errorf("failed to create adapter: %w", err)
	}

	if err := adptr.Start(ctx); err != nil {
		return fmt.Errorf("failed to start adapter: %w", err)
	}

	c.adptr = adptr
	c.mounted = true
	return nil
}

// Unmount detaches the FUSE filesystem.
//
// Returns ErrNotMounted if the client is not currently mounted.
func (c *Client) Unmount() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.mounted {
		return pkgerrors.NewError(pkgerrors.ErrCodeNotInitialized, "filesystem not mounted").
			WithComponent("objectfs-sdk").
			WithOperation("Unmount").
			WithContext("bucket", c.bucket)
	}

	if err := c.adptr.Stop(context.Background()); err != nil {
		return fmt.Errorf("failed to stop adapter: %w", err)
	}

	c.adptr = nil
	c.mounted = false
	return nil
}

// IsMounted reports whether the FUSE filesystem is currently mounted.
func (c *Client) IsMounted() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mounted
}

// Health checks S3 connectivity and returns any detected error.
func (c *Client) Health(ctx context.Context) error {
	return c.backend.HealthCheck(ctx)
}

// Metrics returns a snapshot of the current S3 backend performance metrics.
func (c *Client) Metrics() s3.BackendMetrics {
	return c.backend.GetMetrics()
}

// Close unmounts the filesystem (if mounted) and closes the S3 backend.
//
// Close should always be called when the client is no longer needed.
func (c *Client) Close() error {
	// Unmount calls Lock internally; check IsMounted (RLock) first to avoid
	// calling Unmount when not needed.
	if c.IsMounted() {
		if err := c.Unmount(); err != nil {
			return fmt.Errorf("unmount during close: %w", err)
		}
	}
	if c.backend != nil {
		return c.backend.Close()
	}
	return nil
}
