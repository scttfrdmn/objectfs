package config

import (
	"context"
	"encoding/json"
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A gap the project admits to is cited in two places, and both must point at open work.
//
// README's "Not implemented" table calls itself part of the contract, and its last column says who is
// tracking each gap. The refusals behind those rows log the same issue number, so an operator reading
// `chmod and chown of a directory are not implemented ... issue=N` can find out what is happening. Both
// citations for directory chmod/chown and directory xattrs pointed at #165 and #167 — issues that
// implemented those operations for *files* and were closed as completed, taking the directory halves
// with them. The gaps were real; the tracking was not, and a closed issue reads as evidence the work
// was done, so the natural conclusion was that the rows were stale rather than the links. #567.
//
// Scoped to the Not-implemented section and to log fields deliberately. README links closed issues
// elsewhere and is right to: "There is no pprof listener at any address. See #245" cites finished work
// as history. Only a citation that claims a gap is tracked has to point at something still open.

// githubIssueLink matches a link to an issue in this repository.
var githubIssueLink = regexp.MustCompile(`github\.com/scttfrdmn/objectfs/issues/(\d+)`)

// logIssueField matches an `"issue", N` pair in a structured log call.
var logIssueField = regexp.MustCompile(`"issue",\s*(\d+)\b`)

// gapCitation is one place that claims an issue is tracking a gap.
type gapCitation struct {
	Where string
	Issue int
}

// trackedGapCitations collects every gap citation: the issue links in README's Not-implemented
// section, and every `"issue", N` log field in non-test Go code.
//
// It asserts its own reach, because the property it feeds is checked per citation and an empty list
// satisfies it. The README floor is on the section's *rows*, not its links: the section is what can
// stop being found (a heading renamed, a level changed), and a section with zero links is a legitimate
// state — every gap could be untracked, or none could remain.
func trackedGapCitations(t *testing.T) []gapCitation {
	t.Helper()

	readme := readFile(t, filepath.Join(repoRoot(t), "README.md"))

	section, ok := markdownSection(readme, "### Not implemented")
	if !ok {
		t.Fatal("README.md has no `### Not implemented` heading. This gate reads the tracked-gap " +
			"citations out of that section; if it moved or was renamed, point the gate at the new one")
	}

	rows := 0
	var out []gapCitation

	for i, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Operation") || strings.HasPrefix(line, "|---") {
			continue
		}

		rows++

		for _, m := range githubIssueLink.FindAllStringSubmatch(line, -1) {
			n, _ := strconv.Atoi(m[1])
			out = append(out, gapCitation{Where: "README.md, line " + strconv.Itoa(i+1) + " of the Not implemented section", Issue: n})
		}
	}

	if rows < 5 {
		t.Fatalf("README.md's Not implemented section has %d table rows, expected at least 5. The row "+
			"match has stopped working, and every citation in the table is going unchecked", rows)
	}

	root := repoRoot(t)

	for _, dir := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			rel, _ := filepath.Rel(root, path)

			for i, line := range strings.Split(readFile(t, path), "\n") {
				for _, m := range logIssueField.FindAllStringSubmatch(line, -1) {
					n, _ := strconv.Atoi(m[1])
					out = append(out, gapCitation{Where: rel + ":" + strconv.Itoa(i+1), Issue: n})
				}
			}

			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	return out
}

// markdownSection returns the text under heading, up to the next heading of the same or a higher
// level.
func markdownSection(doc, heading string) (string, bool) {
	level := strings.Count(strings.Fields(heading)[0], "#")

	var (
		body []string
		in   bool
	)

	for _, line := range strings.Split(doc, "\n") {
		if strings.TrimSpace(line) == heading {
			in = true

			continue
		}

		if !in {
			continue
		}

		if strings.HasPrefix(line, "#") {
			if hashes := len(line) - len(strings.TrimLeft(line, "#")); hashes <= level {
				break
			}
		}

		body = append(body, line)
	}

	return strings.Join(body, "\n"), in
}

// TestTrackedGapCitationsAreFound is the offline half: the collector reaches what it claims to.
//
// Two log citations exist when this is written — directory Setattr and directory Setxattr — so a walk
// finding none has stopped matching rather than found a clean tree.
func TestTrackedGapCitationsAreFound(t *testing.T) {
	t.Parallel()

	logs := 0

	for _, c := range trackedGapCitations(t) {
		if !strings.HasPrefix(c.Where, "README.md") {
			logs++
		}
	}

	if logs < 2 {
		t.Fatalf("found %d `\"issue\", N` log fields in non-test Go code, expected at least 2 "+
			"(DirectoryNode.Setattr and DirectoryNode.Setxattr). The walk or the pattern has stopped "+
			"matching, and the API half of this gate is checking nothing", logs)
	}
}

// TestTrackedGapCitationsAreOpen is the API half: every citation points at an open issue.
//
// Skips without `gh` or a token, like the label gate, and for the same reason the `labels` job in
// ci.yml greps its output for this test's SKIP: a skipped gate reports success.
func TestTrackedGapCitationsAreOpen(t *testing.T) {
	t.Parallel()

	citations := trackedGapCitations(t)

	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh is not on PATH; skipping the half of the tracked-gap gate that needs the GitHub API. " +
			"TestTrackedGapCitationsAreFound still ran.")
	}

	issues := map[int][]string{}
	for _, c := range citations {
		issues[c.Issue] = append(issues[c.Issue], c.Where)
	}

	numbers := make([]int, 0, len(issues))
	for n := range issues {
		numbers = append(numbers, n)
	}

	sort.Ints(numbers)

	for _, n := range numbers {
		state, title := issueState(t, n)
		if state == "OPEN" {
			continue
		}

		t.Errorf("issue #%d (%q) is %s, and is cited as tracking an open gap at:\n\t%s\n"+
			"A closed issue reads as the work being done, so a reader concludes the citation is stale "+
			"rather than the gap. If the gap is closed, remove the row or the refusal. If it is still "+
			"real, file an issue for what remains and cite that — #165 and #167 were closed for files "+
			"while their directory halves stayed open (#567).",
			n, title, state, strings.Join(issues[n], "\n\t"))
	}
}

// issueState returns an issue's state and title, or skips when the API is unreachable.
//
// Thirty seconds for the reason labelsFromGitHub gives: a hung `gh` would otherwise sit until go test's
// package deadline and be reported as a panic in a test that is not this one.
func issueState(t *testing.T, n int) (string, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	//nolint:gosec // the only variable argument is an integer
	out, err := exec.CommandContext(ctx, "gh", "issue", "view", strconv.Itoa(n),
		"--repo", "scttfrdmn/objectfs", "--json", "state,title").Output()
	if err != nil {
		t.Skipf("gh issue view %d failed (%v); skipping the API half of the tracked-gap gate. This is "+
			"expected on a fork or without a token.", n, err)
	}

	var issue struct {
		State string `json:"state"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(out, &issue); err != nil {
		t.Fatalf("parse gh issue view %d: %v", n, err)
	}

	return issue.State, issue.Title
}
