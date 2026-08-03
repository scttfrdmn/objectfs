package objectfs

import (
	"context"
	"fmt"
	"sync"

	"github.com/scttfrdmn/objectfs/internal/adapter"
	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	pkgerrors "github.com/scttfrdmn/objectfs/pkg/errors"
	"github.com/scttfrdmn/objectfs/pkg/types"
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
		return nil, fmt.Errorf("failed to initialize S3 backend: %w", err)
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
	// Addresses, not ports, and only equality is checked here. Their shape is checked by
	// Configuration.Validate, which Mount reaches — one implementation, so the SDK and a config file
	// cannot come to different conclusions about what a valid address is.
	if o.metricsAddr == o.healthAddr {
		return pkgerrors.NewError(pkgerrors.ErrCodeInvalidConfig,
			"metrics_addr and health_addr cannot be the same; two listeners cannot share one address").
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
	cfg.Monitoring.Metrics.Addr = c.opts.metricsAddr
	cfg.Monitoring.HealthChecks.Addr = c.opts.healthAddr
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
//
// No user metadata is attached. The POSIX attributes ObjectFS records in object metadata belong to
// the filesystem's own view of a file; an object written through this client is not a file, and
// stamping it with a mode and an owner it never had would make a later mount describe it wrongly.
func (c *Client) Put(ctx context.Context, key string, data []byte) error {
	return c.backend.PutObject(ctx, key, data, nil)
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
