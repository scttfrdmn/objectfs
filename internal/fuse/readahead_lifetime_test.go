//go:build linux || darwin

package fuse

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestReadAhead_MountCancellationStopsTheWorkers is the defect behind this package's contextcheck
// finding, which was not about the context.
//
// [ReadAheadManager.Stop] exists, is documented as safe to call twice, and is called by nothing outside
// tests: not [MountManager.Unmount], not Adapter.Stop, not anything on the unmount path. So every mount
// left ConcurrentReads prefetch workers plus a cleanup ticker running for the life of the process, each
// holding its FileSystem — and through it the backend and the cache — reachable. Measured before the
// fix: five goroutines per NewFileSystem, surviving every unmount.
//
// The fix is not a new Stop call someone has to remember to make on each path. The mount context is now
// a second stop signal, so the cancelMount() the adapter already performs on unmount stops them.
//
// Deliberately serial, and it is the only test in this package that has to be. Counting goroutines by
// frame name (see [readAheadGoroutines]) makes this immune to the substrate servers and S3 clients the
// rest of the package stands up — but not to a sibling that builds a read-ahead manager of its own, and
// newReadPathFixture builds one on every call. A top-level test that does not call t.Parallel() runs
// during the serial phase, while every parallel sibling is still paused, so the only read-ahead
// goroutines that can move during the measurement are this test's.
//
//nolint:paralleltest // serial on purpose; see above
func TestReadAhead_MountCancellationStopsTheWorkers(t *testing.T) {
	const (
		mounts  = 5
		workers = 4 // ConcurrentReads below, plus one cleanup ticker each
	)

	before := readAheadGoroutines()

	mountCtxs := make([]context.CancelFunc, 0, mounts)
	for range mounts {
		// #nosec G118 -- cancel is collected and called below; calling it here, or deferring it, would
		// stop the very goroutines this test exists to catch still running.
		ctx, cancel := context.WithCancel(context.Background())
		mountCtxs = append(mountCtxs, cancel)

		// Exactly what CreatePlatformMountManager builds, without a mount: the goroutines start in the
		// constructor, so nothing needs to be mounted for them to leak.
		NewReadAheadManager(ctx, nil, &ReadAheadConfig{
			Enabled:         true,
			WindowSize:      64 << 10,
			MinSequential:   3,
			ConcurrentReads: workers,
			TTL:             time.Minute,
		})
	}

	running := settledReadAheadGoroutines(func(n int) bool { return n >= before+mounts*(workers+1) })
	if want := before + mounts*(workers+1); running < want {
		t.Fatalf("%d read-ahead goroutines after starting %d managers of %d workers each, want at "+
			"least %d; this is not measuring what it thinks it is", running, mounts, workers, want)
	}

	// What the unmount path does. Nothing calls Stop.
	for _, cancel := range mountCtxs {
		cancel()
	}

	after := settledReadAheadGoroutines(func(n int) bool { return n <= before })
	if after > before {
		t.Errorf("%d read-ahead goroutines still running after every mount context was canceled "+
			"(%d → %d → %d across %d managers of %d workers each). Nothing in the tree calls "+
			"ReadAheadManager.Stop, so cancellation is the only thing that ends these — a "+
			"mount/unmount cycle that leaves them behind holds its FileSystem, backend and cache "+
			"reachable for the life of the process.",
			after-before, before, running, after, mounts, workers)
	}
}

// TestPerformPrefetch_UsesTheMountsContext pins the context half.
//
// Each prefetch derived its 5-second budget from context.Background(), so a prefetch in flight when the
// mount went away ran to its own deadline against a backend being closed underneath it, and nothing a
// mount was configured with reached the speculative reads made on its behalf. The budget is derived
// from the mount context now, which means an already-canceled mount produces no request at all.
func TestPerformPrefetch_UsesTheMountsContext(t *testing.T) {
	t.Parallel()

	f := newReadPathFixture(t)

	const path = "prefetch-ctx.dat"
	f.srv.SeedRandom(path, 8192)

	// No workers: this test calls performPrefetch directly, so a queue nothing drains is what it wants.
	f.fs.readAhead.Stop()
	f.fs.readAhead = NewReadAheadManager(t.Context(), f.fs, &ReadAheadConfig{
		Enabled:         true,
		WindowSize:      64 << 10,
		MinSequential:   3,
		ConcurrentReads: 0,
		TTL:             time.Minute,
	})
	t.Cleanup(f.fs.readAhead.Stop)

	mount, cancel := context.WithCancel(t.Context())
	cancel()

	before := len(f.srv.GETs(path))
	f.fs.readAhead.performPrefetch(mount, &PrefetchRequest{path: path, offset: 0, size: 4096})

	if got := len(f.srv.GETs(path)) - before; got != 0 {
		t.Errorf("a prefetch on a canceled mount context issued %d GET(s), want 0. Deriving the "+
			"5-second budget from context.Background() means an unmount cannot stop a prefetch that "+
			"is already running, and every one of them is speculative work for a mount that is gone.",
			got)
	}
}

// readAheadGoroutines counts the goroutines this manager starts, by the names of the two functions it
// starts them in, rather than counting the process total.
//
// runtime.NumGoroutine() is the obvious instrument and it is the wrong one here. This package's tests
// stand up substrate HTTP servers and S3 clients, so the total sits near 960 and moves constantly; a
// delta of 25 read against that is a difference of two numbers each of which is only accurate to
// whatever a sibling happened to be doing. The first version of this test asserted on that delta and
// flaked on 1 run in 6, reading 24 where 25 had started — the setup guard failing, not the leak
// assertion, which is the worst way for a leak test to fail because it reports as "this is not
// measuring what it thinks it is" when the thing it measures is fine.
//
// Naming the frames measures the quantity the test is actually about. Anything else in the process can
// come and go without touching it.
func readAheadGoroutines() int {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}

		buf = make([]byte, 2*len(buf))
	}

	dump := string(buf)

	return strings.Count(dump, "fuse.(*ReadAheadManager).prefetchWorker") +
		strings.Count(dump, "fuse.(*ReadAheadManager).cleanupWorker")
}

// settledReadAheadGoroutines polls [readAheadGoroutines] until want is satisfied, and returns the last
// reading either way.
//
// Both directions need this. go statements in a constructor have not necessarily reached their first
// instruction when the constructor returns, and a canceled worker has not necessarily been scheduled to
// observe it. Polling for the expected value rather than sleeping a guessed interval is what keeps this
// from being a race in the other direction — and returning the last reading rather than a bool lets the
// caller print what it actually saw.
func settledReadAheadGoroutines(want func(int) bool) int {
	n := readAheadGoroutines()

	for range 100 {
		if want(n) {
			return n
		}

		time.Sleep(20 * time.Millisecond)
		n = readAheadGoroutines()
	}

	return n
}
