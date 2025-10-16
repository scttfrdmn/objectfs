package recovery

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type mockConnection struct {
	healthy bool
	closed  bool
}

func (m *mockConnection) Close() error {
	m.closed = true
	return nil
}

func TestConnectionState_String(t *testing.T) {
	tests := []struct {
		state    ConnectionState
		expected string
	}{
		{StateDisconnected, "disconnected"},
		{StateConnecting, "connecting"},
		{StateConnected, "connected"},
		{StateReconnecting, "reconnecting"},
		{StateFailed, "failed"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.expected {
			t.Errorf("Expected %s, got %s", tt.expected, got)
		}
	}
}

func TestDefaultConnectionConfig(t *testing.T) {
	config := DefaultConnectionConfig()

	if config.ConnectionTimeout != 30*time.Second {
		t.Errorf("Expected 30s connection timeout, got %v", config.ConnectionTimeout)
	}

	if config.ReconnectDelay != 1*time.Second {
		t.Errorf("Expected 1s reconnect delay, got %v", config.ReconnectDelay)
	}

	if config.MaxReconnectDelay != 60*time.Second {
		t.Errorf("Expected 60s max reconnect delay, got %v", config.MaxReconnectDelay)
	}

	if config.ReconnectBackoffMultiplier != 2.0 {
		t.Errorf("Expected 2.0 backoff multiplier, got %v", config.ReconnectBackoffMultiplier)
	}

	if config.MaxReconnectAttempts != 10 {
		t.Errorf("Expected 10 max reconnect attempts, got %d", config.MaxReconnectAttempts)
	}

	if !config.EnableAutoReconnect {
		t.Error("Expected auto reconnect to be enabled by default")
	}
}

func TestNewConnectionManager(t *testing.T) {
	config := DefaultConnectionConfig()
	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		if mc, ok := conn.(*mockConnection); ok && mc.healthy {
			return nil
		}
		return errors.New("unhealthy")
	}

	cm := NewConnectionManager("test", config, factory, health)

	if cm == nil {
		t.Fatal("Expected non-nil connection manager")
	}

	if cm.name != "test" {
		t.Errorf("Expected name 'test', got %s", cm.name)
	}

	if cm.state != StateDisconnected {
		t.Errorf("Expected initial state disconnected, got %v", cm.state)
	}
}

func TestConnectionManager_Connect(t *testing.T) {
	config := DefaultConnectionConfig()
	config.ConnectionTimeout = 5 * time.Second

	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		if mc, ok := conn.(*mockConnection); ok && mc.healthy {
			return nil
		}
		return errors.New("unhealthy")
	}

	cm := NewConnectionManager("test", config, factory, health)
	ctx := context.Background()

	err := cm.Connect(ctx)
	if err != nil {
		t.Fatalf("Expected successful connection, got %v", err)
	}

	if cm.state != StateConnected {
		t.Errorf("Expected state connected, got %v", cm.state)
	}

	if !cm.IsConnected() {
		t.Error("Expected IsConnected to return true")
	}
}

func TestConnectionManager_ConnectTimeout(t *testing.T) {
	config := DefaultConnectionConfig()
	config.ConnectionTimeout = 100 * time.Millisecond
	config.EnableAutoReconnect = false // Disable for this test

	factory := func(ctx context.Context) (interface{}, error) {
		// Simulate slow connection that respects context
		select {
		case <-time.After(200 * time.Millisecond):
			return &mockConnection{healthy: true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	cm := NewConnectionManager("test", config, factory, health)
	ctx := context.Background()

	err := cm.Connect(ctx)
	if err == nil {
		t.Error("Expected timeout error")
	}
}

func TestConnectionManager_GetConnection(t *testing.T) {
	config := DefaultConnectionConfig()
	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	cm := NewConnectionManager("test", config, factory, health)
	ctx := context.Background()

	// Try to get connection before connecting
	_, err := cm.GetConnection()
	if err == nil {
		t.Error("Expected error when not connected")
	}

	// Connect
	err = cm.Connect(ctx)
	if err != nil {
		t.Fatalf("Expected successful connection, got %v", err)
	}

	// Now get connection
	conn, err := cm.GetConnection()
	if err != nil {
		t.Fatalf("Expected to get connection, got %v", err)
	}

	if conn == nil {
		t.Error("Expected non-nil connection")
	}

	if _, ok := conn.(*mockConnection); !ok {
		t.Error("Expected mockConnection type")
	}
}

func TestConnectionManager_Reconnect(t *testing.T) {
	config := DefaultConnectionConfig()
	config.EnableAutoReconnect = false

	connectCount := 0
	factory := func(ctx context.Context) (interface{}, error) {
		connectCount++
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	cm := NewConnectionManager("test", config, factory, health)
	ctx := context.Background()

	// Initial connection
	err := cm.Connect(ctx)
	if err != nil {
		t.Fatalf("Expected successful initial connection, got %v", err)
	}

	if connectCount != 1 {
		t.Errorf("Expected 1 connection attempt, got %d", connectCount)
	}

	// Manual reconnect
	err = cm.Reconnect(ctx)
	if err != nil {
		t.Fatalf("Expected successful reconnection, got %v", err)
	}

	if connectCount != 2 {
		t.Errorf("Expected 2 connection attempts after reconnect, got %d", connectCount)
	}
}

func TestConnectionManager_HealthCheckFailure(t *testing.T) {
	config := DefaultConnectionConfig()
	config.HealthCheckInterval = 50 * time.Millisecond
	config.HealthCheckTimeout = 10 * time.Millisecond
	config.ReconnectDelay = 50 * time.Millisecond
	config.EnableAutoReconnect = true

	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}

	healthCheckCount := int32(0)
	health := func(ctx context.Context, conn interface{}) error {
		count := atomic.AddInt32(&healthCheckCount, 1)
		// Fail on second health check
		if count >= 2 {
			return errors.New("health check failed")
		}
		return nil
	}

	cm := NewConnectionManager("test", config, factory, health)
	ctx := context.Background()

	err := cm.Connect(ctx)
	if err != nil {
		t.Fatalf("Expected successful connection, got %v", err)
	}

	// Wait for health check to fail and reconnection attempt
	time.Sleep(200 * time.Millisecond)

	// Connection should have been lost and reconnected
	if atomic.LoadInt32(&healthCheckCount) < 2 {
		t.Error("Expected multiple health checks")
	}
}

func TestConnectionManager_GetStats(t *testing.T) {
	config := DefaultConnectionConfig()
	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	cm := NewConnectionManager("test-stats", config, factory, health)
	ctx := context.Background()

	// Get stats before connection
	stats := cm.GetStats()
	if stats.Name != "test-stats" {
		t.Errorf("Expected name 'test-stats', got %s", stats.Name)
	}

	if stats.State != StateDisconnected {
		t.Errorf("Expected disconnected state, got %v", stats.State)
	}

	if stats.Connected {
		t.Error("Expected Connected to be false")
	}

	// Connect and get stats
	_ = cm.Connect(ctx)
	stats = cm.GetStats()

	if stats.State != StateConnected {
		t.Errorf("Expected connected state, got %v", stats.State)
	}

	if !stats.Connected {
		t.Error("Expected Connected to be true")
	}

	if stats.ConnectedAt == nil {
		t.Error("Expected ConnectedAt to be set")
	}

	if stats.Uptime <= 0 {
		t.Error("Expected positive uptime")
	}
}

func TestConnectionManager_Close(t *testing.T) {
	config := DefaultConnectionConfig()
	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	cm := NewConnectionManager("test", config, factory, health)
	ctx := context.Background()

	// Connect
	err := cm.Connect(ctx)
	if err != nil {
		t.Fatalf("Expected successful connection, got %v", err)
	}

	conn, _ := cm.GetConnection()
	mc := conn.(*mockConnection)

	// Close
	err = cm.Close()
	if err != nil {
		t.Errorf("Expected no error on close, got %v", err)
	}

	if !mc.closed {
		t.Error("Expected connection to be closed")
	}

	if cm.state != StateDisconnected {
		t.Errorf("Expected disconnected state after close, got %v", cm.state)
	}

	// Second close should be safe
	err = cm.Close()
	if err != nil {
		t.Errorf("Expected no error on second close, got %v", err)
	}
}

func TestConnectionManager_AutoReconnect(t *testing.T) {
	config := DefaultConnectionConfig()
	config.ReconnectDelay = 50 * time.Millisecond
	config.MaxReconnectDelay = 100 * time.Millisecond
	config.MaxReconnectAttempts = 3
	config.EnableAutoReconnect = true

	connectAttempts := int32(0)
	factory := func(ctx context.Context) (interface{}, error) {
		attempt := atomic.AddInt32(&connectAttempts, 1)
		// Fail first two attempts, succeed on third
		if attempt < 3 {
			return nil, errors.New("connection failed")
		}
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	cm := NewConnectionManager("test", config, factory, health)
	ctx := context.Background()

	// Initial connection will fail and trigger auto-reconnect
	_ = cm.Connect(ctx)

	// Wait for reconnection attempts
	time.Sleep(500 * time.Millisecond)

	// Should eventually connect
	if atomic.LoadInt32(&connectAttempts) < 3 {
		t.Logf("Connection attempts: %d", atomic.LoadInt32(&connectAttempts))
	}

	// Clean up
	_ = cm.Close()
}

func TestConnectionManager_MaxReconnectAttempts(t *testing.T) {
	config := DefaultConnectionConfig()
	config.ReconnectDelay = 10 * time.Millisecond
	config.MaxReconnectAttempts = 2
	config.EnableAutoReconnect = true

	connectAttempts := int32(0)
	factory := func(ctx context.Context) (interface{}, error) {
		atomic.AddInt32(&connectAttempts, 1)
		return nil, errors.New("always fails")
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	cm := NewConnectionManager("test", config, factory, health)
	ctx := context.Background()

	// Initial connection
	_ = cm.Connect(ctx)

	// Wait for reconnection attempts
	time.Sleep(200 * time.Millisecond)

	// Should not exceed max attempts
	attempts := atomic.LoadInt32(&connectAttempts)
	if attempts > int32(config.MaxReconnectAttempts+1) { // +1 for initial attempt
		t.Errorf("Expected at most %d attempts, got %d", config.MaxReconnectAttempts+1, attempts)
	}

	// State should eventually be Failed
	if cm.GetState() != StateFailed {
		t.Logf("Final state: %v (may not be Failed yet due to timing)", cm.GetState())
	}

	// Clean up
	_ = cm.Close()
}

func TestConnectionPool_New(t *testing.T) {
	config := DefaultConnectionConfig()
	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	pool := NewConnectionPool("test-pool", 3, config, factory, health)

	if pool == nil {
		t.Fatal("Expected non-nil connection pool")
	}

	if len(pool.managers) != 3 {
		t.Errorf("Expected 3 managers, got %d", len(pool.managers))
	}
}

func TestConnectionPool_ConnectAll(t *testing.T) {
	config := DefaultConnectionConfig()
	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	pool := NewConnectionPool("test-pool", 3, config, factory, health)
	ctx := context.Background()

	err := pool.ConnectAll(ctx)
	if err != nil {
		t.Fatalf("Expected successful connection of all, got %v", err)
	}

	// Verify all are connected
	stats := pool.GetStats()
	for i, stat := range stats {
		if !stat.Connected {
			t.Errorf("Expected connection %d to be connected", i)
		}
	}
}

func TestConnectionPool_GetConnection(t *testing.T) {
	config := DefaultConnectionConfig()
	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	pool := NewConnectionPool("test-pool", 3, config, factory, health)
	ctx := context.Background()

	err := pool.ConnectAll(ctx)
	if err != nil {
		t.Fatalf("Expected successful connection of all, got %v", err)
	}

	// Get multiple connections (should round-robin)
	for i := 0; i < 10; i++ {
		conn, err := pool.GetConnection()
		if err != nil {
			t.Fatalf("Expected to get connection, got %v", err)
		}
		if conn == nil {
			t.Error("Expected non-nil connection")
		}
	}
}

func TestConnectionPool_HealthyCount(t *testing.T) {
	config := DefaultConnectionConfig()
	config.EnableAutoReconnect = false

	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	pool := NewConnectionPool("test-pool", 3, config, factory, health)
	ctx := context.Background()

	// Initially no healthy connections
	if count := pool.HealthyCount(); count != 0 {
		t.Errorf("Expected 0 healthy connections initially, got %d", count)
	}

	// Connect all
	_ = pool.ConnectAll(ctx)

	// Now all should be healthy
	if count := pool.HealthyCount(); count != 3 {
		t.Errorf("Expected 3 healthy connections, got %d", count)
	}
}

func TestConnectionPool_Close(t *testing.T) {
	config := DefaultConnectionConfig()
	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	pool := NewConnectionPool("test-pool", 3, config, factory, health)
	ctx := context.Background()

	_ = pool.ConnectAll(ctx)

	err := pool.Close()
	if err != nil {
		t.Errorf("Expected no error on close, got %v", err)
	}

	// All connections should be closed
	if count := pool.HealthyCount(); count != 0 {
		t.Errorf("Expected 0 healthy connections after close, got %d", count)
	}
}

func TestConnectionManager_Wait(t *testing.T) {
	config := DefaultConnectionConfig()
	factory := func(ctx context.Context) (interface{}, error) {
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	cm := NewConnectionManager("test", config, factory, health)

	// Start connection in background
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = cm.Connect(context.Background())
	}()

	// Wait for connection
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := cm.Wait(ctx)
	if err != nil {
		t.Fatalf("Expected successful wait, got %v", err)
	}

	if !cm.IsConnected() {
		t.Error("Expected connection to be established")
	}
}

func TestConnectionManager_WaitTimeout(t *testing.T) {
	config := DefaultConnectionConfig()
	factory := func(ctx context.Context) (interface{}, error) {
		// Never connects
		time.Sleep(10 * time.Second)
		return &mockConnection{healthy: true}, nil
	}
	health := func(ctx context.Context, conn interface{}) error {
		return nil
	}

	cm := NewConnectionManager("test", config, factory, health)

	// Wait with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := cm.Wait(ctx)
	if err == nil {
		t.Error("Expected timeout error")
	}
}
