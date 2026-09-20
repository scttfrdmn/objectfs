package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v2"
)

// One parser for `.github/workflows`, and the reason it is one.
//
// There were four, in `release_gate_test.go` (a struct), `release_signing_test.go` (a second struct
// for one job's two keys), `job_timeouts_test.go` and `pull_request_triggers_test.go` (two
// `map[any]any` walks). #504 was filed when there were two; the fourth arrived with #531's timeout
// gate. `release_signing_test.go`'s own comment on the parser it is replacing said the quiet part:
// it was local "so this file stands alone against `main` and can merge in either order relative to
// the gate change", with a note to converge once both had landed. Both landed.
//
// The cost of four was not the duplication. It was that each copy had to rediscover the two traps
// below, and each rediscovery was a bug first.
//
// # Trap one: `on` is the boolean `true`
//
// yaml.v2 implements YAML 1.1, where `on` is a boolean literal. So a bare `on:` key does not
// unmarshal as the string "on" — it unmarshals as `true`. A `map[string]any` finds no trigger
// section at all, and a test that walks triggers then passes on every file having inspected nothing.
//
// A struct field resolves it, which is why [workflowFile.On] is tagged `yaml:"on"` and why that tag
// is load-bearing rather than decorative. For the map form the key has to be looked up as
// `[]any{true, "on"}` — see [triggerSection], which keeps the second spelling because a future
// yaml.v3 move would flip which one is right and finding neither must stay distinguishable from
// finding an empty one.
//
// # Trap two: a step-scoped assertion has to cut the step out first
//
// Searching a whole workflow for a command finds it in places nothing executes. `cosign verify-blob`
// appears inside a release-notes `echo` in `release.yml`, so an assertion that the verification runs
// was satisfied by the *documentation* of it — and a mutation deleting the executed verification
// still passed. [cutStep] exists for that, and a step-scoped claim that does not use it is checking
// the file rather than the step.

// workflowFile is the subset of workflow syntax this package's tests reason about. It is deliberately
// a subset: a full schema would be a second implementation of Actions, and every field here is one
// some test asserts on.
type workflowFile struct {
	Name string `yaml:"name"`

	// On is tagged rather than looked up, which is what makes the YAML 1.1 `on`-is-true trap above a
	// non-issue for struct consumers. Values stay `any` because yaml.v2 gives nested mappings as
	// `map[any]any`; `triggerSection` is the accessor that knows that.
	On map[string]any `yaml:"on"`

	Permissions map[string]string         `yaml:"permissions"`
	Env         map[string]string         `yaml:"env"`
	Jobs        map[string]workflowJobDef `yaml:"jobs"`
}

// workflowJobDef is one job.
type workflowJobDef struct {
	Name string `yaml:"name"`
	Uses string `yaml:"uses"`

	// Needs is a string or a list of strings in the schema, so it is held as an interface and
	// normalised by needsOf.
	Needs any `yaml:"needs"`

	Permissions map[string]string `yaml:"permissions"`
	Env         map[string]string `yaml:"env"`

	// TimeoutMinutes is `any` because three states are distinct and a test reports each differently:
	// absent (nil) is the 360-minute default nobody chose, a non-int is an expression that makes the
	// bound depend on the triggering event, and an int is the only answer. An `int` field would fold
	// the first two together as 0.
	TimeoutMinutes any `yaml:"timeout-minutes"`

	// Steps distinguishes a reusable-workflow caller from a job that merely also has `uses:` on a
	// step. Held as `any` and tested for nil rather than as a slice, so presence is presence.
	//
	// One difference from the `map[any]any` lookup this replaces, stated rather than hidden: an
	// explicit `steps:` with a null value used to count as present and now does not. Actions rejects
	// that file either way, so no workflow can be in that state and be running.
	Steps any `yaml:"steps"`
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
	case []any:
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

// triggerSection returns one event's configuration from a workflow's `on:` mapping, and whether the
// event is present at all.
//
// The two return values are not the same question and collapsing them is a bug: `pull_request:` with
// no body is the *wanted* shape and unmarshals to nil, so "present with no filters" and "absent"
// both give a nil mapping. A walk that cannot tell them apart counts no triggers and reports a clean
// file rather than a broken lookup.
func triggerSection(wf workflowFile, event string) (map[any]any, bool) {
	spec, present := wf.On[event]
	if !present {
		return nil, false
	}

	cfg, _ := spec.(map[any]any)

	return cfg, true
}

// readWorkflow parses one file under .github/workflows by name.
func readWorkflow(t *testing.T, name string) workflowFile {
	t.Helper()

	return parseWorkflow(t, name, workflowSource(t, name))
}

// workflowSource returns one workflow's text, verbatim.
//
// Text rather than a parse, because plenty of what these tests assert is not addressable through a
// parser: a `run:` block's shell, an `uses:` reference and the version comment beside it, a `${{ }}`
// expression. Those callers want the bytes. What they should not each carry is the path — twelve
// files built `filepath.Join(repoRoot(t), ".github", "workflows", name)` for themselves, which is
// twelve places to fix if the directory ever moves and twelve chances for one of them to keep reading
// a file that is no longer there.
func workflowSource(t *testing.T, name string) string {
	t.Helper()

	return readFile(t, filepath.Join(workflowsDir(t), name))
}

// parseWorkflow unmarshals one workflow, with the reach assertion every caller needs.
func parseWorkflow(t *testing.T, name, body string) workflowFile {
	t.Helper()

	var wf workflowFile
	if err := yaml.Unmarshal([]byte(body), &wf); err != nil {
		t.Fatalf("parsing .github/workflows/%s: %v", name, err)
	}

	if len(wf.Jobs) == 0 {
		t.Fatalf(".github/workflows/%s parsed with zero jobs. Either the file moved or the parse is "+
			"wrong, and a test that inspects nothing passes", name)
	}

	return wf
}

// workflowsDir returns .github/workflows.
func workflowsDir(t *testing.T) string {
	t.Helper()

	return filepath.Join(repoRoot(t), ".github", "workflows")
}

// namedWorkflow pairs a parsed workflow with its filename, which every failure message needs.
type namedWorkflow struct {
	Name string
	File workflowFile
}

// workflowText is one workflow's filename, repo-relative path and verbatim body.
type workflowText struct {
	Name string
	Path string
	Body string
}

// workflowFileFloor is a non-vacuity floor shared by every test that walks the directory. Five
// workflow files exist; below four the walk has stopped finding them and each per-file assertion is
// trivially satisfied with nothing to show it read anything.
//
// One floor, not one per caller. There were four copies of the number 4, each with its own comment
// saying five files exist, which is four places to update and three of them will be missed.
const workflowFileFloor = 4

// readWorkflowTexts reads every workflow in .github/workflows, in directory order.
//
// Walked rather than enumerated, and that is the point of it existing: a list checks the files that
// existed when it was written, and the workflow that arrives with a missing timeout, an unpinned
// action or a copied `branches: [main]` is by definition one nobody thought to add to a list.
//
// Both extensions are accepted. That is not hypothetical tidiness: one of the four walks this replaces
// filtered on `filepath.Ext(name) != ".yml"`, so a `.yaml` workflow would have been skipped by it and
// checked by the other three — and skipping is the direction that looks like passing.
func readWorkflowTexts(t *testing.T) []workflowText {
	t.Helper()

	dir := workflowsDir(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	out := []workflowText{}

	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}

		body, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- a directory entry from .github/workflows
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Join(dir, e.Name()), err)
		}

		out = append(out, workflowText{
			Name: e.Name(),
			Path: filepath.Join(".github", "workflows", e.Name()),
			Body: string(body),
		})
	}

	if len(out) < workflowFileFloor {
		t.Fatalf("found %d workflow files in %s, expected at least %d — the walk is not looking at "+
			"what it claims to, and every caller's per-file assertions are passing on an empty set",
			len(out), dir, workflowFileFloor)
	}

	return out
}

// readWorkflows parses every workflow in .github/workflows, in directory order.
//
// A parse failure is fatal rather than skipped. The per-file loops this replaces reported and
// continued so one bad file would not hide another, which is the right instinct for a finding — but a
// workflow that does not parse runs no jobs at all, so there is no second finding behind it to
// protect.
func readWorkflows(t *testing.T) []namedWorkflow {
	t.Helper()

	texts := readWorkflowTexts(t)

	out := make([]namedWorkflow, 0, len(texts))
	for _, wt := range texts {
		out = append(out, namedWorkflow{Name: wt.Name, File: parseWorkflow(t, wt.Name, wt.Body)})
	}

	return out
}

// cutStep returns the body of the named workflow step, from its `- name:` line to the next one.
//
// Scoping to the step is not tidiness. `release.yml` prints its own verification instructions in a
// release-notes `echo`, so `cosign verify-blob` appears twice in that file and only one of them runs:
// an assertion searching the whole source was satisfied by the documentation of the command, and a
// mutation deleting the *executed* verification passed. Any claim about what a step does has to cut
// the step out first.
//
// A line scan, and the two properties that come from it are both things the `strings.Index` version
// this replaces got wrong. The name is matched against the *whole* trimmed line, so a step named
// `Sign` does not match `Sign and verify` and return a body belonging to another step; and the step
// ends at the next `- name:` at any indentation, rather than at a hardcoded four spaces, so
// re-indenting a workflow — or reading one whose jobs nest differently — cannot silently extend a
// step's body to the end of the file. There is no YAML parse here on purpose: the subject is the text
// of a `run:` block, which a parser would hand back as one opaque string anyway.
func cutStep(source, name string) (string, bool) {
	var (
		body   []string
		inStep bool
	)

	for line := range strings.SplitSeq(source, "\n") {
		trimmed := strings.TrimSpace(line)

		if trimmed == "- name: "+name {
			inStep = true

			continue
		}

		if !inStep {
			continue
		}

		if strings.HasPrefix(trimmed, "- name: ") {
			break
		}

		body = append(body, line)
	}

	if !inStep {
		return "", false
	}

	return strings.Join(body, "\n"), true
}

// jobStep is cutStep for a caller that has no second thing to say when the step is missing.
//
// The failure is fatal rather than reported, because a gate whose subject is absent does not fail —
// it passes, having asserted nothing about anything. That is the only reason this wrapper exists; if
// a caller wants to handle absence itself, it should call cutStep.
func jobStep(t *testing.T, workflow, stepName string) string {
	t.Helper()

	body, ok := cutStep(workflow, stepName)
	if !ok {
		t.Fatalf("found no step named %q. If it was renamed, point this test at the new name — a gate "+
			"that cannot find its subject passes for the wrong reason", stepName)
	}

	return body
}

// jobNamed returns the job whose `name:` is the given display name, with its id.
//
// Looked up by `name:` rather than by id because that is what a required status check reports as, so
// it is the string the rest of the repository is pinned to — see requiredChecks in
// release_gate_test.go. Fatal when absent, for the same reason as jobStep.
func jobNamed(t *testing.T, wf workflowFile, name string) (string, workflowJobDef) {
	t.Helper()

	for _, id := range sortedMapKeys(wf.Jobs) {
		if wf.Jobs[id].Name == name {
			return id, wf.Jobs[id]
		}
	}

	t.Fatalf("no job declares `name: %s`. Its jobs are: %s", name, strings.Join(sortedMapKeys(wf.Jobs), ", "))

	return "", workflowJobDef{}
}
