package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkflowExpressionsAreValid catches a failure with no local symptom at all.
//
// Actions interpolates `${{ ... }}` everywhere in a workflow, including inside a `run:` block and
// including inside what a shell would treat as a comment — the substitution happens before any shell
// exists. An expression it cannot parse fails the *file*, so no job starts and the run reports
// "This run likely failed because of a workflow file issue" with no job list, no log, and no
// annotation. `gh run view` shows that sentence and nothing else.
//
// Which is how a comment cost a round trip here. A step in ci.yml explained that a workflow extractor
// carried only the plain `env:` entries and not the templated ones, and the explanation named them the
// obvious way — as `${{ secrets.* }}`. `secrets.*` is not a valid expression, and neither is the
// `${{ }}` a second comment used to stand for "a templated one". check-yaml passed, shellcheck passed
// on the extracted step, the YAML was well-formed and the shell was correct.
//
// So the rule is: an expression's contents must look like an expression. Deliberately not a parser —
// what has to be distinguished is prose that happens to be inside braces from a real context
// reference, and the two are far apart.
//
// This test used to live in served_repositories_test.go, beside the apt and yum repository gates it was
// written for. Those repositories were never published and their machinery is gone; this gate is about
// every workflow in the tree and had nothing to do with them, so it moved here rather than being
// deleted along with its old neighbors.
func TestWorkflowExpressionsAreValid(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(repoRoot(t), ".github", "workflows")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("could not read %s: %v", dir, err)
	}

	// The contexts and functions a workflow expression can start from. An expression naming none of
	// these is not necessarily wrong — a negation, `format(...)`, a literal string — so a leading
	// function call or literal is accepted too, and only the shapes that cannot parse are rejected.
	//
	// One entry below carries a British double-l spelling, and it is GitHub's rather than a typo — that
	// is how the function is named in the expression language. misspell flags it because the American
	// form is the one it knows. This is a list of identifiers in someone else's language, so the
	// linter is right about English and wrong about the subject, hence the suppression rather than a
	// "fix" that would stop the entry from matching anything.
	contexts := []string{
		"github", "env", "vars", "job", "jobs", "steps", "runner", "secrets", "strategy", "matrix",
		//nolint:misspell // GitHub spells its own function cancelled().
		"needs", "inputs", "always", "success", "failure", "cancelled", "hashFiles", "format",
		"contains", "startsWith", "endsWith", "join", "toJSON", "fromJSON", "!", "'", "\"",
	}

	checked := 0

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yml" {
			continue
		}

		workflow := readFile(t, filepath.Join(dir, entry.Name()))

		for i, after := range strings.Split(workflow, "${{")[1:] {
			// An unclosed opener is its own failure and not this test's: it would be the last fragment
			// with no `}}` anywhere in it, and there is nothing to judge as an expression.
			raw, closed := stringsCutBefore(after, "}}")
			if !closed {
				t.Errorf("%s has a `${{` that is never closed (occurrence %d)", entry.Name(), i+1)

				continue
			}

			expr := strings.TrimSpace(raw)

			checked++

			// An empty expression. `${{ }}` as shorthand for "a templated value" reads naturally in a
			// comment and fails the file.
			if expr == "" {
				t.Errorf("%s has an empty `${{ }}` expression (occurrence %d). Actions parses it "+
					"whether it is in a comment or not, and an empty one fails the whole file — no job "+
					"starts and the run says only \"likely failed because of a workflow file issue\"",
					entry.Name(), i+1)

				continue
			}

			// A glob or a wildcard where a context reference belongs — `secrets.*`, the exact thing that
			// failed here. Checked before the context list, since `secrets.*` starts with a valid context
			// and is still not a valid expression.
			if strings.ContainsAny(expr, "*?") {
				t.Errorf("%s has `${{ %s }}`, which contains a wildcard. There is no globbing in a "+
					"workflow expression; if this is prose describing a family of values, do not write it "+
					"inside braces — Actions interpolates comments too, and an unparseable expression "+
					"fails the file before any job starts", entry.Name(), expr)

				continue
			}

			if !strings.HasPrefix(expr, "(") &&
				!hasAnyPrefix(expr, contexts) {
				t.Errorf("%s has `${{ %s }}`, which starts from no known context or function. If it is "+
					"a real expression, name the context; if it is prose, take it out of the braces",
					entry.Name(), expr)
			}
		}
	}

	// A floor, because this test finding nothing would look exactly like a clean tree. Counted, not
	// guessed: 91 across the five workflows when this was last measured — ci.yml 50, release.yml 30,
	// dependabot-automerge.yml 9, pages.yml 1, security.yml 1. It was 92 before the package-repository
	// deletion, which took pages.yml from 4 to 1; the floor stayed where it was because it is a floor on
	// the walk working, not a pin on the count.
	if checked < 50 {
		t.Fatalf("checked %d workflow expressions across .github/workflows, and there were 91 when "+
			"this was written. Either the workflows moved or the split has stopped matching, and a "+
			"check that inspects nothing passes", checked)
	}
}

// stringsCutBefore returns what precedes the first occurrence of sep, and whether sep was present.
//
// strings.Cut with the tail discarded, named so the call site reads as what it is: an expression runs
// from `${{` up to the closing `}}`, and an absent closer is not an expression at all.
func stringsCutBefore(s, sep string) (string, bool) {
	before, _, found := strings.Cut(s, sep)

	return before, found
}

// hasAnyPrefix reports whether s begins with any of the given prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}

	return false
}
