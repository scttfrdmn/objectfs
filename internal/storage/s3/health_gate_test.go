package s3_test

// The availability gate refused every read of every object after ten reads of a key that did not
// exist.
//
// Two independent defects composed into a total, permanent read outage:
//
//  1. A NoSuchKey was recorded as a health failure. Ten of them drove the s3-reads component from
//     healthy through degraded to unavailable. But an object that was never written is not evidence
//     of anything wrong with S3 — answering "it is not there" requires the service to be up,
//     reachable, authenticating, and correct. Ten stat(2) calls on absent paths are an ordinary
//     minute in the life of a filesystem: shell tab-completion, a build system probing for headers,
//     any open(O_CREAT) of a new file.
//
//  2. StateUnavailable was a one-way door. GetObject checks the gate before calling getObjectRange,
//     which holds the only RecordSuccess("s3-reads") call in the backend — so from the state that
//     needed a success, no success could be produced. Nothing in the repo calls StartHealthChecks,
//     so nothing else could supply one either.
//
// Each fix alone would have been enough to make the observed symptom disappear, and neither alone is
// sufficient: without the classifier a real 404 storm still degrades the mount, and without the probe
// a genuine transient outage still takes it down for good. Both are tested here, through the real
// backend over real HTTP, because that seam is where the composition lives — pkg/health's own tests
// cannot see that GetObject returns before reaching the success it needs.

import (
	"context"
	"strings"
	"testing"

	"github.com/objectfs/objectfs/internal/testaws"
)

// TestReadsOfMissingObjectsDoNotDisableTheMount is the regression test for the composed defect. It
// asserts on an object that exists, because that is the user-visible harm: the file was there, the
// service was healthy, and the read was refused by ObjectFS itself.
func TestReadsOfMissingObjectsDoNotDisableTheMount(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	want := []byte("the file is right here")
	if err := backend.PutObject(ctx, "present.bin", want); err != nil {
		t.Fatalf("seeding an object: %v", err)
	}

	// Thirty is three times the default UnavailableThreshold, so a test that passes here is not
	// passing by staying under a threshold.
	const misses = 30
	for i := range misses {
		_, err := backend.GetObject(ctx, "absent.bin", 0, -1)
		if err == nil {
			t.Fatalf("read %d of a nonexistent object succeeded", i)
		}
		if strings.Contains(err.Error(), "SERVICE_UNAVAILABLE") {
			t.Fatalf("read %d of a nonexistent object was refused by the availability gate rather than "+
				"answered by S3: %v\nThe gate treated \"this object does not exist\" as evidence that S3 "+
				"is unwell.", i, err)
		}
	}

	if !backend.IsReadAvailable() {
		t.Errorf("reads are unavailable after %d reads of a missing key", misses)
	}

	got, err := backend.GetObject(ctx, "present.bin", 0, -1)
	if err != nil {
		t.Fatalf("an object that exists became unreadable after %d reads of one that does not: %v",
			misses, err)
	}
	if string(got) != string(want) {
		t.Errorf("read back %q, want %q", got, want)
	}
}

// TestWritesSurviveReadsOfMissingObjects checks the write side of the same gate. Writes are checked
// against a different component (s3-writes), but the write path issues reads of its own, and a
// classifier that only covered the read component would leave a filesystem that could read but not
// write after the same sequence.
func TestWritesSurviveReadsOfMissingObjects(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	for range 30 {
		_, _ = backend.GetObject(ctx, "absent.bin", 0, -1)
		_, _ = backend.HeadObject(ctx, "absent.bin")
	}

	if !backend.IsWriteAvailable() {
		t.Error("writes are unavailable after reads and stats of a missing key")
	}
	if err := backend.PutObject(ctx, "written.bin", []byte("data")); err != nil {
		t.Fatalf("write refused after reads of a missing key: %v", err)
	}
	if got := ts.GetObject("written.bin"); string(got) != "data" {
		t.Errorf("object stored as %q, want \"data\"", got)
	}
}

// TestStatOfMissingPathsKeepsTheMountUsable is the sequence a shell produces. Every tab-completion
// and every open(O_CREAT) of a new file stats a path that is not there; ObjectFS's own Lookup does
// it once per path component. This must be free.
func TestStatOfMissingPathsKeepsTheMountUsable(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := ts.Backend()
	ctx := context.Background()

	for _, p := range []string{
		"home", "home/user", "home/user/project", "home/user/project/src",
		"home/user/project/src/main.go", "tmp", "tmp/scratch", "var", "var/log", "var/log/app.log",
		"etc", "etc/conf.d", "opt", "opt/tool", "usr", "usr/local", "usr/local/bin",
	} {
		_, _ = backend.HeadObject(ctx, p)
	}

	if !backend.IsReadAvailable() || !backend.IsWriteAvailable() {
		t.Errorf("the mount is unusable after stat(2) on 17 absent paths: reads=%v writes=%v",
			backend.IsReadAvailable(), backend.IsWriteAvailable())
	}
}
