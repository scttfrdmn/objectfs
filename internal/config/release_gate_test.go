package config

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

// The release gate and the CI gate must be the same gate.
//
// They were not. `.github/workflows/ci.yml` defined sixteen jobs, `.github/workflows/release.yml`
// defined five, and the two sets were disjoint — not overlapping, not a subset, disjoint. Neither
// file contained the string `workflow_call`. So a tag ran `build`, `package-linux`,
// `docker-build-push`, `security-scan` and `publish`, and a pull request ran `test`, `lint`,
// `coverage`, `fuzz-smoke` and twelve others, and no commit was ever verified by both. v0.14.0 was
// published having never had `go test` run against it by the workflow that published it.
//
// The fix is that release.yml *calls* ci.yml. The tests here pin the four properties that make
// that fix hold, because each of them can be undone by an edit that looks harmless:
//
//   - ci.yml is callable at all (`workflow_call` in its triggers). Delete that trigger and
//     release.yml's `gate` job fails to resolve — loudly, but only on a tag, which is the one place
//     nobody is watching a red check.
//   - release.yml calls it, and calls *it* rather than a copy.
//   - every job in release.yml is transitively downstream of that call, so a new job cannot be
//     added outside the gate by omission.
//   - the calling job grants every permission ci.yml's jobs ask for. This is the failure that does
//     not announce itself: a called workflow's token is derived from the caller's grants and can
//     only be narrowed, so a missing scope produces a token that silently cannot do the thing.
//
// A fifth test goes the other way and pins the check *names*, because renaming a job is how you
// orphan a required status check and block every pull request with nothing red to point at.

// gateCallPath is the value release.yml's gate job must carry in `uses:`. A local path, so the
// called workflow is always the one from the same commit as the caller — a tag calls the ci.yml
// that tag points at, not main's.
const gateCallPath = "./.github/workflows/ci.yml"

// workflowFile is the subset of workflow syntax these tests reason about.
//
// `On` is tagged `yaml:"on"` and that is load-bearing with yaml.v2, which implements YAML 1.1:
// a bare `on` key parses as the boolean true, so unmarshalling into a map gives a `true` key and
// no `"on"` key at all. Struct field matching resolves it; a map would not. Measured, not assumed.
type workflowFile struct {
	Name        string                    `yaml:"name"`
	On          map[string]interface{}    `yaml:"on"`
	Permissions map[string]string         `yaml:"permissions"`
	Jobs        map[string]workflowJobDef `yaml:"jobs"`
}

// workflowJobDef is one job. `Needs` is a string or a list of strings in the schema, so it is held
// as an interface and normalised by needsOf.
type workflowJobDef struct {
	Name        string            `yaml:"name"`
	Uses        string            `yaml:"uses"`
	Needs       interface{}       `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
}

// needsOf normalises a job's `needs:` into a slice. The schema allows a bare string for the
// single-dependency case, and treating that as absent would make every gated-by-one-job chain look
// like a root.
func needsOf(job workflowJobDef) []string {
	switch v := job.Needs.(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []interface{}:
		out := make([]string, 0, len(v))

		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}

		return out
	default:
		return nil
	}
}

// readWorkflow parses one file under .github/workflows.
func readWorkflow(t *testing.T, name string) workflowFile {
	t.Helper()

	path := filepath.Join(repoRoot(t), ".github", "workflows", name)

	var wf workflowFile
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &wf); err != nil {
		t.Fatalf("parsing .github/workflows/%s: %v", name, err)
	}

	if len(wf.Jobs) == 0 {
		t.Fatalf(".github/workflows/%s parsed with zero jobs. Either the file moved or the parse is "+
			"wrong, and a test that inspects nothing passes", name)
	}

	return wf
}

// TestCIWorkflowIsReusable asserts ci.yml can be called by another workflow.
func TestCIWorkflowIsReusable(t *testing.T) {
	t.Parallel()

	ci := readWorkflow(t, "ci.yml")

	if _, ok := ci.On["workflow_call"]; !ok {
		t.Error(".github/workflows/ci.yml has no `workflow_call` trigger, so release.yml cannot call " +
			"it and a tag is back to running its own private subset of the gate. That is the state " +
			"v0.14.0 shipped in: it was published without `test`, `lint`, `coverage`, `fuzz-smoke`, " +
			"`packaging`, `modulefiles`, `install-script` or `labels` ever having run.\n" +
			"Restore `workflow_call:` under `on:`. Do not instead move these jobs into a separate " +
			"gate.yml — that renames every check to `<caller> / <job>` and orphans all nineteen " +
			"required checks that are pinned to the bare names.")
	}

	// The triggers that keep the check names unchanged. If ci.yml stopped being the top-level
	// workflow for push and pull_request, the required checks would report under a different name.
	for _, trigger := range []string{"push", "pull_request"} {
		if _, ok := ci.On[trigger]; !ok {
			t.Errorf(".github/workflows/ci.yml no longer has an `%s` trigger. Required status checks "+
				"on main are pinned to bare job names (`test`, `lint`, ...), which is only what they "+
				"report as while ci.yml is the top-level workflow for that event", trigger)
		}
	}
}

// TestReleaseIsGatedByCI asserts every release.yml job is transitively downstream of the job that
// calls ci.yml.
//
// Reachability rather than the literal `needs:` entries, so the graph is free to be rearranged as
// long as the property survives. What it forbids is a job with no path back to the gate.
func TestReleaseIsGatedByCI(t *testing.T) {
	t.Parallel()

	release := readWorkflow(t, "release.yml")

	var callers []string

	for id, job := range release.Jobs {
		if job.Uses == gateCallPath {
			callers = append(callers, id)
		}
	}

	sort.Strings(callers)

	if len(callers) == 0 {
		t.Fatalf(".github/workflows/release.yml has no job with `uses: %s`. The release then runs a "+
			"different, smaller set of jobs than any pull request does, which is exactly how v0.14.0 "+
			"was published without ever being tested. Its jobs are: %s", gateCallPath,
			strings.Join(sortedMapKeys(release.Jobs), ", "))
	}

	gated := map[string]bool{}
	for _, id := range callers {
		gated[id] = true
	}

	// Fixed point over `needs:`. The graph is a DAG by Actions' own validation, so this terminates.
	for changed := true; changed; {
		changed = false

		for id, job := range release.Jobs {
			if gated[id] {
				continue
			}

			for _, dep := range needsOf(job) {
				if gated[dep] {
					gated[id] = true
					changed = true

					break
				}
			}
		}
	}

	for _, id := range sortedMapKeys(release.Jobs) {
		if gated[id] {
			continue
		}

		t.Errorf(".github/workflows/release.yml's %q job has no `needs:` path to %s, so it runs "+
			"whether the gate passed or not.\n"+
			"That matters most for the jobs with effects outside this run — docker-build-push pushes "+
			"an image to ghcr and publish creates the release page, and neither is retractable once "+
			"a user has pulled it. Add `needs: %s` (or a dependency on a job that has one).",
			id, gateCallPath, strings.Join(callers, ", "))
	}
}

// TestReleaseDoesNotRestateTheGate asserts release.yml does not carry its own copy of a CI job.
//
// A copy is worse than an omission, because it reports green under a familiar name while having
// drifted. This is the check that would have caught the original state of these two files.
func TestReleaseDoesNotRestateTheGate(t *testing.T) {
	t.Parallel()

	ci := readWorkflow(t, "ci.yml")
	release := readWorkflow(t, "release.yml")

	for _, id := range sortedMapKeys(release.Jobs) {
		if _, clash := ci.Jobs[id]; !clash {
			continue
		}

		if release.Jobs[id].Uses == gateCallPath {
			continue
		}

		t.Errorf(".github/workflows/release.yml defines a job named %q and so does ci.yml. There must "+
			"be exactly one definition of the gate; two jobs with one name is the drift this "+
			"arrangement exists to prevent. Delete the copy in release.yml — calling ci.yml already "+
			"runs it.", id)
	}
}

// TestGateCallerGrantsWhatTheGateNeeds asserts the calling job's `permissions:` cover the union of
// what ci.yml's jobs ask for.
//
// This is the quiet one. Actions does not error when a called workflow requests a scope the caller
// did not grant — it narrows the token instead. ci.yml's `labels` job needs `issues: read` to list
// the repository's labels, and with a token that cannot, `go test` still exits 0 because the test
// skips. The job greps its own output for that skip precisely because a skipped gate reports
// success, but relying on that is relying on a second mechanism to catch a mistake this one can
// just prevent.
func TestGateCallerGrantsWhatTheGateNeeds(t *testing.T) {
	t.Parallel()

	ci := readWorkflow(t, "ci.yml")
	release := readWorkflow(t, "release.yml")

	// Union over the workflow default and every job override.
	needed := map[string]string{}

	merge := func(perms map[string]string) {
		for scope, level := range perms {
			if permissionRank(level) > permissionRank(needed[scope]) {
				needed[scope] = level
			}
		}
	}

	merge(ci.Permissions)

	for _, job := range ci.Jobs {
		merge(job.Permissions)
	}

	if len(needed) == 0 {
		t.Fatal("read no `permissions:` out of .github/workflows/ci.yml. It has a workflow-level " +
			"block and at least one job-level one, so this parsed wrong and the check below is vacuous")
	}

	for _, callerID := range sortedMapKeys(release.Jobs) {
		caller := release.Jobs[callerID]
		if caller.Uses != gateCallPath {
			continue
		}

		if len(caller.Permissions) == 0 {
			t.Errorf(".github/workflows/release.yml's %q job calls the gate with no `permissions:` "+
				"block, so it takes the workflow default. ci.yml's jobs ask for %s; a called workflow "+
				"cannot widen what the caller granted, so any of those not in the default is silently "+
				"unavailable rather than an error", callerID, formatPermissions(needed))

			continue
		}

		for _, scope := range sortedMapKeys(needed) {
			if permissionRank(caller.Permissions[scope]) >= permissionRank(needed[scope]) {
				continue
			}

			t.Errorf(".github/workflows/release.yml's %q job grants %s=%q to the gate, but a job in "+
				"ci.yml asks for %s=%q. A called workflow's token is derived from the caller's grants "+
				"and can only be narrowed, never widened — this does not fail the run, it hands the "+
				"job a token that cannot do the thing and lets it report on a skip.",
				callerID, scope, caller.Permissions[scope], scope, needed[scope])
		}
	}
}

// requiredCheckSuffix matches the matrix suffix Actions appends to a check name, e.g. the
// ` (linux, arm, 7)` in `cross-build (linux, arm, 7)`.
var requiredCheckSuffix = regexp.MustCompile(` \(.*\)$`)

// requiredChecks is the required-status-check list configured on main, verbatim, as returned by
//
//	gh api repos/scttfrdmn/objectfs/branches/main/protection/required_status_checks
//
// It is transcribed here rather than fetched because the point is to fail on a *local* rename,
// before the push that orphans the check — a job renamed in this tree reports under the new name
// and the old name never reports again, which presents as every pull request blocking forever with
// nothing red to point at. There is no failing check to read, because the check is absent.
//
// Branch protection is a repository setting and is not this repository's to change from code; if a
// rename is genuinely wanted, update this list *and* say in the pull request which `gh api` call
// the maintainer has to run against `.../branches/main/protection/required_status_checks`.
//
// `Security Scan` is the one entry that is not defined in ci.yml: it is security.yml's `security`
// job, whose `name:` is `Security Scan`. It is a job name and not the SARIF-derived check the
// scanner also produces, deliberately — a SARIF check does not report on `push: main`, so only a
// job name can be required. Do not "tidy" this by pointing it at the scanner's check.
var requiredChecks = []string{
	"test",
	"lint",
	"coverage",
	"config-examples",
	"cross-build (linux, amd64)",
	"cross-build (linux, arm64)",
	"cross-build (darwin, amd64)",
	"cross-build (darwin, arm64)",
	"build-tags (aws_s3)",
	"build-tags (benchmark)",
	"build-tags (distributed)",
	"build-tags (e2e)",
	"build-tags (integration)",
	"build-tags (fuse_mount)",
	"cross-build (linux, 386)",
	"cross-build (linux, arm, 7)",
	"modulefiles (lmod)",
	"modulefiles (tcl-modules)",
	"packaging",
	"Security Scan",
}

// TestRequiredChecksStillHaveAJobToReportThem asserts every name in requiredChecks is still
// produced by a job in ci.yml or security.yml.
func TestRequiredChecksStillHaveAJobToReportThem(t *testing.T) {
	t.Parallel()

	produced := map[string]string{}

	for _, file := range []string{"ci.yml", "security.yml"} {
		for id, job := range readWorkflow(t, file).Jobs {
			// Both, because a matrix job reports as `<name-or-id> (<cells>)` and either half can be
			// the stable part. `modulefiles` sets `name: modulefiles (${{ matrix.system }})`
			// specifically so its two required contexts do not carry an absolute interpreter path
			// that a distro can move; the suffix is stripped from the declared name for the same
			// reason it is stripped from the required-check string.
			produced[id] = file

			if job.Name != "" {
				produced[requiredCheckSuffix.ReplaceAllString(job.Name, "")] = file
			}
		}
	}

	for _, check := range requiredChecks {
		base := requiredCheckSuffix.ReplaceAllString(check, "")

		if _, ok := produced[base]; ok {
			continue
		}

		t.Errorf("%q is a required status check on main, but no job in ci.yml or security.yml "+
			"reports under %q any more.\n"+
			"A required check with nothing to report it never reports, so every pull request blocks "+
			"forever and no check is red to explain why. Either restore the name, or change branch "+
			"protection in the same change — and if you nested the job inside a reusable workflow, "+
			"note that it now reports as `<caller-job-id> / %s`, which is a different string.",
			check, base, base)
	}
}

// sortedMapKeys returns a map's keys in order, so failures are deterministic.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// permissionRank orders the three values a permission scope can take. Absent and `none` are the
// same thing for the purposes of "does the caller grant enough".
func permissionRank(level string) int {
	switch level {
	case "read":
		return 1
	case "write":
		return 2
	default:
		return 0
	}
}

// formatPermissions renders a scope set for an error message.
func formatPermissions(perms map[string]string) string {
	out := make([]string, 0, len(perms))
	for _, scope := range sortedMapKeys(perms) {
		out = append(out, scope+"="+perms[scope])
	}

	return strings.Join(out, ", ")
}
