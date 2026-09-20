package config

import (
	"testing"
)

// A `pull_request` trigger must not filter on base branch.
//
// `ci.yml` and `security.yml` both carried `branches: [main]` until #512. The effect was that a pull
// request based on anything other than `main` ran **none** of their jobs — not `test`, not `lint`, not
// `coverage`, not `Security Scan`, not one of the twelve `build-tags`/`cross-build` cells. Nothing
// failed. The checks simply never existed, so `gh pr checks` returned one row (`dependabot  skipping`,
// the only workflow with no base filter) and the pull request read as green because nothing was red.
// #511 was reviewed in exactly that state.
//
// Two things make this worth a test rather than a comment. It is silent in the direction that looks
// fine — an absent check set is indistinguishable from a passing one at a glance — and it closes at the
// moment it stops mattering, because GitHub retargets a stacked PR to `main` when its parent merges and
// CI finally runs after review is over.
//
// The directory is walked rather than enumerated, on purpose. Naming `ci.yml` and `security.yml` would
// check the two files that had the bug when this was written; the next workflow is the one likely to
// arrive with `branches: [main]` copied from a README, and that is the case a list cannot see.
//
// `push` filters are left alone. `push: branches: [main]` is a different question with a correct
// answer: it stops a feature-branch push from duplicating the pull request's own run.
func TestNoPullRequestTriggerFiltersItsBaseBranch(t *testing.T) {
	t.Parallel()

	workflows := readWorkflows(t)

	triggers := 0

	// Counted apart from `triggers`, because the shape this gate wants is the one that is hardest to
	// see. `pull_request:` with no body unmarshals to a nil mapping, so a lookup that reports presence
	// as "I got a mapping back" drops exactly the correct files and keeps the filtered ones — and a
	// single `triggers == 0` floor does not notice, because the filtered ones are still counted.
	//
	// Measured, not reasoned about: collapsing triggerSection's two return values into `cfg != nil` took
	// this test from 3 triggers to 1, and it passed. The 2 it lost were ci.yml's and security.yml's, the
	// two files that had #512.
	unfiltered := 0

	for _, wf := range workflows {
		if len(wf.File.On) == 0 {
			t.Errorf("%s declares no `on:` section, or it is not a mapping. Every workflow needs a "+
				"trigger; if this file's shape changed, update the parser in workflow_test.go rather "+
				"than dropping the check", wf.Name)

			continue
		}

		// `pull_request_target` runs with a write token against a fork's head, so it is if anything
		// more important that it not silently skip.
		for _, event := range []string{"pull_request", "pull_request_target"} {
			// Presence and configuration are separate return values on purpose: `pull_request:` with no
			// body is the *wanted* shape and unmarshals to nil, so a single nil-mapping result cannot
			// tell "present, unfiltered" from "absent" — and the counter below, which is this test's
			// non-vacuity guard, depends on exactly that distinction.
			cfg, present := triggerSection(wf.File, event)
			if !present {
				continue
			}
			triggers++

			if cfg == nil {
				unfiltered++
			}

			for _, key := range []string{"branches", "branches-ignore"} {
				filter, found := cfg[key]
				if !found {
					continue
				}

				t.Errorf("%s: `%s:` filters on `%s: %v`.\n"+
					"\tA base-branch filter means a pull request stacked on a feature branch runs none\n"+
					"\tof this file's jobs — and reads as green, because the checks never exist rather\n"+
					"\tthan because they passed. That is #512: a stacked PR returned one row,\n"+
					"\t`dependabot  skipping`, and was reviewed against zero signal.\n"+
					"\tRemove the filter. `push: branches: [main]` is fine and is a different question.",
					wf.Name, event, key, filter)
			}
		}
	}

	// The non-vacuity guard, and the one that catches the YAML 1.1 `on`-is-true trap: three workflows
	// carry a pull-request trigger today. If this reaches zero, the lookup has broken and every file
	// is passing because nothing was inspected. The file floor is readWorkflows' own.
	if triggers == 0 {
		t.Fatalf("found no pull_request or pull_request_target trigger across %d workflow files. The "+
			"`on:` lookup has stopped matching — note that yaml.v2 parses the key `on` as the boolean "+
			"true, which is why workflowFile.On is a tagged struct field — and this test now passes "+
			"vacuously", len(workflows))
	}

	if unfiltered == 0 {
		t.Fatalf("found %d pull-request trigger(s) across %d workflow files and not one of them was an "+
			"empty `pull_request:`. That is the shape this gate wants, and both ci.yml and security.yml "+
			"have it, so zero means triggerSection has stopped reporting a nil mapping as present — which "+
			"silently exempts every correct file and leaves the check running only on the ones that "+
			"already carry a filter", triggers, len(workflows))
	}

	t.Logf("checked %d pull-request triggers across %d workflow files, %d of them unfiltered",
		triggers, len(workflows), unfiltered)
}
