package s3_test

// Audit finding M17: deleting a key that is not there.
//
// `DeleteObject` opens with a `HeadObject`, because a tier constraint is checked against the
// object's age and the age has to be read from somewhere. That makes absence a case the delete has
// to answer for, and the answer is fixed by two things ObjectFS does not get to choose: S3's
// DeleteObject "removes the null version of an object" and succeeds whether or not the key existed,
// and the Go SDK documents the same. So a delete of an absent key is a no-op returning nil.
//
// The original code tested the HEAD's error against `*s3types.NoSuchKey`, which is the error
// `GetObject` returns. `HeadObject` has no body to carry an error document, so it answers a missing
// key with a bare 404 and the SDK renders that as `*s3types.NotFound` — a different type, matching
// nothing, so the absence fell through to the general failure arm and `rm` on an already-deleted
// file returned an error. `isNotFound` now covers both types and both wire codes.
//
// The mirror case matters as much and is the reason this file tests two things. Absence must be
// swallowed; a HEAD that *failed* must not be. Reading a throttle as "already gone" would have
// `DeleteObject` return success for an object still sitting in the bucket, which is the same defect
// as H11 one layer down — success reported for something that did not happen — and worse here,
// because a caller told its delete succeeded has no reason to look again.

import (
	"context"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// TestDeleteObjectOnAnAbsentKeyIsANoOp pins the documented contract.
//
// Both orders are covered: a key that never existed, and a key deleted twice. They are the same
// code path, but they are not the same bug report — the first is a caller cleaning up
// optimistically, the second is a retry after a delete whose response was lost, and a retry that
// fails is the one that turns a transient network fault into a permanent error.
func TestDeleteObjectOnAnAbsentKeyIsANoOp(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(noCompression)
	ctx := context.Background()

	t.Run("a key that was never stored", func(t *testing.T) {
		t.Parallel()

		if err := backend.DeleteObject(ctx, "delete/never-existed"); err != nil {
			t.Fatalf("DeleteObject on an absent key returned %v; S3 and the Go SDK both document "+
				"this as a no-op, so `rm` on a file another client already removed reports a "+
				"failure that did not happen (audit finding M17)", err)
		}
	})

	t.Run("a key deleted twice", func(t *testing.T) {
		t.Parallel()

		const key = "delete/twice"

		if err := backend.PutObject(ctx, key, []byte("transient"), nil); err != nil {
			t.Fatalf("seeding: %v", err)
		}

		if err := backend.DeleteObject(ctx, key); err != nil {
			t.Fatalf("first DeleteObject: %v", err)
		}

		if ts.ObjectExists(key) {
			t.Fatal("the object survived its first delete, so the second is not the case under test")
		}

		// This is the retry: a delete whose response was lost, reissued. It has to succeed, or a
		// dropped packet becomes a permanent error on a key that is already gone.
		if err := backend.DeleteObject(ctx, key); err != nil {
			t.Fatalf("second DeleteObject returned %v; a retried delete must be idempotent", err)
		}
	})
}

// TestDeleteObjectReportsAFailedHeadRatherThanTreatingItAsAbsence is the mirror.
//
// The HEAD is failed with a 503 that outlasts the retry budget while the object is still there. The
// only correct answer is an error: swallowing this as a no-op would report a successful delete for
// an object that remains in the bucket, and a caller told its delete succeeded does not look again.
//
// It also checks the object is still present afterwards, because "returned an error" and "did not
// delete" are separate claims and a delete that errors *after* removing the object would satisfy
// the first while being a worse bug than the one under test.
func TestDeleteObjectReportsAFailedHeadRatherThanTreatingItAsAbsence(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(noCompression)
	ctx := context.Background()

	const key = "delete/throttled"

	if err := backend.PutObject(ctx, key, []byte("still here"), nil); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Times is generous because the harness sets MaxRetries = 2: the fault has to survive every
	// attempt, or the HEAD succeeds on a retry and there is no failure to observe.
	ts.InjectFault(testaws.Fault{
		Method:    "HEAD",
		KeySuffix: key,
		Status:    503,
		Code:      "SlowDown",
		Times:     100,
	})

	err := backend.DeleteObject(ctx, key)
	if err == nil {
		t.Fatal("DeleteObject returned nil while the HEAD it depends on failed with 503 on every " +
			"attempt. The object is still in the bucket, so this reports a delete that did not " +
			"happen — and a caller told its delete succeeded has no reason to look again")
	}

	if !strings.Contains(err.Error(), key) {
		t.Errorf("error does not name the key, so a bulk delete that fails on one object of a "+
			"thousand cannot say which: %v", err)
	}

	if ts.FaultsFired() == 0 {
		t.Fatal("no fault fired, so this test did not exercise a failed HEAD at all")
	}

	// Disarmed before the verification below, because the fault is still armed at this point and
	// `Times: 100` does not care whose request it is: the harness's own confirming HEAD would be
	// throttled too, and the test would report the object gone when it is merely unreadable. Worth
	// stating rather than fixing silently — it is the same trap as targeting a fault by method and
	// path alone, an assertion answered by the fixture rather than by the code.
	ts.ClearFaults()

	if !ts.ObjectExists(key) {
		t.Error("the object was deleted despite the error; an error and a completed delete are " +
			"the worst combination, because the caller retries something already done")
	}
}
