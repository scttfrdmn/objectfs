package s3

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ConnectionPool manages a pool of S3 client connections
type ConnectionPool struct {
	mu          sync.RWMutex
	connections chan *s3.Client
	factory     func() (*s3.Client, error)
	maxSize     int
	currentSize int
	closed      bool

	// Health checking
	healthCheck *HealthChecker

	// Statistics
	stats PoolStats
}

// PoolStats tracks connection pool statistics
type PoolStats struct {
	Active      int       `json:"active"`
	Idle        int       `json:"idle"`
	Total       int       `json:"total"`
	MaxSize     int       `json:"max_size"`
	Hits        int64     `json:"hits"`
	Misses      int64     `json:"misses"`
	Timeouts    int64     `json:"timeouts"`
	Errors      int64     `json:"errors"`
	Created     int64     `json:"created"`
	Destroyed   int64     `json:"destroyed"`
	LastCreated time.Time `json:"last_created"`
	LastError   string    `json:"last_error"`
	LastErrorAt time.Time `json:"last_error_at"`
}

// HealthChecker monitors connection health
type HealthChecker struct {
	pool     *ConnectionPool
	interval time.Duration
	timeout  time.Duration
	stopCh   chan struct{}
	stopped  chan struct{}
}

// NewConnectionPool creates a new connection pool
func NewConnectionPool(maxSize int, factory func() (*s3.Client, error)) (*ConnectionPool, error) {
	if maxSize <= 0 {
		maxSize = 8 // Default pool size
	}

	if factory == nil {
		return nil, fmt.Errorf("connection factory cannot be nil")
	}

	pool := &ConnectionPool{
		connections: make(chan *s3.Client, maxSize),
		factory:     factory,
		maxSize:     maxSize,
		stats: PoolStats{
			MaxSize: maxSize,
		},
	}

	// Initialize health checker
	pool.healthCheck = &HealthChecker{
		pool:     pool,
		interval: 30 * time.Second,
		timeout:  5 * time.Second,
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
	}

	// Start health checker
	go pool.healthCheck.run()

	return pool, nil
}

// Get retrieves a connection from the pool
func (p *ConnectionPool) Get() *s3.Client {
	return p.GetWithTimeout(30 * time.Second)
}

// GetWithTimeout retrieves a connection with a timeout
func (p *ConnectionPool) GetWithTimeout(timeout time.Duration) *s3.Client {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return nil
	}
	p.mu.RUnlock()

	select {
	case conn := <-p.connections:
		p.mu.Lock()
		p.stats.Hits++
		p.stats.Active++
		p.mu.Unlock()
		return conn

	case <-time.After(timeout):
		p.mu.Lock()
		p.stats.Timeouts++
		p.mu.Unlock()

		// Try to create a new connection
		client, err := p.factory()
		if err != nil {
			return nil
		}
		return client

	default:
		// Try to create a new connection
		if p.canCreateConnection() {
			conn, err := p.createConnection()
			if err == nil {
				return conn
			}

			p.mu.Lock()
			p.stats.Errors++
			p.stats.LastError = err.Error()
			p.stats.LastErrorAt = time.Now()
			p.mu.Unlock()
		}

		p.mu.Lock()
		p.stats.Misses++
		p.mu.Unlock()

		// If we can't create or get from pool, return nil
		return nil
	}
}

// Put returns a connection to the pool
func (p *ConnectionPool) Put(conn *s3.Client) {
	if conn == nil {
		return
	}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return
	}
	p.mu.RUnlock()

	select {
	case p.connections <- conn:
		p.mu.Lock()
		p.stats.Active--
		p.mu.Unlock()
	default:
		// Pool is full, discard the connection
		p.mu.Lock()
		p.stats.Destroyed++
		p.currentSize--
		p.mu.Unlock()
	}
}

// Stats returns current pool statistics
func (p *ConnectionPool) Stats() PoolStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stats := p.stats
	stats.Total = p.currentSize
	stats.Idle = len(p.connections)

	return stats
}

// Close closes the connection pool
func (p *ConnectionPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	// Stop health checker
	close(p.healthCheck.stopCh)
	<-p.healthCheck.stopped

	// Close all connections in the pool
	close(p.connections)
	for conn := range p.connections {
		_ = conn // S3 client doesn't need explicit close
	}

	return nil
}

// Resize changes the maximum pool size
func (p *ConnectionPool) Resize(newSize int) error {
	if newSize <= 0 {
		return fmt.Errorf("pool size must be positive")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return fmt.Errorf("pool is closed")
	}

	oldSize := p.maxSize
	p.maxSize = newSize
	p.stats.MaxSize = newSize

	// If shrinking, we may need to drain excess connections
	if newSize < oldSize {
		excess := len(p.connections) - newSize
	drainLoop:
		for i := 0; i < excess; i++ {
			select {
			case <-p.connections:
				p.currentSize--
				p.stats.Destroyed++
			default:
				break drainLoop
			}
		}
	}

	return nil
}

// Warmup pre-fills the pool with connections
func (p *ConnectionPool) Warmup(ctx context.Context, count int) error {
	if count <= 0 {
		count = p.maxSize
	}

	var errors []error
warmupLoop:
	for i := 0; i < count && i < p.maxSize; i++ {
		conn, err := p.createConnection()
		if err != nil {
			errors = append(errors, err)
			continue
		}

		select {
		case p.connections <- conn:
			// Successfully added to pool
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Pool is full
			break warmupLoop
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("warmup partially failed: %d errors", len(errors))
	}

	return nil
}

// Helper methods

func (p *ConnectionPool) canCreateConnection() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.currentSize < p.maxSize && !p.closed
}

func (p *ConnectionPool) createConnection() (*s3.Client, error) {
	conn, err := p.factory()
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.currentSize++
	p.stats.Created++
	p.stats.Active++
	p.stats.LastCreated = time.Now()
	p.mu.Unlock()

	return conn, nil
}

// Health checker implementation

func (hc *HealthChecker) run() {
	defer close(hc.stopped)

	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-hc.stopCh:
			return
		case <-ticker.C:
			hc.checkHealth()
		}
	}
}

func (hc *HealthChecker) checkHealth() {
	// Get a sample of connections to test
	testCount := 3
	if hc.pool.Stats().Idle < testCount {
		testCount = hc.pool.Stats().Idle
	}

	var unhealthyCount int
	for i := 0; i < testCount; i++ {
		conn := hc.pool.GetWithTimeout(1 * time.Second)
		if conn == nil {
			continue
		}

		healthy := hc.testConnection(conn)
		if !healthy {
			unhealthyCount++
			// Don't put unhealthy connection back
			hc.pool.mu.Lock()
			hc.pool.currentSize--
			hc.pool.stats.Destroyed++
			hc.pool.mu.Unlock()
		} else {
			hc.pool.Put(conn)
		}
	}

	// If too many connections are unhealthy, we might want to recreate some
	if unhealthyCount > testCount/2 {
		// Log warning or trigger pool recreation
		hc.pool.mu.Lock()
		hc.pool.stats.LastError = fmt.Sprintf("Found %d unhealthy connections", unhealthyCount)
		hc.pool.stats.LastErrorAt = time.Now()
		hc.pool.mu.Unlock()
	}
}

func (hc *HealthChecker) testConnection(conn *s3.Client) bool {
	ctx, cancel := context.WithTimeout(context.Background(), hc.timeout)
	defer cancel()

	// Simple health check - list buckets (requires minimal permissions)
	// In a real implementation, you might want a more specific health check
	_, err := conn.ListBuckets(ctx, &s3.ListBucketsInput{})
	return err == nil
}
