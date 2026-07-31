package s3_test

import (
	"context"
	"runtime"
	"testing"

	"github.com/objectfs/objectfs/internal/storage/s3"
	"github.com/objectfs/objectfs/internal/testaws"
)

// TestBackendCloseReleasesSockets pins the socket leak that FuzzConfigConstructsBackend found.
//
// Backend.Close drained the ConnectionPool and stopped there. The pool holds *s3.Client values —
// cheap structs that all share one http.Transport — so draining it released no sockets whatsoever.
// The sockets were the transport's idle connections, up to MaxIdleConns of them held for a 90-second
// IdleConnTimeout, and nothing ever closed them. Measured before the fix: 40 create-and-Close cycles
// left 80 sockets open against the endpoint, and the fuzz target failed with "dial tcp: connect:
// can't assign requested address" once the ephemeral port range filled.
//
// It counts file descriptors rather than parsing netstat, because the descriptor is what actually
// runs out. A leak that shows up as "too many open files" in a long-lived mount is the same defect as
// the one that shows up as port exhaustion in a fuzz run, and the FD count catches both.
func TestBackendCloseReleasesSockets(t *testing.T) {
	// Not parallel: it counts a process-wide resource, and a concurrent test opening files would be
	// indistinguishable from the leak.

	sh := testaws.Shared(t)

	bucket, err := sh.Bucket(context.Background())
	if err != nil {
		t.Fatalf("testaws: bucket: %v", err)
	}

	// One cycle first, so anything allocated once — the emulator's own accept-side state, lazily
	// initialized SDK machinery — is already counted in the baseline and not attributed to the loop.
	warm, err := s3.NewBackend(context.Background(), bucket, sh.Config())
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if err := warm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before := openFDs(t)

	const cycles = 30
	for range cycles {
		backend, err := s3.NewBackend(context.Background(), bucket, sh.Config())
		if err != nil {
			t.Fatalf("NewBackend: %v", err)
		}

		// A request, so a socket is actually established. A backend that never talks to the endpoint
		// has nothing to leak, and the health check inside NewBackend already does one — but doing an
		// explicit operation keeps the test honest if that ever changes.
		if _, err := backend.HeadObject(context.Background(), "does-not-exist"); err == nil {
			t.Fatal("HeadObject on a missing key returned no error")
		}

		if err := backend.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	after := openFDs(t)

	// A small allowance, not zero. Go's runtime and the emulator both hold descriptors whose exact
	// count is not this test's business, and the defect being guarded against was 2 per cycle — 60
	// over this loop — so the signal is nowhere near the noise.
	const allowance = 10

	if after > before+allowance {
		t.Errorf("%d create-and-Close cycles leaked descriptors: %d open before, %d after "+
			"(allowing %d). Close is not releasing the transport's idle connections, which is what "+
			"exhausted the ephemeral port range during fuzzing.",
			cycles, before, after, allowance)
	}
}

// openFDs counts the process's open file descriptors.
//
// It probes rather than reading /proc, which does not exist on darwin: for each candidate number it
// asks the runtime whether the descriptor is open. Bounded well above any plausible count for this
// test, and cheap enough at that size.
func openFDs(t *testing.T) int {
	t.Helper()

	// Let anything the previous cycle finalized be released before counting.
	runtime.GC()

	var n int

	const maxFD = 4096
	for fd := range maxFD {
		if fdIsOpen(fd) {
			n++
		}
	}

	return n
}
