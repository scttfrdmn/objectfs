// The label taxonomy in .github/labels.yml must match the labels the repository actually has.
//
// This is the fifth mechanical gate, alongside docs_test.go (config YAML, version claims),
// docs_symbols_test.go (Go symbols, CLI invocations), and docs_links_test.go (markdown links,
// mkdocs nav). It exists for the same reason they do: labels.yml is a hand-maintained description
// of state held somewhere else, and nothing compared the two, so they drifted by nine labels.
//
// # Why this is not a paths-filtered sync job
//
// Issue 190 proposed running a label-sync action on push to main when .github/labels.yml changes.
// That would not have caught a single one of the nine drifts, and the reason is worth stating
// because it generalizes. Every drift originated *on GitHub*: `area: ci-cd` and `area: sdk` were
// created by hand in the web UI, `java` was created by Dependabot when it opened a maven PR, and the
// six duplicate defaults have been there since the repository was created. None of those events
// touches the file, so a job gated on the file changing never runs, and the drift the job exists to
// prevent is precisely the drift it cannot see. Verified rather than assumed: `git log -S` for the
// exact `- name:` form of all nine returns nothing for any of them.
//
// So the check runs unconditionally, and it runs in *both* directions. A job that creates labels
// from the file and ignores extras is green on today's state while all nine drifts are live — the
// failure mode issue 190's own acceptance criteria call out.
//
// # The two halves, and why one is offline
//
// TestDependabotLabelsAreDefined needs no network: it compares two files in this repository. That
// seam is not hypothetical — `automerge` was named by dependabot.yml and defined nowhere, Dependabot
// drops labels it cannot find without reporting it, and every approve and merge step in
// dependabot-automerge.yml was gated on that label. 46 PRs were opened and none merged.
//
// TestLabelsYAMLMatchesTheRepository needs the GitHub API, so it is skipped unless `gh` is present
// and authenticated. That makes it a gate in CI (where GITHUB_TOKEN is) and a courtesy locally.
// A skip is not a pass, which is why the structural half is a separate test that always runs and
// why TestLabelsYAMLIsWellFormed asserts the parse produced something.
package config

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v2"
)

// declaredLabel is one entry in .github/labels.yml.
//
// Description and color are checked as well as the name, because a label whose color and
// description live in a file nobody applies is a label whose color and description are fiction.
type declaredLabel struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Color       string `yaml:"color"`
}

// labelsFromFile parses .github/labels.yml.
//
// A real YAML decode rather than a line scan, unlike navTargets in docs_links_test.go. The
// distinction is not stylistic: mkdocs.yml carries `!!python/name:` tags that need a custom
// resolver or unsafe mode, and labels.yml carries nothing but strings. Where a parse is available it
// is the better answer, and this file is the reason — .github/scripts/sync-labels.sh hand-rolled a
// regex parser for this same file and it has never matched a single entry (see
// TestSyncLabelsScriptCanParseTheFile below).
func labelsFromFile(t *testing.T) []declaredLabel {
	t.Helper()

	path := filepath.Join(repoRoot(t), ".github", "labels.yml")

	//nolint:gosec // a path built from the module root this test located itself
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .github/labels.yml: %v", err)
	}

	var labels []declaredLabel
	if err := yaml.Unmarshal(raw, &labels); err != nil {
		t.Fatalf("parse .github/labels.yml: %v", err)
	}

	return labels
}

// TestLabelsYAMLIsWellFormed asserts the parse produced entries, and that each is complete.
//
// The reach assertion earns its place the same way the one in docs_links_test.go did: a mutation
// that made the parse yield nothing left the comparison test passing on zero labels and green. A
// gate that cannot fail proves nothing, and "it parsed" is not the same claim as "it parsed
// something".
func TestLabelsYAMLIsWellFormed(t *testing.T) {
	t.Parallel()

	labels := labelsFromFile(t)

	if len(labels) < 50 {
		t.Fatalf("parsed %d labels from .github/labels.yml; the file defines many more than that, "+
			"so the decode has stopped working and every test in this file is checking nothing",
			len(labels))
	}

	hexColor := regexp.MustCompile(`^[0-9a-f]{6}$`)
	seen := make(map[string]bool, len(labels))

	for _, l := range labels {
		switch {
		case l.Name == "":
			t.Errorf("a label entry has no name: %+v", l)
		case l.Description == "":
			t.Errorf("label %q has no description. GitHub shows it on hover and in the label list; "+
				"a label with no description is one whose meaning lives only in whoever added it",
				l.Name)
		case !hexColor.MatchString(l.Color):
			t.Errorf("label %q has color %q, which is not six lowercase hex digits. The GitHub API "+
				"rejects a leading # and is case-sensitive about nothing else, so this is the one "+
				"form that always applies cleanly", l.Name, l.Color)
		}

		if seen[l.Name] {
			t.Errorf("label %q is defined twice in .github/labels.yml. The later definition wins on "+
				"sync, so the earlier one is a description of nothing", l.Name)
		}

		seen[l.Name] = true
	}
}

// dependabotLabelRefs returns every label named by a `labels:` block in .github/dependabot.yml,
// each tagged with the ecosystem and directory it came from so a failure names the entry to edit.
//
// A YAML decode, and the first draft of this function is why it says so. It scanned lines for a
// `labels:` key and then took every following `- ` item, which reads `- package-ecosystem: maven`
// as a label named "package-ecosystem: maven" — because a sequence item at the same indent as the
// enclosing key ends the block, and a line scan has no notion of indent. Two files up this test
// forbids exactly that substitution in sync-labels.sh. yaml.Unmarshal ignores the keys this struct
// does not name, so the type only has to describe the three fields worth reading.
func dependabotLabelRefs(t *testing.T) []struct {
	Ecosystem, Directory, Label string
} {
	t.Helper()

	path := filepath.Join(repoRoot(t), ".github", "dependabot.yml")

	//nolint:gosec // a path built from the module root this test located itself
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .github/dependabot.yml: %v", err)
	}

	var parsed struct {
		Updates []struct {
			Ecosystem string   `yaml:"package-ecosystem"`
			Directory string   `yaml:"directory"`
			Labels    []string `yaml:"labels"`
		} `yaml:"updates"`
	}

	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse .github/dependabot.yml: %v", err)
	}

	var refs []struct {
		Ecosystem, Directory, Label string
	}

	for _, u := range parsed.Updates {
		for _, l := range u.Labels {
			refs = append(refs, struct {
				Ecosystem, Directory, Label string
			}{u.Ecosystem, u.Directory, l})
		}
	}

	return refs
}

// TestDependabotLabelsAreDefined is the offline half, and the one with a proven failure behind it.
//
// dependabot.yml labeled every PR `automerge`; that label did not exist; Dependabot drops unknown
// labels without reporting it; and dependabot-automerge.yml gated every approve and merge step on
// `contains(labels.*.name, 'automerge')`. 46 PRs opened, 0 merged, no error anywhere. The comment at
// the top of dependabot.yml records the rule — "do not reference a label from dependabot.yml that is
// not defined there" — and this is that rule as a test, because a rule in a comment is a rule until
// someone is in a hurry.
func TestDependabotLabelsAreDefined(t *testing.T) {
	t.Parallel()

	declared := make(map[string]bool)
	for _, l := range labelsFromFile(t) {
		declared[l.Name] = true
	}

	refs := dependabotLabelRefs(t)

	if len(refs) < 5 {
		t.Fatalf("found %d label references in .github/dependabot.yml; every ecosystem entry has a "+
			"labels: block, so the scan has stopped matching and this test is checking nothing",
			len(refs))
	}

	for _, ref := range refs {
		if declared[ref.Label] {
			continue
		}

		t.Errorf("dependabot.yml's %s entry for %s applies label %q, which .github/labels.yml does "+
			"not define.\nDependabot drops a label it cannot find and reports nothing, so the PR "+
			"arrives without it and anything gated on it silently never runs. That is how "+
			"`automerge` left 46 PRs unmerged. Add it to .github/labels.yml.",
			ref.Ecosystem, ref.Directory, ref.Label)
	}
}

// TestSyncLabelsScriptCanParseTheFile checks that the sync script's parser matches the file it
// parses.
//
// This is narrower than it looks and it is here because of what it found. sync-labels.sh matched
// `- name: "..."` — double quotes — and labels.yml has used single quotes or bare scalars for its
// entire history, so the loop matched 0 of 78 entries. The script then printed "Label sync
// complete!" and exited 0, which is the worst available outcome: issue 190 concluded from the
// symptom that labels.yml "is applied by nothing", when what was true is that it was applied by
// something that reported success without doing anything.
//
// The script now uses this test's own admission rule, so the two cannot disagree: a name is a name
// under any of the three YAML scalar forms. Asserting the count matches the decode is what makes
// this a gate rather than a spot check.
func TestSyncLabelsScriptCanParseTheFile(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	//nolint:gosec // a path built from the module root this test located itself
	raw, err := os.ReadFile(filepath.Join(root, ".github", "scripts", "sync-labels.sh"))
	if err != nil {
		t.Fatalf("read .github/scripts/sync-labels.sh: %v", err)
	}

	script := string(raw)

	// The script reads the file with a YAML parser or not at all. A hand-rolled regex over YAML is
	// what produced the silent no-op, and there is no version of that regex worth defending: the
	// three scalar forms are all legal, all present in this file, and all invisible to a pattern
	// written against one of them.
	if strings.Contains(script, `^-\ name:\ \"`) || strings.Contains(script, `^- name: "`) {
		t.Error(".github/scripts/sync-labels.sh matches `- name: \"...\"` with a hand-rolled " +
			"pattern.\nlabels.yml quotes names with single quotes or leaves them bare, so that " +
			"pattern matches nothing — and the script exits 0 having synced no labels. Parse the " +
			"YAML instead.")
	}

	if !strings.Contains(script, "yaml") && !strings.Contains(script, "yq") {
		t.Error(".github/scripts/sync-labels.sh does not appear to parse labels.yml as YAML.\n" +
			"Its previous regex parser matched 0 of the file's entries and reported success. Use a " +
			"parser, not a pattern.")
	}

	// `gh label sync` is not a gh subcommand — the available ones are clone, create, delete, edit,
	// and list. The header comment named it for as long as the script has existed, so anyone who
	// tried the documented alternative got "unknown command".
	if strings.Contains(script, "gh label sync") {
		t.Error(".github/scripts/sync-labels.sh references `gh label sync`, which does not exist. " +
			"gh label has clone, create, delete, edit, and list.")
	}

	// The same string, in the file the script parses. labels.yml's third line was `# Sync with: gh
	// label sync -f .github/labels.yml scttfrdmn/objectfs` — so the instruction a reader finds first
	// is the one that cannot work, and it is checked here rather than in a second test because it is
	// the same defect in the same pair of files.
	//nolint:gosec // a path built from the module root this test located itself
	labelsRaw, err := os.ReadFile(filepath.Join(root, ".github", "labels.yml"))
	if err != nil {
		t.Fatalf("read .github/labels.yml: %v", err)
	}

	if strings.Contains(string(labelsRaw), "gh label sync") {
		t.Error(".github/labels.yml tells the reader to run `gh label sync`, which is not a gh " +
			"subcommand. Point at .github/scripts/sync-labels.sh instead.")
	}
}

// ghLabel is the shape of one entry from `gh label list --json`.
type ghLabel struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

// labelsFromGitHub reads the repository's labels, or skips if it cannot.
//
// `gh` rather than a raw API call, for the same reason internal/awsrates's integration test shells
// out to the AWS CLI: the credential handling is already solved there and reimplementing it here
// would be a second thing to get wrong. In CI `gh` authenticates from GITHUB_TOKEN.
//
// The context carries a timeout because the alternative is worse than a failure: `gh` reaching a
// hung endpoint would block until `go test`'s own 20-minute deadline, which kills the *whole
// package* and reports it as a panic rather than as one network call that did not answer. Thirty
// seconds is far more than the 0.3–0.5 s this takes when the API is reachable.
func labelsFromGitHub(t *testing.T) []ghLabel {
	t.Helper()

	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh is not on PATH; skipping the half of the label gate that needs the GitHub API. " +
			"The structural half (TestDependabotLabelsAreDefined) still ran.")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	//nolint:gosec // fixed arguments, no interpolation
	out, err := exec.CommandContext(ctx, "gh", "label", "list",
		"--repo", "scttfrdmn/objectfs",
		"--limit", "300",
		"--json", "name,description,color",
	).Output()
	if err != nil {
		t.Skipf("gh label list failed (%v); skipping the API half of the label gate. This is "+
			"expected on a fork or without a token.", err)
	}

	var labels []ghLabel
	if err := json.Unmarshal(out, &labels); err != nil {
		t.Fatalf("parse gh label list output: %v", err)
	}

	return labels
}

// TestLabelsYAMLMatchesTheRepository compares the two sets in both directions.
//
// Both, because the drift ran both ways and only one direction is intuitive. Nine labels existed on
// GitHub and not in the file; none existed in the file and not on GitHub. A create-from-file sync is
// therefore green on all nine — it has nothing to create — which is why issue 190's acceptance
// criteria specify that the test for this gate is to add a label on GitHub without touching the file
// and confirm the gate notices.
//
// Color and description are compared too, not just names. A label the file describes differently
// from the label that exists is drift with a longer fuse: the name filters correctly, so nothing
// looks wrong, and the file is quietly no longer a description of anything.
func TestLabelsYAMLMatchesTheRepository(t *testing.T) {
	t.Parallel()

	declared := make(map[string]declaredLabel)
	for _, l := range labelsFromFile(t) {
		declared[l.Name] = l
	}

	actual := make(map[string]ghLabel)
	for _, l := range labelsFromGitHub(t) {
		actual[l.Name] = l
	}

	var onGitHubOnly, inFileOnly []string

	for name := range actual {
		if _, ok := declared[name]; !ok {
			onGitHubOnly = append(onGitHubOnly, name)
		}
	}

	for name := range declared {
		if _, ok := actual[name]; !ok {
			inFileOnly = append(inFileOnly, name)
		}
	}

	sort.Strings(onGitHubOnly)
	sort.Strings(inFileOnly)

	if len(onGitHubOnly) > 0 {
		t.Errorf("%d label(s) exist on GitHub and are absent from .github/labels.yml:\n  %s\n\n"+
			"This is the direction that drifted, and the direction a create-from-file sync cannot "+
			"see. Either add each to the file, or delete it from the repository:\n"+
			"  gh label delete '<name>'\n"+
			"Check for issues carrying it first — `gh issue list --label '<name>' --state all` — "+
			"since deleting a label removes it from everything that had it.",
			len(onGitHubOnly), strings.Join(onGitHubOnly, "\n  "))
	}

	if len(inFileOnly) > 0 {
		t.Errorf("%d label(s) are declared in .github/labels.yml and do not exist on GitHub:\n  %s\n\n"+
			"Anything that applies one of these — dependabot.yml, an issue template, a workflow — "+
			"gets nothing, silently. Run .github/scripts/sync-labels.sh.",
			len(inFileOnly), strings.Join(inFileOnly, "\n  "))
	}

	for name, want := range declared {
		got, ok := actual[name]
		if !ok {
			continue // already reported above
		}

		if got.Color != want.Color {
			t.Errorf("label %q is color %q on GitHub and %q in .github/labels.yml", name, got.Color,
				want.Color)
		}

		if got.Description != want.Description {
			t.Errorf("label %q describes itself differently in the two places:\n"+
				"  GitHub:     %q\n  labels.yml: %q", name, got.Description, want.Description)
		}
	}
}
