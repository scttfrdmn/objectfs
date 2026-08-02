package s3

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// newTestPool builds a pool whose factory hands out distinct, never-used clients. The clients are
// never called, so an empty s3.Client is sufficient and no credentials or network are involved.
func newTestPool(t *testing.T, maxSize int) *ConnectionPool {
	t.Helper()

	p, err := NewConnectionPool(maxSize, func() (*s3.Client, error) {
		return &s3.Client{}, nil
	})
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	return p
}

// A saturated pool must make the caller wait for a connection to come back, not hand out a nil
// client. This is the defect that mattered most in the pool: Get returned (*s3.Client)(nil) once
// currentSize reached maxSize, and all six call sites dereferenced it unchecked — including the
// path taken by every GET and PUT. On the default 8-connection pool, the ninth concurrent
// operation panicked and unmounted the filesystem under every open descriptor.
func TestPoolExhaustionWaitsForReturnInsteadOfFailing(t *testing.T) {
	t.Parallel()

	const size = 2
	p := newTestPool(t, size)

	held := make([]*s3.Client, 0, size)
	for i := range size {
		conn, err := p.Get()
		if err != nil {
			t.Fatalf("draw %d of %d: %v", i, size, err)
		}
		if conn == nil {
			t.Fatalf("draw %d returned a nil client with no error", i)
		}
		held = append(held, conn)
	}

	// The pool is now fully subscribed. This draw has to block.
	type result struct {
		conn *s3.Client
		err  error
	}
	got := make(chan result, 1)
	go func() {
		conn, err := p.GetWithTimeout(10 * time.Second)
		got <- result{conn: conn, err: err}
	}()

	select {
	case r := <-got:
		t.Fatalf("a draw against a saturated pool returned immediately (conn=%v, err=%v); "+
			"it must wait for a connection to be returned", r.conn != nil, r.err)
	case <-time.After(100 * time.Millisecond):
		// Correct: still waiting.
	}

	p.Put(held[0])

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("draw failed after a connection was returned: %v", r.err)
		}
		if r.conn == nil {
			t.Fatal("draw returned a nil client after a connection was returned")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("draw did not wake after Put returned a connection")
	}

	p.Put(held[1])
}

// When no connection is returned, the wait must end in an error that says what is wrong and what to
// change. The old implementation had a `default` arm, so `select` never reached its `time.After`
// case and the timeout was dead code.
func TestPoolGetTimesOutWithAnActionableError(t *testing.T) {
	t.Parallel()

	p := newTestPool(t, 1)

	conn, err := p.Get()
	if err != nil {
		t.Fatalf("first draw: %v", err)
	}
	defer p.Put(conn)

	start := time.Now()
	second, err := p.GetWithTimeout(50 * time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a draw against a saturated pool with nothing returned must fail, not succeed")
	}
	if second != nil {
		t.Error("a failed draw must not also return a client")
	}
	if elapsed < 50*time.Millisecond {
		t.Errorf("returned after %s, before the %s timeout elapsed", elapsed, 50*time.Millisecond)
	}
	// The message must name the knob to turn; "connection pool error" tells an operator nothing.
	if !strings.Contains(err.Error(), "pool_size") {
		t.Errorf("timeout error %q does not name the config knob that fixes it", err)
	}
	if stats := p.Stats(); stats.Timeouts != 1 {
		t.Errorf("Stats().Timeouts = %d after one timeout, want 1", stats.Timeouts)
	}
}

// Close must not panic when a Put is in flight, and must not lose one either. Close used to close
// the channel after a separate closed check in Put — a check-then-act race whose losing side is a
// send on a closed channel, i.e. a panic during unmount. Run with -race.
func TestPoolConcurrentPutAndCloseIsSafe(t *testing.T) {
	t.Parallel()

	for range 50 {
		p, err := NewConnectionPool(4, func() (*s3.Client, error) {
			return &s3.Client{}, nil
		})
		if err != nil {
			t.Fatalf("NewConnectionPool: %v", err)
		}

		conns := make([]*s3.Client, 0, 4)
		for i := range 4 {
			c, err := p.Get()
			if err != nil {
				t.Fatalf("draw %d: %v", i, err)
			}
			conns = append(conns, c)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})

		for _, c := range conns {
			wg.Add(1)
			go func(c *s3.Client) {
				defer wg.Done()
				<-start
				p.Put(c) // must not panic whether it lands before or after Close
			}(c)
		}

		wg.Go(func() {
			<-start
			if err := p.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})

		close(start)
		wg.Wait()
	}
}

// Close is idempotent, and a draw after it fails cleanly rather than blocking for the full timeout
// or panicking. A blocked Get must also wake on Close.
func TestPoolCloseIsIdempotentAndWakesWaiters(t *testing.T) {
	t.Parallel()

	p, err := NewConnectionPool(1, func() (*s3.Client, error) {
		return &s3.Client{}, nil
	})
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}

	held, err := p.Get()
	if err != nil {
		t.Fatalf("first draw: %v", err)
	}

	waiting := make(chan error, 1)
	go func() {
		_, err := p.GetWithTimeout(30 * time.Second)
		waiting <- err
	}()

	// Give the waiter time to reach the blocking arm.
	time.Sleep(50 * time.Millisecond)

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got: %v", err)
	}

	select {
	case err := <-waiting:
		if err == nil {
			t.Error("a draw blocked across Close must fail, not succeed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a draw blocked across Close did not wake; it waited out its own timeout instead")
	}

	if _, err := p.Get(); err == nil {
		t.Error("Get on a closed pool must fail")
	}

	// Returning a connection to a closed pool is what every deferred Put does during shutdown.
	p.Put(held)
}

// Concurrent draws and returns must never yield a nil client, never exceed maxSize, and never
// deadlock. This is the shape of a live mount: many FUSE operations sharing one pool. Run with
// -race.
func TestPoolConcurrentGetPutRespectsMaxSize(t *testing.T) {
	t.Parallel()

	const (
		size    = 4
		workers = 16
		iters   = 50
	)

	p := newTestPool(t, size)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range iters {
				conn, err := p.GetWithTimeout(10 * time.Second)
				if err != nil {
					t.Errorf("draw failed under contention: %v", err)
					return
				}
				if conn == nil {
					t.Error("draw returned a nil client with no error")
					return
				}
				p.Put(conn)
			}
		})
	}
	wg.Wait()

	stats := p.Stats()
	if stats.Total > size {
		t.Errorf("pool grew to %d connections, above its max of %d", stats.Total, size)
	}
	if stats.Created > int64(size) {
		t.Errorf("factory ran %d times for a pool of %d; connections are not being reused",
			stats.Created, size)
	}
}

// A factory error must surface to the caller rather than becoming a nil client.
func TestPoolFactoryErrorSurfaces(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("no credentials")
	p, err := NewConnectionPool(2, func() (*s3.Client, error) {
		return nil, sentinel
	})
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	conn, err := p.Get()
	if !errors.Is(err, sentinel) {
		t.Errorf("Get error = %v, want it to wrap %v", err, sentinel)
	}
	if conn != nil {
		t.Error("a failed draw must not also return a client")
	}
	if stats := p.Stats(); stats.Errors != 1 || stats.LastError == "" {
		t.Errorf("Stats() did not record the factory error: Errors=%d LastError=%q",
			stats.Errors, stats.LastError)
	}
}

// Warmup must pre-fill the pool so the first requests hit rather than construct.
func TestPoolWarmupPreFills(t *testing.T) {
	t.Parallel()

	p := newTestPool(t, 4)

	if err := p.Warmup(context.Background(), 3); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	if idle := p.Stats().Idle; idle != 3 {
		t.Fatalf("Stats().Idle = %d after warming 3, want 3", idle)
	}

	conn, err := p.Get()
	if err != nil {
		t.Fatalf("Get after Warmup: %v", err)
	}
	if hits := p.Stats().Hits; hits != 1 {
		t.Errorf("Stats().Hits = %d after a draw against a warmed pool, want 1", hits)
	}
	p.Put(conn)
}

// Resize down must converge on the new limit, including for connections that were checked out when
// it ran. Resize up past the channel's fixed capacity must be refused rather than silently leaving a
// maxSize the buffer cannot honor — a reservation for a slot with no buffer space deadlocks the
// return.
func TestPoolResize(t *testing.T) {
	t.Parallel()

	t.Run("shrink drops idle connections", func(t *testing.T) {
		t.Parallel()

		p := newTestPool(t, 4)
		if err := p.Warmup(context.Background(), 4); err != nil {
			t.Fatalf("Warmup: %v", err)
		}

		if err := p.Resize(2); err != nil {
			t.Fatalf("Resize(2): %v", err)
		}

		stats := p.Stats()
		if stats.Total != 2 {
			t.Errorf("Stats().Total = %d after shrinking to 2, want 2", stats.Total)
		}
		if stats.MaxSize != 2 {
			t.Errorf("Stats().MaxSize = %d, want 2", stats.MaxSize)
		}
	})

	t.Run("shrink drops checked-out connections on return", func(t *testing.T) {
		t.Parallel()

		p := newTestPool(t, 4)

		held := make([]*s3.Client, 0, 4)
		for i := range 4 {
			c, err := p.Get()
			if err != nil {
				t.Fatalf("draw %d: %v", i, err)
			}
			held = append(held, c)
		}

		// Nothing is idle, so the shrink cannot take effect yet.
		if err := p.Resize(1); err != nil {
			t.Fatalf("Resize(1): %v", err)
		}

		for _, c := range held {
			p.Put(c)
		}

		if total := p.Stats().Total; total != 1 {
			t.Errorf("Stats().Total = %d after returning 4 to a pool resized to 1, want 1", total)
		}
		// The pool must still work at its new size.
		c, err := p.Get()
		if err != nil {
			t.Fatalf("Get after shrink: %v", err)
		}
		p.Put(c)
	})

	t.Run("grow past capacity is refused", func(t *testing.T) {
		t.Parallel()

		p := newTestPool(t, 2)

		err := p.Resize(64)
		if err == nil {
			t.Fatal("Resize accepted a size above the pool's fixed capacity")
		}
		if !strings.Contains(err.Error(), "pool_size") {
			t.Errorf("Resize error %q does not say how to actually get a bigger pool", err)
		}
		if got := p.Stats().MaxSize; got != 2 {
			t.Errorf("a refused Resize changed MaxSize to %d, want it left at 2", got)
		}

		// Still usable, and still bounded by the original size.
		c, err := p.Get()
		if err != nil {
			t.Fatalf("Get after a refused Resize: %v", err)
		}
		p.Put(c)
	})

	t.Run("rejects non-positive and closed", func(t *testing.T) {
		t.Parallel()

		p := newTestPool(t, 2)
		for _, size := range []int{0, -1} {
			if err := p.Resize(size); err == nil {
				t.Errorf("Resize(%d) was accepted", size)
			}
		}

		if err := p.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := p.Resize(1); err == nil {
			t.Error("Resize on a closed pool was accepted")
		}
	})
}

// Warmup must respect the pool's limit, report factory failures rather than swallowing them, and
// leave no phantom capacity behind when the factory fails — an unreleased reservation would shrink
// the usable pool permanently.
func TestPoolWarmupBoundsAndErrors(t *testing.T) {
	t.Parallel()

	t.Run("caps at maxSize", func(t *testing.T) {
		t.Parallel()

		p := newTestPool(t, 3)
		if err := p.Warmup(context.Background(), 100); err != nil {
			t.Fatalf("Warmup: %v", err)
		}
		if stats := p.Stats(); stats.Idle != 3 || stats.Total != 3 {
			t.Errorf("Stats() Idle=%d Total=%d after warming 100 into a pool of 3, want 3 and 3",
				stats.Idle, stats.Total)
		}
	})

	t.Run("a count of zero warms the whole pool", func(t *testing.T) {
		t.Parallel()

		p := newTestPool(t, 3)
		if err := p.Warmup(context.Background(), 0); err != nil {
			t.Fatalf("Warmup: %v", err)
		}
		if idle := p.Stats().Idle; idle != 3 {
			t.Errorf("Stats().Idle = %d after Warmup(0) on a pool of 3, want 3", idle)
		}
	})

	t.Run("factory failure surfaces and releases its reservation", func(t *testing.T) {
		t.Parallel()

		sentinel := errors.New("no credentials")
		var calls int
		p, err := NewConnectionPool(2, func() (*s3.Client, error) {
			calls++
			// Fail the warm, then succeed, so we can prove the failed attempt did not
			// consume capacity.
			if calls <= 2 {
				return nil, sentinel
			}

			return &s3.Client{}, nil
		})
		if err != nil {
			t.Fatalf("NewConnectionPool: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })

		err = p.Warmup(context.Background(), 2)
		if !errors.Is(err, sentinel) {
			t.Fatalf("Warmup error = %v, want it to wrap %v", err, sentinel)
		}
		if total := p.Stats().Total; total != 0 {
			t.Errorf("Stats().Total = %d after a wholly failed warm, want 0; the reservations "+
				"were not released", total)
		}

		// Full capacity must still be available.
		for i := range 2 {
			c, err := p.Get()
			if err != nil {
				t.Fatalf("draw %d after a failed warm: %v", i, err)
			}
			defer p.Put(c)
		}
	})

	t.Run("a canceled context stops the warm", func(t *testing.T) {
		t.Parallel()

		p := newTestPool(t, 4)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := p.Warmup(ctx, 4); !errors.Is(err, context.Canceled) {
			t.Errorf("Warmup with a canceled context = %v, want context.Canceled", err)
		}
		if idle := p.Stats().Idle; idle != 0 {
			t.Errorf("Stats().Idle = %d after a canceled warm, want 0", idle)
		}
	})

	t.Run("warming a closed pool fails", func(t *testing.T) {
		t.Parallel()

		p := newTestPool(t, 2)
		if err := p.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := p.Warmup(context.Background(), 2); err == nil {
			t.Error("Warmup on a closed pool was accepted")
		}
	})
}

// A nil factory is a programming error and must be rejected at construction, not at first use.
func TestNewConnectionPoolRejectsNilFactory(t *testing.T) {
	t.Parallel()

	if _, err := NewConnectionPool(4, nil); err == nil {
		t.Error("NewConnectionPool accepted a nil factory")
	}
}

// A non-positive size is corrected to the default rather than producing an unbuffered channel that
// blocks its first send forever.
func TestNewConnectionPoolDefaultsNonPositiveSize(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, -1} {
		p, err := NewConnectionPool(size, func() (*s3.Client, error) {
			return &s3.Client{}, nil
		})
		if err != nil {
			t.Fatalf("NewConnectionPool(%d): %v", size, err)
		}
		t.Cleanup(func() { _ = p.Close() })

		if got := p.Stats().MaxSize; got <= 0 {
			t.Errorf("NewConnectionPool(%d) left MaxSize = %d", size, got)
		}

		conn, err := p.Get()
		if err != nil {
			t.Errorf("NewConnectionPool(%d): Get: %v", size, err)
			continue
		}
		p.Put(conn) // would block forever on an unbuffered channel
	}
}
