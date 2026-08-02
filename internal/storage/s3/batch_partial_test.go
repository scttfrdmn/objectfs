package s3_test

// Audit finding H11: GetObjects reported success for a partial failure.
//
// The old collection loop kept only the first error and discarded it unless *every* key had failed:
//
//	if firstError != nil && len(results) == 0 {
//	    return nil, firstError
//	}
//	return results, nil
//
// So 999 of 1000 keys fetched and one throttled returned a nil error. The map is the only other
// channel a caller has, and a missing entry reads as a nil slice — the same thing an absent object
// produces — so there was no way, anywhere in the API, to tell "this object does not exist" from
// "this GET failed and you should retry it". The single-key-fails case is both the likely one and
// the silent one, which is the combination worth a regression test.
//
// These tests are seam tests: what makes a partial batch partial is what the endpoint did, so each
// one arranges the failure at the endpoint (a fault, or a key that was never stored) rather than by
// stubbing the backend. A mock returning a pre-baked (map, error) pair would assert the loop I
// wrote against the loop I wrote.

import (
	"context"
	stderr "errors"
	"fmt"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
	objerrors "github.com/scttfrdmn/objectfs/pkg/errors"
)

// noCompression turns the default zstd codec off for these tests.
//
// Two of them read bytes back through the raw SDK rather than through the backend, and a compressed
// object read that way is a zstd frame, not the payload. Compression is also irrelevant to what is
// under test here — how a batch reports the keys it could not do — and leaving it on would make a
// byte comparison depend on whether the payload happened to exceed MinSize.
func noCompression(cfg *s3.Config) {
	cfg.Compression.Enabled = false
}

// TestGetObjectsReportsAPartialFailure is the H11 regression test.
//
// Three of five keys exist. The contract is that both halves of the answer arrive: the three that
// were fetched, in the map, and the two that were not, in the error. Asserting only one half is how
// this defect survived — the old code satisfied "the successes are present" perfectly.
func TestGetObjectsReportsAPartialFailure(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(noCompression)
	ctx := context.Background()

	present := []string{"batch/a", "batch/b", "batch/c"}
	absent := []string{"batch/missing-1", "batch/missing-2"}

	want := make(map[string][]byte, len(present))

	for i, key := range present {
		data := testaws.DeterministicBytes(key, 1024*(i+1))
		want[key] = data

		if err := backend.PutObject(ctx, key, data, nil); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
	}

	got, err := backend.GetObjects(ctx, append(append([]string{}, present...), absent...))

	// The error half. On the pre-fix code this is nil, because three keys succeeded.
	if err == nil {
		t.Fatal("GetObjects returned a nil error for a batch in which 2 of 5 keys do not exist. " +
			"A caller cannot distinguish that from a batch that entirely succeeded, and the map " +
			"does not tell it either: a key that failed and a key that holds no bytes are both a " +
			"nil slice (audit finding H11)")
	}

	// Both failures are named, not just the first. The old code kept one error and dropped the
	// rest, so a batch with two bad keys could only ever describe one of them.
	for _, key := range absent {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not name the failed key %q: %v", key, err)
		}
	}

	// The success half. A fix that reported the failure by throwing away the results would be a
	// different defect with the same shape — the caller did the work and cannot see the output.
	if len(got) != len(present) {
		t.Fatalf("GetObjects returned %d objects, want %d: %v", len(got), len(present), keysOf(got))
	}

	for key, wantData := range want {
		gotData, ok := got[key]
		if !ok {
			t.Errorf("key %q missing from the results; it exists and its GET succeeded", key)

			continue
		}

		if len(gotData) != len(wantData) {
			t.Errorf("key %q: got %d bytes, want %d", key, len(gotData), len(wantData))

			continue
		}

		if string(gotData) != string(wantData) {
			t.Errorf("key %q: bytes differ from what was stored", key)
		}
	}
}

// TestGetObjectsPartialFailureStaysInspectable is why the failures are joined rather than formatted.
//
// The distinction the contract cares about is "these keys are not there" versus "these GETs failed
// and are worth retrying". An error whose per-key causes have been flattened into a message can be
// logged and nothing else; a caller deciding whether to retry has to string-match, which is not a
// decision anyone should be making from a message. errors.Join preserves the chain, so errors.As
// still reaches the ObjectFSError underneath and its code is readable.
func TestGetObjectsPartialFailureStaysInspectable(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(noCompression)
	ctx := context.Background()

	if err := backend.PutObject(ctx, "inspect/present", []byte("here"), nil); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	_, err := backend.GetObjects(ctx, []string{"inspect/present", "inspect/absent"})
	if err == nil {
		t.Fatal("GetObjects returned nil for a batch with an absent key")
	}

	// Unwrapping to the typed error is the whole point. If this fails, the failures were flattened
	// somewhere on the way out and a caller can no longer tell an absent object from a throttle.
	var objErr *objerrors.ObjectFSError
	if !stderr.As(err, &objErr) {
		t.Fatalf("errors.As could not reach an *ObjectFSError through the batch error; the "+
			"per-key causes were flattened. Got %T: %v", err, err)
	}

	if objErr.Code != objerrors.ErrCodeObjectNotFound {
		t.Errorf("unwrapped error code = %q, want %q — the batch error should carry through what "+
			"actually went wrong per key, and here it was an absent object",
			objErr.Code, objerrors.ErrCodeObjectNotFound)
	}
}

// TestGetObjectsReturnsNilErrorWhenEveryKeySucceeds is the control.
//
// It is what stops the fix from being "always return an error": a non-nil error on a fully
// successful batch would make the error meaningless, which is the same failure as the original
// defect read from the other direction.
func TestGetObjectsReturnsNilErrorWhenEveryKeySucceeds(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(noCompression)
	ctx := context.Background()

	keys := []string{"ok/a", "ok/b", "ok/c", "ok/d"}
	for _, key := range keys {
		if err := backend.PutObject(ctx, key, testaws.DeterministicBytes(key, 512), nil); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
	}

	got, err := backend.GetObjects(ctx, keys)
	if err != nil {
		t.Fatalf("GetObjects returned an error for a batch in which every key exists: %v", err)
	}

	if len(got) != len(keys) {
		t.Fatalf("GetObjects returned %d objects, want %d: %v", len(got), len(keys), keysOf(got))
	}
}

// TestGetObjectsReportsAServerFailureSeparatelyFromAbsence is the case the map genuinely cannot
// express, and the reason the nil error mattered.
//
// Both keys exist. One GET is failed at the endpoint with a 503 that outlasts the retry budget, so
// the caller's map has one entry and one hole — a hole indistinguishable, in the map alone, from
// the object being absent or empty. The error is the only place that difference lives.
func TestGetObjectsReportsAServerFailureSeparatelyFromAbsence(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(noCompression)
	ctx := context.Background()

	for _, key := range []string{"fault/good", "fault/throttled"} {
		if err := backend.PutObject(ctx, key, testaws.DeterministicBytes(key, 256), nil); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
	}

	// Times is generous: the harness sets MaxRetries = 2, and the fault has to survive every
	// attempt or the GET succeeds on a retry and there is no partial batch to observe.
	ts.InjectFault(testaws.Fault{
		Method:    "GET",
		KeySuffix: "fault/throttled",
		Status:    503,
		Code:      "SlowDown",
		Times:     100,
	})

	got, err := backend.GetObjects(ctx, []string{"fault/good", "fault/throttled"})
	if err == nil {
		t.Fatal("GetObjects returned nil for a batch in which one key's GET was failed with 503 " +
			"on every attempt. Both keys exist, so the caller's only signal that one of them was " +
			"not fetched is the error")
	}

	if !strings.Contains(err.Error(), "fault/throttled") {
		t.Errorf("error does not name the key whose GET failed: %v", err)
	}

	if _, ok := got["fault/good"]; !ok {
		t.Errorf("the key that succeeded is absent from the results: %v", keysOf(got))
	}

	if _, ok := got["fault/throttled"]; ok {
		t.Error("the key whose GET failed on every attempt is present in the results")
	}

	if ts.FaultsFired() == 0 {
		t.Error("no fault fired, so this test did not exercise a failed GET at all")
	}
}

// TestPutObjectsReportsEveryFailureAndAttemptsEveryObject covers the other half of the same edit.
//
// PutObjects has no partial-success channel — a caller that gets an error knows only that something
// is not durable — so the error is the entire report and it has to name every object that failed.
// The stronger requirement is the second one: PutObjects must attempt all of them. A batch that
// stops at the first failure leaves the caller unable to retry, because it cannot know which
// objects were never tried, and "not durable" and "never attempted" need the same remedy but are
// reached from different states.
func TestPutObjectsReportsEveryFailureAndAttemptsEveryObject(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(noCompression)
	ctx := context.Background()

	objects := map[string][]byte{
		"put/ok-1":     testaws.DeterministicBytes("put/ok-1", 128),
		"put/ok-2":     testaws.DeterministicBytes("put/ok-2", 128),
		"put/denied-1": testaws.DeterministicBytes("put/denied-1", 128),
		"put/denied-2": testaws.DeterministicBytes("put/denied-2", 128),
	}

	for _, key := range []string{"put/denied-1", "put/denied-2"} {
		ts.InjectFault(testaws.Fault{
			Method:    "PUT",
			KeySuffix: key,
			Status:    403,
			Code:      "AccessDenied",
			Times:     100,
		})
	}

	err := backend.PutObjects(ctx, objects)
	if err == nil {
		t.Fatal("PutObjects returned nil while two of four objects were refused with AccessDenied")
	}

	// Both failures named, and the count stated. "batch put failed" without a count leaves the
	// operator unable to tell a single bad key from a bucket-wide denial.
	for _, key := range []string{"put/denied-1", "put/denied-2"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not name the failed object %q: %v", key, err)
		}
	}

	if !strings.Contains(err.Error(), "2 of 4") {
		t.Errorf("error does not say how many of the batch failed, so a caller cannot tell one bad "+
			"key from a bucket-wide denial: %v", err)
	}

	// Every object attempted, not just the ones before the first failure. This is asserted against
	// the endpoint: the two allowed objects are readable back, which is only true if the batch
	// carried on past the refusals.
	for _, key := range []string{"put/ok-1", "put/ok-2"} {
		if !ts.ObjectExists(key) {
			t.Errorf("object %q was not stored; PutObjects stopped at a failure instead of "+
				"attempting every object, so a caller cannot know what still needs retrying", key)
		}
	}

	var objErr *objerrors.ObjectFSError
	if !stderr.As(err, &objErr) {
		t.Fatalf("errors.As could not reach an *ObjectFSError through the batch error; the "+
			"per-object causes were flattened into a message. Got %T: %v", err, err)
	}
}

// TestPutObjectsReturnsNilWhenEveryObjectSucceeds is the control for the above.
func TestPutObjectsReturnsNilWhenEveryObjectSucceeds(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend(noCompression)
	ctx := context.Background()

	objects := make(map[string][]byte, 4)
	for i := range 4 {
		key := fmt.Sprintf("put-ok/%d", i)
		objects[key] = testaws.DeterministicBytes(key, 256)
	}

	if err := backend.PutObjects(ctx, objects); err != nil {
		t.Fatalf("PutObjects returned an error for a batch in which every object succeeds: %v", err)
	}

	for key, want := range objects {
		got := ts.GetObject(key)
		if string(got) != string(want) {
			t.Errorf("object %q: stored %d bytes, want %d", key, len(got), len(want))
		}
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
