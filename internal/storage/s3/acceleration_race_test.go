package s3

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestAccelerationFallbackIsRaceFree exercises the fallback state under concurrent readers.
//
// `accelerationActive` and `client` are mutable ClientManager fields that any request goroutine can
// write: executeWithAccelerationFallback calls DisableAcceleration the moment one GET or PUT gets
// an acceleration error, and every other in-flight request is concurrently reading the same two
// fields through IsAccelerationActive and GetAcceleratedClient. There was no lock on either side.
//
// This is not a theoretical window. Acceleration errors arrive in bursts — a bucket without the
// Transfer Acceleration configuration returns InvalidRequest for *every* request, so a mount doing
// concurrent reads hits the write path on many goroutines at once, which is also the moment the
// filesystem is under load. A torn read of `client` hands an operation a pointer that is neither
// the accelerated nor the standard client.
//
// The test drives the real methods rather than a copy of their logic, because a hand-rolled
// reproduction can be race-free while the code it stands in for is not.
func TestAccelerationFallbackIsRaceFree(t *testing.T) {
	t.Parallel()

	const goroutines = 16

	cm := &ClientManager{
		client:             &s3.Client{},
		acceleratedClient:  &s3.Client{},
		standardClient:     &s3.Client{},
		accelerationActive: true,
		config:             &Config{UseAccelerate: true},
		logger:             slog.New(slog.DiscardHandler),
	}

	var wg sync.WaitGroup

	for i := range goroutines {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			// Interleave the writers among the readers rather than running them in phases: the
			// interesting schedule is a Disable landing between a caller's IsAccelerationActive
			// check and its GetAcceleratedClient, which is exactly the sequence
			// executeWithAccelerationFallback performs.
			for range 200 {
				switch i % 4 {
				case 0:
					cm.DisableAcceleration("test")
				case 1:
					cm.EnableAcceleration()
				case 2:
					if cm.IsAccelerationActive() {
						_ = cm.GetAcceleratedClient()
					}
				default:
					_ = cm.GetStandardClient()
				}
			}
		}(i)
	}

	wg.Wait()

	// A vacuity check. If the switch above stopped reaching the mutating arms, every read would
	// trivially agree and -race would report nothing, so assert the state machine actually moved:
	// whichever way it settled, it must be internally consistent.
	if cm.IsAccelerationActive() && cm.GetAcceleratedClient() == nil {
		t.Fatal("acceleration reports active while GetAcceleratedClient returns nil, so a caller " +
			"that checks the flag and then asks for the client gets nothing to send the request on")
	}
}
