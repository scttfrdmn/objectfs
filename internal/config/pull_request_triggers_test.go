package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v2"
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

	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	files := 0
	triggers := 0

	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		files++

		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path) // #nosec G304 -- a directory entry from .github/workflows
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		// Keys are `any` rather than `string` because of a YAML 1.1 quirk that matters here: `on` is
		// a boolean literal, so yaml.v2 unmarshals the trigger key as `true` and not as the string
		// "on". A `map[string]any` finds nothing, and this test then passes vacuously on every file.
		var doc map[any]any
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Errorf("%s: parse: %v", e.Name(), err)

			continue
		}

		on := triggerSection(doc)
		if on == nil {
			t.Errorf("%s declares no `on:` section, or it is not a mapping. Every workflow needs a "+
				"trigger; if this file's shape changed, update this test rather than dropping the check",
				e.Name())

			continue
		}

		// `pull_request_target` runs with a write token against a fork's head, so it is if anything
		// more important that it not silently skip.
		for _, event := range []string{"pull_request", "pull_request_target"} {
			spec, ok := on[event]
			if !ok {
				continue
			}
			triggers++

			// `pull_request:` with no body is the wanted shape and unmarshals to nil.
			cfg, isMapping := spec.(map[any]any)
			if !isMapping {
				continue
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
					e.Name(), event, key, filter)
			}
		}
	}

	// Five workflow files exist today. Below four, the walk has stopped finding them.
	if files < 4 {
		t.Fatalf("found %d workflow files in %s, expected at least 4 — this test is not looking at "+
			"what it claims to", files, dir)
	}

	// The non-vacuity guard, and the one that catches the YAML 1.1 `on`-is-true trap above: three
	// workflows carry a pull-request trigger today. If this reaches zero, the lookup has broken and
	// every file is passing because nothing was inspected.
	if triggers == 0 {
		t.Fatalf("found no pull_request or pull_request_target trigger across %d workflow files. The "+
			"`on:` lookup has stopped matching — note that yaml.v2 parses the key `on` as the boolean "+
			"true — and this test now passes vacuously", files)
	}
	t.Logf("checked %d pull-request triggers across %d workflow files", triggers, files)
}

// triggerSection returns a workflow's `on:` mapping, handling the YAML 1.1 boolean-key quirk.
func triggerSection(doc map[any]any) map[any]any {
	for _, key := range []any{true, "on"} {
		if section, ok := doc[key].(map[any]any); ok {
			return section
		}
	}

	return nil
}
