package network

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Stats is an immutable snapshot of network statistics collected by a Monitor.
type Stats struct {
	// BytesSent is the cumulative bytes written across all tracked connections.
	BytesSent int64
	// BytesReceived is the cumulative bytes read.
	BytesReceived int64
	// Connections is the total number of connections established since
	// the Monitor was created (or last Reset).
	Connections int64
	// ActiveConns is the number of currently open tracked connections.
	ActiveConns int64
	// Errors is the number of dial errors recorded.
	Errors int64
	// Algorithm is the congestion control algorithm the Monitor was created
	// with (may be AlgorithmAuto on non-Linux platforms).
	Algorithm Algorithm
	// Timestamp is when this snapshot was taken.
	Timestamp time.Time
}

// Throughput returns the combined send+receive throughput in MiB/s for the
// given elapsed duration.  Returns 0 when elapsed is zero.
func (s Stats) Throughput(elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(s.BytesSent+s.BytesReceived) / (1024 * 1024 * elapsed.Seconds())
}

// Monitor collects lightweight per-connection network statistics.
// All methods are safe for concurrent use.
type Monitor struct {
	algo        Algorithm
	bytesSent   atomic.Int64
	bytesRecv   atomic.Int64
	connections atomic.Int64
	activeConns atomic.Int64
	errors      atomic.Int64

	mu        sync.Mutex
	startedAt time.Time
}

// NewMonitor creates a new Monitor tracking connections that use the given
// congestion control algorithm.
func NewMonitor(algo Algorithm) *Monitor {
	return &Monitor{
		algo:      algo,
		startedAt: time.Now(),
	}
}

// RecordSend adds n to the bytes-sent counter.
func (m *Monitor) RecordSend(n int64) { m.bytesSent.Add(n) }

// RecordReceive adds n to the bytes-received counter.
func (m *Monitor) RecordReceive(n int64) { m.bytesRecv.Add(n) }

// RecordConnection increments the total-connections and active-connections
// counters.  Call RecordConnectionClose when the connection is closed.
func (m *Monitor) RecordConnection() {
	m.connections.Add(1)
	m.activeConns.Add(1)
}

// RecordConnectionClose decrements the active-connections counter.
func (m *Monitor) RecordConnectionClose() {
	m.activeConns.Add(-1)
}

// RecordError increments the error counter (dial failures).
func (m *Monitor) RecordError() { m.errors.Add(1) }

// Snapshot returns a point-in-time copy of the monitor's counters.
func (m *Monitor) Snapshot() Stats {
	return Stats{
		BytesSent:     m.bytesSent.Load(),
		BytesReceived: m.bytesRecv.Load(),
		Connections:   m.connections.Load(),
		ActiveConns:   m.activeConns.Load(),
		Errors:        m.errors.Load(),
		Algorithm:     m.algo,
		Timestamp:     time.Now(),
	}
}

// Uptime returns the duration since the Monitor was created or last Reset.
func (m *Monitor) Uptime() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return time.Since(m.startedAt)
}

// Reset zeros all byte and connection counters.  Active connections counter is
// preserved because open connections remain open.
func (m *Monitor) Reset() {
	m.bytesSent.Store(0)
	m.bytesRecv.Store(0)
	m.connections.Store(0)
	m.errors.Store(0)
	m.mu.Lock()
	m.startedAt = time.Now()
	m.mu.Unlock()
}

// DialContextFunc is the signature of (*net.Dialer).DialContext.
type DialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// WrapDialContext wraps a DialContext function so that every new connection and
// all bytes transferred through it are recorded by the Monitor.
//
// The returned function is a drop-in replacement for the original; it can be
// assigned to (*http.Transport).DialContext.
func (m *Monitor) WrapDialContext(dialCtx DialContextFunc) DialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialCtx(ctx, network, address)
		if err != nil {
			m.RecordError()
			return nil, err
		}
		m.RecordConnection()
		return &trackedConn{Conn: conn, monitor: m}, nil
	}
}

// trackedConn wraps a net.Conn and records bytes sent and received as well as
// connection close events in the associated Monitor.
type trackedConn struct {
	net.Conn
	monitor *Monitor
}

func (tc *trackedConn) Read(b []byte) (int, error) {
	n, err := tc.Conn.Read(b)
	if n > 0 {
		tc.monitor.RecordReceive(int64(n))
	}
	return n, err
}

func (tc *trackedConn) Write(b []byte) (int, error) {
	n, err := tc.Conn.Write(b)
	if n > 0 {
		tc.monitor.RecordSend(int64(n))
	}
	return n, err
}

func (tc *trackedConn) Close() error {
	tc.monitor.RecordConnectionClose()
	return tc.Conn.Close()
}
