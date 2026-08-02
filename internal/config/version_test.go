package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// versionConstant extracts the one authoritative version string, `version = "…"` in
// cmd/objectfs/main.go. It is a regexp rather than an import because cmd/objectfs is package main
// and its constant is unexported, so there is nothing to import — which is also why every document
// that wanted the number copied it by hand.
var versionConstant = regexp.MustCompile(`(?m)^\s*version\s*=\s*"([^"]+)"`)

// currentVersionClaims matches the ways a document asserts what version the project *is* now.
//
// Deliberately narrow on which lines it flags, and deliberately plural because the assertion takes
// more than one written form. A roadmap saying "v0.6.0 — multi-protocol support" is a plan and must
// not fail this test; "**Current Release:** v0.3.0" and "## Current Architecture (v0.2.0)" are both
// claims about the present, and both were wrong by seven and eight releases respectively when this
// test was written. The distinction being drawn is the assertion, not the presence of a number.
//
// Two forms, because two forms is what the repository contained:
//
//   - a labeled field — "**Version:** 2.1", "Current Release: v0.3.0";
//   - "current <anything> (v0.2.0)" — the parenthetical, which slipped past the first version of
//     this check and is why it is a list rather than one pattern. A narrow regexp that passes is
//     indistinguishable from a correct repository until you widen it.
var currentVersionClaims = []*regexp.Regexp{
	regexp.MustCompile(
		`(?im)^[\s\-*>|#]*\**\s*(current\s+(?:version|release)|version)\**\s*:\s*\**\s*v?\d+\.\d+`),
	regexp.MustCompile(`(?i)current\b[^.\n]{0,40}?\(\s*v?\d+\.\d+(?:\.\d+)?\s*\)`),
	regexp.MustCompile(`(?i)v?\d+\.\d+(?:\.\d+)?\s*\(\s*current\s*\)`),
}

// TestNoDocumentRestatesTheCurrentVersion pins the single-source-of-truth rule for the version.
//
// The repository once gave five different answers to "what version is this?" — 0.10.0 in
// cmd/objectfs/main.go, 0.7.0 in CLAUDE.md, v0.3.0 in ROADMAP.md, and v0.2.0 in two more places.
// None of them was a lie when it was written. Each was correct at the moment someone typed it and
// then silently became false, because prose has no mechanism for noticing that it is stale.
//
// The fix is not to correct the four numbers, which only resets the same clock. It is to have one
// authority and a test that fails when a second one appears. A document that needs to name the
// version can point at the constant, or at the GitHub releases page, both of which cannot drift.
//
// A document *may* still discuss versions freely: planned milestones, historical notes, upgrade
// paths. What it may not do is assert which one is current.
func TestNoDocumentRestatesTheCurrentVersion(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	//nolint:gosec // a path built from the module root this test located itself
	mainGo, err := os.ReadFile(filepath.Join(root, "cmd", "objectfs", "main.go"))
	if err != nil {
		t.Fatalf("reading cmd/objectfs/main.go, which holds the authoritative version: %v", err)
	}

	m := versionConstant.FindSubmatch(mainGo)
	if m == nil {
		t.Fatal("cmd/objectfs/main.go no longer declares `version = \"…\"`. If the version moved, " +
			"point this test at its new home rather than deleting the check — the check is what keeps " +
			"there being exactly one home")
	}
	t.Logf("authoritative version: %s (cmd/objectfs/main.go)", m[1])

	for _, path := range markdownFiles(t) {
		rel := shortName(t, path)

		// The changelog's whole job is to name versions, and every heading in it is a historical
		// record rather than a claim about the present.
		if rel == "CHANGELOG.md" {
			continue
		}

		body, err := os.ReadFile(path) //nolint:gosec // a path from a walk of this repository
		if err != nil {
			t.Errorf("reading %s: %v", rel, err)

			continue
		}

		for line := range strings.SplitSeq(string(body), "\n") {
			if !claimsACurrentVersion(line) {
				continue
			}

			t.Errorf("%s asserts a current version: %q\n"+
				"The authority is the `version` constant in cmd/objectfs/main.go (currently %s). A "+
				"number restated in prose cannot be told it has gone stale, which is how this "+
				"repository came to give five different answers at once. Link to the constant or to "+
				"the releases page instead, or — if this line means a planned or historical release "+
				"rather than the current one — say so in words the reader can tell apart.",
				rel, strings.TrimSpace(line), m[1])
		}
	}
}

// claimsACurrentVersion reports whether a line asserts which version is the current one.
func claimsACurrentVersion(line string) bool {
	for _, re := range currentVersionClaims {
		if re.MatchString(line) {
			return true
		}
	}

	return false
}
