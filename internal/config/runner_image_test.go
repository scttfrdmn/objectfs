package config

import (
	"strings"
	"testing"
)

// runnerImage is the one runner label every job in .github/workflows runs on.
//
// Pinned, and the reason is the rest of this repository. Every Go module is in go.mod, every action
// is pinned to a commit SHA, golangci-lint is pinned to an exact version — and the OS under all of it
// was `ubuntu-latest`, the only dependency that changed without a commit. GitHub moves that label to
// Ubuntu 26 from 2026-10-19, which would have changed the base image of all 23 jobs (CI, release,
// docs, auto-merge) on one date with no diff to review. #544.
//
// The image is not incidental here. `packaging` mounts through a /dev/fuse and a fusermount3 nobody
// declared, runs `systemd-analyze verify` (systemd-version-sensitive) and Lmod out of the image, and
// the rpm step's container exists because the image has no rpm. Each of those is a property of the
// image, and a floating label makes each one change underneath a PR that did not touch it.
//
// To move: change this constant and every `runs-on:` together, in one PR, and read its run. That is
// the point — the upgrade becomes a change someone reviews, rather than a date.
const runnerImage = "ubuntu-24.04"

// TestEveryWorkflowJobPinsItsRunnerImage asserts every job runs on runnerImage, literally.
func TestEveryWorkflowJobPinsItsRunnerImage(t *testing.T) {
	t.Parallel()

	checked := 0

	for _, wf := range readWorkflows(t) {
		for _, id := range sortedMapKeys(wf.File.Jobs) {
			job := wf.File.Jobs[id]

			// A reusable-workflow caller has no `runs-on:` of its own — Actions rejects the key beside
			// `uses:` — and the called workflow's jobs are walked by this same loop.
			if job.Uses != "" && job.Steps == nil {
				continue
			}

			checked++

			label, isLabel := job.RunsOn.(string)

			switch {
			case job.RunsOn == nil:
				t.Errorf("%s: job %v has no `runs-on:`. Set `runs-on: %s`.", wf.Name, id, runnerImage)
			case !isLabel:
				t.Errorf("%s: job %v has `runs-on: %v`, which is a list or a group rather than one label.\n"+
					"\tEither one can resolve to an image this test cannot name, so the image stops being a\n"+
					"\treviewable change. Use `runs-on: %s`.", wf.Name, id, job.RunsOn, runnerImage)
			case strings.HasSuffix(label, "-latest"):
				t.Errorf("%s: job %v has `runs-on: %s`.\n"+
					"\tA `-latest` label is the one dependency here that moves without a commit:\n"+
					"\t`ubuntu-latest` became Ubuntu 26 on GitHub's schedule, not this repository's. Use\n"+
					"\t`runs-on: %s`, and move the pin deliberately (#544).", wf.Name, id, label, runnerImage)
			case label != runnerImage:
				t.Errorf("%s: job %v has `runs-on: %s`, want %s.\n"+
					"\tEvery job runs on one image, so a result in one job says something about the others.\n"+
					"\tIf this is the start of a deliberate move, move them all and change runnerImage in\n"+
					"\tthe same PR.", wf.Name, id, label, runnerImage)
			}
		}
	}

	// 23 jobs when this was written. A floor, not a count, for the reason job_timeouts_test.go gives:
	// a lookup that stops matching passes every per-job assertion, and the only tell is the tally.
	if checked < 20 {
		t.Fatalf("checked %d jobs' `runs-on:`, expected at least 20. The walk has stopped finding "+
			"jobs and this test is passing on ones it never read", checked)
	}
}
