package recovery

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/errors"
	"github.com/scttfrdmn/objectfs/pkg/utils"
)

// ConnectionState represents the state of a managed connection
type ConnectionState int

const (
	// StateDisconnected indicates no active connection
	StateDisconnected ConnectionState = iota

	// StateConnecting indicates connection attempt in progress
	StateConnecting

	// StateConnected indicates active healthy connection
	StateConnected

	// StateReconnecting indicates reconnection attempt in progress
	StateReconnecting

	// StateFailed indicates connection failed and won't retry
	StateFailed
)

// String returns the string representation of connection state
func (s ConnectionState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateReconnecting:
		return "reconnecting"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// ConnectionConfig configures connection management behavior
type ConnectionConfig struct {
	// ConnectionTimeout is the timeout for establishing a connection
	ConnectionTimeout time.Duration

	// ReconnectDelay is the initial delay before reconnection attempt
	ReconnectDelay time.Duration

	// MaxReconnectDelay is the maximum delay between reconnection attempts
	MaxReconnectDelay time.Duration

	// ReconnectBackoffMultiplier increases delay after each failed attempt
	ReconnectBackoffMultiplier float64

	// MaxReconnectAttempts limits reconnection attempts (0 = unlimited)
	MaxReconnectAttempts int

	// HealthCheckInterval is how often to check connection health
	HealthCheckInterval time.Duration

	// HealthCheckTimeout is the timeout for health checks
	HealthCheckTimeout time.Duration

	// EnableAutoReconnect enables automatic reconnection on failure
	EnableAutoReconnect bool

	// Logger for connection events
	Logger *utils.StructuredLogger
}

// DefaultConnectionConfig returns sensible defaults
func DefaultConnectionConfig() ConnectionConfig {
	return ConnectionConfig{
		ConnectionTimeout:          30 * time.Second,
		ReconnectDelay:             1 * time.Second,
		MaxReconnectDelay:          60 * time.Second,
		ReconnectBackoffMultiplier: 2.0,
		MaxReconnectAttempts:       10,
		HealthCheckInterval:        30 * time.Second,
		HealthCheckTimeout:         5 * time.Second,
		EnableAutoReconnect:        true,
	}
}

// ConnectionFactory creates new connections
type ConnectionFactory func(ctx context.Context) (any, error)

// HealthChecker checks if a connection is healthy
type HealthChecker func(ctx context.Context, conn any) error

// ConnectionManager manages connections with automatic reconnection
type ConnectionManager struct {
	name    string
	config  ConnectionConfig
	factory ConnectionFactory
	health  HealthChecker
	logger  *utils.StructuredLogger

	mu               sync.RWMutex
	state            ConnectionState
	connection       any
	connectedAt      time.Time
	lastError        error
	reconnectAttempt atomic.Int32
	reconnectDelay   time.Duration

	// healthLoopStarted guards the health-check goroutine, which was started by every successful
	// Connect. Connect is called by Reconnect and by the scheduler on every automatic attempt, so
	// loops accumulated: measured at 58 probes where one loop gives ~10, after six Connect calls at
	// a 20ms interval. Each of those loops also held a shutdownWg slot, and each independently
	// diagnosed a failure and called scheduleReconnect — so a single broken connection produced N
	// concurrent reconnection schedules, each incrementing reconnectAttempt toward
	// MaxReconnectAttempts.
	healthLoopStarted atomic.Bool

	shutdownCh chan struct{}
	shutdownWg sync.WaitGroup
	shutdown   atomic.Int32
}

// ConnectionStats provides connection statistics
type ConnectionStats struct {
	Name             string          `json:"name"`
	State            ConnectionState `json:"state"`
	Connected        bool            `json:"connected"`
	ConnectedAt      *time.Time      `json:"connected_at,omitempty"`
	Uptime           time.Duration   `json:"uptime"`
	ReconnectAttempt int             `json:"reconnect_attempt"`
	LastError        string          `json:"last_error,omitempty"`
}

// NewConnectionManager creates a new connection manager
func NewConnectionManager(name string, config ConnectionConfig, factory ConnectionFactory, health HealthChecker) *ConnectionManager {
	if config.Logger == nil {
		loggerConfig := utils.DefaultStructuredLoggerConfig()
		logger, _ := utils.NewStructuredLogger(loggerConfig)
		config.Logger = logger
	}

	cm := &ConnectionManager{
		name:           name,
		config:         config,
		factory:        factory,
		health:         health,
		logger:         config.Logger,
		state:          StateDisconnected,
		reconnectDelay: config.ReconnectDelay,
		shutdownCh:     make(chan struct{}),
	}

	return cm
}

// Connect establishes the initial connection
func (cm *ConnectionManager) Connect(ctx context.Context) error {
	cm.mu.Lock()
	if cm.state == StateConnected {
		cm.mu.Unlock()
		return nil
	}

	if cm.shutdown.Load() == 1 {
		cm.mu.Unlock()
		return errors.NewError(errors.ErrCodeShutdownInProgress, "connection manager is shutting down")
	}

	cm.state = StateConnecting
	cm.mu.Unlock()

	// The context background work descends from: the caller's values, none of its cancellation.
	//
	// The automatic reconnect below and every probe the health loop makes used to build their own
	// context from context.Background(), so nothing a caller of Connect attached — a trace ID, a
	// deadline-bearing parent, a test's context — was visible to work this type does on that caller's
	// behalf. Passing ctx itself is the other wrong answer: Connect is a call that returns, background
	// reconnection and health checking outlive it by design, and Close is what ends them.
	bg := context.WithoutCancel(ctx)

	cm.logger.Info("Establishing connection", map[string]any{
		"name": cm.name,
	})

	// Create connection with timeout
	connCtx, cancel := context.WithTimeout(ctx, cm.config.ConnectionTimeout)
	defer cancel()

	conn, err := cm.factory(connCtx)
	if err != nil {
		cm.mu.Lock()
		cm.state = StateDisconnected
		cm.lastError = err
		cm.mu.Unlock()

		cm.logger.Error("Connection failed", map[string]any{
			"name":  cm.name,
			"error": err.Error(),
		})

		// Start automatic reconnection if enabled
		if cm.config.EnableAutoReconnect {
			cm.scheduleReconnect(bg)
		}

		return errors.NewError(errors.ErrCodeConnectionFailed, "failed to establish connection").
			WithComponent(cm.name).
			WithCause(err)
	}

	cm.mu.Lock()
	cm.connection = conn
	cm.state = StateConnected
	cm.connectedAt = time.Now()
	cm.lastError = nil
	cm.reconnectAttempt.Store(0)
	cm.reconnectDelay = cm.config.ReconnectDelay
	cm.mu.Unlock()

	cm.logger.Info("Connection established", map[string]any{
		"name": cm.name,
	})

	// Start health check monitoring, once. CompareAndSwap and not a plain check: Connect runs
	// concurrently with itself — the reconnect scheduler calls it from its own goroutine while a
	// caller may call it directly — and two loops is exactly what this prevents.
	if cm.config.HealthCheckInterval > 0 && cm.health != nil && cm.healthLoopStarted.CompareAndSwap(false, true) {
		cm.shutdownWg.Add(1)
		go cm.healthCheckLoop(bg)
	}

	return nil
}

// GetConnection returns the current connection
func (cm *ConnectionManager) GetConnection() (any, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if cm.state != StateConnected {
		return nil, errors.NewError(errors.ErrCodeConnectionFailed, "not connected").
			WithComponent(cm.name).
			WithContext("state", cm.state.String())
	}

	return cm.connection, nil
}

// IsConnected returns true if currently connected
func (cm *ConnectionManager) IsConnected() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state == StateConnected
}

// GetState returns the current connection state
func (cm *ConnectionManager) GetState() ConnectionState {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state
}

// GetStats returns connection statistics
func (cm *ConnectionManager) GetStats() ConnectionStats {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	stats := ConnectionStats{
		Name:             cm.name,
		State:            cm.state,
		Connected:        cm.state == StateConnected,
		ReconnectAttempt: int(cm.reconnectAttempt.Load()),
	}

	if !cm.connectedAt.IsZero() {
		stats.ConnectedAt = &cm.connectedAt
		if cm.state == StateConnected {
			stats.Uptime = time.Since(cm.connectedAt)
		}
	}

	if cm.lastError != nil {
		stats.LastError = cm.lastError.Error()
	}

	return stats
}

// Reconnect manually triggers a reconnection
func (cm *ConnectionManager) Reconnect(ctx context.Context) error {
	cm.logger.Info("Manual reconnection triggered", map[string]any{
		"name": cm.name,
	})

	cm.mu.Lock()
	cm.closeConnection()
	cm.state = StateDisconnected
	cm.mu.Unlock()

	return cm.Connect(ctx)
}

// scheduleReconnect schedules an automatic reconnection attempt.
//
// parent is the caller's context with its cancellation removed — see Connect. The reconnect it
// eventually performs derives a ConnectionTimeout from it; the wait before that is bounded by
// shutdownCh, not by parent, because Close is what cancels a pending reconnect.
func (cm *ConnectionManager) scheduleReconnect(parent context.Context) {
	attempt := cm.reconnectAttempt.Add(1)

	// Check if we've exceeded max attempts
	if cm.config.MaxReconnectAttempts > 0 && int(attempt) > cm.config.MaxReconnectAttempts {
		cm.mu.Lock()
		cm.state = StateFailed
		cm.mu.Unlock()

		cm.logger.Error("Maximum reconnection attempts exceeded", map[string]any{
			"name":     cm.name,
			"attempts": attempt,
		})
		return
	}

	cm.mu.Lock()
	delay := cm.reconnectDelay
	// Increase delay for next attempt
	cm.reconnectDelay = min(time.Duration(float64(cm.reconnectDelay)*cm.config.ReconnectBackoffMultiplier), cm.config.MaxReconnectDelay)
	cm.mu.Unlock()

	cm.logger.Info("Scheduling reconnection", map[string]any{
		"name":    cm.name,
		"attempt": attempt,
		"delay":   delay,
	})

	cm.shutdownWg.Go(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
			if cm.shutdown.Load() == 1 {
				return
			}

			cm.mu.Lock()
			cm.state = StateReconnecting
			cm.mu.Unlock()

			ctx, cancel := context.WithTimeout(parent, cm.config.ConnectionTimeout)
			err := cm.Connect(ctx)
			cancel()

			if err != nil {
				cm.logger.Warn("Reconnection attempt failed", map[string]any{
					"name":    cm.name,
					"attempt": attempt,
					"error":   err.Error(),
				})
				// Will schedule another attempt via Connect
			}

		case <-cm.shutdownCh:
			return
		}
	})
}

// healthCheckLoop probes the connection on a ticker until Close signals shutdownCh.
//
// parent carries the values Connect was called with and none of its cancellation; each probe derives
// its own HealthCheckTimeout from it.
func (cm *ConnectionManager) healthCheckLoop(parent context.Context) {
	defer cm.shutdownWg.Done()

	ticker := time.NewTicker(cm.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if cm.shutdown.Load() == 1 {
				return
			}

			cm.performHealthCheck(parent)

		case <-cm.shutdownCh:
			return
		}
	}
}

// performHealthCheck performs a single health check.
func (cm *ConnectionManager) performHealthCheck(parent context.Context) {
	cm.mu.RLock()
	if cm.state != StateConnected || cm.connection == nil {
		cm.mu.RUnlock()
		return
	}
	conn := cm.connection
	cm.mu.RUnlock()

	ctx, cancel := context.WithTimeout(parent, cm.config.HealthCheckTimeout)
	defer cancel()

	err := cm.health(ctx, conn)
	if err != nil {
		cm.logger.Warn("Health check failed", map[string]any{
			"name":  cm.name,
			"error": err.Error(),
		})

		cm.mu.Lock()
		cm.lastError = err
		cm.closeConnection()
		cm.state = StateDisconnected
		cm.mu.Unlock()

		// Trigger reconnection
		if cm.config.EnableAutoReconnect {
			cm.scheduleReconnect(parent)
		}
	}
}

// closeConnection closes the current connection (must be called with lock held)
func (cm *ConnectionManager) closeConnection() {
	if cm.connection == nil {
		return
	}

	// If connection implements Close(), call it
	if closer, ok := cm.connection.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			cm.logger.Warn("Error closing connection", map[string]any{
				"name":  cm.name,
				"error": err.Error(),
			})
		}
	}

	cm.connection = nil
}

// Close closes the connection and stops reconnection attempts
func (cm *ConnectionManager) Close() error {
	if !cm.shutdown.CompareAndSwap(0, 1) {
		return nil // Already shut down
	}

	cm.logger.Info("Closing connection manager", map[string]any{
		"name": cm.name,
	})

	close(cm.shutdownCh)

	cm.mu.Lock()
	cm.closeConnection()
	cm.state = StateDisconnected
	cm.mu.Unlock()

	// Wait for background goroutines to finish
	cm.shutdownWg.Wait()

	cm.logger.Info("Connection manager closed", map[string]any{
		"name": cm.name,
	})

	return nil
}

// Wait waits for a successful connection (with timeout)
func (cm *ConnectionManager) Wait(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			cm.mu.RLock()
			state := cm.state
			cm.mu.RUnlock()

			switch state {
			case StateConnected:
				return nil
			case StateFailed:
				return errors.NewError(errors.ErrCodeConnectionFailed, "connection failed permanently").
					WithComponent(cm.name)

			// The other three states are what this function exists to wait through, so falling to the
			// next tick is the whole behavior and not an omission. Disconnected and Reconnecting are
			// transient by construction — the manager's retry loop drives them toward Connected or
			// Failed — and Connecting is the first attempt in progress. The caller's context deadline
			// is what bounds the wait; an arm here that returned an error on any of them would turn
			// "not yet" into "never".
			case StateDisconnected, StateConnecting, StateReconnecting:
			}
		}
	}
}

// ConnectionPool manages multiple connections with load balancing
type ConnectionPool struct {
	name      string
	managers  []*ConnectionManager
	nextIndex atomic.Uint32
	logger    *utils.StructuredLogger
}

// NewConnectionPool creates a new connection pool
func NewConnectionPool(name string, size int, config ConnectionConfig, factory ConnectionFactory, health HealthChecker) *ConnectionPool {
	if config.Logger == nil {
		loggerConfig := utils.DefaultStructuredLoggerConfig()
		logger, _ := utils.NewStructuredLogger(loggerConfig)
		config.Logger = logger
	}

	managers := make([]*ConnectionManager, size)
	for i := range size {
		connName := fmt.Sprintf("%s-%d", name, i)
		managers[i] = NewConnectionManager(connName, config, factory, health)
	}

	return &ConnectionPool{
		name:     name,
		managers: managers,
		logger:   config.Logger,
	}
}

// ConnectAll establishes all connections in the pool
func (cp *ConnectionPool) ConnectAll(ctx context.Context) error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(cp.managers))

	for _, mgr := range cp.managers {
		wg.Add(1)
		go func(m *ConnectionManager) {
			defer wg.Done()
			if err := m.Connect(ctx); err != nil {
				errCh <- err
			}
		}(mgr)
	}

	wg.Wait()
	close(errCh)

	// Collect any errors
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to connect %d out of %d connections: %v", len(errs), len(cp.managers), errs[0])
	}

	return nil
}

// GetConnection returns the next available connection (round-robin)
func (cp *ConnectionPool) GetConnection() (any, error) {
	// Try round-robin first
	index := cp.nextIndex.Add(1) % uint32(len(cp.managers))
	conn, err := cp.managers[index].GetConnection()
	if err == nil {
		return conn, nil
	}

	// If that connection is not available, try others
	for i := range cp.managers {
		conn, err = cp.managers[i].GetConnection()
		if err == nil {
			return conn, nil
		}
	}

	return nil, errors.NewError(errors.ErrCodeConnectionPool, "no healthy connections available").
		WithComponent(cp.name)
}

// GetStats returns statistics for all connections in the pool
func (cp *ConnectionPool) GetStats() []ConnectionStats {
	stats := make([]ConnectionStats, len(cp.managers))
	for i, mgr := range cp.managers {
		stats[i] = mgr.GetStats()
	}
	return stats
}

// Close closes all connections in the pool
func (cp *ConnectionPool) Close() error {
	var errs []error
	for _, mgr := range cp.managers {
		if err := mgr.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors closing %d connections: %v", len(errs), errs[0])
	}

	return nil
}

// HealthyCount returns the number of healthy connections
func (cp *ConnectionPool) HealthyCount() int {
	count := 0
	for _, mgr := range cp.managers {
		if mgr.IsConnected() {
			count++
		}
	}
	return count
}
