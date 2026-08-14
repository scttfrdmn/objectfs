package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file gates the gosec baseline and the step that reads it.
//
// The mechanism is small and its failure mode is silent, which is the combination that needs a test.
// `Security Scan` runs `gosec -no-fail`, so the job's exit status carries no information about
// findings; .github/scripts/gosec-gate.sh supplies it by diffing the report against
// .github/gosec-baseline.txt. Remove the step, or point it at a file that does not exist, and the job
// goes green on any number of new integer conversions — which is exactly the state #415 was filed
// about, where a G115 added by #414 reported "1 new alert" and the PR stayed mergeable because the
// check that went red was not among main's required contexts.
//
// What is checked here is what a reader of the two files cannot check for themselves: that every path
// named in the baseline still exists, and that the workflow still invokes the script. Whether the
// *counts* are right is the script's own job, run against the real report on every push.

// gosecBaseline reads .github/gosec-baseline.txt.
func gosecBaseline(t *testing.T) string {
	t.Helper()

	path := filepath.Join(repoRoot(t), ".github", "gosec-baseline.txt")

	//nolint:gosec // A path built from the repository root, in a test.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v.\nThe Security Scan job's gate reads this file and fails "+
			"without it, so its absence is loud rather than silent — but a gate that cannot run is "+
			"still a gate that is not gating", path, err)
	}

	return string(b)
}

// TestGosecBaselineNamesFilesThatExist is the staleness check.
//
// A baseline entry is a standing allowance for findings in one file. When that file is deleted or
// renamed the entry keeps its allowance and nothing points at it any more: the script would report the
// entry as a fixed finding, which is a true statement said misleadingly, and the reflex fix is to
// delete the line rather than to notice the findings moved somewhere unaccounted for.
//
// Checking the paths resolve turns that into a named failure at the point the rename happens.
func TestGosecBaselineNamesFilesThatExist(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	entries := 0

	for line := range strings.SplitSeq(gosecBaseline(t), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Errorf(".github/gosec-baseline.txt line %q has %d fields, and the script parses exactly "+
				"three: rule, file, count. A line it cannot parse becomes an entry that matches no "+
				"finding, which fails the gate with a diff about the wrong thing", line, len(fields))

			continue
		}

		entries++

		rule, file, count := fields[0], fields[1], fields[2]

		if n, err := strconv.Atoi(count); err != nil || n < 1 {
			t.Errorf(".github/gosec-baseline.txt gives %s %s a count of %q. It must be a positive "+
				"integer — a zero-count entry accounts for nothing and should be deleted instead", rule, file, count)
		}

		if _, err := os.Stat(filepath.Join(root, file)); err != nil {
			t.Errorf(".github/gosec-baseline.txt baselines %s %s finding(s) in %s, and there is no such "+
				"file. Either it was renamed — in which case the findings are now unaccounted for and "+
				"the entry has to move with it — or it was deleted, and the entry is a standing "+
				"allowance pointing at nothing", count, rule, file)
		}
	}

	// The gate is a diff against this file, so an empty baseline does not fail open: it would fail
	// every run instead. What it would do is destroy the distinction the mechanism exists for, since
	// the only way back to green is to re-add the twelve pre-existing findings, and the fast way to do
	// that is to stop checking. Assert the file still has content.
	if entries < 5 {
		t.Fatalf("found %d parsable entries in .github/gosec-baseline.txt; there are twelve "+
			"pre-existing G115 findings across eight files, so the format has changed or the file has "+
			"been emptied", entries)
	}
}

// TestSecurityWorkflowGatesOnGosecFindings couples the workflow to the script.
//
// Deleting the step is the change that restores the original defect while every check stays green,
// because the thing it removes is the only thing that could have gone red. So it is asserted rather
// than left to the comment above it.
//
// The `-no-fail` half is asserted too, in the opposite direction from what a reader might expect: the
// flag must **stay**. Dropping it looks like tightening the gate and actually loosens it — the job
// would end before the SARIF format fix and the upload, so the three sdks/c findings would never get
// their path back and would reach no tool at all, which is how #200's real defect survived.
func TestSecurityWorkflowGatesOnGosecFindings(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	path := filepath.Join(root, ".github", "workflows", "security.yml")

	//nolint:gosec // A path built from the repository root, in a test.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}

	workflow := withoutComments(string(b))

	const script = ".github/scripts/gosec-gate.sh"

	if !strings.Contains(workflow, script) {
		t.Errorf(".github/workflows/security.yml does not run %s. gosec runs with -no-fail, so with "+
			"that step gone the job passes on any number of findings and the only check that goes red "+
			"is the code-scanning one, which is a different check and does not report on push: main "+
			"(#415)", script)
	}

	if !strings.Contains(workflow, "-no-fail") {
		t.Error(".github/workflows/security.yml no longer passes -no-fail to gosec. That reads like a " +
			"tightening and is a loosening: the job would end before the SARIF format fix and the " +
			"upload, so the sdks/c findings never get a path back and are reviewed by nothing. The " +
			"gate is the gosec-gate.sh step, not gosec's exit status")
	}

	// A relative path in a `run:` resolves against the workspace root, and the script has to be
	// executable — checkout preserves the mode bit, so a file committed without +x fails the step with
	// "Permission denied" and nothing about gosec.
	info, err := os.Stat(filepath.Join(root, script))
	if err != nil {
		t.Fatalf("%s is referenced by security.yml and does not exist: %v", script, err)
	}

	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable (mode %v). The workflow invokes it directly, so the job would "+
			"fail with a permission error that says nothing about security findings", script, info.Mode().Perm())
	}
}
