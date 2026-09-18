package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v2"
)

// timeoutCeiling is the largest `timeout-minutes` a job may declare.
//
// Three times the longest job in the repository, which is `release.yml`'s `docker-build-push` at 917
// seconds — a two-platform buildx of the whole binary. Everything else is under four minutes. The
// ceiling exists because the failure this test is about is not "no key present", it is "nothing bounds
// the job": `timeout-minutes: 360` passes a presence check and is GitHub's default, so it would satisfy
// the letter of the rule while leaving a wedged job holding a pull request for six hours. If a job
// genuinely needs more than an hour, raise this with the measurement that justifies it rather than
// exempting the job.
const timeoutCeiling = 60

// Every workflow job declares a `timeout-minutes`.
//
// GitHub's default is 360 minutes. Nothing in this repository takes six hours — the whole CI gate is
// under five minutes of wall clock, and the single longest job anywhere is a 15-minute multi-platform
// docker build — so a job that hits the default has not run long, it has hung: a `go test` deadlocked on
// a channel, an `apt-get` waiting on a prompt that will never come, a `curl` to a host that is not
// answering. Left to the default, that hang holds the pull request's required checks for six hours, and
// `cancel-in-progress` does not help because a new push cancels the run rather than the wait.
//
// The values were sized from observed durations rather than guessed: a generous multiple of each job's
// worst observed run, with a floor of 10 minutes so ordinary runner variance never trips one. The aim is
// that a hang fails in minutes and a slow day never fails at all.
//
// The directory is walked and the jobs are read out of the parsed YAML rather than enumerated, for the
// same reason as [TestNoPullRequestTriggerFiltersItsBaseBranch] — a list checks the jobs that existed
// when it was written, and the job that arrives without a timeout is by definition one nobody thought to
// add to a list.
//
// One exemption, and it is a syntax rule rather than a judgement call: a job that calls a reusable
// workflow with `uses:` may not carry `timeout-minutes` at all. The allowed keys on such a job are
// `name`, `uses`, `with`, `secrets`, `needs`, `if` and `permissions`, and adding an eighth fails the
// whole file at parse time — no job, no log. `release.yml`'s `gate` is that job.
//
// It is not an unbounded hole. The jobs inside the workflow it calls are jobs this test checks, and
// because `ci.yml` has no `needs:` at all its fifteen jobs run in parallel — so the caller is bounded by
// the longest of them, 20 minutes, not by their sum. That holds only while the call is local, which is
// why the exemption checks where `uses:` points.
func TestEveryWorkflowJobHasATimeout(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	files := 0
	checked := 0
	exempt := 0

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

		// map[any]any throughout, matching the other workflow tests: yaml.v2 reads a YAML 1.1 document,
		// where a bare `on` is the boolean true, and a map[string]any would find nothing.
		var doc map[any]any
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Errorf("%s: parse: %v", e.Name(), err)

			continue
		}

		jobs, ok := doc["jobs"].(map[any]any)
		if !ok {
			t.Errorf("%s declares no `jobs:` mapping. Every workflow has one; if this file's shape "+
				"changed, update this test rather than letting it skip the file", e.Name())

			continue
		}

		for id, spec := range jobs {
			job, ok := spec.(map[any]any)
			if !ok {
				t.Errorf("%s: job %v is not a mapping", e.Name(), id)

				continue
			}

			// The exemption is checked, not assumed: a reusable-workflow caller has `uses:` and no
			// `steps:`. A job with both is not a caller and does not get out of the rule.
			target, callsWorkflow := job["uses"].(string)
			_, hasSteps := job["steps"]

			if callsWorkflow && !hasSteps {
				if _, found := job["timeout-minutes"]; found {
					t.Errorf("%s: job %v calls a reusable workflow with `uses:` and also sets "+
						"`timeout-minutes`.\n"+
						"\tActions rejects that combination for the whole file, before any job starts:\n"+
						"\tthe only keys allowed beside `uses:` are name, with, secrets, needs, if and\n"+
						"\tpermissions. Remove it — the called workflow's own jobs carry the timeouts.",
						e.Name(), id)
				}

				// And the exemption only holds for a workflow whose jobs this test can see. A local
				// `./.github/workflows/x.yml` is walked by this loop, so the caller inherits real bounds
				// from real jobs. `owner/repo/.github/workflows/x.yml@ref` is not: its jobs live in
				// another repository at a ref this test does not read, and exempting it would mean a job
				// with no timeout of its own calling something with no timeout this repository can
				// verify — a genuine six-hour hole wearing the same shape as the legitimate exemption.
				// There is no way to bound such a job (the key is rejected), so the only honest outcome
				// is to fail and make someone decide.
				if !strings.HasPrefix(target, "./.github/workflows/") {
					t.Errorf("%s: job %v calls the external reusable workflow %q.\n"+
						"\tIt cannot carry `timeout-minutes` — Actions rejects the key beside `uses:` —\n"+
						"\tand this test cannot read the called workflow's jobs either, so nothing bounds\n"+
						"\tit. That is the one shape where this exemption stops being sound. Either\n"+
						"\tvendor the workflow into .github/workflows so its jobs are checked here, or\n"+
						"\treplace the call with a job that runs the action in `steps:` and can be\n"+
						"\tbounded.", e.Name(), id, target)
				} else if _, err := os.Stat(filepath.Join(repoRoot(t), strings.TrimPrefix(target, "./"))); err != nil {
					t.Errorf("%s: job %v calls %q, which does not exist: %v", e.Name(), id, target, err)
				}

				exempt++

				continue
			}

			checked++

			value, found := job["timeout-minutes"]
			if !found {
				t.Errorf("%s: job %v has no `timeout-minutes`.\n"+
					"\tIt therefore gets GitHub's default of 360 minutes, which no job here needs and\n"+
					"\tonly a hung one would reach — where it holds this repository's required checks\n"+
					"\tfor six hours. Add one sized from the job's observed runtime: a generous\n"+
					"\tmultiple of its worst run, floor 10, ceiling %d.", e.Name(), id, timeoutCeiling)

				continue
			}

			minutes, isInt := value.(int)
			if !isInt {
				t.Errorf("%s: job %v has `timeout-minutes: %v`, which is not a literal integer.\n"+
					"\tUse a number. An expression would make the bound depend on the event that\n"+
					"\ttriggered the run, which is the one thing a hang does not vary with.",
					e.Name(), id, value)

				continue
			}

			if minutes < 1 || minutes > timeoutCeiling {
				t.Errorf("%s: job %v has `timeout-minutes: %d`, outside 1..%d.\n"+
					"\tThe ceiling is three times the longest job in the repository. A bound above it\n"+
					"\tbounds nothing anyone is waiting on — 360 is the default this test exists to\n"+
					"\tdisplace. If the job really needs longer, raise timeoutCeiling with the\n"+
					"\tmeasurement that justifies it.", e.Name(), id, minutes, timeoutCeiling)
			}
		}
	}

	// Five workflow files, 24 jobs, of which exactly one is a reusable-workflow caller. These are
	// non-vacuity guards, not an inventory: the walk is what finds the jobs, and every one of these
	// floors was reached by a real mistake in a test in this package — a lookup that stopped matching
	// leaves each per-job assertion trivially satisfied, and the only tell is that nothing was counted.
	if files < 4 {
		t.Fatalf("found %d workflow files in %s, expected at least 4 — this test is not looking at "+
			"what it claims to", files, dir)
	}
	if checked < 20 {
		t.Fatalf("checked %d jobs across %d workflow files, expected at least 20. The `jobs:` lookup "+
			"has stopped matching, or the exemption is swallowing jobs it should not, and this test is "+
			"now passing on jobs it never read", checked, files)
	}
	if exempt > 2 {
		t.Errorf("%d jobs were exempted as reusable-workflow callers, expected 1 (`release.yml`'s "+
			"`gate`). Each exemption is a job with no timeout of its own; if a second caller is "+
			"deliberate, check that the workflow it calls has jobs this test can see", exempt)
	}
	t.Logf("checked %d jobs across %d workflow files, %d exempt as reusable-workflow callers",
		checked, files, exempt)
}
