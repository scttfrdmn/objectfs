package config

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Vetting a tagged suite is not running it.
//
// build_tags_test.go keeps every build tag *compiled*: a cell per tag in ci.yml's build-tags job, and a
// test that fails when a tag has none. That closed #240, where four tagged suites had stopped building.
// It did not make any of them run, and #570 found what that cost. Two suites that need nothing at all
// had been compiled and never executed for as long as the job existed, and the first execution of each
// was red. e2e still asserted wording the code had improved past. distributed waited on a consensus
// engine that #401 made opt-in "because the -tags=distributed suite drives elections deliberately", and
// then nothing turned it on. A compiler cannot see either; only a run can.
//
// So this is the same gate one level up: every build tag a test file uses must be passed to a `go test`
// that some workflow job runs, not just to `go vet`.

// executedTagsExempt are the test-file build tags no job executes yet, each with the open issue that
// tracks running it. An entry is a debt with a name on it, not a permanent excuse: the test fails if an
// exempt tag starts being executed (the entry is stale) or stops existing in the tree.
var executedTagsExempt = map[string]string{
	// Real AWS: the only live verification of conditional writes, which every CAS guarantee rests on.
	// Needs an OIDC role and a scheduled job.
	"integration": "#570",
	// Real AWS plus OBJECTFS_TEST_BUCKET, and it skips without one, so its job must also assert that
	// it ran.
	"aws_s3": "#570",
	// Not hermetic in the sense that matters: a timing result on a shared runner is noise, not signal.
	"benchmark": "#568",
}

// goTestTags matches the -tags value of a `go test` invocation on one line. `go vet -tags=…` does not
// match, which is the point: vetting is what build-tags already does.
var goTestTags = regexp.MustCompile(`\bgo test\b[^\n]*?-tags[= ]"?([^"\s]+)"?`)

// listFlag matches `-list`, which makes `go test` print test names and run none.
var listFlag = regexp.MustCompile(`\s-list\b`)

// TestEveryTestTagIsExecuted asserts every build tag used in a _test.go file is executed by a job.
func TestEveryTestTagIsExecuted(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	inTree := map[string][]string{}

	for _, path := range goFiles(t, root) {
		if !strings.HasSuffix(path, "_test.go") {
			continue
		}

		for _, tag := range tagsInFile(t, path) {
			rel, _ := filepath.Rel(root, path)
			inTree[tag] = append(inTree[tag], rel)
		}
	}

	// Seven when this was written. A floor, so a walk that stops finding constraints cannot pass by
	// having nothing to check.
	if len(inTree) < 6 {
		t.Fatalf("found %d build tags in _test.go files, expected at least 6. The walk or the //go:build "+
			"parse has stopped matching", len(inTree))
	}

	executed := map[string]string{}

	for _, wf := range readWorkflows(t) {
		for _, id := range sortedMapKeys(wf.File.Jobs) {
			job := wf.File.Jobs[id]

			for _, step := range jobSteps(job) {
				for line := range strings.SplitSeq(withoutComments(step.Run), "\n") {
					// `go test -list` prints test names and runs none. Both jobs that execute tags also
					// list them first, to find the tag-only tests, and this test's first version credited
					// that line: deleting the tag from the run that executes the suite left it passing,
					// because the -list beside it still matched. Listing is not executing.
					//
					// Checked against the whole line, not the match. The second version checked the
					// match, which ends at the -tags value — and `go test -tags=x -list .` puts -list
					// after it, so the same two mutations survived again.
					if listFlag.MatchString(line) {
						continue
					}

					for _, m := range goTestTags.FindAllStringSubmatch(line, -1) {
						for _, tag := range resolveTagValue(m[1], step, job) {
							executed[tag] = wf.Name + " job " + id
						}
					}
				}
			}
		}
	}

	// The two this test was written alongside. Without them the resolution below has stopped
	// working — `$TAG` through env through `matrix.tag` is the path tagged-suite takes — and every
	// tag would be reported unexecuted for that reason rather than its own.
	for _, want := range []string{"fuse_mount", "e2e"} {
		if _, ok := executed[want]; !ok {
			t.Fatalf("found no job executing the %s tag, which ci.yml does run. The `go test -tags` "+
				"match or its $TAG / matrix.tag resolution has stopped working", want)
		}
	}

	for _, tag := range sortedMapKeys(inTree) {
		where, ran := executed[tag]
		issue, exempt := executedTagsExempt[tag]

		switch {
		case ran && exempt:
			t.Errorf("the %s build tag is exempt in executedTagsExempt (%s) but %s executes it. Remove "+
				"the exemption — a stale one would excuse the tag the day that job stops running it",
				tag, issue, where)
		case !ran && !exempt:
			t.Errorf("the %s build tag is used by %s and no workflow job runs `go test -tags=%s`. "+
				"build-tags vets it, which proves it compiles and nothing more: e2e and distributed were "+
				"in exactly this state and were both red on their first run (#570). If the suite needs "+
				"nothing, add it to ci.yml's tagged-suite matrix. If it needs something CI does not have, "+
				"add it to executedTagsExempt with the issue tracking that.",
				tag, strings.Join(inTree[tag], ", "), tag)
		}
	}

	for _, tag := range sortedMapKeys(executedTagsExempt) {
		if _, ok := inTree[tag]; !ok {
			t.Errorf("executedTagsExempt lists %s, which no _test.go file uses any more. Remove it.", tag)
		}
	}
}

// resolveTagValue turns a -tags argument into the tags it names.
//
// Three shapes occur. A literal (`fuse_mount`). A matrix expression (`${{ matrix.tag }}`), which names
// every value of the job's `tag:` axis. And a shell variable (`$TAG`) set from the step's env, which is
// how tagged-suite passes the matrix value without interpolating an expression into a run block. A
// value that resolves to none of these is returned empty, so a new indirection reads as "not executed"
// rather than being credited to whatever its text happens to look like.
func resolveTagValue(value string, step workflowStep, job workflowJobDef) []string {
	value = strings.TrimSpace(value)

	if name, ok := strings.CutPrefix(value, "$"); ok {
		name = strings.Trim(name, "{}")

		env, ok := step.Env[name]
		if !ok {
			return nil
		}

		value = env
	}

	if strings.Contains(value, "matrix.tag") {
		out := append([]string(nil), job.Strategy.Matrix.Tag...)
		sort.Strings(out)

		return out
	}

	if strings.ContainsAny(value, "${}") {
		return nil
	}

	return strings.Split(value, ",")
}
