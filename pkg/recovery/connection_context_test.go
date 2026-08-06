package recovery

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// connCtxKey marks a context so a factory or a health check can report which one it was handed.
type connCtxKey struct{}

// TestConnect_BackgroundWorkCarriesTheCallersContext is what the contextcheck findings on this file
// were worth beyond a linter count.
//
// The health loop and the automatic reconnect each built their context from context.Background(), so a
// ConnectionManager did work on its caller's behalf with nothing the caller had attached. Both now
// descend from Connect's context — its values, not its cancellation, since Close is what ends them.
func TestConnect_BackgroundWorkCarriesTheCallersContext(t *testing.T) {
	t.Parallel()

	config := DefaultConnectionConfig()
	config.HealthCheckInterval = 10 * time.Millisecond
	config.HealthCheckTimeout = time.Second
	config.ReconnectDelay = 10 * time.Millisecond
	config.EnableAutoReconnect = true

	// The first connection succeeds; its first health check fails, which closes the connection and
	// schedules a reconnect. So one Connect exercises both background paths.
	var connects atomic.Int32
	reconnectCtx := make(chan any, 4)
	factory := func(ctx context.Context) (any, error) {
		if connects.Add(1) > 1 {
			reconnectCtx <- ctx.Value(connCtxKey{})
		}

		return &mockConnection{healthy: true}, nil
	}

	healthCtx := make(chan any, 4)
	health := func(ctx context.Context, conn any) error {
		select {
		case healthCtx <- ctx.Value(connCtxKey{}):
		default:
		}

		return errors.New("health check failed, so a reconnect is scheduled")
	}

	cm := NewConnectionManager("test", config, factory, health)
	t.Cleanup(func() { _ = cm.Close() })

	ctx := context.WithValue(t.Context(), connCtxKey{}, "from-the-connect-caller")
	if err := cm.Connect(ctx); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	for _, tc := range []struct {
		what string
		ch   chan any
	}{
		{"the health check", healthCtx},
		{"the automatic reconnect's factory call", reconnectCtx},
	} {
		select {
		case got := <-tc.ch:
			if got != "from-the-connect-caller" {
				t.Errorf("%s saw context value %v, want %q: it does not descend from Connect's context",
					tc.what, got, "from-the-connect-caller")
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never ran", tc.what)
		}
	}
}

// TestConnect_BackgroundWorkSurvivesTheCallersCancellation is the other half of the same decision.
//
// Connect returns; the health loop and any pending reconnect do not. Binding them to Connect's context
// rather than to a cancellation-stripped copy would silently stop health checking the moment the
// caller's request finished — the plausible-looking fix that contextcheck would also have accepted.
func TestConnect_BackgroundWorkSurvivesTheCallersCancellation(t *testing.T) {
	t.Parallel()

	config := DefaultConnectionConfig()
	config.HealthCheckInterval = 10 * time.Millisecond
	config.HealthCheckTimeout = time.Second
	config.EnableAutoReconnect = false

	var checks atomic.Int32
	cm := NewConnectionManager("test", config,
		func(ctx context.Context) (any, error) { return &mockConnection{healthy: true}, nil },
		func(ctx context.Context, conn any) error {
			checks.Add(1)

			return ctx.Err()
		})
	t.Cleanup(func() { _ = cm.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	if err := cm.Connect(ctx); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	// The caller is done with the call it made, as any request-scoped caller would be.
	cancel()

	deadline := time.Now().Add(10 * time.Second)
	for cm.GetStats().State == StateConnected && time.Now().Before(deadline) {
		if checks.Load() >= 3 {
			return // Health checking is still running and still passing.
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("after the Connect caller canceled, health checks = %d and state = %v; canceling the "+
		"caller's context must not take health checking down with it", checks.Load(), cm.GetStats().State)
}

// TestConnect_StartsExactlyOneHealthLoop pins the goroutine leak found alongside the context defects.
//
// Every successful Connect started a health-check loop, and Connect is what Reconnect calls and what the
// reconnect scheduler calls on each automatic attempt — so loops accumulated for the life of the process.
// Measured before the fix: 58 probes across ~200ms at a 20ms interval after six Connect calls, where one
// loop gives ~10. Each surplus loop also independently diagnosed a failure and called scheduleReconnect,
// so one broken connection produced N competing reconnection schedules, each driving reconnectAttempt
// toward MaxReconnectAttempts and StateFailed.
func TestConnect_StartsExactlyOneHealthLoop(t *testing.T) {
	t.Parallel()

	const (
		interval = 20 * time.Millisecond
		window   = 300 * time.Millisecond
		rounds   = 6
	)

	config := DefaultConnectionConfig()
	config.HealthCheckInterval = interval
	config.HealthCheckTimeout = time.Second
	config.EnableAutoReconnect = false

	var checks atomic.Int32
	cm := NewConnectionManager("test", config,
		func(ctx context.Context) (any, error) { return &mockConnection{healthy: true}, nil },
		func(ctx context.Context, conn any) error {
			checks.Add(1)

			return nil
		})
	t.Cleanup(func() { _ = cm.Close() })

	if err := cm.Connect(t.Context()); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	for i := range rounds - 1 {
		if err := cm.Reconnect(t.Context()); err != nil {
			t.Fatalf("Reconnect %d: %v", i, err)
		}
	}

	checks.Store(0)
	time.Sleep(window)
	got := checks.Load()

	// One loop ticking every interval over window. Generous headroom for scheduling — the defect
	// multiplies the rate by the number of Connect calls, so 6× is what this has to separate from, and
	// 3× the nominal rate sits well below it.
	nominal := int32(window / interval)
	if got > 3*nominal {
		t.Errorf("%d health checks in %v at a %v interval after %d Connect calls; one loop gives about "+
			"%d, so roughly %d loops are running and each will schedule its own reconnect",
			got, window, interval, rounds, nominal, int32(float64(got)/float64(nominal)+0.5))
	}
	if got == 0 {
		t.Error("no health checks ran at all, so this asserts nothing about how many loops there are")
	}
}
