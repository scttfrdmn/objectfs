//go:build linux || darwin

package fuse

import (
	"sync"
	"testing"
)

// TestWaitReadsTheServerUnderTheLock is a regression test for a data race between Wait and Unmount.
//
// CI's race detector caught the original defect — an unlocked `if m.server != nil { m.server.Wait() }`
// against Unmount's `m.server = nil` under m.mu — but caught it *intermittently*, on one run out of
// many, because on darwin the racing goroutine needs a real macFUSE mount to exist at all. A
// regression test that only fires when the scheduler cooperates is not a regression test, so this one
// drives both sides directly and does not need a mount.
//
// The nil server is the point rather than a limitation. Wait's contract is "return immediately if
// nothing is mounted", so a nil field exercises the exact read that raced, and leaves Wait with no
// server to block on — which is what makes the test terminate. What is asserted is that the read is
// synchronized: the race detector fails the test if Wait touches m.server outside m.mu while
// Unmount's write is in flight.
//
// The second property is the one a plausible "fix" breaks: taking m.mu across server.Wait() would
// also silence the detector, and would deadlock any real unmount forever, since Unmount needs the same
// lock to clear the field. That cannot be asserted here without a live mount, so it is stated in
// Wait's own comment; this test at least fails if the lock is dropped entirely.
func TestWaitReadsTheServerUnderTheLock(t *testing.T) {
	t.Parallel()

	m := NewMountManager(nil, nil)

	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(2)

	// The write side, standing in for Unmount's `m.server = nil`. Writing the field under m.mu is
	// exactly what Unmount does at the end of a successful unmount.
	go func() {
		defer wg.Done()

		for range iterations {
			m.mu.Lock()
			m.server = nil
			m.mu.Unlock()
		}
	}()

	// The read side. Before the fix this dereferenced m.server without holding m.mu.
	go func() {
		defer wg.Done()

		for range iterations {
			m.Wait()
		}
	}()

	wg.Wait()
}

// TestWaitOnAnUnmountedManagerReturns pins the contract the fix relies on.
//
// Wait has to be safe to call on a manager that never mounted, because that is what a caller does
// after Mount returns an error — and the fix reads the field into a local before the nil check, so a
// future edit that drops the check would hang here rather than in production.
func TestWaitOnAnUnmountedManagerReturns(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})

	go func() {
		NewMountManager(nil, nil).Wait()
		close(done)
	}()

	<-done
}
