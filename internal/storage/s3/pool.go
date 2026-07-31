package s3

import (
	"context"
	stderrors "errors"
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

	// done is closed by Close. A Get blocked waiting for a connection selects on it so
	// shutdown does not make callers wait out their full timeout.
	done chan struct{}

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
		done:        make(chan struct{}),
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

// Get retrieves a connection from the pool, waiting up to 30 seconds for one to become available.
//
// It returns an error rather than a nil client. The previous signature returned *s3.Client alone and
// yielded nil once currentSize reached maxSize, which every one of its six call sites dereferenced
// unchecked — including the path taken by every GET and PUT. The ninth concurrent operation on a
// default 8-connection pool panicked and unmounted the filesystem under every open descriptor.
func (p *ConnectionPool) Get() (*s3.Client, error) {
	return p.GetWithTimeout(30 * time.Second)
}

// GetWithTimeout retrieves a connection, waiting up to timeout for one.
//
// The order is: take an idle connection, else create one if the pool is below its limit, else wait
// for a connection to be returned. That last arm is the one that was missing — the old
// implementation had a `default` clause, so `select` never reached its `time.After` case and a full
// pool failed instantly instead of waiting for the connection that was about to come back.
func (p *ConnectionPool) GetWithTimeout(timeout time.Duration) (*s3.Client, error) {
	p.mu.RLock()
	closed := p.closed
	p.mu.RUnlock()
	if closed {
		return nil, fmt.Errorf("connection pool is closed")
	}

	// An idle connection, if one is waiting.
	select {
	case conn := <-p.connections:
		p.mu.Lock()
		p.stats.Hits++
		p.stats.Active++
		p.mu.Unlock()

		return conn, nil
	default:
	}

	// Room to grow. The slot is reserved and released as one operation each, so N concurrent
	// callers against a pool of M create at most M connections. Checking the limit and then
	// incrementing under a second lock — as this did — let all N pass the check before any of
	// them incremented, so 16 concurrent readers built 16 clients for a pool of 4 and threw 12
	// of them away on return.
	if p.reserveSlot() {
		conn, err := p.factory()
		if err != nil {
			p.mu.Lock()
			p.currentSize-- // release the reservation
			p.stats.Errors++
			p.stats.LastError = err.Error()
			p.stats.LastErrorAt = time.Now()
			p.mu.Unlock()

			return nil, fmt.Errorf("create S3 connection: %w", err)
		}

		p.mu.Lock()
		p.stats.Created++
		p.stats.Active++
		p.stats.LastCreated = time.Now()
		p.mu.Unlock()

		return conn, nil
	}

	// At the limit, so wait for one to be returned. Blocking here is correct: the caller needs a
	// client, and the alternative is failing an operation that would have succeeded a millisecond
	// later.
	p.mu.Lock()
	p.stats.Misses++
	p.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case conn := <-p.connections:
		p.mu.Lock()
		p.stats.Hits++
		p.stats.Active++
		p.mu.Unlock()

		return conn, nil

	case <-p.done:
		// Shutdown while waiting. Fail immediately rather than making the caller wait out
		// the timeout for a connection that is never coming.
		return nil, fmt.Errorf("connection pool is closed")

	case <-timer.C:
		p.mu.Lock()
		p.stats.Timeouts++
		size, limit := p.currentSize, p.maxSize
		p.mu.Unlock()

		return nil, fmt.Errorf("timed out after %s waiting for an S3 connection (%d/%d in use); "+
			"raise storage.s3.pool_size", timeout, size, limit)
	}
}

// Put returns a connection to the pool. A nil connection is ignored, so a caller may defer Put
// unconditionally alongside a Get that failed.
//
// The check of closed and the send are one critical section under the write lock. Close takes the
// same lock, so a Put either observes closed and drops the connection or completes its send while
// Close waits — it can never send to a pool that has finished draining. Splitting the two, as this
// once did, is a check-then-act race that panicked on shutdown.
func (p *ConnectionPool) Put(conn *s3.Client) {
	if conn == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		// The connection is dropped; an S3 client needs no teardown. currentSize is not
		// decremented because Close already zeroed the pool's accounting.
		return
	}

	p.stats.Active--

	// A Resize down while this connection was checked out leaves the pool over its new limit.
	// Drop the connection rather than keep it, which is how the pool converges on the new size.
	if p.currentSize > p.maxSize {
		p.currentSize--
		p.stats.Destroyed++

		return
	}

	select {
	case p.connections <- conn:
	default:
		// The buffer holds maxSize and currentSize never exceeds it, so this is unreachable.
		// Discarding is the safe response: blocking here would hold the write lock and deadlock
		// a caller that is merely returning a connection.
		p.stats.Destroyed++
		p.currentSize--
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

// Close shuts the pool down. It is idempotent.
//
// The channel is drained rather than closed. Closing it would be a data race against any Put still
// in flight — and since Put performs its closed check and its send under the same write lock this
// takes, draining is sufficient: after Close returns, no Put can succeed. An S3 client needs no
// explicit teardown, so the drained connections are simply discarded.
func (p *ConnectionPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true

	// Drain while holding the write lock, which excludes every Put.
	for drained := true; drained; {
		select {
		case <-p.connections:
			p.currentSize--
			p.stats.Destroyed++
		default:
			drained = false
		}
	}

	// Wakes any Get already blocked on the wait arm.
	close(p.done)
	p.mu.Unlock()

	// Stop the health checker after releasing the lock: checkHealth calls GetWithTimeout, which takes
	// the read lock, so signaling it while holding the write lock would deadlock.
	close(p.healthCheck.stopCh)
	<-p.healthCheck.stopped

	return nil
}

// Resize changes the maximum number of connections the pool will hold.
//
// It cannot grow past the size the pool was built with: the buffer of the idle channel is fixed at
// construction, and every reservation made by GetWithTimeout and Warmup relies on that buffer having
// room for the connection it is about to produce. Raising maxSize above the buffer would make a
// return block while holding the write lock — a deadlock, not a bigger pool. Growing for real means
// a new channel, so this refuses instead of pretending, and the caller is told to raise
// storage.s3.pool_size and restart.
//
// Shrinking discards idle connections until the total is within the new limit. Connections that are
// currently checked out are left alone; they are dropped by Put once they come back.
func (p *ConnectionPool) Resize(newSize int) error {
	if newSize <= 0 {
		return fmt.Errorf("pool size must be positive, got %d", newSize)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return fmt.Errorf("pool is closed")
	}

	if capacity := cap(p.connections); newSize > capacity {
		return fmt.Errorf("cannot grow the pool from %d to %d: its capacity is fixed at %d; "+
			"raise storage.s3.pool_size and restart", p.maxSize, newSize, capacity)
	}

	p.maxSize = newSize
	p.stats.MaxSize = newSize

	// Discard idle connections until the total fits the new limit.
	for p.currentSize > newSize {
		select {
		case <-p.connections:
			p.currentSize--
			p.stats.Destroyed++
		default:
			// The remainder are checked out. Put drops them on return.
			return nil
		}
	}

	return nil
}

// Warmup pre-fills the pool so the first requests find idle connections instead of constructing
// them. Connections it adds are idle, not active: a caller accounts for one only when it draws it.
//
// A count above the pool's size warms the whole pool; a count of zero or less means the whole pool.
func (p *ConnectionPool) Warmup(ctx context.Context, count int) error {
	p.mu.RLock()
	closed, maxSize := p.closed, p.maxSize
	p.mu.RUnlock()

	if closed {
		return fmt.Errorf("connection pool is closed")
	}

	if count <= 0 || count > maxSize {
		count = maxSize
	}

	var errs []error

	for range count {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// reserveSlot, not a bare increment: warming must respect maxSize and must stop if the
		// pool closed or filled underneath it.
		if !p.reserveSlot() {
			break
		}

		conn, err := p.factory()
		if err != nil {
			p.mu.Lock()
			p.currentSize--
			p.stats.Errors++
			p.stats.LastError = err.Error()
			p.stats.LastErrorAt = time.Now()
			p.mu.Unlock()

			errs = append(errs, err)

			continue
		}

		p.mu.Lock()
		if p.closed {
			// Closed while the factory ran. Drop the connection rather than send to a
			// drained pool.
			p.currentSize--
			p.mu.Unlock()

			return fmt.Errorf("connection pool is closed")
		}
		p.stats.Created++
		p.stats.LastCreated = time.Now()
		p.connections <- conn // cannot block: the reservation guarantees buffer space
		p.mu.Unlock()
	}

	if len(errs) > 0 {
		return fmt.Errorf("warmup partially failed: %d of %d connections: %w",
			len(errs), count, stderrors.Join(errs...))
	}

	return nil
}

// Helper methods

// reserveSlot claims capacity for one new connection, incrementing currentSize under the same lock
// that tested it. A caller that gets true owns the slot and must either produce a connection or
// decrement currentSize.
func (p *ConnectionPool) reserveSlot() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed || p.currentSize >= p.maxSize {
		return false
	}
	p.currentSize++

	return true
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
	testCount := min(hc.pool.Stats().Idle, 3)

	var unhealthyCount int
	for range testCount {
		conn, err := hc.pool.GetWithTimeout(1 * time.Second)
		if err != nil {
			// A saturated or closed pool is not a health signal about any individual
			// connection, so it is not counted as unhealthy.
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
