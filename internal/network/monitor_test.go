package network

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestNewMonitor_NotNil(t *testing.T) {
	t.Parallel()
	m := NewMonitor(AlgorithmBBR)
	if m == nil {
		t.Error("NewMonitor() returned nil")
	}
}

func TestMonitor_RecordSend(t *testing.T) {
	t.Parallel()
	m := NewMonitor(AlgorithmAuto)
	m.RecordSend(100)
	m.RecordSend(200)
	snap := m.Snapshot()
	if snap.BytesSent != 300 {
		t.Errorf("BytesSent = %d, want 300", snap.BytesSent)
	}
}

func TestMonitor_RecordReceive(t *testing.T) {
	t.Parallel()
	m := NewMonitor(AlgorithmAuto)
	m.RecordReceive(512)
	snap := m.Snapshot()
	if snap.BytesReceived != 512 {
		t.Errorf("BytesReceived = %d, want 512", snap.BytesReceived)
	}
}

func TestMonitor_RecordConnection(t *testing.T) {
	t.Parallel()
	m := NewMonitor(AlgorithmAuto)
	m.RecordConnection()
	m.RecordConnection()
	snap := m.Snapshot()
	if snap.Connections != 2 {
		t.Errorf("Connections = %d, want 2", snap.Connections)
	}
	if snap.ActiveConns != 2 {
		t.Errorf("ActiveConns = %d, want 2", snap.ActiveConns)
	}
	m.RecordConnectionClose()
	snap = m.Snapshot()
	if snap.ActiveConns != 1 {
		t.Errorf("ActiveConns after close = %d, want 1", snap.ActiveConns)
	}
	if snap.Connections != 2 {
		t.Errorf("Connections after close = %d, want 2 (total unchanged)", snap.Connections)
	}
}

func TestMonitor_RecordError(t *testing.T) {
	t.Parallel()
	m := NewMonitor(AlgorithmAuto)
	m.RecordError()
	m.RecordError()
	m.RecordError()
	snap := m.Snapshot()
	if snap.Errors != 3 {
		t.Errorf("Errors = %d, want 3", snap.Errors)
	}
}

func TestMonitor_Reset(t *testing.T) {
	t.Parallel()
	m := NewMonitor(AlgorithmAuto)
	m.RecordSend(1000)
	m.RecordReceive(2000)
	m.RecordConnection()
	m.RecordError()
	m.Reset()
	snap := m.Snapshot()
	if snap.BytesSent != 0 {
		t.Errorf("BytesSent after Reset = %d, want 0", snap.BytesSent)
	}
	if snap.BytesReceived != 0 {
		t.Errorf("BytesReceived after Reset = %d, want 0", snap.BytesReceived)
	}
	if snap.Connections != 0 {
		t.Errorf("Connections after Reset = %d, want 0", snap.Connections)
	}
	if snap.Errors != 0 {
		t.Errorf("Errors after Reset = %d, want 0", snap.Errors)
	}
	// Active connections are preserved across Reset.
	if snap.ActiveConns != 1 {
		t.Errorf("ActiveConns after Reset = %d, want 1 (open conns preserved)", snap.ActiveConns)
	}
}

func TestMonitor_SnapshotAlgorithm(t *testing.T) {
	t.Parallel()
	m := NewMonitor(AlgorithmBBR)
	snap := m.Snapshot()
	if snap.Algorithm != AlgorithmBBR {
		t.Errorf("Algorithm = %q, want %q", snap.Algorithm, AlgorithmBBR)
	}
	if snap.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
}

func TestMonitor_Uptime(t *testing.T) {
	t.Parallel()
	m := NewMonitor(AlgorithmAuto)
	// Sleep briefly so uptime is measurable.
	time.Sleep(time.Millisecond)
	up := m.Uptime()
	if up <= 0 {
		t.Errorf("Uptime = %v, want > 0", up)
	}
}

func TestStats_Throughput(t *testing.T) {
	t.Parallel()
	s := Stats{BytesSent: 1024 * 1024, BytesReceived: 1024 * 1024} // 2 MiB total
	throughput := s.Throughput(time.Second)
	// 2 MiB / 1 s = 2.0 MiB/s
	if throughput < 1.9 || throughput > 2.1 {
		t.Errorf("Throughput = %.4f, want ~2.0 MiB/s", throughput)
	}
}

func TestStats_Throughput_ZeroElapsed(t *testing.T) {
	t.Parallel()
	s := Stats{BytesSent: 1000}
	if got := s.Throughput(0); got != 0 {
		t.Errorf("Throughput(0) = %f, want 0", got)
	}
	if got := s.Throughput(-1); got != 0 {
		t.Errorf("Throughput(-1) = %f, want 0", got)
	}
}

// TestWrapDialContext_Success verifies that WrapDialContext records connections and
// that the wrapped conn records bytes on Read/Write/Close.
func TestWrapDialContext_Success(t *testing.T) {
	t.Parallel()

	// Start a local echo server.
	//
	// ListenConfig.Listen rather than net.Listen so the bind is bounded by the test's own context: a
	// net.Listen that blocks in the resolver has nothing to cancel it and hangs until the package
	// timeout, reported against whichever test happens to be running. internal/health's listenHealth
	// takes the same shape for the same reason.
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n])
	}()

	m := NewMonitor(AlgorithmAuto)
	wrapped := m.WrapDialContext(func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	})

	conn, err := wrapped(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("wrapped dial: %v", err)
	}

	// Connection should now be tracked.
	snap := m.Snapshot()
	if snap.Connections != 1 {
		t.Errorf("Connections = %d, want 1", snap.Connections)
	}
	if snap.ActiveConns != 1 {
		t.Errorf("ActiveConns = %d, want 1", snap.ActiveConns)
	}

	// Write some bytes.
	msg := []byte("hello")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read echo back.
	buf := make([]byte, 16)
	n, _ := conn.Read(buf)
	_ = n

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	snap = m.Snapshot()
	if snap.BytesSent < int64(len(msg)) {
		t.Errorf("BytesSent = %d, want >= %d", snap.BytesSent, len(msg))
	}
	if snap.BytesReceived < 1 {
		t.Errorf("BytesReceived = %d, want >= 1", snap.BytesReceived)
	}
	if snap.ActiveConns != 0 {
		t.Errorf("ActiveConns after close = %d, want 0", snap.ActiveConns)
	}
}

// TestWrapDialContext_Error verifies that dial errors increment the error counter.
func TestWrapDialContext_Error(t *testing.T) {
	t.Parallel()
	m := NewMonitor(AlgorithmAuto)
	wrapped := m.WrapDialContext(func(ctx context.Context, network, address string) (net.Conn, error) {
		// Connect to a port nothing is listening on.
		return nil, &net.OpError{Op: "dial", Net: network, Addr: nil, Err: &net.AddrError{Err: "connection refused", Addr: address}}
	})

	_, err := wrapped(t.Context(), "tcp", "127.0.0.1:1")
	if err == nil {
		t.Error("expected error from dial, got nil")
	}

	snap := m.Snapshot()
	if snap.Errors != 1 {
		t.Errorf("Errors = %d, want 1", snap.Errors)
	}
	if snap.Connections != 0 {
		t.Errorf("Connections = %d, want 0 (dial failed)", snap.Connections)
	}
}
